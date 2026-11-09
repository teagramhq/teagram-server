#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ops/qa-password-timestamps.sh"
DOCKER_BIN=$(command -v docker)
readonly DOCKER_BIN
tmp=$(mktemp -d)
postgres_container=
cleanup() {
  local status=$?
  trap - EXIT
  set +e
  if [[ -n "$postgres_container" ]]; then
    "$DOCKER_BIN" stop --time 5 "$postgres_container" >/dev/null 2>&1 || status=1
  fi
  /bin/rm -rf -- "$tmp" || status=1
  exit "$status"
}
trap cleanup EXIT
mkdir -p "$tmp/bin"

cat > "$tmp/bin/id" <<'ID'
#!/usr/bin/env bash
[[ "$1" == -u ]] || exit 99
printf '%s\n' 0
ID
cat > "$tmp/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$MOCK_DOCKER_LOG"
case "$1" in
  inspect)
    [[ "$2" == --format ]] || exit 91
    case "$4" in
      teagram-teagramd-1) service=teagramd ;;
      teagram-postgres-1) service=postgres ;;
      *) exit 92 ;;
    esac
    [[ "${MOCK_INSPECT_FAIL:-}" != "$service" ]] || exit 97
    compose_files=/opt/teagram/docker-compose.yml,/opt/teagram/docker-compose.override.yml
    [[ "${MOCK_BAD_COMPOSE:-}" != "$service" ]] || compose_files=/wrong/compose.yml
    printf 'running|teagram|%s|%s\n' "$service" "$compose_files"
    ;;
  exec)
    [[ "$2" == teagram-postgres-1 ]] || exit 93
    case "$3" in
      pg_isready)
        [[ "${MOCK_PG_NOT_READY:-}" != true ]]
        ;;
      psql)
        query=
        while (($#)); do
          if [[ "$1" == -c ]]; then
            shift
            query=${1:-}
            break
          fi
          shift
        done
        if [[ "$query" == *'username_mode_orphans'* ]]; then
          printf '%s\n' "$query" > "$MOCK_ORPHAN_SQL_PATH"
          if [[ "${MOCK_ORPHAN_PSQL_FAIL:-}" == true ]]; then
            printf '%s\n' 'fixture psql diagnostic' >&2
            exit 94
          fi
          printf '%s\n' "${MOCK_ORPHAN_OUTPUT:-}"
        else
          printf '%s\n' "$query" > "$MOCK_SQL_PATH"
          if [[ "${MOCK_PSQL_FAIL:-}" == true ]]; then
            printf '%s\n' 'fixture psql diagnostic' >&2
            exit 94
          fi
          printf '%s\n' "${MOCK_PSQL_OUTPUT:-}"
        fi
        ;;
      *) exit 95 ;;
    esac
    ;;
  *) exit 96 ;;
esac
DOCKER
chmod +x "$tmp/bin/id" "$tmp/bin/docker"

export PATH="$tmp/bin:$PATH"
export OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export MOCK_DOCKER_LOG="$tmp/docker.log" MOCK_SQL_PATH="$tmp/query.sql" MOCK_ORPHAN_SQL_PATH="$tmp/orphan-query.sql"
export MOCK_PSQL_OUTPUT=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tPRESENT\tABSENT\tNONE\tNOT_STORED\t2026-01-02T03:04:05.123456Z\ntest1\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-02-03T04:05:06.123456Z\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\tNULL'
export MOCK_ORPHAN_OUTPUT=$'5001\t2026-01-10T11:12:13.000000Z\tPRESENT\t2026-01-11T12:13:14.123456Z\n5002\t2026-01-12T00:00:00.000000Z\tABSENT\tNOT_APPLICABLE'

expected_script_output() {
  printf '%s\n' "$MOCK_PSQL_OUTPUT" \
    'unlinked_username_users (id, created_at, password_row, password_updated_at):' \
    "${MOCK_ORPHAN_OUTPUT:-NONE}"
}

output=$("$script")
[[ "$output" == "$(expected_script_output)" ]]
grep -Fq 'up.updated_at' "$MOCK_SQL_PATH"
grep -Fq 'username.owner_type' "$MOCK_SQL_PATH"
grep -Fq 'username.owner_id' "$MOCK_SQL_PATH"
grep -Fq "users.username = targets.handle" "$MOCK_SQL_PATH"
grep -Fq "VALUES ('qa'), ('qa2'), ('test1'), ('test2')" "$MOCK_SQL_PATH"
grep -Fq "'UNRESOLVED_BY_HANDLE'" "$MOCK_SQL_PATH"
grep -Fq "'NOT_STORED'" "$MOCK_SQL_PATH"
if grep -Eiq 'salt1|salt2|verifier' "$MOCK_SQL_PATH"; then
  printf '%s\n' 'timestamp query contains a forbidden password field' >&2
  exit 1
fi
if grep -Eiq '\b(phone|first_name|last_name|salt1|salt2|verifier)\b|users\.username' "$MOCK_ORPHAN_SQL_PATH"; then
  printf '%s\n' 'orphan query contains an unapproved account field' >&2
  exit 1
fi
grep -Fq "users.login_mode = 'username'" "$MOCK_ORPHAN_SQL_PATH"
grep -Fq "username.owner_type = 'user' AND username.owner_id = users.id" "$MOCK_ORPHAN_SQL_PATH"
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 2 ]]

expect_failure() {
  local expected=$1 status=0 output
  shift
  output=$("$script" "$@" 2>/dev/null) || status=$?
  [[ "$status" == "$expected" && -z "$output" ]]
}

expect_failure 64 extra
OPS_SHA=invalid
export OPS_SHA
expect_failure 1
OPS_SHA=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
export OPS_SHA
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 2 ]]

MOCK_BAD_COMPOSE=postgres
export MOCK_BAD_COMPOSE
expect_failure 1
unset MOCK_BAD_COMPOSE
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 2 ]]

MOCK_INSPECT_FAIL=teagramd
export MOCK_INSPECT_FAIL
expect_failure 1
unset MOCK_INSPECT_FAIL
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 2 ]]

MOCK_PG_NOT_READY=true
export MOCK_PG_NOT_READY
expect_failure 1
unset MOCK_PG_NOT_READY

MOCK_PSQL_OUTPUT=$'qa\tMISSING_TARGET_USER\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\nqa2\tNOT_A_USER_HANDLE\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\ntest1\tMULTIPLE\tPRESENT\tYES\tNOT_STORED\tAMBIGUOUS\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-11-09T04:39:00.000001Z'
export MOCK_PSQL_OUTPUT
output=$("$script")
[[ "$output" == "$(expected_script_output)" ]]

MOCK_PSQL_OUTPUT=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tPRESENT\tABSENT\tNONE\tNOT_STORED\t2026-11-09T04:39:00.000001Z\ntest1\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-11-09T04:39:00.000001Z\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\tNULL'
export MOCK_PSQL_OUTPUT
output=$("$script")
[[ "$output" == "$(expected_script_output)" ]]

MOCK_PSQL_OUTPUT=$'test1\t2026-01-02T03:04:05.123456Z\ntest2\tNULL'
export MOCK_PSQL_OUTPUT
MOCK_PSQL_FAIL=true
export MOCK_PSQL_FAIL
if failure_output=$("$script" 2>&1); then
  printf '%s\n' 'password query failure unexpectedly succeeded' >&2
  exit 1
else
  failure_status=$?
fi
[[ "$failure_status" == 1 ]]
grep -Fxq 'fixture psql diagnostic' <<< "$failure_output"
grep -Fxq 'qa-password-timestamps: account status query failed' <<< "$failure_output"
unset MOCK_PSQL_FAIL

MOCK_PSQL_OUTPUT=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tMISSING_TARGET_USER\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\ntest1\tMULTIPLE\tPRESENT\tYES\tNOT_STORED\tAMBIGUOUS\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-01-02T03:04:05.123456Z'
export MOCK_PSQL_OUTPUT
MOCK_ORPHAN_PSQL_FAIL=true
export MOCK_ORPHAN_PSQL_FAIL
if failure_output=$("$script" 2>&1); then
  printf '%s\n' 'orphan query failure unexpectedly succeeded' >&2
  exit 1
else
  failure_status=$?
fi
[[ "$failure_status" == 1 ]]
grep -Fxq 'fixture psql diagnostic' <<< "$failure_output"
grep -Fxq 'qa-password-timestamps: orphan account query failed' <<< "$failure_output"
unset MOCK_ORPHAN_PSQL_FAIL

MOCK_PSQL_OUTPUT=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tPRESENT\tABSENT\tNONE\tNOT_STORED\t2026-01-02T03:04:05Z\ntest1\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-01-02T03:04:05.123456Z\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\tNULL'
export MOCK_PSQL_OUTPUT
expect_failure 1

MOCK_PSQL_OUTPUT=$'qa\tUNRECOGNIZED_STATE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tMISSING_TARGET_USER\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\ntest1\tMULTIPLE\tPRESENT\tYES\tNOT_STORED\tAMBIGUOUS\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-01-02T03:04:05.123456Z'
export MOCK_PSQL_OUTPUT
expect_failure 1

MOCK_PSQL_OUTPUT=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tMISSING_TARGET_USER\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\ntest1\tMULTIPLE\tPRESENT\tYES\tNOT_STORED\tAMBIGUOUS\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-01-02T03:04:05.123456Z'
export MOCK_PSQL_OUTPUT
MOCK_ORPHAN_OUTPUT=$'invalid\t2026-01-10T11:12:13.000000Z\tPRESENT\t2026-01-11T12:13:14.123456Z'
export MOCK_ORPHAN_OUTPUT
expect_failure 1
MOCK_ORPHAN_OUTPUT=$'5001\t2026-01-10T11:12:13.000000Z\tPRESENT\t2026-01-11T12:13:14.123456Z\n5002\t2026-01-12T00:00:00.000000Z\tABSENT\tNOT_APPLICABLE'
export MOCK_ORPHAN_OUTPUT
MOCK_ORPHAN_OUTPUT=
export MOCK_ORPHAN_OUTPUT
output=$("$script")
[[ "$output" == "$(expected_script_output)" ]]
MOCK_ORPHAN_OUTPUT=$'5001\t2026-01-10T11:12:13.000000Z\tPRESENT\t2026-01-11T12:13:14.123456Z\n5002\t2026-01-12T00:00:00.000000Z\tABSENT\tNOT_APPLICABLE'
export MOCK_ORPHAN_OUTPUT

postgres_container="qa-password-query-pg18-${BASHPID}-${RANDOM}"
postgres_image=${QA_PASSWORD_QUERY_PG18_IMAGE:-mirror.gcr.io/library/postgres:18-alpine}
"$DOCKER_BIN" run --detach --rm --name "$postgres_container" --network none --log-driver none \
  --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
  --env PGDATA=/var/lib/postgresql/18/docker \
  --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=teagram \
  "$postgres_image" >/dev/null
postgres_ready=false
for _ in {1..60}; do
  if "$DOCKER_BIN" exec "$postgres_container" pg_isready -h 127.0.0.1 -U postgres -d teagram >/dev/null 2>&1; then
    postgres_ready=true
    break
  fi
  sleep 1
done
[[ "$postgres_ready" == true ]] || {
  printf '%s\n' 'isolated PostgreSQL fixture did not become ready' >&2
  exit 1
}
"$DOCKER_BIN" exec -i "$postgres_container" psql -X -v ON_ERROR_STOP=1 -U postgres -d teagram <<'SQL' >/dev/null
CREATE TABLE users (id BIGINT PRIMARY KEY, username TEXT, login_mode TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL);
CREATE TABLE usernames (handle TEXT PRIMARY KEY, owner_type TEXT NOT NULL, owner_id BIGINT NOT NULL);
CREATE TABLE user_passwords (user_id BIGINT PRIMARY KEY, updated_at TIMESTAMPTZ NOT NULL);
INSERT INTO usernames (handle, owner_type, owner_id) VALUES
  ('qa2', 'user', 2002),
  ('test1', 'user', 3001),
  ('test2', 'user', 4001);
INSERT INTO users (id, username, login_mode, created_at) VALUES
  (3001, 'test1', 'phone', '2025-12-01T00:00:00Z'),
  (3002, 'test1', 'phone', '2025-12-02T00:00:00Z'),
  (4001, 'test2', 'username', '2025-12-03T00:00:00Z'),
  (5001, 'orphan-a', 'username', '2026-01-10T11:12:13Z'),
  (5002, NULL, 'username', '2026-01-12T00:00:00Z');
INSERT INTO user_passwords (user_id, updated_at) VALUES
  (4001, '2026-01-02T03:04:05.123456Z'),
  (5001, '2026-01-11T12:13:14.123456Z');
SQL
postgres_output=$("$DOCKER_BIN" exec -i "$postgres_container" psql -X -A -t -q -F $'\t' \
  -U postgres -d teagram -v ON_ERROR_STOP=1 < "$MOCK_SQL_PATH")
expected_postgres_output=$'qa\tUNRESOLVED_BY_HANDLE\tABSENT\tNONE\tNOT_STORED\tUNKNOWN\nqa2\tMISSING_TARGET_USER\tPRESENT\tNO\tNOT_STORED\tUNKNOWN\ntest1\tMULTIPLE\tPRESENT\tYES\tNOT_STORED\tAMBIGUOUS\ntest2\tPRESENT\tPRESENT\tYES\tNOT_STORED\t2026-01-02T03:04:05.123456Z'
[[ "$postgres_output" == "$expected_postgres_output" ]] || {
  printf '%s\n' 'PostgreSQL query output did not match the seeded account states' >&2
  exit 1
}
printf '%s\n' 'seeded PostgreSQL query output:' "$postgres_output"
postgres_orphan_output=$("$DOCKER_BIN" exec -i "$postgres_container" psql -X -A -t -q -F $'\t' \
  -U postgres -d teagram -v ON_ERROR_STOP=1 < "$MOCK_ORPHAN_SQL_PATH")
expected_postgres_orphan_output=$'5001\t2026-01-10T11:12:13.000000Z\tPRESENT\t2026-01-11T12:13:14.123456Z\n5002\t2026-01-12T00:00:00.000000Z\tABSENT\tNOT_APPLICABLE'
[[ "$postgres_orphan_output" == "$expected_postgres_orphan_output" ]] || {
  printf '%s\n' 'PostgreSQL orphan query output did not match the seeded accounts' >&2
  exit 1
}
printf '%s\n' 'seeded PostgreSQL orphan query output:' "$postgres_orphan_output"

printf '%s\n' 'qa password timestamp fixture cases pass'
