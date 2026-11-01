package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	MaxUserStickerSetsPerAccount = 100
	MaxInstalledStickerSets      = 200
	MaxUserStickerCount          = 120
	MaxUserEmojiCount            = 200
	MaxStoredStickerBytes        = 512 << 10
)

var (
	ErrStickerDocumentInvalid = errors.New("sticker document invalid")
	ErrStickerFileTooLarge    = errors.New("sticker file too large")
	ErrStickerSetLimit        = errors.New("sticker set limit exceeded")
	ErrStickerSetInstallLimit = errors.New("installed sticker set limit exceeded")
	ErrStickerSetNameOccupied = errors.New("sticker set short name occupied")
	ErrStickerFileAlreadyUsed = errors.New("sticker file already belongs to a set")
	ErrStickerSetNotFound     = errors.New("sticker set not found")
	ErrStickerSetEmpty        = errors.New("sticker set must retain at least one sticker")
)

// UserStickerSet is an account-created sticker set. Its items are ordered by
// Position and contain only server-validated format metadata.
type UserStickerSet struct {
	ID              int64
	AccessHash      int64
	CreatorID       int64
	Title           string
	ShortName       string
	Software        string
	Kind            string
	TextColor       bool
	ItemCount       int
	Hash            int
	IsLive          bool
	CreatedAt       time.Time
	Items           []UserStickerSetItem
	Thumbnail       *UserStickerSetThumbnail
	ThumbVersion    int
	ThumbDocumentID int64
}

// UserStickerSetItem is one stored document in an account-created set.
type UserStickerSetItem struct {
	Position   int
	FileID     int64
	AccessHash int64
	Size       int64
	Date       time.Time
	Emoji      string
	Keywords   string
	Format     string
	Width      int
	Height     int
	Duration   int64
	MaskCoords string
}

// UserStickerSetThumbnail is a validated uploaded preview for a regular or
// mask sticker set.
type UserStickerSetThumbnail struct {
	FileID     int64
	AccessHash int64
	Size       int64
	Format     string
	Width      int
	Height     int
	Duration   int64
}

// UserStickerSetItemUpdate contains only fields present in stickers.changeSticker.
type UserStickerSetItemUpdate struct {
	Emoji      *string
	Keywords   *string
	MaskCoords *string
}

// StickerFileRef identifies a client-supplied file reference that must match
// both the owner's uploaded file and its access hash.
type StickerFileRef struct {
	ID         int64
	AccessHash int64
}

// StickerFilesForSet resolves all requested documents in one query. Files
// already assigned to a set are rejected here, before callers read blob bytes.
func (s *Store) StickerFilesForSet(ctx context.Context, ownerID int64, refs []StickerFileRef) ([]File, error) {
	if ownerID <= 0 || len(refs) == 0 || s.withoutBlobStore {
		return nil, ErrStickerDocumentInvalid
	}
	fileIDs := make([]int64, len(refs))
	accessHashes := make([]int64, len(refs))
	seen := make(map[int64]struct{}, len(refs))
	for i, ref := range refs {
		if ref.ID <= 0 || ref.AccessHash == 0 {
			return nil, ErrStickerDocumentInvalid
		}
		if _, ok := seen[ref.ID]; ok {
			return nil, ErrStickerDocumentInvalid
		}
		seen[ref.ID] = struct{}{}
		fileIDs[i] = ref.ID
		accessHashes[i] = ref.AccessHash
	}
	rows, err := s.q.StickerFilesForOwner(ctx, db.StickerFilesForOwnerParams{
		FileIds: fileIDs, AccessHashes: accessHashes, UploaderID: ownerID,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve sticker file rows: %w", err)
	}
	files := make([]File, len(rows))
	for i, row := range rows {
		files[i] = fileFromRow(row)
	}
	return files, nil
}

// StickerFilesForUserSet preflights one addStickerToSet document. A removed
// item may be reactivated in its original set, but active items and files
// assigned to any other set remain unavailable.
func (s *Store) StickerFilesForUserSet(ctx context.Context, ownerID, setID int64, refs []StickerFileRef) ([]File, error) {
	if ownerID <= 0 || setID <= 0 || len(refs) == 0 || s.withoutBlobStore {
		return nil, ErrStickerDocumentInvalid
	}
	fileIDs := make([]int64, len(refs))
	accessHashes := make([]int64, len(refs))
	seen := make(map[int64]struct{}, len(refs))
	for i, ref := range refs {
		if ref.ID <= 0 || ref.AccessHash == 0 {
			return nil, ErrStickerDocumentInvalid
		}
		if _, ok := seen[ref.ID]; ok {
			return nil, ErrStickerDocumentInvalid
		}
		seen[ref.ID] = struct{}{}
		fileIDs[i] = ref.ID
		accessHashes[i] = ref.AccessHash
	}
	rows, err := s.q.StickerFilesForUserSet(ctx, db.StickerFilesForUserSetParams{
		FileIds: fileIDs, AccessHashes: accessHashes, UploaderID: ownerID, SetID: setID,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve user sticker set file rows: %w", err)
	}
	files := make([]File, len(rows))
	for i, row := range rows {
		files[i] = fileFromRow(row)
	}
	return files, nil
}

// ReadStickerFileForSet reads bytes only after StickerFilesForSet preflights
// the full request. The blob key is always derived from the stored row.
func (s *Store) ReadStickerFileForSet(ctx context.Context, file File) ([]byte, error) {
	if file.ID <= 0 || !file.Stored || file.Size <= 0 || s.withoutBlobStore {
		return nil, ErrStickerDocumentInvalid
	}
	if file.Size > MaxStoredStickerBytes {
		return nil, ErrStickerFileTooLarge
	}
	data, err := s.blobs.ReadAt(ctx, blob.Key(file.ID), 0, file.Size)
	if err != nil {
		return nil, fmt.Errorf("read stored sticker bytes: %w", err)
	}
	if int64(len(data)) != file.Size {
		return nil, ErrStickerDocumentInvalid
	}
	return data, nil
}

// CreateUserStickerSet uses the M17 file-reference interlock before adding any
// set item. The per-owner row serializes the account limit; file locks are
// acquired in ascending order and are the final lock class in this write.
func (s *Store) CreateUserStickerSet(ctx context.Context, set UserStickerSet, items []UserStickerSetItem) (UserStickerSet, error) {
	maxItems := MaxUserStickerCount
	if set.Kind == "emojis" {
		maxItems = MaxUserEmojiCount
	} else if set.Kind != "regular" && set.Kind != "mask" {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	if set.CreatorID <= 0 || len(items) == 0 || len(items) > maxItems {
		if len(items) > maxItems {
			return UserStickerSet{}, ErrStickerSetLimit
		}
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	fileIDs := make([]int64, 0, len(items))
	accessHashes := make([]int64, 0, len(items))
	for _, item := range items {
		if item.FileID <= 0 || item.AccessHash == 0 || item.Size <= 0 || item.Size > MaxStoredStickerBytes ||
			item.Width <= 0 || item.Width > 512 || item.Height <= 0 || item.Height > 512 || item.Duration < 0 {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		}
		fileIDs = append(fileIDs, item.FileID)
		accessHashes = append(accessHashes, item.AccessHash)
	}
	if len(ascendingUnique(fileIDs)) != len(fileIDs) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin user sticker set: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if _, err = qtx.LockUserStickerSetOwner(ctx, set.CreatorID); errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	} else if err != nil {
		return UserStickerSet{}, fmt.Errorf("lock sticker set owner: %w", err)
	}
	count, err := qtx.UserCreatedStickerSetCount(ctx, set.CreatorID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("count sticker sets: %w", err)
	}
	if count >= MaxUserStickerSetsPerAccount {
		return UserStickerSet{}, ErrStickerSetLimit
	}
	if err = lockFileRefs(ctx, qtx, fileIDs...); err != nil {
		if errors.Is(err, ErrFileMissing) {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		}
		return UserStickerSet{}, err
	}
	// Recheck ownership, hashes, and file membership after the interlock. No file
	// row can be erased until this transaction commits.
	checkedFiles, err := qtx.StickerFilesForOwner(ctx, db.StickerFilesForOwnerParams{
		FileIds: fileIDs, UploaderID: set.CreatorID, AccessHashes: accessHashes,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("recheck sticker file references: %w", err)
	}
	if len(checkedFiles) != len(items) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}

	accessHash, err := newAccessHash()
	if err != nil {
		return UserStickerSet{}, err
	}
	row, err := qtx.CreateUserCreatedStickerSet(ctx, db.CreateUserCreatedStickerSetParams{
		AccessHash: accessHash,
		CreatorID:  set.CreatorID,
		Title:      set.Title,
		ShortName:  set.ShortName,
		Software:   set.Software,
		Kind:       set.Kind,
		TextColor:  set.TextColor,
		ItemCount:  int32(len(items)), //nolint:gosec // Sticker sets are capped at 200 items.
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "user_created_sticker_sets_short_name_key" {
			return UserStickerSet{}, ErrStickerSetNameOccupied
		}
		return UserStickerSet{}, fmt.Errorf("insert user sticker set: %w", err)
	}
	set = userStickerSetFromRow(row)
	set.Items = make([]UserStickerSetItem, 0, len(items))
	for position, item := range items {
		var maskCoords []byte
		if item.MaskCoords != "" {
			maskCoords = []byte(item.MaskCoords)
		}
		rows, insertErr := qtx.InsertUserCreatedStickerSetItem(ctx, db.InsertUserCreatedStickerSetItemParams{
			SetID:      set.ID,
			Position:   int32(position),
			FileID:     item.FileID,
			Emoji:      item.Emoji,
			Keywords:   item.Keywords,
			Format:     item.Format,
			Width:      int32(item.Width),  //nolint:gosec // Dimensions are validated as <=512 before the transaction.
			Height:     int32(item.Height), //nolint:gosec // Dimensions are validated as <=512 before the transaction.
			DurationNs: item.Duration,
			MaskCoords: maskCoords,
		})
		if insertErr != nil {
			return UserStickerSet{}, fmt.Errorf("insert sticker set item: %w", insertErr)
		}
		if rows != 1 {
			return UserStickerSet{}, ErrStickerFileAlreadyUsed
		}
		item.Position = position
		set.Items = append(set.Items, item)
	}
	set.Hash = userStickerSetHash(set)
	updated, err := qtx.UpdateUserCreatedStickerSetHash(ctx, db.UpdateUserCreatedStickerSetHashParams{
		ID: set.ID, SetHash: int32(set.Hash), //nolint:gosec // Sticker hashes are constrained to positive signed 31-bit values.
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("update user sticker set hash: %w", err)
	}
	if updated != 1 {
		return UserStickerSet{}, fmt.Errorf("update user sticker set hash: updated %d rows, want 1", updated)
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit user sticker set: %w", err)
	}
	return set, nil
}

// AddUserStickerSetItem appends a validated owner upload to a live set. It
// repeats file ownership checks after taking the M17 interlock and serializes
// count and order changes on the set row.
func (s *Store) AddUserStickerSetItem(ctx context.Context, ownerID, setID int64, item UserStickerSetItem) (UserStickerSet, error) {
	if ownerID <= 0 || setID <= 0 || !validStoredStickerItem(item) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin add sticker set item: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := qtx.LockOwnedLiveUserStickerSet(ctx, db.LockOwnedLiveUserStickerSetParams{ID: setID, CreatorID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("lock sticker set for add: %w", err)
	}
	set := userStickerSetFromRow(row)
	maxItems := MaxUserStickerCount
	if set.Kind == "emojis" {
		maxItems = MaxUserEmojiCount
	} else if set.Kind != "regular" && set.Kind != "mask" {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	mutationRows, err := qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items before add: %w", err)
	}
	activeIDs := activeStickerItemIDs(mutationRows)
	if len(activeIDs) >= maxItems {
		return UserStickerSet{}, ErrStickerSetLimit
	}
	if err = lockFileRefs(ctx, qtx, item.FileID); errors.Is(err, ErrFileMissing) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	} else if err != nil {
		return UserStickerSet{}, err
	}
	files, err := qtx.StickerFilesForUserSet(ctx, db.StickerFilesForUserSetParams{
		FileIds: []int64{item.FileID}, UploaderID: ownerID,
		AccessHashes: []int64{item.AccessHash}, SetID: set.ID,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("recheck sticker file before add: %w", err)
	}
	if len(files) != 1 || files[0].Size != item.Size {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	var removedItem bool
	for _, existing := range mutationRows {
		if existing.FileID == item.FileID {
			if existing.IsActive {
				return UserStickerSet{}, ErrStickerFileAlreadyUsed
			}
			removedItem = true
			break
		}
	}
	maskCoords := []byte(nil)
	if item.MaskCoords != "" {
		maskCoords = []byte(item.MaskCoords)
	}
	if removedItem {
		rows, updateErr := qtx.ReactivateUserCreatedStickerSetItem(ctx, db.ReactivateUserCreatedStickerSetItemParams{
			SetID: set.ID, FileID: item.FileID, Emoji: item.Emoji, Keywords: item.Keywords,
			Format:     item.Format,
			Width:      int32(item.Width),  //nolint:gosec // validStoredStickerItem bounds dimensions before this transaction.
			Height:     int32(item.Height), //nolint:gosec // validStoredStickerItem bounds dimensions before this transaction.
			DurationNs: item.Duration, MaskCoords: maskCoords,
		})
		if updateErr != nil {
			return UserStickerSet{}, fmt.Errorf("reactivate removed sticker set item: %w", updateErr)
		}
		if rows != 1 {
			return UserStickerSet{}, fmt.Errorf("reactivate removed sticker set item: updated %d rows, want 1", rows)
		}
	} else {
		if len(mutationRows) >= int(^uint32(0)>>1) {
			return UserStickerSet{}, ErrStickerSetLimit
		}
		rows, insertErr := qtx.InsertUserCreatedStickerSetItem(ctx, db.InsertUserCreatedStickerSetItemParams{
			SetID:    set.ID,
			Position: int32(len(mutationRows)), //nolint:gosec // The length is checked against MaxInt32 immediately above.
			FileID:   item.FileID,
			Emoji:    item.Emoji, Keywords: item.Keywords, Format: item.Format,
			Width:      int32(item.Width),  //nolint:gosec // validStoredStickerItem bounds dimensions before this transaction.
			Height:     int32(item.Height), //nolint:gosec // validStoredStickerItem bounds dimensions before this transaction.
			DurationNs: item.Duration,
			MaskCoords: maskCoords,
		})
		if insertErr != nil {
			return UserStickerSet{}, fmt.Errorf("insert sticker set item: %w", insertErr)
		}
		if rows != 1 {
			return UserStickerSet{}, ErrStickerFileAlreadyUsed
		}
	}
	mutationRows, err = qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items after add: %w", err)
	}
	activeIDs = append(activeIDs, item.FileID)
	if err = compactUserStickerSetItemPositions(ctx, qtx, set.ID, activeIDs, mutationRows); err != nil {
		return UserStickerSet{}, err
	}
	set, err = activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	if err = updateUserStickerSetCountAndHash(ctx, qtx, &set); err != nil {
		return UserStickerSet{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit add sticker set item: %w", err)
	}
	return set, nil
}

// RemoveUserStickerSetItem makes a sticker unavailable for future sends but
// keeps its row as a reference for messages that were sent before removal.
func (s *Store) RemoveUserStickerSetItem(ctx context.Context, ownerID, fileID, accessHash int64) (UserStickerSet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin remove sticker set item: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSetByFile(ctx, qtx, ownerID, fileID, accessHash)
	if err != nil {
		return UserStickerSet{}, err
	}
	set := userStickerSetFromRow(row)
	mutationRows, err := qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items before remove: %w", err)
	}
	activeIDs := activeStickerItemIDs(mutationRows)
	if len(activeIDs) <= 1 {
		return UserStickerSet{}, ErrStickerSetEmpty
	}
	index := stickerItemIDIndex(activeIDs, fileID)
	if index < 0 {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	rows, err := qtx.UpdateUserCreatedStickerSetItemActive(ctx, db.UpdateUserCreatedStickerSetItemActiveParams{
		SetID: set.ID, FileID: fileID, IsActive: false,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("remove sticker set item: %w", err)
	}
	if rows != 1 {
		return UserStickerSet{}, fmt.Errorf("remove sticker set item: updated %d rows, want 1", rows)
	}
	activeIDs = append(activeIDs[:index], activeIDs[index+1:]...)
	mutationRows, err = qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items after remove: %w", err)
	}
	if err = compactUserStickerSetItemPositions(ctx, qtx, set.ID, activeIDs, mutationRows); err != nil {
		return UserStickerSet{}, err
	}
	set, err = activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	if err = updateUserStickerSetCountAndHash(ctx, qtx, &set); err != nil {
		return UserStickerSet{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit remove sticker set item: %w", err)
	}
	return set, nil
}

// ChangeUserStickerSetItemPosition moves one live item to its requested
// zero-based position while preserving removed items as historical references.
func (s *Store) ChangeUserStickerSetItemPosition(ctx context.Context, ownerID, fileID, accessHash int64, position int) (UserStickerSet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin move sticker set item: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSetByFile(ctx, qtx, ownerID, fileID, accessHash)
	if err != nil {
		return UserStickerSet{}, err
	}
	set := userStickerSetFromRow(row)
	mutationRows, err := qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items before move: %w", err)
	}
	activeIDs := activeStickerItemIDs(mutationRows)
	if position < 0 || position >= len(activeIDs) {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	current := stickerItemIDIndex(activeIDs, fileID)
	if current < 0 {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	if current != position {
		activeIDs = append(activeIDs[:current], activeIDs[current+1:]...)
		activeIDs = append(activeIDs, 0)
		copy(activeIDs[position+1:], activeIDs[position:])
		activeIDs[position] = fileID
		if err = compactUserStickerSetItemPositions(ctx, qtx, set.ID, activeIDs, mutationRows); err != nil {
			return UserStickerSet{}, err
		}
	}
	set, err = activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	if current != position {
		if err = updateUserStickerSetCountAndHash(ctx, qtx, &set); err != nil {
			return UserStickerSet{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit move sticker set item: %w", err)
	}
	return set, nil
}

// ChangeUserStickerSetItem updates only the metadata fields present in the
// request; its set row lock serializes the edit with send and delete operations.
func (s *Store) ChangeUserStickerSetItem(ctx context.Context, ownerID, fileID, accessHash int64, update UserStickerSetItemUpdate) (UserStickerSet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin change sticker set item: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSetByFile(ctx, qtx, ownerID, fileID, accessHash)
	if err != nil {
		return UserStickerSet{}, err
	}
	set := userStickerSetFromRow(row)
	mutationRows, err := qtx.UserCreatedStickerSetItemsForMutation(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load sticker set items before change: %w", err)
	}
	index := mutationStickerItemIndex(mutationRows, fileID)
	if index < 0 || !mutationRows[index].IsActive {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	item := userStickerSetItemFromMutation(mutationRows[index])
	if update.Emoji != nil {
		item.Emoji = *update.Emoji
	}
	if update.Keywords != nil {
		item.Keywords = *update.Keywords
	}
	if update.MaskCoords != nil {
		item.MaskCoords = *update.MaskCoords
	}
	maskCoords := []byte(nil)
	if item.MaskCoords != "" {
		maskCoords = []byte(item.MaskCoords)
	}
	rows, err := qtx.UpdateUserCreatedStickerSetItemDetails(ctx, db.UpdateUserCreatedStickerSetItemDetailsParams{
		SetID: set.ID, FileID: fileID, Emoji: item.Emoji, Keywords: item.Keywords, MaskCoords: maskCoords,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("update sticker set item metadata: %w", err)
	}
	if rows != 1 {
		return UserStickerSet{}, fmt.Errorf("update sticker set item metadata: updated %d rows, want 1", rows)
	}
	set, err = activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	if err = updateUserStickerSetCountAndHash(ctx, qtx, &set); err != nil {
		return UserStickerSet{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit change sticker set item: %w", err)
	}
	return set, nil
}

// UserStickerSetByOwnedSticker resolves the caller's live set before API
// validation uses its sticker kind.
func (s *Store) UserStickerSetByOwnedSticker(ctx context.Context, ownerID, fileID, accessHash int64) (UserStickerSet, error) {
	if ownerID <= 0 || fileID <= 0 || accessHash == 0 {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	row, err := s.q.UserCreatedStickerSetByOwnedSticker(ctx, db.UserCreatedStickerSetByOwnedStickerParams{
		CreatorID: ownerID, FileID: fileID, AccessHash: accessHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("resolve owned sticker set: %w", err)
	}
	return s.userStickerSetWithItems(ctx, userStickerSetFromRow(row))
}

// RenameUserStickerSet changes the title of a caller-owned live set and stores
// a new list hash before returning the full owner view.
func (s *Store) RenameUserStickerSet(ctx context.Context, ownerID, setID int64, title string) (UserStickerSet, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin rename sticker set: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSet(ctx, qtx, ownerID, setID)
	if err != nil {
		return UserStickerSet{}, err
	}
	set, err := activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	set.Title = title
	set.Hash = userStickerSetHash(set)
	rows, err := qtx.RenameUserCreatedStickerSet(ctx, db.RenameUserCreatedStickerSetParams{
		Title: set.Title, SetHash: int32(set.Hash), //nolint:gosec // userStickerSetHash constrains values to positive signed 31-bit.
		ID: set.ID,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("rename user sticker set: %w", err)
	}
	if rows != 1 {
		return UserStickerSet{}, fmt.Errorf("rename user sticker set: updated %d rows, want 1", rows)
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit rename sticker set: %w", err)
	}
	return set, nil
}

// SetUserStickerSetThumb replaces or clears a user-uploaded set thumbnail, or
// selects an item as the custom-emoji set thumbnail.
func (s *Store) SetUserStickerSetThumb(ctx context.Context, ownerID, setID int64, thumbnail *UserStickerSetThumbnail, thumbDocumentID int64) (UserStickerSet, error) {
	if ownerID <= 0 || setID <= 0 || thumbDocumentID < 0 {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("begin set sticker set thumbnail: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSet(ctx, qtx, ownerID, setID)
	if err != nil {
		return UserStickerSet{}, err
	}
	set := userStickerSetFromRow(row)
	if set.Kind == "emojis" {
		if thumbnail != nil {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		}
		if thumbDocumentID > 0 {
			found, existsErr := qtx.UserCreatedStickerSetItemExists(ctx, db.UserCreatedStickerSetItemExistsParams{
				SetID: set.ID, FileID: thumbDocumentID,
			})
			if existsErr != nil {
				return UserStickerSet{}, fmt.Errorf("check emoji set thumbnail item: %w", existsErr)
			}
			if !found {
				return UserStickerSet{}, ErrStickerDocumentInvalid
			}
		}
	} else if thumbDocumentID != 0 {
		return UserStickerSet{}, ErrStickerDocumentInvalid
	}
	if thumbnail != nil {
		maxThumbnailSize := int64(32 << 10)
		if thumbnail.Format == "WEBP" {
			maxThumbnailSize = 128 << 10
		}
		if set.Kind == "emojis" || thumbnail.FileID <= 0 || thumbnail.AccessHash == 0 ||
			thumbnail.Size <= 0 || thumbnail.Size > maxThumbnailSize || thumbnail.Width != 100 || thumbnail.Height != 100 ||
			(thumbnail.Format != "WEBP" && thumbnail.Format != "TGS" && thumbnail.Format != "WEBM") {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		}
		if err = lockFileRefs(ctx, qtx, thumbnail.FileID); errors.Is(err, ErrFileMissing) {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		} else if err != nil {
			return UserStickerSet{}, err
		}
		files, fileErr := qtx.StickerFilesForOwner(ctx, db.StickerFilesForOwnerParams{
			FileIds: []int64{thumbnail.FileID}, UploaderID: ownerID,
			AccessHashes: []int64{thumbnail.AccessHash},
		})
		if fileErr != nil {
			return UserStickerSet{}, fmt.Errorf("recheck sticker thumbnail file: %w", fileErr)
		}
		if len(files) != 1 || files[0].Size != thumbnail.Size {
			return UserStickerSet{}, ErrStickerDocumentInvalid
		}
	}
	oldFileID := int64(0)
	if set.Thumbnail != nil {
		oldFileID = set.Thumbnail.FileID
	}
	changed := oldFileID != thumbnailFileID(thumbnail) || set.ThumbDocumentID != thumbDocumentID
	if changed {
		if set.ThumbVersion >= int(^uint32(0)>>1) {
			return UserStickerSet{}, errors.New("sticker set thumbnail version exhausted")
		}
	}
	set.Thumbnail = thumbnail
	set.ThumbDocumentID = thumbDocumentID
	set, err = activeUserStickerSet(ctx, qtx, row)
	if err != nil {
		return UserStickerSet{}, err
	}
	set.Thumbnail = thumbnail
	set.ThumbDocumentID = thumbDocumentID
	if changed {
		set.ThumbVersion++
	}
	set.Hash = userStickerSetHash(set)
	var fileID *int64
	var format *string
	var size *int64
	var width, height *int32
	var duration int64
	if thumbnail != nil {
		fileID = &thumbnail.FileID
		format = &thumbnail.Format
		size = &thumbnail.Size
		thumbWidth := int32(thumbnail.Width)   //nolint:gosec // Thumbnail dimensions were validated as exactly 100x100.
		thumbHeight := int32(thumbnail.Height) //nolint:gosec // Thumbnail dimensions were validated as exactly 100x100.
		width, height = &thumbWidth, &thumbHeight
		duration = thumbnail.Duration
	}
	rows, err := qtx.UpdateUserCreatedStickerSetThumb(ctx, db.UpdateUserCreatedStickerSetThumbParams{
		ThumbFileID: fileID, ThumbFormat: format, ThumbSize: size, ThumbWidth: width,
		ThumbHeight: height, ThumbDurationNs: duration,
		ThumbVersion:    int32(set.ThumbVersion),                   //nolint:gosec // The old version is bounded before its single increment.
		ThumbDocumentID: thumbDocumentID, SetHash: int32(set.Hash), //nolint:gosec // userStickerSetHash constrains values to positive signed 31-bit.
		ID: set.ID,
	})
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("update user sticker set thumbnail: %w", err)
	}
	if rows != 1 {
		return UserStickerSet{}, fmt.Errorf("update user sticker set thumbnail: updated %d rows, want 1", rows)
	}
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit set sticker set thumbnail: %w", err)
	}
	return set, nil
}

// DeleteUserStickerSet marks a set unavailable and removes all install rows.
// Item and thumbnail references stay in place so sent messages and erasure
// accounting retain their files.
func (s *Store) DeleteUserStickerSet(ctx context.Context, ownerID, setID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete sticker set: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	row, err := lockOwnedStickerSet(ctx, qtx, ownerID, setID)
	if err != nil {
		return err
	}
	changed, err := qtx.DeleteUserCreatedStickerSet(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("mark user sticker set deleted: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("mark user sticker set deleted: updated %d rows, want 1", changed)
	}
	if _, err = qtx.DeleteInstalledUserStickerSet(ctx, row.ID); err != nil {
		return fmt.Errorf("remove deleted sticker set installs: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete sticker set: %w", err)
	}
	return nil
}

// UserStickerSetThumbnailForDownload resolves the current live thumbnail using
// the set's public ID/access hash and version, never a client-supplied file ID.
func (s *Store) UserStickerSetThumbnailForDownload(ctx context.Context, setID, accessHash int64, version int) (File, error) {
	if setID <= 0 || accessHash == 0 || version <= 0 || version > int(^uint32(0)>>1) {
		return File{}, ErrStickerSetNotFound
	}
	row, err := s.q.UserCreatedStickerSetThumbnailForDownload(ctx, db.UserCreatedStickerSetThumbnailForDownloadParams{
		ID: setID, AccessHash: accessHash, ThumbVersion: int32(version),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, ErrStickerSetNotFound
	}
	if err != nil {
		return File{}, fmt.Errorf("resolve sticker set thumbnail download: %w", err)
	}
	return fileFromRow(row), nil
}

func validStoredStickerItem(item UserStickerSetItem) bool {
	return item.FileID > 0 && item.AccessHash != 0 && item.Size > 0 && item.Size <= MaxStoredStickerBytes &&
		item.Width > 0 && item.Width <= 512 && item.Height > 0 && item.Height <= 512 && item.Duration >= 0 &&
		(item.Format == "WEBP" || item.Format == "TGS" || item.Format == "WEBM")
}

func lockOwnedStickerSet(ctx context.Context, qtx *db.Queries, ownerID, setID int64) (db.UserCreatedStickerSet, error) {
	if ownerID <= 0 || setID <= 0 {
		return db.UserCreatedStickerSet{}, ErrStickerSetNotFound
	}
	row, err := qtx.LockOwnedLiveUserStickerSet(ctx, db.LockOwnedLiveUserStickerSetParams{ID: setID, CreatorID: ownerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.UserCreatedStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return db.UserCreatedStickerSet{}, fmt.Errorf("lock owned live sticker set: %w", err)
	}
	return row, nil
}

func lockOwnedStickerSetByFile(ctx context.Context, qtx *db.Queries, ownerID, fileID, accessHash int64) (db.UserCreatedStickerSet, error) {
	if ownerID <= 0 || fileID <= 0 || accessHash == 0 {
		return db.UserCreatedStickerSet{}, ErrStickerSetNotFound
	}
	row, err := qtx.LockOwnedLiveUserStickerSetBySticker(ctx, db.LockOwnedLiveUserStickerSetByStickerParams{
		CreatorID: ownerID, FileID: fileID, AccessHash: accessHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.UserCreatedStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return db.UserCreatedStickerSet{}, fmt.Errorf("lock set for owned sticker edit: %w", err)
	}
	return row, nil
}

func activeUserStickerSet(ctx context.Context, qtx *db.Queries, row db.UserCreatedStickerSet) (UserStickerSet, error) {
	return userStickerSetWithItemsQuery(ctx, qtx, userStickerSetFromRow(row))
}

func updateUserStickerSetCountAndHash(ctx context.Context, qtx *db.Queries, set *UserStickerSet) error {
	rows, err := qtx.UserCreatedStickerSetItems(ctx, set.ID)
	if err != nil {
		return fmt.Errorf("reload active sticker set items: %w", err)
	}
	set.Items = make([]UserStickerSetItem, 0, len(rows))
	for _, row := range rows {
		set.Items = append(set.Items, UserStickerSetItem{
			Position: int(row.Position), FileID: row.FileID, AccessHash: row.AccessHash,
			Size: row.Size, Date: row.Date.Time, Emoji: row.Emoji, Keywords: row.Keywords,
			Format: row.Format, Width: int(row.Width), Height: int(row.Height),
			Duration: row.DurationNs, MaskCoords: row.MaskCoords,
		})
	}
	set.ItemCount = len(set.Items)
	set.Hash = userStickerSetHash(*set)
	updated, err := qtx.UpdateUserCreatedStickerSetCountAndHash(ctx, db.UpdateUserCreatedStickerSetCountAndHashParams{
		ID:        set.ID,
		ItemCount: int32(set.ItemCount), //nolint:gosec // The active sticker count is capped at MaxUserEmojiCount.
		SetHash:   int32(set.Hash),      //nolint:gosec // userStickerSetHash constrains values to positive signed 31-bit.
	})
	if err != nil {
		return fmt.Errorf("update sticker set count and hash: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("update sticker set count and hash: updated %d rows, want 1", updated)
	}
	return nil
}

func activeStickerItemIDs(rows []db.UserCreatedStickerSetItemsForMutationRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.IsActive {
			ids = append(ids, row.FileID)
		}
	}
	return ids
}

func mutationStickerItemIndex(rows []db.UserCreatedStickerSetItemsForMutationRow, fileID int64) int {
	for i, row := range rows {
		if row.FileID == fileID {
			return i
		}
	}
	return -1
}

func stickerItemIDIndex(ids []int64, fileID int64) int {
	for i, id := range ids {
		if id == fileID {
			return i
		}
	}
	return -1
}

func userStickerSetItemFromMutation(row db.UserCreatedStickerSetItemsForMutationRow) UserStickerSetItem {
	return UserStickerSetItem{
		Position: int(row.Position), FileID: row.FileID, AccessHash: row.AccessHash,
		Size: row.Size, Date: row.Date.Time, Emoji: row.Emoji, Keywords: row.Keywords,
		Format: row.Format, Width: int(row.Width), Height: int(row.Height),
		Duration: row.DurationNs, MaskCoords: row.MaskCoords,
	}
}

func compactUserStickerSetItemPositions(
	ctx context.Context,
	qtx *db.Queries,
	setID int64,
	activeIDs []int64,
	rows []db.UserCreatedStickerSetItemsForMutationRow,
) error {
	if len(rows) == 0 {
		return errors.New("cannot compact an empty sticker set")
	}
	ordered := make([]int64, 0, len(rows))
	seen := make(map[int64]struct{}, len(rows))
	for _, id := range activeIDs {
		index := mutationStickerItemIndex(rows, id)
		if index < 0 || !rows[index].IsActive {
			return fmt.Errorf("active sticker order contains missing item %d", id)
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("active sticker order repeats item %d", id)
		}
		seen[id] = struct{}{}
		ordered = append(ordered, id)
	}
	for _, row := range rows {
		if row.IsActive {
			if _, ok := seen[row.FileID]; !ok {
				return fmt.Errorf("active sticker order omits item %d", row.FileID)
			}
			continue
		}
		ordered = append(ordered, row.FileID)
	}
	var maxPosition int64
	for _, row := range rows {
		if int64(row.Position) > maxPosition {
			maxPosition = int64(row.Position)
		}
	}
	shift := maxPosition + int64(len(rows)) + 1
	if maxPosition+shift > int64(1<<31-1) {
		return errors.New("sticker set position range exhausted")
	}
	shifted, err := qtx.ShiftUserCreatedStickerSetItemPositions(ctx, db.ShiftUserCreatedStickerSetItemPositionsParams{
		ShiftBy: int32(shift), //nolint:gosec // The signed 32-bit position range is checked immediately above.
		SetID:   setID,
	})
	if err != nil {
		return fmt.Errorf("move sticker set items to temporary positions: %w", err)
	}
	if shifted != int64(len(rows)) {
		return fmt.Errorf("move sticker set items: shifted %d rows, want %d", shifted, len(rows))
	}
	for position, fileID := range ordered {
		updated, updateErr := qtx.SetUserCreatedStickerSetItemPosition(ctx, db.SetUserCreatedStickerSetItemPositionParams{
			SetID: setID, FileID: fileID, Position: int32(position),
		})
		if updateErr != nil {
			return fmt.Errorf("set sticker set item position: %w", updateErr)
		}
		if updated != 1 {
			return fmt.Errorf("set sticker set item position: updated %d rows, want 1", updated)
		}
	}
	return nil
}

func thumbnailFileID(thumbnail *UserStickerSetThumbnail) int64 {
	if thumbnail == nil {
		return 0
	}
	return thumbnail.FileID
}

func userStickerSetFromRow(row db.UserCreatedStickerSet) UserStickerSet {
	set := UserStickerSet{
		ID: row.ID, AccessHash: row.AccessHash, CreatorID: row.CreatorID,
		Title: row.Title, ShortName: row.ShortName, Software: row.Software,
		Kind: row.Kind, TextColor: row.TextColor, ItemCount: int(row.ItemCount),
		Hash: int(row.SetHash), IsLive: row.IsLive, ThumbVersion: int(row.ThumbVersion),
		ThumbDocumentID: row.ThumbDocumentID,
		CreatedAt:       row.CreatedAt.Time,
	}
	if row.ThumbFileID != nil {
		set.Thumbnail = &UserStickerSetThumbnail{
			FileID:   *row.ThumbFileID,
			Format:   *row.ThumbFormat,
			Size:     *row.ThumbSize,
			Width:    int(*row.ThumbWidth),
			Height:   int(*row.ThumbHeight),
			Duration: row.ThumbDurationNs,
		}
	}
	return set
}

func userStickerSetHash(set UserStickerSet) int {
	h := crc32.NewIEEE()
	var id [8]byte
	writeInt := func(value int64) {
		binary.BigEndian.PutUint64(id[:], uint64(value)) //nolint:gosec // Sticker values are non-negative or opaque 64-bit identifiers.
		_, _ = h.Write(id[:])
	}
	writeString := func(value string) {
		writeInt(int64(len(value)))
		_, _ = h.Write([]byte(value))
	}
	writeInt(set.ID)
	writeString(set.Title)
	writeString(set.ShortName)
	writeString(set.Kind)
	if set.TextColor {
		writeInt(1)
	} else {
		writeInt(0)
	}
	writeInt(int64(set.ThumbVersion))
	writeInt(set.ThumbDocumentID)
	if set.Thumbnail != nil {
		writeInt(set.Thumbnail.FileID)
		writeInt(set.Thumbnail.Size)
		writeString(set.Thumbnail.Format)
		writeInt(int64(set.Thumbnail.Width))
		writeInt(int64(set.Thumbnail.Height))
		writeInt(set.Thumbnail.Duration)
	} else {
		writeInt(0)
	}
	for _, item := range set.Items {
		writeInt(int64(item.Position))
		writeInt(item.FileID)
		writeInt(item.AccessHash)
		writeString(item.Emoji)
		writeString(item.Keywords)
		writeString(item.Format)
		writeInt(int64(item.Width))
		writeInt(int64(item.Height))
		writeInt(item.Duration)
		writeString(item.MaskCoords)
	}
	value := int(h.Sum32() & 0x7fffffff)
	if value == 0 {
		return 1
	}
	return value
}

// UserStickerSetByShortName resolves a live set using the shared case-folded
// namespace. Software is retained for the owner-facing internal record only.
func (s *Store) UserStickerSetByShortName(ctx context.Context, shortName string) (UserStickerSet, error) {
	row, err := s.q.UserCreatedStickerSetByShortName(ctx, shortName)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("lookup user sticker set by name: %w", err)
	}
	return s.userStickerSetWithItems(ctx, userStickerSetFromRow(row))
}

// UserStickerSetByID requires the exact access hash and a live set.
func (s *Store) UserStickerSetByID(ctx context.Context, id, accessHash int64) (UserStickerSet, error) {
	row, err := s.q.UserCreatedStickerSetByID(ctx, db.UserCreatedStickerSetByIDParams{ID: id, AccessHash: accessHash})
	if errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("lookup user sticker set by id: %w", err)
	}
	return s.userStickerSetWithItems(ctx, userStickerSetFromRow(row))
}

// UserStickerSetByInstalledID returns a live set for an account's install row.
func (s *Store) UserStickerSetByInstalledID(ctx context.Context, id int64) (UserStickerSet, error) {
	row, err := s.q.UserCreatedStickerSetByInstalledID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return UserStickerSet{}, ErrStickerSetNotFound
	}
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("lookup installed user sticker set: %w", err)
	}
	return s.userStickerSetWithItems(ctx, userStickerSetFromRow(row))
}

// UserStickerSetSummariesByInstalledIDs loads live installed user-set metadata
// in one bounded query without reading any item rows.
func (s *Store) UserStickerSetSummariesByInstalledIDs(ctx context.Context, ids []int64) (map[int64]UserStickerSet, error) {
	positiveIDs := make([]int64, 0, len(ids))
	for _, id := range ascendingUnique(ids) {
		if id > 0 {
			positiveIDs = append(positiveIDs, id)
		}
	}
	result := make(map[int64]UserStickerSet, len(positiveIDs))
	if len(positiveIDs) == 0 {
		return result, nil
	}

	rows, err := s.q.UserCreatedStickerSetsByInstalledIDs(ctx, positiveIDs)
	if err != nil {
		return nil, fmt.Errorf("lookup installed user sticker sets: %w", err)
	}
	for _, row := range rows {
		set := userStickerSetFromRow(row)
		result[set.ID] = set
	}
	return result, nil
}

func (s *Store) userStickerSetWithItems(ctx context.Context, set UserStickerSet) (UserStickerSet, error) {
	return userStickerSetWithItemsQuery(ctx, s.q, set)
}

func userStickerSetWithItemsQuery(ctx context.Context, q *db.Queries, set UserStickerSet) (UserStickerSet, error) {
	rows, err := q.UserCreatedStickerSetItems(ctx, set.ID)
	if err != nil {
		return UserStickerSet{}, fmt.Errorf("load user sticker set items: %w", err)
	}
	set.Items = make([]UserStickerSetItem, 0, len(rows))
	for _, row := range rows {
		set.Items = append(set.Items, UserStickerSetItem{
			Position: int(row.Position), FileID: row.FileID, AccessHash: row.AccessHash,
			Size: row.Size, Date: row.Date.Time, Emoji: row.Emoji, Keywords: row.Keywords,
			Format: row.Format, Width: int(row.Width), Height: int(row.Height),
			Duration: row.DurationNs, MaskCoords: row.MaskCoords,
		})
	}
	return set, nil
}

// UserStickerSetsByFileIDs resolves message document identities through the
// retained set-item record, including removed items and deleted sets. It never
// turns arbitrary positive file ids into stickers.
func (s *Store) UserStickerSetsByFileIDs(ctx context.Context, fileIDs []int64) (map[int64]UserStickerDocument, error) {
	result := make(map[int64]UserStickerDocument, len(fileIDs))
	ids := ascendingUnique(fileIDs)
	if len(ids) == 0 {
		return result, nil
	}
	rows, err := s.q.UserCreatedStickerSetItemsByFileIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("resolve user sticker documents: %w", err)
	}
	for _, row := range rows {
		set := userStickerSetFromRow(db.UserCreatedStickerSet{
			ID: row.SetID, AccessHash: row.SetAccessHash, CreatorID: row.CreatorID,
			Title: row.Title, ShortName: row.ShortName, Software: row.Software,
			Kind: row.Kind, TextColor: row.TextColor, IsLive: row.IsLive,
			CreatedAt: row.CreatedAt,
		})
		result[row.FileID] = UserStickerDocument{
			Set: set,
			Item: UserStickerSetItem{
				Position: int(row.Position), FileID: row.FileID, AccessHash: row.FileAccessHash,
				Size: row.Size, Date: row.Date.Time, Emoji: row.Emoji, Keywords: row.Keywords,
				Format: row.Format, Width: int(row.Width), Height: int(row.Height),
				Duration: row.DurationNs, MaskCoords: row.MaskCoords,
			},
		}
	}
	return result, nil
}

// UserStickerDocument combines the public set identity and one media record.
type UserStickerDocument struct {
	Set  UserStickerSet
	Item UserStickerSetItem
}

// UserStickerDocumentExists matches a stored document only while a live set
// contains it and the supplied file access hash is exact.
func (s *Store) UserStickerDocumentExists(ctx context.Context, id, accessHash int64) (bool, error) {
	return s.q.UserStickerDocumentExists(ctx, db.UserStickerDocumentExistsParams{ID: id, AccessHash: accessHash})
}

// lockLiveUserStickerDocuments is the write-side liveness boundary for sends
// and forwards. It resolves every positive document through set membership and
// holds each owning live set FOR SHARE until the message transaction commits.
func lockLiveUserStickerDocuments(ctx context.Context, qtx *db.Queries, documentIDs ...int64) error {
	var ids []int64
	for _, id := range documentIDs {
		if id > 0 {
			ids = append(ids, id)
		}
	}
	ids = ascendingUnique(ids)
	if len(ids) == 0 {
		return nil
	}
	rows, err := qtx.UserStickerDocumentSetIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("resolve user sticker set membership: %w", err)
	}
	if len(rows) != len(ids) {
		return ErrMediaInvalid
	}
	var setIDs []int64
	for _, row := range rows {
		setIDs = append(setIDs, row.SetID)
	}
	for _, setID := range ascendingUnique(setIDs) {
		if _, err = qtx.LockLiveUserStickerSet(ctx, setID); errors.Is(err, pgx.ErrNoRows) {
			return ErrMediaInvalid
		} else if err != nil {
			return fmt.Errorf("lock live user sticker set: %w", err)
		}
	}
	// A remover can acquire the set row before this transaction and commit while
	// this send waits on FOR SHARE. Re-resolve after all locks so a removed item
	// cannot slip through on the pre-lock snapshot.
	rows, err = qtx.UserStickerDocumentSetIDs(ctx, ids)
	if err != nil {
		return fmt.Errorf("recheck user sticker set membership: %w", err)
	}
	if len(rows) != len(ids) {
		return ErrMediaInvalid
	}
	return nil
}

// UserStickerSetShortNameExists checks the full stored namespace, including
// sets whose serving state has been turned off.
func (s *Store) UserStickerSetShortNameExists(ctx context.Context, shortName string) (bool, error) {
	return s.q.UserStickerSetShortNameExists(ctx, shortName)
}

// UserStickerSetOccupiedShortNames returns the stored names matching any of
// the candidates, case-insensitively. Inactive sets remain reserved names.
func (s *Store) UserStickerSetOccupiedShortNames(ctx context.Context, shortNames []string) (map[string]struct{}, error) {
	occupied := make(map[string]struct{}, len(shortNames))
	if len(shortNames) == 0 {
		return occupied, nil
	}

	normalized := make([]string, len(shortNames))
	for i, shortName := range shortNames {
		normalized[i] = strings.ToLower(shortName)
	}
	names, err := s.q.UserStickerSetOccupiedShortNames(ctx, normalized)
	if err != nil {
		return nil, fmt.Errorf("read occupied sticker set short names: %w", err)
	}
	for _, name := range names {
		occupied[name] = struct{}{}
	}
	return occupied, nil
}
