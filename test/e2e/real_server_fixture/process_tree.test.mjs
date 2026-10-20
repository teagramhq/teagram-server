import assert from 'node:assert/strict';
import {once} from 'node:events';
import {test} from 'node:test';
import {spawnTracked, terminate} from './process_tree.mjs';

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch(error) {
    if(error.code === 'ESRCH') return false;
    throw error;
  }
}

async function startTree() {
  const grandchildCode = [
    "process.on('SIGINT', () => {});",
    "process.on('SIGTERM', () => {});",
    "require('node:fs').writeSync(3, 'ready');",
    'setInterval(() => {}, 1000);'
  ].join('');
  const leaderCode = [
    "const {spawn} = require('node:child_process');",
    `const grandchild = spawn(process.execPath, ['-e', ${JSON.stringify(grandchildCode)}], {stdio: ['ignore', 'ignore', 'ignore', 'pipe']});`,
    "grandchild.stdio[3].once('data', () => process.stdout.write(`${grandchild.pid}\\n`));",
    "process.on('SIGINT', () => process.exit(0));",
    "process.on('SIGTERM', () => process.exit(0));",
    'setInterval(() => {}, 1000);'
  ].join('');
  const leader = spawnTracked(process.execPath, ['-e', leaderCode], {stdio: ['ignore', 'pipe', 'inherit']});
  leader.stdout.setEncoding('utf8');
  const [pidLine] = await once(leader.stdout, 'data');
  return {leader, grandchildPid: Number(pidLine.trim())};
}

async function verifyTreeTermination(signal, timeoutBeforeSignal) {
  const {leader, grandchildPid} = await startTree();
  try {
    const leaderClosed = once(leader, 'close');
    if(timeoutBeforeSignal) await delay(timeoutBeforeSignal);

    const termination = terminate(leader, signal, {graceMs: 1_000});
    await leaderClosed;
    await delay(25);
    assert.equal(processExists(grandchildPid), true, 'grandchild should outlive its process-group leader after SIGTERM/SIGINT');
    assert.equal(await termination, true, 'termination should escalate the process group after its leader exits');
    assert.equal(processExists(grandchildPid), false, 'surviving grandchild should be gone before cleanup continues');
  } finally {
    try {
      process.kill(-leader.pid, 'SIGKILL');
    } catch(error) {
      if(error.code !== 'ESRCH') throw error;
    }
    await delay(25);
  }
}

test('cancellation and timeout terminate surviving process-group descendants', {skip: process.platform === 'win32'}, async (t) => {
  await t.test('workflow cancellation', () => verifyTreeTermination('SIGINT', 0));
  await t.test('command timeout', () => verifyTreeTermination('SIGTERM', 25));
});
