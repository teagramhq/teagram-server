package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// A reaction on a copy its owner has soft-deleted must not be pushed to that
// owner: the update names a message gone from every read surface. Deletion is
// per-copy, so the party who kept their copy still gets the push, and the
// reaction row itself is untouched either way.
func TestDeliverReactionsSkipsDeletedCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reg := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, 2, reg, nil, pgtest.PeerDeriver())

	p := seedReactionPair(t, s, "+15551352001", "+15551352002", "")

	// B deletes their copy, A keeps theirs.
	if _, err := s.DeleteMessages(ctx, p.b.ID, []int64{p.bLocalID}, false); err != nil {
		t.Fatalf("delete B copy: %v", err)
	}

	// A reacts on the copy they still hold. The write reaches both copies,
	// including B's deleted one — this ticket changes push, not storage.
	targets, err := s.SendReaction(ctx, p.a.ID, p.aLocalID, "❤")
	if err != nil {
		t.Fatalf("react: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("reaction targets = %d, want 2", len(targets))
	}
	stored, err := s.ReactionsByOwnerLocal(ctx, p.b.ID, p.bLocalID)
	if err != nil {
		t.Fatalf("reactions on B copy: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored reactions on deleted copy = %d, want 1", len(stored))
	}

	aConn, aFT := newConnFor(t, reg, p.a.ID)
	_, bFT := newConnFor(t, reg, p.b.ID)

	delivered := make(chan struct{}, len(targets))
	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, bool) {},
		func(context.Context, int64, int) {},
		func(pushCtx context.Context, ownerID, localID, userID int64) {
			updater.DeliverReactions(pushCtx, ownerID, localID, userID)
			delivered <- struct{}{}
		},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start reaction listener: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop reaction listener: %v", err)
		}
	})
	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for reaction listener: %v", err)
	}

	// The reaction is committed; the originating RPC closes before its nudges.
	rpcCtx, cancelRPC := context.WithCancel(ctx)
	cancelRPC()
	notifyCtx, cancelNotify := store.NotificationContext(rpcCtx)
	defer cancelNotify()
	for _, target := range targets {
		if err := s.Notify(notifyCtx, store.ChannelReactions, store.ReactionPayload(target.OwnerID, target.LocalID, target.OwnerID)); err != nil {
			t.Fatalf("notify reaction after cancellation: %v", err)
		}
	}
	for range targets {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatal("reaction notification was not delivered")
		}
	}

	if bFT.wasSent() {
		t.Error("B received a reaction push for a message B deleted")
	}
	if !aFT.wasSent() {
		t.Error("A received no reaction push — B's delete must not suppress A's")
	}
	// Reactions stay a zero-pts transient push for the recipient who still gets one.
	if got := aConn.LastPushedPts(); got != 0 {
		t.Errorf("A pts = %d, want 0", got)
	}
}

func TestDeliverReactionsAggregatesBasicGroupCountsForEveryMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	reg := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, 2, reg, nil, pgtest.PeerDeriver())
	fixture := newBasicGroupReactionFixture(t, s)
	seedBasicGroupReactionCounts(t, s, fixture)

	transports := make(map[int64]*fakeTransport, len(fixture.members))
	for _, member := range fixture.members {
		_, transport := newConnFor(t, reg, member.ID)
		transports[member.ID] = transport
	}

	for _, member := range fixture.members {
		updater.DeliverReactions(ctx, member.ID, fixture.localID[member.ID], member.ID)
		update := lastPushedReaction(t, transports[member.ID])
		chosenEmoji := "👍"
		if member.ID == fixture.members[2].ID {
			chosenEmoji = "❤"
		}
		assertBasicGroupReactionCounts(t, update.Reactions, 2, chosenEmoji)
	}

	clearingMember := fixture.members[1]
	if _, rpc := sendReaction(t, s, clearingMember.ID, api.InputPeerChat(clearingMember.ID, fixture.chatID), fixture.localID[clearingMember.ID]); rpc != nil {
		t.Fatalf("clear member %d reaction: %s", clearingMember.ID, rpc.ErrorMessage)
	}
	for _, member := range fixture.members {
		transport := transports[member.ID]
		before := transport.sentMessages()
		updater.DeliverReactions(ctx, member.ID, fixture.localID[member.ID], member.ID)
		if got := transport.sentMessages(); got != before+1 {
			t.Fatalf("member %d push count = %d, want %d", member.ID, got, before+1)
		}
		update := lastPushedReaction(t, transport)
		chosenEmoji := ""
		switch member.ID {
		case fixture.members[0].ID:
			chosenEmoji = "👍"
		case fixture.members[2].ID:
			chosenEmoji = "❤"
		}
		assertBasicGroupReactionCounts(t, update.Reactions, 1, chosenEmoji)
	}
}

func lastPushedReaction(t *testing.T, transport *fakeTransport) *tg.UpdateMessageReactions {
	t.Helper()
	data, err := crypto.NewClientCipher(crypto.DefaultRand()).DecryptFromBuffer(testKey(), &bin.Buffer{Buf: transport.lastSentMessage()})
	if err != nil {
		t.Fatalf("decrypt pushed update: %v", err)
	}
	body := data.MessageDataWithPadding[:data.MessageDataLen]
	var short tg.UpdateShort
	if err := short.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode pushed updateShort: %v", err)
	}
	update, ok := short.Update.(*tg.UpdateMessageReactions)
	if !ok {
		t.Fatalf("pushed update = %T, want *tg.UpdateMessageReactions", short.Update)
	}
	return update
}
