package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

// testSmokeAlbumMedia: A stores three photos with uploadMedia and sends them
// to B as one album. Both sides see one album of three sharing one grouped_id
// in the RPC result, B's live push, getDifference and getHistory, and B
// downloads every item.
func testSmokeAlbumMedia(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551048501", "+15551048502"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "album sender", phoneA), newSmokeClient(t, f, "album recipient", phoneB)

	body := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	multi := make([]tg.InputSingleMedia, 3)
	for i := range multi {
		fileID := int64(1048501 + i)
		parts := uploadSmokeMediaParts(t, f.ctx, a, fileID, body, smokePhotoPartSize)
		var uploaded tg.MessageMediaClass
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			uploaded, err = api.MessagesUploadMedia(ctx, &tg.MessagesUploadMediaRequest{
				Peer: peerUser(a.id, b.id),
				Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
					ID: fileID, Parts: parts, Name: fmt.Sprintf("album-%d.jpg", i), MD5Checksum: smokePhotoMD5(body),
				}},
			})
			return err
		}); err != nil {
			t.Fatalf("uploadMedia album photo %d: %v", i, err)
		}
		photo := smokeAlbumPhoto(t, uploaded, fmt.Sprintf("uploaded photo %d", i))
		multi[i] = tg.InputSingleMedia{
			Media:    &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference}},
			RandomID: int64(1048601 + i),
			Message:  fmt.Sprintf("album caption %d", i),
		}
	}

	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMultiMedia(ctx, &tg.MessagesSendMultiMediaRequest{Peer: peerUser(a.id, b.id), MultiMedia: multi})
		return err
	}); err != nil {
		t.Fatalf("sendMultiMedia: %v", err)
	}
	// The RPC result is handed to the client's updates manager, which owns it
	// from then on; the sender's copies are read from what that manager applied.
	sent := make([]tg.MessageClass, 0, 3)
	for range 3 {
		sent = append(sent, recvOrCtx(t, f.ctx, a.seen.newMsg, "album sender update"))
	}
	groupedID := assertSmokeAlbum(t, sent, body, "sender updates", 0)

	pushed := make([]tg.MessageClass, 0, 3)
	for range 3 {
		pushed = append(pushed, recvOrCtx(t, f.ctx, b.push.newMsg, "album live push"))
	}
	assertSmokeAlbum(t, pushed, body, "recipient live push", groupedID)

	var difference tg.UpdatesDifferenceClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		difference, err = api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
		return err
	}); err != nil {
		t.Fatalf("album getDifference: %v", err)
	}
	full, ok := difference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("album difference = %T, want *tg.UpdatesDifference", difference)
	}
	assertSmokeAlbum(t, full.NewMessages, body, "recipient getDifference", groupedID)

	var history tg.MessagesMessagesClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		history, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerUser(b.id, a.id), Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("album getHistory: %v", err)
	}
	page, ok := history.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("album history = %T, want *tg.MessagesMessages", history)
	}
	for _, message := range assertSmokeAlbumMessages(t, page.Messages, body, "recipient getHistory", groupedID) {
		photo := smokeAlbumPhoto(t, message.Media, "recipient history photo")
		original, ok := photo.Sizes[len(photo.Sizes)-1].(*tg.PhotoSize)
		if !ok {
			t.Fatalf("album photo original size = %T, want *tg.PhotoSize", photo.Sizes[len(photo.Sizes)-1])
		}
		var file tg.UploadFileClass
		if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			file, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: original.Type},
				Limit:    len(body),
			})
			return err
		}); err != nil {
			t.Fatalf("recipient album download %d: %v", message.ID, err)
		}
		if got, ok := file.(*tg.UploadFile); !ok || !bytes.Equal(got.Bytes, body) {
			t.Fatalf("recipient album download %d = %T, want the uploaded %d bytes", message.ID, file, len(body))
		}
	}
}

func smokeAlbumPhoto(t *testing.T, media tg.MessageMediaClass, label string) *tg.Photo {
	t.Helper()
	photoMedia, ok := media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("%s media = %T, want *tg.MessageMediaPhoto", label, media)
	}
	photo, ok := photoMedia.Photo.(*tg.Photo)
	if !ok || photo.ID == 0 || photo.AccessHash == 0 || len(photo.Sizes) == 0 {
		t.Fatalf("%s photo = %#v, want an identified photo", label, photoMedia.Photo)
	}
	return photo
}

// assertSmokeAlbum checks that classes hold exactly one album of three photos
// with captions in order and returns its grouped_id; a non-zero want pins it.
func assertSmokeAlbum(t *testing.T, classes []tg.MessageClass, body []byte, label string, want int64) int64 {
	t.Helper()
	messages := assertSmokeAlbumMessages(t, classes, body, label, want)
	groupedID, _ := messages[0].GetGroupedID()
	return groupedID
}

func assertSmokeAlbumMessages(t *testing.T, classes []tg.MessageClass, body []byte, label string, want int64) []*tg.Message {
	t.Helper()
	var album []*tg.Message
	for _, class := range classes {
		if message, ok := class.(*tg.Message); ok && message.Media != nil {
			album = append(album, message)
		}
	}
	if len(album) != 3 {
		t.Fatalf("%s has %d media messages, want an album of 3", label, len(album))
	}
	if album[0].ID > album[len(album)-1].ID {
		for i, j := 0, len(album)-1; i < j; i, j = i+1, j-1 {
			album[i], album[j] = album[j], album[i]
		}
	}
	groupedID, ok := album[0].GetGroupedID()
	if !ok || groupedID == 0 || (want != 0 && groupedID != want) {
		t.Fatalf("%s grouped_id = (%d, %v), want %d", label, groupedID, ok, want)
	}
	for i, message := range album {
		if got, ok := message.GetGroupedID(); !ok || got != groupedID {
			t.Fatalf("%s item %d grouped_id = (%d, %v), want %d", label, i, got, ok, groupedID)
		}
		if message.Message != fmt.Sprintf("album caption %d", i) {
			t.Fatalf("%s item %d caption = %q", label, i, message.Message)
		}
		photo := smokeAlbumPhoto(t, message.Media, label)
		if original, ok := photo.Sizes[len(photo.Sizes)-1].(*tg.PhotoSize); !ok || original.Size != len(body) {
			t.Fatalf("%s item %d original size = %#v, want %d bytes", label, i, photo.Sizes[len(photo.Sizes)-1], len(body))
		}
	}
	return album
}
