package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

const maxBotUpdateLimit = 100

// BotMessageEvent is a retained incoming text message with its author joined
// from the same bounded event-log query.
type BotMessageEvent struct {
	Pts     int64
	Type    EventType
	LocalID int64
	Message Message
	Sender  User
}

// BotUpdateOffset returns the active bot token's confirmed update offset.
func (s *Store) BotUpdateOffset(ctx context.Context, botID int64, token string) (int64, error) {
	wantHash, ok := botTokenHashForID(botID, token)
	if !ok {
		return 0, ErrBotTokenNotFound
	}

	row, err := s.q.ActiveBotTokenState(ctx, botID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrBotTokenNotFound
	case err != nil:
		return 0, fmt.Errorf("read bot update offset: %w", err)
	}
	if subtle.ConstantTimeCompare(wantHash, row.TokenHash) != 1 {
		return 0, ErrBotTokenNotFound
	}
	return row.UpdateOffset, nil
}

// AdvanceBotUpdateOffset confirms all updates below offset without allowing a
// replayed older offset or a rotated token to move the stored cursor backward.
func (s *Store) AdvanceBotUpdateOffset(ctx context.Context, botID int64, token string, offset int64) (int64, error) {
	if offset < 0 {
		return 0, ErrBotTokenNotFound
	}
	wantHash, ok := botTokenHashForID(botID, token)
	if !ok {
		return 0, ErrBotTokenNotFound
	}

	stored, err := s.q.AdvanceBotUpdateOffset(ctx, db.AdvanceBotUpdateOffsetParams{
		UserID: botID, TokenHash: wantHash, NextOffset: offset,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return 0, ErrBotTokenNotFound
	case err != nil:
		return 0, fmt.Errorf("advance bot update offset: %w", err)
	default:
		return stored, nil
	}
}

// BotMessageEvents returns retained incoming private text-message events at or
// above fromPts with each message and sender loaded in the same query. Other
// event kinds and messages stay as pts gaps.
func (s *Store) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) ([]BotMessageEvent, error) {
	if botID <= 0 || fromPts < 0 {
		return nil, errors.New("bot update window invalid")
	}
	if limit < 1 {
		limit = 1
	} else if limit > maxBotUpdateLimit {
		limit = maxBotUpdateLimit
	}
	rows, err := s.q.BotMessageEvents(ctx, db.BotMessageEventsParams{
		OwnerID: botID,
		FromPts: fromPts,
		Lim:     int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("read bot message events: %w", err)
	}
	events := make([]BotMessageEvent, len(rows))
	for i, row := range rows {
		events[i] = BotMessageEvent{
			Pts:     row.Pts,
			Type:    EventType(row.Type),
			LocalID: row.LocalID,
			Message: Message{
				OwnerID:           row.MessageOwnerID,
				LocalID:           row.MessageLocalID,
				PeerType:          PeerType(row.MessagePeerType),
				PeerID:            row.MessagePeerID,
				FromID:            row.MessageFromID,
				Date:              row.MessageDate.Time,
				Text:              row.MessageText,
				Out:               row.MessageOut,
				Deleted:           row.MessageDeleted,
				Action:            ChatAction(row.MessageActionType),
				FileID:            row.MessageFileID,
				StickerDocumentID: row.MessageStickerDocumentID,
			},
			Sender: User{
				ID:        row.SenderID,
				Kind:      row.SenderKind,
				FirstName: row.SenderFirstName,
				LastName:  row.SenderLastName,
				Username:  row.SenderUsername,
			},
		}
	}
	return events, nil
}

func botTokenHashForID(botID int64, token string) ([]byte, bool) {
	if botID <= 0 {
		return nil, false
	}
	tokenID, secret, ok := parseBotToken(token)
	if !ok || tokenID != botID {
		clear(secret)
		return nil, false
	}
	defer clear(secret)
	digest := sha256.Sum256(secret)
	return digest[:], true
}
