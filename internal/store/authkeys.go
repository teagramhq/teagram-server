package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// AuthKey is a persisted MTProto auth key. UserID is 0 while the key is
// unbound; once login binds it to an account UserID holds that user's id.
// CreatedAt/LastSeenAt back the session dates reported by account.getAuthorizations.
type AuthKey struct {
	ID     int64
	Value  []byte
	UserID int64
	// PendingUserID is the half-authorized user staged by signIn for a 2FA
	// account; it is 0 unless a password challenge is outstanding and never
	// grants access on its own.
	PendingUserID int64
	// PendingStartedAt is the database-clock start of the current pending
	// password login. It is zero for legacy pending rows, which fail closed.
	PendingStartedAt time.Time
	// PendingRemaining is the lease duration computed by Postgres for the
	// requested pending-login lifetime. It is zero when no lease remains.
	PendingRemaining time.Duration
	// Provisional is true when the bound user is username-mode and has not
	// yet completed sign-in (no verifier stored). It is derived from the
	// login_mode column and the absence of a user_passwords row, never stored.
	Provisional bool
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// PendingLogin is the current half-authorized 2FA state for an auth key.
// Active is evaluated by Postgres against its own clock and requested lease.
type PendingLogin struct {
	UserID    int64
	StartedAt time.Time
	Active    bool
}

// SaveAuthKey stores value under id, idempotently. The key value is encrypted at
// rest with the store's master key. Re-saving an existing id refreshes its value
// and last-seen time without touching its user binding.
func (s *Store) SaveAuthKey(ctx context.Context, id int64, value []byte) error {
	enc, err := s.cipher.Seal(value)
	if err != nil {
		return fmt.Errorf("save auth key: %w", err)
	}
	if err := s.q.SaveAuthKey(ctx, db.SaveAuthKeyParams{ID: id, KeyValue: enc}); err != nil {
		return fmt.Errorf("save auth key: %w", err)
	}
	return nil
}

// ValidateAuthKeyEncryption checks whether the configured encryption key can
// open one stored auth key. It reports hasKeys=false for an empty database,
// which is the explicit first-bootstrap case.
func (s *Store) ValidateAuthKeyEncryption(ctx context.Context) (bool, error) {
	var encrypted []byte
	err := s.pool.QueryRow(ctx, `SELECT key_value FROM auth_keys ORDER BY id LIMIT 1`).Scan(&encrypted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("read stored auth key for encryption readiness: %w", err)
	}
	plaintext, err := s.cipher.Open(encrypted)
	if err != nil {
		return true, fmt.Errorf("validate stored auth-key encryption: %w", err)
	}
	clear(plaintext)
	return true, nil
}

// AuthKeyByID returns the auth key for id, ok=false when absent.
// The Provisional field is derived: true when the bound user has
// login_mode='username' and no user_passwords row.
func (s *Store) AuthKeyByID(ctx context.Context, id int64) (AuthKey, bool, error) {
	return s.authKeyByID(ctx, id, 0)
}

// AuthKeyByIDWithPendingLease returns the auth key and the remaining pending
// login lease computed against PostgreSQL's clock in the same lookup.
func (s *Store) AuthKeyByIDWithPendingLease(ctx context.Context, id int64, lifetime time.Duration) (AuthKey, bool, error) {
	return s.authKeyByID(ctx, id, lifetime)
}

func (s *Store) authKeyByID(ctx context.Context, id int64, lifetime time.Duration) (AuthKey, bool, error) {
	row, err := s.q.AuthKeyByID(ctx, db.AuthKeyByIDParams{
		ID:                    id,
		PendingLifetimeMicros: int64(lifetime / time.Microsecond),
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return AuthKey{}, false, nil
	case err != nil:
		return AuthKey{}, false, fmt.Errorf("auth key by id: %w", err)
	}
	key, err := s.authKeyFromDB(row)
	if err != nil {
		return AuthKey{}, false, err
	}
	return key, true, nil
}

// BindAuthKeyUser links the auth key id to userID. The user must already exist.
// It returns ErrAuthKeyNotFound when no auth-key row matches id (absent key or a
// concurrent delete), so callers fail closed instead of reporting a false bind.
func (s *Store) BindAuthKeyUser(ctx context.Context, id, userID int64) error {
	rows, err := s.q.BindAuthKeyUser(ctx, db.BindAuthKeyUserParams{ID: id, UserID: &userID})
	if err != nil {
		return fmt.Errorf("bind auth key user: %w", err)
	}
	if rows == 0 {
		return ErrAuthKeyNotFound
	}
	return nil
}

// SetPendingUser marks the auth key id as half-authorized for userID: the state
// between auth.signIn and auth.checkPassword when the account has 2FA. It clears
// any existing user_id binding so the key is de-authorized until checkPassword
// promotes it — a re-signIn on an already-bound key must not stay authorized as
// the old user. It never grants access on its own; only PromotePendingUser
// authorizes. Returns ErrAuthKeyNotFound when no auth-key row matches id, so
// callers fail closed.
func (s *Store) SetPendingUser(ctx context.Context, id, userID int64) error {
	_, _, err := s.StagePendingUser(ctx, id, userID, 0)
	return err
}

// StagePendingUser marks the auth key as half-authorized and returns the
// database-clock start time and remaining lease for this fresh login window.
func (s *Store) StagePendingUser(ctx context.Context, id, userID int64, lifetime time.Duration) (time.Time, time.Duration, error) {
	staged, err := s.q.SetPendingUser(ctx, db.SetPendingUserParams{
		ID:             id,
		UserID:         &userID,
		LifetimeMicros: int64(lifetime / time.Microsecond),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, ErrAuthKeyNotFound
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("set pending user: %w", err)
	}
	if !staged.PendingStartedAt.Valid {
		return time.Time{}, 0, errors.New("set pending user: database returned no start time")
	}
	return staged.PendingStartedAt.Time, time.Duration(staged.PendingRemainingMicros) * time.Microsecond, nil
}

// PendingLoginByID returns the current pending identity and whether its lease
// is still live, evaluated with the database clock.
func (s *Store) PendingLoginByID(ctx context.Context, id int64, lifetime time.Duration) (PendingLogin, bool, error) {
	row, err := s.q.PendingLoginByID(ctx, db.PendingLoginByIDParams{
		ID:             id,
		LifetimeMicros: int64(lifetime / time.Microsecond),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingLogin{}, false, nil
	}
	if err != nil {
		return PendingLogin{}, false, fmt.Errorf("pending login by auth key: %w", err)
	}
	if row.PendingUserID == nil {
		return PendingLogin{}, false, errors.New("pending login by auth key: pending user is null")
	}
	active, ok := row.Active.(bool)
	if !ok {
		return PendingLogin{}, false, fmt.Errorf("pending login by auth key: active has unexpected type %T", row.Active)
	}
	login := PendingLogin{UserID: *row.PendingUserID, Active: active}
	if row.PendingStartedAt.Valid {
		login.StartedAt = row.PendingStartedAt.Time
	}
	return login, true, nil
}

// ClearExpiredPendingUser clears only the expired pending generation observed
// by the caller. Matching both user and start time keeps cleanup from erasing a
// newer sign-in that raced the expiry check.
func (s *Store) ClearExpiredPendingUser(ctx context.Context, id, userID int64, startedAt time.Time, lifetime time.Duration) error {
	var started pgtype.Timestamptz
	if !startedAt.IsZero() {
		started = pgtype.Timestamptz{Time: startedAt, Valid: true}
	}
	_, err := s.q.ClearExpiredPendingUser(ctx, db.ClearExpiredPendingUserParams{
		ID:             id,
		UserID:         &userID,
		StartedAt:      started,
		LifetimeMicros: int64(lifetime / time.Microsecond),
	})
	if err != nil {
		return fmt.Errorf("clear expired pending user: %w", err)
	}
	return nil
}

// PromotePendingUser authorizes the key only if the current pending identity
// and start time still match the proof's identity and the lease is live.
func (s *Store) PromotePendingUser(ctx context.Context, id, userID int64, startedAt time.Time, lifetime time.Duration) error {
	// Lock separately: PostgreSQL may not recheck UPDATE predicates for an
	// unchanged row after it waits for the row lock.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("promote pending user: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err := qtx.LockAuthKeyForPromotion(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAuthKeyNotFound
		}
		return fmt.Errorf("promote pending user: lock auth key: %w", err)
	}

	started := pgtype.Timestamptz{Time: startedAt, Valid: !startedAt.IsZero()}
	rows, err := qtx.PromotePendingUser(ctx, db.PromotePendingUserParams{
		ID:             id,
		UserID:         &userID,
		StartedAt:      started,
		LifetimeMicros: int64(lifetime / time.Microsecond),
	})
	if err != nil {
		return fmt.Errorf("promote pending user: %w", err)
	}
	if rows == 0 {
		return ErrAuthKeyNotFound
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("promote pending user: commit transaction: %w", err)
	}
	return nil
}

// TouchAuthKey advances last_seen_at to now for id. It backs the DateActive
// field of account.getAuthorizations with real session activity; the mtproto
// loop throttles calls so this is not written on every frame. Touching a missing
// id is a no-op.
func (s *Store) TouchAuthKey(ctx context.Context, id int64) error {
	if err := s.q.TouchAuthKey(ctx, id); err != nil {
		return fmt.Errorf("touch auth key: %w", err)
	}
	return nil
}

// DeleteAuthKey removes the auth key id. Deleting a missing id is a no-op.
func (s *Store) DeleteAuthKey(ctx context.Context, id int64) error {
	if err := s.q.DeleteAuthKey(ctx, id); err != nil {
		return fmt.Errorf("delete auth key: %w", err)
	}
	return nil
}

// AuthKeysByUser returns every auth key bound to userID. The Provisional field
// is not populated (the query does not join users/passwords).
func (s *Store) AuthKeysByUser(ctx context.Context, userID int64) ([]AuthKey, error) {
	rows, err := s.q.AuthKeysByUser(ctx, &userID)
	if err != nil {
		return nil, fmt.Errorf("auth keys by user: %w", err)
	}
	keys := make([]AuthKey, len(rows))
	for i, r := range rows {
		key, err := s.authKeyFromDBBasic(r)
		if err != nil {
			return nil, err
		}
		keys[i] = key
	}
	return keys, nil
}

// ResetAuthorizations removes every bound or password-pending auth key for
// ownerID except callerKeyID. The caller must still be bound to ownerID when
// the reset takes effect. It returns only the removed bound keys, for callers
// that need to evict their live connections, and only after the deletion has
// committed.
func (s *Store) ResetAuthorizations(ctx context.Context, ownerID, callerKeyID int64) ([]int64, error) {
	if ownerID <= 0 || callerKeyID <= 0 {
		return nil, ErrAuthKeyUnauthorized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("reset authorizations: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	// Serialize resets for this owner, then lock and recheck the retained caller
	// so it cannot be revoked or rebound between authorization and deletion.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", ownerID); err != nil {
		return nil, fmt.Errorf("reset authorizations: lock owner: %w", err)
	}
	qtx := s.q.WithTx(tx)
	if _, err := qtx.LockAuthKeyForResetAuthorization(ctx, db.LockAuthKeyForResetAuthorizationParams{
		CallerID: callerKeyID,
		OwnerID:  &ownerID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAuthKeyUnauthorized
	} else if err != nil {
		return nil, fmt.Errorf("reset authorizations: lock caller key: %w", err)
	}

	// One DELETE handles both bound and password-pending keys. PostgreSQL can
	// then recheck a pending row after it waits for a concurrent promotion.
	rows, err := qtx.DeleteOtherAuthKeysForOwner(ctx, db.DeleteOtherAuthKeysForOwnerParams{
		CallerID: callerKeyID,
		OwnerID:  &ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("reset authorizations: delete other keys: %w", err)
	}
	removedBound := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.UserID != nil {
			removedBound = append(removedBound, row.ID)
		}
	}

	if s.authKeyResetBeforeCommitHook != nil {
		s.authKeyResetBeforeCommitHook()
	}
	if err := ctx.Err(); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil {
			return nil, fmt.Errorf("reset authorizations: rollback canceled transaction: %w", errors.Join(err, rollbackErr))
		}
		return nil, fmt.Errorf("reset authorizations: canceled before commit: %w", err)
	}
	// Once commit starts, detach it from request cancellation: PostgreSQL may
	// commit before pgx observes a canceled context, which would otherwise return
	// an error for a durable deletion and hide the target result from the caller.
	if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
		return nil, fmt.Errorf("reset authorizations: commit transaction: %w", err)
	}
	return removedBound, nil
}

// authKeyFromDBBasic maps the basic db.AuthKey struct (used by AuthKeysByUser)
// to the domain type, decrypting the stored key value and collapsing NULL
// user_id to UserID 0. Provisional is always false — the query does not join
// users/passwords.
func (s *Store) authKeyFromDBBasic(k db.AuthKey) (AuthKey, error) {
	value, err := s.cipher.Open(k.KeyValue)
	if err != nil {
		return AuthKey{}, fmt.Errorf("decrypt auth key %d: %w", k.ID, err)
	}
	var userID int64
	if k.UserID != nil {
		userID = *k.UserID
	}
	var pendingUserID int64
	if k.PendingUserID != nil {
		pendingUserID = *k.PendingUserID
	}
	var pendingStartedAt time.Time
	if k.PendingStartedAt.Valid {
		pendingStartedAt = k.PendingStartedAt.Time
	}
	return AuthKey{
		ID:               k.ID,
		Value:            value,
		UserID:           userID,
		PendingUserID:    pendingUserID,
		PendingStartedAt: pendingStartedAt,
		CreatedAt:        k.CreatedAt.Time,
		LastSeenAt:       k.LastSeenAt.Time,
	}, nil
}

// authKeyFromDB maps an AuthKeyByIDRow to the domain type, decrypting the stored
// key value and collapsing a NULL user_id to UserID 0. A decrypt failure (wrong
// master key or corrupt/tampered row) is returned, never silently swallowed.
// Provisional is derived: true when the bound user has login_mode='username'
// and no user_passwords row (HasPassword is false). Unbound keys (user_id NULL)
// are always non-provisional.
func (s *Store) authKeyFromDB(k db.AuthKeyByIDRow) (AuthKey, error) {
	value, err := s.cipher.Open(k.KeyValue)
	if err != nil {
		return AuthKey{}, fmt.Errorf("decrypt auth key %d: %w", k.ID, err)
	}
	var userID int64
	if k.UserID != nil {
		userID = *k.UserID
	}
	var pendingUserID int64
	if k.PendingUserID != nil {
		pendingUserID = *k.PendingUserID
	}
	var pendingStartedAt time.Time
	if k.PendingStartedAt.Valid {
		pendingStartedAt = k.PendingStartedAt.Time
	}
	provisional := false
	if k.UserID != nil && k.LoginMode != nil && *k.LoginMode == "username" {
		hasPw, ok := k.HasPassword.(bool)
		if !ok {
			return AuthKey{}, fmt.Errorf("auth key %d: has_password has unexpected type %T", k.ID, k.HasPassword)
		}
		if !hasPw {
			provisional = true
		}
	}
	return AuthKey{
		ID:               k.ID,
		Value:            value,
		UserID:           userID,
		PendingUserID:    pendingUserID,
		PendingStartedAt: pendingStartedAt,
		PendingRemaining: time.Duration(k.PendingRemainingMicros) * time.Microsecond,
		Provisional:      provisional,
		CreatedAt:        k.CreatedAt.Time,
		LastSeenAt:       k.LastSeenAt.Time,
	}, nil
}
