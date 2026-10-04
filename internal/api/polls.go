package api

import (
	"context"
	"errors"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func (h *handlers) handleSendPollAfterReplyOnConn(
	c *mtproto.Conn,
	r *mtproto.Request,
	req *tg.MessagesSendMediaRequest,
	media *tg.InputMediaPoll,
	peerType store.PeerType,
	peerID int64,
) (bin.Encoder, *replyUpdate, func(), error) {
	switch peerType {
	case store.PeerTypeUser:
		if peerID != r.UserID {
			return nil, nil, nil, errPeerIDInvalid
		}
	case store.PeerTypeChat:
		if err := h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, nil, nil, err
		}
	case store.PeerTypeChannel:
		// Admission is checked and locked with the channel write transaction.
	default:
		return nil, nil, nil, errPeerIDInvalid
	}
	if req.Message != "" || len(req.Entities) != 0 {
		return nil, nil, nil, errMediaInvalid
	}
	draft, err := pollDraftFromInput(media)
	if err != nil {
		return nil, nil, nil, errPollInvalid
	}
	if err = h.store.ValidatePollDraftShape(draft); err != nil {
		return nil, nil, nil, errPollInvalid
	}

	if peerType != store.PeerTypeChannel && req.RandomID != 0 {
		existing, ok, lookupErr := h.store.MessageByRandomID(r.Ctx, r.UserID, req.RandomID)
		if lookupErr != nil {
			h.log.Error("poll random id lookup", "user_id", r.UserID, "err", lookupErr)
			return nil, nil, nil, errInternal
		}
		if ok {
			if existing.Deleted || !existing.Out || existing.FromID != r.UserID || existing.PeerType != peerType || existing.PeerID != peerID {
				return nil, nil, nil, errMediaInvalid
			}
			poll, pollErr := h.store.PollForMessage(r.Ctx, r.UserID, store.PollMessageRef{
				PeerType: peerType, PeerID: peerID, LocalID: existing.LocalID,
			})
			if errors.Is(pollErr, store.ErrMessageInvalid) {
				return nil, nil, nil, errMediaInvalid
			}
			if pollErr != nil {
				h.log.Error("load poll retry", "user_id", r.UserID, "local_id", existing.LocalID, "err", pollErr)
				return nil, nil, nil, errInternal
			}
			pts, ptsErr := h.store.MessagePts(r.Ctx, r.UserID, existing.LocalID)
			if ptsErr != nil {
				h.log.Error("read poll retry pts", "user_id", r.UserID, "local_id", existing.LocalID, "err", ptsErr)
				return nil, nil, nil, errInternal
			}
			return h.pollSendResponse(c, r, existing, poll, pts, req.RandomID, nil)
		}
	}
	if err = h.store.ValidatePollDraft(draft); err != nil {
		return nil, nil, nil, errPollInvalid
	}
	if err = h.checkRateLimit(r, "message_send", h.rateLimitMessageSend); err != nil {
		return nil, nil, nil, err
	}

	if peerType == store.PeerTypeChat {
		return h.sendChatPoll(r, req, peerID, draft)
	}
	if peerType == store.PeerTypeChannel {
		return h.sendChannelPoll(r, req, peerID, draft)
	}
	return h.sendSavedPoll(c, r, req, draft)
}

func (h *handlers) sendChannelPoll(
	r *mtproto.Request,
	req *tg.MessagesSendMediaRequest,
	channelID int64,
	draft store.PollDraft,
) (bin.Encoder, *replyUpdate, func(), error) {
	message, poll, pts, duplicate, err := h.store.PostChannelPollAs(r.Ctx, channelID, r.UserID, req.RandomID, draft)
	switch {
	case errors.Is(err, store.ErrNotMember):
		return nil, nil, nil, errPeerIDInvalid
	case errors.Is(err, store.ErrChatWriteForbidden):
		return nil, nil, nil, errChatWriteForbidden
	case errors.Is(err, store.ErrPollInvalid):
		return nil, nil, nil, errPollInvalid
	case errors.Is(err, store.ErrMessageInvalid):
		return nil, nil, nil, errMediaInvalid
	case err != nil:
		h.log.Error("send channel poll", "user_id", r.UserID, "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	if !duplicate {
		h.notifyChannelPost(r.Ctx, channelID)
	}
	message.Poll = &poll
	channels, err := h.loadChannels(r.Ctx, map[int64]bool{channelID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel poll channel", "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel poll sender", "user_id", r.UserID, "err", err)
		return nil, nil, nil, errInternal
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: int(message.LocalID), RandomID: req.RandomID},
			&tg.UpdateNewChannelMessage{Message: channelMessageToTL(message, r.UserID, nil), Pts: pts, PtsCount: 1},
		},
		Chats: channels,
		Users: users,
		Date:  int(message.Date.Unix()),
	}, nil, nil, nil
}

func (h *handlers) handleSendVote(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesSendVoteRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errInputRequestInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil || peerType == store.PeerTypeUser && peerID != r.UserID {
		return nil, errPeerIDInvalid
	}
	if req.MsgID <= 0 {
		return nil, errMessageIDInvalid
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	if err = h.checkRateLimit(r, "poll_vote", h.rateLimitPollVote); err != nil {
		return nil, err
	}
	ref := store.PollMessageRef{PeerType: peerType, PeerID: peerID, LocalID: int64(req.MsgID)}
	poll, changed, err := h.store.CastPollVoteWithChange(r.Ctx, r.UserID, ref, req.Options)
	if err != nil {
		return nil, pollStoreError(err)
	}
	if changed {
		h.notifyPollVote(r.Ctx, peerType, peerID, poll.ID)
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateMessagePoll{PollID: poll.ID, Results: pollResultsToTL(poll)}},
		Date:    int(h.now().Unix()),
	}, nil
}

func (h *handlers) handleGetPollResults(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPollResultsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errInputRequestInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil || peerType == store.PeerTypeUser && peerID != r.UserID {
		return nil, errPeerIDInvalid
	}
	if req.MsgID <= 0 {
		return nil, errMessageIDInvalid
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	poll, err := h.store.PollForMessage(r.Ctx, r.UserID, store.PollMessageRef{
		PeerType: peerType, PeerID: peerID, LocalID: int64(req.MsgID),
	})
	if err != nil {
		return nil, pollStoreError(err)
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateMessagePoll{PollID: poll.ID, Results: pollResultsToTL(poll)}},
		Date:    int(h.now().Unix()),
	}, nil
}

func (h *handlers) handleGetPollVotes(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPollVotesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errInputRequestInvalid
	}
	if uint32(req.Flags)&^uint32(3) != 0 {
		return nil, errPollInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil || peerType == store.PeerTypeUser && peerID != r.UserID {
		return nil, errPeerIDInvalid
	}
	if req.ID <= 0 {
		return nil, errMessageIDInvalid
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	var option []byte
	if req.Flags.Has(0) {
		if len(req.Option) == 0 {
			return nil, errPollInvalid
		}
		option = req.Option
	}
	offset := ""
	if req.Flags.Has(1) {
		offset = req.Offset
	}
	page, err := h.store.PollVoters(r.Ctx, r.UserID, store.PollMessageRef{
		PeerType: peerType, PeerID: peerID, LocalID: int64(req.ID),
	}, option, offset, req.Limit)
	if err != nil {
		return nil, pollStoreError(err)
	}
	ids := make(map[int64]bool, len(page.Voters))
	for _, voter := range page.Voters {
		ids[voter.UserID] = true
	}
	users, err := h.loadUsers(r.Ctx, ids, r.UserID)
	if err != nil {
		h.log.Error("load poll voter users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	votes := make([]tg.MessagePeerVoteClass, len(page.Voters))
	for i, voter := range page.Voters {
		peer := peerToTL(store.PeerTypeUser, voter.UserID)
		date := int(voter.FirstVotedAt.Unix())
		switch len(voter.Options) {
		case 0:
			return nil, errInternal
		case 1:
			votes[i] = &tg.MessagePeerVote{Peer: peer, Option: voter.Options[0], Date: date}
		default:
			votes[i] = &tg.MessagePeerVoteMultiple{Peer: peer, Options: voter.Options, Date: date}
		}
	}
	result := &tg.MessagesVotesList{Count: page.Count, Votes: votes, Users: users}
	if page.NextOffset != "" {
		result.SetNextOffset(page.NextOffset)
	}
	return result, nil
}

func (h *handlers) handleClosePollAfterReplyOnConn(
	c *mtproto.Conn,
	r *mtproto.Request,
	peerType store.PeerType,
	peerID int64,
	localID int64,
) (bin.Encoder, *replyUpdate, func(), error) {
	ref := store.PollMessageRef{PeerType: peerType, PeerID: peerID, LocalID: localID}
	changed, ownerPts, err := h.store.ClosePollWithUpdates(r.Ctx, r.UserID, ref)
	if errors.Is(err, store.ErrPollDenied) {
		return nil, nil, nil, errMessageIDInvalid
	}
	if err != nil {
		return nil, nil, nil, pollStoreError(err)
	}
	if !changed {
		return &tg.Updates{Date: int(h.now().Unix())}, nil, nil, nil
	}
	if peerType == store.PeerTypeChat {
		h.notifyOwners(r.Ctx, ownerPts, 0)
	}
	if peerType == store.PeerTypeChannel {
		pts := ownerPts[r.UserID]
		if pts <= 0 {
			return nil, nil, nil, errInternal
		}
		messages, loadErr := h.store.ChannelMessages(r.Ctx, peerID, []int64{localID})
		message, ok := messages[localID]
		if loadErr != nil || !ok {
			h.log.Error("load closed channel poll", "channel_id", peerID, "local_id", localID, "err", loadErr)
			return nil, nil, nil, errInternal
		}
		channelMessages := []store.ChannelMessage{message}
		if loadErr = h.attachChannelPollViews(r.Ctx, r.UserID, peerID, channelMessages); loadErr != nil {
			h.log.Error("render closed channel poll", "channel_id", peerID, "local_id", localID, "err", loadErr)
			return nil, nil, nil, errInternal
		}
		users, userErr := h.loadUsers(r.Ctx, map[int64]bool{message.FromID: true}, r.UserID)
		if userErr != nil {
			h.log.Error("load closed channel poll author", "channel_id", peerID, "err", userErr)
			return nil, nil, nil, errInternal
		}
		channels, channelErr := h.loadChannels(r.Ctx, map[int64]bool{peerID: true}, r.UserID)
		if channelErr != nil {
			h.log.Error("load closed channel poll channel", "channel_id", peerID, "err", channelErr)
			return nil, nil, nil, errInternal
		}
		h.notifyChannelPost(r.Ctx, peerID)
		return &tg.Updates{
			Updates: []tg.UpdateClass{&tg.UpdateEditChannelMessage{
				Message: channelMessageToTL(channelMessages[0], r.UserID, nil),
				Pts:     pts, PtsCount: 1,
			}},
			Users: users,
			Chats: channels,
			Date:  int(h.now().Unix()),
		}, nil, nil, nil
	}
	pts := ownerPts[r.UserID]
	if pts <= 0 {
		return nil, nil, nil, errInternal
	}
	var attempt senderRPCAttempt
	if peerType == store.PeerTypeUser {
		attempt = beginSenderRPC(c, r)
		setSenderRPCPts(attempt, pts)
		if h.afterSenderCommit != nil {
			h.afterSenderCommit()
		}
	}
	message, ok, err := h.store.MessageByOwnerLocal(r.Ctx, r.UserID, localID)
	if err != nil || !ok {
		if peerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, errInternal
	}
	poll, err := h.store.PollForMessage(r.Ctx, r.UserID, ref)
	if err != nil {
		if peerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, pollStoreError(err)
	}
	files, err := h.loadFiles(r.Ctx, []store.Message{message})
	if err != nil {
		if peerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, errInternal
	}
	usersByID := map[int64]bool{r.UserID: true}
	var chats []tg.ChatClass
	if peerType == store.PeerTypeChat {
		usersByID = make(map[int64]bool, len(ownerPts))
		for ownerID := range ownerPts {
			usersByID[ownerID] = true
		}
		chats, err = h.loadChats(r.Ctx, map[int64]bool{peerID: true}, r.UserID, nil)
		if err != nil {
			return nil, nil, nil, errInternal
		}
	}
	users, err := h.loadUsers(r.Ctx, usersByID, r.UserID)
	if err != nil {
		if peerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, errInternal
	}
	result := &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateEditMessage{
			Message: messageToTLWithPoll(message, nil, files, nil, nil, poll),
			Pts:     pts, PtsCount: 1,
		}},
		Users: users,
		Chats: chats,
		Date:  int(h.now().Unix()),
	}
	if peerType != store.PeerTypeUser {
		return result, nil, nil, nil
	}
	update := &replyUpdate{
		owner:   r.UserID,
		authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID),
		pts:     pts,
		onFailure: func() {
			h.clearSenderAndNotify(attempt, r)
		},
	}
	afterReply := func() {
		h.notifySendAfterReply(r, pts)
	}
	return result, update, afterReply, nil
}

func (h *handlers) notifyPollVote(ctx context.Context, peerType store.PeerType, peerID, pollID int64) {
	var recipients []int64
	switch peerType {
	case store.PeerTypeChat:
		var err error
		recipients, err = h.store.ChatMemberIDs(ctx, peerID)
		if err != nil {
			h.log.Error("list poll vote recipients", "chat_id", peerID, "poll_id", pollID, "err", err)
			return
		}
	case store.PeerTypeChannel:
		var err error
		recipients, err = h.store.ChannelPollRecipientIDs(ctx, peerID)
		if err != nil {
			h.log.Error("list channel poll vote recipients", "channel_id", peerID, "poll_id", pollID, "err", err)
			return
		}
	default:
		recipients = []int64{peerID}
	}
	notifyCtx, cancel := senderNotifyContext(ctx)
	defer cancel()
	for _, userID := range recipients {
		if err := h.store.Notify(notifyCtx, store.ChannelUpdates, store.PollVotePayload(userID, pollID)); err != nil {
			h.log.Error("notify poll vote", "user_id", userID, "poll_id", pollID, "err", err)
		}
	}
}

func (h *handlers) sendChatPoll(
	r *mtproto.Request,
	req *tg.MessagesSendMediaRequest,
	chatID int64,
	draft store.PollDraft,
) (bin.Encoder, *replyUpdate, func(), error) {
	sender, perOwner, poll, duplicate, err := h.store.SendChatPollMessage(r.Ctx, store.FanOut{
		ChatID: chatID, FromID: r.UserID, RandomID: req.RandomID, MediaRights: []string{"send_polls"},
	}, draft)
	if errors.Is(err, store.ErrNotMember) {
		return nil, nil, nil, errPeerIDInvalid
	}
	if errors.Is(err, store.ErrChatWriteForbidden) {
		return nil, nil, nil, errChatWriteForbidden
	}
	if errors.Is(err, store.ErrMessageInvalid) {
		return nil, nil, nil, errMediaInvalid
	}
	if err != nil {
		h.log.Error("send chat poll message", "user_id", r.UserID, "chat_id", chatID, "err", err)
		return nil, nil, nil, errInternal
	}
	if !duplicate {
		h.notifyOwners(r.Ctx, perOwner, 0)
	}
	return h.pollSendResponse(nil, r, sender, poll, perOwner[r.UserID], req.RandomID, perOwner)
}

func (h *handlers) sendSavedPoll(
	c *mtproto.Conn,
	r *mtproto.Request,
	req *tg.MessagesSendMediaRequest,
	draft store.PollDraft,
) (bin.Encoder, *replyUpdate, func(), error) {
	attempt := beginSenderRPC(c, r)
	sender, pts, poll, _, err := h.store.SendSavedPollMessage(r.Ctx, r.UserID, req.RandomID, draft)
	if err != nil {
		h.clearSenderAndNotify(attempt, r)
		h.log.Error("send Saved Messages poll", "user_id", r.UserID, "err", err)
		if errors.Is(err, store.ErrMessageInvalid) {
			return nil, nil, nil, errMediaInvalid
		}
		return nil, nil, nil, pollStoreError(err)
	}
	setSenderRPCPts(attempt, pts)
	return h.pollSendResponseWithAttempt(c, r, sender, poll, pts, req.RandomID, attempt)
}

func (h *handlers) pollSendResponse(
	c *mtproto.Conn,
	r *mtproto.Request,
	message store.Message,
	poll store.Poll,
	pts int,
	randomID int64,
	perOwner map[int64]int,
) (bin.Encoder, *replyUpdate, func(), error) {
	var attempt senderRPCAttempt
	if message.PeerType == store.PeerTypeUser {
		attempt = beginSenderRPC(c, r)
		setSenderRPCPts(attempt, pts)
	}
	return h.pollSendResponseWithAttempt(c, r, message, poll, pts, randomID, attempt, perOwner)
}

func (h *handlers) pollSendResponseWithAttempt(
	c *mtproto.Conn,
	r *mtproto.Request,
	message store.Message,
	poll store.Poll,
	pts int,
	randomID int64,
	attempt senderRPCAttempt,
	perOwnerSets ...map[int64]int,
) (bin.Encoder, *replyUpdate, func(), error) {
	usersByID := map[int64]bool{r.UserID: true}
	var chats []tg.ChatClass
	if message.PeerType == store.PeerTypeChat {
		if len(perOwnerSets) > 0 && perOwnerSets[0] != nil {
			usersByID = make(map[int64]bool, len(perOwnerSets[0]))
			for ownerID := range perOwnerSets[0] {
				usersByID[ownerID] = true
			}
		}
		var err error
		chats, err = h.loadChats(r.Ctx, map[int64]bool{message.PeerID: true}, r.UserID, nil)
		if err != nil {
			h.log.Error("load poll chat", "chat_id", message.PeerID, "err", err)
			return nil, nil, nil, errInternal
		}
	}
	users, err := h.loadUsers(r.Ctx, usersByID, r.UserID)
	if err != nil {
		h.log.Error("load poll users", "user_id", r.UserID, "err", err)
		if message.PeerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, errInternal
	}
	msg := messageToTLWithPoll(message, nil, nil, nil, nil, poll)
	result := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: int(message.LocalID), RandomID: randomID},
			&tg.UpdateNewMessage{Message: msg, Pts: pts, PtsCount: 1},
		},
		Users: users,
		Chats: chats,
		Date:  int(message.Date.Unix()),
	}
	if message.PeerType != store.PeerTypeUser {
		return result, nil, nil, nil
	}
	update := &replyUpdate{
		owner:   r.UserID,
		authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID),
		pts:     pts,
		onFailure: func() {
			h.clearSenderAndNotify(attempt, r)
		},
	}
	afterReply := func() {
		h.notifySendAfterReply(r, pts)
	}
	return result, update, afterReply, nil
}

func pollDraftFromInput(media *tg.InputMediaPoll) (store.PollDraft, error) {
	if media == nil {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	inputFlags := uint32(media.Flags)
	if inputFlags&^uint32(3) != 0 { // Correct answers and plain solution are the only optional input fields.
		return store.PollDraft{}, store.ErrPollInvalid
	}
	poll := media.Poll
	const supportedPollFlags = (1 << 1) | (1 << 2) | (1 << 3) | (1 << 4) | (1 << 5) | (1 << 6) | (1 << 7) | (1 << 8)
	if uint32(poll.Flags)&^uint32(supportedPollFlags) != 0 || poll.HideResultsUntilClose || poll.SubscribersOnly || len(poll.CountriesISO2) != 0 {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	if len(poll.Question.Entities) != 0 || !validText(poll.Question.Text) {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	draft := store.PollDraft{
		Question:         []byte(poll.Question.Text),
		PublicVoters:     poll.PublicVoters,
		MultipleChoice:   poll.MultipleChoice,
		Quiz:             poll.Quiz,
		OpenAnswers:      poll.OpenAnswers,
		ShuffleAnswers:   poll.ShuffleAnswers,
		RevotingDisabled: poll.RevotingDisabled,
		Solution:         []byte(media.Solution),
	}
	if poll.Closed {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	if (poll.Flags.Has(4) && poll.Flags.Has(5)) || (poll.ClosePeriod != 0 && poll.CloseDate != 0) {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	if poll.Flags.Has(4) {
		if poll.ClosePeriod < 5 || poll.ClosePeriod > 10*60 {
			return store.PollDraft{}, store.ErrPollInvalid
		}
		draft.ClosePeriod = poll.ClosePeriod
	}
	if poll.Flags.Has(5) {
		if poll.CloseDate <= 0 {
			return store.PollDraft{}, store.ErrPollInvalid
		}
		date := time.Unix(int64(poll.CloseDate), 0).UTC()
		draft.CloseDate = &date
	}
	if len(media.SolutionEntities) != 0 {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	if len(draft.Solution) != 0 && !poll.Quiz {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	if !validText(media.Solution) && media.Solution != "" {
		return store.PollDraft{}, store.ErrPollInvalid
	}

	correctAnswersPresent := media.Flags.Has(0)
	if correctAnswersPresent != poll.Quiz {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	correct := make(map[int]bool, len(media.CorrectAnswers))
	for _, index := range media.CorrectAnswers {
		if index < 0 || index >= len(poll.Answers) || correct[index] {
			return store.PollDraft{}, store.ErrPollInvalid
		}
		correct[index] = true
	}
	if poll.Quiz && len(correct) == 0 {
		return store.PollDraft{}, store.ErrPollInvalid
	}
	draft.Answers = make([]store.PollAnswer, len(poll.Answers))
	for i, answerClass := range poll.Answers {
		answer, ok := answerClass.(*tg.PollAnswer)
		if !ok || answer.Flags != 0 || answer.Media != nil || answer.AddedBy != nil || answer.Date != 0 || len(answer.Text.Entities) != 0 {
			return store.PollDraft{}, store.ErrPollInvalid
		}
		draft.Answers[i] = store.PollAnswer{
			Option:  append([]byte(nil), answer.Option...),
			Text:    []byte(answer.Text.Text),
			Correct: correct[i],
		}
	}
	return draft, nil
}

func messageToTLWithPoll(
	message store.Message,
	createUsers []int64,
	files map[int64]*tg.Document,
	replyTexts map[int32]string,
	reactions []store.Reaction,
	poll store.Poll,
) tg.MessageClass {
	result := messageToTL(message, createUsers, files, replyTexts, reactions)
	if msg, ok := result.(*tg.Message); ok {
		msg.SetMedia(&tg.MessageMediaPoll{
			Poll:    pollToTL(poll),
			Results: pollResultsToTL(poll),
		})
	}
	return result
}

func (h *handlers) pollViewsForMessages(ctx context.Context, viewerID int64, messages []store.Message) (map[int64]store.Poll, error) {
	views := make(map[int64]store.Poll)
	if len(messages) == 0 {
		return views, nil
	}
	localIDs := make([]int64, len(messages))
	for i, message := range messages {
		localIDs[i] = message.LocalID
	}
	pollLocalIDs, err := h.store.PollMessageCopiesByOwnerLocalIDs(ctx, viewerID, localIDs)
	if err != nil {
		return nil, err
	}
	pollLocalIDSet := make(map[int64]struct{}, len(pollLocalIDs))
	for _, localID := range pollLocalIDs {
		pollLocalIDSet[localID] = struct{}{}
	}
	for _, message := range messages {
		if _, ok := pollLocalIDSet[message.LocalID]; !ok {
			continue
		}
		delete(pollLocalIDSet, message.LocalID)
		poll, err := h.store.PollForMessage(ctx, viewerID, store.PollMessageRef{
			PeerType: message.PeerType,
			PeerID:   message.PeerID,
			LocalID:  message.LocalID,
		})
		if errors.Is(err, store.ErrMessageInvalid) {
			continue
		}
		if errors.Is(err, store.ErrNotMember) {
			continue
		}
		if err != nil {
			return nil, err
		}
		views[message.LocalID] = poll
	}
	return views, nil
}

func (h *handlers) attachChannelPollViews(ctx context.Context, viewerID, channelID int64, messages []store.ChannelMessage) error {
	if len(messages) == 0 {
		return nil
	}
	localIDs := make([]int64, len(messages))
	for i, message := range messages {
		localIDs[i] = message.LocalID
	}
	pollIDs, err := h.store.ChannelPollMessageLocalIDs(ctx, channelID, localIDs)
	if err != nil {
		return err
	}
	for _, localID := range pollIDs {
		poll, err := h.store.PollForMessage(ctx, viewerID, store.PollMessageRef{
			PeerType: store.PeerTypeChannel,
			PeerID:   channelID,
			LocalID:  localID,
		})
		if errors.Is(err, store.ErrNotMember) || errors.Is(err, store.ErrMessageInvalid) {
			continue
		}
		if err != nil {
			return err
		}
		for i := range messages {
			if messages[i].LocalID == localID {
				messages[i].Poll = &poll
				break
			}
		}
	}
	return nil
}

func (h *handlers) attachChannelPollViewsAcross(ctx context.Context, viewerID int64, messages []store.ChannelMessage) error {
	indices := make(map[int64][]int, len(messages))
	for i, message := range messages {
		indices[message.ChannelID] = append(indices[message.ChannelID], i)
	}
	for channelID, positions := range indices {
		group := make([]store.ChannelMessage, len(positions))
		for i, position := range positions {
			group[i] = messages[position]
		}
		if err := h.attachChannelPollViews(ctx, viewerID, channelID, group); err != nil {
			return err
		}
		for i, position := range positions {
			messages[position] = group[i]
		}
	}
	return nil
}

func pollToTL(poll store.Poll) tg.Poll {
	result := tg.Poll{
		ID:       poll.ID,
		Question: tg.TextWithEntities{Text: string(poll.Question)},
		Creator:  poll.Creator,
		Answers:  make([]tg.PollAnswerClass, len(poll.Answers)),
	}
	result.SetClosed(poll.Closed)
	result.SetPublicVoters(poll.PublicVoters)
	result.SetMultipleChoice(poll.MultipleChoice)
	result.SetQuiz(poll.Quiz)
	result.SetRevotingDisabled(poll.RevotingDisabled)
	result.SetShuffleAnswers(poll.ShuffleAnswers)
	if poll.CloseDate != nil {
		result.SetCloseDate(int(poll.CloseDate.Unix()))
	}
	for i, answer := range poll.Answers {
		result.Answers[i] = &tg.PollAnswer{
			Text:   tg.TextWithEntities{Text: string(answer.Text)},
			Option: append([]byte(nil), answer.Option...),
		}
	}
	return result
}

func pollResultsToTL(poll store.Poll) tg.PollResults {
	answers := make([]tg.PollAnswerVoters, len(poll.Answers))
	for i, answer := range poll.Answers {
		answers[i] = tg.PollAnswerVoters{
			Option:  append([]byte(nil), answer.Option...),
			Voters:  int(answer.VoterCount),
			Chosen:  answer.Chosen,
			Correct: answer.Correct,
		}
	}
	results := tg.PollResults{}
	results.SetResults(answers)
	results.SetTotalVoters(int(poll.VoterCount))
	if len(poll.Solution) != 0 {
		results.SetSolution(string(poll.Solution))
	}
	return results
}

func pollStoreError(err error) error {
	switch {
	case errors.Is(err, store.ErrPollInvalid):
		return errPollInvalid
	case errors.Is(err, store.ErrPollClosed):
		return errPollClosed
	case errors.Is(err, store.ErrPollVoteNotAllowed):
		return errPollRevote
	case errors.Is(err, store.ErrPollVoteRequired), errors.Is(err, store.ErrPollDenied):
		return errPollVoteRequired
	case errors.Is(err, store.ErrNotMember):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrChatWriteForbidden):
		return errChatWriteForbidden
	case errors.Is(err, store.ErrMessageInvalid):
		return errMessageIDInvalid
	default:
		return errInternal
	}
}
