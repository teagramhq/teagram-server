package store

import (
	"context"
	"errors"
	"fmt"
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
)

// UserStickerSet is an account-created sticker set. Its items are ordered by
// Position and contain only server-validated format metadata.
type UserStickerSet struct {
	ID         int64
	AccessHash int64
	CreatorID  int64
	Title      string
	ShortName  string
	Software   string
	Kind       string
	TextColor  bool
	IsLive     bool
	CreatedAt  time.Time
	Items      []UserStickerSetItem
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
	if err = tx.Commit(ctx); err != nil {
		return UserStickerSet{}, fmt.Errorf("commit user sticker set: %w", err)
	}
	return set, nil
}

func userStickerSetFromRow(row db.UserCreatedStickerSet) UserStickerSet {
	return UserStickerSet{
		ID: row.ID, AccessHash: row.AccessHash, CreatorID: row.CreatorID,
		Title: row.Title, ShortName: row.ShortName, Software: row.Software,
		Kind: row.Kind, TextColor: row.TextColor, IsLive: row.IsLive,
		CreatedAt: row.CreatedAt.Time,
	}
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

// UserStickerSetsByInstalledIDs loads live installed user sets and their
// ordered items in two bounded queries, independent of the number of IDs.
func (s *Store) UserStickerSetsByInstalledIDs(ctx context.Context, ids []int64) (map[int64]UserStickerSet, error) {
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
	setIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		set := userStickerSetFromRow(row)
		set.Items = []UserStickerSetItem{}
		result[set.ID] = set
		setIDs = append(setIDs, set.ID)
	}
	if len(setIDs) == 0 {
		return result, nil
	}

	itemRows, err := s.q.UserCreatedStickerSetItemsBySetIDs(ctx, setIDs)
	if err != nil {
		return nil, fmt.Errorf("load installed user sticker set items: %w", err)
	}
	for _, row := range itemRows {
		set := result[row.SetID]
		set.Items = append(set.Items, UserStickerSetItem{
			Position: int(row.Position), FileID: row.FileID, AccessHash: row.AccessHash,
			Size: row.Size, Date: row.Date.Time, Emoji: row.Emoji, Keywords: row.Keywords,
			Format: row.Format, Width: int(row.Width), Height: int(row.Height),
			Duration: row.DurationNs, MaskCoords: row.MaskCoords,
		})
		result[row.SetID] = set
	}
	return result, nil
}

func (s *Store) userStickerSetWithItems(ctx context.Context, set UserStickerSet) (UserStickerSet, error) {
	rows, err := s.q.UserCreatedStickerSetItems(ctx, set.ID)
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

// UserStickerSetsByFileIDs resolves message document identities strictly via
// live set membership. It never turns arbitrary positive file ids into stickers.
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
