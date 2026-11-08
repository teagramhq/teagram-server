package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
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

func TestVerifyTurnstileProbePassesWithSigninGateDisabled(t *testing.T) {
	api := &fakeTurnstileProbeAPI{}
	const username = "demo_probe_0123456789abcdef"

	if err := verifyTurnstileWithAPI(context.Background(), api, username, ""); err != nil {
		t.Fatalf("verify Turnstile probe through CAPTCHA challenge: %v", err)
	}
	if got, want := strings.Join(api.calls, ","), "send-code,sign-up"; got != want {
		t.Fatalf("probe API calls = %q, want %q", got, want)
	}
	if api.sendCodeRequest == nil || api.sendCodeRequest.PhoneNumber != username {
		t.Fatalf("probe send-code request = %+v, want username %q", api.sendCodeRequest, username)
	}
	if api.signUpRequest == nil || api.signUpRequest.PhoneNumber == username || api.signUpRequest.PhoneCodeHash != strings.Repeat("0", 32) || len(api.signUpRequest.FirstName) != 65 {
		t.Fatalf("probe sign-up request = %+v, want separate unbound challenge probe", api.signUpRequest)
	}
}

func TestVerifyTurnstileProbeUsesSignupActionWhenSigninGateIsEnabled(t *testing.T) {
	api := &fakeTurnstileProbeAPI{signInGateEnabled: true}
	const (
		username = "demo_probe_0123456789abcdef"
	)
	token := strings.Repeat("q", 32)

	if err := verifyTurnstileWithAPI(context.Background(), api, username, token); err != nil {
		t.Fatalf("verify Turnstile with sign-in gate enabled: %v", err)
	}
	if got, want := strings.Join(api.calls, ","), "send-code,send-code-with-signup-action,sign-up"; got != want {
		t.Fatalf("probe API calls = %q, want %q", got, want)
	}
	if api.sendCodeRequest == nil || api.sendCodeRequest.PhoneNumber != username {
		t.Fatalf("bare send-code request = %+v, want username %q", api.sendCodeRequest, username)
	}
	if api.wrappedToken != token || api.wrappedSendCodeRequest == nil || api.wrappedSendCodeRequest.PhoneNumber != username {
		t.Fatalf("signup-wrapped request = %+v, token present=%t; want the QA token and username %q", api.wrappedSendCodeRequest, api.wrappedToken != "", username)
	}
	if api.signUpRequest == nil || api.signUpRequest.PhoneNumber == username || api.signUpRequest.PhoneCodeHash != strings.Repeat("0", 32) {
		t.Fatalf("signup gate request = %+v, want a separate unbound username and invalid 32-byte code hash", api.signUpRequest)
	}
}

func TestVerifyTurnstileProbeDoesNotRetryOtherSendCodeErrorsWithQAToken(t *testing.T) {
	api := &fakeTurnstileProbeAPI{sendCodeError: &tgerr.Error{Message: "FLOOD_WAIT_3"}}
	err := verifyTurnstileWithAPI(context.Background(), api, "demo_probe_0123456789abcdef", strings.Repeat("q", 32))
	if err == nil {
		t.Fatal("probe accepted an unrelated sendCode error")
	}
	if got, want := strings.Join(api.calls, ","), "send-code"; got != want {
		t.Fatalf("probe API calls = %q, want %q", got, want)
	}
}

func TestVerifyTurnstileProbeRequiresQATokenForSigninChallenge(t *testing.T) {
	api := &fakeTurnstileProbeAPI{signInGateEnabled: true}
	err := verifyTurnstileWithAPI(context.Background(), api, "demo_probe_0123456789abcdef", "")
	if err == nil {
		t.Fatal("probe accepted a sign-in challenge without the signup QA token")
	}
	if got, want := strings.Join(api.calls, ","), "send-code"; got != want {
		t.Fatalf("probe API calls = %q, want %q", got, want)
	}
}

type fakeTurnstileProbeAPI struct {
	calls                  []string
	signInGateEnabled      bool
	sendCodeError          error
	sendCodeRequest        *tg.AuthSendCodeRequest
	wrappedToken           string
	wrappedSendCodeRequest *tg.AuthSendCodeRequest
	signUpRequest          *tg.AuthSignUpRequest
}

func (api *fakeTurnstileProbeAPI) AuthSendCode(_ context.Context, request *tg.AuthSendCodeRequest) (tg.AuthSentCodeClass, error) {
	api.calls = append(api.calls, "send-code")
	api.sendCodeRequest = request
	if api.sendCodeError != nil {
		return nil, api.sendCodeError
	}
	if api.signInGateEnabled {
		return nil, &tgerr.Error{Message: "RECAPTCHA_CHECK_signin__site-key"}
	}
	return &tg.AuthSentCode{PhoneCodeHash: "probe-hash"}, nil
}

func (api *fakeTurnstileProbeAPI) Invoker() tg.Invoker {
	return api
}

func (api *fakeTurnstileProbeAPI) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	api.calls = append(api.calls, "send-code-with-signup-action")
	request, ok := input.(*tg.InvokeWithReCaptchaRequest)
	if !ok {
		return errors.New("probe sent a non-reCAPTCHA request")
	}
	api.wrappedToken = request.Token
	api.wrappedSendCodeRequest, ok = request.Query.(*tg.AuthSendCodeRequest)
	if !ok {
		return errors.New("probe wrapped a non-auth.sendCode request")
	}
	sent, ok := output.(*tg.AuthSentCodeBox)
	if !ok {
		return errors.New("probe used an unexpected auth.sendCode response type")
	}
	sent.SentCode = &tg.AuthSentCode{PhoneCodeHash: "probe-hash"}
	return nil
}

func (api *fakeTurnstileProbeAPI) AuthSignUp(_ context.Context, request *tg.AuthSignUpRequest) (tg.AuthAuthorizationClass, error) {
	api.calls = append(api.calls, "sign-up")
	api.signUpRequest = request
	return nil, &tgerr.Error{Message: "RECAPTCHA_CHECK_signup__site-key"}
}

func TestReadTurnstileQATokenTrimsAndValidatesSecret(t *testing.T) {
	token := strings.Repeat("q", 32)
	path := filepath.Join(t.TempDir(), "qa-token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0600); err != nil {
		t.Fatalf("write QA token fixture: %v", err)
	}
	got, err := readTurnstileQAToken(path)
	if err != nil {
		t.Fatalf("read QA token: %v", err)
	}
	if got != token {
		t.Fatalf("read QA token = %q, want configured token", got)
	}

	if err := os.WriteFile(path, []byte("not-valid/secret"), 0600); err != nil {
		t.Fatalf("write malformed QA token fixture: %v", err)
	}
	if _, err := readTurnstileQAToken(path); err == nil {
		t.Fatal("readTurnstileQAToken accepted a malformed token")
	}

	link := filepath.Join(t.TempDir(), "qa-token-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatalf("symlink QA token fixture: %v", err)
	}
	if _, err := readTurnstileQAToken(link); err == nil {
		t.Fatal("readTurnstileQAToken accepted a symlink")
	}
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

func TestSendTextKeepsMarkerOutOfSeededMessage(t *testing.T) {
	api := &fakeMarkerSeedInvoker{}
	seeded := seedMessage{
		marker: "[[teagram-demo:maps:text-00]]",
		text:   "Teagram Demo Field Maps note 01: the lower path stayed cool beneath the willow shade.",
	}
	if _, err := sendText(context.Background(), tg.NewClient(api), &tg.InputPeerChannel{ChannelID: 1, AccessHash: 2}, seeded, 0); err != nil {
		t.Fatalf("send seeded text: %v", err)
	}
	if api.sent == nil {
		t.Fatal("sendText did not issue a message request")
	}
	if api.sent.Message != seeded.text {
		t.Fatalf("sent message = %q, want %q", api.sent.Message, seeded.text)
	}
}

func TestOtherSeededContentTextsContainNoInternalMarker(t *testing.T) {
	for _, spec := range publicSpaces {
		if spec.slug == "welcome" {
			continue
		}
		for _, seeded := range seededMessagesForSpace(spec) {
			if strings.Contains(seeded.text, "[[teagram-demo:") {
				t.Errorf("seeded %s message %s contains an internal marker: %q", spec.slug, seeded.marker, seeded.text)
			}
		}
	}
	seeded := reactionSeedMessage()
	if strings.Contains(seeded.text, "[[teagram-demo:") {
		t.Errorf("seeded reaction example contains an internal marker: %q", seeded.text)
	}
}

func TestHistoryMarkersRecognizeMarkerFreeAndLegacyOtherSeedMessages(t *testing.T) {
	for _, spec := range publicSpaces {
		if spec.slug == "welcome" {
			continue
		}
		seededMessages := seededMessagesForSpace(spec)
		for _, test := range []struct {
			name   string
			legacy bool
		}{
			{name: "marker-free"},
			{name: "legacy marker", legacy: true},
		} {
			t.Run(spec.slug+"/"+test.name, func(t *testing.T) {
				messages := make([]tg.MessageClass, 0, len(seededMessages))
				for index, seeded := range seededMessages {
					text := seeded.text
					if test.legacy {
						text = seeded.marker + " " + text
					}
					messages = append(messages, &tg.Message{ID: index + 1, Message: text})
				}
				api := &fakeMarkerSeedInvoker{history: messages}
				markers, err := historyMarkers(context.Background(), tg.NewClient(api), &tg.InputPeerChannel{ChannelID: 1, AccessHash: 2}, seededMessages...)
				if err != nil {
					t.Fatalf("read seeded history: %v", err)
				}
				if got, want := len(markers), len(seededMessages); got != want {
					t.Fatalf("seeded marker count = %d, want %d", got, want)
				}
				if got, want := countSeedMarkers(markers, spec), expectedSeedMarkerCount(spec); got != want {
					t.Fatalf("verified seeded content count = %d, want %d", got, want)
				}
				for index, seeded := range seededMessages {
					if id, ok := markers[seeded.marker]; !ok || id != int64(index+1) {
						t.Errorf("seeded marker %q resolves to (%d, %t), want (%d, true)", seeded.marker, id, ok, index+1)
					}
				}
			})
		}
	}
}

func TestSeededMarkerFromTextRecognizesRetiredCopy(t *testing.T) {
	seeded := seedMessage{
		marker:  "[[teagram-demo:maps:text-00]]",
		text:    "The current route note.",
		retired: []string{"The previous route note."},
	}
	if got := seededMarkerFromText("The previous route note.", []seedMessage{seeded}); got != seeded.marker {
		t.Fatalf("retired copy resolved to %q, want %q", got, seeded.marker)
	}
}

func TestHistoryMarkersRecognizeMarkerFreeAndLegacyReactionMessage(t *testing.T) {
	seeded := reactionSeedMessage()
	for _, test := range []struct {
		name   string
		legacy bool
	}{
		{name: "marker-free"},
		{name: "legacy marker", legacy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			text := seeded.text
			if test.legacy {
				text = seeded.marker + " " + text
			}
			api := &fakeMarkerSeedInvoker{history: []tg.MessageClass{&tg.Message{ID: 13, Message: text}}}
			markers, err := historyMarkers(context.Background(), tg.NewClient(api), &tg.InputPeerUser{UserID: 1, AccessHash: 2}, seeded)
			if err != nil {
				t.Fatalf("read reaction history: %v", err)
			}
			if got := markers[seeded.marker]; got != 13 {
				t.Fatalf("reaction marker resolves to message %d, want 13", got)
			}
		})
	}
}

type fakeMarkerSeedInvoker struct {
	sent    *tg.MessagesSendMessageRequest
	history []tg.MessageClass
}

func (api *fakeMarkerSeedInvoker) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.MessagesSendMessageRequest:
		api.sent = request
		response, ok := output.(*tg.UpdatesBox)
		if !ok {
			return errors.New("unexpected send response type")
		}
		response.Updates = &tg.Updates{Updates: []tg.UpdateClass{&tg.UpdateMessageID{ID: 42, RandomID: request.RandomID}}}
		return nil
	case *tg.MessagesGetHistoryRequest:
		response, ok := output.(*tg.MessagesMessagesBox)
		if !ok {
			return errors.New("unexpected history response type")
		}
		response.Messages = &tg.MessagesMessages{Messages: api.history}
		return nil
	default:
		return errors.New("unexpected seed test request")
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
