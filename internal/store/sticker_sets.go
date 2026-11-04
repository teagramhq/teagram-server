package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

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
	if userID <= 0 || setID == 0 {
		return false, errors.New("install sticker set: invalid user or set identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin install sticker set: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockUserStickerSetOwner(ctx, userID); err != nil {
		return false, fmt.Errorf("lock sticker set owner %d: %w", userID, err)
	}
	if setID > 0 {
		if _, err = qtx.LockLiveUserStickerSet(ctx, setID); errors.Is(err, pgx.ErrNoRows) {
			return false, ErrStickerSetNotFound
		} else if err != nil {
			return false, fmt.Errorf("lock live user sticker set: %w", err)
		}
	}
	installed, err := qtx.UserStickerSetAlreadyInstalled(ctx, db.UserStickerSetAlreadyInstalledParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("check installed sticker set: %w", err)
	}
	if installed {
		if err = tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit repeated sticker set install: %w", err)
		}
		return false, nil
	}
	count, err := qtx.InstalledStickerSetCount(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("count installed sticker sets: %w", err)
	}
	if count >= MaxInstalledStickerSets {
		return false, ErrStickerSetInstallLimit
	}
	rows, err := qtx.InstallStickerSet(ctx, db.InstallStickerSetParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("install sticker set for user %d: %w", userID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit sticker set install: %w", err)
	}
	return rows != 0, nil
}

// UninstallStickerSet removes one resolved catalogue set from one account.
// Removing an absent set is idempotent and reports no change.
func (s *Store) UninstallStickerSet(ctx context.Context, userID, setID int64) (bool, error) {
	if userID <= 0 || setID == 0 {
		return false, errors.New("uninstall sticker set: invalid user or set identity")
	}
	rows, err := s.q.UninstallStickerSet(ctx, db.UninstallStickerSetParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("uninstall sticker set for user %d: %w", userID, err)
	}
	return rows != 0, nil
}

// StickerSetOrderUpdate is one accepted reorder: the kinds it was requested
// for, and the installed set IDs it placed first, in their accepted order.
type StickerSetOrderUpdate struct {
	Masks  bool
	Emojis bool
	SetIDs []int64
}

// ReorderInstalledStickerSets saves the account's installed-set order. setIDs
// are placed first in the order given; IDs not installed by userID and repeats
// are ignored, and every other installed set keeps its current relative order
// after them. It returns the accepted leading IDs, which are empty when none of
// setIDs is installed, and then writes nothing.
func (s *Store) ReorderInstalledStickerSets(ctx context.Context, userID int64, setIDs []int64) ([]int64, error) {
	if userID <= 0 {
		return nil, errors.New("reorder sticker sets: invalid user identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin reorder sticker sets: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockUserStickerSetOwner(ctx, userID); err != nil {
		return nil, fmt.Errorf("lock sticker set owner %d: %w", userID, err)
	}
	rows, err := qtx.StickerSetsByOwner(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list sticker sets for reorder: %w", err)
	}
	installed := make(map[int64]bool, len(rows))
	current := make([]int64, 0, len(rows))
	for _, row := range rows {
		installed[row.SetID] = true
		current = append(current, row.SetID)
	}
	accepted := make([]int64, 0, len(setIDs))
	placed := make(map[int64]bool, len(setIDs))
	for _, id := range setIDs {
		if !installed[id] || placed[id] {
			continue
		}
		placed[id] = true
		accepted = append(accepted, id)
	}
	if len(accepted) == 0 {
		return nil, nil
	}
	order := accepted
	for _, id := range current {
		if !placed[id] {
			order = append(order, id)
		}
	}
	if _, err = qtx.SetUserStickerSetOrder(ctx, db.SetUserStickerSetOrderParams{UserID: userID, SetIds: order}); err != nil {
		return nil, fmt.Errorf("save sticker set order for user %d: %w", userID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit sticker set reorder: %w", err)
	}
	return accepted, nil
}

// InstalledStickerSets returns only the installed-set state for userID, in
// saved order. Callers resolve each set_id against the current static
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
