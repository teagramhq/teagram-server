package main

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRegistrationModesStayBoundToListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	st, err := store.Open(ctx, pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	const dcID = 2
	log := slog.New(slog.DiscardHandler)
	cfg := config.Config{
		DCID:                          dcID,
		RegistrationMode:              config.RegistrationOpen,
		Turnstile:                     config.TurnstileConfig{SiteKey: "site-key", SecretKey: "secret-key", SignInEnabled: true},
		PublicWebSocketListenAddr:     "127.0.0.1:0",
		PublicRegistrationMode:        config.RegistrationInvite,
		PublicWebSocketOriginPatterns: []string{"https://web.teagram.io"},
		MaxFileBytes:                  100 << 20,
		MaxUserStorageBytes:           2 << 30,
		UploadPartTTL:                 config.DefaultUploadPartTTL,
	}
	tgcfg := api.DefaultConfig(dcID, "127.0.0.1", 0)
	type turnstileScope struct {
		signUp bool
		signIn bool
	}
	var turnstileScopes []turnstileScope
	dialogFilterSync := api.NewDialogFilterSync()
	newHandler := func(mode config.RegistrationMode, signUpEnabled, signInEnabled bool) mtproto.Handler {
		turnstileScopes = append(turnstileScopes, turnstileScope{signUp: signUpEnabled, signIn: signInEnabled})
		var siteKey string
		var verifier api.SignUpTurnstileVerifier
		if signUpEnabled {
			siteKey = cfg.Turnstile.SiteKey
			verifier = registrationTestTurnstileVerifier{}
		}
		return api.NewWithPhotoThumbsAndDialogFilterSyncAndUploadPartTTLAndTurnstileAndWelcomeChannelAndSignIn(
			st, dcID, tgcfg, log, false, cfg.MaxFileBytes, blobs, cfg.MaxUserStorageBytes,
			pgtest.PeerDeriver(), pgtest.PhotoDeriver(), cfg.RateLimits, mode, dialogFilterSync, nil,
			cfg.UploadPartTTL, siteKey, verifier, "", signInEnabled, config.RecoveryEmailOff,
		)
	}
	server, publicHandler := newListenerScopedServer(cfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)
	if len(turnstileScopes) != 2 || turnstileScopes[0] != (turnstileScope{}) || turnstileScopes[1] != (turnstileScope{signUp: true, signIn: true}) {
		t.Fatalf("listener Turnstile scopes = %v, want tailnet disabled and public signup/signin enabled", turnstileScopes)
	}
	turnstileScopes = nil
	singleListenerCfg := cfg
	singleListenerCfg.PublicWebSocketListenAddr = ""
	newListenerScopedServer(singleListenerCfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)
	if len(turnstileScopes) != 1 || turnstileScopes[0] != (turnstileScope{signUp: true, signIn: true}) {
		t.Fatalf("single-listener Turnstile scopes = %v, want public signup/signin enabled", turnstileScopes)
	}
	turnstileScopes = nil
	singleListenerCfg.Turnstile.SignInEnabled = false
	newListenerScopedServer(singleListenerCfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)
	if len(turnstileScopes) != 1 || turnstileScopes[0] != (turnstileScope{signUp: true}) {
		t.Fatalf("inactive single-listener Turnstile scopes = %v, want signup enabled and signin disabled", turnstileScopes)
	}
	clientDiscovery, err := discovery.NewDocument("127.0.0.1:2443", dcID, &key.PublicKey, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.SetClientDiscoveryDocument(clientDiscovery); err != nil {
		t.Fatal(err)
	}

	tailnetListener := listenRegistrationTestTCP(t)
	publicListener := listenRegistrationTestTCP(t)
	public := newPublicWebSocketListener(cfg, publicListener, publicHandler, clientDiscovery)
	serveCtx, stopServing := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveMTProto(serveCtx, server, tailnetListener, nil, public)
	}()
	t.Cleanup(func() {
		stopServing()
		closeRegistrationTestListener(t, tailnetListener)
		closeRegistrationTestListener(t, publicListener)
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("serve listeners: %v", err)
		}
	})

	stamp := time.Now().UnixNano()
	t.Run("tailnet TCP", func(t *testing.T) {
		client := registrationTestClient(t, key, dcID, tailnetListener, false, config.RegistrationInvite)
		if err := client.Run(ctx, func(ctx context.Context) error {
			return assertRegistrationClientPolicy(ctx, client, fmt.Sprintf("dualtcp%d", stamp), config.RegistrationOpen, true, false)
		}); err != nil {
			t.Fatalf("tailnet registration flow: %v", err)
		}
	})
	t.Run("public WebSocket", func(t *testing.T) {
		historySession := &session.StorageMemory{}
		historyClient := registrationTestClientWithStorage(t, key, dcID, tailnetListener, false, config.RegistrationInvite, historySession)
		historyIdentifier := fmt.Sprintf("history%d", stamp)
		if err := historyClient.Run(ctx, func(ctx context.Context) error {
			_, err := historyClient.API().AuthSendCode(ctx, &tg.AuthSendCodeRequest{PhoneNumber: historyIdentifier, APIID: 1, APIHash: "hash"})
			if err != nil {
				return fmt.Errorf("tailnet auth.sendCode before listener change: %w", err)
			}
			return nil
		}); err != nil {
			t.Fatalf("tailnet auth-key history: %v", err)
		}
		historyData, err := (&session.Loader{Storage: historySession}).Load(ctx)
		if err != nil {
			t.Fatalf("load tailnet client session: %v", err)
		}
		if len(historyData.AuthKeyID) != 8 {
			t.Fatalf("tailnet session auth key ID length = %d, want 8", len(historyData.AuthKeyID))
		}
		var historyAuthKeyID [8]byte
		copy(historyAuthKeyID[:], historyData.AuthKeyID)
		storedKey, ok, err := st.AuthKeyByID(ctx, mtproto.AuthKeyIDInt64(historyAuthKeyID))
		if err != nil || !ok {
			t.Fatalf("tailnet auth key in shared store: ok=%t err=%v", ok, err)
		}
		if storedKey.UserID != 0 || storedKey.Provisional {
			t.Fatalf("tailnet auth key state = user %d, provisional %t; want unbound history", storedKey.UserID, storedKey.Provisional)
		}

		reusedClient := registrationTestClientWithStorage(t, key, dcID, publicListener, true, config.RegistrationOpen, historySession)
		if err := reusedClient.Run(ctx, func(ctx context.Context) error {
			return assertSignInTurnstileChallenge(ctx, reusedClient, historyIdentifier)
		}); err != nil {
			t.Fatalf("public listener reused auth-key challenge: %v", err)
		}

		client := registrationTestClient(t, key, dcID, publicListener, true, config.RegistrationOpen)
		if err := client.Run(ctx, func(ctx context.Context) error {
			return assertRegistrationClientPolicy(ctx, client, fmt.Sprintf("dualws%d", stamp), config.RegistrationInvite, false, true)
		}); err != nil {
			t.Fatalf("public registration flow: %v", err)
		}
	})
}

func listenRegistrationTestTCP(t *testing.T) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { closeRegistrationTestListener(t, listener) })
	return listener
}

func closeRegistrationTestListener(t *testing.T, listener net.Listener) {
	t.Helper()
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Errorf("close listener: %v", err)
	}
}

func registrationTestClient(t *testing.T, key *rsa.PrivateKey, dcID int, listener net.Listener, websocket bool, spoofMode config.RegistrationMode) *telegram.Client {
	t.Helper()
	return registrationTestClientWithStorage(t, key, dcID, listener, websocket, spoofMode, &session.StorageMemory{})
}

func registrationTestClientWithStorage(t *testing.T, key *rsa.PrivateKey, dcID int, listener net.Listener, websocket bool, spoofMode config.RegistrationMode, storage *session.StorageMemory) *telegram.Client {
	t.Helper()
	options := telegram.Options{
		DC:         dcID,
		PublicKeys: []telegram.PublicKey{{RSA: &key.PublicKey}},
		Device: telegram.DeviceConfig{Params: &tg.JSONObject{Value: []tg.JSONObjectValue{{
			Key:   "registration_mode",
			Value: &tg.JSONString{Value: string(spoofMode)},
		}}}},
		SessionStorage: storage,
	}
	if websocket {
		options.DCList = dcs.List{Domains: map[int]string{
			dcID: fmt.Sprintf("ws://%s/apiws", listener.Addr()),
		}}
		options.Resolver = dcs.Websocket(dcs.WebsocketOptions{})
	} else {
		addr, ok := listener.Addr().(*net.TCPAddr)
		if !ok {
			t.Fatalf("listener address = %T, want *net.TCPAddr", listener.Addr())
		}
		options.DCList = dcs.List{Options: []tg.DCOption{{ID: dcID, IPAddress: "127.0.0.1", Port: addr.Port}}}
		options.Resolver = dcs.Plain(dcs.PlainOptions{})
	}
	return telegram.NewClient(1, "hash", options)
}

func assertSignInTurnstileChallenge(ctx context.Context, client *telegram.Client, identifier string) error {
	_, err := client.API().AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: identifier,
		APIID:       1,
		APIHash:     "hash",
	})
	if err == nil {
		return errors.New("auth.sendCode with prior auth-key history succeeded, want RECAPTCHA_CHECK_signin__site-key")
	}
	if !registrationRPCMessage(err, "RECAPTCHA_CHECK_signin__site-key") {
		return fmt.Errorf("auth.sendCode with prior auth-key history: %w", err)
	}
	return nil
}

func assertRegistrationClientPolicy(ctx context.Context, client *telegram.Client, username string, wantMode config.RegistrationMode, wantSignUp, wantTurnstile bool) error {
	appConfig, err := client.API().HelpGetAppConfig(ctx, 0)
	if err != nil {
		return fmt.Errorf("help.getAppConfig: %w", err)
	}
	mode, err := registrationModeFromAppConfig(appConfig)
	if err != nil {
		return err
	}
	if mode != wantMode {
		return fmt.Errorf("help.getAppConfig registration_mode = %q, want %q", mode, wantMode)
	}

	sendCodeRequest := &tg.AuthSendCodeRequest{
		PhoneNumber: username,
		APIID:       1,
		APIHash:     "hash",
	}
	sent, err := client.API().AuthSendCode(ctx, sendCodeRequest)
	if wantTurnstile {
		if err == nil {
			return errors.New("bare auth.sendCode succeeded, want RECAPTCHA_CHECK_signin__site-key")
		}
		if !registrationRPCMessage(err, "RECAPTCHA_CHECK_signin__site-key") {
			return fmt.Errorf("bare auth.sendCode: %w", err)
		}
		sent, err = sendCodeWithTurnstile(ctx, client, sendCodeRequest, registrationSignInToken)
	}
	if err != nil {
		return fmt.Errorf("auth.sendCode: %w", err)
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return fmt.Errorf("auth.sendCode response = %T, want *tg.AuthSentCode", sent)
	}
	signIn, err := client.API().AuthSignIn(ctx, &tg.AuthSignInRequest{
		PhoneNumber:   username,
		PhoneCodeHash: code.PhoneCodeHash,
	})
	if err != nil {
		if !registrationRPCMessage(err, "SIGN_UP_REQUIRED") {
			return fmt.Errorf("auth.signIn: expected SIGN_UP_REQUIRED, got %w", err)
		}
	} else if _, ok := signIn.(*tg.AuthAuthorizationSignUpRequired); !ok {
		return fmt.Errorf("auth.signIn response = %T, want *tg.AuthAuthorizationSignUpRequired", signIn)
	}

	signUpRequest := &tg.AuthSignUpRequest{
		PhoneNumber:   username,
		PhoneCodeHash: code.PhoneCodeHash,
		FirstName:     "Listener",
	}
	var authorization tg.AuthAuthorizationClass
	if wantTurnstile {
		var result tg.AuthAuthorizationBox
		err = client.API().Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{
			Token: registrationSignUpToken,
			Query: signUpRequest,
		}, &result)
		authorization = result.Authorization
	} else {
		authorization, err = client.API().AuthSignUp(ctx, signUpRequest)
	}
	if wantSignUp {
		if err != nil {
			return fmt.Errorf("auth.signUp: %w", err)
		}
		if _, ok := authorization.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("auth.signUp response = %T, want *tg.AuthAuthorization", authorization)
		}
		return nil
	}
	if err == nil {
		return errors.New("auth.signUp succeeded, want INVITE_HASH_INVALID")
	}
	if !registrationRPCMessage(err, "INVITE_HASH_INVALID") {
		return fmt.Errorf("auth.signUp: %w, want INVITE_HASH_INVALID", err)
	}
	return nil
}

const (
	registrationSignInToken = "registration-signin-token"
	registrationSignUpToken = "registration-signup-token"
)

type registrationTestTurnstileVerifier struct{}

func (registrationTestTurnstileVerifier) Verify(_ context.Context, token, action, _ string) error {
	switch {
	case action == "signin" && token == registrationSignInToken:
		return nil
	case action == "signup" && token == registrationSignUpToken:
		return nil
	default:
		return fmt.Errorf("unexpected Turnstile action %q or token", action)
	}
}

func sendCodeWithTurnstile(ctx context.Context, client *telegram.Client, request *tg.AuthSendCodeRequest, token string) (tg.AuthSentCodeClass, error) {
	var result tg.AuthSentCodeBox
	if err := client.API().Invoker().Invoke(ctx, &tg.InvokeWithReCaptchaRequest{Token: token, Query: request}, &result); err != nil {
		return nil, err
	}
	return result.SentCode, nil
}

func registrationModeFromAppConfig(appConfig tg.HelpAppConfigClass) (config.RegistrationMode, error) {
	got, ok := appConfig.(*tg.HelpAppConfig)
	if !ok {
		return "", fmt.Errorf("help.getAppConfig response = %T, want *tg.HelpAppConfig", appConfig)
	}
	object, ok := got.Config.(*tg.JSONObject)
	if !ok {
		return "", fmt.Errorf("help.getAppConfig config = %T, want *tg.JSONObject", got.Config)
	}
	for _, value := range object.Value {
		if value.Key == "registration_mode" {
			mode, ok := value.Value.(*tg.JSONString)
			if !ok {
				return "", fmt.Errorf("registration_mode = %T, want *tg.JSONString", value.Value)
			}
			return config.RegistrationMode(mode.Value), nil
		}
	}
	return "", errors.New("help.getAppConfig has no registration_mode")
}

func registrationRPCMessage(err error, want string) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Message == want
}
