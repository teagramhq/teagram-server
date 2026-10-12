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
	assertReplicaStatus(t, ctx, collectorB, ownerID, "online")
	drainReplicaStatusFor(t, ctx, collectorB, ownerID, 300*time.Millisecond)

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

	setRule(ownerCmds, "hide owner status", &tg.InputPrivacyValueDisallowAll{})
	assertReplicaStatus(t, ctx, collectorB, ownerID, "recently")

	if err := storeA.SetLastSeenPrivacy(ctx, ownerID, store.LastSeenPrivacyRule{
		BaseRule:          "nobody",
		AllowedUserIDs:    []int64{viewerID},
		DisallowedUserIDs: []int64{},
	}); err != nil {
		t.Fatalf("allow viewer status: %v", err)
	}
	assertReplicaStatus(t, ctx, collectorB, ownerID, "online")

	if err := storeA.SetLastSeenPrivacy(ctx, ownerID, store.LastSeenPrivacyRule{BaseRule: "contacts"}); err != nil {
		t.Fatalf("set contacts privacy: %v", err)
	}
	assertReplicaStatus(t, ctx, collectorB, ownerID, "recently")
	exec(ownerCmds, "add viewer to owner's contacts", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsAddContact(ctx, &tg.ContactsAddContactRequest{
			ID:        inputUser(ownerID, viewerID),
			FirstName: "Viewer",
		})
		return err
	})
	assertReplicaStatus(t, ctx, collectorB, ownerID, "online")
	exec(ownerCmds, "remove viewer from owner's contacts", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsDeleteContacts(ctx, []tg.InputUserClass{inputUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatus(t, ctx, collectorB, ownerID, "recently")

	if err := storeA.SetLastSeenPrivacy(ctx, ownerID, store.LastSeenPrivacyRule{BaseRule: "everybody"}); err != nil {
		t.Fatalf("allow everybody status: %v", err)
	}
	assertReplicaStatus(t, ctx, collectorB, ownerID, "online")
	exec(ownerCmds, "block viewer", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatus(t, ctx, collectorB, ownerID, "empty")
	exec(ownerCmds, "unblock viewer", func(ctx context.Context, client *tg.Client) error {
		_, err := client.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(ownerID, viewerID)})
		return err
	})
	assertReplicaStatus(t, ctx, collectorB, ownerID, "online")
}

func assertReplicaStatus(t *testing.T, ctx context.Context, collector *updateCollector, ownerID int64, want string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		select {
		case update := <-collector.userStatus:
			if update.UserID != ownerID {
				continue
			}
			matches := false
			switch want {
			case "online":
				_, matches = update.Status.(*tg.UserStatusOnline)
			case "recently":
				_, matches = update.Status.(*tg.UserStatusRecently)
			case "empty":
				_, matches = update.Status.(*tg.UserStatusEmpty)
			default:
				t.Fatalf("unknown expected status %q", want)
			}
			if !matches {
				t.Fatalf("owner status push = %T, want %s", update.Status, want)
			}
			return
		case <-waitCtx.Done():
			t.Fatalf("timed out waiting for owner status %s: %v", want, waitCtx.Err())
		}
	}
}

func drainReplicaStatusFor(t *testing.T, ctx context.Context, collector *updateCollector, ownerID int64, duration time.Duration) {
	t.Helper()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case update := <-collector.userStatus:
			if update.UserID != ownerID {
				continue
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting for status updates to settle: %v", ctx.Err())
		}
	}
}
