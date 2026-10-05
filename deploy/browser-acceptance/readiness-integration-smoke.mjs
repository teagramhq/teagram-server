import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import https from "node:https";
import { connect } from "node:net";

import { createConnectObserver } from "./connect-observer.mjs";
import { readiness } from "./runtime.mjs";
import { ALLOWED_HOST, isReadyObserverSnapshot } from "./qa-runtime-adapter.mjs";

const SOURCE_COMMIT = "e".repeat(40);
const CONTENT_DIGEST = `sha256:${"f".repeat(64)}`;

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

async function createFixture(directory, scenario, certificate) {
  const endpoint = `wss://${ALLOWED_HOST}/apiws`;
  const manifest = {
    mode: "private",
    endpoint,
    fingerprint: "0123456789abcdef",
    sourceCommit: SOURCE_COMMIT,
    artifactDigest: scenario === "manifest-mismatch" ? `sha256:${"0".repeat(64)}` : CONTENT_DIGEST,
  };
  const releasePath = `${directory}/${scenario}.json`;
  await writeFile(releasePath, JSON.stringify({
    sourceCommit: SOURCE_COMMIT,
    archiveSha256: CONTENT_DIGEST,
    contentDigest: CONTENT_DIGEST,
    url: `https://${ALLOWED_HOST}/client/?v=${SOURCE_COMMIT}`,
  }), { mode: 0o600 });

  const origin = https.createServer(certificate, (request, response) => {
    const pathname = new URL(request.url, `https://${ALLOWED_HOST}`).pathname;
    if (pathname === "/client/" || pathname === "/client") {
      const html = [
        "<!doctype html><html><head>",
        '<link rel="stylesheet" href="/client/app.css">',
        "</head><body>",
        scenario === "missing-wss" || scenario === "manifest-mismatch"
          ? "<main>credential-free browser readiness fixture</main>"
          : `<script>const socket = new WebSocket(${JSON.stringify(endpoint)}); socket.onerror = () => {};</script>`,
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
    if (request.url !== "/apiws" || scenario === "failed-wss") {
      socket.end("HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\nContent-Length: 0\r\n\r\n");
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
  };
}

async function runScenario(directory, scenario, certificate) {
  const fixture = await createFixture(directory, scenario, certificate);
  try {
    const result = await readiness(fixture.releasePath, {
      testOnly: {
        ignoreHTTPSErrors: true,
        observerSnapshot: async () => fixture.observer.snapshot(),
        proxyServer: fixture.proxyServer,
      },
    });
    return { result, snapshot: fixture.observer.snapshot() };
  } catch (error) {
    return { code: error?.code ?? "unexpected-failure", snapshot: fixture.observer.snapshot() };
  } finally {
    await fixture.closeObserver();
    await fixture.closeOrigin();
  }
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
    assert.ok(positive.snapshot.allowed_connects > 0);
    assert.ok(isReadyObserverSnapshot(positive.snapshot));

    const manifestMismatch = await runScenario(directory, "manifest-mismatch", certificate);
    assert.equal(manifestMismatch.code, "manifest-mismatch", JSON.stringify(manifestMismatch));

    const asset502 = await runScenario(directory, "asset-502", certificate);
    assert.equal(asset502.code, "asset-server-error", JSON.stringify(asset502));

    const missingWss = await runScenario(directory, "missing-wss", certificate);
    assert.equal(missingWss.code, "websocket-not-ready", JSON.stringify(missingWss));

    const failedWss = await runScenario(directory, "failed-wss", certificate);
    assert.equal(failedWss.code, "websocket-not-ready", JSON.stringify(failedWss));

    process.stdout.write(`${JSON.stringify({
      status: "readiness_integration_passed",
      positive: {
        status: positive.result.status,
        http_status: positive.result.http_status,
        browser_wss_status: positive.result.browser_wss_status,
        observer_allowed_connects: positive.snapshot.allowed_connects,
        observer_blocked_requests: positive.snapshot.blocked_requests,
      },
      manifest_mismatch: manifestMismatch.code,
      asset_502: asset502.code,
      missing_wss: missingWss.code,
      failed_wss: failedWss.code,
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
