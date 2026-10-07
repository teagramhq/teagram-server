package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type channelForwardFixture struct {
	store       *store.Store
	dsn         string
	creator     store.User
	forwarder   store.User
	destination store.User
	channel     store.Channel
	post        store.ChannelMessage
}

func newChannelForwardFixture(t *testing.T, applicationName string) channelForwardFixture {
	t.Helper()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStoreWithApplicationName(t, dsn, applicationName)
	creator, err := s.CreateUser(ctx, "+15559410001")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	forwarder, err := s.CreateUser(ctx, "+15559410002")
	if err != nil {
		t.Fatalf("create forwarder: %v", err)
	}
	destination, err := s.CreateUser(ctx, "+15559410003")
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "forward source", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create channel invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, forwarder.ID); err != nil {
		t.Fatalf("join channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "channel source text", 41001, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("create source post: duplicate=%v err=%v", duplicate, err)
	}
	return channelForwardFixture{
		store: s, dsn: dsn, creator: creator, forwarder: forwarder, destination: destination,
		channel: channel, post: post,
	}
}

func (f channelForwardFixture) request() *tg.MessagesForwardMessagesRequest {
	return &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChannel(f.forwarder.ID, f.channel.ID),
		ID:       []int{int(f.post.LocalID)},
		RandomID: []int64{41002},
		ToPeer:   api.InputPeerUser(f.forwarder.ID, f.destination.ID),
	}
}

func TestForwardFromChannelRechecksBanAfterHandlerPrecheck(t *testing.T) {
	t.Parallel()
	runForwardFromChannelRechecksMembership(t, "channel_forward_ban_wait",
		`UPDATE channel_participants SET banned_until = now() + interval '1 hour' WHERE channel_id = $1 AND user_id = $2`)
}

func TestForwardFromChannelRechecksLeaveAfterHandlerPrecheck(t *testing.T) {
	t.Parallel()
	runForwardFromChannelRechecksMembership(t, "channel_forward_leave_wait",
		`DELETE FROM channel_participants WHERE channel_id = $1 AND user_id = $2`)
}

func runForwardFromChannelRechecksMembership(t *testing.T, applicationName, mutation string) {
	t.Helper()
	ctx := context.Background()
	fixture := newChannelForwardFixture(t, applicationName)
	blocker, blockerPID := beginForwardRaceTransaction(t, ctx, fixture.dsn, mutation, fixture.channel.ID, fixture.forwarder.ID)

	forwardDone := make(chan error, 1)
	go func() {
		_, err := api.ForwardMessagesForTest(fixture.store, fixture.forwarder.ID, fixture.request())
		forwardDone <- err
	}()

	observer, err := pgx.Connect(ctx, fixture.dsn)
	if err != nil {
		t.Fatalf("connect forward lock observer: %v", err)
	}
	defer func() {
		if err := observer.Close(ctx); err != nil {
			t.Errorf("close forward lock observer: %v", err)
		}
	}()
	waitForChannelLockWaiter(t, ctx, observer, applicationName, blockerPID, blockerPID)
	select {
	case err := <-forwardDone:
		t.Fatalf("forward completed before ban commit: %v", err)
	default:
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatalf("commit source ban: %v", err)
	}
	forwardErr := <-forwardDone
	rpcError(t, forwardErr, "PEER_ID_INVALID")
	assertNoChannelForwardRows(t, ctx, fixture)
}

func TestForwardFromChannelSkipsInFlightTombstone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newChannelForwardFixture(t, "channel_forward_tombstone_skip")
	blocker, _ := beginForwardRaceTransaction(t, ctx, fixture.dsn,
		`UPDATE channel_messages SET deleted = true WHERE channel_id = $1 AND local_id = $2`,
		fixture.channel.ID, fixture.post.LocalID)

	forwardDone := make(chan error, 1)
	go func() {
		_, err := api.ForwardMessagesForTest(fixture.store, fixture.forwarder.ID, fixture.request())
		forwardDone <- err
	}()
	select {
	case err := <-forwardDone:
		rpcError(t, err, "MESSAGE_ID_INVALID")
	case <-time.After(2 * time.Second):
		if rollbackErr := blocker.Rollback(ctx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			t.Fatalf("release in-flight tombstone: %v", rollbackErr)
		}
		forwardErr := <-forwardDone
		t.Fatalf("forward waited on in-flight tombstone and then returned %v", forwardErr)
	}
	if err := blocker.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatalf("roll back synthetic tombstone: %v", err)
	}
	assertNoChannelForwardRows(t, ctx, fixture)
}

func TestForwardFromChannelKeepsBasicChatSubtypeRestrictions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newChannelForwardFixture(t, "channel_forward_subtype")
	file, err := fixture.store.AllocateAndCompleteFile(
		ctx, fixture.creator.ID, 7, "video/mp4", "source.mp4", api.TestMaxUserStorageBytes,
		[]string{"send_videos"}, func(store.File) error { return nil },
	)
	if err != nil {
		t.Fatalf("allocate stored source file: %v", err)
	}
	fileID := file.ID
	post, _, duplicate, err := fixture.store.PostChannelMessage(ctx, fixture.channel.ID, fixture.creator.ID, "video source", 41003, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel video: duplicate=%v err=%v", duplicate, err)
	}
	chat, err := fixture.store.CreateChat(ctx, fixture.creator.ID, "restricted destination", []int64{fixture.forwarder.ID, fixture.destination.ID})
	if err != nil {
		t.Fatalf("create destination chat: %v", err)
	}
	conn, err := pgx.Connect(ctx, fixture.dsn)
	if err != nil {
		t.Fatalf("connect destination rights: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close destination rights connection: %v", err)
		}
	}()
	setChatDefaultRights(t, conn, chat.ID, "send_videos")
	before := basicChatWriteStats(t, conn, chat.ID)
	_, err = api.ForwardMessagesForTest(fixture.store, fixture.forwarder.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChannel(fixture.forwarder.ID, fixture.channel.ID),
		ID:       []int{int(post.LocalID)},
		RandomID: []int64{41004},
		ToPeer:   api.InputPeerChat(fixture.forwarder.ID, chat.ID),
	})
	rpcError(t, err, "CHAT_WRITE_FORBIDDEN")
	assertChatWriteStats(t, conn, chat.ID, before)
}

func TestForwardFromChannelRejectsWholeBatchWhenFileRowIsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newChannelForwardFixture(t, "channel_forward_missing_file")
	file, err := fixture.store.AllocateAndCompleteFile(
		ctx, fixture.creator.ID, 7, "application/octet-stream", "missing.bin", api.TestMaxUserStorageBytes,
		nil, func(store.File) error { return nil },
	)
	if err != nil {
		t.Fatalf("allocate stored source file: %v", err)
	}
	fileID := file.ID
	mediaPost, _, duplicate, err := fixture.store.PostChannelMessage(ctx, fixture.channel.ID, fixture.creator.ID, "media source", 41005, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel media: duplicate=%v err=%v", duplicate, err)
	}
	deleteForwardSourceFileRow(t, ctx, fixture.dsn, file.ID)
	_, err = api.ForwardMessagesForTest(fixture.store, fixture.forwarder.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChannel(fixture.forwarder.ID, fixture.channel.ID),
		ID:       []int{int(fixture.post.LocalID), int(mediaPost.LocalID)},
		RandomID: []int64{41006, 41007},
		ToPeer:   api.InputPeerUser(fixture.forwarder.ID, fixture.destination.ID),
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	assertNoChannelForwardRows(t, ctx, fixture)
}

func beginForwardRaceTransaction(t *testing.T, ctx context.Context, dsn, mutation string, args ...any) (pgx.Tx, int) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect forward race transaction: %v", err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin forward race transaction: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release forward race transaction: %v", err)
		}
		if err := conn.Close(cleanupCtx); err != nil {
			t.Errorf("close forward race transaction: %v", err)
		}
	})
	if _, err := tx.Exec(ctx, mutation, args...); err != nil {
		t.Fatalf("hold forward race mutation: %v", err)
	}
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read forward race backend pid: %v", err)
	}
	return tx, pid
}

func deleteForwardSourceFileRow(t *testing.T, ctx context.Context, dsn string, fileID int64) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect missing-file fixture: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close missing-file fixture connection: %v", err)
		}
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin missing-file fixture transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatalf("disable foreign-key triggers for missing-file fixture: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM files WHERE id = $1`, fileID); err != nil {
		t.Fatalf("delete source file row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit missing-file fixture: %v", err)
	}
}

func assertNoChannelForwardRows(t *testing.T, ctx context.Context, fixture channelForwardFixture) {
	t.Helper()
	for _, tc := range []struct {
		ownerID int64
		peerID  int64
	}{
		{ownerID: fixture.forwarder.ID, peerID: fixture.destination.ID},
		{ownerID: fixture.destination.ID, peerID: fixture.forwarder.ID},
	} {
		rows, err := fixture.store.History(ctx, tc.ownerID, store.PeerTypeUser, tc.peerID, 0, 10)
		if err != nil {
			t.Fatalf("read forward history for %d: %v", tc.ownerID, err)
		}
		if len(rows) != 0 {
			t.Fatalf("owner %d has %d destination rows after refused forward, want 0", tc.ownerID, len(rows))
		}
	}
}
