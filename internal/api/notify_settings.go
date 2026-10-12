package api

import (
	"errors"
	"math"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

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
	return notifySettingsToTL(value), nil
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
