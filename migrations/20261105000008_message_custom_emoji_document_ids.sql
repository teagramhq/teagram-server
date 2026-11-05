-- Extract custom-emoji document references for indexable history lookups.
CREATE FUNCTION message_custom_emoji_document_ids(entities jsonb)
RETURNS text[]
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT COALESCE(array_agg(entity.value->>'argument'), ARRAY[]::text[])
    FROM jsonb_array_elements(
        CASE
            WHEN jsonb_typeof(entities->'entities') = 'array' THEN entities->'entities'
            ELSE '[]'::jsonb
        END
    ) AS entity(value)
    WHERE jsonb_typeof(entity.value->'type') = 'number'
      AND entity.value->>'type' = '18'
      AND jsonb_typeof(entity.value->'argument') = 'number';
$$;
