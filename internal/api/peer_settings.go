package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// userPeerSettingsBar is the peer-settings bar messages.getPeerSettings and
// users.getFullUser must agree on for the same viewer, peer and state: the
// add-contact and block affordances exist only for a 1:1 peer that is not the
// caller and that the caller has a dialog with. Sharing the builder is what keeps
// the profile bar from flipping between the two RPCs.
func userPeerSettingsBar(snapshot store.PeerSettingsSnapshot, viewerID, targetID int64) tg.PeerSettings {
	settings := tg.PeerSettings{}
	if targetID != viewerID && snapshot.HasDialog {
		settings.SetAddContact(!snapshot.Contact)
		settings.SetBlockContact(!snapshot.Blocked)
	}
	return settings
}

// viewerContactEdge turns a snapshot's caller-owned contact flag into the edge
// userToTL expects. The peer's own edge is never read, so a peer cannot make the
// caller's view look mutual.
func viewerContactEdge(snapshot store.PeerSettingsSnapshot, targetID int64) store.Contact {
	if !snapshot.Contact {
		return store.Contact{}
	}
	return store.Contact{UserID: targetID}
}

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
		result.Settings = userPeerSettingsBar(snapshot, r.UserID, peerID)
		result.Users = append(result.Users, h.userToTL(snapshot.User, r.UserID, peerID == r.UserID, viewerContactEdge(snapshot, peerID)))
	case store.PeerTypeChat:
		result.Chats = append(result.Chats, chatToTL(snapshot.Chat, int(snapshot.ChatParticipantCount), r.UserID))
	case store.PeerTypeChannel:
		result.Chats = append(result.Chats, h.channelToTL(snapshot.Channel, snapshot.ChannelMember, true, r.UserID))
	}
	return result, nil
}
