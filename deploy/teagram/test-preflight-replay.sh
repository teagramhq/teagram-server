#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/ops/preflight-replay.sh"
TMP=$(mktemp -d /tmp/teagram-preflight-replay-test.XXXXXXXXXX)
REAL_RM=$(command -v rm)
REAL_MKTEMP=$(command -v mktemp)
trap '"$REAL_RM" -rf -- "$TMP"' EXIT
chmod 700 "$TMP"

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
git -C "$TMP/source" switch -q -c drift
cat > "$TMP/source/docker-compose.alternate.yml" <<'COMPOSE'
fixture: alternate
# FIXTURE_DRIFT
COMPOSE
git -C "$TMP/source" add docker-compose.alternate.yml
git -C "$TMP/source" commit -q -m drift
git -C "$TMP/source" push -q origin drift
drift_sha=$(git -C "$TMP/source" rev-parse HEAD)

git clone -q "$TMP/origin.git" "$TMP/live"
git -C "$TMP/live" switch -q main
printf 'fixture-password=never-print\n' > "$TMP/live/.env"
printf '# fixture override\n' > "$TMP/live/docker-compose.override.yml"
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
  jq -cn --arg selection "$selection" '[{
    Id:"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    Image:"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    Config:{
      Env:["TG_REPLICA_COUNT=1","TG_CLIENT_ADDR_TRUST=socket","TG_LOG_LOGIN_CODES=false"],
      StopTimeout:120,
      Labels:{"com.docker.compose.project":"teagram","com.docker.compose.project.config_files":$selection}
    },
    State:{Status:"running",ExitCode:0,StartedAt:"2026-10-16T12:00:00Z",FinishedAt:"0001-01-01T00:00:00Z"},
    HostConfig:{LogConfig:{Type:"syslog",Config:{
      "syslog-address":"unixgram:///dev/log","syslog-facility":"daemon",
      "syslog-format":"rfc5424",tag:"teagramd"
    }},PortBindings:{}},
    Mounts:[]
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
  [[ "$actual_selection" == "${FIXTURE_EXPECTED_FILES:?}" ]] || {
    printf '%s\n' 'fixture Compose selection did not follow the live label' >&2
    exit 91
  }
  grace=2m0s
  if grep -q FIXTURE_DRIFT "$project_dir/docker-compose.alternate.yml" 2>/dev/null; then grace=3m0s; fi
  jq -cn --arg grace "$grace" '{services:{teagramd:{
    stop_grace_period:$grace,
    ports:[],volumes:[],network_mode:"",networks:{},
    logging:{driver:"syslog",options:{
      "syslog-address":"unixgram:///dev/log","syslog-facility":"daemon",
      "syslog-format":"rfc5424",tag:"teagramd"
    }},
    environment:{TG_REPLICA_COUNT:"1",TG_CLIENT_ADDR_TRUST:"socket",TG_LOG_LOGIN_CODES:"false"}
  }}}'
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
  /tmp/teagram-preflight-replay.[A-Za-z0-9]*|/tmp/teagram-preflight-replay.[A-Za-z0-9]*/*)
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
if [[ "${FIXTURE_BAD_MKTEMP:-0}" == 1 ]]; then
  printf '%s\n' /tmp/outside-preflight-replay
  exit 0
fi
exec "$REAL_MKTEMP" "$@"
MKTEMP
chmod 700 "$TMP/bin/mktemp"

export PATH="$TMP/bin:$PATH"
export REAL_RM REAL_MKTEMP
export FIXTURE_LIVE_ROOT="$TMP/live"
export FIXTURE_REMOVE_LOG="$TMP/removals.log"
export FIXTURE_UNSAFE_REMOVE_LOG="$TMP/unsafe-removals.log"
export PREFLIGHT_REPLAY_TEST_MODE=1
export PREFLIGHT_REPLAY_TEST_LIVE_CHECKOUT="$TMP/live"
export PREFLIGHT_REPLAY_TEST_REPOSITORY_URL="$TMP/origin.git"
: > "$FIXTURE_REMOVE_LOG"
: > "$FIXTURE_UNSAFE_REMOVE_LOG"

run_replay() {
  local sha=$1 selection=$2 output rc=0
  output=$(PREFLIGHT_REPLAY_TEST_MODE=1 \
      PREFLIGHT_REPLAY_TEST_LIVE_CHECKOUT="$TMP/live" \
      PREFLIGHT_REPLAY_TEST_REPOSITORY_URL="$TMP/origin.git" \
      FIXTURE_LIVE_ROOT="$TMP/live" FIXTURE_LABEL_FILES="$selection" \
      FIXTURE_EXPECTED_FILES="$selection" FIXTURE_REMOVE_LOG="$FIXTURE_REMOVE_LOG" \
      FIXTURE_UNSAFE_REMOVE_LOG="$FIXTURE_UNSAFE_REMOVE_LOG" REAL_RM="$REAL_RM" \
      REAL_MKTEMP="$REAL_MKTEMP" FIXTURE_BAD_MKTEMP="${FIXTURE_BAD_MKTEMP:-0}" \
      PREFLIGHT_REPLAY_TEST_FORCE_COMPAT="${PREFLIGHT_REPLAY_TEST_FORCE_COMPAT:-0}" \
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

# The alternate filename differs from the default Compose selection. The Docker
# stub checks that the verifier received the exact label order.
output=$(PREFLIGHT_REPLAY_TEST_FORCE_COMPAT=1 run_replay "$pass_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" == 0 ]] || { printf 'FAIL dynamic selection replay:\n%s\n' "$output" >&2; exit 1; }
printf '%s\n' 'PASS non-default live selection is followed in order'

output=$(run_replay "$drift_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" != 0 ]] && grep -Fq $'stop_grace_period\tfail' <<< "$output" || {
  printf 'FAIL drifted replay was not rejected:\n%s\n' "$output" >&2
  exit 1
}
if grep -Eq '2m0s|3m0s|fixture-password' <<< "$output"; then
  printf 'FAIL drifted replay exposed values:\n%s\n' "$output" >&2
  exit 1
fi
printf '%s\n' 'PASS drifted replay exits non-zero and redacts values'

bad_sha=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
output=$(run_replay "$bad_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" != 0 ]] || { printf '%s\n' 'FAIL nonexistent full SHA was accepted' >&2; exit 1; }
printf '%s\n' 'PASS unavailable exact SHA is rejected'

before_removals=$(wc -l < "$FIXTURE_REMOVE_LOG" | tr -d ' ')
output=$(FIXTURE_BAD_MKTEMP=1 run_replay "$pass_sha" 'docker-compose.alternate.yml,docker-compose.override.yml')
[[ "$(cat "$TMP/last-rc")" != 0 ]] && grep -Fq 'cleanup refused a path outside its mktemp directory' <<< "$output" || {
  printf 'FAIL unsafe temporary path was not rejected:\n%s\n' "$output" >&2
  exit 1
}
after_removals=$(wc -l < "$FIXTURE_REMOVE_LOG" | tr -d ' ')
[[ "$before_removals" == "$after_removals" ]] && [ ! -s "$FIXTURE_UNSAFE_REMOVE_LOG" ] || {
  printf '%s\n' 'FAIL cleanup attempted to remove outside its mktemp directory' >&2
  exit 1
}
printf '%s\n' 'PASS cleanup refuses a path outside its mktemp directory'
