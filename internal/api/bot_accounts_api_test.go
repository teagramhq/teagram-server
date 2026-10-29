package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func createTestBot(t *testing.T, s *store.Store, username, firstName string) store.BotAccount {
	t.Helper()
	bot, _, err := s.CreateBot(context.Background(), username, firstName)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	return bot
}

func requireBotGroupsBlocked(t *testing.T, err error) {
	t.Helper()
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Code != 400 || rpc.Message != "BOT_GROUPS_BLOCKED" {
		t.Fatalf("bot invite error = %v, want 400 BOT_GROUPS_BLOCKED", err)
	}
}

func TestGetFullUserShowsBotProfileAndOnlyBotInfo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, err := s.CreateUser(ctx, "+15551910001")
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "weather_profile_bot", "Weather")

	full := getFullUserForTest(t, s, viewer.ID, api.InputUser(viewer.ID, bot.ID))
	user := requireFullUserTarget(t, full, bot.ID)
	if user.FirstName != "Weather" || user.Username != "weather_profile_bot" || !user.Bot || !user.BotNochats {
		t.Fatalf("bot user = %+v, want public profile with bot and no-chats flags", user)
	}
	if version, ok := user.GetBotInfoVersion(); !ok || version == 0 {
		t.Fatalf("bot_info_version = (%d, %v), want a nonzero server value", version, ok)
	}
	info, ok := full.FullUser.GetBotInfo()
	if !ok {
		t.Fatal("full user has no bot_info")
	}
	if !info.Zero() {
		t.Fatalf("bot_info = %+v, want minimal empty info without token metadata", info)
	}
	for field, present := range fullUserOptionalFieldsPresent(full.FullUser) {
		want := field == "bot_info" || field == "ttl_period"
		if present != want {
			t.Errorf("full user optional field %q present=%v, want %v", field, present, want)
		}
	}
	assertEncodes(t, full)
}

func TestGetUsersShowsBotsAndLeavesHumanFlagsUnset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, err := s.CreateUser(ctx, "+15551910002")
	if err != nil {
		t.Fatal(err)
	}
	human, err := s.CreateUser(ctx, "+15551910003")
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "forecast_profile_bot", "Forecast")

	users := getUsersForTest(t, s, viewer.ID,
		api.InputUser(viewer.ID, human.ID),
		api.InputUser(viewer.ID, bot.ID),
	)
	if len(users.Elems) != 2 {
		t.Fatalf("users.getUsers returned %d users, want 2", len(users.Elems))
	}
	plain, ok := users.Elems[0].(*tg.User)
	if !ok {
		t.Fatalf("human user = %T, want *tg.User", users.Elems[0])
	}
	if plain.Bot || plain.BotNochats {
		t.Errorf("human user carries bot flags: %+v", plain)
	}
	if version, present := plain.GetBotInfoVersion(); present || version != 0 {
		t.Errorf("human bot_info_version = (%d, %v), want absent", version, present)
	}
	botUser, ok := users.Elems[1].(*tg.User)
	if !ok {
		t.Fatalf("bot user = %T, want *tg.User", users.Elems[1])
	}
	if !botUser.Bot || !botUser.BotNochats {
		t.Errorf("bot user flags = bot:%v nochats:%v, want true/true", botUser.Bot, botUser.BotNochats)
	}
	if version, present := botUser.GetBotInfoVersion(); !present || version == 0 {
		t.Errorf("bot bot_info_version = (%d, %v), want a nonzero server value", version, present)
	}

	resolved, err := api.ResolveUsernameForTest(s, viewer.ID, &tg.ContactsResolveUsernameRequest{Username: "@forecast_profile_bot"})
	if err != nil {
		t.Fatalf("contacts.resolveUsername bot: %v", err)
	}
	peer, ok := resolved.(*tg.ContactsResolvedPeer)
	if !ok || len(peer.Users) != 1 {
		t.Fatalf("contacts.resolveUsername response = %T/%v, want one resolved user", resolved, resolved)
	}
	resolvedBot, ok := peer.Users[0].(*tg.User)
	if !ok || !resolvedBot.Bot || !resolvedBot.BotNochats {
		t.Fatalf("resolved bot user = %T/%v, want bot and no-chats flags", peer.Users[0], peer.Users[0])
	}
}

func TestContactsSearchShowsBotsAsBots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, err := s.CreateUser(ctx, "+15551910008")
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "search_profile_bot", "Forecast")
	if _, err := api.SendMessageForTest(s, viewer.ID, &tg.MessagesSendMessageRequest{
		Peer:     api.InputPeerUser(viewer.ID, bot.ID),
		Message:  "hello",
		RandomID: 1,
	}); err != nil {
		t.Fatalf("open dialog with bot: %v", err)
	}

	res, err := api.ContactsSearchForTest(s, viewer.ID, &tg.ContactsSearchRequest{Q: "forecast", Limit: 10})
	if err != nil {
		t.Fatalf("contacts.search bot: %v", err)
	}
	found, ok := res.(*tg.ContactsFound)
	if !ok {
		t.Fatalf("contacts.search response = %T, want *tg.ContactsFound", res)
	}
	if len(found.Users) != 1 {
		t.Fatalf("contacts.search users = %d, want the bot", len(found.Users))
	}
	user, ok := found.Users[0].(*tg.User)
	if !ok || user.ID != bot.ID || !user.Bot || !user.BotNochats {
		t.Fatalf("contacts.search user = %T/%v, want bot and no-chats flags", found.Users[0], found.Users[0])
	}
}

func TestCreateChatRejectsBotWithoutCreatingMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551910004")
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "chat_invite_bot", "Chat invite")

	_, err = api.CreateChatForTest(s, creator.ID, &tg.MessagesCreateChatRequest{
		Title: "No bots",
		Users: []tg.InputUserClass{api.InputUser(creator.ID, bot.ID)},
	})
	requireBotGroupsBlocked(t, err)
	chats, err := s.ChatsForUser(ctx, creator.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chats) != 0 {
		t.Fatalf("chats after refused bot invite = %d, want none", len(chats))
	}
}

func TestAddChatUserRejectsBotWithoutAddingMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551910005")
	if err != nil {
		t.Fatal(err)
	}
	member, err := s.CreateUser(ctx, "+15551910006")
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "add_invite_bot", "Add invite")
	chat, err := s.CreateChat(ctx, creator.ID, "No bots", []int64{member.ID})
	if err != nil {
		t.Fatal(err)
	}
	before := apiParticipants(t, s, chat.ID)

	_, err = api.AddChatUserForTest(s, creator.ID, &tg.MessagesAddChatUserRequest{
		ChatID: chat.ID,
		UserID: api.InputUser(creator.ID, bot.ID),
	})
	requireBotGroupsBlocked(t, err)
	if got := apiParticipants(t, s, chat.ID); len(got) != len(before) {
		t.Fatalf("participants after refused bot invite = %v, want unchanged %v", got, before)
	}
}

func TestInviteToChannelRejectsBotWithoutAddingMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551910007")
	if err != nil {
		t.Fatal(err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "No bots", "", false)
	if err != nil {
		t.Fatal(err)
	}
	bot := createTestBot(t, s, "channel_invite_bot", "Channel invite")

	_, err = api.InviteToChannelForTest(s, creator.ID, &tg.ChannelsInviteToChannelRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		Users:   []tg.InputUserClass{api.InputUser(creator.ID, bot.ID)},
	})
	requireBotGroupsBlocked(t, err)
	if _, found, err := s.ChannelMemberOf(ctx, channel.ID, bot.ID); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("bot has a channel membership after the refused invite")
	}
}
