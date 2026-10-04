package fleet_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/fleet"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPublisherPublishesLiveSnapshotAndDeletesItOnStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openPublisherStore(t, ctx, dsn)
	registry := mtproto.NewSessionRegistry()
	for _, userID := range []int64{1, 1, 2} {
		if !registry.Add(userID, &mtproto.Conn{}) {
			t.Fatalf("failed to register connection for user %d", userID)
		}
	}
	publisher := fleet.NewPublisher(st, registry, "00000000000000000000000000000071", new("edge-1"), "v1.2.3")

	stop := publisher.Start(ctx)
	waitForFleetSnapshot(t, st, func(snapshot store.FleetSnapshot) bool {
		return len(snapshot.Replicas) == 1 && snapshot.Connections == 3
	})

	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Sessions != 2 || snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 2 {
		t.Errorf("published snapshot = %+v, want 2 sessions and 2 distinct accounts", snapshot)
	}
	if got := *snapshot.Replicas[0].ReplicaID; got != "edge-1" {
		t.Errorf("replica ID = %q, want edge-1", got)
	}

	if err := stop(); err != nil {
		t.Fatalf("publisher shutdown cleanup: %v", err)
	}
	after, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Replicas) != 0 || after.Connections != 0 || after.DistinctAccounts == nil || *after.DistinctAccounts != 0 {
		t.Errorf("snapshot remains after clean shutdown: %+v", after)
	}
}

func TestPublisherKeepsTelemetryLockWaitOffTheMTProtoRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openPublisherStore(t, ctx, dsn)
	registry := mtproto.NewSessionRegistry()
	if !registry.Add(123456789, &mtproto.Conn{}) {
		t.Fatal("failed to register test connection")
	}
	publisher := fleet.NewPublisher(st, registry, "00000000000000000000000000000072", nil, "v1")
	if err := fleet.PublishForTest(ctx, publisher); err != nil {
		t.Fatalf("initial publication: %v", err)
	}
	before, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	control := publisherControlConn(t, ctx, dsn)
	lockTx, err := control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE fleet_live_accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }() //nolint:errcheck // release test lock

	publishDone := make(chan error, 1)
	go func() { publishDone <- fleet.PublishForTest(ctx, publisher) }()
	waitForFleetTableLock(t, ctx, control)

	started := time.Now()
	if !registry.Add(987654321, &mtproto.Conn{}) {
		t.Fatal("registry rejected unrelated connection")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("registry Add waited %v on a telemetry database lock", elapsed)
	}
	select {
	case err := <-publishDone:
		if err == nil {
			t.Fatal("publication succeeded while the telemetry table was locked")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked publication exceeded its writer deadline")
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release telemetry lock: %v", err)
	}
	after, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connections != before.Connections || after.DistinctAccounts == nil || before.DistinctAccounts == nil || *after.DistinctAccounts != *before.DistinctAccounts {
		t.Errorf("failed publication freshened an incomplete sample: before=%+v after=%+v", before, after)
	}
}

func TestPublisherReconstructsAfterTelemetryDataLoss(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openPublisherStore(t, ctx, dsn)
	registry := mtproto.NewSessionRegistry()
	for _, userID := range []int64{7, 9} {
		if !registry.Add(userID, &mtproto.Conn{}) {
			t.Fatalf("failed to register connection for user %d", userID)
		}
	}
	publisher := fleet.NewPublisher(st, registry, "00000000000000000000000000000073", nil, "v1")
	if err := fleet.PublishForTest(ctx, publisher); err != nil {
		t.Fatalf("initial publication: %v", err)
	}
	control := publisherControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `TRUNCATE fleet_live_accounts, fleet_process_snapshots`); err != nil {
		t.Fatalf("simulate telemetry loss: %v", err)
	}
	if err := fleet.PublishForTest(ctx, publisher); err != nil {
		t.Fatalf("republish after telemetry loss: %v", err)
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Replicas) != 1 || snapshot.Connections != 2 || snapshot.Sessions != 2 || snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 2 {
		t.Errorf("reconstructed telemetry = %+v, want one fresh generation and two distinct accounts", snapshot)
	}
}

func TestSafeBuildVersionOmitsUnsafeText(t *testing.T) {
	for input, want := range map[string]string{
		"v1.2.3":                    "v1.2.3",
		"v0.0.0-20261004-gabc123":   "v0.0.0-20261004-gabc123",
		"<script>alert(1)</script>": "",
		"line\nbreak":               "",
	} {
		if got := fleet.SafeBuildVersionForTest(input); got != want {
			t.Errorf("safeBuildVersion(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPublisherLogsNoLiveAccountIDs(t *testing.T) {
	var output bytes.Buffer
	publisher := fleet.NewPublisher(nil, nil, "", nil, "")
	fleet.SetPublisherLoggerForTest(publisher, slog.New(slog.NewTextHandler(&output, nil)))
	fleet.LogPublicationFailureForTest(publisher, errors.New("database detail contains live account 123456789"))
	if strings.Contains(output.String(), "123456789") {
		t.Fatalf("publisher logged a live account identifier: %s", output.String())
	}
}

func openPublisherStore(t *testing.T, ctx context.Context, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return st
}

func publisherControlConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open control connection: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close control connection: %v", err)
		}
	})
	return conn
}

func waitForFleetSnapshot(t *testing.T, st *store.Store, want func(store.FleetSnapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := st.FleetSnapshot(context.Background())
		if err == nil && want(snapshot) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("fleet snapshot did not become available before the deadline")
}

func waitForFleetTableLock(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1
			      FROM pg_stat_activity AS activity
		      JOIN pg_locks AS blocked_lock USING (pid)
		     WHERE activity.pid <> pg_backend_pid()
		       AND activity.datname = current_database()
		       AND activity.wait_event_type = 'Lock'
		       AND blocked_lock.locktype = 'relation'
		       AND blocked_lock.relation = 'fleet_live_accounts'::regclass
		       AND NOT blocked_lock.granted
		)
		`).Scan(&waiting); err != nil {
			t.Fatalf("inspect fleet publisher lock wait: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("publisher never attempted the locked account-set replacement")
}
