import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import test from "node:test";

import { ALLOWED_HOST, createConnectObserver, isValidSnapshot } from "./connect-observer.mjs";

const script = new URL("./observer-snapshot.mjs", import.meta.url).pathname;

function runScript() {
  const child = spawn(process.execPath, [script], { stdio: ["ignore", "pipe", "pipe"] });
  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => { stdout += chunk; });
  child.stderr.on("data", (chunk) => { stderr += chunk; });
  return once(child, "close").then(([status]) => ({ status, stdout, stderr }));
}

async function withControl(t, handler) {
  let server = handler;
  if (!server) {
    // The snapshot reports healthy only while the proxy listener is up.
    const observer = createConnectObserver({ manifestHost: ALLOWED_HOST });
    observer.server.listen(0, "127.0.0.1");
    await once(observer.server, "listening");
    t.after(() => new Promise((resolveClose) => observer.server.close(resolveClose)));
    server = observer.controlServer;
  }
  server.listen(3129, "127.0.0.1");
  await once(server, "listening");
  t.after(() => new Promise((resolveClose) => server.close(resolveClose)));
}

test("the fixed in-image script prints one validated observer snapshot line", async (t) => {
  await withControl(t);
  const result = await runScript();
  assert.equal(result.status, 0);
  assert.equal(result.stderr, "");
  assert.equal(result.stdout.split("\n").length, 2);
  assert.ok(Buffer.byteLength(result.stdout) <= 16_384);
  const snapshot = JSON.parse(result.stdout);
  assert.ok(isValidSnapshot(snapshot));
  assert.equal(snapshot.status, "healthy");
});

test("an invalid observer response prints nothing and fails", async (t) => {
  const { createServer } = await import("node:http");
  await withControl(t, createServer((request, response) => {
    response.writeHead(200, { "content-type": "application/json", connection: "close" });
    response.end(JSON.stringify({ status: "healthy", raw: "x" }));
  }));
  const result = await runScript();
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
});
