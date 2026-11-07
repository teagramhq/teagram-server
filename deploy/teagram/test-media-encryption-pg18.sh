#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
TMP=$(mktemp -d /tmp/teagram-media-pg18-test.XXXXXXXX)
readonly TMP
readonly PG18_IMAGE=${PG18_IMAGE:-mirror.gcr.io/library/postgres:18-alpine}
readonly SOURCE_CONTAINER="media-pg18-source-${BASHPID}-${RANDOM}"
readonly RESTORE_CONTAINER="media-pg18-restore-${BASHPID}-${RANDOM}"
readonly CONTAINERS=("$SOURCE_CONTAINER" "$RESTORE_CONTAINER")

cleanup() {
  local status=$? container
  trap - EXIT
  set +e
  for container in "${CONTAINERS[@]}"; do
    if docker inspect --type container "$container" >/dev/null 2>&1; then
      docker stop --time 5 "$container" >/dev/null 2>&1 || status=1
    fi
  done
  case "$TMP" in
    /tmp/teagram-media-pg18-test.????????)
      [[ -d "$TMP" && ! -L "$TMP" ]] && /bin/rm -rf -- "$TMP" || status=1
      ;;
    *) status=1 ;;
  esac
  exit "$status"
}
trap cleanup EXIT

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

REPORT_ROOT="$TMP/reports"
mkdir -m 700 -- "$REPORT_ROOT"
export TEAGRAM_BLOB_MODE_REPORT_ROOT="$REPORT_ROOT"
# shellcheck disable=SC1091
source "$SCRIPT_DIR/ops/blob-encryption-common.inc"
mkdir -m 700 -- "$EVIDENCE_ROOT"

wait_ready() {
  local container=$1
  for _ in {1..60}; do
    if docker exec "$container" pg_isready -h 127.0.0.1 -U postgres -d teagram >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

docker run --detach --rm --name "$SOURCE_CONTAINER" --network none --log-driver none \
  --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
  --env PGDATA=/var/lib/postgresql/18/docker \
  --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=teagram \
  "$PG18_IMAGE" >/dev/null || fail 'cannot start PostgreSQL 18 dump source'
wait_ready "$SOURCE_CONTAINER" || fail 'PostgreSQL 18 dump source did not become ready'

source_version=$(docker exec "$SOURCE_CONTAINER" pg_dump --version) || fail 'cannot inspect pg_dump version'
[[ "$source_version" =~ \(PostgreSQL\)[[:space:]]18\. ]] || fail 'dump source is not PostgreSQL 18'
docker exec "$SOURCE_CONTAINER" psql -X -v ON_ERROR_STOP=1 -U postgres -d teagram \
  -c "CREATE TABLE public.media_reference_fixture (id bigint PRIMARY KEY, object_key text NOT NULL); INSERT INTO public.media_reference_fixture VALUES (1, 'synthetic-fixture-row');" \
  >/dev/null || fail 'cannot create PostgreSQL 18 dump fixture'

DUMP_PATH="$EVIDENCE_ROOT/pgdump-fixture.dump"
docker exec -i "$SOURCE_CONTAINER" pg_dump --format=custom --no-owner --no-privileges \
  -U postgres -d teagram </dev/null > "$DUMP_PATH" 2> "$EVIDENCE_ROOT/pgdump-fixture.stderr" || \
  fail 'PostgreSQL 18 pg_dump did not produce an archive'
chmod 600 -- "$DUMP_PATH" "$EVIDENCE_ROOT/pgdump-fixture.stderr"
[[ -s "$DUMP_PATH" ]] || fail 'PostgreSQL 18 pg_dump produced an empty archive'

docker run --detach --rm --name "$RESTORE_CONTAINER" --network none --log-driver none \
  --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
  --env PGDATA=/var/lib/postgresql/18/docker \
  --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=teagram \
  "$PG18_IMAGE" >/dev/null || fail 'cannot start isolated PostgreSQL 18 restore server'
wait_ready "$RESTORE_CONTAINER" || fail 'isolated PostgreSQL 18 restore server did not become ready'

# This is the exact pre-fix form. PostgreSQL 18 treats the dash as a path
# instead of reading the archive already connected to stdin.
if docker exec -i "$RESTORE_CONTAINER" pg_restore --list - < "$DUMP_PATH" \
  > "$TMP/old-list.txt" 2> "$EVIDENCE_ROOT/pgrestore-explicit-dash.stderr"; then
  fail 'PostgreSQL 18 unexpectedly accepted the explicit stdin marker'
fi
chmod 600 -- "$EVIDENCE_ROOT/pgrestore-explicit-dash.stderr"
old_form_classification=$(report_pg_restore_failure archive-list \
  "$EVIDENCE_ROOT/pgrestore-explicit-dash.stderr" 2>&1) || fail 'cannot classify the PostgreSQL 18 stdin failure'
[[ "$old_form_classification" == 'pg_restore failure phase=archive-list kind=stdin-operand-treated-as-filename' ]] || \
  fail 'PostgreSQL 18 stdin failure classification did not match'

LIST_PATH="$TMP/pgrestore-list.txt"
LIST_STDERR="$EVIDENCE_ROOT/pgrestore-list.stderr"
run_pg_restore_archive_list "$RESTORE_CONTAINER" "$DUMP_PATH" "$LIST_PATH" "$LIST_STDERR" || \
  fail 'the media operation could not validate a PostgreSQL 18 archive'
[[ -s "$LIST_PATH" ]] || fail 'pg_restore returned an empty PostgreSQL 18 archive table of contents'

RESTORE_STDERR="$EVIDENCE_ROOT/pgrestore-database.stderr"
: > "$RESTORE_STDERR"
chmod 600 -- "$RESTORE_STDERR"
run_pg_restore_database "$RESTORE_CONTAINER" "$DUMP_PATH" "$RESTORE_STDERR" || \
  fail 'the media operation could not restore a PostgreSQL 18 archive'
row_count=$(docker exec "$RESTORE_CONTAINER" psql -X -Atq -U postgres -d teagram \
  -c 'SELECT count(*) FROM public.media_reference_fixture') || fail 'cannot verify the restored PostgreSQL 18 fixture'
[[ "$row_count" == 1 ]] || fail 'PostgreSQL 18 restore did not preserve the fixture row'

# Replaying the restore into the populated target produces a real server-side
# pg_restore error. Only its allowlisted kind and phase may reach operator output.
if run_pg_restore_database "$RESTORE_CONTAINER" "$DUMP_PATH" "$EVIDENCE_ROOT/pgrestore-conflict.stderr" \
  > "$TMP/restore-failure.stdout" 2> "$TMP/restore-failure.classification"; then
  fail 'the conflicting PostgreSQL 18 restore unexpectedly succeeded'
fi
grep -Fxq 'pg_restore failure phase=database-restore kind=restore-query-error' \
  "$TMP/restore-failure.classification" || fail 'restore failure did not produce a sanitized classification'
! grep -Eq 'media_reference_fixture|synthetic-fixture-row|ERROR:|DETAIL:' \
  "$TMP/restore-failure.classification" || fail 'restore output exposed database details'
[[ "$(stat -c '%a' "$EVIDENCE_ROOT/pgrestore-conflict.stderr")" == 600 ]] || \
  fail 'restore diagnostics are not private'

BAD_DUMP_PATH="$TMP/corrupt.dump"
printf '%s\n' 'synthetic invalid archive' > "$BAD_DUMP_PATH"
if run_pg_restore_archive_list "$RESTORE_CONTAINER" "$BAD_DUMP_PATH" "$TMP/corrupt-list.txt" \
  "$EVIDENCE_ROOT/pgrestore-corrupt.stderr" > "$TMP/list-failure.stdout" 2> "$TMP/list-failure.classification"; then
  fail 'pg_restore accepted an invalid archive'
fi
grep -Fxq 'pg_restore failure phase=archive-list kind=invalid-archive' \
  "$TMP/list-failure.classification" || fail 'archive-list failure did not produce a sanitized classification'
! grep -Fq 'synthetic invalid archive' "$TMP/list-failure.classification" || \
  fail 'archive-list failure exposed archive contents'
[[ "$(stat -c '%a' "$EVIDENCE_ROOT/pgrestore-corrupt.stderr")" == 600 ]] || \
  fail 'archive-list diagnostics are not private'

printf '%s\n' 'PASS PostgreSQL 18 pg_dump archive validates, restores, and reports sanitized pg_restore failures'
