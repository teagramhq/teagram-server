package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestGroupedIDOnUserMessageReadPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	sender, err := s.CreateUser(ctx, "+15551298401")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15551298402")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	for i := range 3 {
		if _, _, _, _, err = s.SendMessage(ctx, sender.ID, owner.ID, "album item", int64(i+1), 0, 0); err != nil {
			t.Fatalf("send message %d: %v", i+1, err)
		}
	}
	channelExec(t, ctx, dsn,
		`UPDATE messages SET grouped_id = 7351 WHERE owner_id = $1 AND local_id IN (1, 2)`,
		owner.ID)

	historyEnc, err := api.GetHistoryForTest(s, owner.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(owner.ID, sender.ID), Limit: 3,
	})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	history, ok := historyEnc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("get history result = %T, want *tg.MessagesMessages", historyEnc)
	}
	assertGroupedIDs(t, "messages.getHistory", history.Messages, map[int]int64{1: 7351, 2: 7351, 3: 0})

	getMessagesEnc, err := api.GetMessagesForTest(s, owner.ID, &tg.MessagesGetMessagesRequest{
		ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: 1},
			&tg.InputMessageID{ID: 2},
			&tg.InputMessageID{ID: 3},
		},
	})
	if err != nil {
		t.Fatalf("get messages: %v", err)
	}
	getMessages, ok := getMessagesEnc.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("get messages result = %T, want *tg.MessagesMessages", getMessagesEnc)
	}
	assertGroupedIDs(t, "messages.getMessages", getMessages.Messages, map[int]int64{1: 7351, 2: 7351, 3: 0})

	differenceEnc, err := api.GetDifferenceForTest(s, owner.ID, &tg.UpdatesGetDifferenceRequest{Pts: 0})
	if err != nil {
		t.Fatalf("get difference: %v", err)
	}
	difference, ok := differenceEnc.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("get difference result = %T, want *tg.UpdatesDifference", differenceEnc)
	}
	assertGroupedIDs(t, "updates.getDifference", difference.NewMessages, map[int]int64{1: 7351, 2: 7351, 3: 0})
}

func TestGroupedIDOnChannelMessageReadPaths(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551298411")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Grouped IDs", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	fromPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("read initial channel pts: %v", err)
	}
	postIDs := make([]int, 0, 3)
	for i := range 3 {
		post, _, _, postErr := s.PostChannelMessage(ctx, channel.ID, creator.ID, "album item", int64(i+1), nil, 0)
		if postErr != nil {
			t.Fatalf("post message %d: %v", i+1, postErr)
		}
		postIDs = append(postIDs, int(post.LocalID))
	}
	channelExec(t, ctx, dsn,
		`UPDATE channel_messages SET grouped_id = 7351 WHERE channel_id = $1 AND local_id IN ($2, $3)`,
		channel.ID, postIDs[0], postIDs[1])
	wantGroupedIDs := map[int]int64{postIDs[0]: 7351, postIDs[1]: 7351, postIDs[2]: 0}

	historyEnc, err := api.GetHistoryForTest(s, creator.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Limit: 3,
	})
	if err != nil {
		t.Fatalf("get channel history: %v", err)
	}
	history, ok := historyEnc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("get channel history result = %T, want *tg.MessagesChannelMessages", historyEnc)
	}
	assertGroupedIDs(t, "messages.getHistory on channel", history.Messages, wantGroupedIDs)

	channelMessagesEnc, err := api.GetChannelMessagesForTest(s, creator.ID, &tg.ChannelsGetMessagesRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: postIDs[0]},
			&tg.InputMessageID{ID: postIDs[1]},
			&tg.InputMessageID{ID: postIDs[2]},
		},
	})
	if err != nil {
		t.Fatalf("get channel messages: %v", err)
	}
	channelMessages, ok := channelMessagesEnc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("get channel messages result = %T, want *tg.MessagesChannelMessages", channelMessagesEnc)
	}
	assertGroupedIDs(t, "channels.getMessages", channelMessages.Messages, wantGroupedIDs)

	differenceEnc, err := api.GetChannelDifferenceForTest(s, creator.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		Filter:  &tg.ChannelMessagesFilterEmpty{},
		Pts:     fromPts,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("get channel difference: %v", err)
	}
	difference, ok := differenceEnc.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("get channel difference result = %T, want *tg.UpdatesChannelDifference", differenceEnc)
	}
	assertGroupedIDs(t, "updates.getChannelDifference", difference.NewMessages, wantGroupedIDs)
}

func assertGroupedIDs(t *testing.T, surface string, messages []tg.MessageClass, wantGroupedIDs map[int]int64) {
	t.Helper()
	if len(messages) != len(wantGroupedIDs) {
		t.Fatalf("%s returned %d messages, want %d", surface, len(messages), len(wantGroupedIDs))
	}
	seen := make(map[int]bool, len(messages))
	for _, messageClass := range messages {
		message, ok := messageClass.(*tg.Message)
		if !ok {
			t.Fatalf("%s message = %T, want *tg.Message", surface, messageClass)
		}
		wantGroupedID, expected := wantGroupedIDs[message.ID]
		if !expected || seen[message.ID] {
			t.Fatalf("%s returned unexpected or duplicate message id %d", surface, message.ID)
		}
		seen[message.ID] = true
		groupedID, present := message.GetGroupedID()
		if wantGroupedID == 0 {
			if present {
				t.Errorf("%s message %d grouped_id = %d, want field absent", surface, message.ID, groupedID)
			}
			continue
		}
		if !present || groupedID != wantGroupedID {
			t.Errorf("%s message %d grouped_id = %d, present=%v; want %d", surface, message.ID, groupedID, present, wantGroupedID)
		}
	}
	for id := range wantGroupedIDs {
		if !seen[id] {
			t.Errorf("%s omitted message %d", surface, id)
		}
	}
}
