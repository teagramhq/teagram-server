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
	captured    []*capturedQuery
	timings     []queryTimingSpan
	capture     bool
	timeQueries bool
}

type capturedQuery struct {
	sql       string
	args      []any
	startedAt time.Time
	duration  time.Duration
}

type queryTiming struct {
	name    string
	elapsed time.Duration
}

type queryTimingSpan struct {
	name    string
	started time.Time
	ended   time.Time
	window  *queryTimingWindow
}

type queryTimingWindowContextKey struct{}

type queryTimingWindow struct {
	mu      sync.RWMutex
	started time.Time
	ended   time.Time
}

func newQueryTimingWindow(started time.Time) *queryTimingWindow {
	return &queryTimingWindow{started: started}
}

func (w *queryTimingWindow) finish(ended time.Time) {
	w.mu.Lock()
	w.ended = ended
	w.mu.Unlock()
}

func (w *queryTimingWindow) duration(started, ended time.Time) time.Duration {
	w.mu.RLock()
	windowStarted, windowEnded := w.started, w.ended
	w.mu.RUnlock()
	if windowEnded.IsZero() {
		return 0
	}
	if started.Before(windowStarted) {
		started = windowStarted
	}
	if ended.After(windowEnded) {
		ended = windowEnded
	}
	if !ended.After(started) {
		return 0
	}
	return ended.Sub(started)
}

type queryDurationContextKey struct{}

type queryTraceContextKey struct{}

type queryTraceSpan struct {
	name    string
	started time.Time
	window  *queryTimingWindow
}

func (q *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	startedAt := time.Now()
	q.mu.Lock()
	q.inFlight++
	q.last = startedAt
	q.queries++
	timeQueries := q.timeQueries
	var window *queryTimingWindow
	if timeQueries {
		if requestWindow, ok := ctx.Value(queryTimingWindowContextKey{}).(*queryTimingWindow); ok {
			window = requestWindow
		}
	}
	var captured *capturedQuery
	if q.capture {
		captured = &capturedQuery{
			sql: data.SQL, args: append([]any(nil), data.Args...), startedAt: startedAt,
		}
		q.captured = append(q.captured, captured)
	}
	q.mu.Unlock()
	if captured != nil {
		ctx = context.WithValue(ctx, queryDurationContextKey{}, captured)
	}
	if timeQueries && window != nil {
		ctx = context.WithValue(ctx, queryTraceContextKey{}, queryTraceSpan{
			name: queryName(data.SQL), started: startedAt, window: window,
		})
	}
	return ctx
}

func (q *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	finishedAt := time.Now()
	captured, hasCapture := ctx.Value(queryDurationContextKey{}).(*capturedQuery)
	span, timed := ctx.Value(queryTraceContextKey{}).(queryTraceSpan)
	q.mu.Lock()
	defer q.mu.Unlock()
	if hasCapture && captured != nil {
		captured.duration = finishedAt.Sub(captured.startedAt)
	}
	if timed {
		q.timings = append(q.timings, queryTimingSpan{
			name: span.name, started: span.started, ended: finishedAt, window: span.window,
		})
	}
	q.inFlight--
	q.last = finishedAt
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
	captured := make([]capturedQuery, len(q.captured))
	for i, query := range q.captured {
		captured[i] = *query
		captured[i].args = append([]any(nil), query.args...)
	}
	return q.queries, captured
}

func (q *queryTracer) timingWindow() []queryTiming {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.timings) == 0 {
		return nil
	}
	timings := make([]queryTiming, 0, len(q.timings))
	for _, span := range q.timings {
		elapsed := span.window.duration(span.started, span.ended)
		if elapsed > 0 {
			timings = append(timings, queryTiming{name: span.name, elapsed: elapsed})
		}
	}
	return timings
}

func TestQueryTracerRecordsStatementDuration(t *testing.T) {
	tracer := &queryTracer{}
	tracer.reset(false, true)
	started := time.Now()
	window := newQueryTimingWindow(started)
	ctx := context.WithValue(context.Background(), queryTimingWindowContextKey{}, window)
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- name: Example :one\nSELECT 1"})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	window.finish(time.Now())
	got := tracer.timingWindow()
	if len(got) != 1 || got[0].name != "Example" || got[0].elapsed <= 0 {
		t.Fatalf("query timings = %+v, want a positive duration for Example", got)
	}
}

func TestQueryTracerClipsStatementDurationToDispatchWindow(t *testing.T) {
	tracer := &queryTracer{}
	tracer.reset(false, true)
	started := time.Now()
	window := newQueryTimingWindow(started)
	ctx := context.WithValue(context.Background(), queryTimingWindowContextKey{}, window)
	ctx = tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "-- name: Example :one\nSELECT 1"})
	ended := time.Now()
	window.finish(ended)
	time.Sleep(2 * time.Millisecond)
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})

	got := tracer.timingWindow()
	if len(got) != 1 {
		t.Fatalf("query timings = %+v, want one statement span", got)
	}
	if got[0].elapsed > ended.Sub(started) {
		t.Fatalf("statement duration = %s, exceeds dispatch window %s", got[0].elapsed, ended.Sub(started))
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
