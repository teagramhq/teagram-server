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
		PublicWebSocketListenAddr:     "127.0.0.1:0",
		PublicRegistrationMode:        config.RegistrationInvite,
		PublicWebSocketOriginPatterns: []string{"https://web.teagram.io"},
		MaxFileBytes:                  100 << 20,
		MaxUserStorageBytes:           2 << 30,
		UploadPartTTL:                 config.DefaultUploadPartTTL,
	}
	tgcfg := api.DefaultConfig(dcID, "127.0.0.1", 0)
	newHandler := func(mode config.RegistrationMode) mtproto.Handler {
		return api.New(st, dcID, tgcfg, log, false, cfg.MaxFileBytes, blobs,
			cfg.MaxUserStorageBytes, pgtest.PeerDeriver(), pgtest.PhotoDeriver(), cfg.RateLimits, mode)
	}
	server, publicHandler := newListenerScopedServer(cfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)

	tailnetListener := listenRegistrationTestTCP(t)
	publicListener := listenRegistrationTestTCP(t)
	public := newPublicWebSocketListener(cfg, publicListener, publicHandler)
	serveCtx, stopServing := context.WithCancel(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveMTProto(serveCtx, server, tailnetListener, nil, public)
	}()
	t.Cleanup(func() {
		stopServing()
		_ = tailnetListener.Close()
		_ = publicListener.Close()
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("serve listeners: %v", err)
		}
	})

	stamp := time.Now().UnixNano()
	t.Run("tailnet TCP", func(t *testing.T) {
		client := registrationTestClient(t, key, dcID, tailnetListener, false, config.RegistrationInvite)
		if err := client.Run(ctx, func(ctx context.Context) error {
			return assertRegistrationClientPolicy(ctx, client, fmt.Sprintf("dualtcp%d", stamp), config.RegistrationOpen, true)
		}); err != nil {
			t.Fatalf("tailnet registration flow: %v", err)
		}
	})
	t.Run("public WebSocket", func(t *testing.T) {
		client := registrationTestClient(t, key, dcID, publicListener, true, config.RegistrationOpen)
		if err := client.Run(ctx, func(ctx context.Context) error {
			return assertRegistrationClientPolicy(ctx, client, fmt.Sprintf("dualws%d", stamp), config.RegistrationInvite, false)
		}); err != nil {
			t.Fatalf("public registration flow: %v", err)
		}
	})
}

func listenRegistrationTestTCP(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

func registrationTestClient(t *testing.T, key *rsa.PrivateKey, dcID int, listener net.Listener, websocket bool, spoofMode config.RegistrationMode) *telegram.Client {
	t.Helper()
	options := telegram.Options{
		DC:         dcID,
		PublicKeys: []telegram.PublicKey{{RSA: &key.PublicKey}},
		Device: telegram.DeviceConfig{Params: &tg.JSONObject{Value: []tg.JSONObjectValue{{
			Key:   "registration_mode",
			Value: &tg.JSONString{Value: string(spoofMode)},
		}}}},
		SessionStorage: &session.StorageMemory{},
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

func assertRegistrationClientPolicy(ctx context.Context, client *telegram.Client, username string, wantMode config.RegistrationMode, wantSignUp bool) error {
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

	sent, err := client.API().AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: username,
		APIID:       1,
		APIHash:     "hash",
	})
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

	authorization, err := client.API().AuthSignUp(ctx, &tg.AuthSignUpRequest{
		PhoneNumber:   username,
		PhoneCodeHash: code.PhoneCodeHash,
		FirstName:     "Listener",
	})
	if wantSignUp {
		if err != nil {
			return fmt.Errorf("auth.signUp: %w", err)
		}
		if _, ok := authorization.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("auth.signUp response = %T, want *tg.AuthAuthorization", authorization)
		}
		return nil
	}
	if err == nil || !registrationRPCMessage(err, "INVITE_HASH_INVALID") {
		return fmt.Errorf("auth.signUp error = %v, want INVITE_HASH_INVALID", err)
	}
	return nil
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
