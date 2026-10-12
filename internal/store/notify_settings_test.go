//nolint:testpackage // These tests seed visibility rows directly for cap and lock coverage.
package store

import (
	"context"
	"errors"
	"testing"
	"time"
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

func TestGetNotifySettingsDoesNotWaitOnPeerWriteLocks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openDialogPinsStore(t)
	owner, err := s.CreateUser(ctx, "+15551399502")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	userPeer, err := s.CreateUser(ctx, "+15551399503")
	if err != nil {
		t.Fatalf("create user peer: %v", err)
	}
	if _, err = s.pool.Exec(ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		VALUES ($1, 1, $2, 0)
	`, owner.ID, userPeer.ID); err != nil {
		t.Fatalf("create visible user dialog: %v", err)
	}
	chat, err := s.CreateChat(ctx, owner.ID, "Notify settings lock test", nil)
	if err != nil {
		t.Fatalf("create chat peer: %v", err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "Notify settings lock test", "", true)
	if err != nil {
		t.Fatalf("create channel peer: %v", err)
	}

	tests := []struct {
		name      string
		key       NotifySettingsKey
		lockQuery string
		lockID    int64
	}{
		{
			name:      "peer user advisory lock",
			key:       NotifySettingsKey{Scope: NotifySettingsScopePeer, PeerType: PeerTypeUser, PeerID: userPeer.ID},
			lockQuery: "SELECT pg_advisory_xact_lock($1)",
			lockID:    userPeer.ID,
		},
		{
			name:      "chat row lock",
			key:       NotifySettingsKey{Scope: NotifySettingsScopePeer, PeerType: PeerTypeChat, PeerID: chat.ID},
			lockQuery: "SELECT id FROM chats WHERE id = $1 FOR UPDATE",
			lockID:    chat.ID,
		},
		{
			name:      "channel row lock",
			key:       NotifySettingsKey{Scope: NotifySettingsScopePeer, PeerType: PeerTypeChannel, PeerID: channel.ID},
			lockQuery: "SELECT id FROM channels WHERE id = $1 FOR NO KEY UPDATE",
			lockID:    channel.ID,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			muteUntil := int32(73)
			if changed, err := s.UpdateNotifySettings(ctx, owner.ID, test.key, NotifySettingsValue{MuteUntil: &muteUntil}); err != nil || !changed {
				t.Fatalf("seed notification settings: changed=%v err=%v", changed, err)
			}

			blocker, err := s.pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin lock holder: %v", err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }() //nolint:errcheck // release lock on failure
			if _, err := blocker.Exec(ctx, test.lockQuery, test.lockID); err != nil {
				t.Fatalf("hold peer write lock: %v", err)
			}

			readCtx, cancelRead := context.WithTimeout(ctx, 3*time.Second)
			value, found, err := s.GetNotifySettings(readCtx, owner.ID, test.key)
			cancelRead()
			if err != nil || !found {
				t.Fatalf("read notification settings while peer write lock is held: found=%v err=%v", found, err)
			}
			if value.MuteUntil == nil || *value.MuteUntil != muteUntil {
				t.Fatalf("read mute_until = %v, want %d", value.MuteUntil, muteUntil)
			}
		})
	}
}
