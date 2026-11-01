import assert from 'node:assert/strict';
import {once} from 'node:events';
import {test} from 'node:test';
import {spawnTracked, terminate, terminateFixture} from './process_tree.mjs';

const delay = (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds));

function watchOutput(child) {
  let output = '';
  const waiters = [];
  child.stdout.setEncoding('utf8');
  child.stdout.on('data', (chunk) => {
    output += chunk;
    for(let index = waiters.length - 1; index >= 0; index--) {
      if(output.includes(waiters[index].expected)) {
        waiters[index].resolve();
        waiters.splice(index, 1);
      }
    }
  });
  return {
    output: () => output,
    waitFor: (expected) => output.includes(expected) ? Promise.resolve() : new Promise((resolve) => waiters.push({expected, resolve}))
  };
}

function processExists(pid) {
  try {
    process.kill(pid, 0);
    return true;
  } catch(error) {
    if(error.code === 'ESRCH') return false;
    throw error;
  }
}

async function runWithCleanup(operation, cleanup) {
  let operationFailed = false;
  let operationError;
  try {
    await operation();
  } catch(error) {
    operationFailed = true;
    operationError = error;
  }

  let cleanupFailed = false;
  let cleanupError;
  try {
    await cleanup();
  } catch(error) {
    cleanupFailed = true;
    cleanupError = error;
  }

  if(operationFailed && cleanupFailed) {
    throw new AggregateError([operationError, cleanupError], 'operation and cleanup both failed');
  }
  if(operationFailed) throw operationError;
  if(cleanupFailed) throw cleanupError;
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
  await runWithCleanup(async () => {
    const leaderClosed = once(leader, 'close');
    if(timeoutBeforeSignal) await delay(timeoutBeforeSignal);

    const termination = terminate(leader, signal, {graceMs: 1_000});
    await leaderClosed;
    await delay(25);
    assert.equal(processExists(grandchildPid), true, 'grandchild should outlive its process-group leader after SIGTERM/SIGINT');
    assert.equal(await termination, true, 'termination should escalate the process group after its leader exits');
    assert.equal(processExists(grandchildPid), false, 'surviving grandchild should be gone before cleanup continues');
  }, async () => {
    let cleanupError;
    try {
      process.kill(-leader.pid, 'SIGKILL');
    } catch(error) {
      if(error.code !== 'ESRCH') cleanupError = error;
    }
    await delay(25);
    if(cleanupError) throw cleanupError;
  });
}

test('reports cleanup failures alongside operation failures', async () => {
  const operationError = new Error('test body failed');
  const cleanupError = new Error('process-group cleanup failed');
  let failure;
  try {
    await runWithCleanup(() => { throw operationError; }, () => { throw cleanupError; });
  } catch(error) {
    failure = error;
  }

  assert(failure instanceof AggregateError);
  assert.deepEqual(failure.errors, [operationError, cleanupError]);
});

test('cancellation and timeout terminate surviving process-group descendants', {skip: process.platform === 'win32'}, async (t) => {
  await t.test('workflow cancellation', () => verifyTreeTermination('SIGINT', 0));
  await t.test('command timeout', () => verifyTreeTermination('SIGTERM', 25));
});

test('fixture termination leaves time for a long TERM cleanup trap', {skip: process.platform === 'win32'}, async () => {
  const script = [
    "trap 'sleep 5.25; printf \"cleanup=verified\\n\"; exit 143' TERM",
    "printf 'ready\\n'",
    'while :; do sleep 30; done'
  ].join('\n');
  const leader = spawnTracked('bash', ['-c', script], {stdio: ['ignore', 'pipe', 'ignore']});
  const output = watchOutput(leader);
  try {
    await output.waitFor('ready\n');
    const closed = once(leader, 'close');
    const termination = terminateFixture(leader, 'SIGTERM');
    const [code, signal] = await closed;
    assert.equal(await termination, true);
    assert.equal(code, 143);
    assert.equal(signal, null);
    assert.match(output.output(), /cleanup=verified/);
  } finally {
    await terminate(leader, 'SIGKILL');
  }
});
