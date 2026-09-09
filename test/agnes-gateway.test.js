'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');

const {
  buildUpstreamUrl,
  classifyFailure,
  createAgnesGateway,
  isSafeConnectFailure,
  parseRetryAfterMs,
} = require('../lib/agnes-gateway');

function connectError(code = 'ENETUNREACH') {
  const error = new TypeError('fetch failed');
  error.cause = { code };
  return error;
}

async function startGateway(t, options = {}) {
  const events = [];
  const gateway = createAgnesGateway({
    apiKey: 'agnes-private-key-for-test',
    localToken: 'local-test-token',
    logDir: '.',
    models: ['agnes-3.0-flash'],
    dispatchers: { direct: 'direct-dispatcher', clash: 'clash-dispatcher' },
    logger: (event) => events.push(event),
    rateCooldownMs: 1,
    upstreamRetryMs: 1,
    maxQueueMs: 1_000,
    requestTimeoutMs: 2_000,
    ...options,
  });
  const address = await gateway.listen(0);
  t.after(() => gateway.close());
  return {
    gateway,
    events,
    endpoint: `http://127.0.0.1:${address.port}`,
  };
}

function request(endpoint, prompt = 'Reply OK.') {
  return fetch(`${endpoint}/v1/chat/completions`, {
    method: 'POST',
    headers: {
      authorization: 'Bearer local-test-token',
      'content-type': 'application/json',
    },
    body: JSON.stringify({
      model: 'agnes-3.0-flash',
      messages: [{ role: 'user', content: prompt }],
    }),
  });
}

test('Agnes utility functions recognize rate limits and safe connection failures', () => {
  assert.equal(
    buildUpstreamUrl('https://apihub.agnes-ai.com/v1', '/v1/chat/completions?x=1').href,
    'https://apihub.agnes-ai.com/v1/chat/completions?x=1',
  );
  assert.equal(classifyFailure(429, '').category, 'rate_limit');
  assert.equal(classifyFailure(400, 'inference exceeds tpm/rpm limit').category, 'rate_limit');
  assert.equal(parseRetryAfterMs('2'), 2_000);
  assert.equal(isSafeConnectFailure(connectError('ENOTFOUND')), true);
  assert.equal(isSafeConnectFailure(connectError('ECONNRESET')), false);
});

test('gateway falls back from an unreachable direct route to Clash', async (t) => {
  const calls = [];
  const fetchImpl = async (_url, init) => {
    calls.push(init.dispatcher);
    if (init.dispatcher === 'direct-dispatcher') throw connectError();
    return new Response(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  };
  const { endpoint, events } = await startGateway(t, { fetchImpl });

  const response = await request(endpoint);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('x-agnes-route'), 'clash');
  assert.equal((await response.json()).choices[0].message.content, 'OK');
  assert.deepEqual(calls, ['direct-dispatcher', 'clash-dispatcher']);
  assert.equal(events.some((event) => event.event === 'route_connect_failed'), true);
  assert.equal(events.some((event) => event.event === 'request_succeeded'), true);
});

test('gateway queues a rate-limited request and retries the same Agnes account', async (t) => {
  let calls = 0;
  const fetchImpl = async () => {
    calls += 1;
    if (calls === 1) {
      return new Response(JSON.stringify({ error: { message: 'too many requests' } }), {
        status: 429,
        headers: { 'content-type': 'application/json' },
      });
    }
    return new Response(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  };
  const { endpoint, events } = await startGateway(t, { fetchImpl });

  const response = await request(endpoint);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('x-agnes-rate-retries'), '1');
  assert.equal(calls, 2);
  assert.equal(events.some((event) => event.event === 'rate_limit_queued'), true);
});

test('gateway invokes Clash node recovery only after safe route connection failures', async (t) => {
  const calls = [];
  let recovered = false;
  let recoveries = 0;
  const fetchImpl = async (_url, init) => {
    calls.push(init.dispatcher);
    if (init.dispatcher === 'clash-dispatcher' && recovered) {
      return new Response(JSON.stringify({ choices: [{ message: { content: 'RECOVERED' } }] }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      });
    }
    throw connectError('ECONNREFUSED');
  };
  const recoverNetwork = async () => {
    recoveries += 1;
    recovered = true;
    return { attempted: true, ok: true, reason: 'node_switched' };
  };
  const { endpoint } = await startGateway(t, { fetchImpl, recoverNetwork });

  const response = await request(endpoint);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get('x-agnes-route'), 'clash');
  assert.equal((await response.json()).choices[0].message.content, 'RECOVERED');
  assert.equal(recoveries, 1);
  assert.deepEqual(calls, [
    'direct-dispatcher',
    'clash-dispatcher',
    'clash-dispatcher',
  ]);
});

test('ambiguous transport failures are not retried and cannot duplicate an inference', async (t) => {
  let calls = 0;
  let recoveries = 0;
  const fetchImpl = async () => {
    calls += 1;
    throw connectError('ECONNRESET');
  };
  const { endpoint } = await startGateway(t, {
    fetchImpl,
    recoverNetwork: async () => {
      recoveries += 1;
      return { attempted: true, ok: true };
    },
  });

  const response = await request(endpoint);
  const body = await response.json();
  assert.equal(response.status, 502);
  assert.equal(body.error.code, 'ambiguous_transport_failure');
  assert.equal(calls, 1);
  assert.equal(recoveries, 0);
});

test('health and logs expose neither the API key nor prompt bodies', async (t) => {
  const privateKey = 'agnes-private-key-for-test';
  const privatePrompt = 'private prompt must never be logged';
  const events = [];
  const fetchImpl = async (_url, init) => {
    assert.equal(init.headers.authorization, `Bearer ${privateKey}`);
    return new Response(JSON.stringify({ choices: [{ message: { content: 'OK' } }] }), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    });
  };
  const { endpoint } = await startGateway(t, {
    apiKey: privateKey,
    fetchImpl,
    logger: (event) => events.push(event),
  });

  const response = await request(endpoint, privatePrompt);
  assert.equal(response.status, 200);
  await response.arrayBuffer();
  const healthResponse = await fetch(`${endpoint}/health`);
  const serialized = `${await healthResponse.text()}\n${JSON.stringify(events)}`;
  assert.equal(serialized.includes(privateKey), false);
  assert.equal(serialized.includes(privatePrompt), false);
});
