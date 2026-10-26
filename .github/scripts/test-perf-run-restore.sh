#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SCRIPT="$SCRIPT_DIR/perf-run.sh"
TMP=$(mktemp -d /tmp/teagram-perf-restore.XXXXXXXXXX)
readonly TEMP_PREFIX=/tmp/teagram-perf-restore.

cleanup() {
  local suffix
  case "$TMP" in
    "$TEMP_PREFIX"*)
      suffix=${TMP#"$TEMP_PREFIX"}
      case "$suffix" in
        ''|*/*|*[!A-Za-z0-9]*) return 1 ;;
        *) rm -rf -- "$TMP" ;;
      esac
      ;;
    *) return 1 ;;
  esac
}
trap cleanup EXIT

mkdir -m 700 "$TMP/bin" "$TMP/cache" "$TMP/work"
export PERF_RESTORE_LOG="$TMP/restore.log"

cat > "$TMP/bin/docker" <<'DOCKER'
#!/usr/bin/env bash
set -euo pipefail

[[ "$1" == exec && "$2" == perf-pg ]] || exit 90
case "$3" in
  psql|vacuumdb) exit 0 ;;
  pg_restore) printf '%s\n' "$*" >> "$PERF_RESTORE_LOG" ;;
  *) exit 91 ;;
esac
DOCKER
chmod 700 "$TMP/bin/docker"

PATH="$TMP/bin:$PATH" \
PERF_PG_CONTAINER=perf-pg \
PERF_PG_URL=postgres://postgres:perf@localhost:5432/ \
PERF_CACHE="$TMP/cache" \
PERF_WORK="$TMP/work" \
  bash "$SCRIPT" restore

for database in perf_1x perf_100x; do
  call=$(grep -F "pg_restore -U postgres -d $database " "$PERF_RESTORE_LOG") || {
    printf 'FAIL restore command missing for %s\n' "$database" >&2
    exit 1
  }
  [[ "$call" == *" --no-acl "* ]] || {
    printf 'FAIL restore command does not skip database ACLs for %s: %s\n' "$database" "$call" >&2
    exit 1
  }
done

printf '%s\n' 'PASS performance fixture restore skips cluster-role ACLs'
