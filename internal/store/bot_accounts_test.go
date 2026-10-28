package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBotAccountSchemaProtectsKindsAndAuthKeyBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open inspection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	human, err := st.CreateUser(ctx, "15551234567")
	if err != nil {
		t.Fatalf("create legacy human: %v", err)
	}
	humanID := human.ID
	var kind string
	if err := pool.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, humanID).Scan(&kind); err != nil {
		t.Fatalf("read default user kind: %v", err)
	}
	if kind != "human" {
		t.Fatalf("legacy user's kind = %q, want human", kind)
	}

	var botID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO users (phone, login_mode, first_name, kind)
		VALUES (NULL, 'username', 'Weather', 'bot')
		RETURNING id
	`).Scan(&botID); err != nil {
		t.Fatalf("insert bot: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET kind = 'human' WHERE id = $1`, botID); !isCheckViolation(err) {
		t.Fatalf("change bot kind error = %v, want check-constraint error", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO bot_tokens (user_id, token_hash)
		VALUES ($1, decode(repeat('00', 32), 'hex'))
	`, humanID); !isForeignKeyViolation(err) {
		t.Fatalf("issue bot token for human error = %v, want foreign-key constraint error", err)
	}

	for _, tc := range []struct {
		name  string
		query string
	}{
		{name: "bound user", query: `UPDATE auth_keys SET user_id = $2 WHERE id = $1`},
		{name: "pending user", query: `UPDATE auth_keys SET pending_user_id = $2 WHERE id = $1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const keyID = int64(0x12345678)
			if err := st.SaveAuthKey(ctx, keyID, []byte("auth key")); err != nil {
				t.Fatalf("save auth key: %v", err)
			}
			if _, err := pool.Exec(ctx, tc.query, keyID, botID); !isForeignKeyViolation(err) {
				t.Fatalf("bind bot error = %v, want foreign-key constraint error", err)
			}
		})
	}
}

func isCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func TestBotTokenLifecycleStoresOnlyHashAndNormalizesFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open inspection pool: %v", err)
	}
	t.Cleanup(pool.Close)

	bot, token, err := st.CreateBot(ctx, "Weather_Bot", "Weather")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if bot.ID <= 0 || bot.Username != "weather_bot" || bot.FirstName != "Weather" {
		t.Fatalf("created bot = %+v, want normalized username and supplied profile", bot)
	}
	idText, secretText, ok := strings.Cut(token, ":")
	if !ok || idText != strconv.FormatInt(bot.ID, 10) {
		t.Fatalf("token = %q, want <bot id>:<secret>", token)
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretText)
	if err != nil || len(secret) != 32 {
		t.Fatalf("token secret decoded length = %d, error = %v; want 32 random bytes", len(secret), err)
	}
	secretHash := sha256.Sum256(secret)
	var storedHash []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM bot_tokens WHERE user_id = $1 AND revoked_at IS NULL`, bot.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored token hash: %v", err)
	}
	if !bytes.Equal(storedHash, secretHash[:]) {
		t.Fatalf("stored token hash = %x, want SHA-256 %x", storedHash, secretHash)
	}

	resolvedID, err := st.BotIDByToken(ctx, token)
	if err != nil || resolvedID != bot.ID {
		t.Fatalf("lookup created token = (%d, %v), want (%d, nil)", resolvedID, err, bot.ID)
	}
	for _, candidate := range []string{
		"malformed",
		"0:" + secretText,
		fmt.Sprintf("%d:%s", bot.ID+1000000, secretText),
		fmt.Sprintf("%d:%s", bot.ID, "short"),
	} {
		if _, err := st.BotIDByToken(ctx, candidate); !errors.Is(err, store.ErrBotTokenNotFound) {
			t.Errorf("lookup %q error = %v, want the shared not-found result", candidate, err)
		}
	}

	rotated, err := st.RotateBotToken(ctx, bot.ID)
	if err != nil {
		t.Fatalf("rotate bot token: %v", err)
	}
	if rotated == token {
		t.Fatal("rotated token equals old token")
	}
	if _, err := st.BotIDByToken(ctx, token); !errors.Is(err, store.ErrBotTokenNotFound) {
		t.Fatalf("lookup old token after rotate error = %v, want the shared not-found result", err)
	}
	resolvedID, err = st.BotIDByToken(ctx, rotated)
	if err != nil || resolvedID != bot.ID {
		t.Fatalf("lookup rotated token = (%d, %v), want (%d, nil)", resolvedID, err, bot.ID)
	}

	if err := st.RevokeBotToken(ctx, bot.ID); err != nil {
		t.Fatalf("revoke bot token: %v", err)
	}
	if _, err := st.BotIDByToken(ctx, rotated); !errors.Is(err, store.ErrBotTokenNotFound) {
		t.Fatalf("lookup revoked token error = %v, want the shared not-found result", err)
	}
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM bot_tokens WHERE user_id = $1`, bot.ID).Scan(&storedHash); err != nil {
		t.Fatalf("read revoked token row: %v", err)
	}
	if storedHash != nil {
		t.Fatalf("revoked token hash = %x, want NULL", storedHash)
	}
}
