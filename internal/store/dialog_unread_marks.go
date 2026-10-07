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

const dialogUnreadMarkRateLimitSurface = "dialog_filter_mutation"

var (
	ErrDialogUnreadMarkPeerInvalid = errors.New("dialog unread mark peer invalid")
	ErrDialogUnreadMarkRateLimited = errors.New("dialog unread mark rate limited")
)

// DialogUnreadMarkChange is one current owner-private unread mark and its
// recovery timestamp. False values are retained so unmark transitions can be
// replayed after a missed push.
type DialogUnreadMarkChange struct {
	Peer      PeerDialogKey
	Unread    bool
	ChangedAt time.Time
}

// MarkDialogUnread changes one visible dialog's private manual unread mark.
// Visibility checks, the shared mutation budget, the current value, the
// recovery row, and the peer-free notification commit atomically.
func (s *Store) MarkDialogUnread(ctx context.Context, ownerID int64, peer PeerDialogKey, unread bool) (bool, error) {
	if ownerID <= 0 || peer.PeerID <= 0 || peer.PeerType < PeerTypeUser || peer.PeerType > PeerTypeChannel {
		return false, ErrDialogUnreadMarkPeerInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin dialog unread mark: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	switch peer.PeerType {
	case PeerTypeUser:
		if err = lockOwners(ctx, tx, ownerID, peer.PeerID); err != nil {
			return false, fmt.Errorf("lock dialog unread mark users: %w", err)
		}
		exists, e := qtx.DialogUnreadMarkUserDialogExists(ctx, db.DialogUnreadMarkUserDialogExistsParams{
			OwnerID: ownerID, PeerID: peer.PeerID,
		})
		if e != nil {
			return false, fmt.Errorf("check dialog unread mark user dialog: %w", e)
		}
		if !exists {
			return false, ErrDialogUnreadMarkPeerInvalid
		}
	case PeerTypeChat:
		if _, err = qtx.ChatByIDForUpdate(ctx, peer.PeerID); errors.Is(err, pgx.ErrNoRows) {
			return false, ErrDialogUnreadMarkPeerInvalid
		} else if err != nil {
			return false, fmt.Errorf("lock dialog unread mark chat: %w", err)
		}
		if err = lockOwners(ctx, tx, ownerID); err != nil {
			return false, fmt.Errorf("lock dialog unread mark owner: %w", err)
		}
		member, e := qtx.DialogUnreadMarkChatMember(ctx, db.DialogUnreadMarkChatMemberParams{
			ChatID: peer.PeerID, UserID: ownerID,
		})
		if e != nil {
			return false, fmt.Errorf("check dialog unread mark chat membership: %w", e)
		}
		if !member {
			return false, ErrDialogUnreadMarkPeerInvalid
		}
	case PeerTypeChannel:
		if _, err = qtx.LockChannel(ctx, peer.PeerID); errors.Is(err, pgx.ErrNoRows) {
			return false, ErrDialogUnreadMarkPeerInvalid
		} else if err != nil {
			return false, fmt.Errorf("lock dialog unread mark channel: %w", err)
		}
		if err = lockOwners(ctx, tx, ownerID); err != nil {
			return false, fmt.Errorf("lock dialog unread mark owner: %w", err)
		}
		member, e := qtx.DialogUnreadMarkChannelMember(ctx, db.DialogUnreadMarkChannelMemberParams{
			ChannelID: peer.PeerID, UserID: ownerID,
		})
		if e != nil {
			return false, fmt.Errorf("check dialog unread mark channel membership: %w", e)
		}
		if !member {
			return false, ErrDialogUnreadMarkPeerInvalid
		}
	}

	current, err := qtx.DialogUnreadMarkCurrent(ctx, db.DialogUnreadMarkCurrentParams{
		OwnerID: ownerID, PeerType: int16(peer.PeerType), PeerID: peer.PeerID,
	})
	currentUnread := false
	if err == nil {
		currentUnread = current.Unread
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read current dialog unread mark: %w", err)
	}

	_, err = qtx.TryConsumeRateLimitCost(ctx, db.TryConsumeRateLimitCostParams{
		SubjectID:      ownerID,
		Surface:        dialogUnreadMarkRateLimitSurface,
		Cost:           1,
		WindowDuration: pgtype.Interval{Microseconds: time.Minute.Microseconds(), Valid: true},
		LimitCount:     60,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrDialogUnreadMarkRateLimited
	} else if err != nil {
		return false, fmt.Errorf("consume dialog unread mark rate limit: %w", err)
	}

	if currentUnread == unread {
		if err = tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("commit unchanged dialog unread mark: %w", err)
		}
		return false, nil
	}
	stamp, err := qtx.DialogUnreadMarkTimestamp(ctx)
	if err != nil {
		return false, fmt.Errorf("dialog unread mark timestamp: %w", err)
	}
	if !stamp.Valid {
		return false, errors.New("dialog unread mark timestamp: invalid timestamp")
	}
	if err = qtx.UpsertDialogUnreadMark(ctx, db.UpsertDialogUnreadMarkParams{
		OwnerID: ownerID, PeerType: int16(peer.PeerType), PeerID: peer.PeerID, Unread: unread, ChangedAt: stamp,
	}); err != nil {
		return false, fmt.Errorf("save dialog unread mark: %w", err)
	}
	if err = qtx.NotifyDialogUnreadMark(ctx, DialogUnreadMarkNotificationPayload(ownerID, peer)); err != nil {
		return false, fmt.Errorf("notify dialog unread mark: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit dialog unread mark: %w", err)
	}
	return true, nil
}

// DialogUnreadMarksForPeers returns only currently visible marks from the
// caller's private state.
func (s *Store) DialogUnreadMarksForPeers(ctx context.Context, ownerID int64, peers []PeerDialogKey) (map[PeerDialogKey]bool, error) {
	out := make(map[PeerDialogKey]bool)
	if ownerID <= 0 || len(peers) == 0 {
		return out, nil
	}
	peerTypes := make([]int16, 0, len(peers))
	peerIDs := make([]int64, 0, len(peers))
	for _, peer := range peers {
		peerTypes = append(peerTypes, int16(peer.PeerType))
		peerIDs = append(peerIDs, peer.PeerID)
	}
	rows, err := s.q.DialogUnreadMarksForPeers(ctx, db.DialogUnreadMarksForPeersParams{
		OwnerID: ownerID, PeerTypes: peerTypes, PeerIds: peerIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("read dialog unread marks: %w", err)
	}
	for _, row := range rows {
		out[PeerDialogKey{PeerType: PeerType(row.PeerType), PeerID: row.PeerID}] = true
	}
	return out, nil
}

// DialogUnreadMarkStateForPeer reads one current value only while the owner
// remains entitled to the peer named by the notification.
func (s *Store) DialogUnreadMarkStateForPeer(ctx context.Context, ownerID int64, peer PeerDialogKey) (DialogUnreadMarkChange, bool, error) {
	row, err := s.q.DialogUnreadMarkStateForPeer(ctx, db.DialogUnreadMarkStateForPeerParams{
		OwnerID: ownerID, PeerType: int16(peer.PeerType), PeerID: peer.PeerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return DialogUnreadMarkChange{}, false, nil
	}
	if err != nil {
		return DialogUnreadMarkChange{}, false, fmt.Errorf("read dialog unread mark state: %w", err)
	}
	return DialogUnreadMarkChange{
		Peer:      PeerDialogKey{PeerType: PeerType(row.PeerType), PeerID: row.PeerID},
		Unread:    row.Unread,
		ChangedAt: row.ChangedAt.Time,
	}, true, nil
}

// DialogUnreadMarkChangesForOwnerSince reads the independent guarded recovery
// stream without advancing the owner's PTS state.
func (s *Store) DialogUnreadMarkChangesForOwnerSince(ctx context.Context, ownerID int64, since time.Time) ([]DialogUnreadMarkChange, error) {
	rows, err := s.q.DialogUnreadMarkChangesForOwnerSince(ctx, db.DialogUnreadMarkChangesForOwnerSinceParams{
		OwnerID: ownerID, ChangedSince: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("read dialog unread mark changes: %w", err)
	}
	changes := make([]DialogUnreadMarkChange, 0, len(rows))
	for _, row := range rows {
		changes = append(changes, DialogUnreadMarkChange{
			Peer:      PeerDialogKey{PeerType: PeerType(row.PeerType), PeerID: row.PeerID},
			Unread:    row.Unread,
			ChangedAt: row.ChangedAt.Time,
		})
	}
	return changes, nil
}
