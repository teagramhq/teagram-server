package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestScheduledMessageManagementCalls(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15552241001", "+15552241002"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "scheduled management owner", phoneA)
	b := newSmokeClient(t, f, "scheduled management peer", phoneB)

	chat, err := f.store.CreateChat(f.ctx, a.id, "Scheduled management group", []int64{b.id})
	if err != nil {
		t.Fatalf("create basic group: %v", err)
	}
	privateMessage, _, _, _, err := f.store.SendMessage(f.ctx, a.id, b.id, "private history survives", 2241001, 0, 0) //nolint:dogsled // only the sender message is needed
	if err != nil {
		t.Fatalf("seed private message: %v", err)
	}
	if _, _, _, err := f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: chat.ID, FromID: a.id, Text: "group history survives", RandomID: 2241002,
	}); err != nil {
		t.Fatalf("seed group message: %v", err)
	}
	groupMessages, err := f.store.History(f.ctx, a.id, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil || len(groupMessages) != 1 {
		t.Fatalf("read seeded group message: count=%d err=%v", len(groupMessages), err)
	}
	inaccessibleChat, err := f.store.CreateChat(f.ctx, b.id, "Unresolvable scheduled group", nil)
	if err != nil {
		t.Fatalf("create inaccessible group: %v", err)
	}

	listener, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect notification observer: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(context.Background()); err != nil {
			t.Errorf("close notification observer: %v", err)
		}
	})
	if _, err := listener.Exec(f.ctx, "LISTEN "+store.ChannelUpdates); err != nil {
		t.Fatalf("listen for update publication: %v", err)
	}

	before := scheduledManagementEffects(t, f, a.id, b.id, chat.ID)
	if err := a.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		privateIDs := []int{int(privateMessage.LocalID)}
		for _, ids := range [][]int{nil, privateIDs} {
			result, err := client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{
				Peer: peerUser(a.id, b.id), ID: ids,
			})
			label := fmt.Sprintf("delete private scheduled IDs %v", ids)
			if err != nil {
				return fmt.Errorf("%s: %w", label, err)
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("%s returned %T, want *tg.Updates", label, result)
			}
			if len(updates.Updates) != 0 || len(updates.Users) != 0 || len(updates.Chats) != 0 {
				return fmt.Errorf("%s returned updates/users/chats %d/%d/%d, want all empty", label, len(updates.Updates), len(updates.Users), len(updates.Chats))
			}
		}
		for _, ids := range [][]int{privateIDs, nil} {
			result, err := client.MessagesSendScheduledMessages(ctx, &tg.MessagesSendScheduledMessagesRequest{
				Peer: peerUser(a.id, b.id), ID: ids,
			})
			if len(ids) > 0 {
				if err == nil || !tgerr.Is(err, "MESSAGE_ID_INVALID") {
					return fmt.Errorf("send private scheduled IDs %v error: want MESSAGE_ID_INVALID: %w", ids, err)
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("send empty private scheduled IDs: %w", err)
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("send empty private scheduled IDs returned %T, want *tg.Updates", result)
			}
			if len(updates.Updates) != 0 || len(updates.Users) != 0 || len(updates.Chats) != 0 {
				return fmt.Errorf("send empty private scheduled IDs returned updates/users/chats %d/%d/%d, want all empty", len(updates.Updates), len(updates.Users), len(updates.Chats))
			}
		}

		groupIDs := []int{int(groupMessages[0].LocalID)}
		for _, ids := range [][]int{nil, groupIDs} {
			deleted, err := client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{
				Peer: &tg.InputPeerChat{ChatID: chat.ID}, ID: ids,
			})
			if err != nil {
				return fmt.Errorf("delete group scheduled IDs %v: %w", ids, err)
			}
			groupUpdates, ok := deleted.(*tg.Updates)
			if !ok {
				return fmt.Errorf("delete group result = %T, want *tg.Updates", deleted)
			}
			if len(groupUpdates.Updates) != 0 || len(groupUpdates.Users) != 0 || len(groupUpdates.Chats) != 0 {
				return fmt.Errorf("delete group returned updates/users/chats %d/%d/%d, want all empty", len(groupUpdates.Updates), len(groupUpdates.Users), len(groupUpdates.Chats))
			}
		}
		if _, err := client.MessagesSendScheduledMessages(ctx, &tg.MessagesSendScheduledMessagesRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, ID: groupIDs,
		}); err == nil || !tgerr.Is(err, "MESSAGE_ID_INVALID") {
			return fmt.Errorf("send group scheduled messages: want MESSAGE_ID_INVALID: %w", err)
		}

		missingPeer := peerUser(a.id, 9223372036854770000)
		if _, err := client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{Peer: missingPeer, ID: []int{5}}); err == nil || !tgerr.Is(err, "PEER_ID_INVALID") {
			return fmt.Errorf("delete for unresolvable private peer: want PEER_ID_INVALID: %w", err)
		}
		if _, err := client.MessagesSendScheduledMessages(ctx, &tg.MessagesSendScheduledMessagesRequest{Peer: missingPeer, ID: []int{5}}); err == nil || !tgerr.Is(err, "PEER_ID_INVALID") {
			return fmt.Errorf("send for unresolvable private peer: want PEER_ID_INVALID: %w", err)
		}
		if _, err := client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{Peer: &tg.InputPeerChat{ChatID: inaccessibleChat.ID}}); err == nil || !tgerr.Is(err, "PEER_ID_INVALID") {
			return fmt.Errorf("delete for inaccessible group: want PEER_ID_INVALID: %w", err)
		}
		if _, err := client.MessagesSendScheduledMessages(ctx, &tg.MessagesSendScheduledMessagesRequest{Peer: &tg.InputPeerChat{ChatID: inaccessibleChat.ID}, ID: []int{5}}); err == nil || !tgerr.Is(err, "PEER_ID_INVALID") {
			return fmt.Errorf("send for inaccessible group: want PEER_ID_INVALID: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("scheduled-message management calls: %v", err)
	}

	after := scheduledManagementEffects(t, f, a.id, b.id, chat.ID)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("scheduled-message management changed stored messages, pts, or events: before=%+v after=%+v", before, after)
	}
	waitCtx, cancel := context.WithTimeout(f.ctx, 150*time.Millisecond)
	defer cancel()
	if notification, err := listener.WaitForNotification(waitCtx); err == nil {
		t.Fatalf("scheduled-message management published an update notification: %+v", notification)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for scheduled-message update notification: %v", err)
	}
}

type scheduledManagementSnapshot struct {
	state   store.State
	events  []store.Event
	private []store.Message
	group   []store.Message
}

func scheduledManagementEffects(t *testing.T, f *smokeFixture, ownerID, peerID, chatID int64) map[int64]scheduledManagementSnapshot {
	t.Helper()
	out := make(map[int64]scheduledManagementSnapshot, 2)
	for _, userID := range []int64{ownerID, peerID} {
		state, err := f.store.StateWithoutUnread(f.ctx, userID)
		if err != nil {
			t.Fatalf("read update state for %d: %v", userID, err)
		}
		events, err := f.store.EventsSince(f.ctx, userID, 0)
		if err != nil {
			t.Fatalf("read update events for %d: %v", userID, err)
		}
		private, err := f.store.History(f.ctx, userID, store.PeerTypeUser, map[int64]int64{ownerID: peerID, peerID: ownerID}[userID], 0, 10)
		if err != nil {
			t.Fatalf("read private history for %d: %v", userID, err)
		}
		group, err := f.store.History(f.ctx, userID, store.PeerTypeChat, chatID, 0, 10)
		if err != nil {
			t.Fatalf("read group history for %d: %v", userID, err)
		}
		out[userID] = scheduledManagementSnapshot{state: state, events: events, private: private, group: group}
	}
	return out
}

func TestScheduledMessageManagementRequiresBoundSession(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	unbound := f.savedSessionClient(&session.StorageMemory{})
	if err := unbound.Run(f.ctx, func(ctx context.Context) error {
		client := tg.NewClient(unbound)
		if _, err := client.MessagesDeleteScheduledMessages(ctx, &tg.MessagesDeleteScheduledMessagesRequest{Peer: &tg.InputPeerSelf{}}); err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("unbound deleteScheduledMessages: want AUTH_KEY_UNREGISTERED: %w", err)
		}
		if _, err := client.MessagesSendScheduledMessages(ctx, &tg.MessagesSendScheduledMessagesRequest{Peer: &tg.InputPeerSelf{}, ID: []int{5}}); err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			return fmt.Errorf("unbound sendScheduledMessages: want AUTH_KEY_UNREGISTERED: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("unbound scheduled-message management calls: %v", err)
	}
}
