package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageSearchPostingsInitializationMaxBatchSize bounds the number of unready
// dialogs fetched for one maintenance scan.
const MessageSearchPostingsInitializationMaxBatchSize = 1000

// InitializeMessageSearchPostings rebuilds unready message-search postings in
// bounded scans, with one transaction per dialog. Each committed dialog becomes
// a durable checkpoint, so a later invocation resumes at the remaining markers.
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

		for _, dialog := range dialogs {
			completed, err := s.initializeMessageSearchPostingsDialog(ctx, dialog)
			if err != nil {
				return initialized, err
			}
			if completed {
				initialized++
			}
		}
	}
}

func (s *Store) initializeMessageSearchPostingsDialog(
	ctx context.Context,
	dialog db.MessageSearchUnreadyDialog,
) (completed bool, resultErr error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin message search postings dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	defer func() {
		rollbackErr := tx.Rollback(context.WithoutCancel(ctx))
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback message search postings dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, rollbackErr))
		}
	}()

	qtx := s.q.WithTx(tx)
	params := db.LockMessageSearchUnreadyDialogParams(dialog)
	if _, err := qtx.LockMessageSearchUnreadyDialog(ctx, params); errors.Is(err, pgx.ErrNoRows) {
		// Another initializer committed this dialog after the candidate scan.
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("lock unready message search dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}

	backfillParams := db.BackfillMessageSearchDialogPostingsParams(dialog)
	if err := qtx.BackfillMessageSearchDialogPostings(ctx, backfillParams); err != nil {
		return false, fmt.Errorf("backfill search postings for dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	if err := qtx.MarkMessageSearchDialogReady(ctx, db.MarkMessageSearchDialogReadyParams(dialog)); err != nil {
		return false, fmt.Errorf("mark message search dialog %d/%d/%d ready: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit message search postings dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	return true, nil
}
