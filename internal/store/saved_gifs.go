package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const maxSavedGIFs int32 = 200

// ErrSavedGIFInvalid covers every save refusal, so callers cannot distinguish
// an absent file from a forged hash, a revoked entitlement, or a non-GIF.
var ErrSavedGIFInvalid = errors.New("saved GIF is invalid")

// SaveGIF adds or moves one currently readable animated document to the
// caller's saved list. It uses the file download entitlement in the insert
// statement and trims the owner's rows before committing.
func (s *Store) SaveGIF(ctx context.Context, ownerID, fileID, accessHash int64) error {
	if ownerID <= 0 || fileID <= 0 {
		return ErrSavedGIFInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("save GIF: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	inserted, err := qtx.InsertSavedGIF(ctx, db.InsertSavedGIFParams{
		OwnerID: ownerID, ID: fileID, AccessHash: accessHash,
	})
	if err != nil {
		return fmt.Errorf("save GIF: insert: %w", err)
	}
	if inserted == 0 {
		return ErrSavedGIFInvalid
	}
	if _, err := qtx.TrimSavedGIFs(ctx, db.TrimSavedGIFsParams{OwnerID: ownerID, Limit: maxSavedGIFs}); err != nil {
		return fmt.Errorf("save GIF: trim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("save GIF: commit: %w", err)
	}
	return nil
}

// UnsaveGIF removes only this owner's bookmark. It does not inspect file
// metadata or access hashes, and deleting an absent bookmark succeeds.
func (s *Store) UnsaveGIF(ctx context.Context, ownerID, fileID int64) error {
	if ownerID <= 0 || fileID <= 0 {
		return ErrSavedGIFInvalid
	}
	if _, err := s.q.UnsaveSavedGIF(ctx, db.UnsaveSavedGIFParams{OwnerID: ownerID, FileID: fileID}); err != nil {
		return fmt.Errorf("unsave GIF: %w", err)
	}
	return nil
}

// SavedGIFs returns only currently readable GIF documents, newest saved first.
// Old bookmarks remain inert after revocation and are removed by the account
// bound as newer GIFs are saved.
func (s *Store) SavedGIFs(ctx context.Context, ownerID int64) ([]File, error) {
	if ownerID <= 0 {
		return nil, ErrSavedGIFInvalid
	}
	rows, err := s.q.SavedGIFs(ctx, db.SavedGIFsParams{OwnerID: ownerID, Limit: maxSavedGIFs})
	if err != nil {
		return nil, fmt.Errorf("saved GIFs: %w", err)
	}
	files := make([]File, len(rows))
	for i, row := range rows {
		files[i] = fileFromRow(row)
	}
	return files, nil
}
