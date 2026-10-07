package api

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestDialogFilterEntityRangesUseUTF16Boundaries(t *testing.T) {
	t.Parallel()
	title := tg.TextWithEntities{
		Text:     "A😀B",
		Entities: []tg.MessageEntityClass{&tg.MessageEntityCustomEmoji{Offset: 1, Length: 2, DocumentID: 42}},
	}
	if _, err := validateDialogFilterEntities(title); err != nil {
		t.Fatalf("valid astral UTF-16 entity: %v", err)
	}
	title.Entities = []tg.MessageEntityClass{&tg.MessageEntityCustomEmoji{Offset: 2, Length: 1, DocumentID: 42}}
	if _, err := validateDialogFilterEntities(title); !errors.Is(err, errEntityBoundsInvalid) {
		t.Fatalf("surrogate-splitting entity error = %v, want ENTITY_BOUNDS_INVALID", err)
	}
}

func TestDialogFilterMarkerGuardUsesEarlierDateAndIncludesBoundary(t *testing.T) {
	t.Parallel()
	now := time.Unix(10_000, 0)
	if !dialogStateMarkerWithinGuard(now.Add(-60*time.Second), true, 10_000, now) {
		t.Fatal("marker exactly at the 60-second cutoff was excluded")
	}
	if dialogStateMarkerWithinGuard(now.Add(-61*time.Second), true, 10_000, now) {
		t.Fatal("marker older than the 60-second cutoff was included")
	}
	if !dialogStateMarkerWithinGuard(now.Add(-60*time.Second), true, 10_100, now) {
		t.Fatal("future client date was not clamped to server time")
	}
	if !dialogStateMarkerWithinGuard(now.Add(5*time.Second), true, 10_000, now) {
		t.Fatal("marker within the accepted 5-second skew was excluded")
	}
	if dialogStateMarkerWithinGuard(now, false, 10_000, now) {
		t.Fatal("missing marker triggered a refresh")
	}
}

func TestDialogFilterRateLimitUsesFixedSixtySecondFloodWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	user, err := s.CreateUser(ctx, "+15551090031")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := &handlers{store: s, log: slog.New(slog.DiscardHandler)}
	req := &mtproto.Request{Ctx: ctx, UserID: user.ID}
	for attempt := range 60 {
		if err := h.checkDialogFilterRateLimit(req); err != nil {
			t.Fatalf("mutation attempt %d was denied: %v", attempt+1, err)
		}
	}
	err = h.checkDialogFilterRateLimit(req)
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 || rpc.Message != "FLOOD_WAIT_60" {
		t.Fatalf("61st mutation error = %v, want FLOOD_WAIT_60", err)
	}
}

func TestDialogPinMutationsShareDialogFilterRateBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551090131")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551090132")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, peer.ID, "pin limit fixture", 901321, 0, 0); err != nil {
		t.Fatalf("seed existing 1:1 dialog: %v", err)
	}
	h := testHandlers(s)
	request := &mtproto.Request{Ctx: ctx, UserID: owner.ID}
	for attempt := range 59 {
		if err := h.checkDialogFilterRateLimit(request); err != nil {
			t.Fatalf("shared mutation budget attempt %d: %v", attempt+1, err)
		}
	}
	peerInput := &tg.InputPeerUser{
		UserID: peer.ID, AccessHash: h.peers.Derive(owner.ID, peerhash.KindUser, peer.ID),
	}
	pinRequest := &tg.MessagesToggleDialogPinRequest{
		Pinned: true,
		Peer:   &tg.InputDialogPeer{Peer: peerInput},
	}
	pinRequest.SetFlags()
	var pinBuf bin.Buffer
	if err := pinRequest.Encode(&pinBuf); err != nil {
		t.Fatalf("encode pin request: %v", err)
	}
	if _, err := h.handleToggleDialogPin(&mtproto.Request{Ctx: ctx, UserID: owner.ID, Buf: &pinBuf}); err != nil {
		t.Fatalf("pin at shared budget limit: %v", err)
	}
	filterRequest := &tg.MessagesUpdateDialogFilterRequest{ID: 2}
	filterRequest.SetFlags()
	var filterBuf bin.Buffer
	if err := filterRequest.Encode(&filterBuf); err != nil {
		t.Fatalf("encode filter request: %v", err)
	}
	_, err = h.handleUpdateDialogFilter(&mtproto.Request{Ctx: ctx, UserID: owner.ID, Buf: &filterBuf})
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 || rpc.Message != "FLOOD_WAIT_60" {
		t.Fatalf("filter mutation after 60 shared calls = %v, want FLOOD_WAIT_60", err)
	}
}
