package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/erasureledger"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

// CaptureDeleteMessagesForTest is the disposable seam for the inert message
// delete capture tests. The capture is composed into the same transaction as
// DeleteMessages, but no production caller can reach this test-only function.
func CaptureDeleteMessagesForTest(
	ctx context.Context,
	s *Store,
	ownerID int64,
	localIDs []int64,
	revoke bool,
	epoch int64,
	stream erasureledger.StreamID,
	afterWrite func() error,
) (map[int64]int, error) {
	return s.deleteMessages(ctx, ownerID, localIDs, revoke, messageDeleteCaptureHookForTest(epoch, stream, afterWrite))
}

func ComposeDeleteMessagesForTest(
	ctx context.Context,
	s *Store,
	identity MessageDeleteReceiptLookupIdentity,
	request MessageDeleteReceiptLookupRequest,
	epoch int64,
	stream erasureledger.StreamID,
	afterWrite func() error,
) (map[int64]int, MessageDeleteReceiptLookup, error) {
	return s.composeMessageDelete(ctx, identity, request, epoch, [16]byte(stream),
		messageDeleteCaptureHookForTest(epoch, stream, afterWrite))
}

func messageDeleteCaptureHookForTest(epoch int64, stream erasureledger.StreamID, afterWrite func() error) messageDeleteCaptureHook {
	return func(ctx context.Context, qtx *db.Queries, changed []messageCopyRef) (MessageDeleteReceiptCaptureReference, error) {
		copies := make([]erasureledger.MessageCopy, 0, len(changed))
		for _, ref := range changed {
			if ref.localID < 1 || ref.localID > math.MaxInt32 {
				return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("message local id %d does not fit the ledger codec", ref.localID)
			}
			copies = append(copies, erasureledger.MessageCopy{
				OwnerID: ref.ownerID,
				LocalID: int32(ref.localID), // #nosec G115 -- bounded to MaxInt32 above.
			})
		}
		payload, err := erasureledger.NewMessageCopies(copies...)
		if err != nil {
			return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("build message-copy payload: %w", err)
		}
		var key erasureledger.OperationKey
		for key == (erasureledger.OperationKey{}) {
			if _, err := rand.Read(key[:]); err != nil {
				return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("draw opaque operation key: %w", err)
			}
		}
		position, err := qtx.AllocateErasureStreamPosition(ctx, db.AllocateErasureStreamPositionParams{
			Epoch:    epoch,
			StreamID: stream[:],
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return MessageDeleteReceiptCaptureReference{}, errors.New("erasure stream position exhausted")
		}
		if err != nil {
			return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("allocate erasure stream position: %w", err)
		}
		record, err := erasureledger.NewRecord(erasureledger.KindMessageCopies, epoch, stream, position, key, payload)
		if err != nil {
			return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("build message-copy record: %w", err)
		}
		body, err := erasureledger.Encode(record)
		if err != nil {
			return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("encode message-copy record: %w", err)
		}
		if err = qtx.InsertErasureOutboxRecord(ctx, db.InsertErasureOutboxRecordParams{
			OperationKey: key[:],
			Epoch:        epoch,
			StreamID:     stream[:],
			Seq:          position,
			Kind:         int16(record.Kind), // #nosec G115 -- the validated codec kind is in 1..9.
			Record:       body,
		}); err != nil {
			return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("insert message-copy outbox record: %w", err)
		}
		if afterWrite != nil {
			if err := afterWrite(); err != nil {
				return MessageDeleteReceiptCaptureReference{}, fmt.Errorf("after message-copy outbox write: %w", err)
			}
		}
		return MessageDeleteReceiptCaptureReference{
			OperationKey: [16]byte(key), Epoch: epoch, StreamID: [16]byte(stream), Position: position,
		}, nil
	}
}
