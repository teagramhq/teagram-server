package api

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// authorizationTTLDays is the default inactivity lifetime for existing and
// newly created accounts.
const authorizationTTLDays = 365

// handleGetAuthorizations serves account.getAuthorizations. It lists the auth
// keys bound to the caller's user as sessions. An unbound key (UserID 0) is
// reported as unregistered so the client starts the auth flow. The session
// matching the request's own auth key is flagged Current.
func (h *handlers) handleGetAuthorizations(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetAuthorizationsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	ttlDays, err := h.store.AuthorizationTTLDays(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get authorizations: read TTL", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	keys, err := h.store.AuthKeysByUser(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get authorizations: list keys", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	current := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	showIP := h.store.SessionIPEnabled()
	showDeviceLabels := h.store.SessionDeviceLabelsEnabled()
	auths := make([]tg.Authorization, len(keys))
	for i, k := range keys {
		auths[i] = tg.Authorization{
			Current:     k.ID == current,
			Hash:        k.ID,
			DateCreated: int(k.CreatedAt.Unix()),
			DateActive:  int(k.LastSeenAt.Unix()),
		}
		if showIP {
			auths[i].IP = k.IP
		}
		if showDeviceLabels {
			auths[i].DeviceModel = k.DeviceModel
			auths[i].Platform = k.Platform
			auths[i].SystemVersion = k.SystemVersion
			auths[i].APIID = k.APIID
			auths[i].AppVersion = k.AppVersion
		}
	}
	return &tg.AccountAuthorizations{
		AuthorizationTTLDays: ttlDays,
		Authorizations:       auths,
	}, nil
}

// handleSetAuthorizationTTL stores the caller's inactivity lifetime. The
// store refreshes the calling key in the same transaction so lowering the TTL
// never makes the current session eligible for the next sweep.
func (h *handlers) handleSetAuthorizationTTL(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountSetAuthorizationTTLRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if !validAuthorizationTTLDays(req.AuthorizationTTLDays) {
		return nil, errAuthorizationTTLInvalid
	}
	callerKeyID := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	err := h.store.SetAuthorizationTTL(r.Ctx, r.UserID, callerKeyID, req.AuthorizationTTLDays)
	if errors.Is(err, store.ErrAuthKeyUnauthorized) {
		return nil, errAuthKeyUnreg
	}
	if err != nil {
		h.log.Error("set authorization TTL", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return &tg.BoolTrue{}, nil
}

func validAuthorizationTTLDays(days int) bool {
	switch days {
	case 7, 30, 90, 180, authorizationTTLDays:
		return true
	default:
		return false
	}
}

// handleResetAuthorization serves account.resetAuthorization. It revokes the
// caller's session identified by Hash (an auth key id) by unbinding that key.
// The key is unbound only when it belongs to the caller, so a user cannot reset
// another user's session; a foreign, unbound, or unknown hash returns HASH_INVALID.
// Resetting another session publishes the eviction before the reply, so a client
// that has seen success cannot trigger an update that overtakes it. Only a caller
// resetting its own current session defers, since there the eviction closes the
// socket the reply goes out on.
func (h *handlers) handleResetAuthorization(r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.AccountResetAuthorizationRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	revoked, err := h.store.ResetAuthorization(r.Ctx, r.UserID, req.Hash)
	if err != nil {
		h.log.Error("reset authorization: unbind key", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	if !revoked {
		return nil, nil, errHashInvalid
	}
	// Emitted after the unbind has committed, so the evicted client cannot
	// reconnect on the same key and resolve back to its former account.
	evict := func() { h.notifyEvict(r.Ctx, r.UserID, req.Hash) }
	if selfRevocation(r, req.Hash) {
		return &tg.BoolTrue{}, evict, nil
	}
	evict()
	return &tg.BoolTrue{}, nil, nil
}

// handleResetAuthorizations serves auth.resetAuthorizations. It unbinds every
// other bound or password-pending key for the caller's owner, then publishes an
// exact-key eviction for each formerly bound session before returning success.
func (h *handlers) handleResetAuthorizations(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AuthResetAuthorizationsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}

	callerKeyID := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	removed, err := h.store.ResetAuthorizations(r.Ctx, r.UserID, callerKeyID)
	if errors.Is(err, store.ErrAuthKeyUnauthorized) {
		return nil, errAuthKeyUnreg
	}
	if err != nil {
		h.log.Error("reset authorizations", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	for _, authKeyID := range removed {
		h.notifyEvict(r.Ctx, r.UserID, authKeyID)
	}
	return &tg.BoolTrue{}, nil
}

// handleUpdateStatus serves account.updateStatus. An authenticated caller sets
// their own online/offline state. Offline=true marks the user offline;
// Offline=false marks them online. Returns tg.BoolTrue.
func (h *handlers) handleUpdateStatus(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountUpdateStatusRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	online := !req.Offline
	if err := h.store.SetUserStatus(r.Ctx, r.UserID, online); err != nil {
		h.log.Error("update status", "user_id", r.UserID, "online", online, "err", err)
		return nil, errInternal
	}
	notifyCtx, cancel := senderNotifyContext(r.Ctx)
	defer cancel()
	if err := h.store.Notify(notifyCtx, store.ChannelStatus, store.StatusPayload(r.UserID, online)); err != nil {
		h.log.Error("notify status", "user_id", r.UserID, "err", err)
	}
	return &tg.BoolTrue{}, nil
}

// reservedUsernames is the blocklist of handles that must never be claimed.
var reservedUsernames = map[string]bool{
	"admin":            true,
	"apiws":            true,
	"backgrounds":      true,
	"download":         true,
	"hls":              true,
	"hls_quality_file": true,
	"invite":           true,
	"hls_stream":       true,
	"ping":             true,
	"rtmp":             true,
	"share":            true,
	"support":          true,
	"stream":           true,
	"help":             true,
	"me":               true,
	"settings":         true,
	"telegram":         true,
	"channel":          true,
	"channels":         true,
	"bot":              true,
	"bots":             true,
	"login":            true,
	"signup":           true,
}

func isReservedUsername(username string) bool {
	return reservedUsernames[strings.ToLower(username)]
}

// isReservedAccountUsername adds the bot suffix rule for human account claims.
// Keep it separate from route reservations so bot creation can still claim
// usernames ending in bot, and existing usernames remain resolvable.
func isReservedAccountUsername(username string) bool {
	return isReservedUsername(username) || strings.HasSuffix(strings.ToLower(username), "bot")
}

// IsReservedUsername reports whether username conflicts with a server route
// or one of the globally reserved handles. Human account claims also reserve
// the bot suffix; operator bot creation uses this narrower route blocklist.
func IsReservedUsername(username string) bool {
	return isReservedUsername(username)
}

// handleUpdateUsername serves account.updateUsername. An authenticated caller
// sets or clears their own username.
//
// An empty string clears the current username. A non-empty string must pass
// validation (length, character set, first char, blocklist) before the store
// is consulted. Returns the updated user (UserClass) on success, matching the
// gotd schema for account.updateUsername.
//
// A login_mode='username' account owns no such choice: its handle is the
// credential it signs in with. Every change to it, including clearing it,
// is refused with USERNAME_IMMUTABLE; only a request repeating the stored handle
// byte-for-byte is USERNAME_NOT_MODIFIED. See checkUsernameImmutable for why the
// answer comes before any occupancy probe or quota charge.
func (h *handlers) handleUpdateUsername(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountUpdateUsernameRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}

	username := req.Username
	// Non-empty usernames must pass validation before any DB access.
	if username != "" {
		if !validateUsername(username) {
			return nil, errUsernameInvalid
		}
		if isReservedAccountUsername(username) {
			return nil, errUsernameInvalid
		}
	}
	// The credential guard runs before the lookup budget is charged: a refusal
	// that cannot succeed must not spend quota, and it is decided without
	// consulting the requested name at all.
	if err := h.checkUsernameImmutable(r.Ctx, r.UserID, username); err != nil {
		return nil, err
	}
	if username != "" {
		if err := h.checkUsernameClaimQuota(r.Ctx, r.UserID, username); err != nil {
			return nil, err
		}
	}

	if err := h.store.UpdateUsername(r.Ctx, r.UserID, username); err != nil {
		switch {
		case errors.Is(err, store.ErrUsernameOccupied):
			return nil, errUsernameOccupied
		case errors.Is(err, store.ErrUsernameFloodWait):
			return nil, errUsernameFloodWait
		case errors.Is(err, store.ErrUsernameIsLoginCredential):
			// Backstop only: checkUsernameImmutable answers first for every
			// login_mode='username' caller, and login_mode is never updated, so
			// this arm cannot be reached from here. It stays truthful so no other
			// caller can be handed NOT_MODIFIED for a handle that changed.
			return nil, errUsernameImmutable
		default:
			h.log.Error("update username", "user_id", r.UserID, "username", username, "err", err)
			return nil, errInternal
		}
	}

	// Return the updated user, not BoolTrue: gotd v0.161.0 declares
	// account.updateUsername as returning UserClass.
	updatedUser, ok, err := h.store.UserByID(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("update username: load user", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !ok {
		return nil, errInternal
	}
	wireUser := h.userToTL(updatedUser, r.UserID, true, store.Contact{}, store.LastSeenVisibility{})
	if err := h.attachCurrentProfilePhotos(r.Ctx, r.UserID, []store.User{updatedUser}, []*tg.User{wireUser}); err != nil {
		h.log.Error("update username: load profile photo", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return wireUser, nil
}

// handleCheckUsername reports whether a valid username is available. The
// pre-auth bool is an account-occupancy signal; its network quota throttles
// bulk probing but is not a privacy guarantee. Authenticated callers keep the
// same immutable answers as account.updateUsername.
func (h *handlers) handleCheckUsername(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountCheckUsernameRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	preAuth := r.UserID == 0
	if preAuth {
		if err := h.checkAndChargeRateLimitIP(r, "check_username_ip", h.rateLimitCheckUsernameIP); err != nil {
			return nil, err
		}
	}
	if !validateUsername(req.Username) || isReservedAccountUsername(req.Username) {
		return nil, errUsernameInvalid
	}
	username := strings.ToLower(req.Username)
	if !preAuth {
		if err := h.checkUsernameImmutable(r.Ctx, r.UserID, req.Username); err != nil {
			return nil, err
		}
		if err := h.checkUsernameClaimQuota(r.Ctx, r.UserID, username); err != nil {
			return nil, err
		}
	}

	taken, err := h.store.UsernameHandleTaken(r.Ctx, username)
	if err != nil {
		if preAuth {
			h.log.Error("check pre-auth username: lookup availability", "err", err)
		} else {
			h.log.Error("check account username: lookup availability", "user_id", r.UserID, "err", err)
		}
		return nil, errInternal
	}
	if !taken {
		return &tg.BoolTrue{}, nil
	}
	if preAuth {
		return &tg.BoolFalse{}, nil
	}

	resolved, found, err := h.store.UsernameByHandle(r.Ctx, username)
	if err != nil {
		h.log.Error("check account username: resolve owner", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if found && resolved.Kind == store.UsernameKindUser && resolved.User.ID == r.UserID {
		return &tg.BoolTrue{}, nil
	}
	return &tg.BoolFalse{}, nil
}

// checkUsernameImmutable answers for a login_mode='username' caller before the
// change is attempted, and returns nil for every other account.
//
// The store guard in UpdateUsername is the invariant; this is where the wire
// answer comes from, because answering from there would be wrong twice over. By
// the time UpdateUsername runs, the handler has already charged the lookup budget
// for a change that cannot happen. And the store sees only the login mode, never
// the stored handle, so it can offer exactly one refusal: that is how
// NOT_MODIFIED came to be reported for a handle the caller asked to change.
// Answering here charges nothing, releases nothing, and consults nothing but the
// caller's own stored handle, so the answer is the same whether the requested
// name is free, held by another account, or held by a channel.
//
// Only the exact stored handle counts as unchanged. A case variant is a change:
// every write path lowercases what it stores, so accepting "Operator" against a
// stored "operator" would let a client render a handle the server never held.
// EqualFold is deliberately not used.
func (h *handlers) checkUsernameImmutable(ctx context.Context, userID int64, username string) error {
	loginMode, err := h.store.UserLoginMode(ctx, userID)
	if err != nil {
		h.log.Error("update username: lookup login mode", "user_id", userID, "err", err)
		return errInternal
	}
	if loginMode != "username" {
		return nil
	}
	if username == "" {
		return errUsernameImmutable
	}
	user, ok, err := h.store.UserByID(ctx, userID)
	if err != nil {
		h.log.Error("update username: lookup stored handle", "user_id", userID, "err", err)
		return errInternal
	}
	if !ok {
		return errInternal
	}
	if user.Username != nil && *user.Username == username {
		return errUsernameNotModified
	}
	return errUsernameImmutable
}

const maxProfileAboutRunes = 70

// handleUpdateProfile serves account.updateProfile. An authenticated caller
// changes their own display name and public biography. Name validation is the
// same predicate auth.signUp uses, so a name registration accepts cannot be
// rejected here.
func (h *handlers) handleUpdateProfile(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountUpdateProfileRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	first, hasFirst := req.GetFirstName()
	last, hasLast := req.GetLastName()
	about, hasAbout := req.GetAbout()
	if hasFirst && !validateSignUpName(first) {
		return nil, errInputRequestInvalid
	}
	if hasLast && !validateSignUpName(last) {
		return nil, errInputRequestInvalid
	}
	if hasAbout && (!validText(about) || utf8.RuneCountInString(about) > maxProfileAboutRunes) {
		return nil, errAboutTooLong
	}

	if hasFirst || hasLast || hasAbout {
		if err := h.checkRateLimit(r, "update_profile", h.rateLimitUpdateProfile); err != nil {
			return nil, err
		}
		var firstName, lastName *string
		if hasFirst {
			firstName = &first
		}
		if hasLast {
			lastName = &last
		}
		if err := h.store.UpdateProfile(r.Ctx, r.UserID, firstName, lastName); err != nil {
			h.log.Error("update profile", "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
		if hasAbout {
			if err := h.store.UpdateUserAbout(r.Ctx, r.UserID, about); err != nil {
				h.log.Error("update profile about", "user_id", r.UserID, "err", err)
				return nil, errInternal
			}
		}
	}

	updatedUser, ok, err := h.store.UserByID(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("update profile: load user", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !ok {
		return nil, errInternal
	}
	wireUser := h.userToTL(updatedUser, r.UserID, true, store.Contact{}, store.LastSeenVisibility{})
	if err := h.attachCurrentProfilePhotos(r.Ctx, r.UserID, []store.User{updatedUser}, []*tg.User{wireUser}); err != nil {
		h.log.Error("update profile: load profile photo", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return wireUser, nil
}
