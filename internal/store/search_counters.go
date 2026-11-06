package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// CountSharedMedia counts the requested filters in one snapshot and one scan.
func (s *Store) CountSharedMedia(ctx context.Context, ownerID int64, peerType PeerType, peerID int64, filters []MediaSearchFilter) (map[MediaSearchFilter]int, error) {
	var mask int32
	for _, filter := range filters {
		if filter < MediaSearchFilterDocument || filter > MediaSearchFilterPhotoVideo {
			return nil, fmt.Errorf("unsupported media search filter %d", filter)
		}
		mask |= 1 << (filter - 1)
	}
	counts := make(map[MediaSearchFilter]int, len(filters))
	if mask == 0 {
		return counts, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin shared media counts: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	if peerType == PeerTypeChat {
		member, err := q.IsChatMember(ctx, db.IsChatMemberParams{ChatID: peerID, UserID: ownerID})
		if err != nil {
			return nil, fmt.Errorf("check chat membership: %w", err)
		}
		if !member {
			return nil, ErrNotMember
		}
	}
	if peerType == PeerTypeChannel {
		member, err := q.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{ChannelID: peerID, UserID: ownerID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotMember
		}
		if err != nil {
			return nil, fmt.Errorf("check channel membership: %w", err)
		}
		if channelMemberFromRow(member).Banned(time.Now()) {
			return nil, ErrNotMember
		}
	}
	values := make(map[MediaSearchFilter]int, len(filters))
	if peerType == PeerTypeChannel {
		row, err := q.CountAllFilteredChannelPosts(ctx, db.CountAllFilteredChannelPostsParams{FilterMask: mask, ChannelID: peerID, OwnerID: ownerID})
		if err != nil {
			return nil, fmt.Errorf("count shared channel media: %w", err)
		}
		for _, count := range row {
			values[MediaSearchFilter(count.Filter)] = int(count.Count)
		}
	} else {
		row, err := q.CountAllFilteredMessages(ctx, db.CountAllFilteredMessagesParams{FilterMask: mask, OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID})
		if err != nil {
			return nil, fmt.Errorf("count shared dialog media: %w", err)
		}
		for _, count := range row {
			values[MediaSearchFilter(count.Filter)] = int(count.Count)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit shared media counts: %w", err)
	}
	return values, nil
}
