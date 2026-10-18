package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestChannelProfilePhotoSearchAcceptsActiveMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator := mustUser(t, s, "+15551298101")
	member := mustUser(t, s, "+15551298102")
	banned := mustUser(t, s, "+15551298103")
	leaver := mustUser(t, s, "+15551298104")
	outsider := mustUser(t, s, "+15551298105")

	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{
		Broadcast: true,
		Title:     "Profile search",
	})
	if _, err := api.InviteToChannelForTest(s, creator.ID, &tg.ChannelsInviteToChannelRequest{
		Channel: &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
		Users:   []tg.InputUserClass{api.InputUser(creator.ID, member.ID), api.InputUser(creator.ID, banned.ID), api.InputUser(creator.ID, leaver.ID)},
	}); err != nil {
		t.Fatalf("invite channel members: %v", err)
	}
	memberDialogs, err := api.GetDialogsForTest(s, member.ID)
	if err != nil {
		t.Fatalf("messages.getDialogs for active member: %v", err)
	}
	dialogs, ok := memberDialogs.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("messages.getDialogs result = %T, want *tg.MessagesDialogs", memberDialogs)
	}
	var memberChannel *tg.Channel
	for _, chat := range dialogs.Chats {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == channel.ID {
			memberChannel = ch
			break
		}
	}
	if memberChannel == nil {
		t.Fatal("messages.getDialogs did not return the invited channel for the active member")
	}
	if err := s.SetChannelBan(ctx, channel.ID, creator.ID, banned.ID, nil, true); err != nil {
		t.Fatalf("ban channel member: %v", err)
	}
	if left, err := s.LeaveChannel(ctx, channel.ID, leaver.ID); err != nil || !left {
		t.Fatalf("leave channel: left=%v err=%v", left, err)
	}
	if _, err := sendToChannel(t, s, creator.ID, channel.ID, "channel post", 98101); err != nil {
		t.Fatalf("send channel post: %v", err)
	}

	creatorPeer := &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}
	memberPeer := &tg.InputPeerChannel{ChannelID: memberChannel.ID, AccessHash: memberChannel.AccessHash}
	peers := []struct {
		name   string
		userID int64
		peer   tg.InputPeerClass
	}{
		{name: "creator", userID: creator.ID, peer: creatorPeer},
		{name: "active member", userID: member.ID, peer: memberPeer},
	}
	for _, tc := range peers {
		t.Run(tc.name, func(t *testing.T) {
			history, err := api.GetHistoryForTest(s, tc.userID, &tg.MessagesGetHistoryRequest{Peer: tc.peer, Limit: 20})
			if err != nil {
				t.Fatalf("messages.getHistory: %v", err)
			}
			if !containsChannelPost(t, history, "channel post") {
				t.Fatal("messages.getHistory did not return the channel post")
			}

			search, err := api.SearchForTest(s, tc.userID, &tg.MessagesSearchRequest{
				Peer: tc.peer, Filter: &tg.InputMessagesFilterChatPhotos{}, Limit: 20,
			})
			if err != nil {
				t.Fatalf("messages.search with inputMessagesFilterChatPhotos: %v", err)
			}
			result := channelMediaSearchResult(t, search)
			if result.Count != 0 || len(result.Messages) != 0 {
				t.Fatalf("profile-photo search count/messages = %d/%d, want 0/0 for a channel with no profile-photo service messages", result.Count, len(result.Messages))
			}
		})
	}

	for _, tc := range []struct {
		name   string
		userID int64
	}{
		{name: "banned", userID: banned.ID},
		{name: "left", userID: leaver.ID},
		{name: "non-member", userID: outsider.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := api.SearchForTest(s, tc.userID, &tg.MessagesSearchRequest{
				Peer: api.InputPeerChannel(tc.userID, channel.ID), Filter: &tg.InputMessagesFilterChatPhotos{}, Limit: 20,
			})
			if got := rpcMessage(t, err); got != "PEER_ID_INVALID" {
				t.Fatalf("messages.search error = %s, want PEER_ID_INVALID", got)
			}
		})
	}

	_, err = api.SearchForTest(s, member.ID, &tg.MessagesSearchRequest{
		Peer:   &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
		Filter: &tg.InputMessagesFilterChatPhotos{}, Limit: 20,
	})
	if got := rpcMessage(t, err); got != "PEER_ID_INVALID" {
		t.Fatalf("messages.search with creator's hash error = %s, want PEER_ID_INVALID", got)
	}
}

func containsChannelPost(t *testing.T, enc bin.Encoder, text string) bool {
	t.Helper()
	result := channelMediaSearchResult(t, enc)
	for _, item := range result.Messages {
		message, ok := item.(*tg.Message)
		if ok && message.Message == text {
			return true
		}
	}
	return false
}
