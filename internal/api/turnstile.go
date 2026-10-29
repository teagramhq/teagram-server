package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
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
	// ErrTurnstileBusy asks auth handlers to return FLOOD_WAIT without charging
	// their IP limits.
	ErrTurnstileBusy = errors.New("turnstile verification capacity reached")
)

// SignUpTurnstileVerifier checks a one-use auth challenge. remoteIP is empty
// unless the request address came from an allowlisted PROXY v2 header.
// Implementations return ErrTurnstileBusy when their bounded capacity is full.
type SignUpTurnstileVerifier interface {
	Verify(ctx context.Context, token, action, remoteIP string) error
}

type turnstileVerifier struct {
	secretKey     string
	qaBypassToken string
	endpoint      string
	client        *http.Client
	semaphore     chan struct{}
	hostnames     map[string]struct{}
	logger        *slog.Logger
	timeout       time.Duration
}

// NewTurnstileVerifier builds a Siteverify client from the process environment.
func NewTurnstileVerifier(secretKey string) SignUpTurnstileVerifier {
	return newTurnstileVerifier(secretKey)
}

// NewTurnstileVerifierWithConfig builds a Siteverify client from validated
// startup configuration.
func NewTurnstileVerifierWithConfig(secretKey string, hostnames []string, qaBypassToken string, logger *slog.Logger) SignUpTurnstileVerifier {
	return newTurnstileVerifierWithConfig(secretKey, hostnames, qaBypassToken, logger)
}

func newTurnstileVerifier(secretKey string) *turnstileVerifier {
	hostnames := []string{turnstileHostname}
	if raw := strings.TrimSpace(os.Getenv("TG_TURNSTILE_HOSTNAMES")); raw != "" {
		hostnames = strings.Split(raw, ",")
	}
	for index := range hostnames {
		hostnames[index] = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostnames[index]), "."))
	}
	qaBypassToken := ""
	if path := strings.TrimSpace(os.Getenv("TG_TURNSTILE_QA_BYPASS_TOKEN_FILE")); path != "" {
		data, err := os.ReadFile(path) // #nosec G304,G703 -- validated operator-configured QA secret path.
		if err != nil {
			slog.Default().Error("read Turnstile QA bypass token file", "error", err)
		} else {
			qaBypassToken = strings.TrimSpace(string(data))
			clear(data)
		}
	}
	return newTurnstileVerifierWithConfig(secretKey, hostnames, qaBypassToken, slog.Default())
}

func newTurnstileVerifierWithConfig(secretKey string, hostnames []string, qaBypassToken string, logger *slog.Logger) *turnstileVerifier {
	if len(hostnames) == 0 {
		hostnames = []string{turnstileHostname}
	}
	if logger == nil {
		logger = slog.Default()
	}
	allowedHostnames := make(map[string]struct{}, len(hostnames))
	for _, hostname := range hostnames {
		hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(hostname), "."))
		if hostname != "" {
			allowedHostnames[hostname] = struct{}{}
		}
	}
	return &turnstileVerifier{
		secretKey:     secretKey,
		qaBypassToken: qaBypassToken,
		endpoint:      turnstileSiteverifyURL,
		client: &http.Client{
			Timeout: turnstileVerifyTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		semaphore: make(chan struct{}, turnstileMaxConcurrentChecks),
		hostnames: allowedHostnames,
		logger:    logger,
		timeout:   turnstileVerifyTimeout,
	}
}

func (v *turnstileVerifier) Verify(ctx context.Context, token, action, remoteIP string) error {
	if token == "" || len(token) > turnstileMaxTokenBytes {
		return errTurnstileRejected
	}
	if action == "signup" && v.qaBypassToken != "" && len(token) == len(v.qaBypassToken) &&
		subtle.ConstantTimeCompare([]byte(token), []byte(v.qaBypassToken)) == 1 {
		v.logger.Info("turnstile QA bypass used", "action", action)
		return nil
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
	if !result.Success || result.Action != action {
		return errTurnstileRejected
	}
	if _, ok := v.hostnames[strings.ToLower(strings.TrimSuffix(result.Hostname, "."))]; !ok {
		return errTurnstileRejected
	}
	return nil
}
