package linkselector

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

func TestServerAPIProxyLogsMidstreamErrorAndCompletion(t *testing.T) {
	t.Setenv("TG_INVITE_WEB_SIGNUP_URL", "https://web.example.test/signup")
	t.Setenv("TG_INVITE_DESKTOP_URL", "")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream ResponseWriter does not support hijacking")
			return
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		_, writeErr := buffered.WriteString("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n5\r\n")
		if writeErr == nil {
			writeErr = buffered.Flush()
		}
		if writeErr != nil {
			t.Errorf("write truncated upstream response: %v", writeErr)
		}
		if err := connection.Close(); err != nil {
			t.Errorf("close truncated upstream response: %v", err)
		}
	}))
	t.Cleanup(upstream.Close)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler, err := NewHandler(upstream.URL, "http://web.example.test", "http://landing.example.test", logger)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	selectorServer := httptest.NewServer(handler)
	t.Cleanup(selectorServer.Close)

	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, selectorServer.URL+"/.well-known/teagramd/client?token=SENTINEL_QUERY", nil)
	if err != nil {
		t.Fatalf("build discovery request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request discovery through selector: %v", err)
	}
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Error("discovery body read succeeded despite the truncated upstream chunk")
	}
	if err := response.Body.Close(); err != nil {
		t.Errorf("close discovery response body: %v", err)
	}

	var sawProxyError, sawCompletion bool
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		if record["route"] == "discovery" && record["method"] == http.MethodGet && record["route_class"] == serverAPIRouteClass {
			message, ok := record["msg"].(string)
			if ok && strings.Contains(message, "unexpected EOF") {
				sawProxyError = true
			}
		}
		if record["msg"] == "selector response" {
			if record["reason"] == string(reasonTransport) && record["route_class"] == unavailableRouteClass {
				sawCompletion = true
			}
		}
	}
	if !sawProxyError {
		t.Errorf("midstream proxy error with route context missing: %s", logs.String())
	}
	if !sawCompletion {
		t.Errorf("deferred transport completion record missing: %s", logs.String())
	}
	if strings.Contains(logs.String(), "SENTINEL_QUERY") || strings.Contains(logs.String(), "/.well-known/teagramd/client?") {
		t.Errorf("proxy error logs exposed the request target: %s", logs.String())
	}
}

func TestProxyErrorCausePreservesNonNetworkMessages(t *testing.T) {
	if got := proxyErrorCause(io.ErrUnexpectedEOF); got != io.ErrUnexpectedEOF.Error() {
		t.Fatalf("proxyErrorCause = %q, want %q", got, io.ErrUnexpectedEOF.Error())
	}
}
