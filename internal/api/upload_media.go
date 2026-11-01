package api

import (
	"errors"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// handleUploadMedia stores a caller-owned upload behind an inert grant. The
// grant is not a download right: upload.getFile still requires a live message
// row naming the file.
func (h *handlers) handleUploadMedia(r *mtproto.Request) (bin.Encoder, error) {
	if h.uploadsDisabled {
		return nil, errUploadsDisabled
	}
	var req tg.MessagesUploadMediaRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	peerType, peerID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, err
	}
	photoMedia, isPhoto := req.Media.(*tg.InputMediaUploadedPhoto)
	documentMedia, isDocument := req.Media.(*tg.InputMediaUploadedDocument)
	if peerType == store.PeerTypeChannel && !isPhoto {
		return nil, errPeerIDInvalid
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, peerID, r.UserID); err != nil {
			return nil, err
		}
	}
	if !isPhoto && !isDocument {
		return nil, errMediaInvalid
	}

	var clientFileID int64
	var parts int
	var name, photoChecksum string
	var mediaRights []string
	if isPhoto {
		if photoMedia == nil || photoMedia.Flags != 0 || photoMedia.Spoiler || photoMedia.LivePhoto ||
			photoMedia.Stickers != nil || photoMedia.TTLSeconds != 0 || photoMedia.Video != nil {
			return nil, errMediaInvalid
		}
		clientFileID, parts, name, photoChecksum, err = inputPhotoFileParts(photoMedia.File)
		if err != nil {
			return nil, err
		}
		mediaRights = []string{"send_photos"}
	} else {
		if documentMedia == nil {
			return nil, errMediaInvalid
		}
		if _, ok := documentMedia.GetThumb(); ok {
			return nil, errMediaInvalid
		}
		clientFileID, parts, name, err = inputFileParts(documentMedia.File)
		if err != nil {
			return nil, err
		}
		mediaRights = documentSubtypeRights(documentMedia.Attributes)
	}

	if err := h.checkRateLimit(r, "message_send", h.rateLimitMessageSend); err != nil {
		return nil, err
	}
	if peerType == store.PeerTypeChat {
		if _, err = h.store.CheckChatWritePermission(r.Ctx, peerID, r.UserID, 0, mediaRights); err != nil {
			switch {
			case errors.Is(err, store.ErrNotMember):
				return nil, errPeerIDInvalid
			case errors.Is(err, store.ErrChatWriteForbidden):
				return nil, errChatWriteForbidden
			default:
				h.log.Error("check chat upload media permission", "user_id", r.UserID, "chat_id", peerID, "err", err)
				return nil, errInternal
			}
		}
	}
	if peerType == store.PeerTypeChannel {
		if err = h.store.CheckChannelPhotoPostPermission(r.Ctx, peerID, r.UserID, mediaRights); err != nil {
			if slowModeWait, ok := errors.AsType[*store.SlowModeWaitError](err); ok {
				return nil, rpcErr(420, slowModeWait.Error())
			}
			if errors.Is(err, store.ErrNotMember) {
				return nil, errPeerIDInvalid
			}
			if errors.Is(err, store.ErrChatWriteForbidden) {
				return nil, errChatWriteForbidden
			}
			h.log.Error("check channel upload media permission", "user_id", r.UserID, "channel_id", peerID, "err", err)
			return nil, errInternal
		}
	}

	var file store.File
	if isPhoto {
		file, err = h.assemblePhotoFileWithUploadGrant(r.Ctx, r.UserID, clientFileID, parts, name, photoChecksum)
	} else {
		file, err = h.assembleFileWithUploadGrant(r.Ctx, r.UserID, clientFileID, parts, name, documentMedia.MimeType, mediaRights)
	}
	if err != nil {
		return nil, err
	}
	return h.fileMediaToTL(file), nil
}
