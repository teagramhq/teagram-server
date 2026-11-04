#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'K8s migration version: %s\n' "$1" >&2
  exit 1
}

if (($# != 3)) || [[ ! -d "$1" || ! -s "$2" ]]; then
  fail 'pass the migrations directory, base migration-version file, and previous base version'
fi
migrations_dir="$1"
version_file="$2"
previous_version="$3"
required_version="$(tr -d '\r\n' < "$version_file")"
[[ "$required_version" =~ ^[0-9]{14}$ ]] ||
  fail "base migration version is invalid: ${required_version:-<empty>}"
[[ "$previous_version" =~ ^[0-9]{14}$ ]] ||
  fail "previous base migration version is invalid: ${previous_version:-<empty>}"

shopt -s nullglob
migrations=("$migrations_dir/${required_version}_"*.sql)
((${#migrations[@]} > 0)) ||
  fail "base migration version ${required_version} does not exist in ${migrations_dir}"

previous_migrations=("$migrations_dir/${previous_version}_"*.sql)
((${#previous_migrations[@]} > 0)) ||
  fail "previous base migration version ${previous_version} does not exist in ${migrations_dir}"

if [[ "$required_version" != "$previous_version" ]]; then
  versions=()
  for migration in "$migrations_dir"/*.sql; do
    filename="${migration##*/}"
    if [[ "$filename" =~ ^([0-9]{14})_.+\.sql$ ]]; then
      versions+=("${BASH_REMATCH[1]}")
    fi
  done
  ((${#versions[@]} > 0)) || fail "no valid SQL migrations found in ${migrations_dir}"
  latest_version="$(printf '%s\n' "${versions[@]}" | sort | tail -n 1)"
  [[ "$required_version" == "$latest_version" ]] ||
    fail "changed base migration version ${required_version} must match newest migration ${latest_version}"
fi

printf 'Base K8s migration version %s exists in %s\n' "$required_version" "$migrations_dir"
