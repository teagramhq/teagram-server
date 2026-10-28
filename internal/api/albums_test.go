package api_test

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

// truncatedSendMultiMedia encodes the sendMultiMedia constructor and nothing
// else, so decoding the body fails.
type truncatedSendMultiMedia struct{}

func (truncatedSendMultiMedia) Encode(b *bin.Buffer) error {
	b.PutID(tg.MessagesSendMultiMediaRequestTypeID)
	return nil
}

func TestSendMultiMediaUnboundAndUndecodable(t *testing.T) {
	t.Parallel()
	s, _ := openStoreDSN(t)
	dispatcher := api.New(
		s, 2, &tg.Config{}, slog.New(slog.DiscardHandler), false,
		1<<20, newBlobs(t), api.TestMaxUserStorageBytes, pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.DefaultRateLimits(), config.RegistrationInvite,
	)
	valid := &tg.MessagesSendMultiMediaRequest{
		Peer: &tg.InputPeerSelf{},
		MultiMedia: []tg.InputSingleMedia{
			{Media: &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: 1, AccessHash: 1}}, RandomID: 1},
			{Media: &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: 2, AccessHash: 2}}, RandomID: 2},
		},
	}
	for _, tc := range []struct {
		name    string
		userID  int64
		req     bin.Encoder
		code    int
		message string
	}{
		{"unbound key", 0, valid, 401, "AUTH_KEY_UNREGISTERED"},
		{"undecodable body", 1, truncatedSendMultiMedia{}, 400, "INPUT_METHOD_INVALID"},
	} {
		response := dispatchRegisteredSend(t, dispatcher, tc.userID, false, tc.req)
		rpc := &mt.RPCError{}
		if err := rpc.Decode(&bin.Buffer{Buf: response}); err != nil {
			t.Fatalf("%s: decode rejection: %v", tc.name, err)
		}
		if rpc.ErrorCode != tc.code || rpc.ErrorMessage != tc.message {
			t.Errorf("%s = %d %q, want %d %q", tc.name, rpc.ErrorCode, rpc.ErrorMessage, tc.code, tc.message)
		}
	}
}

func TestSendMultiMediaEnforcesPublishedCaptionLength(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	dispatcher := api.New(
		s, 2, &tg.Config{}, slog.New(slog.DiscardHandler), false,
		1<<20, newBlobs(t), api.TestMaxUserStorageBytes, pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.DefaultRateLimits(), config.RegistrationInvite,
	)
	maxLength := api.DefaultConfig(2, "127.0.0.1", 2443).CaptionLengthMax
	longCaption := strings.Repeat("c", maxLength+1)
	request := &tg.MessagesSendMultiMediaRequest{
		Peer: &tg.InputPeerSelf{},
		MultiMedia: []tg.InputSingleMedia{
			{Media: &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: 1, AccessHash: 1}}, Message: longCaption, RandomID: 1},
			{Media: &tg.InputMediaPhoto{ID: &tg.InputPhoto{ID: 2, AccessHash: 2}}, RandomID: 2},
		},
	}
	response := dispatchRegisteredSend(t, dispatcher, 1, false, request)
	rpc := &mt.RPCError{}
	if err := rpc.Decode(&bin.Buffer{Buf: response}); err != nil {
		t.Fatalf("decode oversized caption rejection: %v", err)
	}
	if rpc.ErrorCode != 400 || rpc.ErrorMessage != "MEDIA_CAPTION_TOO_LONG" {
		t.Fatalf("oversized caption = %d %q, want 400 MEDIA_CAPTION_TOO_LONG", rpc.ErrorCode, rpc.ErrorMessage)
	}
}
