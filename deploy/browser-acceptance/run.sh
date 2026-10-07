#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077

readonly TARGET_HOST="telegram-server.tailaa4918.ts.net"
readonly APPROVED_QA_SCRIPT_SHA256="2ed4107f76a4b2ed6bdbd9933b7009f62d4dba4b2ecb8349835a181fcda6b10c"
readonly IMAGE_ARM64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:824f1a789072e648c62541c2cfa4479c4061a290d5c27766d67dc1dcbc19b321"
readonly IMAGE_AMD64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:cf0daee9b994042e011bc29f20cdff1a9f682a039b43fcd738f7d8a9d3bcd9d6"
readonly SERVED_MANIFEST_MAX_BYTES=16384
readonly PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
readonly COMPOSE=(docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml --project-name telegram-browser-acceptance)

RESULT_JSON=''
RESULT_STATUS=1
PROJECT_TOUCHED=0
MANIFEST_PATH=''
QA_SCRIPT_PATH=''
MODE=''

json_error() {
  printf '{"status":"error","code":"%s"}' "$1"
}

cleanup_and_report() {
  local exit_status=$?
  local cleanup_failed=0
  trap - EXIT
  if (( PROJECT_TOUCHED )); then
    if ! "${COMPOSE[@]}" down --rmi local >/dev/null 2>&1; then
      cleanup_failed=1
    fi
  fi
  if (( cleanup_failed )); then
    RESULT_JSON="$(json_error cleanup-failed)"
    RESULT_STATUS=1
  fi
  if [[ -z "$RESULT_JSON" ]]; then
    RESULT_JSON="$(json_error runtime-failed)"
    RESULT_STATUS=1
  fi
  printf '%s\n' "$RESULT_JSON"
  if (( RESULT_STATUS != 0 )); then
    exit "$RESULT_STATUS"
  fi
  exit "$exit_status"
}
trap cleanup_and_report EXIT

fail() {
  RESULT_JSON="$(json_error "$1")"
  RESULT_STATUS=1
  exit 1
}

usage() {
  fail arguments-invalid
}

parse_options() {
  MODE="${1:-}"
  shift || true
  [[ "$MODE" == "readiness" || "$MODE" == "blocked-control" || "$MODE" == "acceptance" ]] || usage
  while (($#)); do
    case "$1" in
      --manifest)
        (($# >= 2)) || usage
        [[ -z "$MANIFEST_PATH" ]] || usage
        MANIFEST_PATH="$2"
        shift 2
        ;;
      --script)
        (($# >= 2)) || usage
        [[ -z "$QA_SCRIPT_PATH" ]] || usage
        QA_SCRIPT_PATH="$2"
        shift 2
        ;;
      *) usage ;;
    esac
  done
  [[ -n "$MANIFEST_PATH" ]] || usage
  if [[ "$MODE" == "acceptance" ]]; then
    [[ -n "$QA_SCRIPT_PATH" ]] || usage
  elif [[ -n "$QA_SCRIPT_PATH" ]]; then
    usage
  fi
}

resolve_readonly_input() {
  local path="$1"
  [[ -f "$path" && ! -L "$path" ]] || return 1
  case "$path" in
    /*) ;;
    *) path="./$path" ;;
  esac
  local resolved
  resolved="$(realpath "$path" 2>/dev/null)" || return 1
  [[ "$resolved" == /* && "$resolved" != *:* ]] || return 1
  printf '%s' "$resolved"
}

validate_manifest() {
  local path="$1"
  command -v jq >/dev/null 2>&1 || return 1
  jq -e -s --arg host "$TARGET_HOST" '
    def sha256: type == "string" and test("^sha256:[0-9a-f]{64}$");
    length == 1 and
    (.[0] as $record |
      if (
        ($record | type == "object") and
        (($record | keys | sort) == ["archiveSha256", "contentDigest", "sourceCommit", "url"]) and
        ($record.sourceCommit | type == "string" and test("^[0-9a-f]{40}$")) and
        ($record.archiveSha256 | sha256) and
        ($record.contentDigest | sha256) and
        ($record.url | type == "string")
      ) then
        ($record.url | capture("^https://(?<host>[^/:?#]+)(?::(?<port>[0-9]+))?(?<path>/[^?#]*)[?]v=(?<commit>[0-9a-f]{40})$")) as $u |
        $u.host == $host and (($u.port // "443") == "443") and $u.commit == $record.sourceCommit
      else false end
    )
  ' "$path" >/dev/null 2>&1
}

curl_supports_streaming_size_limit() {
  local version major minor
  version="$(curl -q --version 2>/dev/null)" || return 1
  version="${version#curl }"
  version="${version%% *}"
  [[ "$version" =~ ^([0-9]+)\.([0-9]+)\. ]] || return 1
  major="${BASH_REMATCH[1]}"
  minor="${BASH_REMATCH[2]}"
  (( major > 8 || (major == 8 && minor >= 4) ))
}

validate_served_manifest() {
  local path="$1"
  local source_commit content_digest record_path base_path manifest_url served curl_status=0
  command -v curl >/dev/null 2>&1 || return 1
  curl_supports_streaming_size_limit || return 1
  source_commit="$(jq -r '.sourceCommit' "$path" 2>/dev/null)" || return 1
  content_digest="$(jq -r '.contentDigest' "$path" 2>/dev/null)" || return 1
  record_path="$(jq -r '.url | capture("^https://[^/]+(?<path>/[^?#]*)[?]v=[0-9a-f]{40}$").path' "$path" 2>/dev/null)" || return 1
  if [[ "$record_path" == */ ]]; then
    base_path="$record_path"
  else
    base_path="${record_path%/*}/"
  fi
  manifest_url="https://${TARGET_HOST}${base_path}mtproto-target.json?v=${source_commit}"
  served="$(curl -q --noproxy '*' --fail --silent --show-error --max-time 5 \
    --max-filesize "$((SERVED_MANIFEST_MAX_BYTES + 1))" "$manifest_url" 2>/dev/null)" || curl_status=$?
  if (( curl_status == 63 )); then
    return 2
  fi
  (( curl_status == 0 )) || return 1
  local LC_ALL=C
  (( ${#served} <= SERVED_MANIFEST_MAX_BYTES )) || return 2
  jq -e -s --arg host "$TARGET_HOST" --arg source "$source_commit" --arg digest "$content_digest" '
    length == 1 and
    (.[0] | type == "object" and
      (keys | sort) == ["artifactDigest", "endpoint", "fingerprint", "mode", "sourceCommit"] and
      .mode == "private" and .sourceCommit == $source and .artifactDigest == $digest and
      (.fingerprint | type == "string" and test("^[0-9a-f]{16}$")) and
      (.endpoint | type == "string" and test("^wss://" + ($host | gsub("\\."; "\\.")) + "/[^?#]*$")))
  ' <<<"$served" >/dev/null 2>&1 || return 2
}

validate_qa_script() {
  local path="$1"
  local digest
  digest="$(sha256sum -- "$path" 2>/dev/null)" || return 1
  digest="${digest%% *}"
  [[ "$digest" == "$APPROVED_QA_SCRIPT_SHA256" ]]
}

preflight_storage_and_arch() {
  local docker_root available_kib architecture
  docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null)" || return 1
  [[ "$docker_root" == /* && -d "$docker_root" ]] || return 1
  available_kib="$(df -Pk -- "$docker_root" 2>/dev/null | awk 'NR == 2 {print $4}')" || return 1
  [[ "$available_kib" =~ ^[0-9]+$ ]] || return 1
  (( available_kib >= 2097152 )) || return 2

  architecture="$(docker info --format '{{.Architecture}}' 2>/dev/null)" || return 1
  case "$architecture" in
    aarch64|arm64)
      export PLAYWRIGHT_PLATFORM="linux/arm64"
      export PLAYWRIGHT_IMAGE="$IMAGE_ARM64"
      ;;
    x86_64|amd64)
      export PLAYWRIGHT_PLATFORM="linux/amd64"
      export PLAYWRIGHT_IMAGE="$IMAGE_AMD64"
      ;;
    *) return 1 ;;
  esac
  export SECCOMP_PROFILE="$PROJECT_DIR/deploy/browser-acceptance/seccomp-browser.json"
  docker compose version >/dev/null 2>&1 || return 1
}

owner_controlled_directory() {
  local path="$1" owner mode uid
  [[ -d "$path" && ! -L "$path" ]] || return 1
  uid="$(id -u 2>/dev/null)" || return 1
  owner="$(stat -c '%u' -- "$path" 2>/dev/null)" || return 1
  [[ "$owner" == "$uid" ]] || return 1
  mode="$(stat -c '%a' -- "$path" 2>/dev/null)" || return 1
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
  (( (8#$mode & 0022) == 0 ))
}

acquire_runtime_lock() {
  local runtime_base home local_dir lock_dir
  if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
    runtime_base="$XDG_RUNTIME_DIR"
    [[ "$runtime_base" == /* ]] || return 1
    owner_controlled_directory "$runtime_base" || return 1
  else
    home="${HOME:-}"
    [[ "$home" == /* ]] || return 1
    owner_controlled_directory "$home" || return 1
    local_dir="$home/.local"
    if [[ -L "$local_dir" || ( -e "$local_dir" && ! -d "$local_dir" ) ]]; then
      return 1
    fi
    if [[ ! -d "$local_dir" ]]; then
      mkdir -m 755 -- "$local_dir" 2>/dev/null || return 1
    fi
    owner_controlled_directory "$local_dir" || return 1
    runtime_base="$local_dir/run"
    if [[ -L "$runtime_base" || ( -e "$runtime_base" && ! -d "$runtime_base" ) ]]; then
      return 1
    fi
    if [[ ! -d "$runtime_base" ]]; then
      mkdir -m 700 -- "$runtime_base" 2>/dev/null || return 1
    fi
    owner_controlled_directory "$runtime_base" || return 1
    [[ "$(stat -c '%a' -- "$runtime_base" 2>/dev/null)" == "700" ]] || return 1
  fi

  lock_dir="$runtime_base/telegram-browser-acceptance"
  if [[ -L "$lock_dir" || ( -e "$lock_dir" && ! -d "$lock_dir" ) ]]; then
    return 1
  fi
  if [[ ! -d "$lock_dir" ]]; then
    mkdir -m 700 -- "$lock_dir" 2>/dev/null ||
      [[ -d "$lock_dir" && ! -L "$lock_dir" ]] || return 1
  fi
  owner_controlled_directory "$lock_dir" || return 1
  [[ "$(stat -c '%a' -- "$lock_dir" 2>/dev/null)" == "700" ]] || return 1

  exec 9<"$lock_dir" || return 1
  flock -n 9 || return 2
}

prepare() {
  local root
  root="$(resolve_readonly_input "$MANIFEST_PATH")" || fail manifest-invalid
  MANIFEST_PATH="$root"
  validate_manifest "$MANIFEST_PATH" || fail manifest-invalid
  local manifest_status=0
  validate_served_manifest "$MANIFEST_PATH" || manifest_status=$?
  case "$manifest_status" in
    0) ;;
    1) fail manifest-fetch-failed ;;
    *) fail manifest-mismatch ;;
  esac
  if [[ -n "$QA_SCRIPT_PATH" ]]; then
    root="$(resolve_readonly_input "$QA_SCRIPT_PATH")" || fail qa-script-unapproved
    QA_SCRIPT_PATH="$root"
    validate_qa_script "$QA_SCRIPT_PATH" || fail qa-script-unapproved
  fi
  cd "$PROJECT_DIR"
  local lock_status=0
  acquire_runtime_lock || lock_status=$?
  case "$lock_status" in
    0) ;;
    2) fail runtime-busy ;;
    *) fail runtime-unavailable ;;
  esac
  local preflight_status=0
  preflight_storage_and_arch || preflight_status=$?
  case "$preflight_status" in
    0) ;;
    2) fail docker-storage-low ;;
    *) fail runtime-unavailable ;;
  esac
}

build_and_start_observer() {
  PROJECT_TOUCHED=1
  "${COMPOSE[@]}" build --pull browser >/dev/null 2>&1 || fail image-build-failed
  "${COMPOSE[@]}" up --detach --wait observer >/dev/null 2>&1 || fail observer-unhealthy
}

safe_runtime_error() {
  local output="$1"
  local code=''
  local error_pattern='^\{"status":"error","code":"([a-z-]+)"\}$'
  if [[ "$output" =~ $error_pattern ]]; then
    code="${BASH_REMATCH[1]}"
  fi
  case "$code" in
    arguments-invalid|asset-server-error|blocked-host-control-failed|browser-cleanup-failed|browser-unavailable|cleanup-failed|direct-egress-open|independent-capture-unavailable|manifest-fetch-failed|manifest-invalid|manifest-mismatch|observer-not-ready|observer-not-reset|observer-unhealthy|origin-not-ready|profile-storage-not-tmpfs|proxy-policy-invalid|qa-script-unapproved|release-record-invalid|runtime-adapter-unavailable|runtime-snapshot-unavailable|sandbox-proof-invalid|target-address-invalid|target-address-unavailable|websocket-not-ready)
      printf '%s' "$code"
      ;;
    *) printf '%s' runtime-failed ;;
  esac
}

normalize_wss_diagnostic() {
  local line="$1"
  local diagnostic targets handshakes target_match status
  local pattern='^\{"status":"error","code":"websocket-not-ready","wss_diagnostic":"(ambiguous|no-target|target-mismatch|no-handshake|handshake-not-101)","wss_targets":([012]),"wss_handshakes":([012]),"wss_target_match":(true|false),"wss_status":(0|[1-5][0-9]{2})\}$'
  [[ "$line" != *$'\n'* && "$line" =~ $pattern ]] || return 1
  diagnostic="${BASH_REMATCH[1]}"
  targets="${BASH_REMATCH[2]}"
  handshakes="${BASH_REMATCH[3]}"
  target_match="${BASH_REMATCH[4]}"
  status="${BASH_REMATCH[5]}"

  case "$diagnostic" in
    ambiguous)
      [[ "$target_match" == false && "$status" == 0 ]] || return 1
      ;;
    no-target)
      [[ "$targets" == 0 && "$handshakes" == 0 && "$target_match" == false && "$status" == 0 ]] || return 1
      ;;
    target-mismatch)
      [[ "$targets" == 1 || "$targets" == 2 ]] || return 1
      [[ "$target_match" == false ]] || return 1
      if [[ "$handshakes" == 0 ]]; then
        [[ "$status" == 0 ]] || return 1
      else
        [[ "$handshakes" == "$targets" && "$status" != 0 ]] || return 1
      fi
      ;;
    no-handshake)
      [[ ( "$targets" == 1 || "$targets" == 2 ) && "$handshakes" == 0 &&
        "$target_match" == true && "$status" == 0 ]] || return 1
      ;;
    handshake-not-101)
      [[ ( "$targets" == 1 || "$targets" == 2 ) && "$handshakes" == "$targets" &&
        "$target_match" == true && "$status" != 0 && "$status" != 101 ]] || return 1
      ;;
  esac

  printf '{"status":"error","code":"websocket-not-ready","wss_diagnostic":"%s","wss_targets":%s,"wss_handshakes":%s,"wss_target_match":%s,"wss_status":%s}' \
    "$diagnostic" "$targets" "$handshakes" "$target_match" "$status"
}

run_runtime() {
  local mode="$1"
  local output output_with_sentinel status normalized
  set +e
  output_with_sentinel="$(
    "${COMPOSE[@]}" run --no-deps --rm -T \
    --volume "$MANIFEST_PATH:/run/release-record.json:ro" \
      browser "$mode" --manifest /run/release-record.json 2>/dev/null
    status=$?
    printf '\001'
    exit "$status"
  )"
  status=$?
  set -e
  output="${output_with_sentinel%$'\001'}"
  if [[ "$output" != *$'\n' ]]; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  output="${output%$'\n'}"
  if [[ "$output" == *$'\n'* ]]; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  if [[ "$mode" == "readiness" ]] && normalized="$(normalize_wss_diagnostic "$output")"; then
    if (( status != 0 )); then
      printf '%s' "$normalized"
      return 1
    fi
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  if (( status != 0 )); then
    printf '%s' "$(json_error "$(safe_runtime_error "$output")")"
    return 1
  fi
  if [[ "$mode" == "readiness" && "$output" == '{"status":"ready","http_status":200,"asset_502_count":0,"browser_wss_status":101,"browser_wss_unique_targets":1,"observer_success_hosts":1,"observer_target_host":"telegram-server.tailaa4918.ts.net","telegram_org_attempts":0,"payloads_retained":0}' ]]; then
    printf '%s' "$output"
    return 0
  fi
  if [[ "$mode" == "blocked-control" && "$output" == '{"status":"blocked_expected","blocked_telegram_org_attempts":1,"telegram_org_upstream_connects":0,"payloads_retained":0}' ]]; then
    printf '%s' "$output"
    return 0
  fi
  printf '%s' "$(json_error "$(safe_runtime_error "$output")")"
  return 1
}

run_qa_script() {
  local output status code
  set +e
  output="$("${COMPOSE[@]}" run --no-deps --rm -T --entrypoint node \
    --volume "$MANIFEST_PATH:/run/release-record.json:ro" \
    --volume "$QA_SCRIPT_PATH:/run/approved-qa.mjs:ro" \
    browser /run/approved-qa.mjs --mode acceptance \
      --release-record /run/release-record.json \
      --runtime-adapter /opt/browser-acceptance/qa-runtime-adapter.mjs 2>/dev/null)"
  status=$?
  set -e
  if (( status != 0 )); then
    code="$(jq -er 'select(.schemaVersion == 1 and .ok == false) | .failure | select(type == "string" and test("^[a-z-]+$"))' <<<"$output" 2>/dev/null || true)"
    printf '%s' "$(json_error "$(safe_runtime_error "{\"status\":\"error\",\"code\":\"$code\"}")")"
    return 1
  fi
  if [[ "$output" == *$'\n'* ]] || ! jq -e 'type == "object" and .schemaVersion == 1 and .ok == true' <<<"$output" >/dev/null 2>&1; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  printf '%s' "$output"
}

main() {
  local mode output
  parse_options "$@"
  mode="$MODE"
  if [[ "$mode" != "acceptance" ]]; then
    exec </dev/null
  fi
  prepare
  build_and_start_observer
  if [[ "$mode" == "readiness" ]]; then
    if ! output="$(run_runtime blocked-control)"; then
      fail "$(safe_runtime_error "$output")"
    fi
    "${COMPOSE[@]}" restart observer >/dev/null 2>&1 || fail observer-unhealthy
    "${COMPOSE[@]}" up --detach --wait observer >/dev/null 2>&1 || fail observer-unhealthy
    if ! output="$(run_runtime readiness)"; then
      if normalized="$(normalize_wss_diagnostic "$output")"; then
        RESULT_JSON="$normalized"
        RESULT_STATUS=1
        exit 1
      fi
      fail "$(safe_runtime_error "$output")"
    fi
  elif [[ "$mode" == "blocked-control" ]]; then
    if ! output="$(run_runtime blocked-control)"; then
      fail "$(safe_runtime_error "$output")"
    fi
  else
    if ! output="$(run_qa_script)"; then
      fail "$(safe_runtime_error "$output")"
    fi
  fi
  RESULT_JSON="$output"
  RESULT_STATUS=0
}

main "$@"
exit 0
