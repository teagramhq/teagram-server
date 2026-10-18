#!/usr/bin/env bash
set -Eeuo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
NETWORK="rustfs-schema-${BASHPID}"
POSTGRES_CONTAINER="rustfs-schema-pg-${BASHPID}"
POSTGRES_IMAGE="ghcr.io/teagramhq/teagram-server-postgres:16-alpine"
POSTGRES_IMAGE_DIGEST=sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
ATLAS_BIN="${ATLAS_BIN:-atlas}"

command -v "$ATLAS_BIN" >/dev/null 2>&1 || {
  printf 'Atlas CLI is unavailable: %s\n' "$ATLAS_BIN" >&2
  exit 1
}

ATLAS_R69_INPUT=$(mktemp -d "${TMPDIR:-/tmp}/rustfs-schema-r69-atlas.XXXXXX")
ATLAS_R70_INPUT=$(mktemp -d "${TMPDIR:-/tmp}/rustfs-schema-r70-atlas.XXXXXX")
chmod 700 "$ATLAS_R69_INPUT" "$ATLAS_R70_INPUT"

cleanup() {
  docker rm -f "$POSTGRES_CONTAINER" >/dev/null 2>&1 || true
  docker network rm "$NETWORK" >/dev/null 2>&1 || true
  rm -rf -- "$ATLAS_R69_INPUT" "$ATLAS_R70_INPUT"
}
trap cleanup EXIT

python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py" --prepare-atlas-input "$ATLAS_R69_INPUT" 60-69
python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py" --prepare-atlas-input "$ATLAS_R70_INPUT" 60-70

docker pull "$POSTGRES_IMAGE@$POSTGRES_IMAGE_DIGEST" >/dev/null
docker network create "$NETWORK" >/dev/null
docker run -d \
  --name "$POSTGRES_CONTAINER" \
  --network "$NETWORK" \
  --network-alias postgres \
  --publish 127.0.0.1::5432 \
  --health-cmd 'pg_isready -U postgres -d teagram' \
  --health-interval 1s \
  --health-timeout 3s \
  --health-retries 60 \
  -e POSTGRES_PASSWORD=rustfs-schema-fixture \
  -e POSTGRES_DB=teagram \
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
POSTGRES_PORT=$(docker port "$POSTGRES_CONTAINER" 5432/tcp)
POSTGRES_PORT=${POSTGRES_PORT##*:}
[[ "$POSTGRES_PORT" =~ ^[0-9]+$ ]] || {
  printf 'PostgreSQL fixture has an invalid loopback port: %s\n' "$POSTGRES_PORT" >&2
  exit 1
}

"$ATLAS_BIN" migrate apply \
  --dir "file://$ATLAS_R69_INPUT" \
  --url "postgres://postgres:rustfs-schema-fixture@127.0.0.1:$POSTGRES_PORT/teagram?sslmode=disable" >/dev/null

RUSTFS_RELEASE_SET=60-69 RUSTFS_ATLAS_INPUT="$ATLAS_R69_INPUT" \
RUSTFS_POSTGRES_CONTAINER="$POSTGRES_CONTAINER" RUSTFS_POSTGRES_DB=teagram \
  python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py"

docker exec "$POSTGRES_CONTAINER" createdb -U postgres teagram_r70
"$ATLAS_BIN" migrate apply \
  --dir "file://$ATLAS_R70_INPUT" \
  --url "postgres://postgres:rustfs-schema-fixture@127.0.0.1:$POSTGRES_PORT/teagram_r70?sslmode=disable" >/dev/null

RUSTFS_RELEASE_SET=60-70 RUSTFS_ATLAS_INPUT="$ATLAS_R70_INPUT" \
RUSTFS_POSTGRES_CONTAINER="$POSTGRES_CONTAINER" RUSTFS_POSTGRES_DB=teagram_r70 \
  python3 "$SCRIPT_DIR/test-rustfs-schema-postgres.py"
