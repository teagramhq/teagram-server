import assert from "node:assert/strict";
import { createServer } from "node:http";
import { chmod, mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const runScript = join(repoRoot, "deploy/browser-acceptance/run.sh");
const busyboxPhaseScript = join(repoRoot, ".github/scripts/run-busybox-phase-check.sh");
const sourceCommit = "c".repeat(40);
const phaseCommandToken = "e".repeat(64);
const digest = `sha256:${"d".repeat(64)}`;
const approvedQaDigest = "2ed4107f76a4b2ed6bdbd9933b7009f62d4dba4b2ecb8349835a181fcda6b10c";
const blockedResult = "{\"status\":\"blocked_expected\",\"blocked_telegram_org_attempts\":1,\"telegram_org_upstream_connects\":0,\"payloads_retained\":0}";
const readyResult = "{\"status\":\"ready\",\"http_status\":200,\"asset_502_count\":0,\"browser_wss_status\":101,\"browser_wss_unique_targets\":1,\"observer_success_hosts\":1,\"observer_target_host\":\"telegram-server.tailaa4918.ts.net\",\"telegram_org_attempts\":0,\"payloads_retained\":0}";
const acceptanceResult = "{\"schemaVersion\":1,\"ok\":true}";

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
    url: `https://telegram-server.tailaa4918.ts.net/client/?v=${sourceCommit}`,
  }));
  await writeFile(servedManifest, JSON.stringify({
    mode: "private",
    endpoint: "wss://telegram-server.tailaa4918.ts.net/apiws",
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
    if [[ "$*" == *"/run/approved-qa.mjs --mode acceptance"* ]]; then
      if [[ "\${WRAPPER_REQUIRE_CLOSED_STDIN:-0}" == "1" ]]; then
        IFS= read -r -t 0.1 input || exit 99
        [[ "$input" == "credential-sentinel" ]] || exit 98
      fi
      printf '%s' '${acceptanceResult}'
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
  await import("node:fs/promises").then(({ chmod }) => Promise.all([
    chmod(join(bin, "docker"), 0o755),
    chmod(join(bin, "df"), 0o755),
    chmod(join(bin, "curl"), 0o755),
    chmod(join(bin, "sha256sum"), 0o755),
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
  assert.equal(result.stdout, `${acceptanceResult}\n`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.ok(calls.includes(`${manifest}:/run/release-record.json:ro`));
  assert.ok(calls.includes(`${script}:/run/approved-qa.mjs:ro`));
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
    endpoint: "wss://telegram-serverXtailaa4918YtsZnet/apiws",
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
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.stdout, `${acceptanceResult}${String.fromCharCode(10)}`);
  assert.equal(result.stderr, "");
  const calls = await readFile(paths.log, "utf8");
  assert.match(calls, /browser \/run\/approved-qa\.mjs --mode acceptance/u);
  assert.match(calls, /compose .* down --rmi local/u);
  assert.doesNotMatch(calls, /credential-sentinel/u);
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
