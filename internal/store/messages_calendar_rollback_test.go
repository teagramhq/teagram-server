package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSearchResultsCalendarPropagatesRollbackFailure(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openDSN(t, dsn)

	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock holder: %v", err)
	}
	defer func() {
		if err := blocker.Close(context.Background()); err != nil {
			t.Errorf("close lock holder: %v", err)
		}
	}()

	lockTx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin lock holder: %v", err)
	}
	defer func() {
		if err := lockTx.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release messages table lock: %v", err)
		}
	}()
	if _, err := lockTx.Exec(ctx, `LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock messages table: %v", err)
	}

	requestCtx, cancel := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, _, _, err := s.SearchResultsCalendar(requestCtx, 1, store.PeerTypeUser, 2, 0, 0, 1)
		result <- err
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, 10*time.Second)
	defer cancelWait()
	if err := store.WaitForLockWaiters(waitCtx, s, 1); err != nil {
		cancel()
		t.Fatalf("wait for calendar query to block: %v", err)
	}
	cancel()

	err = <-result
	if err == nil {
		t.Fatal("search results calendar succeeded after its query was canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("search results calendar error = %v, want original cancellation error", err)
	}
	if !strings.Contains(err.Error(), "rollback search results calendar snapshot:") {
		t.Fatalf("search results calendar error = %v, want rollback failure context", err)
	}
}
