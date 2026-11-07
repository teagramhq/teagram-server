//go:build performance

package perf_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const dispatchWaitProbeTimeout = 20 * time.Second

type receiveToDispatchObserver struct {
	next mtproto.Handler

	mu              sync.Mutex
	enabled         bool
	maxWait         time.Duration
	samples         int
	missingStamp    bool
	countersStarted chan struct{}
}

func (h *receiveToDispatchObserver) OnMessage(c *mtproto.Conn, req *mtproto.Request) error {
	id := performanceRPCTypeID(req)
	h.mu.Lock()
	if h.enabled {
		if req.ReceivedAt.IsZero() {
			h.missingStamp = true
		} else {
			h.samples++
			h.maxWait = max(h.maxWait, time.Since(req.ReceivedAt))
		}
		if id == tg.MessagesGetSearchCountersRequestTypeID && h.countersStarted != nil {
			close(h.countersStarted)
			h.countersStarted = nil
		}
	}
	h.mu.Unlock()
	return h.next.OnMessage(c, req)
}

func performanceRPCTypeID(req *mtproto.Request) uint32 {
	if req == nil || req.Buf == nil {
		return 0
	}
	b := &bin.Buffer{Buf: req.Buf.Copy()}
	id, err := b.PeekID()
	if err != nil {
		return 0
	}
	for id == tg.InvokeWithLayerRequestTypeID || id == tg.InvokeWithoutUpdatesRequestTypeID {
		query := &rpcConstructorID{}
		var wrapper interface{ Decode(*bin.Buffer) error }
		switch id {
		case tg.InvokeWithLayerRequestTypeID:
			wrapper = &tg.InvokeWithLayerRequest{Query: query}
		case tg.InvokeWithoutUpdatesRequestTypeID:
			wrapper = &tg.InvokeWithoutUpdatesRequest{Query: query}
		}
		if err := wrapper.Decode(b); err != nil {
			return 0
		}
		id = query.id
	}
	return id
}

func (h *receiveToDispatchObserver) CanRunConcurrently(req *mtproto.Request) bool {
	classifier, ok := h.next.(mtproto.ConcurrentRPCClassifier)
	return ok && classifier.CanRunConcurrently(req)
}

func (h *receiveToDispatchObserver) start() (<-chan struct{}, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.enabled {
		return nil, errors.New("receive-to-dispatch window is already active")
	}
	h.enabled = true
	h.maxWait = 0
	h.samples = 0
	h.missingStamp = false
	h.countersStarted = make(chan struct{})
	return h.countersStarted, nil
}

func (h *receiveToDispatchObserver) finish() (time.Duration, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = false
	h.countersStarted = nil
	return h.maxWait, h.samples, h.missingStamp
}

func installReceiveToDispatchObserver(s *server) (*receiveToDispatchObserver, error) {
	if observer, ok := s.rpcTimer.next.(*receiveToDispatchObserver); ok {
		return observer, nil
	}
	observer := &receiveToDispatchObserver{next: s.rpcTimer.next}
	s.rpcTimer.next = observer
	return observer, nil
}

// The performance RPC timer wraps the app handler, so it forwards the
// concurrent-RPC opt-in through its own handler boundary.
func (h *rpcTimingHandler) CanRunConcurrently(req *mtproto.Request) bool {
	classifier, ok := h.next.(mtproto.ConcurrentRPCClassifier)
	return ok && classifier.CanRunConcurrently(req)
}

func searchCountersDispatchWait(ctx context.Context, e *env) (call, string, error) {
	observer, err := installReceiveToDispatchObserver(e.srv)
	if err != nil {
		return nil, "", err
	}
	peer := userPeer(e.f.Owner, e.f.Partner)
	request := &tg.MessagesGetSearchCountersRequest{
		Peer: peer, Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterPhotos{}},
	}
	return func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, dispatchWaitProbeTimeout)
		counterStarted, err := observer.start()
		if err != nil {
			cancel()
			return err
		}
		queryStarted := e.srv.tracer.watchQueryStart("CountAllFilteredMessages")
		counterDone := make(chan error, 1)
		go func() {
			_, err := e.owner.api.MessagesGetSearchCounters(probeCtx, request)
			counterDone <- err
		}()
		counterFinished := false
		observerFinished := false
		defer func() {
			cancel()
			if !counterFinished {
				<-counterDone
			}
			if !observerFinished {
				wait, samples, missing := observer.finish()
				e.maxReceiveToDispatchWait = wait
				e.receiveToDispatchSamples = samples
				e.receiveToDispatchMissingStamp = missing
			}
		}()

		if err := waitDispatchProbe(probeCtx, counterStarted, "search counters dispatch"); err != nil {
			return err
		}
		if err := waitDispatchProbe(probeCtx, queryStarted, "search counters query"); err != nil {
			return err
		}
		started := time.Now()
		_, err = e.owner.api.HelpGetConfig(probeCtx)
		e.elapsed = time.Since(started)
		if err != nil {
			return fmt.Errorf("help.getConfig during search counters: %w", err)
		}
		select {
		case err := <-counterDone:
			counterFinished = true
			if err != nil {
				return fmt.Errorf("messages.getSearchCounters: %w", err)
			}
		case <-probeCtx.Done():
			return fmt.Errorf("messages.getSearchCounters did not finish: %w", probeCtx.Err())
		}
		wait, samples, missing := observer.finish()
		observerFinished = true
		e.maxReceiveToDispatchWait = wait
		e.receiveToDispatchSamples = samples
		e.receiveToDispatchMissingStamp = missing
		if missing || samples < 2 {
			return fmt.Errorf("receive timestamps: missing=%v, samples=%d; want timestamps for both RPCs", missing, samples)
		}
		return nil
	}, "", nil
}

func waitDispatchProbe(ctx context.Context, ready <-chan struct{}, what string) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for %s: %w", what, ctx.Err())
	}
}

func TestReceiveToDispatchObserverCapturesWaitAndForwardsClassifier(t *testing.T) {
	calls := 0
	next := mtproto.ConcurrentRPCs(mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		calls++
		return nil
	}), tg.MessagesGetSearchCountersRequestTypeID)
	observer := &receiveToDispatchObserver{next: next}
	started, err := observer.start()
	if err != nil {
		t.Fatal(err)
	}
	req := perfRPCRequest(t, &tg.InvokeWithLayerRequest{Layer: 228,
		Query: &tg.InvokeWithoutUpdatesRequest{Query: &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}},
	})
	req.ReceivedAt = time.Now().Add(-10 * time.Millisecond)
	if err := observer.OnMessage(nil, req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	select {
	case <-started:
	default:
		t.Fatal("search counters dispatch signal was not sent")
	}
	wait, samples, missing := observer.finish()
	if missing || samples != 1 || wait < 10*time.Millisecond {
		t.Fatalf("receive-to-dispatch = %s, %d samples, missing=%v; want at least 10 ms, one sample, timestamp present", wait, samples, missing)
	}
	if calls != 1 {
		t.Fatalf("next handler calls = %d, want 1", calls)
	}
	if !(&rpcTimingHandler{next: observer}).CanRunConcurrently(req) {
		t.Fatal("RPC timer did not forward the concurrent-RPC classifier")
	}
}
