package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageSearchPostingsInitializationMaxBatchSize bounds the number of dialog
// markers held by one maintenance transaction.
const MessageSearchPostingsInitializationMaxBatchSize = 1000

// InitializeMessageSearchPostings rebuilds unready message-search postings in
// bounded transactions. Each committed batch becomes a durable checkpoint, so
// a later invocation resumes at the remaining unready dialogs.
func (s *Store) InitializeMessageSearchPostings(ctx context.Context, batchSize int) (int64, error) {
	if batchSize < 1 || batchSize > MessageSearchPostingsInitializationMaxBatchSize {
		return 0, fmt.Errorf("initialize message search postings: batch size must be between 1 and %d", MessageSearchPostingsInitializationMaxBatchSize)
	}

	var initialized int64
	for {
		dialogs, err := s.q.MessageSearchUnreadyDialogs(ctx, int32(batchSize))
		if err != nil {
			return initialized, fmt.Errorf("list unready message search dialogs: %w", err)
		}
		if len(dialogs) == 0 {
			return initialized, nil
		}

		completed, err := s.initializeMessageSearchPostingsBatch(ctx, dialogs)
		if err != nil {
			return initialized, err
		}
		initialized += completed
	}
}

func (s *Store) initializeMessageSearchPostingsBatch(
	ctx context.Context,
	dialogs []db.MessageSearchUnreadyDialog,
) (completed int64, resultErr error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin message search postings batch: %w", err)
	}
	defer func() {
		rollbackErr := tx.Rollback(context.WithoutCancel(ctx))
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback message search postings batch: %w", rollbackErr))
		}
	}()

	qtx := s.q.WithTx(tx)
	for _, dialog := range dialogs {
		params := db.LockMessageSearchUnreadyDialogParams(dialog)
		if _, err := qtx.LockMessageSearchUnreadyDialog(ctx, params); errors.Is(err, pgx.ErrNoRows) {
			// Another initializer committed this dialog after the candidate batch
			// was read. Its marker deletion and postings are already atomic.
			continue
		} else if err != nil {
			return 0, fmt.Errorf("lock unready message search dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}

		messageParams := db.LockMessageSearchDialogMessagesParams(dialog)
		if _, err := qtx.LockMessageSearchDialogMessages(ctx, messageParams); err != nil {
			return 0, fmt.Errorf("lock source messages for search dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
		postingParams := db.ClearMessageSearchDialogPostingsParams(dialog)
		if err := qtx.ClearMessageSearchDialogPostings(ctx, postingParams); err != nil {
			return 0, fmt.Errorf("clear search postings for dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
		backfillParams := db.BackfillMessageSearchDialogPostingsParams(dialog)
		if err := qtx.BackfillMessageSearchDialogPostings(ctx, backfillParams); err != nil {
			return 0, fmt.Errorf("backfill search postings for dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
		if err := qtx.MarkMessageSearchDialogReady(ctx, db.MarkMessageSearchDialogReadyParams(dialog)); err != nil {
			return 0, fmt.Errorf("mark message search dialog %d/%d/%d ready: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
		completed++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit message search postings batch: %w", err)
	}
	return completed, nil
}
