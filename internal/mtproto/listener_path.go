package mtproto

// ListenerPath is the trust-boundary policy assigned by the listener owner.
// It is never selected from a connection address, header, or protocol preface.
type ListenerPath string

const (
	// ListenerPathLimited applies the configured per-client and connection bounds.
	ListenerPathLimited ListenerPath = "limited"
	// ListenerPathTailnet exempts connections accepted on the tailnet path from
	// caller-keyed rate and connection limits.
	ListenerPathTailnet ListenerPath = "tailnet"
)
