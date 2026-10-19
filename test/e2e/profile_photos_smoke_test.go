package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store"
)

func seedSmokeProfilePhoto(t *testing.T, f *smokeFixture, ownerID int64, payload, preview []byte) int64 {
	t.Helper()
	ctx := context.Background()
	file, derivativesStored, err := f.store.AllocateAndCompletePhotoFileWithDerivatives(ctx, ownerID, int64(len(payload)), "image/jpeg", "profile.jpg", 1<<30,
		func(file store.File) (store.PhotoAssembly, error) {
			if _, err := f.blobs.Put(ctx, blob.Key(file.ID), bytes.NewReader(payload)); err != nil {
				return store.PhotoAssembly{}, err
			}
			return store.PhotoAssembly{
				Dimensions: store.PhotoDimensions{Width: 640, Height: 480},
				Derivatives: &store.PhotoDerivativeInput{
					MWidth: 320, MHeight: 240, MBytes: preview, Stripped: []byte{1, 1, 1},
				},
			}, nil
		})
	if err != nil {
		t.Fatalf("allocate smoke profile photo: %v", err)
	}
	if !derivativesStored {
		t.Fatal("smoke profile photo derivative was not stored")
	}
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed smoke profile photo: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close smoke profile photo connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx,
		`INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $2)`,
		ownerID, file.ID); err != nil {
		t.Fatalf("insert smoke profile photo: %v", err)
	}
	return file.ID
}

func testSmokeProfilePhotoUpload(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone, viewerPhone = "+15551062001", "+15551062003"
	seedSmokeUsers(t, f, phone, viewerPhone)
	owner := newSmokeClient(t, f, "profile photo owner", phone)
	viewer := newSmokeClient(t, f, "profile photo viewer", viewerPhone)
	payload := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	const fileID = int64(1062001)
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for part := range smokePhotoPartCount {
			start := part * smokePhotoPartSize
			ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
				FileID: fileID, FilePart: part, Bytes: payload[start : start+smokePhotoPartSize],
			})
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("profile photo part %d returned false", part)
			}
		}
		request := &tg.PhotosUploadProfilePhotoRequest{}
		request.SetFile(&tg.InputFile{
			ID: fileID, Parts: smokePhotoPartCount, Name: "fictional-avatar.jpg", MD5Checksum: smokePhotoMD5(payload),
		})
		result, err := client.PhotosUploadProfilePhoto(ctx, request)
		if err != nil {
			return err
		}
		photo, ok := result.Photo.(*tg.Photo)
		if !ok {
			return fmt.Errorf("profile photo = %T, want *tg.Photo", result.Photo)
		}
		if photo.ID <= 0 || photo.DCID <= 0 {
			return fmt.Errorf("profile photo identity = id %d, dc %d", photo.ID, photo.DCID)
		}
		return nil
	}); err != nil {
		t.Fatalf("upload profile photo: %v", err)
	}
	var profile *tg.UsersUserFull
	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		profile, err = client.UsersGetFullUser(ctx, inputUser(viewer.id, owner.id))
		return err
	}); err != nil {
		t.Fatalf("read current profile photo: %v", err)
	}
	if len(profile.Users) != 1 {
		t.Fatalf("full profile returned %d users, want one", len(profile.Users))
	}
	profileUser, ok := profile.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("full profile user = %T, want *tg.User", profile.Users[0])
	}
	userPhoto, ok := profileUser.GetPhoto()
	if !ok {
		t.Fatal("full profile omitted the uploaded avatar")
	}
	userPhotoID, ok := userPhoto.(*tg.UserProfilePhoto)
	if !ok || userPhotoID.PhotoID <= 0 {
		t.Fatalf("full profile photo = %T/%+v, want a selected profile photo", userPhoto, userPhoto)
	}

	var photos tg.PhotosPhotosClass
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		photos, err = client.PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
			UserID: &tg.InputUserSelf{}, Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatalf("list uploaded profile photo: %v", err)
	}
	page, ok := photos.(*tg.PhotosPhotosSlice)
	if !ok || len(page.Photos) != 1 {
		t.Fatalf("profile gallery = %#v, want one uploaded photo", photos)
	}
	photo, ok := page.Photos[0].(*tg.Photo)
	if !ok {
		t.Fatalf("profile gallery photo = %T, want *tg.Photo", page.Photos[0])
	}
	if photo.ID != userPhotoID.PhotoID {
		t.Fatalf("gallery photo id = %d, user profile photo id = %d", photo.ID, userPhotoID.PhotoID)
	}
	var downloaded tg.UploadFileClass
	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		downloaded, err = client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPeerPhotoFileLocation{Peer: peerUser(viewer.id, owner.id), PhotoID: userPhotoID.PhotoID, Big: true},
			Offset:   0, Limit: len(payload),
		})
		return err
	}); err != nil {
		t.Fatalf("download current profile photo: %v", err)
	}
	file, ok := downloaded.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, payload) {
		t.Fatalf("downloaded profile photo = %T, want original JPEG bytes", downloaded)
	}
}

func testSmokeProfileBio(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551062002"
	seedSmokeUsers(t, f, phone)
	owner := newSmokeClient(t, f, "profile bio owner", phone)
	const want = "A fictional naturalist sketching cloud maps and quiet rivers."
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		request := &tg.AccountUpdateProfileRequest{}
		request.SetAbout(want)
		if _, err := client.AccountUpdateProfile(ctx, request); err != nil {
			return err
		}
		profile, err := client.UsersGetFullUser(ctx, &tg.InputUserSelf{})
		if err != nil {
			return err
		}
		about, ok := profile.FullUser.GetAbout()
		if !ok || about != want {
			return fmt.Errorf("profile bio = %q/%t, want %q", about, ok, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("update and read profile bio: %v", err)
	}
}

func assertSmokeProfilePhotoSizes(t *testing.T, ctx context.Context, client *smokeClient, photo *tg.Photo, original, preview []byte, label string) {
	t.Helper()
	downloaded := make(map[string][]byte, 2)
	var originalType string
	for _, class := range photo.Sizes {
		size, ok := class.(*tg.PhotoSize)
		if !ok {
			continue
		}
		want := preview
		if size.Type == "m" {
			// The m response must be the derivative returned by the gallery gate.
		} else {
			originalType = size.Type
			want = original
		}
		var result tg.UploadFileClass
		if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputPhotoFileLocation{
					ID: photo.ID, AccessHash: photo.AccessHash, FileReference: photo.FileReference, ThumbSize: size.Type,
				},
				Offset: 0, Limit: size.Size,
			})
			return err
		}); err != nil {
			t.Fatalf("%s size %q download: %v", label, size.Type, err)
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			t.Fatalf("%s size %q result = %T, want *tg.UploadFile", label, size.Type, result)
		}
		if !bytes.Equal(file.Bytes, want) {
			t.Errorf("%s size %q returned %q, want %q", label, size.Type, file.Bytes, want)
		}
		if len(file.Bytes) != size.Size {
			t.Errorf("%s size %q returned %d bytes, advertised %d", label, size.Type, len(file.Bytes), size.Size)
		}
		downloaded[size.Type] = bytes.Clone(file.Bytes)
	}
	if len(downloaded) != 2 || downloaded["m"] == nil || originalType == "" || downloaded[originalType] == nil {
		t.Fatalf("%s advertised downloadable sizes = %v, want m and the original", label, downloaded)
	}
	if bytes.Equal(downloaded["m"], downloaded[originalType]) {
		t.Fatalf("%s m and %q sizes returned identical bytes", label, originalType)
	}
}
