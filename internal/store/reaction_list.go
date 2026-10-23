package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageReactionListItem is one reactor returned by messages.getMessageReactionsList.
type MessageReactionListItem struct {
	ReactorID int64
	Reaction  string
	Date      time.Time
}

// MessageReactionListPage carries the total matching reactions and one keyset page.
type MessageReactionListPage struct {
	Count   int
	Items   []MessageReactionListItem
	HasMore bool
}

type reactionListDBRow struct {
	messageVisible bool
	reactionCount  int64
	reactorID      *int64
	reaction       *string
	createdAt      pgtype.Timestamptz
}

// MessageReactionListForChat returns reactions from the caller's own readable
// basic-group message copy. The query rechecks current membership, peer, TTL,
// deletion, and service-message state in the same statement as the page.
func (s *Store) MessageReactionListForChat(
	ctx context.Context,
	ownerID, chatID, localID int64,
	reaction string,
	afterReactorID int64,
	limit int,
) (MessageReactionListPage, error) {
	rows, err := s.q.MessageReactionListForOwner(ctx, db.MessageReactionListForOwnerParams{
		OwnerID:         ownerID,
		LocalID:         localID,
		ChatID:          chatID,
		Reaction:        reaction,
		OffsetReactorID: afterReactorID,
		PageSize:        int64(limit + 1),
	})
	if err != nil {
		return MessageReactionListPage{}, fmt.Errorf("load basic-group reaction list: %w", err)
	}
	dbRows := make([]reactionListDBRow, len(rows))
	for i, row := range rows {
		dbRows[i] = reactionListDBRow{
			messageVisible: row.MessageVisible,
			reactionCount:  row.ReactionCount,
			reactorID:      row.ReactorID,
			reaction:       row.Reaction,
			createdAt:      row.CreatedAt,
		}
	}
	return finishMessageReactionListPage(dbRows, limit)
}

// ChannelMessageReactionListForMember returns reactions only when the same
// statement admits the caller as an unbanned member who can still read a live
// megagroup message above their history floor.
func (s *Store) ChannelMessageReactionListForMember(
	ctx context.Context,
	userID, channelID, localID int64,
	reaction string,
	afterReactorID int64,
	limit int,
) (MessageReactionListPage, error) {
	rows, err := s.q.ChannelMessageReactionListForMember(ctx, db.ChannelMessageReactionListForMemberParams{
		UserID:          userID,
		ChannelID:       channelID,
		LocalID:         localID,
		Reaction:        reaction,
		OffsetReactorID: afterReactorID,
		PageSize:        int64(limit + 1),
	})
	if err != nil {
		return MessageReactionListPage{}, fmt.Errorf("load megagroup reaction list: %w", err)
	}
	dbRows := make([]reactionListDBRow, len(rows))
	for i, row := range rows {
		dbRows[i] = reactionListDBRow{
			messageVisible: row.MessageVisible,
			reactionCount:  row.ReactionCount,
			reactorID:      row.ReactorID,
			reaction:       row.Reaction,
			createdAt:      row.CreatedAt,
		}
	}
	return finishMessageReactionListPage(dbRows, limit)
}

func finishMessageReactionListPage(rows []reactionListDBRow, limit int) (MessageReactionListPage, error) {
	if len(rows) == 0 {
		return MessageReactionListPage{}, errors.New("reaction-list query returned no visibility row")
	}
	if !rows[0].messageVisible {
		return MessageReactionListPage{}, ErrMessageInvalid
	}
	page := MessageReactionListPage{Count: int(rows[0].reactionCount)}
	for _, row := range rows {
		if !row.messageVisible || row.reactionCount != rows[0].reactionCount {
			return MessageReactionListPage{}, errors.New("reaction-list rows disagree on message visibility or count")
		}
		if row.reactorID == nil {
			if row.reaction != nil {
				return MessageReactionListPage{}, errors.New("reaction-list row has reaction without reactor id")
			}
			continue
		}
		if row.reaction == nil || !row.createdAt.Valid {
			return MessageReactionListPage{}, errors.New("reaction-list row is missing reaction data")
		}
		page.Items = append(page.Items, MessageReactionListItem{
			ReactorID: *row.reactorID,
			Reaction:  *row.reaction,
			Date:      row.createdAt.Time,
		})
	}
	if len(page.Items) > limit {
		page.HasMore = true
		page.Items = page.Items[:limit]
	}
	return page, nil
}
