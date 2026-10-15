package mtproto

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/transport"
)

const (
	peerQueueFrames = 8
	peerQueueBytes  = 1 << 20
)

type peerFrameResult struct {
	frame *bin.Buffer
	err   error
}

// peerFrameReader owns all receives after RPC admission. Its queue and byte
// accounting belong to run; channels transfer frames without another lock.
// One receive may cross the byte threshold, but never the frame threshold.
type peerFrameReader struct {
	server     *Server
	transport  transport.Conn
	conn       *Conn
	frames     chan peerFrameResult
	demand     chan struct{}
	continuous chan struct{}
	stop       chan struct{}
	done       chan struct{}
	enabled    bool
}

func (r *peerFrameReader) start() {
	if r.done != nil {
		return
	}
	r.frames = make(chan peerFrameResult)
	r.demand = make(chan struct{})
	r.continuous = make(chan struct{})
	r.stop = make(chan struct{})
	r.done = make(chan struct{})
	go r.run()
}

func (r *peerFrameReader) prefetch() {
	r.start()
	if !r.enabled {
		r.enabled = true
		close(r.continuous)
	}
}

func (r *peerFrameReader) run() {
	defer close(r.done)
	var queue []peerFrameResult
	var buffered int
	var receiving <-chan peerFrameResult
	continuous := r.continuous
	enabled, requested, ended := false, false, false
	for {
		if receiving == nil && !ended && (enabled || requested) && len(queue) < peerQueueFrames && buffered < peerQueueBytes {
			result := make(chan peerFrameResult, 1)
			receiving = result
			requested = false
			go func() {
				frame := new(bin.Buffer)
				err := r.transport.Recv(context.Background(), frame)
				if err == nil {
					r.conn.queueReadAheadAcks(frame.Buf)
				} else {
					r.conn.observePeerDisconnect()
					if r.conn.closeSource.Load() == closeSourcePeer {
						if closeErr := r.conn.closeUnderlying(); closeErr != nil && !isDisconnect(closeErr) {
							r.server.log.Info("close transport after peer disconnect", "err", closeErr)
						}
					}
				}
				result <- peerFrameResult{frame: frame, err: err}
			}()
		}
		var output chan peerFrameResult
		var first peerFrameResult
		if len(queue) > 0 {
			output, first = r.frames, queue[0]
		}
		select {
		case <-r.stop:
			if receiving != nil {
				<-receiving
			}
			return
		case <-continuous:
			enabled = true
			continuous = nil
		case <-r.demand:
			requested = true
		case result := <-receiving:
			receiving = nil
			ended = result.err != nil
			queue = append(queue, result)
			buffered += result.frame.Len()
		case output <- first:
			buffered -= first.frame.Len()
			queue[0] = peerFrameResult{}
			queue = queue[1:]
		}
	}
}

func (r *peerFrameReader) next(drainCtx context.Context, readDeadline time.Time, idleDeadline bool) (*bin.Buffer, error) {
	if err := drainCtx.Err(); err != nil {
		return nil, errServerDraining
	}
	r.start()
	if !r.enabled {
		select {
		case r.demand <- struct{}{}:
		case <-drainCtx.Done():
			return nil, errServerDraining
		}
	}
	timeout := r.server.readTimeout
	if !readDeadline.IsZero() {
		timeout = time.Until(readDeadline)
	}
	if timeout < 0 {
		timeout = 0
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case result := <-r.frames:
		return result.frame, result.err
	case <-drainCtx.Done():
		r.server.preserveDrainReadDeadline(r.transport)
		return nil, errServerDraining
	case <-timer.C:
		if err := r.conn.closeServer(); err != nil && !isDisconnect(err) {
			r.server.log.Info("close connection at read deadline", "err", err)
		}
		r.join()
		if idleDeadline {
			return nil, fmt.Errorf("%w: %w", errIdleReadTimeout, context.DeadlineExceeded)
		}
		return nil, context.DeadlineExceeded
	}
}

// join follows transport close, which releases any outstanding receive.
func (r *peerFrameReader) join() {
	if r.done != nil {
		close(r.stop)
		<-r.done
		r.done = nil
	}
}

// peerQueueConn keeps exchanges on the same reader after read-ahead begins.
// The exchanger still owns its contexts and protocol admission.
type peerQueueConn struct {
	transport.Conn

	reader *peerFrameReader
}

func (c peerQueueConn) Recv(ctx context.Context, b *bin.Buffer) error {
	r := c.reader
	r.start()
	if !r.enabled {
		select {
		case r.demand <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case result := <-r.frames:
		b.ResetTo(result.frame.Buf)
		return result.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
