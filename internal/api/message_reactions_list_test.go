package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

type messageReactionsListFixture struct {
	s        *store.Store
	dsn      string
	creator  store.User
	a        store.User
	b        store.User
	outsider store.User
	chat     store.Chat
	message  store.Message
}

func newMessageReactionsListFixture(t *testing.T) messageReactionsListFixture {
	t.Helper()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551924001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	a, err := s.CreateUser(ctx, "+15551924002")
	if err != nil {
		t.Fatalf("create reactor A: %v", err)
	}
	b, err := s.CreateUser(ctx, "+15551924003")
	if err != nil {
		t.Fatalf("create reactor B: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551924004")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Reaction list", []int64{a.ID, b.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message, _, _, err := s.SendChatMessage(ctx, store.FanOut{
		ChatID:   chat.ID,
		FromID:   creator.ID,
		Text:     "react to this",
		RandomID: 992401,
	})
	if err != nil {
		t.Fatalf("send chat message: %v", err)
	}
	return messageReactionsListFixture{
		s: s, dsn: dsn, creator: creator, a: a, b: b, outsider: outsider, chat: chat, message: message,
	}
}

func callMessageReactionsList(
	t *testing.T,
	s *store.Store,
	userID int64,
	req *tg.MessagesGetMessageReactionsListRequest,
) (*tg.MessagesMessageReactionsList, *mt.RPCError) {
	t.Helper()
	body := dispatchChatMethod(t, fullChannelDispatcher(s), userID, "messages.getMessageReactionsList", req)
	var result tg.MessagesMessageReactionsList
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &result, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getMessageReactionsList response as list or RPCError: %v", err)
	}
	return nil, &rpc
}

func chatMessageCopy(t *testing.T, s *store.Store, ownerID, chatID, fanoutID int64) store.Message {
	t.Helper()
	messages, err := s.History(context.Background(), ownerID, store.PeerTypeChat, chatID, 0, 50)
	if err != nil {
		t.Fatalf("read chat history for owner %d: %v", ownerID, err)
	}
	for _, message := range messages {
		if message.FanoutID == fanoutID {
			return message
		}
	}
	t.Fatalf("owner %d history omitted fanout %d", ownerID, fanoutID)
	return store.Message{}
}

func privateMessageCopy(t *testing.T, s *store.Store, ownerID, peerID int64, text string) store.Message {
	t.Helper()
	messages, err := s.History(context.Background(), ownerID, store.PeerTypeUser, peerID, 0, 50)
	if err != nil {
		t.Fatalf("read private history for owner %d: %v", ownerID, err)
	}
	for _, message := range messages {
		if message.Text == text {
			return message
		}
	}
	t.Fatalf("owner %d history omitted private message %q", ownerID, text)
	return store.Message{}
}

func execReactionListSQL(t *testing.T, dsn, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for reaction-list fixture mutation: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close reaction-list fixture mutation connection: %v", err)
		}
	}()
	if _, err := conn.Exec(ctx, query, args...); err != nil {
		t.Fatalf("mutate reaction-list fixture: %v", err)
	}
}

func requireReactionListError(t *testing.T, s *store.Store, userID int64, req *tg.MessagesGetMessageReactionsListRequest, want string) {
	t.Helper()
	result, rpc := callMessageReactionsList(t, s, userID, req)
	if result != nil {
		t.Fatalf("error %s returned a reaction list: %#v", want, result)
	}
	if rpc == nil || rpc.ErrorMessage != want {
		t.Fatalf("reaction-list error = %v, want %s", rpc, want)
	}
}

func reactInMessageListChat(t *testing.T, f messageReactionsListFixture, reactor store.User, reaction string) {
	t.Helper()
	message := chatMessageCopy(t, f.s, reactor.ID, f.chat.ID, f.message.FanoutID)
	updates, rpc := sendReaction(t, f.s, reactor.ID, api.InputPeerChat(reactor.ID, f.chat.ID), message.LocalID, &tg.ReactionEmoji{Emoticon: reaction})
	requireReactionUpdates(t, updates, rpc, "messages.sendReaction")
}

func messageReactionsListRequest(peer tg.InputPeerClass, messageID, limit int) *tg.MessagesGetMessageReactionsListRequest {
	return &tg.MessagesGetMessageReactionsListRequest{Peer: peer, ID: messageID, Limit: limit}
}

func assertReactionListContains(t *testing.T, result *tg.MessagesMessageReactionsList, userID int64, emoticon string) {
	t.Helper()
	if result == nil || len(result.Reactions) != 1 {
		t.Fatalf("reaction list = %#v, want one reaction", result)
	}
	reaction := result.Reactions[0]
	peer, ok := reaction.PeerID.(*tg.PeerUser)
	if !ok || peer.UserID != userID {
		t.Errorf("reactor peer = %#v, want user %d", reaction.PeerID, userID)
	}
	emoji, ok := reaction.Reaction.(*tg.ReactionEmoji)
	if !ok || emoji.Emoticon != emoticon {
		t.Errorf("reactor reaction = %#v, want %q", reaction.Reaction, emoticon)
	}
	if reaction.Date <= 0 {
		t.Errorf("reactor date = %d, want a server timestamp", reaction.Date)
	}
	if len(result.Users) != 1 {
		t.Errorf("users = %d, want only the reactor on this page", len(result.Users))
	} else if user, ok := result.Users[0].(*tg.User); !ok || user.ID != userID {
		t.Errorf("user vector = %#v, want reactor %d", result.Users[0], userID)
	} else if phone, present := user.GetPhone(); present && phone != "" {
		t.Errorf("reactor phone = %q, want private", phone)
	}
}

func TestGetMessageReactionsListBasicGroupPagingAndFilter(t *testing.T) {
	t.Parallel()
	f := newMessageReactionsListFixture(t)
	reactInMessageListChat(t, f, f.a, "👍")
	reactInMessageListChat(t, f, f.b, "❤")
	peer := api.InputPeerChat(f.creator.ID, f.chat.ID)

	first, rpc := callMessageReactionsList(t, f.s, f.creator.ID, messageReactionsListRequest(peer, int(f.message.LocalID), 1))
	if rpc != nil {
		t.Fatalf("first page returned %s", rpc.ErrorMessage)
	}
	if first.Count != 2 || len(first.Reactions) != 1 {
		t.Fatalf("first page count/results = %d/%d, want 2/1", first.Count, len(first.Reactions))
	}
	firstOffset, hasNext := first.GetNextOffset()
	if !hasNext || firstOffset == "" {
		t.Fatal("first page omitted next_offset")
	}

	secondRequest := messageReactionsListRequest(peer, int(f.message.LocalID), 1)
	secondRequest.SetOffset(firstOffset)
	second, rpc := callMessageReactionsList(t, f.s, f.creator.ID, secondRequest)
	if rpc != nil {
		t.Fatalf("second page returned %s", rpc.ErrorMessage)
	}
	if second.Count != 2 || len(second.Reactions) != 1 {
		t.Fatalf("second page count/results = %d/%d, want 2/1", second.Count, len(second.Reactions))
	}
	if _, hasNext := second.GetNextOffset(); hasNext {
		t.Fatal("last page has next_offset")
	}
	firstPeer, firstOK := first.Reactions[0].PeerID.(*tg.PeerUser)
	secondPeer, secondOK := second.Reactions[0].PeerID.(*tg.PeerUser)
	if !firstOK || !secondOK || firstPeer.UserID == secondPeer.UserID {
		t.Fatalf("pages repeated or omitted a reactor: first=%#v second=%#v", first.Reactions, second.Reactions)
	}
	gotReactors := map[int64]bool{firstPeer.UserID: true, secondPeer.UserID: true}
	if !gotReactors[f.a.ID] || !gotReactors[f.b.ID] {
		t.Fatalf("page reactors = %v, want %d and %d", gotReactors, f.a.ID, f.b.ID)
	}

	filteredRequest := messageReactionsListRequest(peer, int(f.message.LocalID), 50)
	filteredRequest.SetReaction(&tg.ReactionEmoji{Emoticon: "👍"})
	filtered, rpc := callMessageReactionsList(t, f.s, f.creator.ID, filteredRequest)
	if rpc != nil {
		t.Fatalf("filtered list returned %s", rpc.ErrorMessage)
	}
	if filtered.Count != 1 {
		t.Fatalf("filtered count = %d, want 1", filtered.Count)
	}
	assertReactionListContains(t, filtered, f.a.ID, "👍")

	history, err := api.GetHistoryForTest(f.s, f.creator.ID, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
	if err != nil {
		t.Fatalf("read group history: %v", err)
	}
	page, ok := history.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("history response = %T, want *tg.MessagesMessages", history)
	}
	var foundListFlag bool
	for _, item := range page.Messages {
		message, ok := item.(*tg.Message)
		if !ok || message.ID != int(f.message.LocalID) {
			continue
		}
		reactions, present := message.GetReactions()
		if !present || !reactions.GetCanSeeList() {
			t.Fatalf("basic group message reactions = %#v, want can_see_list", reactions)
		}
		foundListFlag = true
	}
	if !foundListFlag {
		t.Fatal("group history omitted the reaction message")
	}
}

func TestGetMessageReactionsListRejectsMalformedOffsetAndOversizedLimit(t *testing.T) {
	t.Parallel()
	f := newMessageReactionsListFixture(t)
	peer := api.InputPeerChat(f.outsider.ID, f.chat.ID)
	malformed := messageReactionsListRequest(peer, int(f.message.LocalID), 1)
	malformed.SetOffset("not-a-cursor")
	if _, rpc := callMessageReactionsList(t, f.s, f.outsider.ID, malformed); rpc == nil || rpc.ErrorMessage != "OFFSET_INVALID" {
		t.Fatalf("malformed offset error = %v, want OFFSET_INVALID", rpc)
	}
	for _, limit := range []int{0, 101} {
		if _, rpc := callMessageReactionsList(t, f.s, f.outsider.ID, messageReactionsListRequest(peer, int(f.message.LocalID), limit)); rpc == nil || rpc.ErrorMessage != "LIMIT_INVALID" {
			t.Fatalf("limit %d error = %v, want LIMIT_INVALID before membership check", limit, rpc)
		}
	}
	invalidFilter := messageReactionsListRequest(peer, int(f.message.LocalID), 1)
	invalidFilter.SetReaction(&tg.ReactionCustomEmoji{DocumentID: 1})
	requireReactionListError(t, f.s, f.creator.ID, invalidFilter, "REACTION_INVALID")
	if _, rpc := callMessageReactionsList(t, f.s, 0, messageReactionsListRequest(peer, int(f.message.LocalID), 1)); rpc == nil || rpc.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated call error = %v, want AUTH_KEY_UNREGISTERED", rpc)
	}
}

func TestGetMessageReactionsListRequiresCurrentBasicGroupMembership(t *testing.T) {
	t.Parallel()
	f := newMessageReactionsListFixture(t)
	reactInMessageListChat(t, f, f.a, "👍")
	peer := api.InputPeerChat(f.outsider.ID, f.chat.ID)
	requireReactionListError(t, f.s, f.outsider.ID, messageReactionsListRequest(peer, int(f.message.LocalID), 50), "PEER_ID_INVALID")

	kickedCopy := chatMessageCopy(t, f.s, f.b.ID, f.chat.ID, f.message.FanoutID)
	if _, _, _, err := f.s.RemoveChatUser(context.Background(), f.chat.ID, f.b.ID, f.creator.ID); err != nil {
		t.Fatalf("remove reactor from group: %v", err)
	}
	var keptRows int
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to verify kicked reaction row: %v", err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM message_reactions WHERE owner_id=$1 AND local_id=$2`, f.b.ID, kickedCopy.LocalID).Scan(&keptRows); err != nil {
		t.Fatalf("count kicked member reaction copy: %v", err)
	}
	if err := conn.Close(ctx); err != nil {
		t.Errorf("close kicked reaction check: %v", err)
	}
	if keptRows == 0 {
		t.Fatal("kicked member fixture no longer has its retained reaction copy")
	}
	requireReactionListError(t, f.s, f.b.ID,
		messageReactionsListRequest(api.InputPeerChat(f.b.ID, f.chat.ID), int(kickedCopy.LocalID), 50), "PEER_ID_INVALID")
}

func TestGetMessageReactionsListRejectsHiddenOrWrongPeerMessages(t *testing.T) {
	t.Parallel()
	f := newMessageReactionsListFixture(t)
	reactInMessageListChat(t, f, f.a, "👍")
	groupPeer := api.InputPeerChat(f.creator.ID, f.chat.ID)

	private, senderPts, recipientPts, duplicate, err := f.s.SendMessage(context.Background(), f.creator.ID, f.outsider.ID, "private target", 9924011, 0, 0)
	if err != nil {
		t.Fatalf("send private target (sender pts %d, recipient pts %d, duplicate %t): %v", senderPts, recipientPts, duplicate, err)
	}
	privateCopy := privateMessageCopy(t, f.s, f.outsider.ID, f.creator.ID, "private target")
	if _, err := f.s.SendReaction(context.Background(), f.outsider.ID, privateCopy.LocalID, "🔥"); err != nil {
		t.Fatalf("react to private target: %v", err)
	}
	requireReactionListError(t, f.s, f.creator.ID,
		messageReactionsListRequest(groupPeer, int(private.LocalID), 50), "MSG_ID_INVALID")

	deleted := chatMessageCopy(t, f.s, f.creator.ID, f.chat.ID, f.message.FanoutID)
	if _, err := f.s.SendReaction(context.Background(), f.a.ID, chatMessageCopy(t, f.s, f.a.ID, f.chat.ID, f.message.FanoutID).LocalID, "❤"); err != nil {
		t.Fatalf("add deleted-message reaction: %v", err)
	}
	execReactionListSQL(t, f.dsn, `UPDATE messages SET deleted=true WHERE owner_id=$1 AND local_id=$2`, f.creator.ID, deleted.LocalID)
	requireReactionListError(t, f.s, f.creator.ID, messageReactionsListRequest(groupPeer, int(deleted.LocalID), 50), "MSG_ID_INVALID")

	expiring, _, _, err := f.s.SendChatMessage(context.Background(), store.FanOut{
		ChatID: f.chat.ID, FromID: f.creator.ID, Text: "expired target", RandomID: 9924012,
	})
	if err != nil {
		t.Fatalf("send expiring target: %v", err)
	}
	if _, err := f.s.SendReaction(context.Background(), f.b.ID, chatMessageCopy(t, f.s, f.b.ID, f.chat.ID, expiring.FanoutID).LocalID, "🔥"); err != nil {
		t.Fatalf("add expired-message reaction: %v", err)
	}
	execReactionListSQL(t, f.dsn, `UPDATE messages SET expires_at=statement_timestamp()-interval '1 second' WHERE owner_id=$1 AND local_id=$2`, f.creator.ID, expiring.LocalID)
	requireReactionListError(t, f.s, f.creator.ID, messageReactionsListRequest(groupPeer, int(expiring.LocalID), 50), "MSG_ID_INVALID")
}

func TestGetMessageReactionsListMegagroupVisibilityAndListFlag(t *testing.T) {
	t.Parallel()
	f := newChannelReactionFixture(t, true)
	peer := api.InputPeerChannel(f.creator.ID, f.channel.ID)
	for _, reactor := range []struct {
		user     store.User
		reaction string
	}{{f.a, "👍"}, {f.b, "❤"}} {
		updates, rpc := sendReaction(t, f.s, reactor.user.ID, api.InputPeerChannel(reactor.user.ID, f.channel.ID), f.post.LocalID, &tg.ReactionEmoji{Emoticon: reactor.reaction})
		requireReactionUpdates(t, updates, rpc, "messages.sendReaction")
	}
	result, rpc := callMessageReactionsList(t, f.s, f.creator.ID, messageReactionsListRequest(peer, int(f.post.LocalID), 50))
	if rpc != nil {
		t.Fatalf("megagroup reaction list returned %s", rpc.ErrorMessage)
	}
	if result.Count != 2 || len(result.Reactions) != 2 {
		t.Fatalf("megagroup list count/results = %d/%d, want 2/2", result.Count, len(result.Reactions))
	}
	gotReactors := make(map[int64]bool, len(result.Reactions))
	for _, reaction := range result.Reactions {
		if reaction.Date <= 0 {
			t.Errorf("megagroup reactor date = %d, want a server timestamp", reaction.Date)
		}
		if user, ok := reaction.PeerID.(*tg.PeerUser); ok {
			gotReactors[user.UserID] = true
		}
	}
	if !gotReactors[f.a.ID] || !gotReactors[f.b.ID] || len(result.Users) != 2 {
		t.Fatalf("megagroup reactors/users = %v/%#v, want A and B", gotReactors, result.Users)
	}

	filteredRequest := messageReactionsListRequest(peer, int(f.post.LocalID), 50)
	filteredRequest.SetReaction(&tg.ReactionEmoji{Emoticon: "👍"})
	filtered, rpc := callMessageReactionsList(t, f.s, f.creator.ID, filteredRequest)
	if rpc != nil {
		t.Fatalf("filtered megagroup list returned %s", rpc.ErrorMessage)
	}
	assertReactionListContains(t, filtered, f.a.ID, "👍")

	history, err := api.GetHistoryForTest(f.s, f.creator.ID, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
	if err != nil {
		t.Fatalf("read megagroup history: %v", err)
	}
	page, ok := history.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("megagroup history response = %T, want *tg.MessagesChannelMessages", history)
	}
	var foundListFlag bool
	for _, item := range page.Messages {
		message, ok := item.(*tg.Message)
		if !ok || message.ID != int(f.post.LocalID) {
			continue
		}
		reactions, present := message.GetReactions()
		if !present || !reactions.GetCanSeeList() {
			t.Fatalf("megagroup message reactions = %#v, want can_see_list", reactions)
		}
		foundListFlag = true
	}
	if !foundListFlag {
		t.Fatal("megagroup history omitted the reaction post")
	}

	outsider, err := f.s.CreateUser(context.Background(), "+15551924005")
	if err != nil {
		t.Fatalf("create megagroup outsider: %v", err)
	}
	requireReactionListError(t, f.s, outsider.ID,
		messageReactionsListRequest(api.InputPeerChannel(outsider.ID, f.channel.ID), int(f.post.LocalID), 50), "PEER_ID_INVALID")
	if err := f.s.SetChannelBan(context.Background(), f.channel.ID, f.creator.ID, f.b.ID, nil, true); err != nil {
		t.Fatalf("ban megagroup reactor: %v", err)
	}
	requireReactionListError(t, f.s, f.b.ID,
		messageReactionsListRequest(api.InputPeerChannel(f.b.ID, f.channel.ID), int(f.post.LocalID), 50), "PEER_ID_INVALID")

	execReactionListSQL(t, f.dsn, `UPDATE channel_participants SET available_min_id=$1 WHERE channel_id=$2 AND user_id=$3`, f.post.LocalID, f.channel.ID, f.creator.ID)
	requireReactionListError(t, f.s, f.creator.ID, messageReactionsListRequest(peer, int(f.post.LocalID), 50), "MSG_ID_INVALID")
}

func TestGetMessageReactionsListRefusesBroadcastAndPrivatePeers(t *testing.T) {
	t.Parallel()
	broadcast := newChannelReactionFixture(t, false)
	updates, rpc := sendReaction(t, broadcast.s, broadcast.a.ID, api.InputPeerChannel(broadcast.a.ID, broadcast.channel.ID), broadcast.post.LocalID, &tg.ReactionEmoji{Emoticon: "👍"})
	requireReactionUpdates(t, updates, rpc, "messages.sendReaction")
	requireReactionListError(t, broadcast.s, broadcast.creator.ID,
		messageReactionsListRequest(api.InputPeerChannel(broadcast.creator.ID, broadcast.channel.ID), int(broadcast.post.LocalID), 50), "BROADCAST_FORBIDDEN")

	broadcastHistory, err := api.GetHistoryForTest(broadcast.s, broadcast.creator.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChannel(broadcast.creator.ID, broadcast.channel.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("read broadcast history: %v", err)
	}
	broadcastPage, ok := broadcastHistory.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("broadcast history response = %T, want *tg.MessagesChannelMessages", broadcastHistory)
	}
	for _, item := range broadcastPage.Messages {
		message, ok := item.(*tg.Message)
		if !ok || message.ID != int(broadcast.post.LocalID) {
			continue
		}
		reactions, present := message.GetReactions()
		if !present || reactions.GetCanSeeList() {
			t.Fatalf("broadcast message reactions = %#v, want can_see_list unset", reactions)
		}
		goto broadcastMessageChecked
	}
	t.Fatal("broadcast history omitted the reaction post")

broadcastMessageChecked:
	f := newMessageReactionsListFixture(t)
	private, senderPts, recipientPts, duplicate, err := f.s.SendMessage(context.Background(), f.creator.ID, f.outsider.ID, "private reaction", 9924013, 0, 0)
	if err != nil {
		t.Fatalf("send private reaction target (sender pts %d, recipient pts %d, duplicate %t): %v", senderPts, recipientPts, duplicate, err)
	}
	privateCopy := privateMessageCopy(t, f.s, f.outsider.ID, f.creator.ID, "private reaction")
	if _, err := f.s.SendReaction(context.Background(), f.outsider.ID, privateCopy.LocalID, "👍"); err != nil {
		t.Fatalf("react to private message: %v", err)
	}
	privatePeer := api.InputPeerUser(f.creator.ID, f.outsider.ID)
	requireReactionListError(t, f.s, f.creator.ID,
		messageReactionsListRequest(privatePeer, int(private.LocalID), 50), "PEER_ID_INVALID")

	updatesEncoder, err := api.GetMessagesReactionsForTest(f.s, f.creator.ID, &tg.MessagesGetMessagesReactionsRequest{
		Peer: privatePeer, ID: []int{int(private.LocalID)},
	})
	if err != nil {
		t.Fatalf("read private reaction update: %v", err)
	}
	updates, ok = updatesEncoder.(*tg.Updates)
	if !ok || len(updates.Updates) != 1 {
		t.Fatalf("private reaction updates = %#v, want one update", updatesEncoder)
	}
	reactionUpdate, ok := updates.Updates[0].(*tg.UpdateMessageReactions)
	if !ok || reactionUpdate.Reactions.GetCanSeeList() {
		t.Fatalf("private message reactions = %#v, want can_see_list unset", reactionUpdate)
	}
}
