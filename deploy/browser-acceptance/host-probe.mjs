import { spawn } from "node:child_process";
import { constants } from "node:fs";
import { open, readFile, readdir } from "node:fs/promises";
import { isIP } from "node:net";
import { dirname, isAbsolute, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { ALLOWED_HOST, isValidSnapshot } from "./connect-observer.mjs";

function exactKeys(value, keys) {
  return value !== null && typeof value === "object" && !Array.isArray(value) &&
    JSON.stringify(Object.keys(value).sort()) === JSON.stringify([...keys].sort());
}

function emptyResult() {
  return {
    schemaVersion: 2, ok: false, code: "runtime-unavailable", serverSourceCommit: "",
    registrationClosed: false, loginCodesDisabled: false, revisionMatched: false,
    containersStable: false, networkIsolated: false, fingerprintMatched: false,
    newBindingProven: false, cleanupProven: false, browserExitOk: false,
    boundBefore: 0, boundAfter: 0, pendingBefore: 0, pendingAfter: 0,
  };
}

function runtimeFlags(servers) {
  return servers.map((server) => {
    const flags = new Map();
    if (!Array.isArray(server.flags)) return flags;
    for (const flag of server.flags) {
      if (typeof flag !== "string") return new Map();
      const separator = flag.indexOf("=");
      const name = flag.slice(0, separator);
      if (separator < 0 || !["TG_REGISTRATION", "TG_LOG_LOGIN_CODES", "TG_RSA_KEY_FINGERPRINT"].includes(name) ||
          flags.has(name)) return new Map();
      flags.set(name, flag.slice(separator + 1));
    }
    return flags;
  });
}

// telegramd pins the key as a signed 64-bit decimal; the manifest serves the same bits as unsigned hex.
function fingerprintPinned(pin, fingerprint) {
  if (typeof pin !== "string" || !/^-?[0-9]{1,19}$/u.test(pin) ||
      typeof fingerprint !== "string" || !/^[0-9a-f]{16}$/u.test(fingerprint)) return false;
  const value = BigInt(pin);
  return value.toString() === pin && BigInt.asIntN(64, value) === value &&
    BigInt.asUintN(64, value) === BigInt(`0x${fingerprint}`);
}

function validLabels(labels) {
  return labels !== null && typeof labels === "object" && !Array.isArray(labels) &&
    Object.values(labels).every((value) => typeof value === "string");
}

function validRuntime(servers) {
  return Array.isArray(servers) && new Set(servers.map((server) => server.id)).size === servers.length &&
    servers.every((server) => /^[0-9a-f]{64}$/u.test(server.id) && /^sha256:[0-9a-f]{64}$/u.test(server.imageId) &&
      typeof server.revision === "string" && typeof server.imageRevision === "string" &&
      validLabels(server.labels) && validLabels(server.imageLabels) && server.running === true &&
      typeof server.startedAt === "string" && Number.isFinite(Date.parse(server.startedAt)) &&
      server.labels["org.opencontainers.image.revision"] === server.revision &&
      server.imageLabels["org.opencontainers.image.revision"] === server.imageRevision && Array.isArray(server.flags));
}

function sameLabels(left, right) {
  return JSON.stringify(Object.entries(left).sort(([leftKey], [rightKey]) => leftKey.localeCompare(rightKey))) ===
    JSON.stringify(Object.entries(right).sort(([leftKey], [rightKey]) => leftKey.localeCompare(rightKey)));
}

function sameRuntime(before, after) {
  return after.length === before.length && before.every((server) => after.some((current) => (
    current.id === server.id && current.imageId === server.imageId && current.startedAt === server.startedAt &&
    current.running === server.running && current.revision === server.revision &&
    current.imageRevision === server.imageRevision && sameLabels(current.labels, server.labels) &&
    sameLabels(current.imageLabels, server.imageLabels)
  )));
}

function validBuildIdentity(record) {
  return exactKeys(record, ["capturedAtUtc", "sourceSha", "imageId", "revisionLabel", "evidenceIdentity"]) &&
    typeof record.capturedAtUtc === "string" && Number.isFinite(Date.parse(record.capturedAtUtc)) &&
    /^[0-9a-f]{40}$/u.test(record.sourceSha) && /^sha256:[0-9a-f]{64}$/u.test(record.imageId) &&
    /^[0-9a-f]{40}$/u.test(record.revisionLabel) &&
    typeof record.evidenceIdentity === "string" && record.evidenceIdentity.length > 0;
}

function resolveServerProvenance(servers, records) {
  if (!Array.isArray(servers) || servers.length === 0 || !Array.isArray(records) || !records.every(validBuildIdentity)) return null;
  const revisions = new Set();
  const selectedIdentities = [];
  for (const server of servers) {
    const selected = records.filter((record) => record.imageId === server.imageId);
    if (selected.length === 0) return null;
    const selectedRevisions = new Set(selected.map((record) => record.sourceSha));
    if (selectedRevisions.size !== 1) return null;
    const sourceSha = selected[0].sourceSha;
    if (server.revision !== sourceSha || server.imageRevision !== sourceSha ||
        selected.some((record) => record.revisionLabel !== sourceSha)) return null;
    revisions.add(sourceSha);
    selectedIdentities.push(...selected.map((record) => record.evidenceIdentity));
  }
  if (revisions.size !== 1) return null;
  return { sourceCommit: [...revisions][0], selectedIdentities: selectedIdentities.sort() };
}

function sameObserver(before, after, expectedId) {
  return exactKeys(after, ["id", "startedAt", "running"]) &&
    /^[0-9a-f]{64}$/u.test(after.id) && after.id === expectedId && before.id === after.id &&
    typeof before.startedAt === "string" && before.startedAt === after.startedAt &&
    before.running === true && after.running === true && typeof after.startedAt === "string" &&
    Number.isFinite(Date.parse(after.startedAt));
}

function sameWebManifest(before, after) {
  return JSON.stringify(before) === JSON.stringify(after);
}

function validWebManifest(manifest, webRevision) {
  if (!exactKeys(manifest, ["archiveSha256", "contentDigest", "sourceCommit", "url", "target"]) ||
      !/^[0-9a-f]{40}$/u.test(webRevision) || manifest.sourceCommit !== webRevision ||
      !/^sha256:[0-9a-f]{64}$/u.test(manifest.archiveSha256) ||
      !/^sha256:[0-9a-f]{64}$/u.test(manifest.contentDigest) || typeof manifest.url !== "string" ||
      !exactKeys(manifest.target, ["artifactDigest", "endpoint", "fingerprint", "mode", "sourceCommit"])) return false;
  const target = manifest.target;
  return target.mode === "private" && target.sourceCommit === webRevision &&
    target.artifactDigest === manifest.contentDigest && /^[0-9a-f]{16}$/u.test(target.fingerprint) &&
    typeof target.endpoint === "string" && target.endpoint.startsWith(`wss://${ALLOWED_HOST}/`) &&
    !/[?#]/u.test(target.endpoint);
}

function isolatedNetwork(network, containers) {
  return exactKeys(network, ["internal", "members"]) && network.internal === true &&
    /^[0-9a-f]{64}$/u.test(containers.browserId) && /^[0-9a-f]{64}$/u.test(containers.observerId) &&
    containers.browserId !== containers.observerId && Array.isArray(network.members) &&
    network.members.length === 2 && new Set(network.members).size === 2 &&
    network.members.includes(containers.browserId) && network.members.includes(containers.observerId);
}

const OBSERVER_OUTPUT_LIMIT = 16_384;
const COUNTER_CLAMP = 100;

function observerCounters(output) {
  if (typeof output !== "string" || Buffer.byteLength(output) > OBSERVER_OUTPUT_LIMIT) return null;
  let snapshot;
  try { snapshot = JSON.parse(output); } catch { return null; }
  return isValidSnapshot(snapshot) && snapshot.status === "healthy" ? snapshot : null;
}

function counterEntries(snapshot) {
  return [
    ...Object.entries(snapshot).filter(([key]) => !["status", "allowed_host", "connect_attempts"].includes(key)),
    ...Object.entries(snapshot.connect_attempts).map(([key, value]) => [`connect_attempts.${key}`, value]),
  ];
}

// The single browserLaunchPolicy control is the only blocked request in the
// window; at least one allowed tunnel carried the session.
function connectWindowProven(before, after) {
  if (before === null || after === null) return false;
  const baseline = new Map(counterEntries(before));
  if (!counterEntries(after).every(([key, value]) => value < COUNTER_CLAMP && value >= baseline.get(key))) return false;
  const delta = (key) => after[key] - before[key];
  const attemptDelta = (key) => after.connect_attempts[key] - before.connect_attempts[key];
  return attemptDelta("other") === 0 && delta("other_blocked_count") === 0 &&
    delta("telegram_attempts") === 1 && attemptDelta("telegram-suffix") === 1 && delta("blocked_requests") === 1 &&
    delta("upstream_failures") === 0 && delta("allowed_connects") >= 1;
}

const INT64_MIN = -(1n << 63n);
const INT64_MAX = (1n << 63n) - 1n;

function int64(value, { nonnegative = false, positive = false } = {}) {
  if (typeof value !== "string" || !/^(?:0|-?[1-9][0-9]{0,18})$/u.test(value)) {
    throw new Error("binding evidence invalid");
  }
  const parsed = BigInt(value);
  if (parsed < INT64_MIN || parsed > INT64_MAX || (nonnegative && parsed < 0n) || (positive && parsed < 1n)) {
    throw new Error("binding evidence invalid");
  }
  return parsed;
}

function bindingCut(snapshot) {
  const fields = [
    "complete", "evidence_version", "epoch", "additions", "removals", "last_added_created_at",
    "bound_key_ids", "bound_key_created_ats", "pending_count", "db_time",
  ];
  if (!exactKeys(snapshot, fields) || snapshot.complete !== true || snapshot.evidence_version !== 1 ||
      !Array.isArray(snapshot.bound_key_ids) || !Array.isArray(snapshot.bound_key_created_ats) ||
      snapshot.bound_key_ids.length !== snapshot.bound_key_created_ats.length) {
    throw new Error("binding evidence unavailable");
  }
  const keys = new Map();
  for (let index = 0; index < snapshot.bound_key_ids.length; index++) {
    const id = snapshot.bound_key_ids[index];
    const createdAt = snapshot.bound_key_created_ats[index];
    int64(id);
    int64(createdAt);
    if (keys.has(id)) throw new Error("binding evidence invalid");
    keys.set(id, createdAt);
  }
  return {
    version: snapshot.evidence_version,
    epoch: int64(snapshot.epoch, { positive: true }).toString(),
    additions: int64(snapshot.additions, { nonnegative: true }),
    removals: int64(snapshot.removals, { nonnegative: true }),
    lastAddedCreatedAt: snapshot.last_added_created_at === null
      ? null : int64(snapshot.last_added_created_at),
    keys,
    pending: int64(snapshot.pending_count, { nonnegative: true }),
    dbTime: int64(snapshot.db_time),
  };
}

function sameBoundKeys(baseline, current) {
  return baseline.size === current.size && [...baseline].every(([id, createdAt]) => current.get(id) === createdAt);
}

// Browser stdout has a separate inherited descriptor going directly to run.sh.
// This probe receives Docker lifecycle status, never browser data or callbacks.
// Preflight shares every check and stops at the QA preflight instead of the
// credential forward; it takes no final binding cut and proves no cleanup.
export async function probeAcceptance(webRevision, io, { preflight = false } = {}) {
  const result = emptyResult();
  let failureCode = "runtime-unavailable";
  let browser;
  let readBindings;
  let provenanceBefore;
  let observerBefore;
  let countersBefore = null;
  let webManifestBefore;
  try {
    const before = await io.runtimeSnapshot();
    if (!Array.isArray(before) || before.length === 0) return result;
    failureCode = "fingerprint-unproven";
    webManifestBefore = await io.webManifest();
    if (!validWebManifest(webManifestBefore, webRevision)) { result.code = failureCode; return result; }
    failureCode = "revision-unproven";
    if (!validRuntime(before)) { result.code = failureCode; return result; }
    provenanceBefore = resolveServerProvenance(before, await io.buildIdentities());
    result.revisionMatched = provenanceBefore !== null;
    if (!result.revisionMatched) { result.code = failureCode; return result; }
    result.serverSourceCommit = provenanceBefore.sourceCommit;
    const flags = runtimeFlags(before);
    result.registrationClosed = flags.every((values) => values.get("TG_REGISTRATION") === "closed");
    result.loginCodesDisabled = flags.every((values) => values.get("TG_LOG_LOGIN_CODES") === "false");
    if (!result.registrationClosed || !result.loginCodesDisabled) {
      result.code = "environment-unproven";
      return result;
    }

    failureCode = "runtime-unavailable";
    const containers = await io.prepareBrowser();
    failureCode = "network-unproven";
    result.networkIsolated = isolatedNetwork(await io.networkSnapshot(), containers);
    if (!result.networkIsolated) { result.code = failureCode; return result; }
    failureCode = "fingerprint-unproven";
    const fingerprint = webManifestBefore.target.fingerprint;
    result.fingerprintMatched = flags.every((values) => fingerprintPinned(values.get("TG_RSA_KEY_FINGERPRINT"), fingerprint));
    if (!result.fingerprintMatched) { result.code = failureCode; return result; }

    failureCode = "database-unavailable";
    readBindings = await io.openDatabase();
    const initial = bindingCut(await readBindings());
    const baseline = initial.keys;
    result.boundBefore = Math.min(100, baseline.size);
    result.pendingBefore = Number(initial.pending > 100n ? 100n : initial.pending);

    failureCode = "network-unproven";
    observerBefore = await io.observerSnapshot();
    if (!exactKeys(observerBefore, ["id", "startedAt", "running"]) || observerBefore.id !== containers.observerId ||
        observerBefore.running !== true || !Number.isFinite(Date.parse(observerBefore.startedAt))) {
      result.networkIsolated = false;
      result.code = failureCode;
      return result;
    }
    try { countersBefore = observerCounters(await io.observerCounters(containers.observerId)); } catch { /* fails closed below */ }
    if (countersBefore === null || !counterEntries(countersBefore).every(([, value]) => value < COUNTER_CLAMP)) {
      result.networkIsolated = false;
      result.code = failureCode;
      return result;
    }
    failureCode = "cleanup-unproven";
    browser = io.launchBrowser();
    let browserStatus = 1;
    try { browserStatus = await browser; } catch { /* final evidence still determines whether cleanup is proven */ }
    result.browserExitOk = browserStatus === 0;

    failureCode = "runtime-changed";
    const after = await io.runtimeSnapshot();
    const provenanceAfter = resolveServerProvenance(after, await io.buildIdentities());
    const webManifestAfter = await io.webManifest();
    // Read counters before the identity so a restart before this read is caught as runtime-changed.
    let countersAfter = null;
    try { countersAfter = observerCounters(await io.observerCounters(containers.observerId)); } catch { /* fails closed below */ }
    const observerAfter = await io.observerSnapshot();
    const afterFlags = Array.isArray(after) ? runtimeFlags(after) : [];
    result.containersStable = validRuntime(after) && sameRuntime(before, after) &&
      provenanceAfter !== null && provenanceAfter.sourceCommit === provenanceBefore.sourceCommit &&
      JSON.stringify(provenanceAfter.selectedIdentities) === JSON.stringify(provenanceBefore.selectedIdentities) &&
      sameObserver(observerBefore, observerAfter, containers.observerId) &&
      validWebManifest(webManifestAfter, webRevision) && sameWebManifest(webManifestBefore, webManifestAfter) &&
      afterFlags.every((values) => values.get("TG_REGISTRATION") === "closed" &&
        values.get("TG_LOG_LOGIN_CODES") === "false" &&
        fingerprintPinned(values.get("TG_RSA_KEY_FINGERPRINT"), fingerprint));
    if (!result.containersStable) { result.code = failureCode; return result; }
    failureCode = "network-unproven";
    result.networkIsolated = isolatedNetwork(await io.networkSnapshot(), containers);
    if (!result.networkIsolated) { result.code = failureCode; return result; }
    if (preflight && !result.browserExitOk) { result.code = "browser-failed"; return result; }
    result.networkIsolated = connectWindowProven(countersBefore, countersAfter);
    if (!result.networkIsolated) { result.code = failureCode; return result; }
    if (preflight) {
      result.ok = true;
      result.code = "ok";
      return result;
    }
    failureCode = "cleanup-unproven";
    const final = bindingCut(await readBindings());
    const remaining = final.keys;
    result.boundAfter = Math.min(100, remaining.size);
    result.pendingAfter = Number(final.pending > 100n ? 100n : final.pending);
    const sameEvidence = final.version === initial.version && final.epoch === initial.epoch;
    const additionDelta = final.additions - initial.additions;
    const removalDelta = final.removals - initial.removals;
    const witnessInRunWindow = final.dbTime >= initial.dbTime &&
      final.lastAddedCreatedAt !== null && final.lastAddedCreatedAt >= initial.dbTime &&
      final.lastAddedCreatedAt <= final.dbTime;
    result.newBindingProven = sameEvidence && additionDelta === 1n && removalDelta === 1n &&
      witnessInRunWindow;
    result.cleanupProven = result.newBindingProven && sameBoundKeys(baseline, remaining) &&
      final.pending <= initial.pending;
    result.ok = result.cleanupProven && result.browserExitOk;
    result.code = result.ok ? "ok" : result.cleanupProven ? "browser-failed" : "cleanup-unproven";
  } catch {
    if (browser) await browser.catch(() => undefined);
    result.code = failureCode;
  } finally {
    if (readBindings?.close) {
      try { await readBindings.close(); }
      catch { result.ok = false; result.code = browser ? "cleanup-unproven" : "database-unavailable"; }
    }
  }
  return result;
}

const PROJECTION = '{"id":{{json .Id}},"imageId":{{json .Image}},"revision":{{json (index .Config.Labels "org.opencontainers.image.revision")}},"labels":{{json .Config.Labels}},"startedAt":{{json .State.StartedAt}},"running":{{json .State.Running}},"flags":[{{range .Config.Env}}{{$parts := split . "="}}{{$name := index $parts 0}}{{if or (eq $name "TG_REGISTRATION") (eq $name "TG_LOG_LOGIN_CODES") (eq $name "TG_RSA_KEY_FINGERPRINT")}}{{json .}},{{end}}{{end}}null]}';
const CLASSIFICATION = '{"id":{{json .Id}},"path":{{json .Path}},"service":{{json (index .Config.Labels "com.docker.compose.service")}}}';
const IMAGE_PROJECTION = '{"id":{{json .Id}},"revision":{{json (index .Config.Labels "org.opencontainers.image.revision")}},"labels":{{json .Config.Labels}}}';
const NETWORK_PROJECTION = '{"internal":{{json .Internal}},"members":[{{range $id, $member := .Containers}}{{json $id}},{{end}}null]}';
const OBSERVER_PROJECTION = '{"id":{{json .Id}},"startedAt":{{json .State.StartedAt}},"running":{{json .State.Running}}}';
const MAX_OUTPUT = 1_048_576;
// Inside the image's package tree so the QA runner resolves the pinned Playwright install.
export const QA_SCRIPT_TARGET = "/opt/browser-acceptance/approved-qa.mjs";

function runnerEvidenceLocation() {
  if (process.getuid() !== 0 && process.env.ROLLOUT_RUNNER_TEST_MODE === "1") {
    const root = process.env.ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT;
    if (typeof root !== "string" || !isAbsolute(root)) throw new Error("runner evidence root invalid");
    return { root, owner: process.getuid() };
  }
  return { root: "/root", owner: 0 };
}

function parseRunnerBuildIdentity(text, evidenceIdentity) {
  if (!text.endsWith("\n")) throw new Error("runner build identity invalid");
  const line = text.slice(0, -1);
  if (line.includes("\n") || line.includes("\r")) throw new Error("runner build identity invalid");
  const match = /^captured_at_utc=(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ) source_sha=([0-9a-f]{40}) image_id=(sha256:[0-9a-f]{64}) revision_label_json=("[0-9a-f]{40}")$/u.exec(line);
  if (!match || new Date(match[1]).toISOString().replace(".000Z", "Z") !== match[1]) {
    throw new Error("runner build identity invalid");
  }
  const revisionLabel = JSON.parse(match[4]);
  return {
    capturedAtUtc: match[1], sourceSha: match[2], imageId: match[3], revisionLabel, evidenceIdentity,
  };
}

// Bigint stats; the owner is root in production and the test user under the runner test gate.
export function runnerEvidenceDirectoryValid(stat, expectedOwner) {
  return stat.isDirectory() && stat.uid === BigInt(expectedOwner) && (stat.mode & 0o7777n) === 0o700n;
}

export function runnerEvidenceFileValid(stat, expectedOwner) {
  return stat.isFile() && stat.uid === BigInt(expectedOwner) && (stat.mode & 0o7777n) === 0o600n &&
    stat.nlink === 1n && stat.size <= 1024n;
}

async function readBuildIdentity(rootDirectory, entry, expectedOwner) {
  const directory = await open(`/proc/self/fd/${rootDirectory.fd}/${entry}`,
    constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  try {
    const directoryStat = await directory.stat({ bigint: true });
    if (!runnerEvidenceDirectoryValid(directoryStat, expectedOwner)) throw new Error("runner evidence directory invalid");
    let file;
    try {
      file = await open(`/proc/self/fd/${directory.fd}/built-image.identity`,
        constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    } catch (error) {
      if (error.code === "ENOENT") return null;
      throw error;
    }
    try {
      const fileStat = await file.stat({ bigint: true });
      if (!runnerEvidenceFileValid(fileStat, expectedOwner)) throw new Error("runner build identity file invalid");
      const data = Buffer.alloc(1025);
      let size = 0;
      while (size < data.length) {
        const { bytesRead } = await file.read(data, size, data.length - size, null);
        if (bytesRead === 0) break;
        size += bytesRead;
      }
      if (size > 1024) throw new Error("runner build identity file invalid");
      const text = data.subarray(0, size).toString("utf8");
      const evidenceIdentity = JSON.stringify([
        entry, fileStat.dev.toString(), fileStat.ino.toString(), fileStat.ctimeNs.toString(),
        fileStat.mtimeNs.toString(), fileStat.size.toString(), text,
      ]);
      return parseRunnerBuildIdentity(text, evidenceIdentity);
    } finally {
      await file.close();
    }
  } finally {
    await directory.close();
  }
}

async function runnerBuildIdentities() {
  const { root, owner } = runnerEvidenceLocation();
  const rootDirectory = await open(root, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  try {
    const rootStat = await rootDirectory.stat({ bigint: true });
    if (!runnerEvidenceDirectoryValid(rootStat, owner)) throw new Error("runner evidence root invalid");
    const entries = await readdir(`/proc/self/fd/${rootDirectory.fd}`);
    const identities = [];
    for (const entry of entries.filter((name) => name.endsWith(".build")).sort()) {
      const identity = await readBuildIdentity(rootDirectory, entry, owner);
      if (identity !== null) identities.push(identity);
    }
    return identities;
  } finally {
    await rootDirectory.close();
  }
}

function command(file, args, { input = "", env = process.env, discard = false, passFd, maxOutput = MAX_OUTPUT } = {}) {
  return new Promise((resolveCommand, reject) => {
    const stdio = [input ? "pipe" : "ignore", discard ? "ignore" : "pipe", "ignore"];
    if (passFd !== undefined) stdio.push("ignore", passFd);
    const child = spawn(file, args, { env, stdio });
    const chunks = [];
    let size = 0;
    let failed = false;
    const timer = setTimeout(() => { failed = true; child.kill("SIGKILL"); }, 5_000);
    child.stdout?.on("data", (chunk) => {
      size += chunk.length;
      if (size > maxOutput) { failed = true; child.kill("SIGKILL"); }
      else chunks.push(chunk);
    });
    child.stdin?.on("error", () => { failed = true; });
    child.once("error", () => { failed = true; });
    child.once("close", (status) => {
      clearTimeout(timer);
      if (failed || status !== 0) reject(new Error("host command failed"));
      else resolveCommand(Buffer.concat(chunks).toString("utf8"));
    });
    child.stdin?.end(input);
  });
}

function projectedArray(value) {
  if (!Array.isArray(value) || value.at(-1) !== null) throw new Error("host projection invalid");
  return value.slice(0, -1);
}

async function runtimeSnapshot() {
  // Enumerate all running containers, including replicas with other image tags.
  // Classification has no env fields; only telegramd gets the three-key projection.
  const ids = (await command("docker", ["ps", "--quiet", "--no-trunc", "--filter", "status=running"])).trim().split("\n").filter(Boolean);
  const servers = [];
  for (const id of ids) {
    if (!/^[0-9a-f]{64}$/u.test(id)) throw new Error("container identity invalid");
    const classification = JSON.parse(await command("docker", ["container", "inspect", "--format", CLASSIFICATION, id]));
    if (classification.id !== id || typeof classification.path !== "string") throw new Error("container identity invalid");
    if (classification.path.split("/").at(-1) !== "telegramd" &&
        !/^telegramd(?:[-_].*)?$/u.test(classification.service ?? "")) continue;
    const server = JSON.parse(await command("docker", ["container", "inspect", "--format", PROJECTION, id]));
    if (server.id !== id || !/^sha256:[0-9a-f]{64}$/u.test(server.imageId)) throw new Error("container image invalid");
    server.flags = projectedArray(server.flags);
    const image = JSON.parse(await command("docker", ["image", "inspect", "--format", IMAGE_PROJECTION, server.imageId]));
    if (image.id !== server.imageId) throw new Error("container image invalid");
    server.imageRevision = image.revision;
    server.imageLabels = image.labels;
    servers.push(server);
  }
  return servers;
}

async function privateFile(path) {
  const file = await open(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
  try {
    const stat = await file.stat();
    if (!stat.isFile() || (stat.mode & 0o7777) !== 0o600 || stat.uid !== process.getuid() || stat.nlink !== 1) {
      throw new Error("protected file invalid");
    }
    const data = Buffer.alloc(8193);
    let size = 0;
    while (size < data.length) {
      const { bytesRead } = await file.read(data, size, data.length - size, null);
      if (bytesRead === 0) break;
      size += bytesRead;
    }
    if (size > 8192) throw new Error("protected file invalid");
    return { file, text: data.subarray(0, size).toString("utf8") };
  } catch (error) {
    await file.close();
    throw error;
  }
}

function privateDatabaseHost(host) {
  if (host === "::1" || /^\/[A-Za-z0-9_./-]+$/u.test(host)) return true;
  if (isIP(host) !== 4) return false;
  const [first, second] = host.split(".").map(Number);
  return first === 127 || first === 10 || (first === 172 && second >= 16 && second <= 31) ||
    (first === 192 && second === 168);
}

function passwordEntry(text) {
  const line = text.endsWith("\n") ? text.slice(0, -1) : text;
  if (/[\r\n\0]/u.test(line)) throw new Error("probe credential invalid");
  const fields = [""];
  for (let i = 0; i < line.length; i++) {
    if (line[i] === "\\") {
      if (![":", "\\"].includes(line[++i])) throw new Error("probe credential invalid");
      fields[fields.length - 1] += line[i];
    } else if (line[i] === ":") fields.push("");
    else fields[fields.length - 1] += line[i];
  }
  const [host, port, database, user, password] = fields;
  if (fields.length !== 5 || !privateDatabaseHost(host) || !/^[0-9]{1,5}$/u.test(port) ||
      Number(port) < 1 || Number(port) > 65535 || !/^[A-Za-z0-9_-]{1,63}$/u.test(database) ||
      user !== "browser_acceptance_probe" || !password || password.length > 1024) throw new Error("probe credential invalid");
  return { host, port, database };
}

async function openDatabase(credentialPath, configPath) {
  const configFile = await privateFile(configPath);
  let config;
  try { config = JSON.parse(configFile.text); }
  finally { await configFile.file.close(); }
  if (!exactKeys(config, ["userId"]) || typeof config.userId !== "string" ||
      !/^[1-9][0-9]{0,18}$/u.test(config.userId) || BigInt(config.userId) >= 1n << 63n) {
    throw new Error("probe database config invalid");
  }
  const credentialFile = await privateFile(credentialPath);
  let credential;
  try { credential = passwordEntry(credentialFile.text); }
  catch (error) { await credentialFile.file.close(); throw error; }
  const env = {
    PATH: process.env.PATH, LANG: "C", PGHOST: credential.host, PGPORT: credential.port,
    PGDATABASE: credential.database, PGUSER: "browser_acceptance_probe",
    PGSSLMODE: "disable", PGCONNECT_TIMEOUT: "2", PGPASSFILE: "/proc/self/fd/4",
    PGOPTIONS: "-c default_transaction_read_only=on -c statement_timeout=2000",
  };
  // One atomic versioned function supplies both cuts. The user ID is sent over
  // a host-private pipe, never argv; bigint and timestamp data stay exact text.
  const query = `SELECT pg_catalog.json_build_object(
    'complete', cut.complete,
    'evidence_version', cut.evidence_version,
    'epoch', cut.epoch::text,
    'additions', cut.additions::text,
    'removals', cut.removals::text,
    'last_added_created_at', CASE WHEN cut.last_added_created_at IS NULL THEN NULL
      ELSE (EXTRACT(EPOCH FROM cut.last_added_created_at) * 1000000)::bigint::text END,
    'bound_key_ids', CASE WHEN cut.complete THEN COALESCE((
      SELECT pg_catalog.json_agg(item.id::text ORDER BY item.ordinal)
      FROM pg_catalog.unnest(cut.bound_key_ids) WITH ORDINALITY AS item(id, ordinal)), '[]'::json) END,
    'bound_key_created_ats', CASE WHEN cut.complete THEN COALESCE((
      SELECT pg_catalog.json_agg((EXTRACT(EPOCH FROM item.created_at) * 1000000)::bigint::text ORDER BY item.ordinal)
      FROM pg_catalog.unnest(cut.bound_key_created_ats) WITH ORDINALITY AS item(created_at, ordinal)), '[]'::json) END,
    'pending_count', cut.pending_count::text,
    'db_time', CASE WHEN cut.db_time IS NULL THEN NULL
      ELSE (EXTRACT(EPOCH FROM cut.db_time) * 1000000)::bigint::text END)
  FROM public.browser_acceptance_binding_snapshot_v1(${config.userId}::bigint) AS cut;\n`;
  const readBindings = async () => JSON.parse(await command("psql", ["-X", "--no-password", "--quiet", "--tuples-only", "--no-align", "--set", "ON_ERROR_STOP=1"],
    { input: query, env, passFd: credentialFile.file.fd }));
  readBindings.close = () => credentialFile.file.close();
  return readBindings;
}

function validateRelease(record) {
  if (!exactKeys(record, ["archiveSha256", "contentDigest", "sourceCommit", "url"]) ||
      typeof record.sourceCommit !== "string" || !/^[0-9a-f]{40}$/u.test(record.sourceCommit) ||
      !/^sha256:[0-9a-f]{64}$/u.test(record.archiveSha256) || !/^sha256:[0-9a-f]{64}$/u.test(record.contentDigest) ||
      typeof record.url !== "string") throw new Error("release record invalid");
  const url = new URL(record.url);
  if (url.protocol !== "https:" || url.hostname !== ALLOWED_HOST || url.port || url.username || url.password ||
      url.hash || url.href !== record.url || url.search !== `?v=${record.sourceCommit}`) throw new Error("release record invalid");
}

async function targetMetadata(record) {
  const url = new URL("mtproto-target.json", record.url);
  url.search = `?v=${record.sourceCommit}`;
  const raw = await command("curl", ["-q", "--noproxy", "*", "--proto", "=https", "--fail", "--silent",
    "--max-time", "3", "--max-filesize", "16385", url.href]);
  if (Buffer.byteLength(raw) > 16384) throw new Error("target metadata invalid");
  const target = JSON.parse(raw);
  if (!exactKeys(target, ["artifactDigest", "endpoint", "fingerprint", "mode", "sourceCommit"]) ||
      target.mode !== "private" || target.sourceCommit !== record.sourceCommit || target.artifactDigest !== record.contentDigest ||
      typeof target.fingerprint !== "string" || !/^[0-9a-f]{16}$/u.test(target.fingerprint) ||
      typeof target.endpoint !== "string" || !target.endpoint.startsWith(`wss://${ALLOWED_HOST}/`) || /[?#]/u.test(target.endpoint)) {
    throw new Error("target metadata invalid");
  }
  return target;
}

async function verifiedWebManifest(manifestPath) {
  const record = JSON.parse(await readFile(manifestPath, "utf8"));
  validateRelease(record);
  return { ...record, target: await targetMetadata(record) };
}

function launchBrowser(id, preflight) {
  return new Promise((resolveBrowser) => {
    // fd 3 belongs to run.sh's output validator. The probe cannot read it and
    // neither the protected config nor the probe is mounted into this container.
    // Preflight never attaches stdin, so no credential pipe can reach the QA process.
    const qa = ["node", QA_SCRIPT_TARGET, "--mode", preflight ? "preflight" : "acceptance",
      "--release-record", "/run/release-record.json", "--runtime-adapter", "/opt/browser-acceptance/qa-runtime-adapter.mjs"];
    const child = spawn("docker", preflight ? ["exec", id, ...qa] : ["exec", "--interactive", id, ...qa],
      { stdio: [preflight ? "ignore" : 0, 3, "ignore"] });
    let failed = false;
    const timer = setTimeout(() => { failed = true; child.kill("SIGKILL"); }, 180_000);
    child.once("error", () => { failed = true; });
    child.once("close", (status) => { clearTimeout(timer); resolveBrowser(failed ? 1 : status); });
  });
}

async function main() {
  let result = emptyResult();
  result.code = "arguments-invalid";
  try {
    const options = new Map();
    const args = process.argv.slice(2);
    for (let i = 0; i < args.length; i += 2) {
      if (!["--mode", "--manifest", "--script", "--credential", "--config"].includes(args[i]) ||
          options.has(args[i]) || !args[i + 1]) throw new Error("probe arguments invalid");
      options.set(args[i], args[i + 1]);
    }
    if (options.size !== 5 || !["acceptance", "preflight"].includes(options.get("--mode"))) {
      throw new Error("probe arguments invalid");
    }
    const preflight = options.get("--mode") === "preflight";
    const record = JSON.parse(await readFile(options.get("--manifest"), "utf8"));
    validateRelease(record);
    const directory = dirname(fileURLToPath(import.meta.url));
    const compose = ["compose", "--env-file", "/dev/null", "--project-directory", directory, "--file", `${directory}/compose.yaml`,
      "--project-name", "telegram-browser-acceptance"];
    let containers;
    result = await probeAcceptance(record.sourceCommit, {
      runtimeSnapshot,
      buildIdentities: runnerBuildIdentities,
      webManifest: () => verifiedWebManifest(options.get("--manifest")),
      async prepareBrowser() {
        // Keep an idle container attached while checking topology. The QA
        // process is started via exec only after every host precondition passes.
        const override = JSON.stringify({ services: { browser: {
          entrypoint: ["node", "-e", "setTimeout(() => {}, 300000)"], command: [],
          volumes: [
            { type: "bind", source: resolve(options.get("--manifest")), target: "/run/release-record.json", read_only: true },
            { type: "bind", source: resolve(options.get("--script")), target: QA_SCRIPT_TARGET, read_only: true },
          ],
        } } });
        await command("docker", [...compose, "--file", "-", "up", "--detach", "--wait", "--no-deps", "browser"], { input: override, discard: true });
        containers = {
          browserId: (await command("docker", [...compose, "ps", "--all", "--quiet", "browser"])).trim(),
          observerId: (await command("docker", [...compose, "ps", "--all", "--quiet", "observer"])).trim(),
        };
        return containers;
      },
      async networkSnapshot() {
        const network = JSON.parse(await command("docker", ["network", "inspect", "--format", NETWORK_PROJECTION, "telegram-browser-acceptance-internal"]));
        network.members = projectedArray(network.members);
        return network;
      },
      // Fixed in-image script, no stdin; the browser cannot supply or redirect this evidence.
      observerCounters: (id) => command("docker", ["exec", id, "node", "/opt/browser-acceptance/observer-snapshot.mjs"],
        { maxOutput: OBSERVER_OUTPUT_LIMIT }),
      async observerSnapshot() {
        const id = (await command("docker", [...compose, "ps", "--all", "--quiet", "observer"])).trim();
        if (!/^[0-9a-f]{64}$/u.test(id)) throw new Error("observer identity invalid");
        const snapshot = JSON.parse(await command("docker", ["container", "inspect", "--format", OBSERVER_PROJECTION, id]));
        if (snapshot.id !== id) throw new Error("observer identity invalid");
        return snapshot;
      },
      openDatabase: () => openDatabase(options.get("--credential"), options.get("--config")),
      launchBrowser: () => launchBrowser(containers.browserId, preflight),
      pause: () => pause(50),
    }, { preflight });
  } catch {
    // The only public failure surface is the fixed proof schema below.
    result.ok = false;
  }
  process.stdout.write(`${JSON.stringify(result)}\n`);
  process.exitCode = result.ok ? 0 : 1;
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) await main();
