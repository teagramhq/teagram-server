package api_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
)

func TestGetDialogsUsesConfiguredChannelUnreadLimit(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "postgres://localhost/tg")
	t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("a", 64))
	s := openStore(t)
	for _, tc := range []struct {
		name, limit string
		denied      bool
	}{
		{"default", "", true}, {"raised", "61", false}, {"disabled", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TG_RATE_LIMIT_CHANNEL_UNREAD_COUNT", tc.limit)
			cfg, err := config.Load(slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			owner, err := s.CreateUser(context.Background(), "+1555000"+map[string]string{"default": "101", "raised": "102", "disabled": "103"}[tc.name])
			if err != nil {
				t.Fatal(err)
			}
			h := fullChannelDispatcherWithRateLimits(s, cfg.RateLimits)
			for i := 1; i <= 61; i++ {
				body := dispatchChatMethod(t, h, owner.ID, "messages.getDialogs", &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: 1})
				var rpc mt.RPCError
				if err := rpc.Decode(&bin.Buffer{Buf: body}); err == nil {
					if i == 61 && tc.denied && strings.HasPrefix(rpc.ErrorMessage, "FLOOD_WAIT_") {
						continue
					}
					t.Fatalf("call %d: unexpected RPC error %s", i, rpc.ErrorMessage)
				}
				if i == 61 && tc.denied {
					t.Fatal("61st request succeeded, want FLOOD_WAIT")
				}
				var dialogs tg.MessagesDialogs
				if err := dialogs.Decode(&bin.Buffer{Buf: body}); err != nil {
					t.Fatalf("call %d: decode dialogs: %v", i, err)
				}
			}
		})
	}
}
