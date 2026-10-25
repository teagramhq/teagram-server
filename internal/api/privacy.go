package api

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleGetPrivacy(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetPrivacyRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if isLastSeenPrivacyKey(req.Key) {
		return h.lastSeenPrivacyRulesForOwner(r.Ctx, r.UserID)
	}
	if isAddedByPhonePrivacyKey(req.Key) {
		return h.addedByPhonePrivacyRulesForOwner(r.Ctx, r.UserID)
	}
	return nil, errPrivacyKeyInvalid
}

func (h *handlers) handleSetPrivacy(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountSetPrivacyRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	isLastSeen := isLastSeenPrivacyKey(req.Key)
	isAddedByPhone := isAddedByPhonePrivacyKey(req.Key)
	if !isLastSeen && !isAddedByPhone {
		return nil, errPrivacyKeyInvalid
	}
	if lastSeenPrivacyEntryCount(req.Rules) > store.MaxLastSeenPrivacyUsers {
		return nil, errPrivacyTooLong
	}

	rule, allowUsers, disallowUsers, err := parseLastSeenPrivacyRules(req.Rules)
	if err != nil {
		return nil, err
	}
	rule.AllowedUserIDs, err = h.resolveLastSeenPrivacyUsers(r, allowUsers)
	if err != nil {
		return nil, err
	}
	rule.DisallowedUserIDs, err = h.resolveLastSeenPrivacyUsers(r, disallowUsers)
	if err != nil {
		return nil, err
	}
	if err := h.checkRateLimit(r, "account_set_privacy", h.rateLimitUpdateProfile); err != nil {
		return nil, err
	}
	var setErr error
	if isLastSeen {
		setErr = h.store.SetLastSeenPrivacy(r.Ctx, r.UserID, rule)
	} else {
		setErr = h.store.SetAddedByPhonePrivacy(r.Ctx, r.UserID, rule)
	}
	if err := setErr; err != nil {
		switch {
		case errors.Is(err, store.ErrLastSeenPrivacyTooLong):
			return nil, errPrivacyTooLong
		case errors.Is(err, store.ErrInvalidLastSeenPrivacy):
			return nil, errPrivacyValueInvalid
		default:
			h.log.Error("set account privacy", "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
	}
	if isLastSeen {
		return h.lastSeenPrivacyRulesForOwner(r.Ctx, r.UserID)
	}
	return h.addedByPhonePrivacyRulesForOwner(r.Ctx, r.UserID)
}

func isLastSeenPrivacyKey(key tg.InputPrivacyKeyClass) bool {
	_, ok := key.(*tg.InputPrivacyKeyStatusTimestamp)
	return ok
}

func isAddedByPhonePrivacyKey(key tg.InputPrivacyKeyClass) bool {
	_, ok := key.(*tg.InputPrivacyKeyAddedByPhone)
	return ok
}

func lastSeenPrivacyEntryCount(rules []tg.InputPrivacyRuleClass) int {
	count := 0
	for _, rule := range rules {
		switch value := rule.(type) {
		case *tg.InputPrivacyValueAllowUsers:
			if value != nil {
				count += len(value.Users)
			}
		case *tg.InputPrivacyValueDisallowUsers:
			if value != nil {
				count += len(value.Users)
			}
		}
	}
	return count
}

func parseLastSeenPrivacyRules(rules []tg.InputPrivacyRuleClass) (store.LastSeenPrivacyRule, []tg.InputUserClass, []tg.InputUserClass, error) {
	var out store.LastSeenPrivacyRule
	var allowUsers, disallowUsers []tg.InputUserClass
	hasBase := false
	setBase := func(baseRule string) error {
		if hasBase {
			return errPrivacyValueInvalid
		}
		hasBase = true
		out.BaseRule = baseRule
		return nil
	}
	for _, rule := range rules {
		switch value := rule.(type) {
		case *tg.InputPrivacyValueAllowAll:
			if value == nil {
				return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
			}
			if err := setBase("everybody"); err != nil {
				return store.LastSeenPrivacyRule{}, nil, nil, err
			}
		case *tg.InputPrivacyValueAllowContacts:
			if value == nil {
				return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
			}
			if err := setBase("contacts"); err != nil {
				return store.LastSeenPrivacyRule{}, nil, nil, err
			}
		case *tg.InputPrivacyValueDisallowAll:
			if value == nil {
				return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
			}
			if err := setBase("nobody"); err != nil {
				return store.LastSeenPrivacyRule{}, nil, nil, err
			}
		case *tg.InputPrivacyValueAllowUsers:
			if value == nil {
				return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
			}
			allowUsers = append(allowUsers, value.Users...)
		case *tg.InputPrivacyValueDisallowUsers:
			if value == nil {
				return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
			}
			disallowUsers = append(disallowUsers, value.Users...)
		default:
			return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
		}
	}
	if !hasBase {
		return store.LastSeenPrivacyRule{}, nil, nil, errPrivacyValueInvalid
	}
	return out, allowUsers, disallowUsers, nil
}

func (h *handlers) resolveLastSeenPrivacyUsers(r *mtproto.Request, users []tg.InputUserClass) ([]int64, error) {
	ids := make([]int64, 0, len(users))
	seen := make(map[int64]struct{}, len(users))
	for _, input := range users {
		id, err := h.inputUserID(input, r.UserID)
		if err != nil || id == r.UserID {
			return nil, errPeerIDInvalid
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return []int64{}, nil
	}
	known, err := h.store.UsersByID(r.Ctx, ids)
	if err != nil {
		h.log.Error("set last-seen privacy: resolve users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if len(known) != len(ids) {
		return nil, errPeerIDInvalid
	}
	return ids, nil
}

func (h *handlers) lastSeenPrivacyRulesForOwner(ctx context.Context, ownerID int64) (*tg.AccountPrivacyRules, error) {
	rule, _, err := h.store.GetLastSeenPrivacy(ctx, ownerID)
	if err != nil {
		h.log.Error("get last-seen privacy", "user_id", ownerID, "err", err)
		return nil, errInternal
	}
	return h.privacyRulesForOwner(ctx, ownerID, rule)
}

func (h *handlers) addedByPhonePrivacyRulesForOwner(ctx context.Context, ownerID int64) (*tg.AccountPrivacyRules, error) {
	rule, _, err := h.store.GetAddedByPhonePrivacy(ctx, ownerID)
	if err != nil {
		h.log.Error("get added-by-phone privacy", "user_id", ownerID, "err", err)
		return nil, errInternal
	}
	return h.privacyRulesForOwner(ctx, ownerID, rule)
}

func (h *handlers) privacyRulesForOwner(ctx context.Context, ownerID int64, rule store.LastSeenPrivacyRule) (*tg.AccountPrivacyRules, error) {
	var rules []tg.PrivacyRuleClass
	switch rule.BaseRule {
	case "everybody":
		rules = append(rules, &tg.PrivacyValueAllowAll{})
	case "contacts":
		rules = append(rules, &tg.PrivacyValueAllowContacts{})
	case "nobody":
		rules = append(rules, &tg.PrivacyValueDisallowAll{})
	default:
		h.log.Error("get account privacy: invalid stored base rule", "user_id", ownerID, "base_rule", rule.BaseRule)
		return nil, errInternal
	}
	// Disallow must precede allow because clients keep the first rule per user.
	if len(rule.DisallowedUserIDs) > 0 {
		rules = append(rules, &tg.PrivacyValueDisallowUsers{Users: slices.Clone(rule.DisallowedUserIDs)})
	}
	if len(rule.AllowedUserIDs) > 0 {
		rules = append(rules, &tg.PrivacyValueAllowUsers{Users: slices.Clone(rule.AllowedUserIDs)})
	}

	userIDs := append(slices.Clone(rule.AllowedUserIDs), rule.DisallowedUserIDs...)
	slices.Sort(userIDs)
	userIDs = slices.Compact(userIDs)
	usersByID, err := h.store.UsersByID(ctx, userIDs)
	if err != nil {
		h.log.Error("get account privacy users", "user_id", ownerID, "err", err)
		return nil, errInternal
	}
	users := make([]store.User, 0, len(userIDs))
	for _, id := range userIDs {
		user, ok := usersByID[id]
		if !ok {
			h.log.Error("get account privacy user missing", "user_id", ownerID, "entry_user_id", id)
			return nil, errInternal
		}
		users = append(users, user)
	}
	rendered, err := h.usersToTL(ctx, users, ownerID, false)
	if err != nil {
		h.log.Error("get account privacy: render users", "user_id", ownerID, "err", err)
		return nil, errInternal
	}
	wireUsers := make([]tg.UserClass, len(rendered))
	for i, user := range rendered {
		wireUsers[i] = user
	}
	if rules == nil {
		return nil, fmt.Errorf("get account privacy: no base rule for owner %d", ownerID)
	}
	return &tg.AccountPrivacyRules{Rules: rules, Chats: []tg.ChatClass{}, Users: wireUsers}, nil
}
