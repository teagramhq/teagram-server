'use strict';

const assert = require('node:assert/strict');
const {readFileSync} = require('node:fs');
const {createServer} = require('node:https');
const {createRequire} = require('node:module');
const {join} = require('node:path');

const webRoot = process.env.REAL_CLIENT_WEB_ROOT;
const site = process.env.QA_INVITE_SITE;
const username = process.env.QA_INVITE_USERNAME;
const password = process.env.QA_INVITE_PASSWORD;
const profileRoot = process.env.QA_INVITE_PROFILE_ROOT;
const probeCert = process.env.QA_INVITE_PROBE_CERT;
const probeKey = process.env.QA_INVITE_PROBE_KEY;
const expectedDiscovery = {
  mtproto: {endpoint: 'teagram-demo.incus:443'},
  websocket: {endpoint: 'wss://teagram-demo.incus/apiws'}
};

if(!webRoot || !site || !username || !password || !profileRoot || !probeCert || !probeKey) {
  throw new Error('QA invite browser test inputs are incomplete');
}

const {chromium} = createRequire(join(webRoot, 'package.json'))('@playwright/test');

async function rejectOutsideCertificate() {
  const server = createServer({cert: readFileSync(probeCert), key: readFileSync(probeKey)}, (_request, response) => response.end('unexpected'));
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  let context;
  try {
    context = await chromium.launchPersistentContext(join(profileRoot, 'constraint-probe'), {
      headless: true,
      args: ['--host-resolver-rules=MAP outside-qa.invalid 127.0.0.1', '--no-proxy-server']
    });
    const page = context.pages()[0] || await context.newPage();
    await assert.rejects(page.goto(`https://outside-qa.invalid:${server.address().port}/`, {timeout: 30_000}),
      /ERR_CERT_(?:INVALID|NAME_CONSTRAINT_VIOLATION)/);
  } finally {
    try {
      if(context) await context.close();
    } finally {
      await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
    }
  }
}

async function waitForChats(page, timeout = 45_000) {
  await page.waitForFunction(() => {
    const chats = document.querySelector('#page-chats');
    return chats && getComputedStyle(chats).display !== 'none' && document.querySelector('#new-menu');
  }, null, {timeout});
}

async function signInOnce(attempt) {
  const profile = join(profileRoot, `attempt-${attempt}`);
  const context = await chromium.launchPersistentContext(profile, {
    headless: true,
    args: ['--host-resolver-rules=MAP teagram-demo.incus 127.0.0.1']
  });
  try {
    const page = context.pages()[0] || await context.newPage();
    const response = await page.goto(site, {waitUntil: 'domcontentloaded', timeout: 30_000});
    assert.equal(response?.status(), 200, 'QA Web artifact did not load over trusted TLS');

    const endpoints = await page.evaluate(async() => {
      const [deploymentResponse, discoveryResponse] = await Promise.all([
        fetch('/teagram-config.json', {cache: 'no-store'}),
        fetch('/.well-known/teagramd/client', {cache: 'no-store'})
      ]);
      if(!deploymentResponse.ok || !discoveryResponse.ok) throw new Error('QA config discovery request failed');
      return {
        deployment: await deploymentResponse.json(),
        discovery: await discoveryResponse.json()
      };
    });
    assert.equal(endpoints.deployment.server, 'teagram-demo.incus', 'Web configuration did not select the QA invite host');
    assert.equal(endpoints.deployment.autoGuest, false, 'Web configuration unexpectedly enabled guest accounts');
    assert.equal(endpoints.discovery.mtproto?.endpoint, expectedDiscovery.mtproto.endpoint, 'QA MTProto discovery endpoint changed');
    assert.equal(endpoints.discovery.websocket?.endpoint, expectedDiscovery.websocket.endpoint, 'QA WebSocket discovery endpoint changed');

    const userField = page.getByRole('textbox', {name: 'Username', exact: true});
    await userField.waitFor({state: 'visible', timeout: 30_000});
    await userField.fill(username);
    await userField.press('Enter');

    const passwordField = page.locator('#auth-pages input[name="notsearch_password"]');
    await passwordField.waitFor({state: 'visible', timeout: 30_000});
    await passwordField.fill(password);
    await passwordField.press('Enter');
    await waitForChats(page);

    const authenticated = await page.evaluate(() => {
      const chats = document.querySelector('#page-chats');
      return Boolean(chats && getComputedStyle(chats).display !== 'none' && document.querySelector('#new-menu'));
    });
    assert.equal(authenticated, true, 'Web sign-in did not reach the inbox');
  } finally {
    await context.close();
  }
}

(async() => {
  await rejectOutsideCertificate();
  for(let attempt = 1; attempt <= 3; attempt += 1) {
    await signInOnce(attempt);
  }
  process.stdout.write('Chromium rejected an out-of-scope certificate and three trusted QA Web sign-ins reached the inbox\n');
})().catch((error) => {
  process.stderr.write(`QA invite browser flow failed: ${error?.message || 'unknown browser failure'}\n`);
  process.exitCode = 1;
});
