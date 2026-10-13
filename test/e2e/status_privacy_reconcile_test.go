package e2e_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLastSeenPrivacyChangesPushAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatalf("load server key: %v", err)
	}
	dsn := pgtest.DSN(t)
	storeA, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica A store: %v", err)
	}
	t.Cleanup(func() {
		if err := storeA.Close(); err != nil {
			t.Errorf("close replica A store: %v", err)
		}
	})
	storeB, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("open replica B store: %v", err)
	}
	t.Cleanup(func() {
		if err := storeB.Close(); err != nil {
			t.Errorf("close replica B store: %v", err)
		}
	})

	const dcID = 2
	codes := newMultiCodeSink()
	listenerA := mustListen(t, ctx, "127.0.0.1:0")
	listenerB := mustListen(t, ctx, "127.0.0.1:0")
	portA, portB := tcpPort(t, listenerA), tcpPort(t, listenerB)
	stopA := bootServerWithStatus(t, ctx, key, dcID, storeA, dsn, codes.Logger(), listenerA)
	t.Cleanup(stopA)
	stopB := bootServerWithStatus(t, ctx, key, dcID, storeB, dsn, codes.Logger(), listenerB)
	t.Cleanup(stopB)
	if err := store.WaitForNotificationListener(ctx, storeA, 2); err != nil {
		t.Fatalf("wait for both replica listeners: %v", err)
	}

	const phoneOwner, phoneViewer = "+15551731001", "+15551731002"
	seedUsernameUsers(t, ctx, storeA, phoneOwner, phoneViewer)

	startClient := func(port int, phone string, collector *updateCollector, label string) (int64, chan command) {
		t.Helper()
		client := createClient(port, key, dcID, collector, nil)
		cmds := make(chan command)
		ids := make(chan int64, 1)
		done := make(chan error, 1)
		go func() { done <- runInteractive(ctx, client, flowFor(phone, codes), ids, cmds) }()
		t.Cleanup(func() {
			close(cmds)
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("%s client: %v", label, err)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("%s client did not stop", label)
			}
		})
		select {
		case id := <-ids:
			return id, cmds
		case <-ctx.Done():
			t.Fatalf("%s login: %v", label, ctx.Err())
			return 0, cmds
		}
	}
	collectorA, collectorB := newUpdateCollector(), newUpdateCollector()
	ownerID, ownerCmds := startClient(portA, phoneOwner, collectorA, "owner on replica A")
	viewerID, _ := startClient(portB, phoneViewer, collectorB, "viewer on replica B")

	exec := func(cmds chan command, name string, fn func(context.Context, *tg.Client) error) {
		t.Helper()
		done := make(chan error, 1)
		select {
		case cmds <- command{fn: fn, done: done}:
		case <-ctx.Done():
			t.Fatalf("%s enqueue: %v", name, ctx.Err())
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-ctx.Done():
			t.Fatalf("%s: %v", name, ctx.Err())
		}
	}

	exec(ownerCmds, "establish owner and viewer dialog", func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerUser(ownerID, viewerID),
			Message:  "status privacy",
			RandomID: 17310001,
		})
		return err
	})
	exec(ownerCmds, "make owner online", func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, false)
		return err
	})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "online")

	setRule := func(cmds chan command, name string, rules ...tg.InputPrivacyRuleClass) {
		t.Helper()
		exec(cmds, name, func(ctx context.Context, client *tg.Client) error {
			_, err := client.AccountSetPrivacy(ctx, &tg.AccountSetPrivacyRequest{
				Key:   &tg.InputPrivacyKeyStatusTimestamp{},
				Rules: rules,
			})
			return err
		})
	}

	transitionOwnerStatus(t, ctx, exec, ownerCmds, collectorB, ownerID)
	setRule(ownerCmds, "hide owner status", &tg.InputPrivacyValueDisallowAll{})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "recently")

	setRule(ownerCmds, "allow viewer status",
		&tg.InputPrivacyValueDisallowAll{},
		&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(ownerID, viewerID)}},
	)
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "online")

	transitionOwnerStatus(t, ctx, exec, ownerCmds, collectorB, ownerID)
	setRule(ownerCmds, "restrict status to contacts", &tg.InputPrivacyValueAllowContacts{})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "recently")
	exec(ownerCmds, "add viewer to owner's contacts", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsAddContact(ctx, &tg.ContactsAddContactRequest{
			ID:        inputUser(ownerID, viewerID),
			FirstName: "Viewer",
		})
		return err
	})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "online")
	transitionOwnerStatus(t, ctx, exec, ownerCmds, collectorB, ownerID)
	exec(ownerCmds, "remove viewer from owner's contacts", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsDeleteContacts(ctx, []tg.InputUserClass{inputUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "recently")

	setRule(ownerCmds, "allow everybody status", &tg.InputPrivacyValueAllowAll{})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "online")
	transitionOwnerStatus(t, ctx, exec, ownerCmds, collectorB, ownerID)
	exec(ownerCmds, "block viewer", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "empty")
	exec(ownerCmds, "unblock viewer", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatusSettled(t, ctx, collectorB, ownerID, "online")
}

func transitionOwnerStatus(
	t *testing.T,
	ctx context.Context,
	exec func(chan command, string, func(context.Context, *tg.Client) error),
	ownerCmds chan command,
	collector *updateCollector,
	ownerID int64,
) {
	t.Helper()
	// gotd handles incoming frames concurrently. Observe each setup transition
	// before the next RPC so those callbacks cannot overtake a privacy correction.
	exec(ownerCmds, "transition owner offline", func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, true)
		return err
	})
	assertReplicaStatusSettled(t, ctx, collector, ownerID, "offline")
	exec(ownerCmds, "transition owner online", func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, false)
		return err
	})
	assertReplicaStatusSettled(t, ctx, collector, ownerID, "online")
}

func TestTransitionOwnerStatusWaitsForViewerPush(t *testing.T) {
	t.Parallel()
	collector := newUpdateCollector()
	const ownerID int64 = 1
	calls := 0
	exec := func(_ chan command, _ string, _ func(context.Context, *tg.Client) error) {
		calls++
		var status tg.UserStatusClass = &tg.UserStatusOffline{}
		if calls == 2 {
			select {
			case <-collector.userStatus:
				t.Error("online transition started before the viewer observed the offline push")
			default:
			}
			status = &tg.UserStatusOnline{}
		}
		collector.userStatus <- &tg.UpdateUserStatus{UserID: ownerID, Status: status}
	}
	transitionOwnerStatus(t, context.Background(), exec, nil, collector, ownerID)
	if calls != 2 {
		t.Fatalf("status transitions = %d, want offline then online", calls)
	}
	select {
	case <-collector.userStatus:
		t.Error("transition returned before the viewer observed the online push")
	default:
	}
}

func assertReplicaStatusSettled(t *testing.T, ctx context.Context, collector *updateCollector, ownerID int64, want string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	quietTimer := time.NewTimer(time.Hour)
	if !quietTimer.Stop() {
		<-quietTimer.C
	}
	defer quietTimer.Stop()
	var quiet <-chan time.Time
	var lastStatus tg.UserStatusClass
	sawExpected := false
	for {
		select {
		case update := <-collector.userStatus:
			if update.UserID != ownerID {
				continue
			}
			lastStatus = update.Status
			if replicaStatusMatches(update.Status, want) {
				sawExpected = true
			}
			if sawExpected {
				if !quietTimer.Stop() {
					select {
					case <-quietTimer.C:
					default:
					}
				}
				quietTimer.Reset(300 * time.Millisecond)
				quiet = quietTimer.C
			}
		case <-quiet:
			if !sawExpected || !replicaStatusMatches(lastStatus, want) {
				t.Fatalf("final owner status push = %T, want %s after updates settled", lastStatus, want)
			}
			return
		case <-waitCtx.Done():
			t.Fatalf("timed out waiting for owner status %s to settle: %v", want, waitCtx.Err())
		}
	}
}

func replicaStatusMatches(status tg.UserStatusClass, want string) bool {
	switch want {
	case "online":
		_, ok := status.(*tg.UserStatusOnline)
		return ok
	case "offline":
		_, ok := status.(*tg.UserStatusOffline)
		return ok
	case "recently":
		_, ok := status.(*tg.UserStatusRecently)
		return ok
	case "empty":
		_, ok := status.(*tg.UserStatusEmpty)
		return ok
	default:
		return false
	}
}
