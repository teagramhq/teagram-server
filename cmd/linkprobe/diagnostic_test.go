package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

func TestDiagnosticTargetsAreFixed(t *testing.T) {
	tests := map[string]string{
		"selector-synthetic": "http://selector:8081/syntheticname",
		"selector-root":      "http://selector:8081/",
		"direct-landing":     "http://linklanding:8082/syntheticname",
	}
	for name, want := range tests {
		got, ok := diagnosticTarget(name)
		if !ok || got != want {
			t.Errorf("diagnosticTarget(%q) = (%q, %t), want (%q, true)", name, got, ok, want)
		}
	}
	if _, ok := diagnosticTarget("http://example.com/"); ok {
		t.Fatal("arbitrary diagnostic URLs must be rejected")
	}
}

func TestDiagnosticPrintsStatusWithoutResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		if _, err := w.Write([]byte("synthetic response body must not be recorded")); err != nil {
			t.Errorf("write fixture response: %v", err)
		}
	}))
	defer server.Close()

	var output strings.Builder
	if exitCode := runDiagnostic(newDiagnosticClient(), server.URL, &output); exitCode != 0 {
		t.Fatalf("runDiagnostic exit code = %d, want 0", exitCode)
	}
	if got := output.String(); got != "status=502\n" {
		t.Fatalf("runDiagnostic output = %q, want status only", got)
	}
}

func TestDiagnosticTransportCategoriesAreAllowlisted(t *testing.T) {
	dnsError := &net.DNSError{Err: "synthetic", Name: "synthetic"}
	if got := diagnosticErrorCategory(&url.Error{Op: "Get", URL: "synthetic", Err: dnsError}); got != "name_resolution" {
		t.Fatalf("DNS category = %q, want name_resolution", got)
	}
	if got := diagnosticErrorCategory(&url.Error{Op: "Get", URL: "synthetic", Err: syscall.ECONNREFUSED}); got != "connection_refused" {
		t.Fatalf("refused category = %q, want connection_refused", got)
	}
	if got := diagnosticErrorCategory(&url.Error{Op: "Get", URL: "synthetic", Err: context.DeadlineExceeded}); got != "timeout" {
		t.Fatalf("timeout category = %q, want timeout", got)
	}
	if got := diagnosticErrorCategory(errors.New("unexpected internal detail")); got != "connect_failure" {
		t.Fatalf("fallback category = %q, want connect_failure", got)
	}
}
