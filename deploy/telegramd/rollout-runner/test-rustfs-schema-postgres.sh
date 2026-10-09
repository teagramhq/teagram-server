#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
NETWORK="rustfs-schema-${BASHPID}"
POSTGRES_CONTAINER="rustfs-schema-pg-${BASHPID}"
POSTGRES_IMAGE="mirror.gcr.io/library/postgres:16-alpine"
ATLAS_IMAGE="mirror.gcr.io/arigaio/atlas:1.2.0-alpine"
ATLAS_INPUT=$(mktemp -d "${TMPDIR:-/tmp}/rustfs-schema-atlas.XXXXXX")
chmod 700 "$ATLAS_INPUT"

cleanup() {
  docker rm -f "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf -- "$ATLAS_INPUT"
}
trap cleanup EXIT

python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py" --prepare-atlas-input "$ATLAS_INPUT"

docker network create "$NETWORK" >/dev/null
docker run -d \
  --name "$POSTGRES_CONTAINER" \
  --network "$NETWORK" \
  --network-alias postgres \
  --health-cmd 'pg_isready -U postgres -d telegram' \
  --health-interval 1s \
  --health-timeout 3s \
  --health-retries 60 \
  -e POSTGRES_PASSWORD=rustfs-schema-fixture \
  -e POSTGRES_DB=telegram \
  "$POSTGRES_IMAGE" >/dev/null

ready=false
for _ in $(seq 1 60); do
  if [ "$(docker inspect --format '{{.State.Health.Status}}' "$POSTGRES_CONTAINER")" = healthy ]; then
    ready=true
    break
  fi
  sleep 1
done
if [ "$ready" != true ]; then
  printf '%s\n' 'PostgreSQL 16 schema fixture did not become ready' >&2
  exit 1
fi

docker run --rm \
  --network "$NETWORK" \
  -v "$ATLAS_INPUT:/migrations:ro" \
  "$ATLAS_IMAGE" migrate apply \
  --dir file:///migrations \
  --url 'postgres://postgres:rustfs-schema-fixture@postgres:5432/telegram?sslmode=disable' >/dev/null

R69_ATLAS_INPUT="$ATLAS_INPUT" R69_POSTGRES_CONTAINER="$POSTGRES_CONTAINER" \
  python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py"
