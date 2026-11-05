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
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if len(req.ID) != 1 || req.ID[0] <= 0 {
		return nil, errMessageIDInvalid
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
	setIDs, visible, err := h.store.UserStickerSetsInMessageForReport(r.Ctx, r.UserID, peerType, peerID, int64(req.ID[0]))
	if err != nil {
		h.log.Error("resolve message for report", "reporter_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "message_id", req.ID[0], "err", err)
		return nil, errInternal
	}
	if !visible {
		return nil, errMessageIDInvalid
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

	option := string(req.Option)
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
	if reasonName, ok := strings.CutPrefix(option, "comment:"); ok {
		reason, ok := messageReportReasonByName(reasonName)
		if !ok {
			return nil, errMessageReportOptionInvalid
		}
		if err := h.store.CreateUserStickerSetReportsForMessage(
			r.Ctx, r.UserID, setIDs, peerType, peerID, int64(req.ID[0]), reason.reason, req.Message,
		); err != nil {
			if errors.Is(err, store.ErrUserStickerSetReportRateLimited) {
				return nil, FloodWaitError(3600)
			}
			if errors.Is(err, store.ErrUserStickerSetReportInputInvalid) {
				return nil, errInputRequestInvalid
			}
			h.log.Error("store user sticker-set report", "reporter_id", r.UserID, "peer_type", peerType, "peer_id", peerID, "message_id", req.ID[0], "err", err)
			return nil, errInternal
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
