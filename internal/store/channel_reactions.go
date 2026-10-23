package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ChannelReactionCount is an aggregate count for one channel post. Chosen is
// derived only from the requesting member's own reaction row.
type ChannelReactionCount struct {
	Reaction string
	Count    int
	Chosen   bool
}

// SetChannelAvailableReactions changes the channel reaction setting under the
// channel row lock. Only the creator or an admin with change-info may do so.
func (s *Store) SetChannelAvailableReactions(
	ctx context.Context,
	channelID, callerID int64,
	available ChatAvailableReactions,
) error {
	if err := validateAvailableReactions(available); err != nil {
		return err
	}
	if channelID <= 0 || callerID <= 0 {
		return ErrNotMember
	}
	reactions := available.Emoticons
	if reactions == nil {
		reactions = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin channel reaction setting: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	channel, err := qtx.LockChannel(ctx, channelID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	}
	if err != nil {
		return fmt.Errorf("lock channel for reaction setting: %w", err)
	}
	if channel.DeletedAt.Valid {
		return ErrNotMember
	}
	row, err := qtx.ChannelParticipantByUser(ctx, db.ChannelParticipantByUserParams{
		ChannelID: channelID,
		UserID:    callerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	}
	if err != nil {
		return fmt.Errorf("load channel reaction-setting member: %w", err)
	}
	member := channelMemberFromRow(row)
	if member.Banned(time.Now()) {
		return ErrNotMember
	}
	if member.Role != channelRoleCreator && (member.Role != channelRoleAdmin || !member.AdminChangeInfo) {
		return ErrChatAdminRequired
	}

	if err := qtx.SetChannelAvailableReactions(ctx, db.SetChannelAvailableReactionsParams{
		ChannelID: channelID,
		Mode:      available.Mode,
		Reactions: reactions,
	}); err != nil {
		return fmt.Errorf("set channel available reactions: %w", err)
	}
	updated, err := qtx.BumpChannelVersionForAvailableReactions(ctx, channelID)
	if err != nil {
		return fmt.Errorf("bump channel version for available reactions: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("bump channel version for available reactions: updated %d rows, want 1", updated)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit channel available reactions: %w", err)
	}
	return nil
}

func validateAvailableReactions(available ChatAvailableReactions) error {
	if (available.Mode != ChatAvailableReactionsAll && available.Mode != ChatAvailableReactionsSome && available.Mode != ChatAvailableReactionsNone) ||
		(available.Mode != ChatAvailableReactionsSome && len(available.Emoticons) != 0) ||
		slices.Contains(available.Emoticons, "") {
		return ErrReactionInvalid
	}
	return nil
}

func channelAvailableReactions(ctx context.Context, qtx *db.Queries, channelID int64) (ChatAvailableReactions, error) {
	rows, err := qtx.ChannelAvailableReactionsByIDs(ctx, []int64{channelID})
	if err != nil {
		return ChatAvailableReactions{}, fmt.Errorf("load channel available reactions: %w", err)
	}
	if len(rows) == 0 {
		return ChatAvailableReactions{Mode: ChatAvailableReactionsAll}, nil
	}
	if len(rows) != 1 {
		return ChatAvailableReactions{}, fmt.Errorf("load channel available reactions: got %d rows for channel %d", len(rows), channelID)
	}
	return ChatAvailableReactions{Mode: rows[0].Mode, Emoticons: rows[0].Reactions}, nil
}

// SetChannelMessageReaction records or clears the caller's single reaction.
// It rechecks visibility in the transaction and holds locks in channel,
// participant, post order. This stays compatible with other reaction writers
// and never takes channel_state; tombstones and bans therefore either follow a
// committed reaction or make the reaction observe the new hidden state.
func (s *Store) SetChannelMessageReaction(
	ctx context.Context,
	channelID, reactorID, localID int64,
	reaction string,
) ([]ChannelReactionCount, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin channel message reaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	if _, err := qtx.LockChannelForReaction(ctx, channelID); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotMember
	} else if err != nil {
		return nil, fmt.Errorf("lock live channel for reaction: %w", err)
	}
	participant, err := qtx.ChannelReactionParticipantForShare(ctx, db.ChannelReactionParticipantForShareParams{
		ChannelID: channelID,
		UserID:    reactorID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotMember
	}
	if err != nil {
		return nil, fmt.Errorf("lock channel reaction member: %w", err)
	}
	member := channelMemberFromRow(participant)
	if member.Banned(time.Now()) {
		return nil, ErrNotMember
	}
	if _, err := qtx.ChannelReactionMessageForShare(ctx, db.ChannelReactionMessageForShareParams{
		ChannelID:      channelID,
		LocalID:        localID,
		AvailableMinID: member.AvailableMinID,
	}); errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrMessageInvalid
	} else if err != nil {
		return nil, fmt.Errorf("lock readable channel post for reaction: %w", err)
	}

	available, err := channelAvailableReactions(ctx, qtx, channelID)
	if err != nil {
		return nil, err
	}
	if reaction != "" && !available.Allows(reaction) {
		return nil, ErrReactionInvalid
	}
	if reaction == "" {
		err = qtx.DeleteChannelMessageReaction(ctx, db.DeleteChannelMessageReactionParams{
			ChannelID: channelID,
			LocalID:   localID,
			ReactorID: reactorID,
		})
	} else {
		err = qtx.UpsertChannelMessageReaction(ctx, db.UpsertChannelMessageReactionParams{
			ChannelID: channelID,
			LocalID:   localID,
			ReactorID: reactorID,
			Reaction:  reaction,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("write channel message reaction: %w", err)
	}
	counts, err := channelMessageReactionsByMessageIDs(ctx, qtx, channelID, reactorID, []int64{localID})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit channel message reaction: %w", err)
	}
	return counts[localID], nil
}

// ChannelMessageReactionsByMessageIDs returns aggregate counts and only the
// caller's own chosen reaction for a batch of already-readable posts.
func (s *Store) ChannelMessageReactionsByMessageIDs(
	ctx context.Context,
	channelID, reactorID int64,
	localIDs []int64,
) (map[int64][]ChannelReactionCount, error) {
	if len(localIDs) == 0 {
		return map[int64][]ChannelReactionCount{}, nil
	}
	return channelMessageReactionsByMessageIDs(ctx, s.q, channelID, reactorID, localIDs)
}

func channelMessageReactionsByMessageIDs(
	ctx context.Context,
	qtx *db.Queries,
	channelID, reactorID int64,
	localIDs []int64,
) (map[int64][]ChannelReactionCount, error) {
	byMessage := make(map[int64][]ChannelReactionCount, len(localIDs))
	counts, err := qtx.ChannelReactionCountsByMessageIDs(ctx, db.ChannelReactionCountsByMessageIDsParams{
		ChannelID: channelID,
		LocalIds:  localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("load channel reaction counts: %w", err)
	}
	for _, row := range counts {
		byMessage[row.LocalID] = append(byMessage[row.LocalID], ChannelReactionCount{
			Reaction: row.Reaction,
			Count:    int(row.ReactionCount),
		})
	}
	own, err := qtx.ChannelOwnReactionsByMessageIDs(ctx, db.ChannelOwnReactionsByMessageIDsParams{
		ChannelID: channelID,
		ReactorID: reactorID,
		LocalIds:  localIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("load caller channel reactions: %w", err)
	}
	for _, row := range own {
		for i := range byMessage[row.LocalID] {
			if byMessage[row.LocalID][i].Reaction == row.Reaction {
				byMessage[row.LocalID][i].Chosen = true
				break
			}
		}
	}
	return byMessage, nil
}
