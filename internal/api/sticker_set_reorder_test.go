package api_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestReorderStickerSetsPushesOrderToOtherSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819103")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	registry := mtproto.NewSessionRegistry()
	_, callerTransport := newStickerSetNotifyConn(t, registry, owner.ID, 0)
	_, siblingTransport := newStickerSetNotifyConn(t, registry, owner.ID, 12)
	updater := api.NewUpdater(s, 1, registry, nil, pgtest.PeerDeriver())
	startStickerSetNotifyListener(t, ctx, dsn, updater)
	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for replica listener: %v", err)
	}

	h := fullChannelDispatcher(s)
	a := createReorderTestSet(t, s, blobs, h, owner.ID, "order_push_a")
	b := createReorderTestSet(t, s, blobs, h, owner.ID, "order_push_b")
	// Plain installs coalesce per session, so each one is awaited before the next.
	for i, set := range []*tg.MessagesStickerSet{a, b} {
		if got := installStickerSet(t, h, owner.ID, reorderTestSetRef(set)); got != "success" {
			t.Fatalf("install set %d = %q, want success", set.Set.ID, got)
		}
		if !waitStickerSetNotifyWrites(int32(i+1), siblingTransport) {
			t.Fatalf("sibling sticker-set writes after install %d = %d, want %d", i+1, siblingTransport.sends.Load(), i+1)
		}
	}

	if !reorderStickerSets(t, h, owner.ID, &tg.MessagesReorderStickerSetsRequest{Order: []int64{b.Set.ID, a.Set.ID}}) {
		t.Fatal("reorderStickerSets result = false, want boolTrue")
	}
	if !waitStickerSetNotifyWrites(3, siblingTransport) {
		t.Fatalf("sibling sticker-set writes after reorder = %d, want 3", siblingTransport.sends.Load())
	}
	if got := callerTransport.sends.Load(); got != 0 {
		t.Errorf("originating session writes = %d, want 0", got)
	}
}

func TestReorderStickerSetsPersistsOrderAcrossReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819101")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551819102")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	h := fullChannelDispatcher(s)
	a := createReorderTestSet(t, s, blobs, h, owner.ID, "reorder_a")
	b := createReorderTestSet(t, s, blobs, h, owner.ID, "reorder_b")
	c := createReorderTestSet(t, s, blobs, h, owner.ID, "reorder_c")
	d := createReorderTestSet(t, s, blobs, h, owner.ID, "reorder_d")
	for _, set := range []*tg.MessagesStickerSet{a, b, c} {
		if got := installStickerSet(t, h, owner.ID, reorderTestSetRef(set)); got != "success" {
			t.Fatalf("install set %d = %q, want success", set.Set.ID, got)
		}
		if got := installStickerSet(t, h, other.ID, reorderTestSetRef(set)); got != "success" {
			t.Fatalf("other account install set %d = %q, want success", set.Set.ID, got)
		}
	}

	initial := getAllStickerSets(t, h, owner.ID, 0)
	wantInitial := []int64{a.Set.ID, b.Set.ID, c.Set.ID}
	slices.Sort(wantInitial)
	if got := stickerSetIDs(initial.Sets); !reflect.DeepEqual(got, wantInitial) {
		t.Fatalf("initial order = %v, want set-id order %v", got, wantInitial)
	}

	if got := reorderStickerSets(t, h, owner.ID, &tg.MessagesReorderStickerSetsRequest{Order: []int64{c.Set.ID, a.Set.ID}}); got != true {
		t.Fatalf("reorderStickerSets result = %v, want boolTrue", got)
	}
	reordered := getAllStickerSets(t, h, owner.ID, initial.Hash)
	wantReordered := []int64{c.Set.ID, a.Set.ID, b.Set.ID}
	if got := stickerSetIDs(reordered.Sets); !reflect.DeepEqual(got, wantReordered) {
		t.Fatalf("order after reorder = %v, want %v", got, wantReordered)
	}
	if reordered.Hash == initial.Hash {
		t.Fatalf("getAllStickers hash stayed %d after reorder", reordered.Hash)
	}
	for i, set := range reordered.Sets {
		if initialDate, ok := installedDateByID(initial.Sets, set.ID); !ok || set.InstalledDate != initialDate {
			t.Fatalf("set %d at position %d installed date = %d, want unchanged %d", set.ID, i, set.InstalledDate, initialDate)
		}
	}

	reconnected := getAllStickerSets(t, fullChannelDispatcher(s), owner.ID, 0)
	if got := stickerSetIDs(reconnected.Sets); !reflect.DeepEqual(got, wantReordered) {
		t.Fatalf("order after reconnect = %v, want %v", got, wantReordered)
	}

	otherAll := getAllStickerSets(t, h, other.ID, 0)
	if got := stickerSetIDs(otherAll.Sets); !reflect.DeepEqual(got, wantInitial) {
		t.Fatalf("other account order = %v, want unchanged set-id order %v", got, wantInitial)
	}

	if got := reorderStickerSets(t, h, owner.ID, &tg.MessagesReorderStickerSetsRequest{Order: []int64{b.Set.ID, 999999999}}); got != true {
		t.Fatalf("reorder with a set not installed = %v, want boolTrue", got)
	}
	wantIgnoringUnknown := []int64{b.Set.ID, c.Set.ID, a.Set.ID}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !reflect.DeepEqual(got, wantIgnoringUnknown) {
		t.Fatalf("order after unknown id = %v, want %v", got, wantIgnoringUnknown)
	}

	if got := installStickerSet(t, h, owner.ID, reorderTestSetRef(d)); got != "success" {
		t.Fatalf("install set after reorder = %q, want success", got)
	}
	wantAfterInstall := []int64{b.Set.ID, c.Set.ID, a.Set.ID, d.Set.ID}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !reflect.DeepEqual(got, wantAfterInstall) {
		t.Fatalf("order after install = %v, want new set last %v", got, wantAfterInstall)
	}

	if !uninstallStickerSet(t, h, owner.ID, reorderTestSetRef(c)) {
		t.Fatal("uninstall returned false")
	}
	wantAfterUninstall := []int64{b.Set.ID, a.Set.ID, d.Set.ID}
	if got := stickerSetIDs(getAllStickerSets(t, h, owner.ID, 0).Sets); !reflect.DeepEqual(got, wantAfterUninstall) {
		t.Fatalf("order after uninstall = %v, want %v", got, wantAfterUninstall)
	}
}

func createReorderTestSet(t *testing.T, s *store.Store, blobs blob.Store, h mtproto.Handler, ownerID int64, shortName string) *tg.MessagesStickerSet {
	t.Helper()
	file := storedStickerDocument(t, s, blobs, ownerID, validStickerTGS(t))
	return createStickerSet(t, h, ownerID, createStickerRequest(shortName, file, false, false))
}

func reorderTestSetRef(set *tg.MessagesStickerSet) tg.InputStickerSetClass {
	return &tg.InputStickerSetID{ID: set.Set.ID, AccessHash: set.Set.AccessHash}
}

func stickerSetIDs(sets []tg.StickerSet) []int64 {
	ids := make([]int64, 0, len(sets))
	for _, set := range sets {
		ids = append(ids, set.ID)
	}
	return ids
}

func installedDateByID(sets []tg.StickerSet, id int64) (int, bool) {
	for _, set := range sets {
		if set.ID == id {
			return set.InstalledDate, true
		}
	}
	return 0, false
}

// reorderStickerSets dispatches messages.reorderStickerSets and reports whether
// the server answered boolTrue.
func reorderStickerSets(t *testing.T, h mtproto.Handler, userID int64, req *tg.MessagesReorderStickerSetsRequest) bool {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.reorderStickerSets", req)
	var box tg.BoolBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode reorderStickerSets: %v", err)
	}
	_, ok := box.Bool.(*tg.BoolTrue)
	return ok
}
