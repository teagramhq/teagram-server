package api_test

import (
	"log/slog"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/photohash"
)

func TestHandlerMarksSessionRevocationsAsConcurrentRPCBarriers(t *testing.T) {
	handler := api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		0,
		nil,
		0,
		&peerhash.Deriver{},
		&photohash.Deriver{},
		config.RateLimitsConfig{},
		config.RegistrationClosed,
	)
	concurrent, ok := handler.(mtproto.ConcurrentRPCClassifier)
	if !ok || !concurrent.CanRunConcurrently(tg.MessagesGetSearchCountersRequestTypeID) {
		t.Fatal("configured handler no longer enables concurrent search counters")
	}
	barriers, ok := handler.(mtproto.ConcurrentRPCBarrierClassifier)
	if !ok {
		t.Fatal("configured handler does not expose concurrent RPC barriers")
	}
	for _, methodID := range []uint32{
		tg.AuthLogOutRequestTypeID,
		tg.AuthResetAuthorizationsRequestTypeID,
		tg.AccountResetAuthorizationRequestTypeID,
	} {
		if !barriers.MustWaitForConcurrentRPC(methodID) {
			t.Errorf("method %08x is not a concurrent RPC barrier", methodID)
		}
	}
	if barriers.MustWaitForConcurrentRPC(tg.HelpGetConfigRequestTypeID) {
		t.Fatal("independent help.getConfig unexpectedly became a concurrent RPC barrier")
	}
}
