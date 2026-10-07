package api

import (
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	maxDialogPinRawOrder = 100
)

func (h *handlers) registerDialogPinMutation(d *mtproto.Dispatcher, id uint32, fn methodFunc) {
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

func (h *handlers) handleGetPinnedDialogs(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesGetPinnedDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if req.FolderID == 1 {
		snapshot, err := h.store.PeerDialogsSnapshot(r.Ctx, r.UserID, nil)
		if err != nil {
			h.log.Error("get archived pinned dialogs", "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
		return &tg.MessagesPeerDialogs{State: *stateToTL(snapshot.State)}, nil
	}
	if req.FolderID != 0 {
		return nil, errFolderIDInvalid
	}
	snapshot, err := h.store.DialogPinsPeerSnapshot(r.Ctx, r.UserID, h.now())
	if err != nil {
		h.log.Error("get pinned dialogs", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	result, err := h.peerDialogsToTL(r.Ctx, snapshot.PeerDialogs, r.UserID)
	if err != nil {
		h.log.Error("render pinned dialogs", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	for _, raw := range result.Dialogs {
		if dialog, ok := raw.(*tg.Dialog); ok {
			dialog.SetPinned(true)
			dialog.SetFlags()
		}
	}
	return result, nil
}

func (h *handlers) handleToggleDialogPin(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesToggleDialogPinRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	peer, err := h.inputDialogPinPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	if err := h.checkDialogFilterRateLimit(r); err != nil {
		return nil, err
	}
	if _, err := h.store.ToggleDialogPin(r.Ctx, r.UserID, peer, req.Pinned, h.now()); err != nil {
		if !errors.Is(err, store.ErrDialogPinPeerInvalid) && !errors.Is(err, store.ErrDialogPinsTooMuch) {
			h.log.Error("toggle dialog pin", "user_id", r.UserID, "err", err)
		}
		return nil, dialogPinRPCError(err)
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) handleReorderPinnedDialogs(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.MessagesReorderPinnedDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if req.FolderID != 0 {
		return nil, errFolderIDInvalid
	}
	if len(req.Order) > maxDialogPinRawOrder {
		return nil, errInputRequestInvalid
	}
	order := make([]store.DialogPinPeer, 0, len(req.Order))
	seen := make(map[store.DialogPinPeer]bool, len(req.Order))
	for _, raw := range req.Order {
		peer, err := h.inputDialogPinPeer(raw, r.UserID)
		if err != nil {
			return nil, err
		}
		if seen[peer] {
			continue
		}
		seen[peer] = true
		order = append(order, peer)
	}
	if err := h.checkDialogFilterRateLimit(r); err != nil {
		return nil, err
	}
	if _, err := h.store.ReorderDialogPins(r.Ctx, r.UserID, order, req.Force, h.now()); err != nil {
		if !errors.Is(err, store.ErrDialogPinPeerInvalid) && !errors.Is(err, store.ErrDialogPinsTooMuch) && !errors.Is(err, store.ErrDialogPinOrderTooLong) {
			h.log.Error("reorder dialog pins", "user_id", r.UserID, "err", err)
		}
		return nil, dialogPinRPCError(err)
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) inputDialogPinPeer(raw tg.InputDialogPeerClass, ownerID int64) (store.DialogPinPeer, error) {
	peer, ok := raw.(*tg.InputDialogPeer)
	if !ok || peer.Peer == nil {
		return store.DialogPinPeer{}, errPeerIDInvalid
	}
	peerType, peerID, err := h.inputPeer(peer.Peer, ownerID)
	if err != nil {
		return store.DialogPinPeer{}, err
	}
	return store.DialogPinPeer{PeerType: peerType, PeerID: peerID}, nil
}

func dialogPinRPCError(err error) *tgerr.Error {
	switch {
	case errors.Is(err, store.ErrDialogPinPeerInvalid):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrDialogPinsTooMuch):
		return errPinnedDialogsTooMuch
	case errors.Is(err, store.ErrDialogPinOrderTooLong):
		return errInputRequestInvalid
	default:
		return errInternal
	}
}
