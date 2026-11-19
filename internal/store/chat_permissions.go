package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/demo"
	"github.com/teagramhq/teagram-server/internal/store/db"
)

var chatMediaSubtypeRights = [...]string{
	"send_stickers",
	"send_gifs",
	"send_videos",
	"send_roundvideos",
	"send_audios",
	"send_voices",
}

func hasChatRight(rights []string, right string) bool {
	return slices.Contains(rights, right)
}

func checkDefaultMessageRestriction(rights []string, exempt, media bool, mediaRights []string) error {
	return checkMessageRestriction(rights, exempt, media, mediaRights, false)
}

func checkDemoGeneralChatStickerRestriction(rights []string, exempt, media bool, mediaRights []string) error {
	return checkMessageRestriction(rights, exempt, media, mediaRights, true)
}

func checkMessageRestriction(rights []string, exempt, media bool, mediaRights []string, allowStickerWithSendDocs bool) error {
	if exempt {
		return nil
	}
	if hasChatRight(rights, "send_messages") {
		return ErrChatWriteForbidden
	}
	if media {
		// Photos and stickers have their own restrictions and are not generic
		// documents covered by send_docs.
		if hasChatRight(rights, "send_media") ||
			(hasChatRight(rights, "send_docs") &&
				!hasChatRight(mediaRights, "send_photos") &&
				(!allowStickerWithSendDocs || !hasChatRight(mediaRights, "send_stickers"))) {
			return ErrChatWriteForbidden
		}
		if mediaRights == nil {
			for _, right := range chatMediaSubtypeRights {
				if hasChatRight(rights, right) {
					return ErrChatWriteForbidden
				}
			}
		}
		for _, right := range mediaRights {
			if hasChatRight(rights, right) {
				return ErrChatWriteForbidden
			}
		}
	} else if hasChatRight(rights, "send_plain") {
		return ErrChatWriteForbidden
	}
	return nil
}

func isDemoGeneralChatChannel(ctx context.Context, q *db.Queries, channelID int64) (bool, error) {
	owner, err := q.GetUserByUsername(ctx, demo.SeedOwnerUsername)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("load demo General owner: %w", err)
	}
	if owner.ID <= 0 {
		return false, nil
	}
	for _, handle := range demo.GeneralChatHandles() {
		row, err := q.GetChannelByUsername(ctx, handle)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			continue
		case err != nil:
			return false, fmt.Errorf("load demo General channel %q: %w", handle, err)
		}
		channel := channelFromRow(row)
		if channel.ID == channelID && channel.Title == "General" && channel.Megagroup &&
			channel.CreatorID == owner.ID && channel.Username != nil && *channel.Username == handle {
			return true, nil
		}
	}
	return false, nil
}

// CheckChatWritePermission checks a new basic-chat media send before the
// media handler assembles an upload. The final send repeats the decision in its
// write transaction; this check avoids creating file rows or blobs for a send
// that is already denied. A committed random_id retry bypasses the restriction,
// matching the final send transaction's dedup order.
func (s *Store) CheckChatWritePermission(ctx context.Context, chatID, callerID, randomID int64, mediaRights []string) (duplicate bool, err error) {
	m, err := s.beginChatMutation(ctx, chatID, callerID)
	if err != nil {
		return false, err
	}
	defer func() { _ = m.tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit

	if randomID != 0 {
		_, err = messageRowByRandomID(ctx, m.qtx, callerID, randomID)
		switch {
		case err == nil:
			if err = m.tx.Commit(ctx); err != nil {
				return false, fmt.Errorf("commit permission check: %w", err)
			}
			return true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return false, fmt.Errorf("random_id lookup: %w", err)
		}
	}
	if err = checkDefaultMessageRestriction(m.defaultBannedRights, callerID == m.creatorID, true, mediaRights); err != nil {
		return false, err
	}
	if err = m.tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit permission check: %w", err)
	}
	return false, nil
}
