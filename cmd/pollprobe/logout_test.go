package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/gotd/td/tgerr"
)

func TestLogoutAllReportsAggregateLifecycleFields(t *testing.T) {
	var output bytes.Buffer
	probe := newProbe(probeConfig{}, &output)

	if err := probe.logoutAll(); err != nil {
		t.Fatalf("logoutAll with no authorized sessions: %v", err)
	}

	const want = "assertion=logout_sessions result=pass count=0 failed=0 stopped_before_logout=0 logout_causes=\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
}

func TestLogoutAllFailureEmitsOnlyAFailureSummary(t *testing.T) {
	var stdout, stderr bytes.Buffer
	probe := newProbe(probeConfig{}, &stdout)
	for index, username := range probeUsernames {
		probe.accounts[index] = &probeAccount{username: username, authorized: true}
	}

	err := probe.logoutAll()
	if err == nil {
		t.Fatal("logoutAll succeeded when every authorized account lacked its session")
	}
	if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
		t.Fatalf("write cleanup failure summary: %v", writeErr)
	}

	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "assertion=logout_sessions result=pass") {
		t.Fatalf("failed cleanup emitted a pass marker: %q", combined)
	}
	for _, field := range []string{
		"assertion=logout_sessions result=fail",
		"count=4",
		"failed=4",
		"stopped_before_logout=0",
		"logout_causes=UNKNOWN",
	} {
		if !strings.Contains(combined, field) {
			t.Errorf("failed cleanup summary omitted %s: %q", field, combined)
		}
	}
	if strings.Contains(combined, "synthpoll_") {
		t.Fatalf("cleanup summary exposed a synthetic account identity: %q", combined)
	}
}

func TestLogoutSessionsAttemptsAllFourAndSummarizesOneWrappedDeadline(t *testing.T) {
	var stdout, stderr bytes.Buffer
	probe := newProbe(probeConfig{}, &stdout)
	accounts := logoutTestAccounts(4)
	attempts := 0
	err := probe.logoutSessions(accounts, func(_ context.Context, account *probeAccount) logoutAttemptResult {
		attempts++
		if account == accounts[1] {
			return logoutAttemptResult{err: asProbeFailure("logout_sessions", fmt.Errorf("wrapped: %w", context.DeadlineExceeded))}
		}
		return logoutAttemptResult{loggedOut: true}
	})
	if err == nil {
		t.Fatal("logoutSessions succeeded after a failed logout")
	}
	if attempts != 4 {
		t.Fatalf("logout attempts = %d, want 4", attempts)
	}
	if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
		t.Fatalf("write cleanup failure summary: %v", writeErr)
	}
	if got, want := stderr.String(), "assertion=logout_sessions result=fail error_code=OPERATION_FAILED count=4 failed=1 stopped_before_logout=0 error_codes=OPERATION_FAILED logout_causes=DEADLINE_EXCEEDED\n"; got != want {
		t.Fatalf("failure summary = %q, want %q", got, want)
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed cleanup wrote a success summary: %q", stdout.String())
	}
	for index, account := range accounts {
		if account.authorized != (index == 1) {
			t.Errorf("account %d authorized = %t, want %t", index, account.authorized, index == 1)
		}
	}
}

func TestLogoutSessionsCountsExitedPersistentClientsAtEntry(t *testing.T) {
	var output bytes.Buffer
	probe := newProbe(probeConfig{}, &output)
	accounts := logoutTestAccounts(3)
	close(accounts[0].done)
	if err := probe.logoutSessions(accounts, func(context.Context, *probeAccount) logoutAttemptResult {
		return logoutAttemptResult{loggedOut: true}
	}); err != nil {
		t.Fatalf("logoutSessions: %v", err)
	}
	const want = "assertion=logout_sessions result=pass count=3 failed=0 stopped_before_logout=1 logout_causes=\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
}

func TestLogoutSessionsDoesNotCountClientsThatStopAfterCleanupBegins(t *testing.T) {
	var output bytes.Buffer
	probe := newProbe(probeConfig{}, &output)
	accounts := logoutTestAccounts(2)
	if err := probe.logoutSessions(accounts, func(_ context.Context, account *probeAccount) logoutAttemptResult {
		if account == accounts[0] {
			close(accounts[1].done)
		}
		return logoutAttemptResult{loggedOut: true}
	}); err != nil {
		t.Fatalf("logoutSessions: %v", err)
	}
	const want = "assertion=logout_sessions result=pass count=2 failed=0 stopped_before_logout=0 logout_causes=\n"
	if got := output.String(); got != want {
		t.Fatalf("logout summary = %q, want %q", got, want)
	}
}

func TestLogoutSessionsNilResponseIsAClassifiedFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	probe := newProbe(probeConfig{}, &stdout)
	account := logoutTestAccounts(1)[0]
	err := probe.logoutSessions([]*probeAccount{account}, func(context.Context, *probeAccount) logoutAttemptResult {
		return logoutAttemptResult{err: failure("logout_sessions", "LOGOUT_RESPONSE_INVALID")}
	})
	if err == nil {
		t.Fatal("logoutSessions succeeded after a nil logout response")
	}
	if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
		t.Fatalf("write invalid response summary: %v", writeErr)
	}
	for _, field := range []string{"result=fail", "count=1", "failed=1", "error_codes=LOGOUT_RESPONSE_INVALID", "logout_causes=RESPONSE_INVALID"} {
		if !strings.Contains(stderr.String(), field) {
			t.Errorf("invalid response summary omitted %s: %q", field, stderr.String())
		}
	}
	if stdout.Len() != 0 {
		t.Fatalf("invalid response wrote a success summary: %q", stdout.String())
	}
}

func TestLogoutSessionsSortsAndDeduplicatesCauseCategories(t *testing.T) {
	var stderr bytes.Buffer
	probe := newProbe(probeConfig{}, &bytes.Buffer{})
	accounts := logoutTestAccounts(4)
	errorsByAccount := []error{
		asProbeFailure("logout_sessions", context.Canceled),
		asProbeFailure("logout_sessions", net.ErrClosed),
		asProbeFailure("logout_sessions", fmt.Errorf("wrapped: %w", net.ErrClosed)),
		asProbeFailure("logout_sessions", errors.New("unclassified failure")),
	}
	err := probe.logoutSessions(accounts, func(_ context.Context, account *probeAccount) logoutAttemptResult {
		for index, candidate := range accounts {
			if candidate == account {
				return logoutAttemptResult{err: errorsByAccount[index]}
			}
		}
		return logoutAttemptResult{}
	})
	if err == nil {
		t.Fatal("logoutSessions succeeded after four failures")
	}
	if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
		t.Fatalf("write aggregate failure summary: %v", writeErr)
	}
	if !strings.Contains(stderr.String(), "logout_causes=CANCELED,TRANSPORT_CLOSED,UNKNOWN") {
		t.Fatalf("logout cause set was not sorted and deduplicated: %q", stderr.String())
	}
}

func TestLogoutSessionsClassifiesObservedFallbackAndClientStop(t *testing.T) {
	tests := []struct {
		name       string
		result     logoutAttemptResult
		stopClient bool
		want       string
	}{
		{
			name:   "fallback reconnect failed",
			result: logoutAttemptResult{err: errors.New("opaque connect failure"), reconnectFailed: true},
			want:   "RECONNECT_FAILED",
		},
		{
			name:       "persistent client stopped during logout",
			result:     logoutAttemptResult{err: errors.New("opaque operation failure")},
			stopClient: true,
			want:       "CLIENT_STOPPED",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			probe := newProbe(probeConfig{}, &bytes.Buffer{})
			account := logoutTestAccounts(1)[0]
			err := probe.logoutSessions([]*probeAccount{account}, func(context.Context, *probeAccount) logoutAttemptResult {
				if test.stopClient {
					close(account.done)
				}
				return test.result
			})
			if err == nil {
				t.Fatal("logoutSessions succeeded after a failed attempt")
			}
			if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
				t.Fatalf("write lifecycle failure summary: %v", writeErr)
			}
			if !strings.Contains(stderr.String(), "logout_causes="+test.want) {
				t.Fatalf("lifecycle summary = %q, want cause %s", stderr.String(), test.want)
			}
		})
	}
}

func TestLogoutCauseUsesSentinelAndTypeIdentity(t *testing.T) {
	rpcErr := &tgerr.Error{Code: 500, Message: "AUTH_KEY_UNREGISTERED"}
	networkErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("opaque transport detail")}
	tests := []struct {
		name          string
		err           error
		reconnectFail bool
		clientStopped bool
		want          string
	}{
		{name: "wrapped deadline", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), want: "DEADLINE_EXCEEDED"},
		{name: "wrapped canceled", err: fmt.Errorf("wrapped: %w", context.Canceled), want: "CANCELED"},
		{name: "closed transport", err: net.ErrClosed, want: "TRANSPORT_CLOSED"},
		{name: "eof", err: io.EOF, want: "TRANSPORT_CLOSED"},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: "TRANSPORT_CLOSED"},
		{name: "network operation", err: networkErr, want: "TRANSPORT_CLOSED"},
		{name: "rpc error", err: fmt.Errorf("wrapped: %w", rpcErr), want: "RPC_ERROR"},
		{name: "fallback failed", err: errors.New("opaque reconnect failure"), reconnectFail: true, want: "RECONNECT_FAILED"},
		{name: "client stopped", err: errors.New("opaque operation failure"), clientStopped: true, want: "CLIENT_STOPPED"},
		{name: "arbitrary error", err: errors.New("synthetic user and token-secret-93"), want: "UNKNOWN"},
		{name: "invalid response", err: failure("logout_sessions", "LOGOUT_RESPONSE_INVALID"), want: "RESPONSE_INVALID"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyLogoutCause(test.err, test.reconnectFail, test.clientStopped); got != test.want {
				t.Fatalf("classifyLogoutCause() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestLogoutSessionsKeepsRPCNameAndDoesNotEchoArbitraryErrorText(t *testing.T) {
	t.Run("rpc", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		probe := newProbe(probeConfig{}, &stdout)
		account := logoutTestAccounts(1)[0]
		raw := &tgerr.Error{Code: 500, Message: "AUTH_KEY_UNREGISTERED"}
		err := probe.logoutSessions([]*probeAccount{account}, func(context.Context, *probeAccount) logoutAttemptResult {
			return logoutAttemptResult{err: rpcFailure("logout_sessions", fmt.Errorf("wrapped: %w", raw))}
		})
		if err == nil {
			t.Fatal("logoutSessions succeeded after an RPC failure")
		}
		if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
			t.Fatalf("write RPC failure summary: %v", writeErr)
		}
		for _, field := range []string{"result=fail", "failed=1", "error_codes=AUTH_KEY_UNREGISTERED,RPC_FAILED", "logout_causes=RPC_ERROR", "rpc_error=AUTH_KEY_UNREGISTERED", "rpc_code=500"} {
			if !strings.Contains(stderr.String(), field) {
				t.Errorf("RPC failure summary omitted %s: %q", field, stderr.String())
			}
		}
	})

	t.Run("unknown error text", func(t *testing.T) {
		const privateDetail = "synthpoll_a token-secret-93"
		var stdout, stderr bytes.Buffer
		probe := newProbe(probeConfig{}, &stdout)
		account := logoutTestAccounts(1)[0]
		err := probe.logoutSessions([]*probeAccount{account}, func(context.Context, *probeAccount) logoutAttemptResult {
			return logoutAttemptResult{err: asProbeFailure("logout_sessions", errors.New(privateDetail))}
		})
		if err == nil {
			t.Fatal("logoutSessions succeeded after an unknown failure")
		}
		if writeErr := writeFailure(&stderr, asProbeFailure("logout_sessions", err)); writeErr != nil {
			t.Fatalf("write unknown failure summary: %v", writeErr)
		}
		if !strings.Contains(stderr.String(), "logout_causes=UNKNOWN") {
			t.Fatalf("unknown failure summary omitted UNKNOWN: %q", stderr.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), privateDetail) {
			t.Fatalf("logout evidence exposed error text: %q", stdout.String()+stderr.String())
		}
	})
}

func TestCleanupFailureStillRunsClientShutdown(t *testing.T) {
	var shutdownCalls int
	var attempts int
	probe := newProbe(probeConfig{}, &bytes.Buffer{})
	accounts := logoutTestAccounts(4)
	result := runProbeWithCleanup(
		func() error { return nil },
		func() error {
			return probe.logoutSessions(accounts, func(_ context.Context, account *probeAccount) logoutAttemptResult {
				attempts++
				if account == accounts[2] {
					return logoutAttemptResult{err: asProbeFailure("logout_sessions", context.DeadlineExceeded)}
				}
				return logoutAttemptResult{loggedOut: true}
			})
		},
		func() error { shutdownCalls++; return nil },
	)
	if attempts != 4 {
		t.Fatalf("logout attempts = %d, want 4", attempts)
	}
	if shutdownCalls != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdownCalls)
	}
	if result.scenario != nil || result.logout == nil || result.shutdown != nil {
		t.Fatalf("run errors = %+v, want successful scenario, failed logout, and successful shutdown", result)
	}
	if errors.Join(result.scenario, result.logout, result.shutdown) == nil {
		t.Fatal("failed logout did not make the overall run fail")
	}
}

func TestLogoutCauseOutputNormalizesOutOfEnumValues(t *testing.T) {
	if got := normalizeLogoutCause("NOT_APPROVED"); got != "UNKNOWN" {
		t.Fatalf("normalized out-of-enum cause = %q, want UNKNOWN", got)
	}
}

func logoutTestAccounts(count int) []*probeAccount {
	accounts := make([]*probeAccount, count)
	for index := range accounts {
		accounts[index] = &probeAccount{
			username:   probeUsernames[index],
			authorized: true,
			done:       make(chan struct{}),
		}
	}
	return accounts
}
