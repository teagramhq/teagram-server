package api_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestMAIN1505EmptyGettersThroughDispatcher(t *testing.T) {
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
			name: "stories.getStoriesArchive",
			request: func() bin.Encoder {
				return &tg.StoriesGetStoriesArchiveRequest{Peer: &tg.InputPeerSelf{}, OffsetID: 7, Limit: 20}
			},
			response: func() bin.Decoder { return &tg.StoriesStories{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.StoriesStories)
				if !ok {
					t.Fatalf("stories archive response = %T, want *tg.StoriesStories", response)
				}
				if got.Count != 0 || len(got.Stories) != 0 || len(got.Chats) != 0 || len(got.Users) != 0 {
					t.Fatalf("stories archive = %+v, want empty", got)
				}
			},
		},
		{
			name: "help.getPeerColors hash 0",
			request: func() bin.Encoder {
				return &tg.HelpGetPeerColorsRequest{Hash: 0}
			},
			response: func() bin.Decoder { return &tg.HelpPeerColorsBox{} },
			assert:   assertEmptyPeerColors,
		},
		{
			name: "help.getPeerColors mismatched hash",
			request: func() bin.Encoder {
				return &tg.HelpGetPeerColorsRequest{Hash: 1}
			},
			response: func() bin.Decoder { return &tg.HelpPeerColorsBox{} },
			assert:   assertEmptyPeerColors,
		},
		{
			name: "help.getPeerProfileColors hash 0",
			request: func() bin.Encoder {
				return &tg.HelpGetPeerProfileColorsRequest{Hash: 0}
			},
			response: func() bin.Decoder { return &tg.HelpPeerColorsBox{} },
			assert:   assertEmptyPeerColors,
		},
		{
			name: "help.getPeerProfileColors mismatched hash",
			request: func() bin.Encoder {
				return &tg.HelpGetPeerProfileColorsRequest{Hash: 1}
			},
			response: func() bin.Decoder { return &tg.HelpPeerColorsBox{} },
			assert:   assertEmptyPeerColors,
		},
		{
			name: "aicompose.getTones hash 0",
			request: func() bin.Encoder {
				return &tg.AicomposeGetTonesRequest{Hash: 0}
			},
			response: func() bin.Decoder { return &tg.AicomposeTonesBox{} },
			assert:   assertEmptyTones,
		},
		{
			name: "aicompose.getTones mismatched hash",
			request: func() bin.Encoder {
				return &tg.AicomposeGetTonesRequest{Hash: 1}
			},
			response: func() bin.Decoder { return &tg.AicomposeTonesBox{} },
			assert:   assertEmptyTones,
		},
		{
			name: "account.getDefaultEmojiStatuses hash 0",
			request: func() bin.Encoder {
				return &tg.AccountGetDefaultEmojiStatusesRequest{Hash: 0}
			},
			response: func() bin.Decoder { return &tg.AccountEmojiStatusesBox{} },
			assert:   assertEmptyEmojiStatuses,
		},
		{
			name: "account.getDefaultEmojiStatuses mismatched hash",
			request: func() bin.Encoder {
				return &tg.AccountGetDefaultEmojiStatusesRequest{Hash: 1}
			},
			response: func() bin.Decoder { return &tg.AccountEmojiStatusesBox{} },
			assert:   assertEmptyEmojiStatuses,
		},
		{
			name: "channels.getChannelRecommendations",
			request: func() bin.Encoder {
				request := &tg.ChannelsGetChannelRecommendationsRequest{}
				request.SetChannel(api.InputChannel(1, 7))
				return request
			},
			response: func() bin.Decoder { return &tg.MessagesChats{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.MessagesChats)
				if !ok {
					t.Fatalf("channel recommendations response = %T, want *tg.MessagesChats", response)
				}
				if len(got.Chats) != 0 {
					t.Fatalf("channel recommendations = %d chats, want empty", len(got.Chats))
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
					t.Fatalf("dispatch returned %d %s, want a successful getter response", rpc.ErrorCode, rpc.ErrorMessage)
				}
				t.Fatalf("decode getter response: %v", err)
			}
			method.assert(t, response)
		})
	}
}

func assertEmptyPeerColors(t *testing.T, response bin.Decoder) {
	t.Helper()
	got, ok := response.(*tg.HelpPeerColorsBox)
	if !ok {
		t.Fatalf("peer colors response = %T, want *tg.HelpPeerColorsBox", response)
	}
	full, ok := got.PeerColors.(*tg.HelpPeerColors)
	if !ok {
		t.Fatalf("peer colors variant = %T, want full empty colors", got.PeerColors)
	}
	if full.Hash != 0 || len(full.Colors) != 0 {
		t.Fatalf("peer colors = hash %d, %d colors; want empty", full.Hash, len(full.Colors))
	}
}

func assertEmptyTones(t *testing.T, response bin.Decoder) {
	t.Helper()
	got, ok := response.(*tg.AicomposeTonesBox)
	if !ok {
		t.Fatalf("tones response = %T, want *tg.AicomposeTonesBox", response)
	}
	full, ok := got.Tones.(*tg.AicomposeTones)
	if !ok {
		t.Fatalf("tones variant = %T, want full empty tones", got.Tones)
	}
	if full.Hash != 0 || len(full.Tones) != 0 || len(full.Users) != 0 {
		t.Fatalf("tones = hash %d, %d tones, %d users; want empty", full.Hash, len(full.Tones), len(full.Users))
	}
}

func assertEmptyEmojiStatuses(t *testing.T, response bin.Decoder) {
	t.Helper()
	got, ok := response.(*tg.AccountEmojiStatusesBox)
	if !ok {
		t.Fatalf("emoji statuses response = %T, want *tg.AccountEmojiStatusesBox", response)
	}
	full, ok := got.EmojiStatuses.(*tg.AccountEmojiStatuses)
	if !ok {
		t.Fatalf("emoji statuses variant = %T, want full empty statuses", got.EmojiStatuses)
	}
	if full.Hash != 0 || len(full.Statuses) != 0 {
		t.Fatalf("emoji statuses = hash %d, %d statuses; want empty", full.Hash, len(full.Statuses))
	}
}

func TestMAIN1505GetExportedChatInvitesAuthorization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551505001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	admin, err := s.CreateUser(ctx, "+15551505002")
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551505003")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	banned, err := s.CreateUser(ctx, "+15551505004")
	if err != nil {
		t.Fatalf("create banned member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551505005")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}

	chat, err := s.CreateChat(ctx, creator.ID, "Manage", []int64{admin.ID, member.ID})
	if err != nil {
		t.Fatalf("create basic chat: %v", err)
	}
	if _, _, err := s.SetChatAdmin(ctx, chat.ID, admin.ID, creator.ID, true); err != nil {
		t.Fatalf("make basic chat admin: %v", err)
	}
	channel, err := s.CreateChannel(ctx, creator.ID, "Manage channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannel(t, ctx, dsn, channel.ID, admin.ID)
	joinChannel(t, ctx, dsn, channel.ID, member.ID)
	joinChannel(t, ctx, dsn, channel.ID, banned.ID)
	if err := s.SetChannelRole(ctx, channel.ID, creator.ID, admin.ID, 1); err != nil {
		t.Fatalf("make channel admin: %v", err)
	}
	bannedUntil := time.Now().Add(time.Hour)
	if err := s.SetChannelBan(ctx, channel.ID, creator.ID, banned.ID, &bannedUntil, false); err != nil {
		t.Fatalf("ban channel member: %v", err)
	}

	h := fullChannelDispatcher(s)
	unknownID := int64(9_999_999)
	for _, tc := range []struct {
		name       string
		userID     int64
		peer       tg.InputPeerClass
		adminID    tg.InputUserClass
		wantError  string
		wantResult bool
	}{
		{name: "basic chat creator", userID: creator.ID, peer: &tg.InputPeerChat{ChatID: chat.ID}, adminID: &tg.InputUserSelf{}, wantResult: true},
		{name: "basic chat delegated admin", userID: admin.ID, peer: &tg.InputPeerChat{ChatID: chat.ID}, adminID: &tg.InputUserSelf{}, wantResult: true},
		{name: "basic chat ordinary member with creator filter", userID: member.ID, peer: &tg.InputPeerChat{ChatID: chat.ID}, adminID: api.InputUser(member.ID, creator.ID), wantError: "CHAT_ADMIN_REQUIRED"},
		{name: "basic chat non-member", userID: outsider.ID, peer: &tg.InputPeerChat{ChatID: chat.ID}, adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "unknown basic chat", userID: outsider.ID, peer: &tg.InputPeerChat{ChatID: unknownID}, adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "channel creator", userID: creator.ID, peer: api.InputPeerChannel(creator.ID, channel.ID), adminID: &tg.InputUserSelf{}, wantResult: true},
		{name: "channel delegated admin with attacker filter", userID: admin.ID, peer: api.InputPeerChannel(admin.ID, channel.ID), adminID: api.InputUser(admin.ID, outsider.ID), wantResult: true},
		{name: "channel ordinary member with admin filter", userID: member.ID, peer: api.InputPeerChannel(member.ID, channel.ID), adminID: api.InputUser(member.ID, admin.ID), wantError: "CHAT_ADMIN_REQUIRED"},
		{name: "channel non-member", userID: outsider.ID, peer: api.InputPeerChannel(outsider.ID, channel.ID), adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "inaccessible channel hash", userID: outsider.ID, peer: &tg.InputPeerChannel{ChannelID: channel.ID, AccessHash: 0}, adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "banned channel member", userID: banned.ID, peer: api.InputPeerChannel(banned.ID, channel.ID), adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "unknown channel", userID: outsider.ID, peer: api.InputPeerChannel(outsider.ID, unknownID), adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "invalid user peer", userID: creator.ID, peer: api.InputPeerUser(creator.ID, outsider.ID), adminID: &tg.InputUserSelf{}, wantError: "PEER_ID_INVALID"},
		{name: "unauthenticated", userID: 0, peer: &tg.InputPeerChat{ChatID: chat.ID}, adminID: &tg.InputUserSelf{}, wantError: "AUTH_KEY_UNREGISTERED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, rpc := dispatchExportedChatInvites(t, h, tc.userID, exportedChatInvitesRequest(tc.peer, tc.adminID))
			if tc.wantError != "" {
				if got != nil || rpc == nil || rpc.ErrorMessage != tc.wantError {
					t.Fatalf("reply = %v, error = %v, want %s", got, rpc, tc.wantError)
				}
				return
			}
			if !tc.wantResult || rpc != nil || got == nil {
				t.Fatalf("reply = %v, error = %v, want empty exported-invites result", got, rpc)
			}
			if got.Count != 0 || len(got.Invites) != 0 || len(got.Users) != 0 {
				t.Fatalf("exported invites = count %d, %d invites, %d users; want empty", got.Count, len(got.Invites), len(got.Users))
			}
		})
	}

	if got := channelInviteRowCount(t, ctx, dsn, channel.ID); got != 0 {
		t.Fatalf("getExportedChatInvites added %d channel invite rows, want none", got)
	}
}

func exportedChatInvitesRequest(peer tg.InputPeerClass, adminID tg.InputUserClass) *tg.MessagesGetExportedChatInvitesRequest {
	request := &tg.MessagesGetExportedChatInvitesRequest{
		Peer:       peer,
		AdminID:    adminID,
		OffsetDate: 123,
		OffsetLink: "cursor",
		Limit:      20,
	}
	request.SetRevoked(true)
	request.SetOffsetDate(123)
	request.SetOffsetLink("cursor")
	return request
}

func dispatchExportedChatInvites(
	t *testing.T,
	h mtproto.Handler,
	userID int64,
	request *tg.MessagesGetExportedChatInvitesRequest,
) (*tg.MessagesExportedChatInvites, *mt.RPCError) {
	t.Helper()
	method := settingsHandler{
		name: "messages.getExportedChatInvites",
		request: func() bin.Encoder {
			return request
		},
	}
	body := dispatchSettings(t, h, method, userID, false)
	var result tg.MessagesExportedChatInvites
	if err := result.Decode(&bin.Buffer{Buf: body}); err == nil {
		return &result, nil
	}
	var rpc mt.RPCError
	if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
		t.Fatalf("decode getExportedChatInvites result: %v", err)
	}
	return nil, &rpc
}

func channelInviteRowCount(t *testing.T, ctx context.Context, dsn string, channelID int64) int {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to count channel invites: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close invite count connection: %v", err)
		}
	}()
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM channel_invites WHERE channel_id = $1`, channelID).Scan(&count); err != nil {
		t.Fatalf("count channel invite rows: %v", err)
	}
	return count
}
