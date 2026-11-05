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

func TestReorderStickerSetsReordersMaskAndEmojiSets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551830013")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	regularIDs := createInstalledUserStickerSets(t, s, h, blobs, owner.ID, "kind_regular_alpha", "kind_regular_bravo")
	maskIDs := createInstalledUserStickerSetsOfKind(t, s, h, blobs, owner.ID, true, false, "kind_mask_alpha", "kind_mask_bravo")
	emojiIDs := createInstalledUserStickerSetsOfKind(t, s, h, blobs, owner.ID, false, true, "kind_emoji_alpha", "kind_emoji_bravo")

	beforeMasks := getAllStickerSets(t, h, owner.ID, 0)
	if got, want := stickerSetIDs(beforeMasks.Sets), append(append([]int64(nil), regularIDs...), maskIDs...); !slices.Equal(got, want) {
		t.Fatalf("all-sticker order before mask reorder = %v, want %v", got, want)
	}
	if got, want := stickerSetIDs(getEmojiStickerSets(t, h, owner.ID, 0).Sets), emojiIDs; !slices.Equal(got, want) {
		t.Fatalf("emoji order before reorder = %v, want %v", got, want)
	}
	if got := reorderStickerSets(t, h, owner.ID, true, false, []int64{maskIDs[1], maskIDs[0]}); got != "" {
		t.Fatalf("mask reorder = %q, want boolTrue", got)
	}
	wantMaskOrder := []int64{maskIDs[1], maskIDs[0], regularIDs[0], regularIDs[1]}
	afterMasks := getAllStickerSets(t, h, owner.ID, beforeMasks.Hash)
	if got := stickerSetIDs(afterMasks.Sets); !slices.Equal(got, wantMaskOrder) {
		t.Fatalf("all-sticker order after mask reorder = %v, want %v", got, wantMaskOrder)
	}
	if afterMasks.Hash == beforeMasks.Hash {
		t.Fatalf("all-sticker hash %d did not change after mask reorder", afterMasks.Hash)
	}
	if got := stickerSetIDs(getEmojiStickerSets(t, h, owner.ID, 0).Sets); !slices.Equal(got, emojiIDs) {
		t.Fatalf("emoji order after mask reorder = %v, want unchanged %v", got, emojiIDs)
	}

	beforeEmojis := getEmojiStickerSets(t, h, owner.ID, 0)
	if got := reorderStickerSets(t, h, owner.ID, false, true, []int64{emojiIDs[1], emojiIDs[0]}); got != "" {
		t.Fatalf("emoji reorder = %q, want boolTrue", got)
	}
	wantEmojiOrder := []int64{emojiIDs[1], emojiIDs[0]}
	afterEmojis := getEmojiStickerSets(t, h, owner.ID, beforeEmojis.Hash)
	if got := stickerSetIDs(afterEmojis.Sets); !slices.Equal(got, wantEmojiOrder) {
		t.Fatalf("emoji order after reorder = %v, want %v", got, wantEmojiOrder)
	}
	if afterEmojis.Hash == beforeEmojis.Hash {
		t.Fatalf("emoji hash %d did not change after reorder", afterEmojis.Hash)
	}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !slices.Equal(got, wantMaskOrder) {
		t.Fatalf("all-sticker order after emoji reorder = %v, want unchanged %v", got, wantMaskOrder)
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
	return createInstalledUserStickerSetsOfKind(t, s, h, blobs, ownerID, false, false, names...)
}

func createInstalledUserStickerSetsOfKind(t *testing.T, s *store.Store, h mtproto.Handler, blobs blob.Store, ownerID int64, masks, emojis bool, names ...string) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(names))
	for _, name := range names {
		sticker := validStickerTGS(t)
		if emojis {
			sticker = validStickerTGSSize(t, 100, 100)
		}
		file := storedStickerDocument(t, s, blobs, ownerID, sticker)
		set := createStickerSet(t, h, ownerID, createStickerRequest(name, file, masks, emojis))
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
