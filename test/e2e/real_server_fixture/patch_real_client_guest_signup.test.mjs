import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {test} from 'node:test';

import {patchRealClientGuestSignup} from './patch_real_client_guest_signup.mjs';

test('the pinned guest browser runner exercises the signup QA bypass token', (t) => {
  const sourceRoot = process.env.REAL_CLIENT_GUEST_WEB_ROOT || process.env.REAL_CLIENT_WEB_ROOT;
  assert.ok(sourceRoot, 'REAL_CLIENT_GUEST_WEB_ROOT or REAL_CLIENT_WEB_ROOT must name the pinned guest checkout');
  const webRoot = mkdtempSync(join(tmpdir(), 'real-client-guest-signup-test-'));
  t.after(() => rmSync(webRoot, {recursive: true, force: true}));
  const scripts = join(webRoot, 'scripts/real-client');
  mkdirSync(scripts, {recursive: true});
  for(const name of ['run.mjs', 'browser.cjs']) {
    copyFileSync(join(sourceRoot, 'scripts/real-client', name), join(scripts, name));
  }

  patchRealClientGuestSignup(webRoot);
  const run = readFileSync(join(scripts, 'run.mjs'), 'utf8');
  const browser = readFileSync(join(scripts, 'browser.cjs'), 'utf8');
  execFileSync(process.execPath, ['--check', join(scripts, 'run.mjs')]);
  execFileSync(process.execPath, ['--check', join(scripts, 'browser.cjs')]);
  assert.match(run, /guestSignupQaBypassToken = process\.env\.REAL_CLIENT_GUEST_SIGNUP_QA_BYPASS_TOKEN/);
  assert.match(run, /guestSignupQaBypassToken \? \[\] : \['signup'\]/);
  assert.match(browser, /localStorage\.setItem\('__teagram_turnstile_qa_bypass_token', qaBypassToken\)/);
  assert.match(browser, /failStage\('guest_qa_bypass_storage'\)/);
  assert.match(browser, /failStage\('guest_qa_bypass_send_code'\)/);
  assert.match(browser, /guestSignupQaBypassToken \? \[\] : \['signup'\]/);
  assert.match(browser, /qaBypassUsed: Boolean\(runtime\.guestSignupQaBypassToken\)/);
  assert.throws(() => patchRealClientGuestSignup(webRoot), /already patched/);
});

test('CI keeps the gate-off Turnstile challenge flow beside the QA bypass flow', () => {
  const workflow = readFileSync(join(process.cwd(), '.github/workflows/real-client-browser-flow.yml'), 'utf8');
  assert.match(workflow, /REAL_CLIENT_GUEST_WEB_REVISION: [0-9a-f]{40}/);
  assert.match(workflow, /path: guest-web-source/);
  const guestStepNames = [
    'Run fresh-browser guest signup with the sign-in gate enabled',
    'Run gate-off fresh-browser guest signup with the real challenge',
    'Run gate-off fresh-browser guest signup with the QA bypass'
  ];
  for(const name of guestStepNames) {
    const step = workflow.match(new RegExp(`- name: ${name}([\\s\\S]*?)(?=\\n {6}- name: |$)`));
    assert.ok(step, `${name} must remain in CI`);
    assert.match(step[1], /REAL_CLIENT_WEB_REVISION: \$\{\{ env\.REAL_CLIENT_GUEST_WEB_REVISION \}\}/);
    assert.match(step[1], /REAL_CLIENT_WEB_ROOT: \$\{\{ env\.REAL_CLIENT_GUEST_WEB_ROOT \}\}/);
  }
  const challengeStep = workflow.match(/- name: Run gate-off fresh-browser guest signup with the real challenge([\s\S]*?)(?=\n {6}- name: |$)/);
  assert.ok(challengeStep, 'the gate-off real-challenge browser scenario must remain in CI');
  assert.match(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_ONLY: '1'/);
  assert.match(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_GATE_OFF: '1'/);
  assert.doesNotMatch(challengeStep[1], /REAL_CLIENT_GUEST_SIGNUP_QA_BYPASS/);
});
