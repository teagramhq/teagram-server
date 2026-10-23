package store

// RateLimitListener names the server listener scope whose per-client budgets
// a request may consume. It is assigned by the accepted listener path and is
// never derived from request headers.
type RateLimitListener uint8

const (
	// RateLimitListenerTailnet keeps the existing TCP and WebSocket budgets.
	RateLimitListenerTailnet RateLimitListener = iota
	// RateLimitListenerPublic selects the public WebSocket listener's budgets.
	RateLimitListenerPublic
)
