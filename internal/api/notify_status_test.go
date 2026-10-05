package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

// sendStatus creates a 1:1 dialog between from and to by sending a message.
func sendStatus(t *testing.T, s *store.Store, from, to store.User) {
	t.Helper()
	_, _, _, _, err := s.SendMessage(context.Background(), from.ID, to.ID, "ping", 1, 0, 0) //nolint:dogsled // dialog creation only
	if err != nil {
		t.Fatalf("send message: %v", err)
	}
}

func TestDeliverStatusPushesToPartnersOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reg := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, reg, nil, pgtest.PeerDeriver())

	alice, err := s.CreateUser(ctx, "+1555300001")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+1555300002")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	sendStatus(t, s, alice, bob)

	// Set alice offline so LastSeenAt is populated.
	if err := s.SetUserStatus(ctx, alice.ID, true); err != nil {
		t.Fatalf("set alice online: %v", err)
	}
	if err := s.SetUserStatus(ctx, alice.ID, false); err != nil {
		t.Fatalf("set alice offline: %v", err)
	}

	// Register connections for both users.
	aliceFT := &fakeTransport{}
	bobFT := &fakeTransport{}
	aliceConn := mtproto.NewTestConn(aliceFT, testKey())
	aliceConn.SetOwner(alice.ID)
	reg.Add(alice.ID, aliceConn)
	t.Cleanup(func() { reg.Remove(alice.ID, aliceConn) })

	bobConn := mtproto.NewTestConn(bobFT, testKey())
	bobConn.SetOwner(bob.ID)
	reg.Add(bob.ID, bobConn)
	t.Cleanup(func() { reg.Remove(bob.ID, bobConn) })

	// Alice goes offline → bob should receive updateUserStatus, alice should not.
	statusReceived := make(chan bool, 1)
	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(_ context.Context, userID int64, online bool) {
			if userID == alice.ID {
				statusReceived <- online
			}
			updater.DeliverStatus(ctx, userID, online)
		},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = stop() }() //nolint:errcheck // teardown

	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	// The status mutation committed before the caller disconnected.
	rpcCtx, cancelRPC := context.WithCancel(ctx)
	cancelRPC()
	notifyCtx, cancelNotify := store.NotificationContext(rpcCtx)
	defer cancelNotify()
	if err := s.Notify(notifyCtx, store.ChannelStatus, store.StatusPayload(alice.ID, false)); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case online := <-statusReceived:
		if online {
			t.Fatal("status notification reported online after the committed offline change")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("offline status notification was not delivered")
	}
	currentAlice, ok, err := s.UserByID(ctx, alice.ID)
	if err != nil || !ok || currentAlice.IsOnline {
		t.Fatalf("stored presence after offline mutation: user=%+v found=%v err=%v", currentAlice, ok, err)
	}

	if !waitSent(bobFT) {
		t.Fatal("bob received no push")
	}
	// Alice must NOT have received a push (no self-push).
	if aliceFT.wasSent() {
		t.Fatal("alice received a push — self-push must not happen")
	}

	// Bob's pts watermark must be unchanged (zero-pts transient push).
	if got := bobConn.LastPushedPts(); got != 0 {
		t.Errorf("bob pts = %d, want 0", got)
	}
}

func TestDeliverStatusOnline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reg := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, reg, nil, pgtest.PeerDeriver())

	alice, err := s.CreateUser(ctx, "+1555300011")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+1555300012")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	sendStatus(t, s, alice, bob)

	bobFT := &fakeTransport{}
	bobConn := mtproto.NewTestConn(bobFT, testKey())
	bobConn.SetOwner(bob.ID)
	reg.Add(bob.ID, bobConn)
	t.Cleanup(func() { reg.Remove(bob.ID, bobConn) })

	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(_ context.Context, userID int64, online bool) {
			updater.DeliverStatus(ctx, userID, online)
		},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = stop() }() //nolint:errcheck // teardown

	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	if err := s.Notify(ctx, store.ChannelStatus, store.StatusPayload(alice.ID, true)); err != nil {
		t.Fatalf("notify: %v", err)
	}

	if !waitSent(bobFT) {
		t.Fatal("bob received no push for online status")
	}
}

func TestDeliverStatusNoPartners(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	reg := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, reg, nil, pgtest.PeerDeriver())

	alice, err := s.CreateUser(ctx, "+1555300021")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}

	// Alice has no dialogs.
	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(_ context.Context, userID int64, online bool) {
			updater.DeliverStatus(ctx, userID, online)
		},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = stop() }() //nolint:errcheck // teardown

	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	// Must not panic or error.
	if err := s.Notify(ctx, store.ChannelStatus, store.StatusPayload(alice.ID, true)); err != nil {
		t.Fatalf("notify: %v", err)
	}
}

func TestDeliverStatusMalformedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	// Listener with a status callback that would panic if called with bad data.
	panicOnCall := make(chan struct{})
	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, bool) { close(panicOnCall) },
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = stop() }() //nolint:errcheck // teardown

	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}

	// Malformed payloads must be dropped.
	for _, payload := range []string{"no-pipe", "abc|def", "|1", "1|"} {
		if err := s.Notify(ctx, store.ChannelStatus, payload); err != nil {
			t.Fatalf("notify %q: %v", payload, err)
		}
	}

	select {
	case <-panicOnCall:
		t.Fatal("status callback fired for malformed payload")
	case <-time.After(200 * time.Millisecond):
		// Correct: no dispatch.
	}
}

func TestDeliverStatusListenerStopDoesNotLogCanceledPartnerQuery(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	alice, err := s.CreateUser(ctx, "+1555300031")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+1555300032")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	sendStatus(t, s, alice, bob)
	assertStatusListenerStopDoesNotLogCancellation(t, s, dsn, alice.ID, true, "dialogs")
}

func TestDeliverStatusListenerStopDoesNotLogCanceledStatusRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	alice, err := s.CreateUser(ctx, "+1555300041")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+1555300042")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	sendStatus(t, s, alice, bob)
	assertStatusListenerStopDoesNotLogCancellation(t, s, dsn, alice.ID, false, "users")
}

func assertStatusListenerStopDoesNotLogCancellation(t *testing.T, s *store.Store, dsn string, userID int64, online bool, table string) {
	t.Helper()
	ctx := context.Background()

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock: %v", err)
	}
	defer func() { _ = lockConn.Close(context.Background()) }() //nolint:errcheck // teardown
	lockTx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin table lock: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }() //nolint:errcheck // teardown
	lockQuery := map[string]string{
		"dialogs": "LOCK TABLE dialogs IN ACCESS EXCLUSIVE MODE",
		"users":   "LOCK TABLE users IN ACCESS EXCLUSIVE MODE",
	}[table]
	if lockQuery == "" {
		t.Fatalf("unsupported lock table %q", table)
	}
	if _, err := lockTx.Exec(ctx, lockQuery); err != nil {
		t.Fatalf("lock %s: %v", table, err)
	}

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	updater := api.NewUpdater(s, mtproto.NewSessionRegistry(), log, pgtest.PeerDeriver())
	_, stop, err := store.StartListener(ctx, dsn,
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64, int64) {},
		func(context.Context, int64) {},
		func(context.Context, int64, int64) {},
		func(callbackCtx context.Context, userID int64, online bool) {
			updater.DeliverStatus(callbackCtx, userID, online)
		},
		func(context.Context, int64, int) {},
		func(context.Context, int64, int64, int64) {},
		func(context.Context, store.PeerType, int64, int32) {},
		nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = stop() //nolint:errcheck // teardown
		}
	}()

	if err := store.WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}
	if err := s.Notify(ctx, store.ChannelStatus, store.StatusPayload(userID, online)); err != nil {
		t.Fatalf("notify status: %v", err)
	}
	waitForStatusQueryLock(t, dsn, table)

	stopDone := make(chan error, 1)
	go func() { stopDone <- stop() }()
	select {
	case err := <-stopDone:
		stopped = true
		if err != nil {
			t.Fatalf("stop listener: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener stop did not cancel and join the status callback")
	}

	if got := logs.String(); strings.Contains(got, "level=ERROR") {
		t.Fatalf("intentional listener-stop cancellation logged an error: %s", got)
	}
}

func TestDeliverStatusUnrelatedFailuresRemainObservable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	alice, err := s.CreateUser(ctx, "+1555300051")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := s.CreateUser(ctx, "+1555300052")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	sendStatus(t, s, alice, bob)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	registry := mtproto.NewSessionRegistry()
	updater := api.NewUpdater(s, registry, log, pgtest.PeerDeriver())

	partnerCtx, cancelPartner := context.WithCancel(ctx)
	cancelPartner()
	updater.DeliverStatus(partnerCtx, alice.ID, true)
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "deliver status partners") {
		t.Fatalf("unrelated partner-query cancellation was not logged: %s", got)
	}
	logs.Reset()

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock: %v", err)
	}
	defer func() { _ = lockConn.Close(context.Background()) }() //nolint:errcheck // teardown
	lockTx, err := lockConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin table lock: %v", err)
	}
	defer func() { _ = lockTx.Rollback(context.Background()) }() //nolint:errcheck // teardown
	if _, err := lockTx.Exec(ctx, "LOCK TABLE users IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock users: %v", err)
	}
	statusCtx, cancelStatus := context.WithCancel(ctx)
	statusDone := make(chan struct{})
	go func() {
		updater.DeliverStatus(statusCtx, alice.ID, false)
		close(statusDone)
	}()
	waitForStatusQueryLock(t, dsn, "users")
	cancelStatus()
	select {
	case <-statusDone:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled status read did not finish")
	}
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "deliver status user") {
		t.Fatalf("unrelated status-read cancellation was not logged: %s", got)
	}
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release users lock: %v", err)
	}
	logs.Reset()

	_, bobTransport := newConnFor(t, registry, bob.ID)
	bobTransport.sendErr = errors.New("test transport failure")
	updater.DeliverStatus(ctx, alice.ID, true)
	if got := logs.String(); !strings.Contains(got, "deliver status push") {
		t.Fatalf("status push failure was not logged: %s", got)
	}
	logs.Reset()

	if err := s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	updater.DeliverStatus(ctx, alice.ID, true)
	if got := logs.String(); !strings.Contains(got, "level=ERROR") || !strings.Contains(got, "deliver status partners") {
		t.Fatalf("unrelated store failure was not logged: %s", got)
	}
}

func waitForStatusQueryLock(t *testing.T, dsn, table string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock observer: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // teardown

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE datname = current_database()
				  AND pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock'
				  AND query ILIKE '%' || $1 || '%'
			)`, table).Scan(&blocked)
		if err != nil {
			t.Fatalf("check blocked status query: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("status query did not block on %s", table)
		case <-ticker.C:
		}
	}
}
