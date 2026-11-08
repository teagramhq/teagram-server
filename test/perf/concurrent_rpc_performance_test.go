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

	mu            sync.Mutex
	enabled       bool
	maxWait       time.Duration
	samples       int
	missingStamp  bool
	firstDispatch chan struct{}
}

func (h *receiveToDispatchObserver) OnMessage(c *mtproto.Conn, req *mtproto.Request) error {
	h.mu.Lock()
	if h.enabled {
		if req == nil || req.ReceivedAt.IsZero() {
			h.missingStamp = true
		} else {
			h.samples++
			h.maxWait = max(h.maxWait, time.Since(req.ReceivedAt))
		}
		if h.firstDispatch != nil {
			close(h.firstDispatch)
			h.firstDispatch = nil
		}
	}
	h.mu.Unlock()
	return h.next.OnMessage(c, req)
}

func (h *receiveToDispatchObserver) CanRunConcurrently(typeID uint32) bool {
	classifier, ok := h.next.(mtproto.ConcurrentRPCClassifier)
	return ok && classifier.CanRunConcurrently(typeID)
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
	h.firstDispatch = make(chan struct{})
	return h.firstDispatch, nil
}

func (h *receiveToDispatchObserver) finish() (time.Duration, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.enabled = false
	h.firstDispatch = nil
	return h.maxWait, h.samples, h.missingStamp
}

func installReceiveToDispatchObserver(s *server) *receiveToDispatchObserver {
	if observer, ok := s.rpcTimer.next.(*receiveToDispatchObserver); ok {
		return observer
	}
	observer := &receiveToDispatchObserver{next: s.rpcTimer.next}
	s.rpcTimer.next = observer
	return observer
}

func searchCountersDispatchWait(ctx context.Context, e *env) (call, string, error) {
	observer := installReceiveToDispatchObserver(e.srv)
	peer := userPeer(e.f.Owner, e.f.Partner)
	request := &tg.MessagesGetSearchCountersRequest{
		Peer: peer, Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterPhotos{}},
	}
	return func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, dispatchWaitProbeTimeout)
		counterDispatched, err := observer.start()
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
		finishObserver := func() (time.Duration, int, bool) {
			wait, samples, missing := observer.finish()
			e.maxReceiveToDispatchWait = wait
			e.receiveToDispatchSamples = samples
			observerFinished = true
			return wait, samples, missing
		}
		defer func() {
			cancel()
			if !counterFinished {
				<-counterDone
			}
			if !observerFinished {
				finishObserver()
			}
		}()

		if err := waitDispatchProbe(probeCtx, counterDispatched, "search counters dispatch"); err != nil {
			return err
		}
		if err := waitDispatchProbe(probeCtx, queryStarted, "search counters query"); err != nil {
			return err
		}
		started := time.Now()
		_, err = e.owner.api.HelpGetConfig(probeCtx)
		e.elapsed = time.Since(started)
		e.maxConcurrentRequestLatency = e.elapsed
		e.concurrentRequestSamples = 1
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
		_, samples, missing := finishObserver()
		if missing || samples < 2 {
			return fmt.Errorf("receive timestamps: missing=%v, samples=%d; want timestamps for both RPCs", missing, samples)
		}
		return nil
	}, "", nil
}

func TestReceiveToDispatchObserverCapturesBothRPCsAndForwardsClassifier(t *testing.T) {
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
	for i, rpc := range []bin.Encoder{
		&tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}},
		&tg.HelpGetConfigRequest{},
	} {
		req := perfRPCRequest(t, rpc)
		req.ReceivedAt = time.Now().Add(-30 * time.Millisecond)
		if err := observer.OnMessage(nil, req); err != nil {
			t.Fatalf("observe RPC: %v", err)
		}
		if i == 0 {
			select {
			case <-started:
			default:
				t.Fatal("first dispatch signal was not sent")
			}
		}
	}
	wait, samples, missing := observer.finish()
	if missing || samples != 2 || wait < 25*time.Millisecond {
		t.Fatalf("receive-to-dispatch = %s, %d samples, missing=%v; want both stamped RPCs and a wait of at least 25 ms", wait, samples, missing)
	}
	if calls != 2 {
		t.Fatalf("next handler calls = %d, want 2", calls)
	}
	if !(&rpcTimingHandler{next: observer}).CanRunConcurrently(tg.MessagesGetSearchCountersRequestTypeID) {
		t.Fatal("RPC timer did not forward the concurrent-RPC classifier")
	}
}

func TestReceiveToDispatchObserverFlagsMissingTimestamp(t *testing.T) {
	observer := &receiveToDispatchObserver{next: mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		return nil
	})}
	if _, err := observer.start(); err != nil {
		t.Fatal(err)
	}
	if err := observer.OnMessage(nil, perfRPCRequest(t, &tg.HelpGetConfigRequest{})); err != nil {
		t.Fatalf("observe RPC: %v", err)
	}
	_, samples, missing := observer.finish()
	if !missing || samples != 0 {
		t.Fatalf("missing timestamp = %v, samples = %d; want missing timestamp and no measured sample", missing, samples)
	}
}

func waitDispatchProbe(ctx context.Context, ready <-chan struct{}, what string) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for %s: %w", what, ctx.Err())
	}
}
