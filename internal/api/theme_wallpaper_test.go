package api_test

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestThemeAndWallpaperGetters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		get           func(userID, hash int64) (bin.Encoder, error)
		fullResponse  func(t *testing.T, response bin.Encoder) (int64, int)
		isNotModified func(bin.Encoder) bool
	}{
		{
			name: "wallpapers",
			get:  api.GetWallPapersForTest,
			fullResponse: func(t *testing.T, response bin.Encoder) (int64, int) {
				t.Helper()
				got, ok := response.(*tg.AccountWallPapers)
				if !ok {
					t.Fatalf("response = %T, want *tg.AccountWallPapers", response)
				}
				return got.Hash, len(got.Wallpapers)
			},
			isNotModified: func(response bin.Encoder) bool {
				_, ok := response.(*tg.AccountWallPapersNotModified)
				return ok
			},
		},
		{
			name: "chat themes",
			get:  api.GetChatThemesForTest,
			fullResponse: func(t *testing.T, response bin.Encoder) (int64, int) {
				t.Helper()
				got, ok := response.(*tg.AccountThemes)
				if !ok {
					t.Fatalf("response = %T, want *tg.AccountThemes", response)
				}
				return got.Hash, len(got.Themes)
			},
			isNotModified: func(response bin.Encoder) bool {
				_, ok := response.(*tg.AccountThemesNotModified)
				return ok
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			response, err := test.get(1, 0)
			if err != nil {
				t.Fatalf("get with hash 0: %v", err)
			}
			hash, count := test.fullResponse(t, response)
			if hash == 0 || count != 0 {
				t.Fatalf("full response hash = %d, item count = %d; want nonzero hash and empty list", hash, count)
			}
			var encoded bin.Buffer
			if err := response.Encode(&encoded); err != nil {
				t.Fatalf("encode full response: %v", err)
			}

			unchanged, err := test.get(1, hash)
			if err != nil {
				t.Fatalf("get with returned hash: %v", err)
			}
			if !test.isNotModified(unchanged) {
				t.Fatalf("response with returned hash = %T, want not modified", unchanged)
			}
			var encodedNotModified bin.Buffer
			if err := unchanged.Encode(&encodedNotModified); err != nil {
				t.Fatalf("encode not-modified response: %v", err)
			}

			stale, err := test.get(1, hash+1)
			if err != nil {
				t.Fatalf("get with different hash: %v", err)
			}
			if gotHash, gotCount := test.fullResponse(t, stale); gotHash != hash || gotCount != 0 {
				t.Fatalf("response with different hash = %d, %d items; want %d and empty", gotHash, gotCount, hash)
			}
		})

		t.Run(test.name+" requires authorization", func(t *testing.T) {
			t.Parallel()

			response, err := test.get(0, 0)
			if response != nil {
				t.Fatalf("response = %T, want nil", response)
			}
			if !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
				t.Fatalf("error = %v, want AUTH_KEY_UNREGISTERED", err)
			}
		})
	}
}
