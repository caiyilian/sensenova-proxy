'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const {
  AccountPool,
  buildUpstreamUrl,
  classifyFailure,
  createGateway,
  parseKeyFile,
  parseKeyFileDetailed,
  parseRetryAfterMs,
  redactError,
  validateSenseNovaApiKey,
} = require('../lib/openai-pool');

const KEY_ONE = `sk-${'a'.repeat(32)}`;
const KEY_TWO = `sk-${'b'.repeat(32)}`;
const KEY_THREE = `sk-${'c'.repeat(32)}`;

function waitFor(predicate, timeoutMs = 2_000) {
  const startedAt = Date.now();
  return new Promise((resolve, reject) => {
    const check = () => {
      if (predicate()) {
        resolve();
        return;
      }
      if (Date.now() - startedAt >= timeoutMs) {
        reject(new Error('Timed out waiting for condition'));
        return;
      }
      setTimeout(check, 10);
    };
    check();
  });
}

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      server.removeListener('error', reject);
      resolve(server.address());
    });
  });
}

function close(server) {
  return new Promise((resolve, reject) => {
    server.close((error) => (error ? reject(error) : resolve()));
  });
}

test('parseKeyFile ignores comments, blanks, and duplicates', () => {
  assert.deepEqual(
    parseKeyFile('\uFEFF# private keys\n sk-one \n\n sk-two\nsk-one\n'),
    ['sk-one', 'sk-two'],
  );
});

test('SenseNova key parsing reports malformed and duplicate lines without exposing values', () => {
  assert.equal(validateSenseNovaApiKey(KEY_ONE), null);
  assert.equal(validateSenseNovaApiKey('not-a-key'), 'missing_sk_prefix');
  assert.equal(validateSenseNovaApiKey('sk-too-short'), 'unexpected_length_12');

  const parsed = parseKeyFileDetailed([
    '# private keys',
    KEY_ONE,
    'not-a-key',
    KEY_ONE,
    '',
  ].join('\n'));
  assert.deepEqual(parsed.entries, [{ apiKey: KEY_ONE, lineNumber: 2 }]);
  assert.deepEqual(parsed.invalidLines, [{ lineNumber: 3, reason: 'missing_sk_prefix' }]);
  assert.deepEqual(parsed.duplicateLines, [{ lineNumber: 4, duplicateOfLine: 2 }]);
  const metadata = JSON.stringify({
    invalidLines: parsed.invalidLines,
    duplicateLines: parsed.duplicateLines,
  });
  assert.equal(metadata.includes(KEY_ONE), false);
  assert.equal(metadata.includes('not-a-key'), false);
});

test('classifyFailure recognizes SenseNova retryable failures', () => {
  assert.deepEqual(classifyFailure(400, 'inference exceeds tpm/rpm limit'), {
    category: 'rate_limit', retryable: true, scope: 'model',
  });
  assert.equal(classifyFailure(429, '').category, 'rate_limit');
  assert.equal(classifyFailure(400, 'token plan entitlement exhausted').category, 'quota');
  assert.equal(classifyFailure(429, 'token plan entitlement exhausted').category, 'quota');
  assert.equal(classifyFailure(401, 'bad key').category, 'auth');
  assert.equal(classifyFailure(503, 'temporary').category, 'upstream');
  assert.equal(classifyFailure(400, 'invalid arguments').retryable, false);
});

test('utility functions preserve paths and redact credentials', () => {
  assert.equal(
    buildUpstreamUrl('https://token.sensenova.cn/v1', '/v1/chat/completions?x=1').href,
    'https://token.sensenova.cn/v1/chat/completions?x=1',
  );
  assert.equal(parseRetryAfterMs('3'), 3_000);
  assert.equal(redactError(new Error('Bearer sk-secret-value failed')), '[redacted] failed');
});

test('TPM/RPM cooldown stays fixed instead of growing exponentially', () => {
  let currentTime = 1_000;
  const pool = new AccountPool({ now: () => currentTime });
  const account = { fingerprint: 'test-account' };
  const failure = { category: 'rate_limit', retryable: true, scope: 'model' };

  assert.equal(pool.markFailure(account, 'deepseek-v4-flash', failure), 60_000);
  currentTime += 60_000;
  assert.equal(pool.markFailure(account, 'deepseek-v4-flash', failure), 60_000);
});

test('gateway retries a rate-limited key and streams only the successful response', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-test-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n${KEY_TWO}\n`, 'utf8');
  const attempts = [];

  const upstream = http.createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const authorization = request.headers.authorization;
    attempts.push({ authorization, body: Buffer.concat(chunks).toString('utf8') });

    if (authorization === `Bearer ${KEY_ONE}`) {
      response.writeHead(429, { 'content-type': 'application/json' });
      response.end(JSON.stringify({ error: { message: 'inference exceeds tpm/rpm limit' } }));
      return;
    }

    response.writeHead(200, { 'content-type': 'application/json' });
    response.end(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }));
  });
  const upstreamAddress = await listen(upstream);

  const events = [];
  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    upstreamBaseURL: `http://127.0.0.1:${upstreamAddress.port}/v1`,
    maxQueueMs: 500,
    requestTimeoutMs: 2_000,
    models: ['deepseek-v4-flash'],
    logger: (event) => events.push(event),
  });
  const gatewayAddress = await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    await close(upstream);
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  const response = await fetch(
    `http://127.0.0.1:${gatewayAddress.port}/v1/chat/completions`,
    {
      method: 'POST',
      headers: {
        authorization: 'Bearer test-token',
        'content-type': 'application/json',
      },
      body: JSON.stringify({
        model: 'deepseek-v4-flash',
        messages: [{ role: 'user', content: 'Reply OK.' }],
      }),
    },
  );

  assert.equal(response.status, 200);
  assert.equal(response.headers.get('x-sensenova-pool-account'), 'account-2');
  assert.equal(response.headers.get('x-sensenova-pool-attempts'), '2');
  assert.equal((await response.json()).choices[0].message.content, 'OK');
  assert.deepEqual(attempts.map((item) => item.authorization), [
    `Bearer ${KEY_ONE}`,
    `Bearer ${KEY_TWO}`,
  ]);
  assert.equal(attempts.every((item) => item.body.includes('deepseek-v4-flash')), true);
  assert.equal(events.some((event) => event.event === 'upstream_retry'), true);
  assert.equal(events.some((event) => event.event === 'request_succeeded'), true);
});

test('gateway paces retries after a request has exhausted the whole pool', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-pacing-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n${KEY_TWO}\n`, 'utf8');
  const attemptTimes = [];

  const fetchImpl = async () => {
    attemptTimes.push(Date.now());
    if (attemptTimes.length < 4) {
      return new Response(JSON.stringify({ error: { message: 'inference exceeds tpm/rpm limit' } }), {
        status: 429,
        headers: { 'content-type': 'application/json' },
      });
    }
    return new Response(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  };

  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    maxQueueMs: 1_000,
    requestTimeoutMs: 2_000,
    postSweepProbeIntervalMs: 30,
    poolOptions: {
      cooldowns: {
        rate_limit: { baseMs: 1, maxMs: 1, exponential: false },
      },
    },
    fetchImpl,
    logger: () => {},
  });
  const gatewayAddress = await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  const response = await fetch(
    `http://127.0.0.1:${gatewayAddress.port}/v1/chat/completions`,
    {
      method: 'POST',
      headers: {
        authorization: 'Bearer test-token',
        'content-type': 'application/json',
      },
      body: JSON.stringify({ model: 'deepseek-v4-flash', messages: [] }),
    },
  );

  assert.equal(response.status, 200);
  assert.equal(attemptTimes.length, 4);
  assert.equal(attemptTimes[3] - attemptTimes[2] >= 20, true);
  assert.equal(gateway.stats.pacedProbes >= 1, true);
});

test('health endpoint hot-reloads the key file without revealing keys', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-health-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n`, 'utf8');
  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    logger: () => {},
  });
  const address = await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  let response = await fetch(`http://127.0.0.1:${address.port}/health`);
  let health = await response.json();
  assert.equal(health.accountCount, 1);

  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n${KEY_TWO}\n`, 'utf8');
  response = await fetch(`http://127.0.0.1:${address.port}/health`);
  health = await response.json();
  assert.equal(health.accountCount, 2);
  assert.equal(JSON.stringify(health).includes(KEY_ONE), false);
  assert.equal(JSON.stringify(health).includes(KEY_TWO), false);
});

test('key watcher logs additions, removals, replacements, and invalid lines safely', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-watch-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n`, 'utf8');
  const events = [];
  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    keyWatchIntervalMs: 20,
    keyReloadDebounceMs: 10,
    logger: (event) => events.push(event),
  });
  await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  assert.equal(gateway.keyStore.accounts.length, 1);
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n${KEY_TWO}\n`, 'utf8');
  await waitFor(() => gateway.keyStore.accounts.length === 2);

  fs.writeFileSync(keyFilePath, `${KEY_TWO}\nnot-a-key\n${KEY_THREE}\n`, 'utf8');
  await waitFor(() => (
    gateway.keyStore.accounts.length === 2
    && gateway.keyStore.accounts[1].keyRef !== gateway.keyStore.accounts[0].keyRef
    && gateway.keyStore.invalidLines.length === 1
  ));

  const lastReload = events.filter((event) => event.event === 'keys_reloaded').at(-1);
  assert.equal(lastReload.addedCount, 1);
  assert.equal(lastReload.removedCount, 1);
  assert.equal(lastReload.invalidLineCount, 1);
  assert.deepEqual(lastReload.invalidLines, [{ lineNumber: 2, reason: 'missing_sk_prefix' }]);
  const serializedEvents = JSON.stringify(events);
  assert.equal(serializedEvents.includes(KEY_ONE), false);
  assert.equal(serializedEvents.includes(KEY_TWO), false);
  assert.equal(serializedEvents.includes(KEY_THREE), false);
  assert.equal(serializedEvents.includes('not-a-key'), false);
});

test('an unreadable key file retains the last loaded accounts and later recovers', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-retain-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n`, 'utf8');
  const events = [];
  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    keyWatchIntervalMs: 20,
    keyReloadDebounceMs: 10,
    logger: (event) => events.push(event),
  });
  const address = await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  fs.rmSync(keyFilePath);
  await waitFor(() => events.some((event) => event.event === 'key_reload_failed'));
  let response = await fetch(`http://127.0.0.1:${address.port}/health`);
  let health = await response.json();
  assert.equal(response.status, 200);
  assert.equal(health.accountCount, 1);
  assert.equal(health.keyFileReadError, true);

  fs.writeFileSync(keyFilePath, `${KEY_TWO}\n`, 'utf8');
  await waitFor(() => gateway.keyStore.accounts[0]?.keyRef !== health.accounts[0].keyRef);
  response = await fetch(`http://127.0.0.1:${address.port}/health`);
  health = await response.json();
  assert.equal(health.accountCount, 1);
  assert.equal(health.keyFileReadError, false);
});

test('an upstream authentication failure quarantines only that account', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-auth-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, `${KEY_ONE}\n${KEY_TWO}\n`, 'utf8');
  const attempts = [];
  const events = [];
  const gateway = createGateway({
    keyFilePath,
    logDir: path.join(temporaryDir, 'logs'),
    localToken: 'test-token',
    logger: (event) => events.push(event),
    fetchImpl: async (_url, options) => {
      const authorization = options.headers.get('authorization');
      attempts.push(authorization);
      if (authorization === `Bearer ${KEY_ONE}`) {
        return new Response(JSON.stringify({ error: { message: 'invalid api key' } }), {
          status: 401,
          headers: { 'content-type': 'application/json' },
        });
      }
      return new Response(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      });
    },
  });
  const address = await gateway.listen(0);

  t.after(async () => {
    await gateway.close();
    fs.rmSync(temporaryDir, { recursive: true, force: true });
  });

  const response = await fetch(`http://127.0.0.1:${address.port}/v1/chat/completions`, {
    method: 'POST',
    headers: {
      authorization: 'Bearer test-token',
      'content-type': 'application/json',
    },
    body: JSON.stringify({ model: 'deepseek-v4-flash', messages: [] }),
  });

  assert.equal(response.status, 200);
  assert.deepEqual(attempts, [`Bearer ${KEY_ONE}`, `Bearer ${KEY_TWO}`]);
  const quarantine = events.find((event) => event.event === 'account_quarantined');
  assert.equal(quarantine.category, 'auth');
  assert.equal(quarantine.keyLine, 1);
  assert.equal(JSON.stringify(events).includes(KEY_ONE), false);
  assert.equal(JSON.stringify(events).includes(KEY_TWO), false);
});
