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
BLOB_MOUNT=/srv/teagram-demo-blobs
BLOB_DATA=$BLOB_MOUNT/data
BLOB_MIN_BYTES=$((3 * 1024 * 1024 * 1024))
BLOB_MAX_BYTES=$((4 * 1024 * 1024 * 1024))
MEMORY_BUDGET_BYTES=$((200 * 1024 * 1024))
DEMO_SUBNETS=(198.18.130.0/24 198.18.132.0/24 198.18.133.0/24)
REQUIRED_ENV_KEYS=(POSTGRES_PASSWORD TG_PUBLIC_LINK_PREFIX TG_AUTHKEY_ENC_KEY TEAGRAM_DEMO_TURNSTILE_SITE_KEY TEAGRAM_DEMO_IMAGE_REF)
SECRET_FILES=(turnstile-secret-key turnstile-qa-bypass-token)

failures=0
pass() { printf 'PASS %s\n' "$1"; }
fail() {
  printf 'FAIL %s: %s\n' "$1" "$2"
  failures=$((failures + 1))
}

# Mode bits of a file as octal digits, e.g. 600.
mode_of() { stat -c '%a' -- "$1"; }

# Succeeds when no group or other bits are set.
private_mode() { (( ((8#$1) & 077) == 0 )); }

check_tools() {
  local tool missing=()
  for tool in docker jq python3 stat mountpoint; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
  done
  if ((${#missing[@]} > 0)); then
    fail tools "missing on this host: ${missing[*]}"
    return 1
  fi
  pass tools
}

check_env_file() {
  local key missing=()
  if [[ ! -f $ENV_FILE ]]; then
    fail env-file "missing $ENV_FILE"
    return 0
  fi
  if ! private_mode "$(mode_of "$ENV_FILE")"; then
    fail env-file "$ENV_FILE is readable by group or other"
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
  local name path missing=() bad=()
  if [[ ! -d $SECRETS_DIR ]]; then
    fail secrets "missing directory $SECRETS_DIR"
    return 0
  fi
  if ! private_mode "$(mode_of "$SECRETS_DIR")"; then
    fail secrets "$SECRETS_DIR is readable by group or other"
    return 0
  fi
  for name in "${SECRET_FILES[@]}"; do
    path=$SECRETS_DIR/$name
    if [[ ! -f $path ]]; then
      missing+=("$name")
    # Readable by the non-root server uid through the bind mount, but not
    # writable by group or other.
    elif (( (8#$(mode_of "$path")) & 022 )); then
      bad+=("$name")
    fi
  done
  path=$SECRETS_DIR/cloudflared.env
  if [[ ! -f $path ]]; then
    missing+=(cloudflared.env)
  else
    private_mode "$(mode_of "$path")" || bad+=(cloudflared.env)
    grep -q '^TUNNEL_TOKEN=.' -- "$path" || missing+=("cloudflared.env:TUNNEL_TOKEN")
  fi
  if ((${#missing[@]} > 0)); then
    fail secrets "missing: ${missing[*]}"
    return 0
  fi
  if ((${#bad[@]} > 0)); then
    fail secrets "wrong mode on: ${bad[*]}"
    return 0
  fi
  pass secrets
}

# Renders the stack the way deploy will. The output is kept in a variable and
# never printed.
render_config() {
  # Compose lets shell variables override --env-file, so an exported value
  # would silently replace the one the operator wrote to the env file.
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    TEAGRAM_DEMO_SECRETS_DIR="$SECRETS_DIR" \
    docker compose --env-file "$ENV_FILE" --project-directory "$REPO_ROOT" \
    -f "$COMPOSE_FILE" --project-name teagram-demo config --format json 2>/dev/null
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
    sum=$(jq '[.services[] | select(.restart != null) | .mem_limit | tonumber] | add' <<<"$CONFIG_JSON")
    if (( sum > MEMORY_BUDGET_BYTES )); then
      fail mem-limits "long-running limits total $((sum / 1048576)) MiB, budget is 200 MiB"
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
  local blocks block_size total data_dev mount_dev owner_mode
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

# The demo subnets must not overlap any network this host already has, other
# than the demo's own networks from an earlier deploy.
check_subnet_overlap() {
  local ids=() other overlaps
  mapfile -t ids < <(docker network ls -q 2>/dev/null) || { fail subnets-host "docker network ls failed"; return 0; }
  other=""
  if ((${#ids[@]} > 0)); then
    other=$(docker network inspect "${ids[@]}" 2>/dev/null \
      | jq -r '.[] | select(.Name | startswith("teagram-demo_") | not) | .IPAM.Config[]?.Subnet // empty') \
      || { fail subnets-host "docker network inspect failed"; return 0; }
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
  fail compose-config "docker compose config failed; rerun that command by hand to read the error (its output holds secrets, so do not paste it)"
fi
check_subnet_overlap

if ((failures == 0)); then
  printf 'would_apply\n'
  exit 0
fi
printf 'would_reject (%d failing)\n' "$failures"
exit 1
