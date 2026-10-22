package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type profileSelectionSnapshot struct {
	galleryRows int64
	fileRows    int64
	charged     int64
	current     int64
	revision    int64
}

const profilePhotoLockDomainForTest = int32(0x50463031) // PF01, the store's profile-domain key.

func (f *galleryFixture) selectionSnapshot(userID int64) profileSelectionSnapshot {
	f.t.Helper()
	var snapshot profileSelectionSnapshot
	err := f.writer().QueryRow(context.Background(), `
		SELECT
		  (SELECT count(*) FROM user_photos WHERE user_id = $1),
		  (SELECT count(*) FROM files WHERE uploader_id = $1),
		  (SELECT coalesce(sum(size), 0) FROM files WHERE uploader_id = $1),
		  coalesce(s.current_file_id, 0), coalesce(s.mutation_revision, 0)
		FROM users u
		LEFT JOIN profile_photo_state s ON s.user_id = u.id
		WHERE u.id = $1`, userID).Scan(
		&snapshot.galleryRows, &snapshot.fileRows, &snapshot.charged, &snapshot.current, &snapshot.revision,
	)
	if err != nil {
		f.t.Fatalf("read profile-selection snapshot for %d: %v", userID, err)
	}
	return snapshot
}

func TestGetUsersHydratesCurrentProfilePhotoForVisibleViewer(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144600001", "+1555144600002")
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	request := &tg.UsersGetUsersRequest{ID: []tg.InputUserClass{api.InputUser(f.viewer, f.owner)}}

	result, err := api.GetUsersForTestWithRequest(f.s, f.viewer, request)
	if err != nil {
		t.Fatalf("users.getUsers: %v", err)
	}
	vector, ok := result.(*tg.UserClassVector)
	if !ok || len(vector.Elems) != 1 {
		t.Fatalf("users.getUsers result = %#v, want one user", result)
	}
	user, ok := vector.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("users.getUsers item = %T, want *tg.User", vector.Elems[0])
	}
	profile, ok := user.GetPhoto()
	selected, okProfile := profile.(*tg.UserProfilePhoto)
	if !ok || !okProfile || selected.PhotoID != f.photoID {
		t.Fatalf("users.getUsers profile photo = %T/%+v, want %d", profile, profile, f.photoID)
	}

	f.block(t, f.owner, f.viewer)
	result, err = api.GetUsersForTestWithRequest(f.s, f.viewer, request)
	if err != nil {
		t.Fatalf("users.getUsers after owner blocks viewer: %v", err)
	}
	vector, ok = result.(*tg.UserClassVector)
	if !ok || len(vector.Elems) != 1 {
		t.Fatalf("users.getUsers after owner blocks viewer = %#v, want one user", result)
	}
	user, ok = vector.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("users.getUsers item after owner blocks viewer = %T, want *tg.User", vector.Elems[0])
	}
	profile, _ = user.GetPhoto()
	if _, ok := profile.(*tg.UserProfilePhoto); ok {
		t.Fatalf("blocked viewer received current photo: %T/%+v", profile, profile)
	}
}

func TestUpdateProfilePhotoRejectsEveryNonOwnedCapabilityUniformly(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144600101", "+1555144600102")
	f.selectCurrentPhoto(t, f.owner, f.photoID)

	older := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, older)
	deleted := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, deleted)
	f.deleteEntry(t, f.owner, deleted)
	foreign := f.addStoredPhoto(t, f.viewer)
	f.addEntry(t, f.viewer, foreign)
	rawFile := f.addStoredPhoto(t, f.owner)
	var rawHash int64
	if err := f.writer().QueryRow(context.Background(), `SELECT access_hash FROM files WHERE id = $1`, rawFile).Scan(&rawHash); err != nil {
		t.Fatalf("read raw file access hash: %v", err)
	}

	selfHash := f.credential(f.owner, f.owner, older)
	requests := []struct {
		name string
		id   tg.InputPhotoClass
	}{
		{name: "another owner's gallery photo", id: &tg.InputPhoto{ID: foreign, AccessHash: f.credential(f.owner, f.viewer, foreign), FileReference: []byte("old-ref")}},
		{name: "message file raw hash", id: &tg.InputPhoto{ID: rawFile, AccessHash: rawHash}},
		{name: "guessed id", id: &tg.InputPhoto{ID: 999999999, AccessHash: 11}},
		{name: "own id with zero hash", id: &tg.InputPhoto{ID: older}},
		{name: "own id with wrong hash", id: &tg.InputPhoto{ID: older, AccessHash: selfHash ^ 1}},
		{name: "own id with another viewer's hash", id: &tg.InputPhoto{ID: older, AccessHash: f.credential(f.viewer, f.owner, older)}},
		{name: "own deleted id", id: &tg.InputPhoto{ID: deleted, AccessHash: f.credential(f.owner, f.owner, deleted)}},
		{name: "inputPhotoEmpty clear", id: &tg.InputPhotoEmpty{}},
	}
	ownerBefore := f.selectionSnapshot(f.owner)
	viewerBefore := f.selectionSnapshot(f.viewer)
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			_, err := api.UpdateProfilePhotoForTest(f.s, f.owner, &tg.PhotosUpdateProfilePhotoRequest{ID: test.id})
			if got := rpcMessage(t, err); got != "PHOTO_ID_INVALID" {
				t.Fatalf("updateProfilePhoto error = %s, want PHOTO_ID_INVALID", got)
			}
		})
	}
	if after := f.selectionSnapshot(f.owner); after != ownerBefore {
		t.Errorf("rejected profile-photo inputs changed caller state: before=%+v after=%+v", ownerBefore, after)
	}
	if after := f.selectionSnapshot(f.viewer); after != viewerBefore {
		t.Errorf("rejected profile-photo inputs changed the other account: before=%+v after=%+v", viewerBefore, after)
	}
}

func TestUpdateProfilePhotoRejectsFallbackAndBotBeforeRateAdmission(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144600201", "+1555144600202")
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	selected := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, selected)
	rate := store.RateLimitConfig{Limit: 1, Window: time.Hour}
	input := &tg.InputPhoto{ID: selected, AccessHash: f.credential(f.owner, f.owner, selected), FileReference: []byte("stale-reference")}

	fallback := &tg.PhotosUpdateProfilePhotoRequest{ID: input}
	fallback.SetFallback(true)
	if _, err := api.UpdateProfilePhotoForTestWithRateLimit(f.s, f.owner, fallback, rate); rpcMessage(t, err) != "INPUT_REQUEST_INVALID" {
		t.Fatalf("fallback request error = %s, want INPUT_REQUEST_INVALID", rpcMessage(t, err))
	}
	bot := &tg.PhotosUpdateProfilePhotoRequest{ID: input}
	bot.SetBot(&tg.InputUserSelf{})
	if _, err := api.UpdateProfilePhotoForTestWithRateLimit(f.s, f.owner, bot, rate); rpcMessage(t, err) != "INPUT_REQUEST_INVALID" {
		t.Fatalf("bot request error = %s, want INPUT_REQUEST_INVALID", rpcMessage(t, err))
	}

	result, err := api.UpdateProfilePhotoForTestWithRateLimit(f.s, f.owner, &tg.PhotosUpdateProfilePhotoRequest{ID: input}, rate)
	if err != nil {
		t.Fatalf("valid selection after two early refusals: %v", err)
	}
	photo, ok := result.(*tg.PhotosPhoto)
	if !ok {
		t.Fatalf("valid selection result = %T, want *tg.PhotosPhoto", result)
	}
	selectedPhoto, ok := photo.Photo.(*tg.Photo)
	if !ok || selectedPhoto.ID != selected {
		t.Fatalf("valid selection photo = %T/%+v, want %d", photo.Photo, photo.Photo, selected)
	}
}

func TestUpdateProfilePhotoLockContentionReturnsFloodWaitWithoutChanges(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144600301", "+1555144600302")
	f.selectCurrentPhoto(t, f.owner, f.photoID)
	selected := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, selected)
	before := f.selectionSnapshot(f.owner)

	if _, err := f.writer().Exec(context.Background(),
		`SELECT pg_advisory_lock($1::int, $2::int)`, profilePhotoLockDomainForTest, f.owner); err != nil {
		t.Fatalf("hold profile-domain lock: %v", err)
	}
	started := time.Now()
	_, err := api.UpdateProfilePhotoForTest(f.s, f.owner, &tg.PhotosUpdateProfilePhotoRequest{
		ID: &tg.InputPhoto{ID: selected, AccessHash: f.credential(f.owner, f.owner, selected)},
	})
	if got := rpcMessage(t, err); got != "FLOOD_WAIT_1" {
		t.Fatalf("selection while profile lock is held = %s, want FLOOD_WAIT_1", got)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("profile-lock refusal took %s, want a bounded wait under two seconds", elapsed)
	}
	if after := f.selectionSnapshot(f.owner); after != before {
		t.Fatalf("profile-lock refusal changed state: before=%+v after=%+v", before, after)
	}
	var unlocked bool
	if err := f.writer().QueryRow(context.Background(), `SELECT pg_advisory_unlock($1::int, $2::int)`,
		profilePhotoLockDomainForTest, f.owner).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release profile-domain lock: unlocked=%t err=%v", unlocked, err)
	}
}
