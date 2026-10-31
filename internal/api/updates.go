package api

import (
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/store"
)

// replySnippet returns a short preview of the quoted message text, truncated
// to Telegram snippet width. Uses word boundaries where possible.
func replySnippet(s string) string {
	if len(s) <= 25 {
		return s
	}
	trunc := s[:25]
	if r, size := utf8.DecodeLastRuneInString(trunc); r == utf8.RuneError && size == 1 {
		// single byte of a multi-byte rune—drop it
		trunc = trunc[:len(trunc)-1]
	} else if r == utf8.RuneError {
		// multi-byte rune started inside trunc; remove it entirely
		trunc = trunc[:len(trunc)-size]
	}
	return trunc
}

// messageMedia keeps each message reference namespace separate. FileID names a
// stored file, while StickerDocumentID names a user-set or catalogue sticker;
// the same positive id can be present in both namespaces.
type messageMedia struct {
	files        map[int64]tg.MessageMediaClass
	userStickers map[int64]tg.MessageMediaClass
}

// messageToTL maps a stored message to the wire message. The peer follows
// peer_type; from is always PeerUser, since the author of a chat message is a
// user either way. ids are cast to the wire int space. EditDate populates its
// flag via tg.Message.SetFlags at encode time.
//
// A row carrying an action renders as tg.MessageService instead. createUsers is
// the participant list for a ChatActionCreate row and nil for everything else:
// the mapper stays pure, so the one action that needs a member set is handed it
// rather than fetching it. media carries separate file and sticker maps so
// overlapping ids cannot change which reference a row renders.
// reactions, when non-nil, populates the message's Reactions field.
func messageToTL(m store.Message, createUsers []int64, media *messageMedia, replyTexts map[int32]string, reactions []store.Reaction, dcIDs ...int) tg.MessageClass {
	return messageToTLWithStickerCatalog(m, createUsers, media, replyTexts, reactions, nil, dcIDs...)
}

func messageToTLWithStickerCatalog(m store.Message, createUsers []int64, media *messageMedia, replyTexts map[int32]string, reactions []store.Reaction, stickerSets *stickerSetCatalog, dcIDs ...int) tg.MessageClass {
	var files map[int64]tg.MessageMediaClass
	var userStickers map[int64]tg.MessageMediaClass
	if media != nil {
		files = media.files
		userStickers = media.userStickers
	}
	if m.Action != store.ChatActionNone {
		return &tg.MessageService{
			ID:     int(m.LocalID),
			Out:    m.Out,
			PeerID: peerToTL(m.PeerType, m.PeerID),
			FromID: &tg.PeerUser{UserID: m.FromID},
			Date:   int(m.Date.Unix()),
			Action: actionToTL(m, createUsers, files),
		}
	}
	msg := &tg.Message{
		ID:      int(m.LocalID),
		Out:     m.Out,
		PeerID:  peerToTL(m.PeerType, m.PeerID),
		FromID:  &tg.PeerUser{UserID: m.FromID},
		Message: m.Text,
		Date:    int(m.Date.Unix()),
	}
	if entities := decodeMessageEntities(store.DecodeMessageEntities([]byte(m.Entities))); len(entities) > 0 {
		msg.SetEntities(entities)
	}
	if m.EditDate != nil {
		msg.EditDate = int(m.EditDate.Unix())
	}
	if m.TTLPeriod > 0 {
		msg.SetTTLPeriod(m.TTLPeriod)
	}
	if m.GroupedID != nil {
		msg.SetGroupedID(*m.GroupedID)
	}
	// ReplyToTrusted is set only after atomic validation. Legacy positive ids
	// stay hidden because old rows have no validated provenance.
	if m.ReplyToMsgID > 0 && m.ReplyToTrusted {
		hdr := new(tg.MessageReplyHeader)
		hdr.SetReplyToMsgID(int(m.ReplyToMsgID))
		hdr.SetReplyToPeerID(peerToTL(m.PeerType, m.PeerID))
		if txt := replyTexts[m.ReplyToMsgID]; txt != "" {
			hdr.SetQuoteText(replySnippet(txt))
		}
		msg.SetReplyTo(hdr)
	}
	// SetMedia rather than a plain assignment: Media is a conditional field and
	// encodes only when its flag is set with it.
	if m.StickerDocumentID != nil {
		dcID := 1
		if len(dcIDs) > 0 {
			dcID = dcIDs[0]
		}
		if sticker, ok := userStickers[*m.StickerDocumentID]; ok {
			msg.SetMedia(sticker)
		} else if media, ok := stickerDocumentMedia(stickerSets, *m.StickerDocumentID, dcID); ok {
			msg.SetMedia(media)
		} else {
			msg.SetMedia(&tg.MessageMediaUnsupported{})
		}
	} else if media, ok := files[m.FileID]; ok && m.FileID != 0 {
		msg.SetMedia(media)
	}
	if m.FwdFromID != 0 || !m.FwdDate.IsZero() {
		fwd := tg.MessageFwdHeader{
			Date: int(m.FwdDate.Unix()),
		}
		if m.FwdChannelID != 0 {
			// Channel source: FromID is the channel, not a user.
			fwd.SetFromID(&tg.PeerChannel{ChannelID: m.FwdChannelID})
			fwd.SetChannelPost(int(m.FwdChannelPost))
			if m.FwdPostAuthor != nil {
				fwd.SetPostAuthor(*m.FwdPostAuthor)
			}
		} else if m.FwdFromID != 0 {
			fwd.SetFromID(&tg.PeerUser{UserID: m.FwdFromID})
		}
		msg.SetFwdFrom(fwd)
	}
	if len(reactions) > 0 {
		msg.SetReactions(reactionsToTL(reactions, m.OwnerID, m.PeerType == store.PeerTypeChat))
	}
	return msg
}

func (h *handlers) messageToTL(m store.Message, createUsers []int64, media *messageMedia, replyTexts map[int32]string, reactions []store.Reaction, dcIDs ...int) tg.MessageClass {
	return messageToTLWithStickerCatalog(m, createUsers, media, replyTexts, reactions, h.stickerSets, dcIDs...)
}

// reactionsToTL is the single wire projection of stored reactions, shared by
// every surface that renders them. It aggregates each emoji and marks the
// viewer's reaction as chosen.
//
// It does not disclose other reactor identities through RecentReactions or a
// user list.
func reactionsToTL(reactions []store.Reaction, viewerID int64, canSeeList bool) tg.MessageReactions {
	mr := tg.MessageReactions{
		Results: make([]tg.ReactionCount, 0, len(reactions)),
	}
	if canSeeList {
		mr.SetCanSeeList(true)
	}
	resultByEmoji := make(map[string]int, len(reactions))
	for _, r := range reactions {
		if index, ok := resultByEmoji[r.Reaction]; ok {
			result := &mr.Results[index]
			result.Count++
			if r.ReactorID == viewerID {
				result.SetChosenOrder(1)
			}
			continue
		}
		result := tg.ReactionCount{Reaction: &tg.ReactionEmoji{Emoticon: r.Reaction}, Count: 1}
		if r.ReactorID == viewerID {
			result.SetChosenOrder(1)
		}
		resultByEmoji[r.Reaction] = len(mr.Results)
		mr.Results = append(mr.Results, result)
	}
	return mr
}

func channelReactionsToTL(reactions []store.ChannelReactionCount, canSeeList bool) tg.MessageReactions {
	mr := tg.MessageReactions{Results: make([]tg.ReactionCount, len(reactions))}
	if canSeeList {
		mr.SetCanSeeList(true)
	}
	for i, reaction := range reactions {
		result := tg.ReactionCount{
			Reaction: &tg.ReactionEmoji{Emoticon: reaction.Reaction},
			Count:    reaction.Count,
		}
		if reaction.Chosen {
			result.SetChosenOrder(1)
		}
		mr.Results[i] = result
	}
	return mr
}

// channelMessageToTL maps a stored channel message to the wire message, the
// channel counterpart of messageToTL. A channel keeps one row per message
// rather than one per member, so Out is derived from the viewer here instead of
// being read off the row, and the peer is always the channel.
//
// media uses separate file and sticker maps like messageToTL. The "no media"
// sentinel differs and the trap is worth naming: channel_messages.file_id is
// NULL for no media, while messages.file_id is 0.
func channelMessageToTL(m store.ChannelMessage, viewerID int64, media *messageMedia, broadcast bool, dcIDs ...int) (tg.MessageClass, error) {
	return channelMessageToTLWithStickerCatalog(m, viewerID, media, broadcast, nil, dcIDs...)
}

func channelMessageToTLWithStickerCatalog(m store.ChannelMessage, viewerID int64, media *messageMedia, broadcast bool, stickerSets *stickerSetCatalog, dcIDs ...int) (tg.MessageClass, error) {
	var files map[int64]tg.MessageMediaClass
	var userStickers map[int64]tg.MessageMediaClass
	if media != nil {
		files = media.files
		userStickers = media.userStickers
	}
	if m.Deleted {
		return &tg.MessageEmpty{ID: int(m.LocalID)}, nil
	}
	var action tg.MessageActionClass
	switch m.Action {
	case store.ChannelMessageActionCreate:
		action = &tg.MessageActionChannelCreate{Title: m.Message}
	case store.ChannelMessageActionEditTitle:
		action = &tg.MessageActionChatEditTitle{Title: m.Message}
	case store.ChannelMessageActionSetMessagesTTL:
		action = &tg.MessageActionSetMessagesTTL{Period: m.TTLPeriod}
	case store.ChannelMessageActionEditPhoto:
		if m.FileID == nil {
			return nil, fmt.Errorf("channel photo service message %d has no file", m.LocalID)
		}
		file, ok := files[*m.FileID]
		if !ok {
			return nil, fmt.Errorf("channel photo service message %d file %d is unavailable", m.LocalID, *m.FileID)
		}
		photo, ok := file.(*tg.MessageMediaPhoto)
		if !ok || photo.Photo == nil {
			return nil, fmt.Errorf("channel photo service message %d file %d is not a photo", m.LocalID, *m.FileID)
		}
		action = &tg.MessageActionChatEditPhoto{Photo: photo.Photo}
	case store.ChannelMessageActionDeletePhoto:
		action = &tg.MessageActionChatDeletePhoto{}
	}
	if action != nil {
		service := &tg.MessageService{
			ID:     int(m.LocalID),
			Out:    m.FromID == viewerID,
			PeerID: &tg.PeerChannel{ChannelID: m.ChannelID},
			Date:   int(m.Date.Unix()),
			Action: action,
		}
		if broadcast {
			service.SetPost(true)
		} else {
			service.SetFromID(&tg.PeerUser{UserID: m.FromID})
		}
		return service, nil
	}
	msg := &tg.Message{
		ID:      int(m.LocalID),
		Out:     m.FromID == viewerID,
		PeerID:  &tg.PeerChannel{ChannelID: m.ChannelID},
		Message: m.Message,
		Date:    int(m.Date.Unix()),
	}
	if entities := decodeMessageEntities(store.DecodeMessageEntities([]byte(m.Entities))); len(entities) > 0 {
		msg.SetEntities(entities)
	}
	if broadcast {
		msg.SetPost(true)
		if m.PostAuthor != nil {
			msg.SetPostAuthor(*m.PostAuthor)
		}
	} else {
		msg.SetFromID(&tg.PeerUser{UserID: m.FromID})
	}
	if m.EditDate != nil {
		msg.EditDate = int(m.EditDate.Unix())
	}
	if m.TTLPeriod > 0 {
		msg.SetTTLPeriod(m.TTLPeriod)
	}
	if m.GroupedID != nil {
		msg.SetGroupedID(*m.GroupedID)
	}
	if m.ReplyToMsgID > 0 {
		hdr := new(tg.MessageReplyHeader)
		hdr.SetReplyToMsgID(int(m.ReplyToMsgID))
		hdr.SetReplyToPeerID(&tg.PeerChannel{ChannelID: m.ChannelID})
		msg.SetReplyTo(hdr)
	}
	// SetMedia rather than a plain assignment: Media is a conditional field and
	// encodes only when its flag is set with it.
	if m.StickerDocumentID != nil {
		dcID := 1
		if len(dcIDs) > 0 {
			dcID = dcIDs[0]
		}
		if sticker, ok := userStickers[*m.StickerDocumentID]; ok {
			msg.SetMedia(sticker)
		} else if media, ok := stickerDocumentMedia(stickerSets, *m.StickerDocumentID, dcID); ok {
			msg.SetMedia(media)
		} else {
			msg.SetMedia(&tg.MessageMediaUnsupported{})
		}
	} else if m.FileID != nil {
		if media, ok := files[*m.FileID]; ok {
			msg.SetMedia(media)
		}
	}
	if m.Poll != nil {
		entities := decodeMessageEntities(m.Poll.DescriptionEntities)
		if len(entities) > 0 {
			msg.SetEntities(entities)
		}
		msg.SetMedia(&tg.MessageMediaPoll{
			Poll:    pollToTL(*m.Poll),
			Results: pollResultsToTL(*m.Poll),
		})
	}
	return msg, nil
}

func (h *handlers) channelMessageToTL(m store.ChannelMessage, viewerID int64, media *messageMedia, broadcast bool, dcIDs ...int) (tg.MessageClass, error) {
	return channelMessageToTLWithStickerCatalog(m, viewerID, media, broadcast, h.stickerSets, dcIDs...)
}

// documentToTL names a stored file on the wire. It echoes only the supported
// filename and animated marker; M5 does not decode document bodies, so it
// cannot honestly claim an image size or duration it did not measure.
//
// FileReference is the 8-byte big-endian file id — a placeholder, the same
// posture as the peer access_hash. It is echoed deterministically and ignored
// entirely on input. Half-validating it would make it an oracle; ignoring it
// does not.
func (h *handlers) documentToTL(f store.File) *tg.Document {
	d := &tg.Document{
		ID:            f.ID,
		AccessHash:    f.AccessHash,
		FileReference: binary.BigEndian.AppendUint64(nil, uint64(f.ID)), //nolint:gosec // G115: opaque 64-bit id, sign irrelevant
		Date:          int(f.Date.Unix()),
		MimeType:      f.MimeType,
		Size:          f.Size,
		DCID:          h.dcID,
	}
	if slices.Contains(f.SubtypeRights, "send_gifs") {
		d.Attributes = append(d.Attributes, &tg.DocumentAttributeAnimated{})
	}
	if f.FileName != "" {
		d.Attributes = append(d.Attributes, &tg.DocumentAttributeFilename{FileName: f.FileName})
	}
	return d
}

func (h *handlers) fileMediaToTL(f store.File) tg.MessageMediaClass {
	if f.Kind == store.FileKindPhoto {
		return &tg.MessageMediaPhoto{Photo: h.photoToTL(f)}
	}
	return &tg.MessageMediaDocument{Document: h.documentToTL(f)}
}

func (h *handlers) photoToTL(f store.File) *tg.Photo {
	sizes := make([]tg.PhotoSizeClass, 0, 3)
	if derivatives := f.PhotoDerivatives; derivatives != nil {
		sizes = append(sizes, &tg.PhotoStrippedSize{Type: "i", Bytes: derivatives.Stripped})
		if derivatives.MSize > 0 {
			sizes = append(sizes, &tg.PhotoSize{
				Type: "m",
				W:    derivatives.MWidth,
				H:    derivatives.MHeight,
				Size: derivatives.MSize,
			})
		}
	}
	sizes = append(sizes, &tg.PhotoSize{
		Type: photoSizeType(f.Width, f.Height),
		W:    f.Width,
		H:    f.Height,
		Size: int(f.Size),
	})
	return &tg.Photo{
		ID:            f.ID,
		AccessHash:    f.AccessHash,
		FileReference: binary.BigEndian.AppendUint64(nil, uint64(f.ID)), //nolint:gosec // G115: opaque 64-bit id, sign irrelevant
		Date:          int(f.Date.Unix()),
		Sizes:         sizes,
		DCID:          h.dcID,
	}
}

func photoSizeType(width, height int) string {
	longSide := max(width, height)
	switch {
	case longSide <= 800:
		return "x"
	case longSide <= 1280:
		return "y"
	default:
		return "w"
	}
}

// actionToTL maps a service message's action. Create and EditTitle carry the
// title in the message text; Add/DeleteUser carry their subject in action_user_id.
func actionToTL(m store.Message, createUsers []int64, files map[int64]tg.MessageMediaClass) tg.MessageActionClass {
	switch m.Action {
	case store.ChatActionCreate:
		return &tg.MessageActionChatCreate{Title: m.Text, Users: createUsers}
	case store.ChatActionAddUser:
		return &tg.MessageActionChatAddUser{Users: []int64{m.ActionUserID}}
	case store.ChatActionDeleteUser:
		return &tg.MessageActionChatDeleteUser{UserID: m.ActionUserID}
	case store.ChatActionEditTitle:
		return &tg.MessageActionChatEditTitle{Title: m.Text}
	case store.ChatActionSetMessagesTTL:
		return &tg.MessageActionSetMessagesTTL{Period: m.TTLPeriod}
	case store.ChatActionEditPhoto:
		if media, ok := files[m.FileID].(*tg.MessageMediaPhoto); ok {
			return &tg.MessageActionChatEditPhoto{Photo: media.Photo}
		}
	case store.ChatActionDeletePhoto:
		return &tg.MessageActionChatDeletePhoto{}
	default:
	}
	return &tg.MessageActionEmpty{}
}

// chatToTL maps a stored chat to the wire tg.Chat. selfID marks the creator flag
// for the recipient of this batch. ParticipantsCount comes from the caller
// because the mapper stays pure.
//
// Deactivated is left false: chats has no such column and store.Chat no longer
// carries the field, so false is the only honest answer until one has a reader.
//
// Photo is mandatory on the wire, not optional: (*tg.Chat).EncodeBare fails the
// whole reply when it is nil, so a chat with no photo must say so explicitly.
//
// DefaultBannedRights is always set, empty when nothing is restricted: clients
// read a chat without it as granting a non-creator no rights at all.
func chatToTL(c store.Chat, participantsCount int, selfID int64, photo tg.ChatPhotoClass) *tg.Chat {
	chat := &tg.Chat{
		ID:                c.ID,
		Title:             c.Title,
		Creator:           c.CreatorID == selfID,
		ParticipantsCount: participantsCount,
		Date:              int(c.Date.Unix()),
		Version:           c.Version,
		Photo:             photo,
	}
	chat.SetDefaultBannedRights(chatDefaultBannedRightsToTL(c.DefaultBannedRights))
	return chat
}

// channelToTL maps a stored channel to the wire. member says whether the viewer
// is currently entitled to the channel's metadata; a non-member and a banned
// member are the same answer, since a ban that still served the live title would
// be cosmetic.
//
// AccessHash is derived for (viewerID, c.ID) so only the viewer can use it.
// The forbidden form carries an empty title on purpose, the rule M6 settled for
// ChatForbidden: the row still changes after someone leaves, but the client is
// no longer entitled to its live metadata.
//
// The member view includes a username only for a public channel. Participant
// counts and admin rights are rendered by their dedicated RPCs.
// store.Channel.Version has no wire counterpart either: unlike tg.Chat, the
// current tg.Channel schema carries no version field.
//
// Photo is chatPhotoEmpty rather than left nil, the same as chatToTL: the field
// is mandatory in the channel constructor, so a nil one encodes to an error in
// Conn.SendResult and takes down every reply carrying a channel — after the
// mutation has committed. "M7 stores no photo" is said with the empty
// constructor, not by omitting the field.
//
// A megagroup always carries DefaultBannedRights, empty when nothing is
// restricted, for the same reason chatToTL does.
func (h *handlers) channelToTL(c store.Channel, m store.ChannelMember, member bool, viewerID int64) tg.ChatClass {
	ah := h.peers.Derive(viewerID, peerhash.KindChannel, c.ID)
	if !member || c.DeletedAt != nil {
		return &tg.ChannelForbidden{ID: c.ID, AccessHash: ah, Title: ""}
	}
	var chatPhoto tg.ChatPhotoClass = &tg.ChatPhotoEmpty{}
	if c.CurrentPhoto != nil {
		photo := &tg.ChatPhoto{PhotoID: c.CurrentPhoto.ID, DCID: h.dcID}
		if derivatives := c.CurrentPhoto.PhotoDerivatives; derivatives != nil && len(derivatives.Stripped) > 0 {
			photo.SetStrippedThumb(derivatives.Stripped)
		}
		chatPhoto = photo
	}
	ch := &tg.Channel{
		ID:         c.ID,
		Title:      c.Title,
		AccessHash: ah,
		Date:       int(c.Date.Unix()),
		Megagroup:  c.Megagroup,
		Broadcast:  !c.Megagroup,
		Creator:    m.Role == 2,
		Left:       false,
		Photo:      chatPhoto,
	}
	if c.Username != nil {
		ch.Username = *c.Username
	}
	ch.SetSignatures(c.Signatures)
	if c.Megagroup {
		ch.SetDefaultBannedRights(chatDefaultBannedRightsToTL(c.DefaultBannedRights))
	}
	return ch
}

// contactStatesForUsers loads the viewer's contact edges for the selected
// users. The result is owner-scoped and includes reciprocal state from the
// same query as each edge.
func (h *handlers) contactStatesForUsers(ctx context.Context, viewerID int64, userIDs []int64) (map[int64]store.Contact, error) {
	states := make(map[int64]store.Contact, len(userIDs))
	if viewerID <= 0 || len(userIDs) == 0 {
		return states, nil
	}

	selected := make([]int64, 0, len(userIDs))
	seen := make(map[int64]bool, len(userIDs))
	for _, userID := range userIDs {
		if userID <= 0 || userID == viewerID || seen[userID] {
			continue
		}
		seen[userID] = true
		selected = append(selected, userID)
	}
	if len(selected) == 0 {
		return states, nil
	}
	contacts, err := h.store.ContactStates(ctx, viewerID, selected)
	if err != nil {
		return nil, err
	}
	for _, contact := range contacts {
		states[contact.UserID] = contact
	}
	return states, nil
}

// usersToTL renders a group of users with live contact state for viewerID.
func (h *handlers) usersToTL(ctx context.Context, users []store.User, viewerID int64, markSelf bool) ([]*tg.User, error) {
	userIDs := make([]int64, len(users))
	for i, user := range users {
		userIDs[i] = user.ID
	}
	states, err := h.contactStatesForUsers(ctx, viewerID, userIDs)
	if err != nil {
		return nil, err
	}
	visibility, err := h.lastSeenVisibilityForUsers(ctx, users, viewerID)
	if err != nil {
		return nil, err
	}
	out := make([]*tg.User, len(users))
	for i, user := range users {
		out[i] = h.userToTL(user, viewerID, markSelf && user.ID == viewerID, states[user.ID], visibility[user.ID])
	}
	if err := h.attachCurrentProfilePhotos(ctx, viewerID, users, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (h *handlers) lastSeenVisibilityForUsers(ctx context.Context, users []store.User, viewerID int64) (map[int64]store.LastSeenVisibility, error) {
	return h.store.EvaluateLastSeenPrivacyForViewer(ctx, viewerID, users)
}

// userToTL maps a stored user to the wire tg.User. AccessHash is derived for
// (viewerID, u.ID) so only the viewer can use it. self marks the update
// recipient's own account. Contact flags come from the viewer's live directed
// edge. The phone number is private to its owner, so it is emitted only on the
// self entry — names stay for every peer, since a client needs them to render
// a conversation.
func (h *handlers) userToTL(u store.User, viewerID int64, self bool, contact store.Contact, visibility store.LastSeenVisibility) *tg.User {
	tlUser := &tg.User{
		ID:            u.ID,
		Self:          self,
		Contact:       !self && contact.UserID == u.ID,
		MutualContact: !self && contact.UserID == u.ID && contact.Mutual,
		FirstName:     u.FirstName,
		LastName:      u.LastName,
		AccessHash:    h.peers.Derive(viewerID, peerhash.KindUser, u.ID),
		Status:        userStatusToTL(u, self, visibility),
	}
	if u.Kind == store.UserKindBot {
		tlUser.SetBot(true)
		tlUser.SetBotNochats(true)
		tlUser.SetBotInfoVersion(1)
	}
	if self {
		tlUser.Phone = u.Phone
	}
	if u.Username != nil {
		tlUser.Username = *u.Username
	}
	return tlUser
}

// userStatusToTL maps a store.User's presence fields to the wire status.
// Self always gets UserStatusRecently — Telegram's canonical sentinel for
// "this is your own account; your last-seen is not disclosed to yourself."
func userStatusToTL(u store.User, self bool, visibility store.LastSeenVisibility) tg.UserStatusClass {
	if self {
		return &tg.UserStatusRecently{}
	}
	switch visibility.Level {
	case store.LastSeenAccessHidden:
		return &tg.UserStatusEmpty{}
	case store.LastSeenAccessCoarse:
		switch visibility.Bucket {
		case store.LastSeenBucketRecently:
			return &tg.UserStatusRecently{ByMe: visibility.ByMe}
		case store.LastSeenBucketLastWeek:
			return &tg.UserStatusLastWeek{ByMe: visibility.ByMe}
		case store.LastSeenBucketLastMonth:
			return &tg.UserStatusLastMonth{ByMe: visibility.ByMe}
		default:
			return &tg.UserStatusEmpty{}
		}
	case store.LastSeenAccessExact:
		// Exact status is the only branch allowed to expose the online bit or
		// last-seen timestamp below.
	default:
		return &tg.UserStatusEmpty{}
	}
	if u.IsOnline {
		return &tg.UserStatusOnline{Expires: int(time.Now().Add(5 * time.Minute).Unix())}
	}
	if u.LastSeenAt != nil {
		return &tg.UserStatusOffline{WasOnline: int(u.LastSeenAt.Unix())}
	}
	return &tg.UserStatusEmpty{}
}

func stateToTL(s store.State) *tg.UpdatesState {
	return &tg.UpdatesState{
		Pts:         s.Pts,
		Qts:         s.Qts,
		Date:        s.Date,
		Seq:         s.Seq,
		UnreadCount: s.UnreadCount,
	}
}

// maxDiffEvents caps the events hydrated into one difference/push, bounding the
// work a stale client can force. A truncated batch is returned as a slice with
// an intermediate state so the client re-requests the remainder.
const maxDiffEvents = 500

// chatRemovalStateDateTokenFloor keeps PTS-neutral removal confirmations out
// of the wall-clock range returned by updates.getState. The unique Date token
// and non-regressing PTS/QTS together confirm a difference response.
const chatRemovalStateDateTokenFloor = 2_000_000_000

// updateBatch is one user's hydrated updates plus everything a client must be
// told alongside them. pts[i] is the pts of ups[i], ascending, so one batch can
// serve several of the user's connections without re-querying per connection.
//
// more reports that the batch hit the maxDiffEvents cap and state is an
// intermediate one the client must re-request from.
type updateBatch struct {
	ups   []tg.UpdateClass
	pts   []int
	users []tg.UserClass
	chats []tg.ChatClass
	state store.State
	// head is the user's pts as read for this batch, before any truncation
	// clamp on state. It is what a reader within maxDiffEvents of it can be
	// brought up to by a single further window.
	head int
	more bool
}

// above returns the updates whose pts exceeds fromPts. Events are ordered, so
// the answer is a suffix: a reader already served up to fromPts gets exactly
// the gap between it and the batch, with no duplicate and no hole.
func (b updateBatch) above(fromPts int) []tg.UpdateClass {
	return b.ups[sort.SearchInts(b.pts, fromPts+1):]
}

// buildUpdates hydrates userID's events after fromPts into wire updates plus the
// referenced users and the state to advertise. It is the single delivery path
// shared by updates.getDifference and real-time push. Difference includes the
// basic-dialog unread total; push omits unread totals to avoid account-wide reads.
//
// State is read first, then events are bounded to (fromPts, state.pts], so the
// advertised pts never runs past an event omitted from the response (events and
// their pts bump commit atomically per owner).
func (h *handlers) buildUpdates(ctx context.Context, userID int64, fromPts int, includeBasicUnreadCount bool) (updateBatch, error) {
	stateReader := h.store.StateWithoutUnread
	if includeBasicUnreadCount {
		stateReader = h.store.StateWithoutChannelUnread
	}
	state, err := stateReader(ctx, userID)
	if err != nil {
		return updateBatch{}, err
	}
	// Fetch one past the cap to detect truncation.
	events, err := h.store.EventsWindow(ctx, userID, fromPts, state.Pts, maxDiffEvents+1)
	if err != nil {
		return updateBatch{}, err
	}
	return h.buildUpdateBatch(ctx, userID, fromPts, state, events, maxDiffEvents)
}

func (h *handlers) buildUpdateBatch(ctx context.Context, userID int64, fromPts int, state store.State, events []store.Event, limit int) (updateBatch, error) {
	b := updateBatch{state: state, head: state.Pts}
	if len(events) > limit {
		events = events[:limit]
		b.more = true
	}

	msgs, err := h.batchMessages(ctx, userID, events)
	if err != nil {
		return updateBatch{}, err
	}
	rows := make([]store.Message, 0, len(msgs))
	for _, m := range msgs {
		rows = append(rows, m)
	}
	files, err := h.loadFiles(ctx, rows)
	if err != nil {
		return updateBatch{}, err
	}
	pollViews, err := h.pollViewsForMessages(ctx, userID, rows)
	if err != nil {
		return updateBatch{}, err
	}

	peers := map[int64]bool{}
	basicChats := map[int64]bool{}
	channels := map[int64]bool{}
	for _, ev := range events {
		up, refs, chatRefs, channelRefs, uerr := h.eventToUpdate(ctx, userID, ev, msgs, files, pollViews)
		if uerr != nil {
			return updateBatch{}, uerr
		}
		if up == nil {
			continue
		}
		b.ups = append(b.ups, up)
		b.pts = append(b.pts, ev.Pts)
		for _, id := range refs {
			peers[id] = true
		}
		for _, id := range chatRefs {
			basicChats[id] = true
		}
		for _, id := range channelRefs {
			channels[id] = true
		}
	}
	// When truncated, advertise only through the last included event's pts.
	if b.more && len(events) > 0 {
		b.state.Pts = events[len(events)-1].Pts
	}

	b.users, err = h.loadUsers(ctx, peers, userID)
	if err != nil {
		return updateBatch{}, err
	}
	b.chats, err = h.loadChats(ctx, basicChats, userID)
	if err != nil {
		return updateBatch{}, err
	}
	if len(channels) > 0 {
		channelTL, cerr := h.loadChannels(ctx, channels, userID)
		if cerr != nil {
			return updateBatch{}, cerr
		}
		b.chats = append(b.chats, channelTL...)
	}
	return b, nil
}

// batchMessages loads, in one statement, the message rows the batch's events
// name, keyed by local id. A row that has since vanished is absent, which is the
// same skip its per-event lookup used to produce. Loading them up front is what
// lets the batch's media be hydrated in a single query.
func (h *handlers) batchMessages(ctx context.Context, userID int64, events []store.Event) (map[int64]store.Message, error) {
	seen := make(map[int64]bool, len(events))
	localIDs := make([]int64, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case store.EventNewMessage, store.EventEdit, store.EventReadIn, store.EventReadOut, store.EventPin, store.EventUnpin:
		default:
			continue
		}
		if ev.LocalID == 0 {
			continue
		}
		if seen[ev.LocalID] {
			continue
		}
		seen[ev.LocalID] = true
		localIDs = append(localIDs, ev.LocalID)
	}
	return h.store.MessagesByOwnerLocalIDs(ctx, userID, localIDs)
}

// eventToUpdate builds the wire update for one event owned by userID, returning
// the update, the user ids it references, the chat ids (basic chats only) and
// the channel ids it references. A nil update (message vanished, or an empty
// read marker) is skipped by the caller.
// msgs and files are the batch's pre-loaded rows and their media.
func (h *handlers) eventToUpdate(ctx context.Context, userID int64, ev store.Event, msgs map[int64]store.Message, files *messageMedia, pollViews map[int64]store.Poll) (tg.UpdateClass, []int64, []int64, []int64, error) {
	switch ev.Type {
	case store.EventNewMessage, store.EventEdit:
		m, ok := msgs[ev.LocalID]
		if !ok {
			return nil, nil, nil, nil, nil
		}
		if m.Deleted {
			// Retained timeline events can outlive the message copy's visibility.
			// Keep their PTS slot in the shared stream, but never serialize the
			// deleted row's text, media, action, or sender references again.
			empty := &tg.MessageEmpty{ID: int(m.LocalID)}
			if ev.Type == store.EventEdit {
				return &tg.UpdateEditMessage{Message: empty, Pts: ev.Pts, PtsCount: 1}, nil, nil, nil, nil
			}
			return &tg.UpdateNewMessage{Message: empty, Pts: ev.Pts, PtsCount: 1}, nil, nil, nil, nil
		}
		// The create action's user list is current member ids, so it is the same
		// disclosure loadChats gates: a viewer removed from the chat still replays
		// their retained copy of the event, and must not learn who is in it now.
		// An empty list is what a non-member gets.
		var createUsers []int64
		if m.Action == store.ChatActionCreate {
			member, merr := h.store.IsMember(ctx, m.PeerID, userID)
			if merr != nil {
				return nil, nil, nil, nil, merr
			}
			if member {
				parts, perr := h.store.Participants(ctx, m.PeerID)
				if perr != nil {
					return nil, nil, nil, nil, perr
				}
				createUsers = make([]int64, len(parts))
				for i, p := range parts {
					createUsers[i] = p.UserID
				}
			}
		}
		tlMsg := h.messageToTL(m, createUsers, files, nil, nil, h.dcID)
		if poll, ok := pollViews[m.LocalID]; ok {
			pollMessage, pollErr := messageToTLWithPoll(m, createUsers, files, nil, nil, poll)
			if pollErr != nil {
				return nil, nil, nil, nil, pollErr
			}
			tlMsg = pollMessage
		}
		refs := []int64{m.FromID}
		var chatRefs, channelRefs []int64
		if m.PeerType == store.PeerTypeChat {
			// The peer is the chat, not a user, so the owner is named explicitly:
			// a client rendering a group needs itself in the user list.
			chatRefs = []int64{m.PeerID}
			refs = append(refs, userID)
			// A service message names user ids in its action; without them in the
			// enclosing Users a client renders the add, the removal or the create
			// as unknown users. createUsers is already F6-gated and stays nil for
			// a non-member, so appending it discloses nothing new.
			switch m.Action {
			case store.ChatActionAddUser, store.ChatActionDeleteUser:
				refs = append(refs, m.ActionUserID)
			case store.ChatActionCreate:
				refs = append(refs, createUsers...)
			}
		} else {
			refs = append(refs, m.PeerID)
		}
		// Forwarded messages reference the original sender and optionally a channel.
		if m.FwdFromID != 0 && m.FwdChannelID == 0 {
			refs = append(refs, m.FwdFromID)
		}
		if m.FwdChannelID != 0 {
			channelRefs = []int64{m.FwdChannelID}
		}
		if ev.Type == store.EventEdit {
			return &tg.UpdateEditMessage{Message: tlMsg, Pts: ev.Pts, PtsCount: 1}, refs, chatRefs, channelRefs, nil
		}
		return &tg.UpdateNewMessage{Message: tlMsg, Pts: ev.Pts, PtsCount: 1}, refs, chatRefs, channelRefs, nil

	case store.EventDelete:
		return &tg.UpdateDeleteMessages{Messages: []int{int(ev.LocalID)}, Pts: ev.Pts, PtsCount: 1}, nil, nil, nil, nil

	case store.EventChatParticipantDelete:
		chat, ok, err := h.store.ChatByID(ctx, ev.LocalID)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if !ok {
			return nil, nil, nil, nil, fmt.Errorf("chat %d missing for participant-delete event", ev.LocalID)
		}
		return &tg.UpdateChatParticipantDelete{
			ChatID: chat.ID, UserID: userID, Version: chat.Version,
		}, []int64{userID}, []int64{chat.ID}, nil, nil

	case store.EventReadIn, store.EventReadOut:
		if ev.LocalID == 0 {
			return nil, nil, nil, nil, nil
		}
		m, ok := msgs[ev.LocalID]
		if !ok {
			return nil, nil, nil, nil, nil
		}
		peer := peerToTL(m.PeerType, m.PeerID)
		var refs, chatRefs []int64
		if m.PeerType == store.PeerTypeChat {
			chatRefs = []int64{m.PeerID}
		} else {
			refs = []int64{m.PeerID}
		}
		if ev.Type == store.EventReadOut {
			return &tg.UpdateReadHistoryOutbox{Peer: peer, MaxID: int(ev.LocalID), Pts: ev.Pts, PtsCount: 1}, refs, chatRefs, nil, nil
		}
		return &tg.UpdateReadHistoryInbox{Peer: peer, MaxID: int(ev.LocalID), StillUnreadCount: 0, Pts: ev.Pts, PtsCount: 1}, refs, chatRefs, nil, nil

	case store.EventPin, store.EventUnpin:
		// A deleted copy still names the chat the pin state belongs to, but
		// carries no message id the owner could display.
		m, ok := msgs[ev.LocalID]
		if !ok || m.PeerType != store.PeerTypeChat {
			return nil, nil, nil, nil, nil
		}
		var messages []int
		if !m.Deleted {
			messages = []int{int(ev.LocalID)}
		}
		return &tg.UpdatePinnedMessages{
			Pinned:   ev.Type == store.EventPin,
			Peer:     &tg.PeerChat{ChatID: m.PeerID},
			Messages: messages,
			Pts:      ev.Pts,
			PtsCount: 1,
		}, nil, []int64{m.PeerID}, nil, nil

	default:
		return nil, nil, nil, nil, nil
	}
}

// loadUsers hydrates server-derived user ids into wire users, marking selfID as
// Self. viewerID is the account receiving this response; it is used to derive
// the per-viewer access hash for each user. These ids remain subject to the
// live-edge entitlement check; explicit user peers authorized by a validated
// access hash use loadUsersForUserPeer instead.
func (h *handlers) loadUsers(ctx context.Context, ids map[int64]bool, viewerID int64) ([]tg.UserClass, error) {
	return h.loadUsersWithAuthorizedIDs(ctx, ids, viewerID, nil)
}

// loadUsersForChannelParticipants hydrates ids admitted by the current
// participant-list or participant-row authorization snapshot. The shared
// renderer still derives each user's hash for viewerID and only includes the
// phone on the viewer's own profile.
func (h *handlers) loadUsersForChannelParticipants(ctx context.Context, ids map[int64]bool, viewerID int64) ([]tg.UserClass, error) {
	return h.loadUsersWithAuthorizedIDs(ctx, ids, viewerID, ids)
}

// loadUsersForUserPeer hydrates the viewer and one user peer that was validated
// at the RPC boundary for this request. The peer's valid per-viewer access hash
// permits its public profile even without a live dialog edge.
func (h *handlers) loadUsersForUserPeer(ctx context.Context, viewerID, peerID int64) ([]tg.UserClass, error) {
	ids := map[int64]bool{viewerID: true, peerID: true}
	return h.loadUsersWithAuthorizedIDs(ctx, ids, viewerID, map[int64]bool{peerID: true})
}

// loadUsersWithAuthorizedIDs keeps row-derived ids behind the live-edge gate.
// authorizedIDs carries peers or participant rows admitted by a check made by
// the current request, such as an access hash or channel role.
func (h *handlers) loadUsersWithAuthorizedIDs(ctx context.Context, ids map[int64]bool, viewerID int64, authorizedIDs map[int64]bool) ([]tg.UserClass, error) {
	if len(ids) == 0 {
		return []tg.UserClass{}, nil
	}
	list := make([]int64, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	users, err := h.store.UsersByID(ctx, list)
	if err != nil {
		return nil, err
	}
	// Skip the live-edge query when the caller already admitted every requested
	// id (or it is the viewer); otherwise the query repeats the same membership
	// walk for rows that are already authorized.
	needsEntitlement := false
	for id := range ids {
		if id != viewerID && !authorizedIDs[id] {
			needsEntitlement = true
			break
		}
	}
	var entitled map[int64]bool
	if needsEntitlement {
		entitled, err = h.entitledUserIDs(ctx, viewerID, list)
		if err != nil {
			return nil, err
		}
	}
	return h.renderUsers(ctx, users, viewerID, entitled, authorizedIDs)
}

func (h *handlers) renderUsers(ctx context.Context, users map[int64]store.User, viewerID int64, entitled, authorizedIDs map[int64]bool) ([]tg.UserClass, error) {
	visibleIDs := make([]int64, 0, len(users))
	for id := range users {
		if id == viewerID || entitled[id] || authorizedIDs[id] {
			visibleIDs = append(visibleIDs, id)
		}
	}
	contactStates, err := h.contactStatesForUsers(ctx, viewerID, visibleIDs)
	if err != nil {
		return nil, err
	}
	visibleUsers := make([]store.User, 0, len(visibleIDs))
	for _, id := range visibleIDs {
		visibleUsers = append(visibleUsers, users[id])
	}
	privacySubjects := lastSeenPrivacySubjects(visibleUsers, viewerID)
	visibility := make(map[int64]store.LastSeenVisibility, len(privacySubjects))
	if len(privacySubjects) > 0 {
		visibility, err = h.lastSeenVisibilityForUsers(ctx, privacySubjects, viewerID)
		if err != nil {
			return nil, err
		}
	}
	out := make([]tg.UserClass, 0, len(users))
	for id, u := range users {
		if id != viewerID && !entitled[id] && !authorizedIDs[id] {
			out = append(out, &tg.UserEmpty{ID: id})
			continue
		}
		out = append(out, h.userToTL(u, viewerID, id == viewerID, contactStates[id], visibility[id]))
	}
	return out, nil
}

// entitledUserIDs reports which of ids the viewer is entitled to see live:
// the viewer themself, a 1:1 dialog partner, a current participant of a chat
// the viewer is in, or a current unbanned member of a channel the viewer is in.
// The four sources are the live edges that admit an account's metadata to a
// client — exactly the surfaces the store already uses for chat and channel
// admission. The predicate is one store round trip: the query takes the id set
// and the viewer and returns only the ids an edge admits, so a viewer in a
// large channel does not materialize the channel's whole member list on every
// loadUsers call. The channel edge requires the viewer's own row to be
// unbanned: a banned viewer is not a current member, so the channel admits
// nothing for them.
func (h *handlers) entitledUserIDs(ctx context.Context, viewerID int64, ids []int64) (map[int64]bool, error) {
	rows, err := h.store.EntitledUserIDs(ctx, viewerID, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(rows))
	for _, id := range rows {
		out[id] = true
	}
	return out, nil
}

// loadChats hydrates chat ids for viewerID. A chat the viewer is no longer a
// member of still reaches here — their dialog row and their retained message
// copies survive removal by design — so it must not keep serving live metadata.
// tg.ChatForbidden carries the id and an empty title and nothing else, which is
// what tells a client to stop rendering the chat as active. The title is blanked
// deliberately: the live row keeps changing after removal, and the creator may
// rename the chat, so serving it would leave a writable channel into an account
// that was ejected.
//
// Membership, participant counts, and the selected photo pointer come from one
// repeatable-read snapshot. A cache populated while rendering a create action
// cannot authorize metadata read after that member has been removed.
func (h *handlers) loadChats(ctx context.Context, ids map[int64]bool, viewerID int64) ([]tg.ChatClass, error) {
	chatIDs := make([]int64, 0, len(ids))
	for id := range ids {
		chatIDs = append(chatIDs, id)
	}
	snapshot, err := h.store.ChatListInfoForMemberSnapshot(ctx, viewerID, chatIDs)
	if err != nil {
		return nil, err
	}

	chats := make([]tg.ChatClass, 0, len(ids))
	for _, id := range chatIDs {
		chat, ok := snapshot.Chats[id]
		if !ok {
			// Keep absent chats omitted. This lookup is used only to distinguish a
			// deleted chat from an existing chat excluded by the membership snapshot.
			_, exists, err := h.store.ChatByID(ctx, id)
			if err != nil {
				return nil, err
			}
			if exists {
				chats = append(chats, &tg.ChatForbidden{ID: id, Title: ""})
			}
			continue
		}
		wireChat, err := h.chatToTL(ctx, chat, int(snapshot.ParticipantCounts[id]), viewerID)
		if err != nil {
			return nil, fmt.Errorf("load chat %d photo: %w", id, err)
		}
		chats = append(chats, wireChat)
	}
	return chats, nil
}

// loadChannels hydrates channel ids for viewerID, the channel counterpart of
// loadChats. An id with no channel row is skipped. A viewer who is not a member,
// or whose membership is banned as of now, gets tg.ChannelForbidden — a ban is
// not cosmetic, so it must revoke metadata the same way leaving does.
//
// Two queries per channel per batch, no cache, for the reason loadChats states:
// a batch names very few distinct channels.
func (h *handlers) loadChannels(ctx context.Context, ids map[int64]bool, viewerID int64) ([]tg.ChatClass, error) {
	channels, _, err := h.loadChannelsWithKinds(ctx, ids, viewerID)
	return channels, err
}

// loadChannelsWithKinds returns channel kinds from the same rows used to
// hydrate the channel chats, so callers that render channel messages do not
// need a second ChannelByID read for each channel.
func (h *handlers) loadChannelsWithKinds(ctx context.Context, ids map[int64]bool, viewerID int64) ([]tg.ChatClass, map[int64]bool, error) {
	channels := make([]tg.ChatClass, 0, len(ids))
	broadcasts := make(map[int64]bool, len(ids))
	channelIDs := make([]int64, 0, len(ids))
	for id := range ids {
		channelIDs = append(channelIDs, id)
	}
	snapshots, err := h.store.ChannelsForViewer(ctx, channelIDs, viewerID)
	if err != nil {
		return nil, nil, err
	}
	for _, snapshot := range snapshots {
		ch := snapshot.Channel
		member := snapshot.HasMember && !snapshot.Member.Banned(time.Now())
		broadcasts[ch.ID] = !ch.Megagroup
		channels = append(channels, h.channelToTL(ch, snapshot.Member, member, viewerID))
	}
	return channels, broadcasts, nil
}

// channelBatch is one channel's hydrated updates plus everything a client must
// be told alongside them. pts[i] is the pts of ups[i], ascending.
type channelBatch struct {
	ups        []tg.UpdateClass
	pts        []int
	users      []tg.UserClass
	chats      []tg.ChatClass
	currentPts int
	more       bool
}

// buildChannelUpdates hydrates channelID's events after fromPts into wire
// updates plus the referenced users and channels. It is the single delivery
// path for updates.getChannelDifference; the live push (MAIN-96) must call
// this same function, never a second serialisation path. Limit is clamped into
// [1, maxDiffEvents] before this is called.
//
// State is read first, then events are bounded to (fromPts, currentPts], so the
// advertised pts never runs past an event omitted from the response. This is
// the same ordering buildUpdates uses for the per-account stream.
func (h *handlers) buildChannelUpdates(ctx context.Context, channelID, viewerID int64, fromPts, limit, currentPts int, broadcast bool, availableMinID int64) (channelBatch, error) {
	// Fetch one past the cap to detect truncation.
	events, err := h.store.ChannelEventsWindow(ctx, channelID, fromPts, currentPts, limit+1)
	if err != nil {
		return channelBatch{}, err
	}
	b := channelBatch{currentPts: currentPts}
	if len(events) > limit {
		events = events[:limit]
		b.more = true
	}
	visibleEvents := make([]store.ChannelEvent, 0, len(events))
	for _, ev := range events {
		if ev.LocalID > 0 && ev.LocalID <= availableMinID {
			continue
		}
		visibleEvents = append(visibleEvents, ev)
	}

	// Collect local ids for batched message load.
	localIDs := make([]int64, 0, len(visibleEvents))
	for _, ev := range visibleEvents {
		if ev.Type == store.EventNewMessage || ev.Type == store.EventEdit {
			localIDs = append(localIDs, ev.LocalID)
		}
	}
	msgs, err := h.store.ChannelMessages(ctx, channelID, localIDs)
	if err != nil {
		return channelBatch{}, err
	}

	// Load files only for live messages; tombstones carry identity only.
	chMsgs := make([]store.ChannelMessage, 0, len(msgs))
	for _, m := range msgs {
		chMsgs = append(chMsgs, m)
	}
	if err = h.attachChannelPollViews(ctx, viewerID, channelID, chMsgs); err != nil {
		return channelBatch{}, err
	}
	for _, message := range chMsgs {
		msgs[message.LocalID] = message
	}
	files, err := h.loadChannelFiles(ctx, chMsgs)
	if err != nil {
		return channelBatch{}, err
	}

	peers := map[int64]bool{}
	for _, ev := range visibleEvents {
		up, refs, upErr := h.channelEventToUpdate(ctx, channelID, viewerID, ev, msgs, files, broadcast)
		if upErr != nil {
			return channelBatch{}, upErr
		}
		if up == nil {
			continue
		}
		if ev.Type == store.EventNewMessage {
			if message := msgs[ev.LocalID]; !message.Deleted && message.FromID == viewerID && message.RandomID != 0 {
				// The sender needs the random_id mapping before the post so a live
				// echo can replace its pending send. Other members get only the post.
				b.ups = append(b.ups, &tg.UpdateMessageID{ID: int(message.LocalID), RandomID: message.RandomID})
			}
		}
		b.ups = append(b.ups, up)
		b.pts = append(b.pts, ev.Pts)
		for _, id := range refs {
			peers[id] = true
		}
	}
	// When truncated, pts advertised is the last included event's pts, not
	// the channel's current pts — that is the whole point of the bound.
	if b.more && len(events) > 0 {
		b.currentPts = events[len(events)-1].Pts
	}

	b.users, err = h.loadUsers(ctx, peers, viewerID)
	if err != nil {
		return channelBatch{}, err
	}
	b.chats, err = h.loadChannels(ctx, map[int64]bool{channelID: true}, viewerID)
	if err != nil {
		return channelBatch{}, err
	}
	return b, nil
}

// channelEventToUpdate builds the wire update for one channel event, returning
// the update and the user ids it references. Delete events need no message-row
// hydration; new and edit events for current tombstones are suppressed. A nil
// update is also returned when a new/edit message row is not found.
func (h *handlers) channelEventToUpdate(_ context.Context, channelID, viewerID int64, ev store.ChannelEvent, msgs map[int64]store.ChannelMessage, files *messageMedia, broadcast bool) (tg.UpdateClass, []int64, error) {
	switch ev.Type {
	case store.ChannelEventMetadataRefresh:
		return &tg.UpdateChannel{ChannelID: channelID}, nil, nil
	case store.EventDelete:
		return &tg.UpdateDeleteChannelMessages{
			ChannelID: channelID,
			Messages:  []int{int(ev.LocalID)},
			Pts:       ev.Pts,
			PtsCount:  1,
		}, nil, nil
	case store.EventPin, store.EventUnpin:
		return &tg.UpdatePinnedChannelMessages{
			Pinned:    ev.Type == store.EventPin,
			ChannelID: channelID,
			Messages:  []int{int(ev.LocalID)},
			Pts:       ev.Pts,
			PtsCount:  1,
		}, nil, nil
	case store.EventNewMessage, store.EventEdit:
		m, ok := msgs[ev.LocalID]
		if !ok {
			h.log.Debug("channel message row not found", "local_id", ev.LocalID, "channel_id", channelID, "pts", ev.Pts)
			return nil, nil, nil
		}
		message, err := h.channelMessageToTL(m, viewerID, files, broadcast, h.dcID)
		if err != nil {
			return nil, nil, err
		}
		if m.Deleted {
			return nil, nil, nil
		}
		var refs []int64
		if !broadcast {
			refs = []int64{m.FromID}
		}
		if ev.Type == store.EventEdit {
			return &tg.UpdateEditChannelMessage{
				Message:  message,
				Pts:      ev.Pts,
				PtsCount: 1,
			}, refs, nil
		}
		return &tg.UpdateNewChannelMessage{
			Message:  message,
			Pts:      ev.Pts,
			PtsCount: 1,
		}, refs, nil
	default:
		h.log.Debug("unknown channel event type", "type", ev.Type, "channel_id", channelID, "pts", ev.Pts)
		return nil, nil, nil
	}
}

// handleGetState serves updates.getState.
func (h *handlers) handleGetState(r *mtproto.Request) (bin.Encoder, error) {
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if err := h.checkChannelUnreadCountRateLimit(r); err != nil {
		return nil, err
	}
	st, err := h.store.State(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get state", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return stateToTL(st), nil
}

// handleGetDifference serves updates.getDifference: it replays the caller's
// missed PTS and QTS events alongside bounded owner-private marker streams.
// When caught up it returns differenceEmpty; a client ahead is clamped to empty.
func (h *handlers) handleGetDifference(r *mtproto.Request) (bin.Encoder, error) {
	result, _, err := h.handleGetDifferenceForConn(nil, r)
	return result, err
}

func appendUniqueDifferenceUsers(existing, additional []tg.UserClass) []tg.UserClass {
	seen := make(map[int64]bool, len(existing)+len(additional))
	for _, user := range existing {
		seen[user.GetID()] = true
	}
	for _, user := range additional {
		if seen[user.GetID()] {
			continue
		}
		seen[user.GetID()] = true
		existing = append(existing, user)
	}
	return existing
}

func appendUniqueDifferenceChats(existing, additional []tg.ChatClass) []tg.ChatClass {
	seen := make(map[string]bool, len(existing)+len(additional))
	for _, chat := range existing {
		seen[fmt.Sprintf("%T:%d", chat, chat.GetID())] = true
	}
	for _, chat := range additional {
		key := fmt.Sprintf("%T:%d", chat, chat.GetID())
		if seen[key] {
			continue
		}
		seen[key] = true
		existing = append(existing, chat)
	}
	return existing
}

type secretChatVersionKey struct {
	ID    int32
	Date  int64
	State string
}

func secretChatDeliveryVersion(chat store.SecretChat) store.SecretChatDeliveryVersion {
	return store.SecretChatDeliveryVersion{ID: chat.ID, Date: chat.Date, State: chat.State}
}

func secretChatDeliveryVersionKey(version store.SecretChatDeliveryVersion) secretChatVersionKey {
	return secretChatVersionKey{ID: version.ID, Date: version.Date.UnixMicro(), State: version.State}
}

func mergeSecretChatDeliveryVersions(versions ...[]store.SecretChatDeliveryVersion) []store.SecretChatDeliveryVersion {
	var merged []store.SecretChatDeliveryVersion
	seen := make(map[secretChatVersionKey]struct{})
	for _, batch := range versions {
		for _, version := range batch {
			key := secretChatDeliveryVersionKey(version)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, version)
		}
	}
	return merged
}

func secretChatDeliveryVersions(chats []store.SecretChat) []store.SecretChatDeliveryVersion {
	versions := make([]store.SecretChatDeliveryVersion, 0, len(chats))
	for _, chat := range chats {
		versions = append(versions, secretChatDeliveryVersion(chat))
	}
	return versions
}

func retainSecretChatVersionsSince(versions []store.SecretChatDeliveryVersion, from time.Time) []store.SecretChatDeliveryVersion {
	retained := make([]store.SecretChatDeliveryVersion, 0, len(versions))
	for _, version := range versions {
		if !version.Date.Before(from) {
			retained = append(retained, version)
		}
	}
	return retained
}

func secretChatsNotAcknowledged(chats []store.SecretChat, acknowledged []store.SecretChatDeliveryVersion) []store.SecretChat {
	seen := make(map[secretChatVersionKey]struct{}, len(acknowledged))
	for _, version := range acknowledged {
		seen[secretChatDeliveryVersionKey(version)] = struct{}{}
	}
	unacknowledged := make([]store.SecretChat, 0, len(chats))
	for _, chat := range chats {
		if _, ok := seen[secretChatDeliveryVersionKey(secretChatDeliveryVersion(chat))]; ok {
			continue
		}
		unacknowledged = append(unacknowledged, chat)
	}
	return unacknowledged
}

func secretChatPositionAfter(chat store.SecretChat, afterDate time.Time, afterID *int32) bool {
	if chat.Date.After(afterDate) {
		return true
	}
	if !chat.Date.Equal(afterDate) {
		return false
	}
	return afterID == nil || chat.ID > *afterID
}

func secretChatOverlapStart(resumeAt time.Time) time.Time {
	return resumeAt.Add(-dialogFilterMarkerGuard)
}

const dialogFilterMarkerGuard = 60 * time.Second

func dialogFilterMarkerWithinGuard(markerAt time.Time, found bool, requestDate int, serverNow time.Time) bool {
	if !found {
		return false
	}
	cutoff := time.Unix(int64(requestDate), 0)
	if serverNow.Before(cutoff) {
		cutoff = serverNow
	}
	return !markerAt.Before(cutoff.Add(-dialogFilterMarkerGuard))
}

func dialogStateMarkerWithinGuard(markerAt time.Time, found bool, requestDate int, serverNow time.Time) bool {
	return dialogFilterMarkerWithinGuard(markerAt, found, requestDate, serverNow)
}

func (h *handlers) handleGetDifferenceForConn(c *mtproto.Conn, r *mtproto.Request) (bin.Encoder, func(), error) {
	var req tg.UpdatesGetDifferenceRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, nil, errAuthKeyUnreg
	}
	chatRemovalAuthKeyID := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	pendingRemovalResumeDate, pendingRemovalStateFound, err := h.store.ChatRemovalResumeDateForState(
		r.Ctx, r.UserID, chatRemovalAuthKeyID, req.Date,
	)
	if err != nil {
		h.log.Error("read pending get difference chat removal state", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	confirmedRemovalResumeDate, removalStateConfirmed, err := h.store.AcknowledgeChatRemovalEventsForState(
		r.Ctx, r.UserID, chatRemovalAuthKeyID, req.Pts, req.Qts, req.Date,
	)
	if err != nil {
		h.log.Error("acknowledge get difference chat removals", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	var recovery DialogFilterCapture
	if c != nil {
		recovery = h.dialogFilterSync.Capture(c, r)
	}

	// Read durable refresh markers before selecting the PTS window. Eligible pin
	// refreshes are carried on every reply, and emitted pin/filter flags reserve
	// entries in the PTS stream's budget.
	filterRefresh := recovery.firstDifference || recovery.pending
	markerAt, markerFound, markerErr := h.store.DialogFilterChangeAt(r.Ctx, r.UserID)
	if markerErr != nil {
		h.log.Error("get difference dialog filter marker", "user_id", r.UserID, "err", markerErr)
		return nil, nil, errInternal
	}
	pinMarkerAt, pinMarkerFound, pinMarkerErr := h.store.DialogPinChangeAt(r.Ctx, r.UserID)
	if pinMarkerErr != nil {
		h.log.Error("get difference dialog pin marker", "user_id", r.UserID, "err", pinMarkerErr)
		return nil, nil, errInternal
	}
	state, err := h.store.StateWithoutChannelUnread(r.Ctx, r.UserID)
	if err != nil {
		h.log.Error("get difference state", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	draftReferenceDate := time.Unix(int64(req.Date), 0)
	now := h.now()
	if removalStateConfirmed {
		draftReferenceDate = time.Unix(int64(confirmedRemovalResumeDate), 0)
	} else if pendingRemovalStateFound {
		draftReferenceDate = time.Unix(int64(pendingRemovalResumeDate), 0)
	}
	if now.Before(draftReferenceDate) {
		draftReferenceDate = now
	}
	requestDate := int(draftReferenceDate.Unix())
	notifySettingsSecretChatResumeAt := time.Unix(int64(requestDate), 0)
	var notifySettingsSecretChatResumeID *int32
	notifySettingsAuthKeyID := mtproto.AuthKeyIDInt64(r.AuthKeyID)
	notifySettingsCutoff := draftReferenceDate.Add(-dialogFilterMarkerGuard)
	var notifySettingsAfter *store.NotifySettingsDifferencePosition
	var notifySettingsCursor store.NotifySettingsDifferenceCursor
	var notifySettingsSecretChatAcknowledged []store.SecretChatDeliveryVersion
	notifySettingsCursorFound := false
	notifySettingsCursorDateFound := false
	notifySettingsCursorStateFound := false
	notifySettingsCursorStateDate := 0
	if notifySettingsAuthKeyID != 0 {
		cursor, found, cursorErr := h.store.NotifySettingsDifferenceCursorByAuthKey(r.Ctx, notifySettingsAuthKeyID)
		if cursorErr != nil {
			h.log.Error("get difference notification settings cursor", "user_id", r.UserID, "err", cursorErr)
			return nil, nil, errInternal
		}
		if found {
			if cursor.UserID == r.UserID {
				notifySettingsCursor = cursor
				notifySettingsCursorStateFound = true
				notifySettingsCursorStateDate = cursor.State.Date
				notifySettingsCursorDateFound = cursor.State.Date == req.Date
				notifySettingsSecretChatResumeAt = cursor.SecretChatResumeAt
				notifySettingsSecretChatResumeID = cursor.SecretChatResumeID
				if notifySettingsCursorDateFound {
					// An issued Date remains a token for the logical recovery boundary
					// even if live updates have moved the client's PTS or QTS.
					draftReferenceDate = time.Unix(int64(cursor.ResumeDate), 0)
					if now.Before(draftReferenceDate) {
						draftReferenceDate = now
					}
					requestDate = int(draftReferenceDate.Unix())
				}
				switch {
				case cursor.State.Pts == req.Pts && cursor.State.Qts == req.Qts && cursor.State.Date == req.Date:
					notifySettingsCursorFound = true
					secretChatPageAcknowledged := len(cursor.SecretChatIssued) > 0
					notifySettingsSecretChatAcknowledged = retainSecretChatVersionsSince(
						mergeSecretChatDeliveryVersions(cursor.SecretChatAcknowledged, cursor.SecretChatIssued),
						secretChatOverlapStart(cursor.SecretChatResumeAt),
					)
					cursor.SecretChatAcknowledged = notifySettingsSecretChatAcknowledged
					cursor.SecretChatIssued = nil
					notifySettingsCursor = cursor
					if secretChatPageAcknowledged {
						// Presenting the exact state acknowledges the previous secret-chat
						// page. Persist that before reading the overlap so a lost next page
						// is replayed without consuming this acknowledgement twice.
						if err := h.store.SaveNotifySettingsDifferenceCursor(r.Ctx, cursor); err != nil {
							h.log.Error("acknowledge get difference secret-chat page", "user_id", r.UserID, "err", err)
							return nil, nil, errInternal
						}
					}
					notifySettingsCutoff = cursor.Cutoff
					notifySettingsAfter = &cursor.Last
				case notifySettingsCursorDateFound:
					// Restart settings from the logical Date's guard, while retaining
					// the translation needed to replay and acknowledge lifecycle rows.
					notifySettingsCutoff = draftReferenceDate.Add(-dialogFilterMarkerGuard)
					notifySettingsSecretChatAcknowledged = mergeSecretChatDeliveryVersions(cursor.SecretChatAcknowledged)
				case req.Date < cursor.State.Date:
					// The client is retrying from before the page we issued. Replay the
					// original guarded window so a dropped page is resent instead of
					// skipping rows that were only present in that response.
					notifySettingsSecretChatAcknowledged = mergeSecretChatDeliveryVersions(cursor.SecretChatAcknowledged)
					requestedCutoff := draftReferenceDate.Add(-dialogFilterMarkerGuard)
					staleDate := time.Unix(int64(req.Date), 0)
					resumeDate := time.Unix(int64(cursor.ResumeDate), 0)
					if staleDate.After(resumeDate) {
						staleDate = resumeDate
					}
					if staleDate.After(cursor.Cutoff) {
						staleDate = cursor.Cutoff
					}
					if now.Before(staleDate) {
						staleDate = now
					}
					draftReferenceDate = staleDate
					requestDate = int(draftReferenceDate.Unix())
					notifySettingsSecretChatResumeAt = staleDate
					notifySettingsSecretChatResumeID = nil
					notifySettingsCutoff = cursor.Cutoff
					if requestedCutoff.Before(notifySettingsCutoff) {
						notifySettingsCutoff = requestedCutoff
					}
				default:
					// The client's Date is the recovery boundary once it has moved
					// beyond this cursor. Do not restart from the cursor's original
					// cutoff, which would replay already acknowledged history.
					notifySettingsSecretChatAcknowledged = nil
					notifySettingsSecretChatResumeAt = draftReferenceDate
					notifySettingsSecretChatResumeID = nil
					notifySettingsCutoff = draftReferenceDate.Add(-dialogFilterMarkerGuard)
					notifySettingsCursorStateFound = false
					if deleteErr := h.store.DeleteNotifySettingsDifferenceCursor(r.Ctx, notifySettingsAuthKeyID); deleteErr != nil {
						h.log.Error("discard stale get difference notification settings cursor", "user_id", r.UserID, "err", deleteErr)
						return nil, nil, errInternal
					}
				}
			} else if deleteErr := h.store.DeleteNotifySettingsDifferenceCursor(r.Ctx, notifySettingsAuthKeyID); deleteErr != nil {
				h.log.Error("discard stale get difference notification settings cursor", "user_id", r.UserID, "err", deleteErr)
				return nil, nil, errInternal
			}
		}
	}
	filterRefresh = filterRefresh || dialogFilterMarkerWithinGuard(markerAt, markerFound, requestDate, now)
	pinRefresh := dialogFilterMarkerWithinGuard(pinMarkerAt, pinMarkerFound, requestDate, now)
	draftChanges, err := h.store.CloudDraftChangesForOwnerSince(r.Ctx, r.UserID, draftReferenceDate.Add(-dialogFilterMarkerGuard))
	if err != nil {
		h.log.Error("get difference cloud drafts", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	draftMore := len(draftChanges) > maxDiffEvents
	draftContinuationDate := 0
	if draftMore {
		draftContinuationDate = int(draftChanges[maxDiffEvents].ChangedAt.Unix())
		draftChanges = draftChanges[:maxDiffEvents]
	}
	unreadMarkChanges, err := h.store.DialogUnreadMarkChangesForOwnerSince(r.Ctx, r.UserID, draftReferenceDate.Add(-dialogFilterMarkerGuard))
	if err != nil {
		h.log.Error("get difference dialog unread marks", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	unreadMarkMore := len(unreadMarkChanges) > maxDiffEvents
	unreadMarkContinuationDate := 0
	if unreadMarkMore {
		unreadMarkContinuationDate = int(unreadMarkChanges[maxDiffEvents].ChangedAt.Unix())
		unreadMarkChanges = unreadMarkChanges[:maxDiffEvents]
	}
	notifySettingsChanges, err := h.store.NotifySettingsChangesForOwnerSince(r.Ctx, r.UserID, notifySettingsCutoff, notifySettingsAfter)
	if err != nil {
		h.log.Error("get difference notification settings", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	notifySettingsMore := len(notifySettingsChanges) > maxDiffEvents
	notifySettingsContinuationDate := 0
	var notifySettingsLastDelivered store.NotifySettingsChange
	if notifySettingsMore {
		notifySettingsContinuationDate = int(notifySettingsChanges[maxDiffEvents].ChangedAt.Unix())
		notifySettingsChanges = notifySettingsChanges[:maxDiffEvents]
	}
	if len(notifySettingsChanges) > 0 {
		notifySettingsLastDelivered = notifySettingsChanges[len(notifySettingsChanges)-1]
	}
	// Fetch one extra PTS event to detect truncation at the ordinary cap. Refresh
	// controls reserve room only in this stream; the other replay streams retain
	// their separate limits.
	ptsEvents, err := h.store.EventsWindow(r.Ctx, r.UserID, req.Pts, state.Pts, maxDiffEvents+1)
	if err != nil {
		h.log.Error("get difference pts events", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	ptsMoreAtCap := len(ptsEvents) > maxDiffEvents

	// Role state is a durable, non-pts snapshot with its own cap. The response-success
	// hook consumes only the marker versions carried by this reply.
	adminSnapshots, err := h.store.ChatAdminSnapshotsForMember(r.Ctx, r.UserID, int32(maxDiffEvents+1))
	if err != nil {
		h.log.Error("get difference chat admin snapshots", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	adminMore := len(adminSnapshots) > maxDiffEvents
	if adminMore {
		adminSnapshots = adminSnapshots[:maxDiffEvents]
	}
	var adminUpdates []tg.UpdateClass
	adminEventIDs := make([]int64, 0, len(adminSnapshots))
	var adminUsers []tg.UserClass
	var adminChats []tg.ChatClass
	if len(adminSnapshots) > 0 {
		userIDs := make(map[int64]bool, len(adminSnapshots))
		chatIDs := make(map[int64]bool, len(adminSnapshots))
		for _, snapshot := range adminSnapshots {
			adminEventIDs = append(adminEventIDs, snapshot.EventID)
			userIDs[snapshot.UserID] = true
			chatIDs[snapshot.ChatID] = true
			adminUpdates = append(adminUpdates, &tg.UpdateChatParticipantAdmin{
				ChatID:  snapshot.ChatID,
				UserID:  snapshot.UserID,
				IsAdmin: snapshot.IsAdmin,
				Version: snapshot.Version,
			})
		}
		users, uerr := h.loadUsers(r.Ctx, userIDs, r.UserID)
		if uerr != nil {
			h.log.Error("get difference chat admin users", "user_id", r.UserID, "err", uerr)
			return nil, nil, errInternal
		}
		chats, cerr := h.loadChats(r.Ctx, chatIDs, r.UserID)
		if cerr != nil {
			h.log.Error("get difference chat admin chats", "user_id", r.UserID, "err", cerr)
			return nil, nil, errInternal
		}
		adminUsers, adminChats = users, chats
	}
	consumeAdminMarkers := func() {
		if len(adminEventIDs) == 0 {
			return
		}
		if err := h.store.DeleteChatAdminStateMarkersByEventIDs(r.Ctx, r.UserID, adminEventIDs); err != nil {
			h.log.Error("consume get difference chat admin markers", "user_id", r.UserID, "err", err)
		}
	}

	// Chat removal is durable but PTS-neutral because its wire update has no
	// pts/pts_count fields. getDifference reserves a Date token before returning;
	// only a later request carrying that token acknowledges the page.
	chatRemovalSnapshots, err := h.store.ChatRemovalSnapshotsForAuthKey(r.Ctx, r.UserID, chatRemovalAuthKeyID, int32(maxDiffEvents+1))
	if err != nil {
		h.log.Error("get difference chat removal snapshots", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	chatRemovalMore := len(chatRemovalSnapshots) > maxDiffEvents
	if chatRemovalMore {
		chatRemovalSnapshots = chatRemovalSnapshots[:maxDiffEvents]
	}
	var chatRemovalUpdates []tg.UpdateClass
	chatRemovalChatIDs := make([]int64, 0, len(chatRemovalSnapshots))
	var chatRemovalUsers []tg.UserClass
	var chatRemovalChats []tg.ChatClass
	if len(chatRemovalSnapshots) > 0 {
		userIDs := make(map[int64]bool, len(chatRemovalSnapshots))
		chatIDs := make(map[int64]bool, len(chatRemovalSnapshots))
		for _, snapshot := range chatRemovalSnapshots {
			chatRemovalChatIDs = append(chatRemovalChatIDs, snapshot.ChatID)
			userIDs[snapshot.OwnerID] = true
			chatIDs[snapshot.ChatID] = true
			chatRemovalUpdates = append(chatRemovalUpdates, &tg.UpdateChatParticipantDelete{
				ChatID:  snapshot.ChatID,
				UserID:  snapshot.OwnerID,
				Version: snapshot.Version,
			})
		}
		users, uerr := h.loadUsers(r.Ctx, userIDs, r.UserID)
		if uerr != nil {
			h.log.Error("get difference chat removal users", "user_id", r.UserID, "err", uerr)
			return nil, nil, errInternal
		}
		chats, cerr := h.loadChats(r.Ctx, chatIDs, r.UserID)
		if cerr != nil {
			h.log.Error("get difference chat removal chats", "user_id", r.UserID, "err", cerr)
			return nil, nil, errInternal
		}
		chatRemovalUsers, chatRemovalChats = users, chats
	}
	// Qts replay retains its independent 500-event cap. A client-ahead qts remains
	// clamped to the current state.
	var encMsgs []tg.EncryptedMessageClass
	encMore := false
	newQts := state.Qts
	if req.Qts < state.Qts {
		evts, eerr := h.store.EncryptedEventsWindow(r.Ctx, r.UserID, req.Qts, state.Qts, maxDiffEvents+1)
		if eerr != nil {
			h.log.Error("get difference qts", "user_id", r.UserID, "err", eerr)
			return nil, nil, errInternal
		}
		if len(evts) > maxDiffEvents {
			evts = evts[:maxDiffEvents]
			encMore = true
		}
		for _, ev := range evts {
			encMsgs = append(encMsgs, encryptedMessage(ev))
		}
		if encMore && len(evts) > 0 {
			newQts = evts[len(evts)-1].Qts
		}
	}

	// Secret-chat lifecycle rows remain unbounded. Recognized Date tokens scan the
	// guarded overlap and suppress versions already acknowledged by this auth key,
	// so moved counters and late commits do not skip rows behind the high-water tuple.
	clientDate := time.Unix(int64(req.Date), 0)
	if removalStateConfirmed || pendingRemovalStateFound || (notifySettingsCursorStateFound && !notifySettingsCursorFound) {
		clientDate = draftReferenceDate
	}
	var secretChats []store.SecretChat
	var serr error
	if notifySettingsCursorDateFound {
		fromDate := secretChatOverlapStart(notifySettingsSecretChatResumeAt)
		secretChats, serr = h.store.SecretChatsFromDate(r.Ctx, r.UserID, fromDate)
		secretChats = secretChatsNotAcknowledged(secretChats, notifySettingsSecretChatAcknowledged)
	} else {
		secretChats, serr = h.store.SecretChatsAfterPosition(r.Ctx, r.UserID, clientDate, nil)
		if notifySettingsCursorStateFound {
			secretChats = secretChatsNotAcknowledged(secretChats, notifySettingsSecretChatAcknowledged)
		}
	}
	if serr != nil {
		h.log.Error("get difference secret chats", "user_id", r.UserID, "err", serr)
		return nil, nil, errInternal
	}
	includeFilterRefresh := filterRefresh && !ptsMoreAtCap && !encMore
	includePinRefresh := pinRefresh
	ptsLimit := maxDiffEvents
	if includeFilterRefresh {
		ptsLimit--
	}
	if includePinRefresh {
		ptsLimit--
	}
	b, err := h.buildUpdateBatch(r.Ctx, r.UserID, req.Pts, state, ptsEvents, ptsLimit)
	if err != nil {
		h.log.Error("build get difference updates", "user_id", r.UserID, "err", err)
		return nil, nil, errInternal
	}
	b.users = appendUniqueDifferenceUsers(b.users, adminUsers)
	b.chats = appendUniqueDifferenceChats(b.chats, adminChats)
	b.users = appendUniqueDifferenceUsers(b.users, chatRemovalUsers)
	b.chats = appendUniqueDifferenceChats(b.chats, chatRemovalChats)

	if !b.more && !encMore && !adminMore && !chatRemovalMore && !draftMore && !unreadMarkMore && !notifySettingsMore && len(b.ups) == 0 && len(adminUpdates) == 0 && len(chatRemovalUpdates) == 0 && len(encMsgs) == 0 && len(secretChats) == 0 && len(draftChanges) == 0 && len(unreadMarkChanges) == 0 && len(notifySettingsChanges) == 0 && !includeFilterRefresh && !includePinRefresh {
		emptyDate := max(b.state.Date, requestDate)
		if notifySettingsCursorDateFound {
			emptyDate = max(emptyDate, notifySettingsCursorStateDate)
			if emptyDate != notifySettingsCursorStateDate {
				// The empty reply can issue a newer Date too. Retain its translation
				// so the next poll can recover a lifecycle row committed after this read.
				cursor := notifySettingsCursor
				cursor.Cutoff = notifySettingsCutoff
				cursor.ResumeDate = requestDate
				cursor.SecretChatAcknowledged = notifySettingsSecretChatAcknowledged
				cursor.SecretChatIssued = nil
				cursor.State = store.State{Pts: b.state.Pts, Qts: newQts, Seq: b.state.Seq, Date: emptyDate}
				if err := h.store.SaveNotifySettingsDifferenceCursor(r.Ctx, cursor); err != nil {
					h.log.Error("save empty get difference date cursor", "user_id", r.UserID, "err", err)
					return nil, nil, errInternal
				}
			}
		}
		return &tg.UpdatesDifferenceEmpty{Date: emptyDate, Seq: b.state.Seq}, nil, nil
	}

	var newMessages []tg.MessageClass
	var other []tg.UpdateClass
	for _, u := range b.ups {
		if nm, ok := u.(*tg.UpdateNewMessage); ok {
			newMessages = append(newMessages, nm.Message)
		} else {
			other = append(other, u)
		}
	}
	other = append(other, adminUpdates...)
	other = append(other, chatRemovalUpdates...)
	for _, change := range draftChanges {
		date := change.Draft.UpdatedAt
		if !change.HasDraft {
			date = change.ChangedAt
		}
		other = append(other, &tg.UpdateDraftMessage{
			Peer:  peerToTL(change.Draft.PeerType, change.Draft.PeerID),
			Draft: cloudDraftToTL(change.Draft, change.HasDraft, date),
		})
	}
	for _, change := range unreadMarkChanges {
		update := &tg.UpdateDialogUnreadMark{Peer: &tg.DialogPeer{Peer: peerToTL(change.Peer.PeerType, change.Peer.PeerID)}}
		update.SetUnread(change.Unread)
		other = append(other, update)
	}
	for _, change := range notifySettingsChanges {
		other = append(other, &tg.UpdateNotifySettings{
			Peer:           notifySettingsUpdatePeer(change.Key),
			NotifySettings: *notifySettingsToTL(change.Value),
		})
	}
	for _, sc := range secretChats {
		other = append(other, &tg.UpdateEncryption{
			Chat: h.encryptedChatFor(sc, r.UserID),
			Date: int(sc.Date.Unix()),
		})
	}
	if includeFilterRefresh {
		other = append(other, &tg.UpdateDialogFilters{})
	}
	if includePinRefresh {
		other = append(other, &tg.UpdatePinnedDialogs{})
	}

	// The response advertises the PTS/QTS it actually covered. Date carries forward
	// a recovered client cursor unless a marker stream needs its overlap or
	// continuation position.
	st := b.state
	st.Date = max(st.Date, requestDate)
	st.Qts = newQts
	continuationDates := make([]int, 0, 3)
	if draftMore {
		continuationDates = append(continuationDates, draftContinuationDate)
	}
	if unreadMarkMore {
		continuationDates = append(continuationDates, unreadMarkContinuationDate)
	}
	if notifySettingsMore {
		continuationDates = append(continuationDates, notifySettingsContinuationDate)
	}
	switch {
	case len(continuationDates) > 0:
		// Date is the shared cursor for these independent streams. The next
		// request's existing 60-second overlap replays recent markers and
		// continues from the earliest omitted marker across these streams.
		st.Date = continuationDates[0]
		for _, date := range continuationDates[1:] {
			st.Date = min(st.Date, date)
		}
	case len(unreadMarkChanges) > 0 || len(notifySettingsChanges) > 0:
		// Keep a 60-second overlap for the date-only secret-chat stream while
		// advancing past persistent owner-private markers so they can drain.
		st.Date = int(now.Add(-dialogFilterMarkerGuard).Unix())
	case len(draftChanges) > 0:
		st.Date = max(st.Date, int(now.Unix()))
	}
	resumeDate := st.Date
	if notifySettingsCursorDateFound {
		st.Date = max(st.Date, notifySettingsCursorStateDate)
	}
	dateStreamUpdates := len(draftChanges) > 0 || len(unreadMarkChanges) > 0
	notifySettingsCursorNeedsDateAck := notifySettingsCursorStateFound && (dateStreamUpdates || draftMore || unreadMarkMore || len(secretChats) > 0)
	var chatRemovalResumeDate int
	if len(chatRemovalUpdates) > 0 {
		// Keep a logical Date for the other replay streams, then reserve a unique
		// removal token after all response cursors have selected their final Date.
		chatRemovalResumeDate = st.Date
		st.Date = max(st.Date, req.Date, chatRemovalStateDateTokenFloor-1) + 1
	}
	var notifySettingsCursorToSave *store.NotifySettingsDifferenceCursor
	if notifySettingsAuthKeyID != 0 && (notifySettingsMore || (notifySettingsCursorStateFound && (len(notifySettingsChanges) > 0 || notifySettingsCursorNeedsDateAck || len(chatRemovalUpdates) > 0))) {
		// Date acknowledges this page without consuming PTS/QTS. Keep its logical
		// value separately so the other date-based streams resume at their cursor.
		// Retain a cursor for the last page too; the following request acknowledges
		// it by presenting this returned state, while an old state replays safely.
		// Keep the mapping while date-stream or secret-chat replies are being acknowledged.
		ackDate := max(st.Date, requestDate)
		if notifySettingsCursorStateFound {
			ackDate = max(ackDate, notifySettingsCursorStateDate)
		}
		st.Date = ackDate + 1
		last := store.NotifySettingsDifferencePosition{
			ChangedAt: notifySettingsLastDelivered.ChangedAt,
			Key:       notifySettingsLastDelivered.Key,
		}
		if len(notifySettingsChanges) == 0 && notifySettingsCursorDateFound {
			last = notifySettingsCursor.Last
		}
		secretChatResumeAt := notifySettingsSecretChatResumeAt
		secretChatResumeID := notifySettingsSecretChatResumeID
		for _, secretChat := range secretChats {
			if !secretChatPositionAfter(secretChat, secretChatResumeAt, secretChatResumeID) {
				continue
			}
			secretChatResumeAt = secretChat.Date
			secretChatID := secretChat.ID
			secretChatResumeID = &secretChatID
		}
		cursor := store.NotifySettingsDifferenceCursor{
			AuthKeyID:              notifySettingsAuthKeyID,
			UserID:                 r.UserID,
			Cutoff:                 notifySettingsCutoff,
			ResumeDate:             resumeDate,
			SecretChatResumeAt:     secretChatResumeAt,
			SecretChatResumeID:     secretChatResumeID,
			SecretChatAcknowledged: notifySettingsSecretChatAcknowledged,
			SecretChatIssued:       secretChatDeliveryVersions(secretChats),
			Last:                   last,
			State:                  store.State{Pts: st.Pts, Qts: st.Qts, Seq: st.Seq, Date: st.Date},
		}
		notifySettingsCursorToSave = &cursor
	}
	if len(chatRemovalChatIDs) > 0 {
		if h.chatRemovalIssueHook != nil {
			h.chatRemovalIssueHook()
		}
		// The allocator upsert serializes same-key requests, and the removal
		// mappings commit before the response can be observed. A dropped reply
		// remains pending because only a later client state can acknowledge it.
		stateDate, issueErr := h.store.IssueChatRemovalEventsForAuthKey(
			r.Ctx, r.UserID, chatRemovalAuthKeyID, chatRemovalChatIDs,
			st.Date, st.Pts, st.Qts, chatRemovalResumeDate,
		)
		if issueErr != nil {
			h.log.Error("issue get difference chat removals", "user_id", r.UserID, "err", issueErr)
			return nil, nil, errInternal
		}
		st.Date = stateDate
	}
	if notifySettingsCursorToSave != nil {
		notifySettingsCursorToSave.State.Date = st.Date
		if err := h.store.SaveNotifySettingsDifferenceCursor(r.Ctx, *notifySettingsCursorToSave); err != nil {
			h.log.Error("save get difference notification settings cursor", "user_id", r.UserID, "err", err)
			return nil, nil, errInternal
		}
	}

	if b.more || encMore || adminMore || chatRemovalMore || draftMore || unreadMarkMore || notifySettingsMore {
		var afterReply func()
		if len(adminEventIDs) > 0 {
			afterReply = consumeAdminMarkers
		}
		return &tg.UpdatesDifferenceSlice{
			NewMessages:          newMessages,
			NewEncryptedMessages: encMsgs,
			OtherUpdates:         other,
			Users:                b.users,
			Chats:                b.chats,
			IntermediateState:    *stateToTL(st),
		}, afterReply, nil
	}
	result := &tg.UpdatesDifference{
		NewMessages:          newMessages,
		NewEncryptedMessages: encMsgs,
		OtherUpdates:         other,
		Users:                b.users,
		Chats:                b.chats,
		State:                *stateToTL(st),
	}
	var afterReply func()
	if len(adminEventIDs) > 0 || c != nil && includeFilterRefresh {
		afterReply = func() {
			consumeAdminMarkers()
			if c != nil && includeFilterRefresh {
				h.dialogFilterSync.AcknowledgeDifference(c, r, recovery, true)
			}
		}
	}
	return result, afterReply, nil
}
