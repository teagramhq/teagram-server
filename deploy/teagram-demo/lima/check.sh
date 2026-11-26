#!/usr/bin/env bash
# Read-only preflight for the lima demo stack. It pulls nothing, starts and
# stops no container, and writes no file. It prints one PASS or FAIL line per
# check and ends with would_apply (exit 0) or would_reject (exit 1).
#
# The resolved Compose configuration is never printed: it inlines the env file
# and so contains the database password and the tunnel token.
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"
ENV_FILE=${TEAGRAM_DEMO_LIMA_ENV_FILE:-/var/lib/teagram-demo/lima.env}
SECRETS_DIR=${TEAGRAM_DEMO_SECRETS_DIR:-/var/lib/teagram-demo/secrets}
BLOB_MOUNT=${TEAGRAM_DEMO_BLOB_MOUNT:-/srv/teagram-demo-blobs}
BLOB_DATA=$BLOB_MOUNT/data
BLOB_MIN_BYTES=$((3 * 1024 * 1024 * 1024))
BLOB_MAX_BYTES=$((4 * 1024 * 1024 * 1024))
MEMORY_BUDGET_BYTES=$((200 * 1024 * 1024))
COMPOSE_RENDER_COMMAND='docker compose --env-file <lima.env> --project-directory <checkout> -f deploy/teagram-demo/lima/compose.yaml --project-name teagram-demo config --format json'
DEMO_SUBNETS=(198.18.130.0/24 198.18.132.0/24 198.18.133.0/24)
REQUIRED_ENV_KEYS=(POSTGRES_PASSWORD TG_PUBLIC_LINK_PREFIX TG_AUTHKEY_ENC_KEY TEAGRAM_DEMO_TURNSTILE_SITE_KEY TEAGRAM_DEMO_IMAGE_REF)
SECRET_FILES=(turnstile-secret-key turnstile-qa-bypass-token)

failures=0
pass() { printf 'PASS %s\n' "$1"; }
fail() {
  printf 'FAIL %s: %s\n' "$1" "$2"
  failures=$((failures + 1))
}

check_tools() {
  local tool missing=()
  for tool in docker jq python3 stat mountpoint findmnt; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  if ((${#missing[@]} > 0)); then
    fail tools "missing on this host: ${missing[*]}"
    return 1
  fi
  pass tools
}

check_env_file() {
  local key missing=() owner_mode
  if [[ ! -f $ENV_FILE || -L $ENV_FILE ]]; then
    fail env-file "missing or unsafe file $ENV_FILE"
    return 0
  fi
  if ! owner_mode=$(stat -c '%u:%g:%a' -- "$ENV_FILE" 2>/dev/null); then
    fail env-file "could not inspect $ENV_FILE ownership and mode"
    return 0
  fi
  if [[ $owner_mode != '0:0:600' ]]; then
    fail env-file "$ENV_FILE must be root-owned mode 0600"
    return 0
  fi
  for key in "${REQUIRED_ENV_KEYS[@]}"; do
    grep -q "^${key}=." -- "$ENV_FILE" || missing+=("$key")
  done
  if ((${#missing[@]} > 0)); then
    fail env-file "missing keys: ${missing[*]}"
    return 0
  fi
  pass env-file
}

check_secrets() {
  local name path missing=() empty=() bad=() owner_mode
  if [[ ! -d $SECRETS_DIR || -L $SECRETS_DIR ]]; then
    fail secrets "missing or unsafe directory $SECRETS_DIR"
    return 0
  fi
  if ! owner_mode=$(stat -c '%u:%g:%a' -- "$SECRETS_DIR" 2>/dev/null); then
    fail secrets "could not inspect $SECRETS_DIR ownership and mode"
    return 0
  fi
  if [[ $owner_mode != '0:0:700' ]]; then
    fail secrets "$SECRETS_DIR must be root-owned mode 0700"
    return 0
  fi
  for name in "${SECRET_FILES[@]}"; do
    path=$SECRETS_DIR/$name
    if [[ ! -f $path || -L $path ]]; then
      missing+=("$name")
    elif [[ ! -s $path ]]; then
      empty+=("$name")
    elif ! owner_mode=$(stat -c '%u:%g:%a' -- "$path" 2>/dev/null); then
      bad+=("$name (could not inspect ownership and mode)")
    elif [[ $owner_mode != '0:65532:440' ]]; then
      bad+=("$name ($owner_mode; expected root:65532 mode 0440)")
    fi
  done
  path=$SECRETS_DIR/cloudflared.env
  if [[ ! -f $path || -L $path ]]; then
    missing+=(cloudflared.env)
  else
    if ! owner_mode=$(stat -c '%u:%g:%a' -- "$path" 2>/dev/null); then
      bad+=("cloudflared.env (could not inspect ownership and mode)")
    elif [[ $owner_mode != '0:0:600' ]]; then
      bad+=("cloudflared.env ($owner_mode; expected root:root mode 0600)")
    fi
  fi
  if ((${#missing[@]} > 0)); then
    fail secrets "missing: ${missing[*]}"
    return 0
  fi
  if ((${#empty[@]} > 0)); then
    fail secrets "empty: ${empty[*]}"
    return 0
  fi
  if ((${#bad[@]} > 0)); then
    fail secrets "wrong ownership or mode on: ${bad[*]}"
    return 0
  fi
  pass secret-files
}

# Renders the stack the way deploy will. The output is kept in a variable and
# never printed.
render_config() {
  local stdout_fd compose_stderr status=0
  # Compose lets shell variables override --env-file, so an exported value
  # would silently replace the one the operator wrote to the env file.
  exec {stdout_fd}>&1
  compose_stderr=$(env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    docker compose --env-file "$ENV_FILE" --project-directory "$REPO_ROOT" \
    -f "$COMPOSE_FILE" --project-name teagram-demo \
    config --format json 2>&1 1>&"$stdout_fd") || status=$?
  exec {stdout_fd}>&-
  if ((status != 0)); then
    printf 'compose-config diagnostic: Docker Compose config failed; details withheld because dotenv errors may include values\n' >&2
  elif [[ -n $compose_stderr ]]; then
    printf 'compose-config diagnostic: Docker Compose emitted a diagnostic; details withheld because they may include dotenv values\n' >&2
  fi
  return "$status"
}

check_tunnel_token() {
  if ! jq -e '.services.cloudflared.environment.TUNNEL_TOKEN | type == "string" and length > 0' \
    <<<"$CONFIG_JSON" >/dev/null; then
    fail tunnel-token "cloudflared.env:TUNNEL_TOKEN is missing or empty"
  else
    pass tunnel-token
  fi
}

# Each check below reads the rendered JSON from CONFIG_JSON.
check_config_shape() {
  if ! jq -e '[.services[] | select(has("build"))] | length == 0' <<<"$CONFIG_JSON" >/dev/null; then
    fail config-build "a service has a build key; lima never builds"
  else
    pass config-build
  fi

  if ! jq -e '[.services | keys[] | select(startswith("qa-invite"))] | length == 0' <<<"$CONFIG_JSON" >/dev/null; then
    fail config-qa-invite "the lima stack must not contain qa-invite"
  else
    pass config-qa-invite
  fi

  if ! jq -e '.services.edge.ports | any(.host_ip == "127.0.0.1" and (.published | tostring) == "8080" and (.target | tostring) == "8080")' \
    <<<"$CONFIG_JSON" >/dev/null; then
    fail edge-health-bind "edge must publish loopback port 8080 for the local /healthz rollout check"
  else
    pass edge-health-bind
  fi

  if ! jq -e '.services.teagramd.environment.TG_PG_MAX_CONNS == "3" and (.services | has("teagramd-next") | not)' \
    <<<"$CONFIG_JSON" >/dev/null; then
    fail postgres-pool "the single server must set TG_PG_MAX_CONNS=3 and there must be no candidate service"
  else
    pass postgres-pool
  fi

  local unpinned
  unpinned=$(jq -r '.services | to_entries[] | select((.value.image // "") | test("@sha256:[0-9a-f]{64}$") | not) | .key' <<<"$CONFIG_JSON")
  if [[ -n $unpinned ]]; then
    fail image-digests "not pinned by digest: $(tr '\n' ' ' <<<"$unpinned")"
  else
    pass image-digests
  fi

  local no_limit
  # Compose renders mem_limit as a string of bytes, so convert before comparing.
  no_limit=$(jq -r '.services | to_entries[] | select((.value.mem_limit // "0" | tonumber) <= 0) | .key' <<<"$CONFIG_JSON")
  if [[ -n $no_limit ]]; then
    fail mem-limits "no memory limit: $(tr '\n' ' ' <<<"$no_limit")"
  else
    local sum
    sum=$(jq '[.services | to_entries[] | select(.key != "migrate") | .value.mem_limit | tonumber] | add' <<<"$CONFIG_JSON")
    if (( sum > MEMORY_BUDGET_BYTES )); then
      fail mem-limits "all services except one-shot migrate total $((sum / 1048576)) MiB, budget is 200 MiB"
    else
      pass "mem-limits ($((sum / 1048576)) MiB of 200 MiB)"
    fi
  fi

  local subnets
  subnets=$(jq -r '[.networks.backend.ipam.config[0].subnet, .networks.egress.ipam.config[0].subnet, .networks.edge.ipam.config[0].subnet] | join(" ")' <<<"$CONFIG_JSON")
  if [[ $subnets != "${DEMO_SUBNETS[0]} ${DEMO_SUBNETS[1]} ${DEMO_SUBNETS[2]}" ]]; then
    fail subnets "resolved subnets are $subnets"
  else
    pass subnets
  fi
}

# The Compose blob bind must be the capped mount. The capped filesystem itself
# is checked on the host, so one FAIL line covers every blob problem.
check_blob_cap() {
  local blocks block_size total data_dev mount_dev owner_mode mount_options
  if ! jq -e --arg src "$BLOB_DATA" '.services.teagramd.volumes | any(.type == "bind" and .source == $src)' <<<"$CONFIG_JSON" >/dev/null; then
    fail blob-cap "teagramd does not bind $BLOB_DATA"
    return 0
  fi
  if ! mountpoint -q "$BLOB_MOUNT"; then
    fail blob-cap "$BLOB_MOUNT is not a mount point"
    return 0
  fi
  read -r blocks block_size < <(stat -f -c '%b %S' -- "$BLOB_MOUNT")
  total=$((blocks * block_size))
  if (( total > BLOB_MAX_BYTES || total < BLOB_MIN_BYTES )); then
    fail blob-cap "$BLOB_MOUNT is $total bytes, expected 3 to 4 GiB"
    return 0
  fi
  if ! mount_options=$(findmnt -n -o OPTIONS --target "$BLOB_MOUNT" 2>/dev/null) || [[ -z $mount_options ]]; then
    fail blob-cap "could not inspect mount options for $BLOB_MOUNT"
    return 0
  fi
  case ",$mount_options," in
    *,rw,*) ;;
    *)
      fail blob-cap "$BLOB_MOUNT is mounted read-only"
      return 0
      ;;
  esac
  if [[ ! -d $BLOB_DATA ]]; then
    fail blob-cap "missing $BLOB_DATA"
    return 0
  fi
  mount_dev=$(stat -c '%d' -- "$BLOB_MOUNT")
  data_dev=$(stat -c '%d' -- "$BLOB_DATA")
  if [[ $mount_dev != "$data_dev" ]]; then
    fail blob-cap "$BLOB_DATA is not on the capped filesystem"
    return 0
  fi
  owner_mode=$(stat -c '%u:%g %a' -- "$BLOB_DATA")
  if [[ $owner_mode != '65532:65532 700' ]]; then
    fail blob-cap "$BLOB_DATA is $owner_mode, expected 65532:65532 700"
    return 0
  fi
  pass blob-cap
}

# Captures Docker stdout while retaining only the first stderr line. A failed
# command returns a marker so callers can include the diagnostic in FAIL.
capture_docker() {
  local stdout_fd stderr status=0
  exec {stdout_fd}>&1
  stderr=$(docker "$@" 2>&1 1>&"$stdout_fd") || status=$?
  exec {stdout_fd}>&-
  if ((status != 0)); then
    printf '\n__DOCKER_ERROR__%s\n' "${stderr%%$'\n'*}"
    return "$status"
  fi
  if [[ -n $stderr ]]; then
    printf 'docker diagnostic: %s\n' "${stderr%%$'\n'*}" >&2
  fi
  return 0
}

# The demo subnets must not overlap any network this host already has, other
# than the demo's own networks from an earlier deploy.
check_subnet_overlap() {
  local ids=() ids_output id other overlaps diagnostic
  if ! ids_output=$(capture_docker network ls -q); then
    diagnostic=${ids_output##*$'\n__DOCKER_ERROR__'}
    fail subnets-host "docker network ls failed: ${diagnostic:-no diagnostic}"
    return 0
  fi
  while IFS= read -r id; do
    [[ -n $id ]] && ids+=("$id")
  done <<<"$ids_output"
  other=""
  if ((${#ids[@]} > 0)); then
    local inspect_json
    if ! inspect_json=$(capture_docker network inspect "${ids[@]}"); then
      diagnostic=${inspect_json##*$'\n__DOCKER_ERROR__'}
      fail subnets-host "docker network inspect failed: ${diagnostic:-no diagnostic}"
      return 0
    fi
    other=$(jq -r '.[] | select(.Name | startswith("teagram-demo_") | not) | .IPAM.Config[]?.Subnet // empty' <<<"$inspect_json") \
      || { fail subnets-host "docker network inspect returned invalid JSON"; return 0; }
  fi
  overlaps=$(printf '%s\n' "$other" | python3 -I -c '
import ipaddress, sys
ours = [ipaddress.ip_network(s) for s in sys.argv[1:]]
for line in sys.stdin.read().split():
    theirs = ipaddress.ip_network(line)
    if any(theirs.overlaps(net) for net in ours):
        print(line)
' "${DEMO_SUBNETS[@]}")
  if [[ -n $overlaps ]]; then
    fail subnets-host "overlaps existing network: $(tr '\n' ' ' <<<"$overlaps")"
  else
    pass subnets-host
  fi
}

check_tools || { printf 'would_reject\n'; exit 1; }
check_env_file
check_secrets

CONFIG_JSON=""
if CONFIG_JSON=$(render_config) && [[ -n $CONFIG_JSON ]]; then
  pass compose-config
  check_config_shape
  check_blob_cap
else
  fail compose-config "render failed; Compose diagnostics are withheld to protect dotenv values; command: $COMPOSE_RENDER_COMMAND"
fi
if [[ -n $CONFIG_JSON ]]; then
  check_tunnel_token
fi
check_subnet_overlap

if ((failures == 0)); then
  printf 'would_apply\n'
  exit 0
fi
printf 'would_reject (%d failing)\n' "$failures"
exit 1
