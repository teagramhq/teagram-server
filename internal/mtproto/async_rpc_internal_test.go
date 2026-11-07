package mtproto

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/tg"
)

type asyncRPCFrameConn struct {
	frames [][]byte
	index  int
	eof    chan struct{}
	once   sync.Once
}

func (c *asyncRPCFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.index == len(c.frames) {
		<-c.eof
		return io.EOF
	}
	b.ResetTo(slices.Clone(c.frames[c.index]))
	c.index++
	return nil
}

func (*asyncRPCFrameConn) Send(context.Context, *bin.Buffer) error { return nil }
func (*asyncRPCFrameConn) Close() error                            { return nil }

func (c *asyncRPCFrameConn) end() {
	c.once.Do(func() { close(c.eof) })
}

type asyncRPCOrderHandler struct {
	started      chan struct{}
	cheapStarted chan struct{}
	release      chan struct{}
	mu           sync.Mutex
	mutations    []int64
}

func (h *asyncRPCOrderHandler) CanRunConcurrently(req *Request) bool {
	id, err := req.Buf.PeekID()
	return err == nil && id == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCOrderHandler) OnMessage(c *Conn, req *Request) error {
	id, err := req.Buf.PeekID()
	if err != nil {
		return err
	}
	switch id {
	case tg.MessagesGetSearchCountersRequestTypeID:
		close(h.started)
		<-h.release
	case tg.HelpGetConfigRequestTypeID:
		close(h.cheapStarted)
	case tg.AccountRegisterDeviceRequestTypeID:
		h.mu.Lock()
		h.mutations = append(h.mutations, req.MsgID)
		h.mu.Unlock()
	}
	return c.SendResult(req, &tg.BoolTrue{})
}

func TestConcurrentRPCDoesNotDelayCheapRequestOrReorderMutations(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := &asyncRPCOrderHandler{
		started:      make(chan struct{}),
		cheapStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	dispatcher := ConcurrentRPCs(UnpackInvokeWithAfterMsg(handler, nil), tg.MessagesGetSearchCountersRequestTypeID)
	server := New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	conn := &asyncRPCFrameConn{eof: make(chan struct{}), frames: [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.InvokeWithLayerRequest{Layer: 228,
			Query: &tg.InvokeWithoutUpdatesRequest{Query: &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}},
		}),
		shutdownClientFrame(t, key, 42, 2<<32, &tg.InvokeWithLayerRequest{Layer: 228,
			Query: &tg.InvokeWithoutUpdatesRequest{Query: &tg.HelpGetConfigRequest{}},
		}),
		shutdownClientFrame(t, key, 42, 3<<32, &tg.AccountRegisterDeviceRequest{}),
		shutdownClientFrame(t, key, 42, 4<<32, &tg.AccountRegisterDeviceRequest{}),
	}}
	defer conn.end()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()

	select {
	case <-handler.started:
	case <-time.After(time.Second):
		close(handler.release)
		conn.end()
		err := <-served
		t.Fatalf("getSearchCounters handler did not start; ServeConn = %v", err)
	}
	cheapWasDelayed := false
	select {
	case <-handler.cheapStarted:
	case <-time.After(100 * time.Millisecond):
		cheapWasDelayed = true
	}
	close(handler.release)
	conn.end()
	if cheapWasDelayed {
		t.Fatal("cheap request was held behind the concurrent counters request")
	}

	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after releasing counters request")
	}

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if want := []int64{3 << 32, 4 << 32}; !slices.Equal(handler.mutations, want) {
		t.Fatalf("state-changing request order = %v, want %v", handler.mutations, want)
	}
}

func TestConcurrentRPCWaitsForInvokeAfterMsgDependency(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := &asyncRPCOrderHandler{
		started:      make(chan struct{}),
		cheapStarted: make(chan struct{}),
		release:      make(chan struct{}),
	}
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	dispatcher := ConcurrentRPCs(UnpackInvokeWithAfterMsg(handler, nil), tg.MessagesGetSearchCountersRequestTypeID)
	server := New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	conn := &asyncRPCFrameConn{eof: make(chan struct{}), frames: [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 2<<32, &tg.InvokeAfterMsgRequest{
			MsgID: 1 << 32, Query: &tg.HelpGetConfigRequest{},
		}),
	}}
	defer conn.end()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()

	select {
	case <-handler.started:
	case <-time.After(time.Second):
		close(handler.release)
		conn.end()
		err := <-served
		t.Fatalf("getSearchCounters handler did not start; ServeConn = %v", err)
	}
	dependencyWasSkipped := false
	select {
	case <-handler.cheapStarted:
		dependencyWasSkipped = true
	case <-time.After(100 * time.Millisecond):
	}
	close(handler.release)
	if dependencyWasSkipped {
		t.Fatal("invokeAfterMsg dispatched before its concurrent dependency completed")
	}
	select {
	case <-handler.cheapStarted:
	case <-time.After(time.Second):
		t.Fatal("invokeAfterMsg did not dispatch after its dependency completed")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after invokeAfterMsg")
	}
}
