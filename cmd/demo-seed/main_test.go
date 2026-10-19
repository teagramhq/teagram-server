package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestValidateEndpointAllowsOnlyInternalDemoService(t *testing.T) {
	if err := validateEndpoint("teagramd:2443"); err != nil {
		t.Fatalf("internal endpoint: %v", err)
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
