//nolint:testpackage // This test exercises the private send handler and its logger.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func newDirectSendLogHarness(t *testing.T) (context.Context, *store.Store, *handlers, *strings.Builder) {
	t.Helper()
	ctx, s, h, logs, _ := newDirectSendLogHarnessWithDSN(t)
	return ctx, s, h, logs
}

func newDirectSendLogHarnessWithDSN(t *testing.T) (context.Context, *store.Store, *handlers, *strings.Builder, string) {
	t.Helper()
	ctx := context.Background()
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	t.Cleanup(func() {
		if err := blobs.Close(); err != nil {
			t.Errorf("close blob store: %v", err)
		}
	})
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	var logs strings.Builder
	h := testHandlersWithBlobs(s, blobs)
	h.uploadGrantTTL, h.maxUserStorageBytes = time.Hour, TestMaxUserStorageBytes
	h.log = slog.New(slog.NewJSONHandler(&logs, nil))
	return ctx, s, h, &logs, dsn
}

func directSendLogRecords(t *testing.T, logs *strings.Builder) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func TestDemoGeneralChatInvalidResolutionLogsErrorOnce(t *testing.T) {
	for _, target := range []string{"missing", "wrong channel"} {
		t.Run(target, func(t *testing.T) {
			ctx, s, h, logs := newDirectSendLogHarness(t)
			h.demoGeneralChatEnabled = true
			var channelID int64 = 999
			if target == "wrong channel" {
				owner, err := s.CreateUser(ctx, "+15550001701")
				if err != nil {
					t.Fatalf("create owner: %v", err)
				}
				channel, _, _, err := s.CreateChannelWithServiceMessage(ctx, owner.ID, "Not General", "", false)
				if err != nil {
					t.Fatalf("create wrong channel: %v", err)
				}
				if err := s.ClaimChannelUsername(ctx, channel.ID, demoGeneralChatUsername); err != nil {
					t.Fatalf("claim General handle on wrong channel: %v", err)
				}
				channelID = channel.ID
			}
			for range 2 {
				if isGeneral, err := h.isDemoGeneralChat(ctx, channelID); err != nil || isGeneral {
					t.Fatalf("invalid General resolution = %t, err=%v; want false without error", isGeneral, err)
				}
			}
			records := directSendLogRecords(t, logs)
			if len(records) != 1 {
				t.Fatalf("invalid General resolution logged %d records, want one: %+v", len(records), records)
			}
			if records[0]["level"] != "ERROR" {
				t.Fatalf("invalid General resolution level = %v, want ERROR", records[0]["level"])
			}
		})
	}
}

func TestDemoGeneralChatCacheFollowsReclaimedHandle(t *testing.T) {
	ctx, s, h, _ := newDirectSendLogHarness(t)
	h.demoGeneralChatEnabled = true
	now := time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	owner, err := s.CreateUser(ctx, "+15550001702")
	if err != nil {
		t.Fatalf("create first owner: %v", err)
	}
	first, _, _, err := s.CreateChannelWithServiceMessage(ctx, owner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create first General: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, first.ID, demoGeneralChatUsername); err != nil {
		t.Fatalf("claim first General handle: %v", err)
	}
	if isGeneral, err := h.isDemoGeneralChat(ctx, first.ID); err != nil || !isGeneral {
		t.Fatalf("first General resolution = %t, err=%v", isGeneral, err)
	}
	if err := s.EditChannelUsername(ctx, first.ID, owner.ID, "previousgeneral"); err != nil {
		t.Fatalf("move first General handle: %v", err)
	}
	secondOwner, err := s.CreateUser(ctx, "+15550001703")
	if err != nil {
		t.Fatalf("create second owner: %v", err)
	}
	second, _, _, err := s.CreateChannelWithServiceMessage(ctx, secondOwner.ID, "General", "", true)
	if err != nil {
		t.Fatalf("create replacement General: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, second.ID, demoGeneralChatUsername); err != nil {
		t.Fatalf("claim replacement General handle: %v", err)
	}
	if isGeneral, err := h.isDemoGeneralChat(ctx, first.ID); err != nil || !isGeneral {
		t.Fatalf("old General before cache expiry = %t, err=%v; want cached true", isGeneral, err)
	}
	now = now.Add(demoGeneralChatResolutionTTL + time.Nanosecond)
	if isGeneral, err := h.isDemoGeneralChat(ctx, first.ID); err != nil || isGeneral {
		t.Fatalf("old General after cache expiry = %t, err=%v; want false", isGeneral, err)
	}
	guest, err := s.CreateUser(ctx, "+15550001704")
	if err != nil {
		t.Fatalf("create guest: %v", err)
	}
	h.demoGeneralChatPolicy.perMinute = store.RateLimitConfig{Limit: 1, Window: time.Minute}
	r := &mtproto.Request{Ctx: ctx, UserID: guest.ID}
	if isGeneral, err := h.checkDemoGeneralChatMessages(r, second.ID, []string{"first"}, 1, errMessageTooLong); err != nil || !isGeneral {
		t.Fatalf("first replacement General message: %v", err)
	}
	_, err = h.checkDemoGeneralChatMessages(r, second.ID, []string{"second"}, 1, errMessageTooLong)
	if err == nil || !strings.Contains(err.Error(), "FLOOD_WAIT (60)") {
		t.Fatalf("second replacement General message = %v, want FLOOD_WAIT_60", err)
	}
}

func BenchmarkDemoGeneralChatResolutionCacheHit(b *testing.B) {
	ctx := context.Background()
	blobs, err := blob.NewLocal(b.TempDir())
	if err != nil {
		b.Fatalf("open blob store: %v", err)
	}
	b.Cleanup(func() {
		if err := blobs.Close(); err != nil {
			b.Errorf("close blob store: %v", err)
		}
	})
	s, err := store.Open(ctx, pgtest.DSN(b), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() {
		if err := s.Close(); err != nil {
			b.Errorf("close store: %v", err)
		}
	})
	owner, err := s.CreateUser(ctx, "+15550001705")
	if err != nil {
		b.Fatalf("create owner: %v", err)
	}
	channel, _, _, err := s.CreateChannelWithServiceMessage(ctx, owner.ID, "General", "", true)
	if err != nil {
		b.Fatalf("create General: %v", err)
	}
	if err := s.ClaimChannelUsername(ctx, channel.ID, demoGeneralChatUsername); err != nil {
		b.Fatalf("claim General handle: %v", err)
	}
	h := testHandlers(s)
	h.demoGeneralChatEnabled = true
	if isGeneral, err := h.isDemoGeneralChat(ctx, channel.ID); err != nil || !isGeneral {
		b.Fatalf("warm General resolution = %t, err=%v", isGeneral, err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if isGeneral, err := h.isDemoGeneralChat(ctx, channel.ID); err != nil || !isGeneral {
			b.Fatalf("cached General resolution = %t, err=%v", isGeneral, err)
		}
	}
}

func TestDemoGeneralChatLookupFailureIsCachedForRefreshWindow(t *testing.T) {
	now := time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
	var lookups atomic.Int32
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	h := &handlers{
		now: func() time.Time { return now },
		demoGeneralChatLookup: func(context.Context, string) (store.UsernameResolution, bool, error) {
			if lookups.Add(1) == 1 {
				close(lookupStarted)
			}
			<-releaseLookup
			return store.UsernameResolution{}, false, errors.New("injected username lookup failure")
		},
	}
	h.demoGeneralChatTarget.Store(&demoGeneralChatCache{channelID: 77, expiresAt: now.Add(-time.Nanosecond)})

	const concurrentCalls = 16
	var wg sync.WaitGroup
	started := make(chan struct{}, concurrentCalls)
	for range concurrentCalls {
		wg.Go(func() {
			started <- struct{}{}
			if _, err := h.isDemoGeneralChat(context.Background(), 77); err == nil {
				t.Error("expired cache lookup returned no error")
			}
		})
	}
	for range concurrentCalls {
		<-started
	}
	<-lookupStarted
	close(releaseLookup)
	wg.Wait()
	if got := lookups.Load(); got != 1 {
		t.Fatalf("concurrent expired-cache calls performed %d lookups, want one", got)
	}
	if _, err := h.isDemoGeneralChat(context.Background(), 77); err == nil {
		t.Fatal("cached lookup failure returned no error")
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("call within failed-refresh window performed %d lookups, want one total", got)
	}
	now = now.Add(demoGeneralChatLookupFailureTTL + time.Nanosecond)
	if _, err := h.isDemoGeneralChat(context.Background(), 77); err == nil {
		t.Fatal("lookup after failure-cache expiry returned no error")
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("call after failed-refresh window performed %d lookups, want two total", got)
	}
}

func TestDemoGeneralChatLookupSurvivesCallerCancellation(t *testing.T) {
	now := time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
	lookupStarted := make(chan struct{})
	releaseLookup := make(chan struct{})
	var lookups atomic.Int32
	username := demoGeneralChatUsername
	h := &handlers{
		now: func() time.Time { return now },
		demoGeneralChatLookup: func(ctx context.Context, _ string) (store.UsernameResolution, bool, error) {
			if lookups.Add(1) == 1 {
				close(lookupStarted)
			}
			if _, ok := ctx.Deadline(); !ok {
				return store.UsernameResolution{}, false, errors.New("lookup has no refresh deadline")
			}
			select {
			case <-ctx.Done():
				return store.UsernameResolution{}, false, ctx.Err()
			case <-releaseLookup:
				if err := ctx.Err(); err != nil {
					return store.UsernameResolution{}, false, err
				}
				return store.UsernameResolution{
					Kind: store.UsernameKindChannel,
					Channel: store.Channel{
						ID: 77, Title: "General", Username: &username, Megagroup: true,
					},
				}, true, nil
			}
		},
	}
	h.demoGeneralChatTarget.Store(&demoGeneralChatCache{channelID: 11, expiresAt: now.Add(-time.Nanosecond)})

	callerCtx, cancel := context.WithCancel(context.Background())
	type resolution struct {
		isGeneral bool
		err       error
	}
	firstResult := make(chan resolution, 1)
	go func() {
		isGeneral, err := h.isDemoGeneralChat(callerCtx, 77)
		firstResult <- resolution{isGeneral: isGeneral, err: err}
	}()
	<-lookupStarted
	cancel()

	secondStarted := make(chan struct{})
	secondResult := make(chan resolution, 1)
	go func() {
		close(secondStarted)
		isGeneral, err := h.isDemoGeneralChat(context.Background(), 88)
		secondResult <- resolution{isGeneral: isGeneral, err: err}
	}()
	<-secondStarted
	close(releaseLookup)
	first := <-firstResult
	second := <-secondResult
	if first.err != nil || !first.isGeneral {
		t.Fatalf("cancelled first caller resolution = %t, err=%v; want shared General refresh", first.isGeneral, first.err)
	}
	if second.err != nil || second.isGeneral {
		t.Fatalf("live non-General caller resolution = %t, err=%v; want false without error", second.isGeneral, second.err)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("cancelled refresh and live caller performed %d lookups, want one", got)
	}
}

func TestDemoGeneralChatContextLookupTimeoutIsSharedAndCached(t *testing.T) {
	now := time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
	var lookups atomic.Int32
	lookupStarted := make(chan struct{})
	h := &handlers{
		now: func() time.Time { return now },
		demoGeneralChatLookup: func(ctx context.Context, _ string) (store.UsernameResolution, bool, error) {
			if lookups.Add(1) == 1 {
				close(lookupStarted)
			}
			<-ctx.Done()
			return store.UsernameResolution{}, false, ctx.Err()
		},
	}
	h.demoGeneralChatTarget.Store(&demoGeneralChatCache{channelID: 11, expiresAt: now.Add(-time.Nanosecond)})
	const concurrentCalls = 8
	started := make(chan struct{}, concurrentCalls)
	errorsByCall := make(chan error, concurrentCalls)
	var wg sync.WaitGroup
	start := time.Now()
	for range concurrentCalls {
		wg.Go(func() {
			started <- struct{}{}
			_, err := h.isDemoGeneralChat(context.Background(), 77)
			errorsByCall <- err
		})
	}
	for range concurrentCalls {
		<-started
	}
	<-lookupStarted
	wg.Wait()
	elapsed := time.Since(start)
	for range concurrentCalls {
		if err := <-errorsByCall; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("concurrent lookup error = %v, want context deadline exceeded", err)
		}
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("8 concurrent callers performed %d timed-out lookups, want one", got)
	}
	if elapsed < 2*time.Second || elapsed > demoGeneralChatLookupTimeout+2*time.Second {
		t.Fatalf("8 concurrent calls took %s, want one %s refresh", elapsed, demoGeneralChatLookupTimeout)
	}
	if _, err := h.isDemoGeneralChat(context.Background(), 77); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lookup within cached failure window = %v, want context deadline exceeded", err)
	}
	if got := lookups.Load(); got != 1 {
		t.Fatalf("call within cached failure window performed %d lookups, want one total", got)
	}
}

func uploadDirectSendLogAlbumDocument(t *testing.T, ctx context.Context, s *store.Store, h *handlers, userID, fileID int64) *tg.InputMediaDocument {
	t.Helper()
	if err := s.SaveUploadPart(ctx, userID, fileID, 0, []byte("private album bytes"), TestMaxFileBytes); err != nil {
		t.Fatalf("save album upload: %v", err)
	}
	var body bin.Buffer
	if err := (&tg.MessagesUploadMediaRequest{
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: fileID, Parts: 1, Name: "album.bin"},
			MimeType: "application/octet-stream",
		},
	}).Encode(&body); err != nil {
		t.Fatalf("encode uploadMedia: %v", err)
	}
	media, err := h.handleUploadMedia(&mtproto.Request{Ctx: ctx, UserID: userID, Buf: &body})
	if err != nil {
		t.Fatalf("uploadMedia: %v", err)
	}
	documentMedia, ok := media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("uploadMedia result = %T, want MessageMediaDocument", media)
	}
	document, ok := documentMedia.Document.(*tg.Document)
	if !ok {
		t.Fatalf("uploadMedia document = %T, want Document", documentMedia.Document)
	}
	return &tg.InputMediaDocument{ID: &tg.InputDocument{ID: document.ID, AccessHash: document.AccessHash}}
}

func TestSendMessageLogsDirectRecipientOutcome(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithoutBlobStore())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	var logs strings.Builder
	h := testHandlers(s)
	h.log = slog.New(slog.NewJSONHandler(&logs, nil))

	for _, tc := range []struct {
		name           string
		blocker        bool
		senderPhone    string
		recipientPhone string
		wantCopy       string
		wantReason     string
		wantPts        int64
	}{
		{name: "stored", senderPhone: "+15551232731", recipientPhone: "+15551232732", wantCopy: "stored", wantPts: 1},
		{name: "withheld", blocker: true, senderPhone: "+15551232733", recipientPhone: "+15551232734", wantCopy: "withheld", wantReason: "recipient_blocked_sender"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs.Reset()
			sender, err := s.CreateUser(ctx, tc.senderPhone)
			if err != nil {
				t.Fatalf("create sender: %v", err)
			}
			recipient, err := s.CreateUser(ctx, tc.recipientPhone)
			if err != nil {
				t.Fatalf("create recipient: %v", err)
			}
			if tc.blocker {
				if _, err := s.BlockUser(ctx, recipient.ID, sender.ID); err != nil {
					t.Fatalf("block sender: %v", err)
				}
			}

			const messageText = "private message body must not appear in logs"
			const randomID = 3273001
			var body bin.Buffer
			if err := (&tg.MessagesSendMessageRequest{
				Peer:     InputPeerUser(sender.ID, recipient.ID),
				Message:  messageText,
				RandomID: randomID,
			}).Encode(&body); err != nil {
				t.Fatalf("encode sendMessage: %v", err)
			}
			if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{
				Ctx: ctx, UserID: sender.ID, Buf: &body,
			}); err != nil {
				t.Fatalf("sendMessage: %v", err)
			}

			message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
			if err != nil || !ok {
				t.Fatalf("sender message: found=%v err=%v", ok, err)
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &record); err != nil {
				t.Fatalf("decode log line %q: %v", logs.String(), err)
			}
			if got := record["msg"]; got != "direct message outcome" {
				t.Errorf("log message = %v, want direct message outcome", got)
			}
			if got := record["level"]; got != "INFO" {
				t.Errorf("log level = %v, want INFO", got)
			}
			for key, want := range map[string]any{
				"sender_id":      float64(sender.ID),
				"recipient_id":   float64(recipient.ID),
				"message_id":     float64(message.LocalID),
				"recipient_copy": tc.wantCopy,
				"recipient_pts":  float64(tc.wantPts),
			} {
				if got := record[key]; got != want {
					t.Errorf("%s = %v, want %v", key, got, want)
				}
			}
			if tc.wantReason != "" {
				if got := record["recipient_copy_reason"]; got != tc.wantReason {
					t.Errorf("recipient_copy_reason = %v, want %q", got, tc.wantReason)
				}
			} else if _, ok := record["recipient_copy_reason"]; ok {
				t.Errorf("stored recipient copy unexpectedly has reason %v", record["recipient_copy_reason"])
			}
			if strings.Contains(logs.String(), messageText) {
				t.Errorf("log exposed message text: %q", logs.String())
			}
			if got := strings.Count(logs.String(), "\n"); got != 1 {
				t.Errorf("log lines = %d, want one: %q", got, logs.String())
			}

			logs.Reset()
			var retryBody bin.Buffer
			if err := (&tg.MessagesSendMessageRequest{
				Peer:     InputPeerUser(sender.ID, recipient.ID),
				Message:  messageText,
				RandomID: randomID,
			}).Encode(&retryBody); err != nil {
				t.Fatalf("encode retry sendMessage: %v", err)
			}
			if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{
				Ctx: ctx, UserID: sender.ID, Buf: &retryBody,
			}); err != nil {
				t.Fatalf("retry sendMessage: %v", err)
			}
			var retryRecord map[string]any
			if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &retryRecord); err != nil {
				t.Fatalf("decode retry log line %q: %v", logs.String(), err)
			}
			if got := retryRecord["level"]; got != "INFO" {
				t.Errorf("retry log level = %v, want INFO", got)
			}
			for key, want := range map[string]any{
				"message_id":     float64(message.LocalID),
				"recipient_copy": tc.wantCopy,
				"recipient_pts":  float64(0),
				"duplicate":      true,
			} {
				if got := retryRecord[key]; got != want {
					t.Errorf("retry %s = %v, want %v", key, got, want)
				}
			}
			if tc.wantReason != "" && retryRecord["recipient_copy_reason"] != tc.wantReason {
				t.Errorf("retry recipient_copy_reason = %v, want %q", retryRecord["recipient_copy_reason"], tc.wantReason)
			}
			if got := strings.Count(logs.String(), "\n"); got != 1 {
				t.Errorf("retry log lines = %d, want one: %q", got, logs.String())
			}
		})
	}
}

func TestSendMessageRetryLogsStoredPeerAcrossRecipients(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232770")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	storedRecipient, err := s.CreateUser(ctx, "+15551232771")
	if err != nil {
		t.Fatalf("create stored recipient: %v", err)
	}
	requestedRecipient, err := s.CreateUser(ctx, "+15551232772")
	if err != nil {
		t.Fatalf("create requested recipient: %v", err)
	}
	if _, err := s.BlockUser(ctx, requestedRecipient.ID, sender.ID); err != nil {
		t.Fatalf("block sender at requested recipient: %v", err)
	}
	const (
		randomID = int64(3273170)
		bodyText = "private text owned by the stored peer"
	)
	var seed bin.Buffer
	if err := (&tg.MessagesSendMessageRequest{
		Peer: InputPeerUser(sender.ID, storedRecipient.ID), Message: bodyText, RandomID: randomID,
	}).Encode(&seed); err != nil {
		t.Fatalf("encode seed sendMessage: %v", err)
	}
	if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &seed}); err != nil {
		t.Fatalf("seed sendMessage: %v", err)
	}
	stored, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok {
		t.Fatalf("stored message: found=%v err=%v", ok, err)
	}
	logs.Reset()

	var retry bin.Buffer
	if err := (&tg.MessagesSendMessageRequest{
		Peer: InputPeerUser(sender.ID, requestedRecipient.ID), Message: "private retry body", RandomID: randomID,
	}).Encode(&retry); err != nil {
		t.Fatalf("encode retry sendMessage: %v", err)
	}
	if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retry}); err != nil {
		t.Fatalf("retry sendMessage: %v", err)
	}
	records := directOutcomeRecords(t, directSendLogRecords(t, logs))
	if len(records) != 1 {
		t.Fatalf("direct retry outcome records = %d, want one: %q", len(records), logs.String())
	}
	for key, want := range map[string]any{
		"msg": "direct message outcome", "level": "INFO",
		"sender_id": float64(sender.ID), "recipient_id": float64(stored.PeerID),
		"message_id": float64(stored.LocalID), "recipient_copy": "stored",
		"recipient_pts": float64(0), "duplicate": true,
	} {
		if got := records[0][key]; got != want {
			t.Errorf("retry outcome %s = %v, want %v", key, got, want)
		}
	}
	if strings.Contains(logs.String(), bodyText) || strings.Contains(logs.String(), "private retry body") {
		t.Errorf("retry log exposed message text: %q", logs.String())
	}
}

func TestConcurrentSendMessageDuplicateLogsStoredPeer(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs, dsn := newDirectSendLogHarnessWithDSN(t)
	sender, err := s.CreateUser(ctx, "+15551232773")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	firstRecipient, err := s.CreateUser(ctx, "+15551232774")
	if err != nil {
		t.Fatalf("create first recipient: %v", err)
	}
	secondRecipient, err := s.CreateUser(ctx, "+15551232775")
	if err != nil {
		t.Fatalf("create second recipient: %v", err)
	}
	witness, err := s.CreateUser(ctx, "+15551232776")
	if err != nil {
		t.Fatalf("create witness: %v", err)
	}
	for i, recipient := range []store.User{firstRecipient, secondRecipient} {
		if _, _, _, _, err := s.SendMessage(ctx, witness.ID, recipient.ID, "existing recipient activity", int64(3273171+i), 0, 0); err != nil {
			t.Fatalf("seed recipient state for %d: %v", recipient.ID, err)
		}
		if _, err := s.BlockUser(ctx, recipient.ID, sender.ID); err != nil {
			t.Fatalf("block sender at recipient %d: %v", recipient.ID, err)
		}
	}

	lockConn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect advisory lock holder: %v", err)
	}
	t.Cleanup(func() {
		if err := lockConn.Close(ctx); err != nil {
			t.Errorf("close advisory lock holder: %v", err)
		}
	})
	ownerIDs := []int64{sender.ID, firstRecipient.ID, secondRecipient.ID}
	slices.Sort(ownerIDs)
	lockedOwners := make([]int64, 0, len(ownerIDs))
	releaseOwnerLocks := func() {
		for _, ownerID := range slices.Backward(lockedOwners) {
			var released bool
			if err := lockConn.QueryRow(ctx, `SELECT pg_advisory_unlock($1)`, ownerID).Scan(&released); err != nil {
				t.Errorf("release owner lock %d: %v", ownerID, err)
				continue
			}
			if !released {
				t.Errorf("owner lock %d was not held", ownerID)
			}
		}
		lockedOwners = nil
	}
	defer releaseOwnerLocks()
	for _, ownerID := range ownerIDs {
		if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, ownerID); err != nil {
			t.Fatalf("hold owner lock %d: %v", ownerID, err)
		}
		lockedOwners = append(lockedOwners, ownerID)
	}

	const randomID = int64(3273173)
	results := make(chan error, 2)
	for _, recipient := range []store.User{firstRecipient, secondRecipient} {
		go func() {
			var body bin.Buffer
			if err := (&tg.MessagesSendMessageRequest{
				Peer: InputPeerUser(sender.ID, recipient.ID), Message: "private concurrent send", RandomID: randomID,
			}).Encode(&body); err != nil {
				results <- err
				return
			}
			_, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body})
			results <- err
		}()
	}
	waitForDirectSendLockWaiters(t, ctx, lockConn, 2)
	releaseOwnerLocks()
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("concurrent sendMessage: %v; logs: %s", err, logs.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for concurrent sendMessage calls")
		}
	}

	stored, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok || stored.PeerType != store.PeerTypeUser || stored.PeerLocalID != 0 {
		t.Fatalf("stored withheld message: found=%v peer_type=%v peer_local_id=%d err=%v", ok, stored.PeerType, stored.PeerLocalID, err)
	}
	records := directOutcomeRecords(t, directSendLogRecords(t, logs))
	if len(records) != 2 {
		t.Fatalf("direct outcome records = %d, want two: %q", len(records), logs.String())
	}
	duplicates := 0
	for _, record := range records {
		for key, want := range map[string]any{
			"msg": "direct message outcome", "level": "INFO",
			"sender_id": float64(sender.ID), "recipient_id": float64(stored.PeerID),
			"message_id": float64(stored.LocalID), "recipient_copy": "withheld",
			"recipient_pts": float64(0), "recipient_copy_reason": "recipient_blocked_sender",
		} {
			if got := record[key]; got != want {
				t.Errorf("outcome %s = %v, want %v", key, got, want)
			}
		}
		if duplicate, ok := record["duplicate"].(bool); ok && duplicate {
			duplicates++
		} else if record["duplicate"] != false {
			t.Errorf("duplicate = %v, want boolean", record["duplicate"])
		}
		if strings.Contains(fmt.Sprint(record), "private concurrent send") {
			t.Errorf("outcome log exposed message text: %#v", record)
		}
	}
	if duplicates != 1 {
		t.Errorf("duplicate outcomes = %d, want one: %#v", duplicates, records)
	}
}

func waitForDirectSendLockWaiters(t *testing.T, ctx context.Context, conn *pgx.Conn, want int) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiters int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock'`).Scan(&waiters); err != nil {
			t.Fatalf("count direct send lock waiters: %v", err)
		}
		if waiters >= want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d direct send lock waiters; saw %d", want, waiters)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("waiting for direct send lock waiters: %v", ctx.Err())
		}
	}
}

func TestSendMediaLogsDirectRecipientOutcome(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232741")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551232742")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	const (
		fileID   = int64(3273141)
		randomID = int64(3273142)
		caption  = "private media caption must not be logged"
	)
	if err := s.SaveUploadPart(ctx, sender.ID, fileID, 0, []byte("opaque media bytes"), TestMaxFileBytes); err != nil {
		t.Fatalf("save media upload: %v", err)
	}
	var body bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, recipient.ID),
		Message: caption,
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: fileID, Parts: 1, Name: "private.bin"},
			MimeType: "application/octet-stream",
		},
		RandomID: randomID,
	}).Encode(&body); err != nil {
		t.Fatalf("encode sendMedia: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
		t.Fatalf("sendMedia: %v", err)
	}
	message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok {
		t.Fatalf("sender media message: found=%v err=%v", ok, err)
	}
	assertDirectSendLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, 1, true, caption)

	logs.Reset()
	var retryBody bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, recipient.ID),
		Message: caption,
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: fileID, Parts: 1, Name: "private.bin"},
			MimeType: "application/octet-stream",
		},
		RandomID: randomID,
	}).Encode(&retryBody); err != nil {
		t.Fatalf("encode sendMedia retry: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retryBody}); err != nil {
		t.Fatalf("retry sendMedia: %v", err)
	}
	assertDirectSendReplayLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, 0, true, caption)
}

func TestSendMediaReplayLogsStoredPeer(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232760")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	storedRecipient, err := s.CreateUser(ctx, "+15551232761")
	if err != nil {
		t.Fatalf("create stored recipient: %v", err)
	}
	requestedRecipient, err := s.CreateUser(ctx, "+15551232762")
	if err != nil {
		t.Fatalf("create requested recipient: %v", err)
	}
	const (
		randomID = int64(3273160)
		bodyText = "private message that owns this retry token"
	)
	var seed bin.Buffer
	if err := (&tg.MessagesSendMessageRequest{
		Peer: InputPeerUser(sender.ID, storedRecipient.ID), Message: bodyText, RandomID: randomID,
	}).Encode(&seed); err != nil {
		t.Fatalf("encode seed sendMessage: %v", err)
	}
	if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &seed}); err != nil {
		t.Fatalf("seed sendMessage: %v", err)
	}
	stored, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok {
		t.Fatalf("stored message: found=%v err=%v", ok, err)
	}
	logs.Reset()

	var retry bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, requestedRecipient.ID),
		Message: "private retry caption",
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: 3273161, Parts: 1, Name: "retry.bin"},
			MimeType: "application/octet-stream",
		},
		RandomID: randomID,
	}).Encode(&retry); err != nil {
		t.Fatalf("encode sendMedia retry: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retry}); err != nil {
		t.Fatalf("sendMedia retry: %v", err)
	}
	assertDirectSendReplayLog(t, directOutcomeRecords(t, directSendLogRecords(t, logs)), sender.ID, stored.PeerID, stored.LocalID, 0, true, "private retry caption")
}

func TestSendMediaReplayDoesNotLabelGroupAsUser(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232763")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551232764")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, sender.ID, "retry group", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	const (
		randomID = int64(3273162)
		bodyText = "private group message"
	)
	var seed bin.Buffer
	if err := (&tg.MessagesSendMessageRequest{
		Peer: InputPeerChat(sender.ID, chat.ID), Message: bodyText, RandomID: randomID,
	}).Encode(&seed); err != nil {
		t.Fatalf("encode group sendMessage: %v", err)
	}
	if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &seed}); err != nil {
		t.Fatalf("seed group sendMessage: %v", err)
	}
	stored, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok || stored.PeerType != store.PeerTypeChat {
		t.Fatalf("stored group message: found=%v peer_type=%v err=%v", ok, stored.PeerType, err)
	}
	logs.Reset()

	var retry bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, member.ID),
		Message: "private retry caption",
		Media: &tg.InputMediaUploadedDocument{
			File:     &tg.InputFile{ID: 3273163, Parts: 1, Name: "retry.bin"},
			MimeType: "application/octet-stream",
		},
		RandomID: randomID,
	}).Encode(&retry); err != nil {
		t.Fatalf("encode sendMedia retry: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retry}); err != nil {
		t.Fatalf("sendMedia retry: %v", err)
	}
	if records := directOutcomeRecords(t, directSendLogRecords(t, logs)); len(records) != 0 {
		t.Errorf("group replay produced private-message outcomes: %#v", records)
	}
}

func TestSendCatalogueStickerLogsDirectRecipientOutcome(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232750")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551232751")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	catalog, err := h.stickerSetCatalog()
	if err != nil {
		t.Fatalf("load sticker catalogue: %v", err)
	}
	set := catalog.byShortName[twemojiReactionSetShortName]
	if set == nil || len(set.documents) == 0 {
		t.Fatal("built-in sticker catalogue has no documents")
	}
	document := set.documents[0]
	const (
		randomID = int64(3273153)
		caption  = "private sticker caption must not be logged"
	)
	var body bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, recipient.ID),
		Message: caption,
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		RandomID: randomID,
	}).Encode(&body); err != nil {
		t.Fatalf("encode sticker sendMedia: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
		t.Fatalf("send sticker: %v", err)
	}
	message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok {
		t.Fatalf("sender sticker message: found=%v err=%v", ok, err)
	}
	pts, err := s.MessagePts(ctx, recipient.ID, message.PeerLocalID)
	if err != nil {
		t.Fatalf("recipient sticker pts: %v", err)
	}
	assertDirectSendLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, pts, true, caption)

	logs.Reset()
	var retryBody bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer:    InputPeerUser(sender.ID, recipient.ID),
		Message: caption,
		Media: &tg.InputMediaDocument{ID: &tg.InputDocument{
			ID: document.ID, AccessHash: document.AccessHash,
		}},
		RandomID: randomID,
	}).Encode(&retryBody); err != nil {
		t.Fatalf("encode sticker retry: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retryBody}); err != nil {
		t.Fatalf("retry sticker: %v", err)
	}
	assertDirectSendReplayLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, 0, true, caption)
}

func TestSendPollMediaLogsDirectRecipientOutcome(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232748")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551232749")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	const (
		randomID = int64(3273152)
		question = "private poll question must not be logged"
	)
	var body bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer: InputPeerUser(sender.ID, recipient.ID),
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: question},
			Answers: []tg.PollAnswerClass{
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "first"}, Option: []byte("first")},
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "second"}, Option: []byte("second")},
			},
		}},
		RandomID: randomID,
	}).Encode(&body); err != nil {
		t.Fatalf("encode poll sendMedia: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
		t.Fatalf("send poll: %v", err)
	}
	message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
	if err != nil || !ok {
		t.Fatalf("sender poll message: found=%v err=%v", ok, err)
	}
	pts, err := s.MessagePts(ctx, recipient.ID, message.PeerLocalID)
	if err != nil {
		t.Fatalf("recipient poll pts: %v", err)
	}
	assertDirectSendLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, pts, true, question)

	logs.Reset()
	var retryBody bin.Buffer
	if err := (&tg.MessagesSendMediaRequest{
		Peer: InputPeerUser(sender.ID, recipient.ID),
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: question},
			Answers: []tg.PollAnswerClass{
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "first"}, Option: []byte("first")},
				&tg.PollAnswer{Text: tg.TextWithEntities{Text: "second"}, Option: []byte("second")},
			},
		}},
		RandomID: randomID,
	}).Encode(&retryBody); err != nil {
		t.Fatalf("encode poll retry: %v", err)
	}
	if _, _, _, err := h.handleSendMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retryBody}); err != nil {
		t.Fatalf("retry poll: %v", err)
	}
	assertDirectSendReplayLog(t, directSendLogRecords(t, logs), sender.ID, recipient.ID, message.LocalID, 0, true, question)
}

func TestSendMultiMediaLogsEachDirectAlbumOutcome(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232743")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551232744")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	firstMedia := uploadDirectSendLogAlbumDocument(t, ctx, s, h, sender.ID, 3273143)
	secondMedia := uploadDirectSendLogAlbumDocument(t, ctx, s, h, sender.ID, 3273144)
	const (
		firstRandomID  = int64(3273145)
		secondRandomID = int64(3273146)
		caption        = "private album caption must not be logged"
	)
	req := &tg.MessagesSendMultiMediaRequest{
		Peer: InputPeerUser(sender.ID, recipient.ID),
		MultiMedia: []tg.InputSingleMedia{
			{Media: firstMedia, Message: caption, RandomID: firstRandomID},
			{Media: secondMedia, Message: caption, RandomID: secondRandomID},
		},
	}
	var body bin.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatalf("encode sendMultiMedia: %v", err)
	}
	if _, _, _, err := h.handleSendMultiMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
		t.Fatalf("sendMultiMedia: %v", err)
	}
	first, ok, err := s.MessageByRandomID(ctx, sender.ID, firstRandomID)
	if err != nil || !ok {
		t.Fatalf("first sender album message: found=%v err=%v", ok, err)
	}
	second, ok, err := s.MessageByRandomID(ctx, sender.ID, secondRandomID)
	if err != nil || !ok {
		t.Fatalf("second sender album message: found=%v err=%v", ok, err)
	}
	records := directSendLogRecords(t, logs)
	if len(records) != 2 {
		t.Fatalf("direct album log lines = %d, want 2: %q", len(records), logs.String())
	}
	for _, message := range []store.Message{first, second} {
		pts, err := s.MessagePts(ctx, recipient.ID, message.PeerLocalID)
		if err != nil {
			t.Fatalf("recipient album pts: %v", err)
		}
		assertDirectSendLog(t, records, sender.ID, recipient.ID, message.LocalID, pts, true, caption)
	}

	logs.Reset()
	var retryBody bin.Buffer
	if err := req.Encode(&retryBody); err != nil {
		t.Fatalf("encode album retry: %v", err)
	}
	if _, _, _, err := h.handleSendMultiMediaAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retryBody}); err != nil {
		t.Fatalf("retry sendMultiMedia: %v", err)
	}
	replayRecords := directSendLogRecords(t, logs)
	if len(replayRecords) != 2 {
		t.Fatalf("direct album replay log lines = %d, want 2: %q", len(replayRecords), logs.String())
	}
	for _, message := range []store.Message{first, second} {
		assertDirectSendReplayLog(t, replayRecords, sender.ID, recipient.ID, message.LocalID, 0, true, caption)
	}
}

func TestForwardMessagesLogsEachDirectOutcome(t *testing.T) {
	t.Parallel()
	ctx, s, h, logs := newDirectSendLogHarness(t)
	sender, err := s.CreateUser(ctx, "+15551232745")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	sourcePeer, err := s.CreateUser(ctx, "+15551232746")
	if err != nil {
		t.Fatalf("create source peer: %v", err)
	}
	recipient, err := s.CreateUser(ctx, "+15551232747")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	sourceIDs := []int{3273147, 3273148}
	for i, randomID := range []int64{3273148, 3273149} {
		var body bin.Buffer
		if err := (&tg.MessagesSendMessageRequest{
			Peer: InputPeerUser(sender.ID, sourcePeer.ID), Message: "forward source text", RandomID: randomID,
		}).Encode(&body); err != nil {
			t.Fatalf("encode source send: %v", err)
		}
		if _, _, _, err := h.handleSendMessageAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
			t.Fatalf("send source message %d: %v", i, err)
		}
		message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
		if err != nil || !ok {
			t.Fatalf("source message %d: found=%v err=%v", i, ok, err)
		}
		sourceIDs[i] = int(message.LocalID)
	}
	logs.Reset()
	const caption = "forward source text"
	forwardRandomIDs := []int64{3273150, 3273151}
	req := &tg.MessagesForwardMessagesRequest{
		ToPeer: InputPeerUser(sender.ID, recipient.ID), FromPeer: InputPeerUser(sender.ID, sourcePeer.ID),
		ID: sourceIDs, RandomID: forwardRandomIDs,
	}
	var body bin.Buffer
	if err := req.Encode(&body); err != nil {
		t.Fatalf("encode forwardMessages: %v", err)
	}
	if _, _, _, err := h.handleForwardMessagesAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &body}); err != nil {
		t.Fatalf("forwardMessages: %v", err)
	}
	records := directSendLogRecords(t, logs)
	if len(records) != 2 {
		t.Fatalf("direct forward log lines = %d, want 2: %q", len(records), logs.String())
	}
	for _, randomID := range forwardRandomIDs {
		message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
		if err != nil || !ok {
			t.Fatalf("forwarded sender message: found=%v err=%v", ok, err)
		}
		pts, err := s.MessagePts(ctx, recipient.ID, message.PeerLocalID)
		if err != nil {
			t.Fatalf("forwarded recipient pts: %v", err)
		}
		assertDirectSendLog(t, records, sender.ID, recipient.ID, message.LocalID, pts, true, caption)
	}

	logs.Reset()
	var retryBody bin.Buffer
	if err := req.Encode(&retryBody); err != nil {
		t.Fatalf("encode forward retry: %v", err)
	}
	if _, _, _, err := h.handleForwardMessagesAfterReplyOnConn(nil, &mtproto.Request{Ctx: ctx, UserID: sender.ID, Buf: &retryBody}); err != nil {
		t.Fatalf("retry forwardMessages: %v", err)
	}
	replayRecords := directSendLogRecords(t, logs)
	if len(replayRecords) != 2 {
		t.Fatalf("direct forward replay log lines = %d, want 2: %q", len(replayRecords), logs.String())
	}
	for _, randomID := range forwardRandomIDs {
		message, ok, err := s.MessageByRandomID(ctx, sender.ID, randomID)
		if err != nil || !ok {
			t.Fatalf("forwarded sender replay message: found=%v err=%v", ok, err)
		}
		assertDirectSendReplayLog(t, replayRecords, sender.ID, recipient.ID, message.LocalID, 0, true, caption)
	}
}

func assertDirectSendLog(t *testing.T, records []map[string]any, senderID, recipientID, messageID int64, recipientPts int, stored bool, privateText string) {
	t.Helper()
	assertDirectSendLogWithDuplicate(t, records, senderID, recipientID, messageID, recipientPts, stored, false, privateText)
}

func directOutcomeRecords(t *testing.T, records []map[string]any) []map[string]any {
	t.Helper()
	var outcomes []map[string]any
	for _, record := range records {
		if record["msg"] == "direct message outcome" {
			outcomes = append(outcomes, record)
		}
	}
	return outcomes
}

func assertDirectSendReplayLog(t *testing.T, records []map[string]any, senderID, recipientID, messageID int64, recipientPts int, stored bool, privateText string) {
	t.Helper()
	assertDirectSendLogWithDuplicate(t, records, senderID, recipientID, messageID, recipientPts, stored, true, privateText)
}

func assertDirectSendLogWithDuplicate(t *testing.T, records []map[string]any, senderID, recipientID, messageID int64, recipientPts int, stored, duplicate bool, privateText string) {
	t.Helper()
	wantCopy := "stored"
	if !stored {
		wantCopy = "withheld"
	}
	for _, record := range records {
		if record["message_id"] != float64(messageID) {
			continue
		}
		for key, want := range map[string]any{
			"msg": "direct message outcome", "level": "INFO",
			"sender_id": float64(senderID), "recipient_id": float64(recipientID),
			"message_id": float64(messageID), "recipient_copy": wantCopy,
			"recipient_pts": float64(recipientPts), "duplicate": duplicate,
		} {
			if got := record[key]; got != want {
				t.Errorf("message %d log %s = %v, want %v", messageID, key, got, want)
			}
		}
		if privateText != "" && strings.Contains(fmt.Sprint(record), privateText) {
			t.Errorf("log exposed private message text %q", privateText)
		}
		return
	}
	t.Errorf("no log for message %d in records %#v", messageID, records)
}
