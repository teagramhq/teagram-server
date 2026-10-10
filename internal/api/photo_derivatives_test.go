package api_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

var seededPhotoM = []byte("m-thumbnail-payload-for-bounded-ranges")
var seededPhotoStripped = []byte{1, 20, 30, 0xaa, 0xbb}

func seedPhotoDerivativesBeforePublication(t *testing.T, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect to seed photo derivative: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close photo derivative seeder: %v", err)
		}
	})

	_, err = conn.Exec(context.Background(), fmt.Sprintf(`
CREATE FUNCTION test_seed_photo_derivative_before_publish() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.stored = false AND NEW.stored = true
       AND NEW.media_kind = 'photo' AND NEW.width = 1600 AND NEW.height = 1600 THEN
        INSERT INTO photo_derivatives (file_id, m_width, m_height, m_size, m_bytes, stripped)
        VALUES (NEW.id, 320, 320, %d, decode('%x', 'hex'), decode('%x', 'hex'));
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER test_seed_photo_derivative_before_publish
BEFORE UPDATE OF stored ON files
FOR EACH ROW EXECUTE FUNCTION test_seed_photo_derivative_before_publish();
`, len(seededPhotoM), seededPhotoM, seededPhotoStripped))
	if err != nil {
		t.Fatalf("install photo derivative seeder: %v", err)
	}
}

func assertPhotoDerivatives(t *testing.T, photo *tg.Photo) {
	t.Helper()
	if len(photo.Sizes) != 3 {
		t.Fatalf("photo sizes = %d, want stripped, m, and original", len(photo.Sizes))
	}
	stripped, ok := photo.Sizes[0].(*tg.PhotoStrippedSize)
	if !ok || stripped.Type != "i" || !bytes.Equal(stripped.Bytes, seededPhotoStripped) {
		t.Fatalf("first photo size = %#v, want stripped i metadata", photo.Sizes[0])
	}
	m, ok := photo.Sizes[1].(*tg.PhotoSize)
	if !ok || m.Type != "m" || m.W != 320 || m.H != 320 || m.Size != len(seededPhotoM) {
		t.Fatalf("second photo size = %#v, want 320x320 m size %d", photo.Sizes[1], len(seededPhotoM))
	}
	original, ok := photo.Sizes[2].(*tg.PhotoSize)
	if !ok || original.Type != "w" || original.W != 1600 || original.H != 1600 {
		t.Fatalf("last photo size = %#v, want unchanged 1600x1600 original", photo.Sizes[2])
	}
}

func assertMessagePhotoDerivatives(t *testing.T, message *tg.Message) {
	t.Helper()
	assertPhotoDerivatives(t, photoOfMessage(t, message))
}

func TestSeededPhotoDerivativesRenderAndDownload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551299001")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551299002")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}

	body := jpegPhotoPayload(t, 1600, 1600)
	saveParts(t, s, sender.ID, 99001, body)
	seedPhotoDerivativesBeforePublication(t, dsn)
	result, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(sender.ID, recipient.ID),
		Media:    uploadedPhoto(99001, 1, "photo.jpg", jpegPhotoMD5(body)),
		Message:  "photo derivatives",
		RandomID: 99001,
	})
	if err != nil {
		t.Fatalf("send seeded photo: %v", err)
	}
	message := messageOf(t, result)
	photo := photoOfMessage(t, message)
	assertPhotoDerivatives(t, photo)

	history, err := api.GetHistoryForTest(s, recipient.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(recipient.ID, sender.ID),
	})
	if err != nil {
		t.Fatalf("recipient history: %v", err)
	}
	historyMessages, ok := history.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) != 1 {
		t.Fatalf("recipient history = %#v, want one message", history)
	}
	historyMessage, ok := historyMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("history message = %T, want *tg.Message", historyMessages.Messages[0])
	}
	assertMessagePhotoDerivatives(t, historyMessage)

	difference, err := api.GetDifferenceForTest(s, recipient.ID, &tg.UpdatesGetDifferenceRequest{})
	if err != nil {
		t.Fatalf("recipient difference: %v", err)
	}
	differenceResult, ok := difference.(*tg.UpdatesDifference)
	if !ok || len(differenceResult.NewMessages) != 1 {
		t.Fatalf("recipient difference = %#v, want one new message", difference)
	}
	differenceMessage, ok := differenceResult.NewMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("difference message = %T, want *tg.Message", differenceResult.NewMessages[0])
	}
	assertMessagePhotoDerivatives(t, differenceMessage)

	byID, err := api.GetMessagesForTest(s, recipient.ID, &tg.MessagesGetMessagesRequest{
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: historyMessage.ID}},
	})
	if err != nil {
		t.Fatalf("recipient getMessages: %v", err)
	}
	byIDResult, ok := byID.(*tg.MessagesMessages)
	if !ok || len(byIDResult.Messages) != 1 {
		t.Fatalf("recipient getMessages = %#v, want one message", byID)
	}
	byIDMessage, ok := byIDResult.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("getMessages message = %T, want *tg.Message", byIDResult.Messages[0])
	}
	assertMessagePhotoDerivatives(t, byIDMessage)

	dialogs, err := api.GetDialogsForTest(s, recipient.ID)
	if err != nil {
		t.Fatalf("recipient dialogs: %v", err)
	}
	dialogResult, ok := dialogs.(*tg.MessagesDialogs)
	if !ok || len(dialogResult.Messages) != 1 {
		t.Fatalf("recipient dialogs = %#v, want one top message", dialogs)
	}
	dialogMessage, ok := dialogResult.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("dialog top message = %T, want *tg.Message", dialogResult.Messages[0])
	}
	assertMessagePhotoDerivatives(t, dialogMessage)

	search, err := api.SearchForTest(s, recipient.ID, &tg.MessagesSearchRequest{
		Peer: api.InputPeerUser(recipient.ID, sender.ID), Q: "derivatives", Filter: &tg.InputMessagesFilterPhotos{}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient photo search: %v", err)
	}
	searchResult, ok := search.(*tg.MessagesMessagesSlice)
	if !ok || len(searchResult.Messages) != 1 {
		t.Fatalf("recipient photo search = %#v, want one message", search)
	}
	searchMessage, ok := searchResult.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("search message = %T, want *tg.Message", searchResult.Messages[0])
	}
	assertMessagePhotoDerivatives(t, searchMessage)
	downloadBlobs := &countingDownloadBlobStore{Store: blobs}
	if got := downloadBlobs.reads.Load(); got != 0 {
		t.Fatalf("message hydration made %d blob reads, want none", got)
	}

	file, err := api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Offset:   7,
		Limit:    11,
	})
	if err != nil {
		t.Fatalf("download m thumbnail: %v", err)
	}
	thumbnail, ok := file.(*tg.UploadFile)
	if !ok {
		t.Fatalf("m thumbnail result = %T, want *tg.UploadFile", file)
	}
	if _, ok := thumbnail.Type.(*tg.StorageFileJpeg); !ok {
		t.Fatalf("m thumbnail type = %T, want storage.fileJpeg", thumbnail.Type)
	}
	if !bytes.Equal(thumbnail.Bytes, seededPhotoM[7:18]) {
		t.Fatalf("m thumbnail bytes = %q, want %q", thumbnail.Bytes, seededPhotoM[7:18])
	}
	if got := downloadBlobs.reads.Load(); got != 0 {
		t.Fatalf("m thumbnail made %d blob reads, want none", got)
	}

	for _, thumbSize := range []string{"i", "s", "y"} {
		_, err := api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: thumbSize},
			Limit:    1,
		})
		rpcError(t, err, "LOCATION_INVALID")
	}
	_, err = api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash + 1, ThumbSize: "m"},
		Limit:    1,
	})
	rpcError(t, err, "LOCATION_INVALID")
	stranger, err := s.CreateUser(ctx, "+15551299003")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	_, err = api.GetFileForTest(s, stranger.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Limit:    1,
	})
	rpcError(t, err, "LOCATION_INVALID")
	_, err = api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Offset:   int64(len(seededPhotoM) + 1),
		Limit:    1,
	})
	rpcError(t, err, "LOCATION_INVALID")
	eof, err := api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Offset:   int64(len(seededPhotoM)),
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("read m thumbnail at EOF: %v", err)
	}
	eofFile, ok := eof.(*tg.UploadFile)
	if !ok {
		t.Fatalf("m thumbnail at EOF = %T, want *tg.UploadFile", eof)
	}
	if got := eofFile.Bytes; len(got) != 0 {
		t.Fatalf("m thumbnail at EOF = %q, want empty bytes", got)
	}

	lease, denied, err := s.TryAcquireLimitLease(ctx, recipient.ID, "upload_get_file_in_flight", 1, time.Minute)
	if err != nil || denied != nil || lease == nil {
		t.Fatalf("acquire getFile lease: lease=%v denied=%v err=%v", lease, denied, err)
	}
	_, err = api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Limit:    1,
	})
	rpcError(t, err, "FLOOD_WAIT_1")
	if err := s.ReleaseLimitLease(ctx, lease); err != nil {
		t.Fatalf("release getFile lease: %v", err)
	}

	forwarded, err := api.ForwardMessagesForTest(s, recipient.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerUser(recipient.ID, sender.ID),
		ID:       []int{historyMessage.ID},
		ToPeer:   api.InputPeerUser(recipient.ID, stranger.ID),
		RandomID: []int64{99002},
	})
	if err != nil {
		t.Fatalf("forward photo: %v", err)
	}
	forwardedMessage := messageOf(t, forwarded)
	forwardedPhoto := photoOfMessage(t, forwardedMessage)
	assertPhotoDerivatives(t, forwardedPhoto)
	forwardedFile, err := api.GetFileForTest(s, stranger.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Offset:   0,
		Limit:    len(seededPhotoM),
	})
	if err != nil {
		t.Fatalf("forward recipient m download: %v", err)
	}
	forwardedUpload, ok := forwardedFile.(*tg.UploadFile)
	if !ok {
		t.Fatalf("forward recipient m download = %T, want *tg.UploadFile", forwardedFile)
	}
	if !bytes.Equal(forwardedUpload.Bytes, seededPhotoM) {
		t.Fatalf("forward recipient m bytes = %q, want %q", forwardedUpload.Bytes, seededPhotoM)
	}
	strangerHistory, err := api.GetHistoryForTest(s, stranger.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(stranger.ID, recipient.ID),
	})
	if err != nil {
		t.Fatalf("forward recipient history: %v", err)
	}
	strangerMessages, ok := strangerHistory.(*tg.MessagesMessages)
	if !ok || len(strangerMessages.Messages) != 1 {
		t.Fatalf("forward recipient history = %#v, want one message", strangerHistory)
	}
	strangerMessage, ok := strangerMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("forward recipient message = %T, want *tg.Message", strangerMessages.Messages[0])
	}
	if _, err := s.DeleteMessages(ctx, stranger.ID, []int64{int64(strangerMessage.ID)}, false); err != nil {
		t.Fatalf("delete forwarded photo: %v", err)
	}
	_, err = api.GetFileForTest(s, stranger.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Limit:    1,
	})
	rpcError(t, err, "LOCATION_INVALID")

	original, err := api.GetFileForTest(s, recipient.ID, downloadBlobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "w"},
		Limit:    len(body),
	})
	if err != nil {
		t.Fatalf("download original photo: %v", err)
	}
	originalUpload, ok := original.(*tg.UploadFile)
	if !ok {
		t.Fatalf("original photo download = %T, want *tg.UploadFile", original)
	}
	if !bytes.Equal(originalUpload.Bytes, body) {
		t.Fatalf("original photo bytes changed (%d bytes), want %d", len(originalUpload.Bytes), len(body))
	}

	rateLimited := api.GetFileSeqForTestWithLimits(
		s, downloadBlobs,
		store.RateLimitConfig{Limit: 2, Window: time.Second},
		store.RateLimitConfig{Limit: 3, Window: time.Second},
	)
	requestM := func(userID int64) error {
		_, err := rateLimited(userID, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
			Limit:    1,
		})
		return err
	}
	for range 2 {
		if err := requestM(recipient.ID); err != nil {
			t.Fatalf("allowed account m download: %v", err)
		}
	}
	rpcError(t, requestM(recipient.ID), "FLOOD_WAIT_1")
	if err := requestM(sender.ID); err != nil {
		t.Fatalf("allowed aggregate m download: %v", err)
	}
	rpcError(t, requestM(sender.ID), "FLOOD_WAIT_1")
}

func TestLegacyPhotoDerivativeLocationIsInvalid(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	sender, err := s.CreateUser(ctx, "+15551299011")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551299012")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	body := jpegPhotoPayload(t, 1600, 1600)
	saveParts(t, s, sender.ID, 99011, body)
	result, err := api.SendMediaForTest(s, sender.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(sender.ID, recipient.ID),
		Media:    uploadedPhoto(99011, 1, "photo.jpg", jpegPhotoMD5(body)),
		RandomID: 99011,
	})
	if err != nil {
		t.Fatalf("send legacy photo: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, result))
	if len(photo.Sizes) != 1 {
		t.Fatalf("legacy photo sizes = %d, want original only", len(photo.Sizes))
	}
	_, err = api.GetFileForTest(s, recipient.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Limit:    1,
	})
	rpcError(t, err, "LOCATION_INVALID")
}

func TestSeededPhotoDerivativesRenderAndAuthorizeChannelReaders(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551299021")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551299022")
	if err != nil {
		t.Fatalf("create channel member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551299023")
	if err != nil {
		t.Fatalf("create channel outsider: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo derivatives", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)

	body := jpegPhotoPayload(t, 1600, 1600)
	saveParts(t, s, creator.ID, 99021, body)
	seedPhotoDerivativesBeforePublication(t, dsn)
	sent, err := sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 99021, body, "channel derivatives", 99021)
	if err != nil {
		t.Fatalf("send seeded channel photo: %v", err)
	}
	post, _ := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)
	assertPhotoDerivatives(t, photo)

	historyMessage := channelHistoryPhoto(t, s, member.ID, channel.ID, post.ID)
	assertMessagePhotoDerivatives(t, historyMessage)
	differenceMessage := channelDifferencePhoto(t, s, member.ID, channel.ID, 1, post.ID)
	assertMessagePhotoDerivatives(t, differenceMessage)
	requested, err := api.GetChannelMessagesForTest(s, member.ID, &tg.ChannelsGetMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: post.ID}},
	})
	if err != nil {
		t.Fatalf("get channel photo by id: %v", err)
	}
	requestedMessages := channelMessagesFrom(t, requested).Messages
	if len(requestedMessages) != 1 {
		t.Fatalf("channel getMessages returned %d messages, want one", len(requestedMessages))
	}
	requestedMessage, ok := requestedMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("channel getMessages message = %T, want *tg.Message", requestedMessages[0])
	}
	assertMessagePhotoDerivatives(t, requestedMessage)

	dialogs, err := api.GetDialogsForTest(s, member.ID)
	if err != nil {
		t.Fatalf("channel member dialogs: %v", err)
	}
	dialogResult, ok := dialogs.(*tg.MessagesDialogs)
	if !ok || len(dialogResult.Messages) != 1 {
		t.Fatalf("channel member dialogs = %#v, want one top message", dialogs)
	}
	dialogMessage, ok := dialogResult.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("channel dialog top message = %T, want *tg.Message", dialogResult.Messages[0])
	}
	assertMessagePhotoDerivatives(t, dialogMessage)

	search, err := api.SearchForTest(s, member.ID, &tg.MessagesSearchRequest{
		Peer: channelPeer(member.ID, channel.ID), Q: "derivatives", Filter: &tg.InputMessagesFilterPhotos{}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("channel photo search: %v", err)
	}
	searchMessages := channelMessagesFrom(t, search).Messages
	if len(searchMessages) != 1 {
		t.Fatalf("channel photo search returned %d messages, want one", len(searchMessages))
	}
	searchMessage, ok := searchMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("channel search message = %T, want *tg.Message", searchMessages[0])
	}
	assertMessagePhotoDerivatives(t, searchMessage)

	result, err := api.GetFileForTest(s, member.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		Limit:    len(seededPhotoM),
	})
	if err != nil {
		t.Fatalf("channel member m download: %v", err)
	}
	channelUpload, ok := result.(*tg.UploadFile)
	if !ok {
		t.Fatalf("channel member m download = %T, want *tg.UploadFile", result)
	}
	if !bytes.Equal(channelUpload.Bytes, seededPhotoM) {
		t.Fatalf("channel member m download = %q, want %q", channelUpload.Bytes, seededPhotoM)
	}
	_, err = api.GetFileForTest(s, outsider.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"}, Limit: 1,
	})
	rpcError(t, err, "LOCATION_INVALID")

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to ban channel member: %v", err)
	}
	if _, err = conn.Exec(ctx, `UPDATE channel_participants SET banned_until = now() + interval '1 hour' WHERE channel_id = $1 AND user_id = $2`, channel.ID, member.ID); err != nil {
		t.Fatalf("ban channel member: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close channel ban connection: %v", err)
	}
	_, err = api.GetFileForTest(s, member.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"}, Limit: 1,
	})
	rpcError(t, err, "LOCATION_INVALID")

	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{int64(post.ID)}); err != nil {
		t.Fatalf("tombstone channel photo: %v", err)
	}
	_, err = api.GetFileForTest(s, creator.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"}, Limit: 1,
	})
	rpcError(t, err, "LOCATION_INVALID")
}
