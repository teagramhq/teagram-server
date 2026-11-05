#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'K8s CI migration version: %s\n' "$1" >&2
  exit 1
}

if (($# != 2)) || [[ ! -d "$1" || ! -s "$2/kustomization.yaml" ]]; then
  fail 'pass the migrations directory and CI overlay directory'
fi
migrations_dir="$1"
overlay_dir="$2"
overlay_file="$overlay_dir/kustomization.yaml"

versions="$(find "$migrations_dir" -maxdepth 1 -type f -name '*.sql' -printf '%f\n' \
  | sed -nE 's/^([0-9]{14})_.+\.sql$/\1/p' \
  | sort)"
[[ -n "$versions" ]] || fail "no valid SQL migrations found in ${migrations_dir}"
latest_version="$(tail -n 1 <<< "$versions")"
[[ "$latest_version" =~ ^[0-9]{14}$ ]] ||
  fail "newest migration version is invalid: ${latest_version:-<empty>}"

generator_count="$(yq -r '[.configMapGenerator[] | select(.name == "required-migration-version")] | length' "$overlay_file")"
[[ "$generator_count" == 1 ]] ||
  fail 'the CI overlay must define one required-migration-version generator'
literal_count="$(yq -r '[.configMapGenerator[] | select(.name == "required-migration-version") | .literals[]] | length' "$overlay_file")"
[[ "$literal_count" == 1 ]] ||
  fail 'the CI overlay must define one required migration-version literal'

LATEST_MIGRATION_VERSION="$latest_version" yq -i \
  '(.configMapGenerator[] | select(.name == "required-migration-version").literals[0]) = "migration-version=" + strenv(LATEST_MIGRATION_VERSION)' \
  "$overlay_file"
configured_version="$(yq -r '.configMapGenerator[] | select(.name == "required-migration-version").literals[0] | sub("^migration-version="; "")' "$overlay_file")"
[[ "$configured_version" == "$latest_version" ]] ||
  fail "CI overlay migration version ${configured_version} does not match newest migration ${latest_version}"

printf 'Set CI overlay migration version to %s\n' "$latest_version"
