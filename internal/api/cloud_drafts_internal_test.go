package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestSaveDraftUsesPublishedMessageLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openGetAllDraftsTestStore(t)
	owner, err := s.CreateUser(ctx, "+15551234801")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551234802")
	if err != nil {
		t.Fatalf("create peer: %v", err)
	}
	if _, _, _, duplicate, err := s.SendMessage(ctx, owner.ID, peer.ID, "seed dialog", 1234801, 0, 0); err != nil || duplicate {
		t.Fatalf("seed dialog: duplicate=%v err=%v", duplicate, err)
	}

	h := testHandlers(s)
	saveDraft := func(message string) error {
		t.Helper()
		req := &tg.MessagesSaveDraftRequest{Peer: InputPeerUser(owner.ID, peer.ID), Message: message}
		req.SetFlags()
		var buf bin.Buffer
		if err := req.Encode(&buf); err != nil {
			return err
		}
		_, _, _, err := h.handleSaveDraftAfterReplyOnConn(nil, &mtproto.Request{
			Ctx: ctx, UserID: owner.ID, Buf: &buf,
		})
		return err
	}

	if err := saveDraft(strings.Repeat("x", messageLengthMax)); err != nil {
		t.Fatalf("save draft at message_max_length %d: %v", messageLengthMax, err)
	}
	if err := saveDraft(strings.Repeat("x", messageLengthMax+1)); !errors.Is(err, errMessageTooLong) {
		t.Fatalf("save draft above message_max_length returned %v, want %v", err, errMessageTooLong)
	}
}
