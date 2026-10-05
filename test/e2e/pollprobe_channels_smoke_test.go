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
	"channel_difference_recovery",
	"channel_removal_capture_suppressed",
	"channel_removed_member_denied",
	"channel_outsider_denied",
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
	"channels_scenario_complete",
	"logout_sessions",
}

func safePollprobeChannelAssertions(output string) string {
	allowedFields := map[string]bool{
		"error_code": true, "rpc_error": true, "rpc_code": true, "count": true,
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
		fields := []string{"assertion=" + assertion, "result=" + result}
		for _, part := range parts[2:] {
			key, value, ok := strings.Cut(part, "=")
			if !ok || !allowedFields[key] || value == "" || !safePollprobeField(value) {
				continue
			}
			fields = append(fields, key+"="+value)
		}
		safe = append(safe, strings.Join(fields, " "))
	}
	safe = append(safe, runtimeDetails...)
	return strings.Join(safe, "\n")
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
