package api

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestLivePushPreservesMessageGroupedID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	sender, err := s.CreateUser(ctx, "+15551298421")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	owner, err := s.CreateUser(ctx, "+15551298422")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	if _, _, _, _, err = s.SendMessage(ctx, sender.ID, owner.ID, "album item", 1, 0, 0); err != nil {
		t.Fatalf("send message: %v", err)
	}
	setGroupedIDForPushTest(t, dsn,
		`UPDATE messages SET grouped_id = $3 WHERE owner_id = $1 AND local_id = $2`, owner.ID, int64(1), int64(7351))

	conn := &fakePushConn{}
	log := slog.New(slog.DiscardHandler)
	u := &Updater{h: &handlers{store: s, log: log, peers: pgtest.PeerDeriver()}, log: log}
	u.deliver(ctx, owner.ID, []pushConn{conn}, func(fromPts int) (updateBatch, error) {
		return u.h.buildUpdates(ctx, owner.ID, fromPts, false)
	})
	message := pushedMessage(t, conn, "updateNewMessage")
	if groupedID, ok := message.GetGroupedID(); !ok || groupedID != 7351 {
		t.Fatalf("live updateNewMessage grouped_id = %d, present=%v; want 7351", groupedID, ok)
	}
}

func TestLivePushPreservesChannelMessageGroupedID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	owner, err := s.CreateUser(ctx, "+15551298431")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	channel, err := s.CreateChannel(ctx, owner.ID, "Grouped IDs", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	post, _, _, err := s.PostChannelMessage(ctx, channel.ID, owner.ID, "album item", 1, nil, 0)
	if err != nil {
		t.Fatalf("post channel message: %v", err)
	}
	setGroupedIDForPushTest(t, dsn,
		`UPDATE channel_messages SET grouped_id = $3 WHERE channel_id = $1 AND local_id = $2`, channel.ID, post.LocalID, int64(7351))

	conn := &fakePushConn{}
	log := slog.New(slog.DiscardHandler)
	u := &Updater{h: &handlers{store: s, log: log, peers: pgtest.PeerDeriver()}, log: log}
	u.deliverChannelPost(ctx, channel.ID, func(userID int64) []pushConn {
		if userID == owner.ID {
			return []pushConn{conn}
		}
		return nil
	})
	message := pushedMessage(t, conn, "updateNewChannelMessage")
	if groupedID, ok := message.GetGroupedID(); !ok || groupedID != 7351 {
		t.Fatalf("live updateNewChannelMessage grouped_id = %d, present=%v; want 7351", groupedID, ok)
	}
}

func setGroupedIDForPushTest(t *testing.T, dsn, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close test database connection: %v", err)
		}
	}()
	if _, err := conn.Exec(ctx, query, args...); err != nil {
		t.Fatalf("set grouped_id: %v", err)
	}
}

func pushedMessage(t *testing.T, conn *fakePushConn, updateName string) *tg.Message {
	t.Helper()
	if len(conn.got) != 1 {
		t.Fatalf("%s push batches = %d, want 1", updateName, len(conn.got))
	}
	var messageClass tg.MessageClass
	messageCount := 0
	for _, pushed := range conn.got[0].Updates {
		switch updateName {
		case "updateNewMessage":
			update, ok := pushed.(*tg.UpdateNewMessage)
			if !ok {
				t.Fatalf("push update = %T, want *tg.UpdateNewMessage", pushed)
			}
			messageClass = update.Message
			messageCount++
		case "updateNewChannelMessage":
			if _, ok := pushed.(*tg.UpdateMessageID); ok {
				continue
			}
			update, ok := pushed.(*tg.UpdateNewChannelMessage)
			if !ok {
				t.Fatalf("push update = %T, want *tg.UpdateNewChannelMessage", pushed)
			}
			messageClass = update.Message
			messageCount++
		default:
			t.Fatalf("unexpected update name %q", updateName)
		}
	}
	if messageCount != 1 {
		t.Fatalf("%s updates = %d, want one message update", updateName, messageCount)
	}
	message, ok := messageClass.(*tg.Message)
	if !ok {
		t.Fatalf("pushed message = %T, want *tg.Message", messageClass)
	}
	return message
}
