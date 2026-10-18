package api_test

import (
	"context"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func createWelcomeChannel(t *testing.T, s *store.Store, handle string, megagroup bool) (store.Channel, store.ChannelMessage) {
	t.Helper()
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15550000101")
	if err != nil {
		t.Fatalf("create welcome channel owner: %v", err)
	}
	channel, message, _, err := s.CreateChannelWithServiceMessage(ctx, owner.ID, "Welcome", "", megagroup)
	if err != nil {
		t.Fatalf("create welcome channel: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, channel.ID, handle); err != nil {
		t.Fatalf("claim welcome channel handle: %v", err)
	}
	return channel, message
}

func signUpWelcomeTestAccount(t *testing.T, s *store.Store, runner *api.SignUpTestRunner, keyID [8]byte, username string) *tg.User {
	t.Helper()
	ctx := context.Background()
	runner.SetAuthKeyIDForTest(keyID)
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(keyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, username)
	if err != nil {
		t.Fatalf("issue sign-up code: %v", err)
	}
	response, err := runner.SignUp(&tg.AuthSignUpRequest{
		PhoneNumber:   username,
		PhoneCodeHash: hash,
		FirstName:     "Welcome",
		LastName:      "Reader",
	})
	if err != nil {
		t.Fatalf("sign up %q: %v", username, err)
	}
	authorization, ok := response.(*tg.AuthAuthorization)
	if !ok {
		t.Fatalf("sign-up response = %T, want *tg.AuthAuthorization", response)
	}
	user, ok := authorization.User.(*tg.User)
	if !ok || user.ID == 0 {
		t.Fatalf("sign-up user = %T (%+v), want a persisted user", authorization.User, authorization.User)
	}
	return user
}

func TestSignUpJoinsConfiguredPublicChannelAndReturnsLatestMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	const handle = "welcome_demo"
	channel, latest := createWelcomeChannel(t, s, handle, false)
	existing, err := s.CreateUser(ctx, "+15550000102")
	if err != nil {
		t.Fatalf("create existing account: %v", err)
	}
	if member, found, err := s.ChannelMemberOf(ctx, channel.ID, existing.ID); err != nil || found {
		t.Fatalf("existing account membership before sign-up = %+v, found=%v, err=%v; want no membership", member, found, err)
	}

	logs := &captureHandler{}
	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.10"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	runner.SetWelcomeChannelForTest(handle)
	user := signUpWelcomeTestAccount(t, s, runner, [8]byte{1}, "freshwelcome")

	member, found, err := s.ChannelMemberOf(ctx, channel.ID, user.ID)
	if err != nil || !found {
		t.Fatalf("new account channel membership = %+v, found=%v, err=%v; want immediate membership", member, found, err)
	}
	if member.Role != 0 || member.AdminChangeInfo {
		t.Fatalf("new account channel role = %+v, want a plain member", member)
	}
	if oldMember, found, err := s.ChannelMemberOf(ctx, channel.ID, existing.ID); err != nil || found {
		t.Fatalf("existing account membership after sign-up = %+v, found=%v, err=%v; want no retroactive join", oldMember, found, err)
	}

	response, err := api.GetDialogsForTest(s, user.ID)
	if err != nil {
		t.Fatalf("get dialogs after sign-up: %v", err)
	}
	dialogs, ok := response.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("get dialogs response = %T, want *tg.MessagesDialogs", response)
	}
	if len(dialogs.Dialogs) != 1 {
		t.Fatalf("new account dialogs = %d, want the welcome channel", len(dialogs.Dialogs))
	}
	dialog, ok := dialogs.Dialogs[0].(*tg.Dialog)
	if !ok {
		t.Fatalf("new account dialog = %T, want *tg.Dialog", dialogs.Dialogs[0])
	}
	peer, ok := dialog.Peer.(*tg.PeerChannel)
	if !ok || peer.ChannelID != channel.ID {
		t.Fatalf("welcome dialog peer = %+v, want channel %d", dialog.Peer, channel.ID)
	}
	if dialog.TopMessage != int(latest.LocalID) {
		t.Fatalf("welcome dialog top message = %d, want latest message %d", dialog.TopMessage, latest.LocalID)
	}
	if len(dialogs.Messages) != 1 || dialogs.Messages[0].GetID() != int(latest.LocalID) {
		t.Fatalf("dialog messages = %+v, want latest message %d", dialogs.Messages, latest.LocalID)
	}
	if len(logs.records) != 0 {
		t.Fatalf("valid welcome channel logged warnings: %+v", logs.records)
	}
}

func TestSignUpSkipsConfiguredMegagroup(t *testing.T) {
	t.Parallel()
	s, _ := openStoreDSN(t)
	channel, _ := createWelcomeChannel(t, s, "welcome_group", true)
	logs := &captureHandler{}
	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.11"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	runner.SetWelcomeChannelForTest("welcome_group")
	user := signUpWelcomeTestAccount(t, s, runner, [8]byte{2}, "freshgroupuser")
	if member, found, err := s.ChannelMemberOf(context.Background(), channel.ID, user.ID); err != nil || found {
		t.Fatalf("new account megagroup membership = %+v, found=%v, err=%v; want no join", member, found, err)
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelWarn {
		t.Fatalf("megagroup warning records = %+v, want one warning", logs.records)
	}
	if fields := attrs(logs.records[0]); fields["handle"] != "welcome_group" {
		t.Fatalf("megagroup warning fields = %#v, want configured handle", fields)
	}
}

func TestSignUpWarnsOnceForUnknownWelcomeChannel(t *testing.T) {
	t.Parallel()
	s, _ := openStoreDSN(t)
	logs := &captureHandler{}
	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.12"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	runner.SetWelcomeChannelForTest("missing_welcome")
	for i, username := range []string{"freshmissingone", "freshmissingtwo"} {
		var keyID [8]byte
		keyID[7] = byte(i + 3)
		signUpWelcomeTestAccount(t, s, runner, keyID, username)
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelWarn {
		t.Fatalf("unknown-channel warning records = %+v, want one warning across sign-ups", logs.records)
	}
	fields := attrs(logs.records[0])
	if fields["handle"] != "missing_welcome" {
		t.Errorf("unknown-channel warning fields = %#v, want configured handle", fields)
	}
	if _, ok := fields["user_id"]; ok {
		t.Errorf("unknown-channel warning includes a user identifier: %#v", fields)
	}
}

func TestSignUpWelcomeJoinTimeoutDoesNotFailSignUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	channel, _ := createWelcomeChannel(t, s, "welcome_timeout", false)
	logs := &captureHandler{}
	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.14"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	runner.SetWelcomeChannelForTest("welcome_timeout")
	keyID := [8]byte{6}
	runner.SetAuthKeyIDForTest(keyID)
	if err := s.SaveAuthKey(ctx, mtproto.AuthKeyIDInt64(keyID), make([]byte, 256)); err != nil {
		t.Fatalf("save auth key: %v", err)
	}
	hash, _, err := s.IssueCodeForUsername(ctx, "freshtimeout")
	if err != nil {
		t.Fatalf("issue sign-up code: %v", err)
	}
	request := &tg.AuthSignUpRequest{
		PhoneNumber:   "freshtimeout",
		PhoneCodeHash: hash,
		FirstName:     "Welcome",
		LastName:      "Reader",
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open lock pool: %v", err)
	}
	defer pool.Close()
	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin channel-state lock: %v", err)
	}
	lockTxReleased := false
	defer func() {
		if lockTxReleased {
			return
		}
		if err := lockTx.Rollback(context.Background()); err != nil {
			t.Errorf("release channel-state lock: %v", err)
		}
	}()
	var lockedChannelID int64
	if err := lockTx.QueryRow(ctx, `SELECT channel_id FROM channel_state WHERE channel_id = $1 FOR UPDATE`, channel.ID).Scan(&lockedChannelID); err != nil {
		t.Fatalf("lock welcome channel state: %v", err)
	}

	type signUpResult struct {
		response any
		err      error
	}
	done := make(chan signUpResult, 1)
	go func() {
		response, err := runner.SignUp(request)
		done <- signUpResult{response: response, err: err}
	}()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	var result signUpResult
	select {
	case result = <-done:
	case <-timer.C:
		if err := lockTx.Rollback(context.Background()); err != nil {
			t.Errorf("release channel-state lock: %v", err)
		} else {
			lockTxReleased = true
		}
		result = <-done
		t.Fatalf("sign-up waited for the welcome channel lock instead of timing out; signup error = %v", result.err)
	}
	if err := lockTx.Rollback(context.Background()); err != nil {
		t.Fatalf("release channel-state lock: %v", err)
	}
	lockTxReleased = true
	authorization, ok := result.response.(*tg.AuthAuthorization)
	if result.err != nil || !ok {
		t.Fatalf("sign-up after welcome join timeout = (%T, %v), want authorization", result.response, result.err)
	}
	user, ok := authorization.User.(*tg.User)
	if !ok || user.ID == 0 {
		t.Fatalf("sign-up user after welcome join timeout = %T (%+v), want a persisted user", authorization.User, authorization.User)
	}
	if member, found, err := s.ChannelMemberOf(ctx, channel.ID, user.ID); err != nil || found {
		t.Fatalf("new account membership after welcome join timeout = %+v, found=%v, err=%v; want no join", member, found, err)
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelWarn {
		t.Fatalf("welcome join timeout warnings = %+v, want one warning", logs.records)
	}
}

func TestSignUpWithoutWelcomeChannelDoesNotJoin(t *testing.T) {
	t.Parallel()
	s, _ := openStoreDSN(t)
	channel, _ := createWelcomeChannel(t, s, "welcome_unconfigured", false)
	logs := &captureHandler{}
	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.13"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	user := signUpWelcomeTestAccount(t, s, runner, [8]byte{5}, "freshunconfigured")
	if member, found, err := s.ChannelMemberOf(context.Background(), channel.ID, user.ID); err != nil || found {
		t.Fatalf("new account membership without configuration = %+v, found=%v, err=%v; want no join", member, found, err)
	}
	if len(logs.records) != 1 || logs.records[0].Level != slog.LevelWarn {
		t.Fatalf("unset-channel warning records = %+v, want one warning", logs.records)
	}
}
