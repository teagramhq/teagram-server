package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

const messageDeleteReceiptRetentionFloor = 97 * 24 * time.Hour

func messageDeleteNoChangeRetentionFixture(
	t *testing.T,
	ownerID int64,
	msgOffset int64,
	anchor time.Time,
	deadline time.Time,
) messageDeleteReceiptFixture {
	t.Helper()
	f := messageDeletePendingFixture(t, ownerID)
	f.msgID += msgOffset
	f.state = "local_no_change"
	f.localCompletedAt = &anchor
	f.retainUntil = &deadline
	f.operationKey, f.epoch, f.streamID, f.position = nil, nil, nil, nil
	f.targets, f.pts, f.ptsCount = []int32{}, 0, 0
	return f
}

// This constructor creates an independent synthetic confirmed row from a
// pending fixture. It never changes a local_no_change fixture or stored row.
func messageDeleteConfirmedRetentionFixture(
	t *testing.T,
	ownerID int64,
	msgOffset int64,
	anchor time.Time,
	deadline time.Time,
) messageDeleteReceiptFixture {
	t.Helper()
	f := messageDeletePendingFixture(t, ownerID)
	f.msgID += msgOffset
	f.state = "confirmed"
	f.confirmedAt = &anchor
	f.retainUntil = &deadline
	return f
}

func messageDeleteRetentionIdentity(f messageDeleteReceiptFixture, inventoryOutcome string, inventoryDays int32) db.MessageDeleteReceiptRetentionEligibleParams {
	return db.MessageDeleteReceiptRetentionEligibleParams{
		OwnerID: f.ownerID, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
		InventoryOutcome: inventoryOutcome, InventoryDays: inventoryDays,
	}
}

func messageDeleteRetentionIdentityAt(
	f messageDeleteReceiptFixture,
	inventoryOutcome string,
	inventoryDays int32,
	now time.Time,
) db.MessageDeleteReceiptRetentionEligibleParams {
	params := messageDeleteRetentionIdentity(f, inventoryOutcome, inventoryDays)
	nowUnixNano := now.UnixNano()
	params.TestClockUnixNano = &nowUnixNano
	return params
}

func messageDeleteRetentionExtensionIdentity(f messageDeleteReceiptFixture, inventoryOutcome string, inventoryDays int32) db.ExtendMessageDeleteReceiptRetentionParams {
	return db.ExtendMessageDeleteReceiptRetentionParams{
		OwnerID: f.ownerID, AuthKeyID: f.authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
		InventoryOutcome: inventoryOutcome, InventoryDays: inventoryDays,
	}
}

func TestMessageDeleteReceiptRetentionEligibilityUsesDatabaseClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795081")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin eligibility transaction: %v", err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("roll back eligibility transaction: %v", err)
		}
	})
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&databaseNow); err != nil {
		t.Fatalf("read database transaction clock: %v", err)
	}
	q := db.New(tx)

	cases := []struct {
		name             string
		fixture          messageDeleteReceiptFixture
		inventoryOutcome string
		inventoryDays    int32
		clockNow         time.Time
		useTestClock     bool
		extendDays       int32
		wantEligible     bool
	}{
		{
			name: "local no-change one database microsecond before the floor is protected",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 4,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor+time.Microsecond),
				databaseNow.Add(time.Microsecond)),
			inventoryOutcome: "known", inventoryDays: 97,
		},
		{
			name: "local no-change at the floor is protected",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 8,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor), databaseNow),
			inventoryOutcome: "known", inventoryDays: 97,
		},
		{
			name: "local no-change one database microsecond after the floor is eligible",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 12,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor-time.Microsecond),
				databaseNow.Add(-time.Microsecond)),
			inventoryOutcome: "known", inventoryDays: 97, wantEligible: true,
		},
		{
			name: "unit clock protects one nanosecond before the floor",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 5,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor), databaseNow),
			inventoryOutcome: "known", inventoryDays: 97,
			clockNow: databaseNow.Add(-time.Nanosecond), useTestClock: true,
		},
		{
			name: "unit clock protects at the floor",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 6,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor), databaseNow),
			inventoryOutcome: "known", inventoryDays: 97,
			clockNow: databaseNow, useTestClock: true,
		},
		{
			name: "unit clock makes the receipt eligible one nanosecond after the floor",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 7,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor), databaseNow),
			inventoryOutcome: "known", inventoryDays: 97,
			clockNow: databaseNow.Add(time.Nanosecond), useTestClock: true, wantEligible: true,
		},
		{
			name: "stored 120-day deadline protects an expired 97-day anchor one microsecond before",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 9,
				databaseNow.Add(-120*24*time.Hour+time.Microsecond), databaseNow.Add(-23*24*time.Hour+time.Microsecond)),
			inventoryOutcome: "known", inventoryDays: 97, extendDays: 120,
		},
		{
			name: "stored 120-day deadline protects an expired 97-day anchor at equality",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 10,
				databaseNow.Add(-120*24*time.Hour), databaseNow.Add(-23*24*time.Hour)),
			inventoryOutcome: "known", inventoryDays: 97, extendDays: 120,
		},
		{
			name: "stored 120-day deadline permits an expired 97-day anchor one microsecond afterward",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 11,
				databaseNow.Add(-120*24*time.Hour-time.Microsecond), databaseNow.Add(-23*24*time.Hour-time.Microsecond)),
			inventoryOutcome: "known", inventoryDays: 97, extendDays: 120, wantEligible: true,
		},
		{
			name: "confirmed receipt uses its confirmation anchor",
			fixture: messageDeleteConfirmedRetentionFixture(t, owner, 16,
				databaseNow.Add(-98*24*time.Hour), databaseNow.Add(-24*time.Hour)),
			inventoryOutcome: "known", inventoryDays: 97, wantEligible: true,
		},
		{
			name: "120-day inventory protects a receipt one second past the floor",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 20,
				databaseNow.Add(-messageDeleteReceiptRetentionFloor-time.Second),
				databaseNow.Add(-time.Second)),
			inventoryOutcome: "known", inventoryDays: 120,
		},
		{
			name: "unknown inventory refuses an old terminal receipt",
			fixture: messageDeleteNoChangeRetentionFixture(t, owner, 24,
				databaseNow.Add(-200*24*time.Hour), databaseNow.Add(-103*24*time.Hour)),
			inventoryOutcome: "unknown", inventoryDays: int32(2_147_483_647),
		},
		{
			name: "conflicting inventory refuses an old terminal receipt",
			fixture: messageDeleteConfirmedRetentionFixture(t, owner, 28,
				databaseNow.Add(-200*24*time.Hour), databaseNow.Add(-103*24*time.Hour)),
			inventoryOutcome: "conflicting", inventoryDays: int32(2_147_483_647),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := messageDeleteReceiptRawInsert(ctx, tx, tc.fixture); err != nil {
				t.Fatalf("insert retention fixture: %v", err)
			}
			if tc.extendDays != 0 {
				result, err := q.ExtendMessageDeleteReceiptRetention(ctx,
					messageDeleteRetentionExtensionIdentity(tc.fixture, "known", tc.extendDays))
				if err != nil || result.Outcome != "extended" || !result.RetainUntil.Valid ||
					!result.RetainUntil.Time.Equal(tc.fixture.localCompletedAt.Add(time.Duration(tc.extendDays)*24*time.Hour)) {
					t.Fatalf("extend stored-deadline fixture = %+v, err %v", result, err)
				}
			}
			params := messageDeleteRetentionIdentity(tc.fixture, tc.inventoryOutcome, tc.inventoryDays)
			if tc.useTestClock {
				params = messageDeleteRetentionIdentityAt(tc.fixture, tc.inventoryOutcome, tc.inventoryDays, tc.clockNow)
			}
			eligible, err := q.MessageDeleteReceiptRetentionEligible(ctx, params)
			if err != nil {
				t.Fatalf("evaluate eligibility: %v", err)
			}
			if eligible != tc.wantEligible {
				t.Fatalf("eligible = %v, want %v", eligible, tc.wantEligible)
			}
		})
	}

	pending := messageDeletePendingFixture(t, owner)
	pending.msgID = 1 // A legacy-looking identity has no retention authority.
	if err := messageDeleteReceiptRawInsert(ctx, tx, pending); err != nil {
		t.Fatalf("insert pending receipt: %v", err)
	}
	eligible, err := q.MessageDeleteReceiptRetentionEligible(ctx, messageDeleteRetentionIdentity(pending, "known", int32(2_147_483_647)))
	if err != nil || eligible {
		t.Fatalf("old pending receipt eligible = %v, err %v, want false", eligible, err)
	}
}

func TestMessageDeleteReceiptRetentionExtensionIsMonotonicAndInert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	conn := profilePhotoConn(t, ctx)
	if _, err := conn.Exec(ctx, `SET TIME ZONE 'America/New_York'`); err != nil {
		t.Fatalf("set database timezone across DST: %v", err)
	}
	owner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795082")
	otherOwner := messageDeleteReceiptOwner(t, ctx, conn, "+15551795083")
	q := db.New(conn)
	anchor := time.Date(2026, 3, 7, 12, 0, 0, 0, time.FixedZone("UTC-5", -5*60*60))
	floorDeadline := anchor.Add(messageDeleteReceiptRetentionFloor)
	confirmed := messageDeleteConfirmedRetentionFixture(t, owner, 4, anchor, floorDeadline)
	noChange := messageDeleteNoChangeRetentionFixture(t, owner, 8, anchor, floorDeadline)
	other := messageDeleteNoChangeRetentionFixture(t, otherOwner, 4, anchor, floorDeadline)
	pending := messageDeletePendingFixture(t, owner)
	pending.msgID += 12
	for _, f := range []messageDeleteReceiptFixture{confirmed, noChange, other, pending} {
		if err := messageDeleteReceiptRawInsert(ctx, conn, f); err != nil {
			t.Fatalf("insert receipt fixture for owner %d: %v", f.ownerID, err)
		}
	}
	if _, err := conn.Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id,total) VALUES ($1,17)`, owner); err != nil {
		t.Fatalf("seed owner refusal total: %v", err)
	}
	otherBefore, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(other))
	if err != nil {
		t.Fatalf("read other owner's row before extensions: %v", err)
	}

	for _, f := range []messageDeleteReceiptFixture{confirmed, noChange} {
		t.Run(f.state, func(t *testing.T) {
			before, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
			if err != nil {
				t.Fatalf("read initial receipt: %v", err)
			}
			result, err := q.ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(f, "known", 120))
			if err != nil || result.Outcome != "extended" || !result.RetainUntil.Valid ||
				!result.RetainUntil.Time.Equal(anchor.Add(120*24*time.Hour)) {
				t.Fatalf("120-day extension = %+v, err %v", result, err)
			}
			got, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(f))
			if err != nil {
				t.Fatalf("read extended receipt: %v", err)
			}
			want := before
			want.RetainUntil = got.RetainUntil
			if !reflect.DeepEqual(got, want) || !got.RetainUntil.Time.Equal(anchor.Add(120*24*time.Hour)) {
				t.Fatalf("extension changed fields other than deadline: got %+v, before %+v", got, before)
			}

			var ctidBeforeNoops string
			if err := conn.QueryRow(ctx, `SELECT ctid::text FROM message_delete_receipts WHERE owner_id=$1 AND auth_key_id=$2 AND session_id=$3 AND msg_id=$4`,
				f.ownerID, f.authKeyID, f.sessionID, f.msgID).Scan(&ctidBeforeNoops); err != nil {
				t.Fatalf("read ctid after extension: %v", err)
			}
			for _, tc := range []struct {
				name   string
				status string
				days   int32
			}{
				{name: "shorter", status: "known", days: 119},
				{name: "equal", status: "known", days: 120},
				{name: "unknown", status: "unknown", days: int32(2_147_483_647)},
				{name: "conflicting", status: "conflicting", days: int32(2_147_483_647)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					result, err := q.ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(f, tc.status, tc.days))
					wantOutcome := "unchanged"
					switch tc.status {
					case "unknown":
						wantOutcome = "refused_unknown"
					case "conflicting":
						wantOutcome = "refused_conflicting"
					}
					if err != nil || result.Outcome != wantOutcome || !result.RetainUntil.Valid ||
						!result.RetainUntil.Time.Equal(anchor.Add(120*24*time.Hour)) {
						t.Fatalf("%s extension = %+v, err %v, want %s at unchanged deadline", tc.name, result, err, wantOutcome)
					}
					var ctid string
					if err := conn.QueryRow(ctx, `SELECT ctid::text FROM message_delete_receipts WHERE owner_id=$1 AND auth_key_id=$2 AND session_id=$3 AND msg_id=$4`,
						f.ownerID, f.authKeyID, f.sessionID, f.msgID).Scan(&ctid); err != nil || ctid != ctidBeforeNoops {
						t.Fatalf("%s extension rewrote the receipt: ctid %q -> %q, err %v", tc.name, ctidBeforeNoops, ctid, err)
					}
				})
			}
		})
	}

	pendingResult, err := q.ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(pending, "known", int32(2_147_483_647)))
	if err != nil || pendingResult.Outcome != "pending" || pendingResult.RetainUntil.Valid {
		t.Fatalf("pending extension = %+v, err %v, want no deadline", pendingResult, err)
	}

	otherAfter, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, messageDeleteIdentity(other))
	if err != nil {
		t.Fatalf("read other owner's row: %v", err)
	}
	if !reflect.DeepEqual(otherAfter, otherBefore) {
		t.Fatalf("extension changed another owner's row: got %+v, want %+v", otherAfter, otherBefore)
	}
	for _, ownerID := range []int64{owner, otherOwner} {
		count, err := q.MessageDeleteReceiptOwnerCount(ctx, ownerID)
		want := int64(3)
		if ownerID == otherOwner {
			want = 1
		}
		if err != nil || count != want {
			t.Fatalf("owner %d receipt count = %d, err %v, want %d", ownerID, count, err, want)
		}
	}
	if total, err := q.MessageDeleteCapacityRefusalByOwner(ctx, owner); err != nil || total != 17 {
		t.Fatalf("owner refusal total = %d, err %v, want 17", total, err)
	}
	if _, err := q.MessageDeleteCapacityRefusalByOwner(ctx, otherOwner); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("other owner's refusal total = %v, want no row", err)
	}
}

func TestMessageDeleteReceiptRetentionDoesNotChangeCompositionOutcome(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551795084")
	b := mustUser(t, s, "+15551795085")
	chat := chatWith(t, s, a, b)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "retention composition", RandomID: 7301})
	for _, ownerID := range []int64{a.ID, b.ID} {
		if _, err := s.DeleteMessages(ctx, ownerID, []int64{message.LocalID}, false); err != nil {
			t.Fatalf("clear owner %d copy: %v", ownerID, err)
		}
	}

	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID += 100
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}
	stream := messageDeleteCaptureStream(0x60)
	perOwner, before, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 40, stream, nil)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("compose no-change receipt = %+v, err %v", perOwner, err)
	}
	beforeResult := messageDeleteReceiptLookupFound(t, before)
	if beforeResult.State != "local_no_change" {
		t.Fatalf("composition state = %q, want local_no_change", beforeResult.State)
	}

	q := db.New(store.StorePool(s))
	var receipt messageDeleteReceiptFixture
	receipt.ownerID, receipt.authKeyID, receipt.sessionID, receipt.msgID = a.ID, identity.AuthKeyID[:], identity.SessionID, identity.MsgID
	beforeCount := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID)
	beforeRefusals := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID)
	beforeLookup, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
	if err != nil {
		t.Fatalf("lookup before retention operations: %v", err)
	}
	retained, err := q.ExtendMessageDeleteReceiptRetention(ctx, messageDeleteRetentionExtensionIdentity(receipt, "known", 120))
	if err != nil || retained.Outcome != "extended" {
		t.Fatalf("extend composed receipt = %+v, err %v", retained, err)
	}
	eligible, err := q.MessageDeleteReceiptRetentionEligible(ctx, messageDeleteRetentionIdentity(receipt, "known", 120))
	if err != nil || eligible {
		t.Fatalf("freshly extended receipt eligible = %v, err %v, want false", eligible, err)
	}

	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != beforeCount {
		t.Fatalf("retention operations changed owner count: %d -> %d", beforeCount, count)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != beforeRefusals {
		t.Fatalf("retention operations changed refusal total: %d -> %d", beforeRefusals, total)
	}
	afterLookup, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
	if err != nil || afterLookup.Outcome != beforeLookup.Outcome ||
		beforeLookup.Result == nil || afterLookup.Result == nil || *afterLookup.Result != *beforeLookup.Result {
		t.Fatalf("retention operations changed lookup: before %+v, after %+v, err %v", beforeLookup, afterLookup, err)
	}

	perOwner, after, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 40, stream, func() error {
		t.Fatal("retention operations made composition rerun capture")
		return nil
	})
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("compose retry after retention = %+v, err %v", perOwner, err)
	}
	afterResult := messageDeleteReceiptLookupFound(t, after)
	if *afterResult != *beforeResult {
		t.Fatalf("retention operations changed composition result: before %+v, after %+v", beforeResult, afterResult)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != beforeCount {
		t.Fatalf("composition replay changed receipt count after retention: %d -> %d", beforeCount, count)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != beforeRefusals {
		t.Fatalf("composition replay changed refusal total after retention: %d -> %d", beforeRefusals, total)
	}
}
