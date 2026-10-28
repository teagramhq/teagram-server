package api

import (
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleGetSearchResultsCalendar serves the photo/video preview requested by
// teagram-web's date picker.
func (h *handlers) handleGetSearchResultsCalendar(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetSearchResultsCalendarRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.SavedPeerID != nil {
		return nil, errFilterNotSupported
	}
	if _, ok := req.Filter.(*tg.InputMessagesFilterPhotoVideo); !ok {
		return nil, errFilterNotSupported
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	if peerType != store.PeerTypeUser && peerType != store.PeerTypeChat {
		return nil, errPeerIDInvalid
	}
	if err := h.checkRateLimit(r, "messages_search", h.rateLimitSearchMessages); err != nil {
		return nil, err
	}

	msgs, periods, count, err := h.store.SearchResultsCalendar(
		r.Ctx, r.UserID, peerType, peerID, int64(req.OffsetID), int64(req.OffsetDate), defaultHistoryLimit,
	)
	if err != nil {
		return nil, h.calendarStoreError(r.UserID, err)
	}

	files, err := h.loadFiles(r.Ctx, msgs)
	if err != nil {
		h.log.Error("get search results calendar files", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	var tlMsgs []tg.MessageClass
	var users []tg.UserClass
	var chats []tg.ChatClass
	if peerType == store.PeerTypeChat {
		result, renderErr := h.chatSearch(r, peerID, msgs, files, count, true)
		if renderErr != nil {
			return nil, renderErr
		}
		slice, ok := result.(*tg.MessagesMessagesSlice)
		if !ok {
			h.log.Error("get search results calendar chat response type", "user_id", r.UserID, "type", result)
			return nil, errInternal
		}
		tlMsgs, users, chats = slice.Messages, slice.Users, slice.Chats
	} else {
		tlMsgs = make([]tg.MessageClass, len(msgs))
		for i, message := range msgs {
			tlMsgs[i] = h.messageToTL(message, nil, files, nil, nil, h.dcID)
		}
		users, err = h.twoUsers(r.Ctx, r.UserID, peerID)
		if err != nil {
			h.log.Error("get search results calendar users", "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
	}

	tlPeriods := make([]tg.SearchResultsCalendarPeriod, len(periods))
	for i, period := range periods {
		tlPeriods[i] = tg.SearchResultsCalendarPeriod{
			Date: int(period.Date), MinMsgID: int(period.MinMsgID),
			MaxMsgID: int(period.MaxMsgID), Count: period.Count,
		}
	}

	response := &tg.MessagesSearchResultsCalendar{
		Count: count, Periods: tlPeriods, Messages: tlMsgs, Users: users, Chats: chats,
	}
	if len(msgs) > 0 {
		minDate, minMsgID := msgs[0].Date.Unix(), msgs[0].LocalID
		for _, message := range msgs[1:] {
			if message.Date.Unix() < minDate {
				minDate = message.Date.Unix()
			}
			if message.LocalID < minMsgID {
				minMsgID = message.LocalID
			}
		}
		response.MinDate, response.MinMsgID = int(minDate), int(minMsgID)
	}
	return response, nil
}

func (h *handlers) calendarStoreError(userID int64, err error) error {
	if errors.Is(err, store.ErrNotMember) {
		var joinedErr interface{ Unwrap() []error }
		if errors.As(err, &joinedErr) {
			h.log.Error("get search results calendar", "user_id", userID, "err", err)
		}
		return errPeerIDInvalid
	}
	h.log.Error("get search results calendar", "user_id", userID, "err", err)
	return errInternal
}
