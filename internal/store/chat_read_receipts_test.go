package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type chatReadReceipt struct {
	chatID   int64
	fanoutID int64
	readerID int64
	sentAt   time.Time
	readAt   time.Time
}

func TestChatReadReceiptsDoNotBackfillLegacyInboxMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930011")
	sender := mustUser(t, s, "+15551930012")
	chat, err := s.CreateChat(ctx, sender.ID, "legacy read marker", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "read before receipts", 1930011)
	readerCopy := chatReadReceiptCopy(t, s, reader.ID, message.FanoutID)

	// Model an inbox marker advanced before receipt support existed.
	pool := store.StorePool(s)
	if _, err := pool.Exec(ctx, `UPDATE dialogs SET read_inbox_max_id=$1, unread_count=0 WHERE owner_id=$2 AND peer_type=$3 AND peer_id=$4`, readerCopy.LocalID, reader.ID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("seed legacy read marker: %v", err)
	}
	before, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state before legacy read: %v", err)
	}

	result, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerCopy.LocalID)
	if err != nil {
		t.Fatalf("read legacy marker: %v", err)
	}
	if result.PtsCount != 0 {
		t.Fatalf("legacy marker read pts count = %d, want no-op", result.PtsCount)
	}
	after, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state after legacy read: %v", err)
	}
	if after != before {
		t.Fatalf("reader state after legacy read = %+v, want unchanged %+v", after, before)
	}
	assertNoChatReadReceipt(t, s, chat.ID, message.FanoutID, reader.ID)
}

func TestChatReadReceiptInsertDoesNotDeadlockWithChatSend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930021")
	sender := mustUser(t, s, "+15551930022")
	chat, err := s.CreateChat(ctx, sender.ID, "receipt and send locks", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "read while sending", 1930021)
	readerCopy := chatReadReceiptCopy(t, s, reader.ID, message.FanoutID)

	// Hold the table lock so the read pauses after acquiring owner locks, at
	// ReadMarkers. The send then takes the chat row lock and queues on those
	// owners before the read is released to insert its receipt.
	pool := store.StorePool(s)
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin read gate: %v", err)
	}
	defer func() { _ = gate.Rollback(ctx) }() //nolint:errcheck // cleanup releases the test gate
	if _, err := gate.Exec(ctx, `LOCK TABLE dialogs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock dialogs for read gate: %v", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	readDone := make(chan error, 1)
	go func() {
		_, readErr := s.ReadChatHistory(callCtx, reader.ID, chat.ID, readerCopy.LocalID)
		readDone <- readErr
	}()
	if err := store.WaitForLockWaiters(callCtx, s, 1); err != nil {
		t.Fatalf("read did not reach the dialogs gate: %v", err)
	}

	sendDone := make(chan error, 1)
	go func() {
		_, _, _, sendErr := s.SendChatMessage(callCtx, store.FanOut{
			ChatID: chat.ID, FromID: sender.ID, Text: "concurrent send", RandomID: 1930022,
		})
		sendDone <- sendErr
	}()
	if err := store.WaitForLockWaiters(callCtx, s, 2); err != nil {
		t.Fatalf("send did not queue behind the read owner locks: %v", err)
	}
	if err := gate.Commit(ctx); err != nil {
		t.Fatalf("release read gate: %v", err)
	}

	select {
	case err := <-readDone:
		if err != nil {
			t.Errorf("read while send waits: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("read did not finish after releasing dialogs gate")
	}
	select {
	case err := <-sendDone:
		if err != nil {
			t.Errorf("send after read releases owner locks: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("send did not finish after the read")
	}
	readChatReceipt(t, s, chat.ID, message.FanoutID, reader.ID)
}

func TestChatReadReceiptsCaptureFirstReadTimeAndPersist(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	s := openStore(t, dsn)
	sender := mustUser(t, s, "+15551930001")
	reader := mustUser(t, s, "+15551930002")
	other := mustUser(t, s, "+15551930003")
	chat, err := s.CreateChat(ctx, sender.ID, "receipt times", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, _, _, _, err := s.SendMessage(ctx, sender.ID, other.ID, "offset sender ids", 1930001, 0, 0); err != nil {
		t.Fatalf("offset sender local ids: %v", err)
	}

	first := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "first", 1930002)
	firstCopy := chatReadReceiptCopy(t, s, reader.ID, first.FanoutID)
	if first.LocalID == firstCopy.LocalID {
		t.Fatalf("first sender and reader local ids both equal %d; fixture must exercise distinct owner ids", first.LocalID)
	}
	firstSentAt := ageChatReceiptMessage(t, s, first.FanoutID)
	firstBefore := databaseClock(t, s)
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, firstCopy.LocalID); err != nil {
		t.Fatalf("first read: %v", err)
	}
	firstAfter := databaseClock(t, s)
	firstReceipt := readChatReceipt(t, s, chat.ID, first.FanoutID, reader.ID)
	assertChatReadReceipt(t, firstReceipt, firstSentAt, firstBefore, firstAfter)

	second := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "second", 1930003)
	secondCopy := chatReadReceiptCopy(t, s, reader.ID, second.FanoutID)
	if second.LocalID == secondCopy.LocalID {
		t.Fatalf("second sender and reader local ids both equal %d; fixture must exercise distinct owner ids", second.LocalID)
	}
	secondSentAt := ageChatReceiptMessage(t, s, second.FanoutID)
	time.Sleep(10 * time.Millisecond)
	secondBefore := databaseClock(t, s)
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, secondCopy.LocalID); err != nil {
		t.Fatalf("second read: %v", err)
	}
	secondAfter := databaseClock(t, s)
	secondReceipt := readChatReceipt(t, s, chat.ID, second.FanoutID, reader.ID)
	assertChatReadReceipt(t, secondReceipt, secondSentAt, secondBefore, secondAfter)
	if !firstReceipt.readAt.Before(secondReceipt.readAt) {
		t.Fatalf("read times = %s then %s, want first message time to precede second", firstReceipt.readAt, secondReceipt.readAt)
	}

	// Lower and duplicate reads leave first-read times unchanged.
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, firstCopy.LocalID); err != nil {
		t.Fatalf("out-of-order lower read: %v", err)
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, secondCopy.LocalID); err != nil {
		t.Fatalf("duplicate read: %v", err)
	}
	assertSameChatReadReceipt(t, readChatReceipt(t, s, chat.ID, first.FanoutID, reader.ID), firstReceipt)
	assertSameChatReadReceipt(t, readChatReceipt(t, s, chat.ID, second.FanoutID, reader.ID), secondReceipt)
	if got := chatReadReceiptCount(t, s, chat.ID, reader.ID); got != 2 {
		t.Fatalf("receipt count after replays = %d, want 2", got)
	}

	// A separate Store sees the same timestamps after reopening the pool.
	reopened := openStore(t, dsn)
	assertSameChatReadReceipt(t, readChatReceipt(t, reopened, chat.ID, first.FanoutID, reader.ID), firstReceipt)
	assertSameChatReadReceipt(t, readChatReceipt(t, reopened, chat.ID, second.FanoutID, reader.ID), secondReceipt)
}

func TestChatReadReceiptsBindChatAndSharedFanout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930101")
	firstSender := mustUser(t, s, "+15551930102")
	secondSender := mustUser(t, s, "+15551930103")
	firstChat, err := s.CreateChat(ctx, firstSender.ID, "first receipt chat", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create first chat: %v", err)
	}
	secondChat, err := s.CreateChat(ctx, secondSender.ID, "second receipt chat", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create second chat: %v", err)
	}
	first := sendChatReceiptTestMessage(t, s, firstChat.ID, firstSender.ID, "first chat", 1930101)
	second := sendChatReceiptTestMessage(t, s, secondChat.ID, secondSender.ID, "second chat", 1930102)
	if first.LocalID != second.LocalID {
		t.Fatalf("sender local ids across chats = %d and %d, want equal ids", first.LocalID, second.LocalID)
	}
	firstCopy := chatReadReceiptCopy(t, s, reader.ID, first.FanoutID)
	secondCopy := chatReadReceiptCopy(t, s, reader.ID, second.FanoutID)
	if firstCopy.LocalID == secondCopy.LocalID {
		t.Fatalf("reader local ids across chats both equal %d, want distinct ids", firstCopy.LocalID)
	}
	if first.FanoutID == second.FanoutID {
		t.Fatalf("different chat messages share fanout id %d", first.FanoutID)
	}

	if _, err := s.ReadChatHistory(ctx, reader.ID, firstChat.ID, firstCopy.LocalID); err != nil {
		t.Fatalf("read first chat: %v", err)
	}
	if got := readChatReceipt(t, s, firstChat.ID, first.FanoutID, reader.ID); got.chatID != firstChat.ID {
		t.Fatalf("first receipt chat id = %d, want %d", got.chatID, firstChat.ID)
	}
	assertNoChatReadReceipt(t, s, secondChat.ID, first.FanoutID, reader.ID)
	assertNoChatReadReceipt(t, s, firstChat.ID, second.FanoutID, reader.ID)

	if _, err := s.ReadChatHistory(ctx, reader.ID, secondChat.ID, secondCopy.LocalID); err != nil {
		t.Fatalf("read second chat: %v", err)
	}
	if got := chatReadReceiptCount(t, s, firstChat.ID, reader.ID); got != 1 {
		t.Fatalf("first chat receipt count = %d, want 1", got)
	}
	if got := chatReadReceiptCount(t, s, secondChat.ID, reader.ID); got != 1 {
		t.Fatalf("second chat receipt count = %d, want 1", got)
	}
}

func TestChatReadReceiptsRequireCurrentReaderAndSender(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	creator := mustUser(t, s, "+15551930201")
	reader := mustUser(t, s, "+15551930202")
	sender := mustUser(t, s, "+15551930203")
	removedReader := mustUser(t, s, "+15551930204")
	chat, err := s.CreateChat(ctx, creator.ID, "membership receipts", []int64{reader.ID, sender.ID, removedReader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "sender leaves", 1930201)
	readerCopy := chatReadReceiptCopy(t, s, reader.ID, message.FanoutID)
	if removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, sender.ID, sender.ID); err != nil || !removed {
		t.Fatalf("sender leaves: removed=%v err=%v", removed, err)
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerCopy.LocalID); err != nil {
		t.Fatalf("current reader advances marker: %v", err)
	}
	assertNoChatReadReceipt(t, s, chat.ID, message.FanoutID, reader.ID)

	removedCopy := chatReadReceiptCopy(t, s, removedReader.ID, message.FanoutID)
	if _, _, _, err := s.RemoveChatUser(ctx, chat.ID, removedReader.ID, creator.ID); err != nil {
		t.Fatalf("remove reader: %v", err)
	}
	if _, err := s.ReadChatHistory(ctx, removedReader.ID, chat.ID, removedCopy.LocalID); !errors.Is(err, store.ErrNotMember) {
		t.Fatalf("removed reader error = %v, want ErrNotMember", err)
	}
	if got := chatReadReceiptCount(t, s, chat.ID, removedReader.ID); got != 0 {
		t.Fatalf("removed reader receipts = %d, want 0", got)
	}
}

func TestChatReadReceiptsConcurrentReadCapturesOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930211")
	sender := mustUser(t, s, "+15551930212")
	chat, err := s.CreateChat(ctx, sender.ID, "concurrent receipts", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "one read", 1930211)
	readerCopy := chatReadReceiptCopy(t, s, reader.ID, message.FanoutID)
	before, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state before reads: %v", err)
	}

	start := make(chan struct{})
	results := make(chan store.ChatReadHistoryResult, 2)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			result, readErr := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerCopy.LocalID)
			results <- result
			errs <- readErr
		}()
	}
	ready.Wait()
	close(start)
	var ptsCounts int
	for range 2 {
		result := <-results
		if err := <-errs; err != nil {
			t.Fatalf("concurrent read: %v", err)
		}
		ptsCounts += result.PtsCount
	}
	if ptsCounts != 1 {
		t.Fatalf("concurrent read pts counts sum = %d, want 1", ptsCounts)
	}
	if got := chatReadReceiptCount(t, s, chat.ID, reader.ID); got != 1 {
		t.Fatalf("concurrent read receipts = %d, want 1", got)
	}
	after, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state after reads: %v", err)
	}
	if after.Pts != before.Pts+1 {
		t.Fatalf("reader pts after concurrent reads = %d, want %d", after.Pts, before.Pts+1)
	}
}

func TestChatReadReceiptRetentionUsesMessageSendTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930401")
	sender := mustUser(t, s, "+15551930402")
	chat, err := s.CreateChat(ctx, sender.ID, "receipt retention", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	old := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "outside window", 1930401)
	oldCopy := chatReadReceiptCopy(t, s, reader.ID, old.FanoutID)
	if _, err := store.StorePool(s).Exec(ctx, `UPDATE messages SET date = statement_timestamp() - interval '604801 seconds' WHERE fanout_id=$1`, old.FanoutID); err != nil {
		t.Fatalf("age old message: %v", err)
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, oldCopy.LocalID); err != nil {
		t.Fatalf("read old message: %v", err)
	}
	assertNoChatReadReceipt(t, s, chat.ID, old.FanoutID, reader.ID)

	current := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "within window", 1930402)
	currentCopy := chatReadReceiptCopy(t, s, reader.ID, current.FanoutID)
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, currentCopy.LocalID); err != nil {
		t.Fatalf("read current message: %v", err)
	}
	recent := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "retained", 1930403)
	recentCopy := chatReadReceiptCopy(t, s, reader.ID, recent.FanoutID)
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, recentCopy.LocalID); err != nil {
		t.Fatalf("read recent message: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `UPDATE chat_read_receipts SET sent_at = statement_timestamp() - interval '604801 seconds' WHERE chat_id=$1 AND fanout_id=$2 AND reader_id=$3`, chat.ID, current.FanoutID, reader.ID); err != nil {
		t.Fatalf("age receipt for retention sweep: %v", err)
	}
	if _, err := store.StorePool(s).Exec(ctx, `
INSERT INTO chat_read_receipts (chat_id, fanout_id, reader_id, sent_at)
SELECT $1, -n, $2, statement_timestamp() - interval '604801 seconds'
FROM generate_series(1, $3::bigint) AS n`, chat.ID, reader.ID, int64(store.ChatReadReceiptSweepBatch+1)); err != nil {
		t.Fatalf("seed expired receipt backlog: %v", err)
	}

	deleted, err := s.SweepExpiredChatReadReceipts(ctx)
	if err != nil {
		t.Fatalf("sweep expired receipts: %v", err)
	}
	wantDeleted := int64(store.ChatReadReceiptSweepBatch + 2) // one aged read and more than one bounded batch
	if deleted != wantDeleted {
		t.Fatalf("sweep deleted %d expired receipts, want %d", deleted, wantDeleted)
	}
	assertNoChatReadReceipt(t, s, chat.ID, current.FanoutID, reader.ID)
	if got := chatReadReceiptCount(t, s, chat.ID, reader.ID); got != 1 {
		t.Fatalf("receipt count after sweep = %d, want recent receipt only", got)
	}
	if got := readChatReceipt(t, s, chat.ID, recent.FanoutID, reader.ID); got.fanoutID != recent.FanoutID {
		t.Fatalf("recent receipt after sweep = %+v, want it retained", got)
	}
}

func TestChatReadReceiptAndReadAdvanceRollBackTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := open(t)
	reader := mustUser(t, s, "+15551930301")
	sender := mustUser(t, s, "+15551930302")
	chat, err := s.CreateChat(ctx, sender.ID, "rollback receipts", []int64{reader.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	message := sendChatReceiptTestMessage(t, s, chat.ID, sender.ID, "rollback", 1930301)
	readerCopy := chatReadReceiptCopy(t, s, reader.ID, message.FanoutID)
	before, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state before forced failure: %v", err)
	}
	pool := store.StorePool(s)
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION reject_chat_read_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.type = 4 THEN
        RAISE EXCEPTION 'forced chat read event failure' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER reject_chat_read_event
BEFORE INSERT ON message_events
FOR EACH ROW EXECUTE FUNCTION reject_chat_read_event();`); err != nil {
		t.Fatalf("install forced event failure: %v", err)
	}

	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerCopy.LocalID); err == nil {
		t.Fatal("read succeeded despite forced event failure")
	}
	if got := chatReadReceiptCount(t, s, chat.ID, reader.ID); got != 0 {
		t.Fatalf("receipts after rollback = %d, want 0", got)
	}
	var inboxMax int64
	var unread int32
	if err := pool.QueryRow(ctx, `SELECT read_inbox_max_id, unread_count FROM dialogs WHERE owner_id=$1 AND peer_type=$2 AND peer_id=$3`, reader.ID, int16(store.PeerTypeChat), chat.ID).Scan(&inboxMax, &unread); err != nil {
		t.Fatalf("read marker after rollback: %v", err)
	}
	if inboxMax != 0 || unread != 1 {
		t.Fatalf("read state after rollback = inbox %d unread %d, want 0/1", inboxMax, unread)
	}
	after, err := s.State(ctx, reader.ID)
	if err != nil {
		t.Fatalf("reader state after forced failure: %v", err)
	}
	if after != before {
		t.Fatalf("reader state after rollback = %+v, want unchanged %+v", after, before)
	}
	events, err := s.EventsSince(ctx, reader.ID, before.Pts)
	if err != nil {
		t.Fatalf("events after rollback: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events after rollback = %+v, want none", events)
	}
}

func sendChatReceiptTestMessage(t *testing.T, s *store.Store, chatID, senderID int64, message string, randomID int64) store.Message {
	t.Helper()
	stored, _, duplicate, err := s.SendChatMessage(context.Background(), store.FanOut{
		ChatID: chatID, FromID: senderID, Text: message, RandomID: randomID,
	})
	if err != nil || duplicate {
		t.Fatalf("send chat message %q: duplicate=%v err=%v", message, duplicate, err)
	}
	return stored
}

func chatReadReceiptCopy(t *testing.T, s *store.Store, ownerID, fanoutID int64) store.Message {
	t.Helper()
	var localID int64
	if err := store.StorePool(s).QueryRow(context.Background(), `SELECT local_id FROM messages WHERE owner_id=$1 AND fanout_id=$2 AND out=false AND deleted=false`, ownerID, fanoutID).Scan(&localID); err != nil {
		t.Fatalf("find inbound copy for owner %d/fanout %d: %v", ownerID, fanoutID, err)
	}
	message, ok, err := s.MessageByOwnerLocal(context.Background(), ownerID, localID)
	if err != nil || !ok {
		t.Fatalf("load inbound copy owner %d/local %d: ok=%v err=%v", ownerID, localID, ok, err)
	}
	return message
}

func ageChatReceiptMessage(t *testing.T, s *store.Store, fanoutID int64) time.Time {
	t.Helper()
	pool := store.StorePool(s)
	if _, err := pool.Exec(context.Background(), `UPDATE messages SET date = statement_timestamp() - interval '60 seconds' WHERE fanout_id=$1`, fanoutID); err != nil {
		t.Fatalf("age fanout %d message: %v", fanoutID, err)
	}
	var sentAt time.Time
	if err := pool.QueryRow(context.Background(), `SELECT date FROM messages WHERE fanout_id=$1 AND out=false`, fanoutID).Scan(&sentAt); err != nil {
		t.Fatalf("read fanout %d send time: %v", fanoutID, err)
	}
	return sentAt
}

func databaseClock(t *testing.T, s *store.Store) time.Time {
	t.Helper()
	var now time.Time
	if err := store.StorePool(s).QueryRow(context.Background(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatalf("read database clock: %v", err)
	}
	return now
}

func readChatReceipt(t *testing.T, s *store.Store, chatID, fanoutID, readerID int64) chatReadReceipt {
	t.Helper()
	var receipt chatReadReceipt
	receipt.chatID, receipt.fanoutID, receipt.readerID = chatID, fanoutID, readerID
	if err := store.StorePool(s).QueryRow(context.Background(), `SELECT sent_at, read_at FROM chat_read_receipts WHERE chat_id=$1 AND fanout_id=$2 AND reader_id=$3`, chatID, fanoutID, readerID).Scan(&receipt.sentAt, &receipt.readAt); err != nil {
		t.Fatalf("read chat receipt (%d,%d,%d): %v", chatID, fanoutID, readerID, err)
	}
	return receipt
}

func assertChatReadReceipt(t *testing.T, receipt chatReadReceipt, sentAt, before, after time.Time) {
	t.Helper()
	if receipt.chatID == 0 || receipt.fanoutID == 0 || receipt.readerID == 0 {
		t.Fatalf("receipt identity has zero field: %+v", receipt)
	}
	if !receipt.sentAt.Equal(sentAt) {
		t.Fatalf("receipt send time = %s, want message send time %s", receipt.sentAt, sentAt)
	}
	if receipt.readAt.Before(before) || receipt.readAt.After(after) {
		t.Fatalf("receipt read time %s outside database-clock interval [%s,%s]", receipt.readAt, before, after)
	}
	if !receipt.readAt.After(receipt.sentAt) {
		t.Fatalf("receipt read time %s did not follow message send time %s", receipt.readAt, receipt.sentAt)
	}
}

func assertSameChatReadReceipt(t *testing.T, got, want chatReadReceipt) {
	t.Helper()
	if got.chatID != want.chatID || got.fanoutID != want.fanoutID || got.readerID != want.readerID || !got.sentAt.Equal(want.sentAt) || !got.readAt.Equal(want.readAt) {
		t.Fatalf("receipt = %+v, want unchanged %+v", got, want)
	}
}

func chatReadReceiptCount(t *testing.T, s *store.Store, chatID, readerID int64) int {
	t.Helper()
	var count int
	if err := store.StorePool(s).QueryRow(context.Background(), `SELECT count(*) FROM chat_read_receipts WHERE chat_id=$1 AND reader_id=$2`, chatID, readerID).Scan(&count); err != nil {
		t.Fatalf("count chat receipts: %v", err)
	}
	return count
}

func assertNoChatReadReceipt(t *testing.T, s *store.Store, chatID, fanoutID, readerID int64) {
	t.Helper()
	var exists bool
	if err := store.StorePool(s).QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM chat_read_receipts WHERE chat_id=$1 AND fanout_id=$2 AND reader_id=$3)`, chatID, fanoutID, readerID).Scan(&exists); err != nil {
		t.Fatalf("check absent chat receipt: %v", err)
	}
	if exists {
		t.Fatalf("unexpected chat receipt (%d,%d,%d)", chatID, fanoutID, readerID)
	}
}
