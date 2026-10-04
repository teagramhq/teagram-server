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

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func testSmokeProvisionedAccountLogin(t *testing.T) {
	t.Helper()

	binary := buildSmokeTelegramd(t)
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
			return fmt.Errorf("checkPassword response = %T, want authorization for user %d", response, created.ID)
		}
		account, ok := authorization.User.(*tg.User)
		if !ok || account.ID != created.ID || account.Username != username {
			return fmt.Errorf("checkPassword user = %#v, want user %d with username %q", authorization.User, created.ID, username)
		}

		users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("authenticated users.getUsers: %w", err)
		}
		if len(users) != 1 {
			return fmt.Errorf("authenticated users.getUsers returned %d users, want one", len(users))
		}
		self, ok := users[0].(*tg.User)
		if !ok || self.ID != created.ID || self.Username != username {
			return fmt.Errorf("authenticated users.getUsers self = %#v, want user %d with username %q", users[0], created.ID, username)
		}
		return nil
	}); err != nil {
		t.Fatalf("fresh provisioned-account SRP login: %v", err)
	}

	isAdministrator, err := f.store.IsServerAdministrator(f.ctx, created.ID)
	if err != nil || isAdministrator {
		t.Fatalf("provisioned account administrator = %v, err=%v; want false", isAdministrator, err)
	}
}

func buildSmokeTelegramd(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var cancel context.CancelFunc
	if deadline, ok := t.Deadline(); ok {
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}

	binary := filepath.Join(t.TempDir(), "telegramd")
	command := osexec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/telegramd") // #nosec G204 -- fixed local Go build target.
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build telegramd for provisioning smoke: %v: %s", err, strings.TrimSpace(string(output)))
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
