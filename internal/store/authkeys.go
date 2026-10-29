package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"time"
	"unicode/utf8"

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
	// EverBoundAt marks a key that has been bound; legacy keys use created_at.
	EverBoundAt   time.Time
	DeviceModel   string
	Platform      string
	SystemVersion string
	APIID         int
	AppVersion    string
	IP            string
}

// AuthorizationMetadata is the client-supplied device description and socket
// address stored when an auth key is bound or refreshed on a connection.
type AuthorizationMetadata struct {
	DeviceModel   string
	Platform      string
	SystemVersion string
	APIID         int32
	AppVersion    string
	IP            string
}

func sanitizeAuthorizationMetadata(metadata AuthorizationMetadata) AuthorizationMetadata {
	metadata.DeviceModel = sanitizeAuthorizationString(metadata.DeviceModel)
	metadata.Platform = sanitizeAuthorizationString(metadata.Platform)
	metadata.SystemVersion = sanitizeAuthorizationString(metadata.SystemVersion)
	metadata.AppVersion = sanitizeAuthorizationString(metadata.AppVersion)
	if addr, err := netip.ParseAddr(metadata.IP); err == nil {
		metadata.IP = addr.Unmap().String()
	} else {
		metadata.IP = ""
	}
	return metadata
}

type authorizationMetadataParams struct {
	deviceModel   *string
	platform      *string
	systemVersion *string
	apiID         *int32
	appVersion    *string
	ip            *string
}

func (s *Store) authorizationMetadataParams(metadata AuthorizationMetadata) authorizationMetadataParams {
	metadata = sanitizeAuthorizationMetadata(metadata)
	var params authorizationMetadataParams
	if s.sessionDeviceLabels {
		params.deviceModel = &metadata.DeviceModel
		params.platform = &metadata.Platform
		params.systemVersion = &metadata.SystemVersion
		params.apiID = &metadata.APIID
		params.appVersion = &metadata.AppVersion
	}
	if s.sessionIP {
		params.ip = &metadata.IP
	}
	return params
}

func sanitizeAuthorizationString(value string) string {
	var result strings.Builder
	result.Grow(min(len(value), 128))
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 || r == '\x00' {
			value = value[size:]
			continue
		}
		if result.Len()+size > 128 {
			break
		}
		result.WriteString(value[:size])
		value = value[size:]
	}
	return result.String()
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

// BindAuthKeyUserWithMetadata binds an unbound auth key and stores its
// connection-local initConnection metadata and trusted client socket address.
func (s *Store) BindAuthKeyUserWithMetadata(ctx context.Context, id, userID int64, metadata AuthorizationMetadata) error {
	metadataParams := s.authorizationMetadataParams(metadata)
	rows, err := s.q.BindAuthKeyUserWithMetadata(ctx, db.BindAuthKeyUserWithMetadataParams{
		ID: id, UserID: &userID, DeviceModel: metadataParams.deviceModel, Platform: metadataParams.platform,
		SystemVersion: metadataParams.systemVersion, ApiID: metadataParams.apiID,
		AppVersion: metadataParams.appVersion, Ip: metadataParams.ip,
	})
	if err != nil {
		return fmt.Errorf("bind auth key with metadata: %w", err)
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
func (s *Store) StagePendingUser(ctx context.Context, id, userID int64, lifetime time.Duration) (startedAt time.Time, remaining time.Duration, err error) {
	outcome := "failed"
	defer func() {
		if err == nil {
			outcome = "pending"
		} else if errors.Is(err, ErrAuthKeyNotFound) {
			outcome = "not_found"
		}
		s.logAuthKeyUnbind("auth.signIn", "password_challenge", outcome, id, userID)
	}()

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
	return s.PromotePendingUserWithMetadata(ctx, id, userID, startedAt, lifetime, AuthorizationMetadata{})
}

// PromotePendingUserWithMetadata completes the pending password login and
// stores the device metadata for the connection that proved the password.
func (s *Store) PromotePendingUserWithMetadata(ctx context.Context, id, userID int64, startedAt time.Time, lifetime time.Duration, metadata AuthorizationMetadata) error {
	metadataParams := s.authorizationMetadataParams(metadata)
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
		DeviceModel:    metadataParams.deviceModel,
		Platform:       metadataParams.platform,
		SystemVersion:  metadataParams.systemVersion,
		ApiID:          metadataParams.apiID,
		AppVersion:     metadataParams.appVersion,
		Ip:             metadataParams.ip,
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
	if err := s.q.TouchAuthKey(ctx, db.TouchAuthKeyParams{ID: id, StoreIp: s.sessionIP}); err != nil {
		return fmt.Errorf("touch auth key: %w", err)
	}
	return nil
}

// TouchAuthKeyWithIP advances last_seen_at and refreshes the socket IP for a
// bound key. When metadata is present, changed initConnection fields are
// updated for the same expected owner. Its MTProto caller applies the existing
// per-connection throttle.
func (s *Store) TouchAuthKeyWithIP(ctx context.Context, id int64, addr netip.Addr, expectedUserID int64, metadata *AuthorizationMetadata) error {
	params := db.TouchAuthKeyWithIPParams{ID: id, ExpectedUserID: expectedUserID}
	if s.sessionIP {
		ip := ""
		if addr.IsValid() {
			ip = addr.Unmap().String()
		}
		params.Ip = &ip
	}
	if metadata != nil && s.sessionDeviceLabels {
		cleaned := sanitizeAuthorizationMetadata(*metadata)
		params.UpdateMetadata = true
		params.DeviceModel = cleaned.DeviceModel
		params.Platform = cleaned.Platform
		params.SystemVersion = cleaned.SystemVersion
		params.ApiID = cleaned.APIID
		params.AppVersion = cleaned.AppVersion
	}
	if err := s.q.TouchAuthKeyWithIP(ctx, params); err != nil {
		return fmt.Errorf("touch auth key with IP: %w", err)
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

// ResetAuthorization unbinds keyID only while it belongs to ownerID. It clears
// SRP challenges in the same transaction and returns false for missing or
// already-unbound keys.
func (s *Store) ResetAuthorization(ctx context.Context, ownerID, keyID int64) (revoked bool, err error) {
	outcome := "failed"
	defer func() {
		if err == nil {
			if revoked {
				outcome = "cleared"
			} else {
				outcome = "no_change"
			}
		}
		s.logAuthKeyUnbind("account.resetAuthorization", "client_reset", outcome, keyID, ownerID)
	}()

	if ownerID <= 0 {
		return false, ErrAuthKeyUnauthorized
	}
	if keyID == 0 {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("reset authorization: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)
	rows, err := qtx.ResetAuthorization(ctx, db.ResetAuthorizationParams{ID: keyID, OwnerID: &ownerID})
	if err != nil {
		return false, fmt.Errorf("reset authorization: unbind key: %w", err)
	}
	if rows == 0 {
		return false, nil
	}
	if _, err := qtx.DeleteSRPChallengesForAuthKeys(ctx, []int64{keyID}); err != nil {
		return false, fmt.Errorf("reset authorization: delete SRP challenges: %w", err)
	}
	if err := commitAuthKeyRevocation(ctx, tx, "reset authorization"); err != nil {
		return false, err
	}
	return true, nil
}

// LogOutAuthKey unbinds keyID, clears its SRP challenges, and returns the
// binding that was removed. A missing key is a successful no-op.
func (s *Store) LogOutAuthKey(ctx context.Context, keyID int64) (userID int64, found bool, err error) {
	outcome := "failed"
	defer func() {
		if err == nil {
			if found && userID != 0 {
				outcome = "cleared"
			} else {
				outcome = "no_change"
			}
		}
		s.logAuthKeyUnbind("auth.logOut", "client_logout", outcome, keyID, userID)
	}()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("log out auth key: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)
	userID, err = qtx.UnbindAuthKeyForLogout(ctx, keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("log out auth key: unbind key: %w", err)
	}
	if _, err := qtx.DeleteSRPChallengesForAuthKeys(ctx, []int64{keyID}); err != nil {
		return 0, false, fmt.Errorf("log out auth key: delete SRP challenges: %w", err)
	}
	if err := commitAuthKeyRevocation(ctx, tx, "log out auth key"); err != nil {
		return 0, false, err
	}
	return userID, true, nil
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

// AuthorizationTTLDays returns the inactivity lifetime configured for userID.
func (s *Store) AuthorizationTTLDays(ctx context.Context, userID int64) (int, error) {
	days, err := s.q.AuthorizationTTLDaysByUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("authorization TTL by user: %w", err)
	}
	return int(days), nil
}

// SetAuthorizationTTL updates the owner's inactivity lifetime and refreshes
// the caller's bound auth key in the same transaction. The user row is locked
// before the auth-key row so a caller lowering its TTL cannot expire itself on
// the next sweep.
func (s *Store) SetAuthorizationTTL(ctx context.Context, ownerID, callerKeyID int64, days int) error {
	if ownerID <= 0 || callerKeyID == 0 {
		return ErrAuthKeyUnauthorized
	}
	var ttlDays int32
	switch days {
	case 7:
		ttlDays = 7
	case 30:
		ttlDays = 30
	case 90:
		ttlDays = 90
	case 180:
		ttlDays = 180
	case 365:
		ttlDays = 365
	default:
		return fmt.Errorf("set authorization TTL: unsupported days %d", days)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set authorization TTL: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	qtx := s.q.WithTx(tx)
	updated, err := qtx.SetAuthorizationTTLDays(ctx, db.SetAuthorizationTTLDaysParams{
		UserID:               ownerID,
		AuthorizationTtlDays: ttlDays,
	})
	if err != nil {
		return fmt.Errorf("set authorization TTL: update user: %w", err)
	}
	if updated != 1 {
		return ErrAuthKeyUnauthorized
	}
	touched, err := qtx.TouchBoundAuthKey(ctx, db.TouchBoundAuthKeyParams{
		AuthKeyID: callerKeyID,
		UserID:    &ownerID,
	})
	if err != nil {
		return fmt.Errorf("set authorization TTL: refresh caller key: %w", err)
	}
	if touched != 1 {
		return ErrAuthKeyUnauthorized
	}
	if err := ctx.Err(); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil {
			return fmt.Errorf("set authorization TTL: rollback canceled transaction: %w", errors.Join(err, rollbackErr))
		}
		return fmt.Errorf("set authorization TTL: canceled before commit: %w", err)
	}
	if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("set authorization TTL: commit transaction: %w", err)
	}
	return nil
}

// ExpiredAuthorizationSweepBatch bounds the number of auth-key rows one sweep
// statement may lock and unbind.
const ExpiredAuthorizationSweepBatch = 1000

// SweepExpiredAuthorizations unbinds idle auth keys in bounded batches. Each
// pass locks and unbinds its selected rows in one transaction, clearing any
// password challenges before commit. Concurrent replicas take disjoint sets,
// and a concurrent touch that wins the row lock is rechecked before revocation.
// Returned keys are already durably revoked.
func (s *Store) SweepExpiredAuthorizations(ctx context.Context, batch int) ([]AuthKey, error) {
	if batch <= 0 || batch > math.MaxInt32 {
		return nil, fmt.Errorf("sweep expired authorizations: batch %d out of range", batch)
	}
	batchLimit := int32(batch)
	var revoked []AuthKey
	for {
		batchRevoked, rowCount, err := func() ([]AuthKey, int, error) {
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				return nil, 0, fmt.Errorf("sweep expired authorization batch: begin transaction: %w", err)
			}
			defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

			qtx := s.q.WithTx(tx)
			rows, err := qtx.SweepExpiredAuthorizationBatch(ctx, batchLimit)
			if err != nil {
				return nil, 0, fmt.Errorf("sweep expired authorization batch: unbind keys: %w", err)
			}
			batchRevoked := make([]AuthKey, 0, len(rows))
			targetIDs := make([]int64, 0, len(rows))
			for _, row := range rows {
				if row.UserID == nil {
					return nil, 0, errors.New("sweep expired authorization batch: unbound key has no previous owner")
				}
				batchRevoked = append(batchRevoked, AuthKey{ID: row.ID, UserID: *row.UserID})
				targetIDs = append(targetIDs, row.ID)
			}
			if len(targetIDs) > 0 {
				if _, err := qtx.DeleteSRPChallengesForAuthKeys(ctx, targetIDs); err != nil {
					return nil, 0, fmt.Errorf("sweep expired authorization batch: delete SRP challenges: %w", err)
				}
			}
			if err := commitAuthKeyRevocation(ctx, tx, "sweep expired authorization batch"); err != nil {
				return nil, 0, err
			}
			return batchRevoked, len(rows), nil
		}()
		if err != nil {
			return revoked, err
		}
		if len(batchRevoked) > 0 {
			for _, key := range batchRevoked {
				s.logAuthKeyUnbind("authorization_ttl_sweep", "authorization_ttl_expired", "cleared", key.ID, key.UserID)
			}
			notifyCtx, cancel := NotificationContext(ctx)
			for _, key := range batchRevoked {
				if err := s.Notify(notifyCtx, ChannelEvict, EvictPayload(key.UserID, key.ID)); err != nil {
					s.log.Error("notify evicted authorization", "user_id", key.UserID, "auth_key_id", key.ID, "err", err)
				}
			}
			cancel()
		}
		revoked = append(revoked, batchRevoked...)
		if rowCount < batch {
			return revoked, nil
		}
	}
}

// NeverBoundAuthKeySweepBatch bounds how many auth-key rows one deletion
// transaction may lock and remove.
const NeverBoundAuthKeySweepBatch = 1000

// SweepNeverBoundAuthKeys deletes only unbound keys without an ever-bound
// marker and without a live pending login. Each batch is its own transaction;
// concurrent replicas skip each other's locked rows. It returns only the total
// number deleted so callers cannot log key identifiers.
func (s *Store) SweepNeverBoundAuthKeys(ctx context.Context, idleWindow, maxAge, pendingLifetime time.Duration, batch int) (int, error) {
	if idleWindow <= 0 || maxAge <= 0 || pendingLifetime <= 0 || maxAge < idleWindow {
		return 0, errors.New("sweep never-bound auth keys: invalid retention windows")
	}
	if batch <= 0 || batch > math.MaxInt32 {
		return 0, fmt.Errorf("sweep never-bound auth keys: batch %d out of range", batch)
	}
	params := db.SweepNeverBoundAuthKeyBatchParams{
		IdleWindowMicros:      int64(idleWindow / time.Microsecond),
		MaxAgeMicros:          int64(maxAge / time.Microsecond),
		PendingLifetimeMicros: int64(pendingLifetime / time.Microsecond),
		Batch:                 int32(batch),
	}
	deleted := 0
	for {
		batchDeleted, err := func() (batchDeleted int, batchErr error) {
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				return 0, fmt.Errorf("sweep never-bound auth-key batch: begin transaction: %w", err)
			}
			defer func() {
				if batchErr != nil {
					batchErr = rollbackNeverBoundAuthKeySweepBatch(tx, batchErr)
				}
			}()

			rows, err := s.q.WithTx(tx).SweepNeverBoundAuthKeyBatch(ctx, params)
			if err != nil {
				return 0, fmt.Errorf("sweep never-bound auth-key batch: delete keys: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return 0, fmt.Errorf("sweep never-bound auth-key batch: commit transaction: %w", err)
			}
			return int(rows), nil
		}()
		if err != nil {
			return deleted, err
		}
		deleted += batchDeleted
		if batchDeleted < batch {
			return deleted, nil
		}
	}
}

func rollbackNeverBoundAuthKeySweepBatch(tx pgx.Tx, operationErr error) error {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
		return errors.Join(operationErr, fmt.Errorf("sweep never-bound auth-key batch: rollback transaction: %w", rollbackErr))
	}
	return operationErr
}

// ResetAuthorizations unbinds every bound or password-pending auth key for
// ownerID except callerKeyID. The caller must still be bound to ownerID when
// the reset takes effect. It returns only keys that were bound to the owner,
// for callers that need to evict their live connections, and only after the
// unbind has committed.
func (s *Store) ResetAuthorizations(ctx context.Context, ownerID, callerKeyID int64) (removedBound []int64, err error) {
	outcome := "failed"
	loggedTargets := false
	defer func() {
		if loggedTargets {
			return
		}
		if err == nil {
			outcome = "no_change"
		}
		s.logAuthKeyUnbind("auth.resetAuthorizations", "client_reset_all", outcome, callerKeyID, ownerID)
	}()

	if ownerID <= 0 || callerKeyID == 0 {
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

	// One UPDATE handles both bound and password-pending keys. PostgreSQL can
	// then recheck a pending row after it waits for a concurrent promotion.
	rows, err := qtx.UnbindOtherAuthKeysForOwner(ctx, db.UnbindOtherAuthKeysForOwnerParams{
		CallerID: callerKeyID,
		OwnerID:  &ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("reset authorizations: unbind other keys: %w", err)
	}
	removedBound = make([]int64, 0, len(rows))
	targetIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		targetIDs = append(targetIDs, row.ID)
		if row.UserID != nil {
			removedBound = append(removedBound, row.ID)
		}
	}
	if _, err := qtx.DeleteSRPChallengesForAuthKeys(ctx, targetIDs); err != nil {
		return nil, fmt.Errorf("reset authorizations: delete SRP challenges: %w", err)
	}

	if s.authKeyResetBeforeCommitHook != nil {
		s.authKeyResetBeforeCommitHook()
	}
	if err := commitAuthKeyRevocation(ctx, tx, "reset authorizations"); err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		for _, row := range rows {
			s.logAuthKeyUnbind("auth.resetAuthorizations", "client_reset_all", "cleared", row.ID, ownerID)
		}
		loggedTargets = true
	}
	return removedBound, nil
}

func commitAuthKeyRevocation(ctx context.Context, tx pgx.Tx, operation string) error {
	if err := ctx.Err(); err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(rollbackCtx); rollbackErr != nil {
			return fmt.Errorf("%s: rollback canceled transaction: %w", operation, errors.Join(err, rollbackErr))
		}
		return fmt.Errorf("%s: canceled before commit: %w", operation, err)
	}
	// Once commit starts, detach it from request cancellation: PostgreSQL may
	// commit before pgx observes a canceled context, which would otherwise
	// report an error for a durable revocation and hide its result from callers.
	if err := tx.Commit(context.WithoutCancel(ctx)); err != nil {
		return fmt.Errorf("%s: commit transaction: %w", operation, err)
	}
	return nil
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
	var everBoundAt time.Time
	if k.EverBoundAt.Valid {
		everBoundAt = k.EverBoundAt.Time
	}
	return AuthKey{
		ID:               k.ID,
		Value:            value,
		UserID:           userID,
		PendingUserID:    pendingUserID,
		PendingStartedAt: pendingStartedAt,
		EverBoundAt:      everBoundAt,
		CreatedAt:        k.CreatedAt.Time,
		LastSeenAt:       k.LastSeenAt.Time,
		DeviceModel:      nullableString(k.DeviceModel),
		Platform:         nullableString(k.Platform),
		SystemVersion:    nullableString(k.SystemVersion),
		APIID:            nullableInt32(k.ApiID),
		AppVersion:       nullableString(k.AppVersion),
		IP:               nullableString(k.Ip),
	}, nil
}

func nullableString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func nullableInt32(value *int32) int {
	if value == nil {
		return 0
	}
	return int(*value)
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
	var everBoundAt time.Time
	if k.EverBoundAt.Valid {
		everBoundAt = k.EverBoundAt.Time
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
		EverBoundAt:      everBoundAt,
	}, nil
}
