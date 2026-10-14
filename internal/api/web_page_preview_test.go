package api_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestGetWebPagePreviewDoesNotLogInput(t *testing.T) {
	var logs bytes.Buffer
	h := api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.NewTextHandler(&logs, nil)),
		false,
		1,
		nil,
		1,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)

	const message = "private preview text https://example.invalid/private-preview-token"
	method := settingsHandler{
		name: "messages.getWebPagePreview",
		request: func() bin.Encoder {
			return &tg.MessagesGetWebPagePreviewRequest{Message: message}
		},
		response: func() bin.Decoder { return &tg.MessagesWebPagePreview{} },
	}
	body := dispatchSettings(t, h, method, 1, false)
	var response tg.MessagesWebPagePreview
	if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode messages.getWebPagePreview response: %v", err)
	}
	if _, ok := response.Media.(*tg.MessageMediaEmpty); !ok {
		t.Fatalf("media = %T, want *tg.MessageMediaEmpty", response.Media)
	}
	if len(response.Chats) != 0 || len(response.Users) != 0 {
		t.Fatalf("response contains %d chats and %d users, want empty", len(response.Chats), len(response.Users))
	}
	for _, marker := range []string{message, "https://example.invalid/private-preview-token"} {
		if strings.Contains(logs.String(), marker) {
			t.Fatalf("server logs contain request input %q", marker)
		}
	}
}
