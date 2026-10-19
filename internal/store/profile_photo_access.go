package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ProfilePhotoForDownload is the gallery lane's download gate: it answers
// whether the viewer may read the bytes of one profile photo right now, from
// the live facts and nothing else.
//
// It is one autocommit statement on the pool, and that is the shape, not an
// optimisation. The gallery credential is a stateless MAC, so this read is the
// only live fact in the decision, and running it outside any transaction is
// what makes revocation take effect at commit: a gallery-row delete or a block
// committed before this statement starts is in its answer, and one committed
// after it is not. Nothing is held open for the caller to read bytes
// with — no transaction is left running, and no advisory lock is taken, not
// even the photo owner's. The messaging lane owns the per-owner advisory key,
// and a read path that took it would let one account hammering at
// avatars queue the owner's own sends behind it.
//
// The MAC over (viewer, owner, file) is checked by the caller before it gets
// here; this method re-derives nothing, and a capability that verifies
// still has to pass this gate. Every rejection is [ErrFileNotFound] — no such
// file, no such gallery entry, an entry belonging to someone else, a deleted
// entry, a viewer the target blocked, and a file whose bytes were never
// published are one error, so a download answers no question about whose
// gallery an id appears in.
func (s *Store) ProfilePhotoForDownload(ctx context.Context, ownerID, fileID, viewerID int64) (File, error) {
	row, err := s.q.ProfilePhotoForDownload(ctx, db.ProfilePhotoForDownloadParams{
		OwnerID:  ownerID,
		FileID:   fileID,
		ViewerID: viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return File{}, ErrFileNotFound
	case err != nil:
		return File{}, fmt.Errorf("profile photo for download: %w", err)
	}
	return fileFromRow(row), nil
}

// CurrentProfilePhotoForDownload resolves the selected avatar for the ordinary
// InputPeerPhotoFileLocation path. Unlike gallery downloads, that location has
// no photo MAC, so the live query requires both the viewer-bound peer identity
// and that the requested file remains the owner's current selection.
func (s *Store) CurrentProfilePhotoForDownload(ctx context.Context, ownerID, fileID, viewerID int64) (File, error) {
	row, err := s.q.CurrentProfilePhotoForDownload(ctx, db.CurrentProfilePhotoForDownloadParams{
		OwnerID: ownerID, FileID: fileID, ViewerID: viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return File{}, ErrFileNotFound
	case err != nil:
		return File{}, fmt.Errorf("current profile photo for download: %w", err)
	}
	return fileFromRow(row), nil
}

// CurrentProfilePhotoIDsForViewer returns the visible selected avatar IDs for
// a bounded set of user objects. A blocked viewer and an account with no live
// selected photo both have no entry in the result.
func (s *Store) CurrentProfilePhotoIDsForViewer(ctx context.Context, ownerIDs []int64, viewerID int64) (map[int64]int64, error) {
	rows, err := s.q.CurrentProfilePhotoIDsForViewer(ctx, db.CurrentProfilePhotoIDsForViewerParams{
		OwnerIds: ownerIDs, ViewerID: viewerID,
	})
	if err != nil {
		return nil, fmt.Errorf("current profile photo ids: %w", err)
	}
	out := make(map[int64]int64, len(rows))
	for _, row := range rows {
		if row.FileID != nil {
			out[row.OwnerID] = *row.FileID
		}
	}
	return out, nil
}

// ProfilePhotoDerivativeForDownload returns an advertised m size only while
// the photo remains in the owner's gallery and the owner has not blocked the
// viewer. The query repeats the gallery gate instead of borrowing the message
// lane's derivative entitlement.
func (s *Store) ProfilePhotoDerivativeForDownload(ctx context.Context, ownerID, fileID, viewerID int64) (*PhotoDerivatives, error) {
	row, err := s.q.ProfilePhotoDerivativeForDownload(ctx, db.ProfilePhotoDerivativeForDownloadParams{
		OwnerID: ownerID, FileID: fileID, ViewerID: viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrFileNotFound
	case err != nil:
		return nil, fmt.Errorf("profile photo derivative for download: %w", err)
	}
	return photoDerivativesFromRow(row.MWidth, row.MHeight, row.MSize, row.Stripped), nil
}

// ProfilePhotoDerivativeChunkForDownload reads one bounded m byte slice while
// repeating the live gallery policy in the statement that returns those bytes.
func (s *Store) ProfilePhotoDerivativeChunkForDownload(
	ctx context.Context, ownerID, fileID, viewerID, offset, limit int64,
) ([]byte, error) {
	row, err := s.q.ProfilePhotoDerivativeChunkForDownload(ctx, db.ProfilePhotoDerivativeChunkForDownloadParams{
		// upload.getFile validates the window and the derivative caps its offset.
		ByteOffset: int32(offset), //nolint:gosec // G115: bounded by the 65,536-byte derivative.
		Lim:        int32(limit),  //nolint:gosec // G115: bounded by upload.getFile's 1 MiB limit.
		OwnerID:    ownerID,
		FileID:     fileID,
		ViewerID:   viewerID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrFileNotFound
	case err != nil:
		return nil, fmt.Errorf("profile photo derivative chunk for download: %w", err)
	}
	return row, nil
}
