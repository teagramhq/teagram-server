-- atlas:txmode none

CREATE INDEX CONCURRENTLY admin_sticker_set_events_set_id_recent_idx
    ON admin_sticker_set_events (set_id, id DESC);
