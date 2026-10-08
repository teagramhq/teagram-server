#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR=$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -P "$SCRIPT_DIR/../../.." && pwd)
SCHEMA_GATE="$SCRIPT_DIR/schema-result-gate.sh"
TARGET_SHA=$(git -C "$REPO_ROOT" rev-parse HEAD)
[[ "$TARGET_SHA" =~ ^[0-9a-f]{40}$ ]] || { printf '%s\n' 'cannot resolve a full target commit ID' >&2; exit 1; }

TMP=$(mktemp -d "${TMPDIR:-/tmp}/main1421-schema-gate.XXXXXXXX")
chmod 700 "$TMP"
PROJECT="main1421-schema-gate-$$-${RANDOM}"
COMPOSE_FILE="$TMP/compose.yml"
export COMPOSE_PROJECT_NAME="$PROJECT"
export COMPOSE_FILE
TEST_DSN='postgres://postgres:schema_gate_test@postgres:5432/telegram?sslmode=disable'
PASS_COUNT=0
FAIL_COUNT=0
FAILURES=()

cat > "$COMPOSE_FILE" <<EOF
services:
  postgres:
    image: postgres:16-alpine
    environment:
      POSTGRES_PASSWORD: schema_gate_test
      POSTGRES_DB: telegram
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres -d telegram"]
      interval: 2s
      timeout: 5s
      retries: 30
  atlas:
    image: arigaio/atlas:1.2.0-alpine
    entrypoint: ["atlas"]
    volumes:
      - type: bind
        source: $REPO_ROOT/migrations
        target: /migrations
        read_only: true
EOF
chmod 600 "$COMPOSE_FILE"

cleanup() {
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  if [ "${KEEP_FIXTURE_ARTIFACTS:-0}" = 1 ]; then
    printf 'fixture_artifacts=%s\n' "$TMP"
  else
    rm -rf -- "$TMP"
  fi
}
trap cleanup EXIT

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  printf 'PASS %s\n' "$1"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  FAILURES+=("$1")
  printf 'FAIL %s\n' "$1"
}

sql() {
  docker compose exec -T postgres psql -X -A -t -v ON_ERROR_STOP=1 \
    -U postgres -d telegram -c "$1" >/dev/null 2>&1
}

read_sql() {
  docker compose exec -T postgres psql -X -A -t -v ON_ERROR_STOP=1 \
    -U postgres -d telegram -c "$1" 2>/dev/null
}

new_evidence() {
  local name=$1
  mkdir -m 700 "$TMP/$name"
  printf '%s' "$TMP/$name"
}

expect_precheck_pass() {
  local name=$1 evidence=$2
  if "$SCHEMA_GATE" precheck "$TARGET_SHA" "$REPO_ROOT" "$evidence" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr" && \
     grep -q "^schema_precheck=pass " "$TMP/$name.stdout" && \
     [ "$(stat -c %a "$evidence/schema-precheck.tsv")" = 600 ] && \
     [ "$(stat -c %a "$evidence/schema-precheck-revisions.tsv")" = 600 ] && \
     ! grep -Eqi 'error_stmt|postgres://|psql stderr' "$evidence"/schema-precheck*.tsv; then
    pass "$name"
  else
    fail "$name"
    cat "$TMP/$name.stdout" "$TMP/$name.stderr" >&2
  fi
}

expect_precheck_reject() {
  local name=$1 evidence
  evidence=$(new_evidence "$name")
  if "$SCHEMA_GATE" precheck "$TARGET_SHA" "$REPO_ROOT" "$evidence" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr"; then
    fail "$name unexpectedly passed"
    cat "$TMP/$name.stdout" "$TMP/$name.stderr" >&2
  elif grep -q '^precheck_result=reject$' "$evidence/schema-precheck.tsv" && \
       [ -f "$evidence/schema-precheck-revisions.tsv" ] && \
       ! grep -Eqi 'error_stmt|postgres://|psql stderr' "$evidence"/schema-precheck*.tsv; then
    pass "$name rejects with private evidence"
  else
    fail "$name did not preserve expected private evidence"
    cat "$TMP/$name.stdout" "$TMP/$name.stderr" >&2
  fi
}

expect_post_reject() {
  local name=$1 expected_field=$2 evidence
  evidence=$(new_evidence "$name")
  cp -- "$FULL_EVIDENCE/schema-precheck.tsv" "$evidence/schema-precheck.tsv"
  cp -- "$FULL_EVIDENCE/schema-precheck-revisions.tsv" "$evidence/schema-precheck-revisions.tsv"
  chmod 600 "$evidence"/*.tsv
  if "$SCHEMA_GATE" check "$evidence" "$TARGET_SHA" >"$TMP/$name.stdout" 2>"$TMP/$name.stderr"; then
    fail "$name unexpectedly passed"
    cat "$TMP/$name.stdout" "$TMP/$name.stderr" >&2
  elif grep -q '^gate_result=reject$' "$evidence/schema-result-gate.tsv" && \
       grep -q "^$expected_field=false$" "$evidence/schema-result-gate.tsv" && \
       ! grep -Eqi 'error_stmt|postgres://|psql stderr' "$evidence"/schema-result-gate.tsv; then
    pass "$name rejects in the post gate"
  else
    fail "$name did not reject on $expected_field"
    cat "$TMP/$name.stdout" "$TMP/$name.stderr" >&2
  fi
}

docker compose up -d --wait postgres >/dev/null
printf '%s\n' 'Starting schema precheck against Atlas 1.2.0 release prefix 60-62.'
if docker compose run --rm --no-deps atlas migrate apply --dir file:///migrations \
    --url "$TEST_DSN" --to-version 20261006000062 >"$TMP/atlas-prefix.log" 2>&1; then
  pass 'Atlas 1.2.0 applies the real migration directory through 62'
else
  fail 'Atlas 1.2.0 failed to apply the real migration directory through 62'
fi

PREFIX_EVIDENCE=$(new_evidence prefix-pass)
expect_precheck_pass 'precheck accepts exact starting prefix 60-62 and empty files' "$PREFIX_EVIDENCE"
if grep -q '^starting_revision_ids=20261005000060,20261005000061,20261006000062$' "$PREFIX_EVIDENCE/schema-precheck.tsv" && \
   grep -q '^applied_now=20261006000063,20261007000064,20261007000065,20261007000066,20261008000067$' "$PREFIX_EVIDENCE/schema-precheck.tsv"; then
  pass 'precheck records the exact starting and pending revision IDs'
else
  fail 'precheck revision ID evidence'
fi

sql "INSERT INTO atlas_schema_revisions.atlas_schema_revisions SELECT (jsonb_populate_record(NULL::atlas_schema_revisions.atlas_schema_revisions, to_jsonb(revision) || jsonb_build_object('version', '20261008000068'))).* FROM atlas_schema_revisions.atlas_schema_revisions AS revision WHERE version = '20261006000062'"
expect_precheck_reject 'precheck rejects an unexpected 68 revision row'
sql "DELETE FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261008000068'"

original_hash=$(read_sql "SELECT hash FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261006000062'")
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET hash = 'fixture-tamper' WHERE version = '20261006000062'"
expect_precheck_reject 'precheck rejects a substituted revision hash'
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET hash = '$original_hash' WHERE version = '20261006000062'"

sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET error = 'fixture failure marker' WHERE version = '20261006000062'"
expect_precheck_reject 'precheck rejects a failed revision row'
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET error = NULL WHERE version = '20261006000062'"

sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET applied = 0 WHERE version = '20261006000062'"
expect_precheck_reject 'precheck rejects an incomplete revision row'
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET applied = 1 WHERE version = '20261006000062'"

sql "DELETE FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261005000061'"
expect_precheck_reject 'precheck rejects a gap in the starting revision prefix'
sql "INSERT INTO atlas_schema_revisions.atlas_schema_revisions SELECT (jsonb_populate_record(NULL::atlas_schema_revisions.atlas_schema_revisions, to_jsonb(revision) || jsonb_build_object('version', '20261005000061', 'hash', 'JuiEs5kWKJjML/c08w1CySFUgtQVSON5BSsMqyooL5o=', 'applied', 2, 'total', 2, 'error', NULL, 'error_stmt', NULL))).* FROM atlas_schema_revisions.atlas_schema_revisions AS revision WHERE version = '20261005000060'"
RESTORED_PREFIX_EVIDENCE=$(new_evidence restored-prefix-pass)
expect_precheck_pass 'precheck passes again after restoring the exact 60-62 prefix' "$RESTORED_PREFIX_EVIDENCE"

sql 'UPDATE server_administration SET election_closed = true WHERE singleton_id = 1'
sql "WITH inserted AS (INSERT INTO users (phone) VALUES ('+15550000000') RETURNING id) INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored) SELECT id, 1, 1, 'application/octet-stream', 'schema-gate-fixture.bin', true FROM inserted"
expect_precheck_reject 'precheck rejects a nonempty files table'
sql "DELETE FROM files WHERE file_name = 'schema-gate-fixture.bin'; DELETE FROM users WHERE phone = '+15550000000'"

if docker compose run --rm --no-deps atlas migrate apply --dir file:///migrations \
    --url "$TEST_DSN" >"$TMP/atlas-full.log" 2>&1; then
  pass 'Atlas 1.2.0 applies the real migration directory through 67'
else
  fail 'Atlas 1.2.0 failed to apply the real migration directory through 67'
fi

FULL_EVIDENCE=$(new_evidence full-pass)
expect_precheck_pass 'precheck accepts the complete 60-67 no-op state' "$FULL_EVIDENCE"
if grep -q '^schema_batch=already_applied$' "$FULL_EVIDENCE/schema-precheck.tsv" && \
   grep -q '^applied_now=none$' "$FULL_EVIDENCE/schema-precheck.tsv"; then
  pass 'complete precheck records the already-applied batch'
else
  fail 'complete precheck no-op evidence'
fi
if "$SCHEMA_GATE" check "$FULL_EVIDENCE" "$TARGET_SHA" >"$TMP/post-pass.stdout" 2>"$TMP/post-pass.stderr" && \
   grep -q '^schema_gate=pass ' "$TMP/post-pass.stdout" && \
   grep -q 'resulting_revision_ids=20261005000060,20261005000061,20261006000062,20261006000063,20261007000064,20261007000065,20261007000066,20261008000067' "$TMP/post-pass.stdout" && \
   grep -q 'post_migration_revision_detail_ok=true' "$TMP/post-pass.stdout" && \
   grep -q '^gate_result=pass$' "$FULL_EVIDENCE/schema-result-gate.tsv"; then
  pass 'actual post gate accepts complete PostgreSQL schema and revisions'
else
  fail 'actual post gate rejects the complete PostgreSQL schema'
  cat "$TMP/post-pass.stdout" "$TMP/post-pass.stderr" >&2
fi

sql "DELETE FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261007000066'"
expect_post_reject 'post gate rejects a missing migration-66 revision' post_migration_migration_66_present
sql "INSERT INTO atlas_schema_revisions.atlas_schema_revisions SELECT (jsonb_populate_record(NULL::atlas_schema_revisions.atlas_schema_revisions, to_jsonb(revision) || jsonb_build_object('version', '20261007000066', 'hash', 'o3QLcFMfrTkdKsYDmgJFEfbaTSW2Zn+pTly5jHarasc=', 'applied', 2, 'total', 2, 'error', NULL, 'error_stmt', NULL))).* FROM atlas_schema_revisions.atlas_schema_revisions AS revision WHERE version = '20261007000065'"

sql "DELETE FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261008000067'"
expect_post_reject 'post gate rejects a missing migration-67 revision' post_migration_migration_67_present
sql "INSERT INTO atlas_schema_revisions.atlas_schema_revisions SELECT (jsonb_populate_record(NULL::atlas_schema_revisions.atlas_schema_revisions, to_jsonb(revision) || jsonb_build_object('version', '20261008000067', 'hash', 'Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=', 'applied', 2, 'total', 2, 'error', NULL, 'error_stmt', NULL))).* FROM atlas_schema_revisions.atlas_schema_revisions AS revision WHERE version = '20261007000066'"

sql "INSERT INTO atlas_schema_revisions.atlas_schema_revisions SELECT (jsonb_populate_record(NULL::atlas_schema_revisions.atlas_schema_revisions, to_jsonb(revision) || jsonb_build_object('version', '20261008000068'))).* FROM atlas_schema_revisions.atlas_schema_revisions AS revision WHERE version = '20261008000067'"
expect_post_reject 'post gate rejects an unexpected 68 revision row' post_migration_approved_revision_set_exact
sql "DELETE FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261008000068'"

original_hash=$(read_sql "SELECT hash FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261008000067'")
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET hash = 'fixture-tamper' WHERE version = '20261008000067'"
expect_post_reject 'post gate rejects a substituted revision hash' post_migration_revision_detail_ok
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET hash = '$original_hash' WHERE version = '20261008000067'"

sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET error = 'fixture failure marker' WHERE version = '20261008000067'"
expect_post_reject 'post gate rejects a failed revision row' post_migration_revision_detail_ok
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET error = NULL WHERE version = '20261008000067'"

original_applied=$(read_sql "SELECT applied FROM atlas_schema_revisions.atlas_schema_revisions WHERE version = '20261008000067'")
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET applied = 0 WHERE version = '20261008000067'"
expect_post_reject 'post gate rejects an incomplete revision row' post_migration_revision_detail_ok
sql "UPDATE atlas_schema_revisions.atlas_schema_revisions SET applied = $original_applied WHERE version = '20261008000067'"

sql "WITH inserted AS (INSERT INTO users (phone) VALUES ('+15550000001') RETURNING id) INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored) SELECT id, 1, 1, 'application/octet-stream', 'schema-gate-fixture.bin', true FROM inserted"
expect_post_reject 'post gate rejects a nonempty files table' post_migration_files_empty
sql "DELETE FROM files WHERE file_name = 'schema-gate-fixture.bin'; DELETE FROM users WHERE phone = '+15550000001'"

sql 'ALTER TABLE user_dialog_unread_marks ALTER COLUMN unread TYPE smallint USING unread::integer::smallint'
expect_post_reject 'post gate rejects a migration-66 column type mismatch' post_migration_user_dialog_unread_marks_schema_ok
sql 'ALTER TABLE user_dialog_unread_marks ALTER COLUMN unread TYPE boolean USING unread <> 0'

sql 'ALTER TABLE user_dialog_unread_marks ADD COLUMN fixture_extra integer'
expect_post_reject 'post gate rejects an extra migration-66 table column' post_migration_user_dialog_unread_marks_schema_ok
sql 'ALTER TABLE user_dialog_unread_marks DROP COLUMN fixture_extra'

sql 'ALTER TABLE user_dialog_unread_marks DROP CONSTRAINT user_dialog_unread_marks_peer'
expect_post_reject 'post gate rejects a missing migration-66 constraint' post_migration_user_dialog_unread_marks_schema_ok
sql 'ALTER TABLE user_dialog_unread_marks ADD CONSTRAINT user_dialog_unread_marks_peer CHECK (peer_type BETWEEN 1 AND 3 AND peer_id > 0)'

sql 'ALTER TABLE user_dialog_unread_marks DROP CONSTRAINT user_dialog_unread_marks_pkey'
sql 'ALTER TABLE user_dialog_unread_marks ADD CONSTRAINT user_dialog_unread_marks_pkey PRIMARY KEY (peer_id, peer_type, owner_id)'
expect_post_reject 'post gate rejects migration-66 primary key column order' post_migration_user_dialog_unread_marks_primary_key_columns_exact
sql 'ALTER TABLE user_dialog_unread_marks DROP CONSTRAINT user_dialog_unread_marks_pkey'
sql 'ALTER TABLE user_dialog_unread_marks ADD CONSTRAINT user_dialog_unread_marks_pkey PRIMARY KEY (owner_id, peer_type, peer_id)'

sql 'DROP INDEX public.user_dialog_unread_marks_changed_idx'
sql 'CREATE INDEX user_dialog_unread_marks_changed_idx ON user_dialog_unread_marks (owner_id, changed_at DESC, peer_type, peer_id)'
expect_post_reject 'post gate rejects nondefault migration-66 index ordering' post_migration_user_dialog_unread_marks_changed_idx_exact
sql 'DROP INDEX public.user_dialog_unread_marks_changed_idx'
sql 'CREATE INDEX user_dialog_unread_marks_changed_idx ON user_dialog_unread_marks (owner_id, changed_at, peer_type, peer_id)'

sql 'DROP INDEX public.secret_chats_admin_date_idx'
sql 'CREATE INDEX secret_chats_admin_date_idx ON secret_chats (admin_id, date DESC)'
expect_post_reject 'post gate rejects nondefault migration-67 admin index ordering' post_migration_secret_chats_admin_date_idx_exact
sql 'DROP INDEX public.secret_chats_admin_date_idx'
sql 'CREATE INDEX secret_chats_admin_date_idx ON secret_chats (admin_id, date)'

sql 'DROP INDEX public.secret_chats_participant_date_idx'
sql 'CREATE INDEX secret_chats_participant_date_idx ON secret_chats (participant_id, date DESC)'
expect_post_reject 'post gate rejects nondefault migration-67 participant index ordering' post_migration_secret_chats_participant_date_idx_exact
sql 'DROP INDEX public.secret_chats_participant_date_idx'
sql 'CREATE INDEX secret_chats_participant_date_idx ON secret_chats (participant_id, date)'

sql 'ALTER INDEX public.secret_chats_admin_date_idx SET (fillfactor = 80)'
expect_post_reject 'post gate rejects nondefault migration-67 index storage options' post_migration_secret_chats_admin_date_idx_exact
sql 'ALTER INDEX public.secret_chats_admin_date_idx RESET (fillfactor)'

sql 'CREATE INDEX secret_chats_fixture_extra_idx ON secret_chats (date)'
expect_post_reject 'post gate rejects an extra secret_chats index' post_migration_secret_chats_index_names_exact
sql 'DROP INDEX public.secret_chats_fixture_extra_idx'

RESTORED_EVIDENCE=$(new_evidence restored-post-pass)
cp -- "$FULL_EVIDENCE/schema-precheck.tsv" "$RESTORED_EVIDENCE/schema-precheck.tsv"
cp -- "$FULL_EVIDENCE/schema-precheck-revisions.tsv" "$RESTORED_EVIDENCE/schema-precheck-revisions.tsv"
chmod 600 "$RESTORED_EVIDENCE"/*.tsv
if "$SCHEMA_GATE" check "$RESTORED_EVIDENCE" "$TARGET_SHA" >"$TMP/restored-post-pass.stdout" 2>"$TMP/restored-post-pass.stderr" && \
   grep -q '^gate_result=pass$' "$RESTORED_EVIDENCE/schema-result-gate.tsv"; then
  pass 'post gate passes again after restoring every catalog and revision mutation'
else
  fail 'post gate did not pass after restoring mutations'
  cat "$TMP/restored-post-pass.stdout" "$TMP/restored-post-pass.stderr" >&2
fi

printf 'postgres_schema_gate_fixture=passed:%s failed:%s\n' "$PASS_COUNT" "$FAIL_COUNT"
if [ "$FAIL_COUNT" -ne 0 ]; then
  printf 'postgres_schema_gate_failures=%s\n' "${FAILURES[*]}"
  exit 1
fi
