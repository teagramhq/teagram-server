package api

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type createChatTestKeys struct {
	key    crypto.AuthKey
	userID int64
}

func (s createChatTestKeys) Save(context.Context, crypto.AuthKey) error { return nil }
func (s createChatTestKeys) Touch(context.Context, [8]byte) error       { return nil }
func (s createChatTestKeys) Get(_ context.Context, id [8]byte, _ time.Duration) (crypto.AuthKey, int64, bool, mtproto.PendingLogin, bool, error) {
	return s.key, s.userID, false, mtproto.PendingLogin{}, id == s.key.ID, nil
}

type createChatRPCContexts struct {
	request    context.Context
	completion context.Context
}

type createChatTransportServer struct {
	server      *mtproto.Server
	listener    net.Listener
	client      transport.Conn
	cancel      context.CancelFunc
	key         crypto.AuthKey
	userID      int64
	memberID    int64
	contexts    chan createChatRPCContexts
	handlerDone chan error
	done        chan error
}

func startCreateChatTransportServer(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	userID, memberID int64,
	rpcDeadline time.Duration,
	drainTimeout time.Duration,
) *createChatTransportServer {
	t.Helper()
	var rawKey crypto.Key
	for i := range rawKey {
		rawKey[i] = byte(i + 1)
	}
	key := rawKey.WithID()
	contexts := make(chan createChatRPCContexts, 1)
	handlerDone := make(chan error, 1)
	inner := testHandlers(s)
	handler := mtproto.HandlerFunc(func(c *mtproto.Conn, req *mtproto.Request) error {
		contexts <- createChatRPCContexts{request: req.Ctx, completion: req.CompletionCtx}
		result, err := inner.handleCreateChat(req)
		handlerDone <- err
		if err != nil {
			return err
		}
		return c.SendResult(req, result)
	})
	server := mtproto.New(exchange.PrivateKey{}, 2, createChatTestKeys{key: key, userID: userID}, handler, nil)
	if rpcDeadline > 0 {
		if err := server.SetRPCDeadline(rpcDeadline); err != nil {
			t.Fatalf("set create-chat RPC deadline: %v", err)
		}
	}
	if drainTimeout > 0 {
		server.SetShutdownTimeoutsForTest(drainTimeout, drainTimeout/3)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for create-chat RPC: %v", err)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(serverCtx, listener) }()

	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		cancel()
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close create-chat listener after dial failure: %v", closeErr)
		}
		t.Fatalf("dial create-chat RPC server: %v", err)
	}
	client, err := transport.Abridged.Handshake(rawConn)
	if err != nil {
		cancel()
		if closeErr := rawConn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close create-chat raw connection after handshake failure: %v", closeErr)
		}
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			t.Errorf("close create-chat listener after handshake failure: %v", closeErr)
		}
		t.Fatalf("handshake create-chat RPC client: %v", err)
	}
	peer := &createChatTransportServer{
		server: server, listener: listener, client: client, cancel: cancel, key: key,
		userID: userID, memberID: memberID, contexts: contexts, handlerDone: handlerDone, done: done,
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close create-chat test client: %v", err)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop create-chat test server: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("create-chat test server did not stop")
		}
	})
	return peer
}

func (s *createChatTransportServer) sendCreateChatRequest(t *testing.T, ctx context.Context) {
	t.Helper()
	if err := s.client.Send(ctx, &bin.Buffer{Buf: createChatClientFrame(t, s.key, int64(proto.NewMessageID(time.Now(), proto.MessageFromClient))&^3, &tg.MessagesCreateChatRequest{
		Users: []tg.InputUserClass{InputUser(s.userID, s.memberID)}, Title: "Completion boundary",
	})}); err != nil {
		t.Fatalf("send create-chat RPC: %v", err)
	}
}

func waitCreateChatHandler(t *testing.T, peer *createChatTransportServer) error {
	t.Helper()
	select {
	case err := <-peer.handlerDone:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("create-chat handler did not finish")
		return nil
	}
}

func createChatClientFrame(t *testing.T, key crypto.AuthKey, msgID int64, request bin.Encoder) []byte {
	t.Helper()
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode create-chat request: %v", err)
	}
	data := crypto.EncryptedMessageData{
		SessionID: 42, MessageID: msgID, MessageDataLen: int32(body.Len()), //nolint:gosec // Test request bodies are bounded well below MaxInt32.
		MessageDataWithPadding: body.Copy(),
	}
	var frame bin.Buffer
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, data, &frame); err != nil {
		t.Fatalf("encrypt create-chat request: %v", err)
	}
	return frame.Copy()
}

func openCreateChatDB(t *testing.T) (context.Context, *store.Store, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open create-chat store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close create-chat store: %v", err)
		}
	})
	return ctx, s, dsn
}

func createChatLockBlocker(t *testing.T, ctx context.Context, dsn string, creatorID int64) (*pgx.Conn, pgx.Tx) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect create-chat lock blocker: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close create-chat lock blocker: %v", err)
		}
	})
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin create-chat lock blocker: %v", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("release create-chat lock blocker: %v", err)
			}
		}
	})
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, creatorID); err != nil {
		t.Fatalf("hold create-chat owner lock: %v", err)
	}
	return conn, tx
}

func waitCreateChatAnnouncementBlocked(t *testing.T, ctx context.Context, tx pgx.Tx, creatorID int64) bool {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var committed, blocked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM chats WHERE creator_id = $1)`, creatorID).Scan(&committed); err != nil {
			t.Fatalf("check committed create-chat row: %v", err)
		}
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity AS waiting
				CROSS JOIN LATERAL unnest(pg_blocking_pids(waiting.pid)) AS blockers(pid)
				WHERE blockers.pid = pg_backend_pid()
				  AND waiting.wait_event_type = 'Lock'
				  AND waiting.wait_event = 'advisory'
			)
		`).Scan(&blocked); err != nil {
			t.Fatalf("check create-chat announcement wait: %v", err)
		}
		if committed && blocked {
			return true
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func waitCreateChatAnnouncementCleared(t *testing.T, ctx context.Context, tx pgx.Tx) bool {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity AS waiting
				CROSS JOIN LATERAL unnest(pg_blocking_pids(waiting.pid)) AS blockers(pid)
				WHERE blockers.pid = pg_backend_pid()
				  AND waiting.wait_event_type = 'Lock'
				  AND waiting.wait_event = 'advisory'
			)
		`).Scan(&blocked); err != nil {
			t.Fatalf("check create-chat announcement wait after cancellation: %v", err)
		}
		if !blocked {
			return true
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func assertCreateChatCommitted(t *testing.T, ctx context.Context, s *store.Store, creatorID, memberID int64) {
	t.Helper()
	chats, err := s.ChatsForUser(ctx, creatorID)
	if err != nil || len(chats) != 1 {
		t.Fatalf("creator chats = %d, err=%v; want the committed first phase", len(chats), err)
	}
	for _, userID := range []int64{creatorID, memberID} {
		history, err := s.History(ctx, userID, store.PeerTypeChat, chats[0].ID, 0, 10)
		if err != nil || len(history) != 1 || history[0].Action != store.ChatActionCreate {
			t.Fatalf("user %d chat history = %v, err=%v; want the create announcement", userID, history, err)
		}
	}
}

func assertCreateChatAnnouncementAbsent(t *testing.T, ctx context.Context, s *store.Store, creatorID, memberID int64) {
	t.Helper()
	chats, err := s.ChatsForUser(ctx, creatorID)
	if err != nil || len(chats) != 1 {
		t.Fatalf("creator chats = %d, err=%v; want the committed first phase", len(chats), err)
	}
	for _, userID := range []int64{creatorID, memberID} {
		history, err := s.History(ctx, userID, store.PeerTypeChat, chats[0].ID, 0, 10)
		if err != nil || len(history) != 0 {
			t.Fatalf("user %d chat history = %v, err=%v; announcement should not commit after cancellation", userID, history, err)
		}
	}
}

func TestCreateChatTransportCancellationKeepsCommittedAnnouncementBounded(t *testing.T) {
	t.Parallel()
	ctx, s, dsn := openCreateChatDB(t)
	creator, err := s.CreateUser(ctx, "+15551292131")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551292132")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	peer := startCreateChatTransportServer(t, ctx, s, creator.ID, member.ID, 15*time.Second, 0)
	_, blocker := createChatLockBlocker(t, ctx, dsn, creator.ID)
	peer.sendCreateChatRequest(t, ctx)
	if !waitCreateChatAnnouncementBlocked(t, ctx, blocker, creator.ID) {
		t.Fatal("create-chat announcement did not block after the first commit")
	}
	contexts := <-peer.contexts
	if _, ok := contexts.completion.Deadline(); !ok {
		t.Fatal("admitted completion context lost the original RPC deadline")
	}
	if err := peer.client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close transport after first chat commit: %v", err)
	}
	select {
	case <-contexts.request.Done():
		if !errors.Is(contexts.request.Err(), context.Canceled) {
			t.Fatalf("request context after peer close = %v, want cancellation", contexts.request.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("transport close did not cancel the admitted request")
	}
	if err := contexts.completion.Err(); err != nil {
		t.Fatalf("peer cancellation reached the bounded completion context: %v", err)
	}
	if !waitCreateChatAnnouncementBlocked(t, ctx, blocker, creator.ID) {
		t.Fatal("peer cancellation canceled the committed announcement work")
	}
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release create-chat owner lock: %v", err)
	}
	if err := waitCreateChatHandler(t, peer); err != nil {
		t.Fatalf("create-chat handler after peer cancellation: %v", err)
	}
	assertCreateChatCommitted(t, ctx, s, creator.ID, member.ID)
}

func TestCreateChatCompletionInheritsOriginalRPCDeadline(t *testing.T) {
	t.Parallel()
	ctx, s, dsn := openCreateChatDB(t)
	creator, err := s.CreateUser(ctx, "+15551292141")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551292142")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	peer := startCreateChatTransportServer(t, ctx, s, creator.ID, member.ID, 2*time.Second, 0)
	_, blocker := createChatLockBlocker(t, ctx, dsn, creator.ID)
	started := time.Now()
	peer.sendCreateChatRequest(t, ctx)
	if !waitCreateChatAnnouncementBlocked(t, ctx, blocker, creator.ID) {
		t.Fatal("create-chat announcement did not block after the first commit")
	}
	contexts := <-peer.contexts
	deadline, ok := contexts.completion.Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		t.Fatalf("completion deadline = %v (present %t), want the original two-second admission deadline", deadline, ok)
	}
	select {
	case <-contexts.completion.Done():
		if !errors.Is(contexts.completion.Err(), context.DeadlineExceeded) {
			t.Fatalf("completion context error = %v, want original RPC deadline", contexts.completion.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("committed announcement outlived the original RPC deadline")
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("create-chat deadline took %s to cancel announcement work", elapsed)
	}
	if !waitCreateChatAnnouncementCleared(t, ctx, blocker) {
		t.Fatal("announcement DB waiter survived the original RPC deadline")
	}
	if err := waitCreateChatHandler(t, peer); !errors.Is(err, errInternal) {
		t.Fatalf("create-chat handler after RPC deadline = %v, want internal error", err)
	}
	assertCreateChatAnnouncementAbsent(t, ctx, s, creator.ID, member.ID)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release create-chat owner lock: %v", err)
	}
}

func TestCreateChatCompletionSurvivesDrainStartAndStopsAtHardCutoff(t *testing.T) {
	t.Parallel()
	ctx, s, dsn := openCreateChatDB(t)
	creator, err := s.CreateUser(ctx, "+15551292151")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551292152")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	peer := startCreateChatTransportServer(t, ctx, s, creator.ID, member.ID, 5*time.Second, 1200*time.Millisecond)
	_, blocker := createChatLockBlocker(t, ctx, dsn, creator.ID)
	peer.sendCreateChatRequest(t, ctx)
	if !waitCreateChatAnnouncementBlocked(t, ctx, blocker, creator.ID) {
		t.Fatal("create-chat announcement did not block after the first commit")
	}
	contexts := <-peer.contexts
	if err := peer.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("start graceful server drain: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := contexts.completion.Err(); err != nil {
		t.Fatalf("drain start canceled admitted create-chat completion: %v", err)
	}
	if !waitCreateChatAnnouncementBlocked(t, ctx, blocker, creator.ID) {
		t.Fatal("drain start released announcement DB work before its hard cutoff")
	}
	select {
	case <-contexts.completion.Done():
		if !errors.Is(contexts.completion.Err(), context.Canceled) {
			t.Fatalf("completion context at hard cutoff = %v, want cancellation", contexts.completion.Err())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("hard cutoff did not cancel the committed announcement")
	}
	if !waitCreateChatAnnouncementCleared(t, ctx, blocker) {
		t.Fatal("announcement DB waiter survived the hard cutoff")
	}
	if err := waitCreateChatHandler(t, peer); !errors.Is(err, errInternal) {
		t.Fatalf("create-chat handler after hard cutoff = %v, want internal error", err)
	}
	assertCreateChatAnnouncementAbsent(t, ctx, s, creator.ID, member.ID)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatalf("release create-chat owner lock: %v", err)
	}
}
