package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	// minChatTitle is the minimum non-whitespace title length in code points.
	minChatTitle = 1
	// maxChatTitle bounds the title a client may set. chats.title is an unbounded
	// TEXT column and every rename copies the title into one messages row per
	// member, so an unbounded title is a 200x write amplifier. The store
	// deliberately does not validate, so the bound belongs here.
	maxChatTitle = 255
	// maxChatAbout bounds the description a client may set, matching Telegram's
	// basic-group limit.
	maxChatAbout = 255
	// maxChatUsers caps the invite vector before the store sees it. The store
	// bounds its own allocation and returns ErrChatFull regardless, so this is
	// defence in depth — but a Vector<InputUser> can carry ~1.3M ids inside
	// gotd's 16 MB frame, and rejecting here spares even the per-id lookups.
	maxChatUsers = 200
	// maxGetChatIDs bounds getChats' member-selected participant-count read to
	// 100 chats per request.
	maxGetChatIDs = 100
)

// chatTitle validates a client-supplied chat title and returns the trimmed form
// that gets stored. Empty, whitespace-only, over-length and text the server
// cannot store are one error: the client is not owed a distinction it cannot act
// on differently.
func chatTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	titleLength := utf8.RuneCountInString(title)
	if titleLength < minChatTitle || !validText(title) || titleLength > maxChatTitle {
		return "", errChatTitleInvalid
	}
	return title, nil
}

func chatAbout(raw string) (string, error) {
	about := strings.TrimSpace(raw)
	if utf8.RuneCountInString(about) > maxChatAbout {
		return "", errChatAboutTooLong
	}
	if !validText(about) {
		return "", errChatTitleInvalid
	}
	return about, nil
}

// resolveInvitees maps the request's input users to ids, splitting them into
// members to create the chat with and invitees that do not exist. A user id with
// no users row is reported as missing rather than failing the call — Telegram's
// own behaviour — but a malformed input peer fails the whole call, since it is a
// client bug rather than a fact about an account.
func (h *handlers) resolveInvitees(ctx context.Context, users []tg.InputUserClass, selfID int64) (members []int64, missing []tg.MissingInvitee, err error) {
	for _, u := range users {
		id, err := h.inputUserID(u, selfID)
		if err != nil {
			return nil, nil, err
		}
		user, ok, err := h.store.UserByID(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			missing = append(missing, tg.MissingInvitee{UserID: id})
			continue
		}
		if user.Kind == store.UserKindBot {
			return nil, nil, errBotGroupsBlocked
		}
		members = append(members, id)
	}
	return members, missing, nil
}

// addMissingCreatedChatInvitees reports valid invitees that the store filtered
// before inserting the chat. The participant read is after CreateChat commits,
// so the response reflects the durable membership set and keeps blocked users
// out of both the chat and its success vector. The existing MissingInvitee
// shape intentionally remains: a creator with a previously resolved invitee
// can infer a block from this result, which is an accepted create-time residual.
func (h *handlers) addMissingCreatedChatInvitees(ctx context.Context, chatID int64, memberIDs []int64, missing []tg.MissingInvitee) ([]tg.MissingInvitee, error) {
	participants, err := h.store.Participants(ctx, chatID)
	if err != nil {
		return nil, err
	}
	present := make(map[int64]bool, len(participants))
	for _, participant := range participants {
		present[participant.UserID] = true
	}
	reported := make(map[int64]bool, len(missing))
	for _, invitee := range missing {
		reported[invitee.UserID] = true
	}
	for _, id := range memberIDs {
		if !present[id] && !reported[id] {
			missing = append(missing, tg.MissingInvitee{UserID: id})
			reported[id] = true
		}
	}
	return missing, nil
}

// chatUpdate builds the envelope a chat mutation returns to its caller: the
// caller's own copy of the service message, at the caller's own new pts, plus
// the users and the chat the client needs to render it.
//
// perOwner carries every member's new pts and is server-side notification state
// only — never echoed to the caller. Only perOwner[callerID] leaves this
// function, and its key set is used as the member set.
func (h *handlers) chatUpdate(ctx context.Context, callerID int64, chat store.Chat, sender store.Message, perOwner map[int64]int, createUsers []int64) (*tg.Updates, error) {
	files, err := h.loadFiles(ctx, []store.Message{sender})
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]bool, len(perOwner))
	for uid := range perOwner {
		ids[uid] = true
	}
	users, err := h.loadUsers(ctx, ids, callerID)
	if err != nil {
		return nil, err
	}
	wireChat, err := h.chatToTL(ctx, chat, len(perOwner), callerID)
	if err != nil {
		return nil, err
	}
	return &tg.Updates{
		Updates: []tg.UpdateClass{
			&tg.UpdateNewMessage{
				Message:  h.messageToTL(sender, createUsers, files, nil, nil, h.dcID),
				Pts:      perOwner[callerID],
				PtsCount: 1,
			},
		},
		Users: users,
		Chats: []tg.ChatClass{wireChat},
		Date:  int(sender.Date.Unix()),
	}, nil
}

func (h *handlers) currentChatPhoto(ctx context.Context, chat store.Chat) (tg.ChatPhotoClass, tg.PhotoClass, error) {
	if chat.CurrentPhotoFileID == nil {
		return &tg.ChatPhotoEmpty{}, &tg.PhotoEmpty{}, nil
	}
	if chat.CurrentPhotoUploaderID == nil {
		return nil, nil, fmt.Errorf("chat %d current photo has no uploader", chat.ID)
	}
	files, err := h.store.FilesByIDs(ctx, []int64{*chat.CurrentPhotoFileID})
	if err != nil {
		return nil, nil, fmt.Errorf("load chat %d current photo: %w", chat.ID, err)
	}
	file, ok := files[*chat.CurrentPhotoFileID]
	if !ok || file.Kind != store.FileKindPhoto || file.UploaderID != *chat.CurrentPhotoUploaderID {
		return nil, nil, fmt.Errorf("chat %d current photo %d is unavailable", chat.ID, *chat.CurrentPhotoFileID)
	}
	chatPhoto := &tg.ChatPhoto{PhotoID: file.ID, DCID: h.dcID}
	if derivatives := file.PhotoDerivatives; derivatives != nil && len(derivatives.Stripped) > 0 {
		chatPhoto.SetStrippedThumb(derivatives.Stripped)
	}
	return chatPhoto, h.photoToTL(file), nil
}

func (h *handlers) chatToTL(ctx context.Context, chat store.Chat, participantsCount int, selfID int64) (*tg.Chat, error) {
	photo, _, err := h.currentChatPhoto(ctx, chat)
	if err != nil {
		return nil, err
	}
	return chatToTL(chat, participantsCount, selfID, photo), nil
}

// memberIDs returns the fan-out's owners in ascending order, matching the order
// store.Participants reads them so the create action's user list is identical
// whether a client sees it here or replays it through getDifference.
func memberIDs(perOwner map[int64]int) []int64 {
	ids := make([]int64, 0, len(perOwner))
	for uid := range perOwner {
		ids = append(ids, uid)
	}
	slices.Sort(ids)
	return ids
}

// handleCreateChat serves messages.createChat: it creates the chat, announces it
// to every member with a service message, and returns the caller's own update.
//
// The chat and its announcement are two transactions, which is deliberate and is
// the one place a chat mutation is allowed to split them. A half-completed add,
// removal or rename is harmful because the member set and what members were told
// disagree; a chat whose create announcement failed is in nobody's dialog list,
// creator included, so it fails safe. Nothing unwinds the chat row.
func (h *handlers) handleCreateChat(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesCreateChatRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	// Rate limit before any write.
	if err := h.checkRateLimit(r, "create_chat", h.rateLimitCreateChat); err != nil {
		return nil, err
	}
	title, err := chatTitle(req.Title)
	if err != nil {
		return nil, err
	}
	if len(req.Users) > maxChatUsers {
		return nil, errUsersTooMuch
	}
	members, missing, err := h.resolveInvitees(r.Ctx, req.Users, r.UserID)
	if errors.Is(err, errPeerIDInvalid) {
		return nil, errPeerIDInvalid
	}
	if errors.Is(err, errBotGroupsBlocked) {
		return nil, errBotGroupsBlocked
	}
	if err != nil {
		h.log.Error("create chat: resolve invitees", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	chat, err := h.store.CreateChat(r.Ctx, r.UserID, title, members)
	if errors.Is(err, store.ErrChatFull) {
		return nil, errUsersTooMuch
	}
	if err != nil {
		h.log.Error("create chat", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	completionCtx := r.CompletionCtx
	if completionCtx == nil {
		completionCtx = r.Ctx
	}
	missing, err = h.addMissingCreatedChatInvitees(completionCtx, chat.ID, members, missing)
	if err != nil {
		h.log.Error("create chat: inspect participants", "chat_id", chat.ID, "err", err)
		return nil, errInternal
	}

	sender, perOwner, _, err := h.store.SendChatMessage(completionCtx, store.FanOut{
		ChatID: chat.ID, FromID: r.UserID, Text: title, Action: store.ChatActionCreate,
	})
	if err != nil {
		h.log.Error("create chat announce", "chat_id", chat.ID, "err", err)
		return nil, errInternal
	}
	h.notifyOwners(completionCtx, perOwner, 0)

	ups, err := h.chatUpdate(completionCtx, r.UserID, chat, sender, perOwner, memberIDs(perOwner))
	if err != nil {
		h.log.Error("create chat updates", "chat_id", chat.ID, "err", err)
		return nil, errInternal
	}
	// TTLPeriod is accepted and ignored: M6 stores no per-chat message TTL.
	return &tg.MessagesInvitedUsers{Updates: ups, MissingInvitees: missing}, nil
}

// handleDeleteChat serves messages.deleteChat. The store checks creator
// ownership under the chat row lock, removes the current member set, and records
// per-copy delete events in the same commit. Every refused caller gets the same
// non-PEER_ID_INVALID error because the desktop retries that error by adding
// itself to the chat.
func (h *handlers) handleDeleteChat(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesDeleteChatRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}

	deletion, err := h.store.DeleteChat(r.Ctx, req.ChatID, r.UserID)
	if errors.Is(err, store.ErrChatAdminRequired) {
		return nil, errChatAdminRequired
	}
	if err != nil {
		h.log.Error("delete chat", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if len(deletion.OwnerPts) > 0 {
		h.notifyOwners(r.Ctx, deletion.OwnerPts, 0)
	}
	for _, event := range deletion.RemovalEvents {
		h.notifyChatRemoval(r.Ctx, event.OwnerID, event.ChatID)
	}
	return &tg.BoolTrue{}, nil
}

// notifyChatRemoval pushes the PTS-neutral removal event to every replica with
// a live session for its owner after the deletion transaction commits.
func (h *handlers) notifyChatRemoval(ctx context.Context, ownerID, chatID int64) {
	notifyCtx, cancel := senderNotifyContext(ctx)
	defer cancel()
	if err := h.store.Notify(notifyCtx, store.ChannelUpdates, store.ChatRemovalPayload(ownerID, chatID)); err != nil {
		h.log.Error("notify chat removal", "owner_id", ownerID, "chat_id", chatID, "err", err)
	}
}

// handleEditChatTitle serves messages.editChatTitle: one store call renames the
// chat and announces the rename atomically.
//
// The store checks membership and current default rights in the write's
// transaction under the chats row lock. Absent chats and non-members share
// PEER_ID_INVALID; a restricted member receives CHAT_ADMIN_REQUIRED.
func (h *handlers) handleEditChatTitle(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesEditChatTitleRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	title, err := chatTitle(req.Title)
	if err != nil {
		return nil, err
	}

	chat, sender, perOwner, err := h.store.SetChatTitle(r.Ctx, req.ChatID, r.UserID, title)
	if errors.Is(err, store.ErrNotMember) {
		return nil, errPeerIDInvalid
	}
	if errors.Is(err, store.ErrChatAdminRequired) {
		return nil, errChatAdminRequired
	}
	if err != nil {
		h.log.Error("edit chat title", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	h.notifyOwners(r.Ctx, perOwner, 0)

	ups, err := h.chatUpdate(r.Ctx, r.UserID, chat, sender, perOwner, nil)
	if err != nil {
		h.log.Error("edit chat title updates", "chat_id", chat.ID, "err", err)
		return nil, errInternal
	}
	return ups, nil
}

// handleEditChatPhoto assembles a caller-owned uploaded JPEG without holding a
// chat or member lock, then lets the store re-check membership and change_info
// rights at the final mutation serialization point.
func (h *handlers) handleEditChatPhoto(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesEditChatPhotoRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if h.uploadsDisabled {
		if _, ok := req.Photo.(*tg.InputChatUploadedPhoto); ok {
			return nil, errUploadsDisabled
		}
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.ChatID <= 0 {
		return nil, errPeerIDInvalid
	}

	var fileID, clientFileID int64
	var uploadParts int
	var uploadName, uploadChecksum string
	var hasUpload bool
	switch photo := req.Photo.(type) {
	case *tg.InputChatPhotoEmpty:
		fileID = 0
	case *tg.InputChatUploadedPhoto:
		// Only the ordinary file flag is supported. Exact flag equality also
		// rejects unknown future bits instead of silently accepting a mode this
		// server does not implement.
		if photo.Flags != 1 {
			return nil, errInputRequestInvalid
		}
		input, ok := photo.GetFile()
		if !ok || input == nil {
			return nil, errMediaInvalid
		}
		var parseErr error
		clientFileID, uploadParts, uploadName, uploadChecksum, parseErr = inputPhotoFileParts(input)
		if parseErr != nil {
			return nil, parseErr
		}
		if int64(uploadParts) > (maxPhotoJPEGBytes+store.MaxPartBytes-1)/store.MaxPartBytes {
			return nil, errFilePartInvalid
		}
		if uploadName = sanitizeFileName(uploadName); uploadName == "" {
			uploadName = "group.jpg"
		}
		hasUpload = true
	default:
		return nil, errInputRequestInvalid
	}
	if err := h.checkRateLimit(r, "update_profile", h.rateLimitUpdateProfile); err != nil {
		return nil, err
	}
	if err := h.store.CheckChatPhotoEditPermission(r.Ctx, req.ChatID, r.UserID); err != nil {
		switch {
		case errors.Is(err, store.ErrNotMember):
			return nil, errPeerIDInvalid
		case errors.Is(err, store.ErrChatAdminRequired):
			return nil, errChatAdminRequired
		default:
			h.log.Error("check chat photo permission", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
	}
	if hasUpload {
		file, err := h.assemblePhotoFile(r.Ctx, r.UserID, clientFileID, uploadParts, uploadName, uploadChecksum)
		if err != nil {
			return nil, err
		}
		// The stored file id is allocated by the server, not the client upload id.
		fileID = file.ID
	}

	changed, chat, sender, perOwner, err := h.store.SetChatPhoto(r.Ctx, req.ChatID, r.UserID, fileID)
	if errors.Is(err, store.ErrNotMember) {
		return nil, errPeerIDInvalid
	}
	if errors.Is(err, store.ErrChatAdminRequired) {
		return nil, errChatAdminRequired
	}
	if errors.Is(err, store.ErrChatPhotoUnavailable) {
		return nil, errMediaInvalid
	}
	if err != nil {
		h.log.Error("edit chat photo", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !changed {
		return &tg.Updates{Updates: []tg.UpdateClass{}, Date: int(h.now().Unix())}, nil
	}
	h.notifyOwners(r.Ctx, perOwner, 0)
	updates, err := h.chatUpdate(r.Ctx, r.UserID, chat, sender, perOwner, nil)
	if err != nil {
		h.log.Error("edit chat photo updates", "chat_id", req.ChatID, "err", err)
		return nil, errInternal
	}
	return updates, nil
}

// handleEditChatAbout serves messages.editChatAbout for basic groups and channels.
func (h *handlers) handleEditChatAbout(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesEditChatAboutRequest
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
	switch peerType {
	case store.PeerTypeChat:
		if peerID <= 0 {
			return nil, errPeerIDInvalid
		}
		about, err := chatAbout(req.About)
		if err != nil {
			return nil, err
		}
		changed, members, err := h.store.SetChatAbout(r.Ctx, peerID, r.UserID, about)
		switch {
		case errors.Is(err, store.ErrNotMember):
			return nil, errPeerIDInvalid
		case errors.Is(err, store.ErrChatAdminRequired):
			return nil, errChatAdminRequired
		case err != nil:
			h.log.Error("edit chat about", "chat_id", peerID, "user_id", r.UserID, "err", err)
			return nil, errInternal
		case !changed:
			return nil, errChatAboutNotModified
		}
		h.notifyChatAbout(r.Ctx, peerID, members)
	case store.PeerTypeChannel:
		about, err := channelAboutForEdit(req.About)
		if err != nil {
			return nil, err
		}
		if _, err = h.store.SetChannelAbout(r.Ctx, peerID, r.UserID, about); err != nil {
			switch {
			case errors.Is(err, store.ErrNotMember):
				return nil, errPeerIDInvalid
			case errors.Is(err, store.ErrChatAdminRequired):
				return nil, errChatAdminRequired
			case errors.Is(err, store.ErrChannelAboutNotModified):
				return nil, errChatAboutNotModified
			default:
				h.log.Error("edit channel about", "channel_id", peerID, "user_id", r.UserID, "err", err)
				return nil, errInternal
			}
		}
		h.notifyChannelPost(r.Ctx, peerID)
	default:
		return nil, errPeerIDInvalid
	}
	return &tg.BoolTrue{}, nil
}

func (h *handlers) notifyChatAbout(ctx context.Context, chatID int64, members []int64) {
	notifyCtx, cancel := senderNotifyContext(ctx)
	defer cancel()
	for _, userID := range members {
		if err := h.store.Notify(notifyCtx, store.ChannelUpdates, store.ChatAboutPayload(userID, chatID)); err != nil {
			h.log.Error("notify chat about", "chat_id", chatID, "user_id", userID, "err", err)
		}
	}
}

// handleEditChatDefaultBannedRights saves basic-group or megagroup default
// restrictions. The store re-checks current membership and authority under the
// peer row lock before it compares values or writes. Rights updates have no
// message event or owner pts, so the reply carries Telegram's peer-version
// update alone.
func (h *handlers) handleEditChatDefaultBannedRights(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesEditChatDefaultBannedRightsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	var (
		chatID      int64
		channelPeer *tg.InputPeerChannel
	)
	switch peer := req.Peer.(type) {
	case *tg.InputPeerChat:
		if peer.ChatID <= 0 {
			return nil, errPeerIDInvalid
		}
		chatID = peer.ChatID
	case *tg.InputPeerChannel:
		if peer.ChannelID <= 0 {
			return nil, errPeerIDInvalid
		}
		channelPeer = peer
	default:
		return nil, errPeerIDInvalid
	}
	rights, err := chatDefaultBannedRightsFromTL(req.BannedRights)
	if err != nil {
		return nil, err
	}

	if channelPeer != nil {
		peerType, channelID, err := h.inputPeer(channelPeer, r.UserID)
		if err != nil {
			return nil, err
		}
		if peerType != store.PeerTypeChannel {
			return nil, errPeerIDInvalid
		}
		channel, changed, err := h.store.SetChannelDefaultBannedRights(r.Ctx, channelID, r.UserID, rights)
		if errors.Is(err, store.ErrNotMember) {
			return nil, errPeerIDInvalid
		}
		if err != nil {
			h.log.Error("edit channel default banned rights", "channel_id", channelID, "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
		if !changed {
			return nil, errChatNotModified
		}
		return &tg.Updates{
			Updates: []tg.UpdateClass{&tg.UpdateChatDefaultBannedRights{
				Peer:                &tg.PeerChannel{ChannelID: channel.ID},
				DefaultBannedRights: chatDefaultBannedRightsToTL(channel.DefaultBannedRights),
				Version:             channel.Version,
			}},
			Date: int(h.now().Unix()),
		}, nil
	}

	chat, changed, err := h.store.SetChatDefaultBannedRights(r.Ctx, chatID, r.UserID, rights)
	if errors.Is(err, store.ErrNotMember) {
		return nil, errPeerIDInvalid
	}
	if err != nil {
		h.log.Error("edit chat default banned rights", "chat_id", chatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if !changed {
		return nil, errChatNotModified
	}

	return &tg.Updates{
		Updates: []tg.UpdateClass{&tg.UpdateChatDefaultBannedRights{
			Peer:                &tg.PeerChat{ChatID: chat.ID},
			DefaultBannedRights: chatDefaultBannedRightsToTL(chat.DefaultBannedRights),
			Version:             chat.Version,
		}},
		Date: int(h.now().Unix()),
	}, nil
}

func (h *handlers) handleGetFullChat(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetFullChatRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.ChatID <= 0 {
		return nil, errPeerIDInvalid
	}

	snapshot, err := h.store.ChatInfoForMemberSnapshot(r.Ctx, r.UserID, []int64{req.ChatID})
	if err != nil {
		h.log.Error("get full chat", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	chat, ok := snapshot.Chats[req.ChatID]
	if !ok {
		return nil, errPeerIDInvalid
	}
	participants := snapshot.Participants[req.ChatID]
	notifySettings, err := h.notifySettingsForPeer(r.Ctx, r.UserID, store.PeerTypeChat, chat.ID, store.NotifySettingsScopeChats)
	if err != nil {
		h.log.Error("get full chat notify settings", "chat_id", chat.ID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	wireFull := &tg.ChatFull{
		ID:             chat.ID,
		About:          snapshot.Abouts[chat.ID],
		Participants:   chatParticipantsToTL(chat, participants),
		NotifySettings: notifySettings,
	}
	availableReactions := store.ChatAvailableReactions{Mode: store.ChatAvailableReactionsAll}
	if stored, ok := snapshot.AvailableReactions[chat.ID]; ok {
		availableReactions = stored
	}
	wireReactions, err := chatAvailableReactionsToTL(availableReactions)
	if err != nil {
		h.log.Error("get full chat available reactions", "chat_id", chat.ID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	wireFull.SetAvailableReactions(wireReactions)
	wireFull.SetTTLPeriod(chat.TTLPeriod)
	chatPhoto, fullPhoto, err := h.currentChatPhoto(r.Ctx, chat)
	if err != nil {
		h.log.Error("get full chat photo", "chat_id", chat.ID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	wireFull.SetChatPhoto(fullPhoto)
	pinnedMessageID, hasPinnedMessage, err := h.store.ChatPinnedMessageForOwner(r.Ctx, chat.ID, r.UserID)
	if err != nil {
		h.log.Error("get full chat pin", "chat_id", chat.ID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	if hasPinnedMessage {
		wireFull.SetPinnedMsgID(int(pinnedMessageID))
	}
	users, err := h.chatInfoUsers(r.Ctx, snapshot, r.UserID)
	if err != nil {
		h.log.Error("get full chat users", "chat_id", req.ChatID, "user_id", r.UserID, "err", err)
		return nil, errInternal
	}

	wireChat := chatToTL(chat, len(participants), r.UserID, chatPhoto)
	return &tg.MessagesChatFull{
		FullChat: wireFull,
		Chats:    []tg.ChatClass{wireChat},
		Users:    users,
	}, nil
}

func (h *handlers) handleGetChats(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetChatsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if len(req.ID) > maxGetChatIDs {
		return nil, errLimitInvalid
	}

	chatIDs := make([]int64, 0, len(req.ID))
	seen := make(map[int64]bool, len(req.ID))
	for _, id := range req.ID {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		chatIDs = append(chatIDs, id)
	}

	snapshot, err := h.store.ChatListInfoForMemberSnapshot(r.Ctx, r.UserID, chatIDs)
	if err != nil {
		h.log.Error("get chats", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	chats := make([]tg.ChatClass, 0, len(chatIDs))
	for _, id := range chatIDs {
		chat, ok := snapshot.Chats[id]
		if !ok {
			chats = append(chats, &tg.ChatForbidden{ID: id, Title: ""})
			continue
		}
		wireChat, err := h.chatToTL(r.Ctx, chat, int(snapshot.ParticipantCounts[id]), r.UserID)
		if err != nil {
			h.log.Error("get chats photo", "chat_id", id, "user_id", r.UserID, "err", err)
			return nil, errInternal
		}
		chats = append(chats, wireChat)
	}
	return &tg.MessagesChats{Chats: chats}, nil
}

func chatParticipantsToTL(chat store.Chat, participants []store.Participant) *tg.ChatParticipants {
	wireParticipants := make([]tg.ChatParticipantClass, 0, len(participants))
	for _, participant := range participants {
		if participant.UserID == chat.CreatorID {
			wireParticipants = append(wireParticipants, &tg.ChatParticipantCreator{UserID: participant.UserID})
			continue
		}
		if participant.Admin {
			wireParticipants = append(wireParticipants, &tg.ChatParticipantAdmin{
				UserID:    participant.UserID,
				InviterID: participant.InviterID,
				Date:      int(participant.Date.Unix()),
			})
			continue
		}
		wireParticipants = append(wireParticipants, &tg.ChatParticipant{
			UserID:    participant.UserID,
			InviterID: participant.InviterID,
			Date:      int(participant.Date.Unix()),
		})
	}
	return &tg.ChatParticipants{ChatID: chat.ID, Participants: wireParticipants, Version: chat.Version}
}

func (h *handlers) chatInfoUsers(ctx context.Context, snapshot store.ChatInfoSnapshot, viewerID int64) ([]tg.UserClass, error) {
	ids := make([]int64, 0, len(snapshot.Users))
	for id := range snapshot.Users {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	contactIDs := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id == viewerID || snapshot.EntitledUsers[id] {
			contactIDs = append(contactIDs, id)
		}
	}
	contactStates, err := h.contactStatesForUsers(ctx, viewerID, contactIDs)
	if err != nil {
		return nil, err
	}
	visibleUsers := make([]store.User, 0, len(contactIDs))
	for _, id := range contactIDs {
		visibleUsers = append(visibleUsers, snapshot.Users[id])
	}
	visibility, err := h.lastSeenVisibilityForUsers(ctx, visibleUsers, viewerID)
	if err != nil {
		return nil, err
	}
	users := make([]tg.UserClass, 0, len(ids))
	for _, id := range ids {
		user := snapshot.Users[id]
		if id != viewerID && !snapshot.EntitledUsers[id] {
			users = append(users, &tg.UserEmpty{ID: id})
			continue
		}
		users = append(users, h.userToTL(user, viewerID, id == viewerID, contactStates[id], visibility[id]))
	}
	return users, nil
}
