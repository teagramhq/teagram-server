// Package fragmentlink implements the version 1 Teagram fragment link codec.
package fragmentlink

import (
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"golang.org/x/net/idna"
)

const (
	version         = 1
	versionMask     = 0xc0
	hostPresentMask = 0x20
	typeMask        = 0x1f
	maxFieldBytes   = 255
	maxID           = uint64(1<<63 - 1)
	textAlphabet    = "0123456789abcdefghijklmnopqrstuvwxyz-._:"
)

// DefaultServerHost is selected when Link.Host is empty.
const DefaultServerHost = "server.teagram.io"

// Type identifies the target represented by a fragment link.
type Type uint8

const (
	TypeProfile Type = iota + 1
	TypePublicMessage
	TypePrivateChannelMessage
	TypeJoinChat
	TypeSignUpInvite
	TypeStickerSet
	TypeEmojiSet
)

// String returns the stable name for a v1 target type.
func (linkType Type) String() string {
	switch linkType {
	case TypeProfile:
		return "profile"
	case TypePublicMessage:
		return "public_message"
	case TypePrivateChannelMessage:
		return "private_channel_message"
	case TypeJoinChat:
		return "join_chat"
	case TypeSignUpInvite:
		return "sign_up_invite"
	case TypeStickerSet:
		return "sticker_set"
	case TypeEmojiSet:
		return "emoji_set"
	default:
		return "unknown"
	}
}

// Link is the canonical target value encoded by a v1 fragment link.
// Text fields are returned in lower case. Host is empty for the default server.
type Link struct {
	Type      Type
	Host      string
	Username  string
	ChannelID uint64
	MessageID uint64
	Hash      []byte
	Secret    []byte
	ShortName string
}

// ErrorKind identifies a rejected fragment or target field.
type ErrorKind string

const (
	ErrorInvalidCode        ErrorKind = "invalid_code"
	ErrorUnsupportedVersion ErrorKind = "unsupported_version"
	ErrorUnsupportedType    ErrorKind = "unsupported_type"
	ErrorMissingField       ErrorKind = "missing_field"
	ErrorEmptyField         ErrorKind = "empty_field"
	ErrorLengthOutOfRange   ErrorKind = "length_out_of_range"
	ErrorNonCanonical       ErrorKind = "non_canonical"
	ErrorInvalidText        ErrorKind = "invalid_text"
	ErrorInvalidUsername    ErrorKind = "invalid_username"
	ErrorInvalidHost        ErrorKind = "invalid_host"
	ErrorInvalidID          ErrorKind = "invalid_id"
	ErrorInvalidSecret      ErrorKind = "invalid_secret"
	ErrorUnexpectedField    ErrorKind = "unexpected_field"
)

// Error is the typed error returned when a link or field violates the v1 contract.
type Error struct {
	Kind  ErrorKind
	Field string
}

func (err *Error) Error() string {
	if err.Field == "" {
		return fmt.Sprintf("fragment link: %s", err.Kind)
	}
	return fmt.Sprintf("fragment link %s: %s", err.Field, err.Kind)
}

func codecError(kind ErrorKind, field string) *Error {
	return &Error{Kind: kind, Field: field}
}

// Encode returns the canonical #~ v1 fragment for value.
func Encode(value Link) (string, error) {
	if value.Type < TypeProfile || value.Type > TypeEmojiSet {
		return "", codecError(ErrorUnsupportedType, "type")
	}
	if err := noUnexpectedFields(value); err != nil {
		return "", err
	}

	fields := make([][]byte, 0, 3)
	if value.Host != "" {
		host, err := normalizeHost(value.Host)
		if err != nil {
			return "", err
		}
		if host == DefaultServerHost {
			return "", codecError(ErrorNonCanonical, "host")
		}
		encodedHost, err := encodeText(host, "host")
		if err != nil {
			return "", err
		}
		fields = append(fields, encodedHost)
	}

	switch value.Type {
	case TypeProfile:
		username, encodeErr := encodeUsername(value.Username)
		if encodeErr != nil {
			return "", encodeErr
		}
		fields = append(fields, username)
	case TypePublicMessage:
		username, encodeErr := encodeUsername(value.Username)
		if encodeErr != nil {
			return "", encodeErr
		}
		messageID, encodeErr := encodeID(value.MessageID, "message_id")
		if encodeErr != nil {
			return "", encodeErr
		}
		fields = append(fields, username, messageID)
	case TypePrivateChannelMessage:
		channelID, encodeErr := encodeID(value.ChannelID, "channel_id")
		if encodeErr != nil {
			return "", encodeErr
		}
		messageID, encodeErr := encodeID(value.MessageID, "message_id")
		if encodeErr != nil {
			return "", encodeErr
		}
		fields = append(fields, channelID, messageID)
	case TypeJoinChat:
		if len(value.Hash) == 0 {
			return "", codecError(ErrorEmptyField, "hash")
		}
		fields = append(fields, value.Hash)
	case TypeSignUpInvite:
		if len(value.Secret) == 0 {
			return "", codecError(ErrorEmptyField, "secret")
		}
		if len(value.Secret) != 32 {
			return "", codecError(ErrorInvalidSecret, "secret")
		}
		fields = append(fields, value.Secret)
	case TypeStickerSet, TypeEmojiSet:
		if value.ShortName == "" {
			return "", codecError(ErrorEmptyField, "short_name")
		}
		shortName, encodeErr := encodeText(value.ShortName, "short_name")
		if encodeErr != nil {
			return "", encodeErr
		}
		fields = append(fields, shortName)
	}

	// A field before the last one uses one byte for its length.
	header := byte(version<<6) | byte(value.Type)
	if value.Host != "" {
		header |= hostPresentMask
	}
	encoded := []byte{header}
	for index, field := range fields {
		if index < len(fields)-1 {
			if len(field) == 0 {
				return "", codecError(ErrorEmptyField, fmt.Sprintf("field_%d", index))
			}
			if len(field) > maxFieldBytes {
				return "", codecError(ErrorLengthOutOfRange, fmt.Sprintf("field_%d", index))
			}
			encoded = append(encoded, byte(len(field)&0xff))
		}
		encoded = append(encoded, field...)
	}
	return "#~" + base64.RawURLEncoding.EncodeToString(encoded), nil
}

// Decode parses and validates a canonical #~ v1 fragment.
func Decode(fragment string) (Link, error) {
	if !strings.HasPrefix(fragment, "#~") {
		return Link{}, codecError(ErrorInvalidCode, "fragment")
	}
	encoded := fragment[2:]
	if strings.Contains(encoded, "=") {
		return Link{}, codecError(ErrorNonCanonical, "base64url")
	}
	if encoded == "" {
		return Link{}, codecError(ErrorMissingField, "header")
	}
	for _, char := range encoded {
		if !isBase64URLChar(char) {
			return Link{}, codecError(ErrorInvalidCode, "base64url")
		}
	}
	if _, err := base64.RawURLEncoding.DecodeString(encoded); err != nil {
		return Link{}, codecError(ErrorInvalidCode, "base64url")
	}
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil {
		return Link{}, codecError(ErrorNonCanonical, "base64url")
	}
	if len(decoded) == 0 {
		return Link{}, codecError(ErrorMissingField, "header")
	}

	header := decoded[0]
	if int(header&versionMask)>>6 != version {
		return Link{}, codecError(ErrorUnsupportedVersion, "version")
	}
	linkType := Type(header & typeMask)
	if linkType < TypeProfile || linkType > TypeEmojiSet {
		return Link{}, codecError(ErrorUnsupportedType, "type")
	}
	hasHost := header&hostPresentMask != 0
	fieldCount := dataFieldCount(linkType)
	if hasHost {
		fieldCount++
	}
	fields, err := splitFields(decoded[1:], fieldCount)
	if err != nil {
		return Link{}, err
	}

	value := Link{Type: linkType}
	fieldIndex := 0
	if hasHost {
		hostText, decodeErr := decodeText(fields[fieldIndex], "host")
		if decodeErr != nil {
			return Link{}, decodeErr
		}
		value.Host, err = normalizeHost(hostText)
		if err != nil {
			return Link{}, err
		}
		fieldIndex++
	}

	switch linkType {
	case TypeProfile:
		value.Username, err = decodeUsername(fields[fieldIndex])
	case TypePublicMessage:
		value.Username, err = decodeUsername(fields[fieldIndex])
		if err == nil {
			value.MessageID, err = decodeID(fields[fieldIndex+1], "message_id")
		}
	case TypePrivateChannelMessage:
		value.ChannelID, err = decodeID(fields[fieldIndex], "channel_id")
		if err == nil {
			value.MessageID, err = decodeID(fields[fieldIndex+1], "message_id")
		}
	case TypeJoinChat:
		value.Hash = append([]byte(nil), fields[fieldIndex]...)
	case TypeSignUpInvite:
		if len(fields[fieldIndex]) != 32 {
			err = codecError(ErrorInvalidSecret, "secret")
		} else {
			value.Secret = append([]byte(nil), fields[fieldIndex]...)
		}
	case TypeStickerSet, TypeEmojiSet:
		value.ShortName, err = decodeText(fields[fieldIndex], "short_name")
	}
	if err != nil {
		return Link{}, err
	}
	if linkType == TypeJoinChat && len(value.Hash) == 0 {
		return Link{}, codecError(ErrorEmptyField, "hash")
	}

	canonical, err := Encode(value)
	if err != nil {
		return Link{}, err
	}
	if canonical != fragment {
		return Link{}, codecError(ErrorNonCanonical, "fragment")
	}
	return value, nil
}

func dataFieldCount(linkType Type) int {
	switch linkType {
	case TypePublicMessage, TypePrivateChannelMessage:
		return 2
	default:
		return 1
	}
}

func splitFields(data []byte, count int) ([][]byte, error) {
	fields := make([][]byte, 0, count)
	for index := range count - 1 {
		if len(data) == 0 {
			return nil, codecError(ErrorMissingField, fmt.Sprintf("field_%d", index))
		}
		length := int(data[0])
		data = data[1:]
		if length == 0 {
			return nil, codecError(ErrorEmptyField, fmt.Sprintf("field_%d", index))
		}
		if length > len(data) {
			return nil, codecError(ErrorLengthOutOfRange, fmt.Sprintf("field_%d", index))
		}
		fields = append(fields, data[:length])
		data = data[length:]
	}
	if len(data) == 0 {
		return nil, codecError(ErrorMissingField, fmt.Sprintf("field_%d", count-1))
	}
	fields = append(fields, data)
	return fields, nil
}

func encodeText(value, field string) ([]byte, error) {
	if value == "" {
		return nil, codecError(ErrorEmptyField, field)
	}
	for _, char := range value {
		if char > 0x7f {
			return nil, codecError(ErrorInvalidText, field)
		}
	}
	value = strings.ToLower(value)
	number := new(big.Int)
	base := big.NewInt(int64(len(textAlphabet)))
	for _, char := range value {
		index := strings.IndexRune(textAlphabet, char)
		if index < 0 {
			return nil, codecError(ErrorInvalidText, field)
		}
		number.Mul(number, base)
		number.Add(number, big.NewInt(int64(index+1)))
	}
	return number.Bytes(), nil
}

func decodeText(value []byte, field string) (string, error) {
	if len(value) == 0 {
		return "", codecError(ErrorEmptyField, field)
	}
	if value[0] == 0 {
		return "", codecError(ErrorNonCanonical, field)
	}
	number := new(big.Int).SetBytes(value)

	base := big.NewInt(int64(len(textAlphabet)))
	one := big.NewInt(1)
	characters := make([]byte, 0, len(value)*2)
	for number.Sign() > 0 {
		number.Sub(number, one)
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(number, base, remainder)
		characters = append(characters, textAlphabet[remainder.Int64()])
		number = quotient
	}
	for left, right := 0, len(characters)-1; left < right; left, right = left+1, right-1 {
		characters[left], characters[right] = characters[right], characters[left]
	}
	return string(characters), nil
}

func encodeUsername(username string) ([]byte, error) {
	if username == "" {
		return nil, codecError(ErrorEmptyField, "username")
	}
	if !validUsername(username) {
		return nil, codecError(ErrorInvalidUsername, "username")
	}
	username = strings.ToLower(username)
	return encodeText(username, "username")
}

func decodeUsername(value []byte) (string, error) {
	username, err := decodeText(value, "username")
	if err != nil {
		return "", err
	}
	if !validUsername(username) {
		return "", codecError(ErrorInvalidUsername, "username")
	}
	return username, nil
}

func validUsername(username string) bool {
	if len(username) < 2 || len(username) > 32 || !isASCIILetter(username[0]) {
		return false
	}
	for index := 1; index < len(username); index++ {
		char := username[index]
		if !isUsernameChar(char) {
			return false
		}
	}
	return true
}

func encodeID(value uint64, field string) ([]byte, error) {
	if value == 0 || value > maxID {
		return nil, codecError(ErrorInvalidID, field)
	}
	encoded := make([]byte, 8)
	for index := len(encoded) - 1; value > 0; index-- {
		encoded[index] = byte(value & 0xff)
		value >>= 8
	}
	first := 0
	for first < len(encoded)-1 && encoded[first] == 0 {
		first++
	}
	return encoded[first:], nil
}

func decodeID(value []byte, field string) (uint64, error) {
	if len(value) == 0 {
		return 0, codecError(ErrorEmptyField, field)
	}
	allZero := true
	for _, part := range value {
		if part != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return 0, codecError(ErrorInvalidID, field)
	}
	if value[0] == 0 {
		return 0, codecError(ErrorNonCanonical, field)
	}
	if len(value) > 8 {
		return 0, codecError(ErrorInvalidID, field)
	}
	var decoded uint64
	for _, part := range value {
		decoded = decoded<<8 | uint64(part)
	}
	if decoded > maxID {
		return 0, codecError(ErrorInvalidID, field)
	}
	return decoded, nil
}

func normalizeHost(value string) (string, error) {
	if value == "" {
		return "", codecError(ErrorEmptyField, "host")
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, "/?#@\\\t\r\n ") {
		return "", codecError(ErrorInvalidHost, "host")
	}

	hostname := value
	port := ""
	if strings.Count(value, ":") > 1 {
		return "", codecError(ErrorInvalidHost, "host")
	}
	if parsedHostname, parsedPort, hasPort := strings.CutLast(value, ":"); hasPort {
		hostname = parsedHostname
		port = parsedPort
		if hostname == "" || port == "" {
			return "", codecError(ErrorInvalidHost, "host")
		}
		for _, char := range port {
			if char < '0' || char > '9' {
				return "", codecError(ErrorInvalidHost, "host")
			}
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", codecError(ErrorInvalidHost, "host")
		}
		port = strconv.Itoa(portNumber)
	}

	asciiHostname, err := idna.Lookup.ToASCII(hostname)
	if err != nil {
		return "", codecError(ErrorInvalidHost, "host")
	}
	hostname = strings.ToLower(strings.TrimRight(asciiHostname, "."))
	if hostname == "" || len(hostname) > 253 || strings.Contains(hostname, "..") {
		return "", codecError(ErrorInvalidHost, "host")
	}
	for label := range strings.SplitSeq(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", codecError(ErrorInvalidHost, "host")
		}
		for _, char := range label {
			if !isHostnameChar(char) {
				return "", codecError(ErrorInvalidHost, "host")
			}
		}
	}
	if hostname == "telegram.org" || strings.HasSuffix(hostname, ".telegram.org") {
		return "", codecError(ErrorInvalidHost, "host")
	}
	if port != "" {
		return hostname + ":" + port, nil
	}
	return hostname, nil
}

func isBase64URLChar(char rune) bool {
	return char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_'
}

func isUsernameChar(char byte) bool {
	return isASCIILetter(char) || char >= '0' && char <= '9' || char == '_'
}

func isASCIILetter(char byte) bool {
	return char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z'
}

func isHostnameChar(char rune) bool {
	return char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-'
}

func noUnexpectedFields(value Link) error {
	if value.Type != TypeProfile && value.Type != TypePublicMessage && value.Username != "" {
		return codecError(ErrorUnexpectedField, "username")
	}
	if value.Type != TypePrivateChannelMessage && value.ChannelID != 0 {
		return codecError(ErrorUnexpectedField, "channel_id")
	}
	if value.Type != TypePublicMessage && value.Type != TypePrivateChannelMessage && value.MessageID != 0 {
		return codecError(ErrorUnexpectedField, "message_id")
	}
	if value.Type != TypeJoinChat && len(value.Hash) > 0 {
		return codecError(ErrorUnexpectedField, "hash")
	}
	if value.Type != TypeSignUpInvite && len(value.Secret) > 0 {
		return codecError(ErrorUnexpectedField, "secret")
	}
	if value.Type != TypeStickerSet && value.Type != TypeEmojiSet && value.ShortName != "" {
		return codecError(ErrorUnexpectedField, "short_name")
	}
	return nil
}
