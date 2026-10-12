package mtproto

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCreatedCacheExpires(t *testing.T) {
	registry := NewSessionRegistry()
	now := time.Unix(1_800_000_000, 0)
	var sends atomic.Int32
	send := func() error {
		sends.Add(1)
		return nil
	}
	clock := func() time.Time { return now }

	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, clock, send); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, clock, send); err != nil {
		t.Fatalf("duplicate send: %v", err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("sends before expiry = %d, want 1", got)
	}

	now = now.Add(sessionCreatedTTL + time.Second)
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, clock, send); err != nil {
		t.Fatalf("send after expiry: %v", err)
	}
	if got := sends.Load(); got != 2 {
		t.Fatalf("sends after expiry = %d, want 2", got)
	}
}

func TestSessionCreatedCacheHasFixedCapacity(t *testing.T) {
	registry := NewSessionRegistry()
	now := time.Unix(1_800_000_000, 0)
	var sends atomic.Int32
	clock := func() time.Time { return now }
	send := func() error {
		sends.Add(1)
		return nil
	}
	for sessionID := int64(0); sessionID <= maxRememberedSessions; sessionID++ {
		if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, sessionID, clock, send); err != nil {
			t.Fatalf("remember session %d: %v", sessionID, err)
		}
		now = now.Add(time.Nanosecond)
	}
	if got := len(registry.createdSessions); got != maxRememberedSessions {
		t.Fatalf("remembered sessions = %d, want cap %d", got, maxRememberedSessions)
	}
	if got := sends.Load(); got != maxRememberedSessions+1 {
		t.Fatalf("sends at capacity = %d, want %d", got, maxRememberedSessions+1)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, maxRememberedSessions, clock, send); err != nil {
		t.Fatalf("recent session duplicate: %v", err)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 0, clock, send); err != nil {
		t.Fatalf("evicted session retry: %v", err)
	}
	if got := sends.Load(); got != maxRememberedSessions+2 {
		t.Fatalf("sends after cap eviction = %d, want %d", got, maxRememberedSessions+2)
	}
}

func TestSessionCreatedCacheRetriesFailedSend(t *testing.T) {
	registry := NewSessionRegistry()
	now := func() time.Time { return time.Unix(1_800_000_000, 0) }
	wantErr := errors.New("write failed")
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, now, func() error {
		return wantErr
	}); !errors.Is(err, wantErr) {
		t.Fatalf("failed send = %v, want %v", err, wantErr)
	}
	var sends atomic.Int32
	send := func() error {
		sends.Add(1)
		return nil
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, now, send); err != nil {
		t.Fatalf("retry failed send: %v", err)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, now, send); err != nil {
		t.Fatalf("duplicate successful retry: %v", err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("successful sends after retry = %d, want 1", got)
	}
}

func TestSessionCreatedCacheSerializesConcurrentSends(t *testing.T) {
	registry := NewSessionRegistry()
	startFirst := make(chan struct{})
	finishFirst := make(chan struct{})
	var sends atomic.Int32
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, time.Now, func() error {
			sends.Add(1)
			close(startFirst)
			<-finishFirst
			return nil
		})
	}()
	<-startFirst

	secondAttempted := make(chan struct{})
	secondClockEntered := make(chan struct{})
	secondClockRelease := make(chan struct{})
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	defer cancelSecond()
	secondDone := make(chan error, 1)
	go func() {
		close(secondAttempted)
		secondDone <- registry.sendSessionCreatedOnce(secondCtx, [8]byte{1}, 42, func() time.Time {
			close(secondClockEntered)
			<-secondClockRelease
			return time.Now()
		}, func() error {
			sends.Add(1)
			return nil
		})
	}()
	<-secondAttempted
	<-secondClockEntered
	close(secondClockRelease)
	cancelSecond()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("concurrent waiter = %v, want cancellation while the first send is in flight", err)
	}
	close(finishFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first send: %v", err)
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("concurrent send callbacks = %d, want 1", got)
	}
}
