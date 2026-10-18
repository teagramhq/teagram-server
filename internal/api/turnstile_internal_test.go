package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func writeTurnstileBody(tb testing.TB, w io.Writer, body string) {
	tb.Helper()
	if _, err := io.WriteString(w, body); err != nil {
		tb.Errorf("write test Siteverify response: %v", err)
	}
}

func TestTurnstileVerifierPostsFormAndChecksActionAndHostname(t *testing.T) {
	type capturedRequest struct {
		method      string
		query       string
		contentType string
		form        url.Values
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotForm url.Values
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read siteverify request: %v", err)
			return
		}
		gotForm, err = url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse siteverify form: %v", err)
			return
		}
		captured <- capturedRequest{method: r.Method, query: r.URL.RawQuery, contentType: r.Header.Get("Content-Type"), form: gotForm}
		w.Header().Set("Content-Type", "application/json")
		writeTurnstileBody(t, w, `{"success":true,"action":"signup","hostname":"WEB.TEAGRAM.IO."}`)
	}))
	defer server.Close()

	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = server.URL
	if err := verifier.Verify(context.Background(), "token&part=one", "203.0.113.7"); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	got := <-captured
	if got.method != http.MethodPost || got.query != "" || got.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("request method=%q query=%q content-type=%q", got.method, got.query, got.contentType)
	}
	want := url.Values{
		"secret":   {"private-secret"},
		"response": {"token&part=one"},
		"remoteip": {"203.0.113.7"},
	}
	if !reflect.DeepEqual(got.form, want) {
		t.Fatalf("siteverify form = %v, want %v", got.form, want)
	}
}

func TestTurnstileVerifierFailsClosedForInvalidSiteverifyResponses(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
	}{
		{name: "rejected", statusCode: http.StatusOK, body: `{"success":false}`},
		{name: "wrong action", statusCode: http.StatusOK, body: `{"success":true,"action":"login","hostname":"web.teagram.io"}`},
		{name: "wrong hostname", statusCode: http.StatusOK, body: `{"success":true,"action":"signup","hostname":"other.example"}`},
		{name: "malformed json", statusCode: http.StatusOK, body: `{`},
		{name: "http error", statusCode: http.StatusServiceUnavailable, body: `{"success":true,"action":"signup","hostname":"web.teagram.io"}`},
		{name: "oversized response", statusCode: http.StatusOK, body: strings.Repeat("x", turnstileMaxResponseBytes+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.statusCode)
				writeTurnstileBody(t, w, tc.body)
			}))
			defer server.Close()

			verifier := newTurnstileVerifier("private-secret")
			verifier.endpoint = server.URL
			if err := verifier.Verify(context.Background(), "token", ""); err == nil {
				t.Fatal("Verify accepted an invalid Siteverify response")
			}
		})
	}
}

func TestTurnstileVerifierDoesNotSendRemoteIPWhenUnavailable(t *testing.T) {
	captured := make(chan url.Values, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read siteverify request: %v", err)
			return
		}
		gotForm, err := url.ParseQuery(string(body))
		if err != nil {
			t.Errorf("parse siteverify form: %v", err)
			return
		}
		captured <- gotForm
		writeTurnstileBody(t, w, `{"success":true,"action":"signup","hostname":"web.teagram.io"}`)
	}))
	defer server.Close()

	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = server.URL
	if err := verifier.Verify(context.Background(), "token", ""); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	gotForm := <-captured
	if _, ok := gotForm["remoteip"]; ok {
		t.Fatalf("form contains remoteip without a trusted proxy address: %v", gotForm)
	}
}

func TestTurnstileVerifierRejectsTokenBoundsBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeTurnstileBody(t, w, `{"success":true,"action":"signup","hostname":"web.teagram.io"}`)
	}))
	defer server.Close()
	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = server.URL

	for _, token := range []string{"", strings.Repeat("x", turnstileMaxTokenBytes+1)} {
		if err := verifier.Verify(context.Background(), token, ""); !errors.Is(err, errTurnstileRejected) {
			t.Fatalf("Verify token length %d error = %v, want rejected", len(token), err)
		}
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("invalid token bounds caused %d Siteverify requests, want 0", got)
	}
	if err := verifier.Verify(context.Background(), strings.Repeat("x", turnstileMaxTokenBytes), ""); err != nil {
		t.Fatalf("Verify token at maximum length: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("maximum-length token caused %d Siteverify requests, want 1", got)
	}
}

func TestTurnstileVerifierHonorsCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = server.URL

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := verifier.Verify(ctx, "token", "")
	if !errors.Is(err, errTurnstileUnavailable) {
		t.Fatalf("Verify error = %v, want fail-closed unavailable result", err)
	}
}

func TestTurnstileVerifierBoundsConcurrentSiteverifyCalls(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			writeTurnstileBody(t, w, `{"success":true,"action":"signup","hostname":"web.teagram.io"}`)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = server.URL
	verifier.semaphore = make(chan struct{}, 1)

	first := make(chan error, 1)
	go func() { first <- verifier.Verify(context.Background(), "first-token", "") }()
	<-entered
	if err := verifier.Verify(context.Background(), "second-token", ""); !errors.Is(err, ErrTurnstileBusy) {
		t.Fatalf("second Verify error = %v, want capacity refusal", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Verify: %v", err)
	}
}

func TestTurnstileVerifierRefusesRedirectsWithoutForwardingCredentials(t *testing.T) {
	var forwarded atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read redirect request body: %v", err)
		}
		if strings.Contains(string(body), "private-secret") {
			t.Error("redirect received the Siteverify secret")
		}
		writeTurnstileBody(t, w, `{"success":true,"action":"signup","hostname":"web.teagram.io"}`)
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	verifier := newTurnstileVerifier("private-secret")
	verifier.endpoint = origin.URL
	if err := verifier.Verify(context.Background(), "token", ""); err == nil {
		t.Fatal("Verify accepted a redirect response")
	}
	if got := forwarded.Load(); got != 0 {
		t.Fatalf("credentials were forwarded to redirect target %d times, want 0", got)
	}
}
