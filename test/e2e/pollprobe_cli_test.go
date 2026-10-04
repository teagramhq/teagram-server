package e2e_test

import (
	"context"
	osExec "os/exec"
	"strings"
	"testing"
	"time"
)

func TestPollProbeRejectsMissingTrustInputsBeforeConnecting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := osExec.CommandContext(ctx, "go", "run", "../../cmd/pollprobe")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("pollprobe without explicit trust inputs succeeded")
	}
	if !strings.Contains(string(output), "assertion=configuration_validated result=fail") {
		t.Fatalf("pollprobe did not report a sanitized configuration assertion: %s", output)
	}
	if strings.Contains(string(output), "connection refused") || strings.Contains(string(output), "no such host") {
		t.Fatalf("pollprobe contacted the endpoint before validating trust inputs: %s", output)
	}
}
