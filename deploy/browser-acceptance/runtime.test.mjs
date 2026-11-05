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
  registerWorkerWebSocketObservation,
  runtimeErrorOutput,
} from "./runtime.mjs";
import {
  ALLOWED_HOST,
  browserLaunchPolicy,
  cleanupNewBinding,
  isReadyObserverSnapshot,
  observerSnapshot,
  readReleaseRecord,
  snapshot,
  startIndependentCapture,
  targetAddress,
  verifySandbox,
  waitForObserverSnapshot,
} from "./qa-runtime-adapter.mjs";

const sourceCommit = "a".repeat(40);
const digest = `sha256:${"b".repeat(64)}`;
const expectedEndpoint = "wss://teagram.tailaa4918.ts.net/apiws";

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
    wss_diagnostic: "socket-1-target-mismatch",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: false,
    wss_status: 101,
  });
  assert.doesNotMatch(JSON.stringify(mismatch.diagnostic()), /canary|private-value|ts\.net|\//u);

  const noHandshake = observation();
  createConnection(noHandshake, "pending");
  assert.deepEqual(noHandshake.diagnostic(), {
    wss_diagnostic: "socket-1-no-handshake",
    wss_targets: 1,
    wss_handshakes: 0,
    wss_target_match: true,
    wss_status: 0,
  });

  const rejected = observation();
  createConnection(rejected, "rejected");
  handshake(rejected, "rejected", 503);
  assert.deepEqual(rejected.diagnostic(), {
    wss_diagnostic: "socket-1-handshake-not-101",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
  });
});

test("readiness diagnostics identify the failing socket in a mixed handshake observation", () => {
  const mixed = observation();
  createConnection(mixed, "first-private-id");
  handshake(mixed, "first-private-id", 101);
  createConnection(mixed, "second-private-id");
  handshake(mixed, "second-private-id", 503);

  assert.deepEqual(mixed.diagnostic(), {
    wss_diagnostic: "socket-2-handshake-not-101",
    wss_targets: 2,
    wss_handshakes: 2,
    wss_target_match: true,
    wss_status: 503,
  });
  assert.doesNotMatch(JSON.stringify(mixed.diagnostic()), /first-private-id|second-private-id/u);
});

test("a later target mismatch is identified while an earlier matching socket is pending", () => {
  const mixed = observation();
  createConnection(mixed, "first-private-id");
  createConnection(mixed, "second-private-id", `${expectedEndpoint}?private-value`);

  assert.equal(mixed.isSettled(), true);
  assert.deepEqual(mixed.diagnostic(), {
    wss_diagnostic: "socket-2-target-mismatch",
    wss_targets: 2,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  });
});

test("a later rejected handshake is identified while an earlier matching socket is pending", () => {
  const mixed = observation();
  createConnection(mixed, "first-private-id");
  createConnection(mixed, "second-private-id");
  handshake(mixed, "second-private-id", 503);

  assert.equal(mixed.isSettled(), true);
  assert.deepEqual(mixed.diagnostic(), {
    wss_diagnostic: "socket-2-handshake-not-101",
    wss_targets: 2,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
  });
});

test("a handshake arriving before its WebSocketCreated event is paired by request id", () => {
  const mismatch = observation();
  handshake(mismatch, "worker-request", 101);

  assert.equal(mismatch.isSettled(), false);

  createConnection(mismatch, "worker-request", `${expectedEndpoint}?private-value`);
  assert.equal(mismatch.isSettled(), true);
  assert.deepEqual(mismatch.diagnostic(), {
    wss_diagnostic: "socket-1-target-mismatch",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: false,
    wss_status: 101,
  });
});

test("malformed CDP observations fail closed with a bounded reason", () => {
  const orphan = observation();
  handshake(orphan, "orphan", 101);
  assert.equal(orphan.diagnostic().wss_diagnostic, "untracked-handshake");

  const duplicate = observation();
  createConnection(duplicate, "same");
  createConnection(duplicate, "same");
  assert.equal(duplicate.diagnostic().wss_diagnostic, "observation-malformed");

  const badRequestId = observation();
  createConnection(badRequestId, "bad/request-id");
  assert.equal(badRequestId.diagnostic().wss_diagnostic, "observation-malformed");

  const invalidUrl = observation();
  invalidUrl.created({ requestId: "bad-url", url: 17 });
  assert.equal(invalidUrl.diagnostic().wss_diagnostic, "observation-malformed");

  const invalidParams = observation();
  invalidParams.created([]);
  assert.equal(invalidParams.diagnostic().wss_diagnostic, "observation-malformed");

  for (const status of [99, 600, "101", 101.5]) {
    const invalidStatus = observation();
    createConnection(invalidStatus, "invalid-status");
    handshake(invalidStatus, "invalid-status", status);
    const diagnostic = invalidStatus.diagnostic();
    assert.equal(diagnostic.wss_diagnostic, "observation-malformed");
    assert.equal(diagnostic.wss_handshakes, 0);
  }

  const duplicateResponse = observation();
  createConnection(duplicateResponse, "twice");
  handshake(duplicateResponse, "twice", 101);
  handshake(duplicateResponse, "twice", 101);
  assert.equal(duplicateResponse.diagnostic().wss_diagnostic, "socket-1-duplicate-handshake");
  assert.equal(duplicateResponse.diagnostic().wss_handshakes, 2);

  const beforeExpected = createWebSocketObservation();
  createConnection(beforeExpected, "early");
  beforeExpected.setExpectedEndpoint(expectedEndpoint);
  assert.equal(beforeExpected.diagnostic().wss_diagnostic, "observation-malformed");

  const afterClose = observation();
  createConnection(afterClose, "closed");
  afterClose.close();
  handshake(afterClose, "closed", 101);
  assert.equal(afterClose.diagnostic().wss_diagnostic, "socket-1-no-handshake");
  assert.equal(afterClose.diagnostic().wss_handshakes, 0);

  const overflow = observation();
  for (let index = 0; index < 9; index += 1) createConnection(overflow, `connection-${index}`);
  assert.equal(overflow.diagnostic().wss_diagnostic, "observation-malformed");
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

function fakeCdpSession({
  attachFails = false, enableFails = false, replies = true, detach = "release", scriptLoaded = "after",
  ranBefore = [], log = [],
} = {}) {
  const listeners = new Map();
  const sent = [];
  const session = {
    sent,
    on(name, listener) {
      if (!listeners.has(name)) listeners.set(name, []);
      listeners.get(name).push(listener);
      return session;
    },
    emit(name, params) {
      for (const listener of listeners.get(name) ?? []) listener(params);
    },
    workerMessage(sessionId, message) {
      session.emit("Target.receivedMessageFromTarget", { sessionId, message: JSON.stringify(message) });
    },
    async send(method, params) {
      if (method === "Target.sendMessageToTarget") {
        const message = JSON.parse(params.message);
        sent.push({ method: message.method, sessionId: params.sessionId });
        if (replies) {
          const failed = enableFails && message.method === "Network.enable";
          setImmediate(() => session.workerMessage(params.sessionId, failed
            ? { id: message.id, error: { code: -32000, message: "fixture" } }
            : { id: message.id, result: {} }));
          const ran = ranBefore.includes(params.sessionId.replace(/^observer-/u, ""));
          if (message.method === "Runtime.runIfWaitingForDebugger" && scriptLoaded === "after" && !ran) {
            setImmediate(() => session.workerMessage(params.sessionId, { method: "Inspector.workerScriptLoaded", params: {} }));
          }
        }
        return {};
      }
      sent.push({ method, params });
      log.push(method);
      if (method === "Target.attachToTarget") {
        if (attachFails) throw new Error("fixture attach failure");
        const sessionId = `observer-${params.targetId}`;
        // Chrome reports the script as loaded on attach when the worker already ran.
        if (scriptLoaded === "before" || ranBefore.includes(params.targetId)) {
          setImmediate(() => session.workerMessage(sessionId, { method: "Inspector.workerScriptLoaded", params: {} }));
        }
        return { sessionId };
      }
      if (method === "Target.detachFromTarget") {
        if (detach === "reject") throw new Error("fixture detach failure");
        if (detach === "unanswered") return new Promise(() => {});
      }
      return {};
    },
  };
  return session;
}

function fakeBrowser(...sessions) {
  return { async newBrowserCDPSession() { return sessions.shift(); } };
}

async function settle() {
  for (let turn = 0; turn < 20; turn += 1) await new Promise((resolve) => setImmediate(resolve));
}

async function sharedWorkerObservation(options = {}) {
  const value = observation();
  const log = [];
  const browser = fakeCdpSession({ ...options, log });
  const related = fakeCdpSession({ log });
  const page = fakeCdpSession(options);
  await registerWorkerWebSocketObservation(fakeBrowser(browser, related), page, value);
  browser.emit("Target.attachedToTarget", {
    sessionId: "paused-worker",
    targetInfo: { targetId: "worker", type: "shared_worker" },
    waitingForDebugger: true,
  });
  await settle();
  return { value, browser, related, page, log };
}

test("a SharedWorker socket is observed from before the worker starts", async () => {
  const { value, browser, related, page } = await sharedWorkerObservation();
  // The worker's own children are held before it is resumed.
  assert.deepEqual(related.sent, [{
    method: "Target.autoAttachRelated",
    params: { targetId: "worker", waitForDebuggerOnStart: true, filter: [{ type: "worker" }] },
  }]);
  assert.deepEqual(browser.sent, [
    {
      method: "Target.setAutoAttach",
      params: {
        autoAttach: true,
        waitForDebuggerOnStart: true,
        flatten: true,
        filter: [{ type: "shared_worker" }, { type: "service_worker" }],
      },
    },
    { method: "Target.attachToTarget", params: { targetId: "worker", flatten: false } },
    { method: "Network.enable", sessionId: "observer-worker" },
    { method: "Runtime.runIfWaitingForDebugger", sessionId: "observer-worker" },
    { method: "Target.detachFromTarget", params: { sessionId: "paused-worker" } },
  ]);
  assert.deepEqual(page.sent, [{
    method: "Target.setAutoAttach",
    params: { autoAttach: true, waitForDebuggerOnStart: true, flatten: true, filter: [{ type: "worker" }] },
  }]);

  // The observer's own attach is reported for the same target and must not attach again.
  browser.emit("Target.attachedToTarget", {
    sessionId: "observer-worker",
    targetInfo: { targetId: "worker", type: "shared_worker" },
    waitingForDebugger: false,
  });
  await settle();
  assert.equal(browser.sent.filter((entry) => entry.method === "Target.attachToTarget").length, 1);
  assert.equal(value.isSettled(), false);
  assert.equal(value.diagnostic().wss_diagnostic, "no-target");

  browser.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: { requestId: "worker-1", url: expectedEndpoint },
  });
  browser.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: { requestId: "worker-1", response: { status: 101 } },
  });
  assert.equal(value.isSettled(), true);
  assert.equal(value.diagnostic(), null);
});

test("a dedicated worker is held through the page target until its socket can be observed", async () => {
  const value = observation();
  const browser = fakeCdpSession();
  const page = fakeCdpSession();
  await registerWorkerWebSocketObservation(fakeBrowser(browser, fakeCdpSession()), page, value);
  assert.deepEqual(page.sent, [{
    method: "Target.setAutoAttach",
    params: { autoAttach: true, waitForDebuggerOnStart: true, flatten: true, filter: [{ type: "worker" }] },
  }]);
  page.emit("Target.attachedToTarget", {
    sessionId: "paused-dedicated",
    targetInfo: { targetId: "dedicated", type: "worker" },
    waitingForDebugger: true,
  });
  await settle();
  assert.deepEqual(browser.sent.slice(1), [
    { method: "Target.attachToTarget", params: { targetId: "dedicated", flatten: false } },
    { method: "Network.enable", sessionId: "observer-dedicated" },
    { method: "Runtime.runIfWaitingForDebugger", sessionId: "observer-dedicated" },
  ]);
  // Only detaching the page's holding session lets the worker start.
  assert.deepEqual(page.sent.slice(1), [
    { method: "Target.detachFromTarget", params: { sessionId: "paused-dedicated" } },
  ]);
  browser.workerMessage("observer-dedicated", {
    method: "Network.webSocketCreated",
    params: { requestId: "dedicated-1", url: expectedEndpoint },
  });
  browser.workerMessage("observer-dedicated", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: { requestId: "dedicated-1", response: { status: 101 } },
  });
  assert.equal(value.diagnostic(), null);
  // Messages for a session the observer did not attach are not worker evidence.
  browser.workerMessage("paused-dedicated", {
    method: "Network.webSocketCreated",
    params: { requestId: "foreign", url: expectedEndpoint },
  });
  assert.equal(value.diagnostic(), null);
});

test("a worker is not released before its Network domain is enabled", async () => {
  const held = await sharedWorkerObservation({ replies: false });
  assert.equal(held.browser.sent.some((entry) => entry.method === "Target.detachFromTarget"), false);
  assert.equal(held.browser.sent.some((entry) => entry.method === "Runtime.runIfWaitingForDebugger"), false);
});

test("worker sockets to another endpoint fail closed", async () => {
  const { value, browser } = await sharedWorkerObservation();
  browser.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: { requestId: "worker-1", url: `${expectedEndpoint}-other` },
  });
  browser.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: { requestId: "worker-1", response: { status: 101 } },
  });
  assert.deepEqual(value.diagnostic(), {
    wss_diagnostic: "socket-1-target-mismatch",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: false,
    wss_status: 101,
  });

  const mixed = await sharedWorkerObservation();
  createConnection(mixed.value, "page-1");
  handshake(mixed.value, "page-1", 101);
  mixed.browser.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: { requestId: "worker-1", url: "wss://example.invalid/apiws" },
  });
  mixed.browser.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: { requestId: "worker-1", response: { status: 101 } },
  });
  assert.equal(mixed.value.diagnostic().wss_diagnostic, "socket-2-target-mismatch");
});

test("a worker target that cannot be observed fails closed", async () => {
  const attachFailure = await sharedWorkerObservation({ attachFails: true });
  assert.equal(attachFailure.value.diagnostic().wss_diagnostic, "worker-unobservable");
  assert.deepEqual(attachFailure.browser.sent.at(-1), {
    method: "Target.detachFromTarget",
    params: { sessionId: "paused-worker" },
  });

  const enableFailure = await sharedWorkerObservation({ enableFails: true });
  assert.equal(enableFailure.value.diagnostic().wss_diagnostic, "worker-unobservable");
  assert.equal(enableFailure.browser.sent.some((entry) => entry.method === "Runtime.runIfWaitingForDebugger"), false);

  const unanswered = await sharedWorkerObservation({ replies: false });
  createConnection(unanswered.value, "page-1");
  handshake(unanswered.value, "page-1", 101);
  assert.equal(unanswered.value.isSettled(), false);
  unanswered.value.close();
  assert.equal(unanswered.value.diagnostic().wss_diagnostic, "worker-pending");

  const malformed = await sharedWorkerObservation();
  malformed.browser.emit("Target.receivedMessageFromTarget", { sessionId: "observer-worker", message: "{" });
  assert.equal(malformed.value.diagnostic().wss_diagnostic, "worker-unobservable");

  const missingTarget = observation();
  const browser = fakeCdpSession();
  await registerWorkerWebSocketObservation(fakeBrowser(browser, fakeCdpSession()), fakeCdpSession(), missingTarget);
  browser.emit("Target.attachedToTarget", { sessionId: "paused", targetInfo: {} });
  await settle();
  assert.equal(missingTarget.diagnostic().wss_diagnostic, "worker-unobservable");
});

test("a worker stays pending until its hold is released", async () => {
  for (const detach of ["reject", "unanswered"]) {
    const { value } = await sharedWorkerObservation({ detach });
    createConnection(value, "page-1");
    handshake(value, "page-1", 101);
    assert.equal(value.isSettled(), detach === "reject", detach);
    value.close();
    assert.equal(value.diagnostic().wss_diagnostic,
      detach === "reject" ? "worker-unobservable" : "worker-pending", detach);
  }

  const released = await sharedWorkerObservation();
  createConnection(released.value, "page-1");
  handshake(released.value, "page-1", 101);
  assert.equal(released.value.isSettled(), true);
  assert.equal(released.value.diagnostic(), null);
});

test("a worker's descendants are watched before its own session is attached", async () => {
  const { log } = await sharedWorkerObservation();
  assert.deepEqual(log.slice(0, 3), ["Target.setAutoAttach", "Target.autoAttachRelated", "Target.attachToTarget"]);
});

test("a child a running SharedWorker started before the watch began fails closed", async () => {
  const { value, browser, related } = await sharedWorkerObservation({ ranBefore: ["child"] });
  createConnection(value, "page-1");
  handshake(value, "page-1", 101);
  // Chrome reports the existing child when the watch starts, not waiting for a debugger.
  related.emit("Target.attachedToTarget", {
    sessionId: "existing-child",
    targetInfo: { targetId: "child", type: "worker" },
    waitingForDebugger: false,
  });
  await settle();
  assert.equal(browser.sent.some((entry) => entry.params?.targetId === "child"), true);
  value.close();
  assert.equal(value.diagnostic().wss_diagnostic, "worker-unobservable");
});

test("a worker whose script ran before observation began fails closed", async () => {
  for (const scriptLoaded of ["before", "never"]) {
    const { value } = await sharedWorkerObservation({ scriptLoaded });
    createConnection(value, "page-1");
    handshake(value, "page-1", 101);
    value.close();
    assert.equal(value.diagnostic().wss_diagnostic,
      scriptLoaded === "before" ? "worker-unobservable" : "worker-pending", scriptLoaded);
  }
});

test("workers started by a worker are held and observed before the parent runs", async () => {
  const { value, browser, related } = await sharedWorkerObservation();
  related.emit("Target.attachedToTarget", {
    sessionId: "paused-child",
    targetInfo: { targetId: "child", type: "worker" },
    waitingForDebugger: true,
  });
  await settle();
  assert.deepEqual(browser.sent.slice(-3), [
    { method: "Target.attachToTarget", params: { targetId: "child", flatten: false } },
    { method: "Network.enable", sessionId: "observer-child" },
    { method: "Runtime.runIfWaitingForDebugger", sessionId: "observer-child" },
  ]);
  assert.deepEqual(related.sent.slice(1), [
    {
      method: "Target.autoAttachRelated",
      params: { targetId: "child", waitForDebuggerOnStart: true, filter: [{ type: "worker" }] },
    },
    { method: "Target.detachFromTarget", params: { sessionId: "paused-child" } },
  ]);
  createConnection(value, "page-1");
  handshake(value, "page-1", 101);
  browser.workerMessage("observer-child", {
    method: "Network.webSocketCreated",
    params: { requestId: "child-1", url: `${expectedEndpoint}-other` },
  });
  browser.workerMessage("observer-child", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: { requestId: "child-1", response: { status: 101 } },
  });
  assert.equal(value.diagnostic().wss_diagnostic, "socket-2-target-mismatch");

  const relatedFailure = observation();
  const failingRelated = fakeCdpSession();
  failingRelated.send = async (method) => {
    if (method === "Target.autoAttachRelated") throw new Error("fixture related failure");
    return {};
  };
  const parentBrowser = fakeCdpSession();
  await registerWorkerWebSocketObservation(fakeBrowser(parentBrowser, failingRelated), fakeCdpSession(), relatedFailure);
  parentBrowser.emit("Target.attachedToTarget", {
    sessionId: "paused-worker",
    targetInfo: { targetId: "worker", type: "shared_worker" },
    waitingForDebugger: true,
  });
  await settle();
  assert.equal(relatedFailure.diagnostic().wss_diagnostic, "worker-unobservable");
  assert.equal(parentBrowser.sent.some((entry) => entry.method === "Runtime.runIfWaitingForDebugger"), false);
});

test("a worker that opens no socket in the window stays not ready", async () => {
  const { value } = await sharedWorkerObservation();
  value.close();
  assert.deepEqual(value.diagnostic(), {
    wss_diagnostic: "no-target",
    wss_targets: 0,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  });
});

test("runtime error emission revalidates the exact diagnostic consistency table", () => {
  const valid = {
    wss_diagnostic: "socket-1-handshake-not-101",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
  };
  assert.equal(JSON.stringify(runtimeErrorOutput({ code: "websocket-not-ready", wssDiagnostic: valid })),
    '{"status":"error","code":"websocket-not-ready","wss_diagnostic":"socket-1-handshake-not-101","wss_targets":1,"wss_handshakes":1,"wss_target_match":true,"wss_status":503}');
  assert.deepEqual(runtimeErrorOutput({
    code: "websocket-not-ready",
    wssDiagnostic: { ...valid, wss_status: 101 },
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
  assert.equal(ALLOWED_HOST, "teagram.tailaa4918.ts.net");
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
    url: `https://teagram.tailaa4918.ts.net/client/?v=${sourceCommit}`,
    ...overrides,
  };
}

test("verified release input accepts only the exact pinned host and typed digests", async () => {
  await withRecord(releaseRecord(), async (path) => {
    const result = await readReleaseRecord(path);
    assert.equal(result.sourceCommit, sourceCommit);
    assert.equal(new URL(result.url).hostname, "teagram.tailaa4918.ts.net");
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

test("observer snapshot maps fetch and JSON failures to its stable adapter code", async (t) => {
  await t.test("fetch failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => {
      throw new Error("private observer transport detail");
    });
    await assert.rejects(observerSnapshot(), (error) => error.code === "observer-unhealthy");
  });

  await t.test("JSON parse failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response("{", {status: 200}));
    await assert.rejects(observerSnapshot(), (error) => error.code === "observer-unhealthy");
  });
});

test("observer readiness waits for browser-side health and fails closed at its deadline", async (t) => {
  await t.test("waits until the observer is reachable", async () => {
    let attempts = 0;
    const snapshot = await waitForObserverSnapshot(async () => {
      attempts += 1;
      if (attempts < 3) {
        const error = new Error("observer is not reachable yet");
        error.code = "observer-unhealthy";
        throw error;
      }
      return { status: "healthy" };
    }, 1_000);

    assert.deepEqual(snapshot, { status: "healthy" });
    assert.equal(attempts, 3);
  });

  await t.test("does not wait indefinitely for an unhealthy observer", async () => {
    let attempts = 0;
    await assert.rejects(waitForObserverSnapshot(async () => {
      attempts += 1;
      const error = new Error("observer remains unhealthy");
      error.code = "observer-unhealthy";
      throw error;
    }, 20), (error) => error.code === "observer-unhealthy");
    assert.ok(attempts >= 1 && attempts <= 2);
  });

  await t.test("does not retry unrelated runtime failures", async () => {
    let attempts = 0;
    await assert.rejects(waitForObserverSnapshot(async () => {
      attempts += 1;
      const error = new Error("runtime policy failed");
      error.code = "blocked-host-control-failed";
      throw error;
    }, 1_000), (error) => error.code === "blocked-host-control-failed");
    assert.equal(attempts, 1);
  });
});

test("browser launch policy waits for initially unreachable observer snapshots", async () => {
  let snapshotCalls = 0;
  let blockedControlCompleted = false;
  const snapshot = () => ({
    telegram_attempts: blockedControlCompleted ? 1 : 0,
    blocked_requests: blockedControlCompleted ? 1 : 0,
    connect_attempts: {
      [ALLOWED_HOST]: 2,
      "telegram-suffix": blockedControlCompleted ? 1 : 0,
      other: 0,
    },
    dns_lookups: 0,
    upstream_connects: 0,
  });

  const policy = await browserLaunchPolicy({ targetHost: ALLOWED_HOST }, { testOnly: {
    observerSnapshot: async () => {
      snapshotCalls += 1;
      if (snapshotCalls === 1 || snapshotCalls === 3) {
        const error = new Error("observer is not reachable yet");
        error.code = "observer-unhealthy";
        throw error;
      }
      return snapshot();
    },
    runBlockedHostControl: async () => {
      blockedControlCompleted = true;
    },
  } });

  assert.equal(snapshotCalls, 4);
  assert.equal(policy.blockedHostControlPassed, true);
});

test("target address maps fetch and JSON failures to stable adapter codes", async (t) => {
  await t.test("fetch failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => {
      throw new Error("private address transport detail");
    });
    await assert.rejects(targetAddress(), (error) => error.code === "target-address-unavailable");
  });

  await t.test("JSON parse failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response("{", {status: 200}));
    await assert.rejects(targetAddress(), (error) => error.code === "target-address-invalid");
  });
});

test("readiness requires the observer's exact healthy zero-block contract", () => {
  const healthy = {
    status: "healthy",
    allowed_host: "teagram.tailaa4918.ts.net",
    connect_attempts: {
      "teagram.tailaa4918.ts.net": 1,
      "telegram-suffix": 0,
      other: 0,
    },
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
  assert.equal(isReadyObserverSnapshot({
    ...healthy,
    connect_attempts: { ...healthy.connect_attempts, other: 101 },
  }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, extra: "not an observer field" }), false);
  assert.equal(isReadyObserverSnapshot({ ...healthy, dns_lookups: 0 }), false);
});

test("host-only acceptance observations fail closed without a reviewed provider", async () => {
  await assert.rejects(snapshot("before"), (error) => error.code === "runtime-snapshot-unavailable");
  await assert.rejects(startIndependentCapture({}), (error) => error.code === "independent-capture-unavailable");
  assert.deepEqual(await cleanupNewBinding(), { performed: false });
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
