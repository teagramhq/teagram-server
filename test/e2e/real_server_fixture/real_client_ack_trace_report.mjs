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
