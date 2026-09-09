#!/usr/bin/env node
'use strict';

const fs = require('node:fs');
const path = require('node:path');
const { Agent, ProxyAgent, fetch } = require('undici');
const { createAgnesGateway, createJsonlLogger } = require('./lib/agnes-gateway');
const { createClashRecovery } = require('./lib/clash-recovery');

const MODELS = [
  'agnes-2.0-flash',
  'agnes-2.5-flash',
  'agnes-3.0-flash',
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
const apiKey = process.env.AGNES_API_KEY;
if (!apiKey) {
  console.error('[Agnes Proxy] Startup failed: AGNES_API_KEY is not set.');
  process.exit(1);
}

const host = process.env.AGNES_PROXY_HOST || '127.0.0.1';
const port = envInteger('AGNES_PROXY_PORT', 18788);
const upstreamBaseURL = process.env.AGNES_PROXY_UPSTREAM || 'https://apihub.agnes-ai.com/v1';
const proxyUrl = process.env.AGNES_PROXY_CLASH_URL || 'http://127.0.0.1:7890';
const logDir = process.env.AGNES_PROXY_LOG_DIR
  ? path.resolve(process.env.AGNES_PROXY_LOG_DIR)
  : path.join(rootDir, 'logs', 'agnes-proxy');
const defaultRecoveryScript = path.resolve(
  rootDir,
  '..',
  'android-install',
  'tools',
  'clash-node-helper.ps1',
);
const recoveryScript = process.env.AGNES_CLASH_RECOVERY_SCRIPT
  ? path.resolve(process.env.AGNES_CLASH_RECOVERY_SCRIPT)
  : defaultRecoveryScript;

const logger = createJsonlLogger(logDir);
const directDispatcher = new Agent();
const clashDispatcher = new ProxyAgent(proxyUrl);
const recovery = createClashRecovery({
  scriptPath: recoveryScript,
  targetUrl: `${upstreamBaseURL.replace(/\/$/, '')}/models`,
  timeoutMs: envInteger('AGNES_CLASH_RECOVERY_TIMEOUT_MS', 180_000),
  nodeTestTimeoutMs: envInteger('AGNES_CLASH_NODE_TIMEOUT_MS', 3_000),
  cooldownMs: envInteger('AGNES_CLASH_RECOVERY_COOLDOWN_MS', 5 * 60_000),
  logger,
});

if (!fs.existsSync(recoveryScript)) {
  console.warn(`[Agnes Proxy] Clash recovery helper not found; automatic node switching is disabled: ${recoveryScript}`);
}

const gateway = createAgnesGateway({
  apiKey,
  localToken: process.env.AGNES_PROXY_LOCAL_TOKEN || 'local-agnes-proxy',
  upstreamBaseURL,
  logDir,
  logger,
  fetchImpl: fetch,
  dispatchers: { direct: directDispatcher, clash: clashDispatcher },
  recoverNetwork: recovery.run,
  maxQueueMs: envInteger('AGNES_PROXY_MAX_QUEUE_MS', 10 * 60_000),
  requestTimeoutMs: envInteger('AGNES_PROXY_REQUEST_TIMEOUT_MS', 5 * 60_000),
  rateCooldownMs: envInteger('AGNES_PROXY_RATE_COOLDOWN_MS', 60_000),
  upstreamRetryMs: envInteger('AGNES_PROXY_UPSTREAM_RETRY_MS', 5_000),
  maxUpstreamRetries: envInteger('AGNES_PROXY_MAX_UPSTREAM_RETRIES', 2),
  preferClashMs: envInteger('AGNES_PROXY_PREFER_CLASH_MS', 5 * 60_000),
  models: MODELS,
});

gateway.listen(port, host).then(() => {
  console.log(`[Agnes Proxy] OpenAI-compatible gateway listening on http://${host}:${port}/v1`);
  console.log(`[Agnes Proxy] Health: http://${host}:${port}/health`);
  console.log(`[Agnes Proxy] Direct route with Clash fallback: ${proxyUrl}`);
  console.log(`[Agnes Proxy] Clash recovery: ${recovery.snapshot().enabled ? 'enabled' : 'disabled'}`);
  console.log(`[Agnes Proxy] Logs: ${logDir}`);
}).catch((error) => {
  console.error(`[Agnes Proxy] Startup failed: ${error.message}`);
  process.exitCode = 1;
});

async function shutdown(signal) {
  console.log(`[Agnes Proxy] Received ${signal}; shutting down.`);
  try {
    await gateway.close();
  } finally {
    await Promise.allSettled([directDispatcher.close(), clashDispatcher.close()]);
    process.exit(0);
  }
}

process.once('SIGINT', () => shutdown('SIGINT'));
process.once('SIGTERM', () => shutdown('SIGTERM'));
