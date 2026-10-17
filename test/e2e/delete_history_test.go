package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/store"
)

func callDeleteHistory(c *smokeClient, ctx context.Context, peer tg.InputPeerClass, maxID int, justClear, revoke bool) (*tg.MessagesAffectedHistory, error) {
	var result *tg.MessagesAffectedHistory
	err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
		request := &tg.MessagesDeleteHistoryRequest{Peer: peer, MaxID: maxID}
		request.SetJustClear(justClear)
		request.SetRevoke(revoke)
		var err error
		result, err = client.MessagesDeleteHistory(ctx, request)
		return err
	})
	return result, err
}

func getSmokeHistory(c *smokeClient, ctx context.Context, peer tg.InputPeerClass) (*tg.MessagesMessages, error) {
	return getSmokeHistoryPage(c, ctx, peer, 0)
}

func getSmokeHistoryPage(c *smokeClient, ctx context.Context, peer tg.InputPeerClass, offsetID int) (*tg.MessagesMessages, error) {
	var result *tg.MessagesMessages
	err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
		response, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, OffsetID: offsetID, Limit: 100})
		if err != nil {
			return err
		}
		var ok bool
		result, ok = response.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("history response = %T, want *tg.MessagesMessages", response)
		}
		return nil
	})
	return result, err
}

func assertEmptySmokePrivateDialog(t *testing.T, c *smokeClient, ctx context.Context, peerID int64, deletedMessages ...string) {
	t.Helper()
	for _, source := range []string{"getDialogs", "getPeerDialogs"} {
		if err := c.call(ctx, func(ctx context.Context, client *tg.Client) error {
			var dialogs []tg.DialogClass
			var messages []tg.MessageClass
			if source == "getDialogs" {
				response, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
					OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20,
				})
				if err != nil {
					return err
				}
				dialogs = dialogRows(t, response)
				switch response := response.(type) {
				case *tg.MessagesDialogs:
					messages = response.Messages
				case *tg.MessagesDialogsSlice:
					messages = response.Messages
				default:
					return fmt.Errorf("getDialogs response = %T", response)
				}
			} else {
				response, err := client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
					&tg.InputDialogPeer{Peer: peerUser(c.id, peerID)},
				})
				if err != nil {
					return err
				}
				dialogs, messages = response.Dialogs, response.Messages
			}
			found := false
			for _, entry := range dialogs {
				dialog, ok := entry.(*tg.Dialog)
				if !ok {
					continue
				}
				peer, ok := dialog.Peer.(*tg.PeerUser)
				if !ok || peer.UserID != peerID {
					continue
				}
				found = true
				if dialog.TopMessage != 0 {
					return fmt.Errorf("%s top_message = %d, want 0 after private revoke", source, dialog.TopMessage)
				}
			}
			if !found {
				return fmt.Errorf("%s omitted dialog retained by just_clear", source)
			}
			for _, entry := range messages {
				if message, ok := entry.(*tg.Message); ok && slices.Contains(deletedMessages, message.Message) {
					return fmt.Errorf("%s rendered revoked message %q", source, message.Message)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("verify empty private dialog through %s: %v", source, err)
		}
	}
}

func TestDeleteHistoryKeepsDialogAndOnlyClearsCaller(t *testing.T) {
	t.Parallel()
	testDeleteHistoryKeepsDialogAndOnlyClearsCaller(t)
}

func testDeleteHistoryKeepsDialogAndOnlyClearsCaller(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424001", "+15551424002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory peer", phoneB)

	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		for i := 1; i <= 3; i++ {
			if _, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(a.id, b.id), Message: fmt.Sprintf("history message %d", i), RandomID: int64(1424000 + i),
			}); err != nil {
				return fmt.Errorf("send history message %d: %w", i, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed private history: %v", err)
	}

	result, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, false)
	if err != nil {
		t.Fatalf("clear caller history: %v", err)
	}
	if result.PtsCount != 3 || result.Offset != 0 {
		t.Fatalf("affected history pts_count/offset = %d/%d, want 3/0", result.PtsCount, result.Offset)
	}

	for _, check := range []struct {
		client *smokeClient
		peerID int64
		want   int
	}{
		{client: a, peerID: b.id, want: 0},
		{client: b, peerID: a.id, want: 3},
	} {
		history, err := getSmokeHistory(check.client, f.ctx, peerUser(check.client.id, check.peerID))
		if err != nil {
			t.Fatalf("check history for %d: %v", check.client.id, err)
		}
		if len(history.Messages) != check.want {
			t.Fatalf("history for %d has %d messages, want %d", check.client.id, len(history.Messages), check.want)
		}
	}
	for _, source := range []string{"getDialogs", "getPeerDialogs"} {
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			var dialogs []tg.DialogClass
			var messages []tg.MessageClass
			switch source {
			case "getDialogs":
				response, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
					OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20,
				})
				if err != nil {
					return err
				}
				dialogs = dialogRows(t, response)
				switch response := response.(type) {
				case *tg.MessagesDialogs:
					messages = response.Messages
				case *tg.MessagesDialogsSlice:
					messages = response.Messages
				default:
					return fmt.Errorf("getDialogs response = %T", response)
				}
			case "getPeerDialogs":
				response, err := client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
					&tg.InputDialogPeer{Peer: peerUser(a.id, b.id)},
				})
				if err != nil {
					return err
				}
				dialogs, messages = response.Dialogs, response.Messages
			}
			found := false
			for _, entry := range dialogs {
				dialog, ok := entry.(*tg.Dialog)
				if !ok {
					continue
				}
				peer, ok := dialog.Peer.(*tg.PeerUser)
				if !ok || peer.UserID != b.id {
					continue
				}
				found = true
				if dialog.TopMessage != 0 {
					return fmt.Errorf("%s top_message = %d, want 0 after clear", source, dialog.TopMessage)
				}
			}
			if !found {
				return fmt.Errorf("%s omitted dialog kept by just_clear", source)
			}
			for _, entry := range messages {
				if message, ok := entry.(*tg.Message); ok && message.Message == "history message 3" {
					return fmt.Errorf("%s rendered cleared message", source)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("verify empty retained dialog through %s: %v", source, err)
		}
	}
}

func TestDeletedMessagesDoNotRenderAsDialogTopMessages(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424101", "+15551424102"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleted dialog top caller", phoneA)
	b := newSmokeClient(t, f, "deleted dialog top peer", phoneB)

	var messageID int
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a.id, b.id), Message: "deleted dialog top secret", RandomID: 1424101,
		})
		if err != nil {
			return err
		}
		message, _, ok := outgoingMessage(t, result, "deleted dialog top secret")
		if !ok {
			return errors.New("send result omitted the outgoing message")
		}
		messageID = message.ID
		return nil
	}); err != nil {
		t.Fatalf("send message: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{ID: []int{messageID}})
		return err
	}); err != nil {
		t.Fatalf("delete message: %v", err)
	}

	checkDialogs := func(name string, call func(context.Context, *tg.Client) ([]tg.DialogClass, []tg.MessageClass, error)) {
		t.Helper()
		if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			dialogs, messages, err := call(ctx, client)
			if err != nil {
				return err
			}
			found := false
			for _, entry := range dialogs {
				dialog, ok := entry.(*tg.Dialog)
				if !ok {
					continue
				}
				peer, ok := dialog.Peer.(*tg.PeerUser)
				if !ok || peer.UserID != b.id {
					continue
				}
				found = true
				if dialog.TopMessage != 0 {
					return fmt.Errorf("%s top_message = %d, want 0 for deleted message", name, dialog.TopMessage)
				}
			}
			if !found {
				return fmt.Errorf("%s omitted the private dialog", name)
			}
			for _, entry := range messages {
				if message, ok := entry.(*tg.Message); ok && message.Message == "deleted dialog top secret" {
					return fmt.Errorf("%s rendered the deleted top message", name)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
	}
	checkDialogs("getDialogs", func(ctx context.Context, client *tg.Client) ([]tg.DialogClass, []tg.MessageClass, error) {
		result, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
		if err != nil {
			return nil, nil, err
		}
		switch response := result.(type) {
		case *tg.MessagesDialogs:
			return response.Dialogs, response.Messages, nil
		case *tg.MessagesDialogsSlice:
			return response.Dialogs, response.Messages, nil
		default:
			return nil, nil, fmt.Errorf("getDialogs response = %T", result)
		}
	})
	checkDialogs("getPeerDialogs", func(ctx context.Context, client *tg.Client) ([]tg.DialogClass, []tg.MessageClass, error) {
		result, err := client.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
			&tg.InputDialogPeer{Peer: peerUser(a.id, b.id)},
		})
		if err != nil {
			return nil, nil, err
		}
		return result.Dialogs, result.Messages, nil
	})
}

func TestDeleteHistoryRangeDialogRemovalAndSavedMessages(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551424201", "+15551424202", "+15551424203"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
	a := newSmokeClient(t, f, "deleteHistory range caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory range peer", phoneB)
	c := newSmokeClient(t, f, "deleteHistory unrelated peer", phoneC)

	for i := 1; i <= 3; i++ {
		if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, b.id, fmt.Sprintf("range %d", i), int64(1424200+i), 0, 0); err != nil {
			t.Fatalf("seed range message %d: %v", i, err)
		}
	}
	if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, c.id, "unrelated peer row", 1424204, 0, 0); err != nil {
		t.Fatalf("seed unrelated peer message: %v", err)
	}

	result, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 2, true, false)
	if err != nil {
		t.Fatalf("delete through max id 2: %v", err)
	}
	if result.PtsCount != 2 || result.Offset != 0 {
		t.Fatalf("partial affected history pts_count/offset = %d/%d, want 2/0", result.PtsCount, result.Offset)
	}
	callerHistory, err := getSmokeHistory(a, f.ctx, peerUser(a.id, b.id))
	if err != nil || len(callerHistory.Messages) != 1 {
		t.Fatalf("caller history after partial clear = %v, err=%v, want 1", lenHistory(callerHistory), err)
	}
	remaining, ok := callerHistory.Messages[0].(*tg.Message)
	if !ok || remaining.Message != "range 3" {
		t.Fatalf("remaining caller message = %#v, want range 3", callerHistory.Messages[0])
	}
	unrelated, err := getSmokeHistory(a, f.ctx, peerUser(a.id, c.id))
	if err != nil || len(unrelated.Messages) != 1 {
		t.Fatalf("unrelated history = %v, err=%v, want 1", lenHistory(unrelated), err)
	}

	result, err = callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, false, false)
	if err != nil {
		t.Fatalf("delete chat: %v", err)
	}
	if result.PtsCount != 1 || result.Offset != 0 {
		t.Fatalf("delete chat pts_count/offset = %d/%d, want 1/0", result.PtsCount, result.Offset)
	}
	previousPts := result.Pts
	result, err = callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, false, false)
	if err != nil {
		t.Fatalf("repeat completed delete chat: %v", err)
	}
	if result.PtsCount != 0 || result.Offset != 0 || result.Pts != previousPts {
		t.Fatalf("repeat affected history = %+v, want no change at pts %d", result, previousPts)
	}
	peerHistory, err := getSmokeHistory(b, f.ctx, peerUser(b.id, a.id))
	if err != nil || len(peerHistory.Messages) != 3 {
		t.Fatalf("peer history after self clear = %v, err=%v, want 3", lenHistory(peerHistory), err)
	}
	if err = a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		response, err := client.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 20})
		if err != nil {
			return err
		}
		for _, entry := range dialogRows(t, response) {
			if dialog, ok := entry.(*tg.Dialog); ok {
				if peer, ok := dialog.Peer.(*tg.PeerUser); ok && peer.UserID == b.id {
					return errors.New("deleted chat still appears in getDialogs")
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("verify deleted dialog removal: %v", err)
	}

	if err = a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		if _, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: "saved message", RandomID: 1424205,
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("send Saved Message: %v", err)
	}
	result, err = callDeleteHistory(a, f.ctx, &tg.InputPeerSelf{}, 0, true, true)
	if err != nil {
		t.Fatalf("clear Saved Messages with revoke: %v", err)
	}
	if result.PtsCount != 1 || result.Offset != 0 {
		t.Fatalf("Saved Messages affected history pts_count/offset = %d/%d, want 1/0", result.PtsCount, result.Offset)
	}
	saved, err := getSmokeHistory(a, f.ctx, &tg.InputPeerSelf{})
	if err != nil || len(saved.Messages) != 0 {
		t.Fatalf("Saved Messages after clear = %v, err=%v, want empty", lenHistory(saved), err)
	}
}

func lenHistory(history *tg.MessagesMessages) int {
	if history == nil {
		return -1
	}
	return len(history.Messages)
}

func TestDeleteHistoryBatchesAndCompletesIdempotently(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424301", "+15551424302"
	seedSmokeUsers(t, f, phoneA, phoneB)
	aUser, ok, err := f.store.UserByUsernameWithLoginMode(f.ctx, smokeUsernameForPhone(phoneA))
	if err != nil || !ok {
		t.Fatalf("load caller id: %v", err)
	}
	bUser, ok, err := f.store.UserByUsernameWithLoginMode(f.ctx, smokeUsernameForPhone(phoneB))
	if err != nil || !ok {
		t.Fatalf("load peer id: %v", err)
	}
	for i := 1; i <= 105; i++ {
		if _, _, _, _, err := f.store.SendMessage(f.ctx, aUser.ID, bUser.ID, fmt.Sprintf("batch %d", i), int64(1424300+i), 0, 0); err != nil {
			t.Fatalf("seed batch message %d: %v", i, err)
		}
	}
	a := newSmokeClient(t, f, "deleteHistory batch caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory batch peer", phoneB)

	first, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, false)
	if err != nil {
		t.Fatalf("delete first batch: %v", err)
	}
	if first.PtsCount != 100 || first.Offset != 5 {
		t.Fatalf("first batch pts_count/offset = %d/%d, want 100/5", first.PtsCount, first.Offset)
	}
	second, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, false)
	if err != nil {
		t.Fatalf("delete remaining batch: %v", err)
	}
	if second.PtsCount != 5 || second.Offset != 0 {
		t.Fatalf("second batch pts_count/offset = %d/%d, want 5/0", second.PtsCount, second.Offset)
	}
	third, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, false)
	if err != nil {
		t.Fatalf("repeat completed history delete: %v", err)
	}
	if third.PtsCount != 0 || third.Offset != 0 || third.Pts != second.Pts {
		t.Fatalf("completed repeat = %+v, want no change at pts %d", third, second.Pts)
	}
	callerHistory, err := getSmokeHistory(a, f.ctx, peerUser(a.id, b.id))
	if err != nil || lenHistory(callerHistory) != 0 {
		t.Fatalf("caller history after batches = %d, err=%v, want empty", lenHistory(callerHistory), err)
	}
	peerHistory, err := getSmokeHistory(b, f.ctx, peerUser(b.id, a.id))
	if err != nil || lenHistory(peerHistory) != 100 {
		t.Fatalf("first peer history page after caller clear = %d, err=%v, want 100", lenHistory(peerHistory), err)
	}
	oldest, ok := peerHistory.Messages[len(peerHistory.Messages)-1].(*tg.Message)
	if !ok {
		t.Fatalf("oldest message on first peer page = %T, want *tg.Message", peerHistory.Messages[len(peerHistory.Messages)-1])
	}
	peerHistoryTail, err := getSmokeHistoryPage(b, f.ctx, peerUser(b.id, a.id), oldest.ID)
	if err != nil || lenHistory(peerHistoryTail) != 5 {
		t.Fatalf("remaining peer history after caller clear = %d, err=%v, want 5", lenHistory(peerHistoryTail), err)
	}
}

func TestDeleteHistoryLeavesServiceMessagesInBasicGroups(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424401", "+15551424402"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory group caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory group peer", phoneB)
	chat, err := f.store.CreateChat(f.ctx, a.id, "history service messages", []int64{b.id})
	if err != nil {
		t.Fatalf("create basic group: %v", err)
	}
	if _, _, _, err := f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: chat.ID, FromID: a.id, Text: "history service messages", Action: store.ChatActionCreate, RandomID: 1424400,
	}); err != nil {
		t.Fatalf("create group service message: %v", err)
	}
	if _, _, _, err := f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: chat.ID, FromID: b.id, Text: "ordinary group message", RandomID: 1424401,
	}); err != nil {
		t.Fatalf("send group message: %v", err)
	}

	result, err := callDeleteHistory(a, f.ctx, &tg.InputPeerChat{ChatID: chat.ID}, 0, true, false)
	if err != nil {
		t.Fatalf("clear basic group history: %v", err)
	}
	if result.PtsCount != 1 || result.Offset != 0 {
		t.Fatalf("group affected history pts_count/offset = %d/%d, want 1/0", result.PtsCount, result.Offset)
	}
	history, err := getSmokeHistory(a, f.ctx, &tg.InputPeerChat{ChatID: chat.ID})
	if err != nil {
		t.Fatalf("get group history after clear: %v", err)
	}
	if len(history.Messages) != 1 {
		t.Fatalf("group history has %d messages, want the retained service message", len(history.Messages))
	}
	if _, ok := history.Messages[0].(*tg.MessageService); !ok {
		t.Fatalf("remaining group message = %T, want service message", history.Messages[0])
	}
}

func TestDeleteHistoryPrivateRevokeCanBeInitiatedByEitherParticipant(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424451", "+15551424452"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory alternate caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory alternate peer", phoneB)
	for _, message := range []struct {
		sender *smokeClient
		peer   int64
		text   string
		rid    int64
	}{
		{sender: a, peer: b.id, text: "first participant outbox", rid: 1424441},
		{sender: b, peer: a.id, text: "second participant outbox", rid: 1424442},
	} {
		if _, _, _, _, err := f.store.SendMessage(f.ctx, message.sender.id, message.peer, message.text, message.rid, 0, 0); err != nil {
			t.Fatalf("seed %q: %v", message.text, err)
		}
	}

	result, err := callDeleteHistory(b, f.ctx, peerUser(b.id, a.id), 0, true, true)
	if err != nil {
		t.Fatalf("revoke history as the other participant: %v", err)
	}
	if result.PtsCount != 2 || result.Offset != 0 {
		t.Fatalf("alternate caller pts_count/offset = %d/%d, want 2/0", result.PtsCount, result.Offset)
	}
	for _, check := range []struct {
		client *smokeClient
		peerID int64
	}{{a, b.id}, {b, a.id}} {
		history, err := getSmokeHistory(check.client, f.ctx, peerUser(check.client.id, check.peerID))
		if err != nil || lenHistory(history) != 0 {
			t.Fatalf("history for %d after alternate caller revoke = %d, err=%v, want empty", check.client.id, lenHistory(history), err)
		}
	}
}

func TestDeleteHistoryPrivateRevokePushesToOnlinePeer(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424501", "+15551424502"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory revoke caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory revoke peer", phoneB)

	for _, message := range []struct {
		sender *smokeClient
		peer   int64
		text   string
		rid    int64
	}{
		{sender: a, peer: b.id, text: "caller outbox", rid: 1424501},
		{sender: b, peer: a.id, text: "peer outbox", rid: 1424502},
	} {
		if err := message.sender.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(message.sender.id, message.peer), Message: message.text, RandomID: message.rid,
			})
			return err
		}); err != nil {
			t.Fatalf("send %q: %v", message.text, err)
		}
	}

	result, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, true)
	if err != nil {
		t.Fatalf("revoke private history: %v", err)
	}
	if result.PtsCount != 2 || result.Offset != 0 {
		t.Fatalf("revoke affected history pts_count/offset = %d/%d, want 2/0", result.PtsCount, result.Offset)
	}
	deletedIDs := make([]int, 0, 2)
	for len(deletedIDs) < 2 {
		deletedIDs = append(deletedIDs, recvOrCtx(t, f.ctx, b.push.delMsg, "online peer updateDeleteMessages")...)
	}
	slices.Sort(deletedIDs)
	if !slices.Equal(deletedIDs, []int{1, 2}) {
		t.Fatalf("online peer deleted ids = %v, want [1 2]", deletedIDs)
	}
	for _, check := range []struct {
		client *smokeClient
		peerID int64
	}{{a, b.id}, {b, a.id}} {
		history, err := getSmokeHistory(check.client, f.ctx, peerUser(check.client.id, check.peerID))
		if err != nil || lenHistory(history) != 0 {
			t.Fatalf("history for %d after revoke = %d, err=%v, want empty", check.client.id, lenHistory(history), err)
		}
	}
	assertEmptySmokePrivateDialog(t, b, f.ctx, a.id, "caller outbox", "peer outbox")
}

func TestDeleteHistoryPrivateRevokeReachesOfflinePeerByDifference(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424601", "+15551424602"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory offline revoke caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory offline revoke peer", phoneB)

	for _, message := range []struct {
		sender *smokeClient
		peer   int64
		text   string
		rid    int64
	}{
		{sender: a, peer: b.id, text: "caller outbox offline", rid: 1424601},
		{sender: b, peer: a.id, text: "peer outbox offline", rid: 1424602},
	} {
		if err := message.sender.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
			_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(message.sender.id, message.peer), Message: message.text, RandomID: message.rid,
			})
			return err
		}); err != nil {
			t.Fatalf("send %q: %v", message.text, err)
		}
	}
	var stateBefore *tg.UpdatesState
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		stateBefore, err = client.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("read peer state before disconnect: %v", err)
	}
	b.stopClient(t)
	if _, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, true); err != nil {
		t.Fatalf("revoke private history while peer is offline: %v", err)
	}

	offlineClient := f.savedSessionClient(b.session)
	var difference tg.UpdatesDifferenceClass
	if err := offlineClient.Run(f.ctx, func(ctx context.Context) error {
		var err error
		difference, err = offlineClient.API().UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{
			Pts: stateBefore.Pts, Qts: stateBefore.Qts, Date: stateBefore.Date,
		})
		return err
	}); err != nil {
		t.Fatalf("offline peer getDifference: %v", err)
	}
	full, ok := difference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("getDifference result = %T, want *tg.UpdatesDifference", difference)
	}
	deletedIDs := make([]int, 0, 2)
	for _, update := range full.OtherUpdates {
		if deleted, ok := update.(*tg.UpdateDeleteMessages); ok {
			deletedIDs = append(deletedIDs, deleted.Messages...)
		}
	}
	slices.Sort(deletedIDs)
	if !slices.Equal(deletedIDs, []int{1, 2}) {
		t.Fatalf("offline peer delete updates = %v, want [1 2]", deletedIDs)
	}
}

func TestDeleteHistoryRejectsUnsupportedTargetsWithoutChanges(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551424701", "+15551424702"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "deleteHistory unsupported caller", phoneA)
	b := newSmokeClient(t, f, "deleteHistory unsupported peer", phoneB)
	chat, err := f.store.CreateChat(f.ctx, a.id, "history refusal", []int64{b.id})
	if err != nil {
		t.Fatalf("create basic group: %v", err)
	}
	if _, _, _, err = f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: chat.ID, FromID: a.id, Text: "must survive refusal", RandomID: 1424701,
	}); err != nil {
		t.Fatalf("send group message: %v", err)
	}
	if _, _, _, _, err = f.store.SendMessage(f.ctx, a.id, b.id, "private row must survive refusal", 1424703, 0, 0); err != nil {
		t.Fatalf("send private message: %v", err)
	}
	channel, err := f.store.CreateChannel(f.ctx, a.id, "history refusal channel", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if _, _, _, err = f.store.PostChannelMessage(f.ctx, channel.ID, a.id, "channel post", 1424702, nil, 0); err != nil {
		t.Fatalf("send channel post: %v", err)
	}
	groupBefore, err := f.store.History(f.ctx, a.id, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("read group before refusal: %v", err)
	}
	privateBefore, err := f.store.History(f.ctx, a.id, store.PeerTypeUser, b.id, 0, 20)
	if err != nil {
		t.Fatalf("read private history before refusal: %v", err)
	}
	channelBefore, err := f.store.ChannelHistory(f.ctx, channel.ID, 0, 20)
	if err != nil {
		t.Fatalf("read channel before refusal: %v", err)
	}

	requests := []struct {
		name string
		call func() error
	}{
		{name: "basic-group revoke", call: func() error {
			_, err := callDeleteHistory(a, f.ctx, &tg.InputPeerChat{ChatID: chat.ID}, 0, true, true)
			return err
		}},
		{name: "min date", call: func() error {
			return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				request := &tg.MessagesDeleteHistoryRequest{Peer: peerUser(a.id, b.id)}
				request.SetMinDate(0)
				_, err := client.MessagesDeleteHistory(ctx, request)
				return err
			})
		}},
		{name: "max date", call: func() error {
			return a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
				request := &tg.MessagesDeleteHistoryRequest{Peer: peerUser(a.id, b.id)}
				request.SetMaxDate(0)
				_, err := client.MessagesDeleteHistory(ctx, request)
				return err
			})
		}},
		{name: "channel", call: func() error {
			_, err := callDeleteHistory(a, f.ctx, peerChannel(a.id, channel.ID), 0, true, false)
			return err
		}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			if err := request.call(); !tgerr.Is(err, "PEER_ID_INVALID") {
				t.Fatalf("error = %v, want PEER_ID_INVALID", err)
			}
		})
	}
	groupAfter, err := f.store.History(f.ctx, a.id, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("read group after refusal: %v", err)
	}
	privateAfter, err := f.store.History(f.ctx, a.id, store.PeerTypeUser, b.id, 0, 20)
	if err != nil {
		t.Fatalf("read private history after refusal: %v", err)
	}
	channelAfter, err := f.store.ChannelHistory(f.ctx, channel.ID, 0, 20)
	if err != nil {
		t.Fatalf("read channel after refusal: %v", err)
	}
	if len(groupAfter) != len(groupBefore) || len(privateAfter) != len(privateBefore) || len(channelAfter) != len(channelBefore) {
		t.Fatalf("refused requests changed histories: group %d→%d, private %d→%d, channel %d→%d", len(groupBefore), len(groupAfter), len(privateBefore), len(privateAfter), len(channelBefore), len(channelAfter))
	}
}

func TestDeleteHistoryKeepsMediaWhileAnotherMessageReferencesIt(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551424801", "+15551424802", "+15551424803"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
	a := newSmokeClient(t, f, "deleteHistory media owner", phoneA)
	b := newSmokeClient(t, f, "deleteHistory media peer", phoneB)
	c := newSmokeClient(t, f, "deleteHistory media remaining peer", phoneC)
	const uploadID = 1424801
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		saved, err := client.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: uploadID, FilePart: 0, Bytes: []byte("history media body"),
		})
		if err != nil {
			return err
		}
		if !saved {
			return errors.New("upload part returned false")
		}
		result, err := client.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerUser(a.id, b.id),
			Media: &tg.InputMediaUploadedDocument{
				File: &tg.InputFile{ID: uploadID, Parts: 1, Name: "history.txt"}, MimeType: "text/plain",
			},
			Message: "shared media", RandomID: 1424802,
		})
		if err != nil {
			return err
		}
		message, _, ok := outgoingMessage(t, result, "shared media")
		if !ok {
			return errors.New("sendMedia result omitted the outgoing message")
		}
		document, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			return fmt.Errorf("sent media = %T, want *tg.MessageMediaDocument", message.Media)
		}
		file, ok := document.Document.(*tg.Document)
		if !ok {
			return fmt.Errorf("sent document = %T, want *tg.Document", document.Document)
		}
		if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, c.id, "same file copy", 1424803, file.ID, 0); err != nil {
			return fmt.Errorf("reference same file in another dialog: %w", err)
		}
		if _, err = f.store.FileForDownload(f.ctx, file.ID, file.AccessHash, c.id); err != nil {
			return fmt.Errorf("download before deletion: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("upload and share media: %v", err)
	}

	if _, err := callDeleteHistory(a, f.ctx, peerUser(a.id, b.id), 0, true, true); err != nil {
		t.Fatalf("revoke first private history: %v", err)
	}
	remainingHistory, err := getSmokeHistory(c, f.ctx, peerUser(c.id, a.id))
	if err != nil || lenHistory(remainingHistory) != 1 {
		t.Fatalf("remaining copy history = %d, err=%v, want one", lenHistory(remainingHistory), err)
	}
	fileMessage, ok := remainingHistory.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("remaining copy message = %T, want *tg.Message", remainingHistory.Messages[0])
	}
	media, ok := fileMessage.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("remaining copy media = %T, want document", fileMessage.Media)
	}
	document, ok := media.Document.(*tg.Document)
	if !ok {
		t.Fatalf("remaining copy document = %T, want *tg.Document", media.Document)
	}
	if _, err = f.store.FileForDownload(f.ctx, document.ID, document.AccessHash, c.id); err != nil {
		t.Fatalf("remaining reference lost file access: %v", err)
	}
	if _, err = callDeleteHistory(a, f.ctx, peerUser(a.id, c.id), 0, true, true); err != nil {
		t.Fatalf("revoke final private history: %v", err)
	}
	if _, err = f.store.FileForDownload(f.ctx, document.ID, document.AccessHash, c.id); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("file access after final delete = %v, want ErrFileNotFound", err)
	}
}
