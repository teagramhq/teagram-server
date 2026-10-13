package store_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func bindingEvidenceAsync(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := conn.Exec(ctx, query, args...)
		done <- err
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return done
}

func bindingEvidenceWaitBlocked(t *testing.T, ctx context.Context, observer *pgx.Conn, waiter, blocker uint32) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := observer.QueryRow(ctx, `SELECT $1::integer = ANY (pg_catalog.pg_blocking_pids($2))`, blocker, waiter).Scan(&blocked); err != nil {
			t.Fatalf("observe evidence lock barrier: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("evidence operation did not reach its expected lock barrier")
		case <-ticker.C:
		}
	}
}

func bindingEvidenceAwait(t *testing.T, ctx context.Context, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("evidence operation did not finish")
		return ctx.Err()
	}
}

func TestBindingEvidenceEnrollmentWaitsForEarlierWriter(t *testing.T) {
	t.Parallel()
	for _, reinitialize := range []bool{false, true} {
		name := "initial enrollment"
		if reinitialize {
			name = "reinitialization"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newBindingEvidenceFixture(t, reinitialize)
			enrollment := bindingEvidenceConn(t, f.ctx, f.dsn)
			bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN`)
			bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, f.user)
			done := bindingEvidenceAsync(t, f.ctx, enrollment, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
			bindingEvidenceWaitBlocked(t, f.ctx, f.probe, enrollment.PgConn().PID(), f.owner.PgConn().PID())
			if !reinitialize {
				bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user))
			}
			bindingEvidenceExec(t, f.ctx, f.owner, `COMMIT`)
			if err := bindingEvidenceAwait(t, f.ctx, done); err != nil {
				t.Fatalf("enroll after writer commits: %v", err)
			}
			cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
			bindingEvidenceTotals(t, cut, 0, 0)
			if cut.witness != nil || !reflect.DeepEqual(cut.ids, []int64{1, 2, 3}) {
				t.Fatal("earlier writer escaped the enrollment baseline")
			}
			bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (4, '\x00', $1)`, f.user)
			bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 1, 0)
		})
	}
}

func TestBindingEvidenceRefusesConcurrentSecondEnrollment(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, false)
	second := bindingEvidenceConn(t, f.ctx, f.dsn)
	bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN`)
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	done := bindingEvidenceAsync(t, f.ctx, second, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.other)
	bindingEvidenceWaitBlocked(t, f.ctx, f.probe, second.PgConn().PID(), f.owner.PgConn().PID())
	bindingEvidenceExec(t, f.ctx, f.owner, `COMMIT`)
	bindingEvidenceSQLState(t, bindingEvidenceAwait(t, f.ctx, done), "23505")
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
	bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.other))
}

func TestBindingEvidenceLocksAllAuthRowsBeforeEvidence(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	writer := bindingEvidenceConn(t, f.ctx, f.dsn)
	checker := bindingEvidenceConn(t, f.ctx, f.dsn)
	bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN; UPDATE public.browser_acceptance_binding_evidence SET additions = additions`)
	done := bindingEvidenceAsync(t, f.ctx, writer, `UPDATE auth_keys SET user_id = NULL WHERE id IN (1, 2)`)
	bindingEvidenceWaitBlocked(t, f.ctx, f.probe, writer.PgConn().PID(), f.owner.PgConn().PID())
	_, err := checker.Exec(f.ctx, `SELECT id FROM auth_keys WHERE id = 2 FOR UPDATE NOWAIT`)
	bindingEvidenceSQLState(t, err, "55P03")
	bindingEvidenceExec(t, f.ctx, f.owner, `COMMIT`)
	if err := bindingEvidenceAwait(t, f.ctx, done); err != nil {
		t.Fatalf("bulk unbind after evidence lock releases: %v", err)
	}
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, cut, 0, 2)
	if len(cut.ids) != 0 {
		t.Fatal("bulk unbind retained a bound key")
	}
}

func TestBindingEvidenceLocksEvidenceBeforeSRPCascade(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	srp := bindingEvidenceConn(t, f.ctx, f.dsn)
	deletion := bindingEvidenceConn(t, f.ctx, f.dsn)
	bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO srp_challenges (srp_id, auth_key_id, user_id, b_secret, b_public, expires_at)
		VALUES (1, 1, $1, '\x00', '\x00', now() + interval '1 minute')`, f.user)
	bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN; UPDATE public.browser_acceptance_binding_evidence SET additions = additions`)
	bindingEvidenceExec(t, f.ctx, srp, `BEGIN; UPDATE srp_challenges SET expires_at = expires_at WHERE srp_id = 1`)
	done := bindingEvidenceAsync(t, f.ctx, deletion, `DELETE FROM auth_keys WHERE id = 1`)
	bindingEvidenceWaitBlocked(t, f.ctx, f.probe, deletion.PgConn().PID(), f.owner.PgConn().PID())
	bindingEvidenceExec(t, f.ctx, f.owner, `COMMIT`)
	bindingEvidenceWaitBlocked(t, f.ctx, f.probe, deletion.PgConn().PID(), srp.PgConn().PID())
	bindingEvidenceExec(t, f.ctx, srp, `COMMIT`)
	if err := bindingEvidenceAwait(t, f.ctx, done); err != nil {
		t.Fatalf("delete after evidence and SRP barriers: %v", err)
	}
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, cut, 0, 1)
	var remaining int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM srp_challenges`).Scan(&remaining); err != nil {
		t.Fatalf("read remaining SRP challenges: %v", err)
	}
	if remaining != 0 || !reflect.DeepEqual(cut.ids, []int64{2}) {
		t.Fatal("delete did not commit evidence and SRP cascade atomically")
	}
}

func TestBindingEvidenceConcurrentCutsAreAtomic(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	writer := bindingEvidenceConn(t, f.ctx, f.dsn)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for range 50 {
			if _, err := writer.Exec(ctx, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, f.user); err != nil {
				done <- err
				return
			}
			if _, err := writer.Exec(ctx, `DELETE FROM auth_keys WHERE id = 3`); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	for range 100 {
		cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
		if !cut.complete || cut.added == nil || cut.removed == nil || *cut.added-*cut.removed != int64(len(cut.ids)-2) {
			t.Fatal("cut combined bound keys and totals from different committed states")
		}
		if *cut.added > 0 && cut.witness == nil {
			t.Fatal("cut separated an addition from its witness")
		}
	}
	if err := bindingEvidenceAwait(t, f.ctx, done); err != nil {
		t.Fatalf("concurrent binding writer: %v", err)
	}
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 50, 50)
}

func TestBindingEvidenceConcurrentAuthFlowsKeepWriterOrder(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	for id := int64(3); id <= 25; id++ {
		if err := f.s.SaveAuthKey(f.ctx, id, make([]byte, 256)); err != nil {
			t.Fatalf("save concurrent flow key: %v", err)
		}
		if id >= 4 && id != 9 && id != 10 {
			if err := f.s.BindAuthKeyUser(f.ctx, id, f.user); err != nil {
				t.Fatalf("bind concurrent flow key: %v", err)
			}
		}
	}
	started, _, err := f.s.StagePendingUser(f.ctx, 3, f.user, time.Minute)
	if err != nil {
		t.Fatalf("stage concurrent promotion: %v", err)
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE auth_keys SET last_seen_at = now() - interval '400 days' WHERE id = 7`)
	bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO srp_challenges (srp_id, auth_key_id, user_id, b_secret, b_public, expires_at)
		SELECT id, id, $1, '\x00', '\x00', now() + interval '1 minute' FROM auth_keys WHERE id BETWEEN 3 AND 8`, f.user)
	codeHash, _, err := f.s.IssueCodeForUsername(f.ctx, "evidence_admission")
	if err != nil {
		t.Fatalf("save concurrent admission code: %v", err)
	}
	jobs := []func() error{
		func() error { return f.s.PromotePendingUser(f.ctx, 3, f.user, started, time.Minute) },
		func() error { _, _, err := f.s.LogOutAuthKey(f.ctx, 4); return err },
		func() error { _, err := f.s.ResetAuthorization(f.ctx, f.user, 5); return err },
		func() error { _, err := f.s.ResetAuthorizations(f.ctx, f.user, 1); return err },
		func() error { _, err := f.s.SweepExpiredAuthorizations(f.ctx, 4); return err },
		func() error {
			return f.s.BindAuthKeyUserWithMetadata(f.ctx, 9, f.user, store.AuthorizationMetadata{DeviceModel: "fixture"})
		},
		func() error {
			_, err := f.s.AdmitUsername(f.ctx, "evidence_admission", codeHash, 10, "Fixture", "", false)
			return err
		},
	}
	start := make(chan struct{})
	results := make(chan error, len(jobs))
	for _, job := range jobs {
		go func() {
			<-start
			results <- job()
		}()
	}
	close(start)
	for range jobs {
		err := <-results
		// Bulk reset can invalidate a promotion that has not acquired its row.
		if err != nil && !errors.Is(err, store.ErrAuthKeyNotFound) {
			t.Errorf("concurrent auth flow failed: %v", err)
		}
	}
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	if !cut.complete || cut.added == nil || cut.removed == nil || *cut.added-*cut.removed != int64(len(cut.ids)-2) {
		t.Fatal("concurrent auth operations lost a committed transition")
	}
	var pending, retainedSRP int64
	if err := f.owner.QueryRow(f.ctx, `SELECT
		(SELECT count(*) FROM auth_keys WHERE pending_user_id = $1),
		(SELECT count(*) FROM srp_challenges c JOIN auth_keys k ON k.id = c.auth_key_id
		 WHERE k.user_id IS NULL AND k.pending_user_id IS NULL)`, f.user).Scan(&pending, &retainedSRP); err != nil {
		t.Fatalf("read concurrent flow result: %v", err)
	}
	if cut.pending == nil || *cut.pending != pending || retainedSRP != 0 {
		t.Fatal("concurrent reset, logout or sweep retained cleared login state")
	}
}

func TestBindingEvidenceMigrationPreservesExistingBindings(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, false)
	// Reapply only this additive expansion in the harness's disposable clone.
	bindingEvidenceExec(t, f.ctx, f.owner, `
		DROP FUNCTION public.browser_acceptance_binding_snapshot_v1(BIGINT);
		DROP FUNCTION public.browser_acceptance_enroll_binding_evidence(BIGINT);
		DROP FUNCTION public.browser_acceptance_record_binding_evidence() CASCADE;
		DROP FUNCTION public.browser_acceptance_reject_auth_keys_truncate() CASCADE;
		DROP TABLE public.browser_acceptance_binding_evidence;
		DROP SEQUENCE IF EXISTS public.browser_acceptance_binding_epoch_seq;
	`)
	bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO auth_keys (id, key_value, user_id, created_at)
		SELECT n + 100, '\x00', $1, $2 FROM generate_series(1, 10000) n`, f.user, bindingEvidenceOldTime)
	roleName := "binding_defaults_" + pgtest.RandomHex()
	bindingEvidenceExec(t, f.ctx, f.owner, `CREATE ROLE `+roleName)
	for _, objectType := range []string{"TABLES", "SEQUENCES", "FUNCTIONS"} {
		bindingEvidenceExec(t, f.ctx, f.owner, `ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON `+objectType+` TO `+roleName)
	}
	t.Cleanup(func() {
		for _, objectType := range []string{"TABLES", "SEQUENCES", "FUNCTIONS"} {
			if _, err := f.owner.Exec(context.Background(), `ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON `+objectType+` FROM `+roleName); err != nil {
				t.Errorf("clear default privilege fixture: %v", err)
			}
		}
		if _, err := f.owner.Exec(context.Background(), `DROP ROLE `+roleName); err != nil {
			t.Errorf("drop default privilege fixture role: %v", err)
		}
	})
	migration, err := os.ReadFile(filepath.Join("..", "..", "migrations", "20261013000083_browser_acceptance_binding_evidence.sql"))
	if err != nil {
		t.Fatalf("read evidence expansion: %v", err)
	}
	start := time.Now()
	bindingEvidenceExec(t, f.ctx, f.owner, string(migration))
	t.Logf("evidence expansion with 10002 existing auth keys took %s", time.Since(start))
	bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user))
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, cut, 0, 0)
	if len(cut.ids) != 10002 || cut.witness != nil {
		t.Fatal("migration changed existing bindings or backfilled history")
	}
	reader := bindingEvidenceConn(t, f.ctx, f.dsn)
	bindingEvidenceExec(t, f.ctx, reader, `SET SESSION AUTHORIZATION `+roleName)
	for _, query := range []string{
		`SELECT user_id FROM public.browser_acceptance_binding_evidence`,
		`UPDATE public.browser_acceptance_binding_evidence SET additions = 0`,
		`SELECT nextval('public.browser_acceptance_binding_epoch_seq')`,
		`SELECT complete FROM public.browser_acceptance_binding_snapshot_v1(1)`,
		`SELECT public.browser_acceptance_enroll_binding_evidence(1)`,
		`SELECT public.browser_acceptance_record_binding_evidence()`,
		`SELECT public.browser_acceptance_reject_auth_keys_truncate()`,
	} {
		_, err := reader.Exec(f.ctx, query)
		bindingEvidenceSQLState(t, err, "42501")
	}
}
