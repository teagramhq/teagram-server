package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	// DefaultInviteLifetime is the lifetime used when IssueInvite receives no
	// override, or an explicit zero override.
	DefaultInviteLifetime = 7 * 24 * time.Hour
	// MaxInviteLifetime bounds how long an issued authorization can remain
	// usable. There is deliberately no unbounded-lifetime option.
	MaxInviteLifetime = 30 * 24 * time.Hour
	inviteSecretBytes = 32
	// UserInviteListLimit bounds the number of invite records returned to one user.
	UserInviteListLimit = 100
)

// InviteState is the operator-visible lifecycle state of a registration
// invite. An issued row whose database expiry has passed is reported as
// InviteExpired even before a writer retires the row.
type InviteState string

const (
	InviteIssued   InviteState = "issued"
	InviteConsumed InviteState = "consumed"
	InviteRevoked  InviteState = "revoked"
	InviteExpired  InviteState = "expired"
)

// RegistrationInvite is the metadata operators may list. It intentionally has
// no secret or secret digest field: the cleartext is returned only by
// IssueInvite, and the digest is an internal verification value.
type RegistrationInvite struct {
	ID            int64
	Handle        string
	InviterUserID *int64
	IssuedAt      time.Time
	ExpiresAt     time.Time
	State         InviteState
	ConsumedAt    *time.Time
	RevokedAt     *time.Time
}

// RegistrationInvitee is the account and database redemption time recorded on
// a user-issued invite row.
type RegistrationInvitee struct {
	UserID     int64
	RedeemedAt time.Time
}

// Invite is a short alias for callers that do not need to distinguish this
// record from the other invite types in the store.
type Invite = RegistrationInvite

// InviteAdmissionPolicy describes which invite kinds a signup transaction may
// redeem. Operator invites remain available whenever RequireInvite is true;
// user invites additionally require the explicit listener gate and quota.
type InviteAdmissionPolicy struct {
	RequireInvite    bool
	UserInviteQuota  int
	AllowUserInvites bool
}

// ErrInviteLive is returned when an unexpired, unrevoked and unconsumed invite
// already exists for the canonical handle.
var ErrInviteLive = errors.New("registration invite already live")

// ErrInviteLifetimeInvalid is returned when the requested lifetime is not
// positive at database timestamp precision or exceeds MaxInviteLifetime.
var ErrInviteLifetimeInvalid = errors.New("registration invite lifetime invalid")

// ErrInviteHandleInvalid is returned when no handle was supplied.
var ErrInviteHandleInvalid = errors.New("registration invite handle invalid")

// ErrInviteInviterInvalid is returned when a user invite is issued without a
// positive inviter identity.
var ErrInviteInviterInvalid = errors.New("registration invite inviter invalid")

// ErrInviteTransactionRequired is returned when a caller tries to consume an
// invite without supplying the transaction that must own the state change.
var ErrInviteTransactionRequired = errors.New("registration invite transaction required")

// ErrUserInviteQuotaReached means the inviter has issued the configured number
// of invites during the trailing seven days, regardless of their state.
var ErrUserInviteQuotaReached = errors.New("user invite weekly limit reached")

// ErrUserInvitesDisabled means user-issued invites are disabled by quota.
var ErrUserInvitesDisabled = errors.New("user invites disabled")

// ErrUserInviteMinimumAgeInvalid means the configured account-age gate is
// negative.
var ErrUserInviteMinimumAgeInvalid = errors.New("user invite minimum account age invalid")

// UserInviteAccountTooNewError carries the remaining account-age wait. Seconds
// are rounded up so a caller never retries before the minimum age is reached.
type UserInviteAccountTooNewError struct {
	RemainingWaitSeconds int64
}

func (e *UserInviteAccountTooNewError) Error() string {
	return fmt.Sprintf("user invite account too new; wait %d seconds", e.RemainingWaitSeconds)
}

// IssueInvite creates one invite for handle and returns its metadata plus the
// cleartext secret. The secret is generated once with crypto/rand and only its
// SHA-256 digest is persisted. A missing or zero lifetime selects
// DefaultInviteLifetime; one non-zero override is accepted and cannot exceed
// MaxInviteLifetime.
//
// The expired-row transition and the insert are in one transaction. The
// partial unique index on issued rows arbitrates the race where two callers
// try to issue the first live invite for a handle; a unique conflict is the
// same ErrInviteLive refusal as the serialized path.
func (s *Store) IssueInvite(ctx context.Context, handle string, lifetimes ...time.Duration) (RegistrationInvite, string, error) {
	handle = strings.ToLower(handle)
	if handle == "" {
		return RegistrationInvite{}, "", ErrInviteHandleInvalid
	}
	lifetime, err := inviteLifetime(lifetimes)
	if err != nil {
		return RegistrationInvite{}, "", err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if _, err := qtx.ExpireRegistrationInvite(ctx, handle); err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue invite: expire old row: %w", err)
	}

	secret, digest, err := newRegistrationInviteSecret()
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue invite: random secret: %w", err)
	}

	row, err := qtx.InsertRegistrationInvite(ctx, db.InsertRegistrationInviteParams{
		Handle:       handle,
		SecretDigest: digest,
		LifetimeUs:   lifetime.Microseconds(),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "registration_invites_live_handle_idx" {
			return RegistrationInvite{}, "", ErrInviteLive
		}
		return RegistrationInvite{}, "", fmt.Errorf("issue invite: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue invite: commit: %w", err)
	}
	return registrationInviteFromInsert(row), secret, nil
}

// IssueUserInvite creates a handle-less invite attributed to inviterUserID.
// The returned cleartext is generated once; registration_invites stores only
// its SHA-256 digest. It uses the same default lifetime and hard cap as
// operator-issued invites.
func (s *Store) IssueUserInvite(ctx context.Context, inviterUserID int64, lifetimes ...time.Duration) (RegistrationInvite, string, error) {
	if inviterUserID <= 0 {
		return RegistrationInvite{}, "", ErrInviteInviterInvalid
	}
	lifetime, err := inviteLifetime(lifetimes)
	if err != nil {
		return RegistrationInvite{}, "", err
	}
	secret, digest, err := newRegistrationInviteSecret()
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue user invite: random secret: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue user invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	row, err := s.q.WithTx(tx).InsertUserRegistrationInvite(ctx, db.InsertUserRegistrationInviteParams{
		InviterUserID: &inviterUserID,
		SecretDigest:  digest,
		LifetimeUs:    lifetime.Microseconds(),
	})
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue user invite: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("issue user invite: commit: %w", err)
	}
	return registrationInviteFromUserInsert(row), secret, nil
}

// CreateUserInvite enforces account age and the trailing-seven-day issuance
// quota in one transaction. lockOwners is the existing per-owner advisory lock
// used by account-scoped mutations; this path takes it before the count and
// invite insert, then holds it through commit.
func (s *Store) CreateUserInvite(ctx context.Context, inviterUserID int64, quota int, minimumAccountAge time.Duration) (RegistrationInvite, string, error) {
	if inviterUserID <= 0 {
		return RegistrationInvite{}, "", ErrInviteInviterInvalid
	}
	if quota <= 0 {
		return RegistrationInvite{}, "", ErrUserInvitesDisabled
	}
	if minimumAccountAge < 0 {
		return RegistrationInvite{}, "", ErrUserInviteMinimumAgeInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	if err := lockOwners(ctx, tx, inviterUserID); err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: lock inviter: %w", err)
	}
	qtx := s.q.WithTx(tx)
	accountAge, err := qtx.UserInviteAccountAge(ctx, inviterUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistrationInvite{}, "", ErrInviteInviterInvalid
	}
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: account age: %w", err)
	}
	if minimumAccountAge > 0 {
		eligibleAt := accountAge.CreatedAt.Time.Add(minimumAccountAge)
		remaining := eligibleAt.Sub(accountAge.Now.Time)
		if remaining > 0 {
			seconds := int64(remaining / time.Second)
			if remaining%time.Second != 0 {
				seconds++
			}
			return RegistrationInvite{}, "", &UserInviteAccountTooNewError{RemainingWaitSeconds: seconds}
		}
	}

	usage, err := qtx.UserInviteUsage(ctx, inviterUserID)
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: quota: %w", err)
	}
	if usage.WeeklyCount >= int64(quota) {
		return RegistrationInvite{}, "", ErrUserInviteQuotaReached
	}

	secret, digest, err := newRegistrationInviteSecret()
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: random secret: %w", err)
	}
	row, err := qtx.InsertUserRegistrationInvite(ctx, db.InsertUserRegistrationInviteParams{
		InviterUserID: &inviterUserID,
		SecretDigest:  digest,
		LifetimeUs:    DefaultInviteLifetime.Microseconds(),
	})
	if err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistrationInvite{}, "", fmt.Errorf("create user invite: commit: %w", err)
	}
	return registrationInviteFromUserInsert(row), secret, nil
}

// RevokeUserInvite revokes a live invite only when inviterUserID owns it. The
// filtered update gives unknown, foreign, expired, consumed, and revoked ids
// the same successful no-op result. Its row lock races directly with redemption.
func (s *Store) RevokeUserInvite(ctx context.Context, inviterUserID, id int64) error {
	if inviterUserID <= 0 || id <= 0 {
		return nil
	}
	if _, err := s.q.RevokeUserRegistrationInvite(ctx, db.RevokeUserRegistrationInviteParams{
		ID:            id,
		InviterUserID: inviterUserID,
	}); err != nil {
		return fmt.Errorf("revoke user invite: %w", err)
	}
	return nil
}

// ListUserInvites returns only the caller's metadata, newest first. The count
// and next-available time use one PostgreSQL clock value in a repeatable-read
// snapshot, while the fixed list bound caps the response size.
func (s *Store) ListUserInvites(ctx context.Context, inviterUserID int64) ([]RegistrationInvite, int64, *time.Time, error) {
	if inviterUserID <= 0 {
		return nil, 0, nil, ErrInviteInviterInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list user invites: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	usage, err := qtx.UserInviteUsage(ctx, inviterUserID)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list user invites: usage: %w", err)
	}
	rows, err := qtx.ListUserRegistrationInvitesAt(ctx, db.ListUserRegistrationInvitesAtParams{
		Now:           usage.Now,
		InviterUserID: inviterUserID,
		RowLimit:      UserInviteListLimit,
	})
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list user invites: rows: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, 0, nil, fmt.Errorf("list user invites: commit: %w", err)
	}

	var nextAvailableAt *time.Time
	if usage.OldestIssuedAt.Valid {
		next := usage.OldestIssuedAt.Time.Add(7 * 24 * time.Hour)
		nextAvailableAt = &next
	}
	out := make([]RegistrationInvite, len(rows))
	for i, row := range rows {
		out[i] = RegistrationInvite{
			ID:            row.ID,
			Handle:        nullableString(row.Handle),
			InviterUserID: row.InviterUserID,
			IssuedAt:      row.IssuedAt.Time,
			ExpiresAt:     row.ExpiresAt.Time,
			State:         InviteState(row.State),
			ConsumedAt:    inviteTimestampPtr(row.ConsumedAt),
			RevokedAt:     inviteTimestampPtr(row.RevokedAt),
		}
	}
	return out, usage.WeeklyCount, nextAvailableAt, nil
}

func newRegistrationInviteSecret() (string, []byte, error) {
	var secretBytes [inviteSecretBytes]byte
	if _, err := io.ReadFull(rand.Reader, secretBytes[:]); err != nil {
		return "", nil, err
	}
	secret := hex.EncodeToString(secretBytes[:])
	digest := sha256.Sum256([]byte(secret))
	return secret, digest[:], nil
}

func inviteLifetime(lifetimes []time.Duration) (time.Duration, error) {
	if len(lifetimes) > 1 {
		return 0, ErrInviteLifetimeInvalid
	}
	lifetime := DefaultInviteLifetime
	if len(lifetimes) == 1 && lifetimes[0] != 0 {
		lifetime = lifetimes[0]
	}
	if lifetime < time.Microsecond || lifetime > MaxInviteLifetime {
		return 0, ErrInviteLifetimeInvalid
	}
	return lifetime, nil
}

// VerifyInvite checks a secret against a live invite. Operator invites remain
// bound to their canonical handle; user invites accept any handle. All
// semantic failures return ErrInviteInvalid. The digest lookup and decision
// run in a transaction with the row lock, and expiry uses PostgreSQL time.
func (s *Store) VerifyInvite(ctx context.Context, handle, secret string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("verify invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // read-only transaction
	row, err := s.registrationInviteBySecretDigestForUpdate(ctx, tx, handle, secret)
	if err != nil {
		return fmt.Errorf("verify invite: load: %w", err)
	}
	if !inviteAcceptsHandle(row, handle, secret, true, 1) {
		return ErrInviteInvalid
	}
	return nil
}

func (s *Store) registrationInviteBySecretDigestForUpdate(ctx context.Context, tx pgx.Tx, handle, secret string) (db.RegistrationInviteBySecretDigestForUpdateRow, error) {
	digest := sha256.Sum256([]byte(secret))
	return s.q.WithTx(tx).RegistrationInviteBySecretDigestForUpdate(ctx, db.RegistrationInviteBySecretDigestForUpdateParams{
		Handle:       strings.ToLower(handle),
		SecretDigest: digest[:],
	})
}

// inviteSecretMatches hashes the presented secret and compares two fixed-size
// SHA-256 values with subtle.ConstantTimeCompare. The length check is done
// after the comparison so a malformed stored value cannot skip the comparison
// operation.
func inviteSecretMatches(secret string, stored []byte) bool {
	var storedDigest [sha256.Size]byte
	copy(storedDigest[:], stored)
	presented := sha256.Sum256([]byte(secret))
	compared := subtle.ConstantTimeCompare(presented[:], storedDigest[:])
	return len(stored) == sha256.Size && compared == 1
}

// ConsumeInvite consumes an operator invite inside tx. User invites require
// ConsumeInviteForUser so quota and listener policy are explicit.
func (s *Store) ConsumeInvite(ctx context.Context, tx pgx.Tx, handle, secret string) error {
	return s.ConsumeInviteForUser(ctx, tx, handle, secret, 0, 0, false)
}

// ConsumeInviteForUser verifies and consumes an invite inside tx. Operator
// invites are unaffected by quota or listener mode; user invites require an
// enabled invite listener and a positive configured quota. The redeemer is
// recorded atomically with the state transition.
func (s *Store) ConsumeInviteForUser(ctx context.Context, tx pgx.Tx, handle, secret string, redeemerUserID int64, userInviteQuota int, allowUserInvites bool) error {
	if tx == nil {
		return ErrInviteTransactionRequired
	}
	row, err := s.registrationInviteBySecretDigestForUpdate(ctx, tx, handle, secret)
	if err != nil {
		return fmt.Errorf("consume invite: load: %w", err)
	}
	if !inviteAcceptsHandle(row, handle, secret, allowUserInvites, userInviteQuota) {
		return ErrInviteInvalid
	}
	if row.Handle == nil && redeemerUserID <= 0 {
		return ErrInviteInvalid
	}
	return consumeLockedRegistrationInvite(ctx, s.q.WithTx(tx), row, redeemerUserID)
}

func inviteAcceptsHandle(row db.RegistrationInviteBySecretDigestForUpdateRow, handle, secret string, allowUserInvites bool, userInviteQuota int) bool {
	matches := inviteSecretMatches(secret, row.SecretDigest)
	if !row.Found || !row.Live || !matches {
		return false
	}
	if row.Handle != nil {
		return *row.Handle == strings.ToLower(handle)
	}
	return row.InviterUserID != nil && *row.InviterUserID > 0 && allowUserInvites && userInviteQuota > 0 && !row.OperatorHandleReserved
}

func consumeLockedRegistrationInvite(ctx context.Context, qtx *db.Queries, row db.RegistrationInviteBySecretDigestForUpdateRow, redeemerUserID int64) error {
	var redeemerID *int64
	if redeemerUserID > 0 {
		redeemerID = &redeemerUserID
	}
	changed, err := qtx.ConsumeRegistrationInvite(ctx, db.ConsumeRegistrationInviteParams{
		ID:               row.ID,
		SecretDigest:     row.SecretDigest,
		ConsumedByUserID: redeemerID,
	})
	if err != nil {
		return fmt.Errorf("consume invite: compare-and-swap: %w", err)
	}
	if changed != 1 {
		return ErrInviteInvalid
	}
	return nil
}

// RevokeInvite marks a live invite revoked. It is idempotent for an absent,
// expired, already-consumed, or already-revoked id. The UPDATE's row lock is
// the same decision boundary as ConsumeInvite: whichever operation acquires
// that lock first wins, and the loser observes the terminal state.
func (s *Store) RevokeInvite(ctx context.Context, id int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("revoke invite: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err := qtx.LockRegistrationInvite(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("revoke invite: lock: %w", err)
	}
	if _, err := qtx.RevokeRegistrationInvite(ctx, id); err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke invite: commit: %w", err)
	}
	return nil
}

// ListInvites returns invite metadata in issuance order. The query projects a
// naturally expired issued row as expired using database time, and selects no
// secret or digest column.
func (s *Store) ListInvites(ctx context.Context) ([]RegistrationInvite, error) {
	rows, err := s.q.ListRegistrationInvites(ctx)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	out := make([]RegistrationInvite, len(rows))
	for i, row := range rows {
		out[i] = RegistrationInvite{
			ID:            row.ID,
			Handle:        nullableString(row.Handle),
			InviterUserID: row.InviterUserID,
			IssuedAt:      row.IssuedAt.Time,
			ExpiresAt:     row.ExpiresAt.Time,
			State:         InviteState(row.State),
			ConsumedAt:    inviteTimestampPtr(row.ConsumedAt),
			RevokedAt:     inviteTimestampPtr(row.RevokedAt),
		}
	}
	return out, nil
}

// RevokeUserInvites revokes every currently live invite issued by inviterUserID
// and returns the number of rows changed. Consumed and expired invites are
// untouched.
func (s *Store) RevokeUserInvites(ctx context.Context, inviterUserID int64) (int64, error) {
	count, err := s.q.RevokeUserRegistrationInvites(ctx, inviterUserID)
	if err != nil {
		return 0, fmt.Errorf("revoke user invites: %w", err)
	}
	return count, nil
}

// ListInviteesForUser returns every account recorded as redeeming an invite
// issued by inviterUserID, in redemption order.
func (s *Store) ListInviteesForUser(ctx context.Context, inviterUserID int64) ([]RegistrationInvitee, error) {
	rows, err := s.q.ListRegistrationInvitees(ctx, inviterUserID)
	if err != nil {
		return nil, fmt.Errorf("list invitees: %w", err)
	}
	out := make([]RegistrationInvitee, 0, len(rows))
	for _, row := range rows {
		if row.InviteeUserID == nil || !row.RedeemedAt.Valid {
			return nil, fmt.Errorf("list invitees: invite %d is missing redemption attribution", row.InviteID)
		}
		out = append(out, RegistrationInvitee{
			UserID:     *row.InviteeUserID,
			RedeemedAt: row.RedeemedAt.Time,
		})
	}
	return out, nil
}

// InviterForUser reports the user invite attribution stored for redeemerID.
// found is false when no user-issued invite was recorded, including accounts
// created through open registration or an operator invite.
func (s *Store) InviterForUser(ctx context.Context, redeemerID int64) (inviterID int64, found bool, err error) {
	attributedID, err := s.q.RegistrationInviteForRedeemer(ctx, redeemerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find invite for redeemer: %w", err)
	}
	if attributedID == nil {
		return 0, false, nil
	}
	return *attributedID, true, nil
}

func registrationInviteFromInsert(row db.InsertRegistrationInviteRow) RegistrationInvite {
	return RegistrationInvite{
		ID:            row.ID,
		Handle:        nullableString(row.Handle),
		InviterUserID: row.InviterUserID,
		IssuedAt:      row.IssuedAt.Time,
		ExpiresAt:     row.ExpiresAt.Time,
		State:         InviteState(row.State),
		ConsumedAt:    inviteTimestampPtr(row.ConsumedAt),
		RevokedAt:     inviteTimestampPtr(row.RevokedAt),
	}
}

func registrationInviteFromUserInsert(row db.InsertUserRegistrationInviteRow) RegistrationInvite {
	return RegistrationInvite{
		ID:            row.ID,
		InviterUserID: row.InviterUserID,
		IssuedAt:      row.IssuedAt.Time,
		ExpiresAt:     row.ExpiresAt.Time,
		State:         InviteState(row.State),
		ConsumedAt:    inviteTimestampPtr(row.ConsumedAt),
		RevokedAt:     inviteTimestampPtr(row.RevokedAt),
	}
}

func inviteTimestampPtr(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	result := value.Time
	return &result
}
