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

func testSmokePollProbe(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	passwords := [4]string{
		"pollprobe-smoke-password-a",
		"pollprobe-smoke-password-b",
		"pollprobe-smoke-password-c",
		"pollprobe-smoke-password-d",
	}
	for index, username := range []string{"synthpoll_a", "synthpoll_b", "synthpoll_c", "synthpoll_d"} {
		seedUsernameUser(t, f.ctx, f.store, username, "Synthetic", passwords[index])
	}

	credentialDir := filepath.Join(t.TempDir(), "credentials")
	if err := os.Mkdir(credentialDir, 0o700); err != nil {
		t.Fatal("create pollprobe credential fixture")
	}
	for index, username := range []string{"synthpoll_a", "synthpoll_b", "synthpoll_c", "synthpoll_d"} {
		if err := os.WriteFile(filepath.Join(credentialDir, username+".password"), []byte(passwords[index]+"\n"), 0o600); err != nil {
			t.Fatal("write pollprobe credential fixture")
		}
	}

	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		t.Fatal("encode pollprobe fixture server key")
	}
	keyID := sha256.Sum256(der)
	keyPath := filepath.Join(t.TempDir(), "server-public.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal("write pollprobe fixture server key")
	}
	endpoint := fmt.Sprintf("127.0.0.1:%d", f.port)
	ctx, cancel := context.WithTimeout(f.ctx, 75*time.Second)
	defer cancel()
	cmd := osExec.CommandContext(ctx, "go", "run", "../../cmd/pollprobe",
		"--endpoint", endpoint,
		"--rsa-public-key", keyPath,
		"--rsa-key-id", hex.EncodeToString(keyID[:]),
		"--credentials-dir", credentialDir,
	)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("locate home for pollprobe fixture process")
	}
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		t.Fatalf("pollprobe failed its isolated fixture scenario: %s", safePollprobeAssertions(string(output)))
	}
	outputText := string(output)
	for _, assertion := range []string{
		"account_identities",
		"synthetic_peers_resolved",
		"synthetic_group_created",
		"synthetic_group_members",
		"anonymous_non_voter_denied",
		"anonymous_vote_privacy",
		"outsider_vote_denied",
		"difference_reconnect",
		"difference_recovery",
		"public_non_voter_denied",
		"public_poll_results",
		"public_voter_pagination",
		"repeated_close_idempotent",
		"closed_poll_vote_denied",
		"closed_poll_reconnect",
		"saved_poll_lifecycle",
		"synthetic_text_round_trip",
		"logout_sessions",
	} {
		if !strings.Contains(outputText, "assertion="+assertion+" result=pass") {
			t.Errorf("pollprobe fixture omitted assertion %s", assertion)
		}
	}
	if got := strings.Count(outputText, "assertion=poll_created result=pass"); got != 3 {
		t.Errorf("pollprobe created assertion count = %d, want 3", got)
	}
	if got := strings.Count(outputText, "assertion=synthetic_group_created result=pass"); got != 1 {
		t.Errorf("pollprobe group creation count = %d, want 1", got)
	}
	for _, secret := range passwords {
		if strings.Contains(outputText, secret) {
			t.Error("pollprobe output exposed a fixture password")
		}
	}
	for _, username := range []string{"synthpoll_a", "synthpoll_b", "synthpoll_c", "synthpoll_d"} {
		if strings.Contains(outputText, username) {
			t.Error("pollprobe output exposed an account identity")
		}
	}
	entries, err := os.ReadDir(credentialDir)
	if err != nil {
		t.Fatal("inspect pollprobe credential fixture")
	}
	if len(entries) != 4 {
		t.Errorf("pollprobe credential directory entry count = %d, want 4", len(entries))
	}
}

func safePollprobeAssertions(output string) string {
	allowedAssertions := map[string]bool{
		"configuration_validated":    true,
		"session_transport_ready":    true,
		"account_authentication":     true,
		"account_identities":         true,
		"synthetic_peers_resolved":   true,
		"synthetic_group_created":    true,
		"synthetic_group_members":    true,
		"poll_created":               true,
		"anonymous_non_voter_denied": true,
		"anonymous_vote_privacy":     true,
		"outsider_vote_denied":       true,
		"difference_reconnect":       true,
		"difference_recovery":        true,
		"public_non_voter_denied":    true,
		"public_poll_results":        true,
		"public_voter_pagination":    true,
		"repeated_close_idempotent":  true,
		"closed_poll_vote_denied":    true,
		"closed_poll_reconnect":      true,
		"saved_poll_lifecycle":       true,
		"synthetic_text_round_trip":  true,
		"logout_sessions":            true,
		"scenario_complete":          true,
		"client_shutdown":            true,
	}
	allowedFields := map[string]bool{
		"error_code": true, "rpc_error": true, "rpc_code": true, "count": true,
		"actual_total": true, "answer_count": true, "first_voters": true, "second_voters": true,
		"failed": true, "members": true, "outsiders": true, "voters": true,
		"voter_ids": true, "votes": true, "pages": true, "limit": true,
		"unique": true, "pts_delta_a": true, "pts_delta_b": true,
		"edit_updates": true, "polls": true, "messages": true, "session": true,
	}
	var safe []string
	for _, line := range strings.Split(output, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 2 || !strings.HasPrefix(parts[0], "assertion=") || !strings.HasPrefix(parts[1], "result=") {
			continue
		}
		assertion := strings.TrimPrefix(parts[0], "assertion=")
		result := strings.TrimPrefix(parts[1], "result=")
		if !allowedAssertions[assertion] || result != "pass" && result != "fail" {
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
	return strings.Join(safe, "\n")
}

func safePollprobeField(value string) bool {
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == ',') {
			return false
		}
	}
	return true
}
