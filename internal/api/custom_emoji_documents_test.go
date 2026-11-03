package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestGetCustomEmojiDocumentsReturnsMessageReferencedHistoricalSetMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551819901")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	requester, err := s.CreateUser(ctx, "+15551819902")
	if err != nil {
		t.Fatalf("create requester: %v", err)
	}
	stranger, err := s.CreateUser(ctx, "+15551819904")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	h := fullChannelDispatcher(s)

	emojiFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	emojiRequest := createStickerRequest("custom_emoji_docs", emojiFile, false, true)
	emojiRequest.TextColor = true
	emojiSet := createStickerSet(t, h, owner.ID, emojiRequest)
	emojiDocument, ok := emojiSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("custom emoji set document = %T, want *tg.Document", emojiSet.Documents[0])
	}

	inactiveFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	inactiveSet := createStickerSet(t, h, owner.ID, createStickerRequest("inactive_emoji_docs", inactiveFile, false, true))
	inactiveDocument, ok := inactiveSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("inactive custom emoji set document = %T, want *tg.Document", inactiveSet.Documents[0])
	}
	removedFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	unreferencedRemovedSet := createStickerSet(t, h, owner.ID, createStickerRequest("unreferenced_removed_emoji_docs", removedFile, false, true))
	unreferencedRemovedDocument, ok := unreferencedRemovedSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("unreferenced removed custom emoji set document = %T, want *tg.Document", unreferencedRemovedSet.Documents[0])
	}

	referencedRemovedFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	keptFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	referencedRemovedRequest := createStickerRequest("referenced_removed_emoji_docs", referencedRemovedFile, false, true)
	referencedRemovedRequest.Stickers = append(referencedRemovedRequest.Stickers, tg.InputStickerSetItem{
		Document: &tg.InputDocument{ID: keptFile.ID, AccessHash: keptFile.AccessHash}, Emoji: "✨",
	})
	referencedRemovedSet := createStickerSet(t, h, owner.ID, referencedRemovedRequest)
	referencedRemovedDocument, ok := referencedRemovedSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("referenced removed custom emoji document = %T, want *tg.Document", referencedRemovedSet.Documents[0])
	}

	deletedFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	deletedSet := createStickerSet(t, h, owner.ID, createStickerRequest("deleted_emoji_docs", deletedFile, false, true))
	deletedDocument, ok := deletedSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("deleted custom emoji set document = %T, want *tg.Document", deletedSet.Documents[0])
	}
	if _, err := api.SendMessageForTest(s, owner.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(owner.ID, requester.ID), Message: "⭐⭐", RandomID: 91904,
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityCustomEmoji{Offset: 0, Length: 1, DocumentID: referencedRemovedDocument.ID},
			&tg.MessageEntityCustomEmoji{Offset: 1, Length: 1, DocumentID: deletedDocument.ID},
		},
	}); err != nil {
		t.Fatalf("send custom emoji references: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to sticker test database: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE user_created_sticker_sets SET is_live = false WHERE id = $1", inactiveSet.Set.ID); err != nil {
		closeErr := conn.Close(ctx)
		t.Fatalf("mark custom emoji set inactive: %v", errors.Join(err, closeErr))
	}
	if _, err := conn.Exec(ctx, "UPDATE user_created_sticker_set_items SET is_active = false WHERE set_id = $1", unreferencedRemovedSet.Set.ID); err != nil {
		closeErr := conn.Close(ctx)
		t.Fatalf("mark unreferenced custom emoji item removed: %v", errors.Join(err, closeErr))
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close sticker test database connection: %v", err)
	}
	if got := stickerSetMutation(t, h, owner.ID, "stickers.removeStickerFromSet", &tg.StickersRemoveStickerFromSetRequest{
		Sticker: &tg.InputDocument{ID: referencedRemovedDocument.ID, AccessHash: referencedRemovedDocument.AccessHash},
	}).Set.Count; got != 1 {
		t.Fatalf("remove referenced custom emoji item returned %d items, want 1", got)
	}
	deletedBody := dispatchChatMethod(t, h, owner.ID, "stickers.deleteStickerSet", &tg.StickersDeleteStickerSetRequest{
		Stickerset: &tg.InputStickerSetID{ID: deletedSet.Set.ID, AccessHash: deletedSet.Set.AccessHash},
	})
	var deleteResult tg.BoolTrue
	if err := deleteResult.Decode(&bin.Buffer{Buf: deletedBody}); err != nil {
		t.Fatalf("delete custom emoji set: %v", err)
	}

	regularFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	regularSet := createStickerSet(t, h, owner.ID, createStickerRequest("regular_docs", regularFile, false, false))
	regularDocument, ok := regularSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("regular sticker set document = %T, want *tg.Document", regularSet.Documents[0])
	}

	attachment := storedStickerDocument(t, s, blobs, owner.ID, []byte("private message attachment"))
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, requester.ID, "private attachment", 91901, attachment.ID, 0); err != nil {
		t.Fatalf("send private attachment: %v", err)
	}

	body := dispatchChatMethod(t, h, requester.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{
		DocumentID: []int64{
			emojiDocument.ID, inactiveDocument.ID, unreferencedRemovedDocument.ID,
			referencedRemovedDocument.ID, deletedDocument.ID, regularDocument.ID, attachment.ID, 919999, -919999,
		},
	})
	var documents tg.DocumentClassVector
	if err := documents.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getCustomEmojiDocuments response: %v", err)
	}
	if len(documents.Elems) != 3 {
		t.Fatalf("getCustomEmojiDocuments returned %d documents, want the live member and two message-referenced historical members", len(documents.Elems))
	}
	wantDocuments := []*tg.Document{emojiDocument, referencedRemovedDocument, deletedDocument}
	for i, want := range wantDocuments {
		got, ok := documents.Elems[i].(*tg.Document)
		if !ok || got.ID != want.ID || got.AccessHash != want.AccessHash {
			t.Fatalf("getCustomEmojiDocuments document %d = %#v, want document %d", i, documents.Elems[i], want.ID)
		}
	}
	document, ok := documents.Elems[0].(*tg.Document)
	if !ok {
		t.Fatalf("getCustomEmojiDocuments document = %T, want *tg.Document", documents.Elems[0])
	}
	var customEmoji *tg.DocumentAttributeCustomEmoji
	for _, attribute := range document.Attributes {
		if value, ok := attribute.(*tg.DocumentAttributeCustomEmoji); ok {
			customEmoji = value
		}
	}
	if customEmoji == nil || customEmoji.Alt != "⭐" || !customEmoji.GetFree() {
		t.Fatalf("custom emoji metadata = %+v, want free star emoji metadata", customEmoji)
	}
	setRef, ok := customEmoji.Stickerset.(*tg.InputStickerSetID)
	if !ok || setRef.ID != emojiSet.Set.ID || setRef.AccessHash != emojiSet.Set.AccessHash {
		t.Fatalf("custom emoji set reference = %#v, want set %d", customEmoji.Stickerset, emojiSet.Set.ID)
	}

	if _, err := api.SendMessageForTest(s, stranger.ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerSelf{}, Message: "⭐⭐", RandomID: 91905,
		Entities: []tg.MessageEntityClass{
			&tg.MessageEntityCustomEmoji{Offset: 0, Length: 1, DocumentID: referencedRemovedDocument.ID},
			&tg.MessageEntityCustomEmoji{Offset: 1, Length: 1, DocumentID: deletedDocument.ID},
		},
	}); err == nil {
		t.Error("send custom emoji references after removal and deletion succeeded")
	}

	strangerBody := dispatchChatMethod(t, h, stranger.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{
		DocumentID: []int64{referencedRemovedDocument.ID, deletedDocument.ID},
	})
	var strangerDocuments tg.DocumentClassVector
	if err := strangerDocuments.Decode(&bin.Buffer{Buf: strangerBody}); err != nil {
		t.Fatalf("decode stranger getCustomEmojiDocuments response: %v", err)
	}
	if len(strangerDocuments.Elems) != 0 {
		t.Fatalf("stranger getCustomEmojiDocuments returned %d historical documents, want none", len(strangerDocuments.Elems))
	}
}

func TestGetCustomEmojiDocumentsRejectsMoreThan200IDs(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	user, err := s.CreateUser(context.Background(), "+15551819903")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	atLimit := make([]int64, 200)
	for i := range atLimit {
		atLimit[i] = int64(i + 1)
	}
	body := dispatchChatMethod(t, fullChannelDispatcher(s), user.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{DocumentID: atLimit})
	var documents tg.DocumentClassVector
	if err := documents.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getCustomEmojiDocuments at the limit: %v", err)
	}
	if len(documents.Elems) != 0 {
		t.Fatalf("getCustomEmojiDocuments at 200 unknown IDs returned %d documents, want none", len(documents.Elems))
	}

	tooMany := make([]int64, 201)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	got := stickerRPCError(t, dispatchChatMethod(t, fullChannelDispatcher(s), user.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{DocumentID: tooMany}))
	if got != "CUSTOM_EMOJI_DOCUMENTS_TOO_MUCH" {
		t.Fatalf("getCustomEmojiDocuments with %d IDs = %q, want CUSTOM_EMOJI_DOCUMENTS_TOO_MUCH", len(tooMany), got)
	}
}
