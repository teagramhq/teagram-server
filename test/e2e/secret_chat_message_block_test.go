package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

// nextEncryptedMsgNotify reports whether any encrypted-message nudge for
// recipientID arrives within d, skipping every other one.
func nextEncryptedMsgNotify(t *testing.T, ctx context.Context, conn *pgx.Conn, recipientID int64, d time.Duration) bool {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	prefix := strconv.FormatInt(recipientID, 10) + "|"
	for {
		n, err := conn.WaitForNotification(wait)
		if errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		if err != nil {
			t.Fatalf("wait for encrypted message nudge: %v", err)
		}
		if strings.HasPrefix(n.Payload, prefix) {
			return true
		}
	}
}

// differenceEncryptedRandomIDs returns the random ids of the encrypted
// messages getDifference from qts 0 gives mc.
func differenceEncryptedRandomIDs(t *testing.T, ctx context.Context, mc *mediaClient) map[int64]bool {
	t.Helper()
	ids := map[int64]bool{}
	execChat(t, ctx, mc.cmds, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 1, Date: 1, Qts: 0})
		if err != nil {
			return err
		}
		var msgs []tg.EncryptedMessageClass
		switch d := diff.(type) {
		case *tg.UpdatesDifference:
			msgs = d.NewEncryptedMessages
		case *tg.UpdatesDifferenceSlice:
			msgs = d.NewEncryptedMessages
		case *tg.UpdatesDifferenceEmpty:
		default:
			return fmt.Errorf("difference = %T", diff)
		}
		for _, m := range msgs {
			ids[m.GetRandomID()] = true
		}
		return nil
	})
	return ids
}

// TestSecretChatMessagesFromBlockedUser: on an active chat that survives a
// block, a message to a user who blocked the sender answers exactly as a
// delivered one and reaches the blocker by no push, qts or difference entry; a
// resend of it after the unblock returns the original date and still delivers
// nothing, while a new message is delivered; a sender who blocked the peer gets
// YOU_BLOCKED_USER with nothing written; either party can still discard during
// a block and the other is told; and a withheld request is an unknown id to its
// participant on send.
func TestSecretChatMessagesFromBlockedUser(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551873901", "+15551873902")
	a, b := clients[0], clients[1]
	conn := albumConn(t, ctx, a)
	stateNudges := listenEncryption(t, ctx, a)
	msgNudges := albumConn(t, ctx, a)
	if _, err := msgNudges.Exec(ctx, "LISTEN "+store.ChannelEncryptedMsg); err != nil {
		t.Fatalf("listen for encrypted message nudges: %v", err)
	}

	block := func(blocker, blocked *mediaClient, on bool) {
		execChat(t, ctx, blocker.cmds, func(ctx context.Context, api *tg.Client) error {
			if on {
				_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(blocker.id, blocked.id)})
				return err
			}
			_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(blocker.id, blocked.id)})
			return err
		})
	}
	request := func(randomID int64) int {
		var id int
		execChat(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
			chat, err := api.MessagesRequestEncryption(ctx, &tg.MessagesRequestEncryptionRequest{
				UserID: inputUser(a.id, b.id), RandomID: int(randomID), GA: smokeGA(),
			})
			if err != nil {
				return err
			}
			id = chat.GetID()
			return nil
		})
		return id
	}
	activeChat := func(randomID int64) int {
		id := request(randomID)
		execChat(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesAcceptEncryption(ctx, &tg.MessagesAcceptEncryptionRequest{
				Peer: tg.InputEncryptedChat{ChatID: id, AccessHash: secretChatAccessHash(b.id, id)},
				GB:   smokeGB(), KeyFingerprint: 1873,
			})
			return err
		})
		// Drain both nudges, so a later wait on this chat sees only what
		// follows.
		if !nextEncryptionNotify(t, ctx, stateNudges, b.id, id, 30*time.Second) {
			t.Fatal("responder was not nudged for the request")
		}
		if !nextEncryptionNotify(t, ctx, stateNudges, a.id, id, 30*time.Second) {
			t.Fatal("initiator was not nudged for the accept")
		}
		return id
	}
	// activeChatWhileBlocked opens an active chat with B's block lifted, then
	// restores it.
	activeChatWhileBlocked := func(randomID int64) int {
		block(b, a, false)
		id := activeChat(randomID)
		block(b, a, true)
		return id
	}
	sendFrom := func(mc *mediaClient, chatID int, randomID int64) (int, error) {
		var date int
		err := execMedia(t, ctx, mc.cmds, func(ctx context.Context, api *tg.Client) error {
			sent, err := api.MessagesSendEncrypted(ctx, &tg.MessagesSendEncryptedRequest{
				Peer:     tg.InputEncryptedChat{ChatID: chatID, AccessHash: secretChatAccessHash(mc.id, chatID)},
				RandomID: randomID,
				Data:     []byte("ciphertext"),
			})
			if err != nil {
				return err
			}
			m, ok := sent.(*tg.MessagesSentEncryptedMessage)
			if !ok {
				return fmt.Errorf("sendEncrypted = %T, want *tg.MessagesSentEncryptedMessage", sent)
			}
			date = m.Date
			return nil
		})
		return date, err
	}
	qts := func() int {
		st, err := b.store.State(ctx, b.id)
		if err != nil {
			t.Fatalf("recipient state: %v", err)
		}
		return st.Qts
	}
	discard := func(mc *mediaClient, chatID int) {
		execChat(t, ctx, mc.cmds, func(ctx context.Context, api *tg.Client) error {
			var box tg.EncryptedChatBox
			return api.Invoker().Invoke(ctx, &tg.MessagesDiscardEncryptionRequest{ChatID: chatID}, &box)
		})
	}

	chatID := activeChat(1873001)
	if _, err := sendFrom(a, chatID, 1873101); err != nil {
		t.Fatalf("unblocked send: %v", err)
	}
	if !nextEncryptedMsgNotify(t, ctx, msgNudges, b.id, 30*time.Second) {
		t.Fatal("recipient was not nudged for an unblocked send")
	}
	if !differenceEncryptedRandomIDs(t, ctx, b)[1873101] {
		t.Fatal("recipient's getDifference misses the unblocked send")
	}
	baseQts := qts()

	// B blocked A: the chat stays active and A's send answers as usual.
	block(b, a, true)
	if chat, err := a.store.SecretChatByID(ctx, int32(chatID)); err != nil || chat.State != store.SecretChatActive { //nolint:gosec // chat id is int32 on the wire
		t.Fatalf("active chat after block = %+v, %v; want still active", chat, err)
	}
	const withheldRandomID = 1873102
	before := int(time.Now().Unix())
	date, err := sendFrom(a, chatID, withheldRandomID)
	if err != nil {
		t.Fatalf("send while blocked = %v, want the delivered answer", err)
	}
	if date < before-1 || date > int(time.Now().Unix())+1 {
		t.Fatalf("send while blocked answered date %d, want the current date (~%d)", date, before)
	}
	if nextEncryptedMsgNotify(t, ctx, msgNudges, b.id, 2*time.Second) {
		t.Fatal("blocker was nudged for a withheld message")
	}
	if got := qts(); got != baseQts {
		t.Fatalf("blocker's qts moved %d -> %d on a withheld message", baseQts, got)
	}
	if differenceEncryptedRandomIDs(t, ctx, b)[withheldRandomID] {
		t.Fatal("blocker's getDifference shows a withheld message")
	}

	// A withheld request is an unknown id to the blocker on send.
	withheldChat := request(1873002)
	_, err = sendFrom(b, withheldChat, 1873201)
	assertRPCError(t, err, "ENCRYPTION_ID_INVALID")
	_, err = sendFrom(b, withheldChat+1000000, 1873202)
	assertRPCError(t, err, "ENCRYPTION_ID_INVALID")

	// Either party may still discard during the block, and the other is told.
	second := activeChatWhileBlocked(1873003)
	discard(b, second)
	if !nextEncryptionNotify(t, ctx, stateNudges, a.id, second, 30*time.Second) {
		t.Fatal("blocked user was not told the blocker discarded the chat")
	}
	third := activeChatWhileBlocked(1873004)
	discard(a, third)
	if !nextEncryptionNotify(t, ctx, stateNudges, b.id, third, 30*time.Second) {
		t.Fatal("blocker was not told the blocked user discarded the chat")
	}

	// After the unblock a resend returns the original answer and still
	// delivers nothing; a new message is delivered.
	block(b, a, false)
	resent, err := sendFrom(a, chatID, withheldRandomID)
	if err != nil {
		t.Fatalf("resend after unblock: %v", err)
	}
	if resent != date {
		t.Fatalf("resend after unblock answered date %d, want the original %d", resent, date)
	}
	if nextEncryptedMsgNotify(t, ctx, msgNudges, b.id, time.Second) {
		t.Fatal("blocker was nudged by a resend of a withheld message after unblock")
	}
	if got := qts(); got != baseQts {
		t.Fatalf("resend after unblock moved qts %d -> %d", baseQts, got)
	}
	if differenceEncryptedRandomIDs(t, ctx, b)[withheldRandomID] {
		t.Fatal("blocker's getDifference shows a withheld message after unblock")
	}
	if _, err := sendFrom(a, chatID, 1873103); err != nil {
		t.Fatalf("send after unblock: %v", err)
	}
	if !nextEncryptedMsgNotify(t, ctx, msgNudges, b.id, 30*time.Second) {
		t.Fatal("recipient was not nudged for a send after unblock")
	}
	if !differenceEncryptedRandomIDs(t, ctx, b)[1873103] {
		t.Fatal("recipient's getDifference misses the send after unblock")
	}

	// A blocked B, alone and mutually: refused, nothing written.
	for _, mutual := range []bool{false, true} {
		if mutual {
			block(b, a, true)
		} else {
			block(a, b, true)
		}
		snapshot := func() (int64, int64) {
			var events, withheld int64
			if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM encrypted_events WHERE owner_id = $1), (SELECT count(*) FROM withheld_encrypted_messages WHERE recipient_id = $1)`, b.id).Scan(&events, &withheld); err != nil {
				t.Fatalf("snapshot encrypted rows: %v", err)
			}
			return events, withheld
		}
		events, withheld := snapshot()
		qtsBefore := qts()
		_, err := sendFrom(a, chatID, 1873110)
		assertRPCError(t, err, "YOU_BLOCKED_USER")
		if e, w := snapshot(); e != events || w != withheld || qts() != qtsBefore {
			t.Fatalf("refused send (mutual=%v) changed events %d->%d, withheld %d->%d", mutual, events, e, withheld, w)
		}
	}
}
