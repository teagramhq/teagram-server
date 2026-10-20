package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func hideChannelHistoryAndJoin(t *testing.T, ctx context.Context, s *store.Store, dsn string, channel store.Channel, userID int64) store.ChannelMember {
	t.Helper()
	channelExec(t, ctx, dsn, `UPDATE channels SET pre_history_hidden = true WHERE id = $1`, channel.ID)
	return joinChannelByInvite(t, s, channel, userID)
}

func TestGetChannelDifferenceOmitsEventsForFlooredPosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+155519225201")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	joiner, err := s.CreateUser(ctx, "+155519225202")
	if err != nil {
		t.Fatalf("create joiner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "History floor updates", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	oldTextResult, err := sendToChannel(t, s, creator.ID, channel.ID, "hidden deletion", 225201)
	if err != nil {
		t.Fatalf("send old text: %v", err)
	}
	oldTextUpdate := newChannelMessage(t, oldTextResult)
	oldText, ok := oldTextUpdate.Message.(*tg.Message)
	if !ok {
		t.Fatalf("old text message = %T, want *tg.Message", oldTextUpdate.Message)
	}
	oldPollResult, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Hidden poll?", "A", "B"), RandomID: 225202,
	})
	if err != nil {
		t.Fatalf("send old poll: %v", err)
	}
	oldPoll := channelMessageFromSendResult(t, oldPollResult)
	joinerMember := hideChannelHistoryAndJoin(t, ctx, s, dsn, channel, joiner.ID)

	closed := fixedPollMedia("ignored", "ignored A", "ignored B")
	closed.Poll.SetClosed(true)
	if _, err = api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), ID: oldPoll.ID, Media: closed,
	}); err != nil {
		t.Fatalf("close floored poll: %v", err)
	}
	if _, _, err = s.DeleteChannelMessages(ctx, channel.ID, creator.ID, []int64{int64(oldText.ID)}); err != nil {
		t.Fatalf("delete floored post: %v", err)
	}
	visibleResult, err := sendToChannel(t, s, creator.ID, channel.ID, "visible after join", 225203)
	if err != nil {
		t.Fatalf("send visible post: %v", err)
	}
	visibleUpdate := newChannelMessage(t, visibleResult)
	visible, ok := visibleUpdate.Message.(*tg.Message)
	if !ok {
		t.Fatalf("visible message = %T, want *tg.Message", visibleUpdate.Message)
	}

	result, err := api.GetChannelDifferenceForTest(s, joiner.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(joiner.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: joinerMember.JoinPts, Limit: 20,
	})
	if err != nil {
		t.Fatalf("get channel difference: %v", err)
	}
	difference, ok := result.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("difference = %T, want *tg.UpdatesChannelDifference", result)
	}
	var foundVisible bool
	for _, message := range difference.NewMessages {
		if got, ok := message.(*tg.Message); ok && got.ID == visible.ID {
			foundVisible = true
		}
	}
	if !foundVisible {
		t.Fatalf("difference omitted post %d sent above the floor: %+v", visible.ID, difference.NewMessages)
	}
	for _, update := range difference.OtherUpdates {
		switch got := update.(type) {
		case *tg.UpdateEditChannelMessage:
			if message, ok := got.Message.(*tg.Message); ok && message.ID == oldPoll.ID {
				t.Errorf("difference exposed poll-close edit for floored post %d", oldPoll.ID)
			}
		case *tg.UpdateDeleteChannelMessages:
			for _, id := range got.Messages {
				if id == oldText.ID {
					t.Errorf("difference exposed delete for floored post %d", oldText.ID)
				}
			}
		}
	}

	creatorResult, err := api.GetChannelDifferenceForTest(s, creator.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(creator.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: 1, Limit: 20,
	})
	if err != nil {
		t.Fatalf("creator get channel difference: %v", err)
	}
	creatorDifference, ok := creatorResult.(*tg.UpdatesChannelDifference)
	if !ok {
		t.Fatalf("creator difference = %T, want *tg.UpdatesChannelDifference", creatorResult)
	}
	var creatorSawOldEdit, creatorSawOldDelete bool
	for _, update := range creatorDifference.OtherUpdates {
		switch got := update.(type) {
		case *tg.UpdateEditChannelMessage:
			if message, ok := got.Message.(*tg.Message); ok && message.ID == oldPoll.ID {
				creatorSawOldEdit = true
			}
		case *tg.UpdateDeleteChannelMessages:
			for _, id := range got.Messages {
				creatorSawOldDelete = creatorSawOldDelete || id == oldText.ID
			}
		}
	}
	if !creatorSawOldEdit || !creatorSawOldDelete {
		t.Fatalf("floor-zero creator updates edit/delete=%v/%v, want both", creatorSawOldEdit, creatorSawOldDelete)
	}
}

func TestChannelActionsTreatFlooredPostAsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+155519225211")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	joiner, err := s.CreateUser(ctx, "+155519225212")
	if err != nil {
		t.Fatalf("create joiner: %v", err)
	}
	destination, err := s.CreateUser(ctx, "+155519225213")
	if err != nil {
		t.Fatalf("create destination: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "History floor actions", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	oldResult, err := sendToChannel(t, s, creator.ID, channel.ID, "hidden action target", 225211)
	if err != nil {
		t.Fatalf("send old post: %v", err)
	}
	oldUpdate := newChannelMessage(t, oldResult)
	old, ok := oldUpdate.Message.(*tg.Message)
	if !ok {
		t.Fatalf("old message = %T, want *tg.Message", oldUpdate.Message)
	}
	hideChannelHistoryAndJoin(t, ctx, s, dsn, channel, joiner.ID)

	_, err = api.ForwardMessagesForTest(s, joiner.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChannel(joiner.ID, channel.ID),
		ID:       []int{old.ID},
		RandomID: []int64{225212},
		ToPeer:   api.InputPeerUser(joiner.ID, destination.ID),
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	if _, ok, lookupErr := s.MessageByRandomID(ctx, joiner.ID, 225212); lookupErr != nil || ok {
		t.Fatalf("floored forward copy exists=%v, err=%v; want no copy", ok, lookupErr)
	}

	reactions, err := api.GetMessagesReactionsForTest(s, joiner.ID, &tg.MessagesGetMessagesReactionsRequest{
		Peer: api.InputPeerChannel(joiner.ID, channel.ID), ID: []int{old.ID},
	})
	if err != nil {
		t.Fatalf("get reactions for floored post: %v", err)
	}
	updates, ok := reactions.(*tg.Updates)
	if !ok || len(updates.Updates) != 0 {
		t.Fatalf("floored reaction updates = %#v, want no updates", reactions)
	}
	creatorReactions, err := api.GetMessagesReactionsForTest(s, creator.ID, &tg.MessagesGetMessagesReactionsRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), ID: []int{old.ID},
	})
	if err != nil {
		t.Fatalf("get reactions for floor-zero post: %v", err)
	}
	if got, ok := creatorReactions.(*tg.Updates); !ok || len(got.Updates) != 1 {
		t.Fatalf("floor-zero reaction updates = %#v, want one update", creatorReactions)
	}

	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before reply: %v", err)
	}
	_, err = api.SendMessageForTest(s, joiner.ID, &tg.MessagesSendMessageRequest{
		Peer: channelPeer(joiner.ID, channel.ID), Message: "reply to hidden post", RandomID: 225213,
		ReplyTo: &tg.InputReplyToMessage{ReplyToMsgID: old.ID},
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts {
		t.Fatalf("rejected reply changed channel pts from %d to %d, err %v", beforePts, afterPts, err)
	}
}

func TestChannelPollActionsTreatFlooredPostAsMissing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+155519225221")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	joiner, err := s.CreateUser(ctx, "+155519225222")
	if err != nil {
		t.Fatalf("create joiner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "History floor poll actions", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	result, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Hidden poll?", "A", "B"), RandomID: 225221,
	})
	if err != nil {
		t.Fatalf("send poll: %v", err)
	}
	message := channelMessageFromSendResult(t, result)
	pollMedia, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("poll media = %T, want *tg.MessageMediaPoll", message.Media)
	}
	poll := pollMedia.Poll
	answer, ok := poll.Answers[0].(*tg.PollAnswer)
	if !ok {
		t.Fatalf("poll answer = %T, want *tg.PollAnswer", poll.Answers[0])
	}
	hideChannelHistoryAndJoin(t, ctx, s, dsn, channel, joiner.ID)

	_, err = api.SendVoteForTest(s, joiner.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChannel(joiner.ID, channel.ID), MsgID: message.ID, Options: [][]byte{answer.Option},
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")
	_, err = api.GetPollResultsForTest(s, joiner.ID, &tg.MessagesGetPollResultsRequest{
		Peer: api.InputPeerChannel(joiner.ID, channel.ID), MsgID: message.ID,
	})
	rpcError(t, err, "MESSAGE_ID_INVALID")
}

func TestChannelMediaOnFlooredPostsIsNotDownloadable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	blobs := newBlobs(t)
	creator, err := s.CreateUser(ctx, "+155519225231")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	joiner, err := s.CreateUser(ctx, "+155519225232")
	if err != nil {
		t.Fatalf("create joiner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "History floor media", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	body := jpegPhotoPayload(t, 1600, 1600)
	saveParts(t, s, creator.ID, 225231, body)
	seedPhotoDerivativesBeforePublication(t, dsn)
	result, err := sendPhotoToChannel(t, s, blobs, creator.ID, channel.ID, 225231, body, "hidden photo", 225232)
	if err != nil {
		t.Fatalf("send channel photo: %v", err)
	}
	_, _, photo, err := parseChannelPhotoPost(result)
	if err != nil {
		t.Fatalf("parse channel photo: %v", err)
	}
	hideChannelHistoryAndJoin(t, ctx, s, dsn, channel, joiner.ID)

	if _, err = s.FileForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Errorf("floored channel file download error = %v, want ErrFileNotFound", err)
	}
	if _, err = s.PhotoDerivativeForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Errorf("floored photo derivative metadata error = %v, want ErrFileNotFound", err)
	}
	if _, err = s.PhotoDerivativeChunkForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID, 0, 1); !errors.Is(err, store.ErrFileNotFound) {
		t.Errorf("floored photo derivative bytes error = %v, want ErrFileNotFound", err)
	}
	if _, err = s.FileForDownload(ctx, photo.ID, photo.AccessHash, creator.ID); err != nil {
		t.Fatalf("floor-zero creator download: %v", err)
	}

	fileID := photo.ID
	if _, _, _, err = s.PostChannelMessage(ctx, channel.ID, creator.ID, "visible reference", 225233, &fileID, 0); err != nil {
		t.Fatalf("add visible reference to same file: %v", err)
	}
	if _, err = s.FileForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID); err != nil {
		t.Errorf("download through visible post error = %v", err)
	}
	if _, err = s.PhotoDerivativeForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID); err != nil {
		t.Errorf("derivative metadata through visible post error = %v", err)
	}
	if chunk, err := s.PhotoDerivativeChunkForDownload(ctx, photo.ID, photo.AccessHash, joiner.ID, 0, 1); err != nil || len(chunk) != 1 {
		t.Errorf("derivative chunk through visible post = %d bytes, err %v; want one byte", len(chunk), err)
	}
}
