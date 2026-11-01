package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// ChannelHistoryUser is one page author with the live access and profile state
// needed to render a channel history response.
type ChannelHistoryUser struct {
	User       User
	Entitled   bool
	Contact    Contact
	Visibility LastSeenVisibility
}

// ChannelHistoryUsersForViewer loads page authors and their profile metadata
// in one statement, keeping query count independent of page author count.
func (s *Store) ChannelHistoryUsersForViewer(ctx context.Context, viewerID int64, ids []int64) ([]ChannelHistoryUser, error) {
	if viewerID <= 0 {
		return nil, errors.New("channel history users: viewer ID must be positive")
	}
	ids = uniqueInt64s(ids)
	if len(ids) == 0 {
		return []ChannelHistoryUser{}, nil
	}
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("channel history users: user IDs must be positive")
		}
	}

	rows, err := s.q.ChannelHistoryUsersForViewer(ctx, db.ChannelHistoryUsersForViewerParams{
		ViewerID: viewerID,
		Ids:      ids,
	})
	if err != nil {
		return nil, fmt.Errorf("select channel history users: %w", err)
	}

	users := make([]ChannelHistoryUser, 0, len(rows))
	for _, row := range rows {
		user := UserFromDB(db.UserByIDRow{
			ID:         row.ID,
			Phone:      row.Phone,
			FirstName:  row.FirstName,
			LastName:   row.LastName,
			CreatedAt:  row.CreatedAt,
			IsOnline:   row.IsOnline,
			LastSeenAt: row.LastSeenAt,
			Username:   row.Username,
			Kind:       row.Kind,
		})
		visibility := LastSeenVisibility{Level: LastSeenAccessCoarse, Bucket: LastSeenBucketRecently}
		if row.ID != viewerID {
			visibility, err = lastSeenVisibility(user, lastSeenPrivacyInputs{
				baseRule:        row.OwnerBaseRule,
				ownerBlocked:    row.OwnerBlocked,
				ownerDisallowed: row.OwnerDisallowed,
				ownerAllowed:    row.OwnerAllowed,
				ownerContact:    row.OwnerContact,
				viewerHides:     row.ViewerHidesFromOwner,
			}, s.now())
			if err != nil {
				return nil, fmt.Errorf("evaluate channel history user %d status privacy: %w", row.ID, err)
			}
		}

		contact := Contact{}
		if row.HasContact {
			contact = Contact{UserID: row.ID, Mutual: row.MutualContact}
		}
		users = append(users, ChannelHistoryUser{
			User:       user,
			Entitled:   row.Entitled,
			Contact:    contact,
			Visibility: visibility,
		})
	}
	return users, nil
}
