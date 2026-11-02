package api_test

import (
	"bytes"
	"context"
	"path"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/reactionassets"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestGetArchivedStickersReturnsEmptyForEveryFilterAndOffset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810009")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	ref := &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}
	if got := installStickerSet(t, h, owner.ID, ref); got != "success" {
		t.Fatalf("install sticker set = %q, want success", got)
	}
	if sets := getAllStickerSets(t, h, owner.ID, 0); len(sets.Sets) != 1 {
		t.Fatalf("installed sets = %d, want one setup set", len(sets.Sets))
	}

	for _, test := range []struct {
		name string
		req  *tg.MessagesGetArchivedStickersRequest
	}{
		{name: "no flags", req: &tg.MessagesGetArchivedStickersRequest{}},
		{name: "masks", req: &tg.MessagesGetArchivedStickersRequest{Masks: true, OffsetID: -1, Limit: -7}},
		{name: "emojis", req: &tg.MessagesGetArchivedStickersRequest{Emojis: true, OffsetID: 9_007_199_254_740_991, Limit: 100_000}},
		{name: "masks and emojis", req: &tg.MessagesGetArchivedStickersRequest{Masks: true, Emojis: true, OffsetID: -9_007_199_254_740_991, Limit: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := getArchivedStickerSets(t, h, owner.ID, test.req)
			if got.Count != 0 || len(got.Sets) != 0 {
				t.Fatalf("archived stickers = count %d, %d sets; want empty", got.Count, len(got.Sets))
			}
		})
	}

	if got := stickerSetRPCMessage(t, dispatchChatMethod(t, h, 0, "messages.getArchivedStickers", &tg.MessagesGetArchivedStickersRequest{})); got != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unbound getArchivedStickers = %q, want AUTH_KEY_UNREGISTERED", got)
	}
	if got := stickerSetRPCMessage(t, dispatchChatMethod(t, h, owner.ID, "messages.getArchivedStickers", malformedArchivedStickersRequest{})); got != "INPUT_METHOD_INVALID" {
		t.Fatalf("malformed getArchivedStickers = %q, want INPUT_METHOD_INVALID", got)
	}
}

func TestGetArchivedStickersDoesNotSpendUnimplementedBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810010")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)

	for i := range 64 {
		msgID := int64(1<<32) + int64(i+1)*4
		var body bin.Buffer
		if err := (&tg.MessagesGetArchivedStickersRequest{Limit: i + 1}).Encode(&body); err != nil {
			t.Fatalf("encode getArchivedStickers call %d: %v", i+1, err)
		}
		if err := h.OnMessage(conn, &mtproto.Request{
			AuthKeyID: key.ID,
			UserID:    owner.ID,
			MsgID:     msgID,
			Buf:       &body,
			Ctx:       ctx,
		}); err != nil {
			t.Fatalf("dispatch getArchivedStickers call %d: %v", i+1, err)
		}
	}
	var archived tg.MessagesArchivedStickers
	if err := archived.Decode(&bin.Buffer{Buf: transport.result(t, key, int64(1<<32)+64*4)}); err != nil {
		t.Fatalf("last getArchivedStickers response: %v", err)
	}
	if archived.Count != 0 || len(archived.Sets) != 0 {
		t.Fatalf("getArchivedStickers = count %d, %d sets; want empty", archived.Count, len(archived.Sets))
	}

	var unknown bin.Buffer
	if err := (&tg.AccountRegisterDeviceRequest{}).Encode(&unknown); err != nil {
		t.Fatalf("encode unknown method: %v", err)
	}
	msgID := int64(1<<32) + 65*4
	err = h.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID,
		UserID:    owner.ID,
		MsgID:     msgID,
		Buf:       &unknown,
		Ctx:       ctx,
	})
	if err == nil {
		t.Fatal("unknown method after archived sticker calls succeeded")
	}
	if rpc := mustRPCError(t, err); rpc.Code != 400 || rpc.Message != "INPUT_METHOD_INVALID" {
		t.Fatalf("unknown method after archived sticker calls = %d %s, want 400 INPUT_METHOD_INVALID", rpc.Code, rpc.Message)
	}
}

type malformedArchivedStickersRequest struct{}

func (malformedArchivedStickersRequest) Encode(b *bin.Buffer) error {
	b.PutID(tg.MessagesGetArchivedStickersRequestTypeID)
	return nil
}

func getArchivedStickerSets(t *testing.T, h mtproto.Handler, userID int64, req *tg.MessagesGetArchivedStickersRequest) *tg.MessagesArchivedStickers {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getArchivedStickers", req)
	var got tg.MessagesArchivedStickers
	if err := got.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getArchivedStickers: %v", err)
	}
	return &got
}

func TestStickerSetInstallListsDownloadsAndUninstallsCatalogue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551810001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551810002")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	h := fullChannelDispatcher(s)
	shortName := "twemoji_reactions"
	shortRef := &tg.InputStickerSetShortName{ShortName: shortName}

	if got := installStickerSet(t, h, owner.ID, shortRef); got != "success" {
		t.Fatalf("install result = %q, want success", got)
	}
	installedResponse := getStickerSet(t, h, owner.ID, shortRef, 0)
	installedDate, installed := installedResponse.Set.GetInstalledDate()
	if !installed || installedDate <= 0 {
		t.Errorf("getStickerSet installed date = %d, present %t, want a positive date", installedDate, installed)
	}
	otherResponse := getStickerSet(t, h, other.ID, shortRef, 0)
	if otherDate, otherInstalled := otherResponse.Set.GetInstalledDate(); otherInstalled {
		t.Errorf("other account getStickerSet installed date = %d, want absent", otherDate)
	}
	ownerSets := getAllStickerSets(t, h, owner.ID, 0)
	if len(ownerSets.Sets) != 1 {
		t.Fatalf("owner sticker sets = %d, want one", len(ownerSets.Sets))
	}
	set := ownerSets.Sets[0]
	if set.ShortName != shortName || set.Count != 73 || set.ID == 0 || set.AccessHash == 0 || set.Hash == 0 {
		t.Fatalf("installed set = %+v, want twemoji_reactions with 73 stickers and stable identity", set)
	}
	if set.ID >= 0 {
		t.Fatalf("sticker set id = %d, want a reserved negative id", set.ID)
	}
	otherSets := getAllStickerSets(t, h, other.ID, 0)
	if len(otherSets.Sets) != 0 {
		t.Fatalf("other account sticker sets = %d, want empty", len(otherSets.Sets))
	}
	reopened, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(newBlobs(t)))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	if got := getAllStickerSets(t, fullChannelDispatcher(reopened), owner.ID, 0); len(got.Sets) != 1 {
		t.Fatalf("reopened store installed sets = %d, want one", len(got.Sets))
	}

	full := getStickerSet(t, h, owner.ID, shortRef, 0)
	if full.Set.ID != set.ID || full.Set.AccessHash != set.AccessHash || full.Set.Count != 73 {
		t.Fatalf("getStickerSet metadata = %+v, want installed catalogue identity", full.Set)
	}
	entries, err := reactionassets.Manifest()
	if err != nil {
		t.Fatalf("read committed Twemoji manifest: %v", err)
	}
	if len(full.Documents) != 73 || len(full.Packs) != 73 {
		t.Fatalf("sticker set contains %d documents and %d packs, want 73 each", len(full.Documents), len(full.Packs))
	}
	var firstDocument *tg.Document
	for i, class := range full.Documents {
		document, ok := class.(*tg.Document)
		if !ok {
			t.Fatalf("document %d = %T, want *tg.Document", i, class)
		}
		if document.Date != 1_735_689_600 {
			t.Errorf("document %d date = %d, want stable 2025-01-01 UTC timestamp", i, document.Date)
		}
		if document.ID >= 0 || document.AccessHash == 0 || document.MimeType != "image/webp" || document.Size <= 0 {
			t.Errorf("document %d identity/metadata = id %d hash %d MIME %q size %d", i, document.ID, document.AccessHash, document.MimeType, document.Size)
		}
		var imageSizeFound, stickerFound, filenameFound bool
		for _, attribute := range document.Attributes {
			switch value := attribute.(type) {
			case *tg.DocumentAttributeImageSize:
				imageSizeFound = value.W == 512 && value.H == 512
			case *tg.DocumentAttributeSticker:
				setRef, ok := value.Stickerset.(*tg.InputStickerSetID)
				stickerFound = ok && value.Alt == entries[i].Emoji && setRef.ID == set.ID && setRef.AccessHash == set.AccessHash
			case *tg.DocumentAttributeFilename:
				filenameFound = path.Ext(value.FileName) == ".webp"
			}
		}
		if !imageSizeFound || !stickerFound || !filenameFound {
			t.Errorf("document %d attributes do not name a 512x512 %q sticker in %s", i, entries[i].Emoji, shortName)
		}
		if i == 0 {
			firstDocument = document
		}
	}
	if firstDocument != nil {
		stem := strings.TrimSuffix(entries[0].SVG, ".svg")
		want, err := reactionassets.Files.ReadFile("static/" + stem + ".webp")
		if err != nil {
			t.Fatalf("read committed sticker WebP: %v", err)
		}
		got := getBytes(t, s, nil, owner.ID, firstDocument, 0, int(firstDocument.Size))
		if !bytes.Equal(got, want) {
			t.Fatal("downloaded sticker bytes differ from the committed WebP")
		}
		if len(got) < 12 || !bytes.Equal(got[:4], []byte("RIFF")) || !bytes.Equal(got[8:12], []byte("WEBP")) {
			t.Fatalf("downloaded sticker does not have WebP magic bytes: %x", got[:min(len(got), 12)])
		}
	}

	byID := &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}
	if got := getStickerSet(t, h, owner.ID, byID, 0).Set.ID; got != set.ID {
		t.Fatalf("getStickerSet by id returned set %d, want %d", got, set.ID)
	}
	if got := installStickerSetWithArchive(t, h, owner.ID, byID, true); got != "success" {
		t.Fatalf("repeated install result = %q, want success", got)
	}
	if full := getAllStickerSetResult(t, h, owner.ID, ownerSets.Hash); full != nil {
		t.Fatalf("getAllStickers with current hash returned full hash %d with %d sets", full.Hash, len(full.Sets))
	}
	if unchanged := getStickerSetNotModified(t, h, owner.ID, byID, set.Hash); !unchanged {
		t.Fatal("getStickerSet with current set hash did not return notModified")
	}

	if !uninstallStickerSet(t, h, owner.ID, byID) {
		t.Fatal("uninstall returned false")
	}
	if !uninstallStickerSet(t, h, owner.ID, byID) {
		t.Fatal("uninstall of an absent set returned false")
	}
	removedSet := getStickerSet(t, h, owner.ID, byID, 0).Set
	if _, ok := removedSet.GetInstalledDate(); ok {
		t.Error("getStickerSet after uninstall still has an installed date")
	}
	removed := getAllStickerSetResult(t, h, owner.ID, ownerSets.Hash)
	if removed == nil || len(removed.Sets) != 0 || removed.Hash == ownerSets.Hash {
		t.Fatalf("getAllStickers after uninstall = %+v, want changed hash and empty sets", removed)
	}
}

func TestStickerSetMethodsOnlyAllowCatalogueMutation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810003")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	known := getStickerSet(t, h, owner.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	refs := []struct {
		name       string
		ref        tg.InputStickerSetClass
		wantGetSet bool
	}{
		{name: "unknown short name", ref: &tg.InputStickerSetShortName{ShortName: "twemoji_unknown"}},
		{name: "unknown numeric set id", ref: &tg.InputStickerSetID{ID: -1_234_567_890, AccessHash: 123}},
		{name: "wrong access hash", ref: &tg.InputStickerSetID{ID: known.ID, AccessHash: known.AccessHash + 1}},
		{name: "animated emoji special set", ref: &tg.InputStickerSetAnimatedEmoji{}, wantGetSet: true},
		{name: "dice special set", ref: &tg.InputStickerSetDice{Emoticon: "🎲"}, wantGetSet: true},
		{name: "animated emoji animations special set", ref: &tg.InputStickerSetAnimatedEmojiAnimations{}, wantGetSet: true},
		{name: "premium gifts special set", ref: &tg.InputStickerSetPremiumGifts{}, wantGetSet: true},
		{name: "emoji generic animations special set", ref: &tg.InputStickerSetEmojiGenericAnimations{}, wantGetSet: true},
		{name: "emoji default statuses special set", ref: &tg.InputStickerSetEmojiDefaultStatuses{}, wantGetSet: true},
		{name: "emoji default topic icons special set", ref: &tg.InputStickerSetEmojiDefaultTopicIcons{}, wantGetSet: true},
		{name: "emoji channel default statuses special set", ref: &tg.InputStickerSetEmojiChannelDefaultStatuses{}, wantGetSet: true},
		{name: "ton gifts special set", ref: &tg.InputStickerSetTonGifts{}, wantGetSet: true},
		{name: "empty special set", ref: &tg.InputStickerSetEmpty{}, wantGetSet: true},
		{name: "oversized short name", ref: &tg.InputStickerSetShortName{ShortName: strings.Repeat("x", 256)}},
	}
	for _, tc := range refs {
		t.Run(tc.name, func(t *testing.T) {
			if got := installStickerSet(t, h, owner.ID, tc.ref); got != "STICKERSET_INVALID" {
				t.Errorf("install result = %q, want STICKERSET_INVALID", got)
			}
			uninstallBody := dispatchChatMethod(t, h, owner.ID, "messages.uninstallStickerSet", &tg.MessagesUninstallStickerSetRequest{Stickerset: tc.ref})
			if got := stickerSetRPCMessage(t, uninstallBody); got != "STICKERSET_INVALID" {
				t.Errorf("uninstall result = %q, want STICKERSET_INVALID", got)
			}
			if tc.wantGetSet {
				full := getStickerSet(t, h, owner.ID, tc.ref, 0)
				if full.Set.Hash == 0 || full.Set.Count != 0 || len(full.Packs) != 0 || len(full.Documents) != 0 {
					t.Errorf("special getStickerSet = %+v with %d packs and %d documents, want empty set with non-zero hash", full.Set, len(full.Packs), len(full.Documents))
				}
				if !getStickerSetNotModified(t, h, owner.ID, tc.ref, full.Set.Hash) {
					t.Error("getStickerSet with returned hash did not return notModified")
				}
			} else {
				getBody := dispatchChatMethod(t, h, owner.ID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: tc.ref})
				if got := stickerSetRPCMessage(t, getBody); got != "STICKERSET_INVALID" {
					t.Errorf("getStickerSet result = %q, want STICKERSET_INVALID", got)
				}
			}
		})
	}
	if got := getAllStickerSets(t, h, owner.ID, 0); len(got.Sets) != 0 {
		t.Fatalf("invalid set references installed %d sets", len(got.Sets))
	}
	if _, err := s.InstallStickerSet(ctx, owner.ID, -1_234_567_890); err != nil {
		t.Fatalf("insert stale catalogue row: %v", err)
	}
	if got := getAllStickerSets(t, h, owner.ID, 0); len(got.Sets) != 0 {
		t.Fatalf("stale catalogue row rendered %d sets", len(got.Sets))
	}
}

func TestStickerSetMethodsRequireAnAuthenticatedAccount(t *testing.T) {
	t.Parallel()
	h := fullChannelDispatcher(nil)
	ref := &tg.InputStickerSetShortName{ShortName: "missing"}
	requests := []struct {
		name string
		req  bin.Encoder
	}{
		{name: "get", req: &tg.MessagesGetStickerSetRequest{Stickerset: ref}},
		{name: "install", req: &tg.MessagesInstallStickerSetRequest{Stickerset: ref}},
		{name: "uninstall", req: &tg.MessagesUninstallStickerSetRequest{Stickerset: ref}},
	}
	for _, tc := range requests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stickerSetRPCMessage(t, dispatchChatMethod(t, h, 0, "messages."+tc.name+"StickerSet", tc.req)); got != "AUTH_KEY_UNREGISTERED" {
				t.Fatalf("unauthenticated %s = %q, want AUTH_KEY_UNREGISTERED", tc.name, got)
			}
		})
	}
}

func TestStickerSetInstallAndUninstallShareMutationRateLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810006")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	ref := &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}
	for i := range 60 {
		if got := installStickerSet(t, h, owner.ID, ref); got != "success" {
			t.Fatalf("install mutation %d = %q, want success", i+1, got)
		}
	}
	uninstallBody := dispatchChatMethod(t, h, owner.ID, "messages.uninstallStickerSet", &tg.MessagesUninstallStickerSetRequest{Stickerset: ref})
	if got := stickerSetRPCMessage(t, uninstallBody); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("61st cross-method mutation = %q, want FLOOD_WAIT", got)
	}
}

func TestFeaturedStickerSetsReflectOnlyTheViewersInstalledState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810007")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551810008")
	if err != nil {
		t.Fatalf("create other account: %v", err)
	}
	h := fullChannelDispatcher(s)
	if got := stickerSetRPCMessage(t, dispatchChatMethod(t, h, 0, "messages.getFeaturedStickers", &tg.MessagesGetFeaturedStickersRequest{Hash: 0})); got != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated getFeaturedStickers = %q, want AUTH_KEY_UNREGISTERED", got)
	}
	ownerInitial := getFeaturedStickerSets(t, h, owner.ID, 0)
	otherInitial := getFeaturedStickerSets(t, h, other.ID, 0)
	if ownerInitial.Hash == 0 || ownerInitial.Count != 1 || len(ownerInitial.Sets) != 1 {
		t.Fatalf("featured result = hash %d, count %d, sets %d, want one catalogue set", ownerInitial.Hash, ownerInitial.Count, len(ownerInitial.Sets))
	}
	covered, ok := ownerInitial.Sets[0].(*tg.StickerSetCovered)
	if !ok {
		t.Fatalf("featured set = %T, want *tg.StickerSetCovered", ownerInitial.Sets[0])
	}
	if covered.Set.ShortName != "twemoji_reactions" || covered.Set.Count != 73 {
		t.Fatalf("featured set metadata = %+v, want Twemoji catalogue", covered.Set)
	}
	if _, installed := covered.Set.GetInstalledDate(); installed {
		t.Fatal("uninstalled set exposes an installed date")
	}
	cover, ok := covered.Cover.(*tg.Document)
	if !ok || cover.ID >= 0 {
		t.Fatalf("featured cover = %T (%v), want a catalogue document", covered.Cover, covered.Cover)
	}
	if modified := getFeaturedStickerSets(t, h, owner.ID, ownerInitial.Hash); modified != nil {
		t.Fatal("getFeaturedStickers with current hash returned a full result")
	}
	if got := installStickerSet(t, h, owner.ID, &tg.InputStickerSetID{ID: covered.Set.ID, AccessHash: covered.Set.AccessHash}); got != "success" {
		t.Fatalf("install featured set = %q, want success", got)
	}
	ownerInstalled := getFeaturedStickerSets(t, h, owner.ID, 0)
	if ownerInstalled.Hash == ownerInitial.Hash {
		t.Fatalf("owner featured hash stayed %d after installation", ownerInstalled.Hash)
	}
	installedCovered, ok := ownerInstalled.Sets[0].(*tg.StickerSetCovered)
	if !ok {
		t.Fatalf("installed featured set = %T, want *tg.StickerSetCovered", ownerInstalled.Sets[0])
	}
	if date, installed := installedCovered.Set.GetInstalledDate(); !installed || date <= 0 {
		t.Fatalf("installed featured set date = %d, present %t, want positive date", date, installed)
	}
	if otherAfter := getFeaturedStickerSets(t, h, other.ID, 0); otherAfter.Hash != otherInitial.Hash {
		t.Fatalf("other account featured hash changed from %d to %d after owner installation", otherInitial.Hash, otherAfter.Hash)
	}
	if modified := getFeaturedStickerSets(t, h, owner.ID, ownerInstalled.Hash); modified != nil {
		t.Fatal("getFeaturedStickers with the installed-state hash returned a full result")
	}
}

func TestGetOldFeaturedStickerSetsSupportsPaginationAndHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810011")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)

	full := getOldFeaturedStickerSets(t, h, owner.ID, 0, 1, 0)
	if full.Hash == 0 || full.Count != 1 || len(full.Sets) != 1 {
		t.Fatalf("old featured sets = hash %d, count %d, %d sets; want the catalogue set", full.Hash, full.Count, len(full.Sets))
	}
	covered, ok := full.Sets[0].(*tg.StickerSetCovered)
	if !ok || covered.Set.ShortName != "twemoji_reactions" {
		t.Fatalf("old featured set = %T (%v), want the Twemoji catalogue set", full.Sets[0], full.Sets[0])
	}
	if got := getOldFeaturedStickerSets(t, h, owner.ID, 0, 0, 0); len(got.Sets) != 0 || got.Count != full.Count {
		t.Fatalf("zero-limit old featured sets = count %d, %d entries; want total count %d and no entries", got.Count, len(got.Sets), full.Count)
	}
	if got := getOldFeaturedStickerSets(t, h, owner.ID, 1, 1, 0); len(got.Sets) != 0 || got.Count != full.Count {
		t.Fatalf("past-end old featured sets = count %d, %d entries; want total count %d and no entries", got.Count, len(got.Sets), full.Count)
	}
	if modified := getOldFeaturedStickerSets(t, h, owner.ID, 0, 1, full.Hash); modified != nil {
		t.Fatal("getOldFeaturedStickers with current hash returned a full result")
	}
}

func TestSearchStickerSetsMatchesCatalogueCaseInsensitivelyAndSupportsHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810012")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)

	result := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "TWEMOJI"})
	full, ok := result.(*tg.MessagesFoundStickerSets)
	if !ok || full.Hash == 0 || len(full.Sets) != 1 {
		t.Fatalf("searchStickerSets = %T with hash %d and %d sets; want one catalogue match", result, foundStickerSetsHash(result), foundStickerSetsCount(result))
	}
	covered, ok := full.Sets[0].(*tg.StickerSetCovered)
	if !ok || covered.Set.ShortName != "twemoji_reactions" {
		t.Fatalf("searchStickerSets result = %T (%v), want the Twemoji catalogue set", full.Sets[0], full.Sets[0])
	}
	if modified := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "TWEMOJI", Hash: full.Hash}); modified != nil {
		t.Fatal("searchStickerSets with current hash returned a full result")
	}
}

func TestSearchStickerSetsCombinesCatalogueAndUserMatchesForHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	s := openStickerStore(t, blobs, pgtest.DSN(t))
	creator, err := s.CreateUser(ctx, "+15551810021")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	viewer, err := s.CreateUser(ctx, "+15551810022")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, creator.ID, validStickerTGS(t))
	request := createStickerRequest("generated_twemoji_cats", file, false, false)
	request.Title = "Twemoji Cats"
	created := createStickerSet(t, fullChannelDispatcher(s), creator.ID, request)
	h := fullChannelDispatcher(s)

	result := searchStickerSets(t, h, viewer.ID, &tg.MessagesSearchStickerSetsRequest{Q: "TWEMOJI"})
	full, ok := result.(*tg.MessagesFoundStickerSets)
	if !ok || full.Hash == 0 || len(full.Sets) != 2 {
		t.Fatalf("combined search = %T with hash %d and %d sets, want catalogue and user matches", result, foundStickerSetsHash(result), foundStickerSetsCount(result))
	}
	shortNames := make(map[string]bool, len(full.Sets))
	for _, result := range full.Sets {
		covered, ok := result.(*tg.StickerSetCovered)
		if !ok {
			t.Fatalf("combined search result = %T, want covered sticker set", result)
		}
		shortNames[covered.Set.ShortName] = true
	}
	for _, shortName := range []string{"twemoji_reactions", created.Set.ShortName} {
		if !shortNames[shortName] {
			t.Errorf("combined search omitted set %q", shortName)
		}
	}
	if modified := searchStickerSets(t, h, viewer.ID, &tg.MessagesSearchStickerSetsRequest{Q: "TWEMOJI", Hash: full.Hash}); modified != nil {
		t.Fatal("combined search with current hash returned a full result")
	}
}

func TestSearchStickerSetsRejectsOversizedQueryWithoutDatabaseWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tracer := &stickerSearchQueryCounter{}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551810023")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	tracer.n.Store(0)
	body := dispatchChatMethod(t, fullChannelDispatcher(s), owner.ID, "messages.searchStickerSets", &tg.MessagesSearchStickerSetsRequest{
		Q: strings.Repeat("猫", 501),
	})
	if got := stickerRPCError(t, body); got != "MESSAGE_TOO_LONG" {
		t.Fatalf("oversized sticker-set search error = %q, want MESSAGE_TOO_LONG", got)
	}
	if got := tracer.n.Load(); got != 0 {
		t.Fatalf("oversized sticker-set search ran %d database queries, want none", got)
	}
}

func TestSearchStickerSetsUsesMessagesSearchRateLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810024")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcherWithRateLimits(s, config.RateLimitsConfig{
		SearchMessages: store.RateLimitConfig{Limit: 1, Window: time.Minute},
	})
	request := &tg.MessagesSearchStickerSetsRequest{Q: "TWEMOJI"}
	if result := searchStickerSets(t, h, owner.ID, request); result == nil {
		t.Fatal("first sticker-set search returned nil")
	}
	body := dispatchChatMethod(t, h, owner.ID, "messages.searchStickerSets", request)
	if got := stickerRPCError(t, body); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("second sticker-set search error = %q, want FLOOD_WAIT", got)
	}
}

func TestSearchStickerSetsIncludesOnlyCallerVisibleUserSets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551810014")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateUser(ctx, "+15551810015")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	created := createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("generated_cats", file, false, false))

	search := func() *tg.MessagesFoundStickerSets {
		t.Helper()
		result := searchStickerSets(t, fullChannelDispatcher(s), viewer.ID, &tg.MessagesSearchStickerSetsRequest{Q: "GENERATED", ExcludeFeatured: true})
		full, ok := result.(*tg.MessagesFoundStickerSets)
		if !ok {
			t.Fatalf("searchStickerSets = %T, want full result", result)
		}
		return full
	}
	full := search()
	if full.Hash == 0 || len(full.Sets) != 1 {
		t.Fatalf("searchStickerSets user set result = hash %d, %d sets; want one caller-visible user set", full.Hash, len(full.Sets))
	}
	covered, ok := full.Sets[0].(*tg.StickerSetCovered)
	if !ok || covered.Set.ShortName != "generated_cats" || covered.Set.Creator {
		t.Fatalf("searchStickerSets result = %T (%v); want non-creator view of generated_cats", full.Sets[0], full.Sets[0])
	}
	resolved := getStickerSet(t, fullChannelDispatcher(s), viewer.ID, &tg.InputStickerSetID{ID: covered.Set.ID, AccessHash: covered.Set.AccessHash}, 0)
	if resolved.Set.ShortName != created.Set.ShortName || resolved.Set.Creator {
		t.Fatalf("getStickerSet for search result = %+v; want the viewer-visible non-creator set", resolved.Set)
	}
	if modified := searchStickerSets(t, fullChannelDispatcher(s), viewer.ID, &tg.MessagesSearchStickerSetsRequest{Q: "GENERATED", ExcludeFeatured: true, Hash: full.Hash}); modified != nil {
		t.Fatal("searchStickerSets with current user-set hash returned a full result")
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for set liveness change: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close set liveness connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `UPDATE user_created_sticker_sets SET is_live = false WHERE id = $1`, created.Set.ID); err != nil {
		t.Fatalf("disable user set: %v", err)
	}
	if inactive := search(); len(inactive.Sets) != 0 || inactive.Hash == full.Hash {
		t.Fatalf("searchStickerSets after set deactivation = hash %d, %d sets; want it excluded and hash changed", inactive.Hash, len(inactive.Sets))
	}
}

func TestSearchStickerSetsLoadsMatchingCoversInBulk(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	tracer := &stickerSearchQueryCounter{}
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15551810016")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	for _, shortName := range []string{
		"bulk_cats", "bulk_dogs", "bulkcats", "a_b", "prefix_a_b_suffix", "title_only",
	} {
		file := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
		request := createStickerRequest(shortName, file, false, false)
		if shortName == "title_only" {
			request.Title = "Contains a_b"
		}
		createStickerSet(t, h, owner.ID, request)
	}

	tracer.n.Store(0)
	result := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "BULK", ExcludeFeatured: true})
	full, ok := result.(*tg.MessagesFoundStickerSets)
	if !ok || len(full.Sets) != 3 {
		t.Fatalf("searchStickerSets = %T with %d sets; want all three bulk-created sets", result, foundStickerSetsCount(result))
	}
	for _, result := range full.Sets {
		covered, ok := result.(*tg.StickerSetCovered)
		if !ok || covered.Cover == nil {
			t.Errorf("search result = %T (%v), want a set with a cover", result, result)
		}
	}
	if got := tracer.n.Load(); got != 2 {
		t.Fatalf("searchStickerSets used %d database queries for three results, want one indexed search and one bulk resolution", got)
	}

	tracer.n.Store(0)
	literalResult := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "bulk_cats", ExcludeFeatured: true})
	literal, ok := literalResult.(*tg.MessagesFoundStickerSets)
	if !ok || len(literal.Sets) != 1 {
		t.Fatalf("literal underscore search = %T with %d sets; want only bulk_cats", literalResult, foundStickerSetsCount(literalResult))
	}
	if got := tracer.n.Load(); got != 2 {
		t.Fatalf("literal underscore search used %d database queries, want one search and one bulk resolution", got)
	}

	tracer.n.Store(0)
	shortNameResult := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "a_b", ExcludeFeatured: true})
	shortNameMatch, ok := shortNameResult.(*tg.MessagesFoundStickerSets)
	if !ok || len(shortNameMatch.Sets) != 3 {
		t.Fatalf("substring short-name search = %T with %d sets, want all three matches", shortNameResult, foundStickerSetsCount(shortNameResult))
	}
	matchedShortNames := make(map[string]bool, len(shortNameMatch.Sets))
	for _, result := range shortNameMatch.Sets {
		covered, ok := result.(*tg.StickerSetCovered)
		if !ok || covered.Cover == nil {
			t.Errorf("substring search result = %T (%v), want a covered set", result, result)
			continue
		}
		matchedShortNames[covered.Set.ShortName] = true
	}
	for _, shortName := range []string{"a_b", "prefix_a_b_suffix", "title_only"} {
		if !matchedShortNames[shortName] {
			t.Errorf("substring search omitted matching set %q", shortName)
		}
	}
	if got := tracer.n.Load(); got != 2 {
		t.Fatalf("substring search used %d database queries, want one indexed lookup and one bulk resolution", got)
	}

	for _, query := range []string{"q", "qz", "猫"} {
		tracer.n.Store(0)
		searchResult := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: query, ExcludeFeatured: true})
		found, ok := searchResult.(*tg.MessagesFoundStickerSets)
		if !ok || len(found.Sets) != 0 {
			t.Errorf("search for %q = %T with %d sets, want an empty result", query, searchResult, foundStickerSetsCount(searchResult))
		}
		if got := tracer.n.Load(); got != 0 {
			t.Errorf("search for %q used %d database queries, want none", query, got)
		}
	}

	tracer.n.Store(0)
	searchResult := searchStickerSets(t, h, owner.ID, &tg.MessagesSearchStickerSetsRequest{Q: "!!!", ExcludeFeatured: true})
	found, ok := searchResult.(*tg.MessagesFoundStickerSets)
	if !ok || len(found.Sets) != 0 {
		t.Errorf("search for %q = %T with %d sets, want an empty result", "!!!", searchResult, foundStickerSetsCount(searchResult))
	}
	if got := tracer.n.Load(); got != 1 {
		t.Errorf("search for %q used %d database queries, want one indexed candidate lookup", "!!!", got)
	}
}

type stickerSearchQueryCounter struct{ n atomic.Int64 }

func (c *stickerSearchQueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (*stickerSearchQueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestSearchStickerSetsReturnsEmptyForUnmatchedAndEmptyQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551810013")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)

	for _, req := range []*tg.MessagesSearchStickerSetsRequest{
		{Q: "no matching catalogue set"},
		{Q: "twemoji", ExcludeFeatured: true},
		{Q: "", Hash: 123},
	} {
		result := searchStickerSets(t, h, owner.ID, req)
		full, ok := result.(*tg.MessagesFoundStickerSets)
		if !ok || len(full.Sets) != 0 {
			t.Errorf("searchStickerSets(%q, excludeFeatured=%t, hash=%d) = %T with %d sets; want an empty full result", req.Q, req.ExcludeFeatured, req.Hash, result, foundStickerSetsCount(result))
		}
	}
}

func getFeaturedStickerSets(t *testing.T, h mtproto.Handler, userID int64, hash int64) *tg.MessagesFeaturedStickers {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getFeaturedStickers", &tg.MessagesGetFeaturedStickersRequest{Hash: hash})
	var box tg.MessagesFeaturedStickersBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getFeaturedStickers: %v", err)
	}
	full, ok := box.FeaturedStickers.(*tg.MessagesFeaturedStickers)
	if !ok {
		if _, notModified := box.FeaturedStickers.(*tg.MessagesFeaturedStickersNotModified); notModified {
			return nil
		}
		t.Fatalf("getFeaturedStickers = %T, want full or notModified", box.FeaturedStickers)
	}
	return full
}

func getOldFeaturedStickerSets(t *testing.T, h mtproto.Handler, userID int64, offset, limit int, hash int64) *tg.MessagesFeaturedStickers {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getOldFeaturedStickers", &tg.MessagesGetOldFeaturedStickersRequest{Offset: offset, Limit: limit, Hash: hash})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("getOldFeaturedStickers returned RPC error %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	var box tg.MessagesFeaturedStickersBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getOldFeaturedStickers: %v", err)
	}
	if _, ok := box.FeaturedStickers.(*tg.MessagesFeaturedStickersNotModified); ok {
		return nil
	}
	full, ok := box.FeaturedStickers.(*tg.MessagesFeaturedStickers)
	if !ok {
		t.Fatalf("getOldFeaturedStickers = %T, want full or notModified", box.FeaturedStickers)
	}
	return full
}

func searchStickerSets(t *testing.T, h mtproto.Handler, userID int64, req *tg.MessagesSearchStickerSetsRequest) tg.MessagesFoundStickerSetsClass {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.searchStickerSets", req)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("searchStickerSets returned RPC error %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	var box tg.MessagesFoundStickerSetsBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode searchStickerSets: %v", err)
	}
	if _, ok := box.FoundStickerSets.(*tg.MessagesFoundStickerSetsNotModified); ok {
		return nil
	}
	full, ok := box.FoundStickerSets.(*tg.MessagesFoundStickerSets)
	if !ok {
		t.Fatalf("searchStickerSets = %T, want full or notModified", box.FoundStickerSets)
	}
	return full
}

func foundStickerSetsCount(result tg.MessagesFoundStickerSetsClass) int {
	if full, ok := result.(*tg.MessagesFoundStickerSets); ok && full != nil {
		return len(full.Sets)
	}
	return 0
}

func foundStickerSetsHash(result tg.MessagesFoundStickerSetsClass) int64 {
	if full, ok := result.(*tg.MessagesFoundStickerSets); ok && full != nil {
		return full.Hash
	}
	return 0
}

func TestBuiltInStickerSendRendersToRecipientWithoutFileEntitlement(t *testing.T) {
	t.Parallel()
	s, blobs, caller, receiver, userDocument := downloadFixture(t, "+15551810004", "+15551810005")
	set := getStickerSet(t, fullChannelDispatcher(s), caller.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	full := getStickerSet(t, fullChannelDispatcher(s), caller.ID, &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}, 0)
	document, ok := full.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("first sticker document = %T, want *tg.Document", full.Documents[0])
	}
	available, ok := availableReactionsResponse(t, s, caller.ID, 0).(*tg.MessagesAvailableReactions)
	if !ok || len(available.Reactions) == 0 {
		t.Fatalf("available reactions = %#v, want a non-empty reaction catalogue", available)
	}
	reactionDocument, ok := available.Reactions[0].StaticIcon.(*tg.Document)
	if !ok {
		t.Fatalf("first reaction static icon = %T, want *tg.Document", available.Reactions[0].StaticIcon)
	}

	request := func(id, accessHash int64) error {
		_, err := api.GetFileForTest(s, caller.ID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: id, AccessHash: accessHash, FileReference: document.FileReference},
			Limit:    1024,
		})
		return err
	}
	for _, tc := range []struct {
		name       string
		id, access int64
	}{
		{name: "wrong catalogue hash", id: document.ID, access: document.AccessHash + 1},
		{name: "unknown negative id", id: document.ID - 10_000, access: document.AccessHash},
		{name: "positive files id with catalogue hash", id: userDocument.ID, access: document.AccessHash},
	} {
		if got := rpcMessage(t, request(tc.id, tc.access)); got != "LOCATION_INVALID" {
			t.Errorf("%s download = %q, want LOCATION_INVALID", tc.name, got)
		}
	}
	for _, tc := range []struct {
		name       string
		id, access int64
	}{
		{name: "wrong catalogue hash", id: document.ID, access: document.AccessHash + 1},
		{name: "unknown catalogue id", id: document.ID - 10_000, access: document.AccessHash},
		{name: "positive files id", id: userDocument.ID, access: userDocument.AccessHash},
		{name: "reaction document", id: reactionDocument.ID, access: reactionDocument.AccessHash},
	} {
		_, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer: api.InputPeerUser(caller.ID, receiver.ID),
			Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
				ID: tc.id, AccessHash: tc.access, FileReference: []byte("ignored file reference"),
			}},
			RandomID: 4815162000 + tc.id,
		})
		if got := rpcMessage(t, err); got != "MEDIA_INVALID" {
			t.Errorf("%s send = %q, want MEDIA_INVALID", tc.name, got)
		}
	}
	for i, caption := range []string{string([]byte{0xff}), "bad\x00caption"} {
		_, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer: api.InputPeerUser(caller.ID, receiver.ID),
			Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
				ID: document.ID, AccessHash: document.AccessHash,
			}},
			Message: caption, RandomID: 4815162050 + int64(i),
		})
		if got := rpcMessage(t, err); got != "MESSAGE_EMPTY" {
			t.Errorf("sticker caption %d error = %q, want MESSAGE_EMPTY", i, got)
		}
	}
	if _, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		RandomID: 4815162052,
	}); err != nil {
		t.Fatalf("empty sticker caption: %v", err)
	}
	for i, tc := range []struct {
		name   string
		mutate func(*tg.InputMediaDocument)
	}{
		{name: "spoiler", mutate: func(media *tg.InputMediaDocument) { media.SetSpoiler(true) }},
		{name: "self destruct timer", mutate: func(media *tg.InputMediaDocument) { media.SetTTLSeconds(1) }},
		{name: "search query", mutate: func(media *tg.InputMediaDocument) { media.SetQuery("🙂") }},
		{name: "video cover", mutate: func(media *tg.InputMediaDocument) { media.SetVideoCover(&tg.InputPhotoEmpty{}) }},
		{name: "video timestamp", mutate: func(media *tg.InputMediaDocument) { media.SetVideoTimestamp(1) }},
	} {
		media := &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}}
		tc.mutate(media)
		_, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer: api.InputPeerUser(caller.ID, receiver.ID), Media: media,
			Message: "catalogue sticker", RandomID: 4815162100 + int64(i),
		})
		if got := rpcMessage(t, err); got != "MEDIA_INVALID" {
			t.Errorf("%s sticker send = %q, want MEDIA_INVALID", tc.name, got)
		}
	}
	sent, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash, FileReference: []byte("ignored file reference"),
		}},
		Message:  "catalogue sticker",
		RandomID: 4815162342,
	})
	if err != nil {
		t.Fatalf("messages.sendMedia with catalogue document: %v", err)
	}
	senderDoc := documentOf(t, sent)
	if senderDoc.ID != document.ID || senderDoc.AccessHash != document.AccessHash {
		t.Fatalf("sender message document = (%d, %d), want catalogue document (%d, %d)", senderDoc.ID, senderDoc.AccessHash, document.ID, document.AccessHash)
	}
	if !reflect.DeepEqual(senderDoc.Attributes, document.Attributes) {
		t.Fatalf("sender document attributes = %#v, want sticker-set attributes %#v", senderDoc.Attributes, document.Attributes)
	}
	if !hasStickerAttribute(senderDoc) {
		t.Fatal("sender document has no DocumentAttributeSticker")
	}
	_, err = api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash + 1,
		}},
		Message: "catalogue sticker", RandomID: 4815162342,
	})
	if got := rpcMessage(t, err); got != "MEDIA_INVALID" {
		t.Fatalf("same-random-id retry with wrong catalogue hash = %q, want MEDIA_INVALID", got)
	}
	sender := messageOf(t, sent)
	stored, ok, err := s.MessageByRandomID(context.Background(), caller.ID, 4815162342)
	if err != nil || !ok {
		t.Fatalf("stored sender message = %+v, found %t, err %v", stored, ok, err)
	}
	if stored.FileID != 0 {
		t.Fatalf("catalogue sticker message file_id = %d, want 0", stored.FileID)
	}
	received, err := api.GetHistoryForTest(s, receiver.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(receiver.ID, caller.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient getHistory: %v", err)
	}
	history, ok := received.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("recipient history = %T, want *tg.MessagesMessages", received)
	}
	var receivedMessage *tg.Message
	for _, item := range history.Messages {
		candidate, ok := item.(*tg.Message)
		if ok && candidate.Message == "catalogue sticker" {
			if receivedMessage != nil {
				t.Fatal("recipient history contains duplicate catalogue sticker messages")
			}
			receivedMessage = candidate
		}
	}
	if receivedMessage == nil {
		t.Fatalf("recipient history has %d messages but no catalogue sticker", len(history.Messages))
	}
	receivedMedia, ok := receivedMessage.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("recipient message media = %T, want *tg.MessageMediaDocument", receivedMessage.Media)
	}
	receivedDoc, ok := receivedMedia.Document.(*tg.Document)
	if !ok || receivedDoc.ID != document.ID || receivedDoc.AccessHash != document.AccessHash {
		t.Fatalf("recipient message document = %#v, want catalogue document (%d, %d)", receivedMedia.Document, document.ID, document.AccessHash)
	}
	if !reflect.DeepEqual(receivedDoc.Attributes, document.Attributes) {
		t.Fatalf("recipient document attributes = %#v, want sticker-set attributes %#v", receivedDoc.Attributes, document.Attributes)
	}
	if !hasStickerAttribute(receivedDoc) {
		t.Fatal("recipient document has no DocumentAttributeSticker")
	}
	if _, err := s.FileForDownload(context.Background(), document.ID, document.AccessHash, receiver.ID); err == nil {
		t.Fatal("catalogue sticker unexpectedly acquired a files-table entitlement")
	}
	got, err := api.GetFileForTest(s, receiver.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: receivedDoc.ID, AccessHash: receivedDoc.AccessHash, FileReference: receivedDoc.FileReference},
		Limit:    int(receivedDoc.Size),
	})
	if err != nil {
		t.Fatalf("recipient download from received message: %v", err)
	}
	file, ok := got.(*tg.UploadFile)
	if !ok {
		t.Fatalf("recipient getFile result = %T, want *tg.UploadFile", got)
	}
	entries, err := reactionassets.Manifest()
	if err != nil {
		t.Fatalf("read sticker manifest: %v", err)
	}
	stem := strings.TrimSuffix(entries[0].SVG, ".svg")
	want, err := reactionassets.Files.ReadFile("static/" + stem + ".webp")
	if err != nil {
		t.Fatalf("read committed sticker WebP: %v", err)
	}
	if !bytes.Equal(file.Bytes, want) {
		t.Fatal("recipient downloaded bytes differ from the committed sticker WebP")
	}
	if sender.ID <= 0 {
		t.Fatalf("sender message id = %d, want positive", sender.ID)
	}

	limited := api.GetFileSeqForTestWithLimits(
		s, blobs,
		store.RateLimitConfig{Limit: 1, Window: time.Minute},
		store.RateLimitConfig{},
	)
	limitedRequest := &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
		Limit:    1024,
	}
	if _, err := limited(caller.ID, limitedRequest); err != nil {
		t.Fatalf("first catalogue download: %v", err)
	}
	if got := rpcMessage(t, func() error {
		_, err := limited(caller.ID, limitedRequest)
		return err
	}()); !strings.HasPrefix(got, "FLOOD_WAIT_") {
		t.Fatalf("second catalogue download = %q, want rate limit", got)
	}
}

func hasStickerAttribute(document *tg.Document) bool {
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeSticker); ok {
			return true
		}
	}
	return false
}

func TestCatalogueStickerUsesSendStickersRightInBasicGroupsAndForwards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, creator, member, _ := downloadFixture(t, "+15551810009", "+15551810010")
	set := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	full := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}, 0)
	if len(full.Documents) == 0 {
		t.Fatal("getStickerSet returned no documents")
	}
	document, ok := full.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("first sticker document = %T, want *tg.Document", full.Documents[0])
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Catalogue sticker rights", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, err = api.SendMediaForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(creator.ID, member.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		Message: "forwardable sticker", RandomID: 4815162401,
	}); err != nil {
		t.Fatalf("send private sticker source: %v", err)
	}
	historyResult, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(member.ID, creator.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("member source history: %v", err)
	}
	history, ok := historyResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("member source history = %T, want *tg.MessagesMessages", historyResult)
	}
	var sourceLocalID int
	for _, item := range history.Messages {
		message, ok := item.(*tg.Message)
		if ok && message.Message == "forwardable sticker" {
			sourceLocalID = message.ID
		}
	}
	if sourceLocalID == 0 {
		t.Fatal("member history omitted the private sticker source")
	}
	groupSend, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		Message: "group sticker", RandomID: 4815162402,
	})
	if err != nil {
		t.Fatalf("send catalogue sticker to basic group: %v", err)
	}
	if got := documentOf(t, groupSend); got.ID != document.ID {
		t.Fatalf("basic group sticker document id = %d, want %d", got.ID, document.ID)
	}
	groupSourceLocalID := messageOf(t, groupSend).ID
	groupStored, found, err := s.MessageByRandomID(ctx, member.ID, 4815162402)
	if err != nil || !found {
		t.Fatalf("read group sticker: found=%t err=%v", found, err)
	}
	if groupStored.FileID != 0 || groupStored.StickerDocumentID == nil || *groupStored.StickerDocumentID != document.ID {
		t.Fatalf("group sticker identity file_id=%d sticker_document_id=%v", groupStored.FileID, groupStored.StickerDocumentID)
	}
	if _, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(member.ID, creator.ID), ID: []int{sourceLocalID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{4815162403},
	}); err != nil {
		t.Fatalf("forward catalogue sticker to basic group: %v", err)
	}
	forwarded, found, err := s.MessageByRandomID(ctx, member.ID, 4815162403)
	if err != nil || !found || forwarded.StickerDocumentID == nil || *forwarded.StickerDocumentID != document.ID || forwarded.FileID != 0 {
		t.Fatalf("forwarded group sticker = %+v found=%t err=%v", forwarded, found, err)
	}
	if _, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChat(member.ID, chat.ID), ID: []int{groupSourceLocalID},
		ToPeer: api.InputPeerUser(member.ID, creator.ID), RandomID: []int64{4815162409},
	}); err != nil {
		t.Fatalf("forward basic group catalogue sticker to a user: %v", err)
	}
	chatForwarded, found, err := s.MessageByRandomID(ctx, member.ID, 4815162409)
	if err != nil || !found || chatForwarded.StickerDocumentID == nil || *chatForwarded.StickerDocumentID != document.ID || chatForwarded.FileID != 0 {
		t.Fatalf("forwarded chat sticker = %+v found=%t err=%v", chatForwarded, found, err)
	}
	sourceChannel, err := s.CreateChannel(ctx, creator.ID, "Catalogue sticker forward source", "", true)
	if err != nil {
		t.Fatalf("create forward source megagroup: %v", err)
	}
	joinChannelByInvite(t, s, sourceChannel, member.ID)
	channelSend, err := api.SendMediaForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: channelPeer(creator.ID, sourceChannel.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		Message: "channel forwardable sticker", RandomID: 4815162406,
	})
	if err != nil {
		t.Fatalf("send catalogue sticker source to channel: %v", err)
	}
	var channelSourceLocalID int
	updates, ok := channelSend.(*tg.Updates)
	if !ok {
		t.Fatalf("channel send result = %T, want *tg.Updates", channelSend)
	}
	for _, update := range updates.Updates {
		posted, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		message, ok := posted.Message.(*tg.Message)
		if ok && message.Message == "channel forwardable sticker" {
			channelSourceLocalID = message.ID
		}
	}
	if channelSourceLocalID == 0 {
		t.Fatal("channel send omitted the sticker update")
	}
	if _, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: channelPeer(member.ID, sourceChannel.ID), ID: []int{channelSourceLocalID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{4815162407},
	}); err != nil {
		t.Fatalf("forward channel catalogue sticker to basic group: %v", err)
	}
	channelForwarded, found, err := s.MessageByRandomID(ctx, member.ID, 4815162407)
	if err != nil || !found || channelForwarded.StickerDocumentID == nil || *channelForwarded.StickerDocumentID != document.ID || channelForwarded.FileID != 0 {
		t.Fatalf("forwarded channel sticker = %+v found=%t err=%v", channelForwarded, found, err)
	}
	if _, _, err = s.SetChatDefaultBannedRights(ctx, chat.ID, creator.ID, []string{"send_stickers"}); err != nil {
		t.Fatalf("restrict basic group stickers: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerChat(member.ID, chat.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "blocked group sticker", RandomID: 4815162404,
	})
	if got := rpcMessage(t, err); got != "CHAT_WRITE_FORBIDDEN" {
		t.Fatalf("restricted basic group sticker send = %q, want CHAT_WRITE_FORBIDDEN", got)
	}
	_, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(member.ID, creator.ID), ID: []int{sourceLocalID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{4815162405},
	})
	if got := rpcMessage(t, err); got != "CHAT_WRITE_FORBIDDEN" {
		t.Fatalf("restricted basic group sticker forward = %q, want CHAT_WRITE_FORBIDDEN", got)
	}
	_, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: channelPeer(member.ID, sourceChannel.ID), ID: []int{channelSourceLocalID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{4815162408},
	})
	if got := rpcMessage(t, err); got != "CHAT_WRITE_FORBIDDEN" {
		t.Fatalf("restricted channel sticker forward = %q, want CHAT_WRITE_FORBIDDEN", got)
	}
}

func TestCatalogueStickerRespectsMegagroupAndBroadcastPostRights(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, creator, member, _ := downloadFixture(t, "+15551810011", "+15551810012")
	set := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	full := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}, 0)
	if len(full.Documents) == 0 {
		t.Fatal("getStickerSet returned no documents")
	}
	document, ok := full.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("first sticker document = %T, want *tg.Document", full.Documents[0])
	}
	megagroup, err := s.CreateChannel(ctx, creator.ID, "Catalogue sticker megagroup", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, megagroup, member.ID)
	if _, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(member.ID, megagroup.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "megagroup sticker", RandomID: 4815162411,
	}); err != nil {
		t.Fatalf("member send catalogue sticker to megagroup: %v", err)
	}
	historyResult, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: channelPeer(member.ID, megagroup.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("megagroup sticker history: %v", err)
	}
	var channelMessages []tg.MessageClass
	switch history := historyResult.(type) {
	case *tg.MessagesChannelMessages:
		channelMessages = history.Messages
	case *tg.MessagesMessages:
		channelMessages = history.Messages
	default:
		t.Fatalf("megagroup sticker history = %T, want channel messages", historyResult)
	}
	var channelStickerFound bool
	for _, item := range channelMessages {
		message, ok := item.(*tg.Message)
		if !ok || message.Message != "megagroup sticker" {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("megagroup sticker history media = %T, want *tg.MessageMediaDocument", message.Media)
		}
		receivedDocument, ok := media.Document.(*tg.Document)
		if !ok || receivedDocument.ID != document.ID || receivedDocument.AccessHash != document.AccessHash {
			t.Fatalf("megagroup sticker history document = %T, want catalogue document", media.Document)
		}
		channelStickerFound = true
	}
	if !channelStickerFound {
		t.Fatal("megagroup history omitted catalogue sticker")
	}
	if _, _, err = s.SetChannelDefaultBannedRights(ctx, megagroup.ID, creator.ID, []string{"send_stickers"}); err != nil {
		t.Fatalf("restrict megagroup stickers: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(member.ID, megagroup.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "blocked megagroup sticker", RandomID: 4815162412,
	})
	if got := rpcMessage(t, err); got != "CHAT_WRITE_FORBIDDEN" {
		t.Fatalf("restricted megagroup sticker send = %q, want CHAT_WRITE_FORBIDDEN", got)
	}
	if _, err = api.SendMediaForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(creator.ID, megagroup.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "admin megagroup sticker", RandomID: 4815162413,
	}); err != nil {
		t.Fatalf("admin send catalogue sticker to restricted megagroup: %v", err)
	}

	broadcast, err := s.CreateChannel(ctx, creator.ID, "Catalogue sticker broadcast", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, broadcast, member.ID)
	_, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(member.ID, broadcast.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "blocked broadcast sticker", RandomID: 4815162414,
	})
	if got := rpcMessage(t, err); got != "PEER_ID_INVALID" {
		t.Fatalf("non-admin broadcast sticker send = %q, want PEER_ID_INVALID", got)
	}
	if _, err = api.SendMediaForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(creator.ID, broadcast.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "admin broadcast sticker", RandomID: 4815162415,
	}); err != nil {
		t.Fatalf("admin send catalogue sticker to broadcast: %v", err)
	}
}

func TestCatalogueStickerSendAndRetryRejectBannedMegagroupMember(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, creator, member, _ := downloadFixture(t, "+15551810015", "+15551810016")
	set := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	full := getStickerSet(t, fullChannelDispatcher(s), creator.ID, &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}, 0)
	if len(full.Documents) == 0 {
		t.Fatal("getStickerSet returned no documents")
	}
	document, ok := full.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("first sticker document = %T, want *tg.Document", full.Documents[0])
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Catalogue sticker banned member", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	req := &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(member.ID, channel.ID),
		Media:   &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}},
		Message: "member catalogue sticker", RandomID: 4815162426,
	}
	if _, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req); err != nil {
		t.Fatalf("member send catalogue sticker before ban: %v", err)
	}
	if err = s.SetChannelBan(ctx, channel.ID, creator.ID, member.ID, nil, true); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	newSend := *req
	newSend.RandomID++
	if _, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &newSend); err == nil {
		t.Fatal("banned member sent a catalogue sticker, want PEER_ID_INVALID")
	} else {
		rpcError(t, err, "PEER_ID_INVALID")
	}
	if _, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req); err == nil {
		t.Fatal("banned member retried a catalogue sticker, want PEER_ID_INVALID")
	} else {
		rpcError(t, err, "PEER_ID_INVALID")
	}
}

func TestStaleCatalogueStickerRetriesAndHistoryRenderUnsupported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, blobs, caller, receiver, _ := downloadFixture(t, "+15551810013", "+15551810014")
	const staleID = int64(-799999999)
	const randomID = int64(4815162421)
	if _, _, _, _, err := s.SendStickerMessageWithEntities(ctx, caller.ID, receiver.ID, "stale catalogue sticker", randomID, staleID, 0, nil); err != nil {
		t.Fatalf("seed stale catalogue sticker: %v", err)
	}
	retried, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: staleID, AccessHash: 991,
		}},
		Message: "stale catalogue sticker", RandomID: randomID,
	})
	if err != nil {
		t.Fatalf("retry stale catalogue sticker: %v", err)
	}
	if _, ok := messageOf(t, retried).Media.(*tg.MessageMediaUnsupported); !ok {
		t.Fatalf("stale sticker retry media = %T, want *tg.MessageMediaUnsupported", messageOf(t, retried).Media)
	}
	historyResult, err := api.GetHistoryForTest(s, receiver.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(receiver.ID, caller.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient stale sticker history: %v", err)
	}
	history, ok := historyResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("recipient history = %T, want *tg.MessagesMessages", historyResult)
	}
	found := false
	for _, item := range history.Messages {
		message, ok := item.(*tg.Message)
		if !ok || message.Message != "stale catalogue sticker" {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaUnsupported); !ok {
			t.Fatalf("stale sticker history media = %T, want *tg.MessageMediaUnsupported", message.Media)
		}
		found = true
	}
	if !found {
		t.Fatal("recipient history omitted stale catalogue sticker")
	}
	difference, err := api.GetDifferenceForTest(s, receiver.ID, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
	if err != nil {
		t.Fatalf("recipient stale sticker difference: %v", err)
	}
	full, ok := difference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("recipient difference = %T, want *tg.UpdatesDifference", difference)
	}
	found = false
	for _, item := range full.NewMessages {
		message, ok := item.(*tg.Message)
		if !ok || message.Message != "stale catalogue sticker" {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaUnsupported); !ok {
			t.Fatalf("stale sticker difference media = %T, want *tg.MessageMediaUnsupported", message.Media)
		}
		found = true
	}
	if !found {
		t.Fatal("recipient difference omitted stale catalogue sticker")
	}
}

func getAllStickerSetResult(t *testing.T, h mtproto.Handler, userID int64, hash int64) *tg.MessagesAllStickers {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getAllStickers", &tg.MessagesGetAllStickersRequest{Hash: hash})
	var box tg.MessagesAllStickersBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		var notModified tg.MessagesAllStickersNotModified
		if notModified.Decode(&bin.Buffer{Buf: body}) == nil {
			return nil
		}
		t.Fatalf("decode getAllStickers: %v", err)
	}
	full, ok := box.AllStickers.(*tg.MessagesAllStickers)
	if !ok {
		var notModified tg.MessagesAllStickersNotModified
		if notModified.Decode(&bin.Buffer{Buf: body}) == nil {
			return nil
		}
		t.Fatalf("getAllStickers = %T, want full or notModified", box.AllStickers)
	}
	return full
}

func getAllStickerSets(t *testing.T, h mtproto.Handler, userID int64, hash int64) *tg.MessagesAllStickers {
	t.Helper()
	result := getAllStickerSetResult(t, h, userID, hash)
	if result == nil {
		t.Fatal("getAllStickers unexpectedly returned notModified")
	}
	return result
}

func getStickerSet(t *testing.T, h mtproto.Handler, userID int64, ref tg.InputStickerSetClass, hash int) *tg.MessagesStickerSet {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: ref, Hash: hash})
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		t.Fatalf("getStickerSet RPC error = %q", rpc.ErrorMessage)
	}
	var box tg.MessagesStickerSetBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getStickerSet: %v", err)
	}
	full, ok := box.StickerSet.(*tg.MessagesStickerSet)
	if !ok {
		t.Fatalf("getStickerSet = %T, want full response", box.StickerSet)
	}
	return full
}

func getStickerSetNotModified(t *testing.T, h mtproto.Handler, userID int64, ref tg.InputStickerSetClass, hash int) bool {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: ref, Hash: hash})
	var box tg.MessagesStickerSetBox
	if err := box.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getStickerSet: %v", err)
	}
	_, ok := box.StickerSet.(*tg.MessagesStickerSetNotModified)
	return ok
}

func installStickerSet(t *testing.T, h mtproto.Handler, userID int64, ref tg.InputStickerSetClass) string {
	t.Helper()
	return installStickerSetWithArchive(t, h, userID, ref, false)
}

func installStickerSetWithArchive(t *testing.T, h mtproto.Handler, userID int64, ref tg.InputStickerSetClass, archived bool) string {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.installStickerSet", &tg.MessagesInstallStickerSetRequest{Stickerset: ref, Archived: archived})
	var success tg.MessagesStickerSetInstallResultSuccess
	if success.Decode(&bin.Buffer{Buf: body}) == nil {
		return "success"
	}
	var rpc mt.RPCError
	if rpc.Decode(&bin.Buffer{Buf: body}) == nil {
		return rpc.ErrorMessage
	}
	t.Fatalf("decode installStickerSet result: %x", body)
	return ""
}

func stickerSetRPCMessage(t *testing.T, body []byte) string {
	t.Helper()
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode RPC error: %v", err)
	}
	return rpc.ErrorMessage
}

func uninstallStickerSet(t *testing.T, h mtproto.Handler, userID int64, ref tg.InputStickerSetClass) bool {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.uninstallStickerSet", &tg.MessagesUninstallStickerSetRequest{Stickerset: ref})
	var result tg.BoolTrue
	if err := result.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode uninstallStickerSet: %v", err)
	}
	return true
}
