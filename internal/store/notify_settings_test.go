//nolint:testpackage // These tests seed visibility rows directly to reach the 1000-row boundary.
package store

import (
	"context"
	"errors"
	"testing"
)

func TestNotifySettingsCapAllowsUpdatesAndPrunesRemovedPeers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openDialogPinsStore(t)
	owner, err := s.CreateUser(ctx, "+15551399501")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		SELECT $1::bigint, 1, peer_id, 0 FROM generate_series(1000000, 1000999) AS peer_id
		UNION ALL SELECT $1::bigint, 1, 1001000, 0
	`, owner.ID); err != nil {
		t.Fatalf("seed visible dialogs: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `
		INSERT INTO user_notify_settings (owner_id, scope, peer_type, peer_id, show_previews, changed_at)
		SELECT $1::bigint, 1, 1, peer_id, true, now() FROM generate_series(1000000, 1000999) AS peer_id
	`, owner.ID); err != nil {
		t.Fatalf("seed peer settings to cap: %v", err)
	}

	newPeer := NotifySettingsKey{Scope: NotifySettingsScopePeer, PeerType: PeerTypeUser, PeerID: 1001000}
	falseValue := false
	if _, err = s.UpdateNotifySettings(ctx, owner.ID, newPeer, NotifySettingsValue{ShowPreviews: &falseValue}); !errors.Is(err, ErrNotifySettingsTooMany) {
		t.Fatalf("add peer at cap error = %v, want cap refusal", err)
	}

	existing := NotifySettingsKey{Scope: NotifySettingsScopePeer, PeerType: PeerTypeUser, PeerID: 1000000}
	if changed, err := s.UpdateNotifySettings(ctx, owner.ID, existing, NotifySettingsValue{ShowPreviews: &falseValue}); err != nil || !changed {
		t.Fatalf("update existing at cap: changed=%v err=%v", changed, err)
	}
	if changed, err := s.UpdateNotifySettings(ctx, owner.ID, existing, NotifySettingsValue{}); err != nil || !changed {
		t.Fatalf("clear existing at cap: changed=%v err=%v", changed, err)
	}
	cleared, found, err := s.GetNotifySettings(ctx, owner.ID, existing)
	if err != nil || !found || !emptyNotifySettings(cleared) {
		t.Fatalf("read clear tombstone: value=%+v found=%v err=%v", cleared, found, err)
	}
	if changed, err := s.UpdateNotifySettings(ctx, owner.ID, existing, NotifySettingsValue{ShowPreviews: &falseValue}); err != nil || !changed {
		t.Fatalf("restore peer at cap: changed=%v err=%v", changed, err)
	}

	if _, err = s.pool.Exec(ctx, "DELETE FROM dialogs WHERE owner_id = $1 AND peer_type = 1 AND peer_id = 1000001", owner.ID); err != nil {
		t.Fatalf("remove one peer dialog: %v", err)
	}
	if changed, err := s.UpdateNotifySettings(ctx, owner.ID, newPeer, NotifySettingsValue{ShowPreviews: &falseValue}); err != nil || !changed {
		t.Fatalf("add peer after pruning removed dialog: changed=%v err=%v", changed, err)
	}
	var staleCount, activeCount int64
	if err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM user_notify_settings
		WHERE owner_id = $1 AND scope = 1 AND peer_type = 1 AND peer_id = 1000001
	`, owner.ID).Scan(&staleCount); err != nil {
		t.Fatalf("count pruned setting: %v", err)
	}
	if err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM user_notify_settings
		WHERE owner_id = $1 AND scope = 1
		  AND (show_previews IS NOT NULL OR silent IS NOT NULL OR mute_until IS NOT NULL OR sound IS NOT NULL)
	`, owner.ID).Scan(&activeCount); err != nil {
		t.Fatalf("count active settings: %v", err)
	}
	if staleCount != 0 || activeCount != maxPeerNotifySettings {
		t.Fatalf("after prune: stale rows=%d active rows=%d, want 0 and %d", staleCount, activeCount, maxPeerNotifySettings)
	}
}
