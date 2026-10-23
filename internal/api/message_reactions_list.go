package api

import (
	"encoding/base64"
	"errors"
	"strconv"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const maxMessageReactionListLimit = 100

func (h *handlers) handleGetMessageReactionsList(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetMessageReactionsListRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.Limit < 1 || req.Limit > maxMessageReactionListLimit {
		return nil, errLimitInvalid
	}
	reaction, err := reactionListFilter(req)
	if err != nil {
		return nil, err
	}
	offset, err := parseMessageReactionListOffset(req.Offset)
	if err != nil {
		return nil, errOffsetInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}

	var page store.MessageReactionListPage
	switch peerType {
	case store.PeerTypeChat:
		if err := h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
		page, err = h.store.MessageReactionListForChat(r.Ctx, r.UserID, peerID, int64(req.ID), reaction, offset, req.Limit)
	case store.PeerTypeChannel:
		if _, err := h.requireChannelMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
		broadcast, kindErr := h.channelIsBroadcast(r.Ctx, peerID)
		if kindErr != nil {
			h.log.Error("load channel kind for reaction list", "user_id", r.UserID, "channel_id", peerID, "err", kindErr)
			return nil, errInternal
		}
		if broadcast {
			return nil, errBroadcastForbidden
		}
		page, err = h.store.ChannelMessageReactionListForMember(r.Ctx, r.UserID, peerID, int64(req.ID), reaction, offset, req.Limit)
	default:
		return nil, errPeerIDInvalid
	}
	if errors.Is(err, store.ErrMessageInvalid) {
		return nil, errMsgIDInvalid
	}
	if err != nil {
		h.log.Error("get message reaction list", "user_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "message_id", req.ID, "err", err)
		return nil, errInternal
	}

	usersByID := make(map[int64]bool, len(page.Items))
	reactions := make([]tg.MessagePeerReaction, len(page.Items))
	for i, item := range page.Items {
		usersByID[item.ReactorID] = true
		reactions[i] = tg.MessagePeerReaction{
			PeerID:   &tg.PeerUser{UserID: item.ReactorID},
			Date:     int(item.Date.Unix()),
			Reaction: &tg.ReactionEmoji{Emoticon: item.Reaction},
		}
	}
	users, err := h.loadUsersWithAuthorizedIDs(r.Ctx, usersByID, r.UserID, usersByID)
	if err != nil {
		h.log.Error("load reaction-list users", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	result := &tg.MessagesMessageReactionsList{
		Count:     page.Count,
		Reactions: reactions,
		Chats:     []tg.ChatClass{},
		Users:     users,
	}
	if page.HasMore {
		if len(page.Items) == 0 {
			return nil, errors.New("reaction-list page has more entries but contains no cursor item")
		}
		result.SetNextOffset(formatMessageReactionListOffset(page.Items[len(page.Items)-1].ReactorID))
	}
	return result, nil
}

func reactionListFilter(req tg.MessagesGetMessageReactionsListRequest) (string, error) {
	reaction, ok := req.GetReaction()
	if !ok {
		return "", nil
	}
	emoji, ok := reaction.(*tg.ReactionEmoji)
	if !ok || emoji == nil || utf8.RuneCountInString(emoji.Emoticon) < 1 || utf8.RuneCountInString(emoji.Emoticon) > 32 || !isStandardReactionEmoji(emoji.Emoticon) {
		return "", errReactionInvalid
	}
	return emoji.Emoticon, nil
}

func parseMessageReactionListOffset(offset string) (int64, error) {
	if offset == "" {
		return 0, nil
	}
	if len(offset) < 2 || len(offset) > 26 {
		return 0, errors.New("reaction-list offset has an invalid length")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(offset)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != offset {
		return 0, errors.New("reaction-list offset is malformed")
	}
	value, err := strconv.ParseInt(string(decoded), 10, 64)
	if err != nil || value <= 0 || strconv.FormatInt(value, 10) != string(decoded) {
		return 0, errors.New("reaction-list offset is outside the id range")
	}
	return value, nil
}

func formatMessageReactionListOffset(reactorID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(reactorID, 10)))
}
