package mtproto

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/transport"
)

const (
	webExchangeOrigin = "https://exchange.synthetic.invalid"
)

func TestWebExchangeClientAbortClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "success", want: "none"},
		{name: "deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "closed transport", err: fmt.Errorf("read: %w", io.EOF), want: "transport_closed"},
		{name: "closed pipe", err: fmt.Errorf("write: %w", io.ErrClosedPipe), want: "transport_closed"},
		{name: "unknown", err: errors.New("synthetic failure"), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webExchangeClientAbortReason(tt.err); got != tt.want {
				t.Fatalf("abort reason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWebExchangeServeFailureClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want webExchangeGotdControlFailures
	}{
		{name: "clean return"},
		{name: "peer EOF", err: io.EOF},
		{name: "closed transport", err: net.ErrClosed},
		{name: "closed pipe", err: io.ErrClosedPipe},
		{name: "truncated frame", err: io.ErrUnexpectedEOF, want: webExchangeGotdFailureServeUnexpectedEOF},
		{name: "canceled", err: context.Canceled, want: webExchangeGotdFailureServeCanceled},
		{name: "deadline", err: context.DeadlineExceeded, want: webExchangeGotdFailureServeDeadline},
		{name: "server error", err: errors.New("synthetic failure"), want: webExchangeGotdFailureServeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := webExchangeGotdServeFailure(tt.err); got != tt.want {
				t.Fatalf("serve failure class = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestWebExchangeGotdControlFailureReasonsPreserveDistinctCauses(t *testing.T) {
	var failures webExchangeGotdControlFailures
	failures.add(webExchangeGotdFailureClientClose)
	failures.add(webExchangeGotdFailureServeTimeout)

	if got := failures.first(); got != webExchangeGotdFailureClientClose {
		t.Fatalf("first failure = %d, want client close", got)
	}
	if !failures.has(webExchangeGotdFailureClientClose) || !failures.has(webExchangeGotdFailureServeTimeout) {
		t.Fatal("distinct cleanup failures were not retained")
	}
	if failures.has(webExchangeGotdFailureServeError) {
		t.Fatal("an unrecorded server error was reported")
	}
}

func TestWebExchangeClientOutputFailureClassification(t *testing.T) {
	completeRows := func() []webExchangeClientRow {
		rows := make([]webExchangeClientRow, 0, 6)
		for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
			rows = append(rows, webExchangeClientRow{Case: name})
		}
		return rows
	}

	tests := []struct {
		name       string
		runErr     error
		rowsReady  bool
		rows       []webExchangeClientRow
		wantClass  string
		wantMissed string
	}{
		{name: "observation read failure", wantClass: "rows_unavailable"},
		{name: "no client rows", rowsReady: true, wantClass: "case_missing", wantMissed: "A"},
		{name: "later client row missing", rowsReady: true, rows: []webExchangeClientRow{{Case: "A"}, {Case: "B"}, {Case: "C"}, {Case: "N1"}, {Case: "N2"}}, wantClass: "case_missing", wantMissed: "N3"},
		{name: "runner assertion after all rows", runErr: errors.New("synthetic runner failure"), rowsReady: true, rows: completeRows(), wantClass: "client_assertions"},
		{name: "complete successful output", rowsReady: true, rows: completeRows()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotMissing := classifyWebExchangeClientOutputFailure(tt.runErr, tt.rowsReady, tt.rows)
			if gotClass != tt.wantClass || gotMissing != tt.wantMissed {
				t.Fatalf("failure classification = (%q, %q), want (%q, %q)", gotClass, gotMissing, tt.wantClass, tt.wantMissed)
			}
		})
	}
}

func TestReadWebExchangeClientRowsStrictly(t *testing.T) {
	tests := []struct {
		name         string
		contents     string
		create       bool
		wantCategory string
		wantRows     int
	}{
		{name: "missing output is a read failure", wantCategory: "file_read"},
		{name: "valid rows", contents: `[{"case":"A","client_stage":"unknown","client_abort_reason":"unknown","plain_requests_on_socket":0,"retry_reused_socket":false,"third_constructor":"unknown","proto_error_404":false}]`, create: true, wantRows: 1},
		{name: "invalid JSON is a decode failure", contents: `[{`, create: true, wantCategory: "json_decode"},
		{name: "unknown row shape is a decode failure", contents: `[{"case":"A","unexpected":true}]`, create: true, wantCategory: "json_decode"},
		{name: "missing fixed field is a decode failure", contents: `[{"case":"A","client_stage":"unknown","client_abort_reason":"unknown","plain_requests_on_socket":0,"retry_reused_socket":false,"third_constructor":"unknown"}]`, create: true, wantCategory: "json_decode"},
		{name: "invalid fixed enum is a decode failure", contents: `[{"case":"A","client_stage":"raw-error","client_abort_reason":"unknown","plain_requests_on_socket":0,"retry_reused_socket":false,"third_constructor":"unknown","proto_error_404":false}]`, create: true, wantCategory: "json_decode"},
		{name: "duplicate case is a decode failure", contents: `[{"case":"A","client_stage":"unknown","client_abort_reason":"unknown","plain_requests_on_socket":0,"retry_reused_socket":false,"third_constructor":"unknown","proto_error_404":false},{"case":"A","client_stage":"unknown","client_abort_reason":"unknown","plain_requests_on_socket":0,"retry_reused_socket":false,"third_constructor":"unknown","proto_error_404":false}]`, create: true, wantCategory: "json_decode"},
		{name: "trailing JSON is a decode failure", contents: `[] []`, create: true, wantCategory: "json_decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "client-observations.json")
			if tt.create {
				if err := os.WriteFile(path, []byte(tt.contents), 0o600); err != nil {
					t.Fatal("synthetic client observation fixture could not be written")
				}
			}
			rows, category := readWebExchangeClientRows(path)
			if category != tt.wantCategory || len(rows) != tt.wantRows {
				t.Fatalf("client row result = (category %q, count %d), want (category %q, count %d)", category, len(rows), tt.wantCategory, tt.wantRows)
			}
		})
	}
}

func TestWebExchangePreRequestFailureMarkerSanitization(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "allowed marker",
			line: "synthetic_pre_request_failure case=B category=transport_construction",
			want: []string{"synthetic_pre_request_failure case=B category=transport_construction"},
		},
		{
			name: "non-allowlisted category is unknown",
			line: "synthetic_pre_request_failure case=N2 category=raw-error-value",
			want: []string{"synthetic_pre_request_failure case=N2 category=unknown"},
		},
		{
			name: "malformed category is unknown",
			line: "synthetic_pre_request_failure case=C category=transport_selection extra=raw-error-value",
			want: []string{"synthetic_pre_request_failure case=C category=unknown"},
		},
		{
			name: "unrecognized case fails closed for each fixed case",
			line: "synthetic_pre_request_failure case=live category=raw-error-value",
			want: []string{
				"synthetic_pre_request_failure case=A category=unknown",
				"synthetic_pre_request_failure case=B category=unknown",
				"synthetic_pre_request_failure case=C category=unknown",
				"synthetic_pre_request_failure case=N1 category=unknown",
				"synthetic_pre_request_failure case=N2 category=unknown",
				"synthetic_pre_request_failure case=N3 category=unknown",
			},
		},
		{
			name: "ordinary output is ignored",
			line: "vitest completed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeWebExchangePreRequestFailureLine(tt.line)
			if len(got) != len(tt.want) {
				t.Fatalf("sanitized marker count = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("sanitized marker %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestWebExchangePreRequestMarkerWriter(t *testing.T) {
	writer := &webExchangePreRequestMarkerWriter{}
	if _, err := writer.Write([]byte("ordinary output\nsynthetic_pre_request_failure case=B category=raw-secret\n")); err != nil {
		t.Fatal("marker writer rejected synthetic output")
	}
	if _, err := writer.Write([]byte("synthetic_pre_request_failure case=A category=transport_selection\n")); err != nil {
		t.Fatal("marker writer rejected synthetic output")
	}
	writer.flush()
	want := []string{
		"synthetic_pre_request_failure case=A category=transport_selection",
		"synthetic_pre_request_failure case=B category=unknown",
	}
	got := writer.lines()
	if len(got) != len(want) {
		t.Fatalf("safe marker count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("safe marker %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWebExchangeServerDhBoundaryMarkerSanitization(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{
			name: "allowed actual and control results",
			line: "synthetic_server_dh_boundary subject=actual result=nonce_mismatch",
			want: []string{"synthetic_server_dh_boundary subject=actual result=nonce_mismatch"},
		},
		{
			name: "raw result is unknown for its subject",
			line: "synthetic_server_dh_boundary subject=actual result=raw-secret",
			want: []string{"synthetic_server_dh_boundary subject=actual result=unknown"},
		},
		{
			name: "extra token is unknown for its subject",
			line: "synthetic_server_dh_boundary subject=control result=decrypt_entered raw-secret",
			want: []string{"synthetic_server_dh_boundary subject=control result=unknown"},
		},
		{
			name: "trailing whitespace is outside the exact vocabulary",
			line: "synthetic_server_dh_boundary subject=actual result=nonce_mismatch ",
			want: []string{"synthetic_server_dh_boundary subject=actual result=unknown"},
		},
		{
			name: "unparsable subject fails closed for both subjects",
			line: "synthetic_server_dh_boundary subject=unexpected result=decrypt_entered",
			want: []string{
				"synthetic_server_dh_boundary subject=actual result=unknown",
				"synthetic_server_dh_boundary subject=control result=unknown",
			},
		},
		{
			name: "ordinary output is ignored",
			line: "vitest completed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeWebExchangeServerDhBoundaryLine([]byte(tt.line))
			if len(got) != len(tt.want) {
				t.Fatalf("sanitized boundary marker count = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("sanitized boundary marker %d differs from its fixed result", i)
				}
			}
		})
	}
}

func TestWebExchangeServerDhBoundaryMarkerWriter(t *testing.T) {
	t.Run("sanitizes and emits fixed subject order", func(t *testing.T) {
		writer := &webExchangeServerDhBoundaryMarkerWriter{}
		if _, err := writer.Write([]byte("ordinary output\nsynthetic_server_dh_boundary subject=control result=decrypt_entered\n")); err != nil {
			t.Fatal("boundary marker writer rejected synthetic output")
		}
		if _, err := writer.Write([]byte("synthetic_server_dh_boundary subject=actual result=raw-error-value\n")); err != nil {
			t.Fatal("boundary marker writer rejected synthetic output")
		}
		writer.flush()
		want := []string{
			"synthetic_server_dh_boundary subject=actual result=unknown",
			"synthetic_server_dh_boundary subject=control result=decrypt_entered",
		}
		got := writer.lines()
		if len(got) != len(want) {
			t.Fatalf("safe boundary marker count = %d, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("safe boundary marker %d differs from its fixed result", i)
			}
		}
	})

	t.Run("duplicate subject becomes one unknown marker", func(t *testing.T) {
		writer := &webExchangeServerDhBoundaryMarkerWriter{}
		if _, err := writer.Write([]byte("synthetic_server_dh_boundary subject=actual result=nonce_mismatch\n")); err != nil {
			t.Fatal("boundary marker writer rejected synthetic output")
		}
		if _, err := writer.Write([]byte("synthetic_server_dh_boundary subject=actual result=decrypt_entered\n")); err != nil {
			t.Fatal("boundary marker writer rejected synthetic output")
		}
		writer.flush()
		want := []string{"synthetic_server_dh_boundary subject=actual result=unknown"}
		got := writer.lines()
		if len(got) != len(want) || got[0] != want[0] {
			t.Fatalf("duplicate boundary marker was not collapsed to one unknown result")
		}
	})

	t.Run("unparsable subject makes both results unknown", func(t *testing.T) {
		writer := &webExchangeServerDhBoundaryMarkerWriter{}
		if _, err := writer.Write([]byte("synthetic_server_dh_boundary subject=unexpected result=decrypt_entered\n")); err != nil {
			t.Fatal("boundary marker writer rejected synthetic output")
		}
		writer.flush()
		want := []string{
			"synthetic_server_dh_boundary subject=actual result=unknown",
			"synthetic_server_dh_boundary subject=control result=unknown",
		}
		got := writer.lines()
		if len(got) != len(want) {
			t.Fatalf("unparsable subject produced %d safe results, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("unparsable subject result %d was not unknown", i)
			}
		}
	})
}

func TestWebExchangeServerDhControlCheckpointMarkerSanitization(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "allowed control entry", line: "synthetic_server_dh_control_checkpoint result=decrypt_entered", want: "synthetic_server_dh_control_checkpoint result=decrypt_entered"},
		{name: "allowed incomplete capture", line: "synthetic_server_dh_control_checkpoint result=capture_incomplete", want: "synthetic_server_dh_control_checkpoint result=capture_incomplete"},
		{name: "raw result fails closed", line: "synthetic_server_dh_control_checkpoint result=raw-secret", want: "synthetic_server_dh_control_checkpoint result=unknown"},
		{name: "extra token fails closed", line: "synthetic_server_dh_control_checkpoint result=decrypt_entered extra=raw-secret", want: "synthetic_server_dh_control_checkpoint result=unknown"},
		{name: "trailing whitespace fails closed", line: "synthetic_server_dh_control_checkpoint result=decrypt_entered ", want: "synthetic_server_dh_control_checkpoint result=unknown"},
		{name: "missing marker fails closed", line: "ordinary output", want: "synthetic_server_dh_control_checkpoint result=unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeWebExchangeServerDhControlCheckpointLine([]byte(tt.line)); got != tt.want {
				t.Fatalf("safe checkpoint result differs from the fixed result")
			}
		})
	}
}

func TestWebExchangeServerDhControlCheckpointWriter(t *testing.T) {
	boundary := func(actual, control string) []string {
		return []string{
			formatWebExchangeServerDhBoundary("actual", actual),
			formatWebExchangeServerDhBoundary("control", control),
		}
	}
	tests := []struct {
		name     string
		input    string
		boundary []string
		want     string
	}{
		{
			name:     "decrypt entry cross-checks control",
			input:    "synthetic_server_dh_control_checkpoint result=decrypt_entered\n",
			boundary: boundary("nonce_mismatch", "decrypt_entered"),
			want:     "synthetic_server_dh_control_checkpoint result=decrypt_entered",
		},
		{
			name:     "decrypt entry mismatch fails closed",
			input:    "synthetic_server_dh_control_checkpoint result=decrypt_entered\n",
			boundary: boundary("unknown", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
		{
			name:     "incomplete capture requires unknown boundary pair",
			input:    "synthetic_server_dh_control_checkpoint result=capture_incomplete\n",
			boundary: boundary("unknown", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=capture_incomplete",
		},
		{
			name:     "incomplete capture mismatch fails closed",
			input:    "synthetic_server_dh_control_checkpoint result=capture_incomplete\n",
			boundary: boundary("nonce_mismatch", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
		{
			name:     "incomplete capture control mismatch fails closed",
			input:    "synthetic_server_dh_control_checkpoint result=capture_incomplete\n",
			boundary: boundary("unknown", "decrypt_entered"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
		{
			name:     "control not entered requires unknown boundary pair",
			input:    "synthetic_server_dh_control_checkpoint result=control_not_entered\n",
			boundary: boundary("unknown", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=control_not_entered",
		},
		{
			name:     "control not entered actual mismatch fails closed",
			input:    "synthetic_server_dh_control_checkpoint result=control_not_entered\n",
			boundary: boundary("nonce_mismatch", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
		{
			name:     "control not entered control mismatch fails closed",
			input:    "synthetic_server_dh_control_checkpoint result=control_not_entered\n",
			boundary: boundary("unknown", "decrypt_entered"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
		{
			name:     "absent marker prints unknown",
			boundary: boundary("unknown", "unknown"),
			want:     "synthetic_server_dh_control_checkpoint result=unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &webExchangeServerDhControlCheckpointMarkerWriter{}
			if _, err := writer.Write([]byte(tt.input)); err != nil {
				t.Fatal("checkpoint writer rejected synthetic output")
			}
			writer.flush()
			got := writer.lines(tt.boundary)
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("checkpoint writer did not emit the fixed expected result")
			}
		})
	}

	t.Run("duplicate emits one unknown result", func(t *testing.T) {
		writer := &webExchangeServerDhControlCheckpointMarkerWriter{}
		for range 2 {
			if _, err := writer.Write([]byte("synthetic_server_dh_control_checkpoint result=decrypt_entered\n")); err != nil {
				t.Fatal("checkpoint writer rejected duplicate synthetic output")
			}
		}
		writer.flush()
		got := writer.lines(boundary("unknown", "unknown"))
		if len(got) != 1 || got[0] != "synthetic_server_dh_control_checkpoint result=unknown" {
			t.Fatal("duplicate checkpoint did not fail closed")
		}
	})

	t.Run("overflow fails closed and zeroes retained bytes", func(t *testing.T) {
		writer := &webExchangeServerDhControlCheckpointMarkerWriter{}
		if _, err := writer.Write([]byte("synthetic_server_dh_control_checkpoint result=decrypt_entered " + strings.Repeat("x", 600) + "\n")); err != nil {
			t.Fatal("checkpoint writer rejected oversized synthetic output")
		}
		if len(writer.line) > 512 {
			t.Fatal("checkpoint writer exceeded its input bound")
		}
		retained := writer.line
		writer.flush()
		got := writer.lines(boundary("unknown", "unknown"))
		if len(got) != 1 || got[0] != "synthetic_server_dh_control_checkpoint result=unknown" {
			t.Fatal("overflow checkpoint did not fail closed")
		}
		for _, value := range retained {
			if value != 0 {
				t.Fatal("checkpoint writer retained input bytes")
			}
		}
	})
}

type webExchangeClientRow struct {
	Case              string `json:"case"`
	ClientStage       string `json:"client_stage"`
	ClientAbortReason string `json:"client_abort_reason"`
	PlainRequests     int    `json:"plain_requests_on_socket"`
	RetryReusedSocket bool   `json:"retry_reused_socket"`
	ThirdConstructor  string `json:"third_constructor"`
	ProtoError404     bool   `json:"proto_error_404"`
}

type webExchangeReportRow struct {
	Case              string `json:"case"`
	ClientStage       string `json:"client_stage"`
	ClientAbortReason string `json:"client_abort_reason"`
	PlainRequests     int    `json:"plain_requests_on_socket"`
	RetryReusedSocket bool   `json:"retry_reused_socket"`
	ThirdConstructor  string `json:"third_constructor"`
	ServerStage       string `json:"server_stage"`
	ServerCause       string `json:"server_cause"`
	ProtoError404     bool   `json:"proto_error_404"`
	KeySaved          bool   `json:"key_saved"`
}

type webExchangeReport struct {
	Format string                 `json:"format"`
	Cases  []webExchangeReportRow `json:"cases"`
}

type webExchangeGotdControlObservation struct {
	clientStage       string
	clientAbortReason string
	plainRequests     int
	thirdConstructor  string
	serverStage       string
	serverCause       string
	keySaved          bool
	failures          webExchangeGotdControlFailures
}

type webExchangeGotdControlFailures uint16

const (
	webExchangeGotdFailureExchange webExchangeGotdControlFailures = 1 << iota
	webExchangeGotdFailureSaveObservation
	webExchangeGotdFailureClientClose
	webExchangeGotdFailureServeUnexpectedEOF
	webExchangeGotdFailureServeCanceled
	webExchangeGotdFailureServeDeadline
	webExchangeGotdFailureServeError
	webExchangeGotdFailureServeTimeout
	webExchangeGotdFailureForcedStopError
	webExchangeGotdFailureForcedStopTimeout
)

func (f *webExchangeGotdControlFailures) add(failure webExchangeGotdControlFailures) {
	*f |= failure
}

func (f *webExchangeGotdControlFailures) has(failure webExchangeGotdControlFailures) bool {
	return *f&failure != 0
}

func (f *webExchangeGotdControlFailures) first() webExchangeGotdControlFailures {
	for _, failure := range [...]webExchangeGotdControlFailures{
		webExchangeGotdFailureExchange,
		webExchangeGotdFailureSaveObservation,
		webExchangeGotdFailureClientClose,
		webExchangeGotdFailureServeUnexpectedEOF,
		webExchangeGotdFailureServeCanceled,
		webExchangeGotdFailureServeDeadline,
		webExchangeGotdFailureServeError,
		webExchangeGotdFailureServeTimeout,
		webExchangeGotdFailureForcedStopError,
		webExchangeGotdFailureForcedStopTimeout,
	} {
		if f.has(failure) {
			return failure
		}
	}
	return 0
}

type webExchangeConfig struct {
	Origin    string                     `json:"origin"`
	PublicKey webExchangePublicKey       `json:"public_key"`
	Cases     map[string]webExchangeCase `json:"cases"`
}

type webExchangePublicKey struct {
	Modulus     string `json:"modulus"`
	Exponent    string `json:"exponent"`
	Fingerprint string `json:"fingerprint"`
}

type webExchangeCase struct {
	Endpoint string `json:"endpoint"`
}

// TestWebClientExchangeExperiment is enabled only by the dedicated CI runner.
// It joins the shipped Web authorizer to real, isolated Server instances and
// writes only Security's fixed observation fields to the CI artifact.
func TestWebClientExchangeExperiment(t *testing.T) {
	if os.Getenv("WEB_EXCHANGE_CI_ONLY") != "1" {
		t.Skip("synthetic Web exchange experiment runs only in its dedicated CI job")
	}
	webRoot := os.Getenv("WEB_EXCHANGE_WEB_ROOT")
	artifactPath := os.Getenv("WEB_EXCHANGE_SANITIZED_ARTIFACT_PATH")
	if !filepath.IsAbs(webRoot) || !filepath.IsAbs(artifactPath) {
		t.Fatal("synthetic Web exchange runner configuration is unavailable")
	}
	artifactRoot, err := os.OpenRoot(filepath.Dir(artifactPath))
	if err != nil {
		t.Fatal("synthetic Web exchange report directory could not be opened")
	}
	defer func() {
		if err := artifactRoot.Close(); err != nil {
			t.Error("synthetic Web exchange report directory could not be closed")
		}
	}()
	artifactName := filepath.Base(artifactPath)
	if err := writeWebExchangeReport(artifactRoot, artifactName, unknownWebExchangeReport()); err != nil {
		t.Fatal("synthetic Web exchange report could not be initialized")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Second)
	defer cancel()

	rsaKey, err := rsa.GenerateKey(newWebExchangeReader("rsa-key"), crypto.RSAKeyBits)
	if err != nil {
		t.Fatal("synthetic Web exchange identity generation failed")
	}
	privateKey := exchange.PrivateKey{RSA: rsaKey}
	keyInfo := webExchangePublicKey{
		Modulus:     fmt.Sprintf("%x", rsaKey.N),
		Exponent:    fmt.Sprintf("%x", rsaKey.E),
		Fingerprint: formatWebExchangeFingerprint(privateKey.Fingerprint()),
	}
	if len(keyInfo.Modulus) != 512 {
		t.Fatal("synthetic Web exchange identity has an invalid size")
	}
	if _, err := decodeWebExchangeFingerprint(keyInfo.Fingerprint); err != nil {
		t.Fatal("synthetic Web exchange identity has an invalid fingerprint")
	}
	if keyInfo.Exponent == "3" {
		keyInfo.Exponent = "03"
	} else if len(keyInfo.Exponent)%2 != 0 {
		keyInfo.Exponent = "0" + keyInfo.Exponent
	}

	clientZero := runWebExchangeGotdControl(ctx, privateKey)
	if clientZero.failures != 0 {
		report := unknownWebExchangeReport()
		row := &report.Cases[0]
		row.ClientStage = clientZero.clientStage
		row.ClientAbortReason = clientZero.clientAbortReason
		row.PlainRequests = clientZero.plainRequests
		row.ThirdConstructor = clientZero.thirdConstructor
		row.ServerStage = clientZero.serverStage
		row.ServerCause = clientZero.serverCause
		row.KeySaved = clientZero.keySaved
		if err := writeWebExchangeReport(artifactRoot, artifactName, report); err != nil {
			t.Fatal("synthetic Web exchange report could not record the gotd control result")
		}
		switch clientZero.failures.first() {
		case webExchangeGotdFailureExchange:
			t.Fatal("gotd control exchange failed")
		case webExchangeGotdFailureSaveObservation:
			t.Fatal("gotd control save was not observed before its deadline")
		case webExchangeGotdFailureClientClose:
			t.Fatal("gotd control client close failed")
		case webExchangeGotdFailureServeUnexpectedEOF:
			t.Fatal("gotd control server returned a truncated-frame error")
		case webExchangeGotdFailureServeCanceled:
			t.Fatal("gotd control server returned a canceled-context error")
		case webExchangeGotdFailureServeDeadline:
			t.Fatal("gotd control server returned a deadline error")
		case webExchangeGotdFailureServeError:
			t.Fatal("gotd control server returned an unexpected error")
		case webExchangeGotdFailureServeTimeout:
			t.Fatal("gotd control server did not stop before its teardown deadline")
		case webExchangeGotdFailureForcedStopError:
			t.Fatal("gotd control server returned an error after forced stop")
		case webExchangeGotdFailureForcedStopTimeout:
			t.Fatal("gotd control server did not stop after forced cancellation")
		default:
			t.Fatal("gotd control failed with an unclassified test-harness reason")
		}
	}
	if clientZero.clientStage != "dh_gen_verified" {
		t.Fatal("gotd control did not reach dh_gen_verified")
	}
	if clientZero.clientAbortReason != "none" {
		t.Fatal("gotd control recorded a client abort")
	}
	if !clientZero.keySaved {
		t.Fatal("gotd control did not save an auth key")
	}
	if clientZero.serverStage != "unknown" || clientZero.serverCause != "unknown" {
		t.Fatal("gotd control emitted an exchange stage log")
	}

	servers := make(map[string]*webExchangeServer, 6)
	for _, caseName := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		servers[caseName] = startWebExchangeServer(t, ctx, privateKey, caseName)
	}

	tempRoot := t.TempDir()
	configRoot, err := os.OpenRoot(tempRoot)
	if err != nil {
		t.Fatal("synthetic Web exchange configuration directory could not be opened")
	}
	defer func() {
		if err := configRoot.Close(); err != nil {
			t.Error("synthetic Web exchange configuration directory could not be closed")
		}
	}()
	clientRowsPath := filepath.Join(tempRoot, "client-observations.json")
	defer func() {
		if err := os.RemoveAll(clientRowsPath); err != nil {
			t.Error("synthetic Web client observation file cleanup failed")
		}
		if err := os.RemoveAll(clientRowsPath + ".tmp"); err != nil {
			t.Error("synthetic Web client observation temporary file cleanup failed")
		}
	}()
	config := webExchangeConfig{Origin: webExchangeOrigin, PublicKey: keyInfo, Cases: make(map[string]webExchangeCase, len(servers))}
	for name, server := range servers {
		config.Cases[name] = webExchangeCase{Endpoint: server.endpoint}
	}
	if err := writeWebExchangePrivateFile(configRoot, "config.json", config); err != nil {
		t.Fatal("synthetic Web exchange configuration could not be written")
	}
	clientCtx, stopClient := context.WithTimeout(ctx, 75*time.Second)
	defer stopClient()
	command := exec.CommandContext(clientCtx, "node", "node_modules/vitest/vitest.mjs", "run", "--pool=threads", "--maxWorkers=1", "--testNamePattern=^runs isolated shipped-Web exchanges against synthetic server instances$", "src/tests/websocketExchangeExperiment.test.ts")
	command.Dir = webRoot
	command.Env = append(os.Environ(),
		"WEB_EXCHANGE_CONFIG_PATH="+filepath.Join(tempRoot, "config.json"),
		"WEB_EXCHANGE_CLIENT_ROWS_PATH="+clientRowsPath,
	)
	markerOutput := &webExchangePreRequestMarkerWriter{}
	boundaryOutput := &webExchangeServerDhBoundaryMarkerWriter{}
	checkpointOutput := &webExchangeServerDhControlCheckpointMarkerWriter{}
	command.Stdout = io.MultiWriter(markerOutput, boundaryOutput, checkpointOutput)
	command.Stderr = io.Discard
	runErr := command.Run()
	markerOutput.flush()
	boundaryOutput.flush()
	checkpointOutput.flush()
	for _, marker := range markerOutput.lines() {
		t.Log(marker)
	}
	for _, marker := range boundaryOutput.lines() {
		fmt.Println(marker)
	}
	for _, marker := range checkpointOutput.lines(boundaryOutput.lines()) {
		fmt.Println(marker)
	}

	clientRows, outputFailureCategory := readWebExchangeClientRows(clientRowsPath)
	if outputFailureCategory != "" {
		t.Log(formatWebExchangeClientOutputFailure(outputFailureCategory))
	}
	clientRowsByCase := make(map[string]webExchangeClientRow, len(clientRows))
	for _, row := range clientRows {
		clientRowsByCase[row.Case] = row
	}
	if row := clientRowsByCase["A"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["A"].store, 1)
	}
	if row := clientRowsByCase["B"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["B"].store, 1)
	}
	if row := clientRowsByCase["C"]; row.ClientStage == "dh_gen_verified" {
		waitWebExchangeSaves(t, servers["C"].store, 2)
	}
	report := combineWebExchangeRows(clientRows, servers, clientZero)
	if err := writeWebExchangeReport(artifactRoot, artifactName, report); err != nil {
		t.Fatal("synthetic Web exchange report could not be written")
	}
	switch failure, missingCase := classifyWebExchangeClientOutputFailure(runErr, outputFailureCategory == "", clientRows); failure {
	case "rows_unavailable":
		t.Fatal("synthetic Web client case observations could not be read")
	case "case_missing":
		t.Fatalf("shipped Web client did not return an observation for case %s", missingCase)
	case "client_assertions":
		t.Fatal("shipped Web client assertions failed after recording every case")
	}
	if err := validateWebExchangeReport(report); err != nil {
		t.Fatal("synthetic Web exchange observations violated the fixed experiment contract")
	}
	if report.Cases[0].Case != "C0" || !report.Cases[0].KeySaved || report.Cases[0].ServerStage != "unknown" || report.Cases[0].ServerCause != "unknown" {
		t.Fatal("gotd control observation did not match its completion contract")
	}
	if caseRow(report, "B").ServerStage != "read_client_dh" || caseRow(report, "B").ServerCause != "unexpected_type" || caseRow(report, "B").ProtoError404 || !caseRow(report, "B").RetryReusedSocket || caseRow(report, "B").ThirdConstructor != "req_pq_multi" {
		t.Fatal("injected shipped-Web retry did not produce the expected retry control tuple")
	}
	if caseRow(report, "N1").ServerStage != "read_client_dh" || caseRow(report, "N1").ServerCause != "unexpected_type" || caseRow(report, "N1").ProtoError404 || caseRow(report, "N1").KeySaved || caseRow(report, "N1").ThirdConstructor != "req_pq_multi" {
		t.Fatal("next req_pq control did not produce the expected server tuple")
	}
	for _, name := range []string{"N2", "N3"} {
		row := caseRow(report, name)
		if row.ProtoError404 != true || row.ServerStage != "unknown" || row.ServerCause != "unknown" || row.KeySaved {
			t.Fatal("malformed client-DH control did not stay in the protocol-error path")
		}
	}
}

type webExchangeServer struct {
	endpoint string
	store    *webExchangeStore
	logs     *webExchangeLogHandler
}

func startWebExchangeServer(t *testing.T, parent context.Context, key exchange.PrivateKey, caseName string) *webExchangeServer {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(parent, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("synthetic WebSocket listener could not start")
	}
	store := &webExchangeStore{AuthKeyStore: NewMemoryAuthKeyStore(), saves: make(chan struct{}, 8)}
	logs := &webExchangeLogHandler{}
	server := New(key, 2, store, nil, slog.New(logs))
	server.cipher = crypto.NewServerCipher(newWebExchangeReader("server-" + caseName))
	server.SetWebSocketOriginPatterns([]string{webExchangeOrigin})
	server.SetHandshakeTimeout(5 * time.Second)
	ctx, stop := context.WithCancel(parent)
	served := make(chan error, 1)
	go func() { served <- server.ServeWebSocket(ctx, listener) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-served:
			if err != nil {
				t.Error("synthetic WebSocket server did not stop cleanly")
			}
		case <-time.After(5 * time.Second):
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Error("synthetic WebSocket listener cleanup failed")
			}
			select {
			case err := <-served:
				if err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error("synthetic WebSocket server did not stop cleanly")
				}
			case <-time.After(1 * time.Second):
				t.Error("synthetic WebSocket server did not stop")
			}
		}
	})
	return &webExchangeServer{endpoint: "ws://" + listener.Addr().String() + "/apiws", store: store, logs: logs}
}

func runWebExchangeGotdControl(ctx context.Context, key exchange.PrivateKey) (result webExchangeGotdControlObservation) {
	store := &webExchangeStore{AuthKeyStore: NewMemoryAuthKeyStore(), saves: make(chan struct{}, 2)}
	logs := &webExchangeLogHandler{}
	server := New(key, 2, store, nil, slog.New(logs))
	server.cipher = crypto.NewServerCipher(newWebExchangeReader("server-c0"))
	client, serverConn := transport.Intermediate.Pipe()
	serverCtx, stopServer := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() { served <- server.ServeConn(serverCtx, serverConn) }()
	var closeClientOnce sync.Once
	var closeClientErr error
	closeClient := func() {
		closeClientOnce.Do(func() { closeClientErr = client.Close() })
	}
	defer func() {
		closeClient()
		if closeClientErr != nil {
			result.failures.add(webExchangeGotdFailureClientClose)
		}
		select {
		case serveErr := <-served:
			if failure := webExchangeGotdServeFailure(serveErr); failure != 0 {
				result.failures.add(failure)
			}
		case <-time.After(5 * time.Second):
			result.failures.add(webExchangeGotdFailureServeTimeout)
			stopServer()
			select {
			case serveErr := <-served:
				if serveErr != nil {
					result.failures.add(webExchangeGotdFailureForcedStopError)
				}
			case <-time.After(time.Second):
				result.failures.add(webExchangeGotdFailureForcedStopTimeout)
			}
		}
		stopServer()
		result.serverStage, result.serverCause = logs.snapshot()
		result.keySaved = store.saved.Load() > 0
	}()
	_, err := exchange.NewExchanger(client, 2).
		WithRand(newWebExchangeReader("gotd-c0-client")).
		Client([]exchange.PublicKey{{RSA: &key.RSA.PublicKey}}).
		Run(ctx)
	if err != nil {
		result.clientAbortReason = webExchangeClientAbortReason(err)
		result.failures.add(webExchangeGotdFailureExchange)
		return result
	}
	result.clientStage = "dh_gen_verified"
	result.clientAbortReason = "none"
	result.plainRequests = 3
	result.thirdConstructor = "set_client_dh_params"
	if !webExchangeSavesObserved(store, 1, 2*time.Second) {
		result.failures.add(webExchangeGotdFailureSaveObservation)
	}
	return result
}

func webExchangeClientAbortReason(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrClosedPipe), errors.Is(err, net.ErrClosed):
		return "transport_closed"
	default:
		return "unknown"
	}
}

func webExchangeGotdServeFailure(err error) webExchangeGotdControlFailures {
	// C0 closes its in-memory client pipe before collecting ServeConn's result.
	// Treat that pipe's close signal as teardown; other unexpected errors remain failures.
	if err == nil || isDisconnect(err) || errors.Is(err, io.ErrClosedPipe) {
		return 0
	}
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return webExchangeGotdFailureServeUnexpectedEOF
	case errors.Is(err, context.Canceled):
		return webExchangeGotdFailureServeCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return webExchangeGotdFailureServeDeadline
	default:
		return webExchangeGotdFailureServeError
	}
}

type webExchangeStore struct {
	AuthKeyStore

	saves chan struct{}
	saved atomic.Int32
}

func (s *webExchangeStore) Save(ctx context.Context, key crypto.AuthKey) error {
	if err := s.AuthKeyStore.Save(ctx, key); err != nil {
		return err
	}
	s.saved.Add(1)
	select {
	case s.saves <- struct{}{}:
	default:
	}
	return nil
}

func waitWebExchangeSaves(t *testing.T, store *webExchangeStore, count int32) {
	t.Helper()
	if !webExchangeSavesObserved(store, count, 2*time.Second) {
		t.Fatal("synthetic exchange did not save the expected auth key")
	}
}

func webExchangeSavesObserved(store *webExchangeStore, count int32, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for store.saved.Load() < count {
		select {
		case <-store.saves:
		case <-deadline.C:
			return false
		}
	}
	return true
}

type webExchangeLogHandler struct {
	mu    sync.Mutex
	stage string
	cause string
}

func (h *webExchangeLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *webExchangeLogHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *webExchangeLogHandler) WithGroup(string) slog.Handler            { return h }
func (h *webExchangeLogHandler) Handle(_ context.Context, record slog.Record) error {
	var category, stage, cause string
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "category":
			category = attr.Value.String()
		case "stage":
			stage = attr.Value.String()
		case "cause":
			cause = attr.Value.String()
		}
		return true
	})
	if category == string(serverFailureExchange) && allowedWebExchangeStage(stage) && allowedWebExchangeCause(cause) {
		h.mu.Lock()
		if h.stage == "" {
			h.stage, h.cause = stage, cause
		}
		h.mu.Unlock()
	}
	return nil
}

func (h *webExchangeLogHandler) snapshot() (string, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stage == "" {
		return "unknown", "unknown"
	}
	return h.stage, h.cause
}

type webExchangeReader struct {
	mu      sync.Mutex
	seed    []byte
	counter uint64
	block   []byte
	offset  int
}

func newWebExchangeReader(seed string) *webExchangeReader {
	return &webExchangeReader{seed: []byte(seed)}
}

func (r *webExchangeReader) Read(out []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for written := 0; written < len(out); {
		if r.offset == len(r.block) {
			input := make([]byte, len(r.seed)+8)
			copy(input, r.seed)
			binary.LittleEndian.PutUint64(input[len(r.seed):], r.counter)
			r.counter++
			block := sha256.Sum256(input)
			r.block = block[:]
			r.offset = 0
		}
		copied := copy(out[written:], r.block[r.offset:])
		written += copied
		r.offset += copied
	}
	return len(out), nil
}

func unknownWebExchangeReport() webExchangeReport {
	rows := make([]webExchangeReportRow, 0, 7)
	rows = append(rows, unknownWebExchangeRow("C0"))
	for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		rows = append(rows, unknownWebExchangeRow(name))
	}
	return webExchangeReport{Format: "teagram-web-authorizer-exchange-v1", Cases: rows}
}

func unknownWebExchangeRow(name string) webExchangeReportRow {
	return webExchangeReportRow{
		Case: name, ClientStage: "unknown", ClientAbortReason: "unknown", ThirdConstructor: "unknown",
		ServerStage: "unknown", ServerCause: "unknown",
	}
}

func readWebExchangeClientRows(path string) ([]webExchangeClientRow, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "file_read"
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var encodedRows []json.RawMessage
	if err := decoder.Decode(&encodedRows); err != nil || encodedRows == nil {
		return nil, "json_decode"
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, "json_decode"
	}
	rows := make([]webExchangeClientRow, 0, len(encodedRows))
	seen := make(map[string]struct{}, len(encodedRows))
	for _, encodedRow := range encodedRows {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(encodedRow, &fields); err != nil || !hasExactWebExchangeClientFields(fields) {
			return nil, "json_decode"
		}
		rowDecoder := json.NewDecoder(bytes.NewReader(encodedRow))
		rowDecoder.DisallowUnknownFields()
		var row webExchangeClientRow
		if err := rowDecoder.Decode(&row); err != nil {
			return nil, "json_decode"
		}
		if _, duplicate := seen[row.Case]; duplicate || !validWebExchangeClientRow(row) {
			return nil, "json_decode"
		}
		seen[row.Case] = struct{}{}
		rows = append(rows, row)
	}
	return rows, ""
}

func hasExactWebExchangeClientFields(fields map[string]json.RawMessage) bool {
	if len(fields) != 7 {
		return false
	}
	for _, name := range []string{
		"case", "client_stage", "client_abort_reason", "plain_requests_on_socket",
		"retry_reused_socket", "third_constructor", "proto_error_404",
	} {
		if _, exists := fields[name]; !exists {
			return false
		}
	}
	return true
}

func validWebExchangeClientRow(row webExchangeClientRow) bool {
	return slices.Contains([]string{"A", "B", "C", "N1", "N2", "N3"}, row.Case) &&
		allowedWebExchangeClientStage(row.ClientStage) && allowedWebExchangeAbort(row.ClientAbortReason) &&
		allowedWebExchangeConstructor(row.ThirdConstructor) && row.PlainRequests >= 0 && row.PlainRequests <= 12
}

func classifyWebExchangeClientOutputFailure(runErr error, rowsReady bool, rows []webExchangeClientRow) (string, string) {
	if !rowsReady {
		return "rows_unavailable", ""
	}
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		seen[row.Case] = struct{}{}
	}
	for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		if _, ok := seen[name]; !ok {
			return "case_missing", name
		}
	}
	if runErr != nil {
		return "client_assertions", ""
	}
	return "", ""
}

const webExchangeClientOutputFailureMarker = "synthetic_client_output_failure"

func formatWebExchangeClientOutputFailure(category string) string {
	if category != "file_read" && category != "json_decode" {
		category = "unknown"
	}
	return webExchangeClientOutputFailureMarker + " category=" + category
}

const webExchangePreRequestFailureMarker = "synthetic_pre_request_failure"

var (
	webExchangePreRequestFailurePattern = regexp.MustCompile(`^synthetic_pre_request_failure case=(A|B|C|N1|N2|N3) category=(client_initialization|transport_selection|transport_construction|unknown)$`)
	webExchangePreRequestCasePattern    = regexp.MustCompile(`^synthetic_pre_request_failure case=(A|B|C|N1|N2|N3)([[:space:]]|$)`)
)

func sanitizeWebExchangePreRequestFailureLine(line string) []string {
	markerIndex := strings.Index(line, webExchangePreRequestFailureMarker)
	if markerIndex < 0 {
		return nil
	}
	candidate := strings.TrimSpace(line[markerIndex:])
	if webExchangePreRequestFailurePattern.MatchString(candidate) {
		return []string{candidate}
	}
	if match := webExchangePreRequestCasePattern.FindStringSubmatch(candidate); len(match) >= 2 {
		return []string{formatWebExchangePreRequestFailure(match[1], "unknown")}
	}
	return unknownWebExchangePreRequestFailures()
}

func formatWebExchangePreRequestFailure(caseName, category string) string {
	return webExchangePreRequestFailureMarker + " case=" + caseName + " category=" + category
}

func unknownWebExchangePreRequestFailures() []string {
	markers := make([]string, 0, 6)
	for _, caseName := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		markers = append(markers, formatWebExchangePreRequestFailure(caseName, "unknown"))
	}
	return markers
}

type webExchangePreRequestMarkerWriter struct {
	line     []byte
	overflow bool
	markers  map[string]string
}

func (w *webExchangePreRequestMarkerWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			w.processLine()
			continue
		}
		if len(w.line) < 512 {
			w.line = append(w.line, value)
		} else {
			w.overflow = true
		}
	}
	return len(data), nil
}

func (w *webExchangePreRequestMarkerWriter) processLine() {
	line := string(w.line)
	if w.overflow {
		line += " overflow"
	}
	if w.markers == nil {
		w.markers = make(map[string]string, 6)
	}
	for _, marker := range sanitizeWebExchangePreRequestFailureLine(line) {
		match := webExchangePreRequestCasePattern.FindStringSubmatch(marker)
		if len(match) >= 2 {
			if _, exists := w.markers[match[1]]; !exists {
				w.markers[match[1]] = marker
			}
		}
	}
	w.line = w.line[:0]
	w.overflow = false
}

func (w *webExchangePreRequestMarkerWriter) flush() {
	if len(w.line) != 0 || w.overflow {
		w.processLine()
	}
}

func (w *webExchangePreRequestMarkerWriter) lines() []string {
	lines := make([]string, 0, len(w.markers))
	for _, caseName := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		if marker, ok := w.markers[caseName]; ok {
			lines = append(lines, marker)
		}
	}
	return lines
}

const webExchangeServerDhBoundaryMarker = "synthetic_server_dh_boundary"

var (
	webExchangeServerDhBoundaryPattern        = regexp.MustCompile(`^synthetic_server_dh_boundary subject=(actual|control) result=(decrypt_entered|not_received|envelope_rejected|constructor_rejected|nonce_mismatch|unknown)$`)
	webExchangeServerDhBoundarySubjectPattern = regexp.MustCompile(`^synthetic_server_dh_boundary subject=(actual|control)([[:space:]]|$)`)
)

func sanitizeWebExchangeServerDhBoundaryLine(line []byte) []string {
	markerIndex := bytes.Index(line, []byte(webExchangeServerDhBoundaryMarker))
	if markerIndex < 0 {
		return nil
	}
	candidate := line[markerIndex:]
	if match := webExchangeServerDhBoundaryPattern.FindSubmatch(candidate); len(match) == 3 {
		return []string{formatWebExchangeServerDhBoundary(string(match[1]), string(match[2]))}
	}
	if match := webExchangeServerDhBoundarySubjectPattern.FindSubmatch(candidate); len(match) >= 2 {
		return []string{formatWebExchangeServerDhBoundary(string(match[1]), "unknown")}
	}
	return []string{
		formatWebExchangeServerDhBoundary("actual", "unknown"),
		formatWebExchangeServerDhBoundary("control", "unknown"),
	}
}

func formatWebExchangeServerDhBoundary(subject, result string) string {
	return webExchangeServerDhBoundaryMarker + " subject=" + subject + " result=" + result
}

type webExchangeServerDhBoundaryMarkerWriter struct {
	line       []byte
	overflow   bool
	seen       map[string]bool
	markers    map[string]string
	unknownAll bool
}

func (w *webExchangeServerDhBoundaryMarkerWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			w.processLine()
			continue
		}
		if len(w.line) < 512 {
			w.line = append(w.line, value)
		} else {
			w.overflow = true
		}
	}
	return len(data), nil
}

func (w *webExchangeServerDhBoundaryMarkerWriter) processLine() {
	if w.seen == nil {
		w.seen = make(map[string]bool, 2)
		w.markers = make(map[string]string, 2)
	}
	markers := sanitizeWebExchangeServerDhBoundaryLine(w.line)
	if w.overflow {
		for i, marker := range markers {
			match := webExchangeServerDhBoundaryPattern.FindStringSubmatch(marker)
			if len(match) == 3 {
				markers[i] = formatWebExchangeServerDhBoundary(match[1], "unknown")
			}
		}
	}
	if len(markers) == 2 {
		w.unknownAll = true
	}
	for _, marker := range markers {
		match := webExchangeServerDhBoundaryPattern.FindStringSubmatch(marker)
		if len(match) != 3 {
			continue
		}
		subject := match[1]
		if w.seen[subject] {
			w.markers[subject] = formatWebExchangeServerDhBoundary(subject, "unknown")
			continue
		}
		w.seen[subject] = true
		w.markers[subject] = marker
	}
	for i := range w.line {
		w.line[i] = 0
	}
	w.line = w.line[:0]
	w.overflow = false
}

func (w *webExchangeServerDhBoundaryMarkerWriter) flush() {
	if len(w.line) != 0 || w.overflow {
		w.processLine()
	}
}

func (w *webExchangeServerDhBoundaryMarkerWriter) lines() []string {
	if w.unknownAll {
		return []string{
			formatWebExchangeServerDhBoundary("actual", "unknown"),
			formatWebExchangeServerDhBoundary("control", "unknown"),
		}
	}
	lines := make([]string, 0, len(w.markers))
	for _, subject := range []string{"actual", "control"} {
		if marker, ok := w.markers[subject]; ok {
			lines = append(lines, marker)
		}
	}
	return lines
}

const webExchangeServerDhControlCheckpointMarker = "synthetic_server_dh_control_checkpoint"

var webExchangeServerDhControlCheckpointPattern = regexp.MustCompile(`^synthetic_server_dh_control_checkpoint result=(capture_incomplete|control_not_entered|decrypt_entered|unknown)$`)

func sanitizeWebExchangeServerDhControlCheckpointLine(line []byte) string {
	markerIndex := bytes.Index(line, []byte(webExchangeServerDhControlCheckpointMarker))
	if markerIndex < 0 {
		return formatWebExchangeServerDhControlCheckpoint("unknown")
	}
	candidate := line[markerIndex:]
	if match := webExchangeServerDhControlCheckpointPattern.FindSubmatch(candidate); len(match) == 2 {
		return formatWebExchangeServerDhControlCheckpoint(string(match[1]))
	}
	return formatWebExchangeServerDhControlCheckpoint("unknown")
}

func formatWebExchangeServerDhControlCheckpoint(result string) string {
	return webExchangeServerDhControlCheckpointMarker + " result=" + result
}

type webExchangeServerDhControlCheckpointMarkerWriter struct {
	line      []byte
	overflow  bool
	seen      bool
	duplicate bool
	result    string
}

func (w *webExchangeServerDhControlCheckpointMarkerWriter) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			w.processLine()
			continue
		}
		if len(w.line) < 512 {
			w.line = append(w.line, value)
		} else {
			w.overflow = true
		}
	}
	return len(data), nil
}

func (w *webExchangeServerDhControlCheckpointMarkerWriter) processLine() {
	if bytes.Contains(w.line, []byte(webExchangeServerDhControlCheckpointMarker)) {
		result := sanitizeWebExchangeServerDhControlCheckpointLine(w.line)
		match := webExchangeServerDhControlCheckpointPattern.FindStringSubmatch(result)
		if w.overflow || len(match) != 2 {
			result = formatWebExchangeServerDhControlCheckpoint("unknown")
			match = webExchangeServerDhControlCheckpointPattern.FindStringSubmatch(result)
		}
		if w.seen {
			w.duplicate = true
			w.result = "unknown"
		} else {
			w.seen = true
			w.result = match[1]
		}
	}
	for i := range w.line {
		w.line[i] = 0
	}
	w.line = w.line[:0]
	w.overflow = false
}

func (w *webExchangeServerDhControlCheckpointMarkerWriter) flush() {
	if len(w.line) != 0 || w.overflow {
		w.processLine()
	}
}

func (w *webExchangeServerDhControlCheckpointMarkerWriter) lines(boundaryLines []string) []string {
	result := "unknown"
	if w.seen && !w.duplicate {
		result = w.result
	}
	if !webExchangeServerDhControlCheckpointMatchesBoundary(result, boundaryLines) {
		result = "unknown"
	}
	return []string{formatWebExchangeServerDhControlCheckpoint(result)}
}

func webExchangeServerDhControlCheckpointMatchesBoundary(result string, boundaryLines []string) bool {
	if result == "unknown" {
		return true
	}
	if len(boundaryLines) != 2 {
		return false
	}
	seen := make(map[string]string, 2)
	for _, line := range boundaryLines {
		match := webExchangeServerDhBoundaryPattern.FindStringSubmatch(line)
		if len(match) != 3 {
			return false
		}
		if _, exists := seen[match[1]]; exists {
			return false
		}
		seen[match[1]] = match[2]
	}
	if result == "decrypt_entered" {
		return seen["control"] == "decrypt_entered"
	}
	return seen["actual"] == "unknown" && seen["control"] == "unknown"
}

func combineWebExchangeRows(clientRows []webExchangeClientRow, servers map[string]*webExchangeServer, c0 webExchangeGotdControlObservation) webExchangeReport {
	rowsByCase := make(map[string]webExchangeClientRow, len(clientRows))
	for _, row := range clientRows {
		rowsByCase[row.Case] = row
	}
	rows := []webExchangeReportRow{unknownWebExchangeRow("C0")}
	rows[0].ClientStage = "dh_gen_verified"
	rows[0].ClientAbortReason = "none"
	rows[0].PlainRequests = 3
	rows[0].ThirdConstructor = "set_client_dh_params"
	rows[0].KeySaved = c0.keySaved
	for _, name := range []string{"A", "B", "C", "N1", "N2", "N3"} {
		row, ok := rowsByCase[name]
		if !ok {
			rows = append(rows, unknownWebExchangeRow(name))
			continue
		}
		stage, cause := servers[name].logs.snapshot()
		rows = append(rows, webExchangeReportRow{
			Case: name, ClientStage: row.ClientStage, ClientAbortReason: row.ClientAbortReason,
			PlainRequests: row.PlainRequests, RetryReusedSocket: row.RetryReusedSocket,
			ThirdConstructor: row.ThirdConstructor, ServerStage: stage, ServerCause: cause,
			ProtoError404: row.ProtoError404, KeySaved: servers[name].store.saved.Load() > 0,
		})
	}
	return webExchangeReport{Format: "teagram-web-authorizer-exchange-v1", Cases: rows}
}

func validateWebExchangeReport(report webExchangeReport) error {
	if report.Format != "teagram-web-authorizer-exchange-v1" || len(report.Cases) != 7 {
		return errors.New("invalid report shape")
	}
	want := []string{"C0", "A", "B", "C", "N1", "N2", "N3"}
	for i, row := range report.Cases {
		if row.Case != want[i] || !allowedWebExchangeClientStage(row.ClientStage) || !allowedWebExchangeAbort(row.ClientAbortReason) || !allowedWebExchangeConstructor(row.ThirdConstructor) || !allowedWebExchangeStage(row.ServerStage) || !allowedWebExchangeCause(row.ServerCause) || row.PlainRequests < 0 || row.PlainRequests > 12 {
			return errors.New("invalid report value")
		}
	}
	return nil
}

func writeWebExchangePrivateFile(root *os.Root, name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return root.WriteFile(name, append(data, '\n'), 0o600)
}

func writeWebExchangeReport(root *os.Root, name string, report webExchangeReport) error {
	if err := validateWebExchangeReport(report); err != nil {
		return err
	}
	data, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return root.WriteFile(name, append(data, '\n'), 0o600)
}

func formatWebExchangeFingerprint(fingerprint int64) string {
	const digits = "0123456789abcdef"
	var output [16]byte
	for position := range output {
		shift := 4 * (len(output) - 1 - position)
		output[position] = digits[(fingerprint>>shift)&0xf]
	}
	return string(output[:])
}

func caseRow(report webExchangeReport, name string) webExchangeReportRow {
	for _, row := range report.Cases {
		if row.Case == name {
			return row
		}
	}
	return unknownWebExchangeRow(name)
}

func allowedWebExchangeClientStage(value string) bool {
	switch value {
	case "req_pq_sent", "res_pq_ok", "req_dh_sent", "server_dh_received", "server_dh_validated", "set_client_dh_sent", "dh_gen_verified", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeAbort(value string) bool {
	switch value {
	case "server_dh_validation", "response_mismatch", "transport_closed", "timeout", "none", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeConstructor(value string) bool {
	switch value {
	case "set_client_dh_params", "req_pq_multi", "req_pq", "req_dh_params", "other", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeStage(value string) bool {
	switch value {
	case "read_req_pq", "write_res_pq", "read_req_dh", "decode_inner", "write_server_dh", "read_client_dh", "decode_client_dh", "write_dh_gen", "unknown":
		return true
	default:
		return false
	}
}

func allowedWebExchangeCause(value string) bool {
	switch value {
	case "timeout", "peer_close", "unexpected_type", "decode", "write", "other", "unknown":
		return true
	default:
		return false
	}
}

func decodeWebExchangeFingerprint(value string) (uint64, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 8 {
		return 0, errors.New("invalid synthetic fingerprint")
	}
	var out uint64
	for _, b := range decoded {
		out = out<<8 | uint64(b)
	}
	return out, nil
}
