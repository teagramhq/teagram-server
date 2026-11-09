#!/usr/bin/env bash

alpha_mktemp_file() {
  local directory=$1 prefix=$2
  mktemp "$directory/$prefix.XXXXXX"
}

alpha_mktemp_dir() {
  local directory=$1 prefix=$2
  mktemp -d "$directory/$prefix.XXXXXX"
}

alpha_remote_command_exists() {
  local instance=$1 command_name=$2
  # shellcheck disable=SC2016
  incus exec "$instance" -- sh -c 'command -v "$1" >/dev/null 2>&1' sh "$command_name"
}

alpha_remote_file_matches() {
  local instance=$1 destination=$2 source_file=$3
  incus exec --mode=non-interactive "$instance" -- cmp -s "$destination" - < "$source_file"
}

alpha_install_remote_file() {
  local instance=$1 destination=$2 mode=$3
  # shellcheck disable=SC2016
  incus exec "$instance" -- sh -c '
    set -eu
    destination=$1
    mode=$2
    directory=${destination%/*}
    install -d -o root -g root "$directory"
    temporary=$(mktemp "${destination}.XXXXXX")
    trap "rm -f -- \"$temporary\"" EXIT
    cat > "$temporary"
    chown root:root "$temporary"
    chmod "$mode" "$temporary"
    mv -f -- "$temporary" "$destination"
  ' sh "$destination" "$mode"
}

alpha_ensure_sshd_runtime_dir() {
  local instance=$1
  incus exec "$instance" -- install -d -o root -g root -m 0755 /run/sshd
}

alpha_for_each_line() {
  local lines=$1 callback=$2 line
  shift 2
  while IFS= read -r line <&3; do
    "$callback" "$line" "$@"
  done 3<<< "$lines"
}
