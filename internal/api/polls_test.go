package api_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSendPollInBasicChatReturnsCanonicalViewerPoll(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401001")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401002")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Polls", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	poll := tg.Poll{
		Question: tg.TextWithEntities{Text: "Choose two"},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "Pizza"}, Option: []byte("pizza")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "Sushi"}, Option: []byte("sushi")},
		},
	}
	poll.SetPublicVoters(true)
	poll.SetMultipleChoice(true)
	poll.SetQuiz(true)
	poll.SetOpenAnswers(true)
	poll.SetShuffleAnswers(true)
	media := &tg.InputMediaPoll{Poll: poll, CorrectAnswers: []int{0, 1}}
	media.SetCorrectAnswers([]int{0, 1})

	result, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerChat(creator.ID, chat.ID),
		Media:    media,
		RandomID: 1401001,
	})
	if err != nil {
		t.Fatalf("send poll: %v", err)
	}
	created := messageOf(t, result)
	creatorMedia, ok := created.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("creator media = %T, want messageMediaPoll", created.Media)
	}
	if !creatorMedia.Poll.Creator || !creatorMedia.Poll.Quiz || !creatorMedia.Poll.MultipleChoice || !creatorMedia.Poll.PublicVoters || !creatorMedia.Poll.ShuffleAnswers {
		t.Fatalf("creator poll flags = %+v", creatorMedia.Poll)
	}
	if creatorMedia.Poll.OpenAnswers {
		t.Fatal("creator poll echo retained open_answers")
	}
	if len(creatorMedia.Results.Results) != 2 || creatorMedia.Results.Results[0].Correct || creatorMedia.Results.Results[1].Correct {
		t.Fatalf("creator's initial answer key leaked: %+v", creatorMedia.Results.Results)
	}

	history, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer:  api.InputPeerChat(member.ID, chat.ID),
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("member history: %v", err)
	}
	messages, ok := history.(*tg.MessagesMessages)
	if !ok || len(messages.Messages) != 1 {
		t.Fatalf("member history = %T with %d messages, want one message", history, len(messages.Messages))
	}
	memberMessage, ok := messages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("member message = %T, want *tg.Message", messages.Messages[0])
	}
	memberMedia, ok := memberMessage.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("member media = %T, want messageMediaPoll", memberMessage.Media)
	}
	if memberMedia.Poll.Creator || memberMedia.Poll.OpenAnswers {
		t.Fatalf("member poll flags = creator:%v open_answers:%v", memberMedia.Poll.Creator, memberMedia.Poll.OpenAnswers)
	}
	if memberMedia.Results.Results[0].Correct || memberMedia.Results.Results[1].Correct {
		t.Fatalf("non-voter saw quiz answer key: %+v", memberMedia.Results.Results)
	}
}

func TestSendPollInSavedMessages(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401003")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}

	result, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     &tg.InputPeerSelf{},
		Media:    fixedPollMedia("Saved question?", "A", "B"),
		RandomID: 1401002,
	})
	if err != nil {
		t.Fatalf("send Saved Messages poll: %v", err)
	}
	message := messageOf(t, result)
	if _, ok := message.Media.(*tg.MessageMediaPoll); !ok {
		t.Fatalf("Saved Messages media = %T, want messageMediaPoll", message.Media)
	}
}

func TestPollReadSurfacesHydrateViewerScopedMedia(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401008")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401009")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll surfaces", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	memberState, err := s.State(ctx, member.ID)
	if err != nil {
		t.Fatalf("member state before poll: %v", err)
	}
	media := fixedPollMedia("Which answer?", "A", "B")
	media.Poll.SetQuiz(true)
	media.SetCorrectAnswers([]int{0})
	media.CorrectAnswers = []int{0}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401008,
	})
	if err != nil {
		t.Fatalf("send quiz poll: %v", err)
	}
	creatorMessage := messageOf(t, sent)
	creatorPoll := assertPollMessageForViewer(t, []tg.MessageClass{creatorMessage}, creatorMessage.ID, true)
	if creatorPoll.Results.Results[0].Correct {
		t.Fatal("creator initial poll response revealed the answer key")
	}
	memberHistory, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("member history: %v", err)
	}
	historyMessages, ok := memberHistory.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) == 0 {
		t.Fatalf("member history = %T, want messages containing poll", memberHistory)
	}
	memberMessage := findPollMessage(t, historyMessages.Messages)
	memberPoll := assertPollMessageForViewer(t, historyMessages.Messages, memberMessage.ID, false)
	if memberPoll.Results.Results[0].Correct {
		t.Fatal("member non-voter history revealed the answer key")
	}

	dialogs, err := api.GetDialogsForTest(s, member.ID)
	if err != nil {
		t.Fatalf("getDialogs: %v", err)
	}
	assertPollMessageForViewer(t, messagesFromDialogResponse(t, dialogs), memberMessage.ID, false)
	peerDialogs, err := api.GetPeerDialogsForTest(s, member.ID, &tg.MessagesGetPeerDialogsRequest{
		Peers: []tg.InputDialogPeerClass{&tg.InputDialogPeer{Peer: api.InputPeerChat(member.ID, chat.ID)}},
	})
	if err != nil {
		t.Fatalf("getPeerDialogs: %v", err)
	}
	assertPollMessageForViewer(t, messagesFromDialogResponse(t, peerDialogs), memberMessage.ID, false)

	if _, err = api.UpdatePinnedMessageForTest(s, creator.ID, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), ID: creatorMessage.ID,
	}); err != nil {
		t.Fatalf("pin poll: %v", err)
	}
	search, err := searchPinned(s, member.ID, api.InputPeerChat(member.ID, chat.ID), 0, 10)
	if err != nil {
		t.Fatalf("search pinned poll: %v", err)
	}
	assertPollMessageForViewer(t, pinnedDialogMessages(t, search).Messages, memberMessage.ID, false)

	difference, err := api.GetDifferenceForTest(s, member.ID, &tg.UpdatesGetDifferenceRequest{Pts: memberState.Pts})
	if err != nil {
		t.Fatalf("getDifference: %v", err)
	}
	diff, ok := difference.(*tg.UpdatesDifference)
	if !ok {
		t.Fatalf("getDifference = %T, want *tg.UpdatesDifference", difference)
	}
	assertPollMessageForViewer(t, diff.NewMessages, memberMessage.ID, false)
}

func messagesFromDialogResponse(t *testing.T, result any) []tg.MessageClass {
	t.Helper()
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		return response.Messages
	case *tg.MessagesDialogsSlice:
		return response.Messages
	case *tg.MessagesPeerDialogs:
		return response.Messages
	default:
		t.Fatalf("dialog response = %T, want dialogs with messages", result)
		return nil
	}
}

func findPollMessage(t *testing.T, messages []tg.MessageClass) *tg.Message {
	t.Helper()
	for _, message := range messages {
		if msg, ok := message.(*tg.Message); ok {
			if _, ok := msg.Media.(*tg.MessageMediaPoll); ok {
				return msg
			}
		}
	}
	t.Fatal("messages omitted poll media")
	return nil
}

func firstMessageFromResult(t *testing.T, result any) *tg.Message {
	t.Helper()
	response, ok := result.(*tg.MessagesMessages)
	if !ok || len(response.Messages) == 0 {
		t.Fatalf("message response = %T, want nonempty *tg.MessagesMessages", result)
	}
	message, ok := response.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("first message = %T, want *tg.Message", response.Messages[0])
	}
	return message
}

func votesListFromResult(t *testing.T, result any) *tg.MessagesVotesList {
	t.Helper()
	response, ok := result.(*tg.MessagesVotesList)
	if !ok {
		t.Fatalf("votes response = %T, want *tg.MessagesVotesList", result)
	}
	return response
}

func assertPollMessageForViewer(t *testing.T, messages []tg.MessageClass, id int, creator bool) *tg.MessageMediaPoll {
	t.Helper()
	for _, message := range messages {
		msg, ok := message.(*tg.Message)
		if !ok || msg.ID != id {
			continue
		}
		media, ok := msg.Media.(*tg.MessageMediaPoll)
		if !ok || media.Poll.Creator != creator {
			t.Fatalf("message %d media = %#v, want poll with creator=%v", id, msg.Media, creator)
		}
		return media
	}
	t.Fatalf("messages omitted poll message %d", id)
	return nil
}

func TestPollVoteAndGetResultsKeepQuizKeysViewerScoped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401031")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401032")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Polls", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	poll := tg.Poll{Question: tg.TextWithEntities{Text: "Choose the correct answers"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("a")},
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("b")},
	}}
	poll.SetQuiz(true)
	poll.SetMultipleChoice(true)
	media := &tg.InputMediaPoll{Poll: poll, CorrectAnswers: []int{0}}
	media.SetCorrectAnswers([]int{0})
	media.SetSolution("A is correct")
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401031,
	})
	if err != nil {
		t.Fatalf("send quiz poll: %v", err)
	}
	created := messageOf(t, sent)
	memberHistory, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("member history: %v", err)
	}
	memberMessage := firstMessageFromResult(t, memberHistory)

	creatorBefore, err := s.State(ctx, creator.ID)
	if err != nil {
		t.Fatalf("creator state: %v", err)
	}
	creatorResults, err := api.GetPollResultsForTest(s, creator.ID, &tg.MessagesGetPollResultsRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), MsgID: created.ID, PollHash: 99,
	})
	if err != nil {
		t.Fatalf("creator getPollResults: %v", err)
	}
	creatorUpdate := pollUpdateFromResponse(t, creatorResults)
	if creatorUpdate.PollID == 0 || len(creatorUpdate.Results.Results) != 2 || creatorUpdate.Results.Results[0].Correct || creatorUpdate.Results.Solution != "" {
		t.Fatalf("non-voter result update disclosed quiz key: %+v", creatorUpdate)
	}
	creatorAfter, err := s.State(ctx, creator.ID)
	if err != nil || creatorAfter.Pts != creatorBefore.Pts {
		t.Fatalf("creator getPollResults changed pts %d -> %d, err %v", creatorBefore.Pts, creatorAfter.Pts, err)
	}

	voted, err := api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID, Options: [][]byte{[]byte("a")},
	})
	if err != nil {
		t.Fatalf("cast quiz vote: %v", err)
	}
	voteUpdate := pollUpdateFromResponse(t, voted)
	if voteUpdate.Peer != nil || voteUpdate.MsgID != 0 || !voteUpdate.Poll.Zero() || !voteUpdate.Results.Results[0].Chosen || !voteUpdate.Results.Results[0].Correct {
		t.Fatalf("vote response = %+v; want results without a peer/message/poll and voter-scoped key", voteUpdate)
	}

	creatorResults, err = api.GetPollResultsForTest(s, creator.ID, &tg.MessagesGetPollResultsRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), MsgID: created.ID,
	})
	if err != nil {
		t.Fatalf("creator getPollResults after member vote: %v", err)
	}
	creatorUpdate = pollUpdateFromResponse(t, creatorResults)
	if creatorUpdate.Results.Results[0].Correct || creatorUpdate.Results.Solution != "" {
		t.Fatalf("non-voter creator result update disclosed answer key: %+v", creatorUpdate.Results)
	}
	memberResults, err := api.GetPollResultsForTest(s, member.ID, &tg.MessagesGetPollResultsRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID,
	})
	if err != nil {
		t.Fatalf("voter getPollResults: %v", err)
	}
	memberUpdate := pollUpdateFromResponse(t, memberResults)
	if !memberUpdate.Results.Results[0].Chosen || !memberUpdate.Results.Results[0].Correct || memberUpdate.Results.Solution == "" {
		t.Fatalf("voter result update = %+v; want chosen answer, key, and solution", memberUpdate.Results)
	}

	memberBefore, err := s.State(ctx, member.ID)
	if err != nil {
		t.Fatalf("member state: %v", err)
	}
	if _, err = api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID, Options: [][]byte{[]byte("b")},
	}); err == nil || !strings.Contains(err.Error(), "REVOTE_NOT_ALLOWED") {
		t.Fatalf("changed quiz vote = %v, want REVOTE_NOT_ALLOWED", err)
	}
	if _, err = api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID,
	}); err == nil || !strings.Contains(err.Error(), "REVOTE_NOT_ALLOWED") {
		t.Fatalf("quiz vote retraction = %v, want REVOTE_NOT_ALLOWED", err)
	}
	if _, err = api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID, Options: [][]byte{[]byte("a")},
	}); err != nil {
		t.Fatalf("identical quiz vote retry: %v", err)
	}
	memberAfter, err := s.State(ctx, member.ID)
	if err != nil || memberAfter.Pts != memberBefore.Pts {
		t.Fatalf("vote RPCs changed member pts %d -> %d, err %v", memberBefore.Pts, memberAfter.Pts, err)
	}
}

func TestPollVoteRateLimitRejectsWithoutChangingVote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401041")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401042")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll limits", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: fixedPollMedia("Pick one", "A", "B"), RandomID: 1401041,
	})
	if err != nil {
		t.Fatalf("send poll: %v", err)
	}
	created := messageOf(t, sent)
	memberHistory, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{Peer: api.InputPeerChat(member.ID, chat.ID), Limit: 10})
	if err != nil {
		t.Fatalf("member history: %v", err)
	}
	memberMessage := firstMessageFromResult(t, memberHistory)
	limit := store.RateLimitConfig{Limit: 1, Window: time.Minute}
	for i, option := range [][]byte{[]byte("first"), []byte("second")} {
		_, err = api.SendVoteForTestWithLimits(s, member.ID, limit, &tg.MessagesSendVoteRequest{
			Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: memberMessage.ID, Options: [][]byte{option},
		})
		if i == 0 && err != nil {
			t.Fatalf("first vote: %v", err)
		}
		if i == 1 && (err == nil || !strings.Contains(err.Error(), "FLOOD_WAIT")) {
			t.Fatalf("second vote = %v, want FLOOD_WAIT", err)
		}
	}
	view, err := s.PollForMessage(ctx, member.ID, store.PollMessageRef{
		PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: int64(memberMessage.ID),
	})
	if err != nil {
		t.Fatalf("load vote after rate limit: %v", err)
	}
	if !view.Answers[0].Chosen || view.Answers[1].Chosen {
		t.Fatalf("stored selection after rate limit = %+v, want first option only", view.Answers)
	}
	if created.ID != memberMessage.ID {
		t.Fatalf("member message id = %d, want sender id %d", memberMessage.ID, created.ID)
	}
}

func TestClosingPollIgnoresSuppliedMetadataAndCreatesDurableEdit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401051")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401052")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll close", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	poll := tg.Poll{Question: tg.TextWithEntities{Text: "Original question?"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("a")},
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("b")},
	}}
	poll.SetQuiz(true)
	media := &tg.InputMediaPoll{Poll: poll, CorrectAnswers: []int{0}}
	media.SetCorrectAnswers([]int{0})
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401051,
	})
	if err != nil {
		t.Fatalf("send quiz: %v", err)
	}
	created := messageOf(t, sent)
	creatorBefore, err := s.State(ctx, creator.ID)
	if err != nil {
		t.Fatalf("creator state: %v", err)
	}
	memberBefore, err := s.State(ctx, member.ID)
	if err != nil {
		t.Fatalf("member state: %v", err)
	}
	spoofedPoll := tg.Poll{ID: 42, Question: tg.TextWithEntities{Text: "Tampered?"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "Tampered"}, Option: []byte("evil")},
	}}
	spoofedPoll.SetClosed(true)
	spoofedPoll.SetHideResultsUntilClose(true)
	spoofedPoll.SetQuiz(true)
	spoofedMedia := &tg.InputMediaPoll{Poll: spoofedPoll, CorrectAnswers: []int{99}, Solution: "Tampered solution"}
	spoofedMedia.SetCorrectAnswers([]int{99})
	spoofedMedia.SetSolution("Tampered solution")
	closed, err := api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), ID: created.ID, Message: "replace the question",
		Media: spoofedMedia,
	})
	if err != nil {
		t.Fatalf("close poll: %v", err)
	}
	updates, ok := closed.(*tg.Updates)
	if !ok || len(updates.Updates) != 1 {
		t.Fatalf("close response = %T with %d updates, want one durable edit", closed, len(updates.Updates))
	}
	edit, ok := updates.Updates[0].(*tg.UpdateEditMessage)
	if !ok {
		t.Fatalf("close update = %T, want updateEditMessage", updates.Updates[0])
	}
	closedMessage, ok := edit.Message.(*tg.Message)
	if !ok {
		t.Fatalf("closed message = %T, want *tg.Message", edit.Message)
	}
	closedMedia, ok := closedMessage.Media.(*tg.MessageMediaPoll)
	if !ok || !closedMedia.Poll.Closed || closedMedia.Poll.ID == 42 || closedMedia.Poll.Question.Text != "Original question?" || len(closedMedia.Poll.Answers) != 2 {
		t.Fatalf("closed poll = %#v, want original canonical poll closed", closedMessage.Media)
	}
	if !closedMedia.Results.Results[0].Correct || closedMedia.Results.Results[1].Correct {
		t.Fatalf("closed quiz answer key = %+v, want canonical key", closedMedia.Results.Results)
	}
	creatorAfter, err := s.State(ctx, creator.ID)
	if err != nil || creatorAfter.Pts != creatorBefore.Pts+1 {
		t.Fatalf("creator pts after close = %d, err %v; want %d", creatorAfter.Pts, err, creatorBefore.Pts+1)
	}
	memberAfter, err := s.State(ctx, member.ID)
	if err != nil || memberAfter.Pts != memberBefore.Pts+1 {
		t.Fatalf("member pts after close = %d, err %v; want %d", memberAfter.Pts, err, memberBefore.Pts+1)
	}
	memberHistory, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{Peer: api.InputPeerChat(member.ID, chat.ID), Limit: 10})
	if err != nil {
		t.Fatalf("member history after close: %v", err)
	}
	memberMessage := firstMessageFromResult(t, memberHistory)
	memberMedia, ok := memberMessage.Media.(*tg.MessageMediaPoll)
	if !ok || !memberMedia.Poll.Closed || !memberMedia.Results.Results[0].Correct {
		t.Fatalf("member poll after close = %#v, want closed poll with key", memberMessage.Media)
	}

	if _, err = api.EditMessageForTest(s, member.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), ID: memberMessage.ID, Media: spoofedMedia,
	}); err == nil {
		t.Fatal("non-creator closed poll")
	}
	creatorAfterRetry, err := s.State(ctx, creator.ID)
	if err != nil || creatorAfterRetry.Pts != creatorAfter.Pts {
		t.Fatalf("repeated close changed creator pts to %d, err %v", creatorAfterRetry.Pts, err)
	}
}

func TestPublicPollVoterListRequiresVotePaginatesAndHidesPhone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401061")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	firstVoter, err := s.CreateUser(ctx, "+15551401062")
	if err != nil {
		t.Fatalf("create first voter: %v", err)
	}
	secondVoter, err := s.CreateUser(ctx, "+15551401063")
	if err != nil {
		t.Fatalf("create second voter: %v", err)
	}
	thirdVoter, err := s.CreateUser(ctx, "+15551401064")
	if err != nil {
		t.Fatalf("create third voter: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Public poll", []int64{firstVoter.ID, secondVoter.ID, thirdVoter.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	poll := fixedPollMedia("Who voted?", "A", "B")
	poll.Poll.SetPublicVoters(true)
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: poll, RandomID: 1401061,
	})
	if err != nil {
		t.Fatalf("send public poll: %v", err)
	}
	creatorMessage := messageOf(t, sent)
	memberMessageIDs := map[int64]int{}
	for _, user := range []struct {
		id int64
	}{{firstVoter.ID}, {secondVoter.ID}, {thirdVoter.ID}} {
		history, historyErr := api.GetHistoryForTest(s, user.id, &tg.MessagesGetHistoryRequest{
			Peer: api.InputPeerChat(user.id, chat.ID), Limit: 10,
		})
		if historyErr != nil {
			t.Fatalf("history for voter %d: %v", user.id, historyErr)
		}
		memberMessageIDs[user.id] = firstMessageFromResult(t, history).ID
	}
	requestFor := func(userID int64, offset string, limit int) *tg.MessagesGetPollVotesRequest {
		req := &tg.MessagesGetPollVotesRequest{
			Peer: api.InputPeerChat(userID, chat.ID), ID: creatorMessage.ID, Limit: limit,
		}
		if offset != "" {
			req.SetOffset(offset)
		}
		return req
	}
	if _, err = api.GetPollVotesForTest(s, creator.ID, requestFor(creator.ID, "", 10)); err == nil || !strings.Contains(err.Error(), "POLL_VOTE_REQUIRED") {
		t.Fatalf("non-voter creator voter-list request = %v, want POLL_VOTE_REQUIRED", err)
	}
	if _, err = api.SendVoteForTest(s, firstVoter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(firstVoter.ID, chat.ID), MsgID: memberMessageIDs[firstVoter.ID], Options: [][]byte{[]byte("first")},
	}); err != nil {
		t.Fatalf("first vote: %v", err)
	}
	if _, err = api.SendVoteForTest(s, secondVoter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(secondVoter.ID, chat.ID), MsgID: memberMessageIDs[secondVoter.ID], Options: [][]byte{[]byte("second")},
	}); err != nil {
		t.Fatalf("second vote: %v", err)
	}
	if _, err = api.SendVoteForTest(s, thirdVoter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(thirdVoter.ID, chat.ID), MsgID: memberMessageIDs[thirdVoter.ID], Options: [][]byte{[]byte("first")},
	}); err != nil {
		t.Fatalf("third vote: %v", err)
	}
	if _, err = api.SendVoteForTest(s, creator.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), MsgID: creatorMessage.ID, Options: [][]byte{[]byte("first")},
	}); err != nil {
		t.Fatalf("creator vote: %v", err)
	}

	firstPageEnc, err := api.GetPollVotesForTest(s, firstVoter.ID, requestFor(firstVoter.ID, "", 2))
	if err != nil {
		t.Fatalf("first public voter page: %v", err)
	}
	firstPage := votesListFromResult(t, firstPageEnc)
	if firstPage.Count != 4 || len(firstPage.Votes) != 2 || firstPage.NextOffset == "" {
		t.Fatalf("first voter page = count %d votes %d next %q, want 4/2/nonempty", firstPage.Count, len(firstPage.Votes), firstPage.NextOffset)
	}
	firstIDs := voterIDs(t, firstPage.Votes)
	for _, id := range firstIDs {
		if id == firstVoter.ID {
			continue
		}
		if profile, ok := loadUsersWire(t, firstPage.Users, id).(*tg.User); ok && profile.Phone != "" {
			t.Errorf("voter %d phone = %q, want withheld", id, profile.Phone)
		}
	}
	secondPageEnc, err := api.GetPollVotesForTest(s, firstVoter.ID, requestFor(firstVoter.ID, firstPage.NextOffset, 2))
	if err != nil {
		t.Fatalf("second public voter page: %v", err)
	}
	secondPage := votesListFromResult(t, secondPageEnc)
	if secondPage.Count != 4 || len(secondPage.Votes) != 2 || secondPage.NextOffset != "" {
		t.Fatalf("second voter page = count %d votes %d next %q, want 4/2/empty", secondPage.Count, len(secondPage.Votes), secondPage.NextOffset)
	}
	secondIDs := voterIDs(t, secondPage.Votes)
	for _, id := range secondIDs {
		if slices.Contains(firstIDs, id) {
			t.Errorf("voter %d repeated across cursor pages", id)
		}
	}

	filterRequest := requestFor(firstVoter.ID, "", 10)
	filterRequest.SetOption([]byte("first"))
	filteredEnc, err := api.GetPollVotesForTest(s, firstVoter.ID, filterRequest)
	if err != nil {
		t.Fatalf("filtered voter page: %v", err)
	}
	filtered := votesListFromResult(t, filteredEnc)
	if filtered.Count != 3 || len(filtered.Votes) != 3 {
		t.Fatalf("filtered voter page = count %d votes %d, want 3/3", filtered.Count, len(filtered.Votes))
	}
	badOffset := requestFor(firstVoter.ID, "not-an-offset", 10)
	if _, err = api.GetPollVotesForTest(s, firstVoter.ID, badOffset); err == nil {
		t.Fatal("malformed poll voter offset succeeded")
	}

	privatePoll := fixedPollMedia("Anonymous?", "A", "B")
	privateSent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: privatePoll, RandomID: 1401062,
	})
	if err != nil {
		t.Fatalf("send anonymous poll: %v", err)
	}
	privateMessage := messageOf(t, privateSent)
	_, err = s.ClosePoll(ctx, creator.ID, store.PollMessageRef{PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: int64(privateMessage.ID)})
	if err != nil {
		t.Fatalf("close anonymous poll: %v", err)
	}
	if _, err = api.GetPollVotesForTest(s, firstVoter.ID, &tg.MessagesGetPollVotesRequest{
		Peer: api.InputPeerChat(firstVoter.ID, chat.ID), ID: privateMessage.ID, Limit: 10,
	}); err == nil {
		t.Fatal("anonymous poll voter list succeeded after close")
	}
}

func TestSendPollRejectsHumanOneToOneBeforeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401004")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	other, err := s.CreateUser(ctx, "+15551401005")
	if err != nil {
		t.Fatalf("create other user: %v", err)
	}
	_, err = api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerUser(creator.ID, other.ID),
		Media:    fixedPollMedia("Private question?", "A", "B"),
		RandomID: 1401003,
	})
	if err == nil {
		t.Fatal("poll to another user succeeded, want PEER_ID_INVALID")
	}
	for _, user := range []int64{creator.ID, other.ID} {
		messages, historyErr := s.History(ctx, user, 1, other.ID, 0, 10)
		if historyErr != nil {
			t.Fatalf("history for user %d: %v", user, historyErr)
		}
		if len(messages) != 0 {
			t.Fatalf("rejected private poll wrote %d messages for user %d", len(messages), user)
		}
	}
}

func TestSendPollRejectsExplicitZeroClosePeriodBeforeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, dsn := openStoreDSN(t)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for poll count: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close poll count connection: %v", err)
		}
	})
	creator, err := s.CreateUser(ctx, "+15551401011")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Polls", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}

	stateBefore, err := s.State(ctx, creator.ID)
	if err != nil {
		t.Fatalf("state before invalid poll: %v", err)
	}
	eventsBefore, err := s.EventsSince(ctx, creator.ID, 0)
	if err != nil {
		t.Fatalf("events before invalid poll: %v", err)
	}
	var pollsBefore int64
	if err = conn.QueryRow(ctx, "SELECT count(*) FROM polls").Scan(&pollsBefore); err != nil {
		t.Fatalf("poll count before invalid poll: %v", err)
	}

	media := fixedPollMedia("Timed question?", "A", "B")
	media.Poll.SetClosePeriod(0)
	if !media.Poll.Flags.Has(4) {
		t.Fatal("SetClosePeriod(0) did not mark close_period as present")
	}
	const randomID = 1401012
	_, err = api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: randomID,
	})
	if !tgerr.Is(err, "POLL_ANSWERS_INVALID") {
		t.Fatalf("send poll with close_period=0 error = %v, want POLL_ANSWERS_INVALID", err)
	}

	if _, found, lookupErr := s.MessageByRandomID(ctx, creator.ID, randomID); lookupErr != nil || found {
		t.Fatalf("message lookup after rejected poll found=%v err=%v, want no message", found, lookupErr)
	}
	messages, err := s.History(ctx, creator.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("chat history after invalid poll: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("invalid poll wrote %d chat messages", len(messages))
	}
	var pollsAfter int64
	if err = conn.QueryRow(ctx, "SELECT count(*) FROM polls").Scan(&pollsAfter); err != nil {
		t.Fatalf("poll count after invalid poll: %v", err)
	}
	if pollsAfter != pollsBefore {
		t.Fatalf("poll count changed from %d to %d after rejected poll", pollsBefore, pollsAfter)
	}
	stateAfter, err := s.State(ctx, creator.ID)
	if err != nil || stateAfter.Pts != stateBefore.Pts {
		t.Fatalf("state after rejected poll = %+v, err %v; want pts %d", stateAfter, err, stateBefore.Pts)
	}
	eventsAfter, err := s.EventsSince(ctx, creator.ID, 0)
	if err != nil {
		t.Fatalf("events after invalid poll: %v", err)
	}
	if !slices.Equal(eventsAfter, eventsBefore) {
		t.Fatalf("events changed after rejected poll: before %+v after %+v", eventsBefore, eventsAfter)
	}
}

func TestOpenAnswersPollWithOneOptionIsRejectedBeforeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401006")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Polls", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	poll := tg.Poll{Question: tg.TextWithEntities{Text: "Open question?"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "Only option"}, Option: []byte("only")},
	}}
	poll.SetOpenAnswers(true)
	_, err = api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer:     api.InputPeerChat(creator.ID, chat.ID),
		Media:    &tg.InputMediaPoll{Poll: poll},
		RandomID: 1401004,
	})
	if err == nil {
		t.Fatal("open_answers with one fixed option succeeded, want poll validation error")
	}
	messages, err := s.History(ctx, creator.ID, 2, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("chat history after invalid poll: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("invalid poll wrote %d chat messages", len(messages))
	}
}

func TestSendPollRejectsClientSuppliedCreatorFlagBeforeWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401007")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Polls", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	media := fixedPollMedia("Creator flag?", "A", "B")
	media.Poll.SetCreator(true)
	if _, err = api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401007,
	}); err == nil {
		t.Fatal("poll with client-supplied creator flag succeeded")
	}
	messages, err := s.History(ctx, creator.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("chat history after invalid poll: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("invalid creator flag wrote %d chat messages", len(messages))
	}
}

func TestForwardPollIsRejectedWithoutWritingSavedMessage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401010")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Forward poll", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: fixedPollMedia("Do not forward?", "A", "B"), RandomID: 1401010,
	})
	if err != nil {
		t.Fatalf("send poll: %v", err)
	}
	pollMessage := messageOf(t, sent)
	before, err := s.State(ctx, creator.ID)
	if err != nil {
		t.Fatalf("state before rejected forward: %v", err)
	}
	_, err = api.ForwardMessagesForTest(s, creator.ID, &tg.MessagesForwardMessagesRequest{
		FromPeer: api.InputPeerChat(creator.ID, chat.ID), ToPeer: &tg.InputPeerSelf{},
		ID: []int{pollMessage.ID}, RandomID: []int64{1401011},
	})
	if err == nil || !strings.Contains(err.Error(), "MESSAGE_ID_INVALID") {
		t.Fatalf("forward poll error = %v, want MESSAGE_ID_INVALID", err)
	}
	stateAfter, err := s.State(ctx, creator.ID)
	if err != nil || stateAfter.Pts != before.Pts {
		t.Fatalf("state after rejected forward = %+v, err %v; want pts %d", stateAfter, err, before.Pts)
	}
	saved, err := s.History(ctx, creator.ID, store.PeerTypeUser, creator.ID, 0, 10)
	if err != nil {
		t.Fatalf("Saved Messages after rejected forward: %v", err)
	}
	if len(saved) != 0 {
		t.Fatalf("rejected poll forward wrote %d Saved Messages rows", len(saved))
	}
}

func fixedPollMedia(question, first, second string) *tg.InputMediaPoll {
	return &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: question},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: first}, Option: []byte("first")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: second}, Option: []byte("second")},
		},
	}}
}

func pollUpdateFromResponse(t *testing.T, result any) *tg.UpdateMessagePoll {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("poll RPC result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		if poll, ok := update.(*tg.UpdateMessagePoll); ok {
			return poll
		}
	}
	t.Fatalf("poll RPC response updates = %#v, want updateMessagePoll", updates.Updates)
	return nil
}

func voterIDs(t *testing.T, votes []tg.MessagePeerVoteClass) []int64 {
	t.Helper()
	ids := make([]int64, 0, len(votes))
	for _, vote := range votes {
		switch v := vote.(type) {
		case *tg.MessagePeerVote:
			peer, ok := v.Peer.(*tg.PeerUser)
			if !ok {
				t.Fatalf("voter peer = %T, want *tg.PeerUser", v.Peer)
			}
			ids = append(ids, peer.UserID)
		case *tg.MessagePeerVoteMultiple:
			peer, ok := v.Peer.(*tg.PeerUser)
			if !ok {
				t.Fatalf("multiple voter peer = %T, want *tg.PeerUser", v.Peer)
			}
			ids = append(ids, peer.UserID)
		default:
			t.Fatalf("vote = %T, want messagePeerVote", vote)
		}
	}
	return ids
}
