package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	discoveryURL = "https://demo.teagram.io/.well-known/teagramd/client"
	webOrigin    = "https://web.teagram.io"
)

type clientConfig struct {
	MTProto struct {
		DCID    int    `json:"dc_id"`
		RSASPKI string `json:"rsa_spki"`
	} `json:"mtproto"`
	WebSocket struct {
		Endpoint string `json:"endpoint"`
	} `json:"websocket"`
}

func main() {
	if len(os.Args) != 2 || (os.Args[1] != "active" && os.Args[1] != "disabled") {
		fatalf("usage: go run ./deploy/teagram-demo/turnstile-signin-probe.go active|disabled")
	}
	mode := os.Args[1]
	siteKey := os.Getenv("TEAGRAM_DEMO_TURNSTILE_SITE_KEY")
	if siteKey == "" {
		fatalf("TEAGRAM_DEMO_TURNSTILE_SITE_KEY is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cfg, key, err := discover(ctx)
	if err != nil {
		fatalf("discovery: %v", err)
	}
	if err := runPublic(ctx, cfg, key, siteKey, mode); err != nil {
		fatalf("public probe: %v", err)
	}
	fmt.Println("public_probe=pass")
}

func discover(ctx context.Context) (cfg clientConfig, publicKey *rsa.PublicKey, returnErr error) {
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return clientConfig{}, nil, fmt.Errorf("create discovery request: %w", err)
	}
	resp, err := client.Do(request)
	if err != nil {
		return clientConfig{}, nil, fmt.Errorf("fetch discovery document: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close discovery response: %w", closeErr))
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return clientConfig{}, nil, fmt.Errorf("discovery status=%d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return clientConfig{}, nil, fmt.Errorf("decode discovery document: %w", err)
	}
	if cfg.MTProto.DCID <= 0 || cfg.WebSocket.Endpoint == "" {
		return clientConfig{}, nil, errors.New("discovery document lacks a DC or WebSocket endpoint")
	}
	endpoint, err := url.Parse(cfg.WebSocket.Endpoint)
	if err != nil || endpoint.Scheme != "wss" || endpoint.Host == "" || endpoint.Path != "/apiws" {
		return clientConfig{}, nil, errors.New("discovery WebSocket endpoint is not the expected public route")
	}
	der, err := base64.StdEncoding.DecodeString(cfg.MTProto.RSASPKI)
	if err != nil {
		return clientConfig{}, nil, errors.New("decode discovery RSA key")
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return clientConfig{}, nil, errors.New("parse discovery RSA key")
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return clientConfig{}, nil, errors.New("discovery key is not RSA")
	}
	return cfg, key, nil
}

func runPublic(ctx context.Context, cfg clientConfig, key *rsa.PublicKey, siteKey, mode string) error {
	endpoint, err := url.Parse(cfg.WebSocket.Endpoint)
	if err != nil {
		return err
	}
	client := telegram.NewClient(1, "hash", telegram.Options{
		DC: cfg.MTProto.DCID,
		DCList: dcs.List{Domains: map[int]string{
			cfg.MTProto.DCID: endpoint.String(),
		}},
		PublicKeys: []telegram.PublicKey{{RSA: key}},
		Resolver: dcs.Websocket(dcs.WebsocketOptions{DialOptions: &websocket.DialOptions{
			HTTPHeader: http.Header{"Origin": []string{webOrigin}},
		}}),
		SessionStorage: &session.StorageMemory{},
	})
	return client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		sendCodeErr := requestSendCode(ctx, api)
		if mode == "active" {
			if !rpcIs(sendCodeErr, "RECAPTCHA_CHECK_signin__"+siteKey) {
				return unexpected("public auth.sendCode", sendCodeErr)
			}
			fmt.Println("public_auth.sendCode=signin_challenge(site_key_redacted)")
		} else {
			if !rpcIs(sendCodeErr, "PHONE_NUMBER_INVALID") {
				return unexpected("public auth.sendCode", sendCodeErr)
			}
			fmt.Println("public_auth.sendCode=PHONE_NUMBER_INVALID(no_signin_challenge)")
		}

		_, err := api.AuthSignUp(ctx, &tg.AuthSignUpRequest{
			PhoneNumber: "x", PhoneCodeHash: "invalid", FirstName: "probe",
		})
		if !rpcIs(err, "RECAPTCHA_CHECK_signup__"+siteKey) {
			return unexpected("public auth.signUp", err)
		}
		fmt.Println("public_auth.signUp=signup_challenge(site_key_redacted)")
		return nil
	})
}

func requestSendCode(ctx context.Context, api *tg.Client) error {
	_, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: "x", APIID: 1, APIHash: "hash",
	})
	return err
}

func rpcIs(err error, expected string) bool {
	rpc, ok := tgerr.As(err)
	return ok && rpc.Message == expected
}

func unexpected(operation string, err error) error {
	if err == nil {
		return fmt.Errorf("%s succeeded; expected a challenge or safe invalid-phone response", operation)
	}
	if rpc, ok := tgerr.As(err); ok {
		message := rpc.Message
		if strings.HasPrefix(message, "RECAPTCHA_CHECK_") {
			prefix, _, _ := strings.Cut(message, "__")
			message = prefix + "__<redacted>"
		}
		return fmt.Errorf("%s returned RPC message %s", operation, message)
	}
	return fmt.Errorf("%s returned %T", operation, errors.Unwrap(err))
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
