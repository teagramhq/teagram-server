package mtproto

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
)

const (
	asyncRPCWorkerLimit = 2
	asyncRPCQueueLimit  = 16
)

type asyncRPCFrameConn struct {
	frames     [][]byte
	index      int
	eof        chan struct{}
	closed     chan struct{}
	once       sync.Once
	closeOnce  sync.Once
	mu         sync.Mutex
	sent       [][]byte
	sentNotify chan struct{}
	recvNotify chan int
}

func (c *asyncRPCFrameConn) Recv(_ context.Context, b *bin.Buffer) error {
	if c.index == len(c.frames) {
		select {
		case <-c.eof:
			return io.EOF
		case <-c.closed:
			return net.ErrClosed
		}
	}
	b.ResetTo(slices.Clone(c.frames[c.index]))
	c.index++
	if c.recvNotify != nil {
		select {
		case c.recvNotify <- c.index:
		default:
		}
	}
	return nil
}

func (c *asyncRPCFrameConn) Send(_ context.Context, b *bin.Buffer) error {
	c.mu.Lock()
	c.sent = append(c.sent, slices.Clone(b.Buf))
	c.mu.Unlock()
	if c.sentNotify != nil {
		select {
		case c.sentNotify <- struct{}{}:
		default:
		}
	}
	return nil
}

func (c *asyncRPCFrameConn) Close() error {
	if c.closed != nil {
		c.closeOnce.Do(func() { close(c.closed) })
	}
	return nil
}

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

type asyncRPCRevocationHandler struct {
	counterStarted chan struct{}
	logoutStarted  chan struct{}
	releaseCounter chan struct{}
}

func (*asyncRPCRevocationHandler) CanRunConcurrently(id uint32) bool {
	return id == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCRevocationHandler) OnMessage(c *Conn, req *Request) error {
	id, err := asyncRPCRequestBody(req).PeekID()
	if err != nil {
		return err
	}
	switch id {
	case tg.MessagesGetSearchCountersRequestTypeID:
		close(h.counterStarted)
		<-h.releaseCounter
	case tg.AuthLogOutRequestTypeID:
		close(h.logoutStarted)
	}
	return c.SendResult(req, &tg.BoolTrue{})
}

func (h *asyncRPCOrderHandler) CanRunConcurrently(id uint32) bool {
	return id == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCOrderHandler) OnMessage(c *Conn, req *Request) error {
	id, err := asyncRPCRequestBody(req).PeekID()
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
	failMsgID   int64
	failRelease chan struct{}
	failOnce    sync.Once
}

func (h *asyncRPCBatchHandler) releaseCounters() {
	h.releaseOnce.Do(func() { close(h.release) })
}

func (h *asyncRPCBatchHandler) failCounter() {
	h.failOnce.Do(func() { close(h.failRelease) })
}

func (h *asyncRPCBatchHandler) CanRunConcurrently(id uint32) bool {
	return id == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCBatchHandler) OnMessage(c *Conn, req *Request) error {
	id, err := asyncRPCRequestBody(req).PeekID()
	if err != nil {
		return err
	}
	switch id {
	case tg.MessagesGetSearchCountersRequestTypeID:
		h.counters <- req.MsgID
		if req.MsgID == h.failMsgID && h.failRelease != nil {
			<-h.failRelease
			return errors.New("injected counter failure")
		}
		<-h.release
	case tg.HelpGetConfigRequestTypeID:
		h.configs <- req.MsgID
	case tg.AccountRegisterDeviceRequestTypeID:
		h.mutations <- req.MsgID
	}
	return c.SendResult(req, &tg.BoolTrue{})
}

func asyncRPCBatchServer(t *testing.T, key crypto.AuthKey, handler Handler, frames ...[]byte) (*Server, *asyncRPCFrameConn) {
	t.Helper()
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	dispatcher := UnpackInvokeWithAfterMsg(handler, nil)
	server := New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	return server, &asyncRPCFrameConn{
		eof: make(chan struct{}), closed: make(chan struct{}), frames: frames,
		sentNotify: make(chan struct{}, 64), recvNotify: make(chan int, len(frames)+1),
	}
}

func asyncRPCBatchHandlerForTest() *asyncRPCBatchHandler {
	return &asyncRPCBatchHandler{
		counters:  make(chan int64, 32),
		configs:   make(chan int64, 8),
		mutations: make(chan int64, 8),
		release:   make(chan struct{}),
	}
}

type asyncRPCReply struct {
	key     [8]byte
	session int64
	body    []byte
}

func (c *asyncRPCFrameConn) repliesForKey(t *testing.T, key crypto.AuthKey) []asyncRPCReply {
	t.Helper()
	c.mu.Lock()
	frames := make([][]byte, len(c.sent))
	for i := range c.sent {
		frames[i] = slices.Clone(c.sent[i])
	}
	c.mu.Unlock()
	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	replies := make([]asyncRPCReply, 0, len(frames))
	for _, frame := range frames {
		message := &crypto.EncryptedMessage{}
		if err := message.DecodeWithoutCopy(&bin.Buffer{Buf: frame}); err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		decrypted, err := cipher.Decrypt(key, message)
		if err != nil {
			continue
		}
		replies = append(replies, asyncRPCReply{key: key.ID, session: decrypted.SessionID, body: slices.Clone(decrypted.Data())})
	}
	return replies
}

func asyncRPCResult(t *testing.T, replies []asyncRPCReply, msgID int64) (asyncRPCReply, []byte, bool) {
	t.Helper()
	for _, reply := range replies {
		var result proto.Result
		if err := result.Decode(&bin.Buffer{Buf: reply.body}); err != nil || result.RequestMessageID != msgID {
			continue
		}
		return reply, slices.Clone(result.Result), true
	}
	return asyncRPCReply{}, nil, false
}

func asyncRPCReceivedAt(req *Request) (time.Time, bool) {
	field := reflect.ValueOf(req).Elem().FieldByName("ReceivedAt")
	if !field.IsValid() || !field.CanInterface() {
		return time.Time{}, false
	}
	value, ok := reflect.TypeAssert[time.Time](field)
	return value, ok
}

func asyncRPCBodyField(req *Request) *bin.Buffer {
	field := reflect.ValueOf(req).Elem().FieldByName("Body")
	if !field.IsValid() || !field.CanInterface() {
		return nil
	}
	body, _ := reflect.TypeAssert[*bin.Buffer](field)
	return body
}

func asyncRPCRequestBody(req *Request) *bin.Buffer {
	if body := asyncRPCBodyField(req); body != nil {
		return body
	}
	return req.Buf
}

func asyncRPCGZIP(t *testing.T, message bin.Encoder) *proto.GZIP {
	t.Helper()
	var body bin.Buffer
	if err := message.Encode(&body); err != nil {
		t.Fatalf("encode gzip message: %v", err)
	}
	return &proto.GZIP{Data: body.Copy()}
}

type asyncRPCPaddedEncoder struct {
	message bin.Encoder
	padding int
}

func (e asyncRPCPaddedEncoder) Encode(b *bin.Buffer) error {
	if err := e.message.Encode(b); err != nil {
		return err
	}
	b.Buf = append(b.Buf, make([]byte, e.padding)...)
	return nil
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
	dispatcher := UnpackInvokeWithAfterMsg(handler, nil)
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

func TestConcurrentRPCWaitsBeforeAuthKeyRevocation(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := &asyncRPCRevocationHandler{
		counterStarted: make(chan struct{}),
		logoutStarted:  make(chan struct{}),
		releaseCounter: make(chan struct{}),
	}
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	dispatcher := ConcurrentRPCBarriers(handler, tg.AuthLogOutRequestTypeID)
	server := New(exchange.PrivateKey{}, 2, keys, dispatcher, nil)
	conn := &asyncRPCFrameConn{
		frames: [][]byte{
			shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
			shutdownClientFrame(t, key, 42, 2<<32, &tg.AuthLogOutRequest{}),
		},
		eof:        make(chan struct{}),
		recvNotify: make(chan int, 3),
	}
	defer conn.end()
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()

	select {
	case <-handler.counterStarted:
	case <-time.After(time.Second):
		close(handler.releaseCounter)
		t.Fatal("search counters handler did not start")
	}
	for received := 0; received < 2; {
		select {
		case index := <-conn.recvNotify:
			if index > received {
				received = index
			}
		case <-time.After(time.Second):
			close(handler.releaseCounter)
			t.Fatal("serve loop did not read auth.logOut while counters were active")
		}
	}
	select {
	case <-handler.logoutStarted:
		close(handler.releaseCounter)
		t.Fatal("auth.logOut ran before the earlier counters RPC completed")
	case <-time.After(50 * time.Millisecond):
	}
	close(handler.releaseCounter)
	select {
	case <-handler.logoutStarted:
	case <-time.After(time.Second):
		t.Fatal("auth.logOut did not run after the counters RPC completed")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not finish after auth.logOut")
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
	dispatcher := UnpackInvokeWithAfterMsg(handler, nil)
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

func TestConcurrentRPCClassifiesGZIPCountersAndIndependentRequests(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	frames := [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		shutdownClientFrame(t, key, 42, 2<<32, asyncRPCGZIP(t, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}})),
		shutdownClientFrame(t, key, 42, 3<<32, asyncRPCGZIP(t, &tg.HelpGetConfigRequest{})),
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
			t.Fatalf("only %d counters started before release, want the plain and gzip counters", len(started))
		}
	}
	select {
	case msgID := <-handler.configs:
		if msgID != 3<<32 {
			t.Fatalf("gzip help.getConfig message id = %d, want %d", msgID, 3<<32)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("gzip help.getConfig was held behind active counters")
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

func TestConcurrentRPCFallsBackToSerialForOversizedBodies(t *testing.T) {
	const bodyLimit = maxConcurrentRPCBodyBytes
	for _, test := range []struct {
		name string
		gzip bool
	}{
		{name: "packed frame"},
		{name: "gzip decoded body", gzip: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			padded := asyncRPCPaddedEncoder{
				message: &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
				padding: bodyLimit + 1,
			}
			var packed bin.Buffer
			if test.gzip {
				compressed := asyncRPCGZIP(t, padded)
				if len(compressed.Data) <= bodyLimit {
					t.Fatalf("gzip logical body is %d bytes, want over %d", len(compressed.Data), bodyLimit)
				}
				if err := compressed.Encode(&packed); err != nil {
					t.Fatalf("encode compressed request: %v", err)
				}
			} else if err := padded.Encode(&packed); err != nil {
				t.Fatalf("encode padded request: %v", err)
			}
			if test.gzip && packed.Len() >= bodyLimit {
				t.Fatalf("compressed request is %d bytes, want under %d", packed.Len(), bodyLimit)
			}
			if !test.gzip && packed.Len() <= bodyLimit {
				t.Fatalf("packed request is %d bytes, want over %d", packed.Len(), bodyLimit)
			}

			conn := &Conn{}
			conn.concurrentRPCs.entries = map[int64]chan struct{}{
				1 << 32: make(chan struct{}),
				2 << 32: make(chan struct{}),
			}
			conn.concurrentRPCs.active = maxConcurrentRPCsPerConn
			admission := &rpcAdmission{ctx: context.Background()}
			admission.refs.Store(1)
			req := &Request{
				MsgID:     3 << 32,
				Buf:       &bin.Buffer{Buf: packed.Buf},
				Ctx:       context.Background(),
				admission: admission,
			}
			originalBuf := req.Buf
			server := &Server{}
			handler := asyncRPCBatchHandlerForTest()

			defer func() {
				conn.concurrentRPCs.mu.Lock()
				queued := conn.concurrentRPCs.queue
				conn.concurrentRPCs.queue = nil
				for _, done := range conn.concurrentRPCs.entries {
					close(done)
				}
				conn.concurrentRPCs.entries = nil
				conn.concurrentRPCs.active = 0
				conn.concurrentRPCs.mu.Unlock()
				for _, work := range queued {
					if work.release != nil {
						work.release()
					}
				}
				admission.finish()
			}()

			scheduled, err := server.dispatchConcurrentRPC(conn, req, handler)
			if err != nil {
				t.Fatalf("dispatchConcurrentRPC: %v", err)
			}
			if scheduled {
				t.Fatal("oversized counters request was scheduled concurrently")
			}
			if req.Buf != originalBuf {
				t.Fatal("serial fallback replaced the packed request buffer")
			}
			if got := admission.refs.Load(); got != 1 {
				t.Fatalf("serial fallback retained an admission reference: refs=%d", got)
			}
			conn.concurrentRPCs.mu.Lock()
			queued := len(conn.concurrentRPCs.queue)
			entries := len(conn.concurrentRPCs.entries)
			conn.concurrentRPCs.mu.Unlock()
			if queued != 0 || entries != maxConcurrentRPCsPerConn {
				t.Fatalf("oversized request changed concurrent work: queued=%d entries=%d", queued, entries)
			}
		})
	}
}

func TestConcurrentRPCFailureReleasesQueuedContainerAdmissions(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	handler.failMsgID = 1 << 32
	handler.failRelease = make(chan struct{})
	frames := [][]byte{
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		asyncRPCContainerFrame(t, key, 42, 2<<32, 6<<32,
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
			&tg.HelpGetConfigRequest{},
		),
		shutdownClientFrame(t, key, 42, 7<<32, &tg.HelpGetConfigRequest{}),
	}
	server, conn := asyncRPCBatchServer(t, key, handler, frames...)
	sink := &serverErrorLogSink{}
	logUpdates := sink.watch()
	server.log = slog.New(sink)
	server.shutdown.drainTimeout = 3 * time.Second
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	serveDone := false
	defer func() {
		handler.failCounter()
		handler.releaseCounters()
		conn.end()
		stopShutdownTimers(server.shutdown)
		if !serveDone {
			select {
			case <-served:
			case <-time.After(2 * time.Second):
				t.Error("serve loop did not finish during test cleanup")
			}
		}
	}()

	started := make(map[int64]bool)
	for len(started) < 2 {
		select {
		case msgID := <-handler.counters:
			started[msgID] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d counters started, want one active request from each frame", len(started))
		}
	}
	containerScheduled := false
	for !containerScheduled {
		select {
		case msgID := <-handler.configs:
			if msgID == 7<<32 {
				containerScheduled = true
			}
		case <-time.After(time.Second):
			t.Fatal("serve loop did not finish scheduling the container counters")
		}
	}

	handler.failCounter()
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		t.Fatal("failed active counter did not close the connection")
	}
	handler.releaseCounters()
	conn.end()
	select {
	case <-served:
		serveDone = true
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after counter failure")
	}
	logDeadline := time.NewTimer(time.Second)
	defer logDeadline.Stop()
	for {
		loggedDiscardCount := false
		for _, record := range sink.snapshot() {
			if record.message == "concurrent RPC failed" && fmt.Sprint(record.attrs["discarded_queued_rpcs"]) == "2" {
				loggedDiscardCount = true
				break
			}
		}
		if loggedDiscardCount {
			break
		}
		select {
		case <-logUpdates:
		case <-logDeadline.C:
			t.Fatalf("counter failure logs did not report two discarded queued requests: %+v", sink.snapshot())
		}
	}
	server.shutdown.beginDrain()
	drained := make(chan struct{})
	go func() {
		server.shutdown.waitForRPCs()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown drain retained admissions for discarded container counters")
	}
}

type asyncRPCFixedClassifier struct {
	typeID    uint32
	barrierID uint32
}

func (h asyncRPCFixedClassifier) CanRunConcurrently(typeID uint32) bool {
	return typeID == h.typeID
}

func (h asyncRPCFixedClassifier) MustWaitForConcurrentRPC(typeID uint32) bool {
	return typeID == h.barrierID
}

func (asyncRPCFixedClassifier) OnMessage(*Conn, *Request) error { return nil }

func TestRPCTraceWrapPreservesConcurrentClassifier(t *testing.T) {
	const typeID = tg.MessagesGetSearchCountersRequestTypeID
	const barrierID = tg.AuthLogOutRequestTypeID
	tracer := NewRPCTracerWithExporter(NewMemoryRPCSpanExporter(1))
	defer func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close RPC tracer: %v", err)
		}
	}()

	wrapped := tracer.Wrap(asyncRPCFixedClassifier{typeID: typeID, barrierID: barrierID})
	classifier, ok := wrapped.(interface{ CanRunConcurrently(uint32) bool })
	if !ok || !classifier.CanRunConcurrently(typeID) {
		t.Fatal("RPCTracer.Wrap dropped the next handler's concurrent classifier")
	}
	barrier, ok := wrapped.(ConcurrentRPCBarrierClassifier)
	if !ok || !barrier.MustWaitForConcurrentRPC(barrierID) {
		t.Fatal("RPCTracer.Wrap dropped the next handler's barrier classifier")
	}

	unpacked := UnpackInvokeWithAfterMsg(asyncRPCFixedClassifier{typeID: typeID, barrierID: barrierID}, nil)
	classifier, ok = unpacked.(interface{ CanRunConcurrently(uint32) bool })
	if !ok || !classifier.CanRunConcurrently(typeID) {
		t.Fatal("UnpackInvokeWithAfterMsg dropped the next handler's concurrent classifier")
	}
	barrier, ok = unpacked.(ConcurrentRPCBarrierClassifier)
	if !ok || !barrier.MustWaitForConcurrentRPC(barrierID) {
		t.Fatal("UnpackInvokeWithAfterMsg dropped the next handler's barrier classifier")
	}
}

func TestRPCDependencyOutcomeDoesNotWaitForSocketWriteLock(t *testing.T) {
	conn := &Conn{sessionID: 42}
	conn.recoveryBinding.Store(&recoveryBinding{Session: 42})
	conn.rpcOutcomes[0] = rpcOutcome{msgID: 1 << 32, session: 42, success: true}
	conn.rpcOutcomeNext = 1
	conn.rpcOutcomeSize = 1
	conn.writeMu.Lock()
	result := make(chan struct {
		found   bool
		success bool
	}, 1)
	go func() {
		found, success := conn.RPCDependencyOutcome(42, 1<<32, 2<<32)
		result <- struct {
			found   bool
			success bool
		}{found: found, success: success}
	}()
	select {
	case got := <-result:
		conn.writeMu.Unlock()
		if !got.found || !got.success {
			t.Fatalf("dependency outcome = (%v, %v), want (true, true)", got.found, got.success)
		}
	case <-time.After(50 * time.Millisecond):
		conn.writeMu.Unlock()
		<-result
		t.Fatal("dependency outcome waited for the socket write lock")
	}
}

func TestConcurrentRPCContainerKeepsFrameReceiveTimestamp(t *testing.T) {
	type stamp struct {
		value time.Time
		found bool
	}
	seen := make(chan stamp, 1)
	handler := HandlerFunc(func(_ *Conn, req *Request) error {
		value, found := asyncRPCReceivedAt(req)
		seen <- stamp{value: value, found: found}
		return nil
	})
	key := shutdownTestAuthKey()
	server, conn := asyncRPCBatchServer(t, key, handler, asyncRPCContainerFrame(t, key, 42, 1<<32, 2<<32, &tg.HelpGetConfigRequest{}))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	defer conn.end()
	select {
	case got := <-seen:
		if !got.found || got.value.IsZero() {
			t.Fatalf("container request receive timestamp = (%v, %v), want a non-zero timestamp", got.value, got.found)
		}
	case <-time.After(time.Second):
		t.Fatal("container request did not reach its handler")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not finish after container request")
	}
}

func TestGZIPHandlerKeepsPackedBufferAndExposesDecodedBody(t *testing.T) {
	seen := make(chan gzipObserved, 1)
	handler := &gzipRequestHandler{seen: seen}
	key := shutdownTestAuthKey()
	server, conn := asyncRPCBatchServer(t, key, handler, shutdownClientFrame(t, key, 42, 1<<32,
		asyncRPCGZIP(t, &tg.HelpGetConfigRequest{})))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	defer conn.end()
	select {
	case got := <-seen:
		if got.packedID != proto.GZIPTypeID || got.bodyID != tg.HelpGetConfigRequestTypeID {
			t.Fatalf("packed/decompressed constructor IDs = %x/%x, want gzip/help.getConfig", got.packedID, got.bodyID)
		}
	case <-time.After(time.Second):
		t.Fatal("gzip request did not reach its handler")
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not finish after gzip request")
	}
}

type gzipObserved struct {
	packedID uint32
	bodyID   uint32
}

type gzipRequestHandler struct {
	seen chan<- gzipObserved
}

func (*gzipRequestHandler) CanRunConcurrently(typeID uint32) bool {
	return typeID == tg.HelpGetConfigRequestTypeID
}

func (h *gzipRequestHandler) OnMessage(c *Conn, req *Request) error {
	packedID, err := req.Buf.PeekID()
	if err != nil {
		return err
	}
	body := asyncRPCBodyField(req)
	if body == nil {
		h.seen <- gzipObserved{packedID: packedID}
		return nil
	}
	bodyID, err := body.PeekID()
	if err != nil {
		return err
	}
	h.seen <- gzipObserved{packedID: packedID, bodyID: bodyID}
	return c.SendResult(req, &tg.BoolTrue{})
}

type asyncRPCPeerDisconnectHandler struct {
	started  chan int64
	canceled chan int64
}

func (*asyncRPCPeerDisconnectHandler) CanRunConcurrently(typeID uint32) bool {
	return typeID == tg.MessagesGetSearchCountersRequestTypeID
}

func (h *asyncRPCPeerDisconnectHandler) OnMessage(_ *Conn, req *Request) error {
	h.started <- req.MsgID
	<-req.Ctx.Done()
	h.canceled <- req.MsgID
	return req.Ctx.Err()
}

func TestConcurrentRPCDoesNotRunQueuedCounterAfterPeerDisconnect(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := &asyncRPCPeerDisconnectHandler{
		started: make(chan int64, 8), canceled: make(chan int64, 8),
	}
	container := asyncRPCContainerFrame(t, key, 42, 2<<32, 4<<32,
		&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
		&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
	)
	server, conn := asyncRPCBatchServer(t, key, handler,
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
		container,
	)
	server.shutdown.drainTimeout = 2 * time.Second
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	started := make(map[int64]bool)
	for len(started) < asyncRPCWorkerLimit {
		select {
		case msgID := <-handler.started:
			started[msgID] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d counter handlers started before disconnect, want %d", len(started), asyncRPCWorkerLimit)
		}
	}
	for received := 0; received < 2; {
		select {
		case index := <-conn.recvNotify:
			if index > received {
				received = index
			}
		case <-time.After(time.Second):
			t.Fatal("serve loop did not read the frame containing queued container work")
		}
	}
	conn.end()
	for range asyncRPCWorkerLimit {
		select {
		case <-handler.canceled:
		case <-time.After(time.Second):
			t.Fatal("peer disconnect did not cancel active counter work")
		}
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not drain after peer disconnect")
	}
	server.shutdown.beginDrain()
	drained := make(chan struct{})
	go func() {
		server.shutdown.waitForRPCs()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown retained admission references after peer disconnect")
	}
	select {
	case msgID := <-handler.started:
		t.Fatalf("queued counter %d ran after peer disconnect", msgID)
	default:
	}
}

func TestConcurrentRPCNineteenthCounterGetsInternalAndLogsOverload(t *testing.T) {
	key := shutdownTestAuthKey()
	handler := asyncRPCBatchHandlerForTest()
	sink := &serverErrorLogSink{}
	keys := NewMemoryAuthKeyStore()
	if err := keys.Save(context.Background(), key); err != nil {
		t.Fatalf("save test auth key: %v", err)
	}
	server := New(exchange.PrivateKey{}, 2, keys, UnpackInvokeWithAfterMsg(handler, nil), slog.New(sink))
	frames := make([][]byte, asyncRPCWorkerLimit+asyncRPCQueueLimit+1)
	for i := range frames {
		frames[i] = shutdownClientFrame(t, key, 42, int64(i+1)<<32,
			&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}})
	}
	conn := &asyncRPCFrameConn{
		frames: frames, eof: make(chan struct{}), closed: make(chan struct{}),
		sentNotify: make(chan struct{}, 64), recvNotify: make(chan int, len(frames)+1),
	}
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	started := make([]int64, 0, asyncRPCWorkerLimit)
	for len(started) < asyncRPCWorkerLimit {
		select {
		case msgID := <-handler.counters:
			started = append(started, msgID)
		case <-time.After(time.Second):
			t.Fatal("the two available counter workers did not start")
		}
	}
	var overload *mt.RPCError
	deadline := time.After(time.Second)
	for {
		replies := conn.repliesForKey(t, key)
		if _, payload, found := asyncRPCResult(t, replies, int64(len(frames))<<32); found {
			overload = &mt.RPCError{}
			if err := overload.Decode(&bin.Buffer{Buf: payload}); err != nil {
				t.Fatalf("decode overflow RPC error: %v", err)
			}
			break
		}
		select {
		case <-conn.sentNotify:
		case <-deadline:
			t.Fatal("nineteenth counter did not receive an RPC error")
		}
	}
	if overload.ErrorCode != 500 || overload.ErrorMessage != "INTERNAL" {
		t.Fatalf("nineteenth counter error = %d %q, want 500 INTERNAL", overload.ErrorCode, overload.ErrorMessage)
	}
	handler.releaseCounters()
	seen := make(map[int64]bool, asyncRPCWorkerLimit+asyncRPCQueueLimit)
	for _, msgID := range started {
		seen[msgID] = true
	}
	for len(seen) < asyncRPCWorkerLimit+asyncRPCQueueLimit {
		select {
		case msgID := <-handler.counters:
			seen[msgID] = true
		case <-time.After(time.Second):
			t.Fatalf("only %d of 18 accepted counters reached the handler", len(seen))
		}
	}
	conn.end()
	select {
	case err := <-served:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("ServeConn = %v, want EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve loop did not finish after accepted counters completed")
	}
	logged := false
	for _, record := range sink.snapshot() {
		if record.message == "concurrent RPC queue full" {
			logged = true
			if record.attrs["method"] != "messages.getSearchCounters" || record.attrs["auth_key_id"] == nil {
				t.Fatalf("overload log attributes = %v, want method and auth key ID", record.attrs)
			}
		}
	}
	if !logged {
		t.Fatal("counter overload was not logged")
	}
}

func TestConcurrentRPCWaitsForSessionAndKeyRebinding(t *testing.T) {
	for _, test := range []struct {
		name string
		key  bool
	}{
		{name: "session"},
		{name: "auth key", key: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			key1 := shutdownTestAuthKey()
			var rawKey crypto.Key
			for i := range rawKey {
				rawKey[i] = byte(i + 1)
			}
			key2 := rawKey.WithID()
			secondFrameKey, secondSession := key1, int64(43)
			if test.key {
				secondFrameKey, secondSession = key2, 42
			}
			handler := asyncRPCBatchHandlerForTest()
			keys := NewMemoryAuthKeyStore()
			if err := keys.Save(context.Background(), key1); err != nil {
				t.Fatalf("save first auth key: %v", err)
			}
			if test.key {
				if err := keys.Save(context.Background(), key2); err != nil {
					t.Fatalf("save second auth key: %v", err)
				}
			}
			server := New(exchange.PrivateKey{}, 2, keys, UnpackInvokeWithAfterMsg(handler, nil), nil)
			conn := &asyncRPCFrameConn{
				frames: [][]byte{
					shutdownClientFrame(t, key1, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}),
					shutdownClientFrame(t, secondFrameKey, secondSession, 2<<32, &tg.HelpGetConfigRequest{}),
				},
				eof: make(chan struct{}), closed: make(chan struct{}),
				sentNotify: make(chan struct{}, 64), recvNotify: make(chan int, 3),
			}
			served := make(chan error, 1)
			go func() { served <- server.ServeConn(context.Background(), conn) }()
			select {
			case <-handler.counters:
			case <-time.After(time.Second):
				t.Fatal("search counters handler did not start")
			}
			for received := 0; received < 2; {
				select {
				case index := <-conn.recvNotify:
					if index > received {
						received = index
					}
				case <-time.After(time.Second):
					t.Fatal("serve loop did not read the rebinding frame while counters were active")
				}
			}
			select {
			case msgID := <-handler.configs:
				t.Fatalf("request %d crossed an active counters call before the binding barrier", msgID)
			case <-time.After(50 * time.Millisecond):
			}
			handler.releaseCounters()
			select {
			case msgID := <-handler.configs:
				if msgID != 2<<32 {
					t.Fatalf("post-barrier help.getConfig id = %d, want %d", msgID, 2<<32)
				}
			case <-time.After(time.Second):
				t.Fatal("request after the binding barrier did not run")
			}
			conn.end()
			select {
			case err := <-served:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("ServeConn = %v, want EOF", err)
				}
			case <-time.After(time.Second):
				t.Fatal("serve loop did not finish after rebinding")
			}
			replies := conn.repliesForKey(t, key1)
			counterReply, payload, found := asyncRPCResult(t, replies, 1<<32)
			if !found {
				t.Fatal("counter result was not encrypted under the original auth key")
			}
			if counterReply.session != 42 {
				t.Fatalf("counter result session = %d, want original session 42", counterReply.session)
			}
			if test.key && counterReply.key != key1.ID {
				t.Fatalf("counter result key = %x, want original key %x", counterReply.key, key1.ID)
			}
			var result tg.BoolTrue
			if err := result.Decode(&bin.Buffer{Buf: payload}); err != nil {
				t.Fatalf("decode counter result: %v", err)
			}
		})
	}
}

func TestConcurrentRPCDisconnectLikeHandlerErrorClosesConnection(t *testing.T) {
	handler := HandlerFunc(func(_ *Conn, _ *Request) error { return io.EOF })
	classifier := asyncRPCDisconnectClassifier{Handler: handler}
	key := shutdownTestAuthKey()
	server, conn := asyncRPCBatchServer(t, key, classifier,
		shutdownClientFrame(t, key, 42, 1<<32, &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}))
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(context.Background(), conn) }()
	select {
	case <-conn.closed:
	case <-time.After(time.Second):
		conn.end()
		<-served
		t.Fatal("disconnect-shaped handler error left the connection open")
	}
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("ServeConn succeeded after the handler returned io.EOF")
		}
	case <-time.After(time.Second):
		t.Fatal("serve loop did not exit after the connection closed")
	}
}

type asyncRPCDisconnectClassifier struct{ Handler }

func (asyncRPCDisconnectClassifier) CanRunConcurrently(typeID uint32) bool {
	return typeID == tg.MessagesGetSearchCountersRequestTypeID
}
