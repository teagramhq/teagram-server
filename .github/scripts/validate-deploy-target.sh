#!/usr/bin/env bash
set -euo pipefail

if (($# != 8)); then
  echo '::error::Expected variable names and values for host, port, known hosts, and user' >&2
  exit 2
fi

host_name=$1
host=$2
port_name=$3
port=$4
known_hosts_name=$5
known_hosts=$6
user_name=$7
user=$8

invalid_empty() {
  echo "::error::$1 is empty" >&2
  exit 1
}

invalid_value() {
  echo "::error::$1 is malformed" >&2
  exit 1
}

valid_host() {
  local host_value=$1
  local hostname_pattern='^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$'
  (( ${#host_value} <= 253 )) && [[ "$host_value" =~ $hostname_pattern ]]
}

valid_port() {
  [[ "$1" =~ ^[0-9]{1,5}$ ]] && (( 10#$1 >= 1 && 10#$1 <= 65535 ))
}

valid_known_hosts() {
  local value=$1
  local expected_host=$2
  local expected_port=$3
  local endpoint='' key_type='' key_data='' extra=''
  local key_file

  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  read -r endpoint key_type key_data extra <<< "$value"
  [[ -z "$extra" && "$value" == "$endpoint $key_type $key_data" ]] || return 1
  [[ "$endpoint" == "[$expected_host]:$expected_port" ]] || return 1
  case "$key_type" in
    ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521) ;;
    *) return 1 ;;
  esac
  [[ "$key_data" =~ ^[A-Za-z0-9+/]+={0,2}$ ]] || return 1
  printf '%s' "$key_data" | base64 --decode >/dev/null 2>&1 || return 1
  key_file=$(mktemp) || return 1
  if ! printf '%s %s\n' "$key_type" "$key_data" > "$key_file" || ! ssh-keygen -l -f "$key_file" >/dev/null 2>&1; then
    rm -f -- "$key_file"
    return 1
  fi
  rm -f -- "$key_file"
}

[[ -n "$host" ]] || invalid_empty "$host_name"
valid_host "$host" || invalid_value "$host_name"
[[ -n "$port" ]] || invalid_empty "$port_name"
valid_port "$port" || invalid_value "$port_name"
[[ -n "$known_hosts" ]] || invalid_empty "$known_hosts_name"
valid_known_hosts "$known_hosts" "$host" "$port" || invalid_value "$known_hosts_name"
[[ -n "$user" ]] || invalid_empty "$user_name"
[[ "$user" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]] || invalid_value "$user_name"
