package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/gotd/td/telegram"
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

func TestPublicSendCodeBudgetIsolatedFromTailnetListeners(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	key, err := rsakey.Bootstrap(t.TempDir() + "/key.pem")
	if err != nil {
		t.Fatalf("bootstrap RSA key: %v", err)
	}
	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create blob store: %v", err)
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
	cfg := config.Config{
		DCID:                      dcID,
		RegistrationMode:          config.RegistrationOpen,
		PublicWebSocketListenAddr: "127.0.0.1:0",
		PublicRegistrationMode:    config.RegistrationClosed,
		MaxFileBytes:              100 << 20,
		MaxUserStorageBytes:       2 << 30,
		UploadPartTTL:             config.DefaultUploadPartTTL,
	}
	cfg.RateLimits.SendCodeIP = store.SendCodeIPLimits{
		Calls:  store.RateLimitConfig{Limit: 4, Window: time.Hour},
		Phones: store.RateLimitConfig{Limit: 3, Window: time.Hour},
	}
	tgcfg := api.DefaultConfig(dcID, "127.0.0.1", 0)
	log := slog.New(slog.DiscardHandler)
	newHandler := func(mode config.RegistrationMode, _, _ bool) mtproto.Handler {
		return api.New(st, dcID, tgcfg, log, false, cfg.MaxFileBytes, blobs,
			cfg.MaxUserStorageBytes, pgtest.PeerDeriver(), pgtest.PhotoDeriver(), cfg.RateLimits, mode)
	}
	server, publicHandler := newListenerScopedServer(cfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)
	doc, err := discovery.NewDocument("127.0.0.1:2443", dcID, &key.PublicKey, false, "")
	if err != nil {
		t.Fatalf("build discovery document: %v", err)
	}
	if err := server.SetClientDiscoveryDocument(doc); err != nil {
		t.Fatalf("set discovery document: %v", err)
	}
	tailnetTCP := listenRegistrationTestTCP(t)
	tailnetWebSocket := listenRegistrationTestTCP(t)
	publicWebSocket := listenRegistrationTestTCP(t)
	public := newPublicWebSocketListener(cfg, publicWebSocket, publicHandler, doc)
	serveCtx, stopServing := context.WithCancel(ctx)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- serveMTProto(serveCtx, server, tailnetTCP, tailnetWebSocket, public)
	}()
	t.Cleanup(func() {
		stopServing()
		closeRegistrationTestListener(t, tailnetTCP)
		closeRegistrationTestListener(t, tailnetWebSocket)
		closeRegistrationTestListener(t, publicWebSocket)
		if err := <-serveErr; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
			t.Errorf("serve listeners: %v", err)
		}
	})

	stamp := time.Now().UnixNano()
	publicClient := registrationTestClient(t, key, dcID, publicWebSocket, true, config.RegistrationClosed)
	if err := publicClient.Run(ctx, func(ctx context.Context) error {
		for i := range 3 {
			if err := sendLimitedTestCode(ctx, publicClient, fmt.Sprintf("p%d%d", stamp, i)); err != nil {
				return fmt.Errorf("public auth.sendCode %d: %w", i+1, err)
			}
		}
		if err := sendLimitedTestCode(ctx, publicClient, fmt.Sprintf("po%d", stamp)); err == nil {
			return errors.New("public auth.sendCode past the distinct-identifier limit was allowed")
		} else {
			var rpcErr *tgerr.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != 420 {
				return fmt.Errorf("public auth.sendCode past the distinct-identifier limit = %w, want FLOOD_WAIT", err)
			}
		}
		if err := sendLimitedTestCode(ctx, publicClient, fmt.Sprintf("p%d0", stamp)); err != nil {
			return fmt.Errorf("repeat existing public identifier after phone-quota denial: %w", err)
		}
		if err := sendLimitedTestCode(ctx, publicClient, fmt.Sprintf("p%d0", stamp)); err == nil {
			return errors.New("public auth.sendCode past the call limit was allowed")
		} else {
			var rpcErr *tgerr.Error
			if !errors.As(err, &rpcErr) || rpcErr.Code != 420 {
				return fmt.Errorf("public auth.sendCode past the call limit = %w, want FLOOD_WAIT", err)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("public auth.sendCode budget: %v", err)
	}

	for _, tc := range []struct {
		name      string
		listener  net.Listener
		websocket bool
	}{
		{name: "tailnet TCP", listener: tailnetTCP},
		{name: "tailnet WebSocket", listener: tailnetWebSocket, websocket: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := registrationTestClient(t, key, dcID, tc.listener, tc.websocket, config.RegistrationOpen)
			if err := client.Run(ctx, func(ctx context.Context) error {
				suffix := "tcp"
				if tc.websocket {
					suffix = "ws"
				}
				if err := sendLimitedTestCode(ctx, client, fmt.Sprintf("t%d%s", stamp, suffix)); err != nil {
					return fmt.Errorf("tailnet auth.sendCode: %w", err)
				}
				return nil
			}); err != nil {
				t.Fatalf("tailnet auth.sendCode after public limit exhaustion: %v", err)
			}
		})
	}
}

func sendLimitedTestCode(ctx context.Context, client *telegram.Client, username string) error {
	result, err := client.API().AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: username,
		APIID:       1,
		APIHash:     "hash",
	})
	if err != nil {
		return err
	}
	if _, ok := result.(*tg.AuthSentCode); !ok {
		return fmt.Errorf("auth.sendCode response = %T, want *tg.AuthSentCode", result)
	}
	return nil
}
