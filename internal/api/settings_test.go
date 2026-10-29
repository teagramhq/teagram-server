package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"slices"
	"sync"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/mt"
	"github.com/gotd/td/proto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

type settingsHandler struct {
	name                  string
	handle                func(int64) (bin.Encoder, error)
	request               func() bin.Encoder
	response              func() bin.Decoder
	assert                func(*testing.T, bin.Decoder)
	requiresAuthorization bool
}

func assertAppConfig(t *testing.T, got *tg.HelpAppConfig, mode config.RegistrationMode) {
	t.Helper()
	if got.Hash == 8 {
		t.Fatalf("app config hash = %d, want a hash that includes the effective upload cap", got.Hash)
	}
	object, ok := got.Config.(*tg.JSONObject)
	if !ok {
		t.Fatalf("app config = %T with %v, want JSON object", got.Config, got.Config)
	}
	values := make(map[string]any, len(object.Value))
	for _, value := range object.Value {
		if _, exists := values[value.Key]; exists {
			t.Fatalf("duplicate app config key %q", value.Key)
		}
		values[value.Key] = value.Value
	}
	if len(values) != 32 {
		t.Fatalf("app config has %d keys, want 32", len(values))
	}
	if value, ok := values["upload_max_file_bytes"].(*tg.JSONNumber); !ok || value.Value != float64(api.TestMaxFileBytes) {
		t.Fatalf("upload_max_file_bytes = %v, want %d", values["upload_max_file_bytes"], api.TestMaxFileBytes)
	}
	if _, ok := values["password_min_length"]; ok {
		t.Fatal("app config advertised a password minimum without a server rule")
	}
	if value, ok := values["dialog_filters_enabled"].(*tg.JSONBool); !ok || !value.Value {
		t.Fatalf("dialog_filters_enabled = %v, want true", values["dialog_filters_enabled"])
	}
	if value, ok := values["dialog_filters_limit_default"].(*tg.JSONNumber); !ok || value.Value != 10 {
		t.Fatalf("dialog_filters_limit_default = %v, want 10", values["dialog_filters_limit_default"])
	}
	if value, ok := values["dialog_filters_chats_limit_default"].(*tg.JSONNumber); !ok || value.Value != 100 {
		t.Fatalf("dialog_filters_chats_limit_default = %v, want 100", values["dialog_filters_chats_limit_default"])
	}
	if value, ok := values["dialog_filters_tooltip"].(*tg.JSONBool); !ok || value.Value {
		t.Fatalf("dialog_filters_tooltip = %v, want false", values["dialog_filters_tooltip"])
	}
	if value, ok := values["dialogs_folder_pinned_limit_default"].(*tg.JSONNumber); !ok || value.Value != 100 {
		t.Fatalf("dialogs_folder_pinned_limit_default = %v, want 100", values["dialogs_folder_pinned_limit_default"])
	}
	if value, ok := values["dialogs_pinned_limit_default"].(*tg.JSONNumber); !ok || value.Value != 5 {
		t.Fatalf("dialogs_pinned_limit_default = %v, want 5", values["dialogs_pinned_limit_default"])
	}
	if value, ok := values["chat_read_mark_size_threshold"].(*tg.JSONNumber); !ok || value.Value != 100 {
		t.Fatalf("chat_read_mark_size_threshold = %v, want 100", values["chat_read_mark_size_threshold"])
	}
	if value, ok := values["chat_read_mark_expire_period"].(*tg.JSONNumber); !ok || value.Value != 604800 {
		t.Fatalf("chat_read_mark_expire_period = %v, want 604800", values["chat_read_mark_expire_period"])
	}
	if value, ok := values["upload_max_fileparts_default"].(*tg.JSONNumber); !ok || value.Value != 3200 {
		t.Fatalf("upload_max_fileparts_default = %v, want 3200", values["upload_max_fileparts_default"])
	}
	if value, ok := values["upload_max_fileparts_premium"].(*tg.JSONNumber); !ok || value.Value != 3200 {
		t.Fatalf("upload_max_fileparts_premium = %v, want 3200", values["upload_max_fileparts_premium"])
	}
	if value, ok := values["registration_mode"].(*tg.JSONString); !ok || value.Value != string(mode) {
		t.Fatalf("registration_mode = %v, want %q", values["registration_mode"], mode)
	}
	if value, ok := values["user_invites_enabled"].(*tg.JSONBool); !ok || value.Value {
		t.Fatalf("user_invites_enabled = %v, want false when test quota is unset", values["user_invites_enabled"])
	}
	if value, ok := values["user_invite_quota"].(*tg.JSONNumber); !ok || value.Value != 0 {
		t.Fatalf("user_invite_quota = %v, want 0 when test quota is unset", values["user_invite_quota"])
	}
	if value, ok := values["sticker_import_mode"].(*tg.JSONString); !ok || value.Value != "on" {
		t.Fatalf("sticker_import_mode = %v, want %q", values["sticker_import_mode"], "on")
	}
	for key, want := range map[string]float64{
		"username_min_length":      2,
		"username_max_length":      32,
		"display_name_max_length":  64,
		"bio_max_length":           70,
		"group_title_min_length":   1,
		"group_title_max_length":   255,
		"group_about_max_length":   255,
		"channel_title_min_length": 1,
		"channel_title_max_length": 255,
		"channel_about_max_length": 255,
		"message_max_length":       4096,
		"caption_max_length":       1024,
		"upload_max_part_bytes":    512 * 1024,
	} {
		if value, ok := values[key].(*tg.JSONNumber); !ok || value.Value != want {
			t.Errorf("%s = %v, want %g", key, values[key], want)
		}
	}
	if value, ok := values["username_pattern"].(*tg.JSONString); !ok || value.Value != `^[a-zA-Z][a-zA-Z0-9_]{1,31}$` {
		t.Errorf("username_pattern = %v, want %q", values["username_pattern"], `^[a-zA-Z][a-zA-Z0-9_]{1,31}$`)
	}
}

func settingsHandlers() []settingsHandler {
	return []settingsHandler{
		{
			name:                  "content settings",
			handle:                api.GetContentSettingsForTest,
			request:               func() bin.Encoder { return &tg.AccountGetContentSettingsRequest{} },
			response:              func() bin.Decoder { return &tg.AccountContentSettings{} },
			requiresAuthorization: true,
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountContentSettings)
				if !ok {
					t.Fatalf("response = %T, want *tg.AccountContentSettings", response)
				}
				if got.SensitiveEnabled || got.SensitiveCanChange {
					t.Fatal("content settings advertise unsupported sensitive content")
				}
			},
		},
		{
			name:                  "global privacy settings",
			handle:                api.GetGlobalPrivacySettingsForTest,
			request:               func() bin.Encoder { return &tg.AccountGetGlobalPrivacySettingsRequest{} },
			response:              func() bin.Decoder { return &tg.GlobalPrivacySettings{} },
			requiresAuthorization: true,
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.GlobalPrivacySettings)
				if !ok {
					t.Fatalf("response = %T, want *tg.GlobalPrivacySettings", response)
				}
				if got.Flags != 0 {
					t.Fatalf("global privacy settings flags = %#x, want none", got.Flags)
				}
			},
		},
		{
			name:                  "themes",
			handle:                api.GetThemesForTest,
			request:               func() bin.Encoder { return &tg.AccountGetThemesRequest{} },
			response:              func() bin.Decoder { return &tg.AccountThemes{} },
			requiresAuthorization: true,
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.AccountThemes)
				if !ok {
					t.Fatalf("response = %T, want *tg.AccountThemes", response)
				}
				if got.Hash != 0 || len(got.Themes) != 0 {
					t.Fatalf("themes = hash %d, %d themes; want empty", got.Hash, len(got.Themes))
				}
			},
		},
		{
			name: "app config",
			handle: func(userID int64) (bin.Encoder, error) {
				return api.GetAppConfigForTestWithMode(userID, config.RegistrationInvite)
			},
			request:  func() bin.Encoder { return &tg.HelpGetAppConfigRequest{} },
			response: func() bin.Decoder { return &tg.HelpAppConfig{} },
			assert: func(t *testing.T, response bin.Decoder) {
				t.Helper()
				got, ok := response.(*tg.HelpAppConfig)
				if !ok {
					t.Fatalf("response = %T, want *tg.HelpAppConfig", response)
				}
				assertAppConfig(t, got, config.RegistrationInvite)
			},
		},
	}
}

func TestSettingsHandlersRequireAuthorization(t *testing.T) {
	for _, method := range settingsHandlers() {
		t.Run(method.name, func(t *testing.T) {
			if !method.requiresAuthorization {
				return
			}
			res, err := method.handle(0)
			if res != nil {
				t.Fatalf("response = %T, want nil", res)
			}
			var rpc *tgerr.Error
			if !errors.As(err, &rpc) || rpc.Message != "AUTH_KEY_UNREGISTERED" {
				t.Fatalf("error = %v, want AUTH_KEY_UNREGISTERED", err)
			}
		})
	}
}

func TestSettingsHandlersReturnHonestDefaults(t *testing.T) {
	for _, method := range settingsHandlers() {
		t.Run(method.name, func(t *testing.T) {
			res, err := method.handle(1)
			if err != nil {
				t.Fatalf("handler: %v", err)
			}
			var encoded bin.Buffer
			if err := res.Encode(&encoded); err != nil {
				t.Fatalf("response does not encode: %v", err)
			}

			switch got := res.(type) {
			case *tg.AccountContentSettings:
				if got.SensitiveEnabled || got.SensitiveCanChange {
					t.Fatal("content settings advertise unsupported sensitive content")
				}
			case *tg.GlobalPrivacySettings:
				if got.Flags != 0 {
					t.Fatalf("global privacy settings flags = %#x, want none", got.Flags)
				}
			case *tg.AccountThemes:
				if got.Hash != 0 || len(got.Themes) != 0 {
					t.Fatalf("themes = hash %d, %d themes; want empty", got.Hash, len(got.Themes))
				}
			case *tg.HelpAppConfig:
				assertAppConfig(t, got, config.RegistrationInvite)
			default:
				t.Fatalf("response = %T, want one of the settings success types", res)
			}
		})
	}
}

type settingsDispatcherTransport struct {
	mu          sync.Mutex
	sent        [][]byte
	sendErr     error
	sendEntered chan struct{}
	sendRelease chan struct{}
}

func (t *settingsDispatcherTransport) Send(ctx context.Context, b *bin.Buffer) error {
	if t.sendEntered != nil {
		t.sendEntered <- struct{}{}
	}
	if t.sendRelease != nil {
		select {
		case <-t.sendRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sendErr != nil {
		return t.sendErr
	}
	t.sent = append(t.sent, slices.Clone(b.Buf))
	return nil
}

func (*settingsDispatcherTransport) Recv(context.Context, *bin.Buffer) error { return io.EOF }
func (*settingsDispatcherTransport) Close() error                            { return nil }

func (transport *settingsDispatcherTransport) result(t *testing.T, key crypto.AuthKey, msgID int64) []byte {
	t.Helper()
	transport.mu.Lock()
	frames := slices.Clone(transport.sent)
	transport.mu.Unlock()

	cipher := crypto.NewClientCipher(crypto.DefaultRand())
	for _, frame := range frames {
		message := &crypto.EncryptedMessage{}
		if err := message.DecodeWithoutCopy(&bin.Buffer{Buf: frame}); err != nil {
			t.Fatalf("decode server frame: %v", err)
		}
		decrypted, err := cipher.Decrypt(key, message)
		if err != nil {
			t.Fatalf("decrypt server frame: %v", err)
		}
		var result proto.Result
		if err := result.Decode(&bin.Buffer{Buf: decrypted.Data()}); err != nil {
			continue
		}
		if result.RequestMessageID == msgID {
			return slices.Clone(result.Result)
		}
	}
	t.Fatalf("no RPC result for msg id %d in %d replies", msgID, len(frames))
	return nil
}

func dispatchSettings(t *testing.T, h mtproto.Handler, method settingsHandler, userID int64, provisional bool) []byte {
	t.Helper()
	return dispatchSettingsWithContext(t, h, method, userID, provisional, context.Background())
}

func dispatchSettingsWithContext(t *testing.T, h mtproto.Handler, method settingsHandler, userID int64, provisional bool, ctx context.Context) []byte {
	t.Helper()
	return dispatchSettingsWithContextAndAddr(t, h, method, userID, provisional, ctx, netip.Addr{})
}

func dispatchSettingsWithAddr(t *testing.T, h mtproto.Handler, method settingsHandler, userID int64, provisional bool, addr netip.Addr) []byte {
	t.Helper()
	return dispatchSettingsWithContextAndAddr(t, h, method, userID, provisional, context.Background(), addr)
}

func dispatchSettingsWithContextAndAddr(t *testing.T, h mtproto.Handler, method settingsHandler, userID int64, provisional bool, ctx context.Context, addr netip.Addr) []byte {
	t.Helper()
	key := testKey()
	transport := &settingsDispatcherTransport{}
	conn := mtproto.NewTestConn(transport, key)
	var body bin.Buffer
	if err := method.request().Encode(&body); err != nil {
		t.Fatalf("encode %s request: %v", method.name, err)
	}
	const msgID = int64(1 << 32)
	if err := h.OnMessage(conn, &mtproto.Request{
		AuthKeyID:   key.ID,
		UserID:      userID,
		Provisional: provisional,
		ClientAddr:  addr,
		MsgID:       msgID,
		Buf:         &body,
		Ctx:         ctx,
	}); err != nil {
		t.Fatalf("dispatch %s: %v", method.name, err)
	}
	return transport.result(t, key, msgID)
}

func TestSettingsHandlersThroughDispatcher(t *testing.T) {
	h := api.New(
		nil,
		2,
		&tg.Config{},
		slog.New(slog.DiscardHandler),
		false,
		100<<20,
		nil,
		1,
		pgtest.PeerDeriver(), pgtest.PhotoDeriver(),
		config.RateLimitsConfig{},
		config.RegistrationInvite,
	)

	for _, method := range settingsHandlers() {
		t.Run(method.name, func(t *testing.T) {
			t.Run("authorized", func(t *testing.T) {
				body := dispatchSettings(t, h, method, 1, false)
				response := method.response()
				if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
					t.Fatalf("decode authorized response: %v", err)
				}
				method.assert(t, response)
			})

			for _, session := range []struct {
				name        string
				userID      int64
				provisional bool
			}{
				{name: "unauthenticated"},
				{name: "provisional", userID: 1, provisional: true},
			} {
				t.Run(session.name, func(t *testing.T) {
					if !method.requiresAuthorization {
						body := dispatchSettings(t, h, method, session.userID, session.provisional)
						response := method.response()
						if err := response.Decode(&bin.Buffer{Buf: body}); err != nil {
							t.Fatalf("decode allowed response: %v", err)
						}
						method.assert(t, response)
						return
					}
					body := dispatchSettings(t, h, method, session.userID, session.provisional)
					rpc := &mt.RPCError{}
					if err := rpc.Decode(&bin.Buffer{Buf: body}); err != nil {
						t.Fatalf("decode rejection: %v", err)
					}
					if rpc.ErrorCode != 401 || rpc.ErrorMessage != "AUTH_KEY_UNREGISTERED" {
						t.Fatalf("rejection = %d %q, want 401 AUTH_KEY_UNREGISTERED", rpc.ErrorCode, rpc.ErrorMessage)
					}
				})
			}
		})
	}
}

func TestAppConfigAdvertisesFoldersAndRegistrationModeWithoutAuthorization(t *testing.T) {
	t.Parallel()

	for _, mode := range []config.RegistrationMode{
		config.RegistrationClosed,
		config.RegistrationOpen,
	} {
		t.Run(string(mode), func(t *testing.T) {
			for _, request := range []struct {
				name string
				hash int
			}{
				{name: "uncached", hash: 0},
				{name: "cached", hash: 1},
				{name: "previous config", hash: 2},
			} {
				t.Run(request.name, func(t *testing.T) {
					res, err := api.GetAppConfigForTestWithModeAndHash(0, mode, request.hash)
					if err != nil {
						t.Fatalf("help.getAppConfig: %v", err)
					}
					appConfig, ok := res.(*tg.HelpAppConfig)
					if !ok {
						t.Fatalf("response = %T, want full *tg.HelpAppConfig", res)
					}
					assertAppConfig(t, appConfig, mode)
				})
			}
		})
	}
}

func TestAppConfigAdvertisesUserInvitesForTheAcceptingListener(t *testing.T) {
	t.Parallel()
	hashes := make(map[int]string)
	for _, tc := range []struct {
		name    string
		mode    config.RegistrationMode
		quota   int
		enabled bool
	}{
		{name: "invite mode with quota", mode: config.RegistrationInvite, quota: 5, enabled: true},
		{name: "invite mode disabled quota", mode: config.RegistrationInvite},
		{name: "open mode", mode: config.RegistrationOpen, quota: 5},
		{name: "closed mode", mode: config.RegistrationClosed, quota: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := api.GetAppConfigForTestWithInviteSettings(0, tc.mode, tc.quota)
			if err != nil {
				t.Fatalf("help.getAppConfig: %v", err)
			}
			got, ok := result.(*tg.HelpAppConfig)
			if !ok {
				t.Fatalf("response = %T, want *tg.HelpAppConfig", result)
			}
			if previous, exists := hashes[got.Hash]; exists && previous != tc.name {
				t.Errorf("listener app config hash %d is shared by %q and %q", got.Hash, previous, tc.name)
			}
			hashes[got.Hash] = tc.name
			object, ok := got.Config.(*tg.JSONObject)
			if !ok {
				t.Fatalf("config = %T, want JSON object", got.Config)
			}
			values := make(map[string]any, len(object.Value))
			for _, value := range object.Value {
				values[value.Key] = value.Value
			}
			if value, ok := values["user_invites_enabled"].(*tg.JSONBool); !ok || value.Value != tc.enabled {
				t.Errorf("user_invites_enabled = %v, want %v", values["user_invites_enabled"], tc.enabled)
			}
			if value, ok := values["user_invite_quota"].(*tg.JSONNumber); !ok || value.Value != float64(tc.quota) {
				t.Errorf("user_invite_quota = %v, want %d", values["user_invite_quota"], tc.quota)
			}
			for _, key := range []string{"user_invite_weekly_count", "user_invite_next_available_at", "inviter_user_id"} {
				if _, ok := values[key]; ok {
					t.Errorf("app config exposed user-specific invite value %q", key)
				}
			}
		})
	}
}
