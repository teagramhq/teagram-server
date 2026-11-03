package api

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type actionTurnstileVerifier struct {
	verified map[string]string
	calls    int
}

func (v *actionTurnstileVerifier) Verify(_ context.Context, token string, allowedActions []string, _ string) (string, error) {
	v.calls++
	action := v.verified[token]
	if !turnstileActionAllowed(allowedActions, action) {
		return "", errTurnstileRejected
	}
	return action, nil
}

func TestGuestCreationAvailabilityAndSignupPassWhenSigninGateIsEnabled(t *testing.T) {
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

	const username = "freshguest"
	const signupToken = "signup-token"
	addr := netip.MustParseAddr("198.51.100.31")
	authKeyID := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	wrongAuthKeyID := [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(wrongAuthKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save wrong auth key: %v", err)
	}

	verifier := &actionTurnstileVerifier{verified: map[string]string{signupToken: "signup"}}
	h := testHandlers(s)
	h.registrationMode = config.RegistrationOpen
	h.turnstileSiteKey = "site-key"
	h.turnstileVerifier = verifier
	h.turnstileSignInEnabled = true
	h.rateLimitCheckUsernameIP = store.RateLimitConfig{Limit: 1, Window: time.Hour}

	var availabilityRequest bin.Buffer
	if err := (&tg.AccountCheckUsernameRequest{Username: username}).Encode(&availabilityRequest); err != nil {
		t.Fatalf("encode account.checkUsername: %v", err)
	}
	available, err := h.handleCheckUsername(&mtproto.Request{
		Ctx: ctx, ClientAddr: addr, Buf: &availabilityRequest,
	})
	if err != nil {
		t.Fatalf("account.checkUsername: %v", err)
	}
	if _, ok := available.(*tg.BoolTrue); !ok {
		t.Fatalf("account.checkUsername result = %T, want *tg.BoolTrue", available)
	}

	var sendCodeRequest bin.Buffer
	if err := (&tg.AuthSendCodeRequest{PhoneNumber: username}).Encode(&sendCodeRequest); err != nil {
		t.Fatalf("encode auth.sendCode: %v", err)
	}
	sent, err := h.handleSendCode(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: addr,
		ReCaptchaToken: signupToken, Buf: &sendCodeRequest,
	})
	if err != nil {
		t.Fatalf("auth.sendCode with signup challenge while signin gate is on: %v", err)
	}
	sentCode, ok := sent.(*tg.AuthSentCode)
	if !ok {
		t.Fatalf("auth.sendCode result = %T, want *tg.AuthSentCode", sent)
	}

	var signInRequest bin.Buffer
	if err := (&tg.AuthSignInRequest{
		PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash,
	}).Encode(&signInRequest); err != nil {
		t.Fatalf("encode auth.signIn: %v", err)
	}
	signInResult, err := h.handleSignIn(nil, &mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: addr, Buf: &signInRequest,
	})
	if err != nil {
		t.Fatalf("auth.signIn for a new username: %v", err)
	}
	if _, ok := signInResult.(*tg.AuthAuthorizationSignUpRequired); !ok {
		t.Fatalf("auth.signIn result = %T, want *tg.AuthAuthorizationSignUpRequired", signInResult)
	}

	var wrongKeySignUpRequest bin.Buffer
	if err := (&tg.AuthSignUpRequest{
		PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash, FirstName: "Fresh",
	}).Encode(&wrongKeySignUpRequest); err != nil {
		t.Fatalf("encode wrong-key auth.signUp: %v", err)
	}
	_, err = h.handleSignUp(&mtproto.Request{
		Ctx: ctx, AuthKeyID: wrongAuthKeyID, ClientAddr: addr, Buf: &wrongKeySignUpRequest,
	})
	if rpcErrorMessage(err) != "RECAPTCHA_CHECK_signup__site-key" {
		t.Fatalf("wrong-key auth.signUp error = %v, want signup challenge required", err)
	}

	var signUpRequest bin.Buffer
	if err := (&tg.AuthSignUpRequest{
		PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash, FirstName: "Fresh",
	}).Encode(&signUpRequest); err != nil {
		t.Fatalf("encode auth.signUp: %v", err)
	}
	created, err := h.handleSignUp(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: addr, Buf: &signUpRequest,
	})
	if err != nil {
		t.Fatalf("auth.signUp with the signup challenge bound to sendCode: %v", err)
	}
	if _, ok := created.(*tg.AuthAuthorization); !ok {
		t.Fatalf("auth.signUp result = %T, want *tg.AuthAuthorization", created)
	}
	if stillUsable, err := s.CheckSignUpTurnstileCode(ctx, username, sentCode.PhoneCodeHash, mtproto.AuthKeyIDInt64(authKeyID)); err != nil {
		t.Fatalf("check consumed signup challenge: %v", err)
	} else if stillUsable {
		t.Fatal("signup challenge remained usable after admission")
	}
	var reusedSignUpRequest bin.Buffer
	if err := (&tg.AuthSignUpRequest{
		PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash, FirstName: "Fresh",
	}).Encode(&reusedSignUpRequest); err != nil {
		t.Fatalf("encode repeated auth.signUp: %v", err)
	}
	_, err = h.handleSignUp(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ClientAddr: addr, Buf: &reusedSignUpRequest,
	})
	if got := rpcErrorMessage(err); got != "RECAPTCHA_CHECK_signup__site-key" {
		t.Fatalf("repeated auth.signUp error = %q, want signup challenge required", got)
	}
	if verifier.calls != 1 {
		t.Fatalf("Turnstile verifier calls = %d, want one Siteverify call", verifier.calls)
	}

	var limitedRequest bin.Buffer
	if err := (&tg.AccountCheckUsernameRequest{Username: "anotherguest"}).Encode(&limitedRequest); err != nil {
		t.Fatalf("encode second account.checkUsername: %v", err)
	}
	_, err = h.handleCheckUsername(&mtproto.Request{
		Ctx: ctx, ClientAddr: addr, Buf: &limitedRequest,
	})
	var rpcErr *tgerr.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != 420 {
		t.Fatalf("second account.checkUsername error = %v, want FLOOD_WAIT from unchanged check_username_ip limit", err)
	}
}

func TestSignupVerifiedCodeCannotStageExistingAccountLogin(t *testing.T) {
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

	const username = "existingguest"
	const signupToken = "signup-token"
	authKeyID := [8]byte{8, 7, 6, 5, 4, 3, 2, 1}
	if _, err := s.CreateUsernameAccountWithPassword(ctx, username, []byte{1}, []byte{2}, []byte{3}); err != nil {
		t.Fatalf("create existing username account: %v", err)
	}
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(authKeyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}

	verifier := &actionTurnstileVerifier{verified: map[string]string{signupToken: "signup"}}
	h := testHandlers(s)
	h.turnstileSiteKey = "site-key"
	h.turnstileVerifier = verifier
	h.turnstileSignInEnabled = true

	var sendCodeRequest bin.Buffer
	if err := (&tg.AuthSendCodeRequest{PhoneNumber: username}).Encode(&sendCodeRequest); err != nil {
		t.Fatalf("encode auth.sendCode: %v", err)
	}
	sent, err := h.handleSendCode(&mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, ReCaptchaToken: signupToken, Buf: &sendCodeRequest,
	})
	if err != nil {
		t.Fatalf("auth.sendCode with signup challenge: %v", err)
	}
	sentCode, ok := sent.(*tg.AuthSentCode)
	if !ok {
		t.Fatalf("auth.sendCode result = %T, want *tg.AuthSentCode", sent)
	}

	var signInRequest bin.Buffer
	if err := (&tg.AuthSignInRequest{PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash}).Encode(&signInRequest); err != nil {
		t.Fatalf("encode auth.signIn: %v", err)
	}
	_, err = h.handleSignIn(nil, &mtproto.Request{
		Ctx: ctx, AuthKeyID: authKeyID, Buf: &signInRequest,
	})
	assertSigninTurnstileRequired(t, err)

	key, found, err := s.AuthKeyByID(ctx, mtproto.AuthKeyIDInt64(authKeyID))
	if err != nil || !found {
		t.Fatalf("read auth key after refused sign-in: found=%v err=%v", found, err)
	}
	if key.PendingUserID != 0 {
		t.Fatalf("pending user after signup-verified sign-in = %d, want none", key.PendingUserID)
	}
}

func TestSigninGateOffStillIssuesCodeWithoutToken(t *testing.T) {
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

	verifier := &actionTurnstileVerifier{verified: map[string]string{}}
	h := testHandlers(s)
	h.turnstileVerifier = verifier
	var request bin.Buffer
	if err := (&tg.AuthSendCodeRequest{PhoneNumber: "barevisitor"}).Encode(&request); err != nil {
		t.Fatalf("encode auth.sendCode: %v", err)
	}
	result, err := h.handleSendCode(&mtproto.Request{Ctx: ctx, Buf: &request})
	if err != nil {
		t.Fatalf("auth.sendCode without token while sign-in gate is off: %v", err)
	}
	if _, ok := result.(*tg.AuthSentCode); !ok {
		t.Fatalf("auth.sendCode result = %T, want *tg.AuthSentCode", result)
	}
	if verifier.calls != 0 {
		t.Fatalf("gate-off sendCode made %d verifier calls, want none", verifier.calls)
	}
}
