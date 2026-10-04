import assert from "node:assert/strict";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const runScript = join(repoRoot, "deploy/browser-acceptance/run.sh");
const sourceCommit = "c".repeat(40);
const digest = `sha256:${"d".repeat(64)}`;
const blockedResult = "{\"status\":\"blocked_expected\",\"blocked_telegram_org_attempts\":1,\"telegram_org_upstream_connects\":0,\"payloads_retained\":0}";
const readyResult = "{\"status\":\"ready\",\"http_status\":200,\"asset_502_count\":0,\"browser_wss_status\":101,\"browser_wss_unique_targets\":1,\"observer_success_hosts\":1,\"observer_target_host\":\"telegram-server.tailaa4918.ts.net\",\"telegram_org_attempts\":0,\"payloads_retained\":0}";

async function setup(t) {
  const directory = await mkdtemp(join(tmpdir(), "browser-acceptance-wrapper-"));
  const bin = join(directory, "bin");
  const dockerRoot = join(directory, "docker-root");
  const manifest = join(directory, "release-record.json");
  const servedManifest = join(directory, "served-manifest.json");
  const log = join(directory, "docker-calls.log");
  await mkdir(bin);
  await mkdir(dockerRoot);
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
  if [[ "$*" == *" run "* ]]; then
    if [[ "\${WRAPPER_REQUIRE_CLOSED_STDIN:-0}" == "1" ]]; then
      if IFS= read -r -t 0.1 input; then exit 99; fi
    fi
    if [[ "\${WRAPPER_RUNTIME_FAILURE:-0}" == "1" ]]; then printf '%s\\n' '{"status":"error","code":"observer-unhealthy"}'; exit 1; fi
    if [[ "$*" == *" browser readiness "* ]]; then printf '%s\\n' '${readyResult}'; else printf '%s\\n' '${blockedResult}'; fi
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
cat "$WRAPPER_SERVED_MANIFEST"
`);
  await import("node:fs/promises").then(({ chmod }) => Promise.all([
    chmod(join(bin, "docker"), 0o755),
    chmod(join(bin, "df"), 0o755),
    chmod(join(bin, "curl"), 0o755),
  ]));
  t.after(async () => rm(directory, { recursive: true, force: true }));
  return { bin, dockerRoot, manifest, servedManifest, log };
}

function invoke(paths, args, extraEnv = {}, input) {
  return spawnSync("bash", [runScript, ...args], {
    cwd: repoRoot,
    encoding: "utf8",
    ...(input === undefined ? {} : { input }),
    env: {
      ...process.env,
      PATH: `${paths.bin}:${process.env.PATH}`,
      WRAPPER_DOCKER_LOG: paths.log,
      WRAPPER_DOCKER_ROOT: paths.dockerRoot,
      WRAPPER_SERVED_MANIFEST: paths.servedManifest,
      ...extraEnv,
    },
  });
}

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
