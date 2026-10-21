package api_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestChatRemovalStaleDateDoesNotGrowStateHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551290401")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551290402")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "stale removal date", []int64{member.ID})
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	state, err := s.StateWithoutChannelUnread(ctx, member.ID)
	if err != nil {
		t.Fatalf("read member state: %v", err)
	}
	if _, err := s.DeleteChat(ctx, chat.ID, creator.ID); err != nil {
		t.Fatalf("delete group: %v", err)
	}

	key := [8]byte{0x22, 0x10, 0x20, 0x26, 0x10, 0x20, 0x04, 0x01}
	keyID := mtproto.AuthKeyIDInt64(key)
	request := &tg.UpdatesGetDifferenceRequest{Pts: state.Pts, Qts: state.Qts, Date: 0}
	fixedNow := time.Now().Add(time.Hour)
	for i := range 25 {
		result, err := api.GetDifferenceForTestWithAuthKeyAndNow(s, member.ID, key, func() time.Time {
			return fixedNow
		}, request)
		if err != nil {
			t.Fatalf("stale getDifference %d: %v", i, err)
		}

		var updates []tg.UpdateClass
		switch difference := result.(type) {
		case *tg.UpdatesDifference:
			updates = difference.OtherUpdates
		case *tg.UpdatesDifferenceSlice:
			updates = difference.OtherUpdates
		default:
			t.Fatalf("stale getDifference %d = %T, want a difference response", i, result)
		}
		if !slices.ContainsFunc(updates, func(update tg.UpdateClass) bool {
			removal, ok := update.(*tg.UpdateChatParticipantDelete)
			return ok && removal.ChatID == chat.ID
		}) {
			t.Fatalf("stale getDifference %d omitted pending removal for chat %d", i, chat.ID)
		}
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer conn.Close(ctx) //nolint:errcheck // deferred close is cleanup for this test-owned connection
	var historyRows int
	if err := conn.QueryRow(ctx, `
		SELECT count(*)
		FROM chat_removal_client_state_history
		WHERE owner_id = $1 AND auth_key_id = $2 AND chat_id = $3
	`, member.ID, keyID, chat.ID).Scan(&historyRows); err != nil {
		t.Fatalf("count removal state history: %v", err)
	}
	if historyRows > 4 {
		t.Fatalf("stale getDifference archived %d history rows, want at most 4", historyRows)
	}
}
