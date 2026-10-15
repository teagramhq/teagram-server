package api_test

import (
	"context"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

// unreadInvoker sends gotd's encoded requests through the real dispatcher.
type unreadInvoker struct {
	t      *testing.T
	h      mtproto.Handler
	userID int64
}

func (i unreadInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	body := dispatchSettingsWithContext(i.t, i.h, settingsHandler{name: "unread acknowledgement", request: func() bin.Encoder { return input }}, i.userID, false, ctx)
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
		return tgerr.New(rpc.ErrorCode, rpc.ErrorMessage)
	}
	return output.Decode(&bin.Buffer{Buf: body})
}

func TestUnreadAcknowledgementsThroughGotd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	users, chat := chatWith(t, s, "+15551982001", "+15551982002", "+15551982003")
	sendChatForReadHistory(t, s, chat.ID, users[0].ID, 198201)
	outsider := readHistoryUser(t, s, ctx, "+15551982004")
	if _, _, _, _, err := s.SendMessage(ctx, users[0].ID, users[1].ID, "private", 198202, 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, users[0].ID, users[0].ID, "saved", 198204, 0, 0); err != nil {
		t.Fatal(err)
	}
	h := fullChannelDispatcher(s)
	snapshots := make(map[int64]readHistoryOwnerState)
	for _, user := range users {
		snapshots[user.ID] = snapshotReadHistoryOwner(t, s, user.ID)
	}
	before := snapshots[users[0].ID]
	listener := notificationListener(t, dsn)
	methods := []struct {
		name string
		call func(*tg.Client, tg.InputPeerClass, []int, bool) error
	}{
		{"contents", func(c *tg.Client, _ tg.InputPeerClass, ids []int, _ bool) error {
			got, err := c.MessagesReadMessageContents(ctx, ids)
			if err == nil && (got.Pts != before.pts || got.PtsCount != 0) {
				t.Errorf("contents = %+v, want pts %d count 0", got, before.pts)
			}
			return err
		}},
		{"mentions", func(c *tg.Client, p tg.InputPeerClass, _ []int, filtered bool) error {
			req := &tg.MessagesReadMentionsRequest{Peer: p}
			if filtered {
				req.SetTopMsgID(42)
			}
			got, err := c.MessagesReadMentions(ctx, req)
			if err == nil && (got.Pts != before.pts || got.PtsCount != 0 || got.Offset != 0) {
				t.Errorf("mentions = %+v", got)
			}
			return err
		}},
		{"unread-mentions", func(c *tg.Client, p tg.InputPeerClass, _ []int, filtered bool) error {
			req := &tg.MessagesGetUnreadMentionsRequest{Peer: p, Limit: 10}
			if filtered {
				req.SetTopMsgID(42)
			}
			got, err := c.MessagesGetUnreadMentions(ctx, req)
			if err == nil {
				page, ok := got.(*tg.MessagesMessages)
				if !ok || len(page.Messages) != 0 || len(page.Users) != 0 || len(page.Chats) != 0 {
					t.Errorf("unread mentions = %#v", got)
				}
			}
			return err
		}},
		{"reactions", func(c *tg.Client, p tg.InputPeerClass, _ []int, filtered bool) error {
			req := &tg.MessagesReadReactionsRequest{Peer: p}
			if filtered {
				req.SetTopMsgID(42)
				req.SetSavedPeerID(&tg.InputPeerChat{ChatID: 999999})
			}
			got, err := c.MessagesReadReactions(ctx, req)
			if err == nil && (got.Pts != before.pts || got.PtsCount != 0 || got.Offset != 0) {
				t.Errorf("reactions = %+v", got)
			}
			return err
		}},
	}
	client := func(t *testing.T, id int64) *tg.Client {
		t.Helper()
		return tg.NewClient(unreadInvoker{t: t, h: h, userID: id})
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			for _, filtered := range []bool{false, true} {
				for _, readable := range []struct {
					peer tg.InputPeerClass
					id   int
				}{
					{&tg.InputPeerSelf{}, 3}, {api.InputPeerUser(users[0].ID, users[1].ID), 2}, {&tg.InputPeerChat{ChatID: chat.ID}, 1},
				} {
					if err := method.call(client(t, users[0].ID), readable.peer, []int{readable.id}, filtered); err != nil {
						t.Fatalf("readable peer: %v", err)
					}
				}
				assertReadHistoryRPCError(t, method.call(client(t, 0), &tg.InputPeerChat{ChatID: chat.ID}, []int{1}, filtered), 401, "AUTH_KEY_UNREGISTERED")
				if method.name != "contents" {
					assertReadHistoryRPCError(t, method.call(client(t, outsider.ID), &tg.InputPeerChat{ChatID: chat.ID}, nil, filtered), 400, "PEER_ID_INVALID")
					assertReadHistoryRPCError(t, method.call(client(t, users[0].ID), &tg.InputPeerChat{ChatID: 999999}, nil, filtered), 400, "PEER_ID_INVALID")
					assertReadHistoryRPCError(t, method.call(client(t, users[0].ID), &tg.InputPeerUser{UserID: 999999}, nil, filtered), 400, "PEER_ID_INVALID")
				}
			}
		})
	}
	for _, ids := range [][]int{nil, {999999}, make([]int, 100)} {
		if _, err := client(t, users[0].ID).MessagesReadMessageContents(ctx, ids); err != nil {
			t.Fatal(err)
		}
	}
	_, err := client(t, users[0].ID).MessagesReadMessageContents(ctx, make([]int, 101))
	assertReadHistoryRPCError(t, err, 400, "LIMIT_INVALID")
	for _, user := range users {
		assertReadHistoryOwnerState(t, s, user.ID, snapshots[user.ID])
	}
	assertNoReadHistoryNotification(t, listener)
	state, err := client(t, users[0].ID).UpdatesGetState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := client(t, users[0].ID).UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: state.Pts, Date: state.Date, Qts: state.Qts})
	if err != nil {
		t.Fatal(err)
	}
	// Each dispatcher invocation uses a fresh connection, whose first difference
	// includes the existing dialog-filter refresh. No message or read update is due.
	full, ok := diff.(*tg.UpdatesDifference)
	if !ok || len(full.NewMessages) != 0 || len(full.NewEncryptedMessages) != 0 || len(full.OtherUpdates) != 1 || full.State.Pts != before.pts || full.State.Seq != 0 {
		t.Fatalf("difference = %#v, want only initial filter refresh", diff)
	}
	if _, ok := full.OtherUpdates[0].(*tg.UpdateDialogFilters); !ok {
		t.Fatalf("unexpected difference update: %T", full.OtherUpdates[0])
	}
	// A retained owner-local copy must not let a removed member read a group.
	removedMessage := sendChatForReadHistory(t, s, chat.ID, users[2].ID, 198203)
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, users[2].ID, users[0].ID); err != nil {
		t.Fatal(err)
	}
	for _, method := range methods {
		assertReadHistoryRPCError(t, method.call(client(t, users[2].ID), &tg.InputPeerChat{ChatID: chat.ID}, []int{int(removedMessage.LocalID)}, true), 400, "PEER_ID_INVALID")
	}
}
