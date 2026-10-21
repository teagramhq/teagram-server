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

// MarkChatRemovalEventsDeliveredForAuthKey records events sent in a live push.
// Live pushes carry no applied-state cursor, so they remain replayable.
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

// IssueChatRemovalEventsForAuthKey atomically reserves a unique difference Date
// token for this authorization key and associates it with the removal rows in
// the reply. resumeDate preserves the logical boundary used by other streams.
func (s *Store) IssueChatRemovalEventsForAuthKey(
	ctx context.Context,
	ownerID, authKeyID int64,
	chatIDs []int64,
	candidateDate, statePts, stateQts, resumeDate int,
) (int, error) {
	if len(chatIDs) == 0 {
		return candidateDate, nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin chat removal state reservation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	stateDate, err := qtx.ReserveChatRemovalDifferenceStateDateForAuthKey(ctx, db.ReserveChatRemovalDifferenceStateDateForAuthKeyParams{
		OwnerID: ownerID, AuthKeyID: authKeyID, CandidateDate: int64(candidateDate),
	})
	if err != nil {
		return 0, fmt.Errorf("reserve chat removal difference state date: %w", err)
	}
	if err := qtx.MarkChatRemovalEventsIssuedForAuthKey(ctx, db.MarkChatRemovalEventsIssuedForAuthKeyParams{
		OwnerID: ownerID, AuthKeyID: authKeyID, ChatIds: chatIDs,
		DifferenceStatePts: int64(statePts), DifferenceStateQts: int64(stateQts),
		DifferenceStateDate: stateDate, ResumeDate: int64(resumeDate),
	}); err != nil {
		return 0, fmt.Errorf("mark chat removal events issued: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit chat removal state reservation: %w", err)
	}
	return int(stateDate), nil
}

// ChatRemovalResumeDateForState translates an issued or superseded removal
// token to its logical Date without changing the client's acknowledgement.
func (s *Store) ChatRemovalResumeDateForState(ctx context.Context, ownerID, authKeyID int64, stateDate int) (int, bool, error) {
	stateDateValue := int64(stateDate)
	rows, err := s.q.ChatRemovalResumeDatesForState(ctx, db.ChatRemovalResumeDatesForStateParams{
		OwnerID: ownerID, AuthKeyID: authKeyID, DifferenceStateDate: stateDateValue,
	})
	if err != nil {
		return 0, false, fmt.Errorf("chat removal resume date: %w", err)
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	resumeDate := int(rows[0])
	for _, row := range rows[1:] {
		if int(row) != resumeDate {
			return 0, false, fmt.Errorf("chat removal state date %d maps to multiple resume dates", stateDate)
		}
	}
	return resumeDate, true, nil
}

// AcknowledgeChatRemovalEventsForState retires only rows included in a
// difference reply whose Date token the client later presents with PTS and QTS
// at least as advanced as that response. It returns the logical date before the
// token so other recovery streams do not mistake it for a wall-clock boundary.
func (s *Store) AcknowledgeChatRemovalEventsForState(
	ctx context.Context,
	ownerID, authKeyID int64,
	requestPts, requestQts, stateDate int,
) (int, bool, error) {
	requestPtsValue := int64(requestPts)
	requestQtsValue := int64(requestQts)
	stateDateValue := int64(stateDate)
	rows, err := s.q.AcknowledgeChatRemovalEventsForState(ctx, db.AcknowledgeChatRemovalEventsForStateParams{
		OwnerID: ownerID, AuthKeyID: authKeyID,
		RequestPts: requestPtsValue, RequestQts: requestQtsValue, DifferenceStateDate: stateDateValue,
	})
	if err != nil {
		return 0, false, fmt.Errorf("acknowledge chat removal difference state: %w", err)
	}
	if len(rows) == 0 {
		return 0, false, nil
	}
	resumeDate := int(rows[0])
	for _, row := range rows[1:] {
		if int(row) != resumeDate {
			return 0, false, fmt.Errorf("chat removal state date %d maps to multiple resume dates", stateDate)
		}
	}
	return resumeDate, true, nil
}
