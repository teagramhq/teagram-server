package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// PeerSettingsSnapshot is the caller-scoped read set for messages.getPeerSettings
// and users.getFullUser. User relationship edges and the 1:1 dialog belong to
// ownerID; group metadata is returned only when ownerID is a current member.
// No edge is ever read from the peer's side.
type PeerSettingsSnapshot struct {
	User                 User
	HasDialog            bool
	Contact              bool
	Blocked              bool
	Chat                 Chat
	ChatParticipantCount int64
	Channel              Channel
	ChannelMember        ChannelMember
}

// PeerSettingsForViewer reads one peer and the caller-owned relationship state
// from a repeatable-read, read-only snapshot. Non-visible peers produce
// found=false, allowing the API layer to keep unknown and unauthorized ids
// indistinguishable.
func (s *Store) PeerSettingsForViewer(ctx context.Context, ownerID int64, peer PeerDialogKey) (PeerSettingsSnapshot, bool, error) {
	if ownerID <= 0 || peer.PeerID <= 0 {
		return PeerSettingsSnapshot{}, false, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return PeerSettingsSnapshot{}, false, fmt.Errorf("begin peer settings snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	snapshot := PeerSettingsSnapshot{}
	qtx := s.q.WithTx(tx)
	finish := func(found bool) (PeerSettingsSnapshot, bool, error) {
		if err := tx.Commit(ctx); err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("commit peer settings snapshot: %w", err)
		}
		return snapshot, found, nil
	}

	switch peer.PeerType {
	case PeerTypeUser:
		row, err := qtx.UserByID(ctx, peer.PeerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return finish(false)
		}
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings user: %w", err)
		}
		snapshot.User = UserFromDB(row)

		snapshot.HasDialog, err = qtx.PeerDialogExists(ctx, db.PeerDialogExistsParams{
			OwnerID: ownerID, PeerType: int16(PeerTypeUser), PeerID: peer.PeerID,
		})
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings dialog: %w", err)
		}
		if ownerID != peer.PeerID {
			snapshot.Contact, err = qtx.ContactExists(ctx, db.ContactExistsParams{OwnerID: ownerID, ContactID: peer.PeerID})
			if err != nil {
				return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings contact: %w", err)
			}
			// The block edge belongs to the caller and is true whether or not a
			// dialog exists, so it is read unconditionally. Callers that show it
			// only inside a dialog apply that gate themselves.
			snapshot.Blocked, err = qtx.IsBlocked(ctx, db.IsBlockedParams{BlockerID: ownerID, BlockedID: peer.PeerID})
			if err != nil {
				return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings block: %w", err)
			}
		}
		return finish(true)

	case PeerTypeChat:
		rows, err := qtx.ChatsByIDsForMember(ctx, db.ChatsByIDsForMemberParams{
			UserID: ownerID, ChatIds: []int64{peer.PeerID},
		})
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings member chat: %w", err)
		}
		if len(rows) == 0 {
			return finish(false)
		}
		snapshot.Chat = chatFromRow(rows[0])
		counts, err := qtx.ChatParticipantCountsByChatIDs(ctx, []int64{peer.PeerID})
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("count peer settings chat members: %w", err)
		}
		if len(counts) != 1 {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("count peer settings chat members: got %d rows, want 1", len(counts))
		}
		snapshot.ChatParticipantCount = counts[0].ParticipantCount
		return finish(true)

	case PeerTypeChannel:
		row, err := qtx.ChannelByID(ctx, peer.PeerID)
		if errors.Is(err, pgx.ErrNoRows) {
			return finish(false)
		}
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings channel: %w", err)
		}
		member, err := qtx.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{
			ChannelID: peer.PeerID, UserID: ownerID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return finish(false)
		}
		if err != nil {
			return PeerSettingsSnapshot{}, false, fmt.Errorf("select peer settings channel member: %w", err)
		}
		snapshot.Channel = channelFromRow(row)
		snapshot.ChannelMember = channelMemberFromRow(member)
		if snapshot.ChannelMember.Banned(s.now()) {
			return finish(false)
		}
		return finish(true)

	default:
		return finish(false)
	}
}
