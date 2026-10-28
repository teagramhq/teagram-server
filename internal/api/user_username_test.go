package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

// divergeUserUsername claims handle for userID through the RPC that writes both
// the usernames row and the denormalized users.username copy, then overwrites
// only the copy. The two are written in one transaction today, so a divergence
// has to be forced: what is under test is that a reader takes the handle from
// the usernames row, which stays authoritative whatever the copy says.
func divergeUserUsername(t *testing.T, s *store.Store, dsn string, userID int64, handle, stale string) {
	t.Helper()
	if _, err := api.UpdateUsernameForTest(s, userID, handle); err != nil {
		t.Fatalf("claim username: %v", err)
	}
	execDB(t, dsn, `UPDATE users SET username = $2 WHERE id = $1`, userID, stale)
}

// TestGetUsersReportsAuthoritativeUsername covers users.getUsers, which loads
// the caller by id.
func TestGetUsersReportsAuthoritativeUsername(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	user, err := s.CreateUser(ctx, "15550014001")
	if err != nil {
		t.Fatal(err)
	}
	divergeUserUsername(t, s, dsn, user.ID, "realhandle", "stalehandle")

	res, err := api.GetUsersForTest(s, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertEncodes(t, res)
	vec, ok := res.(*tg.UserClassVector)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := vec.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("elems[0] is not *tg.User: %T", vec.Elems[0])
	}
	if got.Username != "realhandle" {
		t.Errorf("users.getUsers username = %q, want %q (the usernames row, not the copy)",
			got.Username, "realhandle")
	}
}

// TestResolvePhoneReportsAuthoritativeUsername covers contacts.resolvePhone,
// which loads the target by phone.
func TestResolvePhoneReportsAuthoritativeUsername(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	caller, err := s.CreateUser(ctx, "15550014011")
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateUser(ctx, "15550014012")
	if err != nil {
		t.Fatal(err)
	}
	divergeUserUsername(t, s, dsn, target.ID, "realhandle", "stalehandle")

	res, err := api.ResolvePhoneForTest(s, caller.ID, &tg.ContactsResolvePhoneRequest{Phone: "15550014012"})
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := res.(*tg.ContactsResolvedPeer)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := peer.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("users[0] is not *tg.User: %T", peer.Users[0])
	}
	if got.Username != "realhandle" {
		t.Errorf("contacts.resolvePhone username = %q, want %q (the usernames row, not the copy)",
			got.Username, "realhandle")
	}
}

// TestResolveUsernameReportsAuthoritativeUserHandle covers
// contacts.resolveUsername for a user peer: the handle that admitted the user
// to the result is the one reported back, never the copy.
func TestResolveUsernameReportsAuthoritativeUserHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	caller, err := s.CreateUser(ctx, "15550014021")
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateUser(ctx, "15550014022")
	if err != nil {
		t.Fatal(err)
	}
	divergeUserUsername(t, s, dsn, target.ID, "realhandle", "stalehandle")

	res, err := api.ResolveUsernameForTest(s, caller.ID, &tg.ContactsResolveUsernameRequest{Username: "realhandle"})
	if err != nil {
		t.Fatal(err)
	}
	peer, ok := res.(*tg.ContactsResolvedPeer)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := peer.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("users[0] is not *tg.User: %T", peer.Users[0])
	}
	if got.Username != "realhandle" {
		t.Errorf("contacts.resolveUsername username = %q, want %q (the usernames row, not the copy)",
			got.Username, "realhandle")
	}
}

func TestExistingBotSuffixUsernameStillResolvesAndReceivesMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	caller, err := s.CreateUser(ctx, "15550014023")
	if err != nil {
		t.Fatalf("create caller: %v", err)
	}
	target, err := s.CreateUser(ctx, "15550014024")
	if err != nil {
		t.Fatalf("create existing holder: %v", err)
	}
	if err := s.UpdateUsername(ctx, target.ID, "weatherbot"); err != nil {
		t.Fatalf("seed existing username: %v", err)
	}

	if _, err := api.UpdateUsernameForTest(s, target.ID, "WeatherBOT"); rpcMessage(t, err) != "USERNAME_INVALID" {
		t.Fatalf("updateUsername for existing suffix username = %v, want USERNAME_INVALID", err)
	}
	if held, found, err := s.UsernameByHandle(ctx, "weatherbot"); err != nil || !found || held.User.ID != target.ID {
		t.Fatalf("existing username = %#v found=%v err=%v, want it to remain held by user %d", held, found, err, target.ID)
	}

	resolved, err := api.ResolveUsernameForTest(s, caller.ID, &tg.ContactsResolveUsernameRequest{Username: "WeatherBOT"})
	if err != nil {
		t.Fatalf("resolve existing username: %v", err)
	}
	peer, ok := resolved.(*tg.ContactsResolvedPeer)
	if !ok || len(peer.Users) != 1 {
		t.Fatalf("resolve result = %#v, want one resolved user", resolved)
	}
	resolvedUser, ok := peer.Users[0].(*tg.User)
	if !ok || resolvedUser.ID != target.ID {
		t.Fatalf("resolved user = %#v, want user %d", peer.Users[0], target.ID)
	}

	if _, err := api.SendMessageForTest(s, caller.ID, &tg.MessagesSendMessageRequest{
		Peer:     &tg.InputPeerUser{UserID: resolvedUser.ID, AccessHash: resolvedUser.AccessHash},
		Message:  "hello existing holder",
		RandomID: 14023,
	}); err != nil {
		t.Fatalf("send to resolved existing holder: %v", err)
	}
}

// TestContactsSearchReportsAuthoritativeUsername covers contacts.search, whose
// contact arm loads dialog partners by name.
func TestContactsSearchReportsAuthoritativeUsername(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	caller, err := s.CreateUser(ctx, "15550014031")
	if err != nil {
		t.Fatal(err)
	}
	partner, err := s.CreateUser(ctx, "15550014032")
	if err != nil {
		t.Fatal(err)
	}
	if err := api.SetUserFirstNameForTest(dsn, partner.ID, "Authoritative"); err != nil {
		t.Fatalf("set name: %v", err)
	}
	if _, err := api.SendMessageForTest(s, caller.ID, &tg.MessagesSendMessageRequest{
		Peer:     api.InputPeerUser(caller.ID, partner.ID),
		Message:  "hello",
		RandomID: 1,
	}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	divergeUserUsername(t, s, dsn, partner.ID, "realhandle", "stalehandle")

	res, err := api.ContactsSearchForTest(s, caller.ID, &tg.ContactsSearchRequest{Q: "Authoritative", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	found, ok := res.(*tg.ContactsFound)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := found.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("users[0] is not *tg.User: %T", found.Users[0])
	}
	if got.Username != "realhandle" {
		t.Errorf("contacts.search username = %q, want %q (the usernames row, not the copy)",
			got.Username, "realhandle")
	}
}

// TestGetUsersReportsNoUsernameAfterRelease is the case the divergence exists
// to guard: the usernames row is gone but a writer left the copy behind. The
// account no longer holds the handle, so no RPC may keep reporting it.
func TestGetUsersReportsNoUsernameAfterRelease(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	user, err := s.CreateUser(ctx, "15550014041")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.UpdateUsernameForTest(s, user.ID, "releasedhandle"); err != nil {
		t.Fatalf("claim username: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, user.ID, ""); err != nil {
		t.Fatalf("release username: %v", err)
	}
	execDB(t, dsn, `UPDATE users SET username = $2 WHERE id = $1`, user.ID, "releasedhandle")

	res, err := api.GetUsersForTest(s, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	vec, ok := res.(*tg.UserClassVector)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := vec.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("elems[0] is not *tg.User: %T", vec.Elems[0])
	}
	if got.Username != "" {
		t.Errorf("users.getUsers username = %q after release, want empty", got.Username)
	}
}

// TestGetUsersReportsUsernameWhenRowAndCopyAgree pins the unchanged case: a
// handle claimed normally, with no divergence forced, still reaches the wire.
func TestGetUsersReportsUsernameWhenRowAndCopyAgree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	user, err := s.CreateUser(ctx, "15550014051")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.UpdateUsernameForTest(s, user.ID, "agreedhandle"); err != nil {
		t.Fatalf("claim username: %v", err)
	}

	res, err := api.GetUsersForTest(s, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	vec, ok := res.(*tg.UserClassVector)
	if !ok {
		t.Fatalf("unexpected response type: %T", res)
	}
	got, ok := vec.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("elems[0] is not *tg.User: %T", vec.Elems[0])
	}
	if got.Username != "agreedhandle" {
		t.Errorf("users.getUsers username = %q, want %q", got.Username, "agreedhandle")
	}
}
