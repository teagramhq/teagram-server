#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

# The original port state is either absent or the exact mapping owned by this
# operation. Foreign mappings, foreground sessions and Funnel are never changed.
serve_state() {
  local config
  config=$(tailscale serve status --json) || return 1
  jq -er '
    if type != "object" or ((.TCP // {}) | type) != "object" or
       ((.Foreground // {}) | type) != "object" or ((.AllowFunnel // {}) | type) != "object"
    then error("invalid Serve status") else . end
    | if any((.Foreground // {})[]; ((.TCP // {}) | has("2447"))) or
         any((.AllowFunnel // {}) | to_entries[]; (.key | endswith(":2447")) and .value == true)
      then "foreign"
      elif (.TCP // {} | has("2447") | not) then "absent"
      elif .TCP["2447"] == {"TCPForward":"127.0.0.1:2447", "ProxyProtocol":1} then "owned"
      else "foreign" end
  ' <<< "$config"
}

serve_apply() {
  local state marker=$1 previous
  state=$(serve_state) || return 1
  [[ "$state" != foreign ]] || { printf '%s\n' 'port 2447 has a foreign Serve or Funnel mapping' >&2; return 1; }
  if [[ -e "$marker" || -L "$marker" ]]; then
    [[ -f "$marker" && ! -L "$marker" && "$(stat -c '%u:%a' "$marker")" == "$(id -u):600" ]] || return 1
    IFS= read -r previous < "$marker" || return 1
    [[ "$previous" == absent || "$previous" == owned ]] || return 1
  else
    ( set -o noclobber; printf '%s\n' "$state" > "$marker" ) || return 1
    chmod 600 "$marker" || return 1
    sync || return 1
  fi
  if [[ "$state" == absent ]]; then
    tailscale serve --yes --bg --tcp=2447 --proxy-protocol=1 tcp://127.0.0.1:2447 >/dev/null || return 1
  fi
  [[ "$(serve_state)" == owned ]]
}

serve_verify() {
  [[ "$(serve_state)" == owned ]]
}

serve_rollback() {
  local marker=$1 previous state
  [[ -f "$marker" && ! -L "$marker" && "$(stat -c '%u:%a' "$marker")" == "$(id -u):600" ]] || return 1
  IFS= read -r previous < "$marker" || return 1
  state=$(serve_state) || return 1
  case "$previous:$state" in
    absent:owned) tailscale serve --yes --tcp=2447 off >/dev/null || return 1 ;;
    absent:absent|owned:owned) ;;
    *) printf '%s\n' 'Serve mapping changed since apply; refusing rollback' >&2; return 1 ;;
  esac
  [[ "$(serve_state)" == "$previous" ]] || return 1
  rm -- "$marker"
}

verify_host_listeners() {
  local ipv4 ipv6 output line address
  ipv4=$(tailscale ip -4) || return 1
  ipv6=$(tailscale ip -6) || return 1
  [[ "$ipv4" =~ ^100\.[0-9]+\.[0-9]+\.[0-9]+$ && "$ipv6" == fd7a:115c:a1e0:* ]] || return 1
  output=$(ss -Hltnp 'sport = :2447') || return 1
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    read -r _ _ _ address _ <<< "$line"
    case "$address" in
      127.0.0.1:2447) ;;
      "$ipv4:2447"|"[$ipv6]:2447")
        [[ "$line" == *'"tailscaled"'* ]] || return 1 ;;
      *) return 1 ;;
    esac
  done <<< "$output"
}

validate_relay_config() {
  jq -e '
    .services as $s
    | ([ $s | to_entries[] | select(any(.value.volumes[]?; .source == "bot_api_socket")) | .key ] | sort) == ["bot-api-ingress","bot-api-inner"]
    and ([ $s | to_entries[] | select((.value.networks // {}) | has("bot_api_ingress")) | .key ] == ["bot-api-ingress"])
    and (.networks.bot_api_ingress.internal // false) == false
    and $s["bot-api-inner"].network_mode == "service:teagramd"
    and ([ $s | to_entries[] | select((.value.network_mode // "") == "service:teagramd") | .key ] == ["bot-api-inner"])
    and ([ $s | to_entries[] | select((.value.network_mode // "") | startswith("container:")) ] | length) == 0
    and ($s["bot-api-inner"].ports // [] | length) == 0
    and ($s["bot-api-inner"].volumes | length) == 1 and ($s["bot-api-ingress"].volumes | length) == 1
    and $s["bot-api-inner"].command == ["-d", "-d", "-T120", "UNIX-LISTEN:/run/bot-api/relay.sock,fork,unlink-early,mode=0600,max-children=128", "TCP:127.0.0.1:2447"]
    and $s["bot-api-ingress"].command == ["-d", "-d", "-T120", "TCP-LISTEN:2448,bind=0.0.0.0,reuseaddr,fork,max-children=128", "UNIX-CONNECT:/run/bot-api/relay.sock"]
    and $s["bot-api-ingress"].ports == [{"mode":"ingress","host_ip":"127.0.0.1","target":2448,"published":"2447","protocol":"tcp"}]
    and ($s["bot-api-ingress"].networks | keys) == ["bot_api_ingress"]
    and all(["bot-api-inner","bot-api-ingress"][];
      $s[.].image == "alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e"
      and $s[.].entrypoint == null and $s[.].read_only == true and $s[.].cap_drop == ["ALL"])
  ' <<< "$1" >/dev/null
}

if [[ ${BASH_SOURCE[0]} != "$0" ]]; then
  return 0
fi

readonly REPO=/opt/teagram
readonly STATE_DIR=$REPO/.state
readonly ENV_FILE=$REPO/.env
readonly ENV_RECORD=$STATE_DIR/rollout-env
readonly PREVIOUS_ENV_FILE=$STATE_DIR/.env.main-3359-before-bot-api
readonly SERVE_MARKER=$STATE_DIR/bot-api-qa-serve-before
readonly SERVICE=teagram-teagramd-1
readonly RUNNER_DIR=/root/teagramd-rollout-runner
readonly RUNNER=$RUNNER_DIR/rollout-runner.sh
readonly BLOB_HELPER=$RUNNER_DIR/blob-transition-runner.py
readonly EXPECTED_ROOT_UID=0
readonly APP_LISTEN_ADDR=127.0.0.1:2447
readonly CONTAINER_PORT_FORMAT='{{index .Config.Labels "org.opencontainers.image.revision"}}'
readonly COMPOSE_FILES_FORMAT='{{index .Config.Labels "com.docker.compose.project.config_files"}}'
readonly ENV_FORMAT='{{range .Config.Env}}{{println .}}{{end}}'

fail() {
  printf 'Bot API QA operation rejected: %s\n' "$1" >&2
  return 1
}

private_file() {
  [[ -f "$1" && ! -L "$1" && "$(stat -c '%u:%a' -- "$1")" == "$EXPECTED_ROOT_UID:600" ]]
}

safe_state_dir() {
  local owner mode
  [[ -d "$STATE_DIR" && ! -L "$STATE_DIR" ]] || return 1
  owner=$(stat -c '%u' -- "$STATE_DIR") || return 1
  [[ "$owner" == "$EXPECTED_ROOT_UID" ]] || return 1
  mode=$(stat -c '%a' -- "$STATE_DIR") || return 1
  [[ "$mode" =~ ^[0-7]{3,4}$ ]] && (( (8#$mode & 0022) == 0 ))
}

file_sha256() {
  local output
  output=$(sha256sum -- "$1") || return 1
  printf '%s' "${output%% *}"
}

dotenv_count() {
  awk -v name="$2" 'index($0, "=") > 0 && substr($0, 1, index($0, "=") - 1) == name { count++ } END { print count + 0 }' "$1"
}

container_value() {
  local name=$1 output
  output=$(docker inspect --format "$ENV_FORMAT" "$SERVICE") || return 1
  awk -F= -v name="$name" '$1 == name { count++; value=substr($0, index($0, "=") + 1) } END { if (count > 1) exit 1; if (count == 1) printf "%s", value }' <<< "$output"
}

load_compose_authority() {
  local authority_json env_file_lines legacy_parked
  authority_json=$(python3 "$BLOB_HELPER" deployment-env \
    --checkout "$REPO" --state-dir "$STATE_DIR/blob-mode" \
    --report-root /root --lock-path /tmp/teagram-deploy.lock) || {
    fail 'live S3 authority or credential selection is not verified'
    return 1
  }
  env_file_lines=$(jq -er '
    select(.outcome == "s3-accepted") | .env_files |
    if type == "array" and length == 2 and all(.[]; type == "string" and length > 0)
    then .[] else error("invalid environment file list") end
  ' <<< "$authority_json") || { fail 'live S3 environment file selection is invalid'; return 1; }
  mapfile -t COMPOSE_ENV_FILES_ARRAY <<< "$env_file_lines"
  [[ "${#COMPOSE_ENV_FILES_ARRAY[@]}" == 2 && "${COMPOSE_ENV_FILES_ARRAY[0]}" == "$ENV_FILE" ]] || {
    fail 'live S3 environment file selection does not start with the verified .env'
    return 1
  }
  legacy_parked="$(dirname "$REPO")/telegram-server/.env.rustfs-parked"
  case "${COMPOSE_ENV_FILES_ARRAY[1]}" in
    "$REPO/.env.rustfs-parked"|"$legacy_parked"|/root/.env.rustfs-parked) ;;
    *) fail 'live S3 credential file path is not approved'; return 1 ;;
  esac
  export COMPOSE_FILE="$REPO/docker-compose.yml:$REPO/docker-compose.override.yml"
  export COMPOSE_ENV_FILES="${COMPOSE_ENV_FILES_ARRAY[0]},${COMPOSE_ENV_FILES_ARRAY[1]}"
}

TEMP_DIR=
TEMP_ENV=
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$TEMP_ENV" ]]; then
    case "$TEMP_ENV" in
      "$REPO"/.env.main-3359.????????) rm -f -- "$TEMP_ENV" || status=1 ;;
      *) status=1 ;;
    esac
  fi
  if [[ -n "$TEMP_DIR" ]]; then
    case "$TEMP_DIR" in
      /tmp/teagram-bot-api-qa.????????) rm -rf -- "$TEMP_DIR" || status=1 ;;
      *) status=1 ;;
    esac
  fi
  exit "$status"
}
trap cleanup EXIT

verify_base() {
  local record image_sha compose_selection
  if ! private_file "$ENV_FILE" || ! private_file "$ENV_RECORD" || ! private_file "$RUNNER" || ! private_file "$BLOB_HELPER"; then
    fail 'deployment files failed ownership or mode checks'; return 1
  fi
  record=$(cat -- "$ENV_RECORD") || return 1
  [[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || { fail 'invalid rollout record'; return 1; }
  [[ "$(file_sha256 "$ENV_FILE")" == "${BASH_REMATCH[1]}" ]] || { fail 'environment drift'; return 1; }
  [[ "${BASH_REMATCH[2]}" == "$OPS_SHA" ]] || { fail 'rollout record differs from operation'; return 1; }
  [[ "$(git -C "$REPO" rev-parse HEAD)" == "$OPS_SHA" ]] || { fail 'live checkout differs from operation'; return 1; }
  git -C "$REPO" diff --quiet "$OPS_SHA" -- docker-compose.yml || { fail 'live Compose file differs from reviewed commit'; return 1; }
  image_sha=$(docker inspect --format "$CONTAINER_PORT_FORMAT" "$SERVICE") || return 1
  [[ "$image_sha" == "$OPS_SHA" ]] || { fail 'live image differs from operation'; return 1; }
  compose_selection=$(docker inspect --format "$COMPOSE_FILES_FORMAT" "$SERVICE") || return 1
  [[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || { fail 'unexpected Compose selection'; return 1; }
  load_compose_authority
}

verify_relays() {
  local server_id inner_id ingress_id network_id config mounted ids attached
  local -a containers
  [[ "$(container_value TG_BOT_API_LISTEN_ADDR)" == "$APP_LISTEN_ADDR" ]] || { fail 'listener is not loopback'; return 1; }
  server_id=$(docker inspect --format '{{.Id}}' "$SERVICE") || return 1
  inner_id=$(docker compose --profile bot-api-qa ps -q bot-api-inner) || return 1
  ingress_id=$(docker compose --profile bot-api-qa ps -q bot-api-ingress) || return 1
  [[ "$inner_id" =~ ^[0-9a-f]{64}$ && "$ingress_id" =~ ^[0-9a-f]{64}$ ]] || { fail 'relays are missing'; return 1; }
  docker inspect "$inner_id" | jq -e --arg server "$server_id" '
    .[0] | .State.Running and .State.Health.Status == "healthy" and .Config.Entrypoint == ["socat"] and
    .Config.Image == "alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e" and
    .HostConfig.NetworkMode == ("container:" + $server)
    and .Config.Cmd == ["-d", "-d", "-T120", "UNIX-LISTEN:/run/bot-api/relay.sock,fork,unlink-early,mode=0600,max-children=128", "TCP:127.0.0.1:2447"]
    and (.HostConfig.PortBindings // {}) == {} and .HostConfig.ReadonlyRootfs
    and ([.Mounts[] | select(.Destination == "/run/bot-api" and .Name == "teagram_bot_api_socket")] | length) == 1
  ' >/dev/null || { fail 'inner relay validation failed'; return 1; }
  docker inspect "$ingress_id" | jq -e '
    .[0] | .State.Running and .State.Health.Status == "healthy" and .Config.Entrypoint == ["socat"] and
    .Config.Image == "alpine/socat:1.8.0.3@sha256:beb4a68d9e4fe6b0f21ea774a0fde6c31f580dde6368939ed70100c5385b015e" and .HostConfig.ReadonlyRootfs
    and .Config.Cmd == ["-d", "-d", "-T120", "TCP-LISTEN:2448,bind=0.0.0.0,reuseaddr,fork,max-children=128", "UNIX-CONNECT:/run/bot-api/relay.sock"]
    and .HostConfig.PortBindings["2448/tcp"] == [{"HostIp":"127.0.0.1","HostPort":"2447"}]
    and .NetworkSettings.Ports["2448/tcp"] == [{"HostIp":"127.0.0.1","HostPort":"2447"}]
    and ([.Mounts[] | select(.Destination == "/run/bot-api" and .Name == "teagram_bot_api_socket")] | length) == 1
    and (.NetworkSettings.Networks | keys) == ["teagram_bot_api_ingress"]
  ' >/dev/null || { fail 'ingress relay validation failed'; return 1; }
  ids=$(docker ps --all --quiet --no-trunc) || return 1
  mapfile -t containers <<< "$ids"
  attached=$(docker inspect "${containers[@]}" | jq -r --arg server "$server_id" '
    .[] | select(.HostConfig.NetworkMode == ("container:" + $server)) | .Id
  ') || return 1
  [[ "$attached" == "$inner_id" ]] || { fail 'another container shares the server network namespace'; return 1; }
  mounted=$(docker ps --all --quiet --no-trunc --filter volume=teagram_bot_api_socket) || return 1
  [[ "$(printf '%s\n' "$mounted" | LC_ALL=C sort)" == "$(printf '%s\n' "$inner_id" "$ingress_id" | LC_ALL=C sort)" ]] || { fail 'socket volume is mounted by another container'; return 1; }
  network_id=$(docker network inspect teagram_bot_api_ingress --format '{{.Id}}') || return 1
  docker network inspect "$network_id" | jq -e --arg id "$ingress_id" '
    (.[0].Internal | not) and (.[0].Containers | keys) == [$id]
  ' >/dev/null || { fail 'ingress network contains another container'; return 1; }
  verify_host_listeners || { fail 'unexpected host port listener'; return 1; }
  config=$(docker compose --profile bot-api-qa config --format json) || return 1
  validate_relay_config "$config" || { fail 'unsafe relay configuration'; return 1; }
}

MODE=${1:-apply}
[[ $# -le 1 ]] && [[ "$MODE" == apply || "$MODE" == verify || "$MODE" == rollback || "$MODE" == check ]] || { fail 'usage: bot-api-qa [apply|verify|rollback|check]'; exit 64; }
[[ $(id -u) == 0 && ${OPS_SHA:-} =~ ^[0-9a-f]{40}$ ]] || { fail 'operation requires root and a reviewed SHA'; exit 1; }
[[ "$(realpath -e -- "$REPO")" == "$REPO" ]] || { fail 'unsafe checkout'; exit 1; }
cd "$REPO"
safe_state_dir || { fail 'unsafe state directory'; exit 1; }
readonly LOCK_PATH=/tmp/teagram-deploy.lock
if [[ ! -e "$LOCK_PATH" && ! -L "$LOCK_PATH" ]]; then
  ( set -o noclobber; : > "$LOCK_PATH" ) || { fail 'cannot create deployment lock'; exit 1; }
fi
[[ -f "$LOCK_PATH" && ! -L "$LOCK_PATH" && "$(stat -c %u "$LOCK_PATH")" == "$EXPECTED_ROOT_UID" ]] || {
  fail 'deployment lock is unsafe'; exit 1
}
exec 9>>"$LOCK_PATH"
flock -x 9
if [[ "$MODE" == rollback ]]; then
  serve_rollback "$SERVE_MARKER" || { fail 'Serve rollback failed'; exit 1; }
  printf '%s\n' 'Serve state restored'
  exit 0
fi
verify_base
state=$(serve_state) || { fail 'cannot inspect Serve'; exit 1; }
[[ "$state" != foreign ]] || { fail 'port 2447 is already in use'; exit 1; }
preflight() {
  local config count help listeners ingress_id
  config=$(docker compose --profile bot-api-qa config --format json) || { fail 'cannot render relay configuration'; return 1; }
  validate_relay_config "$config" || { fail 'unsafe relay configuration'; return 1; }
  count=$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)
  if [[ "$count" != 0 ]] && { [[ "$count" != 1 ]] || ! grep -Fxq "TG_BOT_API_LISTEN_ADDR=$APP_LISTEN_ADDR" "$ENV_FILE"; }; then
    fail 'listener setting is unsafe'; return 1
  fi
  if [[ "$count" == 0 && ( -e "$PREVIOUS_ENV_FILE" || -L "$PREVIOUS_ENV_FILE" ) ]]; then
    if ! private_file "$PREVIOUS_ENV_FILE" || ! cmp -s -- "$ENV_FILE" "$PREVIOUS_ENV_FILE"; then
      fail 'previous environment backup is unsafe or differs'; return 1
    fi
  fi
  if [[ -e "$SERVE_MARKER" || -L "$SERVE_MARKER" ]]; then
    private_file "$SERVE_MARKER" || { fail 'unsafe Serve state record'; return 1; }
    grep -Exq 'absent|owned' "$SERVE_MARKER" || { fail 'invalid Serve state record'; return 1; }
  fi
  help=$(tailscale serve --help) || return 1
  [[ "$help" == *--proxy-protocol* ]] || { fail 'PROXY protocol is unsupported'; return 1; }
  verify_host_listeners || { fail 'unexpected host port listener'; return 1; }
  listeners=$(ss -Hltnp 'sport = :2447') || return 1
  if [[ "$listeners" == *'127.0.0.1:2447'* ]]; then
    ingress_id=$(docker compose --profile bot-api-qa ps -q bot-api-ingress) || return 1
    [[ "$ingress_id" =~ ^[0-9a-f]{64}$ ]] || { fail 'loopback port is occupied by another process'; return 1; }
    docker inspect "$ingress_id" | jq -e '.[0].HostConfig.PortBindings["2448/tcp"] == [{"HostIp":"127.0.0.1","HostPort":"2447"}]' >/dev/null || {
      fail 'loopback port is not owned by the reviewed ingress'; return 1;
    }
  fi
}
if [[ "$MODE" == check || "$MODE" == apply ]]; then
  preflight || exit 1
fi
if [[ "$MODE" == check ]]; then
  printf 'would_apply source_sha=%s serve=%s\n' "$OPS_SHA" "$state"
  exit 0
fi
if [[ "$MODE" == apply ]]; then
  if [[ "$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)" == 0 ]]; then
  record=$(cat -- "$ENV_RECORD") || { fail 'cannot read verified .env record'; exit 1; }
  [[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || {
    fail 'verified .env record is malformed'
    exit 1
  }
  recorded_env_sha=${BASH_REMATCH[1]}
  target_sha=${BASH_REMATCH[2]}
  [[ "$(file_sha256 "$ENV_FILE")" == "$recorded_env_sha" ]] || { fail 'live .env differs from the verified rollout record'; exit 1; }
  image_sha=$(docker inspect --format "$CONTAINER_PORT_FORMAT" "$SERVICE") || { fail 'cannot inspect live image revision'; exit 1; }
  branch=$(git -C "$REPO" branch --show-current) || { fail 'cannot read live checkout branch'; exit 1; }
  [[ "$branch" == main || -z "$branch" ]] || { fail 'live checkout is not on main'; exit 1; }
  [[ "$image_sha" =~ ^[0-9a-f]{40}$ && "$image_sha" == "$target_sha" && "$OPS_SHA" == "$target_sha" ]] || {
    fail 'live checkout, image, and reviewed operation revisions do not match'
    exit 1
  }
  compose_selection=$(docker inspect --format "$COMPOSE_FILES_FORMAT" "$SERVICE") || { fail 'cannot read live Compose selection'; exit 1; }
  [[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || {
    fail 'live Compose selection is not the reviewed teagram S3 stack'
    exit 1
  }
  TEMP_DIR=$(mktemp -d /tmp/teagram-bot-api-qa.XXXXXXXX) || { fail 'cannot allocate private rollout output directory'; exit 1; }
  [[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%u:%a' -- "$TEMP_DIR")" == "$EXPECTED_ROOT_UID:700" ]] || {
    fail 'private rollout output directory failed ownership or mode checks'
    exit 1
  }
  if [[ -e "$PREVIOUS_ENV_FILE" || -L "$PREVIOUS_ENV_FILE" ]]; then
    private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is unsafe'; exit 1; }
    cmp -s -- "$ENV_FILE" "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup differs from the verified live file'; exit 1; }
  else
    cp -- "$ENV_FILE" "$PREVIOUS_ENV_FILE" || { fail 'cannot save the pre-change .env'; exit 1; }
    if ! chmod 600 -- "$PREVIOUS_ENV_FILE" || ! sync; then
      fail 'cannot protect the pre-change .env backup'
      exit 1
    fi
  fi
  private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is unsafe'; exit 1; }
  [[ "$(file_sha256 "$PREVIOUS_ENV_FILE")" == "$recorded_env_sha" ]] || { fail 'pre-change .env backup is not the last verified file'; exit 1; }
  load_compose_authority || exit 1
  TEMP_ENV=$(mktemp "$REPO/.env.main-3359.XXXXXXXX") || { fail 'cannot stage the Bot API environment settings'; exit 1; }
  if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || \
     ! printf 'TG_BOT_API_LISTEN_ADDR=%s\n' "$APP_LISTEN_ADDR" >> "$TEMP_ENV" || \
     ! chmod 600 -- "$TEMP_ENV" || ! sync; then
    fail 'cannot prepare the named Bot API environment additions'
    exit 1
  fi
  expected_env_sha=$(file_sha256 "$TEMP_ENV") || { fail 'cannot hash the staged Bot API environment'; exit 1; }
  mv -f -- "$TEMP_ENV" "$ENV_FILE" || { fail 'cannot publish the named Bot API environment additions'; exit 1; }
  TEMP_ENV=
  sync || { fail 'cannot sync the named Bot API environment additions'; exit 1; }
  flock -u 9 || { fail 'cannot release the deployment lock for the rollout runner'; exit 1; }
  runner_status=0
  if env ROLLOUT_RUNNER_SOURCE_SHA="$OPS_SHA" \
      ROLLOUT_RUNNER_ADDED_ENV=TG_BOT_API_LISTEN_ADDR \
      ROLLOUT_RUNNER_PREVIOUS_ENV_FILE="$PREVIOUS_ENV_FILE" \
      bash "$RUNNER" apply "$target_sha" "$target_sha" > "$TEMP_DIR/runner.log" 2>&1; then
    :
  else
    runner_status=$?
  fi
  flock -x 9 || { fail 'cannot reacquire the deployment lock after the rollout'; exit 1; }
  if [[ "$runner_status" != 0 ]]; then
    failed_env_sha=$(file_sha256 "$ENV_FILE" 2>/dev/null || true)
    failed_record=$(cat -- "$ENV_RECORD" 2>/dev/null || true)
    if [[ "$failed_env_sha" == "$recorded_env_sha" && "$failed_record" == "$record" ]]; then
      fail "rollout runner failed with exit $runner_status and restored the verified environment" || true
    elif [[ "$failed_env_sha" == "$expected_env_sha" && "$failed_record" == "$record" && \
            "$(docker inspect --format "$CONTAINER_PORT_FORMAT" "$SERVICE" 2>/dev/null || true)" == "$target_sha" ]]; then
      TEMP_ENV=$(mktemp "$REPO/.env.main-3359.XXXXXXXX") || { fail 'rollout failed and the previous environment cannot be staged'; exit "$runner_status"; }
      if cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" && chmod 600 -- "$TEMP_ENV" && sync && \
         mv -f -- "$TEMP_ENV" "$ENV_FILE" && sync; then
        TEMP_ENV=
        fail "rollout runner failed with exit $runner_status; prior environment restored" || true
      else
        fail "rollout runner failed with exit $runner_status; environment restore is unverified" || true
      fi
    else
      fail "rollout runner failed with exit $runner_status; inspect the reviewed rollout evidence" || true
    fi
    exit "$runner_status"
  fi
  else
    if [[ "$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)" != 1 ]] || ! grep -Fxq "TG_BOT_API_LISTEN_ADDR=$APP_LISTEN_ADDR" "$ENV_FILE"; then
      fail 'listener setting is unsafe'; exit 1
    fi
  fi
  verify_base || exit 1
  docker compose --profile bot-api-qa up -d --no-deps --wait --wait-timeout 60 bot-api-inner bot-api-ingress </dev/null || { fail 'cannot start relays'; exit 1; }
  verify_relays || exit 1
  serve_apply "$SERVE_MARKER" || { fail 'cannot configure Serve'; exit 1; }
  verify_host_listeners || { fail 'unexpected host port listener'; exit 1; }
else
  verify_relays || exit 1
  serve_verify || { fail 'Serve mapping differs from the required mapping'; exit 1; }
fi
printf 'bot_api_qa=%s source_sha=%s\n' "$MODE" "$OPS_SHA"
