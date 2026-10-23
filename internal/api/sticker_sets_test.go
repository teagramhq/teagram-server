package api_test

import (
	"bytes"
	"context"
	"path"
	"strings"
	"testing"
	"time"

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

func TestBuiltInStickerSendRendersToRecipientWithoutFileEntitlement(t *testing.T) {
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
	for _, tc := range []struct {
		name       string
		id, access int64
	}{
		{name: "wrong catalogue hash", id: document.ID, access: document.AccessHash + 1},
		{name: "unknown catalogue id", id: document.ID - 10_000, access: document.AccessHash},
		{name: "positive files id", id: userDocument.ID, access: userDocument.AccessHash},
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
