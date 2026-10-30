import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import https from "node:https";
import { inspect } from "node:util";
import { pathToFileURL } from "node:url";
import { connect } from "node:net";

import { createConnectObserver } from "./connect-observer.mjs";
import { createAcceptanceProgress } from "./acceptance-result.mjs";
import { readiness, runtimeErrorOutput } from "./runtime.mjs";
import { ALLOWED_HOST, isReadyObserverSnapshot } from "./qa-runtime-adapter.mjs";

let currentScenario = "setup";

const SOURCE_COMMIT = "e".repeat(40);
const CONTENT_DIGEST = `sha256:${"f".repeat(64)}`;
const URL_CANARY = "wss-diagnostic-canary";
const HEADER_CANARY = "wss-response-header-canary";
const COOKIE_CANARY = "wss-set-cookie-canary";
// The page's timer starts while navigation is still in flight, so the delayed
// scenario is timed from the app request the timer follows, not from when
// Playwright's navigation resolves.
const DELAYED_WSS_TIMER_MS = 5_000;
const MINIMUM_OBSERVATION_MS = 1_000;
const READY_LINE = "{\"status\":\"ready\",\"http_status\":200,\"asset_502_count\":0,\"browser_wss_status\":101,\"browser_wss_unique_targets\":1,\"observer_success_hosts\":1,\"observer_target_host\":\"teagram.tailaa4918.ts.net\",\"telegram_org_attempts\":0,\"payloads_retained\":0}";

function listen(server) {
  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
}

function trackConnections(server) {
  const sockets = new Set();
  server.on("connection", (socket) => {
    sockets.add(socket);
    socket.once("close", () => sockets.delete(socket));
  });
  return async () => {
    for (const socket of sockets) socket.destroy();
    if (!server.listening) return;
    server.closeAllConnections?.();
    await new Promise((resolve, reject) => {
      server.close((error) => error ? reject(error) : resolve());
    });
  };
}

function jsonResponse(response, status, value) {
  const body = JSON.stringify(value);
  response.writeHead(status, {
    "cache-control": "no-store",
    "content-length": String(Buffer.byteLength(body)),
    "content-type": "application/json",
  });
  response.end(body);
}

function rejectUpgrade(socket, status = 503) {
  const reason = status === 404 ? "Not Found" : "Service Unavailable";
  socket.end([
    `HTTP/1.1 ${status} ${reason}`,
    "Connection: close",
    "Content-Length: 0",
    `X-Response-Canary: ${HEADER_CANARY}`,
    `Set-Cookie: diagnostic=${COOKIE_CANARY}; HttpOnly; Secure`,
    "\r\n",
  ].join("\r\n"));
}

async function createFixture(directory, scenario, certificate) {
  const endpoint = `wss://${ALLOWED_HOST}/apiws`;
  const manifest = {
    mode: "private",
    endpoint,
    fingerprint: "0123456789abcdef",
    sourceCommit: SOURCE_COMMIT,
    artifactDigest: scenario.startsWith("manifest-mismatch") ? `sha256:${"0".repeat(64)}` : CONTENT_DIGEST,
  };
  const releasePath = `${directory}/${scenario}.json`;
  const release = {
    sourceCommit: SOURCE_COMMIT,
    archiveSha256: CONTENT_DIGEST,
    contentDigest: CONTENT_DIGEST,
    url: `https://${ALLOWED_HOST}/client/?v=${SOURCE_COMMIT}`,
  };
  await writeFile(releasePath, JSON.stringify(release), { mode: 0o600 });

  let appRequests = 0;
  let appRequestedAt;
  let upgrades = 0;
  let firstUpgradeAt;
  const origin = https.createServer(certificate, (request, response) => {
    const pathname = new URL(request.url, `https://${ALLOWED_HOST}`).pathname;
    if (pathname === "/client/" || pathname === "/client") {
      appRequests += 1;
      appRequestedAt ??= performance.now();
      if (scenario === "manifest-mismatch-app-404") {
        response.writeHead(404, { connection: "close", "content-length": "0" });
        response.end();
        return;
      }
      const script = (() => {
        if (["missing-wss", "manifest-mismatch", "manifest-mismatch-app-404"].includes(scenario)) return "";
        if (scenario === "private-client-worker") {
          const entryUrl = `https://${ALLOWED_HOST}/client/worker.js`;
          const bootstrap = [
            "const pendingConnects=[];",
            "const holdConnect=(event)=>pendingConnects.push(event);",
            "globalThis.addEventListener('connect',holdConnect);",
            `try{await import(${JSON.stringify(entryUrl)});}finally{globalThis.removeEventListener('connect',holdConnect);}`,
            "for(const event of pendingConnects){const source=event.ports?.[0]||event.source;globalThis.dispatchEvent(new MessageEvent('connect',{data:event.data,source}));}",
          ].join("");
          return `<script>const source=${JSON.stringify(bootstrap)};const url=URL.createObjectURL(new Blob([source],{type:"application/javascript"}));new SharedWorker(url,{type:"module"}).port.postMessage("request");</script>`;
        }
        if (scenario === "shared-worker") {
          // The private web client starts its MTProto SharedWorker from a blob URL.
          return "<script>fetch(\"/client/worker.js\").then((response) => response.blob()).then((blob) => { new SharedWorker(URL.createObjectURL(blob), { type: \"module\" }).port.postMessage(\"request\"); });</script>";
        }
        if (scenario === "dedicated-worker") {
          // The web client falls back to a dedicated worker where SharedWorker is unavailable.
          return "<script>fetch(\"/client/worker.js\").then((response) => response.blob()).then((blob) => { new Worker(URL.createObjectURL(blob), { type: \"module\" }); });</script>";
        }
        if (scenario === "nested-worker") return "<script>new Worker(\"/client/outer.js\");</script>";
        if (scenario === "nested-worker-mismatch" || scenario === "nested-worker-mismatch-delayed") {
          return `<script>const socket = new WebSocket(${JSON.stringify(endpoint)}); socket.onerror = () => {}; ${scenario.endsWith("-delayed") ? 'setTimeout(() => new Worker("/client/outer.js"), 250);' : 'new Worker("/client/outer.js");'}</script>`;
        }
        if (scenario.startsWith("shared-worker-")) {
          return "<script>new SharedWorker(\"/client/worker.js\", { type: \"module\" }).port.postMessage(\"request\");</script>";
        }
        if (scenario === "delayed-wss") {
          return `<script>setTimeout(() => { const socket = new WebSocket(${JSON.stringify(endpoint)}); socket.onerror = () => {}; }, ${DELAYED_WSS_TIMER_MS});</script>`;
        }
        let targets = [endpoint];
        if (scenario === "path-mismatch") targets = [`${endpoint}-${URL_CANARY}`];
        if (scenario === "query-mismatch") targets = [`${endpoint}?c=${URL_CANARY}`];
        if (scenario === "multiple-success" || scenario === "mixed-status") targets = [endpoint, endpoint];
        if (scenario === "mixed-target") targets = [endpoint, `${endpoint}-${URL_CANARY}`];
        return `<script>for (const url of ${JSON.stringify(targets)}) { const socket = new WebSocket(url); socket.onerror = () => {}; }</script>`;
      })();
      const html = [
        "<!doctype html><html><head>",
        '<link rel="stylesheet" href="/client/app.css">',
        "</head><body>",
        "<main>credential-free browser readiness fixture</main>",
        script,
        "</body></html>",
      ].join("");
      response.writeHead(200, {
        "cache-control": "no-store",
        "content-length": String(Buffer.byteLength(html)),
        "content-type": "text/html; charset=utf-8",
      });
      response.end(html);
      return;
    }
    if (pathname === "/client/outer.js") {
      const body = "new Worker(\"/client/worker.js\");";
      response.writeHead(200, {
        "cache-control": "no-store",
        "content-length": String(Buffer.byteLength(body)),
        "content-type": "text/javascript",
      });
      response.end(body);
      return;
    }
    if (pathname === "/client/worker.js" || pathname === "/client/worker-chunk.js") {
      const target = ["shared-worker-mismatch", "nested-worker-mismatch", "nested-worker-mismatch-delayed"].includes(scenario)
        ? `${endpoint}-${URL_CANARY}`
        : endpoint;
      const socket = scenario === "shared-worker-silent"
        ? ""
        : `const socket = new WebSocket(${JSON.stringify(target)}); socket.onerror = () => {};`;
      // Like the client, a SharedWorker's entry imports its code by absolute URL and
      // opens its socket for the page's first request. Playwright resumes shared
      // workers itself, so only dedicated workers are held from their first statement.
      let body = socket;
      if (scenario === "private-client-worker") {
        body = `self.addEventListener("connect",(event)=>{const port=event.ports?.[0]||event.source;port.onmessage=()=>{for(const url of ${JSON.stringify(Array(4).fill(endpoint))}){const socket=new WebSocket(url);socket.onerror=()=>{};}};});`;
      } else if (scenario.startsWith("shared-worker")) {
        body = pathname === "/client/worker.js"
          ? `import ${JSON.stringify(`https://${ALLOWED_HOST}/client/worker-chunk.js`)};`
          : `self.onconnect = (event) => { event.ports[0].onmessage = () => { ${socket} }; };`;
      }
      response.writeHead(200, {
        "cache-control": "no-store",
        "content-length": String(Buffer.byteLength(body)),
        "content-type": "text/javascript",
      });
      response.end(body);
      return;
    }
    if (pathname === "/client/mtproto-target.json") {
      jsonResponse(response, 200, manifest);
      return;
    }
    if (pathname === "/client/app.css") {
      if (scenario === "asset-502") {
        response.writeHead(502, { connection: "close", "content-length": "0" });
        response.end();
        return;
      }
      response.writeHead(200, { "content-length": "0", "content-type": "text/css" });
      response.end();
      return;
    }
    response.writeHead(404, { connection: "close", "content-length": "0" });
    response.end();
  });
  origin.on("upgrade", (request, socket) => {
    upgrades += 1;
    firstUpgradeAt ??= performance.now();
    if (scenario === "held-wss") return;
    if (scenario === "destroyed-wss") {
      socket.destroy();
      return;
    }
    if (scenario === "failed-wss" || (scenario === "mixed-status" && upgrades === 2)) {
      rejectUpgrade(socket);
      return;
    }
    const allowMismatchedTarget = [
      "path-mismatch", "query-mismatch", "mixed-target", "shared-worker-mismatch", "nested-worker-mismatch",
      "nested-worker-mismatch-delayed",
    ].includes(scenario);
    if (request.url !== "/apiws" && !allowMismatchedTarget) {
      rejectUpgrade(socket);
      return;
    }
    const key = request.headers["sec-websocket-key"];
    if (typeof key !== "string") {
      socket.destroy();
      return;
    }
    const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
    socket.write([
      "HTTP/1.1 101 Switching Protocols",
      "Upgrade: websocket",
      "Connection: Upgrade",
      `Sec-WebSocket-Accept: ${accept}`,
      "\r\n",
    ].join("\r\n"));
  });
  const closeOrigin = trackConnections(origin);
  await listen(origin);

  const observer = createConnectObserver({
    manifestHost: ALLOWED_HOST,
    resolveHost: async () => [{ address: "100.64.0.7", family: 4 }],
    // Keep the observer's upstream inside this fixture while exercising its pinned-host checks.
    openUpstream: () => connect({ host: "127.0.0.1", port: origin.address().port }),
  });
  const closeObserver = trackConnections(observer.server);
  await listen(observer.server);

  return {
    observer,
    origin,
    closeObserver,
    closeOrigin,
    proxyServer: `http://127.0.0.1:${observer.server.address().port}`,
    releasePath,
    release,
    manifest,
    appRequests: () => appRequests,
    upgrades: () => upgrades,
    appRequestedAt: () => appRequestedAt,
    firstUpgradeAt: () => firstUpgradeAt,
  };
}

async function runScenario(directory, scenario, certificate) {
  currentScenario = scenario;
  const fixture = await createFixture(directory, scenario, certificate);
  let observationMs;
  let observationStartedAt;
  try {
    const result = await readiness(fixture.releasePath, {
      testOnly: {
        ignoreHTTPSErrors: true,
        onObservationComplete: (elapsed) => {
          observationMs = elapsed;
          observationStartedAt = performance.now() - elapsed;
        },
        observerSnapshot: async () => fixture.observer.snapshot(),
        proxyServer: fixture.proxyServer,
      },
    });
    return {
      result,
      snapshot: fixture.observer.snapshot(),
      observationMs,
      observationStartedAt,
      appRequestedAt: fixture.appRequestedAt(),
      firstUpgradeAt: fixture.firstUpgradeAt(),
      appRequests: fixture.appRequests(),
      upgrades: fixture.upgrades(),
    };
  } catch (error) {
    return {
      error: { code: error?.code ?? "unexpected-failure", diagnostic: error?.wssDiagnostic },
      snapshot: fixture.observer.snapshot(),
      observationMs,
      appRequests: fixture.appRequests(),
      upgrades: fixture.upgrades(),
    };
  } finally {
    await fixture.closeObserver();
    await fixture.closeOrigin();
  }
}

export async function captureQaWssProof(proveWss, ...args) {
  try {
    const audit = await proveWss(...args);
    return { result: { connectionCount: audit.connectionCount, status101: audit.status101 } };
  } catch (error) {
    return {
      error: {
        code: error?.code ?? "unexpected-failure",
        message: error?.message ?? "unknown error",
        diagnostic: error?.wssDiagnostic,
      },
    };
  }
}

export function assertQaWssProof(proof, expected, label) {
  if (proof.error) {
    const message = typeof proof.error.message === "string"
      ? proof.error.message.split(/[\r\n]/u, 1)[0]
      : "unknown error";
    const connection = proof.error.diagnostic?.wss_diagnostic;
    const location = typeof connection === "string" ? `; ${connection}` : "";
    throw new Error(`${label}.proveWss failed: ${message} (${proof.error.code}${location})`);
  }
  assert.deepEqual(proof.result, expected, `${label}.result`);
}

async function runQaScenario(directory, scenario, certificate, qa) {
  currentScenario = `qa-${scenario}`;
  const fixture = await createFixture(directory, scenario, certificate);
  const { chromium } = await import("playwright");
  const profileDir = await mkdtemp(`${directory}/qa-profile-`);
  let context;
  try {
    context = await chromium.launchPersistentContext(profileDir, {
      chromiumSandbox: true,
      headless: true,
      ignoreHTTPSErrors: true,
      proxy: { server: fixture.proxyServer, bypass: "" },
      // Playwright applies its certificate override per page, which workers do not inherit.
      args: ["--ignore-certificate-errors"],
    });
    const contextInfo = { context, page: context.pages()[0] ?? await context.newPage(), manifest: null };
    const tracker = await qa.installNetworkGuards(contextInfo, fixture.release, null);
    const progress = createAcceptanceProgress({ sourceCommit: SOURCE_COMMIT });
    const manifest = { endpoint: fixture.manifest.endpoint, fingerprint: fixture.manifest.fingerprint };
    const proof = await captureQaWssProof(
      (...args) => qa.proveWss(...args), contextInfo, fixture.release, manifest, progress, tracker,
    );
    return { ...proof, upgrades: fixture.upgrades() };
  } catch (error) {
    return { error: { code: error?.code ?? "unexpected-failure" }, upgrades: fixture.upgrades() };
  } finally {
    await context?.close();
    await rm(profileDir, { recursive: true, force: true });
    await fixture.closeObserver();
    await fixture.closeOrigin();
  }
}

function errorLine(result) {
  const line = JSON.stringify(runtimeErrorOutput({
    code: result.error.code,
    wssDiagnostic: result.error.diagnostic,
  }));
  const output = JSON.parse(line);
  assert.equal(output.status, "error", "output.status");
  assert.deepEqual(Object.keys(output), output.code === "websocket-not-ready"
    ? ["status", "code", "wss_diagnostic", "wss_targets", "wss_handshakes", "wss_target_match", "wss_status"]
    : ["status", "code"], "Object.keys(output)");
  assertNoDiagnosticLeaks(line);
  return line;
}

function assertNoDiagnosticLeaks(line) {
  assert.equal(line.includes("/"), false, "line.includes(\"/\")");
  assert.doesNotMatch(line, /canary|ts\.net|header|set-cookie|raw error|payload|credential/iu, "line");
}

function assertWssError(result, expected) {
  assert.equal(result.error?.code, "websocket-not-ready", "result.error?.code");
  assert.deepEqual(result.error.diagnostic, expected, "result.error.diagnostic");
  const line = errorLine(result);
  assertNoDiagnosticLeaks(line);
  return JSON.parse(line);
}

export function assertSocketTargetMismatch(result, socket, targets) {
  assert.equal(result.error?.code, "websocket-not-ready", "result.error?.code");
  const diagnostic = result.error.diagnostic;
  assert.equal(diagnostic.wss_diagnostic, `socket-${socket}-target-mismatch`, "wss_diagnostic");
  assert.equal(diagnostic.wss_targets, targets, "wss_targets");
  assert.equal(diagnostic.wss_target_match, false, "wss_target_match");
  assert.ok(diagnostic.wss_status === 0 || (diagnostic.wss_status >= 100 && diagnostic.wss_status <= 599),
    "wss_status is zero or an observed response status");
  if (diagnostic.wss_status !== 0) assert.ok(diagnostic.wss_handshakes >= 1, "wss_handshakes includes the response");
  const line = errorLine(result);
  assertNoDiagnosticLeaks(line);
  return JSON.parse(line);
}

export function assertMixedStatusError(result) {
  assert.equal(result.error?.code, "websocket-not-ready", "mixedStatus.error?.code");
  const diagnostic = result.error.diagnostic;
  assert.equal(diagnostic.wss_targets, 2, "mixedStatus.wss_targets");
  assert.equal(diagnostic.wss_target_match, true, "mixedStatus.wss_target_match");
  const failure = /^socket-([12])-(no-handshake|handshake-not-101)$/u.exec(diagnostic.wss_diagnostic);
  assert.ok(failure, `mixedStatus.wss_diagnostic is a failed socket among two targets (observed ${diagnostic.wss_diagnostic})`);
  if (failure[2] === "no-handshake") {
    assert.equal(diagnostic.wss_handshakes, 1, "mixedStatus.wss_handshakes");
    assert.equal(diagnostic.wss_status, 0, "mixedStatus.wss_status");
  } else {
    assert.equal(diagnostic.wss_handshakes, 2, "mixedStatus.wss_handshakes");
    assert.equal(diagnostic.wss_status, 503, "mixedStatus.wss_status");
  }
  const line = errorLine(result);
  assertNoDiagnosticLeaks(line);
  return JSON.parse(line);
}

export function assertSilentSharedWorkerError(result) {
  const isUnobservable = result.error?.diagnostic?.wss_diagnostic === "worker-unobservable";
  return assertWssError(result, {
    wss_diagnostic: isUnobservable ? "worker-unobservable" : "no-target",
    wss_targets: 0,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  });
}

async function main() {
  process.umask(0o077);
  // The QA runner is mounted beside this smoke at its approved path, never copied into the image.
  const qa = await import(new URL(process.argv[2] ?? "./approved-qa.mjs", import.meta.url).href);
  const directory = await mkdtemp("/dev/shm/browser-readiness-integration-");
  try {
    const keyPath = `${directory}/fixture-key.pem`;
    const certPath = `${directory}/fixture-cert.pem`;
    const generated = spawnSync("openssl", [
      "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
      "-keyout", keyPath, "-out", certPath,
      "-subj", `/CN=${ALLOWED_HOST}`,
      "-addext", `subjectAltName=DNS:${ALLOWED_HOST}`,
    ], { encoding: "utf8", stdio: "ignore" });
    if (generated.error || generated.status !== 0) throw new Error("fixture certificate generation failed");
    await chmod(keyPath, 0o600);
    const certificate = { key: await readFile(keyPath), cert: await readFile(certPath) };

    const positive = await runScenario(directory, "positive", certificate);
    assert.equal(positive.result?.status, "ready", `positive.result?.status (${positive.error?.code ?? "no error code"})`);
    assert.equal(positive.result.http_status, 200, "positive.result.http_status");
    assert.equal(positive.result.browser_wss_status, 101, "positive.result.browser_wss_status");
    assert.equal(JSON.stringify(positive.result), READY_LINE, "JSON.stringify(positive.result)");
    assert.ok(positive.snapshot.allowed_connects > 0, "positive.snapshot.allowed_connects > 0");
    assert.ok(isReadyObserverSnapshot(positive.snapshot), "isReadyObserverSnapshot(positive.snapshot)");

    const delayed = await runScenario(directory, "delayed-wss", certificate);
    assert.equal(delayed.result?.status, "ready", "delayed.result?.status");
    const delayedWssMs = delayed.firstUpgradeAt - delayed.appRequestedAt;
    assert.ok(delayedWssMs >= 3_000, `delayedWssMs >= 3_000 (observed ${delayedWssMs} ms)`);
    assert.ok(delayedWssMs < 10_000, `delayedWssMs < 10_000 (observed ${delayedWssMs} ms)`);
    assert.ok(delayed.firstUpgradeAt - delayed.observationStartedAt >= MINIMUM_OBSERVATION_MS, `delayed.firstUpgradeAt - delayed.observationStartedAt >= MINIMUM_OBSERVATION_MS (observed ${delayed.firstUpgradeAt - delayed.observationStartedAt} ms)`);
    assert.ok(delayed.observationMs < 10_000, `delayed.observationMs < 10_000 (observed ${delayed.observationMs} ms)`);
    assert.equal(JSON.stringify(delayed.result), READY_LINE, "JSON.stringify(delayed.result)");

    const multipleSuccess = await runScenario(directory, "multiple-success", certificate);
    assert.equal(multipleSuccess.result?.status, "ready", "multipleSuccess.result?.status");
    assert.equal(JSON.stringify(multipleSuccess.result), READY_LINE, "JSON.stringify(multipleSuccess.result)");

    const manifestMismatch = await runScenario(directory, "manifest-mismatch", certificate);
    assert.equal(manifestMismatch.error?.code, "manifest-mismatch", "manifestMismatch.error?.code");
    assertNoDiagnosticLeaks(errorLine(manifestMismatch));

    const manifestBeforeApp = await runScenario(directory, "manifest-mismatch-app-404", certificate);
    assert.equal(manifestBeforeApp.error?.code, "manifest-mismatch", "manifestBeforeApp.error?.code");
    assert.equal(manifestBeforeApp.appRequests, 0, "manifestBeforeApp.appRequests");
    assertNoDiagnosticLeaks(errorLine(manifestBeforeApp));

    const asset502 = await runScenario(directory, "asset-502", certificate);
    assert.equal(asset502.error?.code, "asset-server-error", "asset502.error?.code");
    assertNoDiagnosticLeaks(errorLine(asset502));

    const missingWss = await runScenario(directory, "missing-wss", certificate);
    const noTarget = assertWssError(missingWss, {
      wss_diagnostic: "no-target",
      wss_targets: 0,
      wss_handshakes: 0,
      wss_target_match: false,
      wss_status: 0,
    });
    assert.ok(missingWss.observationMs >= 10_000, `missingWss.observationMs >= 10_000 (observed ${missingWss.observationMs} ms)`);
    assert.ok(missingWss.observationMs < 15_000, `missingWss.observationMs < 15_000 (observed ${missingWss.observationMs} ms)`);

    const failedWss = await runScenario(directory, "failed-wss", certificate);
    assert.equal(failedWss.error?.code, "websocket-not-ready", "failedWss.error?.code");
    assert.equal(failedWss.upgrades, 1, "failedWss.upgrades");
    const failedWssLine = errorLine(failedWss);
    const noHandshakeLine = "{\"status\":\"error\",\"code\":\"websocket-not-ready\",\"wss_diagnostic\":\"socket-1-no-handshake\",\"wss_targets\":1,\"wss_handshakes\":0,\"wss_target_match\":true,\"wss_status\":0}";
    const handshakeNot101Line = "{\"status\":\"error\",\"code\":\"websocket-not-ready\",\"wss_diagnostic\":\"socket-1-handshake-not-101\",\"wss_targets\":1,\"wss_handshakes\":1,\"wss_target_match\":true,\"wss_status\":503}";
    assert.ok([noHandshakeLine, handshakeNot101Line].includes(failedWssLine), `failedWssLine must equal ${noHandshakeLine} or ${handshakeNot101Line}; observed ${failedWssLine}`);
    const failedWssDiagnostic = JSON.parse(failedWssLine);
    const failedWssHandshakeEventAvailable = failedWssDiagnostic.wss_handshakes === 1;
    assert.equal(failedWssHandshakeEventAvailable,
      failedWssDiagnostic.wss_diagnostic === "socket-1-handshake-not-101", "failedWssHandshakeEventAvailable");
    const architecture = process.arch === "arm64" ? "arm64" : process.arch === "x64" ? "amd64" : "unknown";
    if (architecture === "arm64") assert.equal(failedWssLine, noHandshakeLine, "failedWssLine");

    const pathMismatch = await runScenario(directory, "path-mismatch", certificate);
    const pathMismatchDiagnostic = assertWssError(pathMismatch, {
      wss_diagnostic: "socket-1-target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });

    const queryMismatch = await runScenario(directory, "query-mismatch", certificate);
    const queryMismatchDiagnostic = assertWssError(queryMismatch, {
      wss_diagnostic: "socket-1-target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });

    const heldWss = await runScenario(directory, "held-wss", certificate);
    const heldWssDiagnostic = assertWssError(heldWss, {
      wss_diagnostic: "socket-1-no-handshake",
      wss_targets: 1,
      wss_handshakes: 0,
      wss_target_match: true,
      wss_status: 0,
    });

    const destroyedWss = await runScenario(directory, "destroyed-wss", certificate);
    const destroyedWssDiagnostic = assertWssError(destroyedWss, {
      wss_diagnostic: "socket-1-no-handshake",
      wss_targets: 1,
      wss_handshakes: 0,
      wss_target_match: true,
      wss_status: 0,
    });

    const mixedStatus = await runScenario(directory, "mixed-status", certificate);
    assert.equal(mixedStatus.upgrades, 2, "mixedStatus.upgrades");
    const mixedStatusDiagnostic = assertMixedStatusError(mixedStatus);

    const mixedTarget = await runScenario(directory, "mixed-target", certificate);
    const mixedTargetDiagnostic = assertSocketTargetMismatch(mixedTarget, 2, 2);

    const sharedWorker = await runScenario(directory, "shared-worker", certificate);
    assert.equal(sharedWorker.result?.status, "ready", "sharedWorker.result?.status");
    assert.equal(JSON.stringify(sharedWorker.result), READY_LINE, "JSON.stringify(sharedWorker.result)");
    assert.equal(sharedWorker.upgrades, 1, "sharedWorker.upgrades");

    const dedicatedWorker = await runScenario(directory, "dedicated-worker", certificate);
    assert.equal(dedicatedWorker.result?.status, "ready", "dedicatedWorker.result?.status");
    assert.equal(JSON.stringify(dedicatedWorker.result), READY_LINE, "JSON.stringify(dedicatedWorker.result)");
    assert.equal(dedicatedWorker.upgrades, 1, "dedicatedWorker.upgrades");

    const nestedWorker = await runScenario(directory, "nested-worker", certificate);
    assert.equal(nestedWorker.result?.status, "ready", "nestedWorker.result?.status");
    assert.equal(JSON.stringify(nestedWorker.result), READY_LINE, "JSON.stringify(nestedWorker.result)");
    assert.equal(nestedWorker.upgrades, 1, "nestedWorker.upgrades");

    const nestedWorkerMismatch = await runScenario(directory, "nested-worker-mismatch", certificate);
    const nestedWorkerMismatchDiagnostic = assertSocketTargetMismatch(nestedWorkerMismatch, 2, 2);

    const sharedWorkerMismatch = await runScenario(directory, "shared-worker-mismatch", certificate);
    const sharedWorkerMismatchDiagnostic = assertWssError(sharedWorkerMismatch, {
      wss_diagnostic: "socket-1-target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });

    const sharedWorkerSilent = await runScenario(directory, "shared-worker-silent", certificate);
    const sharedWorkerSilentDiagnostic = assertSilentSharedWorkerError(sharedWorkerSilent);

    const qaSharedWorker = await runQaScenario(directory, "shared-worker", certificate, qa);
    assertQaWssProof(qaSharedWorker, { connectionCount: 1, status101: true }, "qaSharedWorker");
    const qaPrivateClientWorker = await runQaScenario(directory, "private-client-worker", certificate, qa);
    assertQaWssProof(qaPrivateClientWorker, { connectionCount: 4, status101: true }, "qaPrivateClientWorker");
    assert.equal(qaPrivateClientWorker.upgrades, 4, "qaPrivateClientWorker.upgrades");
    const qaDedicatedWorker = await runQaScenario(directory, "dedicated-worker", certificate, qa);
    assertQaWssProof(qaDedicatedWorker, { connectionCount: 1, status101: true }, "qaDedicatedWorker");
    const qaNestedWorker = await runQaScenario(directory, "nested-worker", certificate, qa);
    assertQaWssProof(qaNestedWorker, { connectionCount: 1, status101: true }, "qaNestedWorker");
    const qaNestedWorkerMismatch = await runQaScenario(directory, "nested-worker-mismatch", certificate, qa);
    assert.equal(qaNestedWorkerMismatch.error?.code, "websocket-not-ready", "qaNestedWorkerMismatch.error?.code");
    assert.equal(qaNestedWorkerMismatch.upgrades, 2, "qaNestedWorkerMismatch.upgrades");
    const qaDelayedNestedWorkerMismatch = await runQaScenario(directory, "nested-worker-mismatch-delayed", certificate, qa);
    assert.equal(qaDelayedNestedWorkerMismatch.error?.code, "websocket-not-ready", "qaDelayedNestedWorkerMismatch.error?.code");
    assert.equal(qaDelayedNestedWorkerMismatch.upgrades, 2, "qaDelayedNestedWorkerMismatch.upgrades");
    const qaSharedWorkerMismatch = await runQaScenario(directory, "shared-worker-mismatch", certificate, qa);
    assert.equal(qaSharedWorkerMismatch.error?.code, "websocket-not-ready", "qaSharedWorkerMismatch.error?.code");
    assert.equal(qaSharedWorkerMismatch.upgrades, 1, "qaSharedWorkerMismatch.upgrades");
    const qaSharedWorkerSilent = await runQaScenario(directory, "shared-worker-silent", certificate, qa);
    assert.equal(qaSharedWorkerSilent.error?.code, "websocket-not-ready", "qaSharedWorkerSilent.error?.code");

    process.stdout.write(`${JSON.stringify({
      status: "readiness_integration_passed",
      architecture,
      positive: {
        status: positive.result.status,
        http_status: positive.result.http_status,
        browser_wss_status: positive.result.browser_wss_status,
        observer_allowed_connects: positive.snapshot.allowed_connects,
        observer_blocked_requests: positive.snapshot.blocked_requests,
      },
      delayed_wss_observation_ms: delayedWssMs,
      multiple_matching_connections: "ready",
      manifest_mismatch: manifestMismatch.error.code,
      manifest_before_app_mismatch: manifestBeforeApp.error.code,
      asset_502: asset502.error.code,
      no_target: noTarget,
      no_target_observation_ms: missingWss.observationMs,
      failed_wss_associated_handshake_event: failedWssHandshakeEventAvailable,
      failed_wss_error_line: failedWssLine,
      path_mismatch: pathMismatchDiagnostic,
      query_mismatch: queryMismatchDiagnostic,
      no_handshake_held: heldWssDiagnostic,
      no_handshake_destroyed: destroyedWssDiagnostic,
      mixed_status: mixedStatusDiagnostic,
      mixed_target: mixedTargetDiagnostic,
      shared_worker: sharedWorker.result.status,
      dedicated_worker: dedicatedWorker.result.status,
      nested_worker: nestedWorker.result.status,
      nested_worker_mismatch: nestedWorkerMismatchDiagnostic,
      shared_worker_mismatch: sharedWorkerMismatchDiagnostic,
      shared_worker_silent: sharedWorkerSilentDiagnostic,
      qa_shared_worker: qaSharedWorker.result.connectionCount === 1 ? "ready" : "unexpected",
      qa_private_client_worker: qaPrivateClientWorker.result.connectionCount === 4 ? "ready" : "unexpected",
      qa_dedicated_worker: qaDedicatedWorker.result.connectionCount === 1 ? "ready" : "unexpected",
      qa_nested_worker: qaNestedWorker.result.connectionCount === 1 ? "ready" : "unexpected",
      qa_nested_worker_mismatch: qaNestedWorkerMismatch.error.code,
      qa_delayed_nested_worker_mismatch: qaDelayedNestedWorkerMismatch.error.code,
      qa_shared_worker_mismatch: qaSharedWorkerMismatch.error.code,
      qa_shared_worker_silent: qaSharedWorkerSilent.error.code,
      payloads_retained: 0,
    })}\n`);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

export function formatSmokeFailure(error, scenario) {
  const prefix = `browser readiness integration smoke failed: scenario=${scenario}`;
  if (error?.code === "ERR_ASSERTION") {
    return `${prefix} expectation=${error.message.split("\n")[0]} expected=${inspect(error.expected)} observed=${inspect(error.actual)} operator=${error.operator}`;
  }
  return `${prefix} error=${error?.message ?? "unknown error"}`;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  main().catch((error) => {
    process.stderr.write(`${formatSmokeFailure(error, currentScenario)}\n`);
    process.exitCode = 1;
  });
}
