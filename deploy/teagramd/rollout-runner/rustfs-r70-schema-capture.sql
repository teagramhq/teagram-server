WITH r70_tables(table_name) AS (
    VALUES
        ('erasure_outbox'::text),
        ('erasure_epoch'::text),
        ('erasure_epoch_completion'::text)
)
SELECT jsonb_build_object(
    'migration_70_schema', jsonb_build_object(
        'tables', (
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
                                'default', pg_get_expr(definition.adbin, definition.adrelid),
                                'identity', attribute.attidentity::text,
                                'generated', attribute.attgenerated::text,
                                'sequence', pg_get_serial_sequence(
                                    format('public.%I', wanted.table_name),
                                    attribute.attname
                                ) IS NOT NULL
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
                                'deferrable', constraint_row.condeferrable,
                                'initially_deferred', constraint_row.condeferred,
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
            )
            FROM r70_tables AS wanted
        ),
        'inbound_foreign_keys', COALESCE((
            SELECT jsonb_agg(
                jsonb_build_object(
                    'table', format('%I.%I', local_namespace.nspname, local_relation.relname),
                    'name', constraint_row.conname
                )
                ORDER BY local_namespace.nspname, local_relation.relname, constraint_row.conname
            )
            FROM pg_constraint AS constraint_row
            JOIN pg_class AS local_relation ON local_relation.oid = constraint_row.conrelid
            JOIN pg_namespace AS local_namespace ON local_namespace.oid = local_relation.relnamespace
            WHERE constraint_row.contype = 'f'
              AND constraint_row.confrelid IN (
                  SELECT to_regclass(format('public.%I', table_name)) FROM r70_tables
              )
              AND constraint_row.conrelid NOT IN (
                  SELECT to_regclass(format('public.%I', table_name)) FROM r70_tables
              )
        ), '[]'::jsonb),
        'user_triggers', (
            SELECT jsonb_object_agg(
                wanted.table_name,
                COALESCE((
                    SELECT jsonb_agg(trigger_row.tgname ORDER BY trigger_row.tgname)
                    FROM pg_trigger AS trigger_row
                    WHERE trigger_row.tgrelid = to_regclass(format('public.%I', wanted.table_name))
                      AND NOT trigger_row.tgisinternal
                ), '[]'::jsonb)
            )
            FROM r70_tables AS wanted
        )
    )
)::text;
