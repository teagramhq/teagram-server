package api_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestMAIN1547StickerSearchMethodsThroughDispatcher(t *testing.T) {
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
			name: "messages.searchStickers emoji suggestion",
			request: func() bin.Encoder {
				return &tg.MessagesSearchStickersRequest{
					Emoticon: "🙂",
					LangCode: []string{"en"},
					Limit:    20,
				}
			},
			response: func() bin.Decoder { return &tg.MessagesFoundStickersBox{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				box, ok := response.(*tg.MessagesFoundStickersBox)
				if !ok {
					t.Fatalf("found stickers response = %T, want *tg.MessagesFoundStickersBox", response)
				}
				got := box.FoundStickers
				full, ok := got.(*tg.MessagesFoundStickers)
				if !ok {
					t.Fatalf("found stickers = %T, want empty full result", got)
				}
				if full.Hash != 0 || len(full.Stickers) != 0 {
					t.Fatalf("found stickers = hash %d, %d stickers; want empty", full.Hash, len(full.Stickers))
				}
			},
		},
		{
			name: "messages.searchEmojiStickerSets emoji suggestion",
			request: func() bin.Encoder {
				return &tg.MessagesSearchEmojiStickerSetsRequest{Q: "🙂"}
			},
			response: func() bin.Decoder { return &tg.MessagesFoundStickerSetsBox{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				box, ok := response.(*tg.MessagesFoundStickerSetsBox)
				if !ok {
					t.Fatalf("found sticker sets response = %T, want *tg.MessagesFoundStickerSetsBox", response)
				}
				got := box.FoundStickerSets
				full, ok := got.(*tg.MessagesFoundStickerSets)
				if !ok {
					t.Fatalf("found sticker sets = %T, want empty full result", got)
				}
				if full.Hash != 0 || len(full.Sets) != 0 {
					t.Fatalf("found sticker sets = hash %d, %d sets; want empty", full.Hash, len(full.Sets))
				}
			},
		},
	}

	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			body := dispatchSettings(t, h, method, 1, false)
			response := method.response()
			if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
				var rpc mt.RPCError
				if decodeErr := rpc.Decode(&bin.Buffer{Buf: body}); decodeErr == nil {
					t.Fatalf("dispatch returned %d %s, want a successful empty response", rpc.ErrorCode, rpc.ErrorMessage)
				}
				t.Fatalf("decode empty response: %v", err)
			}
			method.assert(t, response)
		})
	}
}

func TestMAIN1547GetMessageReadParticipantsValidatesChatMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551547001", "+15551547002")
	creator := users[0]

	message := sendChatForReadHistory(t, s, chat.ID, creator.ID, 154701)
	h := fullChannelDispatcher(s)
	result, rpc := getMessageReadParticipantsViaDispatcher(t, h, creator.ID, chat.ID, int(message.LocalID))
	if rpc != nil {
		t.Fatalf("read participants returned %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if len(result) != 0 {
		t.Fatalf("read participants = %v, want empty because per-message read dates are not stored", result)
	}

	otherChat, err := s.CreateChat(ctx, creator.ID, "Other", []int64{users[1].ID})
	if err != nil {
		t.Fatalf("create other chat: %v", err)
	}
	otherMessage := sendChatForReadHistory(t, s, otherChat.ID, creator.ID, 154702)
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, creator.ID, chat.ID, int(otherMessage.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("foreign chat message error = %v, want PEER_ID_INVALID", rpc)
	}

	memberHistory, err := s.History(ctx, users[1].ID, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("member chat history: %v", err)
	}
	var memberMessageID int
	for _, item := range memberHistory {
		if item.Text == "group message" {
			memberMessageID = int(item.LocalID)
			break
		}
	}
	if memberMessageID == 0 {
		t.Fatal("member message copy missing before removal")
	}
	if removed, _, _, removeErr := s.RemoveChatUser(ctx, chat.ID, users[1].ID, creator.ID); removeErr != nil || !removed {
		t.Fatalf("remove member: removed=%v err=%v", removed, removeErr)
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, users[1].ID, chat.ID, memberMessageID); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("removed member error = %v, want PEER_ID_INVALID", rpc)
	}
}

func TestMAIN1547ReportReadMetricsRequiresChannelMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551547003")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551547004")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551547005")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Read metrics", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := s.CreateChannelInvite(ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	if _, _, err := s.JoinChannelByInvite(ctx, invite, member.ID); err != nil {
		t.Fatalf("join member: %v", err)
	}

	h := fullChannelDispatcher(s)
	if ok, rpc := reportReadMetricsViaDispatcher(t, h, member.ID, api.InputPeerChannel(member.ID, channel.ID)); rpc != nil || !ok {
		t.Fatalf("member reportReadMetrics: ok=%v rpc=%v, want BoolTrue", ok, rpc)
	}
	if ok, rpc := reportReadMetricsViaDispatcher(t, h, outsider.ID, api.InputPeerChannel(outsider.ID, channel.ID)); ok || rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("outsider reportReadMetrics: ok=%v rpc=%v, want PEER_ID_INVALID", ok, rpc)
	}
}

func reportReadMetricsViaDispatcher(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	peer tg.InputPeerClass,
) (bool, *mt.RPCError) {
	t.Helper()
	body := dispatchSettings(t, h, settingsHandler{
		name: "messages.reportReadMetrics",
		request: func() bin.Encoder {
			return &tg.MessagesReportReadMetricsRequest{
				Peer: peer,
				Metrics: []tg.InputMessageReadMetric{{
					MsgID:                         154703,
					ViewID:                        154703,
					TimeInViewMs:                  1200,
					ActiveTimeInViewMs:            900,
					HeightToViewportRatioPermille: 900,
					SeenRangeRatioPermille:        1000,
				}},
			}
		},
	}, userID, false)
	var success tg.BoolTrue
	if err := success.Decode(&bin.Buffer{Buf: body}); err == nil {
		return true, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode reportReadMetrics response: %v", err)
	}
	return false, &rpc
}

func getMessageReadParticipantsViaDispatcher(
	t *testing.T,
	h mtproto.Handler,
	userID, chatID int64,
	msgID int,
) ([]tg.ReadParticipantDate, *mt.RPCError) {
	t.Helper()
	method := settingsHandler{
		name: "messages.getMessageReadParticipants",
		request: func() bin.Encoder {
			return &tg.MessagesGetMessageReadParticipantsRequest{
				Peer:  &tg.InputPeerChat{ChatID: chatID},
				MsgID: msgID,
			}
		},
	}
	body := dispatchSettings(t, h, method, userID, false)
	var result tg.ReadParticipantDateVector
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return result.Elems, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode read participants response: %v", err)
	}
	return nil, &rpc
}
