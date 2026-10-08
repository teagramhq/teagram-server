package api

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// partsReader streams an in-flight upload's parts out of the blob store in
// index order, so assembling a 100 MiB file never holds more than one 512 KiB
// part in memory at a time. It carries the refs the assembly already read —
// one row each, never one part's bytes each — and fetches the bytes as it goes.
// A streaming blob backend may read it on a transport goroutine and return
// from Put while that goroutine is still in Read. On Put failure, assembly
// stops new reads and waits for an active read before inspecting its error.
type partsReader struct {
	ctx       context.Context
	store     *store.Store
	refs      []store.UploadPartRef
	next      int
	buf       []byte
	size      int64
	readErr   atomic.Pointer[partsReadFailure]
	readState atomic.Uint32
	readDone  chan struct{}
}

type partsReadFailure struct {
	err error
}

const (
	partsReaderReady uint32 = iota
	partsReaderReading
	partsReaderStopped
	partsReaderStoppedReading
)

var errConcurrentPartsReaderRead = errors.New("concurrent parts reader reads")

func newPartsReader(ctx context.Context, s *store.Store, refs []store.UploadPartRef, size int64) *partsReader {
	return &partsReader{ctx: ctx, store: s, refs: refs, size: size, readDone: make(chan struct{})}
}

func (p *partsReader) beginRead() error {
	for {
		switch p.readState.Load() {
		case partsReaderReady:
			if p.readState.CompareAndSwap(partsReaderReady, partsReaderReading) {
				return nil
			}
		case partsReaderReading:
			return errConcurrentPartsReaderRead
		case partsReaderStopped, partsReaderStoppedReading:
			return io.EOF
		}
	}
}

func (p *partsReader) finishRead() {
	for {
		switch p.readState.Load() {
		case partsReaderReading:
			if p.readState.CompareAndSwap(partsReaderReading, partsReaderReady) {
				return
			}
		case partsReaderStoppedReading:
			if p.readState.CompareAndSwap(partsReaderStoppedReading, partsReaderStopped) {
				close(p.readDone)
				return
			}
		}
	}
}

func (p *partsReader) stopAndWait() {
	for {
		switch p.readState.Load() {
		case partsReaderReady:
			if p.readState.CompareAndSwap(partsReaderReady, partsReaderStopped) {
				close(p.readDone)
				return
			}
		case partsReaderReading:
			if p.readState.CompareAndSwap(partsReaderReading, partsReaderStoppedReading) {
				<-p.readDone
				return
			}
		case partsReaderStopped:
			return
		case partsReaderStoppedReading:
			<-p.readDone
			return
		}
	}
}

func (p *partsReader) readError() error {
	if failure := p.readErr.Load(); failure != nil {
		return failure.err
	}
	return nil
}

// Size reports the byte count already established by the upload-part rows.
// S3-compatible stores use it to set Content-Length without reading the
// streaming reader into a second whole-file buffer.
func (p *partsReader) Size() int64 {
	if p.size != 0 {
		return p.size
	}
	var size int64
	for _, ref := range p.refs {
		size += ref.Size
	}
	return size
}

// Read fills b from the current part, fetching the next one when it runs out.
// The loop rather than an if matters: a zero-length part would otherwise make
// Read return (0, nil) forever.
func (p *partsReader) Read(b []byte) (int, error) {
	if err := p.beginRead(); err != nil {
		return 0, err
	}
	defer p.finishRead()

	for len(p.buf) == 0 {
		if p.next >= len(p.refs) {
			return 0, io.EOF
		}
		payload, err := p.store.ReadUploadPart(p.ctx, p.refs[p.next])
		if err != nil {
			p.readErr.CompareAndSwap(nil, &partsReadFailure{err: err})
			return 0, err
		}
		p.buf = payload
		p.next++
	}
	n := copy(b, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

// sanitizeMIME constrains the client-supplied mime type before it is stored.
// The server never executes it or selects a code path on it, and it never
// appears in a storage key — the risk is downstream: it is echoed to every
// other client, and it is the field most likely to become a Content-Type the
// first time a CDN or webfile path exists, where a CR or LF in it is a
// header-splitting primitive. Constraining it here means it can never become
// that. Anything that does not qualify becomes the generic type rather than an
// error, so a client with an odd type still sends its file.
func sanitizeMIME(s string) string {
	const fallback = "application/octet-stream"
	slash := strings.IndexByte(s, '/')
	if len(s) == 0 || len(s) > 255 || slash <= 0 || slash == len(s)-1 {
		return fallback
	}
	if strings.IndexByte(s[slash+1:], '/') >= 0 {
		return fallback
	}
	for i := range len(s) {
		// Printable ASCII excluding space: no CR, no LF, no NUL, no control
		// bytes, nothing that needs an encoding to survive a round trip.
		if s[i] < 0x21 || s[i] > 0x7e {
			return fallback
		}
	}
	return s
}

// sanitizeFileName constrains the client-supplied file name. It is stored as an
// opaque column and echoed on download; it is never part of a blob key, which
// is derived from the file id alone, so path traversal is closed by
// construction rather than by this function. What is left is what Postgres
// cannot store and what corrupts a rendered name, and an unusable name becomes
// no name rather than an error.
//
// Spaces and non-ASCII are deliberately allowed: a file name is display text,
// not a header value, and forcing it to ASCII would mangle every non-English
// name.
func sanitizeFileName(s string) string {
	if len(s) > 255 || !validText(s) || strings.ContainsFunc(s, isDangerousRune) {
		return ""
	}
	return s
}

// documentSubtypeRights classifies accepted document attributes for storage.
// Nil means an attribute was unrecognized or malformed; an empty non-nil slice
// means the document is known generic. Names and image dimensions do not imply
// a restricted subtype.
func documentSubtypeRights(attributes []tg.DocumentAttributeClass) []string {
	rights := make(map[string]struct{})
	for _, attribute := range attributes {
		switch value := attribute.(type) {
		case *tg.DocumentAttributeFilename:
			if value == nil {
				return nil
			}
		case *tg.DocumentAttributeImageSize:
			if value == nil {
				return nil
			}
		case *tg.DocumentAttributeSticker:
			if value == nil {
				return nil
			}
			rights["send_stickers"] = struct{}{}
		case *tg.DocumentAttributeCustomEmoji:
			if value == nil {
				return nil
			}
			rights["send_stickers"] = struct{}{}
		case *tg.DocumentAttributeAnimated:
			if value == nil {
				return nil
			}
			rights["send_gifs"] = struct{}{}
		case *tg.DocumentAttributeVideo:
			if value == nil {
				return nil
			}
			if value.RoundMessage {
				rights["send_roundvideos"] = struct{}{}
			} else {
				rights["send_videos"] = struct{}{}
			}
		case *tg.DocumentAttributeAudio:
			if value == nil {
				return nil
			}
			if value.Voice {
				rights["send_voices"] = struct{}{}
			} else {
				rights["send_audios"] = struct{}{}
			}
		default:
			return nil
		}
	}

	classified := make([]string, 0, len(rights))
	for right := range rights {
		classified = append(classified, right)
	}
	sort.Strings(classified)
	return classified
}

// handleSendMedia is the direct handler entry used by tests and callers that do
// not write an RPC result. The dispatcher uses handleSendMediaAfterReply so the
// sender reply metadata is returned for the atomic watermark update, and the
// sender notification is published only after that result reaches the
// originating connection.
func (h *handlers) handleSendMedia(r *mtproto.Request) (bin.Encoder, error) {
	res, _, _, err := h.handleSendMediaAfterReply(r)
	return res, err
}

// handleSendMediaAfterReply serves messages.sendMedia: it assembles an
// in-flight document or validated photo and sends the resulting media message
// to a user or a chat. It returns a hook that advances the originating sender's
// push watermark after the RPC result is written.
//
// The file id written to messages.file_id is the one this handler just
// allocated under the caller's own account, never one the client named, so the
// download gate's "live message owned by caller" check can never be handed an
// entitlement to somebody else's file. The client-supplied id in the input file
// addresses upload parts only, and those are looked up under the caller's user
// id.
func (h *handlers) handleSendMediaAfterReply(r *mtproto.Request) (bin.Encoder, *replyUpdate, func(), error) {
	return h.handleSendMediaAfterReplyOnConn(nil, r)
}

func (h *handlers) handleSendMediaAfterReplyOnConn(c *mtproto.Conn, r *mtproto.Request) (bin.Encoder, *replyUpdate, func(), error) {
	var req tg.MessagesSendMediaRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, nil, errAuthKeyUnreg
	}
	if err := rejectUnsupportedSendOptions(&req); err != nil {
		return nil, nil, nil, err
	}
	// Validate before any expensive work.
	pollMedia, isPoll := req.Media.(*tg.InputMediaPoll)
	if !isPoll && !validText(req.Message) {
		return nil, nil, nil, errMessageEmpty
	}
	var descriptionEntities []store.PollDescriptionEntity
	if isPoll {
		if !validText(req.Message) {
			return nil, nil, nil, errMessageEmpty
		}
		var entityErr error
		descriptionEntities, entityErr = h.encodeMessageEntities(r.UserID, req.Message, req.Entities)
		if entityErr != nil {
			return nil, nil, nil, entityErr
		}
	}
	peerType, toID, err := h.inputPeer(req.Peer, r.UserID)
	if err != nil {
		return nil, nil, nil, err
	}
	photoMedia, isPhoto := req.Media.(*tg.InputMediaUploadedPhoto)
	if _, ok := req.Media.(*tg.InputMediaPhoto); ok {
		return nil, nil, nil, errMediaInvalid
	}
	// A channel post carries a photo or a poll and nothing else. Polls use the
	// shared channel-message store path below and never fall through to owner
	// rows; documents and every other media type still have nowhere to go in a
	// channel, and a reference to an already-stored file stays rejected for
	// every peer. PEER_ID_INVALID is the same answer a non-member gets, so this
	// refusal says nothing about whether the channel exists.
	if peerType == store.PeerTypeChannel && !isPoll && !isPhoto {
		return nil, nil, nil, errPeerIDInvalid
	}
	if isPoll {
		return h.handleSendPollAfterReplyOnConn(c, r, &req, pollMedia, descriptionEntities, peerType, toID)
	}
	if peerType == store.PeerTypeChat {
		if err = h.requireMember(r.Ctx, toID, r.UserID); err != nil {
			return nil, nil, nil, err
		}
	}
	var clientFileID int64
	var parts int
	var name, photoChecksum string
	if isPhoto {
		if photoMedia == nil || photoMedia.Flags != 0 || photoMedia.Spoiler || photoMedia.LivePhoto ||
			photoMedia.Stickers != nil || photoMedia.TTLSeconds != 0 || photoMedia.Video != nil {
			return nil, nil, nil, errMediaInvalid
		}
		clientFileID, parts, name, photoChecksum, err = inputPhotoFileParts(photoMedia.File)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// A channel photo retry resolves in the channel's own (channel_id,
	// random_id) space, never in the sender's message rows: the two spaces are
	// independent, and a random id the caller used for a DM says nothing about a
	// channel post. store.ChannelPhotoRetryAs re-checks current posting rights
	// under the channel state lock before it reads the id, so a ban or demotion
	// cannot be probed through a retry, and it replays only a live post the
	// caller authored whose persisted media really is a photo.
	if req.RandomID != 0 && peerType == store.PeerTypeChannel {
		message, pts, duplicate, retryErr := h.store.ChannelPhotoRetryAs(r.Ctx, toID, r.UserID, req.RandomID)
		switch {
		case errors.Is(retryErr, store.ErrNotMember):
			return nil, nil, nil, errPeerIDInvalid
		case errors.Is(retryErr, store.ErrRandomIDDuplicate):
			return nil, nil, nil, errRandomIDDuplicate
		case errors.Is(retryErr, store.ErrMediaInvalid), errors.Is(retryErr, store.ErrMessageInvalid):
			return nil, nil, nil, errMediaInvalid
		case retryErr != nil:
			h.log.Error("channel photo retry", "user_id", r.UserID, "channel_id", toID, "err", retryErr)
			return nil, nil, nil, errInternal
		case duplicate:
			return h.channelPhotoSendResponse(r, &req, toID, message, pts)
		}
	}

	// Check for a transport retry before any expensive work.
	//
	// A resend whose original is soft-deleted is refused outright, and the
	// refusal is the same one a caller gets for media they never had. It does
	// not matter here whether the file behind that original still exists, and
	// the indistinguishability is the point rather than a side effect: answering
	// differently in the two cases tells the uploader which of their files the
	// eraser has taken, and therefore which recipient deleted which media, from
	// one repeated request. That is threat model finding 6 with its accepted
	// mitigation removed — finding 6 was accepted because a randomized sweep
	// interval degrades "at 14:32" to "eventually", and a probe that is polled
	// rather than timed is immune to that.
	//
	// Refusing here rather than below is deliberate, and it is a rule about
	// deleted messages rather than a second opinion about missing files. The
	// interlock stays the only thing that decides whether a reference may be
	// written; this decides whether a message the sender themself deleted may be
	// replayed, which is a different question about a different subject, and the
	// ticket's rationale for refusing a resend after erasure — "the original the
	// dedup would replay is a message the sender themself deleted" — was always
	// true whether or not the sweep had been past. Reading no files row keeps it
	// that way.
	//
	// Falling through instead would leave two answers to one request: the
	// interlock refuses when the file is gone, but when the file survives the
	// store's own dedup replays the deleted original and the handler renders its
	// document. Fixing it in that dedup would change SendMessage for every
	// caller including the non-media path, which is a far wider blast radius.
	// Reaching this branch at all means the client deleted the message between
	// the send and the retry, which is not a transport retry.
	if req.RandomID != 0 && peerType != store.PeerTypeChannel {
		existing, ok, err := h.store.MessageByRandomID(r.Ctx, r.UserID, req.RandomID)
		if err != nil {
			h.log.Error("random_id lookup", "user_id", r.UserID, "err", err)
			return nil, nil, nil, errInternal
		}
		if ok && existing.Deleted {
			return nil, nil, nil, errMediaInvalid
		}
		if ok {
			// Retry: return the stored message, at the pts it occupies, without
			// rate-limiting or file assembly.
			pts, err := h.store.MessagePts(r.Ctx, r.UserID, existing.LocalID)
			if err != nil {
				h.log.Error("read stored message pts on retry", "user_id", r.UserID, "err", err)
				return nil, nil, nil, errInternal
			}
			if peerType == store.PeerTypeChat {
				chats, err := h.loadChats(r.Ctx, map[int64]bool{toID: true}, r.UserID, nil)
				if err != nil {
					h.log.Error("load chats on retry", "err", err)
					return nil, nil, nil, errInternal
				}
				users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
				if err != nil {
					h.log.Error("load users on retry", "err", err)
					return nil, nil, nil, errInternal
				}
				files, err := h.loadFiles(r.Ctx, []store.Message{existing})
				if err != nil {
					h.log.Error("load files on retry", "err", err)
					return nil, nil, nil, errInternal
				}
				res := &tg.Updates{
					Updates: []tg.UpdateClass{
						&tg.UpdateMessageID{ID: int(existing.LocalID), RandomID: req.RandomID},
						&tg.UpdateNewMessage{Message: messageToTL(existing, nil, files, nil, nil), Pts: pts, PtsCount: 1},
					},
					Users: users,
					Chats: chats,
					Date:  int(existing.Date.Unix()),
				}
				update, afterReply := h.retryReplyAfterSuccess(senderRPCAttempt{}, r, peerType, pts)
				return res, update, afterReply, nil
			}
			attempt := beginSenderRPCAt(c, r, pts)
			users, err := h.twoUsers(r.Ctx, r.UserID, toID)
			if err != nil {
				h.log.Error("load users on retry", "err", err)
				h.clearSenderAndNotify(attempt, r)
				return nil, nil, nil, errInternal
			}
			files, err := h.loadFiles(r.Ctx, []store.Message{existing})
			if err != nil {
				h.log.Error("load files on retry", "err", err)
				h.clearSenderAndNotify(attempt, r)
				return nil, nil, nil, errInternal
			}
			res := &tg.Updates{
				Updates: []tg.UpdateClass{
					&tg.UpdateMessageID{ID: int(existing.LocalID), RandomID: req.RandomID},
					&tg.UpdateNewMessage{Message: messageToTL(existing, nil, files, nil, nil), Pts: pts, PtsCount: 1},
				},
				Users: users,
				Date:  int(existing.Date.Unix()),
			}
			setSenderRPCPts(attempt, pts)
			update, afterReply := h.retryReplyAfterSuccess(attempt, r, peerType, pts)
			return res, update, afterReply, nil
		}
	}

	documentMedia, isDocument := req.Media.(*tg.InputMediaUploadedDocument)
	var mediaRights []string
	if isPhoto {
		mediaRights = []string{"send_photos"}
	} else {
		if !isDocument {
			return nil, nil, nil, errMediaInvalid
		}
		// M5 stores no thumbnails, so a thumbnail is a second file body this
		// handler has nowhere to put.
		if _, ok := documentMedia.GetThumb(); ok {
			return nil, nil, nil, errMediaInvalid
		}
		mediaRights = documentSubtypeRights(documentMedia.Attributes)
	}
	// Committed retries returned above. Charge new sends before checking chat
	// permissions, since that check takes the chat row lock. A concurrent
	// duplicate may spend a token, but repeated denied sends are throttled before
	// they can contend with other chat writes.
	if err := h.checkRateLimit(r, "message_send", h.rateLimitMessageSend); err != nil {
		return nil, nil, nil, err
	}
	duplicate := false
	if peerType == store.PeerTypeChat {
		duplicate, err = h.store.CheckChatWritePermission(r.Ctx, toID, r.UserID, req.RandomID, mediaRights)
		if errors.Is(err, store.ErrNotMember) {
			return nil, nil, nil, errPeerIDInvalid
		}
		if errors.Is(err, store.ErrChatWriteForbidden) {
			return nil, nil, nil, errChatWriteForbidden
		}
		if err != nil {
			h.log.Error("check chat media permission", "user_id", r.UserID, "chat_id", toID, "err", err)
			return nil, nil, nil, errInternal
		}
	}
	// Read-only precheck for a channel photo send: assembly is the one step a
	// refused sender must not be able to make the server repeat per attempt, and
	// the channel_state row lock cannot be held across a blob Put. The store's
	// post transaction repeats every one of these decisions under that lock and
	// is the authority; this only declines the work.
	if peerType == store.PeerTypeChannel {
		if err = h.store.CheckChannelPhotoPostPermission(r.Ctx, toID, r.UserID, mediaRights); err != nil {
			if slowModeWait, ok := errors.AsType[*store.SlowModeWaitError](err); ok {
				return nil, nil, nil, rpcErr(420, slowModeWait.Error())
			}
			if errors.Is(err, store.ErrNotMember) {
				return nil, nil, nil, errPeerIDInvalid
			}
			if errors.Is(err, store.ErrChatWriteForbidden) {
				return nil, nil, nil, errChatWriteForbidden
			}
			h.log.Error("check channel photo post permission", "user_id", r.UserID, "channel_id", toID, "err", err)
			return nil, nil, nil, errInternal
		}
	}

	if !duplicate && !isPhoto {
		clientFileID, parts, name, err = inputFileParts(documentMedia.File)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	// Assembly is the one step in this handler the send path's dedup cannot
	// cover: it consumes the upload parts, so a resend that reached assembly
	// would fail on an upload that is no longer there and report MEDIA_INVALID
	// for a message that was in fact delivered. Check again after the permission
	// lock, since a send with the same random_id may have committed while this
	// request waited for that lock.
	//
	// A channel send skips this lookup and assembles: its dedup lives in the
	// channel's (channel_id, random_id) space and already ran above, and the
	// sender's own message rows are a different space whose file id this request
	// must never carry into a channel post.
	var fileID int64
	existing := false
	if peerType != store.PeerTypeChannel {
		fileID, existing, err = h.resendFileID(r.Ctx, r.UserID, req.RandomID)
		if err != nil {
			return nil, nil, nil, err
		}
		if duplicate && !existing {
			return nil, nil, nil, errMediaInvalid
		}
	}
	if !existing {
		var file store.File
		var aerr error
		if isPhoto {
			file, aerr = h.assemblePhotoFile(r.Ctx, r.UserID, clientFileID, parts, name, photoChecksum)
		} else {
			file, aerr = h.assembleFile(r.Ctx, r.UserID, clientFileID, parts, name, documentMedia.MimeType, documentSubtypeRights(documentMedia.Attributes))
		}
		if aerr != nil {
			return nil, nil, nil, aerr
		}
		fileID = file.ID
	}

	if peerType == store.PeerTypeChat {
		return h.sendChatMedia(c, r, toID, &req, fileID, mediaRights)
	}
	if peerType == store.PeerTypeChannel {
		return h.sendChannelPhoto(r, toID, &req, fileID)
	}

	attempt := beginSenderRPC(c, r)
	sender, senderPts, _, _, err := h.store.SendMessage(r.Ctx, r.UserID, toID, req.Message, req.RandomID, fileID, 0)
	// The file this send names is gone: the send wrote nothing, and the caller
	// hears that rather than an internal error for a state that is theirs to
	// retry from.
	if errors.Is(err, store.ErrFileMissing) {
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errMediaInvalid
	}
	if err != nil {
		h.clearSenderAndNotify(attempt, r)
		h.log.Error("send media", "user_id", r.UserID, "err", err)
		return nil, nil, nil, errInternal
	}
	setSenderRPCPts(attempt, senderPts)
	if h.afterSenderCommit != nil {
		h.afterSenderCommit()
	}

	if toID != r.UserID {
		h.notify(r.Ctx, toID)
	}

	users, err := h.twoUsers(r.Ctx, r.UserID, toID)
	if err != nil {
		h.log.Error("send media users", "err", err)
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errInternal
	}
	// Hydrated off the row that was actually stored rather than off the file
	// this call assembled: on a resend those differ, and keying the map on the
	// wrong id renders the reply as a plain message.
	files, err := h.loadFiles(r.Ctx, []store.Message{sender})
	if err != nil {
		h.log.Error("send media files", "user_id", r.UserID, "err", err)
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errInternal
	}
	res := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: int(sender.LocalID), RandomID: req.RandomID},
			&tg.UpdateNewMessage{Message: messageToTL(sender, nil, files, nil, nil), Pts: senderPts, PtsCount: 1},
		},
		Users: users,
		Date:  int(sender.Date.Unix()),
	}
	update := &replyUpdate{
		owner:   r.UserID,
		authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID),
		pts:     senderPts,
		onFailure: func() {
			h.clearSenderAndNotify(attempt, r)
		},
	}
	afterReply := func() {
		h.notifySendAfterReply(r, senderPts)
	}
	return res, update, afterReply, nil
}

// sendChatMedia fans assembled media out to every member of chatID, whose
// membership requireMember has already established. It returns sender metadata
// and a hook that publishes the fan-out only after the RPC result is written.
func (h *handlers) sendChatMedia(
	c *mtproto.Conn, r *mtproto.Request, chatID int64, req *tg.MessagesSendMediaRequest, fileID int64, mediaRights []string,
) (bin.Encoder, *replyUpdate, func(), error) {
	// Rate limit already checked in handleSendMedia before the peer split.
	attempt := beginSenderRPC(c, r)
	sender, perOwner, _, err := h.store.SendChatMessage(r.Ctx, store.FanOut{
		ChatID: chatID, FromID: r.UserID, Text: req.Message, RandomID: req.RandomID, FileID: fileID,
		MediaRights: mediaRights,
	})
	// Every exit below releases a barrier that held this connection's generic
	// delivery back for the whole send, so an unrelated update that landed during
	// it was skipped rather than queued on the socket. The nudge is what puts that
	// update back on the wire; releasing the barrier alone leaves it waiting for
	// someone else's notification.
	if errors.Is(err, store.ErrNotMember) {
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errPeerIDInvalid
	}
	if errors.Is(err, store.ErrChatWriteForbidden) {
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errChatWriteForbidden
	}
	if errors.Is(err, store.ErrFileMissing) {
		h.clearSenderAndNotify(attempt, r)
		return nil, nil, nil, errMediaInvalid
	}
	if err != nil {
		h.clearSenderAndNotify(attempt, r)
		h.log.Error("send chat media", "user_id", r.UserID, "chat_id", chatID, "err", err)
		return nil, nil, nil, errInternal
	}
	senderPts := perOwner[r.UserID]
	setSenderRPCPts(attempt, senderPts)
	if h.afterSenderCommit != nil {
		h.afterSenderCommit()
	}
	notifyCommitted := func() {
		clearSenderRPC(attempt)
		h.notifyOwners(r.Ctx, perOwner, 0)
	}

	recipients := make(map[int64]bool, len(perOwner))
	for uid := range perOwner {
		recipients[uid] = true
	}
	users, err := h.loadUsers(r.Ctx, recipients, r.UserID)
	if err != nil {
		notifyCommitted()
		h.log.Error("send chat media users", "err", err)
		return nil, nil, nil, errInternal
	}
	chats, err := h.loadChats(r.Ctx, map[int64]bool{chatID: true}, r.UserID, nil)
	if err != nil {
		notifyCommitted()
		h.log.Error("send chat media chats", "err", err)
		return nil, nil, nil, errInternal
	}
	files, err := h.loadFiles(r.Ctx, []store.Message{sender})
	if err != nil {
		notifyCommitted()
		h.log.Error("send chat media files", "user_id", r.UserID, "chat_id", chatID, "err", err)
		return nil, nil, nil, errInternal
	}
	result := &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: int(sender.LocalID), RandomID: req.RandomID},
			&tg.UpdateNewMessage{Message: messageToTL(sender, nil, files, nil, nil), Pts: senderPts, PtsCount: 1},
		},
		Users: users,
		Chats: chats,
		Date:  int(sender.Date.Unix()),
	}
	update := &replyUpdate{
		owner:   r.UserID,
		authKey: mtproto.AuthKeyIDInt64(r.AuthKeyID),
		pts:     senderPts,
		onFailure: func() {
			notifyCommitted()
		},
	}
	afterReply := func() {
		h.notifyOwners(r.Ctx, perOwner, r.UserID)
		h.notifySendAfterReply(r, senderPts)
	}
	return result, update, afterReply, nil
}

// sendChannelPhoto posts an assembled photo to a channel.
//
// There is no membership, restriction or slow-mode check here on purpose, and
// it is the same reasoning sendChannelMessage records for text:
// store.PostChannelPhotoAs re-decides all of them inside one transaction under
// the channel_state row lock, and that is the authorization boundary. The
// read-only precheck in handleSendMediaAfterReplyOnConn exists only to decline
// assembly, so a ban or demotion winning while the upload was being assembled
// is caught here and creates no post and no event. The file this call assembled
// is then simply unreferenced — charged to the sender and reclaimable under the
// accepted erasure policy, exactly as a refused basic-group send leaves it.
func (h *handlers) sendChannelPhoto(
	r *mtproto.Request, channelID int64, req *tg.MessagesSendMediaRequest, fileID int64,
) (bin.Encoder, *replyUpdate, func(), error) {
	message, pts, duplicate, err := h.store.PostChannelPhotoAs(r.Ctx, channelID, r.UserID, req.RandomID, req.Message, fileID, 0)
	if slowModeWait, ok := errors.AsType[*store.SlowModeWaitError](err); ok {
		return nil, nil, nil, rpcErr(420, slowModeWait.Error())
	}
	switch {
	case errors.Is(err, store.ErrNotMember):
		return nil, nil, nil, errPeerIDInvalid
	case errors.Is(err, store.ErrChatWriteForbidden):
		return nil, nil, nil, errChatWriteForbidden
	case errors.Is(err, store.ErrRandomIDDuplicate):
		return nil, nil, nil, errRandomIDDuplicate
	case errors.Is(err, store.ErrMediaInvalid), errors.Is(err, store.ErrMessageInvalid):
		return nil, nil, nil, errMediaInvalid
	case errors.Is(err, store.ErrFileMissing):
		// The file this send names is gone: the post wrote nothing, and the
		// caller hears that rather than an internal error for a state that is
		// theirs to retry from.
		return nil, nil, nil, errMediaInvalid
	case err != nil:
		h.log.Error("send channel photo", "user_id", r.UserID, "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	// Only notify when the post is new. A duplicate means another caller already
	// committed the same random_id and fired the notify.
	if !duplicate {
		h.notifyChannelPost(r.Ctx, channelID)
	}
	return h.channelPhotoSendResponse(r, req, channelID, message, pts)
}

// channelPhotoSendResponse builds the poster-side Updates for a channel photo
// post or the replay of one. The media is hydrated from the row that was
// actually stored rather than from the file this call assembled: on a duplicate
// those differ, and naming the wrong id renders the reply as a plain post.
func (h *handlers) channelPhotoSendResponse(
	r *mtproto.Request, req *tg.MessagesSendMediaRequest, channelID int64, message store.ChannelMessage, pts int,
) (bin.Encoder, *replyUpdate, func(), error) {
	channels, err := h.loadChannels(r.Ctx, map[int64]bool{channelID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel photo channel", "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	users, err := h.loadUsers(r.Ctx, map[int64]bool{r.UserID: true}, r.UserID)
	if err != nil {
		h.log.Error("load channel photo sender", "user_id", r.UserID, "err", err)
		return nil, nil, nil, errInternal
	}
	files, err := h.loadChannelFiles(r.Ctx, []store.ChannelMessage{message})
	if err != nil {
		h.log.Error("load channel photo files", "channel_id", channelID, "err", err)
		return nil, nil, nil, errInternal
	}
	messageTL, err := channelMessageToTL(message, r.UserID, files)
	if err != nil {
		h.log.Error("render channel photo message", "channel_id", channelID, "local_id", message.LocalID, "err", err)
		return nil, nil, nil, errInternal
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateMessageID{ID: int(message.LocalID), RandomID: req.RandomID},
			&tg.UpdateNewChannelMessage{Message: messageTL, Pts: pts, PtsCount: 1},
		},
		Chats: channels,
		Users: users,
		Date:  int(message.Date.Unix()),
	}, nil, nil, nil
}

// resendFileID reports the stored file id and whether randomID already names a
// message owned by userID. A retry of a message without media is still a retry:
// the caller must skip file assembly and let the send transaction return its
// original row.
func (h *handlers) resendFileID(ctx context.Context, userID, randomID int64) (int64, bool, error) {
	if randomID == 0 {
		return 0, false, nil
	}
	existing, ok, err := h.store.MessageByRandomID(ctx, userID, randomID)
	if err != nil {
		h.log.Error("send media random id", "user_id", userID, "err", err)
		return 0, false, errInternal
	}
	if !ok {
		return 0, false, nil
	}
	if existing.Deleted {
		return 0, true, errMediaInvalid
	}
	return existing.FileID, true, nil
}

// inputFileParts reads the three fields assembly needs off an input file.
// tg.InputFile and tg.InputFileBig carry the same id, part count and name;
// every other input file names an already-stored file, which M5 does not send.
func inputFileParts(f tg.InputFileClass) (id int64, parts int, name string, err error) {
	switch v := f.(type) {
	case *tg.InputFile:
		id, parts, name = v.ID, v.Parts, v.Name
	case *tg.InputFileBig:
		id, parts, name = v.ID, v.Parts, v.Name
	default:
		return 0, 0, "", errMediaInvalid
	}
	if id == 0 || parts <= 0 {
		return 0, 0, "", errMediaInvalid
	}
	return id, parts, name, nil
}

func inputPhotoFileParts(f tg.InputFileClass) (id int64, parts int, name, checksum string, err error) {
	file, ok := f.(*tg.InputFile)
	if !ok || file == nil {
		return 0, 0, "", "", errMediaInvalid
	}
	if file.MD5Checksum != "" {
		if len(file.MD5Checksum) != 32 {
			return 0, 0, "", "", errMediaInvalid
		}
		if _, err := hex.DecodeString(file.MD5Checksum); err != nil {
			return 0, 0, "", "", errMediaInvalid
		}
	}
	id, parts, name, err = inputFileParts(file)
	if err != nil {
		return 0, 0, "", "", err
	}
	return id, parts, name, file.MD5Checksum, nil
}

func (h *handlers) uploadPartsForAssembly(
	ctx context.Context, userID, clientFileID int64, parts int,
) ([]store.UploadPartRef, int64, error) {
	n, maxIndex, total, err := h.store.UploadPartsSummary(ctx, userID, clientFileID)
	if err != nil {
		h.log.Error("assemble file", "user_id", userID, "err", err)
		return nil, 0, errInternal
	}
	if n != int64(parts) || maxIndex != parts-1 || total <= 0 {
		return nil, 0, errMediaInvalid
	}
	refs, err := h.store.UploadPartRefs(ctx, userID, clientFileID)
	if err != nil {
		h.log.Error("assemble file", "user_id", userID, "err", err)
		return nil, 0, errInternal
	}
	if len(refs) != parts {
		return nil, 0, errMediaInvalid
	}
	for i, ref := range refs {
		if ref.Index != i || ref.Size <= 0 {
			return nil, 0, errMediaInvalid
		}
	}
	return refs, total, nil
}

// assembleFile turns an in-flight upload into a stored file. The order is the
// contract: the files row is created before the bytes are written and marked
// stored only after, so a crash between them leaves a row that no download can
// resolve rather than a file id serving whatever is at its key. The assembly
// claim is acquired before the allocation commits, then the completion
// transaction takes the files row's shared interlock before Put and holds it
// through the stored transition's commit, so the eraser cannot reclaim a live
// upload even with a small cutoff.
func (h *handlers) assembleFile(
	ctx context.Context, userID, clientFileID int64, parts int, name, mimeType string, subtypeRights []string,
) (store.File, error) {
	refs, total, err := h.uploadPartsForAssembly(ctx, userID, clientFileID, parts)
	if err != nil {
		return store.File{}, err
	}

	var written int64
	var missingPartErr error
	file, err := h.store.AllocateAndCompleteFile(ctx, userID, total, sanitizeMIME(mimeType), sanitizeFileName(name), h.maxUserStorageBytes, subtypeRights, func(file store.File) error {
		var err error
		reader := newPartsReader(ctx, h.store, refs, total)
		written, err = h.blobs.Put(ctx, blob.Key(file.ID), reader)
		if err != nil {
			reader.stopAndWait()
			if readErr := reader.readError(); errors.Is(readErr, store.ErrUploadPartMissing) {
				missingPartErr = readErr
			}
			return err
		}
		// A mismatch means the parts changed under the read, so the blob does
		// not hold the file the row describes and must not be marked stored.
		if written != total {
			return fmt.Errorf("wrote %d bytes, expected %d", written, total)
		}
		return nil
	})
	if errors.Is(err, store.ErrStorageQuota) {
		return store.File{}, errFileQuota
	}
	if err != nil {
		if missingPartErr != nil {
			h.log.Error("assemble file", "user_id", userID, "file_id", file.ID, "err", err, "part_read_err", missingPartErr)
			return store.File{}, errMediaInvalid
		}
		h.log.Error("assemble file", "user_id", userID, "file_id", file.ID, "err", err)
		return store.File{}, errInternal
	}
	// The parts are redundant once the blob is stored, and the TTL sweeper
	// takes whatever this leaves behind, so a failure here does not fail a send
	// that has already succeeded.
	if _, err = h.store.DeleteUploadParts(ctx, userID, clientFileID); err != nil {
		h.log.Error("delete upload parts", "user_id", userID, "file_id", clientFileID, "err", err)
	}

	file.Stored = true
	return file, nil
}

func (h *handlers) assemblePhotoFile(
	ctx context.Context, userID, clientFileID int64, parts int, name, checksum string,
) (store.File, error) {
	refs, total, err := h.uploadPartsForAssembly(ctx, userID, clientFileID, parts)
	if err != nil {
		return store.File{}, err
	}
	if total > maxPhotoJPEGBytes {
		return store.File{}, errMediaInvalid
	}

	var missingPartErr error
	file, err := h.store.AllocateAndCompletePhotoFile(
		ctx, userID, total, "image/jpeg", sanitizeFileName(name), h.maxUserStorageBytes,
		func(file store.File) (store.PhotoDimensions, error) {
			reader := newPartsReader(ctx, h.store, refs, total)
			dimensions, written, assembleErr := h.putAndValidateJPEG(ctx, blob.Key(file.ID), reader, total, checksum)
			reader.stopAndWait()
			if assembleErr != nil {
				if readErr := reader.readError(); errors.Is(readErr, store.ErrUploadPartMissing) {
					missingPartErr = readErr
				}
				return store.PhotoDimensions{}, assembleErr
			}
			if written != total {
				return store.PhotoDimensions{}, fmt.Errorf("wrote %d bytes, expected %d", written, total)
			}
			// validateJPEG caps each dimension at maxPhotoDimension before this conversion.
			return store.PhotoDimensions{Width: int32(dimensions.width), Height: int32(dimensions.height)}, nil //nolint:gosec // G115: dimensions are capped at 10,000 by validateJPEG.
		},
	)
	if errors.Is(err, store.ErrStorageQuota) {
		return store.File{}, errFileQuota
	}
	if errors.Is(err, store.ErrInvalidPhotoDimensions) {
		return store.File{}, errPhotoInvalidDimensions
	}
	if err != nil {
		if verdictErr := photoValidationRPCError(err); verdictErr != nil {
			return store.File{}, verdictErr
		}
		if missingPartErr != nil {
			h.log.Error("assemble photo", "user_id", userID, "file_id", file.ID, "err", err, "part_read_err", missingPartErr)
			return store.File{}, errMediaInvalid
		}
		h.log.Error("assemble photo", "user_id", userID, "file_id", file.ID, "err", err)
		return store.File{}, errInternal
	}
	if _, err = h.store.DeleteUploadParts(ctx, userID, clientFileID); err != nil {
		h.log.Error("delete upload parts", "user_id", userID, "file_id", clientFileID, "err", err)
	}
	return file, nil
}

func photoValidationRPCError(err error) error {
	var validationErr interface{ Verdict() string }
	if !errors.As(err, &validationErr) {
		return nil
	}
	switch validationErr.Verdict() {
	case "MEDIA_INVALID":
		return errMediaInvalid
	case "PHOTO_INVALID_DIMENSIONS":
		return errPhotoInvalidDimensions
	default:
		return nil
	}
}

func (h *handlers) putAndValidateJPEG(
	ctx context.Context, key string, source io.Reader, size int64, checksum string,
) (photoDimensions, int64, error) {
	blobReader, blobWriter := io.Pipe()
	validationReader, validationWriter := io.Pipe()
	type putResult struct {
		written int64
		err     error
	}
	type validationResult struct {
		dimensions photoDimensions
		err        error
	}
	putDone := make(chan putResult, 1)
	validationDone := make(chan validationResult, 1)
	go func() {
		written, err := h.blobs.Put(ctx, key, blobReader)
		if closeErr := blobReader.CloseWithError(err); err == nil && closeErr != nil {
			err = fmt.Errorf("close photo blob reader: %w", closeErr)
		}
		putDone <- putResult{written: written, err: err}
	}()
	go func() {
		dimensions, err := validateJPEG(validationReader, size, checksum)
		if err != nil {
			if _, drainErr := io.Copy(io.Discard, validationReader); drainErr != nil {
				err = errors.Join(err, fmt.Errorf("drain JPEG validation stream: %w", drainErr))
			}
		}
		if closeErr := validationReader.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close JPEG validation reader: %w", closeErr))
		}
		validationDone <- validationResult{dimensions: dimensions, err: err}
	}()

	copied, copyErr := io.Copy(io.MultiWriter(blobWriter, validationWriter), source)
	if copyErr != nil {
		copyErr = errors.Join(copyErr, blobWriter.CloseWithError(copyErr))
		copyErr = errors.Join(copyErr, validationWriter.CloseWithError(copyErr))
	} else {
		if closeErr := blobWriter.Close(); closeErr != nil {
			copyErr = fmt.Errorf("close photo blob stream: %w", closeErr)
		}
		if closeErr := validationWriter.Close(); closeErr != nil {
			copyErr = fmt.Errorf("close JPEG validation stream: %w", closeErr)
		}
	}
	put := <-putDone
	validation := <-validationDone
	if put.err != nil || copyErr != nil {
		return photoDimensions{}, put.written, errors.Join(put.err, copyErr)
	}
	if copied != size || put.written != size {
		return photoDimensions{}, put.written, invalidJPEG()
	}
	if validation.err != nil {
		return photoDimensions{}, put.written, validation.err
	}
	return validation.dimensions, put.written, nil
}
