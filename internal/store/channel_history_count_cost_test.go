package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type historyCountTrace struct {
	mu      sync.Mutex
	queries []historyCountQuery
}

type historyCountQuery struct {
	sql  string
	args []any
}

func (t *historyCountTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.mu.Lock()
	t.queries = append(t.queries, historyCountQuery{sql: data.SQL, args: append([]any(nil), data.Args...)})
	t.mu.Unlock()
	return ctx
}

func (*historyCountTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (t *historyCountTrace) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.queries = nil
}

func (t *historyCountTrace) historyCountQuery() (historyCountQuery, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, query := range t.queries {
		if strings.Contains(query.sql, "-- name: CountChannelHistory") {
			return query, true
		}
	}
	return historyCountQuery{}, false
}

func TestChannelHistoryCountDoesNotReadEveryChannelMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	trace := &historyCountTrace{}
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)), store.WithQueryTracer(trace))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	owner := mustUser(t, s, "+15551280001")
	group, err := s.CreateChannel(ctx, owner.ID, "History count cost", "", true)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.ExecChannelPostSummarySQL(ctx, s, `ALTER TABLE channel_messages DISABLE TRIGGER channel_messages_summary_maintain`); err != nil {
		t.Fatalf("disable summary maintenance for seeded rows: %v", err)
	}
	triggersDisabled := true
	t.Cleanup(func() {
		if triggersDisabled {
			if err := store.ExecChannelPostSummarySQL(context.Background(), s, `ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`); err != nil {
				t.Errorf("restore summary maintenance: %v", err)
			}
		}
	})
	const postCount = 12_000
	if err := store.ExecChannelPostSummarySQL(ctx, s, `
		INSERT INTO channel_messages (channel_id, local_id, from_id, message)
		SELECT $1, post_id, $2, 'seeded history post'
		  FROM generate_series(2::bigint, $3::bigint) AS post_id
	`, group.ID, owner.ID, postCount+1); err != nil {
		t.Fatalf("seed history rows: %v", err)
	}
	if err := store.ExecChannelPostSummarySQL(ctx, s, `ALTER TABLE channel_messages ENABLE TRIGGER channel_messages_summary_maintain`); err != nil {
		t.Fatalf("enable summary maintenance: %v", err)
	}
	triggersDisabled = false
	if err := store.ExecChannelPostSummarySQL(ctx, s, `
		INSERT INTO channel_post_summaries (channel_id, scope_kind, author_id, depth, prefix, live_count)
		VALUES ($1, 0, 0, 0, 0, $2)
	`, group.ID, postCount); err != nil {
		t.Fatalf("seed count summary root: %v", err)
	}
	if err := store.ExecChannelPostSummarySQL(ctx, s, `ANALYZE channel_messages`); err != nil {
		t.Fatalf("analyze seeded history rows: %v", err)
	}

	trace.reset()
	page, count, err := s.ChannelHistoryWithOffset(ctx, group.ID, owner.ID, 0, 0, 0, 0, 0, 100)
	if err != nil {
		t.Fatalf("read seeded history: %v", err)
	}
	if len(page) != 100 || count != postCount+1 {
		t.Fatalf("history page/count = %d/%d, want 100/%d", len(page), count, postCount+1)
	}

	query, found := trace.historyCountQuery()
	if !found {
		t.Fatal("history count statement was not traced")
	}
	var explained []struct {
		Plan historyPlanNode `json:"Plan"`
	}
	var raw []byte
	if err := store.StorePool(s).QueryRow(ctx,
		"EXPLAIN (ANALYZE, FORMAT JSON) "+query.sql,
		query.args...,
	).Scan(&raw); err != nil {
		t.Fatalf("explain history count: %v", err)
	}
	if err := json.Unmarshal(raw, &explained); err != nil {
		t.Fatalf("decode history count plan: %v", err)
	}
	if len(explained) != 1 {
		t.Fatalf("history count plan rows = %d, want 1", len(explained))
	}
	var messageRows int64
	var messageSeqScan string
	explained[0].Plan.walk(func(node historyPlanNode) {
		if node.RelationName != "channel_messages" {
			return
		}
		messageRows += int64(node.ActualRows * node.ActualLoops)
		if node.NodeType == "Seq Scan" {
			messageSeqScan = node.NodeType
		}
	})
	if messageSeqScan != "" || messageRows >= 1_000 {
		t.Fatalf("history count reads channel_messages via %q and visits %d rows, want no sequential scan and fewer than 1000 rows", messageSeqScan, messageRows)
	}
}

type historyPlanNode struct {
	NodeType     string            `json:"Node Type"`
	RelationName string            `json:"Relation Name"`
	ActualRows   float64           `json:"Actual Rows"`
	ActualLoops  float64           `json:"Actual Loops"`
	Plans        []historyPlanNode `json:"Plans"`
}

func (node historyPlanNode) walk(visit func(historyPlanNode)) {
	visit(node)
	for _, child := range node.Plans {
		child.walk(visit)
	}
}
