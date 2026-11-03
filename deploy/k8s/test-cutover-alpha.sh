#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo_root"

plan="$(deploy/k8s/cutover-alpha.sh --dry-run)"
for required in \
  'Would server-dry-run and apply deploy/k8s/prod-cutover' \
  'save a mode-0600 pg_dump' \
  '8 GiB PostgreSQL PVC can hold both the protected dump and restored database' \
  'server_key.pem, enc_key.hex, and local blobs' \
  'encrypt and checksum-copy its objects to RustFS' \
  'Cloudflare dashboard change http://selector:8081 -> http://linkselector:8081' \
  'k8s cloudflared=0, Cloudflare route back to http://selector:8081'; do
  if [[ $plan != *"$required"* ]]; then
    printf 'alpha cutover dry-run omitted: %s\n' "$required" >&2
    exit 1
  fi
done

if [[ $plan == *'TUNNEL_TOKEN'* || $plan == *'POSTGRES_PASSWORD'* || $plan == *'server_key.pem SHA'* ]]; then
  printf 'alpha cutover dry-run exposed a secret identifier or key fingerprint\n' >&2
  exit 1
fi

printf 'alpha cutover dry-run contract passed\n'
