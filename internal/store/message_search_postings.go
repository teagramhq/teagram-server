package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageSearchPostingsInitializationMaxBatchSize bounds the number of messages
// rebuilt in one dialog transaction.
const MessageSearchPostingsInitializationMaxBatchSize = 1000

// InitializeMessageSearchPostings rebuilds unready message-search postings in
// bounded scans and message chunks. Each committed chunk advances the marker's
// cursor, so a later invocation resumes at the remaining history.
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
			completed, err := s.initializeMessageSearchPostingsDialog(ctx, dialog, int32(batchSize))
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
	dialog db.MessageSearchUnreadyDialogsRow,
	batchSize int32,
) (completed bool, resultErr error) {
	for {
		done, found, err := s.initializeMessageSearchPostingsDialogChunk(ctx, dialog, batchSize)
		if err != nil {
			return false, err
		}
		if !found {
			// Another initializer completed this dialog after the candidate scan.
			return false, nil
		}
		if done {
			return true, nil
		}
	}
}

func (s *Store) initializeMessageSearchPostingsDialogChunk(
	ctx context.Context,
	dialog db.MessageSearchUnreadyDialogsRow,
	batchSize int32,
) (done, found bool, resultErr error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, false, fmt.Errorf("begin message search postings dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	defer func() {
		rollbackErr := tx.Rollback(context.WithoutCancel(ctx))
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback message search postings chunk %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, rollbackErr))
		}
	}()

	qtx := s.q.WithTx(tx)
	params := db.LockMessageSearchUnreadyDialogParams(dialog)
	lastLocalID, err := qtx.LockMessageSearchUnreadyDialog(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	} else if err != nil {
		return false, true, fmt.Errorf("lock unready message search dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}

	backfillParams := db.BackfillMessageSearchDialogPostingsParams{
		OwnerID:     dialog.OwnerID,
		PeerType:    dialog.PeerType,
		PeerID:      dialog.PeerID,
		LastLocalID: lastLocalID,
		BatchSize:   batchSize,
	}
	progress, err := qtx.BackfillMessageSearchDialogPostings(ctx, backfillParams)
	if err != nil {
		return false, true, fmt.Errorf("backfill search postings for dialog %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}

	done = progress.ProcessedCount == 0
	if done {
		markParams := db.MarkMessageSearchDialogReadyParams(dialog)
		if err := qtx.MarkMessageSearchDialogReady(ctx, markParams); err != nil {
			return false, true, fmt.Errorf("mark message search dialog %d/%d/%d ready: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
	} else {
		updateParams := db.UpdateMessageSearchUnreadyDialogProgressParams{
			OwnerID:     dialog.OwnerID,
			PeerType:    dialog.PeerType,
			PeerID:      dialog.PeerID,
			LastLocalID: progress.NextLocalID,
		}
		if err := qtx.UpdateMessageSearchUnreadyDialogProgress(ctx, updateParams); err != nil {
			return false, true, fmt.Errorf("advance message search dialog cursor %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, true, fmt.Errorf("commit message search postings chunk %d/%d/%d: %w", dialog.OwnerID, dialog.PeerType, dialog.PeerID, err)
	}
	return done, true, nil
}
