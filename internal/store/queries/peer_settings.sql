-- name: PeerSettingsBarDismissed :one
SELECT EXISTS (
    SELECT 1 FROM peer_settings_bar_dismissals
    WHERE owner_id = $1 AND peer_id = $2
);

-- name: DismissPeerSettingsBar :execrows
INSERT INTO peer_settings_bar_dismissals (owner_id, peer_id)
VALUES ($1, $2)
ON CONFLICT (owner_id, peer_id) DO NOTHING;
