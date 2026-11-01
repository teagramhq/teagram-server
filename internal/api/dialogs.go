package api

import (
	"context"
	"fmt"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

// createUsersForDialog fetches the participant list for a ChatActionCreate row
// when the viewer is still a member. A removed viewer gets nil, matching the
// empty user list getDifference serves for the same event.
func (h *handlers) createUsersForDialog(ctx context.Context, chatID, viewerID int64) ([]int64, error) {
	member, err := h.store.IsMember(ctx, chatID, viewerID)
	if err != nil {
		return nil, err
	}
	if !member {
		return nil, nil
	}
	parts, err := h.store.Participants(ctx, chatID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(parts))
	for i, p := range parts {
		ids[i] = p.UserID
	}
	return ids, nil
}

// maxChannelDialogs caps the channels one getDialogs reply carries.
//
// Channel dialogs are deliberately NOT paged: channels write no dialogs row, so
// there is no page key to offset them by. The account cap is 500 memberships,
// so listing the full set keeps a newly admitted channel replayable on a fresh
// pull while remaining bounded. Channel rows carry their member and peer data
// in one query. Paging this separate block would need a cursor clients do not
// have; it remains on the first page only.
const maxChannelDialogs = 500

// maxPeerDialogs bounds one messages.getPeerDialogs request and all of the
// selected-row and hydration work it can trigger.
const maxPeerDialogs = 100

// channelDialogs builds dialog entries and channel peers for the unbanned
// memberships of userID, returning the top posts so the caller can hydrate
// media for the whole reply in one query. New and backfilled channels use
// their channel-create service message as the top message until the first post.
//
// Single query via ChannelDialogsForUser replaces the previous per-channel
// ChannelHistory + ChannelState loop (2N queries).
func (h *handlers) channelDialogs(ctx context.Context, userID int64, excludePinned bool) ([]tg.ChatClass, []tg.DialogClass, []store.ChannelMessage, map[int64]bool, error) {
	rows, err := h.store.ChannelDialogsForUserWithPins(ctx, userID, excludePinned)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if len(rows) > maxChannelDialogs {
		rows = rows[:maxChannelDialogs]
	}

	chats := make([]tg.ChatClass, 0, len(rows))
	dialogs := make([]tg.DialogClass, 0, len(rows))
	tops := make([]store.ChannelMessage, 0, len(rows))
	broadcasts := make(map[int64]bool, len(rows))
	for _, r := range rows {
		broadcasts[r.Channel.ID] = !r.Channel.Megagroup
		d := &tg.Dialog{
			Peer:           &tg.PeerChannel{ChannelID: r.Channel.ID},
			ReadInboxMaxID: int(r.ReadInboxMaxID),
			UnreadCount:    r.UnreadCount,
			Pinned:         r.Pinned,
		}
		d.SetFlags()
		if r.Top != nil {
			d.TopMessage = int(r.Top.LocalID)
			tops = append(tops, *r.Top)
		}
		d.SetPts(r.Pts)
		dialogs = append(dialogs, d)
		chats = append(chats, h.channelToTL(r.Channel, r.Member, true, userID))
	}
	return chats, dialogs, tops, broadcasts, nil
}

// handleGetDialogs serves messages.getDialogs: the caller's conversation list
// with each dialog's top message and the referenced peer users.
func (h *handlers) handleGetDialogs(r *mtproto.Request) (bin.Encoder, error) {
	profile := startGetDialogsProfiler()
	defer profile.finish()

	var req tg.MessagesGetDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	offsetPeerType, offsetPeerID, hasOffsetPeer, err := h.globalCursorPeer(req.OffsetPeer, r.UserID)
	if err != nil {
		return nil, err
	}
	firstPage := req.OffsetID == 0 && !hasOffsetPeer
	if firstPage {
		if err := h.checkChannelUnreadCountRateLimit(r); err != nil {
			return nil, err
		}
	}
	profile.mark("request validation and rate limit")

	limit := req.Limit
	if limit <= 0 {
		limit = defaultDialogsLimit
	}
	if limit > maxDialogsLimit {
		limit = maxDialogsLimit
	}

	// top_message is not a total order: clearing a dialog sets it to zero, and
	// multiple dialogs can otherwise share an id. Use the echoed offset peer to
	// advance through those ties in the same deterministic order as the query.
	dialogs, err := h.store.DialogsWithPins(r.Ctx, r.UserID, int64(req.OffsetID), offsetPeerType, offsetPeerID, limit, req.ExcludePinned)
	if err != nil {
		h.log.Error("get dialogs", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("dialog page query")

	tlDialogs := make([]tg.DialogClass, 0, len(dialogs))
	// Top messages are collected first and mapped after the loop, so the whole
	// page's media is hydrated in one query rather than one per dialog.
	tops := make([]store.Message, 0, len(dialogs))
	topCreateUsers := make([][]int64, 0, len(dialogs))
	peerIDs := map[int64]bool{r.UserID: true}
	chatIDs := map[int64]bool{}
	topIDs := make([]int64, len(dialogs))
	for i, d := range dialogs {
		topIDs[i] = d.TopMessage
	}
	topMessages, err := h.store.MessagesByOwnerLocalIDs(r.Ctx, r.UserID, topIDs)
	if err != nil {
		h.log.Error("get dialogs top messages", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("top message query")
	for _, d := range dialogs {
		dialog := &tg.Dialog{
			Peer:            peerToTL(d.PeerType, d.PeerID),
			TopMessage:      int(d.TopMessage),
			ReadInboxMaxID:  int(d.ReadInboxMaxID),
			ReadOutboxMaxID: int(d.ReadOutboxMaxID),
			UnreadCount:     d.UnreadCount,
			Pinned:          d.Pinned,
		}
		dialog.SetFlags()
		tlDialogs = append(tlDialogs, dialog)
		if d.PeerType == store.PeerTypeChat {
			chatIDs[d.PeerID] = true
		} else {
			peerIDs[d.PeerID] = true
		}

		m, ok := topMessages[d.TopMessage]
		if ok && !m.Deleted {
			// The author is the peer in a 1:1 but any member in a group, so it is
			// taken off the message rather than off the dialog's peer id.
			peerIDs[m.FromID] = true
			var createUsers []int64
			switch m.Action {
			case store.ChatActionAddUser, store.ChatActionDeleteUser:
				// ActionUserID is stored on the viewer's own row, so no membership
				// gate is needed — the row exists because fan-out wrote it here.
				peerIDs[m.ActionUserID] = true
			case store.ChatActionCreate:
				cu, cerr := h.createUsersForDialog(r.Ctx, m.PeerID, r.UserID)
				if cerr != nil {
					h.log.Error("get dialogs create users", "user_id", r.UserID, "err", cerr)
					return nil, errInternal
				}
				createUsers = cu
				for _, id := range cu {
					peerIDs[id] = true
				}
			}
			tops = append(tops, m)
			topCreateUsers = append(topCreateUsers, createUsers)
		} else {
			// A stale dialog cursor must not make a soft-deleted row visible.
			dialog.TopMessage = 0
		}
	}
	profile.mark("dialog and peer assembly")

	files, err := h.loadFiles(r.Ctx, tops)
	if err != nil {
		h.log.Error("get dialogs files", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("top message files")
	pollViews, err := h.pollViewsForMessages(r.Ctx, r.UserID, tops)
	if err != nil {
		h.log.Error("get dialogs polls", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("top message polls")
	tlMsgs := make([]tg.MessageClass, 0, len(tops))
	for i, m := range tops {
		if poll, ok := pollViews[m.LocalID]; ok {
			tlMessage, pollErr := messageToTLWithPoll(m, topCreateUsers[i], files, nil, nil, poll)
			if pollErr != nil {
				h.log.Error("render dialog poll description", "user_id", r.UserID, "local_id", m.LocalID, "err", pollErr)
				return nil, errInternal
			}
			tlMsgs = append(tlMsgs, tlMessage)
		} else {
			tlMsgs = append(tlMsgs, h.messageToTL(m, topCreateUsers[i], files, nil, nil, h.dcID))
		}
	}
	profile.mark("top message rendering")

	// Channels write no dialogs row — they keep one message row per channel, not
	// one per member — so the caller's channels are prepended to the paged rows
	// rather than read out of the page. Prepend (not append) is deliberate: the
	// client derives its next offset_id from the last dialog's top_message, and a
	// channel's top_message lives in a different id space (channel local_id vs
	// dialogs local_id). By placing channels first, the last entry is always a
	// dialogs row, so the cursor stays valid.
	//
	// First page only: the block is not part of the paged sequence, so repeating
	// it on every page would hand a client that pages to the end one copy per
	// page. A zero top_message can still be a later page, so the peer cursor also
	// distinguishes it from the initial request.
	var channelPeers []tg.ChatClass
	var channelDialogs []tg.DialogClass
	if firstPage {
		peers, ds, channelTops, channelBroadcasts, cerr := h.channelDialogs(r.Ctx, r.UserID, req.ExcludePinned)
		if cerr != nil {
			h.log.Error("get dialogs channels", "user_id", r.UserID, "err", cerr)
			return nil, errInternal
		}
		profile.mark("channel dialog query and hydration")
		if cerr = h.attachChannelPollViewsAcross(r.Ctx, r.UserID, channelTops); cerr != nil {
			h.log.Error("get dialogs channel polls", "user_id", r.UserID, "err", cerr)
			return nil, errInternal
		}
		profile.mark("channel poll hydration")
		channelFiles, ferr := h.loadChannelFiles(r.Ctx, channelTops)
		if ferr != nil {
			h.log.Error("get dialogs channel files", "user_id", r.UserID, "err", ferr)
			return nil, errInternal
		}
		profile.mark("channel file hydration")
		channelPeers, channelDialogs = peers, ds
		tlDialogs = append(channelDialogs, tlDialogs...)
		channelMsgs := make([]tg.MessageClass, 0, len(channelTops)+len(tlMsgs))
		for _, m := range channelTops {
			message, renderErr := h.channelMessageToTL(m, r.UserID, channelFiles, channelBroadcasts[m.ChannelID], h.dcID)
			if renderErr != nil {
				h.log.Error("render channel dialog poll description", "user_id", r.UserID, "channel_id", m.ChannelID, "local_id", m.LocalID, "err", renderErr)
				return nil, errInternal
			}
			channelMsgs = append(channelMsgs, message)
			if !channelBroadcasts[m.ChannelID] {
				peerIDs[m.FromID] = true
			}
		}
		tlMsgs = append(channelMsgs, tlMsgs...)
		profile.mark("channel message rendering")
	}
	if err := h.attachDialogDrafts(r.Ctx, r.UserID, tlDialogs); err != nil {
		h.log.Error("get dialogs drafts", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("draft attachment")
	if err := h.attachDialogUnreadMarks(r.Ctx, r.UserID, tlDialogs); err != nil {
		h.log.Error("get dialogs unread marks", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("unread mark attachment")
	if err := h.attachDialogNotifySettings(r.Ctx, r.UserID, tlDialogs, channelPeers); err != nil {
		h.log.Error("get dialogs notify settings", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("notification settings attachment")

	users, err := h.loadUsers(r.Ctx, peerIDs, r.UserID)
	if err != nil {
		h.log.Error("get dialogs users", "err", err)
		return nil, errInternal
	}
	profile.mark("user loading and rendering")
	// No membership check on the list itself: a dialog row exists only because a
	// fan-out wrote it for this owner, so an attacker-chosen id never reaches here.
	// A removed member keeps their dialog row by design, which is why the viewer is
	// passed down — loadChats degrades those to tg.ChatForbidden, which carries
	// the id and an empty title and nothing else — no live title, version or
	// participant count reaches a removed member.
	if h.getDialogsBeforeLoadChatsHook != nil {
		h.getDialogsBeforeLoadChatsHook()
	}
	chats, err := h.loadChats(r.Ctx, chatIDs, r.UserID)
	if err != nil {
		h.log.Error("get dialogs chats", "err", err)
		return nil, errInternal
	}
	profile.mark("chat loading")
	chats = append(chats, channelPeers...)
	// Read the exact total on every page so short and full pages issue the same
	// statements. The maintained count makes this lookup independent of list size.
	total, err := h.store.CountDialogsWithPins(r.Ctx, r.UserID, req.ExcludePinned)
	if err != nil {
		h.log.Error("get dialogs count", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	profile.mark("dialog count query")
	if len(dialogs) < limit {
		return &tg.MessagesDialogs{Dialogs: tlDialogs, Messages: tlMsgs, Users: users, Chats: chats}, nil
	}
	// CountDialogs counts dialogs rows, and channels have none, so the appended
	// block has to be added or this page would advertise a total smaller than the
	// list it is shipping. channelDialogs is empty on every page but the first,
	// where the block is not shipped either, so no page ever ships more rows than
	// it counts. The count does still shrink by the channel count after the first
	// page, which is the honest reading of a block that is served once and not
	// paged; making it constant would mean paying the per-channel reads on every
	// page purely to count them.
	total += len(channelDialogs)
	return &tg.MessagesDialogsSlice{Count: total, Dialogs: tlDialogs, Messages: tlMsgs, Users: users, Chats: chats}, nil
}

// handleGetPeerDialogs serves messages.getPeerDialogs. Unlike getDialogs, the
// request names an exact peer set; missing or unauthorized peers are omitted
// from the same read snapshot rather than answered with a distinguishable
// existence error.
func (h *handlers) handleGetPeerDialogs(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.MessagesGetPeerDialogsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if len(req.Peers) == 0 {
		return nil, errInputPeersEmpty
	}
	if len(req.Peers) > maxPeerDialogs {
		return nil, errLimitInvalid
	}

	peers := make([]store.PeerDialogKey, 0, len(req.Peers))
	seen := make(map[store.PeerDialogKey]bool, len(req.Peers))
	for _, input := range req.Peers {
		peer, ok := input.(*tg.InputDialogPeer)
		if !ok || peer.Peer == nil {
			return nil, errPeerIDInvalid
		}
		peerType, peerID, err := h.inputPeer(peer.Peer, r.UserID)
		if err != nil {
			return nil, err
		}
		key := store.PeerDialogKey{PeerType: peerType, PeerID: peerID}
		if seen[key] {
			continue
		}
		seen[key] = true
		peers = append(peers, key)
	}
	if err := h.checkChannelUnreadCountRateLimit(r); err != nil {
		return nil, err
	}

	snapshot, err := h.store.PeerDialogsSnapshot(r.Ctx, r.UserID, peers)
	if err != nil {
		h.log.Error("get peer dialogs", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	result, err := h.peerDialogsToTL(r.Ctx, snapshot, r.UserID)
	if err != nil {
		h.log.Error("get peer dialogs: contact state", "user_id", r.UserID, "err", err)
		return nil, errInternal
	}
	return result, nil
}

// peerDialogsToTL is the response-only half of the snapshot path. Row-derived
// users remain behind the viewer-aware entitlement gate; explicitly requested
// user peers were admitted by their validated access hashes. It reads contact
// state for only those users that will be rendered as full users.
func (h *handlers) peerDialogsToTL(ctx context.Context, snapshot store.PeerDialogsSnapshot, viewerID int64) (*tg.MessagesPeerDialogs, error) {
	tlDialogs := make([]tg.DialogClass, 0, len(snapshot.Dialogs))
	tlMsgs := make([]tg.MessageClass, 0, len(snapshot.Dialogs))
	selectedMessages := make([]store.Message, 0, len(snapshot.Dialogs))
	selectedChannelMessages := make([]store.ChannelMessage, 0, len(snapshot.Dialogs))
	for _, selected := range snapshot.Dialogs {
		if selected.Message != nil {
			selectedMessages = append(selectedMessages, *selected.Message)
		}
		if selected.ChannelMessage != nil {
			selectedChannelMessages = append(selectedChannelMessages, *selected.ChannelMessage)
		}
	}
	media, err := h.messageReadMedia(ctx, snapshot.Files, selectedMessages, selectedChannelMessages)
	if err != nil {
		return nil, err
	}
	pollViews, err := h.pollViewsForMessages(ctx, viewerID, selectedMessages)
	if err != nil {
		return nil, err
	}
	for i := range snapshot.Dialogs {
		selected := &snapshot.Dialogs[i]
		if selected.ChannelMessage == nil {
			continue
		}
		channelMessages := []store.ChannelMessage{*selected.ChannelMessage}
		if err = h.attachChannelPollViews(ctx, viewerID, selected.ChannelMessage.ChannelID, channelMessages); err != nil {
			return nil, err
		}
		*selected.ChannelMessage = channelMessages[0]
	}
	chatIDs := make([]int64, 0, len(snapshot.Dialogs))
	channelIDs := make([]int64, 0, len(snapshot.Dialogs))
	seenChats := map[int64]bool{}
	seenChannels := map[int64]bool{}

	for _, selected := range snapshot.Dialogs {
		d := selected.Dialog
		tlDialog := &tg.Dialog{
			Peer:            peerToTL(d.PeerType, d.PeerID),
			TopMessage:      int(d.TopMessage),
			ReadInboxMaxID:  int(d.ReadInboxMaxID),
			ReadOutboxMaxID: int(d.ReadOutboxMaxID),
			UnreadCount:     d.UnreadCount,
		}
		if draft, ok := snapshot.CloudDrafts[store.PeerDialogKey{PeerType: d.PeerType, PeerID: d.PeerID}]; ok {
			tlDialog.SetDraft(cloudDraftToTL(draft, true, draft.UpdatedAt))
		}
		switch d.PeerType {
		case store.PeerTypeChannel:
			tlDialog.SetPts(selected.Pts)
			if !seenChannels[d.PeerID] {
				seenChannels[d.PeerID] = true
				channelIDs = append(channelIDs, d.PeerID)
			}
		case store.PeerTypeChat:
			if !seenChats[d.PeerID] {
				seenChats[d.PeerID] = true
				chatIDs = append(chatIDs, d.PeerID)
			}
		}
		tlDialogs = append(tlDialogs, tlDialog)

		switch {
		case selected.Message != nil:
			var createUsers []int64
			if selected.Message.Action == store.ChatActionCreate && snapshot.ChatMembership[d.PeerID] {
				for _, participant := range snapshot.ChatMembers[d.PeerID] {
					createUsers = append(createUsers, participant.UserID)
				}
			}
			if poll, ok := pollViews[selected.Message.LocalID]; ok {
				tlMessage, pollErr := messageToTLWithPoll(*selected.Message, createUsers, media, nil, nil, poll)
				if pollErr != nil {
					return nil, pollErr
				}
				tlMsgs = append(tlMsgs, tlMessage)
			} else {
				tlMsgs = append(tlMsgs, h.messageToTL(*selected.Message, createUsers, media, nil, nil, h.dcID))
			}
		case selected.ChannelMessage != nil:
			channel := snapshot.Channels[d.PeerID]
			tlMessage, renderErr := h.channelMessageToTL(*selected.ChannelMessage, viewerID, media, !channel.Megagroup, h.dcID)
			if renderErr != nil {
				return nil, renderErr
			}
			tlMsgs = append(tlMsgs, tlMessage)
		}
	}

	userIDs := make([]int64, 0, len(snapshot.Users))
	for id := range snapshot.Users {
		if id == viewerID || snapshot.EntitledUsers[id] || snapshot.ExplicitUserPeers[id] {
			userIDs = append(userIDs, id)
		}
	}
	contactStates, err := h.contactStatesForUsers(ctx, viewerID, userIDs)
	if err != nil {
		return nil, err
	}
	visibleUsers := make([]store.User, 0, len(userIDs))
	for _, id := range userIDs {
		visibleUsers = append(visibleUsers, snapshot.Users[id])
	}
	visibility, err := h.lastSeenVisibilityForUsers(ctx, visibleUsers, viewerID)
	if err != nil {
		return nil, err
	}
	users := make([]tg.UserClass, 0, len(snapshot.Users))
	for id, user := range snapshot.Users {
		if id != viewerID && !snapshot.EntitledUsers[id] && !snapshot.ExplicitUserPeers[id] {
			users = append(users, &tg.UserEmpty{ID: id})
			continue
		}
		users = append(users, h.userToTL(user, viewerID, id == viewerID, contactStates[id], visibility[id]))
	}

	chats := make([]tg.ChatClass, 0, len(chatIDs)+len(channelIDs))
	for _, chatID := range chatIDs {
		chat, ok := snapshot.Chats[chatID]
		if !ok {
			continue
		}
		if !snapshot.ChatMembership[chatID] {
			chats = append(chats, &tg.ChatForbidden{ID: chat.ID, Title: ""})
			continue
		}
		wireChat, err := h.chatToTL(ctx, chat, len(snapshot.ChatMembers[chatID]), viewerID)
		if err != nil {
			return nil, fmt.Errorf("render dialog chat %d: %w", chatID, err)
		}
		chats = append(chats, wireChat)
	}
	for _, channelID := range channelIDs {
		channel, ok := snapshot.Channels[channelID]
		if !ok {
			continue
		}
		chats = append(chats, h.channelToTL(channel, snapshot.ChannelMembers[channelID], true, viewerID))
	}
	if err = h.attachDialogUnreadMarks(ctx, viewerID, tlDialogs); err != nil {
		return nil, err
	}
	if err = h.attachDialogNotifySettings(ctx, viewerID, tlDialogs, chats); err != nil {
		return nil, err
	}

	return &tg.MessagesPeerDialogs{
		Dialogs:  tlDialogs,
		Messages: tlMsgs,
		Chats:    chats,
		Users:    users,
		State:    *stateToTL(snapshot.State),
	}, nil
}
