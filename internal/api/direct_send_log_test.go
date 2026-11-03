//nolint:testpackage // This test exercises the private send handler and its logger.
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

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
