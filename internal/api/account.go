package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// authorizationTTLDays is the session lifetime advertised to clients in
// account.getAuthorizations. The server does not auto-expire sessions, so this
// is a non-misleading policy value (~1 year) rather than a real expiry, and is
// not enforced by any sweep.
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
	keys, err := h.store.AuthKeysByUser(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get authorizations: list keys", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	current := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	auths := make([]tg.Authorization, len(keys))
	for i, k := range keys {
		auths[i] = tg.Authorization{
			Current:     k.ID == current,
			Hash:        k.ID,
			DateCreated: int(k.CreatedAt.Unix()),
			DateActive:  int(k.LastSeenAt.Unix()),
		}
	}
	return &tg.AccountAuthorizations{
		AuthorizationTTLDays: authorizationTTLDays,
		Authorizations:       auths,
	}, nil
}

// handleResetAuthorization serves account.resetAuthorization. It revokes the
// caller's session identified by Hash (an auth key id) by deleting that auth
// key. The key is deleted only when it belongs to the caller, so a user cannot
// reset another user's session; a foreign or unknown hash returns HASH_INVALID.
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
	key, ok, err := h.store.AuthKeyByID(r.Ctx, req.Hash)
	if err != nil {
		h.log.Error("reset authorization: lookup key", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	// Scope check: reject unless the target key is one of the caller's own.
	if !ok || key.UserID != r.UserID {
		return nil, nil, errHashInvalid
	}
	if err := h.store.DeleteAuthKey(r.Ctx, req.Hash); err != nil {
		h.log.Error("reset authorization: delete key", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	// Emitted after the delete has committed, so the evicted client cannot
	// reconnect on the same key and find the row still present.
	evict := func() { h.notifyEvict(r.Ctx, key.UserID, req.Hash) }
	if selfRevocation(r, req.Hash) {
		return &tg.BoolTrue{}, evict, nil
	}
	evict()
	return &tg.BoolTrue{}, nil, nil
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

// IsReservedUsername reports whether username conflicts with a server route
// or a reserved account handle. Local operator tools use the same blocklist as
// the RPC admission paths.
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
		if isReservedUsername(username) {
			return nil, errUsernameInvalid
		}
	}
	// The credential guard runs before the lookup budget is charged: a refusal
	// that cannot succeed must not consume quota, and must not learn
	// anything about whether the target name is taken.
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
	return h.userToTL(updatedUser, r.UserID, true, store.Contact{}), nil
}

// checkUsernameImmutable answers for a login_mode='username' caller before the
// change is attempted, and returns nil for every other account.
//
// The store guard in UpdateUsername is the invariant; this is where the wire
// answer comes from, because the store guard sits after two things a refusal must
// not do: the lookup budget is already charged by the time it runs, and the claim
// insert reports the target as occupied, which would turn a refusal into a free
// occupancy oracle. Answering here charges nothing, releases nothing, and probes
// nothing: the comparison is against the caller's own stored handle, so the answer
// is the same whether the requested name is free, held by another account, or
// held by a channel.
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

// handleUpdateProfile serves account.updateProfile. An authenticated caller
// changes their own first and/or last name. About is accepted on the wire and
// ignored: bio storage is out of scope. Name validation is the same predicate
// auth.signUp uses, so a name that registration would accept cannot be rejected
// here.
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
	if hasFirst && !validateSignUpName(first) {
		return nil, errInputRequestInvalid
	}
	if hasLast && !validateSignUpName(last) {
		return nil, errInputRequestInvalid
	}

	if hasFirst || hasLast {
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
	}

	updatedUser, ok, err := h.store.UserByID(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("update profile: load user", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !ok {
		return nil, errInternal
	}
	return h.userToTL(updatedUser, r.UserID, true, store.Contact{}), nil
}
