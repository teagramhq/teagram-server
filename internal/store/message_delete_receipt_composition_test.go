package store_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

func messageDeleteReceiptOwnerCount(t *testing.T, ctx context.Context, s *store.Store, ownerID int64) int64 {
	t.Helper()
	var count int64
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM message_delete_receipts WHERE owner_id = $1`, ownerID).Scan(&count); err != nil {
		t.Fatalf("count owner receipts: %v", err)
	}
	return count
}

func messageDeleteReceiptRefusalTotal(t *testing.T, ctx context.Context, s *store.Store, ownerID int64) int64 {
	t.Helper()
	var total int64
	err := store.StorePool(s).QueryRow(ctx, `SELECT total FROM message_delete_capacity_refusals WHERE owner_id = $1`, ownerID).Scan(&total)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	if err != nil {
		t.Fatalf("read owner refusal total: %v", err)
	}
	return total
}

func seedMessageDeleteReceiptOwnerCount(t *testing.T, ctx context.Context, s *store.Store, ownerID int64, count int64) {
	t.Helper()
	if count < 0 || count > 65_536 {
		t.Fatalf("invalid receipt seed count %d", count)
	}
	if count == 0 {
		return
	}
	_, err := store.StorePool(s).Exec(ctx, `
		INSERT INTO message_delete_receipts (
			owner_id, auth_key_id, session_id, msg_id, method, id_space, revoke, target_ids,
			pts, pts_count, receipt_key, state, local_completed_at, retain_until
		)
		SELECT $1, decode('ff00010203040580','hex'), 9191, n,
			'messages.deleteMessages', 'owner_message', false, ARRAY[]::integer[],
			0, 0, decode(lpad(to_hex(n), 32, '0'), 'hex'), 'local_no_change',
			statement_timestamp(), statement_timestamp() + INTERVAL '8380800 seconds'
		FROM generate_series(1, $2::bigint) AS n`, ownerID, count)
	if err != nil {
		t.Fatalf("seed %d retained receipts: %v", count, err)
	}
}

func messageDeleteReceiptTestPts(t *testing.T, pts int) int32 {
	t.Helper()
	if pts < 0 || pts > math.MaxInt32 {
		t.Fatalf("test pts %d does not fit int32", pts)
	}
	return int32(pts) // #nosec G115 -- bounded to MaxInt32 above.
}

func messageDeleteCompositionCopyID(t *testing.T, ctx context.Context, s *store.Store, ownerID, fanoutID int64) int64 {
	t.Helper()
	var localID int64
	if err := store.StorePool(s).QueryRow(ctx, `SELECT local_id FROM messages WHERE owner_id = $1 AND fanout_id = $2`, ownerID, fanoutID).Scan(&localID); err != nil {
		t.Fatalf("load owner %d fanout %d copy id: %v", ownerID, fanoutID, err)
	}
	return localID
}

func TestMessageDeleteCompositionStoresChangedResultAndCaptureForRetry(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796001")
	b := mustUser(t, s, "+15551796002")
	chat := chatWith(t, s, a, b)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "composition result", RandomID: 7201})

	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000101
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID), messageDeleteLocalID(t, message.LocalID)},
	}
	epoch, stream := int64(21), messageDeleteCaptureStream(0x51)
	perOwner, composed, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil)
	if err != nil {
		t.Fatalf("compose delete: %v", err)
	}
	if perOwner[a.ID] == 0 {
		t.Fatalf("caller pts = %d, want changed caller copy", perOwner[a.ID])
	}

	result := messageDeleteReceiptLookupFound(t, composed)
	if result.Pts != messageDeleteReceiptTestPts(t, perOwner[a.ID]) || result.PtsCount != 2 || result.State != "pending" {
		t.Fatalf("saved result = %+v, want caller pts %d, raw count 2, pending", result, perOwner[a.ID])
	}
	if result.CaptureReference == nil || result.CaptureReference.Epoch != epoch || result.CaptureReference.StreamID != [16]byte(stream) {
		t.Fatalf("saved capture reference = %+v, want the committed stream reference", result.CaptureReference)
	}

	retryRequest := request
	retryRequest.TargetIDs = []int32{messageDeleteLocalID(t, message.LocalID)}
	perOwner, retry, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, retryRequest, epoch, stream, func() error {
		t.Fatal("matching retry reran capture")
		return nil
	})
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("matching retry = %+v, err %v, want saved result without new effects", perOwner, err)
	}
	retryResult := messageDeleteReceiptLookupFound(t, retry)
	if retryResult.Pts != result.Pts || retryResult.PtsCount != 2 || retryResult.ReceiptKey != result.ReceiptKey || retryResult.CaptureReference == nil || *retryResult.CaptureReference != *result.CaptureReference {
		t.Fatalf("matching retry result = %+v, want original %+v", retryResult, result)
	}
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 1 {
		t.Fatalf("matching retry left %d capture records, want exactly one", len(rows))
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != 1 {
		t.Fatalf("matching retry stream counter = %d, present %v, want original position 1", position, ok)
	}
}

func TestMessageDeleteCompositionSavesNoChangeAndEmptyResults(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796003")
	b := mustUser(t, s, "+15551796004")
	chat := chatWith(t, s, a, b)
	message, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "already cleared", RandomID: 7202})
	for _, ownerID := range []int64{a.ID, b.ID} {
		if _, err := s.DeleteMessages(ctx, ownerID, []int64{message.LocalID}, false); err != nil {
			t.Fatalf("clear owner %d copy: %v", ownerID, err)
		}
	}

	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000102
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}
	epoch, stream := int64(22), messageDeleteCaptureStream(0x52)
	perOwner, saved, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("all-no-change compose = %+v, err %v, want no changed owners", perOwner, err)
	}
	result := messageDeleteReceiptLookupFound(t, saved)
	if result.State != "local_no_change" || result.Pts != 0 || result.PtsCount != 1 || result.CaptureReference != nil || result.ReceiptKey == [16]byte{} {
		t.Fatalf("no-change result = %+v, want local 0/1 result, key and no capture", result)
	}
	duplicates := make([]int32, 101)
	for i := range duplicates {
		duplicates[i] = messageDeleteLocalID(t, message.LocalID)
	}
	request.TargetIDs = duplicates
	_, retry, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil)
	if err != nil {
		t.Fatalf("matching over-limit duplicate retry: %v", err)
	}
	if got := messageDeleteReceiptLookupFound(t, retry); got.Pts != 0 || got.PtsCount != 1 || got.ReceiptKey != result.ReceiptKey {
		t.Fatalf("over-limit retry = %+v, want original no-change result %+v", got, result)
	}
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("no-change compose wrote %d outbox rows, want none", len(rows))
	}
	if _, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); ok {
		t.Fatal("no-change compose allocated a stream position")
	}

	emptyIdentity := identity
	emptyIdentity.MsgID++
	emptyRequest := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message"}
	_, empty, err := store.ComposeDeleteMessagesForTest(ctx, s, emptyIdentity, emptyRequest, epoch, stream, nil)
	if err != nil {
		t.Fatalf("empty compose: %v", err)
	}
	if got := messageDeleteReceiptLookupFound(t, empty); got.State != "local_no_change" || got.Pts != 0 || got.PtsCount != 0 || got.CaptureReference != nil {
		t.Fatalf("empty result = %+v, want local 0/0 result without capture", got)
	}
}

func TestMessageDeleteCompositionFreshAdmissionRejectsMoreThanOneHundredRawIDs(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796005")
	b := mustUser(t, s, "+15551796006")
	message := send(t, s, a, b, "too many raw targets", 7203)
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000103
	targets := make([]int32, 101)
	for i := range targets {
		targets[i] = messageDeleteLocalID(t, message.LocalID)
	}
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: false, TargetIDs: targets,
	}
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	)
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 23, messageDeleteCaptureStream(0x53), nil); !errors.Is(err, store.ErrMessageDeleteReceiptTooManyTargets) {
		t.Fatalf("fresh 101-ID request = %v, want target-bound refusal", err)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	))
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 0 {
		t.Fatalf("fresh target-bound refusal wrote %d receipts", count)
	}
	if count := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); count != 0 {
		t.Fatalf("fresh target-bound refusal incremented capacity total to %d", count)
	}
}

func TestMessageDeleteCompositionMatchingWinnerTakesFinalCapacitySlot(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796007")
	b := mustUser(t, s, "+15551796008")
	message := send(t, s, a, b, "final slot", 7204)
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_535)
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000104
	request := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)},
	}

	var calls atomic.Int32
	entered := make(chan int32, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error {
		n := calls.Add(1)
		entered <- n
		if n == 1 {
			<-release
		}
		return nil
	})
	defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)

	type result struct {
		perOwner map[int64]int
		lookup   store.MessageDeleteReceiptLookup
		err      error
	}
	loserDone := make(chan result, 1)
	go func() {
		perOwner, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 24, messageDeleteCaptureStream(0x54), nil)
		loserDone <- result{perOwner: perOwner, lookup: lookup, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("first request did not observe initial Missing")
	}

	winnerPts, winner, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 24, messageDeleteCaptureStream(0x54), nil)
	if err != nil || len(winnerPts) == 0 {
		closeRelease()
		t.Fatalf("winner compose = %+v, %v", winnerPts, err)
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		closeRelease()
		t.Fatal("winner did not observe initial Missing")
	}
	closeRelease()
	loser := <-loserDone
	if loser.err != nil || len(loser.perOwner) != 0 {
		t.Fatalf("matching loser = %+v, err %v, want original result without writes", loser.perOwner, loser.err)
	}
	want := messageDeleteReceiptLookupFound(t, winner)
	got := messageDeleteReceiptLookupFound(t, loser.lookup)
	if got.ReceiptKey != want.ReceiptKey || got.Pts != want.Pts || got.PtsCount != 1 || got.CaptureReference == nil || want.CaptureReference == nil || *got.CaptureReference != *want.CaptureReference {
		t.Fatalf("loser result = %+v, want committed winner %+v", got, want)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 65_536 {
		t.Fatalf("retained receipt count = %d, want final capacity 65536", count)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
		t.Fatalf("matching loser incremented refusal total to %d", total)
	}
}

func TestMessageDeleteCompositionConflictingLockedLoserReturnsMismatch(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796009")
	b := mustUser(t, s, "+15551796010")
	first := send(t, s, a, b, "winner target", 7205)
	second := send(t, s, a, b, "loser target", 7206)
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_535)
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000105
	winnerRequest := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: false,
		TargetIDs: []int32{messageDeleteLocalID(t, first.LocalID)},
	}
	loserRequest := winnerRequest
	loserRequest.TargetIDs = []int32{messageDeleteLocalID(t, second.LocalID)}

	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	})
	defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)

	type result struct {
		lookup store.MessageDeleteReceiptLookup
		err    error
	}
	loserDone := make(chan result, 1)
	go func() {
		_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, loserRequest, 25, messageDeleteCaptureStream(0x55), nil)
		loserDone <- result{lookup: lookup, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("conflicting request did not observe initial Missing")
	}
	_, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, winnerRequest, 25, messageDeleteCaptureStream(0x55), nil)
	if err != nil {
		closeRelease()
		t.Fatalf("winner compose: %v", err)
	}
	closeRelease()
	loser := <-loserDone
	if loser.err != nil || loser.lookup.Outcome != store.MessageDeleteReceiptLookupRequestMismatch || loser.lookup.Result != nil {
		t.Fatalf("conflicting locked retry = %+v, err %v, want uniform mismatch", loser.lookup, loser.err)
	}
	if msgAt(t, s, a.ID, second.LocalID).Deleted {
		t.Fatal("conflicting loser deleted its different target")
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
		t.Fatalf("conflicting loser incremented refusal total to %d", total)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 65_536 {
		t.Fatalf("conflicting locked loser retained count = %d, want final capacity 65536", count)
	}
}

func TestMessageDeleteCompositionPostRollbackLookupOverridesPrelockRefusal(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		name := "matching winner"
		if conflicting {
			name = "conflicting winner"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			ctx := context.Background()
			a := mustUser(t, s, "+15551796011")
			b := mustUser(t, s, "+15551796012")
			chat := chatWith(t, s, a, b)
			first, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "winner target", RandomID: 7207})
			second, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "loser target", RandomID: 7208})
			firstA := messageDeleteCompositionCopyID(t, ctx, s, a.ID, first.FanoutID)
			firstB := messageDeleteCompositionCopyID(t, ctx, s, b.ID, first.FanoutID)
			secondA := messageDeleteCompositionCopyID(t, ctx, s, a.ID, second.FanoutID)
			secondB := messageDeleteCompositionCopyID(t, ctx, s, b.ID, second.FanoutID)
			fixture := messageDeletePendingFixture(t, a.ID)
			identity := messageDeleteReceiptLookupIdentity(fixture)
			identity.MsgID = 1700000000106
			winnerRequest := store.MessageDeleteReceiptLookupRequest{
				Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
				TargetIDs: []int32{messageDeleteLocalID(t, firstA)},
			}
			loserRequest := winnerRequest
			if conflicting {
				loserRequest.TargetIDs = []int32{messageDeleteLocalID(t, secondA)}
			}

			var calls atomic.Int32
			entered := make(chan struct{}, 1)
			resume := make(chan struct{})
			var resumeOnce sync.Once
			closeResume := func() { resumeOnce.Do(func() { close(resume) }) }
			defer closeResume()
			store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error {
				if calls.Add(1) == 1 {
					close(entered)
					<-resume
				}
				return nil
			})
			defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)

			type result struct {
				lookup store.MessageDeleteReceiptLookup
				err    error
			}
			loserDone := make(chan result, 1)
			go func() {
				_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, loserRequest, 26, messageDeleteCaptureStream(0x56), nil)
				loserDone <- result{lookup: lookup, err: err}
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("loser did not reach the pre-resolution boundary")
			}
			_, winner, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, winnerRequest, 26, messageDeleteCaptureStream(0x56), nil)
			if err != nil {
				closeResume()
				t.Fatalf("winner compose: %v", err)
			}
			refusalTarget := firstA
			if conflicting {
				refusalTarget = secondA
			}
			if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages SET action_type = 1 WHERE owner_id = $1 AND local_id = $2`, a.ID, refusalTarget); err != nil {
				closeResume()
				t.Fatalf("stage pre-lock target refusal: %v", err)
			}
			stateAfterFirst := snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, firstA},
				struct{ ownerID, localID int64 }{b.ID, firstB},
			)
			stateAfterSecond := snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, secondA},
				struct{ ownerID, localID int64 }{b.ID, secondB},
			)
			closeResume()
			loser := <-loserDone
			if loser.err != nil {
				t.Fatalf("post-rollback lookup: %v", loser.err)
			}
			if conflicting {
				if loser.lookup.Outcome != store.MessageDeleteReceiptLookupRequestMismatch || loser.lookup.Result != nil {
					t.Fatalf("post-refusal conflict = %+v, want mismatch", loser.lookup)
				}
			} else {
				want := messageDeleteReceiptLookupFound(t, winner)
				got := messageDeleteReceiptLookupFound(t, loser.lookup)
				if got.ReceiptKey != want.ReceiptKey || got.Pts != want.Pts || got.PtsCount != want.PtsCount {
					t.Fatalf("post-refusal match = %+v, want original %+v", got, want)
				}
			}
			assertMessageDeleteStateUnchanged(t, stateAfterFirst, snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, firstA},
				struct{ ownerID, localID int64 }{b.ID, firstB},
			))
			assertMessageDeleteStateUnchanged(t, stateAfterSecond, snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, secondA},
				struct{ ownerID, localID int64 }{b.ID, secondB},
			))
			if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages SET action_type = 0 WHERE owner_id = $1 AND local_id = $2`, a.ID, refusalTarget); err != nil {
				t.Fatalf("restore target fixture: %v", err)
			}
		})
	}
}

func TestMessageDeleteCompositionStillMissingKeepsPrelockRefusal(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796017")
	b := mustUser(t, s, "+15551796018")
	message := send(t, s, a, b, "still missing", 7212)
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000111
	request := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)}}
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	)
	store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error { return store.ErrMessageInvalid })
	defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)
	_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 29, messageDeleteCaptureStream(0x5a), nil)
	if !errors.Is(err, store.ErrMessageInvalid) || lookup.Outcome != "" || lookup.Result != nil {
		t.Fatalf("still-missing refusal = %+v, %v, want original ErrMessageInvalid", lookup, err)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	))
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 0 {
		t.Fatalf("still-missing refusal wrote %d receipts", count)
	}
}

func TestMessageDeleteCompositionUniqueIdentityLoserRollsBackCaptureAndMutations(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796019")
	b := mustUser(t, s, "+15551796020")
	message := send(t, s, a, b, "identity conflict rollback", 7213)
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000112
	request := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true, TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)}}
	fixture.authKeyID = append([]byte(nil), identity.AuthKeyID[:]...)
	fixture.sessionID, fixture.msgID = identity.SessionID, identity.MsgID
	fixture.method, fixture.idSpace, fixture.revoke = request.Method, request.IDSpace, request.Revoke
	fixture.targets = []int32{messageDeleteLocalID(t, message.LocalID)}
	fixture.pts, fixture.ptsCount = 31, 1

	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	)
	store.SetMessageDeleteReceiptAfterLockedMissHook(s, func() {
		if _, err := db.New(store.StorePool(s)).InsertMessageDeletePendingReceipt(ctx, messageDeletePendingParams(fixture)); err != nil {
			t.Errorf("insert concurrent identity winner: %v", err)
		}
	})
	defer store.SetMessageDeleteReceiptAfterLockedMissHook(s, nil)
	epoch, stream := int64(30), messageDeleteCaptureStream(0x5b)
	perOwner, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, nil)
	if err != nil || len(perOwner) != 0 {
		t.Fatalf("unique identity loser = %+v, %v", perOwner, err)
	}
	result := messageDeleteReceiptLookupFound(t, lookup)
	if result.Pts != fixture.pts || result.PtsCount != fixture.ptsCount || result.ReceiptKey == [16]byte{} {
		t.Fatalf("unique-identity winner result = %+v, want original %d/%d", result, fixture.pts, fixture.ptsCount)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("unique-identity rollback left %d outbox rows", len(rows))
	}
	if _, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); ok {
		t.Fatal("unique-identity rollback left a stream counter")
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
		t.Fatalf("unique-identity loser incremented refusal total to %d", total)
	}
}

func TestMessageDeleteCompositionCaptureAndReceiptFailuresRollBackAllState(t *testing.T) {
	for _, failureMode := range []string{"capture", "allocator", "receipt insert"} {
		t.Run(failureMode, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			ctx := context.Background()
			a := mustUser(t, s, "+15551796024")
			b := mustUser(t, s, "+15551796025")
			message := send(t, s, a, b, "atomic composition failure", 7216)
			fixture := messageDeletePendingFixture(t, a.ID)
			identity := messageDeleteReceiptLookupIdentity(fixture)
			identity.MsgID++
			request := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true, TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)}}
			epoch, stream := int64(32), messageDeleteCaptureStream(0x5e)
			if failureMode == "allocator" {
				if _, err := store.StorePool(s).Exec(ctx, `INSERT INTO erasure_stream_counter (epoch, stream_id, last_position) VALUES ($1, $2, $3)`, epoch, stream[:], int64(math.MaxInt64)); err != nil {
					t.Fatalf("seed exhausted stream counter: %v", err)
				}
			}
			if failureMode == "receipt insert" {
				if _, err := store.StorePool(s).Exec(ctx, `CREATE FUNCTION reject_message_delete_receipt_insert() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced receipt insert failure'; END $$`); err != nil {
					t.Fatalf("install receipt insert function: %v", err)
				}
				if _, err := store.StorePool(s).Exec(ctx, `CREATE TRIGGER reject_message_delete_receipt_insert BEFORE INSERT ON message_delete_receipts FOR EACH ROW EXECUTE FUNCTION reject_message_delete_receipt_insert()`); err != nil {
					t.Fatalf("install receipt insert trigger: %v", err)
				}
			}
			before := snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, message.LocalID},
				struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
			)
			afterWrite := func() error { return nil }
			if failureMode == "capture" {
				afterWrite = func() error { return errors.New("forced capture failure") }
			}
			_, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, epoch, stream, afterWrite)
			if err == nil {
				t.Fatal("failed composition succeeded")
			}
			if failureMode == "capture" && !strings.Contains(err.Error(), "forced capture failure") {
				t.Fatalf("capture failure = %v, want injected cause", err)
			}
			assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
				struct{ ownerID, localID int64 }{a.ID, message.LocalID},
				struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
			))
			if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 0 {
				t.Fatalf("%s failure retained %d receipts", failureMode, count)
			}
			if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
				t.Fatalf("%s failure incremented refusal total to %d", failureMode, total)
			}
			if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
				t.Fatalf("%s failure retained %d outbox rows", failureMode, len(rows))
			}
			position, counterExists := messageDeleteCaptureCounter(t, ctx, s, epoch, stream)
			if failureMode == "allocator" {
				if !counterExists || position != math.MaxInt64 {
					t.Fatalf("exhausted counter = %d, present %v, want unchanged MaxInt64", position, counterExists)
				}
			} else if counterExists {
				t.Fatalf("%s failure retained stream position %d", failureMode, position)
			}
		})
	}
}

func TestMessageDeleteCompositionFailedCapacityCommitLeavesNoRefusal(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796021")
	b := mustUser(t, s, "+15551796022")
	message := send(t, s, a, b, "failed refusal commit", 7214)
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_536)
	if _, err := store.StorePool(s).Exec(ctx, `CREATE FUNCTION reject_message_delete_refusal_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'forced refusal commit failure'; END $$`); err != nil {
		t.Fatalf("install deferred refusal failure: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `CREATE CONSTRAINT TRIGGER reject_message_delete_refusal_commit AFTER INSERT OR UPDATE ON message_delete_capacity_refusals DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_message_delete_refusal_commit()`); err != nil {
		t.Fatalf("install deferred refusal trigger: %v", err)
	}
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000113
	request := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true, TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)}}
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	)
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 31, messageDeleteCaptureStream(0x5c), nil); err == nil || errors.Is(err, store.ErrMessageDeleteReceiptCapacityExceeded) {
		t.Fatalf("failed refusal commit = %v, want database commit error", err)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 0 {
		t.Fatalf("failed refusal commit persisted total %d", total)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 65_536 {
		t.Fatalf("failed refusal commit changed receipt count to %d", count)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	))
}

func TestMessageDeleteCompositionFreshIdentitiesShareOnlyOneFinalSlot(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796013")
	b := mustUser(t, s, "+15551796014")
	first := send(t, s, a, b, "first fresh identity", 7209)
	second := send(t, s, a, b, "second fresh identity", 7210)
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_535)
	fixture1 := messageDeletePendingFixture(t, a.ID)
	fixture2 := messageDeletePendingFixture(t, a.ID)
	identity1 := messageDeleteReceiptLookupIdentity(fixture1)
	identity2 := messageDeleteReceiptLookupIdentity(fixture2)
	identity1.MsgID = 1700000000107
	identity2.MsgID = 1700000000108
	request1 := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", TargetIDs: []int32{messageDeleteLocalID(t, first.LocalID)}}
	request2 := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", TargetIDs: []int32{messageDeleteLocalID(t, second.LocalID)}}

	var calls atomic.Int32
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	closeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	defer closeRelease()
	store.SetMessageDeleteReceiptBeforeResolutionHook(s, func() error {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release
			return nil
		}
		entered <- struct{}{}
		return nil
	})
	defer store.SetMessageDeleteReceiptBeforeResolutionHook(s, nil)

	type result struct {
		lookup store.MessageDeleteReceiptLookup
		err    error
	}
	start := make(chan struct{})
	done := make(chan result, 2)
	go func() {
		<-start
		_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity1, request1, 27, messageDeleteCaptureStream(0x57), nil)
		done <- result{lookup: lookup, err: err}
	}()
	go func() {
		<-start
		_, lookup, err := store.ComposeDeleteMessagesForTest(ctx, s, identity2, request2, 27, messageDeleteCaptureStream(0x57), nil)
		done <- result{lookup: lookup, err: err}
	}()
	close(start)
	for range 2 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			closeRelease()
			t.Fatal("both fresh identities did not observe Missing")
		}
	}
	closeRelease()
	firstResult, secondResult := <-done, <-done
	successes, capacity := 0, 0
	for _, result := range []result{firstResult, secondResult} {
		switch {
		case errors.Is(result.err, store.ErrMessageDeleteReceiptCapacityExceeded):
			capacity++
		case result.err == nil && result.lookup.Outcome == store.MessageDeleteReceiptLookupFoundLocalCommittedResult:
			successes++
		default:
			t.Fatalf("fresh identity result = %+v, want one commit and one capacity refusal", result)
		}
	}
	if successes != 1 || capacity != 1 {
		t.Fatalf("final-slot outcomes = success %d, capacity %d, want 1/1", successes, capacity)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 65_536 {
		t.Fatalf("retained receipt count = %d, want 65536", count)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != 1 {
		t.Fatalf("fresh final-slot loser refusal total = %d, want 1", total)
	}
}

func TestMessageDeleteCompositionCapacityRefusalSaturatesAndIsOwnerScoped(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551796015")
	b := mustUser(t, s, "+15551796016")
	message := send(t, s, a, b, "capacity refusal", 7211)
	seedMessageDeleteReceiptOwnerCount(t, ctx, s, a.ID, 65_536)
	if _, err := store.StorePool(s).Exec(ctx, `INSERT INTO message_delete_capacity_refusals (owner_id, total) VALUES ($1, $2)`, a.ID, int64(math.MaxInt64)); err != nil {
		t.Fatalf("seed saturated refusal total: %v", err)
	}
	fixture := messageDeletePendingFixture(t, a.ID)
	identity := messageDeleteReceiptLookupIdentity(fixture)
	identity.MsgID = 1700000000109
	request := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true, TargetIDs: []int32{messageDeleteLocalID(t, message.LocalID)}}
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	)
	duplicateRequest := request
	duplicateRequest.TargetIDs = []int32{messageDeleteLocalID(t, message.LocalID), messageDeleteLocalID(t, message.LocalID)}
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, duplicateRequest, 28, messageDeleteCaptureStream(0x58), nil); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("duplicate peer delete at capacity = %v, want existing invalid-target refusal", err)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != math.MaxInt64 {
		t.Fatalf("duplicate-target refusal changed saturated total to %d", total)
	}
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, identity, request, 28, messageDeleteCaptureStream(0x58), nil); !errors.Is(err, store.ErrMessageDeleteReceiptCapacityExceeded) {
		t.Fatalf("owner at C compose = %v, want capacity refusal", err)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != math.MaxInt64 {
		t.Fatalf("saturated refusal total = %d, want MaxInt64", total)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, a.ID); count != 65_536 {
		t.Fatalf("refusal changed retained receipt count to %d", count)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, message.LocalID},
		struct{ ownerID, localID int64 }{b.ID, message.PeerLocalID},
	))

	// The saturated owner does not consume another owner's receipt capacity.
	otherFixture := messageDeletePendingFixture(t, b.ID)
	otherIdentity := messageDeleteReceiptLookupIdentity(otherFixture)
	otherIdentity.MsgID = 1700000000110
	otherRequest := store.MessageDeleteReceiptLookupRequest{Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: false, TargetIDs: []int32{messageDeleteLocalID(t, message.PeerLocalID)}}
	_, admitted, err := store.ComposeDeleteMessagesForTest(ctx, s, otherIdentity, otherRequest, 28, messageDeleteCaptureStream(0x59), nil)
	if err != nil || admitted.Outcome != store.MessageDeleteReceiptLookupFoundLocalCommittedResult {
		t.Fatalf("independent owner admission = %+v, %v", admitted, err)
	}
	if count := messageDeleteReceiptOwnerCount(t, ctx, s, b.ID); count != 1 {
		t.Fatalf("independent owner receipt count = %d, want 1", count)
	}

	c := mustUser(t, s, "+15551796023")
	chat := chatWith(t, s, c, a, b)
	group, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: c.ID, Text: "removed member authorization", RandomID: 7215})
	var aGroupLocalID int64
	if err := store.StorePool(s).QueryRow(ctx, `SELECT local_id FROM messages WHERE owner_id = $1 AND fanout_id = $2`, a.ID, group.FanoutID).Scan(&aGroupLocalID); err != nil {
		t.Fatalf("load removed member copy id: %v", err)
	}
	if err := store.RemoveChatParticipant(ctx, s, chat.ID, a.ID); err != nil {
		t.Fatalf("remove saturated owner from chat: %v", err)
	}
	unauthorizedFixture := messageDeletePendingFixture(t, a.ID)
	unauthorizedIdentity := messageDeleteReceiptLookupIdentity(unauthorizedFixture)
	unauthorizedIdentity.MsgID = 1700000000114
	unauthorizedRequest := store.MessageDeleteReceiptLookupRequest{
		Method: "messages.deleteMessages", IDSpace: "owner_message", Revoke: true,
		TargetIDs: []int32{messageDeleteLocalID(t, aGroupLocalID)},
	}
	if _, _, err := store.ComposeDeleteMessagesForTest(ctx, s, unauthorizedIdentity, unauthorizedRequest, 28, messageDeleteCaptureStream(0x5d), nil); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("removed-member request at capacity = %v, want original authorization refusal", err)
	}
	if total := messageDeleteReceiptRefusalTotal(t, ctx, s, a.ID); total != math.MaxInt64 {
		t.Fatalf("authorization refusal changed saturated total to %d", total)
	}
}
