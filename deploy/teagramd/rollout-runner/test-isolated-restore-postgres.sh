#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SOURCE_POSTGRES_IMAGE=ghcr.io/teagramhq/teagram-server-postgres:16-alpine
SOURCE_POSTGRES_IMAGE_DIGEST=sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea
RESTORE_POSTGRES_IMAGE=ghcr.io/teagramhq/teagram-server-postgres:18-alpine
RESTORE_POSTGRES_IMAGE_DIGEST=sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873
TEST_ROLE_PASSWORD=$(openssl rand -hex 24)
ROLE_SQL="CREATE ROLE browser_acceptance_probe LOGIN PASSWORD '$TEST_ROLE_PASSWORD' NOSUPERUSER NOCREATEDB NOCREATEROLE INHERIT NOREPLICATION NOBYPASSRLS"
TMP=$(mktemp -d "${TMPDIR:-/tmp}/isolated-restore-postgres.XXXXXXXX")
chmod 700 "$TMP"
SOURCE_CONTAINER="main1238-restore-fixture-source-$BASHPID"
SUCCESS_CONTAINER="main1238-restore-fixture-success-$BASHPID"
MALFORMED_CONTAINER="main1238-restore-fixture-malformed-$BASHPID"
UNREVIEWED_CONTAINER="main1238-restore-fixture-unreviewed-$BASHPID"
PRECREATED_CONTAINER="main1238-restore-fixture-precreated-$BASHPID"
CONTAINERS=(
  "$SOURCE_CONTAINER"
  "$SUCCESS_CONTAINER"
  "$MALFORMED_CONTAINER"
  "$UNREVIEWED_CONTAINER"
  "$PRECREATED_CONTAINER"
)

fail() {
  printf 'isolated restore fixture failed: %s\n' "$1" >&2
  exit 1
}

container_present() {
  local name=$1 inspection_error inspect_rc
  if inspection_error=$(docker inspect --type container "$name" 2>&1 >/dev/null); then
    return 0
  else
    inspect_rc=$?
  fi
  if [ "$inspect_rc" -eq 1 ]; then
    case "$inspection_error" in
      "Error: No such object: $name"|"Error: No such container: $name"|"Error response from daemon: No such container: $name") return 1 ;;
    esac
  fi
  printf 'container inspection failed (exit=%s): %s\n' \
    "$inspect_rc" "${inspection_error:-no diagnostic output}" >&2
  return 2
}

cleanup() {
  local status=$? cleanup_failed=0 name state
  trap - EXIT
  for name in "${CONTAINERS[@]}"; do
    if container_present "$name"; then
      if ! docker rm -f "$name" >/dev/null; then
        printf 'could not remove fixture container %s\n' "$name" >&2
        cleanup_failed=1
      fi
      if container_present "$name"; then
        printf 'fixture container remains after cleanup: %s\n' "$name" >&2
        cleanup_failed=1
      else
        state=$?
        [ "$state" -eq 1 ] || cleanup_failed=1
      fi
    else
      state=$?
      [ "$state" -eq 1 ] || cleanup_failed=1
    fi
  done
  rm -rf -- "$TMP"
  if [ "$cleanup_failed" -ne 0 ]; then status=1; fi
  exit "$status"
}
trap cleanup EXIT

docker pull "$SOURCE_POSTGRES_IMAGE@$SOURCE_POSTGRES_IMAGE_DIGEST" >/dev/null
docker pull "$RESTORE_POSTGRES_IMAGE@$RESTORE_POSTGRES_IMAGE_DIGEST" >/dev/null

start_container() {
  local name=$1 image=$2 database=$3
  docker run --detach --name "$name" --network none --log-driver none \
    --tmpfs /var/lib/postgresql:rw,nosuid,nodev \
    --env PGDATA=/var/lib/postgresql/18/docker \
    --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB="$database" \
    "$image" >/dev/null
  for ((attempt=0; attempt<60; attempt++)); do
    if docker exec "$name" pg_isready -h 127.0.0.1 -U postgres -d "$database" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  fail "PostgreSQL container did not become ready: $name"
}

restore_with_runner() {
  local container=$1 database=$2 globals_path=$3 dump_path=$4
  (
    ROLLOUT_RUNNER_SOURCE_ONLY=1
    source "$SCRIPT_DIR/rollout-runner.sh"
    restore_database_dump "$container" "$database" "$globals_path" "$dump_path"
  )
}

fingerprint_catalogs() {
  local container=$1 database=$2 fingerprint
  docker exec -i "$container" psql -X -A -t -v ON_ERROR_STOP=1 \
    -U postgres -d "$database" >"$TMP/$container.catalogs" <<'SQL' || return 1
WITH normalized AS (
  SELECT
    'pg_class'::text AS catalog,
    n.nspname::text AS object_schema,
    c.relname::text AS object_name,
    ''::text AS detail,
    pg_get_userbyid(c.relowner)::text AS owner,
    COALESCE((
      SELECT string_agg(
        format('%s:%s:%s:%s', COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable),
        ',' ORDER BY COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable
      )
      FROM aclexplode(COALESCE(c.relacl, CASE WHEN c.relkind = 'S'
        THEN acldefault('S', c.relowner) ELSE acldefault('r', c.relowner) END)) AS entry
      LEFT JOIN pg_roles AS grantee ON grantee.oid = entry.grantee
      JOIN pg_roles AS grantor ON grantor.oid = entry.grantor
      WHERE entry.privilege_type <> 'MAINTAIN'
    ), '')::text AS acl
  FROM pg_class AS c
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'

  UNION ALL

  SELECT
    'pg_attribute', n.nspname, c.relname, a.attname, '',
    COALESCE((
      SELECT string_agg(
        format('%s:%s:%s:%s', COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable),
        ',' ORDER BY COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable
      )
      FROM aclexplode(a.attacl) AS entry
      LEFT JOIN pg_roles AS grantee ON grantee.oid = entry.grantee
      JOIN pg_roles AS grantor ON grantor.oid = entry.grantor
      WHERE entry.privilege_type <> 'MAINTAIN'
    ), '')
  FROM pg_attribute AS a
  JOIN pg_class AS c ON c.oid = a.attrelid
  JOIN pg_namespace AS n ON n.oid = c.relnamespace
  WHERE a.attnum > 0 AND NOT a.attisdropped
    AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'

  UNION ALL

  SELECT
    'pg_proc', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid),
    pg_get_userbyid(p.proowner),
    COALESCE((
      SELECT string_agg(
        format('%s:%s:%s:%s', COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable),
        ',' ORDER BY COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable
      )
      FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) AS entry
      LEFT JOIN pg_roles AS grantee ON grantee.oid = entry.grantee
      JOIN pg_roles AS grantor ON grantor.oid = entry.grantor
      WHERE entry.privilege_type <> 'MAINTAIN'
    ), '')
  FROM pg_proc AS p
  JOIN pg_namespace AS n ON n.oid = p.pronamespace
  WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'

  UNION ALL

  SELECT
    'pg_namespace', n.nspname, n.nspname, '', pg_get_userbyid(n.nspowner),
    COALESCE((
      SELECT string_agg(
        format('%s:%s:%s:%s', COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable),
        ',' ORDER BY COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable
      )
      FROM aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) AS entry
      LEFT JOIN pg_roles AS grantee ON grantee.oid = entry.grantee
      JOIN pg_roles AS grantor ON grantor.oid = entry.grantor
      WHERE entry.privilege_type <> 'MAINTAIN'
    ), '')
  FROM pg_namespace AS n
  WHERE n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%' ESCAPE '\'

  UNION ALL

  SELECT
    'pg_default_acl', COALESCE(n.nspname, ''), d.defaclobjtype::text,
    pg_get_userbyid(d.defaclrole), pg_get_userbyid(d.defaclrole),
    COALESCE((
      SELECT string_agg(
        format('%s:%s:%s:%s', COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable),
        ',' ORDER BY COALESCE(grantee.rolname, 'PUBLIC'), grantor.rolname,
          entry.privilege_type, entry.is_grantable
      )
      FROM aclexplode(d.defaclacl) AS entry
      LEFT JOIN pg_roles AS grantee ON grantee.oid = entry.grantee
      JOIN pg_roles AS grantor ON grantor.oid = entry.grantor
      WHERE entry.privilege_type <> 'MAINTAIN'
    ), '')
  FROM pg_default_acl AS d
  LEFT JOIN pg_namespace AS n ON n.oid = d.defaclnamespace
)
SELECT format('%s|%s|%s|%s|%s|%s', catalog, object_schema, object_name, detail, owner, acl)
FROM normalized
ORDER BY catalog, object_schema, object_name, detail, owner, acl;
SQL
  fingerprint=$(sha256sum "$TMP/$container.catalogs") || return 1
  printf '%s' "${fingerprint%% *}"
}

expect_restore_failure() {
  local name=$1 container=$2 database=$3 globals_path=$4 dump_path=$5
  if restore_with_runner "$container" "$database" "$globals_path" "$dump_path" \
      >"$TMP/$name.stdout" 2>"$TMP/$name.stderr"; then
    fail "$name restore unexpectedly succeeded"
  fi
}

expect_access_denied() {
  local container=$1 name=$2 statement=$3
  if docker exec "$container" psql -X -v ON_ERROR_STOP=1 -U postgres -d teagram \
      -c 'SET ROLE browser_acceptance_probe' -c "$statement" \
      >"$TMP/$name.stdout" 2>"$TMP/$name.stderr"; then
    fail "browser_acceptance_probe unexpectedly passed $name"
  fi
  grep -Fq 'permission denied' "$TMP/$name.stderr" || fail "$name did not fail for lack of privilege"
}

start_container "$SOURCE_CONTAINER" "$SOURCE_POSTGRES_IMAGE" telegram
docker exec "$SOURCE_CONTAINER" psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres -c "$ROLE_SQL" >/dev/null
docker exec -i "$SOURCE_CONTAINER" psql -X -v ON_ERROR_STOP=1 -U postgres -d telegram <<'SQL'
CREATE SCHEMA restore_fixture AUTHORIZATION postgres;
CREATE TABLE restore_fixture.private_rows (
  id bigint NOT NULL PRIMARY KEY,
  visible_value text NOT NULL,
  private_value text NOT NULL
);
INSERT INTO restore_fixture.private_rows VALUES (1, 'visible column', 'private value');
CREATE SEQUENCE restore_fixture.private_sequence;
CREATE FUNCTION restore_fixture.read_private_value() RETURNS text
  LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, restore_fixture
  AS $$ SELECT private_value FROM restore_fixture.private_rows WHERE id = 1 $$;
ALTER TABLE restore_fixture.private_rows OWNER TO postgres;
ALTER SEQUENCE restore_fixture.private_sequence OWNER TO postgres;
ALTER FUNCTION restore_fixture.read_private_value() OWNER TO postgres;
REVOKE ALL ON FUNCTION restore_fixture.read_private_value() FROM PUBLIC;
GRANT USAGE ON SCHEMA restore_fixture TO browser_acceptance_probe;
GRANT SELECT (visible_value) ON TABLE restore_fixture.private_rows TO browser_acceptance_probe;
GRANT EXECUTE ON FUNCTION restore_fixture.read_private_value() TO browser_acceptance_probe;
ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA restore_fixture
  GRANT USAGE ON SEQUENCES TO browser_acceptance_probe;
SQL

DUMP="$TMP/complete.sql"
GLOBALS="$TMP/globals.sql"
docker exec "$SOURCE_CONTAINER" pg_dump -U postgres telegram > "$DUMP"
[ -s "$DUMP" ] || fail 'PostgreSQL produced an empty dump'
tail -c 200 "$DUMP" | grep -qi 'dump complete' || fail 'PostgreSQL dump is incomplete'
grep -Fq 'browser_acceptance_probe' "$DUMP" || fail 'complete dump does not refer to the reviewed role'
grep -Fq 'ALTER DEFAULT PRIVILEGES' "$DUMP" || fail 'complete dump omits default privileges'
docker exec "$SOURCE_CONTAINER" pg_dumpall --globals-only -U postgres > "$GLOBALS"
[ -s "$GLOBALS" ] || fail 'PostgreSQL produced an empty role globals dump'
grep -Fq 'browser_acceptance_probe' "$GLOBALS" || fail 'role globals dump omits the reviewed login'
DUMP_SHA=$(sha256sum "$DUMP")
DUMP_SHA=${DUMP_SHA%% *}

start_container "$SUCCESS_CONTAINER" "$RESTORE_POSTGRES_IMAGE" teagram
restore_with_runner "$SUCCESS_CONTAINER" teagram "$GLOBALS" "$DUMP" \
  >"$TMP/success.stdout" 2>"$TMP/success.stderr" || {
  cat "$TMP/success.stderr" >&2
  fail 'complete ownership and ACL dump did not restore through the runner gate'
}
[ "$(sha256sum "$DUMP" | awk '{print $1}')" = "$DUMP_SHA" ] || fail 'restore changed the hashed dump'

SOURCE_FINGERPRINT=$(fingerprint_catalogs "$SOURCE_CONTAINER" telegram)
RESTORE_FINGERPRINT=$(fingerprint_catalogs "$SUCCESS_CONTAINER" teagram)
if [ "$SOURCE_FINGERPRINT" != "$RESTORE_FINGERPRINT" ]; then
  diff -u "$TMP/$SOURCE_CONTAINER.catalogs" "$TMP/$SUCCESS_CONTAINER.catalogs" >&2 || true
  fail 'owner or normalized ACL fingerprints differ'
fi

ROLE_ATTRIBUTES=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -F '|' -v ON_ERROR_STOP=1 \
  -U postgres -d postgres -c "SELECT rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolinherit, rolreplication, rolbypassrls, rolpassword IS NULL FROM pg_authid WHERE rolname = 'browser_acceptance_probe'")
[ "$ROLE_ATTRIBUTES" = 't|f|f|f|t|f|f|f' ] || fail 'restored scratch role has unexpected login attributes or no password'
SOURCE_ROLE_LIST=$(docker exec "$SOURCE_CONTAINER" psql -X -qAt -F '|' -v ON_ERROR_STOP=1 \
  -U postgres -d postgres -c "SELECT rolname, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolinherit, rolreplication, rolbypassrls, rolconnlimit, COALESCE(rolvaliduntil::text, 'infinity'), COALESCE(rolpassword, '') FROM pg_authid WHERE left(rolname, 3) <> 'pg_' ORDER BY rolname")
RESTORE_ROLE_LIST=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -F '|' -v ON_ERROR_STOP=1 \
  -U postgres -d postgres -c "SELECT rolname, rolcanlogin, rolsuper, rolcreatedb, rolcreaterole, rolinherit, rolreplication, rolbypassrls, rolconnlimit, COALESCE(rolvaliduntil::text, 'infinity'), COALESCE(rolpassword, '') FROM pg_authid WHERE left(rolname, 3) <> 'pg_' ORDER BY rolname")
[ "$SOURCE_ROLE_LIST" = "$RESTORE_ROLE_LIST" ] || fail 'role login attributes or password hashes differ after restore'
UNREVIEWED_ROLES=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 \
  -U postgres -d postgres -c "SELECT COALESCE(string_agg(rolname, ',' ORDER BY rolname), '') FROM pg_roles WHERE rolname NOT IN ('postgres', 'browser_acceptance_probe') AND rolname NOT LIKE 'pg\_%'")
[ -z "$UNREVIEWED_ROLES" ] || fail 'restore created an unreviewed non-system role'

VISIBLE_VALUE=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 \
  -U postgres -d teagram -c 'SET ROLE browser_acceptance_probe' \
  -c 'SELECT visible_value FROM restore_fixture.private_rows')
[ "$VISIBLE_VALUE" = 'visible column' ] || fail 'column-level grant did not survive restore'
PRIVATE_VALUE=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 \
  -U postgres -d teagram -c 'SET ROLE browser_acceptance_probe' \
  -c 'SELECT restore_fixture.read_private_value()')
[ "$PRIVATE_VALUE" = 'private value' ] || fail 'probe cannot execute the SECURITY DEFINER function'
PRIVATE_COLUMN=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -F '|' -v ON_ERROR_STOP=1 \
  -U postgres -d teagram -c "SELECT has_column_privilege('browser_acceptance_probe', 'restore_fixture.private_rows', 'visible_value', 'SELECT'), has_column_privilege('browser_acceptance_probe', 'restore_fixture.private_rows', 'private_value', 'SELECT'), has_sequence_privilege('browser_acceptance_probe', 'restore_fixture.private_sequence', 'SELECT')")
[ "$PRIVATE_COLUMN" = 't|f|f' ] || fail 'probe has an unexpected column or sequence privilege'
expect_access_denied "$SUCCESS_CONTAINER" table-read 'SELECT * FROM restore_fixture.private_rows'
expect_access_denied "$SUCCESS_CONTAINER" sequence-read 'SELECT last_value FROM restore_fixture.private_sequence'
SOURCE_ROWS=$(docker exec "$SOURCE_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d telegram -c 'SELECT count(*) FROM restore_fixture.private_rows')
RESTORE_ROWS=$(docker exec "$SUCCESS_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 -U postgres -d teagram -c 'SELECT count(*) FROM restore_fixture.private_rows')
[ "$SOURCE_ROWS" = "$RESTORE_ROWS" ] || fail 'table row counts differ after the 16-to-18 restore'
printf 'isolated_restore=pass owner_acl_sha256=%s dump_sha256=%s rows=%s\n' "$SOURCE_FINGERPRINT" "$DUMP_SHA" "$RESTORE_ROWS"

cp -- "$DUMP" "$TMP/malformed.sql"
printf '\nTHIS IS NOT VALID SQL;\n' >> "$TMP/malformed.sql"
start_container "$MALFORMED_CONTAINER" "$RESTORE_POSTGRES_IMAGE" teagram
expect_restore_failure malformed "$MALFORMED_CONTAINER" teagram "$GLOBALS" "$TMP/malformed.sql"
printf 'negative_case=malformed_sql:pass\n'

cp -- "$DUMP" "$TMP/unreviewed-role.sql"
printf '\nGRANT SELECT ON TABLE restore_fixture.private_rows TO unreviewed_role;\n' >> "$TMP/unreviewed-role.sql"
start_container "$UNREVIEWED_CONTAINER" "$RESTORE_POSTGRES_IMAGE" teagram
expect_restore_failure unreviewed-role "$UNREVIEWED_CONTAINER" teagram "$GLOBALS" "$TMP/unreviewed-role.sql"
printf 'negative_case=unreviewed_role:pass\n'

start_container "$PRECREATED_CONTAINER" "$RESTORE_POSTGRES_IMAGE" teagram
docker exec "$PRECREATED_CONTAINER" psql -X -v ON_ERROR_STOP=1 -U postgres -d postgres \
  -c "$ROLE_SQL" >/dev/null
expect_restore_failure precreated-role "$PRECREATED_CONTAINER" teagram "$GLOBALS" "$DUMP"
RESTORED_TABLE=$(docker exec "$PRECREATED_CONTAINER" psql -X -qAt -v ON_ERROR_STOP=1 \
  -U postgres -d teagram -c "SELECT COALESCE(to_regclass('restore_fixture.private_rows')::text, '')")
[ -z "$RESTORED_TABLE" ] || fail 'restore proceeded after scratch role creation failed'
printf 'negative_case=role_precreation:pass\n'
