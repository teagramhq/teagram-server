//go:build performance

package perf_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryTracer sees every statement the server's pool sends. Nothing else
// shares that pool while a scenario runs, so the statements between two
// quiet points are exactly the work one RPC caused, including what it
// triggered after its reply (update fan-out, delivery reads).
type queryTracer struct {
	mu       sync.Mutex
	inFlight int
	last     time.Time
	queries  int
	captured []*capturedQuery
	capture  bool
}

type capturedQuery struct {
	sql       string
	args      []any
	startedAt time.Time
	duration  time.Duration
}

type queryDurationContextKey struct{}

func (q *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	startedAt := time.Now()
	q.mu.Lock()
	q.inFlight++
	q.last = startedAt
	q.queries++
	var captured *capturedQuery
	if q.capture {
		captured = &capturedQuery{
			sql: data.SQL, args: append([]any(nil), data.Args...), startedAt: startedAt,
		}
		q.captured = append(q.captured, captured)
	}
	q.mu.Unlock()
	if captured != nil {
		return context.WithValue(ctx, queryDurationContextKey{}, captured)
	}
	return ctx
}

func (q *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	finishedAt := time.Now()
	captured, _ := ctx.Value(queryDurationContextKey{}).(*capturedQuery)
	q.mu.Lock()
	defer q.mu.Unlock()
	if captured != nil {
		captured.duration = finishedAt.Sub(captured.startedAt)
	}
	q.inFlight--
	q.last = finishedAt
}

// reset starts a new counting window. With capture set, the window also keeps
// each statement for EXPLAIN.
func (q *queryTracer) reset(capture bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.queries = 0
	q.captured = nil
	q.capture = capture
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
