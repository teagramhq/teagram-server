package e2e_test

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/catalog"
	"github.com/teagramhq/teagram-server/internal/catalogpublish"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSmoke(t *testing.T) {
	t.Run("launch-getters", func(t *testing.T) {
		t.Parallel()
		testSmokeLaunchGetters(t)
	})
	t.Run("one-to-one", func(t *testing.T) {
		t.Parallel()
		testSmokeOneToOne(t)
	})
	t.Run("photo-media", func(t *testing.T) {
		t.Parallel()
		testSmokePhotoMedia(t)
	})
	t.Run("shared-media-search", func(t *testing.T) {
		t.Parallel()
		testSmokeSharedMediaSearch(t)
	})
	t.Run("saved-messages", func(t *testing.T) {
		t.Parallel()
		testSmokeSavedMessages(t)
	})
	t.Run("default-dialog-filter", func(t *testing.T) {
		t.Parallel()
		testSmokeDefaultDialogFilter(t)
	})
	t.Run("dialog-pins", func(t *testing.T) {
		t.Parallel()
		testSmokeDialogPins(t)
	})
	t.Run("dialog-filters", func(t *testing.T) {
		t.Parallel()
		testSmokeDialogFilters(t)
	})
	t.Run("cloud-drafts", func(t *testing.T) {
		t.Parallel()
		testSmokeCloudDrafts(t)
	})
	t.Run("manual-dialog-unread-mark", func(t *testing.T) {
		t.Parallel()
		testSmokeManualDialogUnreadMark(t)
	})
	t.Run("basic-group", func(t *testing.T) {
		t.Parallel()
		testSmokeBasicGroup(t)
	})
	t.Run("channel", func(t *testing.T) {
		t.Parallel()
		testSmokeChannel(t)
	})
	t.Run("channel-polls", func(t *testing.T) {
		testSmokeChannelPollLifecycle(t)
	})
	t.Run("megagroup-slow-mode", func(t *testing.T) {
		t.Parallel()
		testSmokeMegagroupSlowMode(t)
	})
	t.Run("contacts-search", func(t *testing.T) {
		t.Parallel()
		testSmokeContactsSearch(t)
	})
	t.Run("full-user-profile", func(t *testing.T) {
		t.Parallel()
		testSmokeFullUserProfile(t)
	})
	t.Run("langpack", func(t *testing.T) {
		t.Parallel()
		testSmokeLangpack(t)
	})
	t.Run("username-registration", func(t *testing.T) {
		t.Parallel()
		testSmokeUsernameRegistration(t)
	})
	t.Run("username-password-reset", func(t *testing.T) {
		t.Parallel()
		testSmokeUsernamePasswordReset(t)
	})
	t.Run("pollprobe", func(t *testing.T) {
		t.Parallel()
		testSmokePollProbe(t)
	})
	t.Run("pollprobe-channels", func(t *testing.T) {
		t.Parallel()
		testSmokePollProbeChannels(t)
	})
	t.Run("secret-chat-exchange", func(t *testing.T) {
		t.Parallel()
		testSmokeSecretChatExchange(t)
	})
	t.Run("provisioned-account-login", func(t *testing.T) {
		testSmokeProvisionedAccountLogin(t)
	})
	t.Run("admin-proxy-login", func(t *testing.T) {
		testSmokeAdminProxyLogin(t)
	})
}

func testSmokeOneToOne(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551046001", "+15551046002"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)

	a1 := newSmokeClient(t, f, "A1", phoneA)
	a2 := newSmokeClient(t, f, "A2", phoneA)
	b1 := newSmokeClient(t, f, "B1", phoneB)
	b2 := newSmokeClient(t, f, "B2", phoneB)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, a1.id, 2, "A1", a1.lifecycle)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, b1.id, 2, "B1", b1.lifecycle)

	// Seed A's local ID space through a real Saved Messages send, so the two
	// accounts' IDs differ when they receive the same 1:1 message.
	var seedResult tg.UpdatesClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		seedResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: "smoke-id-seed", RandomID: 1046001,
		})
		return err
	}); err != nil {
		t.Fatalf("send Saved Messages ID seed: %v", err)
	}
	seed := assertSmokeSendResult(t, seedResult, "smoke-id-seed", 1, 1)
	assertObservedMessage(t, f.ctx, a1.seen, seed.Message, seed.ID, true, a1.id, 1, "A1 ID seed", true)
	assertObservedMessage(t, f.ctx, a2.seen, seed.Message, seed.ID, true, a1.id, 1, "A2 ID seed", true)
	assertObservedMessage(t, f.ctx, a2.push, seed.Message, seed.ID, true, a1.id, 1, "A2 ID seed push")

	var aToBResult tg.UpdatesClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		aToBResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a1.id, b1.id), Message: "a-to-b-smoke", RandomID: 1046002,
		})
		return err
	}); err != nil {
		t.Fatalf("A send: %v", err)
	}
	aToB := assertSmokeSendResult(t, aToBResult, "a-to-b-smoke", 2, 2)
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: aToB.ID}})
		if err != nil {
			return err
		}
		messages, ok := result.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("messages.getMessages = %T, want *tg.MessagesMessages", result)
		}
		if len(messages.Messages) != 1 {
			return fmt.Errorf("messages.getMessages returned %d entries, want one message", len(messages.Messages))
		}
		message, ok := messages.Messages[0].(*tg.Message)
		if !ok || message.Message != "a-to-b-smoke" {
			return fmt.Errorf("messages.getMessages content = %#v, want %q", messages.Messages[0], "a-to-b-smoke")
		}
		return nil
	}); err != nil {
		t.Fatalf("get sent message by ID: %v", err)
	}
	assertObservedMessage(t, f.ctx, a1.seen, aToB.Message, aToB.ID, true, b1.id, 2, "A1 sender echo", true)
	assertObservedMessage(t, f.ctx, a2.seen, aToB.Message, aToB.ID, true, b1.id, 2, "A2 sender echo", true)
	assertObservedMessage(t, f.ctx, b1.seen, "a-to-b-smoke", 1, false, a1.id, 1, "B1 incoming", true)
	assertObservedMessage(t, f.ctx, b2.seen, "a-to-b-smoke", 1, false, a1.id, 1, "B2 incoming", true)
	assertObservedMessage(t, f.ctx, a2.push, aToB.Message, aToB.ID, true, b1.id, 2, "A2 sender echo push")
	assertObservedMessage(t, f.ctx, b1.push, "a-to-b-smoke", 1, false, a1.id, 1, "B1 incoming push")
	assertObservedMessage(t, f.ctx, b2.push, "a-to-b-smoke", 1, false, a1.id, 1, "B2 incoming push")

	var bToAResult tg.UpdatesClass
	if err := b1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		bToAResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(b1.id, a1.id), Message: "b-to-a-smoke", RandomID: 1046003,
		})
		return err
	}); err != nil {
		t.Fatalf("B send: %v", err)
	}
	bToA := assertSmokeSendResult(t, bToAResult, "b-to-a-smoke", 2, 2)
	assertObservedMessage(t, f.ctx, b1.seen, bToA.Message, bToA.ID, true, a1.id, 2, "B1 sender echo", true)
	assertObservedMessage(t, f.ctx, b2.seen, bToA.Message, bToA.ID, true, a1.id, 2, "B2 sender echo", true)
	assertObservedMessage(t, f.ctx, a1.seen, "b-to-a-smoke", 3, false, b1.id, 3, "A1 incoming", true)
	assertObservedMessage(t, f.ctx, a2.seen, "b-to-a-smoke", 3, false, b1.id, 3, "A2 incoming", true)
	assertObservedMessage(t, f.ctx, b2.push, bToA.Message, bToA.ID, true, a1.id, 2, "B2 sender echo push")
	assertObservedMessage(t, f.ctx, a1.push, "b-to-a-smoke", 3, false, b1.id, 3, "A1 incoming push")
	assertObservedMessage(t, f.ctx, a2.push, "b-to-a-smoke", 3, false, b1.id, 3, "A2 incoming push")

	var peerSettings *tg.MessagesPeerSettings
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		peerSettings, err = api.MessagesGetPeerSettings(ctx, peerUser(a1.id, b1.id))
		return err
	}); err != nil {
		t.Fatalf("A getPeerSettings: %v", err)
	}
	assertPeerSettings(t, peerSettings, true, true)
	requirePeerSettingsUser(t, peerSettings, b1.id)

	if err := b1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer: peerUser(b1.id, a1.id), MaxID: 1,
		})
		return err
	}); err != nil {
		t.Fatalf("B read A's received message: %v", err)
	}
	assertObservedReadMarker(t, f.ctx, a1.seen, aToB.ID, 4, "A1 read receipt")
	assertObservedReadMarker(t, f.ctx, a2.seen, aToB.ID, 4, "A2 read receipt")
	assertObservedReadMarker(t, f.ctx, a1.push, aToB.ID, 4, "A1 read receipt push")
	assertObservedReadMarker(t, f.ctx, a2.push, aToB.ID, 4, "A2 read receipt push")

	wantA := map[string]smokeHistoryMessage{
		"a-to-b-smoke": {id: 2, out: true},
		"b-to-a-smoke": {id: 3, out: false},
	}
	wantB := map[string]smokeHistoryMessage{
		"a-to-b-smoke": {id: 1, out: false},
		"b-to-a-smoke": {id: 2, out: true},
	}
	for _, check := range []struct {
		client *smokeClient
		peer   int64
		want   map[string]smokeHistoryMessage
	}{
		{client: a1, peer: b1.id, want: wantA},
		{client: a2, peer: b1.id, want: wantA},
		{client: b1, peer: a1.id, want: wantB},
		{client: b2, peer: a1.id, want: wantB},
	} {
		if err := check.client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeHistory(ctx, api, peerUser(check.client.id, check.peer), check.peer, check.want)
		}); err != nil {
			t.Fatalf("getHistory for %d with peer %d: %v", check.client.id, check.peer, err)
		}
	}

	// Every managed stream and live push stream must contain each message once.
	for _, collector := range []*updateCollector{a1.seen, a2.seen, b1.seen, b2.seen, a1.push, a2.push, b1.push, b2.push} {
		assertNoMessageFor(t, f.ctx, collector.newMsg, "smoke session")
	}

	a1.stopClient(t)
	a2.stopClient(t)
	b1.stopClient(t)
	b2.stopClient(t)
	f.restart(t)

	assertSmokeReconnect(t, f, a1.session, a1.id, a1.id, b1.id, wantA)
	assertSmokeReconnect(t, f, b1.session, b1.id, b1.id, a1.id, wantB)
}

func testSmokeLangpack(t *testing.T) {
	t.Helper()
	artifact := langpackSmokeArtifact(t)
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		if _, err := catalogpublish.Publish(f.ctx, f.dsn, artifact, "smoke-source-revision", catalogpublish.PublishOptions{OSUser: "tester"}); err != nil {
			t.Fatalf("publish language catalog: %v", err)
		}
		if err := f.store.RefreshCatalogSnapshot(f.ctx); err != nil {
			t.Fatalf("refresh language catalog: %v", err)
		}
	})
	storage := &session.StorageMemory{}
	unboundClient := f.savedSessionClientWithSystemLangCode(storage, "en-US")
	if err := unboundClient.Run(f.ctx, func(ctx context.Context) error {
		raw := tg.NewClient(unboundClient)
		assertLangpackSmokeCalls(t, ctx, raw, f.dcID, artifact)
		assertHelpConfigSuggestion(t, ctx, raw)
		if _, err := raw.AccountGetPassword(ctx); err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			t.Errorf("account.getPassword error = %v, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("unbound client run: %v", err)
	}

	data, err := (&session.Loader{Storage: storage}).Load(f.ctx)
	if err != nil {
		t.Fatalf("load anonymous client session: %v", err)
	}
	var authKeyID [8]byte
	if len(data.AuthKeyID) != len(authKeyID) {
		t.Fatalf("auth key id length = %d, want %d", len(data.AuthKeyID), len(authKeyID))
	}
	copy(authKeyID[:], data.AuthKeyID)
	key, ok, err := f.store.AuthKeyByID(f.ctx, mtproto.AuthKeyIDInt64(authKeyID))
	if err != nil || !ok || key.UserID != 0 || key.PendingUserID != 0 || key.Provisional {
		t.Fatalf("unbound startup auth key binding = user:%d pending:%d provisional:%v, ok=%v, err=%v", key.UserID, key.PendingUserID, key.Provisional, ok, err)
	}
	username := fmt.Sprintf("langpack%d", time.Now().UnixNano())
	user, err := f.store.CreateUsernameUser(f.ctx, username, "Langpack", "Smoke")
	if err != nil {
		t.Fatalf("create provisional user: %v", err)
	}
	if err := f.store.ClaimUsername(f.ctx, user.ID, username); err != nil {
		t.Fatalf("claim provisional username: %v", err)
	}
	if err := f.store.BindAuthKeyUser(f.ctx, mtproto.AuthKeyIDInt64(authKeyID), user.ID); err != nil {
		t.Fatalf("bind auth key to provisional user: %v", err)
	}

	provisionalClient := f.savedSessionClientWithSystemLangCode(storage, "en_GB")
	if err := provisionalClient.Run(f.ctx, func(ctx context.Context) error {
		raw := tg.NewClient(provisionalClient)
		assertLangpackSmokeCalls(t, ctx, raw, f.dcID, artifact)
		assertHelpConfigSuggestion(t, ctx, raw)
		if _, err := raw.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}}); err == nil || !tgerr.Is(err, "AUTH_KEY_UNREGISTERED") {
			t.Errorf("provisional messages.getDialogs error = %v, want AUTH_KEY_UNREGISTERED", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("provisional client run: %v", err)
	}
}

func assertHelpConfigSuggestion(t *testing.T, ctx context.Context, raw *tg.Client) {
	t.Helper()
	config, err := raw.HelpGetConfig(ctx)
	if err != nil {
		t.Fatalf("help.getConfig: %v", err)
	}
	if config.SuggestedLangCode != catalog.LanguageEnglish {
		t.Fatalf("help.getConfig suggested_lang_code = %q, want %q", config.SuggestedLangCode, catalog.LanguageEnglish)
	}
}

func assertLangpackSmokeCalls(t *testing.T, ctx context.Context, raw *tg.Client, dcID int, artifact catalog.Artifact) {
	t.Helper()
	const langPack = catalog.PackTDesktop
	const langCode = catalog.LanguageEnglish
	wantCount := len(artifact.Entries)

	languages, err := raw.LangpackGetLanguages(ctx, langPack)
	if err != nil {
		t.Fatalf("langpack.getLanguages: %v", err)
	}
	if len(languages) != 1 {
		t.Fatalf("langpack.getLanguages returned %d languages, want only English", len(languages))
	}
	language := languages[0]
	if language.Name != artifact.Name || language.NativeName != artifact.NativeName || language.LangCode != langCode || language.PluralCode != artifact.PluralCode ||
		language.StringsCount != wantCount || language.TranslatedCount != wantCount || !language.Official || language.Rtl || language.Beta || language.BaseLangCode != "" || language.TranslationsURL != "" {
		t.Fatalf("langpack.getLanguages English metadata = %+v", language)
	}

	full, err := raw.LangpackGetLangPack(ctx, &tg.LangpackGetLangPackRequest{LangPack: langPack, LangCode: langCode})
	if err != nil {
		t.Fatalf("langpack.getLangPack: %v", err)
	}
	assertLangpackSmokeDifference(t, full, 0, 1, artifact)

	selected, err := raw.LangpackGetStrings(ctx, &tg.LangpackGetStringsRequest{
		LangPack: langPack,
		LangCode: langCode,
		Keys:     []string{artifact.Entries[0].Key, artifact.Entries[0].Key},
	})
	if err != nil {
		t.Fatalf("langpack.getStrings: %v", err)
	}
	if len(selected) != 1 {
		t.Fatalf("langpack.getStrings returned %d copies, want one", len(selected))
	}
	selectedString, ok := selected[0].(*tg.LangPackString)
	if !ok || selectedString.Key != artifact.Entries[0].Key || selectedString.Value != artifact.Entries[0].Value {
		t.Fatalf("langpack.getStrings response = %#v", selected[0])
	}

	difference, err := raw.LangpackGetDifference(ctx, &tg.LangpackGetDifferenceRequest{LangPack: langPack, LangCode: langCode, FromVersion: 0})
	if err != nil {
		t.Fatalf("langpack.getDifference: %v", err)
	}
	assertLangpackSmokeDifference(t, difference, 0, 1, artifact)

	nearest, err := raw.HelpGetNearestDC(ctx)
	if err != nil {
		t.Fatalf("help.getNearestDc: %v", err)
	}
	if nearest.ThisDC != dcID || nearest.NearestDC != dcID || nearest.Country != "" {
		t.Fatalf("help.getNearestDc = %+v, want configured DC %d only", nearest, dcID)
	}
}

func assertLangpackSmokeDifference(t *testing.T, difference *tg.LangPackDifference, fromVersion, version int, artifact catalog.Artifact) {
	t.Helper()
	if difference.LangCode != catalog.LanguageEnglish || difference.FromVersion != fromVersion || difference.Version != version || len(difference.Strings) != len(artifact.Entries) {
		t.Fatalf("langpack difference metadata = %+v", difference)
	}
	for i, want := range artifact.Entries {
		got, ok := difference.Strings[i].(*tg.LangPackString)
		if !ok || got.Key != want.Key || got.Value != want.Value {
			t.Fatalf("langpack string %d = %#v, want %q=%q", i, difference.Strings[i], want.Key, want.Value)
		}
	}
}

func langpackSmokeArtifact(t *testing.T) catalog.Artifact {
	t.Helper()
	raw := []byte("\"SMOKE_GOODBYE\" = \"Goodbye\";\n\"SMOKE_HELLO\" = \"Hello from the published English catalog\";\n")
	sum := sha256.Sum256(raw)
	artifact, err := catalog.BuildEnglish(raw, catalog.Source{
		URL:      "https://example.test/telegramdesktop/lang.strings",
		Revision: "smoke-revision",
		SHA256:   hex.EncodeToString(sum[:]),
	})
	if err != nil {
		t.Fatalf("build language catalog smoke artifact: %v", err)
	}
	return artifact
}

func testSmokeSavedMessages(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551046003"
	seedPhoneUsers(t, f.ctx, f.store, phone)
	client := newSmokeClient(t, f, "A1", phone)

	var result tg.UpdatesClass
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		result, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: "saved-smoke", RandomID: 1046004,
		})
		return err
	}); err != nil {
		t.Fatalf("send Saved Messages: %v", err)
	}
	saved := assertSmokeSendResult(t, result, "saved-smoke", 1, 1)
	assertObservedMessage(t, f.ctx, client.seen, saved.Message, saved.ID, true, client.id, 1, "Saved Messages sender echo", true)
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		return verifySmokeHistory(ctx, api, &tg.InputPeerSelf{}, client.id, map[string]smokeHistoryMessage{
			"saved-smoke": {id: saved.ID, out: true},
		})
	}); err != nil {
		t.Fatalf("getHistory for Saved Messages: %v", err)
	}
	assertNoMessageFor(t, f.ctx, client.seen.newMsg, "Saved Messages session")
}

func testSmokeCloudDrafts(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049121", "+15551049122"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a1 := newSmokeClient(t, f, "cloud draft smoke owner", phoneA)
	a2 := newSmokeClient(t, f, "cloud draft smoke second session", phoneA)
	b := dialogPinUser(t, f, phoneB)
	seedDialogPinDM(t, f, a1.id, b.ID, 1049121)

	var saved bool
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		saved, err = api.MessagesSaveDraft(ctx, &tg.MessagesSaveDraftRequest{
			Peer: peerUser(a1.id, b.ID), Message: "smoke cloud draft",
		})
		return err
	}); err != nil {
		t.Fatalf("save cloud draft: %v", err)
	}
	if !saved {
		t.Fatal("messages.saveDraft returned BoolFalse")
	}
	update := recvOrCtx(t, f.ctx, a2.push.drafts, "same-owner cloud draft push")
	assertCloudDraftUpdate(t, update, b.ID, "smoke cloud draft", false, 0)
	listed, err := getDialogs(a2, f.ctx, 0, 20, false)
	if err != nil {
		t.Fatalf("getDialogs with cloud draft: %v", err)
	}
	assertCloudDraftInDialogs(t, listed, b.ID, "smoke cloud draft", false, 0)
}

func testSmokeDefaultDialogFilter(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551049003"
	seedPhoneUsers(t, f.ctx, f.store, phone)
	client := newSmokeClient(t, f, "A1", phone)
	otherSession := newSmokeClient(t, f, "A2", phone)

	var listed *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("first get dialog filters: %v", err)
	}
	if listed.TagsEnabled || len(listed.Filters) != 5 {
		t.Fatalf("first get returned %d folders with tags enabled=%v, want All chats and four defaults", len(listed.Filters), listed.TagsEnabled)
	}
	if _, ok := listed.Filters[0].(*tg.DialogFilterDefault); !ok {
		t.Fatalf("first folder = %T, want All chats", listed.Filters[0])
	}
	wantTitles := []string{"Personal", "Channels", "Groups", "Unread"}
	for i, want := range wantTitles {
		folder, ok := listed.Filters[i+1].(*tg.DialogFilter)
		if !ok || folder.ID != i+2 || folder.Title.Text != want {
			t.Fatalf("default folder %d = %#v, want ID %d %s", i, listed.Filters[i+1], i+2, want)
		}
		switch want {
		case "Personal":
			if !folder.Contacts || !folder.NonContacts || !folder.Bots || folder.Groups || folder.Broadcasts {
				t.Fatalf("Personal flags = %+v, want contacts, non-contacts and bots", folder)
			}
		case "Groups":
			if !folder.Groups || folder.Contacts || folder.NonContacts || folder.Broadcasts || folder.Bots {
				t.Fatalf("Groups flags = %+v, want groups only", folder)
			}
		case "Channels":
			if !folder.Broadcasts || folder.Contacts || folder.NonContacts || folder.Groups || folder.Bots {
				t.Fatalf("Channels flags = %+v, want broadcasts only", folder)
			}
		case "Unread":
			if !folder.Contacts || !folder.NonContacts || !folder.Groups || !folder.Broadcasts || !folder.Bots || !folder.ExcludeRead {
				t.Fatalf("Unread flags = %+v, want all chat types excluding read chats", folder)
			}
		}
	}
	var otherListed *tg.MessagesDialogFilters
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		otherListed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("other authorized session refetch: %v", err)
	}
	if len(otherListed.Filters) != 5 {
		t.Fatalf("other authorized session saw %d folders, want All chats and four defaults", len(otherListed.Filters))
	}
	select {
	case update := <-otherSession.push.dialogFilter:
		t.Fatalf("other session received content-bearing updateDialogFilter: %#v", update)
	default:
	}
}

func testSmokeSharedMediaSearch(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551046091", "+15551046092"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)
	a := newSmokeClient(t, f, "media sender", phoneA)
	b := newSmokeClient(t, f, "media viewer", phoneB)

	peerForA := peerUser(a.id, b.id)
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		for _, text := range []string{
			"shared-media-plain-smoke",
			"shared-media-link-smoke https://example.test/shared-media",
		} {
			if _, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerForA, Message: text, RandomID: int64(len(text)) + 1047090,
			}); err != nil {
				return fmt.Errorf("send %q: %w", text, err)
			}
		}
		const fileID = 1047093
		ok, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: 0, Bytes: []byte("shared media smoke document"),
		})
		if err != nil {
			return fmt.Errorf("upload document: %w", err)
		}
		if !ok {
			return errors.New("upload document returned false")
		}
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: peerForA,
			Media: &tg.InputMediaUploadedDocument{
				File: &tg.InputFile{ID: fileID, Parts: 1, Name: "shared-media-smoke.txt"}, MimeType: "text/plain",
			},
			Message: "shared-media-document-smoke", RandomID: 1047094,
		})
		if err != nil {
			return fmt.Errorf("send document: %w", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed shared-media search messages: %v", err)
	}

	search := func(filter tg.MessagesFilterClass) (*tg.MessagesMessagesSlice, error) {
		var result *tg.MessagesMessagesSlice
		err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			res, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
				Peer: peerUser(b.id, a.id), Q: "", Filter: filter, Limit: 100,
			})
			if err != nil {
				return err
			}
			var ok bool
			result, ok = res.(*tg.MessagesMessagesSlice)
			if !ok {
				return fmt.Errorf("messages.search result = %T, want *tg.MessagesMessagesSlice", res)
			}
			return nil
		})
		return result, err
	}

	documents, err := search(&tg.InputMessagesFilterDocument{})
	if err != nil {
		t.Fatalf("search shared documents: %v", err)
	}
	if documents.Count != 1 || len(documents.Messages) != 1 {
		t.Fatalf("document search count=%d messages=%d, want one", documents.Count, len(documents.Messages))
	}
	documentMessage, ok := documents.Messages[0].(*tg.Message)
	if !ok || documentMessage.Message != "shared-media-document-smoke" {
		t.Fatalf("document search message = %T %+v, want shared-media-document-smoke", documents.Messages[0], documents.Messages[0])
	}
	media, ok := documentMessage.Media.(*tg.MessageMediaDocument)
	if !ok {
		t.Fatalf("document search media = %T, want *tg.MessageMediaDocument", documentMessage.Media)
	}
	if doc, ok := media.Document.(*tg.Document); !ok || doc.ID <= 0 || doc.MimeType != "text/plain" {
		t.Fatalf("document search payload = %T %+v, want a text/plain document", media.Document, media.Document)
	}

	links, err := search(&tg.InputMessagesFilterURL{})
	if err != nil {
		t.Fatalf("search shared links: %v", err)
	}
	if links.Count != 1 || len(links.Messages) != 1 {
		t.Fatalf("URL search count=%d messages=%d, want one", links.Count, len(links.Messages))
	}
	linkMessage, ok := links.Messages[0].(*tg.Message)
	if !ok || linkMessage.Message != "shared-media-link-smoke https://example.test/shared-media" {
		t.Fatalf("URL search message = %T %+v, want the shared link", links.Messages[0], links.Messages[0])
	}
	testSmokePrivateSubtypeMediaSearch(t, f, a, b)
	testSmokeChannelSharedMediaSearch(t, f, a, b)
}

func testSmokePrivateSubtypeMediaSearch(t *testing.T, f *smokeFixture, sender, viewer *smokeClient) {
	t.Helper()
	files := []struct {
		name  string
		right string
		text  string
	}{
		{name: "video.mp4", right: "send_videos", text: "private-shared-video-smoke"},
		{name: "animation.gif", right: "send_gifs", text: "private-shared-gif-smoke"},
		{name: "round.mp4", right: "send_roundvideos", text: "private-shared-round-video-smoke"},
		{name: "voice.ogg", right: "send_voices", text: "private-shared-voice-smoke"},
		{name: "music.mp3", right: "send_audios", text: "private-shared-music-smoke"},
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed private shared media: %v", err)
	}
	defer func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close private shared-media seed connection: %v", err)
		}
	}()
	wantIDs := make(map[string]int64, len(files))
	for i, file := range files {
		var fileID int64
		if err := conn.QueryRow(f.ctx, `
			INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored, subtype_rights)
			VALUES ($1, $2, 1, 'application/octet-stream', $3, true, ARRAY[$4]::text[])
			RETURNING id
		`, sender.id, int64(1047300+i), file.name, file.right).Scan(&fileID); err != nil {
			t.Fatalf("seed %s private file: %v", file.right, err)
		}
		_, _, _, duplicate, err := f.store.SendMessage(f.ctx, sender.id, viewer.id, file.text, int64(1047310+i), fileID, 0)
		if err != nil || duplicate {
			t.Fatalf("seed %s private message: duplicate=%v err=%v", file.right, duplicate, err)
		}
		wantIDs[file.text] = fileID
	}
	cases := []struct {
		name   string
		filter tg.MessagesFilterClass
		want   []string
	}{
		{name: "video", filter: &tg.InputMessagesFilterVideo{}, want: []string{"private-shared-video-smoke"}},
		{name: "gif", filter: &tg.InputMessagesFilterGif{}, want: []string{"private-shared-gif-smoke"}},
		{name: "poll", filter: &tg.InputMessagesFilterPoll{}},
		{name: "round voice", filter: &tg.InputMessagesFilterRoundVoice{}, want: []string{"private-shared-voice-smoke", "private-shared-round-video-smoke"}},
		{name: "music", filter: &tg.InputMessagesFilterMusic{}, want: []string{"private-shared-music-smoke"}},
	}
	for _, tc := range cases {
		for _, limit := range []int{100, 0} {
			var result *tg.MessagesMessagesSlice
			if err := viewer.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
				response, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
					Peer: peerUser(viewer.id, sender.id), Q: "", Filter: tc.filter, Limit: limit,
				})
				if err != nil {
					return err
				}
				var ok bool
				result, ok = response.(*tg.MessagesMessagesSlice)
				if !ok {
					return fmt.Errorf("%s private search response = %T, want *tg.MessagesMessagesSlice", tc.name, response)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s private search limit %d: %v", tc.name, limit, err)
			}
			wantMessages := tc.want
			if limit == 0 {
				wantMessages = nil
			}
			if result.Count != len(tc.want) || len(result.Messages) != len(wantMessages) {
				t.Fatalf("%s private search limit %d count/messages = %d/%d, want %d/%d", tc.name, limit, result.Count, len(result.Messages), len(tc.want), len(wantMessages))
			}
			for i, class := range result.Messages {
				message, ok := class.(*tg.Message)
				if !ok || message.Message != wantMessages[i] || wantIDs[wantMessages[i]] <= 0 {
					t.Fatalf("%s private search message %d = %T %+v, want %q", tc.name, i, class, class, wantMessages[i])
				}
				media, ok := message.Media.(*tg.MessageMediaDocument)
				if !ok {
					t.Fatalf("%s private search media = %T, want generic document", tc.name, message.Media)
				}
				document, ok := media.Document.(*tg.Document)
				if !ok || document.ID != wantIDs[wantMessages[i]] {
					t.Fatalf("%s private search document = %T %+v, want file %d", tc.name, media.Document, media.Document, wantIDs[wantMessages[i]])
				}
			}
		}
	}
}

func testSmokeChannelSharedMediaSearch(t *testing.T, f *smokeFixture, sender, viewer *smokeClient) {
	t.Helper()
	channelID := createBroadcastChannel(t, f.ctx, sender.cmds, "Shared media smoke")
	hash := exportChannelInvite(t, f.ctx, sender.id, sender.cmds, channelID)
	if joinedID := importChannelInvite(t, f.ctx, viewer.cmds, hash); joinedID != channelID {
		t.Fatalf("shared-media viewer joined channel %d, want %d", joinedID, channelID)
	}

	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed channel media: %v", err)
	}
	defer func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close channel media seed connection: %v", err)
		}
	}()
	files := []struct {
		name  string
		right string
		text  string
	}{
		{name: "video.mp4", right: "send_videos", text: "channel-shared-video-smoke"},
		{name: "animation.gif", right: "send_gifs", text: "channel-shared-gif-smoke"},
		{name: "round.mp4", right: "send_roundvideos", text: "channel-shared-round-video-smoke"},
		{name: "voice.ogg", right: "send_voices", text: "channel-shared-voice-smoke"},
		{name: "music.mp3", right: "send_audios", text: "channel-shared-music-smoke"},
	}
	wantIDs := make(map[string]int64, len(files))
	for i, file := range files {
		var fileID int64
		if err := conn.QueryRow(f.ctx, `
			INSERT INTO files (uploader_id, access_hash, size, mime_type, file_name, stored, subtype_rights)
			VALUES ($1, $2, 1, 'application/octet-stream', $3, true, ARRAY[$4]::text[])
			RETURNING id
		`, sender.id, int64(1047200+i), file.name, file.right).Scan(&fileID); err != nil {
			t.Fatalf("seed %s file: %v", file.right, err)
		}
		post, _, duplicate, err := f.store.PostChannelMessage(f.ctx, channelID, sender.id, file.text, int64(1047210+i), &fileID, 0)
		if err != nil || duplicate {
			t.Fatalf("seed %s channel post: duplicate=%v err=%v", file.right, duplicate, err)
		}
		wantIDs[file.text] = post.LocalID
	}

	cases := []struct {
		name   string
		filter tg.MessagesFilterClass
		want   []string
	}{
		{name: "video", filter: &tg.InputMessagesFilterVideo{}, want: []string{"channel-shared-video-smoke"}},
		{name: "gif", filter: &tg.InputMessagesFilterGif{}, want: []string{"channel-shared-gif-smoke"}},
		{name: "poll", filter: &tg.InputMessagesFilterPoll{}},
		{name: "round voice", filter: &tg.InputMessagesFilterRoundVoice{}, want: []string{"channel-shared-voice-smoke", "channel-shared-round-video-smoke"}},
		{name: "music", filter: &tg.InputMessagesFilterMusic{}, want: []string{"channel-shared-music-smoke"}},
	}
	for _, tc := range cases {
		for _, limit := range []int{100, 0} {
			var result *tg.MessagesChannelMessages
			if err := viewer.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
				response, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
					Peer: peerChannel(viewer.id, channelID), Q: "", Filter: tc.filter, Limit: limit,
				})
				if err != nil {
					return err
				}
				var ok bool
				result, ok = response.(*tg.MessagesChannelMessages)
				if !ok {
					return fmt.Errorf("%s channel search response = %T, want *tg.MessagesChannelMessages", tc.name, response)
				}
				return nil
			}); err != nil {
				t.Fatalf("%s channel search limit %d: %v", tc.name, limit, err)
			}
			wantMessages := tc.want
			if limit == 0 {
				wantMessages = nil
			}
			if result.Count != len(tc.want) || len(result.Messages) != len(wantMessages) {
				t.Fatalf("%s channel search limit %d count/messages = %d/%d, want %d/%d", tc.name, limit, result.Count, len(result.Messages), len(tc.want), len(wantMessages))
			}
			if len(result.Chats) != 1 || result.Chats[0].GetID() != channelID || result.Pts <= 0 {
				t.Fatalf("%s channel search limit %d chats/pts = %d/%d, want channel and positive pts", tc.name, limit, len(result.Chats), result.Pts)
			}
			for i, class := range result.Messages {
				message, ok := class.(*tg.Message)
				if !ok || message.Message != wantMessages[i] || int64(message.ID) != wantIDs[wantMessages[i]] {
					t.Fatalf("%s channel search message %d = %T %+v, want %q at id %d", tc.name, i, class, class, wantMessages[i], wantIDs[wantMessages[i]])
				}
				media, ok := message.Media.(*tg.MessageMediaDocument)
				if !ok {
					t.Fatalf("%s channel search media = %T, want generic document rendering", tc.name, message.Media)
				}
				if _, ok := media.Document.(*tg.Document); !ok {
					t.Fatalf("%s channel search document = %T, want *tg.Document", tc.name, media.Document)
				}
			}
		}
	}
}

func testSmokeDialogFilters(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone, otherPhone = "+15551049001", "+15551049002"
	seedPhoneUsers(t, f.ctx, f.store, phone, otherPhone)
	client := newSmokeClient(t, f, "A1", phone)
	otherSession := newSmokeClient(t, f, "A2", phone)
	otherOwner := newSmokeClient(t, f, "B1", otherPhone)
	var appConfig *tg.HelpAppConfig
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.HelpGetAppConfig(ctx, 0)
		if err != nil {
			return err
		}
		var ok bool
		appConfig, ok = result.(*tg.HelpAppConfig)
		if !ok {
			return errors.New("help.getAppConfig returned no config")
		}
		return nil
	}); err != nil {
		t.Fatalf("help.getAppConfig: %v", err)
	}
	config, ok := appConfig.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("app config = %T, want *tg.JSONObject", appConfig.Config)
	}
	filtersEnabled := false
	filtersEnabledFound := false
	for _, value := range config.Value {
		if value.Key != "dialog_filters_enabled" {
			continue
		}
		if filtersEnabledFound {
			t.Fatal("app config repeats dialog_filters_enabled")
		}
		enabled, ok := value.Value.(*tg.JSONBool)
		if !ok {
			t.Fatalf("dialog_filters_enabled = %T, want *tg.JSONBool", value.Value)
		}
		filtersEnabled = enabled.Value
		filtersEnabledFound = true
	}
	if !filtersEnabledFound || !filtersEnabled {
		t.Fatal("dialog_filters_enabled is missing or false")
	}
	var seeded *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		seeded, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("initialize default folders: %v", err)
	}
	assertDefaultFolderOrder := func(label string, result *tg.MessagesDialogFilters) {
		if len(result.Filters) != 5 {
			t.Fatalf("%s folders = %d, want All chats and four defaults", label, len(result.Filters))
		}
		if _, ok := result.Filters[0].(*tg.DialogFilterDefault); !ok {
			t.Fatalf("%s first folder = %T, want All chats", label, result.Filters[0])
		}
		want := []struct {
			id    int
			title string
		}{{2, "Personal"}, {3, "Channels"}, {4, "Groups"}, {5, "Unread"}}
		for i, expected := range want {
			folder, ok := result.Filters[i+1].(*tg.DialogFilter)
			if !ok || folder.ID != expected.id || folder.Title.Text != expected.title {
				t.Fatalf("%s folder %d = %#v, want ID %d %s", label, i+1, result.Filters[i+1], expected.id, expected.title)
			}
		}
	}
	assertDefaultFolderOrder("initial", seeded)
	var repeated *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		repeated, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("repeat default folder read: %v", err)
	}
	assertDefaultFolderOrder("repeated", repeated)

	filter := &tg.DialogFilter{ID: 6, Title: tg.TextWithEntities{Text: "Groups"}, Groups: true}
	filter.SetFlags()
	upsert := &tg.MessagesUpdateDialogFilterRequest{ID: 6, Filter: filter}
	upsert.SetFlags()
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesUpdateDialogFilter(ctx, upsert)
		if err == nil && !ok {
			return errors.New("folder create returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("create dialog filter: %v", err)
	}
	var listed *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("list dialog filters: %v", err)
	}
	if listed.TagsEnabled || len(listed.Filters) != 6 {
		t.Fatalf("filters = %d, tags enabled = %v, want All chats, four defaults and ID 6", len(listed.Filters), listed.TagsEnabled)
	}
	custom, ok := listed.Filters[5].(*tg.DialogFilter)
	if !ok || custom.ID != 6 || custom.Title.Text != "Groups" || !custom.Groups {
		t.Fatalf("listed custom filter = %#v, want ID 6 Groups", listed.Filters[5])
	}

	edit := &tg.DialogFilter{ID: 6, Title: tg.TextWithEntities{Text: "Bots"}, Bots: true}
	edit.SetFlags()
	editRequest := &tg.MessagesUpdateDialogFilterRequest{ID: 6, Filter: edit}
	editRequest.SetFlags()
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesUpdateDialogFilter(ctx, editRequest)
		if err == nil && !ok {
			return errors.New("folder edit returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("edit dialog filter: %v", err)
	}
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("list edited dialog filters: %v", err)
	}
	custom, ok = listed.Filters[5].(*tg.DialogFilter)
	if !ok || custom.ID != 6 || custom.Title.Text != "Bots" || !custom.Bots || custom.Groups {
		t.Fatalf("edited custom filter = %#v, want ID 6 Bots", listed.Filters[5])
	}
	var isolated *tg.MessagesDialogFilters
	if err := otherOwner.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		isolated, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("other owner get dialog filters: %v", err)
	}
	if isolated.TagsEnabled || len(isolated.Filters) != 5 {
		t.Fatalf("other owner saw %d folders with tags enabled=%v, want All chats and four defaults", len(isolated.Filters), isolated.TagsEnabled)
	}
	if _, ok := isolated.Filters[0].(*tg.DialogFilterDefault); !ok {
		t.Fatalf("other owner's first folder = %T, want All chats", isolated.Filters[0])
	}
	if personal, ok := isolated.Filters[1].(*tg.DialogFilter); !ok || personal.Title.Text != "Personal" {
		t.Fatalf("other owner's second folder = %#v, want Personal", isolated.Filters[1])
	}

	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesUpdateDialogFiltersOrder(ctx, []int{6, 0})
		if err == nil && !ok {
			return errors.New("folder reorder returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("reorder dialog filters: %v", err)
	}
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("list reordered dialog filters: %v", err)
	}
	if folder, ok := listed.Filters[0].(*tg.DialogFilter); !ok || folder.ID != 6 {
		t.Fatalf("first reordered filter = %#v, want ID 6", listed.Filters[0])
	}
	f.restart(t)
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("other authorized session get edited dialog filters after restart: %v", err)
	}
	if len(listed.Filters) != 6 {
		t.Fatalf("other session saw %d filters after restart, want six", len(listed.Filters))
	}
	persisted, ok := listed.Filters[0].(*tg.DialogFilter)
	if !ok || persisted.ID != 6 || persisted.Title.Text != "Bots" || !persisted.Bots || persisted.Groups {
		t.Fatalf("persisted edited filter = %#v, want ID 6 Bots", listed.Filters[0])
	}
	if err := otherOwner.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		isolated, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("other owner get dialog filters after restart: %v", err)
	}
	if len(isolated.Filters) != 5 {
		t.Fatalf("other owner saw %d folders after restart, want All chats and four defaults", len(isolated.Filters))
	}
	if personal, ok := isolated.Filters[1].(*tg.DialogFilter); !ok || personal.Title.Text != "Personal" {
		t.Fatalf("other owner's persisted second folder = %#v, want Personal", isolated.Filters[1])
	}
	deleteRequest := &tg.MessagesUpdateDialogFilterRequest{ID: 6}
	deleteRequest.SetFlags()
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesUpdateDialogFilter(ctx, deleteRequest)
		if err == nil && !ok {
			return errors.New("folder delete returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("delete dialog filter: %v", err)
	}
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("list dialog filters after delete: %v", err)
	}
	if len(listed.Filters) != 5 {
		t.Fatalf("filters after delete = %d, want All chats and four defaults", len(listed.Filters))
	}
	var suggested []tg.DialogFilterSuggested
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		suggested, err = api.MessagesGetSuggestedDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("get suggested dialog filters: %v", err)
	}
	if len(suggested) != 0 {
		t.Fatalf("suggested filters = %d, want none because all four default titles remain", len(suggested))
	}
}

func testSmokeBasicGroup(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551047001", "+15551047002", "+15551047003"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB, phoneC)
	a, b, c := newSmokeClient(t, f, "A1", phoneA), newSmokeClient(t, f, "B1", phoneB), newSmokeClient(t, f, "C", phoneC)

	// Offset C's message IDs so the sender's read receipt must use C's local ID,
	// not the reader's ID for the same group message.
	var seedResult tg.UpdatesClass
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		seedResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: "group-id-seed", RandomID: 1047000,
		})
		return err
	}); err != nil {
		t.Fatalf("send group ID seed: %v", err)
	}
	seed := assertSmokeSendResult(t, seedResult, "group-id-seed", 1, 1)
	assertObservedMessage(t, f.ctx, c.seen, seed.Message, seed.ID, true, c.id, 1, "C group ID seed", true)

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		created, err := api.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Smoke group",
			Users: []tg.InputUserClass{inputUser(a.id, b.id), inputUser(a.id, c.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok {
			return fmt.Errorf("createChat updates = %T, want *tg.Updates", created.Updates)
		}
		if len(updates.Chats) != 1 {
			return fmt.Errorf("createChat chats = %d, want one chat", len(updates.Chats))
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("createChat chat = %T, want *tg.Chat", updates.Chats[0])
		}
		if chat.ParticipantsCount != 3 {
			return fmt.Errorf("createChat participants = %d, want 3", chat.ParticipantsCount)
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create smoke group: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesSetTyping(ctx, &tg.MessagesSetTypingRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Action: &tg.SendMessageTypingAction{},
		})
		if err == nil && !ok {
			return errors.New("setTyping returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("set smoke group typing: %v", err)
	}
	for _, recipient := range []*smokeClient{b, c} {
		update := recvOrCtx(t, f.ctx, recipient.push.chatTyping, "smoke group typing")
		from, ok := update.FromID.(*tg.PeerUser)
		if update.ChatID != chatID || !ok || from.UserID != a.id {
			t.Fatalf("smoke group typing update = %+v, want chat %d from A %d", update, chatID, a.id)
		}
		if _, ok := update.Action.(*tg.SendMessageTypingAction); !ok {
			t.Fatalf("smoke group typing action = %T, want *tg.SendMessageTypingAction", update.Action)
		}
	}
	timer := time.NewTimer(75 * time.Millisecond)
	select {
	case update := <-a.push.chatTyping:
		t.Fatalf("smoke typing sender received an update: %+v", update)
	case <-timer.C:
	}
	timer.Stop()

	for _, change := range []struct {
		isAdmin bool
		version int
	}{{true, 2}, {false, 3}} {
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.MessagesEditChatAdmin(ctx, &tg.MessagesEditChatAdminRequest{
				ChatID:  chatID,
				UserID:  inputUser(a.id, b.id),
				IsAdmin: change.isAdmin,
			})
			if err != nil {
				return err
			}
			if !result {
				return errors.New("editChatAdmin result is false")
			}
			return nil
		}); err != nil {
			t.Fatalf("set member admin to %t: %v", change.isAdmin, err)
		}
		for _, member := range []*smokeClient{a, b, c} {
			update := recvOrCtx(t, f.ctx, member.push.chatAdmin, "group admin update")
			if update.ChatID != chatID || update.UserID != b.id || update.IsAdmin != change.isAdmin || update.Version != change.version {
				t.Fatalf("admin update for member %d = %+v, want chat %d user %d admin %t version %d", member.id, update, chatID, b.id, change.isAdmin, change.version)
			}
			if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
				full, err := api.MessagesGetFullChat(ctx, chatID)
				if err != nil {
					return fmt.Errorf("getFullChat: %w", err)
				}
				chat, ok := full.FullChat.(*tg.ChatFull)
				if !ok {
					return fmt.Errorf("full chat = %T, want *tg.ChatFull", full.FullChat)
				}
				participants, ok := chat.Participants.(*tg.ChatParticipants)
				if !ok {
					return fmt.Errorf("chat participants = %T, want *tg.ChatParticipants", chat.Participants)
				}
				for _, participant := range participants.Participants {
					if participant.GetUserID() != b.id {
						continue
					}
					_, gotAdmin := participant.(*tg.ChatParticipantAdmin)
					if gotAdmin != change.isAdmin {
						return fmt.Errorf("member %d admin = %t, want %t", b.id, gotAdmin, change.isAdmin)
					}
					return nil
				}
				return fmt.Errorf("getFullChat omitted member %d", b.id)
			}); err != nil {
				t.Fatalf("getFullChat for member %d after admin change: %v", member.id, err)
			}
		}
	}

	wantSenders := map[string]int64{
		"group-a": a.id,
		"group-b": b.id,
		"group-c": c.id,
	}
	memberLocalIDs := map[int64]map[string]int{
		a.id: {},
		b.id: {},
		c.id: {},
	}
	senderLocalIDs := make(map[string]int, len(wantSenders))
	for _, send := range []struct {
		client   *smokeClient
		text     string
		randomID int64
	}{{a, "group-a", 1047001}, {b, "group-b", 1047002}, {c, "group-c", 1047003}} {
		var result tg.UpdatesClass
		if err := send.client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Message: send.text, RandomID: send.randomID,
			})
			return err
		}); err != nil {
			t.Fatalf("%s send to group: %v", send.text, err)
		}
		message, pts, ok := outgoingMessage(t, result, send.text)
		if !ok || countOutgoingMessages(result, send.text) != 1 {
			t.Fatalf("%s send result omitted exactly one outgoing message", send.text)
		}
		if message.ID <= 0 || pts <= 0 || !message.Out {
			t.Fatalf("%s outgoing id/pts/out = %d/%d/%t, want positive id and pts and outgoing", send.text, message.ID, pts, message.Out)
		}
		senderLocalIDs[send.text] = message.ID
		peer, ok := message.PeerID.(*tg.PeerChat)
		if !ok || peer.ChatID != chatID {
			t.Fatalf("%s outgoing peer = %+v, want chat %d", send.text, message.PeerID, chatID)
		}
		from, ok := message.FromID.(*tg.PeerUser)
		if !ok || from.UserID != send.client.id {
			t.Fatalf("%s outgoing sender = %+v, want user %d", send.text, message.FromID, send.client.id)
		}
		for _, member := range []*smokeClient{a, b, c} {
			got := recvOrCtx(t, f.ctx, member.seen.newMsg, send.text+" group update")
			if got.Message != send.text || got.ID <= 0 || got.Out != (member.id == send.client.id) {
				t.Fatalf("%s group update for %d = {text:%q id:%d out:%v}, want positive local id and out:%t", send.text, member.id, got.Message, got.ID, got.Out, member.id == send.client.id)
			}
			updatePeer, ok := got.PeerID.(*tg.PeerChat)
			if !ok || updatePeer.ChatID != chatID {
				t.Fatalf("%s group update for %d peer = %+v, want chat %d", send.text, member.id, got.PeerID, chatID)
			}
			updateFrom, ok := got.FromID.(*tg.PeerUser)
			if !ok || updateFrom.UserID != send.client.id {
				t.Fatalf("%s group update for %d sender = %+v, want user %d", send.text, member.id, got.FromID, send.client.id)
			}
			if member.id == send.client.id && got.ID != message.ID {
				t.Fatalf("%s sender update id = %d, want sender-local id %d", send.text, got.ID, message.ID)
			}
			memberLocalIDs[member.id][send.text] = got.ID
		}
	}

	for _, member := range []*smokeClient{a, b, c} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			if err := verifySmokeGroupHistory(ctx, api, chatID, member.id, wantSenders); err != nil {
				return err
			}
			full, err := api.MessagesGetFullChat(ctx, chatID)
			if err != nil {
				return fmt.Errorf("getFullChat: %w", err)
			}
			return checkFullChat(t, full, chatID, a.id, member.id, b.id, c.id)
		}); err != nil {
			t.Fatalf("smoke group for member %d: %v", member.id, err)
		}
	}

	readerMessageID := memberLocalIDs[a.id]["group-c"]
	senderMessageID := senderLocalIDs["group-c"]
	if readerMessageID <= memberLocalIDs[a.id]["group-b"] {
		t.Fatalf("A's last received group id = %d, want greater than prior id %d", readerMessageID, memberLocalIDs[a.id]["group-b"])
	}
	if readerMessageID == senderMessageID {
		t.Fatalf("group-c reader and sender local IDs both equal %d; fixture must distinguish owner ID spaces", readerMessageID)
	}

	var beforeRead *tg.Dialog
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		beforeRead, err = smokeGroupDialog(ctx, api, chatID)
		return err
	}); err != nil {
		t.Fatalf("A getDialogs before reading group: %v", err)
	}
	if beforeRead.UnreadCount != 2 {
		t.Fatalf("A group unread count before read = %d, want 2", beforeRead.UnreadCount)
	}

	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MaxID: readerMessageID,
		})
		if err != nil {
			return err
		}
		if result == nil {
			return errors.New("readHistory response is nil")
		}
		return nil
	}); err != nil {
		t.Fatalf("A read group history through local id %d: %v", readerMessageID, err)
	}

	assertSenderReceipt := func(collector *updateCollector, label string) {
		t.Helper()
		if got := recvOrCtx(t, f.ctx, collector.readOutbox, label+" updateReadHistoryOutbox max_id"); got != senderMessageID {
			t.Fatalf("%s updateReadHistoryOutbox max_id = %d, want C-local id %d", label, got, senderMessageID)
		}
		if got := recvOrCtx(t, f.ctx, collector.readOutboxPts, label+" updateReadHistoryOutbox pts"); got <= 0 {
			t.Fatalf("%s updateReadHistoryOutbox pts = %d, want positive pts", label, got)
		}
	}
	assertSenderReceipt(c.seen, "C managed updates")
	assertSenderReceipt(c.push, "C live push")

	pollInput := &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: "Which smoke option?"},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "First"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Second"}},
		},
	}}
	const pollDescription = "Description stuff"
	var pollSend tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		pollSend, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Media: pollInput,
			Message: pollDescription, Entities: []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 11}}, RandomID: 1047005,
		})
		return err
	}); err != nil {
		t.Fatalf("send group poll: %v", err)
	}
	pollUpdates, ok := pollSend.(*tg.Updates)
	if !ok {
		t.Fatalf("group poll response = %T, want *tg.Updates", pollSend)
	}
	var pollMessage *tg.Message
	for _, update := range pollUpdates.Updates {
		if created, ok := update.(*tg.UpdateNewMessage); ok {
			if message, ok := created.Message.(*tg.Message); ok {
				pollMessage = message
				break
			}
		}
	}
	if pollMessage == nil {
		t.Fatalf("group poll response omitted the outgoing message: %+v", pollUpdates.Updates)
	}
	media, ok := pollMessage.Media.(*tg.MessageMediaPoll)
	if !ok || media.Poll.Question.Text != "Which smoke option?" || media.Poll.ID <= 0 {
		t.Fatalf("group poll media = %#v, want canonical poll", pollMessage.Media)
	}
	assertSmokePollCaption(t, "group poll send response", pollMessage, pollDescription)
	firstAnswer, ok := media.Poll.Answers[0].(*tg.PollAnswer)
	if !ok || len(firstAnswer.Option) == 0 {
		t.Fatalf("group poll first answer = %#v, want server-assigned option bytes", media.Poll.Answers[0])
	}
	bPoll := recvOrCtx(t, f.ctx, b.seen.newMsg, "B group poll update")
	cPoll := recvOrCtx(t, f.ctx, c.seen.newMsg, "C group poll update")
	for _, received := range []*tg.Message{bPoll, cPoll} {
		receivedPoll, ok := received.Media.(*tg.MessageMediaPoll)
		if !ok || receivedPoll.Poll.ID != media.Poll.ID || receivedPoll.Poll.Creator {
			t.Fatalf("member %d poll copy = %#v, want shared non-creator poll %d", received.FromID, received.Media, media.Poll.ID)
		}
		assertSmokePollCaption(t, fmt.Sprintf("member %d live poll", received.FromID), received, pollDescription)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20})
		if err != nil {
			return fmt.Errorf("get group poll history: %w", err)
		}
		historyMessages, ok := history.(*tg.MessagesMessages)
		if !ok {
			return fmt.Errorf("group poll history = %T, want *tg.MessagesMessages", history)
		}
		found := false
		for _, candidate := range historyMessages.Messages {
			message, ok := candidate.(*tg.Message)
			if ok && message.ID == bPoll.ID {
				assertSmokePollCaption(t, "group poll history", message, pollDescription)
				found = true
			}
		}
		if !found {
			return fmt.Errorf("group poll history omitted message %d", bPoll.ID)
		}
		byID, err := api.MessagesGetMessages(ctx, []tg.InputMessageClass{&tg.InputMessageID{ID: bPoll.ID}})
		if err != nil {
			return fmt.Errorf("get group poll by id: %w", err)
		}
		messages, ok := byID.(*tg.MessagesMessages)
		if !ok || len(messages.Messages) != 1 {
			return fmt.Errorf("group poll getMessages = %T, want one message", byID)
		}
		message, ok := messages.Messages[0].(*tg.Message)
		if !ok {
			return fmt.Errorf("group poll getMessages item = %T, want *tg.Message", messages.Messages[0])
		}
		assertSmokePollCaption(t, "group poll getMessages", message, pollDescription)
		return nil
	}); err != nil {
		t.Fatalf("read described group poll: %v", err)
	}

	var voteResult tg.UpdatesClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		voteResult, err = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, MsgID: bPoll.ID, Options: [][]byte{firstAnswer.Option},
		})
		return err
	}); err != nil {
		t.Fatalf("vote in group poll: %v", err)
	}
	voteUpdates, ok := voteResult.(*tg.Updates)
	if !ok {
		t.Fatalf("group poll vote response = %T, want *tg.Updates", voteResult)
	}
	var voted *tg.UpdateMessagePoll
	for _, update := range voteUpdates.Updates {
		if result, ok := update.(*tg.UpdateMessagePoll); ok {
			voted = result
			break
		}
	}
	if voted == nil || len(voted.Results.Results) != 2 || voted.Results.Results[0].Voters != 1 {
		t.Fatalf("group poll vote results = %+v, want first option with one vote", voted)
	}
	for _, member := range []*smokeClient{a, b, c} {
		live := recvOrCtx(t, f.ctx, member.push.editMsg, fmt.Sprintf("%d live durable poll result", member.id))
		liveMedia, ok := live.Media.(*tg.MessageMediaPoll)
		if !ok || liveMedia.Poll.ID != media.Poll.ID || len(liveMedia.Results.Results) != 2 || liveMedia.Results.Results[0].Voters != 1 {
			t.Fatalf("live poll result for %d = %+v, want poll %d with one first-option vote", member.id, live, media.Poll.ID)
		}
	}

	closePoll := media.Poll
	closePoll.SetClosed(true)
	closeMedia := &tg.InputMediaPoll{Poll: closePoll}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		req := &tg.MessagesEditMessageRequest{Peer: &tg.InputPeerChat{ChatID: chatID}, ID: pollMessage.ID}
		req.SetMedia(closeMedia)
		_, err := api.MessagesEditMessage(ctx, req)
		return err
	}); err != nil {
		t.Fatalf("close group poll: %v", err)
	}
	closed := recvOrCtx(t, f.ctx, b.push.editMsg, "B durable poll close update")
	closedMedia, ok := closed.Media.(*tg.MessageMediaPoll)
	if !ok || closedMedia.Poll.ID != media.Poll.ID || !closedMedia.Poll.Closed {
		t.Fatalf("B closed poll update = %#v, want closed poll %d", closed.Media, media.Poll.ID)
	}

	for i, wantInboxID := range []int{readerMessageID, readerMessageID} {
		var dialog *tg.Dialog
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			dialog, err = smokeGroupDialog(ctx, api, chatID)
			return err
		}); err != nil {
			t.Fatalf("A getDialogs after group read %d: %v", i+1, err)
		}
		if dialog.UnreadCount != 0 || dialog.ReadInboxMaxID != wantInboxID || dialog.ReadInboxMaxID <= beforeRead.ReadInboxMaxID {
			t.Fatalf("A group dialog after read %d = {unread:%d inbox:%d}, want unread=0 and advanced inbox=%d (before %d)", i+1, dialog.UnreadCount, dialog.ReadInboxMaxID, wantInboxID, beforeRead.ReadInboxMaxID)
		}
	}

	// A and B use equal IDs here; C's seeded ID space must retain its own ID in
	// the live notification and full-group display.
	var happyMessageID int
	var happySend tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		happySend, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "group-pin-happy", RandomID: 1047004,
		})
		return err
	}); err != nil {
		t.Fatalf("send happy-path pin message: %v", err)
	}
	happyMessage, _, ok := outgoingMessage(t, happySend, "group-pin-happy")
	if !ok {
		t.Fatal("happy-path pin send omitted its outgoing message")
	}
	happyMessageID = happyMessage.ID
	happyLocalIDs := make(map[int64]int, 3)
	for _, member := range []*smokeClient{a, b, c} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			got, err := smokeGroupMessageID(ctx, api, chatID, "group-pin-happy")
			if err != nil {
				return err
			}
			if got <= 0 {
				return fmt.Errorf("happy-path pin copy id = %d for user %d, want positive ID", got, member.id)
			}
			happyLocalIDs[member.id] = got
			return nil
		}); err != nil {
			t.Fatalf("read happy-path pin copy for member %d: %v", member.id, err)
		}
	}
	if happyLocalIDs[a.id] != happyMessageID || happyLocalIDs[b.id] != happyMessageID {
		t.Fatalf("A/B happy-path pin IDs = %d/%d, want sender-local ID %d", happyLocalIDs[a.id], happyLocalIDs[b.id], happyMessageID)
	}
	var happyPinResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		happyPinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: happyMessageID,
		})
		return err
	}); err != nil {
		t.Fatalf("pin happy-path group message: %v", err)
	}
	assertSmokePinResult(t, happyPinResult, true, chatID, []int{happyMessageID})
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, b.push.pinnedMsg, "B happy-path pin push"), true, chatID, []int{happyLocalIDs[b.id]})
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, c.push.pinnedMsg, "C happy-path pin push"), true, chatID, []int{happyLocalIDs[c.id]})
	for _, member := range []*smokeClient{a, b, c} {
		wantID := happyLocalIDs[member.id]
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			if err := verifySmokeGroupPin(ctx, api, chatID, wantID, "group-pin-happy"); err != nil {
				return err
			}
			result, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Q: "", Filter: &tg.InputMessagesFilterPinned{}, Limit: 100,
			})
			if err != nil {
				return fmt.Errorf("messages.search pinned group: %w", err)
			}
			pinned, ok := result.(*tg.MessagesMessages)
			if !ok || len(pinned.Messages) != 1 {
				return fmt.Errorf("pinned search result = %T, want one group message", result)
			}
			message, ok := pinned.Messages[0].(*tg.Message)
			if !ok || message.ID != wantID || message.Message != "group-pin-happy" {
				return fmt.Errorf("pinned search message = %T %+v, want id %d and text group-pin-happy", pinned.Messages[0], pinned.Messages[0], wantID)
			}
			return nil
		}); err != nil {
			t.Fatalf("reopen happy-path pinned group for member %d: %v", member.id, err)
		}
	}
	var happyUnpinResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		happyUnpinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: happyMessageID, Unpin: true,
		})
		return err
	}); err != nil {
		t.Fatalf("unpin happy-path group message: %v", err)
	}
	assertSmokePinResult(t, happyUnpinResult, false, chatID, nil)
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, b.push.pinnedMsg, "B happy-path unpin push"), false, chatID, nil)
	for _, member := range []*smokeClient{a, b, c} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			if err := verifySmokeGroupUnpinned(ctx, api, chatID); err != nil {
				return err
			}
			result, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Q: "", Filter: &tg.InputMessagesFilterPinned{}, Limit: 100,
			})
			if err != nil {
				return fmt.Errorf("messages.search after group unpin: %w", err)
			}
			pinned, ok := result.(*tg.MessagesMessages)
			if !ok || len(pinned.Messages) != 0 {
				return fmt.Errorf("pinned search after unpin = %T, want empty group messages", result)
			}
			return nil
		}); err != nil {
			t.Fatalf("reopen unpinned group for member %d: %v", member.id, err)
		}
	}

	// Advancing only B's local ID space makes the same next group message have
	// different IDs for A and B, which is the pin namespace regression.
	var pinSeedResult tg.UpdatesClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		pinSeedResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerSelf{}, Message: "group-pin-id-seed", RandomID: 1047005,
		})
		return err
	}); err != nil {
		t.Fatalf("seed B's message ID space: %v", err)
	}
	pinSeed, pinSeedPts, ok := outgoingMessage(t, pinSeedResult, "group-pin-id-seed")
	if !ok {
		t.Fatal("B ID seed omitted its outgoing message")
	}
	if pinSeedPts <= 0 {
		t.Fatalf("B ID seed pts = %d, want positive", pinSeedPts)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		return verifySmokeHistory(ctx, api, &tg.InputPeerSelf{}, b.id, map[string]smokeHistoryMessage{
			"group-pin-id-seed": {id: pinSeed.ID, out: true},
		})
	}); err != nil {
		t.Fatalf("verify B's Saved Messages ID seed: %v", err)
	}

	const mismatchText = "group-pin-mismatch"
	var mismatchSend tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		mismatchSend, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: mismatchText, RandomID: 1047006,
		})
		return err
	}); err != nil {
		t.Fatalf("send mismatched-ID pin message: %v", err)
	}
	mismatchMessage, _, ok := outgoingMessage(t, mismatchSend, mismatchText)
	if !ok {
		t.Fatal("mismatched-ID pin send omitted its outgoing message")
	}
	ownerMessageIDs := make(map[int64]int, 2)
	for _, member := range []*smokeClient{a, b} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			id, err := smokeGroupMessageID(ctx, api, chatID, mismatchText)
			if err != nil {
				return err
			}
			ownerMessageIDs[member.id] = id
			return nil
		}); err != nil {
			t.Fatalf("read mismatched-ID copy for member %d: %v", member.id, err)
		}
	}
	if ownerMessageIDs[a.id] != mismatchMessage.ID {
		t.Fatalf("A group copy id = %d, want sender id %d", ownerMessageIDs[a.id], mismatchMessage.ID)
	}
	if ownerMessageIDs[a.id] == ownerMessageIDs[b.id] {
		t.Fatalf("ID seed did not create distinct owner-local IDs: A/B = %d", ownerMessageIDs[a.id])
	}
	var mismatchPinResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		mismatchPinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: mismatchMessage.ID,
		})
		return err
	}); err != nil {
		t.Fatalf("pin mismatched-ID group message: %v", err)
	}
	assertSmokePinResult(t, mismatchPinResult, true, chatID, []int{ownerMessageIDs[a.id]})
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, b.push.pinnedMsg, "B mismatched-ID pin push"), true, chatID, []int{ownerMessageIDs[b.id]})
	for _, member := range []*smokeClient{a, b} {
		wantID := ownerMessageIDs[member.id]
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeGroupPin(ctx, api, chatID, wantID, mismatchText)
		}); err != nil {
			t.Fatalf("reopen mismatched-ID pinned group for member %d: %v", member.id, err)
		}
	}
	var mismatchUnpinResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		mismatchUnpinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: mismatchMessage.ID, Unpin: true,
		})
		return err
	}); err != nil {
		t.Fatalf("unpin mismatched-ID group message: %v", err)
	}
	assertSmokePinResult(t, mismatchUnpinResult, false, chatID, nil)
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, b.push.pinnedMsg, "B mismatched-ID unpin push"), false, chatID, nil)
	for _, member := range []*smokeClient{a, b} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeGroupUnpinned(ctx, api, chatID)
		}); err != nil {
			t.Fatalf("reopen unpinned mismatched-ID group for member %d: %v", member.id, err)
		}
	}
}

func testSmokeChannel(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneSubscriber = "+15551048001", "+15551048002"
	seedPhoneUsers(t, f.ctx, f.store, phoneCreator, phoneSubscriber)
	creator := newSmokeClient(t, f, "A1", phoneCreator)
	subscriber := newSmokeClient(t, f, "B1", phoneSubscriber)
	execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, client *tg.Client) error {
		cfg, err := client.HelpGetConfig(ctx)
		if err != nil {
			return err
		}
		if cfg.MeURLPrefix != testPublicLinkPrefix {
			return fmt.Errorf("help.getConfig me_url_prefix = %q, want %q", cfg.MeURLPrefix, testPublicLinkPrefix)
		}
		if cfg.DCTxtDomainName != "" {
			return fmt.Errorf("help.getConfig dc_txt_domain_name = %q, want empty", cfg.DCTxtDomainName)
		}
		return nil
	})

	channelID := createBroadcastChannel(t, f.ctx, creator.cmds, "Smoke channel")
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      100,
		})
		if err != nil {
			return err
		}
		var dialogs []tg.DialogClass
		var messages []tg.MessageClass
		switch page := result.(type) {
		case *tg.MessagesDialogs:
			dialogs, messages = page.Dialogs, page.Messages
		case *tg.MessagesDialogsSlice:
			dialogs, messages = page.Dialogs, page.Messages
		default:
			return fmt.Errorf("getDialogs response = %T, want a dialogs result", result)
		}
		for _, item := range dialogs {
			dialog, ok := item.(*tg.Dialog)
			if !ok {
				continue
			}
			peer, ok := dialog.Peer.(*tg.PeerChannel)
			if !ok || peer.ChannelID != channelID {
				continue
			}
			if dialog.TopMessage != 1 {
				return fmt.Errorf("new channel top message = %d, want 1", dialog.TopMessage)
			}
			for _, message := range messages {
				if message.GetID() != dialog.TopMessage {
					continue
				}
				service, ok := message.(*tg.MessageService)
				if !ok {
					return fmt.Errorf("new channel top message = %T, want *tg.MessageService", message)
				}
				action, ok := service.Action.(*tg.MessageActionChannelCreate)
				if !ok || action.Title != "Smoke channel" {
					return fmt.Errorf("new channel create action = %+v, want title %q", service.Action, "Smoke channel")
				}
				return nil
			}
			return fmt.Errorf("getDialogs omitted top message %d for newly created channel", dialog.TopMessage)
		}
		return fmt.Errorf("new channel %d absent from getDialogs before first post", channelID)
	}); err != nil {
		t.Fatalf("new channel getDialogs: %v", err)
	}
	hash := exportChannelInvite(t, f.ctx, creator.id, creator.cmds, channelID)
	if joinedID := importChannelInvite(t, f.ctx, subscriber.cmds, hash); joinedID != channelID {
		t.Fatalf("subscriber joined channel %d, want %d", joinedID, channelID)
	}
	subscriberOtherSession := newSmokeClient(t, f, "B2", phoneSubscriber)
	assertPeerDialog := func(client *smokeClient, wantTopID int, wantText string) {
		t.Helper()
		if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.MessagesGetPeerDialogs(ctx, []tg.InputDialogPeerClass{
				&tg.InputDialogPeer{Peer: peerChannel(client.id, channelID)},
			})
			if err != nil {
				return err
			}
			if len(result.Dialogs) != 1 || len(result.Chats) != 1 {
				return fmt.Errorf("getPeerDialogs counts = dialogs %d chats %d, want 1/1", len(result.Dialogs), len(result.Chats))
			}
			dialog, ok := result.Dialogs[0].(*tg.Dialog)
			if !ok {
				return fmt.Errorf("getPeerDialogs dialog = %T, want *tg.Dialog", result.Dialogs[0])
			}
			peer, ok := dialog.Peer.(*tg.PeerChannel)
			if !ok || peer.ChannelID != channelID {
				return fmt.Errorf("getPeerDialogs peer = %+v, want channel %d", dialog.Peer, channelID)
			}
			channel, ok := result.Chats[0].(*tg.Channel)
			if !ok || channel.ID != channelID {
				return fmt.Errorf("getPeerDialogs channel = %T/%+v, want channel %d", result.Chats[0], result.Chats[0], channelID)
			}
			if dialog.TopMessage != wantTopID {
				return fmt.Errorf("getPeerDialogs top_message = %d, want %d", dialog.TopMessage, wantTopID)
			}
			if wantTopID == 0 {
				if len(result.Messages) != 0 {
					return fmt.Errorf("empty getPeerDialogs messages = %d, want none", len(result.Messages))
				}
				return nil
			}
			if len(result.Messages) != 1 {
				return fmt.Errorf("posted getPeerDialogs messages = %d, want 1", len(result.Messages))
			}
			message, ok := result.Messages[0].(*tg.Message)
			if !ok || message.ID != wantTopID || message.Message != wantText {
				return fmt.Errorf("getPeerDialogs top message = %T/%+v, want id %d text %q", result.Messages[0], result.Messages[0], wantTopID, wantText)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s getPeerDialogs: %v", client.label, err)
		}
	}
	assertPeerDialog(subscriber, 0, "")

	posts := []string{"channel-smoke-one", "channel-smoke-two"}
	postIDs := make(map[string]int, len(posts))
	for i, post := range posts {
		randomID := int64(1048001 + i)
		execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer:     peerChannel(creator.id, channelID),
				Message:  post,
				RandomID: randomID,
			})
			return err
		})
		update := recvOrCtx(t, f.ctx, subscriber.seen.newChannelMsg, "subscriber channel update")
		if update.Msg.Message != post || update.Msg.ID <= 0 {
			t.Fatalf("subscriber channel message = {text:%q id:%d}, want {%q, positive id}", update.Msg.Message, update.Msg.ID, post)
		}
		peer, ok := update.Msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != channelID {
			t.Fatalf("subscriber channel peer = %+v, want channel %d", update.Msg.PeerID, channelID)
		}
		from, ok := update.Msg.FromID.(*tg.PeerUser)
		if !ok || from.UserID != creator.id {
			t.Fatalf("subscriber channel sender = %+v, want creator %d", update.Msg.FromID, creator.id)
		}
		postIDs[post] = update.Msg.ID
	}
	assertPeerDialog(subscriber, postIDs[posts[1]], posts[1])
	noDuplicate := time.NewTimer(50 * time.Millisecond)
	defer noDuplicate.Stop()
	select {
	case duplicate := <-subscriber.seen.newChannelMsg:
		t.Fatalf("subscriber received duplicate channel message: %+v", duplicate.Msg)
	case <-noDuplicate.C:
	case <-f.ctx.Done():
		t.Fatalf("waiting for duplicate channel message check: %s", contextFailureDescription(f.ctx))
	}

	if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peerChannel(subscriber.id, channelID),
			Limit: 10,
		})
		if err != nil {
			return err
		}
		history, ok := result.(*tg.MessagesChannelMessages)
		if !ok {
			return fmt.Errorf("channel history response = %T, want *tg.MessagesChannelMessages", result)
		}
		if len(history.Messages) != len(posts)+1 {
			return fmt.Errorf("channel history count = %d, want %d posts plus creation service message", len(history.Messages), len(posts))
		}
		var sawCreate bool
		for _, class := range history.Messages {
			if service, ok := class.(*tg.MessageService); ok {
				action, actionOK := service.Action.(*tg.MessageActionChannelCreate)
				if !actionOK || service.ID != 1 || action.Title != "Smoke channel" {
					return fmt.Errorf("channel history create service = %+v, want channel creation at id 1", service)
				}
				sawCreate = true
				continue
			}
			message, ok := class.(*tg.Message)
			if !ok {
				return fmt.Errorf("channel history message = %T, want *tg.Message", class)
			}
			wantID, ok := postIDs[message.Message]
			if !ok || message.ID != wantID || message.Out {
				return fmt.Errorf("channel history message = {text:%q id:%d out:%v}, want a known post id and out:false", message.Message, message.ID, message.Out)
			}
			peer, ok := message.PeerID.(*tg.PeerChannel)
			if !ok || peer.ChannelID != channelID {
				return fmt.Errorf("channel history peer = %+v, want channel %d", message.PeerID, channelID)
			}
			from, ok := message.FromID.(*tg.PeerUser)
			if !ok || from.UserID != creator.id {
				return fmt.Errorf("channel history sender = %+v, want creator %d", message.FromID, creator.id)
			}
		}
		if !sawCreate {
			return errors.New("channel history omitted creation service message")
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber getHistory: %v", err)
	}

	checkChannelReadState := func(client *smokeClient, wantMarker, wantUnread int) {
		t.Helper()
		if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			dialog, err := smokeChannelDialog(ctx, api, channelID)
			if err != nil {
				return err
			}
			if dialog.ReadInboxMaxID != wantMarker || dialog.UnreadCount != wantUnread {
				return fmt.Errorf("getDialogs channel read state = marker %d unread %d, want %d/%d", dialog.ReadInboxMaxID, dialog.UnreadCount, wantMarker, wantUnread)
			}
			return nil
		}); err != nil {
			t.Fatalf("%s channel read state: %v", client.label, err)
		}
	}
	checkChannelReadState(subscriber, 1, 2)
	firstPostID := postIDs[posts[0]]
	if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.ChannelsReadMessageContents(ctx, &tg.ChannelsReadMessageContentsRequest{
			Channel: inputChannel(subscriber.id, channelID),
			ID:      []int{firstPostID},
		})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("channels.readMessageContents returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber channels.readMessageContents: %v", err)
	}
	checkChannelReadState(subscriber, 1, 2)
	checkChannelReadState(subscriberOtherSession, 1, 2)

	secondPostID := postIDs[posts[1]]
	if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: inputChannel(subscriber.id, channelID), MaxID: secondPostID,
		})
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("channels.readHistory returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber channels.readHistory: %v", err)
	}
	if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		exported, err := api.ChannelsExportMessageLink(ctx, &tg.ChannelsExportMessageLinkRequest{
			Channel: inputChannel(subscriber.id, channelID),
			ID:      secondPostID,
		})
		if err != nil {
			return err
		}
		want := testPublicLinkPrefix + "c/" + strconv.FormatInt(channelID, 10) + "/" + strconv.Itoa(secondPostID)
		if exported.Link != want {
			return fmt.Errorf("exported channel message link = %q, want %q", exported.Link, want)
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber exportMessageLink: %v", err)
	}
	checkChannelReadState(subscriber, secondPostID, 0)
	if err := subscriberOtherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.ChannelsGetFullChannel(ctx, inputChannel(subscriberOtherSession.id, channelID))
		if err != nil {
			return err
		}
		full, ok := result.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("other-session getFullChannel = %T, want *tg.ChannelFull", result.FullChat)
		}
		if full.ReadInboxMaxID != secondPostID || full.UnreadCount != 0 {
			return fmt.Errorf("other-session full channel read state = marker %d unread %d, want %d/0", full.ReadInboxMaxID, full.UnreadCount, secondPostID)
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber other-session getFullChannel: %v", err)
	}

	const laterPost = "channel-smoke-three"
	execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerChannel(creator.id, channelID), Message: laterPost, RandomID: 1048003,
		})
		return err
	})
	laterUpdate := recvOrCtx(t, f.ctx, subscriber.seen.newChannelMsg, "subscriber later channel update")
	if laterUpdate.Msg.Message != laterPost || laterUpdate.Msg.ID <= secondPostID {
		t.Fatalf("later channel post = {text:%q id:%d}, want %q with id above %d", laterUpdate.Msg.Message, laterUpdate.Msg.ID, laterPost, secondPostID)
	}
	checkChannelReadState(subscriber, secondPostID, 1)
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		fullResult, err := api.ChannelsGetFullChannel(ctx, inputChannel(creator.id, channelID))
		if err != nil {
			return err
		}
		full, ok := fullResult.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("creator getFullChannel = %T, want *tg.ChannelFull", fullResult.FullChat)
		}
		count, ok := full.GetParticipantsCount()
		if full.ID != channelID || !ok || count != 2 || !full.GetCanViewParticipants() {
			return fmt.Errorf("creator getFullChannel id/count/can_view = %d/%d/%v, want %d/2/true", full.ID, count, full.GetCanViewParticipants(), channelID)
		}
		if len(fullResult.Chats) != 1 {
			return fmt.Errorf("creator getFullChannel chats = %d, want 1", len(fullResult.Chats))
		}
		channel, ok := fullResult.Chats[0].(*tg.Channel)
		if !ok || channel.ID != channelID || !channel.Creator || channel.Left {
			return fmt.Errorf("creator getFullChannel channel = %+v, want creator member %d", fullResult.Chats[0], channelID)
		}

		participantsResult, err := api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
			Channel: inputChannel(creator.id, channelID),
			Filter:  &tg.ChannelParticipantsRecent{},
			Limit:   10,
		})
		if err != nil {
			return err
		}
		participants, ok := participantsResult.(*tg.ChannelsChannelParticipants)
		if !ok {
			return fmt.Errorf("getParticipants response = %T, want *tg.ChannelsChannelParticipants", participantsResult)
		}
		if participants.Count != count || len(participants.Participants) != count {
			return fmt.Errorf("getParticipants count/rows = %d/%d, want %d", participants.Count, len(participants.Participants), count)
		}
		roles := make(map[int64]string, len(participants.Participants))
		for _, participant := range participants.Participants {
			switch member := participant.(type) {
			case *tg.ChannelParticipantCreator:
				roles[member.UserID] = "creator"
			case *tg.ChannelParticipant:
				roles[member.UserID] = "member"
			default:
				return fmt.Errorf("getParticipants role = %T, want creator or member", participant)
			}
		}
		if len(roles) != 2 || roles[creator.id] != "creator" || roles[subscriber.id] != "member" {
			return fmt.Errorf("getParticipants roles = %v, want creator %d and subscriber %d", roles, creator.id, subscriber.id)
		}
		return nil
	}); err != nil {
		t.Fatalf("creator channel membership reads: %v", err)
	}

	if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.ChannelsGetFullChannel(ctx, inputChannel(subscriber.id, channelID))
		if err != nil {
			return err
		}
		full, ok := result.FullChat.(*tg.ChannelFull)
		if !ok {
			return fmt.Errorf("subscriber getFullChannel = %T, want *tg.ChannelFull", result.FullChat)
		}
		count, ok := full.GetParticipantsCount()
		if full.ID != channelID || !ok || count != 2 || full.GetCanViewParticipants() {
			return fmt.Errorf("subscriber getFullChannel id/count/can_view = %d/%d/%v, want %d/2/false", full.ID, count, full.GetCanViewParticipants(), channelID)
		}
		if len(result.Chats) != 1 {
			return fmt.Errorf("subscriber getFullChannel chats = %d, want 1", len(result.Chats))
		}
		channel, ok := result.Chats[0].(*tg.Channel)
		if !ok || channel.ID != channelID || channel.Creator || channel.Left {
			return fmt.Errorf("subscriber getFullChannel channel = %+v, want current member %d", result.Chats[0], channelID)
		}
		return nil
	}); err != nil {
		t.Fatalf("subscriber getFullChannel: %v", err)
	}

	var deleted *tg.MessagesAffectedMessages
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		deleted, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: inputChannel(creator.id, channelID), ID: []int{laterUpdate.Msg.ID},
		})
		return err
	}); err != nil {
		t.Fatalf("creator channels.deleteMessages: %v", err)
	}
	if deleted.Pts != laterUpdate.Pts+1 || deleted.PtsCount != 1 {
		t.Fatalf("channels.deleteMessages result = {pts:%d pts_count:%d}, want {%d,1}", deleted.Pts, deleted.PtsCount, laterUpdate.Pts+1)
	}
	deleteUpdate := recvOrCtx(t, f.ctx, subscriber.seen.delChannelMsg, "subscriber channel delete update")
	if deleteUpdate.ChannelID != channelID || deleteUpdate.Pts != deleted.Pts || deleteUpdate.PtsCount != 1 ||
		len(deleteUpdate.Messages) != 1 || deleteUpdate.Messages[0] != laterUpdate.Msg.ID {
		t.Fatalf("subscriber channel delete update = %+v, want channel %d post %d at pts %d/count 1", deleteUpdate, channelID, laterUpdate.Msg.ID, deleted.Pts)
	}
	assertPeerDialog(subscriber, postIDs[posts[1]], posts[1])
	checkChannelReadState(subscriber, secondPostID, 0)
	var replayed *tg.MessagesAffectedMessages
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		replayed, err = api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: inputChannel(creator.id, channelID), ID: []int{laterUpdate.Msg.ID},
		})
		return err
	}); err != nil {
		t.Fatalf("creator channels.deleteMessages replay: %v", err)
	}
	if replayed.Pts != deleted.Pts || replayed.PtsCount != 0 {
		t.Fatalf("channels.deleteMessages replay = {pts:%d pts_count:%d}, want {%d,0}", replayed.Pts, replayed.PtsCount, deleted.Pts)
	}
	checkChannelReadState(subscriber, secondPostID, 0)
	select {
	case duplicate := <-subscriber.seen.delChannelMsg:
		t.Fatalf("subscriber received duplicate channel delete update: %+v", duplicate)
	case <-time.After(50 * time.Millisecond):
	case <-f.ctx.Done():
		t.Fatalf("waiting for duplicate channel delete update check: %s", contextFailureDescription(f.ctx))
	}
	deleteSmokeChannelPost(t, f, channelID, postIDs[posts[1]])
	assertPeerDialog(subscriber, postIDs[posts[0]], posts[0])
	deleteSmokeChannelPost(t, f, channelID, postIDs[posts[0]])
	assertPeerDialog(subscriber, 0, "")
}

func assertSmokePollCaption(t *testing.T, label string, message *tg.Message, want string) {
	t.Helper()
	if message.Message != want {
		t.Fatalf("%s caption = %q, want %q", label, message.Message, want)
	}
	entities, present := message.GetEntities()
	if want == "" {
		if present && len(entities) != 0 {
			t.Fatalf("%s entities = %#v, want none", label, entities)
		}
		return
	}
	if !present || len(entities) != 1 {
		t.Fatalf("%s entities = %#v present=%v, want one bold entity", label, entities, present)
	}
	bold, ok := entities[0].(*tg.MessageEntityBold)
	if !ok || bold.Offset != 0 || bold.Length != 11 {
		t.Fatalf("%s entity = %#v, want bold over Description", label, entities[0])
	}
}

func testSmokeChannelPollLifecycle(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneSubscriber = "+15551048101", "+15551048102"
	seedPhoneUsers(t, f.ctx, f.store, phoneCreator, phoneSubscriber)
	creator := newSmokeClient(t, f, "Poll creator", phoneCreator)
	subscriber := newSmokeClient(t, f, "Poll subscriber", phoneSubscriber)

	createPoll := func(channelID int64, randomID int64, question, description string) (int64, int, []byte, []byte) {
		t.Helper()
		var pollID int64
		var messageID int
		var firstOption []byte
		var secondOption []byte
		media := &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: question},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "B"}},
			},
		}}
		err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			request := &tg.MessagesSendMediaRequest{Peer: peerChannel(creator.id, channelID), Media: media, RandomID: randomID}
			if description != "" {
				request.Message = description
				request.Entities = []tg.MessageEntityClass{&tg.MessageEntityBold{Offset: 0, Length: 11}}
			}
			result, err := api.MessagesSendMedia(ctx, request)
			if err != nil {
				return err
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("send channel poll result = %T, want *tg.Updates", result)
			}
			for _, update := range updates.Updates {
				created, ok := update.(*tg.UpdateNewChannelMessage)
				if !ok {
					continue
				}
				message, ok := created.Message.(*tg.Message)
				if !ok {
					return fmt.Errorf("sent channel poll message = %T, want *tg.Message", created.Message)
				}
				pollMedia, ok := message.Media.(*tg.MessageMediaPoll)
				if !ok || pollMedia.Poll.ID <= 0 || pollMedia.Poll.Question.Text != question {
					return fmt.Errorf("sent channel poll media = %#v, want canonical poll %q", message.Media, question)
				}
				assertSmokePollCaption(t, "channel poll send response", message, description)
				answer, ok := pollMedia.Poll.Answers[0].(*tg.PollAnswer)
				if !ok || len(answer.Option) == 0 {
					return fmt.Errorf("sent channel poll first answer = %#v, want server-assigned option bytes", pollMedia.Poll.Answers[0])
				}
				firstOption = answer.Option
				answer, ok = pollMedia.Poll.Answers[1].(*tg.PollAnswer)
				if !ok || len(answer.Option) == 0 || string(answer.Option) == string(firstOption) {
					return fmt.Errorf("sent channel poll second answer = %#v, want a unique server-assigned option", pollMedia.Poll.Answers[1])
				}
				secondOption = answer.Option
				pollID, messageID = pollMedia.Poll.ID, message.ID
			}
			if pollID <= 0 || messageID <= 0 {
				return errors.New("send channel poll omitted updateNewChannelMessage")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("send channel poll %q: %v", question, err)
		}
		posted := recvOrCtx(t, f.ctx, subscriber.seen.newChannelMsg, "subscriber channel poll post")
		pollMedia, ok := posted.Msg.Media.(*tg.MessageMediaPoll)
		if !ok || pollMedia.Poll.ID != pollID || posted.Msg.ID != messageID {
			t.Fatalf("subscriber channel poll = %#v id %d, want poll %d message %d", posted.Msg.Media, posted.Msg.ID, pollID, messageID)
		}
		assertSmokePollCaption(t, "subscriber live channel poll", posted.Msg, description)
		return pollID, messageID, firstOption, secondOption
	}

	assertLiveVote := func(channelID, pollID int64, messageID int, option []byte) {
		t.Helper()
		var result tg.UpdatesClass
		if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.MessagesSendVote(ctx, &tg.MessagesSendVoteRequest{
				Peer: peerChannel(subscriber.id, channelID), MsgID: messageID, Options: [][]byte{option},
			})
			return err
		}); err != nil {
			t.Fatalf("cast channel poll vote: %v", err)
		}
		assertSmokeChannelPollVote(t, result, pollID)
		for _, client := range []*smokeClient{creator, subscriber} {
			live := recvSmokeChannelPollResults(t, f.ctx, client.seen.pollResults, pollID)
			assertSmokeChannelPollResults(t, live, pollID, 1)
		}
	}

	closePoll := func(channelID int64, pollID int64, messageID int, description string) {
		t.Helper()
		closed := &tg.InputMediaPoll{Poll: tg.Poll{Question: tg.TextWithEntities{Text: "ignored"}}}
		closed.Poll.SetClosed(true)
		var result tg.UpdatesClass
		if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.MessagesEditMessage(ctx, &tg.MessagesEditMessageRequest{
				Peer: peerChannel(creator.id, channelID), ID: messageID, Media: closed,
			})
			return err
		}); err != nil {
			t.Fatalf("close channel poll: %v", err)
		}
		if !smokeChannelPollCloseInUpdates(result, pollID) {
			t.Fatalf("close channel poll response omitted closed poll %d: %T", pollID, result)
		}
		live := recvOrCtx(t, f.ctx, subscriber.seen.editChannelMsg, "live channel poll close")
		pollMedia, ok := live.Msg.Media.(*tg.MessageMediaPoll)
		if !ok || !pollMedia.Poll.Closed || pollMedia.Poll.ID != pollID || live.Msg.ID != messageID {
			t.Fatalf("live channel poll edit = %#v id %d, want closed poll %d message %d", live.Msg.Media, live.Msg.ID, pollID, messageID)
		}
		if err := subscriber.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
				Channel: inputChannel(subscriber.id, channelID), ID: []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
			})
			if err != nil {
				return err
			}
			history, ok := result.(*tg.MessagesChannelMessages)
			if !ok || len(history.Messages) != 1 {
				return fmt.Errorf("channel poll close history = %T, want one channel message", result)
			}
			message, ok := history.Messages[0].(*tg.Message)
			if !ok {
				return fmt.Errorf("channel poll close history message = %T, want *tg.Message", history.Messages[0])
			}
			media, ok := message.Media.(*tg.MessageMediaPoll)
			if !ok || !media.Poll.Closed || media.Poll.ID != pollID {
				return fmt.Errorf("channel poll close history media = %#v, want closed poll %d", message.Media, pollID)
			}
			assertSmokePollCaption(t, "channel poll history", message, description)
			return nil
		}); err != nil {
			t.Fatalf("read closed channel poll: %v", err)
		}
	}

	groupID := createMegagroup(t, f.ctx, creator.cmds, "Smoke poll supergroup")
	groupInvite := exportChannelInvite(t, f.ctx, creator.id, creator.cmds, groupID)
	if joinedID := importChannelInvite(t, f.ctx, subscriber.cmds, groupInvite); joinedID != groupID {
		t.Fatalf("subscriber joined poll supergroup %d, want %d", joinedID, groupID)
	}
	groupPollID, groupMessageID, _, groupOption := createPoll(groupID, 1048101, "Supergroup poll?", "Description stuff")

	assertLiveVote(groupID, groupPollID, groupMessageID, groupOption)
	closePoll(groupID, groupPollID, groupMessageID, "Description stuff")

	broadcastID := createBroadcastChannel(t, f.ctx, creator.cmds, "Smoke poll broadcast")
	broadcastInvite := exportChannelInvite(t, f.ctx, creator.id, creator.cmds, broadcastID)
	if joinedID := importChannelInvite(t, f.ctx, subscriber.cmds, broadcastInvite); joinedID != broadcastID {
		t.Fatalf("subscriber joined poll broadcast %d, want %d", joinedID, broadcastID)
	}
	broadcastPollID, broadcastMessageID, _, broadcastOption := createPoll(broadcastID, 1048102, "Broadcast poll?", "")
	assertLiveVote(broadcastID, broadcastPollID, broadcastMessageID, broadcastOption)
	closePoll(broadcastID, broadcastPollID, broadcastMessageID, "")
}

func assertSmokeChannelPollVote(t *testing.T, result tg.UpdatesClass, pollID int64) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("channel vote result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		if poll, ok := update.(*tg.UpdateMessagePoll); ok {
			assertSmokeChannelPollResults(t, poll, pollID, 1)
			if poll.Peer != nil || poll.MsgID != 0 {
				t.Fatalf("channel vote response names voter message: %+v", poll)
			}
			return
		}
	}
	t.Fatalf("channel vote response omitted poll %d result: %+v", pollID, updates.Updates)
}

func assertSmokeChannelPollResults(t *testing.T, update *tg.UpdateMessagePoll, pollID int64, wantVoters int) {
	t.Helper()
	if update.PollID != pollID || update.Results.TotalVoters != wantVoters || len(update.Results.Results) != 2 || update.Results.Results[1].Voters != wantVoters {
		t.Fatalf("channel poll live result = %+v, want poll %d with %d voters on B", update, pollID, wantVoters)
	}
	if update.Peer != nil || update.MsgID != 0 {
		t.Fatalf("channel poll result reveals voter identity: %+v", update)
	}
}

func recvSmokeChannelPollResults(t *testing.T, ctx context.Context, updates <-chan *tg.UpdateMessagePoll, pollID int64) *tg.UpdateMessagePoll {
	t.Helper()
	for {
		select {
		case update := <-updates:
			if update.PollID == pollID {
				return update
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for live poll results %d: %v", pollID, ctx.Err())
			return nil
		}
	}
}

func smokeChannelPollCloseInUpdates(result tg.UpdatesClass, pollID int64) bool {
	updates, ok := result.(*tg.Updates)
	if !ok {
		return false
	}
	for _, update := range updates.Updates {
		edit, ok := update.(*tg.UpdateEditChannelMessage)
		if !ok {
			continue
		}
		message, ok := edit.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPoll)
		if ok && media.Poll.ID == pollID && media.Poll.Closed {
			return true
		}
	}
	return false
}

func testSmokeContactsSearch(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049001", "+15551049002"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)

	a := newSmokeClient(t, f, "A1", phoneA)
	b := newSmokeClient(t, f, "B1", phoneB)
	if err := f.store.ClaimUsername(f.ctx, a.id, "smokealpha"); err != nil {
		t.Fatalf("claim A username: %v", err)
	}
	if err := f.store.ClaimUsername(f.ctx, b.id, "smokebravo"); err != nil {
		t.Fatalf("claim B username: %v", err)
	}

	var search *tg.ContactsFound
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		search, err = api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: "smokebravo", Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("exact username search: %v", err)
	}
	if search == nil {
		t.Fatal("exact username search returned nil")
	}
	if len(search.Results) != 1 || len(search.Users) != 1 {
		t.Fatalf("exact username search results/users = %d/%d, want one each", len(search.Results), len(search.Users))
	}
	peer, ok := search.Results[0].(*tg.PeerUser)
	if !ok {
		t.Fatalf("exact username search peer type = %T, want *tg.PeerUser", search.Results[0])
	}
	if peer.UserID != b.id {
		t.Fatalf("exact username search peer id = %d, want user %d", peer.UserID, b.id)
	}
	searchUser, err := requireSmokeFullUser(search.Users, b.id, "contacts.search")
	if err != nil {
		t.Fatal(err)
	}

	var users []tg.UserClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		users, err = api.UsersGetUsers(ctx, []tg.InputUserClass{
			&tg.InputUser{UserID: searchUser.ID, AccessHash: searchUser.AccessHash},
		})
		return err
	}); err != nil {
		t.Fatalf("getUsers for search result: %v", err)
	}
	resolvedUser, err := requireSmokeFullUser(users, b.id, "users.getUsers")
	if err != nil {
		t.Fatal(err)
	}

	var added tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		added, err = api.ContactsAddContact(ctx, &tg.ContactsAddContactRequest{
			ID:        &tg.InputUser{UserID: resolvedUser.ID, AccessHash: resolvedUser.AccessHash},
			FirstName: "Smoke Bravo",
			Phone:     "+15550000000",
		})
		return err
	}); err != nil {
		t.Fatalf("add B as contact: %v", err)
	}
	if _, ok := added.(*tg.Updates); !ok {
		t.Fatalf("add contact response = %T, want *tg.Updates", added)
	}

	var contactResult tg.ContactsContactsClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		contactResult, err = api.ContactsGetContacts(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("A getContacts: %v", err)
	}
	contacts, ok := contactResult.(*tg.ContactsContacts)
	if !ok {
		t.Fatalf("A getContacts response = %T, want *tg.ContactsContacts", contactResult)
	}
	if contacts.SavedCount != 1 || len(contacts.Contacts) != 1 || contacts.Contacts[0].UserID != b.id {
		t.Fatalf("A getContacts saved/users = %d/%d, want B %d", contacts.SavedCount, len(contacts.Contacts), b.id)
	}
	contactUser, err := requireSmokeFullUser(contacts.Users, b.id, "A contacts.getContacts")
	if err != nil {
		t.Fatal(err)
	}
	if !contactUser.Contact {
		t.Fatalf("A getContacts B contact flag = false, want true")
	}

	var bContactsResult tg.ContactsContactsClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		bContactsResult, err = api.ContactsGetContacts(ctx, 0)
		return err
	}); err != nil {
		t.Fatalf("B getContacts: %v", err)
	}
	bContacts, ok := bContactsResult.(*tg.ContactsContacts)
	if !ok || bContacts.SavedCount != 0 || len(bContacts.Contacts) != 0 || len(bContacts.Users) != 0 {
		t.Fatalf("B sees A's one-sided contact edge: %T, want an empty contact list", bContactsResult)
	}

	const messageText = "contact-smoke"
	peerForSend := &tg.InputPeerUser{UserID: resolvedUser.ID, AccessHash: resolvedUser.AccessHash}
	var sendResult tg.UpdatesClass
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		sendResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerForSend, Message: messageText, RandomID: 1049001,
		})
		return err
	}); err != nil {
		t.Fatalf("send to searched B peer: %v", err)
	}
	sent, _, ok := outgoingMessage(t, sendResult, messageText)
	if !ok || countOutgoingMessages(sendResult, messageText) != 1 || sent.ID <= 0 || !sent.Out {
		t.Fatalf("send result does not contain one outgoing %q message", messageText)
	}
	sentPeer, ok := sent.PeerID.(*tg.PeerUser)
	if !ok || sentPeer.UserID != b.id {
		t.Fatalf("send result peer type/id = %T/%d, want B %d", sent.PeerID, smokePeerUserID(sent.PeerID), b.id)
	}

	var historyResult any
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		historyResult, err = api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peerForSend, Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("A getHistory with searched B peer: %v", err)
	}
	history, ok := historyResult.(*tg.MessagesMessages)
	if !ok {
		t.Fatalf("A getHistory response = %T, want *tg.MessagesMessages", historyResult)
	}
	if len(history.Messages) != 1 {
		t.Fatalf("A getHistory response = %T, want one message", historyResult)
	}
	historyMessage, ok := history.Messages[0].(*tg.Message)
	if !ok {
		t.Fatalf("A getHistory message = %T, want *tg.Message", history.Messages[0])
	}
	if historyMessage.Message != messageText || historyMessage.ID != sent.ID || !historyMessage.Out {
		t.Fatalf("A getHistory text/id/out = %q/%d/%v, want %q/%d/true", historyMessage.Message, historyMessage.ID, historyMessage.Out, messageText, sent.ID)
	}
	historyPeer, ok := historyMessage.PeerID.(*tg.PeerUser)
	if !ok || historyPeer.UserID != b.id {
		t.Fatalf("A getHistory peer type/id = %T/%d, want B %d", historyMessage.PeerID, smokePeerUserID(historyMessage.PeerID), b.id)
	}
	if _, err := requireSmokeFullUser(history.Users, b.id, "A messages.getHistory"); err != nil {
		t.Fatal(err)
	}
}

func testSmokeFullUserProfile(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049301", "+15551049302"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)

	a := newSmokeClient(t, f, "full-profile A", phoneA)
	b := newSmokeClient(t, f, "full-profile B", phoneB)
	if err := f.store.ClaimUsername(f.ctx, b.id, "smokefullprofile"); err != nil {
		t.Fatalf("claim B username: %v", err)
	}

	getFullUser := func(caller *smokeClient, id tg.InputUserClass) *tg.UsersUserFull {
		t.Helper()
		var full *tg.UsersUserFull
		if err := caller.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			full, err = api.UsersGetFullUser(ctx, id)
			return err
		}); err != nil {
			t.Fatalf("users.getFullUser: %v", err)
		}
		return full
	}

	// A reaches B the way the desktop client does: an exact username search, then
	// the profile read, with no message between them yet.
	var search *tg.ContactsFound
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		search, err = api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: "smokefullprofile", Limit: 10})
		return err
	}); err != nil {
		t.Fatalf("exact username search: %v", err)
	}
	searchUser, err := requireSmokeFullUser(search.Users, b.id, "contacts.search")
	if err != nil {
		t.Fatal(err)
	}

	searchOnly := getFullUser(a, &tg.InputUser{UserID: searchUser.ID, AccessHash: searchUser.AccessHash})
	profileUser, err := requireSmokeFullUser(searchOnly.Users, b.id, "users.getFullUser")
	if err != nil {
		t.Fatal(err)
	}
	if searchOnly.FullUser.ID != b.id || searchOnly.FullUser.Blocked {
		t.Fatalf("full user = {id:%d blocked:%t}, want B unblocked", searchOnly.FullUser.ID, searchOnly.FullUser.Blocked)
	}
	if profileUser.Phone != "" || profileUser.Self || profileUser.Username != "smokefullprofile" {
		t.Fatalf("full user record = {self:%t phone:%q username:%q}, want B's public view with no phone",
			profileUser.Self, profileUser.Phone, profileUser.Username)
	}
	if !searchOnly.FullUser.Settings.Zero() || len(searchOnly.Chats) != 0 || searchOnly.FullUser.CommonChatsCount != 0 || !searchOnly.FullUser.NotifySettings.Zero() {
		t.Fatalf("search-only full user = settings:%+v chats:%d common:%d notify:%+v, want empty bars and no reported common chats",
			searchOnly.FullUser.Settings, len(searchOnly.Chats), searchOnly.FullUser.CommonChatsCount, searchOnly.FullUser.NotifySettings)
	}
	if searchOnly.FullUser.PhoneCallsAvailable || searchOnly.FullUser.PhoneCallsPrivate || searchOnly.FullUser.VideoCallsAvailable ||
		searchOnly.FullUser.CanPinMessage || searchOnly.FullUser.HasScheduled || searchOnly.FullUser.VoiceMessagesForbidden {
		t.Fatalf("full user advertises a capability the server does not implement: %+v", searchOnly.FullUser)
	}
	if _, ok := searchOnly.FullUser.GetProfilePhoto(); ok {
		t.Fatalf("full user claims a profile photo the server does not store")
	}
	if _, ok := searchOnly.FullUser.GetAbout(); ok {
		t.Fatalf("full user claims a biography the server discards on write")
	}

	// A forged hash is refused without saying whether B exists.
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.UsersGetFullUser(ctx, &tg.InputUser{UserID: b.id, AccessHash: searchUser.AccessHash ^ 1})
		return err
	}); err == nil || !tgerr.Is(err, "PEER_ID_INVALID") {
		t.Fatalf("users.getFullUser with a forged hash error = %v, want PEER_ID_INVALID", err)
	}

	// B reading A never sees A's phone.
	bView := getFullUser(b, inputUser(b.id, a.id))
	bViewUser, err := requireSmokeFullUser(bView.Users, a.id, "users.getFullUser from B")
	if err != nil {
		t.Fatal(err)
	}
	if bViewUser.Phone != "" || bViewUser.Self {
		t.Fatalf("B's view of A = {self:%t phone:%q}, want no self flag and no phone", bViewUser.Self, bViewUser.Phone)
	}

	// A dialog turns the peer-settings bar on, and the profile carries exactly
	// what messages.getPeerSettings answers for the same peer.
	const messageText = "full-profile-smoke"
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     &tg.InputPeerUser{UserID: searchUser.ID, AccessHash: searchUser.AccessHash},
			Message:  messageText,
			RandomID: 1049301,
		})
		return err
	}); err != nil {
		t.Fatalf("send A to B: %v", err)
	}
	withDialog := getFullUser(a, inputUser(a.id, b.id))
	var peerSettings *tg.MessagesPeerSettings
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		peerSettings, err = api.MessagesGetPeerSettings(ctx, peerUser(a.id, b.id))
		return err
	}); err != nil {
		t.Fatalf("messages.getPeerSettings: %v", err)
	}
	if !reflect.DeepEqual(withDialog.FullUser.Settings, peerSettings.Settings) {
		t.Fatalf("full user settings = %+v, want the bar getPeerSettings answers %+v", withDialog.FullUser.Settings, peerSettings.Settings)
	}
	if !withDialog.FullUser.Settings.AddContact || !withDialog.FullUser.Settings.BlockContact {
		t.Fatalf("dialog settings = %+v, want add_contact and block_contact for a non-contact peer", withDialog.FullUser.Settings)
	}

	// Blocking is the caller's own state and moves both reads together.
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(a.id, b.id)})
		return err
	}); err != nil {
		t.Fatalf("A blocks B: %v", err)
	}
	blocked := getFullUser(a, inputUser(a.id, b.id))
	if !blocked.FullUser.Blocked {
		t.Fatalf("full user after A blocked B = blocked:%t, want true", blocked.FullUser.Blocked)
	}
	if blocked.FullUser.Settings.BlockContact {
		t.Fatalf("settings after blocking still offer block_contact: %+v", blocked.FullUser.Settings)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(a.id, b.id)})
		return err
	}); err != nil {
		t.Fatalf("A unblocks B: %v", err)
	}
	if unblocked := getFullUser(a, inputUser(a.id, b.id)); unblocked.FullUser.Blocked {
		t.Fatalf("full user after A unblocked B = blocked:%t, want false", unblocked.FullUser.Blocked)
	}

	// Self is the caller's own account, phone included, with no bar.
	self := getFullUser(a, &tg.InputUserSelf{})
	selfUser, err := requireSmokeFullUser(self.Users, a.id, "users.getFullUser self")
	if err != nil {
		t.Fatal(err)
	}
	if self.FullUser.ID != a.id || self.FullUser.Blocked || !self.FullUser.Settings.Zero() {
		t.Fatalf("self full user = {id:%d blocked:%t settings:%+v}, want A with no block and no bar",
			self.FullUser.ID, self.FullUser.Blocked, self.FullUser.Settings)
	}
	if !selfUser.Self || selfUser.Phone != store.NormalizePhone(phoneA) {
		t.Fatalf("self user = {self:%t phone:%q}, want A's own phone %q", selfUser.Self, selfUser.Phone, store.NormalizePhone(phoneA))
	}
}

func smokePeerUserID(peer tg.PeerClass) int64 {
	if user, ok := peer.(*tg.PeerUser); ok {
		return user.UserID
	}
	return 0
}

// smokeRegistrationBranch names one failure branch of the username registration
// scenario. The name stays inside the process: the public assertion ID is the
// literal in the matching assertion in testSmokeUsernameRegistration, and the CI
// sanitizer publishes that ID, its source location and the commit alone.
// Every branch is attributed by that scenario because the assertion
// markers must sit directly in it, which is where Go reports a failure from a
// t.Helper-guarded scenario.
type smokeRegistrationBranch string

const (
	branchReservedCreateUser       smokeRegistrationBranch = "reserved-create-pending-account"
	branchReservedSendCode         smokeRegistrationBranch = "reserved-send-code"
	branchReservedCodeWait         smokeRegistrationBranch = "reserved-code-wait"
	branchReservedSessionLoad      smokeRegistrationBranch = "reserved-session-load"
	branchReservedAuthKeyIDLength  smokeRegistrationBranch = "reserved-auth-key-id-length"
	branchReservedSetPendingUser   smokeRegistrationBranch = "reserved-set-pending-user"
	branchReservedSignIn           smokeRegistrationBranch = "reserved-sign-in"
	branchReservedSignInResponse   smokeRegistrationBranch = "reserved-sign-in-response"
	branchReservedSignUpAccepted   smokeRegistrationBranch = "reserved-sign-up-accepted"
	branchReservedSignUpUnexpected smokeRegistrationBranch = "reserved-sign-up-unexpected"
	branchReservedUsernameLookup   smokeRegistrationBranch = "reserved-username-lookup"
	branchReservedUsernameStored   smokeRegistrationBranch = "reserved-username-stored"
	branchSignupSendCode           smokeRegistrationBranch = "signup-send-code"
	branchSignupCodeWait           smokeRegistrationBranch = "signup-code-wait"
	branchSignupSessionLoad        smokeRegistrationBranch = "signup-session-load"
	branchSignupAuthKeyIDLength    smokeRegistrationBranch = "signup-auth-key-id-length"
	branchSignupSetPendingUser     smokeRegistrationBranch = "signup-set-pending-user"
	branchSignupSignIn             smokeRegistrationBranch = "signup-sign-in"
	branchSignupSignInResponse     smokeRegistrationBranch = "signup-sign-in-response"
	branchSignupSignUp             smokeRegistrationBranch = "signup-sign-up"
	branchSignupAuthorization      smokeRegistrationBranch = "signup-authorization"
	branchSignupUser               smokeRegistrationBranch = "signup-user"
	branchSignupRPCAfter           smokeRegistrationBranch = "signup-rpc-after"
	branchSignupUnexpectedPassword smokeRegistrationBranch = "signup-unexpected-password"
	branchSignupPasswordPrepare    smokeRegistrationBranch = "signup-password-prepare"
	branchSignupPasswordSet        smokeRegistrationBranch = "signup-password-set"
	branchSignInSendCode           smokeRegistrationBranch = "sign-in-send-code"
	branchSignInCodeWait           smokeRegistrationBranch = "sign-in-code-wait"
	branchSignInChallenge          smokeRegistrationBranch = "sign-in-challenge"
	branchSignInChallengeResponse  smokeRegistrationBranch = "sign-in-challenge-response"
	branchSignInGetPassword        smokeRegistrationBranch = "sign-in-get-password"
	branchSignInNoPassword         smokeRegistrationBranch = "sign-in-no-password"
	branchSignInProof              smokeRegistrationBranch = "sign-in-proof"
	branchSignInCheck              smokeRegistrationBranch = "sign-in-check"
	branchSignInAuthorization      smokeRegistrationBranch = "sign-in-authorization"
	branchSignInUser               smokeRegistrationBranch = "sign-in-user"
	branchSignInRPCAfter           smokeRegistrationBranch = "sign-in-rpc-after"
)

// smokeRegistrationRPCTypes lists the RPC error types this scenario expects. Any
// other RPC error is published as the bare Go type, so an unexpected server
// message can never widen what the public annotation carries.
var smokeRegistrationRPCTypes = map[string]bool{
	"SESSION_PASSWORD_NEEDED": true,
	"SIGN_UP_REQUIRED":        true,
	"USERNAME_INVALID":        true,
}

// smokeRegistrationDetail classifies err into the only failure detail this
// scenario publishes: an allow-listed RPC error type, a named session or context
// sentinel, or the bare Go type. Wrapped error text, session and auth-key bytes
// and IDs, login codes and code hashes, password and SRP material, account
// identifiers and DSNs are never part of it.
func smokeRegistrationDetail(err error) string {
	if err == nil {
		return "nil"
	}
	if rpc, ok := errors.AsType[*tgerr.Error](err); ok {
		if smokeRegistrationRPCTypes[rpc.Type] {
			return "tgerr.Error:" + rpc.Type
		}
		return "tgerr.Error"
	}
	switch {
	case errors.Is(err, session.ErrNotFound):
		return "session.ErrNotFound"
	case errors.Is(err, context.DeadlineExceeded):
		return "context.DeadlineExceeded"
	case errors.Is(err, context.Canceled):
		return "context.Canceled"
	}
	return fmt.Sprintf("%T", err)
}

// smokeRegistrationError attributes a registration failure to one branch. The
// underlying error is kept for the local test log only; detail is the classified
// text the scenario may publish.
type smokeRegistrationError struct {
	branch smokeRegistrationBranch
	detail string
	err    error
}

func (e *smokeRegistrationError) Error() string {
	if e.err != nil {
		return string(e.branch) + ": " + e.err.Error()
	}
	return string(e.branch)
}

func (e *smokeRegistrationError) Unwrap() error { return e.err }

// smokeRegistrationStepFailure wraps an error from one registration step.
func smokeRegistrationStepFailure(branch smokeRegistrationBranch, err error) error {
	return &smokeRegistrationError{branch: branch, detail: smokeRegistrationDetail(err), err: err}
}

// smokeRegistrationCheckFailure reports a registration assertion that did not
// hold. detail names the Go type of the checked value, never its value.
func smokeRegistrationCheckFailure(branch smokeRegistrationBranch, detail string) error {
	return &smokeRegistrationError{branch: branch, detail: detail}
}

// smokeRegistrationAttribution resolves the branch and publishable detail for an
// error leaving a registration phase. An error carrying no branch, such as a
// connection failure, is reported by that phase's unattributed assertion.
func smokeRegistrationAttribution(err error) (smokeRegistrationBranch, string) {
	if failure, ok := errors.AsType[*smokeRegistrationError](err); ok {
		return failure.branch, failure.detail
	}
	return "", smokeRegistrationDetail(err)
}

// smokeReservedUsernameSignUp signs up a reserved username and expects every
// signup step to reject it. Each failure is returned attributed to its branch;
// the scenario that calls this reports the assertion, so no marker literal
// belongs in this function.
func smokeReservedUsernameSignUp(f *smokeFixture, username, pendingPhone string) error {
	pending, err := f.store.CreateUser(f.ctx, pendingPhone)
	if err != nil {
		return smokeRegistrationStepFailure(branchReservedCreateUser, err)
	}

	sess := &session.StorageMemory{}
	client := f.savedSessionClient(sess)
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		api := client.API()
		codeHash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchReservedSendCode, err)
		}
		code, err := f.codes.wait(ctx, strings.ToLower(username))
		if err != nil {
			return smokeRegistrationStepFailure(branchReservedCodeWait, err)
		}
		sessionData, err := (&session.Loader{Storage: sess}).Load(ctx)
		if err != nil {
			return smokeRegistrationStepFailure(branchReservedSessionLoad, err)
		}
		if len(sessionData.AuthKeyID) != 8 {
			return smokeRegistrationCheckFailure(branchReservedAuthKeyIDLength, "length "+strconv.Itoa(len(sessionData.AuthKeyID)))
		}
		var authKeyID [8]byte
		copy(authKeyID[:], sessionData.AuthKeyID)
		if err := f.store.SetPendingUser(ctx, mtproto.AuthKeyIDInt64(authKeyID), pending.ID); err != nil {
			return smokeRegistrationStepFailure(branchReservedSetPendingUser, err)
		}

		response, err := signInUsername(ctx, api, username, codeHash, code)
		if err != nil {
			if !isSignUpRequired(err) {
				return smokeRegistrationStepFailure(branchReservedSignIn, err)
			}
		} else if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
			return smokeRegistrationCheckFailure(branchReservedSignInResponse, fmt.Sprintf("%T", response))
		}

		if _, err := signUpUsername(ctx, api, username, codeHash, "Smoke", "Reserved"); !isRPCMessage(err, "USERNAME_INVALID") {
			if err == nil {
				return smokeRegistrationCheckFailure(branchReservedSignUpAccepted, "accepted")
			}
			return smokeRegistrationStepFailure(branchReservedSignUpUnexpected, err)
		}
		return nil
	}); err != nil {
		return err
	}
	if _, found, err := f.store.UserByUsernameWithLoginMode(f.ctx, username); err != nil {
		return smokeRegistrationStepFailure(branchReservedUsernameLookup, err)
	} else if found {
		return smokeRegistrationCheckFailure(branchReservedUsernameStored, "stored")
	}
	return nil
}

func testSmokeUsernameRegistration(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithRegistration(t, config.RegistrationOpen)
	const username, pendingPhone, password = "smokenewacct", "+15551049003", "smoke-password-1049"
	if err := smokeReservedUsernameSignUp(f, "PiNg", "+15551049004"); err != nil {
		branch, detail := smokeRegistrationAttribution(err)
		switch branch {
		case branchReservedCreateUser:
			t.Fatalf("[assert:username-registration.reserved-create-pending-account] create pending reserved signup account: %s", detail)
		case branchReservedSendCode:
			t.Fatalf("[assert:username-registration.reserved-send-code] auth.sendCode for reserved signup: %s", detail)
		case branchReservedCodeWait:
			t.Fatalf("[assert:username-registration.reserved-code-wait] wait for in-memory reserved signup code: %s", detail)
		case branchReservedSessionLoad:
			t.Fatalf("[assert:username-registration.reserved-session-load] load reserved signup session: %s", detail)
		case branchReservedAuthKeyIDLength:
			t.Fatalf("[assert:username-registration.reserved-auth-key-id-length] reserved signup auth key id length, want 8: %s", detail)
		case branchReservedSetPendingUser:
			t.Fatalf("[assert:username-registration.reserved-set-pending-user] stage reserved test signup account: %s", detail)
		case branchReservedSignIn:
			t.Fatalf("[assert:username-registration.reserved-sign-in] signIn before reserved signup: %s", detail)
		case branchReservedSignInResponse:
			t.Fatalf("[assert:username-registration.reserved-sign-in-response] signIn before reserved signup response, want signup required: %s", detail)
		case branchReservedSignUpAccepted:
			t.Fatalf("[assert:username-registration.reserved-sign-up-accepted] reserved auth.signUp succeeded: %s", detail)
		case branchReservedSignUpUnexpected:
			t.Fatalf("[assert:username-registration.reserved-sign-up-unexpected] reserved auth.signUp expected USERNAME_INVALID: %s", detail)
		case branchReservedUsernameLookup:
			t.Fatalf("[assert:username-registration.reserved-username-lookup] lookup reserved smoke username: %s", detail)
		case branchReservedUsernameStored:
			t.Fatalf("[assert:username-registration.reserved-username-stored] reserved username was stored: %s", detail)
		default:
			t.Fatalf("[assert:username-registration.reserved-signup-unattributed] reject reserved username registration: %s", detail)
		}
	}
	pending, err := f.store.CreateUser(f.ctx, pendingPhone)
	if err != nil {
		t.Fatalf("[assert:username-registration.create-pending-account] create pending signup account: %T", err)
	}

	firstSession := &session.StorageMemory{}
	firstClient := f.savedSessionClient(firstSession)
	var accountID int64
	if err := firstClient.Run(f.ctx, func(ctx context.Context) error {
		api := firstClient.API()
		codeHash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupSendCode, err)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupCodeWait, err)
		}
		sessionData, err := (&session.Loader{Storage: firstSession}).Load(ctx)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupSessionLoad, err)
		}
		if len(sessionData.AuthKeyID) != 8 {
			return smokeRegistrationCheckFailure(branchSignupAuthKeyIDLength, "length "+strconv.Itoa(len(sessionData.AuthKeyID)))
		}
		var authKeyID [8]byte
		copy(authKeyID[:], sessionData.AuthKeyID)
		if err := f.store.SetPendingUser(ctx, mtproto.AuthKeyIDInt64(authKeyID), pending.ID); err != nil {
			return smokeRegistrationStepFailure(branchSignupSetPendingUser, err)
		}

		response, err := signInUsername(ctx, api, username, codeHash, code)
		if err != nil {
			if !isSignUpRequired(err) {
				return smokeRegistrationStepFailure(branchSignupSignIn, err)
			}
		} else if _, ok := response.(*tg.AuthAuthorizationSignUpRequired); !ok {
			return smokeRegistrationCheckFailure(branchSignupSignInResponse, fmt.Sprintf("%T", response))
		}

		response, err = signUpUsername(ctx, api, username, codeHash, "Smoke", "Account")
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupSignUp, err)
		}
		authorization, ok := response.(*tg.AuthAuthorization)
		if !ok || authorization.User == nil {
			return smokeRegistrationCheckFailure(branchSignupAuthorization, fmt.Sprintf("%T", response))
		}
		signupUser, ok := authorization.User.(*tg.User)
		if !ok || signupUser.ID <= 0 {
			return smokeRegistrationCheckFailure(branchSignupUser, fmt.Sprintf("%T", authorization.User))
		}
		accountID = signupUser.ID
		passwordState, err := api.AccountGetPassword(ctx)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupRPCAfter, err)
		}
		if passwordState.HasPassword {
			return smokeRegistrationCheckFailure(branchSignupUnexpectedPassword, "has password")
		}
		verifier, salt1, salt2, err := testComputeSRPVerifier([]byte(password))
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupPasswordPrepare, err)
		}
		_, err = api.AccountUpdatePasswordSettings(ctx, &tg.AccountUpdatePasswordSettingsRequest{
			Password: &tg.InputCheckPasswordEmpty{},
			NewSettings: tg.AccountPasswordInputSettings{
				NewAlgo: &tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
					Salt1: salt1,
					Salt2: salt2,
				},
				NewPasswordHash: verifier,
			},
		})
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupPasswordSet, err)
		}
		return nil
	}); err != nil {
		branch, detail := smokeRegistrationAttribution(err)
		switch branch {
		case branchSignupSendCode:
			t.Fatalf("[assert:username-registration.signup-send-code] auth.sendCode for signup: %s", detail)
		case branchSignupCodeWait:
			t.Fatalf("[assert:username-registration.signup-code-wait] wait for in-memory signup code: %s", detail)
		case branchSignupSessionLoad:
			t.Fatalf("[assert:username-registration.signup-session-load] load signup session: %s", detail)
		case branchSignupAuthKeyIDLength:
			t.Fatalf("[assert:username-registration.signup-auth-key-id-length] signup auth key id length, want 8: %s", detail)
		case branchSignupSetPendingUser:
			t.Fatalf("[assert:username-registration.signup-set-pending-user] stage test signup account: %s", detail)
		case branchSignupSignIn:
			t.Fatalf("[assert:username-registration.signup-sign-in] signIn before signup: %s", detail)
		case branchSignupSignInResponse:
			t.Fatalf("[assert:username-registration.signup-sign-in-response] signIn before signup response, want signup required: %s", detail)
		case branchSignupSignUp:
			t.Fatalf("[assert:username-registration.signup-sign-up] auth.signUp: %s", detail)
		case branchSignupAuthorization:
			t.Fatalf("[assert:username-registration.signup-authorization] signUp response, want authorization with a user: %s", detail)
		case branchSignupUser:
			t.Fatalf("[assert:username-registration.signup-user] signUp user, want a full user: %s", detail)
		case branchSignupRPCAfter:
			t.Fatalf("[assert:username-registration.signup-rpc-after] usable RPC after signUp: %s", detail)
		case branchSignupUnexpectedPassword:
			t.Fatalf("[assert:username-registration.signup-unexpected-password] new smoke account unexpectedly has a password: %s", detail)
		case branchSignupPasswordPrepare:
			t.Fatalf("[assert:username-registration.signup-password-prepare] prepare synthetic smoke password: %s", detail)
		case branchSignupPasswordSet:
			t.Fatalf("[assert:username-registration.signup-password-set] set synthetic smoke password: %s", detail)
		default:
			t.Fatalf("[assert:username-registration.signup-unattributed] open username registration: %s", detail)
		}
	}

	secondSession := &session.StorageMemory{}
	secondClient := f.savedSessionClient(secondSession)
	if err := secondClient.Run(f.ctx, func(ctx context.Context) error {
		api := secondClient.API()
		codeHash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignInSendCode, err)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignInCodeWait, err)
		}
		response, err := signInUsername(ctx, api, username, codeHash, code)
		if !isSessionPasswordNeeded(err) {
			if err != nil {
				return smokeRegistrationStepFailure(branchSignInChallenge, err)
			}
			return smokeRegistrationCheckFailure(branchSignInChallengeResponse, fmt.Sprintf("%T", response))
		}
		passwordState, err := api.AccountGetPassword(ctx)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignInGetPassword, err)
		}
		if !passwordState.HasPassword {
			return smokeRegistrationCheckFailure(branchSignInNoPassword, "no password")
		}
		proof, err := auth.PasswordHash([]byte(password), passwordState.SRPID, passwordState.SRPB, passwordState.SecureRandom, passwordState.CurrentAlgo)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignInProof, err)
		}
		response, err = api.AuthCheckPassword(ctx, proof)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignInCheck, err)
		}
		authorization, ok := response.(*tg.AuthAuthorization)
		if !ok || authorization.User == nil {
			return smokeRegistrationCheckFailure(branchSignInAuthorization, fmt.Sprintf("%T", response))
		}
		signinUser, ok := authorization.User.(*tg.User)
		if !ok || signinUser.ID != accountID {
			return smokeRegistrationCheckFailure(branchSignInUser, fmt.Sprintf("%T", authorization.User))
		}
		if _, err := api.AccountGetPassword(ctx); err != nil {
			return smokeRegistrationStepFailure(branchSignInRPCAfter, err)
		}
		return nil
	}); err != nil {
		branch, detail := smokeRegistrationAttribution(err)
		switch branch {
		case branchSignInSendCode:
			t.Fatalf("[assert:username-registration.sign-in-send-code] auth.sendCode for fresh sign-in: %s", detail)
		case branchSignInCodeWait:
			t.Fatalf("[assert:username-registration.sign-in-code-wait] wait for in-memory sign-in code: %s", detail)
		case branchSignInChallenge:
			t.Fatalf("[assert:username-registration.sign-in-challenge] fresh signIn expected password challenge: %s", detail)
		case branchSignInChallengeResponse:
			t.Fatalf("[assert:username-registration.sign-in-challenge-response] fresh signIn response, want password challenge: %s", detail)
		case branchSignInGetPassword:
			t.Fatalf("[assert:username-registration.sign-in-get-password] get fresh sign-in password challenge: %s", detail)
		case branchSignInNoPassword:
			t.Fatalf("[assert:username-registration.sign-in-no-password] fresh sign-in account has no password: %s", detail)
		case branchSignInProof:
			t.Fatalf("[assert:username-registration.sign-in-proof] compute fresh sign-in proof: %s", detail)
		case branchSignInCheck:
			t.Fatalf("[assert:username-registration.sign-in-check] complete fresh signIn: %s", detail)
		case branchSignInAuthorization:
			t.Fatalf("[assert:username-registration.sign-in-authorization] fresh signIn response, want an authorization with a user: %s", detail)
		case branchSignInUser:
			t.Fatalf("[assert:username-registration.sign-in-user] fresh signIn user, want the signed-up user: %s", detail)
		case branchSignInRPCAfter:
			t.Fatalf("[assert:username-registration.sign-in-rpc-after] usable RPC after fresh signIn: %s", detail)
		default:
			t.Fatalf("[assert:username-registration.sign-in-unattributed] fresh username sign-in: %s", detail)
		}
	}
}

func requireSmokeFullUser(users []tg.UserClass, userID int64, source string) (*tg.User, error) {
	var found *tg.User
	for _, candidate := range users {
		switch user := candidate.(type) {
		case *tg.User:
			if user.ID == userID {
				if found != nil {
					return nil, fmt.Errorf("%s returned duplicate user %d", source, userID)
				}
				found = user
			}
		case *tg.UserEmpty:
			if user.ID == userID {
				return nil, fmt.Errorf("%s returned userEmpty for user %d", source, userID)
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%s omitted user %d", source, userID)
	}
	if found.AccessHash == 0 {
		return nil, fmt.Errorf("%s returned user %d without a usable peer identity", source, userID)
	}
	return found, nil
}

type smokeFixture struct {
	ctx                   context.Context
	failures              *clientFailureSignal
	key                   *rsa.PrivateKey
	dsn                   string
	store                 *store.Store
	codes                 *multiCodeSink
	dcID                  int
	port                  int
	listener              *acceptCountingListener
	registry              *mtproto.SessionRegistry
	stop                  func()
	stopCleanupRegistered bool
	regMode               config.RegistrationMode
	// rateLimits is the per-surface budget set the booted server gets.
	// Zero, the fixture default, means no surface is limited, which is what
	// every existing scenario assumes. A scenario that exists to prove a shipped
	// budget does not throttle it sets this to config.DefaultRateLimits().
	rateLimits config.RateLimitsConfig
}

func TestSmokeFixtureRestartKeepsClientCleanupAheadOfServerStop(t *testing.T) {
	var got []string
	var cleanups []func()
	registerCleanup := func(cleanup func()) { cleanups = append(cleanups, cleanup) }
	fixture := &smokeFixture{}
	fixture.setServerStop(registerCleanup, func() { got = append(got, "initial server") })
	registerCleanup(func() { got = append(got, "clients") })
	fixture.setServerStop(registerCleanup, func() { got = append(got, "restarted server") })
	for _, cleanup := range slices.Backward(cleanups) {
		cleanup()
	}
	if want := []string{"clients", "restarted server"}; !slices.Equal(got, want) {
		t.Fatalf("cleanup order = %v, want %v", got, want)
	}
}

func (f *smokeFixture) setServerStop(registerCleanup func(func()), stop func()) {
	f.stop = stop
	if f.stopCleanupRegistered {
		return
	}
	f.stopCleanupRegistered = true
	registerCleanup(func() {
		if f.stop != nil {
			f.stop()
		}
	})
}

// secretChatAccessHash is the hash one viewer carries for a secret chat. Each
// party's hash is derived from its own id, so the responder cannot name a chat
// with the hash the initiator was shown, the same rule the peer helpers in
// main_test.go encode for users and channels.
func secretChatAccessHash(viewerID int64, chatID int) int64 {
	return pgtest.PeerDeriver().Derive(viewerID, peerhash.KindSecret, int64(chatID))
}

// smokeGA and smokeGB are group elements inside the range the server accepts:
// half the canonical modulus and a third of it, in the left-zero-padded 256-byte
// wire form every integer in that group takes. The server validates the element
// and stores it; it does not run the exchange, so a smoke scenario needs values
// that pass validation and nothing more.
func smokeGA() []byte {
	return new(big.Int).Rsh(new(big.Int).SetBytes(srp.PBytes()), 1).FillBytes(make([]byte, 256))
}

func smokeGB() []byte {
	return new(big.Int).Div(new(big.Int).SetBytes(srp.PBytes()), big.NewInt(3)).FillBytes(make([]byte, 256))
}

// testSmokeSecretChatExchange is the secret-chat happy path at the budgets that
// ship, not the fixture default of no budgets. messages.requestEncryption and
// messages.discardEncryption now carry per-account allowances
// (10 and 30 per hour), so the exchange a real user performs has to stay inside
// them and still complete end to end: the initiator is shown a waiting chat, the
// responder's accept carries the agreed key material back, and the initiator's
// discard is answered with the discarded state. The second round on the same two
// accounts is the point of running it twice: the bound is an allowance per window,
// not a one-shot, and a legitimate pair must never be throttled.
func testSmokeSecretChatExchange(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.rateLimits = config.DefaultRateLimits()
	})
	const phoneA, phoneB = "+15551046101", "+15551046102"
	seedPhoneUsers(t, f.ctx, f.store, phoneA, phoneB)

	a := newSmokeClient(t, f, "A", phoneA)
	b := newSmokeClient(t, f, "B", phoneB)

	seenIDs := make([]int, 0, 2)
	for round := range 2 {
		var waiting *tg.EncryptedChatWaiting
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			chat, err := api.MessagesRequestEncryption(ctx, &tg.MessagesRequestEncryptionRequest{
				UserID:   inputUser(a.id, b.id),
				RandomID: 1046100 + round,
				GA:       smokeGA(),
			})
			if err != nil {
				return err
			}
			w, ok := chat.(*tg.EncryptedChatWaiting)
			if !ok {
				return fmt.Errorf("requestEncryption = %T, want *tg.EncryptedChatWaiting", chat)
			}
			waiting = w
			return nil
		}); err != nil {
			t.Fatalf("round %d: requestEncryption: %v", round, err)
		}
		if waiting.AdminID != a.id || waiting.ParticipantID != b.id {
			t.Fatalf("round %d: waiting chat parties = %d/%d, want %d/%d",
				round, waiting.AdminID, waiting.ParticipantID, a.id, b.id)
		}
		if waiting.Date <= 0 {
			t.Fatalf("round %d: waiting chat Date = %d, want a positive timestamp", round, waiting.Date)
		}
		chatID := waiting.ID
		for _, id := range seenIDs {
			if id == chatID {
				t.Fatalf("round %d: chat id %d repeated across rounds", round, chatID)
			}
		}
		seenIDs = append(seenIDs, chatID)

		const fingerprint = 9046100
		gb := smokeGB()
		var active *tg.EncryptedChat
		if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			chat, err := api.MessagesAcceptEncryption(ctx, &tg.MessagesAcceptEncryptionRequest{
				Peer:           tg.InputEncryptedChat{ChatID: chatID, AccessHash: secretChatAccessHash(b.id, chatID)},
				GB:             gb,
				KeyFingerprint: fingerprint,
			})
			if err != nil {
				return err
			}
			c, ok := chat.(*tg.EncryptedChat)
			if !ok {
				return fmt.Errorf("acceptEncryption = %T, want *tg.EncryptedChat", chat)
			}
			active = c
			return nil
		}); err != nil {
			t.Fatalf("round %d: acceptEncryption: %v", round, err)
		}
		if active.ID != chatID {
			t.Fatalf("round %d: accepted chat id = %d, want %d", round, active.ID, chatID)
		}
		if string(active.GAOrB) != string(gb) {
			t.Fatalf("round %d: accepted chat did not carry the responder's g_b back", round)
		}
		if active.KeyFingerprint != fingerprint {
			t.Fatalf("round %d: key fingerprint = %d, want %d", round, active.KeyFingerprint, fingerprint)
		}

		// messages.discardEncryption is answered with encryptedChatDiscarded, which
		// gotd's typed wrapper for this method does not decode, so the discard goes
		// through the raw invoker and the union box that does.
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var box tg.EncryptedChatBox
			if err := api.Invoker().Invoke(ctx, &tg.MessagesDiscardEncryptionRequest{ChatID: chatID}, &box); err != nil {
				return err
			}
			discarded, ok := box.EncryptedChat.(*tg.EncryptedChatDiscarded)
			if !ok {
				return fmt.Errorf("discardEncryption = %T, want *tg.EncryptedChatDiscarded", box.EncryptedChat)
			}
			if discarded.ID != chatID {
				return fmt.Errorf("discardEncryption named chat %d, want %d", discarded.ID, chatID)
			}
			return nil
		}); err != nil {
			t.Fatalf("round %d: discardEncryption: %v", round, err)
		}

		chat, err := f.store.SecretChatByID(f.ctx, int32(chatID)) //nolint:gosec // chat id is int32 on the wire
		if err != nil {
			t.Fatalf("round %d: load chat %d: %v", round, chatID, err)
		}
		if chat.State != store.SecretChatDiscarded {
			t.Fatalf("round %d: chat state = %q, want %q", round, chat.State, store.SecretChatDiscarded)
		}
	}
}

func newSmokeFixture(t *testing.T) *smokeFixture {
	t.Helper()
	return newSmokeFixtureWithRegistration(t, config.RegistrationClosed)
}

func newSmokeFixtureWithRegistration(t *testing.T, regMode config.RegistrationMode) *smokeFixture {
	t.Helper()
	return newSmokeFixtureWithSetup(t, regMode, nil)
}

func newSmokeFixtureWithSetup(t *testing.T, regMode config.RegistrationMode, beforeStart func(*smokeFixture)) *smokeFixture {
	t.Helper()
	return newSmokeFixtureWithDeadline(t, regMode, beforeStart, 90*time.Second)
}

func newSmokeFixtureWithDeadline(t *testing.T, regMode config.RegistrationMode, beforeStart func(*smokeFixture), timeout time.Duration) *smokeFixture {
	t.Helper()
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancelDeadline)
	deadlineCtx = withRegistrySnapshotState(deadlineCtx)
	ctx, cancelFailure := context.WithCancelCause(deadlineCtx)
	t.Cleanup(func() { cancelFailure(nil) })
	key, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	dsn := pgtest.DSN(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(testBlobs(t)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	f := &smokeFixture{ctx: ctx, failures: newClientFailureSignal(cancelFailure), key: key, dsn: dsn, store: st, codes: newMultiCodeSink(), dcID: 2, regMode: regMode}
	if beforeStart != nil {
		beforeStart(f)
	}
	t.Cleanup(func() {
		if f.stop != nil {
			f.stop()
		}
	})
	f.start(t, "127.0.0.1:0")
	return f
}

func (f *smokeFixture) start(t *testing.T, address string) {
	t.Helper()
	baseListener := mustListen(t, f.ctx, address)
	ln := newAcceptCountingListener(baseListener)
	if _, ok := ln.Addr().(*net.TCPAddr); !ok {
		t.Fatalf("listener addr type = %T", ln.Addr())
	}
	f.port = tcpPort(t, ln)
	f.listener = ln
	registry, stop := bootServerWithLimitsAndRegistrationMode(t, f.ctx, f.key, f.dcID, f.store, f.dsn, f.codes.Logger(), ln, f.rateLimits, f.regMode)
	f.registry = registry
	f.setServerStop(t.Cleanup, stop)
}

func (f *smokeFixture) restart(t *testing.T) {
	t.Helper()
	f.stop()
	f.start(t, fmt.Sprintf("127.0.0.1:%d", f.port))
}

func (f *smokeFixture) managedClient(sess *session.StorageMemory, seen, push *updateCollector, manager *updates.Manager) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: sess,
		UpdateHandler:  observedManagerHandler{observer: push, manager: manager},
		Middlewares: []telegram.Middleware{
			hook.UpdateHook(manager.Handle),
			hook.AffectedHook(manager),
		},
	})
}

func (f *smokeFixture) savedSessionClient(sess *session.StorageMemory) *telegram.Client {
	return f.savedSessionClientWithSystemLangCode(sess, "en")
}

func (f *smokeFixture) savedSessionClientWithSystemLangCode(sess *session.StorageMemory, systemLangCode string) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: sess,
		Device:         telegram.DeviceConfig{SystemLangCode: systemLangCode},
	})
}

type smokeClient struct {
	client    *telegram.Client
	session   *session.StorageMemory
	manager   *updates.Manager
	seen      *updateCollector
	push      *updateCollector
	cmds      chan command
	lifecycle *clientLifecycle
	label     string
	id        int64
	stop      sync.Once
}

func newSmokeClient(t *testing.T, f *smokeFixture, label, phone string) *smokeClient {
	t.Helper()
	sess := &session.StorageMemory{}
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	client := &smokeClient{
		client:  f.managedClient(sess, seen, push, manager),
		session: sess,
		manager: manager,
		seen:    seen,
		push:    push,
		cmds:    make(chan command),
		label:   label,
	}
	flow := auth.NewFlow(
		auth.Constant(phone, "", auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			return f.codes.wait(ctx, phone)
		})),
		auth.SendCodeOptions{},
	)
	ids, ready := make(chan int64, 1), make(chan struct{}, 1)
	client.lifecycle = startClientLifecycle(f.ctx, label, f.failures, func(phase *clientPhaseState) error {
		return runManagedInteractive(f.ctx, client.client, flow, ids, ready, client.cmds, manager, true, phase)
	})
	t.Cleanup(func() { client.stopClient(t) })
	loginStarted := time.Now()
	select {
	case client.id = <-ids:
	case <-f.ctx.Done():
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), contextFailureDescription(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("login", time.Since(loginStarted), "cause="+safeErrorClass(client.lifecycle.result.error())))
	}
	managerStarted := time.Now()
	select {
	case <-ready:
	case <-f.ctx.Done():
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), contextFailureDescription(f.ctx)))
	case <-client.lifecycle.result.done:
		t.Fatalf("%s", client.lifecycle.diagnostic("manager readiness", time.Since(managerStarted), "cause="+safeErrorClass(client.lifecycle.result.error())))
	}
	return client
}

func (c *smokeClient) call(ctx context.Context, fn func(context.Context, *tg.Client) error) error {
	started := time.Now()
	done := make(chan error, 1)
	select {
	case c.cmds <- command{fn: fn, done: done}:
	case <-c.lifecycle.result.done:
		return errors.New(c.lifecycle.diagnostic("command", time.Since(started), "cause="+safeErrorClass(c.lifecycle.result.error())))
	case <-ctx.Done():
		return errors.New(c.lifecycle.diagnostic("command", time.Since(started), contextFailureDescription(ctx)))
	}
	select {
	case err := <-done:
		return err
	case <-c.lifecycle.result.done:
		return errors.New(c.lifecycle.diagnostic("command", time.Since(started), "cause="+safeErrorClass(c.lifecycle.result.error())))
	case <-ctx.Done():
		return errors.New(c.lifecycle.diagnostic("command", time.Since(started), contextFailureDescription(ctx)))
	}
}

func (c *smokeClient) stopClient(t *testing.T) {
	t.Helper()
	c.stop.Do(func() {
		stopClientLifecycle(t, c.lifecycle, func() { close(c.cmds) })
		c.manager.Reset()
	})
}

type smokeSend struct {
	Message string
	ID      int
}

func assertSmokeSendResult(t *testing.T, result tg.UpdatesClass, text string, wantID, wantPts int) smokeSend {
	t.Helper()
	message, pts, ok := outgoingMessage(t, result, text)
	if !ok || countOutgoingMessages(result, text) != 1 {
		t.Fatalf("sendMessage result omitted exactly one outgoing %q", text)
	}
	if message.ID != wantID {
		t.Fatalf("outgoing %q id = %d, want %d", text, message.ID, wantID)
	}
	if pts != wantPts {
		t.Fatalf("outgoing %q pts = %d, want %d", text, pts, wantPts)
	}
	return smokeSend{Message: message.Message, ID: message.ID}
}

type smokeHistoryMessage struct {
	id  int
	out bool
}

func verifySmokeHistory(
	ctx context.Context,
	api *tg.Client,
	peer tg.InputPeerClass,
	peerID int64,
	want map[string]smokeHistoryMessage,
) error {
	result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 10})
	if err != nil {
		return err
	}
	history, ok := result.(*tg.MessagesMessages)
	if !ok {
		return fmt.Errorf("history response = %T, want *tg.MessagesMessages", result)
	}
	if len(history.Messages) != len(want) {
		return fmt.Errorf("history count = %d, want %d", len(history.Messages), len(want))
	}
	seen := make(map[string]bool, len(want))
	for _, class := range history.Messages {
		message, ok := class.(*tg.Message)
		if !ok {
			return fmt.Errorf("history message = %T, want *tg.Message", class)
		}
		expect, ok := want[message.Message]
		if !ok {
			return fmt.Errorf("history contains unexpected text %q", message.Message)
		}
		if seen[message.Message] {
			return fmt.Errorf("history contains duplicate text %q", message.Message)
		}
		seen[message.Message] = true
		if message.ID != expect.id || message.Out != expect.out {
			return fmt.Errorf("history %q = {id:%d out:%v}, want {id:%d out:%v}", message.Message, message.ID, message.Out, expect.id, expect.out)
		}
		peer, ok := message.PeerID.(*tg.PeerUser)
		if !ok || peer.UserID != peerID {
			return fmt.Errorf("history %q peer = %+v, want user %d", message.Message, message.PeerID, peerID)
		}
	}
	for text := range want {
		if !seen[text] {
			return fmt.Errorf("history is missing %q", text)
		}
	}
	return nil
}

func smokeGroupDialog(ctx context.Context, api *tg.Client, chatID int64) (*tg.Dialog, error) {
	result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      20,
	})
	if err != nil {
		return nil, err
	}
	var dialogs []tg.DialogClass
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		dialogs = response.Dialogs
	case *tg.MessagesDialogsSlice:
		dialogs = response.Dialogs
	default:
		return nil, fmt.Errorf("getDialogs response = %T, want MessagesDialogs or MessagesDialogsSlice", result)
	}
	for _, entry := range dialogs {
		dialog, ok := entry.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerChat)
		if ok && peer.ChatID == chatID {
			return dialog, nil
		}
	}
	return nil, fmt.Errorf("getDialogs omitted chat %d", chatID)
}

func smokeChannelDialog(ctx context.Context, api *tg.Client, channelID int64) (*tg.Dialog, error) {
	result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{},
		Limit:      20,
	})
	if err != nil {
		return nil, err
	}
	var dialogs []tg.DialogClass
	switch response := result.(type) {
	case *tg.MessagesDialogs:
		dialogs = response.Dialogs
	case *tg.MessagesDialogsSlice:
		dialogs = response.Dialogs
	default:
		return nil, fmt.Errorf("getDialogs response = %T, want MessagesDialogs or MessagesDialogsSlice", result)
	}
	for _, entry := range dialogs {
		dialog, ok := entry.(*tg.Dialog)
		if !ok {
			continue
		}
		peer, ok := dialog.Peer.(*tg.PeerChannel)
		if ok && peer.ChannelID == channelID {
			return dialog, nil
		}
	}
	return nil, fmt.Errorf("getDialogs omitted channel %d", channelID)
}

func deleteSmokeChannelPost(t *testing.T, f *smokeFixture, channelID int64, messageID int) {
	t.Helper()
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to smoke database to delete channel post: %v", err)
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close smoke database connection: %v", err)
		}
	}()
	tag, err := conn.Exec(f.ctx, `
		UPDATE channel_messages
		SET deleted = true
		WHERE channel_id = $1 AND local_id = $2 AND deleted = false`, channelID, messageID)
	if err != nil {
		t.Fatalf("delete smoke channel post %d: %v", messageID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete smoke channel post %d affected %d rows, want 1", messageID, tag.RowsAffected())
	}
}

func verifySmokeGroupHistory(
	ctx context.Context,
	api *tg.Client,
	chatID, viewerID int64,
	want map[string]int64,
) error {
	result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 10,
	})
	if err != nil {
		return err
	}
	history, ok := result.(*tg.MessagesMessages)
	if !ok {
		return fmt.Errorf("group history response = %T, want *tg.MessagesMessages", result)
	}
	seen := make(map[string]bool, len(want))
	ids := make(map[int]bool, len(want))
	for _, class := range history.Messages {
		message, ok := class.(*tg.Message)
		if !ok {
			if _, service := class.(*tg.MessageService); service {
				continue
			}
			return fmt.Errorf("group history message = %T, want *tg.Message or *tg.MessageService", class)
		}
		senderID, ok := want[message.Message]
		if !ok {
			return fmt.Errorf("group history contains unexpected text %q", message.Message)
		}
		if seen[message.Message] {
			return fmt.Errorf("group history contains duplicate text %q", message.Message)
		}
		seen[message.Message] = true
		if message.ID <= 0 || ids[message.ID] {
			return fmt.Errorf("group history %q has invalid owner-local id %d", message.Message, message.ID)
		}
		ids[message.ID] = true
		if message.Out != (senderID == viewerID) {
			return fmt.Errorf("group history %q out = %t for viewer %d, want %t", message.Message, message.Out, viewerID, senderID == viewerID)
		}
		from, ok := message.FromID.(*tg.PeerUser)
		if !ok || from.UserID != senderID {
			return fmt.Errorf("group history %q sender = %+v, want user %d", message.Message, message.FromID, senderID)
		}
		peer, ok := message.PeerID.(*tg.PeerChat)
		if !ok || peer.ChatID != chatID {
			return fmt.Errorf("group history %q peer = %+v, want chat %d", message.Message, message.PeerID, chatID)
		}
	}
	for text := range want {
		if !seen[text] {
			return fmt.Errorf("group history for %d is missing %q", viewerID, text)
		}
	}
	return nil
}

func smokeGroupMessageID(ctx context.Context, api *tg.Client, chatID int64, wantText string) (int, error) {
	result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 20,
	})
	if err != nil {
		return 0, err
	}
	history, ok := result.(*tg.MessagesMessages)
	if !ok {
		return 0, fmt.Errorf("group history response = %T, want *tg.MessagesMessages", result)
	}
	for _, class := range history.Messages {
		message, ok := class.(*tg.Message)
		if !ok || message.Message != wantText {
			continue
		}
		peer, ok := message.PeerID.(*tg.PeerChat)
		if !ok || peer.ChatID != chatID {
			return 0, fmt.Errorf("group message %q peer = %+v, want chat %d", wantText, message.PeerID, chatID)
		}
		return message.ID, nil
	}
	return 0, fmt.Errorf("group history is missing %q", wantText)
}

func verifySmokeGroupPin(ctx context.Context, api *tg.Client, chatID int64, wantID int, wantText string) error {
	fullResult, err := api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return fmt.Errorf("getFullChat: %w", err)
	}
	full, ok := fullResult.FullChat.(*tg.ChatFull)
	if !ok {
		return fmt.Errorf("full chat = %T, want *tg.ChatFull", fullResult.FullChat)
	}
	pinnedID, ok := full.GetPinnedMsgID()
	if !ok || pinnedID != wantID {
		return fmt.Errorf("full chat pinned id = %d/%t, want %d", pinnedID, ok, wantID)
	}
	gotID, err := smokeGroupMessageID(ctx, api, chatID, wantText)
	if err != nil {
		return err
	}
	if gotID != wantID {
		return fmt.Errorf("pinned text %q has owner-local id %d, want %d", wantText, gotID, wantID)
	}
	return nil
}

func verifySmokeGroupUnpinned(ctx context.Context, api *tg.Client, chatID int64) error {
	result, err := api.MessagesGetFullChat(ctx, chatID)
	if err != nil {
		return fmt.Errorf("getFullChat: %w", err)
	}
	full, ok := result.FullChat.(*tg.ChatFull)
	if !ok {
		return fmt.Errorf("full chat = %T, want *tg.ChatFull", result.FullChat)
	}
	if id, present := full.GetPinnedMsgID(); present && id != 0 {
		return fmt.Errorf("full chat pinned id = %d after unpin, want none", id)
	}
	return nil
}

func assertSmokePinResult(t *testing.T, result tg.UpdatesClass, wantPinned bool, chatID int64, wantMessages []int) {
	t.Helper()
	updates, ok := result.(*tg.Updates)
	if !ok {
		t.Fatalf("pin result = %T, want *tg.Updates", result)
	}
	for _, update := range updates.Updates {
		pinned, ok := update.(*tg.UpdatePinnedMessages)
		if ok {
			assertSmokePinnedUpdate(t, pinned, wantPinned, chatID, wantMessages)
			return
		}
	}
	t.Fatal("pin result omitted updatePinnedMessages")
}

func assertSmokePinnedUpdate(t *testing.T, update *tg.UpdatePinnedMessages, wantPinned bool, chatID int64, wantMessages []int) {
	t.Helper()
	peer, ok := update.Peer.(*tg.PeerChat)
	if update.Pinned != wantPinned || !ok || peer.ChatID != chatID {
		t.Fatalf("pin update = {pinned:%t peer:%T}, want pinned=%t chat=%d", update.Pinned, update.Peer, wantPinned, chatID)
	}
	if len(update.Messages) != len(wantMessages) {
		t.Fatalf("pin update messages = %v, want %v", update.Messages, wantMessages)
	}
	for i, id := range wantMessages {
		if update.Messages[i] != id {
			t.Fatalf("pin update messages = %v, want %v", update.Messages, wantMessages)
		}
	}
}

func assertSmokeReconnect(
	t *testing.T,
	f *smokeFixture,
	sess *session.StorageMemory,
	wantUserID, viewerID, peerID int64,
	want map[string]smokeHistoryMessage,
) {
	t.Helper()
	client := f.savedSessionClient(sess)
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !status.Authorized || status.User == nil || status.User.ID != wantUserID {
			return fmt.Errorf("reconnected authorization = %+v, want authorized user %d", status, wantUserID)
		}
		return verifySmokeHistory(ctx, client.API(), peerUser(viewerID, peerID), peerID, want)
	}); err != nil {
		t.Fatalf("reconnect saved session for %d: %v", wantUserID, err)
	}
}
