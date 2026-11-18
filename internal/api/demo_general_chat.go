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

func (h *handlers) checkDemoGeneralChatMessages(r *mtproto.Request, channelID int64, texts []string, cost int, tooLong error) error {
	if !h.demoGeneralChatEnabled {
		return nil
	}
	isGeneral, err := h.isDemoGeneralChat(r.Ctx, channelID)
	if err != nil {
		h.log.Error("resolve demo General chat for message policy", "channel_id", channelID, "err", err)
		return errInternal
	}
	if !isGeneral {
		return nil
	}
	for _, text := range texts {
		if textExceedsLimit(text, demoGeneralChatMessageMax) {
			return tooLong
		}
	}
	if err := h.checkDemoGeneralChatRateLimit(r, demoGeneralChatHourSurface, h.demoGeneralChatPolicy.perHour, cost); err != nil {
		return err
	}
	if err := h.checkDemoGeneralChatRateLimit(r, demoGeneralChatMinuteSurface, h.demoGeneralChatPolicy.perMinute, cost); err != nil {
		return err
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		surface := demoGeneralChatDuplicateSurface + hex.EncodeToString(digest[:])
		if err := h.checkDemoGeneralChatRateLimit(r, surface, h.demoGeneralChatPolicy.duplicateLimit, 1); err != nil {
			return err
		}
	}
	return nil
}

func (h *handlers) isDemoGeneralChat(ctx context.Context, channelID int64) (bool, error) {
	if cached := h.demoGeneralChatChannelID.Load(); cached != 0 {
		return cached == channelID, nil
	}
	resolved, found, err := h.store.UsernameByHandle(ctx, demoGeneralChatUsername)
	if err != nil {
		return false, fmt.Errorf("look up demo General chat: %w", err)
	}
	if !found || resolved.Kind != store.UsernameKindChannel || resolved.Channel.ID <= 0 ||
		resolved.Channel.Username == nil || *resolved.Channel.Username != demoGeneralChatUsername ||
		resolved.Channel.Title != "General" || !resolved.Channel.Megagroup {
		return false, nil
	}
	h.demoGeneralChatChannelID.CompareAndSwap(0, resolved.Channel.ID)
	return h.demoGeneralChatChannelID.Load() == channelID, nil
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
