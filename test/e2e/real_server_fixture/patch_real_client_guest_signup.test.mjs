import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {pathToFileURL} from 'node:url';
import {test} from 'node:test';

import {patchRealClientGuestSignup} from './patch_real_client_guest_signup.mjs';

test('the pinned guest browser runner exercises the signup QA bypass token', async(t) => {
  const sourceRoot = process.env.REAL_CLIENT_GUEST_WEB_ROOT;
  assert.ok(sourceRoot, 'REAL_CLIENT_GUEST_WEB_ROOT must name the pinned guest checkout');
  const webRoot = mkdtempSync(join(tmpdir(), 'real-client-guest-signup-test-'));
  t.after(() => rmSync(webRoot, {recursive: true, force: true}));
  const scripts = join(webRoot, 'scripts/real-client');
  mkdirSync(scripts, {recursive: true});
  for(const name of ['run.mjs', 'browser.cjs', 'observation.cjs', 'contract.mjs']) {
    copyFileSync(join(sourceRoot, 'scripts/real-client', name), join(scripts, name));
  }

  patchRealClientGuestSignup(webRoot);
  const run = readFileSync(join(scripts, 'run.mjs'), 'utf8');
  const browser = readFileSync(join(scripts, 'browser.cjs'), 'utf8');
  const observation = readFileSync(join(scripts, 'observation.cjs'), 'utf8');
  const contract = readFileSync(join(scripts, 'contract.mjs'), 'utf8');
  execFileSync(process.execPath, ['--check', join(scripts, 'run.mjs')]);
  execFileSync(process.execPath, ['--check', join(scripts, 'browser.cjs')]);
  assert.match(run, /guestSignupQaBypassToken = process\.env\.REAL_CLIENT_GUEST_SIGNUP_QA_BYPASS_TOKEN/);
  assert.match(run, /guestSignupTailnet = process\.env\.REAL_CLIENT_GUEST_SIGNUP_TAILNET === '1'/);
  assert.match(run, /guestSignupQaBypassToken \? \[\] : \['signup'\]/);
  assert.match(browser, /localStorage\.setItem\('__teagram_turnstile_qa_bypass_token', qaBypassToken\)/);
  assert.match(browser, /failStage\('guest_qa_bypass_storage'\)/);
  assert.match(browser, /failStage\('guest_qa_bypass_send_code'\)/);
  assert.match(browser, /guestSignupQaBypassToken \? \[\] : \['signup'\]/);
  assert.match(browser, /qaBypassUsed: Boolean\(runtime\.guestSignupQaBypassToken\)/);
  assert.match(browser, /guestSignupTailnet \? 'https:\/\/teagram-dev\.tailaa4918\.ts\.net\/' : PRIVATE_CSP_ORIGIN/);
  assert.match(browser, /guest_tailnet_page_hostname/);
  assert.match(browser, /tailnetPage: Boolean\(runtime\.guestSignupTailnet\)/);
  assert.match(browser, /MAP teagram-dev\.tailaa4918\.ts\.net \$\{runtime\.frontIp\}/);
  assert.match(run, /REAL_CLIENT_GUEST_SIGNUP_TAILNET=1/);
  assert.match(observation, /process\.env\.REAL_CLIENT_GUEST_SIGNUP_TAILNET === '1'/);
  assert.match(contract, /serverReady\.endpoint !== PRIVATE_CSP_ORIGIN/);
  assert.match(contract, /serverReady\.wssEndpoint !== PRIVATE_CSP_WSS/);
  const previousTailnet = process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET;
  process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET = '1';
  try {
    const {parseFixtureReadiness} = await import(`${pathToFileURL(join(scripts, 'contract.mjs')).href}?tailnet-test`);
    const readiness = {
      event: 'server-ready',
      status: 'ready',
      runId: 'a'.repeat(32),
      harnessRevision: 'b'.repeat(40),
      serverRevision: 'c'.repeat(40),
      webRevision: 'd'.repeat(40),
      endpoint: 'https://teagram-dev.tailaa4918.ts.net',
      wssEndpoint: 'wss://teagram-dev.tailaa4918.ts.net/apiws',
      mode: 'private',
      fingerprint: 'e'.repeat(16),
      publicKeySHA256: 'f'.repeat(64)
    };
    assert.throws(() => parseFixtureReadiness(`${JSON.stringify(readiness)}\n`, {
      runId: readiness.runId,
      harnessRevision: readiness.harnessRevision,
      serverRevision: readiness.serverRevision,
      webRevision: readiness.webRevision
    }, {requireArtifact: false, guestSignupOnly: true}), /server-ready security evidence is incomplete/);
  } finally {
    if(previousTailnet === undefined) delete process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET;
    else process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET = previousTailnet;
  }
  assert.throws(() => patchRealClientGuestSignup(webRoot), /already patched/);
});

test('CI keeps the gate-off Turnstile challenge flow beside the QA bypass flow', () => {
  const workflow = readFileSync(join(process.cwd(), '.github/workflows/real-client-browser-flow.yml'), 'utf8');
  assert.match(workflow, /REAL_CLIENT_GUEST_WEB_REVISION: [0-9a-f]{40}/);
  assert.match(workflow, /path: guest-web-source/);
  const guestStepNames = [
    'Run fresh-browser tailnet guest signup with the sign-in gate enabled',
    'Run gate-off fresh-browser guest signup with the real challenge',
    'Run gate-off fresh-browser guest signup with the QA bypass'
  ];
  for(const name of guestStepNames) {
    const step = workflow.match(new RegExp(`- name: ${name}([\\s\\S]*?)(?=\\n {6}- name: |$)`));
    assert.ok(step, `${name} must remain in CI`);
    assert.match(step[1], /REAL_CLIENT_WEB_REVISION: \$\{\{ env\.REAL_CLIENT_GUEST_WEB_REVISION \}\}/);
    assert.match(step[1], /REAL_CLIENT_WEB_ROOT: \$\{\{ env\.REAL_CLIENT_GUEST_WEB_ROOT \}\}/);
  }
  const tailnetStep = workflow.match(/- name: Run fresh-browser tailnet guest signup with the sign-in gate enabled([\s\S]*?)(?=\n {6}- name: |$)/);
  assert.ok(tailnetStep, 'the tailnet first-use signup flow must remain in CI');
  assert.match(tailnetStep[1], /REAL_CLIENT_GUEST_SIGNUP_TAILNET: '1'/);
  const challengeStep = workflow.match(/- name: Run gate-off fresh-browser guest signup with the real challenge([\s\S]*?)(?=\n {6}- name: |$)/);
  assert.ok(challengeStep, 'the gate-off real-challenge browser scenario must remain in CI');
  assert.match(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_ONLY: '1'/);
  assert.match(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_GATE_OFF: '1'/);
  assert.doesNotMatch(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_QA_BYPASS/);
});
