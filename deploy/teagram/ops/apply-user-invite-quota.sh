#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram
readonly STATE_DIR=$REPO/.state
readonly SERVICE=teagram-teagramd-1
readonly ENV_FILE=$REPO/.env
readonly ENV_RECORD=$STATE_DIR/rollout-env
readonly PREVIOUS_ENV_FILE=$STATE_DIR/.env.main-2844-before-user-invites
readonly RUNNER_DIR=/root/teagramd-rollout-runner
readonly RUNNER=$RUNNER_DIR/rollout-runner.sh
readonly BLOB_HELPER=$RUNNER_DIR/blob-transition-runner.py
readonly QUOTA_ENV=TG_USER_INVITE_QUOTA
readonly QUOTA=5
readonly ACCOUNT_AGE_ENV=TG_USER_INVITE_MIN_ACCOUNT_AGE
readonly EXPECTED_ROOT_UID=0
readonly IMAGE_REVISION_FORMAT='{{index .Config.Labels "org.opencontainers.image.revision"}}'
readonly CONTAINER_STATE_FORMAT='{{.State.Status}}'

fail() {
  printf 'user invite quota apply rejected: %s\n' "$1" >&2
  return 1
}

acquire_deploy_lock() {
  exec 9>/tmp/teagram-deploy.lock || return 1
  flock -x 9
}

release_deploy_lock() {
  flock -u 9 || return 1
  exec 9>&-
}

private_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "$EXPECTED_ROOT_UID:600" ]]
}

private_dir() {
  local path=$1
  [[ -d "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "$EXPECTED_ROOT_UID:700" ]]
}

safe_state_dir() {
  local path=$1 owner mode
  [[ -d "$path" && ! -L "$path" ]] || return 1
  owner=$(stat -c '%u' -- "$path") || return 1
  [[ "$owner" == "$EXPECTED_ROOT_UID" ]] || return 1
  mode=$(stat -c '%a' -- "$path") || return 1
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
  (( (8#$mode & 0022) == 0 ))
}

file_sha256() {
  local output
  output=$(sha256sum -- "$1") || return 1
  printf '%s' "${output%% *}"
}

dotenv_count() {
  local file=$1 name=$2
  awk -v name="$name" '
    index($0, "=") > 0 && substr($0, 1, index($0, "=") - 1) == name { count++ }
    END { print count + 0 }
  ' "$file"
}

container_value() {
  local name=$1 output
  output=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$SERVICE") || return 1
  awk -F= -v name="$name" '
    $1 == name { count++; value=substr($0, index($0, "=") + 1) }
    END { if (count > 1) exit 1; if (count == 1) printf "%s", value }
  ' <<< "$output"
}

registration_fingerprint() {
  local output
  output=$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$SERVICE") || return 1
  printf '%s\n' "$output" | awk -F= '
    $1 == "TG_REGISTRATION" ||
    $1 == "TG_PUBLIC_REGISTRATION" ||
    $1 == "TG_WEBSOCKET_LISTEN_ADDR" ||
    $1 == "TG_PUBLIC_WEBSOCKET_LISTEN_ADDR" ||
    $1 == "TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR" { print }
  ' | LC_ALL=C sort | sha256sum | cut -d' ' -f1
}

run_app_config_probe() {
  local image_id=$1
  if docker run --rm --network host --read-only --cap-drop ALL \
    --security-opt no-new-privileges --user 65532:65532 \
    --entrypoint /usr/local/bin/mtproto-smoke "$image_id" \
    --expect-user-invite-quota "$QUOTA" > "$TEMP_DIR/app-config.log" 2>&1 && \
      grep -q "registration_mode=invite user_invites_enabled=true user_invite_quota=$QUOTA" \
        "$TEMP_DIR/app-config.log"; then
    return 0
  fi
  if ! cat -- "$TEMP_DIR/app-config.log" >&2; then
    printf '%s\n' 'user invite apply: app-config probe output is unavailable' >&2
  fi
  return 1
}

verify_existing_rollout() {
  local recorded_source_sha=$1 recorded_env_sha=$2 current_env_sha=$3
  local branch baseline_sha origin_sha live_image_sha container_state registration_mode
  local websocket_listen_addr public_websocket_listen_addr account_age_setting live_quota_setting
  local compose_selection expected_env_sha image_id

  [[ "$current_env_sha" == "$recorded_env_sha" ]] || {
    fail 'live .env differs from the verified rollout record'
    return 1
  }
  private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is unsafe'; return 1; }
  [[ "$(dotenv_count "$PREVIOUS_ENV_FILE" "$QUOTA_ENV")" == 0 && \
      "$(dotenv_count "$PREVIOUS_ENV_FILE" "$ACCOUNT_AGE_ENV")" == 0 ]] || {
    fail 'pre-change .env backup contains invite settings'
    return 1
  }
  TEMP_ENV=$(mktemp "$REPO/.env.main-2844.XXXXXXXX") || {
    fail 'cannot prepare the expected invite environment for verification'
    return 1
  }
  if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || \
     ! printf '%s=%s\n' "$QUOTA_ENV" "$QUOTA" >> "$TEMP_ENV" || \
     ! chmod 600 -- "$TEMP_ENV" || ! sync; then
    fail 'cannot prepare the expected invite environment for verification'
    return 1
  fi
  expected_env_sha=$(file_sha256 "$TEMP_ENV") || {
    fail 'cannot hash the expected invite environment'
    return 1
  }
  if ! cmp -s -- "$TEMP_ENV" "$ENV_FILE" || [[ "$expected_env_sha" != "$recorded_env_sha" ]]; then
    fail 'live .env is not the verified invite-only addition'
    return 1
  fi
  rm -- "$TEMP_ENV" || { fail 'cannot remove the temporary verification environment'; return 1; }
  TEMP_ENV=

  branch=$(git -C "$REPO" branch --show-current) || { fail 'cannot read live checkout branch'; return 1; }
  baseline_sha=$(git -C "$REPO" rev-parse --verify 'HEAD^{commit}') || { fail 'cannot read live checkout SHA'; return 1; }
  [[ "$branch" == main || -z "$branch" ]] || { fail 'live checkout is not on main'; return 1; }
  [[ "$OPS_SHA" == "$baseline_sha" ]] || { fail 'reviewed verification source does not match live main'; return 1; }
  live_image_sha=$(docker inspect --format "$IMAGE_REVISION_FORMAT" "$SERVICE") || {
    fail 'cannot read live container revision for verification'
    return 1
  }
  [[ "$live_image_sha" =~ ^[0-9a-f]{40}$ && "$live_image_sha" == "$recorded_source_sha" ]] || {
    fail 'live container revision does not match the verified rollout record'
    return 1
  }
  git -C "$REPO" fetch -q origin main || { fail 'cannot refresh origin/main'; return 1; }
  origin_sha=$(git -C "$REPO" rev-parse --verify 'origin/main^{commit}') || {
    fail 'cannot read origin/main SHA'
    return 1
  }
  git -C "$REPO" merge-base --is-ancestor "$live_image_sha" "$OPS_SHA" || {
    fail 'live image is not an ancestor of the reviewed verification source'
    return 1
  }
  git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$origin_sha" || {
    fail 'reviewed verification source is no longer on origin/main'
    return 1
  }

  container_state=$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE") || {
    fail 'cannot read live container state'
    return 1
  }
  [[ "$container_state" == running ]] || { fail 'live container is not running'; return 1; }
  registration_mode=$(container_value TG_REGISTRATION) || {
    fail 'cannot inspect default listener registration mode'
    return 1
  }
  [[ "$registration_mode" == invite ]] || { fail 'default listener is not in invite mode'; return 1; }
  websocket_listen_addr=$(container_value TG_WEBSOCKET_LISTEN_ADDR) || {
    fail 'cannot inspect additional WebSocket listener settings'
    return 1
  }
  public_websocket_listen_addr=$(container_value TG_PUBLIC_WEBSOCKET_LISTEN_ADDR) || {
    fail 'cannot inspect additional WebSocket listener settings'
    return 1
  }
  [[ -z "$websocket_listen_addr" && -z "$public_websocket_listen_addr" ]] || {
    fail 'an additional WebSocket listener needs its own app-config verification'
    return 1
  }
  account_age_setting=$(container_value "$ACCOUNT_AGE_ENV") || {
    fail 'cannot inspect live account age setting'
    return 1
  }
  [[ -z "$account_age_setting" ]] || { fail 'live account age setting is not using the 7d default'; return 1; }
  live_quota_setting=$(container_value "$QUOTA_ENV") || {
    fail 'cannot inspect live user invite quota'
    return 1
  }
  [[ "$live_quota_setting" == "$QUOTA" ]] || { fail 'live container does not have the requested quota'; return 1; }
  compose_selection=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$SERVICE") || {
    fail 'cannot read live Compose selection'
    return 1
  }
  [[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || {
    fail 'live Compose selection is not the reviewed teagram S3 stack'
    return 1
  }

  TEMP_DIR=$(mktemp -d /tmp/teagram-user-invite-env-apply.XXXXXXXX) || {
    fail 'cannot create private runner output directory'
    return 1
  }
  [[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%u:%a' -- "$TEMP_DIR")" == "$EXPECTED_ROOT_UID:700" ]] || {
    fail 'runner output directory failed ownership or mode checks'
    return 1
  }
  image_id=$(docker inspect --format '{{.Image}}' "$SERVICE") || { fail 'cannot read the live image ID'; return 1; }
  run_app_config_probe "$image_id" || {
    fail 'help.getAppConfig did not report user invites enabled with the configured quota'
    return 1
  }
  printf 'verification=passed source_sha=%s env_sha256=%s invite_listener=verified quota=%s account_age=7d-default\n' \
    "$live_image_sha" "$recorded_env_sha" "$QUOTA"
}

TEMP_ENV=
TEMP_DIR=
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$TEMP_ENV" ]]; then
    case "$TEMP_ENV" in
      "$REPO"/.env.main-2844.????????) rm -f -- "$TEMP_ENV" || status=1 ;;
      *) printf '%s\n' 'user invite apply: refused cleanup of an unexpected environment path' >&2; status=1 ;;
    esac
  fi
  if [[ -n "$TEMP_DIR" ]]; then
    case "$TEMP_DIR" in
      /tmp/teagram-user-invite-env-apply.????????) rm -rf -- "$TEMP_DIR" || status=1 ;;
      *) printf '%s\n' 'user invite apply: refused cleanup of an unexpected temporary directory' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}
trap cleanup EXIT

MODE=apply
if [[ "$#" -eq 1 && "$1" == verify ]]; then
  MODE=verify
elif [[ "$#" -ne 0 ]]; then
  printf '%s\n' 'usage: apply-user-invite-quota [verify]' >&2
  exit 64
fi
[[ "$(id -u)" == "$EXPECTED_ROOT_UID" ]] || { fail 'operation requires root'; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { fail 'reviewed operation SHA is invalid'; exit 64; }
[[ "$(realpath -e -- "$REPO")" == "$REPO" ]] || { fail 'live checkout is missing or symlinked'; exit 1; }
cd "$REPO" || { fail 'cannot enter the live checkout'; exit 1; }
safe_state_dir "$STATE_DIR" || { fail 'deployment state directory failed ownership or mode checks'; exit 1; }
private_file "$ENV_FILE" || { fail 'live .env is not a root-owned mode-0600 regular file'; exit 1; }
private_file "$ENV_RECORD" || { fail 'verified .env record is not a root-owned mode-0600 regular file'; exit 1; }
private_file "$RUNNER" || { fail 'reviewed rollout runner is unavailable or unsafe'; exit 1; }
private_file "$BLOB_HELPER" || { fail 'reviewed blob authority helper is unavailable or unsafe'; exit 1; }
acquire_deploy_lock || { fail 'cannot acquire the teagram deploy lock'; exit 1; }

record=$(cat -- "$ENV_RECORD") || { fail 'cannot read verified .env record'; exit 1; }
[[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || {
  fail 'verified .env record is malformed'
  exit 1
}
recorded_env_sha=${BASH_REMATCH[1]}
recorded_source_sha=${BASH_REMATCH[2]}
current_env_sha=$(file_sha256 "$ENV_FILE") || { fail 'cannot hash live .env'; exit 1; }
if [[ "$MODE" == verify ]]; then
  verify_existing_rollout "$recorded_source_sha" "$recorded_env_sha" "$current_env_sha" || exit 1
  exit 0
fi
[[ "$current_env_sha" == "$recorded_env_sha" ]] || { fail 'live .env differs from the last verified record'; exit 1; }
[[ "$(dotenv_count "$ENV_FILE" "$QUOTA_ENV")" == 0 ]] || { fail 'user invite quota already exists in the live .env'; exit 1; }
[[ "$(dotenv_count "$ENV_FILE" "$ACCOUNT_AGE_ENV")" == 0 ]] || { fail 'account age is explicitly set instead of using the 7d default'; exit 1; }

if [[ -e "$PREVIOUS_ENV_FILE" || -L "$PREVIOUS_ENV_FILE" ]]; then
  private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is unsafe'; exit 1; }
  cmp -s -- "$ENV_FILE" "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup does not match the verified live file'; exit 1; }
else
  if ! cp -- "$ENV_FILE" "$PREVIOUS_ENV_FILE" || ! chmod 600 -- "$PREVIOUS_ENV_FILE" || ! sync; then
    fail 'cannot save the pre-change .env in the private deployment state directory'
    exit 1
  fi
fi
private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup failed ownership or mode checks'; exit 1; }
[[ "$(file_sha256 "$PREVIOUS_ENV_FILE")" == "$recorded_env_sha" ]] || { fail 'pre-change .env backup does not match its verified digest'; exit 1; }

branch=$(git -C "$REPO" branch --show-current) || { fail 'cannot read live checkout branch'; exit 1; }
baseline_sha=$(git -C "$REPO" rev-parse --verify 'HEAD^{commit}') || { fail 'cannot read live checkout SHA'; exit 1; }
[[ "$branch" == main || -z "$branch" ]] || { fail 'live checkout is not on main'; exit 1; }
target_sha=$(docker inspect --format "$IMAGE_REVISION_FORMAT" "$SERVICE") || { fail 'cannot read live container revision'; exit 1; }
[[ "$target_sha" =~ ^[0-9a-f]{40}$ && "$target_sha" == "$baseline_sha" && "$OPS_SHA" == "$baseline_sha" ]] || {
  fail 'live checkout, image, and reviewed operation revisions do not match'
  exit 1
}
container_state=$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE") || { fail 'cannot read live container state'; exit 1; }
[[ "$container_state" == running ]] || { fail 'live container is not running'; exit 1; }
registration_mode=$(container_value TG_REGISTRATION) || { fail 'cannot read default listener registration mode'; exit 1; }
[[ "$registration_mode" == invite ]] || { fail 'default listener is not in invite mode'; exit 1; }
websocket_listen_addr=$(container_value TG_WEBSOCKET_LISTEN_ADDR) || {
  fail 'cannot inspect additional WebSocket listener settings'
  exit 1
}
public_websocket_listen_addr=$(container_value TG_PUBLIC_WEBSOCKET_LISTEN_ADDR) || {
  fail 'cannot inspect additional WebSocket listener settings'
  exit 1
}
[[ -z "$websocket_listen_addr" && -z "$public_websocket_listen_addr" ]] || {
  fail 'an additional WebSocket listener is active and needs its own app-config verification'
  exit 1
}
account_age_setting=$(container_value "$ACCOUNT_AGE_ENV") || {
  fail 'cannot inspect live account age setting'
  exit 1
}
[[ -z "$account_age_setting" ]] || { fail 'live account age setting is not using the 7d default'; exit 1; }
live_quota_setting=$(container_value "$QUOTA_ENV") || {
  fail 'cannot inspect live user invite quota'
  exit 1
}
[[ -z "$live_quota_setting" ]] || { fail 'user invite quota is already present in the live container'; exit 1; }
registration_before=$(registration_fingerprint) || { fail 'cannot fingerprint listener registration settings'; exit 1; }

compose_selection=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$SERVICE") || {
  fail 'cannot read live Compose selection'
  exit 1
}
[[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || {
  fail 'live Compose selection is not the reviewed teagram S3 stack'
  exit 1
}
export COMPOSE_FILE="$REPO/docker-compose.yml:$REPO/docker-compose.override.yml"
git -C "$REPO" fetch -q origin main || { fail 'cannot refresh origin/main'; exit 1; }
origin_sha=$(git -C "$REPO" rev-parse --verify 'origin/main^{commit}') || { fail 'cannot read origin/main SHA'; exit 1; }
git -C "$REPO" merge-base --is-ancestor "$target_sha" "$origin_sha" || { fail 'live target is no longer on origin/main'; exit 1; }
git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$origin_sha" || { fail 'reviewed operation source is no longer on origin/main'; exit 1; }

authority_json=$(python3 "$BLOB_HELPER" deployment-env \
  --checkout "$REPO" --state-dir "$STATE_DIR/blob-mode" \
  --report-root /root --lock-path /tmp/teagram-deploy.lock) || {
  fail 'live S3 authority or credential selection is not verified'
  exit 1
}
env_file_lines=$(jq -er '
  select(.outcome == "s3-accepted") | .env_files |
  if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
  then .[] else error("invalid environment file list") end
' <<< "$authority_json") || { fail 'live S3 environment file selection is invalid'; exit 1; }
mapfile -t compose_env_files <<< "$env_file_lines"
[[ "${#compose_env_files[@]}" == 2 && "${compose_env_files[0]}" == "$ENV_FILE" ]] || {
  fail 'live S3 environment file selection does not start with the verified .env'
  exit 1
}
legacy_parked="$(dirname "$REPO")/telegram-server/.env.rustfs-parked"
case "${compose_env_files[1]}" in
  "$REPO/.env.rustfs-parked"|"$legacy_parked"|/root/.env.rustfs-parked) ;;
  *) fail 'live S3 credential file path is not approved'; exit 1 ;;
esac
export COMPOSE_ENV_FILES="${compose_env_files[0]},${compose_env_files[1]}"

TEMP_DIR=$(mktemp -d /tmp/teagram-user-invite-env-apply.XXXXXXXX) || { fail 'cannot create private runner output directory'; exit 1; }
[[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%u:%a' -- "$TEMP_DIR")" == "$EXPECTED_ROOT_UID:700" ]] || {
  fail 'runner output directory failed ownership or mode checks'
  exit 1
}
TEMP_ENV=$(mktemp "$REPO/.env.main-2844.XXXXXXXX") || { fail 'cannot stage the named .env addition'; exit 1; }
if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || ! printf '%s=%s\n' "$QUOTA_ENV" "$QUOTA" >> "$TEMP_ENV" || \
   ! chmod 600 -- "$TEMP_ENV" || ! sync; then
  fail 'cannot prepare the named .env addition'
  exit 1
fi
expected_env_sha=$(file_sha256 "$TEMP_ENV") || { fail 'cannot hash staged .env'; exit 1; }
mv -f -- "$TEMP_ENV" "$ENV_FILE" || { fail 'cannot atomically publish the named .env addition'; exit 1; }
TEMP_ENV=
sync || { fail 'cannot sync the named .env addition'; exit 1; }
release_deploy_lock || { fail 'cannot release the deploy lock for the rollout runner'; exit 1; }

runner_status=0
if env \
  ROLLOUT_RUNNER_SOURCE_SHA="$OPS_SHA" \
  ROLLOUT_RUNNER_ADDED_ENV="$QUOTA_ENV" \
  ROLLOUT_RUNNER_PREVIOUS_ENV_FILE="$PREVIOUS_ENV_FILE" \
  bash "$RUNNER" apply "$target_sha" "$baseline_sha" > "$TEMP_DIR/runner.log" 2>&1; then
  :
else
  runner_status=$?
fi
acquire_deploy_lock || { fail 'cannot reacquire the deploy lock after rollout'; exit 1; }

if [[ "$runner_status" -ne 0 ]]; then
  failed_env_sha=$(file_sha256 "$ENV_FILE" 2>/dev/null || true)
  failed_record=$(cat -- "$ENV_RECORD" 2>/dev/null || true)
  rollback_container_verified=0
  failed_image_sha=
  failed_container_state=
  failed_live_quota=
  if failed_image_sha=$(docker inspect --format "$IMAGE_REVISION_FORMAT" "$SERVICE" 2>/dev/null) && \
     failed_container_state=$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE" 2>/dev/null) && \
     failed_live_quota=$(container_value "$QUOTA_ENV" 2>/dev/null); then
    rollback_container_verified=1
  fi
  if [[ "$failed_env_sha" == "$recorded_env_sha" && "$failed_record" == "$record" ]]; then
    printf 'runner_result=failed exit=%s target_sha=%s rollback_env=verified\n' "$runner_status" "$target_sha"
  elif [[ "$failed_env_sha" == "$expected_env_sha" && "$failed_record" == "$record" && \
      "$rollback_container_verified" == 1 && "$failed_image_sha" == "$target_sha" && \
      "$failed_container_state" == running && "$failed_live_quota" == '' ]]; then
    TEMP_ENV=$(mktemp "$REPO/.env.main-2844.XXXXXXXX") || {
      printf 'runner_result=failed exit=%s target_sha=%s rollback_env=unverified\n' "$runner_status" "$target_sha"
      exit "$runner_status"
    }
    if cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" && chmod 600 -- "$TEMP_ENV" && sync && \
       mv -f -- "$TEMP_ENV" "$ENV_FILE" && sync; then
      TEMP_ENV=
      if [[ "$(file_sha256 "$ENV_FILE" 2>/dev/null || true)" == "$recorded_env_sha" && \
          "$(cat -- "$ENV_RECORD" 2>/dev/null || true)" == "$record" ]]; then
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
  exit "$runner_status"
fi

actual_env_sha=$(file_sha256 "$ENV_FILE") || { fail 'cannot verify applied .env digest'; exit 1; }
actual_record=$(cat -- "$ENV_RECORD") || { fail 'cannot verify applied .env record'; exit 1; }
actual_image_sha=$(docker inspect --format "$IMAGE_REVISION_FORMAT" "$SERVICE") || { fail 'cannot verify live container revision'; exit 1; }
[[ "$actual_image_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'live image revision is invalid after apply'; exit 1; }
git -C "$REPO" fetch -q origin main || { fail 'cannot refresh origin/main after apply'; exit 1; }
origin_sha=$(git -C "$REPO" rev-parse --verify 'origin/main^{commit}') || { fail 'cannot read origin/main after apply'; exit 1; }
git -C "$REPO" merge-base --is-ancestor "$target_sha" "$actual_image_sha" || {
  fail 'live image no longer contains the reviewed invite release'
  exit 1
}
git -C "$REPO" merge-base --is-ancestor "$actual_image_sha" "$origin_sha" || {
  fail 'live image is not on origin/main after apply'
  exit 1
}
[[ "$actual_env_sha" == "$expected_env_sha" && "$actual_record" == "env_sha256=$expected_env_sha source_sha=$actual_image_sha" ]] || {
  fail 'runner returned success without recording the applied .env digest'
  exit 1
}
[[ "$(docker inspect --format "$CONTAINER_STATE_FORMAT" "$SERVICE")" == running ]] || {
  fail 'live container is not running after apply'
  exit 1
}
actual_quota_setting=$(container_value "$QUOTA_ENV") || { fail 'cannot verify live user invite quota after apply'; exit 1; }
[[ "$actual_quota_setting" == "$QUOTA" ]] || { fail 'live container does not have the requested quota'; exit 1; }
actual_account_age_setting=$(container_value "$ACCOUNT_AGE_ENV") || {
  fail 'cannot verify live account age setting after apply'
  exit 1
}
[[ -z "$actual_account_age_setting" ]] || { fail 'live account age setting changed from the 7d default'; exit 1; }
[[ "$(registration_fingerprint)" == "$registration_before" ]] || { fail 'listener registration settings changed'; exit 1; }
actual_registration_mode=$(container_value TG_REGISTRATION) || { fail 'cannot verify default listener registration mode after apply'; exit 1; }
[[ "$actual_registration_mode" == invite ]] || { fail 'default listener left invite mode'; exit 1; }

image_id=$(docker inspect --format '{{.Image}}' "$SERVICE") || { fail 'cannot read the live image ID'; exit 1; }
if ! run_app_config_probe "$image_id"; then
  fail 'help.getAppConfig did not report user invites enabled with the configured quota'
  exit 1
fi

printf 'rollout=verified target_sha=%s env_sha256=%s invite_listener=verified quota=%s account_age=7d-default other_listeners=none\n' \
  "$actual_image_sha" "$actual_env_sha" "$QUOTA"
