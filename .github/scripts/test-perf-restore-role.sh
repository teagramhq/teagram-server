#!/usr/bin/env bash
# A pg_dump snapshot retains ACLs but not the cluster roles those ACLs name.
# Keep restore's role bootstrap covered with a minimal snapshot that grants to
# the browser acceptance probe role.
set -euo pipefail

readonly container="perf-restore-role-test-${GITHUB_RUN_ID:-local}-$$"
readonly tmp
tmp="$(mktemp -d)"
readonly image=mirror.gcr.io/postgres:18-alpine
mkdir -p "$tmp/cache"
chmod 0777 "$tmp/cache"

cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

bash .github/scripts/pull-base-images.sh --image "$image"
docker run -d --name "$container" --shm-size=64m \
  -e POSTGRES_PASSWORD=perf -v "$tmp/cache:/cache" \
  "$image" -c shared_buffers=64MB -c max_connections=20 -c autovacuum=off >/dev/null

psql_admin() {
  docker exec "$container" psql -v ON_ERROR_STOP=1 -U postgres -qAt "$@"
}

ready=false
for _ in $(seq 1 60); do
  # The image first starts a temporary socket-only server to initialize the
  # data directory. Wait for the final server's TCP listener instead.
  if docker exec "$container" pg_isready -h 127.0.0.1 -U postgres -q; then
    ready=true
    break
  fi
  sleep 1
done
if [[ "$ready" != true ]]; then
  echo 'Postgres did not become ready for TCP connections' >&2
  docker logs "$container"
  exit 1
fi

psql_admin -c "CREATE ROLE browser_acceptance_probe LOGIN"
for f in 1x 100x; do
  psql_admin -c "CREATE DATABASE perf_$f"
  docker exec "$container" psql -v ON_ERROR_STOP=1 -U postgres -d "perf_$f" -qAt -c \
    "CREATE FUNCTION public.perf_restore_probe() RETURNS integer LANGUAGE sql AS 'SELECT 1'; REVOKE ALL ON FUNCTION public.perf_restore_probe() FROM PUBLIC; GRANT EXECUTE ON FUNCTION public.perf_restore_probe() TO browser_acceptance_probe"
  docker exec "$container" pg_dump -U postgres -Fc -Z 3 -f "/cache/perf_$f.dump" "perf_$f"
  psql_admin -c "DROP DATABASE perf_$f"
done
psql_admin -c "DROP ROLE browser_acceptance_probe"

export PERF_PG_CONTAINER="$container"
export PERF_PG_URL=postgres://postgres:perf@localhost:5432/
export PERF_CACHE="$tmp/cache"
export PERF_WORK="$tmp/perf"
bash .github/scripts/perf-run.sh restore

[[ "$(psql_admin -c "SELECT rolcanlogin AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolinherit AND NOT rolreplication AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname = 'browser_acceptance_probe'")" == t ]]
for f in 1x 100x; do
  [[ "$(docker exec "$container" psql -v ON_ERROR_STOP=1 -U postgres -d "perf_$f" -qAt -c \
    "SELECT has_function_privilege('browser_acceptance_probe', 'public.perf_restore_probe()', 'EXECUTE')")" == t ]]
done
