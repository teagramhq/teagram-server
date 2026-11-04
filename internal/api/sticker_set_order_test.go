package api_test

import (
	"context"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestReorderStickerSetsChangesInstalledOrderForAllStickers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551830010")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551830011")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	h := fullChannelDispatcher(s)
	ids := createInstalledUserStickerSets(t, s, h, blobs, owner.ID, "order_alpha", "order_bravo", "order_charlie")

	before := getAllStickerSets(t, h, owner.ID, 0)
	if got := stickerSetIDs(before.Sets); !slices.Equal(got, ids) {
		t.Fatalf("installed order before reorder = %v, want install order %v", got, ids)
	}
	// The unknown id is ignored and the unlisted set keeps its place after the listed ones.
	if got := reorderStickerSets(t, h, owner.ID, false, false, []int64{ids[2], 999_999_999, ids[0]}); got != "" {
		t.Fatalf("reorderStickerSets = %q, want boolTrue", got)
	}
	want := []int64{ids[2], ids[0], ids[1]}
	after := getAllStickerSets(t, h, owner.ID, 0)
	if got := stickerSetIDs(after.Sets); !slices.Equal(got, want) {
		t.Fatalf("installed order after reorder = %v, want %v", got, want)
	}
	if after.Hash == before.Hash {
		t.Fatalf("getAllStickers hash %d did not change after reorder", after.Hash)
	}
	if result := getAllStickerSetResult(t, h, owner.ID, after.Hash); result != nil {
		t.Fatal("getAllStickers at the reordered hash returned a full result")
	}

	reopened := openStickerStore(t, blobs, dsn)
	if got := stickerSetIDs(getAllStickerSets(t, fullChannelDispatcher(reopened), owner.ID, 0).Sets); !slices.Equal(got, want) {
		t.Fatalf("reordered order after reopen = %v, want %v", got, want)
	}
	if got := stickerSetIDs(getAllStickerSets(t, h, other.ID, 0).Sets); len(got) != 0 {
		t.Fatalf("other account sticker sets = %v, want empty", got)
	}
}

func TestReorderStickerSetsIsScopedByKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551830012")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	ids := createInstalledUserStickerSets(t, s, h, blobs, owner.ID, "kind_alpha", "kind_bravo")

	// Both sets are regular stickers, so a masks reorder names no installed mask set and changes nothing.
	if got := reorderStickerSets(t, h, owner.ID, true, false, []int64{ids[1], ids[0]}); got != "" {
		t.Fatalf("masks reorder = %q, want boolTrue", got)
	}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !slices.Equal(got, ids) {
		t.Fatalf("regular order after masks reorder = %v, want %v", got, ids)
	}
	if got := reorderStickerSets(t, h, owner.ID, false, false, []int64{ids[1], ids[0]}); got != "" {
		t.Fatalf("regular reorder = %q, want boolTrue", got)
	}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !slices.Equal(got, []int64{ids[1], ids[0]}) {
		t.Fatalf("regular order after regular reorder = %v, want %v", got, []int64{ids[1], ids[0]})
	}
	if got := reorderStickerSets(t, h, owner.ID, true, true, ids); got != "STICKERSET_INVALID" {
		t.Fatalf("masks and emojis reorder = %q, want STICKERSET_INVALID", got)
	}
}

func TestReorderStickerSetsRequiresAuthenticatedAccount(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	h := fullChannelDispatcher(s)
	if got := reorderStickerSets(t, h, 0, false, false, nil); got != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("anonymous reorder = %q, want AUTH_KEY_UNREGISTERED", got)
	}
}

// createInstalledUserStickerSets creates one regular set per name for ownerID and installs each, in the order given.
func createInstalledUserStickerSets(t *testing.T, s *store.Store, h mtproto.Handler, blobs blob.Store, ownerID int64, names ...string) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		file := storedStickerDocument(t, s, blobs, ownerID, validStickerTGS(t))
		set := createStickerSet(t, h, ownerID, createStickerRequest(name, file, false, false))
		if got := installStickerSet(t, h, ownerID, &tg.InputStickerSetID{ID: set.Set.ID, AccessHash: set.Set.AccessHash}); got != "success" {
			t.Fatalf("install %s = %q, want success", name, got)
		}
		ids = append(ids, set.Set.ID)
	}
	return ids
}

func stickerSetIDs(sets []tg.StickerSet) []int64 {
	ids := make([]int64, 0, len(sets))
	for _, set := range sets {
		ids = append(ids, set.ID)
	}
	return ids
}

// reorderStickerSets returns "" for boolTrue, or the RPC error name.
func reorderStickerSets(t *testing.T, h mtproto.Handler, userID int64, masks, emojis bool, order []int64) string {
	t.Helper()
	req := &tg.MessagesReorderStickerSetsRequest{Masks: masks, Emojis: emojis, Order: order}
	req.SetFlags()
	body := dispatchChatMethod(t, h, userID, "messages.reorderStickerSets", req)
	var result tg.BoolTrue
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return ""
	}
	return stickerRPCError(t, body)
}
