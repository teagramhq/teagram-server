package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/teagramhq/teagram-server/internal/store"
)

// ownerMessageRows counts every message row ownerID holds, dialog or not, so a
// withheld send that left a row without a dialog still shows up.
func ownerMessageRows(t *testing.T, s *store.Store, ownerID int64) int {
	t.Helper()
	var n int
	if err := store.StorePool(s).QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE owner_id = $1`, ownerID).Scan(&n); err != nil {
		t.Fatalf("count owner %d rows: %v", ownerID, err)
	}
	return n
}

func mustBlock(t *testing.T, s *store.Store, blockerID, blockedID int64) {
	t.Helper()
	if _, err := s.BlockUser(context.Background(), blockerID, blockedID); err != nil {
		t.Fatalf("block %d -> %d: %v", blockerID, blockedID, err)
	}
}

func assertWithheld(t *testing.T, s *store.Store, m store.Message) {
	t.Helper()
	if !m.Out || m.PeerLocalID != 0 {
		t.Fatalf("sender copy = %+v, want an outgoing row recording no recipient copy", m)
	}
	if stored := msgAt(t, s, m.OwnerID, m.LocalID); stored.PeerLocalID != 0 {
		t.Fatalf("stored sender copy peer_local_id = %d, want 0", stored.PeerLocalID)
	}
}

func TestDirectSendsWithheldFromRecipientWhoBlockedSender(t *testing.T) {
	t.Parallel()
	f := newAlbumFixture(t)
	s := f.s
	ctx := context.Background()
	a, b := mustUser(t, s, "+15551862001"), mustUser(t, s, "+15551862002")
	src := send(t, s, a, a, "saved", 0)
	mustBlock(t, s, b.ID, a.ID)

	for _, tc := range []struct {
		name string
		send func(t *testing.T) (sent []store.Message, perOwner map[int64]int)
	}{
		{"message", func(t *testing.T) ([]store.Message, map[int64]int) {
			t.Helper()
			m, senderPts, recipientPts, dup, err := s.SendMessage(ctx, a.ID, b.ID, "hi", 1862101, 0, 0)
			if err != nil || dup || senderPts <= 0 || recipientPts != 0 {
				t.Fatalf("send: senderPts=%d recipientPts=%d dup=%v err=%v", senderPts, recipientPts, dup, err)
			}
			return []store.Message{m}, map[int64]int{a.ID: senderPts}
		}},
		{"poll", func(t *testing.T) ([]store.Message, map[int64]int) {
			t.Helper()
			m, perOwner, _, dup, err := s.SendUserPollMessage(ctx, a.ID, b.ID, 1862102, "q", ordinaryPollDraft())
			if err != nil || dup {
				t.Fatalf("poll: dup=%v err=%v", dup, err)
			}
			return []store.Message{m}, perOwner
		}},
		{"album", func(t *testing.T) ([]store.Message, map[int64]int) {
			t.Helper()
			res, err := s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 1862, 1862110, f.granted(t, a.ID, true), f.granted(t, a.ID, false)))
			if err != nil || res.Duplicate {
				t.Fatalf("album: dup=%v err=%v", res.Duplicate, err)
			}
			return res.Messages, res.PerOwner
		}},
		{"forward", func(t *testing.T) ([]store.Message, map[int64]int) {
			t.Helper()
			perOwner, sent, err := s.ForwardMessages(ctx, a.ID, store.PeerTypeUser, b.ID,
				[]store.ForwardSource{{FromID: a.ID, Date: src.Date, Text: src.Text}, {FromID: a.ID, Date: src.Date, Text: "two"}},
				[]int64{1862120, 1862121})
			if err != nil || len(sent) != 2 {
				t.Fatalf("forward: sent=%d err=%v", len(sent), err)
			}
			msgs := make([]store.Message, len(sent))
			for i, fwd := range sent {
				msgs[i] = fwd.Message
			}
			return msgs, perOwner
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := captureOwnerVisibleSnapshots(t, s, b.ID)
			rowsBefore := ownerMessageRows(t, s, b.ID)
			senderBefore := getOwnerAllocatorState(t, s, a.ID)
			listener := listenForUpdateNotifications(t, s)

			sent, perOwner := tc.send(t)

			for _, m := range sent {
				assertWithheld(t, s, m)
			}
			if _, ok := perOwner[b.ID]; ok {
				t.Fatalf("per-owner pts = %v, want no entry for the recipient", perOwner)
			}
			if perOwner[a.ID] <= 0 {
				t.Fatalf("per-owner pts = %v, want the sender's pts", perOwner)
			}
			if after := getOwnerAllocatorState(t, s, a.ID); after.pts != senderBefore.pts+int64(len(sent)) {
				t.Fatalf("sender pts %d -> %d, want +%d", senderBefore.pts, after.pts, len(sent))
			}
			assertOwnerVisibleSnapshotsUnchanged(t, s, before)
			if n := ownerMessageRows(t, s, b.ID); n != rowsBefore {
				t.Fatalf("recipient rows %d -> %d, want unchanged", rowsBefore, n)
			}
			assertNoUpdateNotification(t, listener)
		})
	}
}

func TestDirectSendsRefusedWhenSenderBlockedRecipient(t *testing.T) {
	t.Parallel()
	for _, mutual := range []bool{false, true} {
		t.Run(map[bool]string{false: "sender block", true: "mutual block"}[mutual], func(t *testing.T) {
			t.Parallel()
			f := newAlbumFixture(t)
			s := f.s
			ctx := context.Background()
			phone := map[bool]string{false: "+1555186201", true: "+1555186202"}[mutual]
			a, b := mustUser(t, s, phone+"1"), mustUser(t, s, phone+"2")
			src := send(t, s, a, a, "saved", 0)
			mustBlock(t, s, a.ID, b.ID)
			if mutual {
				mustBlock(t, s, b.ID, a.ID)
			}
			files := []store.File{f.granted(t, a.ID, true), f.granted(t, a.ID, true)}
			before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID)
			rowsBefore := ownerMessageRows(t, s, a.ID) + ownerMessageRows(t, s, b.ID)
			pollsBefore := pollRowCount(t, s)
			listener := listenForUpdateNotifications(t, s)

			if _, _, _, _, err := s.SendMessage(ctx, a.ID, b.ID, "hi", 1862201, 0, 0); !errors.Is(err, store.ErrYouBlockedUser) {
				t.Errorf("message = %v, want ErrYouBlockedUser", err)
			}
			if _, _, _, _, err := s.SendUserPollMessage(ctx, a.ID, b.ID, 1862202, "q", ordinaryPollDraft()); !errors.Is(err, store.ErrYouBlockedUser) {
				t.Errorf("poll = %v, want ErrYouBlockedUser", err)
			}
			if _, err := s.SendUserAlbum(ctx, b.ID, albumOf(a.ID, 1863, 1862210, files...)); !errors.Is(err, store.ErrYouBlockedUser) {
				t.Errorf("album = %v, want ErrYouBlockedUser", err)
			}
			if _, _, err := s.ForwardMessages(ctx, a.ID, store.PeerTypeUser, b.ID,
				[]store.ForwardSource{{FromID: a.ID, Date: src.Date, Text: src.Text}}, []int64{1862220}); !errors.Is(err, store.ErrYouBlockedUser) {
				t.Errorf("forward = %v, want ErrYouBlockedUser", err)
			}

			assertOwnerVisibleSnapshotsUnchanged(t, s, before)
			if n := ownerMessageRows(t, s, a.ID) + ownerMessageRows(t, s, b.ID); n != rowsBefore {
				t.Errorf("message rows %d -> %d, want unchanged", rowsBefore, n)
			}
			if n := pollRowCount(t, s); n != pollsBefore {
				t.Errorf("poll rows %d -> %d, want unchanged", pollsBefore, n)
			}
			if n := f.grantCount(t, a.ID); n != len(files) {
				t.Errorf("album grants = %d, want %d unconsumed", n, len(files))
			}
			assertNoUpdateNotification(t, listener)
		})
	}
}

func TestSendSelfUnaffectedByBlocks(t *testing.T) {
	t.Parallel()
	s := open(t)
	a, b := mustUser(t, s, "+15551862301"), mustUser(t, s, "+15551862302")
	mustBlock(t, s, a.ID, b.ID)
	mustBlock(t, s, b.ID, a.ID)
	if m := send(t, s, a, a, "note", 1862301); m.PeerID != a.ID {
		t.Fatalf("self send = %+v", m)
	}
}

func TestWithheldSendStaysWithheldAcrossUnblock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a, b := mustUser(t, s, "+15551862401"), mustUser(t, s, "+15551862402")
	mustBlock(t, s, b.ID, a.ID)

	withheld := send(t, s, a, b, "during", 1862401)
	if _, err := s.UnblockUser(ctx, b.ID, a.ID); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	before := captureOwnerVisibleSnapshots(t, s, b.ID)
	rowsBefore := ownerMessageRows(t, s, b.ID)

	resent, _, recipientPts, dup, err := s.SendMessage(ctx, a.ID, b.ID, "during", 1862401, 0, 0)
	if err != nil || !dup || recipientPts != 0 || resent.LocalID != withheld.LocalID {
		t.Fatalf("resend after unblock: local=%d dup=%v recipientPts=%d err=%v, want original withheld answer", resent.LocalID, dup, recipientPts, err)
	}
	assertWithheld(t, s, resent)
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	if n := ownerMessageRows(t, s, b.ID); n != rowsBefore {
		t.Fatalf("recipient rows %d -> %d after resend, want unchanged", rowsBefore, n)
	}

	after := send(t, s, a, b, "after", 1862402)
	if after.PeerLocalID == 0 || msgAt(t, s, b.ID, after.PeerLocalID).Text != "after" {
		t.Fatalf("send after unblock = %+v, want delivered", after)
	}
	history, err := s.History(ctx, b.ID, store.PeerTypeUser, a.ID, 0, 100)
	if err != nil {
		t.Fatalf("recipient history: %v", err)
	}
	if len(history) != 1 || history[0].Text != "after" {
		t.Fatalf("recipient history = %+v, want only the post-unblock message", history)
	}
}

func TestResendKeepsAnswerAfterSenderBlocksRecipient(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a, b := mustUser(t, s, "+15551862501"), mustUser(t, s, "+15551862502")
	delivered := send(t, s, a, b, "before", 1862501)
	src := send(t, s, a, a, "saved", 0)
	_, fwd, err := s.ForwardMessages(ctx, a.ID, store.PeerTypeUser, b.ID,
		[]store.ForwardSource{{FromID: a.ID, Date: src.Date, Text: src.Text}}, []int64{1862502})
	if err != nil {
		t.Fatalf("forward before block: %v", err)
	}
	mustBlock(t, s, a.ID, b.ID)

	m, _, _, dup, err := s.SendMessage(ctx, a.ID, b.ID, "before", 1862501, 0, 0)
	if err != nil || !dup || m.LocalID != delivered.LocalID {
		t.Fatalf("resend after own block: local=%d dup=%v err=%v, want stored answer", m.LocalID, dup, err)
	}
	_, sent, err := s.ForwardMessages(ctx, a.ID, store.PeerTypeUser, b.ID,
		[]store.ForwardSource{{FromID: a.ID, Date: src.Date, Text: src.Text}}, []int64{1862502})
	if err != nil || len(sent) != 1 || sent[0].Message.LocalID != fwd[0].Message.LocalID {
		t.Fatalf("resend-only forward batch: sent=%+v err=%v, want stored answer", sent, err)
	}
	before := captureOwnerVisibleSnapshots(t, s, a.ID, b.ID)
	if _, _, err = s.ForwardMessages(ctx, a.ID, store.PeerTypeUser, b.ID,
		[]store.ForwardSource{{FromID: a.ID, Date: src.Date, Text: src.Text}, {FromID: a.ID, Date: src.Date, Text: src.Text}},
		[]int64{1862502, 1862503}); !errors.Is(err, store.ErrYouBlockedUser) {
		t.Fatalf("mixed forward batch = %v, want ErrYouBlockedUser", err)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
}

func TestWithheldMessageFollowUpsReachOnlySender(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	a, b := mustUser(t, s, "+15551862601"), mustUser(t, s, "+15551862602")
	mustBlock(t, s, b.ID, a.ID)
	m := send(t, s, a, b, "withheld", 1862601)
	before := captureOwnerVisibleSnapshots(t, s, b.ID)
	rowsBefore := ownerMessageRows(t, s, b.ID)

	senderBefore := getOwnerAllocatorState(t, s, a.ID)
	if _, pts, err := s.EditMessage(ctx, a.ID, m.LocalID, "edited"); err != nil || int64(pts) != senderBefore.pts+1 {
		t.Fatalf("edit: pts=%d err=%v, want sender pts %d", pts, err, senderBefore.pts+1)
	}
	if got := msgAt(t, s, a.ID, m.LocalID).Text; got != "edited" {
		t.Fatalf("sender text = %q, want edited", got)
	}
	targets, err := s.SendReaction(ctx, a.ID, m.LocalID, "👍")
	if err != nil || len(targets) != 1 || targets[0].OwnerID != a.ID {
		t.Fatalf("react: targets=%+v err=%v, want only the sender's copy", targets, err)
	}
	var orphans int
	if err = store.StorePool(s).QueryRow(ctx, `SELECT count(*) FROM message_reactions WHERE owner_id = $1`, b.ID).Scan(&orphans); err != nil {
		t.Fatalf("count recipient reactions: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("recipient reaction rows = %d, want 0", orphans)
	}
	if targets, err = s.ClearReaction(ctx, a.ID, m.LocalID); err != nil || len(targets) != 1 || targets[0].OwnerID != a.ID {
		t.Fatalf("clear reaction: targets=%+v err=%v, want only the sender's copy", targets, err)
	}
	perOwner, err := s.DeleteMessages(ctx, a.ID, []int64{m.LocalID}, true)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, ok := perOwner[b.ID]; ok || perOwner[a.ID] <= 0 {
		t.Fatalf("revoke per-owner = %v, want only the sender", perOwner)
	}
	assertOwnerVisibleSnapshotsUnchanged(t, s, before)
	if n := ownerMessageRows(t, s, b.ID); n != rowsBefore {
		t.Fatalf("recipient rows %d -> %d, want unchanged", rowsBefore, n)
	}
}
