#!/usr/bin/env bash

alpha_mktemp_file() {
  local directory=$1 prefix=$2
  mktemp "$directory/$prefix.XXXXXX"
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
    [ -d "$directory" ] || install -d -o root -g root -m 0755 "$directory"
    temporary=$(mktemp "${destination}.XXXXXX")
    trap "rm -f -- \"$temporary\"" EXIT
    cat > "$temporary"
    chown root:root "$temporary"
    chmod "$mode" "$temporary"
    mv -f -- "$temporary" "$destination"
  ' sh "$destination" "$mode"
}
