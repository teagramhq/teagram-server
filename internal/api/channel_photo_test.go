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
// never a replay of something else, and foreign author, tombstone and media-kind
// mismatches all answer the one code.
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
	// Another author's photo random_id belongs to that author, and it answers the
	// same MEDIA_INVALID as a kind mismatch: a photo retry refuses every id it may
	// not replay with one code, so the refusal is not a way to read which
	// post the id names.
	saveParts(t, s, creator.ID, 97324, body)
	_, err = sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 97324, body, "foreign replay", 97331)
	rpcError(t, err, "MEDIA_INVALID")
	if got := channelWriteStats(t, conn, channel.ID); got != before {
		t.Fatalf("channel writes after refused replays = %+v, want %+v", got, before)
	}

	// A tombstoned photo post is not replayable either, with the same refusal as
	// the send whose file was already erased. The takedown itself is one event, so
	// the baseline is taken after it.
	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, other.ID, []int64{int64(theirPost.ID)}); err != nil {
		t.Fatalf("tombstone the photo post: %v", err)
	}
	before = channelWriteStats(t, conn, channel.ID)
	saveParts(t, s, other.ID, 97325, body)
	_, err = sendPhotoToChannel(t, s, blobs, other.ID, channel.ID, 97325, body, "tombstone replay", 97331)
	rpcError(t, err, "MEDIA_INVALID")
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

// assertChannelPhotoReply checks the wire reply header a channel photo post
// carries: the parent's local id plus the channel peer, which is what lets a
// client render the quote in the send response, in history and in the channel
// difference.
func assertChannelPhotoReply(t *testing.T, message *tg.Message, parentID int, channelID int64, label string) {
	t.Helper()
	class, ok := message.GetReplyTo()
	if !ok {
		t.Fatalf("%s carries no reply header, want a reply to post %d", label, parentID)
	}
	header, ok := class.(*tg.MessageReplyHeader)
	if !ok {
		t.Fatalf("%s reply header = %T, want *tg.MessageReplyHeader", label, class)
	}
	if header.ReplyToMsgID != parentID {
		t.Fatalf("%s reply id = %d, want %d", label, header.ReplyToMsgID, parentID)
	}
	peer, ok := header.GetReplyToPeerID()
	if !ok {
		t.Fatalf("%s reply header carries no peer, want channel %d", label, channelID)
	}
	peerChannel, ok := peer.(*tg.PeerChannel)
	if !ok || peerChannel.ChannelID != channelID {
		t.Fatalf("%s reply peer = %+v, want channel %d", label, peer, channelID)
	}
}

func sendPhotoReplyToChannel(
	t *testing.T, s *store.Store, blobs blob.Store, userID, channelID, clientFileID int64,
	body []byte, caption string, randomID int64, replyTo tg.InputReplyToClass,
) (bin.Encoder, error) {
	t.Helper()
	return api.SendMediaForTest(s, userID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    channelPeer(userID, channelID),
		Media:   uploadedPhoto(clientFileID, 1, "219343.jpg", jpegPhotoMD5(body)),
		Message: caption, RandomID: randomID, ReplyTo: replyTo,
	})
}

// TestChannelPhotoReplyKeepsItsParent is the reply contract a channel text post
// already has: a photo send naming a live post stores that parent and every
// read path renders it, while a reply target that is absent, tombstoned or in
// another channel is refused before the server assembles anything.
func TestChannelPhotoReplyKeepsItsParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+15551297801")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297802")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo replies", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	otherChannel, err := s.CreateChannel(ctx, creator.ID, "Other channel", "", true)
	if err != nil {
		t.Fatalf("create other channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)

	parent, _, _, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "parent post", 97811, nil, 0)
	if err != nil {
		t.Fatalf("parent post: %v", err)
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
	saveParts(t, s, member.ID, 97801, body)
	req := &tg.MessagesSendMediaRequest{
		Peer:     channelPeer(member.ID, channel.ID),
		Media:    uploadedPhoto(97801, 1, "219343.jpg", jpegPhotoMD5(body)),
		Message:  "replying",
		RandomID: 97812,
		ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: int(parent.LocalID)},
	}
	sent, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("photo reply: %v", err)
	}
	post, pts := channelPhotoPostOf(t, sent)
	if post.ID == int(parent.LocalID) {
		t.Fatalf("photo reply took the parent's own id %d", post.ID)
	}
	assertChannelPhotoReply(t, post, int(parent.LocalID), channel.ID, "send response")

	photo := photoOfMessage(t, post)
	assertChannelPhotoReply(t, channelHistoryPhoto(t, s, creator.ID, channel.ID, post.ID), int(parent.LocalID), channel.ID, "creator history")
	assertChannelPhotoReply(t, channelDifferencePhoto(t, s, creator.ID, channel.ID, 0, post.ID), int(parent.LocalID), channel.ID, "creator difference")

	// The retry replay renders the stored post, so it carries the same parent.
	replayed, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("photo reply retry: %v", err)
	}
	replayPost, replayPts := channelPhotoPostOf(t, replayed)
	if replayPost.ID != post.ID || replayPts != pts || photoOfMessage(t, replayPost).ID != photo.ID {
		t.Fatalf("retry = post %d pts %d photo %d, want the original %d/%d/%d",
			replayPost.ID, replayPts, photoOfMessage(t, replayPost).ID, post.ID, pts, photo.ID)
	}
	assertChannelPhotoReply(t, replayPost, int(parent.LocalID), channel.ID, "retry response")

	// A reply form naming another peer is refused by the parse itself, before the
	// server assembles anything: no file row, no post, no event, and the upload
	// stays where the client put it.
	before := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, dsn)
	for name, replyTo := range map[string]tg.InputReplyToClass{
		"other channel": &tg.InputReplyToMessage{ReplyToMsgID: int(parent.LocalID), ReplyToPeerID: channelPeer(member.ID, otherChannel.ID)},
		"other user":    &tg.InputReplyToMessage{ReplyToMsgID: int(parent.LocalID), ReplyToPeerID: api.InputPeerUser(member.ID, creator.ID)},
	} {
		fileID, randomID := int64(97820+len(name)), int64(97830+len(name))
		saveParts(t, s, member.ID, fileID, body)
		_, err = sendPhotoReplyToChannel(t, s, blobs, member.ID, channel.ID, fileID, body, name, randomID, replyTo)
		rpcError(t, err, "MESSAGE_ID_INVALID")
		if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, fileID); err != nil || n != 1 {
			t.Fatalf("%s: upload parts = %d, err=%v, want one unconsumed part", name, n, err)
		}
	}
	assertChannelWriteStats(t, conn, channel.ID, before)
	if got := countFiles(t, ctx, dsn); got != filesBefore {
		t.Fatalf("file rows after refused reply peers = %d, want %d: the refusal must precede assembly", got, filesBefore)
	}

	// A reply id naming a post that is not live in this channel is decided inside
	// the post transaction, which runs after assembly. The refusal still writes no
	// post and no event; the file the send built is left unreferenced, charged to
	// the sender and reclaimable, exactly as a refused ban is.
	absentFileID := int64(97850)
	saveParts(t, s, member.ID, absentFileID, body)
	_, err = sendPhotoReplyToChannel(t, s, blobs, member.ID, channel.ID, absentFileID, body, "absent parent", 97851,
		&tg.InputReplyToMessage{ReplyToMsgID: int(parent.LocalID) + 500})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	assertChannelWriteStats(t, conn, channel.ID, before)
	if got := countFiles(t, ctx, dsn); got != filesBefore+1 {
		t.Fatalf("file rows after a reply to an absent post = %d, want %d: that refusal follows assembly", got, filesBefore+1)
	}
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, absentFileID); err != nil || n != 0 {
		t.Fatalf("absent parent: upload parts = %d, err=%v, want them consumed by the assembly", n, err)
	}

	// A tombstoned parent answers the same way, so channel post ids are not an
	// existence oracle.
	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{parent.LocalID}); err != nil {
		t.Fatalf("tombstone the parent: %v", err)
	}
	afterTakedown := channelWriteStats(t, conn, channel.ID)
	filesAfterTakedown := countFiles(t, ctx, dsn)
	saveParts(t, s, member.ID, 97861, body)
	_, err = sendPhotoReplyToChannel(t, s, blobs, member.ID, channel.ID, 97861, body, "tombstoned parent", 97871,
		&tg.InputReplyToMessage{ReplyToMsgID: int(parent.LocalID)})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	assertChannelWriteStats(t, conn, channel.ID, afterTakedown)
	if got := countFiles(t, ctx, dsn); got != filesAfterTakedown+1 {
		t.Fatalf("file rows after a reply to a tombstoned parent = %d, want %d", got, filesAfterTakedown+1)
	}
	if n, _, _, err := s.UploadPartsSummary(ctx, member.ID, 97861); err != nil || n != 0 {
		t.Fatalf("tombstoned parent: upload parts = %d, err=%v, want them consumed by the assembly", n, err)
	}

	// A reply form with no message id means "reply to this channel" and stores no
	// parent, exactly as the text path reads it, so the send still posts.
	saveParts(t, s, member.ID, 97862, body)
	bare, err := sendPhotoReplyToChannel(t, s, blobs, member.ID, channel.ID, 97862, body, "no parent", 97872,
		&tg.InputReplyToStory{Peer: channelPeer(member.ID, channel.ID), StoryID: int(parent.LocalID)})
	if err != nil {
		t.Fatalf("photo send with a channel-level reply form: %v", err)
	}
	barePost, _ := channelPhotoPostOf(t, bare)
	if _, ok := barePost.GetReplyTo(); ok {
		t.Fatalf("channel-level reply form stored a parent on post %d, want none", barePost.ID)
	}
}

// holdChannelStateBarrier takes the channel_state row lock on a side connection
// and hands back that connection; committing or rolling it back releases the
// lock. A channel photo send parks on this lock in the dedup read that opens its
// retry transaction, so holding it lets a test release several sends at once,
// after all of them have finished everything cheap.
func holdChannelStateBarrier(t *testing.T, ctx context.Context, dsn string, channelID int64) *pgx.Conn {
	t.Helper()
	barrier, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect the channel state barrier: %v", err)
	}
	t.Cleanup(func() {
		if err := barrier.Close(context.Background()); err != nil {
			t.Errorf("close the channel state barrier: %v", err)
		}
	})
	if _, err = barrier.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin the channel state barrier: %v", err)
	}
	var locked int64
	if err = barrier.QueryRow(ctx, `SELECT channel_id FROM channel_state WHERE channel_id = $1 FOR UPDATE`, channelID).Scan(&locked); err != nil {
		t.Fatalf("lock the channel state: %v", err)
	}
	if locked != channelID {
		t.Fatalf("barrier locked channel %d, want %d", locked, channelID)
	}
	return barrier
}

// TestChannelPhotoConcurrentSameRandomIDPostsOnce is the duplicate the retry
// contract has to survive: two sends with the same random_id, each with its own
// assembled upload, reaching the post transaction while the other is still in
// flight. One post, one event, one notification, both callers reading the same
// photo at the same pts, and the loser's assembly left behind as an unreferenced
// file the eraser reclaims while the post's own photo stays downloadable.
//
// channel_state is held FOR UPDATE on a side connection for the whole
// window. That is what makes this the transactional duplicate rather than the
// cheap pre-assembly lookup: neither send can post, so both assemble, and the
// decision is made by the dedup read under the lock once the barrier lifts.
func TestChannelPhotoConcurrentSameRandomIDPostsOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	// One blob store behind assembly and the eraser's unlink, the way the
	// process wires them.
	blobs := newBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close store: %v", cerr)
		}
	})
	creator, err := s.CreateUser(ctx, "+15551297901")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297902")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo duplicate race", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)

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
	before := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, dsn)

	barrier := holdChannelStateBarrier(t, ctx, dsn, channel.ID)

	// Two different uploads, so which photo a response names is observable and a
	// reply that rendered the loser's file instead of the stored post cannot pass.
	firstBody, secondBody := jpegPhotoPayload(t, 640, 480), jpegPhotoPayload(t, 800, 600)
	const firstFileID, secondFileID = int64(97901), int64(97902)
	saveParts(t, s, member.ID, firstFileID, firstBody)
	saveParts(t, s, member.ID, secondFileID, secondBody)

	type result struct {
		post  *tg.Message
		pts   int
		photo *tg.Photo
		err   error
	}
	send := func(fileID int64, body []byte) chan result {
		done := make(chan result, 1)
		go func() {
			sent, err := api.SendMediaForTest(s, member.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
				Peer:     channelPeer(member.ID, channel.ID),
				Media:    uploadedPhoto(fileID, 1, "219343.jpg", jpegPhotoMD5(body)),
				Message:  "same random id",
				RandomID: 97911,
			})
			if err != nil {
				done <- result{err: err}
				return
			}
			post, pts := channelPhotoPostOf(t, sent)
			done <- result{post: post, pts: pts, photo: photoOfMessage(t, post)}
		}()
		return done
	}
	firstSend, secondSend := send(firstFileID, firstBody), send(secondFileID, secondBody)

	// Both sends park at the channel_state lock their dedup lookup takes, which
	// is the first thing either does. Lifting the barrier there means neither
	// sees the other's post in that lookup, so both go on to assemble and the
	// duplicate is decided inside the post transaction.
	waitForChannelStateWaiters(t, ctx, conn, 2)
	if _, err = barrier.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("release the barrier: %v", err)
	}

	both := make([]result, 0, 2)
	for range 2 {
		select {
		case got := <-firstSend:
			if got.err != nil {
				t.Fatalf("first concurrent photo send: %v", got.err)
			}
			both = append(both, got)
		case got := <-secondSend:
			if got.err != nil {
				t.Fatalf("second concurrent photo send: %v", got.err)
			}
			both = append(both, got)
		case <-ctx.Done():
			t.Fatalf("waiting for the concurrent photo sends: %s", ctx.Err())
		}
	}
	a, b := both[0], both[1]
	if a.post.ID != b.post.ID || a.pts != b.pts || a.photo.ID != b.photo.ID || a.photo.AccessHash != b.photo.AccessHash {
		t.Fatalf("the two sends answered post %d/pts %d/photo %d:%d and post %d/pts %d/photo %d:%d, want one post at one pts naming one photo",
			a.post.ID, a.pts, a.photo.ID, a.photo.AccessHash, b.post.ID, b.pts, b.photo.ID, b.photo.AccessHash)
	}

	// One post and one event, while both sends did leave a file row behind.
	after := channelWriteStats(t, conn, channel.ID)
	if after.messages != before.messages+1 || after.events != before.events+1 {
		t.Fatalf("channel writes after the race = %+v, want one post and one event over %+v", after, before)
	}
	if got := countFiles(t, ctx, dsn); got != filesBefore+2 {
		t.Fatalf("file rows after the race = %d, want %d: both sends assembled before one lost the lock", got, filesBefore+2)
	}

	// Exactly one notification: the loser's transaction found the committed
	// random_id and fired nothing.
	notifyCtx, cancelNotify := context.WithTimeout(ctx, 5*time.Second)
	defer cancelNotify()
	if _, err = listener.WaitForNotification(notifyCtx); err != nil {
		t.Fatalf("waiting for the single channel post notification: %v", err)
	}
	assertNoChannelPostNotification(t, listener)

	// The stored post names one of the two files; the other is the loser's
	// orphan, and the photo dimensions say which body produced which.
	winnerFileID := a.photo.ID
	size, ok := a.photo.Sizes[0].(*tg.PhotoSize)
	if !ok || (size.W != 640 && size.W != 800) {
		t.Fatalf("posted photo size = %#v, want the 640x480 or the 800x600 original", a.photo.Sizes[0])
	}
	winnerBody, loserBody := firstBody, secondBody
	if size.W == 800 {
		winnerBody, loserBody = secondBody, firstBody
	}
	// The post's photo names one files row and the other upload is the orphan.
	// The rows are told apart by their recorded size, since a client upload id is
	// not a files id: assembly allocates its own.
	var storedSize int64
	if err := conn.QueryRow(ctx, `SELECT size FROM files WHERE id = $1`, winnerFileID).Scan(&storedSize); err != nil {
		t.Fatalf("read the post's file row: %v", err)
	}
	if storedSize != int64(len(winnerBody)) {
		t.Fatalf("the post names file %d of %d bytes, want the winner's upload of %d bytes",
			winnerFileID, storedSize, len(winnerBody))
	}
	var orphanRows int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE id <> $1 AND size = $2`, winnerFileID, len(loserBody)).Scan(&orphanRows); err != nil {
		t.Fatalf("count the orphan row: %v", err)
	}
	if orphanRows != 1 {
		t.Fatalf("orphan file rows = %d, want 1: the race has to reach the post transaction with two completed assemblies", orphanRows)
	}

	// The eraser reclaims exactly the orphan and leaves the post's photo
	// downloadable, byte for byte.
	counts, err := s.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("sweep media erasure: %v", err)
	}
	if counts.Erased != 1 {
		t.Fatalf("sweep counts = %+v, want the one orphan erased", counts)
	}
	if counts.ErasedBytes != int64(len(loserBody)) {
		t.Fatalf("sweep erased %d bytes, want the losing upload's %d: the post's own photo must be held back",
			counts.ErasedBytes, len(loserBody))
	}
	var kept, orphans int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE id = $1`, winnerFileID).Scan(&kept); err != nil {
		t.Fatalf("count the post's file: %v", err)
	}
	if kept != 1 {
		t.Fatalf("the post's own file %d is gone after the sweep", winnerFileID)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM files WHERE id <> $1 AND size = $2`, winnerFileID, len(loserBody)).Scan(&orphans); err != nil {
		t.Fatalf("count the orphan: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("the losing upload's file row survived the sweep")
	}
	if got := countFiles(t, ctx, dsn); got != filesBefore+1 {
		t.Fatalf("file rows after the sweep = %d, want %d", got, filesBefore+1)
	}
	rows, err := s.ChannelHistory(ctx, channel.ID, 0, 10)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("channel holds %d posts after the sweep, want the create message and one photo post", len(rows))
	}
	assertChannelPhotoDownload(t, s, blobs, creator.ID, a.photo, winnerBody, "x")
}

// waitForChannelStateWaiters waits until want request backends are parked on the
// channel_state row lock, which is where a channel photo send stops while
// another transaction holds that row.
func waitForChannelStateWaiters(t *testing.T, ctx context.Context, conn *pgx.Conn, want int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active' AND wait_event_type = 'Lock'
			  AND query LIKE '%LockChannelState%'`).Scan(&waiting); err != nil {
			t.Fatalf("count channel state waiters: %v", err)
		}
		if waiting >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d backends parked on the channel state lock, want %d", waiting, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestChannelPhotoConcurrentTwoAuthorsSameRandomIDRefusesLoser is the
// transactional half of the foreign-author refusal. Two members post with the
// same random_id while channel_state is held, so neither sees the other in
// the cheap pre-assembly lookup, both assemble, and the dedup read inside the
// post transaction is what finds the other author's row. The winner posts; the
// loser is refused with MEDIA_INVALID, the same answer its own retry would give
// it after assembly, and the channel publishes one post, one event and one
// notification. The loser's completed assembly stays behind unreferenced.
func TestChannelPhotoConcurrentTwoAuthorsSameRandomIDRefusesLoser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	blobs := newBlobs(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if cerr := s.Close(); cerr != nil {
			t.Errorf("close store: %v", cerr)
		}
	})
	creator, err := s.CreateUser(ctx, "+15551298001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	firstMember, err := s.CreateUser(ctx, "+15551298002")
	if err != nil {
		t.Fatalf("create first member: %v", err)
	}
	secondMember, err := s.CreateUser(ctx, "+15551298003")
	if err != nil {
		t.Fatalf("create second member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo cross-author race", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, s, channel, firstMember.ID)
	joinChannelByInvite(t, s, channel, secondMember.ID)

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
	before := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, dsn)
	barrier := holdChannelStateBarrier(t, ctx, dsn, channel.ID)

	// Each author uploads their own photo, so the stored row names one of
	// them and the refused send is identifiable by its upload.
	firstBody, secondBody := jpegPhotoPayload(t, 640, 480), jpegPhotoPayload(t, 800, 600)
	const randomID = int64(97943)
	authors := []struct {
		userID int64
		fileID int64
		body   []byte
	}{
		{userID: firstMember.ID, fileID: 97941, body: firstBody},
		{userID: secondMember.ID, fileID: 97942, body: secondBody},
	}
	for _, author := range authors {
		saveParts(t, s, author.userID, author.fileID, author.body)
	}

	type result struct {
		userID int64
		fileID int64
		body   []byte
		post   *tg.Message
		photo  *tg.Photo
		err    error
	}
	sends := make([]chan result, 0, len(authors))
	for _, author := range authors {
		done := make(chan result, 1)
		sends = append(sends, done)
		go func(author struct {
			userID int64
			fileID int64
			body   []byte
		}) {
			sent, err := api.SendMediaForTest(s, author.userID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
				Peer:     channelPeer(author.userID, channel.ID),
				Media:    uploadedPhoto(author.fileID, 1, "219343.jpg", jpegPhotoMD5(author.body)),
				Message:  "cross-author same random id",
				RandomID: randomID,
			})
			if err != nil {
				done <- result{userID: author.userID, fileID: author.fileID, body: author.body, err: err}
				return
			}
			post, _ := channelPhotoPostOf(t, sent)
			done <- result{userID: author.userID, fileID: author.fileID, body: author.body, post: post, photo: photoOfMessage(t, post)}
		}(author)
	}

	// Both sends park on the channel_state lock their dedup read takes, so
	// neither finds the other there and both go on to assemble.
	waitForChannelStateWaiters(t, ctx, conn, 2)
	if _, err = barrier.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("release the barrier: %v", err)
	}

	results := make([]result, 0, len(authors))
	for _, done := range sends {
		select {
		case got := <-done:
			results = append(results, got)
		case <-ctx.Done():
			t.Fatalf("waiting for the cross-author photo sends: %s", ctx.Err())
		}
	}
	var winner, loser result
	for _, got := range results {
		if got.err == nil {
			winner = got
		} else {
			loser = got
		}
	}
	if winner.userID == 0 || loser.userID == 0 {
		t.Fatalf("expected one accepted and one refused send, got %+v and %+v", results[0], results[1])
	}
	rpcError(t, loser.err, "MEDIA_INVALID")
	if winner.userID == loser.userID {
		t.Fatalf("one author both posted and was refused: user %d", winner.userID)
	}

	// The stored row belongs to the winner and names the winner's upload, so the
	// refusal the loser got came from the post transaction finding that row.
	var storedFrom, storedFile int64
	var storedLocal int64
	if err = conn.QueryRow(ctx, `SELECT from_id, file_id, local_id FROM channel_messages
		WHERE channel_id = $1 AND random_id = $2`, channel.ID, randomID).Scan(&storedFrom, &storedFile, &storedLocal); err != nil {
		t.Fatalf("read the stored post: %v", err)
	}
	if storedFrom != winner.userID {
		t.Fatalf("stored post author = %d, want the accepted send's %d", storedFrom, winner.userID)
	}
	if winner.post.ID != int(storedLocal) || winner.photo.ID != storedFile {
		t.Fatalf("winner response = post %d photo %d, want post %d naming the stored file %d",
			winner.post.ID, winner.photo.ID, storedLocal, storedFile)
	}
	// The stored files row is the winner's own assembly: its uploader and its
	// recorded byte count both say so. A client upload id is not a files id, so
	// the row is matched on what assembly recorded.
	var storedUploader int64
	var storedSize int64
	if err = conn.QueryRow(ctx, `SELECT uploader_id, size FROM files WHERE id = $1`, storedFile).Scan(&storedUploader, &storedSize); err != nil {
		t.Fatalf("read the stored file row: %v", err)
	}
	if storedUploader != winner.userID || storedSize != int64(len(winner.body)) {
		t.Fatalf("the post names file %d owned by %d of %d bytes, want the winner's upload of %d bytes",
			storedFile, storedUploader, storedSize, len(winner.body))
	}

	// One post, one event, one notification, and both sends left a file row.
	after := channelWriteStats(t, conn, channel.ID)
	if after.messages != before.messages+1 || after.events != before.events+1 {
		t.Fatalf("channel writes after the cross-author race = %+v, want one post and one event over %+v", after, before)
	}
	if got := countFiles(t, ctx, dsn); got != filesBefore+2 {
		t.Fatalf("file rows after the cross-author race = %d, want %d: the refused send assembled before the post transaction refused it",
			got, filesBefore+2)
	}
	notifyCtx, cancelNotify := context.WithTimeout(ctx, 5*time.Second)
	defer cancelNotify()
	if _, err = listener.WaitForNotification(notifyCtx); err != nil {
		t.Fatalf("waiting for the single channel post notification: %v", err)
	}
	assertNoChannelPostNotification(t, listener)

	// The refused send's upload was consumed by its assembly and its file row is
	// unreferenced: charged to its owner and reclaimable, never published.
	if n, _, _, err := s.UploadPartsSummary(ctx, loser.userID, loser.fileID); err != nil || n != 0 {
		t.Fatalf("refused sender's upload parts = %d, err=%v, want them consumed by the assembly", n, err)
	}
	var loserFileRow int64
	if err = conn.QueryRow(ctx, `SELECT id FROM files WHERE uploader_id = $1 AND size = $2`, loser.userID, len(loser.body)).Scan(&loserFileRow); err != nil {
		t.Fatalf("the refused send left no file row, so it never assembled and the refusal did not come from the post transaction: %v", err)
	}
	var refs int64
	if err = conn.QueryRow(ctx, `SELECT count(*) FROM channel_messages WHERE channel_id = $1 AND file_id = $2`, channel.ID, loserFileRow).Scan(&refs); err != nil {
		t.Fatalf("count references to the refused file: %v", err)
	}
	if refs != 0 {
		t.Fatalf("the refused send's file %d is referenced by %d posts", loserFileRow, refs)
	}

	// The winner's photo is readable by the other member, byte for byte.
	assertChannelPhotoDownload(t, s, blobs, creator.ID, winner.photo, winner.body, "x")
}
