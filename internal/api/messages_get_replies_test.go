package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func getRepliesViaDispatcher(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	req *tg.MessagesGetRepliesRequest,
) (*tg.MessagesChannelMessages, *mt.RPCError) {
	t.Helper()
	body := dispatchChatMethod(t, h, userID, "messages.getReplies", req)
	var result tg.MessagesChannelMessages
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &result, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getReplies result: %v", err)
	}
	return nil, &rpc
}

func sentChannelMessageID(t *testing.T, enc bin.Encoder) int {
	t.Helper()
	update := newChannelMessage(t, enc)
	message, ok := update.Message.(*tg.Message)
	if !ok {
		t.Fatalf("sent channel message = %T, want *tg.Message", update.Message)
	}
	return message.ID
}

func sendChannelReply(t *testing.T, s *store.Store, userID, channelID int64, replyTo int, text string, randomID int64) int {
	t.Helper()
	result, err := api.SendMessageForTest(s, userID, &tg.MessagesSendMessageRequest{
		Peer:     channelPeer(userID, channelID),
		Message:  text,
		RandomID: randomID,
		ReplyTo:  &tg.InputReplyToMessage{ReplyToMsgID: replyTo},
	})
	if err != nil {
		t.Fatalf("send channel reply: %v", err)
	}
	return sentChannelMessageID(t, result)
}

func getRepliesMessageIDs(t *testing.T, result *tg.MessagesChannelMessages) []int {
	t.Helper()
	ids := make([]int, 0, len(result.Messages))
	for _, message := range result.Messages {
		post, ok := message.(*tg.Message)
		if !ok {
			t.Fatalf("reply = %T, want *tg.Message", message)
		}
		ids = append(ids, post.ID)
	}
	return ids
}

func TestMessagesGetRepliesReturnsOnlyDirectLiveReplies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551298001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	replier, err := s.CreateUser(ctx, "+15551298002")
	if err != nil {
		t.Fatalf("create replier: %v", err)
	}
	nestedAuthor, err := s.CreateUser(ctx, "+15551298003")
	if err != nil {
		t.Fatalf("create nested author: %v", err)
	}
	viewer, err := s.CreateUser(ctx, "+15551298004")
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Reply visibility"})
	for _, userID := range []int64{replier.ID, nestedAuthor.ID, viewer.ID} {
		joinChannelByInvite(t, s, store.Channel{ID: channel.ID, CreatorID: creator.ID}, userID)
	}
	root := sentChannelMessageID(t, mustSendChannelMessage(t, s, creator.ID, channel.ID, "root body", 98001))
	first := sendChannelReply(t, s, replier.ID, channel.ID, root, "first direct reply", 98002)
	_ = sendChannelReply(t, s, nestedAuthor.ID, channel.ID, first, "nested reply", 98003)
	last := sendChannelReply(t, s, replier.ID, channel.ID, root, "last direct reply", 98004)
	deleted := sendChannelReply(t, s, creator.ID, channel.ID, root, "deleted direct reply", 98005)
	if _, _, err := s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{int64(deleted)}); err != nil {
		t.Fatalf("delete direct reply: %v", err)
	}

	result, rpc := getRepliesViaDispatcher(t, fullChannelDispatcher(s), viewer.ID, &tg.MessagesGetRepliesRequest{
		Peer:  api.InputPeerChannel(viewer.ID, channel.ID),
		MsgID: root,
		Limit: 10,
	})
	if rpc != nil {
		t.Fatalf("getReplies: %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if result.Count != 2 {
		t.Fatalf("reply count = %d, want 2", result.Count)
	}
	if got, want := getRepliesMessageIDs(t, result), []int{last, first}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("reply ids = %v, want newest-first %v", got, want)
	}
	if len(result.Users) != 1 {
		t.Fatalf("users = %d, want only the direct replier", len(result.Users))
	}
	if user, ok := result.Users[0].(*tg.User); !ok || user.ID != replier.ID {
		t.Fatalf("user = %T/%v, want replier %d", result.Users[0], result.Users[0], replier.ID)
	}
}

func TestMessagesGetRepliesUsesHistoryOffsetSemantics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551298101")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	replier, err := s.CreateUser(ctx, "+15551298102")
	if err != nil {
		t.Fatalf("create replier: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Reply offsets"})
	joinChannelByInvite(t, s, store.Channel{ID: channel.ID, CreatorID: creator.ID}, replier.ID)
	root := sentChannelMessageID(t, mustSendChannelMessage(t, s, creator.ID, channel.ID, "root", 98101))
	ids := []int{
		sendChannelReply(t, s, replier.ID, channel.ID, root, "one", 98102),
		sendChannelReply(t, s, replier.ID, channel.ID, root, "two", 98103),
		sendChannelReply(t, s, replier.ID, channel.ID, root, "three", 98104),
		sendChannelReply(t, s, replier.ID, channel.ID, root, "four", 98105),
	}
	h := fullChannelDispatcher(s)
	tests := []struct {
		name    string
		request tg.MessagesGetRepliesRequest
		want    []int
	}{
		{name: "offset id", request: tg.MessagesGetRepliesRequest{OffsetID: ids[3], Limit: 2}, want: []int{ids[2], ids[1]}},
		{name: "positive add offset", request: tg.MessagesGetRepliesRequest{AddOffset: 1, Limit: 1}, want: []int{ids[2]}},
		{name: "negative add offset around offset id", request: tg.MessagesGetRepliesRequest{OffsetID: ids[2], AddOffset: -1, Limit: 1}, want: []int{ids[2]}},
		{name: "min and max ids", request: tg.MessagesGetRepliesRequest{MinID: ids[1], MaxID: ids[3], Limit: 10}, want: []int{ids[2]}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.request.Peer = api.InputPeerChannel(replier.ID, channel.ID)
			tc.request.MsgID = root
			result, rpc := getRepliesViaDispatcher(t, h, replier.ID, &tc.request)
			if rpc != nil {
				t.Fatalf("getReplies: %d %s", rpc.ErrorCode, rpc.ErrorMessage)
			}
			if got := getRepliesMessageIDs(t, result); len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) || (len(got) > 1 && got[1] != tc.want[1]) {
				t.Fatalf("reply ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMessagesGetRepliesRejectsUnsupportedPeersAndUnauthorizedMembers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551298201")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551298202")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551298203")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	group := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Reply access"})
	joinChannelByInvite(t, s, store.Channel{ID: group.ID, CreatorID: creator.ID}, member.ID)
	root := sentChannelMessageID(t, mustSendChannelMessage(t, s, creator.ID, group.ID, "root", 98201))
	h := fullChannelDispatcher(s)
	var rpc *mt.RPCError

	for _, tc := range []struct {
		name string
		user int64
		peer tg.InputPeerClass
		want string
	}{
		{name: "private chat", user: member.ID, peer: &tg.InputPeerSelf{}, want: "PEER_ID_INVALID"},
		{name: "basic group", user: member.ID, peer: api.InputPeerChat(member.ID, 1), want: "PEER_ID_INVALID"},
		{name: "wrong channel access hash", user: member.ID, peer: &tg.InputPeerChannel{ChannelID: group.ID, AccessHash: api.InputPeerChannel(member.ID, group.ID).AccessHash + 1}, want: "PEER_ID_INVALID"},
		{name: "nonmember", user: outsider.ID, peer: api.InputPeerChannel(outsider.ID, group.ID), want: "PEER_ID_INVALID"},
		{name: "missing channel", user: outsider.ID, peer: api.InputPeerChannel(outsider.ID, group.ID+1_000_000), want: "PEER_ID_INVALID"},
		{name: "missing root", user: member.ID, peer: api.InputPeerChannel(member.ID, group.ID), want: "MSG_ID_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgID := root
			if tc.name == "missing root" {
				msgID += 1_000_000
			}
			_, rpc := getRepliesViaDispatcher(t, h, tc.user, &tg.MessagesGetRepliesRequest{Peer: tc.peer, MsgID: msgID, Limit: 10})
			if rpc == nil || rpc.ErrorMessage != tc.want {
				t.Fatalf("getReplies error = %v, want %s", rpc, tc.want)
			}
		})
	}

	broadcast := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "No reply threads"})
	for _, tc := range []struct {
		name   string
		userID int64
		want   string
	}{
		{name: "broadcast member", userID: creator.ID, want: "MSG_ID_INVALID"},
		{name: "broadcast nonmember with cached valid hash", userID: outsider.ID, want: "PEER_ID_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rpc := getRepliesViaDispatcher(t, h, tc.userID, &tg.MessagesGetRepliesRequest{
				Peer:  api.InputPeerChannel(tc.userID, broadcast.ID),
				MsgID: 1,
				Limit: 10,
			})
			if rpc == nil || rpc.ErrorMessage != tc.want {
				t.Fatalf("broadcast getReplies error = %v, want %s", rpc, tc.want)
			}
		})
	}
	deletedRoot := sentChannelMessageID(t, mustSendChannelMessage(t, s, creator.ID, group.ID, "deleted root", 98202))
	if _, _, err := s.DeleteChannelMessages(ctx, group.ID, creator.ID, []int64{int64(deletedRoot)}); err != nil {
		t.Fatalf("delete root: %v", err)
	}
	_, rpc = getRepliesViaDispatcher(t, h, member.ID, &tg.MessagesGetRepliesRequest{Peer: api.InputPeerChannel(member.ID, group.ID), MsgID: deletedRoot, Limit: 10})
	if rpc == nil || rpc.ErrorMessage != "MSG_ID_INVALID" {
		t.Fatalf("deleted root getReplies error = %v, want MSG_ID_INVALID", rpc)
	}

	chat, err := s.CreateChat(ctx, creator.ID, "Basic group", []int64{member.ID})
	if err != nil {
		t.Fatalf("create basic group: %v", err)
	}
	_, rpc = getRepliesViaDispatcher(t, h, member.ID, &tg.MessagesGetRepliesRequest{Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: root, Limit: 10})
	if rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("basic group getReplies error = %v, want PEER_ID_INVALID", rpc)
	}

	banChannelMember(t, ctx, dsn, group.ID, member.ID, time.Now().Add(time.Hour))
	_, rpc = getRepliesViaDispatcher(t, h, member.ID, &tg.MessagesGetRepliesRequest{Peer: api.InputPeerChannel(member.ID, group.ID), MsgID: root, Limit: 10})
	if rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("banned getReplies error = %v, want PEER_ID_INVALID", rpc)
	}
	channelExec(t, ctx, dsn, `DELETE FROM channel_participants WHERE channel_id = $1 AND user_id = $2`, group.ID, member.ID)
	_, rpc = getRepliesViaDispatcher(t, h, member.ID, &tg.MessagesGetRepliesRequest{Peer: api.InputPeerChannel(member.ID, group.ID), MsgID: root, Limit: 10})
	if rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("former member getReplies error = %v, want PEER_ID_INVALID", rpc)
	}
}

func mustSendChannelMessage(t *testing.T, s *store.Store, userID, channelID int64, text string, randomID int64) bin.Encoder {
	t.Helper()
	result, err := sendToChannel(t, s, userID, channelID, text, randomID)
	if err != nil {
		t.Fatalf("send channel message: %v", err)
	}
	return result
}
