#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077

readonly TARGET_HOST="teagram.tailaa4918.ts.net"
readonly APPROVED_QA_SCRIPT_SHA256="5d638723fe7fe7e2b845fbcc3ccc1aadc0c085ef4a52a2f8e8c092275395333d"
readonly IMAGE_ARM64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:824f1a789072e648c62541c2cfa4479c4061a290d5c27766d67dc1dcbc19b321"
readonly IMAGE_AMD64="mcr.microsoft.com/playwright:v1.61.1-noble@sha256:cf0daee9b994042e011bc29f20cdff1a9f682a039b43fcd738f7d8a9d3bcd9d6"
readonly SERVED_MANIFEST_MAX_BYTES=16384
# teagram-web opens one client socket per account slot on an unauthenticated load.
readonly WSS_CONNECTION_CEILING=4
readonly PROJECT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
readonly COMPOSE=(docker compose --env-file /dev/null --project-directory deploy/browser-acceptance --file deploy/browser-acceptance/compose.yaml --project-name teagram-browser-acceptance)

RESULT_JSON=''
RESULT_STATUS=1
PROJECT_TOUCHED=0
MANIFEST_PATH=''
QA_SCRIPT_PATH=''
PROBE_CREDENTIAL_PATH='/etc/teagram-browser-acceptance/probe.pgpass'
PROBE_CONFIG_PATH='/etc/teagram-browser-acceptance/probe-config.json'
MODE=''

json_error() {
  printf '{"status":"error","code":"%s"}' "$1"
}

cleanup_and_report() {
  local exit_status=$?
  local cleanup_failed=0
  trap - EXIT
  if (( PROJECT_TOUCHED )); then
    if ! "${COMPOSE[@]}" down --rmi local </dev/null >/dev/null 2>&1; then
      cleanup_failed=1
    fi
  fi
  if (( cleanup_failed )); then
    local evidence_result qa_failure_code=''
    if evidence_result="$(jq -e -c 'select(.evidenceVersion == 2) | .status = "error" | .teardown = "failed"' \
      <<<"$RESULT_JSON" 2>/dev/null)"; then
      RESULT_JSON="$evidence_result"
    else
      qa_failure_code="$(jq -er -s '
        select(length == 1) | .[0] |
        select((keys | sort) == ["code", "qa_failure_code", "status"] and
          .status == "error" and (.code | type == "string" and test("^[a-z-]+$"))) |
        .qa_failure_code | select(type == "string" and test("^[a-z-]+$"))
      ' <<<"$RESULT_JSON" 2>/dev/null)" || qa_failure_code=''
      if [[ "$qa_failure_code" == unknown ]] || is_qa_failure_code "$qa_failure_code"; then
        RESULT_JSON="$(printf '{\"status\":\"error\",\"code\":\"cleanup-failed\",\"qa_failure_code\":\"%s\"}' "$qa_failure_code")"
      else
        RESULT_JSON="$(json_error cleanup-failed)"
      fi
    fi
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
  local credential_set=0 config_set=0
  MODE="${1:-}"
  shift || true
  case "$MODE" in
    readiness|blocked-control|acceptance|acceptance-preflight) ;;
    *) usage ;;
  esac
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
      --probe-credential)
        (($# >= 2 && credential_set == 0)) || usage
        [[ "$MODE" == acceptance* && -n "$2" ]] || usage
        PROBE_CREDENTIAL_PATH="$2"
        credential_set=1
        shift 2
        ;;
      --probe-config)
        (($# >= 2 && config_set == 0)) || usage
        [[ "$MODE" == acceptance* && -n "$2" ]] || usage
        PROBE_CONFIG_PATH="$2"
        config_set=1
        shift 2
        ;;
      *) usage ;;
    esac
  done
  [[ -n "$MANIFEST_PATH" ]] || usage
  if [[ "$MODE" == acceptance* ]]; then
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

  lock_dir="$runtime_base/teagram-browser-acceptance"
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
  ulimit -c 0 2>/dev/null || fail runtime-unavailable
  local lock_status=0
  acquire_runtime_lock || lock_status=$?
  case "$lock_status" in
    0) ;;
    2) fail runtime-busy ;;
    *) fail runtime-unavailable ;;
  esac
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
  "${COMPOSE[@]}" build --pull browser </dev/null >/dev/null 2>&1 || fail image-build-failed
  "${COMPOSE[@]}" up --detach --wait observer </dev/null >/dev/null 2>&1 || fail observer-unhealthy
}

safe_runtime_error() {
  local output="$1"
  local code=''
  local error_pattern='^\{"status":"error","code":"([a-z-]+)"\}$'
  if [[ "$output" =~ $error_pattern ]]; then
    code="${BASH_REMATCH[1]}"
  fi
  case "$code" in
    arguments-invalid|asset-server-error|binding-not-created|blocked-host-control-failed|browser-cleanup-failed|browser-unavailable|cleanup-failed|cleanup-unproven|database-unavailable|direct-egress-open|environment-unproven|fingerprint-unproven|independent-capture-unavailable|manifest-fetch-failed|manifest-invalid|manifest-mismatch|network-unproven|observer-not-ready|observer-not-reset|observer-unhealthy|origin-not-ready|profile-storage-not-tmpfs|proxy-policy-invalid|qa-script-unapproved|release-record-invalid|revision-unproven|runtime-adapter-unavailable|runtime-changed|runtime-snapshot-unavailable|runtime-unavailable|sandbox-proof-invalid|target-address-invalid|target-address-unavailable|websocket-not-ready)
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
  if [[ "$mode" == "readiness" && "$output" == '{"status":"ready","http_status":200,"asset_502_count":0,"browser_wss_status":101,"browser_wss_unique_targets":1,"observer_success_hosts":1,"observer_target_host":"teagram.tailaa4918.ts.net","telegram_org_attempts":0,"payloads_retained":0}' ]]; then
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

normalize_acceptance_result() {
  local output="$1"
  local source_commit
  source_commit="$(jq -r '.sourceCommit' "$MANIFEST_PATH" 2>/dev/null)" || return 1
  jq -e -c -s --arg host "$TARGET_HOST" --arg source "$source_commit" --argjson ceiling "$WSS_CONNECTION_CEILING" '
    def acceptance_result:
      if type != "object" then false
      else
        (keys | sort) == ["chatList", "chatListRows", "logout", "ok", "outcome", "phase", "qaFailureCode", "schemaVersion", "sourceCommit", "submission", "targetHost", "wssConnections", "wssEndpointMatched"] and
        (.schemaVersion | type == "number" and . == 3 and floor == 3) and
        (.ok | type == "boolean") and
        ((.phase | type) == "string") and
        (.phase as $phase | [
          "wss-not-opened", "wss-not-101", "req-pq", "req-dh-params", "set-client-dh",
          "init-connection", "password-challenge", "password-check", "signed-in"
        ] | index($phase) != null) and
        ((.outcome | type) == "string") and
        (.outcome as $outcome | [
          "ok", "timeout", "closed", "credential-rejected", "auth-key-unregistered", "flood", "internal", "other"
        ] | index($outcome) != null) and
        ((.logout | type) == "string") and
        (.logout as $logout | [
          "not-attempted", "ok", "timeout", "closed", "auth-key-unregistered", "flood", "internal", "other"
        ] | index($logout) != null) and
        .targetHost == $host and
        (.sourceCommit | type == "string" and test("^[0-9a-f]{40}$") and . == $source) and
        (.wssConnections | type == "number" and floor == . and . >= 0 and . <= $ceiling + 1) and
        (.wssEndpointMatched | type == "boolean") and
        (.submission as $submission | ["none", "username", "password"] | index($submission) != null) and
        (.chatList as $chat_list | ["not-reached", "missing", "rendered"] | index($chat_list) != null) and
        (.chatListRows | type == "number" and floor == . and . >= 0 and . <= 10) and
        (.qaFailureCode as $failure | [
          "arguments-invalid", "asset-server-error", "authentication-failed-once", "authentication-repeated",
          "blocked-host-control-failed", "broken-chat-list", "browser-closed", "browser-cleanup-failed",
          "browser-unavailable", "client-progress-unavailable", "code-card-shown", "configuration-not-safe",
          "credential-clear-failed", "credential-input-invalid", "direct-egress-open", "endpoint-mismatch",
          "manifest-fetch-failed", "manifest-invalid", "manifest-mismatch", "negative-asset-patch-missing",
          "negative-target-not-rejected", "observer-unhealthy", "origin-not-ready", "password-field-missing",
          "profile-storage-not-tmpfs", "proxy-policy-invalid", "release-record-invalid",
          "runtime-adapter-unavailable", "sandbox-proof-invalid", "signup-attempted", "signup-path-shown", "target-address-invalid",
          "target-address-unavailable", "telegram-org-attempt", "unexpected-failure", "username-field-missing",
          "websocket-not-ready", "none"
        ] | index($failure) != null) and
        (.ok == false or (
          .qaFailureCode == "none" and .submission == "password" and .chatList == "rendered" and
          .chatListRows >= 0 and .wssEndpointMatched and .phase == "signed-in" and
          .outcome == "ok" and .logout == "ok" and .wssConnections >= 1 and .wssConnections <= $ceiling
        )) and
        (.ok == true or .qaFailureCode != "none") and
        (.phase as $phase | (($phase != "password-check" and $phase != "signed-in") or .submission == "password")) and
        ((.phase == "wss-not-opened" and .wssConnections == 0) or
          (.phase != "wss-not-opened" and .wssConnections >= 1)) and
        (.phase == "signed-in" or .outcome != "ok") and
        ((.phase == "signed-in" and .logout != "not-attempted") or
          (.phase != "signed-in" and .logout == "not-attempted"))
      end;
    select(length == 1 and (.[0] | acceptance_result)) | .[0] |
    {qaOk: .ok, phase, outcome, logout, wssConnections: (.wssConnections | floor),
      wssEndpointMatched, submission, chatList, chatListRows, qaFailureCode}
  ' <<<"$output" 2>/dev/null
}

flat_json_object() {
  # Streaming retains duplicate keys before jq's object parser collapses them.
  jq -e -n --stream '
    [inputs | select(length == 2) | .[0]] as $paths |
    all($paths[]; length == 1 and (.[0] | type == "string")) and
    (($paths | length) == ($paths | unique | length))
  ' <<<"$1" >/dev/null 2>&1
}

# Prints the single bounded browser line without its newline, or fails.
read_browser_line() {
  local output_with_sentinel output node_status marker
  # This pipe terminates at the wrapper, never at the privileged probe.
  output_with_sentinel="$(
    if env -u NODE_OPTIONS -u NODE_DEBUG -u NODE_V8_COVERAGE node --input-type=module -e '
      let size = 0;
      let chunks = [];
      process.stdin.on("data", (chunk) => {
        size += chunk.length;
        if (size > 16384 || chunk.includes(0)) {
          chunks = []; process.exitCode = 1; process.stdin.destroy(); return;
        }
        chunks.push(chunk);
      });
      process.stdin.on("end", () => { if (!process.exitCode) process.stdout.write(Buffer.concat(chunks)); });
      process.stdin.on("error", () => { process.exitCode = 1; });
    ' 2>/dev/null; then node_status=0; else node_status=$?; fi
    printf '\001%s' "$node_status"
  )"
  marker="${output_with_sentinel##*$'\001'}"
  output="${output_with_sentinel%$'\001'*}"
  [[ "$marker" == 0 ]] || return 1
  local LC_ALL=C
  (( ${#output} <= 16384 )) || return 1
  if [[ "$output" == *$'\n' ]]; then
    output="${output%$'\n'}"
  fi
  [[ "$output" != *$'\n'* ]] || return 1
  printf '%s' "$output"
}

normalize_browser_stream() {
  local output normalized
  local unknown='{"qaOk":false,"phase":"unknown","outcome":"unknown","logout":"unknown","wssConnections":0,"wssEndpointMatched":false,"submission":"unknown","chatList":"unknown","chatListRows":0,"qaFailureCode":"unknown"}'
  if ! output="$(read_browser_line)"; then
    jq -c --arg result invalid '. + {qaResult: $result}' <<<"$unknown"
    return 0
  fi
  if [[ -z "$output" ]]; then
    jq -c --arg result absent '. + {qaResult: $result}' <<<"$unknown"
    return 0
  fi
  if flat_json_object "$output" && normalized="$(normalize_acceptance_result "$output")"; then
    jq -c --arg result valid '. + {qaResult: $result}' <<<"$normalized"
    return 0
  fi
  jq -c --arg result invalid '. + {qaResult: $result}' <<<"$unknown"
}

# QA preflight failures carry only an allow-listed code. Malformed or missing
# child output is reduced to the fixed unknown code.
is_qa_failure_code() {
  case "$1" in
    arguments-invalid|asset-server-error|authentication-failed-once|authentication-repeated|blocked-host-control-failed|broken-chat-list|browser-closed|browser-cleanup-failed|browser-unavailable|client-progress-unavailable|code-card-shown|configuration-not-safe|credential-clear-failed|credential-input-invalid|direct-egress-open|endpoint-mismatch|manifest-fetch-failed|manifest-invalid|manifest-mismatch|negative-asset-patch-missing|negative-target-not-rejected|observer-unhealthy|origin-not-ready|password-field-missing|profile-storage-not-tmpfs|proxy-policy-invalid|release-record-invalid|runtime-adapter-unavailable|sandbox-proof-invalid|signup-path-shown|target-address-invalid|target-address-unavailable|telegram-org-attempt|unexpected-failure|username-field-missing|websocket-not-ready)
      return 0
      ;;
    *) return 1 ;;
  esac
}

normalize_preflight_stream() {
  local output code started=''
  local pattern='^\{"status":"error","code":"([a-z-]+)"\}$'
  if ! IFS= read -r started || [[ "$started" != 'qa child started' ]]; then
    printf '%s\n' '{"qaPreflight":"not-run"}'
    return 0
  fi
  if output="$(read_browser_line)"; then
    if [[ "$output" == 'browser acceptance preflight ready' ]]; then
      printf '%s\n' '{"qaPreflight":"ready"}'
      return 0
    fi
    if [[ "$output" =~ $pattern ]]; then
      code="${BASH_REMATCH[1]}"
      is_qa_failure_code "$code" || code=unknown
      printf '{"qaPreflight":"failed","qaFailureCode":"%s"}\n' "$code"
      return 0
    fi
  fi
  printf '%s\n' '{"qaPreflight":"failed","qaFailureCode":"unknown"}'
}

normalize_probe_result() {
  jq -e -c -s --argjson exit_status "$2" --arg mode "$3" '
    def proof:
      if type != "object" then false else
        (keys | sort) == [
          "accountEvidence", "accountsUnchanged", "baselineIntact", "bindingAdditions", "bindingEvidence",
          "bindingRemovals", "boundAfter", "boundBefore",
          "browserExitOk", "captureAllowedConnects", "captureBlocked", "captureOther", "captureTelegramSuffix",
          "captureUpstreamFailures", "captureWindow", "cleanupProven", "code", "containersStable",
          "fingerprintMatched", "loginCodesDisabled", "networkIsolated", "newBindingProven", "ok",
          "pendingAfter", "pendingBefore", "registrationMode", "revisionMatched", "schemaVersion",
          "serverSourceCommit", "topologyIsolated", "witnessInWindow"
        ] and .schemaVersion == 4 and
        (.serverSourceCommit | type == "string" and (. == "" or test("^[0-9a-f]{40}$"))) and
        (if .revisionMatched then (.serverSourceCommit | test("^[0-9a-f]{40}$")) else .serverSourceCommit == "" end) and
        (to_entries | all(.[] | select(.key | IN(
          "ok", "accountsUnchanged", "loginCodesDisabled", "revisionMatched", "containersStable",
          "networkIsolated", "fingerprintMatched", "newBindingProven", "cleanupProven", "browserExitOk",
          "witnessInWindow", "baselineIntact", "topologyIsolated"
        )); .value | type == "boolean")) and
        ([.boundBefore, .boundAfter, .pendingBefore, .pendingAfter,
          .bindingAdditions, .bindingRemovals, .captureAllowedConnects, .captureTelegramSuffix,
          .captureOther, .captureBlocked, .captureUpstreamFailures] | all(.[];
          type == "number" and floor == . and . >= 0 and . <= 100)) and
        (.bindingAdditions <= 3 and .bindingRemovals <= 3 and
          .captureAllowedConnects <= 99 and .captureTelegramSuffix <= 99 and .captureOther <= 99 and
          .captureBlocked <= 99 and .captureUpstreamFailures <= 99) and
        (.bindingEvidence as $evidence | ["not-reached", "unavailable", "epoch-changed", "complete"] | index($evidence) != null) and
        (.accountEvidence as $evidence | ["not-reached", "unavailable", "complete"] | index($evidence) != null) and
        (.registrationMode as $mode | ["unknown", "closed", "open", "changed"] | index($mode) != null) and
        (.accountsUnchanged == false or .accountEvidence == "complete") and
        (.captureWindow as $window | ["not-reached", "invalid", "complete"] | index($window) != null) and
        (.bindingEvidence == "complete" or (
          .bindingAdditions == 0 and .bindingRemovals == 0 and (.witnessInWindow | not) and (.baselineIntact | not)
        )) and
        (.captureWindow == "complete" or (
          .captureAllowedConnects == 0 and .captureTelegramSuffix == 0 and .captureOther == 0 and
          .captureBlocked == 0 and .captureUpstreamFailures == 0
        )) and
        (.code | type == "string") and
        (.code as $code | ["ok", "arguments-invalid", "runtime-unavailable", "environment-unproven",
          "revision-unproven", "runtime-changed", "network-unproven", "fingerprint-unproven",
          "database-unavailable", "cleanup-unproven", "binding-not-created", "browser-failed",
          "account-created", "account-unproven"] | index($code) != null) and
        (if .ok then
          $exit_status == 0 and .code == "ok" and (.registrationMode == "closed" or .registrationMode == "open") and
          .accountEvidence == "complete" and
          (if $mode == "preflight" then (.accountsUnchanged | not) else .accountsUnchanged end) and .loginCodesDisabled and
          .revisionMatched and (.serverSourceCommit | test("^[0-9a-f]{40}$")) and
          .containersStable and .networkIsolated and .fingerprintMatched and .browserExitOk and .topologyIsolated and
          (if $mode == "preflight" then
            .bindingEvidence == "not-reached" and .bindingAdditions == 0 and .bindingRemovals == 0 and
            (.witnessInWindow | not) and (.baselineIntact | not) and .captureWindow == "complete"
          else
            .bindingEvidence == "complete" and .bindingAdditions == 1 and .bindingRemovals == 1 and
            .witnessInWindow and .baselineIntact and .newBindingProven and .cleanupProven and
            .captureWindow == "complete"
          end)
        else $exit_status != 0 and .code != "ok" end) and
        (.code != "binding-not-created" or (
          .bindingEvidence == "complete" and .bindingAdditions == 0 and .bindingRemovals == 0 and
          .baselineIntact and .pendingAfter <= .pendingBefore and (.browserExitOk | not) and (.ok | not)
        )) and
        (.code != "browser-failed" or (
          (.browserExitOk | not) and
          (if $mode == "preflight" then
            (.cleanupProven | not) and (.newBindingProven | not) and .bindingEvidence == "not-reached"
          else
            .cleanupProven and .newBindingProven and .baselineIntact and .bindingEvidence == "complete"
          end)
        )) and
        (.cleanupProven == false or (.newBindingProven and .baselineIntact and .pendingAfter <= .pendingBefore)) and
        (.code != "account-created" or
          (.accountEvidence == "complete" and (.accountsUnchanged | not) and (.ok | not))) and
        (.code != "account-unproven" or
          ((.accountEvidence == "unavailable" or .accountEvidence == "complete") and
           (.accountsUnchanged | not) and (.ok | not))) and
        (if $mode == "preflight" then
          # Preflight takes one binding cut and never proves a new binding or cleanup.
          (.newBindingProven | not) and (.cleanupProven | not) and .boundAfter == 0 and .pendingAfter == 0 and
          .bindingEvidence == "not-reached" and .bindingAdditions == 0 and .bindingRemovals == 0 and
          (.witnessInWindow | not) and (.baselineIntact | not) and
          (.code != "browser-failed" or (.browserExitOk | not))
        else
          ((.ok | not) or (.newBindingProven and .cleanupProven)) and
          (.code != "browser-failed" or (.cleanupProven and .newBindingProven and (.browserExitOk | not)))
        end)
      end;
    select(length == 2) | map(select(has("registrationMode"))) |
    select(length == 1 and (.[0] | proof)) | .[0]
  ' <<<"$1" 2>/dev/null
}

run_qa_script() {
  local output proof browser_output normalized status code qa_result qa_ok web_commit evidence_status
  local -a lines
  set +e
  output="$(
    { env -u NODE_OPTIONS -u NODE_DEBUG -u NODE_V8_COVERAGE \
        -u ROLLOUT_RUNNER_TEST_MODE -u ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT \
        node "$PROJECT_DIR/deploy/browser-acceptance/host-probe.mjs" --mode acceptance \
          --manifest "$MANIFEST_PATH" --script "$QA_SCRIPT_PATH" \
          --credential "$PROBE_CREDENTIAL_PATH" --config "$PROBE_CONFIG_PATH" \
          3>&1 1>&4 2>/dev/null | normalize_browser_stream; } 4>&1
  )"
  status=$?
  set -e
  mapfile -t lines <<<"$output"
  if (( ${#lines[@]} != 2 )); then printf '%s' "$(json_error runtime-failed)"; return 1; fi
  for line in "${lines[@]}"; do
    if ! flat_json_object "$line"; then printf '%s' "$(json_error runtime-failed)"; return 1; fi
  done
  if ! proof="$(normalize_probe_result "$output" "$status" acceptance)"; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  code="$(jq -r '.code' <<<"$proof")"
  browser_output="$(jq -c -s 'map(select(has("registrationMode") | not)) | select(length == 1) | .[0]' <<<"$output" 2>/dev/null)" || return 1
  qa_result="$(jq -r '.qaResult' <<<"$browser_output")" || return 1
  qa_ok="$(jq -r '.qaOk' <<<"$browser_output")" || return 1
  web_commit="$(jq -r '.sourceCommit' "$MANIFEST_PATH" 2>/dev/null)" || return 1
  if [[ "$code" == ok && "$status" == 0 && "$qa_result" == valid && "$qa_ok" == true && "$(jq -r '.accountsUnchanged' <<<"$proof")" == true ]]; then
    evidence_status=accepted
  else
    evidence_status=error
  fi
  normalized="$(jq -c -n --arg status "$evidence_status" --arg teardown ok \
    --arg host "$TARGET_HOST" --arg source "$web_commit" --argjson proof "$proof" --argjson qa "$browser_output" '
    {
      evidenceVersion: 2, status: $status, code: $proof.code, teardown: $teardown,
      targetHost: $host, sourceCommit: $source, serverSourceCommit: $proof.serverSourceCommit,
      registrationMode: $proof.registrationMode, accountEvidence: $proof.accountEvidence,
      accountsUnchanged: $proof.accountsUnchanged, loginCodesDisabled: $proof.loginCodesDisabled,
      revisionMatched: $proof.revisionMatched, containersStable: $proof.containersStable,
      networkIsolated: $proof.networkIsolated, fingerprintMatched: $proof.fingerprintMatched,
      newBindingProven: $proof.newBindingProven, cleanupProven: $proof.cleanupProven,
      browserExitOk: $proof.browserExitOk, boundBefore: $proof.boundBefore, boundAfter: $proof.boundAfter,
      pendingBefore: $proof.pendingBefore, pendingAfter: $proof.pendingAfter,
      bindingEvidence: $proof.bindingEvidence, bindingAdditions: $proof.bindingAdditions,
      bindingRemovals: $proof.bindingRemovals, witnessInWindow: $proof.witnessInWindow,
      baselineIntact: $proof.baselineIntact, topologyIsolated: $proof.topologyIsolated,
      captureWindow: $proof.captureWindow, captureAllowedConnects: $proof.captureAllowedConnects,
      captureTelegramSuffix: $proof.captureTelegramSuffix, captureOther: $proof.captureOther,
      captureBlocked: $proof.captureBlocked, captureUpstreamFailures: $proof.captureUpstreamFailures,
      qaResult: $qa.qaResult, phase: $qa.phase, outcome: $qa.outcome, logout: $qa.logout,
      wssConnections: $qa.wssConnections, wssEndpointMatched: $qa.wssEndpointMatched,
      submission: $qa.submission, chatList: $qa.chatList, chatListRows: $qa.chatListRows,
      qaFailureCode: $qa.qaFailureCode
    }
  ' 2>/dev/null)" || return 1
  printf '%s' "$normalized"
  return "$status"
}

# Same probe path as acceptance, stopping at the QA preflight. Stdin is already
# /dev/null and the probe never runs an interactive exec in this mode.
run_acceptance_preflight() {
  local output proof status code line web_commit server_commit qa_failure_code
  local -a lines
  set +e
  output="$(
    { env -u NODE_OPTIONS -u NODE_DEBUG -u NODE_V8_COVERAGE \
        -u ROLLOUT_RUNNER_TEST_MODE -u ROLLOUT_RUNNER_TEST_EVIDENCE_ROOT \
        node "$PROJECT_DIR/deploy/browser-acceptance/host-probe.mjs" --mode preflight \
          --manifest "$MANIFEST_PATH" --script "$QA_SCRIPT_PATH" \
          --credential "$PROBE_CREDENTIAL_PATH" --config "$PROBE_CONFIG_PATH" \
          3>&1 1>&4 2>/dev/null </dev/null | normalize_preflight_stream; } 4>&1
  )"
  status=$?
  set -e
  mapfile -t lines <<<"$output"
  if (( ${#lines[@]} != 2 )); then printf '%s' "$(json_error runtime-failed)"; return 1; fi
  for line in "${lines[@]}"; do
    if ! flat_json_object "$line"; then printf '%s' "$(json_error runtime-failed)"; return 1; fi
  done
  if ! proof="$(normalize_probe_result "$output" "$status" preflight)"; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  code="$(jq -r '.code' <<<"$proof")"
  line="$(jq -c -s 'map(select(has("registrationMode") | not)) | select(length == 1) | .[0]' <<<"$output" 2>/dev/null)" || line=''
  if [[ "$code" != ok ]]; then
    qa_failure_code="$(jq -er -s '
      select(length == 1) | .[0] |
      select((keys | sort) == ["qaFailureCode", "qaPreflight"] and .qaPreflight == "failed") |
      .qaFailureCode | select(type == "string" and test("^[a-z-]+$"))
    ' <<<"$line" 2>/dev/null)" || qa_failure_code=''
    if [[ -n "$qa_failure_code" && "$qa_failure_code" != unknown ]] && ! is_qa_failure_code "$qa_failure_code"; then
      qa_failure_code=unknown
    fi
    if [[ "$code" == browser-failed && -z "$qa_failure_code" ]]; then
      qa_failure_code=unknown
    fi
    if [[ -n "$qa_failure_code" ]]; then
      printf '{"status":"error","code":"%s","qa_failure_code":"%s"}' "$code" "$qa_failure_code"
    else
      printf '%s' "$(json_error "$code")"
    fi
    return 1
  fi
  if [[ "$line" != '{"qaPreflight":"ready"}' ]]; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  web_commit="$(jq -r '.sourceCommit' "$MANIFEST_PATH" 2>/dev/null)" || web_commit=''
  server_commit="$(jq -r '.serverSourceCommit' <<<"$proof")"
  if [[ ! "$web_commit" =~ ^[0-9a-f]{40}$ || ! "$server_commit" =~ ^[0-9a-f]{40}$ ]]; then
    printf '%s' "$(json_error runtime-failed)"
    return 1
  fi
  printf '{"status":"acceptance-ready","web_source_commit":"%s","server_source_commit":"%s","registration_mode":"%s","login_codes_disabled":true,"server_revision_proven":true,"runtime_stable":true,"network_isolated":true,"fingerprint_matched":true,"binding_evidence_complete":true,"account_evidence_complete":true,"qa_preflight_ready":true}' \
    "$web_commit" "$server_commit" "$(jq -r '.registrationMode' <<<"$proof")"
}

main() {
  local mode normalized output run_status
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
  elif [[ "$mode" == "acceptance-preflight" ]]; then
    if ! output="$(run_acceptance_preflight)"; then
      RESULT_JSON="$output"
      RESULT_STATUS=1
      exit 1
    fi
  else
    set +e
    output="$(run_qa_script)"
    run_status=$?
    set -e
    RESULT_JSON="$output"
    RESULT_STATUS="$run_status"
    if (( run_status != 0 )); then exit "$run_status"; fi
  fi
  RESULT_JSON="$output"
  RESULT_STATUS=0
}

main "$@"
exit 0
