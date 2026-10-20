package api_test

import (
	"strings"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
)

func TestSetHistoryTTLRejectsPeriodOutsideAllowList(t *testing.T) {
	err := api.SetHistoryTTLForTest(nil, 1, &tg.MessagesSetHistoryTTLRequest{Peer: &tg.InputPeerSelf{}, Period: 2})
	if err == nil || !strings.Contains(err.Error(), "TTL_PERIOD_INVALID") {
		t.Fatalf("messages.setHistoryTTL error = %v, want TTL_PERIOD_INVALID", err)
	}
}
