import { mkdtemp, rm } from "node:fs/promises";
import { statfsSync } from "node:fs";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  ALLOWED_HOST,
  isReadyObserverSnapshot,
  observerSnapshot,
  readReleaseRecord,
  runBrowserNetworkControls,
  runBlockedHostControl,
  verifySandbox,
} from "./qa-runtime-adapter.mjs";

const PROXY_SERVER = "http://browser-observer:3128";
const FAILURES = new Set([
  "arguments-invalid",
  "asset-server-error",
  "blocked-host-control-failed",
  "browser-cleanup-failed",
  "browser-unavailable",
  "direct-egress-open",
  "manifest-fetch-failed",
  "manifest-mismatch",
  "observer-not-ready",
  "observer-not-reset",
  "observer-unhealthy",
  "origin-not-ready",
  "profile-storage-not-tmpfs",
  "release-record-invalid",
  "sandbox-proof-invalid",
  "target-address-invalid",
  "target-address-unavailable",
  "websocket-not-ready",
  "unexpected-failure",
]);

function failure(code, wssDiagnostic) {
  const error = new Error("browser acceptance runtime failed");
  error.code = code;
  if (wssDiagnostic) error.wssDiagnostic = wssDiagnostic;
  return error;
}

function safeCode(error) {
  return typeof error?.code === "string" && FAILURES.has(error.code) ? error.code : "unexpected-failure";
}

function assertTmpfs(path) {
  try {
    if (Number(statfsSync(path).type) === 0x01021994) return;
  } catch {
    // The fixed error below intentionally omits the path and OS details.
  }
  throw failure("profile-storage-not-tmpfs");
}

function assertDebugEnvironment() {
  const blocked = ["DEBUG", "PWDEBUG", "DEBUG_FILE", "PLAYWRIGHT_DISABLE_FORCED_CHROMIUM_PROXIED_LOOPBACK"];
  if (blocked.some((name) => process.env[name] !== undefined)) throw failure("browser-unavailable");
}

function exactKeys(value, keys) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([...keys].sort());
}

function isPlainObject(value) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) return false;
  const prototype = Object.getPrototypeOf(value);
  return prototype === Object.prototype || prototype === null;
}

function isValidWssDiagnostic(value) {
  const fixedDiagnostics = [
    "no-target", "observation-malformed", "untracked-handshake", "worker-unobservable", "worker-pending",
  ];
  const socketDiagnostic = typeof value?.wss_diagnostic === "string" &&
    /^socket-[1-8]-(?:target-mismatch|no-handshake|handshake-not-101|duplicate-handshake)$/u
      .test(value.wss_diagnostic);
  if (!exactKeys(value, [
    "wss_diagnostic", "wss_targets", "wss_handshakes", "wss_target_match", "wss_status",
  ]) || !(fixedDiagnostics.includes(value.wss_diagnostic) || socketDiagnostic) ||
      ![0, 1, 2].includes(value.wss_targets) ||
      ![0, 1, 2].includes(value.wss_handshakes) || typeof value.wss_target_match !== "boolean" ||
      !Number.isInteger(value.wss_status) || (value.wss_status !== 0 &&
        (value.wss_status < 100 || value.wss_status > 599))) {
    return false;
  }

  switch (value.wss_diagnostic) {
    case "observation-malformed":
    case "untracked-handshake":
    case "worker-unobservable":
    case "worker-pending":
      return value.wss_target_match === false && value.wss_status === 0;
    case "no-target":
      return value.wss_targets === 0 && value.wss_handshakes === 0 &&
        value.wss_target_match === false && value.wss_status === 0;
    default:
      if (value.wss_targets === 0) return false;
      if (value.wss_diagnostic.endsWith("-target-mismatch")) {
        return value.wss_target_match === false &&
          (value.wss_status === 0 || (value.wss_handshakes >= 1 && value.wss_status >= 100));
      }
      if (value.wss_diagnostic.endsWith("-no-handshake")) {
        return value.wss_status === 0 && (value.wss_targets !== 1 ||
          (value.wss_handshakes === 0 && value.wss_target_match));
      }
      if (value.wss_diagnostic.endsWith("-handshake-not-101")) {
        return value.wss_handshakes >= 1 && value.wss_status >= 100 && value.wss_status !== 101 &&
          (value.wss_targets !== 1 || value.wss_target_match);
      }
      return value.wss_handshakes === 2 && value.wss_status >= 100 &&
        (value.wss_targets !== 1 || value.wss_target_match);
  }
}

function diagnosticResult(diagnostic, targets, handshakes, targetMatch, status) {
  return {
    wss_diagnostic: diagnostic,
    wss_targets: Math.min(2, targets),
    wss_handshakes: Math.min(2, handshakes),
    wss_target_match: targetMatch,
    wss_status: status,
  };
}

export function createWebSocketObservation() {
  const connections = new Map();
  const pendingHandshakes = new Map();
  let expectedEndpoint = null;
  let handshakeCount = 0;
  let pendingWorkers = 0;
  let malformed = false;
  let workerUnobservable = false;
  let closed = false;

  function hasObservedFailure() {
    if (malformed || workerUnobservable || (closed && pendingWorkers > 0)) return true;
    return [...connections.values()].some((connection) => !connection.match || connection.responses >= 2 ||
      (connection.responses === 1 && connection.status !== 101));
  }

  function diagnostic() {
    const targets = connections.size;
    const handshakes = handshakeCount;
    const entries = [...connections.values()];
    const targetMatch = entries.every((connection) => connection.match);
    if (malformed) return diagnosticResult("observation-malformed", targets, handshakes, false, 0);
    if (pendingHandshakes.size > 0) return diagnosticResult("untracked-handshake", targets, handshakes, false, 0);
    if (workerUnobservable) return diagnosticResult("worker-unobservable", targets, handshakes, false, 0);
    if (closed && pendingWorkers > 0) return diagnosticResult("worker-pending", targets, handshakes, false, 0);
    if (targets === 0) return diagnosticResult("no-target", targets, handshakes, false, 0);

    for (const [index, connection] of entries.entries()) {
      if (!connection.match) {
        const socket = index + 1;
        return diagnosticResult(`socket-${socket}-target-mismatch`, targets, handshakes, targetMatch,
          connection.responses > 0 ? connection.status : 0);
      }
    }
    for (const [index, connection] of entries.entries()) {
      const socket = index + 1;
      if (connection.responses > 1) {
        return diagnosticResult(`socket-${socket}-duplicate-handshake`, targets, handshakes, targetMatch,
          connection.status);
      }
    }
    for (const [index, connection] of entries.entries()) {
      const socket = index + 1;
      if (connection.responses === 1 && connection.status !== 101) {
        return diagnosticResult(`socket-${socket}-handshake-not-101`, targets, handshakes, targetMatch,
          connection.status);
      }
    }
    for (const [index, connection] of entries.entries()) {
      if (connection.responses === 0) {
        return diagnosticResult(`socket-${index + 1}-no-handshake`, targets, handshakes, targetMatch, 0);
      }
    }
    return null;
  }

  return {
    setExpectedEndpoint(value) {
      if (expectedEndpoint !== null || typeof value !== "string" || value.length === 0) {
        malformed = true;
        return;
      }
      expectedEndpoint = value;
    },
    created(params) {
      if (closed) return;
      if (!isPlainObject(params)) {
        malformed = true;
        return;
      }
      if (expectedEndpoint === null) malformed = true;
      const { requestId, url } = params;
      if (typeof requestId !== "string" || !/^[0-9A-Za-z._-]{1,64}$/u.test(requestId)) {
        malformed = true;
        return;
      }
      if (connections.has(requestId)) {
        malformed = true;
        return;
      }
      if (connections.size >= 8) {
        malformed = true;
        return;
      }
      let match = false;
      if (typeof url !== "string") {
        malformed = true;
      } else if (expectedEndpoint !== null) {
        try {
          match = new URL(url).href === expectedEndpoint;
        } catch {
          match = false;
        }
      }
      const connection = { match, responses: 0, status: 0 };
      connections.set(requestId, connection);
      const pending = pendingHandshakes.get(requestId);
      if (pending) {
        connection.responses = pending.responses;
        connection.status = pending.status;
        pendingHandshakes.delete(requestId);
      }
    },
    handshakeResponse(params) {
      if (closed) return;
      if (!isPlainObject(params)) {
        malformed = true;
        return;
      }
      if (expectedEndpoint === null) malformed = true;
      const { requestId, response } = params;
      if (typeof requestId !== "string" || !/^[0-9A-Za-z._-]{1,64}$/u.test(requestId)) {
        malformed = true;
        return;
      }
      const status = response?.status;
      if (!Number.isInteger(status) || status < 100 || status > 599) {
        malformed = true;
        return;
      }
      handshakeCount = Math.min(2, handshakeCount + 1);
      const connection = connections.get(requestId);
      if (!connection) {
        let pending = pendingHandshakes.get(requestId);
        if (!pending) {
          if (pendingHandshakes.size >= 8) {
            malformed = true;
            return;
          }
          pending = { responses: 0, status: 0 };
          pendingHandshakes.set(requestId, pending);
        }
        pending.responses = Math.min(2, pending.responses + 1);
        if (pending.responses === 1) pending.status = status;
        return;
      }
      connection.responses = Math.min(2, connection.responses + 1);
      if (connection.responses === 1) connection.status = status;
    },
    workerUnobservable() {
      if (!closed) workerUnobservable = true;
    },
    workerAttached() {
      if (closed) return () => {};
      pendingWorkers += 1;
      let finished = false;
      return (observable) => {
        if (closed || finished) return;
        finished = true;
        pendingWorkers -= 1;
        if (observable !== true) workerUnobservable = true;
      };
    },
    isSettled() {
      if (closed || hasObservedFailure()) return true;
      return pendingWorkers === 0 && pendingHandshakes.size === 0 && connections.size > 0 &&
        [...connections.values()].every((connection) => connection.responses === 1);
    },
    diagnostic,
    close() {
      closed = true;
    },
  };
}

export function registerWebSocketObservation(session, observation) {
  session.on("Network.webSocketCreated", (params) => observation.created(params));
  session.on("Network.webSocketHandshakeResponseReceived", (params) => observation.handshakeResponse(params));
}

// The web client opens its MTProto socket from a SharedWorker, which is a
// browser-level target the page session never sees. Shared and service
// workers are auto-attached at the browser target, dedicated workers at the
// page target, and every worker's own workers through autoAttachRelated.
// Playwright's CDP sessions cannot address flat child sessions, so worker
// traffic is read through non-flat sessions.
export async function registerWorkerWebSocketObservation(browser, pageSession, observation) {
  const browserSession = await browser.newBrowserCDPSession();
  // Auto-attach for descendants needs its own session: autoAttachRelated replaces
  // a session's setAutoAttach.
  const relatedSession = await browser.newBrowserCDPSession();
  const sessions = new Map();
  const targets = new Set();
  let messageId = 0;

  function receive(event) {
    const target = isPlainObject(event) ? sessions.get(event.sessionId) : undefined;
    if (!target) return;
    let message;
    try {
      message = JSON.parse(event.message);
    } catch {
      message = null;
    }
    if (!isPlainObject(message)) {
      observation.workerUnobservable();
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
      observation.created(message.params);
    } else if (message.method === "Network.webSocketHandshakeResponseReceived") {
      observation.handshakeResponse(message.params);
    }
  }

  function command(target, method) {
    messageId += 1;
    const id = messageId;
    if (method === "Network.enable") target.networkEnableId = id;
    return new Promise((resolve, reject) => {
      target.replies.set(id, { resolve, reject });
      browserSession.send("Target.sendMessageToTarget", {
        sessionId: target.sessionId,
        message: JSON.stringify({ id, method, params: {} }),
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
      observation.workerUnobservable();
      return;
    }
    // Attaching the non-flat session reports this target again; it is already handled.
    if (targets.has(targetId)) return;
    targets.add(targetId);
    const finish = observation.workerAttached();
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
          filter: [{ type: "worker" }],
        });
      }
      const { sessionId } = await browserSession.send("Target.attachToTarget", { targetId, flatten: false });
      let scriptLoaded;
      loadedAfterEnable = new Promise((resolve) => {
        scriptLoaded = resolve;
      });
      const target = { sessionId, replies: new Map(), networkEnableId: 0, networkEnabled: false, scriptLoaded };
      sessions.set(sessionId, target);
      await command(target, "Network.enable");
      await command(target, "Runtime.runIfWaitingForDebugger");
      ready = true;
    } catch {
      // The hold is still released below; the worker counts as unobservable.
    }
    let released = false;
    try {
      await holder.send("Target.detachFromTarget", { sessionId: event.sessionId });
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
    filter: [{ type: "shared_worker" }, { type: "service_worker" }],
  });
  await pageSession.send("Target.setAutoAttach", {
    autoAttach: true,
    waitForDebuggerOnStart: true,
    flatten: true,
    filter: [{ type: "worker" }],
  });
}

export function runtimeErrorOutput(error) {
  const code = safeCode(error);
  const output = { status: "error", code };
  const diagnostic = error?.wssDiagnostic;
  if (code === "websocket-not-ready" && isValidWssDiagnostic(diagnostic)) {
    output.wss_diagnostic = diagnostic.wss_diagnostic;
    output.wss_targets = diagnostic.wss_targets;
    output.wss_handshakes = diagnostic.wss_handshakes;
    output.wss_target_match = diagnostic.wss_target_match;
    output.wss_status = diagnostic.wss_status;
  }
  return output;
}

async function readServedManifest(page, release) {
  const manifestUrl = new URL("mtproto-target.json", release.url);
  manifestUrl.searchParams.set("v", release.sourceCommit);
  let manifest;
  try {
    manifest = await page.evaluate(async (url) => {
      const response = await fetch(url, { cache: "no-store", redirect: "error" });
      if (!response.ok) return null;
      const value = await response.json();
      return {
        keysMatch: JSON.stringify(Object.keys(value).sort()) === JSON.stringify([
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
    throw failure("manifest-fetch-failed");
  }
  if (!manifest || manifest.keysMatch !== true || manifest.mode !== "private" ||
      manifest.sourceCommit !== release.sourceCommit || manifest.artifactDigest !== release.contentDigest ||
      typeof manifest.endpoint !== "string" || typeof manifest.fingerprint !== "string" ||
      !/^[0-9a-f]{16}$/u.test(manifest.fingerprint)) {
    throw failure("manifest-mismatch");
  }
  let endpoint;
  try {
    endpoint = new URL(manifest.endpoint);
  } catch {
    throw failure("manifest-mismatch");
  }
  if (endpoint.protocol !== "wss:" || endpoint.hostname !== ALLOWED_HOST || (endpoint.port && endpoint.port !== "443") ||
      endpoint.username || endpoint.password || endpoint.search || endpoint.hash || endpoint.href !== manifest.endpoint) {
    throw failure("manifest-mismatch");
  }
  return { endpoint: endpoint.href };
}

async function assertSandbox(browser, proxyServer = PROXY_SERVER) {
  const proof = await verifySandbox({ phase: "readiness", expectedProxyServer: proxyServer });
  if (!exactKeys(proof, [
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
  ]) || proof.chromiumSandboxEnabled !== true || !Array.isArray(proof.forbiddenSandboxFlags) ||
      proof.forbiddenSandboxFlags.length !== 0 || proof.observedProxyServer !== proxyServer ||
      proof.observedProxyBypassList !== "<-loopback>" || proof.proxyServerArgCount !== 1 ||
      proof.proxyBypassArgCount !== 1 || !Array.isArray(proof.missingRequiredNetworkArgs) ||
      proof.missingRequiredNetworkArgs.length !== 0 || !Array.isArray(proof.conflictingNetworkArgs) ||
      proof.conflictingNetworkArgs.length !== 0 ||
      ![proof.browserUserNamespaceInode, proof.browserPidNamespaceInode,
        proof.rendererUserNamespaceInode, proof.rendererPidNamespaceInode]
        .every((inode) => Number.isSafeInteger(inode) && inode > 0) ||
      proof.browserUserNamespaceInode === proof.rendererUserNamespaceInode ||
      proof.browserPidNamespaceInode === proof.rendererPidNamespaceInode) {
    throw failure("sandbox-proof-invalid");
  }
  if (!browser.isConnected()) throw failure("sandbox-proof-invalid");
}

export async function readiness(releasePath, { testOnly = {} } = {}) {
  const proxyServer = testOnly.proxyServer ?? PROXY_SERVER;
  const getObserverSnapshot = testOnly.observerSnapshot ?? observerSnapshot;
  assertTmpfs("/dev/shm");
  assertDebugEnvironment();
  const release = await readReleaseRecord(releasePath);
  const before = await getObserverSnapshot();
  if (before.allowed_connects !== 0 || before.blocked_requests !== 0 || before.dns_lookups !== 0 ||
      before.upstream_connects !== 0 || before.upstream_failures !== 0 ||
      before.telegram_attempts !== 0 || before.other_blocked_count !== 0 ||
      before.connect_attempts[ALLOWED_HOST] !== 0 ||
      before.connect_attempts["telegram-suffix"] !== 0 || before.connect_attempts.other !== 0) {
    throw failure("observer-not-reset");
  }

  let playwright;
  try {
    playwright = await import("playwright");
  } catch {
    throw failure("browser-unavailable");
  }
  if (!playwright.chromium) throw failure("browser-unavailable");

  process.env.TMPDIR = "/dev/shm";
  process.env.TMP = "/dev/shm";
  process.env.TEMP = "/dev/shm";
  let profileDir;
  try {
    profileDir = await mkdtemp("/dev/shm/browser-profile-");
  } catch {
    throw failure("profile-storage-not-tmpfs");
  }

  let browser;
  let context;
  let responseStatus = 0;
  let asset502Count = 0;
  const websocket = createWebSocketObservation();
  try {
    const contextOptions = {
      chromiumSandbox: true,
      headless: true,
      proxy: { server: proxyServer, bypass: "" },
      args: [
        "--disable-background-networking",
        "--disable-crash-reporter",
        "--disable-breakpad",
        "--disable-quic",
        "--dns-over-https-mode=off",
        "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
      ],
    };
    if (testOnly.ignoreHTTPSErrors === true) {
      contextOptions.ignoreHTTPSErrors = true;
      // Playwright applies its certificate override per page, which workers do not inherit.
      contextOptions.args.push("--ignore-certificate-errors");
    }
    context = await playwright.chromium.launchPersistentContext(profileDir, contextOptions);
    browser = context.browser();
    const page = await context.newPage();
    const session = await context.newCDPSession(page);
    await session.send("Network.enable");
    registerWebSocketObservation(session, websocket);
    await registerWorkerWebSocketObservation(browser, session, websocket);
    page.on("response", (response) => {
      if (response.status() === 502) asset502Count += 1;
    });
    await assertSandbox(browser, proxyServer);
    let response;
    const manifestUrl = new URL("mtproto-target.json", release.url);
    manifestUrl.searchParams.set("v", release.sourceCommit);
    let manifestResponse;
    try {
      manifestResponse = await page.goto(manifestUrl.href, { waitUntil: "domcontentloaded", timeout: 15_000 });
    } catch {
      throw failure("origin-not-ready");
    }
    if (manifestResponse?.status() !== 200) throw failure("origin-not-ready");
    const expectedEndpoint = (await readServedManifest(page, release)).endpoint;
    websocket.setExpectedEndpoint(expectedEndpoint);

    try {
      response = await page.goto(release.url, { waitUntil: "domcontentloaded", timeout: 15_000 });
    } catch {
      throw failure("origin-not-ready");
    }
    responseStatus = response?.status() ?? 0;
    if (responseStatus !== 200) throw failure("origin-not-ready");

    const startedAt = performance.now();
    const minimumEnd = startedAt + 1_000;
    const deadline = startedAt + 10_000;
    while (performance.now() < minimumEnd) {
      await page.waitForTimeout(Math.min(50, minimumEnd - performance.now()));
    }
    while (!websocket.isSettled() && performance.now() < deadline) {
      await page.waitForTimeout(Math.min(50, deadline - performance.now()));
    }
    if (typeof testOnly.onObservationComplete === "function") {
      testOnly.onObservationComplete(performance.now() - startedAt);
    }
  } catch (error) {
    if (error?.code && FAILURES.has(error.code)) throw error;
    throw failure("browser-unavailable");
  } finally {
    websocket.close();
    try {
      if (context) await context.close();
      await rm(profileDir, { recursive: true, force: true });
    } catch {
      throw failure("browser-cleanup-failed");
    }
  }

  const after = await getObserverSnapshot();
  if (!isReadyObserverSnapshot(after) || after.allowed_connects <= before.allowed_connects ||
      after.connect_attempts[ALLOWED_HOST] <= before.connect_attempts[ALLOWED_HOST] ||
      after.blocked_requests !== 0 || after.telegram_attempts !== 0 || after.other_blocked_count !== 0 ||
      after.upstream_failures !== 0) {
    throw failure("observer-not-ready");
  }
  if (asset502Count !== 0) throw failure("asset-server-error");
  const wssDiagnostic = websocket.diagnostic();
  if (wssDiagnostic) throw failure("websocket-not-ready", wssDiagnostic);

  return {
    status: "ready",
    http_status: responseStatus,
    asset_502_count: asset502Count,
    browser_wss_status: 101,
    browser_wss_unique_targets: 1,
    observer_success_hosts: 1,
    observer_target_host: ALLOWED_HOST,
    telegram_org_attempts: after.telegram_attempts,
    payloads_retained: 0,
  };
}

async function networkSmoke() {
  assertTmpfs("/dev/shm");
  assertDebugEnvironment();
  const observer = await runBrowserNetworkControls();
  let playwright;
  try {
    playwright = await import("playwright");
  } catch {
    throw failure("browser-unavailable");
  }
  if (!playwright.chromium) throw failure("browser-unavailable");
  process.env.TMPDIR = "/dev/shm";
  process.env.TMP = "/dev/shm";
  process.env.TEMP = "/dev/shm";
  let profileDir;
  let context;
  try {
    profileDir = await mkdtemp("/dev/shm/browser-smoke-profile-");
    context = await playwright.chromium.launchPersistentContext(profileDir, {
      chromiumSandbox: true,
      headless: true,
      proxy: { server: PROXY_SERVER, bypass: "" },
      args: [
        "--disable-background-networking",
        "--disable-crash-reporter",
        "--disable-breakpad",
        "--disable-quic",
        "--dns-over-https-mode=off",
        "--force-webrtc-ip-handling-policy=disable_non_proxied_udp",
      ],
    });
    const browser = context.browser();
    await context.newPage();
    await assertSandbox(browser);
  } catch (error) {
    if (error?.code && FAILURES.has(error.code)) throw error;
    throw failure("sandbox-proof-invalid");
  } finally {
    try {
      if (context) await context.close();
      if (profileDir) await rm(profileDir, { recursive: true, force: true });
    } catch {
      throw failure("browser-cleanup-failed");
    }
  }
  return {
    status: "network_smoke_passed",
    blocked_telegram_org_attempts: 1,
    blocked_telegram_org_dns_lookups: 0,
    blocked_telegram_org_upstream_connects: 0,
    direct_tcp_blocked: true,
    chromium_sandbox_proved: true,
    renderer_user_namespace_isolated: true,
    renderer_pid_namespace_isolated: true,
    observer_target_host: observer.allowed_host,
    payloads_retained: 0,
  };
}

function parseArguments(argv) {
  if (argv.length === 1 && argv[0] === "network-smoke") return { mode: "network-smoke" };
  if (argv.length !== 3 || !["readiness", "blocked-control"].includes(argv[0]) || argv[1] !== "--manifest") {
    throw failure("arguments-invalid");
  }
  return { mode: argv[0], manifestPath: argv[2] };
}

async function main() {
  let output;
  try {
    const options = parseArguments(process.argv.slice(2));
    if (options.mode === "network-smoke") {
      output = await networkSmoke();
    } else {
      const { mode, manifestPath } = options;
      await readReleaseRecord(manifestPath);
      if (mode === "blocked-control") {
      await runBlockedHostControl();
      output = {
        status: "blocked_expected",
        blocked_telegram_org_attempts: 1,
        telegram_org_upstream_connects: 0,
        payloads_retained: 0,
      };
      } else {
        output = await readiness(manifestPath);
      }
    }
  } catch (error) {
    output = runtimeErrorOutput(error);
    process.exitCode = 1;
  }
  process.stdout.write(`${JSON.stringify(output)}\n`);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().catch(() => {
    process.stdout.write('{"status":"error","code":"unexpected-failure"}\n');
    process.exitCode = 1;
  });
}
