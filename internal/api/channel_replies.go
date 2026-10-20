package api

import (
	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleGetReplies serves messages.getReplies for direct replies in a
// supergroup. The store returns membership, root validity, count and page from
// one database statement so a stale access hash cannot preserve read access.
func (h *handlers) handleGetReplies(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetRepliesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	peerType, channelID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	if peerType != store.PeerTypeChannel {
		return nil, errPeerIDInvalid
	}

	limit := req.Limit
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}

	snapshot, err := h.store.ChannelRepliesWithOffset(
		r.Ctx,
		channelID,
		r.UserID,
		int64(req.MsgID),
		int64(req.OffsetID),
		int64(req.AddOffset),
		int64(req.MaxID),
		int64(req.MinID),
		limit,
	)
	if err != nil {
		h.log.Error("get channel replies", "user_id", r.UserID, "channel_id", channelID, "msg_id", req.MsgID, "err", err)
		return nil, errInternal
	}
	if !snapshot.ChannelExists {
		return nil, errPeerIDInvalid
	}
	if !snapshot.Megagroup {
		return nil, errMsgIDInvalid
	}
	if !snapshot.IsMember {
		return nil, errPeerIDInvalid
	}
	if !snapshot.RootExists {
		return nil, errMsgIDInvalid
	}
	return h.channelRepliesWithCount(r, channelID, snapshot.Messages, snapshot.Count)
}
