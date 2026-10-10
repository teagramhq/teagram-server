package e2e_test

import (
	"context"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

// seedPhoneUsers creates the existing phone accounts used by e2e login flows.
// Unknown phone sign-in is intentionally refused, so tests that exercise
// authenticated behavior must provision their fixtures explicitly.
func seedPhoneUsers(t *testing.T, ctx context.Context, st *store.Store, phones ...string) {
	t.Helper()
	seedPhoneUsersWithDiagnosticID(t, ctx, st, "", phones...)
}

func seedPhoneUsersWithDiagnosticID(t *testing.T, ctx context.Context, st *store.Store, diagnosticID string, phones ...string) {
	t.Helper()
	for _, phone := range phones {
		if _, err := st.CreateUser(ctx, phone); err != nil {
			smokeFailuref(t, diagnosticID, "seed phone user %s: %v", phone, err)
		}
	}
}
