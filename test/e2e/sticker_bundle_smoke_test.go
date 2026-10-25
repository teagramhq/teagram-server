package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/stickerbundle"
)

func TestStickerBundleCatalogue(t *testing.T) {
	testSmokeStickerBundleCatalogue(t)
}

func testSmokeStickerBundleCatalogue(t *testing.T) {
	t.Helper()
	stickerSets := loadSmokeStickerBundleCatalogue(t)
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.stickerSets = stickerSets
	})
	const phoneA, phoneB = "+15551049801", "+15551049802"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "bundle sticker owner", phoneA)
	b := newSmokeClient(t, f, "bundle sticker recipient", phoneB)

	var featured *tg.MessagesFeaturedStickers
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetFeaturedStickers(ctx, 0)
		if err != nil {
			return err
		}
		var ok bool
		featured, ok = result.(*tg.MessagesFeaturedStickers)
		if !ok || featured.Count != 3 || len(featured.Sets) != 3 {
			return fmt.Errorf("featured sticker sets = %T with count %d and %d entries, want Twemoji and two bundle packs", result, featuredCount(featured), featuredSetCount(featured))
		}
		return nil
	}); err != nil {
		t.Fatalf("get featured bundle sticker packs: %v", err)
	}
	coveredByName := make(map[string]*tg.StickerSetCovered, len(featured.Sets))
	for _, item := range featured.Sets {
		covered, ok := item.(*tg.StickerSetCovered)
		if !ok {
			t.Fatalf("featured sticker set = %T, want *tg.StickerSetCovered", item)
		}
		if _, ok := covered.Cover.(*tg.Document); !ok {
			t.Fatalf("featured %q cover = %T, want *tg.Document", covered.Set.ShortName, covered.Cover)
		}
		coveredByName[covered.Set.ShortName] = covered
	}
	if _, ok := coveredByName["twemoji_reactions"]; !ok {
		t.Fatal("featured sets omitted twemoji_reactions")
	}
	for shortName, title := range map[string]string{
		"openmoji_animals": "OpenMoji Animals (CC BY-SA 4.0)",
		"openmoji_food":    "OpenMoji Food (CC0 1.0)",
	} {
		covered, ok := coveredByName[shortName]
		if !ok || covered.Set.Title != title || covered.Set.Count != 1 {
			t.Fatalf("featured bundle set %q = %+v, want title %q and one sticker", shortName, covered, title)
		}
	}

	ref := &tg.InputStickerSetShortName{ShortName: "openmoji_animals"}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("installStickerSet = %T, want success", result)
		}
		all, err := client.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		sets, ok := all.(*tg.MessagesAllStickers)
		if !ok || len(sets.Sets) != 1 || sets.Sets[0].ShortName != ref.ShortName {
			return fmt.Errorf("getAllStickers = %T with %d sets, want installed %q", all, allStickerCount(all), ref.ShortName)
		}
		return nil
	}); err != nil {
		t.Fatalf("install and list bundle sticker pack: %v", err)
	}

	var document *tg.Document
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		full, ok := result.(*tg.MessagesStickerSet)
		if !ok || len(full.Documents) != 1 || len(full.Packs) != 1 || full.Set.Count != 1 {
			return fmt.Errorf("getStickerSet = %T with %d documents and %d packs, want one of each", result, stickerDocumentCount(full), stickerPackCount(full))
		}
		document, ok = full.Documents[0].(*tg.Document)
		if !ok {
			return fmt.Errorf("bundle sticker document = %T, want *tg.Document", full.Documents[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("get installed bundle sticker set: %v", err)
	}
	wantBytes := smokeStickerArt("animal")
	var downloaded []byte
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
			Limit:    262144,
		})
		if err != nil {
			return err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || !bytes.Equal(file.Bytes, wantBytes) {
			return fmt.Errorf("bundle upload.getFile = %T with %d bytes, want %d exact bundle bytes", result, uploadResultSize(result), len(wantBytes))
		}
		downloaded = bytes.Clone(file.Bytes)
		return nil
	}); err != nil {
		t.Fatalf("download bundle sticker artwork: %v", err)
	}

	const sentText = "bundle sticker smoke"
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id),
			Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
				ID: document.ID, AccessHash: document.AccessHash, FileReference: []byte("ignored"),
			}},
			Message: sentText, RandomID: 1049801,
		})
		return err
	}); err != nil {
		t.Fatalf("send bundle sticker to second account: %v", err)
	}
	var received *tg.Document
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerUser(b.id, a.id), Limit: 10})
		if err != nil {
			return err
		}
		history, ok := result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("recipient history = %T, want *tg.MessagesMessages", result)
		}
		for _, item := range history.Messages {
			message, ok := item.(*tg.Message)
			if !ok || message.Message != sentText {
				continue
			}
			media, ok := message.Media.(*tg.MessageMediaDocument)
			if !ok {
				return fmt.Errorf("recipient sticker media = %T, want *tg.MessageMediaDocument", message.Media)
			}
			received, ok = media.Document.(*tg.Document)
			if !ok {
				return fmt.Errorf("recipient sticker document = %T, want *tg.Document", media.Document)
			}
		}
		if received == nil || received.ID != document.ID || received.AccessHash != document.AccessHash {
			return fmt.Errorf("recipient document = %+v, want catalogue document (%d,%d)", received, document.ID, document.AccessHash)
		}
		return nil
	}); err != nil {
		t.Fatalf("load sent bundle sticker from recipient history: %v", err)
	}
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: received.ID, AccessHash: received.AccessHash, FileReference: received.FileReference},
			Limit:    262144,
		})
		if err != nil {
			return err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || !bytes.Equal(file.Bytes, downloaded) {
			return fmt.Errorf("recipient upload.getFile = %T with %d bytes, want the %d original bytes", result, uploadResultSize(result), len(downloaded))
		}
		return nil
	}); err != nil {
		t.Fatalf("recipient downloads received bundle sticker: %v", err)
	}
}

func loadSmokeStickerBundleCatalogue(t *testing.T) *api.StickerSetCatalog {
	t.Helper()
	dir := t.TempDir()
	packs := []struct {
		shortName, source, license, emoji, filename string
		art                                         []byte
	}{
		{shortName: "openmoji_animals", source: "OpenMoji Animals", license: "CC-BY-SA-4.0", emoji: "🐈", filename: "animal.webp", art: smokeStickerArt("animal")},
		{shortName: "openmoji_food", source: "OpenMoji Food", license: "CC0-1.0", emoji: "🍕", filename: "food.webp", art: smokeStickerArt("food")},
	}
	manifestPacks := make([]map[string]any, 0, len(packs))
	for _, pack := range packs {
		checksum := sha256.Sum256(pack.art)
		file := filepath.ToSlash(filepath.Join(pack.shortName, pack.filename))
		manifestPacks = append(manifestPacks, map[string]any{
			"short_name":  pack.shortName,
			"source":      pack.source,
			"license":     pack.license,
			"attribution": "Artwork by OpenMoji",
			"stickers": []any{map[string]any{
				"emoji": pack.emoji, "file": file, "size": len(pack.art), "sha256": hex.EncodeToString(checksum[:]),
			}},
		})
		artPath := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(artPath), 0o700); err != nil {
			t.Fatalf("create bundle art directory: %v", err)
		}
		if err := os.WriteFile(artPath, pack.art, 0o600); err != nil {
			t.Fatalf("write bundle art: %v", err)
		}
	}
	data, err := json.Marshal(map[string]any{"version": 1, "packs": manifestPacks})
	if err != nil {
		t.Fatalf("encode bundle manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o600); err != nil {
		t.Fatalf("write bundle manifest: %v", err)
	}
	bundle, err := stickerbundle.Load(dir)
	if err != nil {
		t.Fatalf("validate bundle: %v", err)
	}
	catalog, err := api.LoadStickerSetCatalog(bundle)
	if err != nil {
		t.Fatalf("build bundle catalogue: %v", err)
	}
	return catalog
}

func smokeStickerArt(content string) []byte {
	return []byte("RIFF\x00\x00\x00\x00WEBP" + content)
}

func featuredCount(result *tg.MessagesFeaturedStickers) int {
	if result == nil {
		return 0
	}
	return result.Count
}

func featuredSetCount(result *tg.MessagesFeaturedStickers) int {
	if result == nil {
		return 0
	}
	return len(result.Sets)
}

func allStickerCount(result tg.MessagesAllStickersClass) int {
	full, ok := result.(*tg.MessagesAllStickers)
	if !ok {
		return 0
	}
	return len(full.Sets)
}

func stickerDocumentCount(result *tg.MessagesStickerSet) int {
	if result == nil {
		return 0
	}
	return len(result.Documents)
}

func stickerPackCount(result *tg.MessagesStickerSet) int {
	if result == nil {
		return 0
	}
	return len(result.Packs)
}

func uploadResultSize(result tg.UploadFileClass) int {
	file, ok := result.(*tg.UploadFile)
	if !ok {
		return 0
	}
	return len(file.Bytes)
}
