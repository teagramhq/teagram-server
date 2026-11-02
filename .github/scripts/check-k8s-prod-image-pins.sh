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
  printf '%s' "$digest"
}

image_digest ghcr.io/teagramhq/teagramd >/dev/null
migrations_digest="$(image_digest ghcr.io/teagramhq/teagram-migrations)"
rustfs_init_digest="$(image_digest ghcr.io/teagramhq/teagramd-mc-init)"
image_digest ghcr.io/teagramhq/link-edge-landing >/dev/null
image_digest ghcr.io/teagramhq/link-edge-selector >/dev/null

grep --fixed-strings --line-regexp --quiet \
  "  name: db-migrations-${migrations_digest:0:16}" "$manifest" ||
  fail 'migration Job name does not match the migration image digest'
grep --fixed-strings --line-regexp --quiet \
  "  name: rustfs-init-${rustfs_init_digest:0:16}" "$manifest" ||
  fail 'RustFS initializer Job name does not match the init image digest'

printf 'Production K8s image pins passed\n'
