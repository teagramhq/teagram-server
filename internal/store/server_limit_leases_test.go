package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLimitLeaseConcurrentAdmissionAcrossStores(t *testing.T) {
	t.Parallel()

	dsn := pgtest.DSN(t)
	stores := []*store.Store{openStore(t, dsn), openStore(t, dsn)}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type admission struct {
		lease  *store.LimitLease
		denied *store.RateLimitResult
		err    error
	}
	ready := make(chan struct{}, len(stores))
	start := make(chan struct{})
	results := make(chan admission, len(stores))
	for _, st := range stores {
		go func(st *store.Store) {
			ready <- struct{}{}
			<-start
			lease, denied, err := st.TryAcquireLimitLease(ctx, 0, "test_concurrent_admission", 1, time.Minute)
			results <- admission{lease: lease, denied: denied, err: err}
		}(st)
	}
	for range stores {
		<-ready
	}
	close(start)

	var granted, denied int
	var leases []*store.LimitLease
	t.Cleanup(func() {
		for _, lease := range leases {
			if err := stores[0].ReleaseLimitLease(context.Background(), lease); err != nil {
				t.Errorf("release admitted lease: %v", err)
			}
		}
	})
	for range stores {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent lease admission: %v", result.err)
			}
			if result.lease != nil {
				granted++
				leases = append(leases, result.lease)
			}
			if result.denied != nil {
				denied++
			}
		case <-ctx.Done():
			t.Fatalf("concurrent lease admission did not complete: %v", ctx.Err())
		}
	}
	if granted != 1 || denied != 1 {
		t.Fatalf("concurrent admissions granted=%d denied=%d, want exactly one of each", granted, denied)
	}
}
