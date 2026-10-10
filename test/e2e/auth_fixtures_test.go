package e2e_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

// seedPhoneUsers creates legacy phone accounts for phone lookup tests.
func seedPhoneUsers(t *testing.T, ctx context.Context, st *store.Store, phones ...string) {
	t.Helper()
	for _, phone := range phones {
		if _, err := st.CreateUser(ctx, phone); err != nil {
			t.Fatalf("seed phone user %s: %v", phone, err)
		}
	}
}

func usernameUserByIdentity(ctx context.Context, st *store.Store, identity string) (store.User, bool, error) {
	username, ok, err := st.UserByUsernameWithLoginMode(ctx, smokeUsernameForPhone(identity))
	if err != nil || !ok {
		return store.User{}, ok, err
	}
	return st.UserByID(ctx, username.ID)
}

func seedUsernameUsers(t *testing.T, ctx context.Context, st *store.Store, identities ...string) {
	t.Helper()
	seedUsernameUsersWithPassword(t, ctx, st, smokeUsernamePassword, identities...)
}

func seedUsernameUsersWithPassword(t *testing.T, ctx context.Context, st *store.Store, password string, identities ...string) {
	t.Helper()
	for _, identity := range identities {
		handle := smokeUsernameForPhone(identity)
		user, err := st.CreateUsernameUser(ctx, handle, "", "")
		if err != nil {
			t.Fatalf("seed username user: %v", err)
		}
		if err := st.ClaimUsername(ctx, user.ID, handle); err != nil {
			t.Fatalf("claim fixture username: %v", err)
		}
		credential := smokePasswordFor(t, user.ID)
		if password != smokeUsernamePassword {
			verifier, salt1, salt2, err := testComputeSRPVerifier([]byte(password))
			if err != nil {
				t.Fatalf("compute fixture password verifier: %v", err)
			}
			credential.Verifier = verifier
			credential.Salt1 = salt1
			credential.Salt2 = salt2
		}
		if err := st.UpsertPassword(ctx, credential); err != nil {
			t.Fatalf("seed fixture password: %v", err)
		}
	}
}

func smokePhoneForUsername(username string) (string, bool) {
	digits, ok := strings.CutPrefix(username, "smoke")
	if !ok || digits == "" {
		return "", false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return "+" + digits, true
}

const smokeUsernamePassword = "smoke-fixture-password"

var smokePasswordMaterial struct {
	sync.Once

	verifier []byte
	salt1    []byte
	salt2    []byte
	err      error
}

func smokeUsernameForPhone(phone string) string {
	return "smoke" + strings.TrimPrefix(phone, "+")
}

func seedSmokeUsers(t *testing.T, f *smokeFixture, phones ...string) {
	t.Helper()
	handles := make(map[string]string, len(phones))
	for _, phone := range phones {
		handles[phone] = smokeUsernameForPhone(phone)
	}
	seedSmokeUsersWithHandles(t, f, phones, handles)
}

func seedSmokeUsersWithHandles(t *testing.T, f *smokeFixture, phones []string, handles map[string]string) {
	t.Helper()
	if f.authHandles == nil {
		f.authHandles = make(map[string]string, len(phones))
	}
	for _, phone := range phones {
		handle, ok := handles[phone]
		if !ok {
			t.Fatal("missing smoke login handle for fixture identity")
		}
		user, err := f.store.CreateUsernameUser(f.ctx, handle, "Smoke", "")
		if err != nil {
			t.Fatalf("seed username account: %v", err)
		}
		if err := f.store.ClaimUsername(f.ctx, user.ID, handle); err != nil {
			t.Fatalf("claim smoke username: %v", err)
		}
		if err := f.store.UpsertPassword(f.ctx, smokePasswordFor(t, user.ID)); err != nil {
			t.Fatalf("seed smoke password verifier: %v", err)
		}
		f.authHandles[phone] = handle
	}
}

func smokePasswordFor(t *testing.T, userID int64) store.UserPassword {
	t.Helper()
	smokePasswordMaterial.Do(func() {
		smokePasswordMaterial.verifier, smokePasswordMaterial.salt1, smokePasswordMaterial.salt2, smokePasswordMaterial.err =
			testComputeSRPVerifier([]byte(smokeUsernamePassword))
	})
	if smokePasswordMaterial.err != nil {
		t.Fatalf("compute smoke SRP verifier: %v", smokePasswordMaterial.err)
	}
	return store.UserPassword{
		UserID:   userID,
		Salt1:    append([]byte(nil), smokePasswordMaterial.salt1...),
		Salt2:    append([]byte(nil), smokePasswordMaterial.salt2...),
		Verifier: append([]byte(nil), smokePasswordMaterial.verifier...),
	}
}

func smokeAuthHandle(f *smokeFixture, identity string) string {
	if handle, ok := f.authHandles[identity]; ok {
		return handle
	}
	return smokeUsernameForPhone(identity)
}
