#!/usr/bin/env bash
# Guards the RPC performance workflow's fixture fallback and failure labels.
set -euo pipefail

workflow="${PERF_WORKFLOW:-.github/workflows/perf.yml}"
runner="${PERF_RUNNER:-.github/scripts/perf-run.sh}"
postgres="${PERF_POSTGRES:-.github/scripts/perf-postgres.sh}"
expected_restore_if="steps.snapshot.outputs.cache-hit == 'true'"
expected_seed_if="steps.snapshot.outputs.cache-hit != 'true'"
expected_fixture_key="perf-fixture-\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"

fail() {
  printf '%s: %s\n' "$workflow" "$1" >&2
  exit 1
}

query() {
  yq -r "$1" "$workflow"
}

[[ "$(query '.jobs."rpc-performance".timeout-minutes')" == '150' ]] ||
  fail 'the performance job must allow a cache-miss fixture seed to finish'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore fixtures") | .if')" == "$expected_restore_if" ]] ||
  fail 'cached fixtures must be restored only when the snapshot cache hits'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Seed fixtures from this branch") | .if')" == "$expected_seed_if" ]] ||
  fail 'the current branch must seed fixtures when the snapshot cache misses'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Seed fixtures from this branch") | .run')" == 'bash .github/scripts/perf-run.sh seed' ]] ||
  fail 'a cache miss must use the normal fixture seeder'

missing_snapshot_step="$(query '.jobs."rpc-performance".steps[] | select(.name == "No snapshot for this fixture") | .name')"
[[ -z "$missing_snapshot_step" ]] || fail 'a missing main snapshot must not be a standalone failure'

[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Look up the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the snapshot lookup key must include fixture and server setup helpers'
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Store the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the snapshot save key must match the fixture and server setup helpers'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the RPC job restore key must include fixture and server setup helpers'

runner_contents="$(<"$runner")"
[[ "$runner_contents" == *"RPC performance fixture seed / infrastructure failure"* ]] ||
  fail 'fixture seed and infrastructure errors need an explicit summary label'
[[ "$runner_contents" == *"fixture seeder changed; skipping the base comparison"* ]] ||
  fail 'a changed fixture seeder must still allow head measurements without an incompatible base comparison'
postgres_contents="$(<"$postgres")"
[[ "$postgres_contents" == *"pg_isready -h 127.0.0.1 -U postgres -q"* ]] ||
  fail 'Postgres readiness must wait for the TCP listener used by the workflow'

mock_dir="$(mktemp -d)"
trap 'rm -rf "$mock_dir"' EXIT
cat >"$mock_dir/docker" <<'MOCK_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == exec ]] || exit 64
shift
[[ -n "${1:-}" ]] || exit 64
shift
command="${1:-}"
shift || true
case "$command" in
  psql)
    sql=""
    for arg in "$@"; do sql="$arg"; done
    printf 'psql\t%s\n' "$sql" >>"${PERF_DOCKER_EVENTS:?}"
    ;;
  pg_restore|vacuumdb)
    printf '%s\t%s\n' "$command" "$*" >>"${PERF_DOCKER_EVENTS:?}"
    ;;
  *) exit 64 ;;
esac
MOCK_DOCKER
chmod +x "$mock_dir/docker"
events="$mock_dir/events"
PERF_DOCKER_EVENTS="$events" PATH="$mock_dir:$PATH" \
  PERF_PG_CONTAINER=perf-pg PERF_PG_URL=postgres://postgres:perf@localhost:5432/ \
  PERF_CACHE="$mock_dir/cache" RUNNER_TEMP="$mock_dir" \
  bash "$runner" restore
role_line="$(grep -n $'^psql\t.*CREATE ROLE browser_acceptance_probe NOLOGIN' "$events" | head -n1 | cut -d: -f1 || true)"
restore_line="$(grep -n $'^pg_restore\t' "$events" | head -n1 | cut -d: -f1 || true)"
[[ -n "$role_line" && -n "$restore_line" && "$role_line" -lt "$restore_line" ]] ||
  fail 'fixture restore must provision the cached dump role before pg_restore'

printf 'RPC performance workflow contract holds\n'
