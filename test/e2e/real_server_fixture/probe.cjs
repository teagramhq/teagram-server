const fs = require('node:fs');
const net = require('node:net');
const { createHash } = require('node:crypto');
const { chromium } = require('playwright');

const origin = 'https://telegramd.test';
const targets = [
  { class: 'official_domain', url: 'https://telegram.org/' },
  { class: 'short_domain', url: 'https://t.me/' },
  { class: 'legacy_domain', url: 'https://telegram.me/' },
  { class: 'media_domain', url: 'https://telesco.pe/' },
  { class: 'tailnet_name', url: 'https://telegram-server.tailaa4918.ts.net/' },
  { class: 'official_dc_ip', url: 'https://149.154.167.51/' },
  { class: 'official_dc_ipv6', url: 'https://[2001:67c:4e8:f002::a]/' },
  { class: 'cgnat_ip', url: 'https://100.64.0.1/' },
];
const PRIVATE_CSP = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self' 'wasm-unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; media-src 'self' blob:; worker-src 'self' blob:; manifest-src 'self'; connect-src 'self' wss://telegramd.test/apiws;";
const controlledUrls = new Set(targets.map(({ url }) => url));
let failedStage = 'runtime_inputs';

function fail(code, errorClass, stage, details) {
  process.stdout.write(`${JSON.stringify({ status: 'failed', code, ...(errorClass ? { error_class: errorClass } : {}), ...(stage ? { stage } : {}), ...(details ? { details } : {}) })}\n`);
  process.exitCode = 86;
}

function safeErrorClass(error) {
  const known = new Set([
    'AbortError', 'TimeoutError', 'TypeError', 'ECONNREFUSED', 'EHOSTUNREACH',
    'ENETUNREACH', 'ETIMEDOUT', 'EACCES', 'ERR_ADDRESS_UNREACHABLE', 'ERR_CERT_AUTHORITY_INVALID',
    'ERR_CONNECTION_REFUSED', 'ERR_NAME_NOT_RESOLVED', 'ERR_TIMED_OUT',
  ]);
  if (known.has(error?.code)) return error.code;
  if (known.has(error?.name)) return error.name;
  return 'OtherError';
}

function tcpConnectBlocked(host, port, timeoutMs = 1800) {
  return new Promise((resolve) => {
    const socket = net.createConnection({ host, port });
    let settled = false;
    const finish = (blocked, errorClass) => {
      if (settled) return;
      settled = true;
      socket.destroy();
      resolve({ blocked, error_class: errorClass });
    };
    socket.setTimeout(timeoutMs);
    socket.once('connect', () => finish(false, 'connected'));
    socket.once('error', (error) => finish(true, error.code || 'socket_error'));
    socket.once('timeout', () => finish(true, 'timeout'));
  });
}

function chromiumNamespaces() {
  const entries = fs.readdirSync('/proc').filter((entry) => /^\d+$/.test(entry));
  const processes = [];
  for (const pid of entries) {
    try {
      const command = fs.readFileSync(`/proc/${pid}/cmdline`, 'utf8').replaceAll('\0', ' ');
      const executable = fs.readlinkSync(`/proc/${pid}/exe`);
      const executableName = executable.split('/').pop();
      if (['chrome', 'chrome-headless-shell', 'headless_shell', 'chromium'].includes(executableName) &&
          (command.includes('--user-data-dir') || command.includes('--type=renderer'))) {
        processes.push({ pid, command, executableName });
      }
    } catch (error) {
      if (error.code !== 'ENOENT' && error.code !== 'ESRCH') throw error;
    }
  }
  const browser = processes.find(({ command }) => !command.includes('--type='));
  const renderer = processes.find(({ command }) => command.includes('--type=renderer'));
  const noSandboxFlag = processes.some(({ command }) => /--no-sandbox|--disable-setuid-sandbox/.test(command));
  if (!browser || !renderer) {
    return { browser_found: Boolean(browser), renderer_found: Boolean(renderer), no_sandbox_flag: noSandboxFlag };
  }
  const ns = (pid, name) => fs.readlinkSync(`/proc/${pid}/ns/${name}`);
  return {
    browser_found: true,
    renderer_found: true,
    no_sandbox_flag: noSandboxFlag,
    user_namespace_separated: ns(browser.pid, 'user') !== ns(renderer.pid, 'user'),
    pid_namespace_separated: ns(browser.pid, 'pid') !== ns(renderer.pid, 'pid'),
  };
}

async function runPageProbes({ targets, injectUnexpected }) {
  const errorClass = (error) => ['TypeError', 'AbortError', 'TimeoutError'].includes(error?.name) ? error.name : 'UnexpectedError';
  const attempts = await Promise.all(targets.map(async (target) => {
    try {
      await fetch(target.url, {
        method: 'GET', mode: 'no-cors', credentials: 'omit', cache: 'no-store',
        signal: AbortSignal.timeout(1800),
      });
      return { class: target.class, blocked: false };
    } catch (error) {
      const failureClass = errorClass(error);
      return { class: target.class, blocked: failureClass !== 'UnexpectedError', error_class: failureClass };
    }
  }));

  if (injectUnexpected) {
    await issueUnannouncedEgress('page');
  }

  return { attempts };

  async function issueUnannouncedEgress(sourceContext) {
    const fetchAttempt = fetch(`https://unannounced.invalid/${sourceContext}/fetch`, {
      method: 'GET', mode: 'no-cors', credentials: 'omit', cache: 'no-store',
      signal: AbortSignal.timeout(1800),
    }).catch(() => undefined);
    const websocketAttempt = new Promise((resolve) => {
      let settled = false;
      let timer;
      const finish = () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve();
      };
      try {
        const socket = new WebSocket(`wss://unannounced.invalid/${sourceContext}/websocket`);
        timer = setTimeout(finish, 1800);
        socket.addEventListener('open', () => { socket.close(); finish(); }, { once: true });
        socket.addEventListener('error', finish, { once: true });
      } catch {
        finish();
      }
    });
    await Promise.all([fetchAttempt, websocketAttempt]);
  }
}

function createNetworkObserver(browserCdp) {
  const events = [];
  const responses = [];
  const eventKeys = new Set();
  const targets = new Map();
  const discoveredTargets = new Map();
  const pendingTargetWaiters = [];
  const pendingCommands = new Map();
  const setupPromises = new Set();
  const attachingTargets = new Map();
  const preparedSessions = new Set();
  const errors = [];
  let nextCommandId = 0;
  let pageCdp;

  function addTarget(sessionId, targetInfo, parent = browserCdp) {
    if (preparedSessions.has(sessionId)) return;
    preparedSessions.add(sessionId);
    // The page is observed through its own CDP session, and a target reached by two
    // sessions must be observed once: a second observation would record every
    // request twice under different identities. A duplicate session is still
    // released, because a held target stays frozen until it is.
    const observe = targetInfo.type !== 'page' &&
      ![...targets.values()].some((target) => target.targetId === targetInfo.targetId);
    if (observe) targets.set(sessionId, targetInfo);
    const setup = setupTarget(sessionId, targetInfo, parent, observe)
      .catch((error) => errors.push(`target_setup:${targetInfo.type}:${error.message}`))
      .finally(() => setupPromises.delete(setup));
    setupPromises.add(setup);
  }

  function attachTarget(targetInfo) {
    const existing = [...targets.values()].some((target) => target.targetId === targetInfo.targetId);
    if (existing) return Promise.resolve();
    if (attachingTargets.has(targetInfo.targetId)) return attachingTargets.get(targetInfo.targetId);
    const attach = browserCdp.send('Target.attachToTarget', { targetId: targetInfo.targetId, flatten: false })
      .then(({ sessionId }) => addTarget(sessionId, targetInfo))
      .catch((error) => errors.push(`target_attach:${targetInfo.type}:${error.message}`))
      .finally(() => attachingTargets.delete(targetInfo.targetId));
    attachingTargets.set(targetInfo.targetId, attach);
    return attach;
  }

  function recordResponse(targetInfo, type, url, response) {
    let parsed;
    try { parsed = new URL(url); } catch { parsed = null; }
    if (type === 'WebSocket' || parsed?.origin !== origin) return;
    const headers = response.headers || {};
    const cspEntry = Object.entries(headers).find(([name]) => name.toLowerCase() === 'content-security-policy');
    responses.push({
      context: targetInfo.type,
      url,
      status: response.status,
      content_security_policy: cspEntry?.[1] || '',
    });
  }

  async function setupTarget(sessionId, targetInfo, parent, observe) {
    if (observe) {
      await sendTargetCommand(parent, sessionId, 'Network.enable');
      await sendTargetCommand(parent, sessionId, 'Audits.enable');
      // Log replays the entries it recorded before the domain was enabled. That is
      // what closes the shared-worker window: browser-level auto-attach is refused
      // without the flatten protocol, so a shared worker cannot be held at startup,
      // and its first top-level violation would otherwise fall in the gap.
      await sendTargetCommand(parent, sessionId, 'Log.enable');
    }
    // Releasing the target is unconditional: page-level auto-attach holds it at
    // startup, and a manual attach that races that event must not leave it frozen.
    await sendTargetCommand(parent, sessionId, 'Runtime.runIfWaitingForDebugger');
  }

  function sendTargetCommand(parent, sessionId, method, params = {}) {
    const id = ++nextCommandId;
    const key = `${sessionId}:${id}`;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        pendingCommands.delete(key);
        reject(new Error(`cdp_timeout:${method}`));
      }, 5000);
      pendingCommands.set(key, {
        resolve: (result) => { clearTimeout(timer); resolve(result); },
        reject: (error) => { clearTimeout(timer); reject(error); },
      });
      parent.send('Target.sendMessageToTarget', {
        sessionId,
        message: JSON.stringify({ id, method, params }),
      }).catch((error) => {
        const pending = pendingCommands.get(key);
        if (!pending) return;
        pendingCommands.delete(key);
        pending.reject(error);
      });
    });
  }

  function record(targetInfo, kind, url, eventName, policyViolation) {
    let parsed;
    try { parsed = new URL(url); } catch { parsed = null; }
    const controlled = kind === 'fetch' && controlledUrls.has(url);
    const allowedTestOrigin = kind === 'fetch' && parsed?.origin === origin;
    const allowedTestWebsocket = kind === 'websocket' && url === 'wss://telegramd.test/apiws';
    const classification = controlled ? 'controlled_probe' :
      (allowedTestOrigin ? 'test_origin' : (allowedTestWebsocket ? 'allowed_test_websocket' : 'unexpected'));
    const key = `${targetInfo.targetId}:${kind}:${url}`;
    if (eventKeys.has(key)) return;
    eventKeys.add(key);
    events.push({
      context: targetInfo.type,
      kind,
      url,
      classification,
      event: eventName,
      ...(policyViolation ? { policy_violation: policyViolation } : {}),
    });
  }

  function recordCspIssue(targetInfo, issue) {
    const details = issue?.details?.contentSecurityPolicyIssueDetails;
    if (!details || details.isReportOnly) return;
    if (typeof details.blockedURL !== 'string' || details.blockedURL.length === 0) {
      errors.push(`csp_issue_missing_blocked_url:${targetInfo.type}`);
      return;
    }
    const protocol = (() => {
      try { return new URL(details.blockedURL).protocol; } catch { return ''; }
    })();
    if (!['http:', 'https:', 'ws:', 'wss:'].includes(protocol)) return;
    const kind = ['ws:', 'wss:'].includes(protocol) ? 'websocket' : 'fetch';
    record(targetInfo, kind, details.blockedURL, 'Audits.issueAdded', {
      violated_directive: details.violatedDirective,
      report_only: details.isReportOnly,
    });
  }

  const LOG_BLOCKED_URL = /'((?:https?|wss?):\/\/[^']+)'/;

  function recordLogViolation(targetInfo, entry) {
    // A worker reached only after its violation fired still reports it: the Log
    // domain replays entries recorded before enable, so the attempt is not
    // lost just because Network was not live yet.
    if (!['violation', 'security'].includes(entry?.source)) return;
    const match = LOG_BLOCKED_URL.exec(entry.text || '');
    if (!match) return;
    const url = match[1];
    let protocol = '';
    try { protocol = new URL(url).protocol; } catch { return; }
    if (!['http:', 'https:', 'ws:', 'wss:'].includes(protocol)) return;
    const kind = ['ws:', 'wss:'].includes(protocol) ? 'websocket' : 'fetch';
    record(targetInfo, kind, url, 'Log.entryAdded', { log_source: entry.source });
  }

  function dispatchTargetMessage(parent, sessionId, message) {
    let payload;
    try { payload = JSON.parse(message); } catch (error) {
      errors.push(`invalid_cdp_message:${error.message}`);
      return;
    }
    if (payload.id !== undefined) {
      const key = `${sessionId}:${payload.id}`;
      const pending = pendingCommands.get(key);
      if (pending) {
        pendingCommands.delete(key);
        if (payload.error) pending.reject(new Error(payload.error.message || 'cdp_command_failed'));
        else pending.resolve(payload.result || {});
      }
      return;
    }
    if (payload.method === 'Target.attachedToTarget') {
      addTarget(payload.params.sessionId, payload.params.targetInfo, parent);
      return;
    }
    const targetInfo = targets.get(sessionId);
    if (!targetInfo) return;
    if (payload.method === 'Network.requestWillBeSent') {
      const kind = payload.params.type === 'WebSocket' ? 'websocket' : 'fetch';
      record(targetInfo, kind, payload.params.request.url, payload.method);
    } else if (payload.method === 'Network.webSocketCreated') {
      record(targetInfo, 'websocket', payload.params.url, payload.method);
    } else if (payload.method === 'Network.responseReceived') {
      recordResponse(targetInfo, payload.params.type, payload.params.response.url, payload.params.response);
    } else if (payload.method === 'Audits.issueAdded') {
      recordCspIssue(targetInfo, payload.params.issue);
    } else if (payload.method === 'Log.entryAdded') {
      recordLogViolation(targetInfo, payload.params.entry);
    }
  }

  function onPageRequest(event) {
    const kind = event.type === 'WebSocket' ? 'websocket' : 'fetch';
    record({ targetId: 'page-root', type: 'page' }, kind, event.request.url, 'Network.requestWillBeSent');
  }

  function onPageWebSocket(event) {
    record({ targetId: 'page-root', type: 'page' }, 'websocket', event.url, 'Network.webSocketCreated');
  }

  function onPageResponse(event) {
    recordResponse({ targetId: 'page-root', type: 'page' }, event.type, event.response.url, event.response);
  }

  function onTargetCreated(targetInfo) {
    discoveredTargets.set(targetInfo.targetId, targetInfo);
    for (let index = pendingTargetWaiters.length - 1; index >= 0; index--) {
      const waiter = pendingTargetWaiters[index];
      if (waiter.type === targetInfo.type && waiter.url === targetInfo.url) {
        pendingTargetWaiters.splice(index, 1);
        clearTimeout(waiter.timer);
        waiter.resolve(targetInfo);
      }
    }
    if (['shared_worker', 'service_worker'].includes(targetInfo.type)) void attachTarget(targetInfo);
  }

  browserCdp.on('Target.targetCreated', ({ targetInfo }) => onTargetCreated(targetInfo));
  browserCdp.on('Target.targetInfoChanged', ({ targetInfo }) => onTargetCreated(targetInfo));
  browserCdp.on('Target.targetDestroyed', ({ targetId }) => discoveredTargets.delete(targetId));
  browserCdp.on('Target.attachedToTarget', (event) => {
    addTarget(event.sessionId, event.targetInfo, browserCdp);
  });
  browserCdp.on('Target.receivedMessageFromTarget', (event) => {
    dispatchTargetMessage(browserCdp, event.sessionId, event.message);
  });
  browserCdp.on('Target.detachedFromTarget', ({ sessionId }) => {
    targets.delete(sessionId);
    preparedSessions.delete(sessionId);
  });

  return {
    events,
    errors,
    async start() {
      await browserCdp.send('Target.setDiscoverTargets', { discover: true });
    },
    async attachPage(cdpSession) {
      pageCdp = cdpSession;
      pageCdp.on('Network.requestWillBeSent', onPageRequest);
      pageCdp.on('Network.webSocketCreated', onPageWebSocket);
      pageCdp.on('Network.responseReceived', onPageResponse);
      pageCdp.on('Log.entryAdded', ({ entry }) => {
        recordLogViolation({ targetId: 'page-root', type: 'page' }, entry);
      });
      pageCdp.on('Target.attachedToTarget', (event) => {
        addTarget(event.sessionId, event.targetInfo, pageCdp);
      });
      pageCdp.on('Target.receivedMessageFromTarget', (event) => {
        dispatchTargetMessage(pageCdp, event.sessionId, event.message);
      });
      pageCdp.on('Target.detachedFromTarget', ({ sessionId }) => {
        targets.delete(sessionId);
        preparedSessions.delete(sessionId);
      });
      await pageCdp.send('Network.enable');
      pageCdp.on('Audits.issueAdded', ({ issue }) => {
        recordCspIssue({ targetId: 'page-root', type: 'page' }, issue);
      });
      await pageCdp.send('Audits.enable');
      await pageCdp.send('Log.enable');
      // Hold every target this page spawns (its service worker among them) at
      // startup, so Network, Audits and Log are live before the target's own
      // top-level code can reach anything. Browser-level auto-attach is not
      // an option: Chromium refuses it without the flatten protocol, and a raw
      // browser session cannot address the nested sessions it would create.
      await pageCdp.send('Target.setAutoAttach', { autoAttach: true, waitForDebuggerOnStart: true, flatten: false });
    },
    async attachWorker(type, url) {
      let targetInfo = [...discoveredTargets.values()].find((target) => target.type === type && target.url === url);
      if (!targetInfo) {
        targetInfo = await new Promise((resolve, reject) => {
          const waiter = { type, url, resolve, reject };
          waiter.timer = setTimeout(() => {
            const index = pendingTargetWaiters.indexOf(waiter);
            if (index >= 0) pendingTargetWaiters.splice(index, 1);
            reject(new Error(`target_timeout:${type}:${url}`));
          }, 10000);
          pendingTargetWaiters.push(waiter);
        });
      }
      await attachTarget(targetInfo);
      await this.waitForSetup();
    },
    async waitForSetup() {
      while (setupPromises.size > 0 || attachingTargets.size > 0) {
        await Promise.all([...setupPromises, ...attachingTargets.values()]);
      }
    },
    responses,
    attachedTargets() {
      const unique = new Map([...targets.values()].map((target) => [target.targetId, target]));
      return [...unique.values()].map(({ type, url }) => ({ type, url }));
    },
    stop() {
      browserCdp.removeAllListeners();
      pageCdp?.removeAllListeners();
    },
  };
}

async function main() {
  if (process.env.ARTIFACT_PROBE === '1') return artifactMain();
const frontIp = process.env.FRONT_IP;
  const backendIp = process.env.BACKEND_IP;
  const databaseIp = process.env.DATABASE_IP;
  const spki = process.env.TLS_SPKI;
  const expectedEndpoint = process.env.MTPROTO_ENDPOINT;
  const expectedFingerprint = process.env.MTPROTO_FINGERPRINT;
  const expectedPublicKeySHA256 = process.env.MTPROTO_PUBLIC_KEY_SHA256;
  const expectedWssBeforeBrowser = Number.parseInt(process.env.EXPECTED_WSS_BEFORE_BROWSER, 10);
  const injectUnexpected = process.env.INJECT_UNEXPECTED === '1';
  if (!frontIp || !backendIp || !databaseIp || !spki || !expectedEndpoint || !expectedFingerprint ||
      !expectedPublicKeySHA256 || !Number.isSafeInteger(expectedWssBeforeBrowser) || expectedWssBeforeBrowser < 0) {
    return fail('missing_runtime_inputs');
  }

  failedStage = 'chromium_launch';
  const browser = await chromium.launch({
    headless: true,
    chromiumSandbox: true,
    args: [
      `--host-resolver-rules=MAP telegramd.test ${frontIp},MAP * ~NOTFOUND`,
      `--ignore-certificate-errors-spki-list=${spki}`,
      '--disable-background-networking',
      '--disable-component-update',
      '--disable-default-apps',
      '--disable-domain-reliability',
      '--disable-quic',
      '--disable-sync',
      '--dns-prefetch-disable',
    ],
  });

  let observer;
  try {
    failedStage = 'context_setup';
    const browserCdp = await browser.newBrowserCDPSession();
    observer = createNetworkObserver(browserCdp);
    await observer.start();
    const context = await browser.newContext();
    failedStage = 'page_create';
    const page = await context.newPage();
    failedStage = 'observer_setup';
    const pageCdp = await context.newCDPSession(page);
    await observer.attachPage(pageCdp);
    await observer.waitForSetup();
    await page.exposeFunction('__main1324_attach_observer', async (type, path) => {
      await observer.attachWorker(type, `${origin}/${path}`);
    });
    let websocketOpened = false;

    failedStage = 'https_readiness';
    const response = await page.goto(`${origin}/_fixture_probe/`, { waitUntil: 'domcontentloaded', timeout: 10000 });
    if (response?.status() !== 200) return fail('https_readiness_failed');
    failedStage = 'target_manifest';
    const targetManifest = await page.evaluate(async () => {
      const response = await fetch('/_fixture_probe/mtproto-target.json', { cache: 'no-store' });
      if (!response.ok) throw new Error('target-manifest-not-ready');
      return response.json();
    });
    if (targetManifest.endpoint !== expectedEndpoint || targetManifest.fingerprint !== expectedFingerprint ||
        targetManifest.publicKeySHA256 !== expectedPublicKeySHA256) {
      return fail('target_manifest_mismatch');
    }
    failedStage = 'sandbox_proof';
    const namespaces = chromiumNamespaces();
    if (!namespaces.browser_found || !namespaces.renderer_found || namespaces.no_sandbox_flag ||
        !namespaces.user_namespace_separated || !namespaces.pid_namespace_separated) {
      return fail('chromium_sandbox_proof_failed', undefined, undefined, namespaces);
    }
    failedStage = 'page_probes';
    const pageResult = await page.evaluate(runPageProbes, { targets, injectUnexpected });

    failedStage = 'shared_worker';
    const sharedResult = await page.evaluate(async ({ targets, injectUnexpected }) => {
      const worker = new SharedWorker('/_fixture_probe/shared-worker.js');
      return await new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('shared-worker-timeout')), 12000);
        worker.port.onmessage = ({ data }) => { clearTimeout(timer); resolve(data); };
        worker.port.start();
        globalThis.__main1324_attach_observer('shared_worker', '_fixture_probe/shared-worker.js')
          .then(() => worker.port.postMessage({ targets, injectUnexpected }))
          .catch(reject);
      });
    }, { targets, injectUnexpected });

    failedStage = 'service_worker_registration';
    await page.evaluate(async () => {
      await navigator.serviceWorker.register('/_fixture_probe/service-worker.js', { scope: '/_fixture_probe/' });
      await navigator.serviceWorker.ready;
    });
    failedStage = 'service_worker_observer';
    await observer.attachWorker('service_worker', `${origin}/_fixture_probe/service-worker.js`);
    await page.goto(`${origin}/_fixture_probe/`, { waitUntil: 'domcontentloaded', timeout: 10000 });
    failedStage = 'service_worker_probe';
    const serviceResult = await page.evaluate(async ({ targets, injectUnexpected }) => {
      const controller = navigator.serviceWorker.controller;
      if (!controller) throw new Error('service-worker-not-controlling');
      const channel = new MessageChannel();
      return await new Promise((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('service-worker-timeout')), 12000);
        channel.port1.onmessage = ({ data }) => { clearTimeout(timer); resolve(data); };
        controller.postMessage({ type: 'probe', targets, injectUnexpected }, [channel.port2]);
      });
    }, { targets, injectUnexpected });

    failedStage = 'wss_upgrade';
    const websocket = await page.evaluate(() => new Promise((resolve, reject) => {
      const socket = new WebSocket('wss://telegramd.test/apiws');
      const timer = setTimeout(() => { socket.close(); reject(new Error('websocket-timeout')); }, 8000);
      socket.addEventListener('open', () => { clearTimeout(timer); socket.close(); resolve(true); }, { once: true });
      socket.addEventListener('error', () => { clearTimeout(timer); reject(new Error('websocket-error')); }, { once: true });
    }));
    websocketOpened = websocket;
    failedStage = 'front_status';
    await page.waitForTimeout(100);
    const health = await page.evaluate(async () => (await (await fetch('/healthz', { cache: 'no-store' })).json()));

    failedStage = 'direct_tcp_isolation';
    const directTcp = await Promise.all([
      tcpConnectBlocked('149.154.167.51', 443),
      tcpConnectBlocked('2001:67c:4e8:f002::a', 443),
      tcpConnectBlocked('100.64.0.1', 443),
      tcpConnectBlocked(backendIp, 2444),
      tcpConnectBlocked(databaseIp, 5432),
    ]);
    failedStage = 'result_assertions';
    await page.waitForTimeout(250);
    await observer.waitForSetup();
    const probeResults = [pageResult, sharedResult, serviceResult];
    const allControlledBlocked = probeResults.every((result) =>
      result.attempts.length === targets.length && result.attempts.every((attempt) => attempt.blocked));
    const observerControlledUrls = (sourceContext) => new Set(observer.events
      .filter((event) => event.context === sourceContext && event.classification === 'controlled_probe')
      .map((event) => event.url));
    const allControlledObserved = ['page', 'shared_worker', 'service_worker'].every((sourceContext) =>
      observerControlledUrls(sourceContext).size === targets.length &&
      targets.every(({ url }) => observerControlledUrls(sourceContext).has(url)));
    const unexpectedEvents = observer.events.filter((event) => event.classification === 'unexpected');
    // This set requires the negative-control evidence; record() still classifies every non-test URL as unexpected.
    const requiredNegativeControlEvents = new Set([
      'page:fetch:https://unannounced.invalid/page/fetch',
      'page:websocket:wss://unannounced.invalid/page/websocket',
      'shared_worker:fetch:https://unannounced.invalid/shared_worker/fetch',
      'shared_worker:websocket:wss://unannounced.invalid/shared_worker/websocket',
      'service_worker:fetch:https://unannounced.invalid/service_worker/fetch',
      'service_worker:websocket:wss://unannounced.invalid/service_worker/websocket',
    ]);
    const observedInjectedEvents = new Set(unexpectedEvents.map((event) => `${event.context}:${event.kind}:${event.url}`));
    const allInjectedObserved = observedInjectedEvents.size === requiredNegativeControlEvents.size &&
      [...requiredNegativeControlEvents].every((key) => observedInjectedEvents.has(key));
    const allowedWssEvents = observer.events.filter((event) => event.classification === 'allowed_test_websocket');
    const browserWssObserved = health.websocket_101_count === expectedWssBeforeBrowser + 1;
    const passed = !injectUnexpected && allControlledBlocked && directTcp.every((result) => result.blocked) && websocket && websocketOpened &&
      allControlledObserved && allowedWssEvents.length === 1 && browserWssObserved && health.route_errors === 0 &&
      unexpectedEvents.length === 0 && observer.errors.length === 0;
    const injectedFailureDetected = injectUnexpected && allControlledBlocked && directTcp.every((result) => result.blocked) &&
      websocket && websocketOpened && allControlledObserved && allowedWssEvents.length === 1 &&
      browserWssObserved && allInjectedObserved && observer.errors.length === 0;

    const report = {
      status: passed ? 'passed' : (injectedFailureDetected ? 'expected_injected_failure' : 'failed'),
      http_status: response.status(),
      manifest_endpoint: targetManifest.endpoint,
      manifest_fingerprint: targetManifest.fingerprint,
      manifest_public_key_sha256: targetManifest.publicKeySHA256,
      wss_upgrade_status: browserWssObserved ? 101 : 0,
      observed_allowed_wss: allowedWssEvents.length,
      front_route_errors: health.route_errors,
      front_route_error_details: health.route_error_details,
      tls_leaf_spki_pin_only: true,
      worker_probes: Object.fromEntries(probeResults.map((result, index) => [
        ['page', 'shared_worker', 'service_worker'][index],
        { attempted: result.attempts.length, blocked: result.attempts.filter((attempt) => attempt.blocked).length },
      ])),
      observer_controlled_attempts: Object.fromEntries(['page', 'shared_worker', 'service_worker'].map((sourceContext) => [
        sourceContext, observerControlledUrls(sourceContext).size,
      ])),
      observer_targets: observer.attachedTargets(),
      direct_tcp: { attempted: directTcp.length, blocked: directTcp.filter((result) => result.blocked).length },
      observer_unexpected_attempts: unexpectedEvents,
      observer_errors: observer.errors,
      sandbox: namespaces,
    };
    process.stdout.write(`${JSON.stringify(report)}\n`);
    if (!passed) process.exitCode = injectedFailureDetected ? 86 : 1;
  } finally {
    observer?.stop();
    await browser.close();
  }
}

async function artifactMain() {
  const frontIp = process.env.FRONT_IP;
  const spki = process.env.TLS_SPKI;
  const expectedEndpoint = process.env.MTPROTO_ENDPOINT;
  const expectedFingerprint = process.env.MTPROTO_FINGERPRINT;
  const expectedWebRevision = process.env.WEB_REVISION;
  const expectedIndexSHA256 = process.env.ARTIFACT_INDEX_SHA256;
  const expectedManifestSHA256 = process.env.ARTIFACT_MANIFEST_SHA256;
  if (!frontIp || !spki || expectedEndpoint !== 'wss://telegramd.test/apiws' ||
      !/^[0-9a-f]{16}$/.test(expectedFingerprint || '') || !/^[0-9a-f]{40}$/.test(expectedWebRevision || '') ||
      !/^[0-9a-f]{64}$/.test(expectedIndexSHA256 || '') || !/^[0-9a-f]{64}$/.test(expectedManifestSHA256 || '')) {
    return fail('missing_artifact_runtime_inputs', undefined, 'artifact_browser_inputs');
  }

  let browser;
  let observer;
  let failedStage = 'chromium_launch';
  try {
    browser = await chromium.launch({
      headless: true,
      chromiumSandbox: true,
      args: [
        `--host-resolver-rules=MAP telegramd.test ${frontIp},MAP * ~NOTFOUND`,
        `--ignore-certificate-errors-spki-list=${spki}`,
        '--disable-background-networking',
        '--disable-component-update',
        '--disable-default-apps',
        '--disable-domain-reliability',
        '--disable-quic',
        '--disable-sync',
        '--dns-prefetch-disable',
      ],
    });
    failedStage = 'artifact_context_setup';
    const browserCdp = await browser.newBrowserCDPSession();
    observer = createNetworkObserver(browserCdp);
    await observer.start();
    const context = await browser.newContext();
    const page = await context.newPage();
    const pageCdp = await context.newCDPSession(page);
    await observer.attachPage(pageCdp);
    await observer.waitForSetup();

    failedStage = 'artifact_entry';
    const entryResponse = await page.goto(origin, { waitUntil: 'load', timeout: 20000 });
    if (entryResponse?.status() !== 200) return fail('artifact_entry_not_served', undefined, failedStage);
    const entryBytes = await entryResponse.body();
    const entrySHA256 = createHash('sha256').update(entryBytes).digest('hex');
    if (entrySHA256 !== expectedIndexSHA256) return fail('artifact_entry_hash_mismatch', undefined, failedStage);

    failedStage = 'artifact_manifest';
    const manifestResult = await page.evaluate(async () => {
      const response = await fetch('/mtproto-target.json', { cache: 'no-store' });
      return { status: response.status, body: await response.text() };
    });
    const manifestSHA256 = createHash('sha256').update(manifestResult.body).digest('hex');
    let manifest;
    try { manifest = JSON.parse(manifestResult.body); } catch { manifest = null; }
    if (manifestResult.status !== 200 || manifestSHA256 !== expectedManifestSHA256 ||
        manifest?.mode !== 'private' || manifest?.endpoint !== expectedEndpoint ||
        manifest?.fingerprint !== expectedFingerprint || manifest?.sourceCommit !== expectedWebRevision) {
      return fail('artifact_manifest_mismatch', undefined, failedStage);
    }

    failedStage = 'artifact_service_worker';
    await page.waitForFunction(() => Boolean(navigator.serviceWorker?.controller), null, { timeout: 20000 });
    const controllerURL = await page.evaluate(() => navigator.serviceWorker.controller?.scriptURL || '');
    if (!controllerURL.startsWith(`${origin}/`) || controllerURL.startsWith(`${origin}/_fixture_probe/`)) {
      return fail('artifact_service_worker_controller_mismatch', undefined, failedStage);
    }

    failedStage = 'artifact_worker_observation';
    const workerDeadline = Date.now() + 15000;
    let workerTargets = { shared_worker: 0, service_worker: 0 };
    let serviceWorkerObserved = false;
    while (Date.now() < workerDeadline) {
      await page.waitForTimeout(250);
      await observer.waitForSetup();
      const attachedTargets = observer.attachedTargets();
      workerTargets = Object.fromEntries(['shared_worker', 'service_worker'].map((type) => [
        type,
        attachedTargets.filter((target) => target.type === type).length,
      ]));
      serviceWorkerObserved = attachedTargets.some((target) => target.type === 'service_worker' && target.url === controllerURL);
      if (serviceWorkerObserved && workerTargets.shared_worker >= 1) break;
    }
    if (!serviceWorkerObserved || workerTargets.shared_worker < 1) {
      return fail('artifact_worker_targets_missing', undefined, failedStage, workerTargets);
    }

    failedStage = 'artifact_response_headers';
    const artifactResponses = observer.responses.filter((response) => {
      let parsed;
      try { parsed = new URL(response.url); } catch { return false; }
      return response.status === 200 && parsed.origin === origin && parsed.pathname !== '/healthz' && !parsed.pathname.startsWith('/_fixture_probe/');
    });
    const artifactResponsesWithPrivateCSP = artifactResponses.filter((response) =>
      response.status === 200 && response.content_security_policy === PRIVATE_CSP
    ).length;
    const loadedScripts = artifactResponses.some((response) => /\.m?js$/i.test(new URL(response.url).pathname));
    const loadedStyles = artifactResponses.some((response) => /\.css$/i.test(new URL(response.url).pathname));
    const workerResponses = artifactResponses.filter((response) =>
      ['shared_worker', 'service_worker'].includes(response.context)
    );
    const unexpectedAttempts = observer.events.filter((event) => event.classification === 'unexpected');
    const passed = artifactResponses.length >= 4 && artifactResponsesWithPrivateCSP === artifactResponses.length &&
      artifactResponses.every((response) => response.status === 200) && loadedScripts && loadedStyles &&
      unexpectedAttempts.length === 0 && observer.errors.length === 0;
    if (!passed) return fail('artifact_browser_assertions_failed', undefined, failedStage, {
      artifact_responses: artifactResponses.length,
      artifact_responses_with_private_csp: artifactResponsesWithPrivateCSP,
      loaded_scripts: loadedScripts,
      loaded_styles: loadedStyles,
      worker_responses: workerResponses.length,
      unexpected_attempts: unexpectedAttempts.length,
      unexpected_attempt_details: unexpectedAttempts.slice(0, 12).map((event) => ({
        context: event.context,
        kind: event.kind,
        url: event.url,
        ...(event.policy_violation ? { violated_directive: event.policy_violation.violated_directive } : {}),
      })),
      observer_errors: observer.errors.length,
    });

    process.stdout.write(`${JSON.stringify({
      status: 'passed',
      entry_sha256: entrySHA256,
      manifest_sha256: manifestSHA256,
      entry_response_status: entryResponse.status(),
      manifest_response_status: manifestResult.status,
      artifact_responses: artifactResponses.length,
      artifact_responses_with_private_csp: artifactResponsesWithPrivateCSP,
      worker_targets: workerTargets,
      service_worker_controller: controllerURL,
      unexpected_attempts: unexpectedAttempts.length,
      observer_errors: observer.errors.length,
    })}\n`);
  } catch (error) {
    return fail('artifact_browser_probe_failed', safeErrorClass(error), failedStage, {
      message: String(error?.message || 'unknown_error').slice(0, 240),
    });
  } finally {
    observer?.stop();
    await browser?.close();
  }
}

main().catch((error) => fail('browser_probe_failed', safeErrorClass(error), failedStage, {
  message: String(error?.message || 'unknown_error').slice(0, 240),
}));
