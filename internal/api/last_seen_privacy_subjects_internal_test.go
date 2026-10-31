package api

import (
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLastSeenPrivacySubjectsSkipsUsersWithNoPresence(t *testing.T) {
	t.Parallel()

	lastSeen := time.Unix(1_800_000_000, 0)
	users := []store.User{
		{ID: 1, IsOnline: true},
		{ID: 2, IsOnline: true},
		{ID: 3, LastSeenAt: &lastSeen},
		{ID: 4},
	}

	got := lastSeenPrivacySubjects(users, 1)
	if len(got) != 2 {
		t.Fatalf("privacy subjects = %d, want 2", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("privacy subject IDs = %d, %d; want 2, 3", got[0].ID, got[1].ID)
	}
}
