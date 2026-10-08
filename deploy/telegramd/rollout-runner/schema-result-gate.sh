#!/usr/bin/env bash
set -euo pipefail
umask 077

readonly SCRIPT_SOURCE="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)/$(basename "${BASH_SOURCE[0]}")"

readonly -a FIELDS=(
  post_migration_migration_60_present
  post_migration_migration_61_present
  post_migration_migration_62_present
  post_migration_migration_63_present
  post_migration_migration_64_present
  post_migration_migration_65_present
  post_migration_migration_66_present
  post_migration_migration_67_present
  post_migration_approved_revision_set_exact
  post_migration_revision_detail_ok
  post_migration_poll_description_entities_schema_ok
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
  post_migration_user_dialog_unread_marks_schema_ok
  post_migration_user_dialog_unread_marks_primary_key_columns_exact
  post_migration_user_dialog_unread_marks_changed_idx_exact
  post_migration_secret_chats_admin_date_idx_exact
  post_migration_secret_chats_participant_date_idx_exact
  post_migration_secret_chats_index_names_exact
)

readonly -a RELEASE_FILES=(
  20261005000060_file_media_metadata.sql
  20261005000061_validate_file_media_metadata.sql
  20261006000062_trusted_message_replies.sql
  20261006000063_dialog_pins.sql
  20261007000064_cloud_drafts.sql
  20261007000065_poll_description_entities.sql
  20261007000066_dialog_unread_marks.sql
  20261008000067_secret_chat_party_date_idx.sql
)
readonly -a RELEASE_SHA256=(
  5c5ee684f5ba218c5c4d9bc8f0a29788bf9ef62a7fd640d04fbf0a7568d220af
  8263920473a6f48b4d0da35e2496d8464b27e5359fe4e383b961c246654773ab
  1ec9f2f98ea2b0ef6e47f75b8f9ae1ec0a484de21baf1392dcbabc118be3bfdb
  91d35b9751cae11e17c2e00c5461df8dc00cca9809f0a227ccedc7f1ac6e3cfb
  e5b13be532a549fe9c22d9cfe39ce97399f83242d0750e9f6fb3047e359aebf0
  ec8d86a1495cd6a1ec2e262d15fca65f3e6c693d95c1f1b6e7745c8b8a533547
  842cd000072a6c1aa74c0d39a62f89497703ae5a79e045f2f6259b2157aba859
  eb94b35a5303dd6ef3d22c8d3164b13284b9c7529071800af6592ff160573388
)
readonly APPROVED_ATLAS_SUM_SHA256=b2c094461a8224de2adde980a7c510e8254d5c0fae2d1e39a0dd9125cae8e9d9
readonly ATLAS_VALIDATOR_IMAGE=arigaio/atlas:1.2.0-alpine

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
    to_regclass('public.cloud_draft_sync'),
    to_regclass('public.user_dialog_unread_marks')
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
    ('post_migration_migration_65_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261007000065'
    )),
    ('post_migration_migration_66_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261007000066'
    )),
    ('post_migration_migration_67_present', EXISTS (
      SELECT 1 FROM applied_revisions
      WHERE version = '20261008000067'
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
      '20261007000064',
      '20261007000065',
      '20261007000066',
      '20261008000067'
    ]::text[]),
    ('post_migration_revision_detail_ok', (
      SELECT count(*) = 8 AND bool_and(
        revision.applied IS NOT NULL
        AND revision.total IS NOT NULL
        AND revision.applied = revision.total
        AND COALESCE(revision.error, '') = ''
        AND ('h1:' || revision.hash) = approved.expected_hash
      )
      FROM atlas_schema_revisions.atlas_schema_revisions revision
      JOIN (VALUES
        ('20261005000060', 'h1:pVa0QAbrHYJKCFIAetI1233DYfejdfRsgQKvH+VYNBw='),
        ('20261005000061', 'h1:JuiEs5kWKJjML/c08w1CySFUgtQVSON5BSsMqyooL5o='),
        ('20261006000062', 'h1:LndEQrLWR5dJx/H3QM3FY0eNcxm0GQqig3E8FCkKeSw='),
        ('20261006000063', 'h1:KsGc/MVs78pwnV2370VaxVWPGUAeI9AMLiFWQgu906Q='),
        ('20261007000064', 'h1:HRrwny26zZtQBWOeQUcfp5rKuwAEIsNoKyILYCZTfzY='),
        ('20261007000065', 'h1:UagmIV9R7m4NEH629GslmqXa+rWeJIOuwnM5AVc66vU='),
        ('20261007000066', 'h1:o3QLcFMfrTkdKsYDmgJFEfbaTSW2Zn+pTly5jHarasc='),
        ('20261008000067', 'h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=')
      ) AS approved(version, expected_hash) ON approved.version = revision.version::text
    )),
    ('post_migration_poll_description_entities_schema_ok', EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_schema = 'public' AND table_name = 'polls'
        AND column_name = 'description_entities' AND data_type = 'jsonb'
        AND is_nullable = 'NO'
        AND regexp_replace(column_default, '[[:space:]]', '', 'g')
          = '''{"version":1,"entities":[]}''::jsonb'
    )),
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
    )),
    ('post_migration_user_dialog_unread_marks_schema_ok',
      to_regclass('public.user_dialog_unread_marks') IS NOT NULL
      AND (SELECT count(*) = 5 FROM information_schema.columns
           WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks')
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks'
          AND column_name = 'owner_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks'
          AND column_name = 'peer_type' AND data_type = 'smallint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks'
          AND column_name = 'peer_id' AND data_type = 'bigint'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks'
          AND column_name = 'unread' AND data_type = 'boolean'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public' AND table_name = 'user_dialog_unread_marks'
          AND column_name = 'changed_at' AND data_type = 'timestamp with time zone'
          AND is_nullable = 'NO' AND column_default IS NULL
      )
      AND (SELECT count(*) = 3 FROM pg_constraint
           WHERE conrelid = to_regclass('public.user_dialog_unread_marks'))
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_unread_marks')
          AND conname = 'user_dialog_unread_marks_pkey'
          AND contype = 'p' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_unread_marks')
          AND conname = 'user_dialog_unread_marks_owner_id_fkey'
          AND contype = 'f' AND confdeltype = 'c' AND convalidated
      )
      AND EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = to_regclass('public.user_dialog_unread_marks')
          AND conname = 'user_dialog_unread_marks_peer'
          AND contype = 'c' AND convalidated
      )
    ),
    ('post_migration_user_dialog_unread_marks_primary_key_columns_exact', EXISTS (
      SELECT 1 FROM constraint_key_columns
      WHERE conrelid = to_regclass('public.user_dialog_unread_marks')
        AND conname = 'user_dialog_unread_marks_pkey' AND contype = 'p'
        AND key_columns = ARRAY['owner_id', 'peer_type', 'peer_id']::text[]
    )),
    ('post_migration_user_dialog_unread_marks_changed_idx_exact', EXISTS (
      SELECT 1 FROM pg_class index_class
      JOIN pg_namespace index_schema ON index_schema.oid = index_class.relnamespace
      JOIN pg_index index_info ON index_info.indexrelid = index_class.oid
      JOIN pg_am access_method ON access_method.oid = index_class.relam
      WHERE index_schema.nspname = 'public'
        AND index_class.relname = 'user_dialog_unread_marks_changed_idx'
        AND index_info.indrelid = to_regclass('public.user_dialog_unread_marks')
        AND access_method.amname = 'btree'
        AND index_info.indisvalid AND index_info.indisready AND index_info.indislive
        AND NOT index_info.indisunique AND NOT index_info.indisprimary
        AND index_info.indpred IS NULL AND index_info.indexprs IS NULL
        AND index_class.reloptions IS NULL
        AND index_info.indnatts = 4 AND index_info.indnkeyatts = 4
        AND ARRAY(
          SELECT attribute.attname::text
          FROM unnest(index_info.indkey) WITH ORDINALITY AS index_key(attnum, ordinal)
          JOIN pg_attribute attribute
            ON attribute.attrelid = index_info.indrelid AND attribute.attnum = index_key.attnum
          ORDER BY index_key.ordinal
        ) = ARRAY['owner_id', 'changed_at', 'peer_type', 'peer_id']::text[]
        AND ARRAY(
          SELECT index_option::integer
          FROM unnest(index_info.indoption) WITH ORDINALITY AS options(index_option, ordinal)
          ORDER BY ordinal
        ) = ARRAY[0, 0, 0, 0]::integer[]
    )),
    ('post_migration_secret_chats_admin_date_idx_exact', EXISTS (
      SELECT 1 FROM pg_class index_class
      JOIN pg_namespace index_schema ON index_schema.oid = index_class.relnamespace
      JOIN pg_index index_info ON index_info.indexrelid = index_class.oid
      JOIN pg_am access_method ON access_method.oid = index_class.relam
      WHERE index_schema.nspname = 'public'
        AND index_class.relname = 'secret_chats_admin_date_idx'
        AND index_info.indrelid = to_regclass('public.secret_chats')
        AND access_method.amname = 'btree'
        AND index_info.indisvalid AND index_info.indisready AND index_info.indislive
        AND NOT index_info.indisunique AND NOT index_info.indisprimary
        AND index_info.indpred IS NULL AND index_info.indexprs IS NULL
        AND index_class.reloptions IS NULL
        AND index_info.indnatts = 2 AND index_info.indnkeyatts = 2
        AND ARRAY(
          SELECT attribute.attname::text
          FROM unnest(index_info.indkey) WITH ORDINALITY AS index_key(attnum, ordinal)
          JOIN pg_attribute attribute
            ON attribute.attrelid = index_info.indrelid AND attribute.attnum = index_key.attnum
          ORDER BY index_key.ordinal
        ) = ARRAY['admin_id', 'date']::text[]
        AND ARRAY(
          SELECT index_option::integer
          FROM unnest(index_info.indoption) WITH ORDINALITY AS options(index_option, ordinal)
          ORDER BY ordinal
        ) = ARRAY[0, 0]::integer[]
    )),
    ('post_migration_secret_chats_participant_date_idx_exact', EXISTS (
      SELECT 1 FROM pg_class index_class
      JOIN pg_namespace index_schema ON index_schema.oid = index_class.relnamespace
      JOIN pg_index index_info ON index_info.indexrelid = index_class.oid
      JOIN pg_am access_method ON access_method.oid = index_class.relam
      WHERE index_schema.nspname = 'public'
        AND index_class.relname = 'secret_chats_participant_date_idx'
        AND index_info.indrelid = to_regclass('public.secret_chats')
        AND access_method.amname = 'btree'
        AND index_info.indisvalid AND index_info.indisready AND index_info.indislive
        AND NOT index_info.indisunique AND NOT index_info.indisprimary
        AND index_info.indpred IS NULL AND index_info.indexprs IS NULL
        AND index_class.reloptions IS NULL
        AND index_info.indnatts = 2 AND index_info.indnkeyatts = 2
        AND ARRAY(
          SELECT attribute.attname::text
          FROM unnest(index_info.indkey) WITH ORDINALITY AS index_key(attnum, ordinal)
          JOIN pg_attribute attribute
            ON attribute.attrelid = index_info.indrelid AND attribute.attnum = index_key.attnum
          ORDER BY index_key.ordinal
        ) = ARRAY['participant_id', 'date']::text[]
        AND ARRAY(
          SELECT index_option::integer
          FROM unnest(index_info.indoption) WITH ORDINALITY AS options(index_option, ordinal)
          ORDER BY ordinal
        ) = ARRAY[0, 0]::integer[]
    )),
    ('post_migration_secret_chats_index_names_exact',
      ARRAY(
        SELECT index_class.relname::text
        FROM pg_class table_class
        JOIN pg_namespace table_schema ON table_schema.oid = table_class.relnamespace
        JOIN pg_index index_info ON index_info.indrelid = table_class.oid
        JOIN pg_class index_class ON index_class.oid = index_info.indexrelid
        WHERE table_schema.nspname = 'public' AND table_class.relname = 'secret_chats'
        ORDER BY index_class.relname
      ) = ARRAY[
        'secret_chats_admin_date_idx',
        'secret_chats_admin_random_id_idx',
        'secret_chats_admin_state_idx',
        'secret_chats_participant_date_idx',
        'secret_chats_participant_state_idx',
        'secret_chats_pkey'
      ]::text[]
      AND NOT EXISTS (
        SELECT 1 FROM pg_class table_class
        JOIN pg_namespace table_schema ON table_schema.oid = table_class.relnamespace
        JOIN pg_index index_info ON index_info.indrelid = table_class.oid
        WHERE table_schema.nspname = 'public' AND table_class.relname = 'secret_chats'
          AND NOT (index_info.indisvalid AND index_info.indisready AND index_info.indislive)
      )
    )
)
SELECT field || E'\t' || COALESCE(ok::text, 'NULL')
FROM schema_checks
ORDER BY field;
SQL
)

readonly -a PRECHECK_FIELDS=(
  precheck_migration_tree_exact
  precheck_atlas_sum_sha256_exact
  precheck_migration_60_sha256_exact
  precheck_migration_61_sha256_exact
  precheck_migration_62_sha256_exact
  precheck_migration_63_sha256_exact
  precheck_migration_64_sha256_exact
  precheck_migration_65_sha256_exact
  precheck_migration_66_sha256_exact
  precheck_migration_67_sha256_exact
  precheck_atlas_checksums_valid
  precheck_migration_mount_clean
  precheck_revision_prefix_exact
  precheck_revisions_complete
  precheck_revision_hashes_exact
  precheck_files_empty
)

readonly PRECHECK_QUERY=$(cat <<'SQL'
WITH approved_revisions(version, expected_hash, ordinal) AS (
  VALUES
    ('20261005000060', 'h1:pVa0QAbrHYJKCFIAetI1233DYfejdfRsgQKvH+VYNBw=', 1),
    ('20261005000061', 'h1:JuiEs5kWKJjML/c08w1CySFUgtQVSON5BSsMqyooL5o=', 2),
    ('20261006000062', 'h1:LndEQrLWR5dJx/H3QM3FY0eNcxm0GQqig3E8FCkKeSw=', 3),
    ('20261006000063', 'h1:KsGc/MVs78pwnV2370VaxVWPGUAeI9AMLiFWQgu906Q=', 4),
    ('20261007000064', 'h1:HRrwny26zZtQBWOeQUcfp5rKuwAEIsNoKyILYCZTfzY=', 5),
    ('20261007000065', 'h1:UagmIV9R7m4NEH629GslmqXa+rWeJIOuwnM5AVc66vU=', 6),
    ('20261007000066', 'h1:o3QLcFMfrTkdKsYDmgJFEfbaTSW2Zn+pTly5jHarasc=', 7),
    ('20261008000067', 'h1:Lux8heOMbxuuDRHoHwo61qwT/B+Nm05v6jFNbXvz2EE=', 8)
), revisions AS (
  SELECT version::text AS version, applied, total,
         COALESCE(error, '') <> '' AS error_present, hash::text AS hash
  FROM atlas_schema_revisions.atlas_schema_revisions
), batch_revisions AS (
  SELECT * FROM revisions WHERE version >= '20261005000060'
), batch_ids AS (
  SELECT COALESCE(array_agg(version ORDER BY version), ARRAY[]::text[]) AS versions
  FROM batch_revisions
), checks(field, ok) AS (
  SELECT 'revision_prefix_exact',
    cardinality(batch_ids.versions) <= 8
    AND batch_ids.versions = COALESCE((
      SELECT array_agg(version ORDER BY ordinal)
      FROM approved_revisions
      WHERE ordinal <= cardinality(batch_ids.versions)
    ), ARRAY[]::text[])
  FROM batch_ids
  UNION ALL
  SELECT 'revisions_complete', NOT EXISTS (
    SELECT 1 FROM revisions
    WHERE applied IS NULL OR total IS NULL OR applied IS DISTINCT FROM total OR error_present
  )
  UNION ALL
  SELECT 'revision_hashes_exact', NOT EXISTS (
    SELECT 1 FROM batch_revisions revision
    LEFT JOIN approved_revisions approved USING (version)
    WHERE approved.version IS NULL OR ('h1:' || revision.hash) IS DISTINCT FROM approved.expected_hash
  )
  UNION ALL
  SELECT 'files_empty', NOT EXISTS (SELECT 1 FROM public.files)
)
SELECT 'check' || E'\t' || field || E'\t' || COALESCE(ok::text, 'NULL')
FROM checks
UNION ALL
SELECT 'revision' || E'\t' || version || E'\t' || COALESCE(applied::text, '<null>') ||
       E'\t' || COALESCE(total::text, '<null>') || E'\t' || error_present::text ||
       E'\t' || COALESCE(hash, '<null>')
FROM revisions
ORDER BY 1;
SQL
)

usage() {
  printf '%s\n' 'usage: schema-result-gate.sh precheck TARGET_SHA CHECKOUT ROOT_ONLY_EVIDENCE_DIR | check ROOT_ONLY_EVIDENCE_DIR TARGET_SHA | check-target TARGET_SHA CHECKOUT ROOT_ONLY_EVIDENCE_DIR' >&2
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

check_migration_mount_clean() {
  local checkout=$1 changes
  changes=$(git -C "$checkout" status --porcelain=v1 --untracked-files=all --ignored=matching -- migrations/) || {
    fail 'cannot inspect migration bind-mount contents'
    return 1
  }
  [ -z "$changes" ] || { fail 'migration bind-mount contains uncommitted files'; return 1; }
}

verify_target_migration_tree() {
  local target_sha=$1 checkout=$2 evidence_dir=$3
  local tree_file atlas_dir atlas_sum_count=0 sql_count=0 release_count=0
  local entry metadata mode type object path basename sequence release_index field_name index
  local file_sha shape_ok=1 all_release_pins=1 atlas_log
  local -a seen_release=()

  tree_file=$(mktemp "$evidence_dir/.migration-tree.XXXXXXXX") || { fail 'cannot initialize migration tree evidence'; return 1; }
  atlas_dir=$(mktemp -d "$evidence_dir/.atlas-migrations.XXXXXXXX") || {
    rm -f -- "$tree_file"
    fail 'cannot initialize Atlas validation directory'
    return 1
  }
  chmod 600 -- "$tree_file" || { rm -f -- "$tree_file"; rm -rf -- "$atlas_dir"; fail 'cannot protect migration tree capture'; return 1; }
  chmod 700 -- "$atlas_dir" || { rm -f -- "$tree_file"; rm -rf -- "$atlas_dir"; fail 'cannot protect Atlas validation directory'; return 1; }

  if ! git -C "$checkout" ls-tree -r -z "$target_sha" -- migrations/ > "$tree_file"; then
    shape_ok=0
  else
    while IFS= read -r -d '' entry; do
      [[ "$entry" == *$'\t'* ]] || { shape_ok=0; continue; }
      metadata=${entry%%$'\t'*}
      path=${entry#*$'\t'}
      read -r mode type object <<< "$metadata"
      [ "$mode" = 100644 ] && [ "$type" = blob ] && [[ "$object" =~ ^[0-9a-f]{40}$ ]] || { shape_ok=0; continue; }
      [[ "$path" == migrations/* ]] || { shape_ok=0; continue; }
      basename=${path#migrations/}
      [[ "$basename" != */* ]] || { shape_ok=0; continue; }

      if [ "$basename" = atlas.sum ]; then
        atlas_sum_count=$((atlas_sum_count + 1))
      elif [[ "$basename" =~ ^[0-9]{14}_.+\.sql$ ]]; then
        sql_count=$((sql_count + 1))
        sequence=${basename:8:6}
        [[ "$sequence" =~ ^[0-9]{6}$ ]] || { shape_ok=0; continue; }
        sequence=$((10#$sequence))
        release_index=-1
        for index in "${!RELEASE_FILES[@]}"; do
          if [ "$basename" = "${RELEASE_FILES[$index]}" ]; then
            release_index=$index
            break
          fi
        done
        if [ "$sequence" -ge 60 ] && [ "$release_index" -lt 0 ]; then shape_ok=0; fi
        [ "$release_index" -lt 0 ] || { release_count=$((release_count + 1)); seen_release[$release_index]=1; }
      else
        shape_ok=0
        continue
      fi

      if ! git -C "$checkout" show "$target_sha:$path" > "$atlas_dir/$basename"; then
        shape_ok=0
        rm -f -- "$atlas_dir/$basename"
      fi
    done < "$tree_file"
  fi

  [ "$atlas_sum_count" -eq 1 ] || shape_ok=0
  [ "$release_count" -eq 8 ] || shape_ok=0
  [ "$sql_count" -gt 0 ] || shape_ok=0
  PRECHECK_RESULTS[precheck_atlas_sum_sha256_exact]=false
  if [ -f "$atlas_dir/atlas.sum" ]; then
    file_sha=$(sha256sum -- "$atlas_dir/atlas.sum") || file_sha=''
    [ "${file_sha%% *}" = "$APPROVED_ATLAS_SUM_SHA256" ] && PRECHECK_RESULTS[precheck_atlas_sum_sha256_exact]=true
  fi

  for index in "${!RELEASE_FILES[@]}"; do
    field_name="precheck_migration_$((index + 60))_sha256_exact"
    PRECHECK_RESULTS[$field_name]=false
    if [ "${seen_release[$index]:-0}" = 1 ] && [ -f "$atlas_dir/${RELEASE_FILES[$index]}" ]; then
      file_sha=$(sha256sum -- "$atlas_dir/${RELEASE_FILES[$index]}") || file_sha=''
      if [ "${file_sha%% *}" = "${RELEASE_SHA256[$index]}" ]; then
        PRECHECK_RESULTS[$field_name]=true
      else
        all_release_pins=0
      fi
    else
      all_release_pins=0
    fi
    [ "${PRECHECK_RESULTS[$field_name]}" = true ] || all_release_pins=0
  done
  [ "$shape_ok" -eq 1 ] && PRECHECK_RESULTS[precheck_migration_tree_exact]=true || PRECHECK_RESULTS[precheck_migration_tree_exact]=false

  PRECHECK_RESULTS[precheck_atlas_checksums_valid]=false
  atlas_log="$evidence_dir/atlas-validation.log"
  : > "$atlas_log" || { rm -f -- "$tree_file"; rm -rf -- "$atlas_dir"; fail 'cannot initialize Atlas validation evidence'; return 1; }
  chmod 600 -- "$atlas_log" || { rm -f -- "$tree_file"; rm -rf -- "$atlas_dir"; fail 'cannot protect Atlas validation evidence'; return 1; }
  if [ "$shape_ok" -eq 1 ] && [ "$all_release_pins" -eq 1 ] && \
     [ "${PRECHECK_RESULTS[precheck_atlas_sum_sha256_exact]}" = true ] && \
     docker run --rm --network none --volume "$atlas_dir:/migrations:ro" \
       "$ATLAS_VALIDATOR_IMAGE" migrate validate --dir file:///migrations > "$atlas_log" 2>&1; then
    PRECHECK_RESULTS[precheck_atlas_checksums_valid]=true
  fi
  rm -f -- "$tree_file"
  rm -rf -- "$atlas_dir"
}

publish_private_file() {
  local source=$1 destination=$2
  chmod 600 -- "$source" || { fail 'cannot protect schema evidence'; return 1; }
  SCHEMA_GATE_SYNC_PHASE=staged sync || { fail 'cannot sync staged schema evidence'; return 1; }
  mv -- "$source" "$destination" || { fail 'cannot publish schema evidence'; return 1; }
  chmod 600 -- "$destination" || { fail 'cannot protect published schema evidence'; return 1; }
  SCHEMA_GATE_SYNC_PHASE=pre-gate sync || { fail 'cannot sync published schema evidence'; return 1; }
}

gate_sha256() {
  local result
  result=$(sha256sum -- "$SCRIPT_SOURCE") || { fail 'cannot hash the pinned schema gate'; return 1; }
  printf '%s' "${result%% *}"
}

capture_schema_precheck() {
  local target_sha=$1 checkout=$2 evidence_dir=$3
  local raw_file revisions_tmp summary_tmp query_rc=0 malformed=0 line field value
  local version applied total error_present hash mapped timestamp gate_sha result start_ids applied_now
  local schema_batch index
  local write_failed=0
  local -a columns STARTING_REVISIONS=() APPLIED_NOW=()
  local -A PRECHECK_RESULTS=() check_seen=()

  [[ "$target_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'precheck target SHA must be a full commit ID'; return 1; }
  check_evidence_dir "$evidence_dir" || return 1
  [ ! -e "$evidence_dir/schema-precheck.tsv" ] && [ ! -L "$evidence_dir/schema-precheck.tsv" ] || {
    fail 'precheck evidence already exists'
    return 1
  }
  [ ! -e "$evidence_dir/schema-precheck-revisions.tsv" ] && [ ! -L "$evidence_dir/schema-precheck-revisions.tsv" ] || {
    fail 'precheck revision evidence already exists'
    return 1
  }

  for field in "${PRECHECK_FIELDS[@]}"; do PRECHECK_RESULTS[$field]=false; done
  for field in revision_prefix_exact revisions_complete revision_hashes_exact files_empty; do check_seen[$field]=0; done
  verify_target_migration_tree "$target_sha" "$checkout" "$evidence_dir" || return 1
  if check_migration_mount_clean "$checkout"; then PRECHECK_RESULTS[precheck_migration_mount_clean]=true; fi

  raw_file=$(mktemp "$evidence_dir/.schema-precheck-query.XXXXXXXX") || { fail 'cannot initialize precheck query capture'; return 1; }
  revisions_tmp=$(mktemp "$evidence_dir/.schema-precheck-revisions.XXXXXXXX") || {
    rm -f -- "$raw_file"
    fail 'cannot initialize precheck revision evidence'
    return 1
  }
  summary_tmp=$(mktemp "$evidence_dir/.schema-precheck.XXXXXXXX") || {
    rm -f -- "$raw_file" "$revisions_tmp"
    fail 'cannot initialize precheck evidence'
    return 1
  }
  printf 'version\tapplied\ttotal\terror_present\thash\n' > "$revisions_tmp" || {
    rm -f -- "$raw_file" "$revisions_tmp" "$summary_tmp"
    fail 'cannot initialize raw revision evidence'
    return 1
  }
  chmod 600 -- "$raw_file" "$revisions_tmp" "$summary_tmp" || {
    rm -f -- "$raw_file" "$revisions_tmp" "$summary_tmp"
    fail 'cannot protect precheck evidence'
    return 1
  }
  if docker compose exec -T postgres psql -X -A -t -v ON_ERROR_STOP=1 \
      -U postgres -d telegram -c "$PRECHECK_QUERY" </dev/null > "$raw_file" 2>/dev/null; then
    query_rc=0
  else
    query_rc=$?
  fi

  while IFS= read -r line || [ -n "$line" ]; do
    IFS=$'\t' read -r -a columns <<< "$line"
    case "${columns[0]:-}" in
      check)
        [ "${#columns[@]}" -eq 3 ] || { malformed=$((malformed + 1)); continue; }
        field=${columns[1]}
        value=${columns[2]}
        case "$field" in
          revision_prefix_exact) mapped=precheck_revision_prefix_exact ;;
          revisions_complete) mapped=precheck_revisions_complete ;;
          revision_hashes_exact) mapped=precheck_revision_hashes_exact ;;
          files_empty) mapped=precheck_files_empty ;;
          *) malformed=$((malformed + 1)); continue ;;
        esac
        if [ "${check_seen[$field]}" -ne 0 ]; then
          malformed=$((malformed + 1))
        elif [ "$value" = true ] || [ "$value" = false ]; then
          PRECHECK_RESULTS[$mapped]=$value
          check_seen[$field]=1
        else
          malformed=$((malformed + 1))
          check_seen[$field]=1
        fi
        ;;
      revision)
        [ "${#columns[@]}" -eq 6 ] || { malformed=$((malformed + 1)); continue; }
        version=${columns[1]}
        applied=${columns[2]}
        total=${columns[3]}
        error_present=${columns[4]}
        hash=${columns[5]}
        [[ "$version" =~ ^[0-9]{14}$ ]] || { malformed=$((malformed + 1)); continue; }
        [[ "$applied" =~ ^[0-9]+$ || "$applied" = '<null>' ]] || { malformed=$((malformed + 1)); continue; }
        [[ "$total" =~ ^[0-9]+$ || "$total" = '<null>' ]] || { malformed=$((malformed + 1)); continue; }
        [ "$error_present" = true ] || [ "$error_present" = false ] || { malformed=$((malformed + 1)); continue; }
        [ -n "$hash" ] || { malformed=$((malformed + 1)); continue; }
        if ! printf '%s\t%s\t%s\t%s\t%s\n' "$version" "$applied" "$total" "$error_present" "$hash" >> "$revisions_tmp"; then
          write_failed=1
          break
        fi
        if [[ "$version" > '20261005000060' || "$version" = '20261005000060' ]]; then
          STARTING_REVISIONS+=("$version")
        fi
        ;;
      *) malformed=$((malformed + 1)) ;;
    esac
  done < "$raw_file"
  rm -f -- "$raw_file"
  [ "$write_failed" -eq 0 ] || { rm -f -- "$revisions_tmp" "$summary_tmp"; fail 'cannot write raw revision evidence'; return 1; }
  for field in "${!check_seen[@]}"; do
    [ "${check_seen[$field]}" -eq 1 ] || malformed=$((malformed + 1))
  done

  [ "$query_rc" -eq 0 ] && [ "$malformed" -eq 0 ] || {
    PRECHECK_RESULTS[precheck_revision_prefix_exact]=false
    PRECHECK_RESULTS[precheck_revisions_complete]=false
    PRECHECK_RESULTS[precheck_revision_hashes_exact]=false
    PRECHECK_RESULTS[precheck_files_empty]=false
  }
  gate_sha=$(gate_sha256) || { rm -f -- "$revisions_tmp" "$summary_tmp"; return 1; }
  start_ids=$(IFS=,; printf '%s' "${STARTING_REVISIONS[*]}")
  result=pass
  for field in "${PRECHECK_FIELDS[@]}"; do
    [ "${PRECHECK_RESULTS[$field]}" = true ] || result=reject
  done
  [ "$query_rc" -eq 0 ] && [ "$malformed" -eq 0 ] || result=reject
  if [ "$result" = pass ] && [ "${#STARTING_REVISIONS[@]}" -le 8 ]; then
    for ((index=${#STARTING_REVISIONS[@]}; index<8; index++)); do
      APPLIED_NOW+=("${RELEASE_FILES[$index]:0:14}")
    done
  fi
  if [ "$result" = reject ]; then
    schema_batch=60-67
    applied_now=unknown
  elif [ "${#STARTING_REVISIONS[@]}" -eq 8 ]; then
    schema_batch=already_applied
    applied_now=none
  else
    schema_batch=60-67
    applied_now=$(IFS=,; printf '%s' "${APPLIED_NOW[*]}")
  fi
  [ -n "$start_ids" ] || start_ids=none
  timestamp=$(date -u +%Y-%m-%dT%H:%M:%SZ) || { rm -f -- "$revisions_tmp" "$summary_tmp"; fail 'cannot timestamp precheck evidence'; return 1; }
  {
    printf 'captured_at_utc=%s\n' "$timestamp"
    printf 'target_sha=%s\n' "$target_sha"
    printf 'gate_sha=%s\n' "$gate_sha"
    printf 'schema_batch=%s\n' "$schema_batch"
    printf 'starting_revision_ids=%s\n' "$start_ids"
    printf 'applied_now=%s\n' "$applied_now"
    printf 'query_exit_status=%s\n' "$query_rc"
    printf 'malformed_row_count=%s\n' "$malformed"
    for field in "${PRECHECK_FIELDS[@]}"; do printf '%s=%s\n' "$field" "${PRECHECK_RESULTS[$field]}"; done
    printf 'precheck_result=%s\n' "$result"
  } > "$summary_tmp" || { rm -f -- "$revisions_tmp" "$summary_tmp"; fail 'cannot write precheck summary'; return 1; }
  publish_private_file "$revisions_tmp" "$evidence_dir/schema-precheck-revisions.tsv" || { rm -f -- "$summary_tmp"; return 1; }
  publish_private_file "$summary_tmp" "$evidence_dir/schema-precheck.tsv" || return 1

  printf 'schema_precheck=%s schema_batch=%s target_sha=%s gate_sha=%s starting_revision_ids=%s applied_now=%s' \
    "$result" "$schema_batch" "$target_sha" "$gate_sha" "$start_ids" "$applied_now"
  for field in "${PRECHECK_FIELDS[@]}"; do printf ' %s=%s' "$field" "${PRECHECK_RESULTS[$field]}"; done
  printf '\n'
  [ "$result" = pass ]
}

check_target_migration_mount() {
  local target_sha=$1 checkout=$2 evidence_dir=$3
  local head_sha gate_sha result migration_clean=false evidence_tmp
  check_evidence_dir "$evidence_dir" || return 1
  [ ! -e "$evidence_dir/schema-target-tree-check.tsv" ] && [ ! -L "$evidence_dir/schema-target-tree-check.tsv" ] || {
    fail 'target migration cleanliness evidence already exists'
    return 1
  }
  head_sha=$(git -C "$checkout" rev-parse HEAD) || head_sha=unknown
  if [ "$head_sha" = "$target_sha" ] && check_migration_mount_clean "$checkout"; then migration_clean=true; fi
  gate_sha=$(gate_sha256) || return 1
  result=pass
  [ "$head_sha" = "$target_sha" ] && [ "$migration_clean" = true ] || result=reject
  evidence_tmp=$(mktemp "$evidence_dir/.schema-target-tree-check.XXXXXXXX") || { fail 'cannot initialize target migration evidence'; return 1; }
  {
    printf 'target_sha=%s\n' "$target_sha"
    printf 'head_sha=%s\n' "$head_sha"
    printf 'gate_sha=%s\n' "$gate_sha"
    printf 'migration_mount_clean=%s\n' "$migration_clean"
    printf 'target_tree_check=%s\n' "$result"
  } > "$evidence_tmp" || { rm -f -- "$evidence_tmp"; fail 'cannot write target migration evidence'; return 1; }
  publish_private_file "$evidence_tmp" "$evidence_dir/schema-target-tree-check.tsv" || return 1
  printf 'schema_target_tree=%s target_sha=%s gate_sha=%s migration_mount_clean=%s\n' "$result" "$target_sha" "$gate_sha" "$migration_clean"
  [ "$result" = pass ]
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
  local evidence_dir=$1 target_sha=$2 raw_file result_file temp_file query_rc=0
  local line field value known malformed=0 timestamp gate_result=pass expected
  local precheck_file precheck_target precheck_gate_sha precheck_result schema_batch start_ids applied_now gate_sha resulting_ids
  local -A seen=() results=()

  [[ "$target_sha" =~ ^[0-9a-f]{40}$ ]] || { fail 'schema check target SHA must be a full commit ID'; return 1; }
  check_evidence_dir "$evidence_dir" || return 1
  precheck_file="$evidence_dir/schema-precheck.tsv"
  [ -f "$precheck_file" ] && [ ! -L "$precheck_file" ] || { fail 'precheck evidence is missing'; return 1; }
  precheck_target=$(sed -n 's/^target_sha=//p' "$precheck_file")
  precheck_gate_sha=$(sed -n 's/^gate_sha=//p' "$precheck_file")
  precheck_result=$(sed -n 's/^precheck_result=//p' "$precheck_file")
  schema_batch=$(sed -n 's/^schema_batch=//p' "$precheck_file")
  start_ids=$(sed -n 's/^starting_revision_ids=//p' "$precheck_file")
  applied_now=$(sed -n 's/^applied_now=//p' "$precheck_file")
  gate_sha=$(gate_sha256) || return 1
  [ "$precheck_target" = "$target_sha" ] && [ "$precheck_gate_sha" = "$gate_sha" ] && \
    [ "$precheck_result" = pass ] || { fail 'schema check does not match a passing precheck'; return 1; }
  [ "$schema_batch" = 60-67 ] || [ "$schema_batch" = already_applied ] || { fail 'precheck batch evidence is malformed'; return 1; }
  [ -n "$start_ids" ] && [ -n "$applied_now" ] || { fail 'precheck revision summary is incomplete'; return 1; }
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
    resulting_ids=$(printf '%s\n' "${RELEASE_FILES[@]%%_*}" | paste -sd, -)
  else
    resulting_ids=unknown
  fi
  printf 'schema_gate=%s schema_batch=%s target_sha=%s gate_sha=%s starting_revision_ids=%s resulting_revision_ids=%s applied_now=%s' \
    "$gate_result" "$schema_batch" "$target_sha" "$gate_sha" "$start_ids" "$resulting_ids" "$applied_now"
  for field in "${FIELDS[@]}"; do printf ' %s=%s' "$field" "${results[$field]}"; done
  printf '\n'
  [ "$gate_result" = pass ]
}

main() {
  case "${1:-}" in
    precheck)
      [ "$#" -eq 4 ] || { usage; return 64; }
      capture_schema_precheck "$2" "$3" "$4"
      ;;
    check)
      [ "$#" -eq 3 ] || { usage; return 64; }
      capture_schema_result "$2" "$3"
      ;;
    check-target)
      [ "$#" -eq 4 ] || { usage; return 64; }
      [[ "$2" =~ ^[0-9a-f]{40}$ ]] || { fail 'target SHA must be a full commit ID'; return 64; }
      check_target_migration_mount "$2" "$3" "$4"
      ;;
    *) usage; return 64 ;;
  esac
}

main "$@"
