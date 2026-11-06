package api_test

import (
	"context"
	"log/slog"
	"strconv"
	"testing"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSearchCountersFailureLogIdentifiesPeer(t *testing.T) {
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551248996")
	if err != nil {
		t.Fatal(err)
	}
	peer, err := s.CreateUser(ctx, "+15551248997")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	logs := &captureHandler{}
	_, err = api.SearchCountersForTestWithLogger(s, owner.ID, &tg.MessagesGetSearchCountersRequest{
		Peer:    api.InputPeerUser(owner.ID, peer.ID),
		Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterDocument{}},
	}, slog.New(logs))
	if err == nil {
		t.Fatal("search counters succeeded with a closed store")
	}
	if len(logs.records) != 1 {
		t.Fatalf("captured %d failure logs, want one", len(logs.records))
	}
	attrs := map[string]string{}
	logs.records[0].Attrs(func(attr slog.Attr) bool {
		attrs[attr.Key] = attr.Value.String()
		return true
	})
	if attrs["peer_type"] != strconv.Itoa(int(store.PeerTypeUser)) {
		t.Errorf("peer_type log field = %q, want %d", attrs["peer_type"], store.PeerTypeUser)
	}
	if attrs["peer_id"] != strconv.FormatInt(peer.ID, 10) {
		t.Errorf("peer_id log field = %q, want %d", attrs["peer_id"], peer.ID)
	}
}
