package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRegistrationInviteHistoryLookupIndexes(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		column string
	}{
		{name: "registration_invites_inviter_user_id_idx", column: "inviter_user_id"},
		{name: "registration_invites_consumed_by_user_id_idx", column: "consumed_by_user_id"},
	} {
		var definition string
		if err := store.StorePool(s).QueryRow(ctx,
			`SELECT indexdef
			   FROM pg_indexes
			  WHERE schemaname = current_schema()
			    AND tablename = 'registration_invites'
			    AND indexname = $1`, tc.name,
		).Scan(&definition); err != nil {
			t.Fatalf("read %s: %v", tc.name, err)
		}
		if !strings.Contains(definition, "("+tc.column+")") {
			t.Errorf("index %s definition = %q, want column %s", tc.name, definition, tc.column)
		}
	}
}

func TestIssueInviteStoresOnlyDigestAndCanonicalizesHandle(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	invite, secret, err := s.IssueInvite(ctx, "Alice")
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	secretBytes, err := hex.DecodeString(secret)
	if err != nil {
		t.Fatalf("secret is not hex: %v", err)
	}
	if len(secretBytes) < 16 {
		t.Fatalf("secret entropy = %d bytes, want at least 16", len(secretBytes))
	}
	if invite.Handle != "alice" {
		t.Errorf("handle = %q, want alice", invite.Handle)
	}
	if invite.State != store.InviteIssued {
		t.Errorf("state = %q, want issued", invite.State)
	}
	if !invite.ExpiresAt.After(invite.IssuedAt) {
		t.Fatalf("expiry %s is not after issue time %s", invite.ExpiresAt, invite.IssuedAt)
	}
	if lifetime := invite.ExpiresAt.Sub(invite.IssuedAt); lifetime < store.DefaultInviteLifetime-2*time.Second || lifetime > store.DefaultInviteLifetime+2*time.Second {
		t.Errorf("lifetime = %s, want about %s", lifetime, store.DefaultInviteLifetime)
	}

	var storedDigest []byte
	var storedHandle, state string
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT secret_digest, handle, state FROM registration_invites WHERE id = $1`, invite.ID,
	).Scan(&storedDigest, &storedHandle, &state); err != nil {
		t.Fatalf("read stored invite: %v", err)
	}
	wantDigest := sha256.Sum256([]byte(secret))
	if !bytes.Equal(storedDigest, wantDigest[:]) {
		t.Errorf("stored digest = %x, want SHA-256 digest", storedDigest)
	}
	if bytes.Equal(storedDigest, []byte(secret)) {
		t.Error("stored invite contains the returned secret instead of a digest")
	}
	if storedHandle != "alice" || state != string(store.InviteIssued) {
		t.Errorf("stored invite = handle %q state %q, want alice/issued", storedHandle, state)
	}

	if err := s.VerifyInvite(ctx, "ALICE", secret); err != nil {
		t.Fatalf("verify issued invite: %v", err)
	}
	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != invite.ID {
		t.Fatalf("listed invites = %+v, want invite %d", listed, invite.ID)
	}
	if rendered := fmt.Sprintf("%+v", listed[0]); strings.Contains(rendered, secret) {
		t.Error("listed invite contains the secret")
	}
}

func TestIssueInviteRejectsLiveDuplicateAndAllowsExpiredReplacement(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	first, _, err := s.IssueInvite(ctx, "Alice", time.Hour)
	if err != nil {
		t.Fatalf("first invite: %v", err)
	}
	if _, _, err := s.IssueInvite(ctx, "alice", time.Hour); !errors.Is(err, store.ErrInviteLive) {
		t.Fatalf("live duplicate: err = %v, want ErrInviteLive", err)
	}

	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE registration_invites
		    SET issued_at = clock_timestamp() - interval '2 seconds',
		        expires_at = clock_timestamp() - interval '1 second'
		  WHERE id = $1`, first.ID,
	); err != nil {
		t.Fatalf("expire invite: %v", err)
	}
	second, _, err := s.IssueInvite(ctx, "ALICE", time.Hour)
	if err != nil {
		t.Fatalf("replacement invite: %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("replacement reused invite id %d", second.ID)
	}

	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed invites = %d, want 2", len(listed))
	}
	for _, got := range listed {
		switch got.ID {
		case first.ID:
			if got.State != store.InviteExpired {
				t.Errorf("old invite state = %q, want expired", got.State)
			}
		case second.ID:
			if got.State != store.InviteIssued {
				t.Errorf("replacement state = %q, want issued", got.State)
			}
		default:
			t.Errorf("unexpected listed invite %d", got.ID)
		}
	}

	if _, _, err := s.IssueInvite(ctx, "bob", -time.Second); !errors.Is(err, store.ErrInviteLifetimeInvalid) {
		t.Fatalf("negative lifetime: err = %v, want ErrInviteLifetimeInvalid", err)
	}
	if _, _, err := s.IssueInvite(ctx, "bob", store.MaxInviteLifetime+time.Second); !errors.Is(err, store.ErrInviteLifetimeInvalid) {
		t.Fatalf("overlong lifetime: err = %v, want ErrInviteLifetimeInvalid", err)
	}
	if _, _, err := s.IssueInvite(ctx, "max", store.MaxInviteLifetime); err != nil {
		t.Fatalf("maximum lifetime: %v", err)
	}
}

func TestConcurrentIssueInviteHasOneLiveRow(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	const attempts = 10
	results := make([]error, attempts)
	ready := make(chan struct{})
	var readyWG sync.WaitGroup
	var wg sync.WaitGroup
	readyWG.Add(attempts)
	wg.Add(attempts)
	for i := range attempts {
		go func(i int) {
			defer wg.Done()
			readyWG.Done()
			<-ready
			_, _, results[i] = s.IssueInvite(ctx, "Alice", time.Hour)
		}(i)
	}
	readyWG.Wait()
	close(ready)
	wg.Wait()

	var issued, liveErrors int
	for _, err := range results {
		switch {
		case err == nil:
			issued++
		case errors.Is(err, store.ErrInviteLive):
			liveErrors++
		default:
			t.Errorf("unexpected issue error: %v", err)
		}
	}
	if issued != 1 || liveErrors != attempts-1 {
		t.Fatalf("issue results: issued=%d live errors=%d, want 1/%d", issued, liveErrors, attempts-1)
	}

	var liveRows int
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT count(*) FROM registration_invites
		  WHERE handle = 'alice' AND state = 'issued' AND expires_at > clock_timestamp()`,
	).Scan(&liveRows); err != nil {
		t.Fatalf("count live invites: %v", err)
	}
	if liveRows != 1 {
		t.Fatalf("live invite rows = %d, want 1", liveRows)
	}
}

func TestVerifyInviteReturnsOneNegativeForEveryUnusableState(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	type caseInvite struct {
		name   string
		handle string
		secret string
		setup  func(store.RegistrationInvite)
	}
	cases := make([]caseInvite, 0, 4)

	wrong, wrongSecret, err := s.IssueInvite(ctx, "wrong")
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, caseInvite{name: "wrong secret", handle: wrong.Handle, secret: wrongSecret + "x"})

	expired, expiredSecret, err := s.IssueInvite(ctx, "expired")
	if err != nil {
		t.Fatal(err)
	}
	cases = append(cases, caseInvite{
		name: "expired", handle: expired.Handle, secret: expiredSecret,
		setup: func(inv store.RegistrationInvite) {
			if _, err := store.StorePool(s).Exec(ctx,
				`UPDATE registration_invites
				    SET issued_at = clock_timestamp() - interval '2 seconds',
				        expires_at = clock_timestamp() - interval '1 second'
				  WHERE id = $1`, inv.ID,
			); err != nil {
				t.Fatalf("expire %s invite: %v", inv.Handle, err)
			}
		},
	})

	revoked, revokedSecret, err := s.IssueInvite(ctx, "revoked")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeInvite(ctx, revoked.ID); err != nil {
		t.Fatalf("revoke invite: %v", err)
	}
	cases = append(cases, caseInvite{name: "revoked", handle: revoked.Handle, secret: revokedSecret})

	consumed, consumedSecret, err := s.IssueInvite(ctx, "consumed")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, tx, consumed.Handle, consumedSecret); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback failed consume: %v", rollbackErr)
		}
		t.Fatalf("consume invite: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	cases = append(cases, caseInvite{name: "consumed", handle: consumed.Handle, secret: consumedSecret})

	for _, tc := range cases {
		if tc.setup != nil {
			tc.setup(map[string]store.RegistrationInvite{
				"expired":  expired,
				"revoked":  revoked,
				"consumed": consumed,
				"wrong":    wrong,
			}[tc.name])
		}
		if got := s.VerifyInvite(ctx, tc.handle, tc.secret); !errors.Is(got, store.ErrInviteInvalid) {
			t.Errorf("%s: err = %v, want ErrInviteInvalid", tc.name, got)
		}
	}

	if got := s.VerifyInvite(ctx, "never-issued", "anything"); !errors.Is(got, store.ErrInviteInvalid) {
		t.Errorf("missing invite: err = %v, want ErrInviteInvalid", got)
	}

	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list terminal invite states: %v", err)
	}
	states := make(map[string]store.InviteState, len(listed))
	for _, invite := range listed {
		states[invite.Handle] = invite.State
	}
	for handle, want := range map[string]store.InviteState{
		"wrong":    store.InviteIssued,
		"expired":  store.InviteExpired,
		"revoked":  store.InviteRevoked,
		"consumed": store.InviteConsumed,
	} {
		if states[handle] != want {
			t.Errorf("listed %s state = %q, want %q", handle, states[handle], want)
		}
	}
}

func TestConsumeInviteRollbackLeavesInviteLive(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	invite, secret, err := s.IssueInvite(ctx, "rollback")
	if err != nil {
		t.Fatal(err)
	}

	tx, err := store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, tx, invite.Handle, secret); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback failed consume: %v", rollbackErr)
		}
		t.Fatalf("consume in caller transaction: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyInvite(ctx, invite.Handle, secret); err != nil {
		t.Fatalf("verify after rollback: %v", err)
	}

	tx, err = store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeInvite(ctx, tx, invite.Handle, secret); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback failed consume: %v", rollbackErr)
		}
		t.Fatalf("consume after rollback: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyInvite(ctx, invite.Handle, secret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify after committed consume: err = %v, want ErrInviteInvalid", err)
	}
}

func TestConcurrentConsumeInviteHasExactlyOneWinner(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invite, secret, err := s.IssueInvite(ctx, "racing")
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 10
	results := make([]error, attempts)
	ready := make(chan struct{})
	var readyWG sync.WaitGroup
	var wg sync.WaitGroup
	readyWG.Add(attempts)
	wg.Add(attempts)
	for i := range attempts {
		go func(i int) {
			defer wg.Done()
			readyWG.Done()
			<-ready
			tx, err := store.StorePool(s).Begin(ctx)
			if err != nil {
				results[i] = err
				return
			}
			results[i] = s.ConsumeInvite(ctx, tx, invite.Handle, secret)
			if results[i] == nil {
				if err := tx.Commit(ctx); err != nil {
					results[i] = err
				}
			} else if err := tx.Rollback(ctx); err != nil {
				results[i] = err
			}
		}(i)
	}
	readyWG.Wait()
	close(ready)
	wg.Wait()

	var successes, invalids int
	for _, err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, store.ErrInviteInvalid):
			invalids++
		default:
			t.Errorf("unexpected consume error: %v", err)
		}
	}
	if successes != 1 || invalids != attempts-1 {
		t.Fatalf("consume results: successes=%d invalids=%d, want 1/%d", successes, invalids, attempts-1)
	}
	if err := s.VerifyInvite(ctx, invite.Handle, secret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify after race: err = %v, want ErrInviteInvalid", err)
	}
}

func TestRevokeAndConsumeInviteRowOrderChoosesOneWinner(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	invite, secret, err := s.IssueInvite(ctx, "ordered")
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(ctx) }() //nolint:errcheck // cleanup after test
	var lockedID int64
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM registration_invites WHERE id = $1 FOR UPDATE`, invite.ID,
	).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	if lockedID != invite.ID {
		t.Fatalf("locked invite id = %d, want %d", lockedID, invite.ID)
	}

	revokeDone := make(chan error, 1)
	go func() { revokeDone <- s.RevokeInvite(ctx, invite.ID) }()
	if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
		t.Fatal(err)
	}

	consumeDone := make(chan error, 1)
	go func() {
		tx, err := store.StorePool(s).Begin(ctx)
		if err != nil {
			consumeDone <- err
			return
		}
		err = s.ConsumeInvite(ctx, tx, invite.Handle, secret)
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
				err = rollbackErr
			}
		}
		consumeDone <- err
	}()
	if err := store.WaitForLockWaiters(ctx, s, 2); err != nil {
		t.Fatal(err)
	}

	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-revokeDone; err != nil {
		t.Fatalf("revoke winner: %v", err)
	}
	if err := <-consumeDone; !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("consume loser: err = %v, want ErrInviteInvalid", err)
	}
	if err := s.VerifyInvite(ctx, invite.Handle, secret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify after revoke race: err = %v, want ErrInviteInvalid", err)
	}
}

func TestVerifyInviteUsesUniformDatabaseOperations(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	expired, expiredSecret, err := s.IssueInvite(ctx, "uniform-expired")
	if err != nil {
		t.Fatal(err)
	}
	wrong, wrongSecret, err := s.IssueInvite(ctx, "uniform-wrong")
	if err != nil {
		t.Fatal(err)
	}
	right, rightSecret, err := s.IssueInvite(ctx, "uniform-right")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE registration_invites
		    SET issued_at = clock_timestamp() - interval '2 seconds',
		        expires_at = clock_timestamp() - interval '1 second'
		  WHERE id = $1`, expired.ID,
	); err != nil {
		t.Fatalf("expire invite: %v", err)
	}

	cases := []struct {
		name   string
		handle string
		secret string
	}{
		{name: "missing", handle: "uniform-missing", secret: "missing"},
		{name: "expired", handle: expired.Handle, secret: expiredSecret},
		{name: "wrong secret", handle: wrong.Handle, secret: wrongSecret + "x"},
		{name: "right secret", handle: right.Handle, secret: rightSecret},
	}
	accesses := make([]int64, 0, len(cases))
	for _, tc := range cases {
		before, err := store.RegistrationInviteTableAccesses(ctx, s)
		if err != nil {
			t.Fatalf("%s: read access baseline: %v", tc.name, err)
		}
		got := s.VerifyInvite(ctx, tc.handle, tc.secret)
		if tc.name == "right secret" {
			if got != nil {
				t.Fatalf("%s: verify: %v", tc.name, got)
			}
		} else if !errors.Is(got, store.ErrInviteInvalid) {
			t.Fatalf("%s: verify error = %v, want ErrInviteInvalid", tc.name, got)
		}
		after, err := store.RegistrationInviteTableAccesses(ctx, s)
		if err != nil {
			t.Fatalf("%s: read access result: %v", tc.name, err)
		}
		accesses = append(accesses, after-before)
	}
	for i, got := range accesses {
		if got != 2 {
			t.Errorf("case %s table accesses = %d, want digest lookup plus handle guard", cases[i].name, got)
		}
	}
}

func TestConsumeInviteUsesUniformDatabaseOperations(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	expired, expiredSecret, err := s.IssueInvite(ctx, "consume-uniform-expired")
	if err != nil {
		t.Fatal(err)
	}
	wrong, wrongSecret, err := s.IssueInvite(ctx, "consume-uniform-wrong")
	if err != nil {
		t.Fatal(err)
	}
	right, rightSecret, err := s.IssueInvite(ctx, "consume-uniform-right")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StorePool(s).Exec(ctx,
		`UPDATE registration_invites
		    SET issued_at = clock_timestamp() - interval '2 seconds',
		        expires_at = clock_timestamp() - interval '1 second'
		  WHERE id = $1`, expired.ID,
	); err != nil {
		t.Fatalf("expire invite: %v", err)
	}

	cases := []struct {
		name   string
		handle string
		secret string
	}{
		{name: "missing", handle: "consume-uniform-missing", secret: "missing"},
		{name: "expired", handle: expired.Handle, secret: expiredSecret},
		{name: "wrong secret", handle: wrong.Handle, secret: wrongSecret + "x"},
		{name: "right secret", handle: right.Handle, secret: rightSecret},
	}
	for _, tc := range cases {
		before, err := store.RegistrationInviteTableAccesses(ctx, s)
		if err != nil {
			t.Fatalf("%s: read access baseline: %v", tc.name, err)
		}
		tx, err := store.StorePool(s).Begin(ctx)
		if err != nil {
			t.Fatalf("%s: begin: %v", tc.name, err)
		}
		got := s.ConsumeInvite(ctx, tx, tc.handle, tc.secret)
		if got == nil {
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("%s: commit: %v", tc.name, err)
			}
		} else if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("%s: rollback: %v", tc.name, err)
		}
		if tc.name == "right secret" {
			if got != nil {
				t.Fatalf("%s: consume error = %v, want success", tc.name, got)
			}
		} else if !errors.Is(got, store.ErrInviteInvalid) {
			t.Fatalf("%s: consume error = %v, want ErrInviteInvalid", tc.name, got)
		}
		after, err := store.RegistrationInviteTableAccesses(ctx, s)
		if err != nil {
			t.Fatalf("%s: read access result: %v", tc.name, err)
		}
		wantAccesses := int64(2)
		if tc.name == "right secret" {
			wantAccesses++ // successful consume performs the guarded state update.
		}
		if got := after - before; got != wantAccesses {
			t.Errorf("case %s table accesses = %d, want %d", tc.name, got, wantAccesses)
		}
	}
}

func TestInviteExpiryIsCheckedAfterRowLockAcquisition(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	invite, secret, err := s.IssueInvite(ctx, "lock-clock-verify", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM registration_invites WHERE id = $1 FOR UPDATE`, invite.ID,
	).Scan(&lockedID); err != nil {
		if rollbackErr := blocker.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback blocker: %v", rollbackErr)
		}
		t.Fatal(err)
	}
	verifyDone := make(chan error, 1)
	go func() { verifyDone <- s.VerifyInvite(ctx, invite.Handle, secret) }()
	if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
		if rollbackErr := blocker.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback blocker: %v", rollbackErr)
		}
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-verifyDone; !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify after lock-held expiry: err = %v, want ErrInviteInvalid", err)
	}

	invite, secret, err = s.IssueInvite(ctx, "lock-clock-consume", 50*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err = store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.QueryRow(ctx,
		`SELECT id FROM registration_invites WHERE id = $1 FOR UPDATE`, invite.ID,
	).Scan(&lockedID); err != nil {
		if rollbackErr := blocker.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback blocker: %v", rollbackErr)
		}
		t.Fatal(err)
	}
	consumeDone := make(chan error, 1)
	go func() {
		tx, err := store.StorePool(s).Begin(ctx)
		if err != nil {
			consumeDone <- err
			return
		}
		err = s.ConsumeInvite(ctx, tx, invite.Handle, secret)
		if rollbackErr := tx.Rollback(ctx); err == nil && rollbackErr != nil {
			err = rollbackErr
		}
		consumeDone <- err
	}()
	if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
		if rollbackErr := blocker.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback blocker: %v", rollbackErr)
		}
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-consumeDone; !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("consume after lock-held expiry: err = %v, want ErrInviteInvalid", err)
	}
}

func TestUserInviteIsResolvedBySecretAndOperatorInviteStillBindsHandle(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	secret := strings.Repeat("a", 64)
	digest := sha256.Sum256([]byte(secret))
	if _, err := store.StorePool(s).Exec(ctx, `
		INSERT INTO registration_invites (handle, secret_digest, inviter_user_id, issued_at, expires_at)
		VALUES (NULL, $1, $2, clock_timestamp(), clock_timestamp() + interval '7 days')
	`, digest[:], inviter.ID); err != nil {
		t.Fatalf("insert user invite: %v", err)
	}

	for _, handle := range []string{"alice", "bob"} {
		if err := s.VerifyInvite(ctx, handle, secret); err != nil {
			t.Errorf("verify user invite for %q: %v", handle, err)
		}
	}

	operator, operatorSecret, err := s.IssueInvite(ctx, "reserved")
	if err != nil {
		t.Fatalf("issue operator invite: %v", err)
	}
	if err := s.VerifyInvite(ctx, operator.Handle+"-other", operatorSecret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Errorf("verify operator invite for a different handle: err = %v, want ErrInviteInvalid", err)
	}
}

func TestIssueUserInviteStoresOnlyDigestAndInviter(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	issuer, ok := any(s).(interface {
		IssueUserInvite(context.Context, int64, ...time.Duration) (store.RegistrationInvite, string, error)
	})
	if !ok {
		t.Fatal("store does not expose user invite issuance")
	}
	invite, secret, err := issuer.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue user invite: %v", err)
	}
	if invite.Handle != "" {
		t.Errorf("handle = %q, want empty for a user invite", invite.Handle)
	}
	if invite.InviterUserID == nil || *invite.InviterUserID != inviter.ID {
		t.Errorf("invite inviter = %v, want %d", invite.InviterUserID, inviter.ID)
	}
	if invite.State != store.InviteIssued {
		t.Errorf("state = %q, want issued", invite.State)
	}
	if lifetime := invite.ExpiresAt.Sub(invite.IssuedAt); lifetime < store.DefaultInviteLifetime-2*time.Second || lifetime > store.DefaultInviteLifetime+2*time.Second {
		t.Errorf("lifetime = %s, want about %s", lifetime, store.DefaultInviteLifetime)
	}

	var handle *string
	var storedInviterID int64
	var storedRedeemerID *int64
	var storedDigest []byte
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT handle, inviter_user_id, consumed_by_user_id, secret_digest
		FROM registration_invites WHERE id = $1
	`, invite.ID).Scan(&handle, &storedInviterID, &storedRedeemerID, &storedDigest); err != nil {
		t.Fatalf("read user invite row: %v", err)
	}
	wantDigest := sha256.Sum256([]byte(secret))
	if handle != nil || storedInviterID != inviter.ID || storedRedeemerID != nil {
		t.Errorf("stored row handle=%v inviter=%d redeemer=%v; want NULL/%d/NULL", handle, storedInviterID, storedRedeemerID, inviter.ID)
	}
	if !bytes.Equal(storedDigest, wantDigest[:]) || bytes.Equal(storedDigest, []byte(secret)) {
		t.Errorf("stored digest = %x, want SHA-256 digest only", storedDigest)
	}
	listed, err := s.ListInvites(ctx)
	if err != nil {
		t.Fatalf("list user invite: %v", err)
	}
	if len(listed) != 1 || listed[0].Handle != "" || listed[0].InviterUserID == nil || *listed[0].InviterUserID != inviter.ID {
		t.Fatalf("listed user invite = %#v, want handle-less invite attributed to %d", listed, inviter.ID)
	}
}

func TestConsumeUserInviteRequiresQuotaAndInviteListenerAndRecordsRedeemer(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	redeemer, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create redeemer: %v", err)
	}
	invite, secret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue user invite: %v", err)
	}

	for _, tc := range []struct {
		name    string
		quota   int
		allowed bool
	}{
		{name: "quota disabled", quota: 0, allowed: true},
		{name: "listener not in invite mode", quota: 5, allowed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := store.StorePool(s).Begin(ctx)
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			err = s.ConsumeInviteForUser(ctx, tx, "alice", secret, redeemer.ID, tc.quota, tc.allowed)
			if !errors.Is(err, store.ErrInviteInvalid) {
				if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
					t.Errorf("rollback refused consume: %v", rollbackErr)
				}
				t.Fatalf("consume with quota=%d allowed=%v: err = %v, want ErrInviteInvalid", tc.quota, tc.allowed, err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatalf("rollback refused consume: %v", err)
			}
		})
	}
	if err := s.VerifyInvite(ctx, "alice", secret); err != nil {
		t.Fatalf("verify after disabled attempts: %v", err)
	}

	reserved, _, err := s.IssueInvite(ctx, "reserved-user-invite")
	if err != nil {
		t.Fatalf("issue operator handle invite: %v", err)
	}
	reservedUserInvite, reservedSecret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue reserved user invite: %v", err)
	}
	tx, err := store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatalf("begin reserved consume: %v", err)
	}
	if err := s.ConsumeInviteForUser(ctx, tx, reserved.Handle, reservedSecret, redeemer.ID, 5, true); !errors.Is(err, store.ErrInviteInvalid) {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback refused reserved consume: %v", rollbackErr)
		}
		t.Fatalf("consume user invite for operator-reserved handle: err = %v, want ErrInviteInvalid", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback reserved consume: %v", err)
	}
	if err := s.VerifyInvite(ctx, reserved.Handle, reservedSecret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify user invite for operator-reserved handle: err = %v, want ErrInviteInvalid", err)
	}
	if err := s.VerifyInvite(ctx, "alice", secret); err != nil {
		t.Fatalf("verify primary invite before success: %v", err)
	}

	tx, err = store.StorePool(s).Begin(ctx)
	if err != nil {
		t.Fatalf("begin successful consume: %v", err)
	}
	if err := s.ConsumeInviteForUser(ctx, tx, "alice", secret, redeemer.ID, 5, true); err != nil {
		if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			t.Errorf("rollback failed user consume: %v", rollbackErr)
		}
		t.Fatalf("consume enabled user invite: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit enabled user invite: %v", err)
	}
	var storedRedeemerID int64
	var state string
	if err := store.StorePool(s).QueryRow(ctx, `SELECT consumed_by_user_id, state FROM registration_invites WHERE id = $1`, invite.ID).Scan(&storedRedeemerID, &state); err != nil {
		t.Fatalf("read consumed user invite: %v", err)
	}
	if storedRedeemerID != redeemer.ID || state != string(store.InviteConsumed) {
		t.Fatalf("consumed invite redeemer=%d state=%q, want %d/consumed", storedRedeemerID, state, redeemer.ID)
	}
	if err := s.VerifyInvite(ctx, "alice", secret); !errors.Is(err, store.ErrInviteInvalid) {
		t.Fatalf("verify after consume: err = %v, want ErrInviteInvalid", err)
	}
	if listed, err := s.ListInvites(ctx); err != nil {
		t.Fatalf("list after user consumes: %v", err)
	} else if len(listed) != 3 || listed[2].ID != reservedUserInvite.ID || listed[2].InviterUserID == nil {
		t.Errorf("listed invites after consume = %#v, wanted all three rows with inviter on user rows", listed)
	}
}

type quotaUserInviteCreator interface {
	CreateUserInvite(context.Context, int64, int, time.Duration) (store.RegistrationInvite, string, error)
}

func TestConcurrentCreateUserInviteEnforcesWeeklyQuotaAcrossStates(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	creator, ok := any(s).(quotaUserInviteCreator)
	if !ok {
		t.Fatal("store does not expose quota-checked user invite creation")
	}

	const quota = 5
	const attempts = quota + 1
	results := make([]error, attempts)
	ready := make(chan struct{})
	var readyWG, workers sync.WaitGroup
	readyWG.Add(attempts)
	workers.Add(attempts)
	for i := range attempts {
		go func(i int) {
			defer workers.Done()
			readyWG.Done()
			<-ready
			_, _, results[i] = creator.CreateUserInvite(ctx, inviter.ID, quota, 0)
		}(i)
	}
	readyWG.Wait()
	close(ready)
	workers.Wait()

	succeeded := 0
	refused := 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case err.Error() == "user invite weekly limit reached":
			refused++
		default:
			t.Errorf("create attempt %d: unexpected error %v", i, err)
		}
	}
	if succeeded != quota || refused != 1 {
		t.Fatalf("parallel creates succeeded=%d refused=%d, want %d and 1", succeeded, refused, quota)
	}

	var issued int
	if err := store.StorePool(s).QueryRow(ctx, `
		SELECT count(*) FROM registration_invites WHERE inviter_user_id = $1
	`, inviter.ID).Scan(&issued); err != nil {
		t.Fatalf("count created invites: %v", err)
	}
	if issued != quota {
		t.Fatalf("created invites = %d, want quota %d", issued, quota)
	}

	if _, err := store.StorePool(s).Exec(ctx, `
		WITH ordered AS (
			SELECT id, row_number() OVER (ORDER BY id) AS n
			FROM registration_invites WHERE inviter_user_id = $1
		)
		UPDATE registration_invites AS invite
		SET state = CASE ordered.n
				WHEN 1 THEN 'consumed'
				WHEN 2 THEN 'revoked'
				WHEN 3 THEN 'expired'
				ELSE 'issued'
			END,
			consumed_at = CASE WHEN ordered.n = 1 THEN clock_timestamp() END,
			revoked_at = CASE WHEN ordered.n = 2 THEN clock_timestamp() END
		FROM ordered
		WHERE invite.id = ordered.id
	`, inviter.ID); err != nil {
		t.Fatalf("set invite terminal states: %v", err)
	}

	if _, _, err := creator.CreateUserInvite(ctx, inviter.ID, quota, 0); err == nil || err.Error() != "user invite weekly limit reached" {
		t.Fatalf("create after terminal transitions: err = %v, want weekly limit reached", err)
	}
}

func TestCreateUserInviteEnforcesMinimumAccountAgeAndReturnsWait(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	creator, ok := any(s).(quotaUserInviteCreator)
	if !ok {
		t.Fatal("store does not expose quota-checked user invite creation")
	}

	_, _, err = creator.CreateUserInvite(ctx, inviter.ID, 5, 7*24*time.Hour)
	if err == nil {
		t.Fatal("new account created an invite before the minimum age")
	}
	const prefix = "user invite account too new; wait "
	waitText, ok := strings.CutPrefix(err.Error(), prefix)
	if !ok {
		t.Fatalf("error = %q, want account-too-new error with remaining seconds", err)
	}
	waitText, ok = strings.CutSuffix(waitText, " seconds")
	if !ok {
		t.Fatalf("error = %q, want wait duration in seconds", err)
	}
	waitSeconds, err := strconv.ParseInt(waitText, 10, 64)
	if err != nil || waitSeconds <= 0 || waitSeconds > int64((7*24*time.Hour)/time.Second) {
		t.Fatalf("remaining wait = %q (%v), want a positive value no greater than seven days", waitText, err)
	}
}

type userInviteLister interface {
	ListUserInvites(context.Context, int64) ([]store.RegistrationInvite, int64, *time.Time, error)
}

func TestListUserInvitesIsScopedSafeAndReportsWeeklyUsage(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	other, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	redeemer, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create redeemer: %v", err)
	}
	lister, ok := any(s).(userInviteLister)
	if !ok {
		t.Fatal("store does not expose caller-scoped user invite listing")
	}

	old, _, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue old invite: %v", err)
	}
	revoked, revokedSecret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue revoked invite: %v", err)
	}
	used, usedSecret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue used invite: %v", err)
	}
	expired, _, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue expired invite: %v", err)
	}
	naturalExpired, _, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue naturally expired invite: %v", err)
	}
	pending, pendingSecret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue pending invite: %v", err)
	}
	foreign, _, err := s.IssueUserInvite(ctx, other.ID)
	if err != nil {
		t.Fatalf("issue foreign invite: %v", err)
	}

	for _, tc := range []struct {
		id  int64
		sql string
		arg []any
	}{
		{
			id: revoked.ID,
			sql: `UPDATE registration_invites SET issued_at = clock_timestamp() - interval '6 days',
			      expires_at = clock_timestamp() + interval '1 day', state = 'revoked',
			      revoked_at = clock_timestamp() WHERE id = $1`,
		},
		{
			id: used.ID,
			sql: `UPDATE registration_invites SET issued_at = clock_timestamp() - interval '3 days',
			      expires_at = clock_timestamp() + interval '4 days', state = 'consumed',
			      consumed_at = clock_timestamp(), consumed_by_user_id = $2 WHERE id = $1`,
			arg: []any{redeemer.ID},
		},
		{
			id: expired.ID,
			sql: `UPDATE registration_invites SET issued_at = clock_timestamp() - interval '2 days',
			      expires_at = clock_timestamp() - interval '1 day', state = 'expired' WHERE id = $1`,
		},
		{
			id: naturalExpired.ID,
			sql: `UPDATE registration_invites SET issued_at = clock_timestamp() - interval '1 day',
			      expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`,
		},
		{
			id: old.ID,
			sql: `UPDATE registration_invites SET issued_at = clock_timestamp() - interval '8 days',
			      expires_at = clock_timestamp() - interval '1 day', state = 'expired' WHERE id = $1`,
		},
	} {
		args := append([]any{tc.id}, tc.arg...)
		if _, err := store.StorePool(s).Exec(ctx, tc.sql, args...); err != nil {
			t.Fatalf("set invite %d lifecycle state: %v", tc.id, err)
		}
	}

	var oldestCounted time.Time
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT issued_at FROM registration_invites WHERE id = $1`, revoked.ID,
	).Scan(&oldestCounted); err != nil {
		t.Fatalf("read oldest counted invite: %v", err)
	}
	invites, weeklyCount, nextAvailableAt, err := lister.ListUserInvites(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("list user invites: %v", err)
	}
	if len(invites) != 6 {
		t.Fatalf("listed %d invites, want caller's 6 invites", len(invites))
	}
	if weeklyCount != 5 {
		t.Fatalf("weekly count = %d, want 5 including pending, used, expired, and revoked", weeklyCount)
	}
	if nextAvailableAt == nil {
		t.Fatal("next available time is absent while counted invites exist")
	}
	wantNext := oldestCounted.Add(7 * 24 * time.Hour)
	if delta := nextAvailableAt.Sub(wantNext); delta < -2*time.Second || delta > 2*time.Second {
		t.Errorf("next available time = %s, want oldest creation plus seven days (%s)", *nextAvailableAt, wantNext)
	}
	if invites[0].ID != pending.ID || invites[1].ID != naturalExpired.ID || invites[2].ID != expired.ID || invites[3].ID != used.ID || invites[4].ID != revoked.ID || invites[5].ID != old.ID {
		t.Errorf("invite order = [%d %d %d %d %d %d], want newest first", invites[0].ID, invites[1].ID, invites[2].ID, invites[3].ID, invites[4].ID, invites[5].ID)
	}
	states := make(map[int64]store.InviteState, len(invites))
	for _, invite := range invites {
		states[invite.ID] = invite.State
	}
	for id, want := range map[int64]store.InviteState{
		pending.ID:        store.InviteIssued,
		naturalExpired.ID: store.InviteExpired,
		expired.ID:        store.InviteExpired,
		used.ID:           store.InviteConsumed,
		revoked.ID:        store.InviteRevoked,
		old.ID:            store.InviteExpired,
	} {
		if states[id] != want {
			t.Errorf("invite %d state = %q, want %q", id, states[id], want)
		}
	}
	if states[foreign.ID] != "" {
		t.Errorf("listing contains another user's invite %d", foreign.ID)
	}
	if got := fmt.Sprintf("%+v", invites); strings.Contains(got, revokedSecret) || strings.Contains(got, usedSecret) || strings.Contains(got, pendingSecret) {
		t.Errorf("invite listing includes a cleartext secret: %s", got)
	}

	if _, err := store.StorePool(s).Exec(ctx, `
		UPDATE registration_invites
		SET issued_at = clock_timestamp() - interval '8 days',
		    expires_at = clock_timestamp() - interval '1 day'
		WHERE inviter_user_id = $1
	`, inviter.ID); err != nil {
		t.Fatalf("age all inviter invites out of quota window: %v", err)
	}
	_, weeklyCount, nextAvailableAt, err = lister.ListUserInvites(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("list after quota window: %v", err)
	}
	if weeklyCount != 0 || nextAvailableAt != nil {
		t.Errorf("usage after all invites age out = count %d next %v, want 0 and absent", weeklyCount, nextAvailableAt)
	}
}

type userInviteRevoker interface {
	RevokeUserInvite(context.Context, int64, int64) error
}

func TestRevokeUserInviteHidesForeignIDsAndIsIdempotent(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	revoker, ok := any(s).(userInviteRevoker)
	if !ok {
		t.Fatal("store does not expose caller-scoped user invite revocation")
	}
	foreign, _, err := s.IssueUserInvite(ctx, other.ID)
	if err != nil {
		t.Fatalf("issue foreign invite: %v", err)
	}
	if err := revoker.RevokeUserInvite(ctx, owner.ID, foreign.ID); err != nil {
		t.Fatalf("revoke foreign id: %v", err)
	}
	if err := revoker.RevokeUserInvite(ctx, owner.ID, foreign.ID+100000); err != nil {
		t.Fatalf("revoke absent id: %v", err)
	}
	var state string
	if err := store.StorePool(s).QueryRow(ctx, `SELECT state FROM registration_invites WHERE id = $1`, foreign.ID).Scan(&state); err != nil {
		t.Fatalf("read foreign invite state: %v", err)
	}
	if state != string(store.InviteIssued) {
		t.Fatalf("foreign invite state = %q, want issued", state)
	}

	revoked, _, err := s.IssueUserInvite(ctx, owner.ID)
	if err != nil {
		t.Fatalf("issue owned invite: %v", err)
	}
	if err := revoker.RevokeUserInvite(ctx, owner.ID, revoked.ID); err != nil {
		t.Fatalf("revoke owned invite: %v", err)
	}
	if err := revoker.RevokeUserInvite(ctx, owner.ID, revoked.ID); err != nil {
		t.Fatalf("repeat revoke: %v", err)
	}
	if err := store.StorePool(s).QueryRow(ctx, `SELECT state FROM registration_invites WHERE id = $1`, revoked.ID).Scan(&state); err != nil {
		t.Fatalf("read revoked invite state: %v", err)
	}
	if state != string(store.InviteRevoked) {
		t.Fatalf("revoked invite state = %q, want revoked", state)
	}
}

func TestRevokeUserInviteAndRedemptionChooseOneTerminalState(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	inviter, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create inviter: %v", err)
	}
	redeemer, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create redeemer: %v", err)
	}
	invite, secret, err := s.IssueUserInvite(ctx, inviter.ID)
	if err != nil {
		t.Fatalf("issue invite: %v", err)
	}
	revoker, ok := any(s).(userInviteRevoker)
	if !ok {
		t.Fatal("store does not expose caller-scoped user invite revocation")
	}

	start := make(chan struct{})
	revokeDone := make(chan error, 1)
	consumeDone := make(chan error, 1)
	go func() {
		<-start
		revokeDone <- revoker.RevokeUserInvite(ctx, inviter.ID, invite.ID)
	}()
	go func() {
		<-start
		tx, err := store.StorePool(s).Begin(ctx)
		if err != nil {
			consumeDone <- err
			return
		}
		err = s.ConsumeInviteForUser(ctx, tx, "chosen-name", secret, redeemer.ID, 5, true)
		if err == nil {
			err = tx.Commit(ctx)
		} else if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
			err = rollbackErr
		}
		consumeDone <- err
	}()
	close(start)

	revokeErr := <-revokeDone
	consumeErr := <-consumeDone
	if revokeErr != nil {
		t.Fatalf("revoke invite: %v", revokeErr)
	}
	var state string
	var redeemerID *int64
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT state, consumed_by_user_id FROM registration_invites WHERE id = $1`, invite.ID,
	).Scan(&state, &redeemerID); err != nil {
		t.Fatalf("read race result: %v", err)
	}
	switch state {
	case string(store.InviteRevoked):
		if !errors.Is(consumeErr, store.ErrInviteInvalid) || redeemerID != nil {
			t.Fatalf("revoke won but consume error=%v redeemer=%v, want terminal rejection and no redeemer", consumeErr, redeemerID)
		}
	case string(store.InviteConsumed):
		if consumeErr != nil || redeemerID == nil || *redeemerID != redeemer.ID {
			t.Fatalf("consume won but error=%v redeemer=%v, want committed redeemer %d", consumeErr, redeemerID, redeemer.ID)
		}
	default:
		t.Fatalf("race state = %q, want exactly one terminal winner", state)
	}
}

func TestListUserInvitesIsBoundedNewestFirst(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+1555"+pgtest.RandomHex())
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	lister, ok := any(s).(userInviteLister)
	if !ok {
		t.Fatal("store does not expose caller-scoped user invite listing")
	}
	if _, err := store.StorePool(s).Exec(ctx, `
		INSERT INTO registration_invites (handle, inviter_user_id, secret_digest, issued_at, expires_at)
		SELECT NULL, $1, decode(lpad(to_hex(n), 64, '0'), 'hex'),
		       clock_timestamp() - ((102 - n)::bigint * interval '1 second'),
		       clock_timestamp() - ((102 - n)::bigint * interval '1 second') + interval '7 days'
		FROM generate_series(1, 101) AS n
	`, owner.ID); err != nil {
		t.Fatalf("insert list-bound fixtures: %v", err)
	}

	invites, weeklyCount, _, err := lister.ListUserInvites(ctx, owner.ID)
	if err != nil {
		t.Fatalf("list user invites: %v", err)
	}
	if len(invites) != 100 {
		t.Fatalf("listed invites = %d, want bound of 100", len(invites))
	}
	if weeklyCount != 101 {
		t.Fatalf("weekly count = %d, want all 101 rows, independent of list bound", weeklyCount)
	}
	for i := 1; i < len(invites); i++ {
		if invites[i].IssuedAt.After(invites[i-1].IssuedAt) {
			t.Fatalf("invite %d is newer than preceding row: %s > %s", invites[i].ID, invites[i].IssuedAt, invites[i-1].IssuedAt)
		}
	}
}

func TestUserInviteHistoryIndexSupportsOwnerQueries(t *testing.T) {
	t.Parallel()
	s := open(t)

	var definition string
	err := store.StorePool(s).QueryRow(context.Background(), `
		SELECT indexdef
		FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND tablename = 'registration_invites'
		  AND indexname = 'registration_invites_inviter_issued_at_idx'
	`).Scan(&definition)
	if err != nil {
		t.Fatalf("read user invite history index: %v", err)
	}
	if !strings.Contains(definition, "(inviter_user_id, issued_at DESC, id DESC)") {
		t.Fatalf("user invite history index = %q, want inviter, issue time, and id ordering", definition)
	}
}
