package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestChannelTombstonesAreHiddenFromContentReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551293401")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551293402")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Tombstone readers", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	fileID := insertChannelSearchFile(t, ctx, dsn, creator.ID, "erased.bin", []string{}, true)
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "erased secret needle", 93401, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post media: duplicate=%v err=%v", duplicate, err)
	}
	if _, err = api.UpdatePinnedMessageForTest(s, creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: channelPeer(creator.ID, channel.ID), ID: int(post.LocalID),
	}); err != nil {
		t.Fatalf("pin media post: %v", err)
	}
	deleteChannelPost(t, ctx, dsn, channel.ID, post.LocalID)

	requested, err := api.GetChannelMessagesForTest(s, member.ID, &tg.ChannelsGetMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(post.LocalID)}},
	})
	if err != nil {
		t.Fatalf("get tombstoned message: %v", err)
	}
	assertEncodes(t, requested)
	requestedResponse, ok := requested.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("requested messages response = %T, want *tg.MessagesChannelMessages", requested)
	}
	requestedMessages := requestedResponse.Messages
	if len(requestedMessages) != 1 {
		t.Fatalf("requested messages = %d, want one empty marker", len(requestedMessages))
	}
	empty, ok := requestedMessages[0].(*tg.MessageEmpty)
	if !ok || empty.ID != int(post.LocalID) {
		t.Fatalf("requested tombstone = %#v, want MessageEmpty{id:%d}", requestedMessages[0], post.LocalID)
	}

	history, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{Peer: channelPeer(member.ID, channel.ID)})
	if err != nil {
		t.Fatalf("get history: %v", err)
	}
	if got := channelMessagesFrom(t, history).Messages; containsMessageID(got, int(post.LocalID)) {
		t.Fatalf("history contains tombstoned post %d", post.LocalID)
	}
	search, err := searchChannel(s, member.ID, channel.ID, "needle", 0, 10)
	if err != nil {
		t.Fatalf("search channel: %v", err)
	}
	if got := channelMessagesFrom(t, search).Messages; len(got) != 0 {
		t.Fatalf("channel search returned tombstone: %+v", got)
	}
	filtered, err := api.SearchForTest(s, member.ID, &tg.MessagesSearchRequest{
		Peer: channelPeer(member.ID, channel.ID), Q: "", Filter: &tg.InputMessagesFilterDocument{}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("filtered channel search: %v", err)
	}
	if got := channelMessagesFrom(t, filtered).Messages; len(got) != 0 {
		t.Fatalf("filtered channel search returned tombstone: %+v", got)
	}
	global, err := searchGlobal(s, member.ID, "needle", 10)
	if err != nil {
		t.Fatalf("global search: %v", err)
	}
	if got := globalSlice(t, global).Messages; len(got) != 0 {
		t.Fatalf("global search returned tombstone: %+v", got)
	}
	dialogsEnc, err := api.GetDialogsForTest(s, member.ID)
	if err != nil {
		t.Fatalf("get dialogs: %v", err)
	}
	dialogs, ok := dialogsEnc.(*tg.MessagesDialogs)
	if !ok {
		t.Fatalf("dialogs = %T, want *tg.MessagesDialogs", dialogsEnc)
	}
	if containsMessageID(dialogs.Messages, int(post.LocalID)) {
		t.Fatalf("dialogs contain tombstoned post %d", post.LocalID)
	}
	pinned, err := searchPinned(s, member.ID, channelPeer(member.ID, channel.ID), 0, 10)
	if err != nil {
		t.Fatalf("search pinned post: %v", err)
	}
	if got := pinnedChannelMessages(t, pinned); len(got.Messages) != 0 {
		t.Fatalf("pinned search returned tombstone: %+v", got.Messages)
	}
	files, err := s.FilesByIDs(ctx, []int64{fileID})
	if err != nil {
		t.Fatalf("read file fixture: %v", err)
	}
	_, err = api.GetFileForTest(s, member.ID, newBlobs(t), &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{ID: fileID, AccessHash: files[fileID].AccessHash},
		Limit:    1024,
	})
	if msg := rpcMessage(t, err); msg != "LOCATION_INVALID" {
		t.Errorf("download tombstoned channel file = %s, want LOCATION_INVALID", msg)
	}
}

func TestGetChannelDifferenceUsesDeletesForTombstonedNewAndEditEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551293411")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551293412")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Tombstone difference", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	fileID := insertChannelSearchFile(t, ctx, dsn, creator.ID, "difference-erased.bin", []string{}, true)
	first, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "first erased secret", 93411, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post first: duplicate=%v err=%v", duplicate, err)
	}
	second, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "second erased secret", 93412, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post second: duplicate=%v err=%v", duplicate, err)
	}
	channelExec(t, ctx, dsn, `UPDATE channel_events SET type = 2 WHERE channel_id = $1 AND local_id = $2`, channel.ID, second.LocalID)
	deleteChannelPost(t, ctx, dsn, channel.ID, first.LocalID)
	deleteChannelPost(t, ctx, dsn, channel.ID, second.LocalID)
	channelExec(t, ctx, dsn, `UPDATE channel_state SET pts = 5, date = now() WHERE channel_id = $1`, channel.ID)
	channelExec(t, ctx, dsn, `
		INSERT INTO channel_events (channel_id, pts, type, local_id)
		VALUES ($1, 4, 3, $2), ($1, 5, 3, $3)
	`, channel.ID, first.LocalID, second.LocalID)

	firstPage, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: 1, Limit: 1,
	})
	if err != nil {
		t.Fatalf("first channel difference: %v", err)
	}
	page, ok := firstPage.(*tg.UpdatesChannelDifference)
	if !ok || page.Final || page.Pts != 2 || len(page.NewMessages) != 0 || len(page.OtherUpdates) != 0 {
		t.Fatalf("first difference = %#v, want a non-final empty page at pts 2", firstPage)
	}

	secondPage, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: page.Pts, Limit: 1,
	})
	if err != nil {
		t.Fatalf("second channel difference: %v", err)
	}
	page, ok = secondPage.(*tg.UpdatesChannelDifference)
	if !ok || page.Final || page.Pts != 3 || len(page.NewMessages) != 0 || len(page.OtherUpdates) != 0 {
		t.Fatalf("second difference = %#v, want a non-final empty page at pts 3", secondPage)
	}

	thirdPage, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: page.Pts, Limit: 1,
	})
	if err != nil {
		t.Fatalf("third channel difference: %v", err)
	}
	page, ok = thirdPage.(*tg.UpdatesChannelDifference)
	if !ok || page.Final || page.Pts != 4 || len(page.NewMessages) != 0 || len(page.OtherUpdates) != 1 {
		t.Fatalf("third difference = %#v, want first delete at pts 4 and a recoverable gap", thirdPage)
	}
	assertChannelDeleteUpdate(t, page.OtherUpdates[0], channel.ID, int(first.LocalID), 4)

	fourthPage, err := api.GetChannelDifferenceForTest(s, member.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(member.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: page.Pts, Limit: 10,
	})
	if err != nil {
		t.Fatalf("fourth channel difference: %v", err)
	}
	page, ok = fourthPage.(*tg.UpdatesChannelDifference)
	if !ok || !page.Final || page.Pts != 5 || len(page.NewMessages) != 0 || len(page.OtherUpdates) != 1 {
		t.Fatalf("fourth difference = %#v, want the remaining delete at pts 5", fourthPage)
	}
	assertChannelDeleteUpdate(t, page.OtherUpdates[0], channel.ID, int(second.LocalID), 5)
}

func TestChannelTextRetryRejectsTombstoneAndWrongKinds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551293421")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551293422")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Tombstone retry", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, member.ID)
	owned, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "owned live text", 93421, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post owned text: duplicate=%v err=%v", duplicate, err)
	}
	foreign, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, member.ID, "foreign text", 93422, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post foreign text: duplicate=%v err=%v", duplicate, err)
	}
	tombstone, _, duplicate, err := s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "deleted text", 93423, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post tombstone: duplicate=%v err=%v", duplicate, err)
	}
	fileID := insertChannelSearchFile(t, ctx, dsn, creator.ID, "retry-document.bin", []string{}, true)
	document, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "document caption", 93424, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post document: duplicate=%v err=%v", duplicate, err)
	}
	pollResult, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Media: fixedPollMedia("retry poll question", "A", "B"), RandomID: 93425,
	})
	if err != nil {
		t.Fatalf("post poll: %v", err)
	}
	pollMessage := channelMessageFromSendResult(t, pollResult)
	channelExec(t, ctx, dsn, `UPDATE channel_messages SET random_id = $2 WHERE channel_id = $1 AND local_id = 1`, channel.ID, int64(93426))
	deleteChannelPost(t, ctx, dsn, channel.ID, tombstone.LocalID)

	stateBefore, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before retries: %v", err)
	}
	valid, err := sendToChannel(t, s, creator.ID, channel.ID, "retry text", 93421)
	if err != nil {
		t.Fatalf("valid text retry: %v", err)
	}
	validNew := newChannelMessage(t, valid)
	if validNew.Message.GetID() != int(owned.LocalID) || validNew.Pts != int(owned.LocalID) {
		t.Fatalf("valid retry = %+v, want original id and pts %d", validNew, owned.LocalID)
	}
	for _, randomID := range []int64{93422, 93423, 93424, 93425, 93426} {
		_, retryErr := sendToChannel(t, s, creator.ID, channel.ID, "retry text", randomID)
		if msg := rpcMessage(t, retryErr); msg != "RANDOM_ID_DUPLICATE" {
			t.Errorf("text retry random_id %d = %s, want RANDOM_ID_DUPLICATE", randomID, msg)
		}
	}
	if _, _, duplicate, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "raced foreign retry", 93422, nil, 0); err == nil || duplicate {
		t.Errorf("transaction retry for foreign post = duplicate %v err %v, want duplicate error", duplicate, err)
	}
	if _, _, duplicate, err = s.PostChannelMessageAs(ctx, channel.ID, creator.ID, "raced deleted retry", 93423, nil, 0); err == nil || duplicate {
		t.Errorf("transaction retry for tombstone = duplicate %v err %v, want duplicate error", duplicate, err)
	}
	stateAfter, err := s.ChannelState(ctx, channel.ID)
	if err != nil || stateAfter != stateBefore {
		t.Errorf("invalid retries changed channel pts from %d to %d, err %v", stateBefore, stateAfter, err)
	}
	if document.LocalID == owned.LocalID || foreign.LocalID == owned.LocalID || tombstone.LocalID == owned.LocalID || pollMessage.ID == int(owned.LocalID) {
		t.Fatal("retry fixtures unexpectedly shared a message id")
	}
}

func TestChannelTextRetryChecksCurrentPostRightsBeforeReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551293431")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551293432")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Retry rights", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	post, err := sendToChannel(t, s, admin.ID, channel.ID, "admin post", 93431)
	if err != nil {
		t.Fatalf("send as admin: %v", err)
	}
	original := newChannelMessage(t, post)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 0); err != nil {
		t.Fatalf("demote admin: %v", err)
	}
	_, err = sendToChannel(t, s, admin.ID, channel.ID, "retry", 93431)
	if msg := rpcMessage(t, err); msg != "PEER_ID_INVALID" {
		t.Fatalf("demoted retry = %s, want PEER_ID_INVALID", msg)
	}
	pts, err := s.ChannelState(ctx, channel.ID)
	if err != nil || pts != original.Pts {
		t.Fatalf("demoted retry changed channel pts to %d from %d, err %v", pts, original.Pts, err)
	}
}

func TestChannelTextRetryRejectsBannedAdminBeforeReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551293441")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551293442")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Banned retry rights", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, channel, admin.ID)
	if err = s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("promote admin: %v", err)
	}
	post, err := sendToChannel(t, s, admin.ID, channel.ID, "admin post", 93441)
	if err != nil {
		t.Fatalf("send as admin: %v", err)
	}
	original := newChannelMessage(t, post)
	banChannelMember(t, ctx, dsn, channel.ID, admin.ID, time.Now().Add(time.Hour))

	member, found, err := s.ChannelMemberOf(ctx, channel.ID, admin.ID)
	if err != nil || !found || member.Role != 1 || !member.Banned(time.Now()) {
		t.Fatalf("member after ban = %+v, found=%v, err=%v; want banned admin", member, found, err)
	}
	ptsBefore, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("channel state before banned retry: %v", err)
	}
	_, err = sendToChannel(t, s, admin.ID, channel.ID, "retry", 93441)
	if msg := rpcMessage(t, err); msg != "PEER_ID_INVALID" {
		t.Fatalf("banned retry = %s, want PEER_ID_INVALID", msg)
	}
	ptsAfter, err := s.ChannelState(ctx, channel.ID)
	if err != nil || ptsAfter != ptsBefore || ptsAfter != original.Pts {
		t.Fatalf("banned retry changed channel pts from %d to %d (post pts %d), err %v", ptsBefore, ptsAfter, original.Pts, err)
	}
}

func channelMessagesFrom(t *testing.T, enc bin.Encoder) *tg.MessagesChannelMessages {
	t.Helper()
	messages, ok := enc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("channel messages response = %T, want *tg.MessagesChannelMessages", enc)
	}
	return messages
}

func containsMessageID(messages []tg.MessageClass, id int) bool {
	for _, message := range messages {
		if message.GetID() == id {
			return true
		}
	}
	return false
}

func assertChannelDeleteUpdate(t *testing.T, update tg.UpdateClass, channelID int64, messageID, pts int) {
	t.Helper()
	deleted, ok := update.(*tg.UpdateDeleteChannelMessages)
	if !ok || deleted.ChannelID != channelID || len(deleted.Messages) != 1 || deleted.Messages[0] != messageID || deleted.Pts != pts || deleted.PtsCount != 1 {
		t.Fatalf("delete update = %#v, want channel %d message %d at pts %d count 1", update, channelID, messageID, pts)
	}
}
