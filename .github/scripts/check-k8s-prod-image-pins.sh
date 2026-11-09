#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'Production K8s image pins: %s\n' "$1" >&2
  exit 1
}

if (($# != 1)) || [[ ! -s "$1" ]]; then
  fail 'pass the rendered production manifest'
fi
manifest="$1"
zero_digest="$(printf '%064d' 0)"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
source_revision_file="$repo_root/deploy/k8s/prod/source-revision"
[[ -s "$source_revision_file" && ! -L "$source_revision_file" ]] ||
  fail 'missing production source revision marker'
production_source_revision="$(tr -d '\r\n' < "$source_revision_file")"
[[ "$production_source_revision" =~ ^[0-9a-f]{40}$ ]] ||
  fail 'production source revision marker is invalid'

image_digest() {
  local image="$1"
  local -a references
  mapfile -t references < <(
    grep --only-matching --extended-regexp \
      "^[[:space:]]*image: ${image}@sha256:[0-9a-f]{64}$" "$manifest" || true
  )
  ((${#references[@]} > 0)) || fail "missing valid digest for ${image}"

  local digest="${references[0]##*@sha256:}"
  [[ "$digest" != "$zero_digest" ]] || fail "${image} uses an all-zero placeholder digest"
  for reference in "${references[@]}"; do
    [[ "$reference" == "${references[0]}" ]] ||
      fail "${image} has inconsistent digests across its production workloads"
  done

  local published_reference="${image}@sha256:${digest}"
  local registry_output
  if ! registry_output="$(docker manifest inspect "$published_reference" 2>&1)"; then
    fail "GHCR could not resolve ${published_reference}: ${registry_output:-no registry response}"
  fi
  printf '%s' "$digest"
}

teagramd_digest="$(image_digest ghcr.io/teagramhq/teagramd)"
migrations_digest="$(image_digest ghcr.io/teagramhq/teagram-migrations)"
rustfs_init_digest="$(image_digest ghcr.io/teagramhq/teagramd-mc-init)"
image_digest ghcr.io/teagramhq/link-edge-landing >/dev/null
image_digest ghcr.io/teagramhq/link-edge-selector >/dev/null

image_revision() {
  local image="$1"
  local digest="$2"
  local reference="${image}@sha256:${digest}"
  local pull_output
  if ! pull_output="$(docker pull --quiet --platform linux/amd64 "$reference" 2>&1)"; then
    fail "could not pull pinned image ${reference}: ${pull_output:-no registry response}"
  fi

  local revision
  if ! revision="$(docker image inspect "$reference" \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' 2>&1)"; then
    fail "could not read the source revision from ${reference}: ${revision:-no image metadata}"
  fi
  [[ "$revision" =~ ^[0-9a-f]{40}$ ]] ||
    fail "pinned image has an invalid source revision: ${revision:-<empty>}"
  printf '%s' "$revision"
}

migration_tree_at_revision() {
  local revision="$1"
  local fetch_output
  if ! git cat-file -e "${revision}^{commit}" 2>/dev/null; then
    if ! fetch_output="$(git fetch --quiet --no-tags --depth=1 origin "$revision" 2>&1)"; then
      fail "could not fetch image source ${revision}: ${fetch_output:-no Git response}"
    fi
  fi

  local migration_tree
  if ! migration_tree="$(git ls-tree -r "$revision" -- migrations 2>&1)"; then
    fail "could not read migrations at image source ${revision}: ${migration_tree:-no Git response}"
  fi
  printf '%s' "$migration_tree"
}

latest_migration_version() {
  local migration_tree="$1"
  local revision="$2"
  local latest=''
  local version
  local migration_path
  while IFS=$'\t' read -r _ migration_path; do
    if [[ "$migration_path" =~ ^migrations/([0-9]{14})_.+\.sql$ ]]; then
      version="${BASH_REMATCH[1]}"
      [[ "$version" > "$latest" ]] && latest="$version"
    fi
  done <<< "$migration_tree"
  [[ "$latest" =~ ^[0-9]{14}$ ]] ||
    fail "image source ${revision} has no valid SQL migrations"
  printf '%s' "$latest"
}

validate_required_migrations() {
  local server_tree="$1"
  local migration_image_tree="$2"
  local server_revision="$3"
  local migrations_revision="$4"
  local migration_entry
  local migration_path
  local server_migrations=0
  while IFS=$'\t' read -r migration_entry migration_path; do
    [[ "$migration_path" =~ ^migrations/[0-9]{14}_.+\.sql$ ]] || continue
    server_migrations=$((server_migrations + 1))
    if ! grep --fixed-strings --line-regexp --quiet -- \
      "${migration_entry}"$'\t'"${migration_path}" <<< "$migration_image_tree"; then
      fail "pinned migration image source ${migrations_revision} lacks the exact migration ${migration_path} required by teagramd image source ${server_revision}"
    fi
  done <<< "$server_tree"
  ((server_migrations > 0)) ||
    fail "teagramd image source ${server_revision} has no valid SQL migrations"
}

teagramd_revision="$(image_revision ghcr.io/teagramhq/teagramd "$teagramd_digest")"
migrations_revision="$(image_revision ghcr.io/teagramhq/teagram-migrations "$migrations_digest")"
rustfs_init_revision="$(image_revision ghcr.io/teagramhq/teagramd-mc-init "$rustfs_init_digest")"
[[ "$teagramd_revision" == "$production_source_revision" ]] ||
  fail "pinned teagramd image source $teagramd_revision does not match production source revision $production_source_revision"
[[ "$migrations_revision" == "$production_source_revision" ]] ||
  fail "pinned migration image source $migrations_revision does not match production source revision $production_source_revision"
[[ "$rustfs_init_revision" == "$production_source_revision" ]] ||
  fail "pinned S3 client image source $rustfs_init_revision does not match production source revision $production_source_revision"
teagramd_migration_tree="$(migration_tree_at_revision "$teagramd_revision")"
migrations_image_tree="$(migration_tree_at_revision "$migrations_revision")"
migrations_image_version="$(latest_migration_version "$migrations_image_tree" "$migrations_revision")"
validate_required_migrations \
  "$teagramd_migration_tree" "$migrations_image_tree" \
  "$teagramd_revision" "$migrations_revision"

if ! production_migration_version="$(yq -r \
  'select(.kind == "ConfigMap" and (.metadata.name | test("^required-migration-version-"))) | .data["migration-version"]' \
  "$manifest" 2>&1)"; then
  fail "could not read production migration version from rendered manifest: ${production_migration_version:-no YAML response}"
fi
[[ "$production_migration_version" =~ ^[0-9]{14}$ ]] ||
  fail "rendered production migration version is invalid: ${production_migration_version:-<empty>}"
[[ "$production_migration_version" == "$migrations_image_version" ]] ||
  fail "production migration version ${production_migration_version} does not match the newest migration in pinned image source ${migrations_revision} (${migrations_image_version})"

grep --fixed-strings --line-regexp --quiet \
  "  name: db-migrations-${migrations_digest:0:16}" "$manifest" ||
  fail 'migration Job name does not match the migration image digest'
grep --fixed-strings --line-regexp --quiet \
  "  name: rustfs-init-${rustfs_init_digest:0:16}" "$manifest" ||
  fail 'RustFS initializer Job name does not match the init image digest'

printf 'Production K8s image pins passed\n'
