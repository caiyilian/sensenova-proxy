#!/usr/bin/env node
'use strict';

const path = require('path');
const { createGateway } = require('./lib/openai-pool');

const MODELS = [
  'deepseek-v4-flash',
  'sensenova-6.7-flash-lite',
  'glm-5.2',
  'deepseek-v4-pro',
  'kimi-k3',
  'sensenova-6.8-flash-lite',
];

function envInteger(name, fallback) {
  if (!process.env[name]) return fallback;
  const value = Number(process.env[name]);
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new Error(`${name} must be a non-negative integer`);
  }
  return value;
}

const rootDir = __dirname;
const host = process.env.SENSENOVA_POOL_HOST || '127.0.0.1';
const port = envInteger('SENSENOVA_POOL_PORT', 18787);
const keyFilePath = process.env.SENSENOVA_POOL_KEY_FILE
  ? path.resolve(process.env.SENSENOVA_POOL_KEY_FILE)
  : path.join(rootDir, 'sensenova_apikeys');
const logDir = process.env.SENSENOVA_POOL_LOG_DIR
  ? path.resolve(process.env.SENSENOVA_POOL_LOG_DIR)
  : path.join(rootDir, 'logs', 'openai-pool');

const gateway = createGateway({
  keyFilePath,
  logDir,
  localToken: process.env.SENSENOVA_POOL_LOCAL_TOKEN || 'local-sensenova-pool',
  upstreamBaseURL: process.env.SENSENOVA_POOL_UPSTREAM || 'https://token.sensenova.cn/v1',
  maxQueueMs: envInteger('SENSENOVA_POOL_MAX_QUEUE_MS', 10 * 60_000),
  postSweepProbeIntervalMs: envInteger('SENSENOVA_POOL_PROBE_INTERVAL_MS', 5_000),
  requestTimeoutMs: envInteger('SENSENOVA_POOL_REQUEST_TIMEOUT_MS', 5 * 60_000),
  expectedKeyLength: envInteger('SENSENOVA_POOL_EXPECTED_KEY_LENGTH', 35),
  keyWatchIntervalMs: envInteger('SENSENOVA_POOL_KEY_WATCH_MS', 1_000),
  keyReloadDebounceMs: envInteger('SENSENOVA_POOL_KEY_DEBOUNCE_MS', 250),
  models: MODELS,
});

gateway.listen(port, host).then(() => {
  console.log(`[SenseNova Pool] OpenAI-compatible gateway listening on http://${host}:${port}/v1`);
  console.log(`[SenseNova Pool] Health: http://${host}:${port}/health`);
  console.log(`[SenseNova Pool] Keys: ${keyFilePath}`);
  console.log(`[SenseNova Pool] Logs: ${logDir}`);
}).catch((error) => {
  console.error(`[SenseNova Pool] Startup failed: ${error.message}`);
  process.exitCode = 1;
});

async function shutdown(signal) {
  console.log(`[SenseNova Pool] Received ${signal}; shutting down.`);
  try {
    await gateway.close();
  } finally {
    process.exit(0);
  }
}

process.once('SIGINT', () => shutdown('SIGINT'));
process.once('SIGTERM', () => shutdown('SIGTERM'));
