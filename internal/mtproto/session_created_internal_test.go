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
	for keyID := 1; keyID <= maxRememberedSessions/maxRememberedSessionsPerAuthKey; keyID++ {
		for sessionID := range maxRememberedSessionsPerAuthKey {
			if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{byte(keyID)}, int64(sessionID), clock, send); err != nil {
				t.Fatalf("remember key %d session %d: %v", keyID, sessionID, err)
			}
			now = now.Add(time.Nanosecond)
		}
	}
	registry.createdSessionsMu.Lock()
	gotRemembered := len(registry.createdSessions)
	registry.createdSessionsMu.Unlock()
	if gotRemembered != maxRememberedSessions {
		t.Fatalf("remembered sessions = %d, want cap %d", gotRemembered, maxRememberedSessions)
	}
	extraKey := [8]byte{200}
	if err := registry.sendSessionCreatedOnce(context.Background(), extraKey, maxRememberedSessions, clock, send); err != nil {
		t.Fatalf("remember session past global cap: %v", err)
	}
	if got := sends.Load(); got != maxRememberedSessions+1 {
		t.Fatalf("sends at capacity = %d, want %d", got, maxRememberedSessions+1)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), extraKey, maxRememberedSessions, clock, send); err != nil {
		t.Fatalf("recent session duplicate: %v", err)
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 0, clock, send); err != nil {
		t.Fatalf("evicted session retry: %v", err)
	}
	if got := sends.Load(); got != maxRememberedSessions+2 {
		t.Fatalf("sends after cap eviction = %d, want %d", got, maxRememberedSessions+2)
	}
}

func TestSessionCreatedCacheBoundsEachKeyWithoutEvictingOtherKeys(t *testing.T) {
	registry := NewSessionRegistry()
	now := time.Unix(1_800_000_000, 0)
	var sends atomic.Int32
	clock := func() time.Time { return now }
	send := func() error {
		sends.Add(1)
		return nil
	}
	otherKey := [8]byte{2}
	churningKey := [8]byte{1}
	if err := registry.sendSessionCreatedOnce(context.Background(), otherKey, 42, clock, send); err != nil {
		t.Fatalf("remember other key session: %v", err)
	}

	const sessionsPerKey = maxRememberedSessionsPerAuthKey
	for sessionID := int64(0); sessionID <= sessionsPerKey; sessionID++ {
		if err := registry.sendSessionCreatedOnce(context.Background(), churningKey, sessionID, clock, send); err != nil {
			t.Fatalf("remember churning key session %d: %v", sessionID, err)
		}
		now = now.Add(time.Nanosecond)
	}
	wantSends := int32(sessionsPerKey + 2)
	for _, item := range []struct {
		name      string
		authKeyID [8]byte
		sessionID int64
	}{
		{name: "other key marker", authKeyID: otherKey, sessionID: 42},
		{name: "second-oldest churning marker", authKeyID: churningKey, sessionID: 1},
		{name: "newest churning marker", authKeyID: churningKey, sessionID: sessionsPerKey},
	} {
		if err := registry.sendSessionCreatedOnce(context.Background(), item.authKeyID, item.sessionID, clock, send); err != nil {
			t.Fatalf("duplicate %s: %v", item.name, err)
		}
		if got := sends.Load(); got != wantSends {
			t.Fatalf("sends after duplicate %s = %d, want %d", item.name, got, wantSends)
		}
	}
	if err := registry.sendSessionCreatedOnce(context.Background(), churningKey, 0, clock, send); err != nil {
		t.Fatalf("retry oldest churning marker: %v", err)
	}
	if got := sends.Load(); got != wantSends+1 {
		t.Fatalf("sends after retrying oldest churning marker = %d, want %d", got, wantSends+1)
	}
}

func TestSessionCreatedCacheDoesNotTakeConnectionRegistryLock(t *testing.T) {
	registry := NewSessionRegistry()
	registry.mu.Lock()
	defer registry.mu.Unlock()

	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- registry.sendSessionCreatedOnce(context.Background(), [8]byte{1}, 42, time.Now, func() error {
			return nil
		})
	}()
	<-started
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remember session while registry lock is held: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("session-created cache waited for the connection registry lock")
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
