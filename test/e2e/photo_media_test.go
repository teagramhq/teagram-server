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
	"github.com/jackc/pgx/v5"
)

const (
	smokePhotoPartSize  = 32 * 1024
	smokePhotoPartCount = 8
)

func testSmokePhotoMedia(t *testing.T) {
	t.Helper()
	fixture := readSmokePhotoRequestFixture(t)
	body := smokeJPEGAtUploadSize(t, fixture.Request.Fields.Media.Fields.File.Fields.Parts)
	checksum := smokePhotoMD5(body)
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551048001", "+15551048002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a, b := newSmokeClient(t, f, "Photo sender", phoneA), newSmokeClient(t, f, "Photo recipient", phoneB)
	mPreview := seedSmokePhotoDerivativesBeforePublication(t, f)

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
		t.Fatalf("send private photo using captured layer-%d request: %v", fixture.Layer, err)
	}
	privateMessage := outgoingPhotoMessage(t, privateResult)
	privatePhoto := assertSmokePhoto(t, privateMessage, body)
	assertSmokePhotoMDownload(t, f.ctx, b, privatePhoto, mPreview, "private recipient")
	assertSmokePhotoUpdate(t, f.ctx, b.seen, privatePhoto, body, a.id, false, 1, "private live update")
	assertSmokePhotoUpdate(t, f.ctx, b.push, privatePhoto, body, a.id, false, 1, "private push update")

	var difference tg.UpdatesDifferenceClass
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		difference, err = client.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
		return err
	}); err != nil {
		t.Fatalf("get photo difference: %v", err)
	}
	fullDifference, ok := difference.(*tg.UpdatesDifference)
	if !ok || len(fullDifference.NewMessages) != 1 {
		t.Fatalf("photo difference = %T with %d messages, want one message", difference, differenceMessageCount(difference))
	}
	differenceMessage, ok := fullDifference.NewMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("photo difference message = %T, want *tg.Message", fullDifference.NewMessages[0])
	}
	assertSmokeSamePhoto(t, differenceMessage, privatePhoto, body, "private difference")

	privateHistory := smokeHistoryMessageForPhoto(t, f.ctx, b, peerUser(b.id, a.id), "private photo history")
	assertSmokeSamePhoto(t, privateHistory, privatePhoto, body, "private history")
	var byID tg.MessagesMessagesClass
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		byID, err = client.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: privateHistory.ID}})
		return err
	}); err != nil {
		t.Fatalf("get private photo by id: %v", err)
	}
	byIDMessages, ok := byID.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("getMessages photo = %T, want *tg.MessagesMessages", byID)
	}
	if len(byIDMessages.Messages) != 1 {
		t.Fatalf("getMessages photo has %d messages, want one", len(byIDMessages.Messages))
	}
	byIDMessage, ok := byIDMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("getMessages photo message = %T, want *tg.Message", byIDMessages.Messages[0])
	}
	assertSmokeSamePhoto(t, byIDMessage, privatePhoto, body, "private getMessages")
	photoByID := assertSmokePhoto(t, byIDMessage, body)
	assertSmokePhotoDownload(t, f.ctx, b, photoByID, body, "private getMessages recipient")
	assertSmokePhotoMDownload(t, f.ctx, b, photoByID, mPreview, "private getMessages recipient")

	var dialogMessage *tg.Message
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 10})
		if err != nil {
			return err
		}
		var messages []tg.MessageClass
		switch page := result.(type) {
		case *tg.MessagesDialogs:
			messages = page.Messages
		case *tg.MessagesDialogsSlice:
			messages = page.Messages
		default:
			return fmt.Errorf("photo dialogs = %T, want a dialogs result", result)
		}
		for _, class := range messages {
			message, ok := class.(*tg.Message)
			if ok && message.ID == privateHistory.ID {
				dialogMessage = message
				return nil
			}
		}
		return fmt.Errorf("photo dialogs omitted top message %d", privateHistory.ID)
	}); err != nil {
		t.Fatalf("get private photo dialogs: %v", err)
	}
	assertSmokeSamePhoto(t, dialogMessage, privatePhoto, body, "private dialogs")

	var searchMessage *tg.Message
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer: peerUser(b.id, a.id), Q: "", Filter: &tg.InputMessagesFilterPhotos{}, Limit: 10,
		})
		if err != nil {
			return err
		}
		page, ok := result.(*tg.MessagesMessagesSlice)
		if !ok || len(page.Messages) != 1 {
			return fmt.Errorf("photo search = %#v, want one message", result)
		}
		searchMessage, ok = page.Messages[0].(*tg.Message)
		if !ok {
			return fmt.Errorf("photo search message = %T, want *tg.Message", page.Messages[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("search private photo: %v", err)
	}
	assertSmokeSamePhoto(t, searchMessage, privatePhoto, body, "private photo search")

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
		t.Fatalf("create photo smoke group: %v", err)
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
		t.Fatalf("send basic-group photo: %v", err)
	}
	groupMessage := outgoingPhotoMessage(t, groupResult)
	groupPhoto := assertSmokePhoto(t, groupMessage, body)
	assertSmokePhotoUpdate(t, f.ctx, b.seen, groupPhoto, body, chatID, true, 3, "group live update")
	assertSmokePhotoUpdate(t, f.ctx, b.push, groupPhoto, body, chatID, true, 3, "group push update")
	groupHistory := smokeHistoryMessageForPhoto(t, f.ctx, b, &tg.InputPeerChat{ChatID: chatID}, "group photo history")
	assertSmokeSamePhoto(t, groupHistory, groupPhoto, body, "group history")
	assertSmokePhotoDownload(t, f.ctx, b, groupPhoto, body, "group recipient")
	assertSmokePhotoMDownload(t, f.ctx, b, groupPhoto, mPreview, "group recipient")

	smokeChannelPhotoLegs(t, f, a, b, fixture.Request.Fields.Media.Fields.File.Fields.Parts, body, mPreview)

	a.stopClient(t)
	b.stopClient(t)
	f.restart(t)
	restartedRecipient := newSmokeClient(t, f, "Photo recipient after restart", phoneB)
	if restartedRecipient.id != b.id {
		t.Fatalf("recipient id after restart = %d, want %d", restartedRecipient.id, b.id)
	}
	restartedHistory := smokeHistoryMessageForPhoto(t, f.ctx, restartedRecipient, peerUser(restartedRecipient.id, a.id), "private photo history after restart")
	assertSmokeSamePhoto(t, restartedHistory, privatePhoto, body, "private history after restart")
	assertSmokePhotoDownload(t, f.ctx, restartedRecipient, assertSmokePhoto(t, restartedHistory, body), body, "private recipient after restart")
	assertSmokePhotoMDownload(t, f.ctx, restartedRecipient, assertSmokePhoto(t, restartedHistory, body), mPreview, "private recipient after restart")
}

var smokePhotoStrippedPreview = []byte{1, 20, 20, 0xff, 0xd8, 0xff, 0xd9}

func seedSmokePhotoDerivativesBeforePublication(t *testing.T, f *smokeFixture) []byte {
	t.Helper()
	preview := image.NewRGBA(image.Rect(0, 0, 320, 320))
	for y := range 320 {
		for x := range 320 {
			preview.SetRGBA(x, y, color.RGBA{R: 110, G: 150, B: 190, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, preview, nil); err != nil {
		t.Fatalf("encode m preview JPEG: %v", err)
	}
	mPreview := encoded.Bytes()
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed smoke photo derivatives: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close smoke derivative seeder: %v", err)
		}
	}()
	_, err = conn.Exec(f.ctx, fmt.Sprintf(`
CREATE FUNCTION smoke_seed_photo_derivative_before_publish() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.stored = false AND NEW.stored = true AND NEW.media_kind = 'photo' THEN
        INSERT INTO photo_derivatives (file_id, m_width, m_height, m_size, m_bytes, stripped)
        VALUES (NEW.id, 320, 320, %d, decode('%x', 'hex'), decode('%x', 'hex'));
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER smoke_seed_photo_derivative_before_publish
BEFORE UPDATE OF stored ON files
FOR EACH ROW EXECUTE FUNCTION smoke_seed_photo_derivative_before_publish();
`, len(mPreview), mPreview, smokePhotoStrippedPreview))
	if err != nil {
		t.Fatalf("install smoke photo derivative seeder: %v", err)
	}
	return mPreview
}

// smokeChannelPhotoLegs is the channel half of the photo smoke scenario: a
// broadcast creator posts and a subscriber reads the same original, and a
// megagroup member posts with a caption and the creator reads it back. Each leg
// asserts the live update, the history read and a byte-identical download.
func smokeChannelPhotoLegs(t *testing.T, f *smokeFixture, a, b *smokeClient, parts int, body, mPreview []byte) {
	t.Helper()
	checksum := smokePhotoMD5(body)

	broadcastID := smokeCreateChannel(t, f, a, "Photo smoke channel", true, false)
	smokeJoinChannel(t, f, broadcastID, a.id, b.id)
	broadcastPost := smokeSendChannelPhoto(t, f, a, broadcastID, 1048005, 1048006, parts, checksum, "channel caption")
	if broadcastPost.Message != "channel caption" {
		t.Fatalf("broadcast photo caption = %q, want the posted caption", broadcastPost.Message)
	}
	broadcastPhoto := assertSmokePhoto(t, broadcastPost, body)
	assertSmokeChannelPhotoUpdate(t, f.ctx, b.seen, broadcastPhoto, body, broadcastID, broadcastPost.ID, "channel caption", "broadcast live update")
	assertSmokeChannelPhotoUpdate(t, f.ctx, b.push, broadcastPhoto, body, broadcastID, broadcastPost.ID, "channel caption", "broadcast push update")
	broadcastHistory := smokeChannelHistoryPhoto(t, f.ctx, b, broadcastID, broadcastPost.ID, "broadcast history")
	assertSmokeSamePhoto(t, broadcastHistory, broadcastPhoto, body, "broadcast history")
	assertSmokePhotoDownload(t, f.ctx, b, broadcastPhoto, body, "broadcast subscriber")
	assertSmokePhotoMDownload(t, f.ctx, b, broadcastPhoto, mPreview, "broadcast subscriber")

	megagroupID := smokeCreateChannel(t, f, a, "Photo smoke megagroup", false, true)
	smokeJoinChannel(t, f, megagroupID, a.id, b.id)
	memberPost := smokeSendChannelPhoto(t, f, b, megagroupID, 1048007, 1048008, parts, checksum, "member caption")
	memberPhoto := assertSmokePhoto(t, memberPost, body)
	assertSmokeChannelPhotoUpdate(t, f.ctx, a.seen, memberPhoto, body, megagroupID, memberPost.ID, "member caption", "megagroup live update")
	megagroupHistory := smokeChannelHistoryPhoto(t, f.ctx, a, megagroupID, memberPost.ID, "megagroup history")
	assertSmokeSamePhoto(t, megagroupHistory, memberPhoto, body, "megagroup history")
	assertSmokePhotoDownload(t, f.ctx, a, memberPhoto, body, "megagroup reader")
	assertSmokePhotoMDownload(t, f.ctx, a, memberPhoto, mPreview, "megagroup reader")
}

func smokeCreateChannel(t *testing.T, f *smokeFixture, c *smokeClient, title string, broadcast, megagroup bool) int64 {
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
		t.Fatalf("create smoke channel %q: %v", title, err)
	}
	return channelID
}

// smokeJoinChannel admits the member through the store's own join path, so the
// photo scenario does not depend on the invite-link prefix the fixture configures.
func smokeJoinChannel(t *testing.T, f *smokeFixture, channelID, creatorID, userID int64) {
	t.Helper()
	hash, err := f.store.CreateChannelInvite(f.ctx, channelID, creatorID)
	if err != nil {
		t.Fatalf("create smoke channel invite: %v", err)
	}
	if _, _, err = f.store.JoinChannelByInvite(f.ctx, hash, userID); err != nil {
		t.Fatalf("join smoke channel %d: %v", channelID, err)
	}
}

func smokeSendChannelPhoto(
	t *testing.T, f *smokeFixture, c *smokeClient, channelID, fileID, randomID int64,
	parts int, checksum, caption string,
) *tg.Message {
	t.Helper()
	var result tg.UpdatesClass
	if err := c.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		body := smokeJPEGAtUploadSize(t, parts)
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
		t.Fatalf("send channel photo to %d: %v", channelID, err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("channel sendMedia updates = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		posted, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		message, ok := posted.Message.(*tg.Message)
		if !ok {
			t.Fatalf("channel post = %T, want *tg.Message", posted.Message)
		}
		if !message.Out {
			t.Fatalf("channel post out = %v, want the sender's own outgoing post", message.Out)
		}
		return message
	}
	t.Fatal("channel sendMedia carried no updateNewChannelMessage")
	return nil
}

func assertSmokeChannelPhotoUpdate(
	t *testing.T, ctx context.Context, updates *updateCollector, want *tg.Photo, body []byte,
	channelID int64, localID int, caption, label string,
) {
	t.Helper()
	// A channel member also receives their own earlier posts live, so the
	// wait runs until the post this leg sent arrives.
	// Channel local ids restart per channel, so the wait matches the channel and
	// the post id together, skipping the member's own earlier posts.
	for {
		got := recvOrCtx(t, ctx, updates.newChannelMsg, label+" message")
		if got.Msg == nil {
			t.Fatalf("%s carried no channel message", label)
		}
		peer, ok := got.Msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != channelID || got.Msg.ID != localID {
			continue
		}
		if got.Msg.Message != caption || got.Msg.Out {
			t.Fatalf("%s message = {caption:%q out:%v}, want the incoming %q caption", label, got.Msg.Message, got.Msg.Out, caption)
		}
		assertSmokeSamePhoto(t, got.Msg, want, body, label)
		return
	}
}

func smokeChannelHistoryPhoto(
	t *testing.T, ctx context.Context, client *smokeClient, channelID int64, localID int, label string,
) *tg.Message {
	t.Helper()
	var result tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerChannel(client.id, channelID), Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	var classes []tg.MessageClass
	switch got := result.(type) {
	case *tg.MessagesChannelMessages:
		classes = got.Messages
	case *tg.MessagesMessages:
		classes = got.Messages
	default:
		t.Fatalf("%s response = %T, want a messages list", label, result)
	}
	for _, class := range classes {
		message, ok := class.(*tg.Message)
		if !ok || message.ID != localID {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPhoto); !ok {
			t.Fatalf("%s post %d media = %T, want messageMediaPhoto", label, localID, message.Media)
		}
		return message
	}
	t.Fatalf("%s holds no photo post %d among %d posts", label, localID, len(classes))
	return nil
}

func readSmokePhotoRequestFixture(t *testing.T) (fixture struct {
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
		t.Fatalf("read captured photo request fixture: %v", err)
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatalf("decode captured photo request fixture: %v", err)
	}
	if fixture.Layer != 228 || fixture.Client != "Teagram Desktop 7.0.9" || fixture.Source != "captures/messages_sendMedia.3.txt" || fixture.Method != "messages.sendMedia" {
		t.Fatalf("photo request fixture provenance = layer %d, client %q, source %q, method %q", fixture.Layer, fixture.Client, fixture.Source, fixture.Method)
	}
	if fixture.Request.Type != "messages.sendMedia" || fixture.Request.Fields.Peer.Type != "inputPeerUser" || fixture.Request.Fields.Media.Type != "inputMediaUploadedPhoto" || fixture.Request.Fields.Media.Flags != 0 || fixture.Request.Fields.Media.Fields.File.Type != "inputFile" || fixture.Request.Fields.Media.Fields.File.Fields.Parts != smokePhotoPartCount || fixture.Request.Fields.Message != "" {
		t.Fatalf("captured photo request shape = %q peer, %q media flags %d, %q file with %d parts, caption %q", fixture.Request.Fields.Peer.Type, fixture.Request.Fields.Media.Type, fixture.Request.Fields.Media.Flags, fixture.Request.Fields.Media.Fields.File.Type, fixture.Request.Fields.Media.Fields.File.Fields.Parts, fixture.Request.Fields.Message)
	}
	if fixture.Request.Fields.Media.Fields.File.Fields.Name != "REDACTED.jpg" || fixture.Request.Fields.Media.Fields.File.Fields.MD5Checksum != "REDACTED" {
		t.Fatalf("captured photo filename/checksum = %q/%q, want redacted JPG name and checksum", fixture.Request.Fields.Media.Fields.File.Fields.Name, fixture.Request.Fields.Media.Fields.File.Fields.MD5Checksum)
	}
	return fixture
}

func smokeJPEGAtUploadSize(t *testing.T, parts int) []byte {
	t.Helper()
	if parts != smokePhotoPartCount {
		t.Fatalf("captured photo parts = %d, want %d", parts, smokePhotoPartCount)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := range 480 {
		for x := range 640 {
			imageData.SetRGBA(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: uint8((x + y) % 256), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageData, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode synthetic JPEG: %v", err)
	}
	base := encoded.Bytes()
	wantSize := parts * smokePhotoPartSize
	padding := wantSize - len(base)
	if padding < 4 {
		t.Fatalf("encoded JPEG is %d bytes, too large for %d-byte upload", len(base), wantSize)
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
			t.Fatalf("invalid JPEG padding segment size %d", segmentSize)
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
		t.Fatalf("padded JPEG size = %d, want %d", body.Len(), wantSize)
	}
	return body.Bytes()
}

func smokePhotoMD5(body []byte) string {
	sum := md5.Sum(body) // #nosec G401 -- Telegram inputFile requires MD5 for uploaded photos.
	return hex.EncodeToString(sum[:])
}

func outgoingPhotoMessage(t *testing.T, result tg.UpdatesClass) *tg.Message {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("sendMedia updates = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		if newMessage, ok := update.(*tg.UpdateNewMessage); ok {
			if message, ok := newMessage.Message.(*tg.Message); ok && message.Out {
				if message.Message != "" {
					t.Fatalf("photo caption = %q, want empty caption", message.Message)
				}
				return message
			}
		}
	}
	t.Fatal("sendMedia result carried no outgoing photo message")
	return nil
}

func assertSmokePhoto(t *testing.T, message *tg.Message, body []byte) *tg.Photo {
	t.Helper()
	media, ok := message.Media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("photo message media = %T, want *tg.MessageMediaPhoto", message.Media)
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("photo = %T, want *tg.Photo", media.Photo)
	}
	if photo.ID == 0 || photo.AccessHash == 0 || len(photo.FileReference) == 0 {
		t.Fatalf("photo identifiers = id %d, access hash %d, file reference length %d", photo.ID, photo.AccessHash, len(photo.FileReference))
	}
	if photo.DCID != 2 {
		t.Fatalf("photo dc id = %d, want configured dc id 2", photo.DCID)
	}
	if len(photo.Sizes) != 3 {
		t.Fatalf("photo sizes = %d, want stripped, m, and original", len(photo.Sizes))
	}
	stripped, ok := photo.Sizes[0].(*tg.PhotoStrippedSize)
	if !ok || stripped.Type != "i" || !bytes.Equal(stripped.Bytes, smokePhotoStrippedPreview) {
		t.Fatalf("stripped photo size = %#v, want i preview", photo.Sizes[0])
	}
	m, ok := photo.Sizes[1].(*tg.PhotoSize)
	if !ok || m.Type != "m" || m.W != 320 || m.H != 320 || m.Size == 0 || m.Size > 65536 {
		t.Fatalf("m photo size = %#v, want bounded 320x320 m preview", photo.Sizes[1])
	}
	original, ok := photo.Sizes[2].(*tg.PhotoSize)
	if !ok || original.Type != "x" || original.W != 640 || original.H != 480 || original.Size != len(body) {
		t.Fatalf("original photo size = %#v, want x/640x480/%d", photo.Sizes[2], len(body))
	}
	return photo
}

func assertSmokeSamePhoto(t *testing.T, message *tg.Message, want *tg.Photo, body []byte, label string) {
	t.Helper()
	got := assertSmokePhoto(t, message, body)
	if got.ID != want.ID || got.AccessHash != want.AccessHash || !bytes.Equal(got.FileReference, want.FileReference) {
		t.Fatalf("%s photo = id %d hash %d reference %x, want id %d hash %d reference %x", label, got.ID, got.AccessHash, got.FileReference, want.ID, want.AccessHash, want.FileReference)
	}
}

func assertSmokePhotoUpdate(t *testing.T, ctx context.Context, updates *updateCollector, want *tg.Photo, body []byte, peerID int64, group bool, pts int, label string) {
	t.Helper()
	message := recvOrCtx(t, ctx, updates.newMsg, label+" message")
	if message.Message != "" || message.Out {
		t.Fatalf("%s message = {caption:%q out:%v}, want incoming empty-caption photo", label, message.Message, message.Out)
	}
	if group {
		peer, ok := message.PeerID.(*tg.PeerChat)
		if !ok || peer.ChatID != peerID {
			t.Fatalf("%s peer = %+v, want chat %d", label, message.PeerID, peerID)
		}
	} else {
		peer, ok := message.PeerID.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			t.Fatalf("%s peer = %+v, want user %d", label, message.PeerID, peerID)
		}
	}
	if got := recvOrCtx(t, ctx, updates.points, label+" pts"); got != pts {
		t.Fatalf("%s pts = %d, want %d", label, got, pts)
	}
	assertSmokeSamePhoto(t, message, want, body, label)
}

func smokeHistoryMessageForPhoto(t *testing.T, ctx context.Context, client *smokeClient, peer tg.InputPeerClass, label string) *tg.Message {
	t.Helper()
	var result tg.MessagesMessagesClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	messages, ok := result.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("%s response = %T, want *tg.MessagesMessages", label, result)
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
			t.Fatalf("%s has more than one photo message", label)
		}
		photoMessage = message
	}
	if photoMessage == nil {
		t.Fatalf("%s has no photo among %d history messages", label, len(messages.Messages))
	}
	return photoMessage
}

func assertSmokePhotoDownload(t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, body []byte, label string) {
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
		t.Fatalf("%s photo download: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok {
		t.Fatalf("%s photo download = %T, want *tg.UploadFile", label, result)
	}
	if _, ok := file.Type.(*tg.StorageFileJpeg); !ok {
		t.Fatalf("%s photo download type = %T, want *tg.StorageFileJpeg", label, file.Type)
	}
	if !bytes.Equal(file.Bytes, body) {
		t.Fatalf("%s photo download returned %d bytes, want %d identical bytes", label, len(file.Bytes), len(body))
	}
}

func assertSmokePhotoMDownload(t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, body []byte, label string) {
	t.Helper()
	var result tg.UploadFileClass
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: "m"},
			Offset:   0,
			Limit:    len(body),
		})
		return err
	}); err != nil {
		t.Fatalf("%s m photo download: %v", label, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok {
		t.Fatalf("%s m photo download = %T, want *tg.UploadFile", label, result)
	}
	if _, ok := file.Type.(*tg.StorageFileJpeg); !ok {
		t.Fatalf("%s m photo download type = %T, want *tg.StorageFileJpeg", label, file.Type)
	}
	if !bytes.Equal(file.Bytes, body) {
		t.Fatalf("%s m photo download returned %d bytes, want %d identical bytes", label, len(file.Bytes), len(body))
	}
}

func differenceMessageCount(difference tg.UpdatesDifferenceClass) int {
	if full, ok := difference.(*tg.UpdatesDifference); ok {
		return len(full.NewMessages)
	}
	return 0
}
