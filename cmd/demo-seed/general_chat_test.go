package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/demo"
)

func TestSeedUpgradeCreatesMissingGeneralChatIdempotently(t *testing.T) {
	rpc := &generalChatRPC{}
	api := tg.NewClient(rpc)
	if err := verifyGeneralChat(context.Background(), api, demoGeneralChatSpace); err == nil {
		t.Fatal("verify General before upgrade succeeded with no General chat")
	}
	for range 2 {
		if _, err := ensureGeneralChat(context.Background(), api, demoGeneralChatSpace); err != nil {
			t.Fatalf("ensure General chat: %v", err)
		}
		if err := inviteGeneralChatAccounts(context.Background(), api, remoteSpace{
			spec: demoGeneralChatSpace, channelID: 71, accessHash: 93, creator: true,
		}); err != nil {
			t.Fatalf("invite seeded guests to General: %v", err)
		}
	}
	if rpc.createCount != 1 || rpc.usernameCount != 1 {
		t.Fatalf("General chat creates/usernames = %d/%d, want one each", rpc.createCount, rpc.usernameCount)
	}
	if rpc.inviteCount != len(seedAccounts)-1 {
		t.Fatalf("General seeded guest invitations = %d, want %d", rpc.inviteCount, len(seedAccounts)-1)
	}
	if rpc.slowmodeSeconds != demoGeneralChatSlowmodeSeconds || rpc.slowmodeCount != 1 {
		t.Fatalf("General slow mode = %d after %d updates, want one update to %d", rpc.slowmodeSeconds, rpc.slowmodeCount, demoGeneralChatSlowmodeSeconds)
	}
	wantRights := tg.ChatBannedRights{
		ChangeInfo:      true,
		InviteUsers:     true,
		PinMessages:     true,
		ManageTopics:    true,
		SendPolls:       true,
		SendGames:       true,
		SendInline:      true,
		SendGifs:        true,
		SendVideos:      true,
		SendRoundvideos: true,
		SendAudios:      true,
		SendVoices:      true,
		SendDocs:        true,
	}
	wantRights.SetFlags()
	if rpc.defaultBannedRights != wantRights || rpc.defaultBannedRightsWriteCount != 1 {
		t.Fatalf("General default banned rights = %+v after %d writes, want %+v after one write", rpc.defaultBannedRights, rpc.defaultBannedRightsWriteCount, wantRights)
	}
	if err := verifyGeneralChat(context.Background(), api, demoGeneralChatSpace); err != nil {
		t.Fatalf("verify General chat: %v", err)
	}
	rpc.banned = map[int64]bool{}
	rpc.banned[101] = true
	delete(rpc.invited, 101)
	if err := inviteGeneralChatAccounts(context.Background(), api, remoteSpace{
		spec: demoGeneralChatSpace, channelID: 71, accessHash: 93, creator: true,
	}); err != nil {
		t.Fatalf("preserve banned seeded guest during invite convergence: %v", err)
	}
	if rpc.invited[101] || !rpc.banned[101] {
		t.Fatalf("banned seeded guest state invited=%v banned=%v, want uninvited and still banned", rpc.invited[101], rpc.banned[101])
	}
	if err := verifyGeneralChat(context.Background(), api, demoGeneralChatSpace); err != nil {
		t.Fatalf("verify General chat with a banned seeded guest: %v", err)
	}
}

func TestSeedUpgradeUsesReservedFallbackWhenGeneralHandleCollides(t *testing.T) {
	rpc := &generalChatRPC{collidingHandles: map[string]bool{demo.GeneralChatHandle: true}}
	api := tg.NewClient(rpc)
	for range 2 {
		remote, err := ensureGeneralChat(context.Background(), api, demoGeneralChatSpace)
		if err != nil {
			t.Fatalf("ensure General after handle collision: %v", err)
		}
		if remote.spec.username != "teagramdemogeneralchat" || !remote.creator {
			t.Fatalf("resolved General after handle collision = %+v, want seed-owned fallback handle", remote)
		}
	}
	if rpc.createCount != 1 || rpc.createdUsername != "teagramdemogeneralchat" {
		t.Fatalf("fallback General creates=%d handle=%q, want one create on fallback", rpc.createCount, rpc.createdUsername)
	}
	if err := inviteGeneralChatAccounts(context.Background(), api, remoteSpace{
		spec:      spaceSpec{slug: "general", username: rpc.createdUsername, title: "General", megagroup: true},
		channelID: 71, accessHash: 93, creator: true,
	}); err != nil {
		t.Fatalf("invite guests to fallback General: %v", err)
	}
	if err := verifyGeneralChat(context.Background(), api, demoGeneralChatSpace); err != nil {
		t.Fatalf("verify fallback General: %v", err)
	}
}

func TestInviteGeneralChatAccountsRequiresSeedOwner(t *testing.T) {
	rpc := &generalChatRPC{}
	if err := inviteGeneralChatAccounts(context.Background(), tg.NewClient(rpc), remoteSpace{
		spec: demoGeneralChatSpace, channelID: 71, accessHash: 93,
	}); err == nil {
		t.Fatal("inviteGeneralChatAccounts accepted a General channel without seed-owner proof")
	}
	if rpc.inviteCount != 0 {
		t.Fatalf("foreign General invitation count = %d, want none", rpc.inviteCount)
	}
}

type generalChatRPC struct {
	created                       bool
	createdUsername               string
	collidingHandles              map[string]bool
	createCount                   int
	usernameCount                 int
	invited                       map[int64]bool
	banned                        map[int64]bool
	inviteCount                   int
	slowmodeSeconds               int
	slowmodeCount                 int
	defaultBannedRights           tg.ChatBannedRights
	defaultBannedRightsWriteCount int
}

func (rpc *generalChatRPC) Invoke(_ context.Context, input bin.Encoder, output bin.Decoder) error {
	switch request := input.(type) {
	case *tg.ContactsResolveUsernameRequest:
		if rpc.collidingHandles[request.Username] {
			resolved, ok := output.(*tg.ContactsResolvedPeer)
			if !ok {
				return fmt.Errorf("unexpected collision resolution output %T", output)
			}
			resolved.Chats = []tg.ChatClass{&tg.Channel{
				ID: 13, AccessHash: 29, Username: request.Username, Title: "Unrelated public channel", Broadcast: true,
			}}
			return nil
		}
		if !rpc.created {
			return tgerr.New(400, "USERNAME_NOT_OCCUPIED")
		}
		resolved, ok := output.(*tg.ContactsResolvedPeer)
		if !ok {
			return fmt.Errorf("unexpected resolved-peer output %T", output)
		}
		resolved.Chats = []tg.ChatClass{&tg.Channel{
			ID:         71,
			AccessHash: 93,
			Username:   request.Username,
			Title:      demoGeneralChatSpace.title,
			Megagroup:  true,
		}}
	case *tg.ChannelsCreateChannelRequest:
		if request.Title != demoGeneralChatSpace.title || !request.Megagroup || request.Broadcast {
			return fmt.Errorf("unexpected General create request: %+v", request)
		}
		rpc.created = true
		rpc.createCount++
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected create output %T", output)
		}
		updates.Updates = &tg.Updates{Chats: []tg.ChatClass{&tg.Channel{
			ID:         71,
			AccessHash: 93,
			Title:      request.Title,
			Megagroup:  true,
		}}}
	case *tg.ChannelsUpdateUsernameRequest:
		rpc.usernameCount++
		rpc.createdUsername = request.Username
		result, ok := output.(*tg.BoolBox)
		if !ok {
			return fmt.Errorf("unexpected update-username output %T", output)
		}
		result.Bool = &tg.BoolTrue{}
	case *tg.ContactsSearchRequest:
		found, ok := output.(*tg.ContactsFound)
		if !ok {
			return fmt.Errorf("unexpected contacts-search output %T", output)
		}
		for i, account := range seedAccounts {
			if account.username == request.Q {
				found.Users = []tg.UserClass{&tg.User{ID: int64(i + 100), AccessHash: int64(i + 200), Username: account.username}}
				return nil
			}
		}
		return fmt.Errorf("unexpected seeded guest search %q", request.Q)
	case *tg.ChannelsInviteToChannelRequest:
		channel, ok := request.Channel.(*tg.InputChannel)
		if !ok || channel.ChannelID != 71 || len(request.Users) != 1 {
			return fmt.Errorf("unexpected General invitation: %+v", request)
		}
		user, ok := request.Users[0].(*tg.InputUser)
		if !ok {
			return fmt.Errorf("unexpected invited user %T", request.Users[0])
		}
		if rpc.invited == nil {
			rpc.invited = map[int64]bool{}
		}
		if rpc.invited[user.UserID] {
			return tgerr.New(400, "USER_ALREADY_PARTICIPANT")
		}
		if rpc.banned[user.UserID] {
			return tgerr.New(400, "USER_BANNED_IN_CHANNEL")
		}
		rpc.invited[user.UserID] = true
		rpc.inviteCount++
		invited, ok := output.(*tg.MessagesInvitedUsers)
		if !ok {
			return fmt.Errorf("unexpected invite output %T", output)
		}
		invited.Updates = &tg.Updates{}
	case *tg.ChannelsGetParticipantRequest:
		participant, ok := output.(*tg.ChannelsChannelParticipant)
		if !ok {
			return fmt.Errorf("unexpected participant output %T", output)
		}
		switch target := request.Participant.(type) {
		case *tg.InputPeerSelf:
			participant.Participant = &tg.ChannelParticipantCreator{}
		case *tg.InputPeerUser:
			switch {
			case rpc.banned[target.UserID]:
				participant.Participant = &tg.ChannelParticipantBanned{Peer: &tg.PeerUser{UserID: target.UserID}}
			case !rpc.invited[target.UserID]:
				return tgerr.New(400, "USER_NOT_PARTICIPANT")
			default:
				participant.Participant = &tg.ChannelParticipant{UserID: target.UserID}
			}
		default:
			return fmt.Errorf("unexpected General participant target %T", request.Participant)
		}
	case *tg.ChannelsGetFullChannelRequest:
		full, ok := output.(*tg.MessagesChatFull)
		if !ok {
			return fmt.Errorf("unexpected full-channel output %T", output)
		}
		channel := &tg.ChannelFull{}
		if rpc.slowmodeSeconds > 0 {
			channel.SetSlowmodeSeconds(rpc.slowmodeSeconds)
		}
		full.FullChat = channel
	case *tg.MessagesEditChatDefaultBannedRightsRequest:
		peer, ok := request.Peer.(*tg.InputPeerChannel)
		if !ok || peer.ChannelID != 71 {
			return fmt.Errorf("unexpected General default rights peer: %+v", request.Peer)
		}
		if rpc.defaultBannedRights == request.BannedRights {
			return tgerr.New(400, "CHAT_NOT_MODIFIED")
		}
		rpc.defaultBannedRights = request.BannedRights
		rpc.defaultBannedRightsWriteCount++
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected default-rights output %T", output)
		}
		updates.Updates = &tg.Updates{}
	case *tg.ChannelsGetChannelsRequest:
		if len(request.ID) != 1 {
			return fmt.Errorf("General channel request count = %d, want one", len(request.ID))
		}
		channelID, ok := request.ID[0].(*tg.InputChannel)
		if !ok || channelID.ChannelID != 71 {
			return fmt.Errorf("unexpected General channel rights query: %+v", request.ID[0])
		}
		chats, ok := output.(*tg.MessagesChatsBox)
		if !ok {
			return fmt.Errorf("unexpected channel-rights output %T", output)
		}
		username := rpc.createdUsername
		if username == "" {
			username = demoGeneralChatUsername
		}
		channel := &tg.Channel{ID: 71, AccessHash: 93, Username: username, Title: demoGeneralChatSpace.title, Megagroup: true}
		channel.SetDefaultBannedRights(rpc.defaultBannedRights)
		chats.Chats = &tg.MessagesChats{Chats: []tg.ChatClass{channel}}
	case *tg.ChannelsToggleSlowModeRequest:
		if request.Seconds != demoGeneralChatSlowmodeSeconds {
			return fmt.Errorf("slow mode request = %d, want %d", request.Seconds, demoGeneralChatSlowmodeSeconds)
		}
		rpc.slowmodeSeconds = request.Seconds
		rpc.slowmodeCount++
		updates, ok := output.(*tg.UpdatesBox)
		if !ok {
			return fmt.Errorf("unexpected toggle-slowmode output %T", output)
		}
		updates.Updates = &tg.Updates{}
	default:
		return fmt.Errorf("unexpected General chat request %T", input)
	}
	return nil
}
