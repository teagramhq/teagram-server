const assert = require('node:assert/strict');
const { test } = require('node:test');
let classifyWorkerSourceFetchFromResourceTiming;
try {
  ({ classifyWorkerSourceFetchFromResourceTiming } = require('./worker_source_observation.cjs'));
} catch(error) {
  if(error.code !== 'MODULE_NOT_FOUND') throw error;
}

function classify(entries) {
  assert.equal(typeof classifyWorkerSourceFetchFromResourceTiming, 'function');
  return classifyWorkerSourceFetchFromResourceTiming(entries);
}

const ORIGIN = 'https://teagramd.test';

test('recovers a successful worker source fetch from worker resource timing', () => {
  const result = classify([
    { name: `${ORIGIN}/index.worker-Abc123_-.js?cache=1`, responseStatus: 200 },
  ]);

  assert.equal(result, 'ok');
});

test('does not infer a source fetch when the worker did not request the pinned chunk', () => {
  assert.equal(classify([]), 'not_requested');
  assert.equal(classify([
    { name: `${ORIGIN}/other.worker-Abc123_-.js`, responseStatus: 200 },
    { name: 'https://cdn.example.test/index.worker-Abc123_-.js', responseStatus: 200 },
  ]), 'not_requested');
});

test('does not treat a resource without status as a successful fetch', () => {
  assert.equal(classify([
    { name: `${ORIGIN}/index.worker-Abc123_-.js` },
  ]), 'not_requested');
});

test('retains an attempted worker fetch with no HTTP response as failed', () => {
  assert.equal(classify([
    { name: `${ORIGIN}/index.worker-Abc123_-.js`, responseStatus: 0 },
  ]), 'failed');
});

test('retains HTTP failure classes for a matching worker source', () => {
  assert.equal(classify([
    { name: `${ORIGIN}/index.worker-Abc123_-.js`, responseStatus: 404 },
  ]), 'http_4xx');
  assert.equal(classify([
    { name: `${ORIGIN}/index.worker-Abc123_-.js`, responseStatus: 503 },
  ]), 'http_5xx');
});
