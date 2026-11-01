package stickerbundle_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/stickerbundle"
)

type testManifest struct {
	Version int        `json:"version"`
	Packs   []testPack `json:"packs"`
}

type testPack struct {
	ShortName   string        `json:"short_name"`
	Source      string        `json:"source"`
	License     string        `json:"license,omitempty"`
	Attribution string        `json:"attribution"`
	Kind        string        `json:"kind,omitempty"`
	Format      string        `json:"format,omitempty"`
	Stickers    []testSticker `json:"stickers"`
	Art         []byte        `json:"-"`
}

type testSticker struct {
	Emoji  string `json:"emoji"`
	File   string `json:"file"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func TestLoadValidatesBundleAndBuildsTitles(t *testing.T) {
	dir := writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{validTestPack("openmoji_animals")}})

	bundle, err := stickerbundle.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	if len(bundle.Packs) != 1 {
		t.Fatalf("bundle packs = %d, want 1", len(bundle.Packs))
	}
	pack := bundle.Packs[0]
	if pack.Title != "OpenMoji Animals (CC BY-SA 4.0)" {
		t.Fatalf("pack title = %q, want source and license", pack.Title)
	}
	if pack.ShortName != "openmoji_animals" || pack.Attribution != "Emoji by OpenMoji" || len(pack.Stickers) != 1 {
		t.Fatalf("loaded pack metadata = %+v", pack)
	}
	if pack.Kind != stickerbundle.PackKindStickers {
		t.Fatalf("default pack kind = %q, want %q", pack.Kind, stickerbundle.PackKindStickers)
	}
	if pack.Stickers[0].Size != int64(len(testWebP)) || pack.Stickers[0].Path == "" {
		t.Fatalf("loaded sticker = %+v, want a checked file path and size", pack.Stickers[0])
	}
}

func TestLoadAcceptsCustomEmojiPackKind(t *testing.T) {
	pack := validTestPack("catalog_custom_emojis")
	pack.Kind = "emojis"
	bundle, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err != nil {
		t.Fatalf("load custom emoji bundle pack: %v", err)
	}
	if len(bundle.Packs) != 1 {
		t.Fatalf("custom emoji bundle packs = %d, want one", len(bundle.Packs))
	}
	if bundle.Packs[0].Kind != stickerbundle.PackKindEmojis {
		t.Fatalf("custom emoji bundle pack kind = %q, want %q", bundle.Packs[0].Kind, stickerbundle.PackKindEmojis)
	}
}

func TestLoadRejectsUnsupportedPackKind(t *testing.T) {
	pack := validTestPack("unsupported_pack_kind")
	pack.Kind = "emoji"
	_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
		t.Fatalf("load unsupported pack kind error = %v, want pack %q named", err, pack.ShortName)
	}
}

func TestLoadAcceptsTelegramStickerBundleFile(t *testing.T) {
	pack := validTestPack("noto_animated_smileys_emotion")
	art := testTGS(t)
	pack.Stickers[0].File = "art/grinning_face.tgs"
	pack.Format = "tgs"
	pack.Stickers[0].Size = int64(len(art))
	checksum := sha256.Sum256(art)
	pack.Stickers[0].SHA256 = hex.EncodeToString(checksum[:])

	bundle, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err != nil {
		t.Fatalf("load TGS sticker bundle: %v", err)
	}
	if len(bundle.Packs) != 1 || len(bundle.Packs[0].Stickers) != 1 {
		t.Fatalf("loaded TGS bundle = %+v, want one pack with one sticker", bundle)
	}
	sticker := bundle.Packs[0].Stickers[0]
	if bundle.Packs[0].Format != "tgs" || sticker.Format != "tgs" || sticker.MimeType != "application/x-tgsticker" || !sticker.Animated {
		t.Fatalf("loaded TGS sticker metadata = pack=%+v sticker=%+v", bundle.Packs[0], sticker)
	}
}

func TestLoadRejectsInvalidAndOversizedTGSFilesWithPackName(t *testing.T) {
	for _, tc := range []struct {
		name string
		art  []byte
	}{
		{name: "gzip header", art: []byte("not gzip")},
		{name: "size limit", art: append([]byte{0x1f, 0x8b}, bytes.Repeat([]byte("x"), 64*1024)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pack := validTestPack("noto_animated_bad")
			pack.Format = "tgs"
			pack.Stickers[0].File = "art/grinning_face.tgs"
			pack.Art = tc.art
			pack.Stickers[0].Size = int64(len(tc.art))
			checksum := sha256.Sum256(tc.art)
			pack.Stickers[0].SHA256 = hex.EncodeToString(checksum[:])

			_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
			if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
				t.Fatalf("load invalid TGS error = %v, want pack %q named", err, pack.ShortName)
			}
		})
	}
}

func TestLoadRejectsMissingOrDisallowedLicenseWithPackName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		license string
	}{
		{name: "missing"},
		{name: "outside allow-list", license: "GPL-3.0-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pack := validTestPack("unlicensed_pack")
			pack.License = tc.license
			_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
			if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
				t.Fatalf("load invalid license error = %v, want pack %q named", err, pack.ShortName)
			}
		})
	}
}

func TestLoadRejectsStickerSizeAndChecksumMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testSticker)
	}{
		{name: "size", change: func(sticker *testSticker) { sticker.Size++ }},
		{name: "checksum", change: func(sticker *testSticker) { sticker.SHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pack := validTestPack("bad_art")
			tc.change(&pack.Stickers[0])
			_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
			if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
				t.Fatalf("load mismatched file error = %v, want pack %q named", err, pack.ShortName)
			}
		})
	}
}

func TestLoadRejectsMoreThanTelegramStickerLimit(t *testing.T) {
	pack := validTestPack("large_pack")
	pack.Stickers = make([]testSticker, 121)
	for i := range pack.Stickers {
		sticker := validTestPack("large_pack").Stickers[0]
		sticker.File = fmt.Sprintf("art/%03d.webp", i)
		pack.Stickers[i] = sticker
	}
	_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
		t.Fatalf("load oversized pack error = %v, want pack %q named", err, pack.ShortName)
	}
}

func TestLoadRejectsReservedAndDuplicateShortNames(t *testing.T) {
	for _, packs := range [][]testPack{
		{validTestPack("twemoji_reactions")},
		{validTestPack("same_name"), validTestPack("same_name")},
	} {
		_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: packs}))
		if err == nil {
			t.Fatalf("load colliding short names %q succeeded", packs[0].ShortName)
		}
	}
}

func TestLoadRejectsTitleOverTelegramLimit(t *testing.T) {
	pack := validTestPack("long_title")
	pack.Source = strings.Repeat("x", 50)
	_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
		t.Fatalf("load long title error = %v, want pack %q named", err, pack.ShortName)
	}
}

func TestLoadRejectsBundlePathsOutsideRoot(t *testing.T) {
	pack := validTestPack("path_escape")
	pack.Stickers[0].File = "../outside.webp"
	_, err := stickerbundle.Load(writeTestBundle(t, testManifest{Version: 1, Packs: []testPack{pack}}))
	if err == nil || !strings.Contains(err.Error(), pack.ShortName) {
		t.Fatalf("load escaping path error = %v, want pack %q named", err, pack.ShortName)
	}
}

var testWebP = []byte("RIFF\x04\x00\x00\x00WEBP")

func validTestPack(shortName string) testPack {
	digest := sha256.Sum256(testWebP)
	return testPack{
		ShortName:   shortName,
		Source:      "OpenMoji Animals",
		License:     "CC-BY-SA-4.0",
		Attribution: "Emoji by OpenMoji",
		Stickers: []testSticker{{
			Emoji:  "🐈",
			File:   "art/cat.webp",
			Size:   int64(len(testWebP)),
			SHA256: hex.EncodeToString(digest[:]),
		}},
	}
}

func writeTestBundle(t *testing.T, manifest testManifest) string {
	t.Helper()
	dir := t.TempDir()
	for _, pack := range manifest.Packs {
		for _, sticker := range pack.Stickers {
			if filepath.IsLocal(sticker.File) && sticker.File != "." {
				path := filepath.Join(dir, filepath.FromSlash(sticker.File))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("create bundle artwork directory: %v", err)
				}
				art := pack.Art
				if art == nil {
					art = testWebP
					if pack.Format == "tgs" {
						art = testTGS(t)
					}
				}
				if err := os.WriteFile(path, art, 0o600); err != nil {
					t.Fatalf("write bundle artwork: %v", err)
				}
			}
		}
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func testTGS(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(`{"tgs":1,"fr":60,"ip":0,"op":1,"w":512,"h":512,"layers":[],"assets":[]}`)); err != nil {
		t.Fatalf("write TGS test data: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close TGS test data: %v", err)
	}
	return output.Bytes()
}
