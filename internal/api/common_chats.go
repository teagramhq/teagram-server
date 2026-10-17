package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleGetCommonChats serves messages.getCommonChats with the caller-visible
// basic groups and supergroups shared with the requested user.
func (h *handlers) handleGetCommonChats(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesGetCommonChatsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	targetID, err := h.inputUserID(req.UserID, r.UserID)
	if err != nil {
		return nil, err
	}
	if targetID == r.UserID {
		return nil, errUserIDInvalid
	}

	// Resolve the same caller-scoped target as users.getFullUser so an unknown
	// target produces its indistinguishable PEER_ID_INVALID error.
	_, found, err := h.store.PeerSettingsForViewer(r.Ctx, r.UserID, store.PeerDialogKey{
		PeerType: store.PeerTypeUser,
		PeerID:   targetID,
	})
	if err != nil {
		h.log.Error("get common chats: resolve target", "user_id", r.UserID, "peer_id", targetID, "err", err)
		return nil, errInternal
	}
	if !found {
		return nil, errPeerIDInvalid
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}
	page, err := h.store.CommonChatsForUsers(r.Ctx, r.UserID, targetID, req.MaxID, int32(limit))
	if err != nil {
		h.log.Error("get common chats", "user_id", r.UserID, "peer_id", targetID, "err", err)
		return nil, errInternal
	}

	chats := make([]tg.ChatClass, 0, len(page.Chats))
	for _, common := range page.Chats {
		switch common.PeerType {
		case store.PeerTypeChat:
			chats = append(chats, chatToTL(common.BasicGroup, int(common.ParticipantCount), r.UserID))
		case store.PeerTypeChannel:
			chats = append(chats, h.channelToTL(common.Supergroup, store.ChannelMember{Role: common.SupergroupRole}, true, r.UserID))
		}
	}
	return &tg.MessagesChatsSlice{Count: int(page.Count), Chats: chats}, nil
}
