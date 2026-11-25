package api_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestUploadProfilePhotoWithLimaPoolLimit(t *testing.T) {
	t.Parallel()

	const maxConns = 3
	ctx := context.Background()
	blobs := newBlobs(t)
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(),
		store.WithBlobStore(blobs),
		store.WithPoolConfig(store.PoolConfig{MaxConns: maxConns, MaxConnIdleTime: time.Minute}),
	)
	if err != nil {
		t.Fatalf("open store with Lima pool limit: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+1555144800001")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	payload := jpegPhotoPayload(t, 640, 480)
	const fileID = int64(880001)
	if _, err := api.SaveFilePartBlobsForTest(s, blobs, owner.ID, &tg.UploadSaveFilePartRequest{
		FileID: fileID, FilePart: 0, Bytes: payload,
	}); err != nil {
		t.Fatalf("save profile-photo part: %v", err)
	}

	h := api.New(
		s,
		2,
		&tg.Config{MeURLPrefix: testPublicLinkPrefix},
		slog.New(slog.DiscardHandler),
		false,
		api.TestMaxFileBytes,
		blobs,
		api.TestMaxUserStorageBytes,
		pgtest.PeerDeriver(),
		pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)
	request := &tg.PhotosUploadProfilePhotoRequest{}
	request.SetFile(&tg.InputFile{
		ID: fileID, Parts: 1, Name: "avatar.jpg", MD5Checksum: jpegPhotoMD5(payload),
	})
	response := dispatchProfilePhotoMutation(t, h, owner.ID, testKey(), 123, 1<<32, request)
	result := decodePhotosPhoto(t, response)
	photo, ok := result.Photo.(*tg.Photo)
	if !ok || photo.ID == 0 {
		t.Fatalf("uploaded profile photo = %T/%+v, want stored photo", result.Photo, result.Photo)
	}
	if len(result.Users) != 1 {
		t.Fatalf("upload response users = %d, want one owner", len(result.Users))
	}
	user, ok := result.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("upload response user = %T, want *tg.User", result.Users[0])
	}
	profile, hasProfile := user.GetPhoto()
	selected, isSelected := profile.(*tg.UserProfilePhoto)
	if !hasProfile || !isSelected || selected.PhotoID != photo.ID {
		t.Fatalf("owner profile photo = %T/%+v, want uploaded photo %d", profile, profile, photo.ID)
	}
}
