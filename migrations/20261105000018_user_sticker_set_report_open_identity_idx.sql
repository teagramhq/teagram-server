-- Deduplicate identical open reports while allowing distinct or renewed reports.
-- atlas:txmode none

CREATE UNIQUE INDEX CONCURRENTLY user_sticker_set_reports_open_identity_idx
    ON user_sticker_set_reports (
        reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment
    )
    WHERE status = 'open';
