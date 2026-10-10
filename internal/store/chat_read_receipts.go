package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ChatReadMarkSizeThreshold and ChatReadMarkExpirePeriod are Telegram's
// basic-group read-participant eligibility limits.
const (
	ChatReadMarkSizeThreshold = 100
	ChatReadMarkExpirePeriod  = 604800
)

// ChatReadReceiptSweepBatch bounds the rows one expiry-delete statement locks
// and removes. A sweep repeats bounded passes to drain the fixed expired set.
const ChatReadReceiptSweepBatch = 1000

// ChatReadParticipant is the persisted first-read time for one current member.
type ChatReadParticipant struct {
	UserID int64
	ReadAt time.Time
}

// ChatReadParticipantsForMessage returns persisted read dates only for a live
// outgoing copy owned by a current chat member. The SQL query selects and
// authorizes in one statement snapshot, including current membership filtering.
func (s *Store) ChatReadParticipantsForMessage(ctx context.Context, ownerID, chatID, localID int64) (bool, []ChatReadParticipant, error) {
	rows, err := s.q.ChatReadParticipantsForMessage(ctx, db.ChatReadParticipantsForMessageParams{
		OwnerID:       ownerID,
		LocalID:       localID,
		PeerType:      int16(PeerTypeChat),
		PeerID:        chatID,
		ExpirePeriod:  float64(ChatReadMarkExpirePeriod),
		SizeThreshold: ChatReadMarkSizeThreshold,
	})
	if err != nil {
		return false, nil, fmt.Errorf("get chat read participants: %w", err)
	}
	if len(rows) == 0 {
		return false, nil, errors.New("get chat read participants: query returned no authorization row")
	}
	if !rows[0].Authorized {
		return false, nil, nil
	}
	participants := make([]ChatReadParticipant, 0, len(rows))
	for _, row := range rows {
		if row.Authorized != rows[0].Authorized || row.Eligible != rows[0].Eligible {
			return false, nil, errors.New("get chat read participants: inconsistent query state")
		}
		if row.ReaderID == nil {
			if row.ReadAt.Valid {
				return false, nil, errors.New("get chat read participants: date without reader")
			}
			continue
		}
		if !row.ReadAt.Valid {
			return false, nil, fmt.Errorf("get chat read participants: reader %d has no persisted date", *row.ReaderID)
		}
		participants = append(participants, ChatReadParticipant{
			UserID: *row.ReaderID,
			ReadAt: row.ReadAt.Time,
		})
	}
	return true, participants, nil
}

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
