'use strict';

const crypto = require('node:crypto');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { Readable } = require('node:stream');
const { pipeline } = require('node:stream/promises');

const HOP_BY_HOP_HEADERS = new Set([
  'connection',
  'keep-alive',
  'proxy-authenticate',
  'proxy-authorization',
  'te',
  'trailer',
  'transfer-encoding',
  'upgrade',
]);

const ROUTE_FALLBACK_STATUSES = new Set([502, 503, 504, 522, 523, 524]);
const SAFE_CONNECT_FAILURE_CODES = new Set([
  'EAI_AGAIN',
  'ECONNREFUSED',
  'EHOSTUNREACH',
  'ENETDOWN',
  'ENETUNREACH',
  'ENOTFOUND',
  'UND_ERR_CONNECT_TIMEOUT',
]);

function redactError(error) {
  const message = String(error?.message || error || 'unknown error');
  return message
    .replace(/Bearer\s+[^\s,;]+/gi, 'Bearer [redacted]')
    .replace(/\b(?:sk|ak)-[A-Za-z0-9._-]{8,}\b/g, '[redacted]')
    .slice(0, 500);
}

function parseRetryAfterMs(value) {
  if (!value) return null;
  const seconds = Number(value);
  if (Number.isFinite(seconds) && seconds >= 0) return Math.ceil(seconds * 1_000);
  const date = Date.parse(value);
  if (Number.isNaN(date)) return null;
  return Math.max(0, date - Date.now());
}

function classifyFailure(status, text = '') {
  const normalized = String(text).toLowerCase();
  const rateLimited = status === 429
    || /(?:tpm|rpm|rate)[\s/_-]*(?:limit|exceed)/i.test(normalized)
    || normalized.includes('too many requests')
    || normalized.includes('resource exhausted');

  if (rateLimited) return { category: 'rate_limit', retryable: true };
  if (status === 401 || status === 403) return { category: 'auth', retryable: false };
  if (status === 408) return { category: 'upstream_timeout', retryable: true };
  if (status >= 500) return { category: 'upstream', retryable: true };
  return { category: 'request', retryable: false };
}

function errorCodes(error) {
  const codes = [];
  const seen = new Set();
  let current = error;
  while (current && typeof current === 'object' && !seen.has(current)) {
    seen.add(current);
    if (typeof current.code === 'string') codes.push(current.code);
    current = current.cause;
  }
  return codes;
}

function isSafeConnectFailure(error) {
  return errorCodes(error).some((code) => SAFE_CONNECT_FAILURE_CODES.has(code));
}

function createJsonlLogger(logDir) {
  fs.mkdirSync(logDir, { recursive: true });
  return (event) => {
    const record = { timestamp: new Date().toISOString(), ...event };
    const filePath = path.join(logDir, `gateway-${record.timestamp.slice(0, 10)}.jsonl`);
    try {
      fs.appendFileSync(filePath, `${JSON.stringify(record)}\n`, 'utf8');
    } catch (error) {
      console.error(`[Agnes Proxy] Could not write log: ${redactError(error)}`);
    }

    const details = [
      record.event,
      record.requestId,
      record.model,
      record.route,
      record.status,
      record.category,
    ].filter((value) => value !== undefined && value !== null && value !== '');
    console.log(`[Agnes Proxy] ${details.join(' ')}`);
  };
}

function buildUpstreamUrl(baseURL, requestPath) {
  const base = new URL(baseURL.endsWith('/') ? baseURL : `${baseURL}/`);
  const incoming = new URL(requestPath, 'http://127.0.0.1');
  let pathname = incoming.pathname;
  const basePath = base.pathname.replace(/\/$/, '');
  if (basePath && pathname.startsWith(`${basePath}/`)) pathname = pathname.slice(basePath.length + 1);
  else pathname = pathname.replace(/^\//, '');
  const target = new URL(pathname, base);
  target.search = incoming.search;
  return target;
}

function buildUpstreamHeaders(headers, apiKey) {
  const result = {};
  for (const [name, value] of Object.entries(headers)) {
    const lower = name.toLowerCase();
    if (
      HOP_BY_HOP_HEADERS.has(lower)
      || lower === 'host'
      || lower === 'content-length'
      || lower === 'authorization'
      || lower === 'x-api-key'
      || value === undefined
    ) continue;
    result[name] = Array.isArray(value) ? value.join(', ') : value;
  }
  result.authorization = `Bearer ${apiKey}`;
  result['accept-encoding'] = 'identity';
  return result;
}

function responseHeaders(upstream, additions = {}) {
  const headers = {};
  for (const [name, value] of upstream.headers.entries()) {
    if (!HOP_BY_HOP_HEADERS.has(name.toLowerCase())) headers[name] = value;
  }
  return { ...headers, ...additions };
}

function getLocalCredential(request) {
  const authorization = request.headers.authorization || '';
  const bearer = /^Bearer\s+(.+)$/i.exec(authorization);
  if (bearer) return bearer[1].trim();
  return String(request.headers['x-api-key'] || '').trim();
}

function jsonResponse(response, status, body, headers = {}) {
  const payload = Buffer.from(JSON.stringify(body));
  response.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': String(payload.length),
    ...headers,
  });
  response.end(payload);
}

async function readRequestBody(request, maximumBodyBytes) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > maximumBodyBytes) {
      const error = new Error(`Request body exceeds ${maximumBodyBytes} bytes`);
      error.statusCode = 413;
      throw error;
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

function extractModel(body) {
  if (!body?.length) return 'unknown';
  try {
    const parsed = JSON.parse(body.toString('utf8'));
    return typeof parsed.model === 'string' && parsed.model ? parsed.model : 'unknown';
  } catch {
    return 'unknown';
  }
}

function abortableSleep(milliseconds, signal) {
  if (milliseconds <= 0) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const timer = setTimeout(done, milliseconds);
    function done() {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }
    function onAbort() {
      clearTimeout(timer);
      reject(signal.reason || new Error('aborted'));
    }
    if (signal?.aborted) onAbort();
    else signal?.addEventListener('abort', onAbort, { once: true });
  });
}

async function forwardResponse(upstream, response, additions = {}, bufferedBody = null) {
  const headers = responseHeaders(upstream, additions);
  if (bufferedBody) headers['content-length'] = String(bufferedBody.length);
  response.writeHead(upstream.status, headers);
  if (bufferedBody) {
    response.end(bufferedBody);
    return;
  }
  if (!upstream.body) {
    response.end();
    return;
  }
  await pipeline(Readable.fromWeb(upstream.body), response);
}

class RateGate {
  constructor(options = {}) {
    this.now = options.now || Date.now;
    this.sleep = options.sleep || abortableSleep;
    this.defaultCooldownMs = options.defaultCooldownMs ?? 60_000;
    this.pollMs = options.pollMs ?? 250;
    this.states = new Map();
  }

  markLimited(model, retryAfterMs) {
    const cooldownMs = Math.max(this.defaultCooldownMs, retryAfterMs || 0);
    const state = this.states.get(model) || { blockedUntil: 0, probeInFlight: false };
    state.blockedUntil = Math.max(state.blockedUntil, this.now() + cooldownMs);
    state.probeInFlight = false;
    this.states.set(model, state);
    return cooldownMs;
  }

  async wait(model, signal, deadline) {
    let waitedMs = 0;
    while (true) {
      const state = this.states.get(model);
      if (!state) return { probe: false, waitedMs };

      const now = this.now();
      if (now >= deadline) {
        const error = new Error('rate-limit queue timeout');
        error.code = 'AGNES_RATE_QUEUE_TIMEOUT';
        throw error;
      }

      if (now >= state.blockedUntil && !state.probeInFlight) {
        state.probeInFlight = true;
        return { probe: true, waitedMs };
      }

      const waitMs = now < state.blockedUntil
        ? Math.min(this.pollMs, state.blockedUntil - now, deadline - now)
        : Math.min(this.pollMs, deadline - now);
      const started = this.now();
      await this.sleep(Math.max(1, waitMs), signal);
      waitedMs += Math.max(0, this.now() - started);
    }
  }

  markHealthy(model, permit) {
    if (permit?.probe) this.states.delete(model);
  }

  releaseProbe(model, permit) {
    if (!permit?.probe) return;
    const state = this.states.get(model);
    if (state) state.probeInFlight = false;
  }

  snapshot() {
    const now = this.now();
    return [...this.states.entries()].map(([model, state]) => ({
      model,
      retryAfterMs: Math.max(0, state.blockedUntil - now),
      probeInFlight: state.probeInFlight,
    }));
  }
}

class RouteSelector {
  constructor(options = {}) {
    this.now = options.now || Date.now;
    this.preferClashMs = options.preferClashMs ?? 5 * 60_000;
    this.directRetryAt = 0;
    this.lastSuccessfulRoute = null;
    this.failures = { direct: 0, clash: 0 };
  }

  order() {
    return this.now() < this.directRetryAt ? ['clash', 'direct'] : ['direct', 'clash'];
  }

  markTransportFailure(route) {
    this.failures[route] += 1;
    if (route === 'direct') this.directRetryAt = this.now() + this.preferClashMs;
  }

  markConnected(route) {
    this.lastSuccessfulRoute = route;
    if (route === 'direct') {
      this.directRetryAt = 0;
      this.failures.direct = 0;
    } else {
      this.failures.clash = 0;
    }
  }

  snapshot() {
    return {
      preferred: this.now() < this.directRetryAt ? 'clash' : 'direct',
      directProbeAfterMs: Math.max(0, this.directRetryAt - this.now()),
      lastSuccessfulRoute: this.lastSuccessfulRoute,
      transportFailures: { ...this.failures },
    };
  }
}

function createAttemptSignal(parentSignal, timeoutMs) {
  const controller = new AbortController();
  let timedOut = false;
  const onAbort = () => controller.abort(parentSignal.reason || new Error('client disconnected'));
  parentSignal.addEventListener('abort', onAbort, { once: true });
  const timer = setTimeout(() => {
    timedOut = true;
    const error = new Error('upstream response timeout');
    error.code = 'AGNES_RESPONSE_TIMEOUT';
    controller.abort(error);
  }, timeoutMs);
  if (parentSignal.aborted) onAbort();
  return {
    signal: controller.signal,
    timedOut: () => timedOut,
    dispose() {
      clearTimeout(timer);
      parentSignal.removeEventListener('abort', onAbort);
    },
  };
}

function createAgnesGateway(options = {}) {
  if (!options.apiKey) throw new Error('AGNES_API_KEY is required');
  const apiKey = options.apiKey;
  const upstreamBaseURL = options.upstreamBaseURL || 'https://apihub.agnes-ai.com/v1';
  const localToken = options.localToken || 'local-agnes-proxy';
  const maximumBodyBytes = options.maximumBodyBytes ?? 64 * 1024 * 1024;
  const requestTimeoutMs = options.requestTimeoutMs ?? 5 * 60_000;
  const maxQueueMs = options.maxQueueMs ?? 10 * 60_000;
  const upstreamRetryMs = options.upstreamRetryMs ?? 5_000;
  const maxUpstreamRetries = options.maxUpstreamRetries ?? 2;
  const models = options.models || [];
  const fetchImpl = options.fetchImpl || globalThis.fetch;
  const dispatchers = options.dispatchers || {};
  const recoverNetwork = options.recoverNetwork || (async () => ({ attempted: false, ok: false }));
  const logger = options.logger || createJsonlLogger(path.resolve(
    options.logDir || path.join(process.cwd(), 'logs', 'agnes-proxy'),
  ));
  const rateGate = options.rateGate || new RateGate({
    defaultCooldownMs: options.rateCooldownMs,
  });
  const routes = options.routeSelector || new RouteSelector({
    preferClashMs: options.preferClashMs,
  });
  const startedAt = Date.now();
  const stats = {
    requests: 0,
    successes: 0,
    failures: 0,
    rateRetries: 0,
    routeFallbacks: 0,
    recoveries: 0,
    canceled: 0,
  };

  async function fetchRoute(route, request, requestUrl, body, signal) {
    const attemptSignal = createAttemptSignal(signal, requestTimeoutMs);
    try {
      const target = buildUpstreamUrl(upstreamBaseURL, requestUrl);
      const init = {
        method: request.method,
        headers: buildUpstreamHeaders(request.headers, apiKey),
        body: body.length > 0 && !['GET', 'HEAD'].includes(request.method) ? body : undefined,
        redirect: 'manual',
        signal: attemptSignal.signal,
      };
      if (dispatchers[route]) init.dispatcher = dispatchers[route];
      const response = await fetchImpl(target, init);
      routes.markConnected(route);
      return { response, route };
    } catch (error) {
      if (signal.aborted) throw signal.reason || error;
      if (attemptSignal.timedOut()) {
        error.code = error.code || 'AGNES_RESPONSE_TIMEOUT';
      }
      return { error, route, safeToRetry: isSafeConnectFailure(error) };
    } finally {
      attemptSignal.dispose();
    }
  }

  async function fetchWithRoutes(request, requestUrl, body, signal, requestId, model) {
    const bufferedResponses = [];
    let lastError = null;
    let clashConnectFailed = false;

    for (const route of routes.order()) {
      const result = await fetchRoute(route, request, requestUrl, body, signal);
      if (result.response) {
        if (!ROUTE_FALLBACK_STATUSES.has(result.response.status)) return result;
        const bufferedBody = Buffer.from(await result.response.arrayBuffer());
        bufferedResponses.push({ ...result, bufferedBody });
        stats.routeFallbacks += 1;
        logger({
          event: 'route_http_fallback',
          requestId,
          model,
          route,
          status: result.response.status,
        });
        continue;
      }

      lastError = result;
      logger({
        event: result.safeToRetry ? 'route_connect_failed' : 'route_ambiguous_failure',
        requestId,
        model,
        route,
        category: result.safeToRetry ? 'network' : 'transport',
        error: redactError(result.error),
      });

      if (!result.safeToRetry) return result;
      routes.markTransportFailure(route);
      stats.routeFallbacks += 1;
      if (route === 'clash') clashConnectFailed = true;
    }

    if (clashConnectFailed && !signal.aborted) {
      let recovery;
      try {
        recovery = await recoverNetwork({ signal, requestId, model });
      } catch (error) {
        recovery = { attempted: true, ok: false, reason: 'recovery_error' };
        logger({
          event: 'clash_recovery_failed',
          requestId,
          model,
          route: 'clash',
          category: recovery.reason,
          error: redactError(error),
        });
      }
      if (recovery?.attempted) stats.recoveries += 1;
      logger({
        event: recovery?.ok ? 'clash_recovery_succeeded' : 'clash_recovery_unavailable',
        requestId,
        model,
        route: 'clash',
        category: recovery?.reason,
      });
      if (recovery?.ok && !signal.aborted) {
        const recovered = await fetchRoute('clash', request, requestUrl, body, signal);
        if (recovered.response) return recovered;
        lastError = recovered;
      }
    }

    if (bufferedResponses.length > 0) return bufferedResponses.at(-1);
    return lastError || { error: new Error('No Agnes network route is available'), route: 'none' };
  }

  async function handleProxyRequest(request, response, requestUrl) {
    const requestId = crypto.randomUUID().slice(0, 8);
    const requestStartedAt = Date.now();
    const deadline = requestStartedAt + maxQueueMs;
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
      jsonResponse(response, error.statusCode || 400, {
        error: { type: 'invalid_request_error', message: redactError(error) },
      });
      return;
    }

    const model = extractModel(body);
    let rateRetries = 0;
    let upstreamRetries = 0;

    while (!requestAbort.signal.aborted) {
      let permit;
      try {
        permit = await rateGate.wait(model, requestAbort.signal, deadline);
      } catch (error) {
        if (requestAbort.signal.aborted) return;
        stats.failures += 1;
        logger({ event: 'request_failed', requestId, model, category: 'rate_queue_timeout' });
        jsonResponse(response, 503, {
          error: {
            type: 'agnes_proxy_unavailable',
            code: 'rate_queue_timeout',
            message: 'Agnes remained rate-limited beyond the local queue deadline; see the gateway log.',
          },
        }, { 'retry-after': '60', 'x-agnes-rate-retries': String(rateRetries) });
        return;
      }

      const result = await fetchWithRoutes(
        request,
        requestUrl,
        body,
        requestAbort.signal,
        requestId,
        model,
      );

      if (!result?.response) {
        rateGate.releaseProbe(model, permit);
        if (requestAbort.signal.aborted) return;
        stats.failures += 1;
        logger({
          event: 'request_failed',
          requestId,
          model,
          route: result?.route,
          category: result?.safeToRetry ? 'network' : 'ambiguous_transport',
          error: redactError(result?.error),
        });
        jsonResponse(response, 502, {
          error: {
            type: 'agnes_proxy_network_error',
            code: result?.safeToRetry ? 'no_network_route' : 'ambiguous_transport_failure',
            message: result?.safeToRetry
              ? 'Neither the direct nor Clash route could reach Agnes; see the gateway log.'
              : 'The Agnes connection failed after it may have accepted the request, so the gateway did not duplicate the inference.',
          },
        });
        return;
      }

      const upstream = result.response;
      if (upstream.ok) {
        rateGate.markHealthy(model, permit);
        try {
          await forwardResponse(upstream, response, {
            'x-agnes-route': result.route,
            'x-agnes-rate-retries': String(rateRetries),
          }, result.bufferedBody);
          stats.successes += 1;
          logger({
            event: 'request_succeeded',
            requestId,
            model,
            route: result.route,
            status: upstream.status,
            durationMs: Date.now() - requestStartedAt,
          });
        } catch (error) {
          const canceled = requestAbort.signal.aborted || response.destroyed;
          if (canceled) stats.canceled += 1;
          else stats.failures += 1;
          logger({
            event: canceled ? 'request_canceled' : 'response_stream_failed',
            requestId,
            model,
            route: result.route,
            status: upstream.status,
            error: redactError(error),
          });
          if (!response.destroyed) response.destroy(error);
        }
        return;
      }

      const failureBody = result.bufferedBody || Buffer.from(await upstream.arrayBuffer());
      const classification = classifyFailure(upstream.status, failureBody.toString('utf8'));

      if (classification.category === 'rate_limit') {
        const cooldownMs = rateGate.markLimited(
          model,
          parseRetryAfterMs(upstream.headers.get('retry-after')),
        );
        rateRetries += 1;
        stats.rateRetries += 1;
        logger({
          event: 'rate_limit_queued',
          requestId,
          model,
          route: result.route,
          status: upstream.status,
          category: classification.category,
          attempt: rateRetries,
          waitMs: cooldownMs,
        });
        continue;
      }

      rateGate.markHealthy(model, permit);
      if (classification.retryable && upstreamRetries < maxUpstreamRetries) {
        upstreamRetries += 1;
        logger({
          event: 'upstream_retry',
          requestId,
          model,
          route: result.route,
          status: upstream.status,
          category: classification.category,
          attempt: upstreamRetries,
          waitMs: upstreamRetryMs,
        });
        try {
          await abortableSleep(upstreamRetryMs, requestAbort.signal);
        } catch {
          return;
        }
        continue;
      }

      stats.failures += 1;
      logger({
        event: 'request_rejected',
        requestId,
        model,
        route: result.route,
        status: upstream.status,
        category: classification.category,
        durationMs: Date.now() - requestStartedAt,
      });
      await forwardResponse(upstream, response, {
        'x-agnes-route': result.route,
        'x-agnes-rate-retries': String(rateRetries),
      }, failureBody);
      return;
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
      jsonResponse(response, 200, {
        status: 'ok',
        uptimeSeconds: Math.floor((Date.now() - startedAt) / 1_000),
        stats,
        routes: routes.snapshot(),
        rateLimits: rateGate.snapshot(),
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
        data: models.map((id) => ({ id, object: 'model', owned_by: 'agnes' })),
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
      if (response.headersSent || response.destroyed) {
        if (!response.destroyed) response.destroy(error);
        return;
      }
      stats.failures += 1;
      logger({ event: 'gateway_error', error: redactError(error) });
      jsonResponse(response, 500, {
        error: { type: 'gateway_error', message: 'Unexpected local gateway error; see the gateway log.' },
      });
    }
  });

  return {
    server,
    stats,
    rateGate,
    routes,
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
  RateGate,
  RouteSelector,
  buildUpstreamUrl,
  classifyFailure,
  createAgnesGateway,
  createJsonlLogger,
  isSafeConnectFailure,
  parseRetryAfterMs,
  redactError,
};
