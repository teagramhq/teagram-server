package e2e_test

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	osExec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testSmokePollProbeChannels(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	passwords := [4]string{
		"pollprobe-channels-password-a",
		"pollprobe-channels-password-b",
		"pollprobe-channels-password-c",
		"pollprobe-channels-password-d",
	}
	usernames := []string{"synthpoll_a", "synthpoll_b", "synthpoll_c", "synthpoll_d"}
	for index, username := range usernames {
		seedUsernameUser(t, f.ctx, f.store, username, "Synthetic", passwords[index])
	}

	credentialDir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(credentialDir, 0o700); err != nil {
		t.Fatal("create pollprobe channel credential fixture")
	}
	for index, username := range usernames {
		if err := os.WriteFile(filepath.Join(credentialDir, username+".password"), []byte(passwords[index]+"\n"), 0o600); err != nil {
			t.Fatal("write pollprobe channel credential fixture")
		}
	}

	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		t.Fatal("encode pollprobe channel fixture server key")
	}
	keyID := sha256.Sum256(der)
	keyPath := filepath.Join(t.TempDir(), "server-public.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal("write pollprobe channel fixture server key")
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", f.port)
	ctx, cancel := context.WithTimeout(f.ctx, 80*time.Second)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("locate home for pollprobe channel fixture process")
	}
	cmd := osExec.CommandContext(ctx, "go", "run", "../../cmd/pollprobe", // #nosec G204 -- fixed local Go invocation runs against the isolated smoke fixture.
		"--scenario", "channels",
		"--endpoint", endpoint,
		"--rsa-public-key", keyPath,
		"--rsa-key-id", hex.EncodeToString(keyID[:]),
		"--credentials-dir", credentialDir,
	)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("pollprobe failed its isolated channel scenario: %s", safePollprobeChannelAssertions(string(output)))
	}
	outputText := string(output)
	for _, assertion := range channelProbeAssertions {
		if !strings.Contains(outputText, "assertion="+assertion+" result=pass") {
			t.Errorf("pollprobe channel fixture omitted assertion %s", assertion)
		}
	}
	if !channelProbeAssertionsAppearInOrder(outputText,
		"channel_vote_no_pts_recovery",
		"channel_difference_recovery",
		"channel_reconnect_live_capture",
		"channel_removal_capture_suppressed",
		"channel_recovery_poll_closed",
		"channel_repeated_close_idempotent",
		"channel_closed_poll_vote_denied",
		"channel_closed_poll_reconnect",
	) {
		t.Error("pollprobe channel fixture omitted the required vote, close, repeated-close, late-vote, reconnect sequence")
	}
	for _, secret := range passwords {
		if strings.Contains(outputText, secret) {
			t.Error("pollprobe channel output exposed a fixture password")
		}
	}
	for _, username := range usernames {
		if strings.Contains(outputText, username) {
			t.Error("pollprobe channel output exposed an account identity")
		}
	}
	for _, content := range []string{"Supergroup probe poll", "Broadcast probe poll", "A and B are correct"} {
		if strings.Contains(outputText, content) {
			t.Error("pollprobe channel output exposed poll content")
		}
	}
}

var channelProbeAssertions = []string{
	"account_identities",
	"synthetic_peers_resolved",
	"synthetic_supergroup_created",
	"synthetic_supergroup_members",
	"channel_anonymous_non_voter_denied",
	"channel_anonymous_poll_privacy",
	"channel_public_voter_pagination",
	"channel_quiz_privacy",
	"channel_default_poll_ban",
	"channel_member_close_denied",
	"channel_admin_close",
	"channel_vote_no_pts_recovery",
	"channel_difference_recovery",
	"channel_reconnect_live_capture",
	"channel_removal_capture_suppressed",
	"channel_removed_member_denied",
	"channel_outsider_denied",
	"channel_recovery_poll_closed",
	"channel_repeated_close_idempotent",
	"channel_closed_poll_vote_denied",
	"channel_closed_poll_reconnect",
	"synthetic_broadcast_created",
	"synthetic_broadcast_members",
	"broadcast_public_voters_denied",
	"broadcast_subscriber_post_denied",
	"broadcast_anonymous_non_voter_denied",
	"broadcast_anonymous_poll_privacy",
	"broadcast_quiz_privacy",
	"broadcast_member_close_denied",
	"broadcast_admin_close",
	"broadcast_close_recovery",
	"broadcast_outsider_denied",
	"broadcast_repeated_close_idempotent",
	"broadcast_closed_poll_vote_denied",
	"broadcast_closed_poll_reconnect",
	"channel_outsider_global_pts_unchanged",
	"channels_scenario_complete",
	"logout_sessions",
}

func channelProbeAssertionsAppearInOrder(output string, assertions ...string) bool {
	searchFrom := 0
	for _, assertion := range assertions {
		marker := "assertion=" + assertion + " result=pass"
		index := strings.Index(output[searchFrom:], marker)
		if index < 0 {
			return false
		}
		searchFrom += index + len(marker)
	}
	return true
}

func safePollprobeChannelAssertions(output string) string {
	allowedFields := map[string]bool{
		"error_code": true, "rpc_error": true, "rpc_code": true, "count": true,
		"failed": true, "error_codes": true, "logout_causes": true, "stopped_before_logout": true,
		"actual_total": true, "answer_count": true, "first_voters": true, "second_voters": true,
		"members": true, "outsiders": true, "voters": true, "voter_ids": true,
		"votes": true, "pages": true, "limit": true, "unique": true,
		"pts_delta": true, "messages": true, "updates": true, "final": true,
		"chosen": true, "peer_present": true, "message_id_present": true,
	}
	var safe []string
	var runtimeDetails []string
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "panic:") || strings.Contains(line, "fatal error:") {
			runtimeDetails = append(runtimeDetails, "runtime_failure=1")
		}
		if index := strings.Index(line, "/cmd/pollprobe/"); index >= 0 {
			frame := line[index+1:]
			if end := strings.IndexAny(frame, " \t"); end >= 0 {
				frame = frame[:end]
			}
			if dot := strings.Index(frame, ".go:"); dot >= 0 {
				if end := strings.IndexAny(frame[dot+4:], " \t"); end >= 0 {
					frame = frame[:dot+4+end]
				}
				runtimeDetails = append(runtimeDetails, "runtime_frame="+frame)
			}
		}
		assertionOffset := strings.Index(line, "assertion=")
		if assertionOffset < 0 {
			continue
		}
		parts := strings.Fields(line[assertionOffset:])
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "assertion=") || !strings.HasPrefix(parts[1], "result=") {
			continue
		}
		assertion := strings.TrimPrefix(parts[0], "assertion=")
		result := strings.TrimPrefix(parts[1], "result=")
		if !safePollprobeAssertionName(assertion) || result != "pass" && result != "fail" {
			continue
		}
		logoutFields := map[string]string(nil)
		if assertion == "logout_sessions" {
			logoutFields = safeLogoutSummaryFields(parts[2:])
		}
		fields := []string{"assertion=" + assertion, "result=" + result}
		seen := make(map[string]bool, len(parts)-2)
		for _, part := range parts[2:] {
			key, value, ok := strings.Cut(part, "=")
			if !ok || !allowedFields[key] || value == "" || !safePollprobeField(value) || seen[key] {
				continue
			}
			seen[key] = true
			if key == "count" && assertion == "logout_sessions" {
				var exists bool
				value, exists = logoutFields[key]
				if !exists {
					continue
				}
			}
			if isLogoutOnlyField(key) {
				if assertion != "logout_sessions" {
					continue
				}
				var exists bool
				value, exists = logoutFields[key]
				if !exists {
					continue
				}
			}
			fields = append(fields, key+"="+value)
		}
		safe = append(safe, strings.Join(fields, " "))
	}
	safe = append(safe, runtimeDetails...)
	return strings.Join(safe, "\n")
}

func isLogoutOnlyField(key string) bool {
	switch key {
	case "failed", "error_codes", "logout_causes", "stopped_before_logout":
		return true
	default:
		return false
	}
}

func safeLogoutSummaryFields(parts []string) map[string]string {
	// The probe has four fixed accounts and one temporary recovery session.
	const maxSessions = 5
	raw := make(map[string]string, 5)
	duplicate := make(map[string]bool, 5)
	for _, part := range parts {
		key, value, ok := strings.Cut(part, "=")
		if !ok || key != "count" && key != "failed" && key != "error_codes" && key != "logout_causes" && key != "stopped_before_logout" {
			continue
		}
		if _, exists := raw[key]; exists {
			duplicate[key] = true
			continue
		}
		raw[key] = value
	}
	count, countOK := parseLogoutCount(raw["count"], maxSessions)
	if duplicate["count"] || !countOK {
		return nil
	}
	fields := map[string]string{"count": strconv.Itoa(count)}
	failed, failedOK := parseLogoutCount(raw["failed"], count)
	if !duplicate["failed"] && failedOK {
		fields["failed"] = strconv.Itoa(failed)
	} else {
		failedOK = false
	}
	stopped, stoppedOK := parseLogoutCount(raw["stopped_before_logout"], count)
	if !duplicate["stopped_before_logout"] && stoppedOK {
		fields["stopped_before_logout"] = strconv.Itoa(stopped)
	}
	if !failedOK || failed == 0 {
		return fields
	}
	if value := raw["error_codes"]; !duplicate["error_codes"] && value != "" && safePollprobeField(value) {
		fields["error_codes"] = value
	}
	if value := raw["logout_causes"]; !duplicate["logout_causes"] && value != "" && safePollprobeField(value) {
		if causes, causeCount, ok := safeLogoutCauseSet(value); ok && causeCount <= failed {
			fields["logout_causes"] = causes
		}
	}
	return fields
}

func parseLogoutCount(value string, limit int) (int, bool) {
	if value == "" {
		return 0, false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return 0, false
		}
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 || count > limit {
		return 0, false
	}
	return count, true
}

func safeLogoutCauseSet(value string) (string, int, bool) {
	allowed := map[string]bool{
		"DEADLINE_EXCEEDED": true, "CANCELED": true, "TRANSPORT_CLOSED": true,
		"CLIENT_STOPPED": true, "RECONNECT_FAILED": true, "RPC_ERROR": true,
		"RESPONSE_INVALID": true, "UNKNOWN": true,
	}
	causes := make(map[string]bool)
	for cause := range strings.SplitSeq(value, ",") {
		if allowed[cause] {
			causes[cause] = true
		}
	}
	if len(causes) == 0 {
		return "", 0, false
	}
	sorted := make([]string, 0, len(causes))
	for cause := range causes {
		sorted = append(sorted, cause)
	}
	sort.Strings(sorted)
	return strings.Join(sorted, ","), len(sorted), true
}

func TestSafePollprobeChannelAssertionsPreservesOnlySafeLogoutAggregates(t *testing.T) {
	input := strings.Join([]string{
		"assertion=logout_sessions result=pass count=4 failed=2 error_codes=OPERATION_FAILED logout_causes=UNKNOWN,BOGUS,DEADLINE_EXCEEDED,UNKNOWN stopped_before_logout=1 credential=not-safe",
		"assertion=channels_scenario_complete result=pass count=2 failed=1 error_codes=UNKNOWN logout_causes=UNKNOWN stopped_before_logout=1",
		"assertion=logout_sessions result=pass count=-1 failed=2 logout_causes=UNKNOWN stopped_before_logout=1",
		"assertion=logout_sessions result=pass count=4 failed=5 logout_causes=DEADLINE_EXCEEDED stopped_before_logout=1",
		"assertion=logout_sessions result=pass count=6 failed=1 logout_causes=UNKNOWN stopped_before_logout=1",
		"assertion=logout_sessions result=pass count=5 failed=1 error_codes=OPERATION_FAILED logout_causes=UNKNOWN stopped_before_logout=5",
		"assertion=logout_sessions result=pass count=1 failed=1 error_codes=OPERATION_FAILED logout_causes=UNKNOWN;fake_token stopped_before_logout=0",
	}, "\n")
	want := strings.Join([]string{
		"assertion=logout_sessions result=pass count=4 failed=2 error_codes=OPERATION_FAILED logout_causes=DEADLINE_EXCEEDED,UNKNOWN stopped_before_logout=1",
		"assertion=channels_scenario_complete result=pass count=2",
		"assertion=logout_sessions result=pass",
		"assertion=logout_sessions result=pass count=4 stopped_before_logout=1",
		"assertion=logout_sessions result=pass",
		"assertion=logout_sessions result=pass count=5 failed=1 error_codes=OPERATION_FAILED logout_causes=UNKNOWN stopped_before_logout=5",
		"assertion=logout_sessions result=pass count=1 failed=1 error_codes=OPERATION_FAILED stopped_before_logout=0",
	}, "\n")
	if got := safePollprobeChannelAssertions(input); got != want {
		t.Fatalf("sanitized logout diagnostics = %q, want %q", got, want)
	}
}

func safePollprobeAssertionName(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return false
		}
	}
	return true
}
