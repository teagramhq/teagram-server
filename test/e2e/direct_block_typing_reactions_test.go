package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"
)

// ownerReactions renders every reaction row an owner holds, so a withheld
// reaction that wrote anything to that owner shows up as a difference.
func ownerReactions(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64) string {
	t.Helper()
	var got string
	if err := conn.QueryRow(ctx, `SELECT COALESCE(string_agg(local_id || ':' || reactor_id || ':' || reaction, ',' ORDER BY local_id, reactor_id), '')
		FROM message_reactions WHERE owner_id = $1`, ownerID).Scan(&got); err != nil {
		t.Fatalf("reactions of owner %d: %v", ownerID, err)
	}
	return got
}

func drain[T any](ch chan T) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// TestDirectTypingAndReactionsAcrossBlock: while B has blocked A, A's typing
// and reactions answer as delivered and reach nothing of B's; while A has
// blocked B, A's typing still answers true and A's reactions are refused; after
// unblock both flow again and nothing withheld is delivered.
func TestDirectTypingAndReactionsAcrossBlock(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551871301", "+15551871302")
	a, b := clients[0], clients[1]
	conn := albumConn(t, ctx, a)

	sendText := func(from, to *mediaClient, text string, rid int64) int {
		var id int
		execChat(t, ctx, from.cmds, func(ctx context.Context, api *tg.Client) error {
			res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peerUser(from.id, to.id), Message: text, RandomID: rid})
			if err == nil {
				id = firstSentMessage(t, res).ID
			}
			return err
		})
		return id
	}
	// A's copies of a message A wrote and of one B wrote, and B's copies of the same.
	aOwn := sendText(a, b, "from a", 1871301)
	bOfA := recvOrCtx(t, ctx, b.coll.newMsg, "B receives A's message").ID
	bOwn := sendText(b, a, "from b", 1871302)
	aOfB := recvOrCtx(t, ctx, a.coll.newMsg, "A receives B's message").ID

	typing := func(from, to *mediaClient) {
		execChat(t, ctx, from.cmds, func(ctx context.Context, api *tg.Client) error {
			ok, err := api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{Peer: peerUser(from.id, to.id), Action: &tg.SendMessageTypingAction{}})
			if err == nil && !ok {
				return fmt.Errorf("setTyping %d -> %d answered false", from.id, to.id)
			}
			return err
		})
	}
	react := func(from, to *mediaClient, msgID int, emoji string) (tg.UpdatesClass, error) {
		var res tg.UpdatesClass
		err := execMedia(t, ctx, from.cmds, func(ctx context.Context, api *tg.Client) error {
			req := &tg.MessagesSendReactionRequest{Peer: peerUser(from.id, to.id), MsgID: msgID}
			if emoji != "" {
				req.SetReaction([]tg.ReactionClass{&tg.ReactionEmoji{Emoticon: emoji}})
			}
			var err error
			res, err = api.MessagesSendReaction(ctx, req)
			return err
		})
		return res, err
	}
	answerShape := func(res tg.UpdatesClass) string {
		u, ok := res.(*tg.Updates)
		if !ok {
			return fmt.Sprintf("%T", res)
		}
		return fmt.Sprintf("updates=%d users=%d chats=%d", len(u.Updates), len(u.Users), len(u.Chats))
	}
	quiet := func(check func(ctx context.Context)) {
		qctx, qcancel := context.WithTimeout(ctx, 2*time.Second)
		defer qcancel()
		check(qctx)
	}

	// Unblocked baseline: typing and reactions reach the peer.
	typing(a, b)
	if from := recvOrCtx(t, ctx, b.coll.typing, "B typing before block"); from != a.id {
		t.Fatalf("typing from = %d, want %d", from, a.id)
	}
	res, err := react(a, b, aOfB, "👍")
	if err != nil {
		t.Fatalf("react before block: %v", err)
	}
	baseline := answerShape(res)
	recvOrCtx(t, ctx, b.coll.msgReactions, "B reaction before block")
	recvOrCtx(t, ctx, a.coll.msgReactions, "A own reaction before block")

	// B blocked A.
	execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
		return err
	})
	drain(b.coll.msgReactions)
	drain(a.coll.msgReactions)
	recipientBefore, reactionsBefore := captureDirectOwner(t, ctx, conn, b.id), ownerReactions(t, ctx, conn, b.id)
	nudges := listenUserNotify(t, ctx, a)

	typing(a, b)
	typing(b, a)
	for _, msgID := range []int{aOfB, aOwn} {
		res, err := react(a, b, msgID, "❤")
		if err != nil {
			t.Fatalf("react on %d while blocked = %v, want the delivered answer", msgID, err)
		}
		if got := answerShape(res); got != baseline {
			t.Fatalf("react while blocked answered %s, want %s", got, baseline)
		}
		if up := recvOrCtx(t, ctx, a.coll.msgReactions, "A own reaction while blocked"); up.MsgID != msgID {
			t.Fatalf("A reaction update for %d, want %d", up.MsgID, msgID)
		}
	}
	if _, err := react(a, b, aOfB, ""); err != nil {
		t.Fatalf("clear while blocked: %v", err)
	}
	recvOrCtx(t, ctx, a.coll.msgReactions, "A own clear while blocked")
	_, err = react(b, a, bOfA, "🔥")
	assertRPCError(t, err, "YOU_BLOCKED_USER")

	quiet(func(q context.Context) { expectNoPush(t, q, b.coll.typing, "typing from a blocked user") })
	quiet(func(q context.Context) { expectNoPush(t, q, a.coll.typing, "typing from the blocker") })
	quiet(func(q context.Context) { expectNoPush(t, q, b.coll.msgReactions, "a reaction from a blocked user") })
	if got := captureDirectOwner(t, ctx, conn, b.id); got != recipientBefore {
		t.Fatalf("blocker state while blocked %+v, want unchanged %+v", got, recipientBefore)
	}
	if got := ownerReactions(t, ctx, conn, b.id); got != reactionsBefore {
		t.Fatalf("blocker reactions while blocked %q, want unchanged %q", got, reactionsBefore)
	}
	if nextUserNotify(t, ctx, nudges, b.id, time.Second) {
		t.Fatal("blocker was nudged while blocked")
	}

	// After unblock nothing withheld arrives; later typing and reactions do.
	execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(b.id, a.id)})
		return err
	})
	if got := ownerReactions(t, ctx, conn, b.id); got != reactionsBefore {
		t.Fatalf("blocker reactions after unblock %q, want unchanged %q", got, reactionsBefore)
	}
	typing(a, b)
	if from := recvOrCtx(t, ctx, b.coll.typing, "B typing after unblock"); from != a.id {
		t.Fatalf("typing after unblock from = %d, want %d", from, a.id)
	}
	if _, err := react(a, b, aOfB, "🎉"); err != nil {
		t.Fatalf("react after unblock: %v", err)
	}
	if up := recvOrCtx(t, ctx, b.coll.msgReactions, "B reaction after unblock"); up.MsgID != bOwn {
		t.Fatalf("B reaction update for %d, want %d", up.MsgID, bOwn)
	}

	// A blocked B: typing still answers true and reaches nothing, reacting is
	// refused with nothing written, and A may still clear its own reaction.
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(a.id, b.id)})
		return err
	})
	drain(b.coll.msgReactions)
	drain(a.coll.msgReactions)
	senderBefore, peerBefore := ownerReactions(t, ctx, conn, a.id), ownerReactions(t, ctx, conn, b.id)
	typing(a, b)
	quiet(func(q context.Context) { expectNoPush(t, q, b.coll.typing, "typing from the blocker") })
	_, err = react(a, b, aOwn, "😀")
	assertRPCError(t, err, "YOU_BLOCKED_USER")
	if got := ownerReactions(t, ctx, conn, a.id); got != senderBefore {
		t.Fatalf("refused reaction changed the reactor's rows: %q, want %q", got, senderBefore)
	}
	if got := ownerReactions(t, ctx, conn, b.id); got != peerBefore {
		t.Fatalf("refused reaction changed the peer's rows: %q, want %q", got, peerBefore)
	}
	if _, err := react(a, b, aOfB, ""); err != nil {
		t.Fatalf("blocker clearing its reaction: %v", err)
	}
	recvOrCtx(t, ctx, b.coll.msgReactions, "B sees the blocker's reaction cleared")
	if got := ownerReactions(t, ctx, conn, b.id); got != "" {
		t.Fatalf("peer reactions after the blocker cleared = %q, want none", got)
	}
}
