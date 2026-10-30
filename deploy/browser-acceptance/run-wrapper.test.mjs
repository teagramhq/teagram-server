import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createServer } from "node:http";
import { access, chmod, copyFile, mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const runScript = join(repoRoot, "deploy/browser-acceptance/run.sh");
const sharedRuntimeLockPath = "/tmp/teagram-browser-acceptance.lock";
const busyboxPhaseScript = join(repoRoot, ".github/scripts/run-busybox-phase-check.sh");
const sourceCommit = "c".repeat(40);
const phaseCommandToken = "e".repeat(64);
const digest = `sha256:${"d".repeat(64)}`;
// Four account-slot sockets plus one sign-in replacement and one logout reconnect.
const WSS_CONNECTION_CEILING = 6;
const approvedQaDigest = readFileSync(runScript, "utf8")
  .match(/^readonly APPROVED_QA_SCRIPT_SHA256="([0-9a-f]{64})"$/mu)?.[1];
assert.ok(approvedQaDigest, "runner must pin the approved QA script digest");
const blockedResult = "{\"status\":\"blocked_expected\",\"blocked_telegram_org_attempts\":1,\"telegram_org_upstream_connects\":0,\"payloads_retained\":0}";
const readyResult = "{\"status\":\"ready\",\"http_status\":200,\"asset_502_count\":0,\"browser_wss_status\":101,\"browser_wss_unique_targets\":1,\"observer_success_hosts\":1,\"observer_target_host\":\"teagram.tailaa4918.ts.net\",\"telegram_org_attempts\":0,\"payloads_retained\":0}";
const acceptanceResultValue = {
  schemaVersion: 3,
  ok: true,
  phase: "signed-in",
  outcome: "ok",
  logout: "ok",
  targetHost: "teagram.tailaa4918.ts.net",
  sourceCommit,
  wssConnections: 1,
  qaFailureCode: "none",
  submission: "password",
  chatList: "rendered",
  chatListRows: 2,
  wssEndpointMatched: true,
};
const acceptanceResult = JSON.stringify(acceptanceResultValue);
const acceptanceV2ResultValue = {
  schemaVersion: 2, ok: true, phase: "signed-in", outcome: "ok", logout: "ok",
  targetHost: "teagram.tailaa4918.ts.net", sourceCommit, wssConnections: 1,
};
const acceptanceV2Result = JSON.stringify(acceptanceV2ResultValue);
const probeResultValue = {
  schemaVersion: 5, ok: true, code: "ok", serverSourceCommit: "f".repeat(40),
  registrationMode: "open", accountEvidence: "complete", accountsUnchanged: true,
  loginCodesDisabled: true, revisionMatched: true,
  containersStable: true, networkIsolated: true, fingerprintMatched: true,
  newBindingProven: true, cleanupProven: true, browserExitOk: true,
  boundBefore: 2, boundAfter: 2, pendingBefore: 0, pendingAfter: 0,
  bindingEvidence: "complete", bindingAdditions: 1, bindingRemovals: 1,
  witnessInWindow: true, baselineIntact: true, topologyIsolated: true,
  captureWindow: "complete", captureAllowedConnects: 1, captureTelegramSuffix: 1,
  captureOther: 0, captureBlocked: 1, captureUpstreamFailures: 0,
};

function preflightProbeValue(overrides = {}) {
  const value = {
    ...probeResultValue, accountsUnchanged: false,
    newBindingProven: false, cleanupProven: false, boundAfter: 0, pendingAfter: 0,
    bindingEvidence: "not-reached", bindingAdditions: 0, bindingRemovals: 0,
    witnessInWindow: false, baselineIntact: false,
    ...overrides,
  };
  if (value.code === "database-unavailable") value.databaseFailureCategory ??= "unknown";
  else delete value.databaseFailureCategory;
  return value;
}
const preflightReady = `{"status":"acceptance-ready","web_source_commit":"${sourceCommit}","server_source_commit":"${"f".repeat(40)}",` +
  '"registration_mode":"open","login_codes_disabled":true,"server_revision_proven":true,"runtime_stable":true,' +
  '"network_isolated":true,"fingerprint_matched":true,"binding_evidence_complete":true,"account_evidence_complete":true,"qa_preflight_ready":true}';

function acceptanceOutput(overrides = {}) {
  return JSON.stringify({ ...acceptanceResultValue, ...overrides });
}

function acceptanceFailureOutput(overrides = {}) {
  return JSON.stringify({
    ...acceptanceResultValue,
    ok: false,
    phase: "password-check",
    outcome: "credential-rejected",
    logout: "not-attempted",
    wssConnections: 1,
    qaFailureCode: "authentication-failed-once",
    submission: "password",
    chatList: "not-reached",
    chatListRows: 0,
    wssEndpointMatched: false,
    ...overrides,
  });
}

function browserFailedProbeValue(overrides = {}) {
  return { ...probeResultValue, ok: false, code: "browser-failed", browserExitOk: false, ...overrides };
}

const probeV5ResultValue = {
  ...probeResultValue,
  schemaVersion: 5,
  bindingEvidence: "complete",
  bindingAdditions: 1,
  bindingRemovals: 1,
  witnessInWindow: true,
  baselineIntact: true,
  topologyIsolated: true,
  captureWindow: "complete",
  captureAllowedConnects: 1,
  captureTelegramSuffix: 1,
  captureOther: 0,
  captureBlocked: 1,
  captureUpstreamFailures: 0,
};
const probeV3ResultValue = { ...probeV5ResultValue, schemaVersion: 3 };
const legacyRegistrationProbeValue = { ...probeV5ResultValue, registrationClosed: true };
const qaV3ResultValue = {
  ...acceptanceResultValue,
  schemaVersion: 3,
  qaFailureCode: "none",
  submission: "password",
  chatList: "rendered",
  chatListRows: 2,
  wssEndpointMatched: true,
};

function evidenceLine(probe, qa, { qaResult = "valid", teardown = "ok" } = {}) {
  const qaFields = qaResult === "valid" ? {
    phase: qa.phase,
    outcome: qa.outcome,
    logout: qa.logout,
    wssConnections: qa.wssConnections,
    wssEndpointMatched: qa.wssEndpointMatched,
    submission: qa.submission,
    chatList: qa.chatList,
    chatListRows: qa.chatListRows,
    qaFailureCode: qa.qaFailureCode,
  } : {
    phase: "unknown", outcome: "unknown", logout: "unknown", wssConnections: 0,
    wssEndpointMatched: false, submission: "unknown", chatList: "unknown",
    chatListRows: 0, qaFailureCode: "unknown",
  };
  return JSON.stringify({
    evidenceVersion: 2,
    status: probe.ok && probe.code === "ok" && probe.accountsUnchanged && qaResult === "valid" && qa.ok && teardown === "ok"
      ? "accepted" : "error",
    code: probe.code,
    teardown,
    targetHost: "teagram.tailaa4918.ts.net",
    sourceCommit,
    serverSourceCommit: probe.serverSourceCommit,
    registrationMode: probe.registrationMode,
    accountEvidence: probe.accountEvidence,
    accountsUnchanged: probe.accountsUnchanged,
    loginCodesDisabled: probe.loginCodesDisabled,
    revisionMatched: probe.revisionMatched,
    containersStable: probe.containersStable,
    networkIsolated: probe.networkIsolated,
    fingerprintMatched: probe.fingerprintMatched,
    newBindingProven: probe.newBindingProven,
    cleanupProven: probe.cleanupProven,
    browserExitOk: probe.browserExitOk,
    boundBefore: probe.boundBefore,
    boundAfter: probe.boundAfter,
    pendingBefore: probe.pendingBefore,
    pendingAfter: probe.pendingAfter,
    bindingEvidence: probe.bindingEvidence,
    bindingAdditions: probe.bindingAdditions,
    bindingRemovals: probe.bindingRemovals,
    witnessInWindow: probe.witnessInWindow,
    baselineIntact: probe.baselineIntact,
    topologyIsolated: probe.topologyIsolated,
    captureWindow: probe.captureWindow,
    captureAllowedConnects: probe.captureAllowedConnects,
    captureTelegramSuffix: probe.captureTelegramSuffix,
    captureOther: probe.captureOther,
    captureBlocked: probe.captureBlocked,
    captureUpstreamFailures: probe.captureUpstreamFailures,
    qaResult,
    ...qaFields,
  });
}

function bindingNotCreatedProbe() {
  return {
    ...probeV5ResultValue,
    ok: false,
    code: "binding-not-created",
    browserExitOk: false,
    newBindingProven: false,
    cleanupProven: false,
    bindingAdditions: 0,
    bindingRemovals: 0,
    witnessInWindow: false,
  };
}

function revisionUnprovenProbe() {
  return {
    ...probeV5ResultValue,
    ok: false,
    code: "revision-unproven",
    serverSourceCommit: "",
    registrationMode: "unknown",
    accountEvidence: "not-reached",
    accountsUnchanged: false,
    loginCodesDisabled: false,
    revisionMatched: false,
    containersStable: false,
    networkIsolated: false,
    fingerprintMatched: false,
    newBindingProven: false,
    cleanupProven: false,
    browserExitOk: false,
    boundBefore: 0,
    boundAfter: 0,
    pendingBefore: 0,
    pendingAfter: 0,
    bindingEvidence: "not-reached",
    bindingAdditions: 0,
    bindingRemovals: 0,
    witnessInWindow: false,
    baselineIntact: false,
    topologyIsolated: false,
    captureWindow: "not-reached",
    captureAllowedConnects: 0,
    captureTelegramSuffix: 0,
    captureOther: 0,
    captureBlocked: 0,
    captureUpstreamFailures: 0,
  };
}

async function setup(t) {
  const directory = await mkdtemp(join(tmpdir(), "browser-acceptance-wrapper-"));
  const bin = join(directory, "bin");
  const dockerRoot = join(directory, "docker-root");
  const runtimeDir = join(directory, "runtime");
  const checkoutRoot = join(directory, "checkout");
  const fixtureRunScript = join(checkoutRoot, "deploy/browser-acceptance/run.sh");
  const sharedRuntimeLock = join(directory, "shared-browser-acceptance.lock");
  const manifest = join(directory, "release-record.json");
  const servedManifest = join(directory, "served-manifest.json");
  const log = join(directory, "docker-calls.log");
  const curlLog = join(directory, "curl-calls.log");
  await mkdir(bin);
  await mkdir(dockerRoot);
  await mkdir(runtimeDir, { mode: 0o700 });
  await mkdir(dirname(fixtureRunScript), { recursive: true });
  const wrapperSource = await readFile(runScript, "utf8");
  assert.equal(wrapperSource.split(sharedRuntimeLockPath).length - 1, 1);
  await writeFile(fixtureRunScript,
    wrapperSource.replace(sharedRuntimeLockPath, sharedRuntimeLock), { mode: 0o755 });
  await writeFile(manifest, JSON.stringify({
    sourceCommit,
    archiveSha256: digest,
    contentDigest: digest,
    url: `https://teagram.tailaa4918.ts.net/client/?v=${sourceCommit}`,
  }));
  await writeFile(servedManifest, JSON.stringify({
    mode: "private",
    endpoint: "wss://teagram.tailaa4918.ts.net/apiws",
    fingerprint: "0123456789abcdef",
    sourceCommit,
    artifactDigest: digest,
  }));
  await writeFile(join(bin, "docker"), `#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> "$WRAPPER_DOCKER_LOG"
if [[ "$1" == "info" ]]; then
  if [[ "$*" == *"DockerRootDir"* ]]; then printf '%s\\n' "$WRAPPER_DOCKER_ROOT"; else printf '%s\\n' "\${WRAPPER_DOCKER_ARCH:-aarch64}"; fi
  exit 0
fi
if [[ "$1" == "compose" ]]; then
  if [[ "$*" == *"version"* ]]; then exit 0; fi
  if [[ "$*" == *" build --pull browser"* && -n "\${WRAPPER_BUILD_STARTED_FILE:-}" ]]; then
    : >"$WRAPPER_BUILD_STARTED_FILE"
    while [[ ! -e "\${WRAPPER_RELEASE_BUILD_FILE:?}" ]]; do sleep 0.01; done
  fi
  if [[ "$*" == *" down --rmi local"* && -n "\${WRAPPER_CLEANUP_STARTED_FILE:-}" ]]; then
    : >"$WRAPPER_CLEANUP_STARTED_FILE"
    while [[ ! -e "\${WRAPPER_RELEASE_CLEANUP_FILE:?}" ]]; do sleep 0.01; done
  fi
  if [[ "$*" == *" build "* && "\${WRAPPER_BUILD_FAILURE:-0}" == "1" ]]; then exit 1; fi
  if [[ "$*" == *" down --rmi local"* && "\${WRAPPER_CLEANUP_FAILURE:-0}" == "1" ]]; then exit 1; fi
  if [[ "$*" == *" run "* ]]; then
    if [[ "$*" == *"/opt/browser-acceptance/approved-qa.mjs --mode acceptance"* ]]; then
      if [[ "\${WRAPPER_REQUIRE_CLOSED_STDIN:-0}" == "1" ]]; then
        IFS= read -r -t 0.1 input || exit 99
        [[ "$input" == "credential-sentinel" ]] || exit 98
      fi
      if [[ -v WRAPPER_ACCEPTANCE_OUTPUT ]]; then
        printf '%s' "$WRAPPER_ACCEPTANCE_OUTPUT"
      else
        printf '%s' '${acceptanceResult}'
      fi
      exit "\${WRAPPER_ACCEPTANCE_STATUS:-0}"
    else
      if [[ "\${WRAPPER_REQUIRE_CLOSED_STDIN:-0}" == "1" ]]; then
        if IFS= read -r -t 0.1 input; then exit 99; fi
      fi
      if [[ "\${WRAPPER_RUNTIME_FAILURE:-0}" == "1" ]]; then printf '%s\\n' '{"status":"error","code":"observer-unhealthy"}'; exit 1; fi
      if [[ "$*" == *" browser readiness "* ]]; then
        if [[ -n "\${WRAPPER_READINESS_OUTPUT:-}" ]]; then
          printf '%s\\n' "$WRAPPER_READINESS_OUTPUT"
          [[ "\${WRAPPER_READINESS_STATUS:-0}" == "0" ]] || exit 1
        else
          printf '%s\\n' '${readyResult}'
        fi
      else
        if [[ -n "\${WRAPPER_BLOCKED_OUTPUT:-}" ]]; then printf '%s\\n' "$WRAPPER_BLOCKED_OUTPUT"; else printf '%s\\n' '${blockedResult}'; fi
      fi
    fi
  fi
  exit 0
fi
exit 0
`);
  await writeFile(join(bin, "df"), `#!/usr/bin/env bash
printf '%s\\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on'
printf 'mock 5000000 0 3000000 0%% %s\\n' "$WRAPPER_DOCKER_ROOT"
`);
  await writeFile(join(bin, "curl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *"--version"* ]]; then printf '%s\\n' "curl \${WRAPPER_CURL_VERSION:-8.5.0} wrapper"; exit 0; fi
printf '%s\\n' "$*" >> "$WRAPPER_CURL_LOG"
case "\${WRAPPER_CURL_MODE:-}" in
  oversized-content-length) exit 63 ;;
  oversized-chunked) head -c 16385 /dev/zero | tr '\\000' x; exit 63 ;;
esac
  cat "$WRAPPER_SERVED_MANIFEST"
`);
  await writeFile(join(bin, "sha256sum"), `#!/usr/bin/env bash
set -eu
if [[ "\${WRAPPER_ACCEPT_QA_FIXTURE:-0}" == "1" ]]; then
  printf '%s  %s\\n' '${approvedQaDigest}' "\${@: -1}"
else
  exec /usr/bin/sha256sum "$@"
fi
`);
  await writeFile(join(bin, "node"), `#!/usr/bin/env bash
set -eu
if [[ "$1" == --input-type=module ]]; then exec '${process.execPath}' "$@"; fi
[[ "$1" == */host-probe.mjs ]] || exit 97
[[ ! -v ROLLOUT_RUNNER_TEST_MODE && ! -v ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT ]] || exit 96
printf '%s\\n' "host-probe $*" >> "$WRAPPER_DOCKER_LOG"
if [[ "$2 $3" == "--mode preflight" ]]; then
  if [[ "\${WRAPPER_PROBE_SKIP_BROWSER:-0}" != 1 ]]; then
    printf '%s\\n' 'qa child started' >&3
    printf '%s\\n' 'docker exec browser node /opt/browser-acceptance/approved-qa.mjs --mode preflight' >> "$WRAPPER_DOCKER_LOG"
    # The fake QA preflight fails if any byte reaches its stdin.
    if IFS= read -r -t 0.1 -n 1 input || [[ -n "\${input:-}" ]]; then
      : > "$WRAPPER_DOCKER_LOG.stdin-touched"
      printf '%s\\n' '{"status":"error","code":"credential-input-invalid"}' >&3
      printf '%s\\n' '${JSON.stringify(preflightProbeValue({ ok: false, code: "browser-failed", browserExitOk: false }))}'
      exit 1
    fi
    printf '%s\\n' "\${WRAPPER_PREFLIGHT_OUTPUT-browser acceptance preflight ready}" >&3
  fi
  if [[ -v WRAPPER_PROBE_OUTPUT ]]; then printf '%s\\n' "$WRAPPER_PROBE_OUTPUT";
  else printf '%s\\n' '${JSON.stringify(preflightProbeValue())}'; fi
  exit "\${WRAPPER_PROBE_STATUS:-0}"
fi
while (($#)); do
  case "$1" in
    --manifest) printf '%s\\n' "$2:/run/release-record.json:ro" >> "$WRAPPER_DOCKER_LOG"; shift 2 ;;
    --script) printf '%s\\n' "$2:/opt/browser-acceptance/approved-qa.mjs:ro" >> "$WRAPPER_DOCKER_LOG"; shift 2 ;;
    *) shift ;;
  esac
done
if [[ "\${WRAPPER_PROBE_SKIP_BROWSER:-0}" != 1 ]]; then
  printf '%s\\n' 'docker exec --interactive browser node /opt/browser-acceptance/approved-qa.mjs --mode acceptance' >> "$WRAPPER_DOCKER_LOG"
  if [[ "\${WRAPPER_REQUIRE_CLOSED_STDIN:-0}" == 1 ]]; then
    IFS= read -r -t 0.1 input || exit 99
    [[ "$input" == credential-sentinel ]] || exit 98
  fi
  if [[ "\${WRAPPER_ACCEPTANCE_NUL:-0}" == 1 ]]; then printf '\\000' >&3; fi
  if [[ -v WRAPPER_ACCEPTANCE_OUTPUT ]]; then printf '%s' "$WRAPPER_ACCEPTANCE_OUTPUT" >&3;
  else printf '%s' '${acceptanceResult}' >&3; fi
fi
if [[ -v WRAPPER_PROBE_OUTPUT ]]; then
  printf '%s\\n' "$WRAPPER_PROBE_OUTPUT"
elif [[ "\${WRAPPER_ACCEPTANCE_STATUS:-0}" != 0 ]]; then
  printf '%s\\n' '${JSON.stringify({ ...probeResultValue, ok: false, code: "browser-failed", browserExitOk: false })}'
else
  printf '%s\\n' '${JSON.stringify(probeResultValue)}'
fi
exit "\${WRAPPER_PROBE_STATUS:-\${WRAPPER_ACCEPTANCE_STATUS:-0}}"
`);
  await import("node:fs/promises").then(({ chmod }) => Promise.all([
    chmod(join(bin, "docker"), 0o755),
    chmod(join(bin, "df"), 0o755),
    chmod(join(bin, "curl"), 0o755),
    chmod(join(bin, "sha256sum"), 0o755),
    chmod(join(bin, "node"), 0o755),
  ]));
  t.after(async () => rm(directory, { recursive: true, force: true }));
  return { bin, directory, dockerRoot, runtimeDir, runScript: fixtureRunScript, sharedRuntimeLock,
    manifest, servedManifest, log, curlLog };
}

function invoke(paths, args, extraEnv = {}, input, cwd = repoRoot) {
  return invokeScript(paths, paths.runScript, args, extraEnv, input, cwd);
}

function invokeScript(paths, script, args, extraEnv = {}, input, cwd = repoRoot) {
  return spawnSync("bash", [script, ...args], {
    cwd,
    encoding: "utf8",
    ...(input === undefined ? {} : { input }),
    env: {
      ...process.env,
      PATH: `${paths.bin}:${process.env.PATH}`,
      WRAPPER_DOCKER_LOG: paths.log,
      WRAPPER_DOCKER_ROOT: paths.dockerRoot,
      WRAPPER_CURL_LOG: paths.curlLog,
      WRAPPER_SERVED_MANIFEST: paths.servedManifest,
      XDG_RUNTIME_DIR: paths.runtimeDir,
      ...extraEnv,
    },
  });
}

function spawnScript(paths, script, args, extraEnv = {}, cwd = repoRoot) {
  const child = spawn("bash", [script, ...args], {
    cwd,
    env: {
      ...process.env,
      PATH: `${paths.bin}:${process.env.PATH}`,
      WRAPPER_DOCKER_LOG: paths.log,
      WRAPPER_DOCKER_ROOT: paths.dockerRoot,
      WRAPPER_CURL_LOG: paths.curlLog,
      WRAPPER_SERVED_MANIFEST: paths.servedManifest,
      XDG_RUNTIME_DIR: paths.runtimeDir,
      ...extraEnv,
    },
  });
  let stdout = "";
  let stderr = "";
  child.stdout.setEncoding("utf8").on("data", (chunk) => { stdout += chunk; });
  child.stderr.setEncoding("utf8").on("data", (chunk) => { stderr += chunk; });
  const result = new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (status, signal) => resolve({ status, signal, stdout, stderr }));
  });
  return { child, result };
}

async function waitForFile(path) {
  const deadline = Date.now() + 5000;
  while (Date.now() < deadline) {
    try {
      await access(path);
      return;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw new Error(`timed out waiting for fixture file ${path}`);
}

async function ensureSharedLockFile(path) {
  let created = false;
  try {
    await writeFile(path, "", { flag: "wx", mode: 0o666 });
    created = true;
  } catch (error) {
    if (error.code !== "EEXIST") throw error;
  }
  if (created) await chmod(path, 0o666);
}

async function setFixtureLockPath(paths, lockFile) {
  const source = await readFile(paths.runScript, "utf8");
  assert.equal(source.split(paths.sharedRuntimeLock).length - 1, 1);
  await writeFile(paths.runScript, source.replace(paths.sharedRuntimeLock, lockFile), { mode: 0o755 });
}

async function installLockFdProbe(paths) {
  const flockPath = join(paths.bin, "flock");
  await writeFile(flockPath, `#!/usr/bin/env bash
set -eu
flags="$(awk '$1 == "flags:" { print $2 }' /proc/$$/fdinfo/9)"
printf 'LOCK_FD_FLAGS=%s\\n' "$flags" >&2
exec /usr/bin/flock "$@"
`, { mode: 0o755 });
}

function asUser(uid, gid, command, args, options = {}) {
  const env = options.env ?? process.env;
  if (uid === process.getuid() && gid === process.getgid()) {
    return { command, args, options: { ...options, env } };
  }
  const assignments = Object.entries(env)
    .filter(([, value]) => value !== undefined)
    .map(([name, value]) => `${name}=${value}`);
  const setprivArgs = ["--reuid", String(uid), "--regid", String(gid), "--clear-groups", "--",
    "env", ...assignments, command, ...args];
  if (process.getuid() === 0) {
    return { command: "/usr/bin/setpriv", args: setprivArgs, options: { ...options, env: process.env } };
  }
  return { command: "sudo", args: ["-n", "setpriv", ...setprivArgs], options: { ...options, env: process.env } };
}

function spawnAsUser(uid, gid, command, args, options = {}) {
  const invocation = asUser(uid, gid, command, args, options);
  return spawn(invocation.command, invocation.args, invocation.options);
}

function spawnSyncAsUser(uid, gid, command, args, options = {}) {
  const invocation = asUser(uid, gid, command, args, options);
  return spawnSync(invocation.command, invocation.args, invocation.options);
}

function lockFdFlags(stderr) {
  const match = stderr.match(/LOCK_FD_FLAGS=([0-7]+)/u);
  assert.ok(match, `expected lock descriptor flags in stderr, got: ${stderr}`);
  return Number.parseInt(match[1], 8);
}

function assertLockFdIsReadOnly(stderr) {
  const flags = lockFdFlags(stderr);
  assert.equal(flags & 0o3, 0o0, "lock descriptor should be opened read-only");
}

async function invokeBusyboxPhase(t, lane, { phase = "", mode = "valid", status = 1, outputMode = "standard" } = {}) {
  const directory = await mkdtemp(join(tmpdir(), "busybox-phase-protocol-"));
  const bin = join(directory, "bin");
  const argsLog = join(directory, "docker-args.log");
  await mkdir(bin);
  await writeFile(join(bin, "docker"), `#!/usr/bin/env bash
set -eu
if [[ "\${1:-}" == pull ]]; then
  [[ "\${2:-}" == mirror.gcr.io/library/node:24-alpine3.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a ]] || exit 95
  exit 0
fi
[[ "\${1:-}" == run ]] || exit 99
printf '%s\\n' "$@" >"\${FAKE_DOCKER_ARGS_FILE:?}"
phase_dir=''
while (($#)); do
  if [[ "$1" == --volume && $# -ge 2 ]]; then
    case "$2" in
      *:/phase:rw) phase_dir="\${2%:/phase:rw}" ;;
    esac
    shift 2
  else
    shift
  fi
done
[[ -n "$phase_dir" && -d "$phase_dir" ]] || exit 98
[[ "$(stat -c '%a' "$phase_dir")" == 700 && -z "$(ls -A "$phase_dir")" ]] || exit 96
case "\${FAKE_PHASE_MODE:-valid}" in
  missing) ;;
  valid) printf '%s' "\${FAKE_PHASE:?}" >"$phase_dir/current" ;;
  unknown) printf '%s' unknown-phase >"$phase_dir/current" ;;
  duplicate) printf '%s\\n%s' "\${FAKE_PHASE:?}" "\${FAKE_PHASE:?}" >"$phase_dir/current" ;;
  trailing) printf '%s\\n' "\${FAKE_PHASE:?}" >"$phase_dir/current" ;;
  oversized) head -c 1048576 /dev/zero >"$phase_dir/current" ;;
  symlink) ln -s /dev/null "$phase_dir/current" ;;
  non-regular) mkfifo "$phase_dir/current" ;;
  extra-file)
    printf '%s' "\${FAKE_PHASE:?}" >"$phase_dir/current"
    printf '%s' duplicate >"$phase_dir/second"
    ;;
  *) exit 97 ;;
esac
case "\${FAKE_OUTPUT_MODE:-standard}" in
  plain-unterminated)
    printf '%s' 'plain child output without a trailing newline'
    ;;
  forged-command-unterminated)
    printf '%s' '::error::forged after stop-command text literal-ci-password-sentinel literal-admin-token-sentinel'
    ;;
  standard)
    printf '%s\\n' \\
      '::error::forged phase canary literal-ci-password-sentinel literal-admin-token-sentinel' \\
      '::stop-commands::attacker' \\
      '::attacker::' \\
      '::error::forged after stop-command text'
    ;;
  *) exit 97 ;;
esac
exit "\${FAKE_DOCKER_STATUS:-0}"
`);
  await writeFile(join(bin, "git"), `#!/usr/bin/env bash
printf '%s\\n' "\${FAKE_GIT_SHA:?}"
`);
  await writeFile(join(bin, "python3"), `#!/usr/bin/env bash
printf '%s\\n' "\${FAKE_COMMAND_TOKEN:?}"
`);
  await Promise.all([
    chmod(join(bin, "docker"), 0o755),
    chmod(join(bin, "git"), 0o755),
    chmod(join(bin, "python3"), 0o755),
  ]);
  t.after(async () => rm(directory, { recursive: true, force: true }));
  const result = spawnSync("bash", [busyboxPhaseScript, lane], {
    cwd: repoRoot,
    encoding: "utf8",
    env: {
      ...process.env,
      PATH: `${bin}:${process.env.PATH}`,
      GITHUB_WORKSPACE: repoRoot,
      RUNNER_TEMP: directory,
      FAKE_GIT_SHA: sourceCommit,
      FAKE_COMMAND_TOKEN: phaseCommandToken,
      FAKE_DOCKER_ARGS_FILE: argsLog,
      FAKE_PHASE: phase,
      FAKE_PHASE_MODE: mode,
      FAKE_DOCKER_STATUS: String(status),
      FAKE_OUTPUT_MODE: outputMode,
    },
  });
  return { result, argsLog };
}

function readBusyboxPhaseOutput(output) {
  const startMarker = `::stop-commands::${phaseCommandToken}\n`;
  const closeMarker = `\n::${phaseCommandToken}::\n`;
  assert.ok(output.startsWith(startMarker));
  const closeIndex = output.lastIndexOf(closeMarker);
  assert.notEqual(closeIndex, -1);
  return {
    rawOutput: output.slice(startMarker.length, closeIndex),
    annotation: output.slice(closeIndex + closeMarker.length),
  };
}

test("BusyBox phase runner reports only bounded evidence for both lanes", async (t) => {
  const cases = [
    ["browser-wrapper", "package-setup", "valid", 2, "package-setup", 2],
    ["browser-wrapper", "version-guard", "valid", 1, "version-guard", 1],
    ["browser-wrapper", "wrapper-tests", "valid", 1, "wrapper-tests", 1],
    ["compose-path", "version-guard", "valid", 1, "version-guard", 1],
    ["compose-path", "package-setup", "valid", 2, "package-setup", 2],
    ["compose-path", "path-preflight", "valid", 1, "path-preflight", 1],
    ["compose-path", "", "missing", 125, "container-launch", 125],
  ];

  for (const [lane, phase, mode, childStatus, expectedPhase, expectedStatus] of cases) {
    const { result, argsLog } = await invokeBusyboxPhase(t, lane, { phase, mode, status: childStatus });
    assert.equal(result.status, expectedStatus, result.stderr);
    const { rawOutput, annotation } = readBusyboxPhaseOutput(result.stdout);
    assert.match(rawOutput, /::error::forged phase canary literal-ci-password-sentinel literal-admin-token-sentinel/u);
    assert.match(rawOutput, /::stop-commands::attacker\n::attacker::/u);
    assert.equal(
      annotation,
      `::error::${lane} failed (category: phase-failure; phase: ${expectedPhase}; exit: ${expectedStatus}; checked-out commit: ${sourceCommit}; details redacted)\n`,
    );
    assert.doesNotMatch(annotation, /canary|password|token-sentinel|forged/u);
    assert.equal(result.stderr, "");
    const args = await readFile(argsLog, "utf8");
    assert.match(args, /node:24-alpine3\.22@sha256:191c9f0080fcbbc6547a85dc0ff7988072214a355aabdc1d2ec55a7dae5eea8a/u);
    assert.ok(args.includes(`${repoRoot}:/workspace:ro`));
    assert.ok(args.includes(":/phase:rw"));
    assert.ok(args.includes("printf '%s' version-guard > /phase/current"));
    assert.ok(args.includes("printf '%s' package-setup > /phase/current"));
    if (lane === "browser-wrapper") {
      assert.ok(args.includes("printf '%s' wrapper-tests > /phase/current"));
      assert.ok(args.includes("node --test deploy/browser-acceptance/run-wrapper.test.mjs"));
    } else {
      assert.ok(args.includes("printf '%s' path-preflight > /phase/current"));
      assert.ok(args.includes("bash deploy/teagramd/rollout-runner/test-compose-path-preflight.sh"));
    }
  }
});

test("BusyBox phase runner resumes commands after unterminated child output in both lanes", async (t) => {
  const fixtures = [
    ["plain-unterminated", "plain child output without a trailing newline"],
    [
      "forged-command-unterminated",
      "::error::forged after stop-command text literal-ci-password-sentinel literal-admin-token-sentinel",
    ],
  ];

  for (const [lane, phase] of [["browser-wrapper", "wrapper-tests"], ["compose-path", "path-preflight"]]) {
    for (const [outputMode, expectedRawOutput] of fixtures) {
      const { result } = await invokeBusyboxPhase(t, lane, { phase, status: 37, outputMode });
      assert.equal(result.status, 37, result.stderr);
      const { rawOutput, annotation } = readBusyboxPhaseOutput(result.stdout);
      assert.equal(rawOutput, expectedRawOutput);
      assert.equal(
        annotation,
        `::error::${lane} failed (category: phase-failure; phase: ${phase}; exit: 37; checked-out commit: ${sourceCommit}; details redacted)\n`,
      );
      assert.doesNotMatch(annotation, /canary|password|token-sentinel|forged/u);
      assert.equal(result.stderr, "");
    }
  }
});

test("a passing BusyBox lane emits no failure annotation", async (t) => {
  const { result } = await invokeBusyboxPhase(t, "browser-wrapper", {
    phase: "wrapper-tests",
    status: 0,
  });
  assert.equal(result.status, 0, result.stderr);
  const { rawOutput, annotation } = readBusyboxPhaseOutput(result.stdout);
  assert.match(rawOutput, /::error::forged phase/u);
  assert.equal(annotation, "");
  assert.equal(result.stderr, "");
});

test("BusyBox phase runner rejects malformed evidence and keeps the child status", async (t) => {
  for (const mode of ["unknown", "duplicate", "trailing", "oversized", "symlink", "non-regular", "extra-file"]) {
    const { result } = await invokeBusyboxPhase(t, "browser-wrapper", {
      phase: "package-setup",
      mode,
      status: 2,
    });
    assert.equal(result.status, 2, `${mode}: ${result.stderr}`);
    const { annotation } = readBusyboxPhaseOutput(result.stdout);
    assert.equal(
      annotation,
      `::error::browser-wrapper failed (category: phase-failure; phase: unavailable; exit: 2; checked-out commit: ${sourceCommit}; details redacted)\n`,
    );
    assert.equal(result.stderr, "");
  }
});

test("manifest hostname disagreement is rejected before any Docker command", async (t) => {
  const paths = await setup(t);
  const invalidManifest = join(paths.dockerRoot, "invalid.json");
  await writeFile(invalidManifest, JSON.stringify({
    sourceCommit,
    archiveSha256: digest,
    contentDigest: digest,
    url: `https://attacker.example/client/?v=${sourceCommit}`,
  }));
  const result = invoke(paths, ["readiness", "--manifest", invalidManifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-invalid"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("multiple release records are rejected before any Docker command", async (t) => {
  const paths = await setup(t);
  const record = await readFile(paths.manifest, "utf8");
  await writeFile(paths.manifest, `${record}${String.fromCharCode(10)}${record}`);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-invalid"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("missing, symlink, and mount-unsafe manifest paths are rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  const link = join(paths.directory, "release-link.json");
  await symlink(paths.manifest, link);
  const unsafe = join(paths.directory, "release:record.json");
  await writeFile(unsafe, await readFile(paths.manifest));

  for (const manifest of [join(paths.directory, "missing.json"), link, unsafe]) {
    const result = invoke(paths, ["readiness", "--manifest", manifest]);
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '{"status":"error","code":"manifest-invalid"}\n');
    assert.equal(result.stderr, "");
  }
  await assert.rejects(readFile(paths.log));
});

test("relative input names with leading dashes, spaces, and glob characters resolve literally", async (t) => {
  const paths = await setup(t);
  const manifestName = "-release [literal]*.json";
  const scriptName = "-approved [literal]*.mjs";
  const manifest = join(paths.directory, manifestName);
  const script = join(paths.directory, scriptName);
  await writeFile(manifest, await readFile(paths.manifest));
  await writeFile(script, "approved fixture\n");

  const result = invoke(paths, ["acceptance", "--manifest", manifestName, "--script", scriptName], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
  }, undefined, paths.directory);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(probeResultValue, acceptanceResultValue)}\n`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.ok(calls.includes(`${manifest}:/run/release-record.json:ro`));
  assert.ok(calls.includes(`${script}:/opt/browser-acceptance/approved-qa.mjs:ro`));
});

test("served manifest digest or endpoint disagreement is rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  await writeFile(paths.servedManifest, JSON.stringify({
    mode: "private",
    endpoint: "wss://attacker.example/apiws",
    fingerprint: "0123456789abcdef",
    sourceCommit,
    artifactDigest: digest,
  }));
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-mismatch"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("a lookalike WSS hostname is rejected before any Docker command", async (t) => {
  const paths = await setup(t);
  await writeFile(paths.servedManifest, JSON.stringify({
    mode: "private",
    endpoint: "wss://teagramXtailaa4918YtsZnet/apiws",
    fingerprint: "0123456789abcdef",
    sourceCommit,
    artifactDigest: digest,
  }));

  const result = invoke(paths, ["readiness", "--manifest", paths.manifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-mismatch"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("oversized served manifest with a declared length is rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_CURL_MODE: "oversized-content-length",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-mismatch"}\n');
  assert.equal(result.stderr, "");
  assert.match(await readFile(paths.curlLog, "utf8"), /--max-filesize 16385/u);
  await assert.rejects(readFile(paths.log));
});

test("oversized chunked served manifest is capped and rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_CURL_MODE: "oversized-chunked",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-mismatch"}\n');
  assert.equal(result.stderr, "");
  assert.match(await readFile(paths.curlLog, "utf8"), /--max-filesize 16385/u);
  await assert.rejects(readFile(paths.log));
});

test("a served manifest at the 16 KiB limit remains accepted", async (t) => {
  const paths = await setup(t);
  const served = await readFile(paths.servedManifest, "utf8");
  await writeFile(paths.servedManifest, `${served}${" ".repeat(16384 - Buffer.byteLength(served))}`);

  const result = invoke(paths, ["readiness", "--manifest", paths.manifest]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${readyResult}\n`);
  assert.match(await readFile(paths.curlLog, "utf8"), /--max-filesize 16385/u);
});

test("a QA script with an unapproved digest is rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "unapproved-qa.mjs");
  await writeFile(script, "console.log('not approved');\n");

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"qa-script-unapproved"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("a QA script final-component symlink is rejected before Docker startup", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved-qa.mjs");
  const link = join(paths.directory, "qa-link.mjs");
  await writeFile(script, "approved fixture\n");
  await symlink(script, link);

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", link]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"qa-script-unapproved"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
});

test("a QA script cannot be passed to readiness", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "qa.mjs");
  await writeFile(script, "console.log('fixture');\n");

  const result = invoke(paths, ["readiness", "--manifest", paths.manifest, "--script", script]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"arguments-invalid"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.log));
  await assert.rejects(readFile(paths.curlLog));
});

test("acceptance keeps stdin attached to the approved QA runner", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "console.log('approved fixture');\n");

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_REQUIRE_CLOSED_STDIN: "1",
  }, `credential-sentinel${String.fromCharCode(10)}`);
  assert.equal(result.status, 0, `${result.stdout}${result.stderr}\n${await readFile(paths.log, "utf8")}`);
  assert.equal(result.stdout, `${evidenceLine(probeResultValue, acceptanceResultValue)}${String.fromCharCode(10)}`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /exec --interactive browser node \/opt\/browser-acceptance\/approved-qa\.mjs --mode acceptance/u);
  assert.match(calls, /compose .* down --rmi local/u);
  assert.doesNotMatch(calls, /credential-sentinel/u);
});

test("browser wrapper removes rollout runner test evidence overrides", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "console.log('approved fixture');\n");

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    ROLLOUT_RUNNER_TEST_MODE: "1",
    ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT: paths.directory,
  });
  assert.equal(result.status, 0, `${result.stdout}${result.stderr}`);
  assert.equal(result.stdout, `${evidenceLine(probeResultValue, acceptanceResultValue)}\n`);
  assert.equal(result.stderr, "");
  assert.match(await readFile(paths.log, "utf8"), /host-probe/u);
});

test("host cleanup proof overrides a forged browser success", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = { ...probeResultValue, ok: false, code: "cleanup-unproven", cleanupProven: false };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, `${evidenceLine(probe, acceptanceResultValue)}\n`);
  assert.equal(result.stderr, "");
});

test("host preconditions fail before any browser stdin consumer is launched", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = {
    ...revisionUnprovenProbe(),
    code: "environment-unproven",
    serverSourceCommit: "f".repeat(40),
    registrationMode: "unknown",
    loginCodesDisabled: true,
    revisionMatched: true,
  };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_PROBE_STATUS: "1", WRAPPER_PROBE_SKIP_BROWSER: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
  }, "credential-sentinel\n");
  assert.equal(result.status, 1);
  assert.equal(result.stdout, `${evidenceLine(probe, null, { qaResult: "absent" })}\n`);
  assert.doesNotMatch(await readFile(paths.log, "utf8"), /exec --interactive|credential-sentinel/u);
});

test("binding-not-created and QA failure produce the exact flat evidence line", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = bindingNotCreatedProbe();
  const qa = {
    ...qaV3ResultValue,
    ok: false,
    phase: "password-check",
    outcome: "credential-rejected",
    logout: "not-attempted",
    wssConnections: 1,
    qaFailureCode: "authentication-failed-once",
    submission: "password",
    chatList: "not-reached",
    chatListRows: 0,
    wssEndpointMatched: false,
  };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
    WRAPPER_ACCEPTANCE_STATUS: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: JSON.stringify(qa),
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, `${evidenceLine(probe, qa)}\n`);
  assert.equal(result.stderr, "");
});

test("signup-attempted QA results can never produce accepted evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const failedQa = acceptanceFailureOutput({qaFailureCode: "signup-attempted"});
  const failureResult = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: failedQa,
    WRAPPER_ACCEPTANCE_STATUS: "1",
  });
  const failureEvidence = JSON.parse(failureResult.stdout);

  assert.equal(failureResult.status, 1);
  assert.equal(failureEvidence.status, "error");
  assert.equal(failureEvidence.qaResult, "valid");
  assert.equal(failureEvidence.qaFailureCode, "signup-attempted");

  const forgedSuccess = acceptanceOutput({qaFailureCode: "signup-attempted"});
  const successResult = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: forgedSuccess,
  });
  const successEvidence = JSON.parse(successResult.stdout);

  assert.equal(successEvidence.status, "error");
  assert.equal(successEvidence.qaResult, "invalid");
  assert.equal(successEvidence.qaFailureCode, "unknown");
});

test("malformed and absent QA output preserve validated probe evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = {
    ...probeV5ResultValue,
    ok: false,
    code: "cleanup-unproven",
    cleanupProven: false,
    baselineIntact: false,
  };
  const malformed = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
    WRAPPER_ACCEPTANCE_STATUS: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: "{ malformed QA output",
  });
  assert.equal(malformed.status, 1);
  assert.equal(malformed.stdout, `${evidenceLine(probe, null, { qaResult: "invalid" })}\n`);
  assert.equal(malformed.stderr, "");

  const missingProbe = revisionUnprovenProbe();
  const absent = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_SKIP_BROWSER: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(missingProbe),
  });
  assert.equal(absent.status, 1);
  assert.equal(absent.stdout, `${evidenceLine(missingProbe, null, { qaResult: "absent" })}\n`);
  assert.equal(absent.stderr, "");
});

test("a valid probe with a QA v2 line emits invalid QA defaults", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = probeV5ResultValue;
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
    WRAPPER_ACCEPTANCE_OUTPUT: acceptanceV2Result,
  });
  assert.equal(result.status, 0);
  assert.equal(result.stdout, `${evidenceLine(probe, null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("QA invalidity changes accepted to error without changing a successful probe exit", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = probeV5ResultValue;
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
    WRAPPER_ACCEPTANCE_OUTPUT: "not a QA object",
  });
  assert.equal(result.status, 0);
  assert.equal(result.stdout, `${evidenceLine(probe, null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("compose teardown failure preserves the probe code and evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = bindingNotCreatedProbe();
  const qa = {
    ...qaV3ResultValue,
    ok: false,
    phase: "password-check",
    outcome: "credential-rejected",
    logout: "not-attempted",
    qaFailureCode: "authentication-failed-once",
    chatList: "not-reached",
    chatListRows: 0,
    wssEndpointMatched: false,
  };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
    WRAPPER_ACCEPTANCE_STATUS: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: JSON.stringify(qa),
    WRAPPER_CLEANUP_FAILURE: "1",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, `${evidenceLine(probe, qa, { teardown: "failed" })}\n`);
  assert.equal(result.stderr, "");
});

test("account-created is a non-zero error in wrapper evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = {
    ...probeV5ResultValue,
    ok: false,
    code: "account-created",
    accountsUnchanged: false,
  };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, `${evidenceLine(probe, acceptanceResultValue)}\n`);
  assert.equal(JSON.parse(result.stdout).status, "error");
  assert.equal(JSON.parse(result.stdout).code, "account-created");
});

test("the wrapper cannot accept a probe without unchanged account evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = { ...probeV5ResultValue, accountsUnchanged: false };
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(probe),
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
  assert.equal(result.stdout.includes('"status":"accepted"'), false);
});

test("probe schema rejects unknown keys, duplicate keys, unsafe enums, and unclamped counts", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  const canary = "private-probe-output-canary";
  for (const output of [
    JSON.stringify({ ...probeResultValue, detail: canary }),
    JSON.stringify({ ...probeResultValue, code: canary }),
    JSON.stringify({ ...probeResultValue, boundBefore: 101 }),
    JSON.stringify({ ...probeResultValue, cleanupProven: false }),
    JSON.stringify({ ...probeResultValue, bindingEvidence: "unknown" }),
    JSON.stringify({ ...probeResultValue, captureAllowedConnects: 100 }),
    JSON.stringify({ ...probeResultValue, bindingEvidence: "not-reached", bindingAdditions: 1 }),
    JSON.stringify({ ...probeResultValue, captureWindow: "not-reached", captureBlocked: 1 }),
    `{"code":"${canary}",${JSON.stringify(probeResultValue).slice(1)}`,
    `${JSON.stringify(probeResultValue)}\n${JSON.stringify(probeResultValue)}`,
    JSON.stringify(Object.fromEntries(Object.entries(probeResultValue).filter(([key]) => key !== "serverSourceCommit"))),
    JSON.stringify(probeV3ResultValue),
    JSON.stringify(legacyRegistrationProbeValue),
    JSON.stringify({ ...probeResultValue, schemaVersion: 1 }),
    JSON.stringify({ ...probeResultValue, serverSourceCommit: "" }),
    JSON.stringify({ ...probeResultValue, serverSourceCommit: "not-a-commit" }),
  ]) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_PROBE_OUTPUT: output,
    });
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
    assert.equal(result.stderr, "");
    assert.doesNotMatch(result.stdout, /private-probe-output-canary/u);
  }
});

test("a probe code array is rejected as an invalid enum", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify({ ...probeResultValue, ok: false, code: ["ok"] }),
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
  assert.equal(result.stderr, "");
});

test("acceptance rejects a non-string database failure category", async (t) => {
  for (const category of [[], ["config"]]) {
    const paths = await setup(t);
    const script = join(paths.directory, "approved.mjs");
    await writeFile(script, "approved fixture\n");
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_PROBE_STATUS: "1",
      WRAPPER_PROBE_OUTPUT: JSON.stringify({ ...probeResultValue, ok: false, code: "database-unavailable",
        databaseFailureCategory: category }),
    });
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n', JSON.stringify(category));
    assert.equal(result.stderr, "");
  }
});

test("browser NUL bytes fail closed without Bash stripping or warning", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1", WRAPPER_ACCEPTANCE_NUL: "1",
  });
  assert.equal(result.status, 0);
  assert.equal(result.stdout, `${evidenceLine(probeResultValue, null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("acceptance preserves a validated failed stage with a non-zero exit", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const failedResult = acceptanceFailureOutput();

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: failedResult,
    WRAPPER_ACCEPTANCE_STATUS: "1",
  });

  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), JSON.parse(failedResult))}\n`);
  assert.equal(result.stderr, "");
});

test("acceptance allows every WSS connection count up to the client's ceiling on success", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  for (let count = 1; count <= WSS_CONNECTION_CEILING; count += 1) {
    const resultLine = acceptanceOutput({ wssConnections: count });
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: resultLine,
    });

    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, `${evidenceLine(probeResultValue, JSON.parse(resultLine))}\n`);
    assert.equal(result.stderr, "");
  }
});

test("acceptance rejects duplicate fields without emitting their canary values", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const canary = "PRIVATE_ACCEPTANCE_CANARY";
  const failedResult = acceptanceFailureOutput();
  const cases = [
    [`"outcome":"${canary}"`, acceptanceResult, "0"],
    [`"out\\u0063ome":"${canary}"`, acceptanceResult, "0"],
    [`"outcome":{"detail":"${canary}"}`, acceptanceResult, "0"],
    [`"phase":"${canary}"`, failedResult, "1"],
  ];

  for (const [extraField, line, status] of cases) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: `{${extraField},${line.slice(1)}`,
      WRAPPER_ACCEPTANCE_STATUS: status,
    });

    assert.equal(result.status, Number(status), result.stderr);
    assert.equal(result.stdout, `${evidenceLine(
      status === "0" ? probeResultValue : browserFailedProbeValue(), null, { qaResult: "invalid" },
    )}\n`);
    assert.equal(result.stderr, "");
    assert.equal(`${result.stdout}${result.stderr}`.includes(canary), false);
  }
});

test("acceptance serializes validated fields for both exit outcomes", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const failedResult = acceptanceFailureOutput();

  for (const [line, status] of [[acceptanceResult, "0"], [failedResult, "1"]]) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: ` ${line.replace('"schemaVersion":3', '"schemaVersion":3e0')} `,
      WRAPPER_ACCEPTANCE_STATUS: status,
    });

    assert.equal(result.status, Number(status), result.stderr);
    assert.equal(result.stdout, `${evidenceLine(
      status === "0" ? probeResultValue : browserFailedProbeValue(), JSON.parse(line),
    )}\n`);
    assert.equal(result.stderr, "");
  }
});

test("legacy QA failures retain validated host evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const errorLine = JSON.stringify({ schemaVersion: 1, ok: false, failure: "browser-unavailable" });

  for (const output of [errorLine, `${errorLine}\n`]) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: output,
      WRAPPER_ACCEPTANCE_STATUS: "1",
    });

    assert.equal(result.status, 1, result.stderr);
    assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), null, { qaResult: "invalid" })}\n`);
    assert.equal(result.stderr, "");
  }
});

test("legacy duplicate QA fields are invalid while host evidence remains", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: '{"schemaVersion":1,"ok":false,"failure":"PRIVATE_ACCEPTANCE_CANARY","failure":"browser-unavailable"}',
    WRAPPER_ACCEPTANCE_STATUS: "1",
  });

  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("legacy QA with extra stdout lines is invalid while host evidence remains", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const errorLine = JSON.stringify({ schemaVersion: 1, ok: false, failure: "browser-unavailable" });

  for (const output of [`${errorLine}\n{}`, `${errorLine}\n{}\n`]) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: output,
      WRAPPER_ACCEPTANCE_STATUS: "1",
    });

    assert.equal(result.status, 1, result.stderr);
    assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), null, { qaResult: "invalid" })}\n`);
    assert.equal(result.stderr, "");
  }
});

test("legacy QA with multiple objects is invalid while host evidence remains", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: '{"schemaVersion":1,"ok":false,"failure":"browser-unavailable"} {}',
    WRAPPER_ACCEPTANCE_STATUS: "1",
  });

  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("multiline QA is invalid while the wrapper preserves host evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: JSON.stringify(acceptanceResultValue, null, 2),
  });

  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(probeResultValue, null, { qaResult: "invalid" })}\n`);
  assert.equal(result.stderr, "");
});

test("acceptance preserves a failed result that counted a WSS connection above the ceiling", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const failedResult = acceptanceFailureOutput({ wssConnections: WSS_CONNECTION_CEILING + 1 });

  const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1",
    WRAPPER_ACCEPTANCE_OUTPUT: failedResult,
    WRAPPER_ACCEPTANCE_STATUS: "1",
  });

  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, `${evidenceLine(browserFailedProbeValue(), JSON.parse(failedResult))}\n`);
  assert.equal(result.stderr, "");
});

test("the wrapper, QA runner, and progress helper share one WSS connection ceiling", async () => {
  const wrapper = await readFile(runScript, "utf8");
  const qaRunner = await import("./private-browser-acceptance-v2.mjs");
  const progressHelper = await import("./acceptance-result.mjs");

  assert.deepEqual(wrapper.match(/^readonly WSS_CONNECTION_CEILING=(\d+)$/mu)?.slice(1), [`${WSS_CONNECTION_CEILING}`]);
  assert.equal(qaRunner.WSS_CONNECTION_CEILING, WSS_CONNECTION_CEILING);
  assert.equal(progressHelper.WSS_CONNECTION_CEILING, WSS_CONNECTION_CEILING);
});

test("invalid QA schemas use fixed defaults and preserve host evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const invalidOutputs = [
    acceptanceOutput({ unreviewed: "must-not-pass" }),
    acceptanceOutput({ phase: "password-check with private detail" }),
    acceptanceOutput({ outcome: "private detail" }),
    acceptanceOutput({ logout: "private detail" }),
    `${acceptanceResult}\n{}`,
    acceptanceOutput({ wssConnections: WSS_CONNECTION_CEILING + 2 }),
    '{"schemaVersion":1,"ok":true}',
    acceptanceOutput({ sourceCommit: "d".repeat(40) }),
    acceptanceOutput({ targetHost: "attacker.example" }),
    acceptanceOutput({ wssConnections: "1" }),
    "",
  ];

  for (const output of invalidOutputs) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: output,
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(result.stdout, `${evidenceLine(probeResultValue, null, {
      qaResult: output === "" ? "absent" : "invalid",
    })}\n`);
    assert.equal(result.stderr, "");
  }
});

test("inconsistent QA results cannot change the probe code or exit status", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const invalidResults = [
    [acceptanceOutput({ phase: "password-check" }), "0"],
    [acceptanceOutput({ phase: "password-check", ok: false, outcome: "timeout", logout: "not-attempted" }), "0"],
    [acceptanceOutput({ outcome: "timeout" }), "0"],
    [acceptanceOutput({ logout: "timeout" }), "0"],
    [acceptanceOutput({ wssConnections: 0 }), "0"],
    [acceptanceOutput({ wssConnections: WSS_CONNECTION_CEILING + 1 }), "0"],
    [acceptanceOutput({ ok: false }), "0"],
    [acceptanceOutput(), "1"],
    [acceptanceOutput({ ok: false, phase: "wss-not-opened", outcome: "timeout", logout: "not-attempted", wssConnections: 1 }), "1"],
    [acceptanceOutput({ ok: false, phase: "req-pq", outcome: "timeout", logout: "not-attempted", wssConnections: 0 }), "1"],
    [acceptanceOutput({ ok: false, phase: "password-check", outcome: "ok", logout: "not-attempted", wssConnections: 1 }), "1"],
    [acceptanceOutput({ ok: false, phase: "password-check", outcome: "timeout", logout: "ok", wssConnections: 1 }), "1"],
    [acceptanceOutput({ ok: false, phase: "signed-in", outcome: "ok", logout: "not-attempted", wssConnections: 1 }), "1"],
  ];

  for (const [output, status] of invalidResults) {
    const result = invoke(paths, ["acceptance", "--manifest", paths.manifest, "--script", script], {
      WRAPPER_ACCEPT_QA_FIXTURE: "1",
      WRAPPER_ACCEPTANCE_OUTPUT: output,
      WRAPPER_ACCEPTANCE_STATUS: status,
    });
    assert.equal(result.status, Number(status), result.stderr);
    const probe = status === "0" ? probeResultValue : browserFailedProbeValue();
    const qa = output === acceptanceResult ? acceptanceResultValue : null;
    assert.equal(result.stdout, `${evidenceLine(probe, qa, { qaResult: qa ? "valid" : "invalid" })}\n`);
    assert.equal(result.stderr, "");
  }
});

test("curl stops an oversized chunked transfer at the streaming byte cap", async (t) => {
  const server = createServer((_request, response) => {
    response.writeHead(200, { "Transfer-Encoding": "chunked" });
    let sent = 0;
    const writeChunk = () => {
      if (response.destroyed) return;
      response.write(Buffer.alloc(1024, 120));
      sent += 1024;
      if (sent >= 65536) {
        response.end();
      } else {
        setImmediate(writeChunk);
      }
    };
    writeChunk();
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  t.after(() => new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve())));

  const child = spawn("curl", [
    "-q", "--silent", "--max-time", "5", "--max-filesize", "16385",
    `http://127.0.0.1:${server.address().port}/`,
  ]);
  let received = 0;
  child.stdout.on("data", (chunk) => { received += chunk.length; });
  const exitCode = await new Promise((resolve, reject) => {
    child.on("error", reject);
    child.on("close", resolve);
  });
  assert.equal(exitCode, 63);
  assert.equal(received, 16385);
});

test("curl older than 8.4 is rejected before manifest download", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_CURL_VERSION: "8.3.0",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-fetch-failed"}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(paths.curlLog));
  await assert.rejects(readFile(paths.log));
});

test("browser acceptance serializes different checkouts and sessions through failed teardown", async (t) => {
  const paths = await setup(t);
  const alternateRoot = join(paths.directory, "alternate-checkout");
  const alternateRunScript = join(alternateRoot, "deploy/browser-acceptance/run.sh");
  const alternateRuntime = join(paths.directory, "alternate-runtime");
  const buildStarted = join(paths.directory, "build-started");
  const releaseBuild = join(paths.directory, "release-build");
  const cleanupStarted = join(paths.directory, "cleanup-started");
  const releaseCleanup = join(paths.directory, "release-cleanup");
  await mkdir(dirname(alternateRunScript), { recursive: true });
  await mkdir(alternateRuntime, { mode: 0o700 });
  await copyFile(paths.runScript, alternateRunScript);

  const first = spawnScript(paths, paths.runScript, ["blocked-control", "--manifest", paths.manifest], {
    WRAPPER_BUILD_STARTED_FILE: buildStarted,
    WRAPPER_RELEASE_BUILD_FILE: releaseBuild,
    WRAPPER_CLEANUP_STARTED_FILE: cleanupStarted,
    WRAPPER_RELEASE_CLEANUP_FILE: releaseCleanup,
    WRAPPER_RUNTIME_FAILURE: "1",
  });
  try {
    await waitForFile(buildStarted);
    const beforeBuildContention = await readFile(paths.log, "utf8");
    const buildContention = invokeScript(paths, alternateRunScript,
      ["blocked-control", "--manifest", paths.manifest],
      { XDG_RUNTIME_DIR: alternateRuntime, HOME: alternateRoot }, undefined, alternateRoot);
    assert.equal(buildContention.status, 1);
    assert.equal(buildContention.stdout, '{"status":"error","code":"runtime-busy"}\n');
    assert.equal(buildContention.stderr, "");
    assert.equal(await readFile(paths.log, "utf8"), beforeBuildContention);
    assert.equal((await stat(paths.sharedRuntimeLock)).mode & 0o777, 0o666);

    await writeFile(releaseBuild, "continue\n");
    await waitForFile(cleanupStarted);
    const beforeCleanupContention = await readFile(paths.log, "utf8");
    const cleanupContention = invokeScript(paths, alternateRunScript,
      ["blocked-control", "--manifest", paths.manifest],
      { XDG_RUNTIME_DIR: alternateRuntime, HOME: alternateRoot }, undefined, alternateRoot);
    assert.equal(cleanupContention.status, 1);
    assert.equal(cleanupContention.stdout, '{"status":"error","code":"runtime-busy"}\n');
    assert.equal(cleanupContention.stderr, "");
    assert.equal(await readFile(paths.log, "utf8"), beforeCleanupContention);

    await writeFile(releaseCleanup, "continue\n");
    const completion = await first.result;
    assert.equal(completion.status, 1, completion.stderr);
    assert.equal(completion.stdout, '{"status":"error","code":"observer-unhealthy"}\n');
  } finally {
    await writeFile(releaseBuild, "continue\n");
    await writeFile(releaseCleanup, "continue\n");
    if (first.child.exitCode === null && first.child.signalCode === null) first.child.kill("SIGKILL");
    await first.result;
  }
});

test("a killed lock holder leaves a reusable shared lock file", async (t) => {
  const paths = await setup(t);
  const alternateRuntime = join(paths.directory, "alternate-runtime");
  await mkdir(alternateRuntime, { mode: 0o700 });
  await ensureSharedLockFile(paths.sharedRuntimeLock);

  const holder = spawn("bash", ["-c", 'exec 9>>"$1"; flock -n 9 || exit; printf locked; IFS= read -r _',
    "lock-holder", paths.sharedRuntimeLock], {
    stdio: ["pipe", "pipe", "ignore"],
  });
  let holderOutput = "";
  holder.stdout.setEncoding("utf8").on("data", (chunk) => { holderOutput += chunk; });
  const ready = new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("lock holder did not acquire the fixture lock")), 5000);
    holder.stdout.on("data", () => {
      if (holderOutput.includes("locked")) {
        clearTimeout(timeout);
        resolve();
      }
    });
    holder.on("error", reject);
  });
  await ready;
  const closed = new Promise((resolve, reject) => {
    holder.on("error", reject);
    holder.on("close", (status, signal) => resolve({ status, signal }));
  });
  holder.kill("SIGKILL");
  const killed = await closed;
  assert.equal(killed.signal, "SIGKILL");

  const result = invoke(paths, ["blocked-control", "--manifest", paths.manifest], {
    XDG_RUNTIME_DIR: alternateRuntime,
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${blockedResult}\n`);
});

test("browser lock opens an existing sticky /tmp inode read-only", async (t) => {
  const paths = await setup(t);
  const lockFile = join("/tmp", `teagram-browser-acceptance-${process.pid}-${randomUUID()}.lock`);
  t.after(() => rm(lockFile, { force: true }));
  const tempDirectory = await stat("/tmp");
  assert.equal(tempDirectory.uid, 0);
  assert.equal(tempDirectory.mode & 0o1000, 0o1000);

  await setFixtureLockPath(paths, lockFile);
  await ensureSharedLockFile(lockFile);
  await installLockFdProbe(paths);

  const missingManifest = join(paths.directory, "missing-manifest.json");
  const result = invoke(paths, ["blocked-control", "--manifest", missingManifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"manifest-invalid"}\n');
  assertLockFdIsReadOnly(result.stderr);
});

test("browser acceptance reports contention and reuses a sticky /tmp lock across UIDs with protected_regular", async (t) => {
  if (process.platform !== "linux") {
    t.skip("cross-UID fixture requires Linux fs.protected_regular");
    return;
  }
  let protectedRegular;
  try {
    protectedRegular = Number.parseInt((await readFile("/proc/sys/fs/protected_regular", "utf8")).trim(), 10);
  } catch {
    const message = "cross-UID fixture requires readable /proc/sys/fs/protected_regular";
    if (process.env.CI) assert.fail(message);
    t.skip(message);
    return;
  }
  if (!Number.isInteger(protectedRegular) || protectedRegular < 1) {
    const message = "cross-UID fixture requires fs.protected_regular >= 1";
    if (process.env.CI) assert.fail(message);
    t.skip(message);
    return;
  }

  const paths = await setup(t);
  const lockFile = join("/tmp", `teagram-browser-acceptance-${process.pid}-${randomUUID()}.lock`);
  const fixtureRoot = join("/tmp", `teagram-browser-acceptance-cross-uid-${process.pid}-${randomUUID()}`);
  const fixtureRunScript = join(fixtureRoot, "deploy/browser-acceptance/run.sh");
  const fixtureBin = join(fixtureRoot, "bin");
  t.after(() => rm(lockFile, { force: true }));
  t.after(() => rm(fixtureRoot, { recursive: true, force: true }));
  const tempDirectory = await stat("/tmp");
  assert.equal(tempDirectory.uid, 0);
  assert.equal(tempDirectory.mode & 0o1000, 0o1000);

  await mkdir(dirname(fixtureRunScript), { recursive: true, mode: 0o755 });
  await mkdir(fixtureBin, { mode: 0o755 });
  await chmod(fixtureRoot, 0o755);
  const wrapperSource = await readFile(paths.runScript, "utf8");
  assert.equal(wrapperSource.split(paths.sharedRuntimeLock).length - 1, 1);
  await writeFile(fixtureRunScript, wrapperSource.replace(paths.sharedRuntimeLock, lockFile), { mode: 0o755 });
  await installLockFdProbe({ bin: fixtureBin });

  const currentUid = process.getuid();
  const currentGid = process.getgid();
  const ownerUid = currentUid === 0 ? 65533 : currentUid;
  const ownerGid = currentUid === 0 ? 65533 : currentGid;
  const callerUid = ownerUid === 65534 ? 65533 : 65534;
  const callerGid = currentUid === 0 ? callerUid : 65534;
  for (const [uid, gid] of [[ownerUid, ownerGid], [callerUid, callerGid]]) {
    const available = spawnSyncAsUser(uid, gid, "id", ["-u"], {
      encoding: "utf8",
      env: { PATH: "/usr/bin:/bin" },
    });
    if (available.status !== 0 || available.stdout.trim() !== String(uid)) {
      const message = "cross-UID fixture requires root or passwordless sudo with setpriv";
      if (process.env.CI) assert.fail(message);
      t.skip(message);
      return;
    }
  }

  const ownerEnv = { PATH: "/usr/bin:/bin" };
  const createLock = spawnSyncAsUser(ownerUid, ownerGid, "bash", ["-c",
    'umask 0111; : > "$1"; chmod 666 -- "$1"', "lock-file", lockFile], {
    encoding: "utf8",
    env: ownerEnv,
  });
  assert.equal(createLock.status, 0, createLock.stderr);
  assert.equal((await stat(lockFile)).uid, ownerUid);
  assert.equal((await stat(lockFile)).mode & 0o777, 0o666);

  const holder = spawnAsUser(ownerUid, ownerGid, "bash", ["-c",
    'exec 9<> "$1"; /usr/bin/flock -n 9 || exit; printf locked; IFS= read -r _',
    "lock-holder", lockFile], {
    stdio: ["pipe", "pipe", "ignore"],
    env: ownerEnv,
  });
  let holderOutput = "";
  holder.stdout.setEncoding("utf8").on("data", (chunk) => { holderOutput += chunk; });
  const closed = new Promise((resolve) => holder.once("close", (status, signal) => resolve({ status, signal })));
  try {
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error("cross-UID lock holder did not acquire the fixture lock")), 5000);
      holder.stdout.on("data", () => {
        if (holderOutput.includes("locked")) {
          clearTimeout(timeout);
          resolve();
        }
      });
      holder.on("error", reject);
      holder.once("close", () => reject(new Error("cross-UID lock holder exited before acquiring the fixture lock")));
    });

    const wrapperEnv = { PATH: `${fixtureBin}:/usr/bin:/bin`, HOME: fixtureRoot, XDG_RUNTIME_DIR: fixtureRoot };
    const missingManifest = join(fixtureRoot, "missing-manifest.json");
    const contention = spawnSyncAsUser(callerUid, callerGid, "bash",
      [fixtureRunScript, "blocked-control", "--manifest", missingManifest], {
        cwd: fixtureRoot,
        encoding: "utf8",
        env: wrapperEnv,
      });
    assert.equal(contention.status, 1);
    assert.equal(contention.stdout, '{"status":"error","code":"runtime-busy"}\n');
    assertLockFdIsReadOnly(contention.stderr);

    holder.stdin.end("release\n");
    assert.deepEqual(await closed, { status: 0, signal: null });

    const reuse = spawnSyncAsUser(callerUid, callerGid, "bash",
      [fixtureRunScript, "blocked-control", "--manifest", missingManifest], {
        cwd: fixtureRoot,
        encoding: "utf8",
        env: wrapperEnv,
      });
    assert.equal(reuse.status, 1);
    assert.equal(reuse.stdout, '{"status":"error","code":"manifest-invalid"}\n');
    assertLockFdIsReadOnly(reuse.stderr);
  } finally {
    if (holder.exitCode === null && holder.signalCode === null) {
      const didClose = closed.then(() => true);
      holder.kill("SIGKILL");
      await didClose;
    }
  }
});

test("blocked control emits one fixed line and cleans the project on success", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["blocked-control", "--manifest", paths.manifest]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${blockedResult}\n`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /compose .* build --pull browser/u);
  assert.match(calls, /compose .* up --detach --wait observer/u);
  assert.match(calls, /compose .* run --no-deps --rm -T/u);
  assert.match(calls, /compose .* down --rmi local/u);
  assert.doesNotMatch(calls, /\bprune\b/u);
});

test("failed runtime output becomes fixed JSON and still runs scoped cleanup", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["blocked-control", "--manifest", paths.manifest], { WRAPPER_RUNTIME_FAILURE: "1" });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"observer-unhealthy"}\n');
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /compose .* down --rmi local/u);
});

test("a failed image build also triggers isolated cleanup", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["blocked-control", "--manifest", paths.manifest], { WRAPPER_BUILD_FAILURE: "1" });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"image-build-failed"}\n');
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /compose .* down --rmi local/u);
});

test("readiness closes stdin and emits only the fixed metadata line", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_REQUIRE_CLOSED_STDIN: "1",
  }, `credential-sentinel${String.fromCharCode(10)}`);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${readyResult}${String.fromCharCode(10)}`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /compose .* restart observer/u);
  assert.match(calls, /compose .* down --rmi local/u);
});

function wssError(diagnostic, targets, handshakes, targetMatch, status) {
  return JSON.stringify({
    status: "error",
    code: "websocket-not-ready",
    wss_diagnostic: diagnostic,
    wss_targets: targets,
    wss_handshakes: handshakes,
    wss_target_match: targetMatch,
    wss_status: status,
  });
}

test("readiness wrapper re-emits every valid WSS diagnostic row byte-for-byte", async (t) => {
  const paths = await setup(t);
  const rows = [
    ["observation-malformed", 0, 0, false, 0],
    ["untracked-handshake", 0, 1, false, 0],
    ["worker-unobservable", 1, 0, false, 0],
    ["worker-pending", 1, 1, false, 0],
    ["no-target", 0, 0, false, 0],
    ["socket-2-target-mismatch", 2, 2, false, 101],
    ["socket-1-no-handshake", 1, 0, true, 0],
    ["socket-2-no-handshake", 2, 1, true, 0],
    ["socket-1-handshake-not-101", 1, 1, true, 503],
    ["socket-2-handshake-not-101", 2, 2, true, 503],
    ["socket-1-duplicate-handshake", 1, 2, true, 101],
  ];

  for (const row of rows) {
    const expected = wssError(...row);
    const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
      WRAPPER_READINESS_OUTPUT: expected,
      WRAPPER_READINESS_STATUS: "1",
    });
    assert.equal(result.status, 1, result.stderr);
    assert.equal(result.stdout, `${expected}\n`);
    assert.equal(result.stderr, "");
    assert.doesNotMatch(result.stdout, /canary|ts\.net|\/|header/iu);
  }
});

test("readiness wrapper rejects malformed, inconsistent, and multiline diagnostics", async (t) => {
  const paths = await setup(t);
  const valid = wssError("no-target", 0, 0, false, 0);
  const invalid = [
    valid.replace(/\}$/u, ',"extra":true}'),
    '{"status":"error","code":"websocket-not-ready","wss_targets":0,"wss_diagnostic":"no-target","wss_handshakes":0,"wss_target_match":false,"wss_status":0}',
    valid.replace('"wss_targets":0', ' "wss_targets":0'),
    valid.replace('"wss_status":0', '"wss_status":"0"'),
    valid.replace('"wss_status":0', '"wss_status":600'),
    valid.replace('"wss_status":0', '"wss_status":099'),
    valid.replace('"wss_targets":0', '"wss_targets":3'),
    wssError("observation-malformed", 1, 0, true, 0),
    wssError("no-target", 1, 0, false, 0),
    wssError("socket-1-target-mismatch", 0, 0, false, 0),
    wssError("socket-1-target-mismatch", 1, 1, true, 503),
    wssError("socket-1-target-mismatch", 1, 0, false, 101),
    wssError("socket-1-no-handshake", 0, 0, true, 0),
    wssError("socket-1-no-handshake", 1, 1, true, 0),
    wssError("socket-1-no-handshake", 1, 0, false, 0),
    wssError("socket-1-handshake-not-101", 0, 0, true, 503),
    wssError("socket-1-handshake-not-101", 1, 1, false, 503),
    wssError("socket-1-handshake-not-101", 1, 1, true, 101),
    wssError("socket-1-duplicate-handshake", 1, 1, true, 101),
    wssError("socket-1-duplicate-handshake", 1, 2, true, 0),
    valid.replace('"code":"websocket-not-ready"', '"code":"origin-not-ready"'),
    `${valid}\n${valid}`,
    `${valid}\n`,
    `${valid}\n\n\n`,
  ];

  for (const output of invalid) {
    const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
      WRAPPER_READINESS_OUTPUT: output,
      WRAPPER_READINESS_STATUS: "1",
    });
    assert.equal(result.status, 1, result.stderr);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
    assert.equal(result.stderr, "");
  }

  const successStatus = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_READINESS_OUTPUT: wssError("no-target", 0, 0, false, 0),
    WRAPPER_READINESS_STATUS: "0",
  });
  assert.equal(successStatus.status, 1);
  assert.equal(successStatus.stdout, '{"status":"error","code":"runtime-failed"}\n');
});

test("wrapper does not echo canary URLs, headers, or cookies from malformed runtime output", async (t) => {
  const paths = await setup(t);
  const injected = JSON.stringify({
    status: "error",
    code: "websocket-not-ready",
    wss_diagnostic: "socket-1-handshake-not-101",
    wss_targets: 1,
    wss_handshakes: 1,
    wss_target_match: true,
    wss_status: 503,
    observed_url: "https://example.invalid/wss-diagnostic-canary",
    response_header: "wss-response-header-canary",
    set_cookie: "wss-set-cookie-canary",
  });
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_READINESS_OUTPUT: injected,
    WRAPPER_READINESS_STATUS: "1",
  });
  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
  assert.equal(result.stderr, "");
  assert.doesNotMatch(result.stdout, /wss-diagnostic-canary|wss-response-header-canary|wss-set-cookie-canary|ts\.net|\//u);
});

test("extended WSS diagnostics are rejected outside readiness mode", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["blocked-control", "--manifest", paths.manifest], {
    WRAPPER_BLOCKED_OUTPUT: wssError("no-target", 0, 0, false, 0),
  });
  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n');
  assert.equal(result.stderr, "");
});

test("cleanup failure overrides an otherwise valid WSS diagnostic", async (t) => {
  const paths = await setup(t);
  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    WRAPPER_READINESS_OUTPUT: wssError("no-target", 0, 0, false, 0),
    WRAPPER_READINESS_STATUS: "1",
    WRAPPER_CLEANUP_FAILURE: "1",
  });
  assert.equal(result.status, 1, result.stderr);
  assert.equal(result.stdout, '{"status":"error","code":"cleanup-failed"}\n');
  assert.equal(result.stderr, "");
});

async function preflightScript(paths) {
  const script = join(paths.directory, "approved.mjs");
  await writeFile(script, "approved fixture\n");
  return script;
}

function invokePreflight(paths, script, extraEnv = {}) {
  return invoke(paths, ["acceptance-preflight", "--manifest", paths.manifest, "--script", script], {
    WRAPPER_ACCEPT_QA_FIXTURE: "1", ...extraEnv,
  }, "credential-sentinel\n");
}

test("acceptance preflight closes stdin, never runs an interactive exec, and prints one fixed ready line", async (t) => {
  const paths = await setup(t);
  const result = invokePreflight(paths, await preflightScript(paths));
  assert.equal(result.status, 0, `${result.stdout}${result.stderr}`);
  assert.equal(result.stdout, `${preflightReady}\n`);
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(`${paths.log}.stdin-touched`));
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /host-probe \S+ --mode preflight --manifest/u);
  assert.match(calls, /node \/opt\/browser-acceptance\/approved-qa\.mjs --mode preflight/u);
  assert.doesNotMatch(calls, /--interactive|--mode acceptance|credential-sentinel/u);
  assert.match(calls, /compose .* down --rmi local/u);
});

test("acceptance preflight requires the reviewed QA pin before any Docker command", async (t) => {
  const paths = await setup(t);
  const script = join(paths.directory, "unapproved.mjs");
  await writeFile(script, "console.log('not approved');\n");
  const result = invoke(paths, ["acceptance-preflight", "--manifest", paths.manifest, "--script", script]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"qa-script-unapproved"}\n');
  await assert.rejects(readFile(paths.log));

  const missing = invoke(paths, ["acceptance-preflight", "--manifest", paths.manifest]);
  assert.equal(missing.stdout, '{"status":"error","code":"arguments-invalid"}\n');
});

test("the reviewed QA v3 artifact matches the pinned digest", async () => {
  const script = await readFile(join(repoRoot, "deploy/browser-acceptance/private-browser-acceptance-v2.mjs"));
  const pinned = (await readFile(runScript, "utf8")).match(/^readonly APPROVED_QA_SCRIPT_SHA256="([0-9a-f]{64})"$/mu)[1];
  assert.equal(createHash("sha256").update(script).digest("hex"), pinned);
});

test("acceptance preflight takes the shared lock before reading either manifest", async (t) => {
  const paths = await setup(t);
  let createdLockFile = false;
  try {
    await writeFile(paths.sharedRuntimeLock, "", { flag: "wx", mode: 0o666 });
    createdLockFile = true;
  } catch (error) {
    if (error.code !== "EEXIST") throw error;
  }
  if (createdLockFile) await chmod(paths.sharedRuntimeLock, 0o666);
  const holder = spawn("bash", ["-c", 'exec 9>>"$1"; flock -n 9 || exit; printf locked; IFS= read -r _',
    "lock-holder", paths.sharedRuntimeLock], {
    stdio: ["pipe", "pipe", "ignore"],
  });
  let holderOutput = "";
  holder.stdout.setEncoding("utf8").on("data", (chunk) => { holderOutput += chunk; });
  try {
    await new Promise((resolve, reject) => {
      const timeout = setTimeout(() => reject(new Error("lock holder did not acquire the fixture lock")), 5000);
      holder.stdout.on("data", () => {
        if (holderOutput.includes("locked")) {
          clearTimeout(timeout);
          resolve();
        }
      });
      holder.on("error", reject);
    });
    const result = invokePreflight(paths, await preflightScript(paths));
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-busy"}\n');
    await assert.rejects(readFile(paths.curlLog));
    await assert.rejects(readFile(paths.log));
  } finally {
    if (holder.exitCode === null && holder.signalCode === null) {
      const closed = new Promise((resolve) => holder.once("close", resolve));
      holder.kill("SIGKILL");
      await closed;
    }
  }
});

test("each probe readiness failure maps to its fixed error without a QA run", async (t) => {
  for (const code of ["runtime-unavailable", "environment-unproven", "revision-unproven", "runtime-changed",
    "network-unproven", "fingerprint-unproven", "database-unavailable", "account-created", "account-unproven"]) {
    const paths = await setup(t);
    const accountEvidence = code === "account-created" || code === "account-unproven" ? {
      accountEvidence: code === "account-unproven" ? "unavailable" : "complete",
      accountsUnchanged: false,
    } : {};
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PROBE_SKIP_BROWSER: "1", WRAPPER_PROBE_STATUS: "1", WRAPPER_PREFLIGHT_OUTPUT: "",
      WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code, ...accountEvidence })),
    });
    assert.equal(result.status, 1, code);
    const expected = code === "database-unavailable"
      ? '{"status":"error","code":"database-unavailable","database_failure_category":"unknown"}\n'
      : `{"status":"error","code":"${code}"}\n`;
    assert.equal(result.stdout, expected, code);
    assert.doesNotMatch(await readFile(paths.log, "utf8"), /approved-qa\.mjs/u, code);
  }
});

test("preflight rejects a non-string database failure category", async (t) => {
  for (const category of [[], ["config"]]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PROBE_SKIP_BROWSER: "1", WRAPPER_PROBE_STATUS: "1", WRAPPER_PREFLIGHT_OUTPUT: "",
      WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code: "database-unavailable",
        databaseFailureCategory: category })),
    });
    assert.equal(result.status, 1);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n', JSON.stringify(category));
    assert.equal(result.stderr, "");
  }
});

test("a failed QA preflight exposes only its allow-listed failure code", async (t) => {
  const cases = [
    ["known code", '{"status":"error","code":"websocket-not-ready"}', "websocket-not-ready"],
    ["browser close", '{"status":"error","code":"browser-closed"}', "browser-closed"],
    ["blocked host control", '{"status":"error","code":"blocked-host-control-failed"}', "blocked-host-control-failed"],
    ["direct egress", '{"status":"error","code":"direct-egress-open"}', "direct-egress-open"],
    ["observer health", '{"status":"error","code":"observer-unhealthy"}', "observer-unhealthy"],
    ["target address", '{"status":"error","code":"target-address-invalid"}', "target-address-invalid"],
    ["target address unavailable", '{"status":"error","code":"target-address-unavailable"}', "target-address-unavailable"],
    ["unknown code", '{"status":"error","code":"private-canary"}', "unknown"],
    ["crash without a code", "", "unknown"],
    ["free text in code", '{"status":"error","code":"websocket-not-ready private-canary"}', "unknown"],
  ];
  for (const [name, qaOutput, expectedCode] of cases) {
    await t.test(name, async (t) => {
      const paths = await setup(t);
      const result = invokePreflight(paths, await preflightScript(paths), {
        WRAPPER_PREFLIGHT_OUTPUT: qaOutput, WRAPPER_PROBE_STATUS: "1",
        WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code: "browser-failed", browserExitOk: false })),
      });
      assert.equal(result.status, 1);
      assert.equal(result.stdout,
        `{"status":"error","code":"browser-failed","qa_failure_code":"${expectedCode}"}\n`);
      assert.equal(result.stderr, "");
      assert.doesNotMatch(result.stdout, /private-canary/u);
    });
  }
});

test("QA WSS diagnostics survive nested host failures and teardown", async (t) => {
  const qaOutput = wssError("socket-2-target-mismatch", 2, 2, false, 101);
  const expectedDiagnostic = '"qa_failure_code":"websocket-not-ready","wss_diagnostic":"socket-2-target-mismatch",' +
    '"wss_targets":2,"wss_handshakes":2,"wss_target_match":false,"wss_status":101';

  for (const code of ["browser-failed", "runtime-changed"]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PREFLIGHT_OUTPUT: qaOutput,
      WRAPPER_PROBE_STATUS: "1",
      WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code, browserExitOk: false })),
    });
    assert.equal(result.status, 1, code);
    assert.equal(result.stdout,
      `{"status":"error","code":"${code}",${expectedDiagnostic}}\n`, code);
    assert.equal(result.stderr, "", code);
  }

  const paths = await setup(t);
  const result = invokePreflight(paths, await preflightScript(paths), {
    WRAPPER_PREFLIGHT_OUTPUT: qaOutput,
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({
      ok: false, code: "browser-failed", browserExitOk: false,
    })),
    WRAPPER_CLEANUP_FAILURE: "1",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout,
    `{"status":"error","code":"cleanup-failed",${expectedDiagnostic}}\n`);
  assert.equal(result.stderr, "");
});

test("a failed QA preflight code survives a later host stability or network failure", async (t) => {
  for (const code of ["runtime-changed", "network-unproven"]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PREFLIGHT_OUTPUT: '{"status":"error","code":"websocket-not-ready"}',
      WRAPPER_PROBE_STATUS: "1",
      WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code, browserExitOk: false })),
    });
    assert.equal(result.status, 1, code);
    assert.equal(result.stdout,
      `{"status":"error","code":"${code}","qa_failure_code":"websocket-not-ready"}\n`, code);
    assert.equal(result.stderr, "", code);
  }
});

test("a failed QA preflight code survives a later host failure and cleanup failure", async (t) => {
  const paths = await setup(t);
  const result = invokePreflight(paths, await preflightScript(paths), {
    WRAPPER_PREFLIGHT_OUTPUT: '{"status":"error","code":"websocket-not-ready"}',
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code: "network-unproven", browserExitOk: false })),
    WRAPPER_CLEANUP_FAILURE: "1",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout,
    '{"status":"error","code":"cleanup-failed","qa_failure_code":"websocket-not-ready"}\n');
  assert.equal(result.stderr, "");
});

test("QA failure code survives wrapper cleanup failure", async (t) => {
  const paths = await setup(t);
  const result = invokePreflight(paths, await preflightScript(paths), {
    WRAPPER_PREFLIGHT_OUTPUT: '{"status":"error","code":"websocket-not-ready"}',
    WRAPPER_PROBE_STATUS: "1",
    WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code: "browser-failed", browserExitOk: false })),
    WRAPPER_CLEANUP_FAILURE: "1",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout,
    '{"status":"error","code":"cleanup-failed","qa_failure_code":"websocket-not-ready"}\n');
  assert.equal(result.stderr, "");
});

test("acceptance preflight rejects malformed, extra-field, v1 and acceptance-shaped probe output", async (t) => {
  const canary = "private-probe-output-canary";
  for (const [output, status] of [
    [JSON.stringify({ ...preflightProbeValue(), detail: canary }), "0"],
    [JSON.stringify({ ...preflightProbeValue(), schemaVersion: 3 }), "0"],
    [JSON.stringify({ ...preflightProbeValue(), registrationClosed: true }), "0"],
    [JSON.stringify(probeResultValue), "0"],
    [JSON.stringify(preflightProbeValue({ boundAfter: 2 })), "0"],
    [JSON.stringify(preflightProbeValue({ browserExitOk: false })), "0"],
    [JSON.stringify(preflightProbeValue({ serverSourceCommit: "" })), "0"],
    [JSON.stringify(preflightProbeValue()), "1"],
    [JSON.stringify(preflightProbeValue({ ok: false, code: canary })), "1"],
    [`{"code":"${canary}",${JSON.stringify(preflightProbeValue()).slice(1)}`, "0"],
    [`${JSON.stringify(preflightProbeValue())}\n${JSON.stringify(preflightProbeValue())}`, "0"],
    ["not json", "0"],
  ]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PROBE_OUTPUT: output, WRAPPER_PROBE_STATUS: status,
    });
    assert.equal(result.status, 1, output);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n', output);
    assert.equal(result.stderr, "");
  }
});

test("acceptance preflight rejects any QA output other than the fixed ready line", async (t) => {
  for (const line of [
    "browser acceptance preflight ready extra",
    "browser acceptance preflight failed",
    `browser acceptance preflight ready\nbrowser acceptance preflight ready`,
    acceptanceResult,
    "",
  ]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), { WRAPPER_PREFLIGHT_OUTPUT: line });
    assert.equal(result.status, 1, line);
    assert.equal(result.stdout, '{"status":"error","code":"runtime-failed"}\n', line);
  }
});
