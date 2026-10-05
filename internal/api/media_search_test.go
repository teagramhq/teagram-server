package api_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func searchSharedMedia(
	s *store.Store, userID int64, peer tg.InputPeerClass, query string,
	filter tg.MessagesFilterClass, offsetID, limit int,
) (bin.Encoder, error) {
	return api.SearchForTest(s, userID, &tg.MessagesSearchRequest{
		Peer: peer, Q: query, Filter: filter, OffsetID: offsetID, Limit: limit,
	})
}

func sharedMediaSlice(t *testing.T, enc bin.Encoder) *tg.MessagesMessagesSlice {
	t.Helper()
	res, ok := enc.(*tg.MessagesMessagesSlice)
	if !ok {
		t.Fatalf("search result = %T, want *tg.MessagesMessagesSlice", enc)
	}
	return res
}

func sharedMediaMessage(t *testing.T, message tg.MessageClass) *tg.Message {
	t.Helper()
	res, ok := message.(*tg.Message)
	if !ok {
		t.Fatalf("search message = %T, want *tg.Message", message)
	}
	return res
}

func sendSearchDocument(
	t *testing.T, s *store.Store, senderID int64, peer tg.InputPeerClass,
	fileID int64, fileName, caption string, randomID int64,
) {
	t.Helper()
	saveParts(t, s, senderID, fileID, []byte("shared-media-search document"))
	enc, err := api.SendMediaForTest(s, senderID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: peer, Media: uploadedDocument(fileID, 1, fileName, "application/octet-stream"),
		Message: caption, RandomID: randomID,
	})
	if err != nil {
		t.Fatalf("send document %q: %v", caption, err)
	}
	documentOf(t, enc)
}

type sharedMediaSearchPeer struct {
	name    string
	ownerID int64
	peerID  int64
	kind    store.PeerType
	input   tg.InputPeerClass
	sender  int64
}

func TestSearchSharedMediaFiltersAndCountsDialogMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, peer := createSearchUsers(t, ctx, s)
	peerForViewer := api.InputPeerUser(viewer.ID, peer.ID)

	sendSearchDocument(t, s, peer.ID, api.InputPeerUser(peer.ID, viewer.ID), 107401, "contract.pdf", "contract attachment", 107401)
	if _, err := api.SendMessageForTest(s, peer.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(peer.ID, viewer.ID), Message: "review contract at https://example.test/contract", RandomID: 107402,
	}); err != nil {
		t.Fatalf("send URL message: %v", err)
	}
	if _, err := api.SendMessageForTest(s, peer.ID, &tg.MessagesSendMessageRequest{
		Peer: api.InputPeerUser(peer.ID, viewer.ID), Message: "contract discussion", RandomID: 107403,
	}); err != nil {
		t.Fatalf("send plain text message: %v", err)
	}
	sendSearchDocument(t, s, viewer.ID, peerForViewer, 107404, "export.csv", "quarterly export", 107404)

	enc, err := searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterDocument{}, 0, 100)
	if err != nil {
		t.Fatalf("search documents: %v", err)
	}
	result := sharedMediaSlice(t, enc)
	if result.Count != 2 || len(result.Messages) != 2 {
		t.Fatalf("document search count=%d messages=%d, want 2", result.Count, len(result.Messages))
	}
	for i, wantText := range []string{"quarterly export", "contract attachment"} {
		message, ok := result.Messages[i].(*tg.Message)
		if !ok || message.Message != wantText {
			t.Fatalf("document result %d = %T %+v, want %q", i, result.Messages[i], result.Messages[i], wantText)
		}
		media, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			t.Fatalf("document result %q media = %T, want document", wantText, message.Media)
		}
		if _, ok := media.Document.(*tg.Document); !ok {
			t.Fatalf("document result %q payload = %T, want *tg.Document", wantText, media.Document)
		}
		if i > 0 {
			previous := sharedMediaMessage(t, result.Messages[i-1])
			if previous.ID <= message.ID {
				t.Fatalf("document results are not newest-first: previous id %d, current id %d", previous.ID, message.ID)
			}
		}
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterDocument{}, 0, 0)
	if err != nil {
		t.Fatalf("count documents: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 2 || len(result.Messages) != 0 {
		t.Fatalf("count-only document search count=%d messages=%d, want 2 and no messages", result.Count, len(result.Messages))
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterDocument{}, 0, 1)
	if err != nil {
		t.Fatalf("search first document page: %v", err)
	}
	newestDocumentID := resultID(t, sharedMediaSlice(t, enc))
	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterDocument{}, newestDocumentID, 1)
	if err != nil {
		t.Fatalf("page documents: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 2 || len(result.Messages) != 1 {
		t.Fatalf("second document page count=%d messages=%d, want 2 and one", result.Count, len(result.Messages))
	}
	if got := sharedMediaMessage(t, result.Messages[0]).Message; got != "contract attachment" {
		t.Fatalf("second document page message = %q, want contract attachment", got)
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "contract", &tg.InputMessagesFilterDocument{}, 0, 100)
	if err != nil {
		t.Fatalf("search documents by keyword: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 1 || len(result.Messages) != 1 || sharedMediaMessage(t, result.Messages[0]).Message != "contract attachment" {
		t.Fatalf("keyword document search count=%d messages=%v, want only contract attachment", result.Count, result.Messages)
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterURL{}, 0, 100)
	if err != nil {
		t.Fatalf("search URLs: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 1 || len(result.Messages) != 1 || sharedMediaMessage(t, result.Messages[0]).Message != "review contract at https://example.test/contract" {
		t.Fatalf("URL search count=%d messages=%v, want the one link message", result.Count, result.Messages)
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "contract", &tg.InputMessagesFilterURL{}, 0, 100)
	if err != nil {
		t.Fatalf("search URLs by keyword: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 1 || len(result.Messages) != 1 {
		t.Fatalf("keyword URL search count=%d messages=%d, want 1", result.Count, len(result.Messages))
	}
	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "absent", &tg.InputMessagesFilterURL{}, 0, 100)
	if err != nil {
		t.Fatalf("search URLs with no keyword match: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 0 || len(result.Messages) != 0 {
		t.Fatalf("empty URL search count=%d messages=%d, want 0", result.Count, len(result.Messages))
	}

	enc, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterPhotos{}, 0, 100)
	if err != nil {
		t.Fatalf("search photos with no representable photo messages: %v", err)
	}
	result = sharedMediaSlice(t, enc)
	if result.Count != 0 || len(result.Messages) != 0 {
		t.Fatalf("photo search count=%d messages=%d, want an empty result", result.Count, len(result.Messages))
	}

	_, err = searchSharedMedia(s, viewer.ID, &tg.InputPeerUser{
		UserID: peer.ID, AccessHash: api.DeriveUserHash(viewer.ID+1, peer.ID),
	}, "", &tg.InputMessagesFilterDocument{}, 0, 100)
	rpcError(t, err, "PEER_ID_INVALID")

	_, err = searchSharedMedia(s, viewer.ID, peerForViewer, "", &tg.InputMessagesFilterEmpty{}, 0, 100)
	rpcError(t, err, "SEARCH_QUERY_EMPTY")
}

func TestSearchSharedMediaUsesViewerOwnedChatCopies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	users, chat := chatWith(t, s, "+15551297111", "+15551297112")
	creator, viewer := users[0], users[1]
	if _, err := api.SendMessageForTest(s, viewer.ID, &tg.MessagesSendMessageRequest{
		Peer: &tg.InputPeerSelf{}, Message: "advance viewer message IDs", RandomID: 107411,
	}); err != nil {
		t.Fatalf("advance viewer message IDs: %v", err)
	}
	sendSearchDocument(t, s, creator.ID, &tg.InputPeerChat{ChatID: chat.ID}, 107412, "group.pdf", "group attachment", 107412)

	viewerHistory, err := s.History(ctx, viewer.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("read viewer-owned chat copy: %v", err)
	}
	viewerCopy := storedMessageByText(t, viewerHistory, "group attachment")
	creatorHistory, err := s.History(ctx, creator.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("read creator-owned chat copy: %v", err)
	}
	creatorCopy := storedMessageByText(t, creatorHistory, "group attachment")
	if creatorCopy.LocalID == viewerCopy.LocalID {
		t.Fatalf("creator and viewer local IDs both equal %d, want distinct ID spaces", creatorCopy.LocalID)
	}

	enc, err := searchSharedMedia(s, viewer.ID, &tg.InputPeerChat{ChatID: chat.ID}, "", &tg.InputMessagesFilterDocument{}, 0, 100)
	if err != nil {
		t.Fatalf("search group documents: %v", err)
	}
	result := sharedMediaSlice(t, enc)
	if result.Count != 1 || len(result.Messages) != 1 {
		t.Fatalf("group document search count=%d messages=%d, want one viewer-owned copy", result.Count, len(result.Messages))
	}
	message, ok := result.Messages[0].(*tg.Message)
	if !ok || int64(message.ID) != viewerCopy.LocalID {
		t.Fatalf("group search message = %T %+v, want viewer local ID %d", result.Messages[0], result.Messages[0], viewerCopy.LocalID)
	}
}

func TestSearchSharedMediaSubtypeFiltersForUserSelfAndBasicGroup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	viewer, peer := createSearchUsers(t, ctx, s)
	chat, err := s.CreateChat(ctx, viewer.ID, "Media search", []int64{peer.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	emptyViewer, err := s.CreateUser(ctx, "+15551297031")
	if err != nil {
		t.Fatalf("create empty viewer: %v", err)
	}
	emptyPeer, err := s.CreateUser(ctx, "+15551297032")
	if err != nil {
		t.Fatalf("create empty peer: %v", err)
	}
	emptyChat, err := s.CreateChat(ctx, emptyViewer.ID, "Empty media", []int64{emptyPeer.ID})
	if err != nil {
		t.Fatalf("create empty chat: %v", err)
	}

	peers := []sharedMediaSearchPeer{
		{name: "user", ownerID: viewer.ID, peerID: peer.ID, kind: store.PeerTypeUser, input: api.InputPeerUser(viewer.ID, peer.ID), sender: peer.ID},
		{name: "self", ownerID: viewer.ID, peerID: viewer.ID, kind: store.PeerTypeUser, input: &tg.InputPeerSelf{}, sender: viewer.ID},
		{name: "basic group", ownerID: viewer.ID, peerID: chat.ID, kind: store.PeerTypeChat, input: &tg.InputPeerChat{ChatID: chat.ID}, sender: peer.ID},
	}
	emptyPeers := []sharedMediaSearchPeer{
		{name: "empty user", ownerID: emptyViewer.ID, peerID: emptyPeer.ID, kind: store.PeerTypeUser, input: api.InputPeerUser(emptyViewer.ID, emptyPeer.ID)},
		{name: "empty self", ownerID: emptyViewer.ID, peerID: emptyViewer.ID, kind: store.PeerTypeUser, input: &tg.InputPeerSelf{}},
		{name: "empty basic group", ownerID: emptyViewer.ID, peerID: emptyChat.ID, kind: store.PeerTypeChat, input: &tg.InputPeerChat{ChatID: emptyChat.ID}},
	}

	media := []struct {
		name   string
		rights []string
	}{
		{name: "video", rights: []string{"send_videos"}},
		{name: "gif", rights: []string{"send_gifs"}},
		{name: "round-video", rights: []string{"send_roundvideos"}},
		{name: "voice", rights: []string{"send_voices"}},
		{name: "music", rights: []string{"send_audios"}},
	}
	filters := []struct {
		name    string
		filter  tg.MessagesFilterClass
		matches []string
	}{
		{name: "video", filter: &tg.InputMessagesFilterVideo{}, matches: []string{"video"}},
		{name: "gif", filter: &tg.InputMessagesFilterGif{}, matches: []string{"gif"}},
		{name: "poll", filter: &tg.InputMessagesFilterPoll{}, matches: []string{"poll"}},
		{name: "round voice", filter: &tg.InputMessagesFilterRoundVoice{}, matches: []string{"round-video", "voice"}},
		{name: "music", filter: &tg.InputMessagesFilterMusic{}, matches: []string{"music"}},
	}
	texts := make(map[string]map[string]string, len(peers))
	for peerIndex, target := range peers {
		texts[target.name] = make(map[string]string, len(media)+1)
		randID := int64(132500 + peerIndex*100)
		if err := sendSharedMediaSearchSeed(t, ctx, s, target, "ordinary text", 0, nil, randID); err != nil {
			t.Fatalf("seed ordinary text in %s: %v", target.name, err)
		}
		genericID := insertChannelSearchFile(t, ctx, dsn, target.sender, "generic-search.bin", []string{}, true)
		if err := sendSharedMediaSearchSeed(t, ctx, s, target, "generic document", genericID, nil, randID+1); err != nil {
			t.Fatalf("seed generic document in %s: %v", target.name, err)
		}
		for mediaIndex, item := range media {
			caption := fmt.Sprintf("needle %s %s", target.name, item.name)
			texts[target.name][item.name] = caption
			fileID := insertChannelSearchFile(t, ctx, dsn, target.sender, fmt.Sprintf("search-%s-%s.bin", target.name, item.name), item.rights, true)
			if err := sendSharedMediaSearchSeed(t, ctx, s, target, caption, fileID, nil, randID+int64(mediaIndex)+2); err != nil {
				t.Fatalf("seed %s in %s: %v", item.name, target.name, err)
			}
		}
		if target.name == "user" {
			// A self or group poll must not appear in a different 1:1 dialog.
			continue
		}
		caption := fmt.Sprintf("needle %s poll", target.name)
		if target.name == "self" {
			caption = ""
		}
		texts[target.name]["poll"] = caption
		if err := sendSharedMediaSearchSeed(t, ctx, s, target, caption, 0, &store.PollDraft{
			Question: []byte("Pick one"),
			Answers:  []store.PollAnswer{{Option: []byte("a"), Text: []byte("A")}, {Option: []byte("b"), Text: []byte("B")}},
		}, randID+10); err != nil {
			t.Fatalf("seed poll in %s: %v", target.name, err)
		}
	}

	for _, target := range peers {
		for _, tc := range filters {
			t.Run(target.name+"/"+tc.name, func(t *testing.T) {
				enc, err := searchSharedMedia(s, target.ownerID, target.input, "", tc.filter, 0, 100)
				if err != nil {
					t.Fatalf("search: %v", err)
				}
				result := sharedMediaSlice(t, enc)
				matches := tc.matches
				if target.name == "user" && tc.name == "poll" {
					matches = nil
				}
				want := make(map[string]bool, len(matches))
				for _, name := range matches {
					want[texts[target.name][name]] = true
				}
				if result.Count != len(want) || len(result.Messages) != len(want) {
					t.Fatalf("count/messages = %d/%d, want %d/%d", result.Count, len(result.Messages), len(want), len(want))
				}
				for _, class := range result.Messages {
					message := sharedMediaMessage(t, class)
					if !want[message.Message] {
						t.Errorf("unexpected %s result %q", tc.name, message.Message)
					}
					delete(want, message.Message)
					if tc.name == "poll" {
						if _, ok := message.Media.(*tg.MessageMediaPoll); !ok {
							t.Errorf("poll result media = %T, want *tg.MessageMediaPoll", message.Media)
						}
					} else if _, ok := message.Media.(*tg.MessageMediaDocument); !ok {
						t.Errorf("media result = %T, want generic document", message.Media)
					}
				}
				if len(want) != 0 {
					t.Errorf("missing %s result(s): %v", tc.name, want)
				}

				enc, err = searchSharedMedia(s, target.ownerID, target.input, "", tc.filter, 0, 0)
				if err != nil {
					t.Fatalf("count-only search: %v", err)
				}
				countOnly := sharedMediaSlice(t, enc)
				if countOnly.Count != len(matches) || len(countOnly.Messages) != 0 {
					t.Fatalf("count-only count/messages = %d/%d, want %d/0", countOnly.Count, len(countOnly.Messages), len(matches))
				}

				enc, err = searchSharedMedia(s, target.ownerID, target.input, "absent", tc.filter, 0, 100)
				if err != nil {
					t.Fatalf("empty search: %v", err)
				}
				empty := sharedMediaSlice(t, enc)
				if empty.Count != 0 || len(empty.Messages) != 0 {
					t.Fatalf("empty search count/messages = %d/%d, want 0/0", empty.Count, len(empty.Messages))
				}
			})
		}
	}

	for _, target := range emptyPeers {
		for _, tc := range filters {
			for _, limit := range []int{100, 0} {
				enc, err := searchSharedMedia(s, target.ownerID, target.input, "", tc.filter, 0, limit)
				if err != nil {
					t.Fatalf("empty %s %s search with limit %d: %v", target.name, tc.name, limit, err)
				}
				result := sharedMediaSlice(t, enc)
				if result.Count != 0 || len(result.Messages) != 0 {
					t.Fatalf("empty %s %s search with limit %d count/messages = %d/%d, want 0/0", target.name, tc.name, limit, result.Count, len(result.Messages))
				}
			}
		}
	}
}

func TestSearchSharedMediaSubtypeFiltersPreservePeerAccessAndQuota(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	viewer, peer := createSearchUsers(t, ctx, s)
	outsider, err := s.CreateUser(ctx, "+15551297041")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, viewer.ID, "Media access", []int64{peer.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	removed, _, _, err := s.RemoveChatUser(ctx, chat.ID, peer.ID, viewer.ID)
	if err != nil || !removed {
		t.Fatalf("remove group member: removed=%v err=%v", removed, err)
	}
	filters := []tg.MessagesFilterClass{
		&tg.InputMessagesFilterVideo{}, &tg.InputMessagesFilterGif{}, &tg.InputMessagesFilterPoll{},
		&tg.InputMessagesFilterRoundVoice{}, &tg.InputMessagesFilterMusic{},
	}
	for _, filter := range filters {
		for _, limit := range []int{100, 0} {
			_, err := searchSharedMedia(s, viewer.ID, &tg.InputPeerUser{
				UserID: peer.ID, AccessHash: api.DeriveUserHash(viewer.ID+1, peer.ID),
			}, "", filter, 0, limit)
			rpcError(t, err, "PEER_ID_INVALID")

			for _, user := range []store.User{outsider, peer} {
				enc, err := searchSharedMedia(s, user.ID, &tg.InputPeerChat{ChatID: chat.ID}, "", filter, 0, limit)
				if enc != nil {
					t.Fatalf("filter %T limit %d user %d returned %T on group access rejection", filter, limit, user.ID, enc)
				}
				rpcError(t, err, "PEER_ID_INVALID")
			}
		}
	}

	for i, filter := range filters {
		for _, limit := range []int{100, 0} {
			quotaViewer, err := s.CreateUser(ctx, fmt.Sprintf("+1555129705%02d", i*2+limit/100))
			if err != nil {
				t.Fatalf("create quota viewer %d: %v", i, err)
			}
			quota := store.RateLimitConfig{Limit: 1, Window: 10 * time.Second}
			probe := func() error {
				_, err := api.SearchForTestWithLimits(s, quotaViewer.ID, quota, &tg.MessagesSearchRequest{
					Peer: &tg.InputPeerChat{ChatID: chat.ID}, Q: "", Filter: filter, Limit: limit,
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

func sendSharedMediaSearchSeed(
	t *testing.T,
	ctx context.Context,
	s *store.Store,
	target sharedMediaSearchPeer,
	text string,
	fileID int64,
	draft *store.PollDraft,
	randomID int64,
) error {
	t.Helper()
	if draft == nil {
		if target.kind == store.PeerTypeChat {
			_, _, duplicate, err := s.SendChatMessage(ctx, store.FanOut{
				ChatID: target.peerID, FromID: target.sender, Text: text, RandomID: randomID, FileID: fileID,
			})
			if err != nil {
				return err
			}
			if duplicate {
				return fmt.Errorf("chat seed %d was unexpectedly deduplicated", randomID)
			}
			return nil
		}
		_, _, _, duplicate, err := s.SendMessage(ctx, target.sender, target.ownerID, text, randomID, fileID, 0)
		if err != nil {
			return err
		}
		if duplicate {
			return fmt.Errorf("dialog seed %d was unexpectedly deduplicated", randomID)
		}
		return nil
	}

	switch target.kind {
	case store.PeerTypeChat:
		_, _, _, duplicate, err := s.SendChatPollMessage(ctx, store.FanOut{
			ChatID: target.peerID, FromID: target.sender, Text: text, RandomID: randomID,
		}, *draft)
		if err != nil {
			return err
		}
		if duplicate {
			return fmt.Errorf("chat poll seed %d was unexpectedly deduplicated", randomID)
		}
		return nil
	case store.PeerTypeUser:
		if target.sender == target.ownerID {
			_, _, _, duplicate, err := s.SendSavedPollMessage(ctx, target.ownerID, randomID, *draft)
			if err != nil {
				return err
			}
			if duplicate {
				return fmt.Errorf("saved poll seed %d was unexpectedly deduplicated", randomID)
			}
			return nil
		}
		return errors.New("poll seed in a private user peer is unsupported")
	default:
		return fmt.Errorf("unsupported seed peer type %d", target.kind)
	}
}

func TestSearchSharedMediaChannelCountsAndRejectsUnauthorizedViewers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551297121")
	if err != nil {
		t.Fatalf("create channel creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551297122")
	if err != nil {
		t.Fatalf("create channel member: %v", err)
	}
	banned, err := s.CreateUser(ctx, "+15551297123")
	if err != nil {
		t.Fatalf("create banned viewer: %v", err)
	}
	departed, err := s.CreateUser(ctx, "+15551297124")
	if err != nil {
		t.Fatalf("create departed viewer: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551297125")
	if err != nil {
		t.Fatalf("create public non-member: %v", err)
	}
	quotaViewer, err := s.CreateUser(ctx, "+15551297126")
	if err != nil {
		t.Fatalf("create quota probe: %v", err)
	}
	ch, err := s.CreateChannel(ctx, creator.ID, "Shared Media", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	if err := s.EditChannelUsername(ctx, ch.ID, creator.ID, "sharedmediasearch"); err != nil {
		t.Fatalf("make channel public: %v", err)
	}
	for _, user := range []store.User{member, banned, departed} {
		joinChannelByInvite(t, s, ch, user.ID)
	}
	if _, err := sendToChannel(t, s, creator.ID, ch.ID, "channel link https://example.test/channel", 107421); err != nil {
		t.Fatalf("send channel link: %v", err)
	}
	if err := s.SetChannelBan(ctx, ch.ID, creator.ID, banned.ID, nil, true); err != nil {
		t.Fatalf("ban channel viewer: %v", err)
	}
	if left, err := s.LeaveChannel(ctx, ch.ID, departed.ID); err != nil || !left {
		t.Fatalf("remove departed viewer: left=%v err=%v", left, err)
	}

	peer := channelPeer(member.ID, ch.ID)
	enc, err := searchSharedMedia(s, member.ID, peer, "", &tg.InputMessagesFilterURL{}, 0, 100)
	if err != nil {
		t.Fatalf("search channel URLs: %v", err)
	}
	result, ok := enc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("channel URL result = %T, want *tg.MessagesChannelMessages", enc)
	}
	if result.Count != 1 || len(result.Messages) != 1 {
		t.Fatalf("channel URL result = %T count/messages %d/%d, want one", enc, result.Count, len(result.Messages))
	}
	message, ok := result.Messages[0].(*tg.Message)
	if !ok || message.Message != "channel link https://example.test/channel" {
		t.Fatalf("channel URL message = %T %+v, want the channel link", result.Messages[0], result.Messages[0])
	}

	enc, err = searchSharedMedia(s, member.ID, peer, "", &tg.InputMessagesFilterURL{}, 0, 0)
	if err != nil {
		t.Fatalf("count channel URLs: %v", err)
	}
	result, ok = enc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("channel URL count-only result = %T, want *tg.MessagesChannelMessages", enc)
	}
	if result.Count != 1 || len(result.Messages) != 0 {
		t.Fatalf("channel URL count-only result = %T count/messages %d/%d, want 1/0", enc, result.Count, len(result.Messages))
	}
	enc, err = searchSharedMedia(s, member.ID, peer, "", &tg.InputMessagesFilterDocument{}, 0, 100)
	if err != nil {
		t.Fatalf("search channel documents with no representable matches: %v", err)
	}
	result, ok = enc.(*tg.MessagesChannelMessages)
	if !ok {
		t.Fatalf("channel document result = %T, want *tg.MessagesChannelMessages", enc)
	}
	if result.Count != 0 || len(result.Messages) != 0 {
		t.Fatalf("channel document search count/messages = %d/%d, want 0/0", result.Count, len(result.Messages))
	}

	for _, viewer := range []store.User{banned, departed, outsider} {
		_, err := searchSharedMedia(s, viewer.ID, channelPeer(viewer.ID, ch.ID), "", &tg.InputMessagesFilterURL{}, 0, 100)
		rpcError(t, err, "PEER_ID_INVALID")
	}

	quota := store.RateLimitConfig{Limit: 1, Window: 10 * time.Second}
	probe := func() error {
		_, err := api.SearchForTestWithLimits(s, quotaViewer.ID, quota, &tg.MessagesSearchRequest{
			Peer: channelPeer(quotaViewer.ID, ch.ID), Q: "", Filter: &tg.InputMessagesFilterURL{}, Limit: 100,
		})
		return err
	}
	rpcError(t, probe(), "PEER_ID_INVALID")
	if err := probe(); !isFloodWait(err) {
		t.Fatalf("second public non-member search error = %v, want FLOOD_WAIT after first request was charged", err)
	}
}

func createSearchUsers(t *testing.T, ctx context.Context, s *store.Store) (store.User, store.User) {
	t.Helper()
	viewer, err := s.CreateUser(ctx, "+15551297101")
	if err != nil {
		t.Fatalf("create search viewer: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551297102")
	if err != nil {
		t.Fatalf("create search peer: %v", err)
	}
	return viewer, peer
}

func resultID(t *testing.T, result *tg.MessagesMessagesSlice) int {
	t.Helper()
	if len(result.Messages) != 1 {
		t.Fatalf("search returned %d messages, want one", len(result.Messages))
	}
	message, ok := result.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("search message = %T, want *tg.Message", result.Messages[0])
	}
	return message.ID
}
