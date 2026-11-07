package mtproto

import (
	"context"
	"errors"

	"github.com/gotd/td/proto"
)

const maxConcurrentRPCsPerConn = 1

func (c *Conn) startConcurrentRPC(msgID int64) bool {
	c.concurrentRPCMu.Lock()
	defer c.concurrentRPCMu.Unlock()
	if len(c.concurrentRPCs) >= maxConcurrentRPCsPerConn {
		return false
	}
	if c.concurrentRPCs == nil {
		c.concurrentRPCs = make(map[int64]chan struct{})
	}
	c.concurrentRPCs[msgID] = make(chan struct{})
	return true
}

func (c *Conn) finishConcurrentRPC(msgID int64, err error) {
	c.concurrentRPCMu.Lock()
	defer c.concurrentRPCMu.Unlock()
	if err != nil && c.concurrentRPCError == nil {
		c.concurrentRPCError = err
	}
	if done, ok := c.concurrentRPCs[msgID]; ok {
		delete(c.concurrentRPCs, msgID)
		close(done)
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
	if id == proto.MessageContainerTypeID || id == proto.GZIPTypeID {
		return true
	}
	if !isInvokeWrapper(id) {
		return false
	}
	_, transparent := concurrentRPCMethodID(req.Buf)
	return !transparent
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
	c.finishConcurrentRPC(req.MsgID, err)
}
