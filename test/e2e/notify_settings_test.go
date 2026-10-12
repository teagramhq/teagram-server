package e2e_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestNotifySettings(t *testing.T) {
	t.Parallel()
	testSmokeNotifySettings(t)
}

func TestNotifySettingsPeerKindsAndRefusals(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551049201", "+15551049202", "+15551049203"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
	a := newSmokeClient(t, f, "notify settings peer kinds", phoneA)
	b := dialogPinUser(t, f, phoneB)
	c := dialogPinUser(t, f, phoneC)
	seedDialogPinDM(t, f, a.id, b.ID, 1049201)

	pool, err := pgxpool.New(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("open settings fixture pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err = pool.Exec(f.ctx, `
		INSERT INTO chats (id, title, creator_id) VALUES ($1::bigint, 'colliding peer id', $2::bigint)
	`, b.ID, b.ID); err != nil {
		t.Fatalf("create group sharing the user id: %v", err)
	}
	if _, err = pool.Exec(f.ctx, `
		INSERT INTO chat_participants (chat_id, user_id, inviter_id)
		VALUES ($1::bigint, $2::bigint, $2::bigint), ($1::bigint, $3::bigint, $2::bigint)
	`, b.ID, b.ID, a.id); err != nil {
		t.Fatalf("add colliding group members: %v", err)
	}
	if _, err = pool.Exec(f.ctx, `
		SELECT setval(pg_get_serial_sequence('chats', 'id'), GREATEST((SELECT max(id) FROM chats), 1))
	`); err != nil {
		t.Fatalf("advance chat id sequence: %v", err)
	}

	userPeer := &tg.InputNotifyPeer{Peer: peerUser(a.id, b.ID)}
	chatPeer := &tg.InputNotifyPeer{Peer: &tg.InputPeerChat{ChatID: b.ID}}
	var userMute, chatMute tg.InputPeerNotifySettings
	userMute.SetMuteUntil(11)
	chatMute.SetMuteUntil(22)
	if updated, err := updateNotifySettings(a, f.ctx, userPeer, userMute); err != nil || !updated {
		t.Fatalf("set user peer settings: BoolTrue=%v err=%v", updated, err)
	}
	if updated, err := updateNotifySettings(a, f.ctx, chatPeer, chatMute); err != nil || !updated {
		t.Fatalf("set colliding chat peer settings: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsMute(t, f.ctx, a, userPeer, 11)
	assertNotifySettingsMute(t, f.ctx, a, chatPeer, 22)

	var negativeMute tg.InputPeerNotifySettings
	negativeMute.SetMuteUntil(-5)
	defaultUsers := &tg.InputNotifyUsers{}
	if updated, err := updateNotifySettings(a, f.ctx, defaultUsers, negativeMute); err != nil || !updated {
		t.Fatalf("set negative mute_until: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsMute(t, f.ctx, a, defaultUsers, 0)
	defaultBroadcasts := &tg.InputNotifyBroadcasts{}
	empty, err := getNotifySettings(a, f.ctx, defaultBroadcasts)
	if err != nil || !empty.Zero() {
		t.Fatalf("unset broadcasts settings = %+v err=%v, want empty", empty, err)
	}

	var storiesWithMute tg.InputPeerNotifySettings
	storiesWithMute.SetMuteUntil(5)
	storiesWithMute.SetStoriesMuted(true)
	storiesWithMute.SetStoriesHideSender(true)
	storiesWithMute.SetStoriesSound(&tg.NotificationSoundLocal{Title: "client sound", Data: "opaque"})
	if updated, err := updateNotifySettings(a, f.ctx, userPeer, storiesWithMute); err != nil || !updated {
		t.Fatalf("accept stories fields with a mute: BoolTrue=%v err=%v", updated, err)
	}
	got, err := getNotifySettings(a, f.ctx, userPeer)
	if err != nil {
		t.Fatalf("read stories-with-mute settings: %v", err)
	}
	if muteUntil, ok := got.GetMuteUntil(); !ok || muteUntil != 5 {
		t.Fatalf("stories-with-mute mute_until=(%d,%v), want (5,true)", muteUntil, ok)
	}
	if _, ok := got.GetStoriesMuted(); ok {
		t.Fatal("stories_muted was rendered, want ignored")
	}
	if _, ok := got.GetStoriesHideSender(); ok {
		t.Fatal("stories_hide_sender was rendered, want ignored")
	}
	if _, ok := got.GetStoriesIosSound(); ok {
		t.Fatal("stories sound was rendered, want ignored")
	}

	channel, err := f.store.CreateChannel(f.ctx, c.ID, "notify unsupported target", "", false)
	if err != nil {
		t.Fatalf("create unsupported-target channel: %v", err)
	}
	for name, peer := range map[string]tg.InputNotifyPeerClass{
		"forum topic": &tg.InputNotifyForumTopic{Peer: peerChannel(a.id, channel.ID), TopMsgID: 1},
		"community":   &tg.InputNotifyCommunity{Community: inputChannel(a.id, channel.ID)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := updateNotifySettings(a, f.ctx, peer, tg.InputPeerNotifySettings{}); expectTGError(err, "PEER_ID_INVALID") != nil {
				t.Fatalf("update unsupported target error = %v, want PEER_ID_INVALID", err)
			}
			if _, err := getNotifySettings(a, f.ctx, peer); expectTGError(err, "PEER_ID_INVALID") != nil {
				t.Fatalf("get unsupported target error = %v, want PEER_ID_INVALID", err)
			}
		})
	}

	for name, sound := range map[string]tg.NotificationSoundClass{
		"local":    &tg.NotificationSoundLocal{Title: "too long to store", Data: "arbitrary"},
		"ringtone": &tg.NotificationSoundRingtone{ID: 99},
	} {
		t.Run(name, func(t *testing.T) {
			var rejected tg.InputPeerNotifySettings
			rejected.SetMuteUntil(7)
			rejected.SetSound(sound)
			if _, err := updateNotifySettings(a, f.ctx, userPeer, rejected); expectTGError(err, "NOTIFY_SETTINGS_INVALID") != nil {
				t.Fatalf("update custom sound error = %v, want NOTIFY_SETTINGS_INVALID", err)
			}
			assertNotifySettingsMute(t, f.ctx, a, userPeer, 5)
		})
	}

	resetErr := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.AccountResetNotifySettings(ctx)
		return err
	})
	if resetErr == nil || !tgerr.Is(resetErr, "INPUT_METHOD_INVALID") {
		t.Fatalf("account.resetNotifySettings error = %v, want unimplemented method", resetErr)
	}
}

func TestNotifySettingsInvalidPeersDoNotWriteOrCharge(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC, phoneD = "+15551049301", "+15551049302", "+15551049303", "+15551049304"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC, phoneD)
	a := newSmokeClient(t, f, "notify settings invalid peer owner", phoneA)
	b := dialogPinUser(t, f, phoneB)
	c := dialogPinUser(t, f, phoneC)
	d := dialogPinUser(t, f, phoneD)
	seedDialogPinDM(t, f, a.id, b.ID, 1049301)
	visiblePeer := &tg.InputNotifyPeer{Peer: peerUser(a.id, b.ID)}
	var initial tg.InputPeerNotifySettings
	initial.SetMuteUntil(55)
	if updated, err := updateNotifySettings(a, f.ctx, visiblePeer, initial); err != nil || !updated {
		t.Fatalf("seed visible setting: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a.push.notifySettings, "seed notification settings push"), &tg.NotifyPeer{Peer: &tg.PeerUser{UserID: b.ID}}, 55)
	inaccessibleGroup, err := f.store.CreateChat(f.ctx, c.ID, "private notify settings group", []int64{d.ID})
	if err != nil {
		t.Fatalf("create inaccessible group: %v", err)
	}
	bannedChannel, err := f.store.CreateChannel(f.ctx, c.ID, "banned notify settings channel", "", false)
	if err != nil {
		t.Fatalf("create banned channel: %v", err)
	}
	invite, err := f.store.CreateChannelInvite(f.ctx, bannedChannel.ID, c.ID)
	if err != nil {
		t.Fatalf("create banned-channel invite: %v", err)
	}
	if _, _, err = f.store.JoinChannelByInvite(f.ctx, invite, a.id); err != nil {
		t.Fatalf("join channel before banning: %v", err)
	}
	if err = f.store.SetChannelBan(f.ctx, bannedChannel.ID, c.ID, a.id, nil, true); err != nil {
		t.Fatalf("ban owner from channel: %v", err)
	}
	publicChannel, err := f.store.CreateChannel(f.ctx, c.ID, "public notify settings channel", "", false)
	if err != nil {
		t.Fatalf("create public channel: %v", err)
	}
	if err = f.store.EditChannelUsername(f.ctx, publicChannel.ID, c.ID, "publicnotifysettings"); err != nil {
		t.Fatalf("make channel public: %v", err)
	}

	cases := []struct {
		name string
		peer tg.InputNotifyPeerClass
	}{
		{name: "unknown user", peer: &tg.InputNotifyPeer{Peer: peerUser(a.id, 9_999_999)}},
		{name: "user without dialog", peer: &tg.InputNotifyPeer{Peer: peerUser(a.id, d.ID)}},
		{name: "forged user hash", peer: &tg.InputNotifyPeer{Peer: &tg.InputPeerUser{UserID: b.ID, AccessHash: 1}}},
		{name: "non-member group", peer: &tg.InputNotifyPeer{Peer: &tg.InputPeerChat{ChatID: inaccessibleGroup.ID}}},
		{name: "unknown group", peer: &tg.InputNotifyPeer{Peer: &tg.InputPeerChat{ChatID: 9_999_998}}},
		{name: "banned channel", peer: &tg.InputNotifyPeer{Peer: peerChannel(a.id, bannedChannel.ID)}},
		{name: "public channel preview", peer: &tg.InputNotifyPeer{Peer: peerChannel(a.id, publicChannel.ID)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := updateNotifySettings(a, f.ctx, tc.peer, tg.InputPeerNotifySettings{}); expectTGError(err, "PEER_ID_INVALID") != nil {
				t.Fatalf("update error = %v, want PEER_ID_INVALID", err)
			}
			if _, err := getNotifySettings(a, f.ctx, tc.peer); expectTGError(err, "PEER_ID_INVALID") != nil {
				t.Fatalf("get error = %v, want PEER_ID_INVALID", err)
			}
		})
	}
	assertNotifySettingsMute(t, f.ctx, a, visiblePeer, 55)
	assertNoNotifySettingsPush(t, f.ctx, a)

	pool, err := pgxpool.New(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("open settings fixture pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	var tokenCount, settingsCount int64
	if err = pool.QueryRow(f.ctx, `SELECT token_count FROM rate_limits WHERE subject_id = $1 AND surface = 'notify_settings_mutation'`, a.id).Scan(&tokenCount); err != nil {
		t.Fatalf("read mutation budget after invalid peers: %v", err)
	}
	if err = pool.QueryRow(f.ctx, "SELECT count(*) FROM user_notify_settings WHERE owner_id = $1", a.id).Scan(&settingsCount); err != nil {
		t.Fatalf("count settings after invalid peers: %v", err)
	}
	if tokenCount != 1 || settingsCount != 1 {
		t.Fatalf("invalid peer attempts left tokens=%d settings=%d, want 1 and 1", tokenCount, settingsCount)
	}
}

func TestNotifySettingsPeerCapThroughRPC(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phone = "+15551049401"
	seedSmokeUsers(t, f, phone)
	a := newSmokeClient(t, f, "notify settings cap", phone)
	pool, err := pgxpool.New(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("open settings fixture pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	if _, err = pool.Exec(f.ctx, `
		INSERT INTO dialogs (owner_id, peer_type, peer_id, top_message)
		SELECT $1::bigint, 1, peer_id, 0 FROM generate_series(1000000, 1000999) AS peer_id
		UNION ALL SELECT $1::bigint, 1, 1001000, 0
	`, a.id); err != nil {
		t.Fatalf("seed cap dialogs: %v", err)
	}
	if _, err = pool.Exec(f.ctx, `
		INSERT INTO user_notify_settings (owner_id, scope, peer_type, peer_id, show_previews, changed_at)
		SELECT $1::bigint, 1, 1, peer_id, true, now() FROM generate_series(1000000, 1000999) AS peer_id
	`, a.id); err != nil {
		t.Fatalf("seed cap settings: %v", err)
	}
	newPeer := &tg.InputNotifyPeer{Peer: peerUser(a.id, 1001000)}
	var showFalse tg.InputPeerNotifySettings
	showFalse.SetShowPreviews(false)
	if _, err := updateNotifySettings(a, f.ctx, newPeer, showFalse); expectTGError(err, "NOTIFY_SETTINGS_TOO_MUCH") != nil {
		t.Fatalf("new exception at cap error = %v, want NOTIFY_SETTINGS_TOO_MUCH", err)
	}

	existing := &tg.InputNotifyPeer{Peer: peerUser(a.id, 1000000)}
	var updateExisting tg.InputPeerNotifySettings
	updateExisting.SetShowPreviews(false)
	if updated, err := updateNotifySettings(a, f.ctx, existing, updateExisting); err != nil || !updated {
		t.Fatalf("update exception at cap: BoolTrue=%v err=%v", updated, err)
	}
	if updated, err := updateNotifySettings(a, f.ctx, existing, tg.InputPeerNotifySettings{}); err != nil || !updated {
		t.Fatalf("clear exception at cap: BoolTrue=%v err=%v", updated, err)
	}
	cleared, err := getNotifySettings(a, f.ctx, existing)
	if err != nil || !cleared.Zero() {
		t.Fatalf("read cleared exception: settings=%+v err=%v", cleared, err)
	}
	if _, err = pool.Exec(f.ctx, "DELETE FROM dialogs WHERE owner_id = $1 AND peer_type = 1 AND peer_id = 1000001", a.id); err != nil {
		t.Fatalf("remove one peer dialog: %v", err)
	}
	if updated, err := updateNotifySettings(a, f.ctx, newPeer, showFalse); err != nil || !updated {
		t.Fatalf("new exception after prune: BoolTrue=%v err=%v", updated, err)
	}
}

func TestNotifySettingsMutationBudgetThroughRPC(t *testing.T) {
	t.Parallel()
	f := newSmokeFixtureWithDeadline(t, config.RegistrationClosed, nil, 4*time.Minute)
	const phone = "+15551049501"
	seedSmokeUsers(t, f, phone)
	a := newSmokeClient(t, f, "notify settings rate limit", phone)
	peer := &tg.InputNotifyChats{}
	for attempt := range 300 {
		if updated, err := updateNotifySettings(a, f.ctx, peer, tg.InputPeerNotifySettings{}); err != nil || !updated {
			t.Fatalf("settings write %d: BoolTrue=%v err=%v", attempt+1, updated, err)
		}
	}
	if _, err := updateNotifySettings(a, f.ctx, peer, tg.InputPeerNotifySettings{}); expectTGError(err, "FLOOD_WAIT_60") != nil {
		t.Fatalf("301st settings write error = %v, want FLOOD_WAIT_60", err)
	}
	assertNoNotifySettingsPush(t, f.ctx, a)
}

func TestNotifySettingsRejectUnboundRPCs(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	client := createClient(f.port, f.key, f.dcID, newUpdateCollector(), nil)
	err := client.Run(f.ctx, func(ctx context.Context) error {
		apiClient := client.API()
		if _, err := apiClient.AccountGetNotifySettings(ctx, &tg.InputNotifyUsers{}); !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("getNotifySettings error = %w, want AUTH_KEY_UNREGISTERED", err)
		}
		if _, err := apiClient.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
			Peer: &tg.InputNotifyUsers{},
		}); !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("updateNotifySettings error = %w, want AUTH_KEY_UNREGISTERED", err)
		}
		if _, err := apiClient.AccountGetNotifyExceptions(ctx, &tg.AccountGetNotifyExceptionsRequest{}); !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("getNotifyExceptions error = %w, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unbound notification settings requests: %v", err)
	}
}

func testSmokeNotifySettings(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithDeadline(t, config.RegistrationClosed, nil, 4*time.Minute)
	pool, err := pgxpool.New(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("open settings fixture pool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	const phoneA, phoneB = "+15551049101", "+15551049102"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "notify settings owner", phoneA)
	bClient := newSmokeClient(t, f, "notify settings group member", phoneB)
	b := dialogPinUser(t, f, phoneB)

	listener2 := mustListen(t, f.ctx, "127.0.0.1:0")
	stopReplica2 := bootServerWithDelivery(t, f.ctx, f.key, f.dcID, f.store, f.dsn, f.codes.Logger(), listener2)
	var stopReplica2Once sync.Once
	stopReplica2Safe := func() { stopReplica2Once.Do(stopReplica2) }
	t.Cleanup(stopReplica2Safe)
	port1 := f.port
	f.port = tcpPort(t, listener2)
	a2 := newSmokeClient(t, f, "notify settings owner second replica", phoneA)
	f.port = port1

	group, err := f.store.CreateChat(f.ctx, b.ID, "Notify settings group", []int64{a1.id})
	if err != nil {
		t.Fatalf("create visible group: %v", err)
	}
	if _, _, _, err = f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: group.ID, FromID: b.ID, Text: "seed notify settings dialog", RandomID: 1049101,
	}); err != nil {
		t.Fatalf("seed group dialog: %v", err)
	}
	var ptsBefore, eventsBefore int64
	if err = pool.QueryRow(f.ctx, "SELECT pts FROM update_state WHERE user_id = $1", a1.id).Scan(&ptsBefore); err != nil {
		t.Fatalf("read owner pts before settings changes: %v", err)
	}
	if err = pool.QueryRow(f.ctx, "SELECT count(*) FROM message_events WHERE owner_id = $1", a1.id).Scan(&eventsBefore); err != nil {
		t.Fatalf("count owner events before settings changes: %v", err)
	}
	peer := &tg.InputNotifyPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}}
	groupPeer := &tg.PeerChat{ChatID: group.ID}

	var mute tg.InputPeerNotifySettings
	mute.SetMuteUntil(2147483647)
	if updated, err := updateNotifySettings(a1, f.ctx, peer, mute); err != nil || !updated {
		t.Fatalf("mute group: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a1.push.notifySettings, "first-session notification settings push"), &tg.NotifyPeer{Peer: groupPeer}, 2147483647)
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a2.push.notifySettings, "cross-replica notification settings push"), &tg.NotifyPeer{Peer: groupPeer}, 2147483647)
	assertNotifySettingsMute(t, f.ctx, a1, peer, 2147483647)
	assertNotifySettingsMute(t, f.ctx, a2, peer, 2147483647)

	bSettings, err := getNotifySettings(bClient, f.ctx, peer)
	if err != nil {
		t.Fatalf("other group member getNotifySettings: %v", err)
	}
	if !bSettings.Zero() {
		t.Fatalf("other group member settings = %+v, want empty", bSettings)
	}
	assertNoNotifySettingsPush(t, f.ctx, bClient)

	dialogs, err := getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs with stored settings: %v", err)
	}
	assertDialogNotifySettingsEmpty(t, dialogs, groupPeer)
	full, err := callFullChat(a2, f.ctx, group.ID)
	if err != nil {
		t.Fatalf("getFullChat with stored settings: %v", err)
	}
	chatFull, ok := full.FullChat.(*tg.ChatFull)
	if !ok || !chatFull.NotifySettings.Zero() {
		t.Fatalf("getFullChat notify settings = %#v, want empty ChatFull settings", full.FullChat)
	}

	if updated, err := updateNotifySettings(a1, f.ctx, peer, tg.InputPeerNotifySettings{}); err != nil || !updated {
		t.Fatalf("clear group settings: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsUpdateEmpty(t, recvOrCtx(t, f.ctx, a1.push.notifySettings, "first-session notification settings clear"), &tg.NotifyPeer{Peer: groupPeer})
	assertNotifySettingsUpdateEmpty(t, recvOrCtx(t, f.ctx, a2.push.notifySettings, "cross-replica notification settings clear"), &tg.NotifyPeer{Peer: groupPeer})
	assertNoNotifySettingsPush(t, f.ctx, bClient)
	bSettings, err = getNotifySettings(bClient, f.ctx, peer)
	if err != nil || !bSettings.Zero() {
		t.Fatalf("other group member after owner's clear: settings=%+v err=%v", bSettings, err)
	}

	if updated, err := updateNotifySettings(a1, f.ctx, peer, mute); err != nil || !updated {
		t.Fatalf("restore group settings before restart: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a1.push.notifySettings, "restored first-session settings"), &tg.NotifyPeer{Peer: groupPeer}, 2147483647)
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a2.push.notifySettings, "restored cross-replica settings"), &tg.NotifyPeer{Peer: groupPeer}, 2147483647)

	var defaultMute tg.InputPeerNotifySettings
	defaultMute.SetMuteUntil(42)
	defaultPeer := &tg.InputNotifyChats{}
	if updated, err := updateNotifySettings(a1, f.ctx, defaultPeer, defaultMute); err != nil || !updated {
		t.Fatalf("set chats default settings: BoolTrue=%v err=%v", updated, err)
	}
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a1.push.notifySettings, "first-session default settings push"), &tg.NotifyChats{}, 42)
	assertNotifySettingsUpdate(t, recvOrCtx(t, f.ctx, a2.push.notifySettings, "cross-replica default settings push"), &tg.NotifyChats{}, 42)
	assertNotifySettingsMute(t, f.ctx, a1, defaultPeer, 42)
	assertNotifySettingsMute(t, f.ctx, a2, defaultPeer, 42)
	var ptsAfter, eventsAfter int64
	if err = pool.QueryRow(f.ctx, "SELECT pts FROM update_state WHERE user_id = $1", a1.id).Scan(&ptsAfter); err != nil {
		t.Fatalf("read owner pts after settings changes: %v", err)
	}
	if err = pool.QueryRow(f.ctx, "SELECT count(*) FROM message_events WHERE owner_id = $1", a1.id).Scan(&eventsAfter); err != nil {
		t.Fatalf("count owner events after settings changes: %v", err)
	}
	if ptsAfter != ptsBefore || eventsAfter != eventsBefore {
		t.Fatalf("settings mutations changed update state: pts %d->%d, events %d->%d", ptsBefore, ptsAfter, eventsBefore, eventsAfter)
	}
	bSettings, err = getNotifySettings(bClient, f.ctx, defaultPeer)
	if err != nil || !bSettings.Zero() {
		t.Fatalf("other account default settings = %+v err=%v, want empty", bSettings, err)
	}
	assertNoNotifySettingsPush(t, f.ctx, bClient)

	a1Session, a1ID := a1.session, a1.id
	a1.stopClient(t)
	a2.stopClient(t)
	bClient.stopClient(t)
	stopReplica2Safe()
	f.restart(t)

	client := f.savedSessionClient(a1Session)
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized || status.User == nil || status.User.ID != a1ID {
			return fmt.Errorf("restarted session user = %+v, want %d", status, a1ID)
		}
		settings, err := client.API().AccountGetNotifySettings(ctx, peer)
		if err != nil {
			return err
		}
		if value, ok := settings.GetMuteUntil(); !ok || value != 2147483647 {
			return fmt.Errorf("restarted peer mute_until=(%d,%v), want (2147483647,true)", value, ok)
		}
		settings, err = client.API().AccountGetNotifySettings(ctx, defaultPeer)
		if err != nil {
			return err
		}
		if value, ok := settings.GetMuteUntil(); !ok || value != 42 {
			return fmt.Errorf("restarted default mute_until=(%d,%v), want (42,true)", value, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("read persisted settings after restart: %v", err)
	}
}

func updateNotifySettings(c *smokeClient, ctx context.Context, peer tg.InputNotifyPeerClass, settings tg.InputPeerNotifySettings) (bool, error) {
	var result bool
	err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		result, err = client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{Peer: peer, Settings: settings})
		return err
	})
	return result, err
}

func getNotifySettings(c *smokeClient, ctx context.Context, peer tg.InputNotifyPeerClass) (*tg.PeerNotifySettings, error) {
	var result *tg.PeerNotifySettings
	err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		result, err = client.AccountGetNotifySettings(ctx, peer)
		return err
	})
	return result, err
}

func assertNotifySettingsMute(t *testing.T, ctx context.Context, client *smokeClient, peer tg.InputNotifyPeerClass, want int) {
	t.Helper()
	settings, err := getNotifySettings(client, ctx, peer)
	if err != nil {
		t.Fatalf("getNotifySettings: %v", err)
	}
	if muteUntil, ok := settings.GetMuteUntil(); !ok || muteUntil != want {
		t.Fatalf("getNotifySettings mute_until=(%d,%v), want (%d,true)", muteUntil, ok, want)
	}
}

func assertNotifySettingsUpdate(t *testing.T, update *tg.UpdateNotifySettings, peer tg.NotifyPeerClass, wantMute int) {
	t.Helper()
	if update == nil || update.Peer == nil || update.Peer.TypeID() != peer.TypeID() || update.Peer.String() != peer.String() {
		t.Fatalf("updateNotifySettings peer = %#v, want %s", update, peer)
	}
	if muteUntil, ok := update.NotifySettings.GetMuteUntil(); !ok || muteUntil != wantMute {
		t.Fatalf("updateNotifySettings mute_until=(%d,%v), want (%d,true)", muteUntil, ok, wantMute)
	}
}

func assertNotifySettingsUpdateEmpty(t *testing.T, update *tg.UpdateNotifySettings, peer tg.NotifyPeerClass) {
	t.Helper()
	if update == nil || update.Peer == nil || update.Peer.TypeID() != peer.TypeID() || update.Peer.String() != peer.String() {
		t.Fatalf("clear update peer = %#v, want %s", update, peer)
	}
	if !update.NotifySettings.Zero() {
		t.Fatalf("clear update settings = %+v, want empty", update.NotifySettings)
	}
}

func assertNoNotifySettingsPush(t *testing.T, ctx context.Context, client *smokeClient) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case update := <-client.push.notifySettings:
			t.Fatalf("notification settings leaked to account %d: %+v", client.id, update)
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting for notification settings isolation check: %v", ctx.Err())
		}
	}
}

func callFullChat(c *smokeClient, ctx context.Context, chatID int64) (*tg.MessagesChatFull, error) {
	var result *tg.MessagesChatFull
	err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		result, err = client.MessagesGetFullChat(ctx, chatID)
		return err
	})
	return result, err
}

func assertDialogNotifySettingsEmpty(t *testing.T, result tg.MessagesDialogsClass, peer tg.PeerClass) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		if dialog, ok := raw.(*tg.Dialog); ok && sameUnreadMarkPeer(dialog.Peer, peer) {
			if !dialog.NotifySettings.Zero() {
				t.Fatalf("getDialogs notify settings = %+v, want empty", dialog.NotifySettings)
			}
			return
		}
	}
	t.Fatalf("getDialogs omitted peer %s", peer)
}
