package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestProbePerformsObfuscatedAbridgedAuthKeyExchange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), probeTimeout)
	defer cancel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate server RSA key: %v", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for MTProto: %v", err)
	}
	keyStore := mtproto.NewMemoryAuthKeyStore()
	server := mtproto.New(exchange.PrivateKey{RSA: rsaKey}, 2, keyStore, nil, nil)
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.Serve(serverCtx, listener) }()
	t.Cleanup(func() {
		stopServer()
		if err := <-served; err != nil {
			t.Errorf("MTProto server stopped with error: %v", err)
		}
	})

	document, err := discovery.NewDocument(listener.Addr().String(), 2, &rsaKey.PublicKey, false, "")
	if err != nil {
		t.Fatalf("build client discovery: %v", err)
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode client discovery: %v", err)
	}
	discoveryServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery method = %s, want GET", r.Method)
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write client discovery: %v", err)
		}
	}))
	defer discoveryServer.Close()

	if err := probe(ctx, listener.Addr().String(), discoveryServer.URL, discoveryServer.Client()); err != nil {
		t.Fatalf("probe MTProto listener: %v", err)
	}
}

func TestProbeRejectsTCPListenerThatStallsBeforeAuthKeyExchange(t *testing.T) {
	t.Parallel()

	rsaKey, err := rsa.GenerateKey(rand.Reader, crypto.RSAKeyBits)
	if err != nil {
		t.Fatalf("generate server RSA key: %v", err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for stalled MTProto: %v", err)
	}

	document, err := discovery.NewDocument(listener.Addr().String(), 2, &rsaKey.PublicKey, false, "")
	if err != nil {
		t.Fatalf("build client discovery: %v", err)
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("encode client discovery: %v", err)
	}
	discoveryServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("discovery method = %s, want GET", r.Method)
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write client discovery: %v", err)
		}
	}))
	defer discoveryServer.Close()

	const handshakeTimeout = time.Second
	ctx, cancel := context.WithTimeout(t.Context(), handshakeTimeout)
	defer cancel()
	accepted := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() {
			if err := conn.Close(); err != nil {
				t.Errorf("close stalled MTProto connection: %v", err)
			}
		}()
		close(accepted)
		<-ctx.Done()
	}()
	t.Cleanup(func() {
		cancel()
		if err := listener.Close(); err != nil {
			t.Errorf("close stalled MTProto listener: %v", err)
		}
		select {
		case <-serverDone:
		case <-time.After(time.Second):
			t.Error("stalled listener did not stop")
		}
	})

	started := time.Now()
	probeDone := make(chan error, 1)
	go func() {
		probeDone <- probe(ctx, listener.Addr().String(), discoveryServer.URL, discoveryServer.Client())
	}()
	deadline := time.NewTimer(handshakeTimeout + 250*time.Millisecond)
	defer deadline.Stop()
	var probeErr error
	probeReturned := false
	select {
	case <-accepted:
	case probeErr = <-probeDone:
		probeReturned = true
	case <-deadline.C:
		t.Fatal("probe did not reach the TCP listener before its deadline")
	}
	select {
	case <-accepted:
	case <-deadline.C:
		t.Fatal("probe did not establish TCP before its deadline")
	}
	if !probeReturned {
		select {
		case probeErr = <-probeDone:
		case <-deadline.C:
			t.Fatal("probe did not return an error by its deadline")
		}
	}
	if probeErr == nil {
		t.Fatal("probe succeeded without completing the MTProto auth-key exchange")
	}
	if elapsed := time.Since(started); elapsed > handshakeTimeout+250*time.Millisecond {
		t.Fatalf("probe returned after %s, past its deadline", elapsed)
	}
}

func TestLiveTargetUsesMTProtoPort2443(t *testing.T) {
	t.Parallel()
	_, port, err := net.SplitHostPort(liveMTProtoEndpoint)
	if err != nil {
		t.Fatalf("split live MTProto endpoint: %v", err)
	}
	if port != "2443" {
		t.Fatalf("live target port = %q, want 2443", port)
	}
}

func TestParseInviteAppConfig(t *testing.T) {
	t.Parallel()

	appConfig := &tg.HelpAppConfig{Config: &tg.JSONObject{Value: []tg.JSONObjectValue{{
		Key: "registration_mode", Value: &tg.JSONString{Value: "invite"},
	}, {
		Key: "user_invites_enabled", Value: &tg.JSONBool{Value: true},
	}, {
		Key: "user_invite_quota", Value: &tg.JSONNumber{Value: 5},
	}}}}
	got, err := parseInviteAppConfig(appConfig)
	if err != nil {
		t.Fatalf("parseInviteAppConfig: %v", err)
	}
	if got.registrationMode != "invite" || !got.userInvitesEnabled || got.userInviteQuota != 5 {
		t.Fatalf("settings = %+v, want invite, enabled, quota 5", got)
	}
}

func TestParseInviteAppConfigRejectsIncompleteOrInvalidSettings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		values []tg.JSONObjectValue
	}{
		{
			name: "missing quota",
			values: []tg.JSONObjectValue{{Key: "registration_mode", Value: &tg.JSONString{Value: "invite"}},
				{Key: "user_invites_enabled", Value: &tg.JSONBool{Value: true}}},
		},
		{
			name: "fractional quota",
			values: []tg.JSONObjectValue{{Key: "registration_mode", Value: &tg.JSONString{Value: "invite"}},
				{Key: "user_invites_enabled", Value: &tg.JSONBool{Value: true}},
				{Key: "user_invite_quota", Value: &tg.JSONNumber{Value: 5.5}}},
		},
		{
			name: "wrong enabled type",
			values: []tg.JSONObjectValue{{Key: "registration_mode", Value: &tg.JSONString{Value: "invite"}},
				{Key: "user_invites_enabled", Value: &tg.JSONString{Value: "true"}},
				{Key: "user_invite_quota", Value: &tg.JSONNumber{Value: 5}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseInviteAppConfig(&tg.HelpAppConfig{Config: &tg.JSONObject{Value: tc.values}}); err == nil {
				t.Fatal("parseInviteAppConfig succeeded with invalid settings")
			}
		})
	}
}
