package api

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPollVoteNotificationPushesDurableViewerScopedResults(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	creator, err := s.CreateUser(ctx, "+15551801001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551801002")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551801003")
	if err != nil {
		t.Fatalf("create other member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll push", []int64{voter.ID, other.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message, _, duplicate, err := s.SendChatMessage(ctx, store.FanOut{ChatID: chat.ID, FromID: creator.ID, Text: "", RandomID: 1801001})
	if err != nil || duplicate {
		t.Fatalf("send poll message = duplicate %v, err %v", duplicate, err)
	}
	ref := store.PollMessageRef{PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: message.LocalID}
	created, duplicate, err := s.CreatePoll(ctx, creator.ID, ref, store.PollDraft{
		Question: []byte("Choose"), Quiz: true,
		Answers: []store.PollAnswer{
			{Option: []byte("a"), Text: []byte("A"), Correct: true},
			{Option: []byte("b"), Text: []byte("B")},
		},
	})
	if err != nil || duplicate {
		t.Fatalf("create poll = duplicate %v, err %v", duplicate, err)
	}
	refs := map[int64]store.PollMessageRef{}
	for _, ownerID := range []int64{creator.ID, voter.ID, other.ID} {
		history, historyErr := s.History(ctx, ownerID, store.PeerTypeChat, chat.ID, 0, 10)
		if historyErr != nil || len(history) != 1 {
			t.Fatalf("history for %d = %d rows, err %v", ownerID, len(history), historyErr)
		}
		refs[ownerID] = store.PollMessageRef{PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: history[0].LocalID}
	}

	registry := mtproto.NewSessionRegistry()
	transports := map[int64]*recordingNotifyTransport{}
	keys := map[int64]crypto.AuthKey{}
	for i, ownerID := range []int64{creator.ID, voter.ID, other.ID} {
		transport := &recordingNotifyTransport{}
		key := retryTestKey(byte(81 + i))
		conn := mtproto.NewTestConn(transport, key)
		conn.SetOwner(ownerID)
		if !registry.Add(ownerID, conn) {
			t.Fatalf("register connection for %d", ownerID)
		}
		t.Cleanup(func() { registry.Remove(ownerID, conn) })
		transports[ownerID] = transport
		keys[ownerID] = key
	}
	updater := NewUpdater(s, registry, nil, pgtest.PeerDeriver())
	_, stop, err := store.StartListener(ctx, dsn,
		updater.Deliver,
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, bool) {},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop listener: %v", err)
		}
	})
	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	var request bin.Buffer
	if err := (&tg.MessagesSendVoteRequest{
		Peer: &tg.InputPeerChat{ChatID: chat.ID}, MsgID: int(refs[voter.ID].LocalID), Options: [][]byte{[]byte("a")},
	}).Encode(&request); err != nil {
		t.Fatalf("encode vote: %v", err)
	}
	h := testHandlers(s)
	if _, err := h.handleSendVote(&mtproto.Request{Ctx: ctx, UserID: voter.ID, Buf: &request}); err != nil {
		t.Fatalf("cast vote: %v", err)
	}
	for _, ownerID := range []int64{creator.ID, voter.ID, other.ID} {
		deadline := time.Now().Add(5 * time.Second)
		for transports[ownerID].count() == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		frames := transports[ownerID].framesFrom(0)
		if len(frames) != 1 {
			t.Fatalf("owner %d push frames = %d, want one durable update batch", ownerID, len(frames))
		}
		decoded := decodeServerFrames(t, keys[ownerID], frames)
		if len(decoded) != 1 || decoded[0].push == nil {
			t.Fatalf("owner %d decoded push = %+v, want one pushed Updates batch", ownerID, decoded)
		}
		var update *tg.UpdateEditMessage
		for _, candidate := range decoded[0].push.Updates {
			if _, ok := candidate.(*tg.UpdateMessagePoll); ok {
				t.Fatalf("owner %d received a transient poll update", ownerID)
			}
			if edit, ok := candidate.(*tg.UpdateEditMessage); ok {
				message, messageOK := edit.Message.(*tg.Message)
				if !messageOK {
					continue
				}
				media, mediaOK := message.Media.(*tg.MessageMediaPoll)
				if mediaOK && media.Poll.ID == created.ID {
					update = edit
				}
			}
		}
		if update == nil {
			t.Fatalf("owner %d durable push omitted poll edit for poll %d: %+v", ownerID, created.ID, decoded[0].push.Updates)
		}
		message, ok := update.Message.(*tg.Message)
		if !ok {
			t.Fatalf("owner %d edit message = %T, want *tg.Message", ownerID, update.Message)
		}
		pollMedia, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok {
			t.Fatalf("owner %d edit media = %T, want *tg.MessageMediaPoll", ownerID, message.Media)
		}
		wantKey := ownerID == voter.ID
		if len(pollMedia.Results.Results) != 2 || pollMedia.Results.Results[0].Chosen != wantKey || pollMedia.Results.Results[0].Correct != wantKey {
			t.Errorf("owner %d poll results = %+v, want voter-specific chosen/key %v", ownerID, pollMedia.Results.Results, wantKey)
		}
		ownerState, stateErr := s.State(ctx, ownerID)
		if stateErr != nil {
			t.Fatalf("read owner %d state after vote: %v", ownerID, stateErr)
		}
		if got := registry.Conns(ownerID)[0].LastPushedPts(); got != ownerState.Pts {
			t.Errorf("owner %d durable poll push pts = %d, want %d", ownerID, got, ownerState.Pts)
		}
	}
}
