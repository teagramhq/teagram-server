package api

import (
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestUserInviteStateForClient(t *testing.T) {
	for state, want := range map[store.InviteState]string{
		store.InviteIssued:   "pending",
		store.InviteConsumed: "used",
		store.InviteExpired:  "expired",
		store.InviteRevoked:  "revoked",
	} {
		if got := userInviteStateForClient(state); got != want {
			t.Errorf("userInviteStateForClient(%q) = %q, want %q", state, got, want)
		}
	}
}
