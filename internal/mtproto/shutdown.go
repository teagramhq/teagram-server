package mtproto

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultDrainTimeout     = 90 * time.Second
	defaultRetirementWindow = 15 * time.Second
	defaultRetirementSlots  = 16
	drainAdmissionBit       = uint64(1) << 63
	drainAdmissionCountMask = drainAdmissionBit - 1
)

// serverShutdown separates the signal that stops reads and new RPC admission
// from the request context that stays live until work has drained or the hard
// cutoff expires. The admission count and drain bit share one atomic word so a
// request either joins the drain before it starts or is refused after it.
type serverShutdown struct {
	admission atomic.Uint64

	drainCtx    context.Context
	cancelDrain context.CancelFunc
	requestCtx  context.Context
	cancelReq   context.CancelFunc

	drainOnce    sync.Once
	rpcDrainOnce sync.Once
	rpcDrained   chan struct{}
	drainTimer   atomic.Pointer[time.Timer]
	serving      atomic.Int64

	retirementSeq    atomic.Int64
	drainTimeout     time.Duration
	retirementWindow time.Duration
	retirementSlots  int64
}

func newServerShutdown() *serverShutdown {
	drainCtx, cancelDrain := context.WithCancel(context.Background())
	requestCtx, cancelReq := context.WithCancel(context.Background())
	return &serverShutdown{
		drainCtx:         drainCtx,
		cancelDrain:      cancelDrain,
		requestCtx:       requestCtx,
		cancelReq:        cancelReq,
		rpcDrained:       make(chan struct{}),
		drainTimeout:     defaultDrainTimeout,
		retirementWindow: defaultRetirementWindow,
		retirementSlots:  defaultRetirementSlots,
	}
}

func (s *serverShutdown) startServing() {
	s.serving.Add(1)
}

func (s *serverShutdown) finishServing() {
	if s.serving.Add(-1) != 0 || !s.draining() {
		return
	}
	if timer := s.drainTimer.Load(); timer != nil {
		timer.Stop()
	}
	s.cancelReq()
}

func (s *serverShutdown) beginDrain() {
	s.drainOnce.Do(func() {
		for {
			state := s.admission.Load()
			if state&drainAdmissionBit != 0 || s.admission.CompareAndSwap(state, state|drainAdmissionBit) {
				if state&drainAdmissionCountMask == 0 {
					s.rpcDrainOnce.Do(func() { close(s.rpcDrained) })
				}
				break
			}
		}
		timer := time.AfterFunc(s.drainTimeout, s.cancelReq)
		s.drainTimer.Store(timer)
		s.cancelDrain()
	})
}

func (s *serverShutdown) draining() bool {
	return s.admission.Load()&drainAdmissionBit != 0
}

func (s *serverShutdown) admitRPC() (func(), bool) {
	for {
		state := s.admission.Load()
		if state&drainAdmissionBit != 0 || state&drainAdmissionCountMask == drainAdmissionCountMask {
			return nil, false
		}
		if s.admission.CompareAndSwap(state, state+1) {
			return s.finishRPC, true
		}
	}
}

func (s *serverShutdown) finishRPC() {
	state := s.admission.Add(^uint64(0))
	if state&drainAdmissionBit != 0 && state&drainAdmissionCountMask == 0 {
		s.rpcDrainOnce.Do(func() { close(s.rpcDrained) })
	}
}

func (s *serverShutdown) waitForRPCs() {
	select {
	case <-s.rpcDrained:
	case <-s.requestCtx.Done():
	}
}

func (s *serverShutdown) waitForRetirement() {
	if !s.draining() {
		return
	}
	delay := s.retirementDelay()
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-s.requestCtx.Done():
	}
}

func (s *serverShutdown) retirementDelay() time.Duration {
	if s.retirementSlots <= 1 || s.retirementWindow <= 0 {
		return 0
	}
	step := s.retirementWindow / time.Duration(s.retirementSlots-1)
	if step <= 0 {
		return 0
	}
	position := s.retirementSeq.Add(1) - 1
	return time.Duration(position%s.retirementSlots) * step
}
