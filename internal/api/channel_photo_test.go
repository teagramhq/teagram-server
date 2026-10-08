package api_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// channelPhotoPostOf extracts the posted channel message and the pts it occupies
// from a sendMedia result. The channel path answers with updateNewChannelMessage,
// so messageOf and messageUpdateOf, which read updateNewMessage, do not apply.
func channelPhotoPostOf(t *testing.T, enc bin.Encoder) (*tg.Message, int) {
	t.Helper()
	updates, ok := enc.(*tg.Updates)
	if !ok {
		t.Fatalf("sendMedia result = %T, want *tg.Updates", enc)
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
		return message, posted.Pts
	}
	t.Fatal("sendMedia result carried no updateNewChannelMessage")
	return nil, 0
}

func sendPhotoToChannel(
	t *testing.T, s *store.Store, blobs blob.Store, userID, channelID, clientFileID int64,
	body []byte, caption string, randomID int64,
) (bin.Encoder, error) {
	t.Helper()
	return api.SendMediaForTest(s, userID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(userID, channelID),
		Media:   uploadedPhoto(clientFileID, 1, "219343.jpg", jpegPhotoMD5(body)),
		Message: caption, RandomID: randomID,
	})
}

func assertChannelPhotoDownload(t *testing.T, s *store.Store, blobs blob.Store, userID int64, photo *tg.Photo, body []byte, thumbSize string) {
	t.Helper()
	result, err := api.GetFileForTest(s, userID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: thumbSize},
		Limit:    len(body),
	})
	if err != nil {
		t.Fatalf("user %d photo download: %v", userID, err)
	}
	file, ok := result.(*tg.UploadFile)
	if !ok {
		t.Fatalf("user %d download = %T, want *tg.UploadFile", userID, result)
	}
	if !bytes.Equal(file.Bytes, body) {
		t.Fatalf("user %d download = %d bytes, want the %d posted bytes unchanged", userID, len(file.Bytes), len(body))
	}
}

func assertSameChannelPhoto(t *testing.T, message *tg.Message, want *tg.Photo, label string) {
	t.Helper()
	wantSize, ok := want.Sizes[0].(*tg.PhotoSize)
	if !ok {
		t.Fatalf("posted photo size = %T, want *tg.PhotoSize", want.Sizes[0])
	}
	got := photoOfMessage(t, message)
	if got.ID != want.ID || got.AccessHash != want.AccessHash || !bytes.Equal(got.FileReference, want.FileReference) {
		t.Fatalf("%s photo = id %d hash %d reference %x, want id %d hash %d reference %x",
			label, got.ID, got.AccessHash, got.FileReference, want.ID, want.AccessHash, want.FileReference)
	}
	if len(got.Sizes) != 1 {
		t.Fatalf("%s photo sizes = %d, want one original", label, len(got.Sizes))
	}
	size, ok := got.Sizes[0].(*tg.PhotoSize)
	if !ok || size.W != wantSize.W || size.H != wantSize.H {
		t.Fatalf("%s photo size = %#v, want the posted %dx%d original", label, got.Sizes[0], wantSize.W, wantSize.H)
	}
}

func channelHistoryPhoto(t *testing.T, s *store.Store, userID, channelID int64, localID int) *tg.Message {
	t.Helper()
	history, err := api.GetHistoryForTest(s, userID, &tg.MessagesGetHistoryRequest{Peer: channelPeer(userID, channelID), Limit: 10})
	if err != nil {
		t.Fatalf("user %d channel history: %v", userID, err)
	}
	// A channel peer answers with messages/channelMessages, not the
	// messages/messages the user and chat peers use.
	var classes []tg.MessageClass
	switch got := history.(type) {
	case *tg.MessagesChannelMessages:
		classes = got.Messages
	case *tg.MessagesMessages:
		classes = got.Messages
	default:
		t.Fatalf("user %d channel history = %T, want a messages list", userID, history)
	}
	for _, class := range classes {
		message, ok := class.(*tg.Message)
		if !ok || message.ID != localID {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPhoto); !ok {
			t.Fatalf("user %d channel history post %d = %T, want a photo", userID, localID, message.Media)
		}
		return message
	}
	t.Fatalf("user %d channel history holds no photo post %d among %d posts", userID, localID, len(classes))
	return nil
}

func channelDifferencePhoto(t *testing.T, s *store.Store, userID, channelID int64, fromPts int, localID int) *tg.Message {
	t.Helper()
	difference, err := api.GetChannelDifferenceForTest(s, userID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(userID, channelID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: fromPts, Limit: 10,
	})
	if err != nil {
		t.Fatalf("user %d channel difference: %v", userID, err)
	}
	page, ok := difference.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("user %d channel difference = %T, want *tg.UpdatesChannelDifference", userID, difference)
	}
	for _, class := range page.NewMessages {
		message, ok := class.(*tg.Message)
		if !ok || message.ID != localID {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPhoto); !ok {
			t.Fatalf("user %d channel difference post %d = %T, want a photo", userID, localID, message.Media)
		}
		return message
	}
	t.Fatalf("user %d channel difference carried no photo post %d among %d updates", userID, localID, len(page.NewMessages))
	return nil
}

// TestChannelPhotoBroadcastPost is the broadcast half of the contract: the
// creator and an admin post, a subscriber is refused with the same answer a
// non-member gets and the refusal writes nothing, documents stay refused for
// everyone, and every subscriber reads the same photo.
func TestChannelPhotoBroadcastPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551297002")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	subscriber, err := s.CreateUser(ctx, "+15551297003")
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Broadcast photos", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	joinChannelByInvite(t, s, channel, subscriber.ID)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, creator.ID, 97001, body)
	sent, err := sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97001, body, "first post", 97011)
	if err != nil {
		t.Fatalf("creator photo post: %v", err)
	}
	post, pts := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)
	if post.Message != "first post" || pts != 2 {
		t.Fatalf("creator post = caption %q pts %d, want the caption at pts 2", post.Message, pts)
	}
	size, ok := photo.Sizes[0].(*tg.PhotoSize)
	if !ok || photo.ID == 0 || photo.AccessHash == 0 || len(photo.FileReference) == 0 || size.W != 640 || size.H != 480 {
		t.Fatalf("creator post photo = id %d hash %d sizes %d, want a photo with the posted original", photo.ID, photo.AccessHash, len(photo.Sizes))
	}

	saveParts(t, s, admin.ID, 97002, body)
	if _, err = sendPhotoToChannel(t, s, blobs, admin.ID, channel.ID, 97002, body, "admin post", 97012); err != nil {
		t.Fatalf("admin photo post: %v", err)
	}

	before := channelWriteStats(t, conn, channel.ID)
	// The listener starts here so the notification it must not see is only the
	// refused send's; the two accepted posts above already fired theirs.
	refusedListener := listenForChannelPosts(t, ctx, dsn)
	saveParts(t, s, subscriber.ID, 97003, body)
	_, err = sendPhotoToChannel(t, s, blobs, subscriber.ID, channel.ID, 97003, body, "subscriber post", 97013)
	rpcError(t, err, "PEER_ID_INVALID")
	assertChannelWriteStats(t, conn, channel.ID, before)
	assertNoChannelPostNotification(t, refusedListener)
	if n, _, _, err := s.UploadPartsSummary(ctx, subscriber.ID, 97003); err != nil || n != 1 {
		t.Fatalf("refused subscriber upload parts = %d, err=%v, want one unconsumed part", n, err)
	}

	// Only the photo type reached a channel: a document send stays refused for
	// the creator too, so this is not a rights question.
	saveParts(t, s, creator.ID, 97004, []byte("eleven bytes"))
	_, err = api.SendMediaForTest(s, creator.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(creator.ID, channel.ID),
		Media:   uploadedDocument(97004, 1, "file.bin", "application/octet-stream"),
		Message: "document", RandomID: 97014,
	})
	rpcError(t, err, "PEER_ID_INVALID")

	assertSameChannelPhoto(t, channelHistoryPhoto(t, s, subscriber.ID, channel.ID, post.ID), photo, "subscriber history")
	assertSameChannelPhoto(t, channelDifferencePhoto(t, s, subscriber.ID, channel.ID, 0, post.ID), photo, "subscriber difference")
	assertChannelPhotoDownload(t, s, blobs, subscriber.ID, photo, body, "x")
}

// TestChannelPhotoSupergroupMemberPost is the megagroup half: an ordinary member
// posts with a caption, and every current member reads the same original
// dimensions and bytes through history and through the channel difference,
// including after the process restarts.
func TestChannelPhotoSupergroupMemberPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297011")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297012")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	watcher, err := s.CreateUser(ctx, "+15551297013")
	if err != nil {
		t.Fatalf("create watcher: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Supergroup photos", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	joinChannelByInvite(t, s, channel, watcher.ID)

	body := jpegPhotoPayload(t, 800, 600)
	saveParts(t, s, member.ID, 97021, body)
	sent, err := sendPhotoToChannel(t, s, blobs, member.ID, channel.ID, 97021, body, "member caption", 97031)
	if err != nil {
		t.Fatalf("member photo post: %v", err)
	}
	post, _ := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)
	if post.Message != "member caption" || !post.Out {
		t.Fatalf("member post = caption %q out=%v, want the caption on an outgoing post", post.Message, post.Out)
	}
	size, ok := photo.Sizes[0].(*tg.PhotoSize)
	if !ok || size.W != 800 || size.H != 600 || size.Size != len(body) || size.Type != "x" {
		t.Fatalf("posted photo size = %#v, want x/800x600/%d", photo.Sizes[0], len(body))
	}

	for _, viewer := range []store.User{creator, watcher} {
		label := fmt.Sprintf("member %d", viewer.ID)
		assertSameChannelPhoto(t, channelHistoryPhoto(t, s, viewer.ID, channel.ID, post.ID), photo, label+" history")
		assertSameChannelPhoto(t, channelDifferencePhoto(t, s, viewer.ID, channel.ID, 0, post.ID), photo, label+" difference")
		assertChannelPhotoDownload(t, s, blobs, viewer.ID, photo, body, "x")
	}

	restarted, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	assertSameChannelPhoto(t, channelHistoryPhoto(t, restarted, watcher.ID, channel.ID, post.ID), photo, "history after restart")
	assertChannelPhotoDownload(t, restarted, blobs, watcher.ID, photo, body, "x")
}

// TestChannelPhotoSupergroupDefaultRights pins the four default-rights cases the
// channel contract names: send_messages, send_media and send_photos each refuse
// an ordinary member while the admin and creator stay exempt, and a
// send_videos-only ban does not reach a photo at all.
func TestChannelPhotoSupergroupDefaultRights(t *testing.T) {
	t.Parallel()
	for i, tc := range []struct {
		right    string
		wantCode string
	}{
		{right: "send_messages", wantCode: "CHAT_WRITE_FORBIDDEN"},
		{right: "send_media", wantCode: "CHAT_WRITE_FORBIDDEN"},
		{right: "send_photos", wantCode: "CHAT_WRITE_FORBIDDEN"},
		{right: "send_videos", wantCode: ""},
	} {
		t.Run(tc.right, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s, dsn := openStoreDSN(t)
			blobs := newBlobs(t)
			base := i * 4
			creator, err := s.CreateUser(ctx, fmt.Sprintf("+155512971%02d", base+1))
			if err != nil {
				t.Fatalf("create creator: %v", err)
			}
			admin, err := s.CreateUser(ctx, fmt.Sprintf("+155512971%02d", base+2))
			if err != nil {
				t.Fatalf("create admin: %v", err)
			}
			member, err := s.CreateUser(ctx, fmt.Sprintf("+155512971%02d", base+3))
			if err != nil {
				t.Fatalf("create member: %v", err)
			}
			channel, err := s.CreateChannel(ctx, creator.ID, "Photo rights "+tc.right, "", true)
			if err != nil {
				t.Fatalf("create megagroup: %v", err)
			}
			joinChannelByInvite(t, s, channel, admin.ID)
			joinChannelByInvite(t, s, channel, member.ID)
			if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
				t.Fatalf("promote admin: %v", err)
			}
			channelExec(t, ctx, dsn, `UPDATE channels SET default_banned_rights = $2 WHERE id = $1`, channel.ID, []string{tc.right})

			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer func() {
				if err := conn.Close(ctx); err != nil {
					t.Errorf("close: %v", err)
				}
			}()
			listener := listenForChannelPosts(t, ctx, dsn)
			body := jpegPhotoPayload(t, 640, 480)

			before := channelWriteStats(t, conn, channel.ID)
			memberFileID, memberRandom := int64(97100+base), int64(97150+base)
			saveParts(t, s, member.ID, memberFileID, body)
			_, err = sendPhotoToChannel(t, s, blobs, member.ID, channel.ID, memberFileID, body, "member", memberRandom)
			if tc.wantCode != "" {
				rpcError(t, err, tc.wantCode)
				assertChannelWriteStats(t, conn, channel.ID, before)
				assertNoChannelPostNotification(t, listener)
				if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, memberFileID); err != nil || n != 1 {
					t.Fatalf("refused member upload parts = %d, err=%v, want one unconsumed part", n, err)
				}
			} else if err != nil {
				t.Fatalf("member photo post under a %s ban: %v", tc.right, err)
			}

			saveParts(t, s, admin.ID, int64(97110+base), body)
			if _, err = sendPhotoToChannel(t, s, blobs, admin.ID, channel.ID, int64(97110+base), body, "admin", int64(97160+base)); err != nil {
				t.Fatalf("admin photo post under a %s ban: %v", tc.right, err)
			}
			saveParts(t, s, creator.ID, int64(97120+base), body)
			if _, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, int64(97120+base), body, "creator", int64(97170+base)); err != nil {
				t.Fatalf("creator photo post under a %s ban: %v", tc.right, err)
			}
		})
	}
}

// TestChannelPhotoSupergroupSlowModeRefusesBeforeAssembly is the precheck's
// reason to exist: a sender inside the slow-mode window is turned away before the
// server assembles an upload for them.
func TestChannelPhotoSupergroupSlowModeRefusesBeforeAssembly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297201")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297202")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Slow photos", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	channelExec(t, ctx, dsn, `UPDATE channels SET slowmode_seconds = 300 WHERE id = $1`, channel.ID)

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, member.ID, 97201, body)
	if _, err = sendPhotoToChannel(t, s, blobs, member.ID, channel.ID, 97201, body, "first", 97211); err != nil {
		t.Fatalf("first member photo post: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	before := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, dsn)

	saveParts(t, s, member.ID, 97202, body)
	_, err = sendPhotoToChannel(t, s, blobs, member.ID, channel.ID, 97202, body, "second", 97212)
	rpcError(t, err, "SLOWMODE_WAIT_300")
	assertChannelWriteStats(t, conn, channel.ID, before)
	if got := countFiles(t, ctx, dsn); got != filesBefore {
		t.Fatalf("file rows after a slow-mode refusal = %d, want %d: the refusal must precede assembly", got, filesBefore)
	}
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, 97202); err != nil || n != 1 {
		t.Fatalf("slow-mode upload parts = %d, err=%v, want one unconsumed part", n, err)
	}
}

// TestChannelPhotoRetryIsIdempotent is the retry contract end to end: the same
// random_id returns the same post at the same pts with one row, after a restart
// and after a restriction the original send would not have passed, while a
// current ban refuses.
func TestChannelPhotoRetryIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297301")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297302")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo retries", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, member.ID, 97301, body)
	req := &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(member.ID, channel.ID),
		Media:   uploadedPhoto(97301, 1, "219343.jpg", jpegPhotoMD5(body)),
		Message: "retry me", RandomID: 97311,
	}
	first, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("member photo post: %v", err)
	}
	firstPost, firstPts := channelPhotoPostOf(t, first)
	firstPhoto := photoOfMessage(t, firstPost)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	posts := channelWriteStats(t, conn, channel.ID)
	files := countFiles(t, ctx, dsn)

	retried, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("photo retry: %v", err)
	}
	retryPost, retryPts := channelPhotoPostOf(t, retried)
	if retryPost.ID != firstPost.ID || retryPts != firstPts || photoOfMessage(t, retryPost).ID != firstPhoto.ID {
		t.Fatalf("retry = post %d pts %d photo %d, want the original %d/%d/%d",
			retryPost.ID, retryPts, photoOfMessage(t, retryPost).ID, firstPost.ID, firstPts, firstPhoto.ID)
	}
	if got := channelWriteStats(t, conn, channel.ID); got != posts {
		t.Fatalf("channel writes after retry = %+v, want %+v", got, posts)
	}
	if got := countFiles(t, ctx, dsn); got != files {
		t.Fatalf("file rows after retry = %d, want %d: a committed retry assembles nothing", got, files)
	}

	channelExec(t, ctx, dsn, `UPDATE channels SET default_banned_rights = $2 WHERE id = $1`, channel.ID, []string{"send_photos"})
	afterBan, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("photo retry after a send_photos ban: %v", err)
	}
	bannedPost, bannedPts := channelPhotoPostOf(t, afterBan)
	if bannedPost.ID != firstPost.ID || bannedPts != firstPts {
		t.Fatalf("retry after a later ban = post %d pts %d, want the original %d/%d", bannedPost.ID, bannedPts, firstPost.ID, firstPts)
	}

	restarted, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Errorf("close reopened store: %v", err)
		}
	})
	restartedSend, err := api.SendMediaForTest(restarted, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("photo retry after restart: %v", err)
	}
	restartedPost, restartedPts := channelPhotoPostOf(t, restartedSend)
	if restartedPost.ID != firstPost.ID || restartedPts != firstPts {
		t.Fatalf("retry after restart = post %d pts %d, want the original %d/%d", restartedPost.ID, restartedPts, firstPost.ID, firstPts)
	}

	until := time.Now().Add(time.Hour)
	if err = s.SetChannelBan(ctx, channel.ID, creator.ID, member.ID, &until, false); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	rpcError(t, err, "PEER_ID_INVALID")
}

// TestChannelPhotoRetryRefusesForeignAndOtherMedia is the negative half: a
// random_id that does not name this caller's own live photo post is a refusal,
// never a replay of something else.
func TestChannelPhotoRetryRefusesForeignAndOtherMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297311")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551297312")
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo retry refusals", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, other.ID)

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, other.ID, 97321, body)
	theirs, err := sendPhotoToChannel(t, s, blobs, other.ID, channel.ID, 97321, body, "theirs", 97331)
	if err != nil {
		t.Fatalf("other member photo post: %v", err)
	}
	theirPost, _ := channelPhotoPostOf(t, theirs)

	if _, _, _, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "text post", 97332, nil, 0); err != nil {
		t.Fatalf("creator text post: %v", err)
	}
	saveParts(t, s, creator.ID, 97322, body)
	if _, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97322, body, "mine", 97333); err != nil {
		t.Fatalf("creator photo post: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	before := channelWriteStats(t, conn, channel.ID)

	// The caller's own text post under that random_id is not a photo send that
	// already landed.
	saveParts(t, s, creator.ID, 97323, body)
	_, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97323, body, "text replay", 97332)
	rpcError(t, err, "MEDIA_INVALID")
	// Another author's photo random_id belongs to that author.
	saveParts(t, s, creator.ID, 97324, body)
	_, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97324, body, "foreign replay", 97331)
	rpcError(t, err, "RANDOM_ID_DUPLICATE")
	if got := channelWriteStats(t, conn, channel.ID); got != before {
		t.Fatalf("channel writes after refused replays = %+v, want %+v", got, before)
	}

	// A tombstoned photo post is not replayable either, and the refusal is the
	// same one the text path gives for a deleted original. The takedown itself is
	// one event, so the baseline is taken after it.
	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, other.ID, []int64{int64(theirPost.ID)}); err != nil {
		t.Fatalf("tombstone the photo post: %v", err)
	}
	before = channelWriteStats(t, conn, channel.ID)
	saveParts(t, s, other.ID, 97325, body)
	_, err = sendPhotoToChannel(t, s, blobs, other.ID, channel.ID, 97325, body, "tombstone replay", 97331)
	rpcError(t, err, "RANDOM_ID_DUPLICATE")
	if got := channelWriteStats(t, conn, channel.ID); got != before {
		t.Fatalf("channel writes after a refused tombstone replay = %+v, want %+v", got, before)
	}
}

// TestChannelPhotoDownloadGate is the read side: every current member gets the
// identical bytes, and every way of not being one gets the same LOCATION_INVALID.
func TestChannelPhotoDownloadGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297401")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297402")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	leaver, err := s.CreateUser(ctx, "+15551297403")
	if err != nil {
		t.Fatalf("create leaver: %v", err)
	}
	stranger, err := s.CreateUser(ctx, "+15551297404")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo download gate", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	joinChannelByInvite(t, s, channel, leaver.ID)

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, creator.ID, 97401, body)
	sent, err := sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97401, body, "gate", 97411)
	if err != nil {
		t.Fatalf("creator photo post: %v", err)
	}
	post, _ := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)

	assertChannelPhotoDownload(t, s, blobs, member.ID, photo, body, "x")

	if left, err := s.LeaveChannel(ctx, channel.ID, leaver.ID); err != nil || !left {
		t.Fatalf("leave channel: left=%v err=%v", left, err)
	}
	until := time.Now().Add(time.Hour)
	if err = s.SetChannelBan(ctx, channel.ID, creator.ID, member.ID, &until, false); err != nil {
		t.Fatalf("ban member: %v", err)
	}

	for name, userID := range map[string]int64{"nonmember": stranger.ID, "leaver": leaver.ID, "banned member": member.ID} {
		if _, err := api.GetFileForTest(s, userID, blobs, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "x"},
			Limit:    len(body),
		}); err == nil {
			t.Fatalf("%s downloaded a channel photo they are no longer entitled to", name)
		} else {
			rpcError(t, err, "LOCATION_INVALID")
		}
	}
	for name, location := range map[string]tg.InputFileLocationClass{
		"wrong access hash": &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash ^ 0x5a5a, ThumbSize: "x"},
		"wrong size type":   &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "m"},
		"document location": &tg.InputDocumentFileLocation{ID: photo.ID, AccessHash: photo.AccessHash},
	} {
		if _, err := api.GetFileForTest(s, creator.ID, blobs, &tg.UploadGetFileRequest{Location: location, Limit: len(body)}); err == nil {
			t.Fatalf("%s served a channel photo", name)
		} else {
			rpcError(t, err, "LOCATION_INVALID")
		}
	}

	if _, _, err := s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{int64(post.ID)}); err != nil {
		t.Fatalf("tombstone the photo post: %v", err)
	}
	if _, err := api.GetFileForTest(s, creator.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "x"},
		Limit:    len(body),
	}); err == nil {
		t.Fatal("a tombstoned post still serves its photo")
	} else {
		rpcError(t, err, "LOCATION_INVALID")
	}
}

// TestChannelPhotoForeignUploadIDKeepsOwnerParts: the file a channel post names
// is always one this caller's own upload just produced. Naming another account's
// upload id is refused and leaves that account's parts alone.
func TestChannelPhotoForeignUploadIDKeepsOwnerParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	blobs := newBlobs(t)
	owner, err := s.CreateUser(ctx, "+15551297501")
	if err != nil {
		t.Fatalf("create upload owner: %v", err)
	}
	creator, err := s.CreateUser(ctx, "+15551297502")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Foreign upload", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, owner.ID, 97501, body)
	_, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97501, body, "not mine", 97511)
	rpcError(t, err, "MEDIA_INVALID")
	if n, _, _, err := s.UploadPartsSummary(ctx, owner.ID, 97501); err != nil || n != 1 {
		t.Fatalf("owner upload parts after a foreign send = %d, err=%v, want one retained part", n, err)
	}
	rows, err := s.ChannelHistory(ctx, channel.ID, 0, 10)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("channel holds %d posts after a foreign upload send, want the create service message only", len(rows))
	}
}

// TestChannelPhotoBanDuringAssemblyRefusesPost is the race the read-only
// precheck cannot close: the sender is authorized when assembly starts and is not
// when the post transaction runs. The authoritative check under the channel state
// lock refuses, and the file the send assembled stays unreferenced.
func TestChannelPhotoBanDuringAssemblyRefusesPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	member, err := h.store.CreateUser(ctx, "+15551990102")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := h.store.CreateChannel(ctx, h.user.ID, "Ban during assembly", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	hash, err := h.store.CreateChannelInvite(ctx, channel.ID, h.user.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = h.store.JoinChannelByInvite(ctx, hash, member.ID); err != nil {
		t.Fatalf("join member: %v", err)
	}

	body := jpegPhotoPayload(t, 640, 480)
	const clientFileID, randomID = int64(97601), int64(97611)
	saveParts(t, h.store, member.ID, clientFileID, body)
	refs, err := h.store.UploadPartRefs(ctx, member.ID, clientFileID)
	if err != nil {
		t.Fatalf("upload part refs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("upload part refs = %d, want one", len(refs))
	}
	h.partBlob.key = refs[0].Key
	h.partBlob.readStarted = make(chan struct{})
	h.partBlob.releaseRead = make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case h.partBlob.releaseRead <- struct{}{}:
		default:
		}
	})

	sent := make(chan error, 1)
	go func() {
		_, err := api.SendMediaForTest(h.store, member.ID, h.local, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer:    channelPeer(member.ID, channel.ID),
			Media:   uploadedPhoto(clientFileID, 1, "219343.jpg", jpegPhotoMD5(body)),
			Message: "banned mid-assembly", RandomID: randomID,
		})
		sent <- err
	}()

	<-h.partBlob.readStarted
	until := time.Now().Add(time.Hour)
	if err = h.store.SetChannelBan(ctx, channel.ID, h.user.ID, member.ID, &until, false); err != nil {
		t.Fatalf("ban member during assembly: %v", err)
	}
	h.partBlob.releaseRead <- struct{}{}

	if err := <-sent; mediaAssemblyRPCCode(err) != "PEER_ID_INVALID" {
		t.Fatalf("send after a ban during assembly = %v, want PEER_ID_INVALID", err)
	}
	rows, err := h.store.ChannelHistory(ctx, channel.ID, 0, 10)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("channel holds %d posts after a ban won during assembly, want the create service message only", len(rows))
	}
	for _, row := range rows {
		if row.FileID != nil {
			t.Fatalf("channel post %d carries file %d after a refused send", row.LocalID, *row.FileID)
		}
	}
}

// TestChannelPhotoForwardAndTakedown covers the two contracts that only exist
// once a channel post can carry a photo: a basic group's send_photos ban reaches
// a channel-source forward, and a takedown closes the source download while the
// copy an authorized member already made stays downloadable.
func TestChannelPhotoForwardAndTakedown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297601")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297602")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551297603")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo takedown", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	chat, err := s.CreateChat(ctx, peer.ID, "Photo forward group", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()

	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, s, member.ID, 97701, body)
	sent, err := sendPhotoToChannel(t, s, blobs, member.ID, channel.ID, 97701, body, "source", 97711)
	if err != nil {
		t.Fatalf("member photo post: %v", err)
	}
	post, _ := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)

	forwarded, err := api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: channelPeer(member.ID, channel.ID), ID: []int{post.ID},
		ToPeer: api.InputPeerUser(member.ID, peer.ID), RandomID: []int64{97712},
	})
	if err != nil {
		t.Fatalf("forward channel photo to a user: %v", err)
	}
	if got := photoOfMessage(t, messageOf(t, forwarded)); got.ID != photo.ID {
		t.Fatalf("forwarded photo id = %d, want the source %d", got.ID, photo.ID)
	}
	assertChannelPhotoDownload(t, s, blobs, peer.ID, photo, body, "x")

	setChatDefaultRights(t, conn, chat.ID, "send_photos")
	_, err = api.ForwardMessagesForTest(s, member.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: channelPeer(member.ID, channel.ID), ID: []int{post.ID},
		ToPeer: api.InputPeerChat(member.ID, chat.ID), RandomID: []int64{97713},
	})
	rpcError(t, err, "CHAT_WRITE_FORBIDDEN")

	if _, _, err := s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{int64(post.ID)}); err != nil {
		t.Fatalf("takedown the photo post: %v", err)
	}
	if _, err := api.GetFileForTest(s, creator.ID, blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: photo.ID, AccessHash: photo.AccessHash, ThumbSize: "x"},
		Limit:    len(body),
	}); err == nil {
		t.Fatal("the source download stayed open after a takedown")
	} else {
		rpcError(t, err, "LOCATION_INVALID")
	}
	assertChannelPhotoDownload(t, s, blobs, peer.ID, photo, body, "x")
}
