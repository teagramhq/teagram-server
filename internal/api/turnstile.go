package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const (
	turnstileSiteverifyURL       = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
	turnstileHostname            = "web.teagram.io"
	turnstileMaxTokenBytes       = 2048
	turnstileMaxResponseBytes    = 64 << 10
	turnstileMaxConcurrentChecks = 16
	turnstileVerifyTimeout       = 5 * time.Second
)

var (
	errTurnstileRejected    = errors.New("turnstile verification rejected")
	errTurnstileUnavailable = errors.New("turnstile verification unavailable")
	// ErrTurnstileBusy asks the sign-up handler to return FLOOD_WAIT without
	// charging the sign-up IP limit.
	ErrTurnstileBusy = errors.New("turnstile verification capacity reached")
)

// SignUpTurnstileVerifier checks a one-use sign-up challenge. remoteIP is
// empty unless the request address came from an allowlisted PROXY v2 header.
// Implementations return ErrTurnstileBusy when their bounded capacity is full.
type SignUpTurnstileVerifier interface {
	Verify(ctx context.Context, token, remoteIP string) error
}

type turnstileVerifier struct {
	secretKey string
	endpoint  string
	client    *http.Client
	semaphore chan struct{}
	hostnames map[string]struct{}
	timeout   time.Duration
}

// NewTurnstileVerifier builds the production Siteverify client for the demo
// web application's fixed hostname allowlist.
func NewTurnstileVerifier(secretKey string) SignUpTurnstileVerifier {
	return newTurnstileVerifier(secretKey)
}

func newTurnstileVerifier(secretKey string) *turnstileVerifier {
	return &turnstileVerifier{
		secretKey: secretKey,
		endpoint:  turnstileSiteverifyURL,
		client: &http.Client{
			Timeout: turnstileVerifyTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		semaphore: make(chan struct{}, turnstileMaxConcurrentChecks),
		hostnames: map[string]struct{}{turnstileHostname: {}},
		timeout:   turnstileVerifyTimeout,
	}
}

func (v *turnstileVerifier) Verify(ctx context.Context, token, remoteIP string) error {
	if token == "" || len(token) > turnstileMaxTokenBytes {
		return errTurnstileRejected
	}
	select {
	case v.semaphore <- struct{}{}:
		defer func() { <-v.semaphore }()
	default:
		return ErrTurnstileBusy
	}

	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	form := url.Values{
		"secret":   {v.secretKey},
		"response": {token},
	}
	if addr, err := netip.ParseAddr(remoteIP); err == nil {
		form.Set("remoteip", addr.Unmap().String())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, v.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errTurnstileUnavailable
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := v.client.Do(request)
	if err != nil {
		return errTurnstileUnavailable
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, turnstileMaxResponseBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || len(body) > turnstileMaxResponseBytes {
		return errTurnstileUnavailable
	}
	if response.StatusCode != http.StatusOK {
		return errTurnstileUnavailable
	}
	var result struct {
		Success  bool   `json:"success"`
		Action   string `json:"action"`
		Hostname string `json:"hostname"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return errTurnstileUnavailable
	}
	if !result.Success || result.Action != "signup" {
		return errTurnstileRejected
	}
	if _, ok := v.hostnames[strings.ToLower(strings.TrimSuffix(result.Hostname, "."))]; !ok {
		return errTurnstileRejected
	}
	return nil
}
