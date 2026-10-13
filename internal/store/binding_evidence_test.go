package store_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type bindingEvidenceCut struct {
	complete bool
	version  *int32
	epoch    *int64
	added    *int64
	removed  *int64
	witness  *time.Time
	ids      []int64
	created  []time.Time
	pending  *int64
	dbTime   *time.Time
}

type bindingEvidenceFixture struct {
	ctx   context.Context
	dsn   string
	s     *store.Store
	owner *pgx.Conn
	probe *pgx.Conn
	user  int64
	other int64
}

var bindingEvidenceOldTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func bindingEvidenceConn(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to evidence fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close evidence fixture: %v", err)
		}
	})
	return conn
}

func bindingEvidenceExec(t *testing.T, ctx context.Context, conn *pgx.Conn, query string, args ...any) {
	t.Helper()
	if _, err := conn.Exec(ctx, query, args...); err != nil {
		t.Fatalf("execute evidence fixture statement: %v", err)
	}
}

func newBindingEvidenceFixture(t *testing.T, enroll bool) bindingEvidenceFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	dsn := pgtest.DSN(t)
	owner := bindingEvidenceConn(t, ctx, dsn)
	var available bool
	if err := owner.QueryRow(ctx, `SELECT to_regprocedure('public.browser_acceptance_binding_snapshot_v1(bigint)') IS NOT NULL`).Scan(&available); err != nil {
		t.Fatalf("check evidence API: %v", err)
	}
	if !available {
		t.Fatal("versioned binding evidence API is absent")
	}
	s := openStore(t, dsn)
	// Keep the elected administrator separate from the deletable enrolled user.
	other, err := s.CreateUser(ctx, "+15551234002")
	if err != nil {
		t.Fatalf("create unenrolled fixture account: %v", err)
	}
	user, err := s.CreateUser(ctx, "+15551234001")
	if err != nil {
		t.Fatalf("create enrolled fixture account: %v", err)
	}
	probe := bindingEvidenceConn(t, ctx, dsn)
	bindingEvidenceExec(t, ctx, probe, `SET ROLE browser_acceptance_probe`)
	f := bindingEvidenceFixture{ctx: ctx, dsn: dsn, s: s, owner: owner, probe: probe, user: user.ID, other: other.ID}
	bindingEvidenceExec(t, ctx, owner, `INSERT INTO public.auth_keys (id, key_value, user_id, created_at)
		VALUES (1, '\x00', $1, $2), (2, '\x00', $1, $2)`, f.user, bindingEvidenceOldTime)
	if enroll {
		bindingEvidenceExec(t, ctx, owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	}
	return f
}

func bindingEvidenceRead(t *testing.T, ctx context.Context, probe *pgx.Conn, userID int64) bindingEvidenceCut {
	t.Helper()
	var cut bindingEvidenceCut
	err := probe.QueryRow(ctx, `SELECT complete, evidence_version, epoch, additions, removals,
		last_added_created_at, bound_key_ids, bound_key_created_ats, pending_count, db_time
		FROM public.browser_acceptance_binding_snapshot_v1($1)`, userID).Scan(
		&cut.complete, &cut.version, &cut.epoch, &cut.added, &cut.removed,
		&cut.witness, &cut.ids, &cut.created, &cut.pending, &cut.dbTime,
	)
	if err != nil {
		t.Fatalf("read evidence cut: %v", err)
	}
	return cut
}

func bindingEvidenceTotals(t *testing.T, cut bindingEvidenceCut, added, removed int64) {
	t.Helper()
	// Evidence values remain in memory, including on assertion failures.
	if !cut.complete || cut.version == nil || *cut.version != 1 || cut.epoch == nil || *cut.epoch < 1 ||
		cut.added == nil || *cut.added != added || cut.removed == nil || *cut.removed != removed ||
		cut.pending == nil || cut.dbTime == nil || len(cut.ids) != len(cut.created) {
		t.Fatal("cut is incomplete or does not have the expected version, epoch, totals and typed fields")
	}
}

func bindingEvidenceIncomplete(t *testing.T, cut bindingEvidenceCut) {
	t.Helper()
	if cut.complete || cut.version != nil || cut.epoch != nil || cut.added != nil || cut.removed != nil ||
		cut.witness != nil || cut.ids != nil || cut.created != nil || cut.pending != nil || cut.dbTime != nil {
		t.Fatal("incomplete cut disclosed fields other than completeness")
	}
}

func bindingEvidenceSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("statement did not fail with the required SQLSTATE %s", code)
	}
}

func TestBindingEvidenceTransientFixtures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		change  string
		added   int64
		removed int64
		fresh   bool
		bound   []int64
	}{
		{"one fresh key binds and logs out", `INSERT INTO auth_keys (id, key_value, user_id, created_at) VALUES (3, '\x00', $1, $2); UPDATE auth_keys SET user_id = NULL WHERE id = 3`, 11, 9, true, []int64{1, 2}},
		{"two fresh keys bind and log out", `INSERT INTO auth_keys (id, key_value, user_id, created_at) VALUES (3, '\x00', $1, $2), (4, '\x00', $1, $2); DELETE FROM auth_keys WHERE id IN (3, 4)`, 12, 10, true, []int64{1, 2}},
		{"baseline key removed and restored", `DELETE FROM auth_keys WHERE id = 1; INSERT INTO auth_keys (id, key_value, user_id, created_at) VALUES (1, '\x00', $1, '2000-01-01')`, 11, 9, false, []int64{1, 2}},
		{"old unbound key binds and logs out", `INSERT INTO auth_keys (id, key_value, created_at) VALUES (3, '\x00', '2000-01-01'); UPDATE auth_keys SET user_id = $1 WHERE id = 3; UPDATE auth_keys SET user_id = NULL WHERE id = 3`, 11, 9, false, []int64{1, 2}},
		{"baseline key reassigned away and back", `UPDATE auth_keys SET user_id = $2 WHERE id = 1; UPDATE auth_keys SET user_id = $1 WHERE id = 1`, 11, 9, false, []int64{1, 2}},
		{"fresh key remains bound", `INSERT INTO auth_keys (id, key_value, user_id, created_at) VALUES (3, '\x00', $1, $2)`, 11, 8, true, []int64{1, 2, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBindingEvidenceFixture(t, true)
			bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE public.browser_acceptance_binding_evidence SET additions = 10, removals = 8`)
			before := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
			bindingEvidenceTotals(t, before, 10, 8)
			fresh := before.dbTime.Add(time.Microsecond)
			// Simple protocol supports fixture statements with bound parameters.
			args := []any{pgx.QueryExecModeSimpleProtocol, f.user}
			if test.fresh {
				args = append(args, fresh)
			} else if strings.Contains(test.change, "$2") {
				args = append(args, f.other)
			}
			bindingEvidenceExec(t, f.ctx, f.owner, test.change, args...)
			after := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
			bindingEvidenceTotals(t, after, test.added, test.removed)
			if *after.epoch != *before.epoch || !reflect.DeepEqual(after.ids, test.bound) || *after.pending != 0 {
				t.Fatal("fixture changed the epoch, bound set or pending baseline unexpectedly")
			}
			if after.witness == nil || (test.fresh && after.witness.Before(*before.dbTime)) ||
				(!test.fresh && !after.witness.Equal(bindingEvidenceOldTime)) {
				t.Fatal("addition witness does not distinguish fresh keys from baseline disturbances")
			}
			for i, id := range after.ids {
				if id <= 2 && !after.created[i].Equal(bindingEvidenceOldTime) {
					t.Fatal("baseline creation time changed")
				}
			}
		})
	}
}

func TestBindingEvidenceCountsTupleChangesAndIgnoresMetadata(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE auth_keys SET user_id = $1, last_seen_at = now(), device_model = 'fixture', ip = '127.0.0.1' WHERE id = 1`, f.user)
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
	for _, test := range []struct {
		query   string
		added   int64
		removed int64
	}{
		{`UPDATE auth_keys SET id = 11 WHERE id = 1`, 1, 1},
		{`UPDATE auth_keys SET created_at = '2001-01-01' WHERE id = 11`, 2, 2},
		{`UPDATE auth_keys SET user_id = $1 WHERE id = 11`, 2, 3},
		{`DELETE FROM auth_keys WHERE id = 2`, 2, 4},
		{`INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, 2, 4},
	} {
		args := []any{pgx.QueryExecModeSimpleProtocol}
		if strings.Contains(test.query, "$1") {
			args = append(args, f.other)
		}
		bindingEvidenceExec(t, f.ctx, f.owner, test.query, args...)
		bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), test.added, test.removed)
	}
	var evidenceRows int
	if err := f.owner.QueryRow(f.ctx, `SELECT count(*) FROM public.browser_acceptance_binding_evidence`).Scan(&evidenceRows); err != nil {
		t.Fatalf("count enrolled rows: %v", err)
	}
	if evidenceRows != 1 {
		t.Fatal("unenrolled account acquired evidence")
	}
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	if cut.witness == nil || !cut.witness.Equal(time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("removals reconstructed the witness from remaining keys")
	}
}

func TestBindingEvidenceReplicationRoleAndTruncate(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	bindingEvidenceExec(t, f.ctx, f.owner, `SET session_replication_role = replica`)
	bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, f.user)
	bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE auth_keys SET created_at = '2001-01-01' WHERE id = 3`)
	bindingEvidenceExec(t, f.ctx, f.owner, `DELETE FROM auth_keys WHERE id = 3`)
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 2, 2)
	_, err := f.owner.Exec(f.ctx, `TRUNCATE public.auth_keys CASCADE`)
	bindingEvidenceSQLState(t, err, "55000")
	bindingEvidenceExec(t, f.ctx, f.owner, `SET session_replication_role = origin`)
	_, err = f.owner.Exec(f.ctx, `TRUNCATE public.auth_keys CASCADE`)
	bindingEvidenceSQLState(t, err, "55000")
	if cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user); !reflect.DeepEqual(cut.ids, []int64{1, 2}) {
		t.Fatal("refused truncate changed bindings")
	}
}

func TestBindingEvidenceIncompleteCoverage(t *testing.T) {
	t.Parallel()
	for _, change := range []string{
		`ALTER TABLE auth_keys DISABLE TRIGGER "AA_browser_acceptance_binding_evidence"`,
		`ALTER TABLE auth_keys ENABLE TRIGGER "AA_browser_acceptance_binding_evidence"`,
		`ALTER TABLE auth_keys ENABLE REPLICA TRIGGER "AA_browser_acceptance_binding_evidence"`,
		`DROP TRIGGER "AA_browser_acceptance_binding_evidence" ON auth_keys`,
		`ALTER TABLE auth_keys DISABLE TRIGGER browser_acceptance_auth_keys_reject_truncate`,
		`ALTER TABLE auth_keys ENABLE TRIGGER browser_acceptance_auth_keys_reject_truncate`,
		`DROP TRIGGER browser_acceptance_auth_keys_reject_truncate ON auth_keys`,
		`UPDATE public.browser_acceptance_binding_evidence SET evidence_version = 2`,
	} {
		t.Run(change, func(t *testing.T) {
			t.Parallel()
			f := newBindingEvidenceFixture(t, true)
			bindingEvidenceExec(t, f.ctx, f.owner, change)
			bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user))
		})
	}
}

func TestBindingEvidenceEnrollmentLifecycle(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, false)
	for _, userID := range []int64{f.user, f.other, math.MaxInt64} {
		bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, userID))
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	before := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, before, 0, 0)
	if before.witness != nil || !reflect.DeepEqual(before.ids, []int64{1, 2}) {
		t.Fatal("enrollment inferred history instead of establishing a baseline")
	}
	_, err := f.owner.Exec(f.ctx, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.other)
	bindingEvidenceSQLState(t, err, "23505")
	bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.other))
	bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE auth_keys SET user_id = NULL WHERE id = 2`)
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	after := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, after, 0, 0)
	if *after.epoch <= *before.epoch || after.witness != nil {
		t.Fatal("reinitialization reused its epoch or retained history")
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `DELETE FROM auth_keys; DELETE FROM update_state WHERE user_id = $1; DELETE FROM users WHERE id = $1`, pgx.QueryExecModeSimpleProtocol, f.user)
	bindingEvidenceIncomplete(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user))
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.other)
	next := bindingEvidenceRead(t, f.ctx, f.probe, f.other)
	bindingEvidenceTotals(t, next, 0, 0)
	if *next.epoch <= *after.epoch || next.witness != nil || next.ids == nil || len(next.ids) != 0 || next.created == nil {
		t.Fatal("delete and re-enroll reused evidence or failed to represent an empty bound set")
	}
}

func TestBindingEvidenceRollbackAndWriteFailure(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN`)
	bindingEvidenceExec(t, f.ctx, f.owner, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, f.user)
	cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, cut, 0, 0)
	if !reflect.DeepEqual(cut.ids, []int64{1, 2}) || cut.witness != nil {
		t.Fatal("uncommitted evidence or bindings escaped the transaction")
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `ROLLBACK`)
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
	bindingEvidenceExec(t, f.ctx, f.owner, `ALTER TABLE public.browser_acceptance_binding_evidence ADD CONSTRAINT fixture_refuse_addition CHECK (additions = 0)`)
	_, err := f.owner.Exec(f.ctx, `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, f.user)
	bindingEvidenceSQLState(t, err, "23514")
	cut = bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, cut, 0, 0)
	if !reflect.DeepEqual(cut.ids, []int64{1, 2}) || cut.witness != nil {
		t.Fatal("binding committed after an evidence write failed")
	}
	_, err = f.owner.Exec(f.ctx, `UPDATE auth_keys SET id = 11 WHERE id = 1`)
	bindingEvidenceSQLState(t, err, "23514")
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
	bindingEvidenceExec(t, f.ctx, f.owner, `ALTER TABLE public.browser_acceptance_binding_evidence ADD CONSTRAINT fixture_refuse_removal CHECK (removals = 0)`)
	_, err = f.owner.Exec(f.ctx, `DELETE FROM auth_keys WHERE id = 1`)
	bindingEvidenceSQLState(t, err, "23514")
	if cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user); !reflect.DeepEqual(cut.ids, []int64{1, 2}) {
		t.Fatal("deletion committed after an evidence write failed")
	}
}

func TestBindingEvidenceOverflowAbortsBinding(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		column string
		query  string
		added  int64
		remove int64
	}{
		{"addition", "additions", `INSERT INTO auth_keys (id, key_value, user_id) VALUES (3, '\x00', $1)`, math.MaxInt64, 0},
		{"removal", "removals", `UPDATE auth_keys SET user_id = NULL WHERE id = 1`, 0, math.MaxInt64},
		{"tuple replacement", "additions", `UPDATE auth_keys SET id = 11 WHERE id = 1`, math.MaxInt64, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newBindingEvidenceFixture(t, true)
			bindingEvidenceExec(t, f.ctx, f.owner, `UPDATE public.browser_acceptance_binding_evidence SET `+test.column+` = $1`, int64(math.MaxInt64))
			args := []any{pgx.QueryExecModeSimpleProtocol}
			if strings.Contains(test.query, "$1") {
				args = append(args, f.user)
			}
			_, err := f.owner.Exec(f.ctx, test.query, args...)
			bindingEvidenceSQLState(t, err, "22003")
			cut := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
			bindingEvidenceTotals(t, cut, test.added, test.remove)
			if cut.witness != nil || !reflect.DeepEqual(cut.ids, []int64{1, 2}) {
				t.Fatal("overflow changed a binding or wrapped, saturated or reset evidence")
			}
		})
	}
}

func TestBindingEvidencePendingPromotion(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	if err := f.s.SaveAuthKey(f.ctx, 3, make([]byte, 256)); err != nil {
		t.Fatalf("save pending key: %v", err)
	}
	started, _, err := f.s.StagePendingUser(f.ctx, 3, f.user, time.Minute)
	if err != nil {
		t.Fatalf("stage pending key: %v", err)
	}
	before := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, before, 0, 0)
	if *before.pending != 1 || before.witness != nil {
		t.Fatal("staging pending login counted a binding")
	}
	if err := f.s.PromotePendingUser(f.ctx, 3, f.user, started, time.Minute); err != nil {
		t.Fatalf("promote pending key: %v", err)
	}
	after := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, after, 1, 0)
	if *after.pending != 0 || len(after.ids) != 3 || after.witness == nil {
		t.Fatal("promotion did not atomically move pending login into bound evidence")
	}
	if _, _, err := f.s.LogOutAuthKey(f.ctx, 3); err != nil {
		t.Fatalf("log out promoted key: %v", err)
	}
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 1, 1)
}

func TestBindingEvidenceEpochRollbackAndExhaustion(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	before := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceExec(t, f.ctx, f.owner, `BEGIN`)
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	bindingEvidenceExec(t, f.ctx, f.owner, `ROLLBACK`)
	after := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	if !after.complete || *after.epoch != *before.epoch {
		t.Fatal("rolled-back enrollment invalidated committed evidence")
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	next := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	if *next.epoch <= *before.epoch+1 {
		t.Fatal("sequence reused the epoch consumed by a rolled-back enrollment")
	}
	bindingEvidenceExec(t, f.ctx, f.owner, `SELECT setval('public.browser_acceptance_binding_epoch_seq', $1)`, int64(math.MaxInt64))
	_, err := f.owner.Exec(f.ctx, `SELECT public.browser_acceptance_enroll_binding_evidence($1)`, f.user)
	bindingEvidenceSQLState(t, err, "2200H")
	exhausted := bindingEvidenceRead(t, f.ctx, f.probe, f.user)
	bindingEvidenceTotals(t, exhausted, 0, 0)
	if *exhausted.epoch != *next.epoch {
		t.Fatal("sequence exhaustion replaced committed evidence")
	}
}

func TestBindingEvidenceProbeAndPublicPrivileges(t *testing.T) {
	t.Parallel()
	f := newBindingEvidenceFixture(t, true)
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
	refused := []string{
		`SELECT user_id FROM public.browser_acceptance_binding_evidence`,
		`INSERT INTO public.browser_acceptance_binding_evidence (user_id) VALUES (1)`,
		`UPDATE public.browser_acceptance_binding_evidence SET additions = 0`,
		`DELETE FROM public.browser_acceptance_binding_evidence`,
		`TRUNCATE public.browser_acceptance_binding_evidence`,
		`SELECT last_value FROM public.browser_acceptance_binding_epoch_seq`,
		`SELECT nextval('public.browser_acceptance_binding_epoch_seq')`,
		`SELECT setval('public.browser_acceptance_binding_epoch_seq', 1)`,
		`SELECT public.browser_acceptance_enroll_binding_evidence(1)`,
		`SELECT public.browser_acceptance_record_binding_evidence()`,
		`SELECT public.browser_acceptance_reject_auth_keys_truncate()`,
		`UPDATE public.auth_keys SET user_id = NULL`,
	}
	for _, query := range refused {
		_, err := f.probe.Exec(f.ctx, query)
		bindingEvidenceSQLState(t, err, "42501")
	}
	roleName := "binding_public_" + pgtest.RandomHex()
	bindingEvidenceExec(t, f.ctx, f.owner, `CREATE ROLE `+roleName)
	t.Cleanup(func() {
		if _, err := f.owner.Exec(context.Background(), `DROP ROLE `+roleName); err != nil {
			t.Errorf("drop PUBLIC fixture role: %v", err)
		}
	})
	public := bindingEvidenceConn(t, f.ctx, f.dsn)
	bindingEvidenceExec(t, f.ctx, public, `SET SESSION AUTHORIZATION `+roleName)
	for _, query := range append(refused, `SELECT complete FROM public.browser_acceptance_binding_snapshot_v1(1)`) {
		_, err := public.Exec(f.ctx, query)
		bindingEvidenceSQLState(t, err, "42501")
	}
	var safe bool
	if err := f.owner.QueryRow(f.ctx, `SELECT p.provolatile = 's' AND p.prosecdef
		AND p.proconfig @> ARRAY['search_path=pg_catalog'] AND l.lanname = 'sql'
		FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang
		WHERE p.oid = 'public.browser_acceptance_binding_snapshot_v1(bigint)'::regprocedure`).Scan(&safe); err != nil {
		t.Fatalf("check read API hardening: %v", err)
	}
	if !safe {
		t.Fatal("read API is not SQL STABLE SECURITY DEFINER with a pinned search path")
	}
	// Shadow names do not affect the fully qualified definer query.
	bindingEvidenceExec(t, f.ctx, f.owner, `CREATE SCHEMA fixture_shadow; CREATE TABLE fixture_shadow.auth_keys (user_id BIGINT); SET search_path = fixture_shadow, public`)
	bindingEvidenceExec(t, f.ctx, f.probe, `SET search_path = fixture_shadow, public`)
	bindingEvidenceTotals(t, bindingEvidenceRead(t, f.ctx, f.probe, f.user), 0, 0)
}
