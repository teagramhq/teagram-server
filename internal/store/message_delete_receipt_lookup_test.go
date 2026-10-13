package store_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

func messageDeleteReceiptLookupIdentity(f messageDeleteReceiptFixture) store.MessageDeleteReceiptLookupIdentity {
	var authKeyID [8]byte
	copy(authKeyID[:], f.authKeyID)
	return store.MessageDeleteReceiptLookupIdentity{
		OwnerID: f.ownerID, AuthKeyID: authKeyID, SessionID: f.sessionID, MsgID: f.msgID,
	}
}

func messageDeleteReceiptLookupRequest(f messageDeleteReceiptFixture, targets []int32) store.MessageDeleteReceiptLookupRequest {
	return store.MessageDeleteReceiptLookupRequest{
		Method: f.method, IDSpace: f.idSpace, Revoke: f.revoke, TargetIDs: targets,
	}
}

func messageDeleteReceiptInsertForLookup(t *testing.T, ctx context.Context, s *store.Store, f messageDeleteReceiptFixture) {
	t.Helper()
	if _, err := db.New(store.StorePool(s)).InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(f)); err != nil {
		t.Fatalf("insert receipt for lookup: %v", err)
	}
}

func messageDeleteReceiptLookupFound(t *testing.T, got store.MessageDeleteReceiptLookup) *store.MessageDeleteLocalCommittedResult {
	t.Helper()
	if got.Outcome != store.MessageDeleteReceiptLookupFoundLocalCommittedResult || got.Result == nil {
		t.Fatalf("lookup = %+v, want FoundLocalCommittedResult", got)
	}
	return got.Result
}

func TestMessageDeleteReceiptLookupMatchesNormalizedRequestAndReturnsOriginalResult(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15551795101")
	f := messageDeletePendingFixture(t, owner.ID)
	f.targets = []int32{4, 9}
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)

	requestTargets := []int32{9, 4, 4}
	wantTargets := slices.Clone(requestTargets)
	got, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f),
		messageDeleteReceiptLookupRequest(f, requestTargets))
	if err != nil {
		t.Fatalf("lookup matching receipt: %v", err)
	}
	result := messageDeleteReceiptLookupFound(t, got)
	if result.Pts != 7 || result.PtsCount != 2 || result.State != "pending" ||
		!bytes.Equal(result.ReceiptKey[:], f.receiptKey) {
		t.Fatalf("saved result = %+v, want original 7/2 result, key and pending state", result)
	}
	if result.CaptureReference == nil ||
		!bytes.Equal(result.CaptureReference.OperationKey[:], f.operationKey) ||
		result.CaptureReference.Epoch != 3 ||
		!bytes.Equal(result.CaptureReference.StreamID[:], f.streamID) ||
		result.CaptureReference.Position != 11 {
		t.Fatalf("capture reference = %+v, want the opaque stored reference", result.CaptureReference)
	}
	if !slices.Equal(requestTargets, wantTargets) {
		t.Fatalf("lookup mutated retry targets: got %v, want %v", requestTargets, wantTargets)
	}
}

func TestMessageDeleteReceiptLookupUniformlyMismatchesRequestChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15551795102")
	f := messageDeletePendingFixture(t, owner.ID)
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)

	tests := []struct {
		name   string
		mutate func(*store.MessageDeleteReceiptLookupRequest)
	}{
		{name: "method", mutate: func(r *store.MessageDeleteReceiptLookupRequest) { r.Method = "channels.deleteMessages" }},
		{name: "id space", mutate: func(r *store.MessageDeleteReceiptLookupRequest) { r.IDSpace = "channel_message" }},
		{name: "revoke", mutate: func(r *store.MessageDeleteReceiptLookupRequest) { r.Revoke = !r.Revoke }},
		{name: "normalized targets", mutate: func(r *store.MessageDeleteReceiptLookupRequest) { r.TargetIDs = []int32{4, 10} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := messageDeleteReceiptLookupRequest(f, []int32{4, 9})
			tt.mutate(&request)
			got, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f), request)
			if err != nil {
				t.Fatalf("lookup changed request: %v", err)
			}
			if got.Outcome != store.MessageDeleteReceiptLookupRequestMismatch || got.Result != nil {
				t.Fatalf("lookup changed request = %+v, want ReceiptRequestMismatch without result", got)
			}
		})
	}
}

func TestMessageDeleteReceiptLookupIsOwnerAndTupleIsolated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15551795103")
	b := mustUser(t, s, "+15551795104")
	f := messageDeletePendingFixture(t, a.ID)
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)
	request := messageDeleteReceiptLookupRequest(f, []int32{4, 9})

	otherOwner := messageDeleteReceiptLookupIdentity(f)
	otherOwner.OwnerID = b.ID
	conflicting := request
	conflicting.Method = "channels.deleteMessages"
	conflicting.TargetIDs = []int32{99}
	got, err := s.LookupMessageDeleteReceipt(ctx, otherOwner, conflicting)
	if err != nil || got.Outcome != store.MessageDeleteReceiptLookupMissing || got.Result != nil {
		t.Fatalf("owner B lookup of A's receipt = %+v, err %v, want Missing", got, err)
	}

	for _, vary := range []func(*store.MessageDeleteReceiptLookupIdentity){
		func(i *store.MessageDeleteReceiptLookupIdentity) { i.AuthKeyID[0] ^= 0xff },
		func(i *store.MessageDeleteReceiptLookupIdentity) { i.SessionID++ },
		func(i *store.MessageDeleteReceiptLookupIdentity) { i.MsgID++ },
	} {
		identity := messageDeleteReceiptLookupIdentity(f)
		vary(&identity)
		got, err := s.LookupMessageDeleteReceipt(ctx, identity, request)
		if err != nil || got.Outcome != store.MessageDeleteReceiptLookupMissing || got.Result != nil {
			t.Fatalf("changed transport tuple lookup = %+v, err %v, want Missing", got, err)
		}
	}

	bReceipt := messageDeletePendingFixture(t, b.ID)
	bReceipt.authKeyID = slices.Clone(f.authKeyID)
	bReceipt.sessionID, bReceipt.msgID = f.sessionID, f.msgID
	bReceipt.pts, bReceipt.ptsCount = 13, 1
	bReceipt.targets = []int32{21}
	messageDeleteReceiptInsertForLookup(t, ctx, s, bReceipt)
	got, err = s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(bReceipt),
		messageDeleteReceiptLookupRequest(bReceipt, []int32{21, 21}))
	if err != nil {
		t.Fatalf("owner B lookup of B's same-tuple receipt: %v", err)
	}
	result := messageDeleteReceiptLookupFound(t, got)
	if result.Pts != 13 || result.PtsCount != 1 || !bytes.Equal(result.ReceiptKey[:], bReceipt.receiptKey) {
		t.Fatalf("owner B received another owner's result: %+v", result)
	}
}

func TestMessageDeleteReceiptLookupReturnsEveryRetainedStateAndOriginalCount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15551795105")

	pending := messageDeletePendingFixture(t, owner.ID)
	messageDeleteReceiptInsertForLookup(t, ctx, s, pending)

	confirmed := messageDeletePendingFixture(t, owner.ID)
	confirmed.msgID += 4
	confirmed.state = "confirmed"
	anchor := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	confirmed.confirmedAt = &anchor
	confirmedDeadline := anchor.Add(97 * 24 * time.Hour)
	confirmed.retainUntil = &confirmedDeadline
	if err := messageDeleteReceiptRawInsert(ctx, store.StorePool(s), confirmed); err != nil {
		t.Fatalf("insert synthetic retained confirmation: %v", err)
	}

	noChangeGroup := messageDeletePendingFixture(t, owner.ID)
	noChangeGroup.msgID += 8
	noChangeGroup.targets = []int32{4}
	noChangeGroup.pts, noChangeGroup.ptsCount = 0, 2
	noChangeGroup.state = "local_no_change"
	noChangeGroup.operationKey, noChangeGroup.epoch, noChangeGroup.streamID, noChangeGroup.position = nil, nil, nil, nil
	noChangeGroup.localCompletedAt = &anchor
	noChangeGroup.retainUntil = &confirmedDeadline
	if err := messageDeleteReceiptRawInsert(ctx, store.StorePool(s), noChangeGroup); err != nil {
		t.Fatalf("insert group no-change receipt: %v", err)
	}

	noChangeEmpty := messageDeletePendingFixture(t, owner.ID)
	noChangeEmpty.msgID += 12
	noChangeEmpty.targets = []int32{}
	noChangeEmpty.pts, noChangeEmpty.ptsCount = 0, 0
	noChangeEmpty.state = "local_no_change"
	noChangeEmpty.operationKey, noChangeEmpty.epoch, noChangeEmpty.streamID, noChangeEmpty.position = nil, nil, nil, nil
	noChangeEmpty.localCompletedAt = &anchor
	noChangeEmpty.retainUntil = &confirmedDeadline
	if err := messageDeleteReceiptRawInsert(ctx, store.StorePool(s), noChangeEmpty); err != nil {
		t.Fatalf("insert empty no-change receipt: %v", err)
	}

	for _, tc := range []struct {
		name         string
		fixture      messageDeleteReceiptFixture
		requestIDs   []int32
		wantPts      int32
		wantPtsCount int32
		wantState    string
		wantCapture  bool
	}{
		{name: "pending at any age", fixture: pending, requestIDs: []int32{9, 4}, wantPts: 7, wantPtsCount: 2, wantState: "pending", wantCapture: true},
		{name: "retained synthetic confirmation after eligibility", fixture: confirmed, requestIDs: []int32{4, 9}, wantPts: 7, wantPtsCount: 2, wantState: "confirmed", wantCapture: true},
		{name: "no-change duplicate group preserves original count", fixture: noChangeGroup, requestIDs: []int32{4}, wantPtsCount: 2, wantState: "local_no_change"},
		{name: "empty no-change", fixture: noChangeEmpty, requestIDs: []int32{}, wantState: "local_no_change"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(tc.fixture),
				messageDeleteReceiptLookupRequest(tc.fixture, tc.requestIDs))
			if err != nil {
				t.Fatalf("lookup %s receipt: %v", tc.name, err)
			}
			result := messageDeleteReceiptLookupFound(t, got)
			if result.Pts != tc.wantPts || result.PtsCount != tc.wantPtsCount || result.State != tc.wantState {
				t.Fatalf("%s result = %+v, want pts/count/state %d/%d/%s", tc.name, result, tc.wantPts, tc.wantPtsCount, tc.wantState)
			}
			if (result.CaptureReference != nil) != tc.wantCapture {
				t.Fatalf("%s capture reference = %+v, want present %v", tc.name, result.CaptureReference, tc.wantCapture)
			}
		})
	}
}

func TestMessageDeleteReceiptLookupAcceptsMoreThanOneHundredDuplicateTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	owner := mustUser(t, s, "+15551795106")
	f := messageDeletePendingFixture(t, owner.ID)
	f.targets = []int32{4, 9}
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)

	targets := make([]int32, 0, 202)
	for range 101 {
		targets = append(targets, 9, 4)
	}
	wantTargets := slices.Clone(targets)
	got, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f),
		messageDeleteReceiptLookupRequest(f, targets))
	if err != nil {
		t.Fatalf("lookup normalized over-limit retry vector: %v", err)
	}
	result := messageDeleteReceiptLookupFound(t, got)
	if result.Pts != 7 || result.PtsCount != 2 || !bytes.Equal(result.ReceiptKey[:], f.receiptKey) {
		t.Fatalf("over-limit duplicate retry result = %+v, want original result", result)
	}
	if !slices.Equal(targets, wantTargets) {
		t.Fatalf("lookup mutated over-limit retry vector: got %v, want %v", targets, wantTargets)
	}
}

func TestMessageDeleteReceiptLookupSurvivesMembershipAndAuthKeyChangesAndReconnect(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	a := mustUser(t, s, "+15551795107")
	b := mustUser(t, s, "+15551795108")
	const authKeyID = int64(0x1810)
	if err := s.SaveAuthKey(ctx, authKeyID, []byte("lookup test auth key")); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	if err := s.BindAuthKeyUser(ctx, authKeyID, b.ID); err != nil {
		t.Fatalf("bind auth key to owner: %v", err)
	}
	chat := chatWith(t, s, a, b)
	groupMessage, _ := sendChat(t, s, store.FanOut{
		ChatID: chat.ID, FromID: b.ID, Text: "receipt survives membership change", RandomID: 1810,
	})
	if groupMessage.LocalID < 1 || groupMessage.LocalID > math.MaxInt32 {
		t.Fatalf("group message id %d does not fit receipt targets", groupMessage.LocalID)
	}
	f := messageDeletePendingFixture(t, b.ID)
	f.authKeyID = []byte{1, 0x18, 0, 0, 0, 0, 0, 0}
	f.targets = []int32{int32(groupMessage.LocalID)} // #nosec G115 -- bounded by the width check above.
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)

	removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, b.ID, a.ID)
	if err != nil || !removed {
		t.Fatalf("remove receipt owner from chat: removed=%v, err=%v", removed, err)
	}
	if err := s.BindAuthKeyUser(ctx, authKeyID, a.ID); err != nil {
		t.Fatalf("rebind auth key: %v", err)
	}
	if err := s.DeleteAuthKey(ctx, authKeyID); err != nil {
		t.Fatalf("delete auth key: %v", err)
	}

	// Drop this lookup result as if its response were lost before reconnecting.
	if _, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f),
		messageDeleteReceiptLookupRequest(f, f.targets)); err != nil {
		t.Fatalf("lookup after membership and auth-key changes: %v", err)
	}
	reconnected := openStore(t, dsn)
	got, err := reconnected.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f),
		messageDeleteReceiptLookupRequest(f, f.targets))
	if err != nil {
		t.Fatalf("lookup after simulated reconnect: %v", err)
	}
	result := messageDeleteReceiptLookupFound(t, got)
	if result.Pts != 7 || result.PtsCount != 2 || !bytes.Equal(result.ReceiptKey[:], f.receiptKey) {
		t.Fatalf("receipt changed across membership/auth-key/reconnect: %+v", result)
	}
}

func TestMessageDeleteReceiptLookupOutcomesAreReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	a := mustUser(t, s, "+15551795109")
	b := mustUser(t, s, "+15551795110")
	message := send(t, s, a, b, "lookup must not delete copies", 1811)
	if message.LocalID < 1 || message.LocalID > math.MaxInt32 {
		t.Fatalf("message id %d does not fit receipt targets", message.LocalID)
	}
	f := messageDeletePendingFixture(t, a.ID)
	f.targets = []int32{int32(message.LocalID)} // #nosec G115 -- bounded by the width check above.
	messageDeleteReceiptInsertForLookup(t, ctx, s, f)
	if _, err := store.StorePool(s).Exec(ctx,
		`INSERT INTO message_delete_capacity_refusals (owner_id, total) VALUES ($1, 17)`, a.ID); err != nil {
		t.Fatalf("seed refusal accounting: %v", err)
	}

	before := messageDeleteReceiptLookupSnapshot(t, ctx, s, a.ID, f)
	requests := []struct {
		identity store.MessageDeleteReceiptLookupIdentity
		request  store.MessageDeleteReceiptLookupRequest
		want     store.MessageDeleteReceiptLookupOutcome
	}{
		{messageDeleteReceiptLookupIdentity(f), messageDeleteReceiptLookupRequest(f, f.targets), store.MessageDeleteReceiptLookupFoundLocalCommittedResult},
		{messageDeleteReceiptLookupIdentity(f), store.MessageDeleteReceiptLookupRequest{Method: "channels.deleteMessages"}, store.MessageDeleteReceiptLookupRequestMismatch},
		{func() store.MessageDeleteReceiptLookupIdentity {
			i := messageDeleteReceiptLookupIdentity(f)
			i.MsgID++
			return i
		}(), store.MessageDeleteReceiptLookupRequest{Method: "channels.deleteMessages"}, store.MessageDeleteReceiptLookupMissing},
	}
	for _, tc := range requests {
		got, err := s.LookupMessageDeleteReceipt(ctx, tc.identity, tc.request)
		if err != nil || got.Outcome != tc.want {
			t.Fatalf("lookup outcome = %+v, err %v, want %s", got, err, tc.want)
		}
	}
	after := messageDeleteReceiptLookupSnapshot(t, ctx, s, a.ID, f)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("lookup changed local store state:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

type messageDeleteReceiptLookupStoreSnapshot struct {
	receipt      db.MessageDeleteReceipt
	receiptCount int64
	refusalTotal int64
	messageCount int64
	deletedCount int64
	pts          int64
	seq          int64
	nextLocalID  int64
	eventCount   int64
	outboxCount  int64
	counterCount int64
}

func messageDeleteReceiptLookupSnapshot(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	ownerID int64,
	f messageDeleteReceiptFixture,
) messageDeleteReceiptLookupStoreSnapshot {
	t.Helper()
	pool := store.StorePool(s)
	q := db.New(pool)
	identity := messageDeleteIdentity(f)
	receipt, err := q.MessageDeleteReceiptByOwnerIdentity(ctx, identity)
	if err != nil {
		t.Fatalf("snapshot receipt: %v", err)
	}
	var snapshot messageDeleteReceiptLookupStoreSnapshot
	snapshot.receipt = receipt
	if snapshot.receiptCount, err = q.MessageDeleteReceiptOwnerCount(ctx, ownerID); err != nil {
		t.Fatalf("snapshot receipt count: %v", err)
	}
	if snapshot.refusalTotal, err = q.MessageDeleteCapacityRefusalByOwner(ctx, ownerID); err != nil {
		t.Fatalf("snapshot refusal total: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE deleted) FROM messages`).Scan(
		&snapshot.messageCount, &snapshot.deletedCount); err != nil {
		t.Fatalf("snapshot message copies: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT pts, seq, next_local_id FROM update_state WHERE user_id=$1`, ownerID).Scan(
		&snapshot.pts, &snapshot.seq, &snapshot.nextLocalID); err != nil {
		t.Fatalf("snapshot owner update state: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM message_events`).Scan(&snapshot.eventCount); err != nil {
		t.Fatalf("snapshot message events: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM erasure_outbox`).Scan(&snapshot.outboxCount); err != nil {
		t.Fatalf("snapshot erasure outbox: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM erasure_stream_counter`).Scan(&snapshot.counterCount); err != nil {
		t.Fatalf("snapshot erasure stream counter: %v", err)
	}
	return snapshot
}

func TestMessageDeleteReceiptLookupReadErrorsAreNotAbsence(t *testing.T) {
	t.Parallel()
	s := open(t)
	owner := mustUser(t, s, "+15551795111")
	f := messageDeletePendingFixture(t, owner.ID)
	messageDeleteReceiptInsertForLookup(t, context.Background(), s, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := s.LookupMessageDeleteReceipt(ctx, messageDeleteReceiptLookupIdentity(f),
		messageDeleteReceiptLookupRequest(f, []int32{4, 9}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lookup with canceled context err = %v, want context.Canceled", err)
	}
	if got.Outcome == store.MessageDeleteReceiptLookupMissing || got.Result != nil {
		t.Fatalf("database read failure was converted to an outcome: %+v", got)
	}
}

func TestMessageDeleteReceiptLookupResultExposesOnlyLocalResultAndOpaqueReference(t *testing.T) {
	resultFields := []string{"Pts", "PtsCount", "ReceiptKey", "State", "CaptureReference"}
	resultType := reflect.TypeFor[store.MessageDeleteLocalCommittedResult]()
	if got := reflect.VisibleFields(resultType); len(got) != len(resultFields) {
		t.Fatalf("local result fields = %+v, want only %v", got, resultFields)
	} else {
		for i, field := range got {
			if field.Name != resultFields[i] {
				t.Fatalf("local result field %d = %s, want %s", i, field.Name, resultFields[i])
			}
		}
	}
	captureFields := []string{"OperationKey", "Epoch", "StreamID", "Position"}
	captureType := reflect.TypeFor[store.MessageDeleteReceiptCaptureReference]()
	if got := reflect.VisibleFields(captureType); len(got) != len(captureFields) {
		t.Fatalf("capture reference fields = %+v, want only %v", got, captureFields)
	} else {
		for i, field := range got {
			if field.Name != captureFields[i] {
				t.Fatalf("capture reference field %d = %s, want %s", i, field.Name, captureFields[i])
			}
		}
	}
	if resultType == reflect.TypeFor[db.MessageDeleteReceipt]() {
		t.Fatal("lookup result is the full persistence row")
	}
}
