package mtproto

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/transport"
)

func TestPeerFrameReaderBackpressure(t *testing.T) {
	for _, tc := range []struct {
		name           string
		size, admitted int
	}{
		{"frames", 16, 8},
		{"bytes", 600 << 10, 2},
		{"exact byte threshold", 1 << 20, 1},
		{"oversized", 2 << 20, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := New(exchange.PrivateKey{}, 2, NewMemoryAuthKeyStore(), nil, nil)
			client, server := transport.Intermediate.Pipe()
			conn := newConn(server, srv.cipher, srv.msgID, srv.clock, srv.writeTimeout, srv.log)
			reader := &peerFrameReader{server: srv, transport: server, conn: conn}
			sent := make(chan int, 16)
			senderDone := make(chan struct{})
			go func() {
				defer close(senderDone)
				for i := 1; i <= 12; i++ {
					if err := client.Send(context.Background(), &bin.Buffer{Buf: bytes.Repeat([]byte{byte(i)}, tc.size)}); err != nil {
						return
					}
					sent <- i
				}
			}()
			t.Cleanup(func() {
				if err := server.Close(); err != nil {
					t.Error(err)
				}
				if err := client.Close(); err != nil {
					t.Error(err)
				}
				reader.join()
				<-senderDone
			})
			reader.prefetch()
			for i := 1; i <= tc.admitted; i++ {
				select {
				case got := <-sent:
					if got != i {
						t.Fatalf("sent %d, want %d", got, i)
					}
				case <-time.After(time.Second):
					t.Fatalf("reader stopped before frame %d", i)
				}
			}
			select {
			case got := <-sent:
				t.Fatalf("read frame %d beyond queue bound", got)
			case <-time.After(50 * time.Millisecond):
			}
			frame, err := reader.next(context.Background(), time.Time{}, false)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(frame.Buf, bytes.Repeat([]byte{1}, tc.size)) {
				t.Fatal("first dispatched frame out of order")
			}
			select {
			case got := <-sent:
				if got != tc.admitted+1 {
					t.Fatalf("resumed at %d", got)
				}
			case <-time.After(time.Second):
				t.Fatal("reader did not resume after dispatch")
			}
		})
	}
}
