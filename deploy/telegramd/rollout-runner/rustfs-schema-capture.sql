WITH gallery_tables(table_name) AS (
    VALUES
        ('user_photos'::text),
        ('profile_photo_state'::text),
        ('profile_upload_receipt'::text),
        ('profile_delete_operation'::text)
),
gallery_schema AS (
    SELECT jsonb_object_agg(
        wanted.table_name,
        jsonb_build_object(
            'table', format('public.%s', wanted.table_name),
            'columns', COALESCE((
                SELECT jsonb_object_agg(
                    attribute.attname,
                    jsonb_build_object(
                        'type', format_type(attribute.atttypid, attribute.atttypmod),
                        'not_null', attribute.attnotnull,
                        'default', pg_get_expr(definition.adbin, definition.adrelid)
                    )
                )
                FROM pg_attribute AS attribute
                LEFT JOIN pg_attrdef AS definition
                    ON definition.adrelid = attribute.attrelid
                   AND definition.adnum = attribute.attnum
                WHERE attribute.attrelid = to_regclass(format('public.%I', wanted.table_name))
                  AND attribute.attnum > 0
                  AND NOT attribute.attisdropped
            ), '{}'::jsonb),
            'constraints', COALESCE((
                SELECT jsonb_object_agg(
                    constraint_row.conname,
                    jsonb_build_object(
                        'type', constraint_row.contype::text,
                        'validated', constraint_row.convalidated,
                        'columns', COALESCE((
                            SELECT jsonb_agg(local_column.attname ORDER BY local_key.ordinality)
                            FROM unnest(constraint_row.conkey) WITH ORDINALITY AS local_key(attnum, ordinality)
                            JOIN pg_attribute AS local_column
                              ON local_column.attrelid = constraint_row.conrelid
                             AND local_column.attnum = local_key.attnum
                        ), '[]'::jsonb),
                        'referenced_table', CASE
                            WHEN constraint_row.contype = 'f'
                            THEN format('%I.%I', referenced_namespace.nspname, referenced_relation.relname)
                            ELSE NULL
                        END,
                        'referenced_columns', CASE
                            WHEN constraint_row.contype = 'f' THEN COALESCE((
                                SELECT jsonb_agg(referenced_column.attname ORDER BY referenced_key.ordinality)
                                FROM unnest(constraint_row.confkey) WITH ORDINALITY AS referenced_key(attnum, ordinality)
                                JOIN pg_attribute AS referenced_column
                                  ON referenced_column.attrelid = constraint_row.confrelid
                                 AND referenced_column.attnum = referenced_key.attnum
                            ), '[]'::jsonb)
                            ELSE NULL
                        END,
                        'on_delete', CASE constraint_row.confdeltype
                            WHEN 'a' THEN 'NO ACTION'
                            WHEN 'r' THEN 'RESTRICT'
                            WHEN 'c' THEN 'CASCADE'
                            WHEN 'n' THEN 'SET NULL'
                            WHEN 'd' THEN 'SET DEFAULT'
                            ELSE NULL
                        END,
                        'on_update', CASE constraint_row.confupdtype
                            WHEN 'a' THEN 'NO ACTION'
                            WHEN 'r' THEN 'RESTRICT'
                            WHEN 'c' THEN 'CASCADE'
                            WHEN 'n' THEN 'SET NULL'
                            WHEN 'd' THEN 'SET DEFAULT'
                            ELSE NULL
                        END,
                        'match', CASE constraint_row.confmatchtype
                            WHEN 's' THEN 'SIMPLE'
                            WHEN 'f' THEN 'FULL'
                            WHEN 'p' THEN 'PARTIAL'
                            ELSE NULL
                        END,
                        'set_null_columns', CASE
                            WHEN constraint_row.contype = 'f' AND constraint_row.confdelsetcols IS NOT NULL THEN COALESCE((
                                SELECT jsonb_agg(set_null_column.attname ORDER BY set_null_key.ordinality)
                                FROM unnest(constraint_row.confdelsetcols) WITH ORDINALITY AS set_null_key(attnum, ordinality)
                                JOIN pg_attribute AS set_null_column
                                  ON set_null_column.attrelid = constraint_row.conrelid
                                 AND set_null_column.attnum = set_null_key.attnum
                            ), '[]'::jsonb)
                            ELSE '[]'::jsonb
                        END,
                        'referenced_index', CASE
                            WHEN constraint_row.contype = 'f' THEN referenced_index.relname
                            ELSE NULL
                        END,
                        'check_expression', CASE
                            WHEN constraint_row.contype = 'c' THEN pg_get_constraintdef(constraint_row.oid, true)
                            ELSE NULL
                        END
                    )
                )
                FROM pg_constraint AS constraint_row
                LEFT JOIN pg_class AS referenced_relation
                  ON referenced_relation.oid = constraint_row.confrelid
                LEFT JOIN pg_namespace AS referenced_namespace
                  ON referenced_namespace.oid = referenced_relation.relnamespace
                LEFT JOIN pg_class AS referenced_index
                  ON referenced_index.oid = constraint_row.conindid
                WHERE constraint_row.conrelid = to_regclass(format('public.%I', wanted.table_name))
            ), '{}'::jsonb),
            'index_names', COALESCE((
                SELECT jsonb_agg(index_relation.relname ORDER BY index_relation.relname)
                FROM pg_index AS index_row
                JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
                WHERE index_row.indrelid = to_regclass(format('public.%I', wanted.table_name))
            ), '[]'::jsonb),
            'index_validity', COALESCE((
                SELECT jsonb_object_agg(index_relation.relname, index_row.indisvalid)
                FROM pg_index AS index_row
                JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
                WHERE index_row.indrelid = to_regclass(format('public.%I', wanted.table_name))
            ), '{}'::jsonb)
        )
    ) AS tables
    FROM gallery_tables AS wanted
)
SELECT jsonb_build_object(
    'migration_68_schema', (
        SELECT jsonb_build_object(
            'table', 'public.files',
            'index_names', COALESCE((
                SELECT jsonb_agg(index_relation.relname ORDER BY index_relation.relname)
                FROM pg_index AS index_row
                JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
                WHERE index_row.indrelid = 'public.files'::regclass
            ), '[]'::jsonb),
            'index_validity', COALESCE((
                SELECT jsonb_object_agg(index_relation.relname, index_row.indisvalid)
                FROM pg_index AS index_row
                JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
                WHERE index_row.indrelid = 'public.files'::regclass
            ), '{}'::jsonb),
            'ownership_index', (
                SELECT jsonb_build_object(
                    'columns', COALESCE((
                        SELECT jsonb_agg(index_column.attname ORDER BY key_column.ordinality)
                        FROM unnest(index_row.indkey::smallint[]) WITH ORDINALITY AS key_column(attnum, ordinality)
                        LEFT JOIN pg_attribute AS index_column
                          ON index_column.attrelid = index_row.indrelid
                         AND index_column.attnum = key_column.attnum
                    ), '[]'::jsonb),
                    'access_method', access_method.amname,
                    'indisunique', index_row.indisunique,
                    'indisprimary', index_row.indisprimary,
                    'indimmediate', index_row.indimmediate,
                    'indisvalid', index_row.indisvalid,
                    'indisready', index_row.indisready,
                    'indislive', index_row.indislive,
                    'indpred', pg_get_expr(index_row.indpred, index_row.indrelid),
                    'indexprs', pg_get_expr(index_row.indexprs, index_row.indrelid),
                    'indnatts', index_row.indnatts,
                    'indnkeyatts', index_row.indnkeyatts,
                    'indoption', COALESCE((
                        SELECT jsonb_agg(key_option.option_value ORDER BY key_option.ordinality)
                        FROM unnest(index_row.indoption::smallint[]) WITH ORDINALITY AS key_option(option_value, ordinality)
                    ), '[]'::jsonb)
                )
                FROM pg_index AS index_row
                JOIN pg_class AS index_relation ON index_relation.oid = index_row.indexrelid
                JOIN pg_am AS access_method ON access_method.oid = index_relation.relam
                WHERE index_row.indexrelid = to_regclass('public.files_id_uploader_id_key')
            )
        )
    ),
    'migration_69_schema', jsonb_build_object('tables', (SELECT tables FROM gallery_schema))
)::text;
