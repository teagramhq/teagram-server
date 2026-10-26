#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/teagram-ops.sh"
replay_script="$(cd "$(dirname "$0")" && pwd)/ops/replay-check.sh"
tmp=$(mktemp -d)
unrelated=$(mktemp -d /tmp/teagram-ops.test-unrelated.XXXXXX)
cleanup() {
  /bin/rm -rf -- "$tmp"
  /bin/rm -rf -- "$unrelated"
}
trap cleanup EXIT

mkdir -p "$tmp/bin" "$tmp/repo/.state" "$tmp/root"
printf 'TG_RSA_KEY_FINGERPRINT=fixture-value-never-print\nFIXTURE_EMPTY=\n' > "$tmp/repo/.env"
printf 'override fixture\n' > "$tmp/repo/docker-compose.override.yml"
printf 'state fixture\n' > "$tmp/repo/.state/rollout-env"
printf 'compose fixture\n' > "$tmp/repo/docker-compose.yml"
printf 'local blobs fixture\n' > "$tmp/repo/docker-compose.local-blobs.yml"
printf 'fixture env only\n' > "$tmp/repo/.env.rustfs-parked"

OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
OFF_MAIN_SHA=cccccccccccccccccccccccccccccccccccccccc
CONTAINER_ID=dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
export OPS_SHA TARGET_SHA OFF_MAIN_SHA CONTAINER_ID
export MOCK_CURRENT_SHA="$TARGET_SHA" MOCK_REPO="$tmp/repo"
export MOCK_OP_SCRIPT="$replay_script" MOCK_VERIFIER="$tmp/verifier.sh" MOCK_RUNNER="$tmp/runner.sh"
export MOCK_RUN_TEMP_LOG="$tmp/run-temp.log" MOCK_RM_LOG="$tmp/rm.log"
export MOCK_VERIFIER_CALL="$tmp/verifier-call.log"

cat > "$tmp/bin/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1" == -C ]] || { echo "unexpected git call: $*" >&2; exit 99; }
shift 2
case "$1" in
  rev-parse) [[ "${2:-}" == HEAD ]] && printf '%s\n' "$MOCK_CURRENT_SHA" ;;
  branch) [[ "${2:-}" == --show-current ]] && printf '%s\n' main ;;
  status) [[ "${2:-}" == --porcelain ]] && printf ' M fixture\n' ;;
  fetch) exit 0 ;;
  merge-base)
    if [[ "${2:-}" == --is-ancestor && "${4:-}" == origin/main && \
      ( "${3:-}" == "$OPS_SHA" || "${3:-}" == "$TARGET_SHA" ) ]]; then
      exit 0
    fi
    exit 1
    ;;
  cat-file)
    [[ "${2:-}" == -e ]] || exit 99
    [[ "${3:-}" == "$OPS_SHA:deploy/teagram/ops/replay-check.sh" || \
      "${3:-}" == "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-runner.sh" || \
      "${3:-}" == "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-verifier.sh" ]]
    ;;
  show)
    case "${2:-}" in
      "$OPS_SHA:deploy/teagram/ops/replay-check.sh") sed "s|/opt/teagram|$MOCK_REPO|g" "$MOCK_OP_SCRIPT" ;;
      "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-runner.sh") cat "$MOCK_RUNNER" ;;
      "$TARGET_SHA:deploy/teagramd/rollout-runner/rollout-verifier.sh") cat "$MOCK_VERIFIER" ;;
      *) exit 1 ;;
    esac
    ;;
  *) echo "unexpected git call: $*" >&2; exit 99 ;;
esac
GIT

cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  'ps '*) printf '%s\n' 'teagram-teagramd-1 image:fixture Up 1 minute' ;;
  *'{{.Id}}'*) printf '%s\n' "$CONTAINER_ID" ;;
  *'{{.Image}}'*) printf '%s\n' 'sha256:fixture' ;;
  *'{{.State.StartedAt}}'*) printf '%s\n' '2026-10-26T00:00:00Z' ;;
  *'org.opencontainers.image.revision'*) printf '%s\n' "$TARGET_SHA" ;;
  *'com.docker.compose.project.config_files'*)
    printf '%s,%s\n' "$MOCK_REPO/docker-compose.yml" "$MOCK_REPO/docker-compose.override.yml"
    ;;
  *) echo "unexpected docker call: $*" >&2; exit 99 ;;
esac
DOCKER

cat > "$tmp/bin/logger" <<'LOGGER'
#!/bin/sh
exit 0
LOGGER
cat > "$tmp/bin/hostname" <<'HOSTNAME'
#!/bin/sh
printf '%s\n' fixture-host
HOSTNAME
cat > "$tmp/bin/mktemp" <<'MKTEMP'
#!/bin/sh
for arg do
  if [ "$arg" = /tmp/teagram-ops.XXXXXX ]; then
    path=$(/usr/bin/mktemp "$@")
    printf '%s\n' "$path" > "$MOCK_RUN_TEMP_LOG"
    printf '%s\n' "$path"
    exit 0
  fi
done
exec /usr/bin/mktemp "$@"
MKTEMP
cat > "$tmp/bin/rm" <<'RM'
#!/bin/sh
for arg do
  case "$arg" in
    /tmp/teagram-ops.*) printf '%s\n' "$arg" >> "$MOCK_RM_LOG" ;;
  esac
done
exec /bin/rm "$@"
RM
cat > "$tmp/bin/teagram-logs" <<'LOGS'
#!/bin/sh
printf 'log_reader=%s\n' "${SSH_ORIGINAL_COMMAND:-}"
LOGS
chmod 755 "$tmp/bin/"*

cat > "$MOCK_VERIFIER" <<'VERIFIER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$#" == 4 && "$1" == ready && "$2" == target && "$3" == "$CONTAINER_ID" ]]
[[ "$PWD" == "$MOCK_REPO" && "$ROLLOUT_CHECKOUT_PATH" == "$MOCK_REPO" ]]
printf 'sha=%s args=%s compose=%s env_files=%s cwd=%s\n' \
  "$TARGET_SHA" "$*" "$COMPOSE_FILE" "$COMPOSE_ENV_FILES" "$PWD" > "$MOCK_VERIFIER_CALL"
printf 'sample=ready\n' > "$4/readiness-target.tsv"
printf 'role=target result=ready max_seconds=120\n' > "$4/readiness-target.result"
printf '%s\n' 'role=target result=ready max_seconds=120'
VERIFIER
chmod 755 "$MOCK_VERIFIER"
verifier_sha=$(sha256sum "$MOCK_VERIFIER" | cut -d' ' -f1)
printf 'readonly APPROVED_VERIFIER_SHA=%s\n' "$verifier_sha" > "$MOCK_RUNNER"

sed \
  -e "s|/opt/teagram|$tmp/repo|g" \
  -e "s|/usr/local/sbin/teagram-logs|$tmp/bin/teagram-logs|g" \
  "$script" > "$tmp/teagram-ops.sh"
chmod 755 "$tmp/teagram-ops.sh"
export PATH="$tmp/bin:$PATH"

run_command() {
  SSH_ORIGINAL_COMMAND=$1 bash "$tmp/teagram-ops.sh"
}
expect_status() {
  local expected=$1 command=$2 output status=0
  output=$(run_command "$command" 2>&1) || status=$?
  if [[ "$status" != "$expected" ]]; then
    printf 'FAIL command %q: expected exit %s, got %s\n%s\n' "$command" "$expected" "$status" "$output" >&2
    return 1
  fi
}

[[ "$(run_command hostname)" == fixture-host ]]
live=$(run_command live)
[[ "$live" == *"live_sha=$TARGET_SHA"* && "$live" == *'dirty_paths=1'* && "$live" == *'container_id='* ]]
[[ "$(run_command ps)" == *'teagram-teagramd-1 image:fixture Up 1 minute'* ]]
[[ "$(run_command 'logs 2026-10-26T00:00:00Z')" == 'log_reader=2026-10-26T00:00:00Z' ]]
expect_status 64 'logs'
expect_status 64 'logs now extra'

env_output=$(run_command env-names)
[[ "$env_output" == $'FIXTURE_EMPTY\nTG_RSA_KEY_FINGERPRINT' ]]
[[ "$env_output" != *fixture-value-never-print* ]]
fingerprints=$(run_command fingerprints)
[[ "$(printf '%s\n' "$fingerprints" | wc -l | tr -d ' ')" == 5 ]]
[[ "$fingerprints" != *fixture-value-never-print* ]]
for help in help -h --help; do
  help_output=$(run_command "$help" 2>&1)
  [[ "$help_output" == *'hostname'* && "$help_output" == *'env-names'* && "$help_output" == *'run'* ]]
done

sentinel="$tmp/arbitrary-command-ran"
expect_status 64 "touch $sentinel"
[[ ! -e "$sentinel" ]]
expect_status 64 'run'
expect_status 64 "run gggggggggggggggggggggggggggggggggggggggg replay-check $TARGET_SHA"
expect_status 64 "run ${OPS_SHA:0:7} replay-check $TARGET_SHA"
expect_status 65 "run $OFF_MAIN_SHA replay-check $TARGET_SHA"
expect_status 66 "run $OPS_SHA missing-script $TARGET_SHA"
for bad_name in 'nested/name' . .. 'Uppercase'; do
  expect_status 64 "run $OPS_SHA $bad_name $TARGET_SHA"
done

mkdir -p "$unrelated/keep"
printf 'keep this directory\n' > "$unrelated/keep/sentinel"
output=$(run_command "run $OPS_SHA replay-check $TARGET_SHA")
[[ "$output" == *'role=target result=ready'* && "$output" == *"replay_check=pass target_sha=$TARGET_SHA evidence="* ]]
evidence=${output##*evidence=}
[[ -d "$evidence" && -f "$evidence/readiness-target.tsv" && -f "$evidence/replay-check.result" ]]
[[ "$(cat "$evidence/replay-check.result")" == "result=pass target_sha=$TARGET_SHA container_id=$CONTAINER_ID evidence=$evidence" ]]
[[ "$(cat "$MOCK_VERIFIER_CALL")" == *"sha=$TARGET_SHA args=ready target $CONTAINER_ID $evidence compose=$MOCK_REPO/docker-compose.yml:$MOCK_REPO/docker-compose.override.yml env_files=$MOCK_REPO/.env,$MOCK_REPO/.env.rustfs-parked cwd=$MOCK_REPO"* ]]

runner_temp=$(<"$MOCK_RUN_TEMP_LOG")
[[ "$runner_temp" == /tmp/teagram-ops.* && ! -e "$runner_temp" ]]
[[ "$(<"$MOCK_RM_LOG")" == "$runner_temp" ]]
[[ -f "$unrelated/keep/sentinel" ]]
/bin/rm -rf -- "$evidence"

printf '%s\n' 'teagram-ops fixture cases pass'
