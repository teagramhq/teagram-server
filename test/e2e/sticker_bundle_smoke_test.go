package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		if !ok || featured.Count != 4 || len(featured.Sets) != 4 {
			return fmt.Errorf("featured sticker sets = %T with count %d and %d entries, want Twemoji and three bundle packs", result, featuredCount(featured), featuredSetCount(featured))
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
		"openmoji_animals":              "OpenMoji Animals (CC BY-SA 4.0)",
		"openmoji_food":                 "OpenMoji Food (CC0 1.0)",
		"noto_animated_smileys_emotion": "Animated Noto Emoji Smileys & Emotion (CC BY 4.0)",
	} {
		covered, ok := coveredByName[shortName]
		if !ok || covered.Set.Title != title || covered.Set.Count != 1 {
			t.Fatalf("featured bundle set %q = %+v, want title %q and one sticker", shortName, covered, title)
		}
	}

	wantOldCount := len(featured.Sets)
	oldNames := make(map[string]struct{}, wantOldCount)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for offset := range wantOldCount {
			result, err := client.MessagesGetOldFeaturedStickers(ctx, &tg.MessagesGetOldFeaturedStickersRequest{Offset: offset, Limit: 1, Hash: 0})
			if err != nil {
				return err
			}
			oldFeatured, ok := result.(*tg.MessagesFeaturedStickers)
			if !ok || oldFeatured == nil || oldFeatured.Count != wantOldCount || len(oldFeatured.Sets) != 1 {
				return fmt.Errorf("old featured page %d = %T with count %d and %d entries, want one of %d catalogue sets", offset, result, featuredCount(oldFeatured), featuredSetCount(oldFeatured), wantOldCount)
			}
			covered, ok := oldFeatured.Sets[0].(*tg.StickerSetCovered)
			if !ok {
				return fmt.Errorf("old featured sticker set = %T, want *tg.StickerSetCovered", oldFeatured.Sets[0])
			}
			oldNames[covered.Set.ShortName] = struct{}{}
		}
		return nil
	}); err != nil {
		t.Fatalf("get old featured bundle sticker packs: %v", err)
	}
	for shortName := range coveredByName {
		if _, ok := oldNames[shortName]; !ok {
			t.Errorf("old featured sets omitted %q", shortName)
		}
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		query := strings.ToUpper("OpenMoji Food")
		result, err := client.MessagesSearchStickerSets(ctx, &tg.MessagesSearchStickerSetsRequest{Q: query, Hash: 0})
		if err != nil {
			return err
		}
		found, ok := result.(*tg.MessagesFoundStickerSets)
		if !ok || found == nil || len(found.Sets) != 1 {
			count := 0
			if found != nil {
				count = len(found.Sets)
			}
			return fmt.Errorf("searchStickerSets %q = %T with %d sets, want a bundle title match", query, result, count)
		}
		covered, ok := found.Sets[0].(*tg.StickerSetCovered)
		if !ok || covered.Set.ShortName != "openmoji_food" {
			return fmt.Errorf("searchStickerSets %q returned %T, want bundle pack openmoji_food", query, found.Sets[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("search catalogue bundle sticker pack: %v", err)
	}

	installedNames := map[string]struct{}{
		"openmoji_animals":              {},
		"noto_animated_smileys_emotion": {},
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for shortName := range installedNames {
			result, err := client.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{
				Stickerset: &tg.InputStickerSetShortName{ShortName: shortName},
			})
			if err != nil {
				return err
			}
			if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
				return fmt.Errorf("installStickerSet %q = %T, want success", shortName, result)
			}
		}
		all, err := client.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		sets, ok := all.(*tg.MessagesAllStickers)
		if !ok || len(sets.Sets) != len(installedNames) {
			return fmt.Errorf("getAllStickers = %T with %d sets, want both installed bundle packs", all, allStickerCount(all))
		}
		for _, installed := range sets.Sets {
			if _, ok := installedNames[installed.ShortName]; !ok {
				return fmt.Errorf("getAllStickers returned unexpected pack %q", installed.ShortName)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("install and list bundle sticker pack: %v", err)
	}

	document := getSmokeBundleStickerDocument(t, f, a, "openmoji_animals")
	animatedDocument := getSmokeBundleStickerDocument(t, f, a, "noto_animated_smileys_emotion")
	wantBytes := smokeStickerArt("animal")
	animatedBytes := smokeStickerTGS(t)
	if animatedDocument.MimeType != "application/x-tgsticker" || animatedDocument.Size != int64(len(animatedBytes)) {
		t.Fatalf("animated sticker MIME and size = %q / %d, want application/x-tgsticker / %d", animatedDocument.MimeType, animatedDocument.Size, len(animatedBytes))
	}
	var hasAnimatedAttribute, hasImageSize, hasStickerAttribute bool
	var imageWidth, imageHeight int
	for _, attribute := range animatedDocument.Attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeAnimated:
			hasAnimatedAttribute = true
		case *tg.DocumentAttributeImageSize:
			hasImageSize = true
			imageWidth, imageHeight = value.W, value.H
		case *tg.DocumentAttributeSticker:
			hasStickerAttribute = true
		}
	}
	if hasAnimatedAttribute || !hasImageSize || imageWidth != 512 || imageHeight != 512 || !hasStickerAttribute {
		t.Fatalf("animated sticker attributes = %#v, want 512x512 image size and sticker attributes without documentAttributeAnimated", animatedDocument.Attributes)
	}

	transferSmokeBundleSticker(t, f, a, b, document, wantBytes, "static bundle sticker smoke", 1049801)
	transferSmokeBundleSticker(t, f, a, b, animatedDocument, animatedBytes, "animated bundle sticker smoke", 1049802)
}

func loadSmokeStickerBundleCatalogue(t *testing.T) *api.StickerSetCatalog {
	t.Helper()
	dir := t.TempDir()
	packs := []struct {
		shortName, source, license, emoji, filename, format, attribution string
		art                                                              []byte
	}{
		{shortName: "openmoji_animals", source: "OpenMoji Animals", license: "CC-BY-SA-4.0", emoji: "🐈", filename: "animal.webp", attribution: "Artwork by OpenMoji", art: smokeStickerArt("animal")},
		{shortName: "openmoji_food", source: "OpenMoji Food", license: "CC0-1.0", emoji: "🍕", filename: "food.webp", attribution: "Artwork by OpenMoji", art: smokeStickerArt("food")},
		{shortName: "noto_animated_smileys_emotion", source: "Animated Noto Emoji Smileys & Emotion", license: "CC-BY-4.0", emoji: "😀", filename: "grinning_face.tgs", format: "tgs", attribution: "Animated Noto Emoji by Google.", art: smokeStickerTGS(t)},
	}
	manifestPacks := make([]map[string]any, 0, len(packs))
	for _, pack := range packs {
		checksum := sha256.Sum256(pack.art)
		file := filepath.ToSlash(filepath.Join(pack.shortName, pack.filename))
		manifestPack := map[string]any{
			"short_name":  pack.shortName,
			"source":      pack.source,
			"license":     pack.license,
			"attribution": pack.attribution,
			"stickers": []any{map[string]any{
				"emoji": pack.emoji, "file": file, "size": len(pack.art), "sha256": hex.EncodeToString(checksum[:]),
			}},
		}
		if pack.format != "" {
			manifestPack["format"] = pack.format
		}
		manifestPacks = append(manifestPacks, manifestPack)
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

func getSmokeBundleStickerDocument(t *testing.T, f *smokeFixture, owner *smokeClient, shortName string) *tg.Document {
	t.Helper()
	var document *tg.Document
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{
			Stickerset: &tg.InputStickerSetShortName{ShortName: shortName},
		})
		if err != nil {
			return err
		}
		full, ok := result.(*tg.MessagesStickerSet)
		if !ok || len(full.Documents) != 1 || len(full.Packs) != 1 || full.Set.Count != 1 {
			return fmt.Errorf("getStickerSet %q = %T with %d documents and %d packs, want one of each", shortName, result, stickerDocumentCount(full), stickerPackCount(full))
		}
		var okDocument bool
		document, okDocument = full.Documents[0].(*tg.Document)
		if !okDocument {
			return fmt.Errorf("bundle sticker document = %T, want *tg.Document", full.Documents[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("get bundle sticker set %q: %v", shortName, err)
	}
	return document
}

func transferSmokeBundleSticker(
	t *testing.T,
	f *smokeFixture,
	a, b *smokeClient,
	document *tg.Document,
	wantBytes []byte,
	sentText string,
	randomID int64,
) {
	t.Helper()
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
		t.Fatalf("download bundle sticker %q: %v", document.MimeType, err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id),
			Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
				ID: document.ID, AccessHash: document.AccessHash, FileReference: []byte("ignored"),
			}},
			Message: sentText, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("send bundle sticker to second account: %v", err)
	}
	pushed := recvOrCtx(t, f.ctx, b.push.newMsg, "recipient live bundle sticker update")
	if pushed.Message != sentText {
		t.Fatalf("recipient live message = %q, want %q", pushed.Message, sentText)
	}
	pushedMedia, ok := pushed.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("recipient live sticker media = %T, want *tg.MessageMediaDocument", pushed.Media)
	}
	pushedDocument, ok := pushedMedia.Document.(*tg.Document)
	if !ok || pushedDocument.ID != document.ID || pushedDocument.AccessHash != document.AccessHash {
		t.Fatalf("recipient live sticker document = %+v, want catalogue document (%d,%d)", pushedMedia.Document, document.ID, document.AccessHash)
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

func smokeStickerArt(content string) []byte {
	return []byte("RIFF\x00\x00\x00\x00WEBP" + content)
}

func smokeStickerTGS(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(`{"tgs":1,"fr":60,"ip":0,"op":1,"w":512,"h":512,"layers":[],"assets":[]}`)); err != nil {
		t.Fatalf("write smoke TGS file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close smoke TGS file: %v", err)
	}
	return output.Bytes()
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
