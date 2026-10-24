package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mtproxy"
	"github.com/gotd/td/mtproxy/obfuscator"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/transport"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

const (
	liveMTProtoEndpoint   = "100.124.236.66:2443"
	liveDiscoveryURL      = "https://teagram.tailaa4918.ts.net/.well-known/teagramd/client"
	maxDiscoveryBodyBytes = 16 * 1024
	discoveryTimeout      = 10 * time.Second
	dialTimeout           = 10 * time.Second
	exchangeTimeout       = 15 * time.Second
	probeTimeout          = 30 * time.Second
	tempKeyLifetime       = 60
)

type serverIdentity struct {
	dcID      int
	port      int
	publicKey *rsa.PublicKey
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: discoveryTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if err := probe(ctx, liveMTProtoEndpoint, liveDiscoveryURL, client); err != nil {
		return fmt.Errorf("MTProto smoke on %s: %w", liveMTProtoEndpoint, err)
	}
	if _, err := fmt.Printf("MTProto auth-key exchange passed on %s\n", liveMTProtoEndpoint); err != nil {
		return fmt.Errorf("write smoke result: %w", err)
	}
	return nil
}

func probe(ctx context.Context, endpoint, discoveryURL string, client *http.Client) error {
	if err := discovery.ValidateEndpoint(endpoint); err != nil {
		return fmt.Errorf("validate target endpoint: %w", err)
	}
	identity, err := fetchIdentity(ctx, discoveryURL, client)
	if err != nil {
		return err
	}
	targetPort, err := endpointPort(endpoint)
	if err != nil {
		return fmt.Errorf("read target port: %w", err)
	}
	if targetPort != identity.port {
		return fmt.Errorf("target port %d does not match discovery port %d", targetPort, identity.port)
	}
	return exchangeAuthKey(ctx, endpoint, identity)
}

func fetchIdentity(ctx context.Context, discoveryURL string, client *http.Client) (identity serverIdentity, returnErr error) {
	parsedURL, err := url.Parse(discoveryURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return serverIdentity{}, errors.New("discovery URL must be an HTTPS URL with a host")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("build discovery request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("fetch client discovery: %w", err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close discovery response: %w", err))
		}
	}()
	if response.StatusCode != http.StatusOK {
		return serverIdentity{}, fmt.Errorf("client discovery returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxDiscoveryBodyBytes+1))
	if err != nil {
		return serverIdentity{}, fmt.Errorf("read client discovery: %w", err)
	}
	if len(body) > maxDiscoveryBodyBytes {
		return serverIdentity{}, fmt.Errorf("client discovery exceeds %d bytes", maxDiscoveryBodyBytes)
	}
	var document discovery.Document
	if err := json.Unmarshal(body, &document); err != nil {
		return serverIdentity{}, fmt.Errorf("decode client discovery: %w", err)
	}
	if document.Version != discovery.Version {
		return serverIdentity{}, fmt.Errorf("unsupported client discovery version %d", document.Version)
	}
	if err := discovery.ValidateEndpoint(document.MTProto.Endpoint); err != nil {
		return serverIdentity{}, fmt.Errorf("validate discovered endpoint: %w", err)
	}
	if err := discovery.ValidateDCID(document.MTProto.DCID); err != nil {
		return serverIdentity{}, fmt.Errorf("validate discovered DC id: %w", err)
	}
	der, err := base64.StdEncoding.Strict().DecodeString(document.MTProto.RSASPKI)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("decode discovered RSA key: %w", err)
	}
	if base64.StdEncoding.EncodeToString(der) != document.MTProto.RSASPKI {
		return serverIdentity{}, errors.New("discovered RSA key is not canonical base64")
	}
	publicKey, err := discovery.ParseSPKI(der)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("parse discovered RSA key: %w", err)
	}
	port, err := endpointPort(document.MTProto.Endpoint)
	if err != nil {
		return serverIdentity{}, fmt.Errorf("read discovered port: %w", err)
	}
	return serverIdentity{dcID: document.MTProto.DCID, port: port, publicKey: publicKey}, nil
}

func endpointPort(endpoint string) (int, error) {
	_, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 0, fmt.Errorf("split endpoint %q: %w", endpoint, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0, fmt.Errorf("parse endpoint port %q: %w", portText, err)
	}
	return port, nil
}

func exchangeAuthKey(ctx context.Context, endpoint string, identity serverIdentity) (returnErr error) {
	dialer := net.Dialer{Timeout: dialTimeout}
	raw, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return fmt.Errorf("dial target: %w", err)
	}
	defer func() {
		if err := raw.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close MTProto socket: %w", err))
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		if err := raw.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set MTProto deadline: %w", err)
		}
	}
	obfuscated := obfuscator.Obfuscated2(rand.Reader, raw)
	if err := obfuscated.Handshake(codec.Abridged{}.ObfuscatedTag(), identity.dcID, mtproxy.Secret{}); err != nil {
		return fmt.Errorf("start obfuscated abridged transport: %w", err)
	}
	protocol := transport.NewProtocol(func() transport.Codec {
		return codec.NoHeader{Codec: codec.Abridged{}}
	})
	client, err := protocol.Handshake(obfuscated)
	if err != nil {
		return fmt.Errorf("initialize abridged transport: %w", err)
	}
	// The 60-second temporary mode is a client request, not a server-side
	// cleanup guarantee; see this command's security model.
	_, err = exchange.NewExchanger(client, identity.dcID).
		WithTimeout(exchangeTimeout).
		WithTempMode(tempKeyLifetime).
		Client([]exchange.PublicKey{{RSA: identity.publicKey}}).
		Run(ctx)
	if err != nil {
		return fmt.Errorf("complete temporary auth-key exchange: %w", err)
	}
	return nil
}
