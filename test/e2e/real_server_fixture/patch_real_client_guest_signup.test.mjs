import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {copyFileSync, mkdirSync, mkdtempSync, readFileSync, rmSync} from 'node:fs';
import {join} from 'node:path';
import {tmpdir} from 'node:os';
import {test} from 'node:test';

import {patchRealClientGuestSignup} from './patch_real_client_guest_signup.mjs';

test('the pinned guest browser runner exercises the signup QA bypass token', (t) => {
  const sourceRoot = process.env.REAL_CLIENT_WEB_ROOT;
  assert.ok(sourceRoot, 'REAL_CLIENT_WEB_ROOT must name the pinned companion checkout');
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
