package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type authKeyResetter interface {
	ResetAuthorizations(context.Context, int64, int64) ([]int64, error)
}

func requireAuthKeyResetter(t *testing.T, s *store.Store) authKeyResetter {
	t.Helper()
	resetter, ok := any(s).(authKeyResetter)
	if !ok {
		t.Fatal("Store does not expose ResetAuthorizations")
	}
	return resetter
}

func resetAuthorizations(t *testing.T, s *store.Store, ownerID, callerKeyID int64) ([]int64, error) {
	t.Helper()
	return requireAuthKeyResetter(t, s).ResetAuthorizations(context.Background(), ownerID, callerKeyID)
}

func saveBoundAuthKey(t *testing.T, ctx context.Context, s *store.Store, keyID, ownerID int64) {
	t.Helper()
	if err := s.SaveAuthKey(ctx, keyID, []byte("key")); err != nil {
		t.Fatalf("save auth key %d: %v", keyID, err)
	}
	if err := s.BindAuthKeyUser(ctx, keyID, ownerID); err != nil {
		t.Fatalf("bind auth key %d: %v", keyID, err)
	}
}

func requireAuthKey(t *testing.T, ctx context.Context, s *store.Store, keyID int64, wantFound bool, wantOwner, wantPending int64) {
	t.Helper()
	key, found, err := s.AuthKeyByID(ctx, keyID)
	if err != nil {
		t.Fatalf("read auth key %d: %v", keyID, err)
	}
	if found != wantFound {
		t.Fatalf("auth key %d found=%v, want %v", keyID, found, wantFound)
	}
	if found && (key.UserID != wantOwner || key.PendingUserID != wantPending) {
		t.Fatalf("auth key %d has owner=%d pending=%d, want owner=%d pending=%d", keyID, key.UserID, key.PendingUserID, wantOwner, wantPending)
	}
}

func TestResetAuthorizationsOwnerScopeAndRetry(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260101")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	foreign, err := s.CreateUser(ctx, "+15551260102")
	if err != nil {
		t.Fatalf("create foreign owner: %v", err)
	}

	const (
		callerKey         = int64(0x6101)
		boundKeyA         = int64(0x6102)
		boundKeyB         = int64(0x6103)
		pendingKey        = int64(0x6104)
		foreignKey        = int64(0x6105)
		foreignPendingKey = int64(0x6106)
		unboundKey        = int64(0x6107)
	)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, boundKeyA, owner.ID)
	saveBoundAuthKey(t, ctx, s, boundKeyB, owner.ID)
	if err := s.SaveAuthKey(ctx, pendingKey, []byte("pending")); err != nil {
		t.Fatalf("save pending key: %v", err)
	}
	if err := s.SetPendingUser(ctx, pendingKey, owner.ID); err != nil {
		t.Fatalf("set pending owner: %v", err)
	}
	saveBoundAuthKey(t, ctx, s, foreignKey, foreign.ID)
	if err := s.SaveAuthKey(ctx, foreignPendingKey, []byte("foreign pending")); err != nil {
		t.Fatalf("save foreign pending key: %v", err)
	}
	if err := s.SetPendingUser(ctx, foreignPendingKey, foreign.ID); err != nil {
		t.Fatalf("set foreign pending owner: %v", err)
	}
	if err := s.SaveAuthKey(ctx, unboundKey, []byte("unbound")); err != nil {
		t.Fatalf("save unbound key: %v", err)
	}

	removed, err := resetAuthorizations(t, s, owner.ID, callerKey)
	if err != nil {
		t.Fatalf("reset authorizations: %v", err)
	}
	slices.Sort(removed)
	if want := []int64{boundKeyA, boundKeyB}; !slices.Equal(removed, want) {
		t.Fatalf("removed bound key ids = %v, want %v", removed, want)
	}
	requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, boundKeyA, false, 0, 0)
	requireAuthKey(t, ctx, s, boundKeyB, false, 0, 0)
	requireAuthKey(t, ctx, s, pendingKey, false, 0, 0)
	requireAuthKey(t, ctx, s, foreignKey, true, foreign.ID, 0)
	requireAuthKey(t, ctx, s, foreignPendingKey, true, 0, foreign.ID)
	requireAuthKey(t, ctx, s, unboundKey, true, 0, 0)

	removed, err = resetAuthorizations(t, s, owner.ID, callerKey)
	if err != nil {
		t.Fatalf("retry reset authorizations: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("retry removed %v, want no remaining targets", removed)
	}
	requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, foreignKey, true, foreign.ID, 0)
	requireAuthKey(t, ctx, s, foreignPendingKey, true, 0, foreign.ID)
	requireAuthKey(t, ctx, s, unboundKey, true, 0, 0)
}

func TestResetAuthorizationsRejectsInvalidCallerWithoutDeletingTargets(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260111")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551260112")
	if err != nil {
		t.Fatalf("create other owner: %v", err)
	}
	const callerKey = int64(0x6111)
	const targetKey = int64(0x6112)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, targetKey, owner.ID)

	cases := []struct {
		name    string
		ownerID int64
		keyID   int64
	}{
		{name: "anonymous owner", ownerID: 0, keyID: callerKey},
		{name: "rebound scope mismatch", ownerID: other.ID, keyID: callerKey},
		{name: "missing caller", ownerID: owner.ID, keyID: 0xdead},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			removed, err := resetAuthorizations(t, s, tc.ownerID, tc.keyID)
			if !errors.Is(err, store.ErrAuthKeyUnauthorized) {
				t.Fatalf("reset with invalid caller returned ids %v and no authorization error", removed)
			}
			if len(removed) != 0 {
				t.Fatalf("reset with invalid caller exposed removed ids %v", removed)
			}
			requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
			requireAuthKey(t, ctx, s, targetKey, true, owner.ID, 0)
		})
	}
}

func TestResetAuthorizationsConcurrentCallersSerialize(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	type result struct {
		callerID int64
		removed  []int64
		err      error
	}
	resetter := requireAuthKeyResetter(t, s)
	for i := range 20 {
		owner, err := s.CreateUser(ctx, fmt.Sprintf("+155512602%02d", i))
		if err != nil {
			t.Fatalf("create owner %d: %v", i, err)
		}
		callerA := int64(0x7000) + int64(i)*2
		callerB := callerA + 1
		saveBoundAuthKey(t, ctx, s, callerA, owner.ID)
		saveBoundAuthKey(t, ctx, s, callerB, owner.ID)

		start := make(chan struct{})
		results := make(chan result, 2)
		for _, callerID := range []int64{callerA, callerB} {
			go func() {
				<-start
				removed, err := resetter.ResetAuthorizations(ctx, owner.ID, callerID)
				results <- result{callerID: callerID, removed: removed, err: err}
			}()
		}
		close(start)
		first, second := <-results, <-results
		successes := 0
		var winner, loser int64
		for _, got := range []result{first, second} {
			if got.err == nil {
				successes++
				winner = got.callerID
				wantRemoved := callerA
				if winner == callerA {
					wantRemoved = callerB
				}
				if !slices.Equal(got.removed, []int64{wantRemoved}) {
					t.Errorf("iteration %d winner %d removed %v, want [%d]", i, winner, got.removed, wantRemoved)
				}
			} else {
				if !errors.Is(got.err, store.ErrAuthKeyUnauthorized) {
					t.Errorf("iteration %d loser got %v, want ErrAuthKeyUnauthorized", i, got.err)
				}
				loser = got.callerID
			}
		}
		if successes != 1 {
			t.Fatalf("iteration %d had %d successful callers, want exactly one: %+v %+v", i, successes, first, second)
		}
		requireAuthKey(t, ctx, s, winner, true, owner.ID, 0)
		requireAuthKey(t, ctx, s, loser, false, 0, 0)
	}
}

func TestResetAuthorizationsRechecksCallerAfterOwnerLockWait(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"revoked", "rebound"} {
		t.Run(change, func(t *testing.T) {
			dsn := pgtest.DSN(t)
			s := openStore(t, dsn)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			owner, err := s.CreateUser(ctx, "+15551260301")
			if err != nil {
				t.Fatalf("create owner: %v", err)
			}
			other, err := s.CreateUser(ctx, "+15551260302")
			if err != nil {
				t.Fatalf("create other owner: %v", err)
			}
			const callerKey = int64(0x7301)
			const targetKey = int64(0x7302)
			saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
			saveBoundAuthKey(t, ctx, s, targetKey, owner.ID)

			blocker, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect advisory lock holder: %v", err)
			}
			defer func() {
				if err := blocker.Close(context.Background()); err != nil {
					t.Errorf("close advisory lock holder: %v", err)
				}
			}()
			blockerTx, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatalf("begin advisory lock holder: %v", err)
			}
			defer func() { _ = blockerTx.Rollback(context.Background()) }() //nolint:errcheck // release lock on failure
			if _, err := blockerTx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, owner.ID); err != nil {
				t.Fatalf("hold owner lock: %v", err)
			}

			type resetResult struct {
				removed []int64
				err     error
			}
			done := make(chan resetResult, 1)
			resetter := requireAuthKeyResetter(t, s)
			go func() {
				removed, err := resetter.ResetAuthorizations(ctx, owner.ID, callerKey)
				done <- resetResult{removed: removed, err: err}
			}()
			if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
				t.Fatalf("wait for reset to block on owner lock: %v", err)
			}
			if change == "revoked" {
				err = s.DeleteAuthKey(ctx, callerKey)
			} else {
				err = s.BindAuthKeyUser(ctx, callerKey, other.ID)
			}
			if err != nil {
				t.Fatalf("%s caller while reset waits: %v", change, err)
			}
			if err := blockerTx.Commit(ctx); err != nil {
				t.Fatalf("release owner lock: %v", err)
			}
			select {
			case got := <-done:
				if !errors.Is(got.err, store.ErrAuthKeyUnauthorized) || len(got.removed) != 0 {
					t.Fatalf("reset after caller %s returned ids %v, err %v; want authorization error and no result", change, got.removed, got.err)
				}
			case <-ctx.Done():
				t.Fatalf("reset did not finish after releasing owner lock: %v", ctx.Err())
			}
			if change == "revoked" {
				requireAuthKey(t, ctx, s, callerKey, false, 0, 0)
			} else {
				requireAuthKey(t, ctx, s, callerKey, true, other.ID, 0)
			}
			requireAuthKey(t, ctx, s, targetKey, true, owner.ID, 0)
		})
	}
}

func TestResetAuthorizationsKeepsLoginAfterDeleteSnapshot(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	owner, err := s.CreateUser(ctx, "+15551260311")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const callerKey = int64(0x7351)
	const targetKey = int64(0x7352)
	const newLoginKey = int64(0x7353)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, targetKey, owner.ID)
	if err := s.SaveAuthKey(ctx, newLoginKey, []byte("new login")); err != nil {
		t.Fatalf("save new login key: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	store.SetAuthKeyResetBeforeCommitHook(s, func() {
		close(entered)
		<-release
	})
	t.Cleanup(func() {
		store.SetAuthKeyResetBeforeCommitHook(s, nil)
		releaseHook()
	})

	type resetResult struct {
		removed []int64
		err     error
	}
	resetter := requireAuthKeyResetter(t, s)
	done := make(chan resetResult, 1)
	go func() {
		removed, err := resetter.ResetAuthorizations(ctx, owner.ID, callerKey)
		done <- resetResult{removed: removed, err: err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatalf("reset did not reach the post-snapshot point: %v", ctx.Err())
	}
	if err := s.BindAuthKeyUser(ctx, newLoginKey, owner.ID); err != nil {
		t.Fatalf("bind login after reset snapshot: %v", err)
	}
	releaseHook()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("reset authorizations: %v", got.err)
		}
		if !slices.Equal(got.removed, []int64{targetKey}) {
			t.Fatalf("reset removed bound ids %v, want [%d]", got.removed, targetKey)
		}
	case <-ctx.Done():
		t.Fatalf("reset did not commit after login: %v", ctx.Err())
	}
	requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, targetKey, false, 0, 0)
	requireAuthKey(t, ctx, s, newLoginKey, true, owner.ID, 0)
}

func TestResetAuthorizationsAndPendingPromotionRace(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"promotion", "reset"} {
		t.Run(first+" first", func(t *testing.T) {
			dsn := pgtest.DSN(t)
			s := openStore(t, dsn)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			owner, err := s.CreateUser(ctx, "+15551260401")
			if err != nil {
				t.Fatalf("create owner: %v", err)
			}
			const callerKey = int64(0x7401)
			const pendingKey = int64(0x7402)
			saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
			if err := s.SaveAuthKey(ctx, pendingKey, []byte("pending")); err != nil {
				t.Fatalf("save pending key: %v", err)
			}
			if err := s.SetPendingUser(ctx, pendingKey, owner.ID); err != nil {
				t.Fatalf("set pending owner: %v", err)
			}
			pending, ok, err := s.PendingLoginByID(ctx, pendingKey, time.Minute)
			if err != nil || !ok || !pending.Active {
				t.Fatalf("read pending login: pending=%+v ok=%v err=%v", pending, ok, err)
			}

			blocker, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect row lock holder: %v", err)
			}
			defer func() {
				if err := blocker.Close(context.Background()); err != nil {
					t.Errorf("close row lock holder: %v", err)
				}
			}()
			blockerTx, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatalf("begin row lock holder: %v", err)
			}
			defer func() { _ = blockerTx.Rollback(context.Background()) }() //nolint:errcheck // release lock on failure
			var lockedID int64
			if err := blockerTx.QueryRow(ctx, `SELECT id FROM auth_keys WHERE id = $1 FOR UPDATE`, pendingKey).Scan(&lockedID); err != nil {
				t.Fatalf("lock pending key: %v", err)
			}

			type promotionResult struct{ err error }
			type resetResult struct {
				removed []int64
				err     error
			}
			promotionDone := make(chan promotionResult, 1)
			resetDone := make(chan resetResult, 1)
			resetter := requireAuthKeyResetter(t, s)
			startPromotion := func() {
				go func() {
					promotionDone <- promotionResult{err: s.PromotePendingUser(ctx, pendingKey, owner.ID, pending.StartedAt, time.Minute)}
				}()
			}
			startReset := func() {
				go func() {
					removed, err := resetter.ResetAuthorizations(ctx, owner.ID, callerKey)
					resetDone <- resetResult{removed: removed, err: err}
				}()
			}

			if first == "promotion" {
				startPromotion()
				if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
					t.Fatalf("wait for promotion to queue: %v", err)
				}
				startReset()
			} else {
				startReset()
				if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
					t.Fatalf("wait for reset to queue: %v", err)
				}
				startPromotion()
			}
			if err := store.WaitForLockWaiters(ctx, s, 2); err != nil {
				t.Fatalf("wait for both operations to queue: %v", err)
			}
			if err := blockerTx.Commit(ctx); err != nil {
				t.Fatalf("release pending key row lock: %v", err)
			}

			var promoted promotionResult
			select {
			case promoted = <-promotionDone:
			case <-ctx.Done():
				t.Fatalf("promotion did not finish: %v", ctx.Err())
			}
			var reset resetResult
			select {
			case reset = <-resetDone:
			case <-ctx.Done():
				t.Fatalf("reset did not finish: %v", ctx.Err())
			}
			if reset.err != nil {
				t.Fatalf("reset authorizations: %v", reset.err)
			}
			if first == "promotion" {
				if promoted.err != nil {
					t.Fatalf("promotion ordered first: %v", promoted.err)
				}
				if !slices.Equal(reset.removed, []int64{pendingKey}) {
					t.Fatalf("reset after promotion removed bound ids %v, want [%d]", reset.removed, pendingKey)
				}
			} else {
				if !errors.Is(promoted.err, store.ErrAuthKeyNotFound) {
					t.Fatalf("promotion after reset returned %v, want ErrAuthKeyNotFound", promoted.err)
				}
				if len(reset.removed) != 0 {
					t.Fatalf("reset before promotion returned bound ids %v, want none", reset.removed)
				}
			}
			requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
			requireAuthKey(t, ctx, s, pendingKey, false, 0, 0)
		})
	}
}

func TestResetAuthorizationsRollsBackWhenCommitFails(t *testing.T) {
	t.Parallel()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260501")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const callerKey = int64(0x7501)
	const targetKey = int64(0x7502)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, targetKey, owner.ID)

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for deferred trigger: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close deferred trigger connection: %v", err)
		}
	})
	_, err = conn.Exec(ctx, `
CREATE FUNCTION fail_auth_key_reset_at_commit() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'deferred auth key reset failure';
END;
$$;
CREATE CONSTRAINT TRIGGER fail_auth_key_reset_at_commit
AFTER DELETE ON auth_keys DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION fail_auth_key_reset_at_commit();
`)
	if err != nil {
		t.Fatalf("create deferred failure trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DROP TRIGGER IF EXISTS fail_auth_key_reset_at_commit ON auth_keys; DROP FUNCTION IF EXISTS fail_auth_key_reset_at_commit()`); err != nil {
			t.Errorf("drop deferred failure trigger: %v", err)
		}
	})

	removed, err := resetAuthorizations(t, s, owner.ID, callerKey)
	if err == nil {
		t.Fatalf("reset succeeded with deferred commit failure and returned ids %v", removed)
	}
	if removed != nil {
		t.Fatalf("failed reset exposed target result %v, want nil", removed)
	}
	requireAuthKey(t, ctx, s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, ctx, s, targetKey, true, owner.ID, 0)
}

func TestResetAuthorizationsRollsBackWhenCanceledBeforeCommit(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15551260511")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	const callerKey = int64(0x7511)
	const targetKey = int64(0x7512)
	saveBoundAuthKey(t, ctx, s, callerKey, owner.ID)
	saveBoundAuthKey(t, ctx, s, targetKey, owner.ID)

	operationCtx, cancelOperation := context.WithCancel(ctx)
	defer cancelOperation()
	resetter := requireAuthKeyResetter(t, s)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
	store.SetAuthKeyResetBeforeCommitHook(s, func() {
		close(entered)
		<-release
	})
	t.Cleanup(func() {
		store.SetAuthKeyResetBeforeCommitHook(s, nil)
		releaseHook()
	})
	type resetResult struct {
		removed []int64
		err     error
	}
	done := make(chan resetResult, 1)
	go func() {
		removed, err := resetter.ResetAuthorizations(operationCtx, owner.ID, callerKey)
		done <- resetResult{removed: removed, err: err}
	}()
	waitCtx, cancelWait := context.WithTimeout(ctx, 15*time.Second)
	defer cancelWait()
	select {
	case <-entered:
	case <-waitCtx.Done():
		t.Fatalf("reset did not reach the pre-commit point: %v", waitCtx.Err())
	}
	cancelOperation()
	releaseHook()
	select {
	case got := <-done:
		if got.err == nil || got.removed != nil {
			t.Fatalf("canceled reset returned ids %v and err %v, want no result and an error", got.removed, got.err)
		}
	case <-waitCtx.Done():
		t.Fatalf("reset did not stop after context cancellation: %v", waitCtx.Err())
	}
	requireAuthKey(t, context.Background(), s, callerKey, true, owner.ID, 0)
	requireAuthKey(t, context.Background(), s, targetKey, true, owner.ID, 0)
}
