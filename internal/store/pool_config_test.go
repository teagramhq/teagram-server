package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestConfiguredPoolPreservesStatementTimeoutAndAssemblyReserve(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t)+"&pool_max_conns=20", pgtest.EncKey(),
		store.WithoutBlobStore(), store.WithStatementTimeout(3*time.Second),
		store.WithPoolConfig(store.PoolConfig{MaxConns: 8, MinConns: 2, MaxConnIdleTime: time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	pool := store.StorePool(s)
	cfg := pool.Config()
	if cfg.MaxConns != 8 || cfg.MinConns != 2 || cfg.MaxConnIdleTime != time.Minute {
		t.Fatalf("pool settings = max %d, min %d, idle %s", cfg.MaxConns, cfg.MinConns, cfg.MaxConnIdleTime)
	}
	if s.AssemblyConcurrencyLimit() != 2 || s.AssemblyPoolHeadroom() != 6 {
		t.Fatal("assembly slots were not sized from the configured pool")
	}
	// Acquire both connections together to check the timeout on each session.
	for range 2 {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
		var timeout string
		if err := conn.QueryRow(ctx, "SHOW statement_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if timeout != "3s" {
			t.Fatalf("statement_timeout = %s, want 3s", timeout)
		}
	}
}
