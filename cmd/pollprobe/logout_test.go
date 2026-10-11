package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestLogoutAllEmitsStoppedClientAggregate(t *testing.T) {
	probe, output, _, _ := newLogoutTestProbe(t, 0)
	if err := probe.logoutAll(); err != nil {
		t.Fatalf("logoutAll with no authorized sessions: %v", err)
	}
	want := "assertion=logout_sessions result=pass count=0 failed=0 stopped_before_logout=0\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
}

func TestLogoutAllClassifiesLogoutFailuresWithoutErrorText(t *testing.T) {
	const syntheticUsername = "synthetic_user"
	const fakeToken = "fake_token_123"
	secret := syntheticUsername + " " + fakeToken
	tests := []struct {
		name             string
		result           *tg.AuthLoggedOut
		err              error
		stopDuringLogout bool
		wantCause        string
		wantErrorCodes   string
		wantRPCName      string
		wantRPCCode      int
	}{
		{name: "wrapped deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), wantCause: "DEADLINE_EXCEEDED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "wrapped cancellation", err: fmt.Errorf("wrapped: %w", context.Canceled), wantCause: "CANCELED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "closed transport sentinel", err: fmt.Errorf("wrapped: %w", net.ErrClosed), wantCause: "TRANSPORT_CLOSED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "EOF", err: io.EOF, wantCause: "TRANSPORT_CLOSED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "unexpected EOF", err: io.ErrUnexpectedEOF, wantCause: "TRANSPORT_CLOSED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "network operation error", err: &net.OpError{Op: "read", Net: "tcp", Err: errors.New(secret)}, wantCause: "TRANSPORT_CLOSED", wantErrorCodes: "OPERATION_FAILED"},
		{name: "RPC failure", err: &tgerr.Error{Code: 401, Message: "AUTH_KEY_UNREGISTERED"}, wantCause: "RPC_ERROR", wantErrorCodes: "AUTH_KEY_UNREGISTERED,RPC_FAILED", wantRPCName: "AUTH_KEY_UNREGISTERED", wantRPCCode: 401},
		{name: "invalid response", wantCause: "RESPONSE_INVALID", wantErrorCodes: "LOGOUT_RESPONSE_INVALID"},
		{name: "unknown error", err: errors.New(secret), wantCause: "UNKNOWN", wantErrorCodes: "OPERATION_FAILED"},
		{name: "persistent client stopped during logout", err: errors.New("opaque failure"), stopDuringLogout: true, wantCause: "CLIENT_STOPPED", wantErrorCodes: "OPERATION_FAILED"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probe, output, _, stopAccounts := newLogoutTestProbe(t, 1)
			probe.logoutRequest = func(context.Context, *tg.Client) (*tg.AuthLoggedOut, error) {
				if test.stopDuringLogout {
					stopAccounts[0]()
				}
				return test.result, test.err
			}
			err := probe.logoutAll()
			if err == nil {
				t.Fatal("logoutAll accepted a failed logout")
			}
			want := "assertion=logout_sessions result=pass count=1 failed=1 stopped_before_logout=0 error_codes=" + test.wantErrorCodes + " logout_causes=" + test.wantCause + "\n"
			if got := output.String(); got != want {
				t.Fatalf("logout summary = %q, want %q", got, want)
			}
			if strings.Contains(output.String(), syntheticUsername) || strings.Contains(output.String(), fakeToken) ||
				strings.Contains(err.Error(), syntheticUsername) || strings.Contains(err.Error(), fakeToken) {
				t.Fatal("logout summary exposed raw error text")
			}
			if test.wantRPCName != "" {
				problem, ok := errors.AsType[*probeError](err)
				if !ok || problem.rpcError != test.wantRPCName || problem.rpcCode != test.wantRPCCode {
					t.Fatalf("RPC failure = %#v, want name %q and code %d", problem, test.wantRPCName, test.wantRPCCode)
				}
				var failureOutput bytes.Buffer
				if writeErr := writeFailure(&failureOutput, problem); writeErr != nil {
					t.Fatalf("write safe RPC failure: %v", writeErr)
				}
				if !strings.Contains(failureOutput.String(), "rpc_error=AUTH_KEY_UNREGISTERED rpc_code=401") {
					t.Fatalf("RPC failure output lost safe code: %q", failureOutput.String())
				}
			}
			if err := probe.stopClients(); err != nil {
				t.Fatalf("stop clients after failed logout: %v", err)
			}
		})
	}
}

func TestLogoutAllUsesAggregateForStoppedPersistentClientAndFallback(t *testing.T) {
	probe, output, shutdownCalls, stopAccounts := newLogoutTestProbe(t, 1)
	stopAccounts[0]()
	probe.runFallback = func(ctx context.Context, _ *probeAccount, _ string, fn func(context.Context, *tg.Client, *telegram.Client) error) error {
		return fn(ctx, &tg.Client{}, &telegram.Client{})
	}
	probe.logoutRequest = func(context.Context, *tg.Client) (*tg.AuthLoggedOut, error) {
		return &tg.AuthLoggedOut{}, nil
	}
	if err := probe.logoutAll(); err != nil {
		t.Fatalf("logoutAll through fallback: %v", err)
	}
	want := "assertion=logout_sessions result=pass count=1 failed=0 stopped_before_logout=1\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
	if strings.Contains(output.String(), "synthpoll_a") {
		t.Fatal("logout summary exposed which persistent client had stopped")
	}
	if err := probe.stopClients(); err != nil {
		t.Fatalf("stop clients after successful fallback logout: %v", err)
	}
	if *shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", *shutdownCalls)
	}
}

func TestLogoutAllKeepsAttemptingSessionsAfterFailuresAndShutsThemDown(t *testing.T) {
	probe, output, shutdownCalls, _ := newLogoutTestProbe(t, 4)
	calls := 0
	probe.logoutRequest = func(context.Context, *tg.Client) (*tg.AuthLoggedOut, error) {
		calls++
		if calls == 1 {
			return nil, fmt.Errorf("wrapped: %w", context.DeadlineExceeded)
		}
		return &tg.AuthLoggedOut{}, nil
	}
	if err := probe.logoutAll(); err == nil {
		t.Fatal("logoutAll accepted cleanup failures")
	}
	want := "assertion=logout_sessions result=pass count=4 failed=1 stopped_before_logout=0 error_codes=OPERATION_FAILED logout_causes=DEADLINE_EXCEEDED\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
	if calls != 4 {
		t.Fatalf("logout attempts = %d, want all four authorized sessions", calls)
	}
	if err := probe.stopClients(); err != nil {
		t.Fatalf("stop clients after cleanup failures: %v", err)
	}
	if *shutdownCalls != 4 {
		t.Fatalf("shutdown calls = %d, want 4", *shutdownCalls)
	}
}

func TestLogoutAllDeduplicatesAggregateCauseSet(t *testing.T) {
	probe, output, _, _ := newLogoutTestProbe(t, 3)
	probe.logoutRequest = func(context.Context, *tg.Client) (*tg.AuthLoggedOut, error) {
		return nil, fmt.Errorf("wrapped: %w", net.ErrClosed)
	}
	if err := probe.logoutAll(); err == nil {
		t.Fatal("logoutAll accepted failed cleanup")
	}
	want := "assertion=logout_sessions result=pass count=3 failed=3 stopped_before_logout=0 error_codes=OPERATION_FAILED logout_causes=TRANSPORT_CLOSED\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
}

func TestLogoutAllClassifiesFailedFallbackAsReconnectFailure(t *testing.T) {
	probe, output, _, stopAccounts := newLogoutTestProbe(t, 1)
	stopAccounts[0]()
	probe.runFallback = func(context.Context, *probeAccount, string, func(context.Context, *tg.Client, *telegram.Client) error) error {
		return errors.New("fallback failed without a safe typed cause")
	}
	if err := probe.logoutAll(); err == nil {
		t.Fatal("logoutAll accepted a failed fallback")
	}
	want := "assertion=logout_sessions result=pass count=1 failed=1 stopped_before_logout=1 error_codes=OPERATION_FAILED logout_causes=RECONNECT_FAILED\n"
	if got := output.String(); got != want {
		t.Fatalf("fallback logout summary = %q, want %q", got, want)
	}
	if err := probe.stopClients(); err != nil {
		t.Fatalf("stop clients after fallback failure: %v", err)
	}
}

func TestLogoutCauseValidationNormalizesUnknownProducerValues(t *testing.T) {
	if got := normalizeLogoutCause(logoutCause("SYNTHETIC_USER_FAKE_TOKEN")); got != logoutCauseUnknown {
		t.Fatalf("normalized logout cause = %q, want %q", got, logoutCauseUnknown)
	}
}

func newLogoutTestProbe(t *testing.T, count int) (*probe, *bytes.Buffer, *int, []func()) {
	t.Helper()
	output := &bytes.Buffer{}
	probe := newProbe(probeConfig{}, output)
	shutdownCalls := 0
	stopAccounts := make([]func(), count)
	for index := range count {
		done := make(chan struct{})
		runErr := make(chan error, 1)
		var stopped sync.Once
		stop := func() {
			stopped.Do(func() {
				runErr <- nil
				close(done)
			})
		}
		stopAccounts[index] = stop
		probe.accounts[index] = &probeAccount{
			username:   probeUsernames[index],
			session:    &session.StorageMemory{},
			client:     &telegram.Client{},
			api:        &tg.Client{},
			done:       done,
			runErr:     runErr,
			authorized: true,
			cancel: func() {
				shutdownCalls++
				stop()
			},
		}
	}
	return probe, output, &shutdownCalls, stopAccounts
}
