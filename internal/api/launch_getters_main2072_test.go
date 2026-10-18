package api_test

import (
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMAIN2072EmptySponsoredAndGiftCodeGettersThroughDispatcher(t *testing.T) {
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

	methods := []settingsHandler{
		{
			name: "contacts.getSponsoredPeers",
			request: func() bin.Encoder {
				return &tg.ContactsGetSponsoredPeersRequest{Q: "search"}
			},
			response:              func() bin.Decoder { return &tg.ContactsSponsoredPeersBox{} },
			requiresAuthorization: true,
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.ContactsSponsoredPeersBox)
				if !ok {
					t.Fatalf("sponsored peers response = %T, want *tg.ContactsSponsoredPeersBox", response)
				}
				if _, ok := got.SponsoredPeers.(*tg.ContactsSponsoredPeersEmpty); !ok {
					t.Fatalf("sponsored peers result = %T, want *tg.ContactsSponsoredPeersEmpty", got.SponsoredPeers)
				}
			},
		},
		{
			name: "payments.getPremiumGiftCodeOptions",
			request: func() bin.Encoder {
				request := &tg.PaymentsGetPremiumGiftCodeOptionsRequest{}
				request.SetBoostPeer(&tg.InputPeerSelf{})
				return request
			},
			response:              func() bin.Decoder { return &tg.PremiumGiftCodeOptionVector{} },
			requiresAuthorization: true,
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.PremiumGiftCodeOptionVector)
				if !ok {
					t.Fatalf("gift code options response = %T, want *tg.PremiumGiftCodeOptionVector", response)
				}
				if len(got.Elems) != 0 {
					t.Fatalf("gift code options = %d entries, want empty", len(got.Elems))
				}
			},
		},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			t.Run("authorized", func(t *testing.T) {
				body := dispatchSettings(t, h, method, 1, false)
				response := method.response()
				if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				method.assert(t, response)
			})

			t.Run("unauthenticated", func(t *testing.T) {
				body := dispatchSettings(t, h, method, 0, false)
				rpc := &mt.RPCError{}
				if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
					t.Fatalf("decode rejection: %v", err)
				}
				if rpc.ErrorCode != 401 || rpc.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
					t.Fatalf("rejection = %d %q, want 401 AUTH_KEY_UNREGISTERED", rpc.ErrorCode, rpc.ErrorMessage)
				}
			})
		})
	}
}
