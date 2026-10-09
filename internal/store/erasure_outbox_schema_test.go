package store_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// Inert erasure-ledger persistence: the outbox and the epoch/lineage markers,
// tested as a storage contract.
//
// Several tests below write a ledger row in the same transaction as a message
// deletion, to prove the pair commits and rolls back together. That is what the
// outbox has to support, and it is what these tables are for. No production path
// composes them: the criterion is that a writer will be able to, not that one
// does. TestErasureLedgerPersistenceHasNoRuntimeCaller keeps that line enforced,
// and TestErasureLedgerTablesStayEmptyThroughLivePaths is the live half of the
// same fact: a self-delete, a revoke, a channel post delete and the media eraser
// sweep all leave the tables empty.

// erasureLedgerMigration is the file this slice adds, named so the replay test can
// build a database that does not have it and apply it by hand.
const erasureLedgerMigration = "20261008000070_erasure_outbox_epoch_markers.sql"

// erasureLedgerTables are the inert tables this slice adds.
var erasureLedgerTables = []string{"erasure_outbox", "erasure_epoch", "erasure_epoch_completion"}

// erasureLedgerQueries are the generated access points this slice adds. The only
// callers allowed in the repository are the tests in this package.
var erasureLedgerQueries = []string{
	"InsertErasureOutboxRecord",
	"ErasureOutboxByOperationKey",
	"ErasureOutboxStreamRecords",
	"ErasureOutboxTotal",
	"InsertErasureEpochMarker",
	"InsertErasureEpochCompletion",
	"ErasureEpochLineageState",
	"ErasureEpochLineagesForEpoch",
	"ErasureEpochMarkerTotal",
	"ErasureEpochCompletionTotal",
}

// erasureLedgerSchema is the accepted shape of the three tables, spelled
// "table.column:type". It is asserted exactly, so a column cannot arrive
// quietly: no clock column, because the ledger is ordered by (epoch, stream, seq)
// and, off-alpha, by provider arrival evidence, never by a writer's wall time; and
// no text column, because the row envelope carries identifiers and opaque bytes
// only, and the record body is what carries the minimal identifiers.
var erasureLedgerSchema = []string{
	"erasure_epoch.epoch:bigint",
	"erasure_epoch.lineage_id:bytea",
	"erasure_epoch_completion.epoch:bigint",
	"erasure_epoch_completion.lineage_id:bytea",
	"erasure_outbox.epoch:bigint",
	"erasure_outbox.kind:smallint",
	"erasure_outbox.operation_key:bytea",
	"erasure_outbox.record:bytea",
	"erasure_outbox.seq:bigint",
	"erasure_outbox.stream_id:bytea",
}

// erasureLedgerForbiddenTypes are the column types no ledger column may have.
var erasureLedgerForbiddenTypes = []string{
	"text", "character varying", "character", "uuid", "inet",
	"json", "jsonb", "timestamp", "timestamptz", "xml", "bytea[]",
}

// erasureLedgerConn returns a connection to a freshly migrated database, the one
// the pgtest harness builds by replaying migrations/ from scratch.
func erasureLedgerConn(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if err := conn.Close(cleanupCtx); err != nil {
			t.Logf("close connection: %v", err)
		}
	})
	return conn
}

// erasureLedgerPresent reports whether a public relation exists on conn.
func erasureLedgerPresent(t *testing.T, ctx context.Context, conn *pgx.Conn, relation string) bool {
	t.Helper()
	var present bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, relation).Scan(&present); err != nil {
		t.Fatalf("check relation %s: %v", relation, err)
	}
	return present
}

// erasureLedgerStream draws an opaque replica-stream identity, the identity one
// writer lease carries.
func erasureLedgerStream(t *testing.T) erasureledger.StreamID {
	t.Helper()
	id, err := erasureledger.NewStreamID(rand.Reader)
	if err != nil {
		t.Fatalf("draw stream id: %v", err)
	}
	return id
}

// erasureLedgerOpKey draws an opaque operation key, the idempotency identity.
func erasureLedgerOpKey(t *testing.T) erasureledger.OperationKey {
	t.Helper()
	key, err := erasureledger.NewOperationKey(rand.Reader)
	if err != nil {
		t.Fatalf("draw operation key: %v", err)
	}
	return key
}

// erasureLedgerSelfDelete builds a validated KindMessageCopies record for one
// owner-local copy: the body a delete transaction hands the outbox.
func erasureLedgerSelfDelete(t *testing.T, epoch int64, stream erasureledger.StreamID, seq int64, key erasureledger.OperationKey, ownerID int64, localID int32) erasureledger.Record {
	t.Helper()
	body, err := erasureledger.NewSelfDelete(ownerID, localID)
	if err != nil {
		t.Fatalf("message copies body: %v", err)
	}
	rec, err := erasureledger.NewRecord(erasureledger.KindMessageCopies, epoch, stream, seq, key, body)
	if err != nil {
		t.Fatalf("build record: %v", err)
	}
	return rec
}

// erasureLedgerEncode returns the canonical bytes of rec, the value the record
// column stores.
func erasureLedgerEncode(t *testing.T, rec erasureledger.Record) []byte {
	t.Helper()
	data, err := erasureledger.Encode(rec)
	if err != nil {
		t.Fatalf("encode record: %v", err)
	}
	return data
}

// erasureLedgerParams is the generated insert argument for one record.
func erasureLedgerParams(t *testing.T, rec erasureledger.Record) db.InsertErasureOutboxRecordParams {
	t.Helper()
	return db.InsertErasureOutboxRecordParams{
		OperationKey: rec.OpKey[:],
		Epoch:        rec.Epoch,
		StreamID:     rec.Stream[:],
		Seq:          rec.Seq,
		Kind:         erasureLedgerKind(rec.Kind),
		Record:       erasureLedgerEncode(t, rec),
	}
}

// erasureLedgerLocalID narrows a store-assigned local id to the width the record
// body keeps. The codec rejects a local id above int32 and the schema's own
// envelope bounds reject a zero, so the check below is the width proof, not a
// formality: a value that does not fit fails the test instead of writing a
// record the wire cannot express.
func erasureLedgerLocalID(tb testing.TB, localID int64) int32 {
	tb.Helper()
	if localID < 1 || localID > math.MaxInt32 {
		tb.Fatalf("local id %d is outside the int32 width a record carries", localID)
	}
	return int32(localID) // #nosec G115 -- bounded by the width check above.
}

// erasureLedgerKind maps a codec kind to the smallint the column stores. The
// accepted vocabulary is 1 through 9 and the schema's check refuses a kind outside
// it, so the narrowing is the storage width and never a truncation.
func erasureLedgerKind(kind erasureledger.Kind) int16 {
	return int16(kind) // #nosec G115 -- the accepted vocabulary is 1..9, the same bound the column checks.
}

// erasureLedgerCounts reads the three table totals through the generated access,
// which is what every emptiness claim below rests on.
func erasureLedgerCounts(t *testing.T, ctx context.Context, q *db.Queries) (outbox, markers, completions int64) {
	t.Helper()
	outbox, err := q.ErasureOutboxTotal(ctx)
	if err != nil {
		t.Fatalf("outbox total: %v", err)
	}
	markers, err = q.ErasureEpochMarkerTotal(ctx)
	if err != nil {
		t.Fatalf("epoch marker total: %v", err)
	}
	completions, err = q.ErasureEpochCompletionTotal(ctx)
	if err != nil {
		t.Fatalf("epoch completion total: %v", err)
	}
	return outbox, markers, completions
}

// erasureLedgerRawInsert writes one outbox row straight to SQL, so the constraint
// under test is the schema's and not a wrapper's.
func erasureLedgerRawInsert(ctx context.Context, conn *pgx.Conn, operationKey, stream []byte, epoch, seq int64, kind int16, record []byte) error {
	_, err := conn.Exec(ctx, `
		INSERT INTO erasure_outbox (operation_key, epoch, stream_id, seq, kind, record)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		operationKey, epoch, stream, seq, kind, record)
	return err
}

// TestErasureLedgerSchemaLandsInertOnFreshSchema replays the whole migration list
// onto a disposable Postgres through the pgtest harness and asserts the three
// tables arrived, exactly as specified, with nothing attached to them: no
// non-internal trigger (so no hidden writer), and no column carrying a clock,
// text, or an owner identity.
func TestErasureLedgerSchemaLandsInertOnFreshSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fresh schema: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	for _, table := range erasureLedgerTables {
		if !erasureLedgerPresent(t, ctx, conn, table) {
			t.Errorf("table %s missing from the freshly migrated schema", table)
		}
	}

	rows, err := conn.Query(ctx, `
		SELECT table_name || '.' || column_name || ':' || data_type
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = ANY($1)
		ORDER BY table_name, column_name`, erasureLedgerTables)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	defer rows.Close()
	got := make([]string, 0, len(erasureLedgerSchema))
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		got = append(got, column)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	want := append([]string(nil), erasureLedgerSchema...)
	slices.Sort(got)
	slices.Sort(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ledger schema columns = [%s], want [%s]", strings.Join(got, ","), strings.Join(want, ","))
	}

	// The forbidden types, asserted on top of the exact match so a rename cannot
	// smuggle one in: a timestamp column is a clock a writer could order the ledger
	// by, and text is how a phone, a name, or a message body would reach a table
	// whose rows are identifiers and opaque bytes.
	for _, column := range got {
		_, typePart, _ := strings.Cut(column, ":")
		if slices.Contains(erasureLedgerForbiddenTypes, typePart) {
			t.Errorf("column %s has type %s: the ledger envelope carries no clock, no text and no free-form value", column, typePart)
		}
	}

	// No non-internal trigger and no rule on any of the three tables. Nothing
	// attached to them can write a ledger row behind a caller's back, which is what
	// makes the live-path emptiness claim below a fact of the schema. A
	// foreign-key constraint trigger is internal and is not counted here.
	var triggers int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)::bigint
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		WHERE c.relname = ANY($1) AND NOT t.tgisinternal`, erasureLedgerTables).Scan(&triggers); err != nil {
		t.Fatalf("read triggers: %v", err)
	}
	if triggers != 0 {
		t.Errorf("%d non-internal trigger(s) on the ledger tables, want 0: a trigger is a hidden writer", triggers)
	}

	// The application shape still opens against the new schema: these tables are
	// additive, and no existing writer changed.
	opened, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store on fresh schema: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// The generated access runs against the freshly migrated schema, and
	// arrives empty: no live path wrote anything.
	s := openStore(t, dsn)
	outbox, markers, completions := erasureLedgerCounts(t, ctx, db.New(store.StorePool(s)))
	if outbox != 0 || markers != 0 || completions != 0 {
		t.Errorf("ledger totals on a fresh schema = outbox %d markers %d completions %d, want 0/0/0",
			outbox, markers, completions)
	}
}

// TestErasureLedgerMigrationReplaysOnDisposableSchema builds a disposable database
// carrying every migration that sorts before this slice's, then applies this
// slice's file to it. The tables must be absent before and complete after, and the
// file must not be re-runnable: Atlas applies each file exactly once, so a guarded
// file is a file that silently does nothing when it is replayed by hand.
func TestErasureLedgerMigrationReplaysOnDisposableSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, _ := profilePhotoDatabaseBefore(t, ctx, erasureLedgerMigration)

	for _, table := range erasureLedgerTables {
		if erasureLedgerPresent(t, ctx, conn, table) {
			t.Fatalf("table %s exists before its migration", table)
		}
	}

	body := profilePhotoReadMigration(t, erasureLedgerMigration)
	if _, err := conn.Exec(ctx, body); err != nil {
		t.Fatalf("apply %s: %v", erasureLedgerMigration, err)
	}

	for _, table := range erasureLedgerTables {
		if !erasureLedgerPresent(t, ctx, conn, table) {
			t.Errorf("table %s missing after the migration", table)
		}
	}

	// The contract as named schema objects: the idempotency interlock, the
	// completion-to-marker restriction, and the three ordering keys.
	names := []string{
		"erasure_outbox_pkey",
		"erasure_outbox_operation_key_unique",
		"erasure_epoch_pkey",
		"erasure_epoch_completion_pkey",
		"erasure_epoch_completion_marker_exists",
	}
	rows, err := conn.Query(ctx, `
		SELECT conname, contype
		FROM pg_constraint
		WHERE connamespace = 'public'::regnamespace AND conname = ANY($1)
		ORDER BY conname`, names)
	if err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	defer rows.Close()
	found := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan constraint: %v", err)
		}
		found[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read constraints: %v", err)
	}
	for _, want := range []struct{ name, typ string }{
		{"erasure_outbox_pkey", "p"},
		{"erasure_outbox_operation_key_unique", "u"},
		{"erasure_epoch_pkey", "p"},
		{"erasure_epoch_completion_pkey", "p"},
		{"erasure_epoch_completion_marker_exists", "f"},
	} {
		typ, ok := found[want.name]
		if !ok {
			t.Errorf("constraint %s absent after the migration", want.name)
			continue
		}
		if typ != want.typ {
			t.Errorf("constraint %s type %q, want %q", want.name, typ, want.typ)
		}
	}

	// A second apply must fail loudly. The repo convention is no IF NOT EXISTS
	// guards, because Atlas records which files it applied.
	if _, err := conn.Exec(ctx, body); err == nil {
		t.Errorf("applying %s twice succeeded, want a loud failure: the file carries idempotency guards", erasureLedgerMigration)
	}
}

// TestErasureOutboxCommitsWithMessageDeletion is the outbox acceptance criterion:
// a ledger record written in the same transaction as a message deletion commits
// with it, carrying the epoch, the replica stream, the sequence, the accepted kind
// and the unique opaque operation key.
//
// The transaction is composed by the test, deliberately: the live delete path does
// not write here in this slice, and what is under test is that the pair CAN commit
// atomically, on the live delete statement, against the real schema.
func TestErasureOutboxCommitsWithMessageDeletion(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551453101")
	b := mustUser(t, s, "+15551453102")
	sender := send(t, s, a, b, "erase this", 1)

	rec := erasureLedgerSelfDelete(t, 4, erasureLedgerStream(t), 1, erasureLedgerOpKey(t), a.ID, erasureLedgerLocalID(t, sender.LocalID))

	pool := store.StorePool(s)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := db.New(tx)

	// The live delete statement, unmodified, inside the transaction that carries
	// the record.
	changed, err := qtx.SetDeleted(ctx, db.SetDeletedParams{OwnerID: a.ID, LocalID: sender.LocalID})
	if err != nil {
		t.Fatalf("set deleted: %v", err)
	}
	if changed != 1 {
		t.Fatalf("SetDeleted changed %d rows, want 1", changed)
	}
	if err := qtx.InsertErasureOutboxRecord(ctx, erasureLedgerParams(t, rec)); err != nil {
		t.Fatalf("insert outbox record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if !msgAt(t, s, a.ID, sender.LocalID).Deleted {
		t.Fatal("owner copy not deleted by the committed transaction")
	}
	// The authorized copy set is unchanged by the presence of these tables: this is
	// a self-delete, so the peer's mirror stays live.
	if msgAt(t, s, b.ID, sender.PeerLocalID).Deleted {
		t.Error("peer copy deleted by a self-delete whose record carries only the caller's copy")
	}

	q := db.New(pool)
	row, err := q.ErasureOutboxByOperationKey(ctx, rec.OpKey[:])
	if err != nil {
		t.Fatalf("read outbox record: %v", err)
	}
	if !bytes.Equal(row.OperationKey, rec.OpKey[:]) {
		t.Errorf("operation key %x, want %x", row.OperationKey, rec.OpKey[:])
	}
	if row.Epoch != rec.Epoch {
		t.Errorf("epoch %d, want %d", row.Epoch, rec.Epoch)
	}
	if !bytes.Equal(row.StreamID, rec.Stream[:]) {
		t.Errorf("stream %x, want %x", row.StreamID, rec.Stream[:])
	}
	if row.Seq != rec.Seq {
		t.Errorf("seq %d, want %d", row.Seq, rec.Seq)
	}
	if row.Kind != erasureLedgerKind(rec.Kind) {
		t.Errorf("kind %d, want %d", row.Kind, rec.Kind)
	}

	// The envelope columns are a projection of the stored canonical record, not a
	// second source of truth: decoded from the bytes, they agree with the columns.
	decoded, err := erasureledger.Decode(row.Record)
	if err != nil {
		t.Fatalf("decode stored record: %v", err)
	}
	if decoded.Epoch != row.Epoch || decoded.Stream != rec.Stream || decoded.Seq != row.Seq ||
		decoded.OpKey != rec.OpKey || decoded.Kind != rec.Kind {
		t.Errorf("stored columns and stored record disagree: columns (kind %d epoch %d seq %d), decoded (kind %d epoch %d seq %d)",
			row.Kind, row.Epoch, row.Seq, decoded.Kind, decoded.Epoch, decoded.Seq)
	}
	body, ok := decoded.Payload.(erasureledger.MessageCopies)
	if !ok {
		t.Fatalf("stored payload is %T, want erasureledger.MessageCopies", decoded.Payload)
	}
	if len(body.Copies) != 1 || body.Copies[0] != (erasureledger.MessageCopy{OwnerID: a.ID, LocalID: erasureLedgerLocalID(t, sender.LocalID)}) {
		t.Errorf("stored copy set = %+v, want the one copy the transaction deleted", body.Copies)
	}
	// Canonical form: the stored bytes re-encode to themselves, so a replay of this
	// record is byte-identical to the record that was written.
	if reencoded := erasureLedgerEncode(t, decoded); !bytes.Equal(reencoded, row.Record) {
		t.Error("stored record does not re-encode to the stored bytes: the record column is not canonical")
	}

	// The enumeration path returns the record for its (epoch, stream) in seq order.
	walk, err := q.ErasureOutboxStreamRecords(ctx, db.ErasureOutboxStreamRecordsParams{Epoch: rec.Epoch, StreamID: rec.Stream[:]})
	if err != nil {
		t.Fatalf("walk stream: %v", err)
	}
	if len(walk) != 1 || !bytes.Equal(walk[0].OperationKey, rec.OpKey[:]) {
		t.Fatalf("stream walk = %+v, want the one record", walk)
	}
}

// TestErasureOutboxRollbackLeavesNoDeletionAndNoRecord is the other half of the
// atomicity criterion: a transaction that rolls back leaves the message deletion
// and the ledger record both absent, so a ledger row can never describe an erasure
// that did not happen.
func TestErasureOutboxRollbackLeavesNoDeletionAndNoRecord(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551453111")
	b := mustUser(t, s, "+15551453112")
	sender := send(t, s, a, b, "keep this", 1)

	rec := erasureLedgerSelfDelete(t, 4, erasureLedgerStream(t), 1, erasureLedgerOpKey(t), a.ID, erasureLedgerLocalID(t, sender.LocalID))

	pool := store.StorePool(s)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	qtx := db.New(tx)
	if _, err := qtx.SetDeleted(ctx, db.SetDeletedParams{OwnerID: a.ID, LocalID: sender.LocalID}); err != nil {
		t.Fatalf("set deleted: %v", err)
	}
	if err := qtx.InsertErasureOutboxRecord(ctx, erasureLedgerParams(t, rec)); err != nil {
		t.Fatalf("insert outbox record: %v", err)
	}

	// Inside the transaction both halves are visible: they are one unit, and the
	// rollback that follows removes both, not one.
	var deleted bool
	if err := tx.QueryRow(ctx,
		`SELECT deleted FROM messages WHERE owner_id = $1 AND local_id = $2`,
		a.ID, sender.LocalID).Scan(&deleted); err != nil {
		t.Fatalf("read inside the transaction: %v", err)
	}
	if !deleted {
		t.Fatal("owner copy not deleted inside the transaction that carries the record")
	}
	outbox, markers, completions := erasureLedgerCounts(t, ctx, qtx)
	if outbox != 1 {
		t.Fatalf("outbox rows inside the transaction = %d, want 1", outbox)
	}
	if markers != 0 || completions != 0 {
		t.Fatalf("epoch markers inside the outbox transaction = %d/%d, want 0/0", markers, completions)
	}

	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if msgAt(t, s, a.ID, sender.LocalID).Deleted {
		t.Error("owner copy stayed deleted after the rollback")
	}
	q := db.New(pool)
	outbox, markers, completions = erasureLedgerCounts(t, ctx, q)
	if outbox != 0 || markers != 0 || completions != 0 {
		t.Errorf("ledger totals after the rollback = outbox %d markers %d completions %d, want 0/0/0",
			outbox, markers, completions)
	}
	if _, err := q.ErasureOutboxByOperationKey(ctx, rec.OpKey[:]); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("read by operation key after the rollback = %v, want no rows", err)
	}
}

// TestErasureOutboxDuplicateOperationKeyIsRefusedNotApplied pins the idempotency
// interlock. The operation key is what makes a retried write the same record, so a
// repeat is refused by the database and the committed record stays untouched, byte
// for byte, envelope and body. The retry here carries a different epoch, stream,
// sequence and copy set from the committed record, so the refusal can only come
// from the operation key, and a silent replace would be visible as a changed row.
func TestErasureOutboxDuplicateOperationKeyIsRefusedNotApplied(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551453121")
	b := mustUser(t, s, "+15551453122")
	first := send(t, s, a, b, "committed", 1)
	second := send(t, s, a, b, "retry", 2)

	key := erasureLedgerOpKey(t)
	stream := erasureLedgerStream(t)
	committed := erasureLedgerSelfDelete(t, 5, stream, 1, key, a.ID, erasureLedgerLocalID(t, first.LocalID))

	pool := store.StorePool(s)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	qtx := db.New(tx)
	if _, err := qtx.SetDeleted(ctx, db.SetDeletedParams{OwnerID: a.ID, LocalID: first.LocalID}); err != nil {
		t.Fatalf("set deleted: %v", err)
	}
	if err := qtx.InsertErasureOutboxRecord(ctx, erasureLedgerParams(t, committed)); err != nil {
		t.Fatalf("insert committed record: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	q := db.New(pool)
	before, err := q.ErasureOutboxByOperationKey(ctx, key[:])
	if err != nil {
		t.Fatalf("read committed record: %v", err)
	}

	// The retry: the same operation key, everything else different, in a
	// transaction that also deletes the second copy.
	other := erasureLedgerStream(t)
	retry := erasureLedgerSelfDelete(t, 9, other, 7, key, a.ID, erasureLedgerLocalID(t, second.LocalID))
	retryTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin retry: %v", err)
	}
	rqtx := db.New(retryTx)
	if _, err := rqtx.SetDeleted(ctx, db.SetDeletedParams{OwnerID: a.ID, LocalID: second.LocalID}); err != nil {
		t.Fatalf("retry set deleted: %v", err)
	}
	err = rqtx.InsertErasureOutboxRecord(ctx, erasureLedgerParams(t, retry))
	if !profilePhotoViolation(err, "23505", "erasure_outbox_operation_key_unique") {
		t.Fatalf("retry insert = %v, want an erasure_outbox_operation_key_unique violation", err)
	}
	if err := retryTx.Rollback(ctx); err != nil {
		t.Fatalf("rollback retry: %v", err)
	}

	// The committed record is exactly as it was, and the retry's own ordering
	// identity never arrived.
	after, err := q.ErasureOutboxByOperationKey(ctx, key[:])
	if err != nil {
		t.Fatalf("re-read committed record: %v", err)
	}
	if after.Epoch != before.Epoch || after.Seq != before.Seq || after.Kind != before.Kind ||
		!bytes.Equal(after.Record, before.Record) || !bytes.Equal(after.StreamID, before.StreamID) {
		t.Errorf("committed record changed: before (epoch %d stream %x seq %d kind %d), after (epoch %d stream %x seq %d kind %d)",
			before.Epoch, before.StreamID, before.Seq, before.Kind, after.Epoch, after.StreamID, after.Seq, after.Kind)
	}
	if after.Epoch != 5 || after.Seq != 1 || !bytes.Equal(after.StreamID, stream[:]) {
		t.Errorf("committed record is (epoch %d seq %d), want the original (epoch 5 seq 1)", after.Epoch, after.Seq)
	}
	walk, err := q.ErasureOutboxStreamRecords(ctx, db.ErasureOutboxStreamRecordsParams{Epoch: 9, StreamID: other[:]})
	if err != nil {
		t.Fatalf("walk the retried stream: %v", err)
	}
	if len(walk) != 0 {
		t.Errorf("the retry arrived as %+v, want nothing", walk)
	}
	outbox, _, _ := erasureLedgerCounts(t, ctx, q)
	if outbox != 1 {
		t.Errorf("outbox rows = %d, want the single committed record", outbox)
	}
	if msgAt(t, s, a.ID, second.LocalID).Deleted {
		t.Error("the rejected retry deleted its copy: the refusal did not take the whole transaction")
	}
	if !msgAt(t, s, a.ID, first.LocalID).Deleted {
		t.Error("the committed deletion stopped being committed")
	}
}

// TestErasureOutboxEnvelopeFailsClosed asserts each storage bound that turns the
// codec's envelope contract into a schema fact: epoch and sequence that start at
// 1, fixed-length opaque identities, a kind inside the accepted vocabulary, a
// record within the codec's bound, and one record per (epoch, stream, seq).
func TestErasureOutboxEnvelopeFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)

	stream := erasureLedgerStream(t)
	key := erasureLedgerOpKey(t)
	rec := erasureLedgerSelfDelete(t, 4, stream, 1, key, 7, 1)
	valid := erasureLedgerEncode(t, rec)

	cases := []struct {
		name         string
		operationKey []byte
		stream       []byte
		epoch        int64
		seq          int64
		kind         int16
		record       []byte
		constraint   string
	}{
		{name: "epoch below 1", operationKey: key[:], stream: stream[:], epoch: 0, seq: 1, kind: 3,
			record: valid, constraint: "erasure_outbox_epoch_check"},
		{name: "seq below 1", operationKey: key[:], stream: stream[:], epoch: 4, seq: 0, kind: 3,
			record: valid, constraint: "erasure_outbox_seq_check"},
		{name: "short operation key", operationKey: key[:15], stream: stream[:], epoch: 4, seq: 1, kind: 3,
			record: valid, constraint: "erasure_outbox_operation_key_check"},
		{name: "empty operation key", operationKey: []byte{}, stream: stream[:], epoch: 4, seq: 1, kind: 3,
			record: valid, constraint: "erasure_outbox_operation_key_check"},
		{name: "short stream identity", operationKey: key[:], stream: stream[:15], epoch: 4, seq: 1, kind: 3,
			record: valid, constraint: "erasure_outbox_stream_id_check"},
		{name: "kind below the vocabulary", operationKey: key[:], stream: stream[:], epoch: 4, seq: 1, kind: 0,
			record: valid, constraint: "erasure_outbox_kind_check"},
		{name: "kind past the vocabulary", operationKey: key[:], stream: stream[:], epoch: 4, seq: 1, kind: 10,
			record: valid, constraint: "erasure_outbox_kind_check"},
		{name: "empty record", operationKey: key[:], stream: stream[:], epoch: 4, seq: 1, kind: 3,
			record: []byte{}, constraint: "erasure_outbox_record_check"},
		{name: "record past the codec bound", operationKey: key[:], stream: stream[:], epoch: 4, seq: 1, kind: 3,
			record: bytes.Repeat([]byte{0x42}, 65537), constraint: "erasure_outbox_record_check"},
	}
	for _, tc := range cases {
		err := erasureLedgerRawInsert(ctx, conn, tc.operationKey, tc.stream, tc.epoch, tc.seq, tc.kind, tc.record)
		if !profilePhotoViolation(err, "23514", tc.constraint) {
			t.Errorf("%s: insert error = %v, want the %s check violation", tc.name, err, tc.constraint)
		}
	}

	// The ordering identity: one record per (epoch, stream, seq).
	if err := erasureLedgerRawInsert(ctx, conn, key[:], stream[:], 4, 1, 3, valid); err != nil {
		t.Fatalf("insert valid record: %v", err)
	}
	second := erasureLedgerOpKey(t)
	err := erasureLedgerRawInsert(ctx, conn, second[:], stream[:], 4, 1, 3, valid)
	if !profilePhotoViolation(err, "23505", "erasure_outbox_pkey") {
		t.Errorf("duplicate (epoch, stream, seq) error = %v, want the erasure_outbox_pkey violation", err)
	}

	// The sequence is per (epoch, stream): two streams of one epoch are independent
	// writers, and each starts its own sequence at 1.
	independent := erasureLedgerStream(t)
	if err := erasureLedgerRawInsert(ctx, conn, second[:], independent[:], 4, 1, 3, valid); err != nil {
		t.Errorf("same seq on a second stream = %v, want acceptance: sequences are per (epoch, stream)", err)
	}

	outbox, _, _ := erasureLedgerCounts(t, ctx, db.New(conn))
	if outbox != 2 {
		t.Errorf("outbox rows after the accepted writes = %d, want 2", outbox)
	}
}

// TestErasureOutboxStreamWalkRevealsSeqGap is the completeness half of the
// ordering contract: a walk of one (epoch, stream) in seq order shows a gap as a
// gap, and no schema object fills it in. There is no sequence, no column default
// and no trigger on this table, which is what keeps the gap observable at all.
func TestErasureOutboxStreamWalkRevealsSeqGap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	q := db.New(conn)

	stream := erasureLedgerStream(t)
	for _, seq := range []int64{1, 3} {
		rec := erasureLedgerSelfDelete(t, 6, stream, seq, erasureLedgerOpKey(t), 11, erasureLedgerLocalID(t, seq))
		if err := q.InsertErasureOutboxRecord(ctx, erasureLedgerParams(t, rec)); err != nil {
			t.Fatalf("insert seq %d: %v", seq, err)
		}
	}

	walk, err := q.ErasureOutboxStreamRecords(ctx, db.ErasureOutboxStreamRecordsParams{Epoch: 6, StreamID: stream[:]})
	if err != nil {
		t.Fatalf("walk stream: %v", err)
	}
	seqs := make([]int64, 0, len(walk))
	for _, row := range walk {
		seqs = append(seqs, row.Seq)
	}
	if !slices.Equal(seqs, []int64{1, 3}) {
		t.Fatalf("stream walk sequences = %v, want [1 3] so the gap at 2 is visible", seqs)
	}

	// No column default on this table hands out a seq.
	var defaults int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)::bigint
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'erasure_outbox'
		  AND column_default IS NOT NULL`).Scan(&defaults); err != nil {
		t.Fatalf("read defaults: %v", err)
	}
	if defaults != 0 {
		t.Errorf("erasure_outbox has %d column default(s), want 0: a default would fill the gap a reader looks for", defaults)
	}
}

// TestErasureEpochMarkersSeparateCompletedLineageFromRestoredLineage is the
// lineage criterion. Epoch numbers are shared across lineages by construction:
// restoring an older lineage re-runs numbers the replaced lineage already used.
// Completion is a fact about one (epoch, lineage) pair, so the persisted markers
// must answer for the lineage asked about, and a shared number must not lend the
// other lineage's completion to it.
func TestErasureEpochMarkersSeparateCompletedLineageFromRestoredLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	q := db.New(conn)

	restored := bytes.Repeat([]byte{0x01}, erasureledger.LineageIDLen)
	replaced := bytes.Repeat([]byte{0x02}, erasureledger.LineageIDLen)

	// Epoch 7 of the replaced lineage finished long ago; epoch 7 of the restored
	// lineage has only been fenced.
	if err := q.InsertErasureEpochMarker(ctx, db.InsertErasureEpochMarkerParams{Epoch: 7, LineageID: replaced}); err != nil {
		t.Fatalf("establish epoch 7 of the replaced lineage: %v", err)
	}
	if err := q.InsertErasureEpochCompletion(ctx, db.InsertErasureEpochCompletionParams{Epoch: 7, LineageID: replaced}); err != nil {
		t.Fatalf("complete epoch 7 of the replaced lineage: %v", err)
	}
	if err := q.InsertErasureEpochMarker(ctx, db.InsertErasureEpochMarkerParams{Epoch: 7, LineageID: restored}); err != nil {
		t.Fatalf("establish epoch 7 of the restored lineage: %v", err)
	}

	replacedState, err := q.ErasureEpochLineageState(ctx, db.ErasureEpochLineageStateParams{Epoch: 7, LineageID: replaced})
	if err != nil {
		t.Fatalf("state of the replaced lineage: %v", err)
	}
	if !replacedState.Completed {
		t.Error("epoch 7 of the replaced lineage is not completed, want completed")
	}

	restoredState, err := q.ErasureEpochLineageState(ctx, db.ErasureEpochLineageStateParams{Epoch: 7, LineageID: restored})
	if err != nil {
		t.Fatalf("state of the restored lineage: %v", err)
	}
	if restoredState.Completed {
		t.Error("epoch 7 of the restored lineage reads as completed off the other lineage's completion")
	}

	// The shared number is visible as a shared number: two lineages, one epoch, one
	// completion between them.
	lineages, err := q.ErasureEpochLineagesForEpoch(ctx, 7)
	if err != nil {
		t.Fatalf("lineages of epoch 7: %v", err)
	}
	if len(lineages) != 2 {
		t.Fatalf("epoch 7 has %d lineages, want 2 (the replaced and the restored)", len(lineages))
	}
	_, markers, completions := erasureLedgerCounts(t, ctx, q)
	if markers != 2 {
		t.Errorf("epoch markers = %d, want 2", markers)
	}
	if completions != 1 {
		t.Errorf("completions = %d, want 1: the epoch number carries no completion of its own", completions)
	}

	// The restored lineage gets its own completion, and that write leaves the other
	// lineage's evidence alone.
	if err := q.InsertErasureEpochCompletion(ctx, db.InsertErasureEpochCompletionParams{Epoch: 7, LineageID: restored}); err != nil {
		t.Fatalf("complete epoch 7 of the restored lineage: %v", err)
	}
	restoredState, err = q.ErasureEpochLineageState(ctx, db.ErasureEpochLineageStateParams{Epoch: 7, LineageID: restored})
	if err != nil {
		t.Fatalf("state of the restored lineage after completion: %v", err)
	}
	if !restoredState.Completed {
		t.Error("epoch 7 of the restored lineage is not completed after its own completion row")
	}
	var replacedCount int64
	if err := conn.QueryRow(ctx, `
		SELECT count(*)::bigint FROM erasure_epoch_completion
		WHERE epoch = 7 AND lineage_id = $1`, replaced).Scan(&replacedCount); err != nil {
		t.Fatalf("count the replaced lineage's completions: %v", err)
	}
	if replacedCount != 1 {
		t.Errorf("epoch 7 completions for the replaced lineage = %d, want 1", replacedCount)
	}
}

// TestErasureEpochCompletionRestrictsItsMarker asserts the completion interlock:
// completion evidence cannot exist for a pair that was never established, and the
// marker a completion rests on cannot be removed from under it. The bounds on the
// pair (epoch at least 1, a fixed-length opaque lineage) and the single
// completion per pair are asserted with it.
func TestErasureEpochCompletionRestrictsItsMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := erasureLedgerConn(t, ctx)
	q := db.New(conn)

	lineage := bytes.Repeat([]byte{0x03}, erasureledger.LineageIDLen)
	_, markers, completions := erasureLedgerCounts(t, ctx, q)
	if markers != 0 || completions != 0 {
		t.Fatalf("tables start at %d markers and %d completions, want 0/0", markers, completions)
	}

	// A completion with no established marker is refused, so a restore cannot arrive
	// holding completion evidence for an epoch it never fenced.
	_, err := conn.Exec(ctx, `INSERT INTO erasure_epoch_completion (epoch, lineage_id) VALUES ($1, $2)`, 8, lineage)
	if !profilePhotoViolation(err, "23503", "erasure_epoch_completion_marker_exists") {
		t.Fatalf("orphan completion = %v, want an erasure_epoch_completion_marker_exists violation", err)
	}

	if err := q.InsertErasureEpochMarker(ctx, db.InsertErasureEpochMarkerParams{Epoch: 8, LineageID: lineage}); err != nil {
		t.Fatalf("establish epoch 8: %v", err)
	}
	if err := q.InsertErasureEpochCompletion(ctx, db.InsertErasureEpochCompletionParams{Epoch: 8, LineageID: lineage}); err != nil {
		t.Fatalf("complete epoch 8: %v", err)
	}

	// Removing the marker under a completion is restricted, not cascading: a cascade
	// would let one row's deletion erase completed-lineage evidence.
	_, err = conn.Exec(ctx, `DELETE FROM erasure_epoch WHERE epoch = 8 AND lineage_id = $1`, lineage)
	if !profilePhotoViolation(err, "23503", "erasure_epoch_completion_marker_exists") {
		t.Fatalf("deleting the restricted marker = %v, want an erasure_epoch_completion_marker_exists violation", err)
	}

	// The bounds on the pair.
	_, err = conn.Exec(ctx, `INSERT INTO erasure_epoch (epoch, lineage_id) VALUES ($1, $2)`, 0, lineage)
	if !profilePhotoViolation(err, "23514", "erasure_epoch_epoch_check") {
		t.Fatalf("epoch 0 marker = %v, want the epoch bound", err)
	}
	_, err = conn.Exec(ctx, `INSERT INTO erasure_epoch (epoch, lineage_id) VALUES ($1, $2)`, 9, lineage[:15])
	if !profilePhotoViolation(err, "23514", "erasure_epoch_lineage_id_check") {
		t.Fatalf("short lineage marker = %v, want the lineage length bound", err)
	}

	// A pair completes once: a repeated completion is refused rather than counted
	// twice, so completion evidence has no multiplicity to reason about.
	_, err = conn.Exec(ctx, `INSERT INTO erasure_epoch_completion (epoch, lineage_id) VALUES ($1, $2)`, 8, lineage)
	if !profilePhotoViolation(err, "23505", "erasure_epoch_completion_pkey") {
		t.Fatalf("repeated completion = %v, want the primary key violation", err)
	}
}

// TestErasureLedgerTablesStayEmptyThroughLivePaths is the live half of the
// inertness claim. Every path that deletes or erases — a self-delete, a peer
// revoke, a channel post delete, the media eraser sweep — leaves all three tables
// empty, while its own delivery contract (the copies it is authorized to delete,
// the resulting pts, the published delete events) is unchanged. The ledger rows the
// other tests here write are test-only, and this is what distinguishes them from
// capture: no live path produces one.
func TestErasureLedgerTablesStayEmptyThroughLivePaths(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	q := db.New(store.StorePool(s))

	assertEmpty := func(what string) {
		t.Helper()
		outbox, markers, completions := erasureLedgerCounts(t, ctx, q)
		if outbox != 0 || markers != 0 || completions != 0 {
			t.Fatalf("%s wrote ledger rows: outbox %d markers %d completions %d, want 0/0/0",
				what, outbox, markers, completions)
		}
	}
	assertEmpty("an idle store")

	// A peer revoke: both copies go, both pts advance, one delete event each.
	a := mustUser(t, s, "+15551453201")
	b := mustUser(t, s, "+15551453202")
	revoked := send(t, s, a, b, "revoke me", 1)
	perOwner, err := s.DeleteMessages(ctx, a.ID, []int64{revoked.LocalID}, true)
	if err != nil {
		t.Fatalf("revoke delete: %v", err)
	}
	if perOwner[a.ID] != 2 || perOwner[b.ID] != 2 {
		t.Fatalf("revoke pts = %+v, want a and b at 2", perOwner)
	}
	if !msgAt(t, s, a.ID, revoked.LocalID).Deleted || !msgAt(t, s, b.ID, revoked.PeerLocalID).Deleted {
		t.Fatal("revoke did not delete both copies")
	}
	for _, u := range []int64{a.ID, b.ID} {
		events, err := s.EventsSince(ctx, u, 1)
		if err != nil {
			t.Fatalf("events for %d: %v", u, err)
		}
		if len(events) != 1 || events[0].Type != store.EventDelete {
			t.Fatalf("owner %d events = %+v, want one delete event", u, events)
		}
	}
	assertEmpty("a revoke delete")

	// A self-delete: the caller's copy only, the peer's pts untouched.
	self := send(t, s, a, b, "self delete", 2)
	perOwner, err = s.DeleteMessages(ctx, a.ID, []int64{self.LocalID}, false)
	if err != nil {
		t.Fatalf("self delete: %v", err)
	}
	if len(perOwner) != 1 || perOwner[a.ID] != 4 {
		t.Fatalf("self delete pts = %+v, want a alone at 4", perOwner)
	}
	if msgAt(t, s, b.ID, self.PeerLocalID).Deleted {
		t.Error("self delete took the peer's copy")
	}
	assertEmpty("a self delete")

	// A channel post delete, in the channel's own id space.
	author := mustUser(t, s, "+15551453203")
	ch := mustChannel(t, s, author.ID, "news").ID
	channelPost, pts := post(t, s, ch, author.ID, "post", 1)
	if pts != 2 {
		t.Fatalf("channel post pts = %d, want 2", pts)
	}
	chanPts, count, err := s.DeleteChannelMessages(ctx, ch, author.ID, []int64{channelPost.LocalID})
	if err != nil {
		t.Fatalf("channel delete: %v", err)
	}
	if count != 1 || chanPts != 3 {
		t.Fatalf("channel delete = %d rows at pts %d, want 1 row at pts 3", count, chanPts)
	}
	assertEmpty("a channel post delete")

	// The media eraser: the row and the bytes of a file whose every copy is deleted
	// go away, and the ledger stays empty. A sweep is the closest thing in this
	// schema to a destructive erasure, and it is the path a future writer is most
	// likely to be tempted onto.
	owner := mustUser(t, s, "+15551453204")
	peer := mustUser(t, s, "+15551453205")
	file := storedFileWithBytes(t, s, owner.ID)
	deletedBothSides(t, s, owner, peer, file.ID, 1)
	counts := sweep(t, s, future())
	if counts.Erased != 1 {
		t.Fatalf("sweep erased %d files, want 1", counts.Erased)
	}
	if rowPresent(t, s, file.ID) || blobPresent(t, s, file.ID) {
		t.Error("sweep left the erased file's row or bytes behind")
	}
	assertEmpty("the media eraser sweep")
}

// TestErasureLedgerPersistenceHasNoRuntimeCaller enforces the boundary this slice
// is built inside: the generated outbox and epoch access has no caller in
// production code. Its only callers are the acceptance tests in this package, so
// the tables are writable from a test and from nothing else, and no live path can
// capture a record by accident.
func TestErasureLedgerPersistenceHasNoRuntimeCaller(t *testing.T) {
	t.Parallel()

	runtime := erasureLedgerCallSites(t, false)
	if len(runtime) > 0 {
		t.Fatalf("ledger persistence has runtime callers, want none (this slice is inert):\n  %s",
			strings.Join(runtime, "\n  "))
	}

	if tests := erasureLedgerCallSites(t, true); len(tests) == 0 {
		t.Fatal("the generated ledger access has no caller at all, not even in tests")
	}
}

// erasureLedgerCallSites returns "file:line calls Name" for every call to a
// generated ledger query, in the non-test Go files under internal/ and cmd/
// (includeTests false) or in the test files (includeTests true). A method
// declaration is not a call, so the generated declarations are not call sites.
func erasureLedgerCallSites(t *testing.T, includeTests bool) []string {
	t.Helper()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	var sites []string
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") {
				return nil
			}
			if strings.HasSuffix(name, "_test.go") != includeTests {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parse %s: %w", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if slices.Contains(erasureLedgerQueries, sel.Sel.Name) {
					pos := fset.Position(call.Pos())
					sites = append(sites, fmt.Sprintf("%s:%d calls %s",
						strings.TrimPrefix(pos.Filename, root+string(os.PathSeparator)), pos.Line, sel.Sel.Name))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return sites
}
