package mtproto

import (
	"context"
	"time"

	"github.com/gotd/td/bin"
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

// containerAcks returns the ids of the content messages in container, so a
// whole container is owed acknowledgement before its first entry runs. An
// entry queued behind a slow one would otherwise go unacknowledged until its
// own turn.
func containerAcks(container *proto.MessageContainer) []int64 {
	var ids []int64
	for _, m := range container.Messages {
		typeID, err := (&bin.Buffer{Buf: m.Body}).PeekID()
		if err != nil {
			continue
		}
		if needsAck(m.SeqNo, typeID) {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// queueAcks records msgIDs as owed an acknowledgement and arms the delayed
// flush if none is pending.
func (c *Conn) queueAcks(msgIDs ...int64) {
	if len(msgIDs) == 0 || c.acksStopped.Load() {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	room := maxMTProtoMessageIDs - len(c.pendingAcks)
	if room <= 0 {
		return
	}
	if len(msgIDs) > room {
		msgIDs = msgIDs[:room]
	}
	c.pendingAcks = append(c.pendingAcks, msgIDs...)
	if c.ackTimer == nil {
		c.ackTimer = time.AfterFunc(ackDelay, c.flushAcks)
	}
}

// answeredLocked drops msgID from the owed acknowledgements: the response
// being written for it acknowledges it. writeMu must be held.
func (c *Conn) answeredLocked(msgID int64) {
	for i, id := range c.pendingAcks {
		if id == msgID {
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
	ids := c.pendingAcks
	c.pendingAcks = nil
	c.ackTimer = nil
	if len(ids) == 0 || c.acksStopped.Load() {
		c.writeMu.Unlock()
		return
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
		return
	}
	if closeErr := c.closeAfterSendFailure(err); closeErr != nil && !isDisconnect(closeErr) {
		c.log.Info("close connection after failed msgs_ack", "err", closeErr)
	}
}

// stopAcks ends acknowledgement for a connection that has stopped serving, so
// a flush still armed writes nothing to the closed socket.
func (c *Conn) stopAcks() {
	c.acksStopped.Store(true)
}
