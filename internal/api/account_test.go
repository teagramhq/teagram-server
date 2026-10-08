package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

// TestUpdateStatusRefusesUnauthenticated proves an unauthenticated caller
// (UserID 0) is rejected with AUTH_KEY_UNREGISTERED.
func TestUpdateStatusRefusesUnauthenticated(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	_, err := api.UpdateStatusForTest(s, 0, true)
	if err == nil {
		t.Fatal("expected error for unauthenticated caller")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
}

// TestUpdateStatusOffline proves Offline=true calls SetUserStatus(false) and
// leaves the user offline.
func TestUpdateStatusOffline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550000301")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.SetUserStatus(ctx, user.ID, true); err != nil {
		t.Fatalf("set online: %v", err)
	}

	res, err := api.UpdateStatusForTest(s, user.ID, true)
	if err != nil {
		t.Fatalf("update status offline: %v", err)
	}
	if _, ok := res.(*tg.BoolTrue); !ok {
		t.Fatalf("result = %T, want *tg.BoolTrue", res)
	}

	u, ok, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("user by id: %v", err)
	}
	if !ok {
		t.Fatal("user not found")
	}
	if u.IsOnline {
		t.Fatal("user still online after Offline=true")
	}
}

// TestUpdateStatusOnline proves Offline=false calls SetUserStatus(true) and
// leaves the user online.
func TestUpdateStatusOnline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550000302")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.SetUserStatus(ctx, user.ID, false); err != nil {
		t.Fatalf("set offline: %v", err)
	}

	res, err := api.UpdateStatusForTest(s, user.ID, false)
	if err != nil {
		t.Fatalf("update status online: %v", err)
	}
	if _, ok := res.(*tg.BoolTrue); !ok {
		t.Fatalf("result = %T, want *tg.BoolTrue", res)
	}

	u, ok, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("user by id: %v", err)
	}
	if !ok {
		t.Fatal("user not found")
	}
	if !u.IsOnline {
		t.Fatal("user not online after Offline=false")
	}
}

// TestUpdateStatusNonExistentUser proves SetUserStatus failure for a missing
// user returns errInternal, not BoolTrue.
func TestUpdateStatusNonExistentUser(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	_, err := api.UpdateStatusForTest(s, 99999, false)
	if err == nil {
		t.Fatal("expected error for non-existent user")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "INTERNAL" {
		t.Fatalf("error = %v, want INTERNAL", err)
	}
}

// --- account.updateUsername tests ---

// TestUpdateUsernameUnauthenticated proves an unauthenticated caller (UserID 0)
// is rejected with AUTH_KEY_UNREGISTERED.
func TestUpdateUsernameUnauthenticated(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	_, err := api.UpdateUsernameForTest(s, 0, "alice")
	if err == nil {
		t.Fatal("expected error for unauthenticated caller")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
}

// TestUpdateUsernameSuccess proves a valid, available username is claimed and
// stored on both the usernames table and users.username column.
func TestUpdateUsernameSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001001")
	if err != nil {
		t.Fatal(err)
	}

	res, err := api.UpdateUsernameForTest(s, user.ID, "alice")
	if err != nil {
		t.Fatalf("update username: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.Username != "alice" {
		t.Fatalf("returned user username = %q, want alice", uRes.Username)
	}

	u, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("user lookup: ok=%v err=%v", ok, err)
	}
	if u.Username == nil || *u.Username != "alice" {
		t.Fatalf("username = %v, want alice", u.Username)
	}
}

// TestUpdateUsernameOccupied proves a second account cannot claim an already
// taken username.
func TestUpdateUsernameOccupied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user1, err := s.CreateUser(ctx, "+15550001002")
	if err != nil {
		t.Fatal(err)
	}
	user2, err := s.CreateUser(ctx, "+15550001003")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user1.ID, "alice")
	if err != nil {
		t.Fatalf("user1 claim: %v", err)
	}

	_, err = api.UpdateUsernameForTest(s, user2.ID, "alice")
	if err == nil {
		t.Fatal("expected error for occupied username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_OCCUPIED" {
		t.Fatalf("error = %v, want USERNAME_OCCUPIED", err)
	}
}

// TestUpdateUsernameCaseInsensitive proves that "ALICE" is rejected when
// "alice" is already taken (case-insensitive via normalization).
func TestUpdateUsernameCaseInsensitive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user1, err := s.CreateUser(ctx, "+15550001004")
	if err != nil {
		t.Fatal(err)
	}
	user2, err := s.CreateUser(ctx, "+15550001005")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user1.ID, "alice")
	if err != nil {
		t.Fatalf("user1 claim: %v", err)
	}

	_, err = api.UpdateUsernameForTest(s, user2.ID, "ALICE")
	if err == nil {
		t.Fatal("expected error for case-insensitive occupied username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_OCCUPIED" {
		t.Fatalf("error = %v, want USERNAME_OCCUPIED", err)
	}
}

// TestUpdateUsernameClear proves calling with an empty string clears the
// username and releases the handle.
func TestUpdateUsernameClear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001006")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "alice")
	if err != nil {
		t.Fatalf("set username: %v", err)
	}

	res, err := api.UpdateUsernameForTest(s, user.ID, "")
	if err != nil {
		t.Fatalf("clear username: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.Username != "" {
		t.Fatalf("returned user username = %q, want empty", uRes.Username)
	}

	u, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("user lookup: ok=%v err=%v", ok, err)
	}
	if u.Username != nil {
		t.Fatalf("username = %v, want nil", u.Username)
	}

	// Handle should be available for another user.
	user2, err := s.CreateUser(ctx, "+15550001007")
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.UpdateUsernameForTest(s, user2.ID, "alice")
	if err != nil {
		t.Fatalf("reclaim username: %v", err)
	}
}

// TestUpdateUsernameClearIdempotent proves clearing a username when none is set
// returns True (idempotent).
func TestUpdateUsernameClearIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001008")
	if err != nil {
		t.Fatal(err)
	}

	res, err := api.UpdateUsernameForTest(s, user.ID, "")
	if err != nil {
		t.Fatalf("clear empty username: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.Username != "" {
		t.Fatalf("returned user username = %q, want empty", uRes.Username)
	}
}

// TestUpdateUsernameTooShort proves a username shorter than 2 characters is
// rejected with USERNAME_INVALID.
func TestUpdateUsernameTooShort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001009")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "A")
	if err == nil {
		t.Fatal("expected error for too-short username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameDigitLeading proves a username starting with a digit is
// rejected with USERNAME_INVALID.
func TestUpdateUsernameDigitLeading(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001010")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "1alice")
	if err == nil {
		t.Fatal("expected error for digit-leading username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameUnderscoreLeading proves a username starting with an
// underscore is rejected with USERNAME_INVALID.
func TestUpdateUsernameUnderscoreLeading(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001011")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "_alice")
	if err == nil {
		t.Fatal("expected error for underscore-leading username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameInvalidChar proves a username with invalid characters is
// rejected with USERNAME_INVALID.
func TestUpdateUsernameInvalidChar(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001012")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "alice!")
	if err == nil {
		t.Fatal("expected error for invalid character username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameReserved proves a reserved handle like "admin" is rejected
// with USERNAME_INVALID.
func TestUpdateUsernameReserved(t *testing.T) {
	t.Parallel()
	if !api.ValidateUsername("me") {
		t.Fatal("me should pass username format validation")
	}
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001013")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "me")
	if err == nil {
		t.Fatal("expected error for reserved username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameReservedCaseInsensitive proves reserved handles are checked
// case-insensitively.
func TestUpdateUsernameReservedCaseInsensitive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001014")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "ME")
	if err == nil {
		t.Fatal("expected error for reserved username (uppercase)")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameFloodWait proves a third change within 24 hours returns
// FLOOD_WAIT (limit is 2).
func TestUpdateUsernameFloodWait(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001015")
	if err != nil {
		t.Fatal(err)
	}

	// First change: set username.
	_, err = api.UpdateUsernameForTest(s, user.ID, "alice1")
	if err != nil {
		t.Fatalf("first change: %v", err)
	}
	// Second change: clear username.
	_, err = api.UpdateUsernameForTest(s, user.ID, "")
	if err != nil {
		t.Fatalf("second change: %v", err)
	}
	// Third change: should hit flood wait (limit is 2).
	_, err = api.UpdateUsernameForTest(s, user.ID, "alice2")
	if err == nil {
		t.Fatal("expected error for flood wait")
	}
	if !tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("error = %v, want FLOOD_WAIT", err)
	}
}

// TestUpdateUsernameConcurrentBoundary proves that two concurrent requests for
// the same username from different accounts result in exactly one success and
// one USERNAME_OCCUPIED, enforced by the usernames PRIMARY KEY.
func TestUpdateUsernameConcurrentBoundary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user1, err := s.CreateUser(ctx, "+15550001016")
	if err != nil {
		t.Fatal(err)
	}
	user2, err := s.CreateUser(ctx, "+15550001017")
	if err != nil {
		t.Fatal(err)
	}

	type result struct{ err error }
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	ready := make(chan struct{})

	go func() {
		defer wg.Done()
		<-ready
		_, results[0].err = api.UpdateUsernameForTest(s, user1.ID, "racing")
	}()
	go func() {
		defer wg.Done()
		<-ready
		_, results[1].err = api.UpdateUsernameForTest(s, user2.ID, "racing")
	}()

	close(ready)
	wg.Wait()

	var success, occupied int
	for _, r := range results {
		switch {
		case r.err == nil:
			success++
		case tgerr.Is(r.err, "USERNAME_OCCUPIED"):
			occupied++
		default:
			t.Errorf("unexpected error: %v", r.err)
		}
	}

	if success != 1 {
		t.Errorf("successes = %d, want 1", success)
	}
	if occupied != 1 {
		t.Errorf("occupied = %d, want 1", occupied)
	}
}

// TestUpdateUsernameClearCountsAsChange proves that clearing a username uses
// one change token, contributing to the rate limit.
func TestUpdateUsernameClearCountsAsChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001018")
	if err != nil {
		t.Fatal(err)
	}

	// Change 1: set username.
	_, err = api.UpdateUsernameForTest(s, user.ID, "alice")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	// Change 2: clear username (uses one token).
	_, err = api.UpdateUsernameForTest(s, user.ID, "")
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	// Third change should hit flood wait (limit is 2).
	_, err = api.UpdateUsernameForTest(s, user.ID, "bob1234")
	if err == nil {
		t.Fatal("expected flood wait")
	}
	if !tgerr.Is(err, "FLOOD_WAIT") {
		t.Fatalf("error = %v, want FLOOD_WAIT", err)
	}
}

// TestUpdateUsernameConcreteExample exercises the concrete example from the
// ticket: Account 1 sets "myname", Account 2 tries "Myname" (occupied),
// Account 1 clears, Account 2 now claims "myname".
func TestUpdateUsernameConcreteExample(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	acc1, err := s.CreateUser(ctx, "+15550001019")
	if err != nil {
		t.Fatal(err)
	}
	acc2, err := s.CreateUser(ctx, "+15550001020")
	if err != nil {
		t.Fatal(err)
	}

	// Account 1 calls account.updateUsername("myname") → success.
	_, err = api.UpdateUsernameForTest(s, acc1.ID, "myname")
	if err != nil {
		t.Fatalf("acc1 set myname: %v", err)
	}

	// Account 2 calls account.updateUsername("Myname") → USERNAME_OCCUPIED.
	_, err = api.UpdateUsernameForTest(s, acc2.ID, "Myname")
	if err == nil {
		t.Fatal("expected USERNAME_OCCUPIED")
	}
	if !tgerr.Is(err, "USERNAME_OCCUPIED") {
		t.Fatalf("error = %v, want USERNAME_OCCUPIED", err)
	}

	// Account 1 calls account.updateUsername("") → success (uses one change token).
	_, err = api.UpdateUsernameForTest(s, acc1.ID, "")
	if err != nil {
		t.Fatalf("acc1 clear: %v", err)
	}

	// Account 2 now calls account.updateUsername("myname") → success.
	_, err = api.UpdateUsernameForTest(s, acc2.ID, "myname")
	if err != nil {
		t.Fatalf("acc2 claim myname: %v", err)
	}
}

// TestUpdateUsernameTooLong proves a username over 32 characters is rejected.
func TestUpdateUsernameTooLong(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001021")
	if err != nil {
		t.Fatal(err)
	}

	_, err = api.UpdateUsernameForTest(s, user.ID, "abcdefghijklmnopqrstuvwxyz1234567")
	if err == nil {
		t.Fatal("expected error for too-long username")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != "USERNAME_INVALID" {
		t.Fatalf("error = %v, want USERNAME_INVALID", err)
	}
}

// TestUpdateUsernameExactLength proves boundary lengths (2 and 32) are accepted.
func TestUpdateUsernameExactLength(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user1, err := s.CreateUser(ctx, "+15550001022")
	if err != nil {
		t.Fatal(err)
	}
	user2, err := s.CreateUser(ctx, "+15550001023")
	if err != nil {
		t.Fatal(err)
	}

	// 2 chars (minimum)
	_, err = api.UpdateUsernameForTest(s, user1.ID, "Ab")
	if err != nil {
		t.Fatalf("2-char username: %v", err)
	}

	// 32 chars (maximum) — use a second user to avoid rate limit
	_, err = api.UpdateUsernameForTest(s, user2.ID, "abcdefghijklmnopqrstuvwxyz123456")
	if err != nil {
		t.Fatalf("32-char username: %v", err)
	}
}

// TestUpdateUsernameReservedAll proves all reserved handles are rejected.
func TestUpdateUsernameReservedAll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550001023")
	if err != nil {
		t.Fatal(err)
	}

	routeReserved := routeConflictingUsernameVariants()
	reserved := make([]string, 0, 12+len(routeReserved))
	reserved = append(reserved,
		"admin", "support", "help", "me", "settings",
		"telegram", "channel", "channels", "bot", "bots",
		"login", "signup",
	)
	reserved = append(reserved, routeReserved...)
	for _, r := range reserved {
		_, err := api.UpdateUsernameForTest(s, user.ID, r)
		if err == nil {
			t.Errorf("reserved handle %q was accepted", r)
		} else if !tgerr.Is(err, "USERNAME_INVALID") {
			t.Errorf("reserved handle %q: got %v, want USERNAME_INVALID", r, err)
		}
		if _, found, err := s.UsernameByHandle(ctx, r); err != nil {
			t.Errorf("lookup reserved handle %q: %v", r, err)
		} else if found {
			t.Errorf("reserved handle %q was stored", r)
		}
	}
}

// mustUsernameAccount seeds a login_mode='username' account with its credential
// handle already claimed and its SRP row written, which is the shape a real
// username-login account has. The handle is stored lowercased, as every write
// path stores it.
func mustUsernameAccount(t *testing.T, s *store.Store, handle string) store.User {
	t.Helper()
	salt1 := make([]byte, 32)
	salt2 := make([]byte, 32)
	verifier := make([]byte, 256)
	for i := range salt1 {
		salt1[i] = byte(i + 1)
		salt2[i] = byte(200 - i)
	}
	for i := range verifier {
		verifier[i] = byte(i * 7)
	}
	u, err := s.CreateUsernameAccountWithPassword(context.Background(), handle, salt1, salt2, verifier)
	if err != nil {
		t.Fatalf("create username account %q: %v", handle, err)
	}
	return u
}

// TestUpdateUsernameLoginCredentialReject proves a login_mode='username' account
// cannot change or clear its handle: a changed, cleared or case-only input is
// USERNAME_IMMUTABLE, and only the exact stored handle is USERNAME_NOT_MODIFIED.
func TestUpdateUsernameLoginCredentialReject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)

	u := mustUsernameAccount(t, s, "operator")

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"newhandle", "USERNAME_IMMUTABLE"},
		{"", "USERNAME_IMMUTABLE"},
		{"Operator", "USERNAME_IMMUTABLE"},
		{"OPERATOR", "USERNAME_IMMUTABLE"},
		{"operator", "USERNAME_NOT_MODIFIED"},
	} {
		_, err := api.UpdateUsernameForTest(s, u.ID, tc.in)
		if got := rpcMessage(t, err); got != tc.want {
			t.Errorf("updateUsername(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}

	got, ok, err := s.UserByID(ctx, u.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.Username == nil || *got.Username != "operator" {
		t.Fatalf("stored handle = %v, want operator", got.Username)
	}
}

// TestUpdateUsernameImmutableOccupiedTarget proves the immutable refusal is the
// same answer whether the requested handle is free, held by another account, or
// held by a channel. It is decided against the caller's own handle, so it is not
// an occupancy oracle and it charges no lookup.
func TestUpdateUsernameImmutableOccupiedTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	caller := mustUsernameAccount(t, s, "operator")

	other, err := s.CreateUser(ctx, "+15550001201")
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, other.ID, "heldbyuser"); err != nil {
		t.Fatalf("other user claim: %v", err)
	}

	creator, err := s.CreateUser(ctx, "+15550001202")
	if err != nil {
		t.Fatalf("creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Held", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, err := api.EditChannelUsernameForTest(s, creator.ID, &tg.ChannelsUpdateUsernameRequest{
		Channel:  api.InputChannel(creator.ID, channel.ID),
		Username: "heldbychannel",
	}); err != nil {
		t.Fatalf("channel claim: %v", err)
	}

	for _, target := range []string{"freehandle", "heldbyuser", "heldbychannel"} {
		_, err := api.UpdateUsernameForTest(s, caller.ID, target)
		if got := rpcMessage(t, err); got != "USERNAME_IMMUTABLE" {
			t.Errorf("updateUsername(%q) = %s, want USERNAME_IMMUTABLE", target, got)
		}
	}

	// No occupancy probe happened, so no lookup was charged and no handle moved.
	var lookups int
	if err := lookupPool(t, dsn).QueryRow(ctx,
		"SELECT count(*) FROM username_lookups WHERE caller_id = $1", caller.ID).Scan(&lookups); err != nil {
		t.Fatalf("count lookups: %v", err)
	}
	if lookups != 0 {
		t.Errorf("lookup rows charged = %d, want 0", lookups)
	}
	for _, handle := range []string{"heldbyuser", "heldbychannel"} {
		if _, found, err := s.UsernameByHandle(ctx, handle); err != nil {
			t.Fatalf("lookup %q: %v", handle, err)
		} else if !found {
			t.Errorf("handle %q was released by a refused request", handle)
		}
	}
}

// TestUpdateUsernameImmutablePreservesState proves a refusal is a pure answer:
// the stored handle, the usernames row, the SRP salts and verifier, an
// outstanding registration invite for the handle, and both the lookup and the
// change budget are exactly what they were.
func TestUpdateUsernameImmutablePreservesState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	pool := lookupPool(t, dsn)

	u := mustUsernameAccount(t, s, "operator")

	// An invite is keyed by handle: if the handle row were released, an
	// outstanding invite could hand the operator's login name to someone else.
	digest := make([]byte, 32)
	for i := range digest {
		digest[i] = byte(i + 40)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO registration_invites (handle, secret_digest, expires_at)
		 VALUES ($1, $2, clock_timestamp() + interval '1 day')`, "operator", digest); err != nil {
		t.Fatalf("seed registration invite: %v", err)
	}

	type state struct {
		usersHandle *string
		ownerType   string
		ownerID     int64
		salt1       []byte
		salt2       []byte
		verifier    []byte
		invites     int
		lookups     int
		changes     int
	}
	read := func() state {
		t.Helper()
		var st state
		if err := pool.QueryRow(ctx, "SELECT username FROM users WHERE id = $1", u.ID).Scan(&st.usersHandle); err != nil {
			t.Fatalf("read users.username: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT owner_type, owner_id FROM usernames WHERE handle = $1", "operator").
			Scan(&st.ownerType, &st.ownerID); err != nil {
			t.Fatalf("read usernames row: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT salt1, salt2, verifier FROM user_passwords WHERE user_id = $1", u.ID).
			Scan(&st.salt1, &st.salt2, &st.verifier); err != nil {
			t.Fatalf("read user_passwords: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM registration_invites WHERE handle = $1", "operator").
			Scan(&st.invites); err != nil {
			t.Fatalf("count invites: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM username_lookups WHERE caller_id = $1", u.ID).
			Scan(&st.lookups); err != nil {
			t.Fatalf("count lookups: %v", err)
		}
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM username_changes WHERE user_id = $1", u.ID).
			Scan(&st.changes); err != nil {
			t.Fatalf("count changes: %v", err)
		}
		return st
	}

	before := read()
	if before.lookups != 0 || before.changes != 0 {
		t.Fatalf("fixture budgets = lookups %d changes %d, want 0/0", before.lookups, before.changes)
	}

	for _, in := range []string{"newhandle", "Operator", "heldbyuser", ""} {
		_, err := api.UpdateUsernameForTest(s, u.ID, in)
		if got := rpcMessage(t, err); got != "USERNAME_IMMUTABLE" {
			t.Errorf("updateUsername(%q) = %s, want USERNAME_IMMUTABLE", in, got)
		}
		if after := read(); !reflect.DeepEqual(before, after) {
			t.Errorf("state after updateUsername(%q) changed:\n before %+v\n after  %+v", in, before, after)
		}
	}

	// The unchanged input is refused too, and it also charges nothing.
	if _, err := api.UpdateUsernameForTest(s, u.ID, "operator"); rpcMessage(t, err) != "USERNAME_NOT_MODIFIED" {
		t.Errorf("updateUsername(\"operator\") want USERNAME_NOT_MODIFIED")
	}
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Errorf("state after unchanged input changed:\n before %+v\n after  %+v", before, after)
	}
}

// TestUpdateUsernameImmutableValidationPrecedence proves invalid and reserved
// names keep their existing answer on a username-mode account: the credential
// guard never turns a name that was never admissible into USERNAME_IMMUTABLE.
func TestUpdateUsernameImmutableValidationPrecedence(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	u := mustUsernameAccount(t, s, "operator")

	for _, in := range []string{"a", "1abc", "_abc", "bad-name", strings.Repeat("x", 33), "admin", "me", "support", "Settings"} {
		_, err := api.UpdateUsernameForTest(s, u.ID, in)
		if got := rpcMessage(t, err); got != "USERNAME_INVALID" {
			t.Errorf("updateUsername(%q) = %s, want USERNAME_INVALID", in, got)
		}
	}
}

// TestUpdateUsernamePhoneModeChargesOneLookup pins the phone-mode quota order:
// an ordinary changed handle still pays exactly one lookup row and records one
// change. The credential guard must not reorder budget for accounts that are
// allowed to move their handle.
func TestUpdateUsernamePhoneModeChargesOneLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)

	u, err := s.CreateUser(ctx, "+15550001203")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := api.UpdateUsernameForTest(s, u.ID, "phone_moved"); err != nil {
		t.Fatalf("phone-mode claim: %v", err)
	}

	pool := lookupPool(t, dsn)
	var lookups, changes int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM username_lookups WHERE caller_id = $1", u.ID).
		Scan(&lookups); err != nil {
		t.Fatalf("count lookups: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM username_changes WHERE user_id = $1", u.ID).
		Scan(&changes); err != nil {
		t.Fatalf("count changes: %v", err)
	}
	if lookups != 1 {
		t.Errorf("lookup rows = %d, want exactly 1", lookups)
	}
	if changes != 1 {
		t.Errorf("change rows = %d, want exactly 1", changes)
	}
}

// lookupPool opens a raw pool on the same database the store runs on, for a
// test that has to read the budget tables the handlers write.
func lookupPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// --- account.updateProfile tests ---

func TestUpdateProfileUnauthenticated(t *testing.T) {
	t.Parallel()
	s := openStore(t)

	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName(strings.Repeat("a", 65))
	req.SetAbout("private bio")
	_, err := api.UpdateProfileForTest(s, 0, req)
	if err == nil {
		t.Fatal("expected error for unauthenticated caller")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 401 || rpc.Message != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("error = %v, want RPC 401 AUTH_KEY_UNREGISTERED", err)
	}
}

func TestUpdateProfileSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004101")
	if err != nil {
		t.Fatal(err)
	}

	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName("Alice")
	req.SetLastName("Smith")
	res, err := api.UpdateProfileForTest(s, user.ID, req)
	if err != nil {
		t.Fatalf("update profile: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.FirstName != "Alice" || uRes.LastName != "Smith" {
		t.Fatalf("returned name = %q %q, want Alice Smith", uRes.FirstName, uRes.LastName)
	}
	if !uRes.Self {
		t.Fatal("returned user is not self")
	}

	got, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.FirstName != "Alice" || got.LastName != "Smith" {
		t.Fatalf("stored name = %q %q, want Alice Smith", got.FirstName, got.LastName)
	}
}

func TestUpdateProfileLastNameOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004102")
	if err != nil {
		t.Fatal(err)
	}
	seed := &tg.AccountUpdateProfileRequest{}
	seed.SetFirstName("Alice")
	seed.SetLastName("Smith")
	if _, err := api.UpdateProfileForTest(s, user.ID, seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := &tg.AccountUpdateProfileRequest{}
	req.SetLastName("Jones")
	res, err := api.UpdateProfileForTest(s, user.ID, req)
	if err != nil {
		t.Fatalf("update last: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.FirstName != "Alice" || uRes.LastName != "Jones" {
		t.Fatalf("returned name = %q %q, want Alice Jones", uRes.FirstName, uRes.LastName)
	}
}

func TestUpdateProfileRejectsOverlongOrNul(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004103")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		first string
		last  string
	}{
		{name: "long first", first: strings.Repeat("a", 65), last: "Valid"},
		{name: "long last", first: "Valid", last: strings.Repeat("界", 65)},
		{name: "nul first", first: "Bad\x00Name", last: "Valid"},
	}
	for _, tc := range cases {
		req := &tg.AccountUpdateProfileRequest{}
		req.SetFirstName(tc.first)
		req.SetLastName(tc.last)
		_, err := api.UpdateProfileForTest(s, user.ID, req)
		if err == nil {
			t.Errorf("%s: expected INPUT_REQUEST_INVALID, got success", tc.name)
			continue
		}
		var rpc *tgerr.Error
		if !errors.As(err, &rpc) || rpc.Message != "INPUT_REQUEST_INVALID" {
			t.Errorf("%s: error = %v, want INPUT_REQUEST_INVALID", tc.name, err)
		}
	}

	got, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.FirstName != "" || got.LastName != "" {
		t.Fatalf("rejected updates wrote names %q %q", got.FirstName, got.LastName)
	}
}

func TestUpdateProfileAcceptsSignupMaxLength(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004104")
	if err != nil {
		t.Fatal(err)
	}

	first := strings.Repeat("a", 64)
	last := strings.Repeat("界", 64)
	if utf8.RuneCountInString(last) != 64 {
		t.Fatalf("last rune count = %d, want 64", utf8.RuneCountInString(last))
	}
	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName(first)
	req.SetLastName(last)
	res, err := api.UpdateProfileForTest(s, user.ID, req)
	if err != nil {
		t.Fatalf("64-rune names must pass, as at signup: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.FirstName != first || uRes.LastName != last {
		t.Fatal("returned names did not match the 64-rune inputs")
	}
}

func TestUpdateProfileDoesNotChangeOtherUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	alice, err := s.CreateUser(ctx, "+15550004105")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "+15550004106")
	if err != nil {
		t.Fatal(err)
	}
	seed := &tg.AccountUpdateProfileRequest{}
	seed.SetFirstName("Bob")
	seed.SetLastName("Original")
	if _, err := api.UpdateProfileForTest(s, bob.ID, seed); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName("Alice")
	req.SetLastName("Smith")
	if _, err := api.UpdateProfileForTest(s, alice.ID, req); err != nil {
		t.Fatalf("update alice: %v", err)
	}

	got, ok, err := s.UserByID(ctx, bob.ID)
	if err != nil || !ok {
		t.Fatalf("lookup bob: ok=%v err=%v", ok, err)
	}
	if got.FirstName != "Bob" || got.LastName != "Original" {
		t.Fatalf("bob name = %q %q, want Bob Original", got.FirstName, got.LastName)
	}
}

func TestUpdateProfilePeerSeesNewName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	alice, err := s.CreateUser(ctx, "+15550004107")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := s.CreateUser(ctx, "+15550004108")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.SendMessageForTest(s, alice.ID, &tg.MessagesSendMessageRequest{
		Peer:     api.InputPeerUser(alice.ID, bob.ID),
		Message:  "hello",
		RandomID: 4108,
	}); err != nil {
		t.Fatalf("send: %v", err)
	}

	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName("Alicia")
	req.SetLastName("Renamed")
	if _, err := api.UpdateProfileForTest(s, alice.ID, req); err != nil {
		t.Fatalf("update profile: %v", err)
	}

	users, err := api.LoadUsersForTest(s, []int64{alice.ID}, bob.ID)
	if err != nil {
		t.Fatalf("load users: %v", err)
	}
	var seen *tg.User
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && user.ID == alice.ID {
			seen = user
			break
		}
	}
	if seen == nil {
		t.Fatal("bob did not receive alice as a full user")
	}
	if seen.FirstName != "Alicia" || seen.LastName != "Renamed" {
		t.Fatalf("bob sees alice as %q %q, want Alicia Renamed", seen.FirstName, seen.LastName)
	}
}

func TestUpdateProfileRejectsAboutByFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004109")
	if err != nil {
		t.Fatal(err)
	}
	seed := &tg.AccountUpdateProfileRequest{}
	seed.SetFirstName("Original")
	seed.SetLastName("Name")
	if _, err := api.UpdateProfileForTest(s, user.ID, seed); err != nil {
		t.Fatalf("seed profile: %v", err)
	}

	const bio = "Looking for a trail partner: private-bio-sentinel"
	cases := []struct {
		name string
		set  func(*tg.AccountUpdateProfileRequest)
	}{
		{
			name: "valid combined names",
			set: func(req *tg.AccountUpdateProfileRequest) {
				req.SetFirstName("Changed")
				req.SetLastName("Profile")
				req.SetAbout(bio)
			},
		},
		{
			name: "empty clear value",
			set: func(req *tg.AccountUpdateProfileRequest) {
				req.SetAbout("")
			},
		},
		{
			name: "invalid name takes about refusal",
			set: func(req *tg.AccountUpdateProfileRequest) {
				req.SetFirstName(strings.Repeat("a", 65))
				req.SetAbout(bio)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &tg.AccountUpdateProfileRequest{}
			tc.set(req)
			var logs bytes.Buffer
			res, err := api.UpdateProfileForTestWithLogger(s, user.ID, req, slog.New(slog.NewTextHandler(&logs, nil)))
			if res != nil {
				t.Fatalf("result = %T, want no successful User", res)
			}
			var rpc *tgerr.Error
			if !errors.As(err, &rpc) || rpc.Code != 400 || rpc.Message != "ABOUT_NOT_SUPPORTED" {
				t.Fatalf("error = %v, want RPC 400 ABOUT_NOT_SUPPORTED", err)
			}
			if strings.Contains(logs.String(), bio) {
				t.Fatalf("captured logs contain supplied bio: %q", logs.String())
			}
			got, ok, err := s.UserByID(ctx, user.ID)
			if err != nil || !ok {
				t.Fatalf("lookup: ok=%v err=%v", ok, err)
			}
			if got.FirstName != "Original" || got.LastName != "Name" {
				t.Fatalf("rejected update wrote names %q %q", got.FirstName, got.LastName)
			}
		})
	}

	full := getFullUserForTest(t, s, user.ID, &tg.InputUserSelf{})
	if _, ok := full.FullUser.GetAbout(); ok {
		t.Fatal("users.getFullUser reports an about value that is not stored")
	}
}

func TestUpdateProfileAboutRefusalDoesNotConsumeNameLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004112")
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.RateLimitConfig{Limit: 1, Window: time.Minute}

	refused := &tg.AccountUpdateProfileRequest{}
	refused.SetFirstName("Refused")
	refused.SetAbout("private")
	_, err = api.UpdateProfileForTestWithLimits(s, user.ID, cfg, refused)
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 400 || rpc.Message != "ABOUT_NOT_SUPPORTED" {
		t.Fatalf("about update error = %v, want RPC 400 ABOUT_NOT_SUPPORTED", err)
	}

	valid := &tg.AccountUpdateProfileRequest{}
	valid.SetFirstName("Accepted")
	res, err := api.UpdateProfileForTestWithLimits(s, user.ID, cfg, valid)
	if err != nil {
		t.Fatalf("name-only update after refusal: %v", err)
	}
	uRes, ok := res.(*tg.User)
	if !ok {
		t.Fatalf("result = %T, want *tg.User", res)
	}
	if uRes.FirstName != "Accepted" {
		t.Fatalf("returned first name = %q, want Accepted", uRes.FirstName)
	}
	got, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.FirstName != "Accepted" {
		t.Fatalf("stored first name = %q, want Accepted", got.FirstName)
	}
}

func TestUpdateProfileRateLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	user, err := s.CreateUser(ctx, "+15550004110")
	if err != nil {
		t.Fatal(err)
	}

	cfg := store.RateLimitConfig{Limit: 2, Window: 10 * time.Second}
	for i := range 2 {
		req := &tg.AccountUpdateProfileRequest{}
		req.SetFirstName("Name")
		if _, err := api.UpdateProfileForTestWithLimits(s, user.ID, cfg, req); err != nil {
			t.Fatalf("update %d: %v", i+1, err)
		}
	}
	req := &tg.AccountUpdateProfileRequest{}
	req.SetFirstName("Blocked")
	_, err = api.UpdateProfileForTestWithLimits(s, user.ID, cfg, req)
	if err == nil {
		t.Fatal("expected FLOOD_WAIT on third update")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 420 {
		t.Fatalf("error = %v, want FLOOD_WAIT", err)
	}

	got, ok, err := s.UserByID(ctx, user.ID)
	if err != nil || !ok {
		t.Fatalf("lookup: ok=%v err=%v", ok, err)
	}
	if got.FirstName != "Name" {
		t.Fatalf("denied update wrote first_name = %q", got.FirstName)
	}
}
