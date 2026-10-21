package api

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

type replayTurnstileVerifier struct {
	seen      map[string]bool
	actions   []string
	remoteIPs []string
	failure   error
}

func (v *replayTurnstileVerifier) Verify(_ context.Context, token, action, remoteIP string) error {
	v.actions = append(v.actions, action)
	v.remoteIPs = append(v.remoteIPs, remoteIP)
	if v.failure != nil {
		return v.failure
	}
	if action != "signin" {
		return errors.New("wrong action")
	}
	if v.seen[token] {
		return errors.New("replayed token")
	}
	v.seen[token] = true
	return nil
}

func TestAuthSendCodeRequiresAndConsumesTurnstileToken(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	request := &tg.AuthSendCodeRequest{PhoneNumber: "x"}

	_, err := h.handleSendCode(turnstileAuthRequest(t, request, ""))
	assertSigninTurnstileRequired(t, err)
	if len(verifier.seen) != 0 {
		t.Fatalf("missing token triggered %d verifications, want none", len(verifier.seen))
	}

	_, err = h.handleSendCode(turnstileAuthRequest(t, request, "single-use"))
	if got := rpcErrorMessage(err); got != "PHONE_NUMBER_INVALID" {
		t.Fatalf("sendCode with valid token returned %q, want PHONE_NUMBER_INVALID", got)
	}

	_, err = h.handleSendCode(turnstileAuthRequest(t, request, "single-use"))
	assertSigninTurnstileRequired(t, err)
	if len(verifier.actions) != 2 || verifier.actions[0] != "signin" || verifier.actions[1] != "signin" {
		t.Fatalf("verified actions = %v, want [signin signin]", verifier.actions)
	}
}

func TestSigninChallengeDoesNotDependOnIdentifier(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	messages := make([]string, 0, 2)
	for _, identifier := range []string{"taken-identifier", "unused-identifier"} {
		_, err := h.handleSendCode(turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: identifier}, ""))
		assertSigninTurnstileRequired(t, err)
		messages = append(messages, rpcErrorMessage(err))
	}
	if len(messages) != 2 || messages[0] != messages[1] {
		t.Fatalf("challenge errors for identifiers = %v, want identical errors", messages)
	}
	if len(verifier.actions) != 0 {
		t.Fatalf("missing tokens made %d verifier calls, want no identifier-dependent verification", len(verifier.actions))
	}
}

func TestAuthCheckPasswordRequiresTurnstileBeforePasswordValidation(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	request := &tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordSRP{}}

	_, err := h.handleCheckPassword(turnstileAuthRequest(t, request, ""))
	assertSigninTurnstileRequired(t, err)

	validRequest := &tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordEmpty{}}
	_, err = h.handleCheckPassword(turnstileAuthRequest(t, validRequest, "single-use"))
	if got := rpcErrorMessage(err); got != "PASSWORD_HASH_INVALID" {
		t.Fatalf("checkPassword with valid token returned %q, want PASSWORD_HASH_INVALID", got)
	}
	if len(verifier.actions) != 1 || verifier.actions[0] != "signin" {
		t.Fatalf("checkPassword verifier action = %v, want [signin]", verifier.actions)
	}
}

func TestSigninTurnstileTokenCannotBeReusedAcrossLeaves(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	sendCode := turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "x"}, "shared-token")
	sendCode.AuthKeyID = [8]byte{1}
	if _, err := h.handleSendCode(sendCode); rpcErrorMessage(err) != "PHONE_NUMBER_INVALID" {
		t.Fatalf("first signin leaf error = %v, want PHONE_NUMBER_INVALID after challenge admission", err)
	}
	checkPassword := turnstileAuthRequest(t, &tg.AuthCheckPasswordRequest{Password: &tg.InputCheckPasswordEmpty{}}, "shared-token")
	checkPassword.AuthKeyID = [8]byte{2}
	_, err := h.handleCheckPassword(checkPassword)
	assertSigninTurnstileRequired(t, err)
	if len(verifier.actions) != 2 || verifier.actions[0] != "signin" || verifier.actions[1] != "signin" {
		t.Fatalf("cross-leaf verifier actions = %v, want two independent signin checks", verifier.actions)
	}
}

func TestSigninTurnstileDefaultsOffWhenSignupVerifierIsConfigured(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier}
	request := &tg.AuthSendCodeRequest{PhoneNumber: "x"}

	_, err := h.handleSendCode(turnstileAuthRequest(t, request, ""))
	if got := rpcErrorMessage(err); got != "PHONE_NUMBER_INVALID" {
		t.Fatalf("bare sendCode returned %q, want existing signin behavior PHONE_NUMBER_INVALID", got)
	}
	if len(verifier.actions) != 0 {
		t.Fatalf("disabled signin gate made %d verifier calls, want none", len(verifier.actions))
	}
}

func TestSigninTurnstileFailsClosedWhenEnabledWithoutVerifier(t *testing.T) {
	h := &handlers{turnstileSiteKey: "site-key", turnstileSignInEnabled: true}
	_, err := h.handleSendCode(turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "x"}, "token"))
	assertSigninTurnstileRequired(t, err)
}

func TestSigninTurnstileUsesOnlyProxyV2Address(t *testing.T) {
	verifier := &replayTurnstileVerifier{seen: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	socketRequest := turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "socket"}, "socket-token")
	socketRequest.ClientAddr = netip.MustParseAddr("203.0.113.7")
	proxyRequest := turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "proxy"}, "proxy-token")
	proxyRequest.ClientAddr = netip.MustParseAddr("203.0.113.8")
	proxyRequest.ClientAddrFromProxyV2 = true

	for _, request := range []*mtproto.Request{socketRequest, proxyRequest} {
		if err := h.verifyTurnstile(request, "signin"); err != nil {
			t.Fatalf("verify signin token: %v", err)
		}
	}
	if len(verifier.remoteIPs) != 2 || verifier.remoteIPs[0] != "" || verifier.remoteIPs[1] != "203.0.113.8" {
		t.Fatalf("Siteverify remote IPs = %v, want omitted socket address and trusted proxy address", verifier.remoteIPs)
	}
}

func TestSigninTurnstileRedactsVerifierFailureAndToken(t *testing.T) {
	const token = "token-sentinel"
	for _, failure := range []error{errors.New("secret-sentinel " + token), context.DeadlineExceeded} {
		verifier := &replayTurnstileVerifier{seen: make(map[string]bool), failure: failure}
		h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
		_, err := h.handleSendCode(turnstileAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "identifier-sentinel"}, token))
		assertSigninTurnstileRequired(t, err)
		for _, sentinel := range []string{token, "secret-sentinel", "identifier-sentinel"} {
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("signin error exposed %q: %v", sentinel, err)
			}
		}
	}
}

type consumingTurnstileVerifier struct {
	mu       sync.Mutex
	consumed map[string]bool
	calls    int
}

func (v *consumingTurnstileVerifier) Verify(_ context.Context, token, action, _ string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if action != "signin" {
		return errors.New("unexpected action")
	}
	if v.consumed[token] {
		return errors.New("token already consumed")
	}
	v.consumed[token] = true
	return nil
}

func TestSigninTurnstileConcurrentReplayAdmitsAtMostOneAttempt(t *testing.T) {
	verifier := &consumingTurnstileVerifier{consumed: make(map[string]bool)}
	h := &handlers{turnstileSiteKey: "site-key", turnstileVerifier: verifier, turnstileSignInEnabled: true}
	requests := []*mtproto.Request{
		turnstileWrappedAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "x"}, "shared-token"),
		turnstileWrappedAuthRequest(t, &tg.AuthSendCodeRequest{PhoneNumber: "x"}, "shared-token"),
	}
	requests[0].AuthKeyID = [8]byte{1}
	requests[1].AuthKeyID = [8]byte{2}
	next := mtproto.HandlerFunc(func(_ *mtproto.Conn, request *mtproto.Request) error {
		_, err := h.handleSendCode(request)
		return err
	})
	dispatch := mtproto.UnpackInvokeWithAfterMsgAndReCaptcha(next, nil)
	start := make(chan struct{})
	results := make(chan error, len(requests))
	var wait sync.WaitGroup
	for _, request := range requests {
		wait.Add(1)
		go func(request *mtproto.Request) {
			defer wait.Done()
			<-start
			results <- dispatch.OnMessage(nil, request)
		}(request)
	}
	close(start)
	wait.Wait()
	close(results)

	admitted := 0
	rejected := 0
	for err := range results {
		if rpcErrorMessage(err) == "PHONE_NUMBER_INVALID" {
			admitted++
			continue
		}
		rejected++
		assertSigninTurnstileRequired(t, err)
	}
	if admitted != 1 || rejected != 1 {
		t.Fatalf("concurrent reuse admitted %d and rejected %d attempts, want one each", admitted, rejected)
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	if verifier.calls != 2 {
		t.Fatalf("concurrent attempts made %d verifier calls, want two independent calls", verifier.calls)
	}
	for _, request := range requests {
		if request.ReCaptchaToken != "" {
			t.Fatal("wrapped concurrent request retained its token after dispatch")
		}
	}
}

func turnstileWrappedAuthRequest(t *testing.T, request bin.Object, token string) *mtproto.Request {
	t.Helper()
	return turnstileAuthRequest(t, &tg.InvokeWithReCaptchaRequest{Token: token, Query: request}, "")
}

func turnstileAuthRequest(t *testing.T, request bin.Encoder, token string) *mtproto.Request {
	t.Helper()
	var buf bin.Buffer
	if err := request.Encode(&buf); err != nil {
		t.Fatalf("encode auth request: %v", err)
	}
	return &mtproto.Request{Ctx: context.Background(), Buf: &buf, ReCaptchaToken: token}
}

func assertSigninTurnstileRequired(t *testing.T, err error) {
	t.Helper()
	rpc, ok := errors.AsType[*tgerr.Error](err)
	if !ok || rpc.Code != 403 || rpc.Message != "RECAPTCHA_CHECK_signin__site-key" {
		t.Fatalf("error = %v, want 403 RECAPTCHA_CHECK_signin__site-key", err)
	}
}

func rpcErrorMessage(err error) string {
	if rpc, ok := errors.AsType[*tgerr.Error](err); ok {
		return rpc.Message
	}
	return ""
}
