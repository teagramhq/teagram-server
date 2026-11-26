#!/usr/bin/env bash
set -Eeuo pipefail

readonly IPTABLES_CHAIN=TEAGRAM_DEMO_EGRESS
readonly OPENRC_SERVICE=teagram-demo-egress
readonly REPO_ROOT=/opt/teagram-demo
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
  local source_list destination_list subnet cidr
  local -a blocked_cidrs

  (( $# > 0 )) || fail 'at least one source subnet is required'
  source_list=$(IFS=,; printf '%s' "$*")
  destination_list=$source_list

  case "$family" in
    ipv4)
      blocked_cidrs=("${BLOCKED_IPV4_CIDRS[@]}")
      ;;
    ipv6)
      blocked_cidrs=("${BLOCKED_IPV6_CIDRS[@]}")
      ;;
    *) fail "unknown address family: $family" ;;
  esac

  printf '%s\n' "-A DOCKER-USER -s $source_list -j $IPTABLES_CHAIN"
  printf '%s\n' "-A $IPTABLES_CHAIN -d $destination_list -j RETURN"
  for cidr in "${blocked_cidrs[@]}"; do
    printf '%s\n' "-A $IPTABLES_CHAIN -d $cidr -j DROP"
  done
  printf '%s\n' "-A $IPTABLES_CHAIN -m conntrack --ctstate RELATED,ESTABLISHED -j RETURN"
  printf '%s\n' "-A $IPTABLES_CHAIN -j RETURN"
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

  command -v docker >/dev/null 2>&1 || fail 'docker is required to inspect demo network IPv6 settings'
  local network_names
  network_names=$(docker network ls --format '{{.Name}}') || fail 'cannot list Docker networks'

  for network in "${DEMO_NETWORKS[@]}"; do
    if ! grep -Fqx -- "$network" <<<"$network_names"; then
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

apply_family() {
  local binary=$1 family=$2
  shift 2
  local -a subnets=("$@")
  local line source_list
  source_list=$(IFS=,; printf '%s' "${subnets[*]}")

  "$binary" -w 10 -S DOCKER-USER >/dev/null 2>&1 \
    || fail "$binary has no DOCKER-USER chain; start Docker before installing the rules"
  "$binary" -w 10 -C FORWARD -j DOCKER-USER >/dev/null 2>&1 \
    || fail "$binary has no FORWARD hook to DOCKER-USER"
  if ! "$binary" -w 10 -S "$IPTABLES_CHAIN" >/dev/null 2>&1; then
    "$binary" -w 10 -N "$IPTABLES_CHAIN"
  fi
  {
    printf '*filter\n-F %s\n' "$IPTABLES_CHAIN"
    while IFS= read -r line; do
      [[ "$line" == "-A $IPTABLES_CHAIN "* ]] || continue
      printf '%s\n' "$line"
    done < <(render_rules "$family" "${subnets[@]}")
    printf 'COMMIT\n'
  } | "${binary}-restore" -w 10 --noflush

  ensure_user_chain_hook "$binary" "$source_list"
}

ensure_user_chain_hook() {
  local binary=$1 source_list=$2 line hook_count=0 first_user_rule=
  local expected_hook="-A DOCKER-USER -s $source_list -j $IPTABLES_CHAIN"
  local -a user_rules=()

  mapfile -t user_rules < <("$binary" -w 10 -S DOCKER-USER | sed -n '/^-A DOCKER-USER /p')
  for line in "${user_rules[@]}"; do
    [[ -n "$first_user_rule" ]] || first_user_rule=$line
    [[ "$line" == "$expected_hook" ]] && ((hook_count += 1))
  done

  if (( hook_count == 0 )) || [[ "$first_user_rule" != "$expected_hook" ]]; then
    "$binary" -w 10 -I DOCKER-USER 1 -s "$source_list" -j "$IPTABLES_CHAIN"
  fi

  mapfile -t user_rules < <("$binary" -w 10 -S DOCKER-USER | sed -n '/^-A DOCKER-USER /p')
  for ((index=${#user_rules[@]}-1; index >= 1; index--)); do
    if [[ "${user_rules[$index]}" == *" -j $IPTABLES_CHAIN" ]]; then
      "$binary" -w 10 -D DOCKER-USER "$((index + 1))"
    fi
  done
}

remove_family() {
  local binary=$1
  if ! command -v "$binary" >/dev/null 2>&1; then
    return 0
  fi
  if "$binary" -w 10 -S DOCKER-USER >/dev/null 2>&1; then
    local -a user_rules=()
    mapfile -t user_rules < <("$binary" -w 10 -S DOCKER-USER | sed -n '/^-A DOCKER-USER /p')
    for ((index=${#user_rules[@]}-1; index >= 0; index--)); do
      if [[ "${user_rules[$index]}" == *" -j $IPTABLES_CHAIN" ]]; then
        "$binary" -w 10 -D DOCKER-USER "$((index + 1))"
      fi
    done
  fi
  if "$binary" -w 10 -S "$IPTABLES_CHAIN" >/dev/null 2>&1; then
    "$binary" -w 10 -F "$IPTABLES_CHAIN"
    "$binary" -w 10 -X "$IPTABLES_CHAIN"
  fi
}

check_family() {
  local binary=$1 family=$2
  shift 2
  local -a subnets=("$@")
  local user_rules chain_rules expected_rules source_list expected_hook line
  local hook_count=0 chain_hook_count=0 first_user_rule=

  if ! command -v "$binary" >/dev/null 2>&1; then
    fail "$binary is required to check the $family rules"
  fi
  if ! user_rules=$("$binary" -w 10 -S DOCKER-USER 2>/dev/null); then
    fail "$binary is missing DOCKER-USER for $family"
  fi
  "$binary" -w 10 -C FORWARD -j DOCKER-USER >/dev/null 2>&1 \
    || fail "$family FORWARD hook to DOCKER-USER is missing"
  if ! chain_rules=$("$binary" -w 10 -S "$IPTABLES_CHAIN" 2>/dev/null); then
    fail "$family isolation chain is missing"
  fi
  source_list=$(IFS=,; printf '%s' "${subnets[*]}")
  expected_hook="-A DOCKER-USER -s $source_list -j $IPTABLES_CHAIN"

  while IFS= read -r line; do
    [[ "$line" == -A\ DOCKER-USER\ * ]] || continue
    [[ -n "$first_user_rule" ]] || first_user_rule=$line
    [[ "$line" == *" -j $IPTABLES_CHAIN" ]] && ((chain_hook_count += 1))
    [[ "$line" == "$expected_hook" ]] && ((hook_count += 1))
  done <<<"$user_rules"
  [[ "$hook_count" -eq 1 && "$chain_hook_count" -eq 1 && "$first_user_rule" == "$expected_hook" ]] \
    || fail "$family DOCKER-USER hook is missing, duplicated, or out of order"

  chain_rules=$(sed -n '/^-A /p' <<<"$chain_rules")
  expected_rules=$(render_rules "$family" "${subnets[@]}" | sed -n "/^-A $IPTABLES_CHAIN /p")
  [[ "$chain_rules" == "$expected_rules" ]] || fail "$family isolation chain is missing rules or has drift"
}

install_openrc_service() {
  local service_file=/etc/init.d/$OPENRC_SERVICE
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
  rc-update add "$OPENRC_SERVICE" default >/dev/null
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
    ip6tables -w 10 -S DOCKER-USER >/dev/null 2>&1 \
      || fail 'ip6tables has no DOCKER-USER chain; start Docker before installing the rules'
    ip6tables -w 10 -C FORWARD -j DOCKER-USER >/dev/null 2>&1 \
      || fail 'ip6tables has no FORWARD hook to DOCKER-USER'
  fi

  apply_family iptables ipv4 "${DEMO_IPV4_SUBNETS[@]}"
  if (( ${#IPV6_SUBNETS[@]} > 0 )); then
    apply_family ip6tables ipv6 "${IPV6_SUBNETS[@]}"
  else
    remove_family ip6tables
  fi
  printf '%s\n' 'demo egress isolation rules installed'
}

check_rules() {
  require_root
  check_family iptables ipv4 "${DEMO_IPV4_SUBNETS[@]}"
  load_ipv6_subnets
  if (( ${#IPV6_SUBNETS[@]} > 0 )); then
    check_family ip6tables ipv6 "${IPV6_SUBNETS[@]}"
  elif command -v ip6tables >/dev/null 2>&1 && ip6tables -w 10 -S "$IPTABLES_CHAIN" >/dev/null 2>&1; then
    fail 'IPv6 isolation chain is present but no demo network has IPv6 enabled'
  fi
  printf '%s\n' 'demo egress isolation rules are current'
}

remove_rules() {
  require_root
  remove_family iptables
  remove_family ip6tables
  printf '%s\n' 'demo egress isolation rules removed'
}

uninstall_openrc_service() {
  local registered_services
  require_root
  command -v rc-update >/dev/null 2>&1 || fail 'rc-update is required on lima'
  registered_services=$(rc-update show default) || fail 'cannot inspect the OpenRC default runlevel'
  if grep -Eq "^[[:space:]]*$OPENRC_SERVICE([[:space:]]|\|)" <<<"$registered_services"; then
    rc-update del "$OPENRC_SERVICE" default >/dev/null
  fi
  rm -f -- "/etc/init.d/$OPENRC_SERVICE"
}

usage() {
  cat <<'USAGE'
Usage:
  install-egress-isolation.sh [install]
  install-egress-isolation.sh apply-rules|check|remove|uninstall
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
    install_openrc_service
    printf '%s\n' 'OpenRC egress isolation service enabled'
    ;;
  apply-rules) apply_rules ;;
  check) check_rules ;;
  remove) remove_rules ;;
  uninstall)
    uninstall_openrc_service
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
