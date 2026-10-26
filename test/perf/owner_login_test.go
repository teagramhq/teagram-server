//go:build performance

package perf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

const (
	fixtureOwnerUsername = "perftestowner"
	fixtureOwnerPassword = "perf-fixture-password-2026"
)

func TestOwnerUsernamePasswordLogin(t *testing.T) {
	dir := perfDir(t)
	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range fixtures {
		t.Run(s.Name, func(t *testing.T) {
			dsn, ok := fixtureDSN(s)
			if !ok {
				t.Skipf("TG_PERF_DSN_%s is not set", envName(s))
			}
			f, err := loadFixture(fixturePath(dir, s))
			if err != nil {
				t.Fatalf("fixture %s: %v (run TestSeedFixtures first)", s.Name, err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			srv := bootServer(t, ctx, dsn, blobDir(dir, s), key)
			defer srv.stop()

			client := srv.newClient(nil, &session.StorageMemory{})
			if err := client.Run(ctx, func(ctx context.Context) error {
				api := client.API()
				sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
					PhoneNumber: fixtureOwnerUsername,
					APIID:       1,
					APIHash:     "hash",
				})
				if err != nil {
					return fmt.Errorf("auth.sendCode: %w", err)
				}
				sentCode, ok := sent.(*tg.AuthSentCode)
				if !ok {
					return fmt.Errorf("auth.sendCode response = %T, want *tg.AuthSentCode", sent)
				}

				_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{
					PhoneNumber:   fixtureOwnerUsername,
					PhoneCodeHash: sentCode.PhoneCodeHash,
					PhoneCode:     "fixture-login",
				})
				if !tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
					return fmt.Errorf("auth.signIn error = %v, want SESSION_PASSWORD_NEEDED", err)
				}

				password, err := api.AccountGetPassword(ctx)
				if err != nil {
					return fmt.Errorf("account.getPassword: %w", err)
				}
				proof, err := auth.PasswordHash([]byte(fixtureOwnerPassword), password.SRPID, password.SRPB, password.SecureRandom, password.CurrentAlgo)
				if err != nil {
					return fmt.Errorf("compute SRP proof: %w", err)
				}
				result, err := api.AuthCheckPassword(ctx, proof)
				if err != nil {
					return fmt.Errorf("auth.checkPassword: %w", err)
				}
				authorization, ok := result.(*tg.AuthAuthorization)
				if !ok || authorization.User == nil {
					return fmt.Errorf("auth.checkPassword response = %T, want authorization", result)
				}
				user, ok := authorization.User.(*tg.User)
				if !ok || user.ID != f.Owner || user.Username != fixtureOwnerUsername {
					return fmt.Errorf("auth.checkPassword user = %#v, want owner %d with username %q", authorization.User, f.Owner, fixtureOwnerUsername)
				}

				full, err := api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
				if err != nil {
					return fmt.Errorf("authorized users.getFullUser: %w", err)
				}
				if len(full.Users) != 1 {
					return fmt.Errorf("users.getFullUser users = %d, want one owner", len(full.Users))
				}
				self, ok := full.Users[0].(*tg.User)
				if !ok || self.ID != f.Owner || self.Username != fixtureOwnerUsername {
					return fmt.Errorf("users.getFullUser self = %#v, want owner %d with username %q", full.Users, f.Owner, fixtureOwnerUsername)
				}

				contactsResult, err := api.ContactsGetContacts(ctx, 0)
				if err != nil {
					return fmt.Errorf("authorized contacts.getContacts: %w", err)
				}
				contacts, ok := contactsResult.(*tg.ContactsContacts)
				if !ok || contacts.SavedCount != f.Spec.Contacts || len(contacts.Contacts) != f.Spec.Contacts {
					return fmt.Errorf("contacts.getContacts result = %#v, want %d fixture contacts", contactsResult, f.Spec.Contacts)
				}
				return nil
			}); err != nil {
				t.Fatalf("owner username/password login: %v", err)
			}
		})
	}
}
