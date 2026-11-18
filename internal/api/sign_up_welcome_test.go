package api_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
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

func createGeneralChat(t *testing.T, s *store.Store) store.Channel {
	t.Helper()
	ctx := context.Background()
	owner, err := s.CreateUser(ctx, "+15550000119")
	if err != nil {
		t.Fatalf("create General chat owner: %v", err)
	}
	channel, _, _, err := s.CreateChannelWithServiceMessage(ctx, owner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create General chat: %v", err)
	}
	channel, _, err = s.SetChannelDefaultBannedRights(ctx, channel.ID, owner.ID, []string{
		"change_info", "invite_users", "pin_messages", "manage_topics", "send_polls", "send_games", "send_inline",
		"send_gifs", "send_videos", "send_roundvideos", "send_audios", "send_voices", "send_docs",
	})
	if err != nil {
		t.Fatalf("restrict General guest permissions: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, channel.ID, "teagramdemogeneral"); err != nil {
		t.Fatalf("claim General chat handle: %v", err)
	}
	return channel
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

func TestSignUpDemoGeneralChatAutoJoinIsFlagGated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	channel := createGeneralChat(t, s)
	for _, allowed := range []string{"send_messages", "send_media", "send_photos", "send_stickers", "send_plain"} {
		if slices.Contains(channel.DefaultBannedRights, allowed) {
			t.Errorf("General default banned rights = %v, unexpectedly restricts %s", channel.DefaultBannedRights, allowed)
		}
	}
	logs := &captureHandler{}
	disabledRunner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.16"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	disabledUser := signUpWelcomeTestAccount(t, s, disabledRunner, [8]byte{7}, "freshgeneraloff")
	if member, found, err := s.ChannelMemberOf(ctx, channel.ID, disabledUser.ID); err != nil || found {
		t.Fatalf("new account General membership with flag off = %+v, found=%v, err=%v; want no membership", member, found, err)
	}

	runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.15"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
	runner.SetDemoGeneralChatEnabledForTest(true)
	user := signUpWelcomeTestAccount(t, s, runner, [8]byte{8}, "freshgeneraluser")

	member, found, err := s.ChannelMemberOf(ctx, channel.ID, user.ID)
	if err != nil || !found {
		t.Fatalf("new account General membership = %+v, found=%v, err=%v; want immediate membership", member, found, err)
	}
	if member.Role != 0 || member.AdminChangeInfo {
		t.Fatalf("new account General role = %+v, want a plain member", member)
	}
	response, err := api.GetDialogsForTest(s, user.ID)
	if err != nil {
		t.Fatalf("get dialogs after General auto-join: %v", err)
	}
	dialogs, ok := response.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("get dialogs response = %T, want *tg.MessagesDialogs", response)
	}
	for _, raw := range dialogs.Dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerChannel)
		if ok && peer.ChannelID == channel.ID {
			return
		}
	}
	t.Fatalf("new account dialogs = %+v, want General channel %d", dialogs.Dialogs, channel.ID)
}

func TestDemoGeneralChatRejectsOversizeDuplicateAndExcessMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	channel := createGeneralChat(t, s)
	newMember := func(phone string) store.User {
		t.Helper()
		user, err := s.CreateUser(ctx, phone)
		if err != nil {
			t.Fatalf("create General guest: %v", err)
		}
		if _, _, err := s.JoinChannelByUsername(ctx, channel.ID, user.ID); err != nil {
			t.Fatalf("join General: %v", err)
		}
		return user
	}

	oversizeGuest := newMember("+15550000120")
	_, err := api.SendMessageForTestWithDemoGeneralChat(s, oversizeGuest.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(oversizeGuest.ID, channel.ID), Message: strings.Repeat("x", 1001), RandomID: 1,
	})
	rpcError(t, err, "MESSAGE_TOO_LONG")

	duplicateGuest := newMember("+15550000121")
	peer := api.InputPeerChannel(duplicateGuest.ID, channel.ID)
	if _, err := api.SendMessageForTestWithDemoGeneralChat(s, duplicateGuest.ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "same note", RandomID: 2,
	}); err != nil {
		t.Fatalf("send first identical-text check message: %v", err)
	}
	_, err = api.SendMessageForTestWithDemoGeneralChat(s, duplicateGuest.ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "same note", RandomID: 3,
	})
	rpcError(t, err, "FLOOD_WAIT_30")

	rateLimitedGuest := newMember("+15550000122")
	peer = api.InputPeerChannel(rateLimitedGuest.ID, channel.ID)
	for i := range 5 {
		if _, err := api.SendMessageForTestWithDemoGeneralChat(s, rateLimitedGuest.ID, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: fmt.Sprintf("note %d", i), RandomID: int64(i + 10),
		}); err != nil {
			t.Fatalf("send General message %d of 5: %v", i+1, err)
		}
	}
	_, err = api.SendMessageForTestWithDemoGeneralChat(s, rateLimitedGuest.ID, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "sixth note", RandomID: 16,
	})
	rpcError(t, err, "FLOOD_WAIT_60")

	hourLimitedGuest := newMember("+15550000123")
	peer = api.InputPeerChannel(hourLimitedGuest.ID, channel.ID)
	minuteLimit := store.RateLimitConfig{Limit: 100, Window: time.Minute}
	hourLimit := store.RateLimitConfig{Limit: 2, Window: time.Hour}
	for i := range 2 {
		if _, err := api.SendMessageForTestWithDemoGeneralChatLimits(s, hourLimitedGuest.ID, minuteLimit, hourLimit, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: fmt.Sprintf("hourly note %d", i), RandomID: int64(i + 30),
		}); err != nil {
			t.Fatalf("send hourly General message %d of 2: %v", i+1, err)
		}
	}
	_, err = api.SendMessageForTestWithDemoGeneralChatLimits(s, hourLimitedGuest.ID, minuteLimit, hourLimit, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "third hourly note", RandomID: 32,
	})
	rpcError(t, err, "FLOOD_WAIT_3600")
}

func TestDemoGeneralChatAllowsGuestPhotoPosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	channel := createGeneralChat(t, s)
	guest, err := s.CreateUser(ctx, "+15550000124")
	if err != nil {
		t.Fatalf("create General guest: %v", err)
	}
	if _, _, err := s.JoinChannelByUsername(ctx, channel.ID, guest.ID); err != nil {
		t.Fatalf("join General: %v", err)
	}
	blobs := newBlobs(t)
	body := jpegPhotoPayload(t, 640, 480)
	const fileID = 12401
	saveParts(t, s, guest.ID, fileID, body)
	sent, err := api.SendMediaForTestWithDemoGeneralChat(s, guest.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerChannel(guest.ID, channel.ID),
		Media:    uploadedPhoto(fileID, 1, "general.jpg", jpegPhotoMD5(body)),
		Message:  "a small field photo",
		RandomID: fileID,
	})
	if err != nil {
		t.Fatalf("send General photo: %v", err)
	}
	post, _ := channelPhotoPostOf(t, sent)
	photo := photoOfMessage(t, post)
	if photo.ID == 0 || post.Message != "a small field photo" {
		t.Fatalf("General photo post = id %d caption %q, want a stored photo and its caption", photo.ID, post.Message)
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
