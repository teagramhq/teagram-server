#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT_SOURCE="$SCRIPT_DIR/$(basename "${BASH_SOURCE[0]}")"
readonly APPROVED_VERIFIER_SHA=05db10baa9494ffefd39bd61402ac387f390b39dfdacfe49bcc6ccb4e1c71967
readonly APPROVED_SCHEMA_GATE_SHA=ac54d3cf0480383a52414a8bc8b856b5e1d5f6f8d19034c9c576c64627e097c8
readonly APPROVED_SCHEMA_GATE_HELPER_SHA=adc879b1ad2d44c6485242301dd684b4060c17835d64de02ffc90df4b7488523
readonly APPROVED_INITIAL_LOCAL_COMPOSE_SHA=3a4f158c6e1f2ead6676fba85d8d95cfb15557a0fbd8e82230361e0af988e0f7
readonly TARGET_LOCAL_COMPOSE_TARGET_SHA=e58ba203505a37bf5ad3181f22e59a934d4a7442
readonly TARGET_LOCAL_COMPOSE_SHA=d3426792677d0510a75dd254fbbd5fd9d9b19a01d710ec5b9b59f3e8a31447c7
readonly TARGET_LOCAL_COMPOSE_SOURCE_FILE=deploy/telegramd/rollout-runner/local-compose-e58ba20.yml
readonly TARGET_LOCAL_COMPOSE_RUNTIME_FILE=.rollout-compose.local-e58ba20.yml
readonly INITIAL_LOCAL_COMPOSE_TARGET_SHA=777742cc4b3ab0fda6b504a82b314a90aa60918b
readonly INITIAL_LOCAL_COMPOSE_FILE=.rollout-compose.initial-local.yml
readonly INITIAL_LOCAL_LEGACY_BASELINE_SHA=932994e26a86eb1c9ad60f81b3d222b19d3f40b7

ROLLOUT_RUNNER_TEST_MODE=${ROLLOUT_RUNNER_TEST_MODE:-0}
ROLLOUT_PINNED_EXECUTION=${ROLLOUT_PINNED_EXECUTION:-0}
INITIALIZE_LOCAL=0
INITIAL_LOCAL_ARTIFACT_SHA=
RUNNER_ACTION=apply
CHECKOUT=${ROLLOUT_RUNNER_CHECKOUT:-/opt/telegram-server}
EVIDENCE_ROOT=${ROLLOUT_RUNNER_EVIDENCE_ROOT:-/root}
LOCK_PATH=${ROLLOUT_RUNNER_LOCK_PATH:-/tmp/telegram-server-deploy.lock}
ENV_FILE=${ROLLOUT_RUNNER_ENV_FILE:-/opt/telegram-server/.env}
OVERRIDE_FILE=${ROLLOUT_RUNNER_OVERRIDE_FILE:-/opt/telegram-server/docker-compose.override.yml}
READY_SECONDS=${ROLLOUT_RUNNER_READY_SECONDS:-120}
if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
  VERIFIER="$SCRIPT_DIR/rollout-verifier.pinned"
  SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.pinned"
  SCHEMA_GATE_HELPER="$SCRIPT_DIR/schema-result-gate.py"
  MODE_HELPER="$SCRIPT_DIR/blob-mode-state.pinned"
else
  VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
  SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
  SCHEMA_GATE_HELPER="$SCRIPT_DIR/schema-result-gate.py"
  MODE_HELPER="$SCRIPT_DIR/blob-mode-state.py"
fi

fail() {
  printf 'rollout runner rejected: %s\n' "$1" >&2
  return 1
}

usage() {
  printf '%s\n' 'usage: rollout-runner.sh apply|initialize-local TARGET_SHA EXPECTED_BASELINE_SHA' >&2
  printf '%s\n' '       rollout-runner.sh reconcile TARGET_SHA' >&2
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

verify_runtime_sources() {
  local -a tracked_paths source_paths
  local index source_sha target_sha runtime_source_sha
  runtime_source_sha=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  [[ "$runtime_source_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'rollout runtime source SHA must be a full commit ID'; return 1; }
  tracked_paths=(
    deploy/telegramd/rollout-runner/rollout-runner.sh
    deploy/telegramd/rollout-runner/rollout-verifier.sh
    deploy/telegramd/rollout-runner/schema-result-gate.sh
    deploy/telegramd/rollout-runner/schema-result-gate.py
    deploy/telegramd/rollout-runner/blob-mode-state.py
  )
  if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    source_paths=("$SCRIPT_SOURCE" "$VERIFIER" "$SCHEMA_GATE" "$SCHEMA_GATE_HELPER" "$MODE_HELPER")
  else
    source_paths=("$SCRIPT_SOURCE" "$SCRIPT_DIR/rollout-verifier.sh" "$SCRIPT_DIR/schema-result-gate.sh" "$SCHEMA_GATE_HELPER" "$MODE_HELPER")
  fi
  if [ "$INITIALIZE_LOCAL" != 1 ] && [ "$TARGET_SHA" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ]; then
    tracked_paths+=("$TARGET_LOCAL_COMPOSE_SOURCE_FILE")
    source_paths+=("$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE")
  fi
  for index in "${!tracked_paths[@]}"; do
    source_sha=$(sha256_file "${source_paths[$index]}") || { fail 'cannot hash a rollout runtime source'; return 1; }
    target_sha=$(sha256_target_file "$runtime_source_sha" "${tracked_paths[$index]}") || {
      fail "cannot read rollout runtime source from reviewed source revision: ${tracked_paths[$index]}"
      return 1
    }
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

verify_target_local_compose() {
  local artifact_path artifact_sha override_path entry entry_path artifact_count=0 override_count=0
  local -a compose_files
  if [ "$INITIALIZE_LOCAL" = 1 ] || [ "$TARGET_SHA" != "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ]; then
    if [ "${COMPOSE_FILE+x}" = x ] && [ -e "$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE" ]; then
      artifact_path=$(canonical_compose_file_path "$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE") || {
        fail 'cannot resolve the target-local Compose artifact path'
        return 1
      }
      IFS=: read -r -a compose_files <<< "$COMPOSE_FILE"
      for entry in "${compose_files[@]}"; do
        [ -n "$entry" ] || continue
        entry_path=$(canonical_compose_file_path "$entry") || continue
        [ "$entry_path" = "$artifact_path" ] || continue
        fail 'target-local Compose artifact is pinned only to its exact application target'
        return 1
      done
    fi
    return 0
  fi
  artifact_path="$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE"
  [ -f "$artifact_path" ] && [ ! -L "$artifact_path" ] && \
    [ "$(stat -c %u -- "$artifact_path")" = 0 ] && [ "$(stat -c %a -- "$artifact_path")" = 600 ] || {
    fail 'target-local Compose artifact must be a root-owned mode-0600 regular file'
    return 1
  }
  artifact_path=$(canonical_compose_file_path "$artifact_path") || { fail 'cannot resolve the target-local Compose artifact path'; return 1; }
  artifact_sha=$(sha256_file "$artifact_path") || { fail 'cannot hash target-local Compose artifact'; return 1; }
  [ "$artifact_sha" = "$TARGET_LOCAL_COMPOSE_SHA" ] || {
    fail 'target-local Compose artifact differs from the reviewed pin'
    return 1
  }
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
    expected_checkout=/opt/telegram-server
    [ "$EVIDENCE_ROOT" = /root ] || { fail 'production evidence root must be /root'; return 1; }
    [ "$LOCK_PATH" = /tmp/telegram-server-deploy.lock ] || { fail 'production deploy lock path is fixed'; return 1; }
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
    for path in "$SCRIPT_SOURCE" "$VERIFIER" "$SCHEMA_GATE" "$SCHEMA_GATE_HELPER" "$MODE_HELPER"; do
      [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u -- "$path")" = 0 ] && \
        [ "$(stat -c %a -- "$path")" = 600 ] || { fail 'pinned runtime file is not root-only'; return 1; }
    done
  else
    if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
      expected_source_dir=${ROLLOUT_RUNNER_TEST_SOURCE_DIR:-"$CHECKOUT/deploy/telegramd/rollout-runner"}
    else
      expected_source_dir=/root/telegramd-rollout-runner
    fi
    [ "$SCRIPT_DIR" = "$expected_source_dir" ] || {
      fail 'initial runner must come from its root-only source directory'
      return 1
    }
    check_private_dir "$SCRIPT_DIR" || return 1
    for path in "$SCRIPT_SOURCE" "$SCRIPT_DIR/rollout-verifier.sh" "$SCRIPT_DIR/schema-result-gate.sh" "$SCHEMA_GATE_HELPER" "$MODE_HELPER"; do
      [ -f "$path" ] && [ ! -L "$path" ] && [ "$(stat -c %u -- "$path")" = 0 ] && \
        [ "$(stat -c %a -- "$path")" = 600 ] || { fail 'initial runtime file is not root-only'; return 1; }
    done
  fi
}

acquire_shared_lock() {
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    exec 9>"$LOCK_PATH"
    flock -x 9
  else
    exec 9>/tmp/telegram-server-deploy.lock
    flock -x 9
  fi
}

check_private_dir() {
  local dir=$1 mode owner
  [ -d "$dir" ] && [ ! -L "$dir" ] || { fail 'evidence phase directory missing or symlinked'; return 1; }
  mode=$(stat -c %a -- "$dir") || { fail 'cannot inspect evidence directory mode'; return 1; }
  owner=$(stat -c %u -- "$dir") || { fail 'cannot inspect evidence directory owner'; return 1; }
  [ "$mode" = 700 ] && [ "$owner" = 0 ] || { fail 'evidence phase directory must be root-owned mode 0700'; return 1; }
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
  [ "$mode" = 600 ] && [ "$owner" = 0 ] || { fail 'evidence file is not root-owned mode 0600'; return 1; }
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
  local runner_sha verifier_sha schema_sha schema_helper_sha source_revision compose_sha
  local target_local_compose_sha=not-selected target_local_compose_target=not-selected
  pin_runtime_file "$SCRIPT_SOURCE" "$BASELINE_DIR/rollout-runner.pinned" || return 1
  pin_runtime_file "$SCRIPT_DIR/rollout-verifier.sh" "$BASELINE_DIR/rollout-verifier.pinned" || return 1
  pin_runtime_file "$SCRIPT_DIR/schema-result-gate.sh" "$BASELINE_DIR/schema-result-gate.pinned" || return 1
  pin_runtime_file "$SCHEMA_GATE_HELPER" "$BASELINE_DIR/schema-result-gate.py" || return 1
  pin_runtime_file "$SCRIPT_DIR/blob-mode-state.py" "$BASELINE_DIR/blob-mode-state.pinned" || return 1
  VERIFIER="$BASELINE_DIR/rollout-verifier.pinned"
  SCHEMA_GATE="$BASELINE_DIR/schema-result-gate.pinned"
  SCHEMA_GATE_HELPER="$BASELINE_DIR/schema-result-gate.py"
  MODE_HELPER="$BASELINE_DIR/blob-mode-state.pinned"
  verify_approved_gates || return 1
  runner_sha=$(sha256_file "$BASELINE_DIR/rollout-runner.pinned") || { fail 'cannot hash pinned runner'; return 1; }
  verifier_sha=$(sha256_file "$VERIFIER") || { fail 'cannot hash pinned verifier'; return 1; }
  schema_sha=$(sha256_file "$SCHEMA_GATE") || { fail 'cannot hash pinned schema gate'; return 1; }
  schema_helper_sha=$(sha256_file "$SCHEMA_GATE_HELPER") || { fail 'cannot hash pinned schema gate helper'; return 1; }
  local mode_helper_sha
  mode_helper_sha=$(sha256_file "$MODE_HELPER") || { fail 'cannot hash pinned blob-mode helper'; return 1; }
  source_revision=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  compose_sha=not-selected
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    compose_sha=$(sha256_file "$CHECKOUT/$INITIAL_LOCAL_COMPOSE_FILE") || { fail 'cannot hash initial-local Compose artifact'; return 1; }
  fi
  if [ "$INITIALIZE_LOCAL" != 1 ] && [ "$TARGET_SHA" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ]; then
    target_local_compose_sha=$(sha256_file "$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE") || {
      fail 'cannot hash target-local Compose artifact'
      return 1
    }
    target_local_compose_target=$TARGET_SHA
  fi
  write_immutable "$BASELINE_DIR/runtime-pins.txt" \
    "source_revision=$source_revision target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA runner_sha256=$runner_sha verifier_sha256=$verifier_sha schema_gate_sha256=$schema_sha schema_gate_helper_sha256=$schema_helper_sha blob_mode_helper_sha256=$mode_helper_sha initial_local_compose_sha256=$compose_sha target_local_compose_target_sha=$target_local_compose_target target_local_compose_sha256=$target_local_compose_sha" || return 1
}

current_service_id() {
  local service=$1 id
  id=$(docker compose ps -q "$service" </dev/null 2>/dev/null) || return 1
  id=$(printf '%s' "$id" | head -n 1)
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$id"
}

capture_snapshot() {
  local id=$1 dir=$2 name=$3 compose_source=${4:-selected-compose-render} output
  output="$dir/$name.snapshot.json"
  [ ! -e "$output" ] && [ ! -L "$output" ] || { fail 'snapshot evidence already exists'; return 1; }
  ROLLOUT_CHECKOUT_PATH="$CHECKOUT" \
    bash "$VERIFIER" snapshot "$id" "$dir" "$name" "$ENV_FILE" "$OVERRIDE_FILE" "$compose_source" >/dev/null 2>&1 || {
    fail "cannot capture $name snapshot"
    return 1
  }
  [ -f "$output" ] && [ ! -L "$output" ] && [ "$(stat -c %a -- "$output")" = 600 ] || {
    fail "$name snapshot is not private"
    return 1
  }
}

capture_compose_blob_inventory() {
  local path=$1 inventory
  [ ! -e "$path" ] && [ ! -L "$path" ] || { fail 'blob Compose inventory evidence already exists'; return 1; }
  inventory=$(docker compose config --format json </dev/null 2>/dev/null | \
    python3 "$MODE_HELPER" compose --checkout "$CHECKOUT") || {
    fail 'cannot capture the allowlisted blob Compose inventory'
    return 1
  }
  write_immutable "$path" "$inventory"
}

capture_running_blob_inventory() {
  local path=$1 allow_empty=${2:-0} reference_id reference_inspect project ids id inventory
  local -a container_ids=()
  [ ! -e "$path" ] && [ ! -L "$path" ] || { fail 'running blob inventory evidence already exists'; return 1; }
  if [ "$allow_empty" = 1 ]; then
    reference_id=$(docker compose ps -aq telegramd </dev/null 2>/dev/null | head -n 1) || reference_id=''
    [[ "$reference_id" =~ ^[0-9a-f]{64}$ ]] || reference_id=''
  else
    reference_id=$(current_service_id telegramd) || { fail 'running telegramd container ID is unavailable'; return 1; }
  fi
  if [ -n "$reference_id" ]; then
    reference_inspect=$(docker inspect "$reference_id" 2>/dev/null) || { fail 'cannot inspect the running telegramd project'; return 1; }
    project=$(printf '%s' "$reference_inspect" | jq -er '
      if type == "array" then .[0].Config.Labels["com.docker.compose.project"]
      else .Config.Labels["com.docker.compose.project"] end
      | select(type == "string" and test("^[a-z0-9][a-z0-9_-]*$"))
    ') || { fail 'running telegramd project label is missing or invalid'; return 1; }
  else
    [ "$allow_empty" = 1 ] || { fail 'running telegramd container ID is unavailable'; return 1; }
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
    fail 'cannot enumerate containers in the telegramd project'
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
  [ -n "$ids" ] || { fail 'no running containers were found in the telegramd project'; return 1; }
  mapfile -t container_ids <<< "$ids"
  local found_reference=0
  for id in "${container_ids[@]}"; do
    [[ "$id" =~ ^[0-9a-f]{64}$ ]] || { fail 'project container enumeration returned an invalid container ID'; return 1; }
    [ "$id" = "$reference_id" ] && found_reference=1
  done
  [ "$found_reference" -eq 1 ] || { fail 'running telegramd container is absent from project enumeration'; return 1; }
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
  local containers_file=$1 compose_file=$2 allow_empty=${3:-0} allow_unguarded_containers=${4:-0} allow_unguarded_compose=${5:-0}
  local -a mode_args=()
  [ "$allow_empty" = 1 ] && mode_args+=(--allow-empty-containers)
  [ "$allow_unguarded_containers" = 1 ] && mode_args+=(--allow-unguarded-initial-local-containers)
  [ "$allow_unguarded_compose" = 1 ] && mode_args+=(--allow-unguarded-initial-local)
  python3 "$MODE_HELPER" validate \
    --state-dir "$CHECKOUT/.state/blob-mode" --report-root "$EVIDENCE_ROOT" \
    --containers "$containers_file" --compose "$compose_file" \
    --override "$OVERRIDE_FILE" --checkout "$CHECKOUT" "${mode_args[@]}"
}

preflight_target_local_blob_authority() {
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local containers_file="$BASELINE_DIR/initial-local-baseline-containers.json"
    local compose_file="$BASELINE_DIR/initial-local-target-compose.json"
    local preflight_file="$BASELINE_DIR/initial-local-preflight.txt"
    local provenance_file="$BASELINE_DIR/initial-local-provenance.txt"
    local containers_sha compose_sha
    [ "$INITIAL_LOCAL_ARTIFACT_SHA" = "$APPROVED_INITIAL_LOCAL_COMPOSE_SHA" ] || {
      fail 'verified initial-local artifact digest is unavailable'
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
  [ "$TARGET_SHA" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ] || return 0
  local compose_file="$TARGET_DIR/pre-backup-blob-compose.json"
  local containers_file="$TARGET_DIR/pre-backup-blob-containers.json"
  local artifact_sha
  artifact_sha=$(sha256_file "$CHECKOUT/$TARGET_LOCAL_COMPOSE_RUNTIME_FILE") || {
    fail 'cannot hash target-local Compose artifact before backup'
    return 1
  }
  capture_compose_blob_inventory "$compose_file" || {
    fail 'target-local Compose authority preflight failed before backup'
    return 1
  }
  capture_running_blob_inventory "$containers_file" || {
    fail 'running blob authority preflight failed before backup'
    return 1
  }
  if ! validate_blob_authority "$containers_file" "$compose_file"; then
    write_immutable "$TARGET_DIR/pre-backup-blob-authority-rejected.txt" \
      "result=rejected target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA compose_sha256=$artifact_sha" || return 1
    fail 'durable blob authority rejected before backup; target was not started'
    return 1
  fi
  write_immutable "$TARGET_DIR/pre-backup-blob-authority.txt" \
    "result=pass target_sha=$TARGET_SHA expected_baseline_sha=$EXPECTED_BASELINE_SHA compose_sha256=$artifact_sha"
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

capture_backup_and_restore() {
  local postgres_id postgres_inspect postgres_image dump_tmp dump_path dump_stderr
  local dump_rc dump_sha restore_name restore_start restore_stderr postgres_image_ref ready=0 restore_rc=1 cleanup_rc=1 i
  postgres_id=$(current_service_id postgres) || { fail 'Postgres container ID is unavailable'; return 1; }
  postgres_inspect=$(docker inspect "$postgres_id" 2>/dev/null) || { fail 'cannot inspect Postgres image identity'; return 1; }
  postgres_image=$(printf '%s' "$postgres_inspect" | jq -er 'if type == "array" then .[0].Image else .Image end | select(test("^sha256:[0-9a-f]{64}$"))') || {
    fail 'Postgres image identity is invalid'
    return 1
  }
  postgres_image_ref=${postgres_image#sha256:}
  dump_path="$BACKUP_DIR/pgdump-pre-$TARGET_SHA.sql"
  dump_stderr="$BACKUP_DIR/pgdump.stderr"
  [ ! -e "$dump_path" ] && [ ! -L "$dump_path" ] || { fail 'database dump evidence already exists'; return 1; }
  write_immutable "$dump_stderr" '' || return 1
  dump_tmp=$(mktemp "$BACKUP_DIR/.pgdump.XXXXXXXX") || { fail 'cannot allocate database dump'; return 1; }
  if docker compose exec -T postgres pg_dump -U postgres telegram </dev/null >"$dump_tmp" 2>"$dump_stderr"; then
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
      --tmpfs /var/lib/postgresql/data:rw,nosuid,nodev \
      --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=telegram \
      "$postgres_image_ref" </dev/null >/dev/null 2>"$restore_start"; then
    restore_rc=0
  else
    restore_rc=$?
  fi
  if [ "$restore_rc" -eq 0 ]; then
    for ((i=0; i<60; i++)); do
      if docker exec "$restore_name" pg_isready -h 127.0.0.1 -U postgres -d telegram >/dev/null 2>>"$restore_stderr"; then
        ready=1
        break
      fi
      sleep 1
    done
    if [ "$ready" -eq 1 ] && docker exec -i "$restore_name" psql -X -v ON_ERROR_STOP=1 -U postgres -d telegram \
        <"$dump_path" >/dev/null 2>>"$restore_stderr"; then
      restore_rc=0
    else
      restore_rc=1
    fi
    if docker rm -f "$restore_name" >/dev/null 2>>"$restore_stderr"; then cleanup_rc=0; else cleanup_rc=$?; fi
  else
    if docker inspect --type container "$restore_name" >/dev/null 2>&1; then
      if docker rm -f "$restore_name" >/dev/null 2>>"$restore_stderr"; then cleanup_rc=0; else cleanup_rc=$?; fi
    else
      cleanup_rc=0
    fi
  fi
  chmod 600 -- "$restore_start" "$restore_stderr" || { fail 'cannot protect isolated restore evidence'; return 1; }
  if [ "$ready" -eq 1 ] && [ "$restore_rc" -eq 0 ] && [ "$cleanup_rc" -eq 0 ]; then
    write_immutable "$BACKUP_DIR/backup-manifest.txt" \
      "target_sha=$TARGET_SHA dump_path=$dump_path dump_sha256=$dump_sha dump_complete=yes postgres_image_id=$postgres_image isolated_restore=pass restore_network=none restore_data=tmpfs restore_cleanup=pass" || return 1
    printf 'backup_restore=pass dump_sha256=%s\n' "$dump_sha"
    return 0
  fi
  write_immutable "$BACKUP_DIR/backup-result.txt" \
    "target_sha=$TARGET_SHA dump_sha256=$dump_sha dump_complete=yes isolated_restore_ready=$ready restore_exit=$restore_rc restore_cleanup_exit=$cleanup_rc" || return 1
  fail 'isolated restore or cleanup did not pass'
}

record_build_identity() {
  local built_image checked_out_sha image_file
  checked_out_sha=$(git rev-parse HEAD) || { fail 'cannot read checked-out SHA after fast-forward'; return 1; }
  [ "$checked_out_sha" = "$TARGET_SHA" ] || { fail 'checked-out SHA changed before image capture'; return 1; }
  built_image=$(docker image inspect telegramd:local --format '{{.Id}}' 2>/dev/null) || { fail 'cannot inspect built telegramd tag'; return 1; }
  [[ "$built_image" =~ ^sha256:[0-9a-f]{64}$ ]] || { fail 'built telegramd image ID is missing or malformed'; return 1; }
  image_file="$BUILD_DIR/built-image.identity"
  write_immutable "$image_file" "captured_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) source_sha=$checked_out_sha image_id=$built_image" || return 1
  BUILT_IMAGE_ID=$built_image
  printf 'built_image_bound=source:%s image:%s evidence:%s\n' "$checked_out_sha" "$built_image" "$image_file"
}

target_compare() {
  local rc
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    if compare_initial_local_target; then return 0; else rc=$?; fi
    printf 'target_compare=reject exit=%s rows=%s\n' "$rc" "$(wc -l < "$TARGET_DIR/target-comparisons.tsv" 2>/dev/null || printf 0)" >&2
    return "$rc"
  fi
  if ! copy_immutable "$BASELINE_DIR/baseline.snapshot.json" "$TARGET_DIR/baseline.snapshot.json"; then return 1; fi
  if ROLLOUT_CHECKOUT_PATH="$CHECKOUT" bash "$VERIFIER" compare \
      "$TARGET_DIR/baseline.snapshot.json" "$TARGET_DIR/target.snapshot.json" \
      "$BUILT_IMAGE_ID" "$TARGET_DIR" target >/dev/null; then return 0; else rc=$?; fi
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
  local render_fields=(config_sha256 env_sha256 override_sha256 stop_grace_period compose_replica_count compose_client_addr_trust)
  write_immutable "$results" '' || return 1
  [ "$(jq -er '.provenance.compose_source' "$baseline_file")" = live-container-inspection ] && \
    [ "$(jq -er '.provenance.compose_source' "$render_file")" = selected-compose-render ] && \
    [ "$(jq -er '.provenance.compose_source' "$target_file")" = selected-compose-render ] || {
    fail 'initial-local target comparison has invalid baseline or render provenance'
    return 1
  }
  for field in "${baseline_fields[@]}"; do
    before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail "initial-local baseline is missing $field"; return 1; }
    after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail "initial-local target is missing $field"; return 1; }
    if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
    append_evidence_row "$results" target "live_baseline_$field" "$before" "$after" "$result" || return 1
  done
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
  local old_file="$BASELINE_DIR/baseline.snapshot.json"
  local target_file="$TARGET_DIR/preflight.snapshot.json"
  local results="$TARGET_DIR/preflight-comparisons.tsv"
  local fields=(config_sha256 env_sha256 override_sha256 stop_grace_period)
  local field before after result expected failures=0
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local render_file="$BASELINE_DIR/initial-local-target-render.snapshot.json"
    local live_fields=(container_id image_id state mounts_sha256 container_stop_timeout exposure_sha256 container_env_sha256 container_replica_count container_client_addr_trust)
    [ "$(jq -er '.provenance.compose_source' "$old_file")" = live-container-inspection ] && \
      [ "$(jq -er '.provenance.compose_source' "$render_file")" = selected-compose-render ] && \
      [ "$(jq -er '.provenance.compose_source' "$target_file")" = selected-compose-render ] || {
      fail 'initial-local preflight has invalid baseline or render provenance'
      return 1
    }
    write_immutable "$results" '' || return 1
    for field in "${fields[@]}"; do
      before=$(jq -er --arg field "$field" '.[$field]' "$render_file") || { fail 'pinned target render is missing a preflight comparison field'; return 1; }
      after=$(jq -er --arg field "$field" '.[$field]' "$target_file") || { fail 'resolved target is missing a preflight comparison field'; return 1; }
      if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
      append_evidence_row "$results" preflight "selected_render_$field" "$before" "$after" "$result" || return 1
    done
    for field in "${live_fields[@]}"; do
      before=$(jq -er --arg field "$field" '.[$field]' "$old_file") || { fail 'live baseline is missing an inspected preflight comparison field'; return 1; }
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
    if [ "$before" = "$after" ]; then result=pass; else result=fail; failures=$((failures + 1)); fi
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

compare_rollback_to_baseline() {
  local baseline_file="$ROLLBACK_DIR/baseline.snapshot.json"
  local rollback_file="$ROLLBACK_DIR/rollback.snapshot.json"
  local results="$ROLLBACK_DIR/rollback-equivalence.tsv"
  local fields=(image_id mounts_sha256 stop_grace_period container_stop_timeout config_sha256 env_sha256 override_sha256 exposure_sha256 container_env_sha256 compose_replica_count compose_client_addr_trust container_replica_count container_client_addr_trust)
  local field before after state timeout_value grace
  local expected_count=${#fields[@]}
  ROLLBACK_COMPARE_FAILURES=0
  copy_immutable "$BASELINE_DIR/baseline.snapshot.json" "$baseline_file" || return 1
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    local render_file="$ROLLBACK_DIR/initial-local-target-render.snapshot.json"
    local target_render="$BASELINE_DIR/initial-local-target-render.snapshot.json"
    local live_fields=(image_id mounts_sha256 container_stop_timeout exposure_sha256 container_env_sha256)
    local render_fields=(config_sha256 env_sha256 override_sha256 stop_grace_period compose_replica_count compose_client_addr_trust)
    [ "$(jq -er '.provenance.compose_source' "$baseline_file")" = live-container-inspection ] && \
      [ "$(jq -er '.provenance.compose_source' "$target_render")" = selected-compose-render ] && \
      [ "$(jq -er '.provenance.compose_source' "$rollback_file")" = selected-compose-render ] || {
      fail 'initial-local rollback has invalid baseline or render provenance'
      return 1
    }
    copy_immutable "$target_render" "$render_file" || return 1
    write_immutable "$results" '' || return 1
    for field in "${live_fields[@]}"; do
      before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail 'live baseline is missing a rollback comparison field'; return 1; }
      after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback snapshot is missing an inspected comparison field'; return 1; }
      record_rollback_row "$results" "live_baseline_$field" "$before" "$after" || return 1
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
    before=$(jq -er --arg field "$field" '.[$field]' "$baseline_file") || { fail 'baseline snapshot is missing an equivalence field'; return 1; }
    after=$(jq -er --arg field "$field" '.[$field]' "$rollback_file") || { fail 'rollback snapshot is missing an equivalence field'; return 1; }
    record_rollback_row "$results" "$field" "$before" "$after" || return 1
  done
  state=$(jq -er '.state' "$rollback_file") || { fail 'rollback state is missing'; return 1; }
  record_rollback_row "$results" rollback_state running "$state" || return 1
  grace=$(jq -er '.stop_grace_period' "$rollback_file") || { fail 'rollback grace is missing'; return 1; }
  record_rollback_row "$results" rollback_grace_policy 2m0s "$grace" || return 1
  timeout_value=$(jq -er '.container_stop_timeout' "$rollback_file") || { fail 'rollback inspected timeout is missing'; return 1; }
  record_rollback_row "$results" rollback_inspected_stop_timeout 120 "$timeout_value" || return 1
  [ "$expected_count" -eq 13 ] || { fail 'internal rollback equivalence field count changed'; return 1; }
  [ "$ROLLBACK_COMPARE_FAILURES" -eq 0 ] || { fail 'rollback does not match the captured baseline'; return 1; }
}

validate_rollback_blob_authority() {
  local compose_file="$ROLLBACK_DIR/rollback-blob-compose.json"
  local containers_file="$ROLLBACK_DIR/rollback-pre-up-blob-containers.json"
  capture_compose_blob_inventory "$compose_file" || return 1
  capture_running_blob_inventory "$containers_file" 1 || return 1
  validate_blob_authority "$containers_file" "$compose_file" 1 1 1
}

target_failure_marker() {
  local reason=$1
  write_immutable "$TARGET_DIR/target-failure.txt" \
    "captured_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) target_sha=$TARGET_SHA source_sha=$PREVIOUS_SHA reason=$reason target_snapshot=$( [ -f "$TARGET_DIR/target.snapshot.json" ] && sha256_file "$TARGET_DIR/target.snapshot.json" || printf unavailable )"
}

rollback_after_target_failure() {
  local reason=$1
  if ! target_failure_marker "$reason"; then
    printf 'target failure marker unavailable; rolling back anyway (reason=%s)\n' "$reason" >&2
    if ! write_immutable "$ROLLBACK_DIR/target-failure-marker-warning.txt" \
        "target_sha=$TARGET_SHA reason=$reason target_failure_marker=unavailable"; then
      printf 'rollback evidence also could not persist the missing-marker warning (reason=%s)\n' "$reason" >&2
    fi
  fi
  perform_rollback || return 2
}

restore_checkout_and_tag() {
  git reset --hard "$PREVIOUS_SHA" >/dev/null || { fail 'cannot restore the previous checkout'; return 1; }
  docker image tag "${BASE_IMAGE_ID#sha256:}" telegramd:local >/dev/null 2>&1 || { fail 'cannot restore the baseline image tag'; return 1; }
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
  local rollback_id
  restore_checkout_and_tag || return 1
  if ! validate_rollback_blob_authority; then
    write_immutable "$ROLLBACK_DIR/rollback-blob-authority-rejected.txt" \
      "result=rejected checkout_sha=$PREVIOUS_SHA reason=current-containers-or-baseline-render-mismatch" || return 1
    fail 'current containers or baseline rollback render do not satisfy durable blob authority; baseline was not started'
    return 1
  fi
  docker compose up -d --no-build --no-deps telegramd </dev/null || { fail 'baseline telegramd recreation failed'; return 1; }
  rollback_id=$(current_service_id telegramd) || { fail 'rollback telegramd container ID is unavailable'; return 1; }
  capture_snapshot "$rollback_id" "$ROLLBACK_DIR" rollback || return 1
  local equivalence_rc=0 readiness_rc=0
  if compare_rollback_to_baseline; then :; else equivalence_rc=$?; fi
  if run_rollback_readiness "$rollback_id"; then :; else readiness_rc=$?; fi
  if [ "$equivalence_rc" -eq 0 ] && [ "$readiness_rc" -eq 0 ]; then
    write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
      "result=verified checkout_sha=$PREVIOUS_SHA container_id=$rollback_id image_id=$BASE_IMAGE_ID readiness=pass baseline_equivalence=pass" || return 1
    printf 'rollback=verified baseline_sha=%s container_id=%s\n' "$PREVIOUS_SHA" "$rollback_id"
    return 0
  fi
  write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
    "result=failed checkout_sha=$PREVIOUS_SHA container_id=$rollback_id equivalence_exit=$equivalence_rc readiness_exit=$readiness_rc"
  fail 'rollback did not pass baseline-equivalence and readiness gates'
}

run_apply() {
  local branch origin_sha previous_sha baseline_id target_id rc dir runtime_source_sha
  verify_target_local_compose || return 1
  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot fetch origin/main under deployment lock'; return 1; }
  branch=$(git -C "$CHECKOUT" branch --show-current) || { fail 'cannot read checkout branch'; return 1; }
  previous_sha=$(git -C "$CHECKOUT" rev-parse HEAD) || { fail 'cannot read baseline checkout SHA'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot read origin/main SHA'; return 1; }
  runtime_source_sha=${ROLLOUT_RUNNER_SOURCE_SHA:-$TARGET_SHA}
  [[ "$runtime_source_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'rollout runtime source SHA must be a full commit ID'; return 1; }
  [ "$branch" = main ] || { fail 'deployment checkout is not on main'; return 1; }
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
  if [ "$INITIALIZE_LOCAL" = 1 ] || [ "$TARGET_SHA" = "$TARGET_LOCAL_COMPOSE_TARGET_SHA" ]; then
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
  baseline_id=$(current_service_id telegramd) || { fail 'baseline telegramd container ID is unavailable'; return 1; }
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    capture_snapshot "$baseline_id" "$BASELINE_DIR" baseline live-container-inspection || return 1
  else
    capture_snapshot "$baseline_id" "$BASELINE_DIR" baseline || return 1
  fi
  validate_baseline "$BASELINE_DIR/baseline.snapshot.json" || return 1
  if [ "$INITIALIZE_LOCAL" = 1 ]; then
    capture_snapshot "$baseline_id" "$BASELINE_DIR" initial-local-target-render || return 1
  fi
  preflight_target_local_blob_authority || return 1
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
  if git -C "$CHECKOUT" merge-base --is-ancestor "$PREVIOUS_SHA" "$TARGET_SHA"; then
    git -C "$CHECKOUT" merge --ff-only -q "$TARGET_SHA" || { fail 'fast-forward to authorized application target failed'; return 1; }
  elif [ "$INITIALIZE_LOCAL" = 1 ] && [ "$PREVIOUS_SHA" = "$INITIAL_LOCAL_LEGACY_BASELINE_SHA" ] && \
      [ "$TARGET_SHA" = "$INITIAL_LOCAL_COMPOSE_TARGET_SHA" ]; then
    git -C "$CHECKOUT" reset --hard -q "$TARGET_SHA" || { fail 'fixed reviewed initial-local target checkout failed'; return 1; }
  else
    fail 'authorized application target is not a fast-forward from the live baseline'
    return 1
  fi
  [ "$(git -C "$CHECKOUT" rev-parse HEAD)" = "$TARGET_SHA" ] || { fail 'fast-forward did not reach the authorized target'; return 1; }
  if ! verify_approved_gates || ! verify_runtime_sources; then
    git reset --hard "$PREVIOUS_SHA" >/dev/null || { fail 'pinned gate check failed and baseline checkout could not be restored'; return 1; }
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
  elif ! validate_blob_authority "$target_blob_containers" "$target_blob_compose" 0 1 0; then
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
  docker compose build -q telegramd </dev/null || {
    rc=$?
    if ! write_immutable "$BUILD_DIR/build-result.txt" "result=failed exit=$rc source_sha=$TARGET_SHA"; then
      restore_checkout_and_tag || return 1
      fail 'build failed and its evidence could not be persisted; baseline checkout and tag restored'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'telegramd build failed; baseline checkout and tag restored'
    return 1
  }
  if record_build_identity; then :; else
    rc=$?
    if ! write_immutable "$BUILD_DIR/build-result.txt" "result=rejected exit=$rc source_sha=$TARGET_SHA reason=built-image-id-invalid"; then
      restore_checkout_and_tag || return 1
      fail 'built image identity evidence could not be persisted; baseline checkout and tag restored'
      return 1
    fi
    restore_checkout_and_tag || return 1
    fail 'built image identity could not be verified; baseline checkout and tag restored'
    return 1
  fi

  if docker compose up -d </dev/null; then :; else
    rc=$?
    rollback_after_target_failure "compose_up_exit_$rc" || return 2
    return 1
  fi
  target_id=$(current_service_id telegramd) || target_id=''
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
  local target_runtime_compose="$TARGET_DIR/target-blob-compose.json"
  local target_runtime_containers="$TARGET_DIR/target-blob-containers.json"
  if ! capture_compose_blob_inventory "$target_runtime_compose" || \
     ! capture_running_blob_inventory "$target_runtime_containers" || \
     ! validate_blob_authority "$target_runtime_containers" "$target_runtime_compose"; then
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
  write_immutable "$TARGET_DIR/target-result.txt" \
    "result=verified source_sha=$TARGET_SHA image_id=$BUILT_IMAGE_ID container_id=$target_id readiness=pass schema_gate=pass"
  printf 'rollout=verified sha=%s image_id=%s container_id=%s evidence=%s\n' "$TARGET_SHA" "$BUILT_IMAGE_ID" "$target_id" "$TARGET_DIR"
}

main() {
  if [ "$#" -eq 2 ] && [ "$1" = reconcile ]; then
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
  [ "$#" -eq 3 ] && { [ "$1" = apply ] || [ "$1" = initialize-local ]; } || { usage; return 64; }
  RUNNER_ACTION=$1
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
  if [ "$ROLLOUT_PINNED_EXECUTION" = 1 ]; then
    local lock_target
    lock_target=$(readlink "/proc/$$/fd/9") || { fail 'inherited deployment lock descriptor is missing'; return 1; }
    [ "$lock_target" = "$LOCK_PATH" ] || { fail 'inherited deployment lock descriptor points to another path'; return 1; }
  else
    acquire_shared_lock || { fail 'cannot acquire shared deployment lock'; return 1; }
  fi
  verify_approved_gates || return 1
  cd "$CHECKOUT"
  run_apply
}

if [ "${ROLLOUT_RUNNER_SOURCE_ONLY:-0}" != 1 ]; then
  main "$@"
fi
