package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCloneStickerDocumentCopiesCustomEmojiMetadata(t *testing.T) {
	t.Parallel()

	setRef := &tg.InputStickerSetID{ID: 41, AccessHash: 42}
	attribute := &tg.DocumentAttributeCustomEmoji{
		Alt: "⭐", Stickerset: setRef,
	}
	attribute.SetFree(true)
	source := &tg.Document{
		ID: 51, AccessHash: 52,
		Attributes: []tg.DocumentAttributeClass{attribute},
	}

	cloned := cloneStickerDocument(source, 3)
	got, ok := cloned.Attributes[0].(*tg.DocumentAttributeCustomEmoji)
	if !ok {
		t.Fatalf("custom emoji attribute = %T, want *tg.DocumentAttributeCustomEmoji", cloned.Attributes[0])
	}
	if got.Alt != "⭐" || !got.GetFree() || got.Stickerset == nil {
		t.Fatalf("custom emoji attribute = %+v, want free star emoji with a set reference", got)
	}
	gotRef, ok := got.Stickerset.(*tg.InputStickerSetID)
	if !ok || gotRef.ID != 41 || gotRef.AccessHash != 42 {
		t.Fatalf("custom emoji set reference = %#v, want set 41 with access hash 42", got.Stickerset)
	}
	got.Alt = "changed"
	gotRef.ID = 99
	if attribute.Alt != "⭐" || setRef.ID != 41 {
		t.Fatalf("mutating cloned custom emoji metadata changed catalogue source: attribute=%+v set=%+v", attribute, setRef)
	}
}

func TestGetCustomEmojiDocumentsIncludesCatalogEmojiMembersOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	setRef := &tg.InputStickerSetID{ID: -51, AccessHash: 52}
	customEmoji := &tg.DocumentAttributeCustomEmoji{Alt: "⭐", Stickerset: setRef}
	customEmoji.SetFree(true)
	catalogDocument := &tg.Document{
		ID: -41, AccessHash: 42,
		Attributes: []tg.DocumentAttributeClass{customEmoji},
	}
	regularDocument := &tg.Document{
		ID: -43, AccessHash: 44,
		Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeSticker{
			Alt: "⭐", Stickerset: setRef,
		}},
	}
	h := testHandlers(s)
	h.stickerSets = &stickerSetCatalog{
		documentsByID: map[int64]*tg.Document{
			catalogDocument.ID: catalogDocument,
			regularDocument.ID: regularDocument,
		},
	}
	var request bin.Buffer
	if err := (&tg.MessagesGetCustomEmojiDocumentsRequest{DocumentID: []int64{-41, -43, -45}}).Encode(&request); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	response, err := h.handleGetCustomEmojiDocuments(&mtproto.Request{Ctx: ctx, UserID: 99, Buf: &request})
	if err != nil {
		t.Fatalf("get catalog custom emoji documents: %v", err)
	}
	documents, ok := response.(*tg.DocumentClassVector)
	if !ok || len(documents.Elems) != 1 {
		t.Fatalf("catalog custom emoji response = %#v, want only custom emoji catalogue member", response)
	}
	document, ok := documents.Elems[0].(*tg.Document)
	if !ok || document.ID != catalogDocument.ID {
		t.Fatalf("catalog custom emoji document = %#v, want %d", documents.Elems[0], catalogDocument.ID)
	}
	attribute, ok := document.Attributes[0].(*tg.DocumentAttributeCustomEmoji)
	if !ok || attribute.Alt != "⭐" || !attribute.GetFree() {
		t.Fatalf("catalog custom emoji attribute = %#v, want free star emoji metadata", document.Attributes[0])
	}
	returnedSet, ok := attribute.Stickerset.(*tg.InputStickerSetID)
	if !ok || returnedSet.ID != setRef.ID || returnedSet.AccessHash != setRef.AccessHash {
		t.Fatalf("catalog custom emoji set reference = %#v, want set %d", attribute.Stickerset, setRef.ID)
	}
	attribute.Alt = "changed"
	returnedSet.ID = -99
	if customEmoji.Alt != "⭐" || setRef.ID != -51 {
		t.Fatalf("mutating catalog response changed its source: attribute=%+v set=%+v", customEmoji, setRef)
	}
}
