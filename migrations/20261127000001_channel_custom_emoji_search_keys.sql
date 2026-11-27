-- Build exact search keys for the set-based visible channel history index.

CREATE FUNCTION public.channel_message_custom_emoji_location(channel_id BIGINT, local_id BIGINT)
RETURNS NUMRANGE
LANGUAGE SQL
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT numrange(
        $1::NUMERIC * 18446744073709551617::NUMERIC
            + $2::NUMERIC + 9223372036854775808::NUMERIC,
        $1::NUMERIC * 18446744073709551617::NUMERIC
            + $2::NUMERIC + 9223372036854775809::NUMERIC,
        '[)'
    );
$$;

CREATE FUNCTION public.message_custom_emoji_document_vector(entities JSONB)
RETURNS TSVECTOR
LANGUAGE SQL
IMMUTABLE
PARALLEL SAFE
AS $$
    SELECT array_to_tsvector(public.message_custom_emoji_document_ids($1));
$$;
