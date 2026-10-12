package mtproto

import (
	"strings"
	"unicode/utf8"
)

// AuthorizationMetadata holds the bounded initConnection fields used when an
// auth key becomes bound to an account. It is never added to RPC responses by
// the transport layer.
type AuthorizationMetadata struct {
	DeviceModel   string
	Platform      string
	SystemVersion string
	APIID         int
	AppVersion    string
}

const maxAuthorizationMetadataBytes = 128

func boundedAuthorizationString(value string) string {
	var result strings.Builder
	result.Grow(min(len(value), maxAuthorizationMetadataBytes))
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 || r == '\x00' {
			value = value[size:]
			continue
		}
		if result.Len()+size > maxAuthorizationMetadataBytes {
			break
		}
		result.WriteString(value[:size])
		value = value[size:]
	}
	return result.String()
}

func boundedAuthorizationMetadata(metadata AuthorizationMetadata) AuthorizationMetadata {
	metadata.DeviceModel = boundedAuthorizationString(metadata.DeviceModel)
	metadata.Platform = boundedAuthorizationString(metadata.Platform)
	metadata.SystemVersion = boundedAuthorizationString(metadata.SystemVersion)
	metadata.AppVersion = boundedAuthorizationString(metadata.AppVersion)
	return metadata
}

// AuthorizationMetadata returns the bounded device description captured for
// this connection, or the zero value before initConnection has arrived.
func (c *Conn) AuthorizationMetadata() AuthorizationMetadata {
	return c.authorizationMetadata
}

func (c *Conn) setAuthorizationMetadata(metadata AuthorizationMetadata) {
	c.authorizationMetadata = boundedAuthorizationMetadata(metadata)
}
