package e2e_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

var errMediaDroppedPutCause = errors.New("fake S3 put operation failed")

type mediaMissingPutGate struct {
	blob.Store

	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release <-chan struct{}
}

func (b *mediaMissingPutGate) arm() (<-chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entered := make(chan struct{})
	release := make(chan struct{})
	b.armed = true
	b.entered = entered
	b.release = release
	var once sync.Once
	return entered, func() { once.Do(func() { close(release) }) }
}

func (b *mediaMissingPutGate) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	b.mu.Lock()
	if !b.armed {
		b.mu.Unlock()
		return b.Store.Put(ctx, key, r)
	}
	b.armed = false
	entered, release := b.entered, b.release
	b.mu.Unlock()
	close(entered)
	select {
	case <-release:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	return b.Store.Put(ctx, key, r)
}

type mediaCauseDroppingPut struct {
	blob.Store

	lastFailedRead atomic.Int64
}

type mediaReadCounter struct {
	io.Reader

	bytesRead int64
}

func (r *mediaReadCounter) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func (b *mediaCauseDroppingPut) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	counted := &mediaReadCounter{Reader: r}
	n, err := b.Store.Put(ctx, key, counted)
	if err != nil {
		b.lastFailedRead.Store(counted.bytesRead)
		return n, errMediaDroppedPutCause
	}
	return n, nil
}

func bootMediaMissingPayloadEnv(t *testing.T, ctx context.Context) (*store.Store, *store.Store, *mediaMissingPutGate, *mediaCauseDroppingPut, *blob.Local, *mediaClient, *mediaClient, *mediaClient) {
	t.Helper()
	dsn := pgtest.DSN(t)
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	partBlobs := &mediaMissingPutGate{Store: local}
	assembledBlobs := &mediaCauseDroppingPut{Store: partBlobs}
	openStore := func() *store.Store {
		st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(partBlobs))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() {
			if err := st.Close(); err != nil {
				t.Errorf("close store: %v", err)
			}
		})
		return st
	}
	stA, stB := openStore(), openStore()
	const (
		ownerPhone     = "+15551992001"
		recipientPhone = "+15551992002"
		dcID           = 2
	)
	seedPhoneUsers(t, ctx, stA, ownerPhone, recipientPhone)

	key, err := rsakey.LoadOrGenerate(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatalf("RSA key: %v", err)
	}
	codes := newMultiCodeSink()
	logA, logB := codes.Logger(), codes.Logger()
	lnA := mustListen(t, ctx, "127.0.0.1:0")
	lnB := mustListen(t, ctx, "127.0.0.1:0")
	stopA := bootMediaMissingPayloadReplica(t, ctx, key, dcID, stA, dsn, logA, lnA, assembledBlobs)
	t.Cleanup(stopA)
	stopB := bootMediaMissingPayloadReplica(t, ctx, key, dcID, stB, dsn, logB, lnB, assembledBlobs)
	t.Cleanup(stopB)
	addrA, ok := lnA.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("replica A listener address is not TCP")
	}
	addrB, ok := lnB.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("replica B listener address is not TCP")
	}
	portA, portB := addrA.Port, addrB.Port
	ownerA := startMediaMissingPayloadClient(t, ctx, portA, key, dcID, ownerPhone, codes)
	ownerB := startMediaMissingPayloadClient(t, ctx, portB, key, dcID, ownerPhone, codes)
	recipient := startMediaMissingPayloadClient(t, ctx, portB, key, dcID, recipientPhone, codes)
	if ownerA.id != ownerB.id {
		t.Fatalf("replica user ids differ: A=%d B=%d", ownerA.id, ownerB.id)
	}
	return stA, stB, partBlobs, assembledBlobs, local, ownerA, ownerB, recipient
}

func bootMediaMissingPayloadReplica(
	t *testing.T, ctx context.Context, key *rsa.PrivateKey, dcID int, st *store.Store,
	dsn string, log *slog.Logger, ln net.Listener, blobs blob.Store,
) func() {
	t.Helper()
	tgcfg := fixtureConfigForListener(t, dcID, ln)
	dialogFilterSync := api.NewDialogFilterSync()
	handler := api.NewWithDialogFilterSync(st, dcID, tgcfg, log, true, 100<<20, blobs, 2<<30, pgtest.PeerDeriver(), config.DefaultRateLimits(), config.RegistrationClosed, dialogFilterSync)
	server := mtproto.New(exchange.PrivateKey{RSA: key}, dcID, mtproto.NewPgAuthKeyStore(st), handler, log)
	updater := api.NewUpdaterWithDialogFilterSync(st, server.Registry(), log, pgtest.PeerDeriver(), dialogFilterSync)
	_, stopListener, err := store.StartListenerWithDialogFilters(ctx, dsn, updater.Deliver, updater.DeliverTyping, updater.Evict, updater.DeliverChannelPost, updater.DeliverEncryption, updater.DeliverStatus, updater.DeliverEncryptedMsg, updater.DeliverReactions, updater.DeliverPinned, updater.MarkDialogFilters, updater.DialogFilterListenerReconnected, log)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	stopDialogFilterRecovery := updater.StartDialogFilterRecovery(ctx)

	srvCtx, srvCancel := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(srvCtx, ln) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			srvCancel()
			if serr := <-serveErr; serr != nil && !errors.Is(serr, context.Canceled) {
				t.Errorf("server serve: %v", serr)
			}
			if lerr := stopListener(); lerr != nil {
				t.Errorf("listener stop: %v", lerr)
			}
			stopDialogFilterRecovery()
		})
	}
}

func startMediaMissingPayloadClient(t *testing.T, ctx context.Context, port int, key *rsa.PrivateKey, dcID int, phone string, codes *multiCodeSink) *mediaClient {
	t.Helper()
	mc := &mediaClient{
		cmds:  make(chan command),
		coll:  newUpdateCollector(),
		errCh: make(chan error, 1),
	}
	client := createClient(port, key, dcID, mc.coll, nil)
	idCh := make(chan int64, 1)
	go func() { mc.errCh <- runInteractive(ctx, client, flowFor(phone, codes), idCh, mc.cmds) }()
	t.Cleanup(func() {
		close(mc.cmds)
		if err := <-mc.errCh; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("client run: %v", err)
		}
	})
	select {
	case mc.id = <-idCh:
	case <-ctx.Done():
		t.Fatalf("client %s login timeout: %v", phone, ctx.Err())
	}
	return mc
}

type mediaEffectSnapshot struct {
	senderState    store.State
	recipientState store.State
	recipientCount int
}

func snapshotMediaEffects(t *testing.T, ctx context.Context, st *store.Store, senderID, recipientID int64) mediaEffectSnapshot {
	t.Helper()
	senderState, err := st.State(ctx, senderID)
	if err != nil {
		t.Fatalf("sender state: %v", err)
	}
	recipientState, err := st.State(ctx, recipientID)
	if err != nil {
		t.Fatalf("recipient state: %v", err)
	}
	recipientMessages, err := st.History(ctx, recipientID, store.PeerTypeUser, senderID, 0, 100)
	if err != nil {
		t.Fatalf("recipient history: %v", err)
	}
	return mediaEffectSnapshot{senderState: senderState, recipientState: recipientState, recipientCount: len(recipientMessages)}
}

func assertNoFailedMediaEffects(t *testing.T, ctx context.Context, st *store.Store, senderID, recipientID int64, before mediaEffectSnapshot) {
	t.Helper()
	after := snapshotMediaEffects(t, ctx, st, senderID, recipientID)
	if after.senderState != before.senderState || after.recipientState != before.recipientState {
		t.Fatalf("failed send changed update state: sender %+v -> %+v, recipient %+v -> %+v", before.senderState, after.senderState, before.recipientState, after.recipientState)
	}
	for userID, pts := range map[int64]int{senderID: before.senderState.Pts, recipientID: before.recipientState.Pts} {
		events, err := st.EventsSince(ctx, userID, pts)
		if err != nil {
			t.Fatalf("events since failed send for user %d: %v", userID, err)
		}
		if len(events) != 0 {
			t.Fatalf("failed send wrote %d events for user %d", len(events), userID)
		}
	}
	if after.recipientCount != before.recipientCount {
		t.Fatalf("failed send changed recipient history count: %d -> %d", before.recipientCount, after.recipientCount)
	}
}

func assertFailedMediaAssemblyUnstored(t *testing.T, ctx context.Context, st *store.Store, blobs blob.Store, beforeID int64) int64 {
	t.Helper()
	failedID, err := st.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("failed file id ceiling: %v", err)
	}
	if failedID != beforeID+1 {
		t.Fatalf("failed assembly allocated id %d, want %d", failedID, beforeID+1)
	}
	ids, err := st.ExistingFileIDs(ctx, []int64{failedID})
	if err != nil {
		t.Fatalf("failed file rows: %v", err)
	}
	if _, ok := ids[failedID]; !ok {
		t.Fatalf("failed assembly %d did not retain its not-stored row", failedID)
	}
	files, err := st.FilesByIDs(ctx, []int64{failedID})
	if err != nil {
		t.Fatalf("stored failed files: %v", err)
	}
	if _, ok := files[failedID]; ok {
		t.Fatalf("failed assembly %d has a stored file row", failedID)
	}
	if _, err := blobs.ReadAt(ctx, blob.Key(failedID), 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("failed assembly blob = %v, want ErrNotFound", err)
	}
	return failedID
}

func TestMediaMissingUploadPayload(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	stA, stB, gate, droppedPut, local, ownerA, ownerB, recipient := bootMediaMissingPayloadEnv(t, ctx)
	peer := peerUser(ownerB.id, recipient.id)
	const firstFileID = int64(0x5ED12751)
	want := mediaPayload(mediaPartSize + 7)
	old := bytes.Repeat([]byte{0xA7}, mediaPartSize)
	old = append(old, want[mediaPartSize:]...)
	if parts := uploadParts(t, ctx, ownerA, firstFileID, old); parts != 2 {
		t.Fatalf("initial upload parts = %d, want 2", parts)
	}
	oldKey, _, ok, err := stA.UploadPartKey(ctx, ownerA.id, firstFileID, 0)
	if err != nil || !ok {
		t.Fatalf("original part key = %q, found=%v, err=%v", oldKey, ok, err)
	}

	before, err := stB.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling before first failure: %v", err)
	}
	effects := snapshotMediaEffects(t, ctx, stB, ownerB.id, recipient.id)
	entered, release := gate.arm()
	defer release()
	replacementDone := make(chan error, 1)
	replacement := command{
		fn: func(ctx context.Context, client *tg.Client) error {
			ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: firstFileID, FilePart: 0, Bytes: want[:mediaPartSize]})
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("replacement saveFilePart returned false")
			}
			return nil
		},
		done: replacementDone,
	}
	select {
	case ownerA.cmds <- replacement:
	case <-ctx.Done():
		t.Fatalf("replacement enqueue timeout: %v", ctx.Err())
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("replacement put gate timeout: %v", ctx.Err())
	}
	newKey, size, ok, err := stB.UploadPartKey(ctx, ownerB.id, firstFileID, 0)
	if err != nil || !ok || newKey == oldKey || size != mediaPartSize {
		t.Fatalf("committed replacement row key=%q size=%d found=%v err=%v", newKey, size, ok, err)
	}
	if _, err := local.ReadAt(ctx, newKey, 0, size); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("paused replacement bytes = %v, want ErrNotFound", err)
	}
	err = execMedia(t, ctx, ownerB.cmds, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer,
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: firstFileID, Parts: 2, Name: "retry.bin"},
				MimeType: "application/octet-stream",
			},
			Message:  "must not send while replacement bytes are missing",
			RandomID: 920001,
		})
		return err
	})
	assertRPCError(t, err, "MEDIA_INVALID")
	if droppedPut.lastFailedRead.Load() != 0 {
		t.Fatalf("missing first part streamed %d bytes before error, want 0", droppedPut.lastFailedRead.Load())
	}
	assertNoFailedMediaEffects(t, ctx, stB, ownerB.id, recipient.id, effects)
	failedFirstID := assertFailedMediaAssemblyUnstored(t, ctx, stB, local, before)
	select {
	case msg := <-recipient.coll.newMsg:
		t.Fatalf("recipient received a message for failed assembly %d: %+v", failedFirstID, msg)
	default:
	}

	release()
	if err := <-replacementDone; err != nil {
		t.Fatalf("replacement save: %v", err)
	}
	doc := sendUploadedDocument(t, ctx, ownerB, peer, firstFileID, 2, "retry.bin", "retry", 920001)
	if got := downloadDocument(t, ctx, ownerB, doc); !bytes.Equal(got, want) {
		t.Fatalf("first retry downloaded %d bytes, want %d identical bytes", len(got), len(want))
	}
	recvOrCtx(t, ctx, recipient.coll.newMsg, "first successful media retry")

	const laterFileID = int64(0x5ED12752)
	if parts := uploadParts(t, ctx, ownerA, laterFileID, want); parts != 2 {
		t.Fatalf("later-part upload parts = %d, want 2", parts)
	}
	laterKey, _, ok, err := stA.UploadPartKey(ctx, ownerA.id, laterFileID, 1)
	if err != nil || !ok {
		t.Fatalf("later part key = %q, found=%v, err=%v", laterKey, ok, err)
	}
	if err := local.Remove(ctx, laterKey); err != nil {
		t.Fatalf("remove later part bytes: %v", err)
	}
	effects = snapshotMediaEffects(t, ctx, stB, ownerB.id, recipient.id)
	before, err = stB.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling before later failure: %v", err)
	}
	err = execMedia(t, ctx, ownerB.cmds, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer,
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: laterFileID, Parts: 2, Name: "later.bin"},
				MimeType: "application/octet-stream",
			},
			Message:  "must not send a truncated upload",
			RandomID: 920002,
		})
		return err
	})
	assertRPCError(t, err, "MEDIA_INVALID")
	if got := droppedPut.lastFailedRead.Load(); got != mediaPartSize {
		t.Fatalf("missing later part streamed %d bytes, want prior part size %d", got, mediaPartSize)
	}
	assertNoFailedMediaEffects(t, ctx, stB, ownerB.id, recipient.id, effects)
	failedLaterID := assertFailedMediaAssemblyUnstored(t, ctx, stB, local, before)
	select {
	case msg := <-recipient.coll.newMsg:
		t.Fatalf("recipient received a message for failed assembly %d: %+v", failedLaterID, msg)
	default:
	}

	execChat(t, ctx, ownerA.cmds, func(ctx context.Context, client *tg.Client) error {
		ok, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: laterFileID, FilePart: 1, Bytes: want[mediaPartSize:]})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("restore saveFilePart returned false")
		}
		return nil
	})
	doc = sendUploadedDocument(t, ctx, ownerB, peer, laterFileID, 2, "later.bin", "retry", 920002)
	if got := downloadDocument(t, ctx, ownerB, doc); !bytes.Equal(got, want) {
		t.Fatalf("later retry downloaded %d bytes, want %d identical bytes", len(got), len(want))
	}
}
