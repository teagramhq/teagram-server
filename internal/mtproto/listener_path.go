package mtproto

import "fmt"

// ListenerPath is the trust-boundary policy assigned by the listener owner.
// It is never selected from a connection address, header, or protocol preface.
// The zero value is limited so an omitted setting cannot grant an exemption.
type ListenerPath uint8

const (
	// ListenerPathLimited applies the configured per-client and connection bounds.
	ListenerPathLimited ListenerPath = iota
	// ListenerPathTailnet exempts connections accepted on the tailnet path from
	// caller-keyed rate and connection limits.
	ListenerPathTailnet
)

// ParseListenerPath parses the listener policy setting. An empty setting keeps
// the limited zero value.
func ParseListenerPath(raw string) (ListenerPath, error) {
	switch raw {
	case "", "limited":
		return ListenerPathLimited, nil
	case "tailnet":
		return ListenerPathTailnet, nil
	default:
		return ListenerPathLimited, fmt.Errorf("must be %q or %q", "limited", "tailnet")
	}
}

func (p ListenerPath) String() string {
	switch p {
	case ListenerPathLimited:
		return "limited"
	case ListenerPathTailnet:
		return "tailnet"
	default:
		return fmt.Sprintf("ListenerPath(%d)", p)
	}
}
