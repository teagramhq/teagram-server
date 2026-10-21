package e2e_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	gotdsrp "github.com/gotd/td/crypto/srp"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sys/unix"

	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	tsrp "github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
	restoredrill "github.com/teagramhq/teagram-server/test/e2e/restore_drill"
)

func TestRestoreDrillActualDaemon(t *testing.T) {
	fixture := startRestoreDrillDaemon(t)

	t.Run("actual daemon config and synthetic account sign-in", func(t *testing.T) {
		username := "drillhappyuser"
		password := "Synthetic-Password-Marker-90214"
		identity := fixture.createUser(t, username, password)
		invocation := fixture.invocation(t, username, password, fixture.publicKeyPEM, &identity)

		var stdout, stderr bytes.Buffer
		code := restoredrill.RunCLI(fixture.ctx, invocation, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("checker exit code = %d, want success; result: %s", code, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatal("checker wrote failure diagnostics to stderr")
		}
		var got restoredrill.Result
		if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
			t.Fatal("checker stdout was not a typed result")
		}
		if got.Category != restoredrill.CategoryAssertionsPassed ||
			got.HelpGetConfig != restoredrill.AssertionPass ||
			got.SyntheticSignin != restoredrill.AssertionPass {
			t.Fatalf("checker assertions = %q/%q/%q, want all pass", got.Category, got.HelpGetConfig, got.SyntheticSignin)
		}
		artifact := readRestoreDrillResult(t, invocation)
		for _, output := range [][]byte{stdout.Bytes(), stderr.Bytes(), artifact} {
			for _, marker := range []string{username, password, fixture.dsn, fixture.endpoint, "User created:", "user id:"} {
				if bytes.Contains(output, []byte(marker)) {
					t.Fatal("checker output or artifact exposed private provisioning material")
				}
			}
		}
	})

	t.Run("checker does not provision or replace an existing identity", func(t *testing.T) {
		username := "drillcollisionuser"
		password := "Original-Synthetic-Password-90127"
		identity := fixture.createUser(t, username, password)

		setRestoreDrillRecoverySettings(t, fixture.dsn, username)
		before := restoreDrillAccountSnapshot(t, fixture.dsn, username)
		replacementInvocation := fixture.invocation(t, username, "Replacement-Password-Marker-91208", fixture.publicKeyPEM, &identity)
		replacement := restoredrill.Run(fixture.ctx, replacementInvocation)
		if replacement.Category != restoredrill.CategoryAuthFailure ||
			replacement.HelpGetConfig != restoredrill.AssertionPass ||
			replacement.SyntheticSignin != restoredrill.AssertionFail {
			t.Fatalf("replacement credential result = %q/%q/%q, want auth_failure after config only", replacement.Category, replacement.HelpGetConfig, replacement.SyntheticSignin)
		}
		after := restoreDrillAccountSnapshot(t, fixture.dsn, username)
		if !bytes.Equal(before, after) {
			t.Fatal("checker changed the existing synthetic account")
		}
		originalInvocation := fixture.invocation(t, username, password, fixture.publicKeyPEM, &identity)
		if result := restoredrill.Run(fixture.ctx, originalInvocation); result.Category != restoredrill.CategoryAssertionsPassed || result.SyntheticSignin != restoredrill.AssertionPass {
			t.Fatalf("original credentials after rejected replacement = %q/%q, want pass", result.Category, result.SyntheticSignin)
		}
	})

	t.Run("wrong credentials return fixed auth failure", func(t *testing.T) {
		username := "drillwrongpassword"
		identity := fixture.createUser(t, username, "Correct-Synthetic-Password-90142")
		invocation := fixture.invocation(t, username, "Wrong-Password-Marker-12870", fixture.publicKeyPEM, &identity)
		result := restoredrill.Run(fixture.ctx, invocation)
		if result.Category != restoredrill.CategoryAuthFailure || result.HelpGetConfig != restoredrill.AssertionPass || result.SyntheticSignin != restoredrill.AssertionFail {
			t.Fatalf("wrong-credential result = %q/%q/%q, want auth_failure", result.Category, result.HelpGetConfig, result.SyntheticSignin)
		}
	})

	t.Run("wrong RSA key fails before sign-in", func(t *testing.T) {
		username := "drillwrongrsakey"
		identity := fixture.createUser(t, username, "Wrong-Key-Synthetic-Password-91730")
		before := restoreDrillUserCount(t, fixture.dsn)
		wrongKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal("could not create mismatched test key")
		}
		wrongPEM := restoreDrillPublicKeyPEM(t, &wrongKey.PublicKey)
		invocation := fixture.invocation(t, username, "Wrong-Key-Synthetic-Password-91730", wrongPEM, &identity)
		result := restoredrill.Run(fixture.ctx, invocation)
		if result.Category != restoredrill.CategoryKeyMismatch || result.HelpGetConfig != restoredrill.AssertionFail || result.SyntheticSignin != restoredrill.AssertionNotRun {
			t.Fatalf("wrong-key result = %q/%q/%q, want key_mismatch before sign-in", result.Category, result.HelpGetConfig, result.SyntheticSignin)
		}
		if after := restoreDrillUserCount(t, fixture.dsn); after != before {
			t.Fatal("wrong RSA key changed the existing synthetic accounts")
		}
	})

	t.Run("missing isolation evidence refuses before contact or provisioning", func(t *testing.T) {
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("could not create refusal sentinel")
		}
		defer func() {
			if err := listener.Close(); err != nil {
				t.Error("could not close refusal sentinel")
			}
		}()
		endpoint := "ws://" + listener.Addr().String() + "/apiws"
		identity := restoredrill.Identity{ID: 42, Username: "drillrefuseduser"}
		invocation := fixture.invocation(t, identity.Username, "Refused-Password-Marker-41590", fixture.publicKeyPEM, &identity)
		if err := rewriteRestoreDrillInvocation(invocation, func(value *restoredrill.Invocation) {
			value.Endpoint = endpoint
			value.NetworkNamespaceIsolated = false
		}); err != nil {
			t.Fatal("could not prepare refusal case")
		}
		result := restoredrill.Run(fixture.ctx, invocation)
		if result.Category != restoredrill.CategoryInvocationInvalid {
			t.Fatalf("unattested invocation category = %q, want invocation_invalid", result.Category)
		}
		tcpListener, ok := listener.(*net.TCPListener)
		if !ok {
			t.Fatal("refusal sentinel is not a TCP listener")
		}
		deadlineErr := tcpListener.SetDeadline(time.Now().Add(50 * time.Millisecond))
		if deadlineErr != nil {
			t.Fatal("could not bound refusal sentinel")
		}
		conn, err := listener.Accept()
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				t.Error("could not close unexpected connection")
			}
			t.Fatal("invalid invocation connected to its endpoint")
		}
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatal("refusal sentinel did not finish through its bounded timeout")
		}
	})

	t.Run("slow endpoint respects caller deadline", func(t *testing.T) {
		listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal("could not create bounded endpoint")
		}
		defer func() {
			if err := listener.Close(); err != nil {
				t.Error("could not stop bounded endpoint")
			}
		}()
		ctx, cancel := context.WithTimeout(fixture.ctx, 150*time.Millisecond)
		defer cancel()
		endpoint := "ws://" + listener.Addr().String() + "/apiws"
		result := restoredrill.CheckConfig(ctx, endpoint, fixture.publicKeyPEM)
		if result.Category != restoredrill.CategoryTimeout {
			t.Fatalf("bounded endpoint category = %q, want timeout", result.Category)
		}
	})
}

func TestRestoreDrillMissingProvisionedIdentityOutputIsPrivate(t *testing.T) {
	fixture := startRestoreDrillDaemon(t)
	username := "drillfailedoutputuser"
	password := "Failed-Provisioning-Private-Password-48217"
	invocation := fixture.invocation(t, username, password, fixture.publicKeyPEM, nil)

	var stdout, stderr bytes.Buffer
	if code := restoredrill.RunCLI(fixture.ctx, invocation, &stdout, &stderr); code == 0 {
		t.Fatal("checker accepted a missing provisioned identity")
	}
	var result restoredrill.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal("checker stdout was not a typed result")
	}
	if result.Category != restoredrill.CategoryProvisioningFailure ||
		result.Phase != restoredrill.PhaseProvisioning ||
		result.HelpGetConfig != restoredrill.AssertionNotRun ||
		result.SyntheticSignin != restoredrill.AssertionNotRun {
		t.Fatal("missing provisioned identity did not produce the fixed provisioning result")
	}
	artifact := readRestoreDrillResult(t, invocation)
	for _, output := range [][]byte{stdout.Bytes(), stderr.Bytes(), artifact} {
		for _, marker := range []string{username, password, fixture.dsn, fixture.endpoint} {
			if bytes.Contains(output, []byte(marker)) {
				t.Fatal("checker output or artifact exposed private input")
			}
		}
	}
	if stderr.Len() != 0 {
		t.Fatal("checker wrote failure diagnostics to stderr")
	}
}

type restoreDrillDaemon struct {
	ctx          context.Context
	dsn          string
	endpoint     string
	publicKeyPEM []byte
	store        *store.Store
}

type restoreDrillProcess struct {
	done    chan struct{}
	waitErr error
}

func startRestoreDrillDaemon(t *testing.T) *restoreDrillDaemon {
	t.Helper()
	ctx := context.Background()
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-20*time.Second))
		t.Cleanup(cancel)
	}
	dsn := pgtest.DSN(t)
	root := t.TempDir()
	binary := buildSmokeTeagramd(t)
	privateKeyPath := filepath.Join(root, "server-key.pem")
	privateKey, err := rsakey.Bootstrap(privateKeyPath)
	if err != nil {
		t.Fatal("could not create synthetic daemon identity")
	}
	publicKeyPEM := restoreDrillPublicKeyPEM(t, &privateKey.PublicKey)
	authKey := make([]byte, 32)
	if _, err := rand.Read(authKey); err != nil {
		t.Fatal("could not create synthetic auth-key encryption material")
	}
	authKeyFile := filepath.Join(root, "authkey.hex")
	if err := os.WriteFile(authKeyFile, []byte(hex.EncodeToString(authKey)), 0o600); err != nil {
		t.Fatal("could not protect synthetic auth-key encryption material")
	}
	fixtureStore, err := store.Open(ctx, dsn, authKey, store.WithoutBlobStore())
	clear(authKey)
	if err != nil {
		t.Fatal("could not open synthetic fixture store")
	}
	t.Cleanup(func() {
		if err := fixtureStore.Close(); err != nil {
			t.Error("could not close synthetic fixture store")
		}
	})

	tcpPort := reserveRestoreDrillPort(t)
	websocketPort := reserveRestoreDrillPort(t)
	endpoint := "ws://127.0.0.1:" + strconv.Itoa(websocketPort) + "/apiws"
	blobDir := filepath.Join(root, "empty-blobs")
	if err := os.Mkdir(blobDir, 0o700); err != nil {
		t.Fatal("could not create empty synthetic blob directory")
	}
	command := osexec.CommandContext(ctx, binary, "serve") // #nosec G204 -- fixed local server binary and arguments.
	command.Env = []string{
		"TG_POSTGRES_DSN=" + dsn,
		"TG_AUTHKEY_ENC_KEY_FILE=" + authKeyFile,
		"TG_RSA_KEY_PATH=" + privateKeyPath,
		"TG_LISTEN_ADDR=127.0.0.1:" + strconv.Itoa(tcpPort),
		"TG_ADVERTISE_ADDR=127.0.0.1:" + strconv.Itoa(tcpPort),
		"TG_WEBSOCKET_LISTEN_ADDR=127.0.0.1:" + strconv.Itoa(websocketPort),
		"TG_REGISTRATION=closed",
		"TG_LOG_LOGIN_CODES=false",
		"TG_PUBLIC_LINK_PREFIX=https://scratch.test/",
		"TG_BLOB_DIR=" + blobDir,
		"TG_MEDIA_ERASURE_DESTRUCTIVE=false",
		"TG_MEDIA_ERASURE_INTERVAL_MIN=0s",
		"TG_MEDIA_ERASURE_INTERVAL_MAX=0s",
		"TG_MEDIA_ERASURE_REPORT_INTERVAL=0s",
		"TG_BLOB_SCAN_REPORT_INTERVAL=0s",
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal("could not start actual synthetic daemon")
	}
	process := &restoreDrillProcess{done: make(chan struct{})}
	go func() {
		process.waitErr = command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if command.Process == nil {
			return
		}
		if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error("could not stop actual synthetic daemon")
		}
		select {
		case <-process.done:
			if process.waitErr != nil {
				var exitErr *osexec.ExitError
				if !errors.As(process.waitErr, &exitErr) || exitErr.ExitCode() != -1 {
					t.Error("actual synthetic daemon exited unexpectedly during shutdown")
				}
			}
		case <-time.After(10 * time.Second):
			if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				t.Error("could not terminate actual synthetic daemon")
			}
			<-process.done
			t.Error("actual synthetic daemon exceeded its shutdown bound")
		}
	})
	waitRestoreDrillReady(t, ctx, process.done, websocketPort)
	return &restoreDrillDaemon{ctx: ctx, dsn: dsn, endpoint: endpoint, publicKeyPEM: publicKeyPEM, store: fixtureStore}
}

func (f *restoreDrillDaemon) invocation(t *testing.T, username, password string, key []byte, identity *restoredrill.Identity) string {
	t.Helper()
	dir := t.TempDir()
	if err := unix.Chmod(dir, 0o700); err != nil {
		t.Fatal("could not protect synthetic invocation directory")
	}
	writeRestoreDrillFile(t, dir, "server.pub.pem", key, 0o400)
	writeRestoreDrillFile(t, dir, "username", []byte(username+"\n"), 0o400)
	writeRestoreDrillFile(t, dir, "password", []byte(password+"\n"), 0o400)
	if identity != nil {
		provisioned, err := json.Marshal(struct {
			SchemaVersion int    `json:"schema_version"`
			UserID        int64  `json:"user_id"`
			Username      string `json:"username"`
		}{SchemaVersion: 1, UserID: identity.ID, Username: identity.Username})
		if err != nil {
			t.Fatal("could not encode provisioned synthetic identity")
		}
		writeRestoreDrillFile(t, dir, "provisioned.json", provisioned, 0o400)
	}
	invocation := restoredrill.Invocation{
		SchemaVersion:            1,
		Mode:                     "synthetic-only",
		Endpoint:                 f.endpoint,
		ScratchTargetOwned:       true,
		NetworkNamespaceIsolated: true,
		DaemonIdentityPinned:     true,
		RegistrationClosed:       true,
	}
	encoded, err := json.Marshal(invocation)
	if err != nil {
		t.Fatal("could not encode synthetic invocation")
	}
	writeRestoreDrillFile(t, dir, "invocation.json", encoded, 0o600)
	return filepath.Join(dir, "invocation.json")
}

func (f *restoreDrillDaemon) createUser(t *testing.T, username, password string) restoredrill.Identity {
	t.Helper()
	salt1 := make([]byte, 32)
	if _, err := rand.Read(salt1); err != nil {
		t.Fatal("could not create synthetic password salt")
	}
	defer clear(salt1)
	salt2 := make([]byte, 32)
	if _, err := rand.Read(salt2); err != nil {
		t.Fatal("could not create synthetic password salt")
	}
	defer clear(salt2)
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(rand.Reader).NewHash(passwordBytes, gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     tsrp.G,
		P:     tsrp.PBytes(),
	})
	if err != nil {
		t.Fatal("could not derive synthetic account password")
	}
	defer clear(verifier)
	defer clear(augmentedSalt1)
	user, err := f.store.CreateUsernameAccountWithPassword(f.ctx, username, augmentedSalt1, salt2, verifier)
	if err != nil {
		t.Fatal("could not provision synthetic account fixture")
	}
	return restoredrill.Identity{ID: user.ID, Username: username}
}

func waitRestoreDrillReady(t *testing.T, ctx context.Context, done <-chan struct{}, port int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatal("actual synthetic daemon exited before readiness")
		default:
		}
		dialer := &net.Dialer{Timeout: 100 * time.Millisecond}
		connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			if closeErr := connection.Close(); closeErr != nil {
				t.Fatal("could not close readiness connection")
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("actual synthetic daemon readiness exceeded its bound")
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("actual synthetic daemon readiness exceeded its bound")
}

func reserveRestoreDrillPort(t *testing.T) int {
	t.Helper()
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("could not reserve loopback port")
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		if err := listener.Close(); err != nil {
			t.Error("could not release loopback port")
		}
		t.Fatal("loopback listener has a non-TCP address")
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		t.Fatal("could not release loopback port")
	}
	return port
}

func restoreDrillPublicKeyPEM(t *testing.T, key *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal("could not encode synthetic RSA public key")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func writeRestoreDrillFile(t *testing.T, dir, name string, content []byte, mode os.FileMode) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, mode); err != nil { // #nosec G703 -- test uses fixed file names under private temporary directories.
		t.Fatal("could not write protected synthetic invocation file")
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal("could not protect synthetic invocation file")
	}
}

func readRestoreDrillResult(t *testing.T, invocation string) []byte {
	t.Helper()
	result, err := os.ReadFile(filepath.Join(filepath.Dir(invocation), "result.json"))
	if err != nil {
		t.Fatal("checker result artifact was not written")
	}
	return result
}

func rewriteRestoreDrillInvocation(invocation string, mutate func(*restoredrill.Invocation)) error {
	data, err := os.ReadFile(invocation)
	if err != nil {
		return err
	}
	var value restoredrill.Invocation
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	mutate(&value)
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.WriteFile(invocation, encoded, 0o600); err != nil {
		return err
	}
	return nil
}

func restoreDrillAccountSnapshot(t *testing.T, dsn, username string) []byte {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal("could not inspect synthetic account state")
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("could not close synthetic account inspection")
		}
	}()
	var user, password string
	if err := conn.QueryRow(context.Background(), `
		SELECT
		  (SELECT to_jsonb(u)::text FROM users u WHERE u.username = $1),
		  (SELECT to_jsonb(p)::text FROM user_passwords p JOIN users u ON u.id = p.user_id WHERE u.username = $1)
	`, username).Scan(&user, &password); err != nil {
		t.Fatal("could not snapshot synthetic account state")
	}
	return []byte(user + "\n" + password)
}

func setRestoreDrillRecoverySettings(t *testing.T, dsn, username string) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal("could not open synthetic recovery-settings fixture")
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("could not close synthetic recovery-settings fixture")
		}
	}()
	tag, err := conn.Exec(context.Background(), `
		UPDATE user_passwords
		SET hint = 'synthetic recovery marker', recovery_email = 'recovery@example.invalid', has_recovery = true
		WHERE user_id = (SELECT id FROM users WHERE username = $1)
	`, username)
	if err != nil {
		t.Fatal("could not seed synthetic recovery settings")
	}
	if tag.RowsAffected() != 1 {
		t.Fatal("synthetic recovery settings row was not found")
	}
}

func restoreDrillUserCount(t *testing.T, dsn string) int {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal("could not verify synthetic account count")
	}
	defer func() {
		if err := conn.Close(context.Background()); err != nil {
			t.Error("could not close synthetic account count check")
		}
	}()
	var count int
	if err := conn.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&count); err != nil {
		t.Fatal("could not read synthetic account count")
	}
	return count
}
