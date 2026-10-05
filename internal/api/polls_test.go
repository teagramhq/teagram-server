package api_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
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

func TestSendPollAcceptsInputPollAnswersAndSupportsVotingAndClosing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401060")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401061")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Input poll answers", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	media := &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: "Which option?"},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "First"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Second"}},
		},
	}}

	result, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401060,
	})
	if err != nil {
		t.Fatalf("send inputPollAnswer poll: %v", err)
	}
	message := messageOf(t, result)
	pollMedia, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok || len(pollMedia.Poll.Answers) != 2 {
		t.Fatalf("sent poll media = %#v, want two canonical answers", message.Media)
	}
	options := make([][]byte, len(pollMedia.Poll.Answers))
	for i, answerClass := range pollMedia.Poll.Answers {
		answer, ok := answerClass.(*tg.PollAnswer)
		if !ok {
			t.Fatalf("answer %d = %T, want canonical pollAnswer", i, answerClass)
		}
		if len(answer.Option) == 0 {
			t.Fatalf("answer %d has no server-assigned option bytes", i)
		}
		options[i] = answer.Option
	}
	if slices.Equal(options[0], options[1]) {
		t.Fatal("server assigned duplicate option bytes")
	}

	voted, err := api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), MsgID: message.ID, Options: [][]byte{options[0]},
	})
	if err != nil {
		t.Fatalf("vote using assigned option: %v", err)
	}
	if got := pollUpdateFromResponse(t, voted).Results.TotalVoters; got != 1 {
		t.Fatalf("total voters after vote = %d, want 1", got)
	}
	closed, err := s.ClosePoll(ctx, creator.ID, store.PollMessageRef{
		PeerType: store.PeerTypeChat, PeerID: chat.ID, LocalID: int64(message.ID),
	})
	if err != nil || !closed {
		t.Fatalf("close poll = %v, err %v; want closed", closed, err)
	}
}

func TestSendPollStoresAndReturnsDescriptionAndEntitiesOnRetryAndHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401062")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401063")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll descriptions", []int64{member.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	media := &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: "Which option?"},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "First"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Second"}},
		},
	}}
	const description = "Description stuff"
	entities := []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 11}}
	request := func(message string, entities []tg.MessageEntityClass) *tg.MessagesSendMediaRequest {
		return &tg.MessagesSendMediaRequest{
			Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, Message: message,
			Entities: entities, RandomID: 1401062,
		}
	}

	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, request(description, entities))
	if err != nil {
		t.Fatalf("send poll with description: %v", err)
	}
	checkDescription := func(label string, message *tg.Message) {
		t.Helper()
		if message.Message != description {
			t.Fatalf("%s message = %q, want %q", label, message.Message, description)
		}
		gotEntities, ok := message.GetEntities()
		if !ok || len(gotEntities) != 1 {
			t.Fatalf("%s entities = %#v present=%v, want one entity", label, gotEntities, ok)
		}
		bold, ok := gotEntities[0].(*tg.MessageEntityBold)
		if !ok || bold.Offset != 0 || bold.Length != 11 {
			t.Fatalf("%s entity = %#v, want bold over Description", label, gotEntities[0])
		}
		if _, ok := message.Media.(*tg.MessageMediaPoll); !ok {
			t.Fatalf("%s media = %T, want messageMediaPoll", label, message.Media)
		}
	}
	created := messageOf(t, sent)
	checkDescription("send response", created)

	// A transport retry may carry an updated local draft, but the committed
	// random_id still resolves to the original message and its description.
	retried, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, request("replacement text", []tg.MessageEntityClass{
		&tg.MessageEntityItalic{Offset: 0, Length: 11},
	}))
	if err != nil {
		t.Fatalf("retry poll with same random_id: %v", err)
	}
	retryMessage := messageOf(t, retried)
	if retryMessage.ID != created.ID {
		t.Fatalf("retry message id = %d, want original %d", retryMessage.ID, created.ID)
	}
	checkDescription("retry response", retryMessage)

	history, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerChat(member.ID, chat.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("member poll history: %v", err)
	}
	historyMessages, ok := history.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) != 1 {
		t.Fatalf("member history = %T with %d messages, want the poll", history, len(historyMessages.Messages))
	}
	memberMessage, ok := historyMessages.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("member history message = %T, want *tg.Message", historyMessages.Messages[0])
	}
	checkDescription("member history", memberMessage)

	byID, err := api.GetMessagesForTest(s, member.ID, &tg.MessagesGetMessagesRequest{
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: memberMessage.ID}},
	})
	if err != nil {
		t.Fatalf("member getMessages: %v", err)
	}
	checkDescription("member getMessages", firstMessageFromResult(t, byID))
}

func TestSendPollInPrivateChatSupportsVotingAndHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401065")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	member, err := s.CreateUser(ctx, "+15551401066")
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(creator.ID, member.ID),
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Private poll?"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}},
		Message: "Private description", Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 7}}, RandomID: 1401066,
	})
	if err != nil {
		t.Fatalf("send private poll: %v", err)
	}
	created := messageOf(t, sent)
	if created.Message != "Private description" {
		t.Fatalf("sender description = %q, want stored caption", created.Message)
	}
	pollMedia, ok := created.Media.(*tg.MessageMediaPoll)
	if !ok || len(pollMedia.Poll.Answers) != 2 {
		t.Fatalf("sender poll media = %#v, want two-answer poll", created.Media)
	}
	firstAnswer, ok := pollMedia.Poll.Answers[0].(*tg.PollAnswer)
	if !ok || len(firstAnswer.Option) == 0 {
		t.Fatalf("first answer = %#v, want server-assigned option bytes", pollMedia.Poll.Answers[0])
	}
	retried, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(creator.ID, member.ID),
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Private poll?"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}},
		Message: "replacement", Entities: []tg.MessageEntityClass{&tg.MessageEntityItalic{Offset: 0, Length: 11}}, RandomID: 1401066,
	})
	if err != nil {
		t.Fatalf("retry private poll: %v", err)
	}
	retryMessage := messageOf(t, retried)
	if retryMessage.ID != created.ID || retryMessage.Message != "Private description" {
		t.Fatalf("private poll retry = id %d caption %q, want id %d and original caption", retryMessage.ID, retryMessage.Message, created.ID)
	}
	retryEntities, ok := retryMessage.GetEntities()
	if !ok || len(retryEntities) != 1 {
		t.Fatalf("private poll retry entities = %#v present=%v, want original bold entity", retryEntities, ok)
	}
	if bold, ok := retryEntities[0].(*tg.MessageEntityBold); !ok || bold.Length != 7 {
		t.Fatalf("private poll retry entity = %#v, want original bold over Private", retryEntities[0])
	}

	history, err := api.GetHistoryForTest(s, member.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(member.ID, creator.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient private poll history: %v", err)
	}
	historyMessages, ok := history.(*tg.MessagesMessages)
	if !ok || len(historyMessages.Messages) != 1 {
		t.Fatalf("recipient private history = %T with %d messages, want one poll", history, len(historyMessages.Messages))
	}
	received, ok := historyMessages.Messages[0].(*tg.Message)
	if !ok || received.Message != "Private description" {
		t.Fatalf("recipient private message = %#v, want poll caption", historyMessages.Messages[0])
	}
	entities, ok := received.GetEntities()
	if !ok || len(entities) != 1 {
		t.Fatalf("recipient entities = %#v present=%v, want bold entity", entities, ok)
	}
	if bold, ok := entities[0].(*tg.MessageEntityBold); !ok || bold.Length != 7 {
		t.Fatalf("recipient entity = %#v, want bold over Private", entities[0])
	}
	byID, err := api.GetMessagesForTest(s, member.ID, &tg.MessagesGetMessagesRequest{
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: received.ID}},
	})
	if err != nil {
		t.Fatalf("recipient private poll getMessages: %v", err)
	}
	byIDMessage := firstMessageFromResult(t, byID)
	if byIDMessage.Message != "Private description" {
		t.Fatalf("recipient getMessages caption = %q, want stored description", byIDMessage.Message)
	}
	if _, ok := byIDMessage.Media.(*tg.MessageMediaPoll); !ok {
		t.Fatalf("recipient getMessages media = %T, want messageMediaPoll", byIDMessage.Media)
	}

	voted, err := api.SendVoteForTest(s, member.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerUser(member.ID, creator.ID), MsgID: received.ID, Options: [][]byte{firstAnswer.Option},
	})
	if err != nil {
		t.Fatalf("recipient votes in private poll: %v", err)
	}
	voteUpdates, ok := voted.(*tg.Updates)
	if !ok {
		t.Fatalf("private poll vote response = %T, want *tg.Updates", voted)
	}
	var voteResults *tg.PollResults
	for _, update := range voteUpdates.Updates {
		if pollUpdate, ok := update.(*tg.UpdateMessagePoll); ok {
			voteResults = &pollUpdate.Results
		}
	}
	if voteResults == nil || voteResults.TotalVoters != 1 || len(voteResults.Results) != 2 || voteResults.Results[0].Voters != 1 {
		t.Fatalf("private poll vote results = %+v, want one vote on the first answer", voteResults)
	}

	closedPoll := pollMedia.Poll
	closedPoll.SetClosed(true)
	closed, err := api.EditMessageForTest(s, creator.ID, &tg.MessagesEditMessageRequest{
		Peer: api.InputPeerUser(creator.ID, member.ID), ID: created.ID, Media: &tg.InputMediaPoll{Poll: closedPoll},
	})
	if err != nil {
		t.Fatalf("close private poll: %v", err)
	}
	closedUpdates, ok := closed.(*tg.Updates)
	if !ok {
		t.Fatalf("close private poll response = %T, want *tg.Updates", closed)
	}
	var closedMessage *tg.Message
	for _, update := range closedUpdates.Updates {
		if edited, ok := update.(*tg.UpdateEditMessage); ok {
			if message, ok := edited.Message.(*tg.Message); ok {
				closedMessage = message
			}
		}
	}
	if closedMessage == nil {
		t.Fatalf("close private poll response omitted updateEditMessage: %+v", closedUpdates.Updates)
	}
	closedMedia, ok := closedMessage.Media.(*tg.MessageMediaPoll)
	if !ok || !closedMedia.Poll.Closed || closedMessage.Message != "Private description" {
		t.Fatalf("closed private poll message = %#v, want closed poll with original description", closedMessage)
	}
}

func TestSendPollInvalidDescriptionUsesMessageErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401064")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Invalid poll description", nil)
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	for _, tc := range []struct {
		name     string
		message  string
		entities []tg.MessageEntityClass
		randomID int64
		wantErr  string
	}{
		{name: "invalid text", message: "bad\x00text", randomID: 1401064, wantErr: "MESSAGE_EMPTY"},
		{
			name: "invalid entity bounds", message: "short", randomID: 1401065,
			entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 6}},
			wantErr:  "ENTITY_BOUNDS_INVALID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sendErr := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
				Peer: api.InputPeerChat(creator.ID, chat.ID), Media: fixedPollMedia("Question?", "A", "B"),
				Message: tc.message, Entities: tc.entities, RandomID: tc.randomID,
			})
			if !tgerr.Is(sendErr, tc.wantErr) {
				t.Fatalf("send error = %v, want %s", sendErr, tc.wantErr)
			}
		})
	}
	messages, err := s.History(ctx, creator.ID, store.PeerTypeChat, chat.ID, 0, 10)
	if err != nil {
		t.Fatalf("chat history after invalid sends: %v", err)
	}
	if len(messages) != 0 {
		t.Fatalf("invalid poll descriptions wrote %d messages", len(messages))
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
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Saved question?"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}},
		Message: "Saved description", Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 5}},
		RandomID: 1401002,
	})
	if err != nil {
		t.Fatalf("send Saved Messages poll: %v", err)
	}
	message := messageOf(t, result)
	if _, ok := message.Media.(*tg.MessageMediaPoll); !ok {
		t.Fatalf("Saved Messages media = %T, want messageMediaPoll", message.Media)
	}
	if message.Message != "Saved description" {
		t.Fatalf("Saved Messages description = %q, want stored caption", message.Message)
	}
	entities, ok := message.GetEntities()
	if !ok || len(entities) != 1 {
		t.Fatalf("Saved Messages entities = %#v present=%v, want bold entity", entities, ok)
	}
	if bold, ok := entities[0].(*tg.MessageEntityBold); !ok || bold.Offset != 0 || bold.Length != 5 {
		t.Fatalf("Saved Messages entity = %#v, want bold over Saved", entities[0])
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

func TestPollVoteDifferenceIsViewerScopedAndRechecksMembership(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	creator, err := s.CreateUser(ctx, "+15551401901")
	if err != nil {
		t.Fatalf("create creator: %v", err)
	}
	voter, err := s.CreateUser(ctx, "+15551401902")
	if err != nil {
		t.Fatalf("create voter: %v", err)
	}
	observer, err := s.CreateUser(ctx, "+15551401903")
	if err != nil {
		t.Fatalf("create observer: %v", err)
	}
	outsider, err := s.CreateUser(ctx, "+15551401904")
	if err != nil {
		t.Fatalf("create outsider: %v", err)
	}
	chat, err := s.CreateChat(ctx, creator.ID, "Poll recovery", []int64{voter.ID, observer.ID})
	if err != nil {
		t.Fatalf("create chat: %v", err)
	}
	poll := tg.Poll{Question: tg.TextWithEntities{Text: "Choose the correct answer"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("a")},
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("b")},
	}}
	poll.SetQuiz(true)
	media := &tg.InputMediaPoll{Poll: poll, CorrectAnswers: []int{0}}
	media.SetCorrectAnswers([]int{0})
	media.SetSolution("A is correct")
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerChat(creator.ID, chat.ID), Media: media, RandomID: 1401901,
	})
	if err != nil {
		t.Fatalf("send quiz poll: %v", err)
	}
	creatorMessage := messageOf(t, sent)
	creatorMedia, ok := creatorMessage.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("created poll media = %T, want *tg.MessageMediaPoll", creatorMessage.Media)
	}
	messageFor := func(ownerID int64) *tg.Message {
		t.Helper()
		if ownerID == creator.ID {
			return creatorMessage
		}
		result, historyErr := api.GetHistoryForTest(s, ownerID, &tg.MessagesGetHistoryRequest{
			Peer: api.InputPeerChat(ownerID, chat.ID), Limit: 10,
		})
		if historyErr != nil {
			t.Fatalf("get poll history for %d: %v", ownerID, historyErr)
		}
		response, ok := result.(*tg.MessagesMessages)
		if !ok {
			t.Fatalf("history for %d = %T, want *tg.MessagesMessages", ownerID, result)
		}
		return findPollMessage(t, response.Messages)
	}
	creatorMessage = findPollMessage(t, []tg.MessageClass{creatorMessage})
	refs := map[int64]int{
		creator.ID:  creatorMessage.ID,
		voter.ID:    messageFor(voter.ID).ID,
		observer.ID: messageFor(observer.ID).ID,
	}
	before := make(map[int64]int, 4)
	for _, owner := range []int64{creator.ID, voter.ID, observer.ID, outsider.ID} {
		state, stateErr := s.State(ctx, owner)
		if stateErr != nil {
			t.Fatalf("state for %d before vote: %v", owner, stateErr)
		}
		before[owner] = state.Pts
	}
	if _, err = api.SendVoteForTest(s, voter.ID, &tg.MessagesSendVoteRequest{
		Peer: api.InputPeerChat(voter.ID, chat.ID), MsgID: refs[voter.ID], Options: [][]byte{[]byte("a")},
	}); err != nil {
		t.Fatalf("cast vote: %v", err)
	}
	for _, owner := range []struct {
		userID  int64
		localID int
	}{{creator.ID, refs[creator.ID]}, {voter.ID, refs[voter.ID]}, {observer.ID, refs[observer.ID]}} {
		state, stateErr := s.State(ctx, owner.userID)
		if stateErr != nil || state.Pts != before[owner.userID]+1 {
			t.Errorf("owner %d state after vote = %+v, err %v; want pts %d", owner.userID, state, stateErr, before[owner.userID]+1)
		}
		events, eventErr := s.EventsSince(ctx, owner.userID, before[owner.userID])
		if eventErr != nil || len(events) != 1 || events[0].Type != store.EventEdit || events[0].LocalID != int64(owner.localID) {
			t.Errorf("owner %d vote events = %+v, err %v; want one edit for copy %d", owner.userID, events, eventErr, owner.localID)
		}
	}
	outsiderState, err := s.State(ctx, outsider.ID)
	if err != nil || outsiderState.Pts != before[outsider.ID] {
		t.Fatalf("outsider state after vote = %+v, err %v; want unchanged pts %d", outsiderState, err, before[outsider.ID])
	}
	outsiderEvents, err := s.EventsSince(ctx, outsider.ID, before[outsider.ID])
	if err != nil || len(outsiderEvents) != 0 {
		t.Fatalf("outsider events after vote = %+v, err %v; want none", outsiderEvents, err)
	}

	creatorDiff, err := api.GetDifferenceForTest(s, creator.ID, &tg.UpdatesGetDifferenceRequest{Pts: before[creator.ID]})
	if err != nil {
		t.Fatalf("creator getDifference: %v", err)
	}
	creatorEdit, creatorResults := pollEditFromDifference(t, creatorDiff, creatorMedia.Poll.ID)
	if creatorEdit == nil || creatorResults.TotalVoters != 1 || creatorResults.Results[0].Chosen || creatorResults.Results[0].Correct || creatorResults.Solution != "" {
		t.Fatalf("creator recovered poll results = %+v, want count without selection or quiz key", creatorResults)
	}
	voterDiff, err := api.GetDifferenceForTest(s, voter.ID, &tg.UpdatesGetDifferenceRequest{Pts: before[voter.ID]})
	if err != nil {
		t.Fatalf("voter getDifference: %v", err)
	}
	voterEdit, voterResults := pollEditFromDifference(t, voterDiff, creatorMedia.Poll.ID)
	if voterEdit == nil || voterResults.TotalVoters != 1 || !voterResults.Results[0].Chosen || !voterResults.Results[0].Correct || voterResults.Solution == "" {
		t.Fatalf("voter recovered poll results = %+v, want their selection and quiz key", voterResults)
	}

	if removed, _, _, removeErr := s.RemoveChatUser(ctx, chat.ID, observer.ID, creator.ID); removeErr != nil || !removed {
		t.Fatalf("remove observer before replay = %v, err %v", removed, removeErr)
	}
	observerDiff, err := api.GetDifferenceForTest(s, observer.ID, &tg.UpdatesGetDifferenceRequest{Pts: before[observer.ID]})
	if err != nil {
		t.Fatalf("removed observer getDifference: %v", err)
	}
	if hasPollMessageInDifference(observerDiff, creatorMedia.Poll.ID) {
		t.Fatal("removed observer recovered poll contents")
	}
	outsiderDiff, err := api.GetDifferenceForTest(s, outsider.ID, &tg.UpdatesGetDifferenceRequest{Pts: before[outsider.ID]})
	if err != nil {
		t.Fatalf("outsider getDifference: %v", err)
	}
	if hasPollMessageInDifference(outsiderDiff, creatorMedia.Poll.ID) {
		t.Fatal("outsider recovered poll contents")
	}
}

func TestSavedPollVoteDifferenceCanBeReadByMultipleSessions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	owner, err := s.CreateUser(ctx, "+15551401911")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	poll := tg.Poll{Question: tg.TextWithEntities{Text: "Choose the correct answer"}, Answers: []tg.PollAnswerClass{
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("a")},
		&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("b")},
	}}
	poll.SetQuiz(true)
	media := &tg.InputMediaPoll{Poll: poll, CorrectAnswers: []int{0}}
	media.SetCorrectAnswers([]int{0})
	media.SetSolution("A is correct")
	sent, err := api.SendMediaForTest(s, owner.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerSelf{}, Media: media, RandomID: 1401911,
	})
	if err != nil {
		t.Fatalf("send Saved Messages quiz poll: %v", err)
	}
	message := messageOf(t, sent)
	messageMedia, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok {
		t.Fatalf("Saved Messages media = %T, want *tg.MessageMediaPoll", message.Media)
	}
	pollID := messageMedia.Poll.ID
	before, err := s.State(ctx, owner.ID)
	if err != nil {
		t.Fatalf("state before vote: %v", err)
	}
	if _, err = api.SendVoteForTest(s, owner.ID, &tg.MessagesSendVoteRequest{
		Peer: &tg.InputPeerSelf{}, MsgID: message.ID, Options: [][]byte{[]byte("a")},
	}); err != nil {
		t.Fatalf("vote in Saved Messages: %v", err)
	}
	events, err := s.EventsSince(ctx, owner.ID, before.Pts)
	if err != nil || len(events) != 1 || events[0].Type != store.EventEdit || events[0].LocalID != int64(message.ID) {
		t.Fatalf("Saved Messages vote events = %+v, err %v; want one edit for the owner's copy", events, err)
	}
	for session := 1; session <= 2; session++ {
		difference, differenceErr := api.GetDifferenceForTest(s, owner.ID, &tg.UpdatesGetDifferenceRequest{Pts: before.Pts})
		if differenceErr != nil {
			t.Fatalf("Saved Messages session %d getDifference: %v", session, differenceErr)
		}
		edit, results := pollEditFromDifference(t, difference, pollID)
		if edit == nil || results.TotalVoters != 1 || !results.Results[0].Chosen || !results.Results[0].Correct || results.Solution == "" {
			t.Fatalf("Saved Messages session %d results = %+v, want owner's chosen quiz result", session, results)
		}
	}
}

func pollEditFromDifference(t *testing.T, result bin.Encoder, pollID int64) (*tg.Message, *tg.PollResults) {
	t.Helper()
	for _, update := range differenceUpdates(t, result) {
		edit, ok := update.(*tg.UpdateEditMessage)
		if !ok {
			continue
		}
		message, ok := edit.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if ok && media.Poll.ID == pollID {
			return message, &media.Results
		}
	}
	return nil, nil
}

func hasPollMessageInDifference(result bin.Encoder, pollID int64) bool {
	var updates []tg.UpdateClass
	switch difference := result.(type) {
	case *tg.UpdatesDifference:
		updates = difference.OtherUpdates
	case *tg.UpdatesDifferenceSlice:
		updates = difference.OtherUpdates
	}
	for _, update := range updates {
		edit, ok := update.(*tg.UpdateEditMessage)
		if !ok {
			continue
		}
		message, ok := edit.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if ok && media.Poll.ID == pollID {
			return true
		}
	}
	return false
}

func differenceUpdates(t *testing.T, result bin.Encoder) []tg.UpdateClass {
	t.Helper()
	switch difference := result.(type) {
	case *tg.UpdatesDifference:
		return difference.OtherUpdates
	case *tg.UpdatesDifferenceSlice:
		return difference.OtherUpdates
	default:
		t.Fatalf("getDifference = %T, want difference with updates", result)
		return nil
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

func TestSendPollInPrivateChatAcceptsEmptyDescription(t *testing.T) {
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
	sent, err := api.SendMediaForTest(s, creator.ID, newBlobs(t), api.TestMaxUserStorageBytes, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(creator.ID, other.ID), Media: &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Private question?"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}}, RandomID: 1401003,
	})
	if err != nil {
		t.Fatalf("send private poll without description: %v", err)
	}
	created := messageOf(t, sent)
	if created.Message != "" {
		t.Fatalf("private poll without description caption = %q, want empty", created.Message)
	}
	if _, ok := created.Media.(*tg.MessageMediaPoll); !ok {
		t.Fatalf("private poll media = %T, want messageMediaPoll", created.Media)
	}
	history, err := api.GetHistoryForTest(s, other.ID, &tg.MessagesGetHistoryRequest{
		Peer: api.InputPeerUser(other.ID, creator.ID), Limit: 10,
	})
	if err != nil {
		t.Fatalf("recipient private poll history: %v", err)
	}
	got := firstMessageFromResult(t, history)
	if got.Message != "" {
		t.Fatalf("recipient private poll caption = %q, want empty", got.Message)
	}
	if _, ok := got.Media.(*tg.MessageMediaPoll); !ok {
		t.Fatalf("recipient private poll history media = %T, want messageMediaPoll", got.Media)
	}
	for _, peer := range []struct{ userID, peerID int64 }{{creator.ID, other.ID}, {other.ID, creator.ID}} {
		messages, historyErr := s.History(ctx, peer.userID, store.PeerTypeUser, peer.peerID, 0, 10)
		if historyErr != nil {
			t.Fatalf("history for user %d: %v", peer.userID, historyErr)
		}
		if len(messages) != 1 {
			t.Fatalf("private poll wrote %d messages for user %d, want one copy", len(messages), peer.userID)
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
