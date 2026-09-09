'use strict';

const fs = require('node:fs');
const { spawn } = require('node:child_process');

function createClashRecovery(options = {}) {
  const scriptPath = options.scriptPath;
  const targetUrl = options.targetUrl || 'https://apihub.agnes-ai.com/v1/models';
  const timeoutMs = options.timeoutMs ?? 180_000;
  const nodeTestTimeoutMs = options.nodeTestTimeoutMs ?? 3_000;
  const cooldownMs = options.cooldownMs ?? 5 * 60_000;
  const logger = options.logger || (() => {});
  const spawnImpl = options.spawnImpl || spawn;
  const now = options.now || Date.now;
  let inFlight = null;
  let lastStartedAt = 0;
  let lastResult = null;

  function snapshot() {
    return {
      enabled: Boolean(scriptPath && fs.existsSync(scriptPath)),
      running: Boolean(inFlight),
      cooldownRemainingMs: Math.max(0, lastStartedAt + cooldownMs - now()),
      lastResult,
    };
  }

  async function run(context = {}) {
    if (!scriptPath || !fs.existsSync(scriptPath)) {
      return { attempted: false, ok: false, reason: 'recovery_script_missing' };
    }
    if (inFlight) return inFlight;
    if (lastStartedAt && now() - lastStartedAt < cooldownMs) {
      return { attempted: false, ok: false, reason: 'recovery_cooldown' };
    }

    lastStartedAt = now();
    inFlight = new Promise((resolve) => {
      const args = [
        '-NoLogo',
        '-NoProfile',
        '-NonInteractive',
        '-ExecutionPolicy',
        'Bypass',
        '-File',
        scriptPath,
        '-Action',
        'recover',
        '-Url',
        targetUrl,
        '-TimeoutMs',
        String(nodeTestTimeoutMs),
      ];
      let child;
      try {
        child = spawnImpl('powershell.exe', args, {
          stdio: 'ignore',
          windowsHide: true,
        });
      } catch (error) {
        const result = { attempted: true, ok: false, reason: error.code || 'spawn_failed' };
        lastResult = {
          ok: false,
          reason: result.reason,
          finishedAt: new Date().toISOString(),
        };
        logger({
          event: 'clash_recovery_process_failed',
          requestId: context.requestId,
          model: context.model,
          route: 'clash',
          category: result.reason,
        });
        resolve(result);
        return;
      }
      let settled = false;

      function finish(result) {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        context.signal?.removeEventListener('abort', onAbort);
        lastResult = {
          ok: result.ok,
          reason: result.reason,
          finishedAt: new Date().toISOString(),
        };
        logger({
          event: result.ok ? 'clash_recovery_process_succeeded' : 'clash_recovery_process_failed',
          requestId: context.requestId,
          model: context.model,
          route: 'clash',
          category: result.reason,
        });
        resolve(result);
      }

      function onAbort() {
        child.kill();
        finish({ attempted: true, ok: false, reason: 'client_disconnected' });
      }

      const timer = setTimeout(() => {
        child.kill();
        finish({ attempted: true, ok: false, reason: 'recovery_timeout' });
      }, timeoutMs);
      timer.unref?.();

      child.once('error', (error) => {
        finish({ attempted: true, ok: false, reason: error.code || 'spawn_failed' });
      });
      child.once('exit', (code) => {
        finish({
          attempted: true,
          ok: code === 0,
          reason: code === 0 ? 'node_switched' : `exit_${code ?? 'unknown'}`,
        });
      });

      if (context.signal?.aborted) onAbort();
      else context.signal?.addEventListener('abort', onAbort, { once: true });
    }).finally(() => {
      inFlight = null;
    });
    return inFlight;
  }

  return { run, snapshot };
}

module.exports = { createClashRecovery };
