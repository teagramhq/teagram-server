package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const (
	ChatAvailableReactionsAll  = "all"
	ChatAvailableReactionsSome = "some"
	ChatAvailableReactionsNone = "none"
)

// ChatAvailableReactions is the durable reaction setting for one basic group.
// A missing row means Mode is ChatAvailableReactionsAll.
type ChatAvailableReactions struct {
	Mode      string
	Emoticons []string
}

func (available ChatAvailableReactions) Allows(emoticon string) bool {
	switch available.Mode {
	case ChatAvailableReactionsAll:
		return true
	case ChatAvailableReactionsSome:
		return slices.Contains(available.Emoticons, emoticon)
	}
	return false
}

// SetChatAvailableReactions stores a basic group's allowed reactions under the
// existing chats row lock. That lock serializes this setting with group
// reactions and keeps the current title-edit permission decision authoritative.
func (s *Store) SetChatAvailableReactions(ctx context.Context, chatID, callerID int64, available ChatAvailableReactions) error {
	if (available.Mode != ChatAvailableReactionsAll && available.Mode != ChatAvailableReactionsSome && available.Mode != ChatAvailableReactionsNone) ||
		(available.Mode != ChatAvailableReactionsSome && len(available.Emoticons) != 0) {
		return ErrReactionInvalid
	}
	if slices.Contains(available.Emoticons, "") {
		return ErrReactionInvalid
	}
	reactions := available.Emoticons
	if reactions == nil {
		reactions = []string{}
	}

	m, err := s.beginChatMutation(ctx, chatID, callerID)
	if err != nil {
		return err
	}
	defer func() { _ = m.tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	if !m.canChangeInfo(callerID) {
		return ErrChatAdminRequired
	}
	if err := m.qtx.SetChatAvailableReactions(ctx, db.SetChatAvailableReactionsParams{
		ChatID: chatID, Mode: available.Mode, Reactions: reactions,
	}); err != nil {
		return fmt.Errorf("set chat available reactions: %w", err)
	}
	if _, err := m.qtx.BumpChatVersion(ctx, chatID); err != nil {
		return fmt.Errorf("bump chat version for reactions: %w", err)
	}
	if err := m.tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit chat available reactions: %w", err)
	}
	return nil
}

func chatAvailableReactions(ctx context.Context, qtx *db.Queries, chatID int64) (ChatAvailableReactions, error) {
	rows, err := qtx.ChatAvailableReactionsByIDs(ctx, []int64{chatID})
	if err != nil {
		return ChatAvailableReactions{}, fmt.Errorf("load chat available reactions: %w", err)
	}
	if len(rows) == 0 {
		return ChatAvailableReactions{Mode: ChatAvailableReactionsAll}, nil
	}
	if len(rows) != 1 {
		return ChatAvailableReactions{}, fmt.Errorf("load chat available reactions: got %d rows for chat %d", len(rows), chatID)
	}
	return ChatAvailableReactions{Mode: rows[0].Mode, Emoticons: rows[0].Reactions}, nil
}

func chatHasMember(ctx context.Context, qtx *db.Queries, chatID, userID int64) (bool, error) {
	member, err := qtx.IsChatMember(ctx, db.IsChatMemberParams{ChatID: chatID, UserID: userID})
	if err != nil {
		return false, fmt.Errorf("check chat reaction member: %w", err)
	}
	return member, nil
}

func lockChatForReaction(ctx context.Context, qtx *db.Queries, chatID int64) error {
	_, err := qtx.ChatByIDForUpdate(ctx, chatID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	}
	if err != nil {
		return fmt.Errorf("lock chat for reaction: %w", err)
	}
	return nil
}
