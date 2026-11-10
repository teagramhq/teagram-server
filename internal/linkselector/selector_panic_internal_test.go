package linkselector

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnexpectedHandlerPanicLogsAndReturnsHTTP500(t *testing.T) {
	t.Setenv("TG_INVITE_WEB_SIGNUP_URL", "https://web.example.test/signup")
	t.Setenv("TG_INVITE_DESKTOP_URL", "")
	var logs bytes.Buffer
	handler, err := NewHandler("", "http://web.example.test", "http://landing.example.test", slog.New(slog.NewJSONHandler(&logs, nil)))
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	selectorHandler, ok := handler.(*selector)
	if !ok {
		t.Fatalf("NewHandler returned %T, want *selector", handler)
	}
	selectorHandler.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		panic("SENTINEL_PANIC")
	})

	server := httptest.NewServer(handler)
	defer server.Close()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/main.js", nil)
	if err != nil {
		t.Fatalf("build panic request: %v", err)
	}
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("GET after selector panic: %v", err)
	}
	body, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		t.Errorf("read panic response: %v", readErr)
	}
	if err := response.Body.Close(); err != nil {
		t.Errorf("close panic response: %v", err)
	}
	if response.StatusCode != http.StatusInternalServerError {
		t.Errorf("panic response code = %d, want %d", response.StatusCode, http.StatusInternalServerError)
	}
	if len(body) == 0 {
		t.Error("panic response body is empty")
	}

	var sawFailureSummary, sawPanicLog bool
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record %q: %v", line, err)
		}
		if record["msg"] == "selector response" {
			sawFailureSummary = record["reason"] == "panic" &&
				record["route_class"] == unavailableRouteClass &&
				record["status"] == float64(http.StatusInternalServerError)
		}
		if record["msg"] == "selector handler panicked" {
			stack, hasStack := record["stack"].(string)
			sawPanicLog = record["panic_type"] == "string" &&
				record["route_class"] == unavailableRouteClass &&
				record["status"] == float64(http.StatusInternalServerError) &&
				hasStack && strings.Contains(stack, "ServeHTTP")
		}
	}
	if !sawFailureSummary {
		t.Errorf("failure completion record missing or incorrect: %s", logs.String())
	}
	if !sawPanicLog {
		t.Errorf("unexpected panic log missing or incorrect: %s", logs.String())
	}
	if strings.Contains(logs.String(), "SENTINEL_PANIC") {
		t.Errorf("panic payload leaked to logs: %s", logs.String())
	}
}
