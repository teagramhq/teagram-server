package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestGetCustomEmojiDocumentsReturnsOnlyLiveCustomEmojiSetMembers(t *testing.T) {
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

	emojiFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	emojiRequest := createStickerRequest("custom_emoji_docs", emojiFile, false, true)
	emojiRequest.TextColor = true
	emojiSet := createStickerSet(t, fullChannelDispatcher(s), owner.ID, emojiRequest)
	emojiDocument, ok := emojiSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("custom emoji set document = %T, want *tg.Document", emojiSet.Documents[0])
	}

	inactiveFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGSSize(t, 100, 100))
	inactiveSet := createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("inactive_emoji_docs", inactiveFile, false, true))
	inactiveDocument, ok := inactiveSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("inactive custom emoji set document = %T, want *tg.Document", inactiveSet.Documents[0])
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to sticker test database: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE user_created_sticker_sets SET is_live = false WHERE id = $1", inactiveSet.Set.ID); err != nil {
		closeErr := conn.Close(ctx)
		t.Fatalf("mark custom emoji set inactive: %v", errors.Join(err, closeErr))
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close sticker test database connection: %v", err)
	}

	regularFile := storedStickerDocument(t, s, blobs, owner.ID, validStickerTGS(t))
	regularSet := createStickerSet(t, fullChannelDispatcher(s), owner.ID, createStickerRequest("regular_docs", regularFile, false, false))
	regularDocument, ok := regularSet.Documents[0].(*tg.Document)
	if !ok {
		t.Fatalf("regular sticker set document = %T, want *tg.Document", regularSet.Documents[0])
	}

	attachment := storedStickerDocument(t, s, blobs, owner.ID, []byte("private message attachment"))
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, requester.ID, "private attachment", 91901, attachment.ID, 0); err != nil {
		t.Fatalf("send private attachment: %v", err)
	}

	body := dispatchChatMethod(t, fullChannelDispatcher(s), requester.ID, "messages.getCustomEmojiDocuments", &tg.MessagesGetCustomEmojiDocumentsRequest{
		DocumentID: []int64{emojiDocument.ID, inactiveDocument.ID, regularDocument.ID, attachment.ID, 919999},
	})
	var documents tg.DocumentClassVector
	if err := documents.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getCustomEmojiDocuments response: %v", err)
	}
	if len(documents.Elems) != 1 {
		t.Fatalf("getCustomEmojiDocuments returned %d documents, want only the live custom-emoji set member", len(documents.Elems))
	}
	document, ok := documents.Elems[0].(*tg.Document)
	if !ok || document.ID != emojiDocument.ID || document.AccessHash != emojiDocument.AccessHash {
		t.Fatalf("getCustomEmojiDocuments document = %#v, want document %d", documents.Elems[0], emojiDocument.ID)
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
