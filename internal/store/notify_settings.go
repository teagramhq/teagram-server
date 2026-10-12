package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	NotifySettingsScopePeer       NotifySettingsScope = 1
	NotifySettingsScopeUsers      NotifySettingsScope = 2
	NotifySettingsScopeChats      NotifySettingsScope = 3
	NotifySettingsScopeBroadcasts NotifySettingsScope = 4

	maxPeerNotifySettings     = 1000
	notifySettingsRateSurface = "notify_settings_mutation"
	notifySettingsRateLimit   = 300
)

// NotifySettingsScope identifies one private peer or default-scope setting.
type NotifySettingsScope int16

// NotifySettingsKey identifies one owner-private peer or default scope.
type NotifySettingsKey struct {
	Scope    NotifySettingsScope
	PeerType PeerType
	PeerID   int64
}

// NotifySettingsValue contains only fields this server persists in this slice.
// Nil pointers preserve Telegram's tri-state fields; a non-nil zero mute_until
// is an explicit unmute and differs from an unset field.
type NotifySettingsValue struct {
	ShowPreviews *bool
	Silent       *bool
	MuteUntil    *int32
	Sound        *int16
}

// NotifySettingsException is one non-empty per-peer exception visible to an
// owner in the requested default-scope category.
type NotifySettingsException struct {
	PeerType PeerType
	PeerID   int64
	Value    NotifySettingsValue
}

// NotifySettingsChange is one current owner-private value and its recovery timestamp.
type NotifySettingsChange struct {
	Key       NotifySettingsKey
	Value     NotifySettingsValue
	ChangedAt time.Time
}

// NotifySettingsDifferencePosition is the strict ordering key for one page of
// the notification settings recovery stream.
type NotifySettingsDifferencePosition struct {
	ChangedAt time.Time
	Key       NotifySettingsKey
}

// NotifySettingsDifferenceCursor records the page issued to one auth key. The
// next request may continue only when its state matches State exactly.
type NotifySettingsDifferenceCursor struct {
	AuthKeyID int64
	UserID    int64
	Cutoff    time.Time
	// ResumeDate is the pre-acknowledgement date used by the other replay streams.
	ResumeDate int
	Last       NotifySettingsDifferencePosition
	State      State
}

var (
	ErrNotifySettingsPeerInvalid  = errors.New("notification settings peer invalid")
	ErrNotifySettingsTooMany      = errors.New("notification settings peer cap reached")
	ErrNotifySettingsRateLimited  = errors.New("notification settings rate limited")
	ErrNotifySettingsSoundInvalid = errors.New("notification settings sound invalid")
)

// UpdateNotifySettings stores one normalized owner-private value. Peer access,
// pruning, the mutation budget, the cap and the owner-only nudge share one
// transaction so a refused write has no partial state.
func (s *Store) UpdateNotifySettings(ctx context.Context, ownerID int64, key NotifySettingsKey, value NotifySettingsValue) (bool, error) {
	if ownerID <= 0 || !validNotifySettingsKey(key) {
		return false, ErrNotifySettingsPeerInvalid
	}
	if !validNotifySettingsValue(value) {
		return false, ErrNotifySettingsSoundInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin notification settings mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err = lockNotifySettingsKey(ctx, tx, qtx, ownerID, key); err != nil {
		return false, err
	}

	if err = qtx.PruneNotifySettingsForOwner(ctx, ownerID); err != nil {
		return false, fmt.Errorf("prune notification settings for owner: %w", err)
	}

	current, currentFound, err := readNotifySettings(ctx, qtx, ownerID, key)
	if err != nil {
		return false, err
	}
	_, err = qtx.TryConsumeRateLimitCost(ctx, db.TryConsumeRateLimitCostParams{
		SubjectID:      ownerID,
		Surface:        notifySettingsRateSurface,
		Cost:           1,
		WindowDuration: pgtype.Interval{Microseconds: time.Minute.Microseconds(), Valid: true},
		LimitCount:     notifySettingsRateLimit,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotifySettingsRateLimited
	} else if err != nil {
		return false, fmt.Errorf("consume notification settings rate limit: %w", err)
	}

	currentEmpty := !currentFound || emptyNotifySettings(current)
	requestedEmpty := emptyNotifySettings(value)
	if (currentEmpty && requestedEmpty) || (!currentEmpty && !requestedEmpty && sameNotifySettings(current, value)) {
		if err = tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit unchanged notification settings: %w", err)
		}
		return false, nil
	}

	if key.Scope == NotifySettingsScopePeer && !requestedEmpty && currentEmpty {
		count, countErr := qtx.CountActivePeerNotifySettings(ctx, ownerID)
		if countErr != nil {
			return false, fmt.Errorf("count peer notification settings: %w", countErr)
		}
		if count >= maxPeerNotifySettings {
			return false, ErrNotifySettingsTooMany
		}
	}

	stamp, err := qtx.NotifySettingsTimestamp(ctx)
	if err != nil {
		return false, fmt.Errorf("notification settings timestamp: %w", err)
	}
	if !stamp.Valid {
		return false, errors.New("notification settings timestamp: invalid timestamp")
	}
	if err = qtx.UpsertNotifySettings(ctx, db.UpsertNotifySettingsParams{
		OwnerID: ownerID, Scope: int16(key.Scope), PeerType: int16(key.PeerType), PeerID: key.PeerID,
		ShowPreviews: value.ShowPreviews, Silent: value.Silent, MuteUntil: value.MuteUntil,
		Sound: value.Sound, ChangedAt: stamp,
	}); err != nil {
		return false, fmt.Errorf("save notification settings: %w", err)
	}
	if err = qtx.NotifyNotifySettings(ctx, NotifySettingsNotificationPayload(ownerID, key)); err != nil {
		return false, fmt.Errorf("notify notification settings: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit notification settings: %w", err)
	}
	return true, nil
}

// GetNotifySettings reads one value only while its peer remains visible to the
// owner. A missing row and a cleared tombstone both render as empty settings.
func (s *Store) GetNotifySettings(ctx context.Context, ownerID int64, key NotifySettingsKey) (NotifySettingsValue, bool, error) {
	if ownerID <= 0 || !validNotifySettingsKey(key) {
		return NotifySettingsValue{}, false, ErrNotifySettingsPeerInvalid
	}
	row, err := s.q.NotifySettingsForVisibleKey(ctx, db.NotifySettingsForVisibleKeyParams{
		OwnerID: ownerID, Scope: int16(key.Scope), PeerType: int16(key.PeerType), PeerID: key.PeerID,
	})
	if err != nil {
		return NotifySettingsValue{}, false, fmt.Errorf("read notification settings: %w", err)
	}
	if !row.PeerVisible {
		return NotifySettingsValue{}, false, ErrNotifySettingsPeerInvalid
	}
	return NotifySettingsValue{
		ShowPreviews: row.ShowPreviews,
		Silent:       row.Silent,
		MuteUntil:    row.MuteUntil,
		Sound:        row.Sound,
	}, row.Found, nil
}

// NotifySettingsForVisiblePeers returns the owner's non-empty peer settings
// while the same peer-visibility predicate used by point reads holds. The
// settings cap bounds this batch to 1000 rows.
func (s *Store) NotifySettingsForVisiblePeers(ctx context.Context, ownerID int64) (map[PeerDialogKey]NotifySettingsValue, error) {
	if ownerID <= 0 {
		return nil, ErrNotifySettingsPeerInvalid
	}
	rows, err := s.q.NotifySettingsForVisiblePeers(ctx, ownerID)
	if err != nil {
		return nil, fmt.Errorf("read visible notification settings: %w", err)
	}
	values := make(map[PeerDialogKey]NotifySettingsValue, len(rows))
	for _, row := range rows {
		key := PeerDialogKey{PeerType: PeerType(row.PeerType), PeerID: row.PeerID}
		values[key] = NotifySettingsValue{
			ShowPreviews: row.ShowPreviews,
			Silent:       row.Silent,
			MuteUntil:    row.MuteUntil,
			Sound:        row.Sound,
		}
	}
	return values, nil
}

// NotifySettingsExceptions returns the visible peer rows classified by their
// current Telegram category for account.getNotifyExceptions.
func (s *Store) NotifySettingsExceptions(ctx context.Context, ownerID int64, scope NotifySettingsScope) ([]NotifySettingsException, error) {
	if ownerID <= 0 || (scope != NotifySettingsScopeUsers && scope != NotifySettingsScopeChats && scope != NotifySettingsScopeBroadcasts) {
		return nil, ErrNotifySettingsPeerInvalid
	}
	rows, err := s.q.NotifySettingsExceptions(ctx, db.NotifySettingsExceptionsParams{
		OwnerID: ownerID, ExceptionScope: int16(scope),
	})
	if err != nil {
		return nil, fmt.Errorf("read notification settings exceptions: %w", err)
	}
	exceptions := make([]NotifySettingsException, 0, len(rows))
	for _, row := range rows {
		exceptions = append(exceptions, NotifySettingsException{
			PeerType: PeerType(row.PeerType),
			PeerID:   row.PeerID,
			Value: NotifySettingsValue{
				ShowPreviews: row.ShowPreviews,
				Silent:       row.Silent,
				MuteUntil:    row.MuteUntil,
				Sound:        row.Sound,
			},
		})
	}
	return exceptions, nil
}

// NotifySettingsChangesForOwnerSince reads the independent guarded recovery
// stream without advancing the owner's PTS state. after is an exclusive tuple
// boundary for a continuation page.
func (s *Store) NotifySettingsChangesForOwnerSince(ctx context.Context, ownerID int64, since time.Time, after *NotifySettingsDifferencePosition) ([]NotifySettingsChange, error) {
	params := db.NotifySettingsChangesForOwnerSinceParams{
		OwnerID: ownerID, ChangedSince: pgtype.Timestamptz{Time: since, Valid: true},
	}
	if after != nil {
		scope := int16(after.Key.Scope)
		peerType := int16(after.Key.PeerType)
		peerID := after.Key.PeerID
		params.LastChangedAt = pgtype.Timestamptz{Time: after.ChangedAt, Valid: true}
		params.LastScope = &scope
		params.LastPeerType = &peerType
		params.LastPeerID = &peerID
	}
	rows, err := s.q.NotifySettingsChangesForOwnerSince(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("read notification settings changes: %w", err)
	}
	changes := make([]NotifySettingsChange, 0, len(rows))
	for _, row := range rows {
		changes = append(changes, NotifySettingsChange{
			Key: NotifySettingsKey{
				Scope: NotifySettingsScope(row.Scope), PeerType: PeerType(row.PeerType), PeerID: row.PeerID,
			},
			Value: NotifySettingsValue{
				ShowPreviews: row.ShowPreviews, Silent: row.Silent, MuteUntil: row.MuteUntil, Sound: row.Sound,
			},
			ChangedAt: row.ChangedAt.Time,
		})
	}
	return changes, nil
}

// NotifySettingsDifferenceCursorByAuthKey loads the last page issued to this
// authorization. Missing cursors are a normal first-page condition.
func (s *Store) NotifySettingsDifferenceCursorByAuthKey(ctx context.Context, authKeyID int64) (NotifySettingsDifferenceCursor, bool, error) {
	row, err := s.q.NotifySettingsDifferenceCursorByAuthKey(ctx, authKeyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifySettingsDifferenceCursor{}, false, nil
	}
	if err != nil {
		return NotifySettingsDifferenceCursor{}, false, fmt.Errorf("read notification settings difference cursor: %w", err)
	}
	return NotifySettingsDifferenceCursor{
		AuthKeyID:  row.AuthKeyID,
		UserID:     row.UserID,
		Cutoff:     row.Cutoff.Time,
		ResumeDate: int(row.ResumeDate),
		Last: NotifySettingsDifferencePosition{
			ChangedAt: row.LastChangedAt.Time,
			Key: NotifySettingsKey{
				Scope: NotifySettingsScope(row.LastScope), PeerType: PeerType(row.LastPeerType), PeerID: row.LastPeerID,
			},
		},
		State: State{Pts: int(row.StatePts), Qts: int(row.StateQts), Date: int(row.StateDate), Seq: int(row.StateSeq)},
	}, true, nil
}

// SaveNotifySettingsDifferenceCursor stores the state returned with a page and
// its final delivered row as one per-auth-key cursor.
func (s *Store) SaveNotifySettingsDifferenceCursor(ctx context.Context, cursor NotifySettingsDifferenceCursor) error {
	if err := s.q.UpsertNotifySettingsDifferenceCursor(ctx, db.UpsertNotifySettingsDifferenceCursorParams{
		AuthKeyID: cursor.AuthKeyID,
		UserID:    cursor.UserID,
		Cutoff:    pgtype.Timestamptz{Time: cursor.Cutoff, Valid: true},
		LastChangedAt: pgtype.Timestamptz{
			Time: cursor.Last.ChangedAt, Valid: true,
		},
		LastScope:    int16(cursor.Last.Key.Scope),
		LastPeerType: int16(cursor.Last.Key.PeerType),
		LastPeerID:   cursor.Last.Key.PeerID,
		ResumeDate:   int64(cursor.ResumeDate),
		StatePts:     int64(cursor.State.Pts),
		StateQts:     int64(cursor.State.Qts),
		StateDate:    int64(cursor.State.Date),
		StateSeq:     int64(cursor.State.Seq),
	}); err != nil {
		return fmt.Errorf("save notification settings difference cursor: %w", err)
	}
	return nil
}

// DeleteNotifySettingsDifferenceCursor drops a completed or stale page cursor.
func (s *Store) DeleteNotifySettingsDifferenceCursor(ctx context.Context, authKeyID int64) error {
	if err := s.q.DeleteNotifySettingsDifferenceCursor(ctx, authKeyID); err != nil {
		return fmt.Errorf("delete notification settings difference cursor: %w", err)
	}
	return nil
}

func lockNotifySettingsKey(ctx context.Context, tx pgx.Tx, qtx *db.Queries, ownerID int64, key NotifySettingsKey) error {
	switch key.Scope {
	case NotifySettingsScopeUsers, NotifySettingsScopeChats, NotifySettingsScopeBroadcasts:
		if err := lockOwners(ctx, tx, ownerID); err != nil {
			return fmt.Errorf("lock notification settings owner: %w", err)
		}
		return nil
	case NotifySettingsScopePeer:
	default:
		return ErrNotifySettingsPeerInvalid
	}

	switch key.PeerType {
	case PeerTypeUser:
		if err := lockOwners(ctx, tx, ownerID, key.PeerID); err != nil {
			return fmt.Errorf("lock notification settings users: %w", err)
		}
		exists, err := qtx.DialogUnreadMarkUserDialogExists(ctx, db.DialogUnreadMarkUserDialogExistsParams{
			OwnerID: ownerID, PeerID: key.PeerID,
		})
		if err != nil {
			return fmt.Errorf("check notification settings user dialog: %w", err)
		}
		if !exists {
			return ErrNotifySettingsPeerInvalid
		}
	case PeerTypeChat:
		if _, err := qtx.ChatByIDForUpdate(ctx, key.PeerID); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotifySettingsPeerInvalid
		} else if err != nil {
			return fmt.Errorf("lock notification settings chat: %w", err)
		}
		if err := lockOwners(ctx, tx, ownerID); err != nil {
			return fmt.Errorf("lock notification settings owner: %w", err)
		}
		member, err := qtx.DialogUnreadMarkChatMember(ctx, db.DialogUnreadMarkChatMemberParams{
			ChatID: key.PeerID, UserID: ownerID,
		})
		if err != nil {
			return fmt.Errorf("check notification settings chat membership: %w", err)
		}
		if !member {
			return ErrNotifySettingsPeerInvalid
		}
	case PeerTypeChannel:
		if _, err := qtx.LockChannel(ctx, key.PeerID); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotifySettingsPeerInvalid
		} else if err != nil {
			return fmt.Errorf("lock notification settings channel: %w", err)
		}
		if err := lockOwners(ctx, tx, ownerID); err != nil {
			return fmt.Errorf("lock notification settings owner: %w", err)
		}
		member, err := qtx.DialogUnreadMarkChannelMember(ctx, db.DialogUnreadMarkChannelMemberParams{
			ChannelID: key.PeerID, UserID: ownerID,
		})
		if err != nil {
			return fmt.Errorf("check notification settings channel membership: %w", err)
		}
		if !member {
			return ErrNotifySettingsPeerInvalid
		}
	default:
		return ErrNotifySettingsPeerInvalid
	}
	return nil
}

func readNotifySettings(ctx context.Context, qtx *db.Queries, ownerID int64, key NotifySettingsKey) (NotifySettingsValue, bool, error) {
	row, err := qtx.NotifySettingsForKey(ctx, db.NotifySettingsForKeyParams{
		OwnerID: ownerID, Scope: int16(key.Scope), PeerType: int16(key.PeerType), PeerID: key.PeerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return NotifySettingsValue{}, false, nil
	}
	if err != nil {
		return NotifySettingsValue{}, false, fmt.Errorf("read notification settings: %w", err)
	}
	return NotifySettingsValue{
		ShowPreviews: row.ShowPreviews,
		Silent:       row.Silent,
		MuteUntil:    row.MuteUntil,
		Sound:        row.Sound,
	}, true, nil
}

func validNotifySettingsKey(key NotifySettingsKey) bool {
	switch key.Scope {
	case NotifySettingsScopePeer:
		return key.PeerID > 0 && key.PeerType >= PeerTypeUser && key.PeerType <= PeerTypeChannel
	case NotifySettingsScopeUsers, NotifySettingsScopeChats, NotifySettingsScopeBroadcasts:
		return key.PeerType == 0 && key.PeerID == 0
	default:
		return false
	}
}

func validNotifySettingsValue(value NotifySettingsValue) bool {
	return value.Sound == nil || *value.Sound == 0 || *value.Sound == 1
}

func emptyNotifySettings(value NotifySettingsValue) bool {
	return value.ShowPreviews == nil && value.Silent == nil && value.MuteUntil == nil && value.Sound == nil
}

func sameNotifySettings(a, b NotifySettingsValue) bool {
	return equalBoolPtr(a.ShowPreviews, b.ShowPreviews) && equalBoolPtr(a.Silent, b.Silent) &&
		equalInt32Ptr(a.MuteUntil, b.MuteUntil) && equalInt16Ptr(a.Sound, b.Sound)
}

func equalBoolPtr(a, b *bool) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalInt32Ptr(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func equalInt16Ptr(a, b *int16) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
