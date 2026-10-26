-- Support bounded messages.getHistory reads using offset_date.
-- atlas:txmode none

CREATE INDEX CONCURRENTLY messages_owner_peer_date_idx
    ON messages (owner_id, peer_type, peer_id, date DESC, local_id DESC);
