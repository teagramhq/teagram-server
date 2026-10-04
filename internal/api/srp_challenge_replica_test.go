package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCheckPasswordCrossReplica(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	firstStore, dsn := openStoreDSN(t)
	secondStore, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open second store: %v", err)
	}
	t.Cleanup(func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	})

	user, err := firstStore.CreateUser(ctx, "+15551294801")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	const password = "replica password"
	algo := &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
		Salt1: make([]byte, 32),
		Salt2: make([]byte, 32),
		G:     srp.G,
		P:     srp.PBytes(),
	}
	verifier, err := auth.NewPasswordHash([]byte(password), algo)
	if err != nil {
		t.Fatalf("make verifier: %v", err)
	}
	if err := firstStore.UpsertPassword(ctx, store.UserPassword{
		UserID: user.ID, Salt1: algo.Salt1, Salt2: algo.Salt2, Verifier: verifier,
	}); err != nil {
		t.Fatalf("store password: %v", err)
	}

	var authKeyID [8]byte
	authKeyID[7] = 0x48
	keyID := mtproto.AuthKeyIDInt64(authKeyID)
	if err := firstStore.SaveAuthKey(ctx, keyID, []byte("test auth key")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := firstStore.SetPendingUser(ctx, keyID, user.ID); err != nil {
		t.Fatalf("set pending user: %v", err)
	}

	firstReplica := api.SharedHandlersForTest(firstStore)
	secondReplica := api.SharedHandlersForTest(secondStore)
	var getPasswordBuf bin.Buffer
	if err := (&tg.AccountGetPasswordRequest{}).Encode(&getPasswordBuf); err != nil {
		t.Fatalf("encode getPassword: %v", err)
	}
	passwordResult, err := api.HandleGetPassword(firstReplica, &mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, Buf: &getPasswordBuf,
	})
	if err != nil {
		t.Fatalf("getPassword on first replica: %v", err)
	}
	passwordState, ok := passwordResult.(*tg.AccountPassword)
	if !ok || !passwordState.HasPassword {
		t.Fatalf("getPassword result = %T, want password state", passwordResult)
	}
	proof, err := auth.PasswordHash([]byte(password), passwordState.SRPID, passwordState.SRPB,
		passwordState.SecureRandom, passwordState.CurrentAlgo)
	if err != nil {
		t.Fatalf("make SRP proof: %v", err)
	}

	var checkPasswordBuf bin.Buffer
	if err := (&tg.AuthCheckPasswordRequest{Password: proof}).Encode(&checkPasswordBuf); err != nil {
		t.Fatalf("encode checkPassword: %v", err)
	}
	result, err := api.HandleCheckPassword(secondReplica, &mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, Buf: &checkPasswordBuf,
	})
	if err != nil {
		t.Fatalf("checkPassword on second replica: %v", err)
	}
	if authorization, ok := result.(*tg.AuthAuthorization); !ok || authorization.User.GetID() != user.ID {
		t.Fatalf("checkPassword result = %T, want authorization for user %d", result, user.ID)
	}

	var replayBuf bin.Buffer
	if err := (&tg.AuthCheckPasswordRequest{Password: proof}).Encode(&replayBuf); err != nil {
		t.Fatalf("encode replayed checkPassword: %v", err)
	}
	_, err = api.HandleCheckPassword(secondReplica, &mtproto.Request{
		Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &replayBuf,
	})
	var replayRPC *tgerr.Error
	if !errors.As(err, &replayRPC) || replayRPC.Message != "SRP_ID_INVALID" {
		t.Fatalf("replayed checkPassword error = %v, want SRP_ID_INVALID", err)
	}

	var secondGetBuf bin.Buffer
	if err := (&tg.AccountGetPasswordRequest{}).Encode(&secondGetBuf); err != nil {
		t.Fatalf("encode second getPassword: %v", err)
	}
	secondPasswordResult, err := api.HandleGetPassword(firstReplica, &mtproto.Request{
		Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &secondGetBuf,
	})
	if err != nil {
		t.Fatalf("getPassword for expiry check: %v", err)
	}
	secondPasswordState, ok := secondPasswordResult.(*tg.AccountPassword)
	if !ok || !secondPasswordState.HasPassword {
		t.Fatalf("second getPassword result = %T, want password state", secondPasswordResult)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to expire challenge: %v", err)
	}
	if _, err := conn.Exec(ctx,
		`UPDATE srp_challenges SET expires_at = now() - interval '1 second' WHERE srp_id = $1`, secondPasswordState.SRPID,
	); err != nil {
		if closeErr := conn.Close(ctx); closeErr != nil {
			t.Errorf("close expiry connection: %v", closeErr)
		}
		t.Fatalf("expire challenge: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Fatalf("close expiry connection: %v", err)
	}
	var expiredBuf bin.Buffer
	if err := (&tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordSRP{
		SRPID: secondPasswordState.SRPID, A: make([]byte, srp.PadLen), M1: make([]byte, srp.PadLen),
	}}).Encode(&expiredBuf); err != nil {
		t.Fatalf("encode expired checkPassword: %v", err)
	}
	_, err = api.HandleCheckPassword(secondReplica, &mtproto.Request{
		Ctx: ctx, UserID: user.ID, AuthKeyID: authKeyID, Buf: &expiredBuf,
	})
	var expiredRPC *tgerr.Error
	if !errors.As(err, &expiredRPC) || expiredRPC.Message != "SRP_ID_INVALID" {
		t.Fatalf("expired checkPassword error = %v, want SRP_ID_INVALID", err)
	}
}
