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
  parseRetryAfterMs,
  redactError,
} = require('../lib/openai-pool');

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
  fs.writeFileSync(keyFilePath, 'sk-first\nsk-second\n', 'utf8');
  const attempts = [];

  const upstream = http.createServer(async (request, response) => {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const authorization = request.headers.authorization;
    attempts.push({ authorization, body: Buffer.concat(chunks).toString('utf8') });

    if (authorization === 'Bearer sk-first') {
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
    'Bearer sk-first',
    'Bearer sk-second',
  ]);
  assert.equal(attempts.every((item) => item.body.includes('deepseek-v4-flash')), true);
  assert.equal(events.some((event) => event.event === 'upstream_retry'), true);
  assert.equal(events.some((event) => event.event === 'request_succeeded'), true);
});

test('health endpoint hot-reloads the key file without revealing keys', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'sensenova-pool-health-'));
  const keyFilePath = path.join(temporaryDir, 'keys');
  fs.writeFileSync(keyFilePath, 'sk-one\n', 'utf8');
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

  fs.writeFileSync(keyFilePath, 'sk-one\nsk-two\n', 'utf8');
  response = await fetch(`http://127.0.0.1:${address.port}/health`);
  health = await response.json();
  assert.equal(health.accountCount, 2);
  assert.equal(JSON.stringify(health).includes('sk-one'), false);
  assert.equal(JSON.stringify(health).includes('sk-two'), false);
});
