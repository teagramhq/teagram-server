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

image_digest ghcr.io/teagramhq/teagramd >/dev/null
migrations_digest="$(image_digest ghcr.io/teagramhq/teagram-migrations)"
rustfs_init_digest="$(image_digest ghcr.io/teagramhq/teagramd-mc-init)"
image_digest ghcr.io/teagramhq/link-edge-landing >/dev/null
image_digest ghcr.io/teagramhq/link-edge-selector >/dev/null

migrations_reference="ghcr.io/teagramhq/teagram-migrations@sha256:${migrations_digest}"
if ! pull_output="$(docker pull --quiet --platform linux/amd64 "$migrations_reference" 2>&1)"; then
  fail "could not pull pinned migration image ${migrations_reference}: ${pull_output:-no registry response}"
fi
if ! migrations_revision="$(docker image inspect "$migrations_reference" \
  --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' 2>&1)"; then
  fail "could not read the source revision from ${migrations_reference}: ${migrations_revision:-no image metadata}"
fi
[[ "$migrations_revision" =~ ^[0-9a-f]{40}$ ]] ||
  fail "pinned migration image has an invalid source revision: ${migrations_revision:-<empty>}"

if ! git cat-file -e "${migrations_revision}^{commit}" 2>/dev/null; then
  if ! fetch_output="$(git fetch --quiet --no-tags --depth=1 origin "$migrations_revision" 2>&1)"; then
    fail "could not fetch pinned migration image source ${migrations_revision}: ${fetch_output:-no Git response}"
  fi
fi
if ! source_migration_version="$(git show \
  "${migrations_revision}:deploy/k8s/base/migration-version" 2>&1)"; then
  fail "could not read migration-version at pinned image source ${migrations_revision}: ${source_migration_version:-no file}"
fi
source_migration_version="$(printf '%s' "$source_migration_version" | tr -d '\r\n')"
if ! production_migration_version="$(yq -r \
  'select(.kind == "ConfigMap" and (.metadata.name | test("^required-migration-version-"))) | .data["migration-version"]' \
  "$manifest" 2>&1)"; then
  fail "could not read production migration version from rendered manifest: ${production_migration_version:-no YAML response}"
fi
[[ "$production_migration_version" =~ ^[0-9]+$ ]] ||
  fail "rendered production migration version is invalid: ${production_migration_version:-<empty>}"
[[ "$source_migration_version" == "$production_migration_version" ]] ||
  fail "production migration version ${production_migration_version} does not match pinned migration image source ${migrations_revision} (${source_migration_version:-<empty>})"

grep --fixed-strings --line-regexp --quiet \
  "  name: db-migrations-${migrations_digest:0:16}" "$manifest" ||
  fail 'migration Job name does not match the migration image digest'
grep --fixed-strings --line-regexp --quiet \
  "  name: rustfs-init-${rustfs_init_digest:0:16}" "$manifest" ||
  fail 'RustFS initializer Job name does not match the init image digest'

printf 'Production K8s image pins passed\n'
