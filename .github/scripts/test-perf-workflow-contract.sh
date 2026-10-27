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
expected_restore_command="pg_restore -U postgres -d \"perf_\$f\" -j 4 --no-owner --no-acl \"/cache/perf_\$f.dump\""
[[ "$runner_contents" == *"$expected_restore_command"* ]] ||
  fail 'restoring fixture snapshots must not replay database privileges'
# shellcheck disable=SC2016 # This pattern intentionally matches literal runner source.
[[ "$runner_contents" == *'fatal "restore the $f fixture snapshot"'* ]] ||
  fail 'a missing or corrupt fixture snapshot must still fail the job'
[[ "$runner_contents" == *"RPC performance fixture seed / infrastructure failure"* ]] ||
  fail 'fixture seed and infrastructure errors need an explicit summary label'
[[ "$runner_contents" == *"fixture seeder changed; skipping the base comparison"* ]] ||
  fail 'a changed fixture seeder must still allow head measurements without an incompatible base comparison'
[[ "$runner_contents" == *"^Test(LoadFixtureRejectsOlderVersion|OwnerUsernamePasswordLogin|RPCPerformance)$"* ]] ||
  fail 'the performance harness selection must run the stale fixture version regression test'
postgres_contents="$(<"$postgres")"
[[ "$postgres_contents" == *"pg_isready -h 127.0.0.1 -U postgres -q"* ]] ||
  fail 'Postgres readiness must wait for the TCP listener used by the workflow'
[[ "$runner_contents" == *'pg_dump -U postgres -Fc -Z 3 --no-acl'* ]] ||
  fail 'fixture snapshots must omit ACLs that depend on cluster-global roles'
[[ "$runner_contents" == *"pg_restore -U postgres -d \"perf_\$f\" -j 4 --no-owner --no-acl"* ]] ||
  fail 'fixture restores must not require cluster-global roles from the snapshot'

printf 'RPC performance workflow contract holds\n'
