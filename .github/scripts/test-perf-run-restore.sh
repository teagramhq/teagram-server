#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf -- "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/cache" "$tmp/runner"

cat >"$tmp/bin/docker" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DOCKER_LOG"
SH
chmod +x "$tmp/bin/docker"

export PATH="$tmp/bin:$PATH"
export DOCKER_LOG="$tmp/docker.log"
export PERF_PG_CONTAINER=perf-pg
export PERF_PG_URL=postgres://postgres:perf@localhost:5432/
export PERF_CACHE="$tmp/cache"
export RUNNER_TEMP="$tmp/runner"

bash "$script_dir/perf-run.sh" restore

for fixture in 1x 100x; do
  expected="exec perf-pg pg_restore -U postgres -d perf_$fixture -j 4 --no-owner --no-acl /cache/perf_$fixture.dump"
  if ! grep -Fxq -- "$expected" "$DOCKER_LOG"; then
    printf 'fixture restore must omit ACLs for cluster roles absent from the fresh Postgres instance: %s\n' "$fixture" >&2
    exit 1
  fi
done

printf '%s\n' 'RPC performance fixture restore omits unavailable cluster-role ACLs'
