'use strict';

const assert = require('node:assert/strict');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');

const { createClashRecovery } = require('../lib/clash-recovery');

test('Clash recovery is hidden, shared, and cooldown-protected', async (t) => {
  const temporaryDir = fs.mkdtempSync(path.join(os.tmpdir(), 'clash-recovery-test-'));
  const scriptPath = path.join(temporaryDir, 'helper.ps1');
  fs.writeFileSync(scriptPath, '# test helper', 'utf8');
  t.after(() => fs.rmSync(temporaryDir, { recursive: true, force: true }));

  const calls = [];
  const spawnImpl = (command, args, options) => {
    calls.push({ command, args, options });
    const child = new EventEmitter();
    child.kill = () => {};
    setImmediate(() => child.emit('exit', 0));
    return child;
  };
  const recovery = createClashRecovery({
    scriptPath,
    cooldownMs: 60_000,
    spawnImpl,
  });

  const [first, shared] = await Promise.all([recovery.run(), recovery.run()]);
  assert.equal(first.ok, true);
  assert.equal(shared.ok, true);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].command, 'powershell.exe');
  assert.equal(calls[0].options.windowsHide, true);
  assert.equal(calls[0].options.stdio, 'ignore');
  assert.equal(calls[0].args.includes(scriptPath), true);

  const cooled = await recovery.run();
  assert.deepEqual(cooled, { attempted: false, ok: false, reason: 'recovery_cooldown' });
  assert.equal(recovery.snapshot().lastResult.ok, true);
});
