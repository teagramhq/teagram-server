package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	tsrp "github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func TestRunCommandAdminCreateUserPersistsOrdinaryAccountWithRegistrationClosed(t *testing.T) {
	ctx := context.Background()
	dsn, key, st := openPasswordResetTestStore(t)
	t.Setenv("TG_REGISTRATION", "closed")
	t.Setenv("TG_PASSWORD", "environment-secret-marker")

	const password = "stdin-secret-marker" //nolint:gosec // G101: synthetic test password.
	var stderr bytes.Buffer
	err := runAdminCommandWithInput(t, 0, []string{"create-user", "--username", "SynthPoll_A"}, password+"\n", &stderr)
	if err != nil {
		t.Fatalf("create-user with closed registration: %v", err)
	}
	if !strings.HasPrefix(stderr.String(), "User created: synthpoll_a (user id: ") || !strings.HasSuffix(stderr.String(), ")\n") {
		t.Fatalf("stderr = %q, want a username and user id confirmation", stderr.String())
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture database: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close fixture database connection: %v", err)
		}
	}()
	var userID int64
	if err := conn.QueryRow(ctx, `SELECT owner_id FROM usernames WHERE handle = 'synthpoll_a' AND owner_type = 'user'`).Scan(&userID); err != nil {
		t.Fatalf("load claimed username: %v", err)
	}
	user, ok, err := st.UserByID(ctx, userID)
	if err != nil || !ok {
		t.Fatalf("load created user: ok=%v err=%v", ok, err)
	}
	if user.Username == nil || *user.Username != "synthpoll_a" || user.Phone != "" {
		t.Fatalf("created user = %+v, want username-mode account synthpoll_a", user)
	}
	mode, err := st.UserLoginMode(ctx, userID)
	if err != nil || mode != "username" {
		t.Fatalf("login mode = %q, err=%v; want username", mode, err)
	}
	storedPassword, hasPassword, err := st.PasswordByUser(ctx, userID)
	if err != nil || !hasPassword {
		t.Fatalf("load created password: hasPassword=%v err=%v", hasPassword, err)
	}
	if len(storedPassword.Verifier) != tsrp.PadLen || !tsrp.ValidVerifier(storedPassword.Verifier) {
		t.Fatalf("created verifier is invalid: %d bytes", len(storedPassword.Verifier))
	}
	var encryptedVerifier []byte
	if err := conn.QueryRow(ctx, `SELECT verifier FROM user_passwords WHERE user_id = $1`, userID).Scan(&encryptedVerifier); err != nil {
		t.Fatalf("load encrypted verifier: %v", err)
	}
	if bytes.Equal(encryptedVerifier, storedPassword.Verifier) {
		t.Fatal("verifier was stored without encryption")
	}
	for _, secret := range []string{password, "environment-secret-marker", hex.EncodeToString(storedPassword.Verifier), hex.EncodeToString(key)} {
		if strings.Contains(stderr.String(), secret) {
			t.Fatalf("command output exposed a secret: %q", secret)
		}
	}
	isAdmin, err := st.IsServerAdministrator(ctx, userID)
	if err != nil || isAdmin {
		t.Fatalf("created user administrator = %v, err=%v; want false", isAdmin, err)
	}
	var electionClosed bool
	var administratorID *int64
	if err := conn.QueryRow(ctx, `SELECT election_closed, administrator_user_id FROM server_administration WHERE singleton_id = 1`).Scan(&electionClosed, &administratorID); err != nil {
		t.Fatalf("read administrator election: %v", err)
	}
	if !electionClosed || administratorID != nil {
		t.Fatalf("administrator election = closed:%v administrator:%v; want closed without an administrator", electionClosed, administratorID)
	}
}

func TestRunCommandAdminCreateUserRequiresRootBeforeMutation(t *testing.T) {
	dsn, _, _ := openPasswordResetTestStore(t)
	before := createUserAccountSnapshot(t, dsn)
	var stderr bytes.Buffer
	err := runAdminCommandWithInput(t, 1000, []string{"create-user", "--username", "synthpoll_a"}, "stdin-secret-marker\n", &stderr)
	if err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatalf("non-root create-user error = %v, want root-only refusal", err)
	}
	if got := err.Error() + stderr.String(); strings.Contains(got, "stdin-secret-marker") {
		t.Fatal("non-root refusal exposed password input")
	}
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("non-root create-user changed account state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunCommandAdminCreateUserRejectsInvalidAndReservedHandlesBeforeOpeningStore(t *testing.T) {
	dsn, _, _ := openPasswordResetTestStore(t)
	before := createUserAccountSnapshot(t, dsn)
	t.Setenv("TG_POSTGRES_DSN", "not a postgres connection string")
	for _, handle := range []string{"", "a", "1invalid", "bad-name", "admin", "HELP"} {
		var stderr bytes.Buffer
		err := runAdminCommandWithInput(t, 0, []string{"create-user", "--username", handle}, "stdin-secret-marker\n", &stderr)
		if err == nil || !strings.Contains(err.Error(), "username") {
			t.Errorf("handle %q error = %v, want username rejection before database access", handle, err)
		}
		if strings.Contains(errText(err, stderr.String()), "stdin-secret-marker") {
			t.Errorf("handle %q error exposed password input", handle)
		}
	}
	t.Setenv("TG_POSTGRES_DSN", dsn)
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("invalid or reserved handles changed account state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunCommandAdminCreateUserRejectsBadPasswordInputAndFlagsWithoutWrites(t *testing.T) {
	dsn, _, _ := openPasswordResetTestStore(t)
	t.Setenv("TG_PASSWORD", "environment-secret-marker")
	before := createUserAccountSnapshot(t, dsn)
	for _, input := range []string{"", "\n", "\r\n", "first\nsecond\n"} {
		var stderr bytes.Buffer
		err := runAdminCommandWithInput(t, 0, []string{"create-user", "--username", "synthpoll_a"}, input, &stderr)
		if err == nil || !strings.Contains(err.Error(), "password input") {
			t.Errorf("input %q error = %v, want password input rejection", input, err)
		}
		if strings.Contains(errText(err, stderr.String()), "environment-secret-marker") {
			t.Errorf("environment password appeared in diagnostics for input %q", input)
		}
		if after := createUserAccountSnapshot(t, dsn); after != before {
			t.Errorf("invalid password input %q changed account state:\nbefore %s\nafter  %s", input, before, after)
		}
	}
	var stderr bytes.Buffer
	err := runAdminCommandWithInput(t, 0,
		[]string{"create-user", "--username", "synthpoll_a", "--password", "argv-secret-marker"},
		"stdin-secret-marker\n", &stderr)
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("password flag error = %v, want usage refusal", err)
	}
	if strings.Contains(err.Error()+stderr.String(), "argv-secret-marker") || strings.Contains(err.Error()+stderr.String(), "stdin-secret-marker") {
		t.Fatal("password flag refusal exposed a password")
	}
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("password flag changed account state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunCommandAdminCreateUserRejectsInteractivePasswordInput(t *testing.T) {
	dsn, _, _ := openPasswordResetTestStore(t)
	before := createUserAccountSnapshot(t, dsn)
	interactive, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open null device: %v", err)
	}
	defer func() {
		if err := interactive.Close(); err != nil {
			t.Errorf("close null device: %v", err)
		}
	}()
	var stderr bytes.Buffer
	err = runAdminCommandWithEUID([]string{"create-user", "--username", "synthpoll_a"}, interactive, &stderr, 0)
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("interactive create-user error = %v, want interactive refusal", err)
	}
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("interactive input changed account state:\nbefore %s\nafter  %s", before, after)
	}
}

func TestRunCommandAdminCreateUserDoesNotOverwriteExistingAccount(t *testing.T) {
	ctx := context.Background()
	dsn, _, st := openPasswordResetTestStore(t)
	existing := seedPasswordResetUsername(t, ctx, st, "synthpoll_a", "existing-password", "kept hint", nil, false)
	before := createUserAccountSnapshot(t, dsn)
	var stderr bytes.Buffer
	err := runAdminCommandWithInput(t, 0, []string{"create-user", "--username", "synthpoll_a"}, "replacement-secret-marker\n", &stderr)
	if !errors.Is(err, store.ErrUsernameOccupied) {
		t.Fatalf("duplicate create-user error = %v, want username occupied", err)
	}
	if strings.Contains(err.Error()+stderr.String(), "replacement-secret-marker") {
		t.Fatal("duplicate create-user exposed password input")
	}
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("duplicate create-user changed account state:\nbefore %s\nafter  %s", before, after)
	}
	got, ok, err := st.PasswordByUser(ctx, existing.ID)
	if err != nil || !ok || got.Hint != "kept hint" {
		t.Fatalf("existing account password after duplicate: password=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestRunCommandAdminCreateUserConcurrentClaimsHaveOneWinner(t *testing.T) {
	dsn, _, _ := openPasswordResetTestStore(t)
	const contenders = 2
	start := make(chan struct{})
	errs := make([]error, contenders)
	outputs := make([]bytes.Buffer, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = runAdminCommandWithInput(t, 0,
				[]string{"create-user", "--username", "synthpoll_a"},
				"stdin-secret-marker\n", &outputs[i])
		}(i)
	}
	close(start)
	wg.Wait()
	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		if !errors.Is(err, store.ErrUsernameOccupied) {
			t.Errorf("contender %d error = %v, want username occupied or success", i, err)
		}
		if strings.Contains(err.Error()+outputs[i].String(), "stdin-secret-marker") {
			t.Errorf("contender %d exposed password input", i)
		}
	}
	if winners != 1 {
		t.Fatalf("successful claims = %d, want exactly one", winners)
	}
	var users, handles, updateStates, passwords int
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect fixture database: %v", err)
	}
	defer func() { _ = conn.Close(context.Background()) }() //nolint:errcheck // cleanup only
	if err := conn.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM usernames), (SELECT count(*) FROM update_state), (SELECT count(*) FROM user_passwords)`).Scan(&users, &handles, &updateStates, &passwords); err != nil {
		t.Fatalf("count concurrent account rows: %v", err)
	}
	if users != 1 || handles != 1 || updateStates != 1 || passwords != 1 {
		t.Fatalf("concurrent account rows users=%d usernames=%d update_state=%d passwords=%d, want one each", users, handles, updateStates, passwords)
	}
}

func TestRunCommandAdminCreateUserRollsBackAllRowsOnPasswordWriteFailure(t *testing.T) {
	ctx := context.Background()
	dsn, _, _ := openPasswordResetTestStore(t)
	before := createUserAccountSnapshot(t, dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DROP TRIGGER IF EXISTS reject_created_password_test ON user_passwords`); err != nil {
			t.Errorf("drop password trigger: %v", err)
		}
		if _, err := conn.Exec(context.Background(), `DROP FUNCTION IF EXISTS reject_created_password_test()`); err != nil {
			t.Errorf("drop trigger function: %v", err)
		}
		if err := conn.Close(context.Background()); err != nil {
			t.Errorf("close fixture database connection: %v", err)
		}
	})
	if _, err := conn.Exec(ctx, `CREATE FUNCTION reject_created_password_test() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected create-user password write failure'; END $$`); err != nil {
		t.Fatalf("create trigger function: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TRIGGER reject_created_password_test BEFORE INSERT ON user_passwords FOR EACH ROW EXECUTE FUNCTION reject_created_password_test()`); err != nil {
		t.Fatalf("create password trigger: %v", err)
	}
	var stderr bytes.Buffer
	err = runAdminCommandWithInput(t, 0, []string{"create-user", "--username", "synthpoll_a"}, "stdin-secret-marker\n", &stderr)
	if err == nil || !strings.Contains(err.Error(), "write password verifier") {
		t.Fatalf("password database failure = %v, want password write error", err)
	}
	if strings.Contains(err.Error()+stderr.String(), "stdin-secret-marker") {
		t.Fatal("password write failure exposed password input")
	}
	if after := createUserAccountSnapshot(t, dsn); after != before {
		t.Fatalf("password failure left partial account state:\nbefore %s\nafter  %s", before, after)
	}
}

func runAdminCommandWithInput(t *testing.T, euid int, args []string, input string, stderr io.Writer) error {
	t.Helper()
	return runAdminCommandWithEUID(args, strings.NewReader(input), stderr, euid)
}

func createUserAccountSnapshot(t *testing.T, dsn string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect fixture database for snapshot: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close snapshot connection: %v", err)
		}
	}()
	var users, usernames, updates, passwords, administration string
	err = conn.QueryRow(ctx, `
		SELECT
		  (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.id)::text, '[]') FROM (SELECT id, phone, first_name, last_name, username, login_mode FROM users) x),
		  (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.handle, x.owner_type, x.owner_id)::text, '[]') FROM (SELECT handle, owner_type, owner_id FROM usernames) x),
		  (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.user_id)::text, '[]') FROM (SELECT user_id, pts FROM update_state) x),
		  (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.user_id)::text, '[]') FROM (SELECT user_id, salt1, salt2, verifier FROM user_passwords) x),
		  (SELECT COALESCE(jsonb_agg(to_jsonb(x) ORDER BY x.singleton_id)::text, '[]') FROM (SELECT singleton_id, election_closed, administrator_user_id FROM server_administration) x)
	`).Scan(&users, &usernames, &updates, &passwords, &administration)
	if err != nil {
		t.Fatalf("read account state snapshot: %v", err)
	}
	return strings.Join([]string{users, usernames, updates, passwords, administration}, "|")
}
