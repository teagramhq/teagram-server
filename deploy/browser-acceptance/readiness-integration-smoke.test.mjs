import assert from "node:assert/strict";
import test from "node:test";

import {
  assertSilentSharedWorkerError,
  assertSocketTargetMismatch,
  formatSmokeFailure,
} from "./readiness-integration-smoke.mjs";

function silentSharedWorkerResult(diagnostic) {
  return { error: { code: "websocket-not-ready", diagnostic } };
}

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
