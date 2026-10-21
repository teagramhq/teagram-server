package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeHistoryTTL(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15552244001", "+15552244002", "+15552244003"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
	alice := newSmokeClient(t, f, "history ttl Alice", phoneA)
	bob := newSmokeClient(t, f, "history ttl Bob", phoneB)
	charlie := newSmokeClient(t, f, "history ttl Charlie", phoneC)

	const beforeTTL = "before history ttl"
	var oldID int
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(alice.id, bob.id), Message: beforeTTL, RandomID: 2244001,
		})
		if err != nil {
			return err
		}
		message, err := sentSmokeMessage(result, beforeTTL)
		if err != nil {
			return err
		}
		oldID = message.ID
		return nil
	}); err != nil {
		t.Fatalf("send pre-TTL message: %v", err)
	}

	err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerUser(alice.id, bob.id), Period: 2,
		})
		return err
	})
	if !rpcMessageIs(err, "TTL_PERIOD_INVALID") {
		t.Fatalf("invalid TTL period error = %v, want TTL_PERIOD_INVALID", err)
	}

	var setResult tg.UpdatesClass
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		setResult, err = client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerUser(alice.id, bob.id), Period: 86400,
		})
		return err
	}); err != nil {
		t.Fatalf("set one-day TTL: %v", err)
	}
	assertHistoryTTLServiceUpdate(t, setResult, alice.id, bob.id, 86400)
	service, err := bob.seen.waitService(f.ctx, &tg.MessageActionSetMessagesTTL{})
	if err != nil {
		t.Fatalf("Bob service message: %v", err)
	}
	if action, ok := service.svc.Action.(*tg.MessageActionSetMessagesTTL); !ok || action.Period != 86400 {
		t.Fatalf("Bob service action = %#v, want one-day TTL", service.svc.Action)
	}

	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		response, err := client.UsersGetFullUser(ctx, inputUser(alice.id, bob.id))
		if err != nil {
			return err
		}
		if period, ok := response.FullUser.GetTTLPeriod(); !ok || period != 86400 {
			return fmt.Errorf("user full TTL period = %d/%v, want 86400/true", period, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("full user TTL period: %v", err)
	}

	var beforeSendPts int
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		state, err := client.UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		beforeSendPts = state.Pts
		return nil
	}); err != nil {
		t.Fatalf("read Bob pts before TTL message: %v", err)
	}

	const expiringText = "hello"
	const expiringRandomID = int64(2244002)
	var expiringID int
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(alice.id, bob.id), Message: expiringText, RandomID: expiringRandomID,
		})
		if err != nil {
			return err
		}
		message, err := sentSmokeMessage(result, expiringText)
		if err != nil {
			return err
		}
		expiringID = message.ID
		if period, ok := message.GetTTLPeriod(); !ok || period != 86400 {
			return fmt.Errorf("sent TTL period = %d/%v, want 86400/true", period, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("send expiring message: %v", err)
	}

	assertHistoryContainsTTLMessage(t, f.ctx, alice, peerUser(alice.id, bob.id), expiringText, 86400)
	assertHistoryContainsTTLMessage(t, f.ctx, bob, peerUser(bob.id, alice.id), expiringText, 86400)
	assertHistoryContainsPlainMessage(t, f.ctx, alice, bob.id, oldID, beforeTTL)

	message, ok, err := f.store.MessageByRandomID(f.ctx, alice.id, expiringRandomID)
	if err != nil || !ok || message.LocalID != int64(expiringID) || message.PeerLocalID <= 0 {
		t.Fatalf("stored TTL message = %#v, found=%v, err=%v", message, ok, err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect expiry test seam: %v", err)
	}
	defer func() {
		if closeErr := conn.Close(context.Background()); closeErr != nil {
			t.Errorf("close expiry test seam: %v", closeErr)
		}
	}()
	if _, err = conn.Exec(f.ctx, `UPDATE messages SET expires_at = clock_timestamp() - interval '1 second'
		WHERE (owner_id = $1 AND local_id = $2) OR (owner_id = $3 AND local_id = $4)`,
		alice.id, message.LocalID, bob.id, message.PeerLocalID); err != nil {
		t.Fatalf("advance message expiry test seam: %v", err)
	}

	assertHistoryOmitsMessage(t, f.ctx, alice, bob.id, expiringText)
	assertHistoryOmitsMessage(t, f.ctx, bob, alice.id, expiringText)
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		difference, err := client.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: beforeSendPts})
		if err != nil {
			return err
		}
		for _, item := range differenceMessages(difference) {
			if message, ok := item.(*tg.Message); ok && message.Message == expiringText {
				return errors.New("expired message appeared in getDifference")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("getDifference after expiry: %v", err)
	}
	deletedCopies, more, err := f.store.SweepExpiredMessageCopies(f.ctx, store.HistoryTTLSweepBatchSize)
	if err != nil {
		t.Fatalf("sweep expired message copies: %v", err)
	}
	if deletedCopies != 2 || more {
		t.Fatalf("expired copy sweep = deleted %d, more %t, want 2, false", deletedCopies, more)
	}
	deleteUpdate := recvOrCtx(t, f.ctx, bob.seen.delMsgPts, "Bob's open-session TTL delete update")
	if len(deleteUpdate.Messages) != 1 || deleteUpdate.Messages[0] != expiringID || deleteUpdate.PtsCount != 1 {
		t.Fatalf("Bob delete update = %+v, want message %d with pts_count 1", deleteUpdate, expiringID)
	}
	assertHistoryOmitsMessage(t, f.ctx, bob, alice.id, expiringText)

	// A repeated value is a no-op and its response carries no new updates.
	var noOp tg.UpdatesClass
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		noOp, err = client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerUser(alice.id, bob.id), Period: 86400,
		})
		return err
	}); err != nil {
		t.Fatalf("repeat one-day TTL: %v", err)
	}
	if updates, ok := noOp.(*tg.Updates); !ok || len(updates.Updates) != 0 {
		t.Fatalf("same-value response = %#v, want empty updates", noOp)
	}
	quietCtx, cancelQuiet := context.WithTimeout(f.ctx, 150*time.Millisecond)
	defer cancelQuiet()
	if err := bob.seen.waitNoService(quietCtx, &tg.MessageActionSetMessagesTTL{}); err != nil {
		t.Fatalf("same-value update: %v", err)
	}

	chat, err := f.store.CreateChat(f.ctx, alice.id, "TTL group", []int64{bob.id})
	if err != nil {
		t.Fatalf("create TTL group: %v", err)
	}
	if _, _, err := f.store.SetChatDefaultBannedRights(f.ctx, chat.ID, alice.id, []string{"change_info"}); err != nil {
		t.Fatalf("restrict group title changes: %v", err)
	}
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, Period: 604800,
		})
		return err
	}); !rpcMessageIs(err, "CHAT_ADMIN_REQUIRED") {
		t.Fatalf("basic-group member set TTL error = %v, want CHAT_ADMIN_REQUIRED", err)
	}
	var groupSetResult tg.UpdatesClass
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		groupSetResult, err = client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, Period: 604800,
		})
		return err
	}); err != nil {
		t.Fatalf("creator set group TTL: %v", err)
	}
	assertGroupHistoryTTLServiceUpdate(t, groupSetResult, alice.id, chat.ID, 604800)
	groupService, err := bob.seen.waitService(f.ctx, &tg.MessageActionSetMessagesTTL{})
	if err != nil {
		t.Fatalf("Bob group TTL service message: %v", err)
	}
	if action, ok := groupService.svc.Action.(*tg.MessageActionSetMessagesTTL); !ok || action.Period != 604800 {
		t.Fatalf("Bob group TTL service action = %#v, want one-week TTL", groupService.svc.Action)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		response, err := client.MessagesGetFullChat(ctx, chat.ID)
		if err != nil {
			return err
		}
		full, ok := response.FullChat.(*tg.ChatFull)
		if !ok {
			return fmt.Errorf("full chat = %T, want *tg.ChatFull", response.FullChat)
		}
		if period, ok := full.GetTTLPeriod(); !ok || period != 604800 {
			return fmt.Errorf("chat full TTL period = %d/%v, want 604800/true", period, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("full chat TTL period: %v", err)
	}

	const groupText = "group message with TTL"
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: groupText, RandomID: 2244003,
		})
		if err != nil {
			return err
		}
		message, err := sentSmokeMessage(result, groupText)
		if err != nil {
			return err
		}
		if period, ok := message.GetTTLPeriod(); !ok || period != 604800 {
			return fmt.Errorf("sent group TTL period = %d/%v, want 604800/true", period, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("send group TTL message: %v", err)
	}
	groupPeer := &tg.InputPeerChat{ChatID: chat.ID}
	assertHistoryContainsTTLMessage(t, f.ctx, alice, groupPeer, groupText, 604800)
	assertHistoryContainsTTLMessage(t, f.ctx, bob, groupPeer, groupText, 604800)

	channel, err := f.store.CreateChannel(f.ctx, alice.id, "TTL channel", "", true)
	if err != nil {
		t.Fatalf("create TTL channel: %v", err)
	}
	if joinedID := importChannelInvite(t, f.ctx, bob.cmds, exportChannelInvite(t, f.ctx, alice.id, alice.cmds, channel.ID)); joinedID != channel.ID {
		t.Fatalf("Bob joined channel %d, want %d", joinedID, channel.ID)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerChannel(alice.id, channel.ID), Period: 2592000,
		})
		return err
	}); err != nil {
		t.Fatalf("creator set channel TTL: %v", err)
	}
	channelService, err := bob.seen.waitService(f.ctx, &tg.MessageActionSetMessagesTTL{})
	if err != nil {
		t.Fatalf("Bob channel TTL service message: %v", err)
	}
	if action, ok := channelService.svc.Action.(*tg.MessageActionSetMessagesTTL); !ok || action.Period != 2592000 {
		t.Fatalf("Bob channel TTL service action = %#v, want one-month TTL", channelService.svc.Action)
	}
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		response, err := client.ChannelsGetFullChannel(ctx, inputChannel(alice.id, channel.ID))
		if err != nil {
			return err
		}
		full, ok := response.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("full channel = %T, want *tg.ChannelFull", response.FullChat)
		}
		if period, ok := full.GetTTLPeriod(); !ok || period != 2592000 {
			return fmt.Errorf("channel full TTL period = %d/%v, want 2592000/true", period, ok)
		}
		return nil
	}); err != nil {
		t.Fatalf("full channel TTL period: %v", err)
	}

	const channelText = "channel post with TTL"
	var channelPostID int
	if err := alice.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(alice.id, channel.ID), Message: channelText, RandomID: 2244004,
		})
		if err != nil {
			return err
		}
		message, err := sentChannelSmokeMessage(result, channelText)
		if err != nil {
			return err
		}
		if period, ok := message.GetTTLPeriod(); !ok || period != 2592000 {
			return fmt.Errorf("sent channel TTL period = %d/%v, want 2592000/true", period, ok)
		}
		channelPostID = message.ID
		return nil
	}); err != nil {
		t.Fatalf("send channel TTL post: %v", err)
	}
	assertHistoryContainsTTLMessage(t, f.ctx, alice, peerChannel(alice.id, channel.ID), channelText, 2592000)
	assertHistoryContainsTTLMessage(t, f.ctx, bob, peerChannel(bob.id, channel.ID), channelText, 2592000)
	if _, err = conn.Exec(f.ctx, `UPDATE channel_messages SET expires_at = clock_timestamp() - interval '1 second'
		WHERE channel_id = $1 AND local_id = $2`, channel.ID, channelPostID); err != nil {
		t.Fatalf("advance channel post expiry test seam: %v", err)
	}
	deletedPosts, more, err := f.store.SweepExpiredChannelPosts(f.ctx, store.HistoryTTLSweepBatchSize)
	if err != nil {
		t.Fatalf("sweep expired channel posts: %v", err)
	}
	if deletedPosts != 1 || more {
		t.Fatalf("expired channel post sweep = deleted %d, more %t, want 1, false", deletedPosts, more)
	}
	channelDelete := recvOrCtx(t, f.ctx, bob.seen.delChannelMsg, "Bob's open-session channel TTL delete update")
	if channelDelete.ChannelID != channel.ID || len(channelDelete.Messages) != 1 || channelDelete.Messages[0] != channelPostID || channelDelete.PtsCount != 1 {
		t.Fatalf("Bob channel delete update = %+v, want channel %d post %d with pts_count 1", channelDelete, channel.ID, channelPostID)
	}
	assertHistoryOmitsPeerMessage(t, f.ctx, bob, peerChannel(bob.id, channel.ID), channelText)

	if _, err := f.store.BlockUser(f.ctx, charlie.id, bob.id); err != nil {
		t.Fatalf("block Bob for refusal case: %v", err)
	}
	var blockedNoOp tg.UpdatesClass
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		blockedNoOp, err = client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerUser(bob.id, charlie.id), Period: 0,
		})
		return err
	}); err != nil {
		t.Fatalf("set current TTL when peer blocked caller: %v", err)
	}
	if updates, ok := blockedNoOp.(*tg.Updates); !ok || len(updates.Updates) != 0 {
		t.Fatalf("blocked peer same-value response = %#v, want empty updates", blockedNoOp)
	}
	blockedQuietCtx, cancelBlockedQuiet := context.WithTimeout(f.ctx, 150*time.Millisecond)
	defer cancelBlockedQuiet()
	if err := charlie.seen.waitNoService(blockedQuietCtx, &tg.MessageActionSetMessagesTTL{}); err != nil {
		t.Fatalf("same-value request sent a service message to blocked peer: %v", err)
	}
	if err := bob.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSetHistoryTTL(ctx, &tg.MessagesSetHistoryTTLRequest{
			Peer: peerUser(bob.id, charlie.id), Period: 86400,
		})
		return err
	}); !rpcMessageIs(err, "USER_IS_BLOCKED") {
		t.Fatalf("blocked caller set TTL error = %v, want USER_IS_BLOCKED", err)
	}
}

func assertHistoryTTLServiceUpdate(t *testing.T, result tg.UpdatesClass, actorID, peerID int64, period int) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("setHistoryTTL result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		service, ok := newMessage.Message.(*tg.MessageService)
		if !ok {
			continue
		}
		from, fromOK := service.FromID.(*tg.PeerUser)
		peer, peerOK := service.PeerID.(*tg.PeerUser)
		if !fromOK || from.UserID != actorID || !peerOK || peer.UserID != peerID {
			continue
		}
		action, ok := service.Action.(*tg.MessageActionSetMessagesTTL)
		if ok && action.Period == period {
			return
		}
	}
	t.Fatalf("setHistoryTTL updates = %#v, want period %d service update from %d to %d", updates.Updates, period, actorID, peerID)
}

func assertGroupHistoryTTLServiceUpdate(t *testing.T, result tg.UpdatesClass, actorID, chatID int64, period int) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("group setHistoryTTL result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		service, ok := newMessage.Message.(*tg.MessageService)
		if !ok {
			continue
		}
		from, fromOK := service.FromID.(*tg.PeerUser)
		peer, peerOK := service.PeerID.(*tg.PeerChat)
		action, actionOK := service.Action.(*tg.MessageActionSetMessagesTTL)
		if fromOK && from.UserID == actorID && peerOK && peer.ChatID == chatID && actionOK && action.Period == period {
			return
		}
	}
	t.Fatalf("group setHistoryTTL updates = %#v, want period %d service update from %d to chat %d", updates.Updates, period, actorID, chatID)
}

func assertHistoryContainsTTLMessage(t *testing.T, ctx context.Context, caller *smokeClient, peer tg.InputPeerClass, text string, period int) {
	t.Helper()
	if err := caller.call(ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, Limit: 20,
		})
		if err != nil {
			return err
		}
		var messages []tg.MessageClass
		switch history := result.(type) {
		case *tg.MessagesMessages:
			messages = history.Messages
		case *tg.MessagesChannelMessages:
			messages = history.Messages
		default:
			return fmt.Errorf("history = %T, want messages response", result)
		}
		for _, item := range messages {
			message, ok := item.(*tg.Message)
			if !ok || message.Message != text {
				continue
			}
			got, ok := message.GetTTLPeriod()
			if ok && got == period {
				return nil
			}
			return fmt.Errorf("history message TTL period = %d/%v, want %d/true", got, ok, period)
		}
		return fmt.Errorf("history omitted TTL message %q", text)
	}); err != nil {
		t.Fatalf("read TTL message as %s: %v", caller.label, err)
	}
}

func sentChannelSmokeMessage(result tg.UpdatesClass, text string) (*tg.Message, error) {
	updates, ok := result.(*tg.Updates)
	if !ok {
		return nil, fmt.Errorf("channel send result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		message, ok := newMessage.Message.(*tg.Message)
		if ok && message.Message == text {
			return message, nil
		}
	}
	return nil, fmt.Errorf("channel send result omitted %q", text)
}

func assertHistoryContainsPlainMessage(t *testing.T, ctx context.Context, caller *smokeClient, peerID int64, id int, text string) {
	t.Helper()
	if err := caller.call(ctx, func(ctx context.Context, client *tg.Client) error {
		history, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peerUser(caller.id, peerID), Limit: 20,
		})
		if err != nil {
			return err
		}
		messages, ok := history.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("history = %T, want *tg.MessagesMessages", history)
		}
		for _, item := range messages.Messages {
			message, ok := item.(*tg.Message)
			if !ok || message.ID != id || message.Message != text {
				continue
			}
			if _, hasTTL := message.GetTTLPeriod(); hasTTL {
				return errors.New("pre-TTL message was stamped")
			}
			return nil
		}
		return fmt.Errorf("history omitted pre-TTL message %q", text)
	}); err != nil {
		t.Fatalf("read pre-TTL message as %s: %v", caller.label, err)
	}
}

func assertHistoryOmitsMessage(t *testing.T, ctx context.Context, caller *smokeClient, peerID int64, text string) {
	t.Helper()
	assertHistoryOmitsPeerMessage(t, ctx, caller, peerUser(caller.id, peerID), text)
}

func assertHistoryOmitsPeerMessage(t *testing.T, ctx context.Context, caller *smokeClient, peer tg.InputPeerClass, text string) {
	t.Helper()
	if err := caller.call(ctx, func(ctx context.Context, client *tg.Client) error {
		history, err := client.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, Limit: 20,
		})
		if err != nil {
			return err
		}
		var messages []tg.MessageClass
		switch result := history.(type) {
		case *tg.MessagesMessages:
			messages = result.Messages
		case *tg.MessagesChannelMessages:
			messages = result.Messages
		default:
			return fmt.Errorf("history = %T, want messages response", history)
		}
		for _, item := range messages {
			if message, ok := item.(*tg.Message); ok && message.Message == text {
				return fmt.Errorf("expired message %q is readable", text)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("history after expiry as %s: %v", caller.label, err)
	}
}

func sentSmokeMessage(result tg.UpdatesClass, text string) (*tg.Message, error) {
	updates, ok := result.(*tg.Updates)
	if !ok {
		return nil, fmt.Errorf("send result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := newMessage.Message.(*tg.Message)
		if ok && message.Message == text {
			return message, nil
		}
	}
	return nil, fmt.Errorf("send result omitted %q", text)
}

func differenceMessages(difference tg.UpdatesDifferenceClass) []tg.MessageClass {
	switch result := difference.(type) {
	case *tg.UpdatesDifference:
		return result.NewMessages
	case *tg.UpdatesDifferenceSlice:
		return result.NewMessages
	default:
		return nil
	}
}

func rpcMessageIs(err error, want string) bool {
	rpc, ok := tgerr.As(err)
	return ok && rpc.Message == want
}
