import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

import {
  createWebSocketObservation,
  registerWebSocketObservation,
  runtimeErrorOutput,
} from "./runtime.mjs";
import {
  ALLOWED_HOST,
  isReadyObserverSnapshot,
  readReleaseRecord,
  snapshot,
  startIndependentCapture,
  verifySandbox,
} from "./qa-runtime-adapter.mjs";

const sourceCommit = "a".repeat(40);
const digest = `sha256:${"b".repeat(64)}`;
const expectedEndpoint = "wss://telegram-server.tailaa4918.ts.net/apiws";

function observation() {
  const value = createWebSocketObservation();
  value.setExpectedEndpoint(expectedEndpoint);
  return value;
}

function createConnection(value, requestId, url = expectedEndpoint) {
  value.created({ requestId, url });
}

function handshake(value, requestId, status) {
  value.handshakeResponse({ requestId, response: { status } });
}

test("uniform matching WSS connections remain ready", () => {
  for (const count of [1, 2]) {
    const value = observation();
    for (let index = 0; index < count; index += 1) {
      const requestId = `request-${index}`;
      createConnection(value, requestId);
      handshake(value, requestId, 101);
    }
    assert.equal(value.isSettled(), true);
    assert.equal(value.diagnostic(), null);
  }
});

test("readiness diagnostics distinguish missing, mismatched, and rejected handshakes", () => {
  const noTarget = observation();
  assert.deepEqual(noTarget.diagnostic(), {
    wss_diagnostic: "no-target",
    wss_targets: 0,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  });

  const mismatch = observation();
  createConnection(mismatch, "query", `${expectedEndpoint}?canary=private-value`);
  handshake(mismatch, "query", 101);
  assert.deepEqual(mismatch.diagnostic(), {
    wss_diagnostic: "target-mismatch",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: false,
    wss_status: 101,
  });
  assert.doesNotMatch(JSON.stringify(mismatch.diagnostic()), /canary|private-value|ts\.net|\//u);

  const noHandshake = observation();
  createConnection(noHandshake, "pending");
  assert.deepEqual(noHandshake.diagnostic(), {
    wss_diagnostic: "no-handshake",
    wss_targets: 1,
    wss_handshakes: 0,
    wss_target_match: true,
    wss_status: 0,
  });

  const rejected = observation();
  createConnection(rejected, "rejected");
  handshake(rejected, "rejected", 503);
  assert.deepEqual(rejected.diagnostic(), {
    wss_diagnostic: "handshake-not-101",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
  });
});

test("ambiguous and malformed CDP observations fail closed", () => {
  const orphan = observation();
  handshake(orphan, "orphan", 101);
  assert.equal(orphan.diagnostic().wss_diagnostic, "ambiguous");

  const duplicate = observation();
  createConnection(duplicate, "same");
  createConnection(duplicate, "same");
  assert.equal(duplicate.diagnostic().wss_diagnostic, "ambiguous");

  const badRequestId = observation();
  createConnection(badRequestId, "bad/request-id");
  assert.equal(badRequestId.diagnostic().wss_diagnostic, "ambiguous");

  const invalidUrl = observation();
  invalidUrl.created({ requestId: "bad-url", url: 17 });
  assert.equal(invalidUrl.diagnostic().wss_diagnostic, "ambiguous");

  const invalidParams = observation();
  invalidParams.created([]);
  assert.equal(invalidParams.diagnostic().wss_diagnostic, "ambiguous");

  for (const status of [99, 600, "101", 101.5]) {
    const invalidStatus = observation();
    createConnection(invalidStatus, "invalid-status");
    handshake(invalidStatus, "invalid-status", status);
    const diagnostic = invalidStatus.diagnostic();
    assert.equal(diagnostic.wss_diagnostic, "ambiguous");
    assert.equal(diagnostic.wss_handshakes, 0);
  }

  const duplicateResponse = observation();
  createConnection(duplicateResponse, "twice");
  handshake(duplicateResponse, "twice", 101);
  handshake(duplicateResponse, "twice", 101);
  assert.equal(duplicateResponse.diagnostic().wss_diagnostic, "ambiguous");
  assert.equal(duplicateResponse.diagnostic().wss_handshakes, 2);

  const beforeExpected = createWebSocketObservation();
  createConnection(beforeExpected, "early");
  beforeExpected.setExpectedEndpoint(expectedEndpoint);
  assert.equal(beforeExpected.diagnostic().wss_diagnostic, "ambiguous");

  const afterClose = observation();
  createConnection(afterClose, "closed");
  afterClose.close();
  handshake(afterClose, "closed", 101);
  assert.equal(afterClose.diagnostic().wss_diagnostic, "no-handshake");
  assert.equal(afterClose.diagnostic().wss_handshakes, 0);

  const overflow = observation();
  for (let index = 0; index < 9; index += 1) createConnection(overflow, `connection-${index}`);
  assert.equal(overflow.diagnostic().wss_diagnostic, "ambiguous");
  assert.equal(overflow.diagnostic().wss_targets, 2);
});

test("session registers only the original WSS observation events", () => {
  const registered = [];
  registerWebSocketObservation({ on: (name) => registered.push(name) }, {
    created() {},
    handshakeResponse() {},
  });
  assert.deepEqual(registered, [
    "Network.webSocketCreated",
    "Network.webSocketHandshakeResponseReceived",
  ]);
});

test("runtime error emission revalidates the exact diagnostic consistency table", () => {
  const valid = {
    wss_diagnostic: "handshake-not-101",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
  };
  assert.equal(JSON.stringify(runtimeErrorOutput({ code: "websocket-not-ready", wssDiagnostic: valid })),
    '{"status":"error","code":"websocket-not-ready","wss_diagnostic":"handshake-not-101","wss_targets":1,"wss_handshakes":1,"wss_target_match":true,"wss_status":503}');
  assert.deepEqual(runtimeErrorOutput({
    code: "websocket-not-ready",
    wssDiagnostic: { ...valid, wss_target_match: false },
  }), { status: "error", code: "websocket-not-ready" });
  assert.deepEqual(runtimeErrorOutput({
    code: "websocket-not-ready",
    wssDiagnostic: { ...valid, unexpected: "must-not-be-emitted" },
  }), { status: "error", code: "websocket-not-ready" });
  const leaked = JSON.stringify(runtimeErrorOutput({
    code: "websocket-not-ready",
    wssDiagnostic: {
      ...valid,
      observedUrl: "https://example.invalid/wss-diagnostic-canary",
      responseHeader: "wss-response-header-canary",
      setCookie: "wss-set-cookie-canary",
    },
  }));
  assert.equal(leaked, '{"status":"error","code":"websocket-not-ready"}');
  assert.doesNotMatch(leaked, /wss-diagnostic-canary|wss-response-header-canary|wss-set-cookie-canary|\/|ts\.net/u);
});

test("runtime adapter exports the pinned host used by the runtime entrypoint", () => {
  assert.equal(ALLOWED_HOST, "telegram-server.tailaa4918.ts.net");
});

async function withRecord(record, run) {
  const directory = await mkdtemp(join(tmpdir(), "browser-acceptance-test-"));
  const path = join(directory, "release-record.json");
  try {
    await writeFile(path, JSON.stringify(record), { mode: 0o600 });
    await run(path);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

function releaseRecord(overrides = {}) {
  return {
    sourceCommit,
    archiveSha256: digest,
    contentDigest: digest,
    url: `https://telegram-server.tailaa4918.ts.net/client/?v=${sourceCommit}`,
    ...overrides,
  };
}

test("verified release input accepts only the exact pinned host and typed digests", async () => {
  await withRecord(releaseRecord(), async (path) => {
    const result = await readReleaseRecord(path);
    assert.equal(result.sourceCommit, sourceCommit);
    assert.equal(new URL(result.url).hostname, "telegram-server.tailaa4918.ts.net");
  });
  await withRecord(releaseRecord({ url: `https://attacker.example/client/?v=${sourceCommit}` }), async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
  await withRecord(releaseRecord({ contentDigest: "sha256:wrong" }), async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
  await withRecord({ ...releaseRecord(), unexpected: "must-not-widen-policy" }, async (path) => {
    await assert.rejects(readReleaseRecord(path), (error) => error.code === "release-record-invalid");
  });
});

test("readiness requires the observer's exact healthy zero-block contract", () => {
  const healthy = {
    status: "healthy",
    allowed_host: "telegram-server.tailaa4918.ts.net",
    allowed_connects: 1,
    blocked_requests: 0,
    telegram_attempts: 0,
    other_blocked_count: 0,
    dns_lookups: 1,
    upstream_connects: 1,
    upstream_failures: 0,
  };
  assert.equal(isReadyObserverSnapshot(healthy), true);
  assert.equal(isReadyObserverSnapshot({ ...healthy, other_blocked_count: 1 }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, extra: "not an observer field" }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, dns_lookups: 0 }), false);
});

test("host-only acceptance observations fail closed without a reviewed provider", async () => {
  await assert.rejects(snapshot("before"), (error) => error.code === "runtime-snapshot-unavailable");
  await assert.rejects(startIndependentCapture({}), (error) => error.code === "independent-capture-unavailable");
});

test("sandbox proof cannot be inferred when Chromium processes are absent", async () => {
  await assert.rejects(
    verifySandbox({ expectedProxyServer: "http://browser-observer:3128" }),
    (error) => error.code === "sandbox-proof-invalid",
  );
});

for (const executable of ["chrome-headless-shell", "headless_shell"]) {
  for (const flattenedTitle of [false, true]) {
    test(`sandbox inspection records ${executable} with ${flattenedTitle ? "flattened" : "NUL-separated"} argv`, async () => {
      const children = [];
      try {
        for (const args of [
          ["--proxy-server=http://browser-observer:3128", "--proxy-bypass-list=<-loopback>",
            "--disable-background-networking", "--disable-crash-reporter", "--disable-breakpad",
            "--disable-quic", "--dns-over-https-mode=off",
            "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"],
          ["--type=renderer"],
        ]) {
          // Real /proc entries exercise argv discovery; these fixture processes
          // share namespaces and therefore cannot prove sandbox isolation.
          const title = flattenedTitle && args.includes("--type=renderer")
            ? `process.title = ${JSON.stringify(`/ms-playwright/chromium/${executable} ${args.join(" ")}`)}; `
            : "";
          const child = spawn(process.execPath, [
            "-e", `${title}process.send('ready'); setInterval(() => {}, 1000)`, "--", ...args,
          ], { argv0: `/ms-playwright/chromium/${executable}`, stdio: ["ignore", "ignore", "ignore", "ipc"] });
          children.push(child);
          await once(child, "message");
        }
        const proof = await verifySandbox({ expectedProxyServer: "http://browser-observer:3128" });
        assert.equal(proof.observedProxyServer, "http://browser-observer:3128");
        assert.equal(proof.observedProxyBypassList, "<-loopback>");
        assert.deepEqual(proof.missingRequiredNetworkArgs, []);
        assert.deepEqual(proof.conflictingNetworkArgs, []);
        assert.deepEqual(proof.forbiddenSandboxFlags, []);
        assert.ok(proof.browserUserNamespaceInode > 0);
        assert.ok(proof.rendererPidNamespaceInode > 0);
        assert.equal(proof.browserUserNamespaceInode, proof.rendererUserNamespaceInode);
        assert.equal(proof.browserPidNamespaceInode, proof.rendererPidNamespaceInode);
      } finally {
        await Promise.all(children.map(async (child) => {
          const exited = once(child, "exit");
          child.kill();
          await exited;
        }));
      }
    });
  }
}

for (const flattenedTitle of [false, true]) {
  test(`sandbox proof rejects disable-sandbox flags in ${flattenedTitle ? "flattened" : "NUL-separated"} renderer argv`, async () => {
    const children = [];
    try {
      for (const args of [
        ["--proxy-server=http://browser-observer:3128", "--proxy-bypass-list=<-loopback>",
          "--disable-background-networking", "--disable-crash-reporter", "--disable-breakpad",
          "--disable-quic", "--dns-over-https-mode=off",
          "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"],
        ["--type=renderer", "--disable-setuid-sandbox", "--no-sandbox=1"],
      ]) {
        const title = flattenedTitle && args.includes("--type=renderer")
          ? `process.title = ${JSON.stringify(`/ms-playwright/chromium/chrome-headless-shell ${args.join(" ")}`)}; `
          : "";
        const child = spawn(process.execPath, [
          "-e", `${title}process.send('ready'); setInterval(() => {}, 1000)`, "--", ...args,
        ], { argv0: "/ms-playwright/chromium/chrome-headless-shell", stdio: ["ignore", "ignore", "ignore", "ipc"] });
        children.push(child);
        await once(child, "message");
      }

      const proof = await verifySandbox({ expectedProxyServer: "http://browser-observer:3128" });
      assert.equal(proof.chromiumSandboxEnabled, false);
      assert.deepEqual(proof.forbiddenSandboxFlags, ["--disable-setuid-sandbox", "--no-sandbox=1"]);
    } finally {
      await Promise.all(children.map(async (child) => {
        const exited = once(child, "exit");
        child.kill();
        await exited;
      }));
    }
  });
}
