package api_test

import (
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func getUserPhotosForTest(t *testing.T, s *store.Store, viewerID int64, user tg.InputUserClass, offset int, maxID int64, limit int) tg.PhotosPhotosClass {
	t.Helper()
	got, err := api.GetUserPhotosForTest(s, viewerID, &tg.PhotosGetUserPhotosRequest{
		UserID: user,
		Offset: offset,
		MaxID:  maxID,
		Limit:  limit,
	})
	if err != nil {
		t.Fatalf("photos.getUserPhotos: %v", err)
	}
	photos, ok := got.(tg.PhotosPhotosClass)
	if !ok {
		t.Fatalf("photos.getUserPhotos result = %T, want photos.photos or photos.photosSlice", got)
	}
	assertEncodes(t, got)
	return photos
}

func TestGetUserPhotosReturnsNewestOwnPhotosAndPages(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+15559310101", "+15559310102")
	newest := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, newest)

	first := getUserPhotosForTest(t, f.s, f.viewer, api.InputUser(f.viewer, f.owner), 0, 0, 10)
	page, ok := first.(*tg.PhotosPhotosSlice)
	if !ok {
		t.Fatalf("non-empty photos result = %T, want *tg.PhotosPhotosSlice", first)
	}
	if page.Count != 2 || len(page.Photos) != 2 {
		t.Fatalf("first page count/photos = %d/%d, want 2/2", page.Count, len(page.Photos))
	}
	for i, wantID := range []int64{newest, f.photoID} {
		photo, ok := page.Photos[i].(*tg.Photo)
		if !ok || photo.ID != wantID {
			t.Fatalf("photo %d = %T/%+v, want newest-first id %d", i, page.Photos[i], page.Photos[i], wantID)
		}
		if want := f.credential(f.viewer, f.owner, wantID); photo.AccessHash != want {
			t.Fatalf("photo %d access hash = %d, want viewer-bound profile credential %d", i, photo.AccessHash, want)
		}
	}

	byOffset := getUserPhotosForTest(t, f.s, f.viewer, api.InputUser(f.viewer, f.owner), 1, 0, 1)
	offsetPage, ok := byOffset.(*tg.PhotosPhotosSlice)
	if !ok || offsetPage.Count != 2 || len(offsetPage.Photos) != 1 {
		t.Fatalf("offset page = %#v, want count 2 and one photo", byOffset)
	}
	if photo, ok := offsetPage.Photos[0].(*tg.Photo); !ok || photo.ID != f.photoID {
		t.Fatalf("offset photo = %T/%+v, want older id %d", offsetPage.Photos[0], offsetPage.Photos[0], f.photoID)
	}

	byMaxID := getUserPhotosForTest(t, f.s, f.viewer, api.InputUser(f.viewer, f.owner), 0, newest, 10)
	maxIDPage, ok := byMaxID.(*tg.PhotosPhotosSlice)
	if !ok || maxIDPage.Count != 2 || len(maxIDPage.Photos) != 1 {
		t.Fatalf("max_id page = %#v, want count 2 and one photo", byMaxID)
	}
	if photo, ok := maxIDPage.Photos[0].(*tg.Photo); !ok || photo.ID != f.photoID {
		t.Fatalf("max_id photo = %T/%+v, want older id %d", maxIDPage.Photos[0], maxIDPage.Photos[0], f.photoID)
	}

	byExactID := getUserPhotosForTest(t, f.s, f.viewer, api.InputUser(f.viewer, f.owner), -1, newest, 1)
	exactPage, ok := byExactID.(*tg.PhotosPhotosSlice)
	if !ok || exactPage.Count != 2 || len(exactPage.Photos) != 1 {
		t.Fatalf("offset=-1 page = %#v, want count 2 and one photo", byExactID)
	}
	if photo, ok := exactPage.Photos[0].(*tg.Photo); !ok || photo.ID != newest {
		t.Fatalf("offset=-1 photo = %T/%+v, want exact id %d", exactPage.Photos[0], exactPage.Photos[0], newest)
	}
}

func TestGetUserPhotosReturnsEmptyPhotosConstructorAndHonorsBlock(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+15559310201", "+15559310202")

	empty := getUserPhotosForTest(t, f.s, f.viewer, &tg.InputUserSelf{}, 0, 0, 10)
	if result, ok := empty.(*tg.PhotosPhotos); !ok || len(result.Photos) != 0 {
		t.Fatalf("empty gallery result = %#v, want empty *tg.PhotosPhotos", empty)
	}

	if _, err := f.s.BlockUser(t.Context(), f.owner, f.viewer); err != nil {
		t.Fatalf("block gallery viewer: %v", err)
	}
	blocked := getUserPhotosForTest(t, f.s, f.viewer, api.InputUser(f.viewer, f.owner), 0, 0, 10)
	if result, ok := blocked.(*tg.PhotosPhotos); !ok || len(result.Photos) != 0 {
		t.Fatalf("blocked gallery result = %#v, want empty *tg.PhotosPhotos", blocked)
	}
}

func TestGetUserPhotosRejectsInvalidPagingAndReferences(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+15559310301", "+15559310302")

	for _, tc := range []struct {
		name   string
		user   tg.InputUserClass
		offset int
		maxID  int64
		limit  int
		want   string
	}{
		{name: "negative offset", user: api.InputUser(f.viewer, f.owner), offset: -2, limit: 1, want: "OFFSET_INVALID"},
		{name: "negative max id", user: api.InputUser(f.viewer, f.owner), maxID: -1, limit: 1, want: "OFFSET_INVALID"},
		{name: "zero limit", user: api.InputUser(f.viewer, f.owner), limit: 0, want: "LIMIT_INVALID"},
		{name: "oversized limit", user: api.InputUser(f.viewer, f.owner), limit: 101, want: "LIMIT_INVALID"},
		{name: "forged access hash", user: &tg.InputUser{UserID: f.owner, AccessHash: 1}, limit: 1, want: "PEER_ID_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := api.GetUserPhotosForTest(f.s, f.viewer, &tg.PhotosGetUserPhotosRequest{
				UserID: tc.user,
				Offset: tc.offset,
				MaxID:  tc.maxID,
				Limit:  tc.limit,
			})
			if err == nil || rpcMessage(t, err) != tc.want {
				t.Fatalf("photos.getUserPhotos error = %v, want %s", err, tc.want)
			}
		})
	}
}
