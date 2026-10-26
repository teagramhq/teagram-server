package store_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func assertChannelParticipantCountMatchesRows(t *testing.T, ctx context.Context, conn *pgx.Conn, channelID int64) int64 {
	t.Helper()
	var stored, rows int64
	if err := conn.QueryRow(ctx, `
		SELECT participant_count FROM channel_participant_counts WHERE channel_id = $1
	`, channelID).Scan(&stored); err != nil {
		t.Fatalf("read stored participant count: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM channel_participants WHERE channel_id = $1
	`, channelID).Scan(&rows); err != nil {
		t.Fatalf("count participant rows: %v", err)
	}
	if stored != rows {
		t.Fatalf("stored participant count = %d, participant rows = %d", stored, rows)
	}
	return stored
}

func TestChannelParticipantCountTracksMutationsAndConcurrentJoinsAndLeaves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner := mustUser(t, s, "+15551298801")
	channel := mustChannel(t, s, owner.ID, "Participant count")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test connection: %v", err)
		}
	}()

	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 1 {
		t.Fatalf("count after channel creation = %d, want 1", got)
	}

	joiner := mustUser(t, s, "+15551298802")
	invite, err := s.CreateChannelInvite(ctx, channel.ID, owner.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err := s.JoinChannelByInvite(ctx, invite, joiner.ID); err != nil {
		t.Fatalf("join by invite: %v", err)
	}
	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 2 {
		t.Fatalf("count after join = %d, want 2", got)
	}

	banned := mustUser(t, s, "+15551298803")
	kicked := mustUser(t, s, "+15551298804")
	added, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{banned.ID, kicked.ID})
	if err != nil || len(added) != 2 {
		t.Fatalf("add ban and kick targets = %v, %v; want both admitted", added, err)
	}
	until := time.Now().Add(time.Hour)
	if err := store.SetChannelBan(ctx, s, channel.ID, banned.ID, &until); err != nil {
		t.Fatalf("ban participant: %v", err)
	}
	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 4 {
		t.Fatalf("count after ban = %d, want 4", got)
	}

	if _, err := conn.Exec(ctx, `DELETE FROM channel_participants WHERE channel_id = $1 AND user_id = $2`, channel.ID, kicked.ID); err != nil {
		t.Fatalf("kick participant: %v", err)
	}
	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 3 {
		t.Fatalf("count after kick = %d, want 3", got)
	}
	if left, err := s.LeaveChannel(ctx, channel.ID, joiner.ID); err != nil || !left {
		t.Fatalf("leave channel = %v, %v; want true/nil", left, err)
	}
	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 2 {
		t.Fatalf("count after leave = %d, want 2", got)
	}

	const concurrent = 8
	joiners := make([]store.User, concurrent)
	leavers := make([]store.User, concurrent)
	leaverIDs := make([]int64, concurrent)
	for i := range concurrent {
		joiners[i] = mustUser(t, s, fmt.Sprintf("+155512989%02d", i*2))
		leavers[i] = mustUser(t, s, fmt.Sprintf("+155512989%02d", i*2+1))
		leaverIDs[i] = leavers[i].ID
	}
	added, err = s.AddChannelMembers(ctx, channel.ID, owner.ID, leaverIDs)
	if err != nil || len(added) != concurrent {
		t.Fatalf("seed concurrent leavers = %v, %v; want %d", added, err, concurrent)
	}

	start := make(chan struct{})
	errs := make([]error, concurrent*2)
	var wg sync.WaitGroup
	for i := range concurrent {
		joinerID, leaverID := joiners[i].ID, leavers[i].ID
		wg.Go(func() {
			<-start
			_, err := s.AddChannelMembers(ctx, channel.ID, owner.ID, []int64{joinerID})
			errs[i] = err
		})
		wg.Go(func() {
			<-start
			_, err := s.LeaveChannel(ctx, channel.ID, leaverID)
			errs[concurrent+i] = err
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent membership operation %d: %v", i, err)
		}
	}
	if got := assertChannelParticipantCountMatchesRows(t, ctx, conn, channel.ID); got != 10 {
		t.Fatalf("count after concurrent joins and leaves = %d, want 10", got)
	}
}
