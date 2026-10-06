#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
VERIFIER="$SCRIPT_DIR/rollout-verifier.sh"
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
readonly APPROVED_VERIFIER_SHA=f5133ba9c4be17a587fe7e80d01bbe50aa905b9e68586fcdaf992816bdf9b38e
readonly APPROVED_SCHEMA_GATE_SHA=4ecb2a3962749447d0534eda36676b81f098c8b966411c7a75817690586bc1ca

ROLLOUT_RUNNER_TEST_MODE=${ROLLOUT_RUNNER_TEST_MODE:-0}
CHECKOUT=${ROLLOUT_RUNNER_CHECKOUT:-/opt/telegram-server}
EVIDENCE_ROOT=${ROLLOUT_RUNNER_EVIDENCE_ROOT:-/root}
LOCK_PATH=${ROLLOUT_RUNNER_LOCK_PATH:-/tmp/telegram-server-deploy.lock}
ENV_FILE=${ROLLOUT_RUNNER_ENV_FILE:-/opt/telegram-server/.env}
OVERRIDE_FILE=${ROLLOUT_RUNNER_OVERRIDE_FILE:-/opt/telegram-server/docker-compose.override.yml}
READY_SECONDS=${ROLLOUT_RUNNER_READY_SECONDS:-120}

ROLLOUT_VERIFIER_SOURCE_ONLY=1
. "$VERIFIER"

fail() {
  printf 'rollout runner rejected: %s\n' "$1" >&2
  return 1
}

usage() {
  printf '%s\n' 'usage: rollout-runner.sh apply TARGET_SHA EXPECTED_BASELINE_SHA' >&2
}

sha256_file() {
  local output
  output=$(sha256sum -- "$1") || return 1
  printf '%s' "${output%% *}"
}

verify_approved_gates() {
  local verifier_sha schema_sha
  verifier_sha=$(sha256_file "$VERIFIER") || { fail 'cannot hash the rollout verifier'; return 1; }
  schema_sha=$(sha256_file "$SCHEMA_GATE") || { fail 'cannot hash the schema gate'; return 1; }
  [ "$verifier_sha" = "$APPROVED_VERIFIER_SHA" ] || { fail 'rollout verifier hash differs from reviewed artifact'; return 1; }
  [ "$schema_sha" = "$APPROVED_SCHEMA_GATE_SHA" ] || { fail 'schema gate hash differs from reviewed artifact'; return 1; }
}

require_runtime() {
  local uid
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    [ -d "$CHECKOUT" ] || { fail 'fixture checkout is missing'; return 1; }
    [ -d "$EVIDENCE_ROOT" ] && [ ! -L "$EVIDENCE_ROOT" ] || { fail 'fixture evidence root is missing or symlinked'; return 1; }
    return 0
  fi
  uid=$(id -u) || { fail 'cannot inspect effective uid'; return 1; }
  [ "$uid" = 0 ] || { fail 'production rollout requires uid 0'; return 1; }
  [ "$PWD" = /opt/telegram-server ] && [ "$CHECKOUT" = /opt/telegram-server ] || {
    fail 'production rollout must run from /opt/telegram-server'
    return 1
  }
  [ "$EVIDENCE_ROOT" = /root ] && [ "$(stat -c %u /root)" = 0 ] && [ "$(stat -c %a /root)" = 700 ] || {
    fail 'production evidence root must be root-owned mode 0700'
    return 1
  }
  [ "$ENV_FILE" = /opt/telegram-server/.env ] && [ "$OVERRIDE_FILE" = /opt/telegram-server/docker-compose.override.yml ] || {
    fail 'production secret and override paths are fixed'
    return 1
  }
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
  local dir=$1 mode owner expected_owner
  [ -d "$dir" ] && [ ! -L "$dir" ] || { fail 'evidence phase directory missing or symlinked'; return 1; }
  mode=$(stat -c %a -- "$dir") || { fail 'cannot inspect evidence directory mode'; return 1; }
  owner=$(stat -c %u -- "$dir") || { fail 'cannot inspect evidence directory owner'; return 1; }
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then expected_owner=$(id -u); else expected_owner=0; fi
  [ "$mode" = 700 ] && [ "$owner" = "$expected_owner" ] || { fail 'evidence phase directory must be private and owned by the runner'; return 1; }
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
  local path=$1 contents=$2 dir temp mode owner expected_owner
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
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then expected_owner=$(id -u); else expected_owner=0; fi
  [ "$mode" = 600 ] && [ "$owner" = "$expected_owner" ] || { fail 'evidence file is not private'; return 1; }
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

current_service_id() {
  local service=$1 id
  id=$(docker compose ps -q "$service" </dev/null 2>/dev/null) || return 1
  id=$(printf '%s' "$id" | head -n 1)
  [[ "$id" =~ ^[0-9a-f]{64}$ ]] || return 1
  printf '%s' "$id"
}

capture_snapshot() {
  local id=$1 dir=$2 name=$3 output inspect_json compose_json snapshot
  output="$dir/$name.snapshot.json"
  [ ! -e "$output" ] && [ ! -L "$output" ] || { fail 'snapshot evidence already exists'; return 1; }
  if [ "$ROLLOUT_RUNNER_TEST_MODE" != 1 ]; then
    "$VERIFIER" snapshot "$id" "$dir" "$name" "$ENV_FILE" "$OVERRIDE_FILE" >/dev/null 2>&1 || {
      fail "cannot capture $name snapshot"
      return 1
    }
  else
    inspect_json=$(docker inspect "$id" 2>/dev/null) || { fail "cannot inspect $name container"; return 1; }
    compose_json=$(docker compose config --format json </dev/null 2>/dev/null) || { fail "cannot resolve $name Compose config"; return 1; }
    snapshot=$(snapshot_from_json "$inspect_json" "$compose_json" "$ENV_FILE" "$OVERRIDE_FILE") || {
      fail "cannot form allowlisted $name snapshot"
      return 1
    }
    write_immutable "$output" "$snapshot" || return 1
  fi
  [ -f "$output" ] && [ ! -L "$output" ] && [ "$(stat -c %a -- "$output")" = 600 ] || {
    fail "$name snapshot is not private"
    return 1
  }
}

validate_baseline() {
  local file=$1 image state grace timeout_value result_file
  image=$(jq -er '.image_id | select(test("^sha256:[0-9a-f]{64}$"))' "$file") || { fail 'baseline image identity is invalid'; return 1; }
  state=$(jq -er '.state' "$file") || { fail 'baseline state is missing'; return 1; }
  grace=$(jq -er '.stop_grace_period' "$file") || { fail 'baseline grace is missing'; return 1; }
  timeout_value=$(jq -er '.container_stop_timeout' "$file") || { fail 'baseline inspected timeout is missing'; return 1; }
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
      "target_sha=$TARGET_SHA dump_path=$dump_path dump_sha256=$dump_sha dump_complete=yes postgres_image_id=$postgres_image isolated_restore=pass restore_network=none restore_data=tmpfs restore_cleanup=pass"
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
  if ! copy_immutable "$BASELINE_DIR/baseline.snapshot.json" "$TARGET_DIR/baseline.snapshot.json"; then return 1; fi
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    if compare_snapshots "$TARGET_DIR/baseline.snapshot.json" "$TARGET_DIR/target.snapshot.json" \
        "$BUILT_IMAGE_ID" "$TARGET_DIR/target-comparisons.tsv"; then return 0; else rc=$?; fi
  else
    if "$VERIFIER" compare "$TARGET_DIR/baseline.snapshot.json" "$TARGET_DIR/target.snapshot.json" \
        "$BUILT_IMAGE_ID" "$TARGET_DIR" target >/dev/null; then return 0; else rc=$?; fi
  fi
  printf 'target_compare=reject exit=%s rows=%s\n' "$rc" "$(wc -l < "$TARGET_DIR/target-comparisons.tsv" 2>/dev/null || printf 0)" >&2
  return 1
}

run_target_readiness() {
  local id=$1
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    wait_readiness target "$id" "$TARGET_DIR/readiness-target.tsv" "$READY_SECONDS" 1 probe_live
  else
    "$VERIFIER" ready target "$id" "$TARGET_DIR" >/dev/null
  fi
}

run_rollback_readiness() {
  local id=$1
  if [ "$ROLLOUT_RUNNER_TEST_MODE" = 1 ]; then
    wait_readiness rollback "$id" "$ROLLBACK_DIR/readiness-rollback.tsv" "$READY_SECONDS" 1 probe_live
  else
    "$VERIFIER" ready rollback "$id" "$ROLLBACK_DIR" >/dev/null
  fi
}

compare_resolved_preflight() {
  local old_file="$BASELINE_DIR/baseline.snapshot.json"
  local target_file="$TARGET_DIR/preflight.snapshot.json"
  local results="$TARGET_DIR/preflight-comparisons.tsv"
  local fields=(config_sha256 env_sha256 override_sha256 stop_grace_period)
  local field before after result expected failures=0
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

target_failure_marker() {
  local reason=$1
  write_immutable "$TARGET_DIR/target-failure.txt" \
    "captured_at_utc=$(date -u +%Y-%m-%dT%H:%M:%SZ) target_sha=$TARGET_SHA source_sha=$PREVIOUS_SHA reason=$reason target_snapshot=$( [ -f "$TARGET_DIR/target.snapshot.json" ] && sha256_file "$TARGET_DIR/target.snapshot.json" || printf unavailable )"
}

restore_checkout_and_tag() {
  git reset --hard "$PREVIOUS_SHA" >/dev/null || { fail 'cannot restore the previous checkout'; return 1; }
  docker image tag "${BASE_IMAGE_ID#sha256:}" telegramd:local >/dev/null 2>&1 || { fail 'cannot restore the baseline image tag'; return 1; }
}

perform_rollback() {
  local rollback_id
  restore_checkout_and_tag || return 1
  docker compose up -d --no-build --no-deps telegramd </dev/null || { fail 'baseline telegramd recreation failed'; return 1; }
  rollback_id=$(current_service_id telegramd) || { fail 'rollback telegramd container ID is unavailable'; return 1; }
  capture_snapshot "$rollback_id" "$ROLLBACK_DIR" rollback || return 1
  local equivalence_rc=0 readiness_rc=0
  if compare_rollback_to_baseline; then :; else equivalence_rc=$?; fi
  if run_rollback_readiness "$rollback_id"; then :; else readiness_rc=$?; fi
  if [ "$equivalence_rc" -eq 0 ] && [ "$readiness_rc" -eq 0 ]; then
    write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
      "result=verified checkout_sha=$PREVIOUS_SHA container_id=$rollback_id image_id=$BASE_IMAGE_ID readiness=pass baseline_equivalence=pass"
    printf 'rollback=verified baseline_sha=%s container_id=%s\n' "$PREVIOUS_SHA" "$rollback_id"
    return 0
  fi
  write_immutable "$ROLLBACK_DIR/rollback-result.txt" \
    "result=failed checkout_sha=$PREVIOUS_SHA container_id=$rollback_id equivalence_exit=$equivalence_rc readiness_exit=$readiness_rc"
  fail 'rollback did not pass baseline-equivalence and readiness gates'
}

run_apply() {
  local branch origin_sha previous_sha baseline_id target_id rc
  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot fetch origin/main under deployment lock'; return 1; }
  branch=$(git -C "$CHECKOUT" branch --show-current) || { fail 'cannot read checkout branch'; return 1; }
  previous_sha=$(git -C "$CHECKOUT" rev-parse HEAD) || { fail 'cannot read baseline checkout SHA'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot read origin/main SHA'; return 1; }
  [ "$branch" = main ] || { fail 'deployment checkout is not on main'; return 1; }
  [ "$previous_sha" = "$EXPECTED_BASELINE_SHA" ] || { fail 'live checkout differs from the expected baseline SHA'; return 1; }
  [ "$origin_sha" = "$TARGET_SHA" ] || { fail 'origin/main differs from the authorized target SHA'; return 1; }

  PREVIOUS_SHA=$previous_sha
  create_phase_dirs || return 1
  verify_approved_gates || return 1

  baseline_id=$(current_service_id telegramd) || { fail 'baseline telegramd container ID is unavailable'; return 1; }
  capture_snapshot "$baseline_id" "$BASELINE_DIR" baseline || return 1
  validate_baseline "$BASELINE_DIR/baseline.snapshot.json" || return 1
  capture_backup_and_restore || return 1

  git -C "$CHECKOUT" fetch -q origin || { fail 'cannot recheck origin/main before fast-forward'; return 1; }
  origin_sha=$(git -C "$CHECKOUT" rev-parse origin/main) || { fail 'cannot recheck origin/main SHA'; return 1; }
  [ "$origin_sha" = "$TARGET_SHA" ] || { fail 'origin/main drifted after the verified backup'; return 1; }
  git -C "$CHECKOUT" merge --ff-only -q origin/main || { fail 'fast-forward to authorized origin/main failed'; return 1; }
  [ "$(git -C "$CHECKOUT" rev-parse HEAD)" = "$TARGET_SHA" ] || { fail 'fast-forward did not reach the authorized target'; return 1; }
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
    target_failure_marker "compose_up_exit_$rc" || { fail 'target failed and failure evidence could not be persisted; rollback withheld'; return 2; }
    perform_rollback || return 2
    return 1
  fi
  target_id=$(current_service_id telegramd) || target_id=''
  if [ -n "$target_id" ]; then
    if capture_snapshot "$target_id" "$TARGET_DIR" target; then :; else
      target_failure_marker snapshot_capture_failed || { fail 'target snapshot failed and failure evidence could not be persisted; rollback withheld'; return 2; }
      perform_rollback || return 2
      return 1
    fi
  else
    target_failure_marker target_container_id_unavailable || { fail 'target container ID unavailable and failure evidence could not be persisted; rollback withheld'; return 2; }
    perform_rollback || return 2
    return 1
  fi

  if ! target_compare; then
    target_failure_marker target_comparison_rejected || { fail 'target comparison failed and failure evidence could not be persisted; rollback withheld'; return 2; }
    printf '%s\n' 'target comparison rejected; recorded comparison evidence before rollback' >&2
    perform_rollback || return 2
    return 1
  fi
  if run_target_readiness "$target_id"; then :; else
    rc=$?
    target_failure_marker "target_readiness_rejected_exit_$rc" || { fail 'target readiness failed and failure evidence could not be persisted; rollback withheld'; return 2; }
    perform_rollback || return 2
    return 1
  fi
  if "$SCHEMA_GATE" check "$TARGET_DIR" >/dev/null; then :; else
    rc=$?
    target_failure_marker "schema_gate_rejected_exit_$rc" || { fail 'schema gate failed and failure evidence could not be persisted; rollback withheld'; return 2; }
    perform_rollback || return 2
    return 1
  fi
  write_immutable "$TARGET_DIR/target-result.txt" \
    "result=verified source_sha=$TARGET_SHA image_id=$BUILT_IMAGE_ID container_id=$target_id readiness=pass schema_gate=pass"
  printf 'rollout=verified sha=%s image_id=%s container_id=%s evidence=%s\n' "$TARGET_SHA" "$BUILT_IMAGE_ID" "$target_id" "$TARGET_DIR"
}

main() {
  [ "$#" -eq 3 ] && [ "$1" = apply ] || { usage; return 64; }
  TARGET_SHA=$2
  EXPECTED_BASELINE_SHA=$3
  [[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]] || { fail 'target SHA must be a full commit ID'; return 64; }
  [[ "$EXPECTED_BASELINE_SHA" =~ ^[0-9a-f]{40}$ ]] || { fail 'expected baseline SHA must be a full commit ID'; return 64; }
  [[ "$READY_SECONDS" =~ ^[0-9]+$ ]] && [ "$READY_SECONDS" -ge 1 ] && [ "$READY_SECONDS" -le 120 ] || {
    fail 'readiness bound must be 1 through 120 seconds'
    return 64
  }
  require_runtime || return 1
  acquire_shared_lock || { fail 'cannot acquire shared deployment lock'; return 1; }
  verify_approved_gates || return 1
  cd "$CHECKOUT"
  run_apply
}

if [ "${ROLLOUT_RUNNER_SOURCE_ONLY:-0}" != 1 ]; then
  main "$@"
fi
