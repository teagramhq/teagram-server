package api

import (
	"context"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestGetAllDraftsReturnsOnlyCurrentOwnerVisibleDrafts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openGetAllDraftsTestStore(t)

	owner, err := s.CreateUser(ctx, "+15551234581")
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	peer, err := s.CreateUser(ctx, "+15551234582")
	if err != nil {
		t.Fatalf("create user peer: %v", err)
	}
	otherOwner, err := s.CreateUser(ctx, "+15551234583")
	if err != nil {
		t.Fatalf("create other owner: %v", err)
	}

	seed, _, _, duplicate, err := s.SendMessage(ctx, owner.ID, peer.ID, "reply target", 1234581, 0, 0)
	if err != nil || duplicate {
		t.Fatalf("seed user dialog: duplicate=%v err=%v", duplicate, err)
	}
	if _, _, _, duplicate, err = s.SendMessage(ctx, otherOwner.ID, peer.ID, "other dialog", 1234582, 0, 0); err != nil || duplicate {
		t.Fatalf("seed other owner's user dialog: duplicate=%v err=%v", duplicate, err)
	}

	save := func(ownerID int64, peerType store.PeerType, peerID int64, text string, noWebpage bool, replyID int64) time.Time {
		t.Helper()
		changed, stamp, saveErr := s.SaveCloudDraft(ctx, ownerID, peerType, peerID, text, noWebpage, replyID)
		if saveErr != nil || !changed {
			t.Fatalf("save cloud draft for %d/%d: changed=%v err=%v", peerType, peerID, changed, saveErr)
		}
		return stamp
	}
	userDraftDate := save(owner.ID, store.PeerTypeUser, peer.ID, "user draft", true, seed.LocalID)
	save(otherOwner.ID, store.PeerTypeUser, peer.ID, "private other-owner draft", false, 0)

	group, err := s.CreateChat(ctx, owner.ID, "visible draft group", nil)
	if err != nil {
		t.Fatalf("create visible group: %v", err)
	}
	save(owner.ID, store.PeerTypeChat, group.ID, "group draft", false, 0)
	channel, err := s.CreateChannel(ctx, owner.ID, "visible draft channel", "", true)
	if err != nil {
		t.Fatalf("create visible channel: %v", err)
	}
	save(owner.ID, store.PeerTypeChannel, channel.ID, "channel draft", false, 0)

	removedGroup, err := s.CreateChat(ctx, otherOwner.ID, "removed draft group", []int64{owner.ID})
	if err != nil {
		t.Fatalf("create removed group: %v", err)
	}
	save(owner.ID, store.PeerTypeChat, removedGroup.ID, "removed group draft", false, 0)
	if removed, _, _, removeErr := s.RemoveChatUser(ctx, removedGroup.ID, owner.ID, otherOwner.ID); removeErr != nil || !removed {
		t.Fatalf("remove owner from group: removed=%v err=%v", removed, removeErr)
	}

	deletedGroup, err := s.CreateChat(ctx, owner.ID, "deleted draft group", nil)
	if err != nil {
		t.Fatalf("create deleted group: %v", err)
	}
	save(owner.ID, store.PeerTypeChat, deletedGroup.ID, "deleted group draft", false, 0)
	if _, err = s.DeleteChat(ctx, deletedGroup.ID, owner.ID); err != nil {
		t.Fatalf("delete group with stored draft: %v", err)
	}

	createJoinedChannel := func(title string) store.Channel {
		t.Helper()
		ch, createErr := s.CreateChannel(ctx, otherOwner.ID, title, "", true)
		if createErr != nil {
			t.Fatalf("create %s: %v", title, createErr)
		}
		invite, inviteErr := s.CreateChannelInvite(ctx, ch.ID, otherOwner.ID)
		if inviteErr != nil {
			t.Fatalf("create %s invite: %v", title, inviteErr)
		}
		if _, _, joinErr := s.JoinChannelByInvite(ctx, invite, owner.ID); joinErr != nil {
			t.Fatalf("join %s: %v", title, joinErr)
		}
		return ch
	}
	bannedChannel := createJoinedChannel("banned draft channel")
	save(owner.ID, store.PeerTypeChannel, bannedChannel.ID, "banned channel draft", false, 0)
	banUntil := time.Now().Add(time.Hour)
	if err = s.SetChannelBan(ctx, bannedChannel.ID, otherOwner.ID, owner.ID, &banUntil, false); err != nil {
		t.Fatalf("ban owner from channel: %v", err)
	}
	leftChannel := createJoinedChannel("left draft channel")
	save(owner.ID, store.PeerTypeChannel, leftChannel.ID, "left channel draft", false, 0)
	if left, leaveErr := s.LeaveChannel(ctx, leftChannel.ID, owner.ID); leaveErr != nil || !left {
		t.Fatalf("leave channel: left=%v err=%v", left, leaveErr)
	}

	var request bin.Buffer
	if err := (&tg.MessagesGetAllDraftsRequest{}).Encode(&request); err != nil {
		t.Fatalf("encode getAllDrafts request: %v", err)
	}
	response, err := testHandlers(s).handleGetAllDrafts(&mtproto.Request{Ctx: ctx, UserID: owner.ID, Buf: &request})
	if err != nil {
		t.Fatalf("getAllDrafts: %v", err)
	}
	updates, ok := response.(*tg.Updates)
	if !ok {
		t.Fatalf("getAllDrafts response = %T, want *tg.Updates", response)
	}

	wantDrafts := map[string]bool{
		"user:user draft":       false,
		"chat:group draft":      false,
		"channel:channel draft": false,
	}
	wantPeerIDs := map[string]int64{
		"user:user draft":       peer.ID,
		"chat:group draft":      group.ID,
		"channel:channel draft": channel.ID,
	}
	for _, raw := range updates.Updates {
		update, ok := raw.(*tg.UpdateDraftMessage)
		if !ok {
			t.Fatalf("update = %T, want *tg.UpdateDraftMessage", raw)
		}
		draft, ok := update.Draft.(*tg.DraftMessage)
		if !ok {
			t.Fatalf("draft = %T, want *tg.DraftMessage", update.Draft)
		}
		var key string
		var peerID int64
		switch peer := update.Peer.(type) {
		case *tg.PeerUser:
			key = "user:" + draft.Message
			peerID = peer.UserID
		case *tg.PeerChat:
			key = "chat:" + draft.Message
			peerID = peer.ChatID
		case *tg.PeerChannel:
			key = "channel:" + draft.Message
			peerID = peer.ChannelID
		default:
			t.Fatalf("draft peer = %T, want user, chat, or channel", update.Peer)
		}
		seen, expected := wantDrafts[key]
		if !expected || seen {
			t.Fatalf("unexpected or duplicate draft update %q for %T", draft.Message, update.Peer)
		}
		if peerID != wantPeerIDs[key] {
			t.Errorf("draft %q peer id = %d, want %d", draft.Message, peerID, wantPeerIDs[key])
		}
		wantDrafts[key] = true
		if draft.Date <= 0 {
			t.Errorf("draft %q date = %d, want persisted date", draft.Message, draft.Date)
		}
		if draft.Message == "user draft" {
			reply, exists := draft.GetReplyTo()
			message, replyOK := reply.(*tg.InputReplyToMessage)
			if !exists || !replyOK || message.ReplyToMsgID != int(seed.LocalID) || !draft.NoWebpage || draft.Date != int(userDraftDate.Unix()) {
				t.Errorf("user draft = %+v, want no_webpage, reply %d, date %d", draft, seed.LocalID, userDraftDate.Unix())
			}
		}
	}
	for key, seen := range wantDrafts {
		if !seen {
			t.Errorf("missing draft update %q", key)
		}
	}
	if len(updates.Updates) != len(wantDrafts) {
		t.Errorf("draft updates = %d, want %d", len(updates.Updates), len(wantDrafts))
	}

	userIDs := make(map[int64]bool)
	for _, raw := range updates.Users {
		switch user := raw.(type) {
		case *tg.User:
			userIDs[user.ID] = true
		case *tg.UserEmpty:
			userIDs[user.ID] = true
		}
	}
	if !userIDs[owner.ID] || !userIDs[peer.ID] || userIDs[otherOwner.ID] {
		t.Errorf("users = %v, want caller and user peer only", userIDs)
	}

	chatIDs := make(map[int64]bool)
	fullPeerIDs := make(map[int64]bool)
	for _, raw := range updates.Chats {
		switch chat := raw.(type) {
		case *tg.Chat:
			chatIDs[chat.ID] = true
			fullPeerIDs[chat.ID] = true
		case *tg.ChatForbidden:
			chatIDs[chat.ID] = true
		case *tg.Channel:
			chatIDs[chat.ID] = true
			fullPeerIDs[chat.ID] = true
		case *tg.ChannelForbidden:
			chatIDs[chat.ID] = true
		}
	}
	if len(chatIDs) != 2 || !chatIDs[group.ID] || !chatIDs[channel.ID] || !fullPeerIDs[group.ID] || !fullPeerIDs[channel.ID] {
		t.Errorf("chats = %v, want only current group %d and channel %d", chatIDs, group.ID, channel.ID)
	}
	for _, inaccessible := range []int64{removedGroup.ID, deletedGroup.ID, bannedChannel.ID, leftChannel.ID} {
		if chatIDs[inaccessible] {
			t.Errorf("getAllDrafts returned inaccessible chat %d", inaccessible)
		}
	}
}

func openGetAllDraftsTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := pgtest.DSN(t)
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
	}
	s, err := store.Open(context.Background(), dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}
