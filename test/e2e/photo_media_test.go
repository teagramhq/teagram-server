package e2e_test

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Telegram inputFile requires MD5 for uploaded photos.
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	"github.com/gotd/td/tg"
)

const (
	smokePhotoPartSize  = 32 * 1024
	smokePhotoPartCount = 8
)

func testSmokePhotoMedia(t *testing.T) {
	t.Helper()
	fixture := readSmokePhotoRequestFixture(t, "photo-media.request-fixture")
	body := smokeJPEGAtUploadSize(t, fixture.Request.Fields.Media.Fields.File.Fields.Parts, "photo-media.synthetic-image", true)
	checksum := smokePhotoMD5(body)
	f := newSmokeFixtureWithDiagnosticID(t, "photo-media.fixture-setup")
	const phoneA, phoneB = "+15551048001", "+15551048002"
	seedPhoneUsersWithDiagnosticID(t, f.ctx, f.store, "photo-media.user-seed", phoneA, phoneB)
	a := newSmokeClientWithDiagnosticID(t, f, "Photo sender", phoneA, "photo-media.sender-client")
	b := newSmokeClientWithDiagnosticID(t, f, "Photo recipient", phoneB, "photo-media.recipient-client")

	const privateFileID, privateRandomID = int64(1048001), int64(1048002)
	var privateResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for part := range fixture.Request.Fields.Media.Fields.File.Fields.Parts {
			start := part * smokePhotoPartSize
			ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: privateFileID, FilePart: part, Bytes: body[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return fmt.Errorf("upload private photo part %d: %w", part, err)
			}
			if !ok {
				return fmt.Errorf("upload private photo part %d returned false", part)
			}
		}
		var err error
		privateResult, err = client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id),
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: privateFileID, Parts: fixture.Request.Fields.Media.Fields.File.Fields.Parts,
				Name: "219343.jpg", MD5Checksum: checksum,
			}},
			Message: fixture.Request.Fields.Message, RandomID: privateRandomID,
		})
		return err
	}); err != nil {
		t.Fatalf("[assert:photo-media.private-photo-send] send private photo using captured layer-%d request: %v", fixture.Layer, err)
	}
	privateMessage := outgoingPhotoMessage(t, privateResult, "photo-media.private-message")
	privatePhoto := assertSmokePhoto(t, privateMessage, body, "photo-media.private-photo-shape", true)
	assertSmokePhotoUpdate(t, f.ctx, b.seen, privatePhoto, body, a.id, false, 1, "private live update", "photo-media.private-live-update")
	assertSmokePhotoUpdate(t, f.ctx, b.push, privatePhoto, body, a.id, false, 1, "private push update", "photo-media.private-push-update")

	var difference tg.UpdatesDifferenceClass
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		difference, err = client.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
		return err
	}); err != nil {
		t.Fatalf("[assert:photo-media.private-difference] get photo difference: %v", err)
	}
	fullDifference, ok := difference.(*tg.UpdatesDifference)
	if !ok || len(fullDifference.NewMessages) != 1 {
		t.Fatalf("[assert:photo-media.private-difference-shape] photo difference = %T with %d messages, want one message", difference, differenceMessageCount(difference))
	}
	differenceMessage, ok := fullDifference.NewMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("[assert:photo-media.private-difference-message] photo difference message = %T, want *tg.Message", fullDifference.NewMessages[0])
	}
	assertSmokeSamePhoto(t, differenceMessage, privatePhoto, body, "private difference", "photo-media.private-difference-photo", true, true)

	privateHistory := smokeHistoryMessageForPhoto(t, f.ctx, b, peerUser(b.id, a.id), "private photo history", "photo-media.private-history")
	assertSmokeSamePhoto(t, privateHistory, privatePhoto, body, "private history", "photo-media.private-history-photo", true, true)
	assertSmokePhotoDownload(t, f.ctx, b, privatePhoto, body, "private recipient", "photo-media.private-download", true)

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		created, err := client.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Photo smoke group", Users: []tg.InputUserClass{inputUser(a.id, b.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChat result = %T with %d chats, want one chat", created.Updates, len(updates.Chats))
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("[assert:photo-media.group-create] create photo smoke group: %v", err)
	}

	const groupFileID, groupRandomID = int64(1048003), int64(1048004)
	var groupResult tg.UpdatesClass
	// The inputFile MD5 is optional; this full group send still validates and
	// returns the uploaded JPEG when the client omits it.
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for part := range fixture.Request.Fields.Media.Fields.File.Fields.Parts {
			start := part * smokePhotoPartSize
			ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: groupFileID, FilePart: part, Bytes: body[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return fmt.Errorf("upload group photo part %d: %w", part, err)
			}
			if !ok {
				return fmt.Errorf("upload group photo part %d returned false", part)
			}
		}
		var err error
		groupResult, err = client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: groupFileID, Parts: fixture.Request.Fields.Media.Fields.File.Fields.Parts,
				Name: "219343.jpg",
			}},
			Message: fixture.Request.Fields.Message, RandomID: groupRandomID,
		})
		return err
	}); err != nil {
		t.Fatalf("[assert:photo-media.group-photo-send] send basic-group photo: %v", err)
	}
	groupMessage := outgoingPhotoMessage(t, groupResult, "photo-media.group-message")
	groupPhoto := assertSmokePhoto(t, groupMessage, body, "photo-media.group-photo-shape", true)
	assertSmokePhotoUpdate(t, f.ctx, b.seen, groupPhoto, body, chatID, true, 3, "group live update", "photo-media.group-live-update")
	assertSmokePhotoUpdate(t, f.ctx, b.push, groupPhoto, body, chatID, true, 3, "group push update", "photo-media.group-push-update")
	groupHistory := smokeHistoryMessageForPhoto(t, f.ctx, b, &tg.InputPeerChat{ChatID: chatID}, "group photo history", "photo-media.group-history")
	assertSmokeSamePhoto(t, groupHistory, groupPhoto, body, "group history", "photo-media.group-history-photo", true, true)
	assertSmokePhotoDownload(t, f.ctx, b, groupPhoto, body, "group recipient", "photo-media.group-download", true)

	smokeChannelPhotoLegs(t, f, a, b, fixture.Request.Fields.Media.Fields.File.Fields.Parts, body, "photo-media.channel-legs")
}

// smokeChannelPhotoLegs is the channel half of the photo smoke scenario: a
// broadcast creator posts and a subscriber reads the same original, and a
// megagroup member posts with a caption and the creator reads it back. Each leg
// asserts the live update, the history read and a byte-identical download.
func smokeChannelPhotoLegs(t *testing.T, f *smokeFixture, a, b *smokeClient, parts int, body []byte, diagnosticID string) {
	t.Helper()
	checksum := smokePhotoMD5(body)

	broadcastID := smokeCreateChannel(t, f, a, "Photo smoke channel", true, false, diagnosticID)
	smokeJoinChannel(t, f, broadcastID, a.id, b.id, diagnosticID)
	broadcastPost := smokeSendChannelPhoto(t, f, a, broadcastID, 1048005, 1048006, parts, checksum, "channel caption", diagnosticID)
	if broadcastPost.Message != "channel caption" {
		smokeFailuref(t, diagnosticID, "broadcast photo caption = %q, want the posted caption", broadcastPost.Message)
	}
	broadcastPhoto := assertSmokePhoto(t, broadcastPost, body, diagnosticID, false)
	assertSmokeChannelPhotoUpdate(t, f.ctx, b.seen, broadcastPhoto, body, broadcastID, broadcastPost.ID, "channel caption", "broadcast live update", diagnosticID)
	assertSmokeChannelPhotoUpdate(t, f.ctx, b.push, broadcastPhoto, body, broadcastID, broadcastPost.ID, "channel caption", "broadcast push update", diagnosticID)
	broadcastHistory := smokeChannelHistoryPhoto(t, f.ctx, b, broadcastID, broadcastPost.ID, "broadcast history", diagnosticID)
	assertSmokeSamePhoto(t, broadcastHistory, broadcastPhoto, body, "broadcast history", diagnosticID, false, false)
	assertSmokePhotoDownload(t, f.ctx, b, broadcastPhoto, body, "broadcast subscriber", diagnosticID, false)

	megagroupID := smokeCreateChannel(t, f, a, "Photo smoke megagroup", false, true, diagnosticID)
	smokeJoinChannel(t, f, megagroupID, a.id, b.id, diagnosticID)
	memberPost := smokeSendChannelPhoto(t, f, b, megagroupID, 1048007, 1048008, parts, checksum, "member caption", diagnosticID)
	memberPhoto := assertSmokePhoto(t, memberPost, body, diagnosticID, false)
	assertSmokeChannelPhotoUpdate(t, f.ctx, a.seen, memberPhoto, body, megagroupID, memberPost.ID, "member caption", "megagroup live update", diagnosticID)
	megagroupHistory := smokeChannelHistoryPhoto(t, f.ctx, a, megagroupID, memberPost.ID, "megagroup history", diagnosticID)
	assertSmokeSamePhoto(t, megagroupHistory, memberPhoto, body, "megagroup history", diagnosticID, false, false)
	assertSmokePhotoDownload(t, f.ctx, a, memberPhoto, body, "megagroup reader", diagnosticID, false)
}

func smokeCreateChannel(t *testing.T, f *smokeFixture, c *smokeClient, title string, broadcast, megagroup bool, diagnosticID string) int64 {
	t.Helper()
	var channelID int64
	if err := c.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		created, err := client.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title: title, Broadcast: broadcast, Megagroup: megagroup,
		})
		if err != nil {
			return err
		}
		updates, ok := created.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChannel result = %T with %d chats, want one chat", created, len(updates.Chats))
		}
		channel, ok := updates.Chats[0].(*tg.Channel)
		if !ok {
			return fmt.Errorf("createChannel chat = %T, want *tg.Channel", updates.Chats[0])
		}
		channelID = channel.ID
		return nil
	}); err != nil {
		smokeFailuref(t, diagnosticID, "create smoke channel %q: %v", title, err)
	}
	return channelID
}

// smokeJoinChannel admits the member through the store's own join path, so the
// photo scenario does not depend on the invite-link prefix the fixture configures.
func smokeJoinChannel(t *testing.T, f *smokeFixture, channelID, creatorID, userID int64, diagnosticID string) {
	t.Helper()
	hash, err := f.store.CreateChannelInvite(f.ctx, channelID, creatorID)
	if err != nil {
		smokeFailuref(t, diagnosticID, "create smoke channel invite: %v", err)
	}
	if _, _, err = f.store.JoinChannelByInvite(f.ctx, hash, userID); err != nil {
		smokeFailuref(t, diagnosticID, "join smoke channel %d: %v", channelID, err)
	}
}

func smokeSendChannelPhoto(
	t *testing.T, f *smokeFixture, c *smokeClient, channelID, fileID, randomID int64,
	parts int, checksum, caption, diagnosticID string,
) *tg.Message {
	t.Helper()
	var result tg.UpdatesClass
	if err := c.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		body := smokeJPEGAtUploadSize(t, parts, diagnosticID, false)
		for part := range parts {
			start := part * smokePhotoPartSize
			ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: part, Bytes: body[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return fmt.Errorf("upload channel photo part %d: %w", part, err)
			}
			if !ok {
				return fmt.Errorf("upload channel photo part %d returned false", part)
			}
		}
		var err error
		result, err = client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerChannel(c.id, channelID),
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: fileID, Parts: parts, Name: "219343.jpg", MD5Checksum: checksum,
			}},
			Message: caption, RandomID: randomID,
		})
		return err
	}); err != nil {
		smokeFailuref(t, diagnosticID, "send channel photo to %d: %v", channelID, err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		smokeFailuref(t, diagnosticID, "channel sendMedia updates = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		posted, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		message, ok := posted.Message.(*tg.Message)
		if !ok {
			smokeFailuref(t, diagnosticID, "channel post = %T, want *tg.Message", posted.Message)
		}
		if !message.Out {
			smokeFailuref(t, diagnosticID, "channel post out = %v, want the sender's own outgoing post", message.Out)
		}
		return message
	}
	smokeFailuref(t, diagnosticID, "channel sendMedia carried no updateNewChannelMessage")
	return nil
}

func assertSmokeChannelPhotoUpdate(
	t *testing.T, ctx context.Context, updates *updateCollector, want *tg.Photo, body []byte,
	channelID int64, localID int, caption, label, diagnosticID string,
) {
	t.Helper()
	// A channel member also receives their own earlier posts live, so the
	// wait runs until the post this leg sent arrives.
	// Channel local ids restart per channel, so the wait matches the channel and
	// the post id together, skipping the member's own earlier posts.
	for {
		got := recvOrCtx(t, ctx, updates.newChannelMsg, label+" message", diagnosticID)
		if got.Msg == nil {
			smokeFailuref(t, diagnosticID, "%s carried no channel message", label)
		}
		peer, ok := got.Msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != channelID || got.Msg.ID != localID {
			continue
		}
		if got.Msg.Message != caption || got.Msg.Out {
			smokeFailuref(t, diagnosticID, "%s message = {caption:%q out:%v}, want the incoming %q caption", label, got.Msg.Message, got.Msg.Out, caption)
		}
		assertSmokeSamePhoto(t, got.Msg, want, body, label, diagnosticID, false, false)
		return
	}
}

func smokeChannelHistoryPhoto(
	t *testing.T, ctx context.Context, client *smokeClient, channelID int64, localID int, label string,
	diagnosticID string,
) *tg.Message {
	t.Helper()
	var result tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerChannel(client.id, channelID), Limit: 10})
		return err
	}); err != nil {
		smokeFailuref(t, diagnosticID, "%s: %v", label, err)
	}
	var classes []tg.MessageClass
	switch got := result.(type) {
	case *tg.MessagesChannelMessages:
		classes = got.Messages
	case *tg.MessagesMessages:
		classes = got.Messages
	default:
		smokeFailuref(t, diagnosticID, "%s response = %T, want a messages list", label, result)
	}
	for _, class := range classes {
		message, ok := class.(*tg.Message)
		if !ok || message.ID != localID {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPhoto); !ok {
			smokeFailuref(t, diagnosticID, "%s post %d media = %T, want messageMediaPhoto", label, localID, message.Media)
		}
		return message
	}
	smokeFailuref(t, diagnosticID, "%s holds no photo post %d among %d posts", label, localID, len(classes))
	return nil
}

func readSmokePhotoRequestFixture(t *testing.T, callsiteID string) (fixture struct {
	Layer   int    `json:"layer"`
	Client  string `json:"client"`
	Source  string `json:"source"`
	Method  string `json:"method"`
	Request struct {
		Type   string `json:"type"`
		Fields struct {
			Peer struct {
				Type string `json:"type"`
			} `json:"peer"`
			Media struct {
				Type   string `json:"type"`
				Flags  int    `json:"flags"`
				Fields struct {
					File struct {
						Type   string `json:"type"`
						Fields struct {
							Parts       int    `json:"parts"`
							Name        string `json:"name"`
							MD5Checksum string `json:"md5_checksum"`
						} `json:"fields"`
					} `json:"file"`
				} `json:"fields"`
			} `json:"media"`
			Message string `json:"message"`
		} `json:"fields"`
	} `json:"request"`
}) {
	t.Helper()
	body, err := os.ReadFile("../fixtures/client/layer-228/messages_sendMedia.3.json")
	if err != nil {
		t.Fatalf("[assert:%s/photo-media.fixture-file-read] read captured photo request fixture: %v", callsiteID, err)
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("[assert:%s/photo-media.fixture-json-decode] decode captured photo request fixture: %v", callsiteID, err)
	}
	if fixture.Layer != 228 || fixture.Client != "Teagram Desktop 7.0.9" || fixture.Source != "captures/messages_sendMedia.3.txt" || fixture.Method != "messages.sendMedia" {
		t.Fatalf("[assert:%s/photo-media.fixture-provenance] photo request fixture provenance = layer %d, client %q, source %q, method %q", callsiteID, fixture.Layer, fixture.Client, fixture.Source, fixture.Method)
	}
	if fixture.Request.Type != "messages.sendMedia" || fixture.Request.Fields.Peer.Type != "inputPeerUser" || fixture.Request.Fields.Media.Type != "inputMediaUploadedPhoto" || fixture.Request.Fields.Media.Flags != 0 || fixture.Request.Fields.Media.Fields.File.Type != "inputFile" || fixture.Request.Fields.Media.Fields.File.Fields.Parts != smokePhotoPartCount || fixture.Request.Fields.Message != "" {
		t.Fatalf("[assert:%s/photo-media.fixture-request-shape] captured photo request shape = %q peer, %q media flags %d, %q file with %d parts, caption %q", callsiteID, fixture.Request.Fields.Peer.Type, fixture.Request.Fields.Media.Type, fixture.Request.Fields.Media.Flags, fixture.Request.Fields.Media.Fields.File.Type, fixture.Request.Fields.Media.Fields.File.Fields.Parts, fixture.Request.Fields.Message)
	}
	if fixture.Request.Fields.Media.Fields.File.Fields.Name != "REDACTED.jpg" || fixture.Request.Fields.Media.Fields.File.Fields.MD5Checksum != "REDACTED" {
		t.Fatalf("[assert:%s/photo-media.fixture-file-redaction] captured photo filename/checksum = %q/%q, want redacted JPG name and checksum", callsiteID, fixture.Request.Fields.Media.Fields.File.Fields.Name, fixture.Request.Fields.Media.Fields.File.Fields.MD5Checksum)
	}
	return fixture
}

func smokeJPEGAtUploadSize(t *testing.T, parts int, callsiteID string, pairAssertions bool) []byte {
	t.Helper()
	if parts != smokePhotoPartCount {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.image-part-count] captured photo parts = %d, want %d", callsiteID, parts, smokePhotoPartCount)
		}
		smokeFailuref(t, callsiteID, "captured photo parts = %d, want %d", parts, smokePhotoPartCount)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := range 480 {
		for x := range 640 {
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageData, &jpeg.Options{Quality: 85}); err != nil {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.image-encode] encode synthetic JPEG: %v", callsiteID, err)
		}
		smokeFailuref(t, callsiteID, "encode synthetic JPEG: %v", err)
	}
	base := encoded.Bytes()
	wantSize := parts * smokePhotoPartSize
	padding := wantSize - len(base)
	if padding < 4 {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.image-padding-room] encoded JPEG is %d bytes, too large for %d-byte upload", callsiteID, len(base), wantSize)
		}
		smokeFailuref(t, callsiteID, "encoded JPEG is %d bytes, too large for %d-byte upload", len(base), wantSize)
	}
	var body bytes.Buffer
	body.Grow(wantSize)
	body.Write(base[:2])
	for padding > 0 {
		segmentSize := min(padding, 65537)
		if left := padding - segmentSize; left > 0 && left < 4 {
			segmentSize -= 4 - left
		}
		if segmentSize < 4 {
			if pairAssertions {
				t.Fatalf("[assert:%s/photo-media.image-padding-segment] invalid JPEG padding segment size %d", callsiteID, segmentSize)
			}
			smokeFailuref(t, callsiteID, "invalid JPEG padding segment size %d", segmentSize)
		}
		length := uint16(segmentSize - 2) // #nosec G115 -- segmentSize is capped at 65,537, so the JPEG length fits uint16.
		segment := []byte{0xff, 0xe1, 0, 0}
		binary.BigEndian.PutUint16(segment[2:], length)
		body.Write(segment)
		body.Write(bytes.Repeat([]byte{0}, segmentSize-4))
		padding -= segmentSize
	}
	body.Write(base[2:])
	if body.Len() != wantSize {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.image-upload-size] padded JPEG size = %d, want %d", callsiteID, body.Len(), wantSize)
		}
		smokeFailuref(t, callsiteID, "padded JPEG size = %d, want %d", body.Len(), wantSize)
	}
	return body.Bytes()
}

func smokePhotoMD5(body []byte) string {
	sum := md5.Sum(body) // #nosec G401 -- Telegram inputFile requires MD5 for uploaded photos.
	return hex.EncodeToString(sum[:])
}

func outgoingPhotoMessage(t *testing.T, result tg.UpdatesClass, callsiteID string) *tg.Message {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("[assert:%s/photo-media.outgoing-updates-type] sendMedia updates = %T, want *tg.Updates", callsiteID, result)
	}
	for _, update := range updates.Updates {
		if newMessage, ok := update.(*tg.UpdateNewMessage); ok {
			if message, ok := newMessage.Message.(*tg.Message); ok && message.Out {
				if message.Message != "" {
					t.Fatalf("[assert:%s/photo-media.outgoing-caption] photo caption = %q, want empty caption", callsiteID, message.Message)
				}
				return message
			}
		}
	}
	t.Fatalf("[assert:%s/photo-media.outgoing-message-missing] sendMedia result carried no outgoing photo message", callsiteID)
	return nil
}

func assertSmokePhoto(t *testing.T, message *tg.Message, body []byte, callsiteID string, pairAssertions bool) *tg.Photo {
	t.Helper()
	media, ok := message.Media.(*tg.MessageMediaPhoto)
	if !ok {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.message-media-type] photo message media = %T, want *tg.MessageMediaPhoto", callsiteID, message.Media)
		}
		smokeFailuref(t, callsiteID, "photo message media = %T, want *tg.MessageMediaPhoto", message.Media)
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.photo-object-type] photo = %T, want *tg.Photo", callsiteID, media.Photo)
		}
		smokeFailuref(t, callsiteID, "photo = %T, want *tg.Photo", media.Photo)
	}
	if photo.ID == 0 || photo.AccessHash == 0 || len(photo.FileReference) == 0 {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.photo-identifiers] photo identifiers = id %d, access hash %d, file reference length %d", callsiteID, photo.ID, photo.AccessHash, len(photo.FileReference))
		}
		smokeFailuref(t, callsiteID, "photo identifiers = id %d, access hash %d, file reference length %d", photo.ID, photo.AccessHash, len(photo.FileReference))
	}
	if len(photo.Sizes) != 1 {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.photo-size-count] photo sizes = %d, want one original", callsiteID, len(photo.Sizes))
		}
		smokeFailuref(t, callsiteID, "photo sizes = %d, want one original", len(photo.Sizes))
	}
	size, ok := photo.Sizes[0].(*tg.PhotoSize)
	if !ok || size.Type != "x" || size.W != 640 || size.H != 480 || size.Size != len(body) {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.photo-size-shape] photo size = %#v, want x/640x480/%d", callsiteID, photo.Sizes[0], len(body))
		}
		smokeFailuref(t, callsiteID, "photo size = %#v, want x/640x480/%d", photo.Sizes[0], len(body))
	}
	return photo
}

func assertSmokeSamePhoto(
	t *testing.T, message *tg.Message, want *tg.Photo, body []byte, label, callsiteID string,
	pairAssertions, pairPhotoAssertions bool,
) {
	t.Helper()
	got := assertSmokePhoto(t, message, body, callsiteID, pairPhotoAssertions)
	if got.ID != want.ID || got.AccessHash != want.AccessHash || !bytes.Equal(got.FileReference, want.FileReference) {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.photo-identity] %s photo = id %d hash %d reference %x, want id %d hash %d reference %x", callsiteID, label, got.ID, got.AccessHash, got.FileReference, want.ID, want.AccessHash, want.FileReference)
		}
		smokeFailuref(t, callsiteID, "%s photo = id %d hash %d reference %x, want id %d hash %d reference %x", label, got.ID, got.AccessHash, got.FileReference, want.ID, want.AccessHash, want.FileReference)
	}
}

func assertSmokePhotoUpdate(
	t *testing.T, ctx context.Context, updates *updateCollector, want *tg.Photo, body []byte,
	peerID int64, group bool, pts int, label, callsiteID string,
) {
	t.Helper()
	message := recvOrCtx(t, ctx, updates.newMsg, label+" message", callsiteID)
	if message.Message != "" || message.Out {
		t.Fatalf("[assert:%s/photo-media.update-message-fields] %s message = {caption:%q out:%v}, want incoming empty-caption photo", callsiteID, label, message.Message, message.Out)
	}
	if group {
		peer, ok := message.PeerID.(*tg.PeerChat)
		if !ok || peer.ChatID != peerID {
			t.Fatalf("[assert:%s/photo-media.update-peer-chat] %s peer = %+v, want chat %d", callsiteID, label, message.PeerID, peerID)
		}
	} else {
		peer, ok := message.PeerID.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			t.Fatalf("[assert:%s/photo-media.update-peer-user] %s peer = %+v, want user %d", callsiteID, label, message.PeerID, peerID)
		}
	}
	if got := recvOrCtx(t, ctx, updates.points, label+" pts", callsiteID); got != pts {
		t.Fatalf("[assert:%s/photo-media.update-pts] %s pts = %d, want %d", callsiteID, label, got, pts)
	}
	assertSmokeSamePhoto(t, message, want, body, label, callsiteID, true, false)
}

func smokeHistoryMessageForPhoto(t *testing.T, ctx context.Context, client *smokeClient, peer tg.InputPeerClass, label, callsiteID string) *tg.Message {
	t.Helper()
	var result tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("[assert:%s/photo-media.history-request] %s: %v", callsiteID, label, err)
	}
	messages, ok := result.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("[assert:%s/photo-media.history-response-type] %s response = %T, want *tg.MessagesMessages", callsiteID, label, result)
	}
	var photoMessage *tg.Message
	for _, class := range messages.Messages {
		message, ok := class.(*tg.Message)
		if !ok {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPhoto); !ok {
			continue
		}
		if photoMessage != nil {
			t.Fatalf("[assert:%s/photo-media.history-duplicate-photo] %s has more than one photo message", callsiteID, label)
		}
		photoMessage = message
	}
	if photoMessage == nil {
		t.Fatalf("[assert:%s/photo-media.history-photo-missing] %s has no photo among %d history messages", callsiteID, label, len(messages.Messages))
	}
	return photoMessage
}

func assertSmokePhotoDownload(
	t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, body []byte, label, callsiteID string,
	pairAssertions bool,
) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: "x"},
			Offset:   0, Limit: len(body),
		})
		return err
	}); err != nil {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.download-request] %s photo download: %v", callsiteID, label, err)
		}
		smokeFailuref(t, callsiteID, "%s photo download: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.download-file-type] %s photo download = %T, want *tg.UploadFile", callsiteID, label, result)
		}
		smokeFailuref(t, callsiteID, "%s photo download = %T, want *tg.UploadFile", label, result)
	}
	if _, ok := file.Type.(*tg.StorageFileJpeg); !ok {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.download-storage-type] %s photo download type = %T, want *tg.StorageFileJpeg", callsiteID, label, file.Type)
		}
		smokeFailuref(t, callsiteID, "%s photo download type = %T, want *tg.StorageFileJpeg", label, file.Type)
	}
	if !bytes.Equal(file.Bytes, body) {
		if pairAssertions {
			t.Fatalf("[assert:%s/photo-media.download-content] %s photo download returned %d bytes, want %d identical bytes", callsiteID, label, len(file.Bytes), len(body))
		}
		smokeFailuref(t, callsiteID, "%s photo download returned %d bytes, want %d identical bytes", label, len(file.Bytes), len(body))
	}
}

func differenceMessageCount(difference tg.UpdatesDifferenceClass) int {
	if full, ok := difference.(*tg.UpdatesDifference); ok {
		return len(full.NewMessages)
	}
	return 0
}
