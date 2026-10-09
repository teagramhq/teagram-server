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
