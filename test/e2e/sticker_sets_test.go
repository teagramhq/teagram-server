package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

func testSmokeStickerSetCatalogue(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049701", "+15551049702"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "sticker set owner", phoneA)
	a2 := newSmokeClient(t, f, "sticker set owner second session", phoneA)
	b := newSmokeClient(t, f, "sticker set other account", phoneB)

	assertEmpty := func(client *smokeClient, label string) {
		t.Helper()
		if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.MessagesGetAllStickers(ctx, 0)
			if err != nil {
				return err
			}
			all, ok := result.(*tg.MessagesAllStickers)
			if !ok || len(all.Sets) != 0 {
				return fmt.Errorf("%s getAllStickers = %T with %d sets, want full empty result", label, result, lenStickerSets(result))
			}
			return nil
		}); err != nil {
			t.Fatalf("%s sticker sets: %v", label, err)
		}
	}
	assertEmpty(a1, "owner before install")
	assertEmpty(a2, "second owner session before install")
	assertEmpty(b, "other account before install")

	const shortName = "twemoji_reactions"
	ref := &tg.InputStickerSetShortName{ShortName: shortName}
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("installStickerSet result = %T, want success", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("install sticker set: %v", err)
	}
	update := recvOrCtx(t, f.ctx, a2.push.stickerSets, "same-owner UpdateStickerSets")
	if update.Masks || update.Emojis {
		t.Fatalf("UpdateStickerSets flags masks=%t emojis=%t, want no optional payload", update.Masks, update.Emojis)
	}

	var installed tg.StickerSet
	var installedAllHash int64
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		all, ok := result.(*tg.MessagesAllStickers)
		if !ok || len(all.Sets) != 1 {
			return fmt.Errorf("second session getAllStickers = %T, want one installed set", result)
		}
		installed = all.Sets[0]
		installedAllHash = all.Hash
		if installed.ShortName != shortName || installed.Count != 73 || installed.ID == 0 || installed.AccessHash == 0 {
			return fmt.Errorf("installed set = %+v, want %s with 73 stickers", installed, shortName)
		}
		return nil
	}); err != nil {
		t.Fatalf("list installed sticker set from second session: %v", err)
	}
	assertEmpty(b, "other account after install")

	var firstDocument *tg.Document
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		full, ok := result.(*tg.MessagesStickerSet)
		if !ok || len(full.Documents) != 73 || len(full.Packs) != 73 || full.Set.Count != 73 {
			return fmt.Errorf("getStickerSet result = %T, want set with 73 documents and packs", result)
		}
		byID, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{
			Stickerset: &tg.InputStickerSetID{ID: full.Set.ID, AccessHash: full.Set.AccessHash},
		})
		if err != nil {
			return fmt.Errorf("getStickerSet by id: %w", err)
		}
		if _, ok := byID.(*tg.MessagesStickerSet); !ok {
			return fmt.Errorf("getStickerSet by id = %T, want full set", byID)
		}
		var okDocument bool
		firstDocument, okDocument = full.Documents[0].(*tg.Document)
		if !okDocument {
			return fmt.Errorf("first sticker document = %T, want *tg.Document", full.Documents[0])
		}
		if firstDocument.MimeType != "image/webp" || firstDocument.Size <= 0 {
			return fmt.Errorf("first sticker MIME %q size %d, want a non-empty WebP", firstDocument.MimeType, firstDocument.Size)
		}
		return nil
	}); err != nil {
		t.Fatalf("get sticker set: %v", err)
	}
	if firstDocument == nil {
		t.Fatal("getStickerSet returned no first sticker document")
	}
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID: firstDocument.ID, AccessHash: firstDocument.AccessHash, FileReference: firstDocument.FileReference,
			},
			Offset: 0,
			Limit:  262144,
		})
		if err != nil {
			return err
		}
		file, ok := result.(*tg.UploadFile)
		if !ok || int64(len(file.Bytes)) != firstDocument.Size {
			return fmt.Errorf("sticker upload.getFile = %T with %d bytes, want %d bytes", result, lenUploadBytes(result), firstDocument.Size)
		}
		if len(file.Bytes) < 12 || !bytes.Equal(file.Bytes[:4], []byte("RIFF")) || !bytes.Equal(file.Bytes[8:12], []byte("WEBP")) {
			return fmt.Errorf("sticker download lacks WebP magic bytes: %x", file.Bytes[:min(len(file.Bytes), 12)])
		}
		return nil
	}); err != nil {
		t.Fatalf("download sticker WebP: %v", err)
	}

	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		all, err := api.MessagesGetAllStickers(ctx, installedAllHash)
		if err != nil {
			return err
		}
		if _, ok := all.(*tg.MessagesAllStickersNotModified); !ok {
			return fmt.Errorf("getAllStickers with current hash = %T, want notModified", all)
		}
		setResult, err := api.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{
			Stickerset: ref,
			Hash:       installed.Hash,
		})
		if err != nil {
			return err
		}
		if _, ok := setResult.(*tg.MessagesStickerSetNotModified); !ok {
			return fmt.Errorf("getStickerSet with current hash = %T, want notModified", setResult)
		}
		return nil
	}); err != nil {
		t.Fatalf("check sticker set cache hashes: %v", err)
	}

	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		removed, err := api.MessagesUninstallStickerSet(ctx, ref)
		if err != nil {
			return err
		}
		if !removed {
			return errors.New("uninstallStickerSet returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("uninstall sticker set: %v", err)
	}
	_ = recvOrCtx(t, f.ctx, a2.push.stickerSets, "same-owner sticker set removal update")
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		all, ok := result.(*tg.MessagesAllStickers)
		if !ok || len(all.Sets) != 0 || all.Hash == installedAllHash {
			return fmt.Errorf("getAllStickers after uninstall = %T with %d sets and hash %d, want empty changed hash", result, lenStickerSets(result), allHash(result))
		}
		return nil
	}); err != nil {
		t.Fatalf("check sticker set removal from second session: %v", err)
	}
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		removed, err := api.MessagesUninstallStickerSet(ctx, ref)
		if err != nil {
			return err
		}
		if !removed {
			return errors.New("idempotent uninstallStickerSet returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("repeat sticker set uninstall: %v", err)
	}
}

func lenStickerSets(result tg.MessagesAllStickersClass) int {
	all, ok := result.(*tg.MessagesAllStickers)
	if !ok {
		return 0
	}
	return len(all.Sets)
}

func allHash(result tg.MessagesAllStickersClass) int64 {
	all, ok := result.(*tg.MessagesAllStickers)
	if !ok {
		return 0
	}
	return all.Hash
}

func lenUploadBytes(result tg.UploadFileClass) int {
	file, ok := result.(*tg.UploadFile)
	if !ok {
		return 0
	}
	return len(file.Bytes)
}
