#!/usr/bin/env bash
set -euo pipefail

readonly repo_root="$(git rev-parse --show-toplevel)"
readonly base_ref="${MIGRATIONS_BASE_REF:?set MIGRATIONS_BASE_REF to the merge base ref}"
readonly postgres_image="${POSTGRES_IMAGE:?set POSTGRES_IMAGE}"
readonly atlas_apply_image="${ATLAS_APPLY_IMAGE:?set ATLAS_APPLY_IMAGE}"
readonly atlas_lint_binary="${ATLAS_LINT_BIN:?set ATLAS_LINT_BIN}"
readonly password="ci-only-migration"
readonly suffix="$$"
readonly network="migration-ci-${suffix}"
readonly postgres_container="migration-ci-postgres-${suffix}"
readonly temp_dir="$(mktemp -d "$repo_root/.migration-ci.XXXXXX")"
readonly baseline_dir="$temp_dir/base"

cleanup() {
  docker rm --force "$postgres_container" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  rm -rf -- "$temp_dir"
}
trap cleanup EXIT

chmod 755 "$temp_dir"
docker network create "$network" >/dev/null
docker run --detach \
  --name "$postgres_container" \
  --network "$network" \
  --publish 0.0.0.0::5432 \
  --env POSTGRES_PASSWORD="$password" \
  --env POSTGRES_DB=postgres \
  "$postgres_image" >/dev/null

for attempt in $(seq 1 60); do
  if docker exec "$postgres_container" pg_isready -h 127.0.0.1 -U postgres -d postgres >/dev/null 2>&1; then
    break
  fi
  if [ "$attempt" -eq 60 ]; then
    docker logs "$postgres_container"
    exit 1
  fi
  sleep 1
done

readonly postgres_port="$(docker port "$postgres_container" 5432/tcp | awk -F: 'END { print $NF }')"
if [[ "${DOCKER_HOST:-}" == tcp://* ]]; then
  readonly docker_daemon_host="${DOCKER_HOST#tcp://}"
  readonly lint_host="${docker_daemon_host%%:*}"
else
  readonly lint_host=127.0.0.1
fi
readonly lint_dsn="postgres://postgres:${password}@${lint_host}:${postgres_port}/migration_lint?sslmode=disable"
readonly lint_base_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_lint_base?sslmode=disable"
readonly lint_checkpoint_base_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_lint_checkpoint_base?sslmode=disable"
readonly lint_checkpoint_dev_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_lint_checkpoint_dev?sslmode=disable"
readonly fresh_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_fresh?sslmode=disable"
readonly upgrade_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_upgrade?sslmode=disable"

for database in migration_lint migration_lint_base migration_lint_checkpoint_base migration_lint_checkpoint_dev migration_fresh migration_upgrade; do
  docker exec "$postgres_container" createdb -U postgres "$database"
done

apply_migrations() {
  local migration_dir=$1
  local database_dsn=$2
  docker run --rm \
    --network "$network" \
    --volume "$migration_dir:/migrations:ro" \
    "$atlas_apply_image" \
    migrate apply --dir file:///migrations --url "$database_dsn"
}

run_checkpointed_atlas_lint() {
  local changed_lint_dir=$1
  local base_migrations_dir=$2
  local base_checkpoint=$3
  local base_database=$4
  local base_dsn=$5
  local dev_dsn=$6
  shift 6

  local checkpoint_file="$changed_lint_dir/$base_checkpoint"
  if [ -e "$checkpoint_file" ]; then
    printf 'Changed migration overlaps the Atlas lint checkpoint: %s\n' "$checkpoint_file" >&2
    return 1
  fi

  # Atlas lint executes each base statement separately, which cannot replay
  # historical LOCK TABLE statements. Apply the base with Atlas's migration
  # runner, then seed lint from a schema-only checkpoint of that database.
  apply_migrations "$base_migrations_dir" "$base_dsn"
  {
    printf '%s\n' '-- atlas:checkpoint'
    docker exec --env "PGPASSWORD=$password" "$postgres_container" pg_dump \
      --username=postgres \
      --dbname="$base_database" \
      --schema-only \
      --no-owner \
      --no-privileges \
      --no-comments \
      --exclude-table=public.atlas_schema_revisions
  } | sed '/^\\restrict /d; /^\\unrestrict$/d' > "$checkpoint_file"
  "$atlas_lint_binary" migrate lint \
    --dir "file://$changed_lint_dir" \
    --dev-url "$dev_dsn" \
    "$@"
}

docker run --rm \
  --volume "$repo_root/migrations:/migrations:ro" \
  "$atlas_apply_image" \
  migrate validate --dir file:///migrations

python3 -B .github/scripts/migration_lock_lint.py --base "$base_ref"

mapfile -t changed_sql_migrations < <(
  git -C "$repo_root" diff --name-only --diff-filter=AMR "$base_ref...HEAD" -- migrations \
    | awk '/\.sql$/ { print }'
)

mkdir -p "$baseline_dir"
git -C "$repo_root" archive "$base_ref" migrations | tar -x -C "$baseline_dir"
chmod -R a+rX "$baseline_dir"

if [ "${#changed_sql_migrations[@]}" -gt 0 ]; then
  changed_lint_dir="$temp_dir/atlas-lint-changed"
  mkdir -p "$changed_lint_dir"
  for migration in "${changed_sql_migrations[@]}"; do
    cp -- "$repo_root/$migration" "$changed_lint_dir/"
  done

  base_checkpoint="$(git -C "$repo_root" ls-tree -r --name-only "$base_ref" -- migrations \
    | awk '/\.sql$/ { print }' \
    | LC_ALL=C sort \
    | tail -n 1)"
  if [ -z "$base_checkpoint" ]; then
    printf 'No base migration available for Atlas lint checkpoint (%s).\n' "$base_ref" >&2
    exit 1
  fi
  run_checkpointed_atlas_lint \
    "$changed_lint_dir" \
    "$baseline_dir/migrations" \
    "$(basename "$base_checkpoint")" \
    migration_lint_base \
    "$lint_base_dsn" \
    "$lint_dsn" \
    --git-base "$base_ref" \
    --git-dir "$repo_root"
else
  printf '%s\n' 'Atlas migration lint skipped: no changed SQL migrations.'
fi

run_atlas_lint_fixture() {
  local fixture_name=$1
  local expected_result=$2
  local database=$3
  local fixture_source="$repo_root/.github/scripts/testdata/migrations/atlas-lint/$fixture_name"
  local fixture_dir="$temp_dir/atlas-lint-$fixture_name"
  local fixture_dsn="postgres://postgres:${password}@${lint_host}:${postgres_port}/${database}?sslmode=disable"
  local lint_output="$temp_dir/atlas-lint-$fixture_name.log"
  local lint_status=0

  mkdir -p "$fixture_dir"
  cp "$fixture_source"/*.sql "$fixture_dir/"
  "$atlas_lint_binary" migrate hash --dir "file://$fixture_dir" >/dev/null

  if "$atlas_lint_binary" migrate lint \
    --dir "file://$fixture_dir" \
    --dev-url "$fixture_dsn" \
    --latest 1 >"$lint_output" 2>&1; then
    lint_status=0
  else
    lint_status=$?
  fi

  if [[ "$expected_result" == pass && "$lint_status" -ne 0 ]]; then
    cat "$lint_output" >&2
    printf 'Atlas migration lint rejected the %s fixture.\n' "$fixture_name" >&2
    return "$lint_status"
  fi
  if [[ "$expected_result" == reject && "$lint_status" -eq 0 ]]; then
    cat "$lint_output"
    printf 'Atlas migration lint accepted the %s fixture.\n' "$fixture_name" >&2
    return 1
  fi
  if [[ "$expected_result" == reject ]] && ! grep -Fq -- 'destructive changes detected' "$lint_output"; then
    cat "$lint_output" >&2
    printf 'Atlas migration lint rejected %s without reporting a destructive change.\n' "$fixture_name" >&2
    return 1
  fi
  cat "$lint_output"
  printf 'Atlas migration lint %s fixture: %s as expected.\n' "$fixture_name" "$expected_result"
}

docker exec "$postgres_container" createdb -U postgres migration_lint_additive
docker exec "$postgres_container" createdb -U postgres migration_lint_destructive
run_atlas_lint_fixture additive pass migration_lint_additive
run_atlas_lint_fixture destructive reject migration_lint_destructive

checkpoint_base_dir="$temp_dir/atlas-lint-checkpoint-base"
checkpoint_changed_dir="$temp_dir/atlas-lint-checkpoint-changed"
mkdir -p "$checkpoint_base_dir" "$checkpoint_changed_dir"
cp "$repo_root/.github/scripts/testdata/migrations/atlas-lint/checkpoint-base/"*.sql "$checkpoint_base_dir/"
cp "$repo_root/.github/scripts/testdata/migrations/atlas-lint/additive/20260103000000_add_users_email_index.sql" "$checkpoint_changed_dir/"
"$atlas_lint_binary" migrate hash --dir "file://$checkpoint_base_dir" >/dev/null
run_checkpointed_atlas_lint \
  "$checkpoint_changed_dir" \
  "$checkpoint_base_dir" \
  20260102000000_lock_users.sql \
  migration_lint_checkpoint_base \
  "$lint_checkpoint_base_dsn" \
  "$lint_checkpoint_dev_dsn" \
  --latest 1
printf '%s\n' 'Atlas migration lint checkpoint fixture passed.'

apply_migrations "$repo_root/migrations" "$fresh_dsn"

apply_migrations "$baseline_dir/migrations" "$upgrade_dsn"

docker exec "$postgres_container" psql \
  -X \
  -v ON_ERROR_STOP=1 \
  -U postgres \
  -d migration_upgrade \
  -c "INSERT INTO users (first_name, last_name, kind) VALUES ('migration-ci-fixture', 'upgrade', 'bot')"

apply_migrations "$repo_root/migrations" "$upgrade_dsn"

fixture_result="$(docker exec "$postgres_container" psql \
  -XAt \
  -v ON_ERROR_STOP=1 \
  -U postgres \
  -d migration_upgrade \
  -c "SELECT first_name || '|' || last_name FROM users WHERE first_name = 'migration-ci-fixture' AND kind = 'bot'")"
if [ "$fixture_result" != "migration-ci-fixture|upgrade" ]; then
  printf 'upgrade fixture did not survive: %s\n' "$fixture_result" >&2
  exit 1
fi

printf '%s\n' 'Fresh install and upgrade path passed; fixture row survived.'
