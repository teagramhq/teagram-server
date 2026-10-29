package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestEndpointDialHostUsesSelectedInternalService(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		wantHost string
	}{
		{endpoint: defaultEndpoint, wantHost: "teagramd"},
		{endpoint: candidateEndpoint, wantHost: "teagramd-next"},
	} {
		t.Run(test.endpoint, func(t *testing.T) {
			got, err := endpointDialHost(test.endpoint)
			if err != nil {
				t.Fatalf("resolve dial host for %q: %v", test.endpoint, err)
			}
			if got != test.wantHost {
				t.Fatalf("dial host for %q = %q, want %q", test.endpoint, got, test.wantHost)
			}
		})
	}
}

func TestVerificationClientOptionsUseSelectedEndpoint(t *testing.T) {
	var gotHost string
	err := withAccountUsingRunner(context.Background(), apiConfig{dcID: 2}, candidateEndpoint, accountSpec{}, "", nil, nil,
		func(_ context.Context, options telegram.Options, _ telegramClientAction) error {
			if len(options.DCList.Options) != 1 {
				t.Fatalf("verification client DC options = %d, want 1", len(options.DCList.Options))
			}
			gotHost = options.DCList.Options[0].IPAddress
			if got, want := options.DCList.Options[0].Port, 2443; got != want {
				t.Fatalf("verification client dial port = %d, want %d", got, want)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("run verification client setup: %v", err)
	}
	if got, want := gotHost, candidateEndpointHost; got != want {
		t.Fatalf("verification client dial host = %q, want %q", got, want)
	}
}

func TestActivityClientOptionsUseSelectedEndpoint(t *testing.T) {
	var gotHost string
	err := runActivitySessionUsingRunner(
		context.Background(),
		apiConfig{dcID: 2},
		seedOptions{endpoint: candidateEndpoint},
		accountSpec{},
		"",
		"unused-session.json",
		false,
		activityRateLimiter{},
		func(_ context.Context, options telegram.Options, _ telegramClientAction) error {
			if len(options.DCList.Options) != 1 {
				t.Fatalf("activity client DC options = %d, want 1", len(options.DCList.Options))
			}
			gotHost = options.DCList.Options[0].IPAddress
			return nil
		},
	)
	if err != nil {
		t.Fatalf("run activity client setup: %v", err)
	}
	if got, want := gotHost, candidateEndpointHost; got != want {
		t.Fatalf("activity client dial host = %q, want %q", got, want)
	}
}

func TestValidateEndpointAllowsOnlyInternalDemoService(t *testing.T) {
	for _, endpoint := range []string{defaultEndpoint, candidateEndpoint} {
		if err := validateEndpoint(endpoint); err != nil {
			t.Fatalf("internal endpoint %q: %v", endpoint, err)
		}
	}
	if err := validateEndpointForMode(trustedProxyEndpoint, true); err != nil {
		t.Fatalf("trusted proxy endpoint for Turnstile probe: %v", err)
	}
	for _, test := range []struct {
		endpoint        string
		verifyTurnstile bool
	}{
		{endpoint: trustedProxyEndpoint},
		{endpoint: defaultEndpoint, verifyTurnstile: true},
	} {
		if err := validateEndpointForMode(test.endpoint, test.verifyTurnstile); err == nil {
			t.Fatalf("validateEndpointForMode(%q, %t) succeeded", test.endpoint, test.verifyTurnstile)
		}
	}
	for _, endpoint := range []string{
		"demo.teagram.io:443",
		"localhost:2443",
		"127.0.0.1:2443",
		"postgres:5432",
		"https://teagramd:2443",
		"teagramd:2444",
		"teagramd-next:2444",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if err := validateEndpoint(endpoint); err == nil {
				t.Fatalf("validateEndpoint(%q) succeeded", endpoint)
			}
		})
	}
}

func TestProbeUsernameIsValidUsernameModeIdentifier(t *testing.T) {
	username, err := newProbeUsername()
	if err != nil {
		t.Fatalf("generate probe username: %v", err)
	}
	if len(username) > 32 || !regexp.MustCompile(`^demo_probe_[a-f0-9]{16}$`).MatchString(username) {
		t.Fatalf("probe username %q is not a supported throwaway username", username)
	}
}

func TestTurnstileProbeRecognizesOnlySignupCaptchaErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "expected challenge", err: &tgerr.Error{Message: "RECAPTCHA_CHECK_signup__site-key"}, want: true},
		{name: "other RPC error", err: &tgerr.Error{Message: "USERNAME_INVALID"}},
		{name: "missing error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := isSignupCaptchaRequired(test.err); got != test.want {
				t.Fatalf("isSignupCaptchaRequired(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestVerifyTurnstileProbeAcceptsTypedSignupRequiredResponse(t *testing.T) {
	api := &fakeTurnstileProbeAPI{}
	const username = "demo_probe_0123456789abcdef"

	if err := verifyTurnstileWithAPI(context.Background(), api, username); err != nil {
		t.Fatalf("verify Turnstile probe through CAPTCHA challenge: %v", err)
	}
	if got, want := strings.Join(api.calls, ","), "send-code,sign-in,sign-up"; got != want {
		t.Fatalf("probe API calls = %q, want %q", got, want)
	}
	if api.sendCodeRequest == nil || api.sendCodeRequest.PhoneNumber != username {
		t.Fatalf("probe send-code request = %+v, want username %q", api.sendCodeRequest, username)
	}
	if api.signInRequest == nil || api.signInRequest.PhoneNumber != username || api.signInRequest.PhoneCodeHash != "probe-hash" {
		t.Fatalf("probe sign-in request = %+v, want username and returned code hash", api.signInRequest)
	}
	if api.signUpRequest == nil || api.signUpRequest.PhoneNumber != username || api.signUpRequest.PhoneCodeHash != "probe-hash" || len(api.signUpRequest.FirstName) != 65 {
		t.Fatalf("probe sign-up request = %+v, want non-creating probe through CAPTCHA", api.signUpRequest)
	}
}

type fakeTurnstileProbeAPI struct {
	calls           []string
	sendCodeRequest *tg.AuthSendCodeRequest
	signInRequest   *tg.AuthSignInRequest
	signUpRequest   *tg.AuthSignUpRequest
}

func (api *fakeTurnstileProbeAPI) AuthSendCode(_ context.Context, request *tg.AuthSendCodeRequest) (tg.AuthSentCodeClass, error) {
	api.calls = append(api.calls, "send-code")
	api.sendCodeRequest = request
	return &tg.AuthSentCode{PhoneCodeHash: "probe-hash"}, nil
}

func (api *fakeTurnstileProbeAPI) AuthSignIn(_ context.Context, request *tg.AuthSignInRequest) (tg.AuthAuthorizationClass, error) {
	api.calls = append(api.calls, "sign-in")
	api.signInRequest = request
	return &tg.AuthAuthorizationSignUpRequired{}, nil
}

func (api *fakeTurnstileProbeAPI) AuthSignUp(_ context.Context, request *tg.AuthSignUpRequest) (tg.AuthAuthorizationClass, error) {
	api.calls = append(api.calls, "sign-up")
	api.signUpRequest = request
	return nil, &tgerr.Error{Message: "RECAPTCHA_CHECK_signup__site-key"}
}

func TestExpectedSeedMarkerCounts(t *testing.T) {
	for _, spec := range publicSpaces {
		want := 32
		switch {
		case spec.slug == "welcome":
			want = 1
		case spec.megagroup:
			want = len(conversationLines)
		}
		if got := expectedSeedMarkerCount(spec); got != want {
			t.Errorf("expectedSeedMarkerCount(%q) = %d, want %d", spec.slug, got, want)
		}
	}
}

func TestSeedReactionRequestUsesServerStandardHeartEmoji(t *testing.T) {
	request := seedReactionRequest(&tg.InputPeerUser{UserID: 42, AccessHash: 43}, 17)
	if request.MsgID != 17 {
		t.Fatalf("seed reaction message id = %d, want 17", request.MsgID)
	}
	if len(request.Reaction) != 1 {
		t.Fatalf("seed reaction count = %d, want one standard emoji", len(request.Reaction))
	}
	reaction, ok := request.Reaction[0].(*tg.ReactionEmoji)
	if !ok || reaction.Emoticon != "❤" {
		t.Fatalf("seed reaction = %#v, want the server's standard heart emoji", request.Reaction[0])
	}
}

func TestCountSeedMarkersForSpace(t *testing.T) {
	spec := spaceSpec{slug: "maps"}
	markers := map[string]int64{
		"[[teagram-demo:maps:text-00]]":  1,
		"[[teagram-demo:maps:text-01]]":  2,
		"[[teagram-demo:notes:text-00]]": 3,
	}
	if got := countSeedMarkers(markers, spec); got != 2 {
		t.Fatalf("countSeedMarkers = %d, want 2", got)
	}
}

func TestCheckModeNeedsNoConfigAndRejectsNonInternalEndpoints(t *testing.T) {
	if err := run([]string{"--check"}); err != nil {
		t.Fatalf("check without mounted runtime config: %v", err)
	}
	if err := run([]string{"--check", "--endpoint", "demo.teagram.io:443"}); err == nil {
		t.Fatal("check accepted a public endpoint")
	}
	if _, err := parseOptions([]string{"--check", "--seed"}); err == nil {
		t.Fatal("parser accepted multiple modes")
	}
}

func TestSeedManifestCoversRequiredPublicSpaces(t *testing.T) {
	if len(seedAccounts) != 10 {
		t.Fatalf("seed accounts = %d, want ten", len(seedAccounts))
	}
	for _, account := range seedAccounts {
		if account.loginID != account.username {
			t.Fatalf("seed account %s uses unsupported login identifier %q; want its public username", account.username, account.loginID)
		}
	}
	broadcasts, groups := 0, 0
	for _, space := range publicSpaces {
		if space.username == "" || space.title == "" {
			t.Fatalf("public space has empty title or username: %+v", space)
		}
		if space.megagroup {
			groups++
		} else {
			broadcasts++
		}
	}
	if broadcasts != 4 || groups < 2 || groups > 3 {
		t.Fatalf("public spaces = %d broadcast, %d group; want welcome plus three channels and two or three groups", broadcasts, groups)
	}
	if publicSpaces[0].title != "Welcome to the Teagram demo" {
		t.Fatalf("welcome title = %q", publicSpaces[0].title)
	}
	for _, space := range publicSpaces {
		if space.megagroup || space.slug == "welcome" {
			continue
		}
		markers := broadcastMarkers(space)
		if len(markers) != 32 {
			t.Fatalf("channel %s seed markers = %d, want 32", space.username, len(markers))
		}
		seen := make(map[string]bool, len(markers))
		for _, marker := range markers {
			if seen[marker] {
				t.Fatalf("channel %s repeats seed marker %s", space.username, marker)
			}
			seen[marker] = true
		}
	}
	if len(conversationLines) != 12 {
		t.Fatalf("group conversation has %d turns, want 12", len(conversationLines))
	}
}

func TestChannelCreatorRecognitionUsesParticipantRole(t *testing.T) {
	tests := map[string]struct {
		participant tg.ChannelParticipantClass
		want        bool
	}{
		"creator": {participant: &tg.ChannelParticipantCreator{UserID: 1}, want: true},
		"admin":   {participant: &tg.ChannelParticipantAdmin{UserID: 1}, want: false},
		"member":  {participant: &tg.ChannelParticipant{UserID: 1}, want: false},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if got := isChannelCreator(test.participant); got != test.want {
				t.Fatalf("isChannelCreator(%T) = %t, want %t", test.participant, got, test.want)
			}
		})
	}
}

func TestChannelPinnedMessageIDReadsFullChannelState(t *testing.T) {
	channel := &tg.ChannelFull{}
	channel.SetPinnedMsgID(42)
	if got, ok := channelPinnedMessageID(channel); !ok || got != 42 {
		t.Fatalf("channelPinnedMessageID = %d/%t, want 42/true", got, ok)
	}
	if _, ok := channelPinnedMessageID(nil); ok {
		t.Fatal("channelPinnedMessageID accepted a missing full-channel response")
	}
}

func TestAvatarJPEGIsDeterministicAndSelfContained(t *testing.T) {
	first, err := avatarJPEG(3)
	if err != nil {
		t.Fatalf("create avatar: %v", err)
	}
	second, err := avatarJPEG(3)
	if err != nil {
		t.Fatalf("repeat avatar: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("same avatar index produced different image bytes")
	}
	if _, err := jpeg.Decode(bytes.NewReader(first)); err != nil {
		t.Fatalf("generated avatar is not a JPEG: %v", err)
	}
	if got, want := hex.EncodeToString(sha256Bytes(first)), hex.EncodeToString(sha256Bytes(second)); got != want {
		t.Fatalf("image digest changed across identical generation: %s != %s", got, want)
	}
}

func TestRandomPasswordIsStrongHexAndCredentialFileIsPrivate(t *testing.T) {
	password, err := randomPassword()
	if err != nil {
		t.Fatalf("generate password: %v", err)
	}
	if len(password) != 64 {
		t.Fatalf("generated password length = %d, want 64 hex characters", len(password))
	}
	if _, err := hex.DecodeString(password); err != nil {
		t.Fatalf("generated password is not hex: %v", err)
	}

	//nolint:usetesting // The credential writer requires an explicit mode 0700 directory.
	dir, err := os.MkdirTemp(t.TempDir(), "account-inventory-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "account-passwords.json")
	if err := writeCredentialInventoryAt(path, map[string]string{"demo_account": password}); err != nil {
		t.Fatalf("write account credential inventory: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("credential file mode = %s, want regular 0600", info.Mode())
	}
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
