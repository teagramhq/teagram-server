package api

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestDeliverStatusUsesCurrentRowAfterQueuedTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	owner, err := s.CreateUser(ctx, "+15553000901")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	viewer, err := s.CreateUser(ctx, "+15553000902")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, viewer.ID, "status", 1, 0, 0); err != nil {
		t.Fatalf("create dialog: %v", err)
	}
	if err := s.SetUserStatus(ctx, owner.ID, false); err != nil {
		t.Fatalf("set owner offline: %v", err)
	}
	if err := s.SetUserStatus(ctx, owner.ID, true); err != nil {
		t.Fatalf("set owner online: %v", err)
	}

	key := replyTestKey()
	transport := &recordingNotifyTransport{}
	conn := mtproto.NewTestConn(transport, key)
	conn.SetOwner(viewer.ID)
	registry := mtproto.NewSessionRegistry()
	if !registry.Add(viewer.ID, conn) {
		t.Fatal("register viewer connection")
	}
	t.Cleanup(func() { registry.Remove(viewer.ID, conn) })

	updater := NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver())
	// This is the older offline event arriving after the row has advanced back online.
	updater.DeliverStatus(ctx, owner.ID, false)

	frames := transport.framesFrom(0)
	if len(frames) != 1 {
		t.Fatalf("status push frames = %d, want one", len(frames))
	}
	data, err := crypto.NewClientCipher(crypto.DefaultRand()).DecryptFromBuffer(key, &bin.Buffer{Buf: frames[0]})
	if err != nil {
		t.Fatalf("decrypt status push: %v", err)
	}
	var short tg.UpdateShort
	if err := short.Decode(&bin.Buffer{Buf: data.Data()}); err != nil {
		t.Fatalf("decode status push: %v", err)
	}
	update, ok := short.Update.(*tg.UpdateUserStatus)
	if !ok {
		t.Fatalf("pushed update = %T, want *tg.UpdateUserStatus", short.Update)
	}
	if _, ok := update.Status.(*tg.UserStatusOnline); !ok {
		t.Fatalf("pushed status = %T, want current online row", update.Status)
	}
}
