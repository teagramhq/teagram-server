package mtproto

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionRegistrySerializesStatusCallbacks(t *testing.T) {
	registry := NewSessionRegistry()
	oldConn := &Conn{}
	newConn := &Conn{}
	if !registry.Add(7, oldConn) {
		t.Fatal("register existing connection")
	}

	offlineStarted := make(chan struct{})
	releaseOffline := make(chan struct{})
	offlineDone := make(chan struct{})
	onlineDone := make(chan struct{})
	events := make(chan bool, 2)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseOffline) }) }
	defer release()

	go func() {
		registry.removeWithStatusChange(7, oldConn, func() {
			close(offlineStarted)
			<-releaseOffline
			events <- false
		})
		close(offlineDone)
	}()
	select {
	case <-offlineStarted:
	case <-time.After(time.Second):
		t.Fatal("offline callback did not start")
	}

	go func() {
		registry.addWithStatusChange(7, newConn, func() { events <- true })
		close(onlineDone)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		registry.statusMu.Lock()
		statusLock := registry.statusLocks[7]
		waiting := statusLock != nil && statusLock.refs == 2
		registry.statusMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("online transition did not queue behind offline callback")
		}
		runtime.Gosched()
	}
	select {
	case <-onlineDone:
		t.Fatal("online callback completed before the in-flight offline callback")
	default:
	}

	release()
	select {
	case <-offlineDone:
	case <-time.After(time.Second):
		t.Fatal("offline transition did not finish")
	}
	select {
	case <-onlineDone:
	case <-time.After(time.Second):
		t.Fatal("online transition did not finish")
	}

	if got := <-events; got {
		t.Fatal("first status event is online, want offline")
	}
	if got := <-events; !got {
		t.Fatal("second status event is offline, want online")
	}
}

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
