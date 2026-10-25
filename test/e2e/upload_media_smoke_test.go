package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func testSmokeUploadMedia(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551048401", "+15551048402"
	seedSmokeUsers(t, f, phoneA, phoneB)
	uploader, peer := newSmokeClient(t, f, "upload-media sender", phoneA), newSmokeClient(t, f, "upload-media peer", phoneB)
	assertSmokeUploadPartLimits(t, f.ctx, uploader, 200)

	documentBody := []byte("uploadMedia smoke document")
	const documentFileID = int64(1048401)
	documentParts := uploadSmokeMediaParts(t, f.ctx, uploader, documentFileID, documentBody, len(documentBody))
	var documentResult tg.MessageMediaClass
	if err := uploader.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		documentResult, err = api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
			Peer: peerUser(uploader.id, peer.id),
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: documentFileID, Parts: documentParts, Name: "upload-media-smoke.txt"},
				MimeType: "text/plain",
			},
		})
		return err
	}); err != nil {
		t.Fatalf("messages.uploadMedia document: %v", err)
	}
	documentMedia, ok := documentResult.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("uploadMedia document media = %T, want *tg.MessageMediaDocument", documentResult)
	}
	document, ok := documentMedia.Document.(*tg.Document)
	if !ok || document.ID <= 0 || document.AccessHash == 0 || len(document.FileReference) != 8 || document.Size != int64(len(documentBody)) || document.MimeType != "text/plain" {
		t.Fatalf("uploadMedia document = %#v, want stored text/plain metadata for %d bytes", documentMedia.Document, len(documentBody))
	}
	if len(document.Attributes) != 1 {
		t.Fatalf("uploadMedia document attributes = %#v, want filename", document.Attributes)
	}
	filename, ok := document.Attributes[0].(*tg.DocumentAttributeFilename)
	if !ok || filename.FileName != "upload-media-smoke.txt" {
		t.Fatalf("uploadMedia document filename = %#v, want upload-media-smoke.txt", document.Attributes[0])
	}
	assertSmokeUploadMediaDownloadRefused(t, f.ctx, uploader, document)
	assertSmokeUploadMediaDownloadRefused(t, f.ctx, peer, document)

	photoBody := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const photoFileID = int64(1048402)
	photoParts := uploadSmokeMediaParts(t, f.ctx, uploader, photoFileID, photoBody, smokePhotoPartSize)
	var photoResult tg.MessageMediaClass
	if err := uploader.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		photoResult, err = api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
			Peer: peerUser(uploader.id, peer.id),
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: photoFileID, Parts: photoParts, Name: "upload-media-smoke.jpg", MD5Checksum: smokePhotoMD5(photoBody),
			}},
		})
		return err
	}); err != nil {
		t.Fatalf("messages.uploadMedia photo: %v", err)
	}
	photoMedia, ok := photoResult.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("uploadMedia photo media = %T, want *tg.MessageMediaPhoto", photoResult)
	}
	photo, ok := photoMedia.Photo.(*tg.Photo)
	if !ok || photo.ID <= 0 || photo.AccessHash == 0 || len(photo.FileReference) != 8 || photo.DCID != f.dcID || len(photo.Sizes) == 0 {
		t.Fatalf("uploadMedia photo = %#v, want photo id, access hash, reference and size", photoMedia.Photo)
	}
	original, ok := photo.Sizes[len(photo.Sizes)-1].(*tg.PhotoSize)
	if !ok || original.Size != len(photoBody) || original.W != 1600 || original.H != 1600 {
		t.Fatalf("uploadMedia photo original size = %#v, want 1600x1600/%d", photo.Sizes[len(photo.Sizes)-1], len(photoBody))
	}
	assertSmokeUploadMediaDownloadRefused(t, f.ctx, uploader, photo)
	assertSmokeUploadMediaDownloadRefused(t, f.ctx, peer, photo)
}

func assertSmokeUploadPartLimits(t *testing.T, ctx context.Context, client *smokeClient, want int) {
	t.Helper()
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.HelpGetAppConfig(ctx, 0)
		if err != nil {
			return err
		}
		appConfig, ok := result.(*tg.HelpAppConfig)
		if !ok {
			return fmt.Errorf("help.getAppConfig = %T, want *tg.HelpAppConfig", result)
		}
		object, ok := appConfig.Config.(*tg.JSONObject)
		if !ok {
			return fmt.Errorf("app config = %T, want *tg.JSONObject", appConfig.Config)
		}
		limits := map[string]bool{
			"upload_max_fileparts_default": false,
			"upload_max_fileparts_premium": false,
		}
		for _, value := range object.Value {
			_, found := limits[value.Key]
			if !found {
				continue
			}
			number, ok := value.Value.(*tg.JSONNumber)
			if !ok || number.Value != float64(want) {
				return fmt.Errorf("%s = %#v, want %d", value.Key, value.Value, want)
			}
			limits[value.Key] = true
		}
		for key, found := range limits {
			if !found {
				return fmt.Errorf("help.getAppConfig omitted %s", key)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("help.getAppConfig upload limits: %v", err)
	}
}

func uploadSmokeMediaParts(t *testing.T, ctx context.Context, client *smokeClient, fileID int64, body []byte, partSize int) int {
	t.Helper()
	parts := 0
	for offset := 0; offset < len(body); offset += partSize {
		part, chunk := parts, body[offset:min(offset+partSize, len(body))]
		if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
			ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: fileID, FilePart: part, Bytes: chunk})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("saveFilePart %d returned false", part)
			}
			return nil
		}); err != nil {
			t.Fatalf("save uploadMedia smoke part %d: %v", part, err)
		}
		parts++
	}
	return parts
}

func assertSmokeUploadMediaDownloadRefused(t *testing.T, ctx context.Context, client *smokeClient, media any) {
	t.Helper()
	err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var location tg.InputFileLocationClass
		switch item := media.(type) {
		case *tg.Document:
			location = &tg.InputDocumentFileLocation{ID: item.ID, AccessHash: item.AccessHash, FileReference: item.FileReference}
		case *tg.Photo:
			location = &tg.InputPhotoFileLocation{ID: item.ID, AccessHash: item.AccessHash, FileReference: item.FileReference, ThumbSize: "x"}
		default:
			return fmt.Errorf("unsupported uploadMedia result %T", media)
		}
		_, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{Location: location, Offset: 0, Limit: 1})
		return err
	})
	if !tgerr.Is(err, "LOCATION_INVALID") {
		t.Fatalf("upload.getFile for unsent %T = %v, want LOCATION_INVALID", media, err)
	}
}
