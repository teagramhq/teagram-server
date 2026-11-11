package mtproto

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRegistrySampleDeliveryLagLockContention(t *testing.T) {
	r := NewSessionRegistry()
	if !r.Add(1, &Conn{}) {
		t.Fatal("register connection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var calls atomic.Int64
	r.mu.Lock()
	result := make(chan DeliveryLagSample, 1)
	go func() {
		result <- r.SampleDeliveryLag(ctx, func(context.Context, int64) (int64, error) {
			calls.Add(1)
			return 1, nil
		})
	}()
	sample := <-result
	r.mu.Unlock()

	if sample.EligibleConnections != 1 || sample.SampledConnections != 0 || sample.Complete {
		t.Fatalf("lock-contention sample = %+v, want eligible=1, sampled=0, incomplete", sample)
	}
	if calls.Load() != 0 {
		t.Fatalf("account-head calls = %d, want 0 under lock contention", calls.Load())
	}
}

func TestSessionRegistryTailnetConnectionsDoNotUseLimitedPathCap(t *testing.T) {
	r := NewSessionRegistry()
	if err := r.setMaxUserConns(1); err != nil {
		t.Fatalf("set user connection cap: %v", err)
	}

	limited := &Conn{}
	if !r.Add(7, limited) {
		t.Fatal("first limited connection was refused")
	}
	tailnet := &Conn{listenerPath: ListenerPathTailnet}
	if !r.Add(7, tailnet) {
		t.Fatal("tailnet connection was refused by the limited-path cap")
	}
	if got := len(r.Conns(7)); got != 2 {
		t.Fatalf("registered connections = %d, want both public and tailnet connections", got)
	}

	if r.Add(7, &Conn{}) {
		t.Fatal("second limited connection passed the public-path cap")
	}
	r.Remove(7, tailnet)
	if r.Add(7, &Conn{}) {
		t.Fatal("removing a tailnet connection freed a public-path slot")
	}
	r.Remove(7, limited)
	if !r.Add(7, &Conn{}) {
		t.Fatal("limited connection was refused after the limited connection left")
	}
}
