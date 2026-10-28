package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store"
)

func TestAdminBotCommandCreatesRotatesAndRevokesToken(t *testing.T) {
	ctx := context.Background()
	dsn, _, st := openPasswordResetTestStore(t)

	var stdout, stderr bytes.Buffer
	if err := runCommandWithStdin(t, []string{"admin", "bot", "create", "--display-name", "Echo", "--username", "x_Bot"}, "", &stdout, &stderr); err != nil {
		t.Fatalf("create bot: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("create stderr = %q, want empty", stderr.String())
	}
	botID, secret, firstToken := parseBotTokenForTest(t, stdout.String())

	user, err := st.LookupBotToken(ctx, firstToken)
	if err != nil || user.ID != botID || user.Username == nil || *user.Username != "x_bot" {
		t.Fatalf("lookup created bot: user=%+v err=%v", user, err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close fixture database connection: %v", err)
		}
	})
	var kind, name string
	var phone *string
	if err := conn.QueryRow(ctx, `SELECT kind, first_name, phone FROM users WHERE id = $1`, botID).Scan(&kind, &name, &phone); err != nil {
		t.Fatalf("read created bot: %v", err)
	}
	if kind != "bot" || name != "Echo" || phone != nil {
		t.Fatalf("created user kind/name/phone = %q/%q/%v", kind, name, phone)
	}
	var firstHash []byte
	var firstRowJSON string
	if err := conn.QueryRow(ctx, `SELECT secret_hash, to_jsonb(bot_tokens)::text FROM bot_tokens WHERE user_id = $1`, botID).Scan(&firstHash, &firstRowJSON); err != nil {
		t.Fatalf("read stored bot token: %v", err)
	}
	wantHash := sha256.Sum256(secret)
	if !bytes.Equal(firstHash, wantHash[:]) {
		t.Fatal("database did not store SHA-256 of the token secret")
	}
	secretText := base64.RawURLEncoding.EncodeToString(secret)
	if strings.Contains(firstRowJSON, secretText) || strings.Contains(firstRowJSON, hex.EncodeToString(secret)) || strings.Contains(firstRowJSON, firstToken) {
		t.Fatal("database row contains the plaintext bot token secret")
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCommandWithStdin(t, []string{"admin", "bot", "rotate", "--username", "x_Bot"}, "", &stdout, &stderr); err != nil {
		t.Fatalf("rotate bot token: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("rotate stderr = %q, want empty", stderr.String())
	}
	rotatedID, _, rotatedToken := parseBotTokenForTest(t, stdout.String())
	if rotatedID != botID || rotatedToken == firstToken {
		t.Fatalf("rotated token = %q, want a new token for bot %d", rotatedToken, botID)
	}
	oldTokenErr := botTokenLookupError(st, ctx, firstToken)
	if oldTokenErr == nil {
		t.Fatal("old token remained valid after rotation")
	}
	if _, err := st.LookupBotToken(ctx, rotatedToken); err != nil {
		t.Fatalf("new token lookup: %v", err)
	}

	malformedErr := botTokenLookupError(st, ctx, "not-a-token")
	unknownErr := botTokenLookupError(st, ctx, "9223372036854775807:"+base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if malformedErr == nil || unknownErr == nil || malformedErr.Error() != unknownErr.Error() {
		t.Fatalf("malformed lookup error %v differs from unknown lookup error %v", malformedErr, unknownErr)
	}
	if oldTokenErr.Error() != malformedErr.Error() {
		t.Fatalf("rotated lookup error %v differs from malformed lookup error %v", oldTokenErr, malformedErr)
	}

	stdout.Reset()
	stderr.Reset()
	if err := runCommandWithStdin(t, []string{"admin", "bot", "revoke", "--username", "x_Bot"}, "", &stdout, &stderr); err != nil {
		t.Fatalf("revoke bot token: %v", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "revoked") {
		t.Fatalf("revoke output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	revokedErr := botTokenLookupError(st, ctx, rotatedToken)
	if revokedErr == nil || revokedErr.Error() != malformedErr.Error() {
		t.Fatalf("revoked lookup error %v differs from malformed lookup error %v", revokedErr, malformedErr)
	}
}

func TestAdminBotCommandRejectsTokenArguments(t *testing.T) {
	t.Setenv("TG_POSTGRES_DSN", "invalid postgres dsn")
	var stdout, stderr bytes.Buffer
	err := runCommandWithStdin(t,
		[]string{"admin", "bot", "rotate", "--username", "echo_test_bot", "--token", "secret-marker"},
		"", &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("token argument error = %v, want usage error", err)
	}
	if strings.Contains(err.Error()+stdout.String()+stderr.String(), "secret-marker") {
		t.Fatal("token argument appeared in diagnostics")
	}
}

func parseBotTokenForTest(t *testing.T, output string) (int64, []byte, string) {
	t.Helper()
	if !strings.HasSuffix(output, "\n") || strings.Count(output, "\n") != 1 {
		t.Fatalf("bot token output = %q, want exactly one line", output)
	}
	token := strings.TrimSuffix(output, "\n")
	idText, secretText, ok := strings.Cut(token, ":")
	if !ok {
		t.Fatalf("bot token %q has no id separator", token)
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("bot token id %q is invalid", idText)
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(secretText)
	if err != nil || len(secretText) != 43 || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != secretText {
		t.Fatalf("bot token secret encoding is invalid: %v, %d bytes", err, len(secret))
	}
	return id, secret, token
}

func botTokenLookupError(st *store.Store, ctx context.Context, token string) error {
	_, err := st.LookupBotToken(ctx, token)
	return err
}
