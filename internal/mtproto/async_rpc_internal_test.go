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
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/proto"
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

type asyncRPCBatchHandler struct {
	counters    chan int64
	configs     chan int64
	mutations   chan int64
	release     chan struct{}
	releaseOnce sync.Once
}

func (h *asyncRPCBatchHandler) releaseCounters() {
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *asyncRPCBatchHandler) CanRunConcurrently(req *Request) bool {
	id, err := req.Buf.PeekID()
	return err == nil && id == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCBatchHandler) OnMessage(c *Conn, req *Request) error {
	id, err := req.Buf.PeekID()
	if err != nil {
		return err
	}
	switch id {
	case tg.MessagesGetSearchCountersRequestTypeID:
		h.counters <- req.MsgID
		<-h.release
	case tg.HelpGetConfigRequestTypeID:
		h.configs <- req.MsgID
	case tg.AccountRegisterDeviceRequestTypeID:
		h.mutations <- req.MsgID
	}
	return c.SendResult(req, &tg.BoolTrue{})
}

func asyncRPCBatchServer(t *testing.T, key crypto.AuthKey, handler *asyncRPCBatchHandler, frames ...[]byte) (*Server, *asyncRPCFrameConn) {
	t.Helper()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	dispatcher := ConcurrentRPCs(UnpackInvokeWithAfterMsg(handler, nil), tg.MessagesGetSearchCountersRequestTypeID)
	server := New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	return server, &asyncRPCFrameConn{eof: make(chan struct{}), frames: frames}
}

func asyncRPCBatchHandlerForTest() *asyncRPCBatchHandler {
	return &asyncRPCBatchHandler{
		counters:  make(chan int64, 8),
		configs:   make(chan int64, 8),
		mutations: make(chan int64, 8),
		release:   make(chan struct{}),
	}
}

func asyncRPCContainerFrame(t *testing.T, key crypto.AuthKey, sessionID, firstID, containerID int64, messages ...bin.Encoder) []byte {
	t.Helper()
	container := &proto.MessageContainer{Messages: make([]proto.Message, 0, len(messages))}
	for i, message := range messages {
		var body bin.Buffer
		if err := message.Encode(&body); err != nil {
			t.Fatalf("encode container message %d: %v", i, err)
		}
		container.Messages = append(container.Messages, proto.Message{
			ID:    firstID + (int64(i) << 32),
			SeqNo: i*2 + 1,
			Bytes: body.Len(),
			Body:  body.Copy(),
		})
	}
	return shutdownClientFrame(t, key, sessionID, containerID, container)
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

func TestConcurrentRPCSchedulesTwoCountersBeforeFollowingRequests(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	frames := [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 2<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 3<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 4<<32, &tg.HelpGetConfigRequest{}),
	}
	server, conn := asyncRPCBatchServer(t, key, handler, frames...)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	defer func() {
		handler.releaseCounters()
		conn.end()
	}()

	started := make(map[int64]bool)
	for len(started) < 2 {
		select {
		case msgID := <-handler.counters:
			started[msgID] = true
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("only %d counter requests started before release, want both", len(started))
		}
	}
	select {
	case msgID := <-handler.counters:
		t.Fatalf("counter %d started before a worker slot was available", msgID)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case msgID := <-handler.configs:
		if msgID != 4<<32 {
			t.Fatalf("help.getConfig message id = %d, want %d", msgID, 4<<32)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("help.getConfig was held behind concurrent counters")
	}

	handler.releaseCounters()
	select {
	case msgID := <-handler.counters:
		if msgID != 3<<32 {
			t.Fatalf("queued counter message id = %d, want %d", msgID, 3<<32)
		}
	case <-time.After(time.Second):
		t.Fatal("queued counter did not start after a worker slot became available")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after releasing counters")
	}
}

func TestConcurrentRPCContainerDispatchesEligibleEntriesDuringCounters(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	frames := [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		asyncRPCContainerFrame(t, key, 42, 2<<32, 6<<32,
			&tg.HelpGetConfigRequest{},
			&tg.AccountRegisterDeviceRequest{},
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
		),
	}
	server, conn := asyncRPCBatchServer(t, key, handler, frames...)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	defer func() {
		handler.releaseCounters()
		conn.end()
	}()

	started := make(map[int64]bool)
	for len(started) < 2 {
		select {
		case msgID := <-handler.counters:
			started[msgID] = true
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("only %d counter requests started across frames, want both", len(started))
		}
	}
	select {
	case msgID := <-handler.configs:
		if msgID != 2<<32 {
			t.Fatalf("container help.getConfig message id = %d, want %d", msgID, 2<<32)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("container help.getConfig was held behind an earlier counter")
	}
	select {
	case msgID := <-handler.mutations:
		if msgID != 3<<32 {
			t.Fatalf("container mutation message id = %d, want %d", msgID, 3<<32)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("container mutation was held behind an earlier counter")
	}

	handler.releaseCounters()
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after releasing counters")
	}
}

func TestConcurrentRPCContainerSchedulesCountersAndPreservesDependenciesAndMutationOrder(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	frames := [][]byte{
		asyncRPCContainerFrame(t, key, 42, 1<<32, 7<<32,
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
			&tg.AccountRegisterDeviceRequest{},
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
			&tg.AccountRegisterDeviceRequest{},
			&tg.InvokeAfterMsgRequest{MsgID: 3 << 32, Query: &tg.HelpGetConfigRequest{}},
			&tg.AccountRegisterDeviceRequest{},
		),
	}
	server, conn := asyncRPCBatchServer(t, key, handler, frames...)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	defer func() {
		handler.releaseCounters()
		conn.end()
	}()

	started := make(map[int64]bool)
	for len(started) < 2 {
		select {
		case msgID := <-handler.counters:
			started[msgID] = true
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("only %d container counters started before release, want both", len(started))
		}
	}
	for _, want := range []int64{2 << 32, 4 << 32} {
		select {
		case got := <-handler.mutations:
			if got != want {
				t.Fatalf("container mutation order got %d, want %d", got, want)
			}
		case <-time.After(250 * time.Millisecond):
			t.Fatalf("container mutation %d did not dispatch before counter release", want)
		}
	}
	select {
	case msgID := <-handler.configs:
		t.Fatalf("invokeAfterMsg %d ran before its counter dependency completed", msgID)
	case <-time.After(100 * time.Millisecond):
	}

	handler.releaseCounters()
	select {
	case msgID := <-handler.configs:
		if msgID != 5<<32 {
			t.Fatalf("dependent help.getConfig message id = %d, want %d", msgID, 5<<32)
		}
	case <-time.After(time.Second):
		t.Fatal("invokeAfterMsg did not run after its counter dependency completed")
	}
	select {
	case msgID := <-handler.mutations:
		if msgID != 6<<32 {
			t.Fatalf("final mutation message id = %d, want %d", msgID, 6<<32)
		}
	case <-time.After(time.Second):
		t.Fatal("mutation after invokeAfterMsg did not dispatch")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after releasing counters")
	}
}
