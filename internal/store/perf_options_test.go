package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type recordingTracer struct {
	mu   sync.Mutex
	sqls []string
}

func (r *recordingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, data.SQL)
	return ctx
}

func (*recordingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestQueryTracerSeesStoreQueries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tracer := &recordingTracer{}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithQueryTracer(tracer))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := s.UserByID(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tracer.mu.Lock()
	defer tracer.mu.Unlock()
	for _, sql := range tracer.sqls {
		if strings.Contains(sql, "FROM users") {
			return
		}
	}
	t.Fatalf("traced queries = %q, want the user lookup", tracer.sqls)
}

func TestMaxChannelParticipantsOptionBoundsAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore(), store.WithMaxChannelParticipants(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15553330001")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateUser(ctx, "+15553330002")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateUser(ctx, "+15553330003")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.CreateChannel(ctx, owner.ID, "capped", "", false)
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.AddChannelMembers(ctx, ch.ID, owner.ID, []int64{a.ID, b.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 1 || added[0] != a.ID {
		t.Fatalf("added = %v, want only %d under a two-seat bound", added, a.ID)
	}
}
