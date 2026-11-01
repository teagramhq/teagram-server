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

	"github.com/teagramhq/teagram-server/internal/blob"
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
type StickerSetAdminRef interface {
	stickerSetAdminRef()
	String() string
}

type stickerSetAdminRefID struct{ id int64 }

func (stickerSetAdminRefID) stickerSetAdminRef() {}
func (ref stickerSetAdminRefID) String() string  { return fmt.Sprintf("id:%d", ref.id) }

type stickerSetAdminRefShortName struct{ shortName string }

func (stickerSetAdminRefShortName) stickerSetAdminRef() {}
func (ref stickerSetAdminRefShortName) String() string  { return "short_name:" + ref.shortName }

// StickerSetAdminRefByID builds a reference to a sticker set by database ID.
func StickerSetAdminRefByID(id int64) (StickerSetAdminRef, error) {
	if id <= 0 {
		return nil, ErrStickerSetAdminInputInvalid
	}
	return stickerSetAdminRefID{id: id}, nil
}

// StickerSetAdminRefByShortName builds a reference to a sticker set by short name.
func StickerSetAdminRefByShortName(shortName string) (StickerSetAdminRef, error) {
	if shortName == "" || len(shortName) > 64 {
		return nil, ErrStickerSetAdminInputInvalid
	}
	return stickerSetAdminRefShortName{shortName: shortName}, nil
}

type stickerSetContentFingerprint struct {
	fileID     int64
	accessHash int64
	size       int64
	stored     bool
	digest     [sha256.Size]byte
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
	// Blob reads may be remote; fingerprint before opening a transaction, then
	// revalidate the snapshot under the interlock and set row lock before publish.
	var err error
	var snapshot db.UserCreatedStickerSet
	var fingerprints []stickerSetContentFingerprint
	if !live {
		snapshot, fingerprints, err = s.stickerSetTakedownFingerprints(ctx, ref)
		if err != nil {
			return err
		}
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
	switch setRef := ref.(type) {
	case stickerSetAdminRefID:
		row, err = qtx.AdminLockUserStickerSetByID(ctx, setRef.id)
	case stickerSetAdminRefShortName:
		row, err = qtx.AdminLockUserStickerSetByShortName(ctx, setRef.shortName)
	default:
		return ErrStickerSetAdminInputInvalid
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
		if row.ID != snapshot.ID || row.ItemCount != snapshot.ItemCount {
			return ErrStickerSetStateConflict
		}
		items, queryErr := qtx.TakedownUserStickerSetFiles(ctx, row.ID)
		if queryErr != nil {
			return fmt.Errorf("recheck sticker files for takedown: %w", queryErr)
		}
		if len(items) != len(fingerprints) || len(items) != int(row.ItemCount) {
			return ErrStickerSetStateConflict
		}
		for i, item := range items {
			fingerprint := fingerprints[i]
			if item.FileID != fingerprint.fileID || item.AccessHash != fingerprint.accessHash ||
				item.Size != fingerprint.size || item.Stored != fingerprint.stored {
				return ErrStickerSetStateConflict
			}
			updated, updateErr := qtx.UpdateUserStickerSetItemContentSHA256(ctx, db.UpdateUserStickerSetItemContentSHA256Params{
				ContentSha256: fingerprint.digest[:], SetID: row.ID, FileID: item.FileID,
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

func (s *Store) stickerSetTakedownFingerprints(
	ctx context.Context,
	ref StickerSetAdminRef,
) (db.UserCreatedStickerSet, []stickerSetContentFingerprint, error) {
	var row db.UserCreatedStickerSet
	var err error
	switch setRef := ref.(type) {
	case stickerSetAdminRefID:
		row, err = s.q.AdminReadUserStickerSetByID(ctx, setRef.id)
	case stickerSetAdminRefShortName:
		row, err = s.q.AdminReadUserStickerSetByShortName(ctx, setRef.shortName)
	default:
		return db.UserCreatedStickerSet{}, nil, ErrStickerSetAdminInputInvalid
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return db.UserCreatedStickerSet{}, nil, ErrStickerSetNotFound
	}
	if err != nil {
		return db.UserCreatedStickerSet{}, nil, fmt.Errorf("read user sticker set before takedown: %w", err)
	}
	if !row.IsLive {
		return db.UserCreatedStickerSet{}, nil, ErrStickerSetStateConflict
	}
	items, err := s.q.TakedownUserStickerSetFiles(ctx, row.ID)
	if err != nil {
		return db.UserCreatedStickerSet{}, nil, fmt.Errorf("load sticker files before takedown: %w", err)
	}
	if len(items) != int(row.ItemCount) {
		return db.UserCreatedStickerSet{}, nil, fmt.Errorf("takedown set %d has %d files, want %d: %w", row.ID, len(items), row.ItemCount, ErrStickerDocumentInvalid)
	}
	fingerprints := make([]stickerSetContentFingerprint, len(items))
	for i, item := range items {
		file := File{ID: item.FileID, AccessHash: item.AccessHash, Size: item.Size, Stored: item.Stored}
		data, readErr := s.ReadStickerFileForSet(ctx, file)
		if readErr != nil {
			wrappedErr := fmt.Errorf("read file %d while taking down sticker set %d: %w", item.FileID, row.ID, readErr)
			if errors.Is(readErr, blob.ErrNotFound) || errors.Is(readErr, ErrStickerDocumentInvalid) {
				return db.UserCreatedStickerSet{}, nil, fmt.Errorf("%w: %w", ErrStickerDocumentInvalid, wrappedErr)
			}
			return db.UserCreatedStickerSet{}, nil, wrappedErr
		}
		fingerprints[i] = stickerSetContentFingerprint{
			fileID: item.FileID, accessHash: item.AccessHash, size: item.Size, stored: item.Stored,
			digest: sha256.Sum256(data),
		}
	}
	return row, fingerprints, nil
}

func validStickerSetAdminRef(ref StickerSetAdminRef) bool {
	switch setRef := ref.(type) {
	case stickerSetAdminRefID:
		return setRef.id > 0
	case stickerSetAdminRefShortName:
		return setRef.shortName != "" && len(setRef.shortName) <= 64
	default:
		return false
	}
}

func validStickerSetAuditReason(reason string) bool {
	return len(reason) > 0 && len(reason) <= 512 && utf8.ValidString(reason) &&
		!strings.ContainsRune(reason, 0) && strings.TrimSpace(reason) != ""
}
