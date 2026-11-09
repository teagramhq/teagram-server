#!/usr/bin/env bash
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/ops/qa-password-timestamps.sh"
tmp=$(mktemp -d)
cleanup() { /bin/rm -rf -- "$tmp"; }
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
        printf '%s\n' "$query" > "$MOCK_SQL_PATH"
        [[ "${MOCK_PSQL_FAIL:-}" != true ]] || exit 94
        printf '%s\n' "${MOCK_PSQL_OUTPUT:-}"
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
export MOCK_DOCKER_LOG="$tmp/docker.log" MOCK_SQL_PATH="$tmp/query.sql"
export MOCK_PSQL_OUTPUT=$'test1\t2026-01-02T03:04:05.123456Z\ntest2\tNULL'

output=$("$script")
[[ "$output" == "$MOCK_PSQL_OUTPUT" ]]
grep -Fq 'up.updated_at' "$MOCK_SQL_PATH"
grep -Fq 'username.owner_type' "$MOCK_SQL_PATH"
if grep -Eiq 'salt1|salt2|verifier' "$MOCK_SQL_PATH"; then
  printf '%s\n' 'timestamp query contains a forbidden password field' >&2
  exit 1
fi
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 1 ]]

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
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 1 ]]

MOCK_BAD_COMPOSE=postgres
export MOCK_BAD_COMPOSE
expect_failure 1
unset MOCK_BAD_COMPOSE
[[ "$(grep -c '^exec teagram-postgres-1 psql ' "$MOCK_DOCKER_LOG")" == 1 ]]

MOCK_PG_NOT_READY=true
export MOCK_PG_NOT_READY
expect_failure 1
unset MOCK_PG_NOT_READY

MOCK_PSQL_OUTPUT=$'test1\tNULL\ntest2\t2026-11-09T04:39:00.000001Z'
export MOCK_PSQL_OUTPUT
output=$("$script")
[[ "$output" == "$MOCK_PSQL_OUTPUT" ]]

MOCK_PSQL_OUTPUT=$'test1\t2026-01-02T03:04:05Z\ntest2\tNULL'
export MOCK_PSQL_OUTPUT
expect_failure 1

printf '%s\n' 'qa password timestamp fixture cases pass'
