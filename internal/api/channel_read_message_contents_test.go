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

func TestChannelsReadMessageContentsLeavesHistoryAndDeliveryStateUnchanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator := readHistoryUser(t, s, ctx, "+15551239951")
	reader := readHistoryUser(t, s, ctx, "+15551239952")
	otherReader := readHistoryUser(t, s, ctx, "+15551239953")
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Content acknowledgement"})
	otherChannel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Other channel"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, user := range []store.User{reader, otherReader} {
		if _, _, err := s.JoinChannelByInvite(ctx, invite, user.ID); err != nil {
			t.Fatalf("join reader %d: %v", user.ID, err)
		}
	}
	first := postReadHistoryChannelMessage(t, ctx, s, channel.ID, creator.ID, "first", 127001)
	second := postReadHistoryChannelMessage(t, ctx, s, channel.ID, creator.ID, "second", 127002)
	deleted := postReadHistoryChannelMessage(t, ctx, s, channel.ID, creator.ID, "deleted", 127003)
	var outOfChannel int
	for i := range 4 {
		message := postReadHistoryChannelMessage(t, ctx, s, otherChannel.ID, creator.ID, "other", int64(127010+i))
		outOfChannel = int(message.LocalID)
	}
	channelExec(t, ctx, dsn, `UPDATE channel_messages SET deleted = true WHERE channel_id = $1 AND local_id = $2`, channel.ID, deleted.LocalID)

	beforeAudit := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, reader.ID)
	users := []int64{creator.ID, reader.ID, otherReader.ID}
	deliveryBefore := make(map[int64]channelReadHistoryDeliveryState, len(users))
	markersBefore := make(map[int64]int64, len(users))
	for _, userID := range users {
		deliveryBefore[userID] = readChannelReadHistoryDeliveryState(t, ctx, s, userID, channel.ID)
		markersBefore[userID] = readChannelMarker(t, ctx, dsn, channel.ID, userID)
	}
	listener := listenForChannelReadHistoryNotifications(t, ctx, dsn)
	h := fullChannelDispatcher(s)
	ids := []int{int(first.LocalID), int(second.LocalID), int(deleted.LocalID), 10000, outOfChannel}
	if ok, rpc := channelsReadMessageContentsViaDispatcher(t, h, reader.ID, false, api.InputChannel(reader.ID, channel.ID), ids); rpc != nil || !ok {
		t.Fatalf("channels.readMessageContents: ok=%v rpc=%v, want BoolTrue", ok, rpc)
	}
	afterAudit := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, reader.ID)
	requireChannelReadHistoryAuditChanges(t, beforeAudit, afterAudit, false, false, false)
	for _, userID := range users {
		if got := readChannelMarker(t, ctx, dsn, channel.ID, userID); got != markersBefore[userID] {
			t.Errorf("user %d marker changed from %d to %d", userID, markersBefore[userID], got)
		}
		deliveryAfter := readChannelReadHistoryDeliveryState(t, ctx, s, userID, channel.ID)
		requireChannelReadHistoryDeliveryUnchanged(t, "channels.readMessageContents", deliveryBefore[userID], deliveryAfter)
	}
	requireChannelReadState(t, s, reader.ID, channel.ID, 1, 2)
	requireNoChannelReadHistoryNotification(t, listener)

	if ok, rpc := channelsReadMessageContentsViaDispatcher(t, h, reader.ID, false, api.InputChannel(reader.ID, channel.ID), nil); rpc != nil || !ok {
		t.Fatalf("empty channels.readMessageContents: ok=%v rpc=%v, want BoolTrue", ok, rpc)
	}
	if ok, rpc := channelsReadMessageContentsViaDispatcher(t, h, reader.ID, false, api.InputChannel(reader.ID, channel.ID), ids); rpc != nil || !ok {
		t.Fatalf("repeated channels.readMessageContents: ok=%v rpc=%v, want BoolTrue", ok, rpc)
	}
	afterRepeat := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, reader.ID)
	requireChannelReadHistoryAuditChanges(t, beforeAudit, afterRepeat, false, false, false)
	for _, userID := range users {
		if got := readChannelMarker(t, ctx, dsn, channel.ID, userID); got != markersBefore[userID] {
			t.Errorf("repeated acknowledgement changed user %d marker from %d to %d", userID, markersBefore[userID], got)
		}
		deliveryAfter := readChannelReadHistoryDeliveryState(t, ctx, s, userID, channel.ID)
		requireChannelReadHistoryDeliveryUnchanged(t, "repeated channels.readMessageContents", deliveryBefore[userID], deliveryAfter)
	}
	requireChannelReadState(t, s, reader.ID, channel.ID, 1, 2)
	requireNoChannelReadHistoryNotification(t, listener)
}

func TestChannelsReadMessageContentsRejectsMoreThanOneHundredIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator := readHistoryUser(t, s, ctx, "+15551239961")
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Content limit"})
	ids := make([]int, 0, 101)
	for i := range 100 {
		ids = append(ids, i+1)
	}
	before := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, creator.ID)
	if ok, rpc := channelsReadMessageContentsViaDispatcher(
		t, fullChannelDispatcher(s), creator.ID, false, api.InputChannel(creator.ID, channel.ID), ids,
	); rpc != nil || !ok {
		t.Fatalf("100 IDs: ok=%v rpc=%v, want BoolTrue", ok, rpc)
	}
	afterHundred := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, creator.ID)
	requireChannelReadHistoryAuditChanges(t, before, afterHundred, false, false, false)
	ids = append(ids, 101)
	listener := listenForChannelReadHistoryNotifications(t, ctx, dsn)
	if ok, rpc := channelsReadMessageContentsViaDispatcher(
		t, fullChannelDispatcher(s), creator.ID, false, api.InputChannel(creator.ID, channel.ID), ids,
	); ok || rpc == nil || rpc.ErrorCode != 400 || rpc.ErrorMessage != "LIMIT_INVALID" {
		t.Fatalf("101 IDs: ok=%v rpc=%v, want 400 LIMIT_INVALID", ok, rpc)
	}
	after := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, creator.ID)
	requireChannelReadHistoryAuditChanges(t, before, after, false, false, false)
	requireNoChannelReadHistoryNotification(t, listener)
}

func TestChannelsReadMessageContentsRejectsUnauthorizedCallers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator := readHistoryUser(t, s, ctx, "+15551239971")
	member := readHistoryUser(t, s, ctx, "+15551239972")
	banned := readHistoryUser(t, s, ctx, "+15551239973")
	removed := readHistoryUser(t, s, ctx, "+15551239974")
	outsider := readHistoryUser(t, s, ctx, "+15551239975")
	channel := createChannel(t, s, creator.ID, &tg.ChannelsCreateChannelRequest{Broadcast: true, Title: "Content authorization"})
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	for _, user := range []store.User{member, banned, removed} {
		if _, _, err := s.JoinChannelByInvite(ctx, invite, user.ID); err != nil {
			t.Fatalf("join user %d: %v", user.ID, err)
		}
	}
	postReadHistoryChannelMessage(t, ctx, s, channel.ID, creator.ID, "post", 127071)
	banUntil := time.Now().Add(time.Hour)
	if err := s.SetChannelBan(ctx, channel.ID, creator.ID, banned.ID, &banUntil, false); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	if left, err := s.LeaveChannel(ctx, channel.ID, removed.ID); err != nil || !left {
		t.Fatalf("remove member: left=%v err=%v", left, err)
	}
	beforeAudit := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, member.ID)
	users := []int64{creator.ID, member.ID, banned.ID, removed.ID, outsider.ID}
	deliveryBefore := make(map[int64]channelReadHistoryDeliveryState, len(users))
	for _, userID := range users {
		deliveryBefore[userID] = readChannelReadHistoryDeliveryState(t, ctx, s, userID, channel.ID)
	}
	listener := listenForChannelReadHistoryNotifications(t, ctx, dsn)
	h := fullChannelDispatcher(s)
	for _, tc := range []struct {
		name        string
		userID      int64
		input       tg.InputChannelClass
		provisional bool
		want        string
	}{
		{name: "outsider", userID: outsider.ID, input: api.InputChannel(outsider.ID, channel.ID), want: "PEER_ID_INVALID"},
		{name: "borrowed hash", userID: member.ID, input: api.InputChannel(creator.ID, channel.ID), want: "PEER_ID_INVALID"},
		{name: "forged hash", userID: member.ID, input: &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.ID}, want: "PEER_ID_INVALID"},
		{name: "banned member", userID: banned.ID, input: api.InputChannel(banned.ID, channel.ID), want: "PEER_ID_INVALID"},
		{name: "removed member", userID: removed.ID, input: api.InputChannel(removed.ID, channel.ID), want: "PEER_ID_INVALID"},
		{name: "unauthenticated", userID: 0, input: api.InputChannel(member.ID, channel.ID), want: "AUTH_KEY_UNREGISTERED"},
		{name: "provisional", userID: member.ID, input: api.InputChannel(member.ID, channel.ID), provisional: true, want: "AUTH_KEY_UNREGISTERED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok, rpc := channelsReadMessageContentsViaDispatcher(t, h, tc.userID, tc.provisional, tc.input, []int{2}); ok || rpc == nil || rpc.ErrorMessage != tc.want {
				t.Fatalf("channels.readMessageContents: ok=%v rpc=%v, want %s", ok, rpc, tc.want)
			}
		})
	}
	afterAudit := readChannelReadHistoryAudit(t, ctx, dsn, channel.ID, member.ID)
	requireChannelReadHistoryAuditChanges(t, beforeAudit, afterAudit, false, false, false)
	for _, userID := range users {
		deliveryAfter := readChannelReadHistoryDeliveryState(t, ctx, s, userID, channel.ID)
		requireChannelReadHistoryDeliveryUnchanged(t, "refused channels.readMessageContents", deliveryBefore[userID], deliveryAfter)
	}
	requireNoChannelReadHistoryNotification(t, listener)
}

func channelsReadMessageContentsViaDispatcher(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	provisional bool,
	channel tg.InputChannelClass,
	ids []int,
) (bool, *mt.RPCError) {
	t.Helper()
	body := dispatchSettings(t, h, settingsHandler{
		name: "channels.readMessageContents",
		request: func() bin.Encoder {
			return &tg.ChannelsReadMessageContentsRequest{Channel: channel, ID: ids}
		},
	}, userID, provisional)
	var success tg.BoolTrue
	if err := success.Decode(&bin.Buffer{Buf: body}); err == nil {
		return true, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode channels.readMessageContents response: %v", err)
	}
	return false, &rpc
}
