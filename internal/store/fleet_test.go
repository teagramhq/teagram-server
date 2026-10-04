package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestFleetSnapshotAggregatesConnectionsAndExactDistinctAccounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)

	if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       "00000000000000000000000000000001",
		ReplicaID:        new("edge-a"),
		Version:          "v1.2.3",
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}); err != nil {
		t.Fatalf("publish replica A: %v", err)
	}
	if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       "00000000000000000000000000000002",
		ReplicaID:        new("edge-b"),
		Version:          "v1.2.3",
		Connections:      3,
		Sessions:         2,
		AccountIDs:       []int64{2, 3},
		AccountsComplete: true,
	}); err != nil {
		t.Fatalf("publish replica B: %v", err)
	}

	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatalf("read fleet snapshot: %v", err)
	}
	if snapshot.Connections != 5 || snapshot.Sessions != 4 {
		t.Errorf("fleet counts = (%d connections, %d sessions), want (5, 4)", snapshot.Connections, snapshot.Sessions)
	}
	if snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 3 {
		t.Errorf("distinct accounts = %v, want 3", snapshot.DistinctAccounts)
	}
	if len(snapshot.Replicas) != 2 {
		t.Fatalf("replicas = %d, want 2", len(snapshot.Replicas))
	}
	for _, replica := range snapshot.Replicas {
		if replica.DuplicateReplicaID {
			t.Errorf("replica %q unexpectedly marked duplicate", *replica.ReplicaID)
		}
		if replica.StartedAt.IsZero() || replica.HeartbeatAt.IsZero() {
			t.Errorf("replica timestamps are missing: %+v", replica)
		}
	}
	control := fleetControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `
		UPDATE fleet_process_snapshots
		   SET process_started_at = clock_timestamp() - interval '32 seconds',
		       heartbeat_at = clock_timestamp() - interval '31 seconds',
		       expires_at = clock_timestamp() - interval '1 second'
		 WHERE generation = '00000000000000000000000000000002'
	`); err != nil {
		t.Fatalf("expire replica B: %v", err)
	}
	afterExpiry, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterExpiry.Connections != 2 || afterExpiry.Sessions != 2 || afterExpiry.DistinctAccounts == nil || *afterExpiry.DistinctAccounts != 2 {
		t.Errorf("expired replica still contributes to fleet totals: %+v", afterExpiry)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"account_ids", "user_ids", "user_id", "account_id"} {
		if bytes.Contains(encoded, []byte(`"`+field+`"`)) {
			t.Errorf("fleet response exposes identifier field %q", field)
		}
	}
}

func TestFleetSnapshotFlagsDuplicateIDsAndSupersedesStaleGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	oldGeneration := "00000000000000000000000000000011"
	newGeneration := "00000000000000000000000000000012"
	for _, sample := range []store.FleetProcessSample{
		{Generation: oldGeneration, ReplicaID: new("edge"), Connections: 2, Sessions: 2, AccountIDs: []int64{1, 2}, AccountsComplete: true},
		{Generation: newGeneration, ReplicaID: new("edge"), Connections: 3, Sessions: 2, AccountIDs: []int64{2, 3}, AccountsComplete: true},
	} {
		if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
			t.Fatalf("publish %s: %v", sample.Generation, err)
		}
	}

	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Replicas) != 2 || !snapshot.Replicas[0].DuplicateReplicaID || !snapshot.Replicas[1].DuplicateReplicaID {
		t.Errorf("simultaneous duplicate IDs are not both flagged: %+v", snapshot.Replicas)
	}

	control := fleetControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `
		UPDATE fleet_process_snapshots
		   SET process_started_at = clock_timestamp() - interval '31 seconds',
		       heartbeat_at = clock_timestamp() - interval '21 seconds',
		       expires_at = clock_timestamp() + interval '9 seconds'
		 WHERE generation = $1
	`, oldGeneration); err != nil {
		t.Fatalf("age old generation: %v", err)
	}
	snapshot, err = st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Connections != 3 || snapshot.Sessions != 2 || snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 2 {
		t.Errorf("superseded fleet snapshot = %+v, want only new generation with 3 connections and 2 distinct accounts", snapshot)
	}
	if len(snapshot.Replicas) != 1 || snapshot.Replicas[0].Generation != newGeneration || snapshot.Replicas[0].DuplicateReplicaID {
		t.Errorf("superseded replica still counted or duplicate flag retained: %+v", snapshot.Replicas)
	}
}

func TestFleetSnapshotEmptySetAndExpiredGenerationCleanup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	generation := "00000000000000000000000000000021"
	if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       generation,
		Connections:      0,
		Sessions:         0,
		AccountsComplete: true,
	}); err != nil {
		t.Fatalf("publish empty set: %v", err)
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 0 {
		t.Errorf("empty set distinct accounts = %v, want exact zero", snapshot.DistinctAccounts)
	}

	control := fleetControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `
		UPDATE fleet_process_snapshots
		   SET process_started_at = clock_timestamp() - interval '32 seconds',
		       heartbeat_at = clock_timestamp() - interval '31 seconds',
		       expires_at = clock_timestamp() - interval '1 second'
		 WHERE generation = $1
	`, generation); err != nil {
		t.Fatalf("expire generation: %v", err)
	}
	if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       "00000000000000000000000000000022",
		Connections:      0,
		Sessions:         0,
		AccountsComplete: true,
	}); err != nil {
		t.Fatalf("publish after expiry: %v", err)
	}
	var oldSnapshots, oldAccounts int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_process_snapshots WHERE generation = $1`, generation).Scan(&oldSnapshots); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_live_accounts WHERE generation = $1`, generation).Scan(&oldAccounts); err != nil {
		t.Fatal(err)
	}
	if oldSnapshots != 0 || oldAccounts != 0 {
		t.Errorf("expired telemetry rows remain: snapshots=%d accounts=%d", oldSnapshots, oldAccounts)
	}
	var businessRows int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&businessRows); err != nil {
		t.Fatal(err)
	}
	if businessRows != 0 {
		t.Errorf("telemetry cleanup changed business data: users=%d", businessRows)
	}
}

func TestFleetSnapshotExpiryCleanupSkipsInFlightUnchangedRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	const generation = "00000000000000000000000000000071"
	const otherGeneration = "00000000000000000000000000000072"
	sample := store.FleetProcessSample{
		Generation:       generation,
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	installFleetSnapshotInsertGate(t, ctx, control, generation)
	releaseGate := holdFleetTestAdvisoryGate(t, ctx, control)
	gateHeld := true
	defer func() {
		if gateHeld {
			releaseGate()
		}
	}()

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- st.PublishFleetSnapshot(ctx, sample) }()
	if err := waitForFleetAdvisoryWaiters(ctx, control, "INSERT INTO fleet_process_snapshots", 1); err != nil {
		t.Fatal(err)
	}

	// The insert trigger pauses after account synchronization but before the
	// upsert resolves its conflict. Expire the existing row during that window.
	if _, err := control.Exec(ctx, `
		UPDATE fleet_process_snapshots
		   SET process_started_at = clock_timestamp() - interval '3 seconds',
		       heartbeat_at = clock_timestamp() - interval '2 seconds',
		       expires_at = clock_timestamp() - interval '1 second'
		 WHERE generation = $1
	`, generation); err != nil {
		t.Fatalf("expire generation during refresh: %v", err)
	}
	if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
		Generation:       otherGeneration,
		Connections:      1,
		Sessions:         1,
		AccountIDs:       []int64{3},
		AccountsComplete: true,
	}); err != nil {
		t.Fatalf("publish while refresh is paused: %v", err)
	}
	var retained int
	if err := control.QueryRow(ctx, `
		SELECT count(*) FROM fleet_live_accounts WHERE generation = $1
	`, generation).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		releaseGate()
		gateHeld = false
		if err := <-refreshDone; err != nil {
			t.Errorf("finish paused refresh after failed retention check: %v", err)
		}
		t.Fatalf("expiry cleanup removed the in-flight complete account set: retained %d rows, want 2", retained)
	}

	releaseGate()
	gateHeld = false
	if err := <-refreshDone; err != nil {
		t.Fatalf("complete paused refresh: %v", err)
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 3 {
		t.Fatalf("concurrent refresh distinct accounts = %v, want 3", snapshot.DistinctAccounts)
	}
	for _, replica := range snapshot.Replicas {
		if replica.Generation == generation && (replica.DistinctAccounts == nil || *replica.DistinctAccounts != 2) {
			t.Errorf("refreshed generation has non-exact accounts: %+v", replica)
		}
	}
}

func TestFleetSnapshotCleanupFirstThenRepublishExpiredGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	const generation = "00000000000000000000000000000073"
	const otherGeneration = "00000000000000000000000000000074"
	sample := store.FleetProcessSample{
		Generation:       generation,
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	if _, err := control.Exec(ctx, `
		UPDATE fleet_process_snapshots
		   SET process_started_at = clock_timestamp() - interval '3 seconds',
		       heartbeat_at = clock_timestamp() - interval '2 seconds',
		       expires_at = clock_timestamp() - interval '1 second'
		 WHERE generation = $1
	`, generation); err != nil {
		t.Fatalf("expire generation: %v", err)
	}
	installFleetAccountDeleteGate(t, ctx, control, generation)
	releaseGate := holdFleetTestAdvisoryGate(t, ctx, control)
	gateHeld := true
	defer func() {
		if gateHeld {
			releaseGate()
		}
	}()

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
			Generation:       otherGeneration,
			Connections:      1,
			Sessions:         1,
			AccountIDs:       []int64{3},
			AccountsComplete: true,
		})
	}()
	if err := waitForFleetAdvisoryWaiters(ctx, control, "DELETE FROM fleet_live_accounts", 1); err != nil {
		t.Fatal(err)
	}

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- st.PublishFleetSnapshot(ctx, sample) }()
	if err := waitForFleetAdvisoryWaiters(ctx, control, "teagram:fleet-generation:", 1); err != nil {
		t.Fatal(err)
	}
	releaseGate()
	gateHeld = false
	if err := <-cleanupDone; err != nil {
		t.Fatalf("finish expiry cleanup: %v", err)
	}
	if err := <-refreshDone; err != nil {
		t.Fatalf("republish cleaned generation: %v", err)
	}

	var snapshotRows, accountRows int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_process_snapshots WHERE generation = $1`, generation).Scan(&snapshotRows); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_live_accounts WHERE generation = $1`, generation).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if snapshotRows != 1 || accountRows != 2 {
		t.Fatalf("republished generation has snapshots/accounts %d/%d, want 1/2", snapshotRows, accountRows)
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 3 {
		t.Errorf("republished fleet distinct accounts = %v, want 3", snapshot.DistinctAccounts)
	}
}

func TestFleetSnapshotGracefulDeletionSerializesWithPublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	const generation = "00000000000000000000000000000075"
	sample := store.FleetProcessSample{
		Generation:       generation,
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	installFleetSnapshotInsertGate(t, ctx, control, generation)
	releaseGate := holdFleetTestAdvisoryGate(t, ctx, control)
	gateHeld := true
	defer func() {
		if gateHeld {
			releaseGate()
		}
	}()
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- st.PublishFleetSnapshot(ctx, sample) }()
	if err := waitForFleetAdvisoryWaiters(ctx, control, "INSERT INTO fleet_process_snapshots", 1); err != nil {
		t.Fatal(err)
	}
	deleteDone := make(chan error, 1)
	go func() { deleteDone <- st.DeleteFleetSnapshot(ctx, generation) }()
	if err := waitForFleetAdvisoryWaiters(ctx, control, "teagram:fleet-generation:", 1); err != nil {
		releaseGate()
		gateHeld = false
		refreshErr := <-refreshDone
		deleteErr := <-deleteDone
		t.Fatalf("%v (publish result: %v; delete result: %v)", err, refreshErr, deleteErr)
	}
	releaseGate()
	gateHeld = false
	if err := <-refreshDone; err != nil {
		t.Fatalf("finish publication during shutdown: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete snapshot after publication: %v", err)
	}

	var snapshotRows, accountRows int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_process_snapshots WHERE generation = $1`, generation).Scan(&snapshotRows); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_live_accounts WHERE generation = $1`, generation).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if snapshotRows != 0 || accountRows != 0 {
		t.Fatalf("shutdown/publication left snapshots/accounts %d/%d, want 0/0", snapshotRows, accountRows)
	}
}

func TestFleetSnapshotCleansRepeatedExpiredGenerationsAndKeepsOnlyTelemetryRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `
		INSERT INTO rate_limits (subject_id, surface, expires_at)
		VALUES (100, 'fleet-sentinel', clock_timestamp() + interval '1 hour')
	`); err != nil {
		t.Fatalf("insert business-data sentinel: %v", err)
	}
	generations := []string{
		"00000000000000000000000000000025",
		"00000000000000000000000000000026",
		"00000000000000000000000000000027",
		"00000000000000000000000000000028",
	}
	for i, generation := range generations {
		userID := int64(i + 1)
		if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
			Generation:       generation,
			ReplicaID:        new("edge"),
			Connections:      1,
			Sessions:         1,
			AccountIDs:       []int64{userID},
			AccountsComplete: true,
		}); err != nil {
			t.Fatalf("publish generation %d: %v", i+1, err)
		}
		if i == len(generations)-1 {
			continue
		}
		if _, err := control.Exec(ctx, `
			UPDATE fleet_process_snapshots
			   SET process_started_at = clock_timestamp() - interval '32 seconds',
			       heartbeat_at = clock_timestamp() - interval '31 seconds',
			       expires_at = clock_timestamp() - interval '1 second'
			 WHERE generation = $1
		`, generation); err != nil {
			t.Fatalf("expire generation %d: %v", i+1, err)
		}
	}
	var snapshots, accountRows, sentinelRows int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_process_snapshots`).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_live_accounts`).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if err := control.QueryRow(ctx, `SELECT count(*) FROM rate_limits WHERE subject_id = 100 AND surface = 'fleet-sentinel'`).Scan(&sentinelRows); err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || accountRows != 1 {
		t.Errorf("restart churn retained telemetry rows: snapshots=%d accounts=%d, want 1/1", snapshots, accountRows)
	}
	if sentinelRows != 1 {
		t.Errorf("restart cleanup changed business data: rate-limit sentinel rows=%d", sentinelRows)
	}
}

func TestFleetSnapshotPublicationIsAtomicWhenTelemetryTableIsLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	sample := store.FleetProcessSample{
		Generation:       "00000000000000000000000000000031",
		ReplicaID:        new("edge"),
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	before, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	control := fleetControlConn(t, ctx, dsn)
	lockTx, err := control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE fleet_live_accounts IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }() //nolint:errcheck // release test lock

	sample.Connections = 5
	sample.Sessions = 2
	sample.AccountIDs = []int64{2, 3}
	started := time.Now()
	err = st.PublishFleetSnapshot(ctx, sample)
	if err == nil {
		t.Fatal("publication succeeded while the live-account table was locked")
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Fatalf("locked publication took %v, want bounded failure", time.Since(started))
	}

	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release telemetry lock: %v", err)
	}
	after, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connections != before.Connections || after.DistinctAccounts == nil || before.DistinctAccounts == nil || *after.DistinctAccounts != *before.DistinctAccounts {
		t.Errorf("failed publication changed the visible complete snapshot: before=%+v after=%+v", before, after)
	}
}

func TestFleetSnapshotFailedHeartbeatRollsBackAccountDiff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	sample := store.FleetProcessSample{
		Generation:       "00000000000000000000000000000034",
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	before, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}

	lockTx, err := control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedGeneration string
	if err := lockTx.QueryRow(ctx, `
		SELECT generation FROM fleet_process_snapshots WHERE generation = $1 FOR UPDATE
	`, sample.Generation).Scan(&lockedGeneration); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }() //nolint:errcheck // release test lock

	sample.Connections = 3
	sample.AccountIDs = []int64{2, 3}
	if err := st.PublishFleetSnapshot(ctx, sample); err == nil {
		t.Fatal("publication succeeded while its snapshot row was locked")
	}
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release telemetry row lock: %v", err)
	}

	after, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connections != before.Connections || after.DistinctAccounts == nil || before.DistinctAccounts == nil || *after.DistinctAccounts != *before.DistinctAccounts {
		t.Errorf("failed heartbeat changed the visible snapshot: before=%+v after=%+v", before, after)
	}
	var accountIDs []int64
	rows, err := control.Query(ctx, `
		SELECT user_id FROM fleet_live_accounts WHERE generation = $1 ORDER BY user_id
	`, sample.Generation)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		accountIDs = append(accountIDs, userID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if !reflect.DeepEqual(accountIDs, []int64{1, 2}) {
		t.Errorf("failed heartbeat left account set %v, want original [1 2]", accountIDs)
	}
}

func TestFleetSnapshotFailedSetReplacementRollsBackPreviousCompleteSample(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	sample := store.FleetProcessSample{
		Generation:       "00000000000000000000000000000032",
		ReplicaID:        new("edge"),
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{1, 2},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	before, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sample.Connections = 5
	sample.AccountIDs = []int64{2, 2}
	if err := st.PublishFleetSnapshot(ctx, sample); err == nil {
		t.Fatal("duplicate account IDs unexpectedly published")
	}
	after, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connections != before.Connections || after.DistinctAccounts == nil || before.DistinctAccounts == nil || *after.DistinctAccounts != *before.DistinctAccounts {
		t.Errorf("failed set replacement changed the published snapshot: before=%+v after=%+v", before, after)
	}
}

func TestFleetSnapshotDoesNotRewriteUnchangedAccountRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	sample := store.FleetProcessSample{
		Generation:       "00000000000000000000000000000033",
		Connections:      2,
		Sessions:         2,
		AccountIDs:       []int64{41, 42},
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	var before string
	if err := control.QueryRow(ctx, `SELECT xmin::text FROM fleet_live_accounts WHERE generation = $1 AND user_id = 42`, sample.Generation).Scan(&before); err != nil {
		t.Fatal(err)
	}
	sample.Connections = 3
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("unchanged-set publish: %v", err)
	}
	var after string
	if err := control.QueryRow(ctx, `SELECT xmin::text FROM fleet_live_accounts WHERE generation = $1 AND user_id = 42`, sample.Generation).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("unchanged account row was rewritten: xmin %s became %s", before, after)
	}
}

func TestFleetSnapshotReaderReturnsErrorWhenTelemetryIsLocked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	lockTx, err := control.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockTx.Exec(ctx, `LOCK TABLE fleet_process_snapshots IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }() //nolint:errcheck // release test lock

	if snapshot, err := st.FleetSnapshot(ctx); err == nil {
		t.Fatalf("locked fleet read returned a successful snapshot: %+v", snapshot)
	} else if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fleet read exceeded its bounded deadline: %v", err)
	}
}

func TestFleetSnapshotCountsExactlyAtCapAndDisablesDistinctAboveIt(t *testing.T) {
	// Keep this 100k-row writer benchmark isolated from the package's parallel
	// database tests so scheduler contention does not trip its bounded deadline.
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	accountIDs := make([]int64, 100_000)
	for i := range accountIDs {
		accountIDs[i] = int64(i + 1)
	}
	sample := store.FleetProcessSample{
		Generation:       "00000000000000000000000000000041",
		Connections:      100_000,
		Sessions:         100_000,
		AccountIDs:       accountIDs,
		AccountsComplete: true,
	}
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("publish exactly-cap set: %v", err)
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DistinctAccounts == nil || *snapshot.DistinctAccounts != 100_000 {
		t.Errorf("distinct at cap = %v, want 100000", snapshot.DistinctAccounts)
	}
	if len(snapshot.Replicas) != 1 || snapshot.Replicas[0].DistinctAccounts == nil || *snapshot.Replicas[0].DistinctAccounts != 100_000 {
		t.Errorf("replica distinct at cap = %+v, want exact 100000", snapshot.Replicas)
	}

	sample.Connections = 100_001
	sample.Sessions = 100_001
	sample.AccountIDs = nil
	sample.AccountsComplete = false
	if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
		t.Fatalf("publish over-cap set: %v", err)
	}
	snapshot, err = st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DistinctAccounts != nil {
		t.Errorf("distinct above cap = %d, want unavailable", *snapshot.DistinctAccounts)
	}
	if len(snapshot.Replicas) != 1 || snapshot.Replicas[0].DistinctAccounts != nil {
		t.Errorf("replica distinct above cap = %+v, want unavailable", snapshot.Replicas)
	}
	control := fleetControlConn(t, ctx, dsn)
	var retained int
	if err := control.QueryRow(ctx, `SELECT count(*) FROM fleet_live_accounts WHERE generation = $1`, sample.Generation).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 0 {
		t.Errorf("over-cap generation retained %d account rows, want none", retained)
	}
}

func TestFleetSnapshotRejectsInvalidMetadataAndAccountRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	invalid := []store.FleetProcessSample{
		{Generation: "<script>", Connections: 0, Sessions: 0, AccountsComplete: true},
		{Generation: "00000000000000000000000000000051", ReplicaID: new("<script>"), Connections: 0, Sessions: 0, AccountsComplete: true},
		{Generation: "00000000000000000000000000000052", Version: "<script>", Connections: 0, Sessions: 0, AccountsComplete: true},
		{Generation: "00000000000000000000000000000053", Connections: 0, Sessions: 1, AccountIDs: []int64{0}, AccountsComplete: true},
	}
	for _, sample := range invalid {
		if err := st.PublishFleetSnapshot(ctx, sample); err == nil {
			t.Errorf("PublishFleetSnapshot accepted invalid sample %+v", sample)
		}
	}
}

func TestFleetSnapshotFailsClosedBeyondReplicaReadLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	control := fleetControlConn(t, ctx, dsn)
	if _, err := control.Exec(ctx, `
		WITH sample_time AS MATERIALIZED (SELECT clock_timestamp() AS at)
		INSERT INTO fleet_process_snapshots (
		    generation, replica_id, version, process_started_at, heartbeat_at,
		    expires_at, connections, sessions, accounts_complete
		)
		SELECT lpad(to_hex(generation), 32, '0'), NULL, '', sample_time.at,
		       sample_time.at, sample_time.at + interval '30 seconds', 1, 1, TRUE
		  FROM generate_series(1, $1) AS generation
	 CROSS JOIN sample_time
	`, store.FleetMaxReplicas+1); err != nil {
		t.Fatalf("seed over-limit snapshots: %v", err)
	}
	if snapshot, err := st.FleetSnapshot(ctx); err == nil {
		t.Fatalf("over-limit fleet returned a partial success: %+v", snapshot)
	}
}

func openFleetStore(t *testing.T, ctx context.Context, dsn string) *store.Store {
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

func fleetControlConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
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

func installFleetSnapshotInsertGate(t *testing.T, ctx context.Context, conn *pgx.Conn, generation string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `
		CREATE FUNCTION test_fleet_snapshot_insert_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE previous_lock_timeout text;
		BEGIN
			IF NEW.generation = TG_ARGV[0] THEN
				previous_lock_timeout := current_setting('lock_timeout');
				PERFORM set_config('lock_timeout', '0', true);
				PERFORM pg_advisory_xact_lock(760011, 760012);
				PERFORM set_config('lock_timeout', previous_lock_timeout, true);
			END IF;
			RETURN NEW;
		END;
		$$
	`); err != nil {
		t.Fatalf("create snapshot insert gate: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER test_fleet_snapshot_insert_gate
		BEFORE INSERT ON fleet_process_snapshots
		FOR EACH ROW EXECUTE FUNCTION test_fleet_snapshot_insert_gate('%s')
	`, generation)); err != nil {
		t.Fatalf("install snapshot insert gate: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fleet_snapshot_insert_gate ON fleet_process_snapshots`); err != nil {
			t.Errorf("drop snapshot insert gate trigger: %v", err)
		}
		if _, err := conn.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fleet_snapshot_insert_gate()`); err != nil {
			t.Errorf("drop snapshot insert gate function: %v", err)
		}
	})
}

func installFleetAccountDeleteGate(t *testing.T, ctx context.Context, conn *pgx.Conn, generation string) {
	t.Helper()
	if _, err := conn.Exec(ctx, `
		CREATE FUNCTION test_fleet_account_delete_gate() RETURNS trigger LANGUAGE plpgsql AS $$
		DECLARE previous_lock_timeout text;
		BEGIN
			IF OLD.generation = TG_ARGV[0] THEN
				previous_lock_timeout := current_setting('lock_timeout');
				PERFORM set_config('lock_timeout', '0', true);
				PERFORM pg_advisory_xact_lock(760011, 760012);
				PERFORM set_config('lock_timeout', previous_lock_timeout, true);
			END IF;
			RETURN OLD;
		END;
		$$
	`); err != nil {
		t.Fatalf("create account delete gate: %v", err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`
		CREATE TRIGGER test_fleet_account_delete_gate
		BEFORE DELETE ON fleet_live_accounts
		FOR EACH ROW EXECUTE FUNCTION test_fleet_account_delete_gate('%s')
	`, generation)); err != nil {
		t.Fatalf("install account delete gate: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DROP TRIGGER IF EXISTS test_fleet_account_delete_gate ON fleet_live_accounts`); err != nil {
			t.Errorf("drop account delete gate trigger: %v", err)
		}
		if _, err := conn.Exec(context.Background(), `DROP FUNCTION IF EXISTS test_fleet_account_delete_gate()`); err != nil {
			t.Errorf("drop account delete gate function: %v", err)
		}
	})
}

func holdFleetTestAdvisoryGate(t *testing.T, ctx context.Context, conn *pgx.Conn) func() {
	t.Helper()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(760011, 760012)`); err != nil {
		t.Fatalf("hold fleet test advisory gate: %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		var unlocked bool
		if err := conn.QueryRow(context.Background(), `SELECT pg_advisory_unlock(760011, 760012)`).Scan(&unlocked); err != nil {
			t.Errorf("release fleet test advisory gate: %v", err)
		} else if !unlocked {
			t.Errorf("fleet test advisory gate was not held")
		}
		released = true
	}
	t.Cleanup(release)
	return release
}

func waitForFleetAdvisoryWaiters(ctx context.Context, conn *pgx.Conn, queryFragment string, want int) error {
	deadline := time.Now().Add(5 * time.Second)
	maxWaiters := 0
	maxQueries := "<none>"
	for time.Now().Before(deadline) {
		var waiters int
		var queries string
		if err := conn.QueryRow(ctx, `
			SELECT count(*), COALESCE(string_agg(query, E'\\n'), '<none>')
			  FROM pg_stat_activity
			 WHERE datname = current_database()
			   AND pid <> pg_backend_pid()
			   AND wait_event_type = 'Lock'
			   AND wait_event = 'advisory'
			   AND query LIKE '%' || $1 || '%'
		`, queryFragment).Scan(&waiters, &queries); err != nil {
			return fmt.Errorf("inspect fleet advisory waiters: %w", err)
		}
		if waiters >= want {
			return nil
		}
		if waiters > maxWaiters {
			maxWaiters = waiters
			maxQueries = queries
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("saw at most %d of %d waiters for %q; waiting queries: %s", maxWaiters, want, queryFragment, maxQueries)
}

func TestFleetSnapshotReplicaOrderIsStable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := openFleetStore(t, ctx, dsn)
	for _, generation := range []string{
		"00000000000000000000000000000061",
		"00000000000000000000000000000062",
	} {
		if err := st.PublishFleetSnapshot(ctx, store.FleetProcessSample{
			Generation:       generation,
			Connections:      1,
			Sessions:         1,
			AccountIDs:       []int64{1},
			AccountsComplete: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := st.FleetSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{snapshot.Replicas[0].Generation, snapshot.Replicas[1].Generation}
	want := []string{"00000000000000000000000000000061", "00000000000000000000000000000062"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("replica generations = %v, want stable ascending order %v", got, want)
	}
	if snapshot.Replicas[0].DuplicateReplicaID || snapshot.Replicas[1].DuplicateReplicaID {
		t.Errorf("unset replica IDs were treated as duplicates: %+v", snapshot.Replicas)
	}
}
