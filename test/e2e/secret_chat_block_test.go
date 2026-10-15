package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

// listenEncryption subscribes to the secret-chat state nudge.
func listenEncryption(t *testing.T, ctx context.Context, mc *mediaClient) *pgx.Conn {
	t.Helper()
	conn := albumConn(t, ctx, mc)
	if _, err := conn.Exec(ctx, "LISTEN "+store.ChannelEncryption); err != nil {
		t.Fatalf("listen for encryption nudges: %v", err)
	}
	return conn
}

// nextEncryptionNotify reports whether a nudge naming userID and chatID
// arrives within d, skipping every other one.
func nextEncryptionNotify(t *testing.T, ctx context.Context, conn *pgx.Conn, userID int64, chatID int, d time.Duration) bool {
	t.Helper()
	_, ok := anyEncryptionNotify(t, ctx, conn, d, userID, chatID)
	return ok
}

// anyEncryptionNotify watches one window of d for a nudge naming userID and
// any of chatIDs, so a nudge for one chat cannot be skipped while waiting for
// another. It returns the chat the first match named.
func anyEncryptionNotify(t *testing.T, ctx context.Context, conn *pgx.Conn, d time.Duration, userID int64, chatIDs ...int) (int, bool) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	want := make(map[string]int, len(chatIDs))
	for _, id := range chatIDs {
		want[store.EncryptionPayload(userID, int64(id))] = id
	}
	for {
		n, err := conn.WaitForNotification(wait)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, false
		}
		if err != nil {
			t.Fatalf("wait for encryption nudge: %v", err)
		}
		if id, ok := want[n.Payload]; ok {
			return id, true
		}
	}
}

// differenceSecretChatIDs returns the chat ids getDifference shows mc in
// updateEncryption.
func differenceSecretChatIDs(t *testing.T, ctx context.Context, mc *mediaClient) map[int]bool {
	t.Helper()
	ids := map[int]bool{}
	execChat(t, ctx, mc.cmds, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 1, Date: 1})
		if err != nil {
			return err
		}
		var other []tg.UpdateClass
		switch d := diff.(type) {
		case *tg.UpdatesDifference:
			other = d.OtherUpdates
		case *tg.UpdatesDifferenceSlice:
			other = d.OtherUpdates
		case *tg.UpdatesDifferenceEmpty:
		default:
			return fmt.Errorf("difference = %T", diff)
		}
		for _, u := range other {
			if enc, ok := u.(*tg.UpdateEncryption); ok {
				ids[enc.Chat.GetID()] = true
			}
		}
		return nil
	})
	return ids
}

// storedSecretChatIDs returns every chat id either difference read gives
// userID.
func storedSecretChatIDs(t *testing.T, ctx context.Context, mc *mediaClient, userID int64) map[int]bool {
	t.Helper()
	after, err := mc.store.SecretChatsAfterPosition(ctx, userID, time.Unix(0, 0), nil)
	if err != nil {
		t.Fatalf("secret chats after position: %v", err)
	}
	from, err := mc.store.SecretChatsFromDate(ctx, userID, time.Unix(0, 0))
	if err != nil {
		t.Fatalf("secret chats from date: %v", err)
	}
	ids := map[int]bool{}
	for _, c := range append(after, from...) {
		ids[int(c.ID)] = true
	}
	return ids
}

// TestSecretChatRequestsFromBlockedUser: a request to a user who blocked the
// initiator answers exactly as a delivered one, never reaches the blocker by
// push, difference, accept or discard, stays hidden after the initiator
// discards it and after an unblock, while a later request is delivered; an
// active chat survives a block; and an initiator who blocked the peer gets
// YOU_BLOCKED_USER with nothing written.
func TestSecretChatRequestsFromBlockedUser(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	clients := bootMediaEnv(t, ctx, "+15551872901", "+15551872902", "+15551872903")
	a, b, c := clients[0], clients[1], clients[2]
	conn := albumConn(t, ctx, a)
	nudges := listenEncryption(t, ctx, a)

	request := func(peerID, randomID int64) (*tg.EncryptedChatWaiting, error) {
		var waiting *tg.EncryptedChatWaiting
		err := execMedia(t, ctx, a.cmds, func(ctx context.Context, api *tg.Client) error {
			chat, err := api.MessagesRequestEncryption(ctx, &tg.MessagesRequestEncryptionRequest{
				UserID: inputUser(a.id, peerID), RandomID: int(randomID), GA: smokeGA(),
			})
			if err != nil {
				return err
			}
			w, ok := chat.(*tg.EncryptedChatWaiting)
			if !ok {
				return fmt.Errorf("requestEncryption = %T, want *tg.EncryptedChatWaiting", chat)
			}
			waiting = w
			return nil
		})
		return waiting, err
	}
	discard := func(mc *mediaClient, chatID int) error {
		return execMedia(t, ctx, mc.cmds, func(ctx context.Context, api *tg.Client) error {
			var box tg.EncryptedChatBox
			return api.Invoker().Invoke(ctx, &tg.MessagesDiscardEncryptionRequest{ChatID: chatID}, &box)
		})
	}
	accept := func(chatID int) error {
		return execMedia(t, ctx, b.cmds, func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesAcceptEncryption(ctx, &tg.MessagesAcceptEncryptionRequest{
				Peer: tg.InputEncryptedChat{ChatID: chatID, AccessHash: secretChatAccessHash(b.id, chatID)},
				GB:   smokeGB(), KeyFingerprint: 1872,
			})
			return err
		})
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
	shape := func(w *tg.EncryptedChatWaiting) string {
		return fmt.Sprintf("admin=%d participant=%d hash=%v date=%v", w.AdminID, w.ParticipantID, w.AccessHash != 0, w.Date > 0)
	}

	baseline, err := request(c.id, 1872001)
	if err != nil {
		t.Fatalf("request to an unblocked peer: %v", err)
	}
	if !nextEncryptionNotify(t, ctx, nudges, c.id, baseline.ID, 30*time.Second) {
		t.Fatal("unblocked peer was not nudged for the request")
	}

	block(b, a, true)
	const withheldRandomID = 1872002
	withheld, err := request(b.id, withheldRandomID)
	if err != nil {
		t.Fatalf("request while blocked = %v, want the delivered answer", err)
	}
	// The unblocked answer, with only the participant swapped.
	wantShape := shape(&tg.EncryptedChatWaiting{AdminID: baseline.AdminID, ParticipantID: b.id, AccessHash: baseline.AccessHash, Date: baseline.Date})
	if got := shape(withheld); got != wantShape {
		t.Fatalf("request while blocked answered %s, want %s", got, wantShape)
	}
	discarded, err := request(b.id, 1872003)
	if err != nil {
		t.Fatalf("second request while blocked: %v", err)
	}
	assertRPCError(t, accept(withheld.ID), "ENCRYPTION_ID_INVALID")
	assertRPCError(t, discard(b, withheld.ID), "ENCRYPTION_ID_INVALID")
	if err := discard(a, discarded.ID); err != nil {
		t.Fatalf("initiator discard of a withheld request: %v", err)
	}
	if id, ok := anyEncryptionNotify(t, ctx, nudges, 2*time.Second, b.id, withheld.ID, discarded.ID); ok {
		t.Fatalf("blocker was nudged for withheld chat %d (requested %d, discarded %d)", id, withheld.ID, discarded.ID)
	}
	for _, id := range []int{withheld.ID, discarded.ID} {
		if storedSecretChatIDs(t, ctx, b, b.id)[id] {
			t.Fatalf("blocker's difference reads include withheld chat %d", id)
		}
		if differenceSecretChatIDs(t, ctx, b)[id] {
			t.Fatalf("blocker's getDifference shows withheld chat %d", id)
		}
		if !storedSecretChatIDs(t, ctx, a, a.id)[id] {
			t.Fatalf("initiator's difference reads miss its own chat %d", id)
		}
	}
	if state, err := a.store.SecretChatByID(ctx, int32(withheld.ID)); err != nil || state.State != store.SecretChatRequested { //nolint:gosec // chat id is int32 on the wire
		t.Fatalf("withheld request after blocker's accept and discard = %+v, %v; want still requested", state, err)
	}

	block(b, a, false)
	resent, err := request(b.id, withheldRandomID)
	if err != nil {
		t.Fatalf("resend after unblock: %v", err)
	}
	if resent.ID != withheld.ID || resent.Date != withheld.Date {
		t.Fatalf("resend after unblock = %+v, want the original answer %+v", resent, withheld)
	}
	if nextEncryptionNotify(t, ctx, nudges, b.id, withheld.ID, time.Second) {
		t.Fatal("blocker was nudged by a resend of a withheld request after unblock")
	}
	if differenceSecretChatIDs(t, ctx, b)[withheld.ID] {
		t.Fatal("blocker's getDifference shows the withheld request after unblock")
	}
	assertRPCError(t, accept(withheld.ID), "ENCRYPTION_ID_INVALID")

	delivered, err := request(b.id, 1872004)
	if err != nil {
		t.Fatalf("request after unblock: %v", err)
	}
	if !nextEncryptionNotify(t, ctx, nudges, b.id, delivered.ID, 30*time.Second) {
		t.Fatal("peer was not nudged for the request after unblock")
	}
	if !differenceSecretChatIDs(t, ctx, b)[delivered.ID] {
		t.Fatal("peer's getDifference misses the request after unblock")
	}
	if err := accept(delivered.ID); err != nil {
		t.Fatalf("accept after unblock: %v", err)
	}
	if !nextEncryptionNotify(t, ctx, nudges, a.id, delivered.ID, 30*time.Second) {
		t.Fatal("initiator was not nudged for the accept")
	}

	block(b, a, true)
	if active, err := a.store.SecretChatByID(ctx, int32(delivered.ID)); err != nil || active.State != store.SecretChatActive { //nolint:gosec // chat id is int32 on the wire
		t.Fatalf("active chat after block = %+v, %v; want still active", active, err)
	}
	if nextEncryptionNotify(t, ctx, nudges, a.id, delivered.ID, time.Second) {
		t.Fatal("initiator was nudged about its active chat when the peer blocked it")
	}
	block(b, a, false)

	for _, mutual := range []bool{false, true} {
		if mutual {
			block(b, a, true)
		} else {
			block(a, b, true)
		}
		var rows, seq int64
		snapshot := func() (int64, int64) {
			var r, s int64
			if err := conn.QueryRow(ctx, `SELECT (SELECT count(*) FROM secret_chats WHERE admin_id = $1), (SELECT last_value FROM secret_chats_id_seq)`, a.id).Scan(&r, &s); err != nil {
				t.Fatalf("snapshot secret chats: %v", err)
			}
			return r, s
		}
		rows, seq = snapshot()
		_, err := request(b.id, 1872010)
		assertRPCError(t, err, "YOU_BLOCKED_USER")
		if r, s := snapshot(); r != rows || s != seq {
			t.Fatalf("refused request (mutual=%v) changed rows %d->%d, sequence %d->%d", mutual, rows, r, seq, s)
		}
	}
}
