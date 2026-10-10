package api_test

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5/pgxpool"

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

func TestMAIN1594GetMessageReadParticipantsUsesStoredReadDates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	pool := readParticipantsTestPool(t, ctx, dsn)
	users, chat := chatWith(t, s, "+15551547001", "+15551547002", "+15551547003")
	creator, sender, reader := users[0], users[1], users[2]

	message := sendChatForReadHistory(t, s, chat.ID, sender.ID, 154701)
	h := fullChannelDispatcher(s)
	result, rpc := getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil {
		t.Fatalf("read participants returned %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if len(result) != 0 {
		t.Fatalf("read participants before read = %v, want empty", result)
	}

	otherChat, err := s.CreateChat(ctx, creator.ID, "Other", []int64{users[1].ID})
	if err != nil {
		t.Fatalf("create other chat: %v", err)
	}
	otherMessage := sendChatForReadHistory(t, s, otherChat.ID, sender.ID, 154702)
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(otherMessage.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("foreign chat message error = %v, want PEER_ID_INVALID", rpc)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET fanout_id=0 WHERE owner_id=$1 AND local_id=$2 AND peer_type=$3 AND peer_id=$4`, sender.ID, otherMessage.LocalID, int16(store.PeerTypeChat), otherChat.ID); err != nil {
		t.Fatalf("clear foreign message identity: %v", err)
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, otherChat.ID, int(otherMessage.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("message without shared identity error = %v, want PEER_ID_INVALID", rpc)
	}

	memberHistory, err := s.History(ctx, reader.ID, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("member chat history: %v", err)
	}
	var memberMessageID int64
	for _, item := range memberHistory {
		if item.FanoutID == message.FanoutID {
			memberMessageID = item.LocalID
			break
		}
	}
	if memberMessageID == 0 {
		t.Fatal("member message copy missing before removal")
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, reader.ID, chat.ID, int(memberMessageID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("incoming-copy lookup error = %v, want PEER_ID_INVALID", rpc)
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, memberMessageID); err != nil {
		t.Fatalf("member readHistory: %v", err)
	}
	var storedReadAt time.Time
	if err := pool.QueryRow(ctx, `SELECT read_at FROM chat_read_receipts WHERE chat_id=$1 AND fanout_id=$2 AND reader_id=$3`, chat.ID, message.FanoutID, reader.ID).Scan(&storedReadAt); err != nil {
		t.Fatalf("read persisted first-read date: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chat_read_receipts (chat_id, fanout_id, reader_id, sent_at, read_at) VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, chat.ID, message.FanoutID, sender.ID, message.Date, storedReadAt); err != nil {
		t.Fatalf("seed sender receipt: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil {
		t.Fatalf("read participants after member read returned %d %s", rpc.ErrorCode, rpc.ErrorMessage)
	}
	if len(result) != 1 || result[0].UserID != reader.ID || result[0].Date != int(storedReadAt.Unix()) {
		t.Fatalf("read participants after member read = %v, want reader %d at stored date %d", result, reader.ID, storedReadAt.Unix())
	}

	second := sendChatForReadHistory(t, s, chat.ID, sender.ID, 154703)
	secondHistory, err := s.History(ctx, reader.ID, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("member history after second send: %v", err)
	}
	var secondMemberMessageID int64
	for _, item := range secondHistory {
		if item.FanoutID == second.FanoutID {
			secondMemberMessageID = item.LocalID
			break
		}
	}
	if secondMemberMessageID == 0 {
		t.Fatal("second member message copy missing")
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, secondMemberMessageID); err != nil {
		t.Fatalf("member read second message: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 1 || result[0].UserID != reader.ID || result[0].Date != int(storedReadAt.Unix()) {
		t.Fatalf("first message participants after later read = %v rpc=%v, want original stored date %d", result, rpc, storedReadAt.Unix())
	}

	legacy := sendChatForReadHistory(t, s, chat.ID, sender.ID, 154704)
	legacyHistory, err := s.History(ctx, reader.ID, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("reader history before legacy marker: %v", err)
	}
	var legacyCopy store.Message
	for _, item := range legacyHistory {
		if item.FanoutID == legacy.FanoutID {
			legacyCopy = item
			break
		}
	}
	if legacyCopy.LocalID == 0 {
		t.Fatal("legacy reader copy missing")
	}
	if _, err := pool.Exec(ctx, `UPDATE dialogs SET read_inbox_max_id=$1 WHERE owner_id=$2 AND peer_type=$3 AND peer_id=$4`, legacyCopy.LocalID, reader.ID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("seed legacy read marker: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(legacy.LocalID))
	if rpc != nil || len(result) != 0 {
		t.Fatalf("legacy message participants = %v rpc=%v, want empty", result, rpc)
	}

	if removed, _, _, removeErr := s.RemoveChatUser(ctx, chat.ID, reader.ID, creator.ID); removeErr != nil || !removed {
		t.Fatalf("remove reader: removed=%v err=%v", removed, removeErr)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 0 {
		t.Fatalf("participants after reader removal = %v rpc=%v, want empty", result, rpc)
	}
	outsider, err := s.CreateUser(ctx, "+15551547004")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, outsider.ID, chat.ID, int(second.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("outsider sender lookup error = %v, want PEER_ID_INVALID", rpc)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET deleted=true WHERE owner_id=$1 AND local_id=$2 AND peer_type=$3 AND peer_id=$4`, sender.ID, message.LocalID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("delete sender copy: %v", err)
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("deleted sender copy error = %v, want PEER_ID_INVALID", rpc)
	}
	if removed, _, _, removeErr := s.RemoveChatUser(ctx, chat.ID, sender.ID, creator.ID); removeErr != nil || !removed {
		t.Fatalf("remove sender: removed=%v err=%v", removed, removeErr)
	}
	if _, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(second.LocalID)); rpc == nil || rpc.ErrorMessage != "PEER_ID_INVALID" {
		t.Fatalf("removed sender error = %v, want PEER_ID_INVALID", rpc)
	}
}

func TestMAIN1594ReadParticipantEligibilityBoundaries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	pool := readParticipantsTestPool(t, ctx, dsn)
	users := make([]store.User, 99)
	for i := range users {
		user, err := s.CreateUser(ctx, fmt.Sprintf("+15551548%03d", i))
		if err != nil {
			t.Fatalf("create user %d: %v", i, err)
		}
		users[i] = user
	}
	sender, reader := users[0], users[1]
	participants := make([]int64, 0, len(users)-1)
	for _, user := range users[1:] {
		participants = append(participants, user.ID)
	}
	chat, err := s.CreateChat(ctx, sender.ID, "eligibility", participants)
	if err != nil {
		t.Fatalf("create 99-member chat: %v", err)
	}
	message := sendChatForReadHistory(t, s, chat.ID, sender.ID, 154801)
	readerHistory, err := s.History(ctx, reader.ID, store.PeerTypeChat, chat.ID, 0, 20)
	if err != nil {
		t.Fatalf("reader history: %v", err)
	}
	var readerLocalID int64
	for _, item := range readerHistory {
		if item.FanoutID == message.FanoutID {
			readerLocalID = item.LocalID
			break
		}
	}
	if readerLocalID == 0 {
		t.Fatal("reader message copy missing")
	}
	if _, err := s.ReadChatHistory(ctx, reader.ID, chat.ID, readerLocalID); err != nil {
		t.Fatalf("read eligible message: %v", err)
	}
	h := fullChannelDispatcher(s)
	result, rpc := getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 1 || result[0].UserID != reader.ID {
		t.Fatalf("99-member chat participants = %v rpc=%v, want reader %d", result, rpc, reader.ID)
	}
	extra, err := s.CreateUser(ctx, "+15551548999")
	if err != nil {
		t.Fatalf("create 100th member: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO chat_participants (chat_id, user_id, inviter_id) VALUES ($1,$2,$3)`, chat.ID, extra.ID, sender.ID); err != nil {
		t.Fatalf("add 100th member: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 0 {
		t.Fatalf("100-member chat participants = %v rpc=%v, want empty", result, rpc)
	}

	if _, err := pool.Exec(ctx, `UPDATE messages SET date=statement_timestamp()-interval '604799 seconds' WHERE owner_id=$1 AND local_id=$2 AND peer_type=$3 AND peer_id=$4`, sender.ID, message.LocalID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("set message age to 604799 seconds: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM chat_participants WHERE chat_id=$1 AND user_id=$2`, chat.ID, extra.ID); err != nil {
		t.Fatalf("restore 99-member chat: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 1 || result[0].UserID != reader.ID {
		t.Fatalf("message at 604799 seconds participants = %v rpc=%v, want reader %d", result, rpc, reader.ID)
	}
	if _, err := pool.Exec(ctx, `UPDATE messages SET date=statement_timestamp()-interval '604800 seconds' WHERE owner_id=$1 AND local_id=$2 AND peer_type=$3 AND peer_id=$4`, sender.ID, message.LocalID, int16(store.PeerTypeChat), chat.ID); err != nil {
		t.Fatalf("set message age to 604800 seconds: %v", err)
	}
	result, rpc = getMessageReadParticipantsViaDispatcher(t, h, sender.ID, chat.ID, int(message.LocalID))
	if rpc != nil || len(result) != 0 {
		t.Fatalf("message at 604800 seconds participants = %v rpc=%v, want empty", result, rpc)
	}
}

func readParticipantsTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open test database pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
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
