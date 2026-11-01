package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/stickerbundle"
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

	bundle := loadCustomEmojiCatalogTestBundle(t)
	catalog, err := buildStickerSetCatalog(bundle)
	if err != nil {
		t.Fatalf("build custom emoji catalogue from bundle: %v", err)
	}
	customSet := catalog.byShortName["catalog_custom_emojis"]
	if customSet == nil || !customSet.set.GetEmojis() || len(customSet.documents) != 1 {
		t.Fatalf("custom emoji catalogue set = %+v, want one emoji document", customSet)
	}
	productionEmojiSet := catalog.byShortName[twemojiCustomEmojiSetShortName]
	if productionEmojiSet == nil || !productionEmojiSet.set.GetEmojis() || len(productionEmojiSet.documents) == 0 {
		t.Fatalf("production custom emoji catalogue set = %+v, want built-in emoji documents", productionEmojiSet)
	}
	if len(catalog.featured) != 2 {
		t.Fatalf("featured sticker sets = %d, want built-in reactions and the regular bundle pack", len(catalog.featured))
	}
	catalogDocument := customSet.documents[0]
	productionDocument := productionEmojiSet.documents[0]
	if len(catalogDocument.Attributes) < 2 {
		t.Fatalf("custom emoji catalogue attributes = %#v, want image size and custom emoji metadata", catalogDocument.Attributes)
	}
	customAttribute, ok := catalogDocument.Attributes[1].(*tg.DocumentAttributeCustomEmoji)
	if !ok || customAttribute.Alt != "⭐" || !customAttribute.GetFree() {
		t.Fatalf("custom emoji catalogue attribute = %#v, want free star emoji", catalogDocument.Attributes[1])
	}
	imageSize, ok := catalogDocument.Attributes[0].(*tg.DocumentAttributeImageSize)
	if !ok || imageSize.W != 100 || imageSize.H != 100 {
		t.Fatalf("custom emoji catalogue image size = %#v, want 100x100", catalogDocument.Attributes[0])
	}
	setRef, ok := customAttribute.Stickerset.(*tg.InputStickerSetID)
	if !ok || setRef.ID != customSet.set.ID || setRef.AccessHash != customSet.set.AccessHash {
		t.Fatalf("custom emoji catalogue set reference = %#v, want set %d", customAttribute.Stickerset, customSet.set.ID)
	}
	productionAttribute, found := findCustomEmojiAttribute(productionDocument)
	if !found || productionAttribute.Alt == "" || !productionAttribute.GetFree() {
		t.Fatalf("production custom emoji attribute = %#v, want free Twemoji emoji metadata", productionAttribute)
	}
	productionSetRef, ok := productionAttribute.Stickerset.(*tg.InputStickerSetID)
	if !ok || productionSetRef.ID != productionEmojiSet.set.ID || productionSetRef.AccessHash != productionEmojiSet.set.AccessHash {
		t.Fatalf("production custom emoji set reference = %#v, want set %d", productionAttribute.Stickerset, productionEmojiSet.set.ID)
	}
	regularSet := catalog.byShortName["catalog_stickers"]
	if regularSet == nil || len(regularSet.documents) != 1 {
		t.Fatalf("regular catalogue set = %+v, want one sticker document", regularSet)
	}
	regularDocument := regularSet.documents[0]
	h := testHandlers(s)
	h.stickerSets = catalog
	var request bin.Buffer
	if err := (&tg.MessagesGetCustomEmojiDocumentsRequest{DocumentID: []int64{productionDocument.ID, catalogDocument.ID, regularDocument.ID, 0}}).Encode(&request); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	response, err := h.handleGetCustomEmojiDocuments(&mtproto.Request{Ctx: ctx, UserID: 99, Buf: &request})
	if err != nil {
		t.Fatalf("get catalog custom emoji documents: %v", err)
	}
	documents, ok := response.(*tg.DocumentClassVector)
	if !ok || len(documents.Elems) != 2 {
		t.Fatalf("catalog custom emoji response = %#v, want both production and bundle custom emoji members", response)
	}
	document, ok := documents.Elems[0].(*tg.Document)
	if !ok || document.ID != productionDocument.ID {
		t.Fatalf("production custom emoji document = %#v, want %d", documents.Elems[0], productionDocument.ID)
	}
	attribute, found := findCustomEmojiAttribute(document)
	if !found || attribute.Alt != productionAttribute.Alt || !attribute.GetFree() {
		t.Fatalf("production custom emoji attribute = %#v, want free Twemoji metadata", attribute)
	}
	returnedSet, ok := attribute.Stickerset.(*tg.InputStickerSetID)
	if !ok || returnedSet.ID != productionSetRef.ID || returnedSet.AccessHash != productionSetRef.AccessHash {
		t.Fatalf("production custom emoji set reference = %#v, want set %d", attribute.Stickerset, productionSetRef.ID)
	}
	bundleDocument, ok := documents.Elems[1].(*tg.Document)
	if !ok || bundleDocument.ID != catalogDocument.ID {
		t.Fatalf("bundle custom emoji document = %#v, want %d", documents.Elems[1], catalogDocument.ID)
	}
	bundleAttribute, found := findCustomEmojiAttribute(bundleDocument)
	if !found || bundleAttribute.Alt != "⭐" || !bundleAttribute.GetFree() {
		t.Fatalf("bundle custom emoji attribute = %#v, want free star emoji metadata", bundleAttribute)
	}
	returnedBundleSet, ok := bundleAttribute.Stickerset.(*tg.InputStickerSetID)
	if !ok || returnedBundleSet.ID != setRef.ID || returnedBundleSet.AccessHash != setRef.AccessHash {
		t.Fatalf("bundle custom emoji set reference = %#v, want set %d", bundleAttribute.Stickerset, setRef.ID)
	}
	attribute.Alt = "changed"
	returnedSet.ID = -99
	if productionAttribute.Alt == "changed" || productionSetRef.ID != productionEmojiSet.set.ID {
		t.Fatalf("mutating production catalogue response changed its source: attribute=%+v set=%+v", productionAttribute, productionSetRef)
	}
}

type customEmojiCatalogManifest struct {
	Version int                      `json:"version"`
	Packs   []customEmojiCatalogPack `json:"packs"`
}

type customEmojiCatalogPack struct {
	ShortName   string                      `json:"short_name"`
	Source      string                      `json:"source"`
	License     string                      `json:"license"`
	Attribution string                      `json:"attribution"`
	Kind        string                      `json:"kind,omitempty"`
	Stickers    []customEmojiCatalogSticker `json:"stickers"`
}

type customEmojiCatalogSticker struct {
	Emoji  string `json:"emoji"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func loadCustomEmojiCatalogTestBundle(t *testing.T) *stickerbundle.Bundle {
	t.Helper()
	root := t.TempDir()
	art := testBundleWebP("custom emoji catalogue art")
	checksum := sha256.Sum256(art)
	manifest := customEmojiCatalogManifest{
		Version: 1,
		Packs: []customEmojiCatalogPack{
			{
				ShortName: "catalog_custom_emojis", Source: "OpenMoji Custom Emoji",
				License: "CC-BY-SA-4.0", Attribution: "Emoji by OpenMoji", Kind: "emojis",
				Stickers: []customEmojiCatalogSticker{{
					Emoji: "⭐", File: "catalog_custom_emojis/star.webp", Size: int64(len(art)), SHA256: hex.EncodeToString(checksum[:]),
				}},
			},
			{
				ShortName: "catalog_stickers", Source: "OpenMoji Animals",
				License: "CC-BY-SA-4.0", Attribution: "Emoji by OpenMoji",
				Stickers: []customEmojiCatalogSticker{{
					Emoji: "🐈", File: "catalog_stickers/cat.webp", Size: int64(len(art)), SHA256: hex.EncodeToString(checksum[:]),
				}},
			},
		},
	}
	for _, pack := range manifest.Packs {
		file := filepath.Join(root, filepath.FromSlash(pack.Stickers[0].File))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatalf("create custom emoji catalogue asset directory: %v", err)
		}
		if err := os.WriteFile(file, art, 0o600); err != nil {
			t.Fatalf("write custom emoji catalogue asset: %v", err)
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode custom emoji catalogue manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatalf("write custom emoji catalogue manifest: %v", err)
	}
	bundle, err := stickerbundle.Load(root)
	if err != nil {
		t.Fatalf("load custom emoji catalogue bundle: %v", err)
	}
	return bundle
}

func findCustomEmojiAttribute(document *tg.Document) (*tg.DocumentAttributeCustomEmoji, bool) {
	for _, attribute := range document.Attributes {
		if value, ok := attribute.(*tg.DocumentAttributeCustomEmoji); ok {
			return value, true
		}
	}
	return nil, false
}
