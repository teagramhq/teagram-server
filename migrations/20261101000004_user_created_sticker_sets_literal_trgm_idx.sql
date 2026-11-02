-- Encode each Unicode code point into fixed-width hex so punctuation and
-- underscores also yield pg_trgm terms. The mapping is reversible and safe
-- for a substring index.
-- atlas:txmode none

CREATE FUNCTION public.user_sticker_set_search_key(value text)
RETURNS text
LANGUAGE SQL
IMMUTABLE
STRICT
PARALLEL SAFE
AS $function$
    SELECT string_agg(lpad(to_hex(ascii(substr(value, char_index, 1))), 6, '0'), '' ORDER BY char_index)
    FROM generate_series(1, char_length(value)) AS char_indexes(char_index)
$function$;
