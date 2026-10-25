package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/stickerbundle"
)

func TestAnimatedStickerBundleDocumentsUseTGSMetadata(t *testing.T) {
	catalog := loadAnimatedTestStickerCatalog(t)
	set := catalog.byShortName["noto_animated_smileys_emotion"]
	if set == nil || len(set.documents) != 1 {
		t.Fatalf("animated sticker set = %+v, want one document", set)
	}
	document := set.documents[0]
	if document.MimeType != "application/x-tgsticker" {
		t.Fatalf("animated sticker MIME type = %q, want application/x-tgsticker", document.MimeType)
	}
	var hasAnimated, hasImageSize, hasSticker, hasFilename bool
	var imageWidth, imageHeight int
	for _, attribute := range document.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeAnimated:
			hasAnimated = true
		case *tg.DocumentAttributeImageSize:
			hasImageSize = true
			imageWidth, imageHeight = value.W, value.H
		case *tg.DocumentAttributeSticker:
			hasSticker = true
		case *tg.DocumentAttributeFilename:
			hasFilename = true
		}
	}
	if hasAnimated || !hasImageSize || imageWidth != 512 || imageHeight != 512 || !hasSticker || !hasFilename {
		t.Fatalf("animated sticker attributes = %#v, want 512x512 image size, sticker, and filename attributes without animated", document.Attributes)
	}
}

func loadAnimatedTestStickerCatalog(t *testing.T) *stickerSetCatalog {
	t.Helper()
	dir := t.TempDir()
	art := testAnimatedBundleTGS(t)
	checksum := sha256.Sum256(art)
	file := "noto_animated_smileys_emotion/1f600.tgs"
	manifest := map[string]any{
		"version": 1,
		"packs": []any{map[string]any{
			"short_name":  "noto_animated_smileys_emotion",
			"source":      "Animated Noto Emoji Smileys & Emotion",
			"license":     "CC-BY-4.0",
			"attribution": "Animated Noto Emoji by Google.",
			"format":      "tgs",
			"stickers": []any{map[string]any{
				"emoji": "😀", "file": file, "size": len(art), "sha256": hex.EncodeToString(checksum[:]),
			}},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode animated sticker manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o700); err != nil {
		t.Fatalf("create animated sticker directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), art, 0o600); err != nil {
		t.Fatalf("write animated sticker art: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write animated sticker manifest: %v", err)
	}
	bundle, err := stickerbundle.Load(dir)
	if err != nil {
		t.Fatalf("load animated sticker bundle: %v", err)
	}
	catalog, err := buildStickerSetCatalog(bundle)
	if err != nil {
		t.Fatalf("build animated sticker catalogue: %v", err)
	}
	return catalog
}

func testAnimatedBundleTGS(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(`{"tgs":1,"fr":60,"ip":0,"op":1,"w":512,"h":512,"layers":[],"assets":[]}`)); err != nil {
		t.Fatalf("write animated sticker TGS data: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close animated sticker TGS data: %v", err)
	}
	return output.Bytes()
}
