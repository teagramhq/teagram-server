'use strict';

// Classify only the exact same-origin MTProto bundle from this worker's timeline.
function classifyWorkerSourceFetchFromResourceTiming(entries) {
  if(!Array.isArray(entries)) return 'not_requested';

  let failedStatus;
  for(const entry of entries) {
    if(!entry || typeof entry.name !== 'string') continue;
    let resourceURL;
    try {
      resourceURL = new URL(entry.name);
    } catch {
      continue;
    }
    if(resourceURL.origin !== 'https://teagramd.test' ||
        !/^\/index\.worker-[A-Za-z0-9_-]{8}\.js$/.test(resourceURL.pathname)) continue;

    const status = entry.responseStatus;
    if(status === 0) {
      failedStatus ||= 'failed';
      continue;
    }
    if(!Number.isInteger(status) || status < 100 || status > 599) continue;
    if(status < 400) return 'ok';
    failedStatus ||= status < 500 ? 'http_4xx' : 'http_5xx';
  }
  return failedStatus || 'not_requested';
}

module.exports = {classifyWorkerSourceFetchFromResourceTiming};
