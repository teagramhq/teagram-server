package mtproto

import (
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/proto"
)

const (
	maxConcurrentRPCsPerConn       = 2
	maxQueuedConcurrentRPCsPerConn = 16
)

type concurrentRPCWork struct {
	msgID   int64
	run     func()
	release func()
}

// concurrentRPCSet owns the in-flight and queued work for one connection.
// Entries remain present exactly while their work is running or queued.
type concurrentRPCSet struct {
	mu      sync.Mutex
	entries map[int64]chan struct{}
	active  int
	queue   []concurrentRPCWork
	err     error
}

func (s *concurrentRPCSet) start(msgID int64, work concurrentRPCWork) (started, duplicate bool) {
	s.mu.Lock()
	if _, exists := s.entries[msgID]; exists {
		s.mu.Unlock()
		return true, true
	}
	if len(s.entries) >= maxConcurrentRPCsPerConn+maxQueuedConcurrentRPCsPerConn {
		s.mu.Unlock()
		return false, false
	}
	if s.entries == nil {
		s.entries = make(map[int64]chan struct{})
	}
	s.entries[msgID] = make(chan struct{})
	if s.active < maxConcurrentRPCsPerConn {
		s.active++
		s.mu.Unlock()
		go work.run()
		return true, false
	}
	s.queue = append(s.queue, work)
	s.mu.Unlock()
	return true, false
}

// finish removes one completed entry and returns the number of queued entries
// discarded after an error. Retained admissions are released outside the set
// lock so shutdown accounting cannot run arbitrary cleanup under it.
func (s *concurrentRPCSet) finish(msgID int64, err error) int {
	s.mu.Lock()
	if err != nil && s.err == nil {
		s.err = err
	}
	if done, ok := s.entries[msgID]; ok {
		delete(s.entries, msgID)
		close(done)
	}
	s.active--
	var next func()
	var discarded []func()
	discardedCount := 0
	if err != nil {
		for _, work := range s.queue {
			discardedCount++
			if done, ok := s.entries[work.msgID]; ok {
				delete(s.entries, work.msgID)
				close(done)
			}
			if work.release != nil {
				discarded = append(discarded, work.release)
			}
		}
		s.queue = nil
	} else if len(s.queue) > 0 {
		work := s.queue[0]
		s.queue[0] = concurrentRPCWork{}
		s.queue = s.queue[1:]
		s.active++
		next = work.run
	}
	s.mu.Unlock()
	for _, release := range discarded {
		release()
	}
	if next != nil {
		go next()
	}
	return discardedCount
}

func (s *concurrentRPCSet) pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries) != 0
}

func (s *concurrentRPCSet) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *concurrentRPCSet) wait() error {
	for {
		s.mu.Lock()
		if len(s.entries) == 0 {
			err := s.err
			s.mu.Unlock()
			return err
		}
		done := make([]chan struct{}, 0, len(s.entries))
		for _, ch := range s.entries {
			done = append(done, ch)
		}
		s.mu.Unlock()
		for _, ch := range done {
			<-ch
		}
	}
}

func concurrentRPCBarrier(req *Request) bool {
	if req == nil {
		return false
	}
	body := req.BodyBuffer()
	if body == nil {
		return false
	}
	id, err := body.PeekID()
	if err != nil {
		return false
	}
	if id == proto.GZIPTypeID {
		return true
	}
	if !isInvokeWrapper(id) {
		return false
	}
	_, transparent := concurrentRPCMethodID(body)
	return !transparent
}

func concurrentRPCRequestView(req *Request) (*Request, bool) {
	if req == nil || req.Buf == nil {
		return nil, false
	}
	body := req.BodyBuffer()
	if body == nil {
		return nil, false
	}
	id, err := body.PeekID()
	if err != nil {
		return nil, false
	}
	if id != proto.GZIPTypeID {
		return req, true
	}
	if req.gzipBody == nil {
		var content proto.GZIP
		compressed := &bin.Buffer{Buf: body.Buf}
		if err := content.Decode(compressed); err != nil {
			return nil, false
		}
		req.gzipBody = &bin.Buffer{Buf: content.Data}
	}
	view := *req
	view.Body = req.gzipBody
	return &view, true
}

func concurrentRPCMethodName(req *Request) string {
	if req != nil && req.rpcTypeIDSet {
		if method := compiledMethodName(req.rpcTypeID); method != "" {
			return method
		}
	}
	return requestRPCMethod(req)
}

func (s *Server) runConcurrentRPC(c *Conn, req *Request, handler Handler, release func()) {
	err := s.handleWithHandler(c, req, handler)
	if req.contextCanceled || errors.Is(err, errServerDraining) || errors.Is(err, errPeerDisconnected) {
		err = nil
	}
	if err != nil {
		if closeErr := c.closeServer(); closeErr != nil && !isDisconnect(closeErr) {
			s.log.Info("close connection after concurrent RPC failure", "err", closeErr)
		}
	}
	if release != nil {
		release()
	}
	discarded := c.concurrentRPCs.finish(req.MsgID, err)
	if err != nil {
		if dropped, ok := s.concurrentRPCFailureLog.allow(time.Now(), preAuthLogInterval); ok {
			s.log.Info("concurrent RPC failed",
				"method", concurrentRPCMethodName(req),
				"auth_key_id", requestAuthKeyID(req),
				"discarded_queued_rpcs", discarded,
				"error", safeRPCErrorName(err),
				"suppressed", dropped)
		}
		s.logConnectionFailure(err)
	}
}

func (s *Server) dispatchConcurrentRPC(c *Conn, req *Request, handler Handler) (bool, error) {
	if c == nil {
		return false, nil
	}
	view, valid := concurrentRPCRequestView(req)
	if valid && concurrentRPCBarrier(view) && c.concurrentRPCs.pending() {
		if err := c.concurrentRPCs.wait(); err != nil {
			return true, err
		}
	}
	if !valid {
		return false, nil
	}
	typeID, eligible := concurrentRPCMethodID(view.BodyBuffer())
	if !eligible {
		return false, nil
	}
	classifier, ok := handler.(ConcurrentRPCClassifier)
	if !ok || !classifier.CanRunConcurrently(typeID) {
		return false, nil
	}
	req.rpcTypeID = typeID
	req.rpcTypeIDSet = true
	var release func()
	if req.admission != nil {
		release = req.admission.retain()
		if release == nil {
			return true, errPeerDisconnected
		}
	}
	packed := req.Buf
	req.Buf = &bin.Buffer{Buf: slices.Clone(packed.Buf)}
	if req.Body == packed {
		req.Body = req.Buf
	} else if req.Body != nil && req.Body != req.gzipBody {
		req.Body = &bin.Buffer{Buf: slices.Clone(req.Body.Buf)}
	}
	work := concurrentRPCWork{
		msgID:   req.MsgID,
		release: release,
		run:     func() { s.runConcurrentRPC(c, req, handler, release) },
	}
	started, duplicate := c.concurrentRPCs.start(req.MsgID, work)
	if duplicate {
		if release != nil {
			release()
		}
		return true, nil
	}
	if !started {
		if release != nil {
			release()
		}
		if dropped, allow := s.concurrentRPCOverloadLog.allow(time.Now(), preAuthLogInterval); allow {
			s.log.Info("concurrent RPC queue full",
				"method", concurrentRPCMethodName(req),
				"auth_key_id", requestAuthKeyID(req),
				"suppressed", dropped)
		}
		return true, s.handleWithHandler(c, req, HandlerFunc(func(c *Conn, req *Request) error {
			return c.SendErr(req, errInternalRPC)
		}))
	}
	return true, nil
}
