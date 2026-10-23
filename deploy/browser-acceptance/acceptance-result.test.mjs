import assert from "node:assert/strict";
import test from "node:test";

import * as adapter from "./qa-runtime-adapter.mjs";

const sourceCommit = "c".repeat(40);

function createProgress() {
  assert.equal(typeof adapter.createAcceptanceProgress, "function");
  return adapter.createAcceptanceProgress({ sourceCommit });
}

function advanceTo(progress, phase) {
  progress.observeWssConnection();
  progress.observeWssHandshake(101);
  const events = [
    "req-pq",
    "req-dh-params",
    "set-client-dh",
    "init-connection",
    "password-challenge",
    "password-check",
  ];
  for (const event of events) {
    progress.observeClientEvent(event);
    if (event === phase) break;
  }
  if (phase === "signed-in") progress.markSignedIn();
}

test("client observations produce only the bounded acceptance phases", async () => {
  const cases = [
    { phase: "wss-not-opened", setup() {} },
    { phase: "wss-not-101", setup(progress) { progress.observeWssConnection(); } },
    { phase: "req-pq", setup(progress) { progress.observeWssConnection(); progress.observeWssHandshake(101); } },
    { phase: "req-dh-params", setup(progress) { advanceTo(progress, "req-dh-params"); } },
    { phase: "set-client-dh", setup(progress) { advanceTo(progress, "set-client-dh"); } },
    { phase: "init-connection", setup(progress) { advanceTo(progress, "init-connection"); } },
    { phase: "password-challenge", setup(progress) { advanceTo(progress, "password-challenge"); } },
    { phase: "password-check", setup(progress) { advanceTo(progress, "password-check"); } },
    { phase: "signed-in", setup(progress) { advanceTo(progress, "signed-in"); } },
  ];

  for (const { phase, setup } of cases) {
    const progress = createProgress();
    setup(progress);
    const result = await progress.finish(async (method) => {
      assert.equal(phase, "signed-in");
      assert.equal(method, "auth.logOut");
    });
    assert.equal(result.phase, phase);
    if (phase !== "signed-in") assert.equal(result.logout, "not-attempted");
  }
});

test("a successful sign-in logs out the current session before finish returns", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  const calls = [];
  const result = await progress.finish(async (method) => {
    calls.push(method);
  });
  calls.push("teardown");

  assert.deepEqual(calls, ["auth.logOut", "teardown"]);
  assert.deepEqual(result, {
    schemaVersion: 2,
    ok: true,
    phase: "signed-in",
    outcome: "ok",
    logout: "ok",
    targetHost: "teagram.tailaa4918.ts.net",
    sourceCommit,
    wssConnections: 1,
  });
});

// teagram-web opens four account-slot sockets on load, may replace one closed
// slot after sign-in, then reconnects the logged-out slot once at teardown.
const WSS_ACCOUNT_SLOT_COUNT = 4;
const WSS_CONNECTION_CEILING = WSS_ACCOUNT_SLOT_COUNT + 2;

test("sign-in with all four account-slot sockets succeeds", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  for (let index = 1; index < WSS_ACCOUNT_SLOT_COUNT; index += 1) progress.observeWssConnection();
  const result = await progress.finish(async () => null);

  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT);
  assert.equal(result.ok, true);
});

test("one logout reconnect after the four account-slot sockets succeeds", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  for (let index = 1; index < WSS_ACCOUNT_SLOT_COUNT; index += 1) progress.observeWssConnection();
  const result = await progress.finish(async (method) => {
    assert.equal(method, "auth.logOut");
    await Promise.resolve();
    progress.observeWssConnection();
  });

  assert.equal(result.phase, "signed-in");
  assert.equal(result.outcome, "ok");
  assert.equal(result.logout, "ok");
  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT + 1);
  assert.equal(result.ok, true);
  assert.equal(progress.observeWssConnection(), false);
  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT + 1);
});

test("only one reconnect can open during logout", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  for (let index = 1; index < WSS_ACCOUNT_SLOT_COUNT; index += 1) progress.observeWssConnection();
  const result = await progress.finish(async () => {
    assert.equal(progress.observeWssConnection(), true);
    assert.equal(progress.observeWssConnection(), false);
  });

  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT + 2);
  assert.equal(result.ok, false);
});

test("a reconnect can arrive before the close event for its authenticated slot", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  for (let index = 1; index < WSS_ACCOUNT_SLOT_COUNT; index += 1) progress.observeWssConnection();
  assert.equal(progress.observeWssConnection(), true);
  assert.equal(progress.observeWssClosed(), true);

  const result = await progress.finish(async () => null);
  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT + 1);
  assert.equal(result.ok, true);
});

test("a second sign-in reconnect exceeds the lifecycle budget", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  for (let index = 1; index < WSS_ACCOUNT_SLOT_COUNT; index += 1) progress.observeWssConnection();
  assert.equal(progress.observeWssConnection(), true);
  assert.equal(progress.observeWssClosed(), true);
  assert.equal(progress.observeWssConnection(), false);
  const result = await progress.finish(async () => null);

  assert.equal(result.logout, "ok");
  assert.equal(result.wssConnections, WSS_ACCOUNT_SLOT_COUNT + 2);
  assert.equal(result.ok, false);
});

test("WSS connection counts clamp one above the ceiling and an invalid event cannot advance the phase", async () => {
  const progress = createProgress();
  for (let index = 0; index < WSS_CONNECTION_CEILING + 3; index += 1) progress.observeWssConnection();
  progress.observeClientEvent("password-check");
  const result = await progress.finish();

  assert.equal(result.phase, "wss-not-101");
  assert.equal(result.wssConnections, WSS_CONNECTION_CEILING + 1);
  assert.equal(result.ok, false);
});

test("the fixed RPC error map requires an exact code and message pair", () => {
  const cases = [
    [{ error_code: 400, error_message: "PASSWORD_HASH_INVALID" }, "credential-rejected"],
    [{ error_code: 400, error_message: "SESSION_PASSWORD_NEEDED" }, "other"],
    [{ error_code: 401, error_message: "AUTH_KEY_UNREGISTERED" }, "auth-key-unregistered"],
    [{ error_code: 420, error_message: "FLOOD_WAIT_123456" }, "flood"],
    [{ error_code: 500, error_message: "INTERNAL" }, "internal"],
    [{ error_code: 400, error_message: "SRP_ID_INVALID" }, "other"],
    [{ error_code: 401, error_message: "PASSWORD_HASH_INVALID" }, "other"],
    [{ error_code: 420, error_message: "FLOOD_WAIT_1234567" }, "other"],
    [{ error_code: 500, error_message: "INTERNAL_EXTRA" }, "other"],
  ];

  assert.equal(typeof adapter.acceptanceRpcOutcome, "function");
  for (const [error, outcome] of cases) {
    assert.equal(adapter.acceptanceRpcOutcome(error), outcome);
  }
  assert.equal(adapter.acceptanceRpcOutcome(null), "other");
});

test("an RPC failure reports only its mapped outcome and never its raw message", async () => {
  const progress = createProgress();
  advanceTo(progress, "password-check");
  progress.recordRpcError({ error_code: 400, error_message: "PASSWORD_HASH_INVALID" });
  const result = await progress.finish();

  assert.equal(result.phase, "password-check");
  assert.equal(result.outcome, "credential-rejected");
  assert.equal(JSON.stringify(result).includes("PASSWORD_HASH_INVALID"), false);
});

test("non-RPC timeout and closed failures map to their fixed outcomes", async () => {
  const timeout = createProgress();
  timeout.recordClientFailure({ kind: "timeout" });
  assert.equal((await timeout.finish()).outcome, "timeout");

  const closed = createProgress();
  closed.recordClientFailure({ kind: "closed" });
  assert.equal((await closed.finish()).outcome, "closed");
});

test("logout errors are enum-only, one-shot, and never use credential-rejected", async () => {
  const cases = [
    [{ error_code: 401, error_message: "AUTH_KEY_UNREGISTERED" }, "auth-key-unregistered"],
    [{ error_code: 420, error_message: "FLOOD_WAIT_2" }, "flood"],
    [{ error_code: 500, error_message: "INTERNAL" }, "internal"],
    [{ error_code: 400, error_message: "PASSWORD_HASH_INVALID" }, "other"],
    [{ kind: "closed" }, "closed"],
    [{ kind: "timeout" }, "timeout"],
    [{ message: "private logout detail" }, "other"],
  ];

  for (const [error, expected] of cases) {
    const progress = createProgress();
    advanceTo(progress, "signed-in");
    let calls = 0;
    const result = await progress.finish(async (method) => {
      calls += 1;
      assert.equal(method, "auth.logOut");
      throw error;
    });
    await progress.finish(async () => { calls += 1; });
    assert.equal(calls, 1);
    assert.equal(result.logout, expected);
    assert.equal(JSON.stringify(result).includes("private logout detail"), false);
    assert.equal(result.ok, false);
  }
});

test("a non-reply from auth.logOut becomes timeout before finish returns", async () => {
  const progress = createProgress();
  advanceTo(progress, "signed-in");
  let calls = 0;
  const result = await progress.finish(async (method) => {
    calls += 1;
    assert.equal(method, "auth.logOut");
    return new Promise(() => {});
  }, { logoutTimeoutMs: 1 });

  assert.equal(calls, 1);
  assert.equal(result.logout, "timeout");
  assert.equal(result.ok, false);
});

test("an auth.logOut rpc_error reply is mapped instead of reported as success", async () => {
  const cases = [
    [{ error_code: 400, error_message: "PASSWORD_HASH_INVALID" }, "other"],
    [{ error_code: 401, error_message: "AUTH_KEY_UNREGISTERED" }, "auth-key-unregistered"],
    [{ error_code: 420, error_message: "FLOOD_WAIT_123456" }, "flood"],
    [{ error_code: 500, error_message: "INTERNAL" }, "internal"],
    [{ error_code: 401, error_message: "PASSWORD_HASH_INVALID" }, "other"],
    [{ error_code: 400, error_message: "SESSION_PASSWORD_NEEDED" }, "other"],
  ];

  for (const [rpcError, expected] of cases) {
    const progress = createProgress();
    advanceTo(progress, "signed-in");
    const result = await progress.finish(async (method) => {
      assert.equal(method, "auth.logOut");
      return { rpc_error: rpcError };
    });

    assert.equal(result.logout, expected);
    assert.equal(result.ok, false);
    assert.equal(JSON.stringify(result).includes(rpcError.error_message), false);
  }
});

test("failed sign-in never invokes logout and logs out is not-attempted", async () => {
  const progress = createProgress();
  advanceTo(progress, "password-check");
  progress.recordClientFailure({ kind: "closed" });
  let calls = 0;
  const result = await progress.finish(async () => { calls += 1; });

  assert.equal(calls, 0);
  assert.equal(result.phase, "password-check");
  assert.equal(result.outcome, "closed");
  assert.equal(result.logout, "not-attempted");
  assert.equal(result.ok, false);
});
