package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const AdminStickerSetPageSize int32 = 50

// AdminUserStickerSet is the operator-visible provenance for an imported set.
type AdminUserStickerSet struct {
	ID        int64
	Title     string
	ShortName string
	CreatorID int64
	CreatedAt time.Time
	Software  string
	IsLive    bool
}

// AdminStickerSetEvent is one immutable operator action.
type AdminStickerSetEvent struct {
	ID                 int64
	Action             string
	SetID              int64
	SetTitle           string
	SetShortName       string
	Reason             string
	SessionFingerprint []byte
	CreatedAt          time.Time
}

// StickerSetAdminRef selects one set by its database ID or short name.
type StickerSetAdminRef struct {
	ID        int64
	ShortName string
}

func (s *Store) AdminUserStickerSetsPage(ctx context.Context, offset, limit int32) ([]AdminUserStickerSet, error) {
	if offset < 0 || limit < 1 || limit > AdminStickerSetPageSize+1 {
		return nil, ErrStickerSetAdminInputInvalid
	}
	rows, err := s.q.AdminUserStickerSetsPage(ctx, db.AdminUserStickerSetsPageParams{
		OffsetCount: offset,
		LimitCount:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list admin user sticker sets: %w", err)
	}
	sets := make([]AdminUserStickerSet, 0, len(rows))
	for _, row := range rows {
		sets = append(sets, AdminUserStickerSet{
			ID: row.ID, Title: row.Title, ShortName: row.ShortName,
			CreatorID: row.CreatorID, CreatedAt: row.CreatedAt.Time,
			Software: row.Software, IsLive: row.IsLive,
		})
	}
	return sets, nil
}

func (s *Store) AdminStickerSetEventsPage(ctx context.Context, offset, limit int32) ([]AdminStickerSetEvent, error) {
	if offset < 0 || limit < 1 || limit > AdminStickerSetPageSize+1 {
		return nil, ErrStickerSetAdminInputInvalid
	}
	rows, err := s.q.AdminStickerSetEventsPage(ctx, db.AdminStickerSetEventsPageParams{
		OffsetCount: offset,
		LimitCount:  limit,
	})
	if err != nil {
		return nil, fmt.Errorf("list admin sticker-set events: %w", err)
	}
	events := make([]AdminStickerSetEvent, 0, len(rows))
	for _, row := range rows {
		events = append(events, AdminStickerSetEvent{
			ID: row.ID, Action: row.Action, SetID: row.SetID,
			SetTitle: row.SetTitle, SetShortName: row.SetShortName,
			Reason: row.Reason, SessionFingerprint: row.SessionFingerprint,
			CreatedAt: row.CreatedAt.Time,
		})
	}
	return events, nil
}

// TakedownUserStickerSet makes a set unavailable and records the operator action
// in the same transaction. Its contents and install rows stay in place.
func (s *Store) TakedownUserStickerSet(ctx context.Context, ref StickerSetAdminRef, reason string, sessionFingerprint []byte) error {
	return s.setUserStickerSetLive(ctx, ref, false, "takedown", reason, sessionFingerprint)
}

// RestoreUserStickerSet makes a previously removed set available again and
// appends a restore event without changing its contents or installs.
func (s *Store) RestoreUserStickerSet(ctx context.Context, ref StickerSetAdminRef, reason string, sessionFingerprint []byte) error {
	return s.setUserStickerSetLive(ctx, ref, true, "restore", reason, sessionFingerprint)
}

func (s *Store) setUserStickerSetLive(
	ctx context.Context,
	ref StickerSetAdminRef,
	live bool,
	action, reason string,
	sessionFingerprint []byte,
) error {
	if !validStickerSetAdminRef(ref) || !validStickerSetAuditReason(reason) || len(sessionFingerprint) != sha256.Size {
		return ErrStickerSetAdminInputInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin sticker-set %s: %w", action, err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err = qtx.LockStickerTakedownInterlock(ctx); err != nil {
		return fmt.Errorf("lock sticker takedown interlock: %w", err)
	}

	var row db.UserCreatedStickerSet
	if ref.ID != 0 {
		row, err = qtx.AdminLockUserStickerSetByID(ctx, ref.ID)
	} else {
		row, err = qtx.AdminLockUserStickerSetByShortName(ctx, ref.ShortName)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrStickerSetNotFound
	}
	if err != nil {
		return fmt.Errorf("lock user sticker set for %s: %w", action, err)
	}
	if row.IsLive == live {
		return ErrStickerSetStateConflict
	}

	if !live {
		items, queryErr := qtx.TakedownUserStickerSetFiles(ctx, row.ID)
		if queryErr != nil {
			return fmt.Errorf("load sticker files for takedown: %w", queryErr)
		}
		if len(items) != int(row.ItemCount) {
			return fmt.Errorf("takedown set %d has %d files, want %d: %w", row.ID, len(items), row.ItemCount, ErrStickerDocumentInvalid)
		}
		for _, item := range items {
			file := File{ID: item.FileID, AccessHash: item.AccessHash, Size: item.Size, Stored: item.Stored}
			data, readErr := s.ReadStickerFileForSet(ctx, file)
			if readErr != nil {
				return fmt.Errorf("read file %d while taking down sticker set %d: %w", item.FileID, row.ID, readErr)
			}
			digest := sha256.Sum256(data)
			updated, updateErr := qtx.UpdateUserStickerSetItemContentSHA256(ctx, db.UpdateUserStickerSetItemContentSHA256Params{
				ContentSha256: digest[:], SetID: row.ID, FileID: item.FileID,
			})
			if updateErr != nil {
				return fmt.Errorf("store blocked sticker content hash for file %d: %w", item.FileID, updateErr)
			}
			if updated != 1 {
				return fmt.Errorf("store blocked sticker content hash for file %d: updated %d rows, want 1", item.FileID, updated)
			}
		}
	}

	updated, err := qtx.AdminSetUserStickerSetLive(ctx, db.AdminSetUserStickerSetLiveParams{IsLive: live, ID: row.ID})
	if err != nil {
		return fmt.Errorf("set user sticker set %d live state: %w", row.ID, err)
	}
	if updated != 1 {
		return fmt.Errorf("set user sticker set %d live state: updated %d rows, want 1", row.ID, updated)
	}
	if err = qtx.InsertAdminStickerSetEvent(ctx, db.InsertAdminStickerSetEventParams{
		Action: action, SetID: row.ID, SetTitle: row.Title, SetShortName: row.ShortName,
		Reason: reason, SessionFingerprint: sessionFingerprint,
	}); err != nil {
		return fmt.Errorf("append sticker-set %s audit event: %w", action, err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit sticker-set %s: %w", action, err)
	}
	return nil
}

func validStickerSetAdminRef(ref StickerSetAdminRef) bool {
	return (ref.ID > 0 && ref.ShortName == "") || (ref.ID == 0 && ref.ShortName != "" && len(ref.ShortName) <= 64)
}

func validStickerSetAuditReason(reason string) bool {
	return len(reason) > 0 && len(reason) <= 512 && utf8.ValidString(reason) &&
		!strings.ContainsRune(reason, 0) && strings.TrimSpace(reason) != ""
}
