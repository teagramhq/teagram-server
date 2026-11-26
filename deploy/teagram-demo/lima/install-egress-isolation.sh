#!/usr/bin/env bash
set -Eeuo pipefail

readonly IPTABLES_CHAIN=TEAGRAM_DEMO_EGRESS
readonly IPTABLES_INPUT_CHAIN=TEAGRAM_DEMO_INPUT
readonly OPENRC_SERVICE=teagram-demo-egress
readonly OPENRC_STACK_SERVICE=teagram-demo-stack
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

fail() {
  printf 'demo egress isolation: %s\n' "$1" >&2
  exit 1
}

require_root() {
  [[ "$EUID" -eq 0 ]] || fail 'must run as root'
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
    printf '%s\n' "-A DOCKER-USER -s $subnet -j $IPTABLES_CHAIN"
    printf '%s\n' "-A INPUT -s $subnet -j $IPTABLES_INPUT_CHAIN"
  done
  for subnet in "$@"; do
    printf '%s\n' "-A $IPTABLES_CHAIN -d $subnet -j RETURN"
  done
  printf '%s\n' "-A $IPTABLES_CHAIN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN"
  for cidr in "${blocked_cidrs[@]}"; do
    printf '%s\n' "-A $IPTABLES_CHAIN -d $cidr -j DROP"
  done
  printf '%s\n' "-A $IPTABLES_CHAIN -j RETURN"

  printf '%s\n' "-A $IPTABLES_INPUT_CHAIN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN"
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

require_firewall_rule() {
  local description=$1 first_line output status
  shift
  if output=$("$@" 2>&1); then
    return 0
  else
    status=$?
  fi
  first_line=${output%%$'\n'*}
  [[ -n $first_line ]] || first_line='no diagnostic was returned'
  fail "$description (exit $status): $first_line"
}

apply_family() {
  local binary=$1 family=$2
  shift 2
  local -a subnets=("$@")
  local line status chain

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
  require_firewall_rule "$binary has no FORWARD hook to DOCKER-USER" \
    "$binary" -w 10 -C FORWARD -j DOCKER-USER
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
    done < <(render_rules "$family" "${subnets[@]}")
    printf 'COMMIT\n'
  } | "${binary}-restore" -w 10 --noflush

  ensure_source_hooks "$binary" DOCKER-USER "$IPTABLES_CHAIN" '' "${subnets[@]}"
  ensure_source_hooks "$binary" INPUT "$IPTABLES_INPUT_CHAIN" "$IPTABLES_CHAIN" "${subnets[@]}"
}

ensure_source_hooks() {
  local binary=$1 parent_chain=$2 hook_chain=$3 stale_chain=$4
  local subnet index prefix_matches=1 listing status
  shift 4
  local -a subnets=("$@")
  local -a expected_hooks=() parent_rules=()

  for subnet in "${subnets[@]}"; do
    expected_hooks+=("-A $parent_chain -s $subnet -j $hook_chain")
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

  if (( prefix_matches == 0 )); then
    for ((index = ${#subnets[@]} - 1; index >= 0; index--)); do
      "$binary" -w 10 -I "$parent_chain" 1 -s "${subnets[$index]}" -j "$hook_chain"
    done
  fi

  if chain_listing "$binary" "$parent_chain"; then
    listing=$CHAIN_LISTING
  else
    status=$?
    chain_listing_error "$binary" "$parent_chain" "$status" "$binary is missing $parent_chain"
  fi
  mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")
  for ((index = ${#parent_rules[@]} - 1; index >= ${#expected_hooks[@]}; index--)); do
    if [[ "${parent_rules[$index]}" == *" -j $hook_chain" ]] \
      || [[ -n $stale_chain && "${parent_rules[$index]}" == *" -j $stale_chain" ]]; then
      "$binary" -w 10 -D "$parent_chain" "$((index + 1))"
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
  for parent_chain in DOCKER-USER INPUT; do
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

  for parent_chain in DOCKER-USER INPUT; do
    listing=${parent_listings[$parent_chain]}
    mapfile -t parent_rules < <(sed -n "/^-A $parent_chain /p" <<<"$listing")
    for ((index = ${#parent_rules[@]} - 1; index >= 0; index--)); do
      for managed_chain in "${managed_chains[@]}"; do
        if [[ "${parent_rules[$index]}" == *" -j $managed_chain" ]]; then
          "$binary" -w 10 -D "$parent_chain" "$((index + 1))"
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
  local binary=$1 family=$2 parent_chain=$3 hook_chain=$4 stale_chain=$5
  local listing subnet line index hook_count=0 status
  shift 5
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
    expected_hooks+=("-A $parent_chain -s $subnet -j $hook_chain")
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
  check_source_hooks "$binary" "$family" DOCKER-USER "$IPTABLES_CHAIN" '' "${subnets[@]}"
  check_source_hooks "$binary" "$family" INPUT "$IPTABLES_INPUT_CHAIN" "$IPTABLES_CHAIN" "${subnets[@]}"
  require_firewall_rule "$family FORWARD hook to DOCKER-USER is missing" \
    "$binary" -w 10 -C FORWARD -j DOCKER-USER
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
  env -u COMPOSE_FILE -u COMPOSE_PROJECT_NAME -u COMPOSE_PROFILES \
    -u POSTGRES_PASSWORD -u TG_PUBLIC_LINK_PREFIX -u TG_AUTHKEY_ENC_KEY \
    -u TEAGRAM_DEMO_TURNSTILE_SITE_KEY -u TEAGRAM_DEMO_IMAGE_REF \
    -u TG_RSA_KEY_FINGERPRINT \
    TEAGRAM_DEMO_SECRETS_DIR="$COMPOSE_SECRETS_DIR" \
    docker compose --env-file "$COMPOSE_ENV_FILE" --project-directory "$REPO_ROOT" \
      -f "$COMPOSE_FILE" --project-name teagram-demo "$@"
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
  disable_existing_restart_policies
  compose_stack create --no-build
  apply_rules
  compose_stack up -d --no-build
}

stack_stop() {
  require_root
  require_repo_path
  compose_stack --profile candidate stop
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

start() {
  ebegin "Creating demo networks, applying isolation, then starting the stack"
  /opt/teagram-demo/deploy/teagram-demo/lima/install-egress-isolation.sh stack-start
  eend $?
}

stop() {
  ebegin "Stopping the teagram demo stack"
  /opt/teagram-demo/deploy/teagram-demo/lima/install-egress-isolation.sh stack-stop
  eend $?
}
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

remove_rules() {
  require_root
  remove_family iptables
  remove_family ip6tables
  printf '%s\n' 'demo egress isolation rules removed'
}

uninstall_openrc_services() {
  local registered_services
  local service
  require_root
  command -v rc-update >/dev/null 2>&1 || fail 'rc-update is required on lima'
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
  install-egress-isolation.sh stack-start|stack-stop (OpenRC stack service)
  install-egress-isolation.sh --render-v4 [source-subnet ...]
  install-egress-isolation.sh --render-v6 source-subnet [...]

The default installs the OpenRC boot service and applies the rules. The render
modes print the desired rules without reading or changing a firewall.
USAGE
}

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
  uninstall)
    stack_stop
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
