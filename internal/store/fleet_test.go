package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	t.Parallel()
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
