package e2e_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

const (
	realFixtureServerRevision = "7b5fcc9c68c1b275cad7d076a343d6d476cd447d"
	realFixtureWebRevision    = "84961bf77003a1bdb582d1096d988f1d304e3d1f"
)

func TestRealServerFixtureUsesCIAMD64BrowserImage(t *testing.T) {
	workflow, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	ciImage := regexp.MustCompile(`(?m)^[ \t]*-[ \t]*arch:[ \t]*amd64\n[ \t]*runner:[ \t]*ubuntu-26\.04\n[ \t]*platform:[ \t]*linux/amd64\n[ \t]*image:[ \t]*(\S+)$`).FindSubmatch(workflow)
	if len(ciImage) != 2 {
		t.Fatal("CI workflow has no pinned amd64 Playwright image")
	}
	dockerfile, err := os.ReadFile(filepath.Join("real_server_fixture", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	fixtureImage := regexp.MustCompile(`(?m)^FROM (mcr\.microsoft\.com/playwright[^\r\n]+)$`).FindSubmatch(dockerfile)
	if len(fixtureImage) != 2 {
		t.Fatal("fixture Dockerfile has no pinned Playwright image")
	}
	if !bytes.Equal(fixtureImage[1], ciImage[1]) {
		t.Fatalf("fixture Playwright image = %q, want CI amd64 image %q", fixtureImage[1], ciImage[1])
	}
}

func TestRealServerFixtureRejectsLiveEndpointBeforeMutation(t *testing.T) {
	script := filepath.Join("real_server_fixture", "run.sh")
	command := fixtureCommand(context.Background(), "bash", script,
		"--server-revision", realFixtureServerRevision,
		"--web-revision", realFixtureWebRevision,
		"--run-id", "00000000000000000000000000000000",
		"--endpoint", "wss://telegram-server.tailaa4918.ts.net/apiws",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("fixture accepted a caller-supplied endpoint")
	}
	if !strings.Contains(stderr.String(), "--endpoint is not accepted") {
		t.Fatalf("fixture rejection = %q, want explicit endpoint rejection", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("fixture rejection wrote readiness output %q", stdout.String())
	}
}

func TestRealServerFixtureRejectsUnapprovedWebRevisionBeforeMutation(t *testing.T) {
	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", "09373cc2713d31e93664c38a4fd0335ea37a5f01",
		"--run-id", "00000000000000000000000000000000",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("unapproved web revision exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "web revision does not match the accepted fixture pin") {
		t.Fatalf("unapproved web revision diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("unapproved web revision wrote readiness output %q", stdout.String())
	}
}

func TestRealServerFixtureRejectsServerRevisionMismatchBeforeMutation(t *testing.T) {
	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", "6668a0a3519909ef512fdc59e4937975f108671d",
		"--web-revision", realFixtureWebRevision,
		"--run-id", "00000000000000000000000000000000",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("server revision mismatch exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "server revision does not match the checked-out source") {
		t.Fatalf("server revision mismatch diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("server revision mismatch wrote readiness output %q", stdout.String())
	}
}

func TestRealServerFixtureRejectsResourceCollisionBeforeMutation(t *testing.T) {
	runID := newRealFixtureRunID(t)
	network := "telegram-fixture-" + runID + "edge"
	if output, err := fixtureCommand(context.Background(), "docker", "network", "create", "--driver", "bridge", "--internal", "--ipv6=false", network).CombinedOutput(); err != nil {
		t.Fatalf("create unowned collision network: %v: %s", err, strings.TrimSpace(string(output)))
	}
	t.Cleanup(func() {
		if output, err := fixtureCommand(context.Background(), "docker", "network", "rm", network).CombinedOutput(); err != nil {
			t.Errorf("remove collision test network: %v: %s", err, strings.TrimSpace(string(output)))
		}
	})

	command := fixtureCommand(context.Background(), "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("resource collision exit = %v, want exit 2; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "resource name already exists: "+network) {
		t.Fatalf("resource collision diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("resource collision wrote readiness output %q", stdout.String())
	}
	if err := fixtureCommand(context.Background(), "docker", "network", "inspect", network).Run(); err != nil {
		t.Fatalf("fixture removed an unowned collision network: %v", err)
	}
	for _, suffix := range []string{"browser", "tls-front", "telegramd", "database", "client", "atlas"} {
		if err := fixtureCommand(context.Background(), "docker", "container", "inspect", "telegram-fixture-"+runID+suffix).Run(); err == nil {
			t.Errorf("fixture created container %s before rejecting the collision", suffix)
		}
	}
}

func TestRealServerFixture(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	serverRevision := currentServerRevision(t)
	runA, runB := newRealFixtureRunID(t), newRealFixtureRunID(t)
	if runA == runB {
		t.Fatal("random fixture run IDs collided")
	}

	type startResult struct {
		fixture *realFixtureProcess
		err     error
	}
	started := make(chan startResult, 2)
	var wait sync.WaitGroup
	for _, runID := range []string{runA, runB} {
		wait.Add(1)
		go func(id string) {
			defer wait.Done()
			fixture, err := startRealServerFixture(ctx, id, serverRevision, realFixtureWebRevision, nil)
			started <- startResult{fixture: fixture, err: err}
		}(runID)
	}
	wait.Wait()
	close(started)
	var fixtures []*realFixtureProcess
	for result := range started {
		if result.err != nil {
			for _, fixture := range fixtures {
				if err := fixture.stopBySignal(); err != nil {
					t.Errorf("stop partially started fixture: %v", err)
				}
			}
			t.Fatal(result.err)
		}
		fixtures = append(fixtures, result.fixture)
	}
	if len(fixtures) != 2 {
		t.Fatalf("ready fixtures = %d, want 2", len(fixtures))
	}
	a, b := fixtures[0], fixtures[1]
	t.Cleanup(func() {
		for _, fixture := range fixtures {
			if !fixture.stopped {
				if err := fixture.stopBySignal(); err != nil {
					t.Errorf("stop fixture during test cleanup: %v", err)
				}
			}
		}
	})
	validateRealFixtureReady(t, a, runA, serverRevision, realFixtureWebRevision)
	validateRealFixtureReady(t, b, runB, serverRevision, realFixtureWebRevision)
	if a.ready.RunID == b.ready.RunID || a.ready.Credentials[0].Username == b.ready.Credentials[0].Username {
		t.Fatal("concurrent fixtures reused a run identity or synthetic username")
	}
	if a.ready.Fingerprint == b.ready.Fingerprint {
		t.Fatal("concurrent fixtures reused an MTProto RSA identity")
	}

	marker := "fixture-isolation-" + newRealFixtureRunID(t)
	if output, err := sendFixtureMessage(ctx, a, a.ready.Credentials[1].Username, marker); err != nil {
		t.Fatalf("send isolation probe: %v: %s", err, output)
	}
	if got := fixtureSQL(ctx, t, a, "SELECT count(*) FROM messages WHERE message = '"+marker+"'"); got != "2" {
		t.Fatalf("sender fixture message rows = %q, want 2", got)
	}
	if got := fixtureSQL(ctx, t, b, "SELECT count(*) FROM messages WHERE message = '"+marker+"'"); got != "0" {
		t.Fatalf("second fixture can see first fixture's message: rows=%q", got)
	}
	if got := fixtureSQL(ctx, t, b, "SELECT count(*) FROM auth_keys WHERE user_id IS NOT NULL"); got != "2" {
		t.Fatalf("fresh second fixture auth keys = %q, want only its two new client sessions", got)
	}

	if err := a.stopByEOF(); err != nil {
		t.Fatalf("stop first fixture: %v", err)
	}
	assertFixtureResourcesAbsent(t, a.ready)
	if err := authenticateFixtureUser(ctx, b, 0); err != nil {
		t.Fatalf("second fixture after stopping first: %v", err)
	}
	if err := b.stopBySignal(); err != nil {
		t.Fatalf("stop second fixture by cancellation: %v", err)
	}
	assertFixtureResourcesAbsent(t, b.ready)
}

func TestRealServerFixtureStartupFailureCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	serverRevision := currentServerRevision(t)
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", serverRevision,
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_FAIL_AFTER=server-ready")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		t.Fatal("injected startup failure returned success")
	}
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("injected startup failure = %v, want exit 42; stderr=%q", err, stderr.String())
	}
	if stdout.Len() != 0 || strings.Contains(stdout.String(), "mtprotoPublicKey") {
		t.Fatalf("failed fixture advertised readiness: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "injected startup failure after server-ready") {
		t.Fatalf("failure diagnostic = %q", stderr.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureReadinessTimeoutCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_READINESS_TIMEOUT=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("injected readiness timeout exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "telegramd readiness timeout") {
		t.Fatalf("readiness timeout diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("unready fixture wrote readiness output %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureCleanupFailureIsNonzero(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_FAIL_AFTER=server-ready", "TELEGRAM_FIXTURE_TEST_CLEANUP_FAILURE=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("injected cleanup failure exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "injected cleanup failure") || !strings.Contains(stderr.String(), "cleanup=failed") {
		t.Fatalf("cleanup failure diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("fixture with a cleanup failure wrote readiness output %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

func TestRealServerFixtureRejectsTargetKeyMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	runID := newRealFixtureRunID(t)
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", currentServerRevision(t),
		"--web-revision", realFixtureWebRevision,
		"--run-id", runID,
	)
	command.Env = append(os.Environ(), "TELEGRAM_FIXTURE_TEST_MISMATCH_TARGET=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("target key mismatch exit = %v, want exit 1; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), `"code":"target_manifest_mismatch"`) {
		t.Fatalf("target key mismatch diagnostic = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("mismatched fixture wrote readiness output %q", stdout.String())
	}
	assertNamedFixtureResourcesAbsent(t, runID)
}

type realFixtureCredential struct {
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
}

type realFixtureEvidence struct {
	HTTPStatus                  int                     `json:"httpStatus"`
	WSSUpgradeStatus            int                     `json:"wssUpgradeStatus"`
	AllowedWSSObserved          int                     `json:"allowedWssObserved"`
	WorkerProbes                map[string]fixtureProbe `json:"workerProbes"`
	ObserverControlledAttempts  map[string]int          `json:"observerControlledAttempts"`
	DirectTCP                   fixtureProbe            `json:"directTCP"`
	UnexpectedAttempts          int                     `json:"unexpectedAttempts"`
	UnexpectedDetectionVerified bool                    `json:"unexpectedDetectionVerified"`
}

type fixtureProbe struct {
	Attempted int `json:"attempted"`
	Blocked   int `json:"blocked"`
}

type realFixtureSecurity struct {
	RegistrationClosed  bool `json:"registrationClosed"`
	LoginCodeLogging    bool `json:"loginCodeLogging"`
	ElectionClosed      bool `json:"electionClosed"`
	AdministratorIsNull bool `json:"administratorIsNull"`
	OrdinaryUsers       int  `json:"ordinaryUsers"`
	UsernameAccounts    int  `json:"usernameAccounts"`
	PasswordVerifiers   int  `json:"passwordVerifiers"`
	InitialAuthKeys     int  `json:"initialAuthKeys"`
	InitialMessages     int  `json:"initialMessages"`
	FinalAuthKeys       int  `json:"finalAuthKeys"`
}

type realFixtureCleanup struct {
	Kind            string `json:"kind"`
	ProcessID       int    `json:"processId"`
	ResourcePrefix  string `json:"resourcePrefix"`
	SecretDirectory string `json:"secretDirectory"`
}

type realFixtureReady struct {
	Status         string                  `json:"status"`
	RunID          string                  `json:"runId"`
	ServerRevision string                  `json:"serverRevision"`
	WebRevision    string                  `json:"webRevision"`
	EvidenceClass  string                  `json:"evidenceClass"`
	Endpoint       string                  `json:"endpoint"`
	WSSEndpoint    string                  `json:"wssEndpoint"`
	PublicKeyPEM   string                  `json:"mtprotoPublicKeyPEM"`
	Fingerprint    string                  `json:"fingerprint"`
	LeafSPKI       string                  `json:"leafSPKI"`
	Credentials    []realFixtureCredential `json:"credentials"`
	Security       realFixtureSecurity     `json:"security"`
	Evidence       realFixtureEvidence     `json:"evidence"`
	Cleanup        realFixtureCleanup      `json:"cleanupHandle"`
}

type realFixtureProcess struct {
	cmd     *osexec.Cmd
	stdin   io.WriteCloser
	ready   realFixtureReady
	stderr  *synchronizedBuffer
	stopped bool
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(data)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func startRealServerFixture(ctx context.Context, runID, serverRevision, webRevision string, extraEnv []string) (*realFixtureProcess, error) {
	command := fixtureCommand(ctx, "bash", filepath.Join("real_server_fixture", "run.sh"),
		"--server-revision", serverRevision,
		"--web-revision", webRevision,
		"--run-id", runID,
	)
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return command.Process.Signal(syscall.SIGTERM)
	}
	command.WaitDelay = 10 * time.Second
	command.Env = append(os.Environ(), extraEnv...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdout pipe: %w", err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("fixture stdin pipe: %w", err)
	}
	stderr := &synchronizedBuffer{}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start real-server fixture: %w", err)
	}
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		closeErr := stdin.Close()
		waitErr := command.Wait()
		return nil, fmt.Errorf("fixture did not report readiness (stderr=%q): %w", stderr.String(), errors.Join(err, closeErr, waitErr))
	}
	var ready realFixtureReady
	if err := json.Unmarshal([]byte(line), &ready); err != nil {
		closeErr := stdin.Close()
		waitErr := command.Wait()
		return nil, fmt.Errorf("decode fixture readiness: %w", errors.Join(err, closeErr, waitErr))
	}
	fixture := &realFixtureProcess{cmd: command, stdin: stdin, ready: ready, stderr: stderr}
	return fixture, nil
}

func (f *realFixtureProcess) stopByEOF() error {
	if f.stopped {
		return nil
	}
	if err := f.stdin.Close(); err != nil {
		return fmt.Errorf("close fixture control stream: %w", err)
	}
	err := f.cmd.Wait()
	f.stopped = true
	if err != nil {
		return fmt.Errorf("fixture cleanup failed: %w: %s", err, f.stderr.String())
	}
	return nil
}

func (f *realFixtureProcess) stopBySignal() error {
	if f.stopped {
		return nil
	}
	if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal fixture cleanup: %w", err)
	}
	err := f.cmd.Wait()
	f.stopped = true
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 143 {
		return fmt.Errorf("fixture cancellation exit = %w, want 143: %s", err, f.stderr.String())
	}
	return nil
}

func currentServerRevision(t *testing.T) string {
	t.Helper()
	output, err := fixtureCommand(context.Background(), "git", "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("read server revision: %v", err)
	}
	return strings.TrimSpace(string(output))
}

func newRealFixtureRunID(t *testing.T) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate fixture identity: %v", err)
	}
	return hex.EncodeToString(random)
}

func validateRealFixtureReady(t *testing.T, fixture *realFixtureProcess, runID, serverRevision, webRevision string) {
	t.Helper()
	ready := fixture.ready
	if ready.Status != "ready" || ready.RunID != runID || ready.ServerRevision != serverRevision || ready.WebRevision != webRevision {
		t.Fatalf("fixture readiness metadata does not match pinned inputs: status=%q run=%q server=%q web=%q", ready.Status, ready.RunID, ready.ServerRevision, ready.WebRevision)
	}
	if ready.EvidenceClass != "production-telegramd" {
		t.Fatalf("fixture evidence class = %q, want production-telegramd", ready.EvidenceClass)
	}
	if ready.Endpoint != "https://telegramd.test" || ready.WSSEndpoint != "wss://telegramd.test/apiws" {
		t.Fatalf("fixture endpoints = %q / %q, want fixed synthetic origin", ready.Endpoint, ready.WSSEndpoint)
	}
	if ready.Fingerprint == "fbb62871f07fae2a" || !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(ready.Fingerprint) {
		t.Fatalf("fixture fingerprint is invalid or matches the production identity: %q", ready.Fingerprint)
	}
	block, rest := pemDecode([]byte(ready.PublicKeyPEM))
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatal("fixture did not return a single PEM public key")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse fixture public key: %v", err)
	}
	rsaPublicKey, ok := publicKey.(*rsa.PublicKey)
	if !ok || rsaPublicKey.N.BitLen() != 2048 || rsaPublicKey.E != 65537 {
		t.Fatalf("fixture public key = %T, want RSA-2048/65537", publicKey)
	}
	fingerprint := rsakey.Fingerprint(rsaPublicKey)
	//nolint:gosec // Preserve the MTProto fingerprint's full 64-bit wire representation.
	if got := fmt.Sprintf("%016x", uint64(fingerprint)); got != ready.Fingerprint {
		t.Fatalf("fixture public key fingerprint = %q, descriptor says %q", got, ready.Fingerprint)
	}
	spki, err := base64.StdEncoding.DecodeString(ready.LeafSPKI)
	if err != nil || len(spki) != 32 {
		t.Fatalf("TLS leaf SPKI is not a SHA-256 pin: length=%d err=%v", len(spki), err)
	}
	certificatePEM, err := os.ReadFile(filepath.Join(ready.Cleanup.SecretDirectory, "tls.crt"))
	if err != nil {
		t.Fatalf("read fixture TLS leaf certificate: %v", err)
	}
	certificateBlock, rest := pemDecode(certificatePEM)
	if certificateBlock == nil || len(strings.TrimSpace(string(rest))) != 0 {
		t.Fatal("fixture TLS certificate is not a single PEM block")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		t.Fatalf("parse fixture TLS leaf certificate: %v", err)
	}
	if err := certificate.VerifyHostname("telegramd.test"); err != nil {
		t.Fatalf("fixture TLS certificate hostname: %v", err)
	}
	leafSPKI := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	if !bytes.Equal(spki, leafSPKI[:]) {
		t.Fatal("returned TLS SPKI does not match the fixture leaf certificate")
	}
	if len(ready.Credentials) != 2 || ready.Credentials[0].Username == ready.Credentials[1].Username {
		t.Fatalf("fixture credentials do not contain two distinct users")
	}
	passwords := make(map[string]struct{}, 2)
	for _, credential := range ready.Credentials {
		if filepath.Dir(credential.PasswordFile) != ready.Cleanup.SecretDirectory {
			t.Fatalf("protected credential path is outside the fixture secret directory: %q", credential.PasswordFile)
		}
		info, err := os.Stat(credential.PasswordFile)
		if err != nil {
			t.Fatalf("protected credential reference is unavailable: %v", err)
		}
		if info.Mode().Perm() != 0o400 {
			t.Fatalf("password file permissions = %o, want 400", info.Mode().Perm())
		}
		secret, err := os.ReadFile(credential.PasswordFile)
		if err != nil {
			t.Fatalf("read protected credential: %v", err)
		}
		passwords[string(secret)] = struct{}{}
		if strings.Contains(fixture.stderr.String(), string(secret)) {
			t.Fatal("fixture wrote a synthetic password to stderr")
		}
	}
	if len(passwords) != 2 {
		t.Fatal("fixture generated duplicate passwords")
	}
	if !ready.Security.RegistrationClosed || ready.Security.LoginCodeLogging || !ready.Security.ElectionClosed || !ready.Security.AdministratorIsNull || ready.Security.OrdinaryUsers != 2 || ready.Security.UsernameAccounts != 2 || ready.Security.PasswordVerifiers != 2 || ready.Security.InitialAuthKeys != 0 || ready.Security.InitialMessages != 0 || ready.Security.FinalAuthKeys != 2 {
		t.Fatalf("fixture security state did not hold before and after authentication: %+v", ready.Security)
	}
	if ready.Evidence.HTTPStatus != 200 || ready.Evidence.WSSUpgradeStatus != 101 || ready.Evidence.AllowedWSSObserved != 1 {
		t.Fatalf("fixture HTTPS/WSS evidence = %+v", ready.Evidence)
	}
	for _, contextName := range []string{"page", "shared_worker", "service_worker"} {
		probe, ok := ready.Evidence.WorkerProbes[contextName]
		if !ok || probe.Attempted != 8 || probe.Blocked != 8 || ready.Evidence.ObserverControlledAttempts[contextName] != 8 {
			t.Fatalf("%s egress probe was skipped, unblocked, or unobserved: probe=%+v evidence=%+v", contextName, probe, ready.Evidence.ObserverControlledAttempts)
		}
	}
	if ready.Evidence.DirectTCP.Attempted != 5 || ready.Evidence.DirectTCP.Blocked != 5 || ready.Evidence.UnexpectedAttempts != 0 || !ready.Evidence.UnexpectedDetectionVerified {
		t.Fatalf("fixture egress boundary evidence = %+v", ready.Evidence)
	}
	if ready.Cleanup.Kind != "stdin-eof-or-signal" || ready.Cleanup.ProcessID <= 0 || ready.Cleanup.ResourcePrefix != "telegram-fixture-"+runID || !filepath.IsAbs(ready.Cleanup.SecretDirectory) {
		t.Fatalf("fixture cleanup handle is incomplete: %+v", ready.Cleanup)
	}
}

func sendFixtureMessage(ctx context.Context, fixture *realFixtureProcess, recipient, marker string) (string, error) {
	credential := fixture.ready.Credentials[0]
	command := fixtureCommand(ctx, "docker", "exec", fixture.ready.Cleanup.ResourcePrefix+"client",
		"/run/app/fixture-auth-check", "--endpoint", "wss://telegramd.test/apiws",
		"--tls-root-file", "/run/secrets/tls.crt",
		"--public-key", "/run/secrets/server.pub.pem",
		"--username", credential.Username,
		"--password-file", "/run/secrets/a-password",
		"--send-message-to", recipient,
		"--message", marker,
	)
	output, err := command.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func authenticateFixtureUser(ctx context.Context, fixture *realFixtureProcess, index int) error {
	credential := fixture.ready.Credentials[index]
	passwordFile := []string{"a-password", "b-password"}[index]
	command := fixtureCommand(ctx, "docker", "exec", fixture.ready.Cleanup.ResourcePrefix+"client",
		"/run/app/fixture-auth-check", "--endpoint", "wss://telegramd.test/apiws",
		"--tls-root-file", "/run/secrets/tls.crt",
		"--public-key", "/run/secrets/server.pub.pem",
		"--username", credential.Username,
		"--password-file", "/run/secrets/"+passwordFile,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("real SRP login failed: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func fixtureSQL(ctx context.Context, t *testing.T, fixture *realFixtureProcess, query string) string {
	t.Helper()
	command := fixtureCommand(ctx, "docker", "exec", fixture.ready.Cleanup.ResourcePrefix+"database",
		"psql", "-U", "postgres", "-d", "telegram", "-X", "-qAt", "-c", query)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture database assertion failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output))
}

func assertFixtureResourcesAbsent(t *testing.T, ready realFixtureReady) {
	t.Helper()
	assertNamedFixtureResourcesAbsent(t, ready.RunID)
	for _, credential := range ready.Credentials {
		if _, err := os.Stat(credential.PasswordFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("fixture left a protected password file after cleanup: %v", err)
		}
	}
	if _, err := os.Stat(ready.Cleanup.SecretDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fixture left its secret directory after cleanup: %v", err)
	}
}

func assertNamedFixtureResourcesAbsent(t *testing.T, runID string) {
	t.Helper()
	prefix := "telegram-fixture-" + runID
	for _, suffix := range []string{"browser", "tls-front", "telegramd", "database", "client", "atlas"} {
		assertDockerResourceAbsent(t, "container", prefix+suffix)
	}
	for _, suffix := range []string{"edge", "server"} {
		assertDockerResourceAbsent(t, "network", prefix+suffix)
	}
}

func assertDockerResourceAbsent(t *testing.T, kind, name string) {
	t.Helper()
	output, err := fixtureCommand(context.Background(), "docker", kind, "inspect", name).CombinedOutput()
	if err == nil {
		t.Errorf("fixture left owned %s %s", kind, name)
		return
	}
	var exitErr *osexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Errorf("inspect fixture %s %s: %v: %s", kind, name, err, strings.TrimSpace(string(output)))
		return
	}
	message := string(output)
	missing := kind == "container" && (strings.Contains(message, "No such object:") || strings.Contains(message, "No such container:"))
	missing = missing || kind == "network" && strings.Contains(message, "network "+name+" not found")
	if !missing {
		t.Errorf("cannot verify fixture %s %s is absent: %v: %s", kind, name, err, strings.TrimSpace(message))
	}
}

func fixtureCommand(ctx context.Context, name string, args ...string) *osexec.Cmd {
	if ctx == nil {
		ctx = context.Background()
	}
	switch name {
	case "bash", "docker", "git":
	default:
		panic("unexpected fixture command: " + name)
	}
	//nolint:gosec // The executable is allowlisted and arguments are passed as distinct argv fields, never shell text.
	return osexec.CommandContext(ctx, name, args...)
}

func pemDecode(data []byte) (*pem.Block, []byte) {
	return pem.Decode(data)
}
