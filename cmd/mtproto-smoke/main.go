package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gotd/td/exchange"
	"github.com/gotd/td/mtproxy"
	"github.com/gotd/td/mtproxy/obfuscator"
	"github.com/gotd/td/proto/codec"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
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
	expectedInviteQuota := flag.Int("expect-user-invite-quota", -1, "require help.getAppConfig to advertise the given invite quota")
	endpoint := flag.String("endpoint", liveMTProtoEndpoint, "MTProto endpoint to probe")
	discoveryURL := flag.String("discovery-url", liveDiscoveryURL, "HTTPS client-discovery URL")
	flag.Parse()
	quotaExpectationSet := false
	flag.Visit(func(value *flag.Flag) {
		if value.Name == "expect-user-invite-quota" {
			quotaExpectationSet = true
		}
	})
	if quotaExpectationSet && *expectedInviteQuota < 0 {
		log.Fatal("expected user invite quota must be non-negative")
	}
	if flag.NArg() != 0 {
		log.Fatal("unexpected arguments")
	}
	if err := run(*expectedInviteQuota, *endpoint, *discoveryURL); err != nil {
		log.Fatal(err)
	}
}

func run(expectedInviteQuota int, endpoint, discoveryURL string) error {
	if expectedInviteQuota < -1 {
		return errors.New("expected user invite quota must be non-negative")
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	client := &http.Client{
		Timeout: discoveryTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if expectedInviteQuota >= 0 {
		settings, err := probeAppConfig(ctx, endpoint, discoveryURL, client)
		if err != nil {
			return fmt.Errorf("MTProto app-config smoke on %s: %w", endpoint, err)
		}
		if settings.registrationMode != "invite" || !settings.userInvitesEnabled || settings.userInviteQuota != expectedInviteQuota {
			return fmt.Errorf("help.getAppConfig registration_mode=%q user_invites_enabled=%t user_invite_quota=%d; want invite, true, %d",
				settings.registrationMode, settings.userInvitesEnabled, settings.userInviteQuota, expectedInviteQuota)
		}
		if _, err := fmt.Printf("help.getAppConfig passed on %s registration_mode=%s user_invites_enabled=%t user_invite_quota=%d\n",
			endpoint, settings.registrationMode, settings.userInvitesEnabled, settings.userInviteQuota); err != nil {
			return fmt.Errorf("write app-config smoke result: %w", err)
		}
		return nil
	}
	if err := probe(ctx, endpoint, discoveryURL, client); err != nil {
		return fmt.Errorf("MTProto smoke on %s: %w", endpoint, err)
	}
	if _, err := fmt.Printf("MTProto auth-key exchange passed on %s\n", endpoint); err != nil {
		return fmt.Errorf("write smoke result: %w", err)
	}
	return nil
}

type inviteAppConfig struct {
	registrationMode   string
	userInvitesEnabled bool
	userInviteQuota    int
}

func probeAppConfig(ctx context.Context, endpoint, discoveryURL string, httpClient *http.Client) (inviteAppConfig, error) {
	if err := discovery.ValidateEndpoint(endpoint); err != nil {
		return inviteAppConfig{}, fmt.Errorf("validate target endpoint: %w", err)
	}
	identity, err := fetchIdentity(ctx, discoveryURL, httpClient)
	if err != nil {
		return inviteAppConfig{}, err
	}
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return inviteAppConfig{}, fmt.Errorf("split target endpoint: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return inviteAppConfig{}, fmt.Errorf("parse target port: %w", err)
	}
	if port != identity.port {
		return inviteAppConfig{}, fmt.Errorf("target port %d does not match discovery port %d", port, identity.port)
	}

	client := telegram.NewClient(1, "hash", telegram.Options{
		DC:             identity.dcID,
		PublicKeys:     []telegram.PublicKey{{RSA: identity.publicKey}},
		SessionStorage: &session.StorageMemory{},
		DCList: dcs.List{Options: []tg.DCOption{{
			ID: identity.dcID, IPAddress: host, Port: port,
		}}},
		Resolver: dcs.Plain(dcs.PlainOptions{}),
	})
	var settings inviteAppConfig
	if err := client.Run(ctx, func(ctx context.Context) error {
		appConfig, err := client.API().HelpGetAppConfig(ctx, 0)
		if err != nil {
			return fmt.Errorf("help.getAppConfig: %w", err)
		}
		settings, err = parseInviteAppConfig(appConfig)
		return err
	}); err != nil {
		return inviteAppConfig{}, err
	}
	return settings, nil
}

func parseInviteAppConfig(appConfig tg.HelpAppConfigClass) (inviteAppConfig, error) {
	full, ok := appConfig.(*tg.HelpAppConfig)
	if !ok {
		return inviteAppConfig{}, fmt.Errorf("help.getAppConfig response is %T, want *tg.HelpAppConfig", appConfig)
	}
	values, ok := full.Config.(*tg.JSONObject)
	if !ok {
		return inviteAppConfig{}, fmt.Errorf("help.getAppConfig config is %T, want *tg.JSONObject", full.Config)
	}
	var settings inviteAppConfig
	var gotMode, gotEnabled, gotQuota bool
	for _, value := range values.Value {
		switch value.Key {
		case "registration_mode":
			item, ok := value.Value.(*tg.JSONString)
			if !ok {
				return inviteAppConfig{}, errors.New("help.getAppConfig registration_mode is not a string")
			}
			settings.registrationMode = item.Value
			gotMode = true
		case "user_invites_enabled":
			item, ok := value.Value.(*tg.JSONBool)
			if !ok {
				return inviteAppConfig{}, errors.New("help.getAppConfig user_invites_enabled is not a boolean")
			}
			settings.userInvitesEnabled = item.Value
			gotEnabled = true
		case "user_invite_quota":
			item, ok := value.Value.(*tg.JSONNumber)
			maxInt := float64(int(^uint(0) >> 1))
			if !ok || math.Trunc(item.Value) != item.Value || item.Value < 0 ||
				item.Value > maxInt || (strconv.IntSize == 64 && item.Value == maxInt) {
				return inviteAppConfig{}, errors.New("help.getAppConfig user_invite_quota is not a non-negative integer")
			}
			settings.userInviteQuota = int(item.Value)
			gotQuota = true
		}
	}
	if !gotMode || !gotEnabled || !gotQuota {
		return inviteAppConfig{}, errors.New("help.getAppConfig omitted user invite settings")
	}
	return settings, nil
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
