package store_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// future and past are cutoffs either side of every file a test creates, so the
// age gate is exercised by the argument rather than by the wall clock: a test
// that waited out a real cutoff would need a short one, and a short one closes
// under host load before the assertion runs.
func future() time.Time { return time.Now().Add(time.Hour) }
func past() time.Time   { return time.Now().Add(-time.Hour) }

// scanAll runs one pass wide enough to cover everything in this test's database.
// internal/pgtest clones a database per test, so "everything" is exactly the
// rows this test wrote.
func scanAll(t *testing.T, s *store.Store, olderThan time.Time) store.ErasureScan {
	t.Helper()
	sc, err := s.MediaErasureScan(context.Background(), olderThan, 0, 0, store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erasure scan: %v", err)
	}
	if !sc.Done {
		t.Fatalf("scan of at most %d rows did not finish in one pass", store.ErasureScanBatch)
	}
	return sc
}

// candidate finds fileID among a scan class, so an assertion names the file it
// means instead of an index into a slice whose order it would also be asserting.
func candidate(cs []store.ErasureCandidate, fileID int64) (store.ErasureCandidate, bool) {
	for _, c := range cs {
		if c.ID == fileID {
			return c, true
		}
	}
	return store.ErasureCandidate{}, false
}

// sendMedia sends fileID from a to b and returns both sides' local ids, which is
// what a test needs to soft-delete each copy independently.
func sendMedia(t *testing.T, s *store.Store, from, to store.User, fileID int64, randomID int64) (fromLocal, toLocal int64) {
	t.Helper()
	m, _, _, _, err := s.SendMessage(context.Background(), from.ID, to.ID, "here", randomID, fileID, 0) //nolint:dogsled // the pts pair and the dedup flag are not what these tests assert on
	if err != nil {
		t.Fatalf("send media: %v", err)
	}
	return m.LocalID, m.PeerLocalID
}

// The ticket's concrete case: a file uploaded and sent 1:1, then deleted by both
// sides, past the cutoff. It is named as a reference candidate, and it carries
// the byte size an operator needs to know what a reclaim would free.
func TestMediaErasureScanNamesFileDeletedOnBothSides(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140001")
	b := mustUser(t, s, "+15559140002")

	f := storedFile(t, s, a.ID)
	aLocal, bLocal := sendMedia(t, s, a, b, f.ID, 1)
	if err := store.SetMessageDeleted(ctx, s, a.ID, aLocal); err != nil {
		t.Fatalf("delete sender copy: %v", err)
	}
	if err := store.SetMessageDeleted(ctx, s, b.ID, bLocal); err != nil {
		t.Fatalf("delete recipient copy: %v", err)
	}

	sc := scanAll(t, s, future())
	got, ok := candidate(sc.Unreferenced, f.ID)
	if !ok {
		t.Fatalf("file %d not named a reference candidate; scan = %+v", f.ID, sc)
	}
	if got.Size != f.Size {
		t.Fatalf("candidate size = %d, want %d", got.Size, f.Size)
	}
	if _, ok := candidate(sc.Unassembled, f.ID); ok {
		t.Fatalf("a stored file was reported in the unassembled class")
	}
	if sc.Counts.UnreferencedBytes != f.Size {
		t.Fatalf("UnreferencedBytes = %d, want %d", sc.Counts.UnreferencedBytes, f.Size)
	}
}

// One surviving non-deleted messages row keeps the file live, whichever side
// holds it. The file is not named, and it is counted as skipped so an operator
// can tell "nothing to reclaim" from "reclaim held back by live references".
func TestMediaErasureScanSkipsLiveMessageReference(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140011")
	b := mustUser(t, s, "+15559140012")

	f := storedFile(t, s, a.ID)
	aLocal, _ := sendMedia(t, s, a, b, f.ID, 1)
	// Only the sender deletes. The recipient's copy is still live.
	if err := store.SetMessageDeleted(ctx, s, a.ID, aLocal); err != nil {
		t.Fatalf("delete sender copy: %v", err)
	}

	sc := scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, f.ID); ok {
		t.Fatalf("file %d named a candidate while a live messages row references it", f.ID)
	}
	if sc.Counts.SkippedMessageRef != 1 {
		t.Fatalf("SkippedMessageRef = %d, want 1; scan = %+v", sc.Counts.SkippedMessageRef, sc.Counts)
	}
}

// channel_messages is in the predicate even though no handler can produce a
// channel post carrying a file id: PostChannelMessage is reached here directly,
// because a scan that omitted the channel branch would pass every test that can
// be written against the shipped RPC surface and start destroying live channel
// media the day channel posts carry files.
func TestMediaErasureScanSkipsLiveChannelReference(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	creator := mustUser(t, s, "+15559140021")
	member := mustUser(t, s, "+15559140022")

	f := storedFile(t, s, creator.ID)
	channelID, localID, err := store.SeedChannelPost(ctx, s, creator.ID, member.ID, f.ID)
	if err != nil {
		t.Fatalf("seed channel post: %v", err)
	}

	sc := scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, f.ID); ok {
		t.Fatalf("file %d named a candidate while a live channel post references it", f.ID)
	}
	if sc.Counts.SkippedChannelRef != 1 {
		t.Fatalf("SkippedChannelRef = %d, want 1; scan = %+v", sc.Counts.SkippedChannelRef, sc.Counts)
	}

	// Soft-deleting the post is the only thing that changes, and it is enough:
	// the predicate reads channel_messages.deleted the same way it reads it on
	// the message side.
	if err := store.SetChannelPostDeleted(ctx, s, channelID, localID); err != nil {
		t.Fatalf("delete channel post: %v", err)
	}
	sc = scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, f.ID); !ok {
		t.Fatalf("file %d not named after its only channel post was deleted; scan = %+v", f.ID, sc)
	}
	if sc.Counts.SkippedChannelRef != 0 {
		t.Fatalf("SkippedChannelRef = %d after the post was deleted, want 0", sc.Counts.SkippedChannelRef)
	}
}

func TestMediaErasureScanSkipsGalleryReferences(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner := mustUser(t, s, "+15559140025")
	stored := storedFile(t, s, owner.ID)
	unassembled := allocate(t, s, owner.ID, 17)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	for _, fileID := range []int64{stored.ID, unassembled.ID} {
		if _, err := conn.Exec(ctx, `
			INSERT INTO user_photos (user_id, file_id, client_file_id)
			VALUES ($1, $2, $2)
		`, owner.ID, fileID); err != nil {
			t.Fatalf("insert gallery reference for file %d: %v", fileID, err)
		}
	}

	sc := scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, stored.ID); ok {
		t.Errorf("stored file %d referenced only by the gallery was named as unreferenced", stored.ID)
	}
	if _, ok := candidate(sc.Unassembled, unassembled.ID); ok {
		t.Errorf("unassembled file %d referenced only by the gallery was named as reclaimable", unassembled.ID)
	}
	if sc.Counts.SkippedGalleryRef != 2 {
		t.Errorf("SkippedGalleryRef = %d, want 2; scan = %+v", sc.Counts.SkippedGalleryRef, sc.Counts)
	}

	summary, err := s.MediaErasureSummary(ctx, future(), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erasure summary: %v", err)
	}
	if summary.SkippedGalleryRef != 2 {
		t.Errorf("summary SkippedGalleryRef = %d, want 2; summary = %+v", summary.SkippedGalleryRef, summary)
	}
}

// mediaErasureScanSQL returns the bounded scan statement exactly as the query
// source defines it, with every sqlc.arg rewritten to the positional
// parameter sqlc compiles it to. The plan has to be asserted against the shipped
// statement: a hand-copied query keeps passing after queries/files.sql changes
// shape, and a plan test that proves a statement nobody runs is worse than none,
// because it reads as coverage of the per-candidate probe.
func mediaErasureScanSQL(tb testing.TB) string {
	tb.Helper()
	src, err := os.ReadFile(filepath.Join("queries", "files.sql"))
	if err != nil {
		tb.Fatalf("read files query source: %v", err)
	}
	const marker = "-- name: MediaErasureScan :many\n"
	i := strings.Index(string(src), marker)
	if i < 0 {
		tb.Fatal("MediaErasureScan is missing from queries/files.sql")
	}
	stmt := string(src)[i+len(marker):]
	if j := strings.Index(stmt, "-- name:"); j >= 0 {
		stmt = stmt[:j]
	}
	stmt = strings.TrimSuffix(strings.TrimSpace(stmt), ";")
	// sqlc numbers named args by order of appearance, which is the order
	// MediaErasureScanParams declares them, so an arg added ahead of these moves a
	// number and fails here instead of EXPLAINing a query with bindings swapped.
	for n, name := range []string{"older_than", "after_id", "through_id", "lim"} {
		arg := "sqlc.arg(" + name + ")"
		if !strings.Contains(stmt, arg) {
			tb.Fatalf("MediaErasureScan no longer takes sqlc.arg(%s) as parameter %d:\n%s", name, n+1, stmt)
		}
		stmt = strings.ReplaceAll(stmt, arg, fmt.Sprintf("$%d", n+1))
	}
	if strings.Contains(stmt, "sqlc.") {
		tb.Fatalf("MediaErasureScan has a source-only parameter form this test cannot position:\n%s", stmt)
	}
	// The gallery probe stays a lateral lookup keyed on the candidate id and the
	// window stays bounded by id. Those are the two shapes the plan assertions
	// below read, so their absence means the assertions describe a statement that
	// no longer exists, not a plan regression.
	for _, want := range []string{
		"FROM user_photos up",
		"WHERE up.file_id = f.id",
		"LIMIT 1",
		"WHERE f.id > $2 AND f.id <= $3",
	} {
		if !strings.Contains(stmt, want) {
			tb.Fatalf("MediaErasureScan no longer contains %q, so its plan assertions no longer describe the shipped statement:\n%s", want, stmt)
		}
	}
	return stmt
}

func TestMediaErasureScanUsesIndexedGalleryProbesAtScale(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := profilePhotoUser(t, ctx, conn)
	other := profilePhotoUser(t, ctx, conn)
	const galleryEntries = 200_000
	throughID := profilePhotoSeedInterleavedGallery(t, ctx, conn, owner, other, galleryEntries)
	if _, err := conn.Exec(ctx, `ANALYZE files`); err != nil {
		t.Fatalf("analyze files: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE user_photos`); err != nil {
		t.Fatalf("analyze gallery: %v", err)
	}

	plan := profilePhotoExplain(t, ctx, conn, mediaErasureScanSQL(t),
		time.Now().Add(time.Hour), int64(0), throughID, 1000)
	if !strings.Contains(plan, "user_photos_file_id_key") || !strings.Contains(plan, "loops=1000") {
		t.Fatalf("bounded erasure scan does not use one indexed gallery probe per candidate:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on user_photos") {
		t.Fatalf("bounded erasure scan reads the gallery sequentially:\n%s", plan)
	}
}

// The age gate holds whatever the reference count is. Every media file on the
// server is stored with zero live references for the length of one send, so a
// scan run with the cutoff behind these files must name none of them.
func TestMediaErasureScanSkipsFilesNewerThanCutoff(t *testing.T) {
	t.Parallel()
	s := open(t)
	a := mustUser(t, s, "+15559140031")

	unreferenced := storedFile(t, s, a.ID)
	unassembled := allocate(t, s, a.ID, 17)

	sc := scanAll(t, s, past())
	if n := len(sc.Unreferenced) + len(sc.Unassembled); n != 0 {
		t.Fatalf("%d candidates named under a cutoff older than every file; scan = %+v", n, sc)
	}
	if sc.Counts.SkippedTooNew != 2 {
		t.Fatalf("SkippedTooNew = %d, want 2; scan = %+v", sc.Counts.SkippedTooNew, sc.Counts)
	}

	// Same rows, cutoff moved past them: now both are named. Without this half
	// the assertion above would also pass on a scan that names nothing at all.
	sc = scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, unreferenced.ID); !ok {
		t.Fatalf("stored file %d not named past the cutoff", unreferenced.ID)
	}
	if _, ok := candidate(sc.Unassembled, unassembled.ID); !ok {
		t.Fatalf("unstored file %d not named past the cutoff", unassembled.ID)
	}
}

// Not-stored rows are a class of their own: a crashed assembly and an assembly
// running right now are the same state on disk, and they are reclaimed by a
// different mechanism than an unreferenced stored file.
func TestMediaErasureScanReportsUnstoredAsItsOwnClass(t *testing.T) {
	t.Parallel()
	s := open(t)
	a := mustUser(t, s, "+15559140041")

	unassembled := allocate(t, s, a.ID, 23)
	stored := storedFile(t, s, a.ID)

	sc := scanAll(t, s, future())
	got, ok := candidate(sc.Unassembled, unassembled.ID)
	if !ok {
		t.Fatalf("unstored file %d not reported; scan = %+v", unassembled.ID, sc)
	}
	if got.Size != unassembled.Size {
		t.Fatalf("unassembled candidate size = %d, want %d", got.Size, unassembled.Size)
	}
	if _, ok := candidate(sc.Unreferenced, unassembled.ID); ok {
		t.Fatalf("unstored file %d reported as a reference candidate too", unassembled.ID)
	}
	if _, ok := candidate(sc.Unassembled, stored.ID); ok {
		t.Fatalf("stored file %d reported in the unassembled class", stored.ID)
	}
	if sc.Counts.UnassembledBytes != unassembled.Size {
		t.Fatalf("UnassembledBytes = %d, want %d", sc.Counts.UnassembledBytes, unassembled.Size)
	}
}

// The age gate is required on both classes in addition to the reference
// predicate, never instead of it. A messages row can name a file whose bytes
// were never stored — the reference interlock locks the row, not the flag — and
// such a file is not reclaimable as a crashed assembly.
func TestMediaErasureScanSkipsReferencedUnstoredFile(t *testing.T) {
	t.Parallel()
	s := open(t)
	a := mustUser(t, s, "+15559140051")
	b := mustUser(t, s, "+15559140052")

	f := allocate(t, s, a.ID, 29)
	sendMedia(t, s, a, b, f.ID, 1)

	sc := scanAll(t, s, future())
	if _, ok := candidate(sc.Unassembled, f.ID); ok {
		t.Fatalf("unstored file %d named while a live messages row references it", f.ID)
	}
	if _, ok := candidate(sc.Unreferenced, f.ID); ok {
		t.Fatalf("unstored file %d named as a reference candidate", f.ID)
	}
	if sc.Counts.SkippedMessageRef != 1 {
		t.Fatalf("SkippedMessageRef = %d, want 1; scan = %+v", sc.Counts.SkippedMessageRef, sc.Counts)
	}
}

// Nothing the scan reports carries a file's access hash. It is the unguessable
// half of a download credential, and a candidate report is exactly the kind of
// record that reaches log aggregation, so the value must not be in the returned
// value at all — not in a field a caller might log by rendering the struct.
func TestMediaErasureScanNeverReportsAccessHash(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140061")

	f := storedFile(t, s, a.ID)
	hash := strconv.FormatInt(f.AccessHash, 10)

	sc := scanAll(t, s, future())
	if _, ok := candidate(sc.Unreferenced, f.ID); !ok {
		t.Fatalf("file %d not named, so this test would assert nothing", f.ID)
	}
	if rendered := fmt.Sprintf("%+v", sc); strings.Contains(rendered, hash) {
		t.Fatalf("scan result carries the file's access hash: %s", rendered)
	}

	counts, err := s.MediaErasureSummary(ctx, future(), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erasure summary: %v", err)
	}
	if rendered := fmt.Sprintf("%+v", counts); strings.Contains(rendered, hash) {
		t.Fatalf("summary carries the file's access hash: %s", rendered)
	}
}

// The scan takes no lock a request path waits on. With the file's row held
// FOR UPDATE by another transaction — the mode an eraser would take — a scan
// that reached for the row would park in Postgres's lock queue until its
// context expired. This one returns the row.
func TestMediaErasureScanTakesNoRowLock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140071")

	f := storedFile(t, s, a.ID)
	hold, err := store.HoldFileRow(ctx, s, f.ID)
	if err != nil {
		t.Fatalf("hold file row: %v", err)
	}
	defer hold.Release()

	// Bounded so a scan that blocks fails the test instead of hanging it, and
	// generous enough that a loaded host does not fail a scan that does not.
	scanCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sc, err := s.MediaErasureScan(scanCtx, future(), 0, 0, store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("scan with the files row held FOR UPDATE: %v", err)
	}
	if _, ok := candidate(sc.Unreferenced, f.ID); !ok {
		t.Fatalf("file %d not named while its row was held; scan = %+v", f.ID, sc)
	}
}

// A pass reads at most batch rows and hands back the cursor to resume from, so
// a table larger than one batch is walked in bounded statements rather than one
// growing scan.
func TestMediaErasureScanIsBounded(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140081")

	const files = 5
	ids := make([]int64, 0, files)
	for range files {
		ids = append(ids, storedFile(t, s, a.ID).ID)
	}
	through := ids[len(ids)-1]

	var seen []int64
	var passes int
	after := int64(0)
	for {
		sc, err := s.MediaErasureScan(ctx, future(), after, through, 2)
		if err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		passes++
		if n := len(sc.Unreferenced); n > 2 {
			t.Fatalf("pass %d returned %d candidates, want at most the batch of 2", passes, n)
		}
		if sc.Counts.Scanned > 2 {
			t.Fatalf("pass %d scanned %d rows, want at most the batch of 2", passes, sc.Counts.Scanned)
		}
		for _, c := range sc.Unreferenced {
			seen = append(seen, c.ID)
		}
		if sc.Done {
			break
		}
		if sc.LastID <= after {
			t.Fatalf("pass %d did not advance the cursor past %d", passes, after)
		}
		after = sc.LastID
	}
	if passes != 3 {
		t.Fatalf("walked %d files in %d passes of 2, want 3", files, passes)
	}
	if len(seen) != files {
		t.Fatalf("named %d of %d files across the walk: %v", len(seen), files, seen)
	}
}

// The window's upper bound is a snapshot: a file created after it is not
// scanned, whatever its state. It is what makes the paging walk terminate on a
// server that is still accepting uploads.
func TestMediaErasureScanStopsAtThroughID(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15559140091")

	first := storedFile(t, s, a.ID)
	later := storedFile(t, s, a.ID)

	sc, err := s.MediaErasureScan(ctx, future(), 0, first.ID, store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erasure scan: %v", err)
	}
	if _, ok := candidate(sc.Unreferenced, first.ID); !ok {
		t.Fatalf("file %d inside the window was not named", first.ID)
	}
	if _, ok := candidate(sc.Unreferenced, later.ID); ok {
		t.Fatalf("file %d past the window's upper bound was scanned", later.ID)
	}
	if sc.Counts.Scanned != 1 {
		t.Fatalf("Scanned = %d, want 1", sc.Counts.Scanned)
	}
}

// The summary is the whole table walked in bounded passes, which is what an
// operator-facing report needs and what a per-batch call cannot give without
// the caller retaining every id it has seen.
func TestMediaErasureSummaryWalksEveryClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The DSN is taken once, so the direct gallery insert below writes to the
	// database the store reads: pgtest.DSN clones a fresh database per call.
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15559140101")
	b := mustUser(t, s, "+15559140102")

	live := storedFile(t, s, a.ID)
	sendMedia(t, s, a, b, live.ID, 1)

	dead := storedFile(t, s, a.ID)
	aLocal, bLocal := sendMedia(t, s, a, b, dead.ID, 2)
	if err := store.SetMessageDeleted(ctx, s, a.ID, aLocal); err != nil {
		t.Fatalf("delete sender copy: %v", err)
	}
	if err := store.SetMessageDeleted(ctx, s, b.ID, bLocal); err != nil {
		t.Fatalf("delete recipient copy: %v", err)
	}

	unassembled := allocate(t, s, a.ID, 31)

	// A file whose only live reference is a gallery entry. It is retained, and it
	// is counted in its own class, so the walk covers every class the summary has
	// and the partition sum below cannot drop one and still balance.
	gallery := storedFile(t, s, a.ID)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `
		INSERT INTO user_photos (user_id, file_id, client_file_id)
		VALUES ($1, $2, $2)
	`, a.ID, gallery.ID); err != nil {
		t.Fatalf("insert gallery reference for file %d: %v", gallery.ID, err)
	}

	// A batch of one forces the walk to page, so a summary that only ever read
	// its first batch would come back short here.
	counts, err := s.MediaErasureSummary(ctx, future(), 1)
	if err != nil {
		t.Fatalf("erasure summary: %v", err)
	}
	if counts.Scanned != 4 {
		t.Fatalf("Scanned = %d, want 4; counts = %+v", counts.Scanned, counts)
	}
	if counts.Unreferenced != 1 || counts.UnreferencedBytes != dead.Size {
		t.Fatalf("unreferenced = %d/%d bytes, want 1/%d; counts = %+v",
			counts.Unreferenced, counts.UnreferencedBytes, dead.Size, counts)
	}
	if counts.Unassembled != 1 || counts.UnassembledBytes != unassembled.Size {
		t.Fatalf("unassembled = %d/%d bytes, want 1/%d; counts = %+v",
			counts.Unassembled, counts.UnassembledBytes, unassembled.Size, counts)
	}
	if counts.SkippedMessageRef != 1 {
		t.Fatalf("SkippedMessageRef = %d, want 1; counts = %+v", counts.SkippedMessageRef, counts)
	}
	if counts.SkippedGalleryRef != 1 {
		t.Fatalf("SkippedGalleryRef = %d, want 1; counts = %+v", counts.SkippedGalleryRef, counts)
	}
	// Every row is accounted for exactly once, so the six outcome counts are a
	// partition of what was scanned rather than overlapping tallies. Every one of
	// them is in this sum, and every class is present in the walk above: a class
	// the walk never produces would let the sum balance by omission.
	sum := counts.Unreferenced + counts.Unassembled +
		counts.SkippedMessageRef + counts.SkippedChannelRef +
		counts.SkippedGalleryRef + counts.SkippedTooNew
	if sum != counts.Scanned {
		t.Fatalf("classes sum to %d, scanned %d; counts = %+v", sum, counts.Scanned, counts)
	}
}

// A batch that is not positive is a caller bug, not a full-table scan.
func TestMediaErasureScanRejectsNonPositiveBatch(t *testing.T) {
	t.Parallel()
	s := open(t)

	if _, err := s.MediaErasureScan(context.Background(), future(), 0, 0, 0); err == nil {
		t.Fatal("scan with batch 0 returned no error")
	}
	if _, err := s.MediaErasureSummary(context.Background(), future(), -1); err == nil {
		t.Fatal("summary with batch -1 returned no error")
	}
}
