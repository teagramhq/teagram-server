package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/session"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestPeerSettings(t *testing.T) {
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551370001", "+15551370002", "+15551370003"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB, phoneC)

	a := newSmokeClient(t, f, "peer-settings A", phoneA)
	c := newSmokeClient(t, f, "peer-settings C", phoneC)
	b, ok, err := f.store.UserByPhone(f.ctx, phoneB)
	if err != nil || !ok {
		t.Fatalf("B lookup: ok=%v err=%v", ok, err)
	}
	if _, _, _, _, err := f.store.SendMessage(f.ctx, a.id, b.ID, "peer-settings seed", 1370001, 0, 0); err != nil {
		t.Fatalf("seed A/B dialog: %v", err)
	}

	listener, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect notification observer: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(context.Background()); err != nil {
			t.Errorf("close notification observer: %v", err)
		}
	})
	if _, err := listener.Exec(f.ctx, "LISTEN "+store.ChannelUpdates); err != nil {
		t.Fatalf("listen for update publication: %v", err)
	}

	before := peerSettingsEffectsSnapshot(t, f, listener, a.id)
	var initial *tg.MessagesPeerSettings
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		initial, err = api.MessagesGetPeerSettings(ctx, peerUser(a.id, b.ID))
		return err
	}); err != nil {
		t.Fatalf("get existing non-contact peer settings: %v", err)
	}
	after := peerSettingsEffectsSnapshot(t, f, listener, a.id)
	assertPeerSettingsEffectsUnchanged(t, before, after)
	assertNoPeerSettingsUpdateNotification(t, f.ctx, listener)
	assertPeerSettings(t, initial, true, true)
	initialPeer := requirePeerSettingsUser(t, initial, b.ID)
	if initialPeer.Contact || initialPeer.MutualContact {
		t.Fatalf("initial peer contact flags = %v/%v, want caller-owned false/false", initialPeer.Contact, initialPeer.MutualContact)
	}
	initialBytes := encodePeerSettings(t, initial)

	// Changes owned by B must not change A's response, including the explicit
	// user object carried alongside PeerSettings.
	if _, err := f.store.AddContact(f.ctx, b.ID, a.id); err != nil {
		t.Fatalf("add caller to B's contacts: %v", err)
	}
	if _, err := f.store.BlockUser(f.ctx, b.ID, a.id); err != nil {
		t.Fatalf("block caller from B: %v", err)
	}
	unchanged := getPeerSettings(t, f.ctx, a, peerUser(a.id, b.ID))
	if got := encodePeerSettings(t, unchanged); !bytes.Equal(got, initialBytes) {
		t.Fatalf("peer-owned contact/block state changed caller response bytes")
	}
	assertPeerSettings(t, unchanged, true, true)
	if user := requirePeerSettingsUser(t, unchanged, b.ID); user.Contact || user.MutualContact {
		t.Fatalf("peer-owned contact appeared in caller response: contact=%v mutual=%v", user.Contact, user.MutualContact)
	}

	withoutDialog := getPeerSettings(t, f.ctx, a, peerUser(a.id, c.id))
	assertPeerSettings(t, withoutDialog, false, false)
	requirePeerSettingsUser(t, withoutDialog, c.id)
	self := getPeerSettings(t, f.ctx, a, &tg.InputPeerSelf{})
	assertPeerSettings(t, self, false, false)
	requirePeerSettingsUser(t, self, a.id)

	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsAddContact(ctx, &tg.ContactsAddContactRequest{ID: inputUser(a.id, b.ID)})
		return err
	}); err != nil {
		t.Fatalf("add caller-owned contact: %v", err)
	}
	withContact := getPeerSettings(t, f.ctx, a, peerUser(a.id, b.ID))
	assertPeerSettings(t, withContact, false, true)
	withContactUser := requirePeerSettingsUser(t, withContact, b.ID)
	if !withContactUser.Contact || withContactUser.MutualContact {
		t.Fatalf("returned user contact flags = %v/%v, want caller edge without peer-owned mutual state", withContactUser.Contact, withContactUser.MutualContact)
	}

	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(a.id, b.ID)})
		return err
	}); err != nil {
		t.Fatalf("block caller-owned peer: %v", err)
	}
	withBlock := getPeerSettings(t, f.ctx, a, peerUser(a.id, b.ID))
	assertPeerSettings(t, withBlock, false, false)

	chat, err := f.store.CreateChat(f.ctx, a.id, "Peer settings group", []int64{a.id, b.ID})
	if err != nil {
		t.Fatalf("create member-only basic chat: %v", err)
	}
	chatSettings := getPeerSettings(t, f.ctx, a, &tg.InputPeerChat{ChatID: chat.ID})
	assertEmptyPeerSettings(t, chatSettings)
	if len(chatSettings.Chats) != 1 {
		t.Fatalf("basic chat response chats = %d, want one", len(chatSettings.Chats))
	}
	if got, ok := chatSettings.Chats[0].(*tg.Chat); !ok || got.ID != chat.ID {
		t.Fatalf("basic chat response = %T/%v, want current member chat %d", chatSettings.Chats[0], chatSettings.Chats[0], chat.ID)
	}

	channel, err := f.store.CreateChannel(f.ctx, a.id, "Peer settings channel", "", true)
	if err != nil {
		t.Fatalf("create member-only channel: %v", err)
	}
	channelSettings := getPeerSettings(t, f.ctx, a, peerChannel(a.id, channel.ID))
	assertEmptyPeerSettings(t, channelSettings)
	if len(channelSettings.Chats) != 1 {
		t.Fatalf("channel response chats = %d, want one", len(channelSettings.Chats))
	}
	if got, ok := channelSettings.Chats[0].(*tg.Channel); !ok || got.ID != channel.ID {
		t.Fatalf("channel response = %T/%v, want current member channel %d", channelSettings.Chats[0], channelSettings.Chats[0], channel.ID)
	}

	forgedUser := *peerUser(a.id, b.ID)
	forgedUser.AccessHash ^= 1
	forgedUserErr := peerSettingsRPCError(t, f.ctx, a, &forgedUser, "PEER_ID_INVALID")
	missingUserErr := peerSettingsRPCError(t, f.ctx, a, peerUser(a.id, 1<<55), "PEER_ID_INVALID")
	if forgedUserErr.Error() != missingUserErr.Error() {
		t.Fatalf("forged user hash error %q differs from missing user error %q", forgedUserErr, missingUserErr)
	}

	unknownPeerID := int64(1 << 55)
	unknownChatErr := peerSettingsRPCError(t, f.ctx, c, &tg.InputPeerChat{ChatID: unknownPeerID}, "PEER_ID_INVALID")
	nonmemberChatErr := peerSettingsRPCError(t, f.ctx, c, &tg.InputPeerChat{ChatID: chat.ID}, "PEER_ID_INVALID")
	if unknownChatErr.Error() != nonmemberChatErr.Error() {
		t.Fatalf("unknown chat error %q differs from non-member chat error %q", unknownChatErr, nonmemberChatErr)
	}
	unknownChannelErr := peerSettingsRPCError(t, f.ctx, c, peerChannel(c.id, unknownPeerID), "PEER_ID_INVALID")
	nonmemberChannelErr := peerSettingsRPCError(t, f.ctx, c, peerChannel(c.id, channel.ID), "PEER_ID_INVALID")
	if unknownChannelErr.Error() != nonmemberChannelErr.Error() {
		t.Fatalf("unknown channel error %q differs from non-member channel error %q", unknownChannelErr, nonmemberChannelErr)
	}

	assertPeerSettingsAuthKeyGates(t, f, a, b.ID)
}

type peerSettingsEffects struct {
	state      store.State
	dialogs    []store.Dialog
	events     int64
	rateLimits int64
	contacts   int64
	blocks     int64
}

func peerSettingsEffectsSnapshot(t *testing.T, f *smokeFixture, conn *pgx.Conn, userID int64) peerSettingsEffects {
	t.Helper()
	var snapshot peerSettingsEffects
	var err error
	snapshot.state, err = f.store.State(f.ctx, userID)
	if err != nil {
		t.Fatalf("read update state: %v", err)
	}
	snapshot.dialogs, err = f.store.Dialogs(f.ctx, userID, 0, 100)
	if err != nil {
		t.Fatalf("read dialogs: %v", err)
	}
	for query, target := range map[string]*int64{
		"SELECT count(*) FROM message_events WHERE owner_id = $1":  &snapshot.events,
		"SELECT count(*) FROM rate_limits WHERE subject_id = $1":   &snapshot.rateLimits,
		"SELECT count(*) FROM user_contacts WHERE owner_id = $1":   &snapshot.contacts,
		"SELECT count(*) FROM blocked_users WHERE blocker_id = $1": &snapshot.blocks,
	} {
		if err := conn.QueryRow(f.ctx, query, userID).Scan(target); err != nil {
			t.Fatalf("read peer settings side effects: %v", err)
		}
	}
	return snapshot
}

func assertPeerSettingsEffectsUnchanged(t *testing.T, before, after peerSettingsEffects) {
	t.Helper()
	if before.state != after.state || !slices.Equal(before.dialogs, after.dialogs) ||
		before.events != after.events || before.rateLimits != after.rateLimits ||
		before.contacts != after.contacts || before.blocks != after.blocks {
		t.Fatalf("getPeerSettings changed state: before=%+v after=%+v", before, after)
	}
}

func assertNoPeerSettingsUpdateNotification(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	notification, err := conn.WaitForNotification(waitCtx)
	if err == nil {
		t.Fatalf("getPeerSettings published update notification: %+v", notification)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for getPeerSettings notification: %v", err)
	}
}

func assertPeerSettings(t *testing.T, got *tg.MessagesPeerSettings, addContact, blockContact bool) {
	t.Helper()
	if got == nil {
		t.Fatal("getPeerSettings response is nil")
	}
	wantFlags := bin.Fields(0)
	if addContact {
		wantFlags.Set(1)
	}
	if blockContact {
		wantFlags.Set(2)
	}
	if got.Settings.Flags != wantFlags || got.Settings.AddContact != addContact || got.Settings.BlockContact != blockContact {
		t.Fatalf("peer settings flags/add/block = %032b/%v/%v, want %032b/%v/%v", got.Settings.Flags, got.Settings.AddContact, got.Settings.BlockContact, wantFlags, addContact, blockContact)
	}
	if got.Settings.ReportSpam || got.Settings.ShareContact || got.Settings.NeedContactsException ||
		got.Settings.ReportGeo || got.Settings.Autoarchived || got.Settings.InviteMembers ||
		got.Settings.RequestChatBroadcast || got.Settings.BusinessBotPaused || got.Settings.BusinessBotCanReply {
		t.Fatalf("peer settings advertise an unsupported boolean field: %+v", got.Settings)
	}
	for _, present := range peerSettingsOptionalFieldsPresent(got.Settings) {
		if present {
			t.Fatalf("peer settings advertise an unsupported optional field: %+v", got.Settings)
		}
	}
}

func peerSettingsOptionalFieldsPresent(settings tg.PeerSettings) []bool {
	_, geo := settings.GetGeoDistance()
	_, requestTitle := settings.GetRequestChatTitle()
	_, requestDate := settings.GetRequestChatDate()
	_, businessBotID := settings.GetBusinessBotID()
	_, businessBotURL := settings.GetBusinessBotManageURL()
	_, paidMessage := settings.GetChargePaidMessageStars()
	_, registration := settings.GetRegistrationMonth()
	_, phoneCountry := settings.GetPhoneCountry()
	_, nameChange := settings.GetNameChangeDate()
	_, photoChange := settings.GetPhotoChangeDate()
	return []bool{geo, requestTitle, requestDate, businessBotID, businessBotURL, paidMessage, registration, phoneCountry, nameChange, photoChange}
}

func assertEmptyPeerSettings(t *testing.T, got *tg.MessagesPeerSettings) {
	t.Helper()
	if got == nil || !got.Settings.Zero() || len(got.Users) != 0 {
		t.Fatalf("group peer settings response = %+v, want empty settings and no users", got)
	}
}

func requirePeerSettingsUser(t *testing.T, got *tg.MessagesPeerSettings, userID int64) *tg.User {
	t.Helper()
	if got == nil || len(got.Users) != 1 {
		if got == nil {
			t.Fatal("getPeerSettings response is nil")
		}
		t.Fatalf("getPeerSettings users = %d, want the explicit peer %d", len(got.Users), userID)
	}
	user, ok := got.Users[0].(*tg.User)
	if !ok || user.ID != userID {
		t.Fatalf("getPeerSettings user = %T/%v, want explicit user %d", got.Users[0], got.Users[0], userID)
	}
	return user
}

func getPeerSettings(t *testing.T, ctx context.Context, caller *smokeClient, peer tg.InputPeerClass) *tg.MessagesPeerSettings {
	t.Helper()
	var result *tg.MessagesPeerSettings
	if err := caller.call(ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesGetPeerSettings(ctx, peer)
		return err
	}); err != nil {
		t.Fatalf("messages.getPeerSettings: %v", err)
	}
	return result
}

func peerSettingsRPCError(t *testing.T, ctx context.Context, caller *smokeClient, peer tg.InputPeerClass, want string) error {
	t.Helper()
	err := caller.call(ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesGetPeerSettings(ctx, peer)
		return err
	})
	if err == nil || !tgerr.Is(err, want) {
		t.Fatalf("messages.getPeerSettings error = %v, want %s", err, want)
	}
	return err
}

func encodePeerSettings(t *testing.T, response *tg.MessagesPeerSettings) []byte {
	t.Helper()
	var buffer bin.Buffer
	if err := buffer.Encode(response); err != nil {
		t.Fatalf("encode peer settings response: %v", err)
	}
	return buffer.Copy()
}

func assertPeerSettingsAuthKeyGates(t *testing.T, f *smokeFixture, owner *smokeClient, peerID int64) {
	t.Helper()
	unbound := f.savedSessionClient(&session.StorageMemory{})
	if err := unbound.Run(f.ctx, func(ctx context.Context) error {
		_, err := tg.NewClient(unbound).MessagesGetPeerSettings(ctx, &tg.InputPeerSelf{})
		if err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			t.Errorf("unbound messages.getPeerSettings error = %v, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("run unbound peer settings check: %v", err)
	}

	data, err := (&session.Loader{Storage: owner.session}).Load(f.ctx)
	if err != nil {
		t.Fatalf("load caller session: %v", err)
	}
	var authKeyID [8]byte
	if len(data.AuthKeyID) != len(authKeyID) {
		t.Fatalf("caller auth key id length = %d, want %d", len(data.AuthKeyID), len(authKeyID))
	}
	copy(authKeyID[:], data.AuthKeyID)
	username := fmt.Sprintf("peersettings%d", time.Now().UnixNano())
	provisional, err := f.store.CreateUsernameUser(f.ctx, username, "Peer", "Settings")
	if err != nil {
		t.Fatalf("create provisional user: %v", err)
	}
	if err := f.store.ClaimUsername(f.ctx, provisional.ID, username); err != nil {
		t.Fatalf("claim provisional username: %v", err)
	}
	if err := f.store.BindAuthKeyUser(f.ctx, mtproto.AuthKeyIDInt64(authKeyID), provisional.ID); err != nil {
		t.Fatalf("bind caller auth key to provisional user: %v", err)
	}
	owner.stopClient(t)
	provisionalClient := f.savedSessionClient(owner.session)
	if err := provisionalClient.Run(f.ctx, func(ctx context.Context) error {
		_, err := tg.NewClient(provisionalClient).MessagesGetPeerSettings(ctx, peerUser(provisional.ID, peerID))
		if err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			t.Errorf("provisional messages.getPeerSettings error = %v, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("run provisional peer settings check: %v", err)
	}
}
