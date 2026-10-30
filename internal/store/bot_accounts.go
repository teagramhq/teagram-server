package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	UserKindHuman   = "human"
	UserKindBot     = "bot"
	botSecretSize   = 32
	maxBotNameRunes = 64
)

var (
	ErrBotAccount            = errors.New("bot account")
	ErrBotTokenNotFound      = errors.New("bot token not found")
	ErrBotUsernameInvalid    = errors.New("bot username invalid")
	ErrBotRecipientForbidden = errors.New("bot recipient is not eligible")
	ErrBotBlockedByUser      = errors.New("user blocked bot")
	botUsernameRE            = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{1,31}$`)
)

// BotAccount is the public profile created by an operator.
type BotAccount struct {
	ID        int64
	Username  string
	FirstName string
}

// CreateBot creates a bot identity and its first token in one transaction.
// The token is returned only to the caller; its SHA-256 digest is persisted.
func (s *Store) CreateBot(ctx context.Context, username, firstName string) (BotAccount, string, error) {
	handle := strings.ToLower(username)
	if !botUsernameRE.MatchString(handle) {
		return BotAccount{}, "", ErrBotUsernameInvalid
	}
	if !validBotFirstName(firstName) {
		return BotAccount{}, "", errors.New("bot first name invalid")
	}

	secret, tokenHash, err := newBotSecret()
	if err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot token: %w", err)
	}
	defer clear(secret[:])
	defer clear(tokenHash[:])

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	botID, err := qtx.CreateBotUser(ctx, firstName)
	if err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: insert user: %w", err)
	}
	if err := qtx.EnsureUpdateState(ctx, botID); err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: ensure update state: %w", err)
	}
	_, err = qtx.ClaimUsername(ctx, db.ClaimUsernameParams{
		Handle:    handle,
		OwnerType: "user",
		OwnerID:   botID,
	})
	if err != nil {
		if isUsernameClaimConflict(err) {
			return BotAccount{}, "", ErrUsernameOccupied
		}
		return BotAccount{}, "", fmt.Errorf("create bot: claim username: %w", err)
	}
	if _, err := qtx.SetUsername(ctx, db.SetUsernameParams{ID: botID, Username: &handle}); err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: set username: %w", err)
	}
	if err := qtx.InsertBotToken(ctx, db.InsertBotTokenParams{UserID: botID, TokenHash: tokenHash[:]}); err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: store token hash: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return BotAccount{}, "", fmt.Errorf("create bot: commit: %w", err)
	}

	return BotAccount{ID: botID, Username: handle, FirstName: firstName}, formatBotToken(botID, secret[:]), nil
}

// RotateBotToken replaces the active token with a freshly generated secret.
func (s *Store) RotateBotToken(ctx context.Context, botID int64) (string, error) {
	if botID <= 0 {
		return "", ErrBotTokenNotFound
	}
	secret, tokenHash, err := newBotSecret()
	if err != nil {
		return "", fmt.Errorf("rotate bot token: %w", err)
	}
	defer clear(secret[:])
	defer clear(tokenHash[:])

	rows, err := s.q.RotateBotToken(ctx, db.RotateBotTokenParams{UserID: botID, TokenHash: tokenHash[:]})
	if err != nil {
		return "", fmt.Errorf("rotate bot token: %w", err)
	}
	if rows != 1 {
		return "", ErrBotTokenNotFound
	}
	return formatBotToken(botID, secret[:]), nil
}

// RevokeBotToken invalidates the active token and removes its stored digest.
func (s *Store) RevokeBotToken(ctx context.Context, botID int64) error {
	if botID <= 0 {
		return ErrBotTokenNotFound
	}
	rows, err := s.q.RevokeBotToken(ctx, botID)
	if err != nil {
		return fmt.Errorf("revoke bot token: %w", err)
	}
	if rows != 1 {
		return ErrBotTokenNotFound
	}
	return nil
}

// BotIDByToken returns the bot id for a valid token. Invalid, unknown, and
// revoked tokens all return ErrBotTokenNotFound.
func (s *Store) BotIDByToken(ctx context.Context, token string) (int64, error) {
	botID, secret, ok := parseBotToken(token)
	if !ok {
		return 0, ErrBotTokenNotFound
	}
	defer clear(secret)
	wantHash := sha256.Sum256(secret)

	storedHash, err := s.q.ActiveBotTokenHash(ctx, botID)
	if errors.Is(err, pgx.ErrNoRows) {
		var zeroHash [sha256.Size]byte
		subtle.ConstantTimeCompare(wantHash[:], zeroHash[:])
		return 0, ErrBotTokenNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("lookup bot token: %w", err)
	}
	if subtle.ConstantTimeCompare(wantHash[:], storedHash) != 1 {
		return 0, ErrBotTokenNotFound
	}
	return botID, nil
}

// UserKind returns the immutable kind for an existing account.
func (s *Store) UserKind(ctx context.Context, userID int64) (string, bool, error) {
	kind, err := s.q.UserKindByID(ctx, userID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("user kind: %w", err)
	default:
		return kind, true, nil
	}
}

func newBotSecret() ([botSecretSize]byte, [sha256.Size]byte, error) {
	var secret [botSecretSize]byte
	if _, err := io.ReadFull(rand.Reader, secret[:]); err != nil {
		return [botSecretSize]byte{}, [sha256.Size]byte{}, fmt.Errorf("read random secret: %w", err)
	}
	digest := sha256.Sum256(secret[:])
	return secret, digest, nil
}

func formatBotToken(botID int64, secret []byte) string {
	return strconv.FormatInt(botID, 10) + ":" + base64.RawURLEncoding.EncodeToString(secret)
}

func parseBotToken(token string) (int64, []byte, bool) {
	botIDText, secretText, ok := strings.Cut(token, ":")
	if !ok || strings.Contains(secretText, ":") {
		return 0, nil, false
	}
	botID, err := strconv.ParseInt(botIDText, 10, 64)
	if err != nil || botID <= 0 || strconv.FormatInt(botID, 10) != botIDText {
		return 0, nil, false
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(secretText)
	if err != nil || len(secret) != botSecretSize || base64.RawURLEncoding.EncodeToString(secret) != secretText {
		clear(secret)
		return 0, nil, false
	}
	return botID, secret, true
}

func validBotFirstName(name string) bool {
	return utf8.ValidString(name) && !strings.ContainsRune(name, '\x00') && utf8.RuneCountInString(name) <= maxBotNameRunes
}
