package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

// storedPhotoFile assembles one photo file the way the send path does: the row,
// the bytes and the validated metadata commit together, so the row carries
// media_kind = photo and subtype_rights = {send_photos}. That is the state the
// channel post transaction reads its restrictions from.
func storedPhotoFile(t *testing.T, s *store.Store, uploaderID int64) store.File {
	t.Helper()
	ctx := context.Background()
	blobs := testBlobs(t)
	file, err := s.AllocateAndCompletePhotoFile(ctx, uploaderID, 10, "image/jpeg", "photo.jpg", int64(1)<<30,
		func(file store.File) (store.PhotoDimensions, error) {
			_, err := blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader([]byte("jpeg bytes")))
			return store.PhotoDimensions{Width: 640, Height: 480}, err
		})
	if err != nil {
		t.Fatalf("assemble photo file: %v", err)
	}
	return file
}

func channelJoinInvite(t *testing.T, s *store.Store, channelID, creatorID, userID int64) {
	t.Helper()
	hash, err := s.CreateChannelInvite(context.Background(), channelID, creatorID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(context.Background(), hash, userID); err != nil {
		t.Fatalf("join channel: %v", err)
	}
}

func channelPostCount(t *testing.T, s *store.Store, channelID int64) int {
	t.Helper()
	rows, err := s.ChannelHistory(context.Background(), channelID, 0, 50)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	return len(rows)
}

func channelLocalIDForRandom(t *testing.T, s *store.Store, channelID, randomID int64) int64 {
	t.Helper()
	rows, err := s.ChannelHistory(context.Background(), channelID, 0, 50)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	for _, row := range rows {
		if row.RandomID == randomID {
			return row.LocalID
		}
	}
	t.Fatalf("no channel post carries random_id %d", randomID)
	return 0
}

// A channel post names a file this request assembled, but the eraser can take
// that row between the assembly and the post transaction. The post must fail
// closed rather than write a row pointing at nothing: the RESTRICT foreign
// key would abort the insert with an error no caller can act on.
func TestChannelPhotoPostFailsClosedOnAbsentFileRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559130001")
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo gate", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	before := channelPostCount(t, s, channel.ID)

	var created bool
	_, _, created, err = s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 33001, "photo", 999999999, 0)
	if created {
		t.Fatal("a photo post naming an absent file row reported a new post")
	}
	if !errors.Is(err, store.ErrFileMissing) {
		t.Fatalf("post photo with absent file row: err = %v, want ErrFileMissing", err)
	}
	if got := channelPostCount(t, s, channel.ID); got != before {
		t.Fatalf("channel holds %d posts after a failed photo post, want %d", got, before)
	}
}

// The interlock itself, not just its absence: a reference insert running against
// a transaction that holds the file row exclusively must park behind it and then
// see that the row is gone. Without the share lock the post would commit
// alongside the eraser's delete and leave a dangling reference.
func TestChannelPhotoPostSerializesAgainstFileRowRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559130002")
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo eraser race", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	file := storedPhotoFile(t, s, creator.ID)

	hold, err := store.HoldFileRow(ctx, s, file.ID)
	if err != nil {
		t.Fatalf("hold file row: %v", err)
	}
	done := make(chan struct{})
	var postErr error
	go func() {
		defer close(done)
		_, _, _, postErr = s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 33002, "photo", file.ID, 0)
	}()

	if err = store.WaitForLockWaiters(ctx, s, 1); err != nil {
		hold.Release()
		t.Fatalf("wait for the post to block: %v", err)
	}
	select {
	case <-done:
		t.Fatal("the post committed while the file row was held exclusively — it took no lock on the row")
	default:
	}

	if err = hold.EraseAndCommit(ctx); err != nil {
		t.Fatalf("erase and commit: %v", err)
	}
	<-done

	if !errors.Is(postErr, store.ErrFileMissing) {
		t.Fatalf("post after the row was erased: err = %v, want ErrFileMissing", postErr)
	}
	if got := channelPostCount(t, s, channel.ID); got != 1 {
		t.Fatalf("channel holds %d posts after a failed photo post, want the create service message only", got)
	}
}

// A photo resend must land on the photo post that send created. The
// caller's own live text, poll and document posts under the same random id are
// different sends, and replaying one of them would hand the client a post this
// request never authorized.
func TestChannelPhotoRetryRequiresPersistedPhotoKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559130003")
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo retry kinds", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	other := mustUser(t, s, "+15559130004")
	channelJoinInvite(t, s, channel.ID, creator.ID, other.ID)

	photo := storedPhotoFile(t, s, creator.ID)
	if _, _, _, err = s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 33010, "photo", photo.ID, 0); err != nil {
		t.Fatalf("post photo: %v", err)
	}
	retried, retryPts, isDuplicate, err := s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33010)
	if err != nil || !isDuplicate {
		t.Fatalf("photo retry: duplicate=%v err=%v, want the stored post replayed", isDuplicate, err)
	}
	if retried.FileID == nil || retryPts <= 0 {
		t.Fatalf("retried post = file %v pts %d, want the photo post at its own pts", retried.FileID, retryPts)
	}

	if _, _, _, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "text", 33011, nil, 0); err != nil {
		t.Fatalf("post text: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33011); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("photo retry of a text post: err = %v, want ErrMediaInvalid", err)
	}

	document := storedFile(t, s, creator.ID)
	if _, _, _, err = s.PostChannelMessage(ctx, channel.ID, creator.ID, "document", 33012, &document.ID, 0); err != nil {
		t.Fatalf("post document: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33012); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("photo retry of a document post: err = %v, want ErrMediaInvalid", err)
	}

	draft := store.PollDraft{
		Question: []byte("Which option?"),
		Answers:  []store.PollAnswer{{Option: []byte("a"), Text: []byte("A")}, {Option: []byte("b"), Text: []byte("B")}},
	}
	if _, _, _, _, err = s.PostChannelPollAs(ctx, channel.ID, creator.ID, 33013, "poll", draft); err != nil {
		t.Fatalf("post poll: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33013); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("photo retry of a poll post: err = %v, want ErrMediaInvalid", err)
	}

	if _, _, _, err = s.PostChannelMessageAs(ctx, channel.ID, other.ID, "theirs", 33014, nil, 0); err != nil {
		t.Fatalf("member post: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33014); !errors.Is(err, store.ErrRandomIDDuplicate) {
		t.Fatalf("photo retry of another author's post: err = %v, want ErrRandomIDDuplicate", err)
	}

	localID := channelLocalIDForRandom(t, s, channel.ID, 33010)
	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{localID}); err != nil {
		t.Fatalf("tombstone the photo post: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33010); !errors.Is(err, store.ErrRandomIDDuplicate) {
		t.Fatalf("photo retry of a tombstoned post: err = %v, want ErrRandomIDDuplicate", err)
	}
}

// A committed retry stays a retry after a restriction the original send would
// not have survived, because default rights are judged after the dedup. A ban is
// still a refusal: the retry re-checks current rights, not the ones the original
// send had.
func TestChannelPhotoRetrySurvivesLaterRestrictionButNotABan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559130005")
	channel, err := s.CreateChannel(ctx, creator.ID, "Photo retry rights", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	photo := storedPhotoFile(t, s, creator.ID)
	if _, _, _, err = s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 33020, "photo", photo.ID, 0); err != nil {
		t.Fatalf("post photo: %v", err)
	}
	member := mustUser(t, s, "+15559130006")
	channelJoinInvite(t, s, channel.ID, creator.ID, member.ID)
	memberPhoto := storedPhotoFile(t, s, member.ID)
	if _, _, _, err = s.PostChannelPhotoAs(ctx, channel.ID, member.ID, 33022, "member photo", memberPhoto.ID, 0); err != nil {
		t.Fatalf("member post photo: %v", err)
	}

	if _, _, err = s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{"send_photos"}); err != nil {
		t.Fatalf("restrict send_photos: %v", err)
	}
	message, pts, duplicate, err := s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33020)
	if err != nil || !duplicate {
		t.Fatalf("retry after a send_photos ban: duplicate=%v err=%v, want the stored post", duplicate, err)
	}
	if message.FileID == nil || pts <= 0 {
		t.Fatalf("retried post = file %v pts %d, want the photo post at its own pts", message.FileID, pts)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, creator.ID, 33021); err != nil {
		t.Fatalf("retry with an unknown random id: err = %v, want no post and no error", err)
	}

	until := time.Now().Add(time.Hour)
	if err = s.SetChannelBan(ctx, channel.ID, creator.ID, member.ID, &until, false); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	if _, _, _, err = s.ChannelPhotoRetryAs(ctx, channel.ID, member.ID, 33022); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("retry by a banned member: err = %v, want ErrNotMember", err)
	}
}

// The megagroup restriction is decided on the file's persisted subtype_rights,
// not on what a request claims: a send_videos ban does not reach a photo, and a
// send_photos ban does, while admins stay exempt.
func TestChannelPhotoPostRightsComeFromStoredSubtype(t *testing.T) {
	t.Parallel()
	for i, tc := range []struct {
		right string
		want  error
	}{
		{right: "send_videos", want: nil},
		{right: "send_photos", want: store.ErrChatWriteForbidden},
		{right: "send_media", want: store.ErrChatWriteForbidden},
		{right: "send_messages", want: store.ErrChatWriteForbidden},
	} {
		t.Run(tc.right, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := open(t)
			creator := mustUser(t, s, fmt.Sprintf("+155591301%02d", i*2+1))
			channel, err := s.CreateChannel(ctx, creator.ID, "Photo rights "+tc.right, "", true)
			if err != nil {
				t.Fatalf("create megagroup: %v", err)
			}
			member := mustUser(t, s, fmt.Sprintf("+155591301%02d", i*2+2))
			channelJoinInvite(t, s, channel.ID, creator.ID, member.ID)
			if _, _, err = s.SetChannelDefaultBannedRights(ctx, channel.ID, creator.ID, []string{tc.right}); err != nil {
				t.Fatalf("set default rights: %v", err)
			}

			photo := storedPhotoFile(t, s, member.ID)
			_, _, _, err = s.PostChannelPhotoAs(ctx, channel.ID, member.ID, 33030, "photo", photo.ID, 0)
			if !errors.Is(err, tc.want) {
				t.Fatalf("member photo post under %s: err = %v, want %v", tc.right, err, tc.want)
			}

			creatorPhoto := storedPhotoFile(t, s, creator.ID)
			if _, _, _, err = s.PostChannelPhotoAs(ctx, channel.ID, creator.ID, 33031, "creator photo", creatorPhoto.ID, 0); err != nil {
				t.Fatalf("creator photo post under %s: err = %v, want allowed", tc.right, err)
			}
		})
	}
}
