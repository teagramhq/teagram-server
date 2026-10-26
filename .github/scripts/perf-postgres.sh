#!/usr/bin/env bash
# Start the performance gate's Postgres: the production image with its
# defaults, durability included, so write budgets are measured against a server
# that fsyncs. The seeder turns synchronous_commit off for its own sessions.
# Autovacuum is off so planner statistics change only when perf-run.sh analyzes:
# a background ANALYZE between rounds, or between base and head, would change
# plans under the blocking checks.
set -euo pipefail

: "${PERF_PG_CONTAINER:?}" "${PERF_CACHE:?}"
bash .github/scripts/pull-base-images.sh --image mirror.gcr.io/postgres:18-alpine
mkdir -p "$PERF_CACHE"
# The container's postgres user writes the dumps.
chmod 0777 "$PERF_CACHE"
docker run -d --name "$PERF_PG_CONTAINER" -p 5432:5432 --shm-size=1g \
  -e POSTGRES_PASSWORD=perf -v "$PERF_CACHE:/cache" \
  mirror.gcr.io/postgres:18-alpine -c shared_buffers=1GB -c max_connections=200 -c autovacuum=off
ready=false
for _ in $(seq 1 60); do
  # The image uses a temporary socket-only server while initializing. The
  # workflow needs TCP, so wait for the final server instead of that socket.
  if docker exec "$PERF_PG_CONTAINER" pg_isready -h 127.0.0.1 -U postgres -q; then
    ready=true
    break
  fi
  sleep 1
done
if [ "$ready" != true ]; then
  echo "::error::Postgres did not become ready for TCP connections"
  docker logs "$PERF_PG_CONTAINER"
  exit 1
fi

# Fixture dumps carry ACLs for this cluster-wide role, which pg_dump does not
# include. Keep it non-login here; the performance job only restores grants, and
# the migration sets the role up when seeding from scratch.
docker exec "$PERF_PG_CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -qAt \
  -c "CREATE ROLE browser_acceptance_probe NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"
