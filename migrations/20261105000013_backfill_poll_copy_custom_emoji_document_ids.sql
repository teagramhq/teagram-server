-- Backfill retained poll copies in committed batches. Insert triggers populate
-- new copies while the existing rows are reconciled.
-- atlas:txmode none
-- atlas:nolint blocking
-- atlas:lock-waiver: The block commits only bounded 500-row updates; schema changes and indexes use separate migrations.

DO $migration$
DECLARE
    last_scope_id BIGINT := 0;
    last_local_id BIGINT := 0;
    high_scope_id BIGINT;
    high_local_id BIGINT;
    scope_ids BIGINT[];
    local_ids BIGINT[];
    poll_ids BIGINT[];
BEGIN
    SELECT copy.owner_id, copy.local_id
      INTO high_scope_id, high_local_id
      FROM poll_message_copies AS copy
     ORDER BY copy.owner_id DESC, copy.local_id DESC
     LIMIT 1;
    COMMIT;

    WHILE high_scope_id IS NOT NULL LOOP
        SELECT array_agg(batch.owner_id ORDER BY batch.owner_id, batch.local_id),
               array_agg(batch.local_id ORDER BY batch.owner_id, batch.local_id),
               array_agg(batch.poll_id ORDER BY batch.owner_id, batch.local_id)
          INTO scope_ids, local_ids, poll_ids
          FROM (
              SELECT copy.owner_id, copy.local_id, copy.poll_id
                FROM poll_message_copies AS copy
               WHERE (copy.owner_id, copy.local_id) > (last_scope_id, last_local_id)
                 AND (copy.owner_id, copy.local_id) <= (high_scope_id, high_local_id)
               ORDER BY copy.owner_id, copy.local_id
               LIMIT 500
          ) AS batch;

        EXIT WHEN scope_ids IS NULL;

        UPDATE poll_message_copies AS copy
           SET custom_emoji_document_ids = message_custom_emoji_document_ids(poll.description_entities)
          FROM unnest(scope_ids, local_ids, poll_ids) AS batch(owner_id, local_id, poll_id)
          JOIN polls AS poll ON poll.id = batch.poll_id
         WHERE copy.owner_id = batch.owner_id
           AND copy.local_id = batch.local_id
           AND copy.custom_emoji_document_ids IS DISTINCT FROM message_custom_emoji_document_ids(poll.description_entities);

        last_scope_id := scope_ids[array_length(scope_ids, 1)];
        last_local_id := local_ids[array_length(local_ids, 1)];
        COMMIT;
    END LOOP;

    last_scope_id := 0;
    last_local_id := 0;
    high_scope_id := NULL;
    high_local_id := NULL;

    SELECT copy.channel_id, copy.local_id
      INTO high_scope_id, high_local_id
      FROM channel_poll_messages AS copy
     ORDER BY copy.channel_id DESC, copy.local_id DESC
     LIMIT 1;
    COMMIT;

    WHILE high_scope_id IS NOT NULL LOOP
        SELECT array_agg(batch.channel_id ORDER BY batch.channel_id, batch.local_id),
               array_agg(batch.local_id ORDER BY batch.channel_id, batch.local_id),
               array_agg(batch.poll_id ORDER BY batch.channel_id, batch.local_id)
          INTO scope_ids, local_ids, poll_ids
          FROM (
              SELECT copy.channel_id, copy.local_id, copy.poll_id
                FROM channel_poll_messages AS copy
               WHERE (copy.channel_id, copy.local_id) > (last_scope_id, last_local_id)
                 AND (copy.channel_id, copy.local_id) <= (high_scope_id, high_local_id)
               ORDER BY copy.channel_id, copy.local_id
               LIMIT 500
          ) AS batch;

        EXIT WHEN scope_ids IS NULL;

        UPDATE channel_poll_messages AS copy
           SET custom_emoji_document_ids = message_custom_emoji_document_ids(poll.description_entities)
          FROM unnest(scope_ids, local_ids, poll_ids) AS batch(channel_id, local_id, poll_id)
          JOIN polls AS poll ON poll.id = batch.poll_id
         WHERE copy.channel_id = batch.channel_id
           AND copy.local_id = batch.local_id
           AND copy.custom_emoji_document_ids IS DISTINCT FROM message_custom_emoji_document_ids(poll.description_entities);

        last_scope_id := scope_ids[array_length(scope_ids, 1)];
        last_local_id := local_ids[array_length(local_ids, 1)];
        COMMIT;
    END LOOP;
END;
$migration$;
