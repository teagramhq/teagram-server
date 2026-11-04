#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

plan="$(deploy/k8s/cutover-alpha.sh --dry-run)"
for required in \
  'Would server-dry-run and apply deploy/k8s/prod-cutover' \
  'fence writes before the final PostgreSQL snapshot' \
  'stop all running Compose services except PostgreSQL' \
  'verify no PostgreSQL client connections remain' \
  'save a mode-0600 pg_dump' \
  '8 GiB PostgreSQL PVC can hold both the protected dump and restored database' \
  'server_key.pem, enc_key.hex, and local blobs' \
  'encrypt and checksum-copy its objects to RustFS' \
  'all five tailnet-only Serve handlers' \
  'planned downtime from the write fence until all edges switch' \
  'Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081' \
  'verify every k8s cloudflared pod has stopped before restoring routes' \
  'k3s writes since the switch are not carried back'; do
  if [[ $plan != *"$required"* ]]; then
    printf 'alpha cutover dry-run omitted: %s\n' "$required" >&2
    exit 1
  fi
done

cutover_image='ghcr.io/teagramhq/teagramd@sha256:e307d5d93ab000d61dc77251a599b00e031e6dac7cdf026ef8d2d621734e7c3f'
cutover_digest=${cutover_image#*@}
if ! rg --fixed-strings --quiet "digest: $cutover_digest" deploy/k8s/prod-cutover/kustomization.yaml; then
  printf 'alpha cutover does not pin the CI-verified blob-migration image\n' >&2
  exit 1
fi

for path in \
  '/apiws' \
  '/admin' \
  '/.well-known/teagramd/client' \
  '/.well-known/telegramd/client'; do
  if ! rg --fixed-strings --quiet "$path" deploy/k8s/cutover-alpha.sh; then
    printf 'alpha cutover does not route Serve path: %s\n' "$path" >&2
    exit 1
  fi
done

if ! rg --quiet 'backend_type.*client backend.*pg_backend_pid' deploy/k8s/cutover-alpha.sh; then
  printf 'alpha cutover does not verify source PostgreSQL client sessions are fenced\n' >&2
  exit 1
fi

if ! rg --fixed-strings --quiet 'TG_ADMIN_ORIGIN' deploy/k8s/cutover-alpha.sh ||
  ! rg --fixed-strings --quiet 'TG_ADMIN_ORIGIN' deploy/k8s/prod-cutover/teagramd-cutover-listeners.yaml; then
  printf 'alpha cutover does not preserve the existing admin origin configuration\n' >&2
  exit 1
fi

if [[ $plan == *'TUNNEL_TOKEN'* || $plan == *'POSTGRES_PASSWORD'* || $plan == *'server_key.pem SHA'* ]]; then
  printf 'alpha cutover dry-run exposed a secret identifier or key fingerprint\n' >&2
  exit 1
fi

printf 'alpha cutover dry-run contract passed\n'
