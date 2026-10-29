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
mkdir -m 700 "$ROOT_FIXTURE" "$ROOT_FIXTURE/teagramd-rollout-runner" "$REPO_FIXTURE" "$BIN"
mkdir -m 700 "$REPO_FIXTURE/.state"

sed \
  -e "s@readonly ROOT=/root@readonly ROOT=$ROOT_FIXTURE@" \
  -e "s@readonly REPO=/opt/teagram@readonly REPO=$REPO_FIXTURE@" \
  -e "s@readonly EXPECTED_ROOT_UID=0@readonly EXPECTED_ROOT_UID=$MOCK_OWNER_UID@" \
  deploy/teagram/ops/apply-tailnet-exchange-limit.sh > "$OP"
chmod 700 "$OP"

cat > "$BIN/git" <<'GIT'
#!/usr/bin/env bash
set -euo pipefail
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

cat > "$ROOT_FIXTURE/teagramd-rollout-runner/rollout-runner.sh" <<'RUNNER'
#!/usr/bin/env bash
set -euo pipefail
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
env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$MOCK_TARGET_SHA" > "$MOCK_ENV_RECORD"
chmod 600 "$MOCK_ENV_RECORD"
printf 'TG_RATE_LIMIT_KEY_EXCHANGE_IP=20\nTG_KEY_EXCHANGE_PER_CONNECTION=2\nTG_KEY_EXCHANGE_GLOBAL_RATE=5\nTG_KEY_EXCHANGE_GLOBAL_BURST=30\nTG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0\n' > "$MOCK_CONTAINER_ENV_FILE"
RUNNER
chmod 600 "$ROOT_FIXTURE/teagramd-rollout-runner/rollout-runner.sh"

export PATH="$BIN:$PATH"
export OPS_SHA MOCK_REPO="$REPO_FIXTURE" MOCK_TARGET_SHA="$TARGET_SHA" MOCK_OPS_SHA="$OPS_SHA"
export MOCK_CONTAINER_ENV_FILE="$TMP/container-env" MOCK_RUNNER_MODE=success
export MOCK_ENV_FILE="$REPO_FIXTURE/.env" MOCK_ENV_RECORD="$REPO_FIXTURE/.state/rollout-env"
export MOCK_BACKUP="$ROOT_FIXTURE/.env.main-2478-pre-key-exchange-20261025T170009Z"

setup_fixture() {
  printf 'FIXTURE_PRIVATE_VALUE=do-not-print-fixture-value\nTG_LOG_LOGIN_CODES=false\n' > "$MOCK_ENV_FILE"
  chmod 600 "$MOCK_ENV_FILE"
  cp -- "$MOCK_ENV_FILE" "$MOCK_BACKUP"
  chmod 600 "$MOCK_BACKUP"
  env_sha=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1)
  printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$TARGET_SHA" > "$MOCK_ENV_RECORD"
  chmod 600 "$MOCK_ENV_RECORD"
  printf 'TG_RATE_LIMIT_KEY_EXCHANGE_IP=20\nTG_KEY_EXCHANGE_PER_CONNECTION=2\nTG_KEY_EXCHANGE_GLOBAL_RATE=5\nTG_KEY_EXCHANGE_GLOBAL_BURST=30\n' > "$MOCK_CONTAINER_ENV_FILE"
  rm -f -- "$TMP/runner-called"
}

run_operation() {
  env OPS_SHA="$OPS_SHA" MOCK_RUNNER_CALL="$TMP/runner-called" bash "$OP" "$@"
}

setup_fixture
success_output=$(run_operation)
[[ "$success_output" == *"rollout=verified target_sha=$TARGET_SHA"* && \
  "$success_output" == *'tailnet_budget=0 other_budgets=unchanged'* ]]
[[ "$success_output" != *'do-not-print-fixture-value'* && "$success_output" != *'private configuration value'* ]]
[[ "$(cat "$MOCK_ENV_RECORD")" == "env_sha256=$(sha256sum "$MOCK_ENV_FILE" | cut -d' ' -f1) source_sha=$TARGET_SHA" ]]
[[ "$(tail -n 1 "$MOCK_ENV_FILE")" == 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET=0' ]]

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

printf '%s\n' 'tailnet exchange apply operation fixture passed'
