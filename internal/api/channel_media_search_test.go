package api_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSearchChannelOnlyMediaSubtypeFilters(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551297201")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297202")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	ch, err := s.CreateChannel(ctx, creator.ID, "Shared media", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, ch, member.ID)

	if _, err := sendToChannel(t, s, creator.ID, ch.ID, "needle plain text", 97201); err != nil {
		t.Fatalf("post plain text: %v", err)
	}
	if _, err := sendToChannel(t, s, creator.ID, ch.ID, "needle https://example.test/channel", 97202); err != nil {
		t.Fatalf("post URL: %v", err)
	}
	seedFile := func(name string, rights []string, stored bool) int64 {
		t.Helper()
		return insertChannelSearchFile(t, ctx, dsn, creator.ID, name, rights, stored)
	}
	postFile := func(text string, fileID int64, randomID int64) store.ChannelMessage {
		t.Helper()
		msg, _, dup, err := s.PostChannelMessage(ctx, ch.ID, creator.ID, text, randomID, &fileID, 0)
		if err != nil || dup {
			t.Fatalf("post file %q: dup=%v err=%v", text, dup, err)
		}
		return msg
	}
	postFile("needle generic document", seedFile("generic.bin", []string{}, true), 97203)
	postFile("needle unknown subtype", seedFile("unknown.bin", nil, true), 97204)
	postFile("needle unstored video", seedFile("unstored.mp4", []string{"send_videos"}, false), 97205)
	videoNeedle := postFile("needle video", seedFile("video.mp4", []string{"send_videos"}, true), 97206)
	gifNeedle := postFile("needle gif", seedFile("animated.gif", []string{"send_gifs"}, true), 97207)
	roundNeedle := postFile("needle round video", seedFile("round.mp4", []string{"send_roundvideos"}, true), 97208)
	voiceQuiet := postFile("quiet voice", seedFile("voice.ogg", []string{"send_voices"}, true), 97209)
	musicNeedle := postFile("needle music", seedFile("music.mp3", []string{"send_audios"}, true), 97210)
	videoQuiet := postFile("quiet video", seedFile("quiet.mp4", []string{"send_videos"}, true), 97211)
	gifQuiet := postFile("quiet gif", seedFile("quiet.gif", []string{"send_gifs"}, true), 97212)
	roundQuiet := postFile("quiet round video", seedFile("quiet-round.mp4", []string{"send_roundvideos"}, true), 97213)
	voiceNeedle := postFile("needle voice", seedFile("needle.ogg", []string{"send_voices"}, true), 97214)
	musicQuiet := postFile("quiet music", seedFile("quiet.mp3", []string{"send_audios"}, true), 97215)
	deletedVideo := postFile("needle deleted video", seedFile("deleted.mp4", []string{"send_videos"}, true), 97216)
	deleteChannelPost(t, ctx, dsn, ch.ID, deletedVideo.LocalID)
	serviceSubtype := postFile("needle service subtype", seedFile("service.bin", []string{
		"send_videos", "send_gifs", "send_roundvideos", "send_voices", "send_audios",
	}, true), 97217)
	channelExec(t, ctx, dsn,
		`UPDATE channel_messages SET action_type = 1 WHERE channel_id = $1 AND local_id = $2`,
		ch.ID, serviceSubtype.LocalID)

	filters := []struct {
		name       string
		filter     tg.MessagesFilterClass
		wantAll    []int64
		wantNeedle []int64
	}{
		{"video", &tg.InputMessagesFilterVideo{}, []int64{videoQuiet.LocalID, videoNeedle.LocalID}, []int64{videoNeedle.LocalID}},
		{"gif", &tg.InputMessagesFilterGif{}, []int64{gifQuiet.LocalID, gifNeedle.LocalID}, []int64{gifNeedle.LocalID}},
		{"poll", &tg.InputMessagesFilterPoll{}, nil, nil},
		{"round voice", &tg.InputMessagesFilterRoundVoice{}, []int64{voiceNeedle.LocalID, roundQuiet.LocalID, voiceQuiet.LocalID, roundNeedle.LocalID}, []int64{voiceNeedle.LocalID, roundNeedle.LocalID}},
		{"music", &tg.InputMessagesFilterMusic{}, []int64{musicQuiet.LocalID, musicNeedle.LocalID}, []int64{musicNeedle.LocalID}},
	}
	peer := channelPeer(member.ID, ch.ID)
	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := searchSharedMedia(s, member.ID, peer, "", tc.filter, 0, 100)
			if err != nil {
				t.Fatalf("search: %v", err)
			}
			got := channelMediaSearchResult(t, enc)
			assertChannelMediaResult(t, got, tc.wantAll)

			enc, err = searchSharedMedia(s, member.ID, peer, "", tc.filter, 0, 0)
			if err != nil {
				t.Fatalf("count-only search: %v", err)
			}
			got = channelMediaSearchResult(t, enc)
			if got.Count != len(tc.wantAll) || len(got.Messages) != 0 {
				t.Fatalf("count-only count/messages = %d/%d, want %d/0", got.Count, len(got.Messages), len(tc.wantAll))
			}

			enc, err = searchSharedMedia(s, member.ID, peer, "needle", tc.filter, 0, 100)
			if err != nil {
				t.Fatalf("keyword search: %v", err)
			}
			got = channelMediaSearchResult(t, enc)
			assertChannelMediaResult(t, got, tc.wantNeedle)

			if len(tc.wantAll) > 0 {
				enc, err = searchSharedMedia(s, member.ID, peer, "", tc.filter, 0, 1)
				if err != nil {
					t.Fatalf("first page: %v", err)
				}
				first := channelMediaSearchResult(t, enc)
				if first.Count != len(tc.wantAll) || !sameIDs(channelMediaIDs(t, first.Messages), tc.wantAll[:1]) {
					t.Fatalf("first page count/ids = %d/%v, want %d/%v", first.Count, channelMediaIDs(t, first.Messages), len(tc.wantAll), tc.wantAll[:1])
				}
				enc, err = searchSharedMedia(s, member.ID, peer, "", tc.filter, int(tc.wantAll[0]), 100)
				if err != nil {
					t.Fatalf("older page: %v", err)
				}
				older := channelMediaSearchResult(t, enc)
				if older.Count != len(tc.wantAll) || !sameIDs(channelMediaIDs(t, older.Messages), tc.wantAll[1:]) {
					t.Fatalf("older page count/ids = %d/%v, want %d/%v", older.Count, channelMediaIDs(t, older.Messages), len(tc.wantAll), tc.wantAll[1:])
				}
			}
		})
	}
}

func TestSearchChannelOnlyMediaFiltersRejectOtherPeerTypes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, peerUser := createSearchUsers(t, ctx, s)
	outsider, err := s.CreateUser(ctx, "+15551297225")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, viewer.ID, "Search scope", []int64{peerUser.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, peerUser.ID, viewer.ID)
	if err != nil || !removed {
		t.Fatalf("remove group member: removed=%v err=%v", removed, err)
	}
	peers := []struct {
		name   string
		userID int64
		peer   tg.InputPeerClass
	}{
		{"user", viewer.ID, api.InputPeerUser(viewer.ID, peerUser.ID)},
		{"basic group member", viewer.ID, &tg.InputPeerChat{ChatID: chat.ID}},
		{"basic group outsider", outsider.ID, &tg.InputPeerChat{ChatID: chat.ID}},
		{"basic group removed member", peerUser.ID, &tg.InputPeerChat{ChatID: chat.ID}},
	}
	filters := []tg.MessagesFilterClass{
		&tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{},
		&tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{},
	}
	limits := []int{100, 0}
	queries := []string{"", "needle"}
	cfg := store.RateLimitConfig{Limit: 1000, Window: time.Minute}
	for _, peer := range peers {
		for _, filter := range filters {
			for _, limit := range limits {
				for _, query := range queries {
					_, err := api.SearchForTestWithLimits(s, peer.userID, cfg, &tg.MessagesSearchRequest{
						Peer: peer.peer, Q: query, Filter: filter, Limit: limit,
					})
					if err == nil {
						t.Fatalf("%s filter %T query %q limit %d: expected INPUT_FILTER_INVALID", peer.name, filter, query, limit)
					}
					rpcError(t, err, "INPUT_FILTER_INVALID")
				}
			}
		}
	}
}

func TestSearchChannelOnlyMediaFiltersAcceptEmptyChannelResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551297205")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297206")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	ch, err := s.CreateChannel(ctx, creator.ID, "Empty media", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, ch, member.ID)

	filters := []tg.MessagesFilterClass{
		&tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{},
		&tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{},
	}
	for _, filter := range filters {
		for _, limit := range []int{100, 0} {
			enc, err := searchSharedMedia(s, member.ID, channelPeer(member.ID, ch.ID), "", filter, 0, limit)
			if err != nil {
				t.Fatalf("filter %T limit %d: %v", filter, limit, err)
			}
			result := channelMediaSearchResult(t, enc)
			if result.Count != 0 || len(result.Messages) != 0 {
				t.Fatalf("filter %T limit %d count/messages = %d/%d, want 0/0", filter, limit, result.Count, len(result.Messages))
			}
			if len(result.Chats) != 1 || result.Chats[0].GetID() != ch.ID || result.Pts <= 0 {
				t.Fatalf("filter %T limit %d chats/pts = %d/%d, want channel and positive pts", filter, limit, len(result.Chats), result.Pts)
			}
		}
	}
}

func TestSearchChannelOnlyMediaFiltersPreserveMembershipAndQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551297211")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	banned, err := s.CreateUser(ctx, "+15551297212")
	if err != nil {
		t.Fatalf("create banned member: %v", err)
	}
	removed, err := s.CreateUser(ctx, "+15551297213")
	if err != nil {
		t.Fatalf("create removed member: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551297214")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	ch, err := s.CreateChannel(ctx, creator.ID, "Admission", "", true)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := s.EditChannelUsername(ctx, ch.ID, creator.ID, "channelmediaadmission"); err != nil {
		t.Fatalf("make channel public: %v", err)
	}
	joinChannelByInvite(t, s, ch, banned.ID)
	joinChannelByInvite(t, s, ch, removed.ID)
	if err := s.SetChannelBan(ctx, ch.ID, creator.ID, banned.ID, nil, true); err != nil {
		t.Fatalf("ban member: %v", err)
	}
	if left, err := s.LeaveChannel(ctx, ch.ID, removed.ID); err != nil || !left {
		t.Fatalf("remove member: left=%v err=%v", left, err)
	}

	filters := []tg.MessagesFilterClass{
		&tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{},
		&tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{},
	}
	limits := []int{100, 0}
	cfg := store.RateLimitConfig{Limit: 1000, Window: time.Minute}
	for _, filter := range filters {
		for _, limit := range limits {
			for _, viewer := range []store.User{outsider, removed, banned} {
				enc, err := api.SearchForTestWithLimits(s, viewer.ID, cfg, &tg.MessagesSearchRequest{
					Peer: channelPeer(viewer.ID, ch.ID), Q: "", Filter: filter, Limit: limit,
				})
				if enc != nil {
					t.Fatalf("filter %T limit %d viewer %d returned a response %T on rejection", filter, limit, viewer.ID, enc)
				}
				rpcError(t, err, "PEER_ID_INVALID")
			}

			_, err := api.SearchForTest(s, creator.ID, &tg.MessagesSearchRequest{
				Peer: &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: api.DeriveChannelHash(creator.ID+1, ch.ID)},
				Q:    "", Filter: filter, Limit: limit,
			})
			rpcError(t, err, "PEER_ID_INVALID")
		}
	}

	for i, filter := range filters {
		for _, limit := range limits {
			quotaViewer, err := s.CreateUser(ctx, fmt.Sprintf("+155512973%02d", i*2+limit/100))
			if err != nil {
				t.Fatalf("create quota viewer %d: %v", i, err)
			}
			quota := store.RateLimitConfig{Limit: 1, Window: 10 * time.Second}
			probe := func() error {
				_, err := api.SearchForTestWithLimits(s, quotaViewer.ID, quota, &tg.MessagesSearchRequest{
					Peer: channelPeer(quotaViewer.ID, ch.ID), Q: "", Filter: filter, Limit: limit,
				})
				return err
			}
			rpcError(t, probe(), "PEER_ID_INVALID")
			if err := probe(); !isFloodWait(err) {
				t.Fatalf("filter %T limit %d second non-member request = %v, want FLOOD_WAIT", filter, limit, err)
			}
		}
	}
}

func TestSearchChannelOnlyMediaRechecksBanInsideSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, _ := openStoreDSN(t)
	creator, err := s.CreateUser(ctx, "+15551297231")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297232")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	pollMember, err := s.CreateUser(ctx, "+15551297233")
	if err != nil {
		t.Fatalf("create poll member: %v", err)
	}
	ch, err := s.CreateChannel(ctx, creator.ID, "Snapshot", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	joinChannelByInvite(t, s, ch, member.ID)
	joinChannelByInvite(t, s, ch, pollMember.ID)

	type outcome struct {
		response bin.Encoder
		err      error
	}
	for _, tc := range []struct {
		user   store.User
		filter tg.MessagesFilterClass
	}{
		{member, &tg.InputMessagesFilterVideo{}},
		{pollMember, &tg.InputMessagesFilterPoll{}},
	} {
		entered := make(chan struct{})
		release := make(chan struct{})
		var releaseOnce sync.Once
		releaseHook := func() { releaseOnce.Do(func() { close(release) }) }
		store.SetFilteredChannelSearchSnapshotHook(s, func() {
			close(entered)
			<-release
		})
		func() {
			defer func() {
				releaseHook()
				store.SetFilteredChannelSearchSnapshotHook(s, nil)
			}()

			done := make(chan outcome, 1)
			go func() {
				response, err := searchSharedMedia(s, tc.user.ID, channelPeer(tc.user.ID, ch.ID), "", tc.filter, 0, 100)
				done <- outcome{response: response, err: err}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatalf("filter %T did not reach its snapshot barrier", tc.filter)
			}

			if err := s.SetChannelBan(ctx, ch.ID, creator.ID, tc.user.ID, nil, true); err != nil {
				t.Fatalf("ban member between admission and snapshot: %v", err)
			}
			releaseHook()
			select {
			case result := <-done:
				if result.response != nil {
					t.Fatalf("banned filter %T returned response %T", tc.filter, result.response)
				}
				rpcError(t, result.err, "PEER_ID_INVALID")
			case <-time.After(3 * time.Second):
				t.Fatalf("filter %T did not finish after the ban", tc.filter)
			}
		}()
	}
}

func insertChannelSearchFile(t *testing.T, ctx context.Context, dsn string, uploaderID int64, name string, rights []string, stored bool) int64 {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to seed file: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close file seed connection: %v", err)
		}
	}()
	var id int64
	err = conn.QueryRow(ctx, `
		INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored, subtype_rights)
		VALUES ($1, $2, 1, 'application/octet-stream', $3, $4, $5)
		RETURNING id
	`, uploaderID, int64(len(name)+100), name, stored, rights).Scan(&id)
	if err != nil {
		t.Fatalf("insert file %q: %v", name, err)
	}
	return id
}

func channelMediaSearchResult(t *testing.T, enc bin.Encoder) *tg.MessagesChannelMessages {
	t.Helper()
	assertEncodes(t, enc)
	result, ok := enc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("channel media result = %T, want *tg.MessagesChannelMessages", enc)
	}
	return result
}

func channelMediaIDs(t *testing.T, messages []tg.MessageClass) []int64 {
	t.Helper()
	ids := make([]int64, len(messages))
	for i, message := range messages {
		got, ok := message.(*tg.Message)
		if !ok {
			t.Fatalf("channel media message %d = %T, want *tg.Message", i, message)
		}
		ids[i] = int64(got.ID)
	}
	return ids
}

func assertChannelMediaResult(t *testing.T, result *tg.MessagesChannelMessages, want []int64) {
	t.Helper()
	got := channelMediaIDs(t, result.Messages)
	if result.Count != len(want) || !sameIDs(got, want) {
		t.Fatalf("channel media count/ids = %d/%v, want %d/%v", result.Count, got, len(want), want)
	}
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
