package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// Inert flush persistence: the per-stream claim, the per-position confirmation
// and the closed-stream marker, tested as a storage contract through the
// generated queries. Proof values are supplied by these tests only; no store
// method accepts them.

const erasureFlushMigration = "20261015000088_erasure_flush_state.sql"

var erasureFlushTables = []string{"erasure_flush_claim", "erasure_flush_confirmation", "erasure_flush_closed_stream"}

// erasureFlushStream is one (epoch, stream) with outbox rows at seq 1..n.
type erasureFlushStream struct {
	epoch   int64
	stream  []byte
	keys    [][]byte
	records [][]byte
}

// position returns the operation key and canonical bytes stored at seq.
func (s erasureFlushStream) position(seq int64) (key, record []byte) {
	return s.keys[seq-1], s.records[seq-1]
}

// erasureFlushProof is the verifier evidence one confirmation carries.
type erasureFlushProof struct {
	key        []byte
	checksum   []byte
	arrivalSeq int64
}

func erasureFlushOutbox(t *testing.T, ctx context.Context, q *db.Queries, epoch int64, n int64) erasureFlushStream {
	t.Helper()
	stream := erasureLedgerStream(t)
	s := erasureFlushStream{epoch: epoch, stream: stream[:]}
	for seq := int64(1); seq <= n; seq++ {
		key := erasureLedgerOpKey(t)
		rec := erasureLedgerSelfDelete(t, epoch, stream, seq, key, 7001, erasureLedgerLocalID(t, seq))
		params := erasureLedgerParams(t, rec)
		if err := q.InsertErasureOutboxRecord(ctx, params); err != nil {
			t.Fatalf("insert outbox seq %d: %v", seq, err)
		}
		s.keys = append(s.keys, params.OperationKey)
		s.records = append(s.records, params.Record)
	}
	return s
}

// erasureFlushMatchingProof is the evidence a verifier holding the stored bytes
// at seq reports.
func erasureFlushMatchingProof(s erasureFlushStream, seq, arrivalSeq int64) erasureFlushProof {
	key, record := s.position(seq)
	sum := sha256.Sum256(record)
	return erasureFlushProof{key: key, checksum: sum[:], arrivalSeq: arrivalSeq}
}

func erasureFlushHolder(t *testing.T) []byte {
	t.Helper()
	key := erasureLedgerOpKey(t)
	return key[:]
}

func erasureFlushGrant(ctx context.Context, q *db.Queries, s erasureFlushStream, holder []byte, position int64) (int64, error) {
	return q.GrantErasureFlushClaim(ctx, db.GrantErasureFlushClaimParams{
		Holder: holder, Epoch: s.epoch, StreamID: s.stream, Position: position,
	})
}

func erasureFlushAdvance(ctx context.Context, q *db.Queries, s erasureFlushStream, holder []byte, generation, position int64) (int64, error) {
	return q.AdvanceErasureFlushClaim(ctx, db.AdvanceErasureFlushClaimParams{
		Epoch: s.epoch, StreamID: s.stream, Holder: holder, Generation: generation, Position: position,
	})
}

func erasureFlushTakeOver(ctx context.Context, q *db.Queries, s erasureFlushStream, holder []byte, generation int64) (db.TakeOverErasureFlushClaimRow, error) {
	return q.TakeOverErasureFlushClaim(ctx, db.TakeOverErasureFlushClaimParams{
		Holder: holder, Epoch: s.epoch, StreamID: s.stream, Generation: generation,
	})
}

func erasureFlushConfirm(ctx context.Context, q *db.Queries, s erasureFlushStream, seq, generation int64, proof erasureFlushProof) (int64, error) {
	return q.ConfirmErasureFlushPosition(ctx, db.ConfirmErasureFlushPositionParams{
		Checksum:     proof.checksum,
		ArrivalSeq:   proof.arrivalSeq,
		ProofKind:    "synthetic",
		OperationKey: proof.key,
		Epoch:        s.epoch,
		StreamID:     s.stream,
		Seq:          seq,
		Generation:   generation,
	})
}

func erasureFlushMustGrant(t *testing.T, ctx context.Context, q *db.Queries, s erasureFlushStream, holder []byte, position int64) int64 {
	t.Helper()
	generation, err := erasureFlushGrant(ctx, q, s, holder, position)
	if err != nil {
		t.Fatalf("grant claim at %d: %v", position, err)
	}
	return generation
}

func erasureFlushMustAdvance(t *testing.T, ctx context.Context, q *db.Queries, s erasureFlushStream, holder []byte, generation, position int64) int64 {
	t.Helper()
	next, err := erasureFlushAdvance(ctx, q, s, holder, generation, position)
	if err != nil {
		t.Fatalf("advance claim to %d: %v", position, err)
	}
	if next <= generation {
		t.Fatalf("advance generation %d -> %d, want an increase", generation, next)
	}
	return next
}

func erasureFlushWantConfirm(t *testing.T, ctx context.Context, q *db.Queries, s erasureFlushStream, seq, generation int64, proof erasureFlushProof, want int64) {
	t.Helper()
	got, err := erasureFlushConfirm(ctx, q, s, seq, generation, proof)
	if err != nil {
		t.Fatalf("confirm seq %d at generation %d: %v", seq, generation, err)
	}
	if got != want {
		t.Fatalf("confirm seq %d at generation %d affected %d rows, want %d", seq, generation, got, want)
	}
}

func erasureFlushWantNoRow(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("%s: err %v, want refusal (no row)", what, err)
	}
}

func erasureFlushConfirmations(t *testing.T, ctx context.Context, q *db.Queries, s erasureFlushStream) []db.ErasureFlushConfirmation {
	t.Helper()
	rows, err := q.ErasureFlushStreamConfirmations(ctx, db.ErasureFlushStreamConfirmationsParams{Epoch: s.epoch, StreamID: s.stream})
	if err != nil {
		t.Fatalf("read confirmations: %v", err)
	}
	return rows
}

// erasureFlushWantStored asserts the stream's confirmations are exactly proofs,
// keyed by seq.
func erasureFlushWantStored(t *testing.T, ctx context.Context, q *db.Queries, s erasureFlushStream, proofs map[int64]erasureFlushProof) {
	t.Helper()
	rows := erasureFlushConfirmations(t, ctx, q, s)
	if len(rows) != len(proofs) {
		t.Fatalf("stream has %d confirmations, want %d: %+v", len(rows), len(proofs), rows)
	}
	for _, row := range rows {
		want, ok := proofs[row.Seq]
		if !ok || row.Epoch != s.epoch || !bytes.Equal(row.StreamID, s.stream) ||
			!bytes.Equal(row.OperationKey, want.key) || !bytes.Equal(row.Checksum, want.checksum) ||
			row.ArrivalSeq != want.arrivalSeq || row.ProofKind != "synthetic" {
			t.Fatalf("stored confirmation %+v, want %+v", row, want)
		}
	}
}

func assertErasureFlushTablesEmpty(t *testing.T, ctx context.Context, conn db.DBTX, activity string) {
	t.Helper()
	for _, table := range erasureFlushTables {
		var count int64
		// table is drawn only from the fixed flush relation list.
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("%s count after %s: %v", table, activity, err)
		}
		if count != 0 {
			t.Fatalf("%s wrote %d rows into %s", activity, count, table)
		}
	}
}

// erasureFlushAwaitLockWait returns once the backend pid is waiting on a lock.
func erasureFlushAwaitLockWait(t *testing.T, ctx context.Context, observer *pgx.Conn, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting bool
		if err := observer.QueryRow(ctx, `
			SELECT EXISTS (SELECT FROM pg_stat_activity WHERE pid = $1 AND wait_event_type = 'Lock')`,
			int32(pid)).Scan(&waiting); err != nil { // #nosec G115 -- a Postgres backend pid fits int32.
			t.Fatalf("read backend %d wait state: %v", pid, err)
		}
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d never waited on the claim row lock", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestErasureFlushSchemaIsInsertOnlyEvidenceWithoutOutboxKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)

	rows, err := conn.Query(ctx, `
		SELECT table_name || '.' || column_name || ':' || data_type
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = ANY($1)
		ORDER BY 1`, erasureFlushTables)
	if err != nil {
		t.Fatalf("read flush columns: %v", err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read flush columns: %v", err)
	}
	want := []string{
		"erasure_flush_claim.epoch:bigint",
		"erasure_flush_claim.generation:bigint",
		"erasure_flush_claim.holder:bytea",
		"erasure_flush_claim.position:bigint",
		"erasure_flush_claim.stream_id:bytea",
		"erasure_flush_closed_stream.epoch:bigint",
		"erasure_flush_closed_stream.position:bigint",
		"erasure_flush_closed_stream.stream_id:bytea",
		"erasure_flush_confirmation.arrival_seq:bigint",
		"erasure_flush_confirmation.checksum:bytea",
		"erasure_flush_confirmation.epoch:bigint",
		"erasure_flush_confirmation.operation_key:bytea",
		"erasure_flush_confirmation.proof_kind:text",
		"erasure_flush_confirmation.seq:bigint",
		"erasure_flush_confirmation.stream_id:bytea",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("flush columns = %v, want %v", got, want)
	}

	// No foreign key in either direction: a confirmed predecessor must stay
	// provable after its outbox row is removed, and no trigger rewrites a row.
	var foreignKeys, triggers int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND (conrelid = ANY($1::regclass[]) OR confrelid = ANY($1::regclass[]))`,
		erasureFlushTables).Scan(&foreignKeys); err != nil {
		t.Fatalf("read flush foreign keys: %v", err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FROM pg_trigger WHERE tgrelid = ANY($1::regclass[])`,
		erasureFlushTables).Scan(&triggers); err != nil {
		t.Fatalf("read flush triggers: %v", err)
	}
	if foreignKeys != 0 || triggers != 0 {
		t.Fatalf("flush foreign keys/triggers = %d/%d, want 0/0", foreignKeys, triggers)
	}

	// The proof kind admits synthetic proof only, at the storage boundary.
	q := db.New(conn)
	s := erasureFlushOutbox(t, ctx, q, 1, 1)
	holder := erasureFlushHolder(t)
	generation := erasureFlushMustGrant(t, ctx, q, s, holder, 1)
	proof := erasureFlushMatchingProof(s, 1, 1)
	_, err = q.ConfirmErasureFlushPosition(ctx, db.ConfirmErasureFlushPositionParams{
		Checksum: proof.checksum, ArrivalSeq: proof.arrivalSeq, ProofKind: "provider",
		OperationKey: proof.key, Epoch: s.epoch, StreamID: s.stream, Seq: 1, Generation: generation,
	})
	messageDeleteReceiptWantSQLState(t, err, "23514")
	erasureFlushWantStored(t, ctx, q, s, nil)
}

func TestErasureFlushMigrationReplaysOnDisposableSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, erasureFlushMigration)
	for _, table := range erasureFlushTables {
		if erasureLedgerPresent(t, ctx, conn, table) {
			t.Fatalf("%s exists before its migration", table)
		}
	}
	body := profilePhotoReadMigration(t, erasureFlushMigration)
	if _, err := conn.Exec(ctx, body); err != nil {
		t.Fatalf("apply %s: %v", erasureFlushMigration, err)
	}
	for _, table := range erasureFlushTables {
		if !erasureLedgerPresent(t, ctx, conn, table) {
			t.Fatalf("%s missing after its migration", table)
		}
	}
	if _, err := conn.Exec(ctx, body); err == nil {
		t.Errorf("applying %s twice succeeded, want a loud failure", erasureFlushMigration)
	}
}

// TestErasureFlushQueriesNeverRewriteEvidence reads every query source: no query
// updates or deletes a confirmation or a closed marker, deletes a claim, or
// changes a claim without raising its generation.
func TestErasureFlushQueriesNeverRewriteEvidence(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join("queries", "*.sql"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("list query sources: %v (%d files)", err, len(paths))
	}
	forbidden := regexp.MustCompile(`\b(update\s+erasure_flush_(confirmation|closed_stream)|delete\s+from\s+erasure_flush_\w+|truncate\b[^;]*erasure_flush_)`)
	claimUpdate := regexp.MustCompile(`\bupdate\s+erasure_flush_claim\b`)
	evidenceInsert := regexp.MustCompile(`\binsert\s+into\s+erasure_flush_(confirmation|closed_stream)\b`)
	claimUpdates := 0
	for _, path := range paths {
		body, err := os.ReadFile(path) // #nosec G304 -- fixed query source directory.
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for statement := range strings.SplitSeq(string(body), ";") {
			sql := strings.ToLower(strings.Join(strings.Fields(profilePhotoStripComments(statement)), " "))
			if forbidden.MatchString(sql) {
				t.Errorf("%s rewrites flush evidence: %s", path, sql)
			}
			if evidenceInsert.MatchString(sql) && strings.Contains(sql, "do update") {
				t.Errorf("%s upserts flush evidence: %s", path, sql)
			}
			if claimUpdate.MatchString(sql) {
				claimUpdates++
				if !strings.Contains(sql, "generation = claim.generation + 1") {
					t.Errorf("%s changes a claim without raising its generation: %s", path, sql)
				}
			}
		}
	}
	if claimUpdates != 2 {
		t.Fatalf("found %d claim updates, want advance and takeover", claimUpdates)
	}
}

// TestErasureFlushSerialOrderAndImmutableConfirmation is the ticket's example:
// n+1 is never claimed before n confirms, and a confirmed row never changes.
func TestErasureFlushSerialOrderAndImmutableConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := db.New(erasureLedgerConn(t, ctx))
	s := erasureFlushOutbox(t, ctx, q, 4, 2)
	holder := erasureFlushHolder(t)

	_, err := erasureFlushGrant(ctx, q, s, holder, 2)
	erasureFlushWantNoRow(t, "claim seq 2 while seq 1 is unconfirmed", err)
	generation := erasureFlushMustGrant(t, ctx, q, s, holder, 1)
	if generation != 1 {
		t.Fatalf("first grant generation = %d, want 1", generation)
	}
	_, err = erasureFlushGrant(ctx, q, s, holder, 1)
	erasureFlushWantNoRow(t, "second grant over an existing claim", err)
	_, err = erasureFlushAdvance(ctx, q, s, holder, generation, 2)
	erasureFlushWantNoRow(t, "advance to seq 2 while seq 1 is unconfirmed", err)

	// Wrong key, wrong checksum and wrong position all leave seq 1 pending.
	first := erasureFlushMatchingProof(s, 1, 10)
	wrongKey := first
	wrongKey.key = s.keys[1]
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, wrongKey, 0)
	wrongSum := first
	wrongSum.checksum = bytes.Repeat([]byte{0xab}, sha256.Size)
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, wrongSum, 0)
	erasureFlushWantConfirm(t, ctx, q, s, 2, generation, erasureFlushMatchingProof(s, 2, 11), 0)
	erasureFlushWantStored(t, ctx, q, s, nil)

	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, first, 1)
	_, err = erasureFlushAdvance(ctx, q, s, holder, generation, 1)
	erasureFlushWantNoRow(t, "advance onto confirmed seq 1", err)
	_, err = erasureFlushAdvance(ctx, q, s, erasureFlushHolder(t), generation, 2)
	erasureFlushWantNoRow(t, "advance by a holder that does not hold the claim", err)
	advanced := erasureFlushMustAdvance(t, ctx, q, s, holder, generation, 2)
	_, err = erasureFlushAdvance(ctx, q, s, holder, generation, 2)
	erasureFlushWantNoRow(t, "advance at a superseded generation", err)

	second := erasureFlushMatchingProof(s, 2, 11)
	erasureFlushWantConfirm(t, ctx, q, s, 2, advanced, second, 1)
	replay := second
	replay.arrivalSeq = 99
	erasureFlushWantConfirm(t, ctx, q, s, 2, advanced, replay, 0)
	erasureFlushWantStored(t, ctx, q, s, map[int64]erasureFlushProof{1: first, 2: second})
}

// TestErasureFlushStaleGenerationConfirmRacesTakeover interleaves a stale
// holder's confirmation, carrying different arrival evidence, with the takeover
// CAS through a row-lock barrier, in both orders.
func TestErasureFlushStaleGenerationConfirmRacesTakeover(t *testing.T) {
	t.Parallel()

	t.Run("takeover first", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		dsn := pgtest.DSN(t)
		observer := erasureFlushConnect(t, ctx, dsn)
		stale := erasureFlushConnect(t, ctx, dsn)
		q := db.New(observer)
		s := erasureFlushOutbox(t, ctx, q, 2, 1)
		oldHolder, newHolder := erasureFlushHolder(t), erasureFlushHolder(t)
		generation := erasureFlushMustGrant(t, ctx, q, s, oldHolder, 1)

		takeover, err := erasureFlushConnect(t, ctx, dsn).Begin(ctx)
		if err != nil {
			t.Fatalf("begin takeover: %v", err)
		}
		taken, err := erasureFlushTakeOver(ctx, db.New(takeover), s, newHolder, generation)
		if err != nil {
			t.Fatalf("takeover: %v", err)
		}

		staleProof := erasureFlushMatchingProof(s, 1, 500)
		done := make(chan struct{})
		var staleRows int64
		var staleErr error
		go func() {
			defer close(done)
			staleRows, staleErr = erasureFlushConfirm(ctx, db.New(stale), s, 1, generation, staleProof)
		}()
		erasureFlushAwaitLockWait(t, ctx, observer, stale.PgConn().PID())
		if err := takeover.Commit(ctx); err != nil {
			t.Fatalf("commit takeover: %v", err)
		}
		<-done
		if staleErr != nil || staleRows != 0 {
			t.Fatalf("stale confirm after takeover = %d rows, err %v; want 0", staleRows, staleErr)
		}
		erasureFlushWantStored(t, ctx, q, s, nil)

		if taken.Position != 1 || taken.Generation != generation+1 {
			t.Fatalf("takeover = %+v, want position 1 at generation %d", taken, generation+1)
		}
		current := erasureFlushMatchingProof(s, 1, 501)
		erasureFlushWantConfirm(t, ctx, q, s, 1, taken.Generation, current, 1)
		erasureFlushWantConfirm(t, ctx, q, s, 1, generation, staleProof, 0)
		erasureFlushWantStored(t, ctx, q, s, map[int64]erasureFlushProof{1: current})
	})

	t.Run("confirm first", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		dsn := pgtest.DSN(t)
		observer := erasureFlushConnect(t, ctx, dsn)
		taker := erasureFlushConnect(t, ctx, dsn)
		q := db.New(observer)
		s := erasureFlushOutbox(t, ctx, q, 2, 1)
		oldHolder, newHolder := erasureFlushHolder(t), erasureFlushHolder(t)
		generation := erasureFlushMustGrant(t, ctx, q, s, oldHolder, 1)

		confirm, err := erasureFlushConnect(t, ctx, dsn).Begin(ctx)
		if err != nil {
			t.Fatalf("begin confirm: %v", err)
		}
		first := erasureFlushMatchingProof(s, 1, 700)
		if rows, err := erasureFlushConfirm(ctx, db.New(confirm), s, 1, generation, first); err != nil || rows != 1 {
			t.Fatalf("confirm under the claim lock = %d rows, err %v; want 1", rows, err)
		}

		done := make(chan struct{})
		var taken db.TakeOverErasureFlushClaimRow
		var takeErr error
		go func() {
			defer close(done)
			taken, takeErr = erasureFlushTakeOver(ctx, db.New(taker), s, newHolder, generation)
		}()
		erasureFlushAwaitLockWait(t, ctx, observer, taker.PgConn().PID())
		if err := confirm.Commit(ctx); err != nil {
			t.Fatalf("commit confirm: %v", err)
		}
		<-done
		if takeErr != nil || taken.Generation != generation+1 || taken.Position != 1 {
			t.Fatalf("takeover after confirm = %+v, err %v", taken, takeErr)
		}

		different := erasureFlushMatchingProof(s, 1, 701)
		erasureFlushWantConfirm(t, ctx, q, s, 1, generation, different, 0)
		erasureFlushWantConfirm(t, ctx, q, s, 1, taken.Generation, different, 0)
		erasureFlushWantStored(t, ctx, q, s, map[int64]erasureFlushProof{1: first})
	})
}

func erasureFlushConnect(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Logf("close connection: %v", err)
		}
	})
	return conn
}

// TestErasureFlushPredecessorSurvivesOutboxRemoval: a confirmed predecessor is
// proven by its confirmation row, never by its outbox row, and "first" is seq 1.
func TestErasureFlushPredecessorSurvivesOutboxRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	q := db.New(conn)
	removeOutbox := func(s erasureFlushStream, seq int64) {
		t.Helper()
		if _, err := conn.Exec(ctx, `DELETE FROM erasure_outbox WHERE epoch = $1 AND stream_id = $2 AND seq = $3`,
			s.epoch, s.stream, seq); err != nil {
			t.Fatalf("remove outbox seq %d: %v", seq, err)
		}
	}

	confirmed := erasureFlushOutbox(t, ctx, q, 3, 2)
	holder := erasureFlushHolder(t)
	generation := erasureFlushMustGrant(t, ctx, q, confirmed, holder, 1)
	first := erasureFlushMatchingProof(confirmed, 1, 1)
	erasureFlushWantConfirm(t, ctx, q, confirmed, 1, generation, first, 1)
	removeOutbox(confirmed, 1)
	generation = erasureFlushMustAdvance(t, ctx, q, confirmed, holder, generation, 2)
	second := erasureFlushMatchingProof(confirmed, 2, 2)
	erasureFlushWantConfirm(t, ctx, q, confirmed, 2, generation, second, 1)
	erasureFlushWantStored(t, ctx, q, confirmed, map[int64]erasureFlushProof{1: first, 2: second})

	unconfirmed := erasureFlushOutbox(t, ctx, q, 3, 2)
	removeOutbox(unconfirmed, 1)
	_, err := erasureFlushGrant(ctx, q, unconfirmed, holder, 2)
	erasureFlushWantNoRow(t, "claim seq 2 after unconfirmed seq 1 was removed", err)
	_, err = erasureFlushGrant(ctx, q, unconfirmed, holder, 1)
	erasureFlushWantNoRow(t, "claim a removed seq 1", err)
	// A claim row the grant would never have written proves the confirmation
	// checks the predecessor itself rather than trusting the claim.
	if _, err := conn.Exec(ctx, `
		INSERT INTO erasure_flush_claim (epoch, stream_id, position, holder, generation)
		VALUES ($1, $2, 2, $3, 1)`, unconfirmed.epoch, unconfirmed.stream, holder); err != nil {
		t.Fatalf("plant claim at seq 2: %v", err)
	}
	erasureFlushWantConfirm(t, ctx, q, unconfirmed, 2, 1, erasureFlushMatchingProof(unconfirmed, 2, 1), 0)
	erasureFlushWantStored(t, ctx, q, unconfirmed, nil)
}

// TestErasureFlushChecksumIsComputedOverCurrentRecord: bytes altered between
// claim and confirm refuse the checksum of the bytes originally read.
func TestErasureFlushChecksumIsComputedOverCurrentRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	q := db.New(conn)
	s := erasureFlushOutbox(t, ctx, q, 5, 1)
	holder := erasureFlushHolder(t)
	generation := erasureFlushMustGrant(t, ctx, q, s, holder, 1)
	original := erasureFlushMatchingProof(s, 1, 1)

	if _, err := conn.Exec(ctx, `
		UPDATE erasure_outbox SET record = record || '\x00'::bytea
		WHERE epoch = $1 AND stream_id = $2 AND seq = 1`, s.epoch, s.stream); err != nil {
		t.Fatalf("alter stored record: %v", err)
	}
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, original, 0)
	erasureFlushWantStored(t, ctx, q, s, nil)

	_, record := s.position(1)
	altered := sha256.Sum256(append(slices.Clone(record), 0))
	current := original
	current.checksum = altered[:]
	erasureFlushWantConfirm(t, ctx, q, s, 1, generation, current, 1)
	erasureFlushWantStored(t, ctx, q, s, map[int64]erasureFlushProof{1: current})
}

// TestErasureFlushClosedStreamRefusesWhileOthersProgress: a closed stream
// refuses grant, advance, takeover and confirmation; a repeated close keeps the
// first marker; another stream progresses.
func TestErasureFlushClosedStreamRefusesWhileOthersProgress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	q := db.New(erasureLedgerConn(t, ctx))
	closed := erasureFlushOutbox(t, ctx, q, 6, 2)
	open := erasureFlushOutbox(t, ctx, q, 6, 2)
	neverClaimed := erasureFlushOutbox(t, ctx, q, 6, 1)
	holder := erasureFlushHolder(t)

	generation := erasureFlushMustGrant(t, ctx, q, closed, holder, 1)
	first := erasureFlushMatchingProof(closed, 1, 1)
	erasureFlushWantConfirm(t, ctx, q, closed, 1, generation, first, 1)
	generation = erasureFlushMustAdvance(t, ctx, q, closed, holder, generation, 2)

	closeStream := func(s erasureFlushStream, position, want int64) {
		t.Helper()
		got, err := q.CloseErasureFlushStream(ctx, db.CloseErasureFlushStreamParams{
			Epoch: s.epoch, StreamID: s.stream, Position: position,
		})
		if err != nil || got != want {
			t.Fatalf("close at %d = %d rows, err %v; want %d", position, got, err, want)
		}
	}
	closeStream(closed, 2, 1)
	closeStream(closed, 1, 0)
	closeStream(neverClaimed, 1, 1)
	marker, err := q.ErasureFlushClosedStreamByStream(ctx, db.ErasureFlushClosedStreamByStreamParams{
		Epoch: closed.epoch, StreamID: closed.stream,
	})
	if err != nil || marker.Position != 2 {
		t.Fatalf("closed marker = %+v, err %v; want the first colliding position 2", marker, err)
	}

	erasureFlushWantConfirm(t, ctx, q, closed, 2, generation, erasureFlushMatchingProof(closed, 2, 2), 0)
	_, err = erasureFlushTakeOver(ctx, q, closed, erasureFlushHolder(t), generation)
	erasureFlushWantNoRow(t, "takeover on a closed stream", err)
	_, err = erasureFlushAdvance(ctx, q, closed, holder, generation, 2)
	erasureFlushWantNoRow(t, "advance on a closed stream", err)
	_, err = erasureFlushGrant(ctx, q, neverClaimed, holder, 1)
	erasureFlushWantNoRow(t, "grant on a closed stream", err)
	erasureFlushWantStored(t, ctx, q, closed, map[int64]erasureFlushProof{1: first})
	claim, err := q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: closed.epoch, StreamID: closed.stream})
	if err != nil || claim.Position != 2 || claim.Generation != generation || !bytes.Equal(claim.Holder, holder) {
		t.Fatalf("closed stream claim = %+v, err %v; want unchanged at 2, generation %d", claim, err, generation)
	}

	openGeneration := erasureFlushMustGrant(t, ctx, q, open, holder, 1)
	openFirst := erasureFlushMatchingProof(open, 1, 1)
	erasureFlushWantConfirm(t, ctx, q, open, 1, openGeneration, openFirst, 1)
	openGeneration = erasureFlushMustAdvance(t, ctx, q, open, holder, openGeneration, 2)
	openSecond := erasureFlushMatchingProof(open, 2, 2)
	erasureFlushWantConfirm(t, ctx, q, open, 2, openGeneration, openSecond, 1)
	erasureFlushWantStored(t, ctx, q, open, map[int64]erasureFlushProof{1: openFirst, 2: openSecond})
}
