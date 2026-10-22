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
	ownerRequests := &clientRequestRecorder{}
	owner := newSmokeClientWithRequests(t, f, "profile photo owner", phone, ownerRequests)
	viewer := newSmokeClient(t, f, "profile photo viewer", viewerPhone)
	payload := smokeJPEGAtUploadSize(t, smokePhotoPartCount)
	upload := func(fileID int64) (*tg.Photo, error) {
		var uploaded *tg.Photo
		err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
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
			uploaded = photo
			return nil
		})
		return uploaded, err
	}
	first, err := upload(1062001)
	if err != nil {
		t.Fatalf("upload first profile photo: %v", err)
	}
	second, err := upload(1062002)
	if err != nil {
		t.Fatalf("upload second profile photo: %v", err)
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
	if userPhotoID.PhotoID != second.ID {
		t.Fatalf("current profile photo id = %d, want latest upload %d", userPhotoID.PhotoID, second.ID)
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
	if !ok || len(page.Photos) != 2 {
		t.Fatalf("profile gallery = %#v, want both uploaded photos", photos)
	}
	gallery := make([]*tg.Photo, len(page.Photos))
	for i, item := range page.Photos {
		photo, ok := item.(*tg.Photo)
		if !ok {
			t.Fatalf("profile gallery photo %d = %T, want *tg.Photo", i, item)
		}
		gallery[i] = photo
	}
	if gallery[0].ID != second.ID || gallery[1].ID != first.ID {
		t.Fatalf("profile gallery order = [%d %d], want [%d %d]", gallery[0].ID, gallery[1].ID, second.ID, first.ID)
	}

	selectionStart := ownerRequests.count()
	var selected *tg.PhotosPhoto
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		selected, err = client.PhotosUpdateProfilePhoto(ctx, &tg.PhotosUpdateProfilePhotoRequest{
			ID: &tg.InputPhoto{
				ID: gallery[1].ID, AccessHash: gallery[1].AccessHash,
				FileReference: gallery[1].FileReference,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("reselect earlier profile photo: %v", err)
	}
	if err := ownerRequests.matchFixtureSince(selectionStart, "photos_updateProfilePhoto.1"); err != nil {
		t.Fatalf("match profile-photo reselection request fixture: %v", err)
	}
	selectedPhoto, ok := selected.Photo.(*tg.Photo)
	if !ok || selectedPhoto.ID != first.ID {
		t.Fatalf("reselection response photo = %T/%+v, want earlier photo %d", selected.Photo, selected.Photo, first.ID)
	}
	if len(selected.Users) != 1 {
		t.Fatalf("reselection returned %d users, want the owner", len(selected.Users))
	}
	selectedUser, ok := selected.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("reselection user = %T, want *tg.User", selected.Users[0])
	}
	selectedUserPhoto, hasPhoto := selectedUser.GetPhoto()
	selectedUserProfile, ok := selectedUserPhoto.(*tg.UserProfilePhoto)
	if !hasPhoto || !ok || selectedUserProfile.PhotoID != first.ID {
		t.Fatalf("reselection user photo = %T/%+v, want earlier photo %d", selectedUserPhoto, selectedUserPhoto, first.ID)
	}

	var users []tg.UserClass
	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		users, err = client.UsersGetUsers(ctx, []tg.InputUserClass{inputUser(viewer.id, owner.id)})
		return err
	}); err != nil {
		t.Fatalf("read current profile photo through users.getUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users.getUsers returned %d users, want the owner", len(users))
	}
	updatedUser, ok := users[0].(*tg.User)
	if !ok {
		t.Fatalf("users.getUsers user = %T, want *tg.User", users[0])
	}
	updatedUserPhoto, hasPhoto := updatedUser.GetPhoto()
	updatedProfilePhoto, ok := updatedUserPhoto.(*tg.UserProfilePhoto)
	if !hasPhoto || !ok || updatedProfilePhoto.PhotoID != first.ID {
		t.Fatalf("users.getUsers photo = %T/%+v, want selected photo %d", updatedUserPhoto, updatedUserPhoto, first.ID)
	}

	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPeerPhotoFileLocation{Peer: peerUser(viewer.id, owner.id), PhotoID: second.ID, Big: true},
			Offset:   0, Limit: len(payload),
		})
		if err == nil || !tgErrorIs(err, "LOCATION_INVALID") {
			return fmt.Errorf("download old current photo: %w, want LOCATION_INVALID", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("old current peer-photo location: %v", err)
	}

	var viewerPhotos tg.PhotosPhotosClass
	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		viewerPhotos, err = client.PhotosGetUserPhotos(ctx, &tg.PhotosGetUserPhotosRequest{
			UserID: inputUser(viewer.id, owner.id), Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatalf("read retained profile photo gallery: %v", err)
	}
	viewerPage, ok := viewerPhotos.(*tg.PhotosPhotosSlice)
	if !ok || len(viewerPage.Photos) != 2 {
		t.Fatalf("viewer profile gallery = %#v, want both retained photos", viewerPhotos)
	}
	var retained *tg.Photo
	for _, item := range viewerPage.Photos {
		photo, ok := item.(*tg.Photo)
		if ok && photo.ID == second.ID {
			retained = photo
			break
		}
	}
	if retained == nil {
		t.Fatalf("old current photo %d is missing from the gallery", second.ID)
	}
	var retainedSize *tg.PhotoSize
	for _, class := range retained.Sizes {
		if size, ok := class.(*tg.PhotoSize); ok && size.Type != "m" {
			retainedSize = size
			break
		}
	}
	if retainedSize == nil {
		t.Fatalf("old current photo %d has no original size", second.ID)
	}
	var downloaded tg.UploadFileClass
	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		downloaded, err = client.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputPhotoFileLocation{
				ID: retained.ID, AccessHash: retained.AccessHash,
				FileReference: retained.FileReference, ThumbSize: retainedSize.Type,
			},
			Offset: 0, Limit: retainedSize.Size,
		})
		return err
	}); err != nil {
		t.Fatalf("download retained photo through gallery lane: %v", err)
	}
	file, ok := downloaded.(*tg.UploadFile)
	if !ok || !bytes.Equal(file.Bytes, payload) {
		t.Fatalf("gallery download = %T, want original JPEG bytes", downloaded)
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
