package api

import (
	"errors"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

var historyTTLRateLimit = store.RateLimitConfig{Limit: 1, Window: time.Minute}

func (h *handlers) handleSetHistoryTTLAfterReplyOnConn(
	c *mtproto.Conn, r *mtproto.Request,
) (bin.Encoder, *replyUpdate, func(), error) {
	var req tg.MessagesSetHistoryTTLRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, nil, errAuthKeyUnreg
	}
	if !store.ValidHistoryTTLPeriod(req.Period) {
		return nil, nil, nil, errTTLPeriodInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, nil, nil, err
	}
	switch peerType {
	case store.PeerTypeUser:
		return h.setUserHistoryTTLAfterReply(c, r, peerID, req.Period)
	case store.PeerTypeChat:
		return h.setChatHistoryTTLAfterReply(c, r, peerID, req.Period)
	case store.PeerTypeChannel:
		return h.setChannelHistoryTTLAfterReply(r, peerID, req.Period)
	default:
		return nil, nil, nil, errPeerIDInvalid
	}
}

func (h *handlers) setUserHistoryTTLAfterReply(
	c *mtproto.Conn, r *mtproto.Request, peerID int64, period int,
) (bin.Encoder, *replyUpdate, func(), error) {
	if peerID == r.UserID {
		return nil, nil, nil, errPeerIDInvalid
	}
	attempt := beginSenderRPC(c, r)
	sender, senderPts, peerPts, changed, err := h.store.SetUserHistoryTTL(r.Ctx, r.UserID, peerID, period, historyTTLRateLimit)
	if err != nil {
		clearSenderRPC(attempt)
		return nil, nil, nil, h.mapHistoryTTLError(r, "user", peerID, err)
	}
	if !changed {
		clearSenderRPC(attempt)
		return &tg.Updates{Date: int(h.now().Unix())}, nil, nil, nil
	}
	setSenderRPCPts(attempt, senderPts)
	users, err := h.twoUsers(r.Ctx, r.UserID, peerID)
	if err != nil {
		h.clearSenderAndNotify(attempt, r)
		h.notify(r.Ctx, peerID)
		h.log.Error("set user history TTL: load users", "user_id", r.UserID, "peer_id", peerID, "err", err)
		return nil, nil, nil, errInternal
	}
	result := &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateNewMessage{
			Message: messageToTL(sender, nil, nil, nil, nil), Pts: senderPts, PtsCount: 1,
		}},
		Users: users,
		Date:  int(sender.Date.Unix()),
	}
	var update *replyUpdate
	if c != nil {
		update = &replyUpdate{
			owner: r.UserID, authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID), pts: senderPts,
			onFailure: func() {
				h.clearSenderAndNotify(attempt, r)
				if peerPts > 0 {
					h.notify(r.Ctx, peerID)
				}
			},
		}
	}
	afterReply := func() {
		if c != nil {
			h.notifySendAfterReply(r, senderPts)
		} else {
			h.notify(r.Ctx, r.UserID)
		}
		if peerPts > 0 {
			h.notify(r.Ctx, peerID)
		}
	}
	return result, update, afterReply, nil
}

func (h *handlers) setChatHistoryTTLAfterReply(
	c *mtproto.Conn, r *mtproto.Request, chatID int64, period int,
) (bin.Encoder, *replyUpdate, func(), error) {
	attempt := beginSenderRPC(c, r)
	chat, sender, perOwner, changed, err := h.store.SetChatHistoryTTL(r.Ctx, chatID, r.UserID, period, historyTTLRateLimit)
	if err != nil {
		clearSenderRPC(attempt)
		return nil, nil, nil, h.mapHistoryTTLError(r, "chat", chatID, err)
	}
	if !changed {
		clearSenderRPC(attempt)
		return &tg.Updates{Date: int(h.now().Unix())}, nil, nil, nil
	}
	senderPts := perOwner[r.UserID]
	setSenderRPCPts(attempt, senderPts)
	skipSender := int64(0)
	if c != nil {
		skipSender = r.UserID
	}
	h.notifyOwners(r.Ctx, perOwner, skipSender)
	updates, err := h.chatUpdate(r.Ctx, r.UserID, chat, sender, perOwner, nil)
	if err != nil {
		if c != nil {
			h.clearSenderAndNotify(attempt, r)
		}
		h.log.Error("set chat history TTL: render update", "user_id", r.UserID, "chat_id", chatID, "err", err)
		return nil, nil, nil, errInternal
	}
	var update *replyUpdate
	if c != nil {
		update = &replyUpdate{
			owner: r.UserID, authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID), pts: senderPts,
			onFailure: func() { h.clearSenderAndNotify(attempt, r) },
		}
	}
	afterReply := func() {
		if c != nil {
			h.notifySendAfterReply(r, senderPts)
		} else {
			h.notify(r.Ctx, r.UserID)
		}
	}
	return updates, update, afterReply, nil
}

func (h *handlers) setChannelHistoryTTLAfterReply(
	r *mtproto.Request, channelID int64, period int,
) (bin.Encoder, *replyUpdate, func(), error) {
	channel, service, pts, changed, err := h.store.SetChannelHistoryTTL(r.Ctx, channelID, r.UserID, period, historyTTLRateLimit)
	if err != nil {
		return nil, nil, nil, h.mapHistoryTTLError(r, "channel", channelID, err)
	}
	if !changed {
		return &tg.Updates{Date: int(h.now().Unix())}, nil, nil, nil
	}
	chats, err := h.loadChannels(r.Ctx, map[int64]bool{channelID: true}, r.UserID)
	if err != nil {
		h.notifyChannelPost(r.Ctx, channelID)
		h.log.Error("set channel history TTL: load channel", "user_id", r.UserID, "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
	if err != nil {
		h.notifyChannelPost(r.Ctx, channelID)
		h.log.Error("set channel history TTL: load user", "user_id", r.UserID, "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	message, err := channelMessageToTL(service, r.UserID, nil, !channel.Megagroup)
	if err != nil {
		h.notifyChannelPost(r.Ctx, channelID)
		h.log.Error("set channel history TTL: render service message", "channel_id", channelID, "local_id", service.LocalID, "err", err)
		return nil, nil, nil, errInternal
	}
	result := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateNewChannelMessage{Message: message, Pts: pts, PtsCount: 1},
			&tg.UpdateChannel{ChannelID: channelID},
		},
		Chats: chats,
		Users: users,
		Date:  int(service.Date.Unix()),
	}
	return result, nil, func() { h.notifyChannelPost(r.Ctx, channelID) }, nil
}

func (h *handlers) mapHistoryTTLError(r *mtproto.Request, peerType string, peerID int64, err error) error {
	switch {
	case errors.Is(err, store.ErrHistoryTTLPeriodInvalid):
		return errTTLPeriodInvalid
	case errors.Is(err, store.ErrHistoryTTLPeerInvalid), errors.Is(err, store.ErrNotMember):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrChatAdminRequired):
		return errChatAdminRequired
	case errors.Is(err, store.ErrYouBlockedUser):
		return errYouBlockedUser
	case errors.Is(err, store.ErrPeerBlockedUser):
		return errUserIsBlocked
	}
	if rateLimited, ok := errors.AsType[*store.RateLimitedError](err); ok {
		h.recordRateLimitDenial("history_ttl")
		return FloodWaitError(int(rateLimited.Wait / time.Second))
	}
	h.log.Error("set history TTL", "user_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "err", err)
	return errInternal
}
