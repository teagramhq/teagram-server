package e2e_test

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBotCannotSignInOrStagePasswordLogin(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test database: %v", err)
		}
	})
	const username = "botlogin_test_bot"
	const phone = "15551234567"
	const phoneIdentifier = "+15551234567"
	if _, err := conn.Exec(ctx, `UPDATE server_administration SET election_closed = true WHERE singleton_id = 1`); err != nil {
		t.Fatalf("close administrator election: %v", err)
	}
	var botID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO users (phone, login_mode, kind, first_name)
		VALUES ($1, 'username', 'bot', 'Echo')
		RETURNING id`, phone).Scan(&botID); err != nil {
		t.Fatalf("insert bot: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO usernames (handle, owner_type, owner_id) VALUES ($1, 'user', $2)`, username, botID); err != nil {
		t.Fatalf("claim bot username: %v", err)
	}
	verifier, salt1, salt2, err := testComputeSRPVerifier([]byte("bot-password"))
	if err != nil {
		t.Fatalf("compute bot password verifier: %v", err)
	}
	if err := st.UpsertPassword(ctx, store.UserPassword{UserID: botID, Salt1: salt1, Salt2: salt2, Verifier: verifier}); err != nil {
		t.Fatalf("plant bot password verifier: %v", err)
	}

	const dcID = 2
	codes := newCodeSink()
	ln := mustListen(t, ctx, "127.0.0.1:0")
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener addr type = %T", ln.Addr())
	}
	clientPort := addr.Port
	stop := bootServerWithRegMode(t, ctx, key, dcID, st, codes.Logger(), ln, config.RegistrationInvite)
	t.Cleanup(stop)

	client := newUsernameClient(clientPort, key, dcID, nil)
	if err := client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		_, err := api.AuthSignIn(ctx, &tg.AuthSignInRequest{PhoneNumber: phoneIdentifier})
		if !isRPCMessage(err, "BOT_LOGIN_FORBIDDEN") {
			return unexpectedRPCError("phone signIn", "BOT_LOGIN_FORBIDDEN", err)
		}

		hash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return fmt.Errorf("sendCode for bot username: %w", err)
		}
		if _, err := signInUsername(ctx, api, username, hash, ""); !isRPCMessage(err, "BOT_LOGIN_FORBIDDEN") {
			return unexpectedRPCError("username signIn", "BOT_LOGIN_FORBIDDEN", err)
		}
		if _, err := signUpUsername(ctx, api, username, hash, "Echo", ""); !isRPCMessage(err, "BOT_LOGIN_FORBIDDEN") {
			return unexpectedRPCError("bot signUp", "BOT_LOGIN_FORBIDDEN", err)
		}

		_, err = api.AuthCheckPassword(ctx, &tg.InputCheckPasswordSRP{SRPID: 1, A: []byte{1}, M1: []byte{1}})
		if !isRPCMessage(err, "AUTH_KEY_UNREGISTERED") {
			return unexpectedRPCError("checkPassword", "AUTH_KEY_UNREGISTERED after refused signIn", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("bot login refusal: %v", err)
	}

	var bound, pending int64
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE user_id = $1), count(*) FILTER (WHERE pending_user_id = $1)
		FROM auth_keys`, botID).Scan(&bound, &pending); err != nil {
		t.Fatalf("read auth key bindings: %v", err)
	}
	if bound != 0 || pending != 0 {
		t.Fatalf("bot auth key bindings = bound:%d pending:%d, want none", bound, pending)
	}
}

func unexpectedRPCError(operation, want string, err error) error {
	if err == nil {
		return fmt.Errorf("%s returned no error, want %s", operation, want)
	}
	return fmt.Errorf("%s error, want %s: %w", operation, want, err)
}
