package linkselector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerAPIProxyErrorLogsCauseAndAllowlistedRoute(t *testing.T) {
	t.Setenv("TG_INVITE_WEB_SIGNUP_URL", "https://web.example.test/signup")
	t.Setenv("TG_INVITE_DESKTOP_URL", "")

	upstream := httptest.NewServer(http.NotFoundHandler())
	serverURL := upstream.URL
	upstream.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, err := NewHandler(serverURL, "http://web.example.test", "http://landing.example.test", logger)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/apiws?token=SENTINEL_QUERY", nil)
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("response status = %d, want %d", response.Code, http.StatusBadGateway)
	}

	var proxyFailure map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		if record["msg"] == "selector upstream request failed" {
			proxyFailure = record
			break
		}
	}
	if proxyFailure == nil {
		t.Fatalf("proxy failure log missing: %s", logs.String())
	}
	if got := proxyFailure["route_class"]; got != serverAPIRouteClass {
		t.Errorf("route_class = %v, want %q", got, serverAPIRouteClass)
	}
	if got := proxyFailure["route"]; got != "websocket" {
		t.Errorf("route = %v, want websocket", got)
	}
	if got := proxyFailure["method"]; got != http.MethodGet {
		t.Errorf("method = %v, want %s", got, http.MethodGet)
	}
	if got, ok := proxyFailure["cause"].(string); !ok || !strings.Contains(got, "connection refused") {
		t.Errorf("cause = %v, want connection refused", proxyFailure["cause"])
	}
	if strings.Contains(logs.String(), "SENTINEL_QUERY") || strings.Contains(logs.String(), "/apiws?") {
		t.Errorf("proxy failure logs exposed the request target: %s", logs.String())
	}
}
