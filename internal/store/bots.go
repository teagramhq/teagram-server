package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const BotTokenSecretSize = 32

var (
	ErrBotNotFound        = errors.New("bot not found")
	ErrBotAccount         = errors.New("bot account cannot use MTProto authentication")
	ErrBotTokenNotFound   = errors.New("bot token not found")
	ErrBotUsernameInvalid = errors.New("bot username must end in bot")
)

// CreateBot inserts a bot account, claims its username, provisions update
// state, and stores only the SHA-256 digest of the token secret in one
// transaction.
func (s *Store) CreateBot(ctx context.Context, displayName, username string, secret []byte) (User, error) {
	handle, err := normalizeBotUsername(username)
	if err != nil {
		return User{}, err
	}
	if len(secret) != BotTokenSecretSize {
		return User{}, fmt.Errorf("create bot: token secret must be %d bytes", BotTokenSecretSize)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("create bot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	row, err := qtx.CreateBotUser(ctx, displayName)
	if err != nil {
		return User{}, fmt.Errorf("create bot: insert user: %w", err)
	}
	if _, err := qtx.ClaimUsername(ctx, db.ClaimUsernameParams{
		Handle:    handle,
		OwnerType: "user",
		OwnerID:   row.ID,
	}); err != nil {
		if isUsernameClaimConflict(err) {
			return User{}, ErrUsernameOccupied
		}
		return User{}, fmt.Errorf("create bot: claim username: %w", err)
	}
	if _, err := qtx.SetUsername(ctx, db.SetUsernameParams{ID: row.ID, Username: &handle}); err != nil {
		return User{}, fmt.Errorf("create bot: set username: %w", err)
	}
	if err := qtx.EnsureUpdateState(ctx, row.ID); err != nil {
		return User{}, fmt.Errorf("create bot: ensure update state: %w", err)
	}
	secretHash := sha256.Sum256(secret)
	if err := qtx.UpsertBotToken(ctx, db.UpsertBotTokenParams{UserID: row.ID, SecretHash: secretHash[:]}); err != nil {
		return User{}, fmt.Errorf("create bot: store token digest: %w", err)
	}
	if err := closeServerAdministratorElection(ctx, qtx); err != nil {
		return User{}, fmt.Errorf("create bot: close administrator election: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("create bot: commit: %w", err)
	}

	user := User{
		ID:        row.ID,
		FirstName: row.FirstName,
		LastName:  row.LastName,
		Username:  &handle,
	}
	return user, nil
}

// RotateBotToken replaces the active token digest for the bot named by
// username. The secret is never retained by the store.
func (s *Store) RotateBotToken(ctx context.Context, username string, secret []byte) (int64, error) {
	handle, err := normalizeBotUsername(username)
	if err != nil {
		return 0, err
	}
	if len(secret) != BotTokenSecretSize {
		return 0, fmt.Errorf("rotate bot token: token secret must be %d bytes", BotTokenSecretSize)
	}

	userID, err := s.q.BotIDByUsername(ctx, handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrBotNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("rotate bot token: find bot: %w", err)
	}
	secretHash := sha256.Sum256(secret)
	if err := s.q.UpsertBotToken(ctx, db.UpsertBotTokenParams{UserID: userID, SecretHash: secretHash[:]}); err != nil {
		return 0, fmt.Errorf("rotate bot token: store token digest: %w", err)
	}
	return userID, nil
}

// RevokeBotToken deletes the active token digest for the bot named by
// username.
func (s *Store) RevokeBotToken(ctx context.Context, username string) error {
	handle, err := normalizeBotUsername(username)
	if err != nil {
		return err
	}
	userID, err := s.q.BotIDByUsername(ctx, handle)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrBotNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke bot token: find bot: %w", err)
	}
	deleted, err := s.q.DeleteBotToken(ctx, userID)
	if err != nil {
		return fmt.Errorf("revoke bot token: delete token: %w", err)
	}
	if deleted == 0 {
		return ErrBotTokenNotFound
	}
	return nil
}

// LookupBotToken resolves an active token to its bot. Malformed, unknown,
// rotated, and revoked tokens all return ErrBotTokenNotFound.
func (s *Store) LookupBotToken(ctx context.Context, token string) (User, error) {
	userID, candidateHash, ok := parseBotToken(token)
	if !ok {
		var zero [sha256.Size]byte
		subtle.ConstantTimeCompare(zero[:], zero[:])
		return User{}, ErrBotTokenNotFound
	}

	storedHash, err := s.q.ActiveBotTokenHashByID(ctx, userID)
	if errors.Is(err, pgx.ErrNoRows) {
		var zero [sha256.Size]byte
		subtle.ConstantTimeCompare(candidateHash[:], zero[:])
		return User{}, ErrBotTokenNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("bot token lookup: read token digest: %w", err)
	}
	if len(storedHash) != sha256.Size {
		return User{}, fmt.Errorf("bot token lookup: stored digest has invalid length %d", len(storedHash))
	}
	if subtle.ConstantTimeCompare(storedHash, candidateHash[:]) != 1 {
		return User{}, ErrBotTokenNotFound
	}

	user, found, err := s.UserByID(ctx, userID)
	if err != nil {
		return User{}, fmt.Errorf("bot token lookup: load bot: %w", err)
	}
	if !found {
		return User{}, ErrBotTokenNotFound
	}
	return user, nil
}

// UserKindByPhone returns the account kind for a phone number.
func (s *Store) UserKindByPhone(ctx context.Context, phone string) (string, bool, error) {
	normalized := NormalizePhone(phone)
	kind, err := s.q.UserKindByPhone(ctx, &normalized)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("user kind by phone: %w", err)
	default:
		return kind, true, nil
	}
}

func normalizeBotUsername(username string) (string, error) {
	username = strings.TrimPrefix(username, "@")
	if len(username) < 3 || !strings.EqualFold(username[len(username)-3:], "bot") {
		return "", ErrBotUsernameInvalid
	}
	return strings.ToLower(username), nil
}

func parseBotToken(token string) (int64, [sha256.Size]byte, bool) {
	var hash [sha256.Size]byte
	idText, secretText, ok := strings.Cut(token, ":")
	if !ok || strings.ContainsRune(secretText, ':') {
		return 0, hash, false
	}
	userID, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || userID <= 0 || strconv.FormatInt(userID, 10) != idText {
		return 0, hash, false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(secretText)
	if err != nil || len(secret) != BotTokenSecretSize || base64.RawURLEncoding.EncodeToString(secret) != secretText {
		clear(secret)
		return 0, hash, false
	}
	hash = sha256.Sum256(secret)
	clear(secret)
	return userID, hash, true
}
