package mtproto

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
)

// Bounds on what one connection remembers about requests it has answered.
// Evicting an entry raises the cache's low-water mark, so the bound costs a
// late resend a bad_msg_notification, never a second execution.
const (
	rpcReplayMaxEntries    = 512
	rpcReplayMaxBytes      = 512 << 10
	rpcReplayMaxEntryBytes = 64 << 10
)

// badMsgIDTooLow is the bad_msg_notification code for a msg_id the server can
// no longer tell apart from a resend.
const badMsgIDTooLow = 16

type rpcReplayVerdict int

const (
	rpcReplayExecute rpcReplayVerdict = iota
	rpcReplayAnswer
	rpcReplayDrop
	rpcReplayTooLow
)

// rpcReplayCache remembers the content requests dispatched on one auth key and
// session, with the rpc_result each produced, so a retransmit under the same
// msg_id is answered instead of executed twice. It is guarded by writeMu.
//
// A key or session change clears it; an owner change deliberately does not. A
// self-revocation unbinds the key between a request and its resend, and every
// stored result was already sent to this same key and session.
type rpcReplayCache struct {
	authKey int64
	session int64
	// results maps a dispatched msg_id to its encoded rpc_result. A nil result
	// means the request is still running, or its result was over
	// rpcReplayMaxEntryBytes: either way a duplicate is dropped.
	results map[int64][]byte
	// order holds the msg_ids in results, oldest first.
	order []int64
	bytes int
	// lowWater is the highest evicted msg_id. A msg_id at or below it may be a
	// resend of a forgotten request, so it is refused rather than run.
	lowWater int64
}

func (r *rpcReplayCache) reset(authKey, session int64) {
	*r = rpcReplayCache{authKey: authKey, session: session}
}

// begin records msgID as dispatched unless it was seen before, in which case
// it reports how to treat the duplicate and any result to answer it with.
func (r *rpcReplayCache) begin(authKey, session, msgID int64) (rpcReplayVerdict, []byte) {
	if r.authKey != authKey || r.session != session {
		r.reset(authKey, session)
	}
	if result, ok := r.results[msgID]; ok {
		if result == nil {
			return rpcReplayDrop, nil
		}
		return rpcReplayAnswer, result
	}
	if msgID <= r.lowWater {
		return rpcReplayTooLow, nil
	}
	if r.results == nil {
		r.results = make(map[int64][]byte)
	}
	r.results[msgID] = nil
	r.order = append(r.order, msgID)
	r.evict()
	return rpcReplayExecute, nil
}

// awaiting reports whether msgID is dispatched in this scope and still has no
// remembered result.
func (r *rpcReplayCache) awaiting(authKey, session, msgID int64) bool {
	if r.authKey != authKey || r.session != session {
		return false
	}
	result, ok := r.results[msgID]
	return ok && result == nil
}

// record stores the encoded rpc_result sent for msgID. A result over the
// per-entry cap stays unrecorded, leaving the request answered without a body.
func (r *rpcReplayCache) record(authKey, session, msgID int64, result []byte) {
	if !r.awaiting(authKey, session, msgID) || len(result) > rpcReplayMaxEntryBytes {
		return
	}
	r.results[msgID] = result
	r.bytes += len(result)
	r.evict()
}

func (r *rpcReplayCache) evict() {
	for len(r.order) > rpcReplayMaxEntries || r.bytes > rpcReplayMaxBytes {
		oldest := r.order[0]
		r.order = r.order[1:]
		r.bytes -= len(r.results[oldest])
		delete(r.results, oldest)
		r.lowWater = max(r.lowWater, oldest)
	}
}

func requestAuthKeyID(req *Request) int64 {
	return crypto.AuthKey{ID: req.AuthKeyID}.IntID()
}

// admitContentRPC reports whether req, a content request, should be executed.
// A duplicate of an answered request is answered again with the stored
// rpc_result under a fresh server msg_id; a duplicate of a running request, or
// of one whose result was too large to keep, is dropped; a msg_id at or below
// the low-water mark is refused with bad_msg_notification 16.
func (c *Conn) admitContentRPC(req *Request) (bool, error) {
	writeCtx, cancel := c.boundedWriteContext(context.WithoutCancel(req.Ctx))
	defer cancel()

	var sendErr error
	c.writeMu.Lock()
	verdict, result := c.rpcReplay.begin(requestAuthKeyID(req), req.SessionID, req.MsgID)
	switch verdict {
	case rpcReplayExecute:
		c.writeMu.Unlock()
		return true, nil
	case rpcReplayDrop:
		c.writeMu.Unlock()
		return false, nil
	case rpcReplayAnswer:
		// sendLocked encrypts in place, so the stored result is copied.
		sendErr = c.sendLocked(writeCtx, proto.MessageServerResponse, &bin.Buffer{Buf: slices.Clone(result)})
	case rpcReplayTooLow:
		var b bin.Buffer
		if err := (&mt.BadMsgNotification{
			BadMsgID:    req.MsgID,
			BadMsgSeqno: req.seqNo,
			ErrorCode:   badMsgIDTooLow,
		}).Encode(&b); err != nil {
			c.writeMu.Unlock()
			return false, fmt.Errorf("encode bad_msg_notification: %w", err)
		}
		sendErr = c.sendLocked(writeCtx, proto.MessageServerResponse, &b)
	}
	c.writeMu.Unlock()
	if sendErr != nil {
		if closeErr := c.closeAfterSendFailure(sendErr); closeErr != nil {
			sendErr = errors.Join(sendErr, fmt.Errorf("close failed RPC transport: %w", closeErr))
		}
		return false, fmt.Errorf("answer duplicate request: %w", sendErr)
	}
	return false, nil
}
