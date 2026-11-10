const MAX_BROWSER_REPORT_LINE_LENGTH = 2 * 1024 * 1024;

export function parseRealClientBrowserReport(line) {
  if(typeof line !== 'string' || line.length > MAX_BROWSER_REPORT_LINE_LENGTH) return undefined;
  let report;
  try {
    report = JSON.parse(line);
  } catch {
    return undefined;
  }
  if(!report || typeof report !== 'object' || Array.isArray(report) ||
      !['passed', 'failed'].includes(report.status)) return undefined;
  return report;
}

export function isMessageCountFailure(report) {
  return report?.status === 'failed' && typeof report.stage === 'string' &&
    /^[a-z0-9]+(?:_[a-z0-9]+)*_message_count$/.test(report.stage);
}

export function createMessageAckTraceEvidence(report) {
  const traceReport = report?.messageSendAcknowledgementTraces;
  if(!traceReport || !Array.isArray(traceReport.alice) || !Array.isArray(traceReport.bob)) return undefined;

  return {
    ...(isMessageCountFailure(report) ? {failedCheck: report.stage} : {}),
    traces: {alice: traceReport.alice, bob: traceReport.bob},
    ...(report.messageSendAcknowledgementTraceDiagnostics
      ? {diagnostics: report.messageSendAcknowledgementTraceDiagnostics}
      : {}),
    ...(report.messageSendAcknowledgementTraceCaptureErrors
      ? {captureErrors: report.messageSendAcknowledgementTraceCaptureErrors}
      : {})
  };
}

export function createRealClientBrowserReportCollector() {
  let pendingLine = '';
  let droppingOversizedLine = false;
  let report;

  const consumeLine = (line) => {
    const candidate = parseRealClientBrowserReport(line.endsWith('\r') ? line.slice(0, -1) : line);
    if(candidate) report = candidate;
  };

  return {
    write(chunk) {
      if(typeof chunk !== 'string') throw new TypeError('browser runner stdout chunks must be strings');
      let offset = 0;
      while(offset < chunk.length) {
        const newline = chunk.indexOf('\n', offset);
        const end = newline < 0 ? chunk.length : newline;
        const segment = chunk.slice(offset, end);

        if(droppingOversizedLine) {
          if(newline >= 0) droppingOversizedLine = false;
        } else {
          const line = pendingLine + segment;
          pendingLine = '';
          if(line.length > MAX_BROWSER_REPORT_LINE_LENGTH) {
            droppingOversizedLine = newline < 0;
          } else if(newline < 0) {
            pendingLine = line;
          } else {
            consumeLine(line);
          }
        }

        if(newline < 0) break;
        offset = newline + 1;
      }
    },
    finish() {
      if(!droppingOversizedLine && pendingLine) consumeLine(pendingLine);
      pendingLine = '';
      return report;
    }
  };
}

export function collectMessageAckTraceEvidence({guestSignupOnly, runnerError, browserReport}) {
  const evidence = createMessageAckTraceEvidence(browserReport);
  if(guestSignupOnly || runnerError) return evidence;
  if(browserReport?.status !== 'passed' || !evidence || evidence.traces.alice.length === 0) {
    throw new Error('successful browser flow omitted Alice acknowledgement trace records');
  }
  return evidence;
}

export function configureAckTraceBuildEnvironment(environment, guestSignupOnly) {
  const configuredEnvironment = {...environment};
  delete configuredEnvironment.VITE_REAL_CLIENT_ACK_TRACE;
  if(!guestSignupOnly) configuredEnvironment.VITE_REAL_CLIENT_ACK_TRACE = '1';
  return configuredEnvironment;
}
