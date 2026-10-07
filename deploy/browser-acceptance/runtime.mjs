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
  if (!exactKeys(value, [
    "wss_diagnostic", "wss_targets", "wss_handshakes", "wss_target_match", "wss_status",
  ]) || !["ambiguous", "no-target", "target-mismatch", "no-handshake", "handshake-not-101"]
    .includes(value.wss_diagnostic) || ![0, 1, 2].includes(value.wss_targets) ||
      ![0, 1, 2].includes(value.wss_handshakes) || typeof value.wss_target_match !== "boolean" ||
      !Number.isInteger(value.wss_status) || (value.wss_status !== 0 &&
        (value.wss_status < 100 || value.wss_status > 599))) {
    return false;
  }

  switch (value.wss_diagnostic) {
    case "ambiguous":
      return value.wss_target_match === false && value.wss_status === 0;
    case "no-target":
      return value.wss_targets === 0 && value.wss_handshakes === 0 &&
        value.wss_target_match === false && value.wss_status === 0;
    case "target-mismatch":
      return (value.wss_targets === 1 || value.wss_targets === 2) &&
        (value.wss_handshakes === 0 || value.wss_handshakes === value.wss_targets) &&
        value.wss_target_match === false &&
        (value.wss_handshakes === 0 ? value.wss_status === 0 : value.wss_status >= 100);
    case "no-handshake":
      return (value.wss_targets === 1 || value.wss_targets === 2) && value.wss_handshakes === 0 &&
        value.wss_target_match === true && value.wss_status === 0;
    case "handshake-not-101":
      return (value.wss_targets === 1 || value.wss_targets === 2) &&
        value.wss_handshakes === value.wss_targets && value.wss_target_match === true &&
        value.wss_status >= 100 && value.wss_status !== 101;
    default:
      return false;
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
  let expectedEndpoint = null;
  let orphanCount = 0;
  let handshakeCount = 0;
  let malformed = false;
  let closed = false;

  function isAmbiguous() {
    if (malformed || orphanCount > 0) return true;
    const entries = [...connections.values()];
    if (entries.some((connection) => connection.responses >= 2)) return true;
    if (entries.length < 2) return false;
    const tuples = new Set(entries.map((connection) => JSON.stringify([
      connection.match,
      connection.responses === 1,
      connection.responses === 1 ? connection.status : 0,
    ])));
    return tuples.size > 1;
  }

  function diagnostic() {
    const targets = connections.size;
    const handshakes = handshakeCount;
    if (isAmbiguous()) return diagnosticResult("ambiguous", targets, handshakes, false, 0);
    if (targets === 0) return diagnosticResult("no-target", targets, handshakes, false, 0);

    const entries = [...connections.values()];
    const targetMatch = entries.every((connection) => connection.match);
    const allResponded = entries.every((connection) => connection.responses === 1);
    const noneResponded = entries.every((connection) => connection.responses === 0);
    const status = allResponded ? entries[0].status : 0;
    if (!targetMatch) return diagnosticResult("target-mismatch", targets, handshakes, false, status);
    if (noneResponded) return diagnosticResult("no-handshake", targets, handshakes, true, 0);
    if (!allResponded) return diagnosticResult("ambiguous", targets, handshakes, false, 0);
    if (status !== 101) return diagnosticResult("handshake-not-101", targets, handshakes, true, status);
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
      connections.set(requestId, { match, responses: 0, status: 0 });
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
        orphanCount = Math.min(2, orphanCount + 1);
        return;
      }
      connection.responses = Math.min(2, connection.responses + 1);
      if (connection.responses === 1) connection.status = status;
    },
    isSettled() {
      if (closed || isAmbiguous()) return true;
      return connections.size > 0 && [...connections.values()].every((connection) => connection.responses === 1);
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
      before.telegram_attempts !== 0 || before.other_blocked_count !== 0) {
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
    if (testOnly.ignoreHTTPSErrors === true) contextOptions.ignoreHTTPSErrors = true;
    context = await playwright.chromium.launchPersistentContext(profileDir, contextOptions);
    browser = context.browser();
    const page = await context.newPage();
    const session = await context.newCDPSession(page);
    await session.send("Network.enable");
    registerWebSocketObservation(session, websocket);
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
