package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

const (
	maxChannelCaptureEntries = 64
	maxChannelCapturePolls   = 8
	channelDifferenceLimit   = 64
	channelLiveUpdateWindow  = 10 * time.Second
	channelNegativeWindow    = channelLiveUpdateWindow
)

const (
	supergroupChannelName = "supergroup"
	broadcastChannelName  = "broadcast"
)

type probeChannel struct {
	id           int64
	creatorHash  int64
	megagroup    bool
	createdInRun bool
	accessHashes map[int64]int64
}

type channelUpdateCapture struct {
	mu       sync.Mutex
	pollIDs  map[int64]struct{}
	updates  []tg.UpdateClass
	notified chan struct{}
}

func newChannelUpdateCapture() *channelUpdateCapture {
	return &channelUpdateCapture{
		pollIDs:  make(map[int64]struct{}, maxChannelCapturePolls),
		updates:  make([]tg.UpdateClass, 0, maxChannelCaptureEntries),
		notified: make(chan struct{}, 1),
	}
}

func (c *channelUpdateCapture) AddPoll(pollID int64) error {
	if pollID <= 0 {
		return failure("channel_update_capture", "POLL_ID_INVALID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.pollIDs[pollID]; exists {
		return nil
	}
	if len(c.pollIDs) >= maxChannelCapturePolls {
		return failure("channel_update_capture", "POLL_CAPTURE_LIMIT")
	}
	c.pollIDs[pollID] = struct{}{}
	return nil
}

func (c *channelUpdateCapture) Handle(_ context.Context, result tg.UpdatesClass) error {
	for _, update := range channelCaptureUpdates(result) {
		pollID, matches := capturedChannelPollID(update)
		if !matches {
			continue
		}
		c.mu.Lock()
		_, registered := c.pollIDs[pollID]
		stored := registered && len(c.updates) < maxChannelCaptureEntries
		if stored {
			c.updates = append(c.updates, update)
		}
		c.mu.Unlock()
		if stored {
			select {
			case c.notified <- struct{}{}:
			default:
			}
		}
	}
	return nil
}

func channelCaptureUpdates(result tg.UpdatesClass) []tg.UpdateClass {
	if short, ok := result.(*tg.UpdateShort); ok && short != nil {
		if short.Update == nil {
			return nil
		}
		return []tg.UpdateClass{short.Update}
	}
	return updateList(result)
}

func (c *channelUpdateCapture) Snapshot() []tg.UpdateClass {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]tg.UpdateClass(nil), c.updates...)
}

func capturedChannelPollID(update tg.UpdateClass) (int64, bool) {
	switch value := update.(type) {
	case *tg.UpdateMessagePoll:
		if value == nil {
			return 0, false
		}
		return value.PollID, value.PollID > 0
	case *tg.UpdateEditChannelMessage:
		if value == nil {
			return 0, false
		}
		message, ok := value.Message.(*tg.Message)
		if !ok || message == nil {
			return 0, false
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || media == nil || media.Poll.ID <= 0 {
			return 0, false
		}
		return media.Poll.ID, true
	default:
		return 0, false
	}
}

func (p *probe) addChannelPollCapture(username string, pollID int64) error {
	account := p.account(username)
	if account == nil || account.capture == nil {
		return failure("channel_update_capture", "CAPTURE_NOT_CONFIGURED")
	}
	return account.capture.AddPoll(pollID)
}

func (p *probe) channelUpdateHandler(username string) (telegram.UpdateHandler, error) {
	account := p.account(username)
	if account == nil || account.capture == nil {
		return nil, failure("channel_update_capture", "CAPTURE_NOT_CONFIGURED")
	}
	return account.capture, nil
}

func (p *probe) channelUpdateSnapshot(username string) []tg.UpdateClass {
	account := p.account(username)
	if account == nil || account.capture == nil {
		return nil
	}
	return account.capture.Snapshot()
}

func (p *probe) channelInput(channelName, username string) (*tg.InputChannel, error) {
	channel, ok := p.channelRefs[channelName]
	if !ok || channel == nil || !channel.createdInRun || channel.id <= 0 || channel.creatorHash == 0 {
		return nil, failure("channel_reference", "CHANNEL_NOT_CREATED_IN_RUN")
	}
	account := p.account(username)
	if account == nil || !fixedUsername(username) || account.userID <= 0 {
		return nil, failure("channel_reference", "PEER_OUTSIDE_ALLOWLIST")
	}
	accessHash, ok := channel.accessHashes[account.userID]
	if !ok || accessHash == 0 {
		return nil, failure("channel_reference", "CHANNEL_VIEW_NOT_RESOLVED")
	}
	return &tg.InputChannel{ChannelID: channel.id, AccessHash: accessHash}, nil
}

func (p *probe) channelPeer(channelName, username string) (*tg.InputPeerChannel, error) {
	input, err := p.channelInput(channelName, username)
	if err != nil {
		return nil, err
	}
	return &tg.InputPeerChannel{ChannelID: input.ChannelID, AccessHash: input.AccessHash}, nil
}

func (p *probe) channelInviteTargets(usernames ...string) ([]tg.InputUserClass, error) {
	if len(usernames) == 0 || len(usernames) > 2 {
		return nil, failure("channel_invite", "INVITEE_OUTSIDE_ALLOWLIST")
	}
	seen := make(map[string]bool, len(usernames))
	targets := make([]tg.InputUserClass, 0, len(usernames))
	for _, username := range usernames {
		if username != "synthpoll_b" && username != "synthpoll_c" || seen[username] {
			return nil, failure("channel_invite", "INVITEE_OUTSIDE_ALLOWLIST")
		}
		peer := p.peersFromA[username]
		if !p.safePeer(username, peer) {
			return nil, failure("channel_invite", "INVITEE_NOT_VERIFIED")
		}
		seen[username] = true
		targets = append(targets, &tg.InputUser{UserID: peer.userID, AccessHash: peer.accessHash})
	}
	return targets, nil
}

func (p *probe) createChannel(ctx context.Context, name, assertion string, megagroup bool) error {
	if name != supergroupChannelName && name != broadcastChannelName || p.channelRefs[name] != nil {
		return failure(assertion, "CHANNEL_CONFIGURATION_INVALID")
	}
	title := fmt.Sprintf("pollprobe-%s-%d", name, time.Now().UnixNano())
	return p.withAccount(ctx, p.account("synthpoll_a"), assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
			Title: title, Broadcast: !megagroup, Megagroup: megagroup,
		})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		updates, ok := result.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return failure(assertion, "CHANNEL_CREATE_RESPONSE_INVALID")
		}
		channel, ok := updates.Chats[0].(*tg.Channel)
		if !ok || channel.ID <= 0 || channel.AccessHash == 0 || channel.Megagroup != megagroup || channel.Broadcast == megagroup || channel.Username != "" || len(channel.Usernames) != 0 {
			return failure(assertion, "CHANNEL_CREATE_RESPONSE_INVALID")
		}
		p.channelRefs[name] = &probeChannel{
			id:           channel.ID,
			creatorHash:  channel.AccessHash,
			megagroup:    megagroup,
			createdInRun: true,
			accessHashes: map[int64]int64{p.account("synthpoll_a").userID: channel.AccessHash},
		}
		return nil
	})
}

func (p *probe) inviteChannelMembers(ctx context.Context, channelName, assertion string) error {
	targets, err := p.channelInviteTargets("synthpoll_b", "synthpoll_c")
	if err != nil {
		return err
	}
	input, err := p.channelInput(channelName, "synthpoll_a")
	if err != nil {
		return err
	}
	if err := p.withAccount(ctx, p.account("synthpoll_a"), assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{Channel: input, Users: targets})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		if result == nil || result.Updates == nil || len(result.MissingInvitees) != 0 {
			return failure(assertion, "CHANNEL_INVITE_INCOMPLETE")
		}
		return nil
	}); err != nil {
		return err
	}
	for _, username := range []string{"synthpoll_b", "synthpoll_c"} {
		if err := p.resolveChannelView(ctx, channelName, username, assertion); err != nil {
			return err
		}
	}
	return p.pass(assertion, "members=3", "outsiders=1")
}

func (p *probe) resolveChannelView(ctx context.Context, channelName, username, assertion string) error {
	channelRef := p.channelRefs[channelName]
	if channelRef == nil || !channelRef.createdInRun || channelRef.id <= 0 {
		return failure(assertion, "CHANNEL_NOT_CREATED_IN_RUN")
	}
	account := p.account(username)
	if account == nil || !fixedUsername(username) {
		return failure(assertion, "PEER_OUTSIDE_ALLOWLIST")
	}
	return p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}, Limit: maxHistoryMessages})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		var chats []tg.ChatClass
		switch dialogs := result.(type) {
		case *tg.MessagesDialogs:
			chats = dialogs.Chats
		case *tg.MessagesDialogsSlice:
			chats = dialogs.Chats
		default:
			return failure(assertion, "CHANNEL_VIEW_RESPONSE_INVALID")
		}
		var found *tg.Channel
		for _, chat := range chats {
			candidate, ok := chat.(*tg.Channel)
			if !ok || candidate.ID != channelRef.id {
				continue
			}
			if found != nil {
				return failure(assertion, "CHANNEL_VIEW_AMBIGUOUS")
			}
			found = candidate
		}
		if found == nil || found.AccessHash == 0 || found.Left || found.Username != "" || len(found.Usernames) != 0 {
			return failure(assertion, "CHANNEL_VIEW_INVALID")
		}
		channelRef.accessHashes[account.userID] = found.AccessHash
		return nil
	})
}

func (p *probe) channelSnapshot(ctx context.Context, account *probeAccount, channelName, assertion string) (int, int, error) {
	var pts, historyCount int
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		input, err := p.channelInput(channelName, account.username)
		if err != nil {
			return err
		}
		full, err := api.ChannelsGetFullChannel(ctx, input)
		if err != nil {
			return rpcFailure(assertion, err)
		}
		channelFull, ok := full.FullChat.(*tg.ChannelFull)
		if !ok || channelFull.ID != input.ChannelID || channelFull.Pts < 0 {
			return failure(assertion, "CHANNEL_STATE_INVALID")
		}
		peer, err := p.channelPeer(channelName, account.username)
		if err != nil {
			return err
		}
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: maxHistoryMessages})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		messages, ok := channelHistoryMessages(history)
		if !ok {
			return failure(assertion, "CHANNEL_HISTORY_RESPONSE_INVALID")
		}
		pts, historyCount = channelFull.Pts, len(messages)
		return nil
	}); err != nil {
		return 0, 0, err
	}
	return pts, historyCount, nil
}

func channelHistoryMessages(result tg.MessagesMessagesClass) ([]tg.MessageClass, bool) {
	switch messages := result.(type) {
	case *tg.MessagesChannelMessages:
		return messages.Messages, true
	case *tg.MessagesMessages:
		return messages.Messages, true
	case *tg.MessagesMessagesSlice:
		return messages.Messages, true
	default:
		return nil, false
	}
}

func (p *probe) channelMessage(ctx context.Context, account *probeAccount, channelName string, messageID int, assertion string) (*tg.Message, error) {
	var found *tg.Message
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		input, err := p.channelInput(channelName, account.username)
		if err != nil {
			return err
		}
		result, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: input, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
		})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		messages, ok := channelHistoryMessages(result)
		if !ok || len(messages) != 1 {
			return failure(assertion, "CHANNEL_MESSAGE_RESPONSE_INVALID")
		}
		message, ok := messages[0].(*tg.Message)
		if !ok || message.ID != messageID {
			return failure(assertion, "CHANNEL_MESSAGE_RESPONSE_INVALID")
		}
		found = message
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func (p *probe) runChannels(ctx context.Context) error {
	if err := p.createChannel(ctx, supergroupChannelName, "synthetic_supergroup_created", true); err != nil {
		return err
	}
	if err := p.pass("synthetic_supergroup_created", "kind=supergroup"); err != nil {
		return err
	}
	if err := p.inviteChannelMembers(ctx, supergroupChannelName, "synthetic_supergroup_members"); err != nil {
		return err
	}
	if err := p.runSupergroupPolls(ctx); err != nil {
		return err
	}
	if err := p.createChannel(ctx, broadcastChannelName, "synthetic_broadcast_created", false); err != nil {
		return err
	}
	if err := p.pass("synthetic_broadcast_created", "kind=broadcast"); err != nil {
		return err
	}
	if err := p.inviteChannelMembers(ctx, broadcastChannelName, "synthetic_broadcast_members"); err != nil {
		return err
	}
	return p.runBroadcastPolls(ctx)
}

func (p *probe) runSupergroupPolls(ctx context.Context) error {
	a, b, c, d := p.account("synthpoll_a"), p.account("synthpoll_b"), p.account("synthpoll_c"), p.account("synthpoll_d")
	messageID, pollID, err := p.sendChannelPoll(ctx, a, supergroupChannelName, "Supergroup probe poll", false, false, "channel_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	vote, err := p.castChannelVote(ctx, b, supergroupChannelName, messageID, pollID, []byte("B"), "channel_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	if !anonymousChannelVoteMatches(vote, pollID, 1, map[string]int{"A": 0, "B": 1}) {
		return anonymousChannelVoteFailure("channel_anonymous_poll_privacy", vote)
	}
	creatorPeer, err := p.channelPeer(supergroupChannelName, a.username)
	if err != nil {
		return err
	}
	creatorResults, err := p.readPollResults(ctx, a, creatorPeer, messageID, pollID, "channel_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	if !anonymousChannelResultsMatch(creatorResults, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("channel_anonymous_poll_privacy", "POLL_RESULT_MISMATCH")
	}
	if _, err := p.awaitChannelPoll(ctx, c, pollID, "channel_anonymous_poll_privacy", func(update *tg.UpdateMessagePoll) error {
		if !anonymousChannelNonVoterUpdateMatches(update, pollID, 1, map[string]int{"A": 0, "B": 1}) {
			return failure("channel_anonymous_poll_privacy", "LIVE_RESULT_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.expectChannelRPCError(ctx, b, "channel_anonymous_non_voter_denied", "POLL_VOTE_REQUIRED", 403, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(supergroupChannelName, b.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: peer, ID: messageID, Limit: 1})
		return err
	}); err != nil {
		return err
	}
	if err := p.pass("channel_anonymous_non_voter_denied", "rpc_error=POLL_VOTE_REQUIRED", "rpc_code=403"); err != nil {
		return err
	}
	if err := p.pass("channel_anonymous_poll_privacy", "voters=1", "voter_ids=0", "chosen=0"); err != nil {
		return err
	}

	publicMessageID, publicPollID, err := p.sendChannelPoll(ctx, a, supergroupChannelName, "Supergroup public probe", true, false, "channel_public_voter_pagination")
	if err != nil {
		return err
	}
	if _, err := p.castChannelVote(ctx, b, supergroupChannelName, publicMessageID, publicPollID, []byte("A"), "channel_public_voter_pagination"); err != nil {
		return err
	}
	publicResults, err := p.castChannelVote(ctx, c, supergroupChannelName, publicMessageID, publicPollID, []byte("B"), "channel_public_voter_pagination")
	if err != nil {
		return err
	}
	if !pollCountsMatch(&publicResults.Results, 2, map[string]int{"A": 1, "B": 1}) {
		return pollCountFailure("channel_public_voter_pagination", &publicResults.Results)
	}
	count, err := p.channelVoterPages(ctx, b, supergroupChannelName, publicMessageID, b.userID, c.userID)
	if err != nil {
		return err
	}
	if err := p.pass("channel_public_voter_pagination", fmt.Sprintf("voters=%d", count), "limit=1", "unique=2"); err != nil {
		return err
	}

	quizMessageID, quizPollID, err := p.sendChannelPoll(ctx, a, supergroupChannelName, "Supergroup probe quiz", false, true, "channel_quiz_privacy")
	if err != nil {
		return err
	}
	if err := p.checkNonVoterQuiz(ctx, c, supergroupChannelName, quizMessageID, quizPollID, "channel_quiz_privacy"); err != nil {
		return err
	}
	quizVote, err := p.castChannelVote(ctx, b, supergroupChannelName, quizMessageID, quizPollID, []byte("B"), "channel_quiz_privacy")
	if err != nil {
		return err
	}
	if !voterQuizResultsMatch(&quizVote.Results, "A and B are correct") {
		return failure("channel_quiz_privacy", "QUIZ_VOTER_RESULTS_MISMATCH")
	}
	if _, err := p.awaitChannelPoll(ctx, c, quizPollID, "channel_quiz_privacy", func(update *tg.UpdateMessagePoll) error {
		if update.Results.TotalVoters != 1 || !nonVoterQuizResults(update.Results) || hasVoterIdentity(&update.Results) {
			return failure("channel_quiz_privacy", "QUIZ_NON_VOTER_RESULTS_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.checkNonVoterQuiz(ctx, c, supergroupChannelName, quizMessageID, quizPollID, "channel_quiz_privacy"); err != nil {
		return err
	}
	if err := p.pass("channel_quiz_privacy", "voters=1", "correct_visible_to_voter=1", "correct_visible_to_non_voter=0"); err != nil {
		return err
	}

	if err := p.verifyDefaultPollBan(ctx, a, c, supergroupChannelName, "channel_default_poll_ban"); err != nil {
		return err
	}
	if err := p.verifyChannelMemberCloseDenied(ctx, a, c, supergroupChannelName, messageID, pollID, "channel_member_close_denied"); err != nil {
		return err
	}
	if err := p.promoteChannelAdmin(ctx, a, b, supergroupChannelName, "channel_admin_close"); err != nil {
		return err
	}
	publicCloseMessageID := publicMessageID
	if err := p.closeChannelPoll(ctx, b, supergroupChannelName, publicCloseMessageID, publicPollID, "channel_admin_close"); err != nil {
		return err
	}
	if err := p.pass("channel_admin_close", "sender_or_admin=1"); err != nil {
		return err
	}

	return p.recoverAndRestrictSupergroup(ctx, a, b, c, d, messageID, pollID)
}

func (p *probe) sendChannelPoll(ctx context.Context, account *probeAccount, channelName, question string, public, quiz bool, assertion string) (int, int64, error) {
	requestID, err := randomID()
	if err != nil {
		return 0, 0, failure(assertion, "RANDOM_REQUEST_ID_UNAVAILABLE")
	}
	poll := tg.Poll{
		Question: tg.TextWithEntities{Text: question},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("A")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("B")},
		},
	}
	poll.SetPublicVoters(public)
	poll.SetQuiz(quiz)
	media := &tg.InputMediaPoll{Poll: poll}
	if quiz {
		media.SetCorrectAnswers([]int{1})
		media.SetSolution("A and B are correct")
	}
	var messageID int
	var pollID int64
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := p.channelPeer(channelName, account.username)
		if err != nil {
			return err
		}
		result, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{Peer: peer, Media: media, RandomID: requestID})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		message, ok := newChannelPollMessage(result)
		if !ok {
			return failure(assertion, "POLL_MESSAGE_MISSING")
		}
		pollMedia, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || pollMedia.Poll.ID <= 0 || pollMedia.Poll.Question.Text != question || pollMedia.Poll.PublicVoters != public || pollMedia.Poll.Quiz != quiz || message.ID <= 0 {
			return failure(assertion, "POLL_MESSAGE_INVALID")
		}
		messageID, pollID = message.ID, pollMedia.Poll.ID
		return nil
	}); err != nil {
		return 0, 0, err
	}
	for _, username := range []string{"synthpoll_c"} {
		if err := p.addChannelPollCapture(username, pollID); err != nil {
			return 0, 0, err
		}
	}
	if channelName == broadcastChannelName {
		if err := p.addChannelPollCapture("synthpoll_b", pollID); err != nil {
			return 0, 0, err
		}
	}
	return messageID, pollID, nil
}

func newChannelPollMessage(result tg.UpdatesClass) (*tg.Message, bool) {
	var found *tg.Message
	for _, update := range updateList(result) {
		created, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		message, ok := created.Message.(*tg.Message)
		if !ok {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPoll); !ok {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = message
	}
	return found, found != nil
}

func (p *probe) castChannelVote(ctx context.Context, account *probeAccount, channelName string, messageID int, pollID int64, option []byte, assertion string) (*tg.UpdateMessagePoll, error) {
	var found *tg.UpdateMessagePoll
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := p.channelPeer(channelName, account.username)
		if err != nil {
			return err
		}
		result, err := api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: peer, MsgID: messageID, Options: [][]byte{option}})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		for _, update := range updateList(result) {
			poll, ok := update.(*tg.UpdateMessagePoll)
			if !ok || poll.PollID != pollID {
				continue
			}
			if found != nil {
				return failure(assertion, "POLL_UPDATE_AMBIGUOUS")
			}
			found = poll
		}
		if found == nil || found.Peer != nil || found.MsgID != 0 {
			return failure(assertion, "POLL_VOTER_IDENTITY_EXPOSED")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func anonymousChannelVoteMatches(update *tg.UpdateMessagePoll, pollID int64, voters int, options map[string]int) bool {
	return update != nil && update.PollID == pollID && update.Peer == nil && update.MsgID == 0 && anonymousChannelVoterResultsMatch(&update.Results, voters, options)
}

func anonymousChannelNonVoterUpdateMatches(update *tg.UpdateMessagePoll, pollID int64, voters int, options map[string]int) bool {
	return update != nil && update.PollID == pollID && update.Peer == nil && update.MsgID == 0 && anonymousChannelResultsMatch(&update.Results, voters, options)
}

func anonymousChannelVoterResultsMatch(results *tg.PollResults, voters int, options map[string]int) bool {
	if results == nil || !pollCountsMatch(results, voters, options) || hasVoterIdentity(results) {
		return false
	}
	chosen := 0
	for _, answer := range results.Results {
		if answer.Chosen {
			if answer.Voters == 0 {
				return false
			}
			chosen++
		}
	}
	return chosen == 1
}

func anonymousChannelVoteFailure(assertion string, update *tg.UpdateMessagePoll) error {
	if update == nil {
		return failure(assertion, "VOTE_UPDATE_MISSING")
	}
	return failureWithFields(assertion, "VOTE_UPDATE_MISMATCH",
		fmt.Sprintf("actual_total=%d", update.Results.TotalVoters),
		fmt.Sprintf("answer_count=%d", len(update.Results.Results)),
		fmt.Sprintf("first_voters=%d", answerVoterCount(&update.Results, 0)),
		fmt.Sprintf("second_voters=%d", answerVoterCount(&update.Results, 1)),
		fmt.Sprintf("voter_ids=%d", boolInt(hasVoterIdentity(&update.Results))),
		fmt.Sprintf("chosen=%d", boolInt(hasChosenAnswer(&update.Results))),
		fmt.Sprintf("peer_present=%d", boolInt(update.Peer != nil)),
		fmt.Sprintf("message_id_present=%d", boolInt(update.MsgID != 0)),
	)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func anonymousChannelResultsMatch(results *tg.PollResults, voters int, options map[string]int) bool {
	return pollCountsMatch(results, voters, options) && !hasChosenAnswer(results) && !hasVoterIdentity(results)
}

func (p *probe) awaitChannelPoll(ctx context.Context, account *probeAccount, pollID int64, assertion string, check func(*tg.UpdateMessagePoll) error) (*tg.UpdateMessagePoll, error) {
	if account == nil || account.capture == nil || pollID <= 0 {
		return nil, failure(assertion, "POLL_CAPTURE_NOT_CONFIGURED")
	}
	timer := time.NewTimer(channelLiveUpdateWindow)
	defer timer.Stop()
	for {
		for _, update := range account.capture.Snapshot() {
			poll, ok := update.(*tg.UpdateMessagePoll)
			if !ok || poll.PollID != pollID {
				continue
			}
			if err := check(poll); err != nil {
				return nil, err
			}
			return poll, nil
		}
		select {
		case <-account.capture.notified:
		case <-timer.C:
			return nil, failure(assertion, "LIVE_POLL_UPDATE_TIMEOUT")
		case <-ctx.Done():
			return nil, failure(assertion, "DEADLINE_EXCEEDED")
		}
	}
}

func (p *probe) expectChannelRPCError(ctx context.Context, account *probeAccount, assertion, wantName string, wantCode int, call func(context.Context, *tg.Client) error) error {
	var rpcErr error
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		rpcErr = call(ctx, api)
		return nil
	}); err != nil {
		return err
	}
	return expectRPCError(assertion, wantName, wantCode, rpcErr)
}

func (p *probe) channelVoterPages(ctx context.Context, account *probeAccount, channelName string, messageID int, wantFirst, wantSecond int64) (int, error) {
	var count int
	if err := p.withAccount(ctx, account, "channel_public_voter_pagination", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := p.channelPeer(channelName, account.username)
		if err != nil {
			return err
		}
		first, err := api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: peer, ID: messageID, Limit: maxPollVotesPage})
		if err != nil {
			return rpcFailure("channel_public_voter_pagination", err)
		}
		if first == nil || first.Count != 2 || len(first.Votes) != 1 || first.NextOffset == "" {
			return failure("channel_public_voter_pagination", "FIRST_PAGE_INVALID")
		}
		firstID, ok := getVoterID(first.Votes[0])
		if !ok {
			return failure("channel_public_voter_pagination", "VOTER_IDENTITY_INVALID")
		}
		second, err := api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{
			Peer: peer, ID: messageID, Limit: maxPollVotesPage, Offset: first.NextOffset,
		})
		if err != nil {
			return rpcFailure("channel_public_voter_pagination", err)
		}
		if second == nil || second.Count != 2 || len(second.Votes) != 1 || second.NextOffset != "" {
			return failure("channel_public_voter_pagination", "SECOND_PAGE_INVALID")
		}
		secondID, ok := getVoterID(second.Votes[0])
		if !ok || firstID == secondID || !sameIDs(firstID, secondID, wantFirst, wantSecond) {
			return failure("channel_public_voter_pagination", "VOTER_PAGES_MISMATCH")
		}
		count = first.Count
		return nil
	}); err != nil {
		return 0, err
	}
	return count, nil
}

func (p *probe) checkNonVoterQuiz(ctx context.Context, account *probeAccount, channelName string, messageID int, pollID int64, assertion string) error {
	message, err := p.channelMessage(ctx, account, channelName, messageID, assertion)
	if err != nil {
		return err
	}
	media, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok || media.Poll.ID != pollID || !media.Poll.Quiz || !nonVoterQuizResults(media.Results) {
		return failure(assertion, "QUIZ_NON_VOTER_RESULTS_MISMATCH")
	}
	return nil
}

func nonVoterQuizResults(results tg.PollResults) bool {
	if results.Solution != "" || hasVoterIdentity(&results) {
		return false
	}
	for _, answer := range results.Results {
		if answer.Correct || answer.Chosen {
			return false
		}
	}
	return len(results.Results) == 2
}

func voterQuizResultsMatch(results *tg.PollResults, solution string) bool {
	if results == nil || results.TotalVoters != 1 || results.Solution != solution || len(results.Results) != 2 || hasVoterIdentity(results) {
		return false
	}
	correct, found := answerByOption(results, "B")
	if !found || !correct.Correct || !correct.Chosen {
		return false
	}
	other, found := answerByOption(results, "A")
	return found && !other.Correct && !other.Chosen
}

func answerByOption(results *tg.PollResults, option string) (tg.PollAnswerVoters, bool) {
	if results == nil {
		return tg.PollAnswerVoters{}, false
	}
	for _, answer := range results.Results {
		if string(answer.Option) == option {
			return answer, true
		}
	}
	return tg.PollAnswerVoters{}, false
}

func (p *probe) verifyDefaultPollBan(ctx context.Context, owner, member *probeAccount, channelName, assertion string) error {
	beforePts, beforeHistory, err := p.channelSnapshot(ctx, owner, channelName, assertion)
	if err != nil {
		return err
	}
	if err := p.setDefaultPollBan(ctx, owner, channelName, true, assertion); err != nil {
		return err
	}
	writeErr := p.expectChannelRPCError(ctx, member, assertion, "CHAT_WRITE_FORBIDDEN", 403, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(channelName, member.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Media: channelPollMedia("Rejected by default poll ban", false, false), RandomID: 1246002,
		})
		return err
	})
	afterPts, afterHistory, snapshotErr := p.channelSnapshot(ctx, owner, channelName, assertion)
	restoreErr := p.setDefaultPollBan(ctx, owner, channelName, false, assertion)
	if restoreErr != nil {
		return restoreErr
	}
	if writeErr != nil {
		return writeErr
	}
	if snapshotErr != nil {
		return snapshotErr
	}
	if beforePts != afterPts || beforeHistory != afterHistory {
		return failureWithFields(assertion, "DENIED_WRITE_CHANGED_STATE", fmt.Sprintf("pts_delta=%d", afterPts-beforePts), fmt.Sprintf("messages_delta=%d", afterHistory-beforeHistory))
	}
	return p.pass(assertion, "rpc_error=CHAT_WRITE_FORBIDDEN", "rpc_code=403", "pts_delta=0", "messages_delta=0")
}

func channelPollMedia(question string, public, quiz bool) *tg.InputMediaPoll {
	poll := tg.Poll{
		Question: tg.TextWithEntities{Text: question},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "A"}, Option: []byte("A")},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: "B"}, Option: []byte("B")},
		},
	}
	poll.SetPublicVoters(public)
	poll.SetQuiz(quiz)
	media := &tg.InputMediaPoll{Poll: poll}
	if quiz {
		media.SetCorrectAnswers([]int{1})
		media.SetSolution("A and B are correct")
	}
	return media
}

func (p *probe) setDefaultPollBan(ctx context.Context, owner *probeAccount, channelName string, banned bool, assertion string) error {
	return p.withAccount(ctx, owner, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := p.channelPeer(channelName, owner.username)
		if err != nil {
			return err
		}
		rights := tg.ChatBannedRights{SendPolls: banned}
		_, err = api.MessagesEditChatDefaultBannedRights(ctx, &tg.MessagesEditChatDefaultBannedRightsRequest{Peer: peer, BannedRights: rights})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		return nil
	})
}

func (p *probe) verifyChannelMemberCloseDenied(ctx context.Context, owner, member *probeAccount, channelName string, messageID int, pollID int64, assertion string) error {
	beforePts, beforeHistory, err := p.channelSnapshot(ctx, owner, channelName, assertion)
	if err != nil {
		return err
	}
	err = p.expectChannelRPCError(ctx, member, assertion, "MESSAGE_ID_INVALID", 400, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(channelName, member.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesEditMessage(ctx, closedPollRequest(peer, messageID))
		return err
	})
	if err != nil {
		return err
	}
	afterPts, afterHistory, err := p.channelSnapshot(ctx, owner, channelName, assertion)
	if err != nil {
		return err
	}
	if beforePts != afterPts || beforeHistory != afterHistory || pollID <= 0 {
		return failureWithFields(assertion, "DENIED_WRITE_CHANGED_STATE", fmt.Sprintf("pts_delta=%d", afterPts-beforePts), fmt.Sprintf("messages_delta=%d", afterHistory-beforeHistory))
	}
	return p.pass(assertion, "rpc_error=MESSAGE_ID_INVALID", "rpc_code=400", "pts_delta=0", "messages_delta=0")
}

func (p *probe) promoteChannelAdmin(ctx context.Context, owner, admin *probeAccount, channelName, assertion string) error {
	peer := p.peersFromA[admin.username]
	if !p.safePeer(admin.username, peer) {
		return failure(assertion, "ADMIN_OUTSIDE_ALLOWLIST")
	}
	return p.withAccount(ctx, owner, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		channel, err := p.channelInput(channelName, owner.username)
		if err != nil {
			return err
		}
		_, err = api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
			Channel: channel,
			UserID:  &tg.InputUser{UserID: peer.userID, AccessHash: peer.accessHash},
			AdminRights: tg.ChatAdminRights{
				ChangeInfo: true,
			},
		})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		return nil
	})
}

func (p *probe) editChannelPoll(ctx context.Context, account *probeAccount, channelName string, messageID int, assertion string) (tg.UpdatesClass, error) {
	var response tg.UpdatesClass
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := p.channelPeer(channelName, account.username)
		if err != nil {
			return err
		}
		response, err = api.MessagesEditMessage(ctx, closedPollRequest(peer, messageID))
		if err != nil {
			return rpcFailure(assertion, err)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return response, nil
}

func (p *probe) closeChannelPoll(ctx context.Context, account *probeAccount, channelName string, messageID int, pollID int64, assertion string) error {
	response, err := p.editChannelPoll(ctx, account, channelName, messageID, assertion)
	if err != nil {
		return err
	}
	if !closedChannelPollInUpdates(response, pollID) {
		return failure(assertion, "POLL_CLOSE_UPDATE_MISSING")
	}
	return nil
}

func closedChannelPollInUpdates(result tg.UpdatesClass, pollID int64) bool {
	for _, update := range updateList(result) {
		edit, ok := update.(*tg.UpdateEditChannelMessage)
		if !ok {
			continue
		}
		message, ok := edit.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if ok && media.Poll.ID == pollID && media.Poll.Closed {
			return true
		}
	}
	return false
}

func (p *probe) recoverAndRestrictSupergroup(ctx context.Context, owner, voter, observer, outsider *probeAccount, p1MessageID int, p1PollID int64) error {
	baselinePts, _, err := p.channelSnapshot(ctx, observer, supergroupChannelName, "channel_difference_recovery")
	if err != nil {
		return err
	}
	if err := p.stopAccount(ctx, observer, "channel_difference_recovery"); err != nil {
		return err
	}
	p4MessageID, p4PollID, err := p.sendChannelPoll(ctx, owner, supergroupChannelName, "Supergroup recovery probe", false, false, "channel_difference_recovery")
	if err != nil {
		return err
	}
	p4Vote, err := p.castChannelVote(ctx, voter, supergroupChannelName, p4MessageID, p4PollID, []byte("B"), "channel_difference_recovery")
	if err != nil {
		return err
	}
	if !anonymousChannelVoteMatches(p4Vote, p4PollID, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("channel_difference_recovery", "VOTE_UPDATE_MISMATCH")
	}
	if err := p.closeChannelPoll(ctx, owner, supergroupChannelName, p1MessageID, p1PollID, "channel_difference_recovery"); err != nil {
		return err
	}
	if err := p.reconnectAccount(ctx, observer, "channel_difference_recovery"); err != nil {
		return err
	}
	difference, err := p.getChannelDifference(ctx, observer, supergroupChannelName, baselinePts, "channel_difference_recovery")
	if err != nil {
		return err
	}
	if !difference.final || difference.empty || len(difference.messages)+len(difference.updates) > channelDifferenceLimit {
		return failure("channel_difference_recovery", "CHANNEL_DIFFERENCE_INVALID")
	}
	p4Recovered, foundP4 := pollMessageResults(difference.messages, p4PollID)
	p1Closed, foundP1 := closedChannelPollResults(difference.updates, p1PollID)
	if !foundP4 || !anonymousChannelResultsMatch(p4Recovered, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("channel_difference_recovery", "NEW_POLL_RESULTS_NOT_RECOVERED")
	}
	if !foundP1 || !p1Closed.Poll.Closed || !anonymousChannelResultsMatch(&p1Closed.Results, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("channel_difference_recovery", "CLOSED_POLL_RESULTS_NOT_RECOVERED")
	}
	if containsPollVoteUpdate(difference.updates, p4PollID) {
		return failure("channel_difference_recovery", "VOTE_EVENT_REQUIRED_FOR_RECOVERY")
	}
	observerPeer, err := p.channelPeer(supergroupChannelName, observer.username)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		messageID int
		pollID    int64
	}{{p1MessageID, p1PollID}, {p4MessageID, p4PollID}} {
		results, readErr := p.readPollResults(ctx, observer, observerPeer, item.messageID, item.pollID, "channel_difference_recovery")
		if readErr != nil {
			return readErr
		}
		if !anonymousChannelResultsMatch(results, 1, map[string]int{"A": 0, "B": 1}) {
			return failure("channel_difference_recovery", "POLL_RESULTS_READ_RECOVERY_MISMATCH")
		}
	}
	p1Read, err := p.channelMessage(ctx, observer, supergroupChannelName, p1MessageID, "channel_difference_recovery")
	if err != nil {
		return err
	}
	p1Media, ok := p1Read.Media.(*tg.MessageMediaPoll)
	if !ok || !p1Media.Poll.Closed || p1Media.Poll.ID != p1PollID || !anonymousChannelResultsMatch(&p1Media.Results, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("channel_difference_recovery", "MESSAGE_READ_RECOVERY_MISMATCH")
	}
	if err := p.pass("channel_difference_recovery", "final=1", "messages=1", "updates=1", "votes=1"); err != nil {
		return err
	}

	if err := p.banChannelMember(ctx, owner, observer, supergroupChannelName, "channel_removal_capture_suppressed"); err != nil {
		return err
	}
	beforeCapture := p.channelUpdateSnapshot(observer.username)
	if countCapturedPoll(beforeCapture, p4PollID) != 0 {
		return failure("channel_removal_capture_suppressed", "PRE_BAN_POLL_CAPTURED")
	}
	aVote, err := p.castChannelVote(ctx, owner, supergroupChannelName, p4MessageID, p4PollID, []byte("A"), "channel_removal_capture_suppressed")
	if err != nil {
		return err
	}
	if !anonymousChannelVoteMatches(aVote, p4PollID, 2, map[string]int{"A": 1, "B": 1}) {
		return failure("channel_removal_capture_suppressed", "VOTE_UPDATE_MISMATCH")
	}
	if err := p.waitForNoCapturedPoll(ctx, observer, p4PollID, len(beforeCapture), channelNegativeWindow, "channel_removal_capture_suppressed"); err != nil {
		return err
	}
	if err := p.pass("channel_removal_capture_suppressed", "updates=0"); err != nil {
		return err
	}
	if err := p.expectDeniedChannelPollRPCs(ctx, observer, supergroupChannelName, observer.username, p4MessageID, p4PollID, "channel_removed_member_denied"); err != nil {
		return err
	}
	if err := p.pass("channel_removed_member_denied", "rpcs=5", "rpc_error=PEER_ID_INVALID", "rpc_code=400"); err != nil {
		return err
	}
	if err := p.verifyChannelOutsider(ctx, outsider, supergroupChannelName, "channel_outsider_denied", p4MessageID, p4PollID); err != nil {
		return err
	}
	return p.verifyRepeatedChannelClose(ctx, owner, voter, supergroupChannelName, p1MessageID, p1PollID, "channel_repeated_close_idempotent", "channel_closed_poll_vote_denied", "channel_closed_poll_reconnect")
}

type channelDifferenceView struct {
	final    bool
	empty    bool
	pts      int
	messages []tg.MessageClass
	updates  []tg.UpdateClass
}

func (p *probe) getChannelDifference(ctx context.Context, account *probeAccount, channelName string, pts int, assertion string) (channelDifferenceView, error) {
	var view channelDifferenceView
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		input, err := p.channelInput(channelName, account.username)
		if err != nil {
			return err
		}
		result, err := api.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: input, Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: pts, Limit: channelDifferenceLimit,
		})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		switch difference := result.(type) {
		case *tg.UpdatesChannelDifference:
			view = channelDifferenceView{final: difference.Final, pts: difference.Pts, messages: difference.NewMessages, updates: difference.OtherUpdates}
		case *tg.UpdatesChannelDifferenceEmpty:
			view = channelDifferenceView{final: difference.Final, empty: true, pts: difference.Pts}
		default:
			return failure(assertion, "CHANNEL_DIFFERENCE_RESPONSE_INVALID")
		}
		return nil
	}); err != nil {
		return channelDifferenceView{}, err
	}
	return view, nil
}

func pollMessageResults(messages []tg.MessageClass, pollID int64) (*tg.PollResults, bool) {
	var found *tg.PollResults
	for _, item := range messages {
		message, ok := item.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || media.Poll.ID != pollID {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = &media.Results
	}
	return found, found != nil
}

func closedChannelPollResults(updates []tg.UpdateClass, pollID int64) (*tg.MessageMediaPoll, bool) {
	var found *tg.MessageMediaPoll
	for _, update := range updates {
		edit, ok := update.(*tg.UpdateEditChannelMessage)
		if !ok {
			continue
		}
		message, ok := edit.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || media.Poll.ID != pollID || !media.Poll.Closed {
			continue
		}
		if found != nil {
			return nil, false
		}
		found = media
	}
	return found, found != nil
}

func containsPollVoteUpdate(updates []tg.UpdateClass, pollID int64) bool {
	for _, update := range updates {
		if poll, ok := update.(*tg.UpdateMessagePoll); ok && poll.PollID == pollID {
			return true
		}
	}
	return false
}

func (p *probe) banChannelMember(ctx context.Context, owner, member *probeAccount, channelName, assertion string) error {
	memberPeer := p.peersFromA[member.username]
	if !p.safePeer(member.username, memberPeer) {
		return failure(assertion, "MEMBER_OUTSIDE_ALLOWLIST")
	}
	return p.withAccount(ctx, owner, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		channel, err := p.channelInput(channelName, owner.username)
		if err != nil {
			return err
		}
		_, err = api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:      channel,
			Participant:  &tg.InputPeerUser{UserID: memberPeer.userID, AccessHash: memberPeer.accessHash},
			BannedRights: tg.ChatBannedRights{ViewMessages: true},
		})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		return nil
	})
}

func (p *probe) waitForNoCapturedPoll(ctx context.Context, account *probeAccount, pollID int64, baseline int, window time.Duration, assertion string) error {
	if account == nil || account.capture == nil || pollID <= 0 {
		return failure(assertion, "POLL_CAPTURE_NOT_CONFIGURED")
	}
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		updates := account.capture.Snapshot()
		if countCapturedPoll(updates[baseline:], pollID) != 0 {
			return failure(assertion, "REMOVED_MEMBER_CAPTURED_POLL")
		}
		select {
		case <-account.capture.notified:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return failure(assertion, "DEADLINE_EXCEEDED")
		}
	}
}

func countCapturedPoll(updates []tg.UpdateClass, pollID int64) int {
	count := 0
	for _, update := range updates {
		if captured, ok := capturedChannelPollID(update); ok && captured == pollID {
			count++
		}
	}
	return count
}

func (p *probe) expectDeniedChannelPollRPCs(ctx context.Context, account *probeAccount, channelName, referenceUser string, messageID int, pollID int64, assertion string) error {
	peer, err := p.channelPeer(channelName, referenceUser)
	if err != nil {
		return err
	}
	input, err := p.channelInput(channelName, referenceUser)
	if err != nil {
		return err
	}
	checks := []func(context.Context, *tg.Client) error{
		func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesGetPollResults(ctx, &tg.MessagesGetPollResultsRequest{Peer: peer, MsgID: messageID})
			return err
		},
		func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: peer, MsgID: messageID, Options: [][]byte{[]byte("A")}})
			return err
		},
		func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: peer, ID: messageID, Limit: 1})
			return err
		},
		func(ctx context.Context, api *tg.Client) error {
			_, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: input, ID: []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}}})
			return err
		},
		func(ctx context.Context, api *tg.Client) error {
			_, err := api.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
				Channel: input, Filter: &tg.ChannelMessagesFilterEmpty{}, Pts: 0, Limit: channelDifferenceLimit,
			})
			return err
		},
	}
	for _, check := range checks {
		if err := p.expectChannelRPCError(ctx, account, assertion, "PEER_ID_INVALID", 400, check); err != nil {
			return err
		}
	}
	if pollID <= 0 {
		return failure(assertion, "POLL_ID_INVALID")
	}
	return nil
}

func (p *probe) verifyChannelOutsider(ctx context.Context, outsider *probeAccount, channelName, assertion string, messageID int, pollID int64) error {
	stateBefore, err := p.getState(ctx, outsider, assertion)
	if err != nil {
		return err
	}
	if err := p.expectDeniedChannelPollRPCs(ctx, outsider, channelName, "synthpoll_a", messageID, pollID, assertion); err != nil {
		return err
	}
	stateAfter, err := p.getState(ctx, outsider, assertion)
	if err != nil {
		return err
	}
	if stateAfter.Pts != stateBefore.Pts {
		return failureWithFields(assertion, "OUTSIDER_PTS_CHANGED", fmt.Sprintf("pts_delta=%d", stateAfter.Pts-stateBefore.Pts))
	}
	return p.pass(assertion, "rpcs=5", "rpc_error=PEER_ID_INVALID", "rpc_code=400", "pts_delta=0")
}

func (p *probe) verifyBroadcastRejectedWrites(ctx context.Context, owner, subscriber *probeAccount) error {
	beforePts, beforeHistory, err := p.channelSnapshot(ctx, owner, broadcastChannelName, "broadcast_public_voters_denied")
	if err != nil {
		return err
	}
	if err := p.expectChannelRPCError(ctx, owner, "broadcast_public_voters_denied", "BROADCAST_PUBLIC_VOTERS_FORBIDDEN", 400, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(broadcastChannelName, owner.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Media: channelPollMedia("Rejected public voters poll", true, false), RandomID: 1246003,
		})
		return err
	}); err != nil {
		return err
	}
	afterPts, afterHistory, err := p.channelSnapshot(ctx, owner, broadcastChannelName, "broadcast_public_voters_denied")
	if err != nil {
		return err
	}
	if beforePts != afterPts || beforeHistory != afterHistory {
		return failure("broadcast_public_voters_denied", "DENIED_WRITE_CHANGED_STATE")
	}
	if err := p.pass("broadcast_public_voters_denied", "rpc_error=BROADCAST_PUBLIC_VOTERS_FORBIDDEN", "rpc_code=400", "pts_delta=0", "messages_delta=0"); err != nil {
		return err
	}
	if err := p.expectChannelRPCError(ctx, subscriber, "broadcast_subscriber_post_denied", "PEER_ID_INVALID", 400, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(broadcastChannelName, subscriber.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peer, Media: channelPollMedia("Rejected subscriber poll", false, false), RandomID: 1246004,
		})
		return err
	}); err != nil {
		return err
	}
	afterPts, afterHistory, err = p.channelSnapshot(ctx, owner, broadcastChannelName, "broadcast_subscriber_post_denied")
	if err != nil {
		return err
	}
	if beforePts != afterPts || beforeHistory != afterHistory {
		return failure("broadcast_subscriber_post_denied", "DENIED_WRITE_CHANGED_STATE")
	}
	return p.pass("broadcast_subscriber_post_denied", "rpc_error=PEER_ID_INVALID", "rpc_code=400", "pts_delta=0", "messages_delta=0")
}

func (p *probe) recoverBroadcastClose(ctx context.Context, owner, voter, observer *probeAccount) (int, int64, error) {
	baselinePts, _, err := p.channelSnapshot(ctx, observer, broadcastChannelName, "broadcast_close_recovery")
	if err != nil {
		return 0, 0, err
	}
	if err := p.stopAccount(ctx, observer, "broadcast_close_recovery"); err != nil {
		return 0, 0, err
	}
	messageID, pollID, err := p.sendChannelPoll(ctx, owner, broadcastChannelName, "Broadcast close recovery probe", false, false, "broadcast_close_recovery")
	if err != nil {
		return 0, 0, err
	}
	if err := p.closeChannelPoll(ctx, owner, broadcastChannelName, messageID, pollID, "broadcast_close_recovery"); err != nil {
		return 0, 0, err
	}
	if err := p.reconnectAccount(ctx, observer, "broadcast_close_recovery"); err != nil {
		return 0, 0, err
	}
	difference, err := p.getChannelDifference(ctx, observer, broadcastChannelName, baselinePts, "broadcast_close_recovery")
	if err != nil {
		return 0, 0, err
	}
	messageResults, foundMessage := pollMessageResults(difference.messages, pollID)
	closed, foundClosed := closedChannelPollResults(difference.updates, pollID)
	if !difference.final || difference.empty || len(difference.messages)+len(difference.updates) > channelDifferenceLimit || !foundMessage || !foundClosed {
		return 0, 0, failure("broadcast_close_recovery", "CHANNEL_DIFFERENCE_INVALID")
	}
	if closed == nil || !closed.Poll.Closed || !pollCountsMatch(messageResults, 0, map[string]int{"A": 0, "B": 0}) || containsPollVoteUpdate(difference.updates, pollID) {
		return 0, 0, failure("broadcast_close_recovery", "CLOSED_POLL_NOT_RECOVERED")
	}
	message, err := p.channelMessage(ctx, observer, broadcastChannelName, messageID, "broadcast_close_recovery")
	if err != nil {
		return 0, 0, err
	}
	media, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok || media.Poll.ID != pollID || !media.Poll.Closed {
		return 0, 0, failure("broadcast_close_recovery", "CLOSED_STATE_NOT_PERSISTED")
	}
	if err := p.pass("broadcast_close_recovery", "final=1", "messages=1", "updates=1"); err != nil {
		return 0, 0, err
	}
	return messageID, pollID, nil
}

func (p *probe) verifyRepeatedChannelClose(ctx context.Context, owner, voter *probeAccount, channelName string, messageID int, pollID int64, repeatedAssertion, voteAssertion, reconnectAssertion string) error {
	ptsOwner, historyOwner, err := p.channelSnapshot(ctx, owner, channelName, repeatedAssertion)
	if err != nil {
		return err
	}
	ptsVoter, historyVoter, err := p.channelSnapshot(ctx, voter, channelName, repeatedAssertion)
	if err != nil {
		return err
	}
	response, err := p.editChannelPoll(ctx, owner, channelName, messageID, repeatedAssertion)
	if err != nil {
		return err
	}
	updates, ok := response.(*tg.Updates)
	if !ok || len(updates.Updates) != 0 {
		return failure(repeatedAssertion, "SECOND_EDIT_UPDATE")
	}
	afterPtsOwner, afterHistoryOwner, err := p.channelSnapshot(ctx, owner, channelName, repeatedAssertion)
	if err != nil {
		return err
	}
	afterPtsVoter, afterHistoryVoter, err := p.channelSnapshot(ctx, voter, channelName, repeatedAssertion)
	if err != nil {
		return err
	}
	if ptsOwner != afterPtsOwner || historyOwner != afterHistoryOwner || ptsVoter != afterPtsVoter || historyVoter != afterHistoryVoter {
		return failureWithFields(repeatedAssertion, "SECOND_CLOSE_CHANGED_STATE", fmt.Sprintf("pts_delta=%d", afterPtsOwner-ptsOwner), fmt.Sprintf("messages_delta=%d", afterHistoryOwner-historyOwner))
	}
	ownerDiff, err := p.getChannelDifference(ctx, owner, channelName, ptsOwner, repeatedAssertion)
	if err != nil {
		return err
	}
	voterDiff, err := p.getChannelDifference(ctx, voter, channelName, ptsVoter, repeatedAssertion)
	if err != nil {
		return err
	}
	if !emptyChannelDifference(ownerDiff) || !emptyChannelDifference(voterDiff) {
		return failure(repeatedAssertion, "POST_CLOSE_DIFFERENCE_NOT_EMPTY")
	}
	if err := p.pass(repeatedAssertion, "updates=0", "differences=0", "pts_delta=0", "messages_delta=0"); err != nil {
		return err
	}
	if err := p.expectChannelRPCError(ctx, voter, voteAssertion, "MESSAGE_POLL_CLOSED", 400, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(channelName, voter.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: peer, MsgID: messageID, Options: [][]byte{[]byte("A")}})
		return err
	}); err != nil {
		return err
	}
	if err := p.pass(voteAssertion, "rpc_error=MESSAGE_POLL_CLOSED", "rpc_code=400"); err != nil {
		return err
	}
	if err := p.reconnectAccount(ctx, voter, reconnectAssertion); err != nil {
		return err
	}
	message, err := p.channelMessage(ctx, voter, channelName, messageID, reconnectAssertion)
	if err != nil {
		return err
	}
	media, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok || media.Poll.ID != pollID || !media.Poll.Closed {
		return failure(reconnectAssertion, "CLOSED_STATE_NOT_PERSISTED")
	}
	return p.pass(reconnectAssertion, "polls=1", "session=preserved")
}

func emptyChannelDifference(difference channelDifferenceView) bool {
	return difference.final && (difference.empty || len(difference.messages) == 0 && len(difference.updates) == 0)
}

func (p *probe) runBroadcastPolls(ctx context.Context) error {
	a, b, c, d := p.account("synthpoll_a"), p.account("synthpoll_b"), p.account("synthpoll_c"), p.account("synthpoll_d")
	if err := p.verifyBroadcastRejectedWrites(ctx, a, c); err != nil {
		return err
	}
	messageID, pollID, err := p.sendChannelPoll(ctx, a, broadcastChannelName, "Broadcast probe poll", false, false, "broadcast_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	vote, err := p.castChannelVote(ctx, b, broadcastChannelName, messageID, pollID, []byte("B"), "broadcast_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	if !anonymousChannelVoteMatches(vote, pollID, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("broadcast_anonymous_poll_privacy", "VOTE_UPDATE_MISMATCH")
	}
	creatorPeer, err := p.channelPeer(broadcastChannelName, a.username)
	if err != nil {
		return err
	}
	results, err := p.readPollResults(ctx, a, creatorPeer, messageID, pollID, "broadcast_anonymous_poll_privacy")
	if err != nil {
		return err
	}
	if !anonymousChannelResultsMatch(results, 1, map[string]int{"A": 0, "B": 1}) {
		return failure("broadcast_anonymous_poll_privacy", "POLL_RESULT_MISMATCH")
	}
	if _, err := p.awaitChannelPoll(ctx, c, pollID, "broadcast_anonymous_poll_privacy", func(update *tg.UpdateMessagePoll) error {
		if !anonymousChannelNonVoterUpdateMatches(update, pollID, 1, map[string]int{"A": 0, "B": 1}) {
			return failure("broadcast_anonymous_poll_privacy", "LIVE_RESULT_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := p.awaitChannelPoll(ctx, b, pollID, "broadcast_anonymous_poll_privacy", func(update *tg.UpdateMessagePoll) error {
		if !anonymousChannelVoteMatches(update, pollID, 1, map[string]int{"A": 0, "B": 1}) {
			return failure("broadcast_anonymous_poll_privacy", "LIVE_RESULT_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.expectChannelRPCError(ctx, b, "broadcast_anonymous_non_voter_denied", "POLL_VOTE_REQUIRED", 403, func(ctx context.Context, api *tg.Client) error {
		peer, err := p.channelPeer(broadcastChannelName, b.username)
		if err != nil {
			return err
		}
		_, err = api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: peer, ID: messageID, Limit: 1})
		return err
	}); err != nil {
		return err
	}
	if err := p.pass("broadcast_anonymous_non_voter_denied", "rpc_error=POLL_VOTE_REQUIRED", "rpc_code=403"); err != nil {
		return err
	}
	if err := p.pass("broadcast_anonymous_poll_privacy", "voters=1", "voter_ids=0", "chosen=0"); err != nil {
		return err
	}

	quizMessageID, quizPollID, err := p.sendChannelPoll(ctx, a, broadcastChannelName, "Broadcast probe quiz", false, true, "broadcast_quiz_privacy")
	if err != nil {
		return err
	}
	if err := p.checkNonVoterQuiz(ctx, c, broadcastChannelName, quizMessageID, quizPollID, "broadcast_quiz_privacy"); err != nil {
		return err
	}
	quizVote, err := p.castChannelVote(ctx, b, broadcastChannelName, quizMessageID, quizPollID, []byte("B"), "broadcast_quiz_privacy")
	if err != nil {
		return err
	}
	if !voterQuizResultsMatch(&quizVote.Results, "A and B are correct") {
		return failure("broadcast_quiz_privacy", "QUIZ_VOTER_RESULTS_MISMATCH")
	}
	if _, err := p.awaitChannelPoll(ctx, c, quizPollID, "broadcast_quiz_privacy", func(update *tg.UpdateMessagePoll) error {
		if update.Results.TotalVoters != 1 || !nonVoterQuizResults(update.Results) || hasVoterIdentity(&update.Results) {
			return failure("broadcast_quiz_privacy", "QUIZ_NON_VOTER_RESULTS_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if _, err := p.awaitChannelPoll(ctx, b, quizPollID, "broadcast_quiz_privacy", func(update *tg.UpdateMessagePoll) error {
		if !voterQuizResultsMatch(&update.Results, "A and B are correct") {
			return failure("broadcast_quiz_privacy", "QUIZ_VOTER_RESULTS_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.checkNonVoterQuiz(ctx, c, broadcastChannelName, quizMessageID, quizPollID, "broadcast_quiz_privacy"); err != nil {
		return err
	}
	if err := p.pass("broadcast_quiz_privacy", "voters=1", "correct_visible_to_voter=1", "correct_visible_to_non_voter=0"); err != nil {
		return err
	}
	if err := p.verifyChannelMemberCloseDenied(ctx, a, c, broadcastChannelName, messageID, pollID, "broadcast_member_close_denied"); err != nil {
		return err
	}
	if err := p.promoteChannelAdmin(ctx, a, b, broadcastChannelName, "broadcast_admin_close"); err != nil {
		return err
	}
	if err := p.closeChannelPoll(ctx, b, broadcastChannelName, messageID, pollID, "broadcast_admin_close"); err != nil {
		return err
	}
	if err := p.pass("broadcast_admin_close", "sender_or_admin=1"); err != nil {
		return err
	}
	messageID, pollID, err = p.recoverBroadcastClose(ctx, a, b, c)
	if err != nil {
		return err
	}
	if err := p.verifyChannelOutsider(ctx, d, broadcastChannelName, "broadcast_outsider_denied", messageID, pollID); err != nil {
		return err
	}
	return p.verifyRepeatedChannelClose(ctx, a, b, broadcastChannelName, messageID, pollID, "broadcast_repeated_close_idempotent", "broadcast_closed_poll_vote_denied", "broadcast_closed_poll_reconnect")
}
