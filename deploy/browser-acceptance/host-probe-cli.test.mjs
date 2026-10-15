import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmod, link, mkdtemp, mkdir, readFile, rm, symlink, unlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const probePath = join(repoRoot, "deploy/browser-acceptance/host-probe.mjs");
const webRevision = "c".repeat(40);
const serverRevision = "f".repeat(40);
const ids = { server: "b".repeat(64), browser: "d".repeat(64), observer: "e".repeat(64) };
const privateCanary = "private-probe-password-canary";

async function hostFixture(t) {
  const directory = await mkdtemp(join(tmpdir(), "acceptance-host-probe-"));
  const bin = join(directory, "bin");
  const log = join(directory, "commands.log");
  const credential = join(directory, "probe.pgpass");
  const config = join(directory, "probe-config.json");
  const manifest = join(directory, "release.json");
  const script = join(directory, "qa.mjs");
  const dbReadCount = join(directory, "db-read-count");
  const queryLog = join(directory, "queries.sql");
  const twoEvidence = join(directory, "two-evidence");
  const evidenceRoot = join(directory, "runner-evidence");
  const buildDirectory = join(evidenceRoot, "run-20261013.build");
  const buildIdentity = join(buildDirectory, "built-image.identity");
  await mkdir(bin);
  await mkdir(evidenceRoot, { mode: 0o700 });
  await mkdir(buildDirectory, { mode: 0o700 });
  await writeFile(buildIdentity,
    `captured_at_utc=2026-10-13T00:00:00Z source_sha=${serverRevision} image_id=sha256:${"a".repeat(64)} revision_label_json="${serverRevision}"\n`,
    { mode: 0o600 });
  await writeFile(credential, `127.0.0.1:5432:telegram:browser_acceptance_probe:${privateCanary}\n`, { mode: 0o600 });
  await writeFile(config, '{"userId":"987654321098765432"}', { mode: 0o600 });
  await writeFile(manifest, JSON.stringify({
    sourceCommit: webRevision, archiveSha256: `sha256:${"a".repeat(64)}`, contentDigest: `sha256:${"a".repeat(64)}`,
    url: `https://telegram-server.tailaa4918.ts.net/client/?v=${webRevision}`,
  }));
  await writeFile(script, "approved fixture\n");
  await writeFile(join(bin, "docker"), `#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> "$HOST_COMMAND_LOG"
if [[ "$1" == info ]]; then
  if [[ "$*" == *DockerRootDir* ]]; then printf '%s\\n' '${directory}'; else printf '%s\\n' aarch64; fi
  exit 0
fi
case "$1 $2" in
  'ps --quiet') printf '%s\\n' '${ids.server}'; exit 0 ;;
  'container inspect')
    if [[ "$*" == *'"flags"'* ]]; then
      printf '%s\\n' '{"id":"${ids.server}","imageId":"sha256:${"a".repeat(64)}","revision":"${serverRevision}","labels":{"com.docker.compose.service":"telegramd","org.opencontainers.image.revision":"${serverRevision}"},"startedAt":"2026-10-13T00:00:00Z","running":true,"flags":["TG_REGISTRATION=closed","TG_LOG_LOGIN_CODES=false","TG_RSA_KEY_FINGERPRINT=81985529216486895",null]}'
    elif [[ "$*" == *'"startedAt"'* ]]; then
      printf '%s\\n' '{"id":"${ids.observer}","startedAt":"2026-10-13T00:00:00Z","running":true}'
    else
      printf '%s\\n' '{"id":"${ids.server}","path":"/usr/local/bin/telegramd","service":"telegramd"}'
    fi
    exit 0 ;;
  'image inspect') printf '%s\\n' '{"id":"sha256:${"a".repeat(64)}","revision":"${serverRevision}","labels":{"org.opencontainers.image.revision":"${serverRevision}"}}'; exit 0 ;;
  'network inspect') printf '%s\\n' '{"internal":true,"members":["${ids.browser}","${ids.observer}",null]}'; exit 0 ;;
  "exec ${ids.observer}")
    [[ "$*" == "exec ${ids.observer} node /opt/browser-acceptance/observer-snapshot.mjs" ]] || exit 93
    if [[ "\${HOST_OBSERVER_OVERSIZE:-0}" == 1 ]]; then head -c 20000 /dev/zero | tr '\\0' ' '; exit 0; fi
    if [[ -e '${directory}/observer-read' ]]; then
      printf '%s\\n' '{"status":"healthy","allowed_host":"telegram-server.tailaa4918.ts.net","connect_attempts":{"other":0,"telegram-server.tailaa4918.ts.net":2,"telegram-suffix":1},"allowed_connects":2,"blocked_requests":1,"telegram_attempts":1,"other_blocked_count":0,"dns_lookups":2,"upstream_connects":2,"upstream_failures":0}'
    else
      : > '${directory}/observer-read'
      printf '%s\\n' '{"status":"healthy","allowed_host":"telegram-server.tailaa4918.ts.net","connect_attempts":{"other":0,"telegram-server.tailaa4918.ts.net":1,"telegram-suffix":0},"allowed_connects":1,"blocked_requests":0,"telegram_attempts":0,"other_blocked_count":0,"dns_lookups":1,"upstream_connects":1,"upstream_failures":0}'
    fi
    exit 0 ;;
  "exec ${ids.browser}")
    [[ "$*" == "exec ${ids.browser} node /opt/browser-acceptance/approved-qa.mjs --mode preflight --release-record /run/release-record.json --runtime-adapter /opt/browser-acceptance/qa-runtime-adapter.mjs" ]] || exit 95
    # The fake QA preflight fails if any byte reaches its stdin.
    if IFS= read -r -t 1 -n 1 input || [[ -n "\${input:-}" ]]; then
      : > '${directory}/preflight-stdin-touched'
      printf '%s\\n' 'browser acceptance preflight failed'
      exit 1
    fi
    if [[ "\${HOST_BROWSER_STATUS:-0}" != 0 ]]; then printf '%s\\n' 'browser acceptance preflight failed'; exit 1; fi
    printf '%s\\n' 'browser acceptance preflight ready'
    exit 0 ;;
  'exec --interactive')
    [[ "$*" == *'node /opt/browser-acceptance/approved-qa.mjs --mode acceptance'* ]] || exit 95
    if [[ "\${HOST_READ_STDIN:-0}" == 1 ]]; then IFS= read -r input; [[ "$input" == credential-sentinel ]] || exit 94; fi
    printf '%s\\n' '{"schemaVersion":2,"ok":true,"phase":"signed-in","outcome":"ok","logout":"ok","targetHost":"telegram-server.tailaa4918.ts.net","sourceCommit":"${webRevision}","wssConnections":1}'
    exit "\${HOST_BROWSER_STATUS:-0}" ;;
esac
if [[ "$1" == compose ]]; then
  if [[ "$*" == *version* || "$*" == *' build '* || "$*" == *' up --detach --wait observer'* || "$*" == *' down --rmi local'* ]]; then exit 0; fi
  if [[ "$*" == *'--file - up '* ]]; then
    input="$(cat)"
    [[ "$input" == *'/opt/browser-acceptance/approved-qa.mjs'* && "$input" != *'private-probe'* && "$input" != *'987654321098765432'* ]] || exit 92
    exit 0
  fi
  if [[ "$*" == *'ps --all --quiet browser'* ]]; then printf '%s\\n' '${ids.browser}'; exit 0; fi
  if [[ "$*" == *'ps --all --quiet observer'* ]]; then printf '%s\\n' '${ids.observer}'; exit 0; fi
fi
exit 91
`);
  await writeFile(join(bin, "psql"), `#!/usr/bin/env bash
set -eu
HOST_COMMAND_LOG='${log}'
HOST_DB_READ_COUNT='${dbReadCount}'
HOST_QUERY_LOG='${queryLog}'
printf '%s\\n' "psql $*" >> "$HOST_COMMAND_LOG"
[[ "$PGUSER" == browser_acceptance_probe && "$PGHOST" == 127.0.0.1 ]] || exit 90
[[ ! -v PGPASSWORD ]] || exit 85
[[ "$(cat "$PGPASSFILE")" == '127.0.0.1:5432:telegram:browser_acceptance_probe:${privateCanary}' ]] || exit 84
[[ "$PGOPTIONS" == *default_transaction_read_only=on* ]] || exit 89
query="$(cat)"
[[ "$query" == *public.browser_acceptance_binding_snapshot_v1* && "$query" != *browser_acceptance_bound_auth_key_ids* && "$query" != *browser_acceptance_pending_sign_in_count* ]] || exit 88
[[ "$query" == *987654321098765432* ]] || exit 87
[[ "$query" != *key_value* ]] || exit 87
[[ "$query" != *DELETE* && "$query" != *UPDATE* ]] || exit 87
if [[ -e '${directory}/db-error' ]]; then printf '%s\\n' '${privateCanary}' >&2; exit 86; fi
printf '%s\\n' "$query" >> "$HOST_QUERY_LOG"
if [[ -f "$HOST_DB_READ_COUNT" ]]; then read -r cut_count < "$HOST_DB_READ_COUNT"; else cut_count=0; fi
cut_count=$((cut_count + 1))
printf '%s\\n' "$cut_count" > "$HOST_DB_READ_COUNT"
if [[ "$cut_count" == 1 ]]; then
  printf '%s\\n' '{"complete":true,"evidence_version":1,"epoch":"9","additions":"10","removals":"8","last_added_created_at":"1791878399000000","bound_key_ids":["1","2"],"bound_key_created_ats":["1791878399000000","1791878399000000"],"pending_count":"0","db_time":"1791878400000000"}'
elif [[ -e '${twoEvidence}' ]]; then
  printf '%s\\n' '{"complete":true,"evidence_version":1,"epoch":"9","additions":"12","removals":"10","last_added_created_at":"1791878400000002","bound_key_ids":["1","2"],"bound_key_created_ats":["1791878399000000","1791878399000000"],"pending_count":"0","db_time":"1791878400000003"}'
else
  printf '%s\\n' '{"complete":true,"evidence_version":1,"epoch":"9","additions":"11","removals":"9","last_added_created_at":"1791878400000001","bound_key_ids":["1","2"],"bound_key_created_ats":["1791878399000000","1791878399000000"],"pending_count":"0","db_time":"1791878400000003"}'
fi
`);
  await writeFile(join(bin, "curl"), `#!/usr/bin/env bash
set -eu
if [[ "$*" == *--version* ]]; then printf '%s\\n' 'curl 8.5.0'; exit 0; fi
printf '%s\\n' "curl $*" >> "$HOST_COMMAND_LOG"
printf '%s\\n' '{"mode":"private","endpoint":"wss://telegram-server.tailaa4918.ts.net/apiws","fingerprint":"0123456789abcdef","sourceCommit":"${webRevision}","artifactDigest":"sha256:${"a".repeat(64)}"}'
`);
  await writeFile(join(bin, "df"), `#!/usr/bin/env bash
printf '%s\\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on' 'fake 5000000 0 3000000 0% ${directory}'
`);
  await writeFile(join(bin, "sha256sum"), `#!/usr/bin/env bash
printf '%s\\n' '5fc4b73d2614b322bf1e81c064696a2f67262e54a8cfacb5bfe283647605fe97  approved-fixture'
`);
  await Promise.all(["docker", "psql", "curl", "df", "sha256sum"].map((name) => chmod(join(bin, name), 0o755)));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return { directory, bin, log, credential, config, manifest, script, dbReadCount, queryLog, twoEvidence,
    evidenceRoot, buildDirectory, buildIdentity };
}

function invoke(paths, extraEnv = {}, args = [], input = "credential-sentinel\n", mode = "acceptance") {
  return spawnSync(process.execPath, [probePath, "--mode", mode, "--manifest", paths.manifest, "--script", paths.script,
    "--credential", paths.credential, "--config", paths.config, ...args], {
    cwd: repoRoot, input, encoding: "utf8", timeout: 10_000,
    stdio: ["pipe", "pipe", "pipe", "pipe"],
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, HOST_COMMAND_LOG: paths.log,
      HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog,
      ROLLOUT_RUNNER_TEST_MODE: "1", ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT: paths.evidenceRoot, ...extraEnv },
  });
}

test("host CLI emits one proof line and forwards browser stdout on a separate descriptor", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, { HOST_READ_STDIN: "1" });
  assert.equal(result.status, 0, `${result.stderr}${result.stdout}\n${await readFile(paths.log, "utf8")}`);
  assert.match(result.stdout, /^\{"schemaVersion":2,/u, "host CLI must produce an independent proof");
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.ok, true);
  assert.equal(proof.code, "ok");
  assert.equal(proof.serverSourceCommit, serverRevision);
  assert.equal(result.stdout.split("\n").length, 2);
  assert.equal(JSON.parse(result.output[3]).phase, "signed-in");
  assert.equal(result.stderr, "");
  const commands = await readFile(paths.log, "utf8");
  assert.doesNotMatch(`${result.stdout}${result.stderr}${result.output[3]}${commands}`, /private-probe|987654321098765432|credential-sentinel/u);
  assert.match(commands, /TG_REGISTRATION.*TG_LOG_LOGIN_CODES.*TG_RSA_KEY_FINGERPRINT/u);
  assert.match(commands, /ps --all --quiet observer/u);
  assert.equal(commands.split("\n").filter((line) => (
    line === `exec ${ids.observer} node /opt/browser-acceptance/observer-snapshot.mjs`)).length, 2,
  "the host reads the observer itself at both cuts");
  assert.doesNotMatch(commands, /json \.Config\.Env|\.env|TG_POSTGRES_DSN|TG_AUTHKEY_ENC_KEY/u);
  const queries = await readFile(paths.queryLog, "utf8");
  assert.equal((queries.match(/public\.browser_acceptance_binding_snapshot_v1/g) ?? []).length, 2,
    "one pinned atomic API call must supply each cut");
  assert.doesNotMatch(queries, /browser_acceptance_bound_auth_key_ids|browser_acceptance_pending_sign_in_count/u);
  assert.match(queries, /bound_key_ids/u);
  assert.match(queries, /bound_key_created_ats/u);
  assert.match(queries, /last_added_created_at/u);
});

test("the CLI rejects two transient bindings from atomic deltas", async (t) => {
  const paths = await hostFixture(t);
  await writeFile(paths.twoEvidence, "");
  const result = invoke(paths);
  assert.equal(result.status, 1);
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.code, "cleanup-unproven");
  assert.equal(proof.ok, false);
  assert.equal(proof.cleanupProven, false);
});

for (const mode of [0o644, 0o400, 0o660]) {
  test(`credential file mode ${mode.toString(8)} is rejected before stdin forwarding`, async (t) => {
    const paths = await hostFixture(t);
    await chmod(paths.credential, mode);
    const result = invoke(paths);
    assert.equal(result.status, 1);
    assert.equal(JSON.parse(result.stdout).code, "database-unavailable");
    assert.equal(result.output[3], "");
    assert.equal(result.stderr, "");
    assert.doesNotMatch(await readFile(paths.log, "utf8"), /exec --interactive|psql/u);
  });
}

test("host config symlinks are rejected without disclosing their contents", async (t) => {
  const paths = await hostFixture(t);
  const link = join(paths.directory, "linked-config");
  await symlink(paths.config, link);
  paths.config = link;
  const result = invoke(paths);
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).code, "database-unavailable");
  assert.equal(result.stderr, "");
  assert.equal(result.output[3], "");
});

test("the probe refuses a server DSN or arbitrary role in the credential file", async (t) => {
  const paths = await hostFixture(t);
  await writeFile(paths.credential, `127.0.0.1:5432:telegram:owner:${privateCanary}\n`);
  const result = invoke(paths);
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).code, "database-unavailable");
  assert.doesNotMatch(await readFile(paths.log, "utf8"), /exec --interactive|psql/u);
});

test("candidate runner evidence requires a root-style owner-only directory and file", async (t) => {
  const cases = [
    ["directory mode", async (paths) => chmod(paths.buildDirectory, 0o755)],
    ["file mode", async (paths) => chmod(paths.buildIdentity, 0o644)],
    ["hard link", async (paths) => link(paths.buildIdentity, join(paths.directory, "linked-identity"))],
    ["symlink", async (paths) => {
      const target = join(paths.directory, "identity-target");
      await writeFile(target, await readFile(paths.buildIdentity));
      await unlink(paths.buildIdentity);
      await symlink(target, paths.buildIdentity);
    }],
    ["non-regular file", async (paths) => {
      await unlink(paths.buildIdentity);
      await mkdir(paths.buildIdentity);
    }],
    ["non-directory candidate", async (paths) => {
      await rm(paths.buildDirectory, { recursive: true });
      await writeFile(paths.buildDirectory, "not a directory");
    }],
    ["symlink candidate directory", async (paths) => {
      const target = join(paths.evidenceRoot, "hidden-build");
      await mkdir(target, { mode: 0o700 });
      await writeFile(join(target, "built-image.identity"), await readFile(paths.buildIdentity), { mode: 0o600 });
      await rm(paths.buildDirectory, { recursive: true });
      await symlink(target, paths.buildDirectory, "dir");
    }],
    ["oversized record", async (paths) => writeFile(paths.buildIdentity, `${await readFile(paths.buildIdentity, "utf8")}${" ".repeat(1025)}`)],
    ["wrong key order", async (paths) => writeFile(paths.buildIdentity,
      `source_sha=${serverRevision} captured_at_utc=2026-10-13T00:00:00Z image_id=sha256:${"a".repeat(64)} revision_label_json="${serverRevision}"`)],
    ["multiple trailing newlines", async (paths) => writeFile(paths.buildIdentity, `${await readFile(paths.buildIdentity, "utf8")}\n`)],
    ["extra key", async (paths) => writeFile(paths.buildIdentity, `${await readFile(paths.buildIdentity, "utf8")} extra=value`)],
  ];
  for (const [name, corrupt] of cases) {
    const paths = await hostFixture(t);
    await corrupt(paths);
    const result = invoke(paths);
    assert.equal(result.status, 1, name);
    assert.equal(JSON.parse(result.stdout).code, "revision-unproven", name);
    assert.equal(result.output[3], "", name);
    assert.doesNotMatch(await readFile(paths.log, "utf8"), /exec --interactive|psql/u, name);
  }
});

test("the host probe has no CLI evidence-root override", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, {}, ["--evidence-root", paths.evidenceRoot]);
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).code, "arguments-invalid");
  assert.equal(result.output[3], "");
  await assert.rejects(readFile(paths.log));
});

test("a production environment cannot redirect runner evidence away from /root", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, { ROLLOUT_RUNNER_TEST_MODE: "0" });
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).code, "revision-unproven");
  assert.equal(result.output[3], "");
  assert.doesNotMatch(await readFile(paths.log, "utf8"), /exec --interactive|psql/u);
});

test("database stderr and connection errors never leave probe memory", async (t) => {
  const paths = await hostFixture(t);
  await writeFile(join(paths.directory, "db-error"), "");
  const result = invoke(paths);
  assert.equal(result.status, 1);
  assert.equal(JSON.parse(result.stdout).code, "database-unavailable");
  assert.equal(result.stderr, "");
  assert.doesNotMatch(result.stdout, /private-probe/u);
});

test("a non-zero browser exit cannot turn proven cleanup into a pass", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, { HOST_BROWSER_STATUS: "1" });
  assert.equal(result.status, 1);
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.ok, false);
  assert.equal(proof.code, "browser-failed");
  assert.equal(proof.browserExitOk, false);
  assert.equal(proof.cleanupProven, true);
});

test("the real wrapper ignores runner test evidence overrides before credential forwarding", async (t) => {
  const paths = await hostFixture(t);
  const result = spawnSync("bash", [join(repoRoot, "deploy/browser-acceptance/run.sh"), "acceptance",
    "--manifest", paths.manifest, "--script", paths.script,
    "--probe-credential", paths.credential, "--probe-config", paths.config], {
    cwd: repoRoot, encoding: "utf8", input: "credential-sentinel\n", timeout: 10_000,
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, XDG_RUNTIME_DIR: paths.directory,
      HOST_COMMAND_LOG: paths.log, HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog,
      HOST_READ_STDIN: "1", ROLLOUT_RUNNER_TEST_MODE: "1",
      ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT: paths.evidenceRoot },
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"revision-unproven"}\n');
  assert.equal(result.stderr, "");
  const commands = await readFile(paths.log, "utf8");
  assert.match(commands, /down --rmi local/u);
  assert.doesNotMatch(commands + result.stdout, /private-probe|987654321098765432|credential-sentinel/u);
  assert.doesNotMatch(commands, /exec --interactive|psql/u);
});

test("oversized observer output fails closed before credential forwarding", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, { HOST_OBSERVER_OVERSIZE: "1" });
  assert.equal(result.status, 1);
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.code, "network-unproven");
  assert.equal(proof.networkIsolated, false);
  const commands = await readFile(paths.log, "utf8");
  assert.match(commands, /observer-snapshot\.mjs/u);
  assert.doesNotMatch(commands, /exec --interactive/u);
  assert.equal(commands.split("\n").filter((line) => line.startsWith("psql ")).length, 1);
});

test("the real wrapper joins the real v2 proof and browser pipe and tears down the project", async (t) => {
  const paths = await hostFixture(t);
  // run.sh strips the runner test gate, so only this fixture shim can restore it for the probe.
  await writeFile(join(paths.bin, "node"), `#!/usr/bin/env bash
set -eu
if [[ "\${1:-}" == */host-probe.mjs ]]; then
  [[ ! -v ROLLOUT_RUNNER_TEST_MODE && ! -v ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT ]] || exit 96
  export ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT='${paths.evidenceRoot}'
fi
exec '${process.execPath}' "$@"
`, { mode: 0o755 });
  const result = spawnSync("bash", [join(repoRoot, "deploy/browser-acceptance/run.sh"), "acceptance",
    "--manifest", paths.manifest, "--script", paths.script,
    "--probe-credential", paths.credential, "--probe-config", paths.config], {
    cwd: repoRoot, encoding: "utf8", input: "credential-sentinel\n", timeout: 10_000,
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, XDG_RUNTIME_DIR: paths.directory,
      HOST_COMMAND_LOG: paths.log, HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog,
      HOST_READ_STDIN: "1", ROLLOUT_RUNNER_TEST_MODE: "1",
      ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT: paths.evidenceRoot },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(JSON.parse(result.stdout).ok, true);
  assert.equal(result.stdout.split("\n").length, 2);
  assert.equal(result.stderr, "");
  const commands = await readFile(paths.log, "utf8");
  assert.match(commands, /exec --interactive/u);
  assert.match(commands, /down --rmi local/u);
  assert.doesNotMatch(commands + result.stdout, /private-probe|987654321098765432|credential-sentinel/u);
});

async function invokeRealWrapper(paths, mode, extraEnv = {}) {
  // run.sh strips the runner test gate, so only this fixture shim can restore it for the probe.
  await writeFile(join(paths.bin, "node"), `#!/usr/bin/env bash
set -eu
if [[ "\${1:-}" == */host-probe.mjs ]]; then
  [[ ! -v ROLLOUT_RUNNER_TEST_MODE && ! -v ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT ]] || exit 96
  export ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT='${paths.evidenceRoot}'
fi
exec '${process.execPath}' "$@"
`, { mode: 0o755 });
  return spawnSync("bash", [join(repoRoot, "deploy/browser-acceptance/run.sh"), mode,
    "--manifest", paths.manifest, "--script", paths.script,
    "--probe-credential", paths.credential, "--probe-config", paths.config], {
    cwd: repoRoot, encoding: "utf8", input: "credential-sentinel\n", timeout: 10_000,
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, XDG_RUNTIME_DIR: paths.directory,
      HOST_COMMAND_LOG: paths.log, HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog,
      ...extraEnv },
  });
}

test("the fake QA preflight fails when any stdin byte reaches it", async (t) => {
  const paths = await hostFixture(t);
  const result = spawnSync(join(paths.bin, "docker"), ["exec", ids.browser, "node", "/opt/browser-acceptance/approved-qa.mjs", "--mode",
    "preflight", "--release-record", "/run/release-record.json", "--runtime-adapter",
    "/opt/browser-acceptance/qa-runtime-adapter.mjs"], {
    encoding: "utf8", input: "credential-sentinel\n", env: { ...process.env, HOST_COMMAND_LOG: paths.log },
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "browser acceptance preflight failed\n");
});

test("probe preflight runs the QA preflight without stdin after one binding cut", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, {}, [], "credential-sentinel\n", "preflight");
  assert.equal(result.status, 0, `${result.stdout}\n${await readFile(paths.log, "utf8")}`);
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.ok, true);
  assert.equal(proof.code, "ok");
  assert.equal(proof.serverSourceCommit, serverRevision);
  assert.equal(proof.newBindingProven, false);
  assert.equal(proof.cleanupProven, false);
  assert.equal(result.output[3], "browser acceptance preflight ready\n");
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(join(paths.directory, "preflight-stdin-touched")));
  const commands = await readFile(paths.log, "utf8");
  assert.doesNotMatch(commands, /exec --interactive|--mode acceptance/u);
  assert.match(commands, new RegExp(`exec ${ids.browser} node /opt/browser-acceptance/approved-qa\\.mjs --mode preflight`, "u"));
  const queries = await readFile(paths.queryLog, "utf8");
  assert.equal((queries.match(/public\.browser_acceptance_binding_snapshot_v1/g) ?? []).length, 1);
});

test("the probe rejects a missing or unknown mode", async (t) => {
  for (const mode of ["", "readiness"]) {
    const paths = await hostFixture(t);
    const result = invoke(paths, {}, [], "", mode);
    assert.equal(result.status, 1);
    assert.equal(JSON.parse(result.stdout).code, "arguments-invalid");
    await assert.rejects(readFile(paths.log));
  }
});

test("the real wrapper preflight prints one fixed ready line with closed stdin and no interactive exec", async (t) => {
  const paths = await hostFixture(t);
  const result = await invokeRealWrapper(paths, "acceptance-preflight");
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(result.stdout, `{"status":"acceptance-ready","web_source_commit":"${webRevision}","server_source_commit":"${serverRevision}",` +
    '"registration_closed":true,"login_codes_disabled":true,"server_revision_proven":true,"runtime_stable":true,' +
    '"network_isolated":true,"fingerprint_matched":true,"binding_evidence_complete":true,"qa_preflight_ready":true}\n');
  assert.equal(result.stderr, "");
  await assert.rejects(readFile(join(paths.directory, "preflight-stdin-touched")));
  const commands = await readFile(paths.log, "utf8");
  assert.doesNotMatch(commands, /exec --interactive|--mode acceptance/u);
  assert.match(commands, /--mode preflight/u);
  assert.match(commands, /down --rmi local/u);
  assert.equal(commands.split("\n").filter((line) => line.startsWith("psql ")).length, 1);
  assert.doesNotMatch(commands + result.stdout, /private-probe|987654321098765432|credential-sentinel/u);
});

test("the real wrapper preflight maps a failed QA preflight to a fixed error", async (t) => {
  const paths = await hostFixture(t);
  const result = await invokeRealWrapper(paths, "acceptance-preflight", { HOST_BROWSER_STATUS: "1" });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"browser-failed"}\n');
  assert.equal(result.stderr, "");
});

test("the real wrapper preflight stops on probe-credential checks before the QA process", async (t) => {
  const paths = await hostFixture(t);
  await chmod(paths.credential, 0o644);
  const result = await invokeRealWrapper(paths, "acceptance-preflight");
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"database-unavailable"}\n');
  assert.doesNotMatch(await readFile(paths.log, "utf8"), /approved-qa\.mjs --mode|psql/u);
});

test("a preflight success does not let acceptance skip its own checks", async (t) => {
  const paths = await hostFixture(t);
  const ready = await invokeRealWrapper(paths, "acceptance-preflight");
  assert.equal(ready.status, 0, ready.stdout);
  await writeFile(join(paths.directory, "db-read-count"), "0\n");
  const changed = await readFile(paths.buildIdentity, "utf8");
  await writeFile(paths.buildIdentity, changed.replaceAll(serverRevision, "9".repeat(40)));
  const result = await invokeRealWrapper(paths, "acceptance", { HOST_READ_STDIN: "1" });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, '{"status":"error","code":"revision-unproven"}\n');
  const commands = await readFile(paths.log, "utf8");
  assert.doesNotMatch(commands, /exec --interactive/u);
});
