package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

type fakeTurnstileVerifier struct {
	seen        map[string]bool
	remoteIPs   []string
	actions     []string
	verifyCalls int
	failure     error
	allowReplay bool
}

func (f *fakeTurnstileVerifier) Verify(_ context.Context, token, action, remoteIP string) error {
	f.verifyCalls++
	f.actions = append(f.actions, action)
	f.remoteIPs = append(f.remoteIPs, remoteIP)
	if f.failure != nil {
		return f.failure
	}
	if f.seen[token] && !f.allowReplay {
		return errors.New("rejected")
	}
	f.seen[token] = true
	return nil
}

func TestSignUpTurnstileRequiresFreshTokenBeforeChargingIPLimit(t *testing.T) {
	s := openStore(t)
	keyID := [8]byte{0x31, 0x41, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
	runner, _ := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{Limit: 1, Window: time.Minute})
	verifier := &fakeTurnstileVerifier{seen: make(map[string]bool)}
	runner.SetTurnstileVerifierForTest("site-key", verifier)
	request := signUpRequest("captchauser", "missing-code", "First", "Last")

	_, err := runner.SignUpWithToken(request, "", false)
	assertTurnstileRequired(t, err)
	if verifier.verifyCalls != 0 {
		t.Fatalf("empty token made %d verifier calls, want 0", verifier.verifyCalls)
	}
	_, err = runner.SignUpWithToken(request, strings.Repeat("x", 2049), false)
	assertTurnstileRequired(t, err)
	if verifier.verifyCalls != 0 {
		t.Fatalf("oversized token made %d verifier calls, want 0", verifier.verifyCalls)
	}

	_, err = runner.SignUpWithToken(request, strings.Repeat("x", 2048), false)
	if got := signUpRPCMessage(err); got != "PHONE_CODE_INVALID" {
		t.Fatalf("valid token then missing code error = %q, want PHONE_CODE_INVALID (proves empty token did not spend the IP limit)", got)
	}
	if verifier.verifyCalls != 1 {
		t.Fatalf("valid token made %d verifier calls, want 1", verifier.verifyCalls)
	}
	if len(verifier.actions) != 1 || verifier.actions[0] != "signup" {
		t.Fatalf("verified actions = %v, want [signup]", verifier.actions)
	}
	if len(verifier.remoteIPs) != 1 || verifier.remoteIPs[0] != "" {
		t.Fatalf("socket-trusted address was sent to verifier: %v", verifier.remoteIPs)
	}
}

func TestSignUpTurnstileAcceptsVerifiedTokenAndRejectsReplay(t *testing.T) {
	s := openStore(t)
	keyID := [8]byte{0x42, 0x41, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
	runner, _ := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{})
	verifier := &fakeTurnstileVerifier{seen: make(map[string]bool)}
	runner.SetTurnstileVerifierForTest("site-key", verifier)
	hash, _, err := s.IssueCodeForUsername(context.Background(), "captchauser")
	if err != nil {
		t.Fatal(err)
	}
	request := signUpRequest("captchauser", hash, "First", "Last")

	result, err := runner.SignUpWithToken(request, "single-use-token", true)
	if err != nil {
		t.Fatalf("sign up with valid Turnstile token: %v", err)
	}
	if _, ok := result.(*tg.AuthAuthorization); !ok {
		t.Fatalf("sign up result = %T, want *tg.AuthAuthorization", result)
	}
	if len(verifier.remoteIPs) != 1 || verifier.remoteIPs[0] != "203.0.113.211" {
		t.Fatalf("verified remote IPs = %v, want trusted proxy address", verifier.remoteIPs)
	}

	_, err = runner.SignUpWithToken(request, "single-use-token", true)
	assertTurnstileRequired(t, err)
	if verifier.verifyCalls != 2 {
		t.Fatalf("same token made %d verifier calls, want a fresh Siteverify call for every sign-up", verifier.verifyCalls)
	}
}

func TestSignUpTurnstileUsesSameErrorForVerifierRejectionAndOutage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure error
	}{
		{name: "rejected token", failure: errors.New("rejected")},
		{name: "siteverify outage", failure: errors.New("network unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			keyID := [8]byte{0x51, 0x01, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
			runner, _ := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{})
			verifier := &fakeTurnstileVerifier{seen: make(map[string]bool), failure: tc.failure}
			runner.SetTurnstileVerifierForTest("site-key", verifier)
			_, err := runner.SignUpWithToken(signUpRequest("captchauser", "missing-code", "First", "Last"), "bad-token", false)
			assertTurnstileRequired(t, err)
		})
	}
}

func TestSignUpTurnstileCapacityRefusalReturnsFloodWaitBeforeIPCharge(t *testing.T) {
	s := openStore(t)
	keyID := [8]byte{0x71, 0x41, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
	runner, _ := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{Limit: 1, Window: time.Minute})
	verifier := &fakeTurnstileVerifier{seen: make(map[string]bool), failure: api.ErrTurnstileBusy}
	runner.SetTurnstileVerifierForTest("site-key", verifier)
	request := signUpRequest("captchauser", "missing-code", "First", "Last")
	if _, err := runner.SignUpWithToken(request, "busy-token", false); !isFloodWait(err) {
		t.Fatalf("capacity refusal error = %v, want FLOOD_WAIT", err)
	}
	verifier.failure = nil
	_, err := runner.SignUpWithToken(request, "valid-token", false)
	if got := signUpRPCMessage(err); got != "PHONE_CODE_INVALID" {
		t.Fatalf("retry after capacity refusal = %q, want PHONE_CODE_INVALID (proves no IP charge)", got)
	}
}

func TestSignUpAcceptedQABypassStillChargesIPLimit(t *testing.T) {
	s := openStore(t)
	keyID := [8]byte{0x71, 0x51, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
	runner, _ := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{Limit: 1, Window: time.Minute})
	verifier := &fakeTurnstileVerifier{seen: make(map[string]bool), allowReplay: true}
	runner.SetTurnstileVerifierForTest("site-key", verifier)
	request := signUpRequest("captchauser", "missing-code", "First", "Last")
	qaBypassToken := strings.Repeat("q", 32)

	if _, err := runner.SignUpWithToken(request, qaBypassToken, false); signUpRPCMessage(err) != "PHONE_CODE_INVALID" {
		t.Fatalf("first accepted QA bypass result = %q, want PHONE_CODE_INVALID after charging the IP limit", signUpRPCMessage(err))
	}
	if _, err := runner.SignUpWithToken(request, qaBypassToken, false); !isFloodWait(err) {
		t.Fatalf("second accepted QA bypass error = %v, want FLOOD_WAIT from the same IP limit", err)
	}
	if verifier.verifyCalls != 2 {
		t.Fatalf("reusable QA token made %d verifier calls, want 2", verifier.verifyCalls)
	}
}

func TestSignUpTurnstileRedactsVerifierErrorAndToken(t *testing.T) {
	s := openStore(t)
	keyID := [8]byte{0x61, 0x41, 0x59, 0x26, 0x53, 0x58, 0x97, 0x93}
	runner, logs := newLoggedSignUpRunner(t, s, keyID, config.RegistrationOpen, store.RateLimitConfig{})
	const secret, token = "secret-sentinel", "token-sentinel"
	verifier := &fakeTurnstileVerifier{seen: make(map[string]bool), failure: errors.New(secret + " " + token)}
	runner.SetTurnstileVerifierForTest("site-key", verifier)
	_, err := runner.SignUpWithToken(signUpRequest("captchauser", "missing-code", "First", "Last"), token, false)
	assertTurnstileRequired(t, err)
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), token) {
		t.Fatalf("RPC error exposed challenge data: %v", err)
	}
	for _, record := range logs.records {
		if strings.Contains(record.Message, secret) || strings.Contains(record.Message, token) {
			t.Fatalf("log message exposed challenge data: %q", record.Message)
		}
		for key, value := range attrs(record) {
			if strings.Contains(value, secret) || strings.Contains(value, token) {
				t.Fatalf("log field %q exposed challenge data: %q", key, value)
			}
		}
	}
}

func assertTurnstileRequired(t *testing.T, err error) {
	t.Helper()
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 403 || rpc.Message != "RECAPTCHA_CHECK_signup__site-key" {
		t.Fatalf("error = %v, want 403 RECAPTCHA_CHECK_signup__site-key", err)
	}
}
