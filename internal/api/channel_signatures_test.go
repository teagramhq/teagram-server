package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestBroadcastPostsHideAuthorOnEveryReadSurface(t *testing.T) {
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551822470")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	author, err := s.CreateUser(ctx, "+15551822471")
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	subscriber, err := s.CreateUser(ctx, "+15551822472")
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	destination, err := s.CreateUser(ctx, "+15551822473")
	if err != nil {
		t.Fatalf("create forward destination: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Anonymous posts", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, author.ID)
	if err := s.SetChannelRole(ctx, channel.ID, creator.ID, author.ID, 1); err != nil {
		t.Fatalf("promote broadcast poster: %v", err)
	}
	member := joinChannelByInvite(t, s, channel, subscriber.ID)
	post, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, author.ID, "broadcast visibility proof", 224701, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post to broadcast channel: duplicate=%v err=%v", duplicate, err)
	}

	assertSubscriberView := func(t *testing.T, messages []tg.MessageClass, users []tg.UserClass) {
		t.Helper()
		var found *tg.Message
		for _, message := range messages {
			candidate, ok := message.(*tg.Message)
			if ok && candidate.ID == int(post.LocalID) {
				found = candidate
				break
			}
		}
		if found == nil {
			t.Fatalf("post %d missing from response: %T", post.LocalID, messages)
		}
		assertEncodes(t, found)
		if !found.GetPost() {
			t.Fatal("broadcast post has no post flag")
		}
		if _, ok := found.GetFromID(); ok {
			t.Fatal("broadcast post exposes from_id")
		}
		if found.Out {
			t.Fatal("subscriber sees the broadcast post as outgoing")
		}
		for _, user := range users {
			if user.GetID() == author.ID {
				t.Fatalf("users vector contains broadcast author %d", author.ID)
			}
		}
	}

	t.Run("getHistory", func(t *testing.T) {
		enc, err := api.GetHistoryForTest(s, subscriber.ID, &tg.MessagesGetHistoryRequest{
			Peer: channelPeer(subscriber.ID, channel.ID), Limit: 10,
		})
		if err != nil {
			t.Fatalf("getHistory: %v", err)
		}
		result, ok := enc.(*tg.MessagesChannelMessages)
		if !ok {
			t.Fatalf("getHistory response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("messages.search", func(t *testing.T) {
		enc, err := api.SearchForTest(s, subscriber.ID, &tg.MessagesSearchRequest{
			Peer: channelPeer(subscriber.ID, channel.ID),
			Q:    "broadcast visibility proof", Filter: &tg.InputMessagesFilterEmpty{}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("messages.search: %v", err)
		}
		result, ok := enc.(*tg.MessagesChannelMessages)
		if !ok {
			t.Fatalf("messages.search response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("channels.getMessages", func(t *testing.T) {
		enc, err := api.GetChannelMessagesForTest(s, subscriber.ID, &tg.ChannelsGetMessagesRequest{
			Channel: api.InputChannel(subscriber.ID, channel.ID),
			ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(post.LocalID)}},
		})
		if err != nil {
			t.Fatalf("channels.getMessages: %v", err)
		}
		result, ok := enc.(*tg.MessagesChannelMessages)
		if !ok {
			t.Fatalf("channels.getMessages response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("getChannelDifference", func(t *testing.T) {
		enc, err := api.GetChannelDifferenceForTest(s, subscriber.ID, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: api.InputChannel(subscriber.ID, channel.ID),
			Filter:  &tg.ChannelMessagesFilterEmpty{},
			Pts:     member.JoinPts,
			Limit:   100,
		})
		if err != nil {
			t.Fatalf("getChannelDifference: %v", err)
		}
		result, ok := enc.(*tg.UpdatesChannelDifference)
		if !ok {
			t.Fatalf("getChannelDifference response = %T", enc)
		}
		assertSubscriberView(t, result.NewMessages, result.Users)
	})

	t.Run("live push", func(t *testing.T) {
		enc, err := api.BuildChannelUpdatesForTest(s, subscriber.ID, channel.ID, member.JoinPts, 100)
		if err != nil {
			t.Fatalf("build live channel updates: %v", err)
		}
		assertEncodes(t, enc)
		result, ok := enc.(*tg.Updates)
		if !ok {
			t.Fatalf("live push response = %T", enc)
		}
		var messages []tg.MessageClass
		for _, update := range result.Updates {
			if added, ok := update.(*tg.UpdateNewChannelMessage); ok {
				messages = append(messages, added.Message)
			}
		}
		assertSubscriberView(t, messages, result.Users)
	})

	t.Run("dialog top message", func(t *testing.T) {
		enc, err := api.GetDialogsForTest(s, subscriber.ID)
		if err != nil {
			t.Fatalf("get dialogs: %v", err)
		}
		result, ok := enc.(*tg.MessagesDialogs)
		if !ok {
			t.Fatalf("get dialogs response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("peer dialog top message", func(t *testing.T) {
		enc, err := api.GetPeerDialogsForTest(s, subscriber.ID, &tg.MessagesGetPeerDialogsRequest{
			Peers: []tg.InputDialogPeerClass{
				&tg.InputDialogPeer{Peer: channelPeer(subscriber.ID, channel.ID)},
			},
		})
		if err != nil {
			t.Fatalf("get peer dialogs: %v", err)
		}
		result, ok := enc.(*tg.MessagesPeerDialogs)
		if !ok {
			t.Fatalf("get peer dialogs response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("searchGlobal", func(t *testing.T) {
		enc, err := api.SearchGlobalForTest(s, subscriber.ID, &tg.MessagesSearchGlobalRequest{
			Q:          "broadcast visibility proof",
			Filter:     &tg.InputMessagesFilterEmpty{},
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      10,
		})
		if err != nil {
			t.Fatalf("searchGlobal: %v", err)
		}
		result, ok := enc.(*tg.MessagesMessagesSlice)
		if !ok {
			t.Fatalf("searchGlobal response = %T", enc)
		}
		assertSubscriberView(t, result.Messages, result.Users)
	})

	t.Run("author view is outgoing", func(t *testing.T) {
		enc, err := api.GetHistoryForTest(s, author.ID, &tg.MessagesGetHistoryRequest{
			Peer: channelPeer(author.ID, channel.ID), Limit: 10,
		})
		if err != nil {
			t.Fatalf("author getHistory: %v", err)
		}
		result, ok := enc.(*tg.MessagesChannelMessages)
		if !ok {
			t.Fatalf("author getHistory response = %T", enc)
		}
		var found *tg.Message
		for _, message := range result.Messages {
			candidate, ok := message.(*tg.Message)
			if ok && candidate.ID == int(post.LocalID) {
				found = candidate
				break
			}
		}
		if found == nil {
			t.Fatalf("author history omitted post %d", post.LocalID)
		}
		assertEncodes(t, found)
		if !found.GetPost() || !found.Out {
			t.Fatalf("author post flags = post:%v out:%v, want post and outgoing", found.GetPost(), found.Out)
		}
		if _, ok := found.GetFromID(); ok {
			t.Fatal("broadcast post exposes from_id in author's view")
		}
	})

	t.Run("forward stores no author id", func(t *testing.T) {
		enc, err := api.ForwardMessagesForTest(s, subscriber.ID, &tg.MessagesForwardMessagesRequest{
			FromPeer: channelPeer(subscriber.ID, channel.ID),
			ID:       []int{int(post.LocalID)},
			ToPeer:   api.InputPeerUser(subscriber.ID, destination.ID),
			RandomID: []int64{224702},
		})
		if err != nil {
			t.Fatalf("forward broadcast post: %v", err)
		}
		result, ok := enc.(*tg.Updates)
		if !ok {
			t.Fatalf("forward response = %T", enc)
		}
		var forwarded *tg.Message
		for _, update := range result.Updates {
			if added, ok := update.(*tg.UpdateNewMessage); ok {
				forwarded, ok = added.Message.(*tg.Message)
				if !ok {
					t.Fatalf("forwarded message = %T", added.Message)
				}
				break
			}
		}
		if forwarded == nil {
			t.Fatal("forward response has no forwarded message")
		}
		assertEncodes(t, forwarded)
		header, ok := forwarded.GetFwdFrom()
		if !ok {
			t.Fatal("forwarded message has no forward header")
		}
		from, ok := header.GetFromID()
		if !ok {
			t.Fatal("forward header has no source channel")
		}
		channelPeer, ok := from.(*tg.PeerChannel)
		if !ok || channelPeer.ChannelID != channel.ID {
			t.Fatalf("forward source = %T %v, want channel %d", from, from, channel.ID)
		}
		channelPost, ok := header.GetChannelPost()
		if !ok || channelPost != int(post.LocalID) {
			t.Fatalf("forward channel_post = %d present=%v, want %d", channelPost, ok, post.LocalID)
		}
		for _, user := range result.Users {
			if user.GetID() == author.ID {
				t.Fatalf("forward users vector contains original author %d", author.ID)
			}
		}
		stored, err := s.History(ctx, subscriber.ID, store.PeerTypeUser, destination.ID, 0, 10)
		if err != nil {
			t.Fatalf("read stored forward: %v", err)
		}
		if len(stored) != 1 || stored[0].FwdFromID != 0 {
			t.Fatalf("stored forward source author = %+v, want no user id", stored)
		}
	})

	t.Run("megagroup keeps sender", func(t *testing.T) {
		group, err := s.CreateChannel(ctx, author.ID, "Visible senders", "", true)
		if err != nil {
			t.Fatalf("create megagroup: %v", err)
		}
		joinChannelByInvite(t, s, group, subscriber.ID)
		groupPost, _, duplicate, err := s.PostChannelMessageAs(ctx, group.ID, author.ID, "group sender remains visible", 224703, nil, 0)
		if err != nil || duplicate {
			t.Fatalf("post to megagroup: duplicate=%v err=%v", duplicate, err)
		}
		enc, err := api.GetHistoryForTest(s, subscriber.ID, &tg.MessagesGetHistoryRequest{
			Peer: channelPeer(subscriber.ID, group.ID), Limit: 10,
		})
		if err != nil {
			t.Fatalf("megagroup getHistory: %v", err)
		}
		result, ok := enc.(*tg.MessagesChannelMessages)
		if !ok {
			t.Fatalf("megagroup getHistory response = %T", enc)
		}
		var found *tg.Message
		for _, message := range result.Messages {
			candidate, ok := message.(*tg.Message)
			if ok && candidate.ID == int(groupPost.LocalID) {
				found = candidate
				break
			}
		}
		if found == nil {
			t.Fatal("megagroup history omitted post")
		}
		assertEncodes(t, found)
		from, ok := found.GetFromID()
		peer, peerOK := from.(*tg.PeerUser)
		if found.GetPost() || !ok || !peerOK || peer.UserID != author.ID {
			t.Fatalf("megagroup sender rendering = post:%v from:%T, want user %d and no post flag", found.GetPost(), from, author.ID)
		}
	})
}
