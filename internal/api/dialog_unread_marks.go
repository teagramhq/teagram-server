package api

import (
	"context"
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) registerDialogUnreadMarkMutation(d *mtproto.Dispatcher, id uint32, fn methodFunc) {
	d.HandleFunc(id, func(c *mtproto.Conn, req *mtproto.Request) error {
		if provisionalBlocked(id, req) {
			return c.SendErr(req, errAuthKeyUnreg)
		}
		result, err := fn(req)
		if err != nil {
			var rpc *tgerr.Error
			if !errors.As(err, &rpc) {
				rpc = errInternal
			}
			return c.SendErr(req, rpc)
		}
		return c.SendResult(req, result)
	})
}

func (h *handlers) handleMarkDialogUnread(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesMarkDialogUnreadRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if _, parentPeer := req.GetParentPeer(); parentPeer {
		return nil, errPeerIDInvalid
	}
	peer, ok := req.Peer.(*tg.InputDialogPeer)
	if !ok || peer.Peer == nil {
		return nil, errPeerIDInvalid
	}
	peerType, peerID, err := h.inputPeer(peer.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	_, err = h.store.MarkDialogUnread(r.Ctx, r.UserID, store.PeerDialogKey{
		PeerType: peerType, PeerID: peerID,
	}, req.GetUnread())
	if err != nil {
		if !errors.Is(err, store.ErrDialogUnreadMarkPeerInvalid) && !errors.Is(err, store.ErrDialogUnreadMarkRateLimited) {
			h.log.Error("mark dialog unread", "user_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "err", err)
		}
		return nil, dialogUnreadMarkRPCError(err)
	}
	return &tg.BoolTrue{}, nil
}

func dialogUnreadMarkRPCError(err error) *tgerr.Error {
	switch {
	case errors.Is(err, store.ErrDialogUnreadMarkPeerInvalid):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrDialogUnreadMarkRateLimited):
		return FloodWaitError(60)
	default:
		return errInternal
	}
}

func (h *handlers) attachDialogUnreadMarks(ctx context.Context, ownerID int64, dialogs []tg.DialogClass) error {
	peers := make([]store.PeerDialogKey, 0, len(dialogs))
	for _, raw := range dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || dialog.Peer == nil {
			continue
		}
		if peer, ok := unreadMarkPeerKey(dialog.Peer); ok {
			peers = append(peers, peer)
		}
	}
	marks, err := h.store.DialogUnreadMarksForPeers(ctx, ownerID, peers)
	if err != nil {
		return err
	}
	for _, raw := range dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || dialog.Peer == nil {
			continue
		}
		if peer, ok := unreadMarkPeerKey(dialog.Peer); ok && marks[peer] {
			dialog.SetUnreadMark(true)
		}
	}
	return nil
}

func unreadMarkPeerKey(peer tg.PeerClass) (store.PeerDialogKey, bool) {
	switch p := peer.(type) {
	case *tg.PeerUser:
		return store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: p.UserID}, p.UserID > 0
	case *tg.PeerChat:
		return store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: p.ChatID}, p.ChatID > 0
	case *tg.PeerChannel:
		return store.PeerDialogKey{PeerType: store.PeerTypeChannel, PeerID: p.ChannelID}, p.ChannelID > 0
	default:
		return store.PeerDialogKey{}, false
	}
}
