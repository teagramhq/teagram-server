#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'K8s migration version: %s\n' "$1" >&2
  exit 1
}

if (($# != 2)) || [[ ! -d "$1" || ! -s "$2" ]]; then
  fail 'pass the migrations directory and base migration-version file'
fi
migrations_dir="$1"
version_file="$2"
required_version="$(tr -d '\r\n' < "$version_file")"
[[ "$required_version" =~ ^[0-9]{14}$ ]] ||
  fail "base migration version is invalid: ${required_version:-<empty>}"

shopt -s nullglob
migrations=("$migrations_dir/${required_version}_"*.sql)
((${#migrations[@]} > 0)) ||
  fail "base migration version ${required_version} does not exist in ${migrations_dir}"

printf 'Base K8s migration version %s exists in %s\n' "$required_version" "$migrations_dir"
