package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestChannelWidthRefusalDoesNotNotifyPostSubscribers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551498101")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "width refusal", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect channel post listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close(context.Background()) }) //nolint:errcheck // teardown
	if _, err = listener.Exec(ctx, "LISTEN tg_channel_post"); err != nil {
		t.Fatalf("listen for channel post notifications: %v", err)
	}
	if _, err = listener.Exec(ctx, `UPDATE channel_state SET pts = $2 WHERE channel_id = $1`, channel.ID, int64(1<<31-1)); err != nil {
		t.Fatalf("exhaust channel pts: %v", err)
	}

	reply, err := api.SendMessageForTest(s, creator.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerChannel(creator.ID, channel.ID), Message: "refused", RandomID: 1498101,
	})
	if err == nil || reply != nil {
		t.Fatalf("exhausted channel send returned reply=%T err=%v, want refusal", reply, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	notification, waitErr := listener.WaitForNotification(waitCtx)
	if waitErr == nil {
		t.Fatalf("unexpected channel post notification: %+v", notification)
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("waiting for channel post notification: %v", waitErr)
	}
}
