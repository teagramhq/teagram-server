package mtproto

import (
	"context"
	"errors"
	"slices"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/proto"
)

const (
	maxConcurrentRPCsPerConn       = 2
	maxQueuedConcurrentRPCsPerConn = 16
)

type concurrentRPCWork struct {
	msgID     int64
	run       func()
	admission *rpcAdmission
}

func (c *Conn) startConcurrentRPC(msgID int64, run func(), admission *rpcAdmission) (bool, bool) {
	c.concurrentRPCMu.Lock()
	if _, exists := c.concurrentRPCs[msgID]; exists {
		c.concurrentRPCMu.Unlock()
		return true, true
	}
	if len(c.concurrentRPCs) >= maxConcurrentRPCsPerConn+maxQueuedConcurrentRPCsPerConn {
		c.concurrentRPCMu.Unlock()
		return false, false
	}
	if c.concurrentRPCs == nil {
		c.concurrentRPCs = make(map[int64]chan struct{})
	}
	c.concurrentRPCs[msgID] = make(chan struct{})
	if c.concurrentRPCActive < maxConcurrentRPCsPerConn {
		c.concurrentRPCActive++
		c.concurrentRPCMu.Unlock()
		go run()
		return true, false
	}
	c.concurrentRPCQueue = append(c.concurrentRPCQueue, concurrentRPCWork{msgID: msgID, run: run, admission: admission})
	c.concurrentRPCMu.Unlock()
	return true, false
}

func (c *Conn) finishConcurrentRPC(msgID int64, err error) {
	c.concurrentRPCMu.Lock()
	if err != nil && c.concurrentRPCError == nil {
		c.concurrentRPCError = err
	}
	if done, ok := c.concurrentRPCs[msgID]; ok {
		delete(c.concurrentRPCs, msgID)
		close(done)
	}
	c.concurrentRPCActive--
	var next func()
	var discarded []*rpcAdmission
	if err != nil {
		for _, work := range c.concurrentRPCQueue {
			if done, ok := c.concurrentRPCs[work.msgID]; ok {
				delete(c.concurrentRPCs, work.msgID)
				close(done)
			}
			if work.admission != nil {
				discarded = append(discarded, work.admission)
			}
		}
		c.concurrentRPCQueue = nil
	} else if len(c.concurrentRPCQueue) > 0 {
		work := c.concurrentRPCQueue[0]
		c.concurrentRPCQueue[0] = concurrentRPCWork{}
		c.concurrentRPCQueue = c.concurrentRPCQueue[1:]
		c.concurrentRPCActive++
		next = work.run
	}
	c.concurrentRPCMu.Unlock()
	for _, admission := range discarded {
		admission.finish()
	}
	if next != nil {
		go next()
	}
}

func (c *Conn) concurrentRPCsPending() bool {
	c.concurrentRPCMu.Lock()
	defer c.concurrentRPCMu.Unlock()
	return len(c.concurrentRPCs) != 0
}

func (c *Conn) concurrentRPCFailure() error {
	c.concurrentRPCMu.Lock()
	defer c.concurrentRPCMu.Unlock()
	return c.concurrentRPCError
}

func (c *Conn) waitConcurrentRPCs() error {
	for {
		c.concurrentRPCMu.Lock()
		if len(c.concurrentRPCs) == 0 {
			err := c.concurrentRPCError
			c.concurrentRPCMu.Unlock()
			return err
		}
		done := make([]chan struct{}, 0, len(c.concurrentRPCs))
		for _, ch := range c.concurrentRPCs {
			done = append(done, ch)
		}
		c.concurrentRPCMu.Unlock()
		for _, ch := range done {
			<-ch
		}
	}
}

func concurrentRPCBarrier(req *Request) bool {
	if req == nil || req.Buf == nil {
		return false
	}
	id, err := req.Buf.PeekID()
	if err != nil {
		return false
	}
	if id == proto.GZIPTypeID {
		return true
	}
	if !isInvokeWrapper(id) {
		return false
	}
	_, transparent := concurrentRPCMethodID(req.Buf)
	return !transparent
}

func concurrentRPCRequestView(req *Request) (*Request, bool) {
	if req == nil || req.Buf == nil {
		return nil, false
	}
	id, err := req.Buf.PeekID()
	if err != nil {
		return nil, false
	}
	if id != proto.GZIPTypeID {
		return req, true
	}
	if req.gzipBody != nil {
		view := *req
		view.Buf = req.gzipBody
		return &view, true
	}
	var content proto.GZIP
	compressed := &bin.Buffer{Buf: slices.Clone(req.Buf.Buf)}
	if err := content.Decode(compressed); err != nil {
		return nil, false
	}
	req.gzipBody = &bin.Buffer{Buf: content.Data}
	view := *req
	view.Buf = req.gzipBody
	return &view, true
}

func (s *Server) runConcurrentRPC(c *Conn, req *Request, handler Handler) {
	err := s.handleWithHandler(c, req, handler)
	if errors.Is(err, context.Canceled) || errors.Is(err, errServerDraining) ||
		errors.Is(err, errPeerDisconnected) || isDisconnect(err) {
		err = nil
	}
	if err != nil {
		s.logConnectionFailure(err)
		if closeErr := c.closeServer(); closeErr != nil && !isDisconnect(closeErr) {
			s.log.Info("close connection after concurrent RPC failure", "err", closeErr)
		}
	}
	if req.admission != nil {
		req.admission.finish()
	}
	c.finishConcurrentRPC(req.MsgID, err)
}

func (s *Server) dispatchConcurrentRPC(c *Conn, req *Request, handler Handler) (bool, error) {
	if c == nil {
		return false, nil
	}
	view, valid := concurrentRPCRequestView(req)
	if valid && concurrentRPCBarrier(view) && c.concurrentRPCsPending() {
		if err := c.waitConcurrentRPCs(); err != nil {
			return true, err
		}
	}
	if !valid {
		return false, nil
	}
	classifier, ok := handler.(ConcurrentRPCClassifier)
	if !ok || !classifier.CanRunConcurrently(view) {
		return false, nil
	}
	if req.admission != nil && !req.admission.retain() {
		return true, errPeerDisconnected
	}
	req.Buf = &bin.Buffer{Buf: slices.Clone(req.Buf.Buf)}
	run := func() {
		s.runConcurrentRPC(c, req, handler)
	}
	started, duplicate := c.startConcurrentRPC(req.MsgID, run, req.admission)
	if duplicate {
		if req.admission != nil {
			req.admission.finish()
		}
		return true, nil
	}
	if !started {
		if req.admission != nil {
			req.admission.finish()
		}
		return true, s.handleWithHandler(c, req, HandlerFunc(func(c *Conn, req *Request) error {
			return c.SendErr(req, errInternalRPC)
		}))
	}
	return true, nil
}
