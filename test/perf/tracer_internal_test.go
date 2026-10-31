//go:build performance

package perf_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestQueryTracerCapturesStatementDuration(t *testing.T) {
	t.Parallel()

	tracer := &queryTracer{}
	tracer.reset(true)
	ctx := tracer.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT 1"})
	time.Sleep(time.Millisecond)
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{})
	_, queries := tracer.window()
	if len(queries) != 1 {
		t.Fatalf("captured queries = %d, want 1", len(queries))
	}

	if queries[0].duration <= 0 {
		t.Fatalf("captured query duration = %v, want a positive duration", queries[0].duration)
	}
}
