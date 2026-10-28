package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// AdmitUsername verifies the username's phone-code hash and creates the
// provisional account in the same transaction. When requireInvite is true, it
// also consumes the invite secret stored in the phone-code row. Every
// account-side write is rolled back when any later step fails, including the
// invite consumption when required.
func (s *Store) AdmitUsername(ctx context.Context, handle, phoneCodeHash string, authKeyID int64, firstName, lastName string, requireInvite bool) (User, error) {
	return s.AdmitUsernameWithMetadata(ctx, handle, phoneCodeHash, authKeyID, firstName, lastName, requireInvite, AuthorizationMetadata{})
}

// AdmitUsernameWithMetadata atomically creates a username-mode account and
// binds the initiating auth key with its connection-local metadata.
func (s *Store) AdmitUsernameWithMetadata(ctx context.Context, handle, phoneCodeHash string, authKeyID int64, firstName, lastName string, requireInvite bool, metadata AuthorizationMetadata) (User, error) {
	return s.AdmitUsernameWithInvitePolicy(ctx, handle, phoneCodeHash, authKeyID, firstName, lastName, InviteAdmissionPolicy{
		RequireInvite: requireInvite,
	}, metadata)
}

// AdmitUsernameWithInvitePolicy atomically creates a username-mode account,
// binds the initiating auth key, and consumes the required invite under the
// listener's policy.
func (s *Store) AdmitUsernameWithInvitePolicy(ctx context.Context, handle, phoneCodeHash string, authKeyID int64, firstName, lastName string, invitePolicy InviteAdmissionPolicy, metadata AuthorizationMetadata) (User, error) {
	handle = strings.ToLower(handle)
	metadataParams := s.authorizationMetadataParams(metadata)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("admit username: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	// Serialize all attempts for the connection key before reading any handle,
	// invite, or code-dependent state. A pending password challenge is allowed
	// here because the key is still unbound; the bind below clears it in this
	// same transaction. A bound or missing key fails before account-side work.
	if _, err := qtx.LockUnboundAuthKey(ctx, authKeyID); errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrAuthKeyNotFound
	} else if err != nil {
		return User{}, fmt.Errorf("admit username: lock auth key: %w", err)
	}

	codeRow, err := qtx.GetCodeByHashAndPhone(ctx, db.GetCodeByHashAndPhoneParams{
		CodeHash: phoneCodeHash,
		Phone:    handle,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return User{}, ErrCodeInvalid
	case err != nil:
		return User{}, fmt.Errorf("admit username: check code hash: %w", err)
	}
	if err := validateCodeHash(codeRow); err != nil {
		return User{}, err
	}
	existingUser, err := qtx.GetUserByUsernameWithLoginMode(ctx, handle)
	if err == nil {
		if existingUser.Kind == UserKindBot {
			return User{}, ErrBotAccount
		}
		return User{}, ErrUsernameOccupied
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("admit username: check username owner: %w", err)
	}
	if _, err := qtx.GetUsernameByHandle(ctx, handle); err == nil {
		return User{}, ErrUsernameOccupied
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return User{}, fmt.Errorf("admit username: check username: %w", err)
	}
	var inviteRow *db.RegistrationInviteBySecretDigestForUpdateRow
	if invitePolicy.RequireInvite {
		row, err := s.registrationInviteBySecretDigestForUpdate(ctx, tx, handle, codeRow.Code)
		if err != nil {
			return User{}, fmt.Errorf("admit username: load invite: %w", err)
		}
		if !inviteAcceptsHandle(row, handle, codeRow.Code, invitePolicy.AllowUserInvites, invitePolicy.UserInviteQuota) {
			return User{}, ErrInviteInvalid
		}
		inviteRow = &row
	}
	if rows, err := qtx.ConsumeCodeForUsername(ctx, db.ConsumeCodeForUsernameParams{
		Phone:    handle,
		CodeHash: phoneCodeHash,
	}); err != nil {
		return User{}, fmt.Errorf("admit username: consume code: %w", err)
	} else if rows != 1 {
		return User{}, ErrCodeInvalid
	}

	u, err := qtx.CreateUsernameUser(ctx, db.CreateUsernameUserParams{
		FirstName: firstName,
		LastName:  lastName,
	})
	if err != nil {
		return User{}, fmt.Errorf("admit username: create user: %w", err)
	}
	if inviteRow != nil {
		if err := consumeLockedRegistrationInvite(ctx, qtx, *inviteRow, u.ID); err != nil {
			return User{}, fmt.Errorf("admit username: consume invite: %w", err)
		}
	}
	if err := qtx.EnsureUpdateState(ctx, u.ID); err != nil {
		return User{}, fmt.Errorf("admit username: ensure update state: %w", err)
	}

	_, err = qtx.ClaimUsername(ctx, db.ClaimUsernameParams{
		Handle:    handle,
		OwnerType: "user",
		OwnerID:   u.ID,
	})
	if err != nil {
		if isUsernameClaimConflict(err) {
			claimedUser, lookupErr := qtx.GetUserByUsernameWithLoginMode(ctx, handle)
			if lookupErr == nil && claimedUser.Kind == UserKindBot {
				return User{}, ErrBotAccount
			}
			if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
				return User{}, fmt.Errorf("admit username: resolve conflicting owner: %w", lookupErr)
			}
			return User{}, ErrUsernameOccupied
		}
		return User{}, fmt.Errorf("admit username: claim username: %w", err)
	}
	if _, err := qtx.SetUsername(ctx, db.SetUsernameParams{ID: u.ID, Username: &handle}); err != nil {
		return User{}, fmt.Errorf("admit username: set username: %w", err)
	}
	if err := s.electServerAdministrator(ctx, qtx, u.ID); err != nil {
		return User{}, fmt.Errorf("admit username: election: %w", err)
	}

	rows, err := qtx.BindAuthKeyUserWithMetadata(ctx, db.BindAuthKeyUserWithMetadataParams{
		ID: authKeyID, UserID: &u.ID, DeviceModel: metadataParams.deviceModel, Platform: metadataParams.platform,
		SystemVersion: metadataParams.systemVersion, ApiID: metadataParams.apiID,
		AppVersion: metadataParams.appVersion, Ip: metadataParams.ip,
	})
	if err != nil {
		return User{}, fmt.Errorf("admit username: bind auth key with metadata: %w", err)
	}
	if rows == 0 {
		return User{}, ErrAuthKeyNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("admit username: commit: %w", err)
	}
	return UserFromCreateUsernameUser(u), nil
}
