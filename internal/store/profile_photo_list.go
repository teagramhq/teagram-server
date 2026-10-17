package store

import (
	"context"
	"fmt"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ProfilePhotosForOwner returns one visible page of an owner's stored gallery
// and the visible gallery count. The owner-directed block check and page read
// share one statement, so a blocked viewer receives no photo identity or count.
func (s *Store) ProfilePhotosForOwner(ctx context.Context, ownerID, viewerID int64, offset int, maxID int64, limit int) ([]File, int64, error) {
	rows, err := s.q.ProfilePhotosByOwner(ctx, db.ProfilePhotosByOwnerParams{
		PageOffset: int64(offset),
		MaxID:      maxID,
		PageLimit:  int64(limit),
		OwnerID:    ownerID,
		ViewerID:   viewerID,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("profile photos by owner: %w", err)
	}
	if len(rows) == 0 {
		return []File{}, 0, nil
	}

	files := make([]File, 0, len(rows))
	total := rows[0].Total
	for _, row := range rows {
		if !row.HasPhoto {
			continue
		}
		file := fileFromRow(db.File{
			ID:            row.ID,
			UploaderID:    row.UploaderID,
			AccessHash:    row.AccessHash,
			Size:          row.Size,
			MimeType:      row.MimeType,
			FileName:      row.FileName,
			Stored:        row.Stored,
			Date:          row.Date,
			SubtypeRights: row.SubtypeRights,
			MediaKind:     row.MediaKind,
			Width:         row.Width,
			Height:        row.Height,
		})
		if row.DerivativeFileID != nil {
			file.PhotoDerivatives = photoDerivativesFromRow(row.MWidth, row.MHeight, row.MSize, row.Stripped)
		}
		files = append(files, file)
	}
	return files, total, nil
}
