package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
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

type blockingStickerSetNotifyTransport struct {
	stickerSetNotifyTransport

	entered chan struct{}
	release <-chan struct{}
	first   sync.Once
}

func (t *blockingStickerSetNotifyTransport) Send(ctx context.Context, b *bin.Buffer) error {
	blocked := false
	t.first.Do(func() {
		blocked = true
		close(t.entered)
	})
	if blocked {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.release:
		}
	}
	return t.stickerSetNotifyTransport.Send(ctx, b)
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

func TestStickerSetReorderPushesKindOrderToSessions(t *testing.T) {
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
	if !waitStickerSetNotifyWrites(1, callerTransport, localTransport, remoteTransport) {
		t.Fatalf("sticker-set order writes: caller=%d local=%d remote=%d, want one on every session", callerTransport.sends.Load(), localTransport.sends.Load(), remoteTransport.sends.Load())
	}
	for name, transport := range map[string]*stickerSetNotifyTransport{"originating session": callerTransport, "local sibling": localTransport, "remote session": remoteTransport} {
		update := decodeStickerSetOrder(t, transport.lastSent())
		if update.Masks || update.Emojis {
			t.Errorf("%s update flags = masks:%t emojis:%t, want regular flags false", name, update.Masks, update.Emojis)
		}
		if !slices.Equal(update.Order, []int64{ids[1], ids[0]}) {
			t.Errorf("%s update order = %v, want %v", name, update.Order, []int64{ids[1], ids[0]})
		}
		body := decryptPushedFrame(t, transport.lastSent())
		if bytes.Contains(body, littleEndianUint32(tg.UpdateStickerSetsTypeID)) {
			t.Errorf("%s frame carries plain updateStickerSets, want only the order update", name)
		}
	}
}

func TestStickerSetReorderReconcilesOriginAfterOlderPush(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs := testBlobs(t)
	dsn := pgtest.DSN(t)
	s := openStickerStore(t, blobs, dsn)
	owner, err := s.CreateUser(ctx, "+15551830022")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ids := createInstalledUserStickerSets(t, s, fullChannelDispatcher(s), blobs, owner.ID, "push_race_alpha", "push_race_bravo")

	registry := mtproto.NewSessionRegistry()
	release := make(chan struct{})
	transport := &blockingStickerSetNotifyTransport{entered: make(chan struct{}), release: release}
	conn := mtproto.NewTestConn(transport, testKey())
	conn.SetOwner(owner.ID)
	conn.SetSession(22)
	if !registry.Add(owner.ID, conn) {
		t.Fatal("register originating session")
	}
	var releaseOnce sync.Once
	releaseFirstPush := func() { releaseOnce.Do(func() { close(release) }) }
	firstDone := make(chan struct{})
	firstStarted := false
	t.Cleanup(func() {
		releaseFirstPush()
		if firstStarted {
			select {
			case <-firstDone:
			case <-time.After(5 * time.Second):
				t.Error("older sticker-set order push did not finish after release")
			}
		}
		registry.Remove(owner.ID, conn)
	})

	updater := api.NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver())
	olderOrder := []int64{ids[1], ids[0]}
	if _, err := s.ReorderInstalledStickerSets(ctx, owner.ID, olderOrder); err != nil {
		t.Fatalf("commit older reorder: %v", err)
	}
	authKeyID := conn.AuthKeyID()
	firstStarted = true
	go func() {
		updater.DeliverStickerSets(ctx, owner.ID, authKeyID, 11, &store.StickerSetOrder{Kind: "regular", IDs: olderOrder})
		close(firstDone)
	}()
	select {
	case <-transport.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("older order push did not reach the originating session")
	}

	latestOrder := []int64{ids[0], ids[1]}
	committed, err := s.ReorderInstalledStickerSets(ctx, owner.ID, latestOrder)
	if err != nil {
		t.Fatalf("commit newer reorder: %v", err)
	}
	if len(committed) != len(latestOrder) || committed[0].ID != latestOrder[0] || committed[1].ID != latestOrder[1] {
		t.Fatalf("newer committed order = %v, want %v", committed, latestOrder)
	}
	releaseFirstPush()
	select {
	case <-firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("older order push did not finish after release")
	}
	if got := decodeStickerSetOrder(t, transport.lastSent()).Order; !slices.Equal(got, olderOrder) {
		t.Fatalf("delayed older order = %v, want %v", got, olderOrder)
	}

	updater.DeliverStickerSets(ctx, owner.ID, authKeyID, 22, &store.StickerSetOrder{Kind: "regular", IDs: latestOrder})
	if got := transport.sends.Load(); got != 2 {
		t.Fatalf("originating session order writes = %d, want older and reconciliation updates", got)
	}
	if got := decodeStickerSetOrder(t, transport.lastSent()).Order; !slices.Equal(got, latestOrder) {
		t.Errorf("originating session's final order = %v, want committed order %v", got, latestOrder)
	}
}

func TestStickerSetReorderPushesMaskAndEmojiFlags(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		masks  bool
		emojis bool
	}{
		{name: "mask", masks: true},
		{name: "emoji", emojis: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			blobs := testBlobs(t)
			dsn := pgtest.DSN(t)
			s := openStickerStore(t, blobs, dsn)
			owner, err := s.CreateUser(ctx, "+15551830021")
			if err != nil {
				t.Fatalf("create owner: %v", err)
			}
			h := fullChannelDispatcher(s)
			ids := createInstalledUserStickerSetsOfKind(t, s, h, blobs, owner.ID, tc.masks, tc.emojis, "push_kind_alpha", "push_kind_bravo")

			registry := mtproto.NewSessionRegistry()
			caller, callerTransport := newStickerSetNotifyConn(t, registry, owner.ID, 0)
			_, siblingTransport := newStickerSetNotifyConn(t, registry, owner.ID, 12)
			if caller.AuthKeyID() == 0 {
				t.Fatal("test caller must have an auth key")
			}
			startStickerSetNotifyListener(t, ctx, dsn, api.NewUpdater(s, 2, registry, nil, pgtest.PeerDeriver()))
			if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
				t.Fatalf("wait for sticker-set listener: %v", err)
			}

			wantOrder := []int64{ids[1], ids[0]}
			if got := reorderStickerSets(t, h, owner.ID, tc.masks, tc.emojis, wantOrder); got != "" {
				t.Fatalf("%s reorder = %q, want boolTrue", tc.name, got)
			}
			if !waitStickerSetNotifyWrites(1, callerTransport, siblingTransport) {
				t.Fatalf("session writes: caller=%d sibling=%d, want one on both", callerTransport.sends.Load(), siblingTransport.sends.Load())
			}
			for name, transport := range map[string]*stickerSetNotifyTransport{"originating session": callerTransport, "sibling": siblingTransport} {
				update := decodeStickerSetOrder(t, transport.lastSent())
				if update.Masks != tc.masks || update.Emojis != tc.emojis {
					t.Errorf("%s %s update flags = masks:%t emojis:%t, want masks:%t emojis:%t", tc.name, name, update.Masks, update.Emojis, tc.masks, tc.emojis)
				}
				if !slices.Equal(update.Order, wantOrder) {
					t.Errorf("%s %s update order = %v, want %v", tc.name, name, update.Order, wantOrder)
				}
			}
		})
	}
}

func decodeStickerSetOrder(t *testing.T, frame []byte) *tg.UpdateStickerSetsOrder {
	t.Helper()
	body := decryptPushedFrame(t, frame)
	start := bytes.Index(body, littleEndianUint32(tg.UpdateStickerSetsOrderTypeID))
	if start < 0 {
		t.Fatalf("frame lacks updateStickerSetsOrder: %x", body)
	}
	var update tg.UpdateStickerSetsOrder
	if err := update.Decode(&bin.Buffer{Buf: body[start:]}); err != nil {
		t.Fatalf("decode updateStickerSetsOrder: %v", err)
	}
	return &update
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
