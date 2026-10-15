import assert from "node:assert/strict";
import test from "node:test";

import { formatSmokeFailure } from "./readiness-integration-smoke.mjs";

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
