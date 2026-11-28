package fragmentlink_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/fragmentlink"
)

type vectorFile struct {
	Valid   []vectorCase `json:"valid"`
	Invalid []vectorCase `json:"invalid"`
}

type vectorCase struct {
	Name  string                 `json:"name"`
	Input vectorInput            `json:"input"`
	Code  string                 `json:"code"`
	Rule  fragmentlink.ErrorKind `json:"rule,omitempty"`
}

type vectorInput struct {
	Type      fragmentlink.Type `json:"type"`
	Host      string            `json:"host,omitempty"`
	Username  string            `json:"username,omitempty"`
	ChannelID uint64            `json:"channel_id,omitempty"`
	MessageID uint64            `json:"message_id,omitempty"`
	JoinHash  string            `json:"join_hash_hex,omitempty"`
	Secret    string            `json:"secret_hex,omitempty"`
	ShortName string            `json:"short_name,omitempty"`
}

func (input vectorInput) link(t *testing.T) fragmentlink.Link {
	t.Helper()
	value := fragmentlink.Link{
		Type: input.Type, Host: input.Host, Username: input.Username,
		ChannelID: input.ChannelID, MessageID: input.MessageID, ShortName: input.ShortName,
	}
	if input.JoinHash != "" {
		decoded, err := hex.DecodeString(input.JoinHash)
		if err != nil {
			t.Fatalf("decode join hash %q: %v", input.JoinHash, err)
		}
		value.Hash = decoded
	}
	if input.Secret != "" {
		decoded, err := hex.DecodeString(input.Secret)
		if err != nil {
			t.Fatalf("decode secret %q: %v", input.Secret, err)
		}
		value.Secret = decoded
	}
	return value
}

func TestVectors(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", "docs", "fragment-link-v1-vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors vectorFile
	if err := json.Unmarshal(contents, &vectors); err != nil {
		t.Fatal(err)
	}

	if len(vectors.Valid) < 14 {
		t.Fatalf("valid vectors = %d, want at least 14", len(vectors.Valid))
	}
	if len(vectors.Invalid) < 15 {
		t.Fatalf("invalid vectors = %d, want at least 15", len(vectors.Invalid))
	}
	validCounts := make(map[fragmentlink.Type]int)
	for _, testCase := range vectors.Valid {
		validCounts[testCase.Input.Type]++
	}
	for linkType := fragmentlink.TypeProfile; linkType <= fragmentlink.TypeEmojiSet; linkType++ {
		if validCounts[linkType] < 2 {
			t.Errorf("valid vectors for %s = %d, want at least 2", linkType, validCounts[linkType])
		}
	}

	t.Run("valid", func(t *testing.T) {
		for _, testCase := range vectors.Valid {
			t.Run(testCase.Name, func(t *testing.T) {
				input := testCase.Input.link(t)
				code, err := fragmentlink.Encode(input)
				if err != nil {
					t.Fatalf("Encode() error = %v", err)
				}
				if code != testCase.Code {
					t.Fatalf("Encode() = %q, want %q", code, testCase.Code)
				}

				decoded, err := fragmentlink.Decode(testCase.Code)
				if err != nil {
					t.Fatalf("Decode() error = %v", err)
				}
				if !reflect.DeepEqual(decoded, input) {
					t.Fatalf("Decode() = %#v, want %#v", decoded, input)
				}
				reencoded, err := fragmentlink.Encode(decoded)
				if err != nil {
					t.Fatalf("Encode(Decode(code)) error = %v", err)
				}
				if reencoded != testCase.Code {
					t.Fatalf("Encode(Decode(code)) = %q, want %q", reencoded, testCase.Code)
				}
			})
		}
	})

	t.Run("invalid", func(t *testing.T) {
		for _, testCase := range vectors.Invalid {
			t.Run(testCase.Name, func(t *testing.T) {
				_, err := fragmentlink.Decode(testCase.Code)
				if err == nil {
					t.Fatalf("Decode(%q) succeeded", testCase.Code)
				}
				var codecErr *fragmentlink.Error
				if !errors.As(err, &codecErr) {
					t.Fatalf("Decode() error type = %T, want *Error", err)
				}
				if codecErr.Kind != testCase.Rule {
					t.Fatalf("Decode() error kind = %q, want %q (%v)", codecErr.Kind, testCase.Rule, err)
				}
			})
		}
	})
}

func TestRandomValidLinksRoundTrip(t *testing.T) {
	for range 300 {
		for linkType := fragmentlink.TypeProfile; linkType <= fragmentlink.TypeEmojiSet; linkType++ {
			input := randomValidLink(t, linkType)
			code, err := fragmentlink.Encode(input)
			if err != nil {
				t.Fatalf("Encode(%#v) error = %v", input, err)
			}
			decoded, err := fragmentlink.Decode(code)
			if err != nil {
				t.Fatalf("Decode(%q) error = %v", code, err)
			}
			if !reflect.DeepEqual(decoded, input) {
				t.Fatalf("round trip = %#v, want %#v", decoded, input)
			}
		}
	}
}

func TestEncodeRejectsTextOutsideAlphabet(t *testing.T) {
	for _, shortName := range []string{"bad name", "kitty!", "\u212AKitty"} {
		_, err := fragmentlink.Encode(fragmentlink.Link{Type: fragmentlink.TypeStickerSet, ShortName: shortName})
		if err == nil {
			t.Fatalf("Encode() accepted short name %q outside the v1 alphabet", shortName)
		}
		var codecErr *fragmentlink.Error
		if !errors.As(err, &codecErr) || codecErr.Kind != fragmentlink.ErrorInvalidText {
			t.Fatalf("Encode() error = %v, want *Error with kind %q", err, fragmentlink.ErrorInvalidText)
		}
	}
}

func TestDecodeRequiresFragmentMarker(t *testing.T) {
	_, err := fragmentlink.Decode("~QQHDpNc")
	if err == nil {
		t.Fatal("Decode() accepted a fragment without #~")
	}
	var codecErr *fragmentlink.Error
	if !errors.As(err, &codecErr) || codecErr.Kind != fragmentlink.ErrorInvalidCode {
		t.Fatalf("Decode() error = %v, want kind %q", err, fragmentlink.ErrorInvalidCode)
	}
}

func TestEncodeCaseFoldsTextFields(t *testing.T) {
	lowerUsername := fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "alice"}
	lowerCode, err := fragmentlink.Encode(lowerUsername)
	if err != nil {
		t.Fatal(err)
	}
	upperCode, err := fragmentlink.Encode(fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "ALICE"})
	if err != nil {
		t.Fatal(err)
	}
	if upperCode != lowerCode {
		t.Fatalf("uppercase username code = %q, want folded code %q", upperCode, lowerCode)
	}
	decoded, err := fragmentlink.Decode(upperCode)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, lowerUsername) {
		t.Fatalf("decoded uppercase username = %#v, want %#v", decoded, lowerUsername)
	}

	lowerSet := fragmentlink.Link{Type: fragmentlink.TypeStickerSet, ShortName: "funny_cats"}
	lowerSetCode, err := fragmentlink.Encode(lowerSet)
	if err != nil {
		t.Fatal(err)
	}
	upperSetCode, err := fragmentlink.Encode(fragmentlink.Link{Type: fragmentlink.TypeStickerSet, ShortName: "FUNNY_CATS"})
	if err != nil {
		t.Fatal(err)
	}
	if upperSetCode != lowerSetCode {
		t.Fatalf("uppercase short-name code = %q, want folded code %q", upperSetCode, lowerSetCode)
	}
}

func TestEncodeNormalizesHost(t *testing.T) {
	cases := []struct {
		name          string
		host          string
		canonicalHost string
	}{
		{name: "uppercase ASCII", host: "CHAT.example.org", canonicalHost: "chat.example.org"},
		{name: "Unicode and trailing dot", host: "BÜCHER.Example.", canonicalHost: "xn--bcher-kva.example"},
		{name: "trailing dot", host: "chat.example.org.", canonicalHost: "chat.example.org"},
		{name: "leading zero port", host: "chat.example.org:08443", canonicalHost: "chat.example.org:8443"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			gotCode, err := fragmentlink.Encode(fragmentlink.Link{
				Type: fragmentlink.TypeProfile, Host: testCase.host, Username: "alice",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := fragmentlink.Link{Type: fragmentlink.TypeProfile, Host: testCase.canonicalHost, Username: "alice"}
			wantCode, err := fragmentlink.Encode(want)
			if err != nil {
				t.Fatal(err)
			}
			if gotCode != wantCode {
				t.Fatalf("Encode() = %q, want normalized code %q", gotCode, wantCode)
			}
			decoded, err := fragmentlink.Decode(gotCode)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, want) {
				t.Fatalf("decoded = %#v, want normalized value %#v", decoded, want)
			}
		})
	}
}

func TestEncodeRejectsInvalidTypedFields(t *testing.T) {
	cases := []struct {
		name  string
		value fragmentlink.Link
		kind  fragmentlink.ErrorKind
	}{
		{name: "invalid username", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "a"}, kind: fragmentlink.ErrorInvalidUsername},
		{name: "non-ascii username", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "\u212A" + "lice"}, kind: fragmentlink.ErrorInvalidUsername},
		{name: "zero message id", value: fragmentlink.Link{Type: fragmentlink.TypePublicMessage, Username: "alice"}, kind: fragmentlink.ErrorInvalidID},
		{name: "missing channel id", value: fragmentlink.Link{Type: fragmentlink.TypePrivateChannelMessage, MessageID: 1}, kind: fragmentlink.ErrorInvalidID},
		{name: "short invite secret", value: fragmentlink.Link{Type: fragmentlink.TypeSignUpInvite, Secret: make([]byte, 31)}, kind: fragmentlink.ErrorInvalidSecret},
		{name: "invalid host port", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Host: "chat.example.org:70000", Username: "alice"}, kind: fragmentlink.ErrorInvalidHost},
		{name: "explicit default host", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Host: fragmentlink.DefaultServerHost, Username: "alice"}, kind: fragmentlink.ErrorNonCanonical},
		{name: "unexpected id", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "alice", MessageID: 1}, kind: fragmentlink.ErrorUnexpectedField},
		{name: "unexpected empty hash", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "alice", Hash: []byte{}}, kind: fragmentlink.ErrorUnexpectedField},
		{name: "unexpected empty secret", value: fragmentlink.Link{Type: fragmentlink.TypeProfile, Username: "alice", Secret: []byte{}}, kind: fragmentlink.ErrorUnexpectedField},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := fragmentlink.Encode(testCase.value)
			if err == nil {
				t.Fatal("Encode() succeeded")
			}
			var codecErr *fragmentlink.Error
			if !errors.As(err, &codecErr) || codecErr.Kind != testCase.kind {
				t.Fatalf("Encode() error = %v, want kind %q", err, testCase.kind)
			}
		})
	}
}

func randomValidLink(t *testing.T, linkType fragmentlink.Type) fragmentlink.Link {
	t.Helper()
	data := make([]byte, 128)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	position := 0
	nextByte := func() byte {
		value := data[position]
		position++
		return value
	}
	nextInt := func(limit int) int { return int(nextByte()) % limit }
	value := fragmentlink.Link{Type: linkType}
	if nextInt(2) == 0 {
		value.Host = "chat.example.org"
	} else {
		value.Host = "chat.example.org:8443"
	}

	switch linkType {
	case fragmentlink.TypeProfile:
		value.Username = randomUsername(nextInt)
	case fragmentlink.TypePublicMessage:
		value.Username = randomUsername(nextInt)
		value.MessageID = randomID(nextByte)
	case fragmentlink.TypePrivateChannelMessage:
		value.ChannelID = randomID(nextByte)
		value.MessageID = randomID(nextByte)
	case fragmentlink.TypeJoinChat:
		value.Hash = make([]byte, nextInt(32)+1)
		for index := range value.Hash {
			value.Hash[index] = nextByte()
		}
	case fragmentlink.TypeSignUpInvite:
		value.Secret = make([]byte, 32)
		for index := range value.Secret {
			value.Secret[index] = nextByte()
		}
	case fragmentlink.TypeStickerSet, fragmentlink.TypeEmojiSet:
		value.ShortName = randomShortName(nextInt)
	}
	if nextInt(2) == 0 {
		value.Host = ""
	}
	return value
}

func randomID(nextByte func() byte) uint64 {
	var id uint64
	for range 8 {
		id = id<<8 | uint64(nextByte())
	}
	id &= 1<<63 - 1
	if id == 0 {
		return 1
	}
	return id
}

func randomUsername(nextInt func(int) int) string {
	const first = "abcdefghijklmnopqrstuvwxyz"
	const rest = "abcdefghijklmnopqrstuvwxyz0123456789_"
	length := nextInt(31) + 2
	username := []byte{first[nextInt(len(first))]}
	for len(username) < length {
		username = append(username, rest[nextInt(len(rest))])
	}
	return string(username)
}

func randomShortName(nextInt func(int) int) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz-._:"
	length := nextInt(32) + 1
	shortName := make([]byte, length)
	for i := range shortName {
		shortName[i] = alphabet[nextInt(len(alphabet))]
	}
	return string(shortName)
}

func TestTypeNamesRemainStable(t *testing.T) {
	got := []string{
		fragmentlink.TypeProfile.String(), fragmentlink.TypePublicMessage.String(), fragmentlink.TypePrivateChannelMessage.String(),
		fragmentlink.TypeJoinChat.String(), fragmentlink.TypeSignUpInvite.String(), fragmentlink.TypeStickerSet.String(), fragmentlink.TypeEmojiSet.String(),
	}
	want := []string{"profile", "public_message", "private_channel_message", "join_chat", "sign_up_invite", "sticker_set", "emoji_set"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("type names = %q, want %q", got, want)
	}
}
