package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func testSmokeUserCreatedStickerSets(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, nil)
	const phone = "+15551049911"
	seedSmokeUsers(t, f, phone)
	recorder := &clientRequestRecorder{}
	owner := newSmokeClientWithRequests(t, f, "user-created sticker set owner", phone, recorder)

	stickerBytes := smokeCustomEmojiTGS(t)
	document := uploadSmokeStickerDocument(t, f, owner, 1049911, "star.tgs", stickerBytes)

	request := &tg.StickersCreateStickerSetRequest{
		UserID: &tg.InputUserSelf{}, Title: "Smoke emoji set", ShortName: "smoke_custom_emoji",
		Emojis: true, TextColor: true,
		Stickers: []tg.InputStickerSetItem{{
			Document: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
			Emoji:    "⭐",
		}},
	}
	request.SetSoftware("teagram-smoke")
	var created *tg.MessagesStickerSet
	if err := fixtureClientCall(t, owner, "stickers_createStickerSet.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersCreateStickerSet(ctx, request)
		if err != nil {
			return err
		}
		var ok bool
		created, ok = result.(*tg.MessagesStickerSet)
		if !ok || created.Set.ShortName != request.ShortName || created.Set.Count != 1 || !created.Set.Creator {
			return fmt.Errorf("createStickerSet result = %#v, want owner set %q with one sticker", result, request.ShortName)
		}
		return nil
	}); err != nil {
		t.Fatalf("create custom emoji set with captured client request: %v", err)
	}
	if !created.Set.Emojis || !created.Set.TextColor || len(created.Documents) != 1 {
		t.Fatalf("created custom emoji set = %+v with %d documents, want emoji/text-color set with one sticker", created.Set, len(created.Documents))
	}
	ref := &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}
	var initialEmojiHash int64
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesInstallStickerSet(ctx, &tg.MessagesInstallStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesStickerSetInstallResultSuccess); !ok {
			return fmt.Errorf("installStickerSet result = %T, want success", result)
		}
		emojiResult, err := client.MessagesGetEmojiStickers(ctx, 0)
		if err != nil {
			return err
		}
		emojis, ok := emojiResult.(*tg.MessagesAllStickers)
		if !ok || len(emojis.Sets) != 1 || emojis.Sets[0].ID != created.Set.ID {
			return fmt.Errorf("getEmojiStickers result = %#v, want created set %d", emojiResult, created.Set.ID)
		}
		initialEmojiHash = emojis.Hash
		allResult, err := client.MessagesGetAllStickers(ctx, 0)
		if err != nil {
			return err
		}
		all, ok := allResult.(*tg.MessagesAllStickers)
		if !ok || len(all.Sets) != 0 {
			return fmt.Errorf("getAllStickers result = %#v, want custom emoji set filtered out", allResult)
		}
		return nil
	}); err != nil {
		t.Fatalf("install and list custom emoji set: %v", err)
	}
	added := uploadSmokeStickerDocument(t, f, owner, 1049912, "sparkle.tgs", stickerBytes)
	editSmokeCustomEmojiStickerSet(t, f, owner, created, document, added, initialEmojiHash)
}

func editSmokeCustomEmojiStickerSet(t *testing.T, f *smokeFixture, owner *smokeClient, created *tg.MessagesStickerSet, original, added *tg.Document, initialHash int64) {
	t.Helper()
	ref := &tg.InputStickerSetID{ID: created.Set.ID, AccessHash: created.Set.AccessHash}
	addedRef := &tg.InputDocument{ID: added.ID, AccessHash: added.AccessHash, FileReference: added.FileReference}
	originalRef := &tg.InputDocument{ID: original.ID, AccessHash: original.AccessHash, FileReference: original.FileReference}
	var updated *tg.MessagesStickerSet
	assertSetResult := func(result tg.MessagesStickerSetClass, err error, method string, wantCount int) error {
		if err != nil {
			return err
		}
		var ok bool
		updated, ok = result.(*tg.MessagesStickerSet)
		if !ok || updated.Set.Count != wantCount || len(updated.Documents) != wantCount {
			return fmt.Errorf("%s result = %#v, want messages.stickerSet with %d documents", method, result, wantCount)
		}
		return nil
	}

	add := tg.InputStickerSetItem{Document: addedRef, Emoji: "⭐"}
	if err := fixtureClientCall(t, owner, "stickers_addStickerToSet.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersAddStickerToSet(ctx, &tg.StickersAddStickerToSetRequest{Stickerset: ref, Sticker: add})
		return assertSetResult(result, err, "addStickerToSet", 2)
	}); err != nil {
		t.Fatalf("addStickerToSet smoke request: %v", err)
	}
	if err := fixtureClientCall(t, owner, "stickers_changeStickerPosition.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersChangeStickerPosition(ctx, &tg.StickersChangeStickerPositionRequest{Sticker: addedRef, Position: 0})
		if err = assertSetResult(result, err, "changeStickerPosition", 2); err != nil {
			return err
		}
		first, ok := updated.Documents[0].(*tg.Document)
		if !ok || first.ID != added.ID {
			return fmt.Errorf("changeStickerPosition first document = %#v, want %d", updated.Documents[0], added.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("changeStickerPosition smoke request: %v", err)
	}
	change := &tg.StickersChangeStickerRequest{Sticker: addedRef}
	change.SetEmoji("🌟")
	change.SetKeywords("star,glow")
	if err := fixtureClientCall(t, owner, "stickers_changeSticker.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersChangeSticker(ctx, change)
		if err = assertSetResult(result, err, "changeSticker", 2); err != nil {
			return err
		}
		if len(updated.Keywords) != 1 || updated.Keywords[0].DocumentID != added.ID {
			return fmt.Errorf("changeSticker keywords = %+v, want added sticker %d", updated.Keywords, added.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("changeSticker smoke request: %v", err)
	}
	thumb := &tg.StickersSetStickerSetThumbRequest{Stickerset: ref}
	thumb.SetThumbDocumentID(added.ID)
	if err := fixtureClientCall(t, owner, "stickers_setStickerSetThumb.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersSetStickerSetThumb(ctx, thumb)
		if err = assertSetResult(result, err, "setStickerSetThumb", 2); err != nil {
			return err
		}
		if updated.Set.ThumbDocumentID != added.ID {
			return fmt.Errorf("setStickerSetThumb document id = %d, want %d", updated.Set.ThumbDocumentID, added.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("setStickerSetThumb smoke request: %v", err)
	}
	if err := fixtureClientCall(t, owner, "stickers_removeStickerFromSet.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersRemoveStickerFromSet(ctx, originalRef)
		return assertSetResult(result, err, "removeStickerFromSet", 1)
	}); err != nil {
		t.Fatalf("removeStickerFromSet smoke request: %v", err)
	}
	if err := fixtureClientCall(t, owner, "stickers_renameStickerSet.1", func(ctx context.Context, client *tg.Client) error {
		result, err := client.StickersRenameStickerSet(ctx, &tg.StickersRenameStickerSetRequest{Stickerset: ref, Title: "Edited smoke set"})
		if err = assertSetResult(result, err, "renameStickerSet", 1); err != nil {
			return err
		}
		if updated.Set.Title != "Edited smoke set" {
			return fmt.Errorf("renameStickerSet title = %q, want Edited smoke set", updated.Set.Title)
		}
		return nil
	}); err != nil {
		t.Fatalf("renameStickerSet smoke request: %v", err)
	}
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetStickerSet(ctx, &tg.MessagesGetStickerSetRequest{Stickerset: ref})
		if err != nil {
			return err
		}
		full, ok := result.(*tg.MessagesStickerSet)
		if !ok {
			return fmt.Errorf("installer getStickerSet after edits = %T, want *tg.MessagesStickerSet", result)
		}
		if full.Set.Title != "Edited smoke set" || full.Set.Count != 1 || len(full.Documents) != 1 {
			return fmt.Errorf("installer getStickerSet after edits = %#v, want renamed one-sticker set", result)
		}
		document, ok := full.Documents[0].(*tg.Document)
		if !ok || document.ID != added.ID {
			return fmt.Errorf("installer getStickerSet document = %#v, want %d", full.Documents[0], added.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("installer getStickerSet after edits: %v", err)
	}
	var editedEmojiHash int64
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetEmojiStickers(ctx, initialHash)
		if err != nil {
			return err
		}
		list, ok := result.(*tg.MessagesAllStickers)
		if !ok || list.Hash == initialHash || len(list.Sets) != 1 || list.Sets[0].Count != 1 {
			return fmt.Errorf("installer getEmojiStickers after edits = %#v, want changed one-set list", result)
		}
		editedEmojiHash = list.Hash
		return nil
	}); err != nil {
		t.Fatalf("installer getEmojiStickers after edits: %v", err)
	}
	if err := fixtureClientCall(t, owner, "stickers_deleteStickerSet.1", func(ctx context.Context, client *tg.Client) error {
		deleted, err := client.StickersDeleteStickerSet(ctx, ref)
		if err != nil {
			return err
		}
		if !deleted {
			return errors.New("deleteStickerSet returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("deleteStickerSet smoke request: %v", err)
	}
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetEmojiStickers(ctx, editedEmojiHash)
		if err != nil {
			return err
		}
		list, ok := result.(*tg.MessagesAllStickers)
		if !ok || len(list.Sets) != 0 || list.Hash == editedEmojiHash {
			return fmt.Errorf("installer getEmojiStickers after deletion = %#v, want changed empty list", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("installer getEmojiStickers after deletion: %v", err)
	}
}

func uploadSmokeStickerDocument(t *testing.T, f *smokeFixture, owner *smokeClient, uploadID int64, name string, data []byte) *tg.Document {
	t.Helper()
	parts := uploadSmokeMediaParts(t, f.ctx, owner, uploadID, data, len(data))
	var document *tg.Document
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
			Peer: &tg.InputPeerSelf{},
			Media: &tg.InputMediaUploadedDocument{
				File:       &tg.InputFile{ID: uploadID, Parts: parts, Name: name},
				MimeType:   "application/x-tgsticker",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}, &tg.DocumentAttributeAnimated{}},
			},
		})
		if err != nil {
			return err
		}
		media, ok := result.(*tg.MessageMediaDocument)
		if !ok {
			return fmt.Errorf("uploadMedia result = %T, want *tg.MessageMediaDocument", result)
		}
		document, ok = media.Document.(*tg.Document)
		if !ok || document.ID <= 0 || document.AccessHash == 0 {
			return fmt.Errorf("uploaded sticker document = %#v, want stored document identity", media.Document)
		}
		return nil
	}); err != nil {
		t.Fatalf("upload sticker document %q: %v", name, err)
	}
	return document
}

func smokeCustomEmojiTGS(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write([]byte(`{"tgs":1,"fr":60,"ip":0,"op":1,"w":100,"h":100,"layers":[],"assets":[]}`)); err != nil {
		t.Fatalf("write smoke custom emoji TGS file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close smoke custom emoji TGS file: %v", err)
	}
	if output.Len() == 0 {
		t.Fatal("smoke custom emoji TGS file is empty")
	}
	return output.Bytes()
}
