package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

func (s *Store) ChatRemovalEventForOwnerChat(ctx context.Context, ownerID, chatID int64) (ChatRemovalEvent, bool, error) {
	row, err := s.q.ChatRemovalEventForOwnerChat(ctx, db.ChatRemovalEventForOwnerChatParams{
		OwnerID: ownerID,
		ChatID:  chatID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ChatRemovalEvent{}, false, nil
	case err != nil:
		return ChatRemovalEvent{}, false, fmt.Errorf("chat removal event for owner/chat: %w", err)
	}
	return ChatRemovalEvent{OwnerID: row.OwnerID, ChatID: row.ChatID, Version: int(row.Version)}, true, nil
}

// ChatRemovalSnapshotsForOwner returns pending PTS-neutral removal events.
// Their chat IDs are consumed only after a successful difference response.
func (s *Store) ChatRemovalSnapshotsForOwner(ctx context.Context, ownerID int64, limit int32) ([]ChatRemovalEvent, error) {
	rows, err := s.q.ChatRemovalSnapshotsForOwner(ctx, db.ChatRemovalSnapshotsForOwnerParams{
		OwnerID: ownerID,
		Lim:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("chat removal snapshots for owner: %w", err)
	}
	events := make([]ChatRemovalEvent, len(rows))
	for i, row := range rows {
		events[i] = ChatRemovalEvent{
			OwnerID: row.OwnerID, ChatID: row.ChatID, Version: int(row.Version),
		}
	}
	return events, nil
}

// DeleteChatRemovalStateMarkersForOwnerChats consumes only the removals
// included in the successful getDifference response.
func (s *Store) DeleteChatRemovalStateMarkersForOwnerChats(ctx context.Context, ownerID int64, chatIDs []int64) error {
	if len(chatIDs) == 0 {
		return nil
	}
	if err := s.q.DeleteChatRemovalStateMarkersForOwnerChats(ctx, db.DeleteChatRemovalStateMarkersForOwnerChatsParams{
		OwnerID: ownerID,
		ChatIds: chatIDs,
	}); err != nil {
		return fmt.Errorf("delete chat removal state markers: %w", err)
	}
	return nil
}
