package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

func mustUser(t *testing.T, s *store.Store, phone string) store.User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), phone)
	if err != nil {
		t.Fatalf("create user %s: %v", phone, err)
	}
	return u
}

func send(t *testing.T, s *store.Store, from, to store.User, text string, rid int64) store.Message {
	t.Helper()
	m, _, _, _, err := s.SendMessage(context.Background(), from.ID, to.ID, text, rid, 0, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send %q: %v", text, err)
	}
	return m
}

func msgAt(t *testing.T, s *store.Store, ownerID, localID int64) store.Message {
	t.Helper()
	m, ok, err := s.MessageByOwnerLocal(context.Background(), ownerID, localID)
	if err != nil || !ok {
		t.Fatalf("message (%d,%d): ok=%v err=%v", ownerID, localID, ok, err)
	}
	return m
}

func TestSendMessageTwoSided(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240001")
	b := mustUser(t, s, "+15551240002")

	sender, senderPts, recipientPts, dup, err := s.SendMessage(ctx, a.ID, b.ID, "hi", 111, 0, 0)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if dup {
		t.Fatal("first send flagged dup")
	}
	if senderPts != 1 || recipientPts != 1 {
		t.Fatalf("pts = sender %d recipient %d, want 1,1", senderPts, recipientPts)
	}
	if !sender.Out || sender.OwnerID != a.ID || sender.PeerID != b.ID || sender.FromID != a.ID {
		t.Fatalf("sender row wrong: %+v", sender)
	}
	if sender.LocalID != 1 {
		t.Fatalf("sender local_id = %d, want 1", sender.LocalID)
	}
	if sender.Text != "hi" {
		t.Fatalf("sender text = %q", sender.Text)
	}

	// Recipient's inbox copy has its own local_id space, also starting at 1.
	recv, ok, err := s.MessageByOwnerLocal(ctx, b.ID, 1)
	if err != nil || !ok {
		t.Fatalf("recipient row: ok=%v err=%v", ok, err)
	}
	if recv.Out || recv.OwnerID != b.ID || recv.PeerID != a.ID || recv.FromID != a.ID {
		t.Fatalf("recipient row wrong: %+v", recv)
	}
	if recv.Text != "hi" {
		t.Fatalf("recipient text = %q", recv.Text)
	}

	// One new-message event per owner.
	for _, u := range []store.User{a, b} {
		ev, err := s.EventsSince(ctx, u.ID, 0)
		if err != nil {
			t.Fatalf("events %d: %v", u.ID, err)
		}
		if len(ev) != 1 || ev[0].Type != store.EventNewMessage {
			t.Fatalf("owner %d events = %+v, want one new-message", u.ID, ev)
		}
	}
}

// Both sides of a 1:1 pair carry the same file id — that is what makes the
// download gate pass for the recipient.
func TestSendMessageCarriesFileIDOnBothSides(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240021")
	b := mustUser(t, s, "+15551240022")

	f, err := s.AllocateFile(ctx, a.ID, 11, "text/plain", "hello.txt", bigQuota)
	if err != nil {
		t.Fatalf("allocate file: %v", err)
	}
	sender, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "here", 5, f.ID, 0) //nolint:dogsled // only the stored message is needed here
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if sender.FileID != f.ID {
		t.Fatalf("sender row file_id = %d, want %d", sender.FileID, f.ID)
	}
	if own := msgAt(t, s, a.ID, sender.LocalID); own.FileID != f.ID {
		t.Fatalf("stored sender row file_id = %d, want %d", own.FileID, f.ID)
	}
	if recv := msgAt(t, s, b.ID, sender.PeerLocalID); recv.FileID != f.ID {
		t.Fatalf("recipient row file_id = %d, want %d", recv.FileID, f.ID)
	}
}

func TestSendMessageRandomIDDedup(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240011")
	b := mustUser(t, s, "+15551240012")

	first, _, _, dup, err := s.SendMessage(ctx, a.ID, b.ID, "once", 999, 0, 0)
	if err != nil || dup {
		t.Fatalf("first send: dup=%v err=%v", dup, err)
	}
	again, sPts, rPts, dup, err := s.SendMessage(ctx, a.ID, b.ID, "once", 999, 0, 0)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if !dup {
		t.Fatal("resend with same random_id not flagged dup")
	}
	if again.LocalID != first.LocalID {
		t.Fatalf("dup returned local_id %d, want original %d", again.LocalID, first.LocalID)
	}
	// pts must not advance on a dedup'd resend.
	if sPts != 1 || rPts != 1 {
		t.Fatalf("dup pts = sender %d recipient %d, want unchanged 1,1", sPts, rPts)
	}
	st, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st.Pts != 1 {
		t.Fatalf("sender pts after dup = %d, want 1", st.Pts)
	}
}

func TestSendMessageRejectsReplyOutsideDestinationDialogBeforeWrites(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240101")
	b := mustUser(t, s, "+15551240102")
	c := mustUser(t, s, "+15551240103")

	wrongDialogTarget, _, _, _, err := s.SendMessage(ctx, a.ID, c.ID, "other dialog", 2101, 0, 0) //nolint:dogsled // only the target row and error are needed
	if err != nil {
		t.Fatalf("send other-dialog target: %v", err)
	}

	before, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state before invalid reply: %v", err)
	}
	beforeB, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("recipient state before invalid reply: %v", err)
	}
	_, _, _, _, err = s.SendMessage(ctx, a.ID, b.ID, "invalid reply", 2102, 0, wrongDialogTarget.LocalID) //nolint:dogsled // only the error is needed
	if !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("cross-dialog reply error = %v, want ErrMessageInvalid", err)
	}

	after, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state after invalid reply: %v", err)
	}
	if after.Pts != before.Pts {
		t.Errorf("sender pts = %d after invalid reply, want unchanged %d", after.Pts, before.Pts)
	}
	afterB, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("recipient state after invalid reply: %v", err)
	}
	if afterB.Pts != beforeB.Pts {
		t.Errorf("recipient pts = %d after invalid reply, want unchanged %d", afterB.Pts, beforeB.Pts)
	}
	events, err := s.EventsSince(ctx, a.ID, 0)
	if err != nil {
		t.Fatalf("sender events after invalid reply: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("sender events after invalid reply = %d, want only the other-dialog send", len(events))
	}
	events, err = s.EventsSince(ctx, b.ID, 0)
	if err != nil {
		t.Fatalf("recipient events after invalid reply: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("recipient events after invalid reply = %d, want none", len(events))
	}
	history, err := s.History(ctx, a.ID, store.PeerTypeUser, b.ID, 0, 10)
	if err != nil {
		t.Fatalf("destination history: %v", err)
	}
	if len(history) != 0 {
		t.Errorf("destination history after invalid reply = %+v, want empty", history)
	}
}

func TestSendMessageDoesNotReplyToDeletedRecipientTargetCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240111")
	b := mustUser(t, s, "+15551240112")

	target := send(t, s, a, b, "target", 2111)
	if _, err := s.DeleteMessages(ctx, b.ID, []int64{target.PeerLocalID}, false); err != nil {
		t.Fatalf("delete recipient target copy: %v", err)
	}

	reply, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "reply", 2112, 0, target.LocalID) //nolint:dogsled // only the sender row and error are needed
	if err != nil {
		t.Fatalf("send reply: %v", err)
	}
	recipient := msgAt(t, s, b.ID, reply.PeerLocalID)
	if recipient.ReplyToMsgID != 0 {
		t.Fatalf("recipient reply_to_msg_id = %d, want no reference to deleted target copy", recipient.ReplyToMsgID)
	}
	if !reply.ReplyToTrusted || recipient.ReplyToTrusted {
		t.Fatalf("reply trust = sender %v recipient %v, want true/false", reply.ReplyToTrusted, recipient.ReplyToTrusted)
	}
}

func TestSendMessageUsesEachOwnersActiveReplyTarget(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240121")
	b := mustUser(t, s, "+15551240122")
	c := mustUser(t, s, "+15551240123")

	if _, _, _, _, err := s.SendMessage(ctx, c.ID, b.ID, "earlier inbox row", 2121, 0, 0); err != nil {
		t.Fatalf("seed B local id space: %v", err)
	}
	target := send(t, s, a, b, "target", 2122)
	reply, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "reply", 2123, 0, target.LocalID) //nolint:dogsled // only the sender row and error are needed
	if err != nil {
		t.Fatalf("send reply: %v", err)
	}
	recipient := msgAt(t, s, b.ID, reply.PeerLocalID)
	if target.LocalID == target.PeerLocalID {
		t.Fatalf("test setup did not create different target ids: sender=%d recipient=%d", target.LocalID, target.PeerLocalID)
	}
	if int64(reply.ReplyToMsgID) != target.LocalID || int64(recipient.ReplyToMsgID) != target.PeerLocalID {
		t.Errorf("reply target ids = sender %d recipient %d, want sender %d recipient %d", reply.ReplyToMsgID, recipient.ReplyToMsgID, target.LocalID, target.PeerLocalID)
	}
	if !reply.ReplyToTrusted || !recipient.ReplyToTrusted {
		t.Errorf("validated reply trust = sender %v recipient %v, want true/true", reply.ReplyToTrusted, recipient.ReplyToTrusted)
	}
}

func TestSendSelfMessageCanReplyWithinSavedDialog(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	user := mustUser(t, s, "+15551240131")
	target, _, _, _, err := s.SendMessage(ctx, user.ID, user.ID, "saved", 2131, 0, 0) //nolint:dogsled // only the sender row and error are needed
	if err != nil {
		t.Fatalf("send saved message: %v", err)
	}
	reply, _, _, _, err := s.SendMessage(ctx, user.ID, user.ID, "saved reply", 2132, 0, target.LocalID) //nolint:dogsled // only the sender row and error are needed
	if err != nil {
		t.Fatalf("reply in saved dialog: %v", err)
	}
	if int64(reply.ReplyToMsgID) != target.LocalID || !reply.ReplyToTrusted {
		t.Errorf("saved reply target/trust = %d/%v, want %d/true", reply.ReplyToMsgID, reply.ReplyToTrusted, target.LocalID)
	}
}

func TestSendMessageRetryKeepsValidatedReplyAfterTargetDeletion(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551240141")
	b := mustUser(t, s, "+15551240142")
	target := send(t, s, a, b, "target", 2141)
	first, _, _, dup, err := s.SendMessage(ctx, a.ID, b.ID, "reply", 2142, 0, target.LocalID)
	if err != nil || dup {
		t.Fatalf("first reply: dup=%v err=%v", dup, err)
	}
	if _, err = s.DeleteMessages(ctx, a.ID, []int64{target.LocalID}, false); err != nil {
		t.Fatalf("delete reply target: %v", err)
	}
	before, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state before retry: %v", err)
	}
	again, _, _, dup, err := s.SendMessage(ctx, a.ID, b.ID, "reply", 2142, 0, target.LocalID)
	if err != nil || !dup {
		t.Fatalf("retry after target deletion: dup=%v err=%v", dup, err)
	}
	after, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state after retry: %v", err)
	}
	if again.LocalID != first.LocalID || again.ReplyToMsgID != first.ReplyToMsgID || !again.ReplyToTrusted {
		t.Errorf("retry returned %+v, want original trusted reply %+v", again, first)
	}
	if after.Pts != before.Pts {
		t.Errorf("sender pts after retry = %d, want unchanged %d", after.Pts, before.Pts)
	}
}

// TestConcurrentOppositeFirstSends fires A->B and B->A simultaneously across
// many fresh user pairs: the sorted advisory-lock ordering (plus state rows
// provisioned at user creation) must keep both first sends deadlock-free.
func TestConcurrentOppositeFirstSends(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()

	sendErr := func(from, to store.User, rid int64) error {
		_, _, _, _, e := s.SendMessage(ctx, from.ID, to.ID, "x", rid, 0, 0)
		return e
	}

	for i := range 15 {
		a := mustUser(t, s, "+1555127"+string(rune('a'+i))+"01")
		b := mustUser(t, s, "+1555127"+string(rune('a'+i))+"02")
		var wg sync.WaitGroup
		errs := make([]error, 2)
		wg.Go(func() { errs[0] = sendErr(a, b, int64(2*i+1)) })
		wg.Go(func() { errs[1] = sendErr(b, a, int64(2*i+2)) })
		wg.Wait()
		for j, e := range errs {
			if e != nil {
				t.Fatalf("round %d send %d: %v", i, j, e)
			}
		}
	}
}

func TestHistoryPaging(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551250001")
	b := mustUser(t, s, "+15551250002")

	for i, txt := range []string{"m1", "m2", "m3"} {
		send(t, s, a, b, txt, int64(1000+i))
	}

	all, err := s.History(ctx, a.ID, store.PeerTypeUser, b.ID, 0, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("history len = %d, want 3", len(all))
	}
	if all[0].LocalID != 3 || all[2].LocalID != 1 {
		t.Fatalf("history not newest-first: %d..%d", all[0].LocalID, all[2].LocalID)
	}

	older, err := s.History(ctx, a.ID, store.PeerTypeUser, b.ID, 2, 10) // strictly older than local_id 2
	if err != nil {
		t.Fatalf("history page: %v", err)
	}
	if len(older) != 1 || older[0].LocalID != 1 {
		t.Fatalf("paged history = %+v, want only local_id 1", older)
	}
}

func TestEditMessageBothSides(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551250011")
	b := mustUser(t, s, "+15551250012")

	sender := send(t, s, a, b, "orig", 1)

	peerID, newPts, err := s.EditMessage(ctx, a.ID, sender.LocalID, "edited")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if peerID != b.ID {
		t.Fatalf("edit peer = %d, want %d", peerID, b.ID)
	}
	if newPts != 2 {
		t.Fatalf("editor pts = %d, want 2", newPts)
	}

	own := msgAt(t, s, a.ID, sender.LocalID)
	if own.Text != "edited" || own.EditDate == nil {
		t.Fatalf("owner row not edited: %+v", own)
	}
	mirror := msgAt(t, s, b.ID, sender.PeerLocalID)
	if mirror.Text != "edited" || mirror.EditDate == nil {
		t.Fatalf("mirror row not edited: %+v", mirror)
	}

	// Edit events on both owners.
	for _, u := range []int64{a.ID, b.ID} {
		ev, err := s.EventsSince(ctx, u, 1)
		if err != nil {
			t.Fatalf("events owner %d: %v", u, err)
		}
		if len(ev) != 1 || ev[0].Type != store.EventEdit {
			t.Fatalf("owner %d edit event = %+v", u, ev)
		}
	}

	// Non-owned / inbound edits fail closed.
	if _, _, err := s.EditMessage(ctx, a.ID, 999, "x"); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("edit absent: want ErrMessageInvalid, got %v", err)
	}
	if _, _, err := s.EditMessage(ctx, b.ID, sender.PeerLocalID, "x"); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("edit inbound: want ErrMessageInvalid, got %v", err)
	}
}

func TestDeleteMessagesBothSides(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551250021")
	b := mustUser(t, s, "+15551250022")

	sender := send(t, s, a, b, "bye", 1)

	perOwner, err := s.DeleteMessages(ctx, a.ID, []int64{sender.LocalID}, true)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if perOwner[a.ID] != 2 || perOwner[b.ID] != 2 {
		t.Fatalf("delete pts = %+v, want a,b at 2", perOwner)
	}

	own := msgAt(t, s, a.ID, sender.LocalID)
	if !own.Deleted {
		t.Fatal("owner row not deleted")
	}
	mirror := msgAt(t, s, b.ID, sender.PeerLocalID)
	if !mirror.Deleted {
		t.Fatal("mirror row not deleted")
	}
	for _, u := range []int64{a.ID, b.ID} {
		ev, err := s.EventsSince(ctx, u, 1)
		if err != nil {
			t.Fatalf("events owner %d: %v", u, err)
		}
		if len(ev) != 1 || ev[0].Type != store.EventDelete {
			t.Fatalf("owner %d delete event = %+v", u, ev)
		}
	}

	if _, err := s.DeleteMessages(ctx, a.ID, []int64{999}, true); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("delete absent: want ErrMessageInvalid, got %v", err)
	}
}

// TestDeleteMessagesOneSided pins the revoke=false contract: the caller's row
// is soft-deleted and the caller's pts bumped, and nothing touches the peer's
// row, pts or event log.
func TestDeleteMessagesOneSided(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a := mustUser(t, s, "+15551250031")
	b := mustUser(t, s, "+15551250032")

	sender := send(t, s, a, b, "bye", 1)

	// a's own copy deleted without revoking b: b keeps its mirror and stays at
	// pts 1 with only the send event.
	perOwner, err := s.DeleteMessages(ctx, a.ID, []int64{sender.LocalID}, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(perOwner) != 1 || perOwner[a.ID] != 2 {
		t.Fatalf("delete pts = %+v, want only a at 2", perOwner)
	}

	own := msgAt(t, s, a.ID, sender.LocalID)
	if !own.Deleted {
		t.Fatal("owner row not deleted")
	}
	mirror := msgAt(t, s, b.ID, sender.PeerLocalID)
	if mirror.Deleted {
		t.Fatal("mirror row deleted despite revoke=false")
	}

	stA, err := s.State(ctx, a.ID)
	if err != nil {
		t.Fatalf("state a: %v", err)
	}
	if stA.Pts != 2 {
		t.Fatalf("a pts = %d, want 2", stA.Pts)
	}
	stB, err := s.State(ctx, b.ID)
	if err != nil {
		t.Fatalf("state b: %v", err)
	}
	if stB.Pts != 1 {
		t.Fatalf("b pts = %d, want unchanged 1", stB.Pts)
	}
	eva, err := s.EventsSince(ctx, a.ID, 1)
	if err != nil {
		t.Fatalf("events a: %v", err)
	}
	if len(eva) != 1 || eva[0].Type != store.EventDelete {
		t.Fatalf("a delete event = %+v", eva)
	}
	evb, err := s.EventsSince(ctx, b.ID, 0)
	if err != nil {
		t.Fatalf("events b: %v", err)
	}
	if len(evb) != 1 || evb[0].Type != store.EventNewMessage {
		t.Fatalf("b events = %+v, want only the original send", evb)
	}

	// The reverse direction: b deletes her inbound copy one-sidedly and a keeps
	// the message.
	sender2 := send(t, s, a, b, "again", 2)
	if _, err := s.DeleteMessages(ctx, b.ID, []int64{sender2.PeerLocalID}, false); err != nil {
		t.Fatalf("one-sided inbound delete: %v", err)
	}
	if _, err := s.DeleteMessages(ctx, a.ID, []int64{999}, false); !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("delete absent: want ErrMessageInvalid, got %v", err)
	}
	aRow := msgAt(t, s, a.ID, sender2.LocalID)
	if aRow.Deleted {
		t.Fatal("sender row deleted by recipient's one-sided delete")
	}
	bRow := msgAt(t, s, b.ID, sender2.PeerLocalID)
	if !bRow.Deleted {
		t.Fatal("deleter's own row not deleted")
	}
}
