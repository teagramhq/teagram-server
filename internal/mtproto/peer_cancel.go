package mtproto

import (
	"context"
	"errors"
	"sync"
)

var (
	errPeerDisconnected    = errors.New("peer disconnected")
	errServerWriteDeadline = errors.New("server write deadline")
)

const (
	closeSourceUnknown uint32 = iota
	closeSourcePeer
	closeSourceServer
)

type activePeerRPC struct {
	mu           sync.Mutex
	done         bool
	cancelSource uint8
	userID       int64
	ctx          context.Context
	cancel       context.CancelFunc
}

const (
	activeCancelNone uint8 = iota
	activeCancelPeer
	activeCancelServer
)

func (r *activePeerRPC) cancelByPeer() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done || r.cancelSource != activeCancelNone || (r.ctx != nil && r.ctx.Err() != nil) {
		return false
	}
	r.cancelSource = activeCancelPeer
	r.cancel()
	return true
}

func (r *activePeerRPC) cancelByServer() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.done || r.cancelSource != activeCancelNone {
		return false
	}
	r.cancelSource = activeCancelServer
	r.cancel()
	return true
}

func (r *activePeerRPC) finish() {
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
}

func (c *Conn) beginActiveRPC(active *activePeerRPC) bool {
	c.activeRPCMu.Lock()
	defer c.activeRPCMu.Unlock()
	if c.peerLost.Load() || c.closeSource.Load() != closeSourceUnknown {
		return false
	}
	if c.activeRPC != nil {
		panic("overlapping MTProto RPCs on one connection")
	}
	c.activeRPC = active
	return true
}

func (c *Conn) finishActiveRPC(active *activePeerRPC) {
	active.finish()
	c.activeRPCMu.Lock()
	if c.activeRPC == active {
		c.activeRPC = nil
	}
	c.activeRPCMu.Unlock()
}

// observePeerDisconnect records the read-side observation before cancellation
// and consumes allowance only if the RPC is still active when cancellation is
// applied. A server-owned close has already recorded its source and wins this
// race without touching the peer budget.
func (c *Conn) observePeerDisconnect() {
	c.activeRPCMu.Lock()
	if c.closeSource.Load() != closeSourceUnknown {
		c.activeRPCMu.Unlock()
		return
	}
	c.closeSource.Store(closeSourcePeer)
	c.peerLost.Store(true)
	active := c.activeRPC
	c.activeRPCMu.Unlock()

	if active == nil || c.peerCancels == nil {
		return
	}
	reservation, denial := c.peerCancels.reserve(active.userID)
	if denial != peerCancelAllowed {
		return
	}
	if active.cancelByPeer() {
		reservation.commit()
	} else {
		reservation.release()
	}
}

// markServerClose records server ownership before the transport is closed and
// stops active work without consulting peer cancellation allowance.
func (c *Conn) markServerClose() {
	c.markServerCloseState(true)
}

func (c *Conn) markServerCloseState(cancelActive bool) {
	c.activeRPCMu.Lock()
	c.closeSource.CompareAndSwap(closeSourceUnknown, closeSourceServer)
	active := c.activeRPC
	c.activeRPCMu.Unlock()
	if cancelActive && active != nil {
		active.cancelByServer()
	}
}

func (c *Conn) prefetchPeerFrame() {
	if c.startPeerRead != nil {
		c.startPeerRead()
	}
}
