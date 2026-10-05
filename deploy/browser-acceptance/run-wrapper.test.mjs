import assert from "node:assert/strict";
import { createServer } from "node:http";
import { mkdtemp, mkdir, readFile, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn, spawnSync } from "node:child_process";
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
set -eu
if [[ "$*" == *"--version"* ]]; then printf '%s\\n' "curl \${WRAPPER_CURL_VERSION:-8.5.0} wrapper"; exit 0; fi
printf '%s\\n' "$*" >> "$WRAPPER_CURL_LOG"
case "\${WRAPPER_CURL_MODE:-}" in
  oversized-content-length) exit 63 ;;
  oversized-chunked) head -c 16385 /dev/zero | tr '\\000' x; exit 63 ;;
esac
cat "$WRAPPER_SERVED_MANIFEST"
`);
  await import("node:fs/promises").then(({ chmod }) => Promise.all([
    chmod(join(bin, "docker"), 0o755),
    chmod(join(bin, "df"), 0o755),
    chmod(join(bin, "curl"), 0o755),
  ]));
  t.after(async () => rm(directory, { recursive: true, force: true }));
  return { bin, dockerRoot, runtimeDir, lockDir, manifest, servedManifest, log, curlLog };
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
      WRAPPER_CURL_LOG: paths.curlLog,
      WRAPPER_SERVED_MANIFEST: paths.servedManifest,
      XDG_RUNTIME_DIR: paths.runtimeDir,
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
