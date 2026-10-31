#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

[ "$(id -u)" = 0 ] || { printf '%s\n' 'run this fixture as root to exercise the privilege drop' >&2; exit 1; }

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../.." && pwd)
SOURCE_SCRIPT="$SCRIPT_DIR/ops/preflight-replay.sh"
SCRIPT=
readonly REPO_ROOT
readonly SOURCE_SCRIPT
readonly LEGACY_RUNNER_SHA=082083851cc458ce3e26fae7c2fdaaea935e0630
readonly LEGACY_RUNNER_PATH=deploy/teagramd/rollout-runner/rollout-runner.sh
REAL_RM=$(command -v rm)
REAL_MKTEMP=$(command -v mktemp)
readonly TMP_PREFIX=/tmp/teagram-preflight-replay-test.
TMP=
TMP_VALIDATED=0
NETWORK_PROBE_PID=

fixture_tmp_is_direct_child() {
  local path=$1 suffix
  case "$path" in
    "$TMP_PREFIX"*) suffix=${path#"$TMP_PREFIX"} ;;
    *) return 1 ;;
  esac
  [[ "$suffix" =~ ^[A-Za-z0-9]{10}$ ]] || return 1
  [ ! -L "$path" ] && [ "$(readlink -f -- "$path" 2>/dev/null)" = "$path" ]
}

cleanup() {
  local rc=$?
  trap - EXIT
  if [ -n "$NETWORK_PROBE_PID" ]; then
    kill "$NETWORK_PROBE_PID" 2>/dev/null || :
    wait "$NETWORK_PROBE_PID" 2>/dev/null || :
  fi
  if [ -n "$TMP" ]; then
    if [ "$TMP_VALIDATED" = 1 ] && fixture_tmp_is_direct_child "$TMP"; then
      "$REAL_RM" -rf -- "$TMP" || rc=1
    else
      printf '%s\n' 'fixture cleanup refused an unvalidated or unsafe temporary path' >&2
      rc=1
    fi
  fi
  exit "$rc"
}

if ! TMP=$("$REAL_MKTEMP" -d "$TMP_PREFIX"XXXXXXXXXX); then
  printf '%s\n' 'cannot create preflight replay fixture directory' >&2
  exit 1
fi
trap cleanup EXIT
fixture_tmp_is_direct_child "$TMP" && [ -d "$TMP" ] && \
  [ "$(stat -c %u:%a -- "$TMP")" = '0:700' ] || {
  printf '%s\n' 'fixture temporary directory is outside its private prefix' >&2
  exit 1
}
TMP_VALIDATED=1
if [ "${FIXTURE_TEST_ROOT_TMP_ONLY:-0}" = 1 ]; then
  exit 0
fi

SCRIPT="$TMP/preflight-replay.sh"
cp -p "$SOURCE_SCRIPT" "$SCRIPT"
if git -C "$TMP" rev-parse --show-toplevel >/dev/null 2>&1; then
  printf '%s\n' 'FAIL replay fixture copy is inside a Git worktree' >&2
  exit 1
fi

chmod 700 "$TMP"
chmod 755 "$TMP"
mkdir -m 777 "$TMP/host-exposure"

cat > "$TMP/network-probe-listener.py" <<'PYTHON'
import socket
import sys

port_file, connection_file = sys.argv[1:]
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen(4)
with open(port_file, "w", encoding="ascii") as output:
    output.write(f"{listener.getsockname()[1]}\n")
while True:
    connection, _ = listener.accept()
    connection.close()
    with open(connection_file, "a", encoding="ascii") as output:
        output.write("connected\n")
PYTHON
python3 "$TMP/network-probe-listener.py" "$TMP/network-probe.port" \
  "$TMP/network-probe-connections.log" &
NETWORK_PROBE_PID=$!
for ((attempt = 1; attempt <= 50; attempt++)); do
  [ -s "$TMP/network-probe.port" ] && break
  kill -0 "$NETWORK_PROBE_PID" 2>/dev/null || {
    printf '%s\n' 'host network probe did not start' >&2
    exit 1
  }
  sleep 0.1
done
NETWORK_PROBE_PORT=$(cat "$TMP/network-probe.port")
[[ "$NETWORK_PROBE_PORT" =~ ^[0-9]+$ ]] || {
  printf '%s\n' 'host network probe returned an invalid port' >&2
  exit 1
}

export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

mkdir -m 700 "$TMP/bin" "$TMP/source"
mkdir -p "$TMP/source/deploy/teagramd/rollout-runner"
cp -p "$SCRIPT_DIR/../teagramd/rollout-runner/rollout-runner.sh" \
  "$TMP/source/deploy/teagramd/rollout-runner/rollout-runner.sh"
cp -p "$SCRIPT_DIR/../teagramd/rollout-runner/rollout-verifier.sh" \
  "$TMP/source/deploy/teagramd/rollout-runner/rollout-verifier.sh"
git init -q --bare "$TMP/origin.git"
git init -q -b main "$TMP/source"
git -C "$TMP/source" remote add origin "$TMP/origin.git"
cat > "$TMP/source/docker-compose.fixture.yml" <<'COMPOSE'
fixture: primary
COMPOSE
cat > "$TMP/source/docker-compose.alternate.yml" <<'COMPOSE'
fixture: alternate
COMPOSE
git -C "$TMP/source" add docker-compose.fixture.yml docker-compose.alternate.yml deploy/teagramd/rollout-runner
git -C "$TMP/source" commit -q -m baseline
git -C "$TMP/source" push -q -u origin main
git --git-dir="$TMP/origin.git" symbolic-ref HEAD refs/heads/main
pass_sha=$(git -C "$TMP/source" rev-parse HEAD)
if git -c safe.directory="$REPO_ROOT" -C "$REPO_ROOT" \
   cat-file -e "$LEGACY_RUNNER_SHA:$LEGACY_RUNNER_PATH" 2>/dev/null; then
  git -c safe.directory="$REPO_ROOT" -C "$REPO_ROOT" \
    show "$LEGACY_RUNNER_SHA:$LEGACY_RUNNER_PATH" > "$TMP/source/$LEGACY_RUNNER_PATH"
else
  git -C "$TMP/source" fetch --quiet --depth=1 \
    https://github.com/teagramhq/teagram-server.git "$LEGACY_RUNNER_SHA"
  git -C "$TMP/source" show "$LEGACY_RUNNER_SHA:$LEGACY_RUNNER_PATH" \
    > "$TMP/source/$LEGACY_RUNNER_PATH"
fi
chmod 755 "$TMP/source/$LEGACY_RUNNER_PATH"
if grep -Fq 'compare_resolved_preflight_command' "$TMP/source/$LEGACY_RUNNER_PATH"; then
  printf '%s\n' 'FAIL legacy runner fixture unexpectedly has the replay command' >&2
  exit 1
fi
git -C "$TMP/source" add "$LEGACY_RUNNER_PATH"
git -C "$TMP/source" commit -q -m legacy-runner
git -C "$TMP/source" push -q origin HEAD:legacy-runner
legacy_runner_sha=$(git -C "$TMP/source" rev-parse HEAD)
git -C "$TMP/source" reset -q --hard "$pass_sha"
git -C "$TMP/source" switch -q -c missing-compose
git -C "$TMP/source" rm -q docker-compose.alternate.yml
git -C "$TMP/source" commit -q -m missing-selected-compose
git -C "$TMP/source" push -q origin missing-compose
missing_compose_sha=$(git -C "$TMP/source" rev-parse HEAD)
git -C "$TMP/source" switch -q main
git -C "$TMP/source" switch -q -c drift
cat > "$TMP/source/docker-compose.alternate.yml" <<'COMPOSE'
fixture: alternate
# FIXTURE_DRIFT
COMPOSE
git -C "$TMP/source" add docker-compose.alternate.yml
git -C "$TMP/source" commit -q -m drift
git -C "$TMP/source" push -q origin drift
drift_sha=$(git -C "$TMP/source" rev-parse HEAD)

git -C "$TMP/source" switch -q main
test_tmp_name=${TMP##*/}
for kind in runner verifier; do
  if [ "$kind" = runner ]; then
    source_script="$TMP/source/deploy/teagramd/rollout-runner/rollout-runner.sh"
  else
    source_script="$TMP/source/deploy/teagramd/rollout-runner/rollout-verifier.sh"
  fi
  cat > "$TMP/$kind-attack-prefix.sh" <<PREFIX
if [ "\${ROLLOUT_PREFLIGHT_REPLAY_SANDBOX:-0}" = 1 ]; then
  printf '%s\\n' "\$(id -u)" > "\$PREFLIGHT_REPLAY_SANDBOX_EVIDENCE_DIR/$kind.uid"
  mkdir -p "/tmp/$test_tmp_name/host-exposure"
  printf '%s\\n' attacked > "/tmp/$test_tmp_name/host-exposure/sentinel"
  if (exec 9<>/dev/tcp/127.0.0.1/$NETWORK_PROBE_PORT) 2>/dev/null; then
    printf '%s\\n' exposed > "\$PREFLIGHT_REPLAY_SANDBOX_EVIDENCE_DIR/$kind.network"
    exec 9<&- 9>&-
  else
    printf '%s\\n' blocked > "\$PREFLIGHT_REPLAY_SANDBOX_EVIDENCE_DIR/$kind.network"
  fi
fi
PREFIX
  {
    sed -n '1,2p' "$source_script"
    cat "$TMP/$kind-attack-prefix.sh"
    sed -n '3,$p' "$source_script"
  } > "$source_script.attacked"
  chmod --reference="$source_script" "$source_script.attacked"
  mv -- "$source_script.attacked" "$source_script"
done
git -C "$TMP/source" add deploy/teagramd/rollout-runner/rollout-runner.sh \
  deploy/teagramd/rollout-runner/rollout-verifier.sh
git -C "$TMP/source" commit -q -m sandbox-attack-fixture
git -C "$TMP/source" push -q origin HEAD:sandbox-attack
sandbox_attack_sha=$(git -C "$TMP/source" rev-parse HEAD)

git clone -q "$TMP/origin.git" "$TMP/live"
git -C "$TMP/live" switch -q main
printf 'fixture-password=never-print\n' > "$TMP/live/.env"
printf '# fixture override\n' > "$TMP/live/docker-compose.override.yml"
mkdir -m 700 "$TMP/live/.secrets"
: > "$TMP/live/.secrets/teagramd-turnstile"
chmod 444 "$TMP/live/.secrets/teagramd-turnstile"
chmod 600 "$TMP/live/.env" "$TMP/live/docker-compose.override.yml"

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "${1:-}" == inspect ]]; then
  [[ $# == 2 && "$2" == teagram-teagramd-1 || $# == 2 && "$2" == aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ]] || {
    printf '%s\n' 'unexpected fixture inspect request' >&2
    exit 90
  }
  selection=
  IFS=, read -r -a selected <<< "${FIXTURE_LABEL_FILES:?}"
  for entry in "${selected[@]}"; do
    if [[ -n "$selection" ]]; then selection+=,; fi
    selection+="${FIXTURE_LIVE_ROOT:?}/$entry"
  done
  inspect_env='["TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_LOG_LOGIN_CODES=false"]'
  inspect_mounts='[]'
  if [[ "${FIXTURE_TURNSTILE:-0}" == 1 ]]; then
    inspect_env=$(jq -cn '[
      "TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_LOG_LOGIN_CODES=false",
      "TG_TURNSTILE_SITE_KEY=fixture-site-key",
      "TG_TURNSTILE_SECRET_KEY_FILE=/run/secrets/teagramd-turnstile",
      "TG_TURNSTILE_SIGNIN_ENABLED=false"
    ]')
    inspect_mounts=$(jq -cn --arg source "${FIXTURE_TURNSTILE_SECRET_SOURCE:?}" '[{
      Type:"bind",Source:$source,Destination:"/run/secrets/teagramd-turnstile",RW:false
    }]')
  fi
  jq -cn --arg selection "$selection" --argjson inspect_env "$inspect_env" --argjson inspect_mounts "$inspect_mounts" '[{
    Id:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    Image:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    Config:{
      Env:$inspect_env,
      StopTimeout:120,
      Labels:{"com.docker.compose.project":"teagram","com.docker.compose.project.config_files":$selection}
    },
    State:{Status:"running",ExitCode:0,StartedAt:"2026-10-16T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z"},
    HostConfig:{LogConfig:{Type:"syslog",Config:{
      "syslog-address":"unixgram:///dev/log","syslog-facility":"daemon",
      "syslog-format":"rfc5424",tag:"teagramd"
    }},PortBindings:{}},
    Mounts:$inspect_mounts
  }]'
  exit 0
fi

if [[ "${1:-}" == compose ]]; then
  shift
  project_dir=
  actual_selection=
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --project-directory) project_dir=$2; shift 2 ;;
      --project-name) shift 2 ;;
      --file)
        file=$2
        if [[ -n "$actual_selection" ]]; then actual_selection+=,; fi
        actual_selection+="${file##*/}"
        shift 2
        ;;
      *) shift ;;
    esac
  done
  case "$project_dir" in
    */baseline|*/target) expected_cache="${project_dir%/*}/compose-cache" ;;
    *)
      printf '%s\n' 'fixture Compose project directory is invalid' >&2
      exit 93
      ;;
  esac
  [[ "${COMPOSE_EXPERIMENTAL_GIT_REMOTE:-}" == false && \
     "${COMPOSE_EXPERIMENTAL_OCI_REMOTE:-}" == false && \
     "${XDG_CACHE_HOME:-}" == "$expected_cache" ]] || {
    printf '%s\n' 'Compose remote loaders or evidence cache are not isolated' >&2
    exit 94
  }
  [[ "$actual_selection" == "${FIXTURE_EXPECTED_FILES:?}" ]] || {
    printf '%s\n' 'fixture Compose selection did not follow the live label' >&2
    exit 91
  }
  grace=2m0s
  if grep -q FIXTURE_DRIFT "$project_dir/docker-compose.alternate.yml" 2>/dev/null; then grace=3m0s; fi
  if [[ "${FIXTURE_TURNSTILE:-0}" == 1 ]]; then
    jq -cn --arg grace "$grace" --arg secret "${FIXTURE_TURNSTILE_SECRET_SOURCE:?}" '{
      services:{teagramd:{
        stop_grace_period:$grace,
        ports:[],volumes:[],network_mode:"",networks:{},
        logging:{driver:"syslog",options:{
          "syslog-address":"unixgram:///dev/log","syslog-facility":"daemon",
          "syslog-format":"rfc5424",tag:"teagramd"
        }},
        environment:{
          TG_REPLICA_COUNT:"1",TG_CLIENT_ADDR_TRUST:"socket",TG_LOG_LOGIN_CODES:"false",
          TG_TURNSTILE_SITE_KEY:"fixture-site-key",
          TG_TURNSTILE_SECRET_KEY_FILE:"/run/secrets/teagramd-turnstile",
          TG_TURNSTILE_SIGNIN_ENABLED:"false"
        },
        secrets:[{source:"teagramd_turnstile",target:"teagramd-turnstile"}]
      }},
      secrets:{teagramd_turnstile:{file:$secret}}
    }'
  else
    jq -cn --arg grace "$grace" '{services:{teagramd:{
      stop_grace_period:$grace,
      ports:[],volumes:[],network_mode:"",networks:{},
      logging:{driver:"syslog",options:{
        "syslog-address":"unixgram:///dev/log","syslog-facility":"daemon",
        "syslog-format":"rfc5424",tag:"teagramd"
      }},
      environment:{TG_REPLICA_COUNT:"1",TG_CLIENT_ADDR_TRUST:"socket",TG_LOG_LOGIN_CODES:"false"}
    }}}'
  fi
  exit 0
fi

printf '%s\n' 'unexpected fixture docker request' >&2
exit 92
DOCKER
chmod 700 "$TMP/bin/docker"

cat > "$TMP/bin/rm" <<'RM'
#!/usr/bin/env bash
set -Eeuo pipefail
path=${!#}
case "$path" in
  "$FIXTURE_TEST_ROOT"|"$FIXTURE_TEST_ROOT"/*|/tmp/teagram-preflight-replay.[A-Za-z0-9]*|/tmp/teagram-preflight-replay.[A-Za-z0-9]*/*)
    if [[ "$path" == /tmp/teagram-preflight-replay.[A-Za-z0-9]* ]] && [ -d "$path/sandbox/tmp/replay-output" ]; then
      for kind in verifier runner; do
        marker="$path/sandbox/tmp/replay-output/$kind.uid"
        if [ -f "$marker" ] && [ ! -L "$marker" ]; then
          printf '%s\t' "$kind" >> "${FIXTURE_SANDBOX_UID_LOG:?}"
          cat -- "$marker" >> "${FIXTURE_SANDBOX_UID_LOG:?}"
        fi
        network_marker="$path/sandbox/tmp/replay-output/$kind.network"
        if [ -f "$network_marker" ] && [ ! -L "$network_marker" ]; then
          printf '%s\t' "$kind" >> "${FIXTURE_SANDBOX_NETWORK_LOG:?}"
          cat -- "$network_marker" >> "${FIXTURE_SANDBOX_NETWORK_LOG:?}"
        fi
      done
    fi
    printf '%s\n' "$path" >> "${FIXTURE_REMOVE_LOG:?}"
    exec "$REAL_RM" "$@"
    ;;
  *)
    printf '%s\n' "$path" >> "${FIXTURE_UNSAFE_REMOVE_LOG:?}"
    exit 93
    ;;
esac
RM
chmod 700 "$TMP/bin/rm"

cat > "$TMP/bin/mktemp" <<'MKTEMP'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${FIXTURE_BAD_ROOT_MKTEMP:-0}" == 1 ]]; then
  printf '%s\n' "${FIXTURE_BAD_ROOT_PATH:?}"
  exit 0
fi
if [[ "${FIXTURE_BAD_MKTEMP:-0}" == 1 ]]; then
  printf '%s\n' /tmp/outside-preflight-replay
  exit 0
fi
exec "$REAL_MKTEMP" "$@"
MKTEMP
chmod 700 "$TMP/bin/mktemp"

before_host_exposure_mode=$(stat -c %a -- "$TMP/host-exposure")
: > "$TMP/unsafe-root-removals.log"
if FIXTURE_BAD_ROOT_MKTEMP=1 \
   FIXTURE_BAD_ROOT_PATH="$TMP/host-exposure" \
   FIXTURE_TEST_ROOT_TMP_ONLY=1 \
   FIXTURE_TEST_ROOT=/tmp/preflight-replay-no-match \
   FIXTURE_REMOVE_LOG="$TMP/removals.log" \
   FIXTURE_UNSAFE_REMOVE_LOG="$TMP/unsafe-root-removals.log" \
   PATH="$TMP/bin:$PATH" bash "${BASH_SOURCE[0]}" > "$TMP/unsafe-root-mktmp.log" 2>&1; then
  printf '%s\n' 'FAIL root fixture accepted an unsafe mktemp path' >&2
  exit 1
fi
grep -Fq 'fixture temporary directory is outside its private prefix' "$TMP/unsafe-root-mktmp.log" && \
  grep -Fq 'fixture cleanup refused an unvalidated or unsafe temporary path' "$TMP/unsafe-root-mktmp.log" && \
  [ "$(stat -c %a -- "$TMP/host-exposure")" = "$before_host_exposure_mode" ] && \
  [ ! -s "$TMP/unsafe-root-removals.log" ] || {
  printf '%s\n' 'FAIL root fixture used or removed an unsafe mktemp path' >&2
  cat "$TMP/unsafe-root-mktmp.log" >&2
  exit 1
}
printf '%s\n' 'PASS root fixture rejects unsafe mktemp paths before use and cleanup'

export PATH="$TMP/bin:$PATH"
export REAL_RM REAL_MKTEMP
export FIXTURE_SYSTEM_MKTEMP="$REAL_MKTEMP"
export FIXTURE_LIVE_ROOT="$TMP/live"
export FIXTURE_TURNSTILE_SECRET_SOURCE="$TMP/live/.secrets/teagramd-turnstile"
export FIXTURE_TEST_ROOT="$TMP"
export FIXTURE_REMOVE_LOG="$TMP/removals.log"
export FIXTURE_UNSAFE_REMOVE_LOG="$TMP/unsafe-removals.log"
export FIXTURE_SANDBOX_UID_LOG="$TMP/sandbox-uids.log"
export FIXTURE_SANDBOX_NETWORK_LOG="$TMP/sandbox-network.log"
export FIXTURE_HOST_SENTINEL="$TMP/host-exposure/sentinel"
export PREFLIGHT_REPLAY_TEST_MODE=1
export PREFLIGHT_REPLAY_TEST_LIVE_CHECKOUT="$TMP/live"
export PREFLIGHT_REPLAY_TEST_REPOSITORY_URL="$TMP/origin.git"
: > "$FIXTURE_REMOVE_LOG"
: > "$FIXTURE_UNSAFE_REMOVE_LOG"
: > "$FIXTURE_SANDBOX_UID_LOG"
: > "$FIXTURE_SANDBOX_NETWORK_LOG"
"$REAL_RM" -f -- "$FIXTURE_HOST_SENTINEL"

run_replay() {
  local sha=$1 selection=$2 output rc=0
  output=$(PREFLIGHT_REPLAY_TEST_MODE=1 \
      PREFLIGHT_REPLAY_TEST_LIVE_CHECKOUT="$TMP/live" \
      PREFLIGHT_REPLAY_TEST_REPOSITORY_URL="$TMP/origin.git" \
      FIXTURE_LIVE_ROOT="$TMP/live" FIXTURE_LABEL_FILES="$selection" \
      FIXTURE_EXPECTED_FILES="$selection" FIXTURE_REMOVE_LOG="$FIXTURE_REMOVE_LOG" \
      FIXTURE_TURNSTILE="${FIXTURE_TURNSTILE:-0}" \
      FIXTURE_TURNSTILE_SECRET_SOURCE="$FIXTURE_TURNSTILE_SECRET_SOURCE" \
      FIXTURE_UNSAFE_REMOVE_LOG="$FIXTURE_UNSAFE_REMOVE_LOG" REAL_RM="$REAL_RM" \
      FIXTURE_SANDBOX_UID_LOG="$FIXTURE_SANDBOX_UID_LOG" \
      FIXTURE_SANDBOX_NETWORK_LOG="$FIXTURE_SANDBOX_NETWORK_LOG" \
      FIXTURE_HOST_SENTINEL="$FIXTURE_HOST_SENTINEL" \
      REAL_MKTEMP="$REAL_MKTEMP" FIXTURE_BAD_MKTEMP="${FIXTURE_BAD_MKTEMP:-0}" \
      PATH="$PATH" bash "$SCRIPT" "$sha" 2>&1) || rc=$?
  printf '%s\n' "$rc" > "$TMP/last-rc"
  printf '%s' "$output"
}

output=$(run_replay "$pass_sha" 'docker-compose.fixture.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" == 0 ]] || { printf 'FAIL all-pass replay:\n%s\n' "$output" >&2; exit 1; }
[[ "$(wc -l <<< "$output" | tr -d ' ')" == 12 ]] || { printf 'FAIL all-pass row count:\n%s\n' "$output" >&2; exit 1; }
if grep -qv $'\tpass$' <<< "$output" || grep -Eq '2m0s|3m0s|fixture-password' <<< "$output"; then
  printf 'FAIL all-pass replay exposed values or failed a row:\n%s\n' "$output" >&2
  exit 1
fi
printf '%s\n' 'PASS all-pass replay prints only field/result rows'

output=$(FIXTURE_TURNSTILE=1 run_replay "$pass_sha" 'docker-compose.fixture.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" == 0 ]] || { printf 'FAIL configured Turnstile replay:\n%s\n' "$output" >&2; exit 1; }
if grep -qv $'\tpass$' <<< "$output" || grep -Eq 'fixture-site-key|fixture-password' <<< "$output"; then
  printf 'FAIL configured Turnstile replay failed or exposed values:\n%s\n' "$output" >&2
  exit 1
fi
printf '%s\n' 'PASS configured Turnstile replay uses the live secret path without exposing values'

# This authentic pre-replay runner from PR #870 uses the legacy snapshot paths.
# The Docker stub checks that the verifier follows the exact labeled file order.
output=$(run_replay "$legacy_runner_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" == 0 ]] || { printf 'FAIL legacy runner compatibility replay:\n%s\n' "$output" >&2; exit 1; }
printf '%s\n' 'PASS pre-replay runner compatibility follows the live selection'

output=$(run_replay "$missing_compose_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
if [[ "$(cat "$TMP/last-rc")" == 0 ]] || \
   ! grep -Fq 'PR head is missing a selected Compose file' <<< "$output"; then
  printf 'FAIL missing selected PR Compose file was not rejected:\n%s\n' "$output" >&2
  exit 1
fi
printf '%s\n' 'PASS missing selected PR Compose file is rejected'

output=$(run_replay "$drift_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
if [[ "$(cat "$TMP/last-rc")" == 0 ]] || ! grep -Fq $'stop_grace_period\tfail' <<< "$output"; then
  printf 'FAIL drifted replay was not rejected:\n%s\n' "$output" >&2
  exit 1
fi
if grep -Eq '2m0s|3m0s|fixture-password' <<< "$output"; then
  printf 'FAIL drifted replay exposed values:\n%s\n' "$output" >&2
  exit 1
fi
printf '%s\n' 'PASS drifted replay exits non-zero and redacts values'

: > "$FIXTURE_SANDBOX_UID_LOG"
: > "$FIXTURE_SANDBOX_NETWORK_LOG"
rm -f -- "$FIXTURE_HOST_SENTINEL"
output=$(run_replay "$sandbox_attack_sha" 'docker-compose.fixture.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" == 0 ]] || { printf 'FAIL sandbox attack replay:\n%s\n' "$output" >&2; exit 1; }
[[ "$(cat "$FIXTURE_SANDBOX_UID_LOG")" == $'verifier\t65534\nrunner\t65534' ]] || {
  printf 'FAIL PR verifier and runner did not both execute as uid 65534:\n%s\n' \
    "$(cat "$FIXTURE_SANDBOX_UID_LOG")" >&2
  exit 1
}
[[ "$(cat "$FIXTURE_SANDBOX_NETWORK_LOG")" == $'verifier\tblocked\nrunner\tblocked' ]] && \
  [ ! -s "$TMP/network-probe-connections.log" ] || {
  printf 'FAIL PR code reached the host network canary:\n%s\n' \
    "$(cat "$FIXTURE_SANDBOX_NETWORK_LOG")" >&2
  exit 1
}
[ ! -e "$FIXTURE_HOST_SENTINEL" ] || {
  printf '%s\n' 'FAIL PR code wrote through the sandbox to a host path' >&2
  exit 1
}
printf '%s\n' 'PASS PR verifier and runner are unprivileged and cannot write to host paths'
printf '%s\n' 'PASS PR verifier and runner cannot reach a host network service'

bad_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
output=$(run_replay "$bad_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" != 0 ]] || { printf '%s\n' 'FAIL nonexistent full SHA was accepted' >&2; exit 1; }
printf '%s\n' 'PASS unavailable exact SHA is rejected'

before_removals=$(wc -l < "$FIXTURE_REMOVE_LOG" | tr -d ' ')
output=$(FIXTURE_BAD_MKTEMP=1 run_replay "$pass_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
if [[ "$(cat "$TMP/last-rc")" == 0 ]] || ! grep -Fq 'cleanup refused a path outside its mktemp directory' <<< "$output"; then
  printf 'FAIL unsafe temporary path was not rejected:\n%s\n' "$output" >&2
  exit 1
fi
after_removals=$(wc -l < "$FIXTURE_REMOVE_LOG" | tr -d ' ')
[[ "$before_removals" == "$after_removals" ]] && [ ! -s "$FIXTURE_UNSAFE_REMOVE_LOG" ] || {
  printf '%s\n' 'FAIL cleanup attempted to remove outside its mktemp directory' >&2
  exit 1
}
printf '%s\n' 'PASS cleanup refuses a path outside its mktemp directory'
