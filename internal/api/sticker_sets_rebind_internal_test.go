package api

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

type stickerSetRebindTransport struct {
	entered chan<- struct{}
	release <-chan struct{}
	sends   atomic.Int32
}

func (t *stickerSetRebindTransport) Send(ctx context.Context, _ *bin.Buffer) error {
	t.sends.Add(1)
	if t.release == nil {
		return nil
	}
	t.entered <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.release:
		return nil
	}
}

func (*stickerSetRebindTransport) Recv(context.Context, *bin.Buffer) error {
	return context.Canceled
}

func (*stickerSetRebindTransport) Close() error { return nil }

func TestDeliverStickerSetsDoesNotPushAfterSessionRebind(t *testing.T) {
	t.Parallel()

	const ownerID, originSessionID = int64(42), int64(100)
	var rawKey crypto.Key
	for i := range rawKey {
		rawKey[i] = byte(i)
	}
	key := rawKey.WithID()

	registry := mtproto.NewSessionRegistry()
	entered := make(chan struct{}, transientFanoutWorkerCount)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBlockers := func() { releaseOnce.Do(func() { close(release) }) }

	var connections []*mtproto.Conn
	addConn := func(sessionID int64, transport *stickerSetRebindTransport) *mtproto.Conn {
		conn := mtproto.NewTestConn(transport, key)
		conn.SetOwner(ownerID)
		conn.SetSession(sessionID)
		if !registry.Add(ownerID, conn) {
			t.Fatalf("register session %d", sessionID)
		}
		connections = append(connections, conn)
		return conn
	}
	for i := range transientFanoutWorkerCount {
		addConn(int64(i+1), &stickerSetRebindTransport{entered: entered, release: release})
	}
	targetTransport := &stickerSetRebindTransport{}
	target := addConn(99, targetTransport)
	originTransport := &stickerSetRebindTransport{}
	addConn(originSessionID, originTransport)

	done := make(chan struct{})
	t.Cleanup(func() {
		releaseBlockers()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("sticker-set fan-out did not stop after releasing blocked writes")
		}
		for _, conn := range connections {
			registry.Remove(ownerID, conn)
		}
	})

	updater := NewUpdater(nil, 2, registry, nil, nil)
	go func() {
		updater.DeliverStickerSets(context.Background(), ownerID, mtproto.AuthKeyIDInt64(key.ID), originSessionID)
		close(done)
	}()
	for range transientFanoutWorkerCount {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("transient fan-out workers did not reach the blocking sessions")
		}
	}

	// The updater has snapshotted this connection as a sibling, but it now
	// rebinds to the originating session before a fan-out worker can write it.
	target.SetSession(originSessionID)
	releaseBlockers()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sticker-set fan-out did not finish after releasing blocked writes")
	}

	if got := targetTransport.sends.Load(); got != 0 {
		t.Errorf("rebound origin transport writes = %d, want 0", got)
	}
	if got := originTransport.sends.Load(); got != 0 {
		t.Errorf("origin transport writes = %d, want 0", got)
	}
}
