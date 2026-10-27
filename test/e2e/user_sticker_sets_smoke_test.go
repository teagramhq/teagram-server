package e2e_test

import (
	"bytes"
	"compress/gzip"
	"context"
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
	const uploadID = int64(1049911)
	parts := uploadSmokeMediaParts(t, f.ctx, owner, uploadID, stickerBytes, len(stickerBytes))
	var document *tg.Document
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
			Peer: &tg.InputPeerSelf{},
			Media: &tg.InputMediaUploadedDocument{
				File:       &tg.InputFile{ID: uploadID, Parts: parts, Name: "star.tgs"},
				MimeType:   "application/x-tgsticker",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: "star.tgs"}, &tg.DocumentAttributeAnimated{}},
			},
		})
		if err != nil {
			return err
		}
		media, ok := result.(*tg.MessageMediaDocument)
		if !ok {
			return fmt.Errorf("uploadMedia result = %T, want *tg.MessageMediaDocument", result)
		}
		var okDocument bool
		document, okDocument = media.Document.(*tg.Document)
		if !okDocument || document.ID <= 0 || document.AccessHash == 0 {
			return fmt.Errorf("uploaded sticker document = %#v, want stored document identity", media.Document)
		}
		return nil
	}); err != nil {
		t.Fatalf("upload custom emoji document: %v", err)
	}

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
