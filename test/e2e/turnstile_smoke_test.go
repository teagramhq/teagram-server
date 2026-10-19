package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
)

type smokeTurnstileVerification struct {
	token    string
	remoteIP string
}

type smokeTurnstileVerifier struct {
	verified chan smokeTurnstileVerification
}

func (v *smokeTurnstileVerifier) Verify(_ context.Context, token, remoteIP string) error {
	v.verified <- smokeTurnstileVerification{token: token, remoteIP: remoteIP}
	return nil
}

func testSmokeTurnstileSignUp(t *testing.T) {
	t.Helper()
	verifier := &smokeTurnstileVerifier{verified: make(chan smokeTurnstileVerification, 1)}
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

	select {
	case verification := <-verifier.verified:
		if verification.token != token || verification.remoteIP != "" {
			t.Fatalf("Turnstile verification = %+v, want token forwarded and socket peer omitted", verification)
		}
	default:
		t.Fatal("Turnstile verifier was not called")
	}
}

var _ api.SignUpTurnstileVerifier = (*smokeTurnstileVerifier)(nil)
