#!/usr/bin/env bash
set -euo pipefail

TMP=$(mktemp -d /tmp/teagram-tailnet-env-test.XXXXXXXX)
cleanup() {
  local status=$?
  trap - EXIT
  case "$TMP" in
    /tmp/teagram-tailnet-env-test.????????) rm -rf -- "$TMP" || status=1 ;;
    *) printf '%s\n' 'refusing to remove an unexpected fixture directory' >&2; status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

ROOT_FIXTURE=$TMP/root
REPO_FIXTURE=$TMP/repo
BIN=$TMP/bin
OP=$TMP/apply-tailnet-exchange-limit.sh
OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
TARGET_SHA=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
MOCK_OWNER_UID=$(id -u)
REAL_SYNC=$(command -v sync)
mkdir -m 700 "$ROOT_FIXTURE" "$ROOT_FIXTURE/teagramd-rollout-runner" "$REPO_FIXTURE" "$BIN"
mkdir -m 700 "$REPO_FIXTURE/.state"

sed \
  -e "s@readonly ROOT=/root@readonly ROOT=$ROOT_FIXTURE@" \
  -e "s@readonly REPO=/opt/teagram@readonly REPO=$REPO_FIXTURE@" \
  -e "s@readonly DEPLOY_LOCK=/tmp/teagram-deploy.lock@readonly DEPLOY_LOCK=$TMP/deploy.lock@" \
  -e "s@readonly EXPECTED_ROOT_UID=0@readonly EXPECTED_ROOT_UID=$MOCK_OWNER_UID@" \
  deploy/teagram/ops/apply-tailnet-exchange-limit.sh > "$OP"
chmod 700 "$OP"

cat > "$BIN/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
if /usr/bin/flock -n "$MOCK_DEPLOY_LOCK" -c true; then
  printf '%s\n' 'operation lock is not held during git preflight' >&2
  exit 98
fi
[[ "$1" == -C && "$2" == "$MOCK_REPO" ]] || { printf 'unexpected git command\n' >&2; exit 99; }
shift 2
case "$*" in
  'branch --show-current') printf '%s\n' main ;;
  'rev-parse --verify HEAD^{commit}') printf '%s\n' "$MOCK_TARGET_SHA" ;;
  'rev-parse --verify origin/main^{commit}') printf '%s\n' "$MOCK_OPS_SHA" ;;
  'fetch -q origin main') ;;
  'merge-base --is-ancestor '*) exit 0 ;;
  *) printf 'unexpected git command\n' >&2; exit 99 ;;
esac
GIT

cat > "$BIN/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
if /usr/bin/flock -n "$MOCK_DEPLOY_LOCK" -c true; then
  printf '%s\n' 'operation lock is not held during docker preflight or verification' >&2
  exit 98
fi
[[ "$1" == inspect && "$2" == --format ]] || { printf 'unexpected docker command\n' >&2; exit 99; }
format=$3
case "$format" in
  *org.opencontainers.image.revision*) printf '%s\n' "$MOCK_TARGET_SHA" ;;
  *State.Status*) printf '%s\n' running ;;
  *com.docker.compose.project.config_files*) printf '%s/docker-compose.yml,%s/docker-compose.override.yml\n' "$MOCK_REPO" "$MOCK_REPO" ;;
  *'range .Config.Env'*) cat "$MOCK_CONTAINER_ENV_FILE" ;;
  *) printf 'unexpected docker format\n' >&2; exit 99 ;;
esac
DOCKER
chmod 755 "$BIN/git" "$BIN/docker"

cat > "$BIN/sync" <<'SYNC'
#!/usr/bin/env bash
set -euo pipefail
sync_count=0
if [[ -f "$MOCK_SYNC_COUNT" ]]; then
  read -r sync_count < "$MOCK_SYNC_COUNT"
fi
sync_count=$((sync_count + 1))
printf '%s\n' "$sync_count" > "$MOCK_SYNC_COUNT"
if [[ "${MOCK_SYNC_FAIL_ON:-}" == "$sync_count" ]]; then
  exit 1
fi
if [[ "${MOCK_SIGNAL_ON_SYNC:-}" == "$sync_count" ]]; then
  kill -s "$MOCK_SIGNAL" "$PPID"
fi
exec "$MOCK_REAL_SYNC" "$@"
SYNC
chmod 755 "$BIN/sync"

cat > "$ROOT_FIXTURE/teagramd-rollout-runner/rollout-runner.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
[[ "$(readlink "/proc/$BASHPID/fd/9")" == "$MOCK_DEPLOY_LOCK" ]] || {
  printf '%s\n' 'shared deployment lock descriptor was not passed to runner' >&2
  exit 96
}
if /usr/bin/flock -n "$MOCK_DEPLOY_LOCK" -c true; then
  printf '%s\n' 'operation lock is not held through the runner call' >&2
  exit 97
fi
printf '%s\n' called > "$MOCK_RUNNER_CALL"
[[ "$#" == 3 && "$1" == apply && "$2" == "$MOCK_TARGET_SHA" && "$3" == "$MOCK_TARGET_SHA" ]]
[[ "$ROLLOUT_RUNNER_SOURCE_SHA" == "$MOCK_OPS_SHA" ]]
[[ "$COMPOSE_FILE" == "$MOCK_REPO/docker-compose.yml:$MOCK_REPO/docker-compose.override.yml" ]]
[[ "$ROLLOUT_RUNNER_ADDED_ENV" == TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ]]
[[ "$ROLLOUT_RUNNER_PREVIOUS_ENV_FILE" == "$MOCK_BACKUP" ]]
[[ "$(tail -n 1 "$MOCK_ENV_FILE")" == 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' ]]
printf '%s\n' 'runner output contains a private configuration value' >&2
if [[ "$MOCK_RUNNER_MODE" == fail ]]; then
  exit 7
fi
printf 'TG_RATE_LIMIT_KEY_EXCHANGE_IP=20\nTG_KEY_EXCHANGE_PER_CONNECTION=2\nTG_KEY_EXCHANGE_GLOBAL_RATE=5\nTG_KEY_EXCHANGE_GLOBAL_BURST=30\nTG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0\n' > "$MOCK_CONTAINER_ENV_FILE"
if [[ -n "${MOCK_PROCESS_GROUP_SIGNAL:-}" ]]; then
  operation_pgid=$(ps -o pgid= -p "$PPID" | tr -d '[:space:]')
  [[ "$operation_pgid" =~ ^[0-9]+$ ]]
  kill -s "$MOCK_PROCESS_GROUP_SIGNAL" -- "-$operation_pgid"
fi
env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$MOCK_TARGET_SHA" > "$MOCK_ENV_RECORD"
chmod 600 "$MOCK_ENV_RECORD"
RUNNER
chmod 600 "$ROOT_FIXTURE/teagramd-rollout-runner/rollout-runner.sh"

export PATH="$BIN:$PATH"
export OPS_SHA MOCK_REPO="$REPO_FIXTURE" MOCK_TARGET_SHA="$TARGET_SHA" MOCK_OPS_SHA="$OPS_SHA"
export MOCK_CONTAINER_ENV_FILE="$TMP/container-env" MOCK_RUNNER_MODE=success
export MOCK_ENV_FILE="$REPO_FIXTURE/.env" MOCK_ENV_RECORD="$REPO_FIXTURE/.state/rollout-env"
export MOCK_BACKUP="$ROOT_FIXTURE/.env.main-2478-pre-key-exchange-20261025T170009Z"
export MOCK_DEPLOY_LOCK="$TMP/deploy.lock"
export MOCK_SYNC_COUNT="$TMP/sync-count" MOCK_REAL_SYNC="$REAL_SYNC" MOCK_SYNC_FAIL_ON=
MOCK_PROCESS_GROUP_SIGNAL=''
MOCK_SIGNAL_ON_SYNC=''
MOCK_SIGNAL=''
export MOCK_PROCESS_GROUP_SIGNAL MOCK_SIGNAL_ON_SYNC MOCK_SIGNAL

setup_fixture() {
  printf 'FIXTURE_PRIVATE_VALUE=do-not-print-fixture-value\nTG_LOG_LOGIN_CODES=false\n' > "$MOCK_ENV_FILE"
  chmod 600 "$MOCK_ENV_FILE"
  cp -- "$MOCK_ENV_FILE" "$MOCK_BACKUP"
  chmod 600 "$MOCK_BACKUP"
  env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
  printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$TARGET_SHA" > "$MOCK_ENV_RECORD"
  chmod 600 "$MOCK_ENV_RECORD"
  printf 'TG_RATE_LIMIT_KEY_EXCHANGE_IP=20\nTG_KEY_EXCHANGE_PER_CONNECTION=2\nTG_KEY_EXCHANGE_GLOBAL_RATE=5\nTG_KEY_EXCHANGE_GLOBAL_BURST=30\n' > "$MOCK_CONTAINER_ENV_FILE"
  printf '0\n' > "$MOCK_SYNC_COUNT"
  rm -f -- "$TMP/runner-called"
}

run_operation() {
  if [[ -n "$MOCK_PROCESS_GROUP_SIGNAL" ]]; then
    local operation_status=0
    if setsid --wait env OPS_SHA="$OPS_SHA" MOCK_RUNNER_CALL="$TMP/runner-called" bash "$OP" > "$TMP/operation.log" 2>&1; then
      :
    else
      operation_status=$?
    fi
    cat -- "$TMP/operation.log"
    return "$operation_status"
  fi
  env OPS_SHA="$OPS_SHA" MOCK_RUNNER_CALL="$TMP/runner-called" bash "$OP"
}

setup_fixture
success_output=$(run_operation)
[[ "$success_output" == *"rollout=verified target_sha=$TARGET_SHA"* && \
  "$success_output" == *'tailnet_budget=0 other_budgets=unchanged'* ]]
[[ "$success_output" != *'do-not-print-fixture-value'* && "$success_output" != *'private configuration value'* ]]
[[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1) source_sha=$TARGET_SHA" ]]
[[ "$(tail -n 1 "$MOCK_ENV_FILE")" == 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' ]]

setup_fixture
MOCK_SYNC_FAIL_ON=2
export MOCK_SYNC_FAIL_ON
sync_status=0
sync_output=$(run_operation 2>&1) || sync_status=$?
[[ "$sync_status" != 0 ]] || { printf '%s\n' 'post-rename sync failure unexpectedly succeeded' >&2; exit 1; }
cmp -s -- "$MOCK_ENV_FILE" "$MOCK_BACKUP" || {
  printf '%s\n' 'post-rename sync failure did not restore the verified .env' >&2
  exit 1
}
restored_env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
[[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$restored_env_sha source_sha=$TARGET_SHA" ]] || {
  printf '%s\n' 'post-rename sync failure left the rollout record inconsistent' >&2
  exit 1
}
[[ ! -e "$TMP/runner-called" ]] || { printf '%s\n' 'runner ran after post-rename sync failure' >&2; exit 1; }
[[ "$sync_output" == *'rollback_env=restored'* ]] || {
  printf '%s\n' 'post-rename sync failure did not report verified rollback' >&2
  exit 1
}
MOCK_SYNC_FAIL_ON=
export MOCK_SYNC_FAIL_ON

for signal in TERM HUP; do
  setup_fixture
  MOCK_SIGNAL_ON_SYNC=2 MOCK_SIGNAL=$signal
  export MOCK_SIGNAL_ON_SYNC MOCK_SIGNAL
  interrupted_status=0
  interrupted_output=$(run_operation 2>&1) || interrupted_status=$?
  if [[ "$signal" == TERM ]]; then
    expected_status=143
  else
    expected_status=129
  fi
  [[ "$interrupted_status" == "$expected_status" ]] || {
    printf 'post-publish %s did not preserve its signal exit status\n' "$signal" >&2
    exit 1
  }
  cmp -s -- "$MOCK_ENV_FILE" "$MOCK_BACKUP" || {
    printf 'post-publish %s did not restore the verified .env\n' "$signal" >&2
    exit 1
  }
  restored_env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
  [[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$restored_env_sha source_sha=$TARGET_SHA" ]] || {
    printf 'post-publish %s left the rollout record inconsistent\n' "$signal" >&2
    exit 1
  }
  [[ ! -e "$TMP/runner-called" ]] || {
    printf 'runner ran after post-publish %s\n' "$signal" >&2
    exit 1
  }
  [[ "$interrupted_output" == *"signal=$signal"*'rollback_env=restored'* ]] || {
    printf 'post-publish %s did not report verified rollback\n' "$signal" >&2
    exit 1
  }
done
MOCK_SIGNAL_ON_SYNC=
MOCK_SIGNAL=
export MOCK_SIGNAL_ON_SYNC MOCK_SIGNAL

for signal in TERM HUP; do
  setup_fixture
  MOCK_PROCESS_GROUP_SIGNAL=$signal
  export MOCK_PROCESS_GROUP_SIGNAL
  interrupted_status=0
  interrupted_output=$(run_operation 2>&1) || interrupted_status=$?
  if [[ "$signal" == TERM ]]; then
    expected_status=143
  else
    expected_status=129
  fi
  [[ "$interrupted_status" == "$expected_status" ]] || {
    printf 'runner-session %s did not preserve its signal exit status\n' "$signal" >&2
    exit 1
  }
  [[ "$interrupted_output" == *"signal=$signal"*'rollback_env=recorded'* && \
    "$interrupted_output" != *'rollback_env=restored'* ]] || {
    printf 'runner-session %s reported recovery before recording the recreated container\n' "$signal" >&2
    exit 1
  }
  [[ "$(tail -n 1 "$MOCK_ENV_FILE")" == 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' ]] || {
    printf 'runner-session %s did not retain the applied .env with the recreated container\n' "$signal" >&2
    exit 1
  }
  applied_env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
  [[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$applied_env_sha source_sha=$TARGET_SHA" ]] || {
    printf 'runner-session %s did not record the recreated container environment\n' "$signal" >&2
    exit 1
  }
  grep -Fxq 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' "$MOCK_CONTAINER_ENV_FILE" || {
    printf 'runner-session %s did not finish recreating the container\n' "$signal" >&2
    exit 1
  }
done
MOCK_PROCESS_GROUP_SIGNAL=''
export MOCK_PROCESS_GROUP_SIGNAL

setup_fixture
MOCK_RUNNER_MODE=fail
export MOCK_RUNNER_MODE
runner_status=0
failed_output=$(run_operation 2>&1) || runner_status=$?
[[ "$runner_status" == 7 && "$failed_output" == *'runner_result=failed'* && "$failed_output" == *'rollback_env=restored'* ]]
[[ "$failed_output" != *'do-not-print-fixture-value'* && "$failed_output" != *'private configuration value'* ]]
cmp -s -- "$MOCK_ENV_FILE" "$MOCK_BACKUP"
[[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1) source_sha=$TARGET_SHA" ]]

setup_fixture
printf 'env_sha256=%064d source_sha=%s\n' 1 "$TARGET_SHA" > "$MOCK_ENV_RECORD"
chmod 600 "$MOCK_ENV_RECORD"
runner_status=0
stale_output=$(run_operation 2>&1) || runner_status=$?
[[ "$runner_status" != 0 && "$stale_output" == *'do not match the last verified record'* ]]
[[ ! -e "$TMP/runner-called" ]]
cmp -s -- "$MOCK_ENV_FILE" "$MOCK_BACKUP"

setup_fixture
printf 'FIXTURE_PRIVATE_VALUE=do-not-print-fixture-value\nTG_LOG_LOGIN_CODES=false\nTG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=20\n' > "$MOCK_ENV_FILE"
cp -- "$MOCK_ENV_FILE" "$MOCK_BACKUP"
chmod 600 "$MOCK_ENV_FILE" "$MOCK_BACKUP"
env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$TARGET_SHA" > "$MOCK_ENV_RECORD"
chmod 600 "$MOCK_ENV_RECORD"
runner_status=0
existing_output=$(run_operation 2>&1) || runner_status=$?
[[ "$runner_status" != 0 && "$existing_output" == *'already exists in the pre-change .env'* ]]
[[ ! -e "$TMP/runner-called" ]]

runner_lock_status=0
exec 9>"$TMP/inherited-runner.lock"
flock -x 9
export ROLLOUT_RUNNER_TEST_MODE=1 ROLLOUT_RUNNER_LOCK_PATH="$TMP/inherited-runner.lock"
# Child Bash expands the inherited lock path after it starts.
# shellcheck disable=SC2016
timeout 3s bash -c '
  set -Eeuo pipefail
  export ROLLOUT_RUNNER_SOURCE_ONLY=1
  source deploy/teagramd/rollout-runner/rollout-runner.sh
  [[ "$(readlink "/proc/$BASHPID/fd/9")" == "$ROLLOUT_RUNNER_LOCK_PATH" ]]
  acquire_shared_lock
  [[ "$(readlink "/proc/$BASHPID/fd/9")" == "$ROLLOUT_RUNNER_LOCK_PATH" ]]
' || runner_lock_status=$?
flock -u 9
exec 9>&-
[[ "$runner_lock_status" == 0 ]]

printf '%s\n' 'tailnet exchange apply operation fixture passed'
