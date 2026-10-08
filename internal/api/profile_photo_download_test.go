package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/photohash"
	"github.com/teagramhq/teagram-server/internal/store"
)

// The gallery download lane's tests. The lane is unregistered, so every call
// goes in through api.ProfilePhotoGet* — the synthetic entry a future RPC will
// call. What these tests pin is the shape of the authorization, not its
// arithmetic: who the credential admits, what every rejection answers,
// when a revocation takes effect, and what the read path must never hold.
//
// user_photos has no writer in the server yet (the upload slice is separate), so
// the gallery rows these tests read are written here, in the shape a writer will
// write them.

// galleryPayload is one avatar's bytes.
var galleryPayload = []byte("profile-photo-bytes-0123456789abcdef")

// galleryBlobs counts the reads that reach the object store, can pause a read on
// command, and can be told to remove an object mid-read. The pause is what
// makes "the read is in progress" an observed event on the blob boundary rather
// than a sleep: the test waits for the read to arrive, acts while it is sitting
// there, and then releases it.
type galleryBlobs struct {
	blob.Store

	reads atomic.Int64

	mu      sync.Mutex
	started chan struct{}
	release chan struct{}
}

func (b *galleryBlobs) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	b.reads.Add(1)
	b.mu.Lock()
	started, release := b.started, b.release
	b.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
	}
	return b.Store.ReadAt(ctx, key, offset, limit)
}

// pauseReads makes every later read wait, and returns a channel that fires the
// first time a read arrives in the pause.
func (b *galleryBlobs) pauseReads() (entered <-chan struct{}) {
	in := make(chan struct{}, 1)
	out := make(chan struct{})
	b.mu.Lock()
	b.started, b.release = in, out
	b.mu.Unlock()
	return in
}

// releaseReads lets every paused read continue.
func (b *galleryBlobs) releaseReads() {
	b.mu.Lock()
	release := b.release
	b.started, b.release = nil, nil
	b.mu.Unlock()
	if release != nil {
		close(release)
	}
}

// galleryFixture is one owner with a live stored gallery photo, one viewer
// entitled to it, and the raw connections the tests need to write rows, hold a
// transaction open, and read what Postgres reports about the running sessions.
type galleryFixture struct {
	t       *testing.T
	s       *store.Store
	dsn     string
	blobs   *galleryBlobs
	deriver *photohash.Deriver
	owner   int64
	viewer  int64
	photoID int64

	writerConn   *pgx.Conn
	observerConn *pgx.Conn
	chatRow      store.Chat
	chatReady    bool
	rand         atomic.Int64
}

func newGalleryFixture(t *testing.T, ownerPhone, viewerPhone string) *galleryFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	f := &galleryFixture{t: t, s: s, dsn: dsn, blobs: &galleryBlobs{Store: newBlobs(t)}, deriver: pgtest.PhotoDeriver()}
	owner, err := s.CreateUser(ctx, ownerPhone)
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateUser(ctx, viewerPhone)
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	f.owner, f.viewer = owner.ID, viewer.ID
	f.photoID = f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, f.photoID)
	return f
}

// writer returns a cached raw connection these tests write gallery rows on, and
// that can hold a transaction open across a lane call.
func (f *galleryFixture) writer() *pgx.Conn {
	f.t.Helper()
	if f.writerConn == nil {
		f.writerConn = f.rawConn("writer")
	}
	return f.writerConn
}

// observer returns a cached raw connection on a separate session, for reading
// pg_locks and pg_stat_activity while another session is mid-request.
func (f *galleryFixture) observer() *pgx.Conn {
	f.t.Helper()
	if f.observerConn == nil {
		f.observerConn = f.rawConn("observer")
	}
	return f.observerConn
}

func (f *galleryFixture) rawConn(kind string) *pgx.Conn {
	f.t.Helper()
	conn, err := pgx.Connect(context.Background(), f.dsn)
	if err != nil {
		f.t.Fatalf("connect (%s): %v", kind, err)
	}
	f.t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			f.t.Errorf("close %s connection: %v", kind, err)
		}
	})
	return conn
}

// addStoredPhoto assembles a validated stored JPEG owned by ownerID, with
// galleryPayload as its bytes, and returns the file id.
func (f *galleryFixture) addStoredPhoto(t *testing.T, ownerID int64) int64 {
	t.Helper()
	file, err := f.s.AllocateAndCompletePhotoFile(context.Background(), ownerID, int64(len(galleryPayload)),
		"image/jpeg", "avatar.jpg", api.TestMaxUserStorageBytes,
		func(file store.File) (store.PhotoDimensions, error) {
			_, err := f.blobs.Put(context.Background(), blob.Key(file.ID), bytes.NewReader(galleryPayload))
			return store.PhotoDimensions{Width: 64, Height: 48}, err
		})
	if err != nil {
		t.Fatalf("assemble photo: %v", err)
	}
	return file.ID
}

// addEntry writes the (owner, file) gallery row: the live fact the gate reads.
func (f *galleryFixture) addEntry(t *testing.T, ownerID, fileID int64) {
	t.Helper()
	if _, err := f.writer().Exec(context.Background(),
		`INSERT INTO user_photos (user_id, file_id, client_file_id) VALUES ($1, $2, $2)`,
		ownerID, fileID); err != nil {
		t.Fatalf("add gallery entry (owner=%d file=%d): %v", ownerID, fileID, err)
	}
}

func (f *galleryFixture) deleteEntry(t *testing.T, ownerID, fileID int64) {
	t.Helper()
	tag, err := f.writer().Exec(context.Background(),
		`DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, ownerID, fileID)
	if err != nil {
		t.Fatalf("delete gallery entry (owner=%d file=%d): %v", ownerID, fileID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete gallery entry affected %d rows, want 1", tag.RowsAffected())
	}
}

func (f *galleryFixture) block(t *testing.T, blockerID, blockedID int64) {
	t.Helper()
	if _, err := f.s.BlockUser(context.Background(), blockerID, blockedID); err != nil {
		t.Fatalf("block %d -> %d: %v", blockerID, blockedID, err)
	}
}

func (f *galleryFixture) unblock(t *testing.T, blockerID, blockedID int64) {
	t.Helper()
	if _, err := f.s.UnblockUser(context.Background(), blockerID, blockedID); err != nil {
		t.Fatalf("unblock %d -> %d: %v", blockerID, blockedID, err)
	}
}

// credential is the capability the server issues to viewerID for (ownerID,
// fileID): the same derivation a gallery list would have handed that viewer.
func (f *galleryFixture) credential(viewerID, ownerID, fileID int64) int64 {
	return f.deriver.Derive(viewerID, ownerID, fileID)
}

// get runs one gallery download and returns the bytes it served, failing the
// test on any error.
func (f *galleryFixture) get(t *testing.T, viewerID int64, ownerID, fileID int64) []byte {
	t.Helper()
	enc, err := f.call(t, viewerID, ownerID, fileID, 0, len(galleryPayload))
	if err != nil {
		t.Fatalf("gallery download (viewer=%d owner=%d file=%d): %v", viewerID, ownerID, fileID, err)
	}
	file, ok := enc.(*tg.UploadFile)
	if !ok {
		t.Fatalf("result type = %T, want *tg.UploadFile", enc)
	}
	assertEncodes(t, enc)
	return file.Bytes
}

// call is get's error-returning form: the raw lane answer, for the rejection
// cases.
func (f *galleryFixture) call(t *testing.T, viewerID int64, ownerID, fileID int64, offset int64, limit int) (bin.Encoder, error) {
	t.Helper()
	return f.seq(t)(context.Background(), viewerID, f.request(ownerID, fileID, offset, limit))
}

// seq returns one gallery lane bound to a single handlers value, so successive
// calls share the in-flight download slot the lane shares with upload.getFile.
func (f *galleryFixture) seq(t *testing.T) api.ProfilePhotoSeq {
	t.Helper()
	return api.ProfilePhotoGetSeqForTest(f.s, f.blobs)
}

// request is the lane's input for (owner, file) as that owner's peer, with the
// capability a viewer would have been shown.
func (f *galleryFixture) request(ownerID, fileID int64, offset int64, limit int) api.ProfilePhotoGet {
	return api.ProfilePhotoGet{
		Peer:       api.InputPeerUser(f.viewer, ownerID),
		PhotoID:    fileID,
		Credential: f.credential(f.viewer, ownerID, fileID),
		Offset:     offset,
		Limit:      limit,
	}
}

// TestPhotoGalleryDownloadServesBoundCapability is the one case that must
// answer with bytes: the capability issued to this viewer for this owner's live
// stored photo. The window behavior is asserted here too, and it must be
// upload.getFile's exactly — the lane shares that plumbing, and a short tail is
// how a client walks a file.
func TestPhotoGalleryDownloadServesBoundCapability(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500001", "+1555144500002")

	if got := f.get(t, f.viewer, f.owner, f.photoID); !bytes.Equal(got, galleryPayload) {
		t.Fatalf("served %q, want the photo bytes", got)
	}

	getFile := f.seq(t)
	cases := []struct {
		name           string
		offset         int64
		limit          int
		want           []byte
		wantEmptyType  bool
		wantJPEGType   bool
		wantZeroLength bool
	}{
		{
			name:         "middle window",
			offset:       5,
			limit:        4,
			want:         galleryPayload[5:9],
			wantJPEGType: true,
		},
		{
			name:         "tail window runs past the end",
			offset:       int64(len(galleryPayload)) - 3,
			limit:        1000,
			want:         galleryPayload[len(galleryPayload)-3:],
			wantJPEGType: true,
		},
		{
			name:           "offset at the end is legal and empty",
			offset:         int64(len(galleryPayload)),
			limit:          100,
			want:           nil,
			wantEmptyType:  true,
			wantZeroLength: true,
		},
	}
	for _, c := range cases {
		req := f.request(f.owner, f.photoID, c.offset, c.limit)
		enc, err := getFile(context.Background(), f.viewer, req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		file, ok := enc.(*tg.UploadFile)
		if !ok {
			t.Fatalf("%s: result type = %T, want *tg.UploadFile", c.name, enc)
		}
		assertEncodes(t, enc)
		if !bytes.Equal(file.Bytes, c.want) {
			t.Errorf("%s: served %q, want %q", c.name, file.Bytes, c.want)
		}
		if c.wantJPEGType {
			if _, ok := file.Type.(*tg.StorageFileJpeg); !ok {
				t.Errorf("%s: Type = %T, want storage.fileJpeg for a validated photo", c.name, file.Type)
			}
		}
		if c.wantEmptyType {
			if file.Type == nil {
				t.Errorf("%s: Type is nil, want a file type on the empty reply", c.name)
			}
			if len(file.Bytes) != 0 {
				t.Errorf("%s: served %d bytes at the end of the file, want 0", c.name, len(file.Bytes))
			}
		}
	}

	// The owner is a viewer of their own gallery, through the same lane.
	self, err := getFile(context.Background(), f.owner, api.ProfilePhotoGet{
		Peer:       &tg.InputPeerSelf{},
		PhotoID:    f.photoID,
		Credential: f.credential(f.owner, f.owner, f.photoID),
		Limit:      len(galleryPayload),
	})
	if err != nil {
		t.Fatalf("the owner reading their own photo: %v", err)
	}
	if file, ok := self.(*tg.UploadFile); !ok || !bytes.Equal(file.Bytes, galleryPayload) {
		t.Fatalf("the owner's read served %v, want the photo bytes", self)
	}

	if got := f.blobs.reads.Load(); got < 5 {
		t.Fatalf("blob reads = %d, want one per served request", got)
	}
}

// TestPhotoGalleryDownloadRejectionsAreIndistinguishable is the acceptance
// matrix. Every way a gallery request is not entitled answers the identical
// LOCATION_INVALID, with no server log record: file ids are dense, so a lane
// that answers "no such file" separately from "not this owner's gallery",
// "deleted", "blocked" or "bytes gone" is an enumeration oracle over every
// account's gallery.
func TestPhotoGalleryDownloadRejectionsAreIndistinguishable(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500011", "+1555144500012")
	logs := &captureHandler{}
	getFile := api.ProfilePhotoGetSeqForTestWithLogger(f.s, f.blobs, slog.New(logs),
		store.RateLimitConfig{}, store.RateLimitConfig{})

	stranger, err := f.s.CreateUser(context.Background(), "+1555144500013")
	if err != nil {
		t.Fatalf("create stranger: %v", err)
	}

	// A photo in the viewer's own gallery: the cross-owner and received-file
	// cases need a file the viewer is entitled to somewhere else.
	ownPhoto := f.addStoredPhoto(t, f.viewer)
	f.addEntry(t, f.viewer, ownPhoto)

	received := f.receivedDocument(t)

	rows := []struct {
		name   string
		viewer int64
		req    api.ProfilePhotoGet
	}{
		{
			// The capability is bound to the viewer it was issued to. Handing it
			// to another account buys nothing, and the two accounts' answers
			// are the same string.
			name:   "capability copied from another viewer",
			viewer: stranger.ID,
			req: api.ProfilePhotoGet{
				Peer:       api.InputPeerUser(stranger.ID, f.owner),
				PhotoID:    f.photoID,
				Credential: f.credential(f.viewer, f.owner, f.photoID),
				Limit:      100,
			},
		},
		{
			name:   "wrong owner: peer names a target with no such entry",
			viewer: f.viewer,
			req:    f.request(stranger.ID, f.photoID, 0, 100),
		},
		{
			name:   "foreign entry: the file sits in another owner's gallery",
			viewer: f.viewer,
			req:    f.request(f.owner, ownPhoto, 0, 100),
		},
		{
			name:   "no gallery entry",
			viewer: f.viewer,
			req:    f.request(f.owner, f.addStoredPhoto(t, f.owner), 0, 100),
		},
		{
			name:   "entry deleted",
			viewer: f.viewer,
			req: func() api.ProfilePhotoGet {
				fileID := f.addStoredPhoto(t, f.owner)
				f.addEntry(t, f.owner, fileID)
				f.deleteEntry(t, f.owner, fileID)
				return f.request(f.owner, fileID, 0, 100)
			}(),
		},
		{
			name:   "bytes never published",
			viewer: f.viewer,
			req: func() api.ProfilePhotoGet {
				pending, err := f.s.AllocateFile(context.Background(), f.owner, 10, "image/jpeg",
					"avatar.jpg", api.TestMaxUserStorageBytes)
				if err != nil {
					t.Fatalf("allocate file: %v", err)
				}
				f.addEntry(t, f.owner, pending.ID)
				return f.request(f.owner, pending.ID, 0, 100)
			}(),
		},
		{
			name:   "no such file",
			viewer: f.viewer,
			req:    f.request(f.owner, 999_999_999, 0, 100),
		},
		{
			// The peer access_hash and the file's raw access_hash are both
			// capabilities of another domain. Neither is a gallery credential.
			name:   "peer access hash as credential",
			viewer: f.viewer,
			req: func() api.ProfilePhotoGet {
				r := f.request(f.owner, f.photoID, 0, 100)
				r.Credential = api.DeriveUserHash(f.viewer, f.owner)
				return r
			}(),
		},
		{
			name:   "file's raw access hash as credential",
			viewer: f.viewer,
			req: func() api.ProfilePhotoGet {
				r := f.request(f.owner, f.photoID, 0, 100)
				r.Credential = f.accessHash(t, f.photoID)
				return r
			}(),
		},
		{
			name:   "file received in a message, not in any gallery",
			viewer: f.viewer,
			req:    f.request(f.owner, received, 0, 100),
		},
	}

	for _, r := range rows {
		_, err := getFile(context.Background(), r.viewer, r.req)
		if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
			t.Errorf("%s: got %s, want LOCATION_INVALID", r.name, msg)
		}
	}

	// A block is the same answer, and it is the one that arrives without the
	// request changing at all.
	f.block(t, f.owner, f.viewer)
	if _, err := getFile(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, 100)); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Errorf("target blocked the viewer: got %s, want LOCATION_INVALID", rpcMessage(t, err))
	}

	// Every rejection above is a client mistake on a path anyone can reach: none
	// of it is a server event, so none of it is logged.
	if len(logs.records) != 0 {
		t.Errorf("rejections logged %d records, want none: %v", len(logs.records), logs.records)
	}
}

// TestPhotoGalleryDownloadBlockRevokesAndUnblockRestores pins the block half of
// the gate, including the half the acceptance names: unblocking restores access
// only while the gallery row is live.
func TestPhotoGalleryDownloadBlockRevokesAndUnblockRestores(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500021", "+1555144500022")

	f.block(t, f.owner, f.viewer)
	if _, err := f.call(t, f.viewer, f.owner, f.photoID, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("blocked viewer: got %v, want LOCATION_INVALID", err)
	}
	f.unblock(t, f.owner, f.viewer)
	if got := f.get(t, f.viewer, f.owner, f.photoID); !bytes.Equal(got, galleryPayload) {
		t.Fatalf("after unblocking: served %q, want the photo bytes", got)
	}

	// The reverse edge is the viewer's own choice about their own inbox, and it
	// says nothing about whose avatar they may read.
	f.block(t, f.viewer, f.owner)
	if got := f.get(t, f.viewer, f.owner, f.photoID); !bytes.Equal(got, galleryPayload) {
		t.Fatalf("with the viewer blocking the owner: served %q, want the photo bytes", got)
	}
	f.unblock(t, f.viewer, f.owner)

	// A deleted entry with the block lifted is still no photo: the block was
	// never what made the photo readable, and unblocking cannot resurrect a
	// row that is gone.
	f.deleteEntry(t, f.owner, f.photoID)
	f.block(t, f.owner, f.viewer)
	f.unblock(t, f.owner, f.viewer)
	if _, err := f.call(t, f.viewer, f.owner, f.photoID, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("deleted entry after unblocking: got %v, want LOCATION_INVALID", err)
	}
}

// TestPhotoGalleryDownloadRevokesAtCommit is the lane-level commit order: an
// in-flight write is invisible to the lane's one statement, and the call that
// starts after the commit is the one that is denied. It is the same fact the
// store-level gate test pins, asserted through the handler that has to rely on
// it in order to hold nothing across a blob read.
func TestPhotoGalleryDownloadRevokesAtCommit(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500031", "+1555144500032")
	ctx := context.Background()
	writer := f.writer()

	deleted := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, deleted)
	blocked := f.addStoredPhoto(t, f.owner)
	f.addEntry(t, f.owner, blocked)

	tx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_photos WHERE user_id = $1 AND file_id = $2`, f.owner, deleted); err != nil {
		t.Fatalf("delete in transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO blocked_users (blocker_id, blocked_id) VALUES ($1, $2)`, f.owner, f.viewer); err != nil {
		t.Fatalf("block in transaction: %v", err)
	}
	if got := f.get(t, f.viewer, f.owner, deleted); !bytes.Equal(got, galleryPayload) {
		t.Fatalf("read with the delete uncommitted served %q, want the photo bytes", got)
	}
	if got := f.get(t, f.viewer, f.owner, blocked); !bytes.Equal(got, galleryPayload) {
		t.Fatalf("read with the block uncommitted served %q, want the photo bytes", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, fileID := range []int64{deleted, blocked} {
		if _, err := f.call(t, f.viewer, f.owner, fileID, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Fatalf("read of %d after the commit: got %v, want LOCATION_INVALID", fileID, err)
		}
	}
}

// TestPhotoGalleryDownloadAdmittedBytesFinish is the race the design accepts:
// the lane does not re-check after admission, so a delete committed while the
// bytes are on their way does not retract a read that was already authorized.
// The next call is the one that is denied.
func TestPhotoGalleryDownloadAdmittedBytesFinish(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500041", "+1555144500042")

	entered := f.blobs.pauseReads()
	served := make(chan []byte, 1)
	go func() {
		enc, err := f.seq(t)(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, len(galleryPayload)))
		if err != nil {
			served <- nil
			return
		}
		file, ok := enc.(*tg.UploadFile)
		if !ok {
			served <- nil
			return
		}
		served <- file.Bytes
	}()

	<-entered
	f.deleteEntry(t, f.owner, f.photoID)
	f.blobs.releaseReads()

	got := <-served
	if !bytes.Equal(got, galleryPayload) {
		t.Fatalf("admitted read served %q, want the photo bytes", got)
	}
	if _, err := f.call(t, f.viewer, f.owner, f.photoID, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("the call after the delete: got %v, want LOCATION_INVALID", err)
	}
}

// TestPhotoGalleryDownloadEraserRaceIsAnOrdinaryReadFailure is the other half
// of the same race: the gallery photo's bytes are reclaimable once nothing live
// references them, so a reclaim that lands between admission and the object read
// must answer the lane's one rejection, not a server fault, and must not log.
func TestPhotoGalleryDownloadEraserRaceIsAnOrdinaryReadFailure(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500051", "+1555144500052")
	logs := &captureHandler{}
	getFile := api.ProfilePhotoGetSeqForTestWithLogger(f.s, f.blobs, slog.New(logs),
		store.RateLimitConfig{}, store.RateLimitConfig{})

	entered := f.blobs.pauseReads()
	failed := make(chan string, 1)
	go func() {
		_, err := getFile(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, len(galleryPayload)))
		failed <- rpcMessage(t, err)
	}()

	<-entered
	// The reclaim the eraser performs: the entry goes, then the object.
	f.deleteEntry(t, f.owner, f.photoID)
	if err := f.blobs.Remove(context.Background(), blob.Key(f.photoID)); err != nil {
		t.Fatalf("reclaim the object: %v", err)
	}
	f.blobs.releaseReads()

	if msg := <-failed; msg != "LOCATION_INVALID" {
		t.Fatalf("read of a reclaimed photo: got %s, want LOCATION_INVALID", msg)
	}
	if _, err := f.call(t, f.viewer, f.owner, f.photoID, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("the call after the reclaim: got %v, want LOCATION_INVALID", err)
	}
	if len(logs.records) != 0 {
		t.Errorf("eraser race logged %d records, want none: %v", len(logs.records), logs.records)
	}
}

// TestPhotoGalleryDownloadMessageCredentialCannotSubstitute is the entitlement
// boundary in both directions. A gallery credential is not a message
// entitlement, and a message's raw files.access_hash is not a gallery
// credential: the two lanes' gates read disjoint facts.
func TestPhotoGalleryDownloadMessageCredentialCannotSubstitute(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500061", "+1555144500062")

	// A document the viewer received: entitled through the message lane, and
	// through nothing in the gallery lane.
	received := f.receivedDocument(t)
	accessHash := f.accessHash(t, received)
	enc, err := api.GetFileForTest(f.s, f.viewer, f.blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: received, AccessHash: accessHash},
		Limit:    len(galleryPayload),
	})
	if err != nil {
		t.Fatalf("message-lane read of a received document: %v", err)
	}
	if file, ok := enc.(*tg.UploadFile); !ok || !bytes.Equal(file.Bytes, galleryPayload) {
		t.Fatalf("message lane served %v, want the file bytes", enc)
	}

	// The same viewer, shown the gallery credential for that same file:
	// no gallery entry, so the gallery lane denies.
	if _, err := f.call(t, f.viewer, f.owner, received, 0, 100); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("gallery read of a file no gallery names: got %v, want LOCATION_INVALID", err)
	}

	// The reverse: a viewer entitled to an avatar is not entitled to the
	// photo's bytes through the message lane. The raw hash is the message
	// lane's credential, and the avatar has no live message naming it.
	avatarHash := f.accessHash(t, f.photoID)
	_, err = api.GetFileForTest(f.s, f.viewer, f.blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputPhotoFileLocation{ID: f.photoID, AccessHash: avatarHash, ThumbSize: "x"},
		Limit:    len(galleryPayload),
	})
	if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
		t.Fatalf("message-lane read of a gallery photo: got %s, want LOCATION_INVALID", msg)
	}
	if _, err := api.GetFileForTest(f.s, f.viewer, f.blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: f.photoID, AccessHash: f.credential(f.viewer, f.owner, f.photoID)},
		Limit:    len(galleryPayload),
	}); rpcMessage(t, err) != "LOCATION_INVALID" {
		t.Fatalf("message-lane read with a gallery credential: got %v, want LOCATION_INVALID", err)
	}
}

// TestPhotoGalleryDownloadPausedReadHoldsNoOwnerLockAndNoTransaction is the
// resource criterion. The messaging lane serializes an account's sends
// on a per-owner advisory key, so an avatar read that took that key — or ran
// inside a transaction that did — would let one viewer's avatar traffic queue the
// owner's own DMs and group sends behind it. With a blob read parked, the
// observers prove: the owner's key is free, no advisory lock is held by this
// database, nothing is left idle in transaction, and the owner's sends commit.
func TestPhotoGalleryDownloadPausedReadHoldsNoOwnerLockAndNoTransaction(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500071", "+1555144500072")

	entered := f.blobs.pauseReads()
	done := make(chan error, 1)
	go func() {
		_, err := f.seq(t)(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, len(galleryPayload)))
		done <- err
	}()
	<-entered

	observer := f.observer()
	if n := galleryAdvisoryLockCount(t, observer); n != 0 {
		t.Fatalf("granted advisory locks during the read = %d, want 0", n)
	}
	if !galleryTryOwnerLock(t, observer, f.owner) {
		t.Fatalf("the owner's messaging advisory key is held during the read: the lane took it")
	}
	if n := galleryIdleInTransactionCount(t, observer); n != 0 {
		t.Fatalf("sessions idle in transaction during the read = %d, want 0: a transaction spans ReadAt", n)
	}

	// The traffic the lane must not stall, while the read is still parked.
	f.sendWithin(t, f.owner, f.viewer, "dm out during the read")
	f.sendWithin(t, f.viewer, f.owner, "dm in during the read")
	f.groupSendWithin(t, f.viewer, "group send during the read")

	f.blobs.releaseReads()
	if err := <-done; err != nil {
		t.Fatalf("paused read: %v", err)
	}

	// The control that gives the assertions above teeth: with the owner's key
	// genuinely held by another session, the same send does not complete. A lane
	// that took the key would show up as a stalled send, and a test that never
	// demonstrates a stall would not notice.
	f.sendStallsWhileOwnerKeyHeld(t)
}

// TestPhotoGalleryDownloadHammeringDoesNotStallTargetMessaging is the same
// property under load rather than in one sample: one viewer reading a peer's
// avatar over and over, with the peer's own DM and group sends
// between every read. The reads are rate-limited on the viewer, never on the
// target, and the target's key is never taken.
func TestPhotoGalleryDownloadHammeringDoesNotStallTargetMessaging(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500081", "+1555144500082")
	getFile := f.seq(t)
	observer := f.observer()

	const rounds = 25
	for i := range rounds {
		req := f.request(f.owner, f.photoID, int64(i%len(galleryPayload)), 16)
		enc, err := getFile(context.Background(), f.viewer, req)
		if err != nil {
			t.Fatalf("round %d: gallery read: %v", i, err)
		}
		if _, ok := enc.(*tg.UploadFile); !ok {
			t.Fatalf("round %d: result type = %T, want *tg.UploadFile", i, enc)
		}
		// The target's own traffic, on the far side of a lane that must not
		// serialize any of it.
		f.sendWithin(t, f.owner, f.viewer, fmt.Sprintf("dm %d", i))
		f.groupSendWithin(t, f.viewer, fmt.Sprintf("group %d", i))
		if n := galleryAdvisoryLockCount(t, observer); n != 0 {
			t.Fatalf("round %d: granted advisory locks = %d, want 0", i, n)
		}
	}
	if got := f.blobs.reads.Load(); got != rounds {
		t.Fatalf("blob reads = %d, want %d", got, rounds)
	}
}

// TestPhotoGalleryDownloadSharesGetFileInFlightSlot pins the resource the
// download path holds per account: one in-flight slot, shared by both lanes. A
// second lane with a slot of its own would double how many blob reads one
// account can keep open at once.
func TestPhotoGalleryDownloadSharesGetFileInFlightSlot(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500091", "+1555144500092")

	entered := f.blobs.pauseReads()
	gallery := make(chan error, 1)
	go func() {
		_, err := f.seq(t)(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, len(galleryPayload)))
		gallery <- err
	}()
	<-entered
	// With the gallery read in flight, the account's next download of any kind is
	// the busy answer, and the gallery lane's own second read is too.
	if _, err := api.GetFileForTest(f.s, f.viewer, f.blobs, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: f.photoID, AccessHash: f.accessHash(t, f.photoID)},
		Limit:    16,
	}); rpcMessage(t, err) != "FLOOD_WAIT_1" {
		t.Fatalf("download while a gallery read is in flight: got %v, want the busy answer", err)
	}
	if _, err := f.seq(t)(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, 100)); rpcMessage(t, err) != "FLOOD_WAIT_1" {
		t.Fatalf("second gallery read while one is in flight: got %v, want the busy answer", err)
	}
	f.blobs.releaseReads()
	if err := <-gallery; err != nil {
		t.Fatalf("parked gallery read: %v", err)
	}
	// The slot is released with the request, so the account reads again.
	if got := f.get(t, f.viewer, f.owner, f.photoID); !bytes.Equal(got, galleryPayload) {
		t.Fatal("the slot stayed held after the read finished")
	}
}

// TestPhotoGalleryDownloadSharesGetFileRateLimit pins that the gallery lane is
// charged to upload.getFile's per-account download budget: adding a lane must not
// add an allowance, and the counter is one per account and surface, not one per
// code path. One read on each lane fits a budget of two; the third is denied
// on both lanes.
func TestPhotoGalleryDownloadSharesGetFileRateLimit(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500093", "+1555144500094")
	// A file the viewer may read through the message lane, so the message-lane
	// calls below get past authorization and are judged on the budget alone.
	received := f.receivedDocument(t)
	messageReq := &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: received, AccessHash: f.accessHash(t, received)},
		Limit:    16,
	}

	budget := store.RateLimitConfig{Limit: 2, Window: time.Second}
	gallery := api.ProfilePhotoGetSeqForTestWithLimits(f.s, f.blobs, budget, store.RateLimitConfig{})
	getFile := api.GetFileSeqForTestWithLimits(f.s, f.blobs, budget, store.RateLimitConfig{})

	if _, err := gallery(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, 16)); err != nil {
		t.Fatalf("first gallery read inside the budget: %v", err)
	}
	if _, err := getFile(f.viewer, messageReq); err != nil {
		t.Fatalf("first message-lane read inside the budget: %v", err)
	}
	if _, err := gallery(context.Background(), f.viewer, f.request(f.owner, f.photoID, 0, 16)); rpcMessage(t, err) != "FLOOD_WAIT_1" {
		t.Fatalf("gallery read past the account's download budget: got %v, want FLOOD_WAIT_1", err)
	}
	if _, err := getFile(f.viewer, messageReq); rpcMessage(t, err) != "FLOOD_WAIT_1" {
		t.Fatalf("message-lane read after the gallery lane spent the budget: got %v, want FLOOD_WAIT_1", err)
	}
}

// TestPhotoGalleryDownloadWindowBoundsAreGetFileBounds pins that the gallery lane
// inherited upload.getFile's window rules unchanged, including the cap that
// bounds one reply and therefore the per-request buffer.
func TestPhotoGalleryDownloadWindowBoundsAreGetFileBounds(t *testing.T) {
	t.Parallel()
	f := newGalleryFixture(t, "+1555144500095", "+1555144500096")

	windows := []struct {
		name   string
		offset int64
		limit  int
	}{
		{"zero limit", 0, 0},
		{"negative limit", 0, -1},
		{"limit past the protocol maximum", 0, api.MaxDownloadChunk + 1},
		{"negative offset", -1, 16},
		{"offset past the end", int64(len(galleryPayload)) + 1, 16},
	}
	for _, w := range windows {
		if _, err := f.call(t, f.viewer, f.owner, f.photoID, w.offset, w.limit); rpcMessage(t, err) != "LOCATION_INVALID" {
			t.Errorf("%s: got %v, want LOCATION_INVALID", w.name, err)
		}
	}
}

// send runs one direct send and hands back what these tests read: the row, the
// dedup flag, and the error. The store's five-result signature is not what a
// messaging-progress assertion is about.
func (f *galleryFixture) send(ctx context.Context, fromID, toID int64, text string) (store.Message, bool, error) {
	sender, _, _, dup, err := f.s.SendMessage(ctx, fromID, toID, text, f.random(), 0, 0)
	return sender, dup, err
}

// sendWithin runs one direct send and fails the test if it does not commit
// inside the deadline. A send that blocks behind a lock is the failure these
// tests are looking for, so the deadline is the assertion.
func (f *galleryFixture) sendWithin(t *testing.T, fromID, toID int64, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sender, dup, err := f.send(ctx, fromID, toID, text)
	if err != nil {
		t.Fatalf("send %q from %d to %d: %v", text, fromID, toID, err)
	}
	if dup {
		t.Fatalf("send %q reported a duplicate", text)
	}
	if sender.LocalID == 0 {
		t.Fatalf("send %q committed no message", text)
	}
}

// groupSendWithin runs one chat send in a chat the owner is a member of.
func (f *galleryFixture) groupSendWithin(t *testing.T, fromID int64, text string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	chat, err := f.chat(ctx)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if _, _, _, err := f.s.SendChatMessage(ctx, store.FanOut{
		ChatID: chat.ID, FromID: fromID, Text: text, RandomID: f.random(),
	}); err != nil {
		t.Fatalf("group send %q from %d: %v", text, fromID, err)
	}
}

// chat is a group the owner and the viewer are both in, created once per
// fixture: creating it is not the measurement, and a fresh chat per send would
// put chat-creation locks in the middle of what these tests watch.
func (f *galleryFixture) chat(ctx context.Context) (store.Chat, error) {
	if f.chatReady {
		return f.chatRow, nil
	}
	chat, err := f.s.CreateChat(ctx, f.owner, "gallery observers", []int64{f.viewer})
	if err != nil {
		return store.Chat{}, err
	}
	f.chatRow, f.chatReady = chat, true
	return chat, nil
}

// sendStallsWhileOwnerKeyHeld is the control: a session holding the owner's
// advisory key makes the same send miss its deadline. Without it, "the sends
// completed while the read was parked" would only show that a send never blocks
// for anything at all.
func (f *galleryFixture) sendStallsWhileOwnerKeyHeld(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	holder := f.writer()
	tx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, f.owner); err != nil {
		t.Fatalf("take the owner's key: %v", err)
	}
	if n := galleryAdvisoryLockCount(t, f.observer()); n != 1 {
		t.Fatalf("control: advisory locks held = %d, want 1", n)
	}

	quick, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	_, _, err = f.send(quick, f.owner, f.viewer, "must not commit while held")
	if err == nil {
		t.Fatal("control: the send committed while the owner's key was held by another session")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("control send error: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit holder: %v", err)
	}
}

// receivedDocument leaves the viewer with a stored document the owner sent them:
// a file with a real message-lane entitlement and no gallery entry. It returns
// the file id.
func (f *galleryFixture) receivedDocument(t *testing.T) int64 {
	t.Helper()
	const clientFileID = 4450001
	saveParts(t, f.s, f.owner, clientFileID, galleryPayload)
	enc, err := api.SendMediaForTest(f.s, f.owner, f.blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(f.owner, f.viewer),
		Media:    uploadedDocument(clientFileID, 1, "note.txt", "text/plain"),
		RandomID: clientFileID,
	})
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	doc := documentOf(t, enc)
	return doc.ID
}

// accessHash reads the raw files.access_hash of one file: the message lane's
// credential, never the gallery lane's.
func (f *galleryFixture) accessHash(t *testing.T, fileID int64) int64 {
	t.Helper()
	files, err := f.s.FilesByIDs(context.Background(), []int64{fileID})
	if err != nil {
		t.Fatalf("load file metadata: %v", err)
	}
	file, ok := files[fileID]
	if !ok {
		t.Fatalf("file %d has no row", fileID)
	}
	return file.AccessHash
}

// random is a per-fixture send token: unique within the fixture, which is what
// the store's send dedup keys on.
func (f *galleryFixture) random() int64 { return f.rand.Add(1) }

// galleryAdvisoryLockCount counts the advisory locks granted in this test's
// database. pg_locks is cluster-wide and every cloned database numbers its users
// from the same small sequence, so the database is the only meaningful scope.
func galleryAdvisoryLockCount(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_locks l
		JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted AND a.datname = current_database()`).Scan(&n); err != nil {
		t.Fatalf("count advisory locks: %v", err)
	}
	return n
}

// galleryTryOwnerLock reports whether the messaging lane's per-owner advisory key
// is free: taking it here and releasing it at once is the direct statement that
// the download path is not holding it.
func galleryTryOwnerLock(t *testing.T, conn *pgx.Conn, ownerID int64) bool {
	t.Helper()
	var got bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1)`, ownerID).Scan(&got); err != nil {
		t.Fatalf("try the owner's advisory key: %v", err)
	}
	if got {
		if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, ownerID); err != nil {
			t.Fatalf("release the owner's advisory key: %v", err)
		}
	}
	return got
}

// galleryIdleInTransactionCount counts sessions in this database sitting inside
// an open transaction. The lane's gate is one autocommit statement, so during a
// blob read this must be zero.
func galleryIdleInTransactionCount(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND state = 'idle in transaction'`).Scan(&n); err != nil {
		t.Fatalf("count idle-in-transaction sessions: %v", err)
	}
	return n
}
