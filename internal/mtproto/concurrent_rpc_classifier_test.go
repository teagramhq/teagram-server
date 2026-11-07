package mtproto_test

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestConcurrentRPCClassifierUnwrapsOnlyStatelessEnvelopes(t *testing.T) {
	const methodID = tg.MessagesGetSearchCountersRequestTypeID
	searchCounters := func() *tg.MessagesGetSearchCountersRequest {
		return &tg.MessagesGetSearchCountersRequest{Peer: &tg.InputPeerEmpty{}}
	}
	handler := mtproto.ConcurrentRPCs(mtproto.HandlerFunc(func(*mtproto.Conn, *mtproto.Request) error {
		return nil
	}), methodID)
	classifier, ok := handler.(mtproto.ConcurrentRPCClassifier)
	if !ok {
		t.Fatal("ConcurrentRPCs handler does not expose its classifier")
	}
	for _, tc := range []struct {
		name string
		body bin.Encoder
		want bool
	}{
		{name: "direct", body: searchCounters(), want: true},
		{name: "layer envelope", body: &tg.InvokeWithLayerRequest{Layer: 228, Query: searchCounters()}, want: true},
		{name: "updates envelope", body: &tg.InvokeWithoutUpdatesRequest{Query: searchCounters()}, want: true},
		{name: "after message dependency", body: &tg.InvokeAfterMsgRequest{MsgID: 1, Query: searchCounters()}, want: false},
		{name: "init connection state", body: &tg.InitConnectionRequest{Query: searchCounters()}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifier.CanRunConcurrently(encodeReq(t, tc.body)); got != tc.want {
				t.Fatalf("CanRunConcurrently = %v, want %v", got, tc.want)
			}
		})
	}
}
