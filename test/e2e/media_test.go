package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

// mediaPartSize is the protocol part size, used both as the upload part size
// and as the download window.
const mediaPartSize = 512 * 1024

// mediaClient is one logged-in gotd client: its user id, its command channel
// and the updates it received.
type mediaClient struct {
	id    int64
	cmds  chan command
	coll  *updateCollector
	errCh chan error
}

type mediaReplicaEnv struct {
	ownerA *mediaClient
	ownerB *mediaClient
	others []*mediaClient
	stores [2]*store.Store
	local  *blob.Local
	blobs  *gatedMediaBlobs
	dsn    string
}

type gatedMediaBlobs struct {
	blob.Store

	mu   sync.Mutex
	gate *mediaPartPutGate
}

type mediaPartPutGate struct {
	started chan string
	release chan struct{}
	once    sync.Once
}

func (b *gatedMediaBlobs) blockNextPartPut() *mediaPartPutGate {
	gate := &mediaPartPutGate{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	b.mu.Lock()
	b.gate = gate
	b.mu.Unlock()
	return gate
}

func (b *gatedMediaBlobs) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	b.mu.Lock()
	gate := b.gate
	if gate != nil && strings.HasPrefix(key, blob.PartsPrefix) {
		b.gate = nil
	} else {
		gate = nil
	}
	b.mu.Unlock()
	if gate == nil {
		return b.Store.Put(ctx, key, r)
	}
	return b.Store.Put(ctx, key, &mediaGateReader{Reader: r, ctx: ctx, gate: gate, key: key})
}

type mediaGateReader struct {
	io.Reader

	ctx  context.Context
	gate *mediaPartPutGate
	key  string
	once sync.Once
}

func (r *mediaGateReader) Read(p []byte) (int, error) {
	r.once.Do(func() { r.gate.started <- r.key })
	select {
	case <-r.gate.release:
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	}
	return r.Reader.Read(p)
}

func (g *mediaPartPutGate) unblock() {
	g.once.Do(func() { close(g.release) })
}

// bootMediaEnv boots a server with delivery and logs in one client per phone.
// The blob directory comes from the boot helper, which already gives each
// booted server its own t.TempDir(), so nothing here reads the environment and
// t.Parallel() stays usable.
//
// Unlike the other e2e environments this one runs under the shipped rate-limit
// defaults rather than with limits off, so the upload path here is wired the
// way a real deployment wires it: the limit is on, keyed per account, and the
// client is a real one. What that catches is a default left at zero-window or a
// surface wired to the wrong budget — not the numbers themselves, which these
// uploads stay far below. Where the bound actually sits is a handler test.
func bootMediaEnv(t *testing.T, ctx context.Context, phones ...string) []*mediaClient {
	t.Helper()

	key, err := rsakey.LoadOrGenerate(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cerr := st.Close(); cerr != nil {
			t.Errorf("store close: %v", cerr)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	ln := mustListen(t, ctx, "127.0.0.1:0")
	port := tcpPort(t, ln)
	_, stop := bootServerWithLimits(t, ctx, key, dcID, st, dsn, codes.Logger(), ln, config.DefaultRateLimits())
	t.Cleanup(stop)

	clients := make([]*mediaClient, 0, len(phones))
	t.Cleanup(func() {
		for _, mc := range clients {
			close(mc.cmds)
			if rerr := <-mc.errCh; rerr != nil && !errors.Is(rerr, context.Canceled) && !errors.Is(rerr, context.DeadlineExceeded) {
				t.Errorf("client run: %v", rerr)
			}
		}
	})
	for _, phone := range phones {
		seedPhoneUsers(t, ctx, st, phone)
		mc := &mediaClient{
			cmds:  make(chan command),
			coll:  newUpdateCollector(),
			errCh: make(chan error, 1),
		}
		client := createClient(port, key, dcID, mc.coll, nil)
		idCh := make(chan int64, 1)
		go func() { mc.errCh <- runInteractive(ctx, client, flowFor(phone, codes), idCh, mc.cmds) }()
		select {
		case mc.id = <-idCh:
		case <-ctx.Done():
			t.Fatalf("client %s login timeout: %v", phone, ctx.Err())
		}
		clients = append(clients, mc)
	}
	return clients
}

// bootMediaReplicaEnv starts two server instances with separate Postgres pools
// against one test database and one shared local blob directory.
func bootMediaReplicaEnv(t *testing.T, ctx context.Context, phones ...string) *mediaReplicaEnv {
	t.Helper()
	if len(phones) < 2 {
		t.Fatal("media replica environment needs an owner and another account")
	}

	key, err := rsakey.LoadOrGenerate(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("shared local blob store: %v", err)
	}
	blobs := &gatedMediaBlobs{Store: local}

	var env mediaReplicaEnv
	env.dsn, env.local, env.blobs = dsn, local, blobs
	for i := range env.stores {
		st, openErr := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
		if openErr != nil {
			t.Fatalf("open replica %c store: %v", 'A'+i, openErr)
		}
		env.stores[i] = st
		t.Cleanup(func() {
			if closeErr := st.Close(); closeErr != nil {
				t.Errorf("replica %c store close: %v", 'A'+i, closeErr)
			}
		})
	}

	seedPhoneUsers(t, ctx, env.stores[0], phones...)
	codes := newMultiCodeSink()
	listeners := []net.Listener{
		mustListen(t, ctx, "127.0.0.1:0"),
		mustListen(t, ctx, "127.0.0.1:0"),
	}
	ports := [2]int{tcpPort(t, listeners[0]), tcpPort(t, listeners[1])}
	for i, ln := range listeners {
		_, stop := bootServerWithLimitsAndRegistrationModeAndBlobs(
			t, ctx, key, 2, env.stores[i], dsn, codes.Logger(), ln,
			config.DefaultRateLimits(), config.RegistrationClosed, blobs,
		)
		t.Cleanup(stop)
	}

	var clients []*mediaClient
	t.Cleanup(func() {
		for _, mc := range clients {
			close(mc.cmds)
			if runErr := <-mc.errCh; runErr != nil && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded) {
				t.Errorf("client run: %v", runErr)
			}
		}
	})
	startClient := func(port int, phone string, sess *session.StorageMemory) *mediaClient {
		mc := &mediaClient{
			cmds:  make(chan command),
			coll:  newUpdateCollector(),
			errCh: make(chan error, 1),
		}
		clients = append(clients, mc)
		client := createClient(port, key, 2, mc.coll, sess)
		idCh := make(chan int64, 1)
		go func() { mc.errCh <- runInteractive(ctx, client, flowFor(phone, codes), idCh, mc.cmds) }()
		select {
		case mc.id = <-idCh:
		case <-ctx.Done():
			t.Fatalf("client %s login timeout: %v", phone, ctx.Err())
		}
		return mc
	}

	ownerSession := &session.StorageMemory{}
	env.ownerA = startClient(ports[0], phones[0], ownerSession)
	env.ownerB = startClient(ports[1], phones[0], ownerSession)
	for _, phone := range phones[1:] {
		env.others = append(env.others, startClient(ports[1], phone, &session.StorageMemory{}))
	}
	return &env
}

// execMedia runs one command on a client and returns its error instead of
// failing the test, for the cases where the error is the assertion.
func execMedia(t *testing.T, ctx context.Context, cmds chan command, fn func(ctx context.Context, c *tg.Client) error) error {
	t.Helper()
	d := make(chan error, 1)
	select {
	case cmds <- command{fn: fn, done: d}:
	case <-ctx.Done():
		t.Fatalf("command enqueue timeout: %v", ctx.Err())
	}
	return <-d
}

func assertRPCError(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s, got nil error", want)
	}
	var tgErr *tgerr.Error
	if !errors.As(err, &tgErr) {
		t.Fatalf("error type = %T, want *tgerr.Error", err)
	}
	if tgErr.Message != want {
		t.Fatalf("error = %s, want %s", tgErr.Message, want)
	}
}

// mediaPayload builds a payload whose every byte depends on its position, so a
// part assembled out of order fails a byte comparison rather than passing on a
// repeated filler byte.
func mediaPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i)
	}
	return b
}

// uploadParts saves payload as 512 KiB parts under fileID and returns the part
// count, with the last part short whenever the payload does not divide evenly.
func uploadParts(t *testing.T, ctx context.Context, mc *mediaClient, fileID int64, payload []byte) int {
	t.Helper()
	parts := 0
	for off := 0; off < len(payload); off += mediaPartSize {
		part, chunk := parts, payload[off:min(off+mediaPartSize, len(payload))]
		saveMediaPart(t, ctx, mc, fileID, part, chunk)
		parts++
	}
	return parts
}

func saveMediaPart(t *testing.T, ctx context.Context, mc *mediaClient, fileID int64, part int, chunk []byte) {
	t.Helper()
	if err := saveMediaPartResult(t, ctx, mc, fileID, part, chunk); err != nil {
		t.Fatalf("saveFilePart %d: %v", part, err)
	}
}

func saveMediaPartResult(t *testing.T, ctx context.Context, mc *mediaClient, fileID int64, part int, chunk []byte) error {
	t.Helper()
	return execMedia(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
		ok, err := c.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: part, Bytes: chunk,
		})
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("saveFilePart %d returned false", part)
		}
		return nil
	})
}

// sendUploadedDocument sends the just-uploaded parts as a document and returns
// the document off the sender's own updateNewMessage.
func sendUploadedDocument(
	t *testing.T, ctx context.Context, mc *mediaClient, peer tg.InputPeerClass,
	fileID int64, parts int, name, caption string, randomID int64,
) *tg.Document {
	t.Helper()
	var ups tg.UpdatesClass
	execChat(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer,
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: fileID, Parts: parts, Name: name},
				MimeType: "application/octet-stream",
			},
			Message:  caption,
			RandomID: randomID,
		})
		if err != nil {
			return err
		}
		ups = res
		return nil
	})

	full, ok := ups.(*tg.Updates)
	if !ok {
		t.Fatalf("sendMedia updates type = %T, want *tg.Updates", ups)
	}
	for _, u := range full.Updates {
		newMsg, ok := u.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		msg, ok := newMsg.Message.(*tg.Message)
		if !ok {
			t.Fatalf("sendMedia message type = %T, want *tg.Message", newMsg.Message)
		}
		return documentOf(t, msg)
	}
	t.Fatal("sendMedia carried no updateNewMessage")
	return nil
}

// documentOf reads the document off a message, failing if the message carries
// no document media.
func documentOf(t *testing.T, msg *tg.Message) *tg.Document {
	t.Helper()
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("message media = %T, want *tg.MessageMediaDocument", msg.Media)
	}
	doc, ok := media.Document.(*tg.Document)
	if !ok {
		t.Fatalf("document = %T, want *tg.Document", media.Document)
	}
	return doc
}

// downloadDocument walks a document in 512 KiB windows until a reply comes back
// shorter than the window, which is how the last window is recognised without
// assuming the file divides evenly.
func downloadDocument(t *testing.T, ctx context.Context, mc *mediaClient, doc *tg.Document) []byte {
	t.Helper()
	var out []byte
	for offset := int64(0); ; {
		var chunk []byte
		at := offset
		execChat(t, ctx, mc.cmds, func(ctx context.Context, c *tg.Client) error {
			res, err := c.UploadGetFile(ctx, &tg.UploadGetFileRequest{
				Location: &tg.InputDocumentFileLocation{
					ID:            doc.ID,
					AccessHash:    doc.AccessHash,
					FileReference: doc.FileReference,
				},
				Offset: at,
				Limit:  mediaPartSize,
			})
			if err != nil {
				return err
			}
			file, ok := res.(*tg.UploadFile)
			if !ok {
				return fmt.Errorf("getFile reply type = %T, want *tg.UploadFile", res)
			}
			chunk = file.Bytes
			return nil
		})
		out = append(out, chunk...)
		offset += int64(len(chunk))
		if len(chunk) < mediaPartSize {
			return out
		}
	}
}

// assertSameDocument compares the fields a recipient must see unchanged.
func assertSameDocument(t *testing.T, got, want *tg.Document, who string) {
	t.Helper()
	if got.ID != want.ID || got.AccessHash != want.AccessHash {
		t.Fatalf("%s document (%d,%d), want (%d,%d)", who, got.ID, got.AccessHash, want.ID, want.AccessHash)
	}
	if got.Size != want.Size {
		t.Fatalf("%s document size = %d, want %d", who, got.Size, want.Size)
	}
	if got.MimeType != want.MimeType {
		t.Fatalf("%s document mime = %q, want %q", who, got.MimeType, want.MimeType)
	}
	if fileNameOf(got) != fileNameOf(want) {
		t.Fatalf("%s document name = %q, want %q", who, fileNameOf(got), fileNameOf(want))
	}
}

func waitForAdvisoryBlock(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		var blocked bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity AS waiting
				CROSS JOIN LATERAL unnest(pg_blocking_pids(waiting.pid)) AS blockers(pid)
				WHERE blockers.pid = pg_backend_pid()
				  AND waiting.wait_event_type = 'Lock'
				  AND waiting.wait_event = 'advisory'
			)
		`).Scan(&blocked)
		if err != nil {
			t.Fatalf("check advisory-lock waiter: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("replica upload did not wait on the held owner advisory lock")
		case <-ctx.Done():
			t.Fatalf("wait for advisory-lock contention: %v", ctx.Err())
		}
	}
}

func fileNameOf(doc *tg.Document) string {
	for _, a := range doc.Attributes {
		if name, ok := a.(*tg.DocumentAttributeFilename); ok {
			return name.FileName
		}
	}
	return ""
}

// TestMediaRoundTrip is the M5 gate: A uploads a multi-part payload, sends it to
// B, and B downloads bytes identical to what A uploaded, with the shipped
// rate-limit defaults in force rather than switched off.
func TestMediaRoundTrip(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clients := bootMediaEnv(t, ctx, "+15551310001", "+15551310002")
	a, b := clients[0], clients[1]

	payload := mediaPayload(mediaPartSize + 7)
	const clientFileID = int64(0x5ED10001)
	parts := uploadParts(t, ctx, a, clientFileID, payload)
	if parts != 2 {
		t.Fatalf("upload parts = %d, want 2", parts)
	}

	peerB := peerUser(a.id, b.id)
	doc := sendUploadedDocument(t, ctx, a, peerB, clientFileID, parts, "gate.bin", "here", 910001)
	if doc.Size != int64(len(payload)) {
		t.Fatalf("document size = %d, want %d", doc.Size, len(payload))
	}
	if doc.MimeType != "application/octet-stream" {
		t.Fatalf("document mime = %q", doc.MimeType)
	}
	if fileNameOf(doc) != "gate.bin" {
		t.Fatalf("document name = %q, want %q", fileNameOf(doc), "gate.bin")
	}

	// B sees the same document on its own history.
	peerA := peerUser(b.id, a.id)
	var bDoc *tg.Document
	execChat(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerA, Limit: 10})
		if err != nil {
			return err
		}
		msgs, ok := res.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("history type = %T, want *tg.MessagesMessages", res)
		}
		if len(msgs.Messages) != 1 {
			return fmt.Errorf("history len = %d, want 1", len(msgs.Messages))
		}
		msg, ok := msgs.Messages[0].(*tg.Message)
		if !ok {
			return fmt.Errorf("history message type = %T, want *tg.Message", msgs.Messages[0])
		}
		if msg.Message != "here" {
			return fmt.Errorf("caption = %q, want %q", msg.Message, "here")
		}
		bDoc = documentOf(t, msg)
		return nil
	})
	assertSameDocument(t, bDoc, doc, "B")

	// The gate: the bytes B downloads are the bytes A uploaded.
	got := downloadDocument(t, ctx, b, bDoc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("downloaded %d bytes, want %d identical bytes", len(got), len(payload))
	}

	// A wrong access hash resolves to nothing, not to the file.
	err := execMedia(t, ctx, b.cmds, func(ctx context.Context, c *tg.Client) error {
		_, gerr := c.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID:            bDoc.ID,
				AccessHash:    bDoc.AccessHash + 1,
				FileReference: bDoc.FileReference,
			},
			Offset: 0,
			Limit:  mediaPartSize,
		})
		return gerr
	})
	assertRPCError(t, err, "LOCATION_INVALID")
}

// TestMediaCrossReplicaUploadHandover proves an in-flight multipart upload is
// shared across replicas and remains isolated by account while assembly and
// the part sweeper overlap an active retry.
func TestMediaCrossReplicaUploadHandover(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	env := bootMediaReplicaEnv(t, ctx, "+15551312660", "+15551312661", "+15551312662")
	ownerA, ownerB := env.ownerA, env.ownerB
	stranger, recipient := env.others[0], env.others[1]
	payload := mediaPayload(mediaPartSize + 7)
	const clientFileID int64 = 0x5ED12660

	// A writes the first part and B writes the second for the same account and
	// file id. The later retry replaces A's first payload with the expected one.
	ownerFirstPart := bytes.Repeat([]byte{'a'}, mediaPartSize)
	if err := saveMediaPartResult(t, ctx, ownerA, clientFileID, 0, ownerFirstPart); err != nil {
		t.Fatalf("replica A first part: %v", err)
	}
	if err := saveMediaPartResult(t, ctx, ownerB, clientFileID, 1, payload[mediaPartSize:]); err != nil {
		t.Fatalf("replica B second part: %v", err)
	}
	if err := saveMediaPartResult(t, ctx, stranger, clientFileID, 0, bytes.Repeat([]byte{'z'}, mediaPartSize)); err != nil {
		t.Fatalf("unrelated account reusing file id: %v", err)
	}
	gotOwnerPart, ok, err := env.stores[0].UploadPart(ctx, ownerA.id, clientFileID, 0)
	if err != nil || !ok {
		t.Fatalf("read owner's first part after unrelated upload: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(gotOwnerPart, ownerFirstPart) {
		t.Fatal("unrelated account reusing the file id changed the owner's first part")
	}

	// Hold the owner's advisory lock on one connection and prove a save sent to
	// replica B waits for it before committing its part row.
	lockConn, err := pgx.Connect(ctx, env.dsn)
	if err != nil {
		t.Fatalf("connect advisory-lock holder: %v", err)
	}
	lockTx, err := lockConn.Begin(ctx)
	if err != nil {
		if closeErr := lockConn.Close(context.Background()); closeErr != nil {
			t.Errorf("close advisory-lock holder after begin failure: %v", closeErr)
		}
		t.Fatalf("begin advisory-lock holder: %v", err)
	}
	lockOpen := true
	defer func() {
		if lockOpen {
			if rollbackErr := lockTx.Rollback(context.Background()); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
				t.Errorf("release owner advisory lock: %v", rollbackErr)
			}
		}
		if closeErr := lockConn.Close(context.Background()); closeErr != nil {
			t.Errorf("close advisory-lock holder: %v", closeErr)
		}
	}()
	if _, err := lockTx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", ownerA.id); err != nil {
		t.Fatalf("hold owner advisory lock: %v", err)
	}
	lockSaveDone := make(chan error, 1)
	go func() {
		lockSaveDone <- saveMediaPartResult(t, ctx, ownerB, clientFileID+1, 0, []byte("lock probe"))
	}()
	select {
	case lockErr := <-lockSaveDone:
		if lockErr != nil {
			t.Fatalf("replica B lock probe save: %v", lockErr)
		}
		t.Fatal("replica B upload completed while the owner's advisory lock was held")
	default:
	}
	waitForAdvisoryBlock(t, ctx, lockTx)
	if err := lockTx.Commit(ctx); err != nil {
		t.Fatalf("release owner advisory lock: %v", err)
	}
	lockOpen = false
	select {
	case lockErr := <-lockSaveDone:
		if lockErr != nil {
			t.Fatalf("replica B lock probe after release: %v", lockErr)
		}
	case <-ctx.Done():
		t.Fatalf("replica B lock probe did not resume: %v", ctx.Err())
	}

	// Pause a replacement part after its database row commits but before its
	// bytes land. Assembly on A must fail closed during that gap; meanwhile B's
	// orphan sweeper must respect the TTL margin and leave both old-enough paths.
	gate := env.blobs.blockNextPartPut()
	defer gate.unblock()
	retryDone := make(chan error, 1)
	go func() {
		retryDone <- saveMediaPartResult(t, ctx, ownerB, clientFileID, 0, payload[:mediaPartSize])
	}()
	var activePartKey string
	select {
	case activePartKey = <-gate.started:
	case <-ctx.Done():
		t.Fatalf("replica B retry did not reach shared blob backend: %v", ctx.Err())
	}

	assemblyErr := execMedia(t, ctx, ownerA.cmds, func(ctx context.Context, c *tg.Client) error {
		_, sendErr := c.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(ownerA.id, recipient.id),
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: clientFileID, Parts: 2, Name: "handover.bin"},
				MimeType: "application/octet-stream",
			},
			Message:  "handover",
			RandomID: 91012660,
		})
		return sendErr
	})
	if assemblyErr == nil {
		t.Fatal("assembly during an active part retry succeeded without the committed part bytes")
	}

	partTTL := 6 * time.Hour
	oldEnoughForTTLOnly := time.Now().Add(-(partTTL + time.Hour))
	activeTemp := filepath.Join(env.local.RootDir(), filepath.FromSlash(activePartKey+blob.TempSuffix))
	if err := os.Chtimes(activeTemp, oldEnoughForTTLOnly, oldEnoughForTTLOnly); err != nil {
		t.Fatalf("age active part temporary: %v", err)
	}
	orphanKey, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("new orphan part key: %v", err)
	}
	if _, err := env.local.Put(ctx, orphanKey, bytes.NewReader([]byte("orphan"))); err != nil {
		t.Fatalf("write orphan part: %v", err)
	}
	orphanPath := filepath.Join(env.local.RootDir(), filepath.FromSlash(orphanKey))
	if err := os.Chtimes(orphanPath, oldEnoughForTTLOnly, oldEnoughForTTLOnly); err != nil {
		t.Fatalf("age orphan part: %v", err)
	}
	swept, err := env.stores[1].ReclaimOrphanedPartBytes(ctx, time.Now(), partTTL)
	if err != nil {
		t.Fatalf("replica B part sweep: %v", err)
	}
	if swept.Objects != 0 {
		t.Fatalf("part sweep reclaimed %+v inside the TTL margin, want nothing", swept)
	}
	for _, path := range []string{activeTemp, orphanPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("part sweep removed protected path %s: %v", path, err)
		}
	}

	gate.unblock()
	select {
	case retryErr := <-retryDone:
		if retryErr != nil {
			t.Fatalf("replica B replacement part: %v", retryErr)
		}
	case <-ctx.Done():
		t.Fatalf("replica B replacement part did not finish: %v", ctx.Err())
	}

	doc := sendUploadedDocument(t, ctx, ownerA, peerUser(ownerA.id, recipient.id), clientFileID, 2, "handover.bin", "handover", 91012661)
	if doc.Size != int64(len(payload)) {
		t.Fatalf("document size = %d, want %d", doc.Size, len(payload))
	}
	for _, replica := range []struct {
		mc  *mediaClient
		who string
	}{{ownerA, "A"}, {ownerB, "B"}} {
		got := downloadDocument(t, ctx, replica.mc, doc)
		if !bytes.Equal(got, payload) {
			t.Fatalf("replica %s downloaded %d bytes, want %d identical bytes", replica.who, len(got), len(payload))
		}
	}

	readErr := execMedia(t, ctx, stranger.cmds, func(ctx context.Context, c *tg.Client) error {
		_, getErr := c.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			},
			Offset: 0,
			Limit:  mediaPartSize,
		})
		return getErr
	})
	assertRPCError(t, readErr, "LOCATION_INVALID")
}

// TestMediaDownloadRequiresOwnMessage proves the capability is not the pair: C,
// holding the exact (id, access_hash) B holds, is refused because no live
// message of C's names the file.
func TestMediaDownloadRequiresOwnMessage(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clients := bootMediaEnv(t, ctx, "+15551311001", "+15551311002", "+15551311003")
	a, b, c := clients[0], clients[1], clients[2]

	payload := mediaPayload(mediaPartSize + 7)
	const clientFileID = int64(0x5ED10002)
	parts := uploadParts(t, ctx, a, clientFileID, payload)

	peerB := peerUser(a.id, b.id)
	doc := sendUploadedDocument(t, ctx, a, peerB, clientFileID, parts, "gate.bin", "here", 910002)

	// B, the recipient, can read it.
	if got := downloadDocument(t, ctx, b, doc); !bytes.Equal(got, payload) {
		t.Fatalf("B downloaded %d bytes, want %d identical bytes", len(got), len(payload))
	}

	// C was sent nothing, so the same pair is inert in its hands.
	err := execMedia(t, ctx, c.cmds, func(ctx context.Context, cl *tg.Client) error {
		_, gerr := cl.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID:            doc.ID,
				AccessHash:    doc.AccessHash,
				FileReference: doc.FileReference,
			},
			Offset: 0,
			Limit:  mediaPartSize,
		})
		return gerr
	})
	assertRPCError(t, err, "LOCATION_INVALID")
}

// TestMediaInChatFanOut proves one stored file entitles every member of the
// chat it was sent to: B and C both download the same document.
func TestMediaInChatFanOut(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	clients := bootMediaEnv(t, ctx, "+15551312001", "+15551312002", "+15551312003")
	a, b, c := clients[0], clients[1], clients[2]

	var chatID int64
	execChat(t, ctx, a.cmds, func(ctx context.Context, cl *tg.Client) error {
		inv, err := cl.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Media",
			Users: []tg.InputUserClass{
				inputUser(a.id, b.id),
				inputUser(a.id, c.id),
			},
		})
		if err != nil {
			return err
		}
		ups, ok := inv.Updates.(*tg.Updates)
		if !ok {
			return fmt.Errorf("createChat updates type = %T, want *tg.Updates", inv.Updates)
		}
		if len(ups.Chats) != 1 {
			return errors.New("createChat: no chat in response")
		}
		chat, ok := ups.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat type = %T, want *tg.Chat", ups.Chats[0])
		}
		chatID = chat.ID
		return nil
	})
	for _, m := range []struct {
		mc  *mediaClient
		who string
	}{{b, "B"}, {c, "C"}} {
		_, err := m.mc.coll.waitService(ctx, &tg.MessageActionChatCreate{})
		if err != nil {
			t.Fatalf("%s wait create service: %v", m.who, err)
		}
	}

	payload := mediaPayload(mediaPartSize + 7)
	const clientFileID = int64(0x5ED10003)
	parts := uploadParts(t, ctx, a, clientFileID, payload)
	doc := sendUploadedDocument(t, ctx, a,
		&tg.InputPeerChat{ChatID: chatID}, clientFileID, parts, "gate.bin", "here", 910003)

	// Both members receive the message live and download the same file.
	for _, m := range []struct {
		mc  *mediaClient
		who string
	}{{b, "B"}, {c, "C"}} {
		msg := recvOrCtx(t, ctx, m.mc.coll.newMsg, m.who+" chat media updateNewMessage")
		memberDoc := documentOf(t, msg)
		assertSameDocument(t, memberDoc, doc, m.who)
		if got := downloadDocument(t, ctx, m.mc, memberDoc); !bytes.Equal(got, payload) {
			t.Fatalf("%s downloaded %d bytes, want %d identical bytes", m.who, len(got), len(payload))
		}
	}
}
