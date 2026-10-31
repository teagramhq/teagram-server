package store_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func lockMigrationReplays(tb testing.TB, ctx context.Context) func() {
	tb.Helper()
	unlock, err := pgtest.LockMigrations(ctx)
	if err != nil {
		tb.Fatalf("lock migration replay: %v", err)
	}
	return func() {
		if err := unlock(); err != nil {
			tb.Errorf("unlock migration replay: %v", err)
		}
	}
}

func TestMigrationReplayLockUsesAdminDatabase(t *testing.T) {
	ctx := context.Background()
	const migrationLockKey = int64(0x7467)
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		t.Fatalf("hold admin migration lock: %v", err)
	}
	adminLockHeld := true
	defer func() {
		if adminLockHeld {
			if _, err := admin.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
				t.Errorf("release admin migration lock during cleanup: %v", err)
			}
		}
	}()

	type lockResult struct {
		unlock func() error
		err    error
	}
	acquired := make(chan lockResult, 1)
	go func() {
		unlock, err := pgtest.LockMigrations(ctx)
		acquired <- lockResult{unlock: unlock, err: err}
	}()

	watchCtx, stopWatching := context.WithTimeout(ctx, 3*time.Second)
	defer stopWatching()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	waiting := false
	timedOut := false
	var early *lockResult
	for !waiting && early == nil && !timedOut {
		select {
		case result := <-acquired:
			early = &result
		case <-watchCtx.Done():
			timedOut = true
		case <-ticker.C:
			if err := admin.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM pg_locks AS locks
					JOIN pg_stat_activity AS activity USING (pid)
					WHERE locks.locktype = 'advisory' AND locks.classid = 0
					  AND locks.objid = $1::oid AND locks.objsubid = 1 AND NOT locks.granted
					  AND activity.application_name = 'pgtest-migration-replay'
				)
			`, migrationLockKey).Scan(&waiting); err != nil {
				t.Fatalf("observe admin migration lock wait: %v", err)
			}
		}
	}

	if _, err := admin.Exec(ctx, `SELECT pg_advisory_unlock($1)`, migrationLockKey); err != nil {
		t.Fatalf("release admin migration lock: %v", err)
	}
	adminLockHeld = false
	if early != nil {
		if early.err == nil && early.unlock != nil {
			if err := early.unlock(); err != nil {
				t.Errorf("release prematurely acquired migration lock: %v", err)
			}
		}
		t.Fatalf("migration replay lock returned before the admin-database lock was released: %v", early.err)
	}
	if !waiting {
		result := <-acquired
		if result.err == nil && result.unlock != nil {
			if err := result.unlock(); err != nil {
				t.Errorf("release migration lock after timeout: %v", err)
			}
		}
		t.Fatalf("migration replay did not wait on the admin-database lock: %v", result.err)
	}

	result := <-acquired
	if result.err != nil {
		t.Fatalf("acquire migration lock after admin lock release: %v", result.err)
	}
	if err := result.unlock(); err != nil {
		t.Fatalf("release migration lock: %v", err)
	}
}

// TestOpenRejectsUnmigratedSchema proves Open's fail-fast schema check fires:
// dropping a table the latest migration created makes Open error at startup
// instead of failing later with a confusing "relation does not exist".
func TestOpenRejectsUnmigratedSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP TABLE message_events`); err != nil {
		t.Fatalf("drop table: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingChannelReadStateSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP TABLE channel_read_state`); err != nil {
		t.Fatalf("drop channel read state: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingNotifySettingsTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP TABLE user_notify_settings`); err != nil {
		t.Fatalf("drop notify settings table: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingOwnerDialogCounts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP TABLE owner_dialog_counts`); err != nil {
		t.Fatalf("drop owner dialog counts: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingAuthorizationTTLColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `ALTER TABLE users DROP COLUMN authorization_ttl_days`); err != nil {
		t.Fatalf("drop authorization TTL column: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingAuthKeysEverBoundColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `
		ALTER TABLE auth_keys DROP CONSTRAINT auth_keys_user_requires_ever_bound;
		DROP TRIGGER auth_keys_mark_ever_bound ON auth_keys;
		DROP FUNCTION public.auth_keys_mark_ever_bound();
		ALTER TABLE auth_keys DROP COLUMN ever_bound_at;
	`); err != nil {
		t.Fatalf("remove auth-key ever-bound schema: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsUnvalidatedAuthKeysEverBoundConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `
		ALTER TABLE auth_keys DROP CONSTRAINT auth_keys_user_requires_ever_bound;
		ALTER TABLE auth_keys
			ADD CONSTRAINT auth_keys_user_requires_ever_bound
			CHECK (user_id IS NULL OR ever_bound_at IS NOT NULL) NOT VALID;
	`); err != nil {
		t.Fatalf("make auth-key ever-bound constraint unvalidated: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestAuthKeysEverBoundMigrationBackfillsExistingRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner, err := s.CreateUser(ctx, "+15551239999")
	if err != nil {
		t.Fatalf("create legacy binding owner: %v", err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	if _, err := conn.Exec(ctx, `
		ALTER TABLE auth_keys DROP CONSTRAINT IF EXISTS auth_keys_user_requires_ever_bound;
		DROP TRIGGER IF EXISTS auth_keys_capture_ever_bound_backfill_new_key ON auth_keys;
		DROP FUNCTION IF EXISTS public.auth_keys_capture_ever_bound_backfill_new_key();
		DROP TRIGGER IF EXISTS auth_keys_mark_ever_bound ON auth_keys;
		DROP FUNCTION IF EXISTS public.auth_keys_mark_ever_bound();
		DROP TABLE IF EXISTS public.auth_keys_ever_bound_backfill_snapshot;
		DROP TABLE IF EXISTS public.auth_keys_ever_bound_backfill_new_keys;
		ALTER TABLE auth_keys DROP COLUMN ever_bound_at;
	`); err != nil {
		t.Fatalf("restore pre-marker schema: %v", err)
	}
	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := conn.Exec(ctx, "INSERT INTO auth_keys (id, key_value, user_id, created_at) VALUES (910000000000000001, decode('00', 'hex'), NULL, $1), (910000000000000002, decode('00', 'hex'), $2, $1)", createdAt, owner.ID); err != nil {
		t.Fatalf("seed legacy auth keys: %v", err)
	}
	schemaMigration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261024000001_auth_keys_ever_bound.sql"))
	if err != nil {
		t.Fatalf("read ever-bound schema migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(schemaMigration)); err != nil {
		t.Fatalf("apply ever-bound schema migration: %v", err)
	}
	const (
		freshID         = int64(910000000000000003)
		freshUnboundID  = int64(910000000000000004)
		postMigrationID = int64(910000000000000005)
	)
	if _, err := conn.Exec(ctx, "INSERT INTO auth_keys (id, key_value) VALUES ($1, decode('00', 'hex')), ($2, decode('00', 'hex'))", freshID, freshUnboundID); err != nil {
		t.Fatalf("insert fresh auth keys after snapshot: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE auth_keys SET user_id = $1 WHERE id = $2", owner.ID, freshID); err != nil {
		t.Fatalf("bind a key through the old server between migrations: %v", err)
	}
	var everBoundAt time.Time
	if err := conn.QueryRow(ctx, "SELECT ever_bound_at FROM auth_keys WHERE id = $1", freshID).Scan(&everBoundAt); err != nil {
		t.Fatalf("read old-server bind marker before backfill: %v", err)
	}
	if everBoundAt.IsZero() {
		t.Fatal("database did not mark an old-server bind between migrations")
	}

	partialMarker := createdAt.Add(time.Hour)
	if _, err := conn.Exec(ctx, "UPDATE auth_keys SET ever_bound_at = $1 WHERE id = 910000000000000001", partialMarker); err != nil {
		t.Fatalf("simulate a committed backfill batch: %v", err)
	}

	backfillMigration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261024000002_auth_keys_ever_bound_backfill.sql"))
	if err != nil {
		t.Fatalf("read ever-bound backfill migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(backfillMigration)); err != nil {
		t.Fatalf("apply ever-bound backfill migration: %v", err)
	}

	for _, key := range []struct {
		id            int64
		wantEverBound time.Time
	}{
		{id: 910000000000000001, wantEverBound: partialMarker},
		{id: 910000000000000002, wantEverBound: createdAt},
	} {
		var everBoundAt time.Time
		if err := conn.QueryRow(ctx, "SELECT ever_bound_at FROM auth_keys WHERE id = $1", key.id).Scan(&everBoundAt); err != nil {
			t.Fatalf("read migrated key %d: %v", key.id, err)
		}
		if !everBoundAt.Equal(key.wantEverBound) {
			t.Errorf("legacy key %d marker = %s, want %s", key.id, everBoundAt, key.wantEverBound)
		}
	}

	var marked bool
	if err := conn.QueryRow(ctx, "SELECT ever_bound_at IS NOT NULL FROM auth_keys WHERE id = $1", freshUnboundID).Scan(&marked); err != nil {
		t.Fatalf("read fresh unbound key marker: %v", err)
	}
	if marked {
		t.Fatal("freshly inserted unbound key has an ever-bound marker")
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE auth_keys DISABLE TRIGGER auth_keys_mark_ever_bound"); err != nil {
		t.Fatalf("disable compatibility trigger for constraint check: %v", err)
	}
	_, err = conn.Exec(ctx, "UPDATE auth_keys SET user_id = $1 WHERE id = $2", owner.ID, freshUnboundID)
	var constraintErr *pgconn.PgError
	if !errors.As(err, &constraintErr) || constraintErr.Code != "23514" || constraintErr.ConstraintName != "auth_keys_user_requires_ever_bound" {
		t.Fatalf("bind with the marker trigger disabled error = %v, want auth_keys_user_requires_ever_bound check violation", err)
	}
	if _, err := conn.Exec(ctx, "ALTER TABLE auth_keys ENABLE TRIGGER auth_keys_mark_ever_bound"); err != nil {
		t.Fatalf("reenable compatibility trigger: %v", err)
	}
	if _, err := conn.Exec(ctx, "INSERT INTO auth_keys (id, key_value) VALUES ($1, decode('00', 'hex'))", postMigrationID); err != nil {
		t.Fatalf("insert a fresh key after migration: %v", err)
	}
	if _, err := conn.Exec(ctx, "UPDATE auth_keys SET user_id = $1 WHERE id = $2", owner.ID, postMigrationID); err != nil {
		t.Fatalf("bind a key through the old server after migration: %v", err)
	}
	if err := conn.QueryRow(ctx, "SELECT ever_bound_at IS NOT NULL FROM auth_keys WHERE id = $1", postMigrationID).Scan(&marked); err != nil {
		t.Fatalf("read post-migration old-server bind marker: %v", err)
	}
	if !marked {
		t.Fatal("database did not mark an old-server bind after migration")
	}
}

func TestOpenRejectsMissingFileSubtypeRightsConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `ALTER TABLE files DROP CONSTRAINT files_subtype_rights_valid`); err != nil {
		t.Fatalf("drop subtype rights constraint: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestChatAdminMarkerIndexesSupportParticipantCascades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close

	for _, index := range []struct {
		name  string
		table string
	}{
		{name: "chat_admin_event_recipients_chat_target_idx", table: "chat_admin_event_recipients"},
		{name: "chat_admin_state_markers_chat_target_idx", table: "chat_admin_state_markers"},
	} {
		var definition string
		if err := conn.QueryRow(ctx, `
			SELECT indexdef
			FROM pg_indexes
			WHERE schemaname = current_schema() AND indexname = $1 AND tablename = $2
		`, index.name, index.table).Scan(&definition); err != nil {
			t.Fatalf("read %s definition: %v", index.name, err)
		}
		if !strings.Contains(definition, "(chat_id, target_id)") {
			t.Errorf("%s does not cover participant cascade columns: %s", index.name, definition)
		}
	}
}

func TestOwnerDialogCountsBackfillExistingDialogs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	owner := mustUser(t, s, "+15551263998")
	emptyOwner := mustUser(t, s, "+15551263997")
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close connection: %v", err)
		}
	})

	if _, err := conn.Exec(ctx, `
		DROP TRIGGER dialogs_initialize_owner_count_before_write ON public.dialogs;
		DROP TRIGGER dialogs_track_owner_count ON public.dialogs;
		DROP FUNCTION public.initialize_owner_dialog_count_before_write();
		DROP TRIGGER users_initialize_owner_dialog_count ON public.users;
		DROP FUNCTION public.initialize_owner_dialog_count_for_user();
		DROP FUNCTION public.track_owner_dialog_count();
		DROP FUNCTION public.apply_owner_dialog_count_delta(BIGINT, BIGINT);
		DROP TABLE public.owner_dialog_counts;
	`); err != nil {
		t.Fatalf("remove current dialog count schema: %v", err)
	}
	for peerID := int64(730001); peerID < 730004; peerID++ {
		if _, err := conn.Exec(ctx, `
			INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
			VALUES ($1, 1, $2, 0)
		`, owner.ID, peerID); err != nil {
			t.Fatalf("seed existing dialog %d: %v", peerID, err)
		}
	}

	for _, filename := range []string{
		"20261026000003_owner_dialog_counts.sql",
		"20261026000004_backfill_owner_dialog_counts.sql",
		"20261026000005_owner_dialog_counts_for_users.sql",
		"20261026000006_backfill_zero_dialog_counts.sql",
	} {
		migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", filename))
		if err != nil {
			t.Fatalf("read migration %s: %v", filename, err)
		}
		if _, err := conn.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("apply migration %s: %v", filename, err)
		}
	}

	var stored, actual int64
	if err := conn.QueryRow(ctx, `
		SELECT counts.dialog_count, (
			SELECT count(*) FROM public.dialogs WHERE owner_id = counts.owner_id
		)
		FROM public.owner_dialog_counts AS counts
		WHERE counts.owner_id = $1 AND counts.initialized
	`, owner.ID).Scan(&stored, &actual); err != nil {
		t.Fatalf("read backfilled dialog count: %v", err)
	}
	if stored != 3 || actual != 3 {
		t.Fatalf("backfilled dialog count = %d, dialogs rows = %d; want both 3", stored, actual)
	}
	if count, err := s.CountDialogs(ctx, owner.ID); err != nil || count != 3 {
		t.Fatalf("CountDialogs after backfill = %d, err %v; want 3", count, err)
	}
	if err := conn.QueryRow(ctx, `
		SELECT dialog_count
		FROM public.owner_dialog_counts
		WHERE owner_id = $1 AND initialized
	`, emptyOwner.ID).Scan(&stored); err != nil {
		t.Fatalf("read backfilled zero dialog count: %v", err)
	}
	if stored != 0 {
		t.Fatalf("backfilled zero dialog count = %d, want 0", stored)
	}
}

func TestDialogCountBackfillsReleaseLocksPerOwner(t *testing.T) {
	for _, filename := range []string{
		"20261026000004_backfill_owner_dialog_counts.sql",
		"20261026000006_backfill_zero_dialog_counts.sql",
	} {
		name := strings.TrimSuffix(filename, ".sql")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testDialogCountBackfillReleasesLocksPerOwner(t, filename)
		})
	}
}

func testDialogCountBackfillReleasesLocksPerOwner(t *testing.T, filename string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	setup, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect setup: %v", err)
	}
	t.Cleanup(func() {
		if _, err := setup.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_pause_owner_dialog_backfill ON public.owner_dialog_counts;
			DROP FUNCTION IF EXISTS public.test_pause_owner_dialog_backfill();
		`); err != nil {
			t.Errorf("drop test backfill pause trigger: %v", err)
		}
		if err := setup.Close(context.Background()); err != nil {
			t.Errorf("close setup connection: %v", err)
		}
	})

	ownerA := mustUser(t, s, "+15551263840")
	ownerB := mustUser(t, s, "+15551263841")
	if ownerA.ID >= ownerB.ID {
		t.Fatalf("test owners are out of order: %d then %d", ownerA.ID, ownerB.ID)
	}
	if _, err := setup.Exec(ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		VALUES ($1, 1, 740001, 0), ($2, 1, 740002, 0)
	`, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("seed backfill owners: %v", err)
	}
	if _, err := setup.Exec(ctx, `
		UPDATE owner_dialog_counts
		SET dialog_count = 0, initialized = FALSE
		WHERE owner_id IN ($1, $2)
	`, ownerA.ID, ownerB.ID); err != nil {
		t.Fatalf("mark backfill owners uninitialized: %v", err)
	}
	if _, err := setup.Exec(ctx, `
		CREATE FUNCTION public.test_pause_owner_dialog_backfill()
		RETURNS TRIGGER
		LANGUAGE plpgsql
		AS $function$
		BEGIN
		    IF NEW.owner_id = current_setting('test.backfill_pause_owner', TRUE)::BIGINT
		       AND NOT OLD.initialized AND NEW.initialized THEN
		        PERFORM pg_sleep(2);
		    END IF;
		    RETURN NEW;
		END;
		$function$
	`); err != nil {
		t.Fatalf("create test backfill pause function: %v", err)
	}
	if _, err := setup.Exec(ctx, `
		CREATE TRIGGER test_pause_owner_dialog_backfill
		BEFORE UPDATE OF initialized ON public.owner_dialog_counts
		FOR EACH ROW
		EXECUTE FUNCTION public.test_pause_owner_dialog_backfill()
	`); err != nil {
		t.Fatalf("create test backfill pause trigger: %v", err)
	}

	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", filename))
	if err != nil {
		t.Fatalf("read migration %s: %v", filename, err)
	}
	migrationDone := make(chan error, 1)
	go func() {
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			migrationDone <- fmt.Errorf("connect migration: %w", err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // test connection cleanup
		if _, err := conn.Exec(ctx, `SELECT set_config('test.backfill_pause_owner', $1, FALSE)`, strconv.FormatInt(ownerA.ID, 10)); err != nil {
			migrationDone <- fmt.Errorf("set backfill pause owner: %w", err)
			return
		}
		_, err = conn.Exec(ctx, string(migration))
		migrationDone <- err
	}()

	probe, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock probe: %v", err)
	}
	defer func() { _ = probe.Close(context.Background()) }() //nolint:errcheck // best-effort close
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	locked := false
	for !locked {
		select {
		case err := <-migrationDone:
			if err != nil {
				t.Fatalf("run migration before lock probe: %v", err)
			}
			t.Fatal("backfill completed before locking the uninitialized owner")
		case <-ctx.Done():
			t.Fatalf("backfill did not lock owner %d: %v", ownerA.ID, ctx.Err())
		case <-ticker.C:
			var initialized bool
			err := probe.QueryRow(ctx, `
				SELECT initialized
				FROM owner_dialog_counts
				WHERE owner_id = $1
				FOR UPDATE NOWAIT
			`, ownerA.ID).Scan(&initialized)
			if err == nil {
				continue
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
				t.Fatalf("probe owner count lock: %v", err)
			}
			locked = true
		}
	}

	live, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect live sender: %v", err)
	}
	defer func() { _ = live.Close(context.Background()) }() //nolint:errcheck // best-effort close
	tx, err := live.Begin(ctx)
	if err != nil {
		t.Fatalf("begin live send: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // rollback after commit is expected
	if _, err := tx.Exec(ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		VALUES ($1, 1, 740003, 0)
	`, ownerB.ID); err != nil {
		t.Fatalf("insert sender dialog: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		VALUES ($1, 1, 740004, 0)
	`, ownerA.ID); err != nil {
		t.Fatalf("insert recipient dialog while backfill runs: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit live send: %v", err)
	}
	if err := <-migrationDone; err != nil {
		t.Fatalf("complete migration with concurrent send: %v", err)
	}

	for _, owner := range []struct {
		id   int64
		name string
	}{{ownerA.ID, "recipient"}, {ownerB.ID, "sender"}} {
		var stored, actual int64
		if err := setup.QueryRow(ctx, `
			SELECT counts.dialog_count, (
				SELECT count(*) FROM dialogs WHERE owner_id = counts.owner_id
			)
			FROM owner_dialog_counts AS counts
			WHERE counts.owner_id = $1 AND counts.initialized
		`, owner.id).Scan(&stored, &actual); err != nil {
			t.Fatalf("read %s count: %v", owner.name, err)
		}
		if stored != 2 || actual != 2 {
			t.Errorf("%s count = %d, dialogs rows = %d; want both 2", owner.name, stored, actual)
		}
	}
}

func TestOpenRejectsMissingCurrentChatPhotoIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX chats_current_photo_file_id_idx`); err != nil {
		t.Fatalf("drop current-photo reverse index: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsMissingChatPhotoReceiptIndex(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX profile_upload_receipt_file_id_idx`); err != nil {
		t.Fatalf("drop profile-upload receipt reverse index: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestServerAdministrationMigrationClosesNonEmptyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const administrationMigration = "20260913000039_server_administration.sql"

	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, name)

	conn, err := pgx.Connect(ctx, pgtest.DSNFrom(name))
	if err != nil {
		t.Fatalf("connect to database: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	for _, entry := range migs {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= administrationMigration {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO users (phone) VALUES ('15550000999')`); err != nil {
		t.Fatalf("seed existing user: %v", err)
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", administrationMigration))
	if err != nil {
		t.Fatalf("read administration migration: %v", err)
	}
	if _, err := conn.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply administration migration: %v", err)
	}

	var rows int
	var closed bool
	var administrator *int64
	if err := conn.QueryRow(ctx, `
		SELECT count(*), bool_and(election_closed), max(administrator_user_id)
		FROM server_administration
	`).Scan(&rows, &closed, &administrator); err != nil {
		t.Fatalf("read migrated administration: %v", err)
	}
	if rows != 1 || !closed || administrator != nil {
		t.Fatalf("migrated administration = rows %d closed %v administrator %v, want 1/true/nil", rows, closed, administrator)
	}
}

// TestServerAdministrationMigrationSerializesLegacyInsert proves migration 39
// cannot decide that the database is empty while an old server's user insert is
// still in flight. The migration must wait for the writer, then observe the
// committed user and close the election without assigning an administrator.
func TestServerAdministrationMigrationSerializesLegacyInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const administrationMigration = "20260913000039_server_administration.sql"

	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, name)

	dsn := pgtest.DSNFrom(name)
	legacyConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("legacy connect: %v", err)
	}
	defer func() { _ = legacyConn.Close(ctx) }() //nolint:errcheck // best-effort close
	migrationConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("migration connect: %v", err)
	}
	defer func() { _ = migrationConn.Close(ctx) }() //nolint:errcheck // best-effort close
	observerConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("observer connect: %v", err)
	}
	defer func() { _ = observerConn.Close(ctx) }() //nolint:errcheck // best-effort close

	for _, entry := range migs {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= administrationMigration {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := legacyConn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", administrationMigration))
	if err != nil {
		t.Fatalf("read administration migration: %v", err)
	}
	const (
		lockStatement     = "LOCK TABLE users IN SHARE MODE;"
		triggerDefinition = "CREATE FUNCTION ensure_server_administration_closed()"
	)
	triggerOffset := strings.Index(string(body), triggerDefinition)
	if triggerOffset < 0 {
		t.Fatalf("administration migration must install the user commit invariant")
	}
	lockOffset := strings.Index(string(body), lockStatement)
	if lockOffset < triggerOffset {
		t.Fatalf("administration migration must serialize users before initialization")
	}
	if _, err := migrationConn.Exec(ctx, string(body[:triggerOffset])); err != nil {
		t.Fatalf("apply administration table creation: %v", err)
	}

	legacyTx, err := legacyConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin legacy insert: %v", err)
	}
	defer func() { _ = legacyTx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op
	if _, err := legacyTx.Exec(ctx, `INSERT INTO users (phone) VALUES ($1)`, "1555000"+pgtest.RandomHex()); err != nil {
		t.Fatalf("legacy insert: %v", err)
	}

	var migrationPID int32
	if err := migrationConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&migrationPID); err != nil {
		t.Fatalf("read migration backend pid: %v", err)
	}
	migrationDone := make(chan error, 1)
	go func() {
		_, err := migrationConn.Exec(ctx, string(body[triggerOffset:]))
		migrationDone <- err
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := observerConn.QueryRow(waitCtx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE pid = $1
				  AND relation = 'public.users'::regclass
				  AND NOT granted
			)
		`, migrationPID).Scan(&waiting); err != nil {
			t.Fatalf("observe migration lock: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-migrationDone:
			t.Fatalf("migration completed before waiting for legacy insert: %v", err)
		case <-waitCtx.Done():
			t.Fatalf("migration did not wait for legacy insert: %v", waitCtx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	if err := legacyTx.Commit(ctx); err != nil {
		t.Fatalf("commit legacy insert: %v", err)
	}
	if err := <-migrationDone; err != nil {
		t.Fatalf("apply administration migration: %v", err)
	}

	var closed bool
	var administrator *int64
	if err := observerConn.QueryRow(ctx, `
		SELECT election_closed, administrator_user_id
		FROM server_administration
		WHERE singleton_id = 1
	`).Scan(&closed, &administrator); err != nil {
		t.Fatalf("read migrated administration: %v", err)
	}
	if !closed || administrator != nil {
		t.Fatalf("migrated administration = closed %v administrator %v, want true/nil", closed, administrator)
	}
}

// TestServerAdministrationMigrationRejectsQueuedLegacyInsert proves a legacy
// insert queued behind migration cannot commit after migration initializes an
// open election. The database must reject that insert at commit rather than
// leave a nonempty database with an open election.
func TestServerAdministrationMigrationRejectsQueuedLegacyInsert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const administrationMigration = "20260913000039_server_administration.sql"

	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, name)

	dsn := pgtest.DSNFrom(name)
	legacyConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("legacy connect: %v", err)
	}
	defer func() { _ = legacyConn.Close(ctx) }() //nolint:errcheck // best-effort close
	migrationConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("migration connect: %v", err)
	}
	defer func() { _ = migrationConn.Close(ctx) }() //nolint:errcheck // best-effort close
	observerConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("observer connect: %v", err)
	}
	defer func() { _ = observerConn.Close(ctx) }() //nolint:errcheck // best-effort close

	for _, entry := range migs {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= administrationMigration {
			continue
		}
		body, err := os.ReadFile(filepath.Join("..", "..", "migrations", entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if _, err := legacyConn.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply migration %s: %v", entry.Name(), err)
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "..", "migrations", administrationMigration))
	if err != nil {
		t.Fatalf("read administration migration: %v", err)
	}
	const lockStatement = "LOCK TABLE users IN SHARE MODE;"
	lockOffset := strings.Index(string(body), lockStatement)
	if lockOffset < 0 {
		t.Fatalf("administration migration must serialize users before initialization")
	}
	if _, err := migrationConn.Exec(ctx, string(body[:lockOffset])); err != nil {
		t.Fatalf("apply administration table creation: %v", err)
	}
	initialization := string(body[lockOffset+len(lockStatement):])

	migrationTx, err := migrationConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin migration: %v", err)
	}
	defer func() { _ = migrationTx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op
	if _, err := migrationTx.Exec(ctx, lockStatement); err != nil {
		t.Fatalf("acquire migration lock: %v", err)
	}

	var legacyPID int32
	if err := legacyConn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&legacyPID); err != nil {
		t.Fatalf("read legacy backend pid: %v", err)
	}
	legacyTx, err := legacyConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin legacy insert: %v", err)
	}
	defer func() { _ = legacyTx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op
	legacyInsertDone := make(chan error, 1)
	go func() {
		_, err := legacyTx.Exec(ctx, `INSERT INTO users (phone) VALUES ($1)`, "1555000"+pgtest.RandomHex())
		legacyInsertDone <- err
	}()

	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var waiting bool
		if err := observerConn.QueryRow(waitCtx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE pid = $1
				  AND relation = 'public.users'::regclass
				  AND NOT granted
			)
		`, legacyPID).Scan(&waiting); err != nil {
			t.Fatalf("observe legacy lock: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-legacyInsertDone:
			t.Fatalf("legacy insert completed before migration committed: %v", err)
		case <-waitCtx.Done():
			t.Fatalf("legacy insert did not wait for migration lock: %v", waitCtx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}

	if _, err := migrationTx.Exec(ctx, initialization); err != nil {
		t.Fatalf("initialize administration election: %v", err)
	}
	if err := migrationTx.Commit(ctx); err != nil {
		t.Fatalf("commit administration migration: %v", err)
	}
	if err := <-legacyInsertDone; err != nil {
		t.Fatalf("queued legacy insert: %v", err)
	}
	if err := legacyTx.Commit(ctx); err == nil {
		t.Fatal("queued legacy insert committed while administration election was open")
	} else if !strings.Contains(err.Error(), "server administrator election is open") {
		t.Fatalf("queued legacy insert failed for the wrong reason: %v", err)
	}

	var users int
	var closed bool
	var administrator *int64
	if err := observerConn.QueryRow(ctx, `
		SELECT count(*) FROM users
	`).Scan(&users); err != nil {
		t.Fatalf("count users after rejected insert: %v", err)
	}
	if err := observerConn.QueryRow(ctx, `
		SELECT election_closed, administrator_user_id
		FROM server_administration
		WHERE singleton_id = 1
	`).Scan(&closed, &administrator); err != nil {
		t.Fatalf("read migrated administration: %v", err)
	}
	if users != 0 || closed || administrator != nil {
		t.Fatalf("database after rejected insert = users %d closed %v administrator %v, want 0/false/nil", users, closed, administrator)
	}
}

// TestOpenRejectsPrePartBlobSchema proves the sentinel also catches the state a
// deployment can be in before the part-blob migration is applied: the template
// carries the current schema, so undo that migration's artifacts by hand
// (re-add payload, drop size and blob_key) and Open must fail at startup rather
// than later, on the first upload query that names a missing column.
func TestOpenRejectsPrePartBlobSchema(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `ALTER TABLE upload_parts DROP COLUMN size, DROP COLUMN blob_key, ADD COLUMN payload BYTEA NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("revert part-blob migration: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsPartBlobPayloadNotDropped proves the sentinel's payload clause
// fires on its own: with the migration's size and blob_key columns already in
// place, the one thing left is dropping payload. Without that clause the
// sentinel would see a migrated schema where the part bytes are still in
// Postgres.
func TestOpenRejectsPartBlobPayloadNotDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `ALTER TABLE upload_parts ADD COLUMN payload BYTEA NOT NULL DEFAULT ''`); err != nil {
		t.Fatalf("re-add payload: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsMissingMessageFileIdx proves the sentinel covers an index and
// not just tables and columns. A missing index shows up in no column check, so
// without this clause a database that stopped at the part-blob migration opens
// clean and then plans the per-row Seq Scan the index exists to remove.
func TestOpenRejectsMissingMessageFileIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX messages_file_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsInvalidMessageFileIdx proves the sentinel rejects an index
// that exists by name but is invalid and therefore invisible to the planner.
func TestOpenRejectsInvalidMessageFileIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX messages_file_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX messages_file_idx ON messages (file_id) WHERE file_id <> 0`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.messages_file_idx'::regclass`); err != nil {
		t.Fatalf("mark index invalid: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

func TestOpenRejectsInvalidDialogsOwnerPageIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX dialogs_owner_page_idx`); err != nil {
		t.Fatalf("drop dialogs owner page index: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX dialogs_owner_page_idx ON dialogs (owner_id, top_message DESC, peer_type, peer_id)`); err != nil {
		t.Fatalf("recreate dialogs owner page index: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.dialogs_owner_page_idx'::regclass`); err != nil {
		t.Fatalf("mark dialogs owner page index invalid: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsInvalidPartBlobKeyIdx covers the other index sentinel with
// the same planner-invisible failure mode.
func TestOpenRejectsInvalidPartBlobKeyIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX upload_parts_blob_key_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE INDEX upload_parts_blob_key_idx ON upload_parts (blob_key)`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.upload_parts_blob_key_idx'::regclass`); err != nil {
		t.Fatalf("mark index invalid: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsInvalidRegistrationInviteLiveIdx covers the live-invite
// uniqueness index, which has the same invalid-index failure mode.
func TestOpenRejectsInvalidRegistrationInviteLiveIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX registration_invites_live_handle_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX registration_invites_live_handle_idx ON registration_invites (handle) WHERE state = 'issued'`); err != nil {
		t.Fatalf("create index: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = 'public.registration_invites_live_handle_idx'::regclass`); err != nil {
		t.Fatalf("mark index invalid: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsMissingPartBlobKeyIdx is the same clause for the index the
// orphan pass's live-key gate reads. A database stopped one migration short
// passes every column check, so without this the server starts and each batch
// of that gate becomes a Seq Scan of upload_parts — the pass keeps working and
// only its cost changes, which is precisely the failure nothing else reports.
func TestOpenRejectsMissingPartBlobKeyIdx(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP INDEX upload_parts_blob_key_idx`); err != nil {
		t.Fatalf("drop index: %v", err)
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// TestOpenRejectsPartBlobMigrationMissing proves the sentinel catches a
// migrations/ directory that stops short of the part-blob migration: a fresh
// database built from every migration file except that one must fail Open, not
// pass it and die on the first upload.
func TestOpenRejectsPartBlobMigrationMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	migs, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	// The sentinel guards against a migrations/ directory that stops short of
	// the part-blob migration: the one that adds the blob_key column. Find it
	// by what it does rather than by position or by name. Position would let an
	// index-only migration added after it shift the "latest", leaving the test
	// withholding a file whose absence is harmless; name would match this
	// branch's own upload_parts_blob_key_idx just as readily as the column
	// migration it is named after.
	//
	// The discriminator is "blob_key" together with "ADD COLUMN", and it takes
	// the last match: exactly one file in migrations/ adds a column by that
	// name today. A future migration that adds another column called blob_key —
	// on any table — moves the selector onto it, and the sentinel this test
	// drives is upload_parts.blob_key specifically. Whoever writes that
	// migration has to narrow this, and the failure will be loud rather than
	// vacuous: withholding the wrong file leaves the startup sentinel's
	// conditions satisfied, so Open succeeds and the assertion below fires.
	var latest string
	for _, e := range migs {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", e.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "blob_key") && strings.Contains(string(b), "ADD COLUMN") {
			latest = e.Name()
		}
	}
	if latest == "" {
		t.Fatal("part-blob migration not found")
	}

	admin, err := pgx.Connect(ctx, pgtest.AdminDSN())
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }() //nolint:errcheck // best-effort close

	name := "t_" + pgtest.RandomHex()
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, name)

	dsn := pgtest.DSNFrom(name)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	defer lockMigrationReplays(t, ctx)()
	for _, e := range migs {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		// Skip the part-blob migration and every migration that comes after
		// it: they may depend on the blob_key column it adds, and one of them
		// does — upload_parts_blob_key_idx indexes that column, so applying it
		// without the column is an error rather than a schema short of the
		// sentinel.
		if e.Name() >= latest {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", e.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", e.Name(), err)
		}
		if _, err := conn.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", e.Name(), err)
		}
	}

	requireOpenMigrationError(t, ctx, dsn)
}

// The public listener's budget tables are not a startup requirement: a
// database restored at an earlier head still opens and serves the tailnet
// budgets, while a public-listener charge fails instead of falling back to
// the tailnet rows.
func TestOpenAcceptsSchemaWithoutPublicListenerBudgets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }() //nolint:errcheck // best-effort close
	if _, err := conn.Exec(ctx, `DROP TABLE public_send_code_ip_calls, public_send_code_ip_phones, public_sign_in_fail_calls`); err != nil {
		t.Fatalf("drop public listener budgets: %v", err)
	}

	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatalf("Open must not require the public listener budgets: %v", err)
	}
	defer func() { _ = s.Close() }() //nolint:errcheck // best-effort close

	addr := netip.MustParseAddr("203.0.113.40")
	cfg := store.SendCodeIPLimits{Calls: store.RateLimitConfig{Limit: 1, Window: time.Hour}}
	if res, err := s.CheckAndChargeSendCodeIPForListener(ctx, addr, "15550000040", cfg, store.RateLimitListenerTailnet); err != nil || res != nil {
		t.Fatalf("tailnet charge = %v, %v; want allowed", res, err)
	}
	if _, err := s.CheckAndChargeSendCodeIPForListener(ctx, addr, "15550000041", cfg, store.RateLimitListenerPublic); err == nil {
		t.Fatal("public charge succeeded without its budget table")
	}
}

// requireOpenMigrationError asserts Open fails with the migration error, not a
// connection error or nil.
func requireOpenMigrationError(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	if _, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t))); err == nil {
		t.Fatal("Open must fail against an un-migrated schema")
	} else if !strings.Contains(err.Error(), "not migrated") {
		t.Fatalf("want a migration error, got %v", err)
	}
}
