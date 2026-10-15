package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/erasureledger/synthledger"
	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// Serial claim, flush and confirm composed over committed capture and the
// synthetic provider. The composition lives in this test file only: proof
// values come from the synthetic proof reader, and no store method accepts
// them. Every database write is one short autocommit statement, so no
// transaction or lock is open while the provider is called.

const erasureFlushComposeTimeout = 60 * time.Second

// errErasureFlushVerifierUnavailable is the injected verifier outage.
var errErasureFlushVerifierUnavailable = errors.New("verifier unavailable")

type erasureFlushOutcome uint8

const (
	erasureFlushPending erasureFlushOutcome = iota
	erasureFlushConfirmed
	erasureFlushClosed
)

func (o erasureFlushOutcome) String() string {
	switch o {
	case erasureFlushPending:
		return "pending"
	case erasureFlushConfirmed:
		return "confirmed"
	case erasureFlushClosed:
		return "closed"
	}
	return fmt.Sprintf("outcome(%d)", uint8(o))
}

// erasureFlushCreate is one create a worker handed the provider.
type erasureFlushCreate struct {
	seq    int64
	holder []byte
	body   []byte
	answer synthledger.Answer
	err    error
}

// erasureFlushRecorder is the shared log of every worker create.
type erasureFlushRecorder struct {
	mu      sync.Mutex
	creates []erasureFlushCreate
}

func (r *erasureFlushRecorder) add(c erasureFlushCreate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.creates = append(r.creates, c)
}

func (r *erasureFlushRecorder) all() []erasureFlushCreate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.creates)
}

// erasureFlushWorker is one serial flusher on one (epoch, stream): it claims a
// position, hands the provider the stored outbox bytes, reads the verifier, and
// confirms only on the verifier's evidence.
type erasureFlushWorker struct {
	conn    db.DBTX
	q       *db.Queries
	epoch   int64
	stream  erasureledger.StreamID
	holder  []byte
	flusher synthledger.Flusher
	proof   func(synthledger.Position) (synthledger.Proof, error)
	// beforeIO runs immediately before every provider call.
	beforeIO func(call string, pos synthledger.Position) error
	rec      *erasureFlushRecorder
}

func newErasureFlushWorker(t *testing.T, conn db.DBTX, p *synthledger.Provider, epoch int64, stream erasureledger.StreamID, rec *erasureFlushRecorder) *erasureFlushWorker {
	t.Helper()
	return &erasureFlushWorker{
		conn: conn, q: db.New(conn), epoch: epoch, stream: stream,
		holder:  erasureFlushHolder(t),
		flusher: p.NewFlusher(epoch, stream),
		proof:   p.ProofReader().Proof,
		rec:     rec,
	}
}

func (w *erasureFlushWorker) grant(ctx context.Context, seq int64) (int64, error) {
	return w.q.GrantErasureFlushClaim(ctx, db.GrantErasureFlushClaimParams{
		Holder: w.holder, Epoch: w.epoch, StreamID: w.stream[:], Position: seq,
	})
}

func (w *erasureFlushWorker) advance(ctx context.Context, generation, seq int64) (int64, error) {
	return w.q.AdvanceErasureFlushClaim(ctx, db.AdvanceErasureFlushClaimParams{
		Epoch: w.epoch, StreamID: w.stream[:], Holder: w.holder, Generation: generation, Position: seq,
	})
}

func (w *erasureFlushWorker) takeOver(ctx context.Context, generation int64) (db.TakeOverErasureFlushClaimRow, error) {
	return w.q.TakeOverErasureFlushClaim(ctx, db.TakeOverErasureFlushClaimParams{
		Holder: w.holder, Epoch: w.epoch, StreamID: w.stream[:], Generation: generation,
	})
}

func (w *erasureFlushWorker) io(call string, pos synthledger.Position) error {
	if w.beforeIO == nil {
		return nil
	}
	return w.beforeIO(call, pos)
}

// flush runs one position under a committed claim at generation. The create
// answer is never proof: only a verifier read naming the stored key can confirm,
// and the confirming statement compares the verifier's checksum with SHA-256 of
// the stored bytes. A verifier read naming a different key closes the stream.
func (w *erasureFlushWorker) flush(ctx context.Context, seq, generation int64) (erasureFlushOutcome, error) {
	pos := synthledger.Position{Epoch: w.epoch, Stream: w.stream, Seq: seq}
	var key, record []byte
	if err := w.conn.QueryRow(ctx, `
		SELECT operation_key, record FROM erasure_outbox
		WHERE epoch = $1 AND stream_id = $2 AND seq = $3`, w.epoch, w.stream[:], seq).Scan(&key, &record); err != nil {
		return erasureFlushPending, fmt.Errorf("read outbox seq %d: %w", seq, err)
	}

	if err := w.io("create", pos); err != nil {
		return erasureFlushPending, err
	}
	answer, err := w.flusher.Create(record)
	w.rec.add(erasureFlushCreate{seq: seq, holder: w.holder, body: record, answer: answer, err: err})
	if errors.Is(err, synthledger.ErrResponseLost) {
		return erasureFlushPending, nil
	}
	if err != nil {
		return erasureFlushPending, fmt.Errorf("create seq %d: %w", seq, err)
	}

	if err := w.io("proof", pos); err != nil {
		return erasureFlushPending, err
	}
	proof, ok := w.verify(pos)
	if !ok {
		return erasureFlushPending, nil
	}
	held := proof.Key()
	if !bytes.Equal(held[:], key) {
		if _, err := w.q.CloseErasureFlushStream(ctx, db.CloseErasureFlushStreamParams{
			Epoch: w.epoch, StreamID: w.stream[:], Position: seq,
		}); err != nil {
			return erasureFlushPending, fmt.Errorf("close stream at seq %d: %w", seq, err)
		}
		return erasureFlushClosed, nil
	}
	sum := proof.Checksum()
	rows, err := w.q.ConfirmErasureFlushPosition(ctx, db.ConfirmErasureFlushPositionParams{
		Checksum:     sum[:],
		ArrivalSeq:   proof.ArrivalSeq(),
		ProofKind:    "synthetic",
		OperationKey: held[:],
		Epoch:        w.epoch,
		StreamID:     w.stream[:],
		Seq:          seq,
		Generation:   generation,
	})
	if err != nil {
		return erasureFlushPending, fmt.Errorf("confirm seq %d: %w", seq, err)
	}
	if rows == 1 {
		return erasureFlushConfirmed, nil
	}
	return erasureFlushPending, nil
}

// verify is the verifier read. An unavailable verifier, nothing held, or proof
// about another position is no proof.
func (w *erasureFlushWorker) verify(pos synthledger.Position) (synthledger.Proof, bool) {
	proof, err := w.proof(pos)
	return proof, err == nil && proof.Position() == pos
}

// run grants seq 1 and flushes in order through seq through, stopping at the
// first position that does not confirm.
func (w *erasureFlushWorker) run(ctx context.Context, through int64) (int64, erasureFlushOutcome, error) {
	generation, err := w.grant(ctx, 1)
	if err != nil {
		return 1, erasureFlushPending, fmt.Errorf("grant seq 1: %w", err)
	}
	for seq := int64(1); ; seq++ {
		outcome, err := w.flush(ctx, seq, generation)
		if err != nil || outcome != erasureFlushConfirmed || seq == through {
			return seq, outcome, err
		}
		if generation, err = w.advance(ctx, generation, seq+1); err != nil {
			return seq + 1, erasureFlushPending, fmt.Errorf("advance to seq %d: %w", seq+1, err)
		}
	}
}

func erasureFlushProvider(t *testing.T) *synthledger.Provider {
	t.Helper()
	return synthledger.New(t, synthledger.NewMemoryStore(t))
}

func erasureFlushAt(epoch int64, stream erasureledger.StreamID, seq int64) synthledger.Position {
	return synthledger.Position{Epoch: epoch, Stream: stream, Seq: seq}
}

// erasureFlushCaptured is one committed capture position.
type erasureFlushCaptured struct {
	seq    int64
	key    []byte
	record []byte
}

// erasureFlushCaptureDelete sends from to to and captures to's delete of its
// copy, returning the position the capture committed.
func erasureFlushCaptureDelete(t *testing.T, ctx context.Context, s *store.Store, from, to store.User, rid, epoch int64, stream erasureledger.StreamID) erasureFlushCaptured {
	t.Helper()
	message := send(t, s, from, to, "flush capture", rid)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, to.ID, []int64{message.PeerLocalID}, false, epoch, stream, nil); err != nil {
		t.Fatalf("capture delete: %v", err)
	}
	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	last := rows[len(rows)-1]
	return erasureFlushCaptured{seq: last.position, key: last.operationKey, record: last.record}
}

// erasureFlushForged re-encodes the record stored at a position under key with
// a different payload: valid bytes for the same slot that are not the stored
// bytes.
func erasureFlushForged(t *testing.T, stored []byte, key erasureledger.OperationKey) []byte {
	t.Helper()
	rec, err := erasureledger.Decode(stored)
	if err != nil {
		t.Fatalf("decode stored record: %v", err)
	}
	payload, err := erasureledger.NewMessageCopies(erasureledger.MessageCopy{OwnerID: 999_999, LocalID: 1})
	if err != nil {
		t.Fatalf("forged payload: %v", err)
	}
	forged, err := erasureledger.NewRecord(rec.Kind, rec.Epoch, rec.Stream, rec.Seq, key, payload)
	if err != nil {
		t.Fatalf("forged record: %v", err)
	}
	body := erasureLedgerEncode(t, forged)
	if bytes.Equal(body, stored) {
		t.Fatal("forged bytes equal the stored bytes")
	}
	return body
}

func erasureFlushWantOutcome(t *testing.T, what string, got erasureFlushOutcome, err error, want erasureFlushOutcome) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// erasureFlushStreamOf is the slice (a) fixture view of a captured stream.
func erasureFlushStreamOf(epoch int64, stream erasureledger.StreamID) erasureFlushStream {
	return erasureFlushStream{epoch: epoch, stream: stream[:]}
}

// erasureFlushProofOf is the evidence a verifier read reports, as stored.
func erasureFlushProofOf(t *testing.T, p *synthledger.Provider, pos synthledger.Position) erasureFlushProof {
	t.Helper()
	proof, err := p.ProofReader().Proof(pos)
	if err != nil {
		t.Fatalf("verifier read at %d: %v", pos.Seq, err)
	}
	key, sum := proof.Key(), proof.Checksum()
	return erasureFlushProof{key: key[:], checksum: sum[:], arrivalSeq: proof.ArrivalSeq()}
}

func erasureFlushLogSeqs(p *synthledger.Provider) []int64 {
	log := p.CreateLog()
	seqs := make([]int64, 0, len(log))
	for _, pos := range log {
		seqs = append(seqs, pos.Seq)
	}
	return seqs
}

func erasureFlushWantOpen(t *testing.T, ctx context.Context, q *db.Queries, epoch int64, stream erasureledger.StreamID) {
	t.Helper()
	_, err := q.ErasureFlushClosedStreamByStream(ctx, db.ErasureFlushClosedStreamByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stream closed marker read: err %v, want the stream open", err)
	}
}

func erasureFlushWantClosedAt(t *testing.T, ctx context.Context, q *db.Queries, epoch int64, stream erasureledger.StreamID, position int64) {
	t.Helper()
	marker, err := q.ErasureFlushClosedStreamByStream(ctx, db.ErasureFlushClosedStreamByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil || marker.Position != position {
		t.Fatalf("closed marker = %+v, err %v; want closed at %d", marker, err, position)
	}
}

// erasureFlushSnapshotTables are the rows one whole-snapshot rewind restores
// together.
var erasureFlushSnapshotTables = []string{
	"erasure_outbox", "erasure_stream_counter",
	"erasure_flush_claim", "erasure_flush_confirmation", "erasure_flush_closed_stream",
	"message_delete_receipts",
}

// erasureFlushSnapshot copies every snapshot table at one point into schema
// name.
func erasureFlushSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("begin snapshot %s: %v", name, err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back snapshot %s: %v", name, err)
		}
	}()
	// name and the tables are fixed identifiers chosen by these tests.
	if _, err := tx.Exec(ctx, `CREATE SCHEMA `+name); err != nil {
		t.Fatalf("create snapshot schema %s: %v", name, err)
	}
	for _, table := range erasureFlushSnapshotTables {
		if _, err := tx.Exec(ctx, `CREATE TABLE `+name+`.`+table+` AS TABLE public.`+table); err != nil {
			t.Fatalf("snapshot %s into %s: %v", table, name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit snapshot %s: %v", name, err)
	}
}

// erasureFlushRewind restores every snapshot table from schema name in one
// transaction, on the same epoch. The provider is not touched.
func erasureFlushRewind(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rewind to %s: %v", name, err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back rewind to %s: %v", name, err)
		}
	}()
	for _, table := range erasureFlushSnapshotTables {
		if _, err := tx.Exec(ctx, `DELETE FROM public.`+table); err != nil {
			t.Fatalf("clear %s: %v", table, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO public.`+table+` TABLE `+name+`.`+table); err != nil {
			t.Fatalf("restore %s from %s: %v", table, name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit rewind to %s: %v", name, err)
	}
}

// erasureFlushHeldAcrossIO lists, from the oracle connection, every other
// backend of this database that is idle in a transaction or holds a ledger
// relation lock or an advisory lock (the owner lock is advisory).
func erasureFlushHeldAcrossIO(ctx context.Context, oracle *pgxpool.Conn) ([]string, error) {
	rows, err := oracle.Query(ctx, `
		SELECT format('backend %s is %s: %s', pid, state, left(query, 120))
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
		  AND state LIKE 'idle in transaction%'
		UNION ALL
		SELECT format('backend %s holds %s %s lock on %s', l.pid, l.mode, l.locktype,
		              coalesce(l.relation::regclass::text, l.objid::text))
		FROM pg_locks AS l
		WHERE l.pid <> pg_backend_pid()
		  AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND (l.locktype = 'advisory' OR l.relation::regclass::text LIKE 'erasure\_%')`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TestErasureFlushStalledProviderLeavesSendAndDeleteCommitting: a create blocked
// at the provider after its claim committed holds nothing that a DM send or a
// captured delete on the same stream waits for.
func TestErasureFlushStalledProviderLeavesSendAndDeleteCommitting(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	a := mustUser(t, s, "+15551860101")
	b := mustUser(t, s, "+15551860102")
	epoch, stream := int64(41), messageDeleteCaptureStream(0x61)
	first := erasureFlushCaptureDelete(t, ctx, s, a, b, 7601, epoch, stream)

	p := erasureFlushProvider(t)
	w := newErasureFlushWorker(t, pool, p, epoch, stream, &erasureFlushRecorder{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStall)
	entered := p.StallCreate(erasureFlushAt(epoch, stream, first.seq), release)

	generation, err := w.grant(ctx, first.seq)
	if err != nil {
		t.Fatalf("grant seq %d: %v", first.seq, err)
	}
	type result struct {
		outcome erasureFlushOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, err := w.flush(ctx, first.seq, generation)
		done <- result{outcome, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("create never reached the provider")
	}
	claim, err := q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil || claim.Generation != generation || claim.Position != first.seq {
		t.Fatalf("claim seen from another connection = %+v, err %v; want committed at %d", claim, err, first.seq)
	}

	deadline, cancelDeadline := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDeadline()
	message, _, _, _, err := s.SendMessage(deadline, a.ID, b.ID, "while stalled", 7602, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send while the provider is stalled: %v", err)
	}
	if _, err := store.CaptureDeleteMessagesForTest(deadline, s, b.ID, []int64{message.PeerLocalID}, false, epoch, stream, nil); err != nil {
		t.Fatalf("delete while the provider is stalled: %v", err)
	}
	if !msgAt(t, s, b.ID, message.PeerLocalID).Deleted {
		t.Fatal("delete committed without deleting the copy")
	}
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 2 || rows[1].position != first.seq+1 {
		t.Fatalf("capture while stalled = %+v, want the next position on the same stream", rows)
	}
	select {
	case got := <-done:
		t.Fatalf("flush finished while the provider was stalled: %+v", got)
	default:
	}

	releaseStall()
	got := <-done
	erasureFlushWantOutcome(t, "stalled flush after release", got.outcome, got.err, erasureFlushConfirmed)
	generation, err = w.advance(ctx, generation, first.seq+1)
	if err != nil {
		t.Fatalf("advance to the delete captured while stalled: %v", err)
	}
	outcome, err := w.flush(ctx, first.seq+1, generation)
	erasureFlushWantOutcome(t, "flush of the delete captured while stalled", outcome, err, erasureFlushConfirmed)
}

// TestErasureFlushHoldsNoTransactionOrLockAcrossProviderIO checks, during every
// provider call, every other backend of the cloned database: none is idle in a
// transaction, and none holds a ledger relation lock or an advisory lock.
func TestErasureFlushHoldsNoTransactionOrLockAcrossProviderIO(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	oracle, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire oracle connection: %v", err)
	}
	defer oracle.Release()
	a := mustUser(t, s, "+15551860201")
	b := mustUser(t, s, "+15551860202")
	epoch, stream := int64(42), messageDeleteCaptureStream(0x62)
	const positions = 3
	for i := range int64(positions) {
		erasureFlushCaptureDelete(t, ctx, s, a, b, 7610+i, epoch, stream)
	}

	// The oracle sees a transaction left open holding a claim row write and the
	// owner lock, so a clean reading below is not a blind one.
	control, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin control transaction: %v", err)
	}
	if _, err := control.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, a.ID); err != nil {
		t.Fatalf("control owner lock: %v", err)
	}
	if _, err := control.Exec(ctx, `SELECT FROM erasure_stream_counter WHERE epoch = $1 FOR UPDATE`, epoch); err != nil {
		t.Fatalf("control counter lock: %v", err)
	}
	held, err := erasureFlushHeldAcrossIO(ctx, oracle)
	if err != nil {
		t.Fatalf("oracle read: %v", err)
	}
	joined := strings.Join(held, "\n")
	if !strings.Contains(joined, "idle in transaction") || !strings.Contains(joined, "advisory") || !strings.Contains(joined, "erasure_stream_counter") {
		t.Fatalf("oracle missed the control transaction:\n%s", joined)
	}
	if err := control.Rollback(ctx); err != nil {
		t.Fatalf("roll back control transaction: %v", err)
	}

	p := erasureFlushProvider(t)
	p.DropResponse(erasureFlushAt(epoch, stream, 2))
	w := newErasureFlushWorker(t, pool, p, epoch, stream, &erasureFlushRecorder{})
	var calls []string
	w.beforeIO = func(call string, pos synthledger.Position) error {
		calls = append(calls, fmt.Sprintf("%s %d", call, pos.Seq))
		held, err := erasureFlushHeldAcrossIO(ctx, oracle)
		if err != nil {
			return fmt.Errorf("oracle read before %s %d: %w", call, pos.Seq, err)
		}
		if len(held) > 0 {
			return fmt.Errorf("held across %s %d:\n%s", call, pos.Seq, strings.Join(held, "\n"))
		}
		return nil
	}

	seq, outcome, err := w.run(ctx, positions)
	if err != nil || seq != 2 || outcome != erasureFlushPending {
		t.Fatalf("run with a lost response at 2 = seq %d %v, err %v; want pending at 2", seq, outcome, err)
	}
	claim, err := w.q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	outcome, err = w.flush(ctx, 2, claim.Generation)
	erasureFlushWantOutcome(t, "retry of seq 2", outcome, err, erasureFlushConfirmed)
	generation, err := w.advance(ctx, claim.Generation, 3)
	if err != nil {
		t.Fatalf("advance to 3: %v", err)
	}
	outcome, err = w.flush(ctx, 3, generation)
	erasureFlushWantOutcome(t, "flush of seq 3", outcome, err, erasureFlushConfirmed)

	want := []string{"create 1", "proof 1", "create 2", "create 2", "proof 2", "create 3", "proof 3"}
	if !slices.Equal(calls, want) {
		t.Fatalf("oracle ran before %v, want every provider call %v", calls, want)
	}
	if got := erasureFlushLogSeqs(p); !slices.Equal(got, []int64{1, 2, 2, 3}) {
		t.Fatalf("provider creates = %v, want every create checked", got)
	}
}

// TestErasureFlushTakeoverRepeatsIdenticalBytesAndKeepsOneConfirmation: worker
// 1 stalls in create, an explicit takeover hands worker 2 the claim, worker 2
// confirms, and worker 1's resumed create and confirm change nothing.
func TestErasureFlushTakeoverRepeatsIdenticalBytesAndKeepsOneConfirmation(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	a := mustUser(t, s, "+15551860301")
	b := mustUser(t, s, "+15551860302")
	epoch, stream := int64(43), messageDeleteCaptureStream(0x63)
	captured := erasureFlushCaptureDelete(t, ctx, s, a, b, 7620, epoch, stream)
	pos := erasureFlushAt(epoch, stream, captured.seq)

	p := erasureFlushProvider(t)
	rec := &erasureFlushRecorder{}
	first := newErasureFlushWorker(t, pool, p, epoch, stream, rec)
	second := newErasureFlushWorker(t, pool, p, epoch, stream, rec)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseStall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseStall)
	entered := p.StallCreate(pos, release)

	firstGeneration, err := first.grant(ctx, captured.seq)
	if err != nil {
		t.Fatalf("worker 1 grant: %v", err)
	}
	type result struct {
		outcome erasureFlushOutcome
		err     error
	}
	firstDone := make(chan result, 1)
	go func() {
		outcome, err := first.flush(ctx, captured.seq, firstGeneration)
		firstDone <- result{outcome, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("worker 1 create never reached the provider")
	}

	taken, err := second.takeOver(ctx, firstGeneration)
	if err != nil || taken.Position != captured.seq || taken.Generation != firstGeneration+1 {
		t.Fatalf("takeover = %+v, err %v; want the same position at generation %d", taken, err, firstGeneration+1)
	}
	outcome, err := second.flush(ctx, captured.seq, taken.Generation)
	erasureFlushWantOutcome(t, "worker 2 flush", outcome, err, erasureFlushConfirmed)
	secondEvidence := erasureFlushProofOf(t, p, pos)

	releaseStall()
	resumed := <-firstDone
	erasureFlushWantOutcome(t, "worker 1 resumed flush", resumed.outcome, resumed.err, erasureFlushPending)

	creates := rec.all()
	if len(creates) != 2 {
		t.Fatalf("creates = %d, want worker 2's and worker 1's", len(creates))
	}
	if !bytes.Equal(creates[0].holder, second.holder) || creates[0].answer != synthledger.AnswerCreated ||
		!bytes.Equal(creates[1].holder, first.holder) || creates[1].answer != synthledger.AnswerMatchingProof {
		t.Fatalf("creates = %+v, want worker 2 created then worker 1 matched", creates)
	}
	for _, c := range creates {
		if !bytes.Equal(c.body, captured.record) {
			t.Fatalf("create by %x sent bytes that are not the stored record", c.holder)
		}
	}
	if got := erasureFlushLogSeqs(p); !slices.Equal(got, []int64{captured.seq, captured.seq}) {
		t.Fatalf("provider creates = %v, want the same position twice", got)
	}
	sum := sha256.Sum256(captured.record)
	if !bytes.Equal(secondEvidence.checksum, sum[:]) || !bytes.Equal(secondEvidence.key, captured.key) {
		t.Fatalf("worker 2 evidence %+v does not describe the stored record", secondEvidence)
	}
	erasureFlushWantStored(t, ctx, q, erasureFlushStreamOf(epoch, stream), map[int64]erasureFlushProof{captured.seq: secondEvidence})
	claim, err := q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil || !bytes.Equal(claim.Holder, second.holder) || claim.Generation != taken.Generation {
		t.Fatalf("claim after worker 1 resumed = %+v, err %v; want worker 2 at generation %d", claim, err, taken.Generation)
	}
}

// TestErasureFlushLostResponseConfirmsOnlyOnVerifierProof: the provider stores
// position n and the answer is lost. The retry sends the same key and bytes, and
// only a verifier read whose SHA-256 matches the stored bytes confirms. Without
// proof, or with conflicting proof, n stays pending and n+1 is never attempted.
func TestErasureFlushLostResponseConfirmsOnlyOnVerifierProof(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T, seed byte, phone string, rid int64) (context.Context, *pgxpool.Pool, int64, erasureledger.StreamID, erasureFlushCaptured, *synthledger.Provider, *erasureFlushWorker, *erasureFlushRecorder) {
		t.Helper()
		s := open(t)
		ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
		t.Cleanup(cancel)
		pool := store.StorePool(s)
		a := mustUser(t, s, phone+"1")
		b := mustUser(t, s, phone+"2")
		epoch, stream := int64(44), messageDeleteCaptureStream(seed)
		first := erasureFlushCaptureDelete(t, ctx, s, a, b, rid, epoch, stream)
		erasureFlushCaptureDelete(t, ctx, s, a, b, rid+1, epoch, stream)
		p := erasureFlushProvider(t)
		rec := &erasureFlushRecorder{}
		return ctx, pool, epoch, stream, first, p, newErasureFlushWorker(t, pool, p, epoch, stream, rec), rec
	}
	wantNextNeverAttempted := func(t *testing.T, ctx context.Context, w *erasureFlushWorker, p *synthledger.Provider, generation int64) {
		t.Helper()
		_, err := w.advance(ctx, generation, 2)
		erasureFlushWantNoRow(t, "advance to 2 while 1 is pending", err)
		if slices.Contains(erasureFlushLogSeqs(p), 2) {
			t.Fatalf("provider creates = %v, want no create at 2", erasureFlushLogSeqs(p))
		}
		erasureFlushWantStored(t, ctx, w.q, erasureFlushStreamOf(w.epoch, w.stream), nil)
		erasureFlushWantOpen(t, ctx, w.q, w.epoch, w.stream)
	}

	t.Run("verified retry confirms", func(t *testing.T) {
		t.Parallel()
		ctx, _, epoch, stream, first, p, w, rec := setup(t, 0x64, "+1555186040", 7630)
		p.DropResponse(erasureFlushAt(epoch, stream, 1))
		generation, err := w.grant(ctx, 1)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		outcome, err := w.flush(ctx, 1, generation)
		erasureFlushWantOutcome(t, "flush with a lost response", outcome, err, erasureFlushPending)
		wantNextNeverAttempted(t, ctx, w, p, generation)

		outcome, err = w.flush(ctx, 1, generation)
		erasureFlushWantOutcome(t, "retry", outcome, err, erasureFlushConfirmed)
		creates := rec.all()
		if len(creates) != 2 || !errors.Is(creates[0].err, synthledger.ErrResponseLost) || creates[1].answer != synthledger.AnswerMatchingProof {
			t.Fatalf("creates = %+v, want a lost response then a matching repeat", creates)
		}
		for _, c := range creates {
			decoded, err := erasureledger.Decode(c.body)
			if err != nil || !bytes.Equal(c.body, first.record) || !bytes.Equal(decoded.OpKey[:], first.key) {
				t.Fatalf("create sent other bytes or another key than the stored record (err %v)", err)
			}
		}
		evidence := erasureFlushProofOf(t, p, erasureFlushAt(epoch, stream, 1))
		sum := sha256.Sum256(first.record)
		if !bytes.Equal(evidence.checksum, sum[:]) {
			t.Fatalf("verifier checksum %x, want SHA-256 of the stored bytes %x", evidence.checksum, sum)
		}
		erasureFlushWantStored(t, ctx, w.q, erasureFlushStreamOf(epoch, stream), map[int64]erasureFlushProof{1: evidence})
	})

	t.Run("no proof stays pending", func(t *testing.T) {
		t.Parallel()
		ctx, _, epoch, stream, _, p, w, _ := setup(t, 0x65, "+1555186041", 7640)
		p.DropResponse(erasureFlushAt(epoch, stream, 1))
		w.proof = func(synthledger.Position) (synthledger.Proof, error) {
			return synthledger.Proof{}, errErasureFlushVerifierUnavailable
		}
		generation, err := w.grant(ctx, 1)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		for attempt := range 3 {
			outcome, err := w.flush(ctx, 1, generation)
			erasureFlushWantOutcome(t, fmt.Sprintf("attempt %d without proof", attempt), outcome, err, erasureFlushPending)
		}
		if got := erasureFlushLogSeqs(p); !slices.Equal(got, []int64{1, 1, 1}) {
			t.Fatalf("provider creates = %v, want three creates at 1", got)
		}
		wantNextNeverAttempted(t, ctx, w, p, generation)
	})

	t.Run("conflicting proof stays pending", func(t *testing.T) {
		t.Parallel()
		ctx, _, epoch, stream, first, p, w, _ := setup(t, 0x66, "+1555186042", 7650)
		// An earlier create stored other bytes under the same key at 1, and its
		// answer was lost.
		pos := erasureFlushAt(epoch, stream, 1)
		p.DropResponse(pos)
		if _, err := p.NewFlusher(epoch, stream).Create(erasureFlushForged(t, first.record, erasureledger.OperationKey(first.key))); !errors.Is(err, synthledger.ErrResponseLost) {
			t.Fatalf("earlier create: err %v, want a lost response", err)
		}
		generation, err := w.grant(ctx, 1)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		outcome, err := w.flush(ctx, 1, generation)
		erasureFlushWantOutcome(t, "flush against conflicting proof", outcome, err, erasureFlushPending)
		evidence := erasureFlushProofOf(t, p, pos)
		sum := sha256.Sum256(first.record)
		if !bytes.Equal(evidence.key, first.key) || bytes.Equal(evidence.checksum, sum[:]) {
			t.Fatalf("verifier evidence %+v, want the same key with a checksum that is not the stored bytes'", evidence)
		}
		wantNextNeverAttempted(t, ctx, w, p, generation)
	})
}

// TestErasureFlushBarrierHoldsWithoutTheProviderGate: with the provider's
// sequence gate off, concurrent claimers and a worker restart between claim and
// confirm still produce a strictly ordered provider log, and the claim CAS
// refuses n+1 while n is pending.
func TestErasureFlushBarrierHoldsWithoutTheProviderGate(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	const positions, claimers, restartAt = 5, 6, 3
	fixture := erasureFlushOutbox(t, ctx, q, 45, positions)
	epoch, stream := fixture.epoch, erasureledger.StreamID(fixture.stream)

	p := erasureFlushProvider(t)
	p.DisableSequenceGate()
	rec := &erasureFlushRecorder{}
	var mu sync.Mutex
	var violations []string
	// Every create checks, before it reaches the provider, that its predecessor
	// is confirmed in the database.
	beforeIO := func(call string, pos synthledger.Position) error {
		if call != "create" || pos.Seq == 1 {
			return nil
		}
		var confirmed bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT FROM erasure_flush_confirmation WHERE epoch = $1 AND stream_id = $2 AND seq = $3)`,
			pos.Epoch, pos.Stream[:], pos.Seq-1).Scan(&confirmed); err != nil {
			return fmt.Errorf("read predecessor of %d: %w", pos.Seq, err)
		}
		if !confirmed {
			mu.Lock()
			violations = append(violations, fmt.Sprintf("create %d before %d confirmed", pos.Seq, pos.Seq-1))
			mu.Unlock()
		}
		return nil
	}
	newWorker := func() *erasureFlushWorker {
		w := newErasureFlushWorker(t, pool, p, epoch, stream, rec)
		w.beforeIO = beforeIO
		return w
	}

	var finished, restarted atomic.Bool
	var refusals atomic.Int64
	crashed := make(chan int64, 1)
	refuse := func(what string, err error) error {
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%s: err %w, want refusal", what, err)
		}
		refusals.Add(1)
		return nil
	}
	loop := func(w *erasureFlushWorker) error {
		for !finished.Load() {
			if err := ctx.Err(); err != nil {
				return err
			}
			claim, err := w.q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
			if errors.Is(err, pgx.ErrNoRows) {
				if _, err := w.grant(ctx, 1); err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return fmt.Errorf("grant 1: %w", err)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("read claim: %w", err)
			}
			seq, generation := claim.Position, claim.Generation
			if !bytes.Equal(claim.Holder, w.holder) {
				if seq < positions {
					_, err := w.grant(ctx, seq+1)
					if err := refuse("non-holder grant", err); err != nil {
						return err
					}
					_, err = w.advance(ctx, generation, seq+1)
					if err := refuse("non-holder advance", err); err != nil {
						return err
					}
				}
				time.Sleep(time.Millisecond)
				continue
			}
			if seq == restartAt && restarted.CompareAndSwap(false, true) {
				// Restart between claim and confirm: the claim is committed and
				// this worker is gone.
				crashed <- generation
				return nil
			}
			if seq < positions {
				_, err := w.advance(ctx, generation, seq+1)
				if err := refuse(fmt.Sprintf("holder advance to %d while %d is pending", seq+1, seq), err); err != nil {
					return err
				}
			}
			outcome, err := w.flush(ctx, seq, generation)
			if err != nil {
				return err
			}
			if outcome != erasureFlushConfirmed {
				return fmt.Errorf("flush %d = %v, want confirmed", seq, outcome)
			}
			if seq == positions {
				finished.Store(true)
				return nil
			}
			if _, err := w.advance(ctx, generation, seq+1); err != nil {
				return fmt.Errorf("advance to %d after %d confirmed: %w", seq+1, seq, err)
			}
		}
		return nil
	}

	var wg sync.WaitGroup
	errs := make(chan error, claimers+1)
	start := func(w *erasureFlushWorker) {
		wg.Go(func() {
			if err := loop(w); err != nil {
				errs <- err
			}
		})
	}
	for range claimers {
		start(newWorker())
	}

	var generation int64
	select {
	case generation = <-crashed:
	case err := <-errs:
		t.Fatalf("claimer failed before the restart: %v", err)
	case <-ctx.Done():
		t.Fatal("no worker reached the restart position")
	}
	_, err := erasureFlushGrant(ctx, q, fixture, erasureFlushHolder(t), restartAt+1)
	erasureFlushWantNoRow(t, "grant past the abandoned position", err)
	claim, err := q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil || claim.Position != restartAt || claim.Generation != generation {
		t.Fatalf("claim after the crash = %+v, err %v; want %d at generation %d", claim, err, restartAt, generation)
	}
	_, err = erasureFlushAdvance(ctx, q, fixture, claim.Holder, generation, restartAt+1)
	erasureFlushWantNoRow(t, "the crashed holder's advance past its pending position", err)

	restartedWorker := newWorker()
	if _, err := restartedWorker.takeOver(ctx, generation); err != nil {
		t.Fatalf("restarted worker takeover: %v", err)
	}
	start(restartedWorker)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		return
	}

	if len(violations) > 0 {
		t.Fatalf("barrier violations: %v", violations)
	}
	if got := erasureFlushLogSeqs(p); !slices.Equal(got, []int64{1, 2, 3, 4, 5}) {
		t.Fatalf("provider creates = %v, want strictly ordered 1..%d", got, positions)
	}
	if rows := erasureFlushConfirmations(t, ctx, q, fixture); len(rows) != positions {
		t.Fatalf("confirmations = %d, want %d", len(rows), positions)
	}
	if refusals.Load() < positions-1 {
		t.Fatalf("claim CAS refusals = %d, want at least one per pending position", refusals.Load())
	}
}

// TestErasureFlushRestoreRetriesUnderOriginalIdentity: a whole-snapshot rewind on
// the same epoch, with provider state kept, leaves the position pending again;
// the retry sends the original key, seq and bytes and confirms.
func TestErasureFlushRestoreRetriesUnderOriginalIdentity(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	a := mustUser(t, s, "+15551860601")
	b := mustUser(t, s, "+15551860602")
	chat := chatWith(t, s, a, b)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "restore", RandomID: 7660})
	identity := messageDeleteReceiptLookupIdentity(messageDeletePendingFixture(t, a.ID))
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}
	epoch, stream := int64(46), messageDeleteCaptureStream(0x67)
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil); err != nil {
		t.Fatalf("compose delete: %v", err)
	}
	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	if len(rows) != 1 {
		t.Fatalf("captured %d positions, want 1", len(rows))
	}
	captured := rows[0]
	receiptState := func() string {
		t.Helper()
		var state string
		if err := pool.QueryRow(ctx, `SELECT state FROM message_delete_receipts WHERE owner_id = $1`, a.ID).Scan(&state); err != nil {
			t.Fatalf("read receipt: %v", err)
		}
		return state
	}
	erasureFlushSnapshot(t, ctx, pool, "flush_restore_pending")

	p := erasureFlushProvider(t)
	rec := &erasureFlushRecorder{}
	_, outcome, err := newErasureFlushWorker(t, pool, p, epoch, stream, rec).run(ctx, 1)
	erasureFlushWantOutcome(t, "flush before the rewind", outcome, err, erasureFlushConfirmed)
	pos := erasureFlushAt(epoch, stream, captured.position)
	evidence := erasureFlushProofOf(t, p, pos)
	view := erasureFlushStreamOf(epoch, stream)
	erasureFlushWantStored(t, ctx, q, view, map[int64]erasureFlushProof{captured.position: evidence})
	if state := receiptState(); state != "confirmed" {
		t.Fatalf("receipt before the rewind = %s, want confirmed", state)
	}

	erasureFlushRewind(t, ctx, pool, "flush_restore_pending")
	erasureFlushWantStored(t, ctx, q, view, nil)
	_, err = q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	erasureFlushWantNoRow(t, "claim after the rewind", err)
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != captured.position {
		t.Fatalf("stream counter after the rewind = %d, present %v; want %d", position, ok, captured.position)
	}
	if state := receiptState(); state != "pending" {
		t.Fatalf("receipt after the rewind = %s, want pending", state)
	}

	_, outcome, err = newErasureFlushWorker(t, pool, p, epoch, stream, rec).run(ctx, 1)
	erasureFlushWantOutcome(t, "retry after the rewind", outcome, err, erasureFlushConfirmed)
	creates := rec.all()
	if len(creates) != 2 || creates[1].answer != synthledger.AnswerMatchingProof {
		t.Fatalf("creates = %+v, want the original create and a matching retry", creates)
	}
	for _, c := range creates {
		decoded, err := erasureledger.Decode(c.body)
		if err != nil || c.seq != captured.position || decoded.Seq != captured.position ||
			!bytes.Equal(decoded.OpKey[:], captured.operationKey) || !bytes.Equal(c.body, captured.record) {
			t.Fatalf("create %+v (err %v) is not the original key, seq and bytes", c, err)
		}
	}
	erasureFlushWantStored(t, ctx, q, view, map[int64]erasureFlushProof{captured.position: evidence})
	if state := receiptState(); state != "confirmed" {
		t.Fatalf("receipt after the retry = %s, want confirmed", state)
	}
}

// TestErasureFlushLostLineageClosesTheStreamWithoutOverwrite: after a rewind, a
// new capture reuses position n+1 under a new key. The verifier shows the old
// key there: nothing is overwritten or confirmed, the stream closes, later
// claims refuse and other streams progress. A rewind past the closure and a
// retry close it again and still overwrite nothing.
func TestErasureFlushLostLineageClosesTheStreamWithoutOverwrite(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	a := mustUser(t, s, "+15551860701")
	b := mustUser(t, s, "+15551860702")
	epoch, stream, other := int64(47), messageDeleteCaptureStream(0x68), messageDeleteCaptureStream(0x69)
	view := erasureFlushStreamOf(epoch, stream)
	p := erasureFlushProvider(t)
	rec := &erasureFlushRecorder{}

	erasureFlushCaptureDelete(t, ctx, s, a, b, 7670, epoch, stream)
	erasureFlushSnapshot(t, ctx, pool, "flush_lineage_one")
	lost := erasureFlushCaptureDelete(t, ctx, s, a, b, 7671, epoch, stream)
	_, outcome, err := newErasureFlushWorker(t, pool, p, epoch, stream, rec).run(ctx, 2)
	erasureFlushWantOutcome(t, "flush of the original lineage", outcome, err, erasureFlushConfirmed)
	lostName := synthledger.KeyName(erasureledger.OperationKey(lost.key))
	wantProviderUnchanged := func(newKey []byte) {
		t.Helper()
		if body, ok := p.StoredBody(lostName); !ok || !bytes.Equal(body, lost.record) {
			t.Fatal("the provider no longer holds the original bytes at the collision")
		}
		if _, ok := p.StoredBody(synthledger.KeyName(erasureledger.OperationKey(newKey))); ok {
			t.Fatal("the provider stored the new lineage's bytes")
		}
	}

	erasureFlushRewind(t, ctx, pool, "flush_lineage_one")
	w := newErasureFlushWorker(t, pool, p, epoch, stream, rec)
	_, outcome, err = w.run(ctx, 1)
	erasureFlushWantOutcome(t, "retry of seq 1 after the rewind", outcome, err, erasureFlushConfirmed)
	replacement := erasureFlushCaptureDelete(t, ctx, s, a, b, 7672, epoch, stream)
	if replacement.seq != lost.seq || bytes.Equal(replacement.key, lost.key) {
		t.Fatalf("new capture at %d key %x, want position %d under a new key", replacement.seq, replacement.key, lost.seq)
	}
	erasureFlushSnapshot(t, ctx, pool, "flush_lineage_two")

	claim, err := q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: epoch, StreamID: stream[:]})
	if err != nil {
		t.Fatalf("read claim: %v", err)
	}
	generation, err := w.advance(ctx, claim.Generation, replacement.seq)
	if err != nil {
		t.Fatalf("advance to the new capture: %v", err)
	}
	outcome, err = w.flush(ctx, replacement.seq, generation)
	erasureFlushWantOutcome(t, "flush of the new capture", outcome, err, erasureFlushClosed)
	creates := rec.all()
	if last := creates[len(creates)-1]; last.answer != synthledger.AnswerLostLineage || !bytes.Equal(last.body, replacement.record) {
		t.Fatalf("last create = %+v, want the new bytes answered as lost lineage", last)
	}
	if evidence := erasureFlushProofOf(t, p, erasureFlushAt(epoch, stream, lost.seq)); !bytes.Equal(evidence.key, lost.key) {
		t.Fatalf("verifier key at %d = %x, want the original %x", lost.seq, evidence.key, lost.key)
	}
	wantProviderUnchanged(replacement.key)
	erasureFlushWantClosedAt(t, ctx, q, epoch, stream, lost.seq)
	confirmations := erasureFlushConfirmations(t, ctx, q, view)
	if len(confirmations) != 1 || confirmations[0].Seq != 1 {
		t.Fatalf("confirmations = %+v, want only seq 1", confirmations)
	}
	later := erasureFlushCaptureDelete(t, ctx, s, a, b, 7673, epoch, stream)
	_, err = w.advance(ctx, generation, later.seq)
	erasureFlushWantNoRow(t, "advance on the closed stream", err)
	_, err = w.takeOver(ctx, generation)
	erasureFlushWantNoRow(t, "takeover on the closed stream", err)
	erasureFlushWantConfirm(t, ctx, q, view, replacement.seq, generation, erasureFlushProof{
		key: replacement.key, checksum: func() []byte { sum := sha256.Sum256(replacement.record); return sum[:] }(), arrivalSeq: 99,
	}, 0)

	erasureFlushCaptureDelete(t, ctx, s, a, b, 7674, epoch, other)
	_, outcome, err = newErasureFlushWorker(t, pool, p, epoch, other, rec).run(ctx, 1)
	erasureFlushWantOutcome(t, "flush of another stream", outcome, err, erasureFlushConfirmed)

	erasureFlushRewind(t, ctx, pool, "flush_lineage_two")
	erasureFlushWantOpen(t, ctx, q, epoch, stream)
	retry := newErasureFlushWorker(t, pool, p, epoch, stream, rec)
	taken, err := retry.takeOver(ctx, claim.Generation)
	if err != nil {
		t.Fatalf("takeover after the rewind past the closure: %v", err)
	}
	generation, err = retry.advance(ctx, taken.Generation, replacement.seq)
	if err != nil {
		t.Fatalf("advance after the rewind past the closure: %v", err)
	}
	outcome, err = retry.flush(ctx, replacement.seq, generation)
	erasureFlushWantOutcome(t, "retry after the rewind past the closure", outcome, err, erasureFlushClosed)
	wantProviderUnchanged(replacement.key)
	erasureFlushWantClosedAt(t, ctx, q, epoch, stream, lost.seq)
	if confirmations := erasureFlushConfirmations(t, ctx, q, view); len(confirmations) != 1 {
		t.Fatalf("confirmations after the second closure = %+v, want only seq 1", confirmations)
	}
}

// TestErasureFlushConflictingProofAndUnverifiedLostLineageStayOpen: a verifier
// read showing the same key with other bytes, and a lost-lineage answer from
// create alone while the verifier is unavailable, both leave the stream open and
// the position pending.
func TestErasureFlushConflictingProofAndUnverifiedLostLineageStayOpen(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T, seed byte, phone string, rid int64) (context.Context, int64, erasureledger.StreamID, erasureFlushCaptured, *synthledger.Provider, *erasureFlushWorker, *erasureFlushRecorder) {
		t.Helper()
		s := open(t)
		ctx, cancel := context.WithTimeout(context.Background(), erasureFlushComposeTimeout)
		t.Cleanup(cancel)
		pool := store.StorePool(s)
		a := mustUser(t, s, phone+"1")
		b := mustUser(t, s, phone+"2")
		epoch, stream := int64(48), messageDeleteCaptureStream(seed)
		first := erasureFlushCaptureDelete(t, ctx, s, a, b, rid, epoch, stream)
		p := erasureFlushProvider(t)
		rec := &erasureFlushRecorder{}
		return ctx, epoch, stream, first, p, newErasureFlushWorker(t, pool, p, epoch, stream, rec), rec
	}
	wantOpenAndPending := func(t *testing.T, ctx context.Context, w *erasureFlushWorker, generation int64) {
		t.Helper()
		erasureFlushWantOpen(t, ctx, w.q, w.epoch, w.stream)
		erasureFlushWantStored(t, ctx, w.q, erasureFlushStreamOf(w.epoch, w.stream), nil)
		claim, err := w.q.ErasureFlushClaimByStream(ctx, db.ErasureFlushClaimByStreamParams{Epoch: w.epoch, StreamID: w.stream[:]})
		if err != nil || claim.Position != 1 || claim.Generation != generation {
			t.Fatalf("claim = %+v, err %v; want still at 1, generation %d", claim, err, generation)
		}
	}

	t.Run("same key conflicting proof", func(t *testing.T) {
		t.Parallel()
		ctx, epoch, stream, first, p, w, rec := setup(t, 0x6a, "+1555186080", 7680)
		if answer, err := p.NewFlusher(epoch, stream).Create(erasureFlushForged(t, first.record, erasureledger.OperationKey(first.key))); err != nil || answer != synthledger.AnswerCreated {
			t.Fatalf("plant same-key bytes: %v, err %v", answer, err)
		}
		generation, err := w.grant(ctx, 1)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		for attempt := range 2 {
			outcome, err := w.flush(ctx, 1, generation)
			erasureFlushWantOutcome(t, fmt.Sprintf("attempt %d against same-key conflicting proof", attempt), outcome, err, erasureFlushPending)
		}
		for _, c := range rec.all() {
			if c.answer != synthledger.AnswerConflictingProof {
				t.Fatalf("create answer = %v, want conflicting proof", c.answer)
			}
		}
		wantOpenAndPending(t, ctx, w, generation)
	})

	t.Run("create-only lost lineage while the verifier is unavailable", func(t *testing.T) {
		t.Parallel()
		ctx, epoch, stream, first, p, w, rec := setup(t, 0x6b, "+1555186081", 7690)
		if answer, err := p.NewFlusher(epoch, stream).Create(erasureFlushForged(t, first.record, erasureLedgerOpKey(t))); err != nil || answer != synthledger.AnswerCreated {
			t.Fatalf("plant another lineage: %v, err %v", answer, err)
		}
		verify := w.proof
		w.proof = func(synthledger.Position) (synthledger.Proof, error) {
			return synthledger.Proof{}, errErasureFlushVerifierUnavailable
		}
		generation, err := w.grant(ctx, 1)
		if err != nil {
			t.Fatalf("grant: %v", err)
		}
		outcome, err := w.flush(ctx, 1, generation)
		erasureFlushWantOutcome(t, "flush with only a create answer", outcome, err, erasureFlushPending)
		if creates := rec.all(); len(creates) != 1 || creates[0].answer != synthledger.AnswerLostLineage {
			t.Fatalf("creates = %+v, want one lost-lineage answer", creates)
		}
		wantOpenAndPending(t, ctx, w, generation)

		// The same position closes once a verifier read shows the other key.
		w.proof = verify
		outcome, err = w.flush(ctx, 1, generation)
		erasureFlushWantOutcome(t, "flush with the verifier back", outcome, err, erasureFlushClosed)
		erasureFlushWantClosedAt(t, ctx, w.q, epoch, stream, 1)
	})
}
