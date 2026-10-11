import assert from 'node:assert/strict';
import test from 'node:test';

import {runBoundedCase} from './websocketExchangeCaseLifecycle.mjs';

test('a timed-out case persists its last stage and quiesces during teardown', async() => {
  let active = true;
  let released = false;
  let stage = 'server_dh_received';
  let abortReason = 'unknown';
  let cleanupComplete = false;
  const snapshots = [];
  let releaseOperation;
  const operationGate = new Promise((resolve) => { releaseOperation = resolve; });
  const row = () => ({
    case: 'A',
    client_stage: stage,
    client_abort_reason: abortReason,
    plain_requests_on_socket: 2,
    retry_reused_socket: false,
    third_constructor: 'req_dh_params',
    proto_error_404: false
  });
  const persist = () => snapshots.push(JSON.stringify([row()]));

  const result = await runBoundedCase({
    execute: async() => {
      await operationGate;
      if(active) {
        stage = 'dh_gen_verified';
        persist();
      }
    },
    onTimeout: () => {
      abortReason = 'timeout';
      persist();
    },
    teardown: () => {
      active = false;
      cleanupComplete = true;
      released = true;
      releaseOperation();
    },
    isQuiescent: () => cleanupComplete && released,
    executionBudgetMs: 10,
    teardownBudgetMs: 100
  });

  assert.equal(result.status, 'timed_out');
  assert.equal(result.quiescent, true);
  assert.equal(result.verificationPassed, false);
  const savedRows = JSON.parse(snapshots.at(-1));
  assert.deepEqual(Object.keys(savedRows[0]), [
    'case', 'client_stage', 'client_abort_reason', 'plain_requests_on_socket',
    'retry_reused_socket', 'third_constructor', 'proto_error_404'
  ]);
  assert.equal(savedRows[0].client_stage, 'server_dh_received');
  assert.equal(savedRows[0].client_abort_reason, 'timeout');
  assert.equal(savedRows[0].plain_requests_on_socket, 2);
  assert.equal(stage, 'server_dh_received');
});

test('successful and unexpectedly rejected cases both run bounded teardown', async() => {
  for(const execute of [async() => 'complete', async() => { throw new Error('discarded'); }]) {
    let cleanupComplete = false;
    const result = await runBoundedCase({
      execute,
      onTimeout: () => assert.fail('completed execution must not time out'),
      teardown: () => { cleanupComplete = true; },
      isQuiescent: () => cleanupComplete,
      executionBudgetMs: 100,
      teardownBudgetMs: 100
    });

    assert.equal(cleanupComplete, true);
    assert.equal(result.quiescent, true);
    assert.equal(result.verificationPassed, result.status === 'completed');
    assert.notEqual(result.status, 'teardown_failed');
  }
});
