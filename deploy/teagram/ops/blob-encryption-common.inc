#!/usr/bin/env bash

readonly REPO=/opt/teagram
readonly STATE_DIR="$REPO/.state"
readonly AUTHORITY_DIR="$STATE_DIR/blob-mode"
readonly LOCK_FILE=/tmp/teagram-deploy.lock
readonly EXPECTED_COMPOSE_FILES="$REPO/docker-compose.yml,$REPO/docker-compose.override.yml"
readonly POSTGRES_CONTAINER=teagram-postgres-1
readonly SERVER_CONTAINER=teagram-teagramd-1
readonly RUSTFS_CONTAINER=teagram-rustfs-1
readonly EXPECTED_ROOT_UID=0
readonly KEY_DIR=/etc/teagramd/media-keys
readonly KEY_FILE="$KEY_DIR/1.key"
readonly KEY_BACKUP=/etc/teagramd/media-key-backups/1.key
readonly EVIDENCE_ROOT=/root/teagram-media-encryption
readonly PROGRESS_PATH=/run/teagramd/reports/blob-encryption-progress.jsonl
readonly MINIMUM_MEDIA_SHA=91e50a0

declare -a COMPOSE=()
declare -a SERVER_CONTAINERS=()
COMPOSE_ENV_FILES=()
REPORT_ROOT=${TEAGRAM_BLOB_MODE_REPORT_ROOT:-/var/lib/teagram-blob-mode-reports}
export TEAGRAM_BLOB_MODE_REPORT_ROOT="$REPORT_ROOT"
SERVER_STOPPED=0
RUSTFS_STOPPED=0
SERVER_IMAGE_ID=
LIVE_IMAGE_SHA=

ops_fail() {
  printf 'media encryption operation rejected: %s\n' "$1" >&2
  exit 1
}

sha256_text() {
  local digest
  digest=$(printf '%s' "$1" | sha256sum) || return 1
  printf '%s' "${digest%% *}"
}

sha256_file_contents() {
  local digest
  digest=$(sha256sum < "$1") || return 1
  printf '%s' "${digest%% *}"
}

report_hash_mismatch() {
  local invariant=$1 expected=$2 actual=$3 expected_sha actual_sha
  expected_sha=$(sha256_text "$expected") || ops_fail 'cannot fingerprint preflight invariant'
  actual_sha=$(sha256_text "$actual") || ops_fail 'cannot fingerprint preflight invariant'
  printf 'preflight mismatch invariant=%s expected_sha256=%s actual_sha256=%s\n' \
    "$invariant" "$expected_sha" "$actual_sha" >&2
}

check_hash_value() {
  local invariant=$1 expected=$2 actual=$3
  [[ "$expected" == "$actual" ]] && return 0
  report_hash_mismatch "$invariant" "$expected" "$actual"
  return 1
}

check_compose_invariant() {
  local invariant=$1 filter=$2 expected=$3 actual
  actual=$(jq -cS "$filter" "$OPS_TEMP_DIR/compose.json" 2>/dev/null) || actual='invalid-compose-json'
  [[ "$actual" == "$expected" ]] && return 0
  report_hash_mismatch "$invariant" "$expected" "$actual"
  return 1
}

media_path_metadata() {
  local path=$1 kind=$2 format=$3 metadata
  case "$kind" in
    directory) [[ -d "$path" && ! -L "$path" ]] || { printf 'missing-or-unsafe-directory'; return; } ;;
    file) [[ -f "$path" && ! -L "$path" ]] || { printf 'missing-or-unsafe-file'; return; } ;;
    *) ops_fail 'invalid media metadata check' ;;
  esac
  metadata=$(stat -c "$format" -- "$path" 2>/dev/null) || { printf 'metadata-unavailable'; return; }
  printf '%s:%s' "$kind" "$metadata"
}

private_dir() {
  local path=$1 mode
  [[ -d "$path" && ! -L "$path" ]] || return 1
  [[ "$(stat -c '%u' -- "$path")" == "$EXPECTED_ROOT_UID" ]] || return 1
  mode=$(stat -c '%a' -- "$path") || return 1
  [[ "$mode" == 700 ]]
}

private_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "$EXPECTED_ROOT_UID:600" ]]
}

authority_record_file() {
  local path=$1
  [[ -f "$path" && ! -L "$path" && "$(stat -c '%u:%a' -- "$path")" == "$EXPECTED_ROOT_UID:644" ]]
}

ensure_evidence_root() {
  if [[ ! -e "$EVIDENCE_ROOT" && ! -L "$EVIDENCE_ROOT" ]]; then
    mkdir -m 700 -- "$EVIDENCE_ROOT" || ops_fail 'cannot create private evidence directory'
  fi
  private_dir "$EVIDENCE_ROOT" || ops_fail 'evidence directory is not root-owned mode 0700'
}

stage_runtime_helpers() {
  git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-transition-runner.py" \
    > "$OPS_TEMP_DIR/blob-transition-runner.py" || ops_fail 'cannot stage the reviewed blob transition verifier'
  git -C "$REPO" show "$OPS_SHA:deploy/teagramd/rollout-runner/blob-mode-state.py" \
    > "$OPS_TEMP_DIR/blob-mode-state.py" || ops_fail 'cannot stage the reviewed blob authority verifier'
  chmod 600 -- "$OPS_TEMP_DIR/blob-transition-runner.py" "$OPS_TEMP_DIR/blob-mode-state.py" || \
    ops_fail 'cannot protect staged blob authority verifiers'
}

validate_source_and_runtime() {
  local branch checkout_sha origin_sha
  [[ -d "$REPO" && ! -L "$REPO" ]] || ops_fail 'server checkout is not a real directory'
  [[ -f "$REPO/.env" && ! -L "$REPO/.env" ]] || ops_fail '.env is not a regular file'
  [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || ops_fail 'deployment state is not a real directory'
  [[ -d "$AUTHORITY_DIR" && ! -L "$AUTHORITY_DIR" ]] || ops_fail 'blob authority is not a real directory'

  branch=$(git -C "$REPO" branch --show-current) || ops_fail 'cannot read live checkout branch'
  checkout_sha=$(git -C "$REPO" rev-parse --verify 'HEAD^{commit}') || ops_fail 'cannot read live checkout SHA'
  [[ "$branch" == main || -z "$branch" ]] || ops_fail 'live checkout is not on main'
  [[ "$checkout_sha" == "$LIVE_IMAGE_SHA" ]] || ops_fail 'live checkout and teagramd image revisions differ'

  git -C "$REPO" fetch -q origin main || ops_fail 'cannot refresh origin/main'
  origin_sha=$(git -C "$REPO" rev-parse --verify 'origin/main^{commit}') || ops_fail 'cannot read origin/main SHA'
  git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$origin_sha" || \
    ops_fail 'reviewed operation source is not on origin/main'
  git -C "$REPO" merge-base --is-ancestor "$OPS_SHA" "$LIVE_IMAGE_SHA" || \
    ops_fail 'reviewed operation source has not been deployed'
  git -C "$REPO" merge-base --is-ancestor "$MINIMUM_MEDIA_SHA" "$LIVE_IMAGE_SHA" || \
    ops_fail 'live image does not contain the media encryption operator'
}

inspect_container() {
  local name=$1 expected_service=$2 inspection
  inspection=$(docker inspect --format \
    '{{.State.Status}}|{{index .Config.Labels "com.docker.compose.project"}}|{{index .Config.Labels "com.docker.compose.service"}}|{{index .Config.Labels "com.docker.compose.project.config_files"}}|{{.Image}}|{{index .Config.Labels "org.opencontainers.image.revision"}}|{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' \
    "$name") || ops_fail "cannot inspect $expected_service container"
  IFS='|' read -r INSPECT_STATE INSPECT_PROJECT INSPECT_SERVICE INSPECT_FILES INSPECT_IMAGE \
    INSPECT_REVISION INSPECT_HEALTH <<< "$inspection"
  [[ -n "$INSPECT_STATE" && "$INSPECT_PROJECT" == teagram && "$INSPECT_SERVICE" == "$expected_service" && \
     "$INSPECT_FILES" == "$EXPECTED_COMPOSE_FILES" ]] || ops_fail "$expected_service container identity is not the live S3 stack"
}

validate_live_stack() {
  local names writer_names writer_name writer_service
  names=$(docker ps -a --filter 'label=com.docker.compose.project=teagram' \
    --filter 'label=com.docker.compose.service=teagramd' --format '{{.Names}}') || \
    ops_fail 'cannot list teagramd containers'
  mapfile -t SERVER_CONTAINERS <<< "$names"
  [[ "${#SERVER_CONTAINERS[@]}" == 1 && "${SERVER_CONTAINERS[0]}" == "$SERVER_CONTAINER" ]] || \
    ops_fail 'live S3 stack must have exactly the inspected teagramd replica'

  writer_names=$(docker ps -a --format '{{.Names}}|{{.Label "com.docker.compose.service"}}') || \
    ops_fail 'cannot list containers to verify all teagramd writers'
  while IFS='|' read -r writer_name writer_service; do
    [[ -n "$writer_name" && -n "$writer_service" ]] || continue
    if [[ "$writer_service" == teagramd* && \
      ( "$writer_name" != "$SERVER_CONTAINER" || "$writer_service" != teagramd ) ]]; then
      ops_fail 'an additional teagramd writer container is present'
    fi
  done <<< "$writer_names"

  inspect_container "$SERVER_CONTAINER" teagramd
  [[ "$INSPECT_STATE" == running && "$INSPECT_REVISION" =~ ^[0-9a-f]{40}$ && \
     "$INSPECT_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || ops_fail 'teagramd is not running a verified image'
  SERVER_IMAGE_ID=$INSPECT_IMAGE
  LIVE_IMAGE_SHA=$INSPECT_REVISION

  inspect_container "$POSTGRES_CONTAINER" postgres
  [[ "$INSPECT_STATE" == running && "$INSPECT_HEALTH" == healthy ]] || \
    ops_fail 'Postgres is not healthy'
  inspect_container "$RUSTFS_CONTAINER" rustfs
  [[ "$INSPECT_STATE" == running && "$INSPECT_HEALTH" == healthy ]] || \
    ops_fail 'RustFS is not healthy'
}

configure_compose() {
  local authority_json env_file_lines legacy_parked compose_stderr preflight_failed=0
  authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-transition-runner.py" deployment-env \
    --checkout "$REPO" --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT" --lock-path "$LOCK_FILE") || \
    ops_fail 'live S3 authority or credential selection is not verified'
  env_file_lines=$(jq -er '
    select(.outcome == "s3-accepted") | .env_files |
    if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
    then .[] else error("invalid environment file list") end
  ' <<< "$authority_json") || ops_fail 'live S3 environment selection is invalid'
  mapfile -t COMPOSE_ENV_FILES <<< "$env_file_lines"
  [[ "${#COMPOSE_ENV_FILES[@]}" == 2 && "${COMPOSE_ENV_FILES[0]}" == "$REPO/.env" ]] || \
    ops_fail 'live S3 environment selection does not start with the verified .env'

  legacy_parked="$(dirname "$REPO")/telegram-server/.env.rustfs-parked"
  case "${COMPOSE_ENV_FILES[1]}" in
    "$REPO/.env.rustfs-parked"|"$legacy_parked"|/root/.env.rustfs-parked) ;;
    *) ops_fail 'live S3 environment selection includes an unapproved credential path' ;;
  esac
  for env_file in "${COMPOSE_ENV_FILES[@]}"; do
    [[ -f "$env_file" && ! -L "$env_file" && "$(stat -c '%u' -- "$env_file")" == "$EXPECTED_ROOT_UID" ]] || \
      ops_fail 'verified Compose environment file is unavailable or not root-owned'
  done

  [[ -f "$REPO/docker-compose.yml" && ! -L "$REPO/docker-compose.yml" && \
     -f "$REPO/docker-compose.override.yml" && ! -L "$REPO/docker-compose.override.yml" ]] || \
    ops_fail 'live Compose input is missing or symlinked'
  COMPOSE=(docker compose --project-directory "$REPO" --project-name teagram \
    --file "$REPO/docker-compose.yml" --file "$REPO/docker-compose.override.yml" \
    --env-file "${COMPOSE_ENV_FILES[0]}" --env-file "${COMPOSE_ENV_FILES[1]}")

  compose_stderr="$EVIDENCE_ROOT/compose-render-$RUN_ID.stderr"
  [[ ! -e "$compose_stderr" && ! -L "$compose_stderr" ]] || \
    ops_fail 'Compose render diagnostic path already exists'
  if ! "${COMPOSE[@]}" --profile migration config --format json </dev/null \
    > "$OPS_TEMP_DIR/compose.json" 2> "$compose_stderr"; then
    chmod 600 -- "$compose_stderr" || ops_fail 'cannot protect Compose render diagnostics'
    ops_fail "live Compose render failed; private diagnostic retained at $compose_stderr"
  fi
  chmod 600 -- "$compose_stderr" || ops_fail 'cannot protect Compose render diagnostics'
  check_compose_invariant migration_image \
    '((.services // {})["blob-encrypt-migrate"] // {}).image // null' \
    '"teagramd:local"' || preflight_failed=1
  check_compose_invariant migration_entrypoint \
    '((.services // {})["blob-encrypt-migrate"] // {}).entrypoint // null' \
    '["/usr/local/bin/blob-encrypt-migrate"]' || preflight_failed=1
  check_compose_invariant migration_user \
    '((.services // {})["blob-encrypt-migrate"] // {}).user // null' \
    '"0:0"' || preflight_failed=1
  check_compose_invariant migration_read_only \
    '((.services // {})["blob-encrypt-migrate"] // {}).read_only // null' \
    'true' || preflight_failed=1
  check_compose_invariant migration_profile \
    '((.services // {})["blob-encrypt-migrate"] // {}).profiles // null' \
    '["migration"]' || preflight_failed=1
  check_compose_invariant migration_media_key_mount \
    '((.services // {})["blob-encrypt-migrate"] // {} | .volumes // [] | if type == "array" then . else [] end) | [ .[] | select(type == "object" and .target == "/run/teagramd/media-keys") | {source:(.source // null),target:(.target // null),read_only:(.read_only // null)} ]' \
    "$(jq -cnS --arg source "$KEY_DIR" '[{source:$source,target:"/run/teagramd/media-keys",read_only:true}]')" || preflight_failed=1
  check_compose_invariant migration_reports_mount \
    '((.services // {})["blob-encrypt-migrate"] // {} | .volumes // [] | if type == "array" then . else [] end) | [ .[] | select(type == "object" and .target == "/run/teagramd/reports") | {source:(.source // null),target:(.target // null)} ]' \
    "$(jq -cn --arg root "$REPORT_ROOT" '[{source:$root,target:"/run/teagramd/reports"}]')" || preflight_failed=1
  check_compose_invariant migration_authority_mount \
    '((.services // {})["blob-encrypt-migrate"] // {} | .volumes // [] | if type == "array" then . else [] end) | [ .[] | select(type == "object" and .target == "/run/teagramd/blob-mode") | {source:(.source // null),target:(.target // null)} ]' \
    '[{"source":"/opt/teagram/.state/blob-mode","target":"/run/teagramd/blob-mode"}]' || preflight_failed=1
  check_compose_invariant migration_networks \
    '((.services // {})["blob-encrypt-migrate"] // {} | .networks // {} | if type == "object" then keys | sort else [] end)' \
    '["default","rustfs_private"]' || preflight_failed=1
  check_compose_invariant server_image \
    '((.services // {}).teagramd // {}).image // null' \
    '"teagramd:local"' || preflight_failed=1
  check_compose_invariant server_media_key_current \
    '((.services // {}).teagramd // {} | .environment // {}).TG_MEDIA_KEY_CURRENT // null' \
    '"1"' || preflight_failed=1
  check_compose_invariant server_media_key_dir \
    '((.services // {}).teagramd // {} | .environment // {}).TG_MEDIA_KEY_DIR // null' \
    '"/run/teagramd/media-keys"' || preflight_failed=1
  check_compose_invariant server_media_key_mount \
    '((.services // {}).teagramd // {} | .volumes // [] | if type == "array" then . else [] end) | [ .[] | select(type == "object" and .target == "/run/teagramd/media-keys") | {source:(.source // null),target:(.target // null),read_only:(.read_only // null)} ]' \
    "$(jq -cnS --arg source "$KEY_DIR" '[{source:$source,target:"/run/teagramd/media-keys",read_only:true}]')" || preflight_failed=1
  [[ "$preflight_failed" == 0 ]] || ops_fail 'Compose migration or media-key mount failed reviewed preflight'
}

check_media_key() {
  local invalid_metadata=0 key_sha backup_sha key_length key_length_valid
  check_hash_value media_key_directory_metadata 'directory:65532:700:65532' \
    "$(media_path_metadata "$KEY_DIR" directory '%u:%a:%g')" || invalid_metadata=1
  check_hash_value media_key_file_metadata 'file:65532:600:65532' \
    "$(media_path_metadata "$KEY_FILE" file '%u:%a:%g')" || invalid_metadata=1
  local backup_parent_mode backup_parent=${KEY_BACKUP%/*}
  backup_parent=${backup_parent%/*}
  [[ -d "$backup_parent" && ! -L "$backup_parent" && "$(stat -c '%u' -- "$backup_parent")" == "$EXPECTED_ROOT_UID" ]] || \
    ops_fail 'media key backup parent is unsafe'
  backup_parent_mode=$(stat -c '%a' -- "$backup_parent") || ops_fail 'cannot inspect media key backup parent'
  [[ "$backup_parent_mode" =~ ^[0-7]{3,4}$ ]] || ops_fail 'media key backup parent is unsafe'
  (( (8#$backup_parent_mode & 0022) == 0 )) || ops_fail 'media key backup parent is unsafe'
  private_dir "${KEY_BACKUP%/*}" || ops_fail 'media key backup directory is unsafe'
  check_hash_value media_key_backup_metadata 'file:0:600' \
    "$(media_path_metadata "$KEY_BACKUP" file '%u:%a')" || invalid_metadata=1
  [[ "$invalid_metadata" == 0 ]] || ops_fail 'media key metadata does not match reviewed preflight'

  key_sha=$(sha256_file_contents "$KEY_FILE") || ops_fail 'cannot fingerprint mounted media key'
  backup_sha=$(sha256_file_contents "$KEY_BACKUP") || ops_fail 'cannot fingerprint media key backup'
  if [[ "$key_sha" != "$backup_sha" ]]; then
    ops_fail 'host-local media key backup differs from the mounted key'
  fi

  key_length=$(wc -c < "$KEY_FILE") || ops_fail 'cannot inspect media key encoding length'
  if [[ "$key_length" == 64 || "$key_length" == 65 ]]; then
    key_length_valid=64-or-65-bytes
  else
    key_length_valid="${key_length}-bytes"
  fi
  check_hash_value media_key_hex_length '64-or-65-bytes' "$key_length_valid" || \
    ops_fail 'media key file does not have the expected hex encoding length'
  if ! LC_ALL=C grep -Eq '^[0-9a-fA-F]{64}\r?$' "$KEY_FILE"; then
    report_hash_mismatch media_key_hex_encoding valid-64-character-hex invalid
    ops_fail 'media key file is not hex encoded'
  fi
}

verify_authority() {
  local wanted=$1 authority_json
  authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-mode-state.py" authority \
    --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT") || ops_fail 'blob authority verification failed'
  [[ "$(jq -er '.outcome' <<< "$authority_json")" == "$wanted" ]] || \
    ops_fail "blob authority is not $wanted"
}

verify_encrypted_inventory() {
  local authority_json transition_id expected_sha actual_sha inventory_path inventory_unaccounted_ids
  local record_path="$AUTHORITY_DIR/mode.json"
  authority_record_file "$record_path" || ops_fail 'encrypted authority head is missing or unsafe'
  authority_json=$(cd "$REPO" && python3 "$OPS_TEMP_DIR/blob-mode-state.py" authority \
    --state-dir "$AUTHORITY_DIR" --report-root "$REPORT_ROOT") || ops_fail 'blob authority verification failed'
  [[ "$(jq -er '.outcome' <<< "$authority_json")" == encrypted ]] || ops_fail 'blob authority is not encrypted'
  transition_id=$(jq -er '.transition_id | select(test("^[0-9a-f-]{36}$"))' "$record_path") || \
    ops_fail 'encrypted authority transition ID is invalid'
  expected_sha=$(jq -er '.evidence.encrypted_inventory_sha256 | select(test("^[0-9a-f]{64}$"))' "$record_path") || \
    ops_fail 'encrypted authority inventory digest is invalid'
  inventory_path="$REPORT_ROOT/teagramd-blob-encryption-inventory-$transition_id.json"
  INVENTORY_REPORT_PATH=$inventory_path
  private_file "$inventory_path" || ops_fail 'encrypted inventory report is missing or unsafe'
  actual_sha=$(sha256sum -- "$inventory_path") || ops_fail 'cannot hash encrypted inventory report'
  actual_sha=${actual_sha%% *}
  [[ "$actual_sha" == "$expected_sha" ]] || ops_fail 'encrypted inventory digest does not match authority'

  INVENTORY_REFERENCED=$(jq -er '.evidence.object_count | select(type == "number" and . >= 0)' "$record_path") || \
    ops_fail 'encrypted authority object count is invalid'
  INVENTORY_ENCRYPTED=$(jq -er '
    select(.schema == "teagram.blob-encryption-inventory/v1") |
    .encrypted.classes.valid_envelope.count | select(type == "number" and . >= 0)
  ' "$inventory_path") || ops_fail 'encrypted inventory valid-envelope count is invalid'
  INVENTORY_NO_VALID=$(jq -er '
    select(.schema == "teagram.blob-encryption-inventory/v1") |
    .encrypted.classes.no_valid_envelope.count | select(type == "number" and . >= 0)
  ' "$inventory_path") || ops_fail 'encrypted inventory invalid-envelope count is invalid'
  INVENTORY_MISSING=$(jq -er '.encrypted.classes.missing.count | select(type == "number" and . >= 0)' "$inventory_path") || \
    ops_fail 'encrypted inventory missing-object count is invalid'
  # shellcheck disable=SC2034
  INVENTORY_PLAINTEXT=$(jq -er '.plaintext.classes.no_valid_envelope.count | select(type == "number" and . >= 0)' "$inventory_path") || \
    ops_fail 'plaintext inventory source count is invalid'
  INVENTORY_PLAINTEXT_MISSING=$(jq -er '.plaintext.classes.missing.count | select(type == "number" and . >= 0)' "$inventory_path") || \
    ops_fail 'plaintext inventory missing-object count is invalid'
  INVENTORY_PLAINTEXT_UNACCOUNTED=$(jq -er '
    select(.schema == "teagram.blob-encryption-inventory/v1") |
    .plaintext.classes.unaccounted.count | select(type == "number" and . >= 0)
  ' "$inventory_path") || ops_fail 'plaintext inventory unaccounted-object count is invalid'
  inventory_unaccounted_ids=$(jq -er '
    .plaintext.classes.unaccounted.ids | select(type == "array") | length
  ' "$inventory_path") || ops_fail 'plaintext inventory unaccounted-object IDs are invalid'
  [[ "$INVENTORY_PLAINTEXT_UNACCOUNTED" == "$inventory_unaccounted_ids" ]] || \
    ops_fail 'plaintext inventory unaccounted-object count does not match its IDs'
  [[ "$INVENTORY_REFERENCED" == "$INVENTORY_ENCRYPTED" && "$INVENTORY_NO_VALID" == 0 && \
     "$INVENTORY_MISSING" == 0 && "$INVENTORY_PLAINTEXT_MISSING" == 0 ]] || \
    ops_fail 'encrypted inventory does not account for every referenced object'
}

require_no_unaccounted_plaintext() {
  [[ "${INVENTORY_PLAINTEXT_UNACCOUNTED:-}" == 0 ]] || \
    ops_fail "encrypted migration inventory contains ${INVENTORY_PLAINTEXT_UNACCOUNTED:-unknown} unaccounted plaintext objects in $INVENTORY_REPORT_PATH; resolve them before reporting migration complete"
}

require_no_unaccounted_objects() {
  local result_path=$1 unaccounted_count
  unaccounted_count=$(jq -er '(.unaccounted // []) | select(type == "array") | length' "$result_path") || \
    ops_fail "unaccounted object result is invalid: $result_path"
  [[ "$unaccounted_count" == 0 ]] || \
    ops_fail "media migration result contains $unaccounted_count unaccounted objects in $result_path; resolve them before reporting migration complete"
}

verify_migration_image() {
  local image_id
  image_id=$(docker image inspect --format '{{.Id}}' teagramd:local) || \
    ops_fail 'cannot inspect the teagramd migration image tag'
  [[ "$image_id" == "$SERVER_IMAGE_ID" ]] || \
    ops_fail 'teagramd:local no longer identifies the inspected running server image'
}

run_migration_job() {
  local operation=$1 result_path=$2 stderr_path=$3 job_args=(--writes stopped --progress "$PROGRESS_PATH")
  if [[ "$operation" == cleanup ]]; then
    job_args+=(--cleanup)
  elif [[ "$operation" != encrypt ]]; then
    ops_fail 'unknown media migration job mode'
  fi
  [[ ! -e "$result_path" && ! -L "$result_path" && ! -e "$stderr_path" && ! -L "$stderr_path" ]] || \
    ops_fail 'media migration evidence path already exists'
  verify_migration_image
  if "${COMPOSE[@]}" --profile migration run --rm --no-deps blob-encrypt-migrate "${job_args[@]}" \
    > "$OPS_TEMP_DIR/job.json" 2> "$stderr_path"; then
    chmod 600 -- "$stderr_path" || ops_fail 'cannot protect migration diagnostics'
  else
    chmod 600 -- "$stderr_path" || true
    ops_fail "blob-encrypt-migrate $operation failed; private diagnostic retained at $stderr_path"
  fi
  jq -e --arg mode "$operation" '
    select(.mode == $mode and .authority == "encrypted" and .writes == "stopped" and
      (.objects | type == "number" and . >= 0) and (.bytes | type == "number" and . >= 0) and
      ((.unaccounted // []) | type == "array"))
  ' "$OPS_TEMP_DIR/job.json" >/dev/null || ops_fail 'blob-encrypt-migrate returned an invalid result'
  cp -- "$OPS_TEMP_DIR/job.json" "$result_path" || ops_fail 'cannot preserve migration result'
  chmod 600 -- "$result_path" || ops_fail 'cannot protect migration result'
}

wait_healthy() {
  local name=$1 expected_service=$2 attempts=${3:-60} attempt
  for ((attempt = 1; attempt <= attempts; attempt++)); do
    inspect_container "$name" "$expected_service"
    if [[ "$INSPECT_STATE" == running && "$INSPECT_HEALTH" == healthy ]]; then
      return 0
    fi
    sleep 1
  done
  ops_fail "$expected_service did not become healthy"
}

restart_rustfs() {
  inspect_container "$RUSTFS_CONTAINER" rustfs
  RUSTFS_STOPPED=1
  if [[ "$INSPECT_STATE" != running ]]; then
    docker start "$RUSTFS_CONTAINER" >/dev/null || ops_fail 'could not start RustFS'
  else
    docker restart --time 30 "$RUSTFS_CONTAINER" >/dev/null || ops_fail 'RustFS restart failed'
  fi
  wait_healthy "$RUSTFS_CONTAINER" rustfs 60
  # Caller EXIT traps use this flag to avoid restarting an already recovered service.
  # shellcheck disable=SC2034
  RUSTFS_STOPPED=0
}

start_server_and_smoke() {
  local smoke_log=$1
  inspect_container "$SERVER_CONTAINER" teagramd
  if [[ "$INSPECT_STATE" != running ]]; then
    SERVER_STOPPED=1
    docker start "$SERVER_CONTAINER" >/dev/null || ops_fail 'could not start teagramd'
  fi
  for _ in {1..60}; do
    inspect_container "$SERVER_CONTAINER" teagramd
    [[ "$INSPECT_STATE" == running ]] && break
    sleep 1
  done
  [[ "$INSPECT_STATE" == running ]] || ops_fail 'teagramd did not enter the running state'
  # Caller EXIT traps use this flag to avoid restarting an already recovered service.
  # shellcheck disable=SC2034
  SERVER_STOPPED=0
  SERVER_IMAGE_ID=$INSPECT_IMAGE
  [[ "$INSPECT_REVISION" == "$LIVE_IMAGE_SHA" ]] || ops_fail 'teagramd restarted with a different image revision'
  if ! docker run --rm --network host --read-only --cap-drop ALL \
    --security-opt no-new-privileges --user 65532:65532 \
    --entrypoint /usr/local/bin/mtproto-smoke "$SERVER_IMAGE_ID" > "$smoke_log" 2>&1; then
    chmod 600 -- "$smoke_log" || true
    ops_fail "teagramd MTProto smoke failed; private diagnostic retained at $smoke_log"
  fi
  chmod 600 -- "$smoke_log" || ops_fail 'cannot protect MTProto smoke output'
}

latest_backup_manifest() {
  local -a manifests=()
  shopt -s nullglob
  manifests=("$EVIDENCE_ROOT"/pgdump-pre-migration-*.manifest)
  shopt -u nullglob
  [[ "${#manifests[@]}" -gt 0 ]] || ops_fail 'verified pre-migration database dump is unavailable'
  BACKUP_MANIFEST=${manifests[-1]}
  private_file "$BACKUP_MANIFEST" || ops_fail 'database backup manifest is unsafe'
  jq -e '.result == "isolated-restore-pass" and (.dump_path | type == "string") and (.dump_sha256 | test("^[0-9a-f]{64}$"))' \
    "$BACKUP_MANIFEST" >/dev/null || ops_fail 'database backup manifest is invalid'
  BACKUP_DUMP=$(jq -er '.dump_path' "$BACKUP_MANIFEST") || ops_fail 'database dump path is invalid'
  private_file "$BACKUP_DUMP" || ops_fail 'verified database dump is missing or unsafe'
  local expected actual
  expected=$(jq -er '.dump_sha256' "$BACKUP_MANIFEST") || ops_fail 'database dump digest is unavailable'
  actual=$(sha256sum -- "$BACKUP_DUMP") || ops_fail 'cannot hash database dump'
  [[ "${actual%% *}" == "$expected" ]] || ops_fail 'database dump no longer matches its verification manifest'
}

check_progress_journal() {
  local host_path="$REPORT_ROOT/${PROGRESS_PATH##*/}"
  private_file "$host_path" || ops_fail 'migration progress journal is missing or unsafe'
  jq -e 'select(.type == "header" and .schema == "teagram.blob-encryption-progress/v1" and .writes == "stopped")' \
    < <(head -n 1 -- "$host_path") >/dev/null || ops_fail 'migration progress journal does not match stopped writers'
}
