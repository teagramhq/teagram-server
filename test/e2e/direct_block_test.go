package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

// directOwnerSnapshot is everything a 1:1 send can change for one owner: the
// update state, the message rows and the event log.
type directOwnerSnapshot struct {
	Pts, NextLocalID, Rows, Events, Dialogs int64
}

func captureDirectOwner(t *testing.T, ctx context.Context, conn *pgx.Conn, ownerID int64) directOwnerSnapshot {
	t.Helper()
	var s directOwnerSnapshot
	if err := conn.QueryRow(ctx, `SELECT
		COALESCE((SELECT pts FROM update_state WHERE user_id = $1), 0),
		COALESCE((SELECT next_local_id FROM update_state WHERE user_id = $1), 0),
		(SELECT count(*) FROM messages WHERE owner_id = $1),
		(SELECT count(*) FROM message_events WHERE owner_id = $1),
		(SELECT count(*) FROM dialogs WHERE owner_id = $1)`, ownerID).
		Scan(&s.Pts, &s.NextLocalID, &s.Rows, &s.Events, &s.Dialogs); err != nil {
		t.Fatalf("snapshot owner %d: %v", ownerID, err)
	}
	return s
}

// directSendShape is the part of a send's answer that must not depend on
// whether the recipient blocked the sender.
func directSendShape(t *testing.T, res tg.UpdatesClass) []string {
	t.Helper()
	updates, ok := res.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", res)
	}
	shape := make([]string, 0, len(updates.Updates)+1)
	for _, u := range updates.Updates {
		shape = append(shape, fmt.Sprintf("%T", u))
	}
	return append(shape, fmt.Sprintf("users=%d", len(updates.Users)))
}

func firstSentMessage(t *testing.T, res tg.UpdatesClass) *tg.Message {
	t.Helper()
	updates, ok := res.(*tg.Updates)
	if !ok {
		t.Fatalf("send result = %T, want *tg.Updates", res)
	}
	for _, u := range updates.Updates {
		if nm, ok := u.(*tg.UpdateNewMessage); ok {
			if m, ok := nm.Message.(*tg.Message); ok {
				return m
			}
		}
	}
	t.Fatalf("send result carries no new message: %+v", res)
	return nil
}

// listenUserNotify subscribes to the cross-replica update nudge, which is
// what reaches a user even when getDifference then has nothing to return.
func listenUserNotify(t *testing.T, ctx context.Context, mc *mediaClient) *pgx.Conn {
	t.Helper()
	conn := albumConn(t, ctx, mc)
	if _, err := conn.Exec(ctx, "LISTEN "+store.ChannelUpdates); err != nil {
		t.Fatalf("listen for update nudges: %v", err)
	}
	return conn
}

// nextUserNotify reports whether a nudge for userID arrives within d,
// skipping nudges for anyone else.
func nextUserNotify(t *testing.T, ctx context.Context, conn *pgx.Conn, userID int64, d time.Duration) bool {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	want := strconv.FormatInt(userID, 10)
	for {
		n, err := conn.WaitForNotification(wait)
		if errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if err != nil {
			t.Fatalf("wait for update nudge: %v", err)
		}
		if n.Payload == want {
			return true
		}
	}
}

func expectNoPush[T any](t *testing.T, ctx context.Context, ch chan T, what string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("recipient received %s: %+v", what, v)
	case <-ctx.Done():
	}
}

// TestDirectSendsFromBlockedSender: every 1:1 send path from a sender the
// recipient blocked answers exactly as a delivered send and leaves the
// recipient untouched, follow-ups on the withheld copy stay on the sender's
// side, unblocking delivers only later sends, and a sender who blocked the
// recipient gets YOU_BLOCKED_USER with nothing written.
func TestDirectSendsFromBlockedSender(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551862901", "+15551862902", "+15551862903")
	a, b, c := clients[0], clients[1], clients[2]
	conn := albumConn(t, ctx, a)

	var saved *tg.Message
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: &tg.InputPeerSelf{}, Message: "saved", RandomID: 1862900})
		if err == nil {
			saved = firstSentMessage(t, res)
		}
		return err
	})

	nextID := int64(1862910)
	rid := func() int64 { nextID++; return nextID }
	var textRandomID, forwardRandomID int64
	paths := []struct {
		name string
		send func(peerID int64) (tg.UpdatesClass, error)
	}{
		{"sendMessage", func(peerID int64) (res tg.UpdatesClass, err error) {
			textRandomID = rid()
			err = execMedia(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
				res, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peerUser(a.id, peerID), Message: "text", RandomID: textRandomID})
				return err
			})
			return res, err
		}},
		{"sendMedia", func(peerID int64) (res tg.UpdatesClass, err error) {
			fileID := rid()
			parts := uploadParts(t, ctx, a, fileID, mediaPayload(512))
			err = execMedia(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
				res, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
					Peer: peerUser(a.id, peerID), RandomID: rid(),
					Media: &tg.InputMediaUploadedDocument{File: &tg.InputFile{ID: fileID, Parts: parts, Name: "f.bin"}, MimeType: "application/octet-stream"},
				})
				return err
			})
			return res, err
		}},
		{"poll", func(peerID int64) (res tg.UpdatesClass, err error) {
			err = execMedia(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
				res, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
					Peer: peerUser(a.id, peerID), RandomID: rid(),
					Media: &tg.InputMediaPoll{Poll: tg.Poll{
						Question: tg.TextWithEntities{Text: "Which?"},
						Answers: []tg.PollAnswerClass{
							&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "One"}},
							&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Two"}},
						},
					}},
				})
				return err
			})
			return res, err
		}},
		{"sendMultiMedia", func(peerID int64) (tg.UpdatesClass, error) {
			first := uploadMediaDocument(t, ctx, a, peerID, rid(), "1.bin", []byte("one"))
			second := uploadMediaDocument(t, ctx, a, peerID, rid(), "2.bin", []byte("two"))
			return sendAlbum(t, ctx, a, peerUser(a.id, peerID), albumDocumentItem(first, rid()), albumDocumentItem(second, rid()))
		}},
		{"forwardMessages", func(peerID int64) (res tg.UpdatesClass, err error) {
			forwardRandomID = rid()
			err = execMedia(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
				res, err = api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
					FromPeer: &tg.InputPeerSelf{}, ID: []int{saved.ID}, RandomID: []int64{forwardRandomID}, ToPeer: peerUser(a.id, peerID),
				})
				return err
			})
			return res, err
		}},
	}

	// The unblocked answer each path gives, to a third user.
	baseline := make(map[string][]string, len(paths))
	for _, p := range paths {
		res, err := p.send(c.id)
		if err != nil {
			t.Fatalf("%s to an unblocked peer: %v", p.name, err)
		}
		baseline[p.name] = directSendShape(t, res)
	}

	execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
		return err
	})
	recipientBefore := captureDirectOwner(t, ctx, conn, b.id)

	withheld := make(map[string]*tg.Message, len(paths))
	for _, p := range paths {
		senderBefore := captureDirectOwner(t, ctx, conn, a.id)
		res, err := p.send(b.id)
		if err != nil {
			t.Fatalf("%s while blocked = %v, want the delivered answer", p.name, err)
		}
		if got := directSendShape(t, res); !reflect.DeepEqual(got, baseline[p.name]) {
			t.Fatalf("%s while blocked answered %v, want %v", p.name, got, baseline[p.name])
		}
		if after := captureDirectOwner(t, ctx, conn, a.id); after.Pts <= senderBefore.Pts {
			t.Fatalf("%s while blocked: sender pts %d -> %d, want advanced", p.name, senderBefore.Pts, after.Pts)
		}
		withheld[p.name] = firstSentMessage(t, res)
	}

	text, withheldRandomID, withheldForwardID := withheld["sendMessage"], textRandomID, forwardRandomID
	nudges := listenUserNotify(t, ctx, a)
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{Peer: peerUser(a.id, b.id), ID: text.ID, Message: "edited"})
		return err
	})
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendReaction(ctx, &tg.MessagesSendReactionRequest{
			Peer: peerUser(a.id, b.id), MsgID: text.ID, Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: "❤"}},
		})
		return err
	})
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{Revoke: true, ID: []int{withheld["sendMedia"].ID}})
		return err
	})
	pollMedia, ok := withheld["poll"].Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("withheld poll media = %T", withheld["poll"].Media)
	}
	closed := pollMedia.Poll
	closed.SetClosed(true)
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		req := &tg.MessagesEditMessageRequest{Peer: peerUser(a.id, b.id), ID: withheld["poll"].ID}
		req.SetMedia(&tg.InputMediaPoll{Poll: closed})
		_, err := api.MessagesEditMessage(ctx, req)
		return err
	})
	retryForward := func() {
		execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
			res, err := api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
				FromPeer: &tg.InputPeerSelf{}, ID: []int{saved.ID}, RandomID: []int64{withheldForwardID}, ToPeer: peerUser(a.id, b.id),
			})
			if err == nil && firstSentMessage(t, res).ID != withheld["forwardMessages"].ID {
				return fmt.Errorf("forward retry answered %+v, want message %d", res, withheld["forwardMessages"].ID)
			}
			return err
		})
	}
	retryForward()
	resend := func() int {
		var id int
		execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
			res, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peerUser(a.id, b.id), Message: "text", RandomID: withheldRandomID})
			if err == nil {
				id = firstSentMessage(t, res).ID
			}
			return err
		})
		return id
	}
	if id := resend(); id != text.ID {
		t.Fatalf("resend while blocked = message %d, want %d", id, text.ID)
	}

	quiet, quietCancel := context.WithTimeout(ctx, 2*time.Second)
	expectNoPush(t, quiet, b.coll.newMsg, "a new message")
	quietCancel()
	quiet, quietCancel = context.WithTimeout(ctx, 200*time.Millisecond)
	expectNoPush(t, quiet, b.coll.editMsg, "an edit")
	quietCancel()
	quiet, quietCancel = context.WithTimeout(ctx, 200*time.Millisecond)
	expectNoPush(t, quiet, b.coll.msgReactions, "a reaction")
	quietCancel()
	quiet, quietCancel = context.WithTimeout(ctx, 200*time.Millisecond)
	expectNoPush(t, quiet, b.coll.delMsg, "a delete")
	quietCancel()
	if got := captureDirectOwner(t, ctx, conn, b.id); got != recipientBefore {
		t.Fatalf("recipient while blocked %+v, want unchanged %+v", got, recipientBefore)
	}
	if nextUserNotify(t, ctx, nudges, b.id, time.Second) {
		t.Fatal("recipient was nudged while blocked")
	}

	execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(b.id, a.id)})
		return err
	})
	if id := resend(); id != text.ID {
		t.Fatalf("resend after unblock = message %d, want %d", id, text.ID)
	}
	retryForward()
	if nextUserNotify(t, ctx, nudges, b.id, time.Second) {
		t.Fatal("recipient was nudged by a retry of a withheld send after unblock")
	}
	if got := captureDirectOwner(t, ctx, conn, b.id); got != recipientBefore {
		t.Fatalf("recipient after unblocked resend %+v, want unchanged %+v", got, recipientBefore)
	}
	execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{Peer: peerUser(a.id, b.id), Message: "after", RandomID: rid()})
		return err
	})
	if !nextUserNotify(t, ctx, nudges, b.id, 30*time.Second) {
		t.Fatal("recipient was not nudged for the post-unblock send")
	}
	if got := recvOrCtx(t, ctx, b.coll.newMsg, "post-unblock message"); got.Message != "after" {
		t.Fatalf("first message the recipient received = %q, want the post-unblock one", got.Message)
	}
	execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerUser(b.id, a.id), Limit: 50})
		if err != nil {
			return err
		}
		page, ok := res.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("history = %T", res)
		}
		if len(page.Messages) != 1 {
			return fmt.Errorf("recipient history = %d messages, want only the post-unblock one", len(page.Messages))
		}
		if m, ok := page.Messages[0].(*tg.Message); !ok || m.Message != "after" {
			return fmt.Errorf("recipient history = %+v, want the post-unblock message", page.Messages[0])
		}
		return nil
	})

	for _, mutual := range []bool{false, true} {
		if mutual {
			execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
				_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
				return err
			})
		} else {
			execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
				_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(a.id, b.id)})
				return err
			})
		}
		for _, p := range paths {
			senderBefore, recipientBefore := captureDirectOwner(t, ctx, conn, a.id), captureDirectOwner(t, ctx, conn, b.id)
			_, err := p.send(b.id)
			assertRPCError(t, err, "YOU_BLOCKED_USER")
			if got := captureDirectOwner(t, ctx, conn, a.id); got != senderBefore {
				t.Fatalf("%s (mutual=%v): sender %+v, want unchanged %+v", p.name, mutual, got, senderBefore)
			}
			if got := captureDirectOwner(t, ctx, conn, b.id); got != recipientBefore {
				t.Fatalf("%s (mutual=%v): recipient %+v, want unchanged %+v", p.name, mutual, got, recipientBefore)
			}
		}
	}
}
