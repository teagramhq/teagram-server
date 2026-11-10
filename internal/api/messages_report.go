package api

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const messageReportCommentMaxBytes = 512

var (
	errMessageReportOptionInvalid = rpcErr(400, "REPORT_OPTION_INVALID")
	messageReportReasons          = []messageReportReason{
		{label: "Copyright", reason: "copyright"},
		{label: "Abuse", reason: "abuse"},
		{label: "Other", reason: "other"},
	}
)

type messageReportReason struct {
	label  string
	reason string
}

func (h *handlers) handleReportMessage(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesReportRequest
	if err := req.Decode(r.BodyBuffer()); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if len(req.ID) == 0 || len(req.ID) > maxHistoryLimit {
		return nil, errMessageIDInvalid
	}
	messageIDs := make([]int64, len(req.ID))
	for i, messageID := range req.ID {
		if messageID <= 0 {
			return nil, errMessageIDInvalid
		}
		messageIDs[i] = int64(messageID)
	}
	if len(req.Message) > messageReportCommentMaxBytes {
		return nil, errMessageTooLong
	}
	if !utf8.ValidString(req.Message) || strings.ContainsRune(req.Message, '\x00') {
		return nil, errInputRequestInvalid
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	option := string(req.Option)
	reasonName, isComment := strings.CutPrefix(option, "comment:")
	if !isComment {
		visible, err := h.store.UserStickerSetReportMessagesVisible(r.Ctx, r.UserID, peerType, peerID, messageIDs)
		if err != nil {
			h.log.Error("resolve messages for report", "reporter_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "message_ids", messageIDs, "err", err)
			return nil, errInternal
		}
		if !visible {
			return nil, errMessageIDInvalid
		}
	}

	if len(req.Option) == 0 {
		options := make([]tg.MessageReportOption, 0, len(messageReportReasons))
		for _, reason := range messageReportReasons {
			options = append(options, tg.MessageReportOption{
				Text: reason.label, Option: []byte("reason:" + reason.reason),
			})
		}
		return &tg.ReportResultChooseOption{
			Title:   "Why are you reporting this message?",
			Options: options,
		}, nil
	}

	if reasonName, ok := strings.CutPrefix(option, "reason:"); ok {
		reason, ok := messageReportReasonByName(reasonName)
		if !ok {
			return nil, errMessageReportOptionInvalid
		}
		return &tg.ReportResultAddComment{
			Optional: true,
			Option:   []byte("comment:" + reason.reason),
		}, nil
	}
	if isComment {
		reason, ok := messageReportReasonByName(reasonName)
		if !ok {
			return nil, errMessageReportOptionInvalid
		}
		var visible bool
		if r.Tailnet() {
			visible, err = h.store.CreateUserStickerSetReportsUnmetered(
				r.Ctx, r.UserID, peerType, peerID, messageIDs, reason.reason, req.Message,
			)
		} else {
			visible, err = h.store.CreateUserStickerSetReports(
				r.Ctx, r.UserID, peerType, peerID, messageIDs, reason.reason, req.Message,
			)
		}
		if err != nil {
			if errors.Is(err, store.ErrUserStickerSetReportRateLimited) {
				return nil, FloodWaitError(3600)
			}
			if errors.Is(err, store.ErrUserStickerSetReportInputInvalid) {
				return nil, errInputRequestInvalid
			}
			h.log.Error("store user sticker-set reports", "reporter_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "message_ids", messageIDs, "err", err)
			return nil, errInternal
		}
		if !visible {
			return nil, errMessageIDInvalid
		}
		return &tg.ReportResultReported{}, nil
	}
	return nil, errMessageReportOptionInvalid
}

func messageReportReasonByName(name string) (messageReportReason, bool) {
	for _, reason := range messageReportReasons {
		if reason.reason == name {
			return reason, true
		}
	}
	return messageReportReason{}, false
}
