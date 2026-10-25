package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func emptySavedDialogs() *tg.MessagesSavedDialogs {
	return &tg.MessagesSavedDialogs{
		Dialogs:  []tg.SavedDialogClass{},
		Messages: []tg.MessageClass{},
		Chats:    []tg.ChatClass{},
		Users:    []tg.UserClass{},
	}
}

func (h *handlers) handleGetSavedDialogs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSavedDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	// Monoforum saved topics are not stored here. The regular Saved Messages
	// list has one entry: the account's own My Notes dialog.
	if req.ParentPeer != nil || req.OffsetID != 0 {
		return emptySavedDialogs(), nil
	}

	history, err := h.store.History(r.Ctx, r.UserID, store.PeerTypeUser, r.UserID, 0, 1)
	if err != nil {
		h.log.Error("get saved dialogs history", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	topMessage := 0
	tlMessages := []tg.MessageClass{}
	if len(history) > 0 {
		files, loadErr := h.loadFiles(r.Ctx, history)
		if loadErr != nil {
			h.log.Error("get saved dialogs files", "user_id", r.UserID, "err", loadErr)
			return nil, errInternal
		}
		pollViews, pollErr := h.pollViewsForMessages(r.Ctx, r.UserID, history)
		if pollErr != nil {
			h.log.Error("get saved dialogs polls", "user_id", r.UserID, "err", pollErr)
			return nil, errInternal
		}
		message := history[0]
		topMessage = int(message.LocalID)
		if poll, ok := pollViews[message.LocalID]; ok {
			tlMessage, renderErr := messageToTLWithPoll(message, nil, files, nil, nil, poll)
			if renderErr != nil {
				h.log.Error("render saved dialog poll", "user_id", r.UserID, "local_id", message.LocalID, "err", renderErr)
				return nil, errInternal
			}
			tlMessages = append(tlMessages, tlMessage)
		} else {
			tlMessages = append(tlMessages, h.messageToTL(message, nil, files, nil, nil, h.dcID))
		}
	}

	users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
	if err != nil {
		h.log.Error("get saved dialogs users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return &tg.MessagesSavedDialogs{
		Dialogs: []tg.SavedDialogClass{&tg.SavedDialog{
			Peer:       &tg.PeerUser{UserID: r.UserID},
			TopMessage: topMessage,
		}},
		Messages: tlMessages,
		Chats:    []tg.ChatClass{},
		Users:    users,
	}, nil
}

func (h *handlers) handleGetPinnedSavedDialogs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPinnedSavedDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return emptySavedDialogs(), nil
}
