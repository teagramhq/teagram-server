package api_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChannelPollSendHistoryAndDifference(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551430002")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{
		Megagroup: true,
		Title:     "Channel poll lifecycle",
	})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, member.ID); err != nil {
		t.Fatalf("join member: %v", err)
	}

	result, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerChannel(creator.ID, channel.ID),
		Media:    fixedPollMedia("Channel question?", "A", "B"),
		RandomID: 1430001,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("send channel poll response = %T, want *tg.Updates", result)
	}
	var sent *tg.Message
	for _, update := range updates.Updates {
		if created, ok := update.(*tg.UpdateNewChannelMessage); ok {
			var ok bool
			sent, ok = created.Message.(*tg.Message)
			if !ok {
				t.Fatalf("channel poll send message = %T, want *tg.Message", created.Message)
			}
		}
	}
	if sent == nil {
		t.Fatalf("send channel poll response omitted a channel message: %+v", updates.Updates)
	}
	media, ok := sent.Media.(*tg.MessageMediaPoll)
	if !ok || media.Poll.ID <= 0 || media.Poll.Question.Text != "Channel question?" {
		t.Fatalf("sent channel poll media = %#v, want canonical poll", sent.Media)
	}

	history, err := api.GetChannelMessagesForTest(s, member.ID, &tg.ChannelsGetMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: sent.ID}},
	})
	if err != nil {
		t.Fatalf("get channel poll message: %v", err)
	}
	channelMessages, ok := history.(*tg.MessagesChannelMessages)
	if !ok || len(channelMessages.Messages) != 1 {
		t.Fatalf("channel messages = %T with %d entries, want one channel message", history, len(channelMessages.Messages))
	}
	memberMessage, ok := channelMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("channel message = %T, want *tg.Message", channelMessages.Messages[0])
	}
	memberMedia, ok := memberMessage.Media.(*tg.MessageMediaPoll)
	if !ok || memberMedia.Poll.ID != media.Poll.ID || memberMedia.Poll.Creator {
		t.Fatalf("member channel poll = %#v, want non-creator view of poll %d", memberMessage.Media, media.Poll.ID)
	}

	difference, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		Filter:  &tg.ChannelMessagesFilterEmpty{},
		Pts:     1,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("get channel difference: %v", err)
	}
	channelDifference, ok := difference.(*tg.UpdatesChannelDifference)
	if !ok || len(channelDifference.NewMessages) != 1 {
		t.Fatalf("channel difference = %T with %d new messages, want one poll post", difference, len(channelDifference.NewMessages))
	}
	diffMessage, ok := channelDifference.NewMessages[0].(*tg.Message)
	if !ok {
		t.Fatalf("difference message = %T, want *tg.Message", channelDifference.NewMessages[0])
	}
	diffMedia, ok := diffMessage.Media.(*tg.MessageMediaPoll)
	if !ok || diffMedia.Poll.ID != media.Poll.ID {
		t.Fatalf("difference poll media = %#v, want poll %d", diffMessage.Media, media.Poll.ID)
	}
}

func TestSupergroupPollVotesUpdateResultsWithoutChannelPts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430011")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551430012")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	observer, err := s.CreateUser(ctx, "+15551430013")
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Channel poll results"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, memberID := range []int64{voter.ID, observer.ID} {
		if _, _, err = s.JoinChannelByInvite(ctx, invite, memberID); err != nil {
			t.Fatalf("join channel member: %v", err)
		}
	}
	media := fixedPollMedia("Choose one", "A", "B")
	media.Poll.SetPublicVoters(true)
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: media, RandomID: 1430011,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	sentMessage := channelMessageFromSendResult(t, sent)
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before vote: %v", err)
	}
	if _, err = api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChannel(voter.ID, channel.ID), MsgID: sentMessage.ID, Options: [][]byte{[]byte("second")},
	}); err != nil {
		t.Fatalf("cast channel poll vote: %v", err)
	}
	for _, viewerID := range []int64{voter.ID, observer.ID} {
		result, resultErr := api.GetPollResultsForTest(s, viewerID, &tg.MessagesGetPollResultsRequest{
			Peer: api.InputPeerChannel(viewerID, channel.ID), MsgID: sentMessage.ID,
		})
		if resultErr != nil {
			t.Fatalf("get channel poll results for %d: %v", viewerID, resultErr)
		}
		update := pollUpdateFromResponse(t, result)
		if update.Peer != nil || update.MsgID != 0 || !update.Poll.Zero() || update.Results.TotalVoters != 1 || update.Results.Results[1].Voters != 1 {
			t.Fatalf("channel poll results disclose voter identity or have wrong counts: %+v", update)
		}
	}
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts {
		t.Fatalf("vote changed channel pts from %d to %d, err %v", beforePts, afterPts, err)
	}
}

func TestChannelPollCloseIsOneDurableChannelEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430021")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551430022")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551430023")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Channel poll close"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, userID := range []int64{member.ID, admin.ID} {
		if _, _, err = s.JoinChannelByInvite(ctx, invite, userID); err != nil {
			t.Fatalf("join channel user: %v", err)
		}
	}
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote channel admin: %v", err)
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Close this?", "A", "B"), RandomID: 1430021,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before close: %v", err)
	}
	closeRequest := func(userID int64) *tg.MessagesEditMessageRequest {
		closedPoll := fixedPollMedia("ignored", "ignored A", "ignored B")
		closedPoll.Poll.SetClosed(true)
		return &tg.MessagesEditMessageRequest{
			Peer: api.InputPeerChannel(userID, channel.ID), ID: message.ID, Media: closedPoll,
		}
	}
	if _, err = api.EditMessageForTest(s, member.ID, closeRequest(member.ID)); err == nil {
		t.Fatal("ordinary channel member closed another user's poll")
	}
	closed, err := api.EditMessageForTest(s, creator.ID, closeRequest(creator.ID))
	if err != nil {
		t.Fatalf("close channel poll: %v", err)
	}
	updates, ok := closed.(*tg.Updates)
	if !ok || len(updates.Updates) != 1 {
		t.Fatalf("close response = %T with %d updates, want one channel edit", closed, len(updates.Updates))
	}
	edit, ok := updates.Updates[0].(*tg.UpdateEditChannelMessage)
	if !ok {
		t.Fatalf("close update = %T, want updateEditChannelMessage", updates.Updates[0])
	}
	closedMessage, ok := edit.Message.(*tg.Message)
	if !ok {
		t.Fatalf("closed message = %T, want *tg.Message", edit.Message)
	}
	closedMedia, ok := closedMessage.Media.(*tg.MessageMediaPoll)
	if !ok || !closedMedia.Poll.Closed || closedMedia.Poll.Question.Text != "Close this?" {
		t.Fatalf("closed poll media = %#v, want original canonical poll closed", closedMessage.Media)
	}
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts+1 || edit.Pts != afterPts {
		t.Fatalf("channel pts after close = %d (edit pts %d), err %v; want %d", afterPts, edit.Pts, err, beforePts+1)
	}
	again, err := api.EditMessageForTest(s, creator.ID, closeRequest(creator.ID))
	if err != nil {
		t.Fatalf("repeat channel poll close: %v", err)
	}
	if againUpdates, ok := again.(*tg.Updates); !ok || len(againUpdates.Updates) != 0 {
		t.Fatalf("repeat close = %#v, want empty no-op updates", again)
	}
	finalPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || finalPts != afterPts {
		t.Fatalf("repeat close changed channel pts %d to %d, err %v", afterPts, finalPts, err)
	}
	difference, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: beforePts, Limit: 10,
	})
	if err != nil {
		t.Fatalf("get channel difference after close: %v", err)
	}
	channelDifference, ok := difference.(*tg.UpdatesChannelDifference)
	if !ok || len(channelDifference.OtherUpdates) != 1 {
		t.Fatalf("close difference = %T with %d other updates, want one durable edit", difference, len(channelDifference.OtherUpdates))
	}
	if _, ok = channelDifference.OtherUpdates[0].(*tg.UpdateEditChannelMessage); !ok {
		t.Fatalf("difference update = %T, want updateEditChannelMessage", channelDifference.OtherUpdates[0])
	}
	adminPoll, err := api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID), Media: fixedPollMedia("Admin may close?", "A", "B"), RandomID: 1430022,
	})
	if err != nil {
		t.Fatalf("send member-owned channel poll: %v", err)
	}
	adminMessage := channelMessageFromSendResult(t, adminPoll)
	adminClose := fixedPollMedia("ignored", "ignored A", "ignored B")
	adminClose.Poll.SetClosed(true)
	adminResult, err := api.EditMessageForTest(s, admin.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChannel(admin.ID, channel.ID), ID: adminMessage.ID, Media: adminClose,
	})
	if err != nil {
		t.Fatalf("channel admin close member poll: %v", err)
	}
	if adminUpdates, ok := adminResult.(*tg.Updates); !ok || len(adminUpdates.Updates) != 1 {
		t.Fatalf("admin close response = %T with %d updates, want one durable edit", adminResult, len(adminUpdates.Updates))
	}
}

func TestChannelPollPostingHonorsRestrictionsAndBroadcastRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430031")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551430032")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551430033")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	group := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Restricted poll group"})
	invite, err := s.CreateChannelInvite(ctx, group.ID, creator.ID)
	if err != nil {
		t.Fatalf("create group invite: %v", err)
	}
	for _, userID := range []int64{admin.ID, member.ID} {
		if _, _, err = s.JoinChannelByInvite(ctx, invite, userID); err != nil {
			t.Fatalf("join group member: %v", err)
		}
	}
	if err = s.SetChannelRole(ctx, group.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote group admin: %v", err)
	}
	if _, _, err = s.SetChannelDefaultBannedRights(ctx, group.ID, creator.ID, []string{"send_polls"}); err != nil {
		t.Fatalf("restrict group polls: %v", err)
	}
	groupPts, err := s.ChannelState(ctx, group.ID)
	if err != nil {
		t.Fatalf("group state: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, group.ID), Media: fixedPollMedia("Member poll?", "A", "B"), RandomID: 1430031,
	})
	if err == nil || !strings.Contains(err.Error(), "CHAT_WRITE_FORBIDDEN") {
		t.Fatalf("member poll send = %v, want CHAT_WRITE_FORBIDDEN", err)
	}
	unchangedGroupPts, err := s.ChannelState(ctx, group.ID)
	if err != nil || unchangedGroupPts != groupPts {
		t.Fatalf("rejected member poll changed pts %d to %d, err %v", groupPts, unchangedGroupPts, err)
	}
	if _, err = api.SendMediaForTest(s, admin.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(admin.ID, group.ID), Media: fixedPollMedia("Admin poll?", "A", "B"), RandomID: 1430032,
	}); err != nil {
		t.Fatalf("admin poll send with member restriction: %v", err)
	}
	if _, _, err = s.SetChannelDefaultBannedRights(ctx, group.ID, creator.ID, []string{"send_media"}); err != nil {
		t.Fatalf("restrict group media: %v", err)
	}
	mediaRestrictedPts, err := s.ChannelState(ctx, group.ID)
	if err != nil {
		t.Fatalf("group state after media restriction: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, group.ID), Media: fixedPollMedia("Member media-restricted poll?", "A", "B"), RandomID: 1430036,
	})
	if err == nil || !strings.Contains(err.Error(), "CHAT_WRITE_FORBIDDEN") {
		t.Fatalf("member poll under send_media restriction = %v, want CHAT_WRITE_FORBIDDEN", err)
	}
	unchangedMediaRestrictedPts, err := s.ChannelState(ctx, group.ID)
	if err != nil || unchangedMediaRestrictedPts != mediaRestrictedPts {
		t.Fatalf("rejected media-restricted poll changed pts %d to %d, err %v", mediaRestrictedPts, unchangedMediaRestrictedPts, err)
	}
	if _, err = api.SendMediaForTest(s, admin.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(admin.ID, group.ID), Media: fixedPollMedia("Admin media-restricted poll?", "A", "B"), RandomID: 1430037,
	}); err != nil {
		t.Fatalf("admin poll send with send_media restriction: %v", err)
	}
	if _, _, err = s.SetChannelDefaultBannedRights(ctx, group.ID, creator.ID, []string{"send_plain"}); err != nil {
		t.Fatalf("restrict group plain messages: %v", err)
	}
	if _, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, group.ID), Media: fixedPollMedia("Member poll with send_plain restricted?", "A", "B"), RandomID: 1430038,
	}); err != nil {
		t.Fatalf("member poll with send_plain restriction: %v", err)
	}

	broadcast := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Poll broadcast"})
	broadcastInvite, err := s.CreateChannelInvite(ctx, broadcast.ID, creator.ID)
	if err != nil {
		t.Fatalf("create broadcast invite: %v", err)
	}
	for _, userID := range []int64{admin.ID, member.ID} {
		if _, _, err = s.JoinChannelByInvite(ctx, broadcastInvite, userID); err != nil {
			t.Fatalf("join broadcast member: %v", err)
		}
	}
	if err = s.SetChannelRole(ctx, broadcast.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote broadcast admin: %v", err)
	}
	broadcastPts, err := s.ChannelState(ctx, broadcast.ID)
	if err != nil {
		t.Fatalf("broadcast state: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, broadcast.ID), Media: fixedPollMedia("Member broadcast poll?", "A", "B"), RandomID: 1430033,
	})
	if err == nil || !strings.Contains(err.Error(), "PEER_ID_INVALID") {
		t.Fatalf("broadcast member poll send = %v, want PEER_ID_INVALID", err)
	}
	public := fixedPollMedia("Public broadcast poll?", "A", "B")
	public.Poll.SetPublicVoters(true)
	_, err = api.SendMediaForTest(s, admin.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(admin.ID, broadcast.ID), Media: public, RandomID: 1430034,
	})
	if !tgerr.Is(err, "BROADCAST_PUBLIC_VOTERS_FORBIDDEN") {
		t.Fatalf("broadcast public-voter poll send = %v, want BROADCAST_PUBLIC_VOTERS_FORBIDDEN", err)
	}
	unchangedBroadcastPts, err := s.ChannelState(ctx, broadcast.ID)
	if err != nil || unchangedBroadcastPts != broadcastPts {
		t.Fatalf("rejected broadcast poll changed pts %d to %d, err %v", broadcastPts, unchangedBroadcastPts, err)
	}
	if _, err = api.SendMediaForTest(s, admin.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(admin.ID, broadcast.ID), Media: fixedPollMedia("Private broadcast poll?", "A", "B"), RandomID: 1430035,
	}); err != nil {
		t.Fatalf("admin private poll send: %v", err)
	}
}

func TestChannelPollSlowModeReturnsWaitWithoutStateChange(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430101")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551430102")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Poll slow mode"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, member.ID); err != nil {
		t.Fatalf("join member: %v", err)
	}
	if _, changed, setErr := s.SetChannelSlowMode(ctx, channel.ID, creator.ID, 10); setErr != nil || !changed {
		t.Fatalf("enable channel slow mode: changed %v err %v", changed, setErr)
	}
	if _, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID), Media: fixedPollMedia("First poll?", "A", "B"), RandomID: 1430101,
	}); err != nil {
		t.Fatalf("send first channel poll: %v", err)
	}
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("read channel state before slow-mode rejection: %v", err)
	}
	beforeEvents, err := s.ChannelEventsWindow(ctx, channel.ID, 0, beforePts, 20)
	if err != nil {
		t.Fatalf("read channel events before slow-mode rejection: %v", err)
	}
	_, err = api.SendMediaForTest(s, member.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(member.ID, channel.ID), Media: fixedPollMedia("Second poll?", "A", "B"), RandomID: 1430102,
	})
	rpc, ok := tgerr.As(err)
	if !ok || rpc.Code != 420 || rpc.Type != "SLOWMODE_WAIT" || rpc.Argument < 1 {
		t.Fatalf("channel poll during slow mode = %v, want 420 SLOWMODE_WAIT_<seconds>", err)
	}
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts {
		t.Fatalf("slow-mode rejection changed channel pts %d to %d, err %v", beforePts, afterPts, err)
	}
	afterEvents, err := s.ChannelEventsWindow(ctx, channel.ID, 0, afterPts, 20)
	if err != nil || !slices.Equal(afterEvents, beforeEvents) {
		t.Fatalf("slow-mode rejection changed channel events from %+v to %+v, err %v", beforeEvents, afterEvents, err)
	}
}

func TestChannelPollRetryAfterDeadlineReturnsCanonicalPollWithoutNewEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430111")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Poll retry deadline"})
	closeDate := time.Now().UTC().Add(8 * time.Second).Truncate(time.Second)
	media := fixedPollMedia("Original poll?", "A", "B")
	media.Poll.SetCloseDate(int(closeDate.Unix()))
	req := &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: media, RandomID: 1430111,
	}
	first, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("send initial timed channel poll: %v", err)
	}
	firstMessage := channelMessageFromSendResult(t, first)
	firstPoll, ok := firstMessage.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("initial channel poll media = %T, want poll", firstMessage.Media)
	}
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("read channel state before retry: %v", err)
	}
	beforeEvents, err := s.ChannelEventsWindow(ctx, channel.ID, 0, beforePts, 20)
	if err != nil {
		t.Fatalf("read channel events before retry: %v", err)
	}
	time.Sleep(time.Until(closeDate) + time.Second)
	retryMedia := fixedPollMedia("Changed retry payload", "A", "B")
	retryMedia.Poll.SetCloseDate(int(closeDate.Unix()))
	req.Media = retryMedia
	retry, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, req)
	if err != nil {
		t.Fatalf("retry expired channel poll: %v", err)
	}
	retryMessage := channelMessageFromSendResult(t, retry)
	retryPoll, ok := retryMessage.Media.(*tg.MessageMediaPoll)
	if !ok || retryMessage.ID != firstMessage.ID || retryPoll.Poll.ID != firstPoll.Poll.ID || retryPoll.Poll.Question.Text != "Original poll?" {
		t.Fatalf("expired poll retry message = %+v, want canonical poll %d with original question", retryMessage, firstPoll.Poll.ID)
	}
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts {
		t.Fatalf("expired poll retry changed channel pts %d to %d, err %v", beforePts, afterPts, err)
	}
	afterEvents, err := s.ChannelEventsWindow(ctx, channel.ID, 0, afterPts, 20)
	if err != nil || !slices.Equal(afterEvents, beforeEvents) {
		t.Fatalf("expired poll retry changed channel events from %+v to %+v, err %v", beforeEvents, afterEvents, err)
	}
}

func TestChannelPollMembershipPrecedesClosedStateForBansAndRemoval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430041")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	banned, err := s.CreateUser(ctx, "+15551430042")
	if err != nil {
		t.Fatalf("create banned member: %v", err)
	}
	leaver, err := s.CreateUser(ctx, "+15551430043")
	if err != nil {
		t.Fatalf("create leaver: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Poll membership boundary"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, userID := range []int64{banned.ID, leaver.ID} {
		if _, _, err = s.JoinChannelByInvite(ctx, invite, userID); err != nil {
			t.Fatalf("join channel member: %v", err)
		}
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Already closed?", "A", "B"), RandomID: 1430041,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	closedPoll := fixedPollMedia("ignored", "ignored A", "ignored B")
	closedPoll.Poll.SetClosed(true)
	if _, err = api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), ID: message.ID, Media: closedPoll,
	}); err != nil {
		t.Fatalf("close channel poll: %v", err)
	}
	if err = s.SetChannelBan(ctx, channel.ID, creator.ID, banned.ID, nil, true); err != nil {
		t.Fatalf("ban poll member: %v", err)
	}
	if _, err = s.LeaveChannel(ctx, channel.ID, leaver.ID); err != nil {
		t.Fatalf("remove poll member: %v", err)
	}
	for _, userID := range []int64{banned.ID, leaver.ID} {
		peer := api.InputPeerChannel(userID, channel.ID)
		if _, err = api.SendVoteForTest(s, userID, &tg.MessagesSendVoteRequest{
			Peer: peer, MsgID: message.ID, Options: [][]byte{[]byte("invalid-option")},
		}); err == nil || !strings.Contains(err.Error(), "PEER_ID_INVALID") {
			t.Fatalf("closed poll vote by inactive member %d = %v, want PEER_ID_INVALID before poll state", userID, err)
		}
		if _, err = api.GetPollResultsForTest(s, userID, &tg.MessagesGetPollResultsRequest{Peer: peer, MsgID: message.ID}); err == nil || !strings.Contains(err.Error(), "PEER_ID_INVALID") {
			t.Fatalf("closed poll results for inactive member %d = %v, want PEER_ID_INVALID", userID, err)
		}
	}
}

func TestSupergroupPublicPollVoterPagesRequireMembershipAndStayBounded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430051")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voters := make([]store.User, 3)
	for i := range voters {
		voters[i], err = s.CreateUser(ctx, fmt.Sprintf("+1555143005%d", i+2))
		if err != nil {
			t.Fatalf("create voter: %v", err)
		}
	}
	outsider, err := s.CreateUser(ctx, "+15551430055")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Public channel poll"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, voter := range voters {
		if _, _, err = s.JoinChannelByInvite(ctx, invite, voter.ID); err != nil {
			t.Fatalf("join voter: %v", err)
		}
	}
	media := fixedPollMedia("Who voted?", "A", "B")
	media.Poll.SetPublicVoters(true)
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: media, RandomID: 1430051,
	})
	if err != nil {
		t.Fatalf("send public channel poll: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	requestFor := func(userID int64, offset string, limit int) *tg.MessagesGetPollVotesRequest {
		req := &tg.MessagesGetPollVotesRequest{Peer: api.InputPeerChannel(userID, channel.ID), ID: message.ID, Limit: limit}
		if offset != "" {
			req.SetOffset(offset)
		}
		return req
	}
	if _, err = api.GetPollVotesForTest(s, creator.ID, requestFor(creator.ID, "", 1)); err == nil || !strings.Contains(err.Error(), "POLL_VOTE_REQUIRED") {
		t.Fatalf("non-voter channel voter list = %v, want POLL_VOTE_REQUIRED", err)
	}
	for i, voter := range voters {
		option := []byte("first")
		if i == 1 {
			option = []byte("second")
		}
		if _, err = api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
			Peer: api.InputPeerChannel(voter.ID, channel.ID), MsgID: message.ID, Options: [][]byte{option},
		}); err != nil {
			t.Fatalf("channel poll vote for %d: %v", voter.ID, err)
		}
	}
	first, err := api.GetPollVotesForTest(s, voters[0].ID, requestFor(voters[0].ID, "", 2))
	if err != nil {
		t.Fatalf("first voter page: %v", err)
	}
	firstPage := votesListFromResult(t, first)
	if firstPage.Count != 3 || len(firstPage.Votes) != 2 || firstPage.NextOffset == "" {
		t.Fatalf("first voter page = count %d votes %d next %q, want 3/2/nonempty", firstPage.Count, len(firstPage.Votes), firstPage.NextOffset)
	}
	firstIDs := voterIDs(t, firstPage.Votes)
	for _, id := range firstIDs {
		if id == voters[0].ID {
			continue
		}
		if profile, ok := loadUsersWire(t, firstPage.Users, id).(*tg.User); ok && profile.Phone != "" {
			t.Errorf("channel voter %d phone = %q, want withheld", id, profile.Phone)
		}
	}
	second, err := api.GetPollVotesForTest(s, voters[0].ID, requestFor(voters[0].ID, firstPage.NextOffset, 2))
	if err != nil {
		t.Fatalf("second voter page: %v", err)
	}
	secondPage := votesListFromResult(t, second)
	if secondPage.Count != 3 || len(secondPage.Votes) != 1 || secondPage.NextOffset != "" {
		t.Fatalf("second voter page = count %d votes %d next %q, want 3/1/empty", secondPage.Count, len(secondPage.Votes), secondPage.NextOffset)
	}
	for _, id := range voterIDs(t, secondPage.Votes) {
		if slices.Contains(firstIDs, id) {
			t.Errorf("channel voter %d repeated across cursor pages", id)
		}
	}
	if _, err = api.GetPollVotesForTest(s, outsider.ID, requestFor(outsider.ID, "", 2)); err == nil || !strings.Contains(err.Error(), "PEER_ID_INVALID") {
		t.Fatalf("non-member channel voter list = %v, want PEER_ID_INVALID", err)
	}
}

func TestChannelQuizKeyRemainsViewerScopedUntilVoteOrClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430061")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551430062")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Private quiz poll"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, voter.ID); err != nil {
		t.Fatalf("join voter: %v", err)
	}
	quiz := tg.Poll{
		Question: tg.TextWithEntities{Text: "Choose both correct answers"},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("a")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("b")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "C"}, Option: []byte("c")},
		},
	}
	quiz.SetQuiz(true)
	quiz.SetMultipleChoice(true)
	media := &tg.InputMediaPoll{Poll: quiz, CorrectAnswers: []int{0, 1}, Solution: "A and B"}
	media.SetCorrectAnswers([]int{0, 1})
	media.SetSolution("A and B")
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: media, RandomID: 1430061,
	})
	if err != nil {
		t.Fatalf("send channel quiz: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	readAs := func(userID int64) *tg.MessageMediaPoll {
		history, historyErr := api.GetChannelMessagesForTest(s, userID, &tg.ChannelsGetMessagesRequest{
			Channel: api.InputChannel(userID, channel.ID), ID: []tg.InputMessageClass{&tg.InputMessageID{ID: message.ID}},
		})
		if historyErr != nil {
			t.Fatalf("get channel quiz for %d: %v", userID, historyErr)
		}
		response, ok := history.(*tg.MessagesChannelMessages)
		if !ok || len(response.Messages) != 1 {
			t.Fatalf("channel quiz history = %T, want one message", history)
		}
		readMessage, ok := response.Messages[0].(*tg.Message)
		if !ok {
			t.Fatalf("channel quiz message = %T, want *tg.Message", response.Messages[0])
		}
		poll, ok := readMessage.Media.(*tg.MessageMediaPoll)
		if !ok {
			t.Fatalf("channel quiz media = %T, want poll media", readMessage.Media)
		}
		return poll
	}
	for _, userID := range []int64{creator.ID, voter.ID} {
		poll := readAs(userID)
		if !poll.Poll.Quiz || !poll.Poll.RevotingDisabled || poll.Results.Solution != "" || poll.Results.Results[0].Correct || poll.Results.Results[1].Correct {
			t.Fatalf("unvoted channel quiz disclosed key to %d: %+v", userID, poll)
		}
	}
	dialogs, err := api.GetDialogsForTest(s, voter.ID)
	if err != nil {
		t.Fatalf("get channel dialogs: %v", err)
	}
	assertPollMessageForViewer(t, messagesFromDialogResponse(t, dialogs), message.ID, false)
	peerDialogs, err := api.GetPeerDialogsForTest(s, voter.ID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChannel(voter.ID, channel.ID)}},
	})
	if err != nil {
		t.Fatalf("get channel peer dialogs: %v", err)
	}
	assertPollMessageForViewer(t, messagesFromDialogResponse(t, peerDialogs), message.ID, false)
	if _, err = api.GetPollVotesForTest(s, voter.ID, &tg.MessagesGetPollVotesRequest{
		Peer: api.InputPeerChannel(voter.ID, channel.ID), ID: message.ID, Limit: 10,
	}); err == nil || !strings.Contains(err.Error(), "POLL_VOTE_REQUIRED") {
		t.Fatalf("private channel quiz voter list = %v, want POLL_VOTE_REQUIRED", err)
	}
	voted, err := api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChannel(voter.ID, channel.ID), MsgID: message.ID, Options: [][]byte{[]byte("a"), []byte("b")},
	})
	if err != nil {
		t.Fatalf("vote in channel quiz: %v", err)
	}
	voteUpdate := pollUpdateFromResponse(t, voted)
	if voteUpdate.Peer != nil || voteUpdate.MsgID != 0 || !voteUpdate.Poll.Zero() || !voteUpdate.Results.Results[0].Chosen || !voteUpdate.Results.Results[1].Chosen || !voteUpdate.Results.Results[0].Correct || voteUpdate.Results.Solution != "A and B" {
		t.Fatalf("channel quiz vote response = %+v, want anonymous voter-scoped key", voteUpdate)
	}
	creatorResults, err := api.GetPollResultsForTest(s, creator.ID, &tg.MessagesGetPollResultsRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), MsgID: message.ID,
	})
	if err != nil {
		t.Fatalf("creator results before close: %v", err)
	}
	creatorUpdate := pollUpdateFromResponse(t, creatorResults)
	if creatorUpdate.Results.Results[0].Correct || creatorUpdate.Results.Solution != "" {
		t.Fatalf("non-voter creator saw quiz key before close: %+v", creatorUpdate.Results)
	}
	closedMedia := fixedPollMedia("ignored", "ignored A", "ignored B")
	closedMedia.Poll.SetClosed(true)
	if _, err = api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), ID: message.ID, Media: closedMedia,
	}); err != nil {
		t.Fatalf("close channel quiz: %v", err)
	}
	for _, userID := range []int64{creator.ID, voter.ID} {
		poll := readAs(userID)
		if !poll.Poll.Closed || !poll.Results.Results[0].Correct || !poll.Results.Results[1].Correct || poll.Results.Solution != "A and B" {
			t.Fatalf("closed channel quiz did not disclose canonical key to %d: %+v", userID, poll)
		}
	}
}

func TestChannelPollVoteWaitsForConcurrentBan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock barrier: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close lock barrier: %v", err)
		}
	}()
	creator, err := s.CreateUser(ctx, "+15551430071")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551430072")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Vote ban race"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, voter.ID); err != nil {
		t.Fatalf("join voter: %v", err)
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Vote only while active?", "A", "B"), RandomID: 1430071,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ban barrier: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }() //nolint:errcheck // best effort cleanup
	var blockerPID int32
	if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
		t.Fatalf("read ban barrier pid: %v", err)
	}
	if _, err = tx.Exec(ctx, `UPDATE channel_participants SET banned_until = 'infinity' WHERE channel_id = $1 AND user_id = $2`, channel.ID, voter.ID); err != nil {
		t.Fatalf("hold committed ban row: %v", err)
	}
	voteDone := make(chan error, 1)
	go func() {
		_, voteErr := api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
			Peer: api.InputPeerChannel(voter.ID, channel.ID), MsgID: message.ID, Options: [][]byte{[]byte("first")},
		})
		voteDone <- voteErr
	}()
	waitForPollParticipantWaiter(t, ctx, conn, blockerPID)
	select {
	case err = <-voteDone:
		t.Fatalf("vote completed before concurrent ban committed: %v", err)
	default:
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatalf("commit ban barrier: %v", err)
	}
	if err = <-voteDone; err == nil || !strings.Contains(err.Error(), "PEER_ID_INVALID") {
		t.Fatalf("vote after committed ban = %v, want PEER_ID_INVALID", err)
	}
	poll, err := s.PollForMessage(ctx, creator.ID, store.PollMessageRef{
		PeerType: store.PeerTypeChannel, PeerID: channel.ID, LocalID: int64(message.ID),
	})
	if err != nil || poll.VoterCount != 0 {
		t.Fatalf("vote committed across ban boundary: voters=%d err=%v, want zero", poll.VoterCount, err)
	}
}

func TestChannelPollVoteAndCloseRaceProducesOneDurableEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551430081")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551430082")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Megagroup: true, Title: "Vote close race"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err = s.JoinChannelByInvite(ctx, invite, voter.ID); err != nil {
		t.Fatalf("join voter: %v", err)
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("Vote or close?", "A", "B"), RandomID: 1430081,
	})
	if err != nil {
		t.Fatalf("send channel poll: %v", err)
	}
	message := channelMessageFromSendResult(t, sent)
	beforePts, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before race: %v", err)
	}
	start := make(chan struct{})
	voteDone := make(chan error, 1)
	closeDone := make(chan error, 1)
	go func() {
		<-start
		_, voteErr := api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
			Peer: api.InputPeerChannel(voter.ID, channel.ID), MsgID: message.ID, Options: [][]byte{[]byte("first")},
		})
		voteDone <- voteErr
	}()
	go func() {
		<-start
		closed := fixedPollMedia("ignored", "ignored A", "ignored B")
		closed.Poll.SetClosed(true)
		_, closeErr := api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
			Peer: api.InputPeerChannel(creator.ID, channel.ID), ID: message.ID, Media: closed,
		})
		closeDone <- closeErr
	}()
	close(start)
	select {
	case err = <-closeDone:
		if err != nil {
			t.Fatalf("concurrent close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent close did not finish")
	}
	select {
	case err = <-voteDone:
		if err != nil && !strings.Contains(err.Error(), "POLL_CLOSED") {
			t.Fatalf("concurrent vote = %v, want success or POLL_CLOSED", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent vote did not finish")
	}
	afterPts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPts != beforePts+1 {
		t.Fatalf("vote/close race channel pts %d -> %d err %v, want exactly one close edit", beforePts, afterPts, err)
	}
	poll, err := s.PollForMessage(ctx, voter.ID, store.PollMessageRef{
		PeerType: store.PeerTypeChannel, PeerID: channel.ID, LocalID: int64(message.ID),
	})
	if err != nil || !poll.Closed || poll.VoterCount > 1 {
		t.Fatalf("poll after vote/close race = closed %v voters %d err %v, want closed with at most one vote", poll.Closed, poll.VoterCount, err)
	}
}

func waitForPollParticipantWaiter(t *testing.T, ctx context.Context, conn *pgx.Conn, blockerPID int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := conn.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database()
				  AND wait_event_type = 'Lock'
				  AND $1 = ANY(pg_blocking_pids(pid))
			)
		`, blockerPID).Scan(&waiting); err != nil {
			t.Fatalf("inspect poll membership waiter: %v", err)
		}
		if waiting {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("channel poll mutation did not wait on the participant row lock")
}

func channelMessageFromSendResult(t *testing.T, result any) *tg.Message {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("channel poll send response = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		if created, ok := update.(*tg.UpdateNewChannelMessage); ok {
			message, ok := created.Message.(*tg.Message)
			if !ok {
				t.Fatalf("channel poll send message = %T, want *tg.Message", created.Message)
			}
			return message
		}
	}
	t.Fatalf("channel poll send response omitted updateNewChannelMessage: %+v", updates.Updates)
	return nil
}
