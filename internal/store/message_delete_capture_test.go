package store_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/store"
)

type messageDeleteLedgerRow struct {
	operationKey []byte
	epoch        int64
	stream       []byte
	position     int64
	kind         int16
	record       []byte
}

func messageDeleteCaptureStream(seed byte) erasureledger.StreamID {
	var stream erasureledger.StreamID
	for i := range stream {
		stream[i] = seed
	}
	return stream
}

func messageDeleteLocalID(t *testing.T, localID int64) int32 {
	t.Helper()
	if localID < 1 || localID > math.MaxInt32 {
		t.Fatalf("local id %d is outside the ledger codec width", localID)
	}
	return int32(localID) // #nosec G115 -- bounded to MaxInt32 above.
}

func messageDeleteCaptureRows(t *testing.T, ctx context.Context, s *store.Store, epoch int64, stream erasureledger.StreamID) []messageDeleteLedgerRow {
	t.Helper()
	rows, err := store.StorePool(s).Query(ctx, `
		SELECT operation_key, epoch, stream_id, seq, kind, record
		FROM erasure_outbox WHERE epoch = $1 AND stream_id = $2 ORDER BY seq`, epoch, stream[:])
	if err != nil {
		t.Fatalf("read captured message deletes: %v", err)
	}
	defer rows.Close()

	var got []messageDeleteLedgerRow
	for rows.Next() {
		var row messageDeleteLedgerRow
		if err := rows.Scan(&row.operationKey, &row.epoch, &row.stream, &row.position, &row.kind, &row.record); err != nil {
			t.Fatalf("scan captured message delete: %v", err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read captured message deletes: %v", err)
	}
	return got
}

func messageDeleteCaptureCounter(t *testing.T, ctx context.Context, s *store.Store, epoch int64, stream erasureledger.StreamID) (int64, bool) {
	t.Helper()
	var position int64
	err := store.StorePool(s).QueryRow(ctx, `
		SELECT last_position FROM erasure_stream_counter WHERE epoch = $1 AND stream_id = $2`, epoch, stream[:]).Scan(&position)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false
	}
	if err != nil {
		t.Fatalf("read stream counter: %v", err)
	}
	return position, true
}

func messageDeleteCaptureCounterCount(t *testing.T, ctx context.Context, s *store.Store) int64 {
	t.Helper()
	var count int64
	if err := store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM erasure_stream_counter`).Scan(&count); err != nil {
		t.Fatalf("count stream counters: %v", err)
	}
	return count
}

func messageDeleteCaptureDecoded(t *testing.T, rows []messageDeleteLedgerRow, wantCopies []erasureledger.MessageCopy) []erasureledger.Record {
	t.Helper()
	if len(rows) == 0 {
		t.Fatal("capture wrote no outbox record")
	}
	decoded := make([]erasureledger.Record, 0, len(rows))
	for i, row := range rows {
		if len(row.operationKey) != erasureledger.OperationKeyLen || bytes.Equal(row.operationKey, make([]byte, erasureledger.OperationKeyLen)) {
			t.Errorf("record %d operation key is not opaque non-zero 128-bit data: %x", i, row.operationKey)
		}
		record, err := erasureledger.Decode(row.record)
		if err != nil {
			t.Fatalf("decode record %d: %v", i, err)
		}
		if record.Kind != erasureledger.KindMessageCopies || record.Epoch != row.epoch || !bytes.Equal(record.Stream[:], row.stream) || record.Seq != row.position || !bytes.Equal(record.OpKey[:], row.operationKey) {
			t.Errorf("record %d envelope does not match its columns: %+v / %+v", i, record, row)
		}
		if row.kind != int16(erasureledger.KindMessageCopies) {
			t.Errorf("record %d kind column = %d, want message copies", i, row.kind)
		}
		payload, ok := record.Payload.(erasureledger.MessageCopies)
		if !ok {
			t.Fatalf("record %d payload has type %T, want MessageCopies", i, record.Payload)
		}
		if wantCopies != nil && !reflect.DeepEqual(payload.Copies, wantCopies) {
			t.Errorf("record %d copies = %+v, want %+v", i, payload.Copies, wantCopies)
		}
		decoded = append(decoded, record)
	}
	return decoded
}

type messageDeleteVisibleState struct {
	message store.Message
	found   bool
	pts     int
	events  []store.Event
}

func snapshotMessageDeleteState(t *testing.T, s *store.Store, owners ...struct{ ownerID, localID int64 }) map[int64]messageDeleteVisibleState {
	t.Helper()
	got := make(map[int64]messageDeleteVisibleState, len(owners))
	for _, key := range owners {
		message, found := msgOpt(t, s, key.ownerID, key.localID)
		got[key.ownerID] = messageDeleteVisibleState{
			message: message,
			found:   found,
			pts:     ptsOf(t, s, key.ownerID),
			events:  eventsOf(t, s, key.ownerID, 0),
		}
	}
	return got
}

func assertMessageDeleteStateUnchanged(t *testing.T, before, after map[int64]messageDeleteVisibleState) {
	t.Helper()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("delete changed messages, pts, or events:\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestDeleteMessageCaptureUsesOnlyChangedCurrentChatCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454001")
	b := mustUser(t, s, "+15551454002")
	c := mustUser(t, s, "+15551454003")
	chat := chatWith(t, s, a, b, c)
	sender, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "group capture body", RandomID: 7101})
	aCopy, _ := msgOpt(t, s, a.ID, sender.LocalID)
	bCopy, _ := msgOpt(t, s, b.ID, sender.LocalID)
	cCopy, _ := msgOpt(t, s, c.ID, sender.LocalID)

	// B clears the local copy first; C then leaves and keeps its frozen copy.
	if _, err := s.DeleteMessages(ctx, b.ID, []int64{bCopy.LocalID}, false); err != nil {
		t.Fatalf("B self-delete: %v", err)
	}
	if err := store.RemoveChatParticipant(ctx, s, chat.ID, c.ID); err != nil {
		t.Fatalf("remove C: %v", err)
	}
	untouchedBefore := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{b.ID, bCopy.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	)
	epoch, stream := int64(7), messageDeleteCaptureStream(0x31)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{aCopy.LocalID}, true, epoch, stream, nil); err != nil {
		t.Fatalf("capture group revoke: %v", err)
	}

	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	messageDeleteCaptureDecoded(t, rows, []erasureledger.MessageCopy{{OwnerID: a.ID, LocalID: messageDeleteLocalID(t, aCopy.LocalID)}})
	if len(rows) != 1 || rows[0].position != 1 {
		t.Fatalf("captured rows = %+v, want one record at position 1", rows)
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != 1 {
		t.Fatalf("stream counter = %d, present %v, want 1", position, ok)
	}
	untouchedAfter := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{b.ID, bCopy.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	)
	assertMessageDeleteStateUnchanged(t, untouchedBefore, untouchedAfter)
	if !msgAt(t, s, a.ID, aCopy.LocalID).Deleted {
		t.Fatal("creator's changed copy was not deleted")
	}
	if !msgAt(t, s, b.ID, bCopy.LocalID).Deleted {
		t.Fatal("B's earlier self-deleted copy was restored")
	}
	if msgAt(t, s, c.ID, cCopy.LocalID).Deleted {
		t.Fatal("C's frozen copy was deleted after leaving")
	}
}

func TestDeleteMessageCaptureNoChangeConsumesNoPosition(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454101")
	b := mustUser(t, s, "+15551454102")
	chat := chatWith(t, s, a, b)
	sender, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: a.ID, Text: "already gone", RandomID: 7102})
	if _, err := s.DeleteMessages(ctx, a.ID, []int64{sender.LocalID}, false); err != nil {
		t.Fatalf("A self-delete: %v", err)
	}
	bCopy, _ := msgOpt(t, s, b.ID, sender.LocalID)
	if _, err := s.DeleteMessages(ctx, b.ID, []int64{bCopy.LocalID}, false); err != nil {
		t.Fatalf("B self-delete: %v", err)
	}

	epoch, stream := int64(8), messageDeleteCaptureStream(0x32)
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, bCopy.LocalID},
	)
	perOwner, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{sender.LocalID}, true, epoch, stream, nil)
	if err != nil {
		t.Fatalf("all-deleted group revoke: %v", err)
	}
	if len(perOwner) != 0 {
		t.Fatalf("all-deleted group revoke pts = %+v, want no changes", perOwner)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, bCopy.LocalID},
	))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("all-deleted group revoke captured %d records, want none", len(rows))
	}
	if got := messageDeleteCaptureCounterCount(t, ctx, s); got != 0 {
		t.Fatalf("all-deleted group revoke left %d stream counters, want none", got)
	}
}

func TestDeleteMessageCaptureSelfAndPeerRevokeCopies(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454201")
	b := mustUser(t, s, "+15551454202")
	epoch, stream := int64(9), messageDeleteCaptureStream(0x33)

	self := send(t, s, a, b, "self-only private body", 7103)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{self.LocalID}, false, epoch, stream, nil); err != nil {
		t.Fatalf("capture self-delete: %v", err)
	}
	first := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	messageDeleteCaptureDecoded(t, first, []erasureledger.MessageCopy{{OwnerID: a.ID, LocalID: messageDeleteLocalID(t, self.LocalID)}})
	if bytes.Contains(first[0].record, []byte("self-only private body")) {
		t.Fatal("captured record contains message content")
	}
	if msgAt(t, s, b.ID, self.PeerLocalID).Deleted {
		t.Fatal("self-delete changed the peer copy")
	}

	peer := send(t, s, b, a, "peer revoke private body", 7104)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, b.ID, []int64{peer.LocalID}, true, epoch, stream, nil); err != nil {
		t.Fatalf("capture peer revoke: %v", err)
	}
	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	decoded := messageDeleteCaptureDecoded(t, rows, nil)
	if len(rows) != 2 || rows[0].position != 1 || rows[1].position != 2 {
		t.Fatalf("self and peer revoke positions = %+v, want contiguous 1,2", rows)
	}
	peerCopies, ok := decoded[1].Payload.(erasureledger.MessageCopies)
	if !ok {
		t.Fatalf("peer revoke payload has type %T", decoded[1].Payload)
	}
	wantPeerCopies := []erasureledger.MessageCopy{
		{OwnerID: a.ID, LocalID: messageDeleteLocalID(t, peer.PeerLocalID)},
		{OwnerID: b.ID, LocalID: messageDeleteLocalID(t, peer.LocalID)},
	}
	if !reflect.DeepEqual(peerCopies.Copies, wantPeerCopies) {
		t.Fatalf("peer revoke copies = %+v, want %+v", peerCopies.Copies, wantPeerCopies)
	}
	if bytes.Contains(rows[1].record, []byte("peer revoke private body")) {
		t.Fatal("captured peer revoke record contains message content")
	}
}

func TestDeleteMessageCaptureRejectsUnauthorizedCallersWithoutMutation(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454301")
	b := mustUser(t, s, "+15551454302")
	c := mustUser(t, s, "+15551454303")
	chat := chatWith(t, s, a, b, c)
	sender, _ := sendChat(t, s, store.FanOut{ChatID: chat.ID, FromID: b.ID, Text: "authority", RandomID: 7105})
	cCopy, _ := msgOpt(t, s, c.ID, sender.LocalID)

	epoch, stream := int64(10), messageDeleteCaptureStream(0x34)
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, c.ID, []int64{cCopy.LocalID}, true, epoch, stream, nil); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("non-author member revoke = %v, want ErrMessageInvalid", err)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	))
	if err := store.RemoveChatParticipant(ctx, s, chat.ID, b.ID); err != nil {
		t.Fatalf("remove message author: %v", err)
	}
	beforeRemovedAttempt := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, b.ID, []int64{sender.LocalID}, true, epoch, stream, nil); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("removed author revoke = %v, want ErrMessageInvalid", err)
	}
	assertMessageDeleteStateUnchanged(t, beforeRemovedAttempt, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{c.ID, cCopy.LocalID},
	))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("unauthorized calls captured %d records, want none", len(rows))
	}
	if got := messageDeleteCaptureCounterCount(t, ctx, s); got != 0 {
		t.Fatalf("unauthorized calls left %d stream counters, want none", got)
	}
}

func TestDeleteMessageCaptureForcedRollbackRestoresAllState(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454401")
	b := mustUser(t, s, "+15551454402")
	sender := send(t, s, a, b, "rollback", 7106)
	epoch, stream := int64(11), messageDeleteCaptureStream(0x35)
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.PeerLocalID},
	)
	rollback := errors.New("forced capture rollback")
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{sender.LocalID}, true, epoch, stream, func() error { return rollback }); !errors.Is(err, rollback) {
		t.Fatalf("capture with forced rollback = %v, want forced error", err)
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.PeerLocalID},
	))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("forced rollback left %d outbox records, want none", len(rows))
	}
	if got := messageDeleteCaptureCounterCount(t, ctx, s); got != 0 {
		t.Fatalf("forced rollback left %d stream counters, want none", got)
	}
}

func TestDeleteMessageCaptureAllocatorExhaustionRollsBack(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454501")
	b := mustUser(t, s, "+15551454502")
	sender := send(t, s, a, b, "exhaustion", 7107)
	epoch, stream := int64(12), messageDeleteCaptureStream(0x36)
	if _, err := store.StorePool(s).Exec(ctx, `
		INSERT INTO erasure_stream_counter (epoch, stream_id, last_position)
		VALUES ($1, $2, $3)`, epoch, stream[:], int64(math.MaxInt64)); err != nil {
		t.Fatalf("seed exhausted stream counter: %v", err)
	}
	before := snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.PeerLocalID},
	)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{sender.LocalID}, true, epoch, stream, nil); err == nil {
		t.Fatal("capture at MaxInt64 succeeded, want allocator exhaustion")
	}
	assertMessageDeleteStateUnchanged(t, before, snapshotMessageDeleteState(t, s,
		struct{ ownerID, localID int64 }{a.ID, sender.LocalID},
		struct{ ownerID, localID int64 }{b.ID, sender.PeerLocalID},
	))
	if rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream); len(rows) != 0 {
		t.Fatalf("allocator exhaustion left %d outbox records, want none", len(rows))
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != math.MaxInt64 {
		t.Fatalf("exhausted stream counter = %d, present %v, want unchanged MaxInt64", position, ok)
	}
}

func TestDeleteMessageCaptureConcurrentWritersKeepContiguousPositionsAcrossRollback(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	firstOwner := mustUser(t, s, "+15551454601")
	firstPeer := mustUser(t, s, "+15551454602")
	rollbackOwner := mustUser(t, s, "+15551454603")
	rollbackPeer := mustUser(t, s, "+15551454604")
	secondOwner := mustUser(t, s, "+15551454605")
	secondPeer := mustUser(t, s, "+15551454606")
	first := send(t, s, firstOwner, firstPeer, "first", 7108)
	rolledBack := send(t, s, rollbackOwner, rollbackPeer, "rolled back", 7109)
	second := send(t, s, secondOwner, secondPeer, "second", 7110)
	epoch, stream := int64(13), messageDeleteCaptureStream(0x37)

	firstAllocated, releaseFirst := make(chan struct{}), make(chan struct{})
	rollbackAllocated, releaseRollback := make(chan struct{}), make(chan struct{})
	var releaseFirstOnce, releaseRollbackOnce sync.Once
	closeFirst := func() { releaseFirstOnce.Do(func() { close(releaseFirst) }) }
	closeRollback := func() { releaseRollbackOnce.Do(func() { close(releaseRollback) }) }
	t.Cleanup(func() { closeFirst(); closeRollback() })
	type captureResult struct {
		ownerID int64
		err     error
	}
	done := make(chan captureResult, 3)
	go func() {
		_, err := store.CaptureDeleteMessagesForTest(ctx, s, firstOwner.ID, []int64{first.LocalID}, true, epoch, stream, func() error {
			close(firstAllocated)
			<-releaseFirst
			return nil
		})
		done <- captureResult{ownerID: firstOwner.ID, err: err}
	}()
	select {
	case <-firstAllocated:
	case <-time.After(10 * time.Second):
		t.Fatal("first writer did not reach the allocated position")
	}

	forced := errors.New("intervening rollback")
	go func() {
		_, err := store.CaptureDeleteMessagesForTest(ctx, s, rollbackOwner.ID, []int64{rolledBack.LocalID}, true, epoch, stream, func() error {
			close(rollbackAllocated)
			<-releaseRollback
			return forced
		})
		done <- captureResult{ownerID: rollbackOwner.ID, err: err}
	}()
	if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
		t.Fatalf("rollback writer did not queue on the stream counter: %v", err)
	}
	go func() {
		_, err := store.CaptureDeleteMessagesForTest(ctx, s, secondOwner.ID, []int64{second.LocalID}, true, epoch, stream, nil)
		done <- captureResult{ownerID: secondOwner.ID, err: err}
	}()
	if err := store.WaitForLockWaiters(ctx, s, 2); err != nil {
		t.Fatalf("second writer did not queue behind the rollback writer: %v", err)
	}
	closeFirst()
	select {
	case <-rollbackAllocated:
	case <-time.After(10 * time.Second):
		t.Fatal("rollback writer did not acquire the released stream counter")
	}
	closeRollback()

	results := make(map[int64]error, 3)
	for range 3 {
		select {
		case result := <-done:
			results[result.ownerID] = result.err
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent capture writer did not finish")
		}
	}
	if results[firstOwner.ID] != nil || results[secondOwner.ID] != nil || !errors.Is(results[rollbackOwner.ID], forced) {
		t.Fatalf("capture results = %+v, want first/second success and the forced rollback", results)
	}
	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	if len(rows) != 2 || rows[0].position != 1 || rows[1].position != 2 {
		t.Fatalf("committed positions = %+v, want contiguous 1,2 around the rollback", rows)
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, stream); !ok || position != 2 {
		t.Fatalf("stream counter = %d, present %v, want 2", position, ok)
	}
}

func TestDeleteMessageCaptureIndependentStreamsProgressIndependently(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454701")
	b := mustUser(t, s, "+15551454702")
	c := mustUser(t, s, "+15551454703")
	d := mustUser(t, s, "+15551454704")
	first := send(t, s, a, b, "first stream", 7111)
	second := send(t, s, c, d, "second stream", 7112)
	epoch := int64(14)
	streamA, streamB := messageDeleteCaptureStream(0x38), messageDeleteCaptureStream(0x39)
	firstAllocated, releaseFirst := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	closeFirst := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	defer closeFirst()
	firstDone := make(chan error, 1)
	go func() {
		_, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{first.LocalID}, true, epoch, streamA, func() error {
			close(firstAllocated)
			<-releaseFirst
			return nil
		})
		firstDone <- err
	}()
	select {
	case <-firstAllocated:
	case <-time.After(10 * time.Second):
		t.Fatal("first stream writer did not reach the allocated position")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, err := store.CaptureDeleteMessagesForTest(ctx, s, c.ID, []int64{second.LocalID}, true, epoch, streamB, nil)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("independent stream capture: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer for an independent stream waited on the first stream")
	}
	closeFirst()
	if err := <-firstDone; err != nil {
		t.Fatalf("first stream capture: %v", err)
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, streamA); !ok || position != 1 {
		t.Fatalf("stream A counter = %d, present %v, want 1", position, ok)
	}
	if position, ok := messageDeleteCaptureCounter(t, ctx, s, epoch, streamB); !ok || position != 1 {
		t.Fatalf("stream B counter = %d, present %v, want 1", position, ok)
	}
}

func TestDeleteMessageCaptureOperationBodiesContainIdentifiersOnly(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551454801")
	b := mustUser(t, s, "+15551454802")
	body := "content must not be captured"
	sender := send(t, s, a, b, body, 7113)
	epoch, stream := int64(15), messageDeleteCaptureStream(0x3a)
	if _, err := store.CaptureDeleteMessagesForTest(ctx, s, a.ID, []int64{sender.LocalID}, true, epoch, stream, nil); err != nil {
		t.Fatalf("capture: %v", err)
	}
	rows := messageDeleteCaptureRows(t, ctx, s, epoch, stream)
	decoded := messageDeleteCaptureDecoded(t, rows, []erasureledger.MessageCopy{
		{OwnerID: a.ID, LocalID: messageDeleteLocalID(t, sender.LocalID)},
		{OwnerID: b.ID, LocalID: messageDeleteLocalID(t, sender.PeerLocalID)},
	})
	if len(decoded) != 1 {
		t.Fatalf("decoded records = %d, want 1", len(decoded))
	}
	if bytes.Contains(rows[0].record, []byte(body)) {
		t.Fatal("record bytes contain the message body")
	}
}
