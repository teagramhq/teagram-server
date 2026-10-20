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
	firstName := "Ann"
	if err := s.UpdateProfile(ctx, author.ID, &firstName, nil); err != nil {
		t.Fatalf("set broadcast poster name: %v", err)
	}
	toggleReq := &tg.ChannelsToggleSignaturesRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
	}
	toggleReq.SetSignaturesEnabled(true)
	toggle, err := api.ToggleSignaturesForTest(s, creator.ID, toggleReq)
	if err != nil {
		t.Fatalf("toggle signatures: %v", err)
	}
	updated, ok := toggle.(*tg.Updates)
	if !ok {
		t.Fatalf("toggle response = %T, want *tg.Updates", toggle)
	}
	if len(updated.Chats) != 1 {
		t.Fatalf("toggle chats = %d, want the updated channel", len(updated.Chats))
	}
	updatedChannel, ok := updated.Chats[0].(*tg.Channel)
	if !ok || !updatedChannel.GetSignatures() {
		t.Fatalf("toggle channel = %T %+v, want signatures enabled", updated.Chats[0], updated.Chats[0])
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
		if got, present := found.GetPostAuthor(); !present || got != "Ann" {
			t.Fatalf("post_author = %q, present=%v; want stored signature Ann", got, present)
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
		if got, present := header.GetPostAuthor(); !present || got != "Ann" {
			t.Fatalf("forward post_author = %q, present=%v; want Ann", got, present)
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

func TestSearchGlobalRendersMixedChannelKindsFromOnePage(t *testing.T) {
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551822480")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	author, err := s.CreateUser(ctx, "+15551822481")
	if err != nil {
		t.Fatalf("create author: %v", err)
	}
	subscriber, err := s.CreateUser(ctx, "+15551822482")
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	broadcast, err := s.CreateChannel(ctx, creator.ID, "Broadcast", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	megagroup, err := s.CreateChannel(ctx, creator.ID, "Megagroup", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	for _, channel := range []store.Channel{broadcast, megagroup} {
		joinChannelByInvite(t, s, channel, author.ID)
		joinChannelByInvite(t, s, channel, subscriber.ID)
		if channel.ID == broadcast.ID {
			if err := s.SetChannelRole(ctx, channel.ID, creator.ID, author.ID, 1); err != nil {
				t.Fatalf("promote broadcast poster: %v", err)
			}
		}
		if _, _, duplicate, postErr := s.PostChannelMessageAs(ctx, channel.ID, author.ID, "channel visibility proof", channel.ID, nil, 0); postErr != nil || duplicate {
			t.Fatalf("post in channel %d: duplicate=%v err=%v", channel.ID, duplicate, postErr)
		}
	}

	enc, err := api.SearchGlobalForTest(s, subscriber.ID, &tg.MessagesSearchGlobalRequest{
		Q:          "channel visibility proof",
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
	posts := map[int64]*tg.Message{}
	for _, message := range result.Messages {
		post, ok := message.(*tg.Message)
		if !ok {
			t.Fatalf("search result = %T, want *tg.Message", message)
		}
		peer, ok := post.PeerID.(*tg.PeerChannel)
		if !ok {
			t.Fatalf("search result peer = %T, want *tg.PeerChannel", post.PeerID)
		}
		posts[peer.ChannelID] = post
	}
	for channelID, wantBroadcast := range map[int64]bool{broadcast.ID: true, megagroup.ID: false} {
		post, found := posts[channelID]
		if !found {
			t.Fatalf("search omitted channel %d", channelID)
		}
		if post.GetPost() != wantBroadcast {
			t.Errorf("channel %d post flag = %v, want %v", channelID, post.GetPost(), wantBroadcast)
		}
		fromID, hasFromID := post.GetFromID()
		if wantBroadcast && hasFromID {
			t.Errorf("broadcast channel %d exposes from_id %v", channelID, fromID)
		}
		if !wantBroadcast && (!hasFromID || fromID.String() != (&tg.PeerUser{UserID: author.ID}).String()) {
			t.Errorf("megagroup channel %d from_id = %v, present=%v, want author %d", channelID, fromID, hasFromID, author.ID)
		}
	}
}

func TestToggleBroadcastSignaturesChecksAuthorityAndExposesChannelFlag(t *testing.T) {
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551822490")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551822491")
	if err != nil {
		t.Fatalf("create change-info admin: %v", err)
	}
	adminWithoutChangeInfo, err := s.CreateUser(ctx, "+15551822492")
	if err != nil {
		t.Fatalf("create admin without change-info: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551822493")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Signatures", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	joinChannelByInvite(t, s, channel, adminWithoutChangeInfo.ID)
	joinChannelByInvite(t, s, channel, member.ID)
	if err = s.SetChannelRoleWithRights(ctx, channel.ID, creator.ID, admin.ID, 1, true); err != nil {
		t.Fatalf("promote change-info admin: %v", err)
	}
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, adminWithoutChangeInfo.ID, 1); err != nil {
		t.Fatalf("promote admin without change-info: %v", err)
	}

	on := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(admin.ID, channel.ID)}
	on.SetSignaturesEnabled(true)
	result, err := api.ToggleSignaturesForTest(s, admin.ID, on)
	if err != nil {
		t.Fatalf("admin with change_info toggle on: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("toggle response = %T, want *tg.Updates", result)
	}
	if len(updates.Chats) != 1 {
		t.Fatalf("toggle chats = %d, want one updated channel", len(updates.Chats))
	}
	chat, ok := updates.Chats[0].(*tg.Channel)
	if !ok || !chat.GetSignatures() {
		t.Fatalf("updated channel = %T %+v, want signatures enabled", updates.Chats[0], updates.Chats[0])
	}

	fullResult, err := api.GetFullChannelForTest(s, creator.ID, &tg.ChannelsGetFullChannelRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
	})
	if err != nil {
		t.Fatalf("get full channel: %v", err)
	}
	full, ok := fullResult.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("full channel response = %T, want *tg.MessagesChatFull", fullResult)
	}
	if len(full.Chats) != 1 {
		t.Fatalf("full channel chats = %d, want one channel", len(full.Chats))
	}
	fullChannel, ok := full.Chats[0].(*tg.Channel)
	if !ok || !fullChannel.GetSignatures() {
		t.Fatalf("full channel chat = %T %+v, want signatures enabled", full.Chats[0], full.Chats[0])
	}

	for _, caller := range []struct {
		name string
		user store.User
	}{
		{name: "admin without change_info", user: adminWithoutChangeInfo},
		{name: "ordinary member", user: member},
	} {
		t.Run(caller.name, func(t *testing.T) {
			req := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(caller.user.ID, channel.ID)}
			if _, callErr := api.ToggleSignaturesForTest(s, caller.user.ID, req); rpcMessage(t, callErr) != "CHAT_ADMIN_REQUIRED" {
				t.Fatalf("toggle error = %v, want CHAT_ADMIN_REQUIRED", callErr)
			}
		})
	}

	off := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(creator.ID, channel.ID)}
	result, err = api.ToggleSignaturesForTest(s, creator.ID, off)
	if err != nil {
		t.Fatalf("creator toggle off: %v", err)
	}
	updates, ok = result.(*tg.Updates)
	if !ok || len(updates.Chats) != 1 {
		t.Fatalf("toggle-off response = %T, want updated channel", result)
	}
	chat, ok = updates.Chats[0].(*tg.Channel)
	if !ok || chat.GetSignatures() {
		t.Fatalf("toggle-off channel = %T %+v, want signatures disabled", updates.Chats[0], updates.Chats[0])
	}

	megagroup, err := s.CreateChannel(ctx, creator.ID, "Megagroup", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	megagroupReq := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(creator.ID, megagroup.ID)}
	megagroupReq.SetSignaturesEnabled(true)
	if _, err = api.ToggleSignaturesForTest(s, creator.ID, megagroupReq); rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("megagroup toggle error = %v, want PEER_ID_INVALID", err)
	}
}

func TestBroadcastSignatureIsCapturedForEachPost(t *testing.T) {
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551822500")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551822501")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	subscriber, err := s.CreateUser(ctx, "+15551822502")
	if err != nil {
		t.Fatalf("create subscriber: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Signature snapshots", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	joinChannelByInvite(t, s, channel, subscriber.ID)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote broadcast poster: %v", err)
	}
	firstName := "Ann"
	if err = s.UpdateProfile(ctx, admin.ID, &firstName, nil); err != nil {
		t.Fatalf("set admin name: %v", err)
	}
	unnamedAdmin, err := s.CreateUser(ctx, "+15551822503")
	if err != nil {
		t.Fatalf("create admin without display name: %v", err)
	}
	joinChannelByInvite(t, s, channel, unnamedAdmin.ID)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, unnamedAdmin.ID, 1); err != nil {
		t.Fatalf("promote admin without display name: %v", err)
	}

	unsigned, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, admin.ID, "before signatures", 225001, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post before enabling signatures: duplicate=%v err=%v", duplicate, err)
	}
	if unsigned.PostAuthor != nil {
		t.Fatalf("unsigned post author = %v, want nil", *unsigned.PostAuthor)
	}

	enable := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(creator.ID, channel.ID)}
	enable.SetSignaturesEnabled(true)
	if _, err = api.ToggleSignaturesForTest(s, creator.ID, enable); err != nil {
		t.Fatalf("enable signatures: %v", err)
	}
	signed, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, admin.ID, "signed by Ann", 225002, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post with signatures on: duplicate=%v err=%v", duplicate, err)
	}
	if signed.PostAuthor == nil || *signed.PostAuthor != "Ann" {
		t.Fatalf("signed post author = %v, want Ann", signed.PostAuthor)
	}

	unnamed, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, unnamedAdmin.ID, "signed without a display name", 225005, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post by admin without a display name: duplicate=%v err=%v", duplicate, err)
	}
	if unnamed.PostAuthor != nil {
		t.Fatalf("post author without a display name = %q, want nil", *unnamed.PostAuthor)
	}

	firstName = "Bea"
	if err = s.UpdateProfile(ctx, admin.ID, &firstName, nil); err != nil {
		t.Fatalf("rename admin: %v", err)
	}
	disable := &tg.ChannelsToggleSignaturesRequest{Channel: api.InputChannel(creator.ID, channel.ID)}
	if _, err = api.ToggleSignaturesForTest(s, creator.ID, disable); err != nil {
		t.Fatalf("disable signatures: %v", err)
	}
	unsignedAgain, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, admin.ID, "after signatures", 225003, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post with signatures off: duplicate=%v err=%v", duplicate, err)
	}
	if unsignedAgain.PostAuthor != nil {
		t.Fatalf("post after disabling signatures has author %q, want nil", *unsignedAgain.PostAuthor)
	}

	stored, err := s.ChannelMessages(ctx, channel.ID, []int64{unsigned.LocalID, signed.LocalID, unsignedAgain.LocalID})
	if err != nil {
		t.Fatalf("reload posts: %v", err)
	}
	if stored[unsigned.LocalID].PostAuthor != nil {
		t.Errorf("pre-toggle post was restamped: %v", stored[unsigned.LocalID].PostAuthor)
	}
	if stored[signed.LocalID].PostAuthor == nil || *stored[signed.LocalID].PostAuthor != "Ann" {
		t.Errorf("signed post author after rename and toggle-off = %v, want Ann", stored[signed.LocalID].PostAuthor)
	}
	if stored[unsignedAgain.LocalID].PostAuthor != nil {
		t.Errorf("post after toggle-off has signature %v", stored[unsignedAgain.LocalID].PostAuthor)
	}

	history, err := api.GetHistoryForTest(s, subscriber.ID, &tg.MessagesGetHistoryRequest{
		Peer: channelPeer(subscriber.ID, channel.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("history after toggle-off: %v", err)
	}
	page, ok := history.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("history response = %T", history)
	}
	for _, message := range page.Messages {
		post, ok := message.(*tg.Message)
		if !ok {
			continue
		}
		if post.ID == int(signed.LocalID) {
			if got, present := post.GetPostAuthor(); !present || got != "Ann" {
				t.Fatalf("rendered signed post author = %q, present=%v; want Ann after toggle-off", got, present)
			}
		}
	}

	forwarded, err := api.ForwardMessagesForTest(s, subscriber.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: channelPeer(subscriber.ID, channel.ID),
		ID:       []int{int(signed.LocalID)},
		ToPeer:   api.InputPeerUser(subscriber.ID, admin.ID),
		RandomID: []int64{225004},
	})
	if err != nil {
		t.Fatalf("forward signed post: %v", err)
	}
	updates, ok := forwarded.(*tg.Updates)
	if !ok {
		t.Fatalf("forward response = %T", forwarded)
	}
	var forwardedMessage *tg.Message
	for _, update := range updates.Updates {
		if added, ok := update.(*tg.UpdateNewMessage); ok {
			if message, ok := added.Message.(*tg.Message); ok {
				forwardedMessage = message
			}
		}
	}
	if forwardedMessage == nil {
		t.Fatal("forward response has no message")
	}
	header, ok := forwardedMessage.GetFwdFrom()
	if !ok {
		t.Fatal("forwarded message has no forward header")
	}
	if got, present := header.GetPostAuthor(); !present || got != "Ann" {
		t.Fatalf("forward author = %q, present=%v; want the stored Ann signature", got, present)
	}
}
