package api

import "github.com/teagramhq/teagram-server/internal/store"

// lastSeenPrivacySubjects contains the viewer and non-self users whose stored
// status can expose presence information. The evaluator handles the viewer
// without a database query and returns the coarse self status.
func lastSeenPrivacySubjects(users []store.User, viewerID int64) []store.User {
	subjects := make([]store.User, 0, len(users))
	for _, user := range users {
		if user.ID == viewerID || user.IsOnline || user.LastSeenAt != nil {
			subjects = append(subjects, user)
		}
	}
	return subjects
}
