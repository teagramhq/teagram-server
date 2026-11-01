#!/usr/bin/env bash
# Guards the RPC performance workflow's triggers, fixture fallback, and failure labels.
set -euo pipefail

workflow="${PERF_WORKFLOW:-.github/workflows/perf.yml}"
runner="${PERF_RUNNER:-.github/scripts/perf-run.sh}"
postgres="${PERF_POSTGRES:-.github/scripts/perf-postgres.sh}"
expected_restore_if="steps.snapshot.outputs.cache-hit == 'true' || steps.legacy-snapshot.outputs.cache-hit == 'true'"
expected_seed_if="steps.snapshot.outputs.cache-hit != 'true' && steps.legacy-snapshot.outputs.cache-hit != 'true'"
expected_fixture_key="perf-fixture-v2-\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"
expected_legacy_fixture_key="perf-fixture-\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"
expected_fixture_version="\${{ hashFiles('test/perf/fixture_test.go', 'test/perf/server_test.go') }}"

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
[[ "$(query '.jobs."ops-gate-pr-during-main".name')" == 'ops: gate-pr-during-main' ]] ||
  fail 'performance PR gate must use the ops stage naming contract'
[[ "$(query '.jobs."ops-gate-pr-during-main".steps[] | select(.name == "Defer pull request checks while main CI is active") | .run')" == 'bash .github/scripts/gate-pr-checks-during-main.sh' ]] ||
  fail 'performance PR runs must defer before their fixture and measurement jobs'
[[ "$(query '.jobs."test-perf-fixture-snapshot".needs')" == 'ops-gate-pr-during-main' ]] ||
  fail 'the fixture preparation must wait for the PR gate'
[[ "$(query '.jobs."test-rpc-performance".needs | sort | join(",")')" == 'ops-gate-pr-during-main,test-perf-fixture-snapshot' ]] ||
  fail 'the performance measurement must wait for the PR gate'
expected_concurrency_group="\${{ github.workflow }}-\${{ github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || format('run-{0}', github.run_id) }}"
[[ "$(query '.concurrency.group')" == "$expected_concurrency_group" ]] ||
  fail 'performance runs must coalesce updates per pull request and keep main isolated'
[[ "$(query '.concurrency."cancel-in-progress"')" == "\${{ github.event_name == 'pull_request' }}" ]] ||
  fail 'only updates to the same pull request may cancel a performance run'
[[ "$(query '.concurrency.queue // ""')" == '' ]] ||
  fail 'performance updates must coalesce instead of filling a FIFO queue'

[[ "$(query '.jobs."test-rpc-performance".timeout-minutes')" == '150' ]] ||
  fail 'the performance job must allow a cache-miss fixture seed to finish'
[[ "$(query '.jobs."test-perf-fixture-snapshot".name')" == 'test: perf-fixture-snapshot' ]] ||
  fail 'the snapshot job must use the test stage naming contract'
[[ "$(query '.jobs."publish-perf-fixture".name')" == 'publish: perf-fixture' ]] ||
  fail 'the publisher job must use the publish stage naming contract'
[[ "$(query '.jobs."check-perf-fixture-release".name')" == 'check: perf-fixture-release' ]] ||
  fail 'the release verifier must use the check stage naming contract'
[[ "$(query '.jobs."test-rpc-performance".name')" == 'test: rpc-performance' ]] ||
  fail 'the RPC job must use the test stage naming contract'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Restore fixtures") | .if')" == "$expected_restore_if" ]] ||
  fail 'cached fixtures must be restored only when the snapshot cache hits'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Seed fixtures from this branch") | .if')" == "$expected_seed_if" ]] ||
  fail 'the current branch must seed fixtures when the snapshot cache misses'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Seed fixtures from this branch") | .run')" == 'bash .github/scripts/perf-run.sh seed' ]] ||
  fail 'a cache miss must use the normal fixture seeder'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Measure base and head") | .env.FIXTURE_VERSION')" == "$expected_fixture_version" ]] ||
  fail 'performance reports must identify the fixture seeder version'

missing_snapshot_step="$(query '.jobs."test-rpc-performance".steps[] | select(.name == "No snapshot for this fixture") | .name')"
[[ -z "$missing_snapshot_step" ]] || fail 'a missing main snapshot must not be a standalone failure'

[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Look up the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the snapshot lookup key must include fixture and server setup helpers'
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Look up the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the snapshot lookup must use the same cache path as its save'
snapshot_lookup_if="$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Look up the snapshot") | .if')"
[[ -z "$snapshot_lookup_if" || "$snapshot_lookup_if" == 'null' ]] ||
  fail 'snapshot lookup must run when a matching public release already exists'
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Store the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the snapshot save key must match the fixture and server setup helpers'
snapshot_cache_miss_if="steps.snapshot.outputs.cache-hit != 'true'"
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.uses == "actions/setup-go@v7.0.0") | .if')" == "$snapshot_cache_miss_if" ]] ||
  fail 'release-exists plus cache-miss must install Go and refill the cache'
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.uses == "ariga/setup-atlas@v0.3") | .if')" == "$snapshot_cache_miss_if" ]] ||
  fail 'release-exists plus cache-miss must install Atlas and refill the cache'
for step in 'Start Postgres' 'Seed fixtures' 'Record snapshot metadata' 'Store the snapshot'; do
  [[ "$(query ".jobs.\"test-perf-fixture-snapshot\".steps[] | select(.name == \"$step\") | .if")" == "$snapshot_cache_miss_if" ]] ||
    fail "release-exists plus cache-miss must still refill the cache at $step"
done
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Record snapshot metadata") | .run | contains(".perf-cache/.fixture-metadata.json")')" == 'true' ]] ||
  fail 'the seed provenance must be recorded in the fixture cache'
# Inspect the literal GitHub expression in the workflow text.
# shellcheck disable=SC2016
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Record snapshot metadata") | .run | contains("--arg server_commit \"$GITHUB_SHA\"")')" == 'true' ]] ||
  fail 'the snapshot metadata must record the seeding commit'
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Store the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the snapshot save must match the lookup cache path'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Restore the snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the RPC job restore key must include fixture and server setup helpers'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Restore the snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the RPC job must use the same snapshot cache path'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Restore legacy cache for PR measurements") | .with.key')" == "$expected_legacy_fixture_key" ]] ||
  fail 'PR measurements must retain access to the existing v1 snapshot cache'
[[ "$(query '.jobs."test-rpc-performance".steps[] | select(.name == "Restore legacy cache for PR measurements") | .if')" == "github.event_name == 'pull_request' && steps.snapshot.outputs.cache-hit != 'true'" ]] ||
  fail 'only PR measurements may fall back to a legacy snapshot cache'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Require snapshot seed metadata") | .name')" == 'Require snapshot seed metadata' ]] ||
  fail 'the publisher must fail closed when cached seed provenance is missing'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Restore the fixture snapshot") | .with.path')" == '.perf-cache' ]] ||
  fail 'the publisher must use the same snapshot cache path'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Restore the fixture snapshot") | .with.key')" == "$expected_fixture_key" ]] ||
  fail 'the publisher must restore the provenance-aware snapshot cache'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Build release assets and manifest") | .run | contains("--metadata .perf-cache/.fixture-metadata.json")')" == 'true' ]] ||
  fail 'the release manifest must use the cached seed provenance'
[[ -z "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Recover metadata for an older snapshot cache") | .name')" ]] ||
  fail 'the publisher must not fabricate the seed commit for a legacy cache'
[[ "$(query '.jobs."test-perf-fixture-snapshot".steps[] | select(.name == "Check for an existing fixture release") | .run | contains("perf_fixture_release.py lookup")')" == 'true' ]] ||
  fail 'snapshot preparation must find published releases and drafts by tag'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Recheck release after acquiring the publisher slot") | .run | contains("perf_fixture_release.py lookup")')" == 'true' ]] ||
  fail 'the serialized publisher must recheck draft releases before uploading'
[[ "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Publish complete fixture release") | .run | contains("perf_fixture_release.py publish")')" == 'true' ]] ||
  fail 'release creation, resumable upload, verification, and publication must use the tested publisher'
publish_if="$(query '.jobs."publish-perf-fixture".if')"
[[ "$publish_if" == *"needs.test-perf-fixture-snapshot.outputs.release_exists != 'true'"* &&
   "$publish_if" == *"needs.test-perf-fixture-snapshot.outputs.release_draft == 'true'"* ]] ||
  fail 'a published release must remain unchanged while a cache miss refills the snapshot'
[[ "$(query '.jobs."test-perf-fixture-snapshot".permissions.contents')" == 'read' ]] ||
  fail 'snapshot preparation must keep read-only release permissions'
[[ "$(query '.jobs."publish-perf-fixture".permissions.contents')" == 'write' ]] ||
  fail 'only the publisher job may write release contents'
[[ "$(query '.jobs."check-perf-fixture-release".needs[0]')" == 'test-perf-fixture-snapshot' ]] ||
  fail 'public release verification must wait for the fixture snapshot lookup'
[[ "$(query '.jobs."check-perf-fixture-release".needs[1]')" == 'publish-perf-fixture' ]] ||
  fail 'public release verification must wait for the publisher when it runs'
[[ "$(query '.jobs."check-perf-fixture-release".permissions.contents')" == 'read' ]] ||
  fail 'public release verification must not have permission to modify releases'
verify_if="$(query '.jobs."check-perf-fixture-release".if')"
[[ "$verify_if" == *'always()'* && "$verify_if" == *"needs.publish-perf-fixture.result == 'skipped'"* ]] ||
  fail 'public release verification must still run when an existing published release skips the publisher'
[[ "$(query '.jobs."check-perf-fixture-release".steps[] | select(.name == "Verify anonymous fixture asset downloads") | .run | contains("verify_perf_fixture_release.py")')" == 'true' ]] ||
  fail 'each eligible main run must verify release downloads anonymously'
[[ -z "$(query '.jobs."publish-perf-fixture".steps[] | select(.name == "Verify anonymous asset downloads") | .name')" ]] ||
  fail 'anonymous verification must remain independent of release publication'

release_publisher="$(<.github/scripts/perf_fixture_release.py)"
[[ "$release_publisher" == *"release(tagName: \$tagName)"* ]] ||
  fail 'release lookups must include drafts through GraphQL rather than the published-only tag REST endpoint'
[[ "$release_publisher" == *'releases/{database_id}/assets?per_page=100'* ]] ||
  fail 'draft assets must be verified by release ID'
[[ "$release_publisher" == *'"--clobber"'* ]] ||
  fail 'a retry must safely replace any partially uploaded draft assets'

runner_contents="$(<"$runner")"
expected_restore_command="pg_restore -U postgres -d \"perf_\$f\" -j 4 --no-owner --no-acl \"/cache/perf_\$f.dump\""
[[ "$runner_contents" == *"$expected_restore_command"* ]] ||
  fail 'restoring fixture snapshots must not replay database privileges'
# shellcheck disable=SC2016 # This pattern intentionally matches literal runner source.
[[ "$runner_contents" == *'fatal "restore the $f fixture snapshot"'* ]] ||
  fail 'a missing or corrupt fixture snapshot must still fail the job'
[[ "$runner_contents" == *"RPC performance fixture seed / infrastructure failure"* ]] ||
  fail 'fixture seed and infrastructure errors need an explicit summary label'
[[ "$runner_contents" == *'go test -tags performance -count=1 -timeout 125m '* ]] ||
  fail 'a cache-miss fixture seed must fit within the 150-minute performance job budget'
[[ "$runner_contents" == *"fixture seeder changed; skipping the base comparison"* ]] ||
  fail 'a changed fixture seeder must still allow head measurements without an incompatible base comparison'
[[ "$runner_contents" == *"^Test(LoadFixtureRejectsOlderVersion|OwnerUsernamePasswordLogin|RPCPerformance|CallWithGetDialogsProfileRestoresEnvironment|GetDialogsProfilingUsesEveryMeasuredSample)$"* ]] ||
  fail 'the performance harness selection must run the stale fixture version regression test'
postgres_contents="$(<"$postgres")"
[[ "$postgres_contents" == *"pg_isready -h 127.0.0.1 -p 5432 -U postgres -q"* ]] ||
  fail 'Postgres readiness must wait for the TCP listener used by the workflow'
[[ "$runner_contents" == *'pg_dump -U postgres -Fc -Z 3 --no-acl'* ]] ||
  fail 'fixture snapshots must omit ACLs that depend on cluster-global roles'
[[ "$runner_contents" == *"pg_restore -U postgres -d \"perf_\$f\" -j 4 --no-owner --no-acl"* ]] ||
  fail 'fixture restores must not require cluster-global roles from the snapshot'

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
