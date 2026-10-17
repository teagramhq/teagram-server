import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { copyFile, mkdir, mkdtemp, readFile, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import test from "node:test";

import {
  probeAcceptance, QA_SCRIPT_TARGET, runnerEvidenceDirectoryValid, runnerEvidenceFileValid,
} from "./host-probe.mjs";
const webRevision = "c".repeat(40);
const serverRevision = "f".repeat(40);
const fingerprint = "0123456789abcdef";
const pin = "81985529216486895";
const imageId = `sha256:${"a".repeat(64)}`;
const serverId = "b".repeat(64);
const browserId = "d".repeat(64);
const observerId = "e".repeat(64);
const start = "1791878400000000";

function server(overrides = {}) {
  return {
    id: serverId, imageId, imageRevision: serverRevision, revision: serverRevision,
    labels: { "com.docker.compose.service": "telegramd", "org.opencontainers.image.revision": serverRevision },
    imageLabels: { "org.opencontainers.image.revision": serverRevision },
    startedAt: "2026-10-13T00:00:00Z", running: true,
    flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`],
    ...overrides,
  };
}

function buildIdentity(overrides = {}) {
  return {
    capturedAtUtc: "2026-10-13T00:00:00Z",
    sourceSha: serverRevision,
    imageId,
    revisionLabel: serverRevision,
    evidenceIdentity: "build-record-1",
    ...overrides,
  };
}

function webManifest(overrides = {}) {
  return {
    archiveSha256: `sha256:${"1".repeat(64)}`,
    contentDigest: `sha256:${"2".repeat(64)}`,
    sourceCommit: webRevision,
    url: `https://teagram.tailaa4918.ts.net/client/?v=${webRevision}`,
    target: {
      artifactDigest: `sha256:${"2".repeat(64)}`,
      endpoint: "wss://teagram.tailaa4918.ts.net/apiws",
      fingerprint,
      mode: "private",
      sourceCommit: webRevision,
    },
    ...overrides,
  };
}

function observer(overrides = {}) {
  return { id: observerId, startedAt: "2026-10-13T00:00:00Z", running: true, ...overrides };
}

function connectCounters({ attempts = {}, ...overrides } = {}) {
  return {
    status: "healthy",
    allowed_host: "teagram.tailaa4918.ts.net",
    connect_attempts: { "teagram.tailaa4918.ts.net": 1, "telegram-suffix": 0, other: 0, ...attempts },
    allowed_connects: 1, blocked_requests: 0, telegram_attempts: 0, other_blocked_count: 0,
    dns_lookups: 1, upstream_connects: 1, upstream_failures: 0,
    ...overrides,
  };
}

// One allowed tunnel plus the single blocked Telegram policy control.
function windowCounters({ attempts = {}, ...overrides } = {}) {
  return connectCounters({
    attempts: { "teagram.tailaa4918.ts.net": 2, "telegram-suffix": 1, ...attempts },
    allowed_connects: 2, blocked_requests: 1, telegram_attempts: 1, dns_lookups: 2, upstream_connects: 2,
    ...overrides,
  });
}

function counterOutput(snapshot) {
  return `${JSON.stringify(snapshot)}\n`;
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

function fixture({ runtimes = [[server()], [server()]], networks, snapshots, buildIdentities, webManifests,
  observers, counters, accountHighWaters = ["100", "100"], browserStatus = 0 } = {}) {
  const events = [];
  networks ??= [{ internal: true, members: [browserId, observerId] }, { internal: true, members: [browserId, observerId] }];
  snapshots ??= [bindingSnapshot([1, 2]), successfulCut()];
  buildIdentities ??= [[buildIdentity()], [buildIdentity()]];
  webManifests ??= [webManifest(), webManifest()];
  observers ??= [observer(), observer()];
  counters ??= [counterOutput(connectCounters()), counterOutput(windowCounters())];
  let snapshotIndex = 0;
  let accountHighWaterIndex = 0;
  const io = {
    async runtimeSnapshot() { events.push("runtime"); return runtimes.shift(); },
    async prepareBrowser() { events.push("prepare-browser"); return { browserId, observerId }; },
    async networkSnapshot() { events.push("network"); return networks.shift(); },
    async buildIdentities() { events.push("build-identities"); return buildIdentities.shift(); },
    async webManifest() { events.push("web-manifest"); return webManifests.shift(); },
    async observerSnapshot() { events.push("observer"); return observers.shift(); },
    async observerCounters(id) {
      events.push("observer-counters");
      assert.equal(id, observerId);
      const output = counters.shift();
      if (output instanceof Error) throw output;
      return output;
    },
    async openDatabase() {
      events.push("protected-files");
      return async () => {
        events.push("bindings");
        const snapshot = snapshots[snapshotIndex++];
        return snapshot;
      };
    },
    async readAccountHighWater() {
      events.push("account-high-water");
      const value = accountHighWaters[accountHighWaterIndex++];
      if (value instanceof Error) throw value;
      return value;
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
  return probeAcceptance(webRevision, f.io);
}

test("open registration with an unchanged account high-water mark passes", async () => {
  const flags = ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
  const f = fixture({
    runtimes: [[server({ flags })], [server({ flags })]],
    accountHighWaters: ["9223372036854775806", "9223372036854775806"],
  });
  const result = await probe(f);
  assert.equal(result.code, "ok");
  assert.equal(result.registrationMode, "open");
  assert.equal(result.accountEvidence, "complete");
  assert.equal(result.accountsUnchanged, true);
  assert.ok(f.events.indexOf("account-high-water") > f.events.indexOf("bindings"));
  assert.ok(f.events.lastIndexOf("account-high-water") > f.events.lastIndexOf("bindings"));
});

for (const [name, registrationFlags] of [
  ["invite", ["TG_REGISTRATION=invite"]],
  ["uppercase", ["TG_REGISTRATION=OPEN"]],
  ["empty", ["TG_REGISTRATION="]],
  ["unset", []],
]) {
  test(`${name} registration is unknown before the database opens`, async () => {
    const flags = [...registrationFlags, "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
    const f = fixture({ runtimes: [[server({ flags })]] });
    const result = await probe(f);
    assert.equal(result.code, "environment-unproven");
    assert.equal(result.registrationMode, "unknown");
    assert.equal(f.events.includes("protected-files"), false);
  });
}

test("mixed registration replicas fail before the database opens", async () => {
  const open = ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
  const closed = ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
  const f = fixture({ runtimes: [[server({ flags: open }), server({ id: "9".repeat(64), flags: closed })]] });
  const result = await probe(f);
  assert.equal(result.code, "environment-unproven");
  assert.equal(result.registrationMode, "unknown");
  assert.equal(f.events.includes("protected-files"), false);
});

test("a registration flip after launch is runtime-changed and still gets the post account read", async () => {
  const open = ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
  const closed = ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`];
  const f = fixture({
    runtimes: [[server({ flags: open })], [server({ flags: closed })]],
    accountHighWaters: ["100", "100"],
  });
  const result = await probe(f);
  assert.equal(result.code, "runtime-changed");
  assert.equal(result.registrationMode, "changed");
  assert.equal(result.accountEvidence, "complete");
  assert.equal(f.events.filter((event) => event === "account-high-water").length, 2);
  assert.equal(f.events.filter((event) => event === "bindings").length, 1,
    "the failed post-cut does not reach the final binding cut");
});

test("an unreadable post registration cut is changed and still gets the post account read", async () => {
  const f = fixture({ accountHighWaters: ["100", "100"] });
  let runtimeReads = 0;
  f.io.runtimeSnapshot = async () => {
    if (++runtimeReads === 2) throw new Error("private-runtime-canary");
    return [server()];
  };
  const result = await probe(f);
  assert.equal(result.code, "runtime-changed");
  assert.equal(result.registrationMode, "changed");
  assert.equal(result.accountEvidence, "complete");
  assert.equal(f.events.filter((event) => event === "account-high-water").length, 2);
  assert.doesNotMatch(JSON.stringify(result), /private-runtime-canary/u);
});

for (const [name, options] of [
  ["a clean binding cleanup", {}],
  ["binding-not-created", { browserStatus: 1, snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 2])] }],
  ["post-cut runtime-changed", { runtimes: [[server()], [server({ startedAt: "2026-10-13T01:00:00Z" })]] }],
  ["a browser launch error", { launchError: true }],
]) {
  test(`an increased account high-water overrides ${name}`, async () => {
    const f = fixture({ accountHighWaters: ["100", "101"], ...options });
    if (options.launchError) {
      f.io.launchBrowser = () => {
        f.events.push("stdin-forwarded");
        throw new Error("private-browser-launch-canary");
      };
    }
    const result = await probe(f);
    assert.equal(result.code, "account-created");
    assert.equal(result.ok, false);
    assert.equal(result.accountEvidence, "complete");
    assert.equal(result.accountsUnchanged, false);
    assert.equal(f.events.filter((event) => event === "account-high-water").length, 2);
    assert.doesNotMatch(JSON.stringify(result), /private-browser-launch-canary|\b100\b|\b101\b/u);
  });
}

test("a failed post account read is account-unproven", async () => {
  const f = fixture({
    runtimes: [[server()], [server({ startedAt: "2026-10-13T01:00:00Z" })]],
    accountHighWaters: ["100", new Error("private-high-water-canary")],
  });
  const result = await probe(f);
  assert.equal(result.code, "account-unproven");
  assert.equal(result.accountEvidence, "unavailable");
  assert.equal(result.accountsUnchanged, false);
  assert.doesNotMatch(JSON.stringify(result), /private-high-water-canary/u);
});

test("invalid account high-water data fails closed before browser launch", async () => {
  const f = fixture({ accountHighWaters: ["9223372036854775808"] });
  const result = await probe(f);
  assert.equal(result.code, "database-unavailable");
  assert.equal(result.accountEvidence, "unavailable");
  assert.equal(f.events.includes("stdin-forwarded"), false);
});

test("a backwards account high-water is account-unproven", async () => {
  const result = await probe(fixture({ accountHighWaters: ["100", "99"] }));
  assert.equal(result.code, "account-unproven");
  assert.equal(result.accountEvidence, "complete");
  assert.equal(result.accountsUnchanged, false);
});

test("a failed pre account read stops before browser launch", async () => {
  const f = fixture({ accountHighWaters: [new Error("private-high-water-canary")] });
  const result = await probe(f);
  assert.equal(result.code, "database-unavailable");
  assert.equal(result.accountEvidence, "unavailable");
  assert.equal(result.accountsUnchanged, false);
  assert.equal(f.events.includes("stdin-forwarded"), false);
});

test("account high-water sentinel values never appear in serialized probe evidence", async () => {
  const sentinel = "9223372036854775806";
  const f = fixture({ accountHighWaters: [sentinel, sentinel] });
  const result = await probe(f);
  assert.equal(result.accountEvidence, "complete");
  assert.equal(result.accountsUnchanged, true);
  assert.doesNotMatch(JSON.stringify(result), /9223372036854775806/u);
});

test("one newly created binding followed by in-band logout proves cleanup", async () => {
  const f = fixture();
  const result = await probe(f);
  assert.deepEqual(result, {
    schemaVersion: 4, ok: true, code: "ok", serverSourceCommit: serverRevision,
    registrationMode: "closed", accountEvidence: "complete", accountsUnchanged: true,
    loginCodesDisabled: true, revisionMatched: true,
    containersStable: true, networkIsolated: true, fingerprintMatched: true,
    newBindingProven: true, cleanupProven: true, browserExitOk: true,
    boundBefore: 2, boundAfter: 2, pendingBefore: 0, pendingAfter: 0,
    bindingEvidence: "complete", bindingAdditions: 1, bindingRemovals: 1,
    witnessInWindow: true, baselineIntact: true, topologyIsolated: true,
    captureWindow: "complete", captureAllowedConnects: 1, captureTelegramSuffix: 1,
    captureOther: 0, captureBlocked: 1, captureUpstreamFailures: 0,
  });
  assert.deepEqual(f.events, [
    "runtime", "web-manifest", "build-identities", "prepare-browser", "network", "protected-files", "bindings",
    "account-high-water",
    "observer", "observer-counters", "stdin-forwarded", "runtime", "build-identities",
    "web-manifest", "observer-counters", "observer", "network", "bindings", "account-high-water",
  ]);
  assert.equal(f.events.filter((event) => event === "bindings").length, 2,
    "one atomic API call must produce each cut; sampled legacy reads are not used");
  assert.doesNotMatch(JSON.stringify(result), /1791878400000000|1791878399000000|"epoch"|"additions"|"removals"/u);
});

for (const [name, flags] of [
  ["unset registration", ["TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`]],
  ["login-code logging", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=true", `TG_RSA_KEY_FINGERPRINT=${pin}`]],
  ["unset login-code logging", ["TG_REGISTRATION=closed", `TG_RSA_KEY_FINGERPRINT=${pin}`]],
  ["case-altered flag", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=False", `TG_RSA_KEY_FINGERPRINT=${pin}`]],
  ["duplicate flag", ["TG_REGISTRATION=closed", "TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`]],
  ["unprojected secret", ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`, "TG_POSTGRES_DSN=private-canary"]],
]) {
  test(`${name} fails closed before stdin or protected files are read`, async () => {
    const f = fixture({ runtimes: [[server({ flags })]] });
    const result = await probe(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, "environment-unproven");
    assert.deepEqual(f.events, ["runtime", "web-manifest", "build-identities"]);
    assert.doesNotMatch(JSON.stringify(result), /private-canary/u);
  });
}

test("every running replica must provide the protected environment flags", async () => {
  const f = fixture({ runtimes: [[server(), server({ id: "f".repeat(64), flags: ["TG_REGISTRATION=open"] })]] });
  assert.equal((await probe(f)).code, "environment-unproven");
  assert.deepEqual(f.events, ["runtime", "web-manifest", "build-identities"]);
});

for (const [name, runtime] of [
  ["no running server", []],
  ["missing OCI label", [server({ revision: null, labels: { "com.docker.compose.service": "telegramd" } })]],
  ["stale image label", [server({ imageRevision: "e".repeat(40), imageLabels: { "org.opencontainers.image.revision": "e".repeat(40) } })]],
  ["malformed image ID", [server({ imageId: "telegramd:local" })]],
  ["wrong revision", [server({ revision: "e".repeat(40), labels: { "com.docker.compose.service": "telegramd", "org.opencontainers.image.revision": "e".repeat(40) } })]],
]) {
  test(`${name} fails closed before browser execution`, async () => {
    const f = fixture({ runtimes: [runtime] });
    const result = await probe(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, runtime.length ? "revision-unproven" : "runtime-unavailable");
    assert.equal(f.events.includes("protected-files"), false);
    assert.equal(f.events.includes("stdin-forwarded"), false);
  });
}

for (const [name, options] of [
  ["no image-matched runner record", { buildIdentities: [[], []] }],
  ["runner record label mismatch", { buildIdentities: [[buildIdentity({ revisionLabel: "e".repeat(40) })], [buildIdentity({ revisionLabel: "e".repeat(40) })]] }],
  ["conflicting records for one image", { buildIdentities: [[buildIdentity(), buildIdentity({ sourceSha: "e".repeat(40), evidenceIdentity: "build-record-2" })], [buildIdentity(), buildIdentity({ sourceSha: "e".repeat(40), evidenceIdentity: "build-record-2" })]] }],
  ["container labelled with the web SHA has no matching server record", {
    runtimes: [[server({ revision: webRevision, imageRevision: webRevision,
      labels: { "com.docker.compose.service": "telegramd", "org.opencontainers.image.revision": webRevision },
      imageLabels: { "org.opencontainers.image.revision": webRevision } })]],
    buildIdentities: [[], []],
  }],
]) {
  test(`${name} fails before opening the database`, async () => {
    const f = fixture(options);
    const result = await probe(f);
    assert.equal(result.code, "revision-unproven");
    assert.equal(result.serverSourceCommit, "");
    assert.equal(f.events.includes("protected-files"), false);
    assert.equal(f.events.includes("stdin-forwarded"), false);
  });
}

test("mixed replica server revisions fail before opening the database", async () => {
  const otherImage = `sha256:${"9".repeat(64)}`;
  const otherServer = "9".repeat(64);
  const otherRevision = "e".repeat(40);
  const f = fixture({
    runtimes: [[server(), server({ id: otherServer, imageId: otherImage, revision: otherRevision,
      imageRevision: otherRevision,
      labels: { "com.docker.compose.service": "telegramd", "org.opencontainers.image.revision": otherRevision },
      imageLabels: { "org.opencontainers.image.revision": otherRevision } })]],
    buildIdentities: [[buildIdentity(), buildIdentity({ imageId: otherImage, sourceSha: otherRevision,
      revisionLabel: otherRevision, evidenceIdentity: "build-record-2" })]],
  });
  assert.equal((await probe(f)).code, "revision-unproven");
  assert.equal(f.events.includes("protected-files"), false);
});

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

for (const [name, changes] of [
  ["a replaced runner record", { buildIdentities: [[buildIdentity()], [buildIdentity({ evidenceIdentity: "replacement-record" })]] }],
  ["an observer restart", { observers: [observer(), observer({ startedAt: "2026-10-13T01:00:00Z" })] }],
  ["changed container labels", { runtimes: [[server()], [server({ labels: { "com.docker.compose.service": "telegramd", note: "changed" , "org.opencontainers.image.revision": serverRevision } })]] }],
  ["changed image labels", { runtimes: [[server()], [server({ imageLabels: { "org.opencontainers.image.revision": serverRevision, note: "changed" } })]] }],
  ["changed registration flag", { runtimes: [[server()], [server({ flags: ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`] })]] }],
  ["changed login-code flag", { runtimes: [[server()], [server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=true", `TG_RSA_KEY_FINGERPRINT=${pin}`] })]] }],
  ["changed effective fingerprint", { runtimes: [[server()], [server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", "TG_RSA_KEY_FINGERPRINT=fedcba9876543210"] })]] }],
  ["an added replica", { runtimes: [[server()], [server(), server({ id: "9".repeat(64) })]] }],
  ["a changed web manifest", { webManifests: [webManifest(), webManifest({ target: { ...webManifest().target, endpoint: "wss://teagram.tailaa4918.ts.net/changed" } })] }],
]) {
  test(`${name} after the browser exits prevents a pass`, async () => {
    const result = await probe(fixture(changes));
    assert.equal(result.code, "runtime-changed");
    assert.equal(result.ok, false);
  });
}

test("a post-cut registration flag flip clears its evidence and fails runtime stability", async () => {
  const result = await probe(fixture({
    runtimes: [[server()], [server({ flags: ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`] })]],
  }));
  assert.equal(result.code, "runtime-changed");
  assert.equal(result.registrationMode, "changed");
  assert.equal(result.loginCodesDisabled, true);
  assert.equal(result.fingerprintMatched, true);
  assert.equal(result.containersStable, true);
});

test("a post-cut flag flip is recorded even when the observer snapshot throws", async () => {
  const f = fixture({
    runtimes: [[server()], [server({ flags: ["TG_REGISTRATION=open", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${pin}`] })]],
  });
  let observerReads = 0;
  f.io.observerSnapshot = async () => {
    if (++observerReads === 2) throw new Error("observer snapshot failed");
    return observer();
  };

  const result = await probe(f);
  assert.equal(result.code, "runtime-changed");
  assert.equal(result.registrationMode, "changed");
  assert.equal(result.loginCodesDisabled, true);
  assert.equal(result.fingerprintMatched, true);
});

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

test("deltas (0,0,1,1,1,0,1) of other, other blocked, Telegram, Telegram suffix, blocked, upstream failures and allowed tunnels pass", async () => {
  const f = fixture({ counters: [
    counterOutput(connectCounters({ attempts: { other: 3 }, other_blocked_count: 3, blocked_requests: 3 })),
    counterOutput(windowCounters({ attempts: { other: 3 }, other_blocked_count: 3, blocked_requests: 4,
      allowed_connects: 4, upstream_connects: 4, dns_lookups: 4 })),
  ] });
  const result = await probe(f);
  assert.equal(result.code, "ok");
  assert.equal(result.networkIsolated, true);
  assert.equal(result.captureWindow, "complete");
  assert.deepEqual([
    result.captureAllowedConnects, result.captureTelegramSuffix, result.captureOther,
    result.captureBlocked, result.captureUpstreamFailures,
  ], [3, 1, 0, 1, 0]);
  assert.doesNotMatch(JSON.stringify(result), /allowed_connects|connect_attempts|telegram_attempts|healthy/u);
});

test("a window carrying the web client's full WSS socket ceiling beside its page tunnel passes", async () => {
  // teagram-web opens one client socket per account slot on an unauthenticated load.
  const wssConnectionCeiling = 4;
  const tunnels = 1 + wssConnectionCeiling + 1;
  const f = fixture({ counters: [
    counterOutput(connectCounters()),
    counterOutput(windowCounters({ attempts: { "teagram.tailaa4918.ts.net": tunnels }, allowed_connects: tunnels,
      dns_lookups: tunnels, upstream_connects: tunnels })),
  ] });
  const result = await probe(f);
  assert.equal(result.code, "ok");
  assert.equal(result.networkIsolated, true);
  assert.equal(result.captureWindow, "complete");
  assert.equal(result.captureAllowedConnects, tunnels - 1);
});

for (const [name, after] of [
  ["an extra other CONNECT attempt", windowCounters({ attempts: { other: 1 }, other_blocked_count: 1, blocked_requests: 2 })],
  ["an extra other blocked request", windowCounters({ other_blocked_count: 1, blocked_requests: 2 })],
  ["no blocked Telegram policy control", windowCounters({ attempts: { "telegram-suffix": 0 }, telegram_attempts: 0, blocked_requests: 0 })],
  ["two Telegram attempts", windowCounters({ telegram_attempts: 2 })],
  ["two Telegram-suffix attempts", windowCounters({ attempts: { "telegram-suffix": 2 } })],
  ["two blocked requests", windowCounters({ blocked_requests: 2 })],
  ["an unexpected blocked Telegram count", windowCounters({ attempts: { "telegram-suffix": 2 }, telegram_attempts: 2, blocked_requests: 2 })],
  ["an upstream failure", windowCounters({ upstream_failures: 1 })],
  ["no allowed tunnel", windowCounters({ allowed_connects: 1 })],
  ["a clamped allowed counter", windowCounters({ allowed_connects: 100 })],
  ["a clamped DNS counter", windowCounters({ dns_lookups: 100 })],
  ["a clamped allowed-host attempt counter", windowCounters({ attempts: { "teagram.tailaa4918.ts.net": 100 } })],
  ["a counter regression", windowCounters({ dns_lookups: 0 })],
  ["an allowed-host attempt regression", windowCounters({ attempts: { "teagram.tailaa4918.ts.net": 0 } })],
  ["an unhealthy observer", windowCounters({ status: "unhealthy" })],
]) {
  test(`${name} across the window is network-unproven`, async () => {
    const f = fixture({ counters: [counterOutput(connectCounters()), counterOutput(after)] });
    const result = await probe(f);
    assert.equal(result.code, "network-unproven");
    assert.equal(result.networkIsolated, false);
    assert.equal(result.topologyIsolated, true);
    assert.equal(result.ok, false);
    const before = connectCounters();
    const afterEntries = [
      ["allowed_connects", after.allowed_connects - before.allowed_connects],
      ["connect_attempts.telegram-suffix", after.connect_attempts["telegram-suffix"] - before.connect_attempts["telegram-suffix"]],
      ["connect_attempts.other", after.connect_attempts.other - before.connect_attempts.other],
      ["blocked_requests", after.blocked_requests - before.blocked_requests],
      ["upstream_failures", after.upstream_failures - before.upstream_failures],
    ];
    const allCounters = [
      ...Object.entries(after).filter(([key]) => !["status", "allowed_host", "connect_attempts"].includes(key)),
      ...Object.entries(after.connect_attempts),
    ];
    const beforeCounters = new Map([
      ...Object.entries(before).filter(([key]) => !["status", "allowed_host", "connect_attempts"].includes(key)),
      ...Object.entries(before.connect_attempts),
    ]);
    const captureComplete = after.status === "healthy" &&
      allCounters.every(([key, value]) => value < 100 && value >= beforeCounters.get(key));
    assert.equal(result.captureWindow, captureComplete ? "complete" : "invalid");
    assert.deepEqual([
      result.captureAllowedConnects, result.captureTelegramSuffix, result.captureOther,
      result.captureBlocked, result.captureUpstreamFailures,
    ], captureComplete ? afterEntries.map(([, value]) => value) : [0, 0, 0, 0, 0]);
    assert.equal(f.events.filter((event) => event === "observer-counters").length, 2, "no retry");
    assert.equal(f.events.filter((event) => event === "bindings").length, 1);
    assert.doesNotMatch(JSON.stringify(result), /allowed_connects|connect_attempts|telegram_attempts|healthy/u);
  });
}

const invalidCounterOutputs = [
  ["a failed read", new Error("docker exec failed")],
  ["empty output", ""],
  ["missing output", undefined],
  ["non-JSON output", "not json\n"],
  ["an extra field", counterOutput({ ...connectCounters(), raw: "x" })],
  ["a missing counter", counterOutput((({ dns_lookups, ...rest }) => rest)(connectCounters()))],
  ["an out-of-range counter", counterOutput(connectCounters({ allowed_connects: 101 }))],
  ["a negative counter", counterOutput(connectCounters({ upstream_failures: -1 }))],
  ["a different allowed host", counterOutput(connectCounters({ allowed_host: "example.com" }))],
  ["an unhealthy observer", counterOutput(connectCounters({ status: "unhealthy" }))],
  ["oversized output", `${JSON.stringify(connectCounters())}${" ".repeat(16_384)}\n`],
];

for (const [name, output] of invalidCounterOutputs) {
  test(`${name} from the observer before credential forwarding is network-unproven`, async () => {
    const f = fixture({ counters: [output] });
    const result = await probe(f);
    assert.equal(result.code, "network-unproven");
    assert.equal(result.networkIsolated, false);
    assert.equal(f.events.filter((event) => event === "bindings").length, 1);
    assert.ok(!f.events.includes("stdin-forwarded"));
    assert.equal(f.events.filter((event) => event === "observer-counters").length, 1, "no retry");
  });

  test(`${name} from the observer after the browser exits is network-unproven`, async () => {
    const f = fixture({ counters: [counterOutput(connectCounters()), output] });
    const result = await probe(f);
    assert.equal(result.code, "network-unproven");
    assert.equal(result.networkIsolated, false);
    assert.equal(result.ok, false);
    assert.equal(f.events.filter((event) => event === "observer-counters").length, 2, "no retry");
  });
}

test("a pre-cut counter at the clamp is network-unproven before credential forwarding", async () => {
  const f = fixture({ counters: [counterOutput(connectCounters({ dns_lookups: 100 }))] });
  assert.equal((await probe(f)).code, "network-unproven");
  assert.ok(!f.events.includes("stdin-forwarded"));
});

test("an observer restart that resets counters stays runtime-changed", async () => {
  const f = fixture({
    observers: [observer(), observer({ startedAt: "2026-10-13T01:00:00Z" })],
    counters: [counterOutput(connectCounters({ allowed_connects: 5 })), counterOutput(connectCounters({ allowed_connects: 0 }))],
  });
  const result = await probe(f);
  assert.equal(result.code, "runtime-changed");
  assert.equal(result.ok, false);
});

test("the host TLS fingerprint must equal the effective server fingerprint", async () => {
  const f = fixture({ webManifests: [
    webManifest({ target: { ...webManifest().target, fingerprint: "fedcba9876543210" } }), webManifest(),
  ] });
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
  const f = fixture({ webManifests: [webManifest(), webManifest({
    target: { ...webManifest().target, fingerprint: "fedcba9876543210" },
  })] });
  const result = await probe(f);
  assert.equal(result.ok, false);
  assert.equal(result.code, "runtime-changed");
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
  assert.equal(result.bindingEvidence, "unavailable");
  assert.equal(result.captureWindow, "complete");
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
    assert.equal(result.bindingEvidence, name === "enrollment epoch changes" ? "epoch-changed" : "unavailable");
    assert.equal(result.bindingAdditions, 0);
    assert.equal(result.bindingRemovals, 0);
    assert.equal(result.witnessInWindow, false);
    assert.equal(result.baselineIntact, false);
  });
}

test("zero binding delta with an intact baseline and failed browser exit is binding-not-created", async () => {
  const result = await probe(fixture({
    browserStatus: 1,
    snapshots: [bindingSnapshot([1, 2], 0), bindingSnapshot([1, 2], 0)],
  }));
  assert.equal(result.code, "binding-not-created");
  assert.equal(result.ok, false);
  assert.equal(result.bindingEvidence, "complete");
  assert.equal(result.bindingAdditions, 0);
  assert.equal(result.bindingRemovals, 0);
  assert.equal(result.baselineIntact, true);
  assert.equal(result.browserExitOk, false);
});

for (const [name, options, expectedDiagnostics] of [
  ["browser exit succeeded", { browserStatus: 0, snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 2])] }, [0, 0, true, false]],
  ["pending count rose", { snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 2], 1)] }, [0, 0, true, false]],
  ["baseline binding changed", { snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 3], 0, {
    additions: "11", removals: "8", last_added_created_at: "1791878400000001", db_time: "1791878400000002",
  })] }, [1, 0, false, true]],
  ["two bindings transitioned", { snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 2], 0, {
    additions: "12", removals: "10", last_added_created_at: "1791878400000002", db_time: "1791878400000003",
  })] }, [2, 2, true, true]],
  ["five transitions clamp to three", { snapshots: [bindingSnapshot([1, 2]), bindingSnapshot([1, 2], 0, {
    additions: "15", removals: "13", last_added_created_at: "1791878400000002", db_time: "1791878400000003",
  })] }, [3, 3, true, true]],
  ["witness predates the run", { snapshots: [bindingSnapshot([1, 2]), successfulCut([1, 2], 0, {
    last_added_created_at: "1791878399999999",
  })] }, [1, 1, true, false]],
]) {
  test(`binding-not-created is not emitted when ${name}`, async () => {
    const result = await probe(fixture({ browserStatus: 1, ...options }));
    assert.equal(result.code, "cleanup-unproven");
    assert.equal(result.bindingEvidence, "complete");
    assert.deepEqual([
      result.bindingAdditions, result.bindingRemovals, result.baselineIntact, result.witnessInWindow,
    ], expectedDiagnostics);
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

async function preflight(f) {
  return probeAcceptance(webRevision, f.io, { preflight: true });
}

test("preflight runs every acceptance check in the accepted order and stops after one binding cut", async () => {
  const f = fixture();
  const result = await preflight(f);
  assert.deepEqual(result, {
    schemaVersion: 4, ok: true, code: "ok", serverSourceCommit: serverRevision,
    registrationMode: "closed", accountEvidence: "complete", accountsUnchanged: false,
    loginCodesDisabled: true, revisionMatched: true,
    containersStable: true, networkIsolated: true, fingerprintMatched: true,
    newBindingProven: false, cleanupProven: false, browserExitOk: true,
    boundBefore: 2, boundAfter: 0, pendingBefore: 0, pendingAfter: 0,
    bindingEvidence: "not-reached", bindingAdditions: 0, bindingRemovals: 0,
    witnessInWindow: false, baselineIntact: false, topologyIsolated: true,
    captureWindow: "complete", captureAllowedConnects: 1, captureTelegramSuffix: 1,
    captureOther: 0, captureBlocked: 1, captureUpstreamFailures: 0,
  });
  assert.deepEqual(f.events, [
    "runtime", "web-manifest", "build-identities", "prepare-browser", "network", "protected-files", "bindings",
    "account-high-water",
    "observer", "observer-counters", "stdin-forwarded", "runtime", "build-identities",
    "web-manifest", "observer-counters", "observer", "network",
  ]);
  assert.equal(f.events.filter((event) => event === "account-high-water").length, 1);
});

test("a failed QA preflight is browser-failed after the post-cut checks", async () => {
  const result = await preflight(fixture({ browserStatus: 1 }));
  assert.equal(result.ok, false);
  assert.equal(result.code, "browser-failed");
  assert.equal(result.browserExitOk, false);
  assert.equal(result.cleanupProven, false);
});

test("unreached evidence steps stay at their defaults after revision preflight failure", async () => {
  const result = await probe(fixture({ buildIdentities: [[], []] }));
  assert.equal(result.code, "revision-unproven");
  assert.equal(result.bindingEvidence, "not-reached");
  assert.equal(result.captureWindow, "not-reached");
  assert.deepEqual([
    result.bindingAdditions, result.bindingRemovals, result.captureAllowedConnects,
    result.captureTelegramSuffix, result.captureOther, result.captureBlocked, result.captureUpstreamFailures,
  ], [0, 0, 0, 0, 0, 0, 0]);
});

test("sentinel identifiers and database metadata never appear in serialized probe evidence", async () => {
  const browser = "7".repeat(64);
  const observerIdValue = "8".repeat(64);
  const serverIdValue = "9".repeat(64);
  const epoch = "77777777777777777";
  const keyId = "8888888888888888";
  const createdAt = "6666666666666666";
  const dbTime = "5555555555555555";
  const baseline = bindingSnapshot([keyId], 0, {
    epoch, bound_key_created_ats: [createdAt], db_time: dbTime,
  });
  const final = bindingSnapshot([keyId], 0, {
    epoch, additions: "10", removals: "8", last_added_created_at: createdAt,
    bound_key_created_ats: [createdAt], db_time: "77777777777777770",
  });
  const f = fixture({
    runtimes: [[server({ id: serverIdValue })], [server({ id: serverIdValue })]],
    networks: [
      { internal: true, members: [browser, observerIdValue] },
      { internal: true, members: [browser, observerIdValue] },
    ],
    snapshots: [baseline, final],
  });
  f.io.prepareBrowser = async () => ({ browserId: browser, observerId: observerIdValue });
  f.io.observerSnapshot = async () => observer({ id: observerIdValue });
  let counterReads = 0;
  f.io.observerCounters = async () => counterOutput(counterReads++ === 0 ? connectCounters() : windowCounters());
  const result = await probe(f);
  assert.equal(result.bindingEvidence, "complete");
  assert.equal(f.events.filter((event) => event === "bindings").length, 2);
  const serialized = JSON.stringify(result);
  for (const sentinel of [browser, observerIdValue, serverIdValue, epoch, keyId, createdAt, dbTime]) {
    assert.equal(serialized.includes(sentinel), false, `serialized result contains ${sentinel}`);
  }
});

for (const [name, options, code] of [
  ["a runtime change", { runtimes: [[server()], [server({ startedAt: "2026-10-13T01:00:00Z" })]] }, "runtime-changed"],
  ["an observer restart", { observers: [observer(), observer({ startedAt: "2026-10-13T01:00:00Z" })] }, "runtime-changed"],
  ["a changed web manifest", { webManifests: [webManifest(), webManifest({ contentDigest: `sha256:${"3".repeat(64)}` })] }, "runtime-changed"],
  ["no blocked policy control in the window", { counters: [counterOutput(connectCounters()), counterOutput(connectCounters())] }, "network-unproven"],
]) {
  test(`${name} during preflight is ${code}`, async () => {
    const f = fixture(options);
    const result = await preflight(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, code);
    assert.equal(f.events.filter((event) => event === "bindings").length, 1);
  });
}

for (const [name, options, code] of [
  ["missing server provenance", { buildIdentities: [[], []] }, "revision-unproven"],
  ["a shared network", { networks: [{ internal: false, members: [browserId, observerId] }] }, "network-unproven"],
  ["an unmatched fingerprint", { runtimes: [[server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", "TG_RSA_KEY_FINGERPRINT=fedcba9876543210"] })]] }, "fingerprint-unproven"],
  ["incomplete enrollment", { snapshots: [bindingSnapshot([1, 2], 0, { complete: false })] }, "database-unavailable"],
  ["unreadable observer counters", { counters: [new Error("docker exec failed")] }, "network-unproven"],
]) {
  test(`${name} stops preflight before the QA process starts`, async () => {
    const f = fixture(options);
    const result = await preflight(f);
    assert.equal(result.ok, false);
    assert.equal(result.code, code);
    assert.ok(!f.events.includes("stdin-forwarded"));
  });
}

function pinned(value) {
  return server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false", `TG_RSA_KEY_FINGERPRINT=${value}`] });
}

const negativeFingerprint = "fedcba9876543210";
const negativePin = "-81985529216486896";
const negativeManifest = () => webManifest({ target: { ...webManifest().target, fingerprint: negativeFingerprint } });

test("a negative decimal pin matches the served unsigned hex fingerprint", async () => {
  const f = fixture({
    runtimes: [[pinned(negativePin)], [pinned(negativePin)]],
    webManifests: [negativeManifest(), negativeManifest()],
  });
  const result = await preflight(f);
  assert.equal(result.code, "ok");
  assert.equal(result.fingerprintMatched, true);
  assert.equal(result.containersStable, true);
});

for (const [name, value] of [
  ["a different value", "81985529216486896"],
  ["an empty value", ""],
  ["the hex form", fingerprint],
  ["a non-integer value", "8198552921648689.5"],
  ["a value above int64", "9223372036854775808"],
  ["a value below int64", "-9223372036854775809"],
  ["the unsigned decimal of a negative pin", "18364758544493064720"],
  ["a leading plus", `+${pin}`],
  ["a leading zero", `0${pin}`],
  ["leading whitespace", ` ${pin}`],
  ["trailing whitespace", `${pin} `],
]) {
  test(`${name} as the server pin is fingerprint-unproven`, async () => {
    const f = fixture({ runtimes: [[pinned(value)], [pinned(value)]] });
    const result = await preflight(f);
    assert.equal(result.code, "fingerprint-unproven");
    assert.equal(result.fingerprintMatched, false);
    assert.ok(!f.events.includes("stdin-forwarded"));
  });
}

test("a missing server pin is fingerprint-unproven", async () => {
  const f = fixture({ runtimes: [[server({ flags: ["TG_REGISTRATION=closed", "TG_LOG_LOGIN_CODES=false"] })]] });
  const result = await preflight(f);
  assert.equal(result.code, "fingerprint-unproven");
  assert.ok(!f.events.includes("stdin-forwarded"));
});

test("the unsigned decimal of a negative pin is fingerprint-unproven against its hex", async () => {
  const f = fixture({
    runtimes: [[pinned("18364758544493064720")], [pinned("18364758544493064720")]],
    webManifests: [negativeManifest(), negativeManifest()],
  });
  assert.equal((await preflight(f)).code, "fingerprint-unproven");
});

for (const served of ["0123456789ABCDEF", "123456789abcdef", "00123456789abcdef", "0x23456789abcdef"]) {
  test(`a served fingerprint of ${served} is fingerprint-unproven`, async () => {
    const manifest = () => webManifest({ target: { ...webManifest().target, fingerprint: served } });
    const f = fixture({ webManifests: [manifest(), manifest()] });
    const result = await preflight(f);
    assert.equal(result.code, "fingerprint-unproven");
    assert.ok(!f.events.includes("stdin-forwarded"));
  });
}

test("int64 bounds match their served hex fingerprints", async () => {
  for (const [hex, value] of [["7fffffffffffffff", "9223372036854775807"], ["8000000000000000", "-9223372036854775808"]]) {
    const manifest = () => webManifest({ target: { ...webManifest().target, fingerprint: hex } });
    const f = fixture({ runtimes: [[pinned(value)], [pinned(value)]], webManifests: [manifest(), manifest()] });
    assert.equal((await preflight(f)).code, "ok", hex);
  }
});

for (const [name, value] of [["the hex form", fingerprint], ["a leading zero", `0${pin}`], ["a different value", "1"]]) {
  test(`${name} as the server pin after the run is runtime-changed`, async () => {
    const f = fixture({ runtimes: [[server()], [pinned(value)]] });
    const result = await preflight(f);
    assert.equal(result.code, "runtime-changed");
    assert.equal(result.containersStable, true);
    assert.equal(result.fingerprintMatched, false);
  });
}

test("runner evidence candidates owned by another uid are rejected", async (t) => {
  const directory = await mkdtemp(join(tmpdir(), "acceptance-evidence-owner-"));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const candidate = join(directory, "run.build");
  const record = join(candidate, "built-image.identity");
  await mkdir(candidate, { mode: 0o700 });
  await writeFile(record, "record\n", { mode: 0o600 });
  const directoryStat = await stat(candidate, { bigint: true });
  const fileStat = await stat(record, { bigint: true });
  const owner = process.getuid();
  const foreign = owner === 65534 ? 65533 : 65534;
  const ownedBy = (base, uid) => Object.create(base, { uid: { value: BigInt(uid) } });

  assert.equal(runnerEvidenceDirectoryValid(directoryStat, owner), true);
  assert.equal(runnerEvidenceFileValid(fileStat, owner), true);
  assert.equal(runnerEvidenceDirectoryValid(ownedBy(directoryStat, 0), 0), true);
  assert.equal(runnerEvidenceFileValid(ownedBy(fileStat, 0), 0), true);
  for (const expected of [foreign, 0].filter((uid) => uid !== owner)) {
    assert.equal(runnerEvidenceDirectoryValid(directoryStat, expected), false, `directory for ${expected}`);
    assert.equal(runnerEvidenceFileValid(fileStat, expected), false, `file for ${expected}`);
  }
  for (const expected of [owner, 0]) {
    assert.equal(runnerEvidenceDirectoryValid(ownedBy(directoryStat, foreign), expected), false);
    assert.equal(runnerEvidenceFileValid(ownedBy(fileStat, foreign), expected), false);
  }
});

test("the QA runner mount resolves Playwright from the image package tree", async (t) => {
  const dockerfile = await readFile(new URL("./Dockerfile", import.meta.url), "utf8");
  const packageDirectory = dirname(QA_SCRIPT_TARGET);
  assert.match(dockerfile, new RegExp(`^WORKDIR ${packageDirectory}$`, "mu"));
  assert.match(dockerfile, /^RUN npm ci --omit=dev /mu);
  assert.match(await readFile(new URL("./package.json", import.meta.url), "utf8"), /"playwright"/u);

  // Mirror the image layout: Playwright installed only under the package tree.
  const root = await mkdtemp(join(tmpdir(), "acceptance-qa-resolve-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const marker = join(root, "playwright-loaded");
  const stub = join(root, packageDirectory, "node_modules", "playwright");
  await mkdir(stub, { recursive: true });
  await writeFile(join(stub, "package.json"), '{"name":"playwright","type":"module","exports":"./index.mjs"}');
  await writeFile(join(stub, "index.mjs"),
    `import { writeFileSync } from "node:fs";\nwriteFileSync(${JSON.stringify(marker)}, "");\nexport const chromium = {};\n`);
  const adapter = join(root, "adapter.mjs");
  await writeFile(adapter, [
    "export function readReleaseRecord() { throw new Error('fixture'); }",
    "export function browserLaunchPolicy() {}", "export function verifySandbox() {}",
    "export function createAcceptanceProgress() {}", "export function acceptanceRpcOutcome() {}",
  ].join("\n"));
  const pinned = new URL("./private-browser-acceptance-v2.mjs", import.meta.url);
  const run = async (target) => {
    await mkdir(dirname(join(root, target)), { recursive: true });
    await copyFile(pinned, join(root, target));
    await rm(marker, { force: true });
    spawnSync(process.execPath, [join(root, target), "--mode", "preflight", "--release-record",
      join(root, "missing.json"), "--runtime-adapter", adapter], { stdio: "ignore", timeout: 10_000 });
    return stat(marker).then(() => true, () => false);
  };
  assert.equal(await run("/run/approved-qa.mjs"), false, "outside the package tree Playwright is unresolvable");
  assert.equal(await run(QA_SCRIPT_TARGET), true);
});
