import assert from 'node:assert/strict';
import {test} from 'node:test';

import {
  collectMessageAckTraceEvidence,
  configureAckTraceBuildEnvironment,
  createMessageAckTraceEvidence,
  createRealClientBrowserReportCollector,
  isMessageCountFailure
} from './real_client_ack_trace_report.mjs';

function passingBrowserReport(alice = [{token: 'send-1-abcdef12', checkName: 'alice_first_message_message_count'}], bob = []) {
  return {
    status: 'passed',
    messageSendAcknowledgementTraces: {alice, bob}
  };
}

test('collects a browser report split across stdout chunks', () => {
  const browserReport = passingBrowserReport();
  const output = JSON.stringify(browserReport);
  const collector = createRealClientBrowserReportCollector();

  collector.write(output.slice(0, 19));
  collector.write(output.slice(19));

  assert.deepEqual(collector.finish(), browserReport);
});

test('collects a browser report when CRLF boundaries split between stdout chunks', () => {
  const browserReport = passingBrowserReport();
  const collector = createRealClientBrowserReportCollector();

  collector.write('pnpm runner output\r');
  collector.write(`\n${JSON.stringify(browserReport)}\r`);
  collector.write('\n');

  assert.deepEqual(collector.finish(), browserReport);
});

test('drops oversized reports and requires evidence for a successful browser run', () => {
  const collector = createRealClientBrowserReportCollector();
  collector.write(JSON.stringify({...passingBrowserReport(), padding: 'x'.repeat(2 * 1024 * 1024)}));

  assert.throws(() => collectMessageAckTraceEvidence({
    guestSignupOnly: false,
    runnerError: undefined,
    browserReport: collector.finish()
  }), /successful browser flow omitted Alice acknowledgement trace records/);
});

test('marks the failed message-count check and keeps both account arrays', () => {
  const browserReport = {
    status: 'failed',
    stage: 'alice_group_message_message_count',
    messageSendAcknowledgementTraces: {
      alice: [{token: 'send-1-abcdef12', checkName: 'alice_group_message_message_count', events: []}],
      bob: [{token: 'send-2-abcdef12', checkName: 'bob_first_message_message_count', events: []}]
    },
    messageSendAcknowledgementTraceDiagnostics: {alice: {switchEnabled: true}, bob: {switchEnabled: true}}
  };

  assert.equal(isMessageCountFailure(browserReport), true);
  assert.deepEqual(createMessageAckTraceEvidence(browserReport), {
    failedCheck: 'alice_group_message_message_count',
    traces: browserReport.messageSendAcknowledgementTraces,
    diagnostics: browserReport.messageSendAcknowledgementTraceDiagnostics
  });
  assert.deepEqual(collectMessageAckTraceEvidence({
    guestSignupOnly: false,
    runnerError: new Error('browser assertion failed'),
    browserReport
  }), createMessageAckTraceEvidence(browserReport));
});

test('passing reports keep Alice traces without inventing a failed check for Bob', () => {
  const browserReport = passingBrowserReport();

  assert.deepEqual(collectMessageAckTraceEvidence({
    guestSignupOnly: false,
    runnerError: undefined,
    browserReport
  }), {
    traces: browserReport.messageSendAcknowledgementTraces
  });
  assert.deepEqual(browserReport.messageSendAcknowledgementTraces.bob, []);
});

test('missing trace fields do not produce evidence', () => {
  assert.equal(createMessageAckTraceEvidence({status: 'passed'}), undefined);
});

test('a successful run without a report must fail evidence validation', () => {
  const collector = createRealClientBrowserReportCollector();
  collector.write('pnpm runner output\n{not json}\n');

  assert.throws(() => collectMessageAckTraceEvidence({
    guestSignupOnly: false,
    runnerError: undefined,
    browserReport: collector.finish()
  }), /successful browser flow omitted Alice acknowledgement trace records/);
});

test('a successful run with no Alice sends must fail evidence validation', () => {
  assert.throws(() => collectMessageAckTraceEvidence({
    guestSignupOnly: false,
    runnerError: undefined,
    browserReport: passingBrowserReport([], [])
  }), /successful browser flow omitted Alice acknowledgement trace records/);
});

test('acknowledgement tracing is enabled only for the main flow build', () => {
  const inheritedEnvironment = {KEEP_SETTING: 'yes', VITE_REAL_CLIENT_ACK_TRACE: '0'};

  assert.deepEqual(configureAckTraceBuildEnvironment(inheritedEnvironment, false), {
    KEEP_SETTING: 'yes',
    VITE_REAL_CLIENT_ACK_TRACE: '1'
  });
  assert.deepEqual(configureAckTraceBuildEnvironment(inheritedEnvironment, true), {KEEP_SETTING: 'yes'});
});
