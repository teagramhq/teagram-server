import {execFileSync} from 'node:child_process';
import {readFileSync, writeFileSync} from 'node:fs';
import {join, resolve} from 'node:path';
import {pathToFileURL} from 'node:url';

function replaceOnce(source, before, after, description) {
  const first = source.indexOf(before);
  if(first < 0 || source.indexOf(before, first + before.length) >= 0) {
    throw new Error(`pinned web runner no longer has one ${description} insertion point`);
  }
  return `${source.slice(0, first)}${after}${source.slice(first + before.length)}`;
}

export function patchRealClientGuestSignup(webRoot) {
  const runPath = join(webRoot, 'scripts/real-client/run.mjs');
  const browserPath = join(webRoot, 'scripts/real-client/browser.cjs');
  const observationPath = join(webRoot, 'scripts/real-client/observation.cjs');
  let run = readFileSync(runPath, 'utf8');
  let browser = readFileSync(browserPath, 'utf8');
  let observation = readFileSync(observationPath, 'utf8');
  if(run.includes('guestSignupQaBypassToken') || browser.includes('guestSignupQaBypassToken') || observation.includes('REAL_CLIENT_GUEST_SIGNUP_TAILNET')) {
    throw new Error('pinned web guest-signup runner is already patched');
  }

  observation = replaceOnce(observation,
    "const PRIVATE_CSP_ORIGIN = 'https://teagramd.test';",
    "const PRIVATE_CSP_ORIGIN = process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET === '1' ? 'https://teagram-dev.tailaa4918.ts.net' : 'https://teagramd.test';",
    'guest-signup tailnet observation origin');

  run = replaceOnce(run,
    '  const guestSignupGateOff = args.guestSignupGateOff === true;',
    '  const guestSignupGateOff = args.guestSignupGateOff === true;\n  const guestSignupTailnet = process.env.REAL_CLIENT_GUEST_SIGNUP_TAILNET === \'1\';\n  const guestSignupQaBypassToken = process.env.REAL_CLIENT_GUEST_SIGNUP_QA_BYPASS_TOKEN;',
    'guest-signup QA token input');
  run = replaceOnce(run,
    '      ...(guestSignupOnly ? {guestSignupOnly: true} : {}),',
    '      ...(guestSignupOnly ? {guestSignupOnly: true} : {}),\n      ...(guestSignupOnly && guestSignupTailnet ? {guestSignupTailnet: true} : {}),\n      ...(guestSignupOnly && guestSignupQaBypassToken ? {guestSignupQaBypassToken} : {}),',
    'guest-signup QA token forwarding');
  run = replaceOnce(run,
    "['exec', '-i', browserName, 'node', remoteScript],",
    "['exec', '-i', ...(guestSignupTailnet ? ['--env', 'REAL_CLIENT_GUEST_SIGNUP_TAILNET=1'] : []), browserName, 'node', remoteScript],",
    'guest-signup tailnet observer environment');
  run = replaceOnce(run,
    "JSON.stringify(browserResult.guestSignup?.turnstileActions) !== JSON.stringify(['signup']) ||",
    "JSON.stringify(browserResult.guestSignup?.turnstileActions) !== JSON.stringify(guestSignupQaBypassToken ? [] : ['signup']) ||",
    'guest-signup QA Turnstile action expectation');
  run = replaceOnce(run,
    'browserResult.guestSignup?.authorized !== true || browserResult.screenshotsCaptured !== 2 ||',
    'browserResult.guestSignup?.authorized !== true ||\n          browserResult.guestSignup?.qaBypassUsed !== Boolean(guestSignupQaBypassToken) || browserResult.guestSignup?.tailnetPage !== guestSignupTailnet || browserResult.screenshotsCaptured !== 2 ||',
    'guest-signup QA bypass evidence');

  browser = replaceOnce(browser,
    "const PRIVATE_CSP_WSS = 'wss://teagramd.test/apiws';",
    "const PRIVATE_CSP_WSS = `${PRIVATE_CSP_ORIGIN.replace('https:', 'wss:')}/apiws`;",
    'guest-signup tailnet websocket endpoint');
  browser = replaceOnce(browser,
    '        `--host-resolver-rules=MAP teagramd.test ${runtime.frontIp},MAP * ~NOTFOUND`,',
    '        `--host-resolver-rules=MAP teagramd.test ${runtime.frontIp}${runtime.guestSignupTailnet ? `,MAP teagram-dev.tailaa4918.ts.net ${runtime.frontIp}` : \'\'},MAP * ~NOTFOUND`,',
    'guest-signup tailnet hostname routing');
  browser = replaceOnce(browser,
    '      (config.guestSignupOnly !== undefined && !guestSignupOnly) ||',
    '      (config.guestSignupOnly !== undefined && !guestSignupOnly) ||\n      (config.guestSignupTailnet !== undefined && (!guestSignupOnly || config.guestSignupTailnet !== true)) ||',
    'guest-signup tailnet input validation');
  browser = replaceOnce(browser,
    `      await guestPage.addInitScript(() => {
        window.__fixtureTurnstileActions = [];
        window.turnstile = {`,
    `      await guestPage.addInitScript(({qaBypassToken}) => {
        window.__fixtureTurnstileActions = [];
        if(qaBypassToken) localStorage.setItem('__teagram_turnstile_qa_bypass_token', qaBypassToken);
        window.turnstile = {`,
    'guest-signup QA bypass storage');
  browser = replaceOnce(browser,
    `          remove() {}
        };
      });

      const response = await guestPage.goto(PRIVATE_CSP_ORIGIN, {waitUntil: 'domcontentloaded', timeout: 30_000});`,
    `          remove() {}
        };
      }, {qaBypassToken: runtime.guestSignupQaBypassToken});

      const response = await guestPage.goto(PRIVATE_CSP_ORIGIN, {waitUntil: 'domcontentloaded', timeout: 30_000});`,
    'guest-signup QA bypass init argument');
  browser = replaceOnce(browser,
    "      const response = await guestPage.goto(PRIVATE_CSP_ORIGIN, {waitUntil: 'domcontentloaded', timeout: 30_000});",
    "      const response = await guestPage.goto(runtime.guestSignupTailnet ? 'https://teagram-dev.tailaa4918.ts.net/' : PRIVATE_CSP_ORIGIN, {waitUntil: 'domcontentloaded', timeout: 30_000});\n      if(runtime.guestSignupTailnet && new URL(guestPage.url()).hostname !== 'teagram-dev.tailaa4918.ts.net') failStage('guest_tailnet_page_hostname');",
    'guest-signup tailnet page');
  browser = replaceOnce(browser,
    `      const discovery = await guestPage.evaluate(() => ({
        guestSignUpEnabled: window.__TEAGRAM_ACTIVE_GUEST_SIGN_UP__,
        turnstileSiteKey: window.__TEAGRAM_ACTIVE_TURNSTILE_SITE_KEY__
      }));
      if(discovery.guestSignUpEnabled !== true || discovery.turnstileSiteKey !== 'fixture-turnstile-site-key') {
        failStage('guest_discovery_capabilities');
      }`,
    `      const discovery = await guestPage.evaluate((qaBypassToken) => ({
        guestSignUpEnabled: window.__TEAGRAM_ACTIVE_GUEST_SIGN_UP__,
        turnstileSiteKey: window.__TEAGRAM_ACTIVE_TURNSTILE_SITE_KEY__,
        qaBypassStored: !qaBypassToken || localStorage.getItem('__teagram_turnstile_qa_bypass_token') === qaBypassToken
      }), runtime.guestSignupQaBypassToken);
      if(discovery.guestSignUpEnabled !== true || discovery.turnstileSiteKey !== 'fixture-turnstile-site-key') {
        failStage('guest_discovery_capabilities');
      }
      if(!discovery.qaBypassStored) failStage('guest_qa_bypass_storage');`,
    'guest-signup QA bypass storage assertion');
  browser = replaceOnce(browser,
    `      const challenge = guestPage.getByTestId('turnstile-challenge');
      await challenge.waitFor({state: 'visible', timeout: 30_000}).catch(() => failStage('guest_turnstile_challenge'));
      if(await challenge.getAttribute('data-sitekey') !== 'fixture-turnstile-site-key') failStage('guest_turnstile_site_key');
      await guestPage.getByTestId('fixture-turnstile-complete').click().catch(() => failStage('guest_turnstile_complete'));`,
    `      const challenge = guestPage.getByTestId('turnstile-challenge');
      if(runtime.guestSignupQaBypassToken) {
        if(await challenge.count() !== 0) failStage('guest_unexpected_turnstile_challenge');
      } else {
        await challenge.waitFor({state: 'visible', timeout: 30_000}).catch(() => failStage('guest_turnstile_challenge'));
        if(await challenge.getAttribute('data-sitekey') !== 'fixture-turnstile-site-key') failStage('guest_turnstile_site_key');
        await guestPage.getByTestId('fixture-turnstile-complete').click().catch(() => failStage('guest_turnstile_complete'));
      }`,
    'guest-signup QA bypass challenge behavior');
  browser = replaceOnce(browser,
    `      if(JSON.stringify(turnstileActions) !== JSON.stringify(['signup'])) failStage('guest_turnstile_action');`,
    `      const expectedTurnstileActions = runtime.guestSignupQaBypassToken ? [] : ['signup'];
      if(JSON.stringify(turnstileActions) !== JSON.stringify(expectedTurnstileActions)) failStage('guest_turnstile_action');`,
    'guest-signup QA bypass action assertion');
  browser = replaceOnce(browser,
    `      const termsDialog = guestPage.getByRole('dialog');
      await termsDialog.waitFor({state: 'visible', timeout: 30_000}).catch(() => failStage('guest_terms_dialog'));`,
    `      const termsDialog = guestPage.getByRole('dialog');
      if(runtime.guestSignupQaBypassToken) {
        await guestPage.waitForFunction(() => {
          const dialog = document.querySelector('[role="dialog"]');
          const rejected = [...document.querySelectorAll('[role="alert"]')]
          .some((alert) => alert.textContent?.includes('The security check failed.'));
          return Boolean(dialog) || rejected;
        }, null, {timeout: 30_000}).catch(() => failStage('guest_qa_bypass_send_code'));
        if(await guestPage.getByText('The security check failed. Try again.', {exact: true}).count() > 0) {
          failStage('guest_qa_bypass_send_code');
        }
      } else {
        await termsDialog.waitFor({state: 'visible', timeout: 30_000}).catch(() => failStage('guest_terms_dialog'));
      }`,
    'guest-signup QA bypass rejection stage');
  browser = replaceOnce(browser,
    `guestSignup: {availabilityVisible: true, appConfigLoaded: true, turnstileActions, authorized: true},`,
    `guestSignup: {availabilityVisible: true, appConfigLoaded: true, turnstileActions, authorized: true,
          qaBypassUsed: Boolean(runtime.guestSignupQaBypassToken), tailnetPage: Boolean(runtime.guestSignupTailnet)},`,
    'guest-signup QA bypass result');

  writeFileSync(runPath, run);
  writeFileSync(browserPath, browser);
  writeFileSync(observationPath, observation);
}

function parseArguments(argv) {
  const values = new Map();
  for(let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if(!['--web-root', '--web-revision'].includes(name) || !value || values.has(name)) {
      throw new Error('expected --web-root and --web-revision');
    }
    values.set(name, value);
  }
  const webRoot = values.get('--web-root');
  const webRevision = values.get('--web-revision');
  if(values.size !== 2 || !/^[0-9a-f]{40}$/.test(webRevision || '')) {
    throw new Error('expected --web-root and a full lowercase --web-revision SHA');
  }
  return {webRoot: resolve(webRoot), webRevision};
}

function main() {
  const {webRoot, webRevision} = parseArguments(process.argv.slice(2));
  let actualRevision;
  try {
    actualRevision = execFileSync('git', ['-C', webRoot, 'rev-parse', 'HEAD'], {encoding: 'utf8'}).trim();
  } catch {
    throw new Error('pinned web checkout is unavailable');
  }
  if(actualRevision !== webRevision) throw new Error('pinned web checkout changed before the guest-signup patch');
  patchRealClientGuestSignup(webRoot);
}

if(process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  try {
    main();
  } catch(error) {
    process.stderr.write(`${error?.message || 'guest-signup runner patch failed'}\n`);
    process.exitCode = 1;
  }
}
