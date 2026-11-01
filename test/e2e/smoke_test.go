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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/updates/hook"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/gotd/td/transport"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/catalog"
	"github.com/teagramhq/teagram-server/internal/catalogpublish"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/photothumb"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestSmoke(t *testing.T) {
	t.Run("bot-api-get-me", func(t *testing.T) {
		t.Parallel()
		testSmokeBotAPIGetMe(t)
	})
	t.Run("bot-api-send-message", func(t *testing.T) {
		t.Parallel()
		testSmokeBotAPISendMessage(t)
	})
	t.Run("turnstile-signup", func(t *testing.T) {
		t.Parallel()
		testSmokeTurnstileSignUp(t)
	})
	t.Run("turnstile-signin", func(t *testing.T) {
		t.Parallel()
		testSmokeTurnstileSignIn(t)
	})
	t.Run("unread-acknowledgements", func(t *testing.T) {
		t.Parallel()
		testSmokeUnreadAcknowledgements(t)
	})
	t.Run("launch-getters", func(t *testing.T) {
		t.Parallel()
		testSmokeLaunchGetters(t)
	})
	t.Run("sticker-set-catalogue", func(t *testing.T) {
		t.Parallel()
		testSmokeStickerSetCatalogue(t)
	})
	t.Run("sticker-bundle-catalogue", func(t *testing.T) {
		t.Parallel()
		testSmokeStickerBundleCatalogue(t)
	})
	t.Run("user-created-sticker-sets", func(t *testing.T) {
		t.Parallel()
		testSmokeUserCreatedStickerSets(t)
	})
	t.Run("help-countries", func(t *testing.T) {
		t.Parallel()
		testSmokeHelpCountries(t)
	})
	t.Run("reset-authorizations", func(t *testing.T) {
		t.Parallel()
		testSmokeResetAuthorizations(t)
	})
	t.Run("authorization-device-metadata", func(t *testing.T) {
		t.Parallel()
		testSmokeAuthorizationDeviceMetadata(t)
	})
	t.Run("authorization-ttl", func(t *testing.T) {
		t.Parallel()
		testSmokeAuthorizationTTL(t)
	})
	t.Run("last-seen-privacy", func(t *testing.T) {
		t.Parallel()
		testSmokeLastSeenPrivacy(t)
	})
	t.Run("contacts-get-statuses", func(t *testing.T) {
		t.Parallel()
		testSmokeContactsGetStatuses(t)
	})
	t.Run("one-to-one", func(t *testing.T) {
		t.Parallel()
		testSmokeOneToOne(t)
	})
	t.Run("history-ttl", func(t *testing.T) {
		t.Parallel()
		testSmokeHistoryTTL(t)
	})
	t.Run("peer-disconnect", func(t *testing.T) {
		t.Parallel()
		testSmokePeerDisconnect(t)
	})
	t.Run("photo-media", func(t *testing.T) {
		t.Parallel()
		testSmokePhotoMedia(t)
	})
	t.Run("profile-photo-upload", func(t *testing.T) {
		t.Parallel()
		testSmokeProfilePhotoUpload(t)
	})
	t.Run("profile-bio", func(t *testing.T) {
		t.Parallel()
		testSmokeProfileBio(t)
	})
	t.Run("upload-media", func(t *testing.T) {
		t.Parallel()
		testSmokeUploadMedia(t)
	})
	t.Run("uploads-disabled", func(t *testing.T) {
		t.Parallel()
		testSmokeUploadsDisabled(t)
	})
	t.Run("upload-app-config-byte-limit", func(t *testing.T) {
		t.Parallel()
		testSmokeUploadAppConfigByteLimit(t)
	})
	t.Run("saved-gifs", func(t *testing.T) {
		t.Parallel()
		testSmokeSavedGIFs(t)
	})
	t.Run("saved-gifs-client-request-fixtures", func(t *testing.T) {
		t.Parallel()
		testSmokeSavedGIFRequestFixtures(t)
	})
	t.Run("album-media", func(t *testing.T) {
		t.Parallel()
		testSmokeAlbumMedia(t)
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
	t.Run("get-all-drafts", func(t *testing.T) {
		t.Parallel()
		testSmokeGetAllDrafts(t)
	})
	t.Run("manual-dialog-unread-mark", func(t *testing.T) {
		t.Parallel()
		testSmokeManualDialogUnreadMark(t)
	})
	t.Run("notify-settings", func(t *testing.T) {
		t.Parallel()
		testSmokeNotifySettings(t)
	})
	t.Run("notify-settings-recovery", func(t *testing.T) {
		t.Parallel()
		testSmokeNotifySettingsRecovery(t)
	})
	t.Run("notify-settings-reads", func(t *testing.T) {
		t.Parallel()
		testSmokeNotifySettingsHydratedReads(t)
	})
	t.Run("basic-group", func(t *testing.T) {
		t.Parallel()
		testSmokeBasicGroup(t)
	})
	t.Run("basic-group-reactions", func(t *testing.T) {
		t.Parallel()
		testSmokeBasicGroupReactions(t)
	})
	t.Run("channel-reactions", func(t *testing.T) {
		t.Parallel()
		testSmokeChannelReactions(t)
	})
	t.Run("available-reactions", func(t *testing.T) {
		t.Parallel()
		testSmokeAvailableReactions(t)
	})
	t.Run("basic-group-photo", func(t *testing.T) {
		t.Parallel()
		testSmokeBasicGroupPhoto(t)
	})
	t.Run("channel-edit-photo", func(t *testing.T) {
		t.Parallel()
		testChannelEditPhoto(t)
	})
	t.Run("delete-basic-group", func(t *testing.T) {
		t.Parallel()
		testSmokeDeleteBasicGroup(t)
	})
	t.Run("delete-history", func(t *testing.T) {
		t.Parallel()
		testDeleteHistoryKeepsDialogAndOnlyClearsCaller(t)
	})
	t.Run("channel", func(t *testing.T) {
		t.Parallel()
		testSmokeChannel(t)
	})
	t.Run("channel-delete", func(t *testing.T) {
		t.Parallel()
		testSmokeChannelDelete(t)
	})
	t.Run("channel-membership-revocation", func(t *testing.T) {
		t.Parallel()
		testSmokeChannelMembershipRevocation(t)
	})
	t.Run("mtproto-service-messages", func(t *testing.T) {
		t.Parallel()
		testSmokeMTProtoServiceMessages(t)
	})
	t.Run("channel-polls", func(t *testing.T) {
		testSmokeChannelPollLifecycle(t)
	})
	t.Run("peer-settings-bar-dismissal", func(t *testing.T) {
		t.Parallel()
		testSmokePeerSettingsBarDismissal(t)
	})
	t.Run("client-request-fixtures", func(t *testing.T) {
		// Keep this multi-client fixture out of concurrent smoke DB load.
		testSmokeClientRequestFixtures(t)
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
	t.Run("bot-user-profile", func(t *testing.T) {
		t.Parallel()
		testSmokeBotUserProfile(t)
	})
	t.Run("common-chats", func(t *testing.T) {
		t.Parallel()
		testSmokeCommonChats(t)
	})
	t.Run("langpack", func(t *testing.T) {
		t.Parallel()
		testSmokeLangpack(t)
	})
	t.Run("username-registration", func(t *testing.T) {
		t.Parallel()
		testSmokeUsernameRegistration(t)
	})
	t.Run("user-invites", func(t *testing.T) {
		t.Parallel()
		testSmokeUserInvites(t)
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
	t.Run("secret-chat-blocked-request", func(t *testing.T) {
		t.Parallel()
		testSmokeSecretChatBlockedRequest(t)
	})
	t.Run("secret-chat-blocked-message", func(t *testing.T) {
		t.Parallel()
		testSmokeSecretChatBlockedMessage(t)
	})
	t.Run("provisioned-account-login", func(t *testing.T) {
		testSmokeProvisionedAccountLogin(t)
	})
	t.Run("admin-proxy-login", func(t *testing.T) {
		testSmokeAdminProxyLogin(t)
	})
	t.Run("admin-sticker-takedown", func(t *testing.T) {
		testSmokeAdminStickerTakedown(t)
	})
}

func testSmokeOneToOne(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551046001", "+15551046002"
	seedSmokeUsers(t, f, phoneA, phoneB)

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

	const urlText = "smoke link 😀 https://t.me/+smoke"
	var urlResult tg.UpdatesClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		urlResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a1.id, b1.id), Message: urlText, RandomID: 1046004,
		})
		return err
	}); err != nil {
		t.Fatalf("send URL message: %v", err)
	}
	urlMessage, urlPts, ok := outgoingMessage(t, urlResult, urlText)
	if !ok || urlMessage.ID != 4 || urlPts != 5 {
		t.Fatalf("URL send = message %#v pts %d, want message id 4 pts 5", urlMessage, urlPts)
	}
	assertSmokeURLEntity(t, urlMessage, urlText, "https://t.me/+smoke")
	assertObservedMessage(t, f.ctx, a1.seen, urlText, 4, true, b1.id, 5, "A1 URL sender echo", true)
	assertObservedMessage(t, f.ctx, a2.seen, urlText, 4, true, b1.id, 5, "A2 URL sender echo", true)
	assertObservedMessage(t, f.ctx, b1.seen, urlText, 3, false, a1.id, 4, "B1 URL incoming", true)
	assertObservedMessage(t, f.ctx, b2.seen, urlText, 3, false, a1.id, 4, "B2 URL incoming", true)
	assertObservedMessage(t, f.ctx, a2.push, urlText, 4, true, b1.id, 5, "A2 URL sender push")
	assertObservedMessage(t, f.ctx, b1.push, urlText, 3, false, a1.id, 4, "B1 URL incoming push")
	assertObservedMessage(t, f.ctx, b2.push, urlText, 3, false, a1.id, 4, "B2 URL incoming push")

	wantA := map[string]smokeHistoryMessage{
		"a-to-b-smoke": {id: 2, out: true},
		"b-to-a-smoke": {id: 3, out: false},
		urlText:        {id: 4, out: true, url: "https://t.me/+smoke"},
	}
	wantB := map[string]smokeHistoryMessage{
		"a-to-b-smoke": {id: 1, out: false},
		"b-to-a-smoke": {id: 2, out: true},
		urlText:        {id: 3, out: false, url: "https://t.me/+smoke"},
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

func testSmokePeerDisconnect(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.persistStatus = true
	})
	const phoneA, phoneB = "+15551046101", "+15551046102"
	seedSmokeUsers(t, f, phoneA, phoneB)

	a1 := newSmokeClient(t, f, "peer disconnect A1", phoneA)
	a2 := newSmokeClient(t, f, "peer disconnect A2", phoneA)
	b1 := newSmokeClient(t, f, "peer disconnect B", phoneB)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, a1.id, 2, "peer disconnect A1", a1.lifecycle)

	seedText := "peer-disconnect-seed"
	var seedResult tg.UpdatesClass
	if err := a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		seedResult, err = api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a1.id, b1.id), Message: seedText, RandomID: time.Now().UnixNano(),
		})
		return err
	}); err != nil {
		t.Fatalf("[assert:peer-disconnect.seed-send] could not seed A to B dialog")
	}
	if _, _, ok := outgoingMessage(t, seedResult, seedText); !ok || countOutgoingMessages(seedResult, seedText) != 1 {
		t.Fatal("[assert:peer-disconnect.seed-result] seed send omitted its outgoing message")
	}

	beforeA, ok := snapshotPeerDisconnectOwner(f, a1.id, b1.id)
	if !ok {
		t.Fatal("[assert:peer-disconnect.initial-state] sender state snapshot unavailable")
	}
	beforeB, ok := snapshotPeerDisconnectOwner(f, b1.id, a1.id)
	if !ok {
		t.Fatal("[assert:peer-disconnect.initial-state] recipient state snapshot unavailable")
	}
	blockerConn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatal("[assert:peer-disconnect.owner-lock-open] could not open owner-lock blocker")
	}
	t.Cleanup(func() {
		if err := blockerConn.Close(context.Background()); err != nil {
			t.Errorf("close owner-lock blocker connection: %v", err)
		}
	})
	blocker, err := blockerConn.Begin(f.ctx)
	if err != nil {
		t.Fatal("[assert:peer-disconnect.owner-lock-open] could not begin owner-lock blocker")
	}
	blockerReleased := false
	t.Cleanup(func() {
		if blockerReleased {
			return
		}
		if err := blocker.Rollback(context.Background()); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("release owner-lock blocker during cleanup: %v", err)
		}
	})
	if _, err := blocker.Exec(f.ctx, `SELECT pg_advisory_xact_lock($1)`, a1.id); err != nil {
		t.Fatal("[assert:peer-disconnect.owner-lock-acquire] could not hold sender owner lock")
	}

	data, err := (&session.Loader{Storage: a1.session}).Load(f.ctx)
	if err != nil {
		t.Fatalf("load A1's authenticated session: %v", err)
	}
	if len(data.AuthKey) != len(crypto.Key{}) {
		t.Fatalf("A1 auth key length = %d, want %d", len(data.AuthKey), len(crypto.Key{}))
	}
	var rawKey crypto.Key
	copy(rawKey[:], data.AuthKey)
	key := rawKey.WithID()

	// Hold a setup RPC active so the bootstrap shutdown must clear its work.
	// A server-owned close preserves the peer allowance for the raw close below.
	setupSend := make(chan error, 1)
	go func() {
		setupSend <- a1.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			_, sendErr := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: peerUser(a1.id, b1.id), Message: "peer-disconnect-setup-held", RandomID: time.Now().UnixNano(),
			})
			return sendErr
		})
	}()
	if !waitForSmokeAdvisoryBlock(f.ctx, blocker) {
		t.Fatal("[assert:peer-disconnect.setup-send-block] bootstrap send did not block on sender owner lock")
	}
	var setupConns []*mtproto.Conn
	for _, conn := range f.registry.Conns(a1.id) {
		if conn.AuthKeyID() == mtproto.AuthKeyIDInt64(key.ID) {
			setupConns = append(setupConns, conn)
		}
	}
	if len(setupConns) == 0 {
		t.Fatal("[assert:peer-disconnect.setup-transport] bootstrap transport was absent from the registry")
	}
	a1.lifecycle.expectedRemoteClose.Store(true)
	a1.lifecycle.intentionalStop.Store(true)
	for _, conn := range setupConns {
		if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Fatal("[assert:peer-disconnect.setup-transport-close] could not close bootstrap transport from server")
		}
	}
	a1.stopClient(t)
	if err := recvOrCtx(t, f.ctx, setupSend, "bootstrap send cancellation"); err == nil {
		t.Fatal("[assert:peer-disconnect.setup-send-cancel] bootstrap send succeeded during shutdown")
	}
	if !waitForNoAdvisoryBlock(f.ctx, blocker) {
		t.Fatal("[assert:peer-disconnect.setup-waiter-clear] bootstrap DB waiter remained after shutdown")
	}
	if !waitForSmokeRegistryConnections(f.ctx, f.registry, a1.id, 1) {
		t.Fatal("[assert:peer-disconnect.same-user-online] closing A1's command client also closed A2")
	}

	// A single-frame raw A1 connection keeps a queued client frame from masking
	// the TCP close while the blocked send is active.
	sessionID, err := crypto.NewSessionID(crypto.DefaultRand())
	if err != nil {
		t.Fatalf("generate A1 test session ID: %v", err)
	}
	request := &tg.MessagesSendMessageRequest{
		Peer: peerUser(a1.id, b1.id), Message: "peer-disconnect-held", RandomID: time.Now().UnixNano(),
	}
	var body bin.Buffer
	if err := request.Encode(&body); err != nil {
		t.Fatalf("encode blocked A1 send: %v", err)
	}
	messageID := int64(proto.NewMessageID(time.Now(), proto.MessageFromClient))
	messageID -= messageID % 4
	frame := crypto.EncryptedMessageData{
		Salt: data.Salt, SessionID: sessionID, MessageID: messageID, SeqNo: 1,
		MessageDataLen:         int32(body.Len()), //nolint:gosec // the smoke request is bounded below MaxInt32
		MessageDataWithPadding: body.Copy(),
	}
	var encrypted bin.Buffer
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, frame, &encrypted); err != nil {
		t.Fatalf("encrypt blocked A1 send: %v", err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
	rawConn, err := (&net.Dialer{}).DialContext(f.ctx, "tcp", address)
	if err != nil {
		t.Fatalf("open A1's blocked-RPC transport: %v", err)
	}
	t.Cleanup(func() {
		if err := rawConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("close A1's blocked-RPC transport: %v", err)
		}
	})
	wire, err := transport.Abridged.Handshake(rawConn)
	if err != nil {
		t.Fatalf("handshake A1's blocked-RPC transport: %v", err)
	}
	blockedAt := time.Now()
	if err := wire.Send(f.ctx, &bin.Buffer{Buf: encrypted.Copy()}); err != nil {
		t.Fatalf("send blocked A1 RPC: %v", err)
	}
	if !waitForSmokeAdvisoryBlock(f.ctx, blocker) {
		t.Fatal("[assert:peer-disconnect.owner-lock-acquire] send did not block on sender owner lock")
	}
	if err := rawConn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("disconnect A1 during blocked send: %v", err)
	}
	if !waitForNoAdvisoryBlock(f.ctx, blocker) {
		t.Fatal("[assert:peer-disconnect.message-blocker-clear-confirm] DB waiter remained until blocker release")
	}
	if time.Since(blockedAt) >= mtproto.DefaultRPCDeadline {
		t.Fatal("[assert:peer-disconnect.message-blocker-clear-confirm] DB waiter outlasted the RPC deadline")
	}
	if err := blocker.Rollback(f.ctx); err != nil {
		t.Fatal("[assert:peer-disconnect.owner-lock-release] could not release blocker after waiter cleared")
	}
	blockerReleased = true

	if !waitForSmokeRegistryConnections(f.ctx, f.registry, a1.id, 1) {
		t.Fatal("[assert:peer-disconnect.same-user-online] A2 did not keep A online after A1 closed")
	}
	if !waitForSmokeUserStatus(f.ctx, f.store, a1.id, true) {
		t.Fatal("[assert:peer-disconnect.persisted-online] A was not persisted online while A2 remained connected")
	}
	if !peerDisconnectOwnerUnchanged(f, a1.id, b1.id, beforeA) {
		t.Fatal("[assert:peer-disconnect.no-message-or-pts] A message history, events, or pts changed")
	}
	if !peerDisconnectOwnerUnchanged(f, b1.id, a1.id, beforeB) {
		t.Fatal("[assert:peer-disconnect.no-message-or-pts] B message history, events, or pts changed")
	}

	if err := a2.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		authorizations, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(authorizations.Authorizations) == 0 {
			return errors.New("authorization request returned no sessions")
		}
		return nil
	}); err != nil {
		t.Fatalf("[assert:peer-disconnect.a2-authorization] A2 authorization request failed (%s)", safeErrorClass(err))
	}
	if !waitForSmokeRegistryConnections(f.ctx, f.registry, a1.id, 1) {
		t.Fatal("[assert:peer-disconnect.same-user-online] A went offline before its last connection closed")
	}
	a2.stopClient(t)
	if !waitForSmokeRegistryConnections(f.ctx, f.registry, a1.id, 0) {
		t.Fatal("[assert:peer-disconnect.last-session-close] A remained online after its last connection closed")
	}
	if !waitForSmokeUserStatus(f.ctx, f.store, a1.id, false) {
		t.Fatal("[assert:peer-disconnect.persisted-offline] A was not persisted offline after its last connection closed")
	}
	var offline *tg.UserStatusOffline
	for offline == nil {
		update := recvStatus(t, f.ctx, b1.push, "B offline status notification for A")
		if update.UserID != a1.id {
			continue
		}
		if status, ok := update.Status.(*tg.UserStatusOffline); ok {
			offline = status
		}
	}
	if offline.WasOnline <= 0 {
		t.Fatal("[assert:peer-disconnect.offline-notification] recipient received an offline status without a persisted last-seen timestamp")
	}
	b1.stopClient(t)
}

type peerDisconnectOwnerSnapshot struct {
	state   store.State
	events  []store.Event
	history []store.Message
}

func snapshotPeerDisconnectOwner(f *smokeFixture, ownerID, peerID int64) (peerDisconnectOwnerSnapshot, bool) {
	state, err := f.store.State(f.ctx, ownerID)
	if err != nil {
		return peerDisconnectOwnerSnapshot{}, false
	}
	events, err := f.store.EventsSince(f.ctx, ownerID, 0)
	if err != nil {
		return peerDisconnectOwnerSnapshot{}, false
	}
	history, err := f.store.History(f.ctx, ownerID, store.PeerTypeUser, peerID, 0, 100)
	if err != nil {
		return peerDisconnectOwnerSnapshot{}, false
	}
	return peerDisconnectOwnerSnapshot{state: state, events: events, history: history}, true
}

func peerDisconnectOwnerUnchanged(f *smokeFixture, ownerID, peerID int64, before peerDisconnectOwnerSnapshot) bool {
	after, ok := snapshotPeerDisconnectOwner(f, ownerID, peerID)
	return ok && reflect.DeepEqual(after, before)
}

func smokeAdvisoryBlockPresent(ctx context.Context, tx pgx.Tx) (bool, error) {
	// Activity snapshots persist for the blocker transaction. Refresh before
	// each probe so a canceled RPC's replacement backend is visible.
	if _, err := tx.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
		return false, err
	}
	var blocked bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM pg_stat_activity AS waiting
			CROSS JOIN LATERAL unnest(pg_blocking_pids(waiting.pid)) AS blockers(pid)
			WHERE blockers.pid = pg_backend_pid()
			  AND waiting.wait_event_type = 'Lock'
			  AND waiting.wait_event = 'advisory'
		)
	`).Scan(&blocked)
	return blocked, err
}

func waitForSmokeAdvisoryBlock(ctx context.Context, tx pgx.Tx) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		blocked, err := smokeAdvisoryBlockPresent(ctx, tx)
		if err != nil {
			return false
		}
		if blocked {
			return true
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func waitForNoAdvisoryBlock(ctx context.Context, tx pgx.Tx) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(mtproto.DefaultRPCDeadline - 3*time.Second)
	defer timeout.Stop()
	for {
		blocked, err := smokeAdvisoryBlockPresent(ctx, tx)
		if err != nil {
			return false
		}
		if !blocked {
			return true
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func waitForSmokeRegistryConnections(ctx context.Context, registry *mtproto.SessionRegistry, userID int64, want int) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for len(registry.Conns(userID)) != want {
		select {
		case <-ticker.C:
		case <-timeout.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func waitForSmokeUserStatus(ctx context.Context, st *store.Store, userID int64, online bool) bool {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		user, ok, err := st.UserByID(ctx, userID)
		if err == nil && ok && user.IsOnline == online {
			return true
		}
		select {
		case <-ticker.C:
		case <-timeout.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

func testSmokeResetAuthorizations(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551046003"
	seedSmokeUsers(t, f, phone)

	current := newSmokeClient(t, f, "reset-authorizations-current", phone)
	other := newSmokeClient(t, f, "reset-authorizations-other", phone)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, current.id, 2, "reset-authorizations", current.lifecycle)
	other.lifecycle.expectedRemoteClose.Store(true)

	if err := current.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AuthResetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("resetAuthorizations returned false")
		}
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(auths.Authorizations) != 1 || !auths.Authorizations[0].Current {
			return fmt.Errorf("authorizations after reset = %+v, want only the current session", auths.Authorizations)
		}
		return nil
	}); err != nil {
		t.Fatalf("terminate other sessions: %v", err)
	}
	if keys, err := f.store.AuthKeysByUser(f.ctx, current.id); err != nil {
		t.Fatalf("auth keys after reset: %v", err)
	} else if len(keys) != 1 {
		t.Fatalf("auth keys after reset = %d, want only the current session", len(keys))
	}
	other.stopClient(t)
}

func testSmokeAuthorizationDeviceMetadata(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551046030"
	seedSmokeUsers(t, f, phone)
	deviceModel := "Mac\x00Book" + string([]byte{0xff}) + strings.Repeat("X", 1<<20)
	systemVersion := strings.Repeat("a", 127) + "é"
	client := newSmokeClientWithDevice(t, f, "authorization-device-metadata", phone, telegram.DeviceConfig{
		DeviceModel:    deviceModel,
		SystemVersion:  systemVersion,
		AppVersion:     "6.1",
		LangPack:       "tdesktop",
		SystemLangCode: "en",
	})

	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if len(result.Authorizations) != 1 {
			return fmt.Errorf("getAuthorizations returned %d sessions, want 1", len(result.Authorizations))
		}
		auth := result.Authorizations[0]
		wantDeviceModel := "MacBook" + strings.Repeat("X", 121)
		if auth.DeviceModel != wantDeviceModel || auth.SystemVersion != strings.Repeat("a", 127) ||
			auth.AppVersion != "6.1" || auth.Platform != "tdesktop" || auth.APIID != 1 ||
			auth.AppName != "" || auth.IP != "" || auth.Country != "" || auth.Region != "" {
			return fmt.Errorf("authorization metadata = %+v, want sanitized labels and no session IP by default", auth)
		}
		return nil
	}); err != nil {
		t.Fatalf("get authorizations: %v", err)
	}
}

func testSmokeAuthorizationTTL(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551046004"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "authorization-ttl", phone)

	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.AccountSetAuthorizationTTL(ctx, 30)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("setAuthorizationTTL returned false")
		}
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 30 {
			return fmt.Errorf("getAuthorizations TTL = %d, want 30", auths.AuthorizationTTLDays)
		}
		return nil
	}); err != nil {
		t.Fatalf("set and read authorization TTL: %v", err)
	}

	f.restartWithFreshStore(t)
	t.Cleanup(func() { client.stopClient(t) })
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		auths, err := api.AccountGetAuthorizations(ctx)
		if err != nil {
			return err
		}
		if auths.AuthorizationTTLDays != 30 {
			return fmt.Errorf("getAuthorizations after server restart TTL = %d, want 30", auths.AuthorizationTTLDays)
		}
		return nil
	}); err != nil {
		t.Fatalf("read authorization TTL after server restart: %v", err)
	}
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
		if _, err := raw.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{OffsetPeer: &tg.InputPeerEmpty{}}); err != nil {
			t.Errorf("provisional messages.getDialogs error = %v, want success", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("provisional client run: %v", err)
	}
}

func testSmokeHelpCountries(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551040003"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "help countries", phone)
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.HelpGetCountriesList(ctx, &tg.HelpGetCountriesListRequest{LangCode: "", Hash: 0})
		if err != nil {
			return fmt.Errorf("help.getCountriesList: %w", err)
		}
		countries, ok := result.(*tg.HelpCountriesList)
		if !ok {
			return fmt.Errorf("help.getCountriesList = %T, want *tg.HelpCountriesList", result)
		}
		if len(countries.Countries) != 0 || countries.Hash == 0 {
			return fmt.Errorf("help.getCountriesList = %+v, want empty countries and a nonzero hash", countries)
		}
		return nil
	}); err != nil {
		t.Fatalf("country-list smoke request: %v", err)
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
	seedSmokeUsers(t, f, phone)
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
	seedSmokeUsers(t, f, phoneA, phoneB)
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

func testSmokeGetAllDrafts(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB = "+15551049131", "+15551049132"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "get all drafts owner", phoneA)
	peer := dialogPinUser(t, f, phoneB)
	seedDialogPinDM(t, f, a.id, peer.ID, 1049131)

	if err := saveCloudDraft(a, f.ctx, &tg.MessagesSaveDraftRequest{
		Peer: peerUser(a.id, peer.ID), Message: "persisted draft for fresh session",
	}); err != nil {
		t.Fatalf("save cloud draft: %v", err)
	}

	b := newSmokeClient(t, f, "get all drafts fresh session", phoneA)
	var got tg.UpdatesClass
	if err := b.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		var err error
		got, err = client.MessagesGetAllDrafts(ctx)
		return err
	}); err != nil {
		t.Fatalf("getAllDrafts in fresh session: %v", err)
	}
	updates, ok := got.(*tg.Updates)
	if !ok {
		t.Fatalf("getAllDrafts response = %T, want *tg.Updates", got)
	}
	if len(updates.Updates) != 1 {
		t.Fatalf("getAllDrafts returned %d updates, want one persisted draft", len(updates.Updates))
	}
	update, ok := updates.Updates[0].(*tg.UpdateDraftMessage)
	if !ok {
		t.Fatalf("getAllDrafts update = %T, want *tg.UpdateDraftMessage", updates.Updates[0])
	}
	assertCloudDraftUpdate(t, update, peer.ID, "persisted draft for fresh session", false, 0)
}

func testSmokeDefaultDialogFilter(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551049003"
	seedSmokeUsers(t, f, phone)
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
	seedSmokeUsers(t, f, phoneA, phoneB)
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
		{name: "photo video", filter: &tg.InputMessagesFilterPhotoVideo{}, want: []string{"private-shared-video-smoke"}},
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
		{name: "photo video", filter: &tg.InputMessagesFilterPhotoVideo{}, want: []string{"channel-shared-video-smoke"}},
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
	seedSmokeUsers(t, f, phone, otherPhone)
	client := newSmokeClient(t, f, "A1", phone)
	otherSessionReadiness := newDialogFilterReadiness()
	otherSession := newSmokeClientWithConnectionReadiness(t, f, "A2", phone, otherSessionReadiness)
	otherOwnerReadiness := newDialogFilterReadiness()
	otherOwner := newSmokeClientWithConnectionReadiness(t, f, "B1", otherPhone, otherOwnerReadiness)
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
		t.Fatalf("[assert:dialog-filters.app-config-fetch-error] help.getAppConfig: %v", err)
	}
	config, ok := appConfig.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("[assert:dialog-filters.app-config-object-type] app config = %T, want *tg.JSONObject", appConfig.Config)
	}
	filtersEnabled := false
	filtersEnabledFound := false
	for _, value := range config.Value {
		if value.Key != "dialog_filters_enabled" {
			continue
		}
		if filtersEnabledFound {
			t.Fatal("[assert:dialog-filters.app-config-duplicate-enabled] app config repeats dialog_filters_enabled")
		}
		enabled, ok := value.Value.(*tg.JSONBool)
		if !ok {
			t.Fatalf("[assert:dialog-filters.app-config-enabled-type] dialog_filters_enabled = %T, want *tg.JSONBool", value.Value)
		}
		filtersEnabled = enabled.Value
		filtersEnabledFound = true
	}
	if !filtersEnabledFound || !filtersEnabled {
		t.Fatal("[assert:dialog-filters.app-config-enabled] dialog_filters_enabled is missing or false")
	}
	var seeded *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		seeded, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.defaults-initialize-error] initialize default folders: %v", err)
	}
	if len(seeded.Filters) != 5 {
		t.Fatalf("[assert:dialog-filters.initial-default-count] %s folders = %d, want All chats and four defaults", "initial", len(seeded.Filters))
	}
	if _, ok := seeded.Filters[0].(*tg.DialogFilterDefault); !ok {
		t.Fatalf("[assert:dialog-filters.initial-default-all-type] %s first folder = %T, want All chats", "initial", seeded.Filters[0])
	}
	wantInitialDefaultOrder := []struct {
		id    int
		title string
	}{{2, "Personal"}, {3, "Channels"}, {4, "Groups"}, {5, "Unread"}}
	for i, expected := range wantInitialDefaultOrder {
		folder, ok := seeded.Filters[i+1].(*tg.DialogFilter)
		if !ok || folder.ID != expected.id || folder.Title.Text != expected.title {
			t.Fatalf("[assert:dialog-filters.initial-default-order] %s folder %d = %#v, want ID %d %s", "initial", i+1, seeded.Filters[i+1], expected.id, expected.title)
		}
	}
	var repeated *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		repeated, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.repeat-default-read-error] repeat default folder read: %v", err)
	}
	if len(repeated.Filters) != 5 {
		t.Fatalf("[assert:dialog-filters.repeat-default-count] %s folders = %d, want All chats and four defaults", "repeated", len(repeated.Filters))
	}
	if _, ok := repeated.Filters[0].(*tg.DialogFilterDefault); !ok {
		t.Fatalf("[assert:dialog-filters.repeat-default-all-type] %s first folder = %T, want All chats", "repeated", repeated.Filters[0])
	}
	wantRepeatedDefaultOrder := []struct {
		id    int
		title string
	}{{2, "Personal"}, {3, "Channels"}, {4, "Groups"}, {5, "Unread"}}
	for i, expected := range wantRepeatedDefaultOrder {
		folder, ok := repeated.Filters[i+1].(*tg.DialogFilter)
		if !ok || folder.ID != expected.id || folder.Title.Text != expected.title {
			t.Fatalf("[assert:dialog-filters.repeat-default-order] %s folder %d = %#v, want ID %d %s", "repeated", i+1, repeated.Filters[i+1], expected.id, expected.title)
		}
	}

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
		t.Fatalf("[assert:dialog-filters.create-error] create dialog filter: %v", err)
	}
	var listed *tg.MessagesDialogFilters
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.created-list-error] list dialog filters: %v", err)
	}
	if listed.TagsEnabled || len(listed.Filters) != 6 {
		t.Fatalf("[assert:dialog-filters.created-count-tags] filters = %d, tags enabled = %v, want All chats, four defaults and ID 6", len(listed.Filters), listed.TagsEnabled)
	}
	custom, ok := listed.Filters[5].(*tg.DialogFilter)
	if !ok || custom.ID != 6 || custom.Title.Text != "Groups" || !custom.Groups {
		t.Fatalf("[assert:dialog-filters.created-value] listed custom filter = %#v, want ID 6 Groups", listed.Filters[5])
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
		t.Fatalf("[assert:dialog-filters.edit-error] edit dialog filter: %v", err)
	}
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.edited-list-error] list edited dialog filters: %v", err)
	}
	custom, ok = listed.Filters[5].(*tg.DialogFilter)
	if !ok || custom.ID != 6 || custom.Title.Text != "Bots" || !custom.Bots || custom.Groups {
		t.Fatalf("[assert:dialog-filters.edited-value] edited custom filter = %#v, want ID 6 Bots", listed.Filters[5])
	}
	var isolated *tg.MessagesDialogFilters
	if err := otherOwner.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		isolated, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.other-owner-list-error] other owner get dialog filters: %v", err)
	}
	if isolated.TagsEnabled || len(isolated.Filters) != 5 {
		t.Fatalf("[assert:dialog-filters.other-owner-count-tags] other owner saw %d folders with tags enabled=%v, want All chats and four defaults", len(isolated.Filters), isolated.TagsEnabled)
	}
	if _, ok := isolated.Filters[0].(*tg.DialogFilterDefault); !ok {
		t.Fatalf("[assert:dialog-filters.other-owner-all-type] other owner's first folder = %T, want All chats", isolated.Filters[0])
	}
	if personal, ok := isolated.Filters[1].(*tg.DialogFilter); !ok || personal.Title.Text != "Personal" {
		t.Fatalf("[assert:dialog-filters.other-owner-personal] other owner's second folder = %#v, want Personal", isolated.Filters[1])
	}

	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		ok, err := api.MessagesUpdateDialogFiltersOrder(ctx, []int{6, 0})
		if err == nil && !ok {
			return errors.New("folder reorder returned false")
		}
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.reorder-error] reorder dialog filters: %v", err)
	}
	if err := client.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.reordered-list-error] list reordered dialog filters: %v", err)
	}
	if folder, ok := listed.Filters[0].(*tg.DialogFilter); !ok || folder.ID != 6 {
		t.Fatalf("[assert:dialog-filters.reordered-first] first reordered filter = %#v, want ID 6", listed.Filters[0])
	}
	otherSessionMark := otherSessionReadiness.mark()
	otherOwnerMark := otherOwnerReadiness.mark()
	f.restart(t)
	otherSessionReadStarted := time.Now()
	otherSessionReadinessFailure, otherSessionErr := otherSessionReadiness.callAfterFreshReady(f.ctx, otherSessionMark, otherSession.lifecycle.result, func() error {
		return otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			listed, err = api.MessagesGetDialogFilters(ctx)
			return err
		})
	})
	if otherSessionReadinessFailure != nil {
		t.Fatalf("[assert:dialog-filters.other-session-restart-ready] other authorized session did not become ready before restart read: %s", otherSession.lifecycle.diagnostic("replacement connection readiness", time.Since(otherSessionReadStarted), otherSessionReadinessFailure.diagnostic()))
	}
	if otherSessionErr != nil {
		t.Fatalf("[assert:dialog-filters.other-session-restart-read-error] other authorized session get edited dialog filters after restart: %v", otherSessionErr)
	}
	if len(listed.Filters) != 6 {
		t.Fatalf("[assert:dialog-filters.other-session-restart-count] other session saw %d filters after restart, want six", len(listed.Filters))
	}
	persisted, ok := listed.Filters[0].(*tg.DialogFilter)
	if !ok || persisted.ID != 6 || persisted.Title.Text != "Bots" || !persisted.Bots || persisted.Groups {
		t.Fatalf("[assert:dialog-filters.other-session-restart-value] persisted edited filter = %#v, want ID 6 Bots", listed.Filters[0])
	}
	otherOwnerReadStarted := time.Now()
	otherOwnerReadinessFailure, otherOwnerErr := otherOwnerReadiness.callAfterFreshReady(f.ctx, otherOwnerMark, otherOwner.lifecycle.result, func() error {
		return otherOwner.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			isolated, err = api.MessagesGetDialogFilters(ctx)
			return err
		})
	})
	if otherOwnerReadinessFailure != nil {
		t.Fatalf("[assert:dialog-filters.other-owner-restart-ready] other owner did not become ready before restart read: %s", otherOwner.lifecycle.diagnostic("replacement connection readiness", time.Since(otherOwnerReadStarted), otherOwnerReadinessFailure.diagnostic()))
	}
	if otherOwnerErr != nil {
		t.Fatalf("[assert:dialog-filters.other-owner-restart-read-error] other owner get dialog filters after restart: %v", otherOwnerErr)
	}
	if len(isolated.Filters) != 5 {
		t.Fatalf("[assert:dialog-filters.other-owner-restart-count] other owner saw %d folders after restart, want All chats and four defaults", len(isolated.Filters))
	}
	if personal, ok := isolated.Filters[1].(*tg.DialogFilter); !ok || personal.Title.Text != "Personal" {
		t.Fatalf("[assert:dialog-filters.other-owner-restart-personal] other owner's persisted second folder = %#v, want Personal", isolated.Filters[1])
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
		t.Fatalf("[assert:dialog-filters.delete-error] delete dialog filter: %v", err)
	}
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		listed, err = api.MessagesGetDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.deleted-list-error] list dialog filters after delete: %v", err)
	}
	if len(listed.Filters) != 5 {
		t.Fatalf("[assert:dialog-filters.deleted-count] filters after delete = %d, want All chats and four defaults", len(listed.Filters))
	}
	var suggested []tg.DialogFilterSuggested
	if err := otherSession.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		suggested, err = api.MessagesGetSuggestedDialogFilters(ctx)
		return err
	}); err != nil {
		t.Fatalf("[assert:dialog-filters.suggested-list-error] get suggested dialog filters: %v", err)
	}
	if len(suggested) != 0 {
		t.Fatalf("[assert:dialog-filters.suggested-count] suggested filters = %d, want none because all four default titles remain", len(suggested))
	}
}

func testSmokeBasicGroup(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551047001", "+15551047002", "+15551047003"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
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
	var chatVersion int
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
		chatVersion = chat.Version
		return nil
	}); err != nil {
		t.Fatalf("create smoke group: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		updated, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: "Weekend plans",
		})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("editChatAbout returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("set smoke group about: %v", err)
	}
	chatVersion = waitSmokeChatRefresh(t, f.ctx, a, chatID, chatVersion)
	for _, member := range []*smokeClient{b, c} {
		waitSmokeChatRefresh(t, f.ctx, member, chatID, chatVersion-1)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		full, err := api.MessagesGetFullChat(ctx, chatID)
		if err != nil {
			return err
		}
		info, ok := full.FullChat.(*tg.ChatFull)
		if !ok || info.About != "Weekend plans" {
			return fmt.Errorf("member getFullChat about = %T/%q, want Weekend plans", full.FullChat, info.GetAbout())
		}
		return nil
	}); err != nil {
		t.Fatalf("member read smoke group about: %v", err)
	}
	assertChannelRPCError(t, f.ctx, a.cmds, "CHAT_ABOUT_NOT_MODIFIED", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: "Weekend plans",
		})
		return err
	})
	assertChannelRPCError(t, f.ctx, a.cmds, "CHAT_ABOUT_TOO_LONG", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: strings.Repeat("x", 256),
		})
		return err
	})
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatDefaultBannedRights(ctx, &tg.MessagesEditChatDefaultBannedRightsRequest{
			Peer:         &tg.InputPeerChat{ChatID: chatID},
			BannedRights: tg.ChatBannedRights{ChangeInfo: true},
		})
		return err
	}); err != nil {
		t.Fatalf("ban group info changes: %v", err)
	}
	assertChannelRPCError(t, f.ctx, b.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: "Member update",
		})
		return err
	})
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatDefaultBannedRights(ctx, &tg.MessagesEditChatDefaultBannedRightsRequest{
			Peer:         &tg.InputPeerChat{ChatID: chatID},
			BannedRights: tg.ChatBannedRights{},
		})
		return err
	}); err != nil {
		t.Fatalf("restore smoke group info changes: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		updated, err := api.MessagesEditChatAbout(ctx, &tg.MessagesEditChatAboutRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, About: "",
		})
		if err != nil {
			return err
		}
		if !updated {
			return errors.New("clear editChatAbout returned false")
		}
		return nil
	}); err != nil {
		t.Fatalf("clear smoke group about: %v", err)
	}
	for _, member := range []*smokeClient{a, b, c} {
		waitSmokeChatRefresh(t, f.ctx, member, chatID, chatVersion)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		full, err := api.MessagesGetFullChat(ctx, chatID)
		if err != nil {
			return err
		}
		info, ok := full.FullChat.(*tg.ChatFull)
		if !ok || info.About != "" {
			return fmt.Errorf("member getFullChat after clear about = %T/%q, want empty", full.FullChat, info.GetAbout())
		}
		return nil
	}); err != nil {
		t.Fatalf("member read cleared smoke group about: %v", err)
	}
	// Setting and restoring default rights, and clearing the description, each
	// advance the basic group's peer version before the admin changes below.
	chatVersion += 3
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
	}{{true, chatVersion + 1}, {false, chatVersion + 2}} {
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
	var readParticipants []tg.ReadParticipantDate
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		readParticipants, err = api.MessagesGetMessageReadParticipants(ctx, &tg.MessagesGetMessageReadParticipantsRequest{
			Peer:  &tg.InputPeerChat{ChatID: chatID},
			MsgID: senderMessageID,
		})
		return err
	}); err != nil {
		t.Fatalf("C get group message read participants before A reads: %v", err)
	}
	if len(readParticipants) != 0 {
		t.Fatalf("C group message readers before A reads = %v, want empty", readParticipants)
	}
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
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		readParticipants, err = api.MessagesGetMessageReadParticipants(ctx, &tg.MessagesGetMessageReadParticipantsRequest{
			Peer:  &tg.InputPeerChat{ChatID: chatID},
			MsgID: senderMessageID,
		})
		return err
	}); err != nil {
		t.Fatalf("C get group message read participants after A reads: %v", err)
	}
	if len(readParticipants) != 1 || readParticipants[0].UserID != a.id || readParticipants[0].Date <= 0 {
		t.Fatalf("C group message readers after A reads = %v, want A %d with persisted date", readParticipants, a.id)
	}

	const answerURL = "https://poll-answer.invalid/example?a=1"
	pollInput := &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: "Which smoke option?"},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "First"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Second"}},
		},
	}}
	firstInputAnswer, ok := pollInput.Poll.Answers[0].(*tg.InputPollAnswer)
	if !ok {
		t.Fatal("constructed group poll answer is not inputPollAnswer")
	}
	firstInputAnswer.SetMedia(&tg.InputMediaWebPage{URL: answerURL})
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
	answerPageID := assertSmokePollAnswerWebPage(t, "group poll send response", pollMessage, answerURL)
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
		if got := assertSmokePollAnswerWebPage(t, fmt.Sprintf("member %d live poll", received.FromID), received, answerURL); got != answerPageID {
			t.Fatalf("member %d live webpage id = %d, want %d", received.FromID, got, answerPageID)
		}
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
				if got := assertSmokePollAnswerWebPage(t, "group poll history", message, answerURL); got != answerPageID {
					return fmt.Errorf("group poll history webpage id = %d, want %d", got, answerPageID)
				}
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
		if got := assertSmokePollAnswerWebPage(t, "group poll getMessages", message, answerURL); got != answerPageID {
			return fmt.Errorf("group poll getMessages webpage id = %d, want %d", got, answerPageID)
		}
		return nil
	}); err != nil {
		t.Fatalf("read described group poll: %v", err)
	}

	invalidAnswerMedia := func(media tg.InputMediaClass) *tg.InputMediaPoll {
		invalid := &tg.InputMediaPoll{Poll: tg.Poll{
			Question: tg.TextWithEntities{Text: "Invalid answer media?"},
			Answers: []tg.PollAnswerClass{
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "First"}},
				&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "Second"}},
			},
		}}
		answer, ok := invalid.Poll.Answers[0].(*tg.InputPollAnswer)
		if !ok {
			t.Fatal("constructed invalid poll answer is not inputPollAnswer")
		}
		answer.SetMedia(media)
		return invalid
	}
	oversizedURL := "https://x/" + strings.Repeat("a", 2049-len("https://x/"))
	for i, test := range []struct {
		name  string
		media tg.InputMediaClass
		url   string
	}{
		{name: "inputMediaPhoto", media: &tg.InputMediaPhoto{ID: &tg.InputPhotoEmpty{}}},
		{name: "javascript URL", media: &tg.InputMediaWebPage{URL: "javascript:alert(1)"}, url: "javascript:alert(1)"},
		{name: "oversized URL", media: &tg.InputMediaWebPage{URL: oversizedURL}, url: oversizedURL},
	} {
		err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			_, sendErr := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Media: invalidAnswerMedia(test.media), RandomID: int64(1047006 + i),
			})
			if sendErr == nil {
				return fmt.Errorf("send poll with %s succeeded, want POLL_ANSWERS_INVALID", test.name)
			}
			if !tgerr.Is(sendErr, "POLL_ANSWERS_INVALID") {
				return fmt.Errorf("send poll with %s: %w", test.name, sendErr)
			}
			if test.url != "" && strings.Contains(sendErr.Error(), test.url) {
				return fmt.Errorf("send poll with %s echoed its URL in the error", test.name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("reject poll answer %s: %v", test.name, err)
		}
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
	assertSmokePinResult(t, happyUnpinResult, false, chatID, []int{happyMessageID})
	assertSmokePinnedUpdate(t, recvOrCtx(t, f.ctx, b.push.pinnedMsg, "B happy-path unpin push"), false, chatID, []int{happyLocalIDs[b.id]})
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
	// Switching callers leaves their earlier self-notifications in the raw
	// collector. Match the newly committed event by the recipient's pts.
	assertLatestPinPush := func(member *smokeClient, pinned bool, id int) {
		t.Helper()
		state, err := f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatal(err)
		}
		for {
			update := recvOrCtx(t, f.ctx, member.push.pinnedMsg, "latest group pin push")
			if update.Pts < state.Pts {
				continue
			}
			if update.Pts != state.Pts {
				t.Fatalf("pin push pts = %d, want committed pts %d", update.Pts, state.Pts)
			}
			assertSmokePinnedUpdate(t, update, pinned, chatID, []int{id})
			return
		}
	}
	// A member selects its own local ID; the creator must receive its copy.
	var mismatchPinResult tg.UpdatesClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		mismatchPinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: ownerMessageIDs[b.id],
		})
		return err
	}); err != nil {
		t.Fatalf("pin mismatched-ID group message: %v", err)
	}
	assertSmokePinResult(t, mismatchPinResult, true, chatID, []int{ownerMessageIDs[b.id]})
	assertLatestPinPush(a, true, ownerMessageIDs[a.id])
	for _, member := range []*smokeClient{a, b} {
		wantID := ownerMessageIDs[member.id]
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeGroupPin(ctx, api, chatID, wantID, mismatchText)
		}); err != nil {
			t.Fatalf("reopen mismatched-ID pinned group for member %d: %v", member.id, err)
		}
	}
	var mismatchUnpinResult tg.UpdatesClass
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		mismatchUnpinResult, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: ownerMessageIDs[b.id], Unpin: true,
		})
		return err
	}); err != nil {
		t.Fatalf("unpin mismatched-ID group message: %v", err)
	}
	assertSmokePinResult(t, mismatchUnpinResult, false, chatID, []int{ownerMessageIDs[b.id]})
	assertLatestPinPush(a, false, ownerMessageIDs[a.id])
	for _, member := range []*smokeClient{a, b} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeGroupUnpinned(ctx, api, chatID)
		}); err != nil {
			t.Fatalf("reopen unpinned mismatched-ID group for member %d: %v", member.id, err)
		}
	}

	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, ID: happyMessageID,
		})
		return err
	}); err != nil {
		t.Fatalf("re-pin group message before unpin all: %v", err)
	}
	assertLatestPinPush(b, true, happyLocalIDs[b.id])
	var unpinAll *tg.MessagesAffectedHistory
	var unpinAllState *tg.UpdatesState
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		if unpinAll, err = api.MessagesUnpinAllMessages(ctx, &tg.MessagesUnpinAllMessagesRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID},
		}); err != nil {
			return err
		}
		unpinAllState, err = api.UpdatesGetState(ctx)
		return err
	}); err != nil {
		t.Fatalf("unpin all group messages: %v", err)
	}
	if unpinAll.Pts != unpinAllState.Pts || unpinAll.PtsCount != 1 {
		t.Fatalf("unpin all result = %+v, want pts %d and pts_count 1", unpinAll, unpinAllState.Pts)
	}
	assertLatestPinPush(a, false, happyLocalIDs[a.id])
	for _, member := range []*smokeClient{a, b} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeGroupUnpinned(ctx, api, chatID)
		}); err != nil {
			t.Fatalf("reopen group after unpin all for member %d: %v", member.id, err)
		}
	}

	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesEditChatTitle(ctx, &tg.MessagesEditChatTitleRequest{ChatID: chatID, Title: "Member renamed group"})
		return err
	}); err != nil {
		t.Fatalf("member rename: %v", err)
	}
	for _, member := range []*smokeClient{a, b} {
		env, err := member.seen.waitService(f.ctx, &tg.MessageActionChatEditTitle{})
		if err != nil {
			t.Fatalf("member %d rename service: %v", member.id, err)
		}
		action, ok := env.svc.Action.(*tg.MessageActionChatEditTitle)
		if !ok || action.Title != "Member renamed group" {
			t.Fatalf("member %d rename action = %#v", member.id, env.svc.Action)
		}
	}
}

func testSmokeDeleteBasicGroup(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithDeadline(t, config.RegistrationClosed, nil, 180*time.Second)
	const phoneA, phoneB, phoneC, phoneD, phoneE = "+15551047401", "+15551047402", "+15551047403", "+15551047404", "+15551047405"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC, phoneD, phoneE)
	requestsA, requestsB, requestsC := &clientRequestRecorder{}, &clientRequestRecorder{}, &clientRequestRecorder{}
	a, b, c := newSmokeClientWithRequests(t, f, "Delete group A", phoneA, requestsA), newSmokeClientWithRequests(t, f, "Delete group B", phoneB, requestsB), newSmokeClientWithRequests(t, f, "Delete group C", phoneC, requestsC)
	d, e := newSmokeClient(t, f, "Delete group former member", phoneD), newSmokeClient(t, f, "Delete group stranger", phoneE)

	var chatID int64
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		created, err := api.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{
			Title: "Delete smoke group",
			Users: []tg.InputUserClass{inputUser(a.id, b.id), inputUser(a.id, c.id)},
		})
		if err != nil {
			return err
		}
		updates, ok := created.Updates.(*tg.Updates)
		if !ok || len(updates.Chats) != 1 {
			return fmt.Errorf("createChat result = %T with %d chats, want Updates with one chat", created.Updates, len(updates.Chats))
		}
		chat, ok := updates.Chats[0].(*tg.Chat)
		if !ok {
			return fmt.Errorf("created chat = %T, want *tg.Chat", updates.Chats[0])
		}
		chatID = chat.ID
		return nil
	}); err != nil {
		t.Fatalf("create delete smoke group: %v", err)
	}

	assertChannelRPCError(t, f.ctx, b.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteChat(ctx, chatID)
		return err
	})
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesAddChatUser(ctx, &tg.MessagesAddChatUserRequest{
			ChatID: chatID, UserID: inputUser(a.id, d.id),
		})
		return err
	}); err != nil {
		t.Fatalf("creator add former member before deletion: %v", err)
	}
	if err := d.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "former member copy", RandomID: 1047400,
		})
		return err
	}); err != nil {
		t.Fatalf("former member send before removal: %v", err)
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
			ChatID: chatID, UserID: inputUser(a.id, d.id),
		})
		return err
	}); err != nil {
		t.Fatalf("creator remove former member before deletion: %v", err)
	}
	chatBeforeRefusals, found, err := f.store.ChatByID(f.ctx, chatID)
	if err != nil || !found {
		t.Fatalf("chat before refusal checks = %+v, found=%v err=%v", chatBeforeRefusals, found, err)
	}
	statesBeforeRefusals := make(map[int64]store.State, 5)
	for _, member := range []*smokeClient{a, b, c, d, e} {
		statesBeforeRefusals[member.id], err = f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d state before delete refusals: %v", member.id, err)
		}
	}
	assertChannelRPCError(t, f.ctx, d.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteChat(ctx, chatID)
		return err
	})
	assertChannelRPCError(t, f.ctx, e.cmds, "CHAT_ADMIN_REQUIRED", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteChat(ctx, chatID)
		return err
	})
	chatAfterRefusals, found, err := f.store.ChatByID(f.ctx, chatID)
	if err != nil || !found || chatAfterRefusals.Version != chatBeforeRefusals.Version {
		t.Fatalf("chat changed after delete refusals: before=%+v after=%+v found=%v err=%v", chatBeforeRefusals, chatAfterRefusals, found, err)
	}
	participantsAfterRefusals, err := f.store.Participants(f.ctx, chatID)
	if err != nil || len(participantsAfterRefusals) != 3 {
		t.Fatalf("participants after removed-member/stranger refusals = %v, err=%v; want three current members", participantsAfterRefusals, err)
	}
	for _, member := range []*smokeClient{a, b, c, d, e} {
		state, err := f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d state after delete refusals: %v", member.id, err)
		}
		if state.Pts != statesBeforeRefusals[member.id].Pts {
			t.Errorf("member %d pts after delete refusal = %d, want unchanged %d", member.id, state.Pts, statesBeforeRefusals[member.id].Pts)
		}
	}

	for i, member := range []*smokeClient{a, b, c} {
		text := fmt.Sprintf("delete group message %d", i+1)
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Message: text, RandomID: 1047401 + int64(i),
			})
			return err
		}); err != nil {
			t.Fatalf("member %d send before deletion: %v", member.id, err)
		}
	}

	messageIDs := make(map[int64]map[int]bool, 3)
	for _, member := range []*smokeClient{a, b, c} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 100,
			})
			if err != nil {
				return err
			}
			history, ok := result.(*tg.MessagesMessages)
			if !ok {
				return fmt.Errorf("getHistory before deletion = %T, want *tg.MessagesMessages", result)
			}
			messageIDs[member.id] = make(map[int]bool, len(history.Messages))
			for _, message := range history.Messages {
				messageIDs[member.id][message.GetID()] = true
			}
			if len(messageIDs[member.id]) < 4 {
				return fmt.Errorf("member %d history has %d messages, want create/add notices and three messages", member.id, len(messageIDs[member.id]))
			}
			return nil
		}); err != nil {
			t.Fatalf("read member %d history before deletion: %v", member.id, err)
		}
	}

	type deleteEnvelope struct {
		ownerID int64
		ids     []int
		pts     int
		count   int
	}
	ptsBeforeDelete := make(map[int64]int, 3)
	for _, member := range []*smokeClient{a, b, c} {
		state, err := f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d pts before chat deletion: %v", member.id, err)
		}
		ptsBeforeDelete[member.id] = state.Pts
	}
	deleteUpdates := make(chan deleteEnvelope, 64)
	drainCtx, stopDraining := context.WithCancel(f.ctx)
	var drainers sync.WaitGroup
	for _, member := range []*smokeClient{a, b, c} {
		drainers.Go(func() {
			for {
				select {
				case update := <-member.seen.delMsgPts:
					deleteUpdates <- deleteEnvelope{ownerID: member.id, ids: update.Messages, pts: update.Pts, count: update.PtsCount}
				case <-drainCtx.Done():
					return
				}
			}
		})
	}
	defer func() {
		stopDraining()
		drainers.Wait()
	}()
	chatBeforeDelete, found, err := f.store.ChatByID(f.ctx, chatID)
	if err != nil || !found {
		t.Fatalf("chat before deletion = %+v, found=%v err=%v", chatBeforeDelete, found, err)
	}
	getDialogsBeforeRemoval := map[int64]int{
		a.id: requestsA.countMethod("messages.getDialogs"),
		b.id: requestsB.countMethod("messages.getDialogs"),
		c.id: requestsC.countMethod("messages.getDialogs"),
	}

	var deleted bool
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		deleted, err = api.MessagesDeleteChat(ctx, chatID)
		return err
	}); err != nil {
		t.Fatalf("creator deleteChat: %v", err)
	}
	if !deleted {
		t.Fatal("creator deleteChat returned false")
	}

	pending := make(map[int64]map[int]bool, len(messageIDs))
	receivedDeleteUpdates := make(map[int64][]deleteEnvelope, len(messageIDs))
	for ownerID, ids := range messageIDs {
		pending[ownerID] = make(map[int]bool, len(ids))
		for id := range ids {
			pending[ownerID][id] = true
		}
	}
	for len(pending) > 0 {
		var event deleteEnvelope
		select {
		case event = <-deleteUpdates:
		case <-f.ctx.Done():
			t.Fatalf("timed out with deleteChat message copies still pending: %v", pending)
		}
		receivedDeleteUpdates[event.ownerID] = append(receivedDeleteUpdates[event.ownerID], event)
		for _, id := range event.ids {
			delete(pending[event.ownerID], id)
		}
		if len(pending[event.ownerID]) == 0 {
			delete(pending, event.ownerID)
		}
	}

	ptsAfterDelete := make(map[int64]int, 3)
	for _, member := range []*smokeClient{a, b, c} {
		state, err := f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d pts after chat deletion: %v", member.id, err)
		}
		ptsAfterDelete[member.id] = state.Pts
		updates := receivedDeleteUpdates[member.id]
		sort.Slice(updates, func(i, j int) bool { return updates[i].pts < updates[j].pts })
		wantPts := ptsBeforeDelete[member.id]
		for _, update := range updates {
			if update.count <= 0 {
				t.Errorf("member %d delete update has pts_count %d: %+v", member.id, update.count, update)
				continue
			}
			wantPts += update.count
			if update.pts != wantPts {
				t.Errorf("member %d deletion pts jumped to %d, want contiguous pts %d", member.id, update.pts, wantPts)
			}
		}
		if state.Pts != wantPts {
			t.Errorf("member %d state pts after deletion = %d, want last represented delete pts %d", member.id, state.Pts, wantPts)
		}
	}

	for _, member := range []*smokeClient{a, b, c} {
		var event chatParticipantDeleteEnvelope
		select {
		case event = <-member.seen.chatRemoval:
		case <-f.ctx.Done():
			t.Fatalf("timed out waiting for member %d chat-removal update", member.id)
		}
		if event.Update.ChatID != chatID || event.Update.UserID != member.id || event.Update.Version != chatBeforeDelete.Version+1 {
			t.Errorf("member %d chat-removal update = %+v, want chat/user/version %d/%d/%d", member.id, event.Update, chatID, member.id, chatBeforeDelete.Version+1)
		}
		var forbidden bool
		for _, chat := range event.Chats {
			if got, ok := chat.(*tg.ChatForbidden); ok && got.ID == chatID {
				forbidden = true
			}
		}
		if !forbidden {
			t.Errorf("member %d chat-removal update references = %+v, want ChatForbidden %d", member.id, event.Chats, chatID)
		}
		if got := member.requests.countMethod("messages.getDialogs"); got != getDialogsBeforeRemoval[member.id] {
			t.Errorf("member %d made %d getDialogs RPCs before receiving chat removal; want unchanged count %d", member.id, got, getDialogsBeforeRemoval[member.id])
		}
	}

	for _, member := range []*smokeClient{a, b, c} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
				OffsetPeer: &tg.InputPeerEmpty{}, Limit: 100,
			})
			if err != nil {
				return err
			}
			var dialogs []tg.DialogClass
			switch page := result.(type) {
			case *tg.MessagesDialogs:
				dialogs = page.Dialogs
			case *tg.MessagesDialogsSlice:
				dialogs = page.Dialogs
			default:
				return fmt.Errorf("getDialogs after deletion = %T, want dialogs result", result)
			}
			for _, item := range dialogs {
				dialog, ok := item.(*tg.Dialog)
				if !ok {
					continue
				}
				peer, ok := dialog.Peer.(*tg.PeerChat)
				if ok && peer.ChatID == chatID {
					return fmt.Errorf("member %d still has deleted chat %d in getDialogs", member.id, chatID)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("member %d getDialogs after deletion: %v", member.id, err)
		}
		if got := member.requests.countMethod("messages.getDialogs"); got != getDialogsBeforeRemoval[member.id]+1 {
			t.Errorf("member %d getDialogs RPC count = %d, want exactly one RPC after receiving removal", member.id, got)
		}
	}

	differenceCallsBeforeNextMessage := requestsB.countMethod("updates.getDifference")
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: peerUser(a.id, b.id), Message: "post-delete pts continuity", RandomID: 1047498,
		})
		return err
	}); err != nil {
		t.Fatalf("send direct message after chat deletion: %v", err)
	}
	var direct messagePtsUpdate
	for direct.Message == nil {
		select {
		case update := <-b.seen.newMsgPts:
			if update.Message.Message == "post-delete pts continuity" {
				direct = update
			}
		case <-f.ctx.Done():
			t.Fatalf("timed out waiting for post-delete direct message update: %v", f.ctx.Err())
		}
	}
	if direct.PtsCount != 1 || direct.Pts != ptsAfterDelete[b.id]+1 {
		t.Errorf("post-delete direct pts = %d/%d, want contiguous %d/1", direct.Pts, direct.PtsCount, ptsAfterDelete[b.id]+1)
	}
	if got := requestsB.countMethod("updates.getDifference"); got != differenceCallsBeforeNextMessage {
		t.Errorf("member %d invoked updates.getDifference after contiguous post-delete message: before=%d after=%d", b.id, differenceCallsBeforeNextMessage, got)
	}

	assertChannelRPCError(t, f.ctx, b.cmds, "PEER_ID_INVALID", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Limit: 10,
		})
		return err
	})
	assertChannelRPCError(t, f.ctx, b.cmds, "PEER_ID_INVALID", func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer: &tg.InputPeerChat{ChatID: chatID}, Message: "after delete", RandomID: 1047499,
		})
		return err
	})

	storedChat, found, err := f.store.ChatByID(f.ctx, chatID)
	if err != nil || !found || storedChat.ID != chatID {
		t.Fatalf("deleted chat row = %+v, found=%v err=%v; want retained tombstone %d", storedChat, found, err, chatID)
	}
	if storedChat.Version != chatBeforeDelete.Version+1 {
		t.Fatalf("deleted chat version = %d, want one bump from %d", storedChat.Version, chatBeforeDelete.Version)
	}
	participants, err := f.store.Participants(f.ctx, chatID)
	if err != nil || len(participants) != 0 {
		t.Fatalf("deleted chat participants = %v, err=%v; want none", participants, err)
	}
	formerHistory, err := f.store.History(f.ctx, d.id, store.PeerTypeChat, chatID, 0, 100)
	if err != nil {
		t.Fatalf("former member history after deletion: %v", err)
	}
	var formerCopyRetained bool
	for _, message := range formerHistory {
		if message.Text == "former member copy" && !message.Deleted {
			formerCopyRetained = true
		}
	}
	if !formerCopyRetained {
		t.Fatalf("former member history = %+v, want frozen copy retained", formerHistory)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		result, err := api.MessagesGetChats(ctx, []int64{chatID})
		if err != nil {
			return err
		}
		chats, ok := result.(*tg.MessagesChats)
		if !ok || len(chats.Chats) != 1 {
			return fmt.Errorf("getChats after deletion = %T with %d chats, want one tombstone", result, len(chats.GetChats()))
		}
		forbidden, ok := chats.Chats[0].(*tg.ChatForbidden)
		if !ok || forbidden.ID != chatID || forbidden.Title != "" {
			return fmt.Errorf("deleted chat = %T/%+v, want empty ChatForbidden %d", chats.Chats[0], chats.Chats[0], chatID)
		}
		return nil
	}); err != nil {
		t.Fatalf("member getChats after deletion: %v", err)
	}

	states := make(map[int64]store.State, 3)
	for _, member := range []*smokeClient{a, b, c} {
		states[member.id], err = f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d state before repeated delete: %v", member.id, err)
		}
	}
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		var err error
		deleted, err = api.MessagesDeleteChat(ctx, chatID)
		return err
	}); err != nil {
		t.Fatalf("repeat creator deleteChat: %v", err)
	}
	if !deleted {
		t.Fatal("repeat creator deleteChat returned false")
	}
	for _, member := range []*smokeClient{a, b, c} {
		state, err := f.store.StateWithoutUnread(f.ctx, member.id)
		if err != nil {
			t.Fatalf("read member %d state after repeated delete: %v", member.id, err)
		}
		if state.Pts != states[member.id].Pts {
			t.Errorf("member %d pts after repeated delete = %d, want unchanged %d", member.id, state.Pts, states[member.id].Pts)
		}
	}
	repeatedChat, found, err := f.store.ChatByID(f.ctx, chatID)
	if err != nil || !found || repeatedChat.Version != storedChat.Version {
		t.Errorf("chat after repeated delete = %+v, found=%v err=%v; want unchanged version %d", repeatedChat, found, err, storedChat.Version)
	}
}

func waitSmokeChatRefresh(t *testing.T, ctx context.Context, member *smokeClient, chatID int64, previousVersion int) int {
	t.Helper()
	env := recvOrCtx(t, ctx, member.push.chatUpdate, "basic-group chat refresh")
	if env.Update == nil || env.Update.ChatID != chatID {
		t.Fatalf("member %d chat update = %+v, want chat %d", member.id, env.Update, chatID)
	}
	for _, peer := range env.Chats {
		chat, ok := peer.(*tg.Chat)
		if !ok || chat.ID != chatID {
			continue
		}
		if chat.Version <= previousVersion {
			t.Fatalf("member %d chat update version = %d, want > %d", member.id, chat.Version, previousVersion)
		}
		return chat.Version
	}
	t.Fatalf("member %d chat update omitted chat %d", member.id, chatID)
	return 0
}

func testSmokeChannel(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneSubscriber, phoneHiddenJoiner = "+15551048001", "+15551048002", "+15551048003"
	seedSmokeUsers(t, f, phoneCreator, phoneSubscriber, phoneHiddenJoiner)
	creator := newSmokeClient(t, f, "A1", phoneCreator)
	subscriber := newSmokeClient(t, f, "B1", phoneSubscriber)
	hiddenJoiner := newSmokeClient(t, f, "C1", phoneHiddenJoiner)
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
	assertPeerDialog := func(client *smokeClient, wantTopID int, wantText, wantServiceTitle string) {
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
			if wantServiceTitle != "" {
				if len(result.Messages) != 1 || !hasChannelCreateDialogMessage(result.Messages, wantTopID, wantServiceTitle) {
					return fmt.Errorf("getPeerDialogs omitted channel create service message %d with title %q", wantTopID, wantServiceTitle)
				}
				return nil
			}
			if len(result.Messages) != 1 {
				return fmt.Errorf("posted getPeerDialogs messages = %d, want 1", len(result.Messages))
			}
			message, ok := result.Messages[0].(*tg.Message)
			if !ok || message.ID != wantTopID || message.Message != wantText || !message.GetPost() {
				return fmt.Errorf("getPeerDialogs top message = %T/%+v, want id %d text %q", result.Messages[0], result.Messages[0], wantTopID, wantText)
			}
			if from, present := message.GetFromID(); present {
				return fmt.Errorf("getPeerDialogs broadcast post has sender %+v", from)
			}
			for _, user := range result.Users {
				if user.GetID() == creator.id {
					return fmt.Errorf("getPeerDialogs exposed broadcast post author %d", creator.id)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("%s getPeerDialogs: %v", client.label, err)
		}
	}
	assertPeerDialog(subscriber, 1, "", "Smoke channel")

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
		if update.Msg.Message != post || update.Msg.ID <= 0 || update.Msg.Out || !update.Msg.GetPost() {
			t.Fatalf("subscriber channel message = {text:%q id:%d out:%v post:%v}, want {%q, positive id, out:false, post:true}", update.Msg.Message, update.Msg.ID, update.Msg.Out, update.Msg.GetPost(), post)
		}
		peer, ok := update.Msg.PeerID.(*tg.PeerChannel)
		if !ok || peer.ChannelID != channelID {
			t.Fatalf("subscriber channel peer = %+v, want channel %d", update.Msg.PeerID, channelID)
		}
		if from, present := update.Msg.GetFromID(); present {
			t.Fatalf("subscriber broadcast post has sender %+v", from)
		}
		postIDs[post] = update.Msg.ID
	}
	assertSmokeChannelDifferenceTimeout(t, f.ctx, subscriber, channelID, 0, false)
	assertSmokeChannelDifferenceTimeout(t, f.ctx, subscriber, channelID, 1_000_000, true)
	assertPeerDialog(subscriber, postIDs[posts[1]], posts[1], "")
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
			if !ok || message.ID != wantID || message.Out || !message.GetPost() {
				return fmt.Errorf("channel history message = {text:%q id:%d out:%v post:%v}, want a known post id, out:false, post:true", message.Message, message.ID, message.Out, message.GetPost())
			}
			peer, ok := message.PeerID.(*tg.PeerChannel)
			if !ok || peer.ChannelID != channelID {
				return fmt.Errorf("channel history peer = %+v, want channel %d", message.PeerID, channelID)
			}
			if from, present := message.GetFromID(); present {
				return fmt.Errorf("channel history broadcast post has sender %+v", from)
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
		_, err := api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: peerChannel(creator.id, channelID), ID: laterUpdate.Msg.ID,
		})
		return err
	}); err != nil {
		t.Fatalf("pin smoke channel post: %v", err)
	}
	assertSmokeChannelPinUpdate(t, recvOrCtx(t, f.ctx, subscriber.seen.pinnedChanMsg, "subscriber channel pin update"), true, channelID, laterUpdate.Msg.ID, laterUpdate.Pts+1)
	for _, member := range []*smokeClient{creator, subscriber} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeChannelPin(ctx, api, member.id, channelID, laterUpdate.Msg.ID, true)
		}); err != nil {
			t.Fatalf("%s getFullChannel pinned message: %v", member.label, err)
		}
	}
	if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
			Peer: peerChannel(creator.id, channelID), ID: laterUpdate.Msg.ID, Unpin: true,
		})
		return err
	}); err != nil {
		t.Fatalf("unpin smoke channel post: %v", err)
	}
	assertSmokeChannelPinUpdate(t, recvOrCtx(t, f.ctx, subscriber.seen.pinnedChanMsg, "subscriber channel unpin update"), false, channelID, laterUpdate.Msg.ID, laterUpdate.Pts+2)
	for _, member := range []*smokeClient{creator, subscriber} {
		if err := member.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			return verifySmokeChannelPin(ctx, api, member.id, channelID, 0, false)
		}); err != nil {
			t.Fatalf("%s getFullChannel after unpin: %v", member.label, err)
		}
	}
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
	// The pin and unpin above each spent one channel pts.
	if deleted.Pts != laterUpdate.Pts+3 || deleted.PtsCount != 1 {
		t.Fatalf("channels.deleteMessages result = {pts:%d pts_count:%d}, want {%d,1}", deleted.Pts, deleted.PtsCount, laterUpdate.Pts+3)
	}
	deleteUpdate := recvOrCtx(t, f.ctx, subscriber.seen.delChannelMsg, "subscriber channel delete update")
	if deleteUpdate.ChannelID != channelID || deleteUpdate.Pts != deleted.Pts || deleteUpdate.PtsCount != 1 ||
		len(deleteUpdate.Messages) != 1 || deleteUpdate.Messages[0] != laterUpdate.Msg.ID {
		t.Fatalf("subscriber channel delete update = %+v, want channel %d post %d at pts %d/count 1", deleteUpdate, channelID, laterUpdate.Msg.ID, deleted.Pts)
	}
	assertPeerDialog(subscriber, postIDs[posts[1]], posts[1], "")
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
	assertPeerDialog(subscriber, postIDs[posts[0]], posts[0], "")
	deleteSmokeChannelPost(t, f, channelID, postIDs[posts[0]])
	assertPeerDialog(subscriber, 1, "", "Smoke channel")

	exerciseSmokeChannelPreHistoryHidden(t, f, creator, subscriber, hiddenJoiner, nil)
}

func testSmokeChannelMembershipRevocation(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneMember = "+15551048011", "+15551048012"
	seedSmokeUsers(t, f, phoneCreator, phoneMember)
	creator := newSmokeClient(t, f, "A1", phoneCreator)
	member := newSmokeClient(t, f, "B1", phoneMember)
	memberOtherSession := newSmokeClient(t, f, "B2", phoneMember)
	waitForDistinctAuthKeys(t, f.ctx, f.registry, member.id, 2, "B sessions", memberOtherSession.lifecycle)

	channelID := createBroadcastChannel(t, f.ctx, creator.cmds, "Revoked channel")
	hash := exportChannelInvite(t, f.ctx, creator.id, creator.cmds, channelID)
	if joinedID := importChannelInvite(t, f.ctx, member.cmds, hash); joinedID != channelID {
		t.Fatalf("member joined channel %d, want %d", joinedID, channelID)
	}

	execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:      inputChannel(creator.id, channelID),
			Participant:  peerUser(creator.id, member.id),
			BannedRights: tg.ChatBannedRights{ViewMessages: true},
		})
		return err
	})

	assertForbiddenUpdate := func(client *smokeClient, reason string) {
		t.Helper()
		for {
			update := recvOrCtx(t, f.ctx, client.seen.channelUpdate, client.label+" updateChannel after "+reason)
			if update.Update.ChannelID != channelID {
				continue
			}
			if len(update.Chats) != 1 {
				t.Fatalf("%s updateChannel chats = %d, want one metadata peer", client.label, len(update.Chats))
			}
			forbidden, ok := update.Chats[0].(*tg.ChannelForbidden)
			if !ok || forbidden.ID != channelID {
				continue
			}
			if forbidden.Title != "" {
				t.Fatalf("%s forbidden channel title = %q, want empty metadata", client.label, forbidden.Title)
			}
			return
		}
	}
	assertForbiddenUpdate(member, "ban")
	assertForbiddenUpdate(memberOtherSession, "ban")

	if err := member.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel(member.id, channelID)})
		if err != nil {
			return err
		}
		if len(result.GetChats()) != 1 {
			return fmt.Errorf("getChannels chats = %d, want one forbidden channel", len(result.GetChats()))
		}
		if forbidden, ok := result.GetChats()[0].(*tg.ChannelForbidden); !ok || forbidden.ID != channelID {
			return fmt.Errorf("getChannels peer = %T, want ChannelForbidden for %d", result.GetChats()[0], channelID)
		}
		return nil
	}); err != nil {
		t.Fatalf("member getChannels after ban: %v", err)
	}

	execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, client *tg.Client) error {
		_, err := client.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
			Peer:     peerChannel(creator.id, channelID),
			Message:  "private-after-revocation",
			RandomID: 1048011,
		})
		return err
	})

	window := time.NewTimer(time.Second)
	defer window.Stop()
	windowOpen := true
	for windowOpen {
		select {
		case update := <-member.seen.newChannelMsg:
			t.Fatalf("member received post after ban: %q", update.Msg.Message)
		case update := <-memberOtherSession.seen.newChannelMsg:
			t.Fatalf("member's other session received post after ban: %q", update.Msg.Message)
		case <-window.C:
			windowOpen = false
		case <-f.ctx.Done():
			t.Fatalf("waiting for post-revocation delivery window: %s", contextFailureDescription(f.ctx))
		}
	}

	execChannel(t, f.ctx, creator.cmds, func(ctx context.Context, client *tg.Client) error {
		_, err := client.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:      inputChannel(creator.id, channelID),
			Participant:  peerUser(creator.id, member.id),
			BannedRights: tg.ChatBannedRights{},
		})
		return err
	})
	if err := member.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.ChannelsGetChannels(ctx, []tg.InputChannelClass{inputChannel(member.id, channelID)})
		if err != nil {
			return err
		}
		if len(result.GetChats()) != 1 {
			return fmt.Errorf("getChannels after unban chats = %d, want one channel", len(result.GetChats()))
		}
		if channel, ok := result.GetChats()[0].(*tg.Channel); !ok || channel.ID != channelID {
			return fmt.Errorf("getChannels after unban peer = %T, want Channel %d", result.GetChats()[0], channelID)
		}
		return nil
	}); err != nil {
		t.Fatalf("member getChannels after unban: %v", err)
	}
	if err := member.call(f.ctx, func(ctx context.Context, client *tg.Client) error {
		result, err := client.ChannelsLeaveChannel(ctx, inputChannel(member.id, channelID))
		if err != nil {
			return err
		}
		updates, ok := result.(*tg.Updates)
		if !ok || len(updates.Updates) != 1 || len(updates.Chats) != 1 {
			return fmt.Errorf("leaveChannel result = %T, want one channel update and one peer", result)
		}
		if _, ok := updates.Chats[0].(*tg.ChannelForbidden); !ok {
			return fmt.Errorf("leaveChannel peer = %T, want ChannelForbidden", updates.Chats[0])
		}
		return nil
	}); err != nil {
		t.Fatalf("member leaveChannel: %v", err)
	}
	assertForbiddenUpdate(memberOtherSession, "leave")
}

func assertSmokeChannelDifferenceTimeout(t *testing.T, ctx context.Context, client *smokeClient, channelID int64, pts int, wantEmpty bool) {
	t.Helper()
	if err := client.call(ctx, func(ctx context.Context, api *tg.Client) error {
		response, err := api.UpdatesGetChannelDifference(ctx, &tg.UpdatesGetChannelDifferenceRequest{
			Channel: inputChannel(client.id, channelID),
			Filter:  &tg.ChannelMessagesFilterEmpty{},
			Pts:     pts,
			Limit:   100,
		})
		if err != nil {
			return err
		}
		var timeout int
		var present bool
		switch difference := response.(type) {
		case *tg.UpdatesChannelDifferenceEmpty:
			if !wantEmpty {
				return errors.New("getChannelDifference response = empty, want a difference")
			}
			timeout, present = difference.GetTimeout()
		case *tg.UpdatesChannelDifference:
			if wantEmpty {
				return errors.New("getChannelDifference response = difference, want empty")
			}
			timeout, present = difference.GetTimeout()
		case *tg.UpdatesChannelDifferenceTooLong:
			timeout, present = difference.GetTimeout()
		default:
			return fmt.Errorf("getChannelDifference response = %T, want a channel difference", response)
		}
		if !present || timeout != 30 {
			return fmt.Errorf("getChannelDifference timeout = %d, present = %v, want 30 seconds", timeout, present)
		}
		return nil
	}); err != nil {
		t.Fatalf("channel difference timeout smoke: %v", err)
	}
}

func testSmokeMTProtoServiceMessages(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phone = "+15551049901"
	seedSmokeUsers(t, f, phone)
	client := newSmokeClient(t, f, "MTProto service smoke", phone)

	ctx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	defer cancel()
	data, err := (&session.Loader{Storage: client.session}).Load(ctx)
	if err != nil {
		t.Fatalf("load client session: %v", err)
	}
	if len(data.AuthKey) != len(crypto.Key{}) {
		t.Fatalf("auth key length = %d, want %d", len(data.AuthKey), len(crypto.Key{}))
	}
	var rawKey crypto.Key
	copy(rawKey[:], data.AuthKey)
	key := rawKey.WithID()
	sessionID, err := crypto.NewSessionID(crypto.DefaultRand())
	if err != nil {
		t.Fatalf("generate session id: %v", err)
	}

	baseID := int64(proto.NewMessageID(time.Now(), proto.MessageFromClient))
	baseID -= baseID % 4
	stateReqID, resendReqID, allInfoID := baseID, baseID+4, baseID+8
	messages := []bin.Encoder{
		&mt.MsgsStateReq{MsgIDs: []int64{stateReqID - 4}},
		&mt.MsgResendReq{MsgIDs: []int64{resendReqID - 4, resendReqID - 8}},
		&mt.MsgsAllInfo{MsgIDs: []int64{allInfoID - 4}, Info: []byte{1}},
	}
	container := &proto.MessageContainer{Messages: make([]proto.Message, 0, len(messages))}
	for i, message := range messages {
		var body bin.Buffer
		if err := message.Encode(&body); err != nil {
			t.Fatalf("encode service message %d: %v", i, err)
		}
		container.Messages = append(container.Messages, proto.Message{
			ID:    baseID + int64(i*4),
			SeqNo: i * 2,
			Bytes: body.Len(),
			Body:  body.Copy(),
		})
	}
	containerID := baseID + int64(len(messages)*4)
	var body bin.Buffer
	if err := container.Encode(&body); err != nil {
		t.Fatalf("encode service message container: %v", err)
	}
	frame := crypto.EncryptedMessageData{
		Salt:                   data.Salt,
		SessionID:              sessionID,
		MessageID:              containerID,
		MessageDataLen:         int32(body.Len()), //nolint:gosec // the service-message container is small
		MessageDataWithPadding: body.Copy(),
	}
	if err := crypto.NewClientCipher(crypto.DefaultRand()).Encrypt(key, frame, &body); err != nil {
		t.Fatalf("encrypt service message container: %v", err)
	}

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))
	rawConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("dial smoke server: %v", err)
	}
	defer func() {
		if err := rawConn.Close(); err != nil {
			t.Errorf("close smoke connection: %v", err)
		}
	}()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("smoke context has no deadline")
	}
	if err := rawConn.SetDeadline(deadline); err != nil {
		t.Fatalf("set smoke connection deadline: %v", err)
	}
	wire, err := transport.Intermediate.Handshake(rawConn)
	if err != nil {
		t.Fatalf("start intermediate transport: %v", err)
	}
	if err := wire.Send(ctx, &bin.Buffer{Buf: body.Copy()}); err != nil {
		t.Fatalf("send service message container: %v", err)
	}

	clientCipher := crypto.NewClientCipher(crypto.DefaultRand())
	responses := make([][]byte, 0, 3)
	for range 3 {
		var encrypted bin.Buffer
		if err := wire.Recv(ctx, &encrypted); err != nil {
			t.Fatalf("receive service response: %v", err)
		}
		message := &crypto.EncryptedMessage{}
		if err := message.DecodeWithoutCopy(&encrypted); err != nil {
			t.Fatalf("decode encrypted service response: %v", err)
		}
		decrypted, err := clientCipher.Decrypt(key, message)
		if err != nil {
			t.Fatalf("decrypt service response: %v", err)
		}
		responses = append(responses, append([]byte(nil), decrypted.Data()...))
	}
	var created mt.NewSessionCreated
	if err := created.Decode(&bin.Buffer{Buf: responses[0]}); err != nil {
		t.Fatalf("first service response = %x, want new_session_created: %v", responses[0], err)
	}
	states := map[int64][]byte{}
	for _, response := range responses[1:] {
		var info mt.MsgsStateInfo
		if err := info.Decode(&bin.Buffer{Buf: response}); err != nil {
			var rpcError mt.RPCError
			if rpcError.Decode(&bin.Buffer{Buf: response}) == nil {
				t.Fatalf("service response is RPC error %d %q", rpcError.ErrorCode, rpcError.ErrorMessage)
			}
			t.Fatalf("service response = %x, want msgs_state_info: %v", response, err)
		}
		states[info.ReqMsgID] = info.Info
	}
	if got := len(states[stateReqID]); got != 1 {
		t.Fatalf("msgs_state_req response has %d state bytes, want 1", got)
	}
	if got := len(states[resendReqID]); got != 2 {
		t.Fatalf("msg_resend_req response has %d state bytes, want 2", got)
	}
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

func assertSmokePollAnswerWebPage(t *testing.T, label string, message *tg.Message, wantURL string) int64 {
	t.Helper()
	media, ok := message.Media.(*tg.MessageMediaPoll)
	if !ok || len(media.Poll.Answers) != 2 {
		t.Fatalf("%s media = %#v, want a poll with two answers", label, message.Media)
	}
	first, ok := media.Poll.Answers[0].(*tg.PollAnswer)
	if !ok {
		t.Fatalf("%s first answer = %T, want pollAnswer", label, media.Poll.Answers[0])
	}
	webPageMedia, ok := first.Media.(*tg.MessageMediaWebPage)
	if !ok {
		t.Fatalf("%s first answer media = %#v, want messageMediaWebPage", label, first.Media)
	}
	webPage, ok := webPageMedia.Webpage.(*tg.WebPageEmpty)
	if !ok || webPage.ID == 0 || webPage.URL != wantURL {
		t.Fatalf("%s webpage = %#v, want nonzero id and exact URL %q", label, webPageMedia.Webpage, wantURL)
	}
	second, ok := media.Poll.Answers[1].(*tg.PollAnswer)
	if !ok || second.Media != nil {
		t.Fatalf("%s second answer media = %#v, want none", label, media.Poll.Answers[1])
	}
	return webPage.ID
}

func testSmokeChannelPollLifecycle(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneCreator, phoneSubscriber = "+15551048101", "+15551048102"
	seedSmokeUsers(t, f, phoneCreator, phoneSubscriber)
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
	seedSmokeUsers(t, f, phoneA, phoneB)

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
	seedSmokeUsersWithHandles(t, f, []string{phoneA, phoneB}, map[string]string{
		phoneA: smokeUsernameForPhone(phoneA),
		phoneB: "smokefullprofile",
	})

	a := newSmokeClient(t, f, "full-profile A", phoneA)
	b := newSmokeClient(t, f, "full-profile B", phoneB)

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
	if !searchOnly.FullUser.Settings.Zero() || len(searchOnly.Chats) != 0 || searchOnly.FullUser.CommonChatsCount != 0 {
		t.Fatalf("search-only full user = settings:%+v chats:%d common:%d, want empty bars and no reported common chats",
			searchOnly.FullUser.Settings, len(searchOnly.Chats), searchOnly.FullUser.CommonChatsCount)
	}
	if muteUntil, ok := searchOnly.FullUser.NotifySettings.GetMuteUntil(); !ok || muteUntil != 0 {
		t.Fatalf("search-only full user notify mute_until=(%d,%v), want (0,true)", muteUntil, ok)
	}
	if searchOnly.FullUser.PhoneCallsAvailable || searchOnly.FullUser.PhoneCallsPrivate || searchOnly.FullUser.VideoCallsAvailable ||
		searchOnly.FullUser.CanPinMessage || searchOnly.FullUser.HasScheduled || searchOnly.FullUser.VoiceMessagesForbidden {
		t.Fatalf("full user advertises a capability the server does not implement: %+v", searchOnly.FullUser)
	}
	if _, ok := searchOnly.FullUser.GetProfilePhoto(); ok {
		t.Fatalf("full user claims a profile photo the server does not store")
	}
	if _, ok := searchOnly.FullUser.GetAbout(); ok {
		t.Fatalf("full user claims an unset biography")
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

	// Self is the caller's own username account, with no bar.
	self := getFullUser(a, &tg.InputUserSelf{})
	selfUser, err := requireSmokeFullUser(self.Users, a.id, "users.getFullUser self")
	if err != nil {
		t.Fatal(err)
	}
	if self.FullUser.ID != a.id || self.FullUser.Blocked || !self.FullUser.Settings.Zero() {
		t.Fatalf("self full user = {id:%d blocked:%t settings:%+v}, want A with no block and no bar",
			self.FullUser.ID, self.FullUser.Blocked, self.FullUser.Settings)
	}
	wantSelfUsername := smokeUsernameForPhone(phoneA)
	if !selfUser.Self || selfUser.Phone != "" || selfUser.Username != wantSelfUsername {
		t.Fatalf("self user = {self:%t phone:%q username:%q}, want A's own username %q and no phone",
			selfUser.Self, selfUser.Phone, selfUser.Username, wantSelfUsername)
	}
}

func testSmokeBotUserProfile(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	bot, _, err := f.store.CreateBot(f.ctx, "smokebotprofile_bot", "Smoke bot")
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	const phoneHuman = "+15551049303"
	seedSmokeUsers(t, f, phoneHuman)
	requests := &clientRequestRecorder{}
	human := newSmokeClientWithRequests(t, f, "bot profile human", phoneHuman, requests)
	var full *tg.UsersUserFull
	if err := fixtureClientCall(t, human, "users_getFullUser.bot.1", func(ctx context.Context, api *tg.Client) error {
		var err error
		full, err = api.UsersGetFullUser(ctx, inputUser(human.id, bot.ID))
		return err
	}); err != nil {
		t.Fatalf("human users.getFullUser for bot fixture: %v", err)
	}

	user, err := requireSmokeFullUser(full.Users, bot.ID, "human users.getFullUser for bot")
	if err != nil {
		t.Fatal(err)
	}
	if !user.Bot || !user.BotNochats {
		t.Fatalf("bot user flags = bot:%v nochats:%v, want true/true", user.Bot, user.BotNochats)
	}
	if version, ok := user.GetBotInfoVersion(); !ok || version == 0 {
		t.Fatalf("bot_info_version = (%d, %v), want a nonzero value", version, ok)
	}
	if full.FullUser.ID != bot.ID {
		t.Fatalf("full user id = %d, want bot %d", full.FullUser.ID, bot.ID)
	}
	info, ok := full.FullUser.GetBotInfo()
	if !ok || !info.Zero() {
		t.Fatalf("bot_info = (%+v, %v), want minimal empty bot info", info, ok)
	}
}

func testSmokeCommonChats(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const phoneA, phoneB, phoneC = "+15551049401", "+15551049402", "+15551049403"
	seedSmokeUsers(t, f, phoneA, phoneB, phoneC)
	a := newSmokeClient(t, f, "common chats A", phoneA)
	b := newSmokeClient(t, f, "common chats B", phoneB)
	c := newSmokeClient(t, f, "common chats C", phoneC)

	createGroup := func(creator *smokeClient, title string, members ...int64) int64 {
		t.Helper()
		var chatID int64
		err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			users := make([]tg.InputUserClass, 0, len(members))
			for _, memberID := range members {
				users = append(users, inputUser(creator.id, memberID))
			}
			created, err := api.MessagesCreateChat(ctx, &tg.MessagesCreateChatRequest{Title: title, Users: users})
			if err != nil {
				return err
			}
			updates, ok := created.Updates.(*tg.Updates)
			if !ok {
				return fmt.Errorf("messages.createChat result = %T, want *tg.Updates", created.Updates)
			}
			if len(updates.Chats) != 1 {
				return fmt.Errorf("messages.createChat returned %d chats, want one", len(updates.Chats))
			}
			chat, ok := updates.Chats[0].(*tg.Chat)
			if !ok {
				return fmt.Errorf("messages.createChat chat = %T, want *tg.Chat", updates.Chats[0])
			}
			chatID = chat.ID
			return nil
		})
		if err != nil {
			t.Fatalf("create group %q: %v", title, err)
		}
		return chatID
	}

	sharedBasicID := createGroup(a, "Common basic group", b.id)
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed a legacy channel id collision: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close common-chats seed connection: %v", err)
		}
	})
	if _, err := conn.Exec(f.ctx, `
		INSERT INTO channels (id, title, creator_id, megagroup)
		VALUES ($1, 'Legacy colliding supergroup', $2, true)
	`, sharedBasicID, a.id); err != nil {
		t.Fatalf("seed legacy supergroup with basic-group id %d: %v", sharedBasicID, err)
	}
	if _, err := conn.Exec(f.ctx, `INSERT INTO channel_state (channel_id) VALUES ($1)`, sharedBasicID); err != nil {
		t.Fatalf("seed channel state for colliding supergroup %d: %v", sharedBasicID, err)
	}
	for _, memberID := range []int64{a.id, b.id} {
		role := int16(0)
		if memberID == a.id {
			role = 2
		}
		if _, err := conn.Exec(f.ctx, `
			INSERT INTO channel_participants (channel_id, user_id, role, join_pts)
			VALUES ($1, $2, $3, 0)
		`, sharedBasicID, memberID, role); err != nil {
			t.Fatalf("seed member %d in colliding supergroup %d: %v", memberID, sharedBasicID, err)
		}
	}
	createGroup(a, "Alice only", c.id)
	leftBasicID := createGroup(a, "Bob left", b.id)
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
			ChatID: leftBasicID,
			UserID: inputUser(b.id, b.id),
		})
		return err
	}); err != nil {
		t.Fatalf("Bob leaves basic group: %v", err)
	}

	createChannel := func(creator *smokeClient, title string, megagroup bool) *tg.Channel {
		t.Helper()
		var createdChannel *tg.Channel
		if err := creator.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			result, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
				Title: title, Broadcast: !megagroup, Megagroup: megagroup,
			})
			if err != nil {
				return err
			}
			updates, ok := result.(*tg.Updates)
			if !ok {
				return fmt.Errorf("channels.createChannel result = %T, want *tg.Updates", result)
			}
			for _, chat := range updates.Chats {
				if channel, ok := chat.(*tg.Channel); ok {
					createdChannel = channel
					return nil
				}
			}
			return errors.New("channels.createChannel returned no channel")
		}); err != nil {
			t.Fatalf("create channel %q: %v", title, err)
		}
		return createdChannel
	}
	inviteMember := func(inviter *smokeClient, channel *tg.Channel, memberID int64) {
		t.Helper()
		if err := inviter.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			_, err := api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
				Channel: &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
				Users:   []tg.InputUserClass{inputUser(inviter.id, memberID)},
			})
			return err
		}); err != nil {
			t.Fatalf("invite user %d to channel %d: %v", memberID, channel.ID, err)
		}
	}
	supergroup := createChannel(a, "Common supergroup", true)
	inviteMember(a, supergroup, b.id)
	broadcast := createChannel(a, "Shared broadcast", false)
	inviteMember(a, broadcast, b.id)
	bannedSupergroup := createChannel(c, "Caller-banned common supergroup", true)
	inviteMember(c, bannedSupergroup, a.id)
	inviteMember(c, bannedSupergroup, b.id)
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.ChannelsEditBanned(ctx, &tg.ChannelsEditBannedRequest{
			Channel:     &tg.InputChannel{ChannelID: bannedSupergroup.ID, AccessHash: bannedSupergroup.AccessHash},
			Participant: peerUser(c.id, a.id),
			BannedRights: tg.ChatBannedRights{
				ViewMessages: true,
				UntilDate:    0,
			},
		})
		return err
	}); err != nil {
		t.Fatalf("ban Alice from common supergroup %d: %v", bannedSupergroup.ID, err)
	}

	getCommonChats := func(user tg.InputUserClass, maxID int64, limit int) (*tg.MessagesChatsSlice, error) {
		t.Helper()
		var result tg.MessagesChatsClass
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			result, err = api.MessagesGetCommonChats(ctx, &tg.MessagesGetCommonChatsRequest{
				UserID: user, MaxID: maxID, Limit: limit,
			})
			return err
		}); err != nil {
			return nil, err
		}
		page, ok := result.(*tg.MessagesChatsSlice)
		if !ok {
			return nil, fmt.Errorf("messages.getCommonChats result = %T, want *tg.MessagesChatsSlice", result)
		}
		return page, nil
	}
	getFullUser := func(user tg.InputUserClass) (*tg.UsersUserFull, error) {
		t.Helper()
		var full *tg.UsersUserFull
		err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			var err error
			full, err = api.UsersGetFullUser(ctx, user)
			return err
		})
		return full, err
	}

	common, err := getCommonChats(inputUser(a.id, b.id), 0, 10)
	if err != nil {
		t.Fatalf("messages.getCommonChats: %v", err)
	}
	if common.Count != 3 || len(common.Chats) != 3 {
		t.Fatalf("common chats count/page = %d/%d, want 3/3", common.Count, len(common.Chats))
	}
	for _, chat := range common.Chats {
		if got, ok := chat.(*tg.Channel); ok && got.ID == bannedSupergroup.ID {
			t.Fatalf("common chats include supergroup %d after Alice was banned", bannedSupergroup.ID)
		}
	}
	channel, ok := common.Chats[0].(*tg.Channel)
	if !ok || channel.ID != supergroup.ID || !channel.Megagroup || channel.Broadcast {
		t.Fatalf("first common chat = %#v, want supergroup %d", common.Chats[0], supergroup.ID)
	}
	collidingChannel, ok := common.Chats[1].(*tg.Channel)
	if !ok || collidingChannel.ID != sharedBasicID || !collidingChannel.Megagroup {
		t.Fatalf("second common chat = %#v, want legacy supergroup with id %d", common.Chats[1], sharedBasicID)
	}
	group, ok := common.Chats[2].(*tg.Chat)
	if !ok || group.ID != sharedBasicID {
		t.Fatalf("third common chat = %#v, want basic group %d", common.Chats[2], sharedBasicID)
	}

	// Alice's own group and the broadcast channel are not common chats; Bob's
	// former membership in the third basic group must no longer qualify it.
	full, err := getFullUser(inputUser(a.id, b.id))
	if err != nil {
		t.Fatalf("users.getFullUser for Bob: %v", err)
	}
	if full.FullUser.CommonChatsCount != common.Count {
		t.Fatalf("common_chats_count = %d, want %d", full.FullUser.CommonChatsCount, common.Count)
	}
	self, err := getFullUser(&tg.InputUserSelf{})
	if err != nil {
		t.Fatalf("users.getFullUser self: %v", err)
	}
	if self.FullUser.CommonChatsCount != 0 {
		t.Fatalf("self common_chats_count = %d, want 0", self.FullUser.CommonChatsCount)
	}

	repeated, err := getCommonChats(inputUser(a.id, b.id), 0, 10)
	if err != nil {
		t.Fatalf("repeat messages.getCommonChats: %v", err)
	}
	if repeated.Count != common.Count || !reflect.DeepEqual(repeated.Chats, common.Chats) {
		t.Fatalf("repeated common chats = count %d, chats %v; want count %d, chats %v", repeated.Count, repeated.Chats, common.Count, common.Chats)
	}

	firstPage, err := getCommonChats(inputUser(a.id, b.id), 0, 1)
	if err != nil {
		t.Fatalf("first common-chats page: %v", err)
	}
	if firstPage.Count != 3 || len(firstPage.Chats) != 1 {
		t.Fatalf("first page count/chats = %d/%d, want 3/1", firstPage.Count, len(firstPage.Chats))
	}
	firstChannel, ok := firstPage.Chats[0].(*tg.Channel)
	if !ok || firstChannel.ID != supergroup.ID {
		t.Fatalf("first page chat = %#v, want supergroup %d", firstPage.Chats[0], supergroup.ID)
	}
	secondPage, err := getCommonChats(inputUser(a.id, b.id), firstChannel.ID, 1)
	if err != nil {
		t.Fatalf("second common-chats page: %v", err)
	}
	if secondPage.Count != 3 || len(secondPage.Chats) != 2 {
		t.Fatalf("second page count/chats = %d/%d, want 3/2 to keep the equal-id pair together", secondPage.Count, len(secondPage.Chats))
	}
	secondChannel, ok := secondPage.Chats[0].(*tg.Channel)
	if !ok || secondChannel.ID != sharedBasicID {
		t.Fatalf("first second-page chat = %#v, want colliding supergroup %d", secondPage.Chats[0], sharedBasicID)
	}
	secondGroup, ok := secondPage.Chats[1].(*tg.Chat)
	if !ok || secondGroup.ID != sharedBasicID || secondGroup.ID != secondChannel.ID {
		t.Fatalf("second second-page chat = %#v, want basic group with canonical colliding id %d", secondPage.Chats[1], sharedBasicID)
	}
	emptyPage, err := getCommonChats(inputUser(a.id, b.id), secondGroup.ID, 1)
	if err != nil {
		t.Fatalf("empty common-chats page: %v", err)
	}
	if emptyPage.Count != 3 || len(emptyPage.Chats) != 0 {
		t.Fatalf("empty page count/chats = %d/%d, want 3/0", emptyPage.Count, len(emptyPage.Chats))
	}

	for name, selfTarget := range map[string]tg.InputUserClass{
		"inputUserSelf": &tg.InputUserSelf{},
		"own id":        inputUser(a.id, a.id),
	} {
		if _, err := getCommonChats(selfTarget, 0, 10); err == nil || !tgerr.Is(err, "USER_ID_INVALID") {
			t.Errorf("self target %s error = %v, want USER_ID_INVALID", name, err)
		}
	}
	const unknownUserID = int64(9_000_000_000)
	unknownInput := inputUser(a.id, unknownUserID)
	_, commonErr := getCommonChats(unknownInput, 0, 10)
	_, fullUserErr := getFullUser(unknownInput)
	if commonErr == nil || fullUserErr == nil || !tgerr.Is(commonErr, "PEER_ID_INVALID") || !tgerr.Is(fullUserErr, "PEER_ID_INVALID") || commonErr.Error() != fullUserErr.Error() {
		t.Errorf("unknown target errors = common:%v full-user:%v, want the same PEER_ID_INVALID", commonErr, fullUserErr)
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
	branchSignupPreAuthCheck       smokeRegistrationBranch = "signup-pre-auth-check"
	branchSignupSignUp             smokeRegistrationBranch = "signup-sign-up"
	branchSignupAuthorization      smokeRegistrationBranch = "signup-authorization"
	branchSignupUser               smokeRegistrationBranch = "signup-user"
	branchSignupWelcomeDialogs     smokeRegistrationBranch = "signup-welcome-dialogs"
	branchSignupWelcomeChannel     smokeRegistrationBranch = "signup-welcome-channel"
	branchSignupWelcomeMessage     smokeRegistrationBranch = "signup-welcome-message"
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

// smokeRegistrationForceBranch makes one registration branch fail at its own
// step. TG_SMOKE_FORCE_BRANCH sets it, and only the diagnostics verifier sets
// that: it needs the branch's real Go reporting path, end to end, so it fails
// the step's error and lets the scenario's own handler report it. Unset, the
// value matches no branch, so every scenario runs unchanged.
var smokeRegistrationForceBranch = smokeRegistrationBranch(os.Getenv("TG_SMOKE_FORCE_BRANCH"))

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

// loadSmokeSession allows the initial session save to become visible after Run
// signals readiness and the first RPC completes.
func loadSmokeSession(ctx context.Context, storage session.Storage) (*session.Data, error) {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := (&session.Loader{Storage: storage}).Load(ctx)
		if err == nil || !errors.Is(err, session.ErrNotFound) {
			return data, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, err
		case <-ticker.C:
		}
	}
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
		sessionData, err := loadSmokeSession(ctx, sess)
		if smokeRegistrationForceBranch == branchReservedSessionLoad {
			// Diagnostics verifier only: fail the step itself, so the handler below
			// is the code that attributes and reports the branch.
			err = session.ErrNotFound
		}
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
	const username, pendingPhone, password, welcomeHandle = "smokenewacct", "+15551049003", "smoke-password-1049", "smokewelcome"
	const signUpTerms = "Terms for this self-hosted smoke instance."
	var welcomeChannelID int64
	var welcomeLatestMessageID int64
	f := newSmokeFixtureWithSetup(t, config.RegistrationOpen, func(f *smokeFixture) {
		f.welcomeChannel = welcomeHandle
		f.signUpTermsOfService = signUpTerms
		owner, err := f.store.CreateUser(f.ctx, "+15551049005")
		if err != nil {
			t.Fatalf("create welcome channel owner: %v", err)
		}
		channel, latest, _, err := f.store.CreateChannelWithServiceMessage(f.ctx, owner.ID, "Teagram demo welcome", "", false)
		if err != nil {
			t.Fatalf("create welcome channel: %v", err)
		}
		if err := f.store.ClaimChannelUsername(f.ctx, channel.ID, welcomeHandle); err != nil {
			t.Fatalf("claim welcome channel handle: %v", err)
		}
		welcomeChannelID = channel.ID
		welcomeLatestMessageID = latest.LocalID
	})
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
		available, err := api.AccountCheckUsername(ctx, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupPreAuthCheck, err)
		}
		if !available {
			return smokeRegistrationCheckFailure(branchSignupPreAuthCheck, "candidate was reported occupied")
		}
		codeHash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupSendCode, err)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupCodeWait, err)
		}
		sessionData, err := loadSmokeSession(ctx, firstSession)
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
			return smokeRegistrationStepFailure(branchSignupSignIn, err)
		}
		signUpRequired, ok := response.(*tg.AuthAuthorizationSignUpRequired)
		if !ok {
			return smokeRegistrationCheckFailure(branchSignupSignInResponse, fmt.Sprintf("%T", response))
		}
		terms, ok := signUpRequired.GetTermsOfService()
		if !ok {
			return smokeRegistrationCheckFailure(branchSignupSignInResponse, "sign-up terms omitted")
		}
		if !terms.GetPopup() {
			return smokeRegistrationCheckFailure(branchSignupSignInResponse, "sign-up terms prompt disabled")
		}
		if got := terms.GetText(); got != signUpTerms {
			return smokeRegistrationCheckFailure(branchSignupSignInResponse, "unexpected sign-up terms: "+got)
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
		dialogResult, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      20,
		})
		if err != nil {
			return smokeRegistrationStepFailure(branchSignupWelcomeDialogs, err)
		}
		var dialogs []tg.DialogClass
		var messages []tg.MessageClass
		if page, ok := dialogResult.(*tg.MessagesDialogs); ok {
			dialogs, messages = page.Dialogs, page.Messages
		} else if page, ok := dialogResult.(*tg.MessagesDialogsSlice); ok {
			dialogs, messages = page.Dialogs, page.Messages
		} else {
			return smokeRegistrationCheckFailure(branchSignupWelcomeDialogs, fmt.Sprintf("%T", dialogResult))
		}
		var welcome *tg.Dialog
		for _, item := range dialogs {
			dialog, ok := item.(*tg.Dialog)
			if !ok {
				continue
			}
			peer, ok := dialog.Peer.(*tg.PeerChannel)
			if ok && peer.ChannelID == welcomeChannelID {
				welcome = dialog
				break
			}
		}
		if welcome == nil {
			return smokeRegistrationCheckFailure(branchSignupWelcomeChannel, "omitted")
		}
		if welcome.TopMessage != int(welcomeLatestMessageID) {
			return smokeRegistrationCheckFailure(branchSignupWelcomeChannel, "wrong latest message")
		}
		latestFound := false
		for _, message := range messages {
			if message.GetID() == int(welcomeLatestMessageID) {
				latestFound = true
				break
			}
		}
		if !latestFound {
			return smokeRegistrationCheckFailure(branchSignupWelcomeMessage, "latest message omitted")
		}
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
		case branchSignupPreAuthCheck:
			t.Fatalf("[assert:username-registration.signup-pre-auth-check] account.checkUsername before signup: %s", detail)
		case branchSignupSignUp:
			t.Fatalf("[assert:username-registration.signup-sign-up] auth.signUp: %s", detail)
		case branchSignupAuthorization:
			t.Fatalf("[assert:username-registration.signup-authorization] signUp response, want authorization with a user: %s", detail)
		case branchSignupUser:
			t.Fatalf("[assert:username-registration.signup-user] signUp user, want a full user: %s", detail)
		case branchSignupWelcomeDialogs:
			t.Fatalf("[assert:username-registration.signup-welcome-dialogs] getDialogs after sign-up: %s", detail)
		case branchSignupWelcomeChannel:
			t.Fatalf("[assert:username-registration.signup-welcome-channel] welcome channel missing or not latest in getDialogs: %s", detail)
		case branchSignupWelcomeMessage:
			t.Fatalf("[assert:username-registration.signup-welcome-message] latest welcome message missing from getDialogs: %s", detail)
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
	ctx      context.Context
	failures *clientFailureSignal
	key      *rsa.PrivateKey
	dsn      string
	store    *store.Store
	blobs    blob.Store
	// uploadMaxFileBytes simulates TG_UPLOAD_MAX_BYTES in smoke fixtures.
	uploadMaxFileBytes          int64
	uploadsDisabled             bool
	stickerSets                 *api.StickerSetCatalog
	stickerImportMode           config.StickerImportMode
	photoThumbs                 *photothumb.Supervisor
	codes                       *multiCodeSink
	authHandles                 map[string]string
	dcID                        int
	port                        int
	listener                    *acceptCountingListener
	registry                    *mtproto.SessionRegistry
	stop                        func()
	stopCleanupRegistered       bool
	suppressChannelPostDelivery func(channelID int64) bool
	regMode                     config.RegistrationMode
	userInviteQuota             int
	userInviteMinAccountAge     time.Duration
	welcomeChannel              string
	turnstileSiteKey            string
	turnstileVerifier           api.SignUpTurnstileVerifier
	turnstileSignInEnabled      bool
	recoveryEmailMode           config.RecoveryEmailMode
	signUpTermsOfService        string
	persistStatus               bool
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
	seedSmokeUsers(t, f, phoneA, phoneB)

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

// smokeSecretChatsInDifference returns the secret-chat ids c's getDifference
// carries in updateEncryption.
func smokeSecretChatsInDifference(t *testing.T, f *smokeFixture, c *smokeClient) map[int]bool {
	t.Helper()
	ids := map[int]bool{}
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 1, Date: 1})
		if err != nil {
			return err
		}
		var other []tg.UpdateClass
		switch d := diff.(type) {
		case *tg.UpdatesDifference:
			other = d.OtherUpdates
		case *tg.UpdatesDifferenceSlice:
			other = d.OtherUpdates
		case *tg.UpdatesDifferenceEmpty:
		default:
			return fmt.Errorf("getDifference = %T", diff)
		}
		for _, u := range other {
			if enc, ok := u.(*tg.UpdateEncryption); ok {
				ids[enc.Chat.GetID()] = true
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("getDifference: %v", err)
	}
	return ids
}

// testSmokeSecretChatBlockedRequest is the blocked-request path at the shipped
// budgets: a request to a user who blocked the initiator answers the initiator
// with a waiting chat, exactly as a delivered one, while the blocker never sees
// it in getDifference and cannot accept it. After an unblock, a new request
// reaches the peer, which also proves the difference read is not vacuous.
func testSmokeSecretChatBlockedRequest(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.rateLimits = config.DefaultRateLimits()
	})
	const phoneA, phoneB = "+15551872101", "+15551872102"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "A", phoneA)
	b := newSmokeClient(t, f, "B", phoneB)

	setBlock := func(on bool) {
		if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			if on {
				_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
				return err
			}
			_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(b.id, a.id)})
			return err
		}); err != nil {
			t.Fatalf("B block=%t A: %v", on, err)
		}
	}
	request := func(randomID int) *tg.EncryptedChatWaiting {
		var waiting *tg.EncryptedChatWaiting
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			chat, err := api.MessagesRequestEncryption(ctx, &tg.MessagesRequestEncryptionRequest{
				UserID: inputUser(a.id, b.id), RandomID: randomID, GA: smokeGA(),
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
			t.Fatalf("requestEncryption %d: %v", randomID, err)
		}
		if waiting.AdminID != a.id || waiting.ParticipantID != b.id || waiting.Date <= 0 ||
			waiting.AccessHash != secretChatAccessHash(a.id, waiting.ID) {
			t.Fatalf("requestEncryption %d answered %+v, want a waiting chat from A to B", randomID, waiting)
		}
		return waiting
	}

	setBlock(true)
	withheld := request(1872101)
	if smokeSecretChatsInDifference(t, f, b)[withheld.ID] {
		t.Fatalf("blocker's getDifference shows withheld chat %d", withheld.ID)
	}
	err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesAcceptEncryption(ctx, &tg.MessagesAcceptEncryptionRequest{
			Peer: tg.InputEncryptedChat{ChatID: withheld.ID, AccessHash: secretChatAccessHash(b.id, withheld.ID)},
			GB:   smokeGB(), KeyFingerprint: 1872,
		})
		return err
	})
	assertRPCError(t, err, "ENCRYPTION_ID_INVALID")

	setBlock(false)
	delivered := request(1872102)
	shown := smokeSecretChatsInDifference(t, f, b)
	if shown[withheld.ID] || !shown[delivered.ID] {
		t.Fatalf("peer's getDifference after unblock = %v, want %d and not %d", shown, delivered.ID, withheld.ID)
	}
}

// smokeEncryptedInDifference returns the random ids of the encrypted messages
// c's getDifference from qts 0 carries.
func smokeEncryptedInDifference(t *testing.T, f *smokeFixture, c *smokeClient) map[int64]bool {
	t.Helper()
	ids := map[int64]bool{}
	if err := c.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		diff, err := api.UpdatesGetDifference(ctx, &tg.UpdatesGetDifferenceRequest{Pts: 1, Date: 1, Qts: 0})
		if err != nil {
			return err
		}
		var msgs []tg.EncryptedMessageClass
		switch d := diff.(type) {
		case *tg.UpdatesDifference:
			msgs = d.NewEncryptedMessages
		case *tg.UpdatesDifferenceSlice:
			msgs = d.NewEncryptedMessages
		case *tg.UpdatesDifferenceEmpty:
		default:
			return fmt.Errorf("getDifference = %T", diff)
		}
		for _, m := range msgs {
			ids[m.GetRandomID()] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("getDifference: %v", err)
	}
	return ids
}

// testSmokeSecretChatBlockedMessage is the blocked-message path at the shipped
// budgets: on an active chat, a message to a user who blocked the sender is
// answered with a date while the blocker's getDifference never carries it.
// After an unblock, a new message reaches the peer's getDifference, which also
// proves the difference read is not vacuous.
func testSmokeSecretChatBlockedMessage(t *testing.T) {
	t.Helper()
	f := newSmokeFixtureWithSetup(t, config.RegistrationClosed, func(f *smokeFixture) {
		f.rateLimits = config.DefaultRateLimits()
	})
	const phoneA, phoneB = "+15551873101", "+15551873102"
	seedSmokeUsers(t, f, phoneA, phoneB)
	a := newSmokeClient(t, f, "A", phoneA)
	b := newSmokeClient(t, f, "B", phoneB)

	var chatID int
	if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		chat, err := api.MessagesRequestEncryption(ctx, &tg.MessagesRequestEncryptionRequest{
			UserID: inputUser(a.id, b.id), RandomID: 1873100, GA: smokeGA(),
		})
		if err != nil {
			return err
		}
		chatID = chat.GetID()
		return nil
	}); err != nil {
		t.Fatalf("requestEncryption: %v", err)
	}
	if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
		_, err := api.MessagesAcceptEncryption(ctx, &tg.MessagesAcceptEncryptionRequest{
			Peer: tg.InputEncryptedChat{ChatID: chatID, AccessHash: secretChatAccessHash(b.id, chatID)},
			GB:   smokeGB(), KeyFingerprint: 1873,
		})
		return err
	}); err != nil {
		t.Fatalf("acceptEncryption: %v", err)
	}
	setBlock := func(on bool) {
		if err := b.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			if on {
				_, err := api.ContactsBlock(ctx, &tg.ContactsBlockRequest{ID: peerUser(b.id, a.id)})
				return err
			}
			_, err := api.ContactsUnblock(ctx, &tg.ContactsUnblockRequest{ID: peerUser(b.id, a.id)})
			return err
		}); err != nil {
			t.Fatalf("B block=%t A: %v", on, err)
		}
	}
	send := func(randomID int64) {
		if err := a.call(f.ctx, func(ctx context.Context, api *tg.Client) error {
			sent, err := api.MessagesSendEncrypted(ctx, &tg.MessagesSendEncryptedRequest{
				Peer:     tg.InputEncryptedChat{ChatID: chatID, AccessHash: secretChatAccessHash(a.id, chatID)},
				RandomID: randomID,
				Data:     []byte("ciphertext"),
			})
			if err != nil {
				return err
			}
			if m, ok := sent.(*tg.MessagesSentEncryptedMessage); !ok || m.Date <= 0 {
				return fmt.Errorf("sendEncrypted = %+v, want a sentEncryptedMessage with a date", sent)
			}
			return nil
		}); err != nil {
			t.Fatalf("sendEncrypted %d: %v", randomID, err)
		}
	}

	setBlock(true)
	send(1873111)
	if smokeEncryptedInDifference(t, f, b)[1873111] {
		t.Fatal("blocker's getDifference shows a withheld encrypted message")
	}

	setBlock(false)
	send(1873112)
	shown := smokeEncryptedInDifference(t, f, b)
	if shown[1873111] || !shown[1873112] {
		t.Fatalf("peer's getDifference after unblock = %v, want 1873112 and not 1873111", shown)
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
	blobs := testBlobs(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store close: %v", err)
		}
	})
	f := &smokeFixture{
		ctx: ctx, failures: newClientFailureSignal(cancelFailure), key: key, dsn: dsn,
		store: st, blobs: blobs, codes: newMultiCodeSink(), dcID: 2,
		regMode: regMode, stickerImportMode: config.StickerImportOn,
		userInviteMinAccountAge: config.DefaultUserInviteMinAccountAge,
		recoveryEmailMode:       config.RecoveryEmailOff, signUpTermsOfService: config.DefaultSignUpTermsOfService,
	}
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
	registry, stop := bootServerWithLimitsAndRegistrationModeAndBlobsAndStatusWithDeliveryHookAndTurnstileAndWelcomeChannelAndStickerCatalog(
		t, f.ctx, f.key, f.dcID, f.store, f.dsn, f.codes.Logger(), ln, f.rateLimits, f.regMode,
		f.blobs, f.persistStatus, nil, f.turnstileSiteKey, f.turnstileVerifier, f.welcomeChannel,
		f.turnstileSignInEnabled, f.recoveryEmailMode, f.suppressChannelPostDelivery, f.signUpTermsOfService,
		f.stickerSets, f.uploadMaxFileBytes, f.stickerImportMode, f.userInviteQuota, f.userInviteMinAccountAge, f.uploadsDisabled, f.photoThumbs,
	)
	f.registry = registry
	f.setServerStop(t.Cleanup, stop)
}

func (f *smokeFixture) restart(t *testing.T) {
	t.Helper()
	f.stop()
	f.start(t, fmt.Sprintf("127.0.0.1:%d", f.port))
}

func (f *smokeFixture) restartWithFreshStore(t *testing.T) {
	t.Helper()
	f.stop()
	if err := f.store.Close(); err != nil {
		t.Fatalf("close store before restart: %v", err)
	}
	st, err := store.Open(f.ctx, f.dsn, pgtest.EncKey(), store.WithBlobStore(f.blobs))
	if err != nil {
		t.Fatalf("reopen store for restart: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close restarted store: %v", err)
		}
	})
	f.store = st
	f.start(t, fmt.Sprintf("127.0.0.1:%d", f.port))
	t.Cleanup(func() { f.stop() })
}

func (f *smokeFixture) managedClientWithDevice(sess *session.StorageMemory, seen, push *updateCollector, manager *updates.Manager, device telegram.DeviceConfig, requests *clientRequestRecorder) *telegram.Client {
	middlewares := []telegram.Middleware{hook.UpdateHook(manager.Handle), hook.AffectedHook(manager)}
	if requests != nil {
		middlewares = append(middlewares, captureClientRequests(requests))
	}
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:             f.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:     []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: sess,
		Device:         device,
		UpdateHandler:  observedManagerHandler{observer: push, manager: manager},
		Middlewares:    middlewares,
	})
}

func (f *smokeFixture) managedClientWithConnectionReadiness(sess *session.StorageMemory, seen, push *updateCollector, manager *updates.Manager, readiness *dialogFilterReadiness) *telegram.Client {
	return telegram.NewClient(1, "hash", telegram.Options{
		DC:                f.dcID,
		DCList:            dcs.List{Options: []tg.DCOption{{ID: f.dcID, IPAddress: "127.0.0.1", Port: f.port}}},
		PublicKeys:        []telegram.PublicKey{{RSA: &f.key.PublicKey}},
		Resolver:          dcs.Plain(dcs.PlainOptions{}),
		SessionStorage:    sess,
		UpdateHandler:     observedManagerHandler{observer: push, manager: manager},
		OnConnectionState: readiness.observe,
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
	cancel    context.CancelFunc
	session   *session.StorageMemory
	manager   *updates.Manager
	requests  *clientRequestRecorder
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
	return newSmokeClientWithDevice(t, f, label, phone, telegram.DeviceConfig{})
}

func newSmokeClientWithRequests(t *testing.T, f *smokeFixture, label, phone string, requests *clientRequestRecorder) *smokeClient {
	t.Helper()
	return newSmokeClientWithDeviceAndRequests(t, f, label, phone, telegram.DeviceConfig{}, requests)
}

func newSmokeClientWithDevice(t *testing.T, f *smokeFixture, label, phone string, device telegram.DeviceConfig) *smokeClient {
	t.Helper()
	return newSmokeClientWithDeviceAndRequests(t, f, label, phone, device, nil)
}

func newSmokeClientWithDeviceAndRequests(t *testing.T, f *smokeFixture, label, phone string, device telegram.DeviceConfig, requests *clientRequestRecorder) *smokeClient {
	t.Helper()
	sess := &session.StorageMemory{}
	clientCtx, cancelClient := context.WithCancel(f.ctx)
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	client := &smokeClient{
		client:   f.managedClientWithDevice(sess, seen, push, manager, device, requests),
		requests: requests,
		cancel:   cancelClient,
		session:  sess,
		manager:  manager,
		seen:     seen,
		push:     push,
		cmds:     make(chan command),
		label:    label,
	}
	username := smokeAuthHandle(f, phone)
	flow := auth.NewFlow(
		auth.Constant(username, smokeUsernamePassword, auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			return f.codes.wait(ctx, username)
		})),
		auth.SendCodeOptions{},
	)
	ids, ready := make(chan int64, 1), make(chan struct{}, 1)
	client.lifecycle = startClientLifecycle(clientCtx, label, f.failures, func(phase *clientPhaseState) error {
		return runManagedInteractive(clientCtx, client.client, flow, ids, ready, client.cmds, manager, true, phase)
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

func newSmokeClientWithConnectionReadiness(t *testing.T, f *smokeFixture, label, phone string, readiness *dialogFilterReadiness) *smokeClient {
	t.Helper()
	sess := &session.StorageMemory{}
	clientCtx, cancelClient := context.WithCancel(f.ctx)
	seen, push := newUpdateCollector(), newUpdateCollector()
	manager := updates.New(updates.Config{Handler: seen})
	client := &smokeClient{
		client:  f.managedClientWithConnectionReadiness(sess, seen, push, manager, readiness),
		cancel:  cancelClient,
		session: sess,
		manager: manager,
		seen:    seen,
		push:    push,
		cmds:    make(chan command),
		label:   label,
	}
	username := smokeAuthHandle(f, phone)
	flow := auth.NewFlow(
		auth.Constant(username, smokeUsernamePassword, auth.CodeAuthenticatorFunc(func(ctx context.Context, _ *tg.AuthSentCode) (string, error) {
			return f.codes.wait(ctx, username)
		})),
		auth.SendCodeOptions{},
	)
	ids, ready := make(chan int64, 1), make(chan struct{}, 1)
	client.lifecycle = startClientLifecycle(clientCtx, label, f.failures, func(phase *clientPhaseState) error {
		return runManagedInteractive(clientCtx, client.client, flow, ids, ready, client.cmds, manager, true, phase)
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
		c.lifecycle.intentionalStop.Store(true)
		if c.cancel != nil {
			c.cancel()
		}
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
	url string
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
		if expect.url != "" {
			if err := smokeURLEntity(message, message.Message, expect.url); err != nil {
				return fmt.Errorf("history %q URL entity: %w", message.Message, err)
			}
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

func assertSmokeURLEntity(t *testing.T, message *tg.Message, text, link string) {
	t.Helper()
	if err := smokeURLEntity(message, text, link); err != nil {
		t.Fatal(err)
	}
}

func smokeURLEntity(message *tg.Message, text, link string) error {
	if message == nil || message.Message != text {
		return fmt.Errorf("message = %#v, want text %q", message, text)
	}
	entities, present := message.GetEntities()
	if !present || len(entities) != 1 {
		return fmt.Errorf("entities = %#v present=%v, want one URL", entities, present)
	}
	urlEntity, ok := entities[0].(*tg.MessageEntityURL)
	if !ok {
		return fmt.Errorf("entity type = %T, want MessageEntityUrl", entities[0])
	}
	prefix, _, ok := strings.Cut(text, link)
	if !ok {
		return fmt.Errorf("message text does not contain URL %q", link)
	}
	wantOffset := len(utf16.Encode([]rune(prefix)))
	wantLength := len(utf16.Encode([]rune(link)))
	if urlEntity.Offset != wantOffset || urlEntity.Length != wantLength {
		return fmt.Errorf("URL span = (%d,%d), want (%d,%d)", urlEntity.Offset, urlEntity.Length, wantOffset, wantLength)
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

func assertSmokeChannelPinUpdate(t *testing.T, update *tg.UpdatePinnedChannelMessages, wantPinned bool, channelID int64, postID, wantPts int) {
	t.Helper()
	if update.Pinned != wantPinned || update.ChannelID != channelID || len(update.Messages) != 1 || update.Messages[0] != postID {
		t.Fatalf("channel pin update = %+v, want pinned=%t channel %d post %d", update, wantPinned, channelID, postID)
	}
	if update.Pts != wantPts || update.PtsCount != 1 {
		t.Fatalf("channel pin update pts = %d/%d, want %d/1", update.Pts, update.PtsCount, wantPts)
	}
}

func verifySmokeChannelPin(ctx context.Context, api *tg.Client, viewerID, channelID int64, wantID int, wantPinned bool) error {
	result, err := api.ChannelsGetFullChannel(ctx, inputChannel(viewerID, channelID))
	if err != nil {
		return fmt.Errorf("getFullChannel: %w", err)
	}
	full, ok := result.FullChat.(*tg.ChannelFull)
	if !ok {
		return fmt.Errorf("full chat = %T, want *tg.ChannelFull", result.FullChat)
	}
	pinnedID, ok := full.GetPinnedMsgID()
	if wantPinned && (!ok || pinnedID != wantID) {
		return fmt.Errorf("full channel pinned id = %d/%t, want %d", pinnedID, ok, wantID)
	}
	if !wantPinned && ok && pinnedID != 0 {
		return fmt.Errorf("full channel pinned id = %d after unpin, want none", pinnedID)
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
	if update.Pts <= 0 || update.PtsCount != 1 {
		t.Fatalf("pin update pts = %d/%d, want a recorded event with pts_count 1", update.Pts, update.PtsCount)
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
