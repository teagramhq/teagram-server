import assert from "node:assert/strict";
import test from "node:test";

import { probeAcceptance } from "./host-probe.mjs";
const revision = "c".repeat(40);
const fingerprint = "0123456789abcdef";
const imageId = `sha256:${"a".repeat(64)}`;
const serverId = "b".repeat(64);
const browserId = "d".repeat(64);
const observerId = "e".repeat(64);
const start = "1791878400000000";

function server(overrides = {}) {
  return {
    id: serverId, imageId, imageRevision: revision, revision,
    startedAt: "2026-10-13T00:00:00Z", running: true,
    flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`],
    ...overrides,
  };
}

function bindingSnapshot(ids, pending = 0, overrides = {}) {
  return {
    complete: true,
    evidence_version: 1,
    epoch: "9",
    additions: "10",
    removals: "8",
    last_added_created_at: "1791878399000000",
    bound_key_ids: ids.map(String),
    bound_key_created_ats: ids.map(() => "1791878399000000"),
    pending_count: String(pending),
    db_time: start,
    ...overrides,
  };
}

function successfulCut(ids = [1, 2], pending = 0, overrides = {}) {
  return bindingSnapshot(ids, pending, {
    additions: "11",
    removals: "9",
    last_added_created_at: "1791878400000001",
    db_time: "1791878400000002",
    ...overrides,
  });
}

function fixture({ runtimes = [[server()], [server()]], networks, snapshots, target = fingerprint, browserStatus = 0 } = {}) {
  const events = [];
  networks ??= [{ internal: true, members: [browserId, observerId] }, { internal: true, members: [browserId, observerId] }];
  snapshots ??= [bindingSnapshot([1, 2]), successfulCut()];
  let snapshotIndex = 0;
  const io = {
    async runtimeSnapshot() { events.push("runtime"); return runtimes.shift(); },
    async prepareBrowser() { events.push("prepare-browser"); return { browserId, observerId }; },
    async networkSnapshot() { events.push("network"); return networks.shift(); },
    async targetFingerprint() { events.push("tls"); return target; },
    async openDatabase() {
      events.push("protected-files");
      return async () => {
        events.push("bindings");
        const snapshot = snapshots[snapshotIndex++];
        return snapshot;
      };
    },
    launchBrowser() {
      events.push("stdin-forwarded");
      return Promise.resolve(browserStatus);
    },
  };
  return { io, events };
}

async function probe(f) {
  assert.equal(typeof probeAcceptance, "function", "host-only acceptance proof is missing");
  return probeAcceptance(revision, f.io);
}

test("one newly created binding followed by in-band logout proves cleanup", async () => {
  const f = fixture();
  const result = await probe(f);
  assert.deepEqual(result, {
    schemaVersion: 1, ok: true, code: "ok",
    registrationClosed: true, loginCodesDisabled: true, revisionMatched: true,
    containersStable: true, networkIsolated: true, fingerprintMatched: true,
    newBindingProven: true, cleanupProven: true, browserExitOk: true,
    boundBefore: 2, boundAfter: 2, pendingBefore: 0, pendingAfter: 0,
  });
  assert.deepEqual(f.events, [
    "runtime", "prepare-browser", "network", "tls", "protected-files", "bindings",
    "stdin-forwarded", "runtime", "network", "tls", "bindings",
  ]);
  assert.equal(f.events.filter((event) => event === "bindings").length, 2,
    "one atomic API call must produce each cut; sampled legacy reads are not used");
  assert.doesNotMatch(JSON.stringify(result), /1791878400000000|1791878399000000|"epoch"|"additions"|"removals"/u);
});

for (const [name, flags] of [
  ["open registration", ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["unset registration", ["TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["login-code logging", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=true", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["unset login-code logging", ["TG_REGISTRATION=closed", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["case-altered flag", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=False", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["duplicate flag", ["TG_REGISTRATION=closed", "TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`]],
  ["unprojected secret", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${fingerprint}`, "TG_POSTGRES_DSN=private-canary"]],
]) {
  test(`${name} fails closed before stdin or protected files are read`, async () => {
    const f = fixture({ runtimes: [[server({ flags })]] });
    const result = await probe(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, "environment-unproven");
    assert.deepEqual(f.events, ["runtime"]);
    assert.doesNotMatch(JSON.stringify(result), /private-canary/u);
  });
}

test("every running replica must have closed registration and disabled logging", async () => {
  const f = fixture({ runtimes: [[server(), server({ id: "f".repeat(64), flags: ["TG_REGISTRATION=open"] })]] });
  assert.equal((await probe(f)).code, "environment-unproven");
  assert.deepEqual(f.events, ["runtime"]);
});

for (const [name, runtime] of [
  ["no running server", []],
  ["missing OCI label", [server({ revision: null })]],
  ["stale image label", [server({ imageRevision: "f".repeat(40) })]],
  ["malformed image ID", [server({ imageId: "telegramd:local" })]],
  ["wrong revision", [server({ revision: "f".repeat(40) })]],
]) {
  test(`${name} fails closed before browser execution`, async () => {
    const f = fixture({ runtimes: [runtime] });
    const result = await probe(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, runtime.length ? "revision-unproven" : "runtime-unavailable");
    assert.deepEqual(f.events, ["runtime"]);
  });
}

for (const [name, post] of [
  ["replacement container", server({ id: "f".repeat(64) })],
  ["restart of the same container", server({ startedAt: "2026-10-13T01:00:00Z" })],
  ["changed image", server({ imageId: `sha256:${"f".repeat(64)}` })],
  ["stopped container", server({ running: false })],
]) {
  test(`${name} prevents a pass after logout`, async () => {
    const result = await probe(fixture({ runtimes: [[server()], [post]] }));
    assert.equal(result.code, "runtime-changed");
    assert.equal(result.ok, false);
    assert.equal(result.containersStable, false);
  });
}

for (const before of [true, false]) {
  for (const network of [
    { internal: false, members: [browserId, observerId] },
    { internal: true, members: [browserId, observerId, "f".repeat(64)] },
    { internal: true, members: [observerId] },
    { internal: true, members: [browserId, browserId] },
  ]) {
    test(`network isolation is checked ${before ? "before" : "after"} credential forwarding: ${JSON.stringify(network)}`, async () => {
      const good = { internal: true, members: [browserId, observerId] };
      const f = fixture({ networks: before ? [network] : [good, network] });
      const result = await probe(f);
      assert.equal(result.code, "network-unproven");
      assert.equal(result.ok, false);
      if (before) assert.ok(!f.events.includes("stdin-forwarded"));
    });
  }
}

test("the host TLS fingerprint must equal the effective server fingerprint", async () => {
  const f = fixture({ target: "fedcba9876543210" });
  const result = await probe(f);
  assert.equal(result.code, "fingerprint-unproven");
  assert.equal(result.fingerprintMatched, false);
  assert.ok(!f.events.includes("stdin-forwarded"));
  assert.doesNotMatch(JSON.stringify(result), /0123456789abcdef|fedcba9876543210/u);
});

test("unset server fingerprint prevents credential forwarding", async () => {
  const f = fixture({ runtimes: [[server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false"] })]] });
  assert.equal((await probe(f)).code, "fingerprint-unproven");
  assert.ok(!f.events.includes("stdin-forwarded"));
});

test("a changed served fingerprint after logout prevents a pass", async () => {
  const f = fixture();
  let fetches = 0;
  f.io.targetFingerprint = async () => ++fetches === 1 ? fingerprint : "fedcba9876543210";
  const result = await probe(f);
  assert.equal(result.ok, false);
  assert.equal(result.code, "fingerprint-unproven");
});

for (const [name, final] of [
  ["two keys bind and log out between cuts", bindingSnapshot([1, 2], 0, {
    additions: "12", removals: "10", last_added_created_at: "1791878400000002",
  })],
  ["a pre-existing key is disturbed", successfulCut([1, 3])],
  ["failed logout leaves a new key bound", bindingSnapshot([1, 2, 3], 0, {
    additions: "11", removals: "8", last_added_created_at: "1791878400000001",
    bound_key_created_ats: ["1791878399000000", "1791878399000000", "1791878400000001"],
  })],
  ["a bound key predates the run cut", successfulCut([1, 2], 0, {
    last_added_created_at: "1791878399999999",
  })],
  ["the final database cut moves backward", successfulCut([1, 2], 0, {
    db_time: "1791878399999999",
  })],
  ["the witness is later than the final database cut", successfulCut([1, 2], 0, {
    last_added_created_at: "1791878400000003",
    db_time: "1791878400000002",
  })],
  ["sign-in stops mid-SRP", bindingSnapshot([1, 2], 1)],
  ["no binding transition occurred", bindingSnapshot([1, 2])],
  ["a pre-existing key is missing after logout", successfulCut([1])],
]) {
  test(`${name} is cleanup-unproven with no second sign-in attempt`, async () => {
    const f = fixture({ snapshots: [bindingSnapshot([1, 2]), final] });
    const result = await probe(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, "cleanup-unproven");
    assert.equal(f.events.filter((event) => event === "stdin-forwarded").length, 1);
    assert.equal(result.cleanupProven, false);
  });
}

test("pending sign-ins must return to baseline", async () => {
  const result = await probe(fixture({ snapshots: [
    bindingSnapshot([1, 2], 1), successfulCut([1, 2], 2),
  ] }));
  assert.equal(result.code, "cleanup-unproven");
});

test("expired baseline pending bindings may reduce the final count", async () => {
  const result = await probe(fixture({ snapshots: [
    bindingSnapshot([1, 2], 1), successfulCut([1, 2], 0),
  ] }));
  assert.equal(result.ok, true);
});

test("one new key that binds and logs out entirely between cuts passes", async () => {
  const f = fixture({ snapshots: [bindingSnapshot([1, 2]), successfulCut()] });
  const result = await probe(f);
  assert.equal(result.ok, true);
  assert.equal(result.newBindingProven, true);
  assert.equal(result.cleanupProven, true);
  assert.equal(f.events.filter((event) => event === "bindings").length, 2);
});

test("key IDs beyond JavaScript safe integers keep exact baseline identity", async () => {
  const ids = ["9007199254740993", "9223372036854775806"];
  const result = await probe(fixture({ snapshots: [bindingSnapshot(ids), successfulCut(ids)] }));
  assert.equal(result.ok, true);
});

test("DB errors after sign-in are redacted and still await the browser before exiting", async () => {
  const f = fixture();
  let finished = false;
  f.io.openDatabase = async () => {
    let reads = 0;
    return async () => {
      if (++reads === 1) return bindingSnapshot([1, 2]);
      throw new Error("private-db-credential-canary");
    };
  };
  f.io.launchBrowser = async () => { await Promise.resolve(); finished = true; return 1; };
  const result = await probe(f);
  assert.equal(result.code, "cleanup-unproven");
  assert.equal(finished, true);
  assert.doesNotMatch(JSON.stringify(result), /private-db-credential-canary/u);
});

test("count clamping cannot hide a lost baseline binding", async () => {
  const baseline = Array.from({ length: 101 }, (_, index) => index + 1);
  const result = await probe(fixture({ snapshots: [
    bindingSnapshot(baseline), successfulCut([...baseline.slice(1), 102]),
  ] }));
  assert.equal(result.boundBefore, 100);
  assert.equal(result.boundAfter, 100);
  assert.equal(result.code, "cleanup-unproven");
});

test("malformed DB metadata cannot be emitted or used as proof", async () => {
  const f = fixture();
  f.io.openDatabase = async () => async () => ({
    ...bindingSnapshot([1, 2]), detail: "private-snapshot-canary",
  });
  const result = await probe(f);
  assert.equal(result.code, "database-unavailable");
  assert.ok(!f.events.includes("stdin-forwarded"));
  assert.doesNotMatch(JSON.stringify(result), /private-snapshot-canary/u);
});

test("missing trigger coverage fails before browser credential forwarding", async () => {
  const incomplete = {
    complete: false, evidence_version: null, epoch: null, additions: null, removals: null,
    last_added_created_at: null, bound_key_ids: null, bound_key_created_ats: null,
    pending_count: null, db_time: null,
  };
  const f = fixture({ snapshots: [incomplete, successfulCut()] });
  const result = await probe(f);
  assert.equal(result.code, "database-unavailable");
  assert.ok(!f.events.includes("stdin-forwarded"));
});

test("unsupported evidence version fails before browser credential forwarding", async () => {
  const f = fixture({ snapshots: [bindingSnapshot([1, 2], 0, { evidence_version: 2 }), successfulCut()] });
  const result = await probe(f);
  assert.equal(result.code, "database-unavailable");
  assert.ok(!f.events.includes("stdin-forwarded"));
});

for (const [name, final] of [
  ["coverage is incomplete", {
    complete: false, evidence_version: null, epoch: null, additions: null, removals: null,
    last_added_created_at: null, bound_key_ids: null, bound_key_created_ats: null,
    pending_count: null, db_time: null,
  }],
  ["bound key arrays are malformed", successfulCut([1, 2], 0, { bound_key_created_ats: ["1791878399000000"] })],
  ["producer version changes", successfulCut([1, 2], 0, { evidence_version: 2 })],
  ["enrollment epoch changes", successfulCut([1, 2], 0, { epoch: "10" })],
]) {
  test(`final cut fails closed when ${name}`, async () => {
    const result = await probe(fixture({ snapshots: [bindingSnapshot([1, 2]), final] }));
    assert.equal(result.code, "cleanup-unproven");
    assert.equal(result.ok, false);
  });
}

test("the checked password-file handle is closed after both successful and failed proof", async () => {
  for (const snapshots of [undefined, [bindingSnapshot([1, 2]), bindingSnapshot([1, 2]), bindingSnapshot([1, 2])]]) {
    const f = fixture({ snapshots });
    const openDatabase = f.io.openDatabase;
    let closed = 0;
    f.io.openDatabase = async () => {
      const reader = await openDatabase();
      reader.close = async () => { closed++; };
      return reader;
    };
    await probe(f);
    assert.equal(closed, 1);
  }
});
