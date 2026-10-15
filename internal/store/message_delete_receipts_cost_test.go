package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// This recorder forwards to real PostgreSQL. EXPLAIN uses the exact generated
// lookup/count/confirmation statement and arguments, rather than a hand-copied substitute.
type messageDeleteReceiptQueryRecorder struct {
	db.DBTX

	statement string
	args      []any
}

func (r *messageDeleteReceiptQueryRecorder) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	r.statement, r.args = statement, args
	return r.DBTX.QueryRow(ctx, statement, args...)
}

type messageDeleteReceiptStorageSize struct {
	heap, indexes, total int64
}

func messageDeleteReceiptStorageOccupancy(t *testing.T, ctx context.Context, conn *pgx.Conn, table string) messageDeleteReceiptStorageSize {
	t.Helper()
	var size messageDeleteReceiptStorageSize
	if err := conn.QueryRow(ctx, `SELECT pg_relation_size($1::regclass),
		pg_indexes_size($1::regclass), pg_total_relation_size($1::regclass)`, table).
		Scan(&size.heap, &size.indexes, &size.total); err != nil {
		t.Fatalf("read storage occupancy: %v", err)
	}
	return size
}

type messageDeleteReceiptPlan struct {
	NodeType         string                     `json:"Node Type"`
	RelationName     string                     `json:"Relation Name"`
	IndexName        string                     `json:"Index Name"`
	ActualRows       float64                    `json:"Actual Rows"`
	ActualLoops      float64                    `json:"Actual Loops"`
	RowsRemoved      float64                    `json:"Rows Removed by Filter"`
	HeapFetches      int64                      `json:"Heap Fetches"`
	SharedHitBlocks  int64                      `json:"Shared Hit Blocks"`
	SharedReadBlocks int64                      `json:"Shared Read Blocks"`
	Plans            []messageDeleteReceiptPlan `json:"Plans"`
}

type messageDeleteReceiptPlanReport struct {
	Plan          messageDeleteReceiptPlan `json:"Plan"`
	PlanningTime  float64                  `json:"Planning Time"`
	ExecutionTime float64                  `json:"Execution Time"`
}

func messageDeleteReceiptExplain(t *testing.T, ctx context.Context, conn *pgx.Conn, recorder *messageDeleteReceiptQueryRecorder, wantRows float64) {
	t.Helper()
	var data []byte
	if err := conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+recorder.statement,
		recorder.args...).Scan(&data); err != nil {
		t.Fatalf("explain generated receipt query: %v", err)
	}
	var reports []messageDeleteReceiptPlanReport
	if err := json.Unmarshal(data, &reports); err != nil {
		t.Fatalf("decode generated query plan: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("got %d EXPLAIN reports", len(reports))
	}
	var scan *messageDeleteReceiptPlan
	var ownerIndex bool
	var walk func(*messageDeleteReceiptPlan)
	walk = func(plan *messageDeleteReceiptPlan) {
		if plan.IndexName == "message_delete_receipts_pkey" || plan.IndexName == "message_delete_receipts_owner_idx" {
			ownerIndex = true
		}
		if plan.RelationName == "message_delete_receipts" {
			if plan.NodeType != "Index Scan" && plan.NodeType != "Index Only Scan" && plan.NodeType != "Bitmap Heap Scan" {
				t.Fatalf("owner query scans beyond its identity/owner index: %s", data)
			}
			scan = plan
		}
		for i := range plan.Plans {
			walk(&plan.Plans[i])
		}
	}
	walk(&reports[0].Plan)
	if scan == nil || !ownerIndex || scan.ActualRows != wantRows || scan.ActualLoops != 1 || scan.RowsRemoved != 0 {
		t.Fatalf("query did not walk only the requested owner range (%g rows): %s", wantRows, data)
	}
	t.Logf("generated query plan: %s; rows=%g loops=%g heap_fetches=%d planning_ms=%.3f execution_ms=%.3f shared_hit_blocks=%d shared_read_blocks=%d",
		scan.NodeType, scan.ActualRows, scan.ActualLoops, scan.HeapFetches,
		reports[0].PlanningTime, reports[0].ExecutionTime, reports[0].Plan.SharedHitBlocks, reports[0].Plan.SharedReadBlocks)
}

func TestMessageDeleteReceiptStorageCostAtOwnerCap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn := profilePhotoConn(t, ctx)
	a := messageDeleteReceiptOwner(t, ctx, conn, "+15551795020")
	b := messageDeleteReceiptOwner(t, ctx, conn, "+15551795021")
	const capacity = int64(65536)
	// Synthetic confirmed rows have the full reference, two timestamps and the
	// maximum normalized set. Deterministic opaque keys are disposable fixtures,
	// not a runtime key-generation policy or provider arrival proof.
	targets := make([]int32, 100)
	for i := range targets {
		targets[i] = math.MaxInt32 - 99 + int32(i) // #nosec G115 -- i is bounded to 0..99.
	}
	f := messageDeletePendingFixture(t, a)
	baseline := messageDeleteReceiptStorageOccupancy(t, ctx, conn, "message_delete_receipts")
	previous := baseline
	for _, owner := range []int64{a, b} {
		started := time.Now()
		if _, err := conn.Exec(ctx, `INSERT INTO message_delete_receipts (
			owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
			pts, pts_count, receipt_key, state, operation_key, epoch, stream_id, position,
			confirmed_at, retain_until
		) SELECT $1::bigint,$2,$3,1700000000000+n*4,'messages.deleteMessages','owner_message',true,$4,
			7,100,decode(md5('receipt:' || ($1::bigint)::text || ':' || n::text),'hex'),'confirmed',
			decode(md5('capture:' || ($1::bigint)::text || ':' || n::text),'hex'),3,$5,n,
			statement_timestamp(),statement_timestamp()+INTERVAL '8380800 seconds'
		FROM generate_series(1,$6::bigint) AS fixture(n)`,
			owner, f.authKeyID, f.sessionID, targets, f.streamID, capacity); err != nil {
			t.Fatalf("populate disposable owner at C: %v", err)
		}
		size := messageDeleteReceiptStorageOccupancy(t, ctx, conn, "message_delete_receipts")
		t.Logf("owner %d: C=%d setup=%s heap_delta=%d index_delta=%d total_delta=%d allocated_bytes_per_receipt=%.2f planning_bytes_per_receipt=2048 planning_bytes_at_C=134217728",
			owner, capacity, time.Since(started), size.heap-previous.heap, size.indexes-previous.indexes,
			size.total-previous.total, float64(size.total-previous.total)/float64(capacity))
		previous = size
	}
	// Establish visibility-map/statistics readiness for the warm, bounded count
	// observation. No index or planner setting is forced to make a bad plan pass.
	if _, err := conn.Exec(ctx, `VACUUM (ANALYZE) message_delete_receipts`); err != nil {
		t.Fatalf("prepare disposable relation statistics: %v", err)
	}
	var postgresVersion string
	if err := conn.QueryRow(ctx, `SHOW server_version`).Scan(&postgresVersion); err != nil {
		t.Fatalf("read PostgreSQL version: %v", err)
	}
	t.Logf("PostgreSQL %s; two independent owners at C; all rows synthetic confirmed, 100 maximal-width positive targets; relation allocation deltas include index page allocation", postgresVersion)
	var plannerFacts string
	if err := conn.QueryRow(ctx, `SELECT json_build_object(
		'pages', relpages, 'all_visible_pages', relallvisible,
		'index_only_scan', current_setting('enable_indexonlyscan'),
		'random_page_cost', current_setting('random_page_cost'),
		'seq_page_cost', current_setting('seq_page_cost'))::text
		FROM pg_class WHERE oid='message_delete_receipts'::regclass`).Scan(&plannerFacts); err != nil {
		t.Fatalf("read count planning facts: %v", err)
	}
	t.Logf("count planning facts: %s", plannerFacts)
	recorder := &messageDeleteReceiptQueryRecorder{DBTX: conn}
	q := db.New(recorder)
	for _, owner := range []int64{a, b} {
		f.ownerID, f.msgID = owner, 1700000000000+capacity*4
		got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
		if err != nil || got.OwnerID != owner || got.Pts != 7 || got.PtsCount != 100 || len(got.TargetIds) != 100 {
			t.Fatalf("owner identity lookup at C = %+v, err %v", got, err)
		}
		messageDeleteReceiptExplain(t, ctx, conn, recorder, 1)
		if count, err := q.MessageDeleteReceiptOwnerCount(ctx, owner); err != nil || count != capacity {
			t.Fatalf("owner count at C = %d, err %v", count, err)
		}
		messageDeleteReceiptExplain(t, ctx, conn, recorder, float64(capacity))
		var average float64
		if err := conn.QueryRow(ctx, `SELECT avg(pg_column_size(r)) FROM message_delete_receipts r WHERE owner_id=$1`, owner).Scan(&average); err != nil {
			t.Fatalf("read per-owner tuple occupancy: %v", err)
		}
		t.Logf("owner %d: average logical tuple bytes=%.2f", owner, average)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,$2),($3,0)`,
		a, int64(math.MaxInt64), b); err != nil {
		t.Fatalf("populate bounded owner accounting: %v", err)
	}
	accounting := messageDeleteReceiptStorageOccupancy(t, ctx, conn, "message_delete_capacity_refusals")
	t.Logf("two owner-accounting entries: heap=%d indexes=%d total=%d; no transport or targets stored", accounting.heap, accounting.indexes, accounting.total)
	messageDeleteReceiptConfirmPlan(t, ctx, conn, targets)
}

// Check confirmation with two owners' full retained histories and thousands of
// pending references sharing all but position with the target. No planner
// settings are forced: the real generated statement must find just one row.
func messageDeleteReceiptConfirmPlan(t *testing.T, ctx context.Context, conn *pgx.Conn, targets []int32) {
	t.Helper()
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551861003")
	q := db.New(conn)
	stream := erasureFlushOutbox(t, ctx, q, 1, 1)
	f := erasureFlushReceiptFixture(t, owner, stream)
	if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_receipts (
		owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
		pts, pts_count, receipt_key, state, operation_key, epoch, stream_id, position
	) SELECT $1,$2,$3,1700000000000+n*4,'messages.deleteMessages','owner_message',true,$4,
		7,100,decode(md5('pending:' || n::text),'hex'),'pending',$5,1,$6,n+1
	FROM generate_series(1,4096) AS fixture(n)`,
		owner, f.authKeyID, f.sessionID, targets, f.operationKey, f.streamID); err != nil {
		t.Fatalf("populate pending references: %v", err)
	}
	// The harness applied this migration to an empty schema. Rebuild it here
	// over retained history as well, exercising its reversible index-only change
	// and the concurrent form against the same volume used for the plan oracle.
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261015000089_message_delete_pending_capture_idx.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP INDEX CONCURRENTLY message_delete_receipts_pending_capture_idx`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if _, err := conn.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("rebuild pending reference index over retained history: %v", err)
	}
	t.Logf("concurrent index rebuild over 131072 retained and 4097 pending receipts: %s", time.Since(started))
	if _, err := conn.Exec(ctx, `ANALYZE message_delete_receipts`); err != nil {
		t.Fatal(err)
	}
	generation := erasureFlushMustGrant(t, ctx, q, stream, erasureFlushHolder(t), 1)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	recorder := &messageDeleteReceiptQueryRecorder{DBTX: tx}
	erasureFlushWantConfirm(t, ctx, db.New(recorder), stream, 1, generation, erasureFlushMatchingProof(stream, 1, 1), 1)
	// EXPLAIN ANALYZE must perform a fresh confirmation, including its receipt
	// update, rather than explain a replay that cannot reach the receipt scan.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var data []byte
	if err := conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+recorder.statement, recorder.args...).Scan(&data); err != nil {
		t.Fatalf("explain confirmation: %v", err)
	}
	var reports []messageDeleteReceiptPlanReport
	if err := json.Unmarshal(data, &reports); err != nil || len(reports) != 1 {
		t.Fatalf("decode confirmation plan: %v: %s", err, data)
	}
	var indexed bool
	var walk func(messageDeleteReceiptPlan)
	walk = func(plan messageDeleteReceiptPlan) {
		if plan.RelationName == "message_delete_receipts" && plan.NodeType != "ModifyTable" {
			if plan.NodeType != "Index Scan" || plan.IndexName != "message_delete_receipts_pending_capture_idx" ||
				plan.ActualRows != 1 || plan.ActualLoops != 1 || plan.RowsRemoved != 0 {
				t.Fatalf("confirmation did not use a bounded pending-reference index scan: %s", data)
			}
			indexed = true
		}
		for _, child := range plan.Plans {
			walk(child)
		}
	}
	walk(reports[0].Plan)
	if !indexed {
		t.Fatalf("confirmation did not reach the receipt index: %s", data)
	}
	t.Logf("confirmation plan at two retained owner caps plus 4097 pending receipts: %s", data)
}
