package api

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestUpdaterDeliveryUsesConfiguredDCIDForDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	sender, err := s.CreateUser(ctx, "+15551234567")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551234568")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	file, err := s.AllocateFile(ctx, sender.ID, 7, "text/plain", "hello.txt", 1<<20)
	if err != nil {
		t.Fatalf("allocate document: %v", err)
	}
	if err := s.MarkFileStored(ctx, file.ID); err != nil {
		t.Fatalf("mark document stored: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, sender.ID, recipient.ID, "document", 1, file.ID, 0); err != nil {
		t.Fatalf("send document message: %v", err)
	}

	const configuredDCID = 7
	key := retryTestKey(41)
	transport := &recordingNotifyTransport{}
	conn := mtproto.NewTestConn(transport, key)
	conn.SetOwner(recipient.ID)
	registry := mtproto.NewSessionRegistry()
	if !registry.Add(recipient.ID, conn) {
		t.Fatal("register recipient connection")
	}
	t.Cleanup(func() { registry.Remove(recipient.ID, conn) })

	updater := NewUpdaterWithDialogFilterSync(s, configuredDCID, registry, slog.New(slog.DiscardHandler), pgtest.PeerDeriver(), NewDialogFilterSync())
	updater.Deliver(ctx, recipient.ID)
	frames := transport.framesFrom(0)
	if len(frames) != 1 {
		t.Fatalf("push frames = %d, want 1", len(frames))
	}
	decoded := decodeServerFrames(t, key, frames)
	if len(decoded) != 1 || decoded[0].push == nil {
		t.Fatalf("decoded frames = %+v, want one updates push", decoded)
	}
	if len(decoded[0].push.Updates) != 1 {
		t.Fatalf("push updates = %d, want 1", len(decoded[0].push.Updates))
	}
	update, ok := decoded[0].push.Updates[0].(*tg.UpdateNewMessage)
	if !ok {
		t.Fatalf("push update = %T, want *tg.UpdateNewMessage", decoded[0].push.Updates[0])
	}
	message, ok := update.Message.(*tg.Message)
	if !ok {
		t.Fatalf("push message = %T, want *tg.Message", update.Message)
	}
	media, ok := message.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("push media = %T, want *tg.MessageMediaDocument", message.Media)
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		t.Fatalf("push document = %T, want *tg.Document", media.Document)
	}
	if document.DCID != configuredDCID {
		t.Fatalf("push document dc id = %d, want configured dc id %d", document.DCID, configuredDCID)
	}
	if document.ID != file.ID || document.AccessHash != file.AccessHash || len(document.FileReference) != 8 {
		t.Fatalf("push document location = id %d hash %d reference %x, want id %d hash %d and a file reference", document.ID, document.AccessHash, document.FileReference, file.ID, file.AccessHash)
	}
}
