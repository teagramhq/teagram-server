package e2e_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestCloudDrafts(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049101", "+15551049102"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "cloud draft owner", phoneA)
	a2 := newSmokeClient(t, f, "cloud draft owner second session", phoneA)
	b := dialogPinUser(t, f, phoneB)
	seedDialogPinDM(t, f, a1.id, b.ID, 1049101)
	target, _, _, duplicate, err := f.store.SendMessage(f.ctx, b.ID, a1.id, "draft reply target", 1049102, 0, 0)
	if err != nil {
		t.Fatalf("seed reply target: %v", err)
	}
	if duplicate {
		t.Fatal("seed reply target unexpectedly deduplicated")
	}
	stateBefore, err := f.store.StateWithoutChannelUnread(f.ctx, a1.id)
	if err != nil {
		t.Fatalf("read initial update state: %v", err)
	}

	var saved bool
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesSaveDraftRequest{
			Peer:      peerUser(a1.id, b.ID),
			Message:   "draft A",
			NoWebpage: true,
		}
		reply := &tg.InputReplyToMessage{ReplyToMsgID: int(target.LocalID)}
		reply.SetReplyToPeerID(peerUser(a1.id, b.ID))
		request.SetReplyTo(reply)
		request.SetFlags()
		var err error
		saved, err = api.MessagesSaveDraft(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("save cloud draft: %v", err)
	}
	if !saved {
		t.Fatal("messages.saveDraft returned BoolFalse, want BoolTrue")
	}
	stateAfter, err := f.store.StateWithoutChannelUnread(f.ctx, a1.id)
	if err != nil {
		t.Fatalf("read update state after draft: %v", err)
	}
	if stateAfter.Pts != stateBefore.Pts || stateAfter.Qts != stateBefore.Qts || stateAfter.Seq != stateBefore.Seq {
		t.Fatalf("draft changed update state from %+v to %+v", stateBefore, stateAfter)
	}
	for i := range int64(501) {
		message, senderPts, recipientPts, duplicate, sendErr := f.store.SendMessage(f.ctx, a1.id, a1.id, "cap event", 1049200+i, 0, 0)
		if sendErr != nil {
			t.Fatalf("seed pts event %d for recovery cap: %v", i, sendErr)
		}
		if message.LocalID <= 0 || senderPts != recipientPts || duplicate {
			t.Fatalf("seed pts event %d result = id %d pts %d/%d duplicate=%v", i, message.LocalID, senderPts, recipientPts, duplicate)
		}
	}
	var difference tg.UpdatesDifferenceClass
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		difference, err = api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: stateBefore.Pts, Qts: stateBefore.Qts, Date: 0})
		return err
	}); err != nil {
		t.Fatalf("getDifference draft recovery: %v", err)
	}
	assertCloudDraftDifference(t, difference, b.ID, "draft A", false)

	update := recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner UpdateDraftMessage")
	assertCloudDraftUpdate(t, update, b.ID, "draft A", true, int(target.LocalID))

	listed, err := getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs: %v", err)
	}
	assertCloudDraftInDialogs(t, listed, b.ID, "draft A", true, int(target.LocalID))

	var peerDialogs *tg.MessagesPeerDialogs
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		peerDialogs, err = api.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
			&tg.InputDialogPeer{Peer: peerUser(a2.id, b.ID)},
		})
		return err
	}); err != nil {
		t.Fatalf("getPeerDialogs: %v", err)
	}
	assertCloudDraftInDialogs(t, &tg.MessagesDialogs{Dialogs: peerDialogs.Dialogs}, b.ID, "draft A", true, int(target.LocalID))

	// A's recipient owns an independent draft for the same dialog.
	bClient := newSmokeClient(t, f, "cloud draft other owner", phoneB)
	if err := bClient.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesSaveDraftRequest{Peer: peerUser(bClient.id, a1.id), Message: "draft B"}
		request.SetFlags()
		_, err := api.MessagesSaveDraft(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("save other owner's draft: %v", err)
	}
	_ = recvOrCtx(t, f.ctx, bClient.push.drafts, "other owner's draft update")
	select {
	case unexpected := <-a2.push.drafts:
		t.Fatalf("other owner's draft reached A's session: %+v", unexpected)
	case <-time.After(200 * time.Millisecond):
	}

	replyOnly := &tg.MessagesSaveDraftRequest{Peer: peerUser(a1.id, b.ID)}
	replyOnly.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(target.LocalID)})
	if err := saveCloudDraft(a1, f.ctx, replyOnly); err != nil {
		t.Fatalf("save reply-only draft: %v", err)
	}
	assertCloudDraftUpdate(t, recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner reply-only draft"), b.ID, "", false, int(target.LocalID))
	listed, err = getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs with reply-only draft: %v", err)
	}
	assertCloudDraftInDialogs(t, listed, b.ID, "", false, int(target.LocalID))

	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		saved, err = api.MessagesSaveDraft(ctx, &tg.MessagesSaveDraftRequest{Peer: peerUser(a1.id, b.ID)})
		return err
	}); err != nil {
		t.Fatalf("clear cloud draft: %v", err)
	}
	if !saved {
		t.Fatal("clearing cloud draft returned BoolFalse, want BoolTrue")
	}
	clearUpdate := recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner draft clear")
	if _, ok := clearUpdate.Draft.(*tg.DraftMessageEmpty); !ok {
		t.Fatalf("cleared draft update = %T, want *tg.DraftMessageEmpty", clearUpdate.Draft)
	}
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		saved, err = api.MessagesSaveDraft(ctx, &tg.MessagesSaveDraftRequest{Peer: peerUser(a1.id, b.ID)})
		return err
	}); err != nil || !saved {
		t.Fatalf("clearing absent cloud draft: BoolTrue=%v err=%v", saved, err)
	}
	select {
	case unexpected := <-a2.push.drafts:
		t.Fatalf("clearing absent draft emitted an update: %+v", unexpected)
	case <-time.After(200 * time.Millisecond):
	}
	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		difference, err = api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: stateBefore.Pts, Qts: stateBefore.Qts, Date: 0})
		return err
	}); err != nil {
		t.Fatalf("getDifference cleared draft recovery: %v", err)
	}
	assertCloudDraftDifference(t, difference, b.ID, "", true)
	cleared, err := getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after clear: %v", err)
	}
	assertNoCloudDraft(t, cleared, b.ID)

	var otherDialogs *tg.MessagesPeerDialogs
	if err := bClient.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		otherDialogs, err = api.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
			&tg.InputDialogPeer{Peer: peerUser(bClient.id, a1.id)},
		})
		return err
	}); err != nil {
		t.Fatalf("read other owner's draft: %v", err)
	}
	assertCloudDraftInDialogs(t, &tg.MessagesDialogs{Dialogs: otherDialogs.Dialogs}, a1.id, "draft B", false, 0)

	group, err := f.store.CreateChat(f.ctx, b.ID, "draft visibility group", []int64{a1.id})
	if err != nil {
		t.Fatalf("create draft visibility group: %v", err)
	}
	if _, _, _, err = f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: group.ID, FromID: b.ID, Text: "group dialog seed", RandomID: 1049122,
	}); err != nil {
		t.Fatalf("seed group dialog: %v", err)
	}
	if err = saveCloudDraft(a1, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: &tg.InputPeerChat{ChatID: group.ID}, Message: "group draft",
	}); err != nil {
		t.Fatalf("save group draft: %v", err)
	}
	assertCloudDraftUpdateForPeer(t, recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner group draft push"), &tg.PeerChat{ChatID: group.ID}, "group draft")
	listed, err = getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs with group draft: %v", err)
	}
	assertPeerDraftInDialogs(t, listed, &tg.PeerChat{ChatID: group.ID}, "group draft")
	if removed, _, _, removeErr := f.store.RemoveChatUser(f.ctx, group.ID, a1.id, b.ID); removeErr != nil || !removed {
		t.Fatalf("remove draft owner from group: removed=%v err=%v", removed, removeErr)
	}
	listed, err = getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after group removal: %v", err)
	}
	assertNoPeerDraftInDialogs(t, listed, &tg.PeerChat{ChatID: group.ID})
	drafts, err := f.store.CloudDraftsForPeers(f.ctx, a1.id, []store.PeerDialogKey{{PeerType: store.PeerTypeChat, PeerID: group.ID}})
	if err != nil || len(drafts) != 0 {
		t.Fatalf("removed group member draft read = %+v, err=%v, want hidden", drafts, err)
	}

	channel, err := f.store.CreateChannel(f.ctx, b.ID, "draft visibility channel", "", true)
	if err != nil {
		t.Fatalf("create draft visibility channel: %v", err)
	}
	invite, err := f.store.CreateChannelInvite(f.ctx, channel.ID, b.ID)
	if err != nil {
		t.Fatalf("create draft visibility invite: %v", err)
	}
	if _, _, err = f.store.JoinChannelByInvite(f.ctx, invite, a1.id); err != nil {
		t.Fatalf("join draft visibility channel: %v", err)
	}
	if err = saveCloudDraft(a1, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: peerChannel(a1.id, channel.ID), Message: "channel draft",
	}); err != nil {
		t.Fatalf("save channel draft: %v", err)
	}
	assertCloudDraftUpdateForPeer(t, recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner channel draft push"), &tg.PeerChannel{ChannelID: channel.ID}, "channel draft")
	listed, err = getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs with channel draft: %v", err)
	}
	assertPeerDraftInDialogs(t, listed, &tg.PeerChannel{ChannelID: channel.ID}, "channel draft")
	if left, leaveErr := f.store.LeaveChannel(f.ctx, channel.ID, a1.id); leaveErr != nil || !left {
		t.Fatalf("remove draft owner from channel: left=%v err=%v", left, leaveErr)
	}
	listed, err = getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after channel removal: %v", err)
	}
	assertNoPeerDraftInDialogs(t, listed, &tg.PeerChannel{ChannelID: channel.ID})
	drafts, err = f.store.CloudDraftsForPeers(f.ctx, a1.id, []store.PeerDialogKey{{PeerType: store.PeerTypeChannel, PeerID: channel.ID}})
	if err != nil || len(drafts) != 0 {
		t.Fatalf("removed channel member draft read = %+v, err=%v, want hidden", drafts, err)
	}
}

func TestCloudDraftRateLimitIsSharedAcrossPeersAndSessions(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phone = "+15551049121"
	seedPhoneUsers(t, f.ctx, f.store, phone)
	firstSession := newSmokeClient(t, f, "cloud draft rate owner", phone)
	secondSession := newSmokeClient(t, f, "cloud draft rate owner second session", phone)

	const allowedSaves = 120
	chats := make([]store.Chat, allowedSaves)
	for i := range chats {
		chat, err := f.store.CreateChat(f.ctx, firstSession.id, fmt.Sprintf("draft rate peer %03d", i), nil)
		if err != nil {
			t.Fatalf("create rate-limit peer %d: %v", i, err)
		}
		chats[i] = chat
	}

	type receivedDraft struct {
		session int
		update  *tg.UpdateDraftMessage
	}
	pushes := make(chan receivedDraft, allowedSaves*2+1)
	for sessionID, client := range []*smokeClient{firstSession, secondSession} {
		go func(sessionID int, client *smokeClient) {
			for {
				select {
				case update := <-client.push.drafts:
					select {
					case pushes <- receivedDraft{session: sessionID, update: update}:
					case <-f.ctx.Done():
						return
					}
				case <-f.ctx.Done():
					return
				}
			}
		}(sessionID, client)
	}

	expected := make(map[int64]string, allowedSaves)
	for i, chat := range chats {
		message := fmt.Sprintf("draft-%03d", i)
		client := firstSession
		if i%2 == 1 {
			client = secondSession
		}
		if err := saveCloudDraft(client, f.ctx, &tg.MessagesSaveDraftRequest{
			Peer: &tg.InputPeerChat{ChatID: chat.ID}, Message: message,
		}); err != nil {
			t.Fatalf("allowed save %d through session %d: %v", i+1, i%2+1, err)
		}
		expected[chat.ID] = message
	}

	seen := make(map[[2]int64]bool, allowedSaves*2)
	for range allowedSaves * 2 {
		select {
		case got := <-pushes:
			peer, ok := got.update.Peer.(*tg.PeerChat)
			if !ok {
				t.Fatalf("rate-limit draft push peer = %T, want *tg.PeerChat", got.update.Peer)
			}
			draft, ok := got.update.Draft.(*tg.DraftMessage)
			if !ok || draft.Message != expected[peer.ChatID] {
				t.Fatalf("rate-limit draft push = %T(%+v), want %q", got.update.Draft, got.update.Draft, expected[peer.ChatID])
			}
			key := [2]int64{int64(got.session), peer.ChatID}
			if seen[key] {
				t.Fatalf("session %d received duplicate draft push for chat %d", got.session, peer.ChatID)
			}
			seen[key] = true
		case <-f.ctx.Done():
			t.Fatalf("waiting for accepted draft pushes: %v", f.ctx.Err())
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for accepted draft pushes in both sessions")
		}
	}
	if len(seen) != allowedSaves*2 {
		t.Fatalf("received %d distinct session/peer pushes, want %d", len(seen), allowedSaves*2)
	}

	lastChat := chats[len(chats)-1]
	err := saveCloudDraft(secondSession, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: &tg.InputPeerChat{ChatID: lastChat.ID}, Message: "must not replace the saved draft",
	})
	if expectTGError(err, "FLOOD_WAIT_60") != nil {
		t.Fatalf("121st account save error = %v, want FLOOD_WAIT_60", err)
	}
	drafts, err := f.store.CloudDraftsForPeers(f.ctx, firstSession.id, []store.PeerDialogKey{{
		PeerType: store.PeerTypeChat, PeerID: lastChat.ID,
	}})
	if err != nil {
		t.Fatalf("read draft after rejected save: %v", err)
	}
	if len(drafts) != 1 || drafts[store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: lastChat.ID}].Message != expected[lastChat.ID] {
		t.Fatalf("rejected save changed prior state: got %+v, want %q", drafts, expected[lastChat.ID])
	}
	select {
	case unexpected := <-pushes:
		t.Fatalf("rejected save emitted a draft update: %+v", unexpected)
	case <-time.After(200 * time.Millisecond):
	}
}

func assertCloudDraftDifference(t *testing.T, result tg.UpdatesDifferenceClass, peerID int64, message string, cleared bool) {
	t.Helper()
	var updates []tg.UpdateClass
	switch difference := result.(type) {
	case *tg.UpdatesDifference:
		updates = difference.OtherUpdates
	case *tg.UpdatesDifferenceSlice:
		updates = difference.OtherUpdates
	default:
		t.Fatalf("getDifference = %T, want difference with cloud draft update", result)
	}
	for _, raw := range updates {
		update, ok := raw.(*tg.UpdateDraftMessage)
		if !ok {
			continue
		}
		peer, ok := update.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			continue
		}
		if cleared {
			if _, ok := update.Draft.(*tg.DraftMessageEmpty); !ok {
				t.Fatalf("getDifference clear draft = %T, want *tg.DraftMessageEmpty", update.Draft)
			}
			return
		}
		draft, ok := update.Draft.(*tg.DraftMessage)
		if !ok || draft.Message != message || draft.Date <= 0 {
			t.Fatalf("getDifference draft = %T(%+v), want message %q with server date", update.Draft, update.Draft, message)
		}
		return
	}
	t.Fatalf("getDifference omitted cloud draft update for peer %d", peerID)
}

func TestCloudDraftValidation(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC, phoneD = "+15551049111", "+15551049112", "+15551049113", "+15551049114"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB, phoneC, phoneD)
	a := newSmokeClient(t, f, "cloud draft validation", phoneA)
	b := dialogPinUser(t, f, phoneB)
	c := dialogPinUser(t, f, phoneC)
	d := dialogPinUser(t, f, phoneD)
	seedDialogPinDM(t, f, a.id, b.ID, 1049111)
	crossTarget, _, _, duplicate, err := f.store.SendMessage(f.ctx, a.id, c.ID, "cross-dialog reply target", 1049112, 0, 0)
	if err != nil {
		t.Fatalf("seed cross-dialog target: %v", err)
	}
	if duplicate {
		t.Fatal("seed cross-dialog target unexpectedly deduplicated")
	}
	deletedTarget, _, _, duplicate, err := f.store.SendMessage(f.ctx, a.id, b.ID, "deleted reply target", 1049113, 0, 0)
	if err != nil {
		t.Fatalf("seed deleted reply target: %v", err)
	}
	if duplicate {
		t.Fatal("seed deleted reply target unexpectedly deduplicated")
	}
	if _, err = f.store.DeleteMessages(f.ctx, a.id, []int64{deletedTarget.LocalID}, true); err != nil {
		t.Fatalf("delete reply target: %v", err)
	}

	if err := saveCloudDraft(a, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: peerUser(a.id, b.ID), Message: "stable draft",
	}); err != nil {
		t.Fatalf("save stable draft: %v", err)
	}

	longText := strings.Repeat("x", 4096)
	if err := saveCloudDraft(a, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: peerUser(a.id, b.ID), Message: longText,
	}); err != nil {
		t.Fatalf("save 4096-rune draft: %v", err)
	}
	for _, input := range []struct {
		name string
		text string
	}{
		{name: "4097 runes", text: strings.Repeat("y", 4097)},
		{name: "invalid UTF-8", text: string([]byte{0xff})},
	} {
		err := saveCloudDraft(a, f.ctx, &tg.MessagesSaveDraftRequest{
			Peer: peerUser(a.id, b.ID), Message: input.text,
		})
		if expectTGError(err, "MESSAGE_TOO_LONG") != nil {
			t.Fatalf("save %s error = %v, want MESSAGE_TOO_LONG", input.name, err)
		}
	}
	listed, err := getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after rejected text: %v", err)
	}
	assertCloudDraftInDialogs(t, listed, b.ID, longText, false, 0)

	webpage := &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, b.ID), Message: "link draft"}
	webpage.SetMedia(&tg.InputMediaWebPage{URL: "https://example.test/must-not-be-fetched"})
	webpage.SetFlags()
	if err := saveCloudDraft(a, f.ctx, webpage); err != nil {
		t.Fatalf("save webpage draft: %v", err)
	}
	listed, err = getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after webpage draft: %v", err)
	}
	draft := cloudDraftFromDialogs(t, listed, b.ID)
	if draft.Message != "link draft" {
		t.Fatalf("webpage draft message = %q, want link draft", draft.Message)
	}
	if _, ok := draft.GetMedia(); ok {
		t.Fatal("webpage draft retained media")
	}
	if _, ok := draft.GetEntities(); ok {
		t.Fatal("webpage draft retained entities")
	}

	entityDraft := &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, b.ID), Message: "styled"}
	entityDraft.SetEntities([]tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 6}})
	entityDraft.SetFlags()
	if err := saveCloudDraft(a, f.ctx, entityDraft); err != nil {
		t.Fatalf("save draft with dropped entities: %v", err)
	}
	listed, err = getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after entity draft: %v", err)
	}
	draft = cloudDraftFromDialogs(t, listed, b.ID)
	if _, ok := draft.GetEntities(); ok {
		t.Fatal("draft retained entities")
	}

	invalidMedia := &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, b.ID), Message: "unchanged"}
	invalidMedia.SetMedia(&tg.InputMediaUploadedPhoto{File: &tg.InputFile{ID: 1, Parts: 1, Name: "unused"}})
	invalidMedia.SetFlags()
	if err := saveCloudDraft(a, f.ctx, invalidMedia); expectTGError(err, "MEDIA_INVALID") != nil {
		t.Fatalf("save unsupported media error = %v, want MEDIA_INVALID", err)
	}
	for name, unsupported := range map[string]func(*tg.MessagesSaveDraftRequest){
		"effect":         func(req *tg.MessagesSaveDraftRequest) { req.SetEffect(0) },
		"suggested post": func(req *tg.MessagesSaveDraftRequest) { req.SetSuggestedPost(tg.SuggestedPost{}) },
		"rich message":   func(req *tg.MessagesSaveDraftRequest) { req.SetRichMessage(&tg.InputRichMessage{}) },
	} {
		req := &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, b.ID), Message: "unchanged"}
		unsupported(req)
		req.SetFlags()
		if err := saveCloudDraft(a, f.ctx, req); expectTGError(err, "INPUT_REQUEST_INVALID") != nil {
			t.Fatalf("save draft with %s error = %v, want INPUT_REQUEST_INVALID", name, err)
		}
	}

	for name, reply := range map[string]tg.InputReplyToClass{
		"cross-dialog target":        &tg.InputReplyToMessage{ReplyToMsgID: int(crossTarget.LocalID)},
		"cross-dialog explicit peer": cloudDraftExplicitPeerReply(t, int(crossTarget.LocalID), peerUser(a.id, c.ID)),
		"deleted target":             &tg.InputReplyToMessage{ReplyToMsgID: int(deletedTarget.LocalID)},
		"zero target":                &tg.InputReplyToMessage{ReplyToMsgID: 0},
		"story target":               &tg.InputReplyToStory{Peer: peerUser(a.id, b.ID), StoryID: 1},
		"top message":                cloudDraftTopReply(t, 1),
		"monoforum":                  cloudDraftMonoforumReply(t, int(crossTarget.LocalID), peerUser(a.id, c.ID)),
	} {
		req := &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, b.ID), Message: "unchanged"}
		req.SetReplyTo(reply)
		req.SetFlags()
		if err := saveCloudDraft(a, f.ctx, req); expectTGError(err, "MESSAGE_ID_INVALID") != nil {
			t.Fatalf("save draft with %s error = %v, want MESSAGE_ID_INVALID", name, err)
		}
	}
	if err := saveCloudDraft(a, f.ctx, &tg.MessagesSaveDraftRequest{Peer: peerUser(a.id, d.ID), Message: "no dialog"}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("save draft without existing dialog error = %v, want PEER_ID_INVALID", err)
	}
	listed, err = getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs after rejected draft changes: %v", err)
	}
	assertCloudDraftInDialogs(t, listed, b.ID, "styled", false, 0)
}

func assertCloudDraftUpdate(t *testing.T, update *tg.UpdateDraftMessage, peerID int64, message string, noWebpage bool, replyID int) {
	t.Helper()
	peer, ok := update.Peer.(*tg.PeerUser)
	if !ok || peer.UserID != peerID {
		t.Fatalf("draft update peer = %T(%+v), want user %d", update.Peer, update.Peer, peerID)
	}
	draft, ok := update.Draft.(*tg.DraftMessage)
	if !ok {
		t.Fatalf("draft update = %T, want *tg.DraftMessage", update.Draft)
	}
	if draft.Message != message || draft.NoWebpage != noWebpage || draft.Date <= 0 {
		t.Fatalf("draft update contents = message %q no_webpage=%v date=%d", draft.Message, draft.NoWebpage, draft.Date)
	}
	assertDraftReply(t, draft, replyID)
}

func assertCloudDraftUpdateForPeer(t *testing.T, update *tg.UpdateDraftMessage, expected tg.PeerClass, message string) {
	t.Helper()
	if !sameDraftPeer(update.Peer, expected) {
		t.Fatalf("draft update peer = %T(%+v), want %T(%+v)", update.Peer, update.Peer, expected, expected)
	}
	draft, ok := update.Draft.(*tg.DraftMessage)
	if !ok || draft.Message != message || draft.Date <= 0 {
		t.Fatalf("draft update = %T(%+v), want message %q with server date", update.Draft, update.Draft, message)
	}
}

func assertPeerDraftInDialogs(t *testing.T, result tg.MessagesDialogsClass, expected tg.PeerClass, message string) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || !sameDraftPeer(dialog.Peer, expected) {
			continue
		}
		rawDraft, ok := dialog.GetDraft()
		if !ok {
			t.Fatalf("dialog for %T(%+v) has no draft", expected, expected)
		}
		draft, ok := rawDraft.(*tg.DraftMessage)
		if !ok || draft.Message != message || draft.Date <= 0 {
			t.Fatalf("dialog draft = %T(%+v), want message %q with server date", rawDraft, rawDraft, message)
		}
		return
	}
	t.Fatalf("dialogs omit peer %T(%+v)", expected, expected)
}

func assertNoPeerDraftInDialogs(t *testing.T, result tg.MessagesDialogsClass, expected tg.PeerClass) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || !sameDraftPeer(dialog.Peer, expected) {
			continue
		}
		if draft, found := dialog.GetDraft(); found {
			t.Fatalf("removed peer %T(%+v) exposed draft %T", expected, expected, draft)
		}
	}
}

func sameDraftPeer(actual, expected tg.PeerClass) bool {
	switch want := expected.(type) {
	case *tg.PeerUser:
		got, ok := actual.(*tg.PeerUser)
		return ok && got.UserID == want.UserID
	case *tg.PeerChat:
		got, ok := actual.(*tg.PeerChat)
		return ok && got.ChatID == want.ChatID
	case *tg.PeerChannel:
		got, ok := actual.(*tg.PeerChannel)
		return ok && got.ChannelID == want.ChannelID
	default:
		return false
	}
}

func assertCloudDraftInDialogs(t *testing.T, result tg.MessagesDialogsClass, peerID int64, message string, noWebpage bool, replyID int) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			continue
		}
		rawDraft, ok := dialog.GetDraft()
		if !ok {
			t.Fatalf("dialog for %d has no draft", peerID)
		}
		draft, ok := rawDraft.(*tg.DraftMessage)
		if !ok {
			t.Fatalf("dialog draft = %T, want *tg.DraftMessage", rawDraft)
		}
		if draft.Message != message || draft.NoWebpage != noWebpage || draft.Date <= 0 {
			t.Fatalf("dialog draft contents = message %q no_webpage=%v date=%d", draft.Message, draft.NoWebpage, draft.Date)
		}
		assertDraftReply(t, draft, replyID)
		return
	}
	t.Fatalf("dialogs omit peer %d", peerID)
}

func assertDraftReply(t *testing.T, draft *tg.DraftMessage, replyID int) {
	t.Helper()
	reply, ok := draft.GetReplyTo()
	if replyID == 0 {
		if ok {
			t.Fatalf("draft unexpectedly has reply: %T", reply)
		}
		return
	}
	if !ok {
		t.Fatal("draft has no reply")
	}
	replyMessage, ok := reply.(*tg.InputReplyToMessage)
	if !ok || replyMessage.ReplyToMsgID != replyID {
		t.Fatalf("draft reply = %T(%+v), want message %d", reply, reply, replyID)
	}
}

func assertNoCloudDraft(t *testing.T, result tg.MessagesDialogsClass, peerID int64) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if ok && peer.UserID == peerID {
			if _, found := dialog.GetDraft(); found {
				t.Fatalf("dialog for %d still has a draft", peerID)
			}
			return
		}
	}
}

func saveCloudDraft(client *smokeClient, ctx context.Context, request *tg.MessagesSaveDraftRequest) error {
	return client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		request.SetFlags()
		_, err := api.MessagesSaveDraft(ctx, request)
		return err
	})
}

func cloudDraftFromDialogs(t *testing.T, result tg.MessagesDialogsClass, peerID int64) *tg.DraftMessage {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			continue
		}
		rawDraft, ok := dialog.GetDraft()
		if !ok {
			t.Fatalf("dialog for %d has no draft", peerID)
		}
		draft, ok := rawDraft.(*tg.DraftMessage)
		if !ok {
			t.Fatalf("dialog draft = %T, want *tg.DraftMessage", rawDraft)
		}
		return draft
	}
	t.Fatalf("dialogs omit peer %d", peerID)
	return nil
}

func cloudDraftTopReply(t *testing.T, topID int) tg.InputReplyToClass {
	t.Helper()
	reply := &tg.InputReplyToMessage{ReplyToMsgID: topID}
	reply.SetTopMsgID(topID)
	return reply
}

func cloudDraftExplicitPeerReply(t *testing.T, replyID int, peer tg.InputPeerClass) tg.InputReplyToClass {
	t.Helper()
	reply := &tg.InputReplyToMessage{ReplyToMsgID: replyID}
	reply.SetReplyToPeerID(peer)
	return reply
}

func cloudDraftMonoforumReply(t *testing.T, replyID int, peer tg.InputPeerClass) tg.InputReplyToClass {
	t.Helper()
	reply := &tg.InputReplyToMessage{ReplyToMsgID: replyID}
	reply.SetMonoforumPeerID(peer)
	return reply
}
