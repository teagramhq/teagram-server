package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const ownerWireMax int64 = 1<<31 - 1

func TestOwnerWidthRefusalDoesNotNotifyConnectedSender(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(t *testing.T, h *handlers, s *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, dest store.User) error
	}{
		{name: "send", run: ownerWidthRefusedSend},
		{name: "media", run: ownerWidthRefusedMedia},
		{name: "private poll", run: ownerWidthRefusedPrivatePoll},
		{name: "saved poll", run: ownerWidthRefusedSavedPoll},
		{name: "edit", run: ownerWidthRefusedEdit},
		{name: "single forward", run: ownerWidthRefusedForward},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dsn := pgtest.DSN(t)
			blobs, err := blob.NewLocal(t.TempDir())
			if err != nil {
				t.Fatalf("blob store: %v", err)
			}
			s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() }) //nolint:errcheck // teardown

			h := testHandlers(s)
			h.blobs = blobs
			h.maxUserStorageBytes = 1 << 30
			owner, err := s.CreateUser(ctx, "+15558100101")
			if err != nil {
				t.Fatalf("create owner: %v", err)
			}
			peer, err := s.CreateUser(ctx, "+15558100102")
			if err != nil {
				t.Fatalf("create peer: %v", err)
			}
			dest, err := s.CreateUser(ctx, "+15558100103")
			if err != nil {
				t.Fatalf("create destination: %v", err)
			}
			stateDB, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect state database: %v", err)
			}
			t.Cleanup(func() { _ = stateDB.Close(context.Background()) }) //nolint:errcheck // teardown

			notifications := listenForOwnerWidthUpdates(t, ctx, dsn)
			if err := tc.run(t, h, s, ctx, stateDB, owner, peer, dest); err == nil {
				t.Fatal("request succeeded despite owner state exhaustion")
			}
			assertNoOwnerWidthUpdate(t, notifications)
		})
	}
}

func ownerWidthRefusedSend(t *testing.T, h *handlers, _ *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, _ store.User) error {
	t.Helper()
	setOwnerWidthState(t, ctx, stateDB, peer.ID, ownerWireMax, 81)
	return runConnectedOwnerWidthRequest(t, h, owner.ID, &tg.MessagesSendMessageRequest{
		Peer: apiInputPeerUser(owner.ID, peer.ID), Message: "refused", RandomID: 8001,
	}, h.handleSendMessageAfterReplyOnConn)
}

func ownerWidthRefusedMedia(t *testing.T, h *handlers, _ *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, _ store.User) error {
	t.Helper()
	setOwnerWidthState(t, ctx, stateDB, peer.ID, ownerWireMax, 82)
	const fileID = 8002
	var upload bin.Buffer
	if err := (&tg.UploadSaveFilePartRequest{FileID: fileID, FilePart: 0, Bytes: []byte("media")}).Encode(&upload); err != nil {
		t.Fatalf("encode upload part: %v", err)
	}
	if _, err := h.handleSaveFilePart(&mtproto.Request{Ctx: ctx, UserID: owner.ID, Buf: &upload}); err != nil {
		t.Fatalf("save upload part: %v", err)
	}
	return runConnectedOwnerWidthRequest(t, h, owner.ID, &tg.MessagesSendMediaRequest{
		Peer: apiInputPeerUser(owner.ID, peer.ID), Message: "refused media", RandomID: 8002,
		Media: &tg.InputMediaUploadedDocument{
			File: &tg.InputFile{ID: fileID, Parts: 1, Name: "refused.txt"}, MimeType: "text/plain",
		},
	}, h.handleSendMediaAfterReplyOnConn)
}

func ownerWidthRefusedPrivatePoll(t *testing.T, h *handlers, _ *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, _ store.User) error {
	t.Helper()
	setOwnerWidthState(t, ctx, stateDB, peer.ID, ownerWireMax, 83)
	return runConnectedOwnerWidthRequest(t, h, owner.ID, ownerWidthPollRequest(apiInputPeerUser(owner.ID, peer.ID), 8006), h.handleSendMediaAfterReplyOnConn)
}

func ownerWidthRefusedSavedPoll(t *testing.T, h *handlers, _ *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, _, _ store.User) error {
	t.Helper()
	setOwnerWidthState(t, ctx, stateDB, owner.ID, 12, ownerWireMax+1)
	return runConnectedOwnerWidthRequest(t, h, owner.ID, ownerWidthPollRequest(&tg.InputPeerSelf{}, 8007), h.handleSendMediaAfterReplyOnConn)
}

func ownerWidthPollRequest(peer tg.InputPeerClass, randomID int64) *tg.MessagesSendMediaRequest {
	return &tg.MessagesSendMediaRequest{
		Peer: peer, Message: "poll description", RandomID: randomID,
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Choose one"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}},
	}
}

func ownerWidthRefusedEdit(t *testing.T, h *handlers, s *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, _ store.User) error {
	t.Helper()
	message, senderPts, recipientPts, duplicate, err := s.SendMessage(ctx, owner.ID, peer.ID, "before edit", 8003, 0, 0)
	if err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if duplicate || senderPts <= 0 || recipientPts <= 0 {
		t.Fatalf("seed send returned pts=%d/%d duplicate=%t", senderPts, recipientPts, duplicate)
	}
	setOwnerWidthState(t, ctx, stateDB, owner.ID, ownerWireMax, message.LocalID+1)
	return runConnectedOwnerWidthRequest(t, h, owner.ID, &tg.MessagesEditMessageRequest{
		Peer: apiInputPeerUser(owner.ID, peer.ID), ID: int(message.LocalID), Message: "after edit",
	}, h.handleEditMessageAfterReplyOnConn)
}

func ownerWidthRefusedForward(t *testing.T, h *handlers, s *store.Store, ctx context.Context, stateDB *pgx.Conn, owner, peer, dest store.User) error {
	t.Helper()
	_, senderPts, recipientPts, duplicate, err := s.SendMessage(ctx, peer.ID, owner.ID, "forward source", 8004, 0, 0)
	if err != nil {
		t.Fatalf("seed source message: %v", err)
	}
	if duplicate || senderPts <= 0 || recipientPts <= 0 {
		t.Fatalf("seed send returned pts=%d/%d duplicate=%t", senderPts, recipientPts, duplicate)
	}
	message, ok, err := s.MessageByOwnerLocal(ctx, owner.ID, 1)
	if err != nil || !ok {
		t.Fatalf("load source copy: ok=%t err=%v", ok, err)
	}
	setOwnerWidthState(t, ctx, stateDB, dest.ID, ownerWireMax, 84)
	return runConnectedOwnerWidthRequest(t, h, owner.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: apiInputPeerUser(owner.ID, peer.ID), ID: []int{int(message.LocalID)},
		RandomID: []int64{8005}, ToPeer: apiInputPeerUser(owner.ID, dest.ID),
	}, h.handleForwardMessagesAfterReplyOnConn)
}

type ownerWidthHandler func(*mtproto.Conn, *mtproto.Request) (bin.Encoder, *replyUpdate, func(), error)

func runConnectedOwnerWidthRequest(t *testing.T, h *handlers, userID int64, request bin.Encoder, handler ownerWidthHandler) error {
	t.Helper()
	var buf bin.Buffer
	if err := request.Encode(&buf); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	key := retryTestKey(91)
	conn := mtproto.NewTestConn(&recordingNotifyTransport{}, key)
	conn.SetOwner(userID)
	r := &mtproto.Request{Ctx: context.Background(), UserID: userID, AuthKeyID: key.ID, Buf: &buf}
	result, update, afterReply, err := handler(conn, r)
	if result != nil || update != nil || afterReply != nil {
		t.Errorf("refused request returned result=%T update=%v afterReply=%t", result, update, afterReply != nil)
	}
	if _, pending := conn.PendingRPCUpdate(userID); pending {
		t.Errorf("sender RPC reservation remained pending after refusal")
	}
	return err
}

func setOwnerWidthState(t *testing.T, ctx context.Context, stateDB *pgx.Conn, ownerID, pts, nextLocalID int64) {
	t.Helper()
	if _, err := stateDB.Exec(ctx, `UPDATE update_state SET pts = $2, next_local_id = $3 WHERE user_id = $1`, ownerID, pts, nextLocalID); err != nil {
		t.Fatalf("set owner %d update state: %v", ownerID, err)
	}
}

func listenForOwnerWidthUpdates(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect update listener: %v", err)
	}
	if _, err := conn.Exec(ctx, "LISTEN "+store.ChannelUpdates); err != nil {
		if closeErr := conn.Close(ctx); closeErr != nil {
			t.Errorf("close update listener after LISTEN failure: %v", closeErr)
		}
		t.Fatalf("listen for updates: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) }) //nolint:errcheck // teardown
	return conn
}

func assertNoOwnerWidthUpdate(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	notification, err := conn.WaitForNotification(ctx)
	if err == nil {
		t.Fatalf("unexpected update notification: %+v", notification)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for update notification: %v", err)
	}
}

func apiInputPeerUser(viewerID, peerID int64) *tg.InputPeerUser {
	return &tg.InputPeerUser{UserID: peerID, AccessHash: pgtest.PeerDeriver().Derive(viewerID, peerhash.KindUser, peerID)}
}
