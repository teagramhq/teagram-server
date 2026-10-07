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
	cloudDraftRateLimitSurface = "messages_save_draft"
	cloudDraftRateLimit        = 120
	cloudDraftRateLimitWindow  = time.Minute
)

var ErrCloudDraftRateLimited = errors.New("cloud draft save rate limited")

// CloudDraft is the current owner-private draft for one dialog.
type CloudDraft struct {
	PeerType     PeerType
	PeerID       int64
	Message      string
	NoWebpage    bool
	ReplyToMsgID int64
	UpdatedAt    time.Time
}

// CloudDraftChange is the latest current value or clear marker for one peer.
type CloudDraftChange struct {
	Draft     CloudDraft
	HasDraft  bool
	ChangedAt time.Time
}

// SaveCloudDraft writes a draft or clear marker for an existing owner-visible
// peer. Membership is rechecked while holding the same locks as removal, and
// rate-limit admission, the value, and its recovery marker commit together.
func (s *Store) SaveCloudDraft(ctx context.Context, ownerID int64, peerType PeerType, peerID int64, message string, noWebpage bool, replyToMsgID int64) (bool, time.Time, error) {
	if ownerID <= 0 || peerID <= 0 || peerType < PeerTypeUser || peerType > PeerTypeChannel {
		return false, time.Time{}, ErrNotMember
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("begin cloud draft: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	switch peerType {
	case PeerTypeUser:
		if err = lockOwners(ctx, tx, ownerID, peerID); err != nil {
			return false, time.Time{}, fmt.Errorf("lock cloud draft owners: %w", err)
		}
		exists, e := qtx.CloudDraftUserDialogExists(ctx, db.CloudDraftUserDialogExistsParams{OwnerID: ownerID, PeerID: peerID})
		if e != nil {
			return false, time.Time{}, fmt.Errorf("check cloud draft user dialog: %w", e)
		}
		if !exists {
			return false, time.Time{}, ErrNotMember
		}
	case PeerTypeChat:
		if _, err = qtx.ChatByIDForUpdate(ctx, peerID); errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, ErrNotMember
		} else if err != nil {
			return false, time.Time{}, fmt.Errorf("lock cloud draft chat: %w", err)
		}
		if err = lockOwners(ctx, tx, ownerID); err != nil {
			return false, time.Time{}, fmt.Errorf("lock cloud draft owner: %w", err)
		}
		member, e := qtx.IsChatMember(ctx, db.IsChatMemberParams{ChatID: peerID, UserID: ownerID})
		if e != nil {
			return false, time.Time{}, fmt.Errorf("check cloud draft chat membership: %w", e)
		}
		if !member {
			return false, time.Time{}, ErrNotMember
		}
	case PeerTypeChannel:
		if _, err = qtx.LockChannel(ctx, peerID); errors.Is(err, pgx.ErrNoRows) {
			return false, time.Time{}, ErrNotMember
		} else if err != nil {
			return false, time.Time{}, fmt.Errorf("lock cloud draft channel: %w", err)
		}
		if err = lockOwners(ctx, tx, ownerID); err != nil {
			return false, time.Time{}, fmt.Errorf("lock cloud draft owner: %w", err)
		}
		member, e := qtx.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{ChannelID: peerID, UserID: ownerID})
		if errors.Is(e, pgx.ErrNoRows) || e == nil && channelMemberFromRow(member).Banned(s.now()) {
			return false, time.Time{}, ErrNotMember
		}
		if e != nil {
			return false, time.Time{}, fmt.Errorf("check cloud draft channel membership: %w", e)
		}
	}

	if replyToMsgID > 0 {
		if peerType == PeerTypeChannel {
			target, e := qtx.ChannelMessageByLocal(ctx, db.ChannelMessageByLocalParams{ChannelID: peerID, LocalID: replyToMsgID})
			if errors.Is(e, pgx.ErrNoRows) || e == nil && (target.Deleted || target.ActionType != 0) {
				return false, time.Time{}, ErrMessageInvalid
			}
			if e != nil {
				return false, time.Time{}, fmt.Errorf("validate cloud draft channel reply: %w", e)
			}
		} else {
			_, e := qtx.ActiveOrdinaryMessageInDialog(ctx, db.ActiveOrdinaryMessageInDialogParams{
				OwnerID: ownerID, LocalID: replyToMsgID, PeerType: int16(peerType), PeerID: peerID,
			})
			if errors.Is(e, pgx.ErrNoRows) {
				return false, time.Time{}, ErrMessageInvalid
			}
			if e != nil {
				return false, time.Time{}, fmt.Errorf("validate cloud draft reply: %w", e)
			}
		}
	}

	if _, err = qtx.TryConsumeRateLimitCost(ctx, db.TryConsumeRateLimitCostParams{
		SubjectID:      ownerID,
		Surface:        cloudDraftRateLimitSurface,
		Cost:           1,
		WindowDuration: pgtype.Interval{Microseconds: cloudDraftRateLimitWindow.Microseconds(), Valid: true},
		LimitCount:     cloudDraftRateLimit,
	}); errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, ErrCloudDraftRateLimited
	} else if err != nil {
		return false, time.Time{}, fmt.Errorf("consume cloud draft rate limit: %w", err)
	}

	stamp, err := qtx.CloudDraftTimestamp(ctx)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("cloud draft timestamp: %w", err)
	}
	if !stamp.Valid {
		return false, time.Time{}, errors.New("cloud draft timestamp: invalid timestamp")
	}
	changed := true
	if message == "" && replyToMsgID == 0 {
		var rows int64
		rows, err = qtx.DeleteCloudDraft(ctx, db.DeleteCloudDraftParams{OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID})
		if err != nil {
			return false, time.Time{}, fmt.Errorf("delete cloud draft: %w", err)
		}
		changed = rows > 0
	} else if err = qtx.UpsertCloudDraft(ctx, db.UpsertCloudDraftParams{
		OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID,
		Message: message, NoWebpage: noWebpage, Column6: replyToMsgID,
		UpdatedAt: stamp,
	}); err != nil {
		return false, time.Time{}, fmt.Errorf("upsert cloud draft: %w", err)
	}
	if changed {
		if err = qtx.MarkCloudDraftChanged(ctx, db.MarkCloudDraftChangedParams{
			OwnerID: ownerID, PeerType: int16(peerType), PeerID: peerID, ChangedAt: stamp,
		}); err != nil {
			return false, time.Time{}, fmt.Errorf("mark cloud draft changed: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return false, time.Time{}, fmt.Errorf("commit cloud draft: %w", err)
	}
	return changed, stamp.Time, nil
}

// CloudDraftsForPeers returns only drafts whose owner still has access to the
// named peers. The access checks run in the same query as the private text.
func (s *Store) CloudDraftsForPeers(ctx context.Context, ownerID int64, peers []PeerDialogKey) (map[PeerDialogKey]CloudDraft, error) {
	return cloudDraftsForPeers(ctx, s.q, ownerID, peers)
}

func cloudDraftsForPeers(ctx context.Context, q *db.Queries, ownerID int64, peers []PeerDialogKey) (map[PeerDialogKey]CloudDraft, error) {
	out := make(map[PeerDialogKey]CloudDraft)
	if len(peers) == 0 {
		return out, nil
	}
	peerTypes := make([]int16, 0, len(peers))
	peerIDs := make([]int64, 0, len(peers))
	for _, peer := range peers {
		peerTypes = append(peerTypes, int16(peer.PeerType))
		peerIDs = append(peerIDs, peer.PeerID)
	}
	rows, err := q.CloudDraftsForPeers(ctx, db.CloudDraftsForPeersParams{OwnerID: ownerID, PeerTypes: peerTypes, PeerIds: peerIDs})
	if err != nil {
		return nil, fmt.Errorf("read cloud drafts: %w", err)
	}
	for _, row := range rows {
		key := PeerDialogKey{PeerType: PeerType(row.PeerType), PeerID: row.PeerID}
		out[key] = CloudDraft{
			PeerType: key.PeerType, PeerID: key.PeerID, Message: row.Message,
			NoWebpage: row.NoWebpage, ReplyToMsgID: row.ReplyToMsgID, UpdatedAt: row.UpdatedAt.Time,
		}
	}
	return out, nil
}

// CloudDraftStateForPeer returns the latest current value or clear marker for
// one peer. Missing or no-longer-visible peers return found=false.
func (s *Store) CloudDraftStateForPeer(ctx context.Context, ownerID int64, peer PeerDialogKey) (CloudDraftChange, bool, error) {
	row, err := s.q.CloudDraftStateForPeer(ctx, db.CloudDraftStateForPeerParams{
		OwnerID: ownerID, PeerType: int16(peer.PeerType), PeerID: peer.PeerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return CloudDraftChange{}, false, nil
	}
	if err != nil {
		return CloudDraftChange{}, false, fmt.Errorf("read cloud draft state: %w", err)
	}
	hasDraft, ok := row.HasDraft.(bool)
	if !ok {
		return CloudDraftChange{}, false, fmt.Errorf("read cloud draft state: unexpected has_draft value %T", row.HasDraft)
	}
	return CloudDraftChange{
		Draft: CloudDraft{PeerType: PeerType(row.PeerType), PeerID: row.PeerID, Message: row.Message,
			NoWebpage: row.NoWebpage, ReplyToMsgID: row.ReplyToMsgID, UpdatedAt: row.UpdatedAt.Time},
		HasDraft: hasDraft, ChangedAt: row.ChangedAt.Time,
	}, true, nil
}

// CloudDraftChangesForOwnerSince reads the independent, guarded draft recovery
// stream without consuming or advancing the owner's PTS state.
func (s *Store) CloudDraftChangesForOwnerSince(ctx context.Context, ownerID int64, since time.Time) ([]CloudDraftChange, error) {
	rows, err := s.q.CloudDraftChangesForOwnerSince(ctx, db.CloudDraftChangesForOwnerSinceParams{
		OwnerID: ownerID, ChangedSince: pgtype.Timestamptz{Time: since, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("read cloud draft changes: %w", err)
	}
	changes := make([]CloudDraftChange, 0, len(rows))
	for _, row := range rows {
		hasDraft, ok := row.HasDraft.(bool)
		if !ok {
			return nil, fmt.Errorf("read cloud draft changes: unexpected has_draft value %T", row.HasDraft)
		}
		changes = append(changes, CloudDraftChange{
			Draft: CloudDraft{PeerType: PeerType(row.PeerType), PeerID: row.PeerID, Message: row.Message,
				NoWebpage: row.NoWebpage, ReplyToMsgID: row.ReplyToMsgID, UpdatedAt: row.UpdatedAt.Time},
			HasDraft: hasDraft, ChangedAt: row.ChangedAt.Time,
		})
	}
	return changes, nil
}
