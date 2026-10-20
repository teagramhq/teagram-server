package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
)

type smokeTurnstileVerification struct {
	token    string
	action   string
	remoteIP string
}

type smokeTurnstileVerifier struct {
	mu       sync.Mutex
	seen     map[string]struct{}
	verified chan smokeTurnstileVerification
}

func (v *smokeTurnstileVerifier) Verify(_ context.Context, token, action, remoteIP string) error {
	v.mu.Lock()
	if v.seen == nil {
		v.seen = make(map[string]struct{})
	}
	if _, ok := v.seen[token]; ok {
		v.mu.Unlock()
		return errors.New("token was already consumed")
	}
	v.seen[token] = struct{}{}
	v.mu.Unlock()

	v.verified <- smokeTurnstileVerification{token: token, action: action, remoteIP: remoteIP}
	return nil
}

func testSmokeTurnstileSignUp(t *testing.T) {
	t.Helper()
	verifier := &smokeTurnstileVerifier{verified: make(chan smokeTurnstileVerification, 8)}
	const siteKey = "smoke-site-key"
	f := newSmokeFixtureWithSetup(t, config.RegistrationInvite, func(f *smokeFixture) {
		f.turnstileSiteKey = siteKey
		f.turnstileVerifier = verifier
	})

	const username = "turnstile_smoke_user"
	_, inviteSecret, err := f.store.IssueInvite(f.ctx, username)
	if err != nil {
		t.Fatalf("issue sign-up invite: %v", err)
	}
	requests := &clientRequestRecorder{}
	client := telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: &session.StorageMemory{},
		Middlewares:    []telegram.Middleware{captureClientRequests(requests)},
	})

	const token = "smoke-turnstile-token"
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		apiClient := client.API()
		codeHash, err := sendCodeUsername(ctx, apiClient, username)
		if err != nil {
			return fmt.Errorf("sendCode: %w", err)
		}
		response, err := signInUsername(ctx, apiClient, username, codeHash, inviteSecret)
		if err != nil {
			if !isSignUpRequired(err) {
				return fmt.Errorf("signIn: expected SIGN_UP_REQUIRED, got %w", err)
			}
		} else if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
			return fmt.Errorf("signIn response = %T, want AuthAuthorizationSignUpRequired", response)
		}

		fixtureStart := requests.count()
		var result tg.AuthAuthorizationBox
		if err := apiClient.Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{
			Token: token,
			Query: &tg.AuthSignUpRequest{
				PhoneNumber:   username,
				PhoneCodeHash: codeHash,
				FirstName:     "Turnstile",
				LastName:      "Smoke",
			},
		}, &result); err != nil {
			return fmt.Errorf("wrapped auth.signUp: %w", err)
		}
		if _, ok := result.Authorization.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("wrapped auth.signUp response = %T, want *tg.AuthAuthorization", result.Authorization)
		}
		return requests.matchFixtureSince(fixtureStart, "auth.invokeWithReCaptcha.signUp.1")
	}); err != nil {
		t.Fatalf("Turnstile sign-up smoke flow: %v", err)
	}

	assertSmokeTurnstileVerifications(t, verifier, smokeTurnstileVerification{token: token, action: "signup"})
}

func testSmokeTurnstileSignIn(t *testing.T) {
	t.Helper()
	verifier := &smokeTurnstileVerifier{verified: make(chan smokeTurnstileVerification, 8)}
	const siteKey = "smoke-signin-site-key"
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.turnstileSiteKey = siteKey
		f.turnstileVerifier = verifier
		f.turnstileSignInEnabled = true
	})

	const username = "turnstile_signin_user"
	const password = "turnstile-smoke-password-1234"
	userID := seedUsernameUser(t, f.ctx, f.store, username, "Turnstile", password)
	client := telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: &session.StorageMemory{},
	})

	const sendCodeToken = "smoke-turnstile-signin-send-code"
	const checkPasswordToken = "smoke-turnstile-signin-check-password"
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		apiClient := client.API()
		sendCode := &tg.AuthSendCodeRequest{PhoneNumber: username, APIID: 1, APIHash: "hash"}
		if _, err := apiClient.AuthSendCode(ctx, sendCode); !isRPCMessage(err, "RECAPTCHA_CHECK_signin__"+siteKey) {
			return fmt.Errorf("bare sendCode error, want RECAPTCHA_CHECK_signin__%s: %w", siteKey, err)
		}

		var sent tg.AuthSentCodeBox
		if err := apiClient.Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: sendCodeToken, Query: sendCode}, &sent); err != nil {
			return fmt.Errorf("wrapped auth.sendCode: %w", err)
		}
		sentCode, ok := sent.SentCode.(*tg.AuthSentCode)
		if !ok {
			return fmt.Errorf("wrapped auth.sendCode response = %T, want *tg.AuthSentCode", sent.SentCode)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return fmt.Errorf("wait for sign-in code: %w", err)
		}
		if _, err := signInUsername(ctx, apiClient, username, sentCode.PhoneCodeHash, code); !isSessionPasswordNeeded(err) {
			return fmt.Errorf("auth.signIn, want SESSION_PASSWORD_NEEDED: %w", err)
		}

		passwordState, err := apiClient.AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("get SRP challenge: %w", err)
		}
		proof, err := auth.PasswordHash([]byte(password), passwordState.SRPID, passwordState.SRPB, passwordState.SecureRandom, passwordState.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("compute SRP proof: %w", err)
		}
		checkPassword := &tg.AuthCheckPasswordRequest{Password: proof}
		if _, err := apiClient.AuthCheckPassword(ctx, proof); !isRPCMessage(err, "RECAPTCHA_CHECK_signin__"+siteKey) {
			return fmt.Errorf("bare checkPassword error, want RECAPTCHA_CHECK_signin__%s: %w", siteKey, err)
		}
		var authorization tg.AuthAuthorizationBox
		if err := apiClient.Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: sendCodeToken, Query: checkPassword}, &authorization); !isRPCMessage(err, "RECAPTCHA_CHECK_signin__"+siteKey) {
			return fmt.Errorf("checkPassword with consumed sendCode token, want RECAPTCHA_CHECK_signin__%s: %w", siteKey, err)
		}
		if err := apiClient.Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: checkPasswordToken, Query: checkPassword}, &authorization); err != nil {
			return fmt.Errorf("wrapped auth.checkPassword with fresh token: %w", err)
		}
		result, ok := authorization.Authorization.(*tg.AuthAuthorization)
		if !ok || result.User == nil {
			return fmt.Errorf("wrapped auth.checkPassword response = %T, want authorization for user %d", authorization.Authorization, userID)
		}
		account, ok := result.User.(*tg.User)
		if !ok || account.ID != userID || account.Username != username {
			return fmt.Errorf("checkPassword user = %#v, want user %d with username %q", result.User, userID, username)
		}
		users, err := apiClient.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("authenticated users.getUsers: %w", err)
		}
		if len(users) != 1 {
			return fmt.Errorf("authenticated users.getUsers returned %d users, want one", len(users))
		}
		self, ok := users[0].(*tg.User)
		if !ok || self.ID != userID || self.Username != username {
			return fmt.Errorf("authenticated users.getUsers self = %#v, want user %d with username %q", users[0], userID, username)
		}
		if err := apiClient.Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: checkPasswordToken, Query: checkPassword}, &authorization); !isRPCMessage(err, "RECAPTCHA_CHECK_signin__"+siteKey) {
			return fmt.Errorf("checkPassword retry with consumed token, want RECAPTCHA_CHECK_signin__%s: %w", siteKey, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("Turnstile sign-in smoke flow: %v", err)
	}

	assertSmokeTurnstileVerifications(t, verifier,
		smokeTurnstileVerification{token: sendCodeToken, action: "signin"},
		smokeTurnstileVerification{token: checkPasswordToken, action: "signin"},
	)
}

func assertSmokeTurnstileVerifications(t *testing.T, verifier *smokeTurnstileVerifier, want ...smokeTurnstileVerification) {
	t.Helper()
	for _, expected := range want {
		select {
		case got := <-verifier.verified:
			if got.token != expected.token || got.action != expected.action || got.remoteIP != expected.remoteIP {
				t.Fatalf("Turnstile verification = %+v, want %+v", got, expected)
			}
		default:
			t.Fatalf("Turnstile verifier did not receive %+v", expected)
		}
	}
	select {
	case got := <-verifier.verified:
		t.Fatalf("unexpected Turnstile verification: %+v", got)
	default:
	}
}

var _ api.SignUpTurnstileVerifier = (*smokeTurnstileVerifier)(nil)
