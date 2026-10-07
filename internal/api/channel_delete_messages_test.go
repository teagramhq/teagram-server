package api_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func dispatchChannelDeleteMessages(
	t *testing.T,
	ctx context.Context,
	h mtproto.Handler,
	userID int64,
	request *tg.ChannelsDeleteMessagesRequest,
) ([]byte, error) {
	t.Helper()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		return nil, err
	}
	const msgID = int64(1 << 32)
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID: key.ID,
		UserID:    userID,
		MsgID:     msgID,
		Buf:       &body,
		Ctx:       ctx,
	}); err != nil {
		return nil, err
	}
	return transport.result(t, key, msgID), nil
}

func decodeChannelDeleteMessagesResponse(body []byte) (*tg.MessagesAffectedMessages, *mt.RPCError, error) {

	var affected tg.MessagesAffectedMessages
	if err := affected.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &affected, nil, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		return nil, nil, err
	}
	return nil, &rpc, nil
}

func deleteChannelMessagesViaDispatcher(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	request *tg.ChannelsDeleteMessagesRequest,
) (*tg.MessagesAffectedMessages, *mt.RPCError) {
	t.Helper()
	body, err := dispatchChannelDeleteMessages(t, context.Background(), h, userID, request)
	if err != nil {
		t.Fatalf("dispatch channels.deleteMessages: %v", err)
	}
	affected, rpc, err := decodeChannelDeleteMessagesResponse(body)
	if err != nil {
		t.Fatalf("decode channels.deleteMessages response: %v", err)
	}
	return affected, rpc
}

func limitedChannelDeleteDispatcher(s *store.Store, limit int) mtproto.Handler {
	return api.New(
		s, 2, &tg.Config{MeURLPrefix: testPublicLinkPrefix}, slog.New(slog.DiscardHandler), false,
		api.TestMaxFileBytes, nil, 1, pgtest.PeerDeriver(),
		config.RateLimitsConfig{MessageSend: store.RateLimitConfig{Limit: limit, Window: time.Hour}},
		config.RegistrationInvite,
	)
}

func TestChannelsDeleteMessagesRemovesPostAndAdvancesChannelPts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551294801")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Delete post", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "remove this post", 94801, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel message: duplicate=%v err=%v", duplicate, err)
	}
	foreignChannel, err := s.CreateChannel(ctx, creator.ID, "Foreign id namespace", "", false)
	if err != nil {
		t.Fatalf("create second channel: %v", err)
	}
	if _, _, duplicate, err := s.PostChannelMessage(ctx, foreignChannel.ID, creator.ID, "foreign first", 94802, nil, 0); err != nil || duplicate {
		t.Fatalf("post first foreign-channel message: duplicate=%v err=%v", duplicate, err)
	}
	foreignPost, _, duplicate, err := s.PostChannelMessage(ctx, foreignChannel.ID, creator.ID, "foreign second", 94803, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post second foreign-channel message: duplicate=%v err=%v", duplicate, err)
	}

	h := fullChannelDispatcher(s)
	request := &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(creator.ID, channel.ID),
		ID:      []int{int(post.LocalID), int(post.LocalID), int(foreignPost.LocalID), 0, -1, 999999},
	}
	response, rpc := deleteChannelMessagesViaDispatcher(t, h, creator.ID, request)
	if rpc != nil {
		t.Fatalf("channels.deleteMessages: RPC %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if response.Pts != 3 || response.PtsCount != 1 {
		t.Fatalf("affected messages = {pts:%d pts_count:%d}, want {3,1}", response.Pts, response.PtsCount)
	}

	messages, err := s.ChannelMessages(ctx, channel.ID, []int64{post.LocalID})
	if err != nil {
		t.Fatalf("read deleted channel post: %v", err)
	}
	if got := messages[post.LocalID]; !got.Deleted || got.Message != "" {
		t.Fatalf("deleted post = %+v, want a payload-free tombstone", got)
	}
	events, err := s.ChannelEventsWindow(ctx, channel.ID, 2, 3, 10)
	if err != nil {
		t.Fatalf("read delete event: %v", err)
	}
	if len(events) != 1 || events[0].Pts != 3 || events[0].Type != store.EventDelete || events[0].LocalID != post.LocalID {
		t.Fatalf("delete events = %+v, want one delete event at pts 3 for post %d", events, post.LocalID)
	}
	foreignMessages, err := s.ChannelMessages(ctx, foreignChannel.ID, []int64{foreignPost.LocalID})
	if err != nil || foreignMessages[foreignPost.LocalID].Deleted {
		t.Fatalf("foreign-channel post after local id request = %+v err=%v, want live", foreignMessages[foreignPost.LocalID], err)
	}
	difference, err := api.GetChannelDifferenceForTest(s, creator.ID, &tg.UpdatesGetChannelDifferenceRequest{
		Channel: api.InputChannel(creator.ID, channel.ID), Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: 2, Limit: 10,
	})
	if err != nil {
		t.Fatalf("get channel difference after delete: %v", err)
	}
	diff, ok := difference.(*tg.UpdatesChannelDifference)
	if !ok || diff.Pts != 3 || len(diff.NewMessages) != 0 || len(diff.OtherUpdates) != 1 {
		t.Fatalf("channel difference after delete = %#v, want final pts 3 and one delete update", difference)
	}
	deletedUpdate, ok := diff.OtherUpdates[0].(*tg.UpdateDeleteChannelMessages)
	if !ok || deletedUpdate.ChannelID != channel.ID || deletedUpdate.Pts != 3 || deletedUpdate.PtsCount != 1 ||
		len(deletedUpdate.Messages) != 1 || deletedUpdate.Messages[0] != int(post.LocalID) {
		t.Fatalf("channel difference delete update = %#v, want post %d at pts 3/count 1", diff.OtherUpdates[0], post.LocalID)
	}

	replayed, rpc := deleteChannelMessagesViaDispatcher(t, h, creator.ID, request)
	if rpc != nil {
		t.Fatalf("replay channels.deleteMessages: RPC %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if replayed.Pts != 3 || replayed.PtsCount != 0 {
		t.Fatalf("replayed affected messages = {pts:%d pts_count:%d}, want {3,0}", replayed.Pts, replayed.PtsCount)
	}
	noOps, rpc := deleteChannelMessagesViaDispatcher(t, h, creator.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(creator.ID, channel.ID), ID: []int{0, -1, 999999},
	})
	if rpc != nil || noOps.Pts != 3 || noOps.PtsCount != 0 {
		t.Fatalf("missing/nonpositive delete = %+v RPC %v, want pts 3/count 0", noOps, rpc)
	}
	events, err = s.ChannelEventsWindow(ctx, channel.ID, 2, 3, 10)
	if err != nil || len(events) != 1 || events[0].Type != store.EventDelete || events[0].LocalID != post.LocalID {
		t.Fatalf("events after idempotent delete = %+v err=%v, want the original delete only", events, err)
	}
}

func TestMegagroupMemberCannotPartiallyDeleteAnotherMembersPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551294811")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551294812")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551294813")
	if err != nil {
		t.Fatalf("create other member: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Member delete", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannel(t, ctx, dsn, channel.ID, member.ID)
	joinChannel(t, ctx, dsn, channel.ID, other.ID)
	ownPost, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, member.ID, "own", 94811, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post own message: duplicate=%v err=%v", duplicate, err)
	}
	otherPost, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, other.ID, "other", 94812, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post other message: duplicate=%v err=%v", duplicate, err)
	}
	h := fullChannelDispatcher(s)
	_, rpc := deleteChannelMessagesViaDispatcher(t, h, member.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		ID:      []int{int(ownPost.LocalID), int(otherPost.LocalID)},
	})
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_DELETE_FORBIDDEN" || rpc.ErrorCode != 403 {
		t.Fatalf("mixed member delete RPC = %v, want 403 MESSAGE_DELETE_FORBIDDEN", rpc)
	}
	messages, err := s.ChannelMessages(ctx, channel.ID, []int64{ownPost.LocalID, otherPost.LocalID})
	if err != nil {
		t.Fatalf("read channel posts: %v", err)
	}
	if messages[ownPost.LocalID].Deleted || messages[otherPost.LocalID].Deleted {
		t.Fatalf("unauthorized batch partially deleted posts: %+v", messages)
	}
	if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != 3 {
		t.Fatalf("channel pts after refused batch = %d, err %v; want 3", pts, err)
	}

	deleted, rpc := deleteChannelMessagesViaDispatcher(t, h, member.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID),
		ID:      []int{int(ownPost.LocalID)},
	})
	if rpc != nil || deleted.Pts != 4 || deleted.PtsCount != 1 {
		t.Fatalf("member own-post delete = %+v RPC %v, want pts 4/count 1", deleted, rpc)
	}
	moderated, rpc := deleteChannelMessagesViaDispatcher(t, h, creator.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(creator.ID, channel.ID), ID: []int{int(otherPost.LocalID)},
	})
	if rpc != nil || moderated.Pts != 5 || moderated.PtsCount != 1 {
		t.Fatalf("creator delete of another member post = %+v RPC %v, want pts 5/count 1", moderated, rpc)
	}
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, member.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(member.ID, channel.ID), ID: []int{1},
	})
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_DELETE_FORBIDDEN" || rpc.ErrorCode != 403 {
		t.Fatalf("service-message delete RPC = %v, want 403 MESSAGE_DELETE_FORBIDDEN", rpc)
	}
}

func TestBroadcastFormerAdminCannotDeleteOwnPost(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551294821")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551294822")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Broadcast delete", "", false)
	if err != nil {
		t.Fatalf("create broadcast: %v", err)
	}
	joinChannel(t, ctx, dsn, channel.ID, admin.ID)
	channelExec(t, ctx, dsn, `UPDATE channel_participants SET role = 1 WHERE channel_id = $1 AND user_id = $2`, channel.ID, admin.ID)
	creatorPost, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "creator post", 94820, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post as creator: duplicate=%v err=%v", duplicate, err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, admin.ID, "former admin post", 94821, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post as admin: duplicate=%v err=%v", duplicate, err)
	}
	adminDelete, rpc := deleteChannelMessagesViaDispatcher(t, fullChannelDispatcher(s), admin.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(admin.ID, channel.ID), ID: []int{int(creatorPost.LocalID)},
	})
	if rpc != nil || adminDelete.Pts != 4 || adminDelete.PtsCount != 1 {
		t.Fatalf("current admin delete of another author's post = %+v RPC %v, want pts 4/count 1", adminDelete, rpc)
	}
	channelExec(t, ctx, dsn, `UPDATE channel_participants SET role = 0 WHERE channel_id = $1 AND user_id = $2`, channel.ID, admin.ID)

	_, rpc = deleteChannelMessagesViaDispatcher(t, fullChannelDispatcher(s), admin.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(admin.ID, channel.ID), ID: []int{int(post.LocalID)},
	})
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_DELETE_FORBIDDEN" || rpc.ErrorCode != 403 {
		t.Fatalf("former admin delete RPC = %v, want 403 MESSAGE_DELETE_FORBIDDEN", rpc)
	}
	messages, err := s.ChannelMessages(ctx, channel.ID, []int64{post.LocalID})
	if err != nil || messages[post.LocalID].Deleted {
		t.Fatalf("former admin post after refused delete = %+v err=%v, want live", messages[post.LocalID], err)
	}
	if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != 4 {
		t.Fatalf("channel pts after former admin refusal = %d err=%v, want 4", pts, err)
	}
}

func TestChannelsDeleteMessagesValidatesBeforeChannelLookup(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	h := fullChannelDispatcher(s)
	invalidChannel := &tg.InputChannelEmpty{}

	_, rpc := deleteChannelMessagesViaDispatcher(t, h, 0, &tg.ChannelsDeleteMessagesRequest{Channel: invalidChannel})
	if rpc == nil || rpc.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unauthenticated delete RPC = %v, want AUTH_KEY_UNREGISTERED", rpc)
	}
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, 1, &tg.ChannelsDeleteMessagesRequest{Channel: invalidChannel})
	if rpc == nil || rpc.ErrorMessage != "MESSAGE_ID_INVALID" {
		t.Fatalf("empty delete RPC = %v, want MESSAGE_ID_INVALID", rpc)
	}
	tooMany := make([]int, 101)
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, 1, &tg.ChannelsDeleteMessagesRequest{Channel: invalidChannel, ID: tooMany})
	if rpc == nil || rpc.ErrorMessage != "LIMIT_INVALID" {
		t.Fatalf("oversized delete RPC = %v, want LIMIT_INVALID", rpc)
	}
}

func TestNonmemberChannelDeleteErrorsDoNotRevealChannelExistence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551294831")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551294832")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Private delete", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "private", 94831, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel message: duplicate=%v err=%v", duplicate, err)
	}
	h := fullChannelDispatcher(s)
	request := func(channelID int64) *tg.ChannelsDeleteMessagesRequest {
		return &tg.ChannelsDeleteMessagesRequest{Channel: api.InputChannel(outsider.ID, channelID), ID: []int{int(post.LocalID)}}
	}
	existing, err := dispatchChannelDeleteMessages(t, ctx, h, outsider.ID, request(channel.ID))
	if err != nil {
		t.Fatalf("dispatch existing-channel delete: %v", err)
	}
	missing, err := dispatchChannelDeleteMessages(t, ctx, h, outsider.ID, request(int64(1<<62)))
	if err != nil {
		t.Fatalf("dispatch missing-channel delete: %v", err)
	}
	if !bytes.Equal(existing, missing) {
		t.Fatalf("nonmember responses differ for existing/missing channels: %x != %x", existing, missing)
	}
	_, rpc, err := decodeChannelDeleteMessagesResponse(existing)
	if err != nil || rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("nonmember channel delete = RPC %v, err %v; want PEER_ID_INVALID", rpc, err)
	}
	wrongHash := api.InputChannel(creator.ID, channel.ID)
	wrongHash.AccessHash = api.InputChannel(outsider.ID, channel.ID).AccessHash
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, creator.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: wrongHash, ID: []int{int(post.LocalID)},
	})
	if rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("wrong-hash channel delete = RPC %v, want PEER_ID_INVALID", rpc)
	}
}

func TestChannelDeleteChargesRateLimitPerRequestedIDAfterMembershipPrecheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551294841")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551294842")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Delete rate", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "rate", 94841, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel message: duplicate=%v err=%v", duplicate, err)
	}
	h := limitedChannelDeleteDispatcher(s, 1)
	request := func(userID int64, ids ...int) *tg.ChannelsDeleteMessagesRequest {
		return &tg.ChannelsDeleteMessagesRequest{Channel: api.InputChannel(userID, channel.ID), ID: ids}
	}
	_, rpc := deleteChannelMessagesViaDispatcher(t, h, outsider.ID, request(outsider.ID, int(post.LocalID)))
	if rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("nonmember delete RPC = %v, want PEER_ID_INVALID", rpc)
	}
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, creator.ID, request(creator.ID, int(post.LocalID)))
	if rpc != nil {
		t.Fatalf("valid delete after nonmember precheck: RPC %v", rpc)
	}

	second, err := s.CreateUser(ctx, "+15551294843")
	if err != nil {
		t.Fatalf("create second user: %v", err)
	}
	secondChannel, err := s.CreateChannel(ctx, second.ID, "Delete rate cost", "", false)
	if err != nil {
		t.Fatalf("create second channel: %v", err)
	}
	secondPost, _, duplicate, err := s.PostChannelMessage(ctx, secondChannel.ID, second.ID, "rate cost", 94842, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post second channel message: duplicate=%v err=%v", duplicate, err)
	}
	_, rpc = deleteChannelMessagesViaDispatcher(t, h, second.ID, &tg.ChannelsDeleteMessagesRequest{
		Channel: api.InputChannel(second.ID, secondChannel.ID), ID: []int{int(secondPost.LocalID), int(secondPost.LocalID)},
	})
	if rpc == nil || !strings.HasPrefix(rpc.ErrorMessage, "FLOOD_WAIT_") {
		t.Fatalf("two-id delete under one-token limit = RPC %v, want FLOOD_WAIT", rpc)
	}
}

func TestConcurrentChannelDeletesEmitOnlyOneDeleteEvent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551294851")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Concurrent delete", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "one delete", 94851, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel message: duplicate=%v err=%v", duplicate, err)
	}
	h := fullChannelDispatcher(s)
	request := &tg.ChannelsDeleteMessagesRequest{Channel: api.InputChannel(creator.ID, channel.ID), ID: []int{int(post.LocalID)}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan struct {
		affected *tg.MessagesAffectedMessages
		rpc      *mt.RPCError
		err      error
	}, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			body, dispatchErr := dispatchChannelDeleteMessages(t, ctx, h, creator.ID, request)
			if dispatchErr != nil {
				results <- struct {
					affected *tg.MessagesAffectedMessages
					rpc      *mt.RPCError
					err      error
				}{err: dispatchErr}
				return
			}
			affected, rpc, decodeErr := decodeChannelDeleteMessagesResponse(body)
			results <- struct {
				affected *tg.MessagesAffectedMessages
				rpc      *mt.RPCError
				err      error
			}{affected: affected, rpc: rpc, err: decodeErr}
		})
	}
	close(start)
	workers.Wait()
	close(results)
	counts := make(map[int]int)
	for result := range results {
		if result.err != nil || result.rpc != nil || result.affected == nil {
			t.Fatalf("concurrent delete result = %+v", result)
		}
		if result.affected.Pts != 3 {
			t.Errorf("concurrent delete pts = %d, want final pts 3", result.affected.Pts)
		}
		counts[result.affected.PtsCount]++
	}
	if counts[1] != 1 || counts[0] != 1 {
		t.Fatalf("concurrent delete pts_count outcomes = %v, want one 1 and one 0", counts)
	}
	events, err := s.ChannelEventsWindow(ctx, channel.ID, 2, 3, 10)
	if err != nil || len(events) != 1 || events[0].Type != store.EventDelete || events[0].LocalID != post.LocalID {
		t.Fatalf("delete events = %+v err=%v, want one delete event", events, err)
	}
}

func TestChannelDeleteRacingPostKeepsChannelPtsGapFree(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551294861")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Delete and post", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "delete while posting", 94861, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel message: duplicate=%v err=%v", duplicate, err)
	}
	h := fullChannelDispatcher(s)
	start := make(chan struct{})
	deleteDone := make(chan struct {
		body []byte
		err  error
	}, 1)
	postDone := make(chan struct {
		pts     int
		message store.ChannelMessage
		err     error
	}, 1)
	go func() {
		<-start
		body, err := dispatchChannelDeleteMessages(t, ctx, h, creator.ID, &tg.ChannelsDeleteMessagesRequest{
			Channel: api.InputChannel(creator.ID, channel.ID), ID: []int{int(post.LocalID)},
		})
		deleteDone <- struct {
			body []byte
			err  error
		}{body: body, err: err}
	}()
	go func() {
		<-start
		message, pts, duplicate, err := s.PostChannelMessage(ctx, channel.ID, creator.ID, "concurrent new post", 94862, nil, 0)
		if duplicate && err == nil {
			err = errors.New("new post unexpectedly deduplicated")
		}
		postDone <- struct {
			pts     int
			message store.ChannelMessage
			err     error
		}{pts: pts, message: message, err: err}
	}()
	close(start)
	deletion := <-deleteDone
	posted := <-postDone
	if deletion.err != nil {
		t.Fatalf("dispatch concurrent delete: %v", deletion.err)
	}
	deleted, rpc, err := decodeChannelDeleteMessagesResponse(deletion.body)
	if err != nil || rpc != nil || deleted == nil || deleted.PtsCount != 1 {
		t.Fatalf("concurrent delete response = %+v RPC %v err %v, want one delete", deleted, rpc, err)
	}
	if posted.err != nil {
		t.Fatalf("concurrent post: %v", posted.err)
	}
	events, err := s.ChannelEventsWindow(ctx, channel.ID, 2, 4, 10)
	if err != nil || len(events) != 2 || events[0].Pts != 3 || events[1].Pts != 4 {
		t.Fatalf("concurrent events = %+v err=%v, want consecutive pts 3 and 4", events, err)
	}
	var sawDelete, sawPost bool
	for _, event := range events {
		switch event.Type {
		case store.EventDelete:
			sawDelete = event.LocalID == post.LocalID
		case store.EventNewMessage:
			sawPost = event.LocalID == posted.message.LocalID
		}
	}
	if !sawDelete || !sawPost || posted.pts != 3 && posted.pts != 4 || deleted.Pts != 3 && deleted.Pts != 4 {
		t.Fatalf("concurrent post/delete outcomes = events %+v post pts %d delete %+v", events, posted.pts, deleted)
	}
	if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != 4 {
		t.Fatalf("channel pts after concurrent post/delete = %d err=%v, want 4", pts, err)
	}
}

func TestChannelDeleteRechecksMembershipAfterMutationCommits(t *testing.T) {
	for i, tc := range []struct {
		name      string
		mutation  string
		wantError string
	}{
		{name: "demotion", mutation: `UPDATE channel_participants SET role = 0 WHERE channel_id = $1 AND user_id = $2`, wantError: "MESSAGE_DELETE_FORBIDDEN"},
		{name: "ban", mutation: `UPDATE channel_participants SET banned_until = now() + interval '1 hour' WHERE channel_id = $1 AND user_id = $2`, wantError: "PEER_ID_INVALID"},
		{name: "leave", mutation: `DELETE FROM channel_participants WHERE channel_id = $1 AND user_id = $2`, wantError: "PEER_ID_INVALID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			s, dsn := openStoreDSN(t)
			creator, err := s.CreateUser(ctx, "+1555129490"+strconv.Itoa(i+1))
			if err != nil {
				t.Fatalf("create creator: %v", err)
			}
			admin, err := s.CreateUser(ctx, "+1555129491"+strconv.Itoa(i+1))
			if err != nil {
				t.Fatalf("create admin: %v", err)
			}
			channel, err := s.CreateChannel(ctx, creator.ID, "Permission race", "", false)
			if err != nil {
				t.Fatalf("create broadcast: %v", err)
			}
			joinChannel(t, ctx, dsn, channel.ID, admin.ID)
			channelExec(t, ctx, dsn, `UPDATE channel_participants SET role = 1 WHERE channel_id = $1 AND user_id = $2`, channel.ID, admin.ID)
			post, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, admin.ID, "permission race", int64(94901+i), nil, 0)
			if err != nil || duplicate {
				t.Fatalf("post as admin: duplicate=%v err=%v", duplicate, err)
			}

			mutationConn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect mutation transaction: %v", err)
			}
			defer func() {
				if err := mutationConn.Close(context.Background()); err != nil {
					t.Errorf("close mutation connection: %v", err)
				}
			}()
			mutationTx, err := mutationConn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin mutation transaction: %v", err)
			}
			defer func() { _ = mutationTx.Rollback(context.Background()) }() //nolint:errcheck // no-op after commit
			if _, err := mutationTx.Exec(ctx, tc.mutation, channel.ID, admin.ID); err != nil {
				t.Fatalf("hold %s mutation: %v", tc.name, err)
			}
			var blockerPID int
			if err := mutationTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				t.Fatalf("read mutation backend pid: %v", err)
			}

			probe, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatalf("connect lock probe: %v", err)
			}
			defer func() {
				if err := probe.Close(context.Background()); err != nil {
					t.Errorf("close lock probe: %v", err)
				}
			}()
			h := fullChannelDispatcher(s)
			completed := make(chan struct {
				body []byte
				err  error
			}, 1)
			go func() {
				body, err := dispatchChannelDeleteMessages(t, ctx, h, admin.ID, &tg.ChannelsDeleteMessagesRequest{
					Channel: api.InputChannel(admin.ID, channel.ID), ID: []int{int(post.LocalID)},
				})
				completed <- struct {
					body []byte
					err  error
				}{body: body, err: err}
			}()

			var waiting bool
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
		waitForLock:
			for !waiting {
				select {
				case result := <-completed:
					t.Fatalf("delete returned before %s committed: %v", tc.name, result.err)
				case <-ticker.C:
					if err := probe.QueryRow(ctx, `
						SELECT EXISTS (
						  SELECT 1 FROM pg_stat_activity
						  WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))
						)`, blockerPID).Scan(&waiting); err != nil {
						t.Fatalf("check delete lock wait: %v", err)
					}
				case <-deadline.C:
					t.Fatalf("delete did not wait for the %s participant row", tc.name)
				case <-ctx.Done():
					t.Fatalf("waiting for %s row lock: %v", tc.name, ctx.Err())
				}
				if waiting {
					break waitForLock
				}
			}
			if err := mutationTx.Commit(ctx); err != nil {
				t.Fatalf("commit %s mutation: %v", tc.name, err)
			}
			result := <-completed
			if result.err != nil {
				t.Fatalf("dispatch delete after %s: %v", tc.name, result.err)
			}
			_, rpc, err := decodeChannelDeleteMessagesResponse(result.body)
			if err != nil || rpc == nil || rpc.ErrorMessage != tc.wantError {
				t.Fatalf("delete after %s = RPC %v err %v, want %s", tc.name, rpc, err, tc.wantError)
			}
			if pts, err := s.ChannelState(ctx, channel.ID); err != nil || pts != 2 {
				t.Fatalf("channel pts after %s refusal = %d err=%v, want unchanged 2", tc.name, pts, err)
			}
		})
	}
}
