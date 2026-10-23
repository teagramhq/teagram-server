import assert from "node:assert/strict";
import {mkdtemp, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import test from "node:test";
import {runInNewContext} from "node:vm";

import {acceptanceRpcOutcome, createAcceptanceProgress} from "./acceptance-result.mjs";
import {executeMode, installClientProgressObserver, proveWss} from "./private-browser-acceptance-v2.mjs";
import {observerSnapshot, readReleaseRecord, targetAddress} from "./qa-runtime-adapter.mjs";

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
  let wssAuditValidationCalls = 0;
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
    get wssAuditValidationCalls() { return wssAuditValidationCalls; },
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
      },
      async readManifestThroughChromium() {
        step("served-manifest");
        return manifest;
      },
      async proveWss(contextInfo, _release, _manifest, progress) {
        step("wss-101");
        progress.observeWssConnection();
        progress.observeWssHandshake(101);
        contextInfo.wssAudit = {validate() { wssAuditValidationCalls += 1; }};
        contextInfo.wssAudit.validate();
        return {responseStatus: 200, connectionCount: 1, status101: true};
      },
      async verifyAssetLoading(contextInfo) {
        step("asset-loading");
        contextInfo.wssAudit.validate();
      },
      async installClientProgressObserver(_page, _progress, _classifier, state) {
        step("client-progress");
        return state;
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
        prepared.progressState.submission = "password";
        prepared.progressState.chatList = "rendered";
        prepared.progressState.chatListRows = 0;
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

function createAcceptancePage({
  usernameVisible = true,
  passwordVisible = true,
  chatListVisible = true,
  chatListMatches = 1,
  emptyChatStateVisible = false,
  chatRows = 0,
  visibleChatRows = chatRows,
  createAccountLinkVisible = false,
  signupFormVisible = false,
  signupTitleText = "YourName",
  stopAtUsernameFocus = false,
  completeSignIn = false,
  onLogout = () => {},
} = {}) {
  const apiMethods = [];
  let usernameFocused = false;
  let passwordFocused = false;
  const browserWindow = {
    apiManagerProxy: {
      async invoke(_type, payload) {
        apiMethods.push(payload.args?.[0]);
        if (completeSignIn && payload.args?.[0] === "auth.signIn") {
          const error = new Error("fixture password challenge");
          error.type = "SESSION_PASSWORD_NEEDED";
          throw error;
        }
        if (payload.args?.[0] === "auth.logOut") queueMicrotask(onLogout);
        return null;
      },
    },
  };
  let progressCallback;

  function locator(selector, scope, {visibleOnly = false, firstOnly = false} = {}) {
    const isUsername = selector === 'input[aria-label="Username"]';
    const isPassword = selector === 'input[name="notsearch_password"]:visible';
    const isChatsPage = selector === "#page-chats";
    const isChatList = selector === "#chatlist-container .chatlist";
    const isEmptyChatState = selector === "empty-chat-state";
    const isChatRow = selector === "#chatlist-container .chatlist-chat" || selector === ".chatlist-chat";
    const isSignupText = selector === "signup-text";
    const isSignupAvatar = selector === "#canvas-avatar";
    const isSignupHeader = scope === "signup-avatar" && selector === "xpath=../../..";
    const isSignupTitle = scope === "signup-header" && selector === ".text-overflow-wrap";
    const isVisible = () => isUsername ? usernameVisible
      : isPassword ? passwordVisible
        : isChatsPage ? true
          : isChatList ? chatListVisible
            : isEmptyChatState ? emptyChatStateVisible
              : isSignupText ? createAccountLinkVisible || signupFormVisible
              : isSignupAvatar || isSignupHeader ? signupFormVisible
                : isSignupTitle ? signupFormVisible && signupTitleText.length > 0 : false;
    return {
      first() { return locator(selector, scope, {visibleOnly, firstOnly: true}); },
      filter(options = {}) {
        if (isChatList && options.visible === true) {
          return locator(selector, scope, {visibleOnly: true});
        }
        return this;
      },
      locator(childSelector) {
        if (isSignupAvatar && childSelector === "xpath=../../..") return locator(childSelector, "signup-avatar");
        if (isSignupHeader) return locator(childSelector, "signup-header");
        if (isChatList && childSelector === ".chatlist-chat") {
          return locator(childSelector, "visible-chat-list", {visibleOnly, firstOnly});
        }
        return locator(childSelector);
      },
      getByText(text, options = {}) {
        if (isChatsPage && text === "Your chats will appear here" && options.exact === true) {
          return locator("empty-chat-state");
        }
        return locator("unknown");
      },
      async waitFor() {
        const matches = isChatList
          ? (visibleOnly ? Number(chatListVisible) : chatListMatches)
          : 1;
        if (!firstOnly && matches > 1) throw new Error("fixture strict mode violation");
        if (!isVisible()) throw new Error("fixture element is not visible");
      },
      async isVisible() { return isVisible(); },
      async count() {
        if (isChatList) {
          const matches = visibleOnly ? Number(chatListVisible) : chatListMatches;
          return firstOnly ? Math.min(matches, 1) : matches;
        }
        if (isChatRow) return scope === "visible-chat-list" ? visibleChatRows : chatRows;
        return 0;
      },
      async focus() {
        if (isUsername) {
          usernameFocused = true;
          if (stopAtUsernameFocus) throw new Error("fixture stopped before sign-in");
        }
        if (isPassword) passwordFocused = true;
      },
    };
  }

  const page = {
    on() {},
    locator,
    getByText() { return locator("signup-text"); },
    getByRole() { return locator("unknown"); },
    keyboard: {
      async type() {},
      async press(key) {
        if (!completeSignIn || key !== "Enter") return;
        if (passwordFocused) {
          await browserWindow.apiManagerProxy.invoke("manager", {
            name: "passwordManager",
            method: "check",
            args: ["auth.checkPassword"],
          });
          return;
        }
        if (usernameFocused) {
          await browserWindow.apiManagerProxy.invoke("manager", {
            name: "apiManager",
            method: "invokeApi",
            args: ["auth.sendCode", {}],
          });
          await assert.rejects(browserWindow.apiManagerProxy.invoke("manager", {
            name: "apiManager",
            method: "invokeApi",
            args: ["auth.signIn", {}],
          }), (error) => error.type === "SESSION_PASSWORD_NEEDED");
        }
      },
    },
    async exposeFunction(name, callback) {
      progressCallback = (event) => callback(JSON.parse(JSON.stringify(event)));
      browserWindow[name] = progressCallback;
    },
    async evaluate(callback, bridgeName) {
      if (bridgeName) {
        return runInNewContext(`(${callback.toString()})(bridgeName)`, {
          window: browserWindow,
          bridgeName,
        });
      }
      return runInNewContext(`(${callback.toString()})()`, {window: browserWindow});
    },
  };

  return {
    page,
    apiMethods,
    get usernameFocused() { return usernameFocused; },
    async invokeApi(method) {
      return browserWindow.apiManagerProxy.invoke("manager", {
        name: "apiManager",
        method: "invokeApi",
        args: [method, {}],
      });
    },
    emitProgressEvent(event) {
      assert.equal(typeof progressCallback, "function");
      return progressCallback(event);
    },
  };
}

async function useRealAcceptanceFlow(fixture, browserFixture) {
  const {performAcceptance} = await import("./private-browser-acceptance-v2.mjs");
  assert.equal(typeof performAcceptance, "function");
  fixture.dependencies.launchContext = async () => ({
    page: browserFixture.page,
    context: {on() {}},
  });
  fixture.dependencies.installClientProgressObserver = async (page, progress, classifier, state) => {
    const progressState = await installClientProgressObserver(page, progress, classifier, state);
    progressState.authFailureWaiters = [];
    progressState.waitForAuthFailure = (timeoutMs) => new Promise((resolveWaiter) => {
      const timer = setTimeout(() => resolveWaiter({kind: "timeout"}), timeoutMs);
      progressState.authFailureWaiters.push((result) => {
        clearTimeout(timer);
        resolveWaiter(result);
      });
    });
    progressState.cancelAuthFailureWaiters = () => {
      const waiters = progressState.authFailureWaiters.splice(0);
      for (const resolveWaiter of waiters) resolveWaiter({kind: "cancelled"});
    };
    return progressState;
  };
  fixture.dependencies.performAcceptance = (prepared, credentials) => {
    prepared.dependencies = fixture.dependencies;
    return performAcceptance(prepared, credentials);
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

test("preflight emits a validated WSS diagnostic without reading credentials", async () => {
  const fixture = createDependencies();
  fixture.dependencies.proveWss = () => {
    const error = new Error("private websocket detail");
    error.code = "websocket-not-ready";
    error.wssDiagnostic = {
      wss_diagnostic: "socket-2-target-mismatch",
      wss_targets: 2,
      wss_handshakes: 2,
      wss_target_match: false,
      wss_status: 101,
    };
    throw error;
  };

  const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);

  assert.deepEqual(result, {
    output: '{"status":"error","code":"websocket-not-ready","wss_diagnostic":"socket-2-target-mismatch","wss_targets":2,"wss_handshakes":2,"wss_target_match":false,"wss_status":101}',
    exitCode: 1,
  });
  assert.equal(fixture.credentialReads, 0);
});

test("preflight preserves codes from real observer and target address failures", async (t) => {
  await t.test("observer fetch failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => {
      throw new Error("private observer transport detail");
    });
    const fixture = createDependencies();
    fixture.dependencies.browserLaunchPolicy = observerSnapshot;
    const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);
    assert.deepEqual(result, {
      output: '{"status":"error","code":"observer-unhealthy"}',
      exitCode: 1,
    });
  });

  await t.test("observer JSON parse failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response("{", {status: 200}));
    const fixture = createDependencies();
    fixture.dependencies.browserLaunchPolicy = observerSnapshot;
    const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);
    assert.deepEqual(result, {
      output: '{"status":"error","code":"observer-unhealthy"}',
      exitCode: 1,
    });
  });

  await t.test("target address fetch failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => {
      throw new Error("private address transport detail");
    });
    const fixture = createDependencies();
    fixture.dependencies.browserLaunchPolicy = targetAddress;
    const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);
    assert.deepEqual(result, {
      output: '{"status":"error","code":"target-address-unavailable"}',
      exitCode: 1,
    });
  });

  await t.test("target address JSON parse failure", async (t) => {
    t.mock.method(globalThis, "fetch", async () => new Response("{", {status: 200}));
    const fixture = createDependencies();
    fixture.dependencies.browserLaunchPolicy = targetAddress;
    const result = await executeMode({mode: "preflight", releaseRecord: "fixture"}, fixture.dependencies);
    assert.deepEqual(result, {
      output: '{"status":"error","code":"target-address-invalid"}',
      exitCode: 1,
    });
  });
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

test("preflight reports a page crash during a check as a browser failure", async () => {
  const fixture = createDependencies();
  let emitPageCrash;
  fixture.dependencies.launchContext = async () => ({
    page: {
      on(event, listener) {
        if (event === "crash") emitPageCrash = listener;
      },
    },
    context: {on() {}},
  });
  fixture.dependencies.proveWss = async () => {
    emitPageCrash?.();
    throw new Error("Page crashed");
  };
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
      const output = JSON.parse(result.output);
      assert.equal(output.schemaVersion, 3);
      assert.equal(output.qaFailureCode, "unexpected-failure");
      if (checkpoint === "wss-101") {
        assert.equal(output.phase, "wss-not-opened");
        assert.equal(output.ok, false);
        assert.doesNotMatch(result.output, /fixture-private-password|fixture_account_01/u);
      }
    });
  }
});

test("a prepared QA failure preserves its allow-listed failure code", async () => {
  const fixture = createDependencies("sandbox");
  fixture.dependencies.verifySandbox = () => {
    const error = new Error("fixture sandbox failure");
    error.code = "sandbox-proof-invalid";
    throw error;
  };

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "sandbox-proof-invalid");
  assert.equal(output.phase, "wss-not-opened");
  assert.equal(fixture.credentialReads, 0);
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

test("client observer blocks auth.signUp and latches the signup attempt", async () => {
  const calls = [];
  const browserWindow = {
    apiManagerProxy: {
      async invoke(...args) {
        calls.push(args);
        return null;
      },
    },
  };
  const page = {
    async exposeFunction(name, callback) {
      browserWindow[name] = (event) => callback(JSON.parse(JSON.stringify(event)));
    },
    async evaluate(callback, bridgeName) {
      return runInNewContext(`(${callback.toString()})(bridgeName)`, {window: browserWindow, bridgeName});
    },
  };
  const progress = createAcceptanceProgress({sourceCommit});
  assert.equal(progress.observeWssConnection(), true);
  assert.equal(progress.observeWssHandshake(101), true);
  const state = await installClientProgressObserver(page, progress, acceptanceRpcOutcome);

  await assert.rejects(
    browserWindow.apiManagerProxy.invoke("manager", {
      name: "apiManager",
      method: "invokeApi",
      args: ["auth.signUp", {phone_number: usernameCanary}],
    }),
    (error) => error.code === "signup-attempted",
  );

  assert.deepEqual(calls, []);
  assert.equal(state.qaFailureCode, "signup-attempted");
});

test("acceptance emits only the exact sanitized v3 result after all preflight guards", async () => {
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
    "chatList",
    "chatListRows",
    "ok",
    "logout",
    "outcome",
    "phase",
    "qaFailureCode",
    "schemaVersion",
    "sourceCommit",
    "submission",
    "targetHost",
    "wssEndpointMatched",
    "wssConnections",
  ].sort());
  assert.deepEqual(output, {
    schemaVersion: 3,
    ok: true,
    phase: "signed-in",
    outcome: "ok",
    logout: "ok",
    targetHost,
    sourceCommit,
    wssConnections: 1,
    qaFailureCode: "none",
    submission: "password",
    chatList: "rendered",
    chatListRows: 0,
    wssEndpointMatched: true,
  });
  assert.doesNotMatch(result.output, /fixture-private-password|fixture_account_01/u);
});

test("username-field-missing reports no submission without making an auth call", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({usernameVisible: false});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "username-field-missing");
  assert.equal(output.submission, "none");
  assert.equal(output.chatList, "not-reached");
  assert.equal(output.chatListRows, 0);
  assert.deepEqual(browser.apiMethods, []);
});

test("a Create account link on the username sign-in screen does not trigger signup guard", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({createAccountLinkVisible: true, stopAtUsernameFocus: true});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "unexpected-failure");
  assert.equal(output.submission, "none");
  assert.equal(browser.usernameFocused, true);
  assert.deepEqual(browser.apiMethods, []);
});

test("active signup title div with YourName triggers signup guard before username entry", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({signupFormVisible: true});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "signup-path-shown");
  assert.equal(output.submission, "none");
  assert.equal(browser.usernameFocused, false);
  assert.deepEqual(browser.apiMethods, []);
});

test("active signup title div keeps triggering the guard after a name is entered", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({signupFormVisible: true, signupTitleText: "Ada Lovelace"});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "signup-path-shown");
  assert.equal(output.submission, "none");
  assert.equal(browser.usernameFocused, false);
  assert.deepEqual(browser.apiMethods, []);
});

test("a second auth-call reports the latched failure without repeating the call", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage();
  await useRealAcceptanceFlow(fixture, browser);
  fixture.dependencies.performAcceptance = async () => {
    await browser.invokeApi("auth.sendCode");
    let repeatedFailure;
    try {
      await browser.invokeApi("auth.sendCode");
    } catch (error) {
      repeatedFailure = error;
    }
    assert.ok(repeatedFailure);
    await assert.rejects(browser.emitProgressEvent({kind: "invalid"}));
    throw repeatedFailure;
  };

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "authentication-repeated");
  assert.equal(browser.apiMethods.length, 1);
  assert.deepEqual(browser.apiMethods, ["auth.sendCode"]);
});

test("client-progress-unavailable reaches the result line", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage();
  await useRealAcceptanceFlow(fixture, browser);
  fixture.dependencies.performAcceptance = async () => browser.emitProgressEvent({kind: "invalid"});

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "client-progress-unavailable");
  assert.deepEqual(browser.apiMethods, []);
});

test("broken-chat-list after sign-in is reported and attempts in-band logout", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({chatListVisible: false, completeSignIn: true});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "broken-chat-list");
  assert.equal(output.submission, "password");
  assert.equal(output.chatList, "missing");
  assert.equal(output.chatListRows, 0);
  assert.equal(output.phase, "signed-in");
  assert.equal(output.logout, "ok");
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("empty chats state is accepted as a rendered chat list after sign-in", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({
    chatListVisible: false,
    emptyChatStateVisible: true,
    completeSignIn: true,
  });
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 0, result.output);
  assert.equal(output.qaFailureCode, "none");
  assert.equal(output.phase, "signed-in");
  assert.equal(output.logout, "ok");
  assert.equal(output.chatList, "rendered");
  assert.equal(output.chatListRows, 0);
  assert.equal(fixture.wssAuditValidationCalls, 3);
  assert.deepEqual(browser.apiMethods.filter((method) => method === "auth.logOut"), ["auth.logOut"]);
});

test("socket-close events during context teardown do not invalidate the logout WSS proof", async (t) => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({completeSignIn: true});
  await useRealAcceptanceFlow(fixture, browser);
  let wssFixture;
  fixture.dependencies.proveWss = async (contextInfo, _release, _manifest, progress, tracker) => {
    wssFixture = await proveSharedWorkerWss(t, {
      workerSockets: workerSockets(4),
      contextInfo,
      progress,
      tracker,
    });
    return wssFixture.result;
  };
  fixture.dependencies.closeContext = async () => {
    fixture.events.push("close-context");
    for (let index = 0; index < 4; index += 1) {
      wssFixture.browserSession.workerMessage("observer-worker", {
        method: "Network.webSocketClosed",
        params: {requestId: `worker-socket-${index}`, timestamp: index + 1},
      });
    }
  };

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 0, result.output);
  assert.equal(output.ok, true);
  assert.equal(output.logout, "ok");
  assert.equal(output.wssConnections, 4);
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("one logout socket close before the teardown snapshot does not invalidate the WSS proof", async (t) => {
  const fixture = createDependencies(undefined, true);
  let wssFixture;
  const browser = createAcceptancePage({
    completeSignIn: true,
    onLogout() {
      fixture.events.push("logout-socket-close");
      wssFixture.browserSession.workerMessage("observer-worker", {
        method: "Network.webSocketClosed",
        params: {requestId: "worker-socket-0", timestamp: 1},
      });
    },
  });
  await useRealAcceptanceFlow(fixture, browser);
  fixture.dependencies.proveWss = async (contextInfo, _release, _manifest, progress, tracker) => {
    wssFixture = await proveSharedWorkerWss(t, {
      workerSockets: workerSockets(4),
      contextInfo,
      progress,
      tracker,
    });
    return wssFixture.result;
  };

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 0, result.output);
  assert.equal(output.ok, true);
  assert.equal(output.logout, "ok");
  assert.equal(output.wssConnections, 4);
  assert.ok(fixture.events.indexOf("logout-socket-close") < fixture.events.indexOf("close-context"));
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("logout reconnect created during asynchronous context teardown stays within the WSS budget", async (t) => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({completeSignIn: true});
  await useRealAcceptanceFlow(fixture, browser);
  let wssFixture;
  fixture.dependencies.proveWss = async (contextInfo, _release, _manifest, progress, tracker) => {
    wssFixture = await proveSharedWorkerWss(t, {
      workerSockets: workerSockets(4),
      contextInfo,
      progress,
      tracker,
    });
    return wssFixture.result;
  };
  fixture.dependencies.closeContext = async () => {
    fixture.events.push("close-context");
    await Promise.resolve();
    fixture.events.push("teardown-socket-close");
    wssFixture.browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketClosed",
      params: {requestId: "worker-socket-0", timestamp: 1},
    });
    fixture.events.push("teardown-reconnect");
    wssFixture.browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: "worker-socket-during-teardown", url: `wss://${targetHost}/apiws`},
    });
  };

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 0, result.output);
  assert.equal(output.ok, true);
  assert.equal(output.logout, "ok");
  assert.equal(output.wssConnections, 5);
  assert.ok(fixture.events.indexOf("close-context") < fixture.events.indexOf("teardown-reconnect"));
  assert.ok(fixture.events.indexOf("teardown-socket-close") < fixture.events.indexOf("teardown-reconnect"));
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("chat-list check ignores hidden list variants and scopes its row count", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({
    chatListMatches: 5,
    chatRows: 9,
    visibleChatRows: 0,
    completeSignIn: true,
  });
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.notEqual(output.qaFailureCode, "broken-chat-list");
  assert.equal(output.chatList, "rendered");
  assert.equal(output.chatListRows, 0);
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("chat-list row counts clamp at ten in the result line", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({chatRows: 13, completeSignIn: true});
  await useRealAcceptanceFlow(fixture, browser);

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 0);
  assert.equal(output.chatList, "rendered");
  assert.equal(output.chatListRows, 10);
  assert.equal(output.logout, "ok");
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
});

test("WSS mismatch is reported as unmatched in the result line", async () => {
  const fixture = createDependencies(undefined, true);
  const browser = createAcceptancePage({completeSignIn: true});
  await useRealAcceptanceFlow(fixture, browser);
  fixture.dependencies.installNetworkGuards = async () => ({
    expectedEndpoint: "",
    websocketCount: 0,
    websocketMismatch: true,
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
  });

  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);
  const output = JSON.parse(result.output);

  assert.equal(result.exitCode, 1);
  assert.equal(output.qaFailureCode, "endpoint-mismatch");
  assert.equal(output.wssEndpointMatched, false);
  assert.equal(output.phase, "signed-in");
  assert.equal(output.logout, "ok");
  assert.deepEqual(browser.apiMethods, ["auth.sendCode", "auth.signIn", "auth.checkPassword", "auth.logOut"]);
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

async function proveSharedWorkerWss(t, {
  socketUrl,
  pageSocket = false,
  childSocketUrl,
  delayedChild = false,
  workerSockets = [],
  contextInfo: suppliedContextInfo,
  progress: suppliedProgress,
  tracker: suppliedTracker,
  ...sessionOptions
}) {
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
  const contextInfo = suppliedContextInfo ?? {page, context: {}};
  if (suppliedContextInfo) Object.assign(contextInfo.page, page);
  Object.assign(contextInfo.context, {
    async newCDPSession() { return pageSession; },
    browser() { return {async newBrowserCDPSession() { return browserSessions.shift(); }}; },
  });
  const tracker = suppliedTracker ?? {
    telegramOrgAttemptCount: 0,
    telegramOrgWebSocketAttemptCount: 0,
    websocketMismatch: false,
  };
  const progress = suppliedProgress ?? createAcceptanceProgress({sourceCommit});
  try {
    const result = await proveWss(contextInfo, {url: `https://${targetHost}/private/`}, {endpoint}, progress, tracker);
    return {result, log, progress, browserSession, wssAudit: contextInfo.wssAudit, contextInfo};
  } catch (error) {
    return {code: error.code, wssDiagnostic: error.wssDiagnostic, browserSession, relatedSession};
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
    const {code, wssDiagnostic} = await proveSharedWorkerWss(t, {socketUrl: `wss://${targetHost}/apiws-other`});
    assert.equal(code, "websocket-not-ready");
    assert.deepEqual(wssDiagnostic, {
      wss_diagnostic: "socket-1-target-mismatch",
      wss_targets: 1,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 101,
    });
  });
  await t.test("no socket", async (t) => {
    const {code, wssDiagnostic} = await proveSharedWorkerWss(t, {});
    assert.equal(code, "websocket-not-ready");
    assert.deepEqual(wssDiagnostic, {
      wss_diagnostic: "no-target",
      wss_targets: 0,
      wss_handshakes: 0,
      wss_target_match: false,
      wss_status: 0,
    });
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
    const {code, wssDiagnostic} = await proveSharedWorkerWss(t, {pageSocket: true, childSocketUrl: `wss://${targetHost}/apiws-other`});
    assert.equal(code, "websocket-not-ready");
    assert.deepEqual(wssDiagnostic, {
      wss_diagnostic: "socket-2-target-mismatch",
      wss_targets: 2,
      wss_handshakes: 2,
      wss_target_match: false,
      wss_status: 101,
    });
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
const WSS_ACCOUNT_SLOT_CEILING = 4;

function workerSockets(count, overrides = {}) {
  return Array.from({length: count}, (_, index) => ({url: `wss://${targetHost}/apiws`, ...overrides[index]}));
}

test("preflight WSS proof accepts every count from one through the client's socket ceiling", async (t) => {
  for (let count = 1; count <= WSS_ACCOUNT_SLOT_CEILING; count += 1) {
    await t.test(`${count} worker sockets`, async (t) => {
      const {result} = await proveSharedWorkerWss(t, {workerSockets: workerSockets(count)});
      assert.deepEqual(result, {responseStatus: 200, connectionCount: count, status101: true});
    });
  }
});

test("preflight WSS proof fails closed above the ceiling and on any bad connection at the ceiling", async (t) => {
  const cases = {
    "one more than the account-slot ceiling": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING + 1)},
    "a non-matching endpoint": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING, {3: {url: `wss://${targetHost}/apiws-other`}})},
    "a non-101 status": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING, {2: {statuses: [503]}})},
    "a duplicate handshake": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING, {1: {statuses: [101, 101]}})},
    "a missing handshake": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING, {0: {statuses: []}})},
    "an unobservable worker": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING), scriptLoaded: "before"},
    "a pending worker": {workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING), detach: "unanswered", pageSocket: true},
  };
  for (const [name, options] of Object.entries(cases)) {
    await t.test(name, async (t) => {
      const {code} = await proveSharedWorkerWss(t, options);
      assert.equal(code, "websocket-not-ready");
    });
  }
});

test("acceptance reports a successful sign-in with all four account slots", async () => {
  const fixture = createDependencies(undefined, true);
  fixture.dependencies.proveWss = async (contextInfo, _release, _manifest, progress) => {
    fixture.events.push("wss-101");
    for (let index = 0; index < WSS_ACCOUNT_SLOT_CEILING; index += 1) progress.observeWssConnection();
    progress.observeWssHandshake(101);
    contextInfo.wssAudit = {validate() {}};
    return {responseStatus: 200, connectionCount: WSS_ACCOUNT_SLOT_CEILING, status101: true};
  };
  const result = await executeMode({mode: "acceptance", releaseRecord: "fixture"}, fixture.dependencies);

  assert.equal(result.exitCode, 0);
  assert.equal(JSON.parse(result.output).ok, true);
  assert.equal(JSON.parse(result.output).wssConnections, WSS_ACCOUNT_SLOT_CEILING);
});

test("one logout reconnect after the four account-slot sockets is accepted", async (t) => {
  const {result, progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  assert.equal(result.connectionCount, 4);
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());

  const output = await progress.finish(async (method) => {
    assert.equal(method, "auth.logOut");
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: "worker-socket-logout", url: `wss://${targetHost}/apiws`},
    });
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketHandshakeResponseReceived",
      params: {requestId: "worker-socket-logout", response: {status: 101}},
    });
    return null;
  });

  assert.equal(output.logout, "ok");
  assert.equal(output.wssConnections, 5);
  assert.equal(output.ok, true);
  assert.doesNotThrow(() => wssAudit.validateAfterLogout());
});

test("WSS slots opened after the initial settle are pinned at sign-in", async (t) => {
  const {result, progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(2),
  });
  assert.equal(result.connectionCount, 2);

  for (let index = 2; index < WSS_ACCOUNT_SLOT_CEILING; index += 1) {
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: `worker-socket-${index}`, url: `wss://${targetHost}/apiws`},
    });
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketHandshakeResponseReceived",
      params: {requestId: `worker-socket-${index}`, response: {status: 101}},
    });
  }

  assert.doesNotThrow(() => wssAudit.validate());
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());
});

test("a fifth active WSS socket before the first signed-in validation fails", async (t) => {
  const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(WSS_ACCOUNT_SLOT_CEILING),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: {requestId: "worker-socket-before-baseline-validation", url: `wss://${targetHost}/apiws`},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: {requestId: "worker-socket-before-baseline-validation", response: {status: 101}},
  });

  assert.throws(() => wssAudit.validate(), {code: "websocket-not-ready"});
});

test("logout WSS validation retains the one-extra-active-socket limit", async (t) => {
  const {browserSession, progress, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());
  wssAudit.beginLogout();
  wssAudit.beginContextTeardown();

  for (let index = 0; index < 2; index += 1) {
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: `worker-socket-logout-${index}`, url: `wss://${targetHost}/apiws`},
    });
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketHandshakeResponseReceived",
      params: {requestId: `worker-socket-logout-${index}`, response: {status: 101}},
    });
  }

  assert.throws(() => wssAudit.validateAfterLogout(), {code: "websocket-not-ready"});
});

test("a post-sign-in reconnect replaces a closed account-slot socket", async (t) => {
  const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());

  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: {requestId: "worker-socket-after-sign-in", url: `wss://${targetHost}/apiws`},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: {requestId: "worker-socket-after-sign-in", response: {status: 101}},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketClosed",
    params: {requestId: "worker-socket-0", timestamp: 1},
  });

  assert.doesNotThrow(() => wssAudit.validate());
  const output = await progress.finish(async () => null);
  assert.equal(output.wssConnections, 5);
  assert.equal(output.ok, true);
});

test("a post-sign-in socket close without a replacement fails the WSS proof", async (t) => {
  const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);

  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketClosed",
    params: {requestId: "worker-socket-0", timestamp: 1},
  });

  assert.throws(() => wssAudit.validate(), {code: "websocket-not-ready"});
});

test("a sign-in slot replacement and the logout reconnect both fit the observed lifecycle", async (t) => {
  const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());

  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: {requestId: "worker-socket-after-sign-in", url: `wss://${targetHost}/apiws`},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: {requestId: "worker-socket-after-sign-in", response: {status: 101}},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketClosed",
    params: {requestId: "worker-socket-0", timestamp: 1},
  });
  assert.doesNotThrow(() => wssAudit.validate());

  const output = await progress.finish(async () => {
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketCreated",
      params: {requestId: "worker-socket-logout", url: `wss://${targetHost}/apiws`},
    });
    browserSession.workerMessage("observer-worker", {
      method: "Network.webSocketHandshakeResponseReceived",
      params: {requestId: "worker-socket-logout", response: {status: 101}},
    });
    return null;
  });

  assert.equal(output.wssConnections, 6);
  assert.equal(output.ok, true);
  assert.doesNotThrow(() => wssAudit.validateAfterLogout());
});

test("a logout reconnect must use the validated endpoint and may be observed before its handshake", async (t) => {
  for (const scenario of [
    {name: "matching opening without a response", url: `wss://${targetHost}/apiws`, status: null, valid: true},
    {name: "a different endpoint", url: `wss://${targetHost}/apiws-other`, status: 101, valid: false},
    {name: "a rejected handshake", url: `wss://${targetHost}/apiws`, status: 503, valid: false},
  ]) {
    await t.test(scenario.name, async (t) => {
      const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
        workerSockets: workerSockets(4),
      });
      for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
        assert.equal(progress.observeClientEvent(event), true);
      }
      assert.equal(progress.markSignedIn(), true);
      assert.doesNotThrow(() => wssAudit.validate());

      await progress.finish(async () => {
        browserSession.workerMessage("observer-worker", {
          method: "Network.webSocketCreated",
          params: {requestId: "worker-socket-logout", url: scenario.url},
        });
        if (scenario.status !== null) {
          browserSession.workerMessage("observer-worker", {
            method: "Network.webSocketHandshakeResponseReceived",
            params: {requestId: "worker-socket-logout", response: {status: scenario.status}},
          });
        }
        return null;
      });

      if (scenario.valid) {
        assert.doesNotThrow(() => wssAudit.validateAfterLogout());
      } else {
        assert.throws(() => wssAudit.validateAfterLogout(), {code: "websocket-not-ready"});
      }
    });
  }
});

test("a fifth concurrently active socket before logout fails the WSS proof", async (t) => {
  const {progress, browserSession, wssAudit} = await proveSharedWorkerWss(t, {
    workerSockets: workerSockets(4),
  });
  for (const event of ["req-dh-params", "set-client-dh", "init-connection", "password-challenge", "password-check"]) {
    assert.equal(progress.observeClientEvent(event), true);
  }
  assert.equal(progress.markSignedIn(), true);
  assert.doesNotThrow(() => wssAudit.validate());
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketCreated",
    params: {requestId: "worker-socket-before-logout", url: `wss://${targetHost}/apiws`},
  });
  browserSession.workerMessage("observer-worker", {
    method: "Network.webSocketHandshakeResponseReceived",
    params: {requestId: "worker-socket-before-logout", response: {status: 101}},
  });

  assert.throws(() => wssAudit.validate(), {code: "websocket-not-ready"});
  await progress.finish(async () => null);
});

test("preflight WSS proof rejects a nested mismatch discovered after the page handshake", async (t) => {
  const {code} = await proveSharedWorkerWss(t, {
    pageSocket: true,
    childSocketUrl: `wss://${targetHost}/apiws-other`,
    delayedChild: true,
  });
  assert.equal(code, "websocket-not-ready");
});
