package mtproto

import (
	"context"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/transport"
)

type peerFrameResult struct {
	frame *bin.Buffer
	err   error
	done  chan struct{}
}

// peerFrameReader owns the one speculative receive that lets an active RPC
// observe a peer close. It retains at most one complete next frame and is also
// joined after the transport closes on every serving exit.
type peerFrameReader struct {
	server    *Server
	transport transport.Conn
	conn      *Conn
	pending   *peerFrameResult
}

func (r *peerFrameReader) prefetch() {
	if r.pending != nil {
		panic("MTProto peer reader already has a pending receive")
	}
	result := &peerFrameResult{frame: new(bin.Buffer), done: make(chan struct{})}
	r.pending = result
	go func() {
		result.err = r.transport.Recv(context.Background(), result.frame)
		if result.err != nil {
			r.conn.observePeerDisconnect()
			if r.conn.closeSource.Load() == closeSourcePeer {
				if err := r.conn.closeUnderlying(); err != nil && !isDisconnect(err) {
					r.server.log.Info("close transport after peer disconnect", "err", err)
				}
			}
		}
		close(result.done)
	}()
}

func (r *peerFrameReader) next(drainCtx context.Context, pendingLoginDeadline time.Time) (*bin.Buffer, error) {
	if err := drainCtx.Err(); err != nil {
		return nil, errServerDraining
	}
	if r.pending == nil {
		r.prefetch()
	}
	result := r.pending
	timeout := r.server.readTimeout
	if !pendingLoginDeadline.IsZero() {
		remaining := time.Until(pendingLoginDeadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	if timeout < 0 {
		timeout = 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-result.done:
		r.pending = nil
		return result.frame, result.err
	case <-drainCtx.Done():
		r.server.preserveDrainReadDeadline(r.transport)
		return nil, errServerDraining
	case <-timer.C:
		if err := r.conn.closeServer(); err != nil && !isDisconnect(err) {
			r.server.log.Info("close connection at read deadline", "err", err)
		}
		<-result.done
		r.pending = nil
		return nil, context.DeadlineExceeded
	}
}

func (r *peerFrameReader) join() {
	if r.pending != nil {
		<-r.pending.done
		r.pending = nil
	}
}
