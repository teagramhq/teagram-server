#!/usr/bin/env bash
# RPC performance gate.
#
#   perf-run.sh seed      seed both fixtures and dump them into $PERF_CACHE
#                         (main only; a PR never seeds)
#   perf-run.sh restore   restore the fixture snapshot from $PERF_CACHE
#   perf-run.sh measure   measure base and head interleaved, then judge
#
# Environment:
#   PERF_PG_CONTAINER  name of the running Postgres container
#   PERF_PG_URL        URL prefix of that server, ending in "/"
#   PERF_CACHE         fixture snapshot directory, mounted at /cache in the container
#   PERF_HISTORY       directory of earlier main runs' aggregated reports
#   PERF_RECORD        set on main: keep this run's report in $PERF_HISTORY
#   PERF_WORK          scratch directory for binaries and reports ($RUNNER_TEMP/perf)
#   PERF_BASE_SHA      commit to compare against; empty skips the comparison
#   PERF_ROUNDS        interleaved rounds per branch (default 2)
set -euo pipefail

: "${PERF_PG_CONTAINER:?}" "${PERF_PG_URL:?}" "${PERF_CACHE:?}"
PERF_HISTORY="${PERF_HISTORY:-$PERF_CACHE/../.perf-history}"
PERF_WORK="${PERF_WORK:-${RUNNER_TEMP:?}/perf}"
rounds="${PERF_ROUNDS:-2}"
history_keep=20
fixtures=(1x 100x)

psql_admin() {
  docker exec "$PERF_PG_CONTAINER" psql -v ON_ERROR_STOP=1 -U postgres -qAt "$@"
}

dsn() { echo "${PERF_PG_URL}perf_$1$2?sslmode=disable"; }

migrate() {
  local dir="$1" db="$2"
  atlas migrate apply --url "$(dsn "$db" "")" --dir "file://$dir/migrations"
}

seed() {
  mkdir -p "$PERF_CACHE"
  for f in "${fixtures[@]}"; do
    psql_admin -c "CREATE DATABASE perf_$f"
    migrate . "$f"
  done
  TG_PERF_DIR="$PERF_CACHE" TG_PERF_DSN_1X="$(dsn 1x "")" TG_PERF_DSN_100X="$(dsn 100x "")" \
    go test -tags performance -count=1 -timeout 100m -run '^TestSeedFixtures$' -v ./test/perf
  for f in "${fixtures[@]}"; do
    docker exec "$PERF_PG_CONTAINER" pg_dump -U postgres -Fc -Z 3 -f "/cache/perf_$f.dump" "perf_$f"
  done
}

# analyze vacuums a database and rebuilds its planner statistics. Vacuum sets
# the visibility map index-only scans depend on. pg_dump leaves statistics out and
# pg_restore never runs ANALYZE, so a restored fixture has none; autovacuum is
# off on this server, so nothing else fills them in mid-run either. Plans, and
# with them the sequential-scan and rows-read checks, read these statistics.
analyze() {
  docker exec "$PERF_PG_CONTAINER" vacuumdb -U postgres -q --analyze -j 4 -d "$1"
}

restore() {
  for f in "${fixtures[@]}"; do
    psql_admin -c "CREATE DATABASE perf_$f"
    docker exec "$PERF_PG_CONTAINER" pg_restore -U postgres -d "perf_$f" -j 4 --no-owner "/cache/perf_$f.dump"
    # Before base_ready copies it, so the copies carry the same statistics.
    analyze "perf_$f"
  done
}

# revision_ahead_of prints the first of the applied revisions that the tree's
# migrations/ does not contain, and fails when there is none. That is the one
# way base migrations cannot apply: the snapshot was built after a migration
# base predates.
revision_ahead_of() {
  local tree="$1" revs="$2" v
  for v in $revs; do
    if ! compgen -G "$tree/migrations/${v}_*.sql" >/dev/null; then
      echo "$v"
      return 0
    fi
  done
  return 1
}

# base_ready prepares the base branch's databases and test binary. It returns
# 1 only for the documented skips: no base commit, a changed fixture seeder,
# a base without the harness, and a cached fixture already past base's
# migrations. Anything else that goes wrong, a broken migration included,
# fails the job, since skipping would silently drop every base check.
# Its databases are copies of the restored fixtures, migrated with the base
# branch's own migrations before head's are applied.
base_ready() {
  if [ -z "${PERF_BASE_SHA:-}" ]; then
    echo "no base commit" >"$PERF_WORK/base-skipped"
    return 1
  fi
  if ! git diff --quiet "$PERF_BASE_SHA" HEAD -- test/perf/fixture_test.go; then
    echo "the fixture seeder changed; the base harness cannot read its versioned snapshot" >"$PERF_WORK/base-skipped"
    return 1
  fi
  local tree="$PERF_WORK/base"
  git worktree add --detach "$tree" "$PERF_BASE_SHA" >/dev/null || fatal "check out base $PERF_BASE_SHA"
  if [ ! -f "$tree/test/perf/harness_test.go" ]; then
    echo "base has no harness" >"$PERF_WORK/base-skipped"
    return 1
  fi
  for f in "${fixtures[@]}"; do
    psql_admin -c "CREATE DATABASE perf_${f}_base TEMPLATE perf_$f" || fatal "copy the $f fixture for base"
    local revs rev
    revs=$(psql_admin -d "perf_${f}_base" -c "SELECT version FROM atlas_schema_revisions.atlas_schema_revisions ORDER BY version") \
      || fatal "read the migration revisions of the $f fixture"
    if rev=$(revision_ahead_of "$tree" "$revs"); then
      echo "the cached fixture already has migration $rev, which base does not" >"$PERF_WORK/base-skipped"
      return 1
    fi
    migrate "$tree" "${f}_base" || fatal "apply base migrations to the $f fixture"
    analyze "perf_${f}_base" || fatal "analyze the base $f fixture"
  done
  (cd "$tree" && go test -c -tags performance -o "$PERF_WORK/base.test" ./test/perf) || fatal "build the base harness"
}

fatal() {
  echo "::error::could not $1"
  exit 1
}

run_branch() {
  local branch="$1" round="$2" suffix=""
  [ "$branch" = base ] && suffix=_base
  echo "::group::$branch round $round"
  TG_PERF_DIR="$PERF_CACHE" \
    TG_PERF_DSN_1X="$(dsn 1x "$suffix")" TG_PERF_DSN_100X="$(dsn 100x "$suffix")" \
    TG_PERF_REPORT="$PERF_WORK/$branch-$round.json" \
    "$PERF_WORK/$branch.test" -test.run '^Test(OwnerUsernamePasswordLogin|RPCPerformance)$' -test.count=1 -test.timeout 30m -test.v
  echo "::endgroup::"
}

run_rounds() {
  local tag="$1" round
  # Alternate which branch goes first, so drift over the job (a warming page
  # cache, a noisy neighbour) lands on both evenly.
  for round in $(seq 1 "$rounds"); do
    if [ "$with_base" = true ] && [ $((round % 2)) -eq 1 ]; then
      run_branch base "$tag$round"
      run_branch head "$tag$round"
    elif [ "$with_base" = true ]; then
      run_branch head "$tag$round"
      run_branch base "$tag$round"
    else
      run_branch head "$tag$round"
    fi
  done
}

judge() {
  local tag="$1" base_glob=""
  [ "$with_base" = true ] && base_glob="$PERF_WORK/base-$tag*.json"
  go run ./test/perf/cmd/perfreport -head "$PERF_WORK/head-$tag*.json" -base "$base_glob" \
    -history "$PERF_HISTORY/*.json" -waivers test/perf/known_violations.json \
    -required test/perf/required_scenarios.json \
    -aggregate "$PERF_WORK/aggregate.json" >"$PERF_WORK/summary.md"
}

measure() {
  mkdir -p "$PERF_WORK" "$PERF_HISTORY"
  with_base=false
  if base_ready; then
    with_base=true
  fi
  # A migration can rewrite a table and drop its statistics, so each side is
  # analyzed again after its own migrations.
  for f in "${fixtures[@]}"; do
    migrate . "$f"
    analyze "perf_$f"
  done
  go test -c -tags performance -o "$PERF_WORK/head.test" ./test/perf
  run_rounds a
  local status=0
  judge a || status=$?
  # A timing failure on a calibrated scenario counts only when a second set of
  # rounds repeats it.
  if [ "$status" -eq 3 ]; then
    echo "timing checks failed; measuring once more"
    run_rounds b
    status=0
    judge b || status=$?
  fi
  if [ -f "$PERF_WORK/base-skipped" ]; then
    printf '\nBase comparison skipped: %s.\n' "$(cat "$PERF_WORK/base-skipped")" >>"$PERF_WORK/summary.md"
  fi
  cat "$PERF_WORK/summary.md"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    cat "$PERF_WORK/summary.md" >>"$GITHUB_STEP_SUMMARY"
  fi
  # Only a passing main run calibrates: a regression kept in the history would
  # widen the noise band that is meant to catch it.
  if [ -n "${PERF_RECORD:-}" ] && [ "$status" -eq 0 ] && [ -f "$PERF_WORK/aggregate.json" ]; then
    cp "$PERF_WORK/aggregate.json" "$PERF_HISTORY/$(date -u +%Y%m%dT%H%M%S).json"
    ls -1 "$PERF_HISTORY"/*.json | head -n -"$history_keep" | xargs -r rm --
  fi
  return "$status"
}

case "${1:-}" in
  seed) seed ;;
  restore) restore ;;
  measure) measure ;;
  *)
    echo "usage: $0 seed|restore|measure" >&2
    exit 64
    ;;
esac
