package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestChatControlsReadRightsAfterConcurrentRestriction(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"rename", "pin", "unpin"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dsn := pgtest.DSN(t)
			s := openDSN(t, dsn)
			a, b := mustUser(t, s, "+15558912001"), mustUser(t, s, "+15558912002")
			ch := chatWith(t, s, a, b)
			msg, _, _, err := s.SendChatMessage(ctx, store.FanOut{ChatID: ch.ID, FromID: a.ID, Text: "target", RandomID: 1})
			if err != nil {
				t.Fatal(err)
			}
			id := int32(msg.LocalID) //nolint:gosec // test message id
			conn, err := pgx.Connect(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := conn.Close(ctx); err != nil {
					t.Error(err)
				}
			}()
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
					t.Error(err)
				}
			}()
			right := "pin_messages"
			if operation == "rename" {
				right = "change_info"
			}
			if _, err := tx.Exec(ctx, "UPDATE chats SET default_banned_rights = $2 WHERE id = $1", ch.ID, []string{right}); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				if operation == "rename" {
					_, _, _, err := s.SetChatTitle(ctx, ch.ID, b.ID, "blocked")
					result <- err
					return
				}
				var target *int32
				if operation == "pin" {
					target = &id
				}
				_, _, err := s.SetChatPinnedMessage(ctx, ch.ID, b.ID, target)
				result <- err
			}()
			if err := store.WaitForLockWaiters(ctx, s, 1); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, store.ErrChatAdminRequired) {
				t.Fatalf("%s after restriction: %v, want ErrChatAdminRequired", operation, err)
			}
		})
	}
}
