#!/usr/bin/env bash
set -Eeuo pipefail

readonly IPTABLES_CHAIN=TEAGRAM_DEMO_EGRESS
readonly IPTABLES_INPUT_CHAIN=TEAGRAM_DEMO_INPUT
readonly INPUT_BRIDGE_INTERFACE=br+
readonly OPENRC_SERVICE=teagram-demo-egress
readonly OPENRC_STACK_SERVICE=teagram-demo-stack
readonly DOCKER_READY_TIMEOUT_SECONDS=60
readonly DOCKER_INFO_PROBE_TIMEOUT_SECONDS=5
readonly DOCKER_READY_ATTEMPTS=60
readonly FIREWALL_LOCK_PATH=/run/teagram-demo-egress.lock
readonly FIREWALL_LOCK_TIMEOUT_SECONDS=30
readonly REPO_ROOT=/opt/teagram-demo
readonly COMPOSE_FILE=/opt/teagram-demo/deploy/teagram-demo/lima/compose.yaml
readonly COMPOSE_ENV_FILE=${TEAGRAM_DEMO_LIMA_ENV_FILE:-/var/lib/teagram-demo/lima.env}
readonly COMPOSE_SECRETS_DIR=${TEAGRAM_DEMO_SECRETS_DIR:-/var/lib/teagram-demo/secrets}
readonly -a DEMO_IPV4_SUBNETS=(
  198.18.130.0/24
  198.18.132.0/24
  198.18.133.0/24
)
readonly -a DEMO_NETWORKS=(
  teagram-demo_backend
  teagram-demo_egress
  teagram-demo_edge
)
readonly -a BLOCKED_IPV4_CIDRS=(
  10.0.0.0/8
  172.16.0.0/12
  192.168.0.0/16
  100.64.0.0/10
  169.254.0.0/16
  224.0.0.0/4
)
readonly -a BLOCKED_IPV6_CIDRS=(
  fc00::/7
  fe80::/10
  ff00::/8
)

IPV6_SUBNETS=()
MISSING_DEMO_NETWORKS=()
CHAIN_LISTING=''
STACK_LOCK_ACQUIRED=0
STACK_LOCK_SKIPPED=0
FIREWALL_LOCK_ACQUIRED=0

fail() {
  printf 'demo egress isolation: %s\n' "$1" >&2
  exit 1
}

require_root() {
  [[ "$EUID" -eq 0 ]] || fail 'must run as root'
}

docker_info_ready() {
  local probe_timeout_seconds=${1:-$DOCKER_INFO_PROBE_TIMEOUT_SECONDS}

  command -v timeout >/dev/null 2>&1 || fail 'timeout is required on lima'
  timeout "$probe_timeout_seconds" docker info >/dev/null 2>&1
}

wait_for_docker() {
  local ready_timeout_seconds=${1:-$DOCKER_READY_TIMEOUT_SECONDS}
  local max_probe_timeout_seconds=${2:-$DOCKER_INFO_PROBE_TIMEOUT_SECONDS}
  local attempt remaining probe_timeout_seconds
  local deadline=$((SECONDS + ready_timeout_seconds))

  command -v docker >/dev/null 2>&1 || fail 'docker is required on lima'
  for ((attempt = 1; attempt <= DOCKER_READY_ATTEMPTS && SECONDS < deadline; attempt++)); do
    remaining=$((deadline - SECONDS))
    probe_timeout_seconds=$max_probe_timeout_seconds
    if (( probe_timeout_seconds > remaining )); then
      probe_timeout_seconds=$remaining
    fi
    if (( probe_timeout_seconds < 1 )); then
      break
    fi
    if docker_info_ready "$probe_timeout_seconds"; then
      return 0
    fi
    if (( attempt < DOCKER_READY_ATTEMPTS && SECONDS < deadline )); then
      sleep 1
    fi
  done
  fail "Docker daemon did not become ready within ${ready_timeout_seconds} seconds"
}

render_rules() {
  local family=$1
  shift
  local subnet cidr gateway
  local -a blocked_cidrs

  (( $# > 0 )) || fail 'at least one source subnet is required'

  case "$family" in
    ipv4)
      blocked_cidrs=("${BLOCKED_IPV4_CIDRS[@]}")
      ;;
    ipv6)
      blocked_cidrs=("${BLOCKED_IPV6_CIDRS[@]}")
      ;;
    *) fail "unknown address family: $family" ;;
  esac

  for subnet in "$@"; do
    printf '%s\n' "-A FORWARD -s $subnet -j $IPTABLES_CHAIN"
    printf '%s\n' "-A DOCKER-USER -s $subnet -j $IPTABLES_CHAIN"
    printf '%s\n' "-A INPUT -i $INPUT_BRIDGE_INTERFACE -s $subnet -j $IPTABLES_INPUT_CHAIN"
  done
  for subnet in "$@"; do
    printf '%s\n' "-A $IPTABLES_CHAIN -d $subnet -j RETURN"
  done
  printf '%s\n' "-A $IPTABLES_CHAIN -m conntrack --ctstate RELATED,ESTABLISHED --ctdir REPLY -j RETURN"
  for cidr in "${blocked_cidrs[@]}"; do
    printf '%s\n' "-A $IPTABLES_CHAIN -d $cidr -j DROP"
  done
  printf '%s\n' "-A $IPTABLES_CHAIN -j RETURN"

  printf '%s\n' "-A $IPTABLES_INPUT_CHAIN -m conntrack --ctstate RELATED,ESTABLISHED --ctdir REPLY -j RETURN"
  if [[ "$family" == ipv4 ]]; then
    for subnet in "$@"; do
      gateway=$(ipv4_gateway_for_subnet "$subnet") \
        || fail "cannot derive the bridge gateway for $subnet"
      printf '%s\n' "-A $IPTABLES_INPUT_CHAIN -d $gateway/32 -j DROP"
    done
  fi
  for cidr in "${blocked_cidrs[@]}"; do
    printf '%s\n' "-A $IPTABLES_INPUT_CHAIN -d $cidr -j DROP"
  done
  printf '%s\n' "-A $IPTABLES_INPUT_CHAIN -j RETURN"
}

ipv4_gateway_for_subnet() {
  python3 - "$1" <<'PY'
import ipaddress
import sys

network = ipaddress.IPv4Network(sys.argv[1], strict=True)
if network.num_addresses < 4:
    raise SystemExit("IPv4 subnet is too small for a bridge gateway")
print(network.network_address + 1)
PY
}

validate_render_subnet() {
  local family=$1 subnet=$2
  local python_family
  case "$family" in
    ipv4) python_family=IPv4Network ;;
    ipv6) python_family=IPv6Network ;;
    *) fail "unknown address family: $family" ;;
  esac
  python3 - "$python_family" "$subnet" <<'PY'
import ipaddress
import sys

network_type = getattr(ipaddress, sys.argv[1])
try:
    network_type(sys.argv[2], strict=True)
except ValueError as error:
    print(f"invalid source subnet: {error}", file=sys.stderr)
    raise SystemExit(1)
PY
}

render_mode() {
  local family=$1
  shift
  local -a subnets=("$@")

  if (( ${#subnets[@]} == 0 )); then
    if [[ "$family" == ipv4 ]]; then
      subnets=("${DEMO_IPV4_SUBNETS[@]}")
    else
      fail 'IPv6 render mode requires at least one example subnet'
    fi
  fi
  for subnet in "${subnets[@]}"; do
    validate_render_subnet "$family" "$subnet" || fail 'invalid render subnet'
  done
  render_rules "$family" "${subnets[@]}"
}

load_ipv6_subnets() {
  local network config enabled subnet
  local -a fields
  local -A seen=()
  IPV6_SUBNETS=()
  MISSING_DEMO_NETWORKS=()

  command -v docker >/dev/null 2>&1 || fail 'docker is required to inspect demo network IPv6 settings'
  local network_names
  network_names=$(docker network ls --format '{{.Name}}') || fail 'cannot list Docker networks'

  for network in "${DEMO_NETWORKS[@]}"; do
    if ! grep -Fqx -- "$network" <<<"$network_names"; then
      MISSING_DEMO_NETWORKS+=("$network")
      continue
    fi
    config=$(docker network inspect --format '{{.EnableIPv6}} {{range .IPAM.Config}}{{.Subnet}} {{end}}' "$network") \
      || fail "cannot inspect Docker network $network"
    read -r -a fields <<<"$config"
    enabled=${fields[0]:-false}
    [[ "$enabled" == true ]] || continue

    local found_subnet=0
    for subnet in "${fields[@]:1}"; do
      [[ "$subnet" == *:*/* ]] || continue
      [[ -n ${seen[$subnet]:-} ]] && continue
      seen[$subnet]=1
      IPV6_SUBNETS+=("$subnet")
      found_subnet=1
    done
    (( found_subnet == 1 )) || fail "IPv6 is enabled on $network but Docker reported no IPv6 subnet"
  done
}

chain_listing() {
  local binary=$1 chain=$2 listing status first_line
  CHAIN_LISTING=''
  if listing=$("$binary" -w 10 -S "$chain" 2>&1); then
    CHAIN_LISTING=$listing
    return 0
  else
    status=$?
  fi

  if grep -Eqi 'No chain/target/match by that name|chain .* does not exist' <<<"$listing"; then
    return 1
  fi
  first_line=${listing%%$'\n'*}
  [[ -n $first_line ]] || first_line='no diagnostic was returned'
  printf '%s could not inspect %s (exit %s): %s\n' \
    "$binary" "$chain" "$status" "$first_line" >&2
  return 2
}

chain_listing_error() {
  local binary=$1 chain=$2 status=$3 missing_message=$4
  case "$status" in
    1) fail "$missing_message" ;;
    2) fail "$binary could not inspect $chain" ;;
    *) fail "$binary returned unexpected inspection status $status for $chain" ;;
  esac
}

delete_listed_rule() {
  local binary=$1 chain=$2 rule=$3

  # Rule numbers from `-S` become stale when another firewall writer updates
  # the chain. Delete the listed rule itself while xtables holds its lock.
  python3 - "$binary" "$chain" "$rule" <<'PY'
import shlex
import subprocess
import sys

binary, chain, rule = sys.argv[1:]
try:
    fields = shlex.split(rule)
except ValueError as error:
    print(f"cannot parse listed firewall rule: {error}", file=sys.stderr)
    raise SystemExit(1)
if len(fields) < 3 or fields[:2] != ["-A", chain]:
    print(f"unexpected listed firewall rule for {chain}: {rule}", file=sys.stderr)
    raise SystemExit(1)
raise SystemExit(
    subprocess.run(
        [binary, "-w", "10", "-D", chain, *fields[2:]],
        check=False,
    ).returncode
)
PY
}

rule_has_comment() {
  local rule=$1 comment=$2 quoted unquoted
  quoted="--comment \"$comment\""
  unquoted="--comment $comment"
  [[ "$rule" == *"$quoted"* \
    || "$rule" == *"$unquoted "* \
    || "$rule" == *"$unquoted" ]]
}

firewall_lock_path() {
  printf '%s\n' "$FIREWALL_LOCK_PATH"
}

firewall_lock_timeout() {
  printf '%s\n' "$FIREWALL_LOCK_TIMEOUT_SECONDS"
}

firewall_lock_acquire() {
  local lock_path lock_dir timeout old_umask status

  command -v flock >/dev/null 2>&1 || fail 'flock is required for firewall mutations'
  lock_path=$(firewall_lock_path) || fail 'cannot determine the firewall mutation lock path'
  lock_dir=$(dirname "$lock_path")
  timeout=$(firewall_lock_timeout) || fail 'cannot determine the firewall mutation lock timeout'
  [[ "$lock_path" == /* && -d $lock_dir && ! -L $lock_dir && ! -L $lock_path ]] \
    || fail 'firewall mutation lock path must be an absolute file in a real directory'
  [[ "$timeout" =~ ^[1-9][0-9]*$ ]] || fail 'firewall mutation lock timeout must be positive seconds'

  # Lock order is deploy.lock (FD 9), then this leaf lock (FD 8). No other
  # application lock may be acquired while holding the firewall lock.
  old_umask=$(umask)
  umask 077
  exec 8>>"$lock_path" || fail 'cannot open the firewall mutation lock'
  umask "$old_umask"
  if flock -x -w "$timeout" -E 75 8; then
    FIREWALL_LOCK_ACQUIRED=1
    return 0
  else
    status=$?
    exec 8>&-
  fi
  if (( status == 75 )); then
    fail "timed out after ${timeout} seconds waiting for the firewall mutation lock"
  fi
  fail "cannot acquire the firewall mutation lock (exit $status)"
}

firewall_lock_release() {
  if (( FIREWALL_LOCK_ACQUIRED == 1 )); then
    flock -u 8 || fail 'cannot release the firewall mutation lock'
    exec 8>&-
    FIREWALL_LOCK_ACQUIRED=0
  fi
}

apply_family() {
  local binary=$1 family=$2
  shift 2
  local -a subnets=("$@")
  local line status chain rendered_rules

  command -v "$binary" >/dev/null 2>&1 || fail "$binary is required for $family isolation"
  if chain_listing "$binary" DOCKER-USER; then
    :
  else
    status=$?
    chain_listing_error "$binary" DOCKER-USER "$status" \
      "$binary has no DOCKER-USER chain; start Docker before installing the rules"
  fi
  if chain_listing "$binary" INPUT; then
    :
  else
    status=$?
    chain_listing_error "$binary" INPUT "$status" "$binary has no INPUT chain"
  fi
  if chain_listing "$binary" FORWARD; then
    :
  else
    status=$?
    chain_listing_error "$binary" FORWARD "$status" "$binary has no FORWARD chain"
  fi
  rendered_rules=$(render_rules "$family" "${subnets[@]}") \
    || fail "$family isolation rules could not be rendered"
  for chain in "$IPTABLES_CHAIN" "$IPTABLES_INPUT_CHAIN"; do
    if chain_listing "$binary" "$chain"; then
      :
    else
      status=$?
      if (( status == 1 )); then
        "$binary" -w 10 -N "$chain"
      else
        chain_listing_error "$binary" "$chain" "$status" "$binary is missing $chain"
      fi
    fi
  done
  {
    printf '*filter\n-F %s\n-F %s\n' "$IPTABLES_CHAIN" "$IPTABLES_INPUT_CHAIN"
    while IFS= read -r line; do
      [[ "$line" == "-A $IPTABLES_CHAIN "* || "$line" == "-A $IPTABLES_INPUT_CHAIN "* ]] \
        || continue
      printf '%s\n' "$line"
    done <<< "$rendered_rules"
    printf 'COMMIT\n'
  } | "${binary}-restore" -w 10 --noflush

  ensure_source_hooks "$binary" FORWARD "$IPTABLES_CHAIN" '' '' "${subnets[@]}"
  ensure_source_hooks "$binary" DOCKER-USER "$IPTABLES_CHAIN" '' '' "${subnets[@]}"
  ensure_source_hooks "$binary" INPUT "$IPTABLES_INPUT_CHAIN" "$IPTABLES_CHAIN" \
    "$INPUT_BRIDGE_INTERFACE" "${subnets[@]}"
}

ensure_source_hooks() {
  local binary=$1 parent_chain=$2 hook_chain=$3 stale_chain=$4 interface=$5
  local subnet index line prefix_matches=1 listing status
  local hook_count=0 stale_count=0
  local staging_comment="teagram-demo-stage-${BASHPID}"
  shift 5
  local -a subnets=("$@")
  local -a expected_hooks=() parent_rules=() interface_args=()

  [[ -n $interface ]] && interface_args=(-i "$interface")

  for subnet in "${subnets[@]}"; do
    if [[ -n $interface ]]; then
      expected_hooks+=("-A $parent_chain -i $interface -s $subnet -j $hook_chain")
    else
      expected_hooks+=("-A $parent_chain -s $subnet -j $hook_chain")
    fi
  done
  if chain_listing "$binary" "$parent_chain"; then
    listing=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$parent_chain" "$status" "$binary is missing $parent_chain"
  fi
  mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")

  for ((index = 0; index < ${#expected_hooks[@]}; index++)); do
    if [[ "${parent_rules[$index]:-}" != "${expected_hooks[$index]}" ]]; then
      prefix_matches=0
      break
    fi
  done

  for line in "${parent_rules[@]}"; do
    if [[ "$line" == *" -j $hook_chain" ]]; then
      ((hook_count += 1))
    fi
    if [[ -n $stale_chain && "$line" == *" -j $stale_chain" ]]; then
      ((stale_count += 1))
    fi
  done

  if (( prefix_matches == 1 && hook_count == ${#expected_hooks[@]} && stale_count == 0 )); then
    return 0
  fi

  # Install temporary hooks before removing any existing ones. They match the
  # same source subnets and target the same isolation chain, so a failed later
  # insert cannot leave traffic without a hook.
  for ((index = ${#subnets[@]} - 1; index >= 0; index--)); do
    "$binary" -w 10 -I "$parent_chain" 1 -m comment --comment "$staging_comment" \
      "${interface_args[@]}" -s "${subnets[$index]}" -j "$hook_chain"
  done

  if chain_listing "$binary" "$parent_chain"; then
    listing=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$parent_chain" "$status" "$binary is missing $parent_chain"
  fi
  mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")

  # Remove exact listed stale or duplicate rules, leaving the staging hooks in
  # place. A concurrent host rule cannot be deleted by a stale line number.
  for ((index = ${#parent_rules[@]} - 1; index >= 0; index--)); do
    if [[ "${parent_rules[$index]}" == *" -j $hook_chain" ]] \
      || [[ -n $stale_chain && "${parent_rules[$index]}" == *" -j $stale_chain" ]]; then
      if rule_has_comment "${parent_rules[$index]}" "$staging_comment"; then
        continue
      fi
      delete_listed_rule "$binary" "$parent_chain" "${parent_rules[$index]}"
    fi
  done

  # Restore the stable prefix, then remove the temporary protective hooks.
  for ((index = ${#subnets[@]} - 1; index >= 0; index--)); do
    "$binary" -w 10 -I "$parent_chain" 1 "${interface_args[@]}" \
      -s "${subnets[$index]}" -j "$hook_chain"
  done

  if chain_listing "$binary" "$parent_chain"; then
    listing=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$parent_chain" "$status" "$binary is missing $parent_chain"
  fi
  mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")
  for line in "${parent_rules[@]}"; do
    if rule_has_comment "$line" "$staging_comment" && [[ "$line" == *" -j $hook_chain" ]]; then
      delete_listed_rule "$binary" "$parent_chain" "$line"
    fi
  done
}

remove_family() {
  local binary=$1 parent_chain listing status managed_chain index
  local -A parent_listings=() chain_present=()
  local -a parent_rules=() managed_chains=("$IPTABLES_CHAIN" "$IPTABLES_INPUT_CHAIN")
  if ! command -v "$binary" >/dev/null 2>&1; then
    return 0
  fi

  # Inspect every managed chain before changing any hooks or rules. A lock or
  # permission error must not be mistaken for an absent chain during removal.
  for parent_chain in DOCKER-USER INPUT FORWARD; do
    if chain_listing "$binary" "$parent_chain"; then
      parent_listings[$parent_chain]=$CHAIN_LISTING
    else
      status=$?
      if (( status == 1 )); then
        parent_listings[$parent_chain]=''
      else
        chain_listing_error "$binary" "$parent_chain" "$status" "$binary is missing $parent_chain"
      fi
    fi
  done

  for managed_chain in "${managed_chains[@]}"; do
    if chain_listing "$binary" "$managed_chain"; then
      chain_present[$managed_chain]=1
    else
      status=$?
      if (( status == 1 )); then
        chain_present[$managed_chain]=0
      else
        chain_listing_error "$binary" "$managed_chain" "$status" "$binary is missing $managed_chain"
      fi
    fi
  done

  for parent_chain in DOCKER-USER INPUT FORWARD; do
    listing=${parent_listings[$parent_chain]}
    mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")
    for ((index = ${#parent_rules[@]} - 1; index >= 0; index--)); do
      for managed_chain in "${managed_chains[@]}"; do
        if [[ "${parent_rules[$index]}" == *" -j $managed_chain" ]]; then
          delete_listed_rule "$binary" "$parent_chain" "${parent_rules[$index]}"
          break
        fi
      done
    done
  done

  for managed_chain in "${managed_chains[@]}"; do
    if [[ "${chain_present[$managed_chain]}" == 1 ]]; then
      "$binary" -w 10 -F "$managed_chain"
      "$binary" -w 10 -X "$managed_chain"
    fi
  done
}

check_source_hooks() {
  local binary=$1 family=$2 parent_chain=$3 hook_chain=$4 stale_chain=$5 interface=$6
  local listing subnet line index hook_count=0 status
  shift 6
  local -a subnets=("$@")
  local -a expected_hooks=() parent_rules=()

  if chain_listing "$binary" "$parent_chain"; then
    listing=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$parent_chain" "$status" \
      "$binary is missing $parent_chain for $family"
  fi
  mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")
  for subnet in "${subnets[@]}"; do
    if [[ -n $interface ]]; then
      expected_hooks+=("-A $parent_chain -i $interface -s $subnet -j $hook_chain")
    else
      expected_hooks+=("-A $parent_chain -s $subnet -j $hook_chain")
    fi
  done
  (( ${#parent_rules[@]} >= ${#expected_hooks[@]} )) \
    || fail "$family $parent_chain isolation hooks are missing or out of order"
  for ((index = 0; index < ${#expected_hooks[@]}; index++)); do
    [[ "${parent_rules[$index]}" == "${expected_hooks[$index]}" ]] \
      || fail "$family $parent_chain isolation hooks are missing or out of order"
  done
  for line in "${parent_rules[@]}"; do
    if [[ "$line" == *" -j $hook_chain" ]]; then
      ((hook_count += 1))
    fi
    if [[ -n $stale_chain && "$line" == *" -j $stale_chain" ]]; then
      fail "$family $parent_chain still has a hook to $stale_chain"
    fi
  done
  [[ "$hook_count" -eq "${#expected_hooks[@]}" ]] \
    || fail "$family $parent_chain hook is missing, duplicated, or out of order"
}

check_family() {
  local binary=$1 family=$2
  shift 2
  local -a subnets=("$@")
  local chain_rules expected_rules status

  command -v "$binary" >/dev/null 2>&1 || fail "$binary is required to check the $family rules"
  check_source_hooks "$binary" "$family" DOCKER-USER "$IPTABLES_CHAIN" '' '' "${subnets[@]}"
  check_source_hooks "$binary" "$family" INPUT "$IPTABLES_INPUT_CHAIN" "$IPTABLES_CHAIN" \
    "$INPUT_BRIDGE_INTERFACE" "${subnets[@]}"
  check_source_hooks "$binary" "$family" FORWARD "$IPTABLES_CHAIN" '' '' "${subnets[@]}"
  if chain_listing "$binary" "$IPTABLES_CHAIN"; then
    chain_rules=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$IPTABLES_CHAIN" "$status" "$family isolation chain is missing"
  fi

  chain_rules=$(sed -n "/^-A $IPTABLES_CHAIN /p" <<<"$chain_rules")
  expected_rules=$(render_rules "$family" "${subnets[@]}" | sed -n "/^-A $IPTABLES_CHAIN /p")
  [[ "$chain_rules" == "$expected_rules" ]] || fail "$family egress chain is missing rules or has drift"

  if chain_listing "$binary" "$IPTABLES_INPUT_CHAIN"; then
    chain_rules=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$IPTABLES_INPUT_CHAIN" "$status" "$family INPUT chain is missing"
  fi
  chain_rules=$(sed -n "/^-A $IPTABLES_INPUT_CHAIN /p" <<<"$chain_rules")
  expected_rules=$(render_rules "$family" "${subnets[@]}" | sed -n "/^-A $IPTABLES_INPUT_CHAIN /p")
  [[ "$chain_rules" == "$expected_rules" ]] || fail "$family INPUT chain is missing rules or has drift"
}

compose_stack() {
  local active_image_ref=${1:-}
  shift
  local -a image_override=()
  if [[ -n $active_image_ref ]]; then
    image_override=("TEAGRAM_DEMO_IMAGE_REF=$active_image_ref")
  fi
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    -u TG_RSA_KEY_FINGERPRINT \
    "${image_override[@]}" \
    TEAGRAM_DEMO_SECRETS_DIR="$COMPOSE_SECRETS_DIR" \
    docker compose --env-file "$COMPOSE_ENV_FILE" --project-directory "$REPO_ROOT" \
      -f "$COMPOSE_FILE" --project-name teagram-demo "$@"
}

active_release_image_ref() {
  local active_file=${1:-"$REPO_ROOT/.state/teagram-demo/active-release.json"}
  local script_dir
  if [[ ! -e $active_file && ! -L $active_file ]]; then
    printf '\n'
    return 0
  fi
  script_dir=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
  # shellcheck disable=SC2034
  DEMO_DEPLOY_TARGET=lima
  # shellcheck source=../demo-common.sh
  # shellcheck disable=SC1091
  source "$script_dir/../demo-common.sh"
  # shellcheck disable=SC2034
  DEMO_ACTIVE_RELEASE_FILE=$active_file
  demo_active_image_ref
}

disable_existing_restart_policies() {
  local ids
  local -a containers=()
  ids=$(docker ps --all --quiet --filter label=com.docker.compose.project=teagram-demo) \
    || fail 'cannot inspect existing teagram-demo containers'
  [[ -n $ids ]] || return 0
  mapfile -t containers <<<"$ids"
  docker update --restart=no "${containers[@]}" >/dev/null \
    || fail 'cannot disable Docker restart policies for existing teagram-demo containers'
}

stack_start() {
  require_root
  require_repo_path
  stack_lock_try
  (( STACK_LOCK_ACQUIRED == 1 )) || return 2
  stack_start_locked
}

stack_start_locked() {
  local active_image_ref
  active_image_ref=$(active_release_image_ref) \
    || fail 'cannot read the validated active Lima release'
  disable_existing_restart_policies
  compose_stack "$active_image_ref" create --no-build \
    || fail 'cannot create Lima demo containers before applying their egress rules'
  apply_rules
  if [[ -n $active_image_ref ]]; then
    stack_reconcile_locked
  else
    compose_stack "$active_image_ref" up -d --no-build \
      || fail 'cannot start the Lima demo stack after applying its egress rules'
  fi
  stack_lock_release
}

stack_stop() {
  local active_image_ref
  require_root
  require_repo_path
  active_image_ref=$(active_release_image_ref) \
    || fail 'cannot read the validated active Lima release'
  compose_stack "$active_image_ref" --profile candidate stop \
    || fail 'cannot stop the Lima demo stack'
}

stack_lock_try() {
  local state_dir="$REPO_ROOT/.state" lock_path="$REPO_ROOT/.state/deploy.lock" status
  [[ ! -L $state_dir && ! -L $lock_path ]] || fail 'lima deploy lock path must not be a symlink'
  install -d -o root -g root -m 0755 "$state_dir" \
    || fail 'cannot create lima deploy state directory'
  exec 9>>"$lock_path" || fail 'cannot open lima deploy lock'
  if flock -n -s -E 75 9; then
    STACK_LOCK_ACQUIRED=1
    STACK_LOCK_SKIPPED=0
    return 0
  else
    status=$?
  fi
  exec 9>&-
  if (( status == 75 )); then
    STACK_LOCK_ACQUIRED=0
    STACK_LOCK_SKIPPED=1
    return 0
  fi
  fail "cannot acquire a shared lima deploy lock (exit $status)"
}

stack_lock_release() {
  if (( STACK_LOCK_ACQUIRED == 1 )); then
    flock -u 9 || fail 'cannot release the shared lima deploy lock'
    exec 9>&-
    STACK_LOCK_ACQUIRED=0
  fi
}

stack_reconcile_locked() {
  local active_image_ref running_services service
  local -a running_services_list=()
  local -a missing_services=()
  local -A running=()
  local -a expected_services=(postgres turnstile-egress teagramd haproxy edge cloudflared)

  active_image_ref=$(active_release_image_ref) \
    || fail 'cannot read the validated active Lima release'
  running_services=$(compose_stack "$active_image_ref" ps --status running --services) \
    || fail 'cannot inspect running Lima demo services'
  mapfile -t running_services_list <<<"$running_services"
  for service in "${running_services_list[@]}"; do
    [[ -n $service ]] && running[$service]=1
  done
  for service in "${expected_services[@]}"; do
    if [[ -z ${running[$service]:-} ]]; then
      missing_services+=("$service")
    fi
  done
  if (( ${#missing_services[@]} > 0 )); then
    apply_rules
    for service in "${missing_services[@]}"; do
      compose_stack "$active_image_ref" up -d --no-build --no-deps \
        --wait --wait-timeout 120 "$service" \
        || fail "cannot recover stopped Lima demo service: $service"
    done
    printf 'restarted Lima demo services: %s\n' "${missing_services[*]}"
  fi
  stack_lock_release
}

stack_supervise_stop() {
  trap - TERM INT
  printf '%s\n' 'stopping the supervised Lima demo stack'
  stack_stop || fail 'could not stop the supervised Lima demo stack'
  exit 0
}

stack_supervise() {
  local docker_state=unknown
  require_root
  require_repo_path
  trap stack_supervise_stop TERM INT
  while true; do
    stack_lock_try
    if (( STACK_LOCK_SKIPPED == 1 )); then
      sleep 5
      continue
    fi
    stack_start_locked
    break
  done

  while true; do
    if docker_info_ready "$DOCKER_INFO_PROBE_TIMEOUT_SECONDS"; then
      if [[ $docker_state == unavailable ]]; then
        printf '%s\n' 'Docker daemon recovered; checking Lima demo services'
      fi
      docker_state=available
      stack_lock_try
      if (( STACK_LOCK_SKIPPED == 0 )); then
        ensure_firewall_rules_current
        stack_reconcile_locked
      fi
    else
      if [[ $docker_state != unavailable ]]; then
        printf '%s\n' 'Docker daemon is unavailable; waiting before reconciling the Lima demo stack'
      fi
      docker_state=unavailable
    fi
    sleep 10
  done
}

install_openrc_services() {
  local service_file=/etc/init.d/$OPENRC_SERVICE
  local stack_service_file=/etc/init.d/$OPENRC_STACK_SERVICE
  local temp_file

  [[ "$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)" == "$REPO_ROOT" ]] \
    || fail "install from $REPO_ROOT so OpenRC can find the reviewed script"
  command -v rc-update >/dev/null 2>&1 || fail 'rc-update is required on lima'
  temp_file=$(mktemp /etc/init.d/.teagram-demo-egress.XXXXXX)
  cat >"$temp_file" <<'OPENRC'
#!/sbin/openrc-run

description="Install the teagram demo's Docker egress isolation rules"

depend() {
  need docker
  after tailscale
}

start() {
  ebegin "Applying teagram demo egress isolation"
  /opt/teagram-demo/deploy/teagram-demo/lima/install-egress-isolation.sh apply-rules
  eend $?
}
OPENRC
  chown root:root "$temp_file"
  chmod 0755 "$temp_file"
  mv -f "$temp_file" "$service_file"
  temp_file=$(mktemp /etc/init.d/.teagram-demo-stack.XXXXXX)
  cat >"$temp_file" <<'OPENRC'
#!/sbin/openrc-run

description="Start the teagram demo after its egress isolation rules"

depend() {
  need teagram-demo-egress
}

supervisor=supervise-daemon
command=/opt/teagram-demo/deploy/teagram-demo/lima/install-egress-isolation.sh
command_args_foreground="stack-supervise"
respawn_delay=5
respawn_max=0
respawn_period=60
output_logger="logger -t teagram-demo-stack"
error_logger="logger -t teagram-demo-stack"
retry="TERM/30/KILL/5"
OPENRC
  chown root:root "$temp_file"
  chmod 0755 "$temp_file"
  mv -f "$temp_file" "$stack_service_file"
  rc-update add "$OPENRC_SERVICE" default >/dev/null
  rc-update add "$OPENRC_STACK_SERVICE" default >/dev/null
}

require_repo_path() {
  [[ "$(cd -P "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)" == "$REPO_ROOT" ]] \
    || fail "run from $REPO_ROOT so OpenRC can find the reviewed script"
}

apply_rules() {
  require_root
  wait_for_docker
  firewall_lock_acquire
  command -v iptables >/dev/null 2>&1 || fail 'iptables is required on lima'
  command -v iptables-restore >/dev/null 2>&1 || fail 'iptables-restore is required on lima'

  load_ipv6_subnets
  if (( ${#IPV6_SUBNETS[@]} > 0 )); then
    command -v ip6tables >/dev/null 2>&1 || fail 'IPv6 is enabled for a demo network but ip6tables is missing'
    command -v ip6tables-restore >/dev/null 2>&1 \
      || fail 'IPv6 is enabled for a demo network but ip6tables-restore is missing'
  fi

  apply_family iptables ipv4 "${DEMO_IPV4_SUBNETS[@]}"
  if (( ${#MISSING_DEMO_NETWORKS[@]} > 0 )); then
    printf 'IPv6 rules deferred; missing Docker demo networks: %s\n' \
      "${MISSING_DEMO_NETWORKS[*]}" >&2
  elif (( ${#IPV6_SUBNETS[@]} > 0 )); then
    apply_family ip6tables ipv6 "${IPV6_SUBNETS[@]}"
  else
    remove_family ip6tables
  fi
  if (( ${#MISSING_DEMO_NETWORKS[@]} > 0 )); then
    printf '%s\n' 'IPv4 demo egress isolation rules installed; IPv6 rules deferred'
  else
    printf '%s\n' 'demo egress isolation rules installed'
  fi
  firewall_lock_release
}

check_rules() {
  local chain status
  require_root
  check_family iptables ipv4 "${DEMO_IPV4_SUBNETS[@]}"
  load_ipv6_subnets
  if (( ${#MISSING_DEMO_NETWORKS[@]} > 0 )); then
    fail "cannot verify IPv6 rules; missing Docker demo networks: ${MISSING_DEMO_NETWORKS[*]}"
  fi
  if (( ${#IPV6_SUBNETS[@]} > 0 )); then
    check_family ip6tables ipv6 "${IPV6_SUBNETS[@]}"
  elif command -v ip6tables >/dev/null 2>&1; then
    for chain in "$IPTABLES_CHAIN" "$IPTABLES_INPUT_CHAIN"; do
      if chain_listing ip6tables "$chain"; then
        fail 'IPv6 isolation chain is present but no demo network has IPv6 enabled'
      else
        status=$?
        if (( status != 1 )); then
          chain_listing_error ip6tables "$chain" "$status" \
            'IPv6 isolation chain is present but no demo network has IPv6 enabled'
        fi
      fi
    done
  fi
  printf '%s\n' 'demo egress isolation rules are current'
}

ensure_firewall_rules_current() {
  if (check_rules >/dev/null 2>&1); then
    return 0
  fi
  printf '%s\n' 'Firewall hook order drifted; reapplying Lima demo egress rules'
  apply_rules
}

remove_rules() {
  require_root
  firewall_lock_acquire
  remove_family iptables
  remove_family ip6tables
  printf '%s\n' 'demo egress isolation rules removed'
  firewall_lock_release
}

uninstall_openrc_services() {
  local registered_services
  local service
  require_root
  command -v rc-update >/dev/null 2>&1 || fail 'rc-update is required on lima'
  if [[ -x /etc/init.d/$OPENRC_STACK_SERVICE ]]; then
    command -v rc-service >/dev/null 2>&1 || fail 'rc-service is required to stop the Lima stack supervisor'
    rc-service "$OPENRC_STACK_SERVICE" stop \
      || fail 'cannot stop the Lima stack supervisor before uninstalling it'
    stack_stop
  else
    stack_stop
  fi
  registered_services=$(rc-update show default) || fail 'cannot inspect the OpenRC default runlevel'
  for service in "$OPENRC_STACK_SERVICE" "$OPENRC_SERVICE"; do
    if grep -Eq "^[[:space:]]*$service([[:space:]]|\|)" <<<"$registered_services"; then
      rc-update del "$service" default >/dev/null
    fi
    rm -f -- "/etc/init.d/$service"
  done
}

usage() {
  cat <<'USAGE'
Usage:
  install-egress-isolation.sh [install]
  install-egress-isolation.sh apply-rules|check|remove|uninstall
  install-egress-isolation.sh stack-start|stack-stop|stack-supervise
  install-egress-isolation.sh --render-v4 [source-subnet ...]
  install-egress-isolation.sh --render-v6 source-subnet [...]

The default installs the OpenRC boot service and applies the rules. The render
modes print the desired rules without reading or changing a firewall.
USAGE
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  mode=${1:-install}
  case "$mode" in
    install)
      require_root
      require_repo_path
      command -v rc-update >/dev/null 2>&1 || fail 'rc-update is required on lima'
      apply_rules
      disable_existing_restart_policies
      install_openrc_services
      printf '%s\n' 'OpenRC egress isolation and ordered stack services enabled'
      ;;
    apply-rules) apply_rules ;;
    check) check_rules ;;
    remove) remove_rules ;;
    stack-start) stack_start ;;
    stack-stop) stack_stop ;;
    stack-supervise) stack_supervise ;;
    uninstall)
      uninstall_openrc_services
      remove_rules
      ;;
    --render-v4)
      shift
      render_mode ipv4 "$@"
      ;;
    --render-v6)
      shift
      render_mode ipv6 "$@"
      ;;
    -h|--help) usage ;;
    *) usage >&2; fail "unknown mode: $mode" ;;
  esac
fi
