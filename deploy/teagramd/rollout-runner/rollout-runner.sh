#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT_SOURCE="$SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")"
readonly APPROVED_VERIFIER_SHA=21850be8f3758e31201895fe58342d234052c22adb2f48d75f180b02addef40e
readonly APPROVED_SCHEMA_GATE_SHA=ac54d3cf0480383a52414a8bc8b856b5e1d5f6f8d19034c9c576c64627e097c8
readonly APPROVED_SCHEMA_GATE_HELPER_SHA=7ae06c2872c60b434068b3715a9ddd5e19b03d4be98dc9881c8dd8f294953d41
readonly APPROVED_INITIAL_LOCAL_COMPOSE_SHA=d764d291ad2ea91cff841d14a0cd2bbcb5981961dbc67406bfeba1196a7b3fa4
readonly APPROVED_LOCAL_COMPOSE_CONTENT_SHA=d025a25d3e388489ddaf181e3d38b4ecd56906aeab75a9f9fbabdc38e017cfdb
readonly SOURCE_LOCAL_COMPOSE_TARGET_SHA=5602c7c13eb1571278f43129345a4b52f0630377
readonly SOURCE_LOCAL_COMPOSE_SHA=05fc5a3e7dbdc2feac58fce870796baebc055b617bc81f6d6b206c5a2c184cc5
readonly SOURCE_LOCAL_COMPOSE_CONTENT_SHA=577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd
readonly SOURCE_LOCAL_COMPOSE_SOURCE_FILE=deploy/teagramd/rollout-runner/local-compose-5602c7c.yml
readonly SOURCE_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-5602c7c.yml
readonly E58_LOCAL_COMPOSE_TARGET_SHA=e58ba203505a37bf5ad3181f22e59a934d4a7442
readonly E58_LOCAL_COMPOSE_SHA=d3426792677d0510a75dd254fbbd5fd9d9b19a01d710ec5b9b59f3e8a31447c7
readonly E58_LOCAL_COMPOSE_SOURCE_FILE=deploy/teagramd/rollout-runner/local-compose-e58ba20.yml
readonly E58_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-e58ba20.yml
readonly TARGET_LOCAL_COMPOSE_TARGET_SHA=598359e900130c307dd84022b0077fd3276f6f36
readonly TARGET_LOCAL_COMPOSE_SHA=714d7f870645e2581234feba81a86e9e529facd0b664b32b2f429eb583dc754f
readonly TARGET_LOCAL_COMPOSE_SOURCE_FILE=deploy/teagramd/rollout-runner/local-compose-598359e.yml
readonly TARGET_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-598359e.yml
readonly DF9_LOCAL_COMPOSE_TARGET_SHA=df9ffe538defd4b99bd9edb2405caacddd1aa1f6
readonly DF9_LOCAL_COMPOSE_SHA=b48e1bc4727b9ed5e05d247fb7f5e3424eca8c1f9b496727e56127b67f7a5ce1
readonly DF9_LOCAL_COMPOSE_SOURCE_FILE=deploy/teagramd/rollout-runner/local-compose-df9ffe5.yml
readonly DF9_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-df9ffe5.yml
readonly CCE_LOCAL_COMPOSE_TARGET_SHA=cce00d185e8e7734b482f116e465647f6ec1fa26
readonly CCE_LOCAL_COMPOSE_SHA=72a91a77db188f106b0062420722166436d38c6cb1974b73b9dd2873fbc5c11c
readonly CCE_LOCAL_COMPOSE_CONTENT_SHA=577dd9885aa9babd3370a97417d1653884f0567c6f50e7feb208ca9d11ca15bd
readonly CCE_LOCAL_COMPOSE_SOURCE_FILE=deploy/teagramd/rollout-runner/local-compose-cce00d1.yml
readonly CCE_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-cce00d1.yml
readonly INITIAL_LOCAL_COMPOSE_TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
readonly INITIAL_LOCAL_COMPOSE_FILE=.rollout-compose.initial-local.yml
readonly INITIAL_LOCAL_LEGACY_BASELINE_SHA=932994e26a86eb1c9ad60f81b3d222b19d3f40b7
readonly PROJECT_MOVE_SOURCE=telegram-server
readonly PROJECT_MOVE_TARGET=teagram
readonly PROJECT_MOVE_PG_IMAGE=postgres:18-alpine
PINNED_LOCAL_COMPOSE_TARGET_SHA=
PINNED_LOCAL_COMPOSE_SHA=
PINNED_LOCAL_COMPOSE_CONTENT_SHA=
PINNED_LOCAL_COMPOSE_SOURCE_FILE=
PINNED_LOCAL_COMPOSE_RUNTIME_FILE=
PINNED_LOCAL_COMPOSE_CARRIED=0
TRACKED_LOCAL_AUTHORITY=0
TRACKED_LOCAL_BLOB_DIR=
BUILD_IDENTITY_REJECTION_REASON=built-image-id-invalid

ROLLOUT_RUNNER_TEST_MODE=${ROLLOUT_RUNNER_TEST_MODE:-0}
ROLLOUT_PINNED_EXECUTION=${ROLLOUT_PINNED_EXECUTION:-0}
INITIALIZE_LOCAL=0
ROLLOUT_COMPOSE_SERVICE=teagramd
INITIAL_LOCAL_ARTIFACT_SHA=
RUNNER_ACTION=apply
TARGET_DATABASE=
REMOVED_PUBLIC_PROFILE_SERVICES=()
CHECKOUT=${ROLLOUT_RUNNER_CHECKOUT:-/opt/teagram}
TARGET_CHECKOUT=${ROLLOUT_RUNNER_TARGET_CHECKOUT:-/opt/teagram}
EVIDENCE_ROOT=${ROLLOUT_RUNNER_EVIDENCE_ROOT:-/root}
PREFLIGHT_EVIDENCE_OWNER=0
if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
  LEGACY_LOG_ARCHIVE=${ROLLOUT_RUNNER_TEST_LOG_ARCHIVE:-}
else
  LEGACY_LOG_ARCHIVE=/root/teagramd-legacy-logs.log
fi
if [ "${ROLLOUT_RUNNER_TEST_MODE:-0}" = 1 ]; then
  LOCK_PATH=${ROLLOUT_RUNNER_LOCK_PATH:-/tmp/teagram-deploy.lock}
  TARGET_LOCK_PATH=${ROLLOUT_RUNNER_TARGET_LOCK_PATH:-/tmp/teagram-deploy.lock}
  SOURCE_LOCK_PATH=${ROLLOUT_RUNNER_SOURCE_LOCK_PATH:-/tmp/telegram-server-deploy.lock}
else
  LOCK_PATH=/tmp/teagram-deploy.lock
  TARGET_LOCK_PATH=/tmp/teagram-deploy.lock
  SOURCE_LOCK_PATH=/tmp/telegram-server-deploy.lock
fi
ENV_FILE=${ROLLOUT_RUNNER_ENV_FILE:-/opt/teagram/.env}
OVERRIDE_FILE=${ROLLOUT_RUNNER_OVERRIDE_FILE:-/opt/teagram/docker-compose.override.yml}
ADDED_ENV=${ROLLOUT_RUNNER_ADDED_ENV:-}
CHANGED_ENV=${ROLLOUT_RUNNER_CHANGED_ENV:-}
PREVIOUS_ENV_FILE=${ROLLOUT_RUNNER_PREVIOUS_ENV_FILE:-}
PREVIOUS_OVERRIDE_FILE=${ROLLOUT_RUNNER_PREVIOUS_OVERRIDE_FILE:-}
CHANGED_ENV_SOURCE=dotenv
if [ -n "$PREVIOUS_OVERRIDE_FILE" ]; then CHANGED_ENV_SOURCE=override; fi
RECORDED_OVERRIDE_SHA=
RECORDED_OVERRIDE_FROM=
ADOPT_ENV=${ROLLOUT_RUNNER_ADOPT_ENV:-0}
ENV_RECORD="$CHECKOUT/.state/rollout-env"
RECORDED_ENV_SHA=
RECORDED_ENV_FROM=
READY_SECONDS=${ROLLOUT_RUNNER_READY_SECONDS:-120}
if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
  VERIFIER="$SCRIPT_DIR/rollout-verifier.pinned"
  SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.pinned"
  SCHEMA_GATE_HELPER="$SCRIPT_DIR/schema-result-gate.py"
  MODE_HELPER="$SCRIPT_DIR/blob-mode-state.pinned"
  STACK_MOVE_HELPER="$SCRIPT_DIR/stack-move-runner.pinned"
else
  VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
  SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
  SCHEMA_GATE_HELPER="$SCRIPT_DIR/schema-result-gate.py"
  MODE_HELPER="$SCRIPT_DIR/blob-mode-state.py"
  STACK_MOVE_HELPER="$SCRIPT_DIR/stack-move-runner.py"
fi
QUALIFIER_HELPER="$SCRIPT_DIR/qualify-rustfs-transition.py"

fail() {
  printf 'rollout runner rejected: %s\n' "$1" >&2
  return 1
}

configure_action_paths() {
  local action=$1
  RUNNER_ACTION=$action
  if [ -n "${ROLLOUT_RUNNER_CHECKOUT:-}" ]; then
    CHECKOUT=$ROLLOUT_RUNNER_CHECKOUT
  elif [ "$action" = move-stack ]; then
    CHECKOUT="/opt/$PROJECT_MOVE_SOURCE"
  else
    CHECKOUT=/opt/teagram
  fi
  if [ -n "${ROLLOUT_RUNNER_TARGET_CHECKOUT:-}" ]; then
    TARGET_CHECKOUT=$ROLLOUT_RUNNER_TARGET_CHECKOUT
  elif [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    TARGET_CHECKOUT="${CHECKOUT%/*}/teagram-target"
  else
    TARGET_CHECKOUT=/opt/teagram
  fi
  ENV_FILE=${ROLLOUT_RUNNER_ENV_FILE:-$CHECKOUT/.env}
  OVERRIDE_FILE=${ROLLOUT_RUNNER_OVERRIDE_FILE:-$CHECKOUT/docker-compose.override.yml}
  ENV_RECORD="$CHECKOUT/.state/rollout-env"
}

restore_database_dump() {
  local container=${1:-} database=${2:-} globals_path=${3:-} dump_path=${4:-}
  [ "$#" -eq 4 ] || { fail 'isolated restore requires a container, database, globals, and dump'; return 64; }
  [[ "$container" =~ ^main1238-restore-[A-Za-z0-9-]+$ ]] || { fail 'isolated restore container name is invalid'; return 64; }
  [[ "$database" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || { fail 'isolated restore database name is invalid'; return 64; }
  [ -f "$globals_path" ] && [ ! -L "$globals_path" ] || { fail 'isolated restore globals are missing or symlinked'; return 1; }
  [ -f "$dump_path" ] && [ ! -L "$dump_path" ] || { fail 'isolated restore dump is missing or symlinked'; return 1; }
  sed -E '/^CREATE ROLE (postgres|"postgres");$/d' "$globals_path" | \
    docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres || return $?
  docker exec -i "$container" psql -X -v ON_ERROR_STOP=1 -U postgres -d "$database" < "$dump_path" || return $?
}

restore_container_present() {
  local container=$1 stderr_file=$2 inspection_error inspect_rc
  if inspection_error=$(docker inspect --type container "$container" 2>&1 >/dev/null); then
    return 0
  else
    inspect_rc=$?
  fi
  if [ "$inspect_rc" -eq 1 ]; then
    case "$inspection_error" in
      "Error: No such object: $container"|"Error: No such container: $container"|"Error response from daemon: No such container: $container") return 1 ;;
    esac
  fi
  printf 'container inspection failed (exit=%s): %s\n' \
    "$inspect_rc" "${inspection_error:-no diagnostic output}" >> "$stderr_file"
  return 2
}

cleanup_restore_container() {
  local container=$1 stderr_file=$2 removal_rc inspect_rc
  if docker rm -f "$container" >/dev/null 2>>"$stderr_file"; then
    removal_rc=0
  else
    removal_rc=$?
    printf 'container removal failed (exit=%s)\n' "$removal_rc" >> "$stderr_file"
  fi
  if restore_container_present "$container" "$stderr_file"; then
    inspect_rc=0
    printf 'container remains after removal\n' >> "$stderr_file"
  else
    inspect_rc=$?
  fi
  [ "$removal_rc" -eq 0 ] && [ "$inspect_rc" -eq 1 ]
}

usage() {
  printf '%s\n' 'usage: rollout-runner.sh apply|initialize-local TARGET_SHA EXPECTED_BASELINE_SHA' >&2
  printf '%s\n' '       rollout-runner.sh move-stack TARGET_SHA EXPECTED_BASELINE_SHA' >&2
  printf '%s\n' '       rollout-runner.sh reconcile TARGET_SHA' >&2
  printf '%s\n' '       rollout-runner.sh compare-resolved-preflight EVIDENCE_DIR' >&2
}

sha256_file() {
  local output
  output=$(sha256sum -- "$1") || return 1
  printf '%s' "${output%% *}"
}

sha256_target_file() {
  local revision=$1 tracked_path=$2 output
  output=$(git -C "$CHECKOUT" show "$revision:$tracked_path" | sha256sum) || return 1
  printf '%s' "${output%% *}"
}

sha256_without_comment_lines() {
  local output
  output=$(awk '!/^[[:space:]]*#/' "$1" | sha256sum) || return 1
  printf '%s' "${output%% *}"
}

sha256_target_file_without_comment_lines() {
  local revision=$1 tracked_path=$2 output
  output=$(git -C "$CHECKOUT" show "$revision:$tracked_path" | awk '!/^[[:space:]]*#/' | sha256sum) || return 1
  printf '%s' "${output%% *}"
}

set_target_local_compose_pin() {
  local first_entry artifact_path checkout_path
  PINNED_LOCAL_COMPOSE_TARGET_SHA=
  PINNED_LOCAL_COMPOSE_SHA=
  PINNED_LOCAL_COMPOSE_CONTENT_SHA=
  PINNED_LOCAL_COMPOSE_SOURCE_FILE=
  PINNED_LOCAL_COMPOSE_RUNTIME_FILE=
  PINNED_LOCAL_COMPOSE_CARRIED=0
  case "$TARGET_SHA" in
    "$SOURCE_LOCAL_COMPOSE_TARGET_SHA")
      PINNED_LOCAL_COMPOSE_TARGET_SHA=$SOURCE_LOCAL_COMPOSE_TARGET_SHA
      PINNED_LOCAL_COMPOSE_SHA=$SOURCE_LOCAL_COMPOSE_SHA
      PINNED_LOCAL_COMPOSE_CONTENT_SHA=$SOURCE_LOCAL_COMPOSE_CONTENT_SHA
      PINNED_LOCAL_COMPOSE_SOURCE_FILE=$SOURCE_LOCAL_COMPOSE_SOURCE_FILE
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=$SOURCE_LOCAL_COMPOSE_RUNTIME_FILE
      ;;
    "$E58_LOCAL_COMPOSE_TARGET_SHA")
      PINNED_LOCAL_COMPOSE_TARGET_SHA=$E58_LOCAL_COMPOSE_TARGET_SHA
      PINNED_LOCAL_COMPOSE_SHA=$E58_LOCAL_COMPOSE_SHA
      PINNED_LOCAL_COMPOSE_CONTENT_SHA=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA
      PINNED_LOCAL_COMPOSE_SOURCE_FILE=$E58_LOCAL_COMPOSE_SOURCE_FILE
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=$E58_LOCAL_COMPOSE_RUNTIME_FILE
      ;;
    "$TARGET_LOCAL_COMPOSE_TARGET_SHA")
      PINNED_LOCAL_COMPOSE_TARGET_SHA=$TARGET_LOCAL_COMPOSE_TARGET_SHA
      PINNED_LOCAL_COMPOSE_SHA=$TARGET_LOCAL_COMPOSE_SHA
      PINNED_LOCAL_COMPOSE_CONTENT_SHA=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA
      PINNED_LOCAL_COMPOSE_SOURCE_FILE=$TARGET_LOCAL_COMPOSE_SOURCE_FILE
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=$TARGET_LOCAL_COMPOSE_RUNTIME_FILE
      ;;
    "$DF9_LOCAL_COMPOSE_TARGET_SHA")
      PINNED_LOCAL_COMPOSE_TARGET_SHA=$DF9_LOCAL_COMPOSE_TARGET_SHA
      PINNED_LOCAL_COMPOSE_SHA=$DF9_LOCAL_COMPOSE_SHA
      PINNED_LOCAL_COMPOSE_CONTENT_SHA=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA
      PINNED_LOCAL_COMPOSE_SOURCE_FILE=$DF9_LOCAL_COMPOSE_SOURCE_FILE
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=$DF9_LOCAL_COMPOSE_RUNTIME_FILE
      ;;
    "$CCE_LOCAL_COMPOSE_TARGET_SHA")
      PINNED_LOCAL_COMPOSE_TARGET_SHA=$CCE_LOCAL_COMPOSE_TARGET_SHA
      PINNED_LOCAL_COMPOSE_SHA=$CCE_LOCAL_COMPOSE_SHA
      PINNED_LOCAL_COMPOSE_CONTENT_SHA=$CCE_LOCAL_COMPOSE_CONTENT_SHA
      PINNED_LOCAL_COMPOSE_SOURCE_FILE=$CCE_LOCAL_COMPOSE_SOURCE_FILE
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=$CCE_LOCAL_COMPOSE_RUNTIME_FILE
      ;;
    *)
      [ "${COMPOSE_FILE+x}" = x ] && [ -n "$COMPOSE_FILE" ] || return 1
      IFS=: read -r first_entry _ <<< "$COMPOSE_FILE"
      case "${first_entry##*/}" in
        .rollout-compose.local-*.yml) ;;
        *) return 1 ;;
      esac
      artifact_path=$(canonical_compose_file_path "$first_entry") || {
        fail 'cannot resolve the carried-forward local Compose artifact'
        return 1
      }
      checkout_path=$(readlink -f "$CHECKOUT") || {
        fail 'cannot resolve the deployment checkout for the local Compose artifact'
        return 1
      }
      [ "$(dirname "$artifact_path")" = "$checkout_path" ] || {
        fail 'carried-forward local Compose artifact must be in the deployment checkout'
        return 1
      }
      local artifact_content_sha
      artifact_content_sha=$(sha256_without_comment_lines "$artifact_path") || {
        fail 'cannot hash carried-forward local Compose content'
        return 1
      }
      case "$artifact_content_sha" in
        "$APPROVED_LOCAL_COMPOSE_CONTENT_SHA")
          PINNED_LOCAL_COMPOSE_TARGET_SHA=$DF9_LOCAL_COMPOSE_TARGET_SHA
          PINNED_LOCAL_COMPOSE_CONTENT_SHA=$APPROVED_LOCAL_COMPOSE_CONTENT_SHA
          PINNED_LOCAL_COMPOSE_SOURCE_FILE=$DF9_LOCAL_COMPOSE_SOURCE_FILE
          ;;
        "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA")
          PINNED_LOCAL_COMPOSE_TARGET_SHA=$CCE_LOCAL_COMPOSE_TARGET_SHA
          PINNED_LOCAL_COMPOSE_CONTENT_SHA=$SOURCE_LOCAL_COMPOSE_CONTENT_SHA
          PINNED_LOCAL_COMPOSE_SOURCE_FILE=$CCE_LOCAL_COMPOSE_SOURCE_FILE
          ;;
        *)
          fail 'carried-forward local Compose content is not a reviewed contract'
          return 1
          ;;
      esac
      PINNED_LOCAL_COMPOSE_RUNTIME_FILE=${first_entry##*/}
      PINNED_LOCAL_COMPOSE_CARRIED=1
      ;;
  esac
}

verify_runtime_sources() {
  local -a tracked_paths source_paths
  local index source_sha target_sha runtime_source_sha
  runtime_source_sha=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  [[ "$runtime_source_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'rollout runtime source SHA must be a full commit ID'; return 1; }
  tracked_paths=(
    deploy/teagramd/rollout-runner/rollout-runner.sh
    deploy/teagramd/rollout-runner/rollout-verifier.sh
    deploy/teagramd/rollout-runner/schema-result-gate.sh
    deploy/teagramd/rollout-runner/schema-result-gate.py
    deploy/teagramd/rollout-runner/blob-mode-state.py
    deploy/teagramd/rollout-runner/qualify-rustfs-transition.py
    deploy/teagramd/rollout-runner/stack-move-runner.py
  )
  if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    source_paths=("$SCRIPT_SOURCE" "$VERIFIER" "$SCHEMA_GATE" "$SCHEMA_GATE_HELPER" "$MODE_HELPER" "$QUALIFIER_HELPER" "$STACK_MOVE_HELPER")
  else
    source_paths=("$SCRIPT_SOURCE" "$SCRIPT_DIR/rollout-verifier.sh" "$SCRIPT_DIR/schema-result-gate.sh" "$SCHEMA_GATE_HELPER" "$MODE_HELPER" "$QUALIFIER_HELPER" "$SCRIPT_DIR/stack-move-runner.py")
  fi
  if [ "$INITIALIZE_LOCAL" != 1 ] && set_target_local_compose_pin; then
    tracked_paths+=("$PINNED_LOCAL_COMPOSE_SOURCE_FILE")
    source_paths+=("$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE")
  fi
  for index in "${!tracked_paths[@]}"; do
    if [ "$PINNED_LOCAL_COMPOSE_CARRIED" = 1 ] && \
       [ "${tracked_paths[$index]}" = "$PINNED_LOCAL_COMPOSE_SOURCE_FILE" ]; then
      source_sha=$(sha256_without_comment_lines "${source_paths[$index]}") || {
        fail 'cannot hash carried-forward local Compose content'
        return 1
      }
      target_sha=$(sha256_target_file_without_comment_lines "$runtime_source_sha" "${tracked_paths[$index]}") || {
        fail "cannot read reviewed local Compose content from source revision: ${tracked_paths[$index]}"
        return 1
      }
      [ "$source_sha" = "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" ] && \
        [ "$target_sha" = "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" ] || {
        fail 'carried-forward local Compose content differs from the reviewed pin'
        return 1
      }
    else
      source_sha=$(sha256_file "${source_paths[$index]}") || { fail 'cannot hash a rollout runtime source'; return 1; }
      target_sha=$(sha256_target_file "$runtime_source_sha" "${tracked_paths[$index]}") || {
        fail "cannot read rollout runtime source from reviewed source revision: ${tracked_paths[$index]}"
        return 1
      }
    fi
    [ "$source_sha" = "$target_sha" ] || {
      fail "runtime copy differs from reviewed source revision: ${tracked_paths[$index]}"
      return 1
    }
  done
}

verify_approved_gates() {
  local verifier_sha schema_sha schema_helper_sha
  verifier_sha=$(sha256_file "$VERIFIER") || { fail 'cannot hash the rollout verifier'; return 1; }
  schema_sha=$(sha256_file "$SCHEMA_GATE") || { fail 'cannot hash the schema gate'; return 1; }
  schema_helper_sha=$(sha256_file "$SCHEMA_GATE_HELPER") || { fail 'cannot hash the schema gate helper'; return 1; }
  [ "$verifier_sha" = "$APPROVED_VERIFIER_SHA" ] || { fail 'rollout verifier hash differs from reviewed artifact'; return 1; }
  [ "$schema_sha" = "$APPROVED_SCHEMA_GATE_SHA" ] || { fail 'schema gate hash differs from reviewed artifact'; return 1; }
  [ "$schema_helper_sha" = "$APPROVED_SCHEMA_GATE_HELPER_SHA" ] || { fail 'schema gate helper hash differs from reviewed artifact'; return 1; }
}

canonical_compose_file_path() {
  local path=$1 working_dir
  [ -n "$path" ] || return 1
  case "$path" in
    /*) ;;
    *)
      working_dir=$(pwd -P) || return 1
      path="$working_dir/$path"
      ;;
  esac
  [ -f "$path" ] || return 1
  readlink -f "$path"
}

require_compose_override() {
  local override_path entry entry_path override_included=0
  local -a compose_files
  [ -e "$OVERRIDE_FILE" ] || [ -L "$OVERRIDE_FILE" ] || return 0
  override_path=$(canonical_compose_file_path "$OVERRIDE_FILE") || { fail 'cannot resolve the Compose override path'; return 1; }
  [ "${COMPOSE_FILE+x}" = x ] || return 0
  [ -n "$COMPOSE_FILE" ] || { fail 'COMPOSE_FILE omits the existing docker-compose.override.yml'; return 1; }
  IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
  for entry in "${compose_files[@]}"; do
    [ -n "$entry" ] || continue
    entry_path=$(canonical_compose_file_path "$entry") || { fail 'cannot resolve a COMPOSE_FILE entry'; return 1; }
    [ "$entry_path" = "$override_path" ] && override_included=1
  done
  [ "$override_included" -eq 1 ] && return 0
  fail 'COMPOSE_FILE omits the existing docker-compose.override.yml'
}

verify_initial_local_compose() {
  local artifact_path artifact_sha override_path='' entry entry_path artifact_count=0 override_count=0
  local expected_entries=1 entry_index=0
  local -a compose_files
  [ "$INITIALIZE_LOCAL" = 1 ] || return 0
  artifact_path="$CHECKOUT/$INITIAL_LOCAL_COMPOSE_FILE"
  [ -f "$artifact_path" ] && [ ! -L "$artifact_path" ] && \
    [ "$(stat -c %u -- "$artifact_path")" = 0 ] && [ "$(stat -c %a -- "$artifact_path")" = 600 ] || {
    fail 'initial-local Compose artifact must be a root-owned mode-0600 regular file'
    return 1
  }
  artifact_path=$(canonical_compose_file_path "$artifact_path") || { fail 'cannot resolve initial-local Compose artifact path'; return 1; }
  artifact_sha=$(sha256_file "$artifact_path") || { fail 'cannot hash initial-local Compose artifact'; return 1; }
  INITIAL_LOCAL_ARTIFACT_SHA=$artifact_sha
  [ "$artifact_sha" = "$APPROVED_INITIAL_LOCAL_COMPOSE_SHA" ] || {
    fail 'initial-local Compose artifact differs from the reviewed pin'
    return 1
  }
  [ "${COMPOSE_FILE+x}" = x ] && [ -n "$COMPOSE_FILE" ] || {
    fail 'initial-local COMPOSE_FILE must select the reviewed local artifact'
    return 1
  }
  if [ -e "$OVERRIDE_FILE" ] || [ -L "$OVERRIDE_FILE" ]; then
    expected_entries=2
    override_path=$(canonical_compose_file_path "$OVERRIDE_FILE") || { fail 'cannot resolve the Compose override path'; return 1; }
  fi
  IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
  for entry in "${compose_files[@]}"; do
    [ -n "$entry" ] || { fail 'initial-local COMPOSE_FILE contains an empty entry'; return 1; }
    entry_path=$(canonical_compose_file_path "$entry") || { fail 'cannot resolve initial-local COMPOSE_FILE entry'; return 1; }
    if [ "$entry_path" = "$artifact_path" ]; then
      [ "$entry_index" -eq 0 ] || { fail 'initial-local Compose artifact must be the first file'; return 1; }
      artifact_count=$((artifact_count + 1))
    elif [ "$expected_entries" -eq 2 ] && [ "$entry_path" = "$override_path" ]; then
      [ "$entry_index" -eq 1 ] || { fail 'existing Compose override must follow the initial-local artifact'; return 1; }
      override_count=$((override_count + 1))
    else
      fail 'initial-local COMPOSE_FILE includes an unapproved Compose file'
      return 1
    fi
    entry_index=$((entry_index + 1))
  done
  [ "$artifact_count" -eq 1 ] && [ "$override_count" -eq $((expected_entries - 1)) ] && \
    [ "${#compose_files[@]}" -eq "$expected_entries" ] || {
    fail 'initial-local COMPOSE_FILE must select the pinned artifact followed by the existing override'
    return 1
  }
}

is_tracked_local_compose_selection() {
  local -a compose_files expected_files
  local index checkout_path expected_path
  [ "${COMPOSE_FILE+x}" = x ] && [ -n "$COMPOSE_FILE" ] || return 1
  IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
  [ "${#compose_files[@]}" -eq 3 ] || return 1
  checkout_path=$(cd -P "$CHECKOUT" && pwd -P) || return 1
  expected_files=(docker-compose.yml docker-compose.local-blobs.yml docker-compose.override.yml)
  for index in "${!expected_files[@]}"; do
    [ -f "$CHECKOUT/${expected_files[$index]}" ] && [ ! -L "$CHECKOUT/${expected_files[$index]}" ] || return 1
    expected_path="$checkout_path/${expected_files[$index]}"
    [ "${compose_files[$index]}" = "$expected_path" ] || return 1
  done
  git -C "$CHECKOUT" ls-files --error-unmatch -- \
    docker-compose.yml docker-compose.local-blobs.yml >/dev/null 2>&1 || return 1
  git -C "$CHECKOUT" cat-file -e "$TARGET_SHA:docker-compose.yml" 2>/dev/null && \
    git -C "$CHECKOUT" cat-file -e "$TARGET_SHA:docker-compose.local-blobs.yml" 2>/dev/null
}

activate_tracked_local_compose() {
  local authority_json outcome backend_kind blob_dir
  authority_json=$(python3 "$MODE_HELPER" authority \
    --state-dir "$CHECKOUT/.state/blob-mode" --report-root "$EVIDENCE_ROOT") || {
    fail 'cannot read durable blob authority for the tracked local Compose selection'
    return 1
  }
  outcome=$(jq -er '.outcome | select(type == "string")' <<< "$authority_json") || {
    fail 'durable blob authority has no valid outcome for the tracked local Compose selection'
    return 1
  }
  backend_kind=$(jq -er '.backend.kind | select(type == "string")' <<< "$authority_json") || {
    fail 'durable blob authority has no valid backend for the tracked local Compose selection'
    return 1
  }
  blob_dir=$(jq -er '.backend.dir | select(type == "string" and startswith("/") and . != "/")' <<< "$authority_json") || {
    fail 'durable blob authority has no valid local blob directory for the tracked Compose selection'
    return 1
  }
  case "$outcome" in
    stack-moved-local|recovered-local) ;;
    *)
      fail 'tracked local Compose selection requires local blob authority'
      return 1
      ;;
  esac
  [ "$backend_kind" = local ] || {
    fail 'tracked local Compose selection requires a local blob backend'
    return 1
  }
  TRACKED_LOCAL_BLOB_DIR=$blob_dir
  TEAGRAMD_STACK_MOVE_BLOB_DIR=$blob_dir
  export TEAGRAMD_STACK_MOVE_BLOB_DIR
  TRACKED_LOCAL_AUTHORITY=1
}

verify_target_local_compose() {
  local artifact_path artifact_sha artifact_content_sha reviewed_content_sha override_path entry entry_path artifact_count=0 override_count=0
  local -a compose_files
  TRACKED_LOCAL_AUTHORITY=0
  TRACKED_LOCAL_BLOB_DIR=
  [ "$INITIALIZE_LOCAL" = 1 ] && return 0
  if ! set_target_local_compose_pin; then
    [ "${COMPOSE_FILE+x}" = x ] || return 0
    IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
    for entry in "${compose_files[@]}"; do
      [ -n "$entry" ] || continue
      case "${entry##*/}" in
        .rollout-compose.local-*.yml)
          fail 'target-local Compose artifact must be selected first and match the reviewed local content'
          return 1
          ;;
      esac
    done
    if is_tracked_local_compose_selection; then
      activate_tracked_local_compose || return 1
    fi
    return 0
  fi
  artifact_path="$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE"
  [ -f "$artifact_path" ] && [ ! -L "$artifact_path" ] && \
    [ "$(stat -c %u -- "$artifact_path")" = 0 ] && [ "$(stat -c %a -- "$artifact_path")" = 600 ] || {
    fail 'target-local Compose artifact must be a root-owned mode-0600 regular file'
    return 1
  }
  artifact_path=$(canonical_compose_file_path "$artifact_path") || { fail 'cannot resolve the target-local Compose artifact path'; return 1; }
  artifact_sha=$(sha256_file "$artifact_path") || { fail 'cannot hash target-local Compose artifact'; return 1; }
  if [ "$PINNED_LOCAL_COMPOSE_CARRIED" = 1 ]; then
    artifact_content_sha=$(sha256_without_comment_lines "$artifact_path") || {
      fail 'cannot hash carried-forward local Compose content'
      return 1
    }
    [ "$artifact_content_sha" = "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" ] || {
      fail 'carried-forward local Compose content differs from the reviewed pin'
      return 1
    }
    reviewed_content_sha=$(sha256_target_file_without_comment_lines \
      "${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}" "$PINNED_LOCAL_COMPOSE_SOURCE_FILE") || {
      fail 'cannot read the last reviewed local Compose content'
      return 1
    }
    [ "$reviewed_content_sha" = "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" ] || {
      fail 'last reviewed local Compose source differs from the approved content'
      return 1
    }
    PINNED_LOCAL_COMPOSE_SHA=$artifact_sha
  else
    [ "$artifact_sha" = "$PINNED_LOCAL_COMPOSE_SHA" ] || {
      fail 'target-local Compose artifact differs from the reviewed pin'
      return 1
    }
    artifact_content_sha=$(sha256_without_comment_lines "$artifact_path") || {
      fail 'cannot hash target-local Compose content'
      return 1
    }
    [ "$artifact_content_sha" = "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" ] || {
      fail 'target-local Compose content differs from the reviewed content pin'
      return 1
    }
  fi
  [ -e "$OVERRIDE_FILE" ] || [ -L "$OVERRIDE_FILE" ] || {
    fail 'target-local rollout requires the existing docker-compose.override.yml'
    return 1
  }
  override_path=$(canonical_compose_file_path "$OVERRIDE_FILE") || { fail 'cannot resolve the Compose override path'; return 1; }
  [ "${COMPOSE_FILE+x}" = x ] && [ -n "$COMPOSE_FILE" ] || {
    fail 'target-local COMPOSE_FILE must select the reviewed artifact and existing override'
    return 1
  }
  IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
  for entry in "${compose_files[@]}"; do
    [ -n "$entry" ] || { fail 'target-local COMPOSE_FILE contains an empty entry'; return 1; }
    entry_path=$(canonical_compose_file_path "$entry") || { fail 'cannot resolve target-local COMPOSE_FILE entry'; return 1; }
    if [ "$entry_path" = "$artifact_path" ]; then
      [ "$artifact_count" -eq 0 ] && [ "$override_count" -eq 0 ] || {
        fail 'target-local Compose artifact must be the first file'
        return 1
      }
      artifact_count=1
    elif [ "$entry_path" = "$override_path" ]; then
      [ "$artifact_count" -eq 1 ] && [ "$override_count" -eq 0 ] || {
        fail 'existing Compose override must follow the target-local artifact'
        return 1
      }
      override_count=1
    else
      fail 'target-local COMPOSE_FILE includes an unapproved Compose file'
      return 1
    fi
  done
  [ "$artifact_count" -eq 1 ] && [ "$override_count" -eq 1 ] && [ "${#compose_files[@]}" -eq 2 ] || {
    fail 'target-local COMPOSE_FILE must select the pinned artifact followed by the existing override'
    return 1
  }
}

require_runtime() {
  local uid expected_checkout expected_source_dir
  uid=$(id -u) || { fail 'cannot inspect effective uid'; return 1; }
  [ "$uid" = 0 ] || { fail 'production rollout requires uid 0'; return 1; }
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    expected_checkout=${ROLLOUT_RUNNER_TEST_CHECKOUT:-}
    [ "$EVIDENCE_ROOT" = /root ] || { fail 'fixture evidence root must be /root'; return 1; }
  else
    if [ "$RUNNER_ACTION" = move-stack ]; then
      expected_checkout="/opt/$PROJECT_MOVE_SOURCE"
      [ "$TARGET_CHECKOUT" = /opt/teagram ] || { fail 'production stack-move target checkout must be /opt/teagram'; return 1; }
      [ "$SOURCE_LOCK_PATH" = /tmp/telegram-server-deploy.lock ] || { fail 'production source deploy lock path is fixed'; return 1; }
    else
      expected_checkout=/opt/teagram
    fi
    [ "$EVIDENCE_ROOT" = /root ] || { fail 'production evidence root must be /root'; return 1; }
    [ "$LOCK_PATH" = /tmp/teagram-deploy.lock ] || { fail 'production deploy lock path is fixed'; return 1; }
  fi
  [ -n "$expected_checkout" ] && [ "$PWD" = "$CHECKOUT" ] && [ "$CHECKOUT" = "$expected_checkout" ] || {
    fail 'runner must execute from its configured checkout'
    return 1
  }
  [ -d "$EVIDENCE_ROOT" ] && [ ! -L "$EVIDENCE_ROOT" ] && \
    [ "$(stat -c %u "$EVIDENCE_ROOT")" = 0 ] && [ "$(stat -c %a "$EVIDENCE_ROOT")" = 700 ] || {
    fail 'production evidence root must be root-owned mode 0700'
    return 1
  }
  [ "$ENV_FILE" = "$CHECKOUT/.env" ] && [ "$OVERRIDE_FILE" = "$CHECKOUT/docker-compose.override.yml" ] || {
    fail 'secret and override paths must be fixed within the checkout'
    return 1
  }
  require_compose_override || return 1
  verify_initial_local_compose || return 1
  verify_target_local_compose || return 1
  if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    [ "$SCRIPT_DIR" = "${ROLLOUT_RUNNER_BASELINE_DIR:-}" ] || { fail 'pinned runner path does not match its baseline evidence directory'; return 1; }
    check_private_dir "$SCRIPT_DIR" || return 1
    for path in "$SCRIPT_SOURCE" "$VERIFIER" "$SCHEMA_GATE" "$SCHEMA_GATE_HELPER" "$MODE_HELPER" "$QUALIFIER_HELPER" "$STACK_MOVE_HELPER"; do
      [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u -- "$path")" = 0 ] && \
        [ "$(stat -c %a -- "$path")" = 600 ] || { fail 'pinned runtime file is not root-only'; return 1; }
    done
  else
    if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
      expected_source_dir=${ROLLOUT_RUNNER_TEST_SOURCE_DIR:-"$CHECKOUT/deploy/teagramd/rollout-runner"}
    else
      expected_source_dir=/root/teagramd-rollout-runner
    fi
    [ "$SCRIPT_DIR" = "$expected_source_dir" ] || {
      fail 'initial runner must come from its root-only source directory'
      return 1
    }
    check_private_dir "$SCRIPT_DIR" || return 1
    for path in "$SCRIPT_SOURCE" "$SCRIPT_DIR/rollout-verifier.sh" "$SCRIPT_DIR/schema-result-gate.sh" "$SCHEMA_GATE_HELPER" "$MODE_HELPER" "$QUALIFIER_HELPER" "$STACK_MOVE_HELPER"; do
      [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u -- "$path")" = 0 ] && \
        [ "$(stat -c %a -- "$path")" = 600 ] || { fail 'initial runtime file is not root-only'; return 1; }
    done
  fi
}

acquire_shared_lock() {
  if [ "${ROLLOUT_RUNNER_TEST_MODE:-0}" = 1 ]; then
    exec 9>"$LOCK_PATH"
  elif [ "$RUNNER_ACTION" = move-stack ]; then
    exec 9>"$TARGET_LOCK_PATH"
  else
    exec 9>/tmp/teagram-deploy.lock
  fi
  flock -x 9
}

check_private_dir() {
  local dir=$1 mode owner
  [ -d "$dir" ] && [ ! -L "$dir" ] || { fail 'evidence phase directory missing or symlinked'; return 1; }
  mode=$(stat -c %a -- "$dir") || { fail 'cannot inspect evidence directory mode'; return 1; }
  owner=$(stat -c %u -- "$dir") || { fail 'cannot inspect evidence directory owner'; return 1; }
  [ "$mode" = 700 ] && [ "$owner" = "$PREFLIGHT_EVIDENCE_OWNER" ] || {
    fail 'evidence phase directory must have the expected owner and mode 0700'
    return 1
  }
}

create_phase_dirs() {
  local target_short stamp phase dir
  target_short=${TARGET_SHA:0:12}
  stamp=$(date -u +%Y%m%dT%H%M%SZ) || { fail 'cannot timestamp evidence run'; return 1; }
  RUN_ID="main1238-$target_short-$stamp"
  for phase in baseline backup build target rollback; do
    dir="$EVIDENCE_ROOT/$RUN_ID.$phase"
    if [ -e "$dir" ] || [ -L "$dir" ]; then
      fail "evidence collision at $phase phase"
      return 1
    fi
    if ! mkdir -m 700 -- "$dir"; then
      fail "cannot create $phase evidence directory"
      return 1
    fi
    check_private_dir "$dir" || return 1
    case "$phase" in
      baseline) BASELINE_DIR=$dir ;;
      backup) BACKUP_DIR=$dir ;;
      build) BUILD_DIR=$dir ;;
      target) TARGET_DIR=$dir ;;
      rollback) ROLLBACK_DIR=$dir ;;
    esac
  done
  sync || { fail 'cannot sync phase directories'; return 1; }
}

write_immutable() {
  local path=$1 contents=$2 dir temp mode owner
  dir=$(dirname -- "$path")
  check_private_dir "$dir" || return 1
  if [ -e "$path" ] || [ -L "$path" ]; then
    fail "evidence collision at $(basename -- "$path")"
    return 1
  fi
  temp=$(mktemp "$dir/.stage.XXXXXXXX") || { fail 'cannot allocate evidence staging file'; return 1; }
  if ! printf '%s\n' "$contents" > "$temp"; then
    rm -f -- "$temp"
    fail 'cannot write evidence staging file'
    return 1
  fi
  if ! chmod 600 -- "$temp" || [ "$(stat -c %a -- "$temp")" != 600 ]; then
    rm -f -- "$temp"
    fail 'cannot protect evidence staging file'
    return 1
  fi
  sync || { rm -f -- "$temp"; fail 'cannot sync staged evidence'; return 1; }
  if ! ln -- "$temp" "$path"; then
    rm -f -- "$temp"
    fail 'cannot publish immutable evidence file'
    return 1
  fi
  rm -- "$temp" || { fail 'cannot remove evidence staging link'; return 1; }
  mode=$(stat -c %a -- "$path") || { fail 'cannot inspect evidence file mode'; return 1; }
  owner=$(stat -c %u -- "$path") || { fail 'cannot inspect evidence file owner'; return 1; }
  [ "$mode" = 600 ] && [ "$owner" = "$PREFLIGHT_EVIDENCE_OWNER" ] || {
    fail 'evidence file does not have the expected owner and mode 0600'
    return 1
  }
  sync || { fail 'cannot sync published evidence'; return 1; }
}

append_evidence_row() {
  local file=$1 phase=$2 field=$3 expected=$4 actual=$5 result=$6 timestamp
  [ -f "$file" ] && [ ! -L "$file" ] || { fail 'comparison evidence file is missing'; return 1; }
  [ "$(stat -c %a -- "$file")" = 600 ] || { fail 'comparison evidence file is not mode 0600'; return 1; }
  timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ) || { fail 'cannot timestamp comparison evidence'; return 1; }
  if ! printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$timestamp" "$phase" "$field" "$expected" "$actual" "$result" >> "$file"; then
    fail 'cannot persist comparison row'
    return 1
  fi
  if ! chmod 600 -- "$file" || [ "$(stat -c %a -- "$file")" != 600 ] || ! sync; then
    fail 'cannot protect or sync comparison row'
    return 1
  fi
}

record_rollback_row() {
  local file=$1 field=$2 before=$3 after=$4 result
  if [ "$before" = "$after" ]; then result=pass; else result=fail; fi
  append_evidence_row "$file" rollback "$field" "$before" "$after" "$result" || return 1
  if [ "$result" = fail ]; then ROLLBACK_COMPARE_FAILURES=$((ROLLBACK_COMPARE_FAILURES + 1)); fi
}

copy_immutable() {
  local source=$1 destination=$2 dir temp source_sha copy_sha
  dir=$(dirname -- "$destination")
  check_private_dir "$dir" || return 1
  [ -f "$source" ] && [ ! -L "$source" ] && [ "$(stat -c %a -- "$source")" = 600 ] || {
    fail 'snapshot copy source is not a private regular file'
    return 1
  }
  [ ! -e "$destination" ] && [ ! -L "$destination" ] || { fail 'snapshot copy destination already exists'; return 1; }
  temp=$(mktemp "$dir/.copy.XXXXXXXX") || { fail 'cannot allocate snapshot copy'; return 1; }
  if ! cp -- "$source" "$temp" || ! chmod 600 -- "$temp"; then
    rm -f -- "$temp"
    fail 'cannot stage snapshot copy'
    return 1
  fi
  source_sha=$(sha256_file "$source") || { rm -f -- "$temp"; fail 'cannot hash source snapshot'; return 1; }
  copy_sha=$(sha256_file "$temp") || { rm -f -- "$temp"; fail 'cannot hash copied snapshot'; return 1; }
  [ "$source_sha" = "$copy_sha" ] || { rm -f -- "$temp"; fail 'snapshot copy digest mismatch'; return 1; }
  sync || { rm -f -- "$temp"; fail 'cannot sync copied snapshot'; return 1; }
  if ! ln -- "$temp" "$destination"; then
    rm -f -- "$temp"
    fail 'cannot publish immutable snapshot copy'
    return 1
  fi
  rm -- "$temp" || { fail 'cannot remove snapshot staging link'; return 1; }
  sync || { fail 'cannot sync published snapshot copy'; return 1; }
}

pin_runtime_file() {
  local source=$1 destination=$2 dir temp source_sha copy_sha
  dir=$(dirname -- "$destination")
  check_private_dir "$dir" || return 1
  [ -f "$source" ] && [ ! -L "$source" ] || { fail 'runtime source file is missing or symlinked'; return 1; }
  [ ! -e "$destination" ] && [ ! -L "$destination" ] || { fail 'pinned runtime destination already exists'; return 1; }
  temp=$(mktemp "$dir/.runtime.XXXXXXXX") || { fail 'cannot allocate pinned runtime file'; return 1; }
  if ! cp -- "$source" "$temp" || ! chmod 600 -- "$temp"; then
    rm -f -- "$temp"
    fail 'cannot stage pinned runtime file'
    return 1
  fi
  source_sha=$(sha256_file "$source") || { rm -f -- "$temp"; fail 'cannot hash runtime source'; return 1; }
  copy_sha=$(sha256_file "$temp") || { rm -f -- "$temp"; fail 'cannot hash pinned runtime file'; return 1; }
  [ "$source_sha" = "$copy_sha" ] || { rm -f -- "$temp"; fail 'pinned runtime copy digest mismatch'; return 1; }
  sync || { rm -f -- "$temp"; fail 'cannot sync pinned runtime file'; return 1; }
  if ! ln -- "$temp" "$destination"; then
    rm -f -- "$temp"
    fail 'cannot publish pinned runtime file without overwrite'
    return 1
  fi
  rm -- "$temp" || { fail 'cannot remove pinned runtime staging link'; return 1; }
  sync || { fail 'cannot sync published pinned runtime file'; return 1; }
}

pin_runtime() {
  local runner_sha verifier_sha schema_sha schema_helper_sha qualifier_sha source_revision compose_sha
  local target_local_compose_sha=not-selected target_local_compose_target=not-selected
  local target_local_compose_reviewed_origin=not-selected
  local target_local_compose_content_sha=not-selected target_local_compose_source=not-selected target_local_compose_runtime=not-selected
  pin_runtime_file "$SCRIPT_SOURCE" "$BASELINE_DIR/rollout-runner.pinned" || return 1
  pin_runtime_file "$SCRIPT_DIR/rollout-verifier.sh" "$BASELINE_DIR/rollout-verifier.pinned" || return 1
  pin_runtime_file "$SCRIPT_DIR/schema-result-gate.sh" "$BASELINE_DIR/schema-result-gate.pinned" || return 1
  pin_runtime_file "$SCHEMA_GATE_HELPER" "$BASELINE_DIR/schema-result-gate.py" || return 1
  pin_runtime_file "$SCRIPT_DIR/blob-mode-state.py" "$BASELINE_DIR/blob-mode-state.pinned" || return 1
  pin_runtime_file "$SCRIPT_DIR/qualify-rustfs-transition.py" "$BASELINE_DIR/qualify-rustfs-transition.py" || return 1
  pin_runtime_file "$SCRIPT_DIR/stack-move-runner.py" "$BASELINE_DIR/stack-move-runner.pinned" || return 1
  VERIFIER="$BASELINE_DIR/rollout-verifier.pinned"
  SCHEMA_GATE="$BASELINE_DIR/schema-result-gate.pinned"
  SCHEMA_GATE_HELPER="$BASELINE_DIR/schema-result-gate.py"
  MODE_HELPER="$BASELINE_DIR/blob-mode-state.pinned"
  QUALIFIER_HELPER="$BASELINE_DIR/qualify-rustfs-transition.py"
  STACK_MOVE_HELPER="$BASELINE_DIR/stack-move-runner.pinned"
  verify_approved_gates || return 1
  runner_sha=$(sha256_file "$BASELINE_DIR/rollout-runner.pinned") || { fail 'cannot hash pinned runner'; return 1; }
  verifier_sha=$(sha256_file "$VERIFIER") || { fail 'cannot hash pinned verifier'; return 1; }
  schema_sha=$(sha256_file "$SCHEMA_GATE") || { fail 'cannot hash pinned schema gate'; return 1; }
  schema_helper_sha=$(sha256_file "$SCHEMA_GATE_HELPER") || { fail 'cannot hash pinned schema gate helper'; return 1; }
  local mode_helper_sha
  mode_helper_sha=$(sha256_file "$MODE_HELPER") || { fail 'cannot hash pinned blob-mode helper'; return 1; }
  qualifier_sha=$(sha256_file "$QUALIFIER_HELPER") || { fail 'cannot hash pinned transition qualifier'; return 1; }
  local stack_move_helper_sha
  stack_move_helper_sha=$(sha256_file "$STACK_MOVE_HELPER") || { fail 'cannot hash pinned stack move helper'; return 1; }
  source_revision=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  compose_sha=not-selected
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    compose_sha=$(sha256_file "$CHECKOUT/$INITIAL_LOCAL_COMPOSE_FILE") || { fail 'cannot hash initial-local Compose artifact'; return 1; }
  fi
  if [ "$INITIALIZE_LOCAL" != 1 ] && set_target_local_compose_pin; then
    target_local_compose_sha=$(sha256_file "$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE") || {
      fail 'cannot hash target-local Compose artifact'
      return 1
    }
    target_local_compose_target=$PINNED_LOCAL_COMPOSE_TARGET_SHA
    target_local_compose_reviewed_origin=$PINNED_LOCAL_COMPOSE_TARGET_SHA
    target_local_compose_content_sha=$(sha256_without_comment_lines \
      "$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE") || {
      fail 'cannot hash target-local Compose content for runtime evidence'
      return 1
    }
    target_local_compose_source=$PINNED_LOCAL_COMPOSE_SOURCE_FILE
    target_local_compose_runtime=$PINNED_LOCAL_COMPOSE_RUNTIME_FILE
  fi
  write_immutable "$BASELINE_DIR/runtime-pins.txt" \
    "source_revision=$source_revision target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA runner_sha256=$runner_sha verifier_sha256=$verifier_sha schema_gate_sha256=$schema_sha schema_gate_helper_sha256=$schema_helper_sha blob_mode_helper_sha256=$mode_helper_sha transition_qualifier_sha256=$qualifier_sha stack_move_helper_sha256=$stack_move_helper_sha initial_local_compose_sha256=$compose_sha target_local_compose_reviewed_origin_sha=$target_local_compose_reviewed_origin target_local_compose_target_sha=$target_local_compose_target target_local_compose_sha256=$target_local_compose_sha target_local_compose_content_sha256=$target_local_compose_content_sha target_local_compose_source_file=$target_local_compose_source target_local_compose_runtime_file=$target_local_compose_runtime" || return 1
}

current_service_id() {
  local service=$1 id
  id=$(docker compose ps -q "$service" </dev/null 2>/dev/null) || return 1
  id=$(printf '%s' "$id" | head -n 1)
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$id"
}

remove_orphaned_public_profile_services() {
  local baseline_container=$1 project target_services service container_ids container_id container_state stop_timeout found=0
  local -a observed_services=() remove_ids=()
  REMOVED_PUBLIC_PROFILE_SERVICES=()

  project=$(docker inspect "$baseline_container" \
    --format '{{ index .Config.Labels "com.docker.compose.project" }}' </dev/null) || {
    fail 'cannot read the baseline Compose project before removing public profile orphans'
    return 1
  }
  [[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
    fail 'baseline Compose project label is missing or invalid before public profile cleanup'
    return 1
  }

  # Keep HAProxy routing while each application replica drains existing sessions.
  for service in teagramd-public-1 teagramd-public-2 public-haproxy; do
    container_ids=$(docker ps --all --quiet --no-trunc \
      --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service" </dev/null) || {
      fail "cannot list old Compose containers for public profile service $service"
      return 1
    }
    if [ -n "$container_ids" ]; then
      observed_services+=("$service")
      found=1
    fi
  done
  [ "$found" = 1 ] || return 0

  target_services=$(docker compose --profile public config --services </dev/null) || {
    fail 'cannot inspect target Compose services before removing public profile orphans'
    return 1
  }
  for service in "${observed_services[@]}"; do
    if printf '%s\n' "$target_services" | grep -Fxq "$service"; then
      continue
    fi
    container_ids=$(docker ps --all --quiet --no-trunc \
      --filter "label=com.docker.compose.project=$project" \
      --filter "label=com.docker.compose.service=$service" </dev/null) || {
      fail "cannot recheck old Compose containers for public profile service $service"
      return 1
    }
    [ -n "$container_ids" ] || continue
    while IFS= read -r container_id; do
      [ -n "$container_id" ] || continue
      [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || {
        fail "old Compose container ID for public profile service $service is invalid"
        return 1
      }
      remove_ids+=("$container_id")
    done <<< "$container_ids"
    REMOVED_PUBLIC_PROFILE_SERVICES+=("$service")
  done

  [ "${#remove_ids[@]}" -gt 0 ] || return 0
  for container_id in "${remove_ids[@]}"; do
    container_state=$(docker inspect "$container_id" \
      --format '{{.State.Running}} {{.Config.StopTimeout}}' </dev/null) || {
      fail "cannot inspect public profile container $container_id before stopping it"
      return 1
    }
    read -r container_state stop_timeout <<< "$container_state"
    case "$container_state" in
      true)
        # The server's bounded shutdown drain is 90 seconds.
        [[ "$stop_timeout" =~ ^[0-9]{1,5}$ ]] && [ "$stop_timeout" -ge 90 ] || {
          fail "public profile container $container_id has no safe configured shutdown timeout"
          return 1
        }
        docker stop --timeout "$stop_timeout" "$container_id" </dev/null >/dev/null || {
          fail "cannot gracefully stop public profile container $container_id"
          return 1
        }
        ;;
      false) ;;
      *)
        fail "public profile container $container_id has an invalid running state"
        return 1
        ;;
    esac
  done
  docker rm "${remove_ids[@]}" </dev/null >/dev/null || {
    fail 'cannot remove stopped public profile containers before replacing teagramd'
    return 1
  }
  printf 'gracefully stopped and removed public profile containers project=%s services=%s\n' \
    "$project" "${REMOVED_PUBLIC_PROFILE_SERVICES[*]}"
}

restore_removed_public_profile_services() {
  local service removed_service
  local -a restore_services=()
  [ "${#REMOVED_PUBLIC_PROFILE_SERVICES[@]}" -gt 0 ] || return 0
  for service in public-haproxy teagramd-public-1 teagramd-public-2; do
    for removed_service in "${REMOVED_PUBLIC_PROFILE_SERVICES[@]}"; do
      if [ "$removed_service" = "$service" ]; then
        restore_services+=("$service")
        break
      fi
    done
  done
  docker compose --profile public up -d --no-build --no-deps \
    "${restore_services[@]}" </dev/null || {
    fail 'could not restore removed public profile services during rollback'
    return 1
  }
  printf 'restored baseline public profile services after rollback: %s\n' \
    "${restore_services[*]}"
}

probe_restored_public_replica() {
  local haproxy_id=$1 address=$2 response status body
  response=$(docker exec "$haproxy_id" sh -c '
    printf "\\r\\n\\r\\n\\000\\r\\nQUIT\\n\\040\\000\\000\\000GET /.well-known/teagramd/client HTTP/1.0\\r\\nHost: localhost\\r\\nConnection: close\\r\\n\\r\\n" |
      busybox nc -w 3 "$1" 2446
  ' sh "$address" 2>/dev/null) || return 1
  status=${response%%$'\r\n'*}
  [[ "$status" =~ ^HTTP/1\.[01]\ 200([[:space:]]|$) ]] || return 1
  body=${response#*$'\r\n\r\n'}
  [ "$body" != "$response" ] || return 1
  printf '%s' "$body" | jq -e \
    '.version == 1 and (.websocket.endpoint | type == "string" and test("^wss://[^/]+/apiws$"))' \
    >/dev/null 2>&1
}

public_backend_network_name() {
  local haproxy_id=$1 project network_names network
  project=$(docker inspect "$haproxy_id" \
    --format '{{ index .Config.Labels "com.docker.compose.project" }}' \
    </dev/null 2>/dev/null) || {
    fail 'cannot read the restored public-haproxy Compose project label'
    return 1
  }
  [[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
    fail 'restored public-haproxy has a missing or invalid Compose project label'
    return 1
  }
  network_names=$(docker network ls \
    --filter "label=com.docker.compose.project=$project" \
    --filter 'label=com.docker.compose.network=public-backend' \
    --format '{{.Name}}' </dev/null 2>/dev/null) || {
    fail 'cannot resolve the restored public backend network by its Compose labels'
    return 1
  }
  network=${network_names%%$'\n'*}
  [ -n "$network" ] && [ "$network_names" = "$network" ] && \
    [[ "$network" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] || {
    fail 'restored Compose project must have exactly one valid public backend network'
    return 1
  }
  printf '%s' "$network"
}

wait_for_restored_public_profile_health() {
  local service removed_service container_ids id state health address expected_address haproxy_id public_backend_network
  local ready deadline remaining
  public_backend_network=''
  local -a services_to_check=()
  local -A ready_ids=()
  [ "${#REMOVED_PUBLIC_PROFILE_SERVICES[@]}" -gt 0 ] || return 0
  [[ "$READY_SECONDS" =~ ^[0-9]+$ ]] && [ "$READY_SECONDS" -ge 1 ] && [ "$READY_SECONDS" -le 120 ] || {
    fail 'public profile readiness bound must be between 1 and 120 seconds'
    return 1
  }

  for service in public-haproxy teagramd-public-1 teagramd-public-2; do
    ready=0
    for removed_service in "${REMOVED_PUBLIC_PROFILE_SERVICES[@]}"; do
      if [ "$removed_service" = "$service" ]; then
        ready=1
        break
      fi
    done
    if [ "$service" = public-haproxy ] && [ "$ready" = 0 ]; then
      for removed_service in "${REMOVED_PUBLIC_PROFILE_SERVICES[@]}"; do
        case "$removed_service" in
          teagramd-public-1|teagramd-public-2) ready=1; break ;;
        esac
      done
    fi
    [ "$ready" = 1 ] && services_to_check+=("$service")
  done

  deadline=$((SECONDS + READY_SECONDS))
  for service in "${services_to_check[@]}"; do
    ready=0
    while [ "$SECONDS" -le "$deadline" ]; do
      container_ids=$(docker compose --profile public ps --all --quiet "$service" </dev/null 2>/dev/null) || container_ids=''
      id=${container_ids%%$'\n'*}
      if [ -n "$id" ]; then
        [ "$container_ids" = "$id" ] || {
          fail "restored public profile service $service has multiple containers"
          return 1
        }
        [[ "$id" =~ ^[0-9a-f]{64}$ ]] || {
          fail "restored public profile service $service has an invalid container ID"
          return 1
        }
        state=$(docker inspect "$id" --format '{{.State.Status}}' </dev/null 2>/dev/null) || state=unknown
        case "$state" in
          dead)
            fail "restored public profile service $service stopped before becoming healthy"
            return 1
            ;;
          exited|created|restarting)
            ;;
          running)
            if [ "$service" = public-haproxy ]; then
              health=$(docker inspect "$id" \
                --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}missing{{end}}' \
                </dev/null 2>/dev/null) || health=unknown
              case "$health" in
                healthy)
                  ready=1
                  ;;
                missing)
                  fail 'restored public-haproxy has no configured Docker healthcheck'
                  return 1
                  ;;
              esac
            else
              case "$service" in
                teagramd-public-1) expected_address=198.18.132.21 ;;
                teagramd-public-2) expected_address=198.18.132.22 ;;
                *) fail "unknown restored public profile service $service"; return 1 ;;
              esac
              haproxy_id=${ready_ids[public-haproxy]:-}
              [ -n "$haproxy_id" ] || {
                fail 'restored public replica has no verified HAProxy container'
                return 1
              }
              if [ -z "$public_backend_network" ]; then
                public_backend_network=$(public_backend_network_name "$haproxy_id") || return 1
              fi
              address=$(docker inspect "$id" \
                --format "{{with index .NetworkSettings.Networks \"$public_backend_network\"}}{{.IPAddress}}{{end}}" \
                </dev/null 2>/dev/null) || address=''
              if [ "$address" = "$expected_address" ] && [ -n "$haproxy_id" ] && \
                  probe_restored_public_replica "$haproxy_id" "$address"; then
                ready=1
              fi
            fi
            ;;
        esac
        if [ "$ready" = 1 ]; then
          ready_ids[$service]=$id
          printf 'restored public profile service healthy service=%s container_id=%s\n' "$service" "$id"
          break
        fi
      fi
      remaining=$((deadline - SECONDS))
      [ "$remaining" -gt 0 ] || break
      sleep 1
    done
    [ "$ready" = 1 ] || {
      fail "restored public profile service $service did not become healthy within ${READY_SECONDS}s"
      return 1
    }
  done
}

rollout_compose_service() {
  printf '%s\n' "$ROLLOUT_COMPOSE_SERVICE"
}

select_rollout_compose_service() {
  [ "$INITIALIZE_LOCAL" = 1 ] || return 0
  local services
  services=$(docker compose config --services </dev/null) || {
    fail 'cannot list services from the pinned initial-local Compose render'
    return 1
  }
  ROLLOUT_COMPOSE_SERVICE=$(printf '%s\n' "$services" | awk '
    $0 == "telegramd" || $0 == "teagramd" { selected=$0; count++ }
    END {if (count == 1) print selected; else exit 1}
  ') || {
    fail 'pinned initial-local Compose render must define exactly one supported server service'
    return 1
  }
}

archive_pre_rollout_teagramd_logs() {
  local container_id driver since archive_dir captured combined
  container_id=$(current_service_id "$(rollout_compose_service)") || {
    fail 'cannot resolve the current teagramd container before replacement'
    return 1
  }
  driver=$(docker inspect "$container_id" --format '{{.HostConfig.LogConfig.Type}}') || {
    fail 'cannot inspect the current teagramd logging driver before replacement'
    return 1
  }
  if [ "$driver" = syslog ]; then
    return 0
  fi
  [ -n "$LEGACY_LOG_ARCHIVE" ] || {
    fail 'legacy log archive path is unavailable'
    return 1
  }

  archive_dir=${LEGACY_LOG_ARCHIVE%/*}
  [ -d "$archive_dir" ] || {
    fail "legacy log archive directory is missing: $archive_dir"
    return 1
  }
  if [ -L "$LEGACY_LOG_ARCHIVE" ] || { [ -e "$LEGACY_LOG_ARCHIVE" ] && [ ! -f "$LEGACY_LOG_ARCHIVE" ]; }; then
    fail 'legacy log archive is not a regular file'
    return 1
  fi
  if [ -f "$LEGACY_LOG_ARCHIVE" ] && grep -Fqx "# teagramd-container-id=$container_id" "$LEGACY_LOG_ARCHIVE"; then
    return 0
  fi

  since=$(date -u -d '7 days ago' +%Y-%m-%dT%H:%M:%SZ) || {
    fail 'cannot compute the legacy log retention cutoff'
    return 1
  }
  captured=$(mktemp "$archive_dir/.teagramd-logs.XXXXXX") || return 1
  combined=$(mktemp "$archive_dir/.teagramd-archive.XXXXXX") || {
    rm -f "$captured"
    return 1
  }
  if ! docker logs --since "$since" --timestamps --tail 5000 "$container_id" 2>&1 | \
    awk 'index($0, "login code issued") == 0' > "$captured"; then
    rm -f "$captured" "$combined"
    fail 'cannot preserve the current teagramd logs before container replacement'
    return 1
  fi
  if [ -f "$LEGACY_LOG_ARCHIVE" ]; then
    if ! { awk 'index($0, "login code issued") == 0' "$LEGACY_LOG_ARCHIVE"; \
      printf '# teagramd-container-id=%s\n' "$container_id"; cat "$captured"; } | \
      tail -c 33554432 > "$combined"; then
      rm -f "$captured" "$combined"
      fail 'cannot update the bounded legacy teagramd log archive'
      return 1
    fi
  elif ! { printf '# teagramd-container-id=%s\n' "$container_id"; cat "$captured"; } | \
    tail -c 33554432 > "$combined"; then
    rm -f "$captured" "$combined"
    fail 'cannot create the bounded legacy teagramd log archive'
    return 1
  fi
  chmod 0600 "$combined" || {
    rm -f "$captured" "$combined"
    fail 'cannot protect the legacy teagramd log archive'
    return 1
  }
  mv -f "$combined" "$LEGACY_LOG_ARCHIVE" || {
    rm -f "$captured" "$combined"
    fail 'cannot publish the legacy teagramd log archive'
    return 1
  }
  rm -f "$captured"
  printf 'preserved recent logs from teagramd container %s\n' "${container_id:0:12}"
}

capture_snapshot() {
  local id=$1 dir=$2 name=$3 compose_source=${4:-selected-compose-render} output verifier_error
  local override_origin_value previous_override_origin_value
  output="$dir/$name.snapshot.json"
  [ ! -e "$output" ] && [ ! -L "$output" ] || { fail 'snapshot evidence already exists'; return 1; }
  if [ "$CHANGED_ENV_SOURCE" = override ]; then
    override_origin_value=$(override_websocket_origins_value "$OVERRIDE_FILE") || {
      fail 'Compose override must set TG_WEBSOCKET_ALLOWED_ORIGINS once under services.teagramd.environment'
      return 1
    }
    previous_override_origin_value=$(override_websocket_origins_value "$PREVIOUS_OVERRIDE_FILE") || {
      fail 'previous Compose override must set TG_WEBSOCKET_ALLOWED_ORIGINS once under services.teagramd.environment'
      return 1
    }
  fi
  if ! verifier_error=$(ROLLOUT_CHECKOUT_PATH="$CHECKOUT" ROLLOUT_COMPOSE_SERVICE="$(rollout_compose_service)" ROLLOUT_ADDED_ENV="$ADDED_ENV" \
    ROLLOUT_CHANGED_ENV="$CHANGED_ENV" ROLLOUT_CHANGED_ENV_SOURCE="$CHANGED_ENV_SOURCE" \
    ROLLOUT_OVERRIDE_ORIGIN_VALUE="${override_origin_value:-}" \
    ROLLOUT_PREVIOUS_OVERRIDE_ORIGIN_VALUE="${previous_override_origin_value:-}" \
    bash "$VERIFIER" snapshot "$id" "$dir" "$name" "$ENV_FILE" "$OVERRIDE_FILE" "$compose_source" 2>&1 >/dev/null); then
    if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ] && [ -n "$verifier_error" ]; then
      printf 'snapshot verifier: %s\n' "$verifier_error" >&2
    fi
    fail "cannot capture $name snapshot"
    return 1
  fi
  [ -f "$output" ] && [ ! -L "$output" ] && [ "$(stat -c %a -- "$output")" = 600 ] || {
    fail "$name snapshot is not private"
    return 1
  }
}

capture_compose_blob_inventory() {
  local path=$1 inventory authority_file="$CHECKOUT/.state/blob-mode/mode.json" retain_rustfs_volume=false authority_mode
  local -a compose_profile_args=()
  [ ! -e "$path" ] && [ ! -L "$path" ] || { fail 'blob Compose inventory evidence already exists'; return 1; }

  if [ -e "$authority_file" ] || [ -L "$authority_file" ]; then
    [ -f "$authority_file" ] && [ ! -L "$authority_file" ] && \
      [ "$(stat -c %u -- "$authority_file")" = 0 ] && \
      authority_mode=$(stat -c %a -- "$authority_file") && \
      (( (8#$authority_mode & 0022) == 0 )) || {
      fail 'durable blob authority mode record is missing or not private'
      return 1
    }
    retain_rustfs_volume=$(jq -r '
      if type != "object" then error("invalid authority")
      elif (.volumes | type) != "object" then error("invalid volumes")
      elif (.volumes | has("rustfsdata") | not) then error("missing rustfsdata")
      elif .volumes.rustfsdata == null then false
      elif (.volumes.rustfsdata | type) == "string" then true
      else error("invalid rustfsdata")
      end
    ' "$authority_file") || {
      fail 'cannot inspect durable blob authority volumes'
      return 1
    }
    case "$retain_rustfs_volume" in
      true|false) ;;
      *) fail 'durable blob authority has an invalid RustFS volume selector'; return 1 ;;
    esac
  fi

  if [ "$retain_rustfs_volume" = true ]; then
    compose_profile_args=(--profile migration)
  fi
  inventory=$(docker compose "${compose_profile_args[@]}" config --format json </dev/null 2>/dev/null | \
    python3 "$MODE_HELPER" compose --checkout "$CHECKOUT") || {
    fail 'cannot capture the allowlisted blob Compose inventory'
    return 1
  }
  write_immutable "$path" "$inventory"
}

capture_running_blob_inventory() {
  local path=$1 allow_empty=${2:-0} reference_id reference_inspect project ids id inventory
  local reference_service
  reference_service=$(rollout_compose_service)
  local -a container_ids=()
  [ "$RUNNER_ACTION" = move-stack ] && reference_service=telegramd
  [ ! -e "$path" ] && [ ! -L "$path" ] || { fail 'running blob inventory evidence already exists'; return 1; }
  if [ "$allow_empty" = 1 ]; then
    reference_id=$(docker compose ps -aq "$reference_service" </dev/null 2>/dev/null | head -n 1) || reference_id=''
    [[ "$reference_id" =~ ^[0-9a-f]{64}$ ]] || reference_id=''
  else
    reference_id=$(current_service_id "$reference_service") || { fail "running $reference_service container ID is unavailable"; return 1; }
  fi
  if [ -n "$reference_id" ]; then
    reference_inspect=$(docker inspect "$reference_id" 2>/dev/null) || { fail "cannot inspect the running $reference_service project"; return 1; }
    project=$(printf '%s' "$reference_inspect" | jq -er '
      if type == "array" then .[0].Config.Labels["com.docker.compose.project"]
      else .Config.Labels["com.docker.compose.project"] end
      | select(type == "string" and test("^[a-z0-9][a-z0-9_-]*$"))
    ') || { fail "running $reference_service project label is missing or invalid"; return 1; }
  else
    [ "$allow_empty" = 1 ] || { fail "running $reference_service container ID is unavailable"; return 1; }
    project=$(docker compose config --format json </dev/null 2>/dev/null | jq -er '.name | select(type == "string" and test("^[a-z0-9][a-z0-9_-]*$"))') || {
      fail 'Compose project name is unavailable for rollback inspection'
      return 1
    }
  fi
  if [ "$allow_empty" = 1 ]; then
    ids=$(docker ps --no-trunc -aq --filter "label=com.docker.compose.project=$project" 2>/dev/null)
  else
    ids=$(docker ps --no-trunc -q --filter "label=com.docker.compose.project=$project" 2>/dev/null)
  fi || {
    fail "cannot enumerate containers in the $reference_service project"
    return 1
  }
  if [ -z "$ids" ] && [ "$allow_empty" = 1 ]; then
    inventory=$(printf '[]' | python3 "$MODE_HELPER" containers --checkout "$CHECKOUT" --allow-empty) || {
      fail 'cannot capture the empty running blob inventory'
      return 1
    }
    write_immutable "$path" "$inventory"
    return $?
  fi
  [ -n "$ids" ] || { fail "no running containers were found in the $reference_service project"; return 1; }
  mapfile -t container_ids <<< "$ids"
  local found_reference=0
  for id in "${container_ids[@]}"; do
    [[ "$id" =~ ^[0-9a-f]{64}$ ]] || { fail 'project container enumeration returned an invalid container ID'; return 1; }
    [ "$id" = "$reference_id" ] && found_reference=1
  done
  [ "$found_reference" -eq 1 ] || { fail "running $reference_service container is absent from project enumeration"; return 1; }
  local container_mode_args=(--checkout "$CHECKOUT")
  [ "$allow_empty" = 1 ] && container_mode_args+=(--allow-empty)
  inventory=$(docker inspect "${container_ids[@]}" 2>/dev/null | \
    python3 "$MODE_HELPER" containers "${container_mode_args[@]}") || {
    fail 'cannot capture the allowlisted running blob inventory'
    return 1
  }
  write_immutable "$path" "$inventory"
}

validate_blob_authority() {
  local containers_file=$1 compose_file=$2 allow_empty=${3:-0} allow_unguarded_containers=${4:-0} allow_unguarded_compose=${5:-0} allow_missing_reports=${6:-0}
  local mode_action=validate
  local -a mode_args=()
  [ "$allow_missing_reports" = 1 ] && mode_action=deployment-validate
  [ "$allow_empty" = 1 ] && mode_args+=(--allow-empty-containers)
  [ "$allow_unguarded_containers" = 1 ] && mode_args+=(--allow-unguarded-initial-local-containers)
  [ "$allow_unguarded_compose" = 1 ] && mode_args+=(--allow-unguarded-initial-local)
  python3 "$MODE_HELPER" "$mode_action" \
    --state-dir "$CHECKOUT/.state/blob-mode" --report-root "$EVIDENCE_ROOT" \
    --containers "$containers_file" --compose "$compose_file" \
    --override "$OVERRIDE_FILE" --checkout "$CHECKOUT" "${mode_args[@]}"
}

approved_logging_transition() {
  case "$1:$2" in
    syslog-v1:syslog-v1|default:syslog-v1) return 0 ;;
    *) return 1 ;;
  esac
}

approved_login_code_logging_transition() {
  case "$1:$2" in
    true:false|false:false|unset:false|not-captured:false) return 0 ;;
    *) return 1 ;;
  esac
}

approved_initial_local_logging_transition() {
  local baseline_sha="$1" baseline_login_codes="$2" target_policy="$3" target_login_codes="$4"
  local default_json_logging_sha approved_syslog_logging_sha
  default_json_logging_sha=$(printf '%s' '{"config":{},"type":"json-file"}' | sha256sum | awk '{print $1}') || return 1
  approved_syslog_logging_sha=$(printf '%s' '{"config":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"},"type":"syslog"}' | sha256sum | awk '{print $1}') || return 1
  [ "$target_policy" = syslog-v1 ] && [ "$target_login_codes" = false ] || return 1
  if [ "$baseline_sha" = "$default_json_logging_sha" ] && \
     { [ "$baseline_login_codes" = true ] || [ "$baseline_login_codes" = false ] || [ "$baseline_login_codes" = unset ]; }; then
    printf 'json-file:%s' "$baseline_login_codes"
  elif [ "$baseline_sha" = "$approved_syslog_logging_sha" ] && [ "$baseline_login_codes" = false ]; then
    printf 'syslog-v1:false'
  else
    return 1
  fi
}

preflight_target_local_blob_authority() {
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local containers_file="$BASELINE_DIR/initial-local-baseline-containers.json"
    local compose_file="$BASELINE_DIR/initial-local-target-compose.json"
    local preflight_file="$BASELINE_DIR/initial-local-preflight.txt"
    local provenance_file="$BASELINE_DIR/initial-local-provenance.txt"
    local render_file="$BASELINE_DIR/initial-local-target-render.snapshot.json"
    local logging_profile_file="$BASELINE_DIR/initial-local-logging-profile-preflight.tsv"
    local containers_sha compose_sha logging_policy login_codes result failures=0
    [ "$INITIAL_LOCAL_ARTIFACT_SHA" = "$APPROVED_INITIAL_LOCAL_COMPOSE_SHA" ] || {
      fail 'verified initial-local artifact digest is unavailable'
      return 1
    }
    logging_policy=$(jq -er '.compose_logging_policy' "$render_file") || {
      fail 'initial-local target render is missing its Compose logging policy'
      return 1
    }
    login_codes=$(jq -er '.compose_log_login_codes' "$render_file") || {
      fail 'initial-local target render is missing its login-code logging policy'
      return 1
    }
    write_immutable "$logging_profile_file" '' || return 1
    if [ "$logging_policy" = syslog-v1 ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$logging_profile_file" preflight compose_logging_policy syslog-v1 "$logging_policy" "$result" || return 1
    if [ "$login_codes" = false ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$logging_profile_file" preflight compose_log_login_codes false "$login_codes" "$result" || return 1
    [ "$failures" -eq 0 ] || {
      fail 'initial-local target must use syslog and disable login-code logging before backup'
      return 1
    }
    capture_running_blob_inventory "$containers_file" || {
      fail 'cannot persist the unguarded local baseline inventory before backup'
      return 1
    }
    capture_compose_blob_inventory "$compose_file" || {
      fail 'cannot persist the pinned target Compose inventory before backup'
      return 1
    }
    containers_sha=$(sha256_file "$containers_file") || { fail 'cannot hash the captured local baseline inventory'; return 1; }
    compose_sha=$(sha256_file "$compose_file") || { fail 'cannot hash the captured target Compose inventory'; return 1; }
    if python3 "$MODE_HELPER" preflight-initial-local \
      --state-dir "$CHECKOUT/.state/blob-mode" \
      --baseline-containers "$containers_file" \
      --preflight-target-compose "$compose_file" \
      --target-artifact-sha256 "$INITIAL_LOCAL_ARTIFACT_SHA" \
      --override "$OVERRIDE_FILE" --checkout "$CHECKOUT" \
      --target-sha "$TARGET_SHA" --baseline-sha "$PREVIOUS_SHA" \
      --lock-path "$LOCK_PATH"; then
      :
    else
      if ! write_immutable "$preflight_file" \
        "result=rejected baseline_sha=$PREVIOUS_SHA target_sha=$TARGET_SHA target_artifact_sha256=$INITIAL_LOCAL_ARTIFACT_SHA baseline_containers_sha256=$containers_sha target_compose_inventory_sha256=$compose_sha"; then
        fail 'initial-local preflight rejected and its evidence could not be persisted; backup was not started'
        return 1
      fi
      fail 'initial-local baseline or pinned target render was rejected before backup'
      return 1
    fi
    write_immutable "$preflight_file" \
      "result=pass baseline_sha=$PREVIOUS_SHA target_sha=$TARGET_SHA target_artifact_sha256=$INITIAL_LOCAL_ARTIFACT_SHA baseline_containers_sha256=$containers_sha target_compose_inventory_sha256=$compose_sha" || {
      fail 'initial-local preflight evidence could not be persisted; backup was not started'
      return 1
    }
    write_immutable "$provenance_file" \
      "baseline_source=running-unguarded-containers baseline_fact_source=docker-inspect baseline_exposure_source=docker-inspect baseline_key_mount_source=docker-inspect baseline_environment_source=docker-inspect baseline_pgdata_mount_source=docker-inspect baseline_sha=$PREVIOUS_SHA target_source=pinned-target-compose target_exposure_source=pinned-target-compose target_key_mount_source=pinned-target-compose target_environment_source=pinned-target-compose target_pgdata_source=pinned-target-compose target_compose_selection=$COMPOSE_FILE target_sha=$TARGET_SHA target_artifact_sha256=$INITIAL_LOCAL_ARTIFACT_SHA baseline_containers_sha256=$containers_sha target_compose_inventory_sha256=$compose_sha" || {
      fail 'initial-local source provenance could not be persisted; backup was not started'
      return 1
    }
    return 0
  fi
  local compose_file="$TARGET_DIR/pre-backup-blob-compose.json"
  local containers_file="$TARGET_DIR/pre-backup-blob-containers.json"
  local compose_sha artifact_sha=not-pinned artifact_content_sha=not-pinned backend
  local compose_contract=reviewed-artifact blob_dir=not-authoritative
  if [ "$TRACKED_LOCAL_AUTHORITY" = 1 ]; then
    compose_contract=tracked-local-authority
    blob_dir=$TRACKED_LOCAL_BLOB_DIR
  fi
  if set_target_local_compose_pin; then
    artifact_sha=$(sha256_file "$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE") || {
      fail 'cannot hash target-local Compose artifact before backup'
      return 1
    }
    artifact_content_sha=$(sha256_without_comment_lines "$CHECKOUT/$PINNED_LOCAL_COMPOSE_RUNTIME_FILE") || {
      fail 'cannot hash target-local Compose content before backup'
      return 1
    }
  fi
  capture_compose_blob_inventory "$compose_file" || {
    fail 'target-local Compose authority preflight failed before backup'
    return 1
  }
  capture_running_blob_inventory "$containers_file" || {
    fail 'running blob authority preflight failed before backup'
    return 1
  }
  compose_sha=$(sha256_file "$compose_file") || { fail 'cannot hash the pre-backup Compose authority render'; return 1; }
  if ! validate_blob_authority "$containers_file" "$compose_file" 0 0 0 1; then
    write_immutable "$TARGET_DIR/pre-backup-blob-authority-rejected.txt" \
      "result=rejected target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA compose_sha256=$artifact_sha render_sha256=$compose_sha" || return 1
    fail 'durable blob authority rejected before backup; target was not started'
    return 1
  fi
  backend=$(jq -er '[.services[].backend.kind] | unique | select(length == 1) | .[0]' "$compose_file") || {
    fail 'pre-backup Compose authority render has no single blob backend'
    return 1
  }
  if [ "$backend" = local ] && [ "$artifact_sha" = not-pinned ] && \
     [ "$TRACKED_LOCAL_AUTHORITY" != 1 ]; then
    write_immutable "$TARGET_DIR/pre-backup-blob-authority-rejected.txt" \
      "result=rejected target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA compose_sha256=not-pinned render_sha256=$compose_sha reason=unpinned-local-target" || return 1
    fail 'local-backend apply requires a reviewed local Compose artifact or unchanged carried-forward content'
    return 1
  fi
  write_immutable "$TARGET_DIR/pre-backup-blob-authority.txt" \
    "result=pass target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA compose_sha256=$artifact_sha compose_content_sha256=$artifact_content_sha render_sha256=$compose_sha backend=$backend compose_contract=$compose_contract blob_dir=$blob_dir"
}

validate_baseline() {
  local file=$1 image state grace timeout_value result_file
  image=$(jq -er '.image_id | select(test("^sha256:[0-9a-f]{64}$"))' "$file") || { fail 'baseline image identity is invalid'; return 1; }
  state=$(jq -er '.state' "$file") || { fail 'baseline state is missing'; return 1; }
  grace=$(jq -er '.stop_grace_period' "$file") || { fail 'baseline grace is missing'; return 1; }
  timeout_value=$(jq -er '.container_stop_timeout' "$file") || { fail 'baseline inspected timeout is missing'; return 1; }
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    [ "$(jq -er '.provenance.compose_source' "$file")" = live-container-inspection ] && \
      [ "$(jq -er '.provenance.stop_grace_period_source' "$file")" = docker-inspect-config-stop-timeout ] && \
      [ "$(jq -er '.compose_replica_count' "$file")" = not-captured ] && \
      [ "$(jq -er '.compose_client_addr_trust' "$file")" = not-captured ] || {
      fail 'initial-local baseline snapshot must contain inspected live facts without target Compose fields'
      return 1
    }
  else
    [ "$(jq -er '.provenance.compose_source' "$file")" = selected-compose-render ] || {
      fail 'baseline snapshot Compose provenance is invalid'
      return 1
    }
  fi
  BASE_IMAGE_ID=$image
  result_file="$BASELINE_DIR/baseline-policy.tsv"
  write_immutable "$result_file" '' || return 1
  BASELINE_POLICY_FAILURES=0
  local field expected actual result
  for field in baseline_state baseline_compose_grace baseline_inspected_stop_timeout; do
    case "$field" in
      baseline_state) expected=running; actual=$state ;;
      baseline_compose_grace) expected=2m0s; actual=$grace ;;
      baseline_inspected_stop_timeout) expected=120; actual=$timeout_value ;;
    esac
    if [ "$expected" = "$actual" ]; then result=pass; else result=fail; BASELINE_POLICY_FAILURES=$((BASELINE_POLICY_FAILURES + 1)); fi
    append_evidence_row "$result_file" baseline "$field" "$expected" "$actual" "$result" || return 1
  done
  [ "$BASELINE_POLICY_FAILURES" -eq 0 ] || { fail 'baseline is not in the approved running/grace state'; return 1; }
}

protected_env_name() {
  case "$1" in
    TG_RSA_KEY_*|TG_AUTHKEY_ENC_KEY*|TG_POSTGRES_*|POSTGRES_*|TG_BLOB_*|RUSTFS_*|TG_REPLICA_*|TG_CLIENT_ADDR_TRUST|COMPOSE_*|DOCKER_*) return 0 ;;
  esac
  return 1
}

unquote_dotenv_value() {
  local value=$1 length=${#1}
  if (( length >= 2 )); then
    case "$value" in
      \"*\"|\'*\')
        value=${value:1:$((length - 2))}
        ;;
    esac
  fi
  printf '%s' "$value"
}

named_env_addition_requested() {
  local name
  local -a requested
  [ -n "$ADDED_ENV" ] || return 1
  IFS=, read -r -a requested <<< "$ADDED_ENV"
  for name in "${requested[@]}"; do
    case "$name" in
      TG_PUBLIC_WEBSOCKET_LISTEN_ADDR|TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR|TG_PUBLIC_REGISTRATION|TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS|TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET|TG_USER_INVITE_QUOTA)
        return 0
        ;;
    esac
  done
  return 1
}

changed_websocket_origins_requested() {
  local name
  local -a requested
  [ -n "$CHANGED_ENV" ] || return 1
  IFS=, read -r -a requested <<< "$CHANGED_ENV"
  for name in "${requested[@]}"; do
    [ "$name" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && return 0
  done
  return 1
}

changed_tailnet_exchange_limit_requested() {
  local name
  local -a requested
  [ -n "$CHANGED_ENV" ] || return 1
  IFS=, read -r -a requested <<< "$CHANGED_ENV"
  for name in "${requested[@]}"; do
    [ "$name" = TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET ] && return 0
  done
  return 1
}

restore_previous_env_for_named_changes() {
  local env_dir temp
  [ "$CHANGED_ENV_SOURCE" != override ] || return 0
  named_env_addition_requested || changed_websocket_origins_requested || changed_tailnet_exchange_limit_requested || return 0
  [ -n "$PREVIOUS_ENV_FILE" ] && [ -f "$PREVIOUS_ENV_FILE" ] && [ ! -L "$PREVIOUS_ENV_FILE" ] && \
    [ "$(stat -c %a -- "$PREVIOUS_ENV_FILE")" = 600 ] && [ "$(stat -c %u -- "$PREVIOUS_ENV_FILE")" = 0 ] || {
    fail 'named environment rollback requires the root-owned mode-0600 previous .env'
    return 1
  }
  [ -f "$ENV_FILE" ] && [ ! -L "$ENV_FILE" ] && \
    [ "$(stat -c %a -- "$ENV_FILE")" = 600 ] && [ "$(stat -c %u -- "$ENV_FILE")" = 0 ] || {
    fail 'cannot restore named environment rollback over an unsafe .env path'
    return 1
  }
  env_dir=$(dirname -- "$ENV_FILE") || { fail 'cannot resolve the .env directory for named environment rollback'; return 1; }
  temp=$(mktemp "$env_dir/.rollout-env-rollback.XXXXXXXX") || {
    fail 'cannot stage the previous .env for named environment rollback'
    return 1
  }
  if ! cp -- "$PREVIOUS_ENV_FILE" "$temp" || ! chmod 600 -- "$temp" || ! sync; then
    rm -f -- "$temp"
    fail 'cannot prepare the previous .env for named environment rollback'
    return 1
  fi
  if ! mv -f -- "$temp" "$ENV_FILE"; then
    rm -f -- "$temp"
    fail 'cannot restore the previous .env for named environment rollback'
    return 1
  fi
  sync || { fail 'cannot persist the previous .env for named environment rollback'; return 1; }
}

restore_previous_override_for_named_changes() {
  local override_dir override_mode temp
  [ "$CHANGED_ENV_SOURCE" = override ] || return 0
  [ -n "$PREVIOUS_OVERRIDE_FILE" ] && [ -f "$PREVIOUS_OVERRIDE_FILE" ] && [ ! -L "$PREVIOUS_OVERRIDE_FILE" ] && \
    [ "$(stat -c %a -- "$PREVIOUS_OVERRIDE_FILE")" = 600 ] && [ "$(stat -c %u -- "$PREVIOUS_OVERRIDE_FILE")" = 0 ] || {
    fail 'named override rollback requires the root-owned mode-0600 previous Compose override'
    return 1
  }
  [ -f "$OVERRIDE_FILE" ] && [ ! -L "$OVERRIDE_FILE" ] && [ "$(stat -c %u -- "$OVERRIDE_FILE")" = 0 ] || {
    fail 'cannot restore named override rollback over an unsafe Compose override path'
    return 1
  }
  override_mode=$(stat -c %a -- "$OVERRIDE_FILE") || { fail 'cannot read the Compose override mode'; return 1; }
  [[ "$override_mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$override_mode & 0022) == 0 )) || {
    fail 'Compose override permissions must not allow group or world writes'
    return 1
  }
  override_dir=$(dirname -- "$OVERRIDE_FILE") || { fail 'cannot resolve the Compose override directory'; return 1; }
  temp=$(mktemp "$override_dir/.rollout-override-rollback.XXXXXXXX") || {
    fail 'cannot stage the previous Compose override for named rollback'
    return 1
  }
  if ! cp -- "$PREVIOUS_OVERRIDE_FILE" "$temp" || ! chmod "$override_mode" -- "$temp" || ! sync; then
    rm -f -- "$temp"
    fail 'cannot prepare the previous Compose override for named rollback'
    return 1
  fi
  if ! mv -f -- "$temp" "$OVERRIDE_FILE"; then
    rm -f -- "$temp"
    fail 'cannot restore the previous Compose override for named rollback'
    return 1
  fi
  sync || { fail 'cannot persist the previous Compose override for named rollback'; return 1; }
}

override_websocket_origins_value() {
  local file=$1
  awk '
    function trim(value) {
      sub(/^[[:space:]]+/, "", value)
      sub(/[[:space:]]+$/, "", value)
      return value
    }
    /^services:[[:space:]]*$/ { services=1; next }
    services && /^[^[:space:]#][^:]*:/ { services=0; teagramd=0; environment=0 }
    services && /^  [^[:space:]][^:]*:[[:space:]]*$/ {
      teagramd=($0 ~ /^  teagramd:[[:space:]]*$/)
      environment=0
      next
    }
    teagramd && /^    [^[:space:]][^:]*:/ {
      environment=($0 ~ /^    environment:[[:space:]]*$/)
      next
    }
    environment && /^      TG_WEBSOCKET_ALLOWED_ORIGINS:[[:space:]]*/ {
      count++
      value=$0
      sub(/^      TG_WEBSOCKET_ALLOWED_ORIGINS:[[:space:]]*/, "", value)
      value=trim(value)
      if (length(value) >= 2 && substr(value,1,1) == substr(value,length(value),1) &&
          (substr(value,1,1) == "\"" || substr(value,1,1) == "\047")) {
        value=substr(value,2,length(value)-2)
      } else if (value ~ /[[:space:]#]/ || value ~ /[\047\"`]/) {
        invalid=1
      }
      found=value
    }
    END {
      if (count != 1 || invalid || found == "") exit 1
      print found
    }
  ' "$file"
}

validate_override_websocket_change() {
  local previous_size previous_sha previous_value current_value override_mode
  local i old_line line changed=0 old_value new_value
  local -a previous_lines current_lines
  [ -n "$CHANGED_ENV" ] && [ "$CHANGED_ENV" = TG_WEBSOCKET_ALLOWED_ORIGINS ] && \
    [ -z "$ADDED_ENV" ] && [ -z "$PREVIOUS_ENV_FILE" ] || {
    fail 'an override backup is supported only for a named TG_WEBSOCKET_ALLOWED_ORIGINS value change'
    return 1
  }
  [ -f "$PREVIOUS_OVERRIDE_FILE" ] && [ ! -L "$PREVIOUS_OVERRIDE_FILE" ] && \
    [ "$(stat -c %a -- "$PREVIOUS_OVERRIDE_FILE")" = 600 ] && [ "$(stat -c %u -- "$PREVIOUS_OVERRIDE_FILE")" = 0 ] || {
    fail 'override value changes require the pre-change override as a root-owned mode-0600 regular file'
    return 1
  }
  [ -f "$OVERRIDE_FILE" ] && [ ! -L "$OVERRIDE_FILE" ] && \
    [ "$(stat -c %u -- "$OVERRIDE_FILE")" = 0 ] || {
    fail 'override value changes require a root-owned regular non-symlink override file'
    return 1
  }
  override_mode=$(stat -c %a -- "$OVERRIDE_FILE") || { fail 'cannot read the Compose override mode'; return 1; }
  [[ "$override_mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$override_mode & 0022) == 0 )) || {
    fail 'Compose override permissions must not allow group or world writes'
    return 1
  }
  previous_size=$(stat -c %s -- "$PREVIOUS_OVERRIDE_FILE") || { fail 'cannot size the previous Compose override'; return 1; }
  [ "$previous_size" -eq 0 ] || [ -z "$(tail -c 1 -- "$PREVIOUS_OVERRIDE_FILE")" ] || {
    fail 'previous Compose override must end with a newline'
    return 1
  }
  [ -z "$(tail -c 1 -- "$OVERRIDE_FILE")" ] || { fail 'Compose override must end with a newline'; return 1; }
  previous_sha=$(sha256_file "$PREVIOUS_OVERRIDE_FILE") || { fail 'cannot hash the previous Compose override'; return 1; }
  [ -n "$RECORDED_OVERRIDE_SHA" ] && [ "$previous_sha" = "$RECORDED_OVERRIDE_SHA" ] || {
    fail 'previous Compose override is not the last verified override'
    return 1
  }
  previous_value=$(override_websocket_origins_value "$PREVIOUS_OVERRIDE_FILE") || {
    fail 'previous Compose override must set TG_WEBSOCKET_ALLOWED_ORIGINS once under services.teagramd.environment'
    return 1
  }
  current_value=$(override_websocket_origins_value "$OVERRIDE_FILE") || {
    fail 'Compose override must set TG_WEBSOCKET_ALLOWED_ORIGINS once under services.teagramd.environment'
    return 1
  }
  ! grep -Eq '^[[:space:]]*(export[[:space:]]+)?TG_WEBSOCKET_ALLOWED_ORIGINS[[:space:]]*=' -- "$ENV_FILE" || {
    fail 'override value changes require TG_WEBSOCKET_ALLOWED_ORIGINS to be absent from .env'
    return 1
  }
  [ "$previous_value" != "$current_value" ] || {
    fail 'TG_WEBSOCKET_ALLOWED_ORIGINS change must change the override value'
    return 1
  }
  mapfile -t previous_lines < "$PREVIOUS_OVERRIDE_FILE"
  mapfile -t current_lines < "$OVERRIDE_FILE"
  [ "${#previous_lines[@]}" -eq "${#current_lines[@]}" ] || { fail 'Compose override changes or removes unrelated lines'; return 1; }
  for ((i = 0; i < ${#previous_lines[@]}; i++)); do
    old_line=${previous_lines[$i]}
    line=${current_lines[$i]}
    if [ "$old_line" != "$line" ]; then
      [[ "$old_line" =~ ^[[:space:]]*TG_WEBSOCKET_ALLOWED_ORIGINS: ]] && \
        [[ "$line" =~ ^[[:space:]]*TG_WEBSOCKET_ALLOWED_ORIGINS: ]] || {
        fail 'Compose override changes or removes unrelated lines'
        return 1
      }
      old_value=${old_line#*:}
      new_value=${line#*:}
      old_value=$(printf '%s' "$old_value" | sed -E 's/^[[:space:]]*//; s/[[:space:]]*$//; s/^(["\047])(.*)\1$/\2/')
      new_value=$(printf '%s' "$new_value" | sed -E 's/^[[:space:]]*//; s/[[:space:]]*$//; s/^(["\047])(.*)\1$/\2/')
      [ "$old_value" != "$new_value" ] || { fail 'Compose override line changes only formatting'; return 1; }
      changed=$((changed + 1))
    fi
  done
  [ "$changed" -eq 1 ] || { fail 'Compose override must change only the named origin assignment'; return 1; }
}

# Before the first record exists, the last verified run's evidence holds the
# .env digest that target or rollback ran with.
read_verified_env_evidence() {
  local result dir snapshot stamp best_stamp='' best_snapshot='' env_sha
  for result in "$EVIDENCE_ROOT"/main1238-*.target/target-result.txt "$EVIDENCE_ROOT"/main1238-*.rollback/rollback-result.txt; do
    [ -f "$result" ] && [ ! -L "$result" ] && grep -q '^result=verified ' "$result" || continue
    dir=${result%/*}
    case "$dir" in
      *.target) snapshot="$dir/target.snapshot.json" ;;
      *) snapshot="$dir/rollback.snapshot.json" ;;
    esac
    [ -f "$snapshot" ] && [ ! -L "$snapshot" ] || continue
    stamp=${dir##*-}
    stamp=${stamp%.*}
    [[ "$stamp" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] && [[ "$stamp" > "$best_stamp" ]] || continue
    best_stamp=$stamp
    best_snapshot=$snapshot
  done
  [ -n "$best_snapshot" ] || return 0
  env_sha=$(jq -er 'select(.provenance.compose_source == "selected-compose-render") | .env_sha256 | select(test("^[0-9a-f]{64}$"))' "$best_snapshot") || {
    fail 'latest verified evidence has no .env digest'
    return 1
  }
  RECORDED_ENV_SHA=$env_sha
  RECORDED_ENV_FROM=$best_snapshot
}

read_verified_override_evidence() {
  local result dir snapshot stamp best_stamp='' best_snapshot='' override_sha
  for result in "$EVIDENCE_ROOT"/main1238-*.target/target-result.txt "$EVIDENCE_ROOT"/main1238-*.rollback/rollback-result.txt; do
    [ -f "$result" ] && [ ! -L "$result" ] && grep -q '^result=verified ' "$result" || continue
    dir=${result%/*}
    case "$dir" in
      *.target) snapshot="$dir/target.snapshot.json" ;;
      *) snapshot="$dir/rollback.snapshot.json" ;;
    esac
    [ -f "$snapshot" ] && [ ! -L "$snapshot" ] || continue
    stamp=${dir##*-}
    stamp=${stamp%.*}
    [[ "$stamp" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] && [[ "$stamp" > "$best_stamp" ]] || continue
    best_stamp=$stamp
    best_snapshot=$snapshot
  done
  [ -n "$best_snapshot" ] || return 0
  override_sha=$(jq -er 'select(.provenance.compose_source == "selected-compose-render") | .override_sha256 | select(test("^[0-9a-f]{64}$"))' "$best_snapshot") || {
    fail 'latest verified evidence has no Compose override digest'
    return 1
  }
  RECORDED_OVERRIDE_SHA=$override_sha
  RECORDED_OVERRIDE_FROM=$best_snapshot
}

read_env_record() {
  local record
  RECORDED_ENV_SHA=
  RECORDED_ENV_FROM=
  if [ ! -e "$ENV_RECORD" ] && [ ! -L "$ENV_RECORD" ]; then
    read_verified_env_evidence
    return
  fi
  [ -f "$ENV_RECORD" ] && [ ! -L "$ENV_RECORD" ] && \
    [ "$(stat -c %a -- "$ENV_RECORD")" = 600 ] && [ "$(stat -c %u -- "$ENV_RECORD")" = 0 ] || {
    fail 'verified .env record must be a root-owned mode-0600 regular file'
    return 1
  }
  record=$(cat -- "$ENV_RECORD") || { fail 'cannot read the verified .env record'; return 1; }
  [[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=[0-9a-f]{40}$ ]] || {
    fail 'verified .env record is malformed'
    return 1
  }
  RECORDED_ENV_SHA=${BASH_REMATCH[1]}
  RECORDED_ENV_FROM=record
}

# Persist a validated .env digest and the release that was verified with it.
persist_verified_env_record() {
  local env_sha=$1 source_sha=$2 state_dir temp
  [[ "$env_sha" =~ ^[0-9a-f]{64}$ ]] || { fail 'verified .env digest is invalid'; return 1; }
  [[ "$source_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'verified .env source SHA is invalid'; return 1; }
  state_dir=$(dirname -- "$ENV_RECORD")
  if [ ! -e "$state_dir" ] && [ ! -L "$state_dir" ]; then
    mkdir -m 700 -- "$state_dir" || { fail 'cannot create the deployment state directory'; return 1; }
  fi
  [ -d "$state_dir" ] && [ ! -L "$state_dir" ] || { fail 'deployment state directory is not a directory'; return 1; }
  temp=$(mktemp "$state_dir/.rollout-env.XXXXXXXX") || { fail 'cannot stage the verified .env record'; return 1; }
  if ! printf 'env_sha256=%s source_sha=%s\n' "$env_sha" "$source_sha" > "$temp" || \
     ! chmod 600 -- "$temp" || ! sync || ! mv -f -- "$temp" "$ENV_RECORD" || ! sync; then
    rm -f -- "$temp"
    fail 'cannot publish the verified .env record'
    return 1
  fi
}

# Each verified target or rollback records the .env it ran with; the next
# apply starts from that file, named additions, or one explicitly supported value change.
record_verified_env() {
  local snapshot=$1 source_sha=$2 env_sha
  env_sha=$(jq -er '.env_sha256 | select(test("^[0-9a-f]{64}$"))' "$snapshot") || {
    fail 'verified snapshot has no .env digest to record'
    return 1
  }
  persist_verified_env_record "$env_sha" "$source_sha"
}

# The current .env must be the last verified one. A release may add variables
# only when the deploy names them and keeps that file: the current file must be
# it plus one assignment per named variable, none already in the live container.
validate_env_baseline() {
  local baseline_file="$BASELINE_DIR/baseline.snapshot.json"
  local name names=' ' added=' ' added_count=0 line key previous_size previous_sha current_sha prefix_sha baseline_env_sha baseline_added
  local change_names=' ' changed=' ' changed_count=0 old_line old_key old_value new_value i
  local -a requested previous_lines current_lines
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    [ -z "$ADDED_ENV" ] && [ -z "$CHANGED_ENV" ] && [ -z "$PREVIOUS_ENV_FILE" ] && \
      [ -z "$PREVIOUS_OVERRIDE_FILE" ] && [ "$ADOPT_ENV" = 0 ] || {
      fail 'initialize-local does not accept environment changes or adoption'
      return 1
    }
    return 0
  fi
  [ "$ADOPT_ENV" = 0 ] || [ "$ADOPT_ENV" = 1 ] || { fail 'ROLLOUT_RUNNER_ADOPT_ENV must be 0 or 1'; return 1; }
  read_env_record || return 1
  current_sha=$(sha256_file "$ENV_FILE") || { fail 'cannot hash the environment file'; return 1; }
  baseline_env_sha=$(jq -er '.env_sha256' "$baseline_file") || { fail 'baseline environment digest is missing'; return 1; }
  [ "$baseline_env_sha" = "$current_sha" ] || { fail 'environment file changed after the baseline snapshot'; return 1; }
  if [ "$ADOPT_ENV" = 1 ]; then
    [ -z "$RECORDED_ENV_SHA" ] || {
      if [ "$RECORDED_ENV_FROM" = record ]; then
        fail 'a verified .env is already recorded; adoption is only for the first guarded apply'
      else
        fail 'a verified .env is already in evidence; adoption is only for a host without one'
      fi
      return 1
    }
    [ -z "$ADDED_ENV" ] && [ -z "$CHANGED_ENV" ] && [ -z "$PREVIOUS_ENV_FILE" ] && \
      [ -z "$PREVIOUS_OVERRIDE_FILE" ] || { fail '.env adoption does not accept environment changes'; return 1; }
    write_immutable "$BASELINE_DIR/env-baseline.txt" "recorded_env_sha256=adopted recorded_from=adopted env_sha256=$current_sha added=none"
    return
  fi
  [ -n "$RECORDED_ENV_SHA" ] || {
    fail 'no verified .env is recorded or in evidence; adopt the live .env once with ROLLOUT_RUNNER_ADOPT_ENV=1'
    return 1
  }
  if [ -z "$ADDED_ENV" ]; then
    if [ -n "$CHANGED_ENV" ]; then
      if [ "$CHANGED_ENV_SOURCE" = override ]; then
        read_verified_override_evidence || return 1
        [ "$current_sha" = "$RECORDED_ENV_SHA" ] || {
          fail 'override value change requires the last verified .env to remain unchanged'
          return 1
        }
        validate_override_websocket_change || return 1
        write_immutable "$BASELINE_DIR/env-baseline.txt" \
          "recorded_env_sha256=$RECORDED_ENV_SHA recorded_from=$RECORDED_ENV_FROM env_sha256=$current_sha added=none changed=$CHANGED_ENV source=override previous_override_sha256=$RECORDED_OVERRIDE_SHA previous_override_from=$RECORDED_OVERRIDE_FROM current_override_sha256=$(sha256_file "$OVERRIDE_FILE")"
        return
      fi
      [ -z "$PREVIOUS_ENV_FILE" ] && { fail 'named environment value changes require the pre-change .env'; return 1; }
      [[ "$CHANGED_ENV" != *,* ]] || { fail 'only one existing environment value may change per apply'; return 1; }
      IFS=, read -r -a requested <<< "$CHANGED_ENV"
      [ "${#requested[@]}" -eq 1 ] || { fail 'environment value changes list is empty'; return 1; }
      for name in "${requested[@]}"; do
        [[ "$name" =~ ^[A-Z][A-Z0-9_]*$ ]] || { fail 'environment value changes must be upper-case variable names'; return 1; }
        case "$name" in
          TG_REGISTRATION|TG_TURNSTILE_SIGNIN_ENABLED|TG_WEBSOCKET_ALLOWED_ORIGINS|TG_PUBLIC_WEBSOCKET_LISTEN_ADDR|TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR|TG_PUBLIC_REGISTRATION|TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS|TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET) ;;
          *) fail "environment value change $name is not an approved setting"; return 1 ;;
        esac
        change_names+="$name "
      done
      [ -f "$PREVIOUS_ENV_FILE" ] && [ ! -L "$PREVIOUS_ENV_FILE" ] && \
        [ "$(stat -c %a -- "$PREVIOUS_ENV_FILE")" = 600 ] && [ "$(stat -c %u -- "$PREVIOUS_ENV_FILE")" = 0 ] || {
        fail 'environment value changes require the pre-change .env as a root-owned mode-0600 regular file'
        return 1
      }
      previous_size=$(stat -c %s -- "$PREVIOUS_ENV_FILE") || { fail 'cannot size the previous environment file'; return 1; }
      [ "$previous_size" -eq 0 ] || [ -z "$(tail -c 1 -- "$PREVIOUS_ENV_FILE")" ] || {
        fail 'previous environment file must end with a newline'
        return 1
      }
      [ -z "$(tail -c 1 -- "$ENV_FILE")" ] || { fail 'environment file must end with a newline'; return 1; }
      previous_sha=$(sha256_file "$PREVIOUS_ENV_FILE") || { fail 'cannot hash the previous environment file'; return 1; }
      [ "$previous_sha" = "$RECORDED_ENV_SHA" ] || {
        fail 'previous environment file is not the last verified .env'
        return 1
      }
      mapfile -t previous_lines < "$PREVIOUS_ENV_FILE"
      mapfile -t current_lines < "$ENV_FILE"
      [ "${#previous_lines[@]}" -eq "${#current_lines[@]}" ] || {
        fail 'environment value change adds or removes .env lines'
        return 1
      }
      for ((i = 0; i < ${#previous_lines[@]}; i++)); do
        old_line=${previous_lines[$i]}
        line=${current_lines[$i]}
        old_key=${old_line%%=*}
        key=${line%%=*}
        if [[ "$change_names" == *" $old_key "* ]]; then
          [ "$old_line" != "$old_key" ] && [ "$line" != "$key" ] && [ "$key" = "$old_key" ] || {
            fail 'environment value change must replace an existing assignment in place'
            return 1
          }
          old_value=${old_line#*=}
          new_value=${line#*=}
          case "$key" in
            TG_REGISTRATION|TG_PUBLIC_REGISTRATION)
              old_value=$(unquote_dotenv_value "$old_value")
              new_value=$(unquote_dotenv_value "$new_value")
              ;;
          esac
          case "$key:$old_value:$new_value" in
            TG_REGISTRATION:closed:open|TG_REGISTRATION:open:closed) ;;
            TG_TURNSTILE_SIGNIN_ENABLED:false:true|TG_TURNSTILE_SIGNIN_ENABLED:true:false) ;;
            TG_PUBLIC_REGISTRATION:closed:invite|TG_PUBLIC_REGISTRATION:invite:closed) ;;
            TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET:*)
              [[ "$old_value" =~ ^[0-9]+$ && "$new_value" =~ ^[0-9]+$ && "$old_value" != "$new_value" ]] || {
                fail 'TG_RATE_LIMIT_KEY_EXCHANGE_IP_TAILNET may change only between distinct non-negative integers'
                return 1
              }
              ;;
            TG_PUBLIC_WEBSOCKET_LISTEN_ADDR:*) [ "$old_value" != "$new_value" ] || { fail 'TG_PUBLIC_WEBSOCKET_LISTEN_ADDR change must change the value'; return 1; } ;;
            TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR:*) [ "$old_value" != "$new_value" ] || { fail 'TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR change must change the value'; return 1; } ;;
            TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS:*) [ "$old_value" != "$new_value" ] || { fail 'TG_PUBLIC_WEBSOCKET_ALLOWED_ORIGINS change must change the value'; return 1; } ;;
            TG_WEBSOCKET_ALLOWED_ORIGINS:*) [ "$old_value" != "$new_value" ] || { fail 'TG_WEBSOCKET_ALLOWED_ORIGINS change must change the value'; return 1; } ;;
            TG_REGISTRATION:*) fail 'TG_REGISTRATION may change only between closed and open'; return 1 ;;
            TG_TURNSTILE_SIGNIN_ENABLED:*) fail 'TG_TURNSTILE_SIGNIN_ENABLED may change only between true and false'; return 1 ;;
            TG_PUBLIC_REGISTRATION:*) fail 'TG_PUBLIC_REGISTRATION may change only between invite and closed'; return 1 ;;
            *) fail "environment value change $key is not an approved setting"; return 1 ;;
          esac
          [[ "$changed" != *" $key "* ]] || { fail "environment value $key is assigned more than once"; return 1; }
          changed+="$key "
          changed_count=$((changed_count + 1))
        else
          [ "$old_line" = "$line" ] || {
            fail 'environment file changes or removes settings outside the named value change'
            return 1
          }
        fi
      done
      [ "$changed_count" -eq "${#requested[@]}" ] || {
        fail 'environment value change is not assigned exactly once'
        return 1
      }
      baseline_added=$(jq -er '.container_env_added' "$baseline_file") || { fail 'baseline added-environment record is missing'; return 1; }
      [ "$baseline_added" = none ] || {
        fail "environment value change is not allowed while additions are live: $baseline_added"
        return 1
      }
      write_immutable "$BASELINE_DIR/env-baseline.txt" \
        "recorded_env_sha256=$RECORDED_ENV_SHA recorded_from=$RECORDED_ENV_FROM env_sha256=$current_sha added=none changed=$CHANGED_ENV"
      return
    fi
    [ -z "$PREVIOUS_ENV_FILE" ] || { fail 'a previous environment file requires named environment additions or value changes'; return 1; }
    [ "$current_sha" = "$RECORDED_ENV_SHA" ] || {
      fail 'environment file differs from the last verified .env; name additions, name a supported value change, or restore it'
      return 1
    }
    write_immutable "$BASELINE_DIR/env-baseline.txt" \
      "recorded_env_sha256=$RECORDED_ENV_SHA recorded_from=$RECORDED_ENV_FROM env_sha256=$current_sha added=none"
    return
  fi
  [ -z "$CHANGED_ENV" ] || { fail 'an apply cannot add and change environment variables together'; return 1; }
  [ -z "$PREVIOUS_OVERRIDE_FILE" ] || { fail 'a previous Compose override requires a named WebSocket origin value change'; return 1; }
  IFS=, read -r -a requested <<< "$ADDED_ENV"
  [ "${#requested[@]}" -gt 0 ] || { fail 'environment additions list is empty'; return 1; }
  for name in "${requested[@]}"; do
    [[ "$name" =~ ^[A-Z][A-Z0-9_]*$ ]] || { fail 'environment additions must be upper-case variable names'; return 1; }
    ! protected_env_name "$name" || { fail "environment addition $name is a protected deployment setting"; return 1; }
    [[ "$names" != *" $name "* ]] || { fail "environment addition $name is named twice"; return 1; }
    names+="$name "
  done
  [ -n "$PREVIOUS_ENV_FILE" ] && [ -f "$PREVIOUS_ENV_FILE" ] && [ ! -L "$PREVIOUS_ENV_FILE" ] && \
    [ "$(stat -c %a -- "$PREVIOUS_ENV_FILE")" = 600 ] && [ "$(stat -c %u -- "$PREVIOUS_ENV_FILE")" = 0 ] || {
    fail 'environment additions require the pre-change .env as a root-owned mode-0600 regular file'
    return 1
  }
  previous_size=$(stat -c %s -- "$PREVIOUS_ENV_FILE") || { fail 'cannot size the previous environment file'; return 1; }
  [ "$previous_size" -eq 0 ] || [ -z "$(tail -c 1 -- "$PREVIOUS_ENV_FILE")" ] || {
    fail 'previous environment file must end with a newline'
    return 1
  }
  [ -z "$(tail -c 1 -- "$ENV_FILE")" ] || { fail 'environment file must end with a newline'; return 1; }
  previous_sha=$(sha256_file "$PREVIOUS_ENV_FILE") || { fail 'cannot hash the previous environment file'; return 1; }
  prefix_sha=$(head -c "$previous_size" -- "$ENV_FILE" | sha256sum) || { fail 'cannot hash the environment file prefix'; return 1; }
  [ "${prefix_sha%% *}" = "$previous_sha" ] || {
    fail 'environment file changes or removes settings of the previous .env'
    return 1
  }
  while IFS= read -r line; do
    key=${line%%=*}
    [ "$key" != "$line" ] && [[ "$names" == *" $key "* ]] || {
      fail 'environment file adds a line that is not a named environment addition'
      return 1
    }
    [[ "$added" != *" $key "* ]] || { fail "environment addition $key is assigned twice"; return 1; }
    added+="$key "
    added_count=$((added_count + 1))
  done < <(tail -c +"$((previous_size + 1))" -- "$ENV_FILE")
  [ "$added_count" -eq "${#requested[@]}" ] || {
    fail 'environment file does not assign every named environment addition'
    return 1
  }
  for name in "${requested[@]}"; do
    ! grep -Eq "^[[:space:]]*(export[[:space:]]+)?$name[[:space:]]*=" -- "$PREVIOUS_ENV_FILE" || {
      fail "environment addition $name is already set in the previous .env"
      return 1
    }
  done
  [ "$previous_sha" = "$RECORDED_ENV_SHA" ] || {
    fail 'previous environment file is not the last verified .env'
    return 1
  }
  baseline_added=$(jq -er '.container_env_added' "$baseline_file") || { fail 'baseline added-environment record is missing'; return 1; }
  [ "$baseline_added" = none ] || {
    fail "environment addition is already set in the live container: $baseline_added"
    return 1
  }
  write_immutable "$BASELINE_DIR/env-baseline.txt" \
    "recorded_env_sha256=$RECORDED_ENV_SHA recorded_from=$RECORDED_ENV_FROM env_sha256=$current_sha added=$ADDED_ENV"
}

capture_backup_and_restore() {
  local postgres_id postgres_inspect postgres_image dump_tmp dump_path dump_stderr globals_path globals_tmp globals_stderr
  local dump_rc dump_sha globals_rc globals_sha restore_name restore_start restore_stderr target_database ready=0 restore_rc=1 cleanup_rc=1 i
  postgres_id=$(current_service_id postgres) || { fail 'Postgres container ID is unavailable'; return 1; }
  postgres_inspect=$(docker inspect "$postgres_id" 2>/dev/null) || { fail 'cannot inspect Postgres image identity'; return 1; }
  postgres_image=$(printf '%s' "$postgres_inspect" | jq -er 'if type == "array" then .[0].Image else .Image end | select(test("^sha256:[0-9a-f]{64}$"))') || {
    fail 'Postgres image identity is invalid'
    return 1
  }
  local source_database
  source_database=$(docker compose config --format json </dev/null | jq -er '.services.postgres.environment.POSTGRES_DB | select(type == "string" and test("^[A-Za-z_][A-Za-z0-9_]*$"))') || {
    fail 'source Compose database name is unavailable'
    return 1
  }
  target_database=$TARGET_DATABASE
  [[ "$target_database" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
    fail 'target Compose database name is unavailable'
    return 1
  }
  dump_path="$BACKUP_DIR/pgdump-pre-$TARGET_SHA.sql"
  dump_stderr="$BACKUP_DIR/pgdump.stderr"
  globals_path="$BACKUP_DIR/pgglobals-pre-$TARGET_SHA.sql"
  globals_stderr="$BACKUP_DIR/pgglobals.stderr"
  [ ! -e "$dump_path" ] && [ ! -L "$dump_path" ] || { fail 'database dump evidence already exists'; return 1; }
  write_immutable "$dump_stderr" '' || return 1
  write_immutable "$globals_stderr" '' || return 1
  dump_tmp=$(mktemp "$BACKUP_DIR/.pgdump.XXXXXXXX") || { fail 'cannot allocate database dump'; return 1; }
  if docker compose exec -T postgres pg_dump -U postgres "$source_database" </dev/null >"$dump_tmp" 2>"$dump_stderr"; then
    dump_rc=0
  else
    dump_rc=$?
  fi
  chmod 600 -- "$dump_tmp" "$dump_stderr" || { rm -f -- "$dump_tmp"; fail 'cannot protect database dump evidence'; return 1; }
  if [ "$dump_rc" -ne 0 ] || ! tail -c 200 "$dump_tmp" | grep -qi 'dump complete'; then
    rm -f -- "$dump_tmp"
    write_immutable "$BACKUP_DIR/backup-result.txt" "dump_exit=$dump_rc dump_complete=no isolated_restore=not_run" || return 1
    fail 'fresh database dump did not complete'
    return 1
  fi
  if ! ln -- "$dump_tmp" "$dump_path"; then
    rm -f -- "$dump_tmp"
    fail 'cannot publish database dump without overwrite'
    return 1
  fi
  rm -- "$dump_tmp" || { fail 'cannot remove dump staging link'; return 1; }
  sync || { fail 'cannot sync database dump'; return 1; }
  dump_sha=$(sha256_file "$dump_path") || { fail 'cannot hash database dump'; return 1; }
  globals_tmp=$(mktemp "$BACKUP_DIR/.pgglobals.XXXXXXXX") || { fail 'cannot allocate role globals dump'; return 1; }
  if docker compose exec -T postgres pg_dumpall --globals-only -U postgres </dev/null >"$globals_tmp" 2>"$globals_stderr"; then
    globals_rc=0
  else
    globals_rc=$?
  fi
  chmod 600 -- "$globals_tmp" "$globals_stderr" || { rm -f -- "$globals_tmp"; fail 'cannot protect role globals evidence'; return 1; }
  if [ "$globals_rc" -ne 0 ] || [ ! -s "$globals_tmp" ]; then
    rm -f -- "$globals_tmp"
    write_immutable "$BACKUP_DIR/backup-result.txt" "dump_exit=0 globals_exit=$globals_rc isolated_restore=not_run" || return 1
    fail 'PostgreSQL role globals dump did not complete'
    return 1
  fi
  [ ! -e "$globals_path" ] && [ ! -L "$globals_path" ] || { rm -f -- "$globals_tmp"; fail 'role globals evidence already exists'; return 1; }
  if ! ln -- "$globals_tmp" "$globals_path"; then
    rm -f -- "$globals_tmp"
    fail 'cannot publish role globals without overwrite'
    return 1
  fi
  rm -- "$globals_tmp" || { fail 'cannot remove role globals staging link'; return 1; }
  sync || { fail 'cannot sync role globals dump'; return 1; }
  globals_sha=$(sha256_file "$globals_path") || { fail 'cannot hash role globals dump'; return 1; }
  restore_name="main1238-restore-${TARGET_SHA:0:12}-${RUN_ID##*-}"
  if docker inspect --type container "$restore_name" >/dev/null 2>&1; then
    fail 'isolated restore container name already exists'
    return 1
  fi
  restore_start="$BACKUP_DIR/restore-start.stderr"
  restore_stderr="$BACKUP_DIR/restore.stderr"
  write_immutable "$restore_start" '' || return 1
  write_immutable "$restore_stderr" '' || return 1
  if docker run --detach --name "$restore_name" --network none --log-driver none \
      --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
      --env PGDATA=/var/lib/postgresql/18/docker \
      --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB="$target_database" \
      "$PROJECT_MOVE_PG_IMAGE" </dev/null >/dev/null 2>"$restore_start"; then
    restore_rc=0
  else
    restore_rc=$?
  fi
  if [ "$restore_rc" -eq 0 ]; then
    for ((i=0; i<60; i++)); do
      if docker exec "$restore_name" pg_isready -h 127.0.0.1 -U postgres -d "$target_database" >/dev/null 2>>"$restore_stderr"; then
        ready=1
        break
      fi
      sleep 1
    done
    if [ "$ready" -eq 1 ] && restore_database_dump "$restore_name" "$target_database" "$globals_path" "$dump_path" \
        >/dev/null 2>>"$restore_stderr"; then
      restore_rc=0
    else
      restore_rc=1
    fi
    if cleanup_restore_container "$restore_name" "$restore_stderr"; then cleanup_rc=0; else cleanup_rc=1; fi
  else
    if restore_container_present "$restore_name" "$restore_stderr"; then
      if cleanup_restore_container "$restore_name" "$restore_stderr"; then cleanup_rc=0; else cleanup_rc=1; fi
    else
      inspect_rc=$?
      if [ "$inspect_rc" -eq 1 ]; then
        cleanup_rc=0
      else
        cleanup_rc=1
      fi
    fi
  fi
  chmod 600 -- "$restore_start" "$restore_stderr" || { fail 'cannot protect isolated restore evidence'; return 1; }
  if [ "$ready" -eq 1 ] && [ "$restore_rc" -eq 0 ] && [ "$cleanup_rc" -eq 0 ]; then
    write_immutable "$BACKUP_DIR/backup-manifest.txt" \
      "target_sha=$TARGET_SHA source_database=$source_database target_database=$target_database dump_path=$dump_path dump_sha256=$dump_sha globals_path=$globals_path globals_sha256=$globals_sha dump_complete=yes postgres_source_image_id=$postgres_image postgres_restore_image=$PROJECT_MOVE_PG_IMAGE isolated_restore=pass restore_network=none restore_data=tmpfs restore_cleanup=pass" || return 1
    printf 'backup_restore=pass dump_sha256=%s globals_sha256=%s target_database=%s\n' "$dump_sha" "$globals_sha" "$target_database"
    return 0
  fi
  write_immutable "$BACKUP_DIR/backup-result.txt" \
    "target_sha=$TARGET_SHA source_database=$source_database target_database=$target_database dump_sha256=$dump_sha globals_sha256=$globals_sha dump_complete=yes isolated_restore_ready=$ready restore_exit=$restore_rc restore_cleanup_exit=$cleanup_rc" || return 1
  fail 'isolated restore or cleanup did not pass'
}

record_build_identity() {
  local built_image checked_out_sha image_file image_tag revision_label revision_label_json
  image_tag="$(rollout_compose_service):local"
  BUILD_IDENTITY_REJECTION_REASON=built-image-id-invalid
  checked_out_sha=$(git rev-parse HEAD) || { fail 'cannot read checked-out SHA after fast-forward'; return 1; }
  [ "$checked_out_sha" = "$TARGET_SHA" ] || { fail 'checked-out SHA changed before image capture'; return 1; }
  built_image=$(docker image inspect "$image_tag" --format '{{.Id}}' 2>/dev/null) || { fail 'cannot inspect built application image tag'; return 1; }
  [[ "$built_image" =~ ^sha256:[0-9a-f]{64}$ ]] || { fail 'built teagramd image ID is missing or malformed'; return 1; }
  BUILD_IDENTITY_REJECTION_REASON=built-image-revision-label-unavailable
  revision_label=$(docker image inspect "$image_tag" \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' 2>/dev/null) || {
    fail 'cannot inspect built teagramd revision label'
    return 1
  }
  BUILD_IDENTITY_REJECTION_REASON=built-image-revision-label-mismatch
  if [ "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" = "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA" ] && \
     [ "$revision_label" != "$checked_out_sha" ]; then
    fail 'built image revision label differs from the validated application target'
    return 1
  fi
  revision_label_json=$(jq -cn --arg label "$revision_label" '$label') || {
    fail 'cannot encode the observed built image revision label'
    return 1
  }
  BUILD_IDENTITY_REJECTION_REASON=built-image-id-invalid
  image_file="$BUILD_DIR/built-image.identity"
  write_immutable "$image_file" \
    "captured_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) source_sha=$checked_out_sha image_id=$built_image revision_label_json=$revision_label_json" || return 1
  BUILT_IMAGE_ID=$built_image
  printf 'built_image_bound=source:%s image:%s evidence:%s\n' "$checked_out_sha" "$built_image" "$image_file"
}

verify_forwarded_build_config() {
  local rendered checkout context revision provenance
  [ "$PINNED_LOCAL_COMPOSE_CONTENT_SHA" = "$SOURCE_LOCAL_COMPOSE_CONTENT_SHA" ] || return 0
  rendered=$(TEAGRAMD_SOURCE_REVISION="$TARGET_SHA" docker compose config --format json </dev/null | \
    jq -cer '
      .services.teagramd.build as $build |
      {
        context: ($build.context | select(type == "string")),
        revision: ($build.args.TEAGRAMD_SOURCE_REVISION | select(type == "string"))
      }
    ') || {
    fail 'cannot resolve the reviewed Compose build revision and context'
    return 1
  }
  checkout=$(readlink -f -- "$CHECKOUT") || { fail 'cannot resolve the validated application checkout'; return 1; }
  context=$(jq -er '.context' <<<"$rendered") || { fail 'resolved Compose build context is missing'; return 1; }
  context=$(readlink -f -- "$context") || { fail 'resolved Compose build context is not a checkout path'; return 1; }
  [ "$context" = "$checkout" ] || {
    fail 'resolved Compose build context differs from the validated application checkout'
    return 1
  }
  revision=$(jq -er '.revision' <<<"$rendered") || { fail 'resolved Compose build revision is missing'; return 1; }
  [ "$revision" = "$TARGET_SHA" ] || {
    fail 'resolved Compose build revision differs from the validated application target'
    return 1
  }
  provenance=$(jq -cn --arg source_sha "$TARGET_SHA" --arg context "$context" --arg revision "$revision" \
    '{source_sha:$source_sha,context:$context,revision:$revision}') || {
    fail 'cannot encode resolved Compose build provenance'
    return 1
  }
  write_immutable "$BUILD_DIR/build-compose-provenance.json" "$provenance"
}

target_compare() {
  local rc expected_logging actual_logging result
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    if compare_initial_local_target; then return 0; else rc=$?; fi
    printf 'target_compare=reject exit=%s rows=%s\n' "$rc" "$(wc -l < "$TARGET_DIR/target-comparisons.tsv" 2>/dev/null || printf 0)" >&2
    return "$rc"
  fi
  if ! copy_immutable "$BASELINE_DIR/baseline.snapshot.json" "$TARGET_DIR/baseline.snapshot.json"; then return 1; fi
  if ROLLOUT_CHECKOUT_PATH="$CHECKOUT" bash "$VERIFIER" compare \
      "$TARGET_DIR/baseline.snapshot.json" "$TARGET_DIR/target.snapshot.json" \
      "$BUILT_IMAGE_ID" "$TARGET_DIR" target >/dev/null; then
    expected_logging=$(jq -er '.compose_logging_policy' "$TARGET_DIR/preflight.snapshot.json") || {
      fail 'preflight snapshot is missing its Compose logging policy'
      return 1
    }
    actual_logging=$(jq -er '.compose_logging_policy' "$TARGET_DIR/target.snapshot.json") || {
      fail 'target snapshot is missing its Compose logging policy'
      return 1
    }
    if [ "$expected_logging" = "$actual_logging" ]; then result=pass; else result=fail; fi
    append_evidence_row "$TARGET_DIR/target-comparisons.tsv" target preflight_compose_logging_policy \
      "$expected_logging" "$actual_logging" "$result" || return 1
    if [ "$result" = pass ]; then return 0; fi
    fail 'target Compose logging policy changed after preflight'
    return 1
  else rc=$?; fi
  printf 'target_compare=reject exit=%s rows=%s\n' "$rc" "$(wc -l < "$TARGET_DIR/target-comparisons.tsv" 2>/dev/null || printf 0)" >&2
  return 1
}

compare_initial_local_target() {
  local baseline_file="$BASELINE_DIR/baseline.snapshot.json"
  local render_file="$BASELINE_DIR/initial-local-target-render.snapshot.json"
  local target_file="$TARGET_DIR/target.snapshot.json"
  local results="$TARGET_DIR/target-comparisons.tsv"
  local failures=0 field before after result expected_image
  local baseline_fields=(mounts_sha256 container_stop_timeout exposure_sha256 container_env_sha256)
  local render_fields=(config_sha256 env_sha256 override_sha256 stop_grace_period compose_replica_count compose_client_addr_trust compose_public_ws_2446 compose_logging_policy compose_log_login_codes)
  write_immutable "$results" '' || return 1
  [ "$(jq -er '.provenance.compose_source' "$baseline_file")" = live-container-inspection ] && \
    [ "$(jq -er '.provenance.compose_source' "$render_file")" = selected-compose-render ] && \
    [ "$(jq -er '.provenance.compose_source' "$target_file")" = selected-compose-render ] || {
    fail 'initial-local target comparison has invalid baseline or render provenance'
    return 1
  }
  for field in "${baseline_fields[@]}"; do
    if [ "$field" = container_env_sha256 ]; then
      before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'initial-local target render is missing container environment digest'; return 1; }
    else
      before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail "initial-local baseline is missing $field"; return 1; }
    fi
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail "initial-local target is missing $field"; return 1; }
    if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" target "live_baseline_$field" "$before" "$after" "$result" || return 1
  done
  before=$(jq -er '.exposure_public_ws_2446' "$baseline_file") || { fail 'initial-local baseline is missing its inspected public WebSocket port'; return 1; }
  after=$(jq -er '.exposure_public_ws_2446' "$target_file") || { fail 'initial-local target is missing its inspected public WebSocket port'; return 1; }
  if [ "$before" = "$after" ] || { [ "$before" = false ] && [ "$after" = true ]; }; then
    result=pass
  else
    result=fail
    failures=$((failures + 1))
  fi
  append_evidence_row "$results" target live_baseline_exposure_public_ws_2446 "$before" "$after" "$result" || return 1
  for field in "${render_fields[@]}"; do
    before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail "initial-local target render is missing $field"; return 1; }
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail "initial-local target is missing $field"; return 1; }
    if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" target "selected_render_$field" "$before" "$after" "$result" || return 1
  done
  expected_image=$BUILT_IMAGE_ID
  after=$(jq -er '.container_id' "$target_file") || { fail 'initial-local target container ID is missing'; return 1; }
  before=$(jq -er '.container_id' "$baseline_file") || { fail 'initial-local baseline container ID is missing'; return 1; }
  if [ -n "$before" ] && [ "$after" != "$before" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
  append_evidence_row "$results" target container_replaced "$before" "$after" "$result" || return 1
  after=$(jq -er '.image_id' "$target_file") || { fail 'initial-local target image ID is missing'; return 1; }
  if [ "$after" = "$expected_image" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
  append_evidence_row "$results" target target_image_matches_built "$expected_image" "$after" "$result" || return 1
  after=$(jq -er '.state' "$target_file") || { fail 'initial-local target state is missing'; return 1; }
  if [ "$after" = running ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
  append_evidence_row "$results" target target_state running "$after" "$result" || return 1
  for field in state container_stop_timeout stop_grace_period; do
    case "$field" in
      state) expected=running; after=$(jq -er '.state' "$baseline_file") || return 1 ;;
      container_stop_timeout) expected=120; after=$(jq -er '.container_stop_timeout' "$baseline_file") || return 1 ;;
      stop_grace_period) expected=2m0s; after=$(jq -er '.stop_grace_period' "$baseline_file") || return 1 ;;
    esac
    if [ "$expected" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" target "live_baseline_$field" "$expected" "$after" "$result" || return 1
  done
  for field in compose_replica_count compose_client_addr_trust container_replica_count container_client_addr_trust; do
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail "initial-local target is missing $field"; return 1; }
    if [ "$field" = compose_replica_count ] || [ "$field" = container_replica_count ]; then expected=1; else expected=socket; fi
    if [ "$after" = "$expected" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" target "target_$field" "$expected" "$after" "$result" || return 1
  done
  [ "$failures" -eq 0 ] || { fail 'initial-local target does not match inspected baseline and pinned render policy'; return 1; }
}

run_target_readiness() {
  local id=$1
  ROLLOUT_CHECKOUT_PATH="$CHECKOUT" ROLLOUT_READY_SECONDS="$READY_SECONDS" \
    bash "$VERIFIER" ready target "$id" "$TARGET_DIR" >/dev/null
}

run_rollback_readiness() {
  local id=$1
  ROLLOUT_CHECKOUT_PATH="$CHECKOUT" ROLLOUT_READY_SECONDS="$READY_SECONDS" \
    bash "$VERIFIER" ready rollback "$id" "$ROLLBACK_DIR" >/dev/null
}

compare_resolved_preflight() {
  local old_file=${PREFLIGHT_BASELINE_SNAPSHOT:-"$BASELINE_DIR/baseline.snapshot.json"}
  local target_file=${PREFLIGHT_TARGET_SNAPSHOT:-"$TARGET_DIR/preflight.snapshot.json"}
  local results=${PREFLIGHT_RESULTS_FILE:-"$TARGET_DIR/preflight-comparisons.tsv"}
  local fields=(config_sha256 env_sha256 override_sha256 stop_grace_period compose_logging_policy compose_log_login_codes media_key_source media_key_current)
  local field before after result expected failures=0
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local render_file="$BASELINE_DIR/initial-local-target-render.snapshot.json"
    local live_fields=(container_id image_id state mounts_sha256 container_stop_timeout exposure_sha256 exposure_public_ws_2446 container_env_sha256 container_logging_sha256 container_log_login_codes container_replica_count container_client_addr_trust)
    local baseline_logging_sha baseline_login_codes target_logging_policy target_login_codes
    [ "$(jq -er '.provenance.compose_source' "$old_file")" = live-container-inspection ] && \
      [ "$(jq -er '.provenance.compose_source' "$render_file")" = selected-compose-render ] && \
      [ "$(jq -er '.provenance.compose_source' "$target_file")" = selected-compose-render ] || {
      fail 'initial-local preflight has invalid baseline or render provenance'
      return 1
    }
    write_immutable "$results" '' || return 1
    baseline_logging_sha=$(jq -er '.container_logging_sha256' "$old_file") || { fail 'live baseline is missing its container logging digest'; return 1; }
    baseline_login_codes=$(jq -er '.container_log_login_codes' "$old_file") || { fail 'live baseline is missing its inspected login-code logging setting'; return 1; }
    target_logging_policy=$(jq -er '.compose_logging_policy' "$render_file") || { fail 'pinned target render is missing its logging policy'; return 1; }
    target_login_codes=$(jq -er '.compose_log_login_codes' "$render_file") || { fail 'pinned target render is missing its login-code logging setting'; return 1; }
    if baseline_logging_transition=$(approved_initial_local_logging_transition \
      "$baseline_logging_sha" "$baseline_login_codes" "$target_logging_policy" "$target_login_codes"); then
      append_evidence_row "$results" preflight initial_local_logging_transition \
        "$baseline_logging_transition" syslog-v1:false pass || return 1
    else
      append_evidence_row "$results" preflight initial_local_logging_transition \
        "sha256:$baseline_logging_sha:$baseline_login_codes" "$target_logging_policy:$target_login_codes" fail || return 1
      fail 'initial-local preflight rejected an unsupported live logging profile'
      return 1
    fi
    for field in "${fields[@]}"; do
      before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'pinned target render is missing a preflight comparison field'; return 1; }
      after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'resolved target is missing a preflight comparison field'; return 1; }
      if [ "$field" = compose_logging_policy ]; then
        if approved_logging_transition "$before" "$after"; then result=pass; else result=fail; failures=$((failures + 1)); fi
      elif [ "$field" = compose_log_login_codes ]; then
        if approved_login_code_logging_transition "$before" "$after"; then result=pass; else result=fail; failures=$((failures + 1)); fi
      elif [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
      append_evidence_row "$results" preflight "selected_render_$field" "$before" "$after" "$result" || return 1
    done
    before=$(jq -er '.compose_public_ws_2446' "$render_file") || { fail 'pinned target render is missing the public WebSocket port policy'; return 1; }
    after=$(jq -er '.compose_public_ws_2446' "$target_file") || { fail 'resolved target is missing the public WebSocket port policy'; return 1; }
    if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" preflight selected_render_compose_public_ws_2446 "$before" "$after" "$result" || return 1
    for field in "${live_fields[@]}"; do
      if [ "$field" = container_env_sha256 ]; then
        before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'pinned target render is missing its container environment digest'; return 1; }
      else
        before=$(jq -er --arg field "$field" '.[$field]' "$old_file") || { fail 'live baseline is missing an inspected preflight comparison field'; return 1; }
      fi
      after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'preflight snapshot is missing an inspected baseline field'; return 1; }
      if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
      append_evidence_row "$results" preflight "live_baseline_$field" "$before" "$after" "$result" || return 1
    done
    for field in compose_replica_count compose_client_addr_trust; do
      after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'resolved target is missing an approved environment setting'; return 1; }
      if [ "$field" = compose_replica_count ]; then expected=1; else expected=socket; fi
      if [ "$expected" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
      append_evidence_row "$results" preflight "target_$field" "$expected" "$after" "$result" || return 1
    done
    [ "$failures" -eq 0 ] || { fail 'initial-local preflight changed live baseline facts or pinned target render'; return 1; }
    return 0
  fi
  write_immutable "$results" '' || return 1
  for field in "${fields[@]}"; do
    before=$(jq -er --arg field "$field" '.[$field]' "$old_file") || { fail 'baseline is missing a resolved configuration comparison field'; return 1; }
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'resolved target is missing a configuration comparison field'; return 1; }
    if [ "$field" = compose_logging_policy ]; then
      if approved_logging_transition "$before" "$after"; then result=pass; else result=fail; failures=$((failures + 1)); fi
    elif [ "$field" = compose_log_login_codes ]; then
      if approved_login_code_logging_transition "$before" "$after"; then result=pass; else result=fail; failures=$((failures + 1)); fi
    elif [ "$field" = media_key_source ] && [ "$before" = unset ] && [[ "$after" = /* ]]; then
      result=pass
    elif [ "$field" = media_key_current ] && [ "$before" = unset ] && [ "$after" = 1 ]; then
      result=pass
    elif [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" preflight "$field" "$before" "$after" "$result" || return 1
  done
  for field in compose_public_ws_2446 exposure_public_ws_2446; do
    before=$(jq -er --arg field "$field" '.[$field]' "$old_file") || { fail "baseline is missing $field"; return 1; }
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail "resolved target is missing $field"; return 1; }
    if [ "$before" = "$after" ] || {
      [ "$field" = compose_public_ws_2446 ] && [ "$before" = false ] && [ "$after" = true ];
    }; then
      result=pass
    else
      result=fail
      failures=$((failures + 1))
    fi
    append_evidence_row "$results" preflight "$field" "$before" "$after" "$result" || return 1
  done
  for field in compose_replica_count compose_client_addr_trust; do
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'resolved target is missing an approved environment setting'; return 1; }
    if [ "$field" = compose_replica_count ]; then expected=1; else expected=socket; fi
    if [ "$expected" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" preflight "$field" "$expected" "$after" "$result" || return 1
  done
  [ "$failures" -eq 0 ] || { fail 'resolved Compose preflight rejected unapproved drift'; return 1; }
}

compare_resolved_preflight_command() {
  local evidence_dir=$1 output_dir=${2:-$1} canonical input results public_rows rc=0 owner=0 sandbox=0 path expected_owner_mode
  if [ "${ROLLOUT_PREFLIGHT_REPLAY_SANDBOX:-0}" = 1 ]; then
    [ "$(id -u)" = 65534 ] || { fail 'preflight replay sandbox requires uid 65534'; return 1; }
    owner=65534
    sandbox=1
  elif [ "${ROLLOUT_PREFLIGHT_TEST_MODE:-0}" = 1 ]; then
    owner=$(id -u) || { fail 'cannot inspect preflight replay owner'; return 1; }
  else
    [ "$(id -u)" = 0 ] || { fail 'preflight replay requires uid 0'; return 1; }
  fi
  PREFLIGHT_EVIDENCE_OWNER=$owner
  for path in "$evidence_dir" "$output_dir"; do
    case "$path" in
      /tmp/*) ;;
      *) fail 'preflight replay evidence must be directly below /tmp'; return 64 ;;
    esac
    case "$path" in
      /tmp/*/*) fail 'preflight replay evidence must be directly below /tmp'; return 64 ;;
    esac
    case "$path" in
      *'/../'*|*/..|*'/./'*) fail 'preflight replay evidence path must be canonical'; return 64 ;;
    esac
    [ -d "$path" ] && [ ! -L "$path" ] || { fail 'preflight replay evidence directory is unavailable'; return 1; }
    canonical=$(readlink -f -- "$path") || return 1
    if [ "$sandbox" = 1 ] && [ "$path" = "$evidence_dir" ]; then
      [ "$canonical" = "$path" ] && [ "$(stat -c %u:%a -- "$path")" = "0:755" ] || {
        fail 'preflight replay snapshots have an invalid owner or mode'
        return 1
      }
    else
      [ "$canonical" = "$path" ] && [ "$(stat -c %u:%a -- "$path")" = "$owner:700" ] || {
        fail 'preflight replay evidence has an invalid owner or mode'
        return 1
      }
    fi
  done
  for input in "$evidence_dir/baseline.snapshot.json" "$evidence_dir/preflight.snapshot.json"; do
    expected_owner_mode="$owner:600"
    [ "$sandbox" = 1 ] && expected_owner_mode=0:444
    [ -f "$input" ] && [ ! -L "$input" ] && [ "$(stat -c %u:%a -- "$input")" = "$expected_owner_mode" ] || {
      fail 'preflight replay snapshots have an invalid owner or mode'
      return 1
    }
  done
  results="$output_dir/preflight-comparisons.tsv"
  [ ! -e "$results" ] && [ ! -L "$results" ] || { fail 'preflight replay comparison already exists'; return 64; }
  BASELINE_DIR=$evidence_dir
  TARGET_DIR=$output_dir
  PREFLIGHT_BASELINE_SNAPSHOT="$evidence_dir/baseline.snapshot.json"
  PREFLIGHT_TARGET_SNAPSHOT="$evidence_dir/preflight.snapshot.json"
  PREFLIGHT_RESULTS_FILE=$results
  INITIALIZE_LOCAL=0
  if compare_resolved_preflight; then :; else rc=$?; fi
  [ -f "$results" ] && [ ! -L "$results" ] && [ "$(stat -c %u:%a -- "$results")" = "$owner:600" ] || {
    fail 'preflight replay comparison evidence is unavailable'
    return 1
  }
  # Preserve empty private values; Bash treats TSV tabs as collapsible IFS whitespace.
  if ! public_rows=$(awk -F '\t' '
    BEGIN {
      count = split("config_sha256 env_sha256 override_sha256 stop_grace_period compose_logging_policy compose_log_login_codes media_key_source media_key_current compose_public_ws_2446 exposure_public_ws_2446 compose_replica_count compose_client_addr_trust", fields, " ")
      for (i = 1; i <= count; i++) allowed[fields[i]] = 1
    }
    # write_immutable creates an empty TSV with one newline before rows append.
    NR == 1 && NF == 0 { next }
    NF != 6 || $2 != "preflight" || !($3 in allowed) || $6 !~ /^(pass|fail)$/ { exit 1 }
    { printf "%s\t%s\n", $3, $6 }
  ' "$results"); then
    fail 'preflight replay comparison evidence is malformed'
    return 1
  fi
  printf '%s\n' "$public_rows"
  return "$rc"
}

compare_rollback_to_baseline() {
  local baseline_file="$ROLLBACK_DIR/baseline.snapshot.json"
  local rollback_file="$ROLLBACK_DIR/rollback.snapshot.json"
  local results="$ROLLBACK_DIR/rollback-equivalence.tsv"
  local fields=(image_id mounts_sha256 stop_grace_period container_stop_timeout config_sha256 env_sha256 override_sha256 exposure_sha256 container_env_sha256 container_logging_sha256 compose_replica_count compose_client_addr_trust container_replica_count container_client_addr_trust compose_public_ws_2446 exposure_public_ws_2446 compose_logging_policy compose_log_login_codes container_log_login_codes media_key_source media_key_current)
  local field before after state timeout_value grace
  local baseline_logging_sha rollback_logging_sha baseline_login_codes rollback_login_codes baseline_logging_transition
  local target_logging_policy target_login_codes rollback_logging_policy rollback_login_codes_value
  local approved_syslog_logging_sha
  local initial_local_logging_transition=0
  local expected_count=${#fields[@]}
  ROLLBACK_COMPARE_FAILURES=0
  copy_immutable "$BASELINE_DIR/baseline.snapshot.json" "$baseline_file" || return 1
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local render_file="$ROLLBACK_DIR/initial-local-target-render.snapshot.json"
    local target_render="$BASELINE_DIR/initial-local-target-render.snapshot.json"
    local live_fields=(image_id mounts_sha256 container_stop_timeout exposure_sha256 exposure_public_ws_2446 container_env_sha256 container_logging_sha256 container_log_login_codes)
    local render_fields=(config_sha256 env_sha256 override_sha256 stop_grace_period compose_replica_count compose_client_addr_trust compose_public_ws_2446 compose_logging_policy compose_log_login_codes media_key_source media_key_current)
    [ "$(jq -er '.provenance.compose_source' "$baseline_file")" = live-container-inspection ] && \
      [ "$(jq -er '.provenance.compose_source' "$target_render")" = selected-compose-render ] && \
      [ "$(jq -er '.provenance.compose_source' "$rollback_file")" = selected-compose-render ] || {
      fail 'initial-local rollback has invalid baseline or render provenance'
      return 1
    }
    copy_immutable "$target_render" "$render_file" || return 1
    baseline_logging_sha=$(jq -er '.container_logging_sha256' "$baseline_file") || { fail 'live baseline is missing its container logging digest'; return 1; }
    rollback_logging_sha=$(jq -er '.container_logging_sha256' "$rollback_file") || { fail 'rollback is missing its container logging digest'; return 1; }
    baseline_login_codes=$(jq -er '.container_log_login_codes' "$baseline_file") || { fail 'live baseline is missing its inspected login-code logging setting'; return 1; }
    rollback_login_codes=$(jq -er '.container_log_login_codes' "$rollback_file") || { fail 'rollback is missing its inspected login-code logging setting'; return 1; }
    target_logging_policy=$(jq -er '.compose_logging_policy' "$render_file") || { fail 'pinned target render is missing its logging policy'; return 1; }
    target_login_codes=$(jq -er '.compose_log_login_codes' "$render_file") || { fail 'pinned target render is missing its login-code logging setting'; return 1; }
    rollback_logging_policy=$(jq -er '.compose_logging_policy' "$rollback_file") || { fail 'rollback render is missing its logging policy'; return 1; }
    rollback_login_codes_value=$(jq -er '.compose_log_login_codes' "$rollback_file") || { fail 'rollback render is missing its login-code logging setting'; return 1; }
    approved_syslog_logging_sha=$(printf '%s' '{"config":{"syslog-address":"unixgram:///dev/log","syslog-facility":"daemon","syslog-format":"rfc5424","tag":"teagramd"},"type":"syslog"}' | sha256sum | awk '{print $1}') || return 1
    # initialize-local keeps its reviewed syslog artifact for rollback.
    if baseline_logging_transition=$(approved_initial_local_logging_transition \
       "$baseline_logging_sha" "$baseline_login_codes" "$target_logging_policy" "$target_login_codes") && \
       [ "$rollback_logging_sha" = "$approved_syslog_logging_sha" ] && \
       [ "$rollback_login_codes" = false ] && \
       [ "$rollback_logging_policy" = syslog-v1 ] && [ "$rollback_login_codes_value" = false ]; then
      initial_local_logging_transition=1
    fi
    write_immutable "$results" '' || return 1
    if [ "$initial_local_logging_transition" = 1 ]; then
      append_evidence_row "$results" rollback initial_local_logging_transition \
        "$baseline_logging_transition" syslog-v1:false pass || return 1
    fi
    for field in "${live_fields[@]}"; do
      if [ "$field" = container_env_sha256 ]; then
        before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'pinned target render is missing its container environment digest'; return 1; }
      else
        before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail 'live baseline is missing a rollback comparison field'; return 1; }
      fi
      after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback snapshot is missing an inspected comparison field'; return 1; }
      if [ "$initial_local_logging_transition" = 1 ] && [ "$field" = container_logging_sha256 ]; then
        append_evidence_row "$results" rollback "live_baseline_$field" "$before" "$after" pass || return 1
      elif [ "$initial_local_logging_transition" = 1 ] && [ "$field" = container_log_login_codes ] && [ "$before" != "$after" ]; then
        append_evidence_row "$results" rollback "live_baseline_$field" "$before" "$after" pass || return 1
      else
        record_rollback_row "$results" "live_baseline_$field" "$before" "$after" || return 1
      fi
    done
    for field in "${render_fields[@]}"; do
      before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'pinned target render is missing a rollback comparison field'; return 1; }
      after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback render is missing a comparison field'; return 1; }
      record_rollback_row "$results" "selected_render_$field" "$before" "$after" || return 1
    done
    for field in container_replica_count container_client_addr_trust; do
      before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail 'live baseline is missing an inspected environment setting'; return 1; }
      after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback is missing an inspected environment setting'; return 1; }
      if [ "$field" = container_replica_count ]; then expected=1; else expected=socket; fi
      if { [ "$before" = unset ] || [ "$before" = "$expected" ]; } && [ "$after" = "$expected" ]; then result=pass; else result=fail; fi
      append_evidence_row "$results" rollback "target_$field" "$before" "$after" "$result" || return 1
      [ "$result" = pass ] || ROLLBACK_COMPARE_FAILURES=$((ROLLBACK_COMPARE_FAILURES + 1))
    done
    state=$(jq -er '.state' "$rollback_file") || { fail 'rollback state is missing'; return 1; }
    record_rollback_row "$results" rollback_state running "$state" || return 1
    grace=$(jq -er '.stop_grace_period' "$rollback_file") || { fail 'rollback grace is missing'; return 1; }
    record_rollback_row "$results" rollback_grace_policy 2m0s "$grace" || return 1
    timeout_value=$(jq -er '.container_stop_timeout' "$rollback_file") || { fail 'rollback inspected timeout is missing'; return 1; }
    record_rollback_row "$results" rollback_inspected_stop_timeout 120 "$timeout_value" || return 1
    [ "$ROLLBACK_COMPARE_FAILURES" -eq 0 ] || { fail 'rollback does not match the inspected baseline and pinned target render'; return 1; }
    return 0
  fi
  write_immutable "$results" '' || return 1
  for field in "${fields[@]}"; do
    if [ "$field" = env_sha256 ] && { named_env_addition_requested || changed_websocket_origins_requested || changed_tailnet_exchange_limit_requested; }; then
      [ -n "$RECORDED_ENV_SHA" ] || { fail 'named environment rollback has no verified previous .env digest'; return 1; }
      before=$RECORDED_ENV_SHA
    elif [ "$field" = override_sha256 ] && [ "$CHANGED_ENV_SOURCE" = override ]; then
      [ -n "$RECORDED_OVERRIDE_SHA" ] || { fail 'named override rollback has no verified previous Compose override digest'; return 1; }
      before=$RECORDED_OVERRIDE_SHA
    else
      before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail 'baseline snapshot is missing an equivalence field'; return 1; }
    fi
    after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback snapshot is missing an equivalence field'; return 1; }
    record_rollback_row "$results" "$field" "$before" "$after" || return 1
  done
  state=$(jq -er '.state' "$rollback_file") || { fail 'rollback state is missing'; return 1; }
  record_rollback_row "$results" rollback_state running "$state" || return 1
  grace=$(jq -er '.stop_grace_period' "$rollback_file") || { fail 'rollback grace is missing'; return 1; }
  record_rollback_row "$results" rollback_grace_policy 2m0s "$grace" || return 1
  timeout_value=$(jq -er '.container_stop_timeout' "$rollback_file") || { fail 'rollback inspected timeout is missing'; return 1; }
  record_rollback_row "$results" rollback_inspected_stop_timeout 120 "$timeout_value" || return 1
  [ "$expected_count" -eq 21 ] || { fail 'internal rollback equivalence field count changed'; return 1; }
  [ "$ROLLBACK_COMPARE_FAILURES" -eq 0 ] || { fail 'rollback does not match the captured baseline'; return 1; }
}

validate_rollback_blob_authority() {
  local compose_file="$ROLLBACK_DIR/rollback-blob-compose.json"
  local containers_file="$ROLLBACK_DIR/rollback-pre-up-blob-containers.json"
  capture_compose_blob_inventory "$compose_file" || return 1
  capture_running_blob_inventory "$containers_file" 1 || return 1
  validate_blob_authority "$containers_file" "$compose_file" 1 1 1 1
}

target_failure_marker() {
  local reason=$1
  write_immutable "$TARGET_DIR/target-failure.txt" \
    "captured_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) target_sha=$TARGET_SHA source_sha=$PREVIOUS_SHA reason=$reason target_snapshot=$( [ -f "$TARGET_DIR/target.snapshot.json" ] && sha256_file "$TARGET_DIR/target.snapshot.json" || printf unavailable )"
}

rollback_after_target_failure() {
  local reason=$1 rollback_rc=0
  if ! target_failure_marker "$reason"; then
    printf 'target failure marker unavailable; rolling back anyway (reason=%s)\n' "$reason" >&2
    if ! write_immutable "$ROLLBACK_DIR/target-failure-marker-warning.txt" \
        "target_sha=$TARGET_SHA reason=$reason target_failure_marker=unavailable"; then
      printf 'rollback evidence also could not persist the missing-marker warning (reason=%s)\n' "$reason" >&2
    fi
  fi
  if perform_rollback; then :; else rollback_rc=$?; fi
  printf 'target_failure_gate=%s rollback_exit=%s\n' "$reason" "$rollback_rc"
  [ "$rollback_rc" -eq 0 ] || return 2
}

restore_checkout_and_tag() {
  (umask 022; git reset --hard "$PREVIOUS_SHA") >/dev/null || { fail 'cannot restore the previous checkout'; return 1; }
  docker image tag "${BASE_IMAGE_ID#sha256:}" "$(rollout_compose_service):local" >/dev/null 2>&1 || { fail 'cannot restore the baseline image tag'; return 1; }
  restore_previous_env_for_named_changes || return 1
  restore_previous_override_for_named_changes || return 1
}

require_clean_build_checkout() {
  local changes build_inputs
  changes=$(git -C "$CHECKOUT" status --porcelain=v1 --untracked-files=no) || {
    fail 'cannot inspect tracked checkout changes'
    return 1
  }
  [ -z "$changes" ] || { fail 'deployment checkout has staged or unstaged tracked changes'; return 1; }
  build_inputs=$(git -C "$CHECKOUT" status --porcelain=v1 --untracked-files=all --ignored=matching -- cmd/ internal/ components/ utils/) || {
    fail 'cannot inspect Docker build inputs'
    return 1
  }
  [ -z "$build_inputs" ] || { fail 'deployment checkout has untracked Docker build inputs'; return 1; }
}

perform_rollback() {
  local rollback_id rollback_env_record
  restore_checkout_and_tag || return 1
  if ! validate_rollback_blob_authority; then
    write_immutable "$ROLLBACK_DIR/rollback-blob-authority-rejected.txt" \
      "result=rejected checkout_sha=$PREVIOUS_SHA reason=current-containers-or-baseline-render-mismatch" || return 1
    fail 'current containers or baseline rollback render do not satisfy durable blob authority; baseline was not started'
    return 1
  fi
  docker compose up -d --no-build --no-deps "$(rollout_compose_service)" </dev/null || { fail "baseline $(rollout_compose_service) recreation failed"; return 1; }
  restore_removed_public_profile_services || return 1
  local public_profile_readiness_rc=0
  if wait_for_restored_public_profile_health; then :; else public_profile_readiness_rc=$?; fi
  rollback_id=$(current_service_id "$(rollout_compose_service)") || { fail "rollback $(rollout_compose_service) container ID is unavailable"; return 1; }
  capture_snapshot "$rollback_id" "$ROLLBACK_DIR" rollback || return 1
  local equivalence_rc=0 readiness_rc=0
  if compare_rollback_to_baseline; then :; else equivalence_rc=$?; fi
  if run_rollback_readiness "$rollback_id"; then :; else readiness_rc=$?; fi
  if [ "$equivalence_rc" -eq 0 ] && [ "$readiness_rc" -eq 0 ] && [ "$public_profile_readiness_rc" -eq 0 ]; then
    if [ -n "$ADDED_ENV" ]; then
      rollback_env_record=$(jq -er '.container_env_added' "$ROLLBACK_DIR/rollback.snapshot.json") || {
        fail 'verified rollback snapshot has no added-environment record'
        return 1
      }
      if [ "$rollback_env_record" = none ]; then
        # The old container omitted the additions, so preserve the digest of
        # the .env it previously ran with for a named retry.
        persist_verified_env_record "$RECORDED_ENV_SHA" "$PREVIOUS_SHA" || return 1
      elif jq -en --arg requested "$ADDED_ENV" --arg retained "$rollback_env_record" \
          '($requested | split(",") | unique) == ($retained | split(",") | unique)' >/dev/null; then
        # The old Compose files retained every addition, so this .env is now
        # the verified baseline and can be retried without addition names.
        record_verified_env "$ROLLBACK_DIR/rollback.snapshot.json" "$PREVIOUS_SHA" || return 1
      else
        fail 'rollback retained only some named environment additions; cannot safely record the .env baseline'
        return 1
      fi
    elif [ -n "$CHANGED_ENV" ]; then
      rollback_env_record=$(jq -er '.container_env_changed' "$ROLLBACK_DIR/rollback.snapshot.json") || {
        fail 'verified rollback snapshot has no changed-environment record'
        return 1
      }
      [ "$rollback_env_record" = "$CHANGED_ENV" ] || {
        fail 'rollback did not retain every named environment value change'
        return 1
      }
      record_verified_env "$ROLLBACK_DIR/rollback.snapshot.json" "$PREVIOUS_SHA" || return 1
    else
      record_verified_env "$ROLLBACK_DIR/rollback.snapshot.json" "$PREVIOUS_SHA" || return 1
    fi
    write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
      "result=verified checkout_sha=$PREVIOUS_SHA container_id=$rollback_id image_id=$BASE_IMAGE_ID readiness=pass baseline_equivalence=pass public_profile_readiness=pass" || return 1
    printf 'rollback=verified baseline_sha=%s container_id=%s\n' "$PREVIOUS_SHA" "$rollback_id"
    return 0
  fi
  write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
    "result=failed checkout_sha=$PREVIOUS_SHA container_id=$rollback_id equivalence_exit=$equivalence_rc readiness_exit=$readiness_rc public_profile_readiness_exit=$public_profile_readiness_rc"
  fail 'rollback did not pass baseline-equivalence, readiness, and public-profile health gates'
}

run_apply() {
  local branch origin_sha previous_sha baseline_id target_id rc dir runtime_source_sha target_container_action
  local authority_preflight_done=0
  verify_target_local_compose || return 1
  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot fetch origin/main under deployment lock'; return 1; }
  branch=$(git -C "$CHECKOUT" branch --show-current) || { fail 'cannot read checkout branch'; return 1; }
  previous_sha=$(git -C "$CHECKOUT" rev-parse HEAD) || { fail 'cannot read baseline checkout SHA'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot read origin/main SHA'; return 1; }
  runtime_source_sha=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  [[ "$runtime_source_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'rollout runtime source SHA must be a full commit ID'; return 1; }
  { [ "$branch" = main ] || [ -z "$branch" ]; } || {
    fail 'deployment checkout must be on main or detached'
    return 1
  }
  [ "$previous_sha" = "$EXPECTED_BASELINE_SHA" ] || { fail 'live checkout differs from the expected baseline SHA'; return 1; }
  git -C "$CHECKOUT" cat-file -e "$TARGET_SHA^{commit}" 2>/dev/null || { fail 'authorized application target is unavailable'; return 1; }
  git -C "$CHECKOUT" cat-file -e "$runtime_source_sha^{commit}" 2>/dev/null || { fail 'reviewed rollout runtime source is unavailable'; return 1; }
  git -C "$CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" "$origin_sha" || {
    fail 'authorized application target is not a reviewed origin/main commit'
    return 1
  }
  git -C "$CHECKOUT" merge-base --is-ancestor "$runtime_source_sha" "$origin_sha" || {
    fail 'rollout runtime source is not a reviewed origin/main commit'
    return 1
  }
  if [ "$INITIALIZE_LOCAL" = 1 ] || set_target_local_compose_pin; then
    git -C "$CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" "$runtime_source_sha" || {
      fail 'reviewed runtime source does not descend from the fixed application target'
      return 1
    }
  fi
  if ! git -C "$CHECKOUT" merge-base --is-ancestor "$previous_sha" "$TARGET_SHA"; then
    [ "$INITIALIZE_LOCAL" = 1 ] && [ "$previous_sha" = "$INITIAL_LOCAL_LEGACY_BASELINE_SHA" ] && \
      [ "$TARGET_SHA" = "$INITIAL_LOCAL_COMPOSE_TARGET_SHA" ] || {
      fail 'authorized application target is not a fast-forward from the live baseline'
      return 1
    }
  fi
  verify_runtime_sources || return 1

  local layout_report layout_changed layout_diagnostics layout_differences difference
  layout_report=$(python3 "$STACK_MOVE_HELPER" inspect \
    --checkout "$CHECKOUT" --target-sha "$TARGET_SHA" \
    --env-file "$ENV_FILE" --override "$OVERRIDE_FILE") || {
    fail 'cannot compare target Compose project and persistent mounts before apply'
    return 1
  }
  layout_changed=$(printf '%s' "$layout_report" | jq -er '.changed | if type == "boolean" then tostring else error("invalid layout result") end') || {
    fail 'target Compose layout comparison is invalid'
    return 1
  }
  if [ "$layout_changed" != false ]; then
    layout_diagnostics=$(printf '%s' "$layout_report" | jq -er '
      .layout_diagnostics
      | select(type == "object")
      | .reason as $reason
      | .public_profile as $public_profile
      | .changed_components as $components
      | select(($reason | type) == "string")
      | select(($public_profile | type) == "string")
      | select(($components | type) == "array")
      | select(all($components[]; test("^(project|server|server_mounts|postgres_mounts|persistent_service_mounts|volumes)$")))
      | "reason=\($reason) components=\($components | join(",")) public_profile=\($public_profile)"
    ') || layout_diagnostics='unavailable'
    layout_differences=$(printf '%s' "$layout_report" | jq -ce '
      .differences
      | select(type == "array" and length > 0)
      | select(all(.[];
          type == "object"
          and keys == ["field", "mount_target", "service"]
          and (.service | type) == "string"
          and (.service | length) > 0
          and (.mount_target == null or ((.mount_target | type) == "string" and (.mount_target | length) > 0))
          and (.field | type) == "string"
          and (.field | length) > 0
        ))
      | map({service, mount_target, field})[]
    ') || {
      fail 'target Compose layout difference report is missing, malformed, or empty'
      return 1
    }
    while IFS= read -r difference; do
      if [ -n "$difference" ]; then
        printf 'layout_difference=%s\n' "$difference" >&2
      fi
    done <<< "$layout_differences"
    fail "Compose project or persistent mounts changed ($layout_diagnostics); use the explicit move-stack action"
    return 1
  fi
  TARGET_DATABASE=$(printf '%s' "$layout_report" | jq -er '.target_database | select(type == "string" and test("^[A-Za-z_][A-Za-z0-9_]*$"))') || {
    fail 'target Compose database name is invalid'
    return 1
  }

  PREVIOUS_SHA=$previous_sha
  if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    RUN_ID=${ROLLOUT_RUNNER_RUN_ID:-}
    BASELINE_DIR=${ROLLOUT_RUNNER_BASELINE_DIR:-}
    BACKUP_DIR=${ROLLOUT_RUNNER_BACKUP_DIR:-}
    BUILD_DIR=${ROLLOUT_RUNNER_BUILD_DIR:-}
    TARGET_DIR=${ROLLOUT_RUNNER_TARGET_DIR:-}
    ROLLBACK_DIR=${ROLLOUT_RUNNER_ROLLBACK_DIR:-}
    [ -n "$RUN_ID" ] && [ -n "$BASELINE_DIR" ] && [ -n "$BACKUP_DIR" ] && \
      [ -n "$BUILD_DIR" ] && [ -n "$TARGET_DIR" ] && [ -n "$ROLLBACK_DIR" ] || {
      fail 'pinned runtime is missing its phase evidence directories'
      return 1
    }
    for dir in "$BASELINE_DIR" "$BACKUP_DIR" "$BUILD_DIR" "$TARGET_DIR" "$ROLLBACK_DIR"; do
      check_private_dir "$dir" || return 1
    done
    verify_approved_gates || return 1
  else
    create_phase_dirs || return 1
    pin_runtime || return 1
    exec env \
      ROLLOUT_PINNED_EXECUTION=1 \
      ROLLOUT_RUNNER_RUN_ID="$RUN_ID" \
      ROLLOUT_RUNNER_BASELINE_DIR="$BASELINE_DIR" \
      ROLLOUT_RUNNER_BACKUP_DIR="$BACKUP_DIR" \
      ROLLOUT_RUNNER_BUILD_DIR="$BUILD_DIR" \
      ROLLOUT_RUNNER_TARGET_DIR="$TARGET_DIR" \
      ROLLOUT_RUNNER_ROLLBACK_DIR="$ROLLBACK_DIR" \
      bash "$BASELINE_DIR/rollout-runner.pinned" "$RUNNER_ACTION" "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
  fi

  require_clean_build_checkout || return 1
  select_rollout_compose_service || return 1
  baseline_id=$(current_service_id "$(rollout_compose_service)") || { fail "baseline $(rollout_compose_service) container ID is unavailable"; return 1; }
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    capture_snapshot "$baseline_id" "$BASELINE_DIR" baseline live-container-inspection || return 1
  else
    capture_snapshot "$baseline_id" "$BASELINE_DIR" baseline || return 1
  fi
  validate_baseline "$BASELINE_DIR/baseline.snapshot.json" || return 1
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    validate_env_baseline || return 1
    capture_snapshot "$baseline_id" "$BASELINE_DIR" initial-local-target-render || return 1
  elif [ ! -f "$CHECKOUT/.state/blob-mode/mode.json" ] || [ -L "$CHECKOUT/.state/blob-mode/mode.json" ]; then
    # Reject absent or unsafe authority before consulting prior-run evidence.
    preflight_target_local_blob_authority || return 1
    authority_preflight_done=1
  else
    validate_env_baseline || return 1
  fi
  if [ "$authority_preflight_done" = 0 ]; then
    preflight_target_local_blob_authority || return 1
  fi
  capture_backup_and_restore || return 1

  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot recheck origin/main before fast-forward'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot recheck origin/main SHA'; return 1; }
  git -C "$CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" "$origin_sha" || {
    fail 'authorized application target left origin/main after the verified backup'
    return 1
  }
  git -C "$CHECKOUT" merge-base --is-ancestor "$runtime_source_sha" "$origin_sha" || {
    fail 'reviewed rollout runtime source left origin/main after the verified backup'
    return 1
  }
  # The caller keeps deploy evidence private with umask 077. Preserve Git's
  # tracked 0644/0755 modes so source checkout changes do not invalidate build layers.
  if git -C "$CHECKOUT" merge-base --is-ancestor "$PREVIOUS_SHA" "$TARGET_SHA"; then
    (umask 022; git -C "$CHECKOUT" merge --ff-only -q "$TARGET_SHA") || { fail 'fast-forward to authorized application target failed'; return 1; }
  elif [ "$INITIALIZE_LOCAL" = 1 ] && [ "$PREVIOUS_SHA" = "$INITIAL_LOCAL_LEGACY_BASELINE_SHA" ] && \
      [ "$TARGET_SHA" = "$INITIAL_LOCAL_COMPOSE_TARGET_SHA" ]; then
    (umask 022; git -C "$CHECKOUT" reset --hard -q "$TARGET_SHA") || { fail 'fixed reviewed initial-local target checkout failed'; return 1; }
  else
    fail 'authorized application target is not a fast-forward from the live baseline'
    return 1
  fi
  [ "$(git -C "$CHECKOUT" rev-parse HEAD)" = "$TARGET_SHA" ] || { fail 'fast-forward did not reach the authorized target'; return 1; }
  if ! verify_approved_gates || ! verify_runtime_sources; then
    restore_checkout_and_tag || return 1
    fail 'pinned gate hashes changed after fast-forward; target was not started'
    return 1
  fi
  if ! capture_snapshot "$baseline_id" "$TARGET_DIR" preflight; then
    write_immutable "$TARGET_DIR/preflight-failure.txt" "result=rejected target_sha=$TARGET_SHA reason=resolved-config-capture-failed" || :
    restore_checkout_and_tag || return 1
    fail 'resolved Compose preflight capture failed; target was not started'
    return 1
  fi
  if compare_resolved_preflight; then :; else
    rc=$?
    if ! write_immutable "$TARGET_DIR/preflight-failure.txt" "result=rejected exit=$rc target_sha=$TARGET_SHA reason=unapproved-resolved-config"; then
      restore_checkout_and_tag || return 1
      fail 'resolved Compose preflight rejected and evidence could not be persisted; target was not started'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'resolved Compose preflight rejected unapproved drift; target was not started'
    return 1
  fi
  local target_blob_compose="$TARGET_DIR/preflight-blob-compose.json"
  local target_blob_containers="$TARGET_DIR/preflight-blob-containers.json"
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    target_blob_compose="$TARGET_DIR/initial-local-target-compose.json"
  fi
  capture_compose_blob_inventory "$target_blob_compose" || {
    restore_checkout_and_tag || return 1
    fail 'cannot capture durable blob authority preflight; target was not started'
    return 1
  }
  capture_running_blob_inventory "$target_blob_containers" || {
    restore_checkout_and_tag || return 1
    fail 'cannot inspect running blob authority before replacement; target was not started'
    return 1
  }
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    python3 "$MODE_HELPER" initialize-local \
      --state-dir "$CHECKOUT/.state/blob-mode" --report-root "$EVIDENCE_ROOT" \
      --baseline-containers "$BASELINE_DIR/initial-local-baseline-containers.json" \
      --current-containers "$target_blob_containers" \
      --preflight-target-compose "$BASELINE_DIR/initial-local-target-compose.json" \
      --target-compose "$target_blob_compose" \
      --target-artifact-sha256 "$INITIAL_LOCAL_ARTIFACT_SHA" \
      --override "$OVERRIDE_FILE" \
      --checkout "$CHECKOUT" --target-sha "$TARGET_SHA" --baseline-sha "$PREVIOUS_SHA" \
      --lock-path "$LOCK_PATH" || {
      local initial_journal="$CHECKOUT/.state/blob-mode/journal/0000000001.json"
      if [ -f "$initial_journal" ] && [ ! -L "$initial_journal" ]; then
        fail 'initial-local journal entry exists; reviewed target checkout retained for reconcile'
        return 1
      fi
      restore_checkout_and_tag || return 1
      fail 'inspected initial-local authority could not be published; baseline checkout and tag restored, target was not started'
      return 1
    }
  elif ! validate_blob_authority "$target_blob_containers" "$target_blob_compose" 0 1 0 1; then
    write_immutable "$TARGET_DIR/blob-authority-rejected.txt" \
      "result=rejected target_sha=$TARGET_SHA reason=durable-blob-authority-mismatch" || {
      restore_checkout_and_tag || return 1
      fail 'durable blob authority rejected and evidence could not be persisted; target was not started'
      return 1
    }
    restore_checkout_and_tag || return 1
    fail 'durable blob authority rejected; target was not started'
    return 1
  fi
  require_clean_build_checkout || return 1
  if bash "$SCHEMA_GATE" check pre "$TARGET_DIR" "$CHECKOUT" >/dev/null; then :; else
    rc=$?
    restore_checkout_and_tag || return 1
    fail "pre-deploy schema gate rejected exit=$rc; target was not started"
    return 1
  fi
  if verify_forwarded_build_config; then :; else
    rc=$?
    if ! write_immutable "$BUILD_DIR/build-result.txt" \
      "result=rejected exit=$rc source_sha=$TARGET_SHA reason=compose-build-provenance-rejected"; then
      restore_checkout_and_tag || return 1
      fail 'Compose build provenance rejected and evidence could not be persisted; baseline checkout and tag restored'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'Compose build provenance rejected; baseline checkout and tag restored'
    return 1
  fi
  TEAGRAMD_SOURCE_REVISION="$TARGET_SHA" docker compose build -q "$(rollout_compose_service)" </dev/null || {
    rc=$?
    if ! write_immutable "$BUILD_DIR/build-result.txt" "result=failed exit=$rc source_sha=$TARGET_SHA"; then
      restore_checkout_and_tag || return 1
      fail 'build failed and its evidence could not be persisted; baseline checkout and tag restored'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'teagramd build failed; baseline checkout and tag restored'
    return 1
  }
  if record_build_identity; then :; else
    rc=$?
    if ! write_immutable "$BUILD_DIR/build-result.txt" \
      "result=rejected exit=$rc source_sha=$TARGET_SHA reason=$BUILD_IDENTITY_REJECTION_REASON"; then
      restore_checkout_and_tag || return 1
      fail 'built image identity evidence could not be persisted; baseline checkout and tag restored'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'built image identity could not be verified; baseline checkout and tag restored'
    return 1
  fi

  if archive_pre_rollout_teagramd_logs; then :; else
    rc=$?
    restore_checkout_and_tag || return 1
    fail "pre-rollout teagramd logs could not be preserved (exit=$rc); baseline checkout and image restored"
    return 1
  fi
  if remove_orphaned_public_profile_services "$baseline_id"; then :; else
    rc=$?
    rollback_after_target_failure "public_profile_cleanup_exit_$rc" || return 2
    return 1
  fi
  if docker compose up -d </dev/null; then :; else
    rc=$?
    rollback_after_target_failure "compose_up_exit_$rc" || return 2
    return 1
  fi
  target_id=$(current_service_id "$(rollout_compose_service)") || target_id=''
  if [ -n "$target_id" ]; then
    if capture_snapshot "$target_id" "$TARGET_DIR" target; then :; else
      rollback_after_target_failure snapshot_capture_failed || return 2
      return 1
    fi
  else
    rollback_after_target_failure target_container_id_unavailable || return 2
    return 1
  fi

  if ! target_compare; then
    rollback_after_target_failure target_comparison_rejected || return 2
    return 1
  fi
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    target_container_action=replaced
  else
    target_container_action=$(awk -F '\t' '
      $2 == "container_reused" { action = "reused"; count++ }
      $2 == "container_replaced" { action = "replaced"; count++ }
      END { if (count != 1) exit 1; print action }
    ' "$TARGET_DIR/target-comparisons.tsv") || {
      rollback_after_target_failure target_container_comparison_evidence_missing || return 2
      return 1
    }
  fi
  local target_runtime_compose="$TARGET_DIR/target-blob-compose.json"
  local target_runtime_containers="$TARGET_DIR/target-blob-containers.json"
  if ! capture_compose_blob_inventory "$target_runtime_compose" || \
     ! capture_running_blob_inventory "$target_runtime_containers" || \
     ! validate_blob_authority "$target_runtime_containers" "$target_runtime_compose" 0 0 0 1; then
    rollback_after_target_failure target_blob_authority_rejected || return 2
    return 1
  fi
  if run_target_readiness "$target_id"; then :; else
    rc=$?
    rollback_after_target_failure "target_readiness_rejected_exit_$rc" || return 2
    return 1
  fi
  if bash "$SCHEMA_GATE" check post "$TARGET_DIR" "$CHECKOUT" >/dev/null; then :; else
    rc=$?
    rollback_after_target_failure "schema_gate_rejected_exit_$rc" || return 2
    return 1
  fi
  record_verified_env "$TARGET_DIR/target.snapshot.json" "$TARGET_SHA" || return 1
  write_immutable "$TARGET_DIR/target-result.txt" \
    "result=verified source_sha=$TARGET_SHA image_id=$BUILT_IMAGE_ID container_id=$target_id container_action=$target_container_action readiness=pass schema_gate=pass"
  printf 'rollout=verified sha=%s image_id=%s container_id=%s container_action=%s evidence=%s\n' \
    "$TARGET_SHA" "$BUILT_IMAGE_ID" "$target_id" "$target_container_action" "$TARGET_DIR"
}

copy_move_inputs() {
  local target_env="$TARGET_CHECKOUT/.env" target_override="$TARGET_CHECKOUT/docker-compose.override.yml"
  local path
  for path in "$ENV_FILE" "$OVERRIDE_FILE"; do
    [ -f "$path" ] && [ ! -L "$path" ] || { fail 'move input is missing or symlinked'; return 1; }
  done
  [ "$(stat -c %a -- "$ENV_FILE")" = 600 ] || { fail 'deployment environment file must be mode 0600'; return 1; }
  [ ! -e "$target_env" ] && [ ! -L "$target_env" ] || { fail 'target environment file already exists'; return 1; }
  [ ! -e "$target_override" ] && [ ! -L "$target_override" ] || { fail 'target Compose override already exists'; return 1; }
  cp -- "$ENV_FILE" "$target_env" || { fail 'cannot stage target environment file'; return 1; }
  chmod 600 -- "$target_env" || { fail 'cannot protect target environment file'; return 1; }

  for path in .secrets .state; do
    if [ -e "$CHECKOUT/$path" ] || [ -L "$CHECKOUT/$path" ]; then
      [ -d "$CHECKOUT/$path" ] && [ ! -L "$CHECKOUT/$path" ] || {
        fail "source $path is missing or symlinked"
        return 1
      }
      [ -z "$(find "$CHECKOUT/$path" -type l -print -quit)" ] || {
        fail "source $path contains a symlink"
        return 1
      }
      [ ! -e "$TARGET_CHECKOUT/$path" ] && [ ! -L "$TARGET_CHECKOUT/$path" ] || {
        fail "target $path already exists"
        return 1
      }
      cp -a -- "$CHECKOUT/$path" "$TARGET_CHECKOUT/$path" || {
        fail "cannot copy source $path into target checkout"
        return 1
      }
    fi
  done
}

run_stack_move() {
  local branch origin_sha previous_sha runtime_source_sha source_blob_compose source_blob_containers blob_validation
  local target_compose_selection=docker-compose.yml:docker-compose.local-blobs.yml
  local source_lock=$SOURCE_LOCK_PATH target_lock=$TARGET_LOCK_PATH
  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot fetch origin/main for stack move'; return 1; }
  branch=$(git -C "$CHECKOUT" branch --show-current) || { fail 'cannot read checkout branch'; return 1; }
  previous_sha=$(git -C "$CHECKOUT" rev-parse HEAD) || { fail 'cannot read baseline checkout SHA'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot read origin/main SHA'; return 1; }
  runtime_source_sha=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  [ "$branch" = main ] || { fail 'stack move checkout is not on main'; return 1; }
  [ "$previous_sha" = "$EXPECTED_BASELINE_SHA" ] || { fail 'live checkout differs from the expected baseline SHA'; return 1; }
  git -C "$CHECKOUT" cat-file -e "$TARGET_SHA^{commit}" 2>/dev/null || { fail 'authorized stack-move target is unavailable'; return 1; }
  git -C "$CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" "$origin_sha" || {
    fail 'stack-move target is not a reviewed origin/main commit'
    return 1
  }
  git -C "$CHECKOUT" merge-base --is-ancestor "$previous_sha" "$TARGET_SHA" || {
    fail 'stack-move target is not a fast-forward from the live baseline'
    return 1
  }
  git -C "$CHECKOUT" merge-base --is-ancestor "$runtime_source_sha" "$origin_sha" || {
    fail 'stack-move runtime source is not a reviewed origin/main commit'
    return 1
  }
  git -C "$CHECKOUT" merge-base --is-ancestor "$TARGET_SHA" "$runtime_source_sha" || {
    fail 'stack-move runtime source does not descend from the target commit'
    return 1
  }
  verify_runtime_sources || return 1
  require_clean_build_checkout || return 1
  [ "$TARGET_CHECKOUT" != "$CHECKOUT" ] || {
    fail 'target checkout path must differ from the source checkout'
    return 1
  }
  [ ! -e "$TARGET_CHECKOUT" ] && [ ! -L "$TARGET_CHECKOUT" ] || {
    fail 'target checkout path already exists'
    return 1
  }
  [ "$source_lock" != "$target_lock" ] || { fail 'source and target deploy locks must differ'; return 1; }

  if [ "$ROLLOUT_PINNED_EXECUTION" != 1 ]; then
    create_phase_dirs || return 1
    pin_runtime || return 1
    exec env \
      ROLLOUT_PINNED_EXECUTION=1 \
      ROLLOUT_RUNNER_RUN_ID="$RUN_ID" \
      ROLLOUT_RUNNER_BASELINE_DIR="$BASELINE_DIR" \
      ROLLOUT_RUNNER_BACKUP_DIR="$BACKUP_DIR" \
      ROLLOUT_RUNNER_BUILD_DIR="$BUILD_DIR" \
      ROLLOUT_RUNNER_TARGET_DIR="$TARGET_DIR" \
      ROLLOUT_RUNNER_ROLLBACK_DIR="$ROLLBACK_DIR" \
      ROLLOUT_RUNNER_LOCK_PATH="$LOCK_PATH" \
      ROLLOUT_RUNNER_SOURCE_LOCK_PATH="$source_lock" \
      ROLLOUT_RUNNER_TARGET_LOCK_PATH="$target_lock" \
      bash "$BASELINE_DIR/rollout-runner.pinned" move-stack "$TARGET_SHA" "$EXPECTED_BASELINE_SHA"
  fi

  RUN_ID=${ROLLOUT_RUNNER_RUN_ID:-}
  TARGET_DIR=${ROLLOUT_RUNNER_TARGET_DIR:-}
  [ -n "$RUN_ID" ] && [ -n "$TARGET_DIR" ] || { fail 'pinned stack move is missing evidence paths'; return 1; }
  check_private_dir "$TARGET_DIR" || return 1
  [ -f "$STACK_MOVE_HELPER" ] && [ ! -L "$STACK_MOVE_HELPER" ] || { fail 'pinned stack move helper is missing'; return 1; }
  source_blob_compose="$TARGET_DIR/stack-move-preflight-blob-compose.json"
  source_blob_containers="$TARGET_DIR/stack-move-preflight-blob-containers.json"
  capture_compose_blob_inventory "$source_blob_compose" || {
    fail 'cannot capture source blob authority before stack move'
    return 1
  }
  capture_running_blob_inventory "$source_blob_containers" || {
    fail 'cannot capture running source blob containers before stack move'
    return 1
  }
  blob_validation=$(validate_blob_authority "$source_blob_containers" "$source_blob_compose") || {
    fail 'source blob authority failed preflight before stack move'
    return 1
  }
  case "$blob_validation" in
    *'outcome=s3-accepted '*|*'outcome=encrypted '*)
      fail 'S3-authorized blob source requires MAIN-1977 bucket-move work; the source stack was not stopped'
      return 1
      ;;
    *'outcome=initial-local '*|*'outcome=recovered-local '*) ;;
    *)
      fail 'stack move requires validated local blob authority before stopping the source project'
      return 1
      ;;
  esac
  (umask 022; git -C "$CHECKOUT" worktree add --detach "$TARGET_CHECKOUT" "$TARGET_SHA" >/dev/null) || {
    fail 'cannot create target checkout worktree'
    return 1
  }
  copy_move_inputs || return 1
  TEAGRAMD_SOURCE_REVISION="$TARGET_SHA" python3 "$STACK_MOVE_HELPER" move \
    --source-dir "$CHECKOUT" --target-dir "$TARGET_CHECKOUT" \
    --source-compose "${COMPOSE_FILE:-}" --target-compose "$target_compose_selection" \
    --target-sha "$TARGET_SHA" \
    --override "$OVERRIDE_FILE" \
    --env-file "$ENV_FILE" --target-env-file "$TARGET_CHECKOUT/.env" \
    --evidence-dir "$TARGET_DIR" \
    --state-dir "$TARGET_CHECKOUT/.state/blob-mode" \
    --report-root "$EVIDENCE_ROOT" --lock-path "$LOCK_PATH" \
    --blob-mode-helper "$MODE_HELPER"
}

main() {
  if [ "$#" -ge 2 ] && [ "$#" -le 3 ] && [ "$1" = compare-resolved-preflight ]; then
    compare_resolved_preflight_command "${@:2}"
    return $?
  fi
  if [ "$#" -eq 2 ] && [ "$1" = reconcile ]; then
    configure_action_paths apply
    TARGET_SHA=$2
    [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]] || { fail 'reconcile target SHA must be a full commit ID'; return 64; }
    require_runtime || return 1
    local reconcile_branch reconcile_head
    reconcile_branch=$(git -C "$CHECKOUT" branch --show-current) || { fail 'cannot read checkout branch'; return 1; }
    reconcile_head=$(git -C "$CHECKOUT" rev-parse HEAD) || { fail 'cannot read current checkout SHA'; return 1; }
    [ "$reconcile_branch" = main ] && [ "$reconcile_head" = "$TARGET_SHA" ] || {
      fail 'reconcile requires the exact reviewed target on the main checkout'
      return 1
    }
    acquire_shared_lock || { fail 'cannot acquire shared deployment lock'; return 1; }
    verify_approved_gates || return 1
    verify_runtime_sources || return 1
    cd "$CHECKOUT"
    python3 "$MODE_HELPER" reconcile --state-dir "$CHECKOUT/.state/blob-mode" \
      --report-root "$EVIDENCE_ROOT" --lock-path "$LOCK_PATH"
    return $?
  fi
  [ "$#" -eq 3 ] && { [ "$1" = apply ] || [ "$1" = initialize-local ] || [ "$1" = move-stack ]; } || { usage; return 64; }
  configure_action_paths "$1"
  [ "$RUNNER_ACTION" = initialize-local ] && INITIALIZE_LOCAL=1
  TARGET_SHA=$2
  EXPECTED_BASELINE_SHA=$3
  [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]] || { fail 'target SHA must be a full commit ID'; return 64; }
  [[ "$EXPECTED_BASELINE_SHA" =~ ^[0-9a-f]{40}$ ]] || { fail 'expected baseline SHA must be a full commit ID'; return 64; }
  [[ "$READY_SECONDS" =~ ^[0-9]+$ ]] && [ "$READY_SECONDS" -ge 1 ] && [ "$READY_SECONDS" -le 120 ] || {
    fail 'readiness bound must be 1 through 120 seconds'
    return 64
  }
  require_runtime || return 1
  if [ "$RUNNER_ACTION" = move-stack ]; then
    LOCK_PATH=$TARGET_LOCK_PATH
    if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
      local source_lock_target target_lock_target
      source_lock_target=$(readlink "/proc/$$/fd/8") || { fail 'inherited source deploy lock descriptor is missing'; return 1; }
      target_lock_target=$(readlink "/proc/$$/fd/9") || { fail 'inherited target deploy lock descriptor is missing'; return 1; }
      [ "$source_lock_target" = "$SOURCE_LOCK_PATH" ] && [ "$target_lock_target" = "$TARGET_LOCK_PATH" ] || {
        fail 'inherited stack-move lock descriptors point to another path'
        return 1
      }
    else
      exec 8>"$SOURCE_LOCK_PATH"
      flock -x 8
      acquire_shared_lock || { fail 'cannot acquire target deploy lock'; return 1; }
    fi
  elif [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    local lock_target
    lock_target=$(readlink "/proc/$$/fd/9") || { fail 'inherited deployment lock descriptor is missing'; return 1; }
    [ "$lock_target" = "$LOCK_PATH" ] || { fail 'inherited deployment lock descriptor points to another path'; return 1; }
  else
    acquire_shared_lock || { fail 'cannot acquire shared deployment lock'; return 1; }
  fi
  verify_approved_gates || return 1
  cd "$CHECKOUT"
  if [ "$RUNNER_ACTION" = move-stack ]; then
    run_stack_move
  else
    run_apply
  fi
}

if [ "${ROLLOUT_RUNNER_SOURCE_ONLY:-0}" != 1 ]; then
  main "$@"
fi
