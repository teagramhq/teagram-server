package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// LastSeenAccess is the level of last-seen detail a viewer may receive.
// Its zero value is hidden so an incomplete evaluation cannot grant access.
type LastSeenAccess uint8

const (
	LastSeenAccessHidden LastSeenAccess = iota
	LastSeenAccessCoarse
	LastSeenAccessExact
)

// LastSeenBucket is the server-computed status shown when exact access is
// unavailable.
type LastSeenBucket uint8

const (
	LastSeenBucketEmpty LastSeenBucket = iota
	LastSeenBucketRecently
	LastSeenBucketLastWeek
	LastSeenBucketLastMonth
)

// LastSeenVisibility contains the access level and, when coarse, the bucket
// derived from the owner's status. ByMe marks a coarse result caused by the
// viewer's own privacy rule.
type LastSeenVisibility struct {
	Level  LastSeenAccess
	Bucket LastSeenBucket
	ByMe   bool
}

// EvaluateLastSeenPrivacy resolves status visibility for one owner and a batch
// of viewers in one query. A missing owner rule keeps the legacy everybody
// rule. Non-positive viewer IDs remain hidden; positive IDs missing from the
// database make the batch incomplete and return an error.
func (s *Store) EvaluateLastSeenPrivacy(ctx context.Context, owner User, viewerIDs []int64) (map[int64]LastSeenVisibility, error) {
	if owner.ID <= 0 {
		return nil, errors.New("evaluate last-seen privacy: owner ID must be positive")
	}

	results := make(map[int64]LastSeenVisibility, len(viewerIDs))
	queryViewerIDs := make([]int64, 0, len(viewerIDs))
	seen := make(map[int64]struct{}, len(viewerIDs))
	for _, viewerID := range viewerIDs {
		if _, exists := seen[viewerID]; exists {
			continue
		}
		seen[viewerID] = struct{}{}
		results[viewerID] = LastSeenVisibility{}
		if viewerID == owner.ID {
			results[viewerID] = LastSeenVisibility{
				Level:  LastSeenAccessCoarse,
				Bucket: LastSeenBucketRecently,
			}
			continue
		}
		if viewerID > 0 {
			queryViewerIDs = append(queryViewerIDs, viewerID)
		}
	}
	if len(queryViewerIDs) == 0 {
		return results, nil
	}

	rows, err := s.q.EvaluateLastSeenPrivacy(ctx, db.EvaluateLastSeenPrivacyParams{
		OwnerID:   owner.ID,
		ViewerIds: queryViewerIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate last-seen privacy: %w", err)
	}
	if len(rows) != len(queryViewerIDs) {
		return nil, fmt.Errorf("evaluate last-seen privacy: got %d results for %d viewers", len(rows), len(queryViewerIDs))
	}

	bucket := lastSeenBucket(owner, s.now())
	for _, row := range rows {
		if row.OwnerBlocked {
			continue
		}

		baseAllows := false
		switch row.OwnerBaseRule {
		case "everybody":
			baseAllows = true
		case "contacts":
			baseAllows = row.OwnerContact
		case "nobody":
		default:
			return nil, fmt.Errorf("evaluate last-seen privacy: unknown base rule %q", row.OwnerBaseRule)
		}
		ownerAllows := !row.OwnerDisallowed && (row.OwnerAllowed || baseAllows)
		if !ownerAllows || row.ViewerHidesFromOwner {
			results[row.ViewerID] = LastSeenVisibility{
				Level:  LastSeenAccessCoarse,
				Bucket: bucket,
				ByMe:   row.ViewerHidesFromOwner,
			}
			continue
		}
		results[row.ViewerID] = LastSeenVisibility{Level: LastSeenAccessExact}
	}
	return results, nil
}

func lastSeenBucket(owner User, now time.Time) LastSeenBucket {
	if owner.IsOnline {
		return LastSeenBucketRecently
	}
	if owner.LastSeenAt == nil {
		return LastSeenBucketEmpty
	}

	age := now.Sub(*owner.LastSeenAt)
	switch {
	case age <= 3*24*time.Hour:
		return LastSeenBucketRecently
	case age <= 7*24*time.Hour:
		return LastSeenBucketLastWeek
	case age <= 30*24*time.Hour:
		return LastSeenBucketLastMonth
	default:
		return LastSeenBucketEmpty
	}
}
