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

func TestMAIN2154EmptySavedMusicAndEmojiKeywordGettersThroughDispatcher(t *testing.T) {
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
		name          string
		constructorID uint32
		request       func() bin.Encoder
		response      func() bin.Decoder
		assert        func(*testing.T, bin.Decoder)
	}{
		{
			name:          "account.getSavedMusicIds",
			constructorID: 0xe09d5faf,
			request: func() bin.Encoder {
				return &tg.AccountGetSavedMusicIDsRequest{Hash: 123}
			},
			response: func() bin.Decoder { return &tg.AccountSavedMusicIDsBox{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountSavedMusicIDsBox)
				if !ok {
					t.Fatalf("saved music IDs response = %T, want *tg.AccountSavedMusicIDsBox", response)
				}
				result, ok := got.SavedMusicIds.(*tg.AccountSavedMusicIDs)
				if !ok || len(result.IDs) != 0 {
					t.Fatalf("saved music IDs = %#v, want an empty account.savedMusicIds result", got.SavedMusicIds)
				}
			},
		},
		{
			name:          "messages.getEmojiKeywordsDifference",
			constructorID: 0x1508b6af,
			request: func() bin.Encoder {
				return &tg.MessagesGetEmojiKeywordsDifferenceRequest{LangCode: "en", FromVersion: 12}
			},
			response: func() bin.Decoder { return &tg.EmojiKeywordsDifference{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.EmojiKeywordsDifference)
				if !ok {
					t.Fatalf("emoji keywords response = %T, want *tg.EmojiKeywordsDifference", response)
				}
				if got.LangCode != "en" || got.FromVersion != 12 || got.Version != 12 || len(got.Keywords) != 0 {
					t.Fatalf("emoji keywords difference = %+v, want an empty difference at version 12 for en", got)
				}
			},
		},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			request := method.request()
			constructor, ok := request.(interface{ TypeID() uint32 })
			if !ok {
				t.Fatalf("request %T does not expose its constructor ID", request)
			}
			if got := constructor.TypeID(); got != method.constructorID {
				t.Fatalf("constructor ID = %#08x, want %#08x", got, method.constructorID)
			}

			body := dispatchSettings(t, h, settingsHandler{
				name:    method.name,
				request: func() bin.Encoder { return request },
			}, 1, false)
			response := method.response()
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				t.Fatalf("decode getter response: %v", err)
			}
			method.assert(t, response)
		})
	}
}
