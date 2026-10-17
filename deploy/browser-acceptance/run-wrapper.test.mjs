import assert from "node:assert/strict";
import { createServer } from "node:http";
import { chmod, mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const runScript = join(repoRoot, "deploy/browser-acceptance/run.sh");
const busyboxPhaseScript = join(repoRoot, ".github/scripts/run-busybox-phase-check.sh");
const sourceCommit = "c".repeat(40);
const phaseCommandToken = "e".repeat(64);
const digest = `sha256:${"d".repeat(64)}`;
// teagram-web opens one client socket per account slot on an unauthenticated load.
const WSS_CONNECTION_CEILING = 4;
const approvedQaDigest = "27c94bd75bcc620d7a16650704ab2b672d2618d95196acc000c1437a10ba92cd";
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
  schemaVersion: 3, ok: true, code: "ok", serverSourceCommit: "f".repeat(40),
  registrationClosed: true, loginCodesDisabled: true, revisionMatched: true,
  containersStable: true, networkIsolated: true, fingerprintMatched: true,
  newBindingProven: true, cleanupProven: true, browserExitOk: true,
  boundBefore: 2, boundAfter: 2, pendingBefore: 0, pendingAfter: 0,
  bindingEvidence: "complete", bindingAdditions: 1, bindingRemovals: 1,
  witnessInWindow: true, baselineIntact: true, topologyIsolated: true,
  captureWindow: "complete", captureAllowedConnects: 1, captureTelegramSuffix: 1,
  captureOther: 0, captureBlocked: 1, captureUpstreamFailures: 0,
};

function preflightProbeValue(overrides = {}) {
  return {
    ...probeResultValue, newBindingProven: false, cleanupProven: false, boundAfter: 0, pendingAfter: 0,
    bindingEvidence: "not-reached", bindingAdditions: 0, bindingRemovals: 0,
    witnessInWindow: false, baselineIntact: false,
    ...overrides,
  };
}
const preflightReady = `{"status":"acceptance-ready","web_source_commit":"${sourceCommit}","server_source_commit":"${"f".repeat(40)}",` +
  '"registration_closed":true,"login_codes_disabled":true,"server_revision_proven":true,"runtime_stable":true,' +
  '"network_isolated":true,"fingerprint_matched":true,"binding_evidence_complete":true,"qa_preflight_ready":true}';

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

const probeV3ResultValue = {
  ...probeResultValue,
  schemaVersion: 3,
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
    evidenceVersion: 1,
    status: probe.ok && probe.code === "ok" && qaResult === "valid" && qa.ok && teardown === "ok"
      ? "accepted" : "error",
    code: probe.code,
    teardown,
    targetHost: "teagram.tailaa4918.ts.net",
    sourceCommit,
    serverSourceCommit: probe.serverSourceCommit,
    registrationClosed: probe.registrationClosed,
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
    ...probeV3ResultValue,
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
    ...probeV3ResultValue,
    ok: false,
    code: "revision-unproven",
    serverSourceCommit: "",
    registrationClosed: false,
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
  const lockDir = join(runtimeDir, "telegram-browser-acceptance");
  const manifest = join(directory, "release-record.json");
  const servedManifest = join(directory, "served-manifest.json");
  const log = join(directory, "docker-calls.log");
  const curlLog = join(directory, "curl-calls.log");
  await mkdir(bin);
  await mkdir(dockerRoot);
  await mkdir(runtimeDir, { mode: 0o700 });
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
  return { bin, directory, dockerRoot, runtimeDir, lockDir, manifest, servedManifest, log, curlLog };
}

function invoke(paths, args, extraEnv = {}, input, cwd = repoRoot) {
  return spawnSync("bash", [runScript, ...args], {
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

async function invokeBusyboxPhase(t, lane, { phase = "", mode = "valid", status = 1, outputMode = "standard" } = {}) {
  const directory = await mkdtemp(join(tmpdir(), "busybox-phase-protocol-"));
  const bin = join(directory, "bin");
  const argsLog = join(directory, "docker-args.log");
  await mkdir(bin);
  await writeFile(join(bin, "docker"), `#!/usr/bin/env bash
set -eu
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
      assert.ok(args.includes("bash deploy/telegramd/rollout-runner/test-compose-path-preflight.sh"));
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
    registrationClosed: false,
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

test("malformed and absent QA output preserve validated probe evidence", async (t) => {
  const paths = await setup(t);
  const script = join(paths.dockerRoot, "approved-qa.mjs");
  await writeFile(script, "approved fixture\n");
  const probe = {
    ...probeV3ResultValue,
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
  const probe = probeV3ResultValue;
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
  const probe = probeV3ResultValue;
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

test("a symlink at the private runtime lock path is rejected without touching its target", async (t) => {
  const paths = await setup(t);
  const targetDir = join(paths.runtimeDir, "target");
  const sentinel = join(targetDir, "sentinel");
  await mkdir(targetDir, { mode: 0o700 });
  await writeFile(sentinel, "preserve this file");
  await symlink(targetDir, paths.lockDir, "dir");

  const result = invoke(paths, ["readiness", "--manifest", paths.manifest]);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-unavailable"}\n');
  assert.equal(result.stderr, "");
  assert.equal(await readFile(sentinel, "utf8"), "preserve this file");
  await assert.rejects(readFile(paths.log));
});

test("the home runtime fallback creates an owner-only lock directory", async (t) => {
  const paths = await setup(t);
  const homeDir = join(paths.runtimeDir, "home");
  const lockDir = join(homeDir, ".local", "run", "telegram-browser-acceptance");
  await mkdir(homeDir, { mode: 0o700 });

  const result = invoke(paths, ["readiness", "--manifest", paths.manifest], {
    HOME: homeDir,
    XDG_RUNTIME_DIR: "",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal((await stat(lockDir)).mode & 0o777, 0o700);
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
    ["ambiguous", 0, 2, false, 0],
    ["no-target", 0, 0, false, 0],
    ["target-mismatch", 2, 2, false, 101],
    ["no-handshake", 1, 0, true, 0],
    ["handshake-not-101", 1, 1, true, 503],
    ["handshake-not-101", 2, 2, true, 503],
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
    wssError("ambiguous", 1, 0, true, 0),
    wssError("no-target", 1, 0, false, 0),
    wssError("target-mismatch", 0, 0, false, 0),
    wssError("target-mismatch", 2, 1, false, 503),
    wssError("target-mismatch", 1, 1, true, 503),
    wssError("target-mismatch", 1, 0, false, 101),
    wssError("no-handshake", 0, 0, true, 0),
    wssError("no-handshake", 1, 1, true, 0),
    wssError("no-handshake", 1, 0, false, 0),
    wssError("handshake-not-101", 0, 0, true, 503),
    wssError("handshake-not-101", 2, 1, true, 503),
    wssError("handshake-not-101", 1, 1, false, 503),
    wssError("handshake-not-101", 1, 1, true, 101),
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
    wss_diagnostic: "handshake-not-101",
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
  assert.equal(pinned, approvedQaDigest);
});

test("acceptance preflight takes the runtime lock before reading either manifest", async (t) => {
  const paths = await setup(t);
  await mkdir(paths.lockDir, { mode: 0o700 });
  const holder = spawn("flock", ["--nonblock", paths.lockDir, "sleep", "5"], { stdio: "ignore" });
  t.after(() => holder.kill());
  await new Promise((resolve) => setTimeout(resolve, 200));
  const result = invokePreflight(paths, await preflightScript(paths));
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"runtime-busy"}\n');
  await assert.rejects(readFile(paths.curlLog));
  await assert.rejects(readFile(paths.log));
});

test("each probe readiness failure maps to its fixed error without a QA run", async (t) => {
  for (const code of ["runtime-unavailable", "environment-unproven", "revision-unproven", "runtime-changed",
    "network-unproven", "fingerprint-unproven", "database-unavailable"]) {
    const paths = await setup(t);
    const result = invokePreflight(paths, await preflightScript(paths), {
      WRAPPER_PROBE_SKIP_BROWSER: "1", WRAPPER_PROBE_STATUS: "1", WRAPPER_PREFLIGHT_OUTPUT: "",
      WRAPPER_PROBE_OUTPUT: JSON.stringify(preflightProbeValue({ ok: false, code })),
    });
    assert.equal(result.status, 1, code);
    assert.equal(result.stdout, `{"status":"error","code":"${code}"}\n`, code);
    assert.doesNotMatch(await readFile(paths.log, "utf8"), /approved-qa\.mjs/u, code);
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
    [JSON.stringify({ ...preflightProbeValue(), schemaVersion: 1 }), "0"],
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
