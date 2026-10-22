package store

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// DeleteProfilePhotos removes only the caller's live gallery entries named by
// fileIDs. The caller has already checked the self-bound photo hashes; this
// store boundary scopes every lookup and write to ownerID again.
func (s *Store) DeleteProfilePhotos(ctx context.Context, ownerID int64, fileIDs []int64) ([]int64, error) {
	fileIDs = uniquePositiveIDs(fileIDs)
	if len(fileIDs) == 0 {
		return []int64{}, nil
	}
	conn, tx, err := s.beginProfilePhotoMutation(ctx, ownerID)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	live, err := qtx.LiveProfilePhotosForDeletion(ctx, db.LiveProfilePhotosForDeletionParams{
		UserID: ownerID, FileIds: fileIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("profile photo deletion: read live gallery entries: %w", err)
	}
	if len(live) == 0 {
		return []int64{}, nil
	}

	receipts, err := qtx.LockProfileUploadReceiptsForDeletion(ctx, db.LockProfileUploadReceiptsForDeletionParams{
		UserID: ownerID, FileIds: live,
	})
	if err != nil {
		return nil, fmt.Errorf("profile photo deletion: lock upload receipts: %w", err)
	}
	clientFileIDs := make([]int64, 0, len(receipts))
	for _, receipt := range receipts {
		clientFileIDs = append(clientFileIDs, receipt.ClientFileID)
	}

	if err := qtx.EnsureProfilePhotoState(ctx, ownerID); err != nil {
		return nil, fmt.Errorf("profile photo deletion: ensure state row: %w", err)
	}
	if _, err := qtx.LockProfilePhotoState(ctx, ownerID); err != nil {
		return nil, fmt.Errorf("profile photo deletion: lock state row: %w", err)
	}
	if len(clientFileIDs) > 0 {
		if _, err := qtx.TerminalizeProfileUploadReceiptsForDeletion(ctx, db.TerminalizeProfileUploadReceiptsForDeletionParams{
			UserID: ownerID, ClientFileIds: clientFileIDs,
		}); err != nil {
			return nil, fmt.Errorf("profile photo deletion: terminalize upload receipts: %w", err)
		}
	}
	state, err := qtx.RemoveProfilePhotoStateEntries(ctx, db.RemoveProfilePhotoStateEntriesParams{
		UserID: ownerID, FileIds: live,
	})
	if err != nil {
		return nil, fmt.Errorf("profile photo deletion: advance profile revision: %w", err)
	}
	deleted, err := qtx.DeleteUserPhotosByIDs(ctx, db.DeleteUserPhotosByIDsParams{
		UserID: ownerID, FileIds: live,
	})
	if err != nil {
		return nil, fmt.Errorf("profile photo deletion: remove gallery entries: %w", err)
	}
	if len(deleted) != len(live) {
		return nil, fmt.Errorf("profile photo deletion: removed %d of %d locked gallery entries at revision %d", len(deleted), len(live), state.MutationRevision)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("profile photo deletion: commit: %w", err)
	}
	return live, nil
}

// ClearCurrentProfilePhoto removes the current entry with the production wait
// bound.
func (s *Store) ClearCurrentProfilePhoto(
	ctx context.Context,
	ownerID, authKeyID, sessionID, msgID int64,
	rateLimit RateLimitConfig,
) error {
	return s.ClearCurrentProfilePhotoWithWait(
		ctx, ownerID, authKeyID, sessionID, msgID, rateLimit, ProfilePhotoSelectionWait,
	)
}

// ClearCurrentProfilePhotoWithWait removes the current entry and records the resolved
// target against the authenticated transport request in the same transaction.
// The owner lock serializes receipt detection with rate admission across
// connections, and the charge commits with the clear. Replaying the same tuple
// returns without resolving current again or spending another token; a new
// msg_id remains a fresh clear. wait bounds connection admission and lock wait.
func (s *Store) ClearCurrentProfilePhotoWithWait(
	ctx context.Context,
	ownerID, authKeyID, sessionID, msgID int64,
	rateLimit RateLimitConfig,
	wait time.Duration,
) error {
	if authKeyID == 0 || msgID <= 0 {
		return errors.New("profile photo clear: invalid transport request identity")
	}
	conn, tx, err := s.beginProfilePhotoMutationWithWait(ctx, ownerID, wait)
	if err != nil {
		return err
	}
	defer conn.Release()
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if _, err := qtx.DeleteExpiredProfilePhotoDeleteOperations(ctx, ownerID); err != nil {
		return fmt.Errorf("profile photo clear: expire old request receipts: %w", err)
	}
	if _, err := qtx.ProfilePhotoDeleteOperation(ctx, db.ProfilePhotoDeleteOperationParams{
		UserID: ownerID, AuthKeyID: authKeyID, SessionID: sessionID, MsgID: msgID,
	}); err == nil {
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("profile photo clear: commit replay: %w", err)
		}
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("profile photo clear: read request receipt: %w", err)
	}

	var state db.ProfilePhotoState
	state, err = qtx.GetProfilePhotoState(ctx, ownerID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("profile photo clear: read current state: %w", err)
	}
	stateExists := err == nil
	var targetFileID *int64
	if stateExists && state.CurrentFileID != nil {
		currentID := *state.CurrentFileID
		targetFileID = &currentID
	}

	var clientFileID *int64
	var clearRevision *int64
	if targetFileID != nil {
		receipts, err := qtx.LockProfileUploadReceiptsForDeletion(ctx, db.LockProfileUploadReceiptsForDeletionParams{
			UserID: ownerID, FileIds: []int64{*targetFileID},
		})
		if err != nil {
			return fmt.Errorf("profile photo clear: lock upload receipt: %w", err)
		}
		if len(receipts) > 0 {
			id := receipts[0].ClientFileID
			clientFileID = &id
		}
		locked, err := qtx.LockProfilePhotoState(ctx, ownerID)
		if err != nil {
			return fmt.Errorf("profile photo clear: lock current state: %w", err)
		}
		if locked.CurrentFileID == nil || *locked.CurrentFileID != *targetFileID {
			return errors.New("profile photo clear: current photo changed while profile lock was held")
		}
		if clientFileID != nil {
			if _, err := qtx.TerminalizeProfileUploadReceiptsForDeletion(ctx, db.TerminalizeProfileUploadReceiptsForDeletionParams{
				UserID: ownerID, ClientFileIds: []int64{*clientFileID},
			}); err != nil {
				return fmt.Errorf("profile photo clear: terminalize upload receipt: %w", err)
			}
		}
		cleared, err := qtx.RemoveProfilePhotoStateEntries(ctx, db.RemoveProfilePhotoStateEntriesParams{
			UserID: ownerID, FileIds: []int64{*targetFileID},
		})
		if err != nil {
			return fmt.Errorf("profile photo clear: advance profile revision: %w", err)
		}
		clearRevision = &cleared.MutationRevision
		deleted, err := qtx.DeleteUserPhotosByIDs(ctx, db.DeleteUserPhotosByIDsParams{
			UserID: ownerID, FileIds: []int64{*targetFileID},
		})
		if err != nil {
			return fmt.Errorf("profile photo clear: remove current gallery entry: %w", err)
		}
		if len(deleted) != 1 || deleted[0] != *targetFileID {
			return fmt.Errorf("profile photo clear: removed entries %v, want current photo %d", deleted, *targetFileID)
		}
	} else if stateExists {
		revision := state.MutationRevision
		clearRevision = &revision
	}

	var operationKey [16]byte
	if _, err := io.ReadFull(cryptorand.Reader, operationKey[:]); err != nil {
		return fmt.Errorf("profile photo clear: generate operation key: %w", err)
	}
	if _, err := qtx.InsertProfilePhotoDeleteOperation(ctx, db.InsertProfilePhotoDeleteOperationParams{
		UserID: ownerID, AuthKeyID: authKeyID, SessionID: sessionID, MsgID: msgID,
		OperationKey: operationKey[:], TargetFileID: targetFileID,
		ClientFileID: clientFileID, ClearRevision: clearRevision,
	}); err != nil {
		return fmt.Errorf("profile photo clear: record request receipt: %w", err)
	}
	// Charge after acquiring the profile and receipt locks so this transaction
	// never holds the limiter row while acquiring another lock class. A denial
	// rolls back the prepared clear and its receipt with the token charge.
	if err := s.consumeRateLimitTx(ctx, qtx, ownerID, "update_profile", rateLimit, 1); err != nil {
		return fmt.Errorf("profile photo clear: charge rate limit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("profile photo clear: commit: %w", err)
	}
	return nil
}

func uniquePositiveIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id < 1 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
