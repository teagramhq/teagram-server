package store

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestNotificationSchedulerMergesMoreThanLaneCapacityOfReconciles(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	scheduler := newNotificationScheduler(ctx)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(func() {
		release()
		scheduler.stop()
	})
	if !scheduler.submit("status:700", notificationTask{
		ctx: ctx,
		run: func(context.Context) {
			close(firstStarted)
			<-releaseFirst
		},
	}) {
		t.Fatal("submit blocking transition")
	}
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatalf("blocking transition did not start: %v", ctx.Err())
	}

	const reconcileChunks = notificationLanePendingLimit + 3
	const viewersPerChunk = maxStatusReconcileViewersPerPayload
	wantViewerIDs := make([]int64, reconcileChunks*viewersPerChunk)
	for i := range wantViewerIDs {
		wantViewerIDs[i] = int64(i + 1)
	}
	delivered := make(chan []int64, 1)
	for chunk := range reconcileChunks {
		start := chunk * viewersPerChunk
		end := start + viewersPerChunk
		reconcileCtx := WithStatusReconcile(ctx, wantViewerIDs[start:end]...)
		if !scheduler.submit("status:700", notificationTask{
			ctx:      reconcileCtx,
			coalesce: true,
			run: func(ctx context.Context) {
				delivered <- StatusReconcileViewerIDsFromContext(ctx)
			},
		}) {
			t.Fatalf("scheduler rejected reconcile chunk %d", chunk+1)
		}
	}
	scheduler.mu.Lock()
	gotPending := scheduler.pending
	scheduler.mu.Unlock()
	if gotPending != 1 {
		t.Fatalf("queued reconciliation tasks = %d, want one merged task", gotPending)
	}

	release()
	select {
	case got := <-delivered:
		if !slices.Equal(got, wantViewerIDs) {
			t.Fatalf("merged reconciliation viewers = %d IDs, want all %d", len(got), len(wantViewerIDs))
		}
	case <-ctx.Done():
		t.Fatalf("merged reconciliation did not run: %v", ctx.Err())
	}
}
