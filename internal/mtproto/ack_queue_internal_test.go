package mtproto

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	gotdmtproto "github.com/gotd/td/mtproto"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/transport"
)

// A real gotd client must not retry calls queued behind a seven-second RPC.
func TestQueuedRPCsAreAcknowledgedBeforeClientResends(t *testing.T) {
	t.Parallel()
	key := ackTestKey()
	store := NewMemoryAuthKeyStore()
	if err := store.Save(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var ids []int64 // Only the serial handler writes; read after serving ends.
	srv := New(exchange.PrivateKey{}, 2, store, HandlerFunc(func(c *Conn, req *Request) error {
		ids = append(ids, req.MsgID)
		if len(ids) == 1 {
			close(started)
			time.Sleep(7 * time.Second)
		}
		return c.SendResult(req, &tg.NearestDC{Country: "NL", ThisDC: 2, NearestDC: 2})
	}), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	served := make(chan error, 1)
	var counted *queueAckCountingConn
	client := gotdmtproto.New(func(context.Context) (transport.Conn, error) {
		clientConn, serverConn := transport.Intermediate.Pipe()
		counted = &queueAckCountingConn{Conn: serverConn, key: key, seen: make(map[int64]int)}
		go func() { served <- srv.serveConnWithContexts(ctx, ctx, counted, netip.Addr{}, nil, nil) }()
		return clientConn, nil
	}, gotdmtproto.Options{DC: 2, Key: key})
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var invokeErr error
	err := client.Run(runCtx, func(ctx context.Context) error {
		defer stop()
		results := make(chan error, 4)
		invoke := func() {
			var result tg.NearestDC
			err := client.Invoke(ctx, &tg.HelpGetNearestDCRequest{}, &result)
			if err == nil && result.Country != "NL" {
				err = errors.New("unexpected nearest DC result")
			}
			results <- err
		}
		go invoke()
		select {
		case <-started:
		case <-ctx.Done():
			return ctx.Err()
		}
		for range 3 {
			go invoke()
		}
		var firstErr error
		for range 4 {
			if err := <-results; err != nil && firstErr == nil {
				firstErr = err
			}
		}
		invokeErr = firstErr
		return firstErr
	})
	if invokeErr != nil {
		t.Fatal(invokeErr)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	<-served
	if len(ids) != 4 {
		t.Fatalf("handled %d calls, want 4", len(ids))
	}
	counted.mu.Lock()
	defer counted.mu.Unlock()
	for _, id := range ids {
		if n := counted.seen[id]; n != 1 {
			t.Fatalf("request %d received %d times, want once", id, n)
		}
	}
}

type queueAckCountingConn struct {
	transport.Conn

	key  crypto.AuthKey
	mu   sync.Mutex
	seen map[int64]int
}

func (c *queueAckCountingConn) Recv(ctx context.Context, b *bin.Buffer) error {
	if err := c.Conn.Recv(ctx, b); err != nil {
		return err
	}
	msg, err := crypto.NewServerCipher(crypto.DefaultRand()).DecryptFromBuffer(c.key, &bin.Buffer{Buf: slices.Clone(b.Buf)})
	if err != nil {
		return err
	}
	ids := []int64{msg.MessageID}
	var container proto.MessageContainer
	if err := container.Decode(&bin.Buffer{Buf: msg.Data()}); err == nil {
		ids = nil
		for _, m := range container.Messages {
			ids = append(ids, m.ID)
		}
	}
	c.mu.Lock()
	for _, id := range ids {
		c.seen[id]++
	}
	c.mu.Unlock()
	return nil
}
