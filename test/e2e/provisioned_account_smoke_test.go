package e2e_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeProvisionedAccountLogin(t *testing.T) {
	t.Helper()

	binary := buildSmokeTeagramd(t)
	f := newSmokeFixtureWithRegistration(t, config.RegistrationClosed)
	const username, password = "smokeprovisioned", "smoke-provisioned-password-1245"

	passwordInput, err := os.CreateTemp(t.TempDir(), "create-user-password-")
	if err != nil {
		t.Fatalf("create protected password input: %v", err)
	}
	t.Cleanup(func() {
		if err := passwordInput.Close(); err != nil {
			t.Errorf("close protected password input: %v", err)
		}
	})
	if err := passwordInput.Chmod(0o600); err != nil {
		t.Fatalf("protect password input: %v", err)
	}
	if _, err := fmt.Fprintln(passwordInput, password); err != nil {
		t.Fatalf("write protected password input: %v", err)
	}
	if _, err := passwordInput.Seek(0, 0); err != nil {
		t.Fatalf("rewind protected password input: %v", err)
	}

	command, err := smokeRootCommand(f.ctx, binary, "admin", "create-user", "--username", username)
	if err != nil {
		t.Fatal(err)
	}
	command.Env = smokeProvisioningEnvironment(f.dsn)
	command.Stdin = passwordInput
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("root-only create-user under closed registration: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("create-user stdout = %q, want empty", stdout.String())
	}

	created, found, err := f.store.UserByUsernameWithLoginMode(f.ctx, username)
	if err != nil || !found {
		t.Fatalf("lookup provisioned account: found=%v err=%v", found, err)
	}
	if created.LoginMode != "username" {
		t.Fatalf("provisioned account login mode = %q, want username", created.LoginMode)
	}
	wantConfirmation := fmt.Sprintf("User created: %s (user id: %d)\n", username, created.ID)
	if stderr.String() != wantConfirmation {
		t.Fatalf("create-user stderr = %q, want %q", stderr.String(), wantConfirmation)
	}

	smokeProvisionedAccountLogin(t, f, created.ID, username, password)

	isAdministrator, err := f.store.IsServerAdministrator(f.ctx, created.ID)
	if err != nil || isAdministrator {
		t.Fatalf("provisioned account administrator = %v, err=%v; want false", isAdministrator, err)
	}

	restoredDSN := dumpAndRestoreSmokeDatabase(t, f.ctx, f.dsn)
	restored := newRestoredSmokeFixture(t, f, restoredDSN)
	restoredUser, found, err := restored.store.UserByUsernameWithLoginMode(restored.ctx, username)
	if err != nil || !found || restoredUser.ID != created.ID || restoredUser.LoginMode != "username" {
		t.Fatalf("restored provisioned account = %+v, found=%v, err=%v", restoredUser, found, err)
	}
	smokeProvisionedAccountLogin(t, restored, created.ID, username, password)
	isAdministrator, err = restored.store.IsServerAdministrator(restored.ctx, created.ID)
	if err != nil || isAdministrator {
		t.Fatalf("restored provisioned account administrator = %v, err=%v; want false", isAdministrator, err)
	}
}

func smokeProvisionedAccountLogin(t *testing.T, f *smokeFixture, userID int64, username, password string) {
	t.Helper()
	client := newUsernameClient(f.port, f.key, f.dcID, &session.StorageMemory{})
	if err := client.Run(f.ctx, func(ctx context.Context) error {
		api := client.API()
		codeHash, err := sendCodeUsername(ctx, api, username)
		if err != nil {
			return fmt.Errorf("sendCode for provisioned account: %w", err)
		}
		code, err := f.codes.wait(ctx, username)
		if err != nil {
			return fmt.Errorf("wait for provisioned account login code: %w", err)
		}
		response, err := signInUsername(ctx, api, username, codeHash, code)
		if !isSessionPasswordNeeded(err) {
			if err != nil {
				return fmt.Errorf("signIn for provisioned account: %w", err)
			}
			return fmt.Errorf("signIn response = %T, want password challenge", response)
		}

		passwordState, err := api.AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("get provisioned account SRP challenge: %w", err)
		}
		proof, err := auth.PasswordHash([]byte(password), passwordState.SRPID, passwordState.SRPB, passwordState.SecureRandom, passwordState.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("compute provisioned account SRP proof: %w", err)
		}
		response, err = api.AuthCheckPassword(ctx, proof)
		if err != nil {
			return fmt.Errorf("complete fresh SRP login: %w", err)
		}
		authorization, ok := response.(*tg.AuthAuthorization)
		if !ok || authorization.User == nil {
			return fmt.Errorf("checkPassword response = %T, want authorization for user %d", response, userID)
		}
		account, ok := authorization.User.(*tg.User)
		if !ok || account.ID != userID || account.Username != username {
			return fmt.Errorf("checkPassword user = %#v, want user %d with username %q", authorization.User, userID, username)
		}

		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("authenticated users.getUsers: %w", err)
		}
		if len(users) != 1 {
			return fmt.Errorf("authenticated users.getUsers returned %d users, want one", len(users))
		}
		self, ok := users[0].(*tg.User)
		if !ok || self.ID != userID || self.Username != username {
			return fmt.Errorf("authenticated users.getUsers self = %#v, want user %d with username %q", users[0], userID, username)
		}
		return nil
	}); err != nil {
		t.Fatalf("fresh provisioned-account SRP login: %v", err)
	}
}

func dumpAndRestoreSmokeDatabase(t *testing.T, ctx context.Context, sourceDSN string) string {
	t.Helper()
	sourceConfig, err := pgx.ParseConfig(sourceDSN)
	if err != nil {
		t.Fatalf("parse source Postgres test DSN: %v", err)
	}
	targetName := "t_restore_" + pgtest.RandomHex()
	admin, err := pgx.Connect(ctx, pgtest.DSNFrom("postgres"))
	if err != nil {
		t.Fatalf("connect to reusable Postgres for restore fixture: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{targetName}.Sanitize()); err != nil {
		if closeErr := admin.Close(ctx); closeErr != nil {
			t.Fatalf("create restored Postgres test database: %v; close reusable Postgres connection: %v", err, closeErr)
		}
		t.Fatalf("create restored Postgres test database: %v", err)
	}
	pgtest.RegisterDatabaseCleanup(t, targetName)
	if err := admin.Close(ctx); err != nil {
		t.Fatalf("close reusable Postgres connection: %v", err)
	}

	dumpPath := filepath.Join(t.TempDir(), "cutover-source.dump")
	dump, err := os.OpenFile(dumpPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("create protected Postgres dump: %v", err)
	}
	dumpCommand := osexec.CommandContext(ctx, "docker", "exec", "tg-test-pg", "pg_dump", // #nosec G204 -- fixed test container and a database name parsed from pgtest's DSN.
		"--format=custom", "--no-owner", "--no-acl", "--username=postgres", "--dbname="+sourceConfig.Database)
	var dumpStderr bytes.Buffer
	dumpCommand.Stdout = dump
	dumpCommand.Stderr = &dumpStderr
	dumpErr := dumpCommand.Run()
	closeErr := dump.Close()
	if dumpErr != nil || closeErr != nil {
		t.Fatalf("create custom-format Postgres dump: run=%v close=%v stderr=%q", dumpErr, closeErr, strings.TrimSpace(dumpStderr.String()))
	}
	if err := os.Chmod(dumpPath, 0o600); err != nil {
		t.Fatalf("protect custom-format Postgres dump: %v", err)
	}

	dump, err = os.Open(dumpPath)
	if err != nil {
		t.Fatalf("open protected Postgres dump: %v", err)
	}
	restoreCommand := osexec.CommandContext(ctx, "docker", "exec", "--interactive", "tg-test-pg", "pg_restore", // #nosec G204 -- fixed test container and a generated test database name.
		"--clean", "--if-exists", "--no-owner", "--no-acl", "--exit-on-error", "--username=postgres", "--dbname="+targetName)
	restoreCommand.Stdin = dump
	var restoreStderr bytes.Buffer
	restoreCommand.Stderr = &restoreStderr
	restoreErr := restoreCommand.Run()
	closeErr = dump.Close()
	if restoreErr != nil || closeErr != nil {
		t.Fatalf("restore custom-format Postgres dump: run=%v close=%v stderr=%q", restoreErr, closeErr, strings.TrimSpace(restoreStderr.String()))
	}
	return pgtest.DSNFrom(targetName)
}

func newRestoredSmokeFixture(t *testing.T, source *smokeFixture, dsn string) *smokeFixture {
	t.Helper()
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancelDeadline)
	deadlineCtx = withRegistrySnapshotState(deadlineCtx)
	ctx, cancelFailure := context.WithCancelCause(deadlineCtx)
	t.Cleanup(func() { cancelFailure(nil) })
	blobs := testBlobs(t)
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open restored Postgres store: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("close restored Postgres store: %v", err)
		}
	})
	f := &smokeFixture{
		ctx: ctx, failures: newClientFailureSignal(cancelFailure), key: source.key, dsn: dsn,
		store: st, blobs: blobs, codes: newMultiCodeSink(), dcID: source.dcID,
		regMode: config.RegistrationClosed, stickerImportMode: config.StickerImportOn,
		userInviteMinAccountAge: config.DefaultUserInviteMinAccountAge,
		recoveryEmailMode:       config.RecoveryEmailOff, signUpTermsOfService: config.DefaultSignUpTermsOfService,
	}
	f.start(t, "127.0.0.1:0")
	return f
}

func buildSmokeTeagramd(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var cancel context.CancelFunc
	if deadline, ok := t.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	binary := filepath.Join(t.TempDir(), "teagramd")
	command := osexec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/teagramd") // #nosec G204 -- fixed local Go build target.
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build teagramd for provisioning smoke: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return binary
}

func smokeRootCommand(ctx context.Context, binary string, args ...string) (*osexec.Cmd, error) {
	commandArgs := append([]string{binary}, args...)
	if os.Geteuid() == 0 {
		return osexec.CommandContext(ctx, commandArgs[0], commandArgs[1:]...), nil // #nosec G204 -- fixed local executable and synthetic test arguments.
	}
	sudo, err := osexec.LookPath("sudo")
	if err != nil {
		return nil, fmt.Errorf("root-only create-user smoke requires root or passwordless sudo: %w", err)
	}
	sudoArgs := make([]string, 0, 3+len(commandArgs))
	sudoArgs = append(sudoArgs,
		"-n",
		"--preserve-env=TG_POSTGRES_DSN,TG_AUTHKEY_ENC_KEY,TG_AUTHKEY_ENC_KEY_FILE,TG_REGISTRATION",
		"--",
	)
	return osexec.CommandContext(ctx, sudo, append(sudoArgs, commandArgs...)...), nil // #nosec G204 -- sudo executes the fixed local CLI with synthetic test arguments.
}

func smokeProvisioningEnvironment(dsn string) []string {
	settings := map[string]string{
		"TG_POSTGRES_DSN":         dsn,
		"TG_AUTHKEY_ENC_KEY":      hex.EncodeToString(pgtest.EncKey()),
		"TG_AUTHKEY_ENC_KEY_FILE": "",
		"TG_REGISTRATION":         string(config.RegistrationClosed),
	}
	env := make([]string, 0, len(os.Environ())+len(settings))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, replace := settings[name]; !replace {
			env = append(env, entry)
		}
	}
	for name, value := range settings {
		env = append(env, name+"="+value)
	}
	return env
}
