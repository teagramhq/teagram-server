#!/usr/bin/env node

import {mkdtemp, rm} from "node:fs/promises";
import {statfsSync} from "node:fs";
import {join, resolve} from "node:path";
import {fileURLToPath, pathToFileURL} from "node:url";

const TARGET_HOST = "teagram.tailaa4918.ts.net";
const PROXY_BYPASS_RULE = "<-loopback>";
const PREFLIGHT_READY = "browser acceptance preflight ready";
const ACCEPTANCE_FAILED = "browser acceptance failed";
// teagram-web keeps four account slots in its MTProto SharedWorker. On an
// unauthenticated load each slot opens one client socket to its base DC and
// nothing else, so a healthy load opens at most this many sockets.
export const WSS_CONNECTION_CEILING = 4;
const REQUIRED_CHROMIUM_ARGS = [
  "--disable-background-networking",
  "--disable-crash-reporter",
  "--disable-breakpad",
  "--disable-quic",
  "--dns-over-https-mode=off",
  "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
];
const POLICY_KEYS = [
  "proxyServer",
  "proxyBypassHosts",
  "observerAllowedTargets",
  "observerConnectOnly",
  "observerHeaderLimitBytes",
  "observerConnectTimeoutEnforced",
  "observerHealthy",
  "browserNetworkInternal",
  "observerOnlyEgress",
  "directTargetTcpBlocked",
  "allowedTargetResolvedToTailnet",
  "blockedHostControlPassed",
  "blockedHostDnsLookups",
  "blockedHostUpstreamConnects",
  "nonProxiedUdpBlocked",
];
const SANDBOX_KEYS = [
  "chromiumSandboxEnabled",
  "forbiddenSandboxFlags",
  "observedProxyServer",
  "observedProxyBypassList",
  "proxyServerArgCount",
  "proxyBypassArgCount",
  "missingRequiredNetworkArgs",
  "conflictingNetworkArgs",
  "browserUserNamespaceInode",
  "browserPidNamespaceInode",
  "rendererUserNamespaceInode",
  "rendererPidNamespaceInode",
];
const RESULT_KEYS = [
  "schemaVersion",
  "ok",
  "phase",
  "outcome",
  "logout",
  "targetHost",
  "sourceCommit",
  "wssConnections",
];
const SAFE_FAILURES = new Set([
  "arguments-invalid",
  "asset-server-error",
  "authentication-repeated",
  "blocked-host-control-failed",
  "browser-closed",
  "browser-cleanup-failed",
  "browser-unavailable",
  "client-progress-unavailable",
  "configuration-not-safe",
  "credential-input-invalid",
  "credential-clear-failed",
  "direct-egress-open",
  "endpoint-mismatch",
  "manifest-fetch-failed",
  "manifest-invalid",
  "manifest-mismatch",
  "negative-asset-patch-missing",
  "negative-target-not-rejected",
  "origin-not-ready",
  "observer-unhealthy",
  "profile-storage-not-tmpfs",
  "proxy-policy-invalid",
  "release-record-invalid",
  "runtime-adapter-unavailable",
  "sandbox-proof-invalid",
  "signup-path-shown",
  "target-address-invalid",
  "target-address-unavailable",
  "code-card-shown",
  "username-field-missing",
  "password-field-missing",
  "authentication-failed-once",
  "broken-chat-list",
  "telegram-org-attempt",
  "websocket-not-ready",
  "unexpected-failure",
]);

class AcceptanceFailure extends Error {
  constructor(code) {
    super("browser acceptance failed");
    this.code = SAFE_FAILURES.has(code) ? code : "unexpected-failure";
  }
}

function fail(code) {
  throw new AcceptanceFailure(code);
}

function isRecord(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function exactKeys(value, keys) {
  return isRecord(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([...keys].sort());
}

function safeFailureCode(error) {
  if (isBrowserClosedError(error)) return "browser-closed";
  const code = error instanceof Error ? error.code : undefined;
  return typeof code === "string" && SAFE_FAILURES.has(code) ? code : "unexpected-failure";
}

function isBrowserClosedError(error) {
  if (!error || typeof error !== "object") return false;
  if (["BrowserClosedError", "TargetClosedError"].includes(error.name)) return true;
  return typeof error.message === "string" &&
    /(?:Target page, context or browser has been closed|Target closed|browser has been closed|context has been closed|page has been closed)/iu.test(error.message);
}

function preflightFailureOutput(code) {
  const safeCode = SAFE_FAILURES.has(code) ? code : "unexpected-failure";
  return JSON.stringify({status: "error", code: safeCode});
}

function requestedPreflight(argv) {
  return argv.some((argument, index) => argument === "--mode" && argv[index + 1] === "preflight");
}

function watchUnexpectedBrowserClose(contextInfo) {
  const state = {expected: false, unexpected: false};
  contextInfo.browserClosure = state;
  const markClosed = () => {
    if (!state.expected) state.unexpected = true;
  };
  for (const target of [contextInfo.context, contextInfo.page]) {
    if (typeof target?.on === "function") target.on("close", markClosed);
  }
  if (typeof contextInfo.page?.on === "function") contextInfo.page.on("crash", markClosed);
  if (typeof contextInfo.context?.browser === "function") {
    try {
      const browser = contextInfo.context.browser();
      if (typeof browser?.on === "function") browser.on("disconnected", markClosed);
    } catch {
      markClosed();
    }
  }
  return state;
}

function parseArgs(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const argument = argv[index];
    if (argument === "--help") {
      if (argv.length !== 1) fail("arguments-invalid");
      return {help: true};
    }
    const key = {
      "--mode": "mode",
      "--release-record": "releaseRecord",
      "--runtime-adapter": "runtimeAdapter",
    }[argument];
    if (!key || options[key] !== undefined || !argv[index + 1] || argv[index + 1].startsWith("--")) {
      fail("arguments-invalid");
    }
    options[key] = argv[index + 1];
    index += 1;
  }
  if (!["preflight", "acceptance"].includes(options.mode) ||
      typeof options.releaseRecord !== "string" || typeof options.runtimeAdapter !== "string") {
    fail("arguments-invalid");
  }
  return options;
}

function assertTmpfs(path) {
  try {
    if (Number(statfsSync(path).type) === 0x01021994) return;
  } catch {
    // The fixed failure excludes filesystem paths and operating-system details.
  }
  fail("profile-storage-not-tmpfs");
}

function assertBrowserEnvironment(environment) {
  const forbidden = [
    "DEBUG",
    "PWDEBUG",
    "DEBUG_FILE",
    "PLAYWRIGHT_DISABLE_FORCED_CHROMIUM_PROXIED_LOOPBACK",
  ];
  if (forbidden.some((name) => environment[name] !== undefined)) fail("proxy-policy-invalid");
}

function normalizeProxyServer(value) {
  if (typeof value !== "string" || value.length === 0) fail("proxy-policy-invalid");
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    fail("proxy-policy-invalid");
  }
  if (parsed.protocol !== "http:" || !parsed.hostname || !parsed.port || parsed.username || parsed.password ||
      parsed.pathname !== "/" || parsed.search || parsed.hash) {
    fail("proxy-policy-invalid");
  }
  return parsed.protocol + "//" + parsed.host.toLowerCase();
}

function normalizeLaunchPolicy(value) {
  if (!exactKeys(value, POLICY_KEYS) || !Array.isArray(value.proxyBypassHosts) ||
      value.proxyBypassHosts.length !== 0 || !Array.isArray(value.observerAllowedTargets) ||
      value.observerAllowedTargets.length !== 1 ||
      !exactKeys(value.observerAllowedTargets[0], ["host", "port"]) ||
      value.observerAllowedTargets[0].host !== TARGET_HOST || value.observerAllowedTargets[0].port !== 443 ||
      value.observerConnectOnly !== true || value.observerHeaderLimitBytes !== 8192 ||
      value.observerConnectTimeoutEnforced !== true || value.observerHealthy !== true ||
      value.browserNetworkInternal !== true || value.observerOnlyEgress !== true ||
      value.directTargetTcpBlocked !== true || value.allowedTargetResolvedToTailnet !== true ||
      value.blockedHostControlPassed !== true || value.blockedHostDnsLookups !== 0 ||
      value.blockedHostUpstreamConnects !== 0 || value.nonProxiedUdpBlocked !== true) {
    fail("proxy-policy-invalid");
  }
  return {
    proxyServer: normalizeProxyServer(value.proxyServer),
    proxyBypassHosts: [],
    observerAllowedTargets: [{host: TARGET_HOST, port: 443}],
    observerConnectOnly: true,
    observerHeaderLimitBytes: 8192,
    observerConnectTimeoutEnforced: true,
    observerHealthy: true,
    browserNetworkInternal: true,
    observerOnlyEgress: true,
    directTargetTcpBlocked: true,
    allowedTargetResolvedToTailnet: true,
    blockedHostControlPassed: true,
    blockedHostDnsLookups: 0,
    blockedHostUpstreamConnects: 0,
    nonProxiedUdpBlocked: true,
  };
}

function validateSandboxProof(value, expectedProxyServer) {
  if (!exactKeys(value, SANDBOX_KEYS) || value.chromiumSandboxEnabled !== true ||
      !Array.isArray(value.forbiddenSandboxFlags) || value.forbiddenSandboxFlags.length !== 0 ||
      value.observedProxyServer !== expectedProxyServer || value.observedProxyBypassList !== PROXY_BYPASS_RULE ||
      value.proxyServerArgCount !== 1 || value.proxyBypassArgCount !== 1 ||
      !Array.isArray(value.missingRequiredNetworkArgs) || value.missingRequiredNetworkArgs.length !== 0 ||
      !Array.isArray(value.conflictingNetworkArgs) || value.conflictingNetworkArgs.length !== 0) {
    fail("sandbox-proof-invalid");
  }
  const namespaceInodes = [
    value.browserUserNamespaceInode,
    value.browserPidNamespaceInode,
    value.rendererUserNamespaceInode,
    value.rendererPidNamespaceInode,
  ];
  if (!namespaceInodes.every((inode) => Number.isSafeInteger(inode) && inode > 0) ||
      value.browserUserNamespaceInode === value.rendererUserNamespaceInode ||
      value.browserPidNamespaceInode === value.rendererPidNamespaceInode) {
    fail("sandbox-proof-invalid");
  }
}

function validateManifest(value, release) {
  const manifestKeys = ["mode", "endpoint", "fingerprint", "sourceCommit", "artifactDigest"];
  if (!exactKeys(value, manifestKeys) || value.mode !== "private" ||
      value.sourceCommit !== release.sourceCommit || value.artifactDigest !== release.contentDigest ||
      typeof value.fingerprint !== "string" || !/^[0-9a-f]{16}$/u.test(value.fingerprint) ||
      typeof value.endpoint !== "string") {
    fail("manifest-mismatch");
  }
  let endpoint;
  try {
    endpoint = new URL(value.endpoint);
  } catch {
    fail("manifest-mismatch");
  }
  if (endpoint.protocol !== "wss:" || endpoint.hostname !== TARGET_HOST ||
      (endpoint.port && endpoint.port !== "443") || endpoint.username || endpoint.password ||
      endpoint.search || endpoint.hash || endpoint.href !== value.endpoint) {
    fail("manifest-mismatch");
  }
  return {endpoint: endpoint.href, fingerprint: value.fingerprint};
}

function validateV2Result(value, sourceCommit) {
  const phases = new Set([
    "wss-not-opened",
    "wss-not-101",
    "req-pq",
    "req-dh-params",
    "set-client-dh",
    "init-connection",
    "password-challenge",
    "password-check",
    "signed-in",
  ]);
  const outcomes = new Set([
    "ok",
    "timeout",
    "closed",
    "credential-rejected",
    "auth-key-unregistered",
    "flood",
    "internal",
    "other",
  ]);
  const logoutValues = new Set([
    "not-attempted", "ok", "timeout", "closed", "credential-rejected",
    "auth-key-unregistered", "flood", "internal", "other",
  ]);
  if (!exactKeys(value, RESULT_KEYS) || value.schemaVersion !== 2 || typeof value.ok !== "boolean" ||
      !phases.has(value.phase) || !outcomes.has(value.outcome) || !logoutValues.has(value.logout) ||
      value.targetHost !== TARGET_HOST || value.sourceCommit !== sourceCommit ||
      !Number.isInteger(value.wssConnections) || value.wssConnections < 0 ||
      value.wssConnections > WSS_CONNECTION_CEILING + 1 ||
      (value.ok && (value.phase !== "signed-in" || value.outcome !== "ok" || value.logout !== "ok" ||
        value.wssConnections < 1 || value.wssConnections > WSS_CONNECTION_CEILING))) {
    fail("unexpected-failure");
  }
  return value;
}

function clearCredentials(credentials) {
  if (!isRecord(credentials)) return;
  if (typeof credentials.username === "string") credentials.username = "";
  if (typeof credentials.password === "string") credentials.password = "";
}

function makeProgressDependencies(adapter) {
  return {
    readReleaseRecord: adapter.readReleaseRecord,
    createAcceptanceProgress: adapter.createAcceptanceProgress,
    acceptanceRpcOutcome: adapter.acceptanceRpcOutcome,
    browserLaunchPolicy: adapter.browserLaunchPolicy,
    verifySandbox: adapter.verifySandbox,
  };
}

export async function prepareAcceptance(options, dependencies) {
  let release;
  let progress;
  let contextInfo;
  let browserClosure;
  let code = "unexpected-failure";
  try {
    release = await dependencies.readReleaseRecord(options.releaseRecord);
    progress = dependencies.createAcceptanceProgress({sourceCommit: release.sourceCommit});

    const policyValue = await dependencies.browserLaunchPolicy({targetHost: TARGET_HOST});
    const policy = normalizeLaunchPolicy(policyValue);
    contextInfo = await dependencies.launchContext(policy, "preflight");
    browserClosure = watchUnexpectedBrowserClose(contextInfo);

    const sandboxProof = await dependencies.verifySandbox(contextInfo, policy, "preflight");
    validateSandboxProof(sandboxProof, policy.proxyServer);

    const tracker = await dependencies.installNetworkGuards(contextInfo, release, null);
    const manifest = await dependencies.readManifestThroughChromium(contextInfo.page, release);
    const normalizedManifest = validateManifest(manifest, release);
    tracker.expectedEndpoint = normalizedManifest.endpoint;

    const wssAudit = await dependencies.proveWss(contextInfo, release, normalizedManifest, progress, tracker);
    await dependencies.verifyAssetLoading(contextInfo, release, tracker);
    const progressState = await dependencies.installClientProgressObserver(
      contextInfo.page,
      progress,
      dependencies.acceptanceRpcOutcome,
    );
    await dependencies.verifyNegativeTargets(policy, release, normalizedManifest);
    if (browserClosure.unexpected) fail("browser-closed");

    return {
      ok: true,
      release,
      progress,
      policy,
      contextInfo,
      tracker,
      manifest: normalizedManifest,
      wssAudit,
      progressState,
      contextClosed: false,
    };
  } catch (error) {
    code = browserClosure?.unexpected ? "browser-closed" : safeFailureCode(error);
    if (contextInfo) {
      try {
        await dependencies.closeContext(contextInfo);
      } catch {
        code = browserClosure?.unexpected ? "browser-closed" : "browser-cleanup-failed";
      }
    }
    return {ok: false, code, release, progress, contextInfo: null};
  }
}

function rpcErrorForOutcome(outcome) {
  switch (outcome) {
    case "credential-rejected": return {error_code: 400, error_message: "PASSWORD_HASH_INVALID"};
    case "auth-key-unregistered": return {error_code: 401, error_message: "AUTH_KEY_UNREGISTERED"};
    case "flood": return {error_code: 420, error_message: "FLOOD_WAIT_1"};
    case "internal": return {error_code: 500, error_message: "INTERNAL"};
    default: return {error_code: 0, error_message: ""};
  }
}

function recordRpcFailure(progress, acceptanceRpcOutcome, outcome) {
  if (outcome === "timeout" || outcome === "closed") {
    progress.recordClientFailure({kind: outcome});
    return;
  }
  const normalizedError = rpcErrorForOutcome(outcome);
  if (acceptanceRpcOutcome(normalizedError) !== outcome) fail("unexpected-failure");
  progress.recordRpcError(normalizedError);
}

async function failedAcceptanceResult(prepared, dependencies, error) {
  if (!prepared.progress) return null;
  const outcome = error && ["timeout", "closed"].includes(error.kind)
    ? error.kind
    : "other";
  recordRpcFailure(prepared.progress, dependencies.acceptanceRpcOutcome, outcome);
  return validateV2Result(
    await prepared.progress.finish(async () => { throw {kind: "closed"}; }),
    prepared.release.sourceCommit,
  );
}

export async function executeMode(options, dependencies) {
  const prepared = await prepareAcceptance(options, dependencies);
  if (!prepared.ok) {
    if (options.mode === "preflight") return {output: preflightFailureOutput(prepared.code), exitCode: 1};
    try {
      const result = await failedAcceptanceResult(prepared, dependencies, {kind: "closed"});
      if (result) return {output: JSON.stringify(result), exitCode: 1};
    } catch {
      // The fixed failure below carries no runtime details.
    }
    return {output: ACCEPTANCE_FAILED, exitCode: 1};
  }

  if (options.mode === "preflight") {
    try {
      await dependencies.closeContext(prepared.contextInfo);
      prepared.contextClosed = true;
      return {output: PREFLIGHT_READY, exitCode: 0};
    } catch {
      return {output: preflightFailureOutput("browser-cleanup-failed"), exitCode: 1};
    }
  }

  let credentials;
  let result;
  let exitCode = 1;
  try {
    credentials = await dependencies.readCredentials();
    result = validateV2Result(
      await dependencies.performAcceptance(prepared, credentials),
      prepared.release.sourceCommit,
    );
    exitCode = result.ok ? 0 : 1;
  } catch (error) {
    try {
      result = await failedAcceptanceResult(prepared, dependencies, error);
    } catch {
      result = null;
    }
  } finally {
    clearCredentials(credentials);
    if (!prepared.contextClosed) {
      try {
        await dependencies.closeContext(prepared.contextInfo);
      } catch {
        exitCode = 1;
      }
    }
  }
  return result
    ? {output: JSON.stringify(result), exitCode}
    : {output: ACCEPTANCE_FAILED, exitCode: 1};
}

function createNetworkTracker() {
  return {
    expectedEndpoint: "",
    websocketCount: 0,
    websocketMismatch: false,
    telegramOrgAttemptCount: 0,
    telegramOrgWebSocketAttemptCount: 0,
    assetResponses: 0,
    assetFailures: 0,
    asset5xx: 0,
    asset502: 0,
    routeFailures: 0,
    pageErrors: 0,
    targetValidationErrors: 0,
    negativePatchCount: 0,
    invalidEndpointCount: 0,
  };
}

function isTelegramOrg(hostname) {
  const normalized = hostname.toLowerCase().replace(/\.+$/u, "");
  return normalized === "telegram.org" || normalized.endsWith(".telegram.org");
}

function isAssetType(resourceType) {
  return ["document", "script", "stylesheet", "image", "font"].includes(resourceType);
}

function patchTarget(text, mode, manifest) {
  if (mode === "negative-absent-endpoint") {
    const count = text.split(manifest.endpoint).length - 1;
    return {text: text.split(manifest.endpoint).join(""), count};
  }
  const replacement = (manifest.fingerprint[0] === "0" ? "1" : "0") + manifest.fingerprint.slice(1);
  const count = text.split(manifest.fingerprint).length - 1;
  return {text: text.split(manifest.fingerprint).join(replacement), count};
}

export async function installNetworkGuards(contextInfo, release, patchMode) {
  const tracker = createNetworkTracker();
  const context = contextInfo.context;
  if (typeof context.routeWebSocket !== "function") fail("browser-unavailable");
  await context.routeWebSocket((socketUrl) => {
    try {
      return isTelegramOrg(new URL(socketUrl).hostname);
    } catch {
      tracker.invalidEndpointCount += 1;
      return false;
    }
  }, (route) => {
    tracker.telegramOrgWebSocketAttemptCount += 1;
    route.close();
  });

  await context.route("**/*", async (route) => {
    const request = route.request();
    let parsed;
    try {
      parsed = new URL(request.url());
    } catch {
      tracker.invalidEndpointCount += 1;
      tracker.routeFailures += 1;
      await route.abort();
      return;
    }
    if (isTelegramOrg(parsed.hostname)) {
      tracker.telegramOrgAttemptCount += 1;
      await route.abort();
      return;
    }
    if (patchMode && request.resourceType() === "script" && parsed.origin === new URL(release.url).origin) {
      try {
        const response = await route.fetch();
        const body = await response.text();
        const patched = patchTarget(body, patchMode, contextInfo.manifest);
        tracker.negativePatchCount += patched.count;
        await route.fulfill({response, body: patched.text});
      } catch {
        tracker.routeFailures += 1;
        await route.abort();
      }
      return;
    }
    try {
      await route.continue();
    } catch {
      tracker.routeFailures += 1;
    }
  });

  contextInfo.page.on("response", (response) => {
    if (!isAssetType(response.request().resourceType())) return;
    tracker.assetResponses += 1;
    if (response.status() >= 500) tracker.asset5xx += 1;
    if (response.status() === 502) tracker.asset502 += 1;
  });
  contextInfo.page.on("requestfailed", (request) => {
    if (isAssetType(request.resourceType())) tracker.assetFailures += 1;
  });
  contextInfo.page.on("websocket", (socket) => {
    tracker.websocketCount += 1;
    let matches = false;
    try {
      matches = new URL(socket.url()).href === tracker.expectedEndpoint;
    } catch {
      tracker.invalidEndpointCount += 1;
    }
    if (!matches) tracker.websocketMismatch = true;
  });
  contextInfo.page.on("pageerror", (error) => {
    tracker.pageErrors += 1;
    if (/embedded target metadata/iu.test(String(error?.message ?? ""))) {
      tracker.targetValidationErrors += 1;
    }
  });
  return tracker;
}

function chromiumOptions(policy) {
  return {
    chromiumSandbox: true,
    headless: true,
    locale: "en-US",
    serviceWorkers: "allow",
    viewport: {width: 1440, height: 1000},
    proxy: {server: policy.proxyServer, bypass: ""},
    args: [...REQUIRED_CHROMIUM_ARGS],
  };
}

async function launchContext(playwright, policy) {
  assertTmpfs("/dev/shm");
  assertBrowserEnvironment(process.env);
  process.env.TMPDIR = "/dev/shm";
  process.env.TMP = "/dev/shm";
  process.env.TEMP = "/dev/shm";
  let profileDir;
  try {
    profileDir = await mkdtemp(join("/dev/shm", "browser-acceptance-v2-"));
  } catch {
    fail("browser-unavailable");
  }
  let context;
  try {
    context = await playwright.chromium.launchPersistentContext(profileDir, chromiumOptions(policy));
  } catch {
    try {
      await rm(profileDir, {recursive: true, force: true});
    } catch {
      fail("browser-cleanup-failed");
    }
    fail("browser-unavailable");
  }
  const page = context.pages()[0] || await context.newPage();
  return {context, page, profileDir, policy, closed: false, manifest: null, tracker: null, wssAudit: null};
}

async function closeContext(contextInfo) {
  if (!contextInfo || contextInfo.closed) return;
  if (contextInfo.browserClosure) contextInfo.browserClosure.expected = true;
  let closeFailed = false;
  try {
    await contextInfo.context.close();
  } catch {
    closeFailed = true;
  }
  try {
    await rm(contextInfo.profileDir, {recursive: true, force: true});
  } catch {
    closeFailed = true;
  }
  contextInfo.closed = true;
  if (closeFailed) fail("browser-cleanup-failed");
}

async function readManifestThroughChromium(page, release) {
  const manifestUrl = new URL("mtproto-target.json", release.url);
  manifestUrl.searchParams.set("v", release.sourceCommit);
  let response;
  try {
    response = await page.goto(manifestUrl.href, {waitUntil: "domcontentloaded", timeout: 15000});
  } catch {
    fail("manifest-fetch-failed");
  }
  if (!response || response.status() !== 200) fail("manifest-fetch-failed");
  let manifest;
  try {
    manifest = await page.evaluate(async (url) => {
      const response = await fetch(url, {cache: "no-store", redirect: "error"});
      if (!response.ok) return null;
      const value = await response.json();
      return {
        exactKeys: JSON.stringify(Object.keys(value).sort()) === JSON.stringify([
          "artifactDigest", "endpoint", "fingerprint", "mode", "sourceCommit",
        ]),
        mode: value.mode,
        endpoint: value.endpoint,
        fingerprint: value.fingerprint,
        sourceCommit: value.sourceCommit,
        artifactDigest: value.artifactDigest,
      };
    }, manifestUrl.href);
  } catch {
    fail("manifest-fetch-failed");
  }
  if (!manifest || manifest.exactKeys !== true) fail("manifest-invalid");
  return {
    mode: manifest.mode,
    endpoint: manifest.endpoint,
    fingerprint: manifest.fingerprint,
    sourceCommit: manifest.sourceCommit,
    artifactDigest: manifest.artifactDigest,
  };
}

// The web client opens its MTProto socket from a SharedWorker, which is a
// browser-level target the page session never sees. Shared and service
// workers are auto-attached at the browser target, dedicated workers at the
// page target, and every worker's own workers through autoAttachRelated.
// Playwright's CDP sessions cannot address flat child sessions, so worker
// traffic is read through non-flat sessions.
async function observeWorkerWebSockets(browser, pageSession, handlers) {
  const browserSession = await browser.newBrowserCDPSession();
  // Auto-attach for descendants needs its own session: autoAttachRelated replaces
  // a session's setAutoAttach.
  const relatedSession = await browser.newBrowserCDPSession();
  const sessions = new Map();
  const targets = new Set();
  let messageId = 0;

  function receive(event) {
    const target = isRecord(event) ? sessions.get(event.sessionId) : undefined;
    if (!target) return;
    let message;
    try {
      message = JSON.parse(event.message);
    } catch {
      message = null;
    }
    if (!isRecord(message)) {
      handlers.unobservable();
      return;
    }
    if (message.id !== undefined) {
      const reply = target.replies.get(message.id);
      if (!reply) return;
      target.replies.delete(message.id);
      if (message.error !== undefined) {
        reply.reject(new Error("worker command failed"));
        return;
      }
      // Set before any later message from this worker is handled.
      if (message.id === target.networkEnableId) target.networkEnabled = true;
      reply.resolve();
    } else if (message.method === "Inspector.workerScriptLoaded") {
      target.scriptLoaded(target.networkEnabled);
    } else if (message.method === "Network.webSocketCreated") {
      handlers.created(message.params);
    } else if (message.method === "Network.webSocketHandshakeResponseReceived") {
      handlers.handshakeResponse(message.params);
    }
  }

  function command(target, method) {
    messageId += 1;
    const id = messageId;
    if (method === "Network.enable") target.networkEnableId = id;
    return new Promise((resolve, reject) => {
      target.replies.set(id, {resolve, reject});
      browserSession.send("Target.sendMessageToTarget", {
        sessionId: target.sessionId,
        message: JSON.stringify({id, method, params: {}}),
      }).catch(reject);
    });
  }

  // A flat auto-attach hold through the page target, or through autoAttachRelated
  // for a worker's own workers, keeps a dedicated worker from starting until it is
  // detached here. Playwright resumes shared and service workers from its own
  // browser session, so those cannot be held: Chrome reports a worker's script as
  // loaded on attach when it has already run, and such a worker is unobservable.
  // Every worker stays pending until its hold is released and its script has
  // loaded after its Network domain was enabled.
  async function attachHeldTarget(holder, event) {
    const targetId = event?.targetInfo?.targetId;
    if (typeof targetId !== "string" || typeof event.sessionId !== "string") {
      handlers.unobservable();
      return;
    }
    // Attaching the non-flat session reports this target again; it is already handled.
    if (targets.has(targetId)) return;
    targets.add(targetId);
    const finish = handlers.attached();
    let loadedAfterEnable;
    let ready = false;
    try {
      // Descendants are watched before this worker's own session exists, so its
      // script only counts as loaded after both; children that already exist are
      // reported when the watch starts. Service workers cannot start workers.
      if (event.targetInfo.type !== "service_worker") {
        await relatedSession.send("Target.autoAttachRelated", {
          targetId,
          waitForDebuggerOnStart: true,
          filter: [{type: "worker"}],
        });
      }
      const {sessionId} = await browserSession.send("Target.attachToTarget", {targetId, flatten: false});
      let scriptLoaded;
      loadedAfterEnable = new Promise((resolve) => {
        scriptLoaded = resolve;
      });
      const target = {sessionId, replies: new Map(), networkEnableId: 0, networkEnabled: false, scriptLoaded};
      sessions.set(sessionId, target);
      await command(target, "Network.enable");
      await command(target, "Runtime.runIfWaitingForDebugger");
      ready = true;
    } catch {
      // The hold is still released below; the worker counts as unobservable.
    }
    let released = false;
    try {
      await holder.send("Target.detachFromTarget", {sessionId: event.sessionId});
      released = true;
    } catch {
      // An unreleased hold leaves the worker unobservable.
    }
    // A held worker only loads its script once released.
    finish(ready && released && await loadedAfterEnable);
  }

  browserSession.on("Target.attachedToTarget", (event) => { attachHeldTarget(browserSession, event); });
  browserSession.on("Target.receivedMessageFromTarget", receive);
  pageSession.on("Target.attachedToTarget", (event) => { attachHeldTarget(pageSession, event); });
  relatedSession.on("Target.attachedToTarget", (event) => { attachHeldTarget(relatedSession, event); });
  await browserSession.send("Target.setAutoAttach", {
    autoAttach: true,
    waitForDebuggerOnStart: true,
    flatten: true,
    filter: [{type: "shared_worker"}, {type: "service_worker"}],
  });
  await pageSession.send("Target.setAutoAttach", {
    autoAttach: true,
    waitForDebuggerOnStart: true,
    flatten: true,
    filter: [{type: "worker"}],
  });
}

async function makeWssAudit(contextInfo, progress) {
  const connections = new Map();
  let malformed = false;
  let handshakeCount = 0;
  let pendingWorkers = 0;
  const handlers = {
    created(params) {
      if (!isRecord(params) || typeof params.requestId !== "string" ||
          !/^[0-9A-Za-z._-]{1,64}$/u.test(params.requestId) || typeof params.url !== "string" ||
          connections.has(params.requestId)) {
        malformed = true;
        return;
      }
      if (connections.size >= WSS_CONNECTION_CEILING) {
        // The audit is not validated again after sign-in, so a socket above the
        // ceiling opened during logout must still reach the result.
        malformed = true;
        progress.observeWssConnection();
        return;
      }
      let matches = false;
      try {
        matches = new URL(params.url).href === contextInfo.manifest.endpoint;
      } catch {
        malformed = true;
      }
      connections.set(params.requestId, {matches, responses: 0, status: 0});
      progress.observeWssConnection();
    },
    handshakeResponse(params) {
      if (!isRecord(params) || typeof params.requestId !== "string" || !isRecord(params.response) ||
          !Number.isInteger(params.response.status) || params.response.status < 100 || params.response.status > 599) {
        malformed = true;
        return;
      }
      handshakeCount = Math.min(WSS_CONNECTION_CEILING + 1, handshakeCount + 1);
      const connection = connections.get(params.requestId);
      if (!connection) {
        malformed = true;
        return;
      }
      connection.responses += 1;
      if (connection.responses === 1) connection.status = params.response.status;
      progress.observeWssHandshake(params.response.status);
    },
    unobservable() {
      malformed = true;
    },
    attached() {
      pendingWorkers += 1;
      let finished = false;
      return (observable) => {
        if (finished) return;
        finished = true;
        pendingWorkers -= 1;
        if (observable !== true) malformed = true;
      };
    },
  };
  const session = await contextInfo.context.newCDPSession(contextInfo.page);
  await session.send("Network.enable");
  session.on("Network.webSocketCreated", handlers.created);
  session.on("Network.webSocketHandshakeResponseReceived", handlers.handshakeResponse);
  await observeWorkerWebSockets(contextInfo.context.browser(), session, handlers);
  return {
    session,
    connections,
    isSettled() {
      return pendingWorkers === 0 && connections.size > 0 &&
        [...connections.values()].every((item) => item.responses === 1);
    },
    validate() {
      const entries = [...connections.values()];
      if (malformed || pendingWorkers !== 0 || entries.length < 1 || entries.length > WSS_CONNECTION_CEILING ||
          handshakeCount !== entries.length || entries.some((item) => !item.matches ||
            item.responses !== 1 || item.status !== 101)) {
        fail("websocket-not-ready");
      }
      return {connectionCount: entries.length, status101: true};
    },
  };
}

export async function proveWss(contextInfo, release, manifest, progress, tracker) {
  contextInfo.manifest = manifest;
  tracker.expectedEndpoint = manifest.endpoint;
  const audit = await makeWssAudit(contextInfo, progress);
  contextInfo.wssAudit = audit;
  let response;
  try {
    response = await contextInfo.page.goto(release.url, {waitUntil: "domcontentloaded", timeout: 60000});
  } catch {
    fail("origin-not-ready");
  }
  if (!response || response.status() !== 200) fail("origin-not-ready");
  // A page handshake can precede attachment of its workers and their descendants.
  // Keep the same minimum observation window as the readiness probe before settling.
  const startedAt = Date.now();
  const minimumEnd = startedAt + 1000;
  const deadline = startedAt + 15000;
  while ((Date.now() < minimumEnd || !audit.isSettled()) && Date.now() < deadline) {
    await contextInfo.page.waitForTimeout(50);
  }
  const result = audit.validate();
  if (tracker.telegramOrgAttemptCount || tracker.telegramOrgWebSocketAttemptCount || tracker.websocketMismatch) {
    fail("telegram-org-attempt");
  }
  return {responseStatus: response.status(), ...result};
}

function assertNoUnsafeNetwork(tracker) {
  if (tracker.telegramOrgAttemptCount !== 0 || tracker.telegramOrgWebSocketAttemptCount !== 0) {
    fail("telegram-org-attempt");
  }
  if (tracker.asset502 !== 0 || tracker.asset5xx !== 0 || tracker.routeFailures !== 0 ||
      tracker.invalidEndpointCount !== 0) {
    fail("asset-server-error");
  }
}

async function verifyAssetLoading(contextInfo, release, tracker) {
  const usernameInput = contextInfo.page.locator('input[aria-label="Username"]');
  try {
    await usernameInput.waitFor({state: "visible", timeout: 30000});
    await contextInfo.page.waitForTimeout(1000);
  } catch {
    fail("origin-not-ready");
  }
  if (tracker.assetResponses < 1) fail("asset-server-error");
  assertNoUnsafeNetwork(tracker);
  if (contextInfo.wssAudit) contextInfo.wssAudit.validate();
}

async function visible(locator) {
  try {
    return await locator.isVisible();
  } catch {
    return false;
  }
}

async function runNegativeTarget(playwright, adapter, policy, release, manifest, mode) {
  const contextInfo = await launchContext(playwright, policy);
  try {
    const sandboxProof = await adapter.verifySandbox({phase: mode, expectedProxyServer: policy.proxyServer});
    validateSandboxProof(sandboxProof, policy.proxyServer);
    const tracker = await installNetworkGuards(contextInfo, release, mode);
    contextInfo.manifest = manifest;
    tracker.expectedEndpoint = manifest.endpoint;
    try {
      await contextInfo.page.goto(release.url, {waitUntil: "domcontentloaded", timeout: 60000});
    } catch {
      // A rejected document navigation is expected when the patched target is refused.
    }
    await contextInfo.page.waitForTimeout(2500);
    const visibleAuthInputs = await contextInfo.page.locator(
      'input[aria-label="Username"], input[type="password"], input[autocomplete="one-time-code"]',
    ).evaluateAll((inputs) => inputs.filter((input) => {
      const element = input;
      return Boolean(element.offsetWidth || element.offsetHeight || element.getClientRects().length);
    }).length);
    if (tracker.negativePatchCount < 1 || visibleAuthInputs !== 0 || tracker.websocketCount !== 0 ||
        tracker.targetValidationErrors < 1) {
      fail("negative-target-not-rejected");
    }
    assertNoUnsafeNetwork(tracker);
  } finally {
    await closeContext(contextInfo);
  }
}

async function verifyNegativeTargets(playwright, adapter, policy, release, manifest) {
  await runNegativeTarget(playwright, adapter, policy, release, manifest, "negative-absent-endpoint");
  await runNegativeTarget(playwright, adapter, policy, release, manifest, "negative-inconsistent-fingerprint");
}

function rpcOutcomeForPageError(error) {
  const code = Number.isInteger(error?.code) ? error.code : error?.error_code;
  const message = typeof error?.type === "string" ? error.type : error?.error_message;
  if (code === 400 && message === "PASSWORD_HASH_INVALID") return "credential-rejected";
  if (code === 401 && message === "AUTH_KEY_UNREGISTERED") return "auth-key-unregistered";
  if (code === 420 && typeof message === "string" && /^FLOOD_WAIT_[0-9]{1,6}$/u.test(message)) return "flood";
  if (code === 500 && message === "INTERNAL") return "internal";
  if (error?.kind === "timeout" || error?.name === "TimeoutError") return "timeout";
  if (error?.kind === "closed" || error?.name === "AbortError") return "closed";
  return "other";
}

function createProgressState() {
  const state = {
    authCalls: {"auth.sendCode": 0, "auth.signIn": 0, "auth.checkPassword": 0},
    logoutRpcCalls: 0,
    logoutRpcSuccesses: 0,
    logoutRpcFailures: [],
    authFailureOutcome: null,
  };
  return state;
}

function handleClientProgressEvent(event, progress, acceptanceRpcOutcome, state) {
  if (!isRecord(event) || typeof event.kind !== "string") fail("client-progress-unavailable");
  switch (event.kind) {
    case "transport-ready":
      if (!exactKeys(event, ["kind"])) fail("client-progress-unavailable");
      for (const phase of ["req-dh-params", "set-client-dh", "init-connection"]) {
        if (!progress.observeClientEvent(phase)) fail("client-progress-unavailable");
      }
      return;
    case "auth-call":
      if (!exactKeys(event, ["kind", "method"]) || !(event.method in state.authCalls)) {
        fail("client-progress-unavailable");
      }
      state.authCalls[event.method] += 1;
      if (state.authCalls[event.method] !== 1) fail("authentication-repeated");
      if (event.method === "auth.checkPassword" && !progress.observeClientEvent("password-check")) {
        fail("client-progress-unavailable");
      }
      return;
    case "password-challenge":
      if (!exactKeys(event, ["kind"]) || !progress.observeClientEvent("password-challenge")) {
        fail("client-progress-unavailable");
      }
      return;
    case "rpc-failure":
      if (!exactKeys(event, ["kind", "outcome"]) || typeof event.outcome !== "string" ||
          !["credential-rejected", "auth-key-unregistered", "flood", "internal", "timeout", "closed", "other"].includes(event.outcome)) {
        fail("client-progress-unavailable");
      }
      state.authFailureOutcome = event.outcome;
      recordRpcFailure(progress, acceptanceRpcOutcome, event.outcome);
      return;
    case "logout-rpc-start":
      if (!exactKeys(event, ["kind"])) fail("client-progress-unavailable");
      state.logoutRpcCalls += 1;
      return;
    case "logout-rpc-success":
      if (!exactKeys(event, ["kind"])) fail("client-progress-unavailable");
      state.logoutRpcSuccesses += 1;
      return;
    case "logout-rpc-failure":
      if (!exactKeys(event, ["kind", "outcome"]) || typeof event.outcome !== "string") {
        fail("client-progress-unavailable");
      }
      state.logoutRpcFailures.push(event.outcome);
      return;
    default:
      fail("client-progress-unavailable");
  }
}

export async function installClientProgressObserver(page, progress, acceptanceRpcOutcome) {
  const state = createProgressState();
  const bindingName = "__browserAcceptanceProgressV2";
  try {
    await page.exposeFunction(bindingName, (event) => {
      handleClientProgressEvent(event, progress, acceptanceRpcOutcome, state);
    });
    const installed = await page.evaluate((bridgeName) => {
      const proxy = window.apiManagerProxy;
      if (!proxy || typeof proxy.invoke !== "function") return false;
      const original = proxy.invoke;
      const report = (event) => window[bridgeName](event);
      const rpcOutcome = (error) => {
        const code = Number.isInteger(error?.code) ? error.code : error?.error_code;
        const message = typeof error?.type === "string" ? error.type : error?.error_message;
        if (code === 400 && message === "PASSWORD_HASH_INVALID") return "credential-rejected";
        if (code === 401 && message === "AUTH_KEY_UNREGISTERED") return "auth-key-unregistered";
        if (code === 420 && typeof message === "string" && /^FLOOD_WAIT_[0-9]{1,6}$/u.test(message)) return "flood";
        if (code === 500 && message === "INTERNAL") return "internal";
        if (error?.kind === "timeout" || error?.name === "TimeoutError") return "timeout";
        if (error?.kind === "closed" || error?.name === "AbortError") return "closed";
        return "other";
      };
      let transportReported = false;
      proxy.invoke = async function(type, payload, ...rest) {
        const isManager = type === "manager" && payload && typeof payload.name === "string";
        if (!isManager) return original.call(this, type, payload, ...rest);
        const isApiManager = payload.name === "apiManager";
        const isPasswordManagerCheck = payload.name === "passwordManager" && payload.method === "check";
        if (!isApiManager && !isPasswordManagerCheck) return original.call(this, type, payload, ...rest);
        if (isPasswordManagerCheck) {
          if (!transportReported) {
            transportReported = true;
            await report({kind: "transport-ready"});
          }
          await report({kind: "auth-call", method: "auth.checkPassword"});
          try {
            return await original.call(this, type, payload, ...rest);
          } catch (error) {
            await report({kind: "rpc-failure", outcome: rpcOutcome(error)});
            throw error;
          }
        }
        if (payload.method !== "invokeApi" || !Array.isArray(payload.args) ||
            typeof payload.args[0] !== "string") {
          return original.call(this, type, payload, ...rest);
        }
        const method = payload.args[0];
        if (!transportReported) {
          transportReported = true;
          await report({kind: "transport-ready"});
        }
        const watchedAuthCall = ["auth.sendCode", "auth.signIn", "auth.checkPassword"].includes(method);
        const watchedLogoutCall = method === "auth.logOut";
        if (watchedAuthCall) await report({kind: "auth-call", method});
        if (watchedLogoutCall) await report({kind: "logout-rpc-start"});
        try {
          const value = await original.call(this, type, payload, ...rest);
          if (watchedLogoutCall) await report({kind: "logout-rpc-success"});
          return value;
        } catch (error) {
          const errorType = typeof error?.type === "string" ? error.type : error?.error_message;
          if (method === "auth.signIn" && errorType === "SESSION_PASSWORD_NEEDED") {
            await report({kind: "password-challenge"});
          } else if (["auth.sendCode", "auth.signIn", "account.getPassword", "auth.checkPassword"].includes(method)) {
            await report({kind: "rpc-failure", outcome: rpcOutcome(error)});
          }
          if (watchedLogoutCall) await report({kind: "logout-rpc-failure", outcome: rpcOutcome(error)});
          throw error;
        }
      };
      return true;
    }, bindingName);
    if (installed !== true) fail("client-progress-unavailable");
  } catch (error) {
    if (error instanceof AcceptanceFailure) throw error;
    fail("client-progress-unavailable");
  }
  return state;
}

async function waitForVisible(locator, code, timeout = 30000) {
  try {
    await locator.waitFor({state: "visible", timeout});
  } catch {
    fail(code);
  }
  if (!(await visible(locator))) fail(code);
}

async function noCodeOrSignup(page) {
  const codeInput = page.locator('input[autocomplete="one-time-code"]:visible');
  if (await visible(codeInput.first())) fail("code-card-shown");
  const signup = page.getByText(/sign up|create account/iu).filter({visible: true});
  if (await visible(signup.first())) fail("signup-path-shown");
}

async function clearInputWithKeyboard(page, locator) {
  if (!(await visible(locator))) return true;
  try {
    await locator.focus();
    await page.keyboard.press("Control+A");
    await page.keyboard.press("Backspace");
    return true;
  } catch {
    return false;
  }
}

async function logoutThroughApp(prepared, state) {
  let completed = false;
  try {
    completed = await prepared.contextInfo.page.evaluate(async () => {
      const proxy = window.apiManagerProxy;
      if (!proxy || typeof proxy.invoke !== "function") return false;
      try {
        await proxy.invoke("manager", {
          name: "apiManager",
          method: "invokeApi",
          args: ["auth.logOut", {}, {ignoreErrors: true}],
          accountNumber: 1,
        });
        return true;
      } catch {
        return false;
      }
    });
  } catch {
    return {kind: "closed"};
  }
  if (!completed) {
    const outcome = state.logoutRpcFailures[0];
    return outcome ? {kind: "failure", outcome} : {kind: "closed"};
  }
  if (state.logoutRpcCalls !== 1 || state.logoutRpcSuccesses !== 1 || state.logoutRpcFailures.length !== 0) {
    return {kind: "closed"};
  }
  try {
    await prepared.dependencies.closeContext(prepared.contextInfo);
    prepared.contextClosed = true;
    return {kind: "complete"};
  } catch {
    return {kind: "closed"};
  }
}

async function logoutWithProgress(prepared, state, progress) {
  const callback = async (method) => {
    if (method !== "auth.logOut") throw {kind: "closed"};
    const result = await logoutThroughApp(prepared, state);
    if (result.kind === "timeout" || result.kind === "closed") throw {kind: result.kind};
    if (result.kind === "failure") throw rpcErrorForOutcome(result.outcome);
    if (result.kind !== "complete") throw {kind: "closed"};
    return null;
  };
  return validateV2Result(await progress.finish(callback, {logoutTimeoutMs: 65000}), prepared.release.sourceCommit);
}

async function performAcceptance(prepared, credentials) {
  const {contextInfo, progress, manifest, tracker, wssAudit} = prepared;
  const page = contextInfo.page;
  const state = prepared.progressState;
  let signedIn = false;
  let authenticated = false;
  let username = credentials.username;
  let password = credentials.password;
  try {
    const usernameInput = page.locator('input[aria-label="Username"]');
    await waitForVisible(usernameInput, "username-field-missing");
    await noCodeOrSignup(page);
    await usernameInput.focus();
    await page.keyboard.type(username);
    username = "";
    credentials.username = "";
    await page.keyboard.press("Enter");
    if (!(await clearInputWithKeyboard(page, usernameInput))) fail("credential-clear-failed");

    const passwordInput = page.locator('input[name="notsearch_password"]:visible');
    await waitForVisible(passwordInput, "password-field-missing", 60000);
    await noCodeOrSignup(page);
    const passwordEvent = state.waitForAuthFailure(120000);
    await passwordInput.focus();
    await page.keyboard.type(password);
    password = "";
    credentials.password = "";
    await page.keyboard.press("Enter");
    if (!(await clearInputWithKeyboard(page, passwordInput))) fail("credential-clear-failed");

    const chatsPage = page.locator("#page-chats");
    const chatResult = await Promise.race([
      chatsPage.waitFor({state: "visible", timeout: 120000}).then(() => ({kind: "signed-in"})).catch(() => ({kind: "timeout"})),
      passwordEvent,
    ]);
    state.cancelAuthFailureWaiters();
    if (chatResult.kind === "failure") {
      recordRpcFailure(progress, prepared.dependencies.acceptanceRpcOutcome, chatResult.outcome);
      fail("authentication-failed-once");
    }
    if (chatResult.kind !== "signed-in") fail("authentication-failed-once");
    authenticated = true;

    const chatList = page.locator("#chatlist-container .chatlist");
    await waitForVisible(chatList, "broken-chat-list", 30000);
    const chatListContainers = await page.locator("#chatlist-container .chatlist").count();
    const chatRows = await page.locator("#chatlist-container .chatlist-chat").count();
    if (chatListContainers < 1 || !Number.isSafeInteger(chatRows) || chatRows < 0) fail("broken-chat-list");
    assertNoUnsafeNetwork(tracker);
    wssAudit.validate();
    if (tracker.websocketMismatch) fail("endpoint-mismatch");
    signedIn = progress.markSignedIn();
    if (!signedIn) fail("authentication-failed-once");
    return await logoutWithProgress(prepared, state, progress);
  } catch (error) {
    if (error instanceof AcceptanceFailure && error.code === "authentication-failed-once" &&
        state.authFailureOutcome) {
      recordRpcFailure(progress, prepared.dependencies.acceptanceRpcOutcome, state.authFailureOutcome);
    } else if (state.authFailureOutcome) {
      recordRpcFailure(progress, prepared.dependencies.acceptanceRpcOutcome, state.authFailureOutcome);
    } else {
      const outcome = rpcOutcomeForPageError(error);
      if (outcome === "timeout" || outcome === "closed") progress.recordClientFailure({kind: outcome});
    }
    if (authenticated && !signedIn && !prepared.contextClosed) {
      try {
        const logoutResult = await logoutThroughApp(prepared, state);
        if (logoutResult.kind === "failure") {
          recordRpcFailure(progress, prepared.dependencies.acceptanceRpcOutcome, logoutResult.outcome);
        } else if (logoutResult.kind === "timeout" || logoutResult.kind === "closed") {
          progress.recordClientFailure({kind: logoutResult.kind});
        }
      } catch {
        progress.recordClientFailure({kind: "closed"});
      }
    }
    return validateV2Result(await progress.finish(async () => { throw {kind: "closed"}; }), prepared.release.sourceCommit);
  } finally {
    username = "";
    password = "";
    clearCredentials(credentials);
  }
}

function addLogoutWaiters(state) {
  state.authFailureWaiters = [];
  state.waitForAuthFailure = (timeoutMs) => new Promise((resolveWaiter) => {
    if (state.authFailureOutcome) {
      resolveWaiter({kind: "failure", outcome: state.authFailureOutcome});
      return;
    }
    const timer = setTimeout(() => resolveWaiter({kind: "timeout"}), timeoutMs);
    state.authFailureWaiters.push((result) => {
      clearTimeout(timer);
      resolveWaiter(result);
    });
  });
  state.cancelAuthFailureWaiters = () => {
    const waiters = state.authFailureWaiters.splice(0);
    for (const resolveWaiter of waiters) resolveWaiter({kind: "cancelled"});
  };
}

function makeDependencies(options, adapter, playwright) {
  const base = makeProgressDependencies(adapter);
  return {
    ...base,
    async launchContext(policy) {
      return launchContext(playwright, policy);
    },
    async verifySandbox(contextInfo, policy, phase) {
      return adapter.verifySandbox({phase, expectedProxyServer: policy.proxyServer});
    },
    installNetworkGuards,
    readManifestThroughChromium,
    proveWss,
    verifyAssetLoading,
    async installClientProgressObserver(page, progress, classifier) {
      const state = await installClientProgressObserver(page, progress, classifier);
      addLogoutWaiters(state);
      return state;
    },
    async verifyNegativeTargets(policy, release, manifest) {
      await verifyNegativeTargets(playwright, adapter, policy, release, manifest);
    },
    closeContext,
    readCredentials: readCredentialsFromStdin,
    performAcceptance(prepared, credentials) {
      prepared.dependencies = this;
      return performAcceptance(prepared, credentials);
    },
  };
}

async function readCredentialsFromStdin() {
  const chunks = [];
  let size = 0;
  let parsed;
  let buffer;
  try {
    for await (const chunk of process.stdin) {
      size += chunk.length;
      if (size > 16384) fail("credential-input-invalid");
      chunks.push(Buffer.from(chunk));
    }
    buffer = Buffer.concat(chunks);
    parsed = JSON.parse(buffer.toString("utf8"));
    if (!exactKeys(parsed, ["username", "password"]) || typeof parsed.username !== "string" ||
        !/^[a-zA-Z][a-zA-Z0-9_]{4,31}$/u.test(parsed.username) || typeof parsed.password !== "string" ||
        parsed.password.length < 1 || parsed.password.length > 4096) {
      fail("credential-input-invalid");
    }
    return {username: parsed.username, password: parsed.password};
  } catch (error) {
    if (error instanceof AcceptanceFailure) throw error;
    fail("credential-input-invalid");
  } finally {
    buffer?.fill(0);
    for (const chunk of chunks) chunk.fill(0);
    clearCredentials(parsed);
  }
}

function helpText() {
  return "Usage: node private-browser-acceptance-v2.mjs --mode preflight|acceptance --release-record FILE --runtime-adapter MODULE\n";
}

async function main() {
  let options;
  try {
    options = parseArgs(process.argv.slice(2));
    if (options.help) {
      process.stdout.write(helpText());
      return;
    }
  } catch {
    process.stdout.write((requestedPreflight(process.argv.slice(2))
      ? preflightFailureOutput("arguments-invalid") : ACCEPTANCE_FAILED) + "\n");
    process.exitCode = 2;
    return;
  }
  let adapter;
  let playwright;
  let failureCode = "runtime-adapter-unavailable";
  try {
    adapter = await import(pathToFileURL(resolve(options.runtimeAdapter)).href);
    if (typeof adapter.readReleaseRecord !== "function" ||
        typeof adapter.browserLaunchPolicy !== "function" || typeof adapter.verifySandbox !== "function" ||
        typeof adapter.createAcceptanceProgress !== "function" || typeof adapter.acceptanceRpcOutcome !== "function") {
      fail("runtime-adapter-unavailable");
    }
    failureCode = "browser-unavailable";
    playwright = await import("playwright");
    if (!playwright.chromium) fail("browser-unavailable");
  } catch (error) {
    const code = error instanceof AcceptanceFailure ? safeFailureCode(error) : failureCode;
    process.stdout.write((options.mode === "preflight"
      ? preflightFailureOutput(code) : ACCEPTANCE_FAILED) + "\n");
    process.exitCode = 1;
    return;
  }
  const dependencies = makeDependencies(options, adapter, playwright);
  const result = await executeMode(options, dependencies);
  process.stdout.write(result.output + "\n");
  process.exitCode = result.exitCode;
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) {
  main().catch(() => {
    process.stdout.write((requestedPreflight(process.argv.slice(2))
      ? preflightFailureOutput("unexpected-failure") : ACCEPTANCE_FAILED) + "\n");
    process.exitCode = 1;
  });
}
