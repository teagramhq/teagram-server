//nolint:testpackage // Handler tests exercise the unexported saved GIF authorization boundary.
package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func savedGIFTestStore(t *testing.T) (*store.Store, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create test blob store: %v", err)
	}
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test store: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test store connection: %v", err)
		}
	})
	return s, conn
}

func savedGIFTestUser(t *testing.T, s *store.Store, phone string) store.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), phone)
	if err != nil {
		t.Fatalf("create user %s: %v", phone, err)
	}
	return u
}

func savedGIFTestFile(t *testing.T, s *store.Store, conn *pgx.Conn, uploader, recipient int64, mimeType string, rights []string, stored bool) store.File {
	t.Helper()
	ctx := context.Background()
	f, err := s.AllocateFile(ctx, uploader, 17, mimeType, "clip.mp4", 1<<31)
	if err != nil {
		t.Fatalf("allocate GIF test file: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE files SET subtype_rights = $2 WHERE id = $1`, f.ID, rights); err != nil {
		t.Fatalf("set GIF test subtype: %v", err)
	}
	if stored {
		if err := s.MarkFileStored(ctx, f.ID); err != nil {
			t.Fatalf("mark GIF test file stored: %v", err)
		}
	}
	if _, _, _, _, err := s.SendMessage(ctx, uploader, recipient, "gif", f.ID+1000, f.ID, 0); err != nil {
		t.Fatalf("send GIF test file: %v", err)
	}
	return f
}

func callSaveGIF(t *testing.T, h *handlers, userID int64, input *tg.InputDocument, unsave bool) (bin.Encoder, error) {
	t.Helper()
	var body bin.Buffer
	if err := (&tg.MessagesSaveGifRequest{ID: input, Unsave: unsave}).Encode(&body); err != nil {
		t.Fatalf("encode messages.saveGif: %v", err)
	}
	return h.handleSaveGif(&mtproto.Request{Ctx: context.Background(), UserID: userID, Buf: &body})
}

func callGetSavedGIFs(t *testing.T, h *handlers, userID, hash int64) (bin.Encoder, error) {
	t.Helper()
	var body bin.Buffer
	if err := (&tg.MessagesGetSavedGifsRequest{Hash: hash}).Encode(&body); err != nil {
		t.Fatalf("encode messages.getSavedGifs: %v", err)
	}
	return h.handleGetSavedGifs(&mtproto.Request{Ctx: context.Background(), UserID: userID, Buf: &body})
}

func TestSavedGIFHandlersSaveListHashAndUnsave(t *testing.T) {
	t.Parallel()
	s, conn := savedGIFTestStore(t)
	uploader := savedGIFTestUser(t, s, "+15559500001")
	owner := savedGIFTestUser(t, s, "+15559500002")
	other := savedGIFTestUser(t, s, "+15559500003")
	file := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "video/mp4", []string{"send_gifs"}, true)
	h := testHandlers(s)

	empty, err := callGetSavedGIFs(t, h, owner.ID, 0)
	if err != nil {
		t.Fatalf("messages.getSavedGifs for empty list: %v", err)
	}
	emptyList, ok := empty.(*tg.MessagesSavedGifs)
	if !ok || emptyList.Hash != 0 || len(emptyList.Gifs) != 0 {
		t.Fatalf("empty saved GIF response = %#v, want full empty list with zero hash", empty)
	}

	result, err := callSaveGIF(t, h, owner.ID, &tg.InputDocument{
		ID: file.ID, AccessHash: file.AccessHash, FileReference: []byte("ignored-reference"),
	}, false)
	if err != nil {
		t.Fatalf("messages.saveGif: %v", err)
	}
	if _, ok := result.(*tg.BoolTrue); !ok {
		t.Fatalf("messages.saveGif result = %T, want *tg.BoolTrue", result)
	}

	result, err = callGetSavedGIFs(t, h, owner.ID, 0)
	if err != nil {
		t.Fatalf("messages.getSavedGifs: %v", err)
	}
	listed, ok := result.(*tg.MessagesSavedGifs)
	if !ok || len(listed.Gifs) != 1 || listed.Hash == 0 {
		t.Fatalf("saved GIF response = %#v, want one GIF and a stable non-zero hash", result)
	}
	repeated, err := callGetSavedGIFs(t, h, owner.ID, 0)
	if err != nil {
		t.Fatalf("repeat messages.getSavedGifs: %v", err)
	}
	repeatedList, ok := repeated.(*tg.MessagesSavedGifs)
	if !ok || repeatedList.Hash != listed.Hash {
		t.Fatalf("repeated saved GIF hash = %#v, want stable hash %d", repeated, listed.Hash)
	}
	document, ok := listed.Gifs[0].(*tg.Document)
	if !ok || document.ID != file.ID || document.AccessHash != file.AccessHash || document.MimeType != "video/mp4" {
		t.Fatalf("saved GIF document = %#v, want file %d metadata", listed.Gifs[0], file.ID)
	}
	animated := false
	for _, attribute := range document.Attributes {
		if _, ok := attribute.(*tg.DocumentAttributeAnimated); ok {
			animated = true
		}
	}
	if !animated {
		t.Fatalf("saved GIF document has no animated attribute: %#v", document.Attributes)
	}
	removed, err := callSaveGIF(t, h, owner.ID, &tg.InputDocument{
		ID: file.ID, AccessHash: file.AccessHash + 1,
	}, true)
	if err != nil {
		t.Fatalf("unsave GIF with stale hash: %v", err)
	}
	if _, ok := removed.(*tg.BoolTrue); !ok {
		t.Fatalf("unsave GIF result = %T, want *tg.BoolTrue", removed)
	}
	removedList, err := callGetSavedGIFs(t, h, owner.ID, 0)
	if err != nil {
		t.Fatalf("list after unsave with stale hash: %v", err)
	}
	if got, ok := removedList.(*tg.MessagesSavedGifs); !ok || len(got.Gifs) != 0 {
		t.Fatalf("saved GIFs after unsave with stale hash = %#v, want empty", removedList)
	}
	if err := s.SaveGIF(context.Background(), owner.ID, file.ID, file.AccessHash); err != nil {
		t.Fatalf("resave entitled GIF: %v", err)
	}

	unchanged, err := callGetSavedGIFs(t, h, owner.ID, listed.Hash)
	if err != nil {
		t.Fatalf("messages.getSavedGifs with current hash: %v", err)
	}
	if _, ok := unchanged.(*tg.MessagesSavedGifsNotModified); !ok {
		t.Fatalf("same-hash response = %T, want *tg.MessagesSavedGifsNotModified", unchanged)
	}

	otherList, err := callGetSavedGIFs(t, h, other.ID, 0)
	if err != nil {
		t.Fatalf("other account messages.getSavedGifs: %v", err)
	}
	if got, ok := otherList.(*tg.MessagesSavedGifs); !ok || len(got.Gifs) != 0 {
		t.Fatalf("other account saved GIFs = %#v, want an empty private list", otherList)
	}
	if _, err := conn.Exec(context.Background(), `UPDATE messages SET deleted = true WHERE owner_id = $1 AND file_id = $2`, owner.ID, file.ID); err != nil {
		t.Fatalf("revoke source message: %v", err)
	}
	changed, err := callGetSavedGIFs(t, h, owner.ID, listed.Hash)
	if err != nil {
		t.Fatalf("messages.getSavedGifs after source revocation: %v", err)
	}
	refreshed, ok := changed.(*tg.MessagesSavedGifs)
	if !ok || len(refreshed.Gifs) != 0 || refreshed.Hash == listed.Hash {
		t.Fatalf("saved GIF response after revocation = %#v, want empty list with changed hash", changed)
	}
	if _, err := s.FileForDownload(context.Background(), file.ID, file.AccessHash, owner.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("download after source revocation = %v, want ErrFileNotFound", err)
	}

	for _, unsave := range []bool{true, true} {
		removed, err := callSaveGIF(t, h, owner.ID, &tg.InputDocument{
			ID: file.ID, AccessHash: file.AccessHash + 1,
		}, unsave)
		if err != nil {
			t.Fatalf("messages.saveGif unsave=%t: %v", unsave, err)
		}
		if _, ok := removed.(*tg.BoolTrue); !ok {
			t.Fatalf("messages.saveGif unsave result = %T, want *tg.BoolTrue", removed)
		}
	}
	removedList, err = callGetSavedGIFs(t, h, owner.ID, 0)
	if err != nil {
		t.Fatalf("messages.getSavedGifs after unsave: %v", err)
	}
	if got, ok := removedList.(*tg.MessagesSavedGifs); !ok || len(got.Gifs) != 0 {
		t.Fatalf("saved GIFs after unsave = %#v, want empty", removedList)
	}
}

func TestSaveGIFRefusalsShareGIFIDInvalidAndUnauthenticatedRequestWins(t *testing.T) {
	t.Parallel()
	s, conn := savedGIFTestStore(t)
	uploader := savedGIFTestUser(t, s, "+15559600001")
	owner := savedGIFTestUser(t, s, "+15559600002")
	stranger := savedGIFTestUser(t, s, "+15559600003")
	valid := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "video/mp4", []string{"send_gifs"}, true)
	unstored := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "video/mp4", []string{"send_gifs"}, false)
	nonAnimated := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "video/mp4", []string{}, true)
	wrongMime := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "text/plain", []string{"send_gifs"}, true)
	unknownRights := savedGIFTestFile(t, s, conn, uploader.ID, owner.ID, "video/mp4", nil, true)
	photo, err := s.AllocateFile(context.Background(), uploader.ID, 17, "image/jpeg", "photo.jpg", 1<<31)
	if err != nil {
		t.Fatalf("allocate photo: %v", err)
	}
	if _, err := conn.Exec(context.Background(), `UPDATE files SET stored = true, media_kind = 'photo', width = 16, height = 16, subtype_rights = ARRAY['send_photos']::text[] WHERE id = $1`, photo.ID); err != nil {
		t.Fatalf("mark photo stored: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(context.Background(), uploader.ID, owner.ID, "photo", photo.ID+1000, photo.ID, 0); err != nil {
		t.Fatalf("send photo: %v", err)
	}
	h := testHandlers(s)

	for _, tc := range []struct {
		name   string
		userID int64
		input  *tg.InputDocument
	}{
		{name: "wrong access hash", userID: owner.ID, input: &tg.InputDocument{ID: valid.ID, AccessHash: valid.AccessHash + 1}},
		{name: "unknown file", userID: owner.ID, input: &tg.InputDocument{ID: valid.ID + 1<<40, AccessHash: valid.AccessHash}},
		{name: "foreign document", userID: stranger.ID, input: &tg.InputDocument{ID: valid.ID, AccessHash: valid.AccessHash}},
		{name: "not stored", userID: owner.ID, input: &tg.InputDocument{ID: unstored.ID, AccessHash: unstored.AccessHash}},
		{name: "photo", userID: owner.ID, input: &tg.InputDocument{ID: photo.ID, AccessHash: photo.AccessHash}},
		{name: "not animated", userID: owner.ID, input: &tg.InputDocument{ID: nonAnimated.ID, AccessHash: nonAnimated.AccessHash}},
		{name: "unsupported MIME type", userID: owner.ID, input: &tg.InputDocument{ID: wrongMime.ID, AccessHash: wrongMime.AccessHash}},
		{name: "unknown subtype rights", userID: owner.ID, input: &tg.InputDocument{ID: unknownRights.ID, AccessHash: unknownRights.AccessHash}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := callSaveGIF(t, h, tc.userID, tc.input, false)
			var rpc *tgerr.Error
			if !errors.As(err, &rpc) || rpc.Message != "GIF_ID_INVALID" || rpc.Code != 400 {
				t.Fatalf("save refusal = %v, want 400 GIF_ID_INVALID", err)
			}
			result, err := callGetSavedGIFs(t, h, tc.userID, 0)
			if err != nil {
				t.Fatalf("list after refusal: %v", err)
			}
			if got, ok := result.(*tg.MessagesSavedGifs); !ok || len(got.Gifs) != 0 {
				t.Fatalf("saved GIFs after refusal = %#v, want unchanged empty list", result)
			}
		})
	}

	var malformed bin.Buffer
	_, err = h.handleSaveGif(&mtproto.Request{Ctx: context.Background(), UserID: 0, Buf: &malformed})
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated malformed saveGif = %v, want AUTH_KEY_UNREGISTERED", err)
	}
	_, err = h.handleGetSavedGifs(&mtproto.Request{Ctx: context.Background(), UserID: 0, Buf: &malformed})
	rpc = nil
	if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated malformed getSavedGifs = %v, want AUTH_KEY_UNREGISTERED", err)
	}
}
