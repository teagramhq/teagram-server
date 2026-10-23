package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// InstalledStickerSet is one catalogue set installed by one account.
type InstalledStickerSet struct {
	ID          int64
	InstalledAt time.Time
}

// InstallStickerSet adds one resolved catalogue set to one account. The
// composite primary key makes repeated installs a no-op.
func (s *Store) InstallStickerSet(ctx context.Context, userID, setID int64) (bool, error) {
	if userID <= 0 || setID >= 0 {
		return false, errors.New("install sticker set: invalid user or set identity")
	}
	rows, err := s.q.InstallStickerSet(ctx, db.InstallStickerSetParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("install sticker set for user %d: %w", userID, err)
	}
	return rows != 0, nil
}

// UninstallStickerSet removes one resolved catalogue set from one account.
// Removing an absent set is idempotent and reports no change.
func (s *Store) UninstallStickerSet(ctx context.Context, userID, setID int64) (bool, error) {
	if userID <= 0 || setID >= 0 {
		return false, errors.New("uninstall sticker set: invalid user or set identity")
	}
	rows, err := s.q.UninstallStickerSet(ctx, db.UninstallStickerSetParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("uninstall sticker set for user %d: %w", userID, err)
	}
	return rows != 0, nil
}

// InstalledStickerSets returns only the installed-set state for userID, in
// stable order. Callers resolve each set_id against the current static
// catalogue and skip rows that no longer have a catalogue entry.
func (s *Store) InstalledStickerSets(ctx context.Context, userID int64) ([]InstalledStickerSet, error) {
	if userID <= 0 {
		return nil, errors.New("list sticker sets: invalid user identity")
	}
	rows, err := s.q.StickerSetsByOwner(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list sticker sets for user %d: %w", userID, err)
	}
	result := make([]InstalledStickerSet, 0, len(rows))
	for _, row := range rows {
		if !row.InstalledAt.Valid {
			return nil, fmt.Errorf("list sticker sets for user %d: set %d has no install time", userID, row.SetID)
		}
		result = append(result, InstalledStickerSet{ID: row.SetID, InstalledAt: row.InstalledAt.Time})
	}
	return result, nil
}
