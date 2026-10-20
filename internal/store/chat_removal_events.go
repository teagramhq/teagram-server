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

// ChatRemovalSnapshotsForAuthKey returns unacknowledged PTS-neutral removals
// for one authorization key. A response write does not acknowledge an update.
func (s *Store) ChatRemovalSnapshotsForAuthKey(ctx context.Context, ownerID, authKeyID int64, limit int32) ([]ChatRemovalEvent, error) {
	rows, err := s.q.ChatRemovalSnapshotsForAuthKey(ctx, db.ChatRemovalSnapshotsForAuthKeyParams{
		OwnerID:   ownerID,
		AuthKeyID: authKeyID,
		Lim:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("chat removal snapshots for auth key: %w", err)
	}
	events := make([]ChatRemovalEvent, len(rows))
	for i, row := range rows {
		events[i] = ChatRemovalEvent{
			OwnerID: row.OwnerID, ChatID: row.ChatID, Version: int(row.Version),
		}
	}
	return events, nil
}

// MarkChatRemovalEventsDeliveredForAuthKey records events sent to one client.
// They remain replayable until AcknowledgeDeliveredChatRemovalEvents runs.
func (s *Store) MarkChatRemovalEventsDeliveredForAuthKey(ctx context.Context, ownerID, authKeyID int64, chatIDs []int64) error {
	if len(chatIDs) == 0 {
		return nil
	}
	if err := s.q.MarkChatRemovalEventsDeliveredForAuthKey(ctx, db.MarkChatRemovalEventsDeliveredForAuthKeyParams{
		OwnerID:   ownerID,
		AuthKeyID: authKeyID,
		ChatIds:   chatIDs,
	}); err != nil {
		return fmt.Errorf("mark chat removal events delivered: %w", err)
	}
	return nil
}

// AcknowledgeDeliveredChatRemovalEvents retires only removals previously
// delivered to this authorization key when it later requests updates.getState.
func (s *Store) AcknowledgeDeliveredChatRemovalEvents(ctx context.Context, ownerID, authKeyID int64) error {
	if err := s.q.AcknowledgeDeliveredChatRemovalEvents(ctx, db.AcknowledgeDeliveredChatRemovalEventsParams{
		OwnerID: ownerID, AuthKeyID: authKeyID,
	}); err != nil {
		return fmt.Errorf("acknowledge delivered chat removal events: %w", err)
	}
	return nil
}
