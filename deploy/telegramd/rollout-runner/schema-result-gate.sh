#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly -a FIELDS=(
  post_migration_migration_60_present
  post_migration_migration_61_present
  post_migration_migration_62_present
  post_migration_migration_63_present
  post_migration_migration_64_present
  post_migration_approved_revision_set_exact
  post_migration_files_subtype_constraint_valid
  post_migration_files_media_metadata_constraint_valid
  post_migration_files_empty
  post_migration_files_media_kind_schema_ok
  post_migration_files_width_schema_ok
  post_migration_files_height_schema_ok
  post_migration_reply_to_trusted_default_false
  post_migration_user_dialog_pins_schema_ok
  post_migration_cloud_drafts_schema_ok
  post_migration_cloud_draft_sync_schema_ok
  post_migration_cloud_draft_sync_changed_idx_present
  post_migration_user_dialog_pins_primary_key_columns_exact
  post_migration_user_dialog_pins_position_unique_columns_exact
  post_migration_cloud_drafts_primary_key_columns_exact
  post_migration_cloud_draft_sync_primary_key_columns_exact
)

readonly SCHEMA_QUERY=$(cat <<'SQL'
WITH applied_revisions AS (
  SELECT version::text AS version
  FROM atlas_schema_revisions.atlas_schema_revisions
), constraint_key_columns AS (
  SELECT constraint_info.conrelid, constraint_info.conname, constraint_info.contype,
         ARRAY(
           SELECT attribute.attname::text
           FROM unnest(constraint_info.conkey) WITH ORDINALITY AS constraint_column(attnum, ordinal)
           JOIN pg_attribute attribute
             ON attribute.attrelid = constraint_info.conrelid
            AND attribute.attnum = constraint_column.attnum
           ORDER BY constraint_column.ordinal
         ) AS key_columns
  FROM pg_constraint constraint_info
  WHERE constraint_info.conrelid IN (
    to_regclass('public.user_dialog_pins'),
    to_regclass('public.cloud_drafts'),
    to_regclass('public.cloud_draft_sync')
  )
), schema_checks(field, ok) AS (
  VALUES
    ('post_migration_migration_60_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261005000060'
    )),
    ('post_migration_migration_61_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261005000061'
    )),
    ('post_migration_migration_62_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261006000062'
    )),
    ('post_migration_migration_63_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261006000063'
    )),
    ('post_migration_migration_64_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261007000064'
    )),
    ('post_migration_approved_revision_set_exact', ARRAY(
      SELECT version FROM applied_revisions
      WHERE version >= '20261005000060'
      ORDER BY version
    ) = ARRAY[
      '20261005000060',
      '20261005000061',
      '20261006000062',
      '20261006000063',
      '20261007000064'
    ]::text[]),
    ('post_migration_files_subtype_constraint_valid', EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conrelid = 'public.files'::regclass
        AND conname = 'files_subtype_rights_valid'
        AND contype = 'c' AND convalidated
    )),
    ('post_migration_files_media_metadata_constraint_valid', EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conrelid = 'public.files'::regclass
        AND conname = 'files_media_metadata_valid'
        AND contype = 'c' AND convalidated
    )),
    ('post_migration_files_empty', NOT EXISTS (SELECT 1 FROM public.files)),
    ('post_migration_files_media_kind_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'media_kind' AND data_type = 'text'
        AND is_nullable = 'NO' AND column_default = '''document''::text'
    )),
    ('post_migration_files_width_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'width' AND data_type = 'integer'
        AND is_nullable = 'YES' AND column_default IS NULL
    )),
    ('post_migration_files_height_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'height' AND data_type = 'integer'
        AND is_nullable = 'YES' AND column_default IS NULL
    )),
    ('post_migration_reply_to_trusted_default_false', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'messages'
        AND column_name = 'reply_to_trusted' AND data_type = 'boolean'
        AND is_nullable = 'NO' AND column_default = 'false'
    )),
    ('post_migration_user_dialog_pins_schema_ok',
      to_regclass('public.user_dialog_pins') IS NOT NULL
      AND (SELECT count(*) = 5 FROM information_schema.columns
           WHERE table_schema = 'public' AND table_name = 'user_dialog_pins')
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_pins'
          AND column_name = 'owner_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_pins'
          AND column_name = 'peer_type' AND data_type = 'smallint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_pins'
          AND column_name = 'peer_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_pins'
          AND column_name = 'position' AND data_type = 'smallint'
          AND is_nullable = 'YES' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_pins'
          AND column_name = 'changed_at' AND data_type = 'timestamp with time zone'
          AND is_nullable = 'NO' AND column_default = 'clock_timestamp()'
      )
      AND (SELECT count(*) = 4 FROM pg_constraint WHERE conrelid = to_regclass('public.user_dialog_pins'))
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_pins')
          AND conname = 'user_dialog_pins_pkey' AND contype = 'p' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_pins')
          AND conname = 'user_dialog_pins_owner_id_fkey' AND contype = 'f'
          AND confdeltype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_pins')
          AND conname = 'user_dialog_pins_peer_position'
          AND contype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_pins')
          AND conname = 'user_dialog_pins_position_unique'
          AND contype = 'u' AND convalidated
      )
    ),
    ('post_migration_user_dialog_pins_primary_key_columns_exact', EXISTS (
      SELECT 1 FROM constraint_key_columns
      WHERE conrelid = to_regclass('public.user_dialog_pins')
        AND conname = 'user_dialog_pins_pkey' AND contype = 'p'
        AND key_columns = ARRAY['owner_id', 'peer_type', 'peer_id']::text[]
    )),
    ('post_migration_user_dialog_pins_position_unique_columns_exact', EXISTS (
      SELECT 1 FROM constraint_key_columns
      WHERE conrelid = to_regclass('public.user_dialog_pins')
        AND conname = 'user_dialog_pins_position_unique' AND contype = 'u'
        AND key_columns = ARRAY['owner_id', 'position']::text[]
    )),
    ('post_migration_cloud_drafts_schema_ok',
      to_regclass('public.cloud_drafts') IS NOT NULL
      AND (SELECT count(*) = 7 FROM information_schema.columns
           WHERE table_schema = 'public' AND table_name = 'cloud_drafts')
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'owner_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'peer_type' AND data_type = 'smallint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'peer_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'message' AND data_type = 'text'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'no_webpage' AND data_type = 'boolean'
          AND is_nullable = 'NO' AND column_default = 'false'
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'reply_to_msg_id' AND data_type = 'bigint'
          AND is_nullable = 'YES' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_drafts'
          AND column_name = 'updated_at' AND data_type = 'timestamp with time zone'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND (SELECT count(*) = 4 FROM pg_constraint WHERE conrelid = to_regclass('public.cloud_drafts'))
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_drafts')
          AND conname = 'cloud_drafts_pkey' AND contype = 'p' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_drafts')
          AND conname = 'cloud_drafts_owner_id_fkey' AND contype = 'f'
          AND confdeltype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_drafts')
          AND conname = 'cloud_drafts_peer' AND contype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_drafts')
          AND conname = 'cloud_drafts_reply' AND contype = 'c' AND convalidated
      )
    ),
    ('post_migration_cloud_drafts_primary_key_columns_exact', EXISTS (
      SELECT 1 FROM constraint_key_columns
      WHERE conrelid = to_regclass('public.cloud_drafts')
        AND conname = 'cloud_drafts_pkey' AND contype = 'p'
        AND key_columns = ARRAY['owner_id', 'peer_type', 'peer_id']::text[]
    )),
    ('post_migration_cloud_draft_sync_schema_ok',
      to_regclass('public.cloud_draft_sync') IS NOT NULL
      AND (SELECT count(*) = 4 FROM information_schema.columns
           WHERE table_schema = 'public' AND table_name = 'cloud_draft_sync')
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_draft_sync'
          AND column_name = 'owner_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_draft_sync'
          AND column_name = 'peer_type' AND data_type = 'smallint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_draft_sync'
          AND column_name = 'peer_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'cloud_draft_sync'
          AND column_name = 'changed_at' AND data_type = 'timestamp with time zone'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND (SELECT count(*) = 3 FROM pg_constraint WHERE conrelid = to_regclass('public.cloud_draft_sync'))
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_draft_sync')
          AND conname = 'cloud_draft_sync_pkey' AND contype = 'p' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_draft_sync')
          AND conname = 'cloud_draft_sync_owner_id_fkey' AND contype = 'f'
          AND confdeltype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.cloud_draft_sync')
          AND conname = 'cloud_draft_sync_peer' AND contype = 'c' AND convalidated
      )
    ),
    ('post_migration_cloud_draft_sync_primary_key_columns_exact', EXISTS (
      SELECT 1 FROM constraint_key_columns
      WHERE conrelid = to_regclass('public.cloud_draft_sync')
        AND conname = 'cloud_draft_sync_pkey' AND contype = 'p'
        AND key_columns = ARRAY['owner_id', 'peer_type', 'peer_id']::text[]
    )),
    ('post_migration_cloud_draft_sync_changed_idx_present', EXISTS (
      SELECT 1 FROM pg_class index_class
      JOIN pg_namespace index_schema ON index_schema.oid = index_class.relnamespace
      JOIN pg_index index_info ON index_info.indexrelid = index_class.oid
      WHERE index_schema.nspname = 'public'
        AND index_class.relname = 'cloud_draft_sync_changed_idx'
        AND index_info.indrelid = to_regclass('public.cloud_draft_sync')
        AND index_info.indisvalid AND index_info.indisready
        AND index_info.indnatts = 4 AND index_info.indnkeyatts = 4
        AND ARRAY(
          SELECT attribute.attname::text
          FROM unnest(index_info.indkey) WITH ORDINALITY AS index_key(attnum, ordinal)
          JOIN pg_attribute attribute
            ON attribute.attrelid = index_info.indrelid AND attribute.attnum = index_key.attnum
          ORDER BY index_key.ordinal
        ) = ARRAY['owner_id', 'changed_at', 'peer_type', 'peer_id']::text[]
    ))
)
SELECT field || E'\t' || COALESCE(ok::text, 'NULL')
FROM schema_checks
ORDER BY field;
SQL
)

usage() {
  printf '%s\n' 'usage: schema-result-gate.sh check ROOT_ONLY_EVIDENCE_DIR' >&2
}

fail() {
  printf 'schema gate rejected: %s\n' "$1" >&2
  return 1
}

check_evidence_dir() {
  local dir=$1 mode owner uid
  [ -d "$dir" ] && [ ! -L "$dir" ] || { fail 'evidence directory missing or symlinked'; return 1; }
  mode=$(stat -c %a -- "$dir") || { fail 'cannot inspect evidence directory'; return 1; }
  owner=$(stat -c %u -- "$dir") || { fail 'cannot inspect evidence owner'; return 1; }
  uid=$(id -u) || { fail 'cannot inspect effective user'; return 1; }
  [ "$mode" = 700 ] && [ "$owner" = "$uid" ] || { fail 'evidence directory must be owned by the caller with mode 0700'; return 1; }
}

write_pre_gate_evidence() {
  local timestamp=$1 query_rc=$2 malformed=$3 field write_failed=0
  if ! printf 'captured_at_utc=%s\n' "$timestamp"; then write_failed=1; fi
  if ! printf 'query_exit_status=%s\n' "$query_rc"; then write_failed=1; fi
  if ! printf 'malformed_row_count=%s\n' "$malformed"; then write_failed=1; fi
  for field in "${FIELDS[@]}"; do
    if ! printf '%s=%s\n' "$field" "${results[$field]}"; then write_failed=1; fi
  done
  if ! printf 'pre_gate_evidence=persisted\n'; then write_failed=1; fi
  return "$write_failed"
}

capture_schema_result() {
  local evidence_dir=$1 raw_file result_file temp_file query_rc=0
  local line field value known malformed=0 timestamp gate_result=pass expected
  local -A seen=() results=()

  check_evidence_dir "$evidence_dir" || return 1
  result_file="$evidence_dir/schema-result-gate.tsv"
  [ ! -e "$result_file" ] && [ ! -L "$result_file" ] || { fail 'schema evidence already exists'; return 1; }
  raw_file=$(mktemp "$evidence_dir/.schema-query.XXXXXXXX") || { fail 'cannot initialize query capture'; return 1; }
  temp_file=$(mktemp "$evidence_dir/.schema-result.XXXXXXXX") || { rm -f -- "$raw_file"; fail 'cannot initialize result evidence'; return 1; }
  if ! chmod 600 -- "$raw_file" "$temp_file"; then
    rm -f -- "$raw_file" "$temp_file"
    fail 'cannot protect query capture'; return 1
  fi

  if docker compose exec -T postgres psql -X -A -t -v ON_ERROR_STOP=1 \
      -U postgres -d telegram -c "$SCHEMA_QUERY" </dev/null >"$raw_file" 2>/dev/null; then
    query_rc=0
  else
    query_rc=$?
  fi

  for field in "${FIELDS[@]}"; do
    seen["$field"]=0
    results["$field"]='<missing>'
  done

  while IFS= read -r line || [ -n "$line" ]; do
    if [[ "$line" != *$'\t'* ]]; then
      malformed=$((malformed + 1))
      continue
    fi
    field=${line%%$'\t'*}
    value=${line#*$'\t'}
    known=0
    for expected in "${FIELDS[@]}"; do
      if [ "$field" = "$expected" ]; then known=1; break; fi
    done
    if [ "$known" -ne 1 ]; then
      malformed=$((malformed + 1))
      continue
    elif [[ "${seen[$field]}" -ne 0 ]]; then
      results["$field"]='<duplicate>'
      malformed=$((malformed + 1))
      continue
    elif [[ "$value" == *$'\t'* ]]; then
      value='<malformed>'
      malformed=$((malformed + 1))
    elif [[ "$value" != true && "$value" != false && "$value" != NULL ]]; then
      value='<malformed>'
      malformed=$((malformed + 1))
    fi
    seen["$field"]=$((seen[$field] + 1))
    results["$field"]=$value
  done < "$raw_file"

  timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ) || { rm -f -- "$raw_file" "$temp_file"; fail 'cannot timestamp schema evidence'; return 1; }
  if ! write_pre_gate_evidence "$timestamp" "$query_rc" "$malformed" > "$temp_file"; then
    rm -f -- "$raw_file" "$temp_file"
    fail 'cannot write per-field evidence'; return 1
  fi
  if ! chmod 600 -- "$temp_file" || [ "$(stat -c %a -- "$temp_file")" != 600 ] || ! SCHEMA_GATE_SYNC_PHASE=staged sync; then
    rm -f -- "$raw_file" "$temp_file"
    fail 'cannot protect or sync per-field evidence'; return 1
  fi
  if ! mv -- "$temp_file" "$result_file" || ! chmod 600 -- "$result_file" || ! SCHEMA_GATE_SYNC_PHASE=pre-gate sync; then
    rm -f -- "$raw_file" "$temp_file"
    fail 'cannot persist per-field evidence before gating'; return 1
  fi
  if ! rm -- "$raw_file" || ! SCHEMA_GATE_SYNC_PHASE=pre-gate sync; then
    fail 'cannot remove transient query capture before gating'; return 1
  fi

  if [ "$query_rc" -ne 0 ] || [ "$malformed" -ne 0 ]; then gate_result=reject; fi
  for field in "${FIELDS[@]}"; do
    if [ "${results[$field]}" != true ]; then gate_result=reject; fi
  done
  if ! printf 'gate_result=%s\n' "$gate_result" >> "$result_file" || ! chmod 600 -- "$result_file" || ! SCHEMA_GATE_SYNC_PHASE=decision sync; then
    fail 'cannot persist final schema decision'; return 1
  fi

  if [ "$gate_result" = pass ]; then
    printf 'schema_gate=pass checks=%s query_exit=%s evidence=%s\n' "${#FIELDS[@]}" "$query_rc" "$result_file"
    return 0
  fi
  printf 'schema_gate=reject checks=%s query_exit=%s malformed_rows=%s evidence=%s\n' \
    "${#FIELDS[@]}" "$query_rc" "$malformed" "$result_file" >&2
  return 1
}

main() {
  [ "$#" -eq 2 ] || { usage; return 64; }
  [ "$1" = check ] || { usage; return 64; }
  capture_schema_result "$2"
}

main "$@"
