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

// BotPollGeneration identifies the per-bot getUpdates poll that currently owns
// the shared offset.
type BotPollGeneration int64

// BotUpdatePoll is the shared offset and generation claimed by one getUpdates poll.
type BotUpdatePoll struct {
	Offset     int64
	Generation BotPollGeneration
}

// BotMessageEvent is a retained incoming text message with its author joined
// from the same bounded event-log query.
type BotMessageEvent struct {
	Pts     int64
	Type    EventType
	LocalID int64
	Message Message
	Sender  User
}

// BotMessageEventPage contains eligible updates from one bounded event window
// and the exclusive offset after the last event scanned in that window.
type BotMessageEventPage struct {
	Events     []BotMessageEvent
	NextOffset int64
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

// StartBotUpdatePoll supersedes prior polls for the bot and notifies every
// replica so they can check whether they still own the current generation.
func (s *Store) StartBotUpdatePoll(ctx context.Context, botID int64, token string) (BotUpdatePoll, error) {
	wantHash, ok := botTokenHashForID(botID, token)
	if !ok {
		return BotUpdatePoll{}, ErrBotTokenNotFound
	}

	row, err := s.q.StartBotUpdatePoll(ctx, db.StartBotUpdatePollParams{
		UserID: botID, TokenHash: wantHash,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return BotUpdatePoll{}, ErrBotTokenNotFound
	case err != nil:
		return BotUpdatePoll{}, fmt.Errorf("start bot update poll: %w", err)
	default:
		return BotUpdatePoll{Offset: row.UpdateOffset, Generation: BotPollGeneration(row.PollGeneration)}, nil
	}
}

// CheckBotUpdatePoll returns ErrBotPollSuperseded unless the token still owns
// generation. It also treats token revocation and rotation as unauthorized.
func (s *Store) CheckBotUpdatePoll(ctx context.Context, botID int64, token string, generation BotPollGeneration) error {
	wantHash, ok := botTokenHashForID(botID, token)
	if !ok {
		return ErrBotTokenNotFound
	}

	row, err := s.q.ActiveBotPollGeneration(ctx, botID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrBotTokenNotFound
	case err != nil:
		return fmt.Errorf("read bot update poll generation: %w", err)
	}
	if subtle.ConstantTimeCompare(wantHash, row.TokenHash) != 1 {
		return ErrBotTokenNotFound
	}
	if row.PollGeneration != int64(generation) {
		return ErrBotPollSuperseded
	}
	return nil
}

// FinishBotUpdatePoll conditionally confirms the requested offset only while
// the caller still owns the active token's current poll generation.
func (s *Store) FinishBotUpdatePoll(ctx context.Context, botID int64, token string, generation BotPollGeneration, offset int64) (int64, error) {
	if offset < 0 {
		return 0, errors.New("bot update offset cannot be negative")
	}
	wantHash, ok := botTokenHashForID(botID, token)
	if !ok {
		return 0, ErrBotTokenNotFound
	}

	stored, err := s.q.FinishBotUpdatePoll(ctx, db.FinishBotUpdatePollParams{
		UserID: botID, TokenHash: wantHash, PollGeneration: int64(generation), NextOffset: offset,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if checkErr := s.CheckBotUpdatePoll(ctx, botID, token, generation); checkErr != nil {
			return 0, checkErr
		}
		return 0, errors.New("finish bot update poll returned no row for its current generation")
	case err != nil:
		return 0, fmt.Errorf("finish bot update poll: %w", err)
	default:
		return stored, nil
	}
}

// BotMessageEvents returns eligible updates from a bounded raw event window,
// with each message and sender loaded in the same query. Other pts values stay
// as gaps; NextOffset advances past only events included in that window.
func (s *Store) BotMessageEvents(ctx context.Context, botID, fromPts int64, limit int) (BotMessageEventPage, error) {
	if botID <= 0 || fromPts < 0 {
		return BotMessageEventPage{}, errors.New("bot update window invalid")
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
		return BotMessageEventPage{}, fmt.Errorf("read bot message events: %w", err)
	}
	page := BotMessageEventPage{NextOffset: fromPts}
	if len(rows) == 0 {
		return page, nil
	}
	page.NextOffset = rows[0].NextOffset
	page.Events = make([]BotMessageEvent, 0, len(rows))
	for _, row := range rows {
		if !row.HasHumanSender {
			continue
		}
		page.Events = append(page.Events, BotMessageEvent{
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
		})
	}
	return page, nil
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
