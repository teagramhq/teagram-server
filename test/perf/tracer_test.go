//go:build performance

package perf_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryTracer sees every statement the server's pool sends. Nothing else
// shares that pool while a scenario runs, so the statements between two
// quiet points are exactly the work one RPC caused, including what it
// triggered after its reply (update fan-out, delivery reads).
type queryTracer struct {
	mu          sync.Mutex
	inFlight    int
	last        time.Time
	queries     int
	captured    []capturedQuery
	timings     []queryTiming
	capture     bool
	timeQueries bool
}

type capturedQuery struct {
	sql  string
	args []any
}

type queryTiming struct {
	name    string
	elapsed time.Duration
}

type queryTraceContextKey struct{}

type queryTraceSpan struct {
	name    string
	started time.Time
}

func (q *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	q.mu.Lock()
	q.inFlight++
	q.last = time.Now()
	q.queries++
	timeQueries := q.timeQueries
	if q.capture {
		q.captured = append(q.captured, capturedQuery{sql: data.SQL, args: append([]any(nil), data.Args...)})
	}
	q.mu.Unlock()
	if !timeQueries {
		return ctx
	}
	span := queryTraceSpan{name: queryName(data.SQL), started: time.Now()}
	return context.WithValue(ctx, queryTraceContextKey{}, span)
}

func (q *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	var span queryTraceSpan
	var ok bool
	q.mu.Lock()
	q.inFlight--
	q.last = time.Now()
	if q.timeQueries {
		span, ok = ctx.Value(queryTraceContextKey{}).(queryTraceSpan)
	}
	if ok {
		q.timings = append(q.timings, queryTiming{name: span.name, elapsed: time.Since(span.started)})
	}
	q.mu.Unlock()
}

func queryName(sql string) string {
	fields := strings.Fields(firstLine(sql))
	if len(fields) >= 3 && fields[0] == "--" && fields[1] == "name:" {
		return fields[2]
	}
	return "unlabeled"
}

// reset starts a new counting window. With capture set, the window also keeps
// each statement for EXPLAIN.
func (q *queryTracer) reset(capture, timeQueries bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = 0
	q.captured = nil
	q.timings = nil
	q.capture = capture
	q.timeQueries = timeQueries
}

func (q *queryTracer) window() (int, []capturedQuery) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.queries, q.captured
}

func (q *queryTracer) timingWindow() []queryTiming {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]queryTiming(nil), q.timings...)
}

func TestQueryTracerRecordsStatementDuration(t *testing.T) {
	tracer := &queryTracer{}
	tracer.reset(false, true)
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "-- name: Example :one\nSELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	got := tracer.timingWindow()
	if len(got) != 1 || got[0].name != "Example" || got[0].elapsed <= 0 {
		t.Fatalf("query timings = %+v, want a positive duration for Example", got)
	}
}

// quietFor is how long the pool must stay idle before the window closes. It
// is longer than any gap between an RPC's reply and the delivery work the
// reply's NOTIFY schedules.
const quietFor = 30 * time.Millisecond

var errNotQuiet = errors.New("server did not go quiet")

// awaitQuiet returns once no statement has been in flight for quietFor.
func (q *queryTracer) awaitQuiet(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		q.mu.Lock()
		idle := q.inFlight == 0 && time.Since(q.last) >= quietFor
		q.mu.Unlock()
		if idle {
			return nil
		}
		if time.Now().After(deadline) {
			return errNotQuiet
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
