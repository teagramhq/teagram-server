package api

import (
	"context"
	"errors"
	"math"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) notifySettingsForPeer(ctx context.Context, ownerID int64, peerType store.PeerType, peerID int64) (tg.PeerNotifySettings, error) {
	value, _, err := h.store.GetNotifySettings(ctx, ownerID, store.NotifySettingsKey{
		Scope: store.NotifySettingsScopePeer, PeerType: peerType, PeerID: peerID,
	})
	if errors.Is(err, store.ErrNotifySettingsPeerInvalid) {
		return tg.PeerNotifySettings{}, nil
	}
	if err != nil {
		return tg.PeerNotifySettings{}, err
	}
	return *notifySettingsToTL(value), nil
}

func (h *handlers) attachDialogNotifySettings(ctx context.Context, ownerID int64, dialogs []tg.DialogClass) error {
	if len(dialogs) == 0 {
		return nil
	}
	values, err := h.store.NotifySettingsForVisiblePeers(ctx, ownerID)
	if err != nil {
		return err
	}
	for _, raw := range dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		var key store.PeerDialogKey
		switch peer := dialog.Peer.(type) {
		case *tg.PeerUser:
			key = store.PeerDialogKey{PeerType: store.PeerTypeUser, PeerID: peer.UserID}
		case *tg.PeerChat:
			key = store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: peer.ChatID}
		case *tg.PeerChannel:
			key = store.PeerDialogKey{PeerType: store.PeerTypeChannel, PeerID: peer.ChannelID}
		default:
			continue
		}
		if value, found := values[key]; found {
			dialog.NotifySettings = *notifySettingsToTL(value)
		}
	}
	return nil
}

func (h *handlers) handleGetNotifySettings(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.AccountGetNotifySettingsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	key, err := h.notifySettingsKey(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	value, _, err := h.store.GetNotifySettings(r.Ctx, r.UserID, key)
	if err != nil {
		if errors.Is(err, store.ErrNotifySettingsPeerInvalid) {
			return nil, errPeerIDInvalid
		}
		h.log.Error("get notification settings", "user_id", r.UserID, "scope", key.Scope, "peer_type", key.PeerType, "peer_id", key.PeerID, "err", err)
		return nil, errInternal
	}
	return notifySettingsToTLForKey(key, value), nil
}

func (h *handlers) handleGetNotifyExceptions(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.AccountGetNotifyExceptionsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	key, err := h.notifySettingsKey(req.Peer, r.UserID)
	if err != nil || key.Scope == store.NotifySettingsScopePeer {
		return nil, errPeerIDInvalid
	}
	exceptions, err := h.store.NotifySettingsExceptions(r.Ctx, r.UserID, key.Scope)
	if err != nil {
		if errors.Is(err, store.ErrNotifySettingsPeerInvalid) {
			return nil, errPeerIDInvalid
		}
		h.log.Error("get notification settings exceptions", "user_id", r.UserID, "scope", key.Scope, "err", err)
		return nil, errInternal
	}

	updates := make([]tg.UpdateClass, 0, len(exceptions))
	userIDs := make(map[int64]bool)
	chatIDs := make(map[int64]bool)
	channelIDs := make(map[int64]bool)
	for _, exception := range exceptions {
		peer := peerToTL(exception.PeerType, exception.PeerID)
		updates = append(updates, &tg.UpdateNotifySettings{
			Peer:           &tg.NotifyPeer{Peer: peer},
			NotifySettings: *notifySettingsToTL(exception.Value),
		})
		switch exception.PeerType {
		case store.PeerTypeUser:
			userIDs[exception.PeerID] = true
		case store.PeerTypeChat:
			chatIDs[exception.PeerID] = true
		case store.PeerTypeChannel:
			channelIDs[exception.PeerID] = true
		}
	}
	users, err := h.loadUsers(r.Ctx, userIDs, r.UserID)
	if err != nil {
		h.log.Error("get notification settings exception users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	chats, err := h.loadChats(r.Ctx, chatIDs, r.UserID, nil)
	if err != nil {
		h.log.Error("get notification settings exception chats", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	channels, err := h.loadChannels(r.Ctx, channelIDs, r.UserID)
	if err != nil {
		h.log.Error("get notification settings exception channels", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	chats = append(chats, channels...)
	return &tg.Updates{Updates: updates, Users: users, Chats: chats, Date: int(h.now().Unix())}, nil
}

func (h *handlers) handleUpdateNotifySettings(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var req tg.AccountUpdateNotifySettingsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	key, err := h.notifySettingsKey(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	value, err := normalizeNotifySettings(req.Settings)
	if err != nil {
		return nil, errNotifySettingsInvalid
	}
	if _, err = h.store.UpdateNotifySettings(r.Ctx, r.UserID, key, value); err != nil {
		if !errors.Is(err, store.ErrNotifySettingsPeerInvalid) &&
			!errors.Is(err, store.ErrNotifySettingsTooMany) &&
			!errors.Is(err, store.ErrNotifySettingsRateLimited) {
			h.log.Error("update notification settings", "user_id", r.UserID, "scope", key.Scope, "peer_type", key.PeerType, "peer_id", key.PeerID, "err", err)
		}
		return nil, notifySettingsRPCError(err)
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) notifySettingsKey(peer tg.InputNotifyPeerClass, ownerID int64) (store.NotifySettingsKey, error) {
	switch p := peer.(type) {
	case *tg.InputNotifyUsers:
		return store.NotifySettingsKey{Scope: store.NotifySettingsScopeUsers}, nil
	case *tg.InputNotifyChats:
		return store.NotifySettingsKey{Scope: store.NotifySettingsScopeChats}, nil
	case *tg.InputNotifyBroadcasts:
		return store.NotifySettingsKey{Scope: store.NotifySettingsScopeBroadcasts}, nil
	case *tg.InputNotifyPeer:
		peerType, peerID, err := h.inputPeer(p.Peer, ownerID)
		if err != nil {
			return store.NotifySettingsKey{}, errPeerIDInvalid
		}
		return store.NotifySettingsKey{Scope: store.NotifySettingsScopePeer, PeerType: peerType, PeerID: peerID}, nil
	default:
		return store.NotifySettingsKey{}, errPeerIDInvalid
	}
}

func normalizeNotifySettings(input tg.InputPeerNotifySettings) (store.NotifySettingsValue, error) {
	var out store.NotifySettingsValue
	if value, ok := input.GetShowPreviews(); ok {
		out.ShowPreviews = &value
	}
	if value, ok := input.GetSilent(); ok {
		out.Silent = &value
	}
	if value, ok := input.GetMuteUntil(); ok {
		if value < math.MinInt32 || value > math.MaxInt32 {
			return store.NotifySettingsValue{}, errNotifySettingsInvalid
		}
		if value < 0 {
			value = 0
		}
		muteUntil := int32(value)
		out.MuteUntil = &muteUntil
	}
	if sound, ok := input.GetSound(); ok {
		switch sound.(type) {
		case *tg.NotificationSoundDefault:
			value := int16(0)
			out.Sound = &value
		case *tg.NotificationSoundNone:
			value := int16(1)
			out.Sound = &value
		default:
			return store.NotifySettingsValue{}, errNotifySettingsInvalid
		}
	}
	// Stories fields are intentionally accepted and ignored by this settings slice.
	return out, nil
}

func notifySettingsToTL(value store.NotifySettingsValue) *tg.PeerNotifySettings {
	out := &tg.PeerNotifySettings{}
	if value.ShowPreviews != nil {
		out.SetShowPreviews(*value.ShowPreviews)
	}
	if value.Silent != nil {
		out.SetSilent(*value.Silent)
	}
	if value.MuteUntil != nil {
		out.SetMuteUntil(int(*value.MuteUntil))
	}
	if value.Sound != nil {
		var sound tg.NotificationSoundClass
		if *value.Sound == 0 {
			sound = &tg.NotificationSoundDefault{}
		} else {
			sound = &tg.NotificationSoundNone{}
		}
		out.SetIosSound(sound)
		out.SetAndroidSound(sound)
		out.SetOtherSound(sound)
	}
	return out
}

func notifySettingsToTLForKey(key store.NotifySettingsKey, value store.NotifySettingsValue) *tg.PeerNotifySettings {
	if key.Scope != store.NotifySettingsScopePeer && value.MuteUntil == nil {
		// Desktop treats a default scope without mute_until as muted.
		muteUntil := int32(0)
		value.MuteUntil = &muteUntil
	}
	return notifySettingsToTL(value)
}

func notifySettingsRPCError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotifySettingsPeerInvalid):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrNotifySettingsTooMany):
		return errNotifySettingsTooMuch
	case errors.Is(err, store.ErrNotifySettingsRateLimited):
		return FloodWaitError(60)
	case errors.Is(err, store.ErrNotifySettingsSoundInvalid):
		return errNotifySettingsInvalid
	default:
		return errInternal
	}
}

func notifySettingsUpdatePeer(key store.NotifySettingsKey) tg.NotifyPeerClass {
	switch key.Scope {
	case store.NotifySettingsScopeUsers:
		return &tg.NotifyUsers{}
	case store.NotifySettingsScopeChats:
		return &tg.NotifyChats{}
	case store.NotifySettingsScopeBroadcasts:
		return &tg.NotifyBroadcasts{}
	default:
		return &tg.NotifyPeer{Peer: peerToTL(key.PeerType, key.PeerID)}
	}
}
