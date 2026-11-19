package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/teagramhq/teagram-server/internal/demo"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	demoGeneralChatUsername         = demo.GeneralChatHandle
	demoGeneralChatMessageMax       = 1000
	demoGeneralChatDuplicateWindow  = 30 * time.Second
	demoGeneralChatResolutionTTL    = time.Minute
	demoGeneralChatLookupFailureTTL = 5 * time.Second
	demoGeneralChatLookupTimeout    = 3 * time.Second
	demoGeneralChatPerMinuteLimit   = 5
	demoGeneralChatPerHourLimit     = 30
	demoGeneralChatCleanupTimeout   = 5 * time.Second
	demoGeneralChatMinuteSurface    = "demo_general_message_minute"
	demoGeneralChatHourSurface      = "demo_general_message_hour"
	demoGeneralChatDuplicateSurface = "demo_general_duplicate_"
)

func (h *handlers) checkDemoGeneralHandleClaim(ctx context.Context, userID int64, username string) error {
	if !h.demoGeneralChatEnabled {
		return nil
	}
	username = strings.ToLower(username)
	if username != demo.SeedOwnerUsername && !demo.IsGeneralChatHandle(username) {
		return nil
	}
	user, ok, err := h.store.UserByID(ctx, userID)
	if err != nil {
		h.log.Error("check demo General handle reservation", "user_id", userID, "err", err)
		return errInternal
	}
	if !ok || (user.Phone != demo.SeedOwnerUsername && (user.Username == nil || *user.Username != demo.SeedOwnerUsername)) {
		return errUsernameInvalid
	}
	return nil
}

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

func (h *handlers) checkDemoGeneralChatMessages(
	r *mtproto.Request, channelID int64, texts []string, cost int, tooLong error, mediaIdentities ...string,
) (bool, error) {
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
	if err := h.preflightDemoGeneralChatRateLimits(r, texts, cost, mediaIdentities...); err != nil {
		return false, err
	}
	return true, nil
}

func (h *handlers) demoGeneralChatRateLimitCharges(
	r *mtproto.Request, texts []string, cost int, mediaIdentities ...string,
) []store.RateLimitCharge {
	if r.Tailnet() {
		return nil
	}
	charges := []store.RateLimitCharge{
		{Surface: demoGeneralChatHourSurface, Config: h.demoGeneralChatPolicy.perHour, Cost: cost},
		{Surface: demoGeneralChatMinuteSurface, Config: h.demoGeneralChatPolicy.perMinute, Cost: cost},
	}
	for _, surface := range demoGeneralChatDuplicateSurfaces(texts, mediaIdentities) {
		charges = append(charges, store.RateLimitCharge{
			Surface: surface, Config: h.demoGeneralChatPolicy.duplicateLimit, Cost: 1,
		})
	}
	return charges
}

func (h *handlers) preflightDemoGeneralChatRateLimits(
	r *mtproto.Request, texts []string, cost int, mediaIdentities ...string,
) error {
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
	for _, surface := range demoGeneralChatDuplicateSurfaces(texts, mediaIdentities) {
		counts[surface]++
		if err := staticDemoGeneralChatRateLimitError(h.demoGeneralChatPolicy.duplicateLimit, counts[surface]); err != nil {
			return err
		}
	}
	return nil
}

func (h *handlers) checkDemoGeneralChatRateLimitBudgets(
	r *mtproto.Request, texts []string, cost int, mediaIdentities ...string,
) error {
	if r.Tailnet() {
		return nil
	}
	for _, limit := range []struct {
		surface string
		config  store.RateLimitConfig
		cost    int
	}{
		{surface: demoGeneralChatHourSurface, config: h.demoGeneralChatPolicy.perHour, cost: cost},
		{surface: demoGeneralChatMinuteSurface, config: h.demoGeneralChatPolicy.perMinute, cost: cost},
	} {
		if limit.cost <= 0 {
			continue
		}
		if err := h.checkDemoGeneralChatRateLimitBudget(r, limit.surface, limit.config, limit.cost); err != nil {
			return err
		}
	}
	counts := make(map[string]int)
	for _, surface := range demoGeneralChatDuplicateSurfaces(texts, mediaIdentities) {
		counts[surface]++
		if err := h.checkDemoGeneralChatRateLimitBudget(r, surface, h.demoGeneralChatPolicy.duplicateLimit, counts[surface]); err != nil {
			return err
		}
	}
	return nil
}

func (h *handlers) checkDemoGeneralChatRateLimitBudget(
	r *mtproto.Request, surface string, config store.RateLimitConfig, cost int,
) error {
	if !config.Enabled() {
		return nil
	}
	result, err := h.store.CheckRateLimitBudgetCost(r.Ctx, r.UserID, surface, config, cost)
	if err != nil {
		h.log.Error("demo General chat rate limit preflight", "user_id", r.UserID, "surface", surface, "err", err)
		return errInternal
	}
	if result == nil {
		return nil
	}
	h.recordRateLimitDenial("message_send")
	return FloodWaitError(int(result.Wait / time.Second))
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

func demoGeneralChatDuplicateSurfaces(texts, mediaIdentities []string) []string {
	var surfaces []string
	for _, text := range texts {
		if text != "" {
			surfaces = append(surfaces, demoGeneralChatDuplicateRateLimitSurface(text))
		}
	}
	for _, identity := range mediaIdentities {
		if identity != "" {
			surfaces = append(surfaces, demoGeneralChatDuplicateRateLimitSurface("media:"+identity))
		}
	}
	return surfaces
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
		resolved.Channel.Username == nil || !demo.IsGeneralChatHandle(*resolved.Channel.Username) ||
		resolved.Channel.Title != "General" || !resolved.Channel.Megagroup || resolved.User.ID <= 0 ||
		resolved.Channel.CreatorID != resolved.User.ID {
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
	owner, ownerFound, err := h.store.UsernameByHandle(ctx, demo.SeedOwnerUsername)
	if err != nil {
		return store.UsernameResolution{}, false, err
	}
	if !ownerFound || owner.Kind != store.UsernameKindUser || owner.User.ID <= 0 {
		return store.UsernameResolution{}, false, nil
	}
	for _, handle := range demo.GeneralChatHandles() {
		resolved, found, err := h.store.UsernameByHandle(ctx, handle)
		if err != nil {
			return store.UsernameResolution{}, false, err
		}
		if found && resolved.Kind == store.UsernameKindChannel && resolved.Channel.CreatorID == owner.User.ID &&
			resolved.Channel.Username != nil && *resolved.Channel.Username == handle &&
			resolved.Channel.Title == "General" && resolved.Channel.Megagroup {
			resolved.User = owner.User
			return resolved, true, nil
		}
	}
	return store.UsernameResolution{}, false, nil
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
