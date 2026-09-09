'use strict';

const crypto = require('crypto');
const fs = require('fs');
const http = require('http');
const path = require('path');
const { Readable } = require('stream');
const { pipeline } = require('stream/promises');

const HOP_BY_HOP_HEADERS = new Set([
  'connection',
  'content-encoding',
  'content-length',
  'host',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'proxy-connection',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
]);

const DEFAULT_COOLDOWNS = Object.freeze({
  rate_limit: { baseMs: 60_000, maxMs: 5 * 60_000, exponential: false },
  quota: { baseMs: 30 * 60_000, maxMs: 5 * 60 * 60_000 },
  auth: { baseMs: 60 * 60_000, maxMs: 24 * 60 * 60_000 },
  upstream: { baseMs: 5_000, maxMs: 2 * 60_000 },
});

function parseKeyFile(text) {
  const seen = new Set();
  const keys = [];

  for (const rawLine of String(text).replace(/^\uFEFF/, '').split(/\r?\n/)) {
    const line = rawLine.trim();
    if (!line || line.startsWith('#')) continue;
    if (seen.has(line)) continue;
    seen.add(line);
    keys.push(line);
  }

  return keys;
}

function keyFingerprint(apiKey) {
  return crypto.createHash('sha256').update(apiKey).digest('hex');
}

function redactError(error) {
  const message = error instanceof Error ? error.message : String(error);
  return message.replace(/(?:sk-|Bearer\s+)[A-Za-z0-9._-]+/gi, '[redacted]');
}

function parseRetryAfterMs(value, now = Date.now()) {
  if (!value) return null;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds * 1_000);
  const timestamp = Date.parse(value);
  if (Number.isFinite(timestamp)) return Math.max(0, timestamp - now);
  return null;
}

function classifyFailure(status, bodyText = '') {
  const text = String(bodyText).toLowerCase();

  if (
    /tpm|rpm|rate.?limit|too many requests|inference exceeds|request frequency|限流|频率/.test(text)
  ) {
    return { category: 'rate_limit', retryable: true, scope: 'model' };
  }

  if (
    /token plan entitlement exhausted|entitlement exhausted|quota|insufficient (?:balance|credit)|balance exhausted|credits? exhausted|额度|余额不足/.test(text)
  ) {
    return { category: 'quota', retryable: true, scope: 'model' };
  }

  if (status === 429) {
    return { category: 'rate_limit', retryable: true, scope: 'model' };
  }

  if (
    status === 401 ||
    status === 403 ||
    /invalid api.?key|authentication|unauthori[sz]ed|forbidden|鉴权|认证失败/.test(text)
  ) {
    return { category: 'auth', retryable: true, scope: 'account' };
  }

  if (status === 408 || status === 425 || status >= 500) {
    return { category: 'upstream', retryable: true, scope: 'model' };
  }

  return { category: 'request', retryable: false, scope: 'none' };
}

function extractModel(body) {
  if (!body || body.length === 0) return 'unknown';
  try {
    const parsed = JSON.parse(body.toString('utf8'));
    return typeof parsed.model === 'string' && parsed.model.trim()
      ? parsed.model.trim()
      : 'unknown';
  } catch {
    return 'unknown';
  }
}

function jsonResponse(response, status, payload, extraHeaders = {}) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    ...extraHeaders,
  });
  response.end(`${JSON.stringify(payload)}\n`);
}

function createJsonlLogger(logDir) {
  fs.mkdirSync(logDir, { recursive: true });

  return (event) => {
    const record = { timestamp: new Date().toISOString(), ...event };
    const day = record.timestamp.slice(0, 10);
    const filePath = path.join(logDir, `gateway-${day}.jsonl`);
    try {
      fs.appendFileSync(filePath, `${JSON.stringify(record)}\n`, 'utf8');
    } catch (error) {
      console.error(`[SenseNova Pool] Could not write log: ${redactError(error)}`);
    }

    const details = [
      record.event,
      record.requestId,
      record.account,
      record.model,
      record.status,
      record.category,
    ].filter((value) => value !== undefined && value !== null && value !== '');
    console.log(`[SenseNova Pool] ${details.join(' ')}`);
  };
}

class KeyStore {
  constructor(filePath, logger) {
    this.filePath = filePath;
    this.logger = logger;
    this.signature = null;
    this.accounts = [];
    this.reloads = 0;
  }

  load() {
    const stat = fs.statSync(this.filePath);
    const signature = `${stat.mtimeMs}:${stat.size}`;
    if (signature === this.signature) return this.accounts;

    const keys = parseKeyFile(fs.readFileSync(this.filePath, 'utf8'));
    this.accounts = keys.map((apiKey, index) => ({
      id: `account-${index + 1}`,
      apiKey,
      fingerprint: keyFingerprint(apiKey),
    }));
    this.signature = signature;
    this.reloads += 1;
    this.logger({
      event: 'keys_reloaded',
      accountCount: this.accounts.length,
      reload: this.reloads,
    });
    return this.accounts;
  }
}

class AccountPool {
  constructor({ cooldowns = DEFAULT_COOLDOWNS, now = () => Date.now() } = {}) {
    this.cooldowns = cooldowns;
    this.now = now;
    this.cursor = 0;
    this.failureState = new Map();
    this.inFlight = new Map();
  }

  stateKey(account, model, scope) {
    return `${account.fingerprint}:${scope === 'account' ? '*' : model}`;
  }

  cooldownFor(account, model) {
    const now = this.now();
    const modelState = this.failureState.get(this.stateKey(account, model, 'model'));
    const accountState = this.failureState.get(this.stateKey(account, model, 'account'));
    const states = [modelState, accountState].filter(Boolean);
    if (states.length === 0) return null;
    const active = states.sort((left, right) => right.until - left.until)[0];
    return active.until > now ? active : null;
  }

  pick(accounts, model, excluded = new Set()) {
    if (accounts.length === 0) return null;
    const candidates = accounts.filter((account) => (
      !excluded.has(account.fingerprint) && !this.cooldownFor(account, model)
    ));
    if (candidates.length === 0) return null;

    const minimumInFlight = Math.min(
      ...candidates.map((account) => this.inFlight.get(account.fingerprint) || 0),
    );
    const leastBusy = new Set(
      candidates
        .filter((account) => (this.inFlight.get(account.fingerprint) || 0) === minimumInFlight)
        .map((account) => account.fingerprint),
    );

    for (let offset = 0; offset < accounts.length; offset += 1) {
      const index = (this.cursor + offset) % accounts.length;
      const account = accounts[index];
      if (leastBusy.has(account.fingerprint)) {
        this.cursor = (index + 1) % accounts.length;
        return account;
      }
    }

    return candidates[0];
  }

  begin(account) {
    this.inFlight.set(
      account.fingerprint,
      (this.inFlight.get(account.fingerprint) || 0) + 1,
    );
  }

  end(account) {
    const next = Math.max(0, (this.inFlight.get(account.fingerprint) || 1) - 1);
    if (next === 0) this.inFlight.delete(account.fingerprint);
    else this.inFlight.set(account.fingerprint, next);
  }

  markFailure(account, model, classification, retryAfterMs = null) {
    const settings = this.cooldowns[classification.category];
    if (!settings) return 0;

    const key = this.stateKey(account, model, classification.scope);
    const previous = this.failureState.get(key);
    const consecutive = previous ? previous.consecutive + 1 : 1;
    const exponentialMs = settings.exponential === false
      ? settings.baseMs
      : Math.min(
        settings.maxMs,
        settings.baseMs * (2 ** Math.min(consecutive - 1, 8)),
      );
    const cooldownMs = Math.min(
      settings.maxMs,
      Math.max(exponentialMs, retryAfterMs || 0),
    );
    const now = this.now();
    this.failureState.set(key, {
      category: classification.category,
      consecutive,
      until: now + cooldownMs,
      lastFailureAt: now,
    });
    return cooldownMs;
  }

  markSuccess(account, model) {
    this.failureState.delete(this.stateKey(account, model, 'model'));
  }

  nextReadyAt(accounts, model) {
    if (accounts.length === 0) return null;
    const times = accounts.map((account) => {
      const cooldown = this.cooldownFor(account, model);
      return cooldown ? cooldown.until : this.now();
    });
    return Math.min(...times);
  }

  snapshot(accounts) {
    return accounts.map((account) => {
      const prefix = `${account.fingerprint}:`;
      const states = [...this.failureState.entries()]
        .filter(([key, state]) => key.startsWith(prefix) && state.until > this.now())
        .map(([, state]) => ({
          category: state.category,
          until: new Date(state.until).toISOString(),
        }));
      return {
        id: account.id,
        inFlight: this.inFlight.get(account.fingerprint) || 0,
        cooldowns: states,
      };
    });
  }
}

function buildUpstreamUrl(baseURL, requestUrl) {
  const incoming = new URL(requestUrl, 'http://127.0.0.1');
  const target = new URL(baseURL);
  const basePath = target.pathname.replace(/\/$/, '');
  let requestPath = incoming.pathname;

  if (basePath.endsWith('/v1') && (requestPath === '/v1' || requestPath.startsWith('/v1/'))) {
    requestPath = requestPath.slice(3) || '/';
  }

  target.pathname = `${basePath}${requestPath.startsWith('/') ? requestPath : `/${requestPath}`}`;
  target.search = incoming.search;
  return target;
}

function buildUpstreamHeaders(incomingHeaders, apiKey) {
  const headers = new Headers();
  for (const [name, rawValue] of Object.entries(incomingHeaders)) {
    const lowerName = name.toLowerCase();
    if (HOP_BY_HOP_HEADERS.has(lowerName)) continue;
    if (lowerName === 'authorization' || lowerName === 'x-api-key') continue;
    if (rawValue === undefined) continue;
    headers.set(name, Array.isArray(rawValue) ? rawValue.join(', ') : String(rawValue));
  }
  headers.set('authorization', `Bearer ${apiKey}`);
  headers.set('accept-encoding', 'identity');
  return headers;
}

function responseHeaders(upstream, additions = {}) {
  const headers = {};
  for (const [name, value] of upstream.headers.entries()) {
    if (!HOP_BY_HOP_HEADERS.has(name.toLowerCase())) headers[name] = value;
  }
  return { ...headers, ...additions };
}

async function readRequestBody(request, maximumBytes) {
  const chunks = [];
  let total = 0;
  for await (const chunk of request) {
    total += chunk.length;
    if (total > maximumBytes) {
      const error = new Error(`Request body exceeds ${maximumBytes} bytes`);
      error.statusCode = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function getLocalCredential(request) {
  const authorization = request.headers.authorization || '';
  const match = /^Bearer\s+(.+)$/i.exec(authorization);
  if (match) return match[1];
  return typeof request.headers['x-api-key'] === 'string'
    ? request.headers['x-api-key']
    : '';
}

function abortableSleep(milliseconds, signal) {
  if (signal.aborted) return Promise.reject(signal.reason || new Error('aborted'));
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      signal.removeEventListener('abort', onAbort);
      resolve();
    }, milliseconds);
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal.reason || new Error('aborted'));
    };
    signal.addEventListener('abort', onAbort, { once: true });
  });
}

async function forwardStream(upstream, response, additions) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(upstream.status, responseHeaders(upstream, additions));
  if (!upstream.body) {
    response.end();
    return;
  }
  await pipeline(Readable.fromWeb(upstream.body), response);
}

function createGateway(options = {}) {
  const keyFilePath = path.resolve(options.keyFilePath);
  const logDir = path.resolve(options.logDir);
  const upstreamBaseURL = options.upstreamBaseURL || 'https://token.sensenova.cn/v1';
  const localToken = options.localToken || 'local-sensenova-pool';
  const maxQueueMs = options.maxQueueMs ?? 10 * 60_000;
  const postSweepProbeIntervalMs = options.postSweepProbeIntervalMs ?? 5_000;
  const requestTimeoutMs = options.requestTimeoutMs ?? 5 * 60_000;
  const maximumBodyBytes = options.maximumBodyBytes ?? 64 * 1024 * 1024;
  const models = options.models || [];
  const fetchImpl = options.fetchImpl || globalThis.fetch;
  const logger = options.logger || createJsonlLogger(logDir);
  const keyStore = new KeyStore(keyFilePath, logger);
  const pool = new AccountPool(options.poolOptions);
  const startedAt = Date.now();
  const stats = {
    requests: 0,
    successes: 0,
    failures: 0,
    retries: 0,
    queued: 0,
    canceled: 0,
    pacedProbes: 0,
  };

  function loadAccounts() {
    try {
      return keyStore.load();
    } catch (error) {
      logger({ event: 'key_load_error', error: redactError(error) });
      return [];
    }
  }

  async function handleProxyRequest(request, response, requestUrl) {
    const requestId = crypto.randomUUID().slice(0, 8);
    const requestStartedAt = Date.now();
    const requestAbort = new AbortController();
    const onAborted = () => requestAbort.abort(new Error('client disconnected'));
    request.once('aborted', onAborted);
    response.once('close', () => {
      if (!response.writableEnded) onAborted();
    });

    let body;
    try {
      body = await readRequestBody(request, maximumBodyBytes);
    } catch (error) {
      const status = error.statusCode || 400;
      jsonResponse(response, status, {
        error: { type: 'invalid_request_error', message: redactError(error) },
      });
      return;
    }

    const model = extractModel(body);
    const attempted = new Set();
    let attempts = 0;
    let lastFailure = null;
    let hasQueued = false;
    let completedPoolSweep = false;
    let nextProbeAt = 0;

    while (!requestAbort.signal.aborted) {
      if (completedPoolSweep && nextProbeAt > Date.now()) {
        const elapsed = Date.now() - requestStartedAt;
        const remaining = maxQueueMs - elapsed;
        if (remaining <= 0) {
          stats.failures += 1;
          logger({
            event: 'request_failed',
            requestId,
            model,
            attempts,
            category: lastFailure?.category || 'pool_unavailable',
            queuedMs: elapsed,
          });
          jsonResponse(response, 503, {
            error: {
              type: 'sensenova_pool_unavailable',
              code: lastFailure?.category || 'pool_unavailable',
              message: 'SenseNova remained unavailable beyond the local queue deadline; see the gateway log.',
            },
          }, { 'retry-after': '5', 'x-sensenova-pool-attempts': String(attempts) });
          return;
        }
        try {
          await abortableSleep(
            Math.min(nextProbeAt - Date.now(), remaining, 2_000),
            requestAbort.signal,
          );
        } catch {
          stats.canceled += 1;
          return;
        }
        continue;
      }

      const accounts = loadAccounts();
      if (accounts.length === 0) {
        stats.failures += 1;
        logger({ event: 'request_failed', requestId, model, category: 'no_accounts' });
        jsonResponse(response, 503, {
          error: {
            type: 'sensenova_pool_unavailable',
            code: 'no_accounts',
            message: `No API keys are configured in ${keyFilePath}`,
          },
        });
        return;
      }

      const account = pool.pick(accounts, model, attempted);
      if (!account) {
        if (attempts > 0) completedPoolSweep = true;
        attempted.clear();
        const nextReadyAt = pool.nextReadyAt(accounts, model);
        const elapsed = Date.now() - requestStartedAt;
        const remaining = maxQueueMs - elapsed;
        const waitMs = nextReadyAt === null ? remaining + 1 : Math.max(25, nextReadyAt - Date.now());

        if (remaining <= 0 || waitMs > remaining) {
          stats.failures += 1;
          const retryAfterSeconds = nextReadyAt
            ? Math.max(1, Math.ceil((nextReadyAt - Date.now()) / 1_000))
            : 1;
          logger({
            event: 'request_failed',
            requestId,
            model,
            attempts,
            category: lastFailure?.category || 'pool_unavailable',
            queuedMs: elapsed,
          });
          jsonResponse(response, 503, {
            error: {
              type: 'sensenova_pool_unavailable',
              code: lastFailure?.category || 'pool_unavailable',
              message: `All ${accounts.length} SenseNova accounts are temporarily unavailable; see the local gateway log.`,
            },
          }, {
            'retry-after': String(retryAfterSeconds),
            'x-sensenova-pool-attempts': String(attempts),
          });
          return;
        }

        if (!hasQueued) {
          hasQueued = true;
          stats.queued += 1;
          logger({
            event: 'request_queued',
            requestId,
            model,
            attempts,
            waitMs,
          });
        }
        try {
          await abortableSleep(Math.min(waitMs, 2_000), requestAbort.signal);
        } catch {
          stats.canceled += 1;
          logger({
            event: 'request_canceled',
            requestId,
            model,
            attempts,
            queuedMs: Date.now() - requestStartedAt,
          });
          return;
        }
        continue;
      }

      attempted.add(account.fingerprint);
      attempts += 1;
      pool.begin(account);
      const attemptStartedAt = Date.now();
      const attemptAbort = new AbortController();
      const abortAttempt = () => attemptAbort.abort(requestAbort.signal.reason);
      requestAbort.signal.addEventListener('abort', abortAttempt, { once: true });
      const attemptTimer = setTimeout(
        () => attemptAbort.abort(new Error('upstream response timeout')),
        requestTimeoutMs,
      );

      let upstream;
      try {
        const target = buildUpstreamUrl(upstreamBaseURL, requestUrl);
        upstream = await fetchImpl(target, {
          method: request.method,
          headers: buildUpstreamHeaders(request.headers, account.apiKey),
          body: body.length > 0 && !['GET', 'HEAD'].includes(request.method) ? body : undefined,
          redirect: 'manual',
          signal: attemptAbort.signal,
        });
        clearTimeout(attemptTimer);

        if (upstream.ok) {
          pool.markSuccess(account, model);
          try {
            await forwardStream(upstream, response, {
              'x-sensenova-pool-account': account.id,
              'x-sensenova-pool-attempts': String(attempts),
            });
            stats.successes += 1;
            logger({
              event: 'request_succeeded',
              requestId,
              account: account.id,
              model,
              status: upstream.status,
              attempts,
              durationMs: Date.now() - requestStartedAt,
            });
          } catch (error) {
            const clientCanceled = requestAbort.signal.aborted || response.destroyed;
            if (clientCanceled) stats.canceled += 1;
            else stats.failures += 1;
            logger({
              event: clientCanceled ? 'request_canceled' : 'response_stream_failed',
              requestId,
              account: account.id,
              model,
              status: upstream.status,
              error: redactError(error),
            });
            if (!response.destroyed) response.destroy(error);
          } finally {
            pool.end(account);
            requestAbort.signal.removeEventListener('abort', abortAttempt);
          }
          return;
        }

        const failureBody = Buffer.from(await upstream.arrayBuffer());
        const failureText = failureBody.toString('utf8');
        const classification = classifyFailure(upstream.status, failureText);

        if (!classification.retryable) {
          pool.end(account);
          requestAbort.signal.removeEventListener('abort', abortAttempt);
          stats.failures += 1;
          logger({
            event: 'request_rejected',
            requestId,
            account: account.id,
            model,
            status: upstream.status,
            category: classification.category,
            attempts,
            durationMs: Date.now() - requestStartedAt,
          });
          response.writeHead(upstream.status, responseHeaders(upstream, {
            'x-sensenova-pool-account': account.id,
            'x-sensenova-pool-attempts': String(attempts),
          }));
          response.end(failureBody);
          return;
        }

        const retryAfterMs = parseRetryAfterMs(upstream.headers.get('retry-after'));
        const cooldownMs = pool.markFailure(account, model, classification, retryAfterMs);
        pool.end(account);
        requestAbort.signal.removeEventListener('abort', abortAttempt);
        stats.retries += 1;
        lastFailure = classification;
        logger({
          event: 'upstream_retry',
          requestId,
          account: account.id,
          model,
          status: upstream.status,
          category: classification.category,
          attempt: attempts,
          cooldownMs,
          durationMs: Date.now() - attemptStartedAt,
        });
        if (completedPoolSweep && postSweepProbeIntervalMs > 0) {
          nextProbeAt = Date.now() + postSweepProbeIntervalMs;
          stats.pacedProbes += 1;
        }
      } catch (error) {
        clearTimeout(attemptTimer);
        pool.end(account);
        requestAbort.signal.removeEventListener('abort', abortAttempt);
        if (requestAbort.signal.aborted) return;

        const classification = { category: 'upstream', retryable: true, scope: 'model' };
        const cooldownMs = pool.markFailure(account, model, classification);
        stats.retries += 1;
        lastFailure = classification;
        logger({
          event: 'upstream_retry',
          requestId,
          account: account.id,
          model,
          category: classification.category,
          attempt: attempts,
          cooldownMs,
          durationMs: Date.now() - attemptStartedAt,
          error: redactError(error),
        });
        if (completedPoolSweep && postSweepProbeIntervalMs > 0) {
          nextProbeAt = Date.now() + postSweepProbeIntervalMs;
          stats.pacedProbes += 1;
        }
      }
    }
  }

  const server = http.createServer(async (request, response) => {
    response.setHeader('access-control-allow-origin', '*');
    response.setHeader('access-control-allow-methods', 'GET, POST, OPTIONS');
    response.setHeader('access-control-allow-headers', 'Content-Type, Authorization, x-api-key');

    if (request.method === 'OPTIONS') {
      response.writeHead(204);
      response.end();
      return;
    }

    const requestUrl = new URL(request.url, 'http://127.0.0.1');
    if (request.method === 'GET' && requestUrl.pathname === '/health') {
      const accounts = loadAccounts();
      jsonResponse(response, accounts.length > 0 ? 200 : 503, {
        status: accounts.length > 0 ? 'ok' : 'no_accounts',
        accountCount: accounts.length,
        keyReloads: keyStore.reloads,
        uptimeSeconds: Math.floor((Date.now() - startedAt) / 1_000),
        stats,
        accounts: pool.snapshot(accounts),
      });
      return;
    }

    if (getLocalCredential(request) !== localToken) {
      jsonResponse(response, 401, {
        error: { type: 'authentication_error', message: 'Invalid local gateway token.' },
      });
      return;
    }

    if (request.method === 'GET' && requestUrl.pathname === '/v1/models') {
      jsonResponse(response, 200, {
        object: 'list',
        data: models.map((id) => ({ id, object: 'model', owned_by: 'sensenova' })),
      });
      return;
    }

    if (request.method !== 'POST' || requestUrl.pathname !== '/v1/chat/completions') {
      jsonResponse(response, 404, {
        error: { type: 'not_found', message: 'Supported endpoint: POST /v1/chat/completions' },
      });
      return;
    }

    stats.requests += 1;
    try {
      await handleProxyRequest(request, response, `${requestUrl.pathname}${requestUrl.search}`);
    } catch (error) {
      stats.failures += 1;
      logger({ event: 'gateway_error', error: redactError(error) });
      jsonResponse(response, 500, {
        error: { type: 'gateway_error', message: 'Unexpected local gateway error; see the gateway log.' },
      });
    }
  });

  return {
    server,
    keyStore,
    pool,
    stats,
    listen(port, host = '127.0.0.1') {
      return new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(port, host, () => {
          server.removeListener('error', reject);
          resolve(server.address());
        });
      });
    },
    close() {
      return new Promise((resolve, reject) => {
        const forceTimer = setTimeout(() => server.closeAllConnections?.(), 1_000);
        forceTimer.unref?.();
        server.close((error) => {
          clearTimeout(forceTimer);
          if (error) reject(error);
          else resolve();
        });
        server.closeIdleConnections?.();
      });
    },
  };
}

module.exports = {
  AccountPool,
  KeyStore,
  buildUpstreamUrl,
  classifyFailure,
  createGateway,
  parseKeyFile,
  parseRetryAfterMs,
  redactError,
};
