#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly -a FIELDS=(
  migration_60_present
  migration_61_present
  migration_62_present
  files_subtype_constraint_valid
  files_media_metadata_constraint_valid
  files_empty
  files_media_kind_schema_ok
  files_width_schema_ok
  files_height_schema_ok
  reply_to_trusted_default_false
)

readonly SCHEMA_QUERY=$(cat <<'SQL'
WITH schema_checks(field, ok) AS (
  VALUES
    ('migration_60_present', EXISTS (
      SELECT 1 FROM atlas_schema_revisions.atlas_schema_revisions
      WHERE version = '20261005000060'
    )),
    ('migration_61_present', EXISTS (
      SELECT 1 FROM atlas_schema_revisions.atlas_schema_revisions
      WHERE version = '20261005000061'
    )),
    ('migration_62_present', EXISTS (
      SELECT 1 FROM atlas_schema_revisions.atlas_schema_revisions
      WHERE version = '20261006000062'
    )),
    ('files_subtype_constraint_valid', EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conrelid = 'public.files'::regclass
        AND conname = 'files_subtype_rights_valid'
        AND contype = 'c' AND convalidated
    )),
    ('files_media_metadata_constraint_valid', EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conrelid = 'public.files'::regclass
        AND conname = 'files_media_metadata_valid'
        AND contype = 'c' AND convalidated
    )),
    ('files_empty', NOT EXISTS (SELECT 1 FROM public.files)),
    ('files_media_kind_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'media_kind' AND data_type = 'text'
        AND is_nullable = 'NO' AND column_default = '''document''::text'
    )),
    ('files_width_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'width' AND data_type = 'integer'
        AND is_nullable = 'YES' AND column_default IS NULL
    )),
    ('files_height_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'files'
        AND column_name = 'height' AND data_type = 'integer'
        AND is_nullable = 'YES' AND column_default IS NULL
    )),
    ('reply_to_trusted_default_false', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'messages'
        AND column_name = 'reply_to_trusted' AND data_type = 'boolean'
        AND is_nullable = 'NO' AND column_default = 'false'
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
