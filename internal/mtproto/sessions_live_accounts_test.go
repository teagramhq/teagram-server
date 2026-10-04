package mtproto_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestSessionRegistrySnapshotLiveAccountsIsCompleteAndDistinct(t *testing.T) {
	t.Parallel()
	registry := mtproto.NewSessionRegistry()
	for _, userID := range []int64{1, 1, 2} {
		if !registry.Add(userID, &mtproto.Conn{}) {
			t.Fatalf("failed to add test connection for user %d", userID)
		}
	}

	snapshot, err := registry.SnapshotLiveAccounts(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Connections != 3 {
		t.Errorf("connections = %d, want 3", snapshot.Connections)
	}
	if snapshot.Sessions != 2 {
		t.Errorf("sessions = %d, want 2", snapshot.Sessions)
	}
	if !snapshot.Complete {
		t.Fatal("snapshot marked incomplete under the account limit")
	}
	slices.Sort(snapshot.AccountIDs)
	if !slices.Equal(snapshot.AccountIDs, []int64{1, 2}) {
		t.Errorf("account IDs = %v, want [1 2]", snapshot.AccountIDs)
	}
}

func TestSessionRegistrySnapshotLiveAccountsDoesNotPublishATruncatedSet(t *testing.T) {
	t.Parallel()
	registry := mtproto.NewSessionRegistry()
	for userID := int64(1); userID <= 3; userID++ {
		if !registry.Add(userID, &mtproto.Conn{}) {
			t.Fatalf("failed to add user %d", userID)
		}
	}

	snapshot, err := registry.SnapshotLiveAccounts(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Connections != 3 || snapshot.Sessions != 3 {
		t.Errorf("counts = (%d connections, %d sessions), want (3, 3)", snapshot.Connections, snapshot.Sessions)
	}
	if snapshot.Complete || len(snapshot.AccountIDs) != 0 {
		t.Errorf("over-limit snapshot = complete %v with %d IDs, want incomplete with no IDs", snapshot.Complete, len(snapshot.AccountIDs))
	}
}

func TestSessionRegistrySnapshotLiveAccountsHonorsContextWhileWaitingForRegistry(t *testing.T) {
	registry := mtproto.NewSessionRegistry()
	unlock := mtproto.LockSessionRegistryForTest(registry)
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := registry.SnapshotLiveAccounts(ctx, 10)
	if err == nil {
		t.Fatal("SnapshotLiveAccounts succeeded after its context expired")
	}
}
