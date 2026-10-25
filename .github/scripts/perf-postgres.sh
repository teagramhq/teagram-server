#!/usr/bin/env bash
# Start the performance gate's Postgres: the production image with its
# defaults, durability included, so write budgets are measured against a server
# that fsyncs. The seeder turns synchronous_commit off for its own sessions.
set -euo pipefail

: "${PERF_PG_CONTAINER:?}" "${PERF_CACHE:?}"
bash .github/scripts/pull-base-images.sh --image mirror.gcr.io/postgres:18-alpine
mkdir -p "$PERF_CACHE"
# The container's postgres user writes the dumps.
chmod 0777 "$PERF_CACHE"
docker run -d --name "$PERF_PG_CONTAINER" -p 5432:5432 --shm-size=1g \
  -e POSTGRES_PASSWORD=perf -v "$PERF_CACHE:/cache" \
  mirror.gcr.io/postgres:18-alpine -c shared_buffers=1GB -c max_connections=200
for _ in $(seq 1 60); do
  if docker exec "$PERF_PG_CONTAINER" pg_isready -U postgres -q; then
    exit 0
  fi
  sleep 1
done
echo "::error::Postgres did not become ready"
exit 1
