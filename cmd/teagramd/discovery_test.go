package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"log/slog"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/discovery"
)

func TestListenerDiscoveryDocumentsAdvertisePublicWebSocketHostOnlyOnPublicListener(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	base := config.Config{
		AdvertiseHost:             "tailnet.example.test",
		AdvertisePort:             2443,
		DCID:                      2,
		AdvertiseDemoGuestSignUp:  true,
		PublicWebSocketListenAddr: "127.0.0.1:2446",
		Turnstile: config.TurnstileConfig{
			SiteKey:   "public-site-key",
			SecretKey: "private-secret-key",
		},
	}
	wipeGeneration := strings.Repeat("f", 64)
	withoutOverride, withoutPublic, err := listenerDiscoveryDocuments(base, &key.PublicKey, wipeGeneration)
	if err != nil {
		t.Fatalf("build default listener discovery: %v", err)
	}

	withPublicHostConfig := base
	withPublicHostConfig.PublicWebSocketAdvertiseHost = "server.teagram.io"
	withPublicHostConfig.PublicWebSocketAdvertisePort = 443
	withOverride, public, err := listenerDiscoveryDocuments(withPublicHostConfig, &key.PublicKey, wipeGeneration)
	if err != nil {
		t.Fatalf("build listener-scoped discovery: %v", err)
	}

	before, err := discovery.MarshalDocument(withoutOverride)
	if err != nil {
		t.Fatalf("marshal default listener discovery: %v", err)
	}
	after, err := discovery.MarshalDocument(withOverride)
	if err != nil {
		t.Fatalf("marshal default listener discovery with public override: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("tailnet discovery changed with public hostname setting:\nbefore: %s\nafter:  %s", before, after)
	}
	if withoutPublic.WebSocket.Endpoint != "wss://tailnet.example.test/apiws" {
		t.Fatalf("public discovery without override = %q, want tailnet endpoint", withoutPublic.WebSocket.Endpoint)
	}
	if public.MTProto.Endpoint != "tailnet.example.test:2443" || public.WebSocket.Endpoint != "wss://server.teagram.io/apiws" {
		t.Fatalf("public discovery endpoints = MTProto %q, WebSocket %q", public.MTProto.Endpoint, public.WebSocket.Endpoint)
	}
	if public.Demo == nil || !public.Demo.GuestSignUp || public.Demo.WipeGeneration != wipeGeneration || public.Turnstile == nil || public.Turnstile.SiteKey != "public-site-key" {
		t.Fatalf("public discovery lost demo or Turnstile fields: %+v", public)
	}
	publicEndpoint, err := url.Parse(public.WebSocket.Endpoint)
	if err != nil {
		t.Fatalf("parse public WebSocket endpoint: %v", err)
	}
	enteredHost := "server.teagram.io"
	if publicEndpoint.Hostname() != enteredHost {
		t.Fatalf("web host-match check would reject: entered %q, endpoint host %q", enteredHost, publicEndpoint.Hostname())
	}
}

func TestListenerDiscoveryDocumentsRequireValidWipeGenerationWhenDemoAdvertisingIsEnabled(t *testing.T) {
	t.Parallel()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cfg := config.Config{
		AdvertiseHost:            "demo.example.test",
		AdvertisePort:            443,
		DCID:                     2,
		AdvertiseDemoGuestSignUp: true,
	}
	for _, generation := range []string{"", "bad", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		if _, _, err := listenerDiscoveryDocuments(cfg, &key.PublicKey, generation); err == nil {
			t.Errorf("listener discovery accepted malformed generation %q", generation)
		}
	}
}

func TestClientConfigOutputIgnoresPublicWebSocketAdvertiseAddr(t *testing.T) {
	t.Setenv("TG_ADVERTISE_ADDR", "tailnet.example.test:2443")
	t.Setenv("TG_ADVERTISE_DEMO_GUEST_SIGN_UP", "")
	t.Setenv("TG_DC_ID", "2")
	t.Setenv("TG_PUBLIC_WEBSOCKET_LISTEN_ADDR", "")
	t.Setenv("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "")
	t.Setenv("TG_TURNSTILE_SITE_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY", "")
	t.Setenv("TG_TURNSTILE_SECRET_KEY_FILE", "")
	t.Setenv("TG_TURNSTILE_SIGNIN_ENABLED", "")
	keyPath := filepath.Join(t.TempDir(), "server-key.pem")
	t.Setenv("TG_RSA_KEY_PATH", keyPath)
	var bootstrapOutput bytes.Buffer
	if err := runCommand([]string{"bootstrap-identity"}, slog.New(slog.DiscardHandler), &bootstrapOutput, &bootstrapOutput); err != nil {
		t.Fatalf("bootstrap-identity: %v", err)
	}

	var withoutSetting bytes.Buffer
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &withoutSetting, &withoutSetting); err != nil {
		t.Fatalf("client-config without public advertisement: %v", err)
	}
	t.Setenv("TG_PUBLIC_WEBSOCKET_ADVERTISE_ADDR", "server.teagram.io:443")
	var withSetting bytes.Buffer
	if err := runCommand([]string{"client-config"}, slog.New(slog.DiscardHandler), &withSetting, &withSetting); err != nil {
		t.Fatalf("client-config with public advertisement: %v", err)
	}
	if !bytes.Equal(withoutSetting.Bytes(), withSetting.Bytes()) {
		t.Fatalf("client-config output changed with public hostname setting:\nbefore: %s\nafter:  %s", withoutSetting.Bytes(), withSetting.Bytes())
	}
}
