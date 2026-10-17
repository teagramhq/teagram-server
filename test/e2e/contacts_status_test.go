package e2e_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeContactsGetStatuses(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const (
		phoneAlice   = "+15551020201"
		phoneBob     = "+15551020202"
		phoneCarol   = "+15551020203"
		phoneBlocked = "+15551020204"
		phoneOther   = "+15551020205"
		phoneEmpty   = "+15551020206"
	)
	seedSmokeUsers(t, f, phoneAlice, phoneBob, phoneCarol, phoneBlocked, phoneOther, phoneEmpty)
	alice := newSmokeClient(t, f, "contacts-status-alice", phoneAlice)
	empty := newSmokeClient(t, f, "contacts-status-empty", phoneEmpty)
	bob := smokeUserByPhone(t, f, phoneBob)
	carol := smokeUserByPhone(t, f, phoneCarol)
	blocked := smokeUserByPhone(t, f, phoneBlocked)
	other := smokeUserByPhone(t, f, phoneOther)

	for _, targetID := range []int64{bob.ID, carol.ID, blocked.ID} {
		if _, err := f.store.AddContact(f.ctx, alice.id, targetID); err != nil {
			t.Fatalf("add contact %d: %v", targetID, err)
		}
	}
	if _, err := f.store.AddContact(f.ctx, other.ID, alice.id); err != nil {
		t.Fatalf("add reverse-only contact: %v", err)
	}
	for _, targetID := range []int64{bob.ID, carol.ID, blocked.ID} {
		if err := f.store.SetUserStatus(f.ctx, targetID, true); err != nil {
			t.Fatalf("mark user %d online: %v", targetID, err)
		}
	}
	if err := f.store.SetLastSeenPrivacy(f.ctx, carol.ID, store.LastSeenPrivacyRule{BaseRule: "nobody"}); err != nil {
		t.Fatalf("hide Carol's last seen: %v", err)
	}
	if _, err := f.store.BlockUser(f.ctx, blocked.ID, alice.id); err != nil {
		t.Fatalf("block Alice: %v", err)
	}

	var statuses []tg.ContactStatus
	var users []tg.UserClass
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		statuses, err = client.ContactsGetStatuses(ctx)
		if err != nil {
			return err
		}
		users, err = client.UsersGetUsers(ctx, []tg.InputUserClass{
			inputUser(alice.id, bob.ID),
			inputUser(alice.id, carol.ID),
			inputUser(alice.id, blocked.ID),
		})
		return err
	}); err != nil {
		t.Fatalf("get contact statuses and matching users: %v", err)
	}
	if len(statuses) != 3 {
		t.Fatalf("contact statuses = %d entries, want three caller-owned contacts", len(statuses))
	}

	wantKinds := map[int64]string{
		bob.ID:     "online",
		carol.ID:   "recently",
		blocked.ID: "empty",
	}
	usersByID := make(map[int64]*tg.User, len(users))
	for _, class := range users {
		user, ok := class.(*tg.User)
		if ok {
			usersByID[user.ID] = user
		}
	}
	statusesByID := make(map[int64]tg.UserStatusClass, len(statuses))
	for _, contactStatus := range statuses {
		if _, expected := wantKinds[contactStatus.UserID]; !expected {
			t.Errorf("contact status includes user %d outside Alice's directed contact list", contactStatus.UserID)
		}
		if _, duplicate := statusesByID[contactStatus.UserID]; duplicate {
			t.Errorf("duplicate contact status for user %d", contactStatus.UserID)
		}
		statusesByID[contactStatus.UserID] = contactStatus.Status
	}
	for userID, kind := range wantKinds {
		status, ok := statusesByID[userID]
		if !ok {
			t.Errorf("contact status missing user %d", userID)
			continue
		}
		assertSmokeStatus(t, status, kind, false)
		user, ok := usersByID[userID]
		if !ok {
			t.Errorf("users.getUsers omitted contact %d", userID)
			continue
		}
		if !contactStatusMatches(status, user.Status) {
			t.Errorf("contact status for %d = %#v, users.getUsers status = %#v", userID, status, user.Status)
		}
	}
	if _, included := statusesByID[other.ID]; included {
		t.Errorf("reverse-only contact %d appeared in Alice's statuses", other.ID)
	}

	var emptyStatuses []tg.ContactStatus
	if err := empty.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		emptyStatuses, err = client.ContactsGetStatuses(ctx)
		return err
	}); err != nil {
		t.Fatalf("get statuses for empty contact list: %v", err)
	}
	if len(emptyStatuses) != 0 {
		t.Fatalf("empty contact status result = %d entries, want none", len(emptyStatuses))
	}
}

func contactStatusMatches(got, want tg.UserStatusClass) bool {
	switch status := got.(type) {
	case *tg.UserStatusOnline:
		other, ok := want.(*tg.UserStatusOnline)
		if !ok {
			return false
		}
		delta := status.Expires - other.Expires
		return delta >= -5 && delta <= 5
	case *tg.UserStatusOffline:
		other, ok := want.(*tg.UserStatusOffline)
		return ok && status.WasOnline == other.WasOnline
	case *tg.UserStatusRecently:
		other, ok := want.(*tg.UserStatusRecently)
		return ok && status.ByMe == other.ByMe
	case *tg.UserStatusLastWeek:
		other, ok := want.(*tg.UserStatusLastWeek)
		return ok && status.ByMe == other.ByMe
	case *tg.UserStatusLastMonth:
		other, ok := want.(*tg.UserStatusLastMonth)
		return ok && status.ByMe == other.ByMe
	case *tg.UserStatusEmpty:
		_, ok := want.(*tg.UserStatusEmpty)
		return ok
	default:
		return false
	}
}

func smokeUserByPhone(t *testing.T, f *smokeFixture, phone string) store.User {
	t.Helper()
	resolved, ok, err := f.store.UsernameByHandle(f.ctx, smokeUsernameForPhone(phone))
	if err != nil || !ok {
		t.Fatalf("lookup smoke user %s: ok=%v err=%v", phone, ok, err)
	}
	if resolved.Kind != store.UsernameKindUser {
		t.Fatalf("smoke account %s resolved to kind %d, want user", phone, resolved.Kind)
	}
	return resolved.User
}
