#!/usr/bin/env bash
# Guards the RPC performance workflow's fixture fallback and failure labels.
set -euo pipefail

workflow="${PERF_WORKFLOW:-.github/workflows/perf.yml}"
runner="${PERF_RUNNER:-.github/scripts/perf-run.sh}"
postgres="${PERF_POSTGRES:-.github/scripts/perf-postgres.sh}"
expected_restore_if="steps.snapshot.outputs.cache-hit == 'true' || steps.legacy-snapshot.outputs.cache-hit == 'true'"
expected_seed_if="steps.snapshot.outputs.cache-hit != 'true' && steps.legacy-snapshot.outputs.cache-hit != 'true'"
expected_fixture_key="perf-fixture-v2-\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"
expected_legacy_fixture_key="perf-fixture-\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"

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
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Look up the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the snapshot lookup must use the same cache path as its save'
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Store the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the snapshot save key must match the fixture and server setup helpers'
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Record snapshot metadata") | .run | contains(".perf-cache/.fixture-metadata.json")')" == 'true' ]] ||
  fail 'the seed provenance must be recorded in the fixture cache'
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Record snapshot metadata") | .if')" == "steps.snapshot.outputs.cache-hit != 'true' && steps.release.outputs.exists != 'true'" ]] ||
  fail 'seed provenance must be captured only when this run creates the snapshot'
# Inspect the literal GitHub expression in the workflow text.
# shellcheck disable=SC2016
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Record snapshot metadata") | .run | contains("--arg server_commit \"$GITHUB_SHA\"")')" == 'true' ]] ||
  fail 'the snapshot metadata must record the seeding commit'
[[ "$(query '.jobs."fixture-snapshot".steps[] | select(.name == "Store the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the snapshot save must match the lookup cache path'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the RPC job restore key must include fixture and server setup helpers'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the RPC job must use the same snapshot cache path'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore legacy cache for PR measurements") | .with.key')" == "$expected_legacy_fixture_key" ]] ||
  fail 'PR measurements must retain access to the existing v1 snapshot cache'
[[ "$(query '.jobs."rpc-performance".steps[] | select(.name == "Restore legacy cache for PR measurements") | .if')" == "github.event_name == 'pull_request' && steps.snapshot.outputs.cache-hit != 'true'" ]] ||
  fail 'only PR measurements may fall back to a legacy snapshot cache'
[[ "$(query '.jobs."publish-fixture".steps[] | select(.name == "Require snapshot seed metadata") | .name')" == 'Require snapshot seed metadata' ]] ||
  fail 'the publisher must fail closed when cached seed provenance is missing'
[[ "$(query '.jobs."publish-fixture".steps[] | select(.name == "Restore the fixture snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the publisher must use the same snapshot cache path'
[[ "$(query '.jobs."publish-fixture".steps[] | select(.name == "Restore the fixture snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the publisher must restore the provenance-aware snapshot cache'
[[ "$(query '.jobs."publish-fixture".steps[] | select(.name == "Build release assets and manifest") | .run | contains("--metadata .perf-cache/.fixture-metadata.json")')" == 'true' ]] ||
  fail 'the release manifest must use the cached seed provenance'
[[ -z "$(query '.jobs."publish-fixture".steps[] | select(.name == "Recover metadata for an older snapshot cache") | .name')" ]] ||
  fail 'the publisher must not fabricate the seed commit for a legacy cache'

runner_contents="$(<"$runner")"
# shellcheck disable=SC2016 # These patterns intentionally match literal runner source.
[[ "$runner_contents" == *'pg_restore -U postgres -d "perf_$f" -j 4 --no-owner --no-acl "/cache/perf_$f.dump"'* ]] ||
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

printf 'RPC performance workflow contract holds\n'
