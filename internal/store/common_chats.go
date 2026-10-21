package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// CommonChat is one basic group or supergroup shared by two users.
type CommonChat struct {
	PeerType         PeerType
	BasicGroup       Chat
	Supergroup       Channel
	ParticipantCount int64
	SupergroupRole   int
}

// CommonChatsPage is one stable page and the total number of shared chats
// before maxID paging.
type CommonChatsPage struct {
	Chats []CommonChat
	Count int64
}

// CommonChatsForUsers returns shared basic groups and readable supergroups,
// newest first. Membership, Count and current channel avatars come from one
// repeatable-read snapshot, and Count is calculated before maxID and limit.
func (s *Store) CommonChatsForUsers(ctx context.Context, viewerID, targetID, maxID int64, limit int32) (CommonChatsPage, error) {
	if viewerID <= 0 || targetID <= 0 {
		return CommonChatsPage{}, errors.New("common chats: user ids must be positive")
	}
	if limit <= 0 {
		return CommonChatsPage{}, errors.New("common chats: limit must be positive")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return CommonChatsPage{}, fmt.Errorf("begin common chats snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	rows, err := qtx.CommonChatsForUsers(ctx, db.CommonChatsForUsersParams{
		ViewerID: viewerID,
		TargetID: targetID,
		MaxID:    maxID,
		Lim:      limit,
	})
	if err != nil {
		return CommonChatsPage{}, fmt.Errorf("select common chats for users %d and %d: %w", viewerID, targetID, err)
	}

	page := CommonChatsPage{Chats: make([]CommonChat, 0, len(rows))}
	if len(rows) == 0 {
		return page, nil
	}
	page.Count = rows[0].TotalCount
	for _, row := range rows {
		if row.PeerType == 0 {
			continue
		}
		common := CommonChat{PeerType: PeerType(row.PeerType)}
		switch common.PeerType {
		case PeerTypeChat:
			common.BasicGroup = Chat{
				ID:                  row.ID,
				Title:               row.Title,
				CreatorID:           row.CreatorID,
				Version:             int(row.Version),
				Date:                row.Date.Time,
				DefaultBannedRights: row.DefaultBannedRights,
			}
			common.ParticipantCount = row.ParticipantCount
		case PeerTypeChannel:
			common.Supergroup = Channel{
				ID:                  row.ID,
				Title:               row.Title,
				CreatorID:           row.CreatorID,
				Megagroup:           true,
				Version:             int(row.Version),
				Date:                row.Date.Time,
				Username:            row.Username,
				DefaultBannedRights: row.DefaultBannedRights,
			}
			common.SupergroupRole = int(row.MemberRole)
		default:
			return CommonChatsPage{}, fmt.Errorf("common chats query returned unsupported peer type %d", row.PeerType)
		}
		page.Chats = append(page.Chats, common)
	}
	channelIDs := make([]int64, 0, len(page.Chats))
	for _, common := range page.Chats {
		if common.PeerType == PeerTypeChannel {
			channelIDs = append(channelIDs, common.Supergroup.ID)
		}
	}
	channelRows, err := qtx.ChannelsByIDs(ctx, channelIDs)
	if err != nil {
		return CommonChatsPage{}, fmt.Errorf("select common channel avatars: %w", err)
	}
	channelByID := make(map[int64]Channel, len(channelRows))
	for _, row := range channelRows {
		channelByID[row.ID] = channelFromRow(row)
	}
	channels := make([]Channel, 0, len(channelIDs))
	members := make(map[int64]ChannelMember, len(channelIDs))
	for _, common := range page.Chats {
		if common.PeerType != PeerTypeChannel {
			continue
		}
		channel, ok := channelByID[common.Supergroup.ID]
		if !ok {
			return CommonChatsPage{}, fmt.Errorf("common channel %d disappeared in snapshot", common.Supergroup.ID)
		}
		channel.Username = common.Supergroup.Username
		channels = append(channels, channel)
		members[channel.ID] = ChannelMember{UserID: viewerID, Role: common.SupergroupRole}
	}
	if err := s.hydrateCurrentChannelPhotos(ctx, qtx, channels, members); err != nil {
		return CommonChatsPage{}, fmt.Errorf("hydrate common channel photos: %w", err)
	}
	hydrated := make(map[int64]Channel, len(channels))
	for _, channel := range channels {
		hydrated[channel.ID] = channel
	}
	for i := range page.Chats {
		if page.Chats[i].PeerType == PeerTypeChannel {
			page.Chats[i].Supergroup = hydrated[page.Chats[i].Supergroup.ID]
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return CommonChatsPage{}, fmt.Errorf("commit common chats snapshot: %w", err)
	}
	return page, nil
}
