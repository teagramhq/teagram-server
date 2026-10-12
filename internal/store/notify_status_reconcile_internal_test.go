package store

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestStatusReconciliationIsNotCoalescedWithTransitions(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	dsn := pgtest.DSN(t)
	s, err := Open(ctx, dsn, pgtest.EncKey(), WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	type statusEvent struct {
		userID    int64
		online    bool
		reconcile bool
		viewerIDs []int64
	}
	events := make(chan statusEvent, 8)
	firstStarted := make(chan struct{})
	barrierDone := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	var callbacks atomic.Int32
	_, stop, err := StartListener(ctx, dsn,
		nil, nil, nil, nil, nil,
		func(ctx context.Context, userID int64, online bool) {
			events <- statusEvent{
				userID:    userID,
				online:    online,
				reconcile: StatusReconcileFromContext(ctx),
				viewerIDs: StatusReconcileViewerIDsFromContext(ctx),
			}
			if userID == 700 && callbacks.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			if userID == 701 {
				barrierDone <- struct{}{}
			}
		},
		nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("start listener: %v", err)
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(releaseFirst) })
		if err := stop(); err != nil {
			t.Errorf("stop listener: %v", err)
		}
	})
	if err := WaitForNotificationListener(ctx, s, 1); err != nil {
		t.Fatalf("wait for listener: %v", err)
	}
	if err := s.Notify(ctx, ChannelStatus, StatusPayload(700, true)); err != nil {
		t.Fatalf("notify initial transition: %v", err)
	}
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatalf("initial status callback did not start: %v", ctx.Err())
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin ordered notifications: %v", err)
	}
	for _, payload := range []string{
		statusReconcilePayload(700, []int64{702}),
		StatusPayload(700, false),
		StatusPayload(701, true),
	} {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, ChannelStatus, payload); err != nil {
			if rollbackErr := tx.Rollback(ctx); rollbackErr != nil {
				t.Errorf("rollback ordered notifications: %v", rollbackErr)
			}
			t.Fatalf("queue status notification %q: %v", payload, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ordered notifications: %v", err)
	}
	select {
	case <-barrierDone:
	case <-ctx.Done():
		t.Fatalf("listener did not queue notifications before barrier: %v", ctx.Err())
	}

	releaseOnce.Do(func() { close(releaseFirst) })
	got := make([]statusEvent, 0, 3)
	for len(got) < 3 {
		select {
		case event := <-events:
			if event.userID == 700 {
				got = append(got, event)
			}
		case <-ctx.Done():
			t.Fatalf("received %d owner callbacks, want transition, reconciliation, transition: %v", len(got), ctx.Err())
		}
	}
	want := []statusEvent{
		{userID: 700, online: true},
		{userID: 700, reconcile: true, viewerIDs: []int64{702}},
		{userID: 700, online: false},
	}
	for i := range want {
		if got[i].userID != want[i].userID || got[i].online != want[i].online || got[i].reconcile != want[i].reconcile || !slices.Equal(got[i].viewerIDs, want[i].viewerIDs) {
			t.Fatalf("owner callback %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestStatusReconcilePayloadsStayBelowNotifyLimit(t *testing.T) {
	t.Parallel()
	const ownerID int64 = 9223372036854775807
	viewerIDs := make([]int64, 500)
	for i := range viewerIDs {
		viewerIDs[i] = ownerID - int64(len(viewerIDs)-i)
	}

	if got := statusReconcilePayload(ownerID, []int64{702, 703}); got != "9223372036854775807|2|702,703" {
		t.Fatalf("status reconciliation payload = %q", got)
	}
	payloads := statusReconcilePayloads(ownerID, viewerIDs)
	if len(payloads) != 2 {
		t.Fatalf("500 viewers encoded in %d payloads, want two", len(payloads))
	}
	for i, payload := range payloads {
		if len(payload) >= 8000 {
			t.Fatalf("payload %d is %d bytes, want fewer than 8000", i, len(payload))
		}
		gotOwner, _, reconcile, gotViewerIDs, err := parseStatusPayload(payload)
		if err != nil {
			t.Fatalf("parse payload %d: %v", i, err)
		}
		if gotOwner != ownerID || !reconcile {
			t.Fatalf("payload %d parsed owner/reconcile = %d/%v", i, gotOwner, reconcile)
		}
		start := i * maxStatusReconcileViewersPerPayload
		end := min(start+maxStatusReconcileViewersPerPayload, len(viewerIDs))
		if !slices.Equal(gotViewerIDs, viewerIDs[start:end]) {
			t.Fatalf("payload %d viewers = %v, want %v", i, gotViewerIDs, viewerIDs[start:end])
		}
	}
}
