package store

import (
	"context"
	"errors"
	"fmt"

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
// newest first. Membership and Count come from the same database statement
// snapshot, and Count is calculated before applying maxID and limit.
func (s *Store) CommonChatsForUsers(ctx context.Context, viewerID, targetID, maxID int64, limit int32) (CommonChatsPage, error) {
	if viewerID <= 0 || targetID <= 0 {
		return CommonChatsPage{}, errors.New("common chats: user ids must be positive")
	}
	if limit <= 0 {
		return CommonChatsPage{}, errors.New("common chats: limit must be positive")
	}

	rows, err := s.q.CommonChatsForUsers(ctx, db.CommonChatsForUsersParams{
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
	return page, nil
}
