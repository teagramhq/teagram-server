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

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"

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

	document, err := discovery.NewDocument(listener.Addr().String(), 2, &rsaKey.PublicKey, false)
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
