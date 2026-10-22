package api_test

import (
	"bytes"
	"context"
	"path"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/reactionassets"
	"github.com/teagramhq/teagram-server/internal/store"
)

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
	removed := getAllStickerSetResult(t, h, owner.ID, ownerSets.Hash)
	if removed == nil || len(removed.Sets) != 0 || removed.Hash == ownerSets.Hash {
		t.Fatalf("getAllStickers after uninstall = %+v, want changed hash and empty sets", removed)
	}
}

func TestStickerSetMethodsRejectEveryNonCatalogueReferenceUniformly(t *testing.T) {
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
		name string
		ref  tg.InputStickerSetClass
	}{
		{name: "unknown short name", ref: &tg.InputStickerSetShortName{ShortName: "twemoji_unknown"}},
		{name: "unknown numeric set id", ref: &tg.InputStickerSetID{ID: -1_234_567_890, AccessHash: 123}},
		{name: "wrong access hash", ref: &tg.InputStickerSetID{ID: known.ID, AccessHash: known.AccessHash + 1}},
		{name: "animated emoji special set", ref: &tg.InputStickerSetAnimatedEmoji{}},
		{name: "dice special set", ref: &tg.InputStickerSetDice{Emoticon: "🎲"}},
		{name: "animated emoji animations special set", ref: &tg.InputStickerSetAnimatedEmojiAnimations{}},
		{name: "premium gifts special set", ref: &tg.InputStickerSetPremiumGifts{}},
		{name: "emoji generic animations special set", ref: &tg.InputStickerSetEmojiGenericAnimations{}},
		{name: "emoji default statuses special set", ref: &tg.InputStickerSetEmojiDefaultStatuses{}},
		{name: "emoji default topic icons special set", ref: &tg.InputStickerSetEmojiDefaultTopicIcons{}},
		{name: "emoji channel default statuses special set", ref: &tg.InputStickerSetEmojiChannelDefaultStatuses{}},
		{name: "ton gifts special set", ref: &tg.InputStickerSetTonGifts{}},
		{name: "empty special set", ref: &tg.InputStickerSetEmpty{}},
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
			getBody := dispatchChatMethod(t, h, owner.ID, "messages.getStickerSet", &tg.MessagesGetStickerSetRequest{Stickerset: tc.ref})
			if got := stickerSetRPCMessage(t, getBody); got != "STICKERSET_INVALID" {
				t.Errorf("getStickerSet result = %q, want STICKERSET_INVALID", got)
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

func TestBuiltInStickerDownloadCannotReachFilesOrSendMedia(t *testing.T) {
	t.Parallel()
	s, blobs, caller, receiver, userDocument := downloadFixture(t, "+15551810004", "+15551810005")
	set := getStickerSet(t, fullChannelDispatcher(s), caller.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}, 0).Set
	full := getStickerSet(t, fullChannelDispatcher(s), caller.ID, &tg.InputStickerSetID{ID: set.ID, AccessHash: set.AccessHash}, 0)
	document, ok := full.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("first sticker document = %T, want *tg.Document", full.Documents[0])
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
	_, err := api.SendMediaForTest(s, caller.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(caller.ID, receiver.ID),
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference,
		}},
		Message:  "catalogue sticker replay",
		RandomID: 4815162342,
	})
	if got := rpcMessage(t, err); got != "MEDIA_INVALID" {
		t.Fatalf("messages.sendMedia with catalogue document = %q, want MEDIA_INVALID", got)
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
