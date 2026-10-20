package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleDeleteScheduledMessages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesDeleteScheduledMessagesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	if _, _, err := h.resolveScheduledManagementPeer(r, req.Peer); err != nil {
		return nil, err
	}
	return h.emptyScheduledUpdates(), nil
}

func (h *handlers) handleSendScheduledMessages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSendScheduledMessagesRequest
	if err := decodeLaunchGetterRequest(r, &req); err != nil {
		return nil, err
	}
	if _, _, err := h.resolveScheduledManagementPeer(r, req.Peer); err != nil {
		return nil, err
	}
	if len(req.ID) != 0 {
		return nil, errMessageIDInvalid
	}
	return h.emptyScheduledUpdates(), nil
}

func (h *handlers) resolveScheduledManagementPeer(r *mtproto.Request, peer tg.InputPeerClass) (store.PeerType, int64, error) {
	peerType, peerID, err := h.inputPeer(peer, r.UserID)
	if err != nil {
		return 0, 0, err
	}
	switch peerType {
	case store.PeerTypeUser:
		_, found, err := h.store.UserByID(r.Ctx, peerID)
		if err != nil {
			h.log.Error("resolve scheduled-message user peer", "user_id", r.UserID, "peer_id", peerID, "err", err)
			return 0, 0, errInternal
		}
		if !found {
			return 0, 0, errPeerIDInvalid
		}
	case store.PeerTypeChat:
		if err := h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return 0, 0, err
		}
	case store.PeerTypeChannel:
		if _, err := h.requireChannelMember(r.Ctx, peerID, r.UserID); err != nil {
			return 0, 0, err
		}
	}
	return peerType, peerID, nil
}

func (h *handlers) emptyScheduledUpdates() *tg.Updates {
	return &tg.Updates{
		Updates: []tg.UpdateClass{},
		Users:   []tg.UserClass{},
		Chats:   []tg.ChatClass{},
		Date:    int(h.now().Unix()),
	}
}
