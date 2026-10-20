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

func TestExpiredRandomIDRetriesDoNotReturnMessageContents(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	owner := mustUser(t, f.s, "+15559185071")
	peer := mustUser(t, f.s, "+15559185072")

	const directRandomID = int64(15001)
	if _, _, _, _, err := f.s.SendMessage(ctx, owner.ID, peer.ID, "expired text", directRandomID, 0, 0); err != nil {
		t.Fatalf("send direct message: %v", err)
	}
	if _, err := f.conn.Exec(ctx, `UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND random_id = $2`, owner.ID, directRandomID); err != nil {
		t.Fatalf("expire direct message: %v", err)
	}
	message, found, err := f.s.MessageByRandomID(ctx, owner.ID, directRandomID)
	if err == nil || found || message.LocalID != 0 || message.Text != "" || message.FileID != 0 {
		t.Fatalf("expired direct lookup = (%+v, found=%v, err=%v), want refusal without row contents", message, found, err)
	}
	if _, _, _, duplicate, sendErr := f.s.SendMessage(ctx, owner.ID, peer.ID, "replacement", directRandomID, 0, 0); !errors.Is(sendErr, store.ErrRandomIDExpired) || duplicate {
		t.Fatalf("expired direct send retry = duplicate=%v err=%v, want expired refusal", duplicate, sendErr)
	}

	const channelTextRandomID = int64(15002)
	channel := mustChannel(t, f.s, owner.ID, "expired retry channel")
	if _, _, _, err = f.s.PostChannelMessageAs(ctx, channel.ID, owner.ID, "expired channel text", channelTextRandomID, nil, 0); err != nil {
		t.Fatalf("post channel text: %v", err)
	}
	if _, err = f.conn.Exec(ctx, `UPDATE channel_messages SET expires_at = statement_timestamp() - interval '1 second' WHERE channel_id = $1 AND random_id = $2`, channel.ID, channelTextRandomID); err != nil {
		t.Fatalf("expire channel text: %v", err)
	}
	channelMessage, _, duplicate, err := f.s.ChannelTextMessageRetryAs(ctx, channel.ID, owner.ID, channelTextRandomID)
	if err == nil || duplicate || channelMessage.LocalID != 0 || channelMessage.Message != "" {
		t.Fatalf("expired channel text retry = (%+v, duplicate=%v, err=%v), want refusal without post contents", channelMessage, duplicate, err)
	}
	if _, _, _, postErr := f.s.PostChannelMessageAs(ctx, channel.ID, owner.ID, "replacement", channelTextRandomID, nil, 0); !errors.Is(postErr, store.ErrRandomIDDuplicate) {
		t.Fatalf("expired channel text send retry = %v, want duplicate refusal", postErr)
	}

	userAlbumFiles := []store.File{f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)}
	const userAlbumRandomID = int64(15100)
	if _, err = f.s.SendUserAlbum(ctx, peer.ID, albumOf(owner.ID, 151, userAlbumRandomID, userAlbumFiles...)); err != nil {
		t.Fatalf("send user album: %v", err)
	}
	if _, err = f.conn.Exec(ctx, `UPDATE messages SET expires_at = statement_timestamp() - interval '1 second' WHERE owner_id = $1 AND random_id = ANY($2)`, owner.ID, []int64{userAlbumRandomID, userAlbumRandomID + 1}); err != nil {
		t.Fatalf("expire user album: %v", err)
	}
	userAlbum, err := f.s.UserAlbumRetry(ctx, owner.ID, store.PeerTypeUser, peer.ID, []int64{userAlbumRandomID, userAlbumRandomID + 1})
	if err == nil || userAlbum.Duplicate || len(userAlbum.Messages) != 0 {
		t.Fatalf("expired user album retry = (%+v, err=%v), want refusal without album contents", userAlbum, err)
	}
	userRetryFiles := []store.File{f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)}
	if _, sendErr := f.s.SendUserAlbum(ctx, peer.ID, albumOf(owner.ID, 153, userAlbumRandomID, userRetryFiles...)); !errors.Is(sendErr, store.ErrRandomIDExpired) {
		t.Fatalf("expired user album send retry = %v, want expired refusal", sendErr)
	}

	channelAlbumFiles := []store.File{f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)}
	const channelAlbumRandomID = int64(15200)
	if _, err = f.s.PostChannelAlbumAs(ctx, channel.ID, 0, albumOf(owner.ID, 152, channelAlbumRandomID, channelAlbumFiles...)); err != nil {
		t.Fatalf("post channel album: %v", err)
	}
	if _, err = f.conn.Exec(ctx, `UPDATE channel_messages SET expires_at = statement_timestamp() - interval '1 second' WHERE channel_id = $1 AND random_id = ANY($2)`, channel.ID, []int64{channelAlbumRandomID, channelAlbumRandomID + 1}); err != nil {
		t.Fatalf("expire channel album: %v", err)
	}
	channelAlbum, err := f.s.ChannelAlbumRetryAs(ctx, channel.ID, owner.ID, []int64{channelAlbumRandomID, channelAlbumRandomID + 1})
	if err == nil || channelAlbum.Duplicate || len(channelAlbum.Messages) != 0 {
		t.Fatalf("expired channel album retry = (%+v, err=%v), want refusal without album contents", channelAlbum, err)
	}
	channelRetryFiles := []store.File{f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)}
	if _, sendErr := f.s.PostChannelAlbumAs(ctx, channel.ID, 0, albumOf(owner.ID, 154, channelAlbumRandomID, channelRetryFiles...)); !errors.Is(sendErr, store.ErrRandomIDDuplicate) {
		t.Fatalf("expired channel album send retry = %v, want duplicate refusal", sendErr)
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

// TestPostChannelAlbumReversedGrantOrderAcrossChannels: two albums from one
// caller to different channels name the same grants in opposite orders. The
// channel paths take no shared owner lock, so only the grant lock order keeps
// them from deadlocking; the loser must answer ErrMediaInvalid.
func TestPostChannelAlbumReversedGrantOrderAcrossChannels(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	owner := mustUser(t, f.s, "+15559185071")
	channels := []store.Channel{mustChannel(t, f.s, owner.ID, "first"), mustChannel(t, f.s, owner.ID, "second")}

	for round := range 20 {
		first, second := f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)
		albums := []store.AlbumSend{
			albumOf(owner.ID, int64(1000+2*round), int64(20000+100*round), first, second),
			albumOf(owner.ID, int64(1001+2*round), int64(20050+100*round), second, first),
		}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Go(func() {
				<-start
				_, errs[i] = f.s.PostChannelAlbumAs(ctx, channels[i].ID, 0, albums[i])
			})
		}
		close(start)
		wg.Wait()
		sent := 0
		for _, err := range errs {
			switch {
			case err == nil:
				sent++
			case !errors.Is(err, store.ErrMediaInvalid):
				t.Fatalf("round %d: losing album = %v, want ErrMediaInvalid", round, err)
			}
		}
		if sent != 1 {
			t.Fatalf("round %d: %d albums sent, want exactly 1 (%v)", round, sent, errs)
		}
	}
}

// TestAlbumGrantsLockInAscendingFileOrder pins the order the reversed-order
// test above relies on: with the lower grant held elsewhere, an album naming
// the higher one first must wait on the lower grant before it locks the
// higher one, so two albums can never each hold one and wait for the other.
func TestAlbumGrantsLockInAscendingFileOrder(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	ctx := context.Background()
	owner := mustUser(t, f.s, "+15559185081")
	channel := mustChannel(t, f.s, owner.ID, "ordered")
	low, high := f.granted(t, owner.ID, true), f.granted(t, owner.ID, true)
	// Re-insert the lower grant so it sits after the higher one in the heap: a
	// scan in physical order then meets the higher grant first.
	if _, err := f.conn.Exec(ctx, `DELETE FROM file_upload_grants WHERE file_id = $1`, low.ID); err != nil {
		t.Fatalf("drop lower grant: %v", err)
	}
	if _, err := f.conn.Exec(ctx, `INSERT INTO file_upload_grants (file_id, owner_id) VALUES ($1, $2)`, low.ID, owner.ID); err != nil {
		t.Fatalf("re-grant lower file: %v", err)
	}

	holder, err := f.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin holder: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }() //nolint:errcheck // released below on the success path
	if _, err = holder.Exec(ctx, `SELECT 1 FROM file_upload_grants WHERE file_id = $1 FOR UPDATE`, low.ID); err != nil {
		t.Fatalf("hold lower grant: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, postErr := f.s.PostChannelAlbumAs(ctx, channel.ID, 0, albumOf(owner.ID, 3000, 30000, high, low))
		done <- postErr
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err = f.conn.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting); err != nil {
			t.Fatalf("read lock waits: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("album never waited on the held lower grant")
		}
		time.Sleep(10 * time.Millisecond)
	}

	probe, err := pgx.Connect(ctx, f.conn.Config().ConnString())
	if err != nil {
		t.Fatalf("connect probe: %v", err)
	}
	defer func() { _ = probe.Close(ctx) }() //nolint:errcheck // test cleanup
	probeTx, err := probe.Begin(ctx)
	if err != nil {
		t.Fatalf("begin probe: %v", err)
	}
	_, probeErr := probeTx.Exec(ctx, `SELECT 1 FROM file_upload_grants WHERE file_id = $1 FOR UPDATE NOWAIT`, high.ID)
	if rbErr := probeTx.Rollback(ctx); rbErr != nil {
		t.Fatalf("roll back probe: %v", rbErr)
	}
	if probeErr != nil {
		t.Fatalf("higher grant was locked while the album waited on the lower one: %v", probeErr)
	}

	if err = holder.Rollback(ctx); err != nil {
		t.Fatalf("release lower grant: %v", err)
	}
	if err = <-done; err != nil {
		t.Fatalf("album after release: %v", err)
	}
}
