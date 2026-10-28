const assert = require('node:assert/strict');
const { test } = require('node:test');
const { assessArtifactResponses } = require('./artifact_response_policy.cjs');

const ORIGIN = 'https://teagramd.test';
const PRIVATE_CSP = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; frame-src 'self' https://challenges.cloudflare.com; object-src 'none'; script-src 'self' 'wasm-unsafe-eval' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; connect-src 'self' wss://teagramd.test/apiws;";

function validArtifactResponses() {
  return [
    { context: 'page', url: `${ORIGIN}/`, status: 200, content_security_policy: PRIVATE_CSP },
    { context: 'page', url: `${ORIGIN}/assets/app.js`, status: 200, content_security_policy: PRIVATE_CSP },
    { context: 'page', url: `${ORIGIN}/assets/app.css`, status: 200, content_security_policy: PRIVATE_CSP },
    { context: 'shared_worker', url: `${ORIGIN}/assets/worker.js`, status: 200, content_security_policy: PRIVATE_CSP },
  ];
}

test('accepts a missing optional runtime config without requiring CSP on its 404', () => {
  const responses = validArtifactResponses();
  responses.push({
    context: 'page',
    url: `${ORIGIN}/teagram-config.json`,
    status: 404,
    content_security_policy: '',
  });

  const result = assessArtifactResponses(responses, ORIGIN, PRIVATE_CSP);

  assert.equal(result.passed, true);
  assert.equal(result.artifactResponses.length, 4);
  assert.equal(result.artifactResponsesWithPrivateCSP, 4);
  assert.deepEqual(result.failedArtifactResponses, []);
  assert.deepEqual(result.missingPrivateCSPResponses, []);
});

test('fails a non-404 response for the optional runtime config', () => {
  const responses = validArtifactResponses();
  responses.push({
    context: 'page',
    url: `${ORIGIN}/teagram-config.json`,
    status: 500,
    content_security_policy: PRIVATE_CSP,
  });

  const result = assessArtifactResponses(responses, ORIGIN, PRIVATE_CSP);

  assert.equal(result.passed, false);
  assert.deepEqual(
    result.failedArtifactResponses.map(({ url, status }) => ({ path: new URL(url).pathname, status })),
    [{ path: '/teagram-config.json', status: 500 }],
  );
});

test('fails an artifact response without the private CSP', () => {
  const responses = validArtifactResponses();
  responses.push({
    context: 'page',
    url: `${ORIGIN}/assets/extra.js`,
    status: 200,
    content_security_policy: '',
  });

  const result = assessArtifactResponses(responses, ORIGIN, PRIVATE_CSP);

  assert.equal(result.passed, false);
  assert.deepEqual(
    result.missingPrivateCSPResponses.map(({ url }) => new URL(url).pathname),
    ['/assets/extra.js'],
  );
});

test('fails a 404 for any other artifact path', () => {
  const responses = validArtifactResponses();
  responses.push({
    context: 'page',
    url: `${ORIGIN}/assets/missing.js`,
    status: 404,
    content_security_policy: PRIVATE_CSP,
  });

  const result = assessArtifactResponses(responses, ORIGIN, PRIVATE_CSP);

  assert.equal(result.passed, false);
  assert.deepEqual(
    result.failedArtifactResponses.map(({ url, status }) => ({ path: new URL(url).pathname, status })),
    [{ path: '/assets/missing.js', status: 404 }],
  );
});
