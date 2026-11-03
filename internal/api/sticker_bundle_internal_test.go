package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/stickerbundle"
)

func TestStickerBundleCatalogueKeepsSetIdentityAcrossArtRegeneration(t *testing.T) {
	firstArt := testBundleWebP("first art")
	secondArt := testBundleWebP("regenerated art")
	first := loadTestStickerCatalogue(t, firstArt)
	second := loadTestStickerCatalogue(t, secondArt)

	if len(first.byShortName) != 4 || len(second.byShortName) != 4 {
		t.Fatalf("catalogue set counts = %d and %d, want built-in sticker and emoji sets plus two bundle packs", len(first.byShortName), len(second.byShortName))
	}
	firstEmojiSet := first.byShortName[twemojiCustomEmojiSetShortName]
	secondEmojiSet := second.byShortName[twemojiCustomEmojiSetShortName]
	if firstEmojiSet == nil || secondEmojiSet == nil || !firstEmojiSet.set.GetEmojis() || len(firstEmojiSet.documents) != 73 {
		t.Fatalf("production custom emoji catalogue set = %+v, want 73 emoji documents", firstEmojiSet)
	}
	if firstEmojiSet.set.ID != secondEmojiSet.set.ID || firstEmojiSet.set.AccessHash != secondEmojiSet.set.AccessHash || firstEmojiSet.set.Hash != secondEmojiSet.set.Hash {
		t.Fatalf("production custom emoji set identity changed: first=%+v second=%+v", firstEmojiSet.set, secondEmojiSet.set)
	}
	if attribute, ok := findCustomEmojiAttribute(firstEmojiSet.documents[0]); !ok || !attribute.GetFree() || attribute.Alt == "" {
		t.Fatalf("production custom emoji document attribute = %#v, want free Twemoji emoji metadata", firstEmojiSet.documents[0].Attributes)
	}
	firstSet := first.byShortName["openmoji_animals"]
	secondSet := second.byShortName["openmoji_animals"]
	if firstSet == nil || secondSet == nil {
		t.Fatal("bundle pack missing from catalogue")
	}
	if firstSet.set.ID != secondSet.set.ID || firstSet.set.AccessHash != secondSet.set.AccessHash {
		t.Fatalf("set identity changed with artwork: first=(%d,%d), second=(%d,%d)", firstSet.set.ID, firstSet.set.AccessHash, secondSet.set.ID, secondSet.set.AccessHash)
	}
	if firstSet.set.Hash == secondSet.set.Hash {
		t.Fatalf("set hash stayed %d after art changed", firstSet.set.Hash)
	}
	if firstSet.set.Title != "OpenMoji Animals (CC BY-SA 4.0)" {
		t.Fatalf("bundle set title = %q", firstSet.set.Title)
	}
	if len(firstSet.documents) != 1 || len(firstSet.packs) != 1 {
		t.Fatalf("bundle set has %d documents and %d sticker packs, want one each", len(firstSet.documents), len(firstSet.packs))
	}
	document := firstSet.documents[0]
	stored := first.documents[reactionDocumentIdentity{id: document.ID, accessHash: document.AccessHash}]
	if len(stored.data) != 0 {
		t.Fatalf("bundle artwork retained %d bytes in memory", len(stored.data))
	}
	read, err := stored.readChunk(context.Background(), 0, int64(len(firstArt)))
	if err != nil {
		t.Fatalf("read bundle artwork range: %v", err)
	}
	if string(read) != string(firstArt) {
		t.Fatalf("bundle artwork bytes = %q, want %q", read, firstArt)
	}
}

func TestStickerBundleCustomEmojiIdentityDependsOnlyOnShortName(t *testing.T) {
	first := loadTestCustomEmojiCatalogue(t, testBundleWebP("first custom emoji art"))
	second := loadTestCustomEmojiCatalogue(t, testBundleWebP("regenerated custom emoji art"))
	firstSet := first.byShortName["openmoji_emoji_animals"]
	secondSet := second.byShortName["openmoji_emoji_animals"]
	if firstSet == nil || secondSet == nil || !firstSet.set.GetEmojis() {
		t.Fatalf("custom emoji catalogue sets = %+v and %+v, want generated emoji set", firstSet, secondSet)
	}
	if firstSet.set.ID != secondSet.set.ID || firstSet.set.AccessHash != secondSet.set.AccessHash {
		t.Fatalf("custom emoji set identity changed with art: first=(%d,%d), second=(%d,%d)", firstSet.set.ID, firstSet.set.AccessHash, secondSet.set.ID, secondSet.set.AccessHash)
	}
	if firstSet.set.Hash == secondSet.set.Hash {
		t.Fatalf("custom emoji set hash stayed %d after art changed", firstSet.set.Hash)
	}
	if firstSet.set.Title != "OpenMoji Animals (CC-BY-SA-4.0)" {
		t.Fatalf("custom emoji set title = %q, want source and SPDX license", firstSet.set.Title)
	}
	document := firstSet.documents[0]
	customEmoji, ok := findCustomEmojiAttribute(document)
	if !ok || customEmoji.Alt != "🐈" || !customEmoji.GetFree() {
		t.Fatalf("custom emoji document attribute = %#v, want free cat emoji metadata", document.Attributes)
	}
	var imageSize *tg.DocumentAttributeImageSize
	for _, attribute := range document.Attributes {
		if value, ok := attribute.(*tg.DocumentAttributeImageSize); ok {
			imageSize = value
		}
	}
	if imageSize == nil || imageSize.W != 100 || imageSize.H != 100 {
		t.Fatalf("custom emoji document image size = %+v, want 100x100", imageSize)
	}
}

func loadTestCustomEmojiCatalogue(t *testing.T, art []byte) *stickerSetCatalog {
	t.Helper()
	dir := t.TempDir()
	const shortName = "openmoji_emoji_animals"
	const artworkPath = "openmoji_emoji_animals/cat.webp"
	checksum := sha256.Sum256(art)
	manifest := map[string]any{
		"version": 1,
		"packs": []any{map[string]any{
			"short_name":  shortName,
			"source":      "OpenMoji Animals",
			"license":     "CC-BY-SA-4.0",
			"attribution": "Emoji by OpenMoji",
			"kind":        stickerbundle.PackKindEmojis,
			"stickers": []any{map[string]any{
				"emoji": "🐈", "file": artworkPath, "size": len(art), "sha256": hex.EncodeToString(checksum[:]),
			}},
		}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode custom emoji bundle manifest: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, artworkPath)), 0o700); err != nil {
		t.Fatalf("create custom emoji artwork directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, artworkPath), art, 0o600); err != nil {
		t.Fatalf("write custom emoji bundle artwork: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write custom emoji bundle manifest: %v", err)
	}
	bundle, err := stickerbundle.Load(dir)
	if err != nil {
		t.Fatalf("load custom emoji bundle: %v", err)
	}
	catalog, err := buildStickerSetCatalog(bundle)
	if err != nil {
		t.Fatalf("build custom emoji sticker catalogue: %v", err)
	}
	return catalog
}

func loadTestStickerCatalogue(t *testing.T, art []byte) *stickerSetCatalog {
	t.Helper()
	dir := t.TempDir()
	manifest := struct {
		Version int `json:"version"`
		Packs   []struct {
			ShortName   string `json:"short_name"`
			Source      string `json:"source"`
			License     string `json:"license"`
			Attribution string `json:"attribution"`
			Stickers    []struct {
				Emoji  string `json:"emoji"`
				File   string `json:"file"`
				Size   int    `json:"size"`
				SHA256 string `json:"sha256"`
			} `json:"stickers"`
		} `json:"packs"`
	}{Version: 1}
	for _, packName := range []string{"openmoji_animals", "openmoji_food"} {
		file := filepath.Join("art", packName+".webp")
		checksum := sha256.Sum256(art)
		sticker := struct {
			Emoji  string `json:"emoji"`
			File   string `json:"file"`
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		}{Emoji: "🐈", File: file, Size: len(art), SHA256: hex.EncodeToString(checksum[:])}
		pack := struct {
			ShortName   string `json:"short_name"`
			Source      string `json:"source"`
			License     string `json:"license"`
			Attribution string `json:"attribution"`
			Stickers    []struct {
				Emoji  string `json:"emoji"`
				File   string `json:"file"`
				Size   int    `json:"size"`
				SHA256 string `json:"sha256"`
			} `json:"stickers"`
		}{ShortName: packName, Source: "OpenMoji Animals", License: "CC-BY-SA-4.0", Attribution: "Emoji by OpenMoji", Stickers: []struct {
			Emoji  string `json:"emoji"`
			File   string `json:"file"`
			Size   int    `json:"size"`
			SHA256 string `json:"sha256"`
		}{sticker}}
		manifest.Packs = append(manifest.Packs, pack)
		fullPath := filepath.Join(dir, file)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatalf("create bundle artwork directory: %v", err)
		}
		if err := os.WriteFile(fullPath, art, 0o600); err != nil {
			t.Fatalf("write bundle artwork: %v", err)
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write bundle manifest: %v", err)
	}
	bundle, err := stickerbundle.Load(dir)
	if err != nil {
		t.Fatalf("load test bundle: %v", err)
	}
	catalog, err := buildStickerSetCatalog(bundle)
	if err != nil {
		t.Fatalf("build sticker catalogue: %v", err)
	}
	return catalog
}

func testBundleWebP(body string) []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBP"), []byte(body)...)
}
