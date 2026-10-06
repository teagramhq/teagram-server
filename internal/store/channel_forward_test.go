package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestForwardChannelSourceUsesStoredPostData(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15559310001")
	forwarder := mustUser(t, s, "+15559310002")
	destination := mustUser(t, s, "+15559310003")
	channel := mustChannel(t, s, author.ID, "source")
	seat(t, s, channel, author.ID, forwarder.ID, 0)
	file := storedFile(t, s, author.ID)
	fileID := file.ID
	source, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, author.ID, "original channel text", 31001, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post channel source: duplicate=%v err=%v", duplicate, err)
	}

	forgedDate := time.Date(2001, time.January, 2, 3, 4, 5, 0, time.UTC)
	_, sent, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{{
		FromID: author.ID + 100, Date: forgedDate, Text: "forged snapshot", ChannelID: channel.ID,
		ChannelPost: testChannelPostID(source.LocalID), FileID: 0,
	}}, []int64{31002})
	if err != nil {
		t.Fatalf("forward channel source: %v", err)
	}
	if len(sent) != 1 {
		t.Fatalf("forward result count = %d, want 1", len(sent))
	}
	got := sent[0].Message
	if got.Text != "original channel text" || got.FwdFromID != author.ID || !got.FwdDate.Equal(source.Date) ||
		got.FileID != file.ID || got.FwdChannelID != channel.ID || got.FwdChannelPost != testChannelPostID(source.LocalID) {
		t.Fatalf("forwarded source fields = %+v, want stored text, author, date, file and channel provenance", got)
	}

	received, err := s.History(ctx, destination.ID, store.PeerTypeUser, forwarder.ID, 0, 10)
	if err != nil {
		t.Fatalf("destination history: %v", err)
	}
	if len(received) != 1 || received[0].Text != "original channel text" || received[0].FileID != file.ID {
		t.Fatalf("destination copy = %+v, want original text and file %d", received, file.ID)
	}
}

func TestForwardChannelSourceRejectsNonLivePostsAtomically(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		localID   int64
		tombstone bool
	}{
		{name: "service post", localID: 1},
		{name: "missing post", localID: 999},
		{name: "tombstoned post", localID: 2, tombstone: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := open(t)
			ctx := context.Background()
			author := mustUser(t, s, fmt.Sprintf("+1555931010%d", i))
			forwarder := mustUser(t, s, fmt.Sprintf("+1555931020%d", i))
			destination := mustUser(t, s, fmt.Sprintf("+1555931030%d", i))
			channel := mustChannel(t, s, author.ID, "source")
			seat(t, s, channel, author.ID, forwarder.ID, 0)
			if tc.tombstone {
				post(t, s, channel.ID, author.ID, "removed", 31010+int64(i))
				if err := store.SetChannelPostDeleted(ctx, s, channel.ID, tc.localID); err != nil {
					t.Fatalf("tombstone source post: %v", err)
				}
			}

			_, _, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{{
				FromID: author.ID, Date: time.Now(), Text: "untrusted snapshot", ChannelID: channel.ID,
				ChannelPost: testChannelPostID(tc.localID),
			}}, []int64{31100 + int64(i)})
			if !errors.Is(err, store.ErrMessageInvalid) {
				t.Fatalf("forward %s = %v, want ErrMessageInvalid", tc.name, err)
			}
			if got := historyLen(t, s, forwarder.ID, store.PeerTypeUser, destination.ID); got != 0 {
				t.Fatalf("sender has %d destination rows after rejected forward, want 0", got)
			}
			if got := historyLen(t, s, destination.ID, store.PeerTypeUser, forwarder.ID); got != 0 {
				t.Fatalf("destination has %d rows after rejected forward, want 0", got)
			}
		})
	}
}

func TestForwardChannelSourcePreservesDuplicateRequestMapping(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15559310401")
	forwarder := mustUser(t, s, "+15559310402")
	destination := mustUser(t, s, "+15559310403")
	channel := mustChannel(t, s, author.ID, "source")
	seat(t, s, channel, author.ID, forwarder.ID, 0)
	source, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, author.ID, "repeat me", 31041, nil, 0)
	if err != nil || duplicate {
		t.Fatalf("post source: duplicate=%v err=%v", duplicate, err)
	}

	_, sent, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{
		{ChannelID: channel.ID, ChannelPost: testChannelPostID(source.LocalID), Text: "first stale snapshot"},
		{ChannelID: channel.ID, ChannelPost: testChannelPostID(source.LocalID), Text: "second stale snapshot"},
	}, []int64{31042, 31043})
	if err != nil {
		t.Fatalf("forward duplicate source ids: %v", err)
	}
	if len(sent) != 2 || sent[0].Message.Text != "repeat me" || sent[1].Message.Text != "repeat me" {
		t.Fatalf("forwarded duplicate requests = %+v, want two copies mapped to the same stored source", sent)
	}
}

func TestForwardChannelSourceRejectsWholeBatchWhenOnePostIsInvalid(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15559310701")
	forwarder := mustUser(t, s, "+15559310702")
	destination := mustUser(t, s, "+15559310703")
	channel := mustChannel(t, s, author.ID, "source")
	seat(t, s, channel, author.ID, forwarder.ID, 0)
	first, _ := post(t, s, channel.ID, author.ID, "valid first post", 31071)
	second, _ := post(t, s, channel.ID, author.ID, "tombstoned second post", 31072)
	if err := store.SetChannelPostDeleted(ctx, s, channel.ID, second.LocalID); err != nil {
		t.Fatalf("tombstone second source post: %v", err)
	}

	_, _, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{
		{ChannelID: channel.ID, ChannelPost: testChannelPostID(first.LocalID), Text: "stale valid snapshot"},
		{ChannelID: channel.ID, ChannelPost: testChannelPostID(second.LocalID), Text: "stale tombstoned snapshot"},
	}, []int64{31073, 31074})
	if !errors.Is(err, store.ErrMessageInvalid) {
		t.Fatalf("forward mixed live and tombstoned sources = %v, want ErrMessageInvalid", err)
	}
	if got := historyLen(t, s, forwarder.ID, store.PeerTypeUser, destination.ID); got != 0 {
		t.Fatalf("sender has %d destination rows after rejected batch, want 0", got)
	}
	if got := historyLen(t, s, destination.ID, store.PeerTypeUser, forwarder.ID); got != 0 {
		t.Fatalf("destination has %d rows after rejected batch, want 0", got)
	}
}

func TestForwardSkipsTombstoningPostsBeforeSharedFileLock(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15559310501")
	forwarder := mustUser(t, s, "+15559310502")
	destination := mustUser(t, s, "+15559310503")
	channel := mustChannel(t, s, author.ID, "source")
	seat(t, s, channel, author.ID, forwarder.ID, 0)
	file := storedFile(t, s, author.ID)
	fileID := file.ID
	first, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, author.ID, "first", 31051, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post first source: duplicate=%v err=%v", duplicate, err)
	}
	second, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, author.ID, "second", 31052, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post second source: duplicate=%v err=%v", duplicate, err)
	}

	tombstoneConn, err := store.ChannelPostSummaryControlConnection(ctx, s)
	if err != nil {
		t.Fatalf("connect tombstone transaction: %v", err)
	}
	tombstoneTx, err := tombstoneConn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tombstone transaction: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := tombstoneTx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release tombstone transaction: %v", err)
		}
		if err := tombstoneConn.Close(cleanupCtx); err != nil {
			t.Errorf("close tombstone transaction connection: %v", err)
		}
	})
	if _, err := tombstoneTx.Exec(ctx,
		`UPDATE channel_messages SET deleted = true WHERE channel_id = $1 AND local_id = ANY($2::bigint[])`,
		channel.ID, []int64{first.LocalID, second.LocalID}); err != nil {
		t.Fatalf("hold both channel post tombstones: %v", err)
	}

	eraserHold, err := store.HoldFileRow(ctx, s, file.ID)
	if err != nil {
		t.Fatalf("hold shared file for eraser: %v", err)
	}
	defer eraserHold.Release()
	forwardDone := make(chan error, 1)
	go func() {
		_, _, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{
			{ChannelID: channel.ID, ChannelPost: testChannelPostID(second.LocalID), Text: "stale second", FileID: file.ID},
			{ChannelID: channel.ID, ChannelPost: testChannelPostID(first.LocalID), Text: "stale first", FileID: file.ID},
		}, []int64{31053, 31054})
		forwardDone <- err
	}()
	select {
	case forwardErr := <-forwardDone:
		if !errors.Is(forwardErr, store.ErrMessageInvalid) {
			t.Fatalf("forward against in-flight tombstones = %v, want ErrMessageInvalid", forwardErr)
		}
	case <-time.After(time.Second):
		if err := tombstoneTx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Fatalf("release blocked tombstone: %v", err)
		}
		eraserHold.Release()
		forwardErr := <-forwardDone
		t.Fatalf("forward waited on tombstoning posts and shared file, then returned %v", forwardErr)
	}

	if err := tombstoneTx.Commit(ctx); err != nil {
		t.Fatalf("commit synthetic tombstones: %v", err)
	}
	eraserHold.Release()
	if err := store.SetFileDate(ctx, s, file.ID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatalf("age shared source file: %v", err)
	}
	counts, err := s.SweepMediaErasure(ctx, time.Now(), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("erase tombstoned shared file after contention: %v", err)
	}
	if counts.Erased != 1 {
		t.Fatalf("eraser counts = %+v, want the shared source file erased", counts)
	}
	if got := historyLen(t, s, forwarder.ID, store.PeerTypeUser, destination.ID); got != 0 {
		t.Fatalf("forwarder has %d destination rows after refused forward, want 0", got)
	}
}

func TestChannelForwardCopyRemainsDownloadableAfterSourceTakedown(t *testing.T) {
	t.Parallel()
	s := open(t)
	ctx := context.Background()
	author := mustUser(t, s, "+15559310601")
	forwarder := mustUser(t, s, "+15559310602")
	destination := mustUser(t, s, "+15559310603")
	viewer := mustUser(t, s, "+15559310604")
	channel := mustChannel(t, s, author.ID, "source")
	seat(t, s, channel, author.ID, forwarder.ID, 0)
	seat(t, s, channel, author.ID, viewer.ID, 0)
	file := storedFile(t, s, author.ID)
	fileID := file.ID
	source, _, duplicate, err := s.PostChannelMessage(ctx, channel.ID, author.ID, "media source", 31061, &fileID, 0)
	if err != nil || duplicate {
		t.Fatalf("post media source: duplicate=%v err=%v", duplicate, err)
	}

	if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, viewer.ID); err != nil {
		t.Fatalf("live channel source was not downloadable to its member: %v", err)
	}
	_, sent, err := s.ForwardMessages(ctx, forwarder.ID, store.PeerTypeUser, destination.ID, []store.ForwardSource{{
		ChannelID: channel.ID, ChannelPost: testChannelPostID(source.LocalID),
	}}, []int64{31062})
	if err != nil || len(sent) != 1 || sent[0].Message.FileID != file.ID {
		t.Fatalf("forward media source: sent=%+v err=%v", sent, err)
	}
	if err := store.SetChannelPostDeleted(ctx, s, channel.ID, source.LocalID); err != nil {
		t.Fatalf("tombstone source post: %v", err)
	}
	if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, viewer.ID); !errors.Is(err, store.ErrFileNotFound) {
		t.Fatalf("tombstoned source download = %v, want ErrFileNotFound", err)
	}
	if _, err := s.FileForDownload(ctx, file.ID, file.AccessHash, destination.ID); err != nil {
		t.Fatalf("retained forward copy is not downloadable: %v", err)
	}
}

func testChannelPostID(localID int64) int32 {
	return int32(localID) //nolint:gosec // test fixtures create only the first few channel posts
}
