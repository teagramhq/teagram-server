package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	demoGeneralChatUsername         = "teagramdemogeneral"
	demoGeneralChatMessageMax       = 1000
	demoGeneralChatDuplicateWindow  = 30 * time.Second
	demoGeneralChatPerMinuteLimit   = 5
	demoGeneralChatPerHourLimit     = 30
	demoGeneralChatMinuteSurface    = "demo_general_message_minute"
	demoGeneralChatHourSurface      = "demo_general_message_hour"
	demoGeneralChatDuplicateSurface = "demo_general_duplicate_"
)

type demoGeneralChatPolicy struct {
	perMinute      store.RateLimitConfig
	perHour        store.RateLimitConfig
	duplicateLimit store.RateLimitConfig
}

func defaultDemoGeneralChatPolicy() demoGeneralChatPolicy {
	return demoGeneralChatPolicy{
		perMinute:      store.RateLimitConfig{Limit: demoGeneralChatPerMinuteLimit, Window: time.Minute},
		perHour:        store.RateLimitConfig{Limit: demoGeneralChatPerHourLimit, Window: time.Hour},
		duplicateLimit: store.RateLimitConfig{Limit: 1, Window: demoGeneralChatDuplicateWindow},
	}
}

func (h *handlers) checkDemoGeneralChatMessages(r *mtproto.Request, channelID int64, texts []string, cost int, tooLong error) (bool, error) {
	if !h.demoGeneralChatEnabled {
		return false, nil
	}
	isGeneral, err := h.isDemoGeneralChat(r.Ctx, channelID)
	if err != nil {
		h.log.Error("resolve demo General chat for message policy", "channel_id", channelID, "err", err)
		return false, errInternal
	}
	if !isGeneral {
		return false, nil
	}
	for _, text := range texts {
		if textExceedsLimit(text, demoGeneralChatMessageMax) {
			return false, tooLong
		}
	}
	if err := h.checkDemoGeneralChatRateLimit(r, demoGeneralChatHourSurface, h.demoGeneralChatPolicy.perHour, cost); err != nil {
		return false, err
	}
	if err := h.checkDemoGeneralChatRateLimit(r, demoGeneralChatMinuteSurface, h.demoGeneralChatPolicy.perMinute, cost); err != nil {
		return false, err
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		surface := demoGeneralChatDuplicateSurface + hex.EncodeToString(digest[:])
		if err := h.checkDemoGeneralChatRateLimit(r, surface, h.demoGeneralChatPolicy.duplicateLimit, 1); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (h *handlers) isDemoGeneralChat(ctx context.Context, channelID int64) (bool, error) {
	cached := h.demoGeneralChatChannelID.Load()
	resolved, found, err := h.store.UsernameByHandle(ctx, demoGeneralChatUsername)
	if err != nil {
		return false, fmt.Errorf("look up demo General chat: %w", err)
	}
	if !found || resolved.Kind != store.UsernameKindChannel || resolved.Channel.ID <= 0 ||
		resolved.Channel.Username == nil || *resolved.Channel.Username != demoGeneralChatUsername ||
		resolved.Channel.Title != "General" || !resolved.Channel.Megagroup {
		h.demoGeneralChatChannelID.CompareAndSwap(cached, 0)
		h.logDemoGeneralChatResolutionError("missing or invalid public supergroup", channelID)
		return false, nil
	}
	h.demoGeneralChatChannelID.CompareAndSwap(cached, resolved.Channel.ID)
	return h.demoGeneralChatChannelID.Load() == channelID, nil
}

func (h *handlers) logDemoGeneralChatResolutionError(reason string, channelID int64) {
	if h.demoGeneralChatWarning.CompareAndSwap(false, true) {
		h.log.Error("demo General chat configuration invalid", "handle", demoGeneralChatUsername, "channel_id", channelID, "reason", reason)
	}
}

func (h *handlers) checkDemoGeneralChatRateLimit(r *mtproto.Request, surface string, cfg store.RateLimitConfig, cost int) error {
	if r.Tailnet() || !cfg.Enabled() {
		return nil
	}
	result, err := h.store.CheckRateLimitCost(r.Ctx, r.UserID, surface, cfg, cost)
	if err != nil {
		h.log.Error("demo General chat rate limit check", "user_id", r.UserID, "surface", surface, "err", err)
		return errInternal
	}
	if result != nil {
		h.recordRateLimitDenial("message_send")
		return FloodWaitError(int(result.Wait / time.Second))
	}
	return nil
}
