package api

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	gotdsrp "github.com/gotd/td/crypto/srp"
	gotdauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	tsrp "github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func tailnetRequest(r *mtproto.Request) *mtproto.Request {
	mtproto.MarkRequestTailnetForTesting(r)
	return r
}

func TestTailnetRequestBypassesCallerRateLimitHelpers(t *testing.T) {
	t.Parallel()

	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	h := &handlers{
		rateLimitSendCodeIP: store.SendCodeIPLimits{Calls: limit, Phones: limit},
	}
	r := tailnetRequest(&mtproto.Request{
		Ctx:        context.Background(),
		UserID:     17,
		ClientAddr: netip.Addr{},
	})

	if err := h.checkRateLimitCost(r, "tailnet-account", limit, 1); err != nil {
		t.Fatalf("tailnet account rate limit = %v, want allow", err)
	}
	if err := h.checkRateLimitCostWithDeadline(r, context.Background(), "tailnet-deadline", limit, 1); err != nil {
		t.Fatalf("tailnet deadline account rate limit = %v, want allow", err)
	}
	if err := h.checkRateLimitWindow(r, "tailnet-window", limit); err != nil {
		t.Fatalf("tailnet window account rate limit = %v, want allow", err)
	}
	if err := h.checkAndChargeRateLimitIP(r, "tailnet-ip", limit); err != nil {
		t.Fatalf("tailnet IP rate limit = %v, want allow", err)
	}
	reservation, denied, err := h.reserveRateLimitIP(r, "tailnet-ip-reserve", limit)
	if err != nil || denied != nil || reservation != nil {
		t.Fatalf("tailnet IP reserve = (%v, %v, %v), want no reservation or denial", reservation, denied, err)
	}
	if err := h.checkSendCodeIP(r, "tailnet-user"); err != nil {
		t.Fatalf("tailnet sendCode IP rate limit = %v, want allow", err)
	}
}

func TestTailnetRPCDoesNotSpendPublicAccountRateLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	user, err := s.CreateUser(ctx, "+15551298001")
	if err != nil {
		t.Fatalf("create caller: %v", err)
	}
	h := &handlers{store: s}
	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	for range 10 {
		if err := h.checkRateLimitCost(tailnetRequest(&mtproto.Request{
			Ctx: ctx, UserID: user.ID,
		}), "listener-path", limit, 1); err != nil {
			t.Fatalf("tailnet RPC rate limit = %v, want allow", err)
		}
	}
	public := &mtproto.Request{Ctx: ctx, UserID: user.ID}
	if err := h.checkRateLimitCost(public, "listener-path", limit, 1); err != nil {
		t.Fatalf("first public RPC = %v, want allow", err)
	}
	err = h.checkRateLimitCost(public, "listener-path", limit, 1)
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 {
		t.Fatal("second public RPC passed after tailnet calls; caller account budget was not preserved")
	}
}

func TestTailnetUsernameSignInEnforcesTargetAccountPasswordFailureBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	fixture := newTailnetUsernameLogin(t, ctx, s, "tailnetcheck", "right-password", store.RateLimitConfig{Limit: 1, Window: time.Minute}, store.RateLimitConfig{})
	for attempt := 1; attempt <= 2; attempt++ {
		challenge := issueTailnetPasswordChallenge(t, fixture.handler, ctx, fixture.authKeyID, 0)
		proof := tailnetPasswordProof(t, challenge, "wrong-password")
		err := callTailnetCheckPassword(t, fixture.handler, ctx, fixture.authKeyID, proof)
		if attempt == 1 && !tgerr.Is(err, "PASSWORD_HASH_INVALID") {
			t.Fatalf("first tailnet username password proof = %v, want PASSWORD_HASH_INVALID for a real challenge", err)
		}
		if attempt == 2 && !tgerr.Is(err, "FLOOD_WAIT") {
			t.Fatalf("second tailnet username password proof = %v, want target-account FLOOD_WAIT", err)
		}
	}
}

func TestTailnetUsernameAccountEnforcesTargetAccountPasswordProofBudget(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	fixture := newTailnetUsernameLogin(t, ctx, s, "tailnetproof", "right-password", store.RateLimitConfig{}, store.RateLimitConfig{Limit: 1, Window: time.Minute})
	pendingChallenge := issueTailnetPasswordChallenge(t, fixture.handler, ctx, fixture.authKeyID, 0)
	goodProof := tailnetPasswordProof(t, pendingChallenge, fixture.password)
	if err := callTailnetCheckPassword(t, fixture.handler, ctx, fixture.authKeyID, goodProof); err != nil {
		t.Fatalf("complete username login before account.getPasswordSettings: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		challenge := issueTailnetPasswordChallenge(t, fixture.handler, ctx, fixture.authKeyID, fixture.user.ID)
		proof := tailnetPasswordProof(t, challenge, "wrong-password")
		var body bin.Buffer
		if err := (&tg.AccountGetPasswordSettingsRequest{Password: proof}).Encode(&body); err != nil {
			t.Fatalf("encode password settings request: %v", err)
		}
		_, err := fixture.handler.handleGetPasswordSettings(tailnetRequest(&mtproto.Request{
			Ctx: ctx, UserID: fixture.user.ID, AuthKeyID: fixture.authKeyID, Buf: &body,
		}))
		if attempt == 1 && !tgerr.Is(err, "PASSWORD_HASH_INVALID") {
			t.Fatalf("first tailnet password settings proof = %v, want PASSWORD_HASH_INVALID for a real challenge", err)
		}
		if attempt == 2 && !tgerr.Is(err, "FLOOD_WAIT") {
			t.Fatalf("second tailnet password settings proof = %v, want target-account FLOOD_WAIT", err)
		}
	}
}

type tailnetUsernameLoginFixture struct {
	user      store.User
	authKeyID [8]byte
	password  string
	handler   *handlers
}

func newTailnetUsernameLogin(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	username, password string,
	checkPasswordLimit, passwordProofLimit store.RateLimitConfig,
) tailnetUsernameLoginFixture {
	t.Helper()

	salt1 := make([]byte, 32)
	salt2 := make([]byte, 32)
	for i := range salt1 {
		salt1[i] = byte(i + 1)
		salt2[i] = byte(255 - i)
	}
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(cryptorand.Reader).NewHash([]byte(password), gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     tsrp.G,
		P:     tsrp.PBytes(),
	})
	if err != nil {
		t.Fatalf("generate username account verifier: %v", err)
	}
	user, err := s.CreateUsernameAccountWithPassword(ctx, username, augmentedSalt1, salt2, verifier)
	clear(verifier)
	if err != nil {
		t.Fatalf("create username account: %v", err)
	}

	authKeyID := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, username)
	if err != nil {
		t.Fatalf("issue username sign-in code: %v", err)
	}
	h := testHandlers(s)
	h.rateLimitCheckPassword = checkPasswordLimit
	h.rateLimitPasswordProof = passwordProofLimit
	var signInBody bin.Buffer
	if err := (&tg.AuthSignInRequest{PhoneNumber: username, PhoneCodeHash: hash}).Encode(&signInBody); err != nil {
		t.Fatalf("encode username sign-in: %v", err)
	}
	_, err = h.handleSignIn(nil, tailnetRequest(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, Buf: &signInBody,
	}))
	if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		t.Fatalf("username sign-in = %v, want SESSION_PASSWORD_NEEDED", err)
	}
	return tailnetUsernameLoginFixture{user: user, authKeyID: authKeyID, password: password, handler: h}
}

func issueTailnetPasswordChallenge(t *testing.T, h *handlers, ctx context.Context, authKeyID [8]byte, userID int64) *tg.AccountPassword {
	t.Helper()
	var body bin.Buffer
	if err := (&tg.AccountGetPasswordRequest{}).Encode(&body); err != nil {
		t.Fatalf("encode getPassword request: %v", err)
	}
	result, err := h.handleGetPassword(tailnetRequest(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, UserID: userID, Buf: &body,
	}))
	if err != nil {
		t.Fatalf("getPassword: %v", err)
	}
	challenge, ok := result.(*tg.AccountPassword)
	if !ok || !challenge.HasPassword || challenge.SRPID == 0 {
		t.Fatalf("getPassword result = %#v, want a password account with an issued SRP challenge", result)
	}
	if srpB, ok := challenge.GetSRPB(); !ok || len(srpB) == 0 {
		t.Fatal("getPassword returned no SRP B")
	}
	return challenge
}

func tailnetPasswordProof(t *testing.T, challenge *tg.AccountPassword, password string) *tg.InputCheckPasswordSRP {
	t.Helper()
	srpB, ok := challenge.GetSRPB()
	if !ok {
		t.Fatal("SRP challenge has no B")
	}
	proof, err := gotdauth.PasswordHash([]byte(password), challenge.SRPID, srpB, challenge.SecureRandom, challenge.CurrentAlgo)
	if err != nil {
		t.Fatalf("compute SRP proof: %v", err)
	}
	return proof
}

func callTailnetCheckPassword(t *testing.T, h *handlers, ctx context.Context, authKeyID [8]byte, proof *tg.InputCheckPasswordSRP) error {
	t.Helper()
	var body bin.Buffer
	if err := (&tg.AuthCheckPasswordRequest{Password: proof}).Encode(&body); err != nil {
		t.Fatalf("encode checkPassword request: %v", err)
	}
	_, err := h.handleCheckPassword(tailnetRequest(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: netip.Addr{}, Buf: &body,
	}))
	return err
}
