import assert from "node:assert/strict";
import {mkdtemp, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";
import {runInNewContext} from "node:vm";

import {acceptanceRpcOutcome, createAcceptanceProgress} from "./acceptance-result.mjs";
import {executeMode, installClientProgressObserver, proveWss} from "./private-browser-acceptance-v2.mjs";
import {readReleaseRecord} from "./qa-runtime-adapter.mjs";

const targetHost = "teagram.tailaa4918.ts.net";
const sourceCommit = "a".repeat(40);
const contentDigest = `sha256:${"b".repeat(64)}`;
const usernameCanary = "fixture_account_01";
const passwordCanary = "fixture-private-password";

function createDependencies(failAt, allowCredentialRead = false) {
  const events = [];
  const credentials = {username: usernameCanary, password: passwordCanary};
  let launchPolicyCalls = 0;
  let credentialReads = 0;
  let logoutMethod;

  const release = {
    sourceCommit,
    archiveSha256: `sha256:${"c".repeat(64)}`,
    contentDigest,
    url: `https://${targetHost}/private/?v=${sourceCommit}`,
  };
  const policy = {
    proxyServer: "http://browser-observer:3128",
    proxyBypassHosts: [],
    observerAllowedTargets: [{host: targetHost, port: 443}],
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
  const sandboxProof = {
    chromiumSandboxEnabled: true,
    forbiddenSandboxFlags: [],
    observedProxyServer: policy.proxyServer,
    observedProxyBypassList: "<-loopback>",
    proxyServerArgCount: 1,
    proxyBypassArgCount: 1,
    missingRequiredNetworkArgs: [],
    conflictingNetworkArgs: [],
    browserUserNamespaceInode: 11,
    browserPidNamespaceInode: 21,
    rendererUserNamespaceInode: 12,
    rendererPidNamespaceInode: 22,
  };
  const manifest = {
    mode: "private",
    endpoint: `wss://${targetHost}/apiws`,
    fingerprint: "d".repeat(16),
    sourceCommit,
    artifactDigest: contentDigest,
  };
  const contextListeners = new Map();
  const contextInfo = {
    page: {fixture: true},
    context: {
      fixture: true,
      on(event, listener) {
        const listeners = contextListeners.get(event) ?? [];
        listeners.push(listener);
        contextListeners.set(event, listeners);
      },
    },
  };

  function step(name) {
    events.push(name);
    if (failAt === "browser-close-mid-check" && name === "wss-101") {
      for (const listener of contextListeners.get("close") ?? []) listener();
      throw new Error("Target page, context or browser has been closed");
    }
    if (failAt === name) throw new Error(`fixture failure at ${name}`);
  }

  return {
    events,
    credentials,
    get launchPolicyCalls() { return launchPolicyCalls; },
    get credentialReads() { return credentialReads; },
    get logoutMethod() { return logoutMethod; },
    dependencies: {
      readReleaseRecord() {
        step("release-record");
        return release;
      },
      createAcceptanceProgress(options) {
        step("progress-helper");
        return createAcceptanceProgress(options);
      },
      acceptanceRpcOutcome,
      browserLaunchPolicy() {
        launchPolicyCalls += 1;
        step("browser-launch-policy");
        return policy;
      },
      async launchContext() {
        step("browser-launch");
        return contextInfo;
      },
      verifySandbox() {
        step("sandbox");
        return sandboxProof;
      },
      async installNetworkGuards() {
        step("network-guards");
        return {};
      },
      async readManifestThroughChromium() {
        step("served-manifest");
        return manifest;
      },
      async proveWss(_context, _release, _manifest, progress) {
        step("wss-101");
        progress.observeWssConnection();
        progress.observeWssHandshake(101);
        return {connectionCount: 1, status101: true};
      },
      async verifyAssetLoading() {
        step("asset-loading");
      },
      async installClientProgressObserver() {
        step("client-progress");
        return {};
      },
      async verifyNegativeTargets() {
        step("negative-targets");
      },
      async closeContext() {
        events.push("close-context");
      },
      async readCredentials() {
        credentialReads += 1;
        events.push("stdin");
        if (!allowCredentialRead) throw new Error("failing-on-read fixture");
        return credentials;
      },
      async performAcceptance(prepared, suppliedCredentials) {
        events.push("acceptance");
        assert.equal(suppliedCredentials.username, usernameCanary);
        assert.equal(suppliedCredentials.password, passwordCanary);
        for (const event of [
          "req-dh-params",
          "set-client-dh",
          "init-connection",
          "password-challenge",
          "password-check",
        ]) {
          assert.equal(prepared.progress.observeClientEvent(event), true);
        }
        assert.equal(prepared.progress.markSignedIn(), true);
        return prepared.progress.finish(async (method) => {
          logoutMethod = method;
          return null;
        });
      },
    },
  };
}

test("preflight runs every guard once in order, prints one fixed line, and never reads stdin", async () => {
  const fixture = createDependencies();
  const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);

  assert.deepEqual(result, {output: "browser acceptance preflight ready", exitCode: 0});
  assert.equal(fixture.launchPolicyCalls, 1);
  assert.equal(fixture.credentialReads, 0);
  assert.deepEqual(fixture.events, [
    "release-record",
    "progress-helper",
    "browser-launch-policy",
    "browser-launch",
    "sandbox",
    "network-guards",
    "served-manifest",
    "wss-101",
    "asset-loading",
    "client-progress",
    "negative-targets",
    "close-context",
  ]);
});

test("preflight fails closed without reading stdin at every pre-stdin checkpoint", async (t) => {
  for (const checkpoint of [
    "release-record",
    "progress-helper",
    "browser-launch-policy",
    "browser-launch",
    "sandbox",
    "network-guards",
    "served-manifest",
    "wss-101",
    "asset-loading",
    "client-progress",
    "negative-targets",
  ]) {
    await t.test(checkpoint, async () => {
      const fixture = createDependencies(checkpoint);
      const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);

      assert.deepEqual(result, {
        output: '{"status":"error","code":"unexpected-failure"}',
        exitCode: 1,
      });
      assert.equal(fixture.credentialReads, 0);
      assert.equal(fixture.events.includes("stdin"), false);
      assert.equal(fixture.launchPolicyCalls, fixture.events.includes("browser-launch-policy") ? 1 : 0);
      if (fixture.events.includes("sandbox")) assert.equal(fixture.events.at(-1), "close-context");
    });
  }
});

test("preflight preserves allow-listed codes from ordinary runtime adapter errors", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "acceptance-adapter-error-"));
  t.after(() => rm(directory, {recursive: true, force: true}));

  const releaseFixture = createDependencies();
  releaseFixture.dependencies.readReleaseRecord = readReleaseRecord;
  const releaseResult = await executeMode({
    mode: "preflight",
    releaseRecord: join(directory, "missing-release-record.json"),
  }, releaseFixture.dependencies);
  assert.deepEqual(releaseResult, {
    output: '{"status":"error","code":"release-record-invalid"}',
    exitCode: 1,
  });

  for (const code of ["sandbox-proof-invalid", "blocked-host-control-failed", "direct-egress-open",
    "observer-unhealthy", "target-address-invalid", "target-address-unavailable"]) {
    const sandboxFixture = createDependencies();
    sandboxFixture.dependencies.verifySandbox = () => {
      const error = new Error("runtime adapter check failed");
      error.code = code;
      throw error;
    };
    const sandboxResult = await executeMode({mode: "preflight", releaseRecord: "fixture"}, sandboxFixture.dependencies);
    assert.deepEqual(sandboxResult, {
      output: `{"status":"error","code":"${code}"}`,
      exitCode: 1,
    });
    assert.equal(sandboxFixture.credentialReads, 0);
  }

  const unknownFixture = createDependencies();
  unknownFixture.dependencies.verifySandbox = () => {
    const error = new Error("private adapter detail");
    error.code = "private-canary";
    throw error;
  };
  const unknownResult = await executeMode({mode: "preflight", releaseRecord: "fixture"}, unknownFixture.dependencies);
  assert.deepEqual(unknownResult, {
    output: '{"status":"error","code":"unexpected-failure"}',
    exitCode: 1,
  });
  assert.doesNotMatch(unknownResult.output, /private-canary|private adapter detail/u);
});

test("preflight reports an unexpected browser close with its own failure code", async () => {
  const fixture = createDependencies("browser-close-mid-check");
  const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);

  assert.deepEqual(result, {
    output: '{"status":"error","code":"browser-closed"}',
    exitCode: 1,
  });
  assert.equal(fixture.credentialReads, 0);
  assert.equal(fixture.events.includes("stdin"), false);
});

test("acceptance does not read credentials when a required preflight check, including WSS 101, fails", async (t) => {
  for (const checkpoint of ["sandbox", "served-manifest", "wss-101", "asset-loading", "negative-targets"]) {
    await t.test(checkpoint, async () => {
      const fixture = createDependencies(checkpoint);
      const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);

      assert.equal(fixture.credentialReads, 0);
      assert.equal(fixture.events.includes("stdin"), false);
      if (checkpoint === "wss-101") {
        const output = JSON.parse(result.output);
        assert.equal(output.schemaVersion, 2);
        assert.equal(output.phase, "wss-not-opened");
        assert.equal(output.ok, false);
        assert.doesNotMatch(result.output, /fixture-private-password|fixture_account_01/u);
      }
    });
  }
});

test("client observer records only bounded auth and in-band logout events", async () => {
  const progress = createAcceptanceProgress({sourceCommit});
  assert.equal(progress.observeWssConnection(), true);
  assert.equal(progress.observeWssHandshake(101), true);
  const bridgeEvents = [];
  const calls = [];
  const browserWindow = {
    apiManagerProxy: {
      async invoke(type, payload) {
        calls.push({type, name: payload.name, method: payload.method});
        const apiMethod = payload.args?.[0];
        if (apiMethod === "auth.signIn") throw {type: "SESSION_PASSWORD_NEEDED"};
        return null;
      },
    },
  };
  const page = {
    async exposeFunction(name, callback) {
      browserWindow[name] = (event) => {
        const serializedEvent = JSON.parse(JSON.stringify(event));
        bridgeEvents.push(serializedEvent);
        return callback(serializedEvent);
      };
    },
    async evaluate(callback, bridgeName) {
      return runInNewContext(`(${callback.toString()})(bridgeName)`, {window: browserWindow, bridgeName});
    },
  };

  const state = await installClientProgressObserver(page, progress, acceptanceRpcOutcome);
  await browserWindow.apiManagerProxy.invoke("manager", {
    name: "apiManager",
    method: "invokeApi",
    args: ["auth.sendCode", {phone_number: usernameCanary}],
  });
  await assert.rejects(
    browserWindow.apiManagerProxy.invoke("manager", {
      name: "apiManager",
      method: "invokeApi",
      args: ["auth.signIn", {phone_code: passwordCanary}],
    }),
    (error) => error.type === "SESSION_PASSWORD_NEEDED",
  );
  await browserWindow.apiManagerProxy.invoke("manager", {
    name: "passwordManager",
    method: "check",
    args: [passwordCanary],
  });
  await browserWindow.apiManagerProxy.invoke("manager", {
    name: "apiManager",
    method: "invokeApi",
    args: ["auth.logOut", {}, {ignoreErrors: true}],
    accountNumber: 1,
  });

  assert.deepEqual(state.authCalls, {"auth.sendCode": 1, "auth.signIn": 1, "auth.checkPassword": 1});
  assert.equal(state.logoutRpcCalls, 1);
  assert.equal(state.logoutRpcSuccesses, 1);
  assert.deepEqual(state.logoutRpcFailures, []);
  assert.equal(progress.markSignedIn(), true);
  const result = await progress.finish(async (method) => assert.equal(method, "auth.logOut"));
  assert.equal(result.ok, true);
  assert.deepEqual(bridgeEvents, [
    {kind: "transport-ready"},
    {kind: "auth-call", method: "auth.sendCode"},
    {kind: "auth-call", method: "auth.signIn"},
    {kind: "password-challenge"},
    {kind: "auth-call", method: "auth.checkPassword"},
    {kind: "logout-rpc-start"},
    {kind: "logout-rpc-success"},
  ]);
  assert.doesNotMatch(JSON.stringify(bridgeEvents), /fixture_account_01|fixture-private-password|phone_number|phone_code/u);
  assert.deepEqual(calls.map(({name, method}) => `${name}.${method}`), [
    "apiManager.invokeApi",
    "apiManager.invokeApi",
    "passwordManager.check",
    "apiManager.invokeApi",
  ]);
});

test("acceptance emits only the exact sanitized v2 result after all preflight guards", async () => {
  const fixture = createDependencies(undefined, true);
  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);

  assert.equal(result.exitCode, 0);
  assert.deepEqual(fixture.events, [
    "release-record",
    "progress-helper",
    "browser-launch-policy",
    "browser-launch",
    "sandbox",
    "network-guards",
    "served-manifest",
    "wss-101",
    "asset-loading",
    "client-progress",
    "negative-targets",
    "stdin",
    "acceptance",
    "close-context",
  ]);
  assert.equal(fixture.launchPolicyCalls, 1);
  assert.equal(fixture.credentialReads, 1);
  assert.equal(fixture.logoutMethod, "auth.logOut");
  assert.equal(fixture.credentials.username, "");
  assert.equal(fixture.credentials.password, "");

  const output = JSON.parse(result.output);
  assert.deepEqual(Object.keys(output).sort(), [
    "ok",
    "logout",
    "outcome",
    "phase",
    "schemaVersion",
    "sourceCommit",
    "targetHost",
    "wssConnections",
  ].sort());
  assert.deepEqual(output, {
    schemaVersion: 2,
    ok: true,
    phase: "signed-in",
    outcome: "ok",
    logout: "ok",
    targetHost,
    sourceCommit,
    wssConnections: 1,
  });
  assert.doesNotMatch(result.output, /fixture-private-password|fixture_account_01/u);
});

function fakeCdpSession({attachFails = false, replies = true, detach = "release", scriptLoaded = "after", log = []} = {}) {
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
      session.emit("Target.receivedMessageFromTarget", {sessionId, message: JSON.stringify(message)});
    },
    async send(method, params) {
      if (method === "Target.sendMessageToTarget") {
        const message = JSON.parse(params.message);
        sent.push({method: message.method, sessionId: params.sessionId});
        if (replies) {
          setImmediate(() => session.workerMessage(params.sessionId, {id: message.id, result: {}}));
          if (message.method === "Runtime.runIfWaitingForDebugger" && scriptLoaded === "after") {
            setImmediate(() => session.workerMessage(params.sessionId, {method: "Inspector.workerScriptLoaded", params: {}}));
          }
        }
        return {};
      }
      sent.push({method, params});
      log.push(method);
      if (method === "Target.attachToTarget") {
        if (attachFails) throw new Error("fixture attach failure");
        const sessionId = `observer-${params.targetId}`;
        // Chrome reports the script as loaded on attach when the worker already ran.
        if (scriptLoaded === "before") {
          setImmediate(() => session.workerMessage(sessionId, {method: "Inspector.workerScriptLoaded", params: {}}));
        }
        return {sessionId};
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

async function proveSharedWorkerWss(t, {socketUrl, pageSocket = false, childSocketUrl, delayedChild = false, workerSockets = [], ...sessionOptions}) {
  t.mock.timers.enable({apis: ["Date"]});
  const endpoint = `wss://${targetHost}/apiws`;
  const log = [];
  const browserSession = fakeCdpSession({...sessionOptions, log});
  const relatedSession = fakeCdpSession({log});
  const pageSession = fakeCdpSession(sessionOptions);
  const browserSessions = [browserSession, relatedSession];
  async function openChild() {
    if (childSocketUrl) {
      relatedSession.emit("Target.attachedToTarget", {
        sessionId: "paused-child",
        targetInfo: {targetId: "child", type: "worker"},
        waitingForDebugger: true,
      });
      for (let turn = 0; turn < 20; turn += 1) await new Promise((resolve) => setImmediate(resolve));
      browserSession.workerMessage("observer-child", {
        method: "Network.webSocketCreated",
        params: {requestId: "child-1", url: childSocketUrl},
      });
      browserSession.workerMessage("observer-child", {
        method: "Network.webSocketHandshakeResponseReceived",
        params: {requestId: "child-1", response: {status: 101}},
      });
    }
  }
  let waitedMs = 0;
  const page = {
    async goto() {
      browserSession.emit("Target.attachedToTarget", {
        sessionId: "paused-worker",
        targetInfo: {targetId: "worker", type: "shared_worker"},
        waitingForDebugger: true,
      });
      for (let turn = 0; turn < 20; turn += 1) await new Promise((resolve) => setImmediate(resolve));
      if (pageSocket) {
        pageSession.emit("Network.webSocketCreated", {requestId: "page-1", url: endpoint});
        pageSession.emit("Network.webSocketHandshakeResponseReceived", {requestId: "page-1", response: {status: 101}});
      }
      if (!delayedChild) await openChild();
      for (const [index, {url, statuses = [101]}] of workerSockets.entries()) {
        browserSession.workerMessage("observer-worker", {
          method: "Network.webSocketCreated",
          params: {requestId: `worker-socket-${index}`, url},
        });
        for (const status of statuses) {
          browserSession.workerMessage("observer-worker", {
            method: "Network.webSocketHandshakeResponseReceived",
            params: {requestId: `worker-socket-${index}`, response: {status}},
          });
        }
      }
      if (socketUrl) {
        browserSession.workerMessage("observer-worker", {
          method: "Network.webSocketCreated",
          params: {requestId: "worker-1", url: socketUrl},
        });
        browserSession.workerMessage("observer-worker", {
          method: "Network.webSocketHandshakeResponseReceived",
          params: {requestId: "worker-1", response: {status: 101}},
        });
      }
      return {status: () => 200};
    },
    async waitForTimeout(milliseconds) {
      waitedMs += milliseconds;
      if (delayedChild && waitedMs >= 250 && waitedMs - milliseconds < 250) await openChild();
      t.mock.timers.tick(milliseconds);
      await new Promise((resolve) => setImmediate(resolve));
    },
  };
  const contextInfo = {
    page,
    context: {
      async newCDPSession() { return pageSession; },
      browser() { return {async newBrowserCDPSession() { return browserSessions.shift(); }}; },
    },
  };
  const tracker = {telegramOrgAttemptCount: 0, telegramOrgWebSocketAttemptCount: 0, websocketMismatch: false};
  const progress = createAcceptanceProgress({sourceCommit});
  try {
    const result = await proveWss(contextInfo, {url: `https://${targetHost}/private/`}, {endpoint}, progress, tracker);
    return {result, log, progress, browserSession};
  } catch (error) {
    return {code: error.code, browserSession, relatedSession};
  }
}

test("preflight WSS proof observes the client's SharedWorker socket", async (t) => {
  const {result} = await proveSharedWorkerWss(t, {socketUrl: `wss://${targetHost}/apiws`});
  assert.deepEqual(result, {responseStatus: 200, connectionCount: 1, status101: true});
});

test("preflight WSS proof watches a worker's descendants before attaching to it", async (t) => {
  const {log} = await proveSharedWorkerWss(t, {socketUrl: `wss://${targetHost}/apiws`});
  assert.deepEqual(log.slice(0, 3), ["Target.setAutoAttach", "Target.autoAttachRelated", "Target.attachToTarget"]);
});

test("preflight WSS proof observes a socket opened by a worker's child worker", async (t) => {
  const {result} = await proveSharedWorkerWss(t, {childSocketUrl: `wss://${targetHost}/apiws`});
  assert.deepEqual(result, {responseStatus: 200, connectionCount: 1, status101: true});
});

test("preflight WSS proof fails closed for worker sockets elsewhere, silent workers, and unattachable workers", async (t) => {
  await t.test("another endpoint", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {socketUrl: `wss://${targetHost}/apiws-other`});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("no socket", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("attach failure", async (t) => {
    const {code, browserSession} = await proveSharedWorkerWss(t, {
      attachFails: true,
      socketUrl: `wss://${targetHost}/apiws`,
    });
    assert.equal(code, "websocket-not-ready");
    assert.deepEqual(browserSession.sent.at(-1), {
      method: "Target.detachFromTarget",
      params: {sessionId: "paused-worker"},
    });
  });
  await t.test("rejected release beside a valid page socket", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {detach: "reject", pageSocket: true});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("unanswered release beside a valid page socket", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {detach: "unanswered", pageSocket: true});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("a child worker's socket elsewhere beside a valid page socket", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {pageSocket: true, childSocketUrl: `wss://${targetHost}/apiws-other`});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("a worker that ran before observation, beside a valid page socket", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {scriptLoaded: "before", pageSocket: true});
    assert.equal(code, "websocket-not-ready");
  });
  await t.test("unanswered attach", async (t) => {
    const {code} = await proveSharedWorkerWss(t, {replies: false, socketUrl: `wss://${targetHost}/apiws`});
    assert.equal(code, "websocket-not-ready");
  });
});

// teagram-web opens one client socket per account slot on an unauthenticated load.
const WSS_CONNECTION_CEILING = 4;

function workerSockets(count, overrides = {}) {
  return Array.from({length: count}, (_, index) => ({url: `wss://${targetHost}/apiws`, ...overrides[index]}));
}

test("preflight WSS proof accepts every count from one through the client's socket ceiling", async (t) => {
  for (let count = 1; count <= WSS_CONNECTION_CEILING; count += 1) {
    await t.test(`${count} worker sockets`, async (t) => {
      const {result} = await proveSharedWorkerWss(t, {workerSockets: workerSockets(count)});
      assert.deepEqual(result, {responseStatus: 200, connectionCount: count, status101: true});
    });
  }
});

test("preflight WSS proof fails closed above the ceiling and on any bad connection at the ceiling", async (t) => {
  const cases = {
    "one more than the ceiling": {workerSockets: workerSockets(WSS_CONNECTION_CEILING + 1)},
    "a non-matching endpoint": {workerSockets: workerSockets(WSS_CONNECTION_CEILING, {3: {url: `wss://${targetHost}/apiws-other`}})},
    "a non-101 status": {workerSockets: workerSockets(WSS_CONNECTION_CEILING, {2: {statuses: [503]}})},
    "a duplicate handshake": {workerSockets: workerSockets(WSS_CONNECTION_CEILING, {1: {statuses: [101, 101]}})},
    "a missing handshake": {workerSockets: workerSockets(WSS_CONNECTION_CEILING, {0: {statuses: []}})},
    "an unobservable worker": {workerSockets: workerSockets(WSS_CONNECTION_CEILING), scriptLoaded: "before"},
    "a pending worker": {workerSockets: workerSockets(WSS_CONNECTION_CEILING), detach: "unanswered", pageSocket: true},
  };
  for (const [name, options] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const {code} = await proveSharedWorkerWss(t, options);
      assert.equal(code, "websocket-not-ready");
    });
  }
});

test("acceptance reports a successful sign-in over the client's socket ceiling", async () => {
  const fixture = createDependencies(undefined, true);
  fixture.dependencies.proveWss = async (_context, _release, _manifest, progress) => {
    fixture.events.push("wss-101");
    for (let index = 0; index < WSS_CONNECTION_CEILING; index += 1) progress.observeWssConnection();
    progress.observeWssHandshake(101);
    return {connectionCount: WSS_CONNECTION_CEILING, status101: true};
  };
  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);

  assert.equal(result.exitCode, 0);
  assert.equal(JSON.parse(result.output).ok, true);
  assert.equal(JSON.parse(result.output).wssConnections, WSS_CONNECTION_CEILING);
});

test("a socket above the ceiling opened during logout makes the run unsuccessful", async (t) => {
  const {result, progress, browserSession} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(WSS_CONNECTION_CEILING),
  });
  assert.equal(result.connectionCount, WSS_CONNECTION_CEILING);
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);

  const output = await progress.finish(async (method) => {
    assert.equal(method, "auth.logOut");
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: "worker-socket-logout", url: `wss://${targetHost}/apiws`},
    });
    return null;
  });

  assert.equal(output.logout, "ok");
  assert.equal(output.wssConnections, WSS_CONNECTION_CEILING + 1);
  assert.equal(output.ok, false);
});

test("preflight WSS proof rejects a nested mismatch discovered after the page handshake", async (t) => {
  const {code} = await proveSharedWorkerWss(t, {
    pageSocket: true,
    childSocketUrl: `wss://${targetHost}/apiws-other`,
    delayedChild: true,
  });
  assert.equal(code, "websocket-not-ready");
});
