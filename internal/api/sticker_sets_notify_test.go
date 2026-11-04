package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type stickerSetNotifyTransport struct {
	sends atomic.Int32
	mu    sync.Mutex
	sent  [][]byte
}

func (t *stickerSetNotifyTransport) Send(_ context.Context, b *bin.Buffer) error {
	t.sends.Add(1)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sent = append(t.sent, bytes.Clone(b.Buf))
	return nil
}

func (t *stickerSetNotifyTransport) lastSent() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.sent) == 0 {
		return nil
	}
	return t.sent[len(t.sent)-1]
}

func (*stickerSetNotifyTransport) Recv(context.Context, *bin.Buffer) error {
	return errors.New("unused")
}

func (*stickerSetNotifyTransport) Close() error { return nil }

func newStickerSetNotifyConn(t *testing.T, registry *mtproto.SessionRegistry, ownerID, sessionID int64) (*mtproto.Conn, *stickerSetNotifyTransport) {
	t.Helper()
	transport := &stickerSetNotifyTransport{}
	conn := mtproto.NewTestConn(transport, testKey())
	conn.SetOwner(ownerID)
	conn.SetSession(sessionID)
	if !registry.Add(ownerID, conn) {
		t.Fatalf("register session %d for owner %d", sessionID, ownerID)
	}
	t.Cleanup(func() { registry.Remove(ownerID, conn) })
	return conn, transport
}

func startStickerSetNotifyListener(t *testing.T, ctx context.Context, dsn string, updater *api.Updater) {
	t.Helper()
	_, stop, err := store.StartListenerWithDialogPinsAndStickerSets(
		ctx, dsn,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		updater.DeliverStickerSets,
		nil, nil,
	)
	if err != nil {
		t.Fatalf("start sticker-set listener: %v", err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Errorf("stop sticker-set listener: %v", err)
		}
	})
}

func waitStickerSetNotifyWrites(want int32, transports ...*stickerSetNotifyTransport) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, transport := range transports {
			if transport.sends.Load() < want {
				ready = false
				break
			}
		}
		if ready {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func TestStickerSetInstallPublishesCrossReplicaNudge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551810051")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect notification listener: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(ctx); err != nil {
			t.Errorf("close notification listener: %v", err)
		}
	})
	if _, err := listener.Exec(ctx, "LISTEN tg_sticker_sets"); err != nil {
		t.Fatalf("listen for sticker-set update: %v", err)
	}

	if got := installStickerSet(t, fullChannelDispatcher(s), owner.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}); got != "success" {
		t.Fatalf("install result = %q, want success", got)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	notification, err := listener.WaitForNotification(waitCtx)
	if err != nil {
		t.Fatalf("sticker-set install did not publish its replica notification: %v", err)
	}
	want := fmt.Sprintf("%d|%d|0", owner.ID, mtproto.AuthKeyIDInt64(testKey().ID))
	if notification.Payload != want {
		t.Fatalf("sticker-set notification payload = %q, want %q", notification.Payload, want)
	}
}

func TestStickerSetInstallPushesAcrossReplicasToOtherSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	owner, err := s.CreateUser(ctx, "+15551810052")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}

	firstRegistry := mtproto.NewSessionRegistry()
	secondRegistry := mtproto.NewSessionRegistry()
	caller, callerTransport := newStickerSetNotifyConn(t, firstRegistry, owner.ID, 0)
	localSession, localTransport := newStickerSetNotifyConn(t, firstRegistry, owner.ID, 12)
	remoteSession, remoteTransport := newStickerSetNotifyConn(t, secondRegistry, owner.ID, 23)
	if caller.AuthKeyID() != localSession.AuthKeyID() || caller.AuthKeyID() != remoteSession.AuthKeyID() {
		t.Fatal("test sessions must share the same auth key")
	}

	staleSession, staleTransport := newStickerSetNotifyConn(t, firstRegistry, owner.ID, 34)
	staleSession.SetOwner(owner.ID + 1000)

	firstUpdater := api.NewUpdater(s, 2, firstRegistry, nil, pgtest.PeerDeriver())
	secondUpdater := api.NewUpdater(s, 2, secondRegistry, nil, pgtest.PeerDeriver())
	startStickerSetNotifyListener(t, ctx, dsn, firstUpdater)
	startStickerSetNotifyListener(t, ctx, dsn, secondUpdater)
	if err := store.WaitForNotificationListener(ctx, s, 2); err != nil {
		t.Fatalf("wait for two replica listeners: %v", err)
	}

	if got := installStickerSet(t, fullChannelDispatcher(s), owner.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}); got != "success" {
		t.Fatalf("install result = %q, want success", got)
	}
	if !waitStickerSetNotifyWrites(1, localTransport, remoteTransport) {
		t.Fatalf("sticker-set update writes: local=%d remote=%d, want one on both replicas", localTransport.sends.Load(), remoteTransport.sends.Load())
	}
	if !uninstallStickerSet(t, fullChannelDispatcher(s), owner.ID, &tg.InputStickerSetShortName{ShortName: "twemoji_reactions"}) {
		t.Fatal("uninstall returned false")
	}
	if !waitStickerSetNotifyWrites(2, localTransport, remoteTransport) {
		t.Fatalf("sticker-set uninstall writes: local=%d remote=%d, want two on both replicas", localTransport.sends.Load(), remoteTransport.sends.Load())
	}
	if got := callerTransport.sends.Load(); got != 0 {
		t.Errorf("originating session writes = %d, want 0", got)
	}
	if got := staleTransport.sends.Load(); got != 0 {
		t.Errorf("stale owner session writes = %d, want 0", got)
	}
	if got := localTransport.sends.Load(); got != 2 {
		t.Errorf("local sibling writes = %d, want two", got)
	}
	if got := remoteTransport.sends.Load(); got != 2 {
		t.Errorf("remote session writes = %d, want two", got)
	}
	for name, conn := range map[string]*mtproto.Conn{
		"local sibling":  localSession,
		"remote session": remoteSession,
	} {
		if got := conn.LastPushedPts(); got != 0 {
			t.Errorf("%s pts watermark = %d, want 0", name, got)
		}
	}
}

func TestStickerSetReorderPushesKindOrderToOtherSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551830020")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	h := fullChannelDispatcher(s)
	ids := createInstalledUserStickerSets(t, s, h, blobs, owner.ID, "push_alpha", "push_bravo")

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect notification listener: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(ctx); err != nil {
			t.Errorf("close notification listener: %v", err)
		}
	})
	if _, err := listener.Exec(ctx, "LISTEN tg_sticker_sets"); err != nil {
		t.Fatalf("listen for sticker-set update: %v", err)
	}

	firstRegistry := mtproto.NewSessionRegistry()
	secondRegistry := mtproto.NewSessionRegistry()
	caller, callerTransport := newStickerSetNotifyConn(t, firstRegistry, owner.ID, 0)
	_, localTransport := newStickerSetNotifyConn(t, firstRegistry, owner.ID, 12)
	_, remoteTransport := newStickerSetNotifyConn(t, secondRegistry, owner.ID, 23)
	if caller.AuthKeyID() == 0 {
		t.Fatal("test caller must have an auth key")
	}

	startStickerSetNotifyListener(t, ctx, dsn, api.NewUpdater(s, 2, firstRegistry, nil, pgtest.PeerDeriver()))
	startStickerSetNotifyListener(t, ctx, dsn, api.NewUpdater(s, 2, secondRegistry, nil, pgtest.PeerDeriver()))
	if err := store.WaitForNotificationListener(ctx, s, 2); err != nil {
		t.Fatalf("wait for two replica listeners: %v", err)
	}

	if got := reorderStickerSets(t, fullChannelDispatcher(s), owner.ID, false, false, []int64{ids[1], ids[0]}); got != "" {
		t.Fatalf("reorder = %q, want boolTrue", got)
	}
	if !waitStickerSetNotifyWrites(1, localTransport, remoteTransport) {
		t.Fatalf("sticker-set order writes: local=%d remote=%d, want one on both replicas", localTransport.sends.Load(), remoteTransport.sends.Load())
	}
	if got := callerTransport.sends.Load(); got != 0 {
		t.Errorf("originating session writes = %d, want 0", got)
	}
	for name, transport := range map[string]*stickerSetNotifyTransport{"local sibling": localTransport, "remote session": remoteTransport} {
		body := decryptPushedFrame(t, transport.lastSent())
		if !bytes.Contains(body, littleEndianUint32(tg.UpdateStickerSetsOrderTypeID)) {
			t.Errorf("%s frame lacks updateStickerSetsOrder: %x", name, body)
		}
		if bytes.Contains(body, littleEndianUint32(tg.UpdateStickerSetsTypeID)) {
			t.Errorf("%s frame carries plain updateStickerSets, want only the order update", name)
		}
		if !bytes.Contains(body, littleEndianInt64(t, ids[1])) || !bytes.Contains(body, littleEndianInt64(t, ids[0])) {
			t.Errorf("%s frame lacks the reordered set ids %v", name, ids)
		}
	}
}

// decryptPushedFrame returns the plaintext TL message of a frame the server
// wrote, decrypted with the test auth key as a client would.
func decryptPushedFrame(t *testing.T, frame []byte) []byte {
	t.Helper()
	data, err := crypto.NewClientCipher(crypto.DefaultRand()).DecryptFromBuffer(testKey(), &bin.Buffer{Buf: frame})
	if err != nil {
		t.Fatalf("decrypt pushed frame: %v", err)
	}
	return data.MessageDataWithPadding[:data.MessageDataLen]
}

func littleEndianUint32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

func littleEndianInt64(t *testing.T, v int64) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, v); err != nil {
		t.Fatalf("encode %d: %v", v, err)
	}
	return buf.Bytes()
}
