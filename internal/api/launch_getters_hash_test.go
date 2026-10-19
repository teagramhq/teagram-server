package api_test

import (
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestLaunchGetterHashZeroReturnsFullEmptyResults(t *testing.T) {
	t.Parallel()
	h := api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		1,
		nil,
		1,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)

	methods := []struct {
		name     string
		request  func(int64) bin.Encoder
		response func() bin.Decoder
		assert   func(*testing.T, bin.Decoder)
	}{
		{
			name: "messages.getAvailableEffects",
			request: func(hash int64) bin.Encoder {
				return &tg.MessagesGetAvailableEffectsRequest{Hash: int(hash)}
			},
			response: func() bin.Decoder { return &tg.MessagesAvailableEffects{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.MessagesAvailableEffects)
				if !ok {
					t.Fatalf("available effects response = %T, want *tg.MessagesAvailableEffects", response)
				}
				if got.Hash != 0 || len(got.Effects) != 0 || len(got.Documents) != 0 {
					t.Fatalf("available effects = hash %d, %d effects and %d documents, want empty", got.Hash, len(got.Effects), len(got.Documents))
				}
			},
		},
		{
			name: "messages.getEmojiStickerGroups",
			request: func(hash int64) bin.Encoder {
				return &tg.MessagesGetEmojiStickerGroupsRequest{Hash: int(hash)}
			},
			response: func() bin.Decoder { return &tg.MessagesEmojiGroups{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.MessagesEmojiGroups)
				if !ok {
					t.Fatalf("emoji sticker groups response = %T, want *tg.MessagesEmojiGroups", response)
				}
				if got.Hash != 0 || len(got.Groups) != 0 {
					t.Fatalf("emoji sticker groups = hash %d, %d groups, want empty", got.Hash, len(got.Groups))
				}
			},
		},
		{
			name: "messages.getTopReactions",
			request: func(hash int64) bin.Encoder {
				return &tg.MessagesGetTopReactionsRequest{Limit: 20, Hash: hash}
			},
			response: func() bin.Decoder { return &tg.MessagesReactions{} },
			assert:   assertEmptyLaunchReactions,
		},
		{
			name: "messages.getRecentReactions",
			request: func(hash int64) bin.Encoder {
				return &tg.MessagesGetRecentReactionsRequest{Limit: 20, Hash: hash}
			},
			response: func() bin.Decoder { return &tg.MessagesReactions{} },
			assert:   assertEmptyLaunchReactions,
		},
		{
			name: "messages.getDefaultTagReactions",
			request: func(hash int64) bin.Encoder {
				return &tg.MessagesGetDefaultTagReactionsRequest{Hash: hash}
			},
			response: func() bin.Decoder { return &tg.MessagesReactions{} },
			assert:   assertEmptyLaunchReactions,
		},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			body := dispatchSettings(t, h, settingsHandler{
				name: method.name,
				request: func() bin.Encoder {
					return method.request(0)
				},
				response:              method.response,
				requiresAuthorization: true,
			}, 1, false)
			response := method.response()
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			method.assert(t, response)
		})
	}
}

func assertEmptyLaunchReactions(t *testing.T, response bin.Decoder) {
	t.Helper()
	got, ok := response.(*tg.MessagesReactions)
	if !ok {
		t.Fatalf("reactions response = %T, want *tg.MessagesReactions", response)
	}
	if got.Hash != 0 || len(got.Reactions) != 0 {
		t.Fatalf("reactions = hash %d, %d entries, want empty", got.Hash, len(got.Reactions))
	}
}
