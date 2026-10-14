package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const albumGrantTTL = 6 * time.Hour

type albumFixture struct {
	s    *store.Store
	conn *pgx.Conn
}

func newAlbumFixture(t *testing.T) albumFixture {
	t.Helper()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect album fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close album fixture: %v", err)
		}
	})
	return albumFixture{s: s, conn: conn}
}

// granted stores a file for ownerID and gives it an upload grant, the state
// uploadMedia leaves behind.
func (f albumFixture) granted(t *testing.T, ownerID int64, photo bool) store.File {
	t.Helper()
	var file store.File
	if photo {
		file = storedPhotoFile(t, f.s, ownerID)
	} else {
		file = storedFileWithBytes(t, f.s, ownerID)
	}
	if _, err := f.conn.Exec(context.Background(),
		`INSERT INTO file_upload_grants (file_id, owner_id) VALUES ($1, $2)`, file.ID, ownerID); err != nil {
		t.Fatalf("grant file %d: %v", file.ID, err)
	}
	return file
}

func (f albumFixture) grantCount(t *testing.T, ownerID int64) int {
	t.Helper()
	var n int
	if err := f.conn.QueryRow(context.Background(),
		`SELECT count(*) FROM file_upload_grants WHERE owner_id = $1`, ownerID).Scan(&n); err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

func (f albumFixture) messageCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.conn.QueryRow(context.Background(), `SELECT count(*) FROM messages WHERE grouped_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count album rows: %v", err)
	}
	return n
}

func albumOf(fromID, groupedID int64, randomBase int64, files ...store.File) store.AlbumSend {
	items := make([]store.AlbumItem, len(files))
	for i, file := range files {
		items[i] = store.AlbumItem{
			FileID: file.ID, AccessHash: file.AccessHash, Photo: file.Kind == store.FileKindPhoto,
			RandomID: randomBase + int64(i), Text: "caption",
		}
	}
	return store.AlbumSend{FromID: fromID, Items: items, GroupedID: groupedID, GrantTTL: albumGrantTTL}
}

func TestSendUserAlbumWritesEveryCopyWithOneGroupedID(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b := mustUser(t, f.s, "+15559185001"), mustUser(t, f.s, "+15559185002")
	files := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, false), f.granted(t, a.ID, true)}

	res, err := f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 77, 500, files...))
	if err != nil {
		t.Fatalf("send album: %v", err)
	}
	if res.Duplicate || len(res.Messages) != 3 || len(res.Pts) != 3 {
		t.Fatalf("result = %+v, want 3 new sender rows", res)
	}
	for i, m := range res.Messages {
		if m.GroupedID == nil || *m.GroupedID != 77 || m.FileID != files[i].ID || m.RandomID != 500+int64(i) || !m.Out {
			t.Fatalf("sender row %d = %+v, want grouped 77, file %d", i, m, files[i].ID)
		}
		if i > 0 && (m.LocalID != res.Messages[i-1].LocalID+1 || res.Pts[i] != res.Pts[i-1]+1) {
			t.Fatalf("sender rows not consecutive: %+v pts %v", res.Messages, res.Pts)
		}
		copyRow := msgAt(t, f.s, b.ID, m.PeerLocalID)
		if copyRow.GroupedID == nil || *copyRow.GroupedID != 77 || copyRow.FileID != files[i].ID || copyRow.Out || copyRow.PeerLocalID != m.LocalID {
			t.Fatalf("recipient row %d = %+v, want linked copy in album 77", i, copyRow)
		}
	}
	if res.PerOwner[a.ID] != res.Pts[2] || res.PerOwner[b.ID] == 0 {
		t.Fatalf("per-owner pts = %v, want sender %d and recipient", res.PerOwner, res.Pts[2])
	}
	if n := f.grantCount(t, a.ID); n != 0 {
		t.Fatalf("grants after send = %d, want all consumed", n)
	}
	if _, err = f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 78, 600, files...)); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("second use of consumed grants = %v, want ErrMediaInvalid", err)
	}
}

func TestSendUserAlbumOneBadItemConsumesNothing(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b := mustUser(t, f.s, "+15559185011"), mustUser(t, f.s, "+15559185012")
	files := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, true), f.granted(t, a.ID, true)}
	foreign := f.granted(t, b.ID, true)
	ungranted := storedPhotoFile(t, f.s, a.ID)

	wrongHash := files[2]
	wrongHash.AccessHash++
	wrongKind := files[2]
	wrongKind.Kind = store.FileKindDocument
	unknown := store.File{ID: files[2].ID + 100000, AccessHash: files[2].AccessHash, Kind: store.FileKindPhoto}
	for name, bad := range map[string]store.File{
		"wrong hash": wrongHash, "kind mismatch": wrongKind, "unknown id": unknown,
		"another account's grant": foreign, "file without grant": ungranted,
	} {
		_, err := f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 90, 700, files[0], files[1], bad))
		if !errors.Is(err, store.ErrMediaInvalid) {
			t.Fatalf("%s: err = %v, want ErrMediaInvalid", name, err)
		}
	}
	if n := f.messageCount(t); n != 0 {
		t.Fatalf("album rows after refusals = %d, want 0", n)
	}
	if n := f.grantCount(t, a.ID); n != 3 {
		t.Fatalf("grants after refusals = %d, want 3", n)
	}
	if n := f.grantCount(t, b.ID); n != 1 {
		t.Fatalf("other account's grants = %d, want 1", n)
	}
	if _, err := f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 91, 700, files...)); err != nil {
		t.Fatalf("send after refusals: %v", err)
	}
}

func TestSendUserAlbumExpiredAndErasedGrantsRefused(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b := mustUser(t, f.s, "+15559185021"), mustUser(t, f.s, "+15559185022")
	files := []store.File{f.granted(t, a.ID, false), f.granted(t, a.ID, false)}

	if _, err := f.conn.Exec(ctx, `UPDATE file_upload_grants SET created_at = now() - interval '7 hours' WHERE file_id = $1`, files[0].ID); err != nil {
		t.Fatalf("age grant: %v", err)
	}
	if _, err := f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 5, 800, files...)); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("expired grant: err = %v, want ErrMediaInvalid", err)
	}

	fresh := f.granted(t, a.ID, false)
	if _, err := f.conn.Exec(ctx, `DELETE FROM files WHERE id = $1`, files[1].ID); err != nil {
		t.Fatalf("erase file: %v", err)
	}
	if _, err := f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 6, 810, fresh, files[1])); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("erased file: err = %v, want ErrMediaInvalid", err)
	}
	if n := f.messageCount(t); n != 0 {
		t.Fatalf("album rows = %d, want 0", n)
	}
}

func TestSendUserAlbumConcurrentUseSendsOnce(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b := mustUser(t, f.s, "+15559185031"), mustUser(t, f.s, "+15559185032")
	files := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, true)}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Go(func() {
			_, errs[i] = f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, int64(100+i), int64(1000+10*i), files...))
		})
	}
	wg.Wait()
	sent := 0
	for _, err := range errs {
		switch {
		case err == nil:
			sent++
		case !errors.Is(err, store.ErrMediaInvalid):
			t.Fatalf("concurrent send: %v", err)
		}
	}
	if sent != 1 {
		t.Fatalf("concurrent sends succeeded %d times, want 1 (%v)", sent, errs)
	}
	if n := f.messageCount(t); n != 4 {
		t.Fatalf("album rows = %d, want 2 items x 2 owners", n)
	}
}

func TestSendUserAlbumResendRules(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b := mustUser(t, f.s, "+15559185041"), mustUser(t, f.s, "+15559185042")
	files := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, true), f.granted(t, a.ID, true)}
	album := albumOf(a.ID, 9, 1100, files...)
	sent, err := f.s.SendUserAlbum(ctx, b.ID, album)
	if err != nil {
		t.Fatalf("send album: %v", err)
	}

	album.GroupedID = 10
	replay, err := f.s.SendUserAlbum(ctx, b.ID, album)
	if err != nil || !replay.Duplicate {
		t.Fatalf("full resend = (%+v, %v), want replay", replay, err)
	}
	for i := range sent.Messages {
		if replay.Messages[i].LocalID != sent.Messages[i].LocalID || replay.Pts[i] != sent.Pts[i] {
			t.Fatalf("replay %d = (%d, pts %d), want (%d, pts %d)", i, replay.Messages[i].LocalID, replay.Pts[i], sent.Messages[i].LocalID, sent.Pts[i])
		}
	}
	pre, err := f.s.UserAlbumRetry(ctx, a.ID, store.PeerTypeUser, b.ID, []int64{1100, 1101, 1102})
	if err != nil || !pre.Duplicate {
		t.Fatalf("read-only retry = (%+v, %v), want replay", pre, err)
	}
	if n := f.messageCount(t); n != 6 {
		t.Fatalf("album rows after replay = %d, want 6", n)
	}

	more := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, true)}
	mixed := albumOf(a.ID, 11, 1101, more...) // 1101 is known, 1102 is known: a subset of the album
	if _, err = f.s.SendUserAlbum(ctx, b.ID, mixed); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("subset resend = %v, want ErrMediaInvalid", err)
	}
	partial := albumOf(a.ID, 12, 1102, more...) // 1102 known, 1103 new
	if _, err = f.s.SendUserAlbum(ctx, b.ID, partial); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("partial resend = %v, want ErrMediaInvalid", err)
	}
	if n := f.grantCount(t, a.ID); n != 2 {
		t.Fatalf("grants after refused resends = %d, want 2", n)
	}

	if _, err = f.s.DeleteMessages(ctx, b.ID, []int64{sent.Messages[1].PeerLocalID}, true); err != nil {
		t.Fatalf("recipient revokes one item: %v", err)
	}
	if _, err = f.s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 13, 1100, files...)); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("resend after delete = %v, want ErrMediaInvalid", err)
	}
	if _, err = f.s.UserAlbumRetry(ctx, a.ID, store.PeerTypeUser, b.ID, []int64{1100, 1101, 1102}); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("read-only retry after delete = %v, want ErrMediaInvalid", err)
	}
}

func TestSendChatAlbumFansOutAndChecksEveryItemsRights(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	a, b, c := mustUser(t, f.s, "+15559185051"), mustUser(t, f.s, "+15559185052"), mustUser(t, f.s, "+15559185053")
	chat, err := f.s.CreateChat(ctx, a.ID, "album", []int64{b.ID, c.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	photo, doc := f.granted(t, b.ID, true), f.granted(t, b.ID, false)
	if _, _, err = f.s.SetChatDefaultBannedRights(ctx, chat.ID, a.ID, []string{"send_docs"}); err != nil {
		t.Fatalf("ban documents: %v", err)
	}
	if _, err = f.s.SendChatAlbum(ctx, chat.ID, albumOf(b.ID, 21, 1200, photo, doc)); !errors.Is(err, store.ErrChatWriteForbidden) {
		t.Fatalf("photo + document under send_docs ban = %v, want ErrChatWriteForbidden", err)
	}
	if n := f.grantCount(t, b.ID); n != 2 {
		t.Fatalf("grants after refusal = %d, want 2", n)
	}
	if _, _, err = f.s.SetChatDefaultBannedRights(ctx, chat.ID, a.ID, []string{}); err != nil {
		t.Fatalf("clear bans: %v", err)
	}

	res, err := f.s.SendChatAlbum(ctx, chat.ID, albumOf(b.ID, 22, 1200, photo, doc))
	if err != nil {
		t.Fatalf("send chat album: %v", err)
	}
	if len(res.PerOwner) != 3 || len(res.Messages) != 2 {
		t.Fatalf("result = %+v, want 2 items to 3 owners", res)
	}
	var copies, fanouts int
	if err = f.conn.QueryRow(ctx,
		`SELECT count(*), count(DISTINCT fanout_id) FROM messages WHERE peer_type = 2 AND peer_id = $1 AND grouped_id = 22`,
		chat.ID).Scan(&copies, &fanouts); err != nil {
		t.Fatalf("count chat album rows: %v", err)
	}
	if copies != 6 || fanouts != 2 {
		t.Fatalf("chat album rows = %d over %d fan-outs, want 6 over 2", copies, fanouts)
	}
}

func TestPostChannelAlbumSharesGroupedIDAndReplays(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	owner := mustUser(t, f.s, "+15559185061")
	channel := mustChannel(t, f.s, owner.ID, "album channel")
	files := []store.File{f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)}

	res, err := f.s.PostChannelAlbumAs(ctx, channel.ID, 0, albumOf(owner.ID, 31, 1300, files...))
	if err != nil {
		t.Fatalf("post channel album: %v", err)
	}
	if res.Duplicate || len(res.Messages) != 2 {
		t.Fatalf("result = %+v, want 2 new posts", res)
	}
	for i, m := range res.Messages {
		if m.GroupedID == nil || *m.GroupedID != 31 || m.FileID == nil || *m.FileID != files[i].ID {
			t.Fatalf("post %d = %+v, want photo %d in album 31", i, m, files[i].ID)
		}
	}
	replay, err := f.s.ChannelAlbumRetryAs(ctx, channel.ID, owner.ID, []int64{1300, 1301})
	if err != nil || !replay.Duplicate || replay.Pts[1] != res.Pts[1] {
		t.Fatalf("channel retry = (%+v, %v), want replay at pts %v", replay, err, res.Pts)
	}
	doc := f.granted(t, owner.ID, false)
	if _, err = f.s.PostChannelAlbumAs(ctx, channel.ID, 0, albumOf(owner.ID, 32, 1400, f.granted(t, owner.ID, true), doc)); !errors.Is(err, store.ErrMediaInvalid) {
		t.Fatalf("document in channel album = %v, want ErrMediaInvalid", err)
	}
}
