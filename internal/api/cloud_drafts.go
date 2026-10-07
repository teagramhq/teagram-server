package api

import (
	"context"
	"errors"
	"math"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const maxCloudDraftTextRunes = 4096

// cloudDraftToTL builds the only draft shape this service persists. Entity and
// quote data, webpage media, and unsupported rich features are not retained.
func cloudDraftToTL(draft store.CloudDraft, hasDraft bool, date time.Time) tg.DraftMessageClass {
	if !hasDraft {
		empty := &tg.DraftMessageEmpty{}
		empty.SetDate(int(date.Unix()))
		return empty
	}
	out := &tg.DraftMessage{Message: draft.Message, Date: int(draft.UpdatedAt.Unix())}
	out.SetNoWebpage(draft.NoWebpage)
	if draft.ReplyToMsgID > 0 {
		out.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(draft.ReplyToMsgID)})
	}
	out.SetFlags()
	return out
}

func (h *handlers) attachDialogDrafts(ctx context.Context, ownerID int64, dialogs []tg.DialogClass) error {
	peers := make([]store.PeerDialogKey, 0, len(dialogs))
	for _, raw := range dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || dialog.Peer == nil {
			continue
		}
		var peer store.PeerDialogKey
		switch value := dialog.Peer.(type) {
		case *tg.PeerUser:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: value.UserID}
		case *tg.PeerChat:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: value.ChatID}
		case *tg.PeerChannel:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeChannel, PeerID: value.ChannelID}
		default:
			continue
		}
		peers = append(peers, peer)
	}
	drafts, err := h.store.CloudDraftsForPeers(ctx, ownerID, peers)
	if err != nil {
		return err
	}
	for _, raw := range dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || dialog.Peer == nil {
			continue
		}
		var peer store.PeerDialogKey
		switch value := dialog.Peer.(type) {
		case *tg.PeerUser:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: value.UserID}
		case *tg.PeerChat:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: value.ChatID}
		case *tg.PeerChannel:
			peer = store.PeerDialogKey{PeerType: store.PeerTypeChannel, PeerID: value.ChannelID}
		default:
			continue
		}
		if draft, ok := drafts[peer]; ok {
			dialog.SetDraft(cloudDraftToTL(draft, true, draft.UpdatedAt))
		}
	}
	return nil
}

func (h *handlers) handleSaveDraftAfterReplyOnConn(_ *mtproto.Conn, r *mtproto.Request) (bin.Encoder, *replyUpdate, func(), error) {
	var req tg.MessagesSaveDraftRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, nil, errAuthKeyUnreg
	}
	if _, ok := req.GetEffect(); ok {
		return nil, nil, nil, errInputRequestInvalid
	}
	if _, ok := req.GetSuggestedPost(); ok {
		return nil, nil, nil, errInputRequestInvalid
	}
	if _, ok := req.GetRichMessage(); ok {
		return nil, nil, nil, errInputRequestInvalid
	}
	if req.GetInvertMedia() {
		return nil, nil, nil, errInputRequestInvalid
	}
	if !utf8.ValidString(req.Message) || utf8.RuneCountInString(req.Message) > maxCloudDraftTextRunes {
		return nil, nil, nil, errMessageTooLong
	}
	if !validText(req.Message) {
		return nil, nil, nil, errMessageEmpty
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, nil, nil, err
	}

	var replyToMsgID int64
	if reply, ok := req.GetReplyTo(); ok {
		message, ok := reply.(*tg.InputReplyToMessage)
		if !ok || message.ReplyToMsgID <= 0 || int64(message.ReplyToMsgID) > math.MaxInt32 {
			return nil, nil, nil, errMessageIDInvalid
		}
		if _, exists := message.GetTopMsgID(); exists {
			return nil, nil, nil, errMessageIDInvalid
		}
		if _, exists := message.GetMonoforumPeerID(); exists {
			return nil, nil, nil, errMessageIDInvalid
		}
		if _, exists := message.GetTodoItemID(); exists {
			return nil, nil, nil, errMessageIDInvalid
		}
		if _, exists := message.GetPollOption(); exists {
			return nil, nil, nil, errMessageIDInvalid
		}
		if target, exists := message.GetReplyToPeerID(); exists {
			targetType, targetID, targetErr := h.inputPeer(target, r.UserID)
			if targetErr != nil || targetType != peerType || targetID != peerID {
				return nil, nil, nil, errMessageIDInvalid
			}
		}
		replyToMsgID = int64(message.ReplyToMsgID)
	}
	if media, ok := req.GetMedia(); ok {
		if _, supported := media.(*tg.InputMediaWebPage); !supported {
			return nil, nil, nil, errMediaInvalid
		}
	}

	changed, _, err := h.store.SaveCloudDraft(r.Ctx, r.UserID, peerType, peerID, req.Message, req.NoWebpage, replyToMsgID)
	switch {
	case errors.Is(err, store.ErrNotMember):
		return nil, nil, nil, errPeerIDInvalid
	case errors.Is(err, store.ErrMessageInvalid):
		return nil, nil, nil, errMessageIDInvalid
	case errors.Is(err, store.ErrCloudDraftRateLimited):
		h.recordRateLimitDenial("messages_save_draft")
		return nil, nil, nil, FloodWaitError(60)
	case err != nil:
		h.log.Error("save cloud draft", "user_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "err", err)
		return nil, nil, nil, errInternal
	}
	peer := store.PeerDialogKey{PeerType: peerType, PeerID: peerID}
	afterReply := func() {
		if !changed {
			return
		}
		ctx, cancel := senderNotifyContext(r.Ctx)
		defer cancel()
		if notifyErr := h.store.Notify(ctx, store.ChannelUpdates, store.CloudDraftNotificationPayload(r.UserID, peer)); notifyErr != nil {
			h.log.Error("notify cloud draft", "user_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "err", notifyErr)
		}
	}
	return &tg.BoolTrue{}, nil, afterReply, nil
}
