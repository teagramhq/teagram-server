#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

readonly REPO=/opt/teagram
readonly STATE_DIR=$REPO/.state
readonly ENV_FILE=$REPO/.env
readonly ENV_RECORD=$STATE_DIR/rollout-env
readonly PREVIOUS_ENV_FILE=$STATE_DIR/.env.main-2825-before-bot-api
readonly BOT_ID_FILE=$STATE_DIR/bot-api-qa-id
readonly SERVICE=teagram-teagramd-1
readonly RUNNER_DIR=/root/teagramd-rollout-runner
readonly RUNNER=$RUNNER_DIR/rollout-runner.sh
readonly BLOB_HELPER=$RUNNER_DIR/blob-transition-runner.py
readonly EXPECTED_ROOT_UID=0
readonly TAILNET_HOST_IP=100.124.236.66
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

verify_live_configuration() {
  local record current_env_sha recorded_env_sha recorded_source_sha image_sha branch compose_selection
  local listen_value host_binding sidecar_id compose_env server_id

  private_file "$ENV_FILE" || { fail 'live .env is not a root-owned mode-0600 regular file'; return 1; }
  private_file "$ENV_RECORD" || { fail 'verified .env record is not a root-owned mode-0600 regular file'; return 1; }
  private_file "$PREVIOUS_ENV_FILE" || { fail 'pre-change .env backup is unsafe or missing'; return 1; }
  private_file "$RUNNER" || { fail 'reviewed rollout runner is unavailable or unsafe'; return 1; }
  private_file "$BLOB_HELPER" || { fail 'reviewed blob authority helper is unavailable or unsafe'; return 1; }

  [[ "$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)" == 1 && \
     "$(dotenv_count "$ENV_FILE" TEAGRAM_BOT_API_HOST_IP)" == 1 ]] || {
    fail 'Bot API listener assignments are missing or duplicated'
    return 1
  }
  grep -Fxq 'TG_BOT_API_LISTEN_ADDR=127.0.0.1:2447' "$ENV_FILE" || {
    fail 'Bot API must bind to the teagramd loopback address'
    return 1
  }
  grep -Fxq "TEAGRAM_BOT_API_HOST_IP=$TAILNET_HOST_IP" "$ENV_FILE" || {
    fail 'published Bot API host address is not the teagram tailnet IP'
    return 1
  }

  record=$(cat -- "$ENV_RECORD") || { fail 'cannot read verified .env record'; return 1; }
  [[ "$record" =~ ^env_sha256=([0-9a-f]{64})\ source_sha=([0-9a-f]{40})$ ]] || {
    fail 'verified .env record is malformed'
    return 1
  }
  recorded_env_sha=${BASH_REMATCH[1]}
  recorded_source_sha=${BASH_REMATCH[2]}
  current_env_sha=$(file_sha256 "$ENV_FILE") || { fail 'cannot hash live .env'; return 1; }
  [[ "$current_env_sha" == "$recorded_env_sha" ]] || { fail 'live .env differs from the verified rollout record'; return 1; }

  branch=$(git -C "$REPO" branch --show-current) || { fail 'cannot read live checkout branch'; return 1; }
  [[ "$branch" == main || -z "$branch" ]] || { fail 'live checkout is not on main'; return 1; }
  image_sha=$(docker inspect --format "$CONTAINER_PORT_FORMAT" "$SERVICE") || { fail 'cannot inspect live image revision'; return 1; }
  [[ "$image_sha" =~ ^[0-9a-f]{40}$ && "$image_sha" == "$OPS_SHA" && "$recorded_source_sha" == "$OPS_SHA" ]] || {
    fail 'operation SHA, live image, and verified environment record do not match'
    return 1
  }
  compose_selection=$(docker inspect --format "$COMPOSE_FILES_FORMAT" "$SERVICE") || {
    fail 'cannot read live Compose file selection'
    return 1
  }
  [[ "$compose_selection" == "$REPO/docker-compose.yml,$REPO/docker-compose.override.yml" ]] || {
    fail 'live Compose selection is not the reviewed teagram S3 stack'
    return 1
  }
  [[ "$(docker inspect --format '{{.State.Status}}' "$SERVICE")" == running ]] || {
    fail 'teagramd is not running'
    return 1
  }
  listen_value=$(container_value TG_BOT_API_LISTEN_ADDR) || {
    fail 'cannot inspect Bot API listener environment'
    return 1
  }
  [[ "$listen_value" == "$APP_LISTEN_ADDR" ]] || { fail 'live Bot API listener is not bound to loopback'; return 1; }

  host_binding=$(docker inspect "$SERVICE" | jq -er '
    .[0].HostConfig.PortBindings["2448/tcp"] as $bindings
    | if ($bindings | type) == "array" and ($bindings | length) == 1 and
         $bindings[0].HostIp == "100.124.236.66" and $bindings[0].HostPort == "2447"
      then "verified" else error("unexpected Bot API host binding") end
  ') || { fail 'published Bot API port is not restricted to the tailnet IP'; return 1; }
  [[ "$host_binding" == verified ]] || return 1

  mapfile -t sidecar_ids < <(docker ps --quiet --no-trunc \
    --filter label=com.docker.compose.project=teagram \
    --filter label=com.docker.compose.service=bot-api-forwarder)
  [[ "${#sidecar_ids[@]}" == 1 && "${sidecar_ids[0]}" =~ ^[0-9a-f]{64}$ ]] || {
    fail 'Bot API forwarder container is missing or duplicated'
    return 1
  }
  sidecar_id=${sidecar_ids[0]}
  server_id=$(docker inspect --format '{{.Id}}' "$SERVICE") || { fail 'cannot inspect teagramd container identity'; return 1; }
  docker inspect "$sidecar_id" | jq -e --arg server_id "$server_id" --arg listener "$listen_value" '
    .[0] as $sidecar
    | $sidecar.State.Status == "running" and $sidecar.State.Health.Status == "healthy" and
      $sidecar.HostConfig.NetworkMode == ("container:" + $server_id) and
      $sidecar.Config.Labels["com.docker.compose.service"] == "bot-api-forwarder" and
      (($sidecar.Config.Env // []) | map(select(startswith("TG_BOT_API_LISTEN_ADDR=")))) == ["TG_BOT_API_LISTEN_ADDR=" + $listener] and
      $sidecar.Config.Image == "teagramd:local" and
      $sidecar.Config.Entrypoint == ["/usr/local/bin/bot-api-forwarder"] and
      $sidecar.HostConfig.ReadonlyRootfs == true and
      (($sidecar.HostConfig.PortBindings // {}) == {}) and
      (($sidecar.Mounts // []) == [])
  ' >/dev/null || { fail 'Bot API forwarder is not healthy in teagramd network namespace'; return 1; }

  load_compose_authority || return 1
  compose_env=$(docker compose config --format json </dev/null | jq -er '.services.teagramd.environment.TG_BOT_API_LISTEN_ADDR') || {
    fail 'cannot resolve Bot API Compose listener configuration'
    return 1
  }
  [[ "$compose_env" == "$APP_LISTEN_ADDR" ]] || { fail 'resolved Compose listener differs from the live bind address'; return 1; }
}

verify_host_port_listeners() {
  local output line state local_address
  local listener_count=0 has_tailnet_listener=0
  output=$(ss -Hltn 'sport = :2447') || { fail 'cannot inspect host Bot API port listeners'; return 1; }
  while IFS= read -r line; do
    [[ -n "$line" ]] || continue
    read -r state _ _ local_address _ <<< "$line"
    [[ "$state" == LISTEN && -n "$local_address" ]] || {
      fail 'host Bot API listener output is malformed'
      return 1
    }
    case "$local_address" in
      127.0.0.1:2447) ;;
      100.124.236.66:2447) has_tailnet_listener=1 ;;
      *) fail 'Bot API port has a public or LAN host listener'; return 1 ;;
    esac
    listener_count=$((listener_count + 1))
  done <<< "$output"
  [[ "$listener_count" -gt 0 && "$has_tailnet_listener" == 1 ]] || {
    fail 'Bot API port is not listening on the tailnet host address'
    return 1
  }
}

TEMP_DIR=
TEMP_ENV=
ID_TEMP=
cleanup() {
  local status=$?
  trap - EXIT
  if [[ -n "$TEMP_ENV" ]]; then
    case "$TEMP_ENV" in
      "$REPO"/.env.main-2825.????????) rm -f -- "$TEMP_ENV" || status=1 ;;
      *) printf '%s\n' 'Bot API QA operation refused cleanup of an unexpected environment path' >&2; status=1 ;;
    esac
  fi
  if [[ -n "$ID_TEMP" ]]; then
    case "$ID_TEMP" in
      "$STATE_DIR"/.bot-api-qa-id.????????) rm -f -- "$ID_TEMP" || status=1 ;;
      *) printf '%s\n' 'Bot API QA operation refused cleanup of an unexpected bot id path' >&2; status=1 ;;
    esac
  fi
  if [[ -n "$TEMP_DIR" ]]; then
    case "$TEMP_DIR" in
      /tmp/teagram-bot-api-qa.????????) rm -rf -- "$TEMP_DIR" || status=1 ;;
      *) printf '%s\n' 'Bot API QA operation refused cleanup of an unexpected temporary directory' >&2; status=1 ;;
    esac
  fi
  exit "$status"
}
trap cleanup EXIT

MODE=${1:-apply}
if [[ "$MODE" == apply ]]; then
  [[ "$#" -le 1 ]] || { fail 'usage: bot-api-qa [apply|verify|provision]'; exit 64; }
elif [[ "$MODE" == verify || "$MODE" == provision ]]; then
  [[ "$#" == 1 ]] || { fail 'usage: bot-api-qa [apply|verify|provision]'; exit 64; }
else
  fail 'usage: bot-api-qa [apply|verify|provision]'
  exit 64
fi
[[ "$(id -u)" == "$EXPECTED_ROOT_UID" ]] || { fail 'operation requires root'; exit 1; }
[[ "${OPS_SHA:-}" =~ ^[0-9a-f]{40}$ ]] || { fail 'reviewed operation SHA is invalid'; exit 64; }
[[ "$(realpath -e -- "$REPO")" == "$REPO" ]] || { fail 'live checkout is missing or symlinked'; exit 1; }
cd "$REPO" || { fail 'cannot enter the live checkout'; exit 1; }
safe_state_dir || { fail 'deployment state directory failed ownership or mode checks'; exit 1; }
exec 9>/tmp/teagram-deploy.lock || { fail 'cannot open the deployment lock'; exit 1; }
flock -x 9 || { fail 'cannot acquire the deployment lock'; exit 1; }

if [[ "$MODE" == apply ]]; then
  private_file "$ENV_FILE" || { fail 'live .env is not a root-owned mode-0600 regular file'; exit 1; }
  private_file "$ENV_RECORD" || { fail 'verified .env record is not a root-owned mode-0600 regular file'; exit 1; }
  private_file "$RUNNER" || { fail 'reviewed rollout runner is unavailable or unsafe'; exit 1; }
  private_file "$BLOB_HELPER" || { fail 'reviewed blob authority helper is unavailable or unsafe'; exit 1; }
  if [[ "$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)" == 1 && \
        "$(dotenv_count "$ENV_FILE" TEAGRAM_BOT_API_HOST_IP)" == 1 ]]; then
    verify_live_configuration || exit 1
    printf 'configuration=already-applied source_sha=%s bind=127.0.0.1:2447 publish=%s:2447 forwarder=healthy\n' "$OPS_SHA" "$TAILNET_HOST_IP"
    exit 0
  fi
  [[ "$(dotenv_count "$ENV_FILE" TG_BOT_API_LISTEN_ADDR)" == 0 && \
     "$(dotenv_count "$ENV_FILE" TEAGRAM_BOT_API_HOST_IP)" == 0 ]] || {
    fail 'Bot API listener settings are partially configured or duplicated'
    exit 1
  }
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
  TEMP_ENV=$(mktemp "$REPO/.env.main-2825.XXXXXXXX") || { fail 'cannot stage the Bot API environment settings'; exit 1; }
  if ! cat -- "$PREVIOUS_ENV_FILE" > "$TEMP_ENV" || \
     ! printf 'TG_BOT_API_LISTEN_ADDR=%s\nTEAGRAM_BOT_API_HOST_IP=%s\n' "$APP_LISTEN_ADDR" "$TAILNET_HOST_IP" >> "$TEMP_ENV" || \
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
      ROLLOUT_RUNNER_ADDED_ENV=TG_BOT_API_LISTEN_ADDR,TEAGRAM_BOT_API_HOST_IP \
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
      TEMP_ENV=$(mktemp "$REPO/.env.main-2825.XXXXXXXX") || { fail 'rollout failed and the previous environment cannot be staged'; exit "$runner_status"; }
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
  verify_live_configuration || exit 1
  printf 'configuration=applied source_sha=%s env_sha256=%s bind=127.0.0.1:2447 publish=%s:2447 forwarder=healthy\n' \
    "$OPS_SHA" "$(file_sha256 "$ENV_FILE")" "$TAILNET_HOST_IP"
  exit 0
fi

if [[ "$MODE" == verify ]]; then
  verify_live_configuration || exit 1
  verify_host_port_listeners || exit 1
  printf 'configuration=verified source_sha=%s bind=127.0.0.1:2447 publish=%s:2447 forwarder=healthy listeners=verified\n' "$OPS_SHA" "$TAILNET_HOST_IP"
  public_probe_ip=$(curl -4 --fail --silent --show-error --max-time 8 https://api.ipify.org) || {
    fail 'cannot determine the host public IPv4 probe address'
    exit 1
  }
  python3 - "$public_probe_ip" <<'PY'
import ipaddress
import sys

try:
    address = ipaddress.ip_address(sys.argv[1])
except ValueError:
    raise SystemExit(1)
if address.version != 4 or not address.is_global:
    raise SystemExit(1)
PY
  printf 'public_probe_ip=%s\n' "$public_probe_ip"
  exit 0
fi

verify_live_configuration || exit 1
private_file "$BOT_ID_FILE" || {
  if [[ -e "$BOT_ID_FILE" || -L "$BOT_ID_FILE" ]]; then fail 'stored test bot id is unsafe'; exit 1; fi
}
TEMP_DIR=$(mktemp -d /tmp/teagram-bot-api-qa.XXXXXXXX) || { fail 'cannot allocate private bot token directory'; exit 1; }
[[ -d "$TEMP_DIR" && ! -L "$TEMP_DIR" && "$(stat -c '%u:%a' -- "$TEMP_DIR")" == "$EXPECTED_ROOT_UID:700" ]] || {
  fail 'private bot token directory failed ownership or mode checks'
  exit 1
}
token_file=$TEMP_DIR/token
error_file=$TEMP_DIR/error
: > "$token_file"
: > "$error_file"
chmod 600 -- "$token_file" "$error_file"

read_token() {
  local wanted_id=$1
  mapfile -t token_lines < "$token_file"
  [[ "${#token_lines[@]}" == 1 ]] || return 1
  TOKEN=${token_lines[0]}
  [[ "$TOKEN" =~ ^([1-9][0-9]{0,18}):[A-Za-z0-9_-]{43}$ ]] || return 1
  [[ "${BASH_REMATCH[1]}" == "$wanted_id" ]]
}

if [[ -e "$BOT_ID_FILE" ]]; then
  private_file "$BOT_ID_FILE" || { fail 'stored test bot id is unsafe'; exit 1; }
  IFS= read -r bot_id < "$BOT_ID_FILE" || { fail 'cannot read stored test bot id'; exit 1; }
  [[ "$bot_id" =~ ^[1-9][0-9]{0,18}$ ]] || { fail 'stored test bot id is malformed'; exit 1; }
  if docker exec --user 0 "$SERVICE" /usr/local/bin/teagramd bot rotate "$bot_id" > "$token_file" 2> "$error_file"; then
    read_token "$bot_id" || { fail 'first bot token rotation returned an invalid token'; exit 1; }
    revoked_token=$TOKEN
  else
    fail 'could not rotate the test bot token'
    exit 1
  fi
else
  [[ ! -L "$BOT_ID_FILE" ]] || { fail 'test bot id path is a symlink'; exit 1; }
  if docker exec --user 0 "$SERVICE" /usr/local/bin/teagramd bot create teagram_test_bot 'Teagram QA' > "$token_file" 2> "$error_file"; then
    mapfile -t token_lines < "$token_file"
    [[ "${#token_lines[@]}" == 1 && "${token_lines[0]}" =~ ^([1-9][0-9]{0,18}):[A-Za-z0-9_-]{43}$ ]] || {
      fail 'bot creation returned an invalid token'
      exit 1
    }
    bot_id=${BASH_REMATCH[1]}
    revoked_token=${token_lines[0]}
    ID_TEMP=$(mktemp "$STATE_DIR/.bot-api-qa-id.XXXXXXXX") || { fail 'cannot stage test bot identity'; exit 1; }
    if ! printf '%s\n' "$bot_id" > "$ID_TEMP" || ! chmod 600 -- "$ID_TEMP" || ! sync; then
      fail 'cannot persist test bot identity'
      exit 1
    fi
    mv -n -- "$ID_TEMP" "$BOT_ID_FILE" || { fail 'cannot publish test bot identity'; exit 1; }
    ID_TEMP=
    private_file "$BOT_ID_FILE" || { fail 'persisted test bot identity is unsafe'; exit 1; }
  else
    fail 'could not create the test bot'
    exit 1
  fi
  : > "$token_file"
  : > "$error_file"
  if docker exec --user 0 "$SERVICE" /usr/local/bin/teagramd bot rotate "$bot_id" > "$token_file" 2> "$error_file"; then
    read_token "$bot_id" || { fail 'bot activation returned an invalid token'; exit 1; }
    active_token=$TOKEN
  else
    fail 'could not activate the test bot token'
    exit 1
  fi
fi

if [[ ! ${active_token:-} ]]; then
  : > "$token_file"
  : > "$error_file"
  if docker exec --user 0 "$SERVICE" /usr/local/bin/teagramd bot rotate "$bot_id" > "$token_file" 2> "$error_file"; then
    read_token "$bot_id" || { fail 'second bot token rotation returned an invalid token'; exit 1; }
    active_token=$TOKEN
  else
    fail 'could not activate the test bot token'
    exit 1
  fi
fi
[[ "$revoked_token" != "$active_token" ]] || { fail 'token rotation did not revoke the prior test token'; exit 1; }
printf 'bot_id=%s\nrevoked_token=%s\nactive_token=%s\n' "$bot_id" "$revoked_token" "$active_token"
