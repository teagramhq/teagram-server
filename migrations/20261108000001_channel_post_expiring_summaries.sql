-- Two summary scopes for live content posts that carry an expiry: scope 2 is
-- keyed by local ID (floor-bounded counts) and scope 3 by expiry time in epoch
-- microseconds (channel-wide count of posts at or before a time). Both are
-- time-invariant, so group history reads the channel-wide expiring total and
-- expired-unswept backlog in O(log n) and then runs the cheaper exact scan:
-- the expired rows above the floor, or the unexpired rows above the floor
-- subtracted from the expiring suffix total. Per-open cost follows the smaller
-- of the backlog and the live expiring posts.
--
-- Online-safe and additive: no source row is read or rewritten here. Existing
-- channels start with expiring_ready = false and history keeps the previous
-- expired-row count for them until
-- `teagramd maintenance initialize-channel-post-expiring-summaries` rebuilds
-- their nodes, a bounded batch of channels per run. New channels are inserted
-- ready. Reversal: drop the index, the column and the scope-2 and scope-3
-- rows, then restore the previous function bodies.
ALTER TABLE channel_post_summary_state
    ADD COLUMN expiring_ready BOOL NOT NULL DEFAULT false;

-- The weaker replacement is added NOT VALID so no table scan runs under the
-- exclusive lock; the next migration validates it without blocking writers.
ALTER TABLE channel_post_summaries
    ADD CONSTRAINT channel_post_summaries_scope_check
        CHECK ((scope_kind = 0 AND author_id = 0) OR
               (scope_kind = 1 AND author_id > 0) OR
               (scope_kind IN (2, 3) AND author_id = 0)) NOT VALID;

-- Expiry time as a nonnegative microsecond key for the scope-3 tree. Instants
-- before the epoch share key 0; they are expired either way.
CREATE FUNCTION channel_post_expiry_key(p_expires_at TIMESTAMPTZ)
RETURNS BIGINT
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
    SELECT GREATEST((EXTRACT(EPOCH FROM p_expires_at) * 1000000)::BIGINT, 0::BIGINT);
$$;
ALTER TABLE channel_post_summaries
    DROP CONSTRAINT channel_post_summaries_check;

CREATE OR REPLACE FUNCTION channel_post_summary_mark_new_channel()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    INSERT INTO channel_post_summary_state (channel_id, version, ready, expiring_ready)
    VALUES (NEW.id, 1, true, true)
    ON CONFLICT (channel_id) DO NOTHING;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION channel_post_summary_maintain_source()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
    old_live BOOL := false;
    new_live BOOL := false;
    old_expiring BOOL := false;
    new_expiring BOOL := false;
    old_expiry_key BIGINT;
    new_expiry_key BIGINT;
    old_channel_id BIGINT;
    old_local_id BIGINT;
    old_from_id BIGINT;
    new_channel_id BIGINT;
    new_local_id BIGINT;
    new_from_id BIGINT;
    target_channel_id BIGINT;
    state_version SMALLINT;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        old_channel_id := OLD.channel_id;
        old_local_id := OLD.local_id;
        old_from_id := OLD.from_id;
        old_live := NOT OLD.deleted AND OLD.action_type = 0;
        old_expiring := old_live AND OLD.expires_at IS NOT NULL;
        IF old_expiring THEN
            old_expiry_key := channel_post_expiry_key(OLD.expires_at);
        END IF;
    END IF;
    IF TG_OP <> 'DELETE' THEN
        new_channel_id := NEW.channel_id;
        new_local_id := NEW.local_id;
        new_from_id := NEW.from_id;
        new_live := NOT NEW.deleted AND NEW.action_type = 0;
        new_expiring := new_live AND NEW.expires_at IS NOT NULL;
        IF new_expiring THEN
            new_expiry_key := channel_post_expiry_key(NEW.expires_at);
        END IF;
    END IF;

    IF NOT old_live AND NOT new_live THEN
        IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE'
       AND old_live AND new_live
       AND old_expiring = new_expiring
       AND old_expiry_key IS NOT DISTINCT FROM new_expiry_key
       AND old_channel_id = new_channel_id
       AND old_local_id = new_local_id
       AND old_from_id = new_from_id
       AND OLD.action_type = NEW.action_type THEN
        RETURN NEW;
    END IF;

    FOR target_channel_id IN
        SELECT changed.channel_id
          FROM (VALUES
                    (CASE WHEN old_live THEN old_channel_id END),
                    (CASE WHEN new_live THEN new_channel_id END)
               ) AS changed(channel_id)
         WHERE changed.channel_id IS NOT NULL
         GROUP BY changed.channel_id
         ORDER BY changed.channel_id
    LOOP
        PERFORM channel_id
          FROM channel_state
         WHERE channel_id = target_channel_id
         FOR UPDATE;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'channel % has no state row for summary maintenance', target_channel_id;
        END IF;

        SELECT version
          INTO state_version
          FROM channel_post_summary_state
         WHERE channel_id = target_channel_id;
        IF FOUND AND state_version <> 1 THEN
            RAISE EXCEPTION 'channel % has unsupported post summary version %',
                target_channel_id, state_version;
        END IF;
    END LOOP;

    IF current_setting('transaction_isolation') <> 'read committed' THEN
        RAISE EXCEPTION 'channel post summary mutations require READ COMMITTED isolation (got %)',
            current_setting('transaction_isolation');
    END IF;

    WITH source_changes (channel_id, local_id, from_id, delta, expiring, expiry_key) AS (
        SELECT old_channel_id, old_local_id, old_from_id, -1::BIGINT, old_expiring, old_expiry_key WHERE old_live
        UNION ALL
        SELECT new_channel_id, new_local_id, new_from_id, 1::BIGINT, new_expiring, new_expiry_key WHERE new_live
    ), scope_changes AS (
        SELECT source.channel_id, 0::SMALLINT AS scope_kind, 0::BIGINT AS author_id,
               source.local_id AS node_key, source.delta
          FROM source_changes AS source
        UNION ALL
        SELECT source.channel_id, 1::SMALLINT AS scope_kind, source.from_id AS author_id,
               source.local_id AS node_key, source.delta
          FROM source_changes AS source
        UNION ALL
        SELECT source.channel_id, 2::SMALLINT AS scope_kind, 0::BIGINT AS author_id,
               source.local_id AS node_key, source.delta
          FROM source_changes AS source
         WHERE source.expiring
        UNION ALL
        SELECT source.channel_id, 3::SMALLINT AS scope_kind, 0::BIGINT AS author_id,
               source.expiry_key AS node_key, source.delta
          FROM source_changes AS source
         WHERE source.expiring
    ), node_changes AS (
        SELECT scope.channel_id, scope.scope_kind, scope.author_id,
               depth.depth::SMALLINT AS depth,
               (scope.node_key >> (63 - depth.depth))::BIGINT AS prefix,
               SUM(scope.delta)::BIGINT AS delta
          FROM scope_changes AS scope
          CROSS JOIN generate_series(0, 63) AS depth(depth)
         GROUP BY scope.channel_id, scope.scope_kind, scope.author_id,
                  depth.depth, (scope.node_key >> (63 - depth.depth))
        HAVING SUM(scope.delta) <> 0
    )
    -- Insert the final value for each key, not the signed delta: the table
    -- check applies before ON CONFLICT, while a negative delta can still yield
    -- a valid nonnegative existing count. A missing/underflowed node stays
    -- negative here and fails the same check. Scopes 2 and 3 are maintained only for
    -- channels whose expiring nodes have been initialized.
    INSERT INTO channel_post_summaries
        (channel_id, scope_kind, author_id, depth, prefix, live_count)
    SELECT nodes.channel_id, nodes.scope_kind, nodes.author_id,
           nodes.depth, nodes.prefix,
           COALESCE(current_summary.live_count, 0)::BIGINT + nodes.delta
      FROM node_changes AS nodes
      JOIN channel_post_summary_state AS summary_state
        ON summary_state.channel_id = nodes.channel_id
       AND summary_state.version = 1
       AND summary_state.ready = true
       AND (nodes.scope_kind < 2 OR summary_state.expiring_ready = true)
      LEFT JOIN channel_post_summaries AS current_summary
        ON current_summary.channel_id = nodes.channel_id
       AND current_summary.scope_kind = nodes.scope_kind
       AND current_summary.author_id = nodes.author_id
       AND current_summary.depth = nodes.depth
       AND current_summary.prefix = nodes.prefix
     ORDER BY nodes.channel_id, nodes.scope_kind, nodes.author_id,
              nodes.depth, nodes.prefix
    ON CONFLICT (channel_id, scope_kind, author_id, depth, prefix)
    DO UPDATE SET live_count = EXCLUDED.live_count;

    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION initialize_channel_post_summaries(p_channel_id BIGINT)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_channel_id <= 0 THEN
        RAISE EXCEPTION 'channel ID must be positive';
    END IF;

    PERFORM channel_id
      FROM channel_state
     WHERE channel_id = p_channel_id
     FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'channel % has no state row for summary initialization', p_channel_id;
    END IF;

    IF EXISTS (
        SELECT 1 FROM channel_messages
         WHERE channel_id = p_channel_id
           AND (local_id <= 0 OR from_id <= 0)
    ) THEN
        RAISE EXCEPTION 'channel % has invalid source post IDs', p_channel_id;
    END IF;

    DELETE FROM channel_post_summaries WHERE channel_id = p_channel_id;

    INSERT INTO channel_post_summaries
        (channel_id, scope_kind, author_id, depth, prefix, live_count)
    SELECT p_channel_id, contributions.scope_kind, contributions.author_id,
           contributions.depth::SMALLINT, contributions.prefix,
           COUNT(*)::BIGINT
      FROM channel_messages AS post
      CROSS JOIN generate_series(0, 63) AS depth(depth)
      CROSS JOIN LATERAL (
          VALUES (0::SMALLINT, 0::BIGINT, post.local_id, true),
                 (1::SMALLINT, post.from_id, post.local_id, true),
                 (2::SMALLINT, 0::BIGINT, post.local_id, post.expires_at IS NOT NULL),
                 (3::SMALLINT, 0::BIGINT,
                  CASE WHEN post.expires_at IS NOT NULL
                       THEN channel_post_expiry_key(post.expires_at) END,
                  post.expires_at IS NOT NULL)
      ) AS scope(scope_kind, author_id, node_key, applies)
      CROSS JOIN LATERAL (
          SELECT scope.scope_kind, scope.author_id,
                 depth.depth::SMALLINT AS depth,
                 (scope.node_key >> (63 - depth.depth))::BIGINT AS prefix
      ) AS contributions
     WHERE post.channel_id = p_channel_id
       AND post.deleted = false
       AND post.action_type = 0
       AND scope.applies
     GROUP BY contributions.scope_kind, contributions.author_id,
              contributions.depth, contributions.prefix
     ORDER BY contributions.scope_kind, contributions.author_id,
              contributions.depth, contributions.prefix;

    INSERT INTO channel_post_summary_state (channel_id, version, ready, expiring_ready)
    VALUES (p_channel_id, 1, true, true)
    ON CONFLICT (channel_id)
    DO UPDATE SET version = 1, ready = true, expiring_ready = true;
END;
$$;

-- Rebuilds only the expiring scope of one already-ready channel. Writers on
-- the channel serialize on the same channel_state lock, so the nodes and the
-- flag become visible together and a writer released afterwards maintains them.
-- Returns false when the channel needs no rebuild (flag set, or the full
-- initializer owns it because the channel is not ready).
CREATE FUNCTION initialize_channel_post_expiring_summaries(p_channel_id BIGINT)
RETURNS BOOLEAN
LANGUAGE plpgsql
AS $$
BEGIN
    IF p_channel_id <= 0 THEN
        RAISE EXCEPTION 'channel ID must be positive';
    END IF;

    PERFORM channel_id
      FROM channel_state
     WHERE channel_id = p_channel_id
     FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'channel % has no state row for summary initialization', p_channel_id;
    END IF;

    PERFORM 1
      FROM channel_post_summary_state
     WHERE channel_id = p_channel_id
       AND version = 1
       AND ready
       AND NOT expiring_ready;
    IF NOT FOUND THEN
        RETURN false;
    END IF;

    DELETE FROM channel_post_summaries
     WHERE channel_id = p_channel_id AND scope_kind IN (2, 3);

    INSERT INTO channel_post_summaries
        (channel_id, scope_kind, author_id, depth, prefix, live_count)
    SELECT p_channel_id, scope.scope_kind, 0::BIGINT, depth.depth::SMALLINT,
           (scope.node_key >> (63 - depth.depth))::BIGINT, COUNT(*)::BIGINT
      FROM channel_messages AS post
      CROSS JOIN LATERAL (
          VALUES (2::SMALLINT, post.local_id),
                 (3::SMALLINT, channel_post_expiry_key(post.expires_at))
      ) AS scope(scope_kind, node_key)
      CROSS JOIN generate_series(0, 63) AS depth(depth)
     WHERE post.channel_id = p_channel_id
       AND post.deleted = false
       AND post.action_type = 0
       AND post.expires_at IS NOT NULL
     GROUP BY scope.scope_kind, depth.depth, (scope.node_key >> (63 - depth.depth))
     ORDER BY scope.scope_kind, depth.depth, (scope.node_key >> (63 - depth.depth));

    UPDATE channel_post_summary_state
       SET expiring_ready = true
     WHERE channel_id = p_channel_id;
    RETURN true;
END;
$$;

-- Channel-wide and floor-bounded expiring counts from primary key probes:
-- expiring_live is the live expiring posts above the floor (scope 2 suffix),
-- expiring_total all live expiring posts (scope 2 root) and expiring_expired
-- those with expires_at at or before p_now (scope 3 prefix, exact to the
-- microsecond). Each is at most 63 probes. expiring_ready is false until the
-- channel's expiring nodes are initialized, so callers can tell "none" from
-- "not rebuilt yet".
CREATE FUNCTION channel_post_expiring_counts(
    p_channel_id BIGINT,
    p_marker BIGINT,
    p_now TIMESTAMPTZ
)
RETURNS TABLE (
    expiring_ready BOOLEAN,
    expiring_live BIGINT,
    expiring_total BIGINT,
    expiring_expired BIGINT
)
LANGUAGE SQL
STABLE
AS $function$
WITH summary_state AS MATERIALIZED (
    SELECT state.version = 1 AND state.ready AND state.expiring_ready AS usable
    FROM channel_post_summary_state AS state
    WHERE state.channel_id = p_channel_id
), suffix_keys AS MATERIALIZED (
    SELECT 0::SMALLINT AS depth, 0::BIGINT AS prefix
    WHERE p_marker = 0
    UNION ALL
    SELECT bits.depth::SMALLINT AS depth,
           (((p_marker >> (64 - bits.depth)) << 1) | 1)::BIGINT AS prefix
    FROM generate_series(1, 63) AS bits(depth)
    WHERE p_marker > 0
      AND ((p_marker >> (63 - bits.depth)) & 1) = 0
), expired_keys AS MATERIALIZED (
    -- Keys strictly below bound cover every expiry key <= the key of p_now.
    SELECT bits.depth::SMALLINT AS depth,
           (((bound.value >> (63 - bits.depth)) >> 1) << 1)::BIGINT AS prefix
    FROM (SELECT channel_post_expiry_key(p_now) + 1 AS value) AS bound
    CROSS JOIN generate_series(1, 63) AS bits(depth)
    WHERE ((bound.value >> (63 - bits.depth)) & 1) = 1
)
SELECT COALESCE((SELECT state.usable FROM summary_state AS state), FALSE)::BOOLEAN AS expiring_ready,
       COALESCE((
           SELECT sum((
               SELECT summary.live_count
               FROM channel_post_summaries AS summary
               WHERE summary.channel_id = p_channel_id
                 AND summary.scope_kind = 2
                 AND summary.author_id = 0
                 AND summary.depth = suffix_key.depth
                 AND summary.prefix = suffix_key.prefix
           ))::BIGINT
           FROM suffix_keys AS suffix_key
           WHERE EXISTS (SELECT 1 FROM summary_state WHERE usable)
       ), 0)::BIGINT AS expiring_live,
       COALESCE((
           SELECT summary.live_count
           FROM channel_post_summaries AS summary
           WHERE summary.channel_id = p_channel_id
             AND summary.scope_kind = 2
             AND summary.author_id = 0
             AND summary.depth = 0
             AND summary.prefix = 0
             AND EXISTS (SELECT 1 FROM summary_state WHERE usable)
       ), 0)::BIGINT AS expiring_total,
       COALESCE((
           SELECT sum((
               SELECT summary.live_count
               FROM channel_post_summaries AS summary
               WHERE summary.channel_id = p_channel_id
                 AND summary.scope_kind = 3
                 AND summary.author_id = 0
                 AND summary.depth = expired_key.depth
                 AND summary.prefix = expired_key.prefix
           ))::BIGINT
           FROM expired_keys AS expired_key
           WHERE EXISTS (SELECT 1 FROM summary_state WHERE usable)
       ), 0)::BIGINT AS expiring_expired;
$function$;
