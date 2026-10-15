package store_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

const messageDeleteRemovalTimeout = 30 * time.Second

func messageDeleteRemovalDatabaseNow(t *testing.T, ctx context.Context, s *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := store.StorePool(s).QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	return now
}

func messageDeleteRemovalRow(t *testing.T, ctx context.Context, s *store.Store, identity store.MessageDeleteReceiptLookupIdentity) (db.MessageDeleteReceipt, bool) {
	t.Helper()
	row, err := db.New(store.StorePool(s)).MessageDeleteReceiptByOwnerIdentity(ctx, db.MessageDeleteReceiptByOwnerIdentityParams{
		OwnerID: identity.OwnerID, AuthKeyID: identity.AuthKeyID[:], SessionID: identity.SessionID, MsgID: identity.MsgID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.MessageDeleteReceipt{}, false
	}
	if err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	return row, true
}

// beginMessageDeleteRemoval leaves the removal uncommitted; cleanup rolls back
// any removal the test did not finish.
func beginMessageDeleteRemoval(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	identity store.MessageDeleteReceiptLookupIdentity,
	inventoryOutcome string,
	inventoryDays int32,
	clock *time.Time,
) *store.MessageDeleteReceiptRemovalForTest {
	t.Helper()
	removal, err := store.BeginMessageDeleteReceiptRemovalForTest(ctx, s, identity, inventoryOutcome, inventoryDays, clock)
	if err != nil {
		t.Fatalf("begin receipt removal: %v", err)
	}
	t.Cleanup(func() {
		if err := removal.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back receipt removal: %v", err)
		}
	})
	return removal
}

func commitMessageDeleteRemoval(t *testing.T, ctx context.Context, removal *store.MessageDeleteReceiptRemovalForTest) {
	t.Helper()
	if err := removal.Commit(ctx); err != nil {
		t.Fatalf("commit receipt removal: %v", err)
	}
}

// Synthetic confirmation is test-only: it turns a composed pending receipt
// into a confirmed one and never touches a local_no_change receipt.
func messageDeleteSyntheticallyConfirm(t *testing.T, ctx context.Context, s *store.Store, identity store.MessageDeleteReceiptLookupIdentity) {
	t.Helper()
	tag, err := store.StorePool(s).Exec(ctx, `
		UPDATE message_delete_receipts
		SET state = 'confirmed', confirmed_at = statement_timestamp(),
		    retain_until = statement_timestamp() + INTERVAL '8380800 seconds'
		WHERE owner_id = $1 AND auth_key_id = $2 AND session_id = $3 AND msg_id = $4 AND state = 'pending'`,
		identity.OwnerID, identity.AuthKeyID[:], identity.SessionID, identity.MsgID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("synthetically confirm receipt: rows %d, err %v", tag.RowsAffected(), err)
	}
}

func messageDeleteOutboxTotal(t *testing.T, ctx context.Context, s *store.Store) int64 {
	t.Helper()
	var outbox, counters int64
	if err := store.StorePool(s).QueryRow(ctx,
		`SELECT (SELECT count(*) FROM erasure_outbox), (SELECT count(*) FROM erasure_stream_counter)`).Scan(&outbox, &counters); err != nil {
		t.Fatalf("count outbox and stream counters: %v", err)
	}
	return outbox + counters
}

func TestMessageDeleteReceiptRemovalDeletesOnlyEligibleTerminalReceipts(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	pool := store.StorePool(s)
	owner := mustUser(t, s, "+15551795201").ID
	other := mustUser(t, s, "+15551795202").ID
	now := messageDeleteRemovalDatabaseNow(t, ctx, s)
	floor := messageDeleteReceiptRetentionFloor
	day := 24 * time.Hour
	clockAnchor := now.Add(-10 * day)
	at := func(t time.Time) *time.Time { return &t }

	cases := []struct {
		name             string
		fixture          messageDeleteReceiptFixture
		inventoryOutcome string
		inventoryDays    int32
		clock            *time.Time
		wantRemoved      bool
	}{
		{
			name:             "local no-change past the floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 4, now.Add(-floor-time.Hour), now.Add(-time.Hour)),
			inventoryOutcome: "known", inventoryDays: 97, wantRemoved: true,
		},
		{
			name:             "confirmed past the floor",
			fixture:          messageDeleteConfirmedRetentionFixture(t, owner, 8, now.Add(-98*day), now.Add(-day)),
			inventoryOutcome: "known", inventoryDays: 97, wantRemoved: true,
		},
		{
			name:             "unit clock one nanosecond before the floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 12, clockAnchor, clockAnchor.Add(floor)),
			inventoryOutcome: "known", inventoryDays: 97, clock: at(clockAnchor.Add(floor - time.Nanosecond)),
		},
		{
			name:             "unit clock at the floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 16, clockAnchor, clockAnchor.Add(floor)),
			inventoryOutcome: "known", inventoryDays: 97, clock: at(clockAnchor.Add(floor)),
		},
		{
			name:             "unit clock one nanosecond after the floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 20, clockAnchor, clockAnchor.Add(floor)),
			inventoryOutcome: "known", inventoryDays: 97, clock: at(clockAnchor.Add(floor + time.Nanosecond)), wantRemoved: true,
		},
		{
			name:             "120-day inventory protects one second past the floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 24, now.Add(-floor-time.Second), now.Add(-time.Second)),
			inventoryOutcome: "known", inventoryDays: 120,
		},
		{
			name:             "stored extended deadline protects an expired floor",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 28, now.Add(-110*day), now.Add(10*day)),
			inventoryOutcome: "known", inventoryDays: 97,
		},
		{
			name:             "unknown inventory",
			fixture:          messageDeleteNoChangeRetentionFixture(t, owner, 32, now.Add(-200*day), now.Add(-103*day)),
			inventoryOutcome: "unknown", inventoryDays: math.MaxInt32,
		},
		{
			name:             "conflicting inventory",
			fixture:          messageDeleteConfirmedRetentionFixture(t, owner, 36, now.Add(-200*day), now.Add(-103*day)),
			inventoryOutcome: "conflicting", inventoryDays: math.MaxInt32,
		},
	}
	pending := messageDeletePendingFixture(t, owner)
	pending.msgID = 1 // Ancient-looking pending receipts have no expiry authority.
	twin := messageDeleteNoChangeRetentionFixture(t, other, 4, now.Add(-floor-time.Hour), now.Add(-time.Hour))
	for _, f := range []messageDeleteReceiptFixture{pending, twin} {
		if err := messageDeleteReceiptRawInsert(ctx, pool, f); err != nil {
			t.Fatalf("insert receipt for owner %d: %v", f.ownerID, err)
		}
	}
	for _, tc := range cases {
		if err := messageDeleteReceiptRawInsert(ctx, pool, tc.fixture); err != nil {
			t.Fatalf("insert %s: %v", tc.name, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,17)`, owner); err != nil {
		t.Fatalf("seed refusal total: %v", err)
	}
	twinBefore, _ := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(twin))
	ownerPts, ownerEvents := ptsOf(t, s, owner), eventsOf(t, s, owner, 0)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := messageDeleteReceiptLookupIdentity(tc.fixture)
			before, _ := messageDeleteRemovalRow(t, ctx, s, identity)
			removal := beginMessageDeleteRemoval(t, ctx, s, identity, tc.inventoryOutcome, tc.inventoryDays, tc.clock)
			if removal.Removed() != tc.wantRemoved {
				t.Fatalf("removed = %v, want %v", removal.Removed(), tc.wantRemoved)
			}
			commitMessageDeleteRemoval(t, ctx, removal)
			after, present := messageDeleteRemovalRow(t, ctx, s, identity)
			if present == tc.wantRemoved {
				t.Fatalf("receipt present after removal = %v, want %v", present, !tc.wantRemoved)
			}
			if present && !reflect.DeepEqual(after, before) {
				t.Fatalf("protected receipt changed: before %+v, after %+v", before, after)
			}
		})
	}

	farFuture := now.Add(10_000 * day)
	removal := beginMessageDeleteRemoval(t, ctx, s, messageDeleteReceiptLookupIdentity(pending), "known", math.MaxInt32, &farFuture)
	if removal.Removed() {
		t.Fatal("pending receipt was removed")
	}
	commitMessageDeleteRemoval(t, ctx, removal)
	if _, present := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(pending)); !present {
		t.Fatal("pending receipt is gone")
	}

	if twinAfter, present := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(twin)); !present || !reflect.DeepEqual(twinAfter, twinBefore) {
		t.Fatalf("removal changed another owner's same-identity receipt: present %v, got %+v, want %+v", present, twinAfter, twinBefore)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, owner); count != 7 {
		t.Fatalf("owner receipt count = %d, want 7 after three removals", count)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, owner); total != 17 {
		t.Fatalf("owner refusal total = %d, want 17", total)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, other); total != 0 {
		t.Fatalf("other owner refusal total = %d, want 0", total)
	}
	if total := messageDeleteOutboxTotal(t, ctx, s); total != 0 {
		t.Fatalf("removal wrote %d outbox or stream counter rows", total)
	}
	if pts, events := ptsOf(t, s, owner), eventsOf(t, s, owner, 0); pts != ownerPts || !reflect.DeepEqual(events, ownerEvents) {
		t.Fatalf("removal changed owner pts or events: %d %+v -> %d %+v", ownerPts, ownerEvents, pts, events)
	}
}

func TestMessageDeleteReceiptRemovalLeavesReceiptProtectedSinceCheck(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	pool := store.StorePool(s)
	q := db.New(pool)
	owner := mustUser(t, s, "+15551795203").ID
	now := messageDeleteRemovalDatabaseNow(t, ctx, s)
	anchor := now.Add(-100 * 24 * time.Hour)

	checkedEligible := func(t *testing.T, offset int64) messageDeleteReceiptFixture {
		t.Helper()
		f := messageDeleteNoChangeRetentionFixture(t, owner, offset, anchor, now.Add(-3*24*time.Hour))
		if err := messageDeleteReceiptRawInsert(ctx, pool, f); err != nil {
			t.Fatalf("insert eligible receipt: %v", err)
		}
		eligible, err := q.MessageDeleteReceiptRetentionEligible(ctx, messageDeleteRetentionIdentity(f, "known", 97))
		if err != nil || !eligible {
			t.Fatalf("receipt eligible at check = %v, err %v, want true", eligible, err)
		}
		return f
	}
	assertKept := func(t *testing.T, f messageDeleteReceiptFixture, removal *store.MessageDeleteReceiptRemovalForTest, want db.MessageDeleteReceipt) {
		t.Helper()
		if removal.Removed() {
			t.Fatal("removal deleted a receipt that became protected")
		}
		commitMessageDeleteRemoval(t, ctx, removal)
		if got, present := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(f)); !present || !reflect.DeepEqual(got, want) {
			t.Fatalf("protected receipt after removal: present %v, got %+v, want %+v", present, got, want)
		}
	}

	t.Run("deadline extended", func(t *testing.T) {
		f := checkedEligible(t, 4)
		if result, err := q.ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(f, "known", 120)); err != nil || result.Outcome != "extended" {
			t.Fatalf("extend after check = %+v, err %v", result, err)
		}
		want, _ := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(f))
		assertKept(t, f, beginMessageDeleteRemoval(t, ctx, s, messageDeleteReceiptLookupIdentity(f), "known", 97, nil), want)
	})
	for i, outcome := range []string{"unknown", "conflicting"} {
		t.Run("inventory "+outcome, func(t *testing.T) {
			f := checkedEligible(t, int64(8+4*i))
			want, _ := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(f))
			assertKept(t, f, beginMessageDeleteRemoval(t, ctx, s, messageDeleteReceiptLookupIdentity(f), outcome, 97, nil), want)
		})
	}
	t.Run("extension commits while removal waits on the row", func(t *testing.T) {
		f := checkedEligible(t, 16)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin extension: %v", err)
		}
		defer func() {
			if err := tx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("roll back extension: %v", err)
			}
		}()
		if result, err := db.New(tx).ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(f, "known", 120)); err != nil || result.Outcome != "extended" {
			t.Fatalf("uncommitted extension = %+v, err %v", result, err)
		}
		type outcome struct {
			removal *store.MessageDeleteReceiptRemovalForTest
			err     error
		}
		done := make(chan outcome, 1)
		go func() {
			removal, err := store.BeginMessageDeleteReceiptRemovalForTest(ctx, s, messageDeleteReceiptLookupIdentity(f), "known", 97, nil)
			done <- outcome{removal: removal, err: err}
		}()
		if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
			t.Fatalf("removal did not wait on the extended row: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit extension: %v", err)
		}
		got := <-done
		if got.err != nil {
			t.Fatalf("removal after concurrent extension: %v", got.err)
		}
		t.Cleanup(func() {
			if err := got.removal.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				t.Errorf("roll back receipt removal: %v", err)
			}
		})
		want, _ := messageDeleteRemovalRow(t, ctx, s, messageDeleteReceiptLookupIdentity(f))
		if !want.RetainUntil.Time.Equal(anchor.Add(120 * 24 * time.Hour)) {
			t.Fatalf("extended deadline = %v, want anchor + 120 days", want.RetainUntil.Time)
		}
		assertKept(t, f, got.removal, want)
	})
}

func TestMessageDeleteReceiptRemovalTakesNoOwnerPollOrStreamLock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	owner := mustUser(t, s, "+15551795204").ID
	now := messageDeleteRemovalDatabaseNow(t, ctx, s)
	f := messageDeleteNoChangeRetentionFixture(t, owner, 4, now.Add(-100*24*time.Hour), now.Add(-3*24*time.Hour))
	if err := messageDeleteReceiptRawInsert(ctx, store.StorePool(s), f); err != nil {
		t.Fatalf("insert eligible receipt: %v", err)
	}
	release, err := store.HoldOwnerLock(ctx, s, owner)
	if err != nil {
		t.Fatalf("hold owner lock: %v", err)
	}
	defer release()

	removal := beginMessageDeleteRemoval(t, ctx, s, messageDeleteReceiptLookupIdentity(f), "known", 97, nil)
	if !removal.Removed() {
		t.Fatal("eligible receipt was not removed while its owner lock was held elsewhere")
	}
	rows, err := store.StorePool(s).Query(ctx, `
		SELECT locktype, COALESCE(relation::regclass::text, '')
		FROM pg_locks
		WHERE pid = $1 AND locktype NOT IN ('virtualxid', 'transactionid')`, removal.BackendPID())
	if err != nil {
		t.Fatalf("read removal locks: %v", err)
	}
	locks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) ([2]string, error) {
		var lock [2]string
		err := row.Scan(&lock[0], &lock[1])
		return lock, err
	})
	if err != nil {
		t.Fatalf("scan removal locks: %v", err)
	}
	allowed := map[string]bool{
		"message_delete_receipts": true, "message_delete_receipts_pkey": true,
		"message_delete_receipts_key_unique": true, "message_delete_receipts_owner_idx": true,
		"message_delete_receipts_pending_capture_idx": true,
	}
	for _, lock := range locks {
		if lock[0] != "relation" || !allowed[lock[1]] {
			t.Fatalf("uncommitted removal holds %s lock on %q, want receipt-table relation locks only: %v", lock[0], lock[1], locks)
		}
	}
	if len(locks) == 0 {
		t.Fatal("removal lock inspection observed no locks for its own backend")
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, owner); count != 1 {
		t.Fatalf("uncommitted removal is already uncounted: owner count %d, want 1", count)
	}
	commitMessageDeleteRemoval(t, ctx, removal)
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, owner); count != 0 {
		t.Fatalf("committed removal still counted: owner count %d, want 0", count)
	}
}

func TestMessageDeleteReceiptRemovalReleasesCapacityOnlyAfterCommit(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	a := mustUser(t, s, "+15551795205")
	b := mustUser(t, s, "+15551795206")
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_536)
	seeded := store.MessageDeleteReceiptLookupIdentity{
		OwnerID: a.ID, AuthKeyID: [8]byte{0xff, 0, 1, 2, 3, 4, 5, 0x80}, SessionID: 9191, MsgID: 1,
	}
	clock := messageDeleteRemovalDatabaseNow(t, ctx, s).Add(98 * 24 * time.Hour)
	first := send(t, s, a, b, "capacity after removal", 7401)
	identity := messageDeleteReceiptLookupIdentity(messageDeletePendingFixture(t, a.ID))
	request := func(m store.Message) store.MessageDeleteReceiptLookupRequest {
		return store.MessageDeleteReceiptLookupRequest{
			Method: "messages.deleteMessages", IDSpace: "owner_message",
			TargetIDs: []int32{messageDeleteLocalID(t, m.LocalID)},
		}
	}
	admit := func(msgID int64, m store.Message) (map[int64]int, error) {
		attempt := identity
		attempt.MsgID = msgID
		perOwner, _, err := store.ComposeDeleteMessagesForTest(ctx, s, attempt, request(m), 41, messageDeleteCaptureStream(0x61), nil)
		return perOwner, err
	}
	assertState := func(t *testing.T, what string, wantCount, wantRefusals int64) {
		t.Helper()
		if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != wantCount {
			t.Fatalf("%s: owner count %d, want %d", what, count, wantCount)
		}
		if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != wantRefusals {
			t.Fatalf("%s: refusal total %d, want %d", what, total, wantRefusals)
		}
	}

	removal := beginMessageDeleteRemoval(t, ctx, s, seeded, "known", 97, &clock)
	if !removal.Removed() {
		t.Fatal("aged seeded receipt was not removed")
	}
	if _, err := admit(1700000000201, first); !errors.Is(err, store.ErrMessageDeleteReceiptCapacityExceeded) {
		t.Fatalf("admission during uncommitted removal = %v, want capacity refusal", err)
	}
	assertState(t, "uncommitted removal", 65_536, 1)
	if err := removal.Rollback(ctx); err != nil {
		t.Fatalf("roll back removal: %v", err)
	}
	if _, err := admit(1700000000202, first); !errors.Is(err, store.ErrMessageDeleteReceiptCapacityExceeded) {
		t.Fatalf("admission after rolled-back removal = %v, want capacity refusal", err)
	}
	assertState(t, "rolled-back removal", 65_536, 2)

	removal = beginMessageDeleteRemoval(t, ctx, s, seeded, "known", 97, &clock)
	if !removal.Removed() {
		t.Fatal("seeded receipt was not removed after rollback")
	}
	commitMessageDeleteRemoval(t, ctx, removal)
	assertState(t, "committed removal", 65_535, 2)
	if perOwner, err := admit(1700000000203, first); err != nil || perOwner[a.ID] == 0 {
		t.Fatalf("admission after committed removal = %+v, err %v, want the released slot", perOwner, err)
	}
	assertState(t, "admission into the released slot", 65_536, 2)
	second := send(t, s, a, b, "capacity full again", 7402)
	if _, err := admit(1700000000204, second); !errors.Is(err, store.ErrMessageDeleteReceiptCapacityExceeded) {
		t.Fatalf("admission beyond capacity = %v, want capacity refusal", err)
	}
	assertState(t, "capacity full again", 65_536, 3)
}

func TestMessageDeleteReceiptRemovalSerialOutcomesForOneToOneRetry(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	a := mustUser(t, s, "+15551795207")
	b := mustUser(t, s, "+15551795208")
	message := send(t, s, a, b, "one-to-one removal", 7403)
	identity := messageDeleteReceiptLookupIdentity(messageDeletePendingFixture(t, a.ID))
	identity.MsgID = 1700000000301
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}
	epoch, stream := int64(42), messageDeleteCaptureStream(0x62)
	_, composed, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil)
	if err != nil {
		t.Fatalf("compose delete: %v", err)
	}
	original := messageDeleteReceiptLookupFound(t, composed)
	messageDeleteSyntheticallyConfirm(t, ctx, s, identity)
	copies := []struct{ ownerID, localID int64 }{{a.ID, message.LocalID}, {b.ID, message.PeerLocalID}}
	before := snapshotMessageDeleteState(t, s, copies...)
	noCapture := func() error {
		t.Error("retry reran capture")
		return nil
	}
	clock := messageDeleteRemovalDatabaseNow(t, ctx, s).Add(98 * 24 * time.Hour)

	removal := beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
	if !removal.Removed() {
		t.Fatal("aged confirmed receipt was not removed")
	}
	perOwner, retry, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, noCapture)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("retry before committed removal = %+v, err %v", perOwner, err)
	}
	got := messageDeleteReceiptLookupFound(t, retry)
	if got.Pts != original.Pts || got.PtsCount != original.PtsCount || got.ReceiptKey != original.ReceiptKey ||
		got.CaptureReference == nil || *got.CaptureReference != *original.CaptureReference {
		t.Fatalf("retry before committed removal = %+v, want original %+v", got, original)
	}
	commitMessageDeleteRemoval(t, ctx, removal)

	if lookup, err := s.LookupMessageDeleteReceipt(ctx, identity, request); err != nil || lookup.Outcome != store.MessageDeleteReceiptLookupMissing {
		t.Fatalf("lookup after committed removal = %+v, err %v, want Missing", lookup, err)
	}
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, noCapture); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("retry after committed removal = %v, want the deleted-target refusal", err)
	}
	if _, present := messageDeleteRemovalRow(t, ctx, s, identity); present {
		t.Fatal("refused retry created a receipt")
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 0 {
		t.Fatalf("owner receipt count = %d, want 0", count)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s, copies...))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 1 {
		t.Fatalf("capture rows = %d, want the original one", len(rows))
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != 1 {
		t.Fatalf("stream counter = %d, present %v, want original position 1", position, ok)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
		t.Fatalf("refusal total = %d, want 0", total)
	}
}

func messageDeleteAllDeletedGroupRetry(t *testing.T, ctx context.Context, s *store.Store, phones [2]string, randomID int64) (
	identity store.MessageDeleteReceiptLookupIdentity,
	request store.MessageDeleteReceiptLookupRequest,
	original *store.MessageDeleteLocalCommittedResult,
	copies []struct{ ownerID, localID int64 },
) {
	t.Helper()
	a := mustUser(t, s, phones[0])
	b := mustUser(t, s, phones[1])
	chat := chatWith(t, s, a, b)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "all deleted", RandomID: randomID})
	bCopy, _ := msgOpt(t, s, b.ID, message.LocalID)
	for _, ownerID := range []int64{a.ID, b.ID} {
		if _, err := s.DeleteMessages(ctx, ownerID, []int64{message.LocalID}, false); err != nil {
			t.Fatalf("clear owner %d copy: %v", ownerID, err)
		}
	}
	identity = messageDeleteReceiptLookupIdentity(messageDeletePendingFixture(t, a.ID))
	identity.MsgID = 1700000000400 + randomID
	request = store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}
	perOwner, composed, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 43, messageDeleteCaptureStream(0x63), nil)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("compose all-deleted group = %+v, err %v", perOwner, err)
	}
	original = messageDeleteReceiptLookupFound(t, composed)
	if original.State != "local_no_change" || original.Pts != 0 || original.PtsCount != 1 {
		t.Fatalf("all-deleted group result = %+v, want local_no_change 0/1", original)
	}
	return identity, request, original, []struct{ ownerID, localID int64 }{{a.ID, message.LocalID}, {b.ID, bCopy.LocalID}}
}

func TestMessageDeleteReceiptRemovalSerialOutcomesForAllDeletedGroupRetry(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	identity, request, original, copies := messageDeleteAllDeletedGroupRetry(t, ctx, s, [2]string{"+15551795209", "+15551795210"}, 7404)
	before := snapshotMessageDeleteState(t, s, copies...)
	clock := messageDeleteRemovalDatabaseNow(t, ctx, s).Add(98 * 24 * time.Hour)
	noCapture := func() error {
		t.Error("all-deleted retry ran capture")
		return nil
	}

	removal := beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
	if !removal.Removed() {
		t.Fatal("aged no-change receipt was not removed")
	}
	_, retry, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 43, messageDeleteCaptureStream(0x63), noCapture)
	if err != nil {
		t.Fatalf("retry before committed removal: %v", err)
	}
	if got := messageDeleteReceiptLookupFound(t, retry); *got != *original {
		t.Fatalf("retry before committed removal = %+v, want original %+v", got, original)
	}
	commitMessageDeleteRemoval(t, ctx, removal)

	perOwner, fresh, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 43, messageDeleteCaptureStream(0x63), noCapture)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("retry after committed removal = %+v, err %v", perOwner, err)
	}
	got := messageDeleteReceiptLookupFound(t, fresh)
	if got.State != "local_no_change" || got.Pts != 0 || got.PtsCount != 1 || got.CaptureReference != nil || got.ReceiptKey == original.ReceiptKey {
		t.Fatalf("fresh admission after removal = %+v, want new local_no_change 0/1 receipt", got)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, identity.OwnerID); count != 1 {
		t.Fatalf("owner receipt count = %d, want the one fresh receipt", count)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s, copies...))
	if total := messageDeleteOutboxTotal(t, ctx, s); total != 0 {
		t.Fatalf("fresh no-change admission wrote %d outbox or stream counter rows", total)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, identity.OwnerID); total != 0 {
		t.Fatalf("refusal total = %d, want 0", total)
	}
}

func TestMessageDeleteReceiptRemovalOverlappingAdmissionEndsInOneOutcome(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
	defer cancel()
	identity, request, original, _ := messageDeleteAllDeletedGroupRetry(t, ctx, s, [2]string{"+15551795211", "+15551795212"}, 7405)
	clock := messageDeleteRemovalDatabaseNow(t, ctx, s).Add(98 * 24 * time.Hour)
	stored, _ := messageDeleteRemovalRow(t, ctx, s, identity)

	// Admission overlapping an uncommitted removal reads the committed receipt
	// without waiting; rolling the removal back keeps that receipt unchanged.
	removal := beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
	if !removal.Removed() {
		t.Fatal("aged no-change receipt was not removed")
	}
	_, overlapped, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 43, messageDeleteCaptureStream(0x63), nil)
	if err != nil {
		t.Fatalf("admission overlapping uncommitted removal: %v", err)
	}
	if got := messageDeleteReceiptLookupFound(t, overlapped); *got != *original {
		t.Fatalf("overlapping admission = %+v, want original %+v", got, original)
	}
	if err := removal.Rollback(ctx); err != nil {
		t.Fatalf("roll back removal: %v", err)
	}
	if got, present := messageDeleteRemovalRow(t, ctx, s, identity); !present || !reflect.DeepEqual(got, stored) {
		t.Fatalf("rolled-back removal changed the receipt: present %v, got %+v, want %+v", present, got, stored)
	}

	// After a committed removal, a fresh admission holding the owner lock past
	// its locked Missing check overlaps a second removal of the same identity.
	removal = beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
	commitMessageDeleteRemoval(t, ctx, removal)
	entered := make(chan struct{})
	release := make(chan struct{})
	store.SetMessageDeleteReceiptAfterLockedMissHook(s, func() {
		close(entered)
		<-release
	})
	defer store.SetMessageDeleteReceiptAfterLockedMissHook(s, nil)
	type result struct {
		lookup store.MessageDeleteReceiptLookup
		err    error
	}
	done := make(chan result, 1)
	go func() {
		_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 43, messageDeleteCaptureStream(0x63), nil)
		done <- result{lookup: lookup, err: err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("fresh admission did not reach its locked Missing check")
	}
	overlapping := beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
	removedDuringAdmission := overlapping.Removed()
	commitMessageDeleteRemoval(t, ctx, overlapping)
	close(release)
	admitted := <-done
	if removedDuringAdmission {
		t.Fatal("removal overlapping an uncommitted admission removed a receipt")
	}
	if admitted.err != nil {
		t.Fatalf("fresh admission overlapping removal: %v", admitted.err)
	}
	fresh := messageDeleteReceiptLookupFound(t, admitted.lookup)
	if fresh.State != "local_no_change" || fresh.ReceiptKey == original.ReceiptKey {
		t.Fatalf("fresh admission = %+v, want a new local_no_change receipt", fresh)
	}
	after, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
	if err != nil || after.Result == nil || *after.Result != *fresh {
		t.Fatalf("stored outcome = %+v, err %v, want only the fresh admission %+v", after, err, fresh)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, identity.OwnerID); count != 1 {
		t.Fatalf("owner receipt count = %d, want 1", count)
	}
}

// An admission that saw Missing waits on the owner lock while a competing
// same-identity receipt exists; removal commits or rolls back before the lock
// is released, so the locked recheck alone decides fresh versus stored.
func TestMessageDeleteReceiptRemovalDecidesAdmissionWaitingOnOwnerLock(t *testing.T) {
	t.Parallel()
	for i, tc := range []struct {
		name   string
		commit bool
		phones [2]string
	}{
		{name: "removal committed", commit: true, phones: [2]string{"+15551795220", "+15551795221"}},
		{name: "removal rolled back", phones: [2]string{"+15551795222", "+15551795223"}},
	} {
		commit := tc.commit
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			ctx, cancel := context.WithTimeout(context.Background(), messageDeleteRemovalTimeout)
			defer cancel()
			base, request, _, copies := messageDeleteAllDeletedGroupRetry(t, ctx, s, tc.phones, 7406+int64(i))
			identity := base
			identity.MsgID++
			stream := messageDeleteCaptureStream(0x64)
			clock := messageDeleteRemovalDatabaseNow(t, ctx, s).Add(98 * 24 * time.Hour)
			before := snapshotMessageDeleteState(t, s, copies...)

			var calls atomic.Int32
			entered := make(chan struct{})
			resume := make(chan struct{})
			var resumeOnce sync.Once
			resumeWaiter := func() { resumeOnce.Do(func() { close(resume) }) }
			defer resumeWaiter()
			store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error {
				if calls.Add(1) == 1 {
					close(entered)
					<-resume
				}
				return nil
			})
			defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)

			type result struct {
				perOwner map[int64]int
				lookup   store.MessageDeleteReceiptLookup
				err      error
			}
			done := make(chan result, 1)
			go func() {
				perOwner, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 44, stream, nil)
				done <- result{perOwner: perOwner, lookup: lookup, err: err}
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("waiting admission did not observe initial Missing")
			}

			_, competing, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 44, stream, nil)
			if err != nil {
				t.Fatalf("competing same-identity admission: %v", err)
			}
			stored := messageDeleteReceiptLookupFound(t, competing)
			occupied := messageDeleteReceiptOwnerCount(t, ctx, s, identity.OwnerID)
			if occupied != 2 {
				t.Fatalf("owner count with competing receipt = %d, want 2", occupied)
			}

			release, err := store.HoldOwnerLock(ctx, s, identity.OwnerID)
			if err != nil {
				t.Fatalf("hold owner lock: %v", err)
			}
			defer release()
			resumeWaiter()
			if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
				t.Fatalf("admission did not wait on the owner lock: %v", err)
			}

			removal := beginMessageDeleteRemoval(t, ctx, s, identity, "known", 97, &clock)
			if !removal.Removed() {
				t.Fatal("competing receipt was not removed while the admission waited")
			}
			if commit {
				commitMessageDeleteRemoval(t, ctx, removal)
			} else if err := removal.Rollback(ctx); err != nil {
				t.Fatalf("roll back removal: %v", err)
			}
			release()

			got := <-done
			if got.err != nil || len(got.perOwner) != 0 {
				t.Fatalf("waiting admission = %+v, err %v", got.perOwner, got.err)
			}
			admitted := messageDeleteReceiptLookupFound(t, got.lookup)
			if commit {
				if admitted.State != "local_no_change" || admitted.Pts != 0 || admitted.PtsCount != 1 ||
					admitted.CaptureReference != nil || admitted.ReceiptKey == stored.ReceiptKey {
					t.Fatalf("admission after committed removal = %+v, want fresh local_no_change 0/1", admitted)
				}
			} else if *admitted != *stored {
				t.Fatalf("admission after rolled-back removal = %+v, want stored %+v", admitted, stored)
			}
			after, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
			if err != nil || after.Result == nil || *after.Result != *admitted {
				t.Fatalf("stored outcome = %+v, err %v, want the admission result %+v", after, err, admitted)
			}
			if count := messageDeleteReceiptOwnerCount(t, ctx, s, identity.OwnerID); count != occupied {
				t.Fatalf("owner count after waiting admission = %d, want %d", count, occupied)
			}
			if total := messageDeleteReceiptRefusalTotal(t, ctx, s, identity.OwnerID); total != 0 {
				t.Fatalf("refusal total = %d, want 0", total)
			}
			assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s, copies...))
			if total := messageDeleteOutboxTotal(t, ctx, s); total != 0 {
				t.Fatalf("no-change admissions wrote %d outbox or stream counter rows", total)
			}
		})
	}
}
