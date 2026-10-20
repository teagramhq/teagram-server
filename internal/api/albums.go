package api

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleSendMultiMediaAfterReplyOnConn serves messages.sendMultiMedia: 2 to
// store.MaxAlbumItems files the caller stored with uploadMedia, sent to one
// peer as one album in one transaction.
//
// Every reference resolves only through an upload grant the caller owns, and
// the store consumes those grants in the send transaction. Unknown ids, wrong
// hashes, other accounts' grants, expired or consumed grants, erased files,
// and files the caller received or sent and deleted all hold no grant for the
// caller and answer the same MEDIA_INVALID, so the reference check is not an
// oracle for anyone's files. The grouped_id is drawn here, never taken from the
// client.
func (h *handlers) handleSendMultiMediaAfterReplyOnConn(c *mtproto.Conn, r *mtproto.Request) (bin.Encoder, *replyUpdate, func(), error) {
	var req tg.MessagesSendMultiMediaRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, nil, errAuthKeyUnreg
	}
	if _, ok := req.GetScheduleDate(); ok {
		return nil, nil, nil, errInputRequestInvalid
	}
	if _, ok := req.GetQuickReplyShortcut(); ok {
		return nil, nil, nil, errInputRequestInvalid
	}
	if len(req.MultiMedia) > store.MaxAlbumItems {
		return nil, nil, nil, errMultiMediaTooLong
	}
	if len(req.MultiMedia) < 2 {
		return nil, nil, nil, errMediaInvalid
	}
	for _, single := range req.MultiMedia {
		if !validText(single.Message) {
			return nil, nil, nil, errMessageEmpty
		}
	}
	peerType, toID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, nil, nil, err
	}
	items, err := albumItems(req.MultiMedia)
	if err != nil {
		return nil, nil, nil, err
	}
	// A channel album carries photos only, exactly as a channel sendMedia does,
	// and with the same PEER_ID_INVALID a non-member gets.
	if peerType == store.PeerTypeChannel {
		for _, item := range items {
			if !item.Photo {
				return nil, nil, nil, errPeerIDInvalid
			}
		}
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, toID, r.UserID); err != nil {
			return nil, nil, nil, err
		}
	}
	var replyToMsgID int64
	if peerType == store.PeerTypeChannel {
		replyTo, hasReplyTo := req.GetReplyTo()
		if replyToMsgID, err = replyToMessageID(replyTo, hasReplyTo, peerType, toID, r.UserID); err != nil {
			return nil, nil, nil, err
		}
	}

	randomIDs := make([]int64, len(items))
	for i, item := range items {
		randomIDs[i] = item.RandomID
	}
	// Dedup runs before any charge and before any grant is touched; the send
	// transaction repeats it under its locks. A channel retry checks posting
	// rights under the channel state lock first, as a channel photo retry does.
	if peerType == store.PeerTypeChannel {
		replay, retryErr := h.store.ChannelAlbumRetryAs(r.Ctx, toID, r.UserID, randomIDs)
		if retryErr != nil {
			return nil, nil, nil, h.channelAlbumError(r, toID, retryErr)
		}
		if replay.Duplicate {
			return h.channelAlbumResponse(r, toID, replay)
		}
	} else {
		replay, retryErr := h.store.UserAlbumRetry(r.Ctx, r.UserID, peerType, toID, randomIDs)
		if errors.Is(retryErr, store.ErrRandomIDExpired) {
			return nil, nil, nil, errRandomIDDuplicate
		}
		if errors.Is(retryErr, store.ErrMediaInvalid) {
			return nil, nil, nil, errMediaInvalid
		}
		if retryErr != nil {
			h.log.Error("album random_id lookup", "user_id", r.UserID, "err", retryErr)
			return nil, nil, nil, errInternal
		}
		if replay.Duplicate {
			return h.userAlbumReplay(c, r, peerType, toID, replay)
		}
	}

	if err = h.checkRateLimitCost(r, "message_send", h.rateLimitMessageSend, len(items)); err != nil {
		return nil, nil, nil, err
	}
	groupedID, err := newGroupedID()
	if err != nil {
		h.log.Error("album grouped id", "err", err)
		return nil, nil, nil, errInternal
	}
	album := store.AlbumSend{FromID: r.UserID, Items: items, GroupedID: groupedID, GrantTTL: h.uploadGrantTTL}

	if peerType == store.PeerTypeChannel {
		result, postErr := h.store.PostChannelAlbumAs(r.Ctx, toID, replyToMsgID, album)
		if postErr != nil {
			return nil, nil, nil, h.channelAlbumError(r, toID, postErr)
		}
		if !result.Duplicate {
			h.notifyChannelPost(r.Ctx, toID)
		}
		return h.channelAlbumResponse(r, toID, result)
	}

	attempt := beginSenderRPC(c, r)
	var result store.AlbumResult
	if peerType == store.PeerTypeChat {
		result, err = h.store.SendChatAlbum(r.Ctx, toID, album)
	} else {
		result, err = h.store.SendUserAlbum(r.Ctx, toID, album)
	}
	if err != nil {
		h.clearSenderAfterStoreFailure(attempt, r, err)
		switch {
		case errors.Is(err, store.ErrRandomIDExpired):
			return nil, nil, nil, errRandomIDDuplicate
		case errors.Is(err, store.ErrMediaInvalid), errors.Is(err, store.ErrFileMissing):
			return nil, nil, nil, errMediaInvalid
		case errors.Is(err, store.ErrNotMember):
			return nil, nil, nil, errPeerIDInvalid
		case errors.Is(err, store.ErrChatWriteForbidden):
			return nil, nil, nil, errChatWriteForbidden
		case errors.Is(err, store.ErrYouBlockedUser):
			return nil, nil, nil, errYouBlockedUser
		}
		h.log.Error("send album", "user_id", r.UserID, "peer_type", peerType, "peer_id", toID, "err", err)
		return nil, nil, nil, errInternal
	}
	if result.Duplicate {
		// A concurrent send with the same tokens committed first.
		clearSenderRPC(attempt)
		return h.userAlbumReplay(c, r, peerType, toID, result)
	}
	senderPts := result.Pts[len(result.Pts)-1]
	setSenderRPCPts(attempt, senderPts)
	if h.afterSenderCommit != nil {
		h.afterSenderCommit()
	}
	notifyCommitted := func() {
		clearSenderRPC(attempt)
		h.notifyOwners(r.Ctx, result.PerOwner, 0)
	}
	if peerType == store.PeerTypeUser {
		// A withheld album leaves the recipient out of PerOwner.
		if _, delivered := result.PerOwner[toID]; delivered && toID != r.UserID {
			h.notify(r.Ctx, toID)
		}
		notifyCommitted = func() { h.clearSenderAndNotify(attempt, r) }
	}

	res, err := h.userAlbumUpdates(r, peerType, toID, result)
	if err != nil {
		notifyCommitted()
		return nil, nil, nil, err
	}
	update := &replyUpdate{
		owner:     r.UserID,
		authKey:   mtproto.AuthKeyIDInt64(r.AuthKeyID),
		pts:       senderPts,
		onFailure: notifyCommitted,
	}
	afterReply := func() {
		if peerType == store.PeerTypeChat {
			h.notifyOwners(r.Ctx, result.PerOwner, r.UserID)
		}
		h.notifySendAfterReply(r, senderPts)
	}
	return res, update, afterReply, nil
}

// albumItems reads the client's album entries. Only a stored-file reference
// with no extra options is accepted: uploaded media must go through
// uploadMedia first, and spoilers, timers, covers and live photos are not
// served. Every item needs its own non-zero random_id, and no file may appear
// twice.
func albumItems(multi []tg.InputSingleMedia) ([]store.AlbumItem, error) {
	items := make([]store.AlbumItem, 0, len(multi))
	seenFiles := make(map[int64]bool, len(multi))
	seenRandom := make(map[int64]bool, len(multi))
	for _, single := range multi {
		item := store.AlbumItem{RandomID: single.RandomID, Text: single.Message}
		switch media := single.Media.(type) {
		case *tg.InputMediaPhoto:
			photo, ok := media.ID.(*tg.InputPhoto)
			if !ok || photo == nil || media.Spoiler || media.LivePhoto || media.Video != nil {
				return nil, errMediaInvalid
			}
			if _, ok := media.GetTTLSeconds(); ok {
				return nil, errMediaInvalid
			}
			item.FileID, item.AccessHash, item.Photo = photo.ID, photo.AccessHash, true
		case *tg.InputMediaDocument:
			document, ok := media.ID.(*tg.InputDocument)
			if !ok || document == nil || media.Spoiler || media.Query != "" {
				return nil, errMediaInvalid
			}
			if _, ok := media.GetVideoCover(); ok {
				return nil, errMediaInvalid
			}
			if _, ok := media.GetVideoTimestamp(); ok {
				return nil, errMediaInvalid
			}
			if _, ok := media.GetTTLSeconds(); ok {
				return nil, errMediaInvalid
			}
			item.FileID, item.AccessHash = document.ID, document.AccessHash
		default:
			return nil, errMediaInvalid
		}
		if item.FileID == 0 || item.RandomID == 0 || seenFiles[item.FileID] || seenRandom[item.RandomID] {
			return nil, errMediaInvalid
		}
		seenFiles[item.FileID], seenRandom[item.RandomID] = true, true
		items = append(items, item)
	}
	return items, nil
}

// newGroupedID draws a non-zero album id from crypto/rand.
func newGroupedID() (int64, error) {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			return 0, fmt.Errorf("read random grouped id: %w", err)
		}
		if id := int64(binary.BigEndian.Uint64(b[:])); id != 0 { //nolint:gosec // G115: opaque 64-bit id, sign irrelevant
			return id, nil
		}
	}
}

func (h *handlers) channelAlbumError(r *mtproto.Request, channelID int64, err error) error {
	if slowModeWait, ok := errors.AsType[*store.SlowModeWaitError](err); ok {
		return rpcErr(420, slowModeWait.Error())
	}
	switch {
	case errors.Is(err, store.ErrNotMember):
		return errPeerIDInvalid
	case errors.Is(err, store.ErrChatWriteForbidden):
		return errChatWriteForbidden
	case errors.Is(err, store.ErrMessageInvalid):
		return errMessageIDInvalid
	case errors.Is(err, store.ErrMediaInvalid), errors.Is(err, store.ErrFileMissing), errors.Is(err, store.ErrRandomIDDuplicate):
		return errMediaInvalid
	}
	h.log.Error("send channel album", "user_id", r.UserID, "channel_id", channelID, "err", err)
	return errInternal
}

// userAlbumReplay answers a resend of a whole committed album with the stored
// rows at the pts they occupy, as a sendMedia retry does for one message.
func (h *handlers) userAlbumReplay(
	c *mtproto.Conn, r *mtproto.Request, peerType store.PeerType, toID int64, replay store.AlbumResult,
) (bin.Encoder, *replyUpdate, func(), error) {
	lastPts := 0
	for _, pts := range replay.Pts {
		lastPts = max(lastPts, pts)
	}
	attempt := senderRPCAttempt{}
	if peerType == store.PeerTypeUser {
		attempt = beginSenderRPCAt(c, r, lastPts)
	}
	res, err := h.userAlbumUpdates(r, peerType, toID, replay)
	if err != nil {
		if peerType == store.PeerTypeUser {
			h.clearSenderAndNotify(attempt, r)
		}
		return nil, nil, nil, err
	}
	if peerType == store.PeerTypeUser {
		setSenderRPCPts(attempt, lastPts)
	}
	update, afterReply := h.retryReplyAfterSuccess(attempt, r, peerType, lastPts)
	return res, update, afterReply, nil
}

func (h *handlers) userAlbumUpdates(r *mtproto.Request, peerType store.PeerType, toID int64, album store.AlbumResult) (*tg.Updates, error) {
	files, err := h.loadFiles(r.Ctx, album.Messages)
	if err != nil {
		h.log.Error("load album files", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	res := &tg.Updates{Date: int(album.Messages[0].Date.Unix())}
	for i, message := range album.Messages {
		res.Updates = append(res.Updates, &tg.UpdateMessageID{ID: int(message.LocalID), RandomID: message.RandomID})
		res.Updates = append(res.Updates, &tg.UpdateNewMessage{
			Message: messageToTL(message, nil, files, nil, nil), Pts: album.Pts[i], PtsCount: 1,
		})
	}
	if peerType == store.PeerTypeChat {
		if res.Chats, err = h.loadChats(r.Ctx, map[int64]bool{toID: true}, r.UserID, nil); err != nil {
			h.log.Error("load album chat", "chat_id", toID, "err", err)
			return nil, errInternal
		}
		recipients := make(map[int64]bool, len(album.PerOwner)+1)
		recipients[r.UserID] = true
		for uid := range album.PerOwner {
			recipients[uid] = true
		}
		if res.Users, err = h.loadUsers(r.Ctx, recipients, r.UserID); err != nil {
			h.log.Error("load album users", "chat_id", toID, "err", err)
			return nil, errInternal
		}
		return res, nil
	}
	if res.Users, err = h.twoUsers(r.Ctx, r.UserID, toID); err != nil {
		h.log.Error("load album users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return res, nil
}

// channelAlbumResponse builds the poster-side Updates for a channel album or
// its replay, hydrated from the stored posts.
func (h *handlers) channelAlbumResponse(
	r *mtproto.Request, channelID int64, album store.ChannelAlbumResult,
) (bin.Encoder, *replyUpdate, func(), error) {
	channels, err := h.loadChannels(r.Ctx, map[int64]bool{channelID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel album channel", "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel album sender", "user_id", r.UserID, "err", err)
		return nil, nil, nil, errInternal
	}
	files, err := h.loadChannelFiles(r.Ctx, album.Messages)
	if err != nil {
		h.log.Error("load channel album files", "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	res := &tg.Updates{Chats: channels, Users: users, Date: int(album.Messages[0].Date.Unix())}
	for i, message := range album.Messages {
		messageTL, renderErr := channelMessageToTL(message, r.UserID, files)
		if renderErr != nil {
			h.log.Error("render channel album message", "channel_id", channelID, "local_id", message.LocalID, "err", renderErr)
			return nil, nil, nil, errInternal
		}
		res.Updates = append(res.Updates,
			&tg.UpdateMessageID{ID: int(message.LocalID), RandomID: message.RandomID},
			&tg.UpdateNewChannelMessage{Message: messageTL, Pts: album.Pts[i], PtsCount: 1},
		)
	}
	return res, nil, nil, nil
}
