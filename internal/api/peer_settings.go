package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleGetPeerSettings(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPeerSettingsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}

	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	snapshot, found, err := h.store.PeerSettingsForViewer(r.Ctx, r.UserID, store.PeerDialogKey{
		PeerType: peerType,
		PeerID:   peerID,
	})
	if err != nil {
		h.log.Error("get peer settings", "peer_type", peerType, "peer_id", peerID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !found {
		return nil, errPeerIDInvalid
	}

	result := &tg.MessagesPeerSettings{
		Settings: tg.PeerSettings{},
		Chats:    []tg.ChatClass{},
		Users:    []tg.UserClass{},
	}
	switch peerType {
	case store.PeerTypeUser:
		if peerID != r.UserID && snapshot.HasDialog {
			result.Settings.SetAddContact(!snapshot.Contact)
			result.Settings.SetBlockContact(!snapshot.Blocked)
		}
		contact := store.Contact{}
		if snapshot.Contact {
			contact.UserID = peerID
		}
		result.Users = append(result.Users, h.userToTL(snapshot.User, r.UserID, peerID == r.UserID, contact))
	case store.PeerTypeChat:
		result.Chats = append(result.Chats, chatToTL(snapshot.Chat, int(snapshot.ChatParticipantCount), r.UserID))
	case store.PeerTypeChannel:
		result.Chats = append(result.Chats, h.channelToTL(snapshot.Channel, snapshot.ChannelMember, true, r.UserID))
	}
	return result, nil
}
