package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// These acknowledgements do not persist per-content, mention or reaction read
// state. They authorize the read and leave history and update delivery untouched.
func (h *handlers) handleReadMessageContents(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesReadMessageContentsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if len(req.ID) > maxChannelReadMessageContentsIDs {
		return nil, errLimitInvalid
	}
	ids := make([]int64, len(req.ID))
	for i, id := range req.ID {
		ids[i] = int64(id)
	}
	messages, err := h.store.MessagesByOwnerLocalIDs(r.Ctx, r.UserID, ids)
	if err != nil {
		h.log.Error("read message contents", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	// IDs are in the caller's namespace. Unknown IDs reveal nothing, while a
	// retained group copy still requires current membership.
	for _, message := range messages {
		if message.PeerType == store.PeerTypeChat {
			if err := h.requireMember(r.Ctx, message.PeerID, r.UserID); err != nil {
				return nil, err
			}
		}
	}
	pts, err := h.unreadAcknowledgementPts(r)
	if err != nil {
		return nil, err
	}
	return &tg.MessagesAffectedMessages{Pts: pts}, nil
}

func (h *handlers) handleReadMentions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesReadMentionsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	return h.acknowledgeUnreadPeer(r, req.Peer)
}

func (h *handlers) handleReadReactions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesReadReactionsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	return h.acknowledgeUnreadPeer(r, req.Peer)
}

func (h *handlers) handleGetUnreadMentions(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetUnreadMentionsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if err := h.authorizeUnreadPeer(r, req.Peer); err != nil {
		return nil, err
	}
	return &tg.MessagesMessages{}, nil
}

func (h *handlers) authorizeUnreadPeer(r *mtproto.Request, peer tg.InputPeerClass) error {
	if r.UserID == 0 {
		return errAuthKeyUnreg
	}
	peerType, peerID, err := h.inputPeer(peer, r.UserID)
	if err != nil {
		return err
	}
	if peerType == store.PeerTypeChannel {
		return errPeerIDInvalid
	}
	if peerType == store.PeerTypeChat {
		return h.requireMember(r.Ctx, peerID, r.UserID)
	}
	return nil
}

func (h *handlers) acknowledgeUnreadPeer(r *mtproto.Request, peer tg.InputPeerClass) (bin.Encoder, error) {
	if err := h.authorizeUnreadPeer(r, peer); err != nil {
		return nil, err
	}
	pts, err := h.unreadAcknowledgementPts(r)
	if err != nil {
		return nil, err
	}
	return &tg.MessagesAffectedHistory{Pts: pts}, nil
}

func (h *handlers) unreadAcknowledgementPts(r *mtproto.Request) (int, error) {
	state, err := h.store.StateWithoutUnread(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("unread acknowledgement state", "user_id", r.UserID, "err", err)
		return 0, errInternal
	}
	return state.Pts, nil
}
