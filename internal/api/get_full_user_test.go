package api_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/store"
)

// getFullUserForTest calls users.getFullUser for viewerID and hands back the
// decoded response, failing the test on any error.
func getFullUserForTest(t *testing.T, s *store.Store, viewerID int64, id tg.InputUserClass) *tg.UsersUserFull {
	t.Helper()
	res, err := api.GetFullUserForTest(s, viewerID, &tg.UsersGetFullUserRequest{ID: id})
	if err != nil {
		t.Fatalf("users.getFullUser: %v", err)
	}
	full, ok := res.(*tg.UsersUserFull)
	if !ok {
		t.Fatalf("users.getFullUser result type = %T, want *tg.UsersUserFull", res)
	}
	assertEncodes(t, res)
	return full
}

// getFullUserErrorBytes is the RPC identity of a users.getFullUser failure, so a
// missing row and a forged hash can be compared byte for byte.
func getFullUserErrorBytes(t *testing.T, s *store.Store, viewerID int64, id tg.InputUserClass) []byte {
	t.Helper()
	_, err := api.GetFullUserForTest(s, viewerID, &tg.UsersGetFullUserRequest{ID: id})
	if err == nil {
		t.Fatal("users.getFullUser: expected error, got nil")
	}
	var rpc *tgerr.Error
	if !errors.As(err, &rpc) {
		t.Fatalf("users.getFullUser error = %v, want RPC error", err)
	}
	return fmt.Appendf(nil, "%d\x00%s\x00%s\x00%d", rpc.Code, rpc.Type, rpc.Message, rpc.Argument)
}

func getPeerSettingsForTest(t *testing.T, s *store.Store, viewerID int64, peer tg.InputPeerClass) *tg.MessagesPeerSettings {
	t.Helper()
	res, err := api.GetPeerSettingsForTest(s, viewerID, &tg.MessagesGetPeerSettingsRequest{Peer: peer})
	if err != nil {
		t.Fatalf("messages.getPeerSettings: %v", err)
	}
	settings, ok := res.(*tg.MessagesPeerSettings)
	if !ok {
		t.Fatalf("messages.getPeerSettings result type = %T, want *tg.MessagesPeerSettings", res)
	}
	assertEncodes(t, res)
	return settings
}

func encodeFullUser(t *testing.T, full *tg.UsersUserFull) []byte {
	t.Helper()
	var buf bin.Buffer
	if err := full.Encode(&buf); err != nil {
		t.Fatalf("encode full user response: %v", err)
	}
	return buf.Copy()
}

// requireFullUserTarget asserts the response names exactly one user, the
// target, and returns its wire record.
func requireFullUserTarget(t *testing.T, full *tg.UsersUserFull, userID int64) *tg.User {
	t.Helper()
	if len(full.Users) != 1 {
		t.Fatalf("users.getFullUser users = %d, want exactly the target %d", len(full.Users), userID)
	}
	user, ok := full.Users[0].(*tg.User)
	if !ok || user.ID != userID {
		t.Fatalf("users.getFullUser user = %T/%v, want target %d", full.Users[0], full.Users[0], userID)
	}
	return user
}

// fullUserOptionalFieldsPresent reports, per field, whether UserFull claims to
// carry it. An absent optional field is a server that says "not stored"; an
// empty-but-present one is a server claiming a value it cannot back, which is
// why each one is asserted individually instead of accepting a photoEmpty.
func fullUserOptionalFieldsPresent(full tg.UserFull) map[string]bool {
	_, about := full.GetAbout()
	_, profilePhoto := full.GetProfilePhoto()
	_, personalPhoto := full.GetPersonalPhoto()
	_, fallbackPhoto := full.GetFallbackPhoto()
	_, botInfo := full.GetBotInfo()
	_, pinnedMsgID := full.GetPinnedMsgID()
	_, folderID := full.GetFolderID()
	_, ttl := full.GetTTLPeriod()
	_, theme := full.GetTheme()
	_, forwardName := full.GetPrivateForwardName()
	_, wallpaper := full.GetWallpaper()
	_, stories := full.GetStories()
	_, savedMusic := full.GetSavedMusic()
	_, note := full.GetNote()
	_, birthday := full.GetBirthday()
	_, mainTab := full.GetMainTab()
	_, botRights := full.GetBotGroupAdminRights()
	_, broadcastRights := full.GetBotBroadcastAdminRights()
	_, botVerification := full.GetBotVerification()
	_, botManagerID := full.GetBotManagerID()
	_, personalChannel := full.GetPersonalChannelID()
	_, personalChannelMsg := full.GetPersonalChannelMessage()
	_, workHours := full.GetBusinessWorkHours()
	_, location := full.GetBusinessLocation()
	_, intro := full.GetBusinessIntro()
	_, greeting := full.GetBusinessGreetingMessage()
	_, away := full.GetBusinessAwayMessage()
	_, starsRating := full.GetStarsRating()
	_, starsPending := full.GetStarsMyPendingRating()
	_, starsPendingDate := full.GetStarsMyPendingRatingDate()
	_, stargifts := full.GetStargiftsCount()
	_, starref := full.GetStarrefProgram()
	_, paidStars := full.GetSendPaidMessagesStars()
	_, disallowedGifts := full.GetDisallowedGifts()
	return map[string]bool{
		"about":                    about,
		"profile_photo":            profilePhoto,
		"personal_photo":           personalPhoto,
		"fallback_photo":           fallbackPhoto,
		"bot_info":                 botInfo,
		"pinned_msg_id":            pinnedMsgID,
		"folder_id":                folderID,
		"ttl_period":               ttl,
		"theme":                    theme,
		"private_forward_name":     forwardName,
		"wallpaper":                wallpaper,
		"stories":                  stories,
		"saved_music":              savedMusic,
		"note":                     note,
		"birthday":                 birthday,
		"main_tab":                 mainTab,
		"bot_group_admin_rights":   botRights,
		"bot_broadcast_rights":     broadcastRights,
		"bot_verification":         botVerification,
		"bot_manager_id":           botManagerID,
		"personal_channel_id":      personalChannel,
		"personal_channel_message": personalChannelMsg,
		"business_work_hours":      workHours,
		"business_location":        location,
		"business_intro":           intro,
		"business_greeting":        greeting,
		"business_away":            away,
		"stars_rating":             starsRating,
		"stars_pending_rating":     starsPending,
		"stars_pending_date":       starsPendingDate,
		"stargifts_count":          stargifts,
		"starref_program":          starref,
		"send_paid_messages_stars": paidStars,
		"disallowed_gifts":         disallowedGifts,
	}
}

// assertFullUserTruthful pins the capability flags and optional fields every
// caller sees, self or not: the server stores no calls, pins, folders, themes,
// TTL, notify state, gifts or stories, so it reports none of them.
func assertFullUserTruthful(t *testing.T, full tg.UserFull, commonChatsCount int) {
	t.Helper()
	if full.PhoneCallsAvailable || full.PhoneCallsPrivate || full.VideoCallsAvailable ||
		full.CanPinMessage || full.HasScheduled || full.VoiceMessagesForbidden ||
		full.ReadDatesPrivate || full.TranslationsDisabled {
		t.Errorf("full user advertises an unsupported capability flag: %+v", full)
	}
	if full.BlockedMyStoriesFrom || full.StoriesPinnedAvailable || full.WallpaperOverridden ||
		full.ContactRequirePremium || full.SponsoredEnabled || full.CanViewRevenue ||
		full.BotCanManageEmojiStatus || full.DisplayGiftsButton || full.NoforwardsMyEnabled ||
		full.NoforwardsPeerEnabled || full.UnofficialSecurityRisk {
		t.Errorf("full user advertises an unsupported boolean field: %+v", full)
	}
	for field, present := range fullUserOptionalFieldsPresent(full) {
		if present {
			t.Errorf("full user advertises optional field %q it does not store", field)
		}
	}
	if !full.NotifySettings.Zero() {
		t.Errorf("full user notify settings = %+v, want empty peerNotifySettings", full.NotifySettings)
	}
	if full.CommonChatsCount != commonChatsCount {
		t.Errorf("full user common_chats_count = %d, want %d", full.CommonChatsCount, commonChatsCount)
	}
}

func TestGetFullUserSelfCarriesPhoneAndEmptyBar(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	caller := chatUser(t, s, 901)
	peer := chatUser(t, s, 902)

	for name, id := range map[string]tg.InputUserClass{
		"inputUserSelf":   &tg.InputUserSelf{},
		"own id and hash": &tg.InputUser{UserID: caller.ID, AccessHash: api.DeriveUserHash(caller.ID, caller.ID)},
	} {
		t.Run(name, func(t *testing.T) {
			full := getFullUserForTest(t, s, caller.ID, id)
			if full.FullUser.ID != caller.ID {
				t.Fatalf("%s full user id = %d, want caller %d", name, full.FullUser.ID, caller.ID)
			}
			if full.FullUser.Blocked {
				t.Errorf("%s self full user blocked = true, want false", name)
			}
			if !full.FullUser.Settings.Zero() {
				t.Errorf("%s self settings = %+v, want an empty bar", name, full.FullUser.Settings)
			}
			assertFullUserTruthful(t, full.FullUser, 0)
			if len(full.Chats) != 0 {
				t.Errorf("%s chats = %d, want an empty vector", name, len(full.Chats))
			}
			user := requireFullUserTarget(t, full, caller.ID)
			if !user.Self || user.Phone != caller.Phone {
				t.Errorf("%s self user = {self:%t phone:%q}, want self with own phone %q", name, user.Self, user.Phone, caller.Phone)
			}
			if user.AccessHash != api.DeriveUserHash(caller.ID, caller.ID) {
				t.Errorf("%s self access_hash = %d, want the caller's own hash", name, user.AccessHash)
			}
		})
	}

	// A peer's view of the caller is not self and never carries the phone.
	other := requireFullUserTarget(t, getFullUserForTest(t, s, peer.ID, api.InputUser(peer.ID, caller.ID)), caller.ID)
	if other.Self || other.Phone != "" {
		t.Errorf("peer view of caller = {self:%t phone:%q}, want neither", other.Self, other.Phone)
	}
}

func TestGetFullUserServesSearchOnlyPeerWithoutPhone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	caller := chatUser(t, s, 903)
	target := chatUser(t, s, 904)
	if err := api.ClaimUsernameForTest(s, target.ID, "fulluserpeer"); err != nil {
		t.Fatal(err)
	}

	found, err := api.ContactsSearchForTest(s, caller.ID, &tg.ContactsSearchRequest{Q: "fulluserpeer", Limit: 10})
	if err != nil {
		t.Fatalf("contacts.search: %v", err)
	}
	search, ok := found.(*tg.ContactsFound)
	if !ok || len(search.Users) != 1 {
		t.Fatalf("contacts.search = %#v, want one exact user", found)
	}
	searchUser, ok := search.Users[0].(*tg.User)
	if !ok {
		t.Fatalf("contacts.search user type = %T, want *tg.User", search.Users[0])
	}
	history, err := s.History(ctx, caller.ID, store.PeerTypeUser, target.ID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 0 {
		t.Fatalf("history before search-only profile = %d messages, want no dialog", len(history))
	}

	full := getFullUserForTest(t, s, caller.ID, &tg.InputUser{UserID: searchUser.ID, AccessHash: searchUser.AccessHash})
	if full.FullUser.ID != target.ID {
		t.Fatalf("full user id = %d, want searched target %d", full.FullUser.ID, target.ID)
	}
	if full.FullUser.Blocked {
		t.Errorf("search-only full user blocked = true, want false")
	}
	if !full.FullUser.Settings.Zero() {
		t.Errorf("search-only settings = %+v, want an empty bar without a dialog", full.FullUser.Settings)
	}
	assertFullUserTruthful(t, full.FullUser, 0)
	user := requireFullUserTarget(t, full, target.ID)
	stored, foundUser, err := s.UserByID(ctx, target.ID)
	if err != nil || !foundUser {
		t.Fatalf("load target: found=%v err=%v", foundUser, err)
	}
	if user.Phone != "" || user.Self || user.Contact {
		t.Errorf("search-only user = {self:%t phone:%q contact:%t}, want a public view with no phone", user.Self, user.Phone, user.Contact)
	}
	if user.FirstName != stored.FirstName || user.Username != *stored.Username || user.AccessHash != searchUser.AccessHash {
		t.Errorf("search-only user = {first_name:%q username:%q access_hash:%d}, want the stored name, username and viewer hash",
			user.FirstName, user.Username, user.AccessHash)
	}
	if user.Status == nil || reflect.TypeOf(user.Status) != reflect.TypeOf(api.UserToTL(stored, caller.ID, false).Status) {
		t.Errorf("search-only user status = %T, want the same rendering users.getUsers gives", user.Status)
	}

	// A contact edge never unlocks the target's phone.
	if _, err := s.AddContact(ctx, caller.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	contact := requireFullUserTarget(t, getFullUserForTest(t, s, caller.ID, api.InputUser(caller.ID, target.ID)), target.ID)
	if contact.Phone != "" || !contact.Contact {
		t.Errorf("contact view of target = {contact:%t phone:%q}, want contact with no phone", contact.Contact, contact.Phone)
	}
}

func TestGetFullUserSettingsMatchPeerSettingsForDialogPeer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	caller := chatUser(t, s, 905)
	peer := chatUser(t, s, 906)
	if _, _, _, _, err := s.SendMessage(ctx, caller.ID, peer.ID, "dialog seed", 14280001, 0, 0); err != nil {
		t.Fatal(err)
	}

	compare := func(what string) {
		t.Helper()
		settings := getPeerSettingsForTest(t, s, caller.ID, api.InputPeerUser(caller.ID, peer.ID))
		full := getFullUserForTest(t, s, caller.ID, api.InputUser(caller.ID, peer.ID))
		if !reflect.DeepEqual(full.FullUser.Settings, settings.Settings) {
			t.Errorf("%s: full user settings = %+v, want a deep match with getPeerSettings %+v", what, full.FullUser.Settings, settings.Settings)
		}
		if !reflect.DeepEqual(full.Users[0], settings.Users[0]) {
			t.Errorf("%s: full user record = %+v, want the same record getPeerSettings returns %+v", what, full.Users[0], settings.Users[0])
		}
	}

	compare("non-contact dialog")
	if _, err := s.AddContact(ctx, caller.ID, peer.ID); err != nil {
		t.Fatal(err)
	}
	compare("caller-owned contact")
	if _, err := s.BlockUser(ctx, caller.ID, peer.ID); err != nil {
		t.Fatal(err)
	}
	compare("caller blocked the peer")

	// Self keeps the same empty bar both RPCs give today.
	selfSettings := getPeerSettingsForTest(t, s, caller.ID, &tg.InputPeerSelf{})
	selfFull := getFullUserForTest(t, s, caller.ID, &tg.InputUserSelf{})
	if !reflect.DeepEqual(selfFull.FullUser.Settings, selfSettings.Settings) {
		t.Errorf("self settings = %+v, want a deep match with getPeerSettings %+v", selfFull.FullUser.Settings, selfSettings.Settings)
	}
}

func TestGetFullUserBlockedFollowsCallerEdgeOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	alice := chatUser(t, s, 907)
	bob := chatUser(t, s, 908)

	// No dialog: the block edge is the caller's own state and stays readable.
	if _, err := s.BlockUser(ctx, alice.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if blocked := getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID)).FullUser.Blocked; !blocked {
		t.Errorf("no-dialog blocked peer = %t, want true", blocked)
	}
	if _, err := s.UnblockUser(ctx, alice.ID, bob.ID); err != nil {
		t.Fatal(err)
	}
	if blocked := getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID)).FullUser.Blocked; blocked {
		t.Errorf("no-dialog unblocked peer = %t, want false", blocked)
	}

	// With a dialog, block and unblock move the flag and nothing else.
	if _, _, _, _, err := s.SendMessage(ctx, alice.ID, bob.ID, "block dialog seed", 14280002, 0, 0); err != nil {
		t.Fatal(err)
	}
	baseline := encodeFullUser(t, getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID)))
	if _, err := api.BlockForTest(s, alice.ID, &tg.ContactsBlockRequest{ID: api.InputPeerUser(alice.ID, bob.ID)}); err != nil {
		t.Fatalf("block: %v", err)
	}
	blockedFull := getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID))
	if !blockedFull.FullUser.Blocked {
		t.Errorf("blocked peer = false, want true")
	}
	if blockedFull.FullUser.Settings.BlockContact {
		t.Errorf("blocked peer still advertises block_contact, want it cleared")
	}
	if _, err := api.UnblockForTest(s, alice.ID, &tg.ContactsUnblockRequest{ID: api.InputPeerUser(alice.ID, bob.ID)}); err != nil {
		t.Fatalf("unblock: %v", err)
	}
	if got := encodeFullUser(t, getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID))); !bytes.Equal(got, baseline) {
		t.Errorf("response after unblock differs from the pre-block baseline")
	}

	// The reverse edge must be invisible: Bob blocking Alice changes nothing Alice
	// can observe, including the explicit user record carried with the profile.
	if _, err := s.BlockUser(ctx, bob.ID, alice.ID); err != nil {
		t.Fatal(err)
	}
	reverse := getFullUserForTest(t, s, alice.ID, api.InputUser(alice.ID, bob.ID))
	if reverse.FullUser.Blocked {
		t.Errorf("peer-owned block changed caller's blocked flag, want false")
	}
	if got := encodeFullUser(t, reverse); !bytes.Equal(got, baseline) {
		t.Errorf("peer-owned block changed the caller's response bytes")
	}
}

func TestGetFullUserRejectsInvalidReferencesWithoutExistenceOracle(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	caller := chatUser(t, s, 909)
	otherViewer := chatUser(t, s, 910)
	existing := chatUser(t, s, 911)
	missingID := existing.ID + 1_000_000

	cases := []struct {
		name  string
		input tg.InputUserClass
	}{
		{name: "flipped hash", input: &tg.InputUser{UserID: existing.ID, AccessHash: api.DeriveUserHash(caller.ID, existing.ID) ^ 1}},
		{name: "incremented hash", input: &tg.InputUser{UserID: existing.ID, AccessHash: api.DeriveUserHash(caller.ID, existing.ID) + 1}},
		{name: "another viewer hash", input: api.InputUser(otherViewer.ID, existing.ID)},
		{name: "zero hash", input: &tg.InputUser{UserID: existing.ID}},
		{name: "zero id", input: &tg.InputUser{AccessHash: api.DeriveUserHash(caller.ID, existing.ID)}},
		{name: "inputUserEmpty", input: &tg.InputUserEmpty{}},
		{name: "inputUserFromMessage", input: &tg.InputUserFromMessage{Peer: api.InputPeerUser(caller.ID, existing.ID), MsgID: 1, UserID: existing.ID}},
		{name: "valid hash for missing id", input: api.InputUser(caller.ID, missingID)},
	}

	var want []byte
	for i, tc := range cases {
		got := getFullUserErrorBytes(t, s, caller.ID, tc.input)
		if string(got) != "400\x00PEER_ID_INVALID\x00PEER_ID_INVALID\x000" {
			t.Errorf("%s error bytes = %q, want PEER_ID_INVALID", tc.name, got)
		}
		if i == 0 {
			want = got
		} else if !bytes.Equal(got, want) {
			t.Errorf("%s error bytes = %q, want byte-identical to a bad hash %q", tc.name, got, want)
		}
	}
}

func TestGetFullUserRejectsMissingSessionAndMissingSelfRow(t *testing.T) {
	t.Parallel()
	s := openStore(t)
	caller := chatUser(t, s, 912)

	if _, err := api.GetFullUserForTest(s, 0, &tg.UsersGetFullUserRequest{ID: &tg.InputUserSelf{}}); err == nil || rpcMessage(t, err) != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unbound session error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
	if _, err := api.GetFullUserForTest(s, 0, &tg.UsersGetFullUserRequest{ID: api.InputUser(caller.ID, caller.ID)}); err == nil || rpcMessage(t, err) != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("unbound session with a reference error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
	// A session whose account row is gone cannot be served a profile of itself.
	if _, err := api.GetFullUserForTest(s, caller.ID+1_000_000, &tg.UsersGetFullUserRequest{ID: &tg.InputUserSelf{}}); err == nil || rpcMessage(t, err) != "AUTH_KEY_UNREGISTERED" {
		t.Fatalf("missing self row error = %v, want AUTH_KEY_UNREGISTERED", err)
	}
}

func TestGetFullUserReportsNoUnsupportedCapabilities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t)
	caller := chatUser(t, s, 913)
	peer := chatUser(t, s, 914)
	if _, _, _, _, err := s.SendMessage(ctx, caller.ID, peer.ID, "capability seed", 14280003, 0, 0); err != nil {
		t.Fatal(err)
	}

	full := getFullUserForTest(t, s, caller.ID, api.InputUser(caller.ID, peer.ID))
	assertFullUserTruthful(t, full.FullUser, 0)
	if full.Chats == nil || len(full.Chats) != 0 {
		t.Errorf("chats = %#v, want an empty vector", full.Chats)
	}
	if full.Users == nil || len(full.Users) != 1 {
		t.Errorf("users = %#v, want exactly the target", full.Users)
	}
}
