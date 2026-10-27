#!/usr/bin/env bash
# Guards the RPC performance workflow's triggers, fixture fallback, and failure labels.
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

mapfile -t pull_request_paths < <(query '.on.pull_request.paths[]')

contains_pr_path() {
  local expected="$1"
  local path
  for path in "${pull_request_paths[@]}"; do
    if [[ "$path" == "$expected" ]]; then
      return 0
    fi
  done
  return 1
}

required_pr_paths=(
  '.github/scripts/perf-*'
  '.github/scripts/test_perf_run.py'
  '.github/workflows/perf.yml'
  'go.mod'
  'go.sum'
  'migrations/**'
  'test/perf/**'
)
for required_path in "${required_pr_paths[@]}"; do
  contains_pr_path "$required_path" || fail "PR performance runs must include '$required_path'"
done

# The harness uses test-only files behind the performance build tag; include test imports.
module_root="$(go list -m -f '{{.Dir}}')"
dependency_dirs="$(
  go list -deps -test -tags=performance -f '{{.Dir}}' ./test/perf |
    awk -v root="$module_root" '
      $0 == root { print "."; next }
      index($0, root "/") == 1 { print substr($0, length(root) + 2) }
    ' |
    sort -u
)"
[[ -n "$dependency_dirs" ]] || fail 'could not derive the performance harness Go dependency paths'

while IFS= read -r dependency_dir; do
  if [[ "$dependency_dir" == '.' ]]; then
    dependency_file='__perf_dependency_contract__.go'
  else
    dependency_file="$dependency_dir/__perf_dependency_contract__.go"
  fi
  dependency_covered=false
  for path_pattern in "${pull_request_paths[@]}"; do
    # shellcheck disable=SC2053 # Match workflow path filters as glob patterns.
    if [[ "$dependency_file" == $path_pattern ]]; then
      dependency_covered=true
      break
    fi
  done
  [[ "$dependency_covered" == true ]] ||
    fail "PR performance paths must cover Go dependency '$dependency_dir'"
done <<< "$dependency_dirs"

[[ "$(query '.on.push.branches | join(",")')" == 'main' ]] ||
  fail 'main pushes must continue to run the performance workflow'
[[ "$(query '.on.workflow_dispatch.inputs.issue.type')" == 'string' ]] ||
  fail 'manual performance dispatch must remain available'

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
