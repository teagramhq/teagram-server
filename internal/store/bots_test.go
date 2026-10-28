package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCreateBotPersistsOnlyTheSecretHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	secret := bytes.Repeat([]byte{0xa5}, store.BotTokenSecretSize)
	bot, err := s.CreateBot(ctx, "Echo", "echo_test_bot", secret)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	var storedHash []byte
	var tokenColumns string
	if err := conn.QueryRow(ctx, `SELECT secret_hash, to_jsonb(bot_tokens)::text FROM bot_tokens WHERE user_id = $1`, bot.ID).Scan(&storedHash, &tokenColumns); err != nil {
		t.Fatalf("read stored bot token: %v", err)
	}
	wantHash := sha256.Sum256(secret)
	if !bytes.Equal(storedHash, wantHash[:]) {
		t.Fatal("stored bot token digest is not SHA-256(secret)")
	}
	for _, plaintext := range []string{
		base64.RawURLEncoding.EncodeToString(secret),
		hex.EncodeToString(secret),
	} {
		if strings.Contains(tokenColumns, plaintext) {
			t.Fatal("bot token row contains the plaintext secret")
		}
	}
}

func TestBotKindIsImmutableAndAuthKeysCannotReferenceBots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})

	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close administrator election: %v", err)
	}
	var humanID int64
	if err := conn.QueryRow(ctx, `INSERT INTO users (phone) VALUES ('15550001111') RETURNING id`).Scan(&humanID); err != nil {
		t.Fatalf("insert human user: %v", err)
	}
	var humanKind string
	if err := conn.QueryRow(ctx, `SELECT kind FROM users WHERE id = $1`, humanID).Scan(&humanKind); err != nil {
		t.Fatalf("read human kind: %v", err)
	}
	if humanKind != "human" {
		t.Fatalf("new human kind = %q, want human", humanKind)
	}
	var botID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO users (phone, login_mode, kind, first_name)
		VALUES (NULL, 'username', 'bot', 'Echo')
		RETURNING id`).Scan(&botID); err != nil {
		t.Fatalf("insert bot: %v", err)
	}

	if _, err := conn.Exec(ctx, `UPDATE users SET kind = 'human' WHERE id = $1`, botID); err == nil {
		t.Fatal("changing a bot user kind succeeded")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("changing bot kind error = %v, want check constraint error", err)
		}
	}

	for i, column := range []string{"user_id", "pending_user_id"} {
		keyID := int64(i + 1)
		if _, err := conn.Exec(ctx, `INSERT INTO auth_keys (id, key_value) VALUES ($1, $2)`, keyID, []byte("auth-key")); err != nil {
			t.Fatalf("insert auth key %d: %v", keyID, err)
		}
		if _, err := conn.Exec(ctx, `UPDATE auth_keys SET `+column+` = $2 WHERE id = $1`, keyID, botID); err == nil {
			t.Errorf("setting auth_keys.%s to bot %d succeeded", column, botID)
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
				t.Errorf("setting auth_keys.%s error = %v, want foreign key error", column, err)
			}
		}
	}
}

func TestPasswordPromotionRejectsBotAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	bot, err := s.CreateBot(ctx, "Echo", "promote_test_bot", bytes.Repeat([]byte{0x42}, store.BotTokenSecretSize))
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	const authKeyID = 0x2820
	if err := s.SaveAuthKey(ctx, authKeyID, []byte("unbound auth key")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.PromotePendingUser(ctx, authKeyID, bot.ID, time.Now(), time.Minute); !errors.Is(err, store.ErrBotAccount) {
		t.Fatalf("promote bot account error = %v, want ErrBotAccount", err)
	}
	key, found, err := s.AuthKeyByID(ctx, authKeyID)
	if err != nil {
		t.Fatalf("read auth key: %v", err)
	}
	if !found || key.UserID != 0 || key.PendingUserID != 0 {
		t.Fatalf("auth key after rejected promotion = %+v, found %t; want unbound", key, found)
	}
}
