package store_test

import (
	"context"
	"testing"
)

const channelParticipantCountsMigration = "20261025000001_channel_participant_counts.sql"
const channelParticipantCountsBackfillMigration = "20261025000002_channel_participant_counts_backfill.sql"

func TestChannelParticipantCountsMigrationBackfillsOlderSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, channelParticipantCountsMigration)

	creatorID := profilePhotoUser(t, ctx, conn)
	memberID := profilePhotoUser(t, ctx, conn)
	joinerID := profilePhotoUser(t, ctx, conn)
	const channelID = 80125001
	const emptyChannelID = 80125002
	for _, id := range []int64{channelID, emptyChannelID} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO channels (id, title, creator_id, megagroup)
			VALUES ($1, 'Migration count', $2, true)
		`, id, creatorID); err != nil {
			t.Fatalf("insert legacy channel %d: %v", id, err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO channel_state (channel_id) VALUES ($1)`, id); err != nil {
			t.Fatalf("insert legacy channel state %d: %v", id, err)
		}
	}
	for _, userID := range []int64{creatorID, memberID} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO channel_participants (channel_id, user_id, role, join_pts)
			VALUES ($1, $2, 0, 0)
		`, channelID, userID); err != nil {
			t.Fatalf("insert legacy participant %d: %v", userID, err)
		}
	}

	if _, err := conn.Exec(ctx, profilePhotoReadMigration(t, channelParticipantCountsMigration)); err != nil {
		t.Fatalf("add participant count schema: %v", err)
	}
	if _, err := conn.Exec(ctx, profilePhotoReadMigration(t, channelParticipantCountsBackfillMigration)); err != nil {
		t.Fatalf("backfill participant counts: %v", err)
	}
	assertCount := func(channelID, want int64) {
		t.Helper()
		var got int64
		if err := conn.QueryRow(ctx, `
			SELECT participant_count
			FROM channel_participant_counts
			WHERE channel_id = $1
		`, channelID).Scan(&got); err != nil {
			t.Fatalf("read participant count for channel %d: %v", channelID, err)
		}
		if got != want {
			t.Fatalf("channel %d participant count = %d, want %d", channelID, got, want)
		}
	}
	assertCount(channelID, 2)
	assertCount(emptyChannelID, 0)

	if _, err := conn.Exec(ctx, `
		INSERT INTO channel_participants (channel_id, user_id, role, join_pts)
		VALUES ($1, $2, 0, 0)
	`, channelID, joinerID); err != nil {
		t.Fatalf("insert participant after migration: %v", err)
	}
	assertCount(channelID, 3)
	if _, err := conn.Exec(ctx, `
		UPDATE channel_participants SET banned_until = now() + interval '1 hour'
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, joinerID); err != nil {
		t.Fatalf("ban participant after migration: %v", err)
	}
	assertCount(channelID, 3)
	if _, err := conn.Exec(ctx, `
		DELETE FROM channel_participants WHERE channel_id = $1 AND user_id = $2
	`, channelID, memberID); err != nil {
		t.Fatalf("delete participant after migration: %v", err)
	}
	assertCount(channelID, 2)

	var rows int64
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM channel_participants WHERE channel_id = $1
	`, channelID).Scan(&rows); err != nil {
		t.Fatalf("count migrated participant rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("migrated participant rows = %d, want 2", rows)
	}
}
