package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type channelReadFloorFixture struct {
	s       *store.Store
	dsn     string
	creator store.User
	reader  store.User
	channel store.Channel
}

func newChannelReadFloorFixture(t *testing.T, suffix, title string) channelReadFloorFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+155518"+suffix+"1")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	reader, err := s.CreateUser(ctx, "+155518"+suffix+"2")
	if err != nil {
		t.Fatalf("create reader: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, title, "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	return channelReadFloorFixture{s: s, dsn: dsn, creator: creator, reader: reader, channel: channel}
}

func (f channelReadFloorFixture) post(t *testing.T, text string, randomID int64) store.ChannelMessage {
	t.Helper()
	post, _, _, err := f.s.PostChannelMessage(context.Background(), f.channel.ID, f.creator.ID, text, randomID, nil, 0)
	if err != nil {
		t.Fatalf("post channel message: %v", err)
	}
	return post
}

func (f channelReadFloorFixture) joinAt(t *testing.T, floor int64) store.ChannelMember {
	t.Helper()
	member := joinChannelByInvite(t, f.s, f.channel, f.reader.ID)
	channelExec(t, context.Background(), f.dsn,
		`UPDATE channel_participants SET available_min_id = $1 WHERE channel_id = $2 AND user_id = $3`,
		floor, f.channel.ID, f.reader.ID,
	)
	return member
}

func channelMessageIDs(t *testing.T, messages []tg.MessageClass) []int {
	t.Helper()
	ids := make([]int, 0, len(messages))
	for _, class := range messages {
		switch message := class.(type) {
		case *tg.Message:
			ids = append(ids, message.ID)
		case *tg.MessageService:
			ids = append(ids, message.ID)
		default:
			t.Fatalf("message = %T, want a channel message", class)
		}
	}
	return ids
}

func channelMessagesResult(t *testing.T, result bin.Encoder) *tg.MessagesChannelMessages {
	t.Helper()
	messages, ok := result.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("response = %T, want *tg.MessagesChannelMessages", result)
	}
	return messages
}

func channelPinID(t *testing.T, localID int64) int32 {
	t.Helper()
	if localID < 0 || localID > 1<<31-1 {
		t.Fatalf("channel post id %d does not fit in a pinned message id", localID)
	}
	return int32(localID) //nolint:gosec // the message id is range-checked above
}

func TestChannelHistoryHidesPostsBelowFloorAndCountsOnlyVisiblePosts(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225101", "Floor history")
	old := make([]store.ChannelMessage, 0, 5)
	for i := range 5 {
		old = append(old, f.post(t, "old history", int64(225100+i)))
	}
	floor := old[len(old)-1].LocalID
	f.joinAt(t, floor)
	visible := make([]store.ChannelMessage, 0, 3)
	for i := range 3 {
		visible = append(visible, f.post(t, "visible history", int64(225110+i)))
	}

	for _, tc := range []struct {
		name string
		req  *tg.MessagesGetHistoryRequest
	}{
		{name: "newest page", req: &tg.MessagesGetHistoryRequest{OffsetID: 0, Limit: 10}},
		{name: "seek page", req: &tg.MessagesGetHistoryRequest{OffsetID: int(visible[2].LocalID), Limit: 10}},
		{name: "around page", req: &tg.MessagesGetHistoryRequest{OffsetID: int(visible[1].LocalID), AddOffset: -1, Limit: 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.req.Peer = api.InputPeerChannel(f.reader.ID, f.channel.ID)
			result, err := api.GetHistoryForTest(f.s, f.reader.ID, tc.req)
			if err != nil {
				t.Fatalf("getHistory: %v", err)
			}
			page := channelMessagesResult(t, result)
			for _, id := range channelMessageIDs(t, page.Messages) {
				if int64(id) <= floor {
					t.Errorf("getHistory returned floored id %d at floor %d", id, floor)
				}
			}
			if tc.name == "newest page" {
				got := channelMessageIDs(t, page.Messages)
				want := []int{int(visible[2].LocalID), int(visible[1].LocalID), int(visible[0].LocalID)}
				if len(got) != len(want) {
					t.Errorf("getHistory ids = %v, want %v", got, want)
				} else {
					for i := range want {
						if got[i] != want[i] {
							t.Errorf("getHistory ids = %v, want %v", got, want)
							break
						}
					}
				}
			}
			if page.Count != len(visible) {
				t.Errorf("getHistory count = %d, want %d visible posts", page.Count, len(visible))
			}
		})
	}
}

func TestChannelSearchAndSearchCountersHideFlooredPosts(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225102", "Floor search")
	hidden := f.post(t, "needle hidden https://floor.example/old", 225120)
	pinID := channelPinID(t, hidden.LocalID)
	if _, _, err := f.s.SetChannelPinnedMessage(context.Background(), f.channel.ID, f.creator.ID, &pinID); err != nil {
		t.Fatalf("pin hidden post: %v", err)
	}
	f.joinAt(t, hidden.LocalID)
	peer := api.InputPeerChannel(f.reader.ID, f.channel.ID)

	t.Run("plain search", func(t *testing.T) {
		result, err := api.SearchForTest(f.s, f.reader.ID, &tg.MessagesSearchRequest{
			Peer: peer, Q: "needle", Filter: &tg.InputMessagesFilterEmpty{}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("messages.search: %v", err)
		}
		page := channelMessagesResult(t, result)
		if len(page.Messages) != 0 {
			t.Fatalf("plain search returned floored post ids %v", channelMessageIDs(t, page.Messages))
		}
	})

	t.Run("pinned search", func(t *testing.T) {
		result, err := api.SearchForTest(f.s, f.reader.ID, &tg.MessagesSearchRequest{
			Peer: peer, Filter: &tg.InputMessagesFilterPinned{}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("pinned messages.search: %v", err)
		}
		page := channelMessagesResult(t, result)
		if len(page.Messages) != 0 {
			t.Fatalf("pinned search returned floored post ids %v", channelMessageIDs(t, page.Messages))
		}
	})

	t.Run("filtered search and counters", func(t *testing.T) {
		result, err := api.SearchForTest(f.s, f.reader.ID, &tg.MessagesSearchRequest{
			Peer: peer, Filter: &tg.InputMessagesFilterURL{}, Limit: 10,
		})
		if err != nil {
			t.Fatalf("filtered messages.search: %v", err)
		}
		page := channelMessagesResult(t, result)
		if len(page.Messages) != 0 || page.Count != 0 {
			t.Fatalf("filtered search returned ids %v with count %d, want no floored result", channelMessageIDs(t, page.Messages), page.Count)
		}
		counters, err := api.SearchCountersForTest(f.s, f.reader.ID, &tg.MessagesGetSearchCountersRequest{
			Peer: peer, Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterURL{}},
		})
		if err != nil {
			t.Fatalf("messages.getSearchCounters: %v", err)
		}
		vector, ok := counters.(*tg.MessagesSearchCounterVector)
		if !ok || len(vector.Elems) != 1 || vector.Elems[0].Count != 0 {
			t.Fatalf("search counters = %#v, want one zero-count URL counter", counters)
		}
	})
}

func TestSearchGlobalHidesFlooredChannelPosts(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225103", "Floor global search")
	hidden := f.post(t, "globalfloorneedle", 225130)
	f.joinAt(t, hidden.LocalID)
	result, err := api.SearchGlobalForTest(f.s, f.reader.ID, &tg.MessagesSearchGlobalRequest{
		Q: "globalfloorneedle", Filter: &tg.InputMessagesFilterEmpty{}, OffsetPeer: &tg.InputPeerEmpty{}, Limit: 10,
	})
	if err != nil {
		t.Fatalf("messages.searchGlobal: %v", err)
	}
	page := globalSlice(t, result)
	if len(page.Messages) != 0 {
		t.Fatalf("searchGlobal returned floored post ids %v", channelMessageIDs(t, page.Messages))
	}
}

func TestChannelGetMessagesAndExportLinkHideFlooredPost(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225104", "Floor id reads")
	hidden := f.post(t, "hidden id post", 225140)
	f.joinAt(t, hidden.LocalID)
	visible := f.post(t, "visible id post", 225141)

	result, err := api.GetChannelMessagesForTest(f.s, f.reader.ID, &tg.ChannelsGetMessagesRequest{
		Channel: api.InputChannel(f.reader.ID, f.channel.ID),
		ID: []tg.InputMessageClass{
			&tg.InputMessageID{ID: int(hidden.LocalID)},
			&tg.InputMessageID{ID: int(visible.LocalID)},
		},
	})
	if err != nil {
		t.Fatalf("channels.getMessages: %v", err)
	}
	page := channelMessagesResult(t, result)
	if got, want := channelMessageIDs(t, page.Messages), []int{int(visible.LocalID)}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("channels.getMessages ids = %v, want only visible id %v", got, want)
	}
	if _, err := api.ExportMessageLinkForTest(f.s, f.reader.ID, &tg.ChannelsExportMessageLinkRequest{
		Channel: api.InputChannel(f.reader.ID, f.channel.ID), ID: int(hidden.LocalID),
	}); err == nil || rpcMessage(t, err) != "PEER_ID_INVALID" {
		t.Fatalf("exportMessageLink error = %v, want PEER_ID_INVALID for floored post", err)
	}
}

func TestChannelRepliesTreatFlooredRootAsMissing(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225105", "Floor replies")
	root := f.post(t, "hidden root", 225150)
	reply, _, _, err := f.s.PostChannelMessage(context.Background(), f.channel.ID, f.creator.ID, "hidden reply", 225151, nil, root.LocalID)
	if err != nil {
		t.Fatalf("post reply: %v", err)
	}
	if int64(reply.ReplyToMsgID) != root.LocalID {
		t.Fatalf("reply target = %d, want %d", reply.ReplyToMsgID, root.LocalID)
	}
	f.joinAt(t, root.LocalID)
	result, rpc := getRepliesViaDispatcher(t, fullChannelDispatcher(f.s), f.reader.ID, &tg.MessagesGetRepliesRequest{
		Peer: api.InputPeerChannel(f.reader.ID, f.channel.ID), MsgID: int(root.LocalID), Limit: 10,
	})
	if rpc == nil || rpc.ErrorMessage != "MSG_ID_INVALID" {
		t.Fatalf("getReplies result = %#v, rpc = %v, want MSG_ID_INVALID for floored root", result, rpc)
	}
}

func TestChannelDialogsUseHistoryFloorForTopUnreadAndReadMarker(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225106", "Floor dialogs")
	old := f.post(t, "hidden dialog top", 225160)
	member := f.joinAt(t, old.LocalID)
	dialog := channelDialogForTest(t, f.s, f.reader.ID, f.channel.ID)
	if dialog.TopMessage != 0 {
		t.Errorf("dialog top message = %d, want no visible top post", dialog.TopMessage)
	}
	if dialog.UnreadCount != 0 {
		t.Errorf("dialog unread count = %d, want 0 below floor", dialog.UnreadCount)
	}
	if dialog.ReadInboxMaxID < int(old.LocalID) {
		t.Errorf("dialog read marker = %d, want at least floor %d", dialog.ReadInboxMaxID, old.LocalID)
	}
	peerResult, err := api.GetPeerDialogsForTest(f.s, f.reader.ID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChannel(f.reader.ID, f.channel.ID)}},
	})
	if err != nil {
		t.Fatalf("messages.getPeerDialogs: %v", err)
	}
	peerDialogs, ok := peerResult.(*tg.MessagesPeerDialogs)
	if !ok || len(peerDialogs.Dialogs) != 1 {
		t.Fatalf("getPeerDialogs response = %T with %d dialogs, want one", peerResult, len(peerDialogs.Dialogs))
	}
	if len(peerDialogs.Messages) != 0 {
		t.Errorf("getPeerDialogs returned %d floored top messages, want none", len(peerDialogs.Messages))
	}
	peerDialog, ok := peerDialogs.Dialogs[0].(*tg.Dialog)
	if !ok {
		t.Fatalf("getPeerDialogs item = %T, want *tg.Dialog", peerDialogs.Dialogs[0])
	}
	if peerDialog.TopMessage != 0 || peerDialog.UnreadCount != 0 || peerDialog.ReadInboxMaxID < int(old.LocalID) {
		t.Errorf("getPeerDialogs top/read/unread = %d/%d/%d, want 0/at least %d/0",
			peerDialog.TopMessage, peerDialog.ReadInboxMaxID, peerDialog.UnreadCount, old.LocalID)
	}
	if member.JoinPts < 0 {
		t.Fatalf("invalid member join pts %d", member.JoinPts)
	}
}

func TestChannelFullInfoOmitsFlooredPinnedID(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225107", "Floor full info")
	hidden := f.post(t, "hidden pinned post", 225170)
	pinID := channelPinID(t, hidden.LocalID)
	if _, _, err := f.s.SetChannelPinnedMessage(context.Background(), f.channel.ID, f.creator.ID, &pinID); err != nil {
		t.Fatalf("pin hidden post: %v", err)
	}
	f.joinAt(t, hidden.LocalID)
	result, err := api.GetFullChannelForTest(f.s, f.reader.ID, &tg.ChannelsGetFullChannelRequest{
		Channel: api.InputChannel(f.reader.ID, f.channel.ID),
	})
	if err != nil {
		t.Fatalf("channels.getFullChannel: %v", err)
	}
	full, ok := result.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("getFullChannel response = %T, want *tg.MessagesChatFull", result)
	}
	channelFull, ok := full.FullChat.(*tg.ChannelFull)
	if !ok {
		t.Fatalf("full chat = %T, want *tg.ChannelFull", full.FullChat)
	}
	if id, present := channelFull.GetPinnedMsgID(); present {
		t.Fatalf("channelFull pinned id = %d, want omitted below floor %d", id, hidden.LocalID)
	}
}

func TestChannelFullInfoUsesHistoryFloorForUnreadAndReadMarker(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225110", "Floor full info read state")
	posts := make([]store.ChannelMessage, 0, 7)
	for i := range 7 {
		posts = append(posts, f.post(t, "full info unread post", int64(225200+i)))
	}
	floor := posts[3].LocalID
	if floor != 5 || posts[len(posts)-1].LocalID != 8 {
		t.Fatalf("fixture floor and last post id = %d/%d, want 5/8", floor, posts[len(posts)-1].LocalID)
	}
	f.joinAt(t, floor)
	channelExec(t, context.Background(), f.dsn,
		`DELETE FROM channel_read_state WHERE channel_id = $1 AND user_id = $2`,
		f.channel.ID, f.reader.ID,
	)

	dialog := channelDialogForTest(t, f.s, f.reader.ID, f.channel.ID)
	if dialog.ReadInboxMaxID != int(floor) || dialog.UnreadCount != 3 {
		t.Fatalf("dialog read/unread = %d/%d, want %d/3", dialog.ReadInboxMaxID, dialog.UnreadCount, floor)
	}
	result, err := api.GetFullChannelForTest(f.s, f.reader.ID, &tg.ChannelsGetFullChannelRequest{
		Channel: api.InputChannel(f.reader.ID, f.channel.ID),
	})
	if err != nil {
		t.Fatalf("channels.getFullChannel: %v", err)
	}
	fullResponse, ok := result.(*tg.MessagesChatFull)
	if !ok {
		t.Fatalf("getFullChannel response = %T, want *tg.MessagesChatFull", result)
	}
	full := fullChannelInfo(t, fullResponse)
	if full.ReadInboxMaxID != dialog.ReadInboxMaxID || full.UnreadCount != dialog.UnreadCount {
		t.Errorf("channelFull read/unread = %d/%d, want dialog values %d/%d",
			full.ReadInboxMaxID, full.UnreadCount, dialog.ReadInboxMaxID, dialog.UnreadCount)
	}
}

func TestChannelUpdatesOmitPinnedIDAtOrBelowHistoryFloor(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225108", "Floor pin updates")
	hidden := f.post(t, "hidden pin update target", 225180)
	pinID := channelPinID(t, hidden.LocalID)
	if _, _, err := f.s.SetChannelPinnedMessage(context.Background(), f.channel.ID, f.creator.ID, &pinID); err != nil {
		t.Fatalf("pin hidden post: %v", err)
	}
	member := f.joinAt(t, hidden.LocalID)
	if _, _, err := f.s.SetChannelPinnedMessage(context.Background(), f.channel.ID, f.creator.ID, nil); err != nil {
		t.Fatalf("unpin hidden post: %v", err)
	}
	result, err := api.BuildChannelUpdatesForTest(f.s, f.reader.ID, f.channel.ID, member.JoinPts, 100)
	if err != nil {
		t.Fatalf("build channel updates: %v", err)
	}
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("updates result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		if pinned, ok := update.(*tg.UpdatePinnedChannelMessages); ok {
			t.Errorf("updates exposed floored pin id %v", pinned.Messages)
		}
	}
}

func TestChannelPinMutationResponseOmitsFlooredMessageID(t *testing.T) {
	t.Parallel()
	f := newChannelReadFloorFixture(t, "225109", "Floor pin mutation response")
	hidden := f.post(t, "hidden pin target", 225190)
	channelExec(t, context.Background(), f.dsn,
		`UPDATE channel_participants SET available_min_id = $1 WHERE channel_id = $2 AND user_id = $3`,
		hidden.LocalID, f.channel.ID, f.creator.ID,
	)

	assertPinResponse := func(name string, result bin.Encoder, wantPinned bool, wantIDs []int) {
		t.Helper()
		updates, ok := result.(*tg.Updates)
		if !ok {
			t.Fatalf("%s response = %T, want *tg.Updates", name, result)
		}
		for _, update := range updates.Updates {
			pin, ok := update.(*tg.UpdatePinnedChannelMessages)
			if !ok {
				continue
			}
			if pin.Pinned != wantPinned {
				t.Errorf("%s pinned = %t, want %t", name, pin.Pinned, wantPinned)
			}
			if len(pin.Messages) != len(wantIDs) {
				t.Errorf("%s message ids = %v, want %v", name, pin.Messages, wantIDs)
				return
			}
			for i, id := range wantIDs {
				if pin.Messages[i] != id {
					t.Errorf("%s message ids = %v, want %v", name, pin.Messages, wantIDs)
					return
				}
			}
			if pin.PtsCount != 1 {
				t.Errorf("%s pts_count = %d, want 1", name, pin.PtsCount)
			}
			return
		}
		t.Errorf("%s response has no UpdatePinnedChannelMessages", name)
	}

	peer := api.InputPeerChannel(f.creator.ID, f.channel.ID)
	result, err := api.UpdatePinnedMessageForTest(f.s, f.creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer, ID: int(hidden.LocalID),
	})
	if err != nil {
		t.Fatalf("pin floored post: %v", err)
	}
	assertPinResponse("pin floored post", result, true, nil)

	result, err = api.UpdatePinnedMessageForTest(f.s, f.creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer, Unpin: true,
	})
	if err != nil {
		t.Fatalf("unpin floored post: %v", err)
	}
	assertPinResponse("unpin floored post", result, false, nil)

	visible := f.post(t, "visible pin target", 225191)
	result, err = api.UpdatePinnedMessageForTest(f.s, f.creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer, ID: int(visible.LocalID),
	})
	if err != nil {
		t.Fatalf("pin visible post: %v", err)
	}
	assertPinResponse("pin visible post", result, true, []int{int(visible.LocalID)})

	result, err = api.UpdatePinnedMessageForTest(f.s, f.creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer, Unpin: true,
	})
	if err != nil {
		t.Fatalf("unpin visible post: %v", err)
	}
	assertPinResponse("unpin visible post", result, false, []int{int(visible.LocalID)})
}
