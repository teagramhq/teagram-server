package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeLastSeenPrivacy(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneOwner, phoneViewer = "+15551047001", "+15551047002"
	seedSmokeUsers(t, f, phoneOwner, phoneViewer)
	owner := newSmokeClient(t, f, "privacy-owner", phoneOwner)
	viewer := newSmokeClient(t, f, "privacy-viewer", phoneViewer)
	if err := f.store.SetUserStatus(f.ctx, owner.id, true); err != nil {
		t.Fatalf("mark owner online: %v", err)
	}

	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountSetPrivacy(ctx, &tg.AccountSetPrivacyRequest{
			Key:   &tg.InputPrivacyKeyStatusTimestamp{},
			Rules: []tg.InputPrivacyRuleClass{&tg.InputPrivacyValueDisallowAll{}},
		})
		return err
	}); err != nil {
		t.Fatalf("set last-seen privacy to Nobody: %v", err)
	}

	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerUser(owner.id, viewer.id),
			Message:  "privacy smoke",
			RandomID: 1047001,
		})
		return err
	}); err != nil {
		t.Fatalf("establish viewer dialog: %v", err)
	}

	if err := viewer.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      20,
		})
		if err != nil {
			return err
		}
		var users []tg.UserClass
		switch dialogs := result.(type) {
		case *tg.MessagesDialogs:
			users = dialogs.Users
		case *tg.MessagesDialogsSlice:
			users = dialogs.Users
		default:
			return fmt.Errorf("getDialogs response = %T", result)
		}
		user, err := requireSmokeFullUser(users, owner.id, "getDialogs")
		if err != nil {
			return err
		}
		if _, ok := user.Status.(*tg.UserStatusRecently); !ok {
			return fmt.Errorf("owner status = %T, want *tg.UserStatusRecently", user.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("viewer sees coarse last-seen status: %v", err)
	}

	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		rules, err := client.AccountGetPrivacy(ctx, &tg.InputPrivacyKeyStatusTimestamp{})
		if err != nil {
			return err
		}
		if len(rules.Rules) != 1 {
			return fmt.Errorf("getPrivacy returned %d rules, want one", len(rules.Rules))
		}
		if _, ok := rules.Rules[0].(*tg.PrivacyValueDisallowAll); !ok {
			return fmt.Errorf("saved rule = %T, want *tg.PrivacyValueDisallowAll", rules.Rules[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("get saved last-seen privacy: %v", err)
	}
}

func TestLastSeenPrivacyReadPaths(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneOwner, phoneViewer = "+15551047101", "+15551047102"
	seedSmokeUsers(t, f, phoneOwner, phoneViewer)
	owner := newSmokeClient(t, f, "privacy-matrix-owner", phoneOwner)
	viewer := newSmokeClient(t, f, "privacy-matrix-viewer", phoneViewer)
	if err := f.store.SetUserStatus(f.ctx, owner.id, true); err != nil {
		t.Fatalf("mark owner online: %v", err)
	}
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerUser(owner.id, viewer.id),
			Message:  "privacy matrix",
			RandomID: 1047101,
		})
		return err
	}); err != nil {
		t.Fatalf("establish viewer dialog: %v", err)
	}

	bases := []struct {
		name  string
		rule  tg.InputPrivacyRuleClass
		allow bool
	}{
		{name: "everybody", rule: &tg.InputPrivacyValueAllowAll{}, allow: true},
		{name: "contacts", rule: &tg.InputPrivacyValueAllowContacts{}},
		{name: "nobody", rule: &tg.InputPrivacyValueDisallowAll{}},
	}
	edges := []string{"none", "owner-to-viewer", "viewer-to-owner"}
	methods := []string{"getUsers", "getDialogs", "getDifference", "resolveUsername"}
	for _, base := range bases {
		for _, edge := range edges {
			t.Run(base.name+"/"+edge, func(t *testing.T) {
				if _, err := f.store.RemoveContact(f.ctx, owner.id, viewer.id); err != nil {
					t.Fatalf("remove owner contact: %v", err)
				}
				if _, err := f.store.RemoveContact(f.ctx, viewer.id, owner.id); err != nil {
					t.Fatalf("remove viewer contact: %v", err)
				}
				switch edge {
				case "owner-to-viewer":
					if _, err := f.store.AddContact(f.ctx, owner.id, viewer.id); err != nil {
						t.Fatalf("add owner contact: %v", err)
					}
				case "viewer-to-owner":
					if _, err := f.store.AddContact(f.ctx, viewer.id, owner.id); err != nil {
						t.Fatalf("add viewer contact: %v", err)
					}
				}
				if err := setSmokePrivacy(f.ctx, owner, base.rule); err != nil {
					t.Fatalf("set %s rule: %v", base.name, err)
				}
				wantExact := base.allow || base.name == "contacts" && edge == "owner-to-viewer"
				want := "recently"
				if wantExact {
					want = "online"
				}
				for _, method := range methods {
					status, err := readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, method)
					if err != nil {
						t.Fatalf("%s: %v", method, err)
					}
					assertSmokeStatus(t, status, want, false)
				}
			})
		}
	}

	if err := setSmokePrivacy(f.ctx, owner,
		&tg.InputPrivacyValueDisallowAll{},
		&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(owner.id, viewer.id)}},
	); err != nil {
		t.Fatalf("allow-list viewer under Nobody: %v", err)
	}
	status, err := readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read allow-listed status: %v", err)
	}
	assertSmokeStatus(t, status, "online", false)

	if err := setSmokePrivacy(f.ctx, owner,
		&tg.InputPrivacyValueAllowAll{},
		&tg.InputPrivacyValueDisallowUsers{Users: []tg.InputUserClass{inputUser(owner.id, viewer.id)}},
	); err != nil {
		t.Fatalf("disallow-list viewer under Everybody: %v", err)
	}
	status, err = readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read disallowed status: %v", err)
	}
	assertSmokeStatus(t, status, "recently", false)

	if _, err := f.store.BlockUser(f.ctx, owner.id, viewer.id); err != nil {
		t.Fatalf("block viewer: %v", err)
	}
	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}); err != nil {
		t.Fatalf("restore Everybody rule: %v", err)
	}
	status, err = readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read blocked status: %v", err)
	}
	assertSmokeStatus(t, status, "empty", false)
	if _, err := f.store.UnblockUser(f.ctx, owner.id, viewer.id); err != nil {
		t.Fatalf("unblock viewer: %v", err)
	}

	if err := setSmokePrivacy(f.ctx, viewer, &tg.InputPrivacyValueDisallowAll{}); err != nil {
		t.Fatalf("hide viewer's status: %v", err)
	}
	status, err = readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read reciprocal status: %v", err)
	}
	assertSmokeStatus(t, status, "recently", true)

	if err := setSmokePrivacy(f.ctx, owner,
		&tg.InputPrivacyValueAllowAll{},
		&tg.InputPrivacyValueDisallowUsers{Users: []tg.InputUserClass{inputUser(owner.id, viewer.id)}},
	); err != nil {
		t.Fatalf("disallow viewer for getPrivacy rendering: %v", err)
	}
	privacy, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("getPrivacy with a user exception: %v", err)
	}
	if len(privacy.Rules) != 2 {
		t.Fatalf("getPrivacy returned %d rules, want base plus user exception", len(privacy.Rules))
	}
	disallowed, ok := privacy.Rules[1].(*tg.PrivacyValueDisallowUsers)
	if !ok || len(disallowed.Users) != 1 || disallowed.Users[0] != viewer.id {
		t.Fatalf("getPrivacy disallow rule = %#v, want viewer %d", privacy.Rules[1], viewer.id)
	}
	if len(privacy.Users) != 1 {
		t.Fatalf("getPrivacy returned %d users, want viewer", len(privacy.Users))
	}
	listedViewer, ok := privacy.Users[0].(*tg.User)
	if !ok || listedViewer.ID != viewer.id {
		t.Fatalf("getPrivacy user = %#v, want viewer %d", privacy.Users[0], viewer.id)
	}
	if listedViewer.AccessHash != inputUser(owner.id, viewer.id).AccessHash {
		t.Fatalf("getPrivacy access hash = %d, want owner-scoped hash %d", listedViewer.AccessHash, inputUser(owner.id, viewer.id).AccessHash)
	}
	assertSmokeStatus(t, listedViewer.Status, "empty", false)
}

func TestLastSeenPrivacyOverlappingExceptionsClientRoundTrip(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneOwner, phoneViewer = "+15551047601", "+15551047602"
	seedSmokeUsers(t, f, phoneOwner, phoneViewer)
	owner := newSmokeClient(t, f, "privacy-overlap-owner", phoneOwner)
	viewer := newSmokeClient(t, f, "privacy-overlap-viewer", phoneViewer)
	if err := f.store.SetUserStatus(f.ctx, owner.id, true); err != nil {
		t.Fatalf("mark owner online: %v", err)
	}
	if err := setSmokePrivacy(f.ctx, owner,
		&tg.InputPrivacyValueAllowAll{},
		&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(owner.id, viewer.id)}},
		&tg.InputPrivacyValueDisallowUsers{Users: []tg.InputUserClass{inputUser(owner.id, viewer.id)}},
	); err != nil {
		t.Fatalf("set overlapping privacy exceptions: %v", err)
	}

	privacy, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("get overlapping privacy exceptions: %v", err)
	}
	if len(privacy.Rules) != 3 {
		t.Fatalf("getPrivacy returned %d rules, want base and both exceptions", len(privacy.Rules))
	}
	if _, ok := privacy.Rules[0].(*tg.PrivacyValueAllowAll); !ok {
		t.Fatalf("base rule = %T, want *tg.PrivacyValueAllowAll", privacy.Rules[0])
	}
	disallowed, disallowedFirst := privacy.Rules[1].(*tg.PrivacyValueDisallowUsers)
	if !disallowedFirst || len(disallowed.Users) != 1 || disallowed.Users[0] != viewer.id {
		t.Errorf("first exception rule = %T, want *tg.PrivacyValueDisallowUsers", privacy.Rules[1])
	}
	allowed, allowSecond := privacy.Rules[2].(*tg.PrivacyValueAllowUsers)
	if !allowSecond || len(allowed.Users) != 1 || allowed.Users[0] != viewer.id {
		t.Errorf("second exception rule = %T, want *tg.PrivacyValueAllowUsers", privacy.Rules[2])
	}

	// Desktop keeps the first exception for a user when converting these TL rules
	// back to editable rules. Round-trip that client result through setPrivacy.
	var firstException tg.InputPrivacyRuleClass
	switch first := privacy.Rules[1].(type) {
	case *tg.PrivacyValueAllowUsers:
		if len(first.Users) != 1 {
			t.Fatalf("first allow-users exception has %d users, want one", len(first.Users))
		}
		firstException = &tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(owner.id, first.Users[0])}}
	case *tg.PrivacyValueDisallowUsers:
		if len(first.Users) != 1 {
			t.Fatalf("first disallow-users exception has %d users, want one", len(first.Users))
		}
		firstException = &tg.InputPrivacyValueDisallowUsers{Users: []tg.InputUserClass{inputUser(owner.id, first.Users[0])}}
	default:
		t.Fatalf("first exception = %T, want allow-users or disallow-users", privacy.Rules[1])
	}
	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}, firstException); err != nil {
		t.Fatalf("set privacy after client round-trip: %v", err)
	}

	roundTripped, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("get privacy after client round-trip: %v", err)
	}
	if len(roundTripped.Rules) != 2 {
		t.Fatalf("round-tripped rules = %d, want base and one exception", len(roundTripped.Rules))
	}
	disallowed, ok := roundTripped.Rules[1].(*tg.PrivacyValueDisallowUsers)
	if !ok || len(disallowed.Users) != 1 || disallowed.Users[0] != viewer.id {
		t.Fatalf("round-tripped exception = %#v, want viewer %d disallowed", roundTripped.Rules[1], viewer.id)
	}
	status, err := readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read owner status after client round-trip: %v", err)
	}
	assertSmokeStatus(t, status, "recently", false)
}

func TestStatusPrivacyFiltersPushes(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneOwner, phoneHidden, phoneAllowed = "+15551047201", "+15551047202", "+15551047203"
	seedSmokeUsers(t, f, phoneOwner, phoneHidden, phoneAllowed)
	owner := newSmokeClient(t, f, "push-privacy-owner", phoneOwner)
	hidden := newSmokeClient(t, f, "push-privacy-hidden", phoneHidden)
	allowed := newSmokeClient(t, f, "push-privacy-allowed", phoneAllowed)
	if err := f.store.SetUserStatus(f.ctx, owner.id, true); err != nil {
		t.Fatalf("mark owner online: %v", err)
	}
	for i, viewer := range []*smokeClient{hidden, allowed} {
		if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     peerUser(owner.id, viewer.id),
				Message:  "push privacy setup",
				RandomID: int64(1047201 + i),
			})
			return err
		}); err != nil {
			t.Fatalf("establish dialog with %s: %v", viewer.label, err)
		}
	}
	if err := setSmokePrivacy(f.ctx, owner,
		&tg.InputPrivacyValueDisallowAll{},
		&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{inputUser(owner.id, allowed.id)}},
	); err != nil {
		t.Fatalf("allow one status viewer: %v", err)
	}
	drainStatus(hidden.push)
	drainStatus(allowed.push)

	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, true)
		return err
	}); err != nil {
		t.Fatalf("set owner offline: %v", err)
	}
	if err := waitSmokeStatus(f.ctx, allowed.push.userStatus, owner.id, "offline"); err != nil {
		t.Fatal(err)
	}
	assertNoSmokeStatus(t, f.ctx, hidden.push.userStatus, owner.id)

	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, false)
		return err
	}); err != nil {
		t.Fatalf("set owner online: %v", err)
	}
	if err := waitSmokeStatus(f.ctx, allowed.push.userStatus, owner.id, "online"); err != nil {
		t.Fatal(err)
	}
	assertNoSmokeStatus(t, f.ctx, hidden.push.userStatus, owner.id)

	if _, err := f.store.BlockUser(f.ctx, owner.id, hidden.id); err != nil {
		t.Fatalf("block hidden viewer: %v", err)
	}
	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}); err != nil {
		t.Fatalf("remove privacy rule before block push check: %v", err)
	}
	drainStatus(hidden.push)
	drainStatus(allowed.push)
	if err := owner.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountUpdateStatus(ctx, true)
		return err
	}); err != nil {
		t.Fatalf("set blocked owner offline: %v", err)
	}
	if err := waitSmokeStatus(f.ctx, allowed.push.userStatus, owner.id, "offline"); err != nil {
		t.Fatal(err)
	}
	assertNoSmokeStatus(t, f.ctx, hidden.push.userStatus, owner.id)
}

func TestLastSeenPrivacyCoarseBuckets(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneOwner, phoneViewer = "+15551047501", "+15551047502"
	seedSmokeUsers(t, f, phoneOwner, phoneViewer)
	owner := newSmokeClient(t, f, "privacy-bucket-owner", phoneOwner)
	viewer := newSmokeClient(t, f, "privacy-bucket-viewer", phoneViewer)
	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueDisallowAll{}); err != nil {
		t.Fatalf("hide owner status: %v", err)
	}
	if err := f.store.SetUserStatus(f.ctx, owner.id, false); err != nil {
		t.Fatalf("set owner offline: %v", err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	}()

	now := time.Now().UTC()
	for _, test := range []struct {
		name string
		age  time.Duration
		want string
	}{
		{name: "recent", age: 2 * 24 * time.Hour, want: "recently"},
		{name: "week", age: 5 * 24 * time.Hour, want: "lastWeek"},
		{name: "month", age: 14 * 24 * time.Hour, want: "lastMonth"},
		{name: "long ago", age: 31 * 24 * time.Hour, want: "empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := conn.Exec(f.ctx, "UPDATE users SET last_seen_at = $1 WHERE id = $2", now.Add(-test.age), owner.id); err != nil {
				t.Fatalf("set last_seen_at: %v", err)
			}
			status, err := readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
			if err != nil {
				t.Fatalf("read coarse status: %v", err)
			}
			assertSmokeStatus(t, status, test.want, false)
		})
	}

	if err := f.store.SetUserStatus(f.ctx, owner.id, true); err != nil {
		t.Fatalf("set owner online: %v", err)
	}
	if _, err := conn.Exec(f.ctx, "UPDATE users SET last_seen_at = $1 WHERE id = $2", now.Add(-31*24*time.Hour), owner.id); err != nil {
		t.Fatalf("set old last_seen_at while online: %v", err)
	}
	status, err := readSmokeOwnerStatus(f.ctx, viewer, owner.id, phoneOwner, "getUsers")
	if err != nil {
		t.Fatalf("read online coarse status: %v", err)
	}
	assertSmokeStatus(t, status, "recently", false)
}

func TestLastSeenPrivacyRejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneOwner, phoneViewer = "+15551047301", "+15551047302"
	seedSmokeUsers(t, f, phoneOwner, phoneViewer)
	owner := newSmokeClient(t, f, "privacy-validation-owner", phoneOwner)
	viewer := newSmokeClient(t, f, "privacy-validation-viewer", phoneViewer)

	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueDisallowAll{}); err != nil {
		t.Fatalf("seed saved Nobody rule: %v", err)
	}
	if _, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyChatInvite{}); !tgerr.Is(err, "PRIVACY_KEY_INVALID") {
		t.Fatalf("getPrivacy with another key = %v, want PRIVACY_KEY_INVALID", err)
	}
	if err := setSmokePrivacyRequest(f.ctx, owner, &tg.InputPrivacyKeyChatInvite{}, &tg.InputPrivacyValueAllowAll{}); !tgerr.Is(err, "PRIVACY_KEY_INVALID") {
		t.Fatalf("setPrivacy with another key = %v, want PRIVACY_KEY_INVALID", err)
	}

	for _, value := range []struct {
		name  string
		value tg.InputPrivacyRuleClass
	}{
		{name: "allow chat participants", value: &tg.InputPrivacyValueAllowChatParticipants{}},
		{name: "disallow chat participants", value: &tg.InputPrivacyValueDisallowChatParticipants{}},
		{name: "allow close friends", value: &tg.InputPrivacyValueAllowCloseFriends{}},
		{name: "allow premium", value: &tg.InputPrivacyValueAllowPremium{}},
		{name: "allow bots", value: &tg.InputPrivacyValueAllowBots{}},
		{name: "disallow bots", value: &tg.InputPrivacyValueDisallowBots{}},
	} {
		t.Run(value.name, func(t *testing.T) {
			if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}, value.value); !tgerr.Is(err, "PRIVACY_VALUE_INVALID") {
				t.Fatalf("setPrivacy with unsupported value = %v, want PRIVACY_VALUE_INVALID", err)
			}
		})
	}

	tooMany := make([]tg.InputUserClass, 1001)
	for i := range tooMany {
		tooMany[i] = &tg.InputUserSelf{}
	}
	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}, &tg.InputPrivacyValueAllowUsers{Users: tooMany}); !tgerr.Is(err, "PRIVACY_TOO_LONG") {
		t.Fatalf("setPrivacy with 1001 entries = %v, want PRIVACY_TOO_LONG", err)
	}

	unknownID := int64(900000000)
	deleted, err := f.store.CreateUsernameUser(f.ctx, "privacy-deleted", "Deleted", "")
	if err != nil {
		t.Fatalf("create user to delete: %v", err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	defer func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	}()
	if _, err := conn.Exec(f.ctx, "DELETE FROM update_state WHERE user_id = $1", deleted.ID); err != nil {
		t.Fatalf("delete user's update state: %v", err)
	}
	if _, err := conn.Exec(f.ctx, "DELETE FROM users WHERE id = $1", deleted.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}

	invalidInputs := []tg.InputUserClass{
		&tg.InputUser{UserID: viewer.id, AccessHash: inputUser(owner.id, viewer.id).AccessHash + 1},
		inputUser(owner.id, unknownID),
		inputUser(owner.id, deleted.ID),
	}
	var rejection string
	for i, input := range invalidInputs {
		err := setSmokePrivacy(f.ctx, owner,
			&tg.InputPrivacyValueAllowAll{},
			&tg.InputPrivacyValueAllowUsers{Users: []tg.InputUserClass{input}},
		)
		if !tgerr.Is(err, "PEER_ID_INVALID") {
			t.Fatalf("entry %d error = %v, want PEER_ID_INVALID", i, err)
		}
		var rpcErr *tgerr.Error
		if !errors.As(err, &rpcErr) {
			t.Fatalf("entry %d error type = %T, want RPC error", i, err)
		}
		if i == 0 {
			rejection = rpcErr.Message
		} else if rpcErr.Message != rejection {
			t.Fatalf("entry %d error = %q, want identical error %q", i, rpcErr.Message, rejection)
		}
	}

	rules, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("read rule after rejected writes: %v", err)
	}
	if len(rules.Rules) != 1 {
		t.Fatalf("saved rules after rejected writes = %d, want one Nobody rule", len(rules.Rules))
	}
	if _, ok := rules.Rules[0].(*tg.PrivacyValueDisallowAll); !ok {
		t.Fatalf("saved rule after rejected writes = %T, want *tg.PrivacyValueDisallowAll", rules.Rules[0])
	}

	viewerRules, err := getSmokePrivacy(f.ctx, viewer, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("viewer reads own default rule: %v", err)
	}
	if len(viewerRules.Rules) != 1 {
		t.Fatalf("viewer saved rules = %d, want one default rule", len(viewerRules.Rules))
	}
	if _, ok := viewerRules.Rules[0].(*tg.PrivacyValueAllowAll); !ok {
		t.Fatalf("viewer saved rule = %T, want *tg.PrivacyValueAllowAll", viewerRules.Rules[0])
	}
}

func TestLastSeenPrivacyRateLimit(t *testing.T) {
	t.Parallel()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.rateLimits = config.DefaultRateLimits()
		f.rateLimits.UpdateProfile = store.RateLimitConfig{Limit: 1, Window: time.Hour}
	})
	const phoneOwner = "+15551047401"
	seedSmokeUsers(t, f, phoneOwner)
	owner := newSmokeClient(t, f, "privacy-rate-limit-owner", phoneOwner)

	if err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueAllowAll{}); err != nil {
		t.Fatalf("first setPrivacy: %v", err)
	}
	err := setSmokePrivacy(f.ctx, owner, &tg.InputPrivacyValueDisallowAll{})
	var rpcErr *tgerr.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != 420 {
		t.Fatalf("second setPrivacy error = %v, want a 420 FLOOD_WAIT", err)
	}
	rules, err := getSmokePrivacy(f.ctx, owner, &tg.InputPrivacyKeyStatusTimestamp{})
	if err != nil {
		t.Fatalf("get privacy after rate limit: %v", err)
	}
	if len(rules.Rules) != 1 {
		t.Fatalf("saved rules = %d, want one", len(rules.Rules))
	}
	if _, ok := rules.Rules[0].(*tg.PrivacyValueAllowAll); !ok {
		t.Fatalf("saved rule after rate limit = %T, want *tg.PrivacyValueAllowAll", rules.Rules[0])
	}
}

func setSmokePrivacy(ctx context.Context, client *smokeClient, rules ...tg.InputPrivacyRuleClass) error {
	return setSmokePrivacyRequest(ctx, client, &tg.InputPrivacyKeyStatusTimestamp{}, rules...)
}

func setSmokePrivacyRequest(ctx context.Context, client *smokeClient, key tg.InputPrivacyKeyClass, rules ...tg.InputPrivacyRuleClass) error {
	return client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.AccountSetPrivacy(ctx, &tg.AccountSetPrivacyRequest{
			Key:   key,
			Rules: rules,
		})
		return err
	})
}

func getSmokePrivacy(ctx context.Context, client *smokeClient, key tg.InputPrivacyKeyClass) (*tg.AccountPrivacyRules, error) {
	var rules *tg.AccountPrivacyRules
	err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		rules, err = api.AccountGetPrivacy(ctx, key)
		return err
	})
	return rules, err
}

func readSmokeOwnerStatus(ctx context.Context, viewer *smokeClient, ownerID int64, ownerPhone, method string) (tg.UserStatusClass, error) {
	var status tg.UserStatusClass
	err := viewer.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var user *tg.User
		var findErr error
		switch method {
		case "getUsers":
			users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{inputUser(viewer.id, ownerID)})
			if err != nil {
				return err
			}
			user, findErr = requireSmokeFullUser(users, ownerID, method)
		case "getDialogs":
			result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
			if err != nil {
				return err
			}
			var users []tg.UserClass
			switch dialogs := result.(type) {
			case *tg.MessagesDialogs:
				users = dialogs.Users
			case *tg.MessagesDialogsSlice:
				users = dialogs.Users
			default:
				return fmt.Errorf("getDialogs response = %T", result)
			}
			user, findErr = requireSmokeFullUser(users, ownerID, method)
		case "getDifference":
			result, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 0, Date: 0, Qts: 0})
			if err != nil {
				return err
			}
			var users []tg.UserClass
			switch difference := result.(type) {
			case *tg.UpdatesDifference:
				users = difference.Users
			case *tg.UpdatesDifferenceSlice:
				users = difference.Users
			default:
				return fmt.Errorf("getDifference response = %T", result)
			}
			user, findErr = requireSmokeFullUser(users, ownerID, method)
		case "resolveUsername":
			result, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: smokeUsernameForPhone(ownerPhone)})
			if err != nil {
				return err
			}
			user, findErr = requireSmokeFullUser(result.Users, ownerID, method)
		default:
			return fmt.Errorf("unknown status read method %q", method)
		}
		if findErr != nil {
			return findErr
		}
		status = user.Status
		return nil
	})
	return status, err
}

func assertSmokeStatus(t *testing.T, status tg.UserStatusClass, kind string, byMe bool) {
	t.Helper()
	switch got := status.(type) {
	case *tg.UserStatusOnline:
		if kind != "online" {
			t.Fatalf("status = %T, want %s", status, kind)
		}
	case *tg.UserStatusRecently:
		if kind != "recently" || got.ByMe != byMe {
			t.Fatalf("status = Recently{ByMe:%v}, want %s with ByMe=%v", got.ByMe, kind, byMe)
		}
	case *tg.UserStatusLastWeek:
		if kind != "lastWeek" || got.ByMe != byMe {
			t.Fatalf("status = LastWeek{ByMe:%v}, want %s with ByMe=%v", got.ByMe, kind, byMe)
		}
	case *tg.UserStatusLastMonth:
		if kind != "lastMonth" || got.ByMe != byMe {
			t.Fatalf("status = LastMonth{ByMe:%v}, want %s with ByMe=%v", got.ByMe, kind, byMe)
		}
	case *tg.UserStatusEmpty:
		if kind != "empty" {
			t.Fatalf("status = %T, want %s", status, kind)
		}
	case *tg.UserStatusOffline:
		if kind != "offline" {
			t.Fatalf("status = %T, want %s", status, kind)
		}
	default:
		t.Fatalf("status = %T, want %s", status, kind)
	}
}

func waitSmokeStatus(ctx context.Context, updates <-chan *tg.UpdateUserStatus, ownerID int64, kind string) error {
	for {
		select {
		case update := <-updates:
			if update.UserID == ownerID {
				switch kind {
				case "online":
					if _, ok := update.Status.(*tg.UserStatusOnline); ok {
						return nil
					}
				case "offline":
					if _, ok := update.Status.(*tg.UserStatusOffline); ok {
						return nil
					}
				default:
					return fmt.Errorf("unknown status kind %q", kind)
				}
			}
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for owner %d %s status: %w", ownerID, kind, ctx.Err())
		}
	}
}

func assertNoSmokeStatus(t *testing.T, ctx context.Context, updates <-chan *tg.UpdateUserStatus, ownerID int64) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case update := <-updates:
			if update.UserID == ownerID {
				t.Fatalf("hidden viewer received status update for owner %d: %T", ownerID, update.Status)
			}
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting for hidden status check: %v", ctx.Err())
		}
	}
}
