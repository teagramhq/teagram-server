package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	profilePhotoSelectionLockWait = 250 * time.Millisecond
	profilePhotoSelectionPoll     = 10 * time.Millisecond
)

// SelectProfilePhoto makes one live photo in the owner's gallery current. The
// caller has already verified the self-bound photo access hash; the live gallery
// row is checked again here under the same profile-domain serialization key the
// upload lane holds. This operation only changes the current pointer and its
// revision.
func (s *Store) SelectProfilePhoto(ctx context.Context, ownerID, fileID int64) (File, bool, error) {
	class, obj, ok := profileOwnerLockKey(ownerID)
	if !ok || fileID < 1 {
		return File{}, false, ErrProfilePhotoUnavailable
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return File{}, false, fmt.Errorf("profile photo selection: acquire connection: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return File{}, false, fmt.Errorf("profile photo selection: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit

	lockCtx, cancel := context.WithTimeout(ctx, profilePhotoSelectionLockWait)
	defer cancel()
	for {
		var acquired bool
		if err := tx.QueryRow(lockCtx,
			`SELECT pg_try_advisory_xact_lock($1::int, $2::int)`, class, obj,
		).Scan(&acquired); err != nil {
			if ctx.Err() == nil && errors.Is(lockCtx.Err(), context.DeadlineExceeded) {
				return File{}, false, ErrProfilePhotoBusy
			}
			return File{}, false, fmt.Errorf("profile photo selection: acquire profile-domain lock: %w", err)
		}
		if acquired {
			break
		}
		select {
		case <-lockCtx.Done():
			if ctx.Err() != nil {
				return File{}, false, ctx.Err()
			}
			return File{}, false, ErrProfilePhotoBusy
		case <-time.After(profilePhotoSelectionPoll):
		}
	}

	qtx := s.q.WithTx(tx)
	state, err := qtx.LockProfilePhotoState(ctx, ownerID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return File{}, false, ErrProfilePhotoUnavailable
	case err != nil:
		return File{}, false, fmt.Errorf("profile photo selection: lock profile state: %w", err)
	}

	rows, err := qtx.ProfilePhotosByOwner(ctx, db.ProfilePhotosByOwnerParams{
		OwnerID: ownerID, ViewerID: ownerID, PageOffset: -1, MaxID: fileID, PageLimit: 1,
	})
	if err != nil {
		return File{}, false, fmt.Errorf("profile photo selection: read live gallery entry: %w", err)
	}
	if len(rows) != 1 || !rows[0].HasPhoto {
		return File{}, false, ErrProfilePhotoUnavailable
	}
	photo := profilePhotoFileFromRow(rows[0])
	changed := state.CurrentFileID == nil || *state.CurrentFileID != fileID
	if changed {
		if _, err := qtx.SelectProfilePhoto(ctx, db.SelectProfilePhotoParams{
			UserID: ownerID, CurrentFileID: &fileID,
		}); err != nil {
			return File{}, false, fmt.Errorf("profile photo selection: update current photo: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return File{}, false, fmt.Errorf("profile photo selection: commit: %w", err)
	}
	return photo, changed, nil
}
