package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	gotdauth "github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

type probeAccount struct {
	username   string
	password   []byte
	session    *session.StorageMemory
	client     *telegram.Client
	api        *tg.Client
	cancel     context.CancelFunc
	done       chan struct{}
	userID     int64
	authorized bool
}

type resolvedPeer struct {
	userID     int64
	accessHash int64
}

type probe struct {
	endpoint   endpoint
	publicKey  *rsa.PublicKey
	accounts   [4]*probeAccount
	peersFromA map[string]resolvedPeer
	peerAFromB resolvedPeer
	groupID    int64
	groupMade  bool
	output     io.Writer
}

func newProbe(config probeConfig, output io.Writer) *probe {
	p := &probe{
		endpoint:   config.endpoint,
		publicKey:  config.publicKey,
		peersFromA: make(map[string]resolvedPeer, 3),
		output:     output,
	}
	for index, credential := range config.creds {
		p.accounts[index] = &probeAccount{
			username: credential.username,
			password: credential.password,
			session:  &session.StorageMemory{},
		}
	}
	return p
}

func (p *probe) execute(ctx context.Context) error {
	if err := p.authenticateAccounts(ctx); err != nil {
		return err
	}
	if err := p.resolvePeers(ctx); err != nil {
		return err
	}
	if err := p.runGroup(ctx); err != nil {
		return err
	}
	if err := p.runSavedMessages(ctx); err != nil {
		return err
	}
	return p.runTextRoundTrip(ctx)
}

func (p *probe) authenticateAccounts(ctx context.Context) error {
	seen := make(map[int64]bool, len(p.accounts))
	for _, account := range p.accounts {
		if account == nil || !fixedUsername(account.username) {
			return failure("account_identities", "UNEXPECTED_ACCOUNT_CONFIGURATION")
		}
		if err := p.startAccount(ctx, account, true); err != nil {
			return err
		}
		if seen[account.userID] {
			return failure("account_identities", "DUPLICATE_ACCOUNT_IDENTITY")
		}
		seen[account.userID] = true
	}
	return p.pass("account_identities", "count=4")
}

func (p *probe) startAccount(ctx context.Context, account *probeAccount, authenticate bool) error {
	runCtx, cancel := context.WithCancel(ctx)
	client := p.newClient(account)
	ready := make(chan error, 1)
	done := make(chan struct{})
	account.client, account.api, account.cancel, account.done = client, client.API(), cancel, done
	go func() {
		_ = client.Run(runCtx, func(runCtx context.Context) error {
			initErr := p.pass("session_transport_ready")
			if initErr != nil {
				ready <- initErr
				return initErr
			}
			if authenticate {
				initErr = p.login(runCtx, account, client.API(), client)
			} else {
				initErr = p.verifySession(runCtx, account, client)
			}
			ready <- initErr
			if initErr != nil {
				return initErr
			}
			<-runCtx.Done()
			return nil
		})
		close(done)
	}()
	select {
	case err := <-ready:
		return err
	case <-ctx.Done():
		cancel()
		return failure("account_authentication", "DEADLINE_EXCEEDED")
	case <-done:
		select {
		case err := <-ready:
			return err
		default:
			return failure("account_authentication", "SESSION_START_FAILED")
		}
	}
}

func (p *probe) login(ctx context.Context, account *probeAccount, api *tg.Client, client *telegram.Client) error {
	sentClass, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: account.username, APIID: probeAppID, APIHash: probeAppHash,
	})
	if err != nil {
		return rpcFailure("account_authentication", err)
	}
	sent, ok := sentClass.(*tg.AuthSentCode)
	if !ok || sent.PhoneCodeHash == "" {
		return failure("account_authentication", "LOGIN_CHALLENGE_INVALID")
	}
	_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{PhoneNumber: account.username, PhoneCodeHash: sent.PhoneCodeHash})
	if !rpcMatches(err, "SESSION_PASSWORD_NEEDED", 401) {
		if err != nil {
			return rpcFailure("account_authentication", err)
		}
		return failure("account_authentication", "PASSWORD_CHALLENGE_MISSING")
	}
	passwordState, err := api.AccountGetPassword(ctx)
	if err != nil {
		return rpcFailure("account_authentication", err)
	}
	if !passwordState.HasPassword || passwordState.CurrentAlgo == nil {
		return failure("account_authentication", "PASSWORD_NOT_CONFIGURED")
	}
	proof, err := gotdauth.PasswordHash(account.password, passwordState.SRPID, passwordState.SRPB, passwordState.SecureRandom, passwordState.CurrentAlgo)
	if err != nil {
		return failure("account_authentication", "PASSWORD_PROOF_INVALID")
	}
	defer clear(proof.A)
	defer clear(proof.M1)
	defer clear(passwordState.SecureRandom)
	response, err := api.AuthCheckPassword(ctx, proof)
	if err != nil {
		return rpcFailure("account_authentication", err)
	}
	authorization, ok := response.(*tg.AuthAuthorization)
	if !ok || authorization.User == nil {
		return failure("account_authentication", "AUTHORIZATION_RESPONSE_INVALID")
	}
	user, ok := authorization.User.(*tg.User)
	if !ok || user.ID <= 0 {
		return failure("account_authentication", "ACCOUNT_IDENTITY_INVALID")
	}
	account.authorized, account.userID = true, user.ID
	return p.verifyUser(ctx, account, client)
}

func (p *probe) verifySession(ctx context.Context, account *probeAccount, client *telegram.Client) error {
	return p.verifyUser(ctx, account, client)
}

func (p *probe) verifyUser(ctx context.Context, account *probeAccount, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return rpcFailure("account_authentication", err)
	}
	if !status.Authorized {
		return failure("account_authentication", "AUTHORIZATION_MISSING")
	}
	self, err := client.Self(ctx)
	if err != nil {
		return rpcFailure("account_identities", err)
	}
	if self == nil || !self.Self || self.ID != account.userID || !strings.EqualFold(self.Username, account.username) {
		return failure("account_identities", "ACCOUNT_IDENTITY_MISMATCH")
	}
	return nil
}

func (p *probe) resolvePeers(ctx context.Context) error {
	a := p.account("synthpoll_a")
	if err := p.withAccount(ctx, a, "synthetic_peers_resolved", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		for _, username := range []string{"synthpoll_b", "synthpoll_c", "synthpoll_d"} {
			peer, err := resolveSyntheticUsername(ctx, api, username, p.account(username).userID)
			if err != nil {
				return err
			}
			p.peersFromA[username] = peer
		}
		return nil
	}); err != nil {
		return err
	}
	b := p.account("synthpoll_b")
	if err := p.withAccount(ctx, b, "synthetic_peers_resolved", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		peer, err := resolveSyntheticUsername(ctx, api, "synthpoll_a", a.userID)
		if err != nil {
			return err
		}
		p.peerAFromB = peer
		return nil
	}); err != nil {
		return err
	}
	return p.pass("synthetic_peers_resolved", "count=4")
}

func resolveSyntheticUsername(ctx context.Context, api *tg.Client, username string, expectedID int64) (resolvedPeer, error) {
	if !fixedUsername(username) || expectedID <= 0 {
		return resolvedPeer{}, failure("synthetic_peers_resolved", "PEER_OUTSIDE_ALLOWLIST")
	}
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return resolvedPeer{}, rpcFailure("synthetic_peers_resolved", err)
	}
	peer, ok := resolved.Peer.(*tg.PeerUser)
	if !ok || peer.UserID != expectedID {
		return resolvedPeer{}, failure("synthetic_peers_resolved", "PEER_IDENTITY_MISMATCH")
	}
	if resolved == nil {
		return resolvedPeer{}, failure("synthetic_peers_resolved", "PEER_IDENTITY_MISMATCH")
	}
	var found *tg.User
	for _, candidate := range resolved.Users {
		user, ok := candidate.(*tg.User)
		if ok && user.ID == expectedID {
			if found != nil {
				return resolvedPeer{}, failure("synthetic_peers_resolved", "PEER_IDENTITY_AMBIGUOUS")
			}
			found = user
		}
	}
	if found == nil || !strings.EqualFold(found.Username, username) || found.AccessHash == 0 {
		return resolvedPeer{}, failure("synthetic_peers_resolved", "PEER_IDENTITY_MISMATCH")
	}
	return resolvedPeer{userID: found.ID, accessHash: found.AccessHash}, nil
}

func (p *probe) runGroup(ctx context.Context) error {
	if err := p.createGroup(ctx); err != nil {
		return err
	}
	if err := p.assertGroupMembers(ctx); err != nil {
		return err
	}
	if err := p.runAnonymousPoll(ctx); err != nil {
		return err
	}
	return p.runPublicPoll(ctx)
}

func (p *probe) createGroup(ctx context.Context) error {
	b, c := p.peersFromA["synthpoll_b"], p.peersFromA["synthpoll_c"]
	if !p.safePeer("synthpoll_b", b) || !p.safePeer("synthpoll_c", c) {
		return failure("synthetic_group_created", "PEER_OUTSIDE_ALLOWLIST")
	}
	title := "synthpoll-" + time.Now().UTC().Format("2006-01-02")
	if err := p.withAccount(ctx, p.account("synthpoll_a"), "synthetic_group_created", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		created, err := api.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: title,
			Users: []tg.InputUserClass{
				&tg.InputUser{UserID: b.userID, AccessHash: b.accessHash},
				&tg.InputUser{UserID: c.userID, AccessHash: c.accessHash},
			},
		})
		if err != nil {
			return rpcFailure("synthetic_group_created", err)
		}
		if created == nil || len(created.MissingInvitees) != 0 {
			return failure("synthetic_group_created", "GROUP_CREATION_INCOMPLETE")
		}
		chatID, ok := createdChatID(created.Updates)
		if !ok || chatID <= 0 {
			return failure("synthetic_group_created", "GROUP_ID_INVALID")
		}
		p.groupID, p.groupMade = chatID, true
		return nil
	}); err != nil {
		return err
	}
	return p.pass("synthetic_group_created", "members=3")
}

func (p *probe) assertGroupMembers(ctx context.Context) error {
	if !p.groupMade || p.groupID <= 0 {
		return failure("synthetic_group_members", "GROUP_NOT_CREATED")
	}
	if err := p.withAccount(ctx, p.account("synthpoll_a"), "synthetic_group_members", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.MessagesGetFullChat(ctx, p.groupID)
		if err != nil {
			return rpcFailure("synthetic_group_members", err)
		}
		if result == nil {
			return failure("synthetic_group_members", "GROUP_RESPONSE_INVALID")
		}
		full, ok := result.FullChat.(*tg.ChatFull)
		if !ok || full.ID != p.groupID {
			return failure("synthetic_group_members", "GROUP_RESPONSE_INVALID")
		}
		participants, ok := full.Participants.(*tg.ChatParticipants)
		if !ok || participants.ChatID != p.groupID || len(participants.Participants) != 3 {
			return failure("synthetic_group_members", "GROUP_MEMBERSHIP_MISMATCH")
		}
		members := make(map[int64]bool, 3)
		for _, participant := range participants.Participants {
			members[participant.GetUserID()] = true
		}
		want := map[int64]bool{
			p.account("synthpoll_a").userID: true,
			p.account("synthpoll_b").userID: true,
			p.account("synthpoll_c").userID: true,
		}
		if len(members) != len(want) || members[p.account("synthpoll_d").userID] {
			return failure("synthetic_group_members", "GROUP_MEMBERSHIP_MISMATCH")
		}
		for id := range want {
			if !members[id] {
				return failure("synthetic_group_members", "GROUP_MEMBERSHIP_MISMATCH")
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return p.pass("synthetic_group_members", "members=3 outsiders=1")
}

func (p *probe) runAnonymousPoll(ctx context.Context) error {
	a, b, c, d := p.account("synthpoll_a"), p.account("synthpoll_b"), p.account("synthpoll_c"), p.account("synthpoll_d")
	messageIDA, pollID, err := p.sendPoll(ctx, a, p.chatPeer(), "Synthetic anonymous poll?", false, "A", "B")
	if err != nil {
		return err
	}
	messageIDB, err := p.pollMessageID(ctx, b, p.chatPeer(), pollID, "anonymous_poll_message")
	if err != nil {
		return err
	}
	baseline, err := p.getState(ctx, c, "difference_baseline")
	if err != nil {
		return err
	}
	var nonVoterErr error
	if err := p.withAccount(ctx, a, "anonymous_non_voter_denied", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		_, nonVoterErr = api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: p.chatPeer(), ID: messageIDA, Limit: 1})
		return nil
	}); err != nil {
		return err
	}
	if denied := expectRPCError("anonymous_non_voter_denied", "POLL_VOTE_REQUIRED", 403, nonVoterErr); denied != nil {
		return denied
	}
	if err := p.pass("anonymous_non_voter_denied", "rpc_error=POLL_VOTE_REQUIRED", "rpc_code=403"); err != nil {
		return err
	}
	voteResults, err := p.castVote(ctx, b, p.chatPeer(), messageIDB, pollID, []byte("B"), "anonymous_vote")
	if err != nil {
		return err
	}
	result, err := p.readPollResults(ctx, a, p.chatPeer(), messageIDA, pollID, "anonymous_poll_results")
	if err != nil {
		return err
	}
	counts := map[string]int{"A": 0, "B": 1}
	if !pollCountsMatch(voteResults, 1, counts) {
		return failureWithFields("anonymous_vote_privacy", "VOTE_UPDATE_MISMATCH",
			fmt.Sprintf("actual_total=%d", voteResults.TotalVoters),
			fmt.Sprintf("answer_count=%d", len(voteResults.Results)),
			fmt.Sprintf("first_voters=%d", answerVoterCount(voteResults, 0)),
			fmt.Sprintf("second_voters=%d", answerVoterCount(voteResults, 1)),
		)
	}
	if !pollCountsMatch(result, 1, counts) {
		return failure("anonymous_vote_privacy", "POLL_RESULT_MISMATCH")
	}
	if hasVoterIdentity(result) {
		return failure("anonymous_vote_privacy", "VOTER_IDENTITY_EXPOSED")
	}
	if err := p.pass("anonymous_vote_privacy", "voters=1 voter_ids=0"); err != nil {
		return err
	}
	var outsiderErr error
	if err := p.withAccount(ctx, d, "outsider_vote_denied", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		_, outsiderErr = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: p.chatPeer(), MsgID: messageIDA, Options: [][]byte{[]byte("B")}})
		return nil
	}); err != nil {
		return err
	}
	if denied := expectRPCError("outsider_vote_denied", "PEER_ID_INVALID", 400, outsiderErr); denied != nil {
		return denied
	}
	if err := p.pass("outsider_vote_denied", "rpc_error=PEER_ID_INVALID", "rpc_code=400"); err != nil {
		return err
	}
	if err := p.reconnectAccount(ctx, c, "difference_reconnect"); err != nil {
		return err
	}
	if err := p.pass("difference_reconnect", "session=preserved"); err != nil {
		return err
	}
	pages, recovered, err := p.recoverVote(ctx, c, baseline, pollID)
	if err != nil {
		return err
	}
	if !recovered {
		return failure("difference_recovery", "MISSED_VOTE_NOT_RECOVERED")
	}
	return p.pass("difference_recovery", "votes=1", fmt.Sprintf("pages=%d", pages))
}

func (p *probe) runPublicPoll(ctx context.Context) error {
	a, b, c := p.account("synthpoll_a"), p.account("synthpoll_b"), p.account("synthpoll_c")
	messageIDA, pollID, err := p.sendPoll(ctx, a, p.chatPeer(), "Synthetic public poll?", true, "first", "second")
	if err != nil {
		return err
	}
	messageIDB, err := p.pollMessageID(ctx, b, p.chatPeer(), pollID, "public_poll_message")
	if err != nil {
		return err
	}
	messageIDC, err := p.pollMessageID(ctx, c, p.chatPeer(), pollID, "public_poll_message")
	if err != nil {
		return err
	}
	var nonVoterErr error
	if err := p.withAccount(ctx, a, "public_non_voter_denied", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		_, nonVoterErr = api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: p.chatPeer(), ID: messageIDA, Limit: 1})
		return nil
	}); err != nil {
		return err
	}
	if denied := expectRPCError("public_non_voter_denied", "POLL_VOTE_REQUIRED", 403, nonVoterErr); denied != nil {
		return denied
	}
	if err := p.pass("public_non_voter_denied", "rpc_error=POLL_VOTE_REQUIRED", "rpc_code=403"); err != nil {
		return err
	}
	if result, err := p.castVote(ctx, b, p.chatPeer(), messageIDB, pollID, []byte("first"), "public_poll_votes"); err != nil {
		return err
	} else if !pollCountsMatch(result, 1, map[string]int{"first": 1}) {
		return failure("public_poll_votes", "POLL_RESULT_MISMATCH")
	}
	if result, err := p.castVote(ctx, c, p.chatPeer(), messageIDC, pollID, []byte("second"), "public_poll_votes"); err != nil {
		return err
	} else if !pollCountsMatch(result, 2, map[string]int{"first": 1, "second": 1}) {
		return failure("public_poll_votes", "POLL_RESULT_MISMATCH")
	}
	results, err := p.readPollResults(ctx, a, p.chatPeer(), messageIDA, pollID, "public_poll_results")
	if err != nil {
		return err
	}
	if !pollCountsMatch(results, 2, map[string]int{"first": 1, "second": 1}) {
		return failure("public_poll_results", "POLL_RESULT_MISMATCH")
	}
	if err := p.pass("public_poll_results", "voters=2"); err != nil {
		return err
	}
	count, err := p.voterPages(ctx, b, messageIDB, b.userID, c.userID)
	if err != nil {
		return err
	}
	if err := p.pass("public_voter_pagination", fmt.Sprintf("voters=%d", count), "limit=1", "unique=2"); err != nil {
		return err
	}
	return p.closeAndCheck(ctx, a, b, messageIDA, messageIDB, pollID)
}

func (p *probe) runSavedMessages(ctx context.Context) error {
	a := p.account("synthpoll_a")
	messageID, pollID, err := p.sendPoll(ctx, a, &tg.InputPeerSelf{}, "Synthetic Saved Messages poll?", false, "saved", "unused")
	if err != nil {
		return err
	}
	voteResults, err := p.castVote(ctx, a, &tg.InputPeerSelf{}, messageID, pollID, []byte("saved"), "saved_poll_vote")
	if err != nil {
		return err
	}
	results, err := p.readPollResults(ctx, a, &tg.InputPeerSelf{}, messageID, pollID, "saved_poll_results")
	if err != nil {
		return err
	}
	counts := map[string]int{"saved": 1, "unused": 0}
	if !pollCountsMatch(voteResults, 1, counts) || !pollCountsMatch(results, 1, counts) || hasVoterIdentity(results) {
		return failure("saved_poll_results", "POLL_RESULT_MISMATCH")
	}
	if err := p.closePoll(ctx, a, &tg.InputPeerSelf{}, messageID, pollID, "saved_poll_close"); err != nil {
		return err
	}
	closed, err := p.findPoll(ctx, a, &tg.InputPeerSelf{}, pollID, "saved_poll_results")
	if err != nil {
		return err
	}
	if closed == nil {
		return failure("saved_poll_results", "CLOSED_POLL_RESULT_MISMATCH")
	}
	media, ok := closed.Media.(*tg.MessageMediaPoll)
	if !ok || !media.Poll.Closed || !pollCountsMatch(&media.Results, 1, map[string]int{"saved": 1, "unused": 0}) {
		return failure("saved_poll_results", "CLOSED_POLL_RESULT_MISMATCH")
	}
	return p.pass("saved_poll_lifecycle", "polls=1 votes=1 closes=1")
}

func (p *probe) runTextRoundTrip(ctx context.Context) error {
	a, b := p.account("synthpoll_a"), p.account("synthpoll_b")
	target := p.peersFromA["synthpoll_b"]
	if !p.safePeer("synthpoll_b", target) || !p.safePeer("synthpoll_a", p.peerAFromB) {
		return failure("synthetic_text_round_trip", "PEER_OUTSIDE_ALLOWLIST")
	}
	requestID, err := randomID()
	if err != nil {
		return failure("synthetic_text_round_trip", "RANDOM_ID_UNAVAILABLE")
	}
	messageText := fmt.Sprintf("synthetic poll probe round-trip-%x", requestID)
	if err := p.withAccount(ctx, a, "synthetic_text_round_trip", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerUser{UserID: target.userID, AccessHash: target.accessHash}, Message: messageText, RandomID: requestID,
		})
		if err != nil {
			return rpcFailure("synthetic_text_round_trip", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := p.withAccount(ctx, b, "synthetic_text_round_trip", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerUser{UserID: p.peerAFromB.userID, AccessHash: p.peerAFromB.accessHash}, Limit: maxHistoryMessages,
		})
		if err != nil {
			return rpcFailure("synthetic_text_round_trip", err)
		}
		found := 0
		for _, candidate := range messageList(history) {
			message, ok := candidate.(*tg.Message)
			if !ok || message.Message != messageText {
				continue
			}
			from, ok := message.FromID.(*tg.PeerUser)
			if !ok || from.UserID != a.userID || message.Out {
				return failure("synthetic_text_round_trip", "TEXT_SENDER_MISMATCH")
			}
			found++
		}
		if found != 1 {
			return failure("synthetic_text_round_trip", "TEXT_ROUND_TRIP_MISMATCH")
		}
		return nil
	}); err != nil {
		return err
	}
	return p.pass("synthetic_text_round_trip", "messages=1")
}

func (p *probe) sendPoll(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, question string, public bool, first, second string) (int, int64, error) {
	requestID, err := randomID()
	if err != nil {
		return 0, 0, failure("poll_created", "RANDOM_ID_UNAVAILABLE")
	}
	poll := tg.Poll{
		Question: tg.TextWithEntities{Text: question},
		Answers: []tg.PollAnswerClass{
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: first}, Option: []byte(first)},
			&tg.PollAnswer{Text: tg.TextWithEntities{Text: second}, Option: []byte(second)},
		},
	}
	poll.SetPublicVoters(public)
	var messageID int
	var pollID int64
	if err := p.withAccount(ctx, account, "poll_created", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{Peer: peer, Media: &tg.InputMediaPoll{Poll: poll}, RandomID: requestID})
		if err != nil {
			return rpcFailure("poll_created", err)
		}
		message, ok := newPollMessage(result)
		if !ok {
			return failure("poll_created", "POLL_MESSAGE_MISSING")
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if !ok || media.Poll.ID <= 0 || media.Poll.Question.Text != question || media.Poll.PublicVoters != public || message.ID <= 0 {
			return failure("poll_created", "POLL_MESSAGE_INVALID")
		}
		messageID, pollID = message.ID, media.Poll.ID
		return nil
	}); err != nil {
		return 0, 0, err
	}
	if err := p.pass("poll_created", "polls=1"); err != nil {
		return 0, 0, err
	}
	return messageID, pollID, nil
}

func (p *probe) castVote(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, messageID int, pollID int64, option []byte, assertion string) (*tg.PollResults, error) {
	var results *tg.PollResults
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		response, err := api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: peer, MsgID: messageID, Options: [][]byte{option}})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		var found bool
		results, found = pollResultsFromUpdates(response, pollID)
		if !found || results == nil {
			return failure(assertion, "POLL_RESULTS_MISSING")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

func (p *probe) readPollResults(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, messageID int, pollID int64, assertion string) (*tg.PollResults, error) {
	var results *tg.PollResults
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		response, err := api.MessagesGetPollResults(ctx, &tg.MessagesGetPollResultsRequest{Peer: peer, MsgID: messageID})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		var found bool
		results, found = pollResultsFromUpdates(response, pollID)
		if !found || results == nil {
			return failure(assertion, "POLL_RESULTS_MISSING")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return results, nil
}

func (p *probe) pollMessageID(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, pollID int64, assertion string) (int, error) {
	message, err := p.findPoll(ctx, account, peer, pollID, assertion)
	if err != nil {
		return 0, err
	}
	return message.ID, nil
}

func (p *probe) findPoll(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, pollID int64, assertion string) (*tg.Message, error) {
	var found *tg.Message
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: maxHistoryMessages})
		if err != nil {
			return rpcFailure(assertion, err)
		}
		for _, candidate := range messageList(history) {
			message, ok := candidate.(*tg.Message)
			if !ok {
				continue
			}
			media, ok := message.Media.(*tg.MessageMediaPoll)
			if !ok || media.Poll.ID != pollID {
				continue
			}
			if found != nil {
				return failure(assertion, "POLL_MESSAGE_AMBIGUOUS")
			}
			found = message
		}
		if found == nil {
			return failure(assertion, "POLL_MESSAGE_NOT_FOUND")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return found, nil
}

func (p *probe) getState(ctx context.Context, account *probeAccount, assertion string) (*tg.UpdatesState, error) {
	var state *tg.UpdatesState
	if err := p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.UpdatesGetState(ctx)
		if err != nil {
			return rpcFailure(assertion, err)
		}
		if result == nil || result.Pts < 0 {
			return failure(assertion, "UPDATE_STATE_INVALID")
		}
		state = result
		return nil
	}); err != nil {
		return nil, err
	}
	return state, nil
}

func (p *probe) recoverVote(ctx context.Context, account *probeAccount, baseline *tg.UpdatesState, pollID int64) (int, bool, error) {
	var pages int
	var recovered bool
	if err := p.withAccount(ctx, account, "difference_recovery", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		state := *baseline
		for page := 0; page < maxDifferencePages; page++ {
			result, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: state.Pts, Date: state.Date, Qts: state.Qts, PtsLimit: 100})
			if err != nil {
				return rpcFailure("difference_recovery", err)
			}
			pages++
			switch difference := result.(type) {
			case *tg.UpdatesDifference:
				recovered = hasPollVote(difference.OtherUpdates, pollID)
				if !recovered {
					return failure("difference_recovery", "MISSED_VOTE_NOT_RECOVERED")
				}
				return nil
			case *tg.UpdatesDifferenceSlice:
				if hasPollVote(difference.OtherUpdates, pollID) {
					recovered = true
					return nil
				}
				state = difference.IntermediateState
			case *tg.UpdatesDifferenceEmpty:
				return failure("difference_recovery", "MISSED_VOTE_NOT_RECOVERED")
			default:
				return failure("difference_recovery", "DIFFERENCE_RESPONSE_INVALID")
			}
		}
		return failure("difference_recovery", "DIFFERENCE_PAGE_LIMIT")
	}); err != nil {
		return pages, false, err
	}
	return pages, recovered, nil
}

func (p *probe) voterPages(ctx context.Context, account *probeAccount, messageID int, wantFirst, wantSecond int64) (int, error) {
	var count int
	if err := p.withAccount(ctx, account, "public_voter_pagination", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		first, err := api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: p.chatPeer(), ID: messageID, Limit: maxPollVotesPage})
		if err != nil {
			return rpcFailure("public_voter_pagination", err)
		}
		if first == nil || first.Count != 2 || len(first.Votes) != 1 || first.NextOffset == "" {
			return failure("public_voter_pagination", "FIRST_PAGE_INVALID")
		}
		firstID, ok := getVoterID(first.Votes[0])
		if !ok {
			return failure("public_voter_pagination", "VOTER_IDENTITY_INVALID")
		}
		second, err := api.MessagesGetPollVotes(ctx, &tg.MessagesGetPollVotesRequest{Peer: p.chatPeer(), ID: messageID, Limit: maxPollVotesPage, Offset: first.NextOffset})
		if err != nil {
			return rpcFailure("public_voter_pagination", err)
		}
		if second == nil || second.Count != 2 || len(second.Votes) != 1 || second.NextOffset != "" {
			return failure("public_voter_pagination", "SECOND_PAGE_INVALID")
		}
		secondID, ok := getVoterID(second.Votes[0])
		if !ok || firstID == secondID || !sameIDs(firstID, secondID, wantFirst, wantSecond) {
			return failure("public_voter_pagination", "VOTER_PAGES_MISMATCH")
		}
		count = first.Count
		return nil
	}); err != nil {
		return 0, err
	}
	return count, nil
}

func (p *probe) closeAndCheck(ctx context.Context, a, b *probeAccount, messageIDA, messageIDB int, pollID int64) error {
	if err := p.closePoll(ctx, a, p.chatPeer(), messageIDA, pollID, "group_poll_close"); err != nil {
		return err
	}
	stateA, err := p.getState(ctx, a, "poll_close_state")
	if err != nil {
		return err
	}
	stateB, err := p.getState(ctx, b, "poll_close_state")
	if err != nil {
		return err
	}
	if err := p.withAccount(ctx, a, "repeated_close_idempotent", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		response, err := api.MessagesEditMessage(ctx, closedPollRequest(p.chatPeer(), messageIDA))
		if err != nil {
			return rpcFailure("repeated_close_idempotent", err)
		}
		if len(updateList(response)) != 0 {
			return failure("repeated_close_idempotent", "SECOND_EDIT_UPDATE")
		}
		return nil
	}); err != nil {
		return err
	}
	stateAAfter, err := p.getState(ctx, a, "repeated_close_idempotent")
	if err != nil {
		return err
	}
	stateBAfter, err := p.getState(ctx, b, "repeated_close_idempotent")
	if err != nil {
		return err
	}
	if stateAAfter.Pts != stateA.Pts || stateBAfter.Pts != stateB.Pts {
		return failure("repeated_close_idempotent", "PTS_ADVANCED")
	}
	hasEdit, err := p.differenceHasPollEdit(ctx, b, stateB, pollID)
	if err != nil {
		return err
	}
	if hasEdit {
		return failure("repeated_close_idempotent", "SECOND_EDIT_UPDATE")
	}
	if err := p.pass("repeated_close_idempotent", "pts_delta_a=0", "pts_delta_b=0", "edit_updates=0"); err != nil {
		return err
	}
	var voteErr error
	if err := p.withAccount(ctx, b, "closed_poll_vote_denied", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		_, voteErr = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{Peer: p.chatPeer(), MsgID: messageIDB, Options: [][]byte{[]byte("first")}})
		return nil
	}); err != nil {
		return err
	}
	if denied := expectRPCError("closed_poll_vote_denied", "MESSAGE_POLL_CLOSED", 400, voteErr); denied != nil {
		return denied
	}
	if err := p.pass("closed_poll_vote_denied", "rpc_error=MESSAGE_POLL_CLOSED", "rpc_code=400"); err != nil {
		return err
	}
	if err := p.reconnectAccount(ctx, b, "closed_poll_reconnect"); err != nil {
		return err
	}
	closed, err := p.findPoll(ctx, b, p.chatPeer(), pollID, "closed_poll_reconnect")
	if err != nil {
		return err
	}
	if closed == nil {
		return failure("closed_poll_reconnect", "CLOSED_STATE_NOT_PERSISTED")
	}
	media, ok := closed.Media.(*tg.MessageMediaPoll)
	if !ok || !media.Poll.Closed {
		return failure("closed_poll_reconnect", "CLOSED_STATE_NOT_PERSISTED")
	}
	return p.pass("closed_poll_reconnect", "polls=1")
}

func (p *probe) closePoll(ctx context.Context, account *probeAccount, peer tg.InputPeerClass, messageID int, pollID int64, assertion string) error {
	return p.withAccount(ctx, account, assertion, func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.MessagesEditMessage(ctx, closedPollRequest(peer, messageID))
		if err != nil {
			return rpcFailure(assertion, err)
		}
		if !closedPollInUpdates(result, pollID) {
			return failure(assertion, "POLL_CLOSE_UPDATE_MISSING")
		}
		return nil
	})
}

func (p *probe) differenceHasPollEdit(ctx context.Context, account *probeAccount, baseline *tg.UpdatesState, pollID int64) (bool, error) {
	var found bool
	if err := p.withAccount(ctx, account, "repeated_close_idempotent", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
		result, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: baseline.Pts, Date: baseline.Date, Qts: baseline.Qts, PtsLimit: 100})
		if err != nil {
			return rpcFailure("repeated_close_idempotent", err)
		}
		var updates []tg.UpdateClass
		switch difference := result.(type) {
		case *tg.UpdatesDifference:
			updates = difference.OtherUpdates
		case *tg.UpdatesDifferenceSlice:
			updates = difference.OtherUpdates
		case *tg.UpdatesDifferenceEmpty:
			updates = nil
		default:
			return failure("repeated_close_idempotent", "DIFFERENCE_RESPONSE_INVALID")
		}
		found = hasPollEdit(updates, pollID)
		return nil
	}); err != nil {
		return false, err
	}
	return found, nil
}

func (p *probe) newClient(account *probeAccount) *telegram.Client {
	return telegram.NewClient(probeAppID, probeAppHash, telegram.Options{
		DC: probeDCID,
		DCList: dcs.List{Options: []tg.DCOption{{
			ID: probeDCID, IPAddress: p.endpoint.host, Port: p.endpoint.port,
		}}},
		PublicKeys:      []telegram.PublicKey{{RSA: p.publicKey}},
		Resolver:        dcs.Plain(dcs.PlainOptions{}),
		SessionStorage:  account.session,
		NoUpdates:       true,
		DialTimeout:     5 * time.Second,
		ExchangeTimeout: 10 * time.Second,
		RetryInterval:   probeDeadline + time.Minute,
		MaxRetries:      1,
	})
}

func (p *probe) reconnectAccount(ctx context.Context, account *probeAccount, assertion string) error {
	if account == nil || account.cancel == nil || account.done == nil {
		return failure(assertion, "ACCOUNT_SESSION_NOT_STARTED")
	}
	account.cancel()
	select {
	case <-account.done:
	case <-ctx.Done():
		return failure(assertion, "DEADLINE_EXCEEDED")
	}
	if err := p.startAccount(ctx, account, false); err != nil {
		return asProbeFailure(assertion, err)
	}
	return nil
}

func (p *probe) withAccount(ctx context.Context, account *probeAccount, assertion string, fn func(context.Context, *tg.Client, *telegram.Client) error) error {
	if account == nil || account.session == nil || !fixedUsername(account.username) {
		return failure(assertion, "ACCOUNT_OUTSIDE_ALLOWLIST")
	}
	if account.api != nil && account.client != nil && account.done != nil {
		select {
		case <-account.done:
		default:
			if err := fn(ctx, account.api, account.client); err != nil {
				return asProbeFailure(assertion, err)
			}
			return nil
		}
	}
	client := p.newClient(account)
	if err := client.Run(ctx, func(ctx context.Context) error { return fn(ctx, client.API(), client) }); err != nil {
		return asProbeFailure(assertion, err)
	}
	return nil
}

func (p *probe) account(username string) *probeAccount {
	for _, account := range p.accounts {
		if account != nil && account.username == username {
			return account
		}
	}
	return nil
}

func (p *probe) chatPeer() *tg.InputPeerChat {
	return &tg.InputPeerChat{ChatID: p.groupID}
}

func (p *probe) safePeer(username string, peer resolvedPeer) bool {
	account := p.account(username)
	return fixedUsername(username) && account != nil && account.userID == peer.userID && peer.accessHash != 0
}

func (p *probe) pass(assertion string, fields ...string) error {
	var outputFields []string
	for _, field := range fields {
		for _, part := range strings.Fields(field) {
			if !safeField(part) {
				return failure(assertion, "INVALID_OUTPUT_FIELD")
			}
			outputFields = append(outputFields, part)
		}
	}
	if _, err := fmt.Fprintf(p.output, "assertion=%s result=pass", safeAssertionName(assertion)); err != nil {
		return failure("probe_output", "LOCAL_OUTPUT_ERROR")
	}
	for _, field := range outputFields {
		if _, err := fmt.Fprintf(p.output, " %s", field); err != nil {
			return failure("probe_output", "LOCAL_OUTPUT_ERROR")
		}
	}
	if _, err := fmt.Fprintln(p.output); err != nil {
		return failure("probe_output", "LOCAL_OUTPUT_ERROR")
	}
	return nil
}

func (p *probe) logoutAll() error {
	attempted, failed := 0, 0
	errorCodes := make(map[string]bool)
	var firstFailure *probeFailure
	for _, account := range p.accounts {
		if account == nil || !account.authorized {
			continue
		}
		attempted++
		ctx, cancel := context.WithTimeout(context.Background(), logoutDeadline)
		var loggedOut bool
		err := p.withAccount(ctx, account, "logout_sessions", func(ctx context.Context, api *tg.Client, _ *telegram.Client) error {
			result, err := api.AuthLogOut(ctx)
			if err != nil {
				return rpcFailure("logout_sessions", err)
			}
			if result == nil {
				return failure("logout_sessions", "LOGOUT_RESPONSE_INVALID")
			}
			loggedOut = true
			return nil
		})
		cancel()
		if err != nil || !loggedOut {
			failed++
			problem := asProbeFailure("logout_sessions", err)
			if err == nil {
				problem = failure("logout_sessions", "LOGOUT_RESPONSE_INVALID")
			}
			if firstFailure == nil {
				firstFailure = problem
			}
			errorCodes[problem.errorCode] = true
			if problem.rpcError != "" {
				errorCodes[problem.rpcError] = true
			}
			continue
		}
		account.authorized = false
	}
	fields := []string{fmt.Sprintf("count=%d", attempted), fmt.Sprintf("failed=%d", failed)}
	if failed != 0 {
		codes := make([]string, 0, len(errorCodes))
		for code := range errorCodes {
			codes = append(codes, safeCode(code))
		}
		sort.Strings(codes)
		fields = append(fields, "error_codes="+strings.Join(codes, ","))
		if err := p.pass("logout_sessions", fields...); err != nil && firstFailure == nil {
			firstFailure = asProbeFailure("logout_sessions", err)
		}
		if firstFailure == nil {
			firstFailure = failure("logout_sessions", "LOGOUT_FAILED")
		}
		return firstFailure
	}
	return p.pass("logout_sessions", fields...)
}

func (p *probe) stopClients() error {
	for _, account := range p.accounts {
		if account == nil || account.cancel == nil || account.done == nil {
			continue
		}
		account.cancel()
		select {
		case <-account.done:
		case <-time.After(logoutDeadline):
			return failure("client_shutdown", "SHUTDOWN_TIMEOUT")
		}
	}
	return nil
}

func fixedUsername(value string) bool {
	for _, username := range probeUsernames {
		if value == username {
			return true
		}
	}
	return false
}

func rpcMatches(err error, name string, code int) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Message == name && rpcErr.Code == code
}

func safeField(field string) bool {
	for _, char := range field {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '=' || char == ',') {
			return false
		}
	}
	return true
}

func sameIDs(first, second, wantFirst, wantSecond int64) bool {
	return first == wantFirst && second == wantSecond || first == wantSecond && second == wantFirst
}

func getVoterID(vote tg.MessagePeerVoteClass) (int64, bool) {
	switch value := vote.(type) {
	case *tg.MessagePeerVote:
		peer, ok := value.Peer.(*tg.PeerUser)
		if ok && peer.UserID > 0 {
			return peer.UserID, true
		}
	case *tg.MessagePeerVoteMultiple:
		peer, ok := value.Peer.(*tg.PeerUser)
		if ok && peer.UserID > 0 {
			return peer.UserID, true
		}
	}
	return 0, false
}

func pollResultsFromUpdates(result tg.UpdatesClass, pollID int64) (*tg.PollResults, bool) {
	for _, update := range updateList(result) {
		if poll, ok := update.(*tg.UpdateMessagePoll); ok && poll.PollID == pollID {
			return &poll.Results, true
		}
	}
	return nil, false
}

func pollCountsMatch(result *tg.PollResults, total int, options map[string]int) bool {
	if result == nil || result.TotalVoters != total || len(result.Results) != len(options) {
		return false
	}
	seen := make(map[string]bool, len(options))
	for _, answer := range result.Results {
		option := string(answer.Option)
		want, ok := options[option]
		if !ok || seen[option] || answer.Voters != want {
			return false
		}
		seen[option] = true
	}
	return len(seen) == len(options)
}

func answerVoterCount(results *tg.PollResults, index int) int {
	if results == nil || index < 0 || index >= len(results.Results) {
		return -1
	}
	return results.Results[index].Voters
}

func hasVoterIdentity(result *tg.PollResults) bool {
	if result == nil || len(result.RecentVoters) != 0 {
		return result != nil
	}
	for _, answer := range result.Results {
		if len(answer.RecentVoters) != 0 {
			return true
		}
	}
	return false
}

func updateList(result tg.UpdatesClass) []tg.UpdateClass {
	switch value := result.(type) {
	case *tg.Updates:
		return value.Updates
	case *tg.UpdatesCombined:
		return value.Updates
	default:
		return nil
	}
}

func updateChats(result tg.UpdatesClass) []tg.ChatClass {
	switch value := result.(type) {
	case *tg.Updates:
		return value.Chats
	case *tg.UpdatesCombined:
		return value.Chats
	default:
		return nil
	}
}

func updateListFromHistory(result tg.MessagesMessagesClass) []tg.MessageClass {
	switch value := result.(type) {
	case *tg.MessagesMessages:
		return value.Messages
	case *tg.MessagesMessagesSlice:
		return value.Messages
	default:
		return nil
	}
}

func createdChatID(result tg.UpdatesClass) (int64, bool) {
	for _, candidate := range updateChats(result) {
		if chat, ok := candidate.(*tg.Chat); ok && chat.ID > 0 {
			return chat.ID, true
		}
	}
	return 0, false
}

func newPollMessage(result tg.UpdatesClass) (*tg.Message, bool) {
	for _, update := range updateList(result) {
		created, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := created.Message.(*tg.Message)
		if !ok {
			continue
		}
		if _, ok := message.Media.(*tg.MessageMediaPoll); ok {
			return message, true
		}
	}
	return nil, false
}

func closedPollRequest(peer tg.InputPeerClass, messageID int) *tg.MessagesEditMessageRequest {
	poll := tg.Poll{}
	poll.SetClosed(true)
	request := &tg.MessagesEditMessageRequest{Peer: peer, ID: messageID}
	request.SetMedia(&tg.InputMediaPoll{Poll: poll})
	return request
}

func closedPollInUpdates(result tg.UpdatesClass, pollID int64) bool {
	for _, update := range updateList(result) {
		edit, ok := update.(*tg.UpdateEditMessage)
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

func hasPollVote(updates []tg.UpdateClass, pollID int64) bool {
	for _, update := range updates {
		if vote, ok := update.(*tg.UpdateMessagePoll); ok && vote.PollID == pollID {
			return true
		}
	}
	return false
}

func hasPollEdit(updates []tg.UpdateClass, pollID int64) bool {
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

func messageList(history tg.MessagesMessagesClass) []tg.MessageClass {
	return updateListFromHistory(history)
}
