package api

import (
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleGetSavedReactionTags(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSavedReactionTagsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.MessagesSavedReactionTags{Tags: []tg.SavedReactionTag{}}, nil
}

func (h *handlers) handleGetAttachMenuBots(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAttachMenuBotsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.AttachMenuBots{
		Bots:  []tg.AttachMenuBot{},
		Users: []tg.UserClass{},
	}, nil
}

func (h *handlers) handleGetStickerSet(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetStickerSetRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.MessagesStickerSetNotModified{}, nil
}

func (h *handlers) handleGetAllDrafts(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetAllDraftsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	drafts, err := h.store.CloudDraftsForOwner(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get all drafts", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if len(drafts) == 0 {
		return &tg.Updates{
			Updates: []tg.UpdateClass{},
			Date:    int(time.Now().Unix()),
		}, nil
	}

	userIDs := map[int64]bool{r.UserID: true}
	chatIDs := make(map[int64]bool)
	channelIDs := make(map[int64]bool)
	for _, draft := range drafts {
		switch draft.PeerType {
		case store.PeerTypeUser:
			userIDs[draft.PeerID] = true
		case store.PeerTypeChat:
			chatIDs[draft.PeerID] = true
		case store.PeerTypeChannel:
			channelIDs[draft.PeerID] = true
		}
	}

	users, err := h.loadUsers(r.Ctx, userIDs, r.UserID)
	if err != nil {
		h.log.Error("get all drafts users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	loadedChats, err := h.loadChats(r.Ctx, chatIDs, r.UserID)
	if err != nil {
		h.log.Error("get all drafts chats", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	loadedChannels, err := h.loadChannels(r.Ctx, channelIDs, r.UserID)
	if err != nil {
		h.log.Error("get all drafts channels", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	visiblePeers := make(map[store.PeerDialogKey]bool, len(loadedChats)+len(loadedChannels))
	chats := make([]tg.ChatClass, 0, len(loadedChats)+len(loadedChannels))
	for _, raw := range loadedChats {
		if chat, ok := raw.(*tg.Chat); ok {
			visiblePeers[store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: chat.ID}] = true
			chats = append(chats, chat)
		}
	}
	for _, raw := range loadedChannels {
		if channel, ok := raw.(*tg.Channel); ok {
			visiblePeers[store.PeerDialogKey{PeerType: store.PeerTypeChannel, PeerID: channel.ID}] = true
			chats = append(chats, channel)
		}
	}

	updates := make([]tg.UpdateClass, 0, len(drafts))
	for _, draft := range drafts {
		if draft.PeerType != store.PeerTypeUser && !visiblePeers[store.PeerDialogKey{PeerType: draft.PeerType, PeerID: draft.PeerID}] {
			continue
		}
		updates = append(updates, &tg.UpdateDraftMessage{
			Peer:  peerToTL(draft.PeerType, draft.PeerID),
			Draft: cloudDraftToTL(draft, true, draft.UpdatedAt),
		})
	}
	return &tg.Updates{
		Updates: updates,
		Users:   users,
		Chats:   chats,
		Date:    int(time.Now().Unix()),
	}, nil
}

func (h *handlers) handleReceivedMessages(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesReceivedMessagesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.ReceivedNotifyMessageVector{Elems: []tg.ReceivedNotifyMessage{}}, nil
}

func (h *handlers) handleGetJoinedCommunities(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.CommunitiesGetJoinedCommunitiesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.MessagesChats{Chats: []tg.ChatClass{}}, nil
}
