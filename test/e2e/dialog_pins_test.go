package e2e_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestDialogPins(t *testing.T) {
	t.Parallel()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC, phoneD, phoneE, phoneF = "+15551049031", "+15551049032", "+15551049033", "+15551049034", "+15551049035", "+15551049036"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB, phoneC, phoneD, phoneE, phoneF)
	a := newSmokeClient(t, f, "pin owner", phoneA)
	a2 := newSmokeClient(t, f, "pin owner second session", phoneA)
	other := newSmokeClient(t, f, "other pin owner", phoneC)
	b := dialogPinUser(t, f, phoneB)
	c := dialogPinUser(t, f, phoneC)
	d := dialogPinUser(t, f, phoneD)
	e := dialogPinUser(t, f, phoneE)
	missing := dialogPinUser(t, f, phoneF)
	seedDialogPinDM(t, f, a.id, b.ID, 1049031)
	seedDialogPinDM(t, f, a.id, c.ID, 1049033)
	seedDialogPinDM(t, f, a.id, d.ID, 1049034)
	seedDialogPinDM(t, f, a.id, e.ID, 1049035)
	group, err := f.store.CreateChat(f.ctx, b.ID, "Visible pinned group", []int64{a.id})
	if err != nil {
		t.Fatalf("create member's visible group: %v", err)
	}
	if _, _, _, err := f.store.SendChatMessage(f.ctx, store.FanOut{
		ChatID: group.ID, FromID: a.id, Text: "visible group dialog", RandomID: 1049032,
	}); err != nil {
		t.Fatalf("seed visible group dialog: %v", err)
	}
	inaccessibleGroup, err := f.store.CreateChat(f.ctx, c.ID, "Inaccessible group", []int64{d.ID})
	if err != nil {
		t.Fatalf("create inaccessible group: %v", err)
	}
	channel, err := f.store.CreateChannel(f.ctx, c.ID, "Pinned channel", "", false)
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	invite, err := f.store.CreateChannelInvite(f.ctx, channel.ID, c.ID)
	if err != nil {
		t.Fatalf("create channel invite: %v", err)
	}
	if _, _, err := f.store.JoinChannelByInvite(f.ctx, invite, a.id); err != nil {
		t.Fatalf("join channel as pin owner: %v", err)
	}

	// The target has no existing conversation yet. It becomes an eligible
	// unpinned peer only after this rejection has been checked.
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, missing.ID)}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("pin 1:1 peer without an existing dialog: %v", err)
	}
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerUser{UserID: b.ID, AccessHash: 123}}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("pin with forged access hash: %v", err)
	}
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: inaccessibleGroup.ID}}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("pin inaccessible basic group: %v", err)
	}
	if _, err := toggleDialogPin(a, f.ctx, false, &tg.InputDialogPeer{Peer: peerUser(a.id, c.ID)}); err != nil {
		t.Fatalf("unpin accessible but already-unpinned peer: %v", err)
	} else {
		assertNoPinRefresh(t, f.ctx, a2.push, "unchanged unpin")
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: inaccessibleGroup.ID}},
	}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("reject mixed valid and inaccessible reorder: %v", err)
	}
	if _, err := reorderDialogPins(a, f.ctx, 1, true, nil); expectTGError(err, "FOLDER_ID_INVALID") != nil {
		t.Fatalf("reject archived-folder reorder: %v", err)
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, []tg.InputDialogPeerClass{&tg.InputDialogPeerFolder{FolderID: 1}}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("reject folder dialog peer: %v", err)
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, []tg.InputDialogPeerClass{&tg.InputDialogPeerCommunity{Community: inputChannel(a.id, channel.ID)}}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("reject community dialog peer: %v", err)
	}
	tooManyRaw := make([]tg.InputDialogPeerClass, 101)
	for i := range tooManyRaw {
		tooManyRaw[i] = &tg.InputDialogPeerFolder{FolderID: 1}
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, tooManyRaw); expectTGError(err, "INPUT_REQUEST_INVALID") != nil {
		t.Fatalf("reject overlong raw reorder before peer validation: %v", err)
	}
	assertNoPinRefresh(t, f.ctx, a2.push, "rejected pin and reorder mutations")
	seedDialogPinDM(t, f, a.id, missing.ID, 1049037)

	listed, err := getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read empty default-folder pins: %v", err)
	}
	if len(listed.Dialogs) != 0 || listed.State.Pts == 0 {
		t.Fatalf("initial pinned dialogs = %d, state pts=%d, want empty pins and a populated state", len(listed.Dialogs), listed.State.Pts)
	}
	stateBeforePinMutations := listed.State
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}); err != nil {
		t.Fatalf("pin Saved Messages: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner Saved Messages pin refresh")
	listed, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read Saved Messages pin: %v", err)
	}
	assertPinnedPeerOrder(t, listed.Dialogs, []tg.PeerClass{&tg.PeerUser{UserID: a.id}})
	if _, err := toggleDialogPin(a, f.ctx, false, &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}); err != nil {
		t.Fatalf("unpin Saved Messages: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner Saved Messages unpin refresh")

	if err := f.store.SaveDialogFilter(f.ctx, a.id, store.DialogFilter{
		ID: 2, Title: "Custom pin isolation",
		PinnedPeers: []store.DialogFilterPeer{{Type: store.PeerTypeUser, ID: b.ID}},
	}); err != nil {
		t.Fatalf("seed custom-folder pin: %v", err)
	}
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}}); err != nil {
		t.Fatalf("pin visible basic group: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner group pin refresh")
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)}); err != nil {
		t.Fatalf("pin existing 1:1 dialog: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner pin refresh")
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}}); err != nil {
		t.Fatalf("repeat existing group pin: %v", err)
	}
	assertNoPinRefresh(t, f.ctx, a2.push, "repeat pin")
	listed, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read insertion order: %v", err)
	}
	assertPinnedPeerOrder(t, listed.Dialogs, []tg.PeerClass{&tg.PeerUser{UserID: b.ID}, &tg.PeerChat{ChatID: group.ID}})

	forceCore := []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}},
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}},
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, forceCore); err != nil {
		t.Fatalf("force reorder default-folder pins: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner reorder refresh")
	if _, err := reorderDialogPins(a, f.ctx, 0, true, forceCore); err != nil {
		t.Fatalf("repeat unchanged force reorder: %v", err)
	}
	assertNoPinRefresh(t, f.ctx, a2.push, "unchanged force reorder")
	listed, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read reordered default-folder pins: %v", err)
	}
	want := []tg.PeerClass{&tg.PeerChat{ChatID: group.ID}, &tg.PeerUser{UserID: b.ID}}
	assertPinnedPeerOrder(t, listed.Dialogs, want)
	for _, raw := range listed.Dialogs {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || !dialog.GetPinned() {
			t.Fatalf("pinned dialog %T has pinned=%v, want true", raw, ok && dialog.GetPinned())
		}
	}
	if listed.State.Pts == 0 {
		t.Fatal("reordered pinned dialogs omitted updates state")
	}
	if listed.State.Pts != stateBeforePinMutations.Pts || listed.State.Qts != stateBeforePinMutations.Qts || listed.State.Seq != stateBeforePinMutations.Seq || listed.State.Date != stateBeforePinMutations.Date {
		t.Fatalf("pin mutations changed update state: before=%+v after=%+v", stateBeforePinMutations, listed.State)
	}
	assertPinnedStateMatchesUpdates(t, a, f.ctx, listed.State)

	var archive *tg.MessagesPeerDialogs
	archive, err = getPinnedDialogs(a, f.ctx, 1)
	if err != nil {
		t.Fatalf("read archive pinned dialogs: %v", err)
	}
	if len(archive.Dialogs) != 0 || len(archive.Messages) != 0 || len(archive.Chats) != 0 || len(archive.Users) != 0 {
		t.Fatalf("archive pin response has peers: dialogs=%d messages=%d chats=%d users=%d", len(archive.Dialogs), len(archive.Messages), len(archive.Chats), len(archive.Users))
	}
	if _, err := getPinnedDialogs(a, f.ctx, 2); expectTGError(err, "FOLDER_ID_INVALID") != nil {
		t.Fatalf("reject unsupported pinned folder: %v", err)
	}
	if _, err := reorderDialogPins(a, f.ctx, 1, false, nil); expectTGError(err, "FOLDER_ID_INVALID") != nil {
		t.Fatalf("reject unsupported reorder folder: %v", err)
	}

	ordinary, err := getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("get ordinary dialogs: %v", err)
	}
	assertDialogPinned(t, ordinary, &tg.PeerChat{ChatID: group.ID}, true)
	assertDialogPinned(t, ordinary, &tg.PeerUser{UserID: b.ID}, true)
	assertDialogPinned(t, ordinary, &tg.PeerUser{UserID: c.ID}, false)

	firstPage, err := getDialogs(a, f.ctx, 0, 3, true)
	if err != nil {
		t.Fatalf("get first unpinned dialog page: %v", err)
	}
	firstRows := dialogRows(t, firstPage)
	if len(firstRows) != 4 {
		t.Fatalf("first exclude_pinned page has %d dialogs, want channel block + 3 rows", len(firstRows))
	}
	firstChannel, ok := firstRows[0].(*tg.Dialog)
	if !ok || firstChannel.Peer.TypeID() != (&tg.PeerChannel{}).TypeID() || firstChannel.Peer.String() != (&tg.PeerChannel{ChannelID: channel.ID}).String() {
		t.Fatalf("first exclude_pinned dialog = %T %v, want prepended channel %d", firstRows[0], firstRows[0], channel.ID)
	}
	pageSlice, ok := firstPage.(*tg.MessagesDialogsSlice)
	if !ok {
		t.Fatalf("first exclude_pinned page = %T, want messages.dialogsSlice", firstPage)
	}
	if pageSlice.Count != 5 {
		t.Fatalf("first exclude_pinned count = %d, want 5", pageSlice.Count)
	}
	for _, raw := range firstRows {
		if dialog, ok := raw.(*tg.Dialog); ok && (dialog.Peer.String() == (&tg.PeerChat{ChatID: group.ID}).String() || dialog.Peer.String() == (&tg.PeerUser{UserID: b.ID}).String()) {
			t.Fatalf("exclude_pinned returned pinned peer %s", dialog.Peer)
		}
	}
	lastOrdinary, ok := firstRows[len(firstRows)-1].(*tg.Dialog)
	if !ok {
		t.Fatalf("last first-page dialog = %T, want ordinary dialog", firstRows[len(firstRows)-1])
	}
	secondPage, err := getDialogs(a, f.ctx, int64(lastOrdinary.TopMessage), 3, true)
	if err != nil {
		t.Fatalf("get second unpinned dialog page: %v", err)
	}
	if got := dialogPeers(secondPage); len(got) != 1 || got[0] != (&tg.PeerUser{UserID: c.ID}).String() {
		t.Fatalf("second exclude_pinned page peers = %v, want user %d and no repeated channel block", got, c.ID)
	}

	var isolated *tg.MessagesPeerDialogs
	isolated, err = getPinnedDialogs(other, f.ctx, 0)
	if err != nil {
		t.Fatalf("read another account's pins: %v", err)
	}
	if len(isolated.Dialogs) != 0 {
		t.Fatalf("another account saw %d of the owner's pins", len(isolated.Dialogs))
	}

	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)}); err != nil {
		t.Fatalf("pin current unbanned channel member dialog: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner channel pin refresh")
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, c.ID)}); err != nil {
		t.Fatalf("pin second 1:1 dialog: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner second 1:1 pin refresh")
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)}); err != nil {
		t.Fatalf("pin fifth default-folder peer: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner fifth pin refresh")
	fullPins, err := getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read five pins: %v", err)
	}
	fullOrder := []tg.PeerClass{
		&tg.PeerUser{UserID: d.ID}, &tg.PeerUser{UserID: c.ID}, &tg.PeerChannel{ChannelID: channel.ID},
		&tg.PeerChat{ChatID: group.ID}, &tg.PeerUser{UserID: b.ID},
	}
	assertPinnedPeerOrder(t, fullPins.Dialogs, fullOrder)
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, e.ID)}); expectTGError(err, "PINNED_DIALOGS_TOO_MUCH") != nil {
		t.Fatalf("sixth default-folder pin: %v", err)
	}
	assertNoPinRefresh(t, f.ctx, a2.push, "rejected sixth pin")
	fullPins, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read pins after sixth-pin rejection: %v", err)
	}
	assertPinnedPeerOrder(t, fullPins.Dialogs, fullOrder)
	if _, err := reorderDialogPins(a, f.ctx, 0, true, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}},
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, c.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, e.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, missing.ID)},
		&tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)},
	}); expectTGError(err, "PINNED_DIALOGS_TOO_MUCH") != nil {
		t.Fatalf("force reorder beyond five peers: %v", err)
	}
	fullPins, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read pins after over-cap reorder: %v", err)
	}
	assertPinnedPeerOrder(t, fullPins.Dialogs, fullOrder)

	if removed, _, _, err := f.store.RemoveChatUser(f.ctx, group.ID, a.id, b.ID); err != nil || !removed {
		t.Fatalf("remove pin owner from group: removed=%v err=%v", removed, err)
	}
	stalePins, err := getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read after group membership removal: %v", err)
	}
	assertPinnedPeerOrder(t, stalePins.Dialogs, []tg.PeerClass{
		&tg.PeerUser{UserID: d.ID}, &tg.PeerUser{UserID: c.ID}, &tg.PeerChannel{ChannelID: channel.ID}, &tg.PeerUser{UserID: b.ID},
	})
	ordinary, err = getDialogs(a, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("read forbidden retained group dialog: %v", err)
	}
	assertDialogPinned(t, ordinary, &tg.PeerChat{ChatID: group.ID}, false)
	assertForbiddenChat(t, ordinary, group.ID)
	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: peerUser(a.id, e.ID)}); err != nil {
		t.Fatalf("replacement pin after stale group cleanup: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "stale pin replacement refresh")
	assertFilterPinUnaffected(t, f, a.id, b.ID)

	// Non-force moves only already-pinned entries listed in the request. The
	// accessible unpinned peer stays ignored and the remaining pin order holds.
	if _, err := reorderDialogPins(a, f.ctx, 0, false, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peerUser(a.id, missing.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)},
		&tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)},
	}); err != nil {
		t.Fatalf("non-force reorder with accessible unpinned peer: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner non-force reorder refresh")
	current, err := getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read non-force reordered pins: %v", err)
	}
	assertPinnedPeerOrder(t, current.Dialogs, []tg.PeerClass{
		&tg.PeerUser{UserID: d.ID}, &tg.PeerChannel{ChannelID: channel.ID}, &tg.PeerUser{UserID: e.ID},
		&tg.PeerUser{UserID: c.ID}, &tg.PeerUser{UserID: b.ID},
	})
	if _, err := reorderDialogPins(a, f.ctx, 0, true, []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: &tg.InputPeerChat{ChatID: group.ID}},
	}); expectTGError(err, "PEER_ID_INVALID") != nil {
		t.Fatalf("reject whole reorder containing stale group: %v", err)
	}
	current, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read after invalid reorder: %v", err)
	}
	assertPinnedPeerOrder(t, current.Dialogs, []tg.PeerClass{
		&tg.PeerUser{UserID: d.ID}, &tg.PeerChannel{ChannelID: channel.ID}, &tg.PeerUser{UserID: e.ID},
		&tg.PeerUser{UserID: c.ID}, &tg.PeerUser{UserID: b.ID},
	})
	forceFinal := []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)},
	}
	if _, err := reorderDialogPins(a, f.ctx, 0, true, forceFinal); err != nil {
		t.Fatalf("force exact deduplicated replacement list: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "same-owner exact reorder refresh")
	current, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read exact force reorder: %v", err)
	}
	assertPinnedPeerOrder(t, current.Dialogs, []tg.PeerClass{
		&tg.PeerChannel{ChannelID: channel.ID}, &tg.PeerUser{UserID: b.ID}, &tg.PeerUser{UserID: d.ID},
	})
	if _, err := reorderDialogPins(a, f.ctx, 0, true, forceFinal); err != nil {
		t.Fatalf("repeat exact force reorder: %v", err)
	}
	assertNoPinRefresh(t, f.ctx, a2.push, "unchanged force order")
	assertFilterPinUnaffected(t, f, a.id, b.ID)

	baseline := current.State
	for _, peer := range []tg.InputDialogPeerClass{
		&tg.InputDialogPeer{Peer: peerChannel(a.id, channel.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		&tg.InputDialogPeer{Peer: peerUser(a.id, d.ID)},
	} {
		if _, err := toggleDialogPin(a, f.ctx, false, peer); err != nil {
			t.Fatalf("unpin last-list peer %v: %v", peer, err)
		}
		recvPinRefresh(t, f.ctx, a2.push, "same-owner unpin refresh")
	}
	listed, err = getPinnedDialogs(a, f.ctx, 0)
	if err != nil {
		t.Fatalf("read after removing final pin: %v", err)
	}
	if len(listed.Dialogs) != 0 {
		t.Fatalf("last unpin left %d visible pins", len(listed.Dialogs))
	}
	difference, err := getDifference(a, f.ctx, baseline, int(time.Now().Add(-time.Minute).Unix()))
	if err != nil {
		t.Fatalf("getDifference after missed last-unpin push: %v", err)
	}
	if !hasPinnedDialogsUpdate(difference) {
		t.Fatalf("getDifference after last unpin omitted UpdatePinnedDialogs: %T", difference)
	}

	if _, err := toggleDialogPin(a, f.ctx, true, &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}); err != nil {
		t.Fatalf("pin Saved Messages for reconnect persistence: %v", err)
	}
	recvPinRefresh(t, f.ctx, a2.push, "pre-reconnect Saved Messages pin refresh")
	a.stopClient(t)
	a2.stopClient(t)
	other.stopClient(t)
	f.restart(t)
	reconnected := f.savedSessionClient(a.session)
	if err := reconnected.Run(f.ctx, func(ctx context.Context) error {
		raw := tg.NewClient(reconnected)
		persisted, err := raw.MessagesGetPinnedDialogs(ctx, 0)
		if err != nil {
			return err
		}
		assertPinnedPeerOrder(t, persisted.Dialogs, []tg.PeerClass{&tg.PeerUser{UserID: a.id}})
		request := &tg.MessagesToggleDialogPinRequest{Pinned: false, Peer: &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}}
		request.SetFlags()
		if _, err := raw.MessagesToggleDialogPin(ctx, request); err != nil {
			return err
		}
		cleared, err := raw.MessagesGetPinnedDialogs(ctx, 0)
		if err != nil {
			return err
		}
		if len(cleared.Dialogs) != 0 {
			return fmt.Errorf("reconnected unpin left %d visible pins", len(cleared.Dialogs))
		}
		return nil
	}); err != nil {
		t.Fatalf("read and mutate persistent pins after reconnect: %v", err)
	}

	assertUnboundAndProvisionalPinCallsRejected(t, f)
}

func dialogPinUser(t *testing.T, f *smokeFixture, phone string) store.User {
	t.Helper()
	user, ok, err := f.store.UserByPhone(f.ctx, phone)
	if err != nil || !ok {
		t.Fatalf("look up dialog pin peer %q: ok=%v err=%v", phone, ok, err)
	}
	return user
}

func seedDialogPinDM(t *testing.T, f *smokeFixture, ownerID, peerID, randomID int64) {
	t.Helper()
	if _, _, _, _, err := f.store.SendMessage(f.ctx, ownerID, peerID, "dialog pin fixture", randomID, 0, 0); err != nil {
		t.Fatalf("seed dialog pin conversation with %d: %v", peerID, err)
	}
}

func toggleDialogPin(c *smokeClient, ctx context.Context, pinned bool, peer tg.InputDialogPeerClass) (bool, error) {
	var result bool
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesToggleDialogPinRequest{Pinned: pinned, Peer: peer}
		request.SetFlags()
		var err error
		result, err = api.MessagesToggleDialogPin(ctx, request)
		return err
	})
	if err == nil && !result {
		return false, errors.New("messages.toggleDialogPin returned BoolFalse")
	}
	return result, err
}

func reorderDialogPins(c *smokeClient, ctx context.Context, folderID int, force bool, order []tg.InputDialogPeerClass) (bool, error) {
	var result bool
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesReorderPinnedDialogsRequest{FolderID: folderID, Force: force, Order: order}
		request.SetFlags()
		var err error
		result, err = api.MessagesReorderPinnedDialogs(ctx, request)
		return err
	})
	if err == nil && !result {
		return false, errors.New("messages.reorderPinnedDialogs returned BoolFalse")
	}
	return result, err
}

func getPinnedDialogs(c *smokeClient, ctx context.Context, folderID int) (*tg.MessagesPeerDialogs, error) {
	var result *tg.MessagesPeerDialogs
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetPinnedDialogs(ctx, folderID)
		return err
	})
	return result, err
}

func getDialogs(c *smokeClient, ctx context.Context, offsetID int64, limit int, excludePinned bool) (tg.MessagesDialogsClass, error) {
	var result tg.MessagesDialogsClass
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesGetDialogsRequest{
			OffsetID:      int(offsetID),
			OffsetPeer:    &tg.InputPeerEmpty{},
			Limit:         limit,
			ExcludePinned: excludePinned,
		}
		request.SetFlags()
		var err error
		result, err = api.MessagesGetDialogs(ctx, request)
		return err
	})
	return result, err
}

func getDifference(c *smokeClient, ctx context.Context, state tg.UpdatesState, date int) (tg.UpdatesDifferenceClass, error) {
	var result tg.UpdatesDifferenceClass
	err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{
			Pts: state.Pts, Qts: state.Qts, Date: date,
		})
		return err
	})
	return result, err
}

func dialogRows(t *testing.T, result tg.MessagesDialogsClass) []tg.DialogClass {
	t.Helper()
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		return response.Dialogs
	case *tg.MessagesDialogsSlice:
		return response.Dialogs
	default:
		t.Fatalf("getDialogs response = %T, want messages.dialogs or messages.dialogsSlice", result)
		return nil
	}
}

func assertDialogPinned(t *testing.T, result tg.MessagesDialogsClass, peer tg.PeerClass, want bool) {
	t.Helper()
	for _, raw := range dialogRows(t, result) {
		dialog, ok := raw.(*tg.Dialog)
		if !ok || dialog.Peer.TypeID() != peer.TypeID() || dialog.Peer.String() != peer.String() {
			continue
		}
		if dialog.GetPinned() != want {
			t.Fatalf("dialog %s pinned=%v, want %v", peer, dialog.GetPinned(), want)
		}
		return
	}
	t.Fatalf("getDialogs omitted peer %s", peer)
}

func assertForbiddenChat(t *testing.T, result tg.MessagesDialogsClass, chatID int64) {
	t.Helper()
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		for _, raw := range response.Chats {
			if forbidden, ok := raw.(*tg.ChatForbidden); ok && forbidden.ID == chatID {
				if forbidden.Title != "" {
					t.Fatalf("removed member received forbidden chat title %q", forbidden.Title)
				}
				return
			}
		}
	case *tg.MessagesDialogsSlice:
		for _, raw := range response.Chats {
			if forbidden, ok := raw.(*tg.ChatForbidden); ok && forbidden.ID == chatID {
				if forbidden.Title != "" {
					t.Fatalf("removed member received forbidden chat title %q", forbidden.Title)
				}
				return
			}
		}
	}
	t.Fatalf("getDialogs omitted ChatForbidden for removed chat %d", chatID)
}

func assertPinnedStateMatchesUpdates(t *testing.T, c *smokeClient, ctx context.Context, got tg.UpdatesState) {
	t.Helper()
	if err := c.call(ctx, func(ctx context.Context, api *tg.Client) error {
		state, err := api.UpdatesGetState(ctx)
		if err != nil {
			return err
		}
		if got.Pts != state.Pts || got.Qts != state.Qts || got.Seq != state.Seq || got.Date != state.Date || got.UnreadCount != state.UnreadCount {
			return fmt.Errorf("pinned state = %+v, updates state = %+v", got, state)
		}
		return nil
	}); err != nil {
		t.Fatalf("compare getPinnedDialogs state: %v", err)
	}
}

func assertFilterPinUnaffected(t *testing.T, f *smokeFixture, ownerID, peerID int64) {
	t.Helper()
	filters, err := f.store.DialogFilterDefinitions(f.ctx, ownerID)
	if err != nil {
		t.Fatalf("read custom dialog folder: %v", err)
	}
	for _, filter := range filters {
		if filter.ID != 2 {
			continue
		}
		if len(filter.PinnedPeers) != 1 || filter.PinnedPeers[0] != (store.DialogFilterPeer{Type: store.PeerTypeUser, ID: peerID}) {
			t.Fatalf("custom-folder pins changed with default-folder mutations: %+v", filter.PinnedPeers)
		}
		return
	}
	t.Fatal("custom folder disappeared after default-folder pin mutations")
}

func assertNoPinRefresh(t *testing.T, ctx context.Context, updates *updateCollector, label string) {
	t.Helper()
	select {
	case <-updates.pinnedDialogs:
		t.Fatalf("%s emitted an unchanged UpdatePinnedDialogs refresh", label)
	case <-time.After(150 * time.Millisecond):
	case <-ctx.Done():
		t.Fatalf("%s wait ended: %v", label, ctx.Err())
	}
}

func hasPinnedDialogsUpdate(difference tg.UpdatesDifferenceClass) bool {
	var updates []tg.UpdateClass
	switch response := difference.(type) {
	case *tg.UpdatesDifference:
		updates = response.OtherUpdates
	case *tg.UpdatesDifferenceSlice:
		updates = response.OtherUpdates
	}
	for _, update := range updates {
		if _, ok := update.(*tg.UpdatePinnedDialogs); ok {
			return true
		}
	}
	return false
}

func assertUnboundAndProvisionalPinCallsRejected(t *testing.T, f *smokeFixture) {
	t.Helper()
	storage := &session.StorageMemory{}
	unbound := f.savedSessionClient(storage)
	if err := unbound.Run(f.ctx, func(ctx context.Context) error {
		assertPinCallsRejected(t, ctx, tg.NewClient(unbound), "unbound")
		return nil
	}); err != nil {
		t.Fatalf("run unbound pin checks: %v", err)
	}
	data, err := (&session.Loader{Storage: storage}).Load(f.ctx)
	if err != nil {
		t.Fatalf("load unbound session: %v", err)
	}
	var authKeyID [8]byte
	if len(data.AuthKeyID) != len(authKeyID) {
		t.Fatalf("unbound auth key id length = %d, want %d", len(data.AuthKeyID), len(authKeyID))
	}
	copy(authKeyID[:], data.AuthKeyID)
	username := fmt.Sprintf("pinprov%d", time.Now().UnixNano())
	provisional, err := f.store.CreateUsernameUser(f.ctx, username, "Pin", "Provisional")
	if err != nil {
		t.Fatalf("create provisional account: %v", err)
	}
	if err := f.store.ClaimUsername(f.ctx, provisional.ID, username); err != nil {
		t.Fatalf("claim provisional username: %v", err)
	}
	if err := f.store.BindAuthKeyUser(f.ctx, mtproto.AuthKeyIDInt64(authKeyID), provisional.ID); err != nil {
		t.Fatalf("bind auth key to provisional account: %v", err)
	}
	provisionalClient := f.savedSessionClient(storage)
	if err := provisionalClient.Run(f.ctx, func(ctx context.Context) error {
		assertPinCallsRejected(t, ctx, tg.NewClient(provisionalClient), "provisional")
		return nil
	}); err != nil {
		t.Fatalf("run provisional pin checks: %v", err)
	}
	for attempt := range 60 {
		result, err := f.store.CheckRateLimitCost(f.ctx, provisional.ID, "dialog_filter_mutation", store.RateLimitConfig{Limit: 60, Window: time.Minute}, 1)
		if err != nil || result != nil {
			t.Fatalf("provisional pin RPC consumed rate budget before attempt %d: result=%+v err=%v", attempt+1, result, err)
		}
	}
	if result, err := f.store.CheckRateLimitCost(f.ctx, provisional.ID, "dialog_filter_mutation", store.RateLimitConfig{Limit: 60, Window: time.Minute}, 1); err != nil || result == nil {
		t.Fatalf("60-call shared budget result=%+v err=%v, want exhausted", result, err)
	}
}

func assertPinCallsRejected(t *testing.T, ctx context.Context, api *tg.Client, label string) {
	t.Helper()
	if _, err := api.MessagesGetPinnedDialogs(ctx, 0); expectTGError(err, "AUTH_KEY_UNREGISTERED") != nil {
		t.Errorf("%s getPinnedDialogs: %v", label, err)
	}
	toggle := &tg.MessagesToggleDialogPinRequest{Pinned: true, Peer: &tg.InputDialogPeer{Peer: &tg.InputPeerSelf{}}}
	toggle.SetFlags()
	if _, err := api.MessagesToggleDialogPin(ctx, toggle); expectTGError(err, "AUTH_KEY_UNREGISTERED") != nil {
		t.Errorf("%s toggleDialogPin: %v", label, err)
	}
	reorder := &tg.MessagesReorderPinnedDialogsRequest{FolderID: 0, Force: true}
	reorder.SetFlags()
	if _, err := api.MessagesReorderPinnedDialogs(ctx, reorder); expectTGError(err, "AUTH_KEY_UNREGISTERED") != nil {
		t.Errorf("%s reorderPinnedDialogs: %v", label, err)
	}
}

func testSmokeDialogPins(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049041", "+15551049042"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a := newSmokeClient(t, f, "dialog pin smoke owner", phoneA)
	b, ok, err := f.store.UserByPhone(f.ctx, phoneB)
	if err != nil || !ok {
		t.Fatalf("look up smoke pin peer: ok=%v err=%v", ok, err)
	}
	if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, b.ID, "smoke pinned dialog", 1049041, 0, 0); err != nil {
		t.Fatalf("seed smoke pin dialog: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		request := &tg.MessagesToggleDialogPinRequest{
			Pinned: true,
			Peer:   &tg.InputDialogPeer{Peer: peerUser(a.id, b.ID)},
		}
		request.SetFlags()
		_, err := api.MessagesToggleDialogPin(ctx, request)
		return err
	}); err != nil {
		t.Fatalf("smoke pin 1:1 dialog: %v", err)
	}
	var result *tg.MessagesPeerDialogs
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetPinnedDialogs(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("smoke read pinned dialogs: %v", err)
	}
	assertPinnedPeerOrder(t, result.Dialogs, []tg.PeerClass{&tg.PeerUser{UserID: b.ID}})
}

func recvPinRefresh(t *testing.T, ctx context.Context, updates *updateCollector, label string) {
	t.Helper()
	select {
	case <-updates.pinnedDialogs:
	case <-ctx.Done():
		t.Fatalf("%s timed out: %v", label, ctx.Err())
	}
}

func assertPinnedPeerOrder(t *testing.T, raw []tg.DialogClass, want []tg.PeerClass) {
	t.Helper()
	if len(raw) != len(want) {
		t.Fatalf("pinned dialog count = %d, want %d", len(raw), len(want))
	}
	got := make([]string, 0, len(raw))
	for i, item := range raw {
		dialog, ok := item.(*tg.Dialog)
		if !ok {
			t.Fatalf("pinned dialog %d = %T, want *tg.Dialog", i, item)
		}
		got = append(got, dialog.Peer.String())
	}
	expected := make([]string, 0, len(want))
	for _, peer := range want {
		expected = append(expected, peer.String())
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("pinned dialog peers = %v, want %v", got, expected)
	}
}

func dialogPeers(result tg.MessagesDialogsClass) []string {
	var dialogs []tg.DialogClass
	switch v := result.(type) {
	case *tg.MessagesDialogs:
		dialogs = v.Dialogs
	case *tg.MessagesDialogsSlice:
		dialogs = v.Dialogs
	}
	out := make([]string, 0, len(dialogs))
	for _, item := range dialogs {
		if dialog, ok := item.(*tg.Dialog); ok {
			out = append(out, dialog.Peer.String())
		}
	}
	return out
}

func expectTGError(err error, want string) error {
	if err == nil {
		return fmt.Errorf("expected %s, got no error", want)
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) || rpc.Message != want {
		return fmt.Errorf("error = %w, want %s", err, want)
	}
	return nil
}
