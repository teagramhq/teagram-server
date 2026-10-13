import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmod, mkdtemp, mkdir, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";

const repoRoot = new URL("../..", import.meta.url).pathname;
const probePath = join(repoRoot, "deploy/browser-acceptance/host-probe.mjs");
const revision = "c".repeat(40);
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
  await mkdir(bin);
  await writeFile(credential, `127.0.0.1:5432:telegram:browser_acceptance_probe:${privateCanary}\n`, { mode: 0o600 });
  await writeFile(config, '{"userId":"987654321098765432"}', { mode: 0o600 });
  await writeFile(manifest, JSON.stringify({
    sourceCommit: revision, archiveSha256: `sha256:${"a".repeat(64)}`, contentDigest: `sha256:${"a".repeat(64)}`,
    url: `https://telegram-server.tailaa4918.ts.net/client/?v=${revision}`,
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
      printf '%s\\n' '{"id":"${ids.server}","imageId":"sha256:${"a".repeat(64)}","revision":"${revision}","startedAt":"2026-10-13T00:00:00Z","running":true,"flags":["TG_REGISTRATION=closed","TG_LOG_LOGIN_CODES=false","TG_RSA_KEY_FINGERPRINT=0123456789abcdef",null]}'
    else
      printf '%s\\n' '{"id":"${ids.server}","path":"/usr/local/bin/telegramd","service":"telegramd"}'
    fi
    exit 0 ;;
  'image inspect') printf '%s\\n' '{"id":"sha256:${"a".repeat(64)}","revision":"${revision}"}'; exit 0 ;;
  'network inspect') printf '%s\\n' '{"internal":true,"members":["${ids.browser}","${ids.observer}",null]}'; exit 0 ;;
  'exec --interactive')
    [[ "$*" == *'node /run/approved-qa.mjs --mode acceptance'* ]] || exit 95
    if [[ "\${HOST_READ_STDIN:-0}" == 1 ]]; then IFS= read -r input; [[ "$input" == credential-sentinel ]] || exit 94; fi
    printf '%s\\n' '{"schemaVersion":2,"ok":true,"phase":"signed-in","outcome":"ok","logout":"ok","targetHost":"telegram-server.tailaa4918.ts.net","sourceCommit":"${revision}","wssConnections":1}'
    exit "\${HOST_BROWSER_STATUS:-0}" ;;
esac
if [[ "$1" == compose ]]; then
  if [[ "$*" == *version* || "$*" == *' build '* || "$*" == *' up --detach --wait observer'* || "$*" == *' down --rmi local'* ]]; then exit 0; fi
  if [[ "$*" == *'--file - up '* ]]; then
    input="$(cat)"
    [[ "$input" == *'/run/approved-qa.mjs'* && "$input" != *'private-probe'* && "$input" != *'987654321098765432'* ]] || exit 92
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
printf '%s\\n' '{"mode":"private","endpoint":"wss://telegram-server.tailaa4918.ts.net/apiws","fingerprint":"0123456789abcdef","sourceCommit":"${revision}","artifactDigest":"sha256:${"a".repeat(64)}"}'
`);
  await writeFile(join(bin, "df"), `#!/usr/bin/env bash
printf '%s\\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on' 'fake 5000000 0 3000000 0% ${directory}'
`);
  await writeFile(join(bin, "sha256sum"), `#!/usr/bin/env bash
printf '%s\\n' '2ed4107f76a4b2ed6bdbd9933b7009f62d4dba4b2ecb8349835a181fcda6b10c  approved-fixture'
`);
  await Promise.all(["docker", "psql", "curl", "df", "sha256sum"].map((name) => chmod(join(bin, name), 0o755)));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return { directory, bin, log, credential, config, manifest, script, dbReadCount, queryLog, twoEvidence };
}

function invoke(paths, extraEnv = {}, args = [], input = "credential-sentinel\n") {
  return spawnSync(process.execPath, [probePath, "--manifest", paths.manifest, "--script", paths.script,
    "--credential", paths.credential, "--config", paths.config, ...args], {
    cwd: repoRoot, input, encoding: "utf8", timeout: 10_000,
    stdio: ["pipe", "pipe", "pipe", "pipe"],
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, HOST_COMMAND_LOG: paths.log,
      HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog, ...extraEnv },
  });
}

test("host CLI emits one proof line and forwards browser stdout on a separate descriptor", async (t) => {
  const paths = await hostFixture(t);
  const result = invoke(paths, { HOST_READ_STDIN: "1" });
  assert.equal(result.status, 0, `${result.stderr}${result.stdout}\n${await readFile(paths.log, "utf8")}`);
  assert.match(result.stdout, /^\{"schemaVersion":1,/u, "host CLI must produce an independent proof");
  const proof = JSON.parse(result.stdout);
  assert.equal(proof.ok, true);
  assert.equal(proof.code, "ok");
  assert.equal(result.stdout.split("\n").length, 2);
  assert.equal(JSON.parse(result.output[3]).phase, "signed-in");
  assert.equal(result.stderr, "");
  const commands = await readFile(paths.log, "utf8");
  assert.doesNotMatch(`${result.stdout}${result.stderr}${result.output[3]}${commands}`, /private-probe|987654321098765432|credential-sentinel/u);
  assert.match(commands, /TG_REGISTRATION.*TG_LOG_LOGIN_CODES.*TG_RSA_KEY_FINGERPRINT/u);
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

test("the real wrapper joins independent proof and browser pipes and tears down the project", async (t) => {
  const paths = await hostFixture(t);
  const result = spawnSync("bash", [join(repoRoot, "deploy/browser-acceptance/run.sh"), "acceptance",
    "--manifest", paths.manifest, "--script", paths.script,
    "--probe-credential", paths.credential, "--probe-config", paths.config], {
    cwd: repoRoot, encoding: "utf8", input: "credential-sentinel\n", timeout: 10_000,
    env: { ...process.env, PATH: `${paths.bin}:${process.env.PATH}`, XDG_RUNTIME_DIR: paths.directory,
      HOST_COMMAND_LOG: paths.log, HOST_DB_READ_COUNT: paths.dbReadCount, HOST_QUERY_LOG: paths.queryLog,
      HOST_READ_STDIN: "1" },
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.equal(JSON.parse(result.stdout).ok, true);
  assert.equal(result.stdout.split("\n").length, 2);
  assert.equal(result.stderr, "");
  const commands = await readFile(paths.log, "utf8");
  assert.match(commands, /down --rmi local/u);
  assert.doesNotMatch(commands + result.stdout, /private-probe|987654321098765432|credential-sentinel/u);
});
