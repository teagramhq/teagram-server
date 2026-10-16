package e2e_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tg"
)

func testSmokeUnreadAcknowledgements(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551982101", "+15551982102"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "unread reader", phoneA)
	b := newSmokeClient(t, f, "unread sender", phoneB)
	if err := b.call(f.ctx, func(ctx context.Context, c *tg.Client) error {
		_, err := c.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peerUser(b.id, a.id), Message: "unread content", RandomID: 1982101})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, c *tg.Client) error {
		state, err := c.UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		contents, err := c.MessagesReadMessageContents(ctx, []int{1})
		if err != nil {
			return err
		}
		if contents.Pts != state.Pts || contents.PtsCount != 0 {
			return fmt.Errorf("contents = %+v, want pts %d count 0", contents, state.Pts)
		}
		peer := peerUser(a.id, b.id)
		mentions, err := c.MessagesReadMentions(ctx, &tg.MessagesReadMentionsRequest{Peer: peer})
		if err != nil {
			return err
		}
		if mentions.Pts != state.Pts || mentions.PtsCount != 0 || mentions.Offset != 0 {
			return fmt.Errorf("mentions = %+v", mentions)
		}
		reactions, err := c.MessagesReadReactions(ctx, &tg.MessagesReadReactionsRequest{Peer: peer})
		if err != nil {
			return err
		}
		if reactions.Pts != state.Pts || reactions.PtsCount != 0 || reactions.Offset != 0 {
			return fmt.Errorf("reactions = %+v", reactions)
		}
		unread, err := c.MessagesGetUnreadMentions(ctx, &tg.MessagesGetUnreadMentionsRequest{Peer: peer, Limit: 10})
		if err != nil {
			return err
		}
		page, ok := unread.(*tg.MessagesMessages)
		if !ok || len(page.Messages) != 0 || len(page.Users) != 0 || len(page.Chats) != 0 {
			return fmt.Errorf("unread mentions = %#v", unread)
		}
		difference, err := c.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: state.Pts, Date: state.Date, Qts: state.Qts})
		if err != nil {
			return err
		}
		if _, ok := difference.(*tg.UpdatesDifferenceEmpty); !ok {
			return fmt.Errorf("difference = %T, want empty", difference)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
