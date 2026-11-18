package api_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
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

func assertChannelHistoryContainsText(t *testing.T, s *store.Store, channelID, userID int64, want string) {
	t.Helper()
	messages, err := s.ChannelHistory(context.Background(), channelID, 0, 100)
	if err != nil {
		t.Fatalf("read General history: %v", err)
	}
	for _, message := range messages {
		if message.FromID == userID && message.Message == want {
			return
		}
	}
	t.Fatalf("General history = %+v, want stored text %q from user %d", messages, want, userID)
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

func TestSignUpDemoGeneralChatJoinFailuresAreBestEffort(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"channel lookup", "membership insert"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			s, dsn := openStoreDSN(t)
			channel := createGeneralChat(t, s)
			conn, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatalf("open fault-injection connection: %v", err)
			}
			defer conn.Close()

			channelsUnavailable := false
			if failure == "channel lookup" {
				if _, err := conn.Exec(ctx, `ALTER TABLE channels RENAME TO test_channels_unavailable`); err != nil {
					t.Fatalf("hide channels table: %v", err)
				}
				channelsUnavailable = true
				defer func() {
					if channelsUnavailable {
						if _, err := conn.Exec(context.Background(), `ALTER TABLE test_channels_unavailable RENAME TO channels`); err != nil {
							t.Errorf("restore channels table: %v", err)
						}
					}
				}()
			} else {
				createFunction := fmt.Sprintf(`CREATE FUNCTION test_fail_general_join() RETURNS trigger AS $$
BEGIN
  IF NEW.channel_id = %d THEN
    RAISE EXCEPTION 'injected General membership failure';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql`, channel.ID)
				if _, err := conn.Exec(ctx, createFunction); err != nil {
					t.Fatalf("create join failure function: %v", err)
				}
				createTrigger := `CREATE TRIGGER test_fail_general_join
BEFORE INSERT ON channel_participants FOR EACH ROW EXECUTE FUNCTION test_fail_general_join()`
				if _, err := conn.Exec(ctx, createTrigger); err != nil {
					t.Fatalf("create join failure trigger: %v", err)
				}
			}

			logs := &captureHandler{}
			runner := api.NewSignUpTestRunner(s, [8]byte{}, netip.MustParseAddr("203.0.113.17"), store.RateLimitConfig{}, config.RegistrationOpen, slog.New(logs))
			runner.SetDemoGeneralChatEnabledForTest(true)
			user := signUpWelcomeTestAccount(t, s, runner, [8]byte{9}, "best_effort_"+strings.ReplaceAll(failure, " ", "_"))
			if failure == "channel lookup" {
				if _, err := conn.Exec(ctx, `ALTER TABLE test_channels_unavailable RENAME TO channels`); err != nil {
					t.Fatalf("restore channels table after sign-up: %v", err)
				}
				channelsUnavailable = false
			}

			persisted, found, err := s.UserByID(ctx, user.ID)
			if err != nil || !found || persisted.ID != user.ID {
				t.Fatalf("persisted sign-up user = %+v, found=%v, err=%v", persisted, found, err)
			}
			if member, found, err := s.ChannelMemberOf(ctx, channel.ID, user.ID); err != nil || found {
				t.Fatalf("General membership after %s failure = %+v, found=%v, err=%v; want no membership", failure, member, found, err)
			}
			if len(logs.records) == 0 {
				t.Fatal("best-effort General join failure was not logged")
			}
		})
	}
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
	assertChannelHistoryContainsText(t, s, channel.ID, duplicateGuest.ID, "same note")
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
		assertChannelHistoryContainsText(t, s, channel.ID, rateLimitedGuest.ID, fmt.Sprintf("note %d", i))
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
		assertChannelHistoryContainsText(t, s, channel.ID, hourLimitedGuest.ID, fmt.Sprintf("hourly note %d", i))
	}
	_, err = api.SendMessageForTestWithDemoGeneralChatLimits(s, hourLimitedGuest.ID, minuteLimit, hourLimit, &tg.MessagesSendMessageRequest{
		Peer: peer, Message: "third hourly note", RandomID: 32,
	})
	rpcError(t, err, "FLOOD_WAIT_3600")
}

func TestDemoGeneralChatLimitsAlbumCaptionsAndItemRate(t *testing.T) {
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
	album := func(caption string, count int, fileBase, randomBase int64) *tg.MessagesSendMultiMediaRequest {
		request := &tg.MessagesSendMultiMediaRequest{
			Peer:       api.InputPeerChannel(0, channel.ID),
			MultiMedia: make([]tg.InputSingleMedia, count),
		}
		for i := range count {
			request.MultiMedia[i] = tg.InputSingleMedia{
				Media:   &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: fileBase + int64(i), AccessHash: 1}},
				Message: caption, RandomID: randomBase + int64(i),
			}
		}
		return request
	}

	t.Run("six items exceed per-guest minute limit", func(t *testing.T) {
		guest := newMember("+15550000125")
		request := album("photo caption", 6, 12500, 12600)
		request.Peer = api.InputPeerChannel(guest.ID, channel.ID)
		_, err := api.SendMultiMediaForTestWithDemoGeneralChat(s, guest.ID, request)
		rpcError(t, err, "FLOOD_WAIT_60")
	})

	t.Run("duplicate captions are refused", func(t *testing.T) {
		guest := newMember("+15550000126")
		request := album("same album caption", 2, 12600, 12700)
		request.Peer = api.InputPeerChannel(guest.ID, channel.ID)
		_, err := api.SendMultiMediaForTestWithDemoGeneralChat(s, guest.ID, request)
		rpcError(t, err, "FLOOD_WAIT_30")
	})

	t.Run("oversized caption is refused", func(t *testing.T) {
		guest := newMember("+15550000127")
		request := album(strings.Repeat("x", 1001), 2, 12700, 12800)
		request.Peer = api.InputPeerChannel(guest.ID, channel.ID)
		_, err := api.SendMultiMediaForTestWithDemoGeneralChat(s, guest.ID, request)
		rpcError(t, err, "MEDIA_CAPTION_TOO_LONG")
	})
}

func TestDemoGeneralChatPhotoCaptionAndMinuteLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	channel := createGeneralChat(t, s)
	blobs := newBlobs(t)
	body := jpegPhotoPayload(t, 640, 480)
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

	captionGuest := newMember("+15550000128")
	const oversizedFileID = int64(12801)
	_, err := api.SendMediaForTestWithDemoGeneralChat(s, captionGuest.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerChannel(captionGuest.ID, channel.ID),
		Media:   uploadedPhoto(oversizedFileID, 1, "general.jpg", jpegPhotoMD5(body)),
		Message: strings.Repeat("x", 1001), RandomID: oversizedFileID,
	})
	rpcError(t, err, "MEDIA_CAPTION_TOO_LONG")

	rateGuest := newMember("+15550000129")
	for i := range 5 {
		fileID := int64(12900 + i)
		saveParts(t, s, rateGuest.ID, fileID, body)
		if _, err := api.SendMediaForTestWithDemoGeneralChat(s, rateGuest.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer:    api.InputPeerChannel(rateGuest.ID, channel.ID),
			Media:   uploadedPhoto(fileID, 1, "general.jpg", jpegPhotoMD5(body)),
			Message: fmt.Sprintf("photo %d", i), RandomID: fileID,
		}); err != nil {
			t.Fatalf("send General photo %d of 5: %v", i+1, err)
		}
	}
	const sixthFileID = int64(12905)
	saveParts(t, s, rateGuest.ID, sixthFileID, body)
	fileIDCeilingBefore, err := s.MaxFileID(ctx)
	if err != nil {
		t.Fatalf("read file id ceiling before refused photo: %v", err)
	}
	observedBlobs := &observedAssembledPutBlob{Store: s.PartBlobs()}
	_, err = api.SendMediaForTestWithDemoGeneralChat(s, rateGuest.ID, observedBlobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerChannel(rateGuest.ID, channel.ID),
		Media:   uploadedPhoto(sixthFileID, 1, "general.jpg", jpegPhotoMD5(body)),
		Message: "photo 5", RandomID: sixthFileID,
	})
	rpcError(t, err, "FLOOD_WAIT_60")
	if keys := observedBlobs.assembledKeys(); len(keys) != 0 {
		t.Errorf("over-limit photo assembled blobs %v, want no persisted file", keys)
	}
	fileIDCeilingAfter, err := s.MaxFileID(ctx)
	if err != nil {
		t.Fatalf("read file id ceiling after refused photo: %v", err)
	}
	if fileIDCeilingAfter != fileIDCeilingBefore {
		t.Errorf("over-limit photo changed stored file id ceiling from %d to %d", fileIDCeilingBefore, fileIDCeilingAfter)
	}
}

func TestDemoGeneralChatSlowModePhotoRefusalDiscardsFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	channel := createGeneralChat(t, s)
	blobs := s.PartBlobs()
	body := jpegPhotoPayload(t, 640, 480)
	guest, err := s.CreateUser(ctx, "+15550000132")
	if err != nil {
		t.Fatalf("create General guest: %v", err)
	}
	if _, _, err := s.JoinChannelByUsername(ctx, channel.ID, guest.ID); err != nil {
		t.Fatalf("join General: %v", err)
	}
	const firstFileID = int64(13201)
	saveParts(t, s, guest.ID, firstFileID, body)
	if _, err := api.SendMediaForTestWithDemoGeneralChat(s, guest.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:    api.InputPeerChannel(guest.ID, channel.ID),
		Media:   uploadedPhoto(firstFileID, 1, "general.jpg", jpegPhotoMD5(body)),
		Message: "first photo before slow mode", RandomID: firstFileID,
	}); err != nil {
		t.Fatalf("send first General photo: %v", err)
	}
	const refusedFileID = int64(13202)
	saveParts(t, s, guest.ID, refusedFileID, body)
	fileIDCeilingBefore, err := s.MaxFileID(ctx)
	if err != nil {
		t.Fatalf("read file id ceiling before slow-mode refusal: %v", err)
	}
	gate := &assembledPutGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(gate.unblock)
	observedBlobs := &observedAssembledPutBlob{Store: blobs, gate: gate}
	result := make(chan error, 1)
	go func() {
		_, sendErr := api.SendMediaForTestWithDemoGeneralChat(s, guest.ID, observedBlobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer:    api.InputPeerChannel(guest.ID, channel.ID),
			Media:   uploadedPhoto(refusedFileID, 1, "general.jpg", jpegPhotoMD5(body)),
			Message: "photo refused by slow mode", RandomID: refusedFileID,
		})
		result <- sendErr
	}()
	select {
	case <-gate.entered:
	case sendErr := <-result:
		gate.unblock()
		t.Fatalf("photo send exited before assembly gate: %v", sendErr)
	case <-time.After(30 * time.Second):
		gate.unblock()
		t.Fatal("photo did not pass precheck and reach blob assembly")
	}
	if _, changed, err := s.SetChannelSlowMode(ctx, channel.ID, channel.CreatorID, 10); err != nil || !changed {
		gate.unblock()
		t.Fatalf("set General slow mode during assembly changed=%v err=%v", changed, err)
	}
	gate.unblock()
	select {
	case err = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("slow-mode refused photo did not finish after blob assembly resumed")
	}
	var rpcErr *tgerr.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != 420 || !strings.HasPrefix(rpcErr.Message, "SLOWMODE_WAIT_") {
		t.Fatalf("slow-mode photo refusal = %v, want 420 SLOWMODE_WAIT", err)
	}
	fileIDCeilingAfter, err := s.MaxFileID(ctx)
	if err != nil {
		t.Fatalf("read file id ceiling after slow-mode refusal: %v", err)
	}
	if fileIDCeilingAfter != fileIDCeilingBefore {
		t.Errorf("slow-mode refusal changed stored file id ceiling from %d to %d", fileIDCeilingBefore, fileIDCeilingAfter)
	}
	keys := observedBlobs.assembledKeys()
	if len(keys) != 1 {
		t.Fatalf("slow-mode photo assembled blob keys = %v, want one discarded file", keys)
	}
	if _, err := blobs.ReadAt(ctx, keys[0], 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("slow-mode refusal blob read error = %v, want blob.ErrNotFound", err)
	}
}

func TestDemoGeneralChatPhotoRateLimitRaceCleansLoser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	channel := createGeneralChat(t, s)
	blobs := s.PartBlobs()
	body := jpegPhotoPayload(t, 640, 480)
	guest, err := s.CreateUser(ctx, "+15550000131")
	if err != nil {
		t.Fatalf("create General guest: %v", err)
	}
	if _, _, err := s.JoinChannelByUsername(ctx, channel.ID, guest.ID); err != nil {
		t.Fatalf("join General: %v", err)
	}
	for i := range 4 {
		fileID := int64(13100 + i)
		saveParts(t, s, guest.ID, fileID, body)
		if _, err := api.SendMediaForTestWithDemoGeneralChat(s, guest.ID, blobs, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer:     api.InputPeerChannel(guest.ID, channel.ID),
			Media:    uploadedPhoto(fileID, 1, "general.jpg", jpegPhotoMD5(body)),
			Message:  fmt.Sprintf("accepted photo %d", i),
			RandomID: fileID,
		}); err != nil {
			t.Fatalf("send General photo %d of 4: %v", i+1, err)
		}
	}

	gate := &assembledPutGate{entered: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(gate.unblock)
	const racingPhotoFileID = int64(13104)
	saveParts(t, s, guest.ID, racingPhotoFileID, body)
	observer := &observedAssembledPutBlob{Store: blobs, gate: gate}
	result := make(chan error, 1)
	go func() {
		_, sendErr := api.SendMediaForTestWithDemoGeneralChat(s, guest.ID, observer, api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
			Peer:     api.InputPeerChannel(guest.ID, channel.ID),
			Media:    uploadedPhoto(racingPhotoFileID, 1, "general.jpg", jpegPhotoMD5(body)),
			Message:  "photo raced by a text post",
			RandomID: racingPhotoFileID,
		})
		result <- sendErr
	}()
	select {
	case <-gate.entered:
	case sendErr := <-result:
		gate.unblock()
		t.Fatalf("photo send exited before assembly gate: %v", sendErr)
	case <-time.After(30 * time.Second):
		gate.unblock()
		t.Fatal("photo did not pass preflight and reach blob assembly")
	}

	if _, err := api.SendMessageForTestWithDemoGeneralChat(s, guest.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(guest.ID, channel.ID), Message: "consume the final General message token", RandomID: 13199,
	}); err != nil {
		gate.unblock()
		t.Fatalf("post General text while photo assembly is in flight: %v", err)
	}
	gate.unblock()
	var sendErr error
	select {
	case sendErr = <-result:
	case <-time.After(30 * time.Second):
		t.Fatal("rate-limit loser did not finish after blob assembly resumed")
	}
	rpcError(t, sendErr, "FLOOD_WAIT_60")

	keys := observer.assembledKeys()
	if len(keys) != 1 {
		t.Fatalf("racing photo assembly keys = %v, want one", keys)
	}
	fileID, ok := blob.ParseKey(keys[0])
	if !ok {
		t.Fatalf("assembled blob key %q does not identify a file", keys[0])
	}
	files, err := s.ExistingFileIDs(ctx, []int64{fileID})
	if err != nil {
		t.Fatalf("check racing photo file row %d: %v", fileID, err)
	}
	if _, exists := files[fileID]; exists {
		t.Errorf("rate-limit loser retained file row %d", fileID)
	}
	if _, err := blobs.ReadAt(ctx, keys[0], 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("rate-limit loser blob read error = %v, want blob.ErrNotFound", err)
	}
}

func TestDemoGeneralChatDoesNotLimitOtherMegagroups(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	_ = createGeneralChat(t, s)
	other, _ := createWelcomeChannel(t, s, "ordinary_group", true)
	guest, err := s.CreateUser(ctx, "+15550000130")
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	if _, _, err := s.JoinChannelByUsername(ctx, other.ID, guest.ID); err != nil {
		t.Fatalf("join other megagroup: %v", err)
	}
	peer := api.InputPeerChannel(guest.ID, other.ID)
	for i := range 10 {
		if _, err := api.SendMessageForTestWithDemoGeneralChat(s, guest.ID, &tg.MessagesSendMessageRequest{
			Peer: peer, Message: fmt.Sprintf("ordinary message %d", i), RandomID: int64(13000 + i),
		}); err != nil {
			t.Fatalf("send other-megagroup message %d of 10: %v", i+1, err)
		}
	}
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
