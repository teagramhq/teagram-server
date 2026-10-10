package store

import (
	"context"
	"fmt"
)

// ChatReadReceiptSweepBatch bounds the rows one expiry-delete statement locks
// and removes. A sweep repeats bounded passes to drain the fixed expired set.
const ChatReadReceiptSweepBatch = 1000

// SweepExpiredChatReadReceipts removes receipt data after the message's
// seven-day eligibility window. It only touches the derived receipt table; the
// original messages and dialog read markers are left intact.
func (s *Store) SweepExpiredChatReadReceipts(ctx context.Context) (int64, error) {
	var total int64
	for {
		deleted, err := s.q.DeleteExpiredChatReadReceipts(ctx, ChatReadReceiptSweepBatch)
		total += deleted
		if err != nil {
			return total, fmt.Errorf("sweep expired chat read receipts: %w", err)
		}
		if deleted < ChatReadReceiptSweepBatch {
			return total, nil
		}
	}
}
