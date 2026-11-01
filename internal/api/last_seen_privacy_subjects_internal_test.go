package api

import (
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLastSeenPrivacySubjectsIncludesViewerAndSkipsOtherUsersWithNoPresence(t *testing.T) {
	t.Parallel()

	lastSeen := time.Unix(1_800_000_000, 0)
	users := []store.User{
		{ID: 1},
		{ID: 2, IsOnline: true},
		{ID: 3, LastSeenAt: &lastSeen},
		{ID: 4},
	}

	got := lastSeenPrivacySubjects(users, 1)
	if len(got) != 3 {
		t.Fatalf("privacy subjects = %d, want 3", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Fatalf("privacy subject IDs = %d, %d, %d; want 1, 2, 3", got[0].ID, got[1].ID, got[2].ID)
	}
}
