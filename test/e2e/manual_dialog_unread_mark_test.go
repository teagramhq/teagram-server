package e2e_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestManualDialogUnreadMark(t *testing.T) {
	t.Parallel()
	t.Run("one-to-one-and-recovery", testSmokeManualDialogUnreadMark)
	t.Run("read-history-remains-separate", testManualDialogUnreadMarkReadHistory)
	t.Run("group-channel-and-removal", testManualDialogUnreadMarkGroupChannelAndRemoval)
	t.Run("invalid-peers-do-not-write-or-charge", testManualDialogUnreadMarkInvalidPeers)
	t.Run("shared-mutation-budget", testManualDialogUnreadMarkSharedBudget)
}

func testSmokeManualDialogUnreadMark(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049171", "+15551049172"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "unread mark owner", phoneA)
	a2 := newSmokeClient(t, f, "unread mark owner second session", phoneA)
	bClient := newSmokeClient(t, f, "unread mark other owner", phoneB)
	b := dialogPinUser(t, f, phoneB)
	seedDialogPinDM(t, f, a1.id, b.ID, 1049171)
	if _, _, _, duplicate, err := f.store.SendMessage(f.ctx, a1.id, a1.id, "saved unread mark dialog", 1049172, 0, 0); err != nil || duplicate {
		t.Fatalf("seed Saved Messages dialog: duplicate=%v err=%v", duplicate, err)
	}

	stateBefore := readUnreadMarkState(t, f.ctx, a1)
	dialogBefore := readUnreadMarkDialog(t, f.ctx, a1, &tg.PeerUser{UserID: b.ID})
	if dialogBefore.GetUnreadMark() {
		t.Fatal("new dialog unexpectedly has unread_mark=true")
	}
	if _, err := markDialogUnread(a1, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a1.id, b.ID)}); err != nil {
		t.Fatalf("mark existing 1:1 dialog unread: %v", err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner unread mark push"), &tg.PeerUser{UserID: b.ID}, true)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "second-session unread mark push"), &tg.PeerUser{UserID: b.ID}, true)
	assertUnreadMarkHydrated(t, f.ctx, a2, b.ID, true)
	assertUnreadMarkReadStateUnchanged(t, f.ctx, a2, b.ID, dialogBefore, stateBefore)

	if ok, err := markDialogUnread(a1, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a1.id, b.ID)}); err != nil || !ok {
		t.Fatalf("repeat unread mark: BoolTrue=%v err=%v", ok, err)
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, a1, a2, "unchanged unread mark")

	if ok, err := markDialogUnread(a1, f.ctx, false, &tg.InputDialogPeer{Peer: peerUser(a1.id, b.ID)}); err != nil || !ok {
		t.Fatalf("clear unread mark: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner unread mark clear"), &tg.PeerUser{UserID: b.ID}, false)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "second-session unread mark clear"), &tg.PeerUser{UserID: b.ID}, false)
	assertUnreadMarkHydrated(t, f.ctx, a2, b.ID, false)
	assertUnreadMarkReadStateUnchanged(t, f.ctx, a2, b.ID, dialogBefore, stateBefore)
	if ok, err := markDialogUnread(a1, f.ctx, false, &tg.InputDialogPeer{Peer: peerUser(a1.id, b.ID)}); err != nil || !ok {
		t.Fatalf("repeat unread mark clear: BoolTrue=%v err=%v", ok, err)
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, a1, a2, "unchanged unread mark clear")

	if err := bClient.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		state, err := api.UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		if state.Pts == 0 {
			return errors.New("other account state has pts=0, want the seeded message event")
		}
		return nil
	}); err != nil {
		t.Fatalf("read other account updates state: %v", err)
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, bClient, bClient, "other account unread mark isolation")
	assertUnreadMarkHydrated(t, f.ctx, bClient, a1.id, false)

	difference, err := getDifference(a2, f.ctx, stateBefore, 0)
	if err != nil {
		t.Fatalf("getDifference after clearing the final unread mark: %v", err)
	}
	assertDialogUnreadMarkDifference(t, difference, &tg.PeerUser{UserID: b.ID}, false)

	selfPeer := &tg.PeerUser{UserID: a1.id}
	selfInput := &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}
	if ok, err := markDialogUnread(a1, f.ctx, true, selfInput); err != nil || !ok {
		t.Fatalf("mark Saved Messages unread: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "Saved Messages mark"), selfPeer, true)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner Saved Messages mark"), selfPeer, true)
	assertUnreadMarkHydrated(t, f.ctx, a2, a1.id, true)
	if ok, err := markDialogUnread(a1, f.ctx, false, selfInput); err != nil || !ok {
		t.Fatalf("clear Saved Messages unread mark: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "Saved Messages clear"), selfPeer, false)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner Saved Messages clear"), selfPeer, false)
	assertUnreadMarkHydrated(t, f.ctx, a2, a1.id, false)
}

func markDialogUnread(c *smokeClient, ctx context.Context, unread bool, peer tg.InputDialogPeerClass) (bool, error) {
	return markDialogUnreadWithParent(c, ctx, unread, peer, nil)
}

func markDialogUnreadWithParent(c *smokeClient, ctx context.Context, unread bool, peer tg.InputDialogPeerClass, parent tg.InputPeerClass) (bool, error) {
	var result bool
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		request := &tg.MessagesMarkDialogUnreadRequest{Unread: unread, Peer: peer}
		if parent != nil {
			request.SetParentPeer(parent)
		}
		result, err = api.MessagesMarkDialogUnread(ctx, request)
		return err
	})
	return result, err
}

func readUnreadMarkState(t *testing.T, ctx context.Context, c *smokeClient) tg.UpdatesState {
	t.Helper()
	var state *tg.UpdatesState
	if err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		state, err = api.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("updates.getState: %v", err)
	}
	return *state
}

func readUnreadMarkDialog(t *testing.T, ctx context.Context, c *smokeClient, peer tg.PeerClass) *tg.Dialog {
	t.Helper()
	listed, err := getDialogs(c, ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("messages.getDialogs: %v", err)
	}
	for _, raw := range dialogRows(t, listed) {
		if dialog, ok := raw.(*tg.Dialog); ok && sameUnreadMarkPeer(dialog.Peer, peer) {
			return dialog
		}
	}
	t.Fatalf("messages.getDialogs omitted peer %s", peer)
	return nil
}

func assertUnreadMarkHydrated(t *testing.T, ctx context.Context, c *smokeClient, peerID int64, want bool) {
	t.Helper()
	peer := &tg.PeerUser{UserID: peerID}
	assertUnreadMarkHydratedPeer(t, ctx, c, peerUser(c.id, peerID), peer, want)
}

func assertUnreadMarkHydratedPeer(t *testing.T, ctx context.Context, c *smokeClient, input tg.InputPeerClass, peer tg.PeerClass, want bool) {
	t.Helper()
	dialog := readUnreadMarkDialog(t, ctx, c, peer)
	if got := dialog.GetUnreadMark(); got != want {
		t.Fatalf("getDialogs unread_mark=%v, want %v", got, want)
	}
	var peerDialogs *tg.MessagesPeerDialogs
	if err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		peerDialogs, err = api.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
			&tg.InputDialogPeer{Peer: input},
		})
		return err
	}); err != nil {
		t.Fatalf("messages.getPeerDialogs: %v", err)
	}
	for _, raw := range peerDialogs.Dialogs {
		if dialog, ok := raw.(*tg.Dialog); ok && sameUnreadMarkPeer(dialog.Peer, peer) {
			if got := dialog.GetUnreadMark(); got != want {
				t.Fatalf("getPeerDialogs unread_mark=%v, want %v", got, want)
			}
			return
		}
	}
	t.Fatalf("messages.getPeerDialogs omitted peer %s", peer)
}

func testManualDialogUnreadMarkGroupChannelAndRemoval(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049181", "+15551049182"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "unread mark group owner", phoneA)
	a2 := newSmokeClient(t, f, "unread mark group second session", phoneA)
	creator := dialogPinUser(t, f, phoneB)

	group, err := f.store.CreateChat(f.ctx, creator.ID, "unread mark group", []int64{a1.id})
	if err != nil {
		t.Fatalf("create visible basic group: %v", err)
	}
	if _, _, _, err = f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: group.ID, FromID: a1.id, Text: "visible unread mark group", RandomID: 1049181,
	}); err != nil {
		t.Fatalf("seed visible basic group dialog: %v", err)
	}
	groupPeer := &tg.PeerChat{ChatID: group.ID}
	groupInput := &tg.InputPeerChat{ChatID: group.ID}
	if ok, err := markDialogUnread(a1, f.ctx, true, &tg.InputDialogPeer{Peer: groupInput}); err != nil || !ok {
		t.Fatalf("mark group unread: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner group mark"), groupPeer, true)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner group mark"), groupPeer, true)
	assertUnreadMarkHydratedPeer(t, f.ctx, a2, groupInput, groupPeer, true)
	if ok, err := markDialogUnread(a1, f.ctx, false, &tg.InputDialogPeer{Peer: groupInput}); err != nil || !ok {
		t.Fatalf("clear group unread mark: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner group clear"), groupPeer, false)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner group clear"), groupPeer, false)
	assertUnreadMarkHydratedPeer(t, f.ctx, a2, groupInput, groupPeer, false)

	channel, err := f.store.CreateChannel(f.ctx, creator.ID, "unread mark channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := f.store.CreateChannelInvite(f.ctx, channel.ID, creator.ID)
	if err != nil {
		t.Fatalf("create channel invite: %v", err)
	}
	if _, _, err := f.store.JoinChannelByInvite(f.ctx, invite, a1.id); err != nil {
		t.Fatalf("join channel: %v", err)
	}
	channelPeer := &tg.PeerChannel{ChannelID: channel.ID}
	channelInput := peerChannel(a1.id, channel.ID)
	if ok, err := markDialogUnread(a1, f.ctx, true, &tg.InputDialogPeer{Peer: channelInput}); err != nil || !ok {
		t.Fatalf("mark channel unread: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner channel mark"), channelPeer, true)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner channel mark"), channelPeer, true)
	assertUnreadMarkHydratedPeer(t, f.ctx, a2, channelInput, channelPeer, true)
	if ok, err := markDialogUnread(a1, f.ctx, false, &tg.InputDialogPeer{Peer: channelInput}); err != nil || !ok {
		t.Fatalf("clear channel unread mark: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner channel clear"), channelPeer, false)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "same-owner channel clear"), channelPeer, false)
	assertUnreadMarkHydratedPeer(t, f.ctx, a2, channelInput, channelPeer, false)

	type markResult struct {
		ok  bool
		err error
	}
	type removeResult struct {
		removed bool
		err     error
	}
	startRace := make(chan struct{})
	marked := make(chan markResult, 1)
	removed := make(chan removeResult, 1)
	go func() {
		<-startRace
		ok, err := markDialogUnread(a1, f.ctx, true, &tg.InputDialogPeer{Peer: groupInput})
		marked <- markResult{ok: ok, err: err}
	}()
	go func() {
		<-startRace
		ok, _, _, err := f.store.RemoveChatUser(f.ctx, group.ID, a1.id, creator.ID)
		removed <- removeResult{removed: ok, err: err}
	}()
	close(startRace)
	var mark markResult
	select {
	case mark = <-marked:
	case <-f.ctx.Done():
		t.Fatalf("concurrent mark timed out: %v", f.ctx.Err())
	}
	var removal removeResult
	select {
	case removal = <-removed:
	case <-f.ctx.Done():
		t.Fatalf("concurrent group removal timed out: %v", f.ctx.Err())
	}
	if removal.err != nil || !removal.removed {
		t.Fatalf("concurrent group removal: removed=%v err=%v", removal.removed, removal.err)
	}
	if mark.err != nil {
		if expectTGError(mark.err, "PEER_ID_INVALID") != nil {
			t.Fatalf("concurrent unread mark error = %v, want success or PEER_ID_INVALID", mark.err)
		}
	} else if !mark.ok {
		t.Fatal("concurrent unread mark returned BoolFalse, want BoolTrue or PEER_ID_INVALID")
	}
	drainDialogUnreadMarkPushes(t, f.ctx, a1, a2, groupPeer)
	groupKey := store.PeerDialogKey{PeerType: store.PeerTypeChat, PeerID: group.ID}
	if err := f.store.Notify(f.ctx, store.ChannelUpdates, store.DialogUnreadMarkNotificationPayload(a1.id, groupKey)); err != nil {
		t.Fatalf("notify stale marked group after removal: %v", err)
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, a1, a2, "removed member stale unread mark")
	assertUnreadMarkHydratedPeer(t, f.ctx, a2, groupInput, groupPeer, false)
}

func testManualDialogUnreadMarkReadHistory(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049185", "+15551049186"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "unread mark read-history owner", phoneA)
	a2 := newSmokeClient(t, f, "unread mark read-history second session", phoneA)
	b := dialogPinUser(t, f, phoneB)
	incoming, _, _, duplicate, err := f.store.SendMessage(f.ctx, b.ID, a1.id, "unread mark inbound", 1049185, 0, 0)
	if err != nil || duplicate {
		t.Fatalf("seed inbound message: duplicate=%v err=%v", duplicate, err)
	}
	peerInput := &tg.InputDialogPeer{Peer: peerUser(a1.id, b.ID)}
	if ok, err := markDialogUnread(a1, f.ctx, true, peerInput); err != nil || !ok {
		t.Fatalf("mark dialog unread before readHistory: BoolTrue=%v err=%v", ok, err)
	}
	peer := &tg.PeerUser{UserID: b.ID}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a1.push.dialogUnread, "owner mark before readHistory"), peer, true)
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "second-session mark before readHistory"), peer, true)
	before := readUnreadMarkDialog(t, f.ctx, a2, peer)
	if before.UnreadCount == 0 {
		t.Fatal("seeded inbound dialog has unread_count=0, want unread message")
	}
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer: peerUser(a1.id, b.ID), MaxID: int(incoming.PeerLocalID),
		})
		return err
	}); err != nil {
		t.Fatalf("messages.readHistory: %v", err)
	}
	after := readUnreadMarkDialog(t, f.ctx, a2, peer)
	if after.ReadInboxMaxID <= before.ReadInboxMaxID || after.UnreadCount != 0 {
		t.Fatalf("readHistory state marker/count = %d/%d, want marker above %d and count 0", after.ReadInboxMaxID, after.UnreadCount, before.ReadInboxMaxID)
	}
	if !after.GetUnreadMark() {
		t.Fatal("readHistory cleared the separate manual unread mark")
	}
	if ok, err := markDialogUnread(a1, f.ctx, false, peerInput); err != nil || !ok {
		t.Fatalf("clear manual mark after open: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a2.push.dialogUnread, "clear manual mark after readHistory"), peer, false)
}

func testManualDialogUnreadMarkInvalidPeers(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC, phoneD = "+15551049191", "+15551049192", "+15551049193", "+15551049194"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB, phoneC, phoneD)
	a := newSmokeClient(t, f, "unread mark invalid owner", phoneA)
	b := dialogPinUser(t, f, phoneB)
	c := dialogPinUser(t, f, phoneC)
	d := dialogPinUser(t, f, phoneD)
	seedDialogPinDM(t, f, a.id, b.ID, 1049191)
	privateGroup, err := f.store.CreateChat(f.ctx, c.ID, "private unread mark group", []int64{d.ID})
	if err != nil {
		t.Fatalf("create inaccessible group: %v", err)
	}
	privateChannel, err := f.store.CreateChannel(f.ctx, c.ID, "private unread mark channel", "", false)
	if err != nil {
		t.Fatalf("create inaccessible channel: %v", err)
	}
	stateBefore := readUnreadMarkState(t, f.ctx, a)
	dialogBefore := readUnreadMarkDialog(t, f.ctx, a, &tg.PeerUser{UserID: b.ID})
	validPeer := &tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)}
	cases := []struct {
		name   string
		peer   tg.InputDialogPeerClass
		parent tg.InputPeerClass
	}{
		{name: "unknown direct dialog", peer: &tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)}},
		{name: "inaccessible group", peer: &tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: privateGroup.ID}}},
		{name: "inaccessible channel", peer: &tg.InputDialogPeer{Peer: peerChannel(a.id, privateChannel.ID)}},
		{name: "parent peer saved sublist", peer: validPeer, parent: peerUser(a.id, b.ID)},
		{name: "folder dialog peer", peer: &tg.InputDialogPeerFolder{FolderID: 1}},
		{name: "community dialog peer", peer: &tg.InputDialogPeerCommunity{Community: inputChannel(a.id, privateChannel.ID)}},
		{name: "forged user access hash", peer: &tg.InputDialogPeer{Peer: &tg.InputPeerUser{UserID: b.ID, AccessHash: 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ok, err := markDialogUnreadWithParent(a, f.ctx, true, tc.peer, tc.parent); ok || expectTGError(err, "PEER_ID_INVALID") != nil {
				t.Fatalf("mark invalid peer: BoolTrue=%v err=%v, want PEER_ID_INVALID", ok, err)
			}
		})
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, a, a, "rejected unread mark mutations")
	assertUnreadMarkReadStateUnchanged(t, f.ctx, a, b.ID, dialogBefore, stateBefore)
	assertUnreadMarkHydrated(t, f.ctx, a, b.ID, false)
	for attempt := range 60 {
		if result, err := f.store.CheckRateLimitCost(f.ctx, a.id, "dialog_filter_mutation", store.RateLimitConfig{Limit: 60, Window: time.Minute}, 1); err != nil || result != nil {
			t.Fatalf("invalid peer consumed mutation budget before attempt %d: result=%v err=%v", attempt+1, result, err)
		}
	}
}

func testManualDialogUnreadMarkSharedBudget(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049201", "+15551049202"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a := newSmokeClient(t, f, "unread mark rate owner", phoneA)
	b := dialogPinUser(t, f, phoneB)
	seedDialogPinDM(t, f, a.id, b.ID, 1049201)
	peer := &tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)}
	for attempt := range 59 {
		if ok, err := toggleDialogPin(a, f.ctx, false, peer); err != nil || !ok {
			t.Fatalf("unchanged pin mutation %d: BoolTrue=%v err=%v", attempt+1, ok, err)
		}
	}
	if ok, err := markDialogUnread(a, f.ctx, true, peer); err != nil || !ok {
		t.Fatalf("unread mark at shared budget limit: BoolTrue=%v err=%v", ok, err)
	}
	assertDialogUnreadMarkUpdate(t, recvOrCtx(t, f.ctx, a.push.dialogUnread, "unread mark at rate limit"), &tg.PeerUser{UserID: b.ID}, true)
	if ok, err := markDialogUnread(a, f.ctx, false, peer); ok || expectTGError(err, "FLOOD_WAIT_60") != nil {
		t.Fatalf("61st shared mutation: BoolTrue=%v err=%v, want FLOOD_WAIT_60", ok, err)
	}
	assertNoDialogUnreadMarkPush(t, f.ctx, a, a, "rate-limited unread mark")
	assertUnreadMarkHydrated(t, f.ctx, a, b.ID, true)
}

func assertUnreadMarkReadStateUnchanged(t *testing.T, ctx context.Context, c *smokeClient, peerID int64, beforeDialog *tg.Dialog, beforeState tg.UpdatesState) {
	t.Helper()
	afterDialog := readUnreadMarkDialog(t, ctx, c, &tg.PeerUser{UserID: peerID})
	if afterDialog.ReadInboxMaxID != beforeDialog.ReadInboxMaxID || afterDialog.UnreadCount != beforeDialog.UnreadCount {
		t.Fatalf("manual unread mark changed message read state: before marker/count=%d/%d after=%d/%d",
			beforeDialog.ReadInboxMaxID, beforeDialog.UnreadCount, afterDialog.ReadInboxMaxID, afterDialog.UnreadCount)
	}
	afterState := readUnreadMarkState(t, ctx, c)
	if afterState.Pts != beforeState.Pts {
		t.Fatalf("manual unread mark changed updates.getState pts from %d to %d", beforeState.Pts, afterState.Pts)
	}
}

func assertDialogUnreadMarkUpdate(t *testing.T, update *tg.UpdateDialogUnreadMark, peer tg.PeerClass, want bool) {
	t.Helper()
	if !sameUnreadMarkDialogPeer(update.Peer, peer) || update.GetUnread() != want {
		t.Fatalf("updateDialogUnreadMark peer=%v unread=%v, want peer=%v unread=%v", update.Peer, update.GetUnread(), peer, want)
	}
}

func assertDialogUnreadMarkDifference(t *testing.T, result tg.UpdatesDifferenceClass, peer tg.PeerClass, want bool) {
	t.Helper()
	var updates []tg.UpdateClass
	switch difference := result.(type) {
	case *tg.UpdatesDifference:
		updates = difference.OtherUpdates
	case *tg.UpdatesDifferenceSlice:
		updates = difference.OtherUpdates
	default:
		t.Fatalf("getDifference = %T, want a difference response", result)
	}
	for _, raw := range updates {
		update, ok := raw.(*tg.UpdateDialogUnreadMark)
		if ok && sameUnreadMarkDialogPeer(update.Peer, peer) {
			if got := update.GetUnread(); got != want {
				t.Fatalf("getDifference unread mark=%v, want %v", got, want)
			}
			return
		}
	}
	t.Fatalf("getDifference omitted unread mark for peer %v", peer)
}

func assertNoDialogUnreadMarkPush(t *testing.T, ctx context.Context, first, second *smokeClient, what string) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case unexpected := <-first.push.dialogUnread:
			t.Fatalf("%s emitted unread mark update to first session: %+v", what, unexpected)
		case unexpected := <-second.push.dialogUnread:
			t.Fatalf("%s emitted unread mark update to second session: %+v", what, unexpected)
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting for %s update check: %v", what, ctx.Err())
		}
	}
}

func drainDialogUnreadMarkPushes(t *testing.T, ctx context.Context, first, second *smokeClient, peer tg.PeerClass) {
	t.Helper()
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case update := <-first.push.dialogUnread:
			assertDialogUnreadMarkUpdate(t, update, peer, true)
		case update := <-second.push.dialogUnread:
			assertDialogUnreadMarkUpdate(t, update, peer, true)
		case <-timer.C:
			return
		case <-ctx.Done():
			t.Fatalf("waiting to drain concurrent unread mark updates: %v", ctx.Err())
		}
	}
}

func sameUnreadMarkPeer(a, b tg.PeerClass) bool {
	return a != nil && b != nil && a.TypeID() == b.TypeID() && a.String() == b.String()
}

func sameUnreadMarkDialogPeer(a tg.DialogPeerClass, b tg.PeerClass) bool {
	peer, ok := a.(*tg.DialogPeer)
	return ok && sameUnreadMarkPeer(peer.Peer, b)
}
