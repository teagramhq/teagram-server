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
readonly fresh_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_fresh?sslmode=disable"
readonly upgrade_dsn="postgres://postgres:${password}@${postgres_container}:5432/migration_upgrade?sslmode=disable"

apply_migrations() {
  local migration_dir=$1
  local database_dsn=$2
  docker run --rm \
    --network "$network" \
    --volume "$migration_dir:/migrations:ro" \
    "$atlas_apply_image" \
    migrate apply --dir file:///migrations --url "$database_dsn" --tx-mode file
}

run_atlas_migration_lint() {
  local migration_dir=$1
  local database_dsn=$2
  local migration_count=$3
  "$atlas_lint_binary" migrate hash --dir "file://$migration_dir" >/dev/null
  "$atlas_lint_binary" migrate lint \
    --dir "file://$migration_dir" \
    --dev-url "$database_dsn" \
    --latest "$migration_count"
}

wrap_migration_in_transaction() {
  local migration_file=$1
  local wrapped_file="$migration_file.tmp"
  {
    printf 'BEGIN;\n'
    cat "$migration_file"
    printf '\nCOMMIT;\n'
  } >"$wrapped_file"
  mv "$wrapped_file" "$migration_file"
}

for database in migration_lint migration_fresh migration_upgrade; do
  docker exec "$postgres_container" createdb -U postgres "$database"
done

docker run --rm \
  --volume "$repo_root/migrations:/migrations:ro" \
  "$atlas_apply_image" \
  migrate validate --dir file:///migrations

python3 -B .github/scripts/migration_lock_lint.py --base "$base_ref"

mapfile -t added_sql_migrations < <(
  git -C "$repo_root" diff --name-only --diff-filter=A "$base_ref...HEAD" -- migrations \
    | awk '/\.sql$/ { print }'
)
mkdir -p "$baseline_dir"
git -C "$repo_root" archive "$base_ref" migrations | tar -x -C "$baseline_dir"
chmod -R a+rX "$baseline_dir"
if [ "${#added_sql_migrations[@]}" -gt 0 ]; then
  lint_migration_dir="$temp_dir/atlas-lint-migrations"
  mkdir -p "$lint_migration_dir"
  cp "$repo_root/migrations/"*.sql "$lint_migration_dir/"
  while IFS= read -r base_migration; do
    wrap_migration_in_transaction "$lint_migration_dir/${base_migration##*/}"
  done < <(grep -rliE --include='*.sql' 'LOCK[[:space:]]+TABLE' "$baseline_dir/migrations")

  run_atlas_migration_lint "$lint_migration_dir" "$lint_dsn" "${#added_sql_migrations[@]}"
else
  printf '%s\n' 'Atlas migration lint skipped: no added SQL migrations.'
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
  while IFS= read -r migration; do
    wrap_migration_in_transaction "$migration"
  done < <(grep -rliE --include='*.sql' 'LOCK[[:space:]]+TABLE' "$fixture_dir")

  if run_atlas_migration_lint "$fixture_dir" "$fixture_dsn" 1 >"$lint_output" 2>&1; then
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
docker exec "$postgres_container" createdb -U postgres migration_lint_base_lock
run_atlas_lint_fixture additive pass migration_lint_additive
run_atlas_lint_fixture destructive reject migration_lint_destructive
run_atlas_lint_fixture base-lock pass migration_lint_base_lock

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
