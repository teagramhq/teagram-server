import assert from "node:assert/strict";
import test from "node:test";

import * as smoke from "./readiness-integration-smoke.mjs";

const {
  assertMixedStatusError,
  assertQaWssProof,
  assertSilentSharedWorkerError,
  assertSocketTargetMismatch,
  captureQaWssProof,
  formatSmokeFailure,
} = smoke;

function silentSharedWorkerResult(diagnostic) {
  return { error: { code: "websocket-not-ready", diagnostic } };
}

test("mixed status diagnostics allow browser socket order to differ from upgrade order", () => {
  for (const [socket, failure] of [
    [1, "no-handshake"],
    [2, "no-handshake"],
    [1, "handshake-not-101"],
    [2, "handshake-not-101"],
  ]) {
    const noHandshake = failure === "no-handshake";
    const result = assertMixedStatusError(silentSharedWorkerResult({
      wss_diagnostic: `socket-${socket}-${failure}`,
      wss_targets: 2,
      wss_handshakes: noHandshake ? 1 : 2,
      wss_target_match: true,
      wss_status: noHandshake ? 0 : 503,
    }));
    assert.equal(result.wss_diagnostic, `socket-${socket}-${failure}`);
  }

  assert.throws(() => assertMixedStatusError(silentSharedWorkerResult({
    wss_diagnostic: "socket-1-no-handshake",
    wss_targets: 2,
    wss_handshakes: 0,
    wss_target_match: true,
    wss_status: 0,
  })));
});

test("mixed target diagnostics identify the mismatched socket with either response timing", () => {
  for (const diagnostic of [
    {
      wss_diagnostic: "socket-2-target-mismatch",
      wss_targets: 2,
      wss_handshakes: 1,
      wss_target_match: false,
      wss_status: 0,
    },
    {
      wss_diagnostic: "socket-2-target-mismatch",
      wss_targets: 2,
      wss_handshakes: 2,
      wss_target_match: false,
      wss_status: 101,
    },
  ]) {
    assert.equal(assertSocketTargetMismatch(silentSharedWorkerResult(diagnostic), 2, 2).wss_diagnostic,
      "socket-2-target-mismatch");
  }

  assert.throws(() => assertSocketTargetMismatch(silentSharedWorkerResult({
    wss_diagnostic: "socket-1-no-handshake",
    wss_targets: 2,
    wss_handshakes: 1,
    wss_target_match: false,
    wss_status: 0,
  }), 2, 2));
});

test("a silent SharedWorker accepts either no target or a safe unobservable result", () => {
  const noTarget = assertSilentSharedWorkerError(silentSharedWorkerResult({
    wss_diagnostic: "no-target",
    wss_targets: 0,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  }));
  assert.equal(noTarget.wss_diagnostic, "no-target");

  const unobservable = assertSilentSharedWorkerError(silentSharedWorkerResult({
    wss_diagnostic: "worker-unobservable",
    wss_targets: 0,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  }));
  assert.equal(unobservable.wss_diagnostic, "worker-unobservable");
});

test("a silent SharedWorker rejects any observed WebSocket target", () => {
  assert.throws(() => assertSilentSharedWorkerError(silentSharedWorkerResult({
    wss_diagnostic: "socket-1-no-handshake",
    wss_targets: 1,
    wss_handshakes: 0,
    wss_target_match: false,
    wss_status: 0,
  })));
});

test("smoke failures identify the scenario, expectation and both values", () => {
  let failure;
  try {
    assert.equal(undefined, "websocket-not-ready", "qaNestedWorkerMismatch.error?.code");
  } catch (error) {
    failure = error;
  }
  const line = formatSmokeFailure(failure, "qa-nested-worker-mismatch");
  assert.match(line, /scenario=qa-nested-worker-mismatch/u);
  assert.match(line, /expectation=qaNestedWorkerMismatch.error\?\.code/u);
  assert.match(line, /expected='websocket-not-ready'/u);
  assert.match(line, /observed=undefined/u);
  assert.equal(line.includes("\n"), false);
});

test("a failed QA WSS proof reports its original message and failed connection", async () => {
  assert.equal(typeof captureQaWssProof, "function");
  assert.equal(typeof assertQaWssProof, "function");
  const forcedError = new Error("forced proveWss failure");
  forcedError.code = "websocket-not-ready";
  forcedError.wssDiagnostic = { wss_diagnostic: "socket-4-no-handshake" };
  const proof = await captureQaWssProof(async () => { throw forcedError; });
  let failure;
  try {
    assertQaWssProof(proof, { connectionCount: 4, status101: true }, "qaPrivateClientWorker");
  } catch (error) {
    failure = error;
  }

  const line = formatSmokeFailure(failure, "qa-private-client-worker");
  assert.match(line, /forced proveWss failure/u);
  assert.match(line, /socket-4-no-handshake/u);
  assert.doesNotMatch(line, /result=undefined|observed=undefined/u);
});

test("timing failures preserve the bound and the failed predicate", () => {
  let failure;
  try {
    assert.ok(16_000 < 15_000, "missingWss.observationMs < 15_000");
  } catch (error) {
    failure = error;
  }
  const line = formatSmokeFailure(failure, "missing-wss");
  assert.match(line, /expectation=missingWss.observationMs < 15_000/u);
  assert.match(line, /expected=true observed=false/u);
});
