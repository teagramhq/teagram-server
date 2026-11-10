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

func TestUnexpectedHandlerPanicLogsFailureBeforeRepanicking(t *testing.T) {
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

	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/main.js", nil)
	response := httptest.NewRecorder()
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		handler.ServeHTTP(response, request)
	}()
	if recovered == nil {
		t.Fatal("ServeHTTP did not re-panic after recording the handler failure")
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
			sawPanicLog = record["panic_type"] == "string" &&
				record["route_class"] == unavailableRouteClass &&
				record["status"] == float64(http.StatusInternalServerError)
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
