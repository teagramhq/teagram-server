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

// ReorderedStickerSet is one set in a committed reorder result. Kind is set
// for user-created sets and empty for catalogue sets.
type ReorderedStickerSet struct {
	ID   int64
	Kind string
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin uninstall sticker set: %w", err)
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
	rows, err := qtx.UninstallStickerSet(ctx, db.UninstallStickerSetParams{UserID: userID, SetID: setID})
	if err != nil {
		return false, fmt.Errorf("uninstall sticker set for user %d: %w", userID, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit sticker set uninstall: %w", err)
	}
	return rows != 0, nil
}

// InstalledStickerSets returns only the installed-set state for userID, in the
// account's chosen order. Callers resolve each set_id against the current static
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

// ReorderInstalledStickerSets places the listed installed sets first, in the
// listed order, and keeps every other installed set in its current relative
// order. Ids the account has not installed are ignored, and a repeated id is
// placed once. The owner row lock serialises the reorder with installs. It
// returns the resulting order and each user-created set's kind from the same
// locked transaction.
func (s *Store) ReorderInstalledStickerSets(ctx context.Context, userID int64, listed []int64) ([]ReorderedStickerSet, error) {
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
	userSetIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.SetID > 0 {
			userSetIDs = append(userSetIDs, row.SetID)
		}
	}
	userSetKinds := make(map[int64]string, len(userSetIDs))
	if len(userSetIDs) > 0 {
		userSets, lookupErr := qtx.UserCreatedStickerSetsByInstalledIDs(ctx, userSetIDs)
		if lookupErr != nil {
			return nil, fmt.Errorf("list user-created sticker-set kinds for reorder: %w", lookupErr)
		}
		for _, set := range userSets {
			userSetKinds[set.ID] = set.Kind
		}
	}
	installed := make(map[int64]ReorderedStickerSet, len(rows))
	current := make([]int64, 0, len(rows))
	for _, row := range rows {
		installed[row.SetID] = ReorderedStickerSet{ID: row.SetID, Kind: userSetKinds[row.SetID]}
		current = append(current, row.SetID)
	}
	order := make([]ReorderedStickerSet, 0, len(current))
	placed := make(map[int64]bool, len(listed))
	for _, id := range listed {
		set, ok := installed[id]
		if !ok || placed[id] {
			continue
		}
		placed[id] = true
		order = append(order, set)
	}
	for _, id := range current {
		if !placed[id] {
			order = append(order, installed[id])
		}
	}
	if len(order) > 0 {
		orderIDs := make([]int64, 0, len(order))
		for _, set := range order {
			orderIDs = append(orderIDs, set.ID)
		}
		if _, err = qtx.SetInstalledStickerSetPositions(ctx, db.SetInstalledStickerSetPositionsParams{UserID: userID, SetIds: orderIDs}); err != nil {
			return nil, fmt.Errorf("set sticker set positions for user %d: %w", userID, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit sticker set reorder: %w", err)
	}
	return order, nil
}

// NotifyStickerSetOrderChanged publishes the current installed order for kind.
// It holds the same owner lock as reorders until the notification transaction
// commits, so a delayed post-reorder callback cannot publish an older snapshot.
func (s *Store) NotifyStickerSetOrderChanged(
	ctx context.Context,
	ownerID, authKeyID, sessionID int64,
	kind string,
	catalogKinds map[int64]string,
) error {
	if ownerID <= 0 || authKeyID == 0 {
		return errors.New("notify sticker-set order: invalid owner or auth key")
	}
	switch kind {
	case "regular", "mask", "emojis":
	default:
		return errors.New("notify sticker-set order: invalid kind")
	}

	notifyCtx, cancel := context.WithTimeout(ctx, notificationTimeout)
	defer cancel()
	tx, err := s.pool.Begin(notifyCtx)
	if err != nil {
		return fmt.Errorf("begin sticker-set order notification: %w", err)
	}
	defer func() { _ = tx.Rollback(notifyCtx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockUserStickerSetOwner(notifyCtx, ownerID); err != nil {
		return fmt.Errorf("lock sticker-set owner %d for notification: %w", ownerID, err)
	}
	rows, err := qtx.StickerSetsByOwner(notifyCtx, ownerID)
	if err != nil {
		return fmt.Errorf("list sticker sets for notification: %w", err)
	}
	userSetIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.SetID > 0 {
			userSetIDs = append(userSetIDs, row.SetID)
		}
	}
	userSetKinds := make(map[int64]string, len(userSetIDs))
	if len(userSetIDs) > 0 {
		userSets, lookupErr := qtx.UserCreatedStickerSetsByInstalledIDs(notifyCtx, userSetIDs)
		if lookupErr != nil {
			return fmt.Errorf("list user-created sticker-set kinds for notification: %w", lookupErr)
		}
		for _, set := range userSets {
			userSetKinds[set.ID] = set.Kind
		}
	}
	order := make([]int64, 0, len(rows))
	for _, row := range rows {
		setKind := userSetKinds[row.SetID]
		if row.SetID < 0 {
			setKind = catalogKinds[row.SetID]
		}
		if setKind == kind {
			order = append(order, row.SetID)
		}
	}
	payload := StickerSetOrderChangedPayload(ownerID, authKeyID, sessionID, kind, order)
	if _, err = tx.Exec(notifyCtx, `SELECT pg_notify($1, $2)`, ChannelStickerSets, payload); err != nil {
		return fmt.Errorf("notify sticker-set order: %w", err)
	}
	if err = tx.Commit(notifyCtx); err != nil {
		return fmt.Errorf("commit sticker-set order notification: %w", err)
	}
	return nil
}
