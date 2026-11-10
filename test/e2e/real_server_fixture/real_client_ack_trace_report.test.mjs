import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import {test} from 'node:test';

import {
  createMessageAckTraceEvidence,
  isMessageCountFailure,
  parseRealClientBrowserReport
} from './real_client_ack_trace_report.mjs';

const fixtureRoot = new URL('.', import.meta.url);
const workflow = readFileSync(new URL('../../../.github/workflows/real-client-browser-flow.yml', fixtureRoot), 'utf8');
const runner = readFileSync(new URL('./run_real_client_browser_flow.mjs', import.meta.url), 'utf8');

test('parses the pinned browser report while ignoring pnpm output', () => {
  assert.equal(parseRealClientBrowserReport('> test:real-client'), undefined);
  assert.equal(parseRealClientBrowserReport('{not json}'), undefined);
  assert.deepEqual(parseRealClientBrowserReport(JSON.stringify({status: 'passed'})), {status: 'passed'});
});

test('recognizes only failed message-count checks', () => {
  assert.equal(isMessageCountFailure({status: 'failed', stage: 'alice_group_message_message_count'}), true);
  assert.equal(isMessageCountFailure({status: 'failed', stage: 'alice_direct_message_ack_trace'}), false);
  assert.equal(isMessageCountFailure({status: 'passed', stage: 'alice_direct_message_count'}), false);
});

test('builds evidence with both account traces and the failing check name', () => {
  const browserReport = {
    status: 'failed',
    stage: 'alice_group_message_message_count',
    messageSendAcknowledgementTraces: {
      alice: [{token: 'send-1-abcdef12', checkName: 'alice_group_message_message_count', events: []}],
      bob: [{token: 'send-2-abcdef12', checkName: 'bob_group_message_message_count', events: []}]
    },
    messageSendAcknowledgementTraceDiagnostics: {alice: {switchEnabled: true}, bob: {switchEnabled: true}}
  };

  assert.deepEqual(createMessageAckTraceEvidence(browserReport), {
    failedCheck: 'alice_group_message_message_count',
    traces: browserReport.messageSendAcknowledgementTraces,
    diagnostics: browserReport.messageSendAcknowledgementTraceDiagnostics
  });
});

test('pins the merged web acknowledgement trace while preserving the guest signup revision', () => {
  assert.match(workflow, /REAL_CLIENT_WEB_REVISION: 8b28c847bc1ff07c711ee4fa9c3a83fc5d8b4324/);
  assert.match(workflow, /REAL_CLIENT_GUEST_WEB_REVISION: 8e23fd09eac68f5ac1beb2468f76d0ffed28f814/);
});

test('enables acknowledgement tracing for the primary web build and captures the browser report', () => {
  assert.match(runner, /VITE_REAL_CLIENT_ACK_TRACE: '1'/);
  assert.match(runner, /captureStdout: true/);
  assert.match(runner, /parseRealClientBrowserReport\(/);
  assert.match(runner, /messageSendAcknowledgementTraces/);
});
