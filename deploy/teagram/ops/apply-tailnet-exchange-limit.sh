#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly ROOT=/root
readonly REPO=/opt/teagram
readonly DEPLOY_LOCK=/tmp/teagram-deploy.lock
readonly SERVICE=teagram-teagramd-1
readonly ENV_FILE=$REPO/.env
readonly ENV_RECORD=$REPO/.state/rollout-env
readonly PREVIOUS_ENV_FILE=$ROOT/.env.main-2478-pre-key-exchange-20261025T170009Z
readonly RUNNER=$ROOT/teagramd-rollout-runner/rollout-runner.sh
readonly TAILNET_ENV=TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET
readonly COMPOSE_FILES=$REPO/docker-compose.yml:$REPO/docker-compose.override.yml
readonly EXPECTED_ROOT_UID=0
readonly IMAGE_REVISION_FORMAT='{{index .Config.Labels "org.opencontainers.image.revision"}}'
readonly CONTAINER_STATE_FORMAT='{{.State.Status}}'

fail() {
  printf 'tailnet exchange apply rejected: %s\n' "$1" >&2
  return 1
}

private_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "$EXPECTED_ROOT_UID:600" ]]
}

file_sha256() {
  local output
  output=$(sha256sum -- "$1") || return 1
  printf '%s' "${output%% *}"
}

restore_recorded_env() {
  local restored_env_sha restored_record
  TEMP_ENV=$(mktemp "$REPO/.env.main-2478.XXXXXXXX") || return 1
  if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || ! chmod 600 -- "$TEMP_ENV"; then
    return 1
  fi
  if ! mv -f -- "$TEMP_ENV" "$ENV_FILE"; then
    return 1
  fi
  TEMP_ENV=
  sync || return 1
  restored_env_sha=$(file_sha256 "$ENV_FILE") || return 1
  restored_record=$(cat -- "$ENV_RECORD") || return 1
  [[ "$restored_env_sha" == "$recorded_env_sha" && "$restored_record" == "$record" ]] || return 1
  cmp -s -- "$PREVIOUS_ENV_FILE" "$ENV_FILE"
}

budget_fingerprint() {
  local env_output
  env_output=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$SERVICE") || return 1
  printf '%s\n' "$env_output" | awk -F= '
    $1 == "TG_RATE_LIMIT_KEY_EXCHANGE_IP" ||
    $1 == "TG_KEY_EXCHANGE_PER_CONNECTION" ||
    $1 == "TG_KEY_EXCHANGE_GLOBAL_RATE" ||
    $1 == "TG_KEY_EXCHANGE_GLOBAL_BURST" ||
    $1 == "TG_RATE_LIMIT_KEY_EXCHANGE_IP_WINDOW" { print }
  ' | LC_ALL=C sort | sha256sum | cut -d' ' -f1
}

read_tailnet_value() {
  local env_output line count=0 value=
  env_output=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$SERVICE") || return 1
  while IFS= read -r line; do
    if [[ "$line" == "$TAILNET_ENV="* ]]; then
      count=$((count + 1))
      value=${line#*=}
    fi
  done <<< "$env_output"
  [[ "$count" -le 1 ]] || return 1
  printf '%s' "$value"
}

TEMP_ENV=
TEMP_DIR=
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$TEMP_ENV" ]]; then
    case "$TEMP_ENV" in
      "$REPO"/.env.main-2478.????????) rm -f -- "$TEMP_ENV" || status=1 ;;
      *) printf '%s\n' 'tailnet exchange apply: refused cleanup of an unexpected environment path' >&2; status=1 ;;
    esac
  fi
  if [[ -n "$TEMP_DIR" ]]; then
    case "$TEMP_DIR" in
      /tmp/teagram-tailnet-env-apply.????????) rm -rf -- "$TEMP_DIR" || status=1 ;;
      *) printf '%s\n' 'tailnet exchange apply: refused cleanup of an unexpected temporary directory' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}
trap cleanup EXIT

if [[ "$#" -ne 0 ]]; then
  printf '%s\n' 'usage: apply-tailnet-exchange-limit' >&2
  exit 64
fi
[[ "$(id -u)" == "$EXPECTED_ROOT_UID" ]] || { fail 'operation requires root'; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { fail 'reviewed operation SHA is invalid'; exit 64; }
# Keep the rollout runner's shared lock across preflight, the .env update, and apply.
exec 9>"$DEPLOY_LOCK"
flock -x 9 || { fail 'cannot acquire shared deployment lock'; exit 1; }
[[ -d "$ROOT" && ! -L "$ROOT" && "$(stat -c '%u:%a' -- "$ROOT")" == "$EXPECTED_ROOT_UID:700" ]] || {
  fail 'root directory failed ownership or mode checks'
  exit 1
}
[[ -d "$REPO" && ! -L "$REPO" && "$(realpath -e -- "$REPO")" == "$REPO" ]] || {
  fail 'live checkout is missing or symlinked'
  exit 1
}
[[ -d "$REPO/.state" && ! -L "$REPO/.state" && "$(stat -c '%u:%a' -- "$REPO/.state")" == "$EXPECTED_ROOT_UID:700" ]] || {
  fail 'deployment state directory failed ownership or mode checks'
  exit 1
}
private_file "$ENV_FILE" || { fail 'live .env is not a root-owned mode-0600 regular file'; exit 1; }
private_file "$ENV_RECORD" || { fail 'verified .env record is not a root-owned mode-0600 regular file'; exit 1; }
private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is not a root-owned mode-0600 regular file'; exit 1; }
private_file "$RUNNER" || { fail 'reviewed rollout runner is unavailable or unsafe'; exit 1; }

record=$(cat -- "$ENV_RECORD") || { fail 'cannot read verified .env record'; exit 1; }
[[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || {
  fail 'verified .env record is malformed'
  exit 1
}
recorded_env_sha=${BASH_REMATCH[1]}
previous_env_sha=$(file_sha256 "$PREVIOUS_ENV_FILE") || { fail 'cannot hash pre-change .env backup'; exit 1; }
current_env_sha=$(file_sha256 "$ENV_FILE") || { fail 'cannot hash live .env'; exit 1; }
[[ "$previous_env_sha" == "$recorded_env_sha" && "$current_env_sha" == "$recorded_env_sha" ]] || {
  fail 'live .env and its backup do not match the last verified record'
  exit 1
}
cmp -s -- "$PREVIOUS_ENV_FILE" "$ENV_FILE" || { fail 'live .env is not byte-identical to the recorded backup'; exit 1; }
if grep -Eq "^[[:space:]]*(export[[:space:]]+)?${TAILNET_ENV}[[:space:]]*=" -- "$PREVIOUS_ENV_FILE"; then
  fail 'tailnet exchange setting already exists in the pre-change .env'
  exit 1
fi

branch=$(git -C "$REPO" branch --show-current) || { fail 'cannot read live checkout branch'; exit 1; }
baseline_sha=$(git -C "$REPO" rev-parse --verify 'HEAD^{commit}') || { fail 'cannot read live checkout SHA'; exit 1; }
[[ "$branch" == main || -z "$branch" ]] || { fail 'live checkout is not on main'; exit 1; }
target_sha=$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$SERVICE") || {
  fail 'cannot read live container revision'
  exit 1
}
[[ "$target_sha" =~ ^[0-9a-f]{40}$ && "$target_sha" == "$baseline_sha" ]] || {
  fail 'live image revision does not match the live checkout'
  exit 1
}
container_state=$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE") || {
  fail 'cannot read live container state'
  exit 1
}
[[ "$container_state" == running ]] || { fail 'live container is not running'; exit 1; }
compose_selection=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$SERVICE") || {
  fail 'cannot read live Compose selection'
  exit 1
}
[[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || {
  fail 'live Compose selection is not the reviewed teagram stack'
  exit 1
}
git -C "$REPO" fetch -q origin main || { fail 'cannot refresh origin/main'; exit 1; }
origin_sha=$(git -C "$REPO" rev-parse --verify 'origin/main^{commit}') || { fail 'cannot read origin/main SHA'; exit 1; }
git -C "$REPO" merge-base --is-ancestor "$target_sha" "$origin_sha" || {
  fail 'live target is no longer on origin/main'
  exit 1
}
git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$origin_sha" || {
  fail 'reviewed operation source is no longer on origin/main'
  exit 1
}
git -C "$REPO" merge-base --is-ancestor "$target_sha" "$OPS_SHA" || {
  fail 'reviewed operation source does not descend from the live target'
  exit 1
}

existing_tailnet_value=$(read_tailnet_value) || { fail 'cannot inspect the live tailnet setting'; exit 1; }
[[ -z "$existing_tailnet_value" ]] || { fail 'live container already has a tailnet exchange setting'; exit 1; }
public_budget_before=$(budget_fingerprint) || { fail 'cannot fingerprint retained exchange budgets'; exit 1; }

TEMP_DIR=$(mktemp -d /tmp/teagram-tailnet-env-apply.XXXXXXXX) || { fail 'cannot create private runner output directory'; exit 1; }
[[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%u:%a' -- "$TEMP_DIR")" == "$EXPECTED_ROOT_UID:700" ]] || {
  fail 'runner output directory failed ownership or mode checks'
  exit 1
}
TEMP_ENV=$(mktemp "$REPO/.env.main-2478.XXXXXXXX") || { fail 'cannot stage the named .env addition'; exit 1; }
if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || ! printf '%s=0\n' "$TAILNET_ENV" >> "$TEMP_ENV" || \
   ! chmod 600 -- "$TEMP_ENV" || ! sync; then
  fail 'cannot prepare the named .env addition'
  exit 1
fi
expected_env_sha=$(file_sha256 "$TEMP_ENV") || { fail 'cannot hash staged .env'; exit 1; }
mv -f -- "$TEMP_ENV" "$ENV_FILE" || { fail 'cannot atomically publish the named .env addition'; exit 1; }
TEMP_ENV=
sync_status=0
sync || sync_status=$?
if [[ "$sync_status" -ne 0 ]]; then
  if restore_recorded_env; then
    printf 'env_update=failed sync_exit=%s target_sha=%s rollback_env=restored\n' "$sync_status" "$target_sha"
  else
    printf 'env_update=failed sync_exit=%s target_sha=%s rollback_env=unverified\n' "$sync_status" "$target_sha"
  fi
  exit "$sync_status"
fi

runner_status=0
if env \
  ROLLOUT_RUNNER_SOURCE_SHA="$OPS_SHA" \
  COMPOSE_FILE="$COMPOSE_FILES" \
  ROLLOUT_RUNNER_ADDED_ENV="$TAILNET_ENV" \
  ROLLOUT_RUNNER_PREVIOUS_ENV_FILE="$PREVIOUS_ENV_FILE" \
  bash "$RUNNER" apply "$target_sha" "$baseline_sha" > "$TEMP_DIR/runner.log" 2>&1; then
  :
else
  runner_status=$?
fi

if [[ "$runner_status" -ne 0 ]]; then
  failed_env_sha=$(file_sha256 "$ENV_FILE" 2>/dev/null || true)
  failed_record=$(cat -- "$ENV_RECORD" 2>/dev/null || true)
  if [[ "$failed_env_sha" == "$recorded_env_sha" && "$failed_record" == "$record" ]]; then
    printf 'runner_result=failed exit=%s target_sha=%s rollback_env=verified\n' "$runner_status" "$target_sha"
  elif [[ "$failed_env_sha" == "$expected_env_sha" && "$failed_record" == "$record" && \
      "$(docker inspect --format "$IMAGE_REVISION_FORMAT" "$SERVICE" 2>/dev/null || true)" == "$target_sha" && \
      "$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE" 2>/dev/null || true)" == running && \
      "$(read_tailnet_value 2>/dev/null || true)" == '' ]]; then
    TEMP_ENV=$(mktemp "$REPO/.env.main-2478.XXXXXXXX") || {
      printf 'runner_result=failed exit=%s target_sha=%s rollback_env=unverified\n' "$runner_status" "$target_sha"
      exit "$runner_status"
    }
    if cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" && chmod 600 -- "$TEMP_ENV" && sync && \
       mv -f -- "$TEMP_ENV" "$ENV_FILE" && sync; then
      TEMP_ENV=
      restored_env_sha=$(file_sha256 "$ENV_FILE" 2>/dev/null || true)
      restored_record=$(cat -- "$ENV_RECORD" 2>/dev/null || true)
      if [[ "$restored_env_sha" == "$recorded_env_sha" && "$restored_record" == "$record" ]]; then
        printf 'runner_result=failed exit=%s target_sha=%s rollback_env=restored\n' "$runner_status" "$target_sha"
      else
        printf 'runner_result=failed exit=%s target_sha=%s rollback_env=unverified\n' "$runner_status" "$target_sha"
      fi
    else
      printf 'runner_result=failed exit=%s target_sha=%s rollback_env=unverified\n' "$runner_status" "$target_sha"
    fi
  else
    printf 'runner_result=failed exit=%s target_sha=%s rollback_env=unverified evidence=rollout-evidence\n' "$runner_status" "$target_sha"
  fi
  if [[ -f "$REPO/deploy/teagram/ops/rollout-evidence.sh" && ! -L "$REPO/deploy/teagram/ops/rollout-evidence.sh" ]] && \
     bash "$REPO/deploy/teagram/ops/rollout-evidence.sh" > "$TEMP_DIR/rollout-evidence.txt" 2>/dev/null; then
    cat -- "$TEMP_DIR/rollout-evidence.txt"
  fi
  exit "$runner_status"
fi

actual_env_sha=$(file_sha256 "$ENV_FILE") || { fail 'cannot verify applied .env digest'; exit 1; }
actual_record=$(cat -- "$ENV_RECORD") || { fail 'cannot verify applied .env record'; exit 1; }
[[ "$actual_env_sha" == "$expected_env_sha" && "$actual_record" == "env_sha256=$expected_env_sha source_sha=$target_sha" ]] || {
  fail 'runner returned success without recording the applied .env digest'
  exit 1
}
actual_image_sha=$(docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$SERVICE") || {
  fail 'cannot verify live container revision after apply'
  exit 1
}
[[ "$actual_image_sha" == "$target_sha" ]] || { fail 'live container revision changed unexpectedly'; exit 1; }
actual_container_state=$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE") || {
  fail 'cannot verify live container state after apply'
  exit 1
}
[[ "$actual_container_state" == running ]] || { fail 'live container is not running after apply'; exit 1; }
actual_tailnet_value=$(read_tailnet_value) || { fail 'cannot inspect applied tailnet setting'; exit 1; }
[[ "$actual_tailnet_value" == 0 ]] || { fail 'live container does not have the requested tailnet value'; exit 1; }
public_budget_after=$(budget_fingerprint) || { fail 'cannot verify retained exchange budgets'; exit 1; }
[[ "$public_budget_after" == "$public_budget_before" ]] || {
  fail 'public, per-connection, or global exchange settings changed'
  exit 1
}
printf 'rollout=verified target_sha=%s env_sha256=%s env_record=verified tailnet_budget=0 other_budgets=unchanged\n' \
  "$target_sha" "$actual_env_sha"
