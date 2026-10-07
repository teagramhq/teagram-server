import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import https from "node:https";
import { connect } from "node:net";

import { createConnectObserver } from "./connect-observer.mjs";
import { readiness, runtimeErrorOutput } from "./runtime.mjs";
import { ALLOWED_HOST, isReadyObserverSnapshot } from "./qa-runtime-adapter.mjs";

const SOURCE_COMMIT = "e".repeat(40);
const CONTENT_DIGEST = `sha256:${"f".repeat(64)}`;
const URL_CANARY = "wss-diagnostic-canary";
const HEADER_CANARY = "wss-response-header-canary";
const COOKIE_CANARY = "wss-set-cookie-canary";
const READY_LINE = "{\"status\":\"ready\",\"http_status\":200,\"asset_502_count\":0,\"browser_wss_status\":101,\"browser_wss_unique_targets\":1,\"observer_success_hosts\":1,\"observer_target_host\":\"telegram-server.tailaa4918.ts.net\",\"telegram_org_attempts\":0,\"payloads_retained\":0}";

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
  await writeFile(releasePath, JSON.stringify({
    sourceCommit: SOURCE_COMMIT,
    archiveSha256: CONTENT_DIGEST,
    contentDigest: CONTENT_DIGEST,
    url: `https://${ALLOWED_HOST}/client/?v=${SOURCE_COMMIT}`,
  }), { mode: 0o600 });

  let appRequests = 0;
  let upgrades = 0;
  const origin = https.createServer(certificate, (request, response) => {
    const pathname = new URL(request.url, `https://${ALLOWED_HOST}`).pathname;
    if (pathname === "/client/" || pathname === "/client") {
      appRequests += 1;
      if (scenario === "manifest-mismatch-app-404") {
        response.writeHead(404, { connection: "close", "content-length": "0" });
        response.end();
        return;
      }
      const script = (() => {
        if (["missing-wss", "manifest-mismatch", "manifest-mismatch-app-404"].includes(scenario)) return "";
        if (scenario === "delayed-wss") {
          return `<script>setTimeout(() => { const socket = new WebSocket(${JSON.stringify(endpoint)}); socket.onerror = () => {}; }, 3000);</script>`;
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
    if (scenario === "held-wss") return;
    if (scenario === "destroyed-wss") {
      socket.destroy();
      return;
    }
    if (scenario === "failed-wss" || (scenario === "mixed-status" && upgrades === 2)) {
      rejectUpgrade(socket);
      return;
    }
    const allowMismatchedTarget = ["path-mismatch", "query-mismatch", "mixed-target"].includes(scenario);
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
    appRequests: () => appRequests,
    upgrades: () => upgrades,
  };
}

async function runScenario(directory, scenario, certificate) {
  const fixture = await createFixture(directory, scenario, certificate);
  let observationMs;
  try {
    const result = await readiness(fixture.releasePath, {
      testOnly: {
        ignoreHTTPSErrors: true,
        onObservationComplete: (elapsed) => { observationMs = elapsed; },
        observerSnapshot: async () => fixture.observer.snapshot(),
        proxyServer: fixture.proxyServer,
      },
    });
    return {
      result,
      snapshot: fixture.observer.snapshot(),
      observationMs,
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

function errorLine(result) {
  const line = JSON.stringify(runtimeErrorOutput({
    code: result.error.code,
    wssDiagnostic: result.error.diagnostic,
  }));
  const output = JSON.parse(line);
  assert.equal(output.status, "error");
  assert.deepEqual(Object.keys(output), output.code === "websocket-not-ready"
    ? ["status", "code", "wss_diagnostic", "wss_targets", "wss_handshakes", "wss_target_match", "wss_status"]
    : ["status", "code"]);
  assertNoDiagnosticLeaks(line);
  return line;
}

function assertNoDiagnosticLeaks(line) {
  assert.equal(line.includes("/"), false, line);
  assert.doesNotMatch(line, /canary|ts\.net|header|set-cookie|raw error|payload|credential/iu);
}

function assertWssError(result, expected, label) {
  assert.equal(result.error?.code, "websocket-not-ready", JSON.stringify(result));
  assert.deepEqual(result.error.diagnostic, expected, `${label}: ${JSON.stringify(result)}`);
  const line = errorLine(result);
  assertNoDiagnosticLeaks(line);
  return JSON.parse(line);
}

async function main() {
  process.umask(0o077);
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
    assert.equal(positive.result?.status, "ready", JSON.stringify(positive));
    assert.equal(positive.result.http_status, 200);
    assert.equal(positive.result.browser_wss_status, 101);
    assert.equal(JSON.stringify(positive.result), READY_LINE);
    assert.ok(positive.snapshot.allowed_connects > 0);
    assert.ok(isReadyObserverSnapshot(positive.snapshot));

    const delayed = await runScenario(directory, "delayed-wss", certificate);
    assert.equal(delayed.result?.status, "ready", JSON.stringify(delayed));
    assert.ok(delayed.observationMs >= 3_000, JSON.stringify(delayed));
    assert.ok(delayed.observationMs < 10_000, JSON.stringify(delayed));
    assert.equal(JSON.stringify(delayed.result), READY_LINE);

    const multipleSuccess = await runScenario(directory, "multiple-success", certificate);
    assert.equal(multipleSuccess.result?.status, "ready", JSON.stringify(multipleSuccess));
    assert.equal(JSON.stringify(multipleSuccess.result), READY_LINE);

    const manifestMismatch = await runScenario(directory, "manifest-mismatch", certificate);
    assert.equal(manifestMismatch.error.code, "manifest-mismatch", JSON.stringify(manifestMismatch));
    assertNoDiagnosticLeaks(errorLine(manifestMismatch));

    const manifestBeforeApp = await runScenario(directory, "manifest-mismatch-app-404", certificate);
    assert.equal(manifestBeforeApp.error.code, "manifest-mismatch", JSON.stringify(manifestBeforeApp));
    assert.equal(manifestBeforeApp.appRequests, 0);
    assertNoDiagnosticLeaks(errorLine(manifestBeforeApp));

    const asset502 = await runScenario(directory, "asset-502", certificate);
    assert.equal(asset502.error.code, "asset-server-error", JSON.stringify(asset502));
    assertNoDiagnosticLeaks(errorLine(asset502));

    const missingWss = await runScenario(directory, "missing-wss", certificate);
    const noTarget = assertWssError(missingWss, {
      wss_diagnostic: "no-target",
      wss_targets: 0,
      wss_handshakes: 0,
      wss_target_match: false,
      wss_status: 0,
    });
    assert.ok(missingWss.observationMs >= 10_000, JSON.stringify(missingWss));
    assert.ok(missingWss.observationMs < 15_000, JSON.stringify(missingWss));

    const failedWss = await runScenario(directory, "failed-wss", certificate);
    assert.equal(failedWss.error?.code, "websocket-not-ready", JSON.stringify(failedWss));
    assert.equal(failedWss.upgrades, 1, JSON.stringify(failedWss));
    const failedWssLine = errorLine(failedWss);
    const noHandshakeLine = "{\"status\":\"error\",\"code\":\"websocket-not-ready\",\"wss_diagnostic\":\"no-handshake\",\"wss_targets\":1,\"wss_handshakes\":0,\"wss_target_match\":true,\"wss_status\":0}";
    const handshakeNot101Line = "{\"status\":\"error\",\"code\":\"websocket-not-ready\",\"wss_diagnostic\":\"handshake-not-101\",\"wss_targets\":1,\"wss_handshakes\":1,\"wss_target_match\":true,\"wss_status\":503}";
    assert.ok([noHandshakeLine, handshakeNot101Line].includes(failedWssLine), failedWssLine);
    const failedWssDiagnostic = JSON.parse(failedWssLine);
    const failedWssHandshakeEventAvailable = failedWssDiagnostic.wss_handshakes === 1;
    assert.equal(failedWssHandshakeEventAvailable, failedWssDiagnostic.wss_diagnostic === "handshake-not-101");
    const architecture = process.arch === "arm64" ? "arm64" : process.arch === "x64" ? "amd64" : "unknown";
    if (architecture === "arm64") assert.equal(failedWssLine, noHandshakeLine);

    const pathMismatch = await runScenario(directory, "path-mismatch", certificate);
    const pathMismatchDiagnostic = assertWssError(pathMismatch, {
      wss_diagnostic: "target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });

    const queryMismatch = await runScenario(directory, "query-mismatch", certificate);
    const queryMismatchDiagnostic = assertWssError(queryMismatch, {
      wss_diagnostic: "target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });

    const heldWss = await runScenario(directory, "held-wss", certificate);
    const heldWssDiagnostic = assertWssError(heldWss, {
      wss_diagnostic: "no-handshake",
      wss_targets: 1,
      wss_handshakes: 0,
      wss_target_match: true,
      wss_status: 0,
    });

    const destroyedWss = await runScenario(directory, "destroyed-wss", certificate);
    const destroyedWssDiagnostic = assertWssError(destroyedWss, {
      wss_diagnostic: "no-handshake",
      wss_targets: 1,
      wss_handshakes: 0,
      wss_target_match: true,
      wss_status: 0,
    });

    const mixedStatus = await runScenario(directory, "mixed-status", certificate);
    const mixedStatusDiagnostic = assertWssError(mixedStatus, {
      wss_diagnostic: "ambiguous",
      wss_targets: 2,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 0,
    });

    const mixedTarget = await runScenario(directory, "mixed-target", certificate);
    const mixedTargetDiagnostic = assertWssError(mixedTarget, {
      wss_diagnostic: "ambiguous",
      wss_targets: 2,
      wss_handshakes: 2,
      wss_target_match: false,
      wss_status: 0,
    });

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
      delayed_wss_observation_ms: delayed.observationMs,
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
      payloads_retained: 0,
    })}\n`);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

main().catch((error) => {
  process.stderr.write(`browser readiness integration smoke failed: ${error?.message ?? "unknown error"}\n`);
  process.exitCode = 1;
});
