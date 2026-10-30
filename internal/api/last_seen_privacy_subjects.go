package api

import "github.com/teagramhq/teagram-server/internal/store"

// lastSeenPrivacySubjects contains only non-self users whose stored status can
// expose presence information.
func lastSeenPrivacySubjects(users []store.User, viewerID int64) []store.User {
	subjects := make([]store.User, 0, len(users))
	for _, user := range users {
		if user.ID != viewerID && (user.IsOnline || user.LastSeenAt != nil) {
			subjects = append(subjects, user)
		}
	}
	return subjects
}
