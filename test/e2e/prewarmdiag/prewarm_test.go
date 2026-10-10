package prewarmdiag_test

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"github.com/teagramhq/teagram-server/test/e2e/prewarmdiag"
)

func TestReportFailureOnSuccess(t *testing.T) {
	var output bytes.Buffer

	if status := prewarmdiag.ReportFailure(&output, "", nil); status != 0 {
		t.Fatalf("status = %d, want 0", status)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestReportFailureOnPrewarmError(t *testing.T) {
	var output bytes.Buffer

	status := prewarmdiag.ReportFailure(&output, "", errors.New("synthetic setup detail"))
	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	const want = "e2e setup failed [setup:pgtest-prewarm]\npgtest prewarm: synthetic setup detail\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestReportFailureDoesNotAttestUnverifiedError(t *testing.T) {
	var output bytes.Buffer
	provenanceDir := t.TempDir()

	status := prewarmdiag.ReportFailure(
		&output,
		provenanceDir,
		errors.New("synthetic setup detail"),
	)
	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	const want = "e2e setup failed [setup:pgtest-prewarm]\npgtest prewarm: synthetic setup detail\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want marker and detail only", got)
	}
	entries, err := os.ReadDir(provenanceDir)
	if err != nil {
		t.Fatalf("read provenance directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("provenance entries = %v, want none", entries)
	}
}
