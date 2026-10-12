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

const MaxLastSeenPrivacyUsers = 1000

var (
	ErrLastSeenPrivacyTooLong = errors.New("last-seen privacy has too many user entries")
	ErrInvalidLastSeenPrivacy = errors.New("invalid last-seen privacy rule")
)

// LastSeenPrivacyRule is the persisted base decision and its user exceptions.
type LastSeenPrivacyRule struct {
	BaseRule          string
	AllowedUserIDs    []int64
	DisallowedUserIDs []int64
}

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

	for _, row := range rows {
		visibility, err := lastSeenVisibility(owner, lastSeenPrivacyInputs{
			baseRule:        row.OwnerBaseRule,
			ownerBlocked:    row.OwnerBlocked,
			ownerDisallowed: row.OwnerDisallowed,
			ownerAllowed:    row.OwnerAllowed,
			ownerContact:    row.OwnerContact,
			viewerHides:     row.ViewerHidesFromOwner,
		}, s.now())
		if err != nil {
			return nil, fmt.Errorf("evaluate last-seen privacy: %w", err)
		}
		results[row.ViewerID] = visibility
	}
	return results, nil
}

// EvaluateLastSeenPrivacyForViewer resolves status visibility for a viewer
// across a set of owners in one query. The zero value for an invalid owner is
// hidden; a missing database row makes the batch fail instead of granting
// access to the owners that did resolve.
func (s *Store) EvaluateLastSeenPrivacyForViewer(ctx context.Context, viewerID int64, owners []User) (map[int64]LastSeenVisibility, error) {
	if viewerID <= 0 {
		return nil, errors.New("evaluate last-seen privacy for viewer: viewer ID must be positive")
	}

	results := make(map[int64]LastSeenVisibility, len(owners))
	ownersByID := make(map[int64]User, len(owners))
	queryOwnerIDs := make([]int64, 0, len(owners))
	for _, owner := range owners {
		if _, exists := ownersByID[owner.ID]; exists {
			continue
		}
		ownersByID[owner.ID] = owner
		results[owner.ID] = LastSeenVisibility{}
		if owner.ID == viewerID {
			results[owner.ID] = LastSeenVisibility{
				Level:  LastSeenAccessCoarse,
				Bucket: LastSeenBucketRecently,
			}
			continue
		}
		if owner.ID > 0 {
			queryOwnerIDs = append(queryOwnerIDs, owner.ID)
		}
	}
	if len(queryOwnerIDs) == 0 {
		return results, nil
	}

	rows, err := s.q.EvaluateLastSeenPrivacyForViewer(ctx, db.EvaluateLastSeenPrivacyForViewerParams{
		ViewerID: viewerID,
		OwnerIds: queryOwnerIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate last-seen privacy for viewer: %w", err)
	}
	if len(rows) != len(queryOwnerIDs) {
		return nil, fmt.Errorf("evaluate last-seen privacy for viewer: got %d results for %d owners", len(rows), len(queryOwnerIDs))
	}
	for _, row := range rows {
		owner, ok := ownersByID[row.OwnerID]
		if !ok {
			return nil, fmt.Errorf("evaluate last-seen privacy for viewer: unexpected owner %d", row.OwnerID)
		}
		visibility, err := lastSeenVisibility(owner, lastSeenPrivacyInputs{
			baseRule:        row.OwnerBaseRule,
			ownerBlocked:    row.OwnerBlocked,
			ownerDisallowed: row.OwnerDisallowed,
			ownerAllowed:    row.OwnerAllowed,
			ownerContact:    row.OwnerContact,
			viewerHides:     row.ViewerHidesFromOwner,
		}, s.now())
		if err != nil {
			return nil, fmt.Errorf("evaluate last-seen privacy for viewer: %w", err)
		}
		results[row.OwnerID] = visibility
	}
	return results, nil
}

// GetLastSeenPrivacy returns the saved status rule, with no rule row represented
// as Everybody so clients and reads retain the legacy default.
func (s *Store) GetLastSeenPrivacy(ctx context.Context, ownerID int64) (LastSeenPrivacyRule, bool, error) {
	if ownerID <= 0 {
		return LastSeenPrivacyRule{}, false, errors.New("get last-seen privacy: owner ID must be positive")
	}
	baseRule, err := s.q.GetLastSeenPrivacyRule(ctx, ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return LastSeenPrivacyRule{BaseRule: "everybody"}, false, nil
	}
	if err != nil {
		return LastSeenPrivacyRule{}, false, fmt.Errorf("get last-seen privacy rule: %w", err)
	}
	rows, err := s.q.GetLastSeenPrivacyRuleUsers(ctx, ownerID)
	if err != nil {
		return LastSeenPrivacyRule{}, false, fmt.Errorf("get last-seen privacy users: %w", err)
	}
	rule := LastSeenPrivacyRule{BaseRule: baseRule}
	for _, row := range rows {
		switch row.EntryType {
		case "allow":
			rule.AllowedUserIDs = append(rule.AllowedUserIDs, row.UserID)
		case "disallow":
			rule.DisallowedUserIDs = append(rule.DisallowedUserIDs, row.UserID)
		default:
			return LastSeenPrivacyRule{}, false, fmt.Errorf("get last-seen privacy users: unknown entry type %q", row.EntryType)
		}
	}
	return rule, true, nil
}

// SetLastSeenPrivacy atomically replaces the owner's saved status rule and its
// allow/disallow lists. It validates the entire input before starting a write.
func (s *Store) SetLastSeenPrivacy(ctx context.Context, ownerID int64, rule LastSeenPrivacyRule) error {
	if ownerID <= 0 || !validLastSeenBaseRule(rule.BaseRule) {
		return ErrInvalidLastSeenPrivacy
	}
	if len(rule.AllowedUserIDs)+len(rule.DisallowedUserIDs) > MaxLastSeenPrivacyUsers {
		return ErrLastSeenPrivacyTooLong
	}
	allowed, err := normalizeLastSeenPrivacyUsers(ownerID, rule.AllowedUserIDs)
	if err != nil {
		return err
	}
	disallowed, err := normalizeLastSeenPrivacyUsers(ownerID, rule.DisallowedUserIDs)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("set last-seen privacy begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)
	if err := qtx.UpsertLastSeenPrivacyRule(ctx, db.UpsertLastSeenPrivacyRuleParams{
		OwnerID:  ownerID,
		BaseRule: rule.BaseRule,
	}); err != nil {
		return fmt.Errorf("set last-seen privacy rule: %w", err)
	}
	if err := qtx.DeleteLastSeenPrivacyRuleUsers(ctx, ownerID); err != nil {
		return fmt.Errorf("set last-seen privacy clear users: %w", err)
	}
	if len(allowed) > 0 {
		if err := qtx.InsertLastSeenPrivacyAllowedUsers(ctx, db.InsertLastSeenPrivacyAllowedUsersParams{
			OwnerID: ownerID,
			Column2: allowed,
		}); err != nil {
			return fmt.Errorf("set last-seen privacy allow users: %w", err)
		}
	}
	if len(disallowed) > 0 {
		if err := qtx.InsertLastSeenPrivacyDisallowedUsers(ctx, db.InsertLastSeenPrivacyDisallowedUsersParams{
			OwnerID: ownerID,
			Column2: disallowed,
		}); err != nil {
			return fmt.Errorf("set last-seen privacy disallow users: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("set last-seen privacy commit: %w", err)
	}
	return nil
}

func validLastSeenBaseRule(rule string) bool {
	switch rule {
	case "everybody", "contacts", "nobody":
		return true
	default:
		return false
	}
}

func normalizeLastSeenPrivacyUsers(ownerID int64, ids []int64) ([]int64, error) {
	seen := make(map[int64]struct{}, len(ids))
	users := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || id == ownerID {
			return nil, ErrInvalidLastSeenPrivacy
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		users = append(users, id)
	}
	slices.Sort(users)
	return users, nil
}

type lastSeenPrivacyInputs struct {
	baseRule        string
	ownerBlocked    bool
	ownerDisallowed bool
	ownerAllowed    bool
	ownerContact    bool
	viewerHides     bool
}

func lastSeenVisibility(owner User, input lastSeenPrivacyInputs, now time.Time) (LastSeenVisibility, error) {
	if input.ownerBlocked {
		return LastSeenVisibility{}, nil
	}
	baseAllows := false
	switch input.baseRule {
	case "everybody":
		baseAllows = true
	case "contacts":
		baseAllows = input.ownerContact
	case "nobody":
	default:
		return LastSeenVisibility{}, fmt.Errorf("unknown base rule %q", input.baseRule)
	}
	if input.ownerDisallowed || input.viewerHides || (!input.ownerAllowed && !baseAllows) {
		return LastSeenVisibility{
			Level:  LastSeenAccessCoarse,
			Bucket: lastSeenBucket(owner, now),
			ByMe:   input.viewerHides,
		}, nil
	}
	return LastSeenVisibility{Level: LastSeenAccessExact}, nil
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
