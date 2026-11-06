package mtproto

import (
	"context"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
)

// ackDelay is how long a received content message may wait for its response
// before the server acknowledges it on its own. A response written within the
// delay is the acknowledgement, so fast requests cost no extra frame. It stays
// well under the 5s gotd waits before resending an unacknowledged request.
const ackDelay = time.Second

// needsAck reports whether a client message with seqNo and body type typeID
// is content-related and so must be acknowledged. Clients mark content
// messages with an odd seqno; the type check keeps a misnumbered
// acknowledgement, container or status notification from being acked back.
func needsAck(seqNo int, typeID uint32) bool {
	if seqNo%2 == 0 {
		return false
	}
	switch typeID {
	case mt.MsgsAckTypeID, proto.MessageContainerTypeID, mt.MsgCopyTypeID, mt.MsgsAllInfoTypeID:
		return false
	}
	return true
}

type queuedAck struct {
	messageID           int64
	rpcTypeID           uint32
	typeIDKnown         bool
	typeIDAuthoritative bool
}

// containerAcks returns the ids of the content messages in container, so a
// whole container is owed acknowledgement before its first entry runs. An
// entry queued behind a slow one would otherwise go unacknowledged until its
// own turn.
func containerAcks(container *proto.MessageContainer) []queuedAck {
	var acks []queuedAck
	for _, m := range container.Messages {
		typeID, err := (&bin.Buffer{Buf: m.Body}).PeekID()
		if err != nil {
			continue
		}
		if needsAck(m.SeqNo, typeID) {
			acks = append(acks, queuedAck{messageID: m.ID, rpcTypeID: typeID, typeIDKnown: true})
		}
	}
	return acks
}

// queueAcks records msgIDs as owed an acknowledgement and arms the delayed
// flush if none is pending.
func (c *Conn) queueAcks(msgIDs ...int64) {
	acks := make([]queuedAck, 0, len(msgIDs))
	for _, id := range msgIDs {
		acks = append(acks, queuedAck{messageID: id})
	}
	c.queueAckRecords(acks...)
}

func (c *Conn) queueRPCAck(messageID int64, rpcTypeID uint32) {
	c.queueAckRecords(queuedAck{
		messageID:           messageID,
		rpcTypeID:           rpcTypeID,
		typeIDKnown:         true,
		typeIDAuthoritative: true,
	})
}

// queueInitialRPCAck records the top-level constructor before wrapper decoding.
// Dispatch replaces it with the authoritative leaf RPC constructor.
func (c *Conn) queueInitialRPCAck(messageID int64, rpcTypeID uint32) {
	c.queueAckRecords(queuedAck{messageID: messageID, rpcTypeID: rpcTypeID, typeIDKnown: true})
}

func (c *Conn) queueAckRecords(acks ...queuedAck) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.queueAcksLocked(acks...)
}

// queueBoundAcks drops deferred read-ahead work if dispatch has since changed
// the binding. Check under writeMu so a rebind cannot reset the queue between
// this check and enqueue.
func (c *Conn) queueBoundAckRecords(keyID [8]byte, session int64, acks ...queuedAck) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.authKey.ID != keyID || c.sessionID != session {
		return
	}
	c.queueAcksLocked(acks...)
}

// queueAcksLocked updates the queue and its timer while writeMu is held.
func (c *Conn) queueAcksLocked(acks ...queuedAck) {
	if len(acks) == 0 || c.acksStopped.Load() {
		return
	}
	// A frame read ahead during an active RPC is queued on arrival and again
	// when it is dispatched; one entry is enough.
	for _, ack := range acks {
		found := false
		for i := range c.pendingAcks {
			if c.pendingAcks[i].messageID == ack.messageID {
				// Read-ahead sees an outer wrapper, while dispatch resolves it to
				// the leaf RPC. A late read-ahead enqueue must not replace that ID.
				if ack.typeIDKnown && (!c.pendingAcks[i].typeIDKnown || ack.typeIDAuthoritative) {
					c.pendingAcks[i] = ack
				}
				found = true
				break
			}
		}
		if found {
			continue
		}
		if len(c.pendingAcks) >= maxMTProtoMessageIDs {
			break
		}
		c.pendingAcks = append(c.pendingAcks, ack)
	}
	if len(c.pendingAcks) > 0 && c.ackTimer == nil {
		c.ackTimer = time.AfterFunc(ackDelay, c.flushAcks)
	}
}

// answeredLocked drops msgID from the owed acknowledgements: the response
// being written for it acknowledges it. writeMu must be held.
func (c *Conn) answeredLocked(msgID int64) {
	for i, ack := range c.pendingAcks {
		if ack.messageID == msgID {
			c.pendingAcks = append(c.pendingAcks[:i], c.pendingAcks[i+1:]...)
			return
		}
	}
}

// flushAcks writes one msgs_ack naming every content message still waiting for
// its response. It runs on its own goroutine, so it is admitted like a push and
// retirement waits for it.
func (c *Conn) flushAcks() {
	if !c.beginPush() {
		return
	}
	defer c.finishPush()
	ctx, cancel := c.boundedWriteContext(context.Background())
	defer cancel()

	c.writeMu.Lock()
	acks := c.pendingAcks
	c.pendingAcks = nil
	c.ackTimer = nil
	if len(acks) == 0 || c.acksStopped.Load() {
		c.writeMu.Unlock()
		return
	}
	ids := make([]int64, len(acks))
	typeIDs := make([]uint32, 0, len(acks))
	seenTypeIDs := make(map[uint32]struct{}, len(acks))
	for i, ack := range acks {
		ids[i] = ack.messageID
		if !ack.typeIDKnown {
			continue
		}
		if _, exists := seenTypeIDs[ack.rpcTypeID]; exists {
			continue
		}
		seenTypeIDs[ack.rpcTypeID] = struct{}{}
		typeIDs = append(typeIDs, ack.rpcTypeID)
	}
	var b bin.Buffer
	if err := (&mt.MsgsAck{MsgIDs: ids}).Encode(&b); err != nil {
		c.writeMu.Unlock()
		c.log.Error("encode msgs_ack", "err", err)
		return
	}
	err := c.sendLocked(ctx, proto.MessageFromServer, &b)
	c.writeMu.Unlock()
	if err == nil {
		attrs := []any{"operation", "msgs_ack"}
		if len(typeIDs) == 1 {
			attrs = append(attrs, "rpc_type_id", typeIDs[0])
		} else if len(typeIDs) > 1 {
			attrs = append(attrs, "rpc_type_ids", typeIDs)
		}
		c.log.Debug("server operation succeeded", attrs...)
		return
	}
	if c.ackFailed != nil {
		c.ackFailed(err, typeIDs)
	}
	if closeErr := c.closeAfterSendFailure(err); closeErr != nil && !isDisconnect(closeErr) {
		c.log.Info("close connection after failed msgs_ack", "err", closeErr)
	}
}

type readAheadAckBinding struct {
	key     crypto.AuthKey
	session int64
}

// queueReadAheadAcks queues the acknowledgements owed for frame, a frame the
// peer reader received while an earlier request is still being handled. The
// serve loop dispatches it only once that request finishes, which can be past
// the client's resend interval. A frame that is not under the connection's
// current key and session is left for the serve loop, which validates it.
//
// The immutable binding snapshot avoids waiting behind a stalled socket write.
// Deferred queueing rechecks the binding under writeMu before admitting IDs.
func (c *Conn) queueReadAheadAcks(frame []byte) {
	encrypted := &crypto.EncryptedMessage{}
	if err := encrypted.DecodeWithoutCopy(&bin.Buffer{Buf: frame}); err != nil {
		return
	}
	binding := c.ackBinding.Load()
	if binding == nil {
		return
	}
	key, session := binding.key, binding.session
	if key.ID != encrypted.AuthKeyID {
		return
	}
	msg, err := c.cipher.Decrypt(key, encrypted)
	if err != nil || msg.SessionID != session {
		return
	}
	perfRPCFrameEvent(c, msg.Data(), msg.MessageID, "receive")
	body := &bin.Buffer{Buf: msg.Data()}
	typeID, err := body.PeekID()
	if err != nil {
		return
	}
	if typeID == proto.MessageContainerTypeID {
		var container proto.MessageContainer
		if err := container.Decode(body); err != nil {
			return
		}
		if acks := containerAcks(&container); len(acks) > 0 {
			go c.queueBoundAckRecords(key.ID, session, acks...)
		}
		return
	}
	if needsAck(int(msg.SeqNo), typeID) {
		go c.queueBoundAckRecords(key.ID, session, queuedAck{
			messageID: msg.MessageID, rpcTypeID: typeID, typeIDKnown: true,
		})
	}
}

// stopAcks ends acknowledgement for a connection that has stopped serving, so
// a flush still armed writes nothing to the closed socket.
func (c *Conn) stopAcks() {
	c.acksStopped.Store(true)
}
