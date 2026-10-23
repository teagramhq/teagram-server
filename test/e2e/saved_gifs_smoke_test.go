package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

func testSmokeSavedGIFs(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const senderPhone, ownerPhone = "+15551048501", "+15551048502"
	seedSmokeUsers(t, f, senderPhone, ownerPhone)
	sender := newSmokeClient(t, f, "saved GIF sender", senderPhone)
	ownerA := newSmokeClient(t, f, "saved GIF first session", ownerPhone)

	const (
		fileID   = int64(1048501)
		randomID = int64(1048502)
		caption  = "received animated GIF smoke"
	)
	body := []byte("saved GIF smoke payload")
	if err := sender.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: 0, Bytes: body,
		})
		if err != nil {
			return fmt.Errorf("upload GIF body: %w", err)
		}
		if !ok {
			return errors.New("upload GIF body returned false")
		}
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(sender.id, ownerA.id),
			Media: &tg.InputMediaUploadedDocument{
				File:       &tg.InputFile{ID: fileID, Parts: 1, Name: "smoke.gif"},
				MimeType:   "video/mp4",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}},
			},
			Message: caption, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("send animated GIF: %v", err)
	}

	received := recvOrCtx(t, ownerA.lifecycle.ctx, ownerA.seen.newMsg, "received saved GIF source")
	if received.Message != caption {
		t.Fatalf("received message = %q, want %q", received.Message, caption)
	}
	messageDocument := documentOf(t, received)
	if !hasAnimatedDocumentAttribute(messageDocument.Attributes) {
		t.Fatalf("received document attributes = %#v, want animated", messageDocument.Attributes)
	}

	var saved bool
	if err := ownerA.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		saved, err = api.MessagesSaveGif(ctx, &tg.MessagesSaveGifRequest{
			ID: &tg.InputDocument{
				ID: messageDocument.ID, AccessHash: messageDocument.AccessHash,
				FileReference: messageDocument.FileReference,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("save GIF from received message: %v", err)
	}
	if !saved {
		t.Fatal("messages.saveGif returned false")
	}

	ownerB := newSmokeClient(t, f, "saved GIF fresh session", ownerPhone)
	if ownerB.session == ownerA.session {
		t.Fatal("saved GIF sessions share session storage")
	}
	if err := ownerB.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetSavedGifs(ctx, 0)
		if err != nil {
			return err
		}
		listed, ok := result.(*tg.MessagesSavedGifs)
		if !ok || len(listed.Gifs) != 1 || listed.Hash == 0 {
			return fmt.Errorf("getSavedGifs response = %#v, want one GIF and a stable hash", result)
		}
		savedDocument, ok := listed.Gifs[0].(*tg.Document)
		if !ok || savedDocument.ID != messageDocument.ID || !hasAnimatedDocumentAttribute(savedDocument.Attributes) {
			return fmt.Errorf("saved GIF document = %#v, want animated document %d", listed.Gifs[0], messageDocument.ID)
		}
		result, err = api.MessagesGetSavedGifs(ctx, listed.Hash)
		if err != nil {
			return err
		}
		if _, ok := result.(*tg.MessagesSavedGifsNotModified); !ok {
			return fmt.Errorf("same-hash getSavedGifs response = %T, want savedGifsNotModified", result)
		}
		fileResult, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID: savedDocument.ID, AccessHash: savedDocument.AccessHash,
				FileReference: savedDocument.FileReference,
			},
			Offset: 0, Limit: len(body),
		})
		if err != nil {
			return fmt.Errorf("download saved GIF: %w", err)
		}
		file, ok := fileResult.(*tg.UploadFile)
		if !ok || !bytes.Equal(file.Bytes, body) {
			return fmt.Errorf("downloaded saved GIF = %T %q, want original bytes %q", fileResult, file.Bytes, body)
		}
		return nil
	}); err != nil {
		t.Fatalf("read saved GIF from fresh session: %v", err)
	}
}

func testSmokeSavedGIFRequestFixtures(t *testing.T, f *smokeFixture, sender, owner *smokeClient) {
	t.Helper()
	const (
		fileID   = int64(10578001)
		randomID = int64(10578002)
		caption  = "fixture animated GIF"
	)
	body := []byte("fixture GIF bytes")
	if err := sender.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: 0, Bytes: body,
		})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("fixture GIF upload returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("upload fixture GIF: %v", err)
	}
	if err := fixtureClientCall(t, sender, "messages_sendMedia.gif.1", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(sender.id, owner.id),
			Media: &tg.InputMediaUploadedDocument{
				File:       &tg.InputFile{ID: fileID, Parts: 1, Name: "smoke.gif"},
				MimeType:   "video/mp4",
				Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeAnimated{}},
			},
			Message: caption, RandomID: randomID,
		})
		return err
	}); err != nil {
		t.Fatalf("replay animated upload fixture: %v", err)
	}
	received := recvMessageWithText(t, owner, caption, "animated GIF fixture receive")
	document := documentOf(t, received)

	if err := fixtureClientCall(t, owner, "messages_saveGif.1", func(ctx context.Context, api *tg.Client) error {
		saved, err := api.MessagesSaveGif(ctx, &tg.MessagesSaveGifRequest{
			ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
		})
		if err != nil {
			return err
		}
		if !saved {
			return errors.New("messages.saveGif returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay messages.saveGif fixture: %v", err)
	}

	if err := fixtureClientCall(t, owner, "messages_getSavedGifs.1", func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetSavedGifs(ctx, 0)
		if err != nil {
			return err
		}
		listed, ok := result.(*tg.MessagesSavedGifs)
		if !ok || len(listed.Gifs) != 1 {
			return fmt.Errorf("messages.getSavedGifs fixture result = %#v, want one saved GIF", result)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay messages.getSavedGifs fixture: %v", err)
	}

	if err := fixtureClientCall(t, owner, "messages_saveGif.2", func(ctx context.Context, api *tg.Client) error {
		saved, err := api.MessagesSaveGif(ctx, &tg.MessagesSaveGifRequest{
			ID:     &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash, FileReference: document.FileReference},
			Unsave: true,
		})
		if err != nil {
			return err
		}
		if !saved {
			return errors.New("messages.saveGif unsave returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("replay messages.saveGif unsave fixture: %v", err)
	}
}

func recvMessageWithText(t *testing.T, client *smokeClient, text, what string) *tg.Message {
	t.Helper()
	for {
		message := recvOrCtx(t, client.lifecycle.ctx, client.seen.newMsg, what)
		if message.Message == text {
			return message
		}
	}
}

func hasAnimatedDocumentAttribute(attributes []tg.DocumentAttributeClass) bool {
	for _, attribute := range attributes {
		if _, ok := attribute.(*tg.DocumentAttributeAnimated); ok {
			return true
		}
	}
	return false
}
