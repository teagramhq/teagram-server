package mtproto

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	errPeerDisconnected    = errors.New("peer disconnected")
	errPeerWriteFailure    = errors.New("peer write disconnect")
	errServerSendFailure   = errors.New("server send failure")
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

func (c *Conn) beginWriteDeadline(deadline time.Time) uint64 {
	c.activeRPCMu.Lock()
	c.writeDeadlineGeneration++
	if c.writeDeadlineGeneration == 0 {
		c.writeDeadlineGeneration++
	}
	generation := c.writeDeadlineGeneration
	c.activeWriteDeadline = deadline
	c.activeRPCMu.Unlock()
	return generation
}

func (c *Conn) finishWriteDeadline(generation uint64, expiredFailure bool) {
	c.activeRPCMu.Lock()
	if c.writeDeadlineGeneration == generation {
		if expiredFailure && !c.activeWriteDeadline.IsZero() && !time.Now().Before(c.activeWriteDeadline) {
			c.markServerCloseLocked()
		}
		c.activeWriteDeadline = time.Time{}
	}
	c.activeRPCMu.Unlock()
}

func (c *Conn) markWriteDeadlineExpired(generation uint64) {
	c.activeRPCMu.Lock()
	if c.writeDeadlineGeneration == generation && !c.activeWriteDeadline.IsZero() && !time.Now().Before(c.activeWriteDeadline) {
		c.markServerCloseLocked()
	}
	c.activeRPCMu.Unlock()
}

// markServerCloseLocked records server ownership without replacing an earlier
// peer observation or cancelling active work. A write deadline can be
// peer-triggered, so it must not bypass the peer cancellation budget.
// activeRPCMu must be held by the caller.
func (c *Conn) markServerCloseLocked() {
	if c.closeSource.Load() == closeSourceUnknown {
		c.closeSource.Store(closeSourceServer)
	}
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
	if !c.activeWriteDeadline.IsZero() && !time.Now().Before(c.activeWriteDeadline) {
		c.markServerCloseLocked()
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
	if c.closeSource.Load() == closeSourceUnknown {
		c.closeSource.Store(closeSourceServer)
	}
	// An explicit server close cancels active work even when a peer
	// observation was recorded first but peer cancellation was denied. Keep
	// that first observation for provenance; cancellation ownership is separate.
	active := c.activeRPC
	c.activeRPCMu.Unlock()
	if cancelActive && active != nil {
		active.cancelByServer()
	}
}

// markServerCloseUnlessPeer preserves an already observed peer disconnect.
// Push cleanup uses this when it must close a transport after a send failure:
// a peer denial must not be upgraded into an unbudgeted server cancellation.
func (c *Conn) markServerCloseUnlessPeer() {
	c.activeRPCMu.Lock()
	var active *activePeerRPC
	if c.closeSource.Load() == closeSourceUnknown {
		c.closeSource.Store(closeSourceServer)
		active = c.activeRPC
	}
	c.activeRPCMu.Unlock()
	if active != nil {
		active.cancelByServer()
	}
}

func (c *Conn) prefetchPeerFrame() {
	if c.startPeerRead != nil {
		c.startPeerRead()
	}
}
