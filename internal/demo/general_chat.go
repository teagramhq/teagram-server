package demo

const (
	// SeedOwnerUsername identifies the non-guest account that owns the demo
	// General channel.
	SeedOwnerUsername = "demo_seed_owner"
	// GeneralChatHandle is the preferred public handle for General.
	GeneralChatHandle = "teagramdemogeneral"
)

var generalChatHandleCandidates = [...]string{
	GeneralChatHandle,
	"teagramdemogeneralchat",
	"teagramdemo_general",
	"teagramdemogeneralgroup",
}

// GeneralChatHandles returns the reserved public handles used to find the demo
// General chat when an older deployment already owns the preferred handle.
func GeneralChatHandles() []string {
	return append([]string(nil), generalChatHandleCandidates[:]...)
}

// IsGeneralChatHandle reports whether username is in the demo General handle
// reservation set.
func IsGeneralChatHandle(username string) bool {
	for _, candidate := range generalChatHandleCandidates {
		if username == candidate {
			return true
		}
	}
	return false
}
