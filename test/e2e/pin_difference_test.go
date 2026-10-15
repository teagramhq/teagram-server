package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

// TestPinDifferenceChat proves a basic-group member offline during a pin and
// an unpin replays both from updates.getDifference, in order and with no pts
// gap, and that a no-op unpin advances no member's pts.
func TestPinDifferenceChat(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const phoneA, phoneB, phoneC = "+15551868021", "+15551868022", "+15551868023"
	f := newUnpinAllFixture(t, ctx, phoneA, phoneB, phoneC)

	sessC := &session.StorageMemory{}
	var cUserID int64
	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		self, err := c.Self(ctx)
		if err != nil {
			return err
		}
		cUserID = self.ID
		return nil
	}); err != nil {
		t.Fatalf("C sign in: %v", err)
	}

	var chatID int64
	var msgID int
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		inv, err := c.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Pin Difference",
			Users: []tg.InputUserClass{inputUser(f.aUserID, f.bUserID), inputUser(f.aUserID, cUserID)},
		})
		if err != nil {
			return err
		}
		ups, ok := inv.Updates.(*tg.Updates)
		if !ok || len(ups.Chats) == 0 {
			return errors.New("createChat: unexpected updates")
		}
		chatID = ups.Chats[0].GetID()
		res, err := c.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "pin while offline", RandomID: 1868021,
		})
		if err != nil {
			return err
		}
		msg, _, ok := outgoingMessage(t, res, "pin while offline")
		if !ok {
			return errors.New("send: no outgoing message")
		}
		msgID = msg.ID
		return nil
	}); err != nil {
		t.Fatalf("A create and send: %v", err)
	}

	// C records its copy and pts, then goes offline.
	var cCopy, fromPts int
	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		id, err := smokeGroupMessageID(ctx, c.API(), chatID, "pin while offline")
		if err != nil {
			return err
		}
		cCopy = id
		state, err := c.API().UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		fromPts = state.Pts
		return nil
	}); err != nil {
		t.Fatalf("C read copy and state: %v", err)
	}

	pin := func(req *tg.MessagesUpdatePinnedMessageRequest) {
		t.Helper()
		req.Peer = &tg.InputPeerChat{ChatID: chatID}
		if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
			_, err := c.MessagesUpdatePinnedMessage(ctx, req)
			return err
		}); err != nil {
			t.Fatalf("A updatePinnedMessage %+v: %v", req, err)
		}
	}
	pin(&tg.MessagesUpdatePinnedMessageRequest{ID: msgID})
	recvOrCtx(t, ctx, f.collB.pinnedMsg, "B pin push")
	pin(&tg.MessagesUpdatePinnedMessageRequest{ID: msgID, Unpin: true})
	recvOrCtx(t, ctx, f.collB.pinnedMsg, "B unpin push")

	// A no-op unpin leaves every member's pts where the unpin left it.
	ptsOf := func(userID int64) int {
		t.Helper()
		state, err := f.st.StateWithoutUnread(ctx, userID)
		if err != nil {
			t.Fatalf("state %d: %v", userID, err)
		}
		return state.Pts
	}
	settled := map[int64]int{f.aUserID: ptsOf(f.aUserID), f.bUserID: ptsOf(f.bUserID), cUserID: ptsOf(cUserID)}
	pin(&tg.MessagesUpdatePinnedMessageRequest{Unpin: true})
	for userID, want := range settled {
		if got := ptsOf(userID); got != want {
			t.Fatalf("pts of %d after no-op unpin = %d, want %d", userID, got, want)
		}
	}

	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		res, err := c.API().UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: fromPts, Date: int(time.Now().Unix())})
		if err != nil {
			return err
		}
		diff, ok := res.(*tg.UpdatesDifference)
		if !ok {
			return fmt.Errorf("getDifference = %T, want *tg.UpdatesDifference", res)
		}
		var pins []*tg.UpdatePinnedMessages
		for _, u := range diff.OtherUpdates {
			if p, ok := u.(*tg.UpdatePinnedMessages); ok {
				pins = append(pins, p)
			}
		}
		if len(pins) != 2 {
			return fmt.Errorf("difference pin updates = %d (%+v), want 2", len(pins), diff.OtherUpdates)
		}
		for i, p := range pins {
			wantPinned := i == 0
			peer, ok := p.Peer.(*tg.PeerChat)
			if p.Pinned != wantPinned || !ok || peer.ChatID != chatID || len(p.Messages) != 1 || p.Messages[0] != cCopy {
				return fmt.Errorf("difference pin %d = %+v, want pinned=%t chat %d message %d", i, p, wantPinned, chatID, cCopy)
			}
			if p.Pts != fromPts+i+1 || p.PtsCount != 1 {
				return fmt.Errorf("difference pin %d pts = %d/%d, want %d/1", i, p.Pts, p.PtsCount, fromPts+i+1)
			}
		}
		if diff.State.Pts != fromPts+2 {
			return fmt.Errorf("difference state pts = %d, want %d", diff.State.Pts, fromPts+2)
		}
		return nil
	}); err != nil {
		t.Fatalf("C catch up: %v", err)
	}
	f.stop()
}

// TestPinDifferenceChannel proves a channel member offline during a pin and an
// unpin replays both from updates.getChannelDifference, in order and with no
// pts gap, and that a no-op unpin advances no channel pts.
func TestPinDifferenceChannel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const phoneA, phoneB, phoneC = "+15551868031", "+15551868032", "+15551868033"
	f := newUnpinAllFixture(t, ctx, phoneA, phoneB, phoneC)

	var channelID int64
	var inviteLink string
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title: "Pin Difference Channel", Megagroup: true,
		})
		if err != nil {
			return err
		}
		ups, ok := res.(*tg.Updates)
		if !ok || len(ups.Chats) == 0 {
			return errors.New("createChannel: unexpected result")
		}
		channelID = ups.Chats[0].GetID()
		inv, err := c.MessagesExportChatInvite(ctx, &tg.MessagesExportChatInviteRequest{Peer: peerChannel(f.aUserID, channelID)})
		if err != nil {
			return err
		}
		exported, ok := inv.(*tg.ChatInviteExported)
		if !ok {
			return errors.New("exportChatInvite: unexpected type")
		}
		inviteLink = exported.Link
		return nil
	}); err != nil {
		t.Fatalf("A create channel: %v", err)
	}

	sessC := &session.StorageMemory{}
	var cUserID int64
	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		self, err := c.Self(ctx)
		if err != nil {
			return err
		}
		cUserID = self.ID
		_, err = c.API().MessagesImportChatInvite(ctx, inviteHash(inviteLink))
		return err
	}); err != nil {
		t.Fatalf("C join: %v", err)
	}

	var postID int
	if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
		res, err := c.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(f.aUserID, channelID), Message: "pin while offline", RandomID: 1868031,
		})
		if err != nil {
			return err
		}
		ups, ok := res.(*tg.Updates)
		if !ok {
			return errors.New("send: unexpected result")
		}
		for _, u := range ups.Updates {
			if nm, ok := u.(*tg.UpdateNewChannelMessage); ok {
				postID = nm.Message.GetID()
			}
		}
		if postID == 0 {
			return errors.New("send: no post id")
		}
		return nil
	}); err != nil {
		t.Fatalf("A post: %v", err)
	}
	fromPts, err := f.st.ChannelState(ctx, channelID)
	if err != nil {
		t.Fatalf("channel pts: %v", err)
	}

	pin := func(req *tg.MessagesUpdatePinnedMessageRequest) {
		t.Helper()
		req.Peer = peerChannel(f.aUserID, channelID)
		if err := f.exec(f.aCmds, func(ctx context.Context, c *tg.Client) error {
			_, err := c.MessagesUpdatePinnedMessage(ctx, req)
			return err
		}); err != nil {
			t.Fatalf("A updatePinnedMessage %+v: %v", req, err)
		}
	}
	pin(&tg.MessagesUpdatePinnedMessageRequest{ID: postID})
	pin(&tg.MessagesUpdatePinnedMessageRequest{ID: postID, Unpin: true})
	pin(&tg.MessagesUpdatePinnedMessageRequest{Unpin: true})
	if after, err := f.st.ChannelState(ctx, channelID); err != nil || after != fromPts+2 {
		t.Fatalf("channel pts after pin, unpin and no-op unpin = %d (%v), want %d", after, err, fromPts+2)
	}

	if err := f.session(phoneC, sessC, func(ctx context.Context, c *telegram.Client) error {
		res, err := c.API().UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: inputChannel(cUserID, channelID),
			Filter:  &tg.ChannelMessagesFilterEmpty{},
			Pts:     fromPts,
			Limit:   100,
		})
		if err != nil {
			return err
		}
		diff, ok := res.(*tg.UpdatesChannelDifference)
		if !ok {
			return fmt.Errorf("getChannelDifference = %T, want *tg.UpdatesChannelDifference", res)
		}
		if len(diff.OtherUpdates) != 2 {
			return fmt.Errorf("channel difference other updates = %+v, want pin and unpin", diff.OtherUpdates)
		}
		for i, u := range diff.OtherUpdates {
			p, ok := u.(*tg.UpdatePinnedChannelMessages)
			wantPinned := i == 0
			if !ok || p.Pinned != wantPinned || p.ChannelID != channelID || len(p.Messages) != 1 || p.Messages[0] != postID {
				return fmt.Errorf("channel difference update %d = %+v, want pinned=%t post %d", i, u, wantPinned, postID)
			}
			if p.Pts != fromPts+i+1 || p.PtsCount != 1 {
				return fmt.Errorf("channel difference update %d pts = %d/%d, want %d/1", i, p.Pts, p.PtsCount, fromPts+i+1)
			}
		}
		if diff.Pts != fromPts+2 {
			return fmt.Errorf("channel difference pts = %d, want %d", diff.Pts, fromPts+2)
		}
		return nil
	}); err != nil {
		t.Fatalf("C catch up: %v", err)
	}
	f.stop()
}
