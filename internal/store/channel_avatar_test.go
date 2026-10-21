package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const channelAvatarConcurrencyTimeout = 30 * time.Second

func channelAvatarStoreHistoryCount(t *testing.T, s *store.Store, channelID int64) int {
	t.Helper()
	rows, err := s.ChannelHistory(context.Background(), channelID, 0, 50)
	if err != nil {
		t.Fatalf("channel history: %v", err)
	}
	return len(rows)
}

func TestChannelPhotoAttachmentInterlocksMediaErasure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559132001")
	channel, err := s.CreateChannel(ctx, creator.ID, "Channel avatar retention", "", false)
	if err != nil {
		t.Fatalf("create broadcast channel: %v", err)
	}
	otherChannel, err := s.CreateChannel(ctx, creator.ID, "Second avatar channel", "", true)
	if err != nil {
		t.Fatalf("create supergroup: %v", err)
	}
	photo := storedPhotoFile(t, s, creator.ID)

	// The attachment path's first file-row lock is compatible with this shared
	// hold. The eraser's exclusive SKIP LOCKED half must leave its candidate for
	// the next pass instead of removing bytes underneath the pending reference.
	release, err := store.HoldFileRowShared(ctx, s, photo.ID)
	if err != nil {
		t.Fatalf("hold file row for attachment: %v", err)
	}
	counts, err := s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch)
	if err != nil || counts.Contended != 1 || counts.Erased != 0 || !rowPresent(t, s, photo.ID) {
		release()
		t.Fatalf("erasure concurrent with attachment = %+v, file present=%v err=%v", counts, rowPresent(t, s, photo.ID), err)
	}
	type attachResult struct {
		changed bool
		channel store.Channel
		message store.ChannelMessage
		pts     int
		err     error
	}
	attached := make(chan attachResult, 1)
	go func() {
		changed, channel, message, pts, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, photo.ID)
		attached <- attachResult{changed: changed, channel: channel, message: message, pts: pts, err: err}
	}()
	deadline := time.Now().Add(channelAvatarConcurrencyTimeout)
	waiting := false
	for time.Now().Before(deadline) {
		if err := store.StorePool(s).QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND pid <> pg_backend_pid()
				  AND wait_event_type = 'Lock' AND query LIKE '%LockPhotoFileForAttach%'
			)
		`).Scan(&waiting); err != nil {
			release()
			t.Fatalf("observe channel photo attach waiting on held file row: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !waiting {
		release()
		t.Fatalf("channel photo attach did not wait on held file row")
	}
	release()
	var attach attachResult
	select {
	case attach = <-attached:
	case <-time.After(channelAvatarConcurrencyTimeout):
		t.Fatal("channel photo attach did not finish after releasing held file row")
	}
	if attach.err != nil || !attach.changed || attach.channel.CurrentPhotoFileID == nil || *attach.channel.CurrentPhotoFileID != photo.ID || attach.message.Action != store.ChannelMessageActionEditPhoto || attach.pts != 2 {
		t.Fatalf("select photo after eraser skipped locked row: changed=%v channel=%+v edit=%+v pts=%d err=%v", attach.changed, attach.channel, attach.message, attach.pts, attach.err)
	}
	if _, _, _, _, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, photo.ID); !errors.Is(err, store.ErrChannelPhotoUnavailable) {
		t.Fatalf("reattach current channel photo = %v, want unavailable", err)
	}
	scan, err := s.MediaErasureScan(ctx, future(), 0, 0, store.ErasureScanBatch)
	if err != nil || scan.Counts.SkippedChannelPhotoRef != 1 || len(scan.Unreferenced) != 0 {
		t.Fatalf("current channel photo erasure scan = %+v err=%v, want selected reference protected", scan, err)
	}
	if counts, err = s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil || counts.Erased != 0 || !rowPresent(t, s, photo.ID) {
		t.Fatalf("current channel photo sweep = %+v, file present=%v err=%v", counts, rowPresent(t, s, photo.ID), err)
	}
	if _, _, _, _, err := s.SetChannelPhoto(ctx, otherChannel.ID, creator.ID, photo.ID); !errors.Is(err, store.ErrChannelPhotoUnavailable) {
		t.Fatalf("attach one channel's avatar to another channel = %v, want unavailable", err)
	}

	changed, cleared, deleteMessage, pts, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, 0)
	if err != nil || !changed || cleared.CurrentPhotoFileID != nil || cleared.CurrentPhotoUploaderID != nil || deleteMessage.Action != store.ChannelMessageActionDeletePhoto || pts != 3 {
		t.Fatalf("clear current channel photo: changed=%v channel=%+v clear=%+v pts=%d err=%v", changed, cleared, deleteMessage, pts, err)
	}
	if _, err := s.FileForDownload(ctx, photo.ID, photo.AccessHash, creator.ID); err != nil {
		t.Fatalf("creator lost retained photo history after clear: %v", err)
	}
	scan, err = s.MediaErasureScan(ctx, future(), 0, 0, store.ErasureScanBatch)
	if err != nil || scan.Counts.SkippedChannelRef != 1 || scan.Counts.SkippedChannelPhotoRef != 0 || len(scan.Unreferenced) != 0 {
		t.Fatalf("cleared channel photo history erasure scan = %+v err=%v, want historical reference protected", scan, err)
	}
	if counts, err = s.SweepMediaErasure(ctx, future(), store.ErasureScanBatch); err != nil || counts.Erased != 0 || !rowPresent(t, s, photo.ID) {
		t.Fatalf("retained channel photo history sweep = %+v, file present=%v err=%v", counts, rowPresent(t, s, photo.ID), err)
	}
	if _, _, _, _, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, photo.ID); !errors.Is(err, store.ErrChannelPhotoUnavailable) {
		t.Fatalf("reselect retained historical channel photo = %v, want unavailable", err)
	}
}

func TestChannelPhotoEventFailureRollsBackSelectionVersionAndPTS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15559132011")
	channel, err := s.CreateChannel(ctx, creator.ID, "Channel avatar rollback", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	photo := storedPhotoFile(t, s, creator.ID)
	before, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok {
		t.Fatalf("load channel before injected failure: ok=%v err=%v", ok, err)
	}
	beforePTS, err := s.ChannelState(ctx, channel.ID)
	if err != nil {
		t.Fatalf("load channel pts before injected failure: %v", err)
	}
	beforeHistory := channelAvatarStoreHistoryCount(t, s, channel.ID)
	_, err = store.StorePool(s).Exec(ctx, `
		CREATE FUNCTION reject_channel_photo_event() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'injected channel photo event failure'; END;
		$$;
		CREATE TRIGGER reject_channel_photo_event BEFORE INSERT ON channel_events
		FOR EACH ROW EXECUTE FUNCTION reject_channel_photo_event();
	`)
	if err != nil {
		t.Fatalf("install channel event failure trigger: %v", err)
	}

	if _, _, _, _, err := s.SetChannelPhoto(ctx, channel.ID, creator.ID, photo.ID); err == nil {
		t.Fatal("photo mutation succeeded despite channel event insert failure")
	}
	after, ok, err := s.ChannelByID(ctx, channel.ID)
	if err != nil || !ok {
		t.Fatalf("reload channel after rollback: ok=%v err=%v", ok, err)
	}
	if after.CurrentPhotoFileID != nil || after.CurrentPhotoUploaderID != nil || after.Version != before.Version {
		t.Fatalf("failed mutation left channel at version %d/photo %v/uploader %v, want %d/empty", after.Version, after.CurrentPhotoFileID, after.CurrentPhotoUploaderID, before.Version)
	}
	afterPTS, err := s.ChannelState(ctx, channel.ID)
	if err != nil || afterPTS != beforePTS {
		t.Fatalf("failed mutation channel pts=%d err=%v, want unchanged %d", afterPTS, err, beforePTS)
	}
	if got := channelAvatarStoreHistoryCount(t, s, channel.ID); got != beforeHistory {
		t.Fatalf("failed mutation left %d history rows, want %d", got, beforeHistory)
	}
}

func TestChannelPhotoFreshAttachmentSerializesAgainstReuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	creator := mustUser(t, s, "+15559132021")
	first, err := s.CreateChannel(ctx, creator.ID, "First photo selection", "", false)
	if err != nil {
		t.Fatalf("create first channel: %v", err)
	}
	second, err := s.CreateChannel(ctx, creator.ID, "Second photo selection", "", false)
	if err != nil {
		t.Fatalf("create second channel: %v", err)
	}
	photo := storedPhotoFile(t, s, creator.ID)

	blocker, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect file-row blocker: %v", err)
	}
	t.Cleanup(func() {
		if err := blocker.Close(context.Background()); err != nil {
			t.Errorf("close file-row blocker: %v", err)
		}
	})
	block, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin file-row blocker: %v", err)
	}
	t.Cleanup(func() { _ = block.Rollback(context.Background()) }) //nolint:errcheck // release on failure
	var lockedFileID int64
	if err := block.QueryRow(ctx, "SELECT id FROM files WHERE id = $1 FOR NO KEY UPDATE", photo.ID).Scan(&lockedFileID); err != nil {
		t.Fatalf("hold file row: %v", err)
	}

	observer, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect lock observer: %v", err)
	}
	t.Cleanup(func() {
		if err := observer.Close(context.Background()); err != nil {
			t.Errorf("close lock observer: %v", err)
		}
	})

	start := make(chan struct{})
	started := make(chan struct{}, 2)
	results := make(chan error, 2)
	for _, channelID := range []int64{first.ID, second.ID} {
		go func() {
			started <- struct{}{}
			<-start
			_, _, _, _, err := s.SetChannelPhoto(ctx, channelID, creator.ID, photo.ID)
			results <- err
		}()
	}
	<-started
	<-started
	close(start)

	deadline := time.Now().Add(channelAvatarConcurrencyTimeout)
	waiting := int64(0)
	for time.Now().Before(deadline) {
		if err := observer.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query LIKE '%LockPhotoFileForAttach%'
		`).Scan(&waiting); err != nil {
			t.Fatalf("observe photo attachment lock waiters: %v", err)
		}
		if waiting == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waiting != 2 {
		t.Fatalf("concurrent photo mutations waiting on held file row = %d, want 2", waiting)
	}
	if err := block.Commit(ctx); err != nil {
		t.Fatalf("release held file row: %v", err)
	}

	successes := 0
	unavailable := 0
	for range 2 {
		select {
		case err := <-results:
			switch {
			case err == nil:
				successes++
			case errors.Is(err, store.ErrChannelPhotoUnavailable):
				unavailable++
			default:
				t.Fatalf("concurrent channel photo attachment = %v, want one success and one unavailable", err)
			}
		case <-time.After(channelAvatarConcurrencyTimeout):
			t.Fatal("concurrent channel photo attachment did not finish")
		}
	}
	if successes != 1 || unavailable != 1 {
		t.Fatalf("concurrent channel photo attachments: success=%d unavailable=%d, want 1/1", successes, unavailable)
	}
}
