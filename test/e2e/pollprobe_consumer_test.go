package e2e_test

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func validatePollprobeRun(runErr error, output string, assertions []string) error {
	if runErr != nil {
		return errors.New("pollprobe process exited unsuccessfully")
	}
	for _, assertion := range assertions {
		if !strings.Contains(output, "assertion="+assertion+" result=pass") {
			return fmt.Errorf("pollprobe output omitted assertion %s", assertion)
		}
	}
	return nil
}

func safePollprobeLogoutField(key, value string) bool {
	switch key {
	case "count", "failed", "stopped_before_logout":
		if value == "" {
			return false
		}
		for _, char := range value {
			if char < '0' || char > '9' {
				return false
			}
		}
		_, err := strconv.ParseUint(value, 10, 64)
		return err == nil
	case "error_codes":
		if value == "" {
			return false
		}
		for code := range strings.SplitSeq(value, ",") {
			if code == "" {
				return false
			}
			for _, char := range code {
				if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
					return false
				}
			}
		}
		return true
	case "logout_causes":
		if value == "" {
			return true
		}
		for cause := range strings.SplitSeq(value, ",") {
			if !safePollprobeLogoutCause(cause) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func safePollprobeLogoutCause(value string) bool {
	switch value {
	case "DEADLINE_EXCEEDED", "CANCELED", "TRANSPORT_CLOSED", "CLIENT_STOPPED", "RECONNECT_FAILED", "RPC_ERROR", "RESPONSE_INVALID", "UNKNOWN":
		return true
	default:
		return false
	}
}

func TestPollProbeConsumersPreserveSafeLogoutEvidence(t *testing.T) {
	input := "assertion=logout_sessions result=fail count=4 failed=1 stopped_before_logout=0 error_codes=OPERATION_FAILED,LOGGED_OUT logout_causes=DEADLINE_EXCEEDED\n"
	want := "assertion=logout_sessions result=fail count=4 failed=1 stopped_before_logout=0 error_codes=OPERATION_FAILED,LOGGED_OUT logout_causes=DEADLINE_EXCEEDED"

	if got := safePollprobeAssertions(input); got != want {
		t.Errorf("group smoke sanitizer = %q, want %q", got, want)
	}
	if got := safePollprobeChannelAssertions(input); got != want {
		t.Errorf("channel smoke sanitizer = %q, want %q", got, want)
	}
}

func TestPollProbeConsumersDropUnsafeLogoutEvidenceAndNewFieldsElsewhere(t *testing.T) {
	input := strings.Join([]string{
		"assertion=logout_sessions result=fail count=-1 failed=1 stopped_before_logout=0 error_codes=OPERATION_FAILED,FAKE/TOKEN logout_causes=DEADLINE_EXCEEDED,NOT_APPROVED extra=kept",
		"assertion=account_identities result=pass count=4 stopped_before_logout=0 logout_causes=RPC_ERROR error_codes=PRIVATE",
	}, "\n")
	want := "assertion=logout_sessions result=fail failed=1 stopped_before_logout=0\nassertion=account_identities result=pass count=4"

	if got := safePollprobeAssertions(input); got != want {
		t.Errorf("group smoke sanitizer = %q, want %q", got, want)
	}
	if got := safePollprobeChannelAssertions(input); got != want {
		t.Errorf("channel smoke sanitizer = %q, want %q", got, want)
	}
}

func TestPollProbeExitStatusCannotBeBypassedByPassMarker(t *testing.T) {
	output := "assertion=logout_sessions result=pass count=4 failed=0 stopped_before_logout=0 logout_causes=\n"
	if err := validatePollprobeRun(errors.New("exit status 1"), output, []string{"logout_sessions"}); err == nil {
		t.Fatal("pollprobe pass marker bypassed a nonzero process exit")
	}
}
