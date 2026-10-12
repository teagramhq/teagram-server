package api

import (
	"context"
	"testing"
	"time"

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

func TestDeliverStatusDoesNotPushToSavedMessagesSelfViewer(t *testing.T) {
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

	owner, err := s.CreateUser(ctx, "+15553000903")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, owner.ID, owner.ID, "saved", 2, 0, 0); err != nil {
		t.Fatalf("create Saved Messages dialog: %v", err)
	}
	partners, err := s.DialogPartners(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list dialog partners: %v", err)
	}
	if len(partners) != 1 || partners[0] != owner.ID {
		t.Fatalf("Saved Messages partners = %v, want owner %d", partners, owner.ID)
	}

	key := replyTestKey()
	transport := &recordingNotifyTransport{}
	conn := mtproto.NewTestConn(transport, key)
	conn.SetOwner(owner.ID)
	registry := mtproto.NewSessionRegistry()
	if !registry.Add(owner.ID, conn) {
		t.Fatal("register owner connection")
	}
	t.Cleanup(func() { registry.Remove(owner.ID, conn) })

	updater := NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver())
	updater.DeliverStatus(store.WithStatusReconcile(ctx, owner.ID), owner.ID, true)
	if frames := transport.framesFrom(0); len(frames) != 0 {
		t.Fatalf("reconciliation pushed %d status frames to the owner", len(frames))
	}
}

func TestStatusReconcileDoesNotPushUnchangedViewers(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
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

	owner, err := s.CreateUser(ctx, "+15553000904")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	hidden, err := s.CreateUser(ctx, "+15553000905")
	if err != nil {
		t.Fatalf("create hidden viewer: %v", err)
	}
	blocked, err := s.CreateUser(ctx, "+15553000906")
	if err != nil {
		t.Fatalf("create blocked viewer: %v", err)
	}
	unrelated, err := s.CreateUser(ctx, "+15553000907")
	if err != nil {
		t.Fatalf("create unrelated contact: %v", err)
	}
	for i, viewer := range []store.User{hidden, blocked} {
		if _, _, _, _, err := s.SendMessage(ctx, owner.ID, viewer.ID, "status", int64(i+3), 0, 0); err != nil {
			t.Fatalf("create dialog with viewer %d: %v", viewer.ID, err)
		}
	}
	if err := s.SetLastSeenPrivacy(ctx, owner.ID, store.LastSeenPrivacyRule{BaseRule: "nobody"}); err != nil {
		t.Fatalf("hide owner status: %v", err)
	}
	if _, err := s.BlockUser(ctx, owner.ID, blocked.ID); err != nil {
		t.Fatalf("block viewer: %v", err)
	}

	registry := mtproto.NewSessionRegistry()
	hiddenTransport := &recordingNotifyTransport{}
	blockedTransport := &recordingNotifyTransport{}
	connections := []struct {
		userID    int64
		transport *recordingNotifyTransport
	}{{hidden.ID, hiddenTransport}, {blocked.ID, blockedTransport}}
	for _, connection := range connections {
		conn := mtproto.NewTestConn(connection.transport, replyTestKey())
		conn.SetOwner(connection.userID)
		if !registry.Add(connection.userID, conn) {
			t.Fatalf("register viewer %d connection", connection.userID)
		}
		t.Cleanup(func() { registry.Remove(connection.userID, conn) })
	}
	updater := NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver())
	callbacks := make(chan bool, 8)
	_, stop, err := store.StartListener(ctx, dsn,
		nil, nil, nil, nil, nil,
		func(ctx context.Context, userID int64, online bool) {
			updater.DeliverStatus(ctx, userID, online)
			if userID == owner.ID {
				callbacks <- store.StatusReconcileFromContext(ctx)
			}
		},
		nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("start notification listener: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop notification listener: %v", err)
		}
	})
	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for notification listener: %v", err)
	}

	waitCallback := func(wantReconcile bool) {
		t.Helper()
		select {
		case got := <-callbacks:
			if got != wantReconcile {
				t.Fatalf("status callback reconcile = %v, want %v", got, wantReconcile)
			}
		case <-ctx.Done():
			t.Fatalf("wait for status callback: %v", ctx.Err())
		}
	}
	statusBarrier := func() {
		t.Helper()
		if err := s.Notify(ctx, store.ChannelStatus, store.StatusPayload(owner.ID, true)); err != nil {
			t.Fatalf("send owner status barrier: %v", err)
		}
	}
	assertNoViewerPush := func(when string) {
		t.Helper()
		if got := hiddenTransport.count(); got != 0 {
			t.Errorf("hidden viewer received %d status frames after %s", got, when)
		}
		if got := blockedTransport.count(); got != 0 {
			t.Errorf("blocked viewer received %d status frames after %s", got, when)
		}
	}

	if _, err := s.AddContact(ctx, owner.ID, unrelated.ID); err != nil {
		t.Fatalf("add unrelated contact: %v", err)
	}
	statusBarrier()
	waitCallback(false)
	assertNoViewerPush("adding an unrelated contact")

	if err := s.SetLastSeenPrivacy(ctx, owner.ID, store.LastSeenPrivacyRule{BaseRule: "nobody"}); err != nil {
		t.Fatalf("save unchanged privacy rule: %v", err)
	}
	statusBarrier()
	waitCallback(false)
	assertNoViewerPush("saving an unchanged privacy rule")
}
