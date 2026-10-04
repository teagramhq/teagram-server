package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// FleetSampleInterval is the cadence at which each process publishes its
	// live gauges and complete connected-account set.
	FleetSampleInterval = 10 * time.Second
	// FleetFreshnessTTL excludes a generation immediately after its last
	// database-timestamped heartbeat is more than this old.
	FleetFreshnessTTL = 30 * time.Second
	// FleetSupersedeAfter excludes an older generation once a newer generation
	// with the same configured ID is fresh and the older heartbeat is missing.
	FleetSupersedeAfter = 2 * FleetSampleInterval
	// FleetWriterDeadline bounds the complete transaction for one publication.
	FleetWriterDeadline = 2 * time.Second
	// FleetReaderDeadline bounds a consistent fleet read, including the exact
	// distinct-account union query.
	FleetReaderDeadline = 2 * time.Second
	// FleetMaxDistinctAccountsPerGeneration bounds retained live account IDs.
	FleetMaxDistinctAccountsPerGeneration = 100_000
	// FleetMaxReplicas bounds response memory and the number of generations used
	// by exact distinct-account queries. Larger fleets fail closed as unavailable.
	FleetMaxReplicas = 256

	fleetLockTimeout            = 250 * time.Millisecond
	fleetCleanupAccountBatch    = 10_000
	fleetCleanupGenerationBatch = 128
)

var (
	fleetGenerationPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	fleetReplicaIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	fleetVersionPattern    = regexp.MustCompile(`^[A-Za-z0-9._+-]{0,128}$`)
)

// FleetProcessSample is one process's bounded local sample. AccountIDs is
// accepted only when AccountsComplete is true; incomplete sets contain no IDs.
type FleetProcessSample struct {
	Generation       string
	ReplicaID        *string
	Version          string
	Connections      int
	Sessions         int
	AccountIDs       []int64
	AccountsComplete bool
}

// FleetReplicaSnapshot contains aggregate values and metadata for one eligible
// process generation. It never includes account identifiers.
type FleetReplicaSnapshot struct {
	Generation         string    `json:"process_generation"`
	ReplicaID          *string   `json:"replica_id"`
	Version            *string   `json:"version"`
	StartedAt          time.Time `json:"process_started_at"`
	HeartbeatAt        time.Time `json:"heartbeat_at"`
	Connections        int64     `json:"connections"`
	Sessions           int64     `json:"sessions"`
	DistinctAccounts   *int64    `json:"distinct_accounts"`
	DuplicateReplicaID bool      `json:"duplicate_replica_id"`
	accountsComplete   bool
}

// FleetSnapshot is the exact bounded read of all eligible process generations.
// DistinctAccounts is nil if any eligible generation exceeded its account cap.
type FleetSnapshot struct {
	SampledAt        time.Time              `json:"sampled_at"`
	Connections      int64                  `json:"connections"`
	Sessions         int64                  `json:"sessions"`
	DistinctAccounts *int64                 `json:"distinct_accounts"`
	Replicas         []FleetReplicaSnapshot `json:"replicas"`
}

// PublishFleetSnapshot atomically replaces one generation's bounded account
// set and refreshes its database-timestamped snapshot. A failed or incomplete
// write leaves the prior complete set and heartbeat unchanged.
func (s *Store) PublishFleetSnapshot(ctx context.Context, sample FleetProcessSample) error {
	if err := validateFleetProcessSample(sample); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, FleetWriterDeadline)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fleet snapshot publication: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), fleetLockTimeout)
		defer rollbackCancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.log.Error("rollback fleet snapshot publication failed", "error_type", fmt.Sprintf("%T", err))
		}
	}()
	if err := setFleetTransactionLimits(ctx, tx); err != nil {
		return fmt.Errorf("bound fleet snapshot publication: %w", err)
	}
	if err := cleanupExpiredFleetTelemetry(ctx, tx); err != nil {
		return fmt.Errorf("clean expired fleet telemetry: %w", err)
	}
	if err := syncFleetAccountSet(ctx, tx, sample); err != nil {
		return fmt.Errorf("sync fleet account set: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO fleet_process_snapshots (
		    generation, replica_id, version, process_started_at, heartbeat_at,
		    expires_at, connections, sessions, accounts_complete
		) VALUES (
		    $1, $2, $3, clock_timestamp(), clock_timestamp(),
		    clock_timestamp() + make_interval(secs => $7::double precision), $4, $5, $6
		)
		ON CONFLICT (generation) DO UPDATE SET
		    replica_id = EXCLUDED.replica_id,
		    version = EXCLUDED.version,
		    heartbeat_at = clock_timestamp(),
		    expires_at = clock_timestamp() + make_interval(secs => $7::double precision),
		    connections = EXCLUDED.connections,
		    sessions = EXCLUDED.sessions,
		    accounts_complete = EXCLUDED.accounts_complete
	`, sample.Generation, sample.ReplicaID, sample.Version, sample.Connections, sample.Sessions, sample.AccountsComplete, float64(FleetFreshnessTTL/time.Second)); err != nil {
		return fmt.Errorf("write fleet process snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fleet snapshot publication: %w", err)
	}
	committed = true
	return nil
}

func syncFleetAccountSet(ctx context.Context, tx pgx.Tx, sample FleetProcessSample) error {
	if !sample.AccountsComplete || len(sample.AccountIDs) == 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM fleet_live_accounts WHERE generation = $1`, sample.Generation); err != nil {
			return fmt.Errorf("clear unavailable or empty account set: %w", err)
		}
		return nil
	}
	var hasCurrentAccounts bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM fleet_live_accounts WHERE generation = $1
		)
	`, sample.Generation).Scan(&hasCurrentAccounts); err != nil {
		return fmt.Errorf("check current account set: %w", err)
	}
	if !hasCurrentAccounts {
		if _, err := tx.CopyFrom(
			ctx,
			pgx.Identifier{"fleet_live_accounts"},
			[]string{"generation", "user_id"},
			pgx.CopyFromSlice(len(sample.AccountIDs), func(i int) ([]any, error) {
				return []any{sample.Generation, sample.AccountIDs[i]}, nil
			}),
		); err != nil {
			return fmt.Errorf("copy initial account set: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE fleet_live_accounts_pending (
		    user_id BIGINT PRIMARY KEY CHECK (user_id > 0)
		) ON COMMIT DROP
	`); err != nil {
		return fmt.Errorf("create pending account set: %w", err)
	}
	if _, err := tx.CopyFrom(
		ctx,
		pgx.Identifier{"fleet_live_accounts_pending"},
		[]string{"user_id"},
		pgx.CopyFromSlice(len(sample.AccountIDs), func(i int) ([]any, error) {
			return []any{sample.AccountIDs[i]}, nil
		}),
	); err != nil {
		return fmt.Errorf("copy pending account set: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM fleet_live_accounts AS current
		 WHERE current.generation = $1
		   AND NOT EXISTS (
		       SELECT 1
		         FROM fleet_live_accounts_pending AS pending
		        WHERE pending.user_id = current.user_id
		   )
	`, sample.Generation); err != nil {
		return fmt.Errorf("delete departed account rows: %w", err)
	}
	var retainedAccounts int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM fleet_live_accounts WHERE generation = $1
	`, sample.Generation).Scan(&retainedAccounts); err != nil {
		return fmt.Errorf("count retained account rows: %w", err)
	}
	if retainedAccounts > len(sample.AccountIDs) {
		return errors.New("retained account set exceeds the sampled account set")
	}
	if retainedAccounts == len(sample.AccountIDs) {
		return nil
	}
	if retainedAccounts == 0 {
		if _, err := tx.CopyFrom(
			ctx,
			pgx.Identifier{"fleet_live_accounts"},
			[]string{"generation", "user_id"},
			pgx.CopyFromSlice(len(sample.AccountIDs), func(i int) ([]any, error) {
				return []any{sample.Generation, sample.AccountIDs[i]}, nil
			}),
		); err != nil {
			return fmt.Errorf("copy replacement account set: %w", err)
		}
		return nil
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO fleet_live_accounts (generation, user_id)
		SELECT $1, pending.user_id
		  FROM fleet_live_accounts_pending AS pending
		 WHERE NOT EXISTS (
		       SELECT 1
		         FROM fleet_live_accounts AS current
		        WHERE current.generation = $1
		          AND current.user_id = pending.user_id
		 )
		ON CONFLICT (generation, user_id) DO NOTHING
	`, sample.Generation); err != nil {
		return fmt.Errorf("insert new account rows: %w", err)
	}
	return nil
}

// FleetSnapshot reads the eligible fleet under one repeatable-read database
// snapshot. Expired and superseded generations are excluded before totals and
// distinct-account counts are computed.
func (s *Store) FleetSnapshot(ctx context.Context) (FleetSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, FleetReaderDeadline)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return FleetSnapshot{}, fmt.Errorf("begin fleet snapshot read: %w", err)
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), fleetLockTimeout)
		defer rollbackCancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.log.Error("rollback fleet snapshot read failed", "error_type", fmt.Sprintf("%T", err))
		}
	}()
	if err := setFleetTransactionLimits(ctx, tx); err != nil {
		return FleetSnapshot{}, fmt.Errorf("bound fleet snapshot read: %w", err)
	}

	var sampledAt time.Time
	if err := tx.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&sampledAt); err != nil {
		return FleetSnapshot{}, fmt.Errorf("read fleet snapshot time: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT generation, replica_id, version, process_started_at, heartbeat_at,
		       connections, sessions, accounts_complete
		  FROM fleet_process_snapshots AS current
		 WHERE current.expires_at > $1
		   AND NOT (
		       current.replica_id IS NOT NULL
	           AND current.heartbeat_at <= $1 - make_interval(secs => $2::double precision)
		       AND EXISTS (
		           SELECT 1
		             FROM fleet_process_snapshots AS newer
		            WHERE newer.replica_id = current.replica_id
		              AND newer.process_started_at > current.process_started_at
		              AND newer.expires_at > $1
		       )
		   )
		 ORDER BY current.process_started_at, current.generation
		 LIMIT $3
	`, sampledAt, float64(FleetSupersedeAfter/time.Second), FleetMaxReplicas+1)
	if err != nil {
		return FleetSnapshot{}, fmt.Errorf("read eligible fleet generations: %w", err)
	}
	defer rows.Close()

	snapshot := FleetSnapshot{SampledAt: sampledAt.UTC(), Replicas: make([]FleetReplicaSnapshot, 0, FleetMaxReplicas)}
	allAccountSetsComplete := true
	for rows.Next() {
		if len(snapshot.Replicas) == FleetMaxReplicas {
			return FleetSnapshot{}, fmt.Errorf("fleet snapshot exceeds %d active generations", FleetMaxReplicas)
		}
		var replica FleetReplicaSnapshot
		var replicaID *string
		var version string
		var accountsComplete bool
		if err := rows.Scan(
			&replica.Generation,
			&replicaID,
			&version,
			&replica.StartedAt,
			&replica.HeartbeatAt,
			&replica.Connections,
			&replica.Sessions,
			&accountsComplete,
		); err != nil {
			return FleetSnapshot{}, fmt.Errorf("scan fleet generation: %w", err)
		}
		if !fleetGenerationPattern.MatchString(replica.Generation) {
			return FleetSnapshot{}, errors.New("fleet generation has invalid format")
		}
		if replicaID != nil && fleetReplicaIDPattern.MatchString(*replicaID) {
			replica.ReplicaID = replicaID
		}
		if version != "" && fleetVersionPattern.MatchString(version) {
			replica.Version = &version
		}
		replica.StartedAt = replica.StartedAt.UTC()
		replica.HeartbeatAt = replica.HeartbeatAt.UTC()
		replica.accountsComplete = accountsComplete
		if !accountsComplete {
			allAccountSetsComplete = false
		}
		if err := addFleetCount(&snapshot.Connections, replica.Connections); err != nil {
			return FleetSnapshot{}, fmt.Errorf("sum fleet connections: %w", err)
		}
		if err := addFleetCount(&snapshot.Sessions, replica.Sessions); err != nil {
			return FleetSnapshot{}, fmt.Errorf("sum fleet sessions: %w", err)
		}
		snapshot.Replicas = append(snapshot.Replicas, replica)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return FleetSnapshot{}, fmt.Errorf("read fleet generations: %w", err)
	}
	markDuplicateReplicaIDs(snapshot.Replicas)

	completeGenerations := make([]string, 0, len(snapshot.Replicas))
	for i := range snapshot.Replicas {
		if snapshot.Replicas[i].accountsComplete {
			completeGenerations = append(completeGenerations, snapshot.Replicas[i].Generation)
		}
	}
	if len(completeGenerations) == 0 {
		if allAccountSetsComplete {
			zero := int64(0)
			snapshot.DistinctAccounts = &zero
		}
		return snapshot, nil
	}
	localCounts, err := fleetDistinctCounts(ctx, tx, completeGenerations)
	if err != nil {
		return FleetSnapshot{}, fmt.Errorf("count fleet account sets: %w", err)
	}
	for i := range snapshot.Replicas {
		if !snapshot.Replicas[i].accountsComplete {
			continue
		}
		if count, ok := localCounts[snapshot.Replicas[i].Generation]; ok {
			snapshot.Replicas[i].DistinctAccounts = &count
		} else {
			zero := int64(0)
			snapshot.Replicas[i].DistinctAccounts = &zero
		}
	}
	if !allAccountSetsComplete {
		return snapshot, nil
	}
	var distinct int64
	if err := tx.QueryRow(ctx, `
		SELECT count(DISTINCT user_id)
		  FROM fleet_live_accounts
		 WHERE generation = ANY($1::text[])
	`, completeGenerations).Scan(&distinct); err != nil {
		return FleetSnapshot{}, fmt.Errorf("count distinct fleet accounts: %w", err)
	}
	snapshot.DistinctAccounts = &distinct
	return snapshot, nil
}

// DeleteFleetSnapshot removes one process generation's ephemeral telemetry on
// graceful shutdown. Expiry remains the crash and database-loss fallback.
func (s *Store) DeleteFleetSnapshot(ctx context.Context, generation string) error {
	if !fleetGenerationPattern.MatchString(generation) {
		return errors.New("fleet generation has invalid format")
	}
	ctx, cancel := context.WithTimeout(ctx, FleetWriterDeadline)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin fleet snapshot cleanup: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), fleetLockTimeout)
		defer rollbackCancel()
		if err := tx.Rollback(rollbackCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			s.log.Error("rollback fleet snapshot cleanup failed", "error_type", fmt.Sprintf("%T", err))
		}
	}()
	if err := setFleetTransactionLimits(ctx, tx); err != nil {
		return fmt.Errorf("bound fleet snapshot cleanup: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fleet_live_accounts WHERE generation = $1`, generation); err != nil {
		return fmt.Errorf("delete fleet account set: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM fleet_process_snapshots WHERE generation = $1`, generation); err != nil {
		return fmt.Errorf("delete fleet process snapshot: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit fleet snapshot cleanup: %w", err)
	}
	committed = true
	return nil
}

func validateFleetProcessSample(sample FleetProcessSample) error {
	if !fleetGenerationPattern.MatchString(sample.Generation) {
		return errors.New("fleet generation has invalid format")
	}
	if sample.ReplicaID != nil && !fleetReplicaIDPattern.MatchString(*sample.ReplicaID) {
		return errors.New("fleet replica ID has invalid format")
	}
	if !fleetVersionPattern.MatchString(sample.Version) {
		return errors.New("fleet version has invalid format")
	}
	if sample.Connections < 0 || sample.Sessions < 0 {
		return errors.New("fleet connection and session counts must not be negative")
	}
	if sample.AccountsComplete {
		if sample.Sessions > FleetMaxDistinctAccountsPerGeneration || len(sample.AccountIDs) != sample.Sessions {
			return errors.New("complete fleet account set does not match its bounded session count")
		}
	} else if sample.Sessions <= FleetMaxDistinctAccountsPerGeneration || len(sample.AccountIDs) != 0 {
		return errors.New("incomplete fleet account set must exceed the cap and contain no IDs")
	}
	for _, userID := range sample.AccountIDs {
		if userID <= 0 {
			return errors.New("fleet account ID must be positive")
		}
	}
	return nil
}

func cleanupExpiredFleetTelemetry(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `
		WITH expired_accounts AS (
		    SELECT account.generation, account.user_id
		      FROM fleet_live_accounts AS account
		      JOIN fleet_process_snapshots AS snapshot USING (generation)
		     WHERE snapshot.expires_at <= clock_timestamp()
		     ORDER BY snapshot.expires_at, account.generation, account.user_id
		     LIMIT $1
		)
		DELETE FROM fleet_live_accounts AS account
		 USING expired_accounts
		 WHERE account.generation = expired_accounts.generation
		   AND account.user_id = expired_accounts.user_id
	`, fleetCleanupAccountBatch); err != nil {
		return fmt.Errorf("delete expired fleet account rows: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM fleet_process_snapshots AS snapshot
		 WHERE snapshot.generation IN (
		       SELECT expired.generation
		         FROM fleet_process_snapshots AS expired
		        WHERE expired.expires_at <= clock_timestamp()
		          AND NOT EXISTS (
		              SELECT 1 FROM fleet_live_accounts AS account
		               WHERE account.generation = expired.generation
		          )
		        ORDER BY expired.expires_at, expired.generation
		        LIMIT $1
		 )
	`, fleetCleanupGenerationBatch); err != nil {
		return fmt.Errorf("delete expired fleet snapshots: %w", err)
	}
	return nil
}

func setFleetTransactionLimits(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '1500ms'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
		return err
	}
	return nil
}

func fleetDistinctCounts(ctx context.Context, tx pgx.Tx, generations []string) (map[string]int64, error) {
	rows, err := tx.Query(ctx, `
		SELECT generation, count(*)
		  FROM fleet_live_accounts
		 WHERE generation = ANY($1::text[])
	 GROUP BY generation
	`, generations)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int64, len(generations))
	for rows.Next() {
		var generation string
		var count int64
		if err := rows.Scan(&generation, &count); err != nil {
			return nil, err
		}
		counts[generation] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

func addFleetCount(total *int64, count int64) error {
	if count < 0 || *total > math.MaxInt64-count {
		return errors.New("fleet aggregate is outside the supported range")
	}
	*total += count
	return nil
}

func markDuplicateReplicaIDs(replicas []FleetReplicaSnapshot) {
	counts := make(map[string]int, len(replicas))
	for i := range replicas {
		if replicas[i].ReplicaID != nil {
			counts[*replicas[i].ReplicaID]++
		}
	}
	for i := range replicas {
		if replicas[i].ReplicaID != nil && counts[*replicas[i].ReplicaID] > 1 {
			replicas[i].DuplicateReplicaID = true
		}
	}
}
