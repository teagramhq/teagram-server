package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	demoGeneralChatUsername         = "teagramdemogeneral"
	demoGeneralChatMessageMax       = 1000
	demoGeneralChatDuplicateWindow  = 30 * time.Second
	demoGeneralChatResolutionTTL    = time.Minute
	demoGeneralChatLookupFailureTTL = 5 * time.Second
	demoGeneralChatLookupTimeout    = 3 * time.Second
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

type demoGeneralChatCache struct {
	channelID int64
	expiresAt time.Time
	err       error
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
	if err := h.preflightDemoGeneralChatRateLimits(r, texts, cost); err != nil {
		return false, err
	}
	return true, nil
}

func (h *handlers) demoGeneralChatRateLimitCharges(r *mtproto.Request, texts []string, cost int) []store.RateLimitCharge {
	if r.Tailnet() {
		return nil
	}
	charges := []store.RateLimitCharge{
		{Surface: demoGeneralChatHourSurface, Config: h.demoGeneralChatPolicy.perHour, Cost: cost},
		{Surface: demoGeneralChatMinuteSurface, Config: h.demoGeneralChatPolicy.perMinute, Cost: cost},
	}
	for _, text := range texts {
		if text == "" {
			continue
		}
		surface := demoGeneralChatDuplicateRateLimitSurface(text)
		charges = append(charges, store.RateLimitCharge{
			Surface: surface, Config: h.demoGeneralChatPolicy.duplicateLimit, Cost: 1,
		})
	}
	return charges
}

func (h *handlers) preflightDemoGeneralChatRateLimits(r *mtproto.Request, texts []string, cost int) error {
	if r.Tailnet() {
		return nil
	}
	for _, limit := range []struct {
		config store.RateLimitConfig
		cost   int
	}{
		{config: h.demoGeneralChatPolicy.perHour, cost: cost},
		{config: h.demoGeneralChatPolicy.perMinute, cost: cost},
	} {
		if err := staticDemoGeneralChatRateLimitError(limit.config, limit.cost); err != nil {
			return err
		}
	}
	if !h.demoGeneralChatPolicy.duplicateLimit.Enabled() {
		return nil
	}
	counts := make(map[string]int)
	for _, text := range texts {
		if text == "" {
			continue
		}
		surface := demoGeneralChatDuplicateRateLimitSurface(text)
		counts[surface]++
		if err := staticDemoGeneralChatRateLimitError(h.demoGeneralChatPolicy.duplicateLimit, counts[surface]); err != nil {
			return err
		}
	}
	return nil
}

func staticDemoGeneralChatRateLimitError(config store.RateLimitConfig, cost int) error {
	if !config.Enabled() || cost <= config.Limit {
		return nil
	}
	wait := config.Window / time.Second
	if config.Window%time.Second != 0 {
		wait++
	}
	if wait < 1 {
		wait = 1
	}
	return FloodWaitError(int(wait))
}

func demoGeneralChatDuplicateRateLimitSurface(text string) string {
	digest := sha256.Sum256([]byte(text))
	return demoGeneralChatDuplicateSurface + hex.EncodeToString(digest[:])
}

func (h *handlers) isDemoGeneralChat(ctx context.Context, channelID int64) (bool, error) {
	now := h.demoGeneralChatNow()
	if cached := h.demoGeneralChatTarget.Load(); cached != nil && now.Before(cached.expiresAt) {
		if cached.err != nil {
			return false, cached.err
		}
		return cached.channelID == channelID, nil
	}

	h.demoGeneralChatCacheMu.Lock()
	defer h.demoGeneralChatCacheMu.Unlock()

	now = h.demoGeneralChatNow()
	if cached := h.demoGeneralChatTarget.Load(); cached != nil && now.Before(cached.expiresAt) {
		if cached.err != nil {
			return false, cached.err
		}
		return cached.channelID == channelID, nil
	}

	lookupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), demoGeneralChatLookupTimeout)
	defer cancel()
	resolved, found, err := h.lookupDemoGeneralChat(lookupCtx)
	if err != nil {
		lookupErr := fmt.Errorf("look up demo General chat: %w", err)
		h.demoGeneralChatTarget.Store(&demoGeneralChatCache{
			expiresAt: h.demoGeneralChatNow().Add(demoGeneralChatLookupFailureTTL),
			err:       lookupErr,
		})
		return false, lookupErr
	}
	var resolvedChannelID int64
	if !found || resolved.Kind != store.UsernameKindChannel || resolved.Channel.ID <= 0 ||
		resolved.Channel.Username == nil || *resolved.Channel.Username != demoGeneralChatUsername ||
		resolved.Channel.Title != "General" || !resolved.Channel.Megagroup {
		h.logDemoGeneralChatResolutionError("missing or invalid public supergroup", channelID)
	} else {
		resolvedChannelID = resolved.Channel.ID
	}
	h.demoGeneralChatTarget.Store(&demoGeneralChatCache{
		channelID: resolvedChannelID,
		expiresAt: now.Add(demoGeneralChatResolutionTTL),
	})
	return resolvedChannelID == channelID, nil
}

func (h *handlers) lookupDemoGeneralChat(ctx context.Context) (store.UsernameResolution, bool, error) {
	if h.demoGeneralChatLookup != nil {
		return h.demoGeneralChatLookup(ctx, demoGeneralChatUsername)
	}
	return h.store.UsernameByHandle(ctx, demoGeneralChatUsername)
}

func (h *handlers) demoGeneralChatNow() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *handlers) logDemoGeneralChatResolutionError(reason string, channelID int64) {
	if h.demoGeneralChatWarning.CompareAndSwap(false, true) {
		h.log.Error("demo General chat configuration invalid", "handle", demoGeneralChatUsername, "channel_id", channelID, "reason", reason)
	}
}

func (h *handlers) demoGeneralChatRateLimitError(r *mtproto.Request, err error) (error, bool) {
	if limited, ok := errors.AsType[*store.RateLimitedError](err); ok {
		h.recordRateLimitDenial("message_send")
		return FloodWaitError(int(limited.Wait / time.Second)), true
	}
	if errors.Is(err, store.ErrRateLimitStorage) {
		h.log.Error("demo General chat rate limit write", "user_id", r.UserID, "err", err)
		return errInternal, true
	}
	return nil, false
}
