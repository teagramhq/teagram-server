package e2e_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobinventory"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	"github.com/teagramhq/teagram-server/internal/store"
)

const migrationTestBucket = "blob-migration-test"

func TestBlobEncryptionMigrationKillResume(t *testing.T) {
	if !requireRootMigrationFixture(t) {
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()

	remote := newMigrationS3(t)
	s3Config := blob.S3Config{
		Endpoint: remote.server.URL, Bucket: migrationTestBucket, Prefix: "teagramd/",
		Region: "us-east-1", AccessKeyID: "test-access", SecretAccessKey: "test-secret",
		AllowInsecureHTTP: true, OperationTimeout: 20 * time.Second,
	}
	plainStore, _, err := blob.NewS3Store(ctx, s3Config, nil, false)
	if err != nil {
		t.Fatalf("create accepted S3 serving store: %v", err)
	}
	dsn := pgtest.DSN(t)
	serverKey, err := rsakey.Bootstrap(filepath.Join(t.TempDir(), "server-key.pem"))
	if err != nil {
		t.Fatalf("bootstrap server key: %v", err)
	}
	const phone = "+15550124777"
	initial := startMigrationMediaHarness(t, ctx, dsn, serverKey, phone, plainStore, true)
	payload := mediaPayload(mediaPartSize + 37)
	doc := uploadMediaDocument(t, ctx, initial.client, initial.client.id, 0x5ED24777, "migration.bin", payload)
	secondPayload := []byte("second plaintext file survives cutover")
	secondDoc := uploadMediaDocument(t, ctx, initial.client, initial.client.id, 0x5ED24779, "second.bin", secondPayload)
	livePartPayload := []byte("live upload part survives migration")
	saveMediaPart(t, ctx, initial.client, 0x5ED24778, 0, livePartPayload)
	references, err := initial.store.BlobInventoryReferences(ctx)
	if err != nil {
		t.Fatalf("read database media references: %v", err)
	}
	var partKey string
	for _, reference := range references {
		if reference.Kind == "part" {
			partKey = reference.Key
		}
	}
	if partKey == "" {
		t.Fatalf("database references = %+v, want the live upload part", references)
	}
	fileKey := blob.Key(doc.ID)
	secondFileKey := blob.Key(secondDoc.ID)
	strayKey := blob.Key(999999)
	if _, err := remote.raw.Put(ctx, strayKey, bytes.NewReader([]byte("unreferenced stray"))); err != nil {
		t.Fatalf("seed unreferenced stray: %v", err)
	}
	if len(references) != 3 || !remote.has("teagramd/"+fileKey) || !remote.has("teagramd/"+secondFileKey) ||
		!remote.has("teagramd/"+partKey) || !remote.has("teagramd/"+strayKey) {
		t.Fatalf("fixture references=%+v; want two plaintext files, one live part, and a stray object", references)
	}

	initial.close(t)
	stateRoot := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(stateRoot, 0o700); err != nil {
		t.Fatalf("create authority fixture root: %v", err)
	}
	authorityDir := filepath.Join(stateRoot, "checkout", ".state", "blob-mode")
	reportDir := filepath.Join(stateRoot, "reports")
	if err := publishAcceptedS3Fixture(t, stateRoot, s3Config); err != nil {
		t.Fatalf("create accepted S3 authority fixture: %v", err)
	}
	if _, err := blobmode.Read(authorityDir, blobmode.EffectiveConfig{BlobS3: &s3Config}); err != nil {
		t.Fatalf("read accepted S3 authority fixture: %v", err)
	}
	progressPath := filepath.Join(t.TempDir(), "migration-progress.jsonl")
	mediaKeyDir := filepath.Join(t.TempDir(), "media-keys")
	if err := os.Mkdir(mediaKeyDir, 0o700); err != nil {
		t.Fatalf("create media key directory: %v", err)
	}
	mediaKey := bytes.Repeat([]byte{0x44}, blob.MediaKeyLen)
	if err := os.WriteFile(filepath.Join(mediaKeyDir, "4.key"), []byte(hex.EncodeToString(mediaKey)), 0o600); err != nil {
		t.Fatalf("write test media key: %v", err)
	}
	env := migrationCommandEnv(dsn, s3Config, mediaKeyDir)
	binary := buildMigrationCommand(t)
	remote.blockNextPut("teagramd/" + blob.EncryptedObjectsPrefix + "/" + partKey)
	// #nosec G204 -- The built command is repository code; all arguments name this test's private fixtures.
	command := osexec.CommandContext(ctx, binary,
		"--writes", "drained", "--progress", progressPath,
		"--authority-dir", authorityDir, "--report-dir", reportDir,
	)
	command.Env = env
	var firstStdout, firstStderr bytes.Buffer
	command.Stdout, command.Stderr = &firstStdout, &firstStderr
	if err := command.Start(); err != nil {
		t.Fatalf("start migration subprocess: %v", err)
	}
	childFinished := false
	t.Cleanup(func() {
		if !childFinished {
			killAndWaitMigration(t, command)
		}
	})
	select {
	case <-remote.blockedPut:
	case <-ctx.Done():
		killAndWaitMigration(t, command)
		childFinished = true
		t.Fatalf("migration did not reach the live-part write: %v\nstdout: %s\nstderr: %s", ctx.Err(), firstStdout.String(), firstStderr.String())
	case <-time.After(45 * time.Second):
		killAndWaitMigration(t, command)
		childFinished = true
		t.Fatalf("migration did not reach the live-part write\nstdout: %s\nstderr: %s", firstStdout.String(), firstStderr.String())
	}
	progress, err := os.ReadFile(progressPath)
	if err != nil {
		t.Fatalf("read fsynced migration progress after interruption: %v", err)
	}
	if !hasVerifiedProgress(progress, fileKey) || !hasVerifiedProgress(progress, secondFileKey) {
		t.Fatalf("progress before killing subprocess does not verify both files %q and %q: %s", fileKey, secondFileKey, progress)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("kill migration subprocess: %v", err)
	}
	waitErr := command.Wait()
	childFinished = true
	var exitErr *osexec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("migration subprocess wait = %v, want SIGKILL", waitErr)
	}
	waitStatus, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !waitStatus.Signaled() || waitStatus.Signal() != syscall.SIGKILL {
		t.Fatalf("migration subprocess status = %v, want SIGKILL", exitErr.ProcessState)
	}

	authority, err := blobmode.Read(authorityDir, blobmode.EffectiveConfig{BlobS3: &s3Config})
	if err != nil || authority.Outcome != blobmode.OutcomeS3Accepted {
		t.Fatalf("authority after killed migration = %+v, %v; want s3-accepted", authority, err)
	}
	firstTarget := "teagramd/" + blob.EncryptedObjectsPrefix + "/" + fileKey
	if got := remote.putCount(firstTarget); got != 1 {
		t.Fatalf("verified file envelope writes before retry = %d, want 1", got)
	}
	for _, key := range []string{fileKey, secondFileKey, partKey} {
		if !remote.has("teagramd/" + key) {
			t.Fatalf("interrupted migration removed plaintext object %q", key)
		}
	}
	cleanupArgs := []string{"--writes", "drained", "--progress", progressPath,
		"--authority-dir", authorityDir, "--report-dir", reportDir, "--cleanup"}
	if stdout, stderr, err := runMigrationCommand(ctx, binary, env, cleanupArgs...); err == nil ||
		!strings.Contains(stderr, "plaintext cleanup requires encrypted blob-mode authority") {
		t.Fatalf("cleanup before authority switch = %v, stdout=%s stderr=%s", err, stdout, stderr)
	}
	if !remote.has("teagramd/"+fileKey) || !remote.has("teagramd/"+secondFileKey) || !remote.has("teagramd/"+partKey) {
		t.Fatal("authority-gated cleanup removed plaintext before the encrypted switch")
	}

	remote.allowBlockedPuts()
	stdout, stderr, err := runMigrationCommand(ctx, binary, env,
		"--writes", "drained", "--progress", progressPath,
		"--authority-dir", authorityDir, "--report-dir", reportDir,
	)
	if err != nil {
		t.Fatalf("resume migration subprocess: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	var resumed struct {
		Authority       string                   `json:"authority"`
		Objects         int                      `json:"objects"`
		AlreadyVerified int                      `json:"already_verified"`
		Unaccounted     []blobinventory.ObjectID `json:"unaccounted"`
	}
	if err := json.Unmarshal([]byte(stdout), &resumed); err != nil {
		t.Fatalf("decode resumed migration output %q: %v", stdout, err)
	}
	if resumed.Authority != blobmode.OutcomeEncrypted || resumed.Objects != 3 || resumed.AlreadyVerified != 2 {
		t.Fatalf("resumed migration summary = %+v, want encrypted authority, three references, and two reused copies", resumed)
	}
	if !containsMigrationObject(resumed.Unaccounted, strayKey) {
		t.Fatalf("unaccounted inventory = %+v, want stray %q reported", resumed.Unaccounted, strayKey)
	}
	if got := remote.putCount(firstTarget); got != 1 {
		t.Fatalf("verified file envelope writes after resume = %d, want no rewrite", got)
	}
	secondTarget := "teagramd/" + blob.EncryptedObjectsPrefix + "/" + secondFileKey
	if got := remote.putCount(secondTarget); got != 1 {
		t.Fatalf("second verified file envelope writes after resume = %d, want no rewrite", got)
	}
	if authority, err := blobmode.Read(authorityDir, blobmode.EffectiveConfig{BlobS3: &s3Config}); err != nil || authority.Outcome != blobmode.OutcomeEncrypted {
		t.Fatalf("authority after completed migration = %+v, %v; want encrypted", authority, err)
	}

	stdout, stderr, err = runMigrationCommand(ctx, binary, env, cleanupArgs...)
	if err != nil {
		t.Fatalf("explicit plaintext cleanup: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	var cleanupSummary struct {
		Objects int `json:"objects"`
		Removed int `json:"plaintext_objects_removed"`
	}
	if err := json.Unmarshal([]byte(stdout), &cleanupSummary); err != nil || cleanupSummary.Objects != 3 || cleanupSummary.Removed != 3 {
		t.Fatalf("explicit cleanup summary = %+v, %v; want three referenced objects removed", cleanupSummary, err)
	}
	if remote.has("teagramd/"+fileKey) || remote.has("teagramd/"+secondFileKey) || remote.has("teagramd/"+partKey) {
		t.Fatal("explicit cleanup retained a referenced plaintext copy")
	}
	if !remote.has("teagramd/" + strayKey) {
		t.Fatal("explicit cleanup removed the unreferenced stray object")
	}

	keys, err := blob.NewKeyRing(4, map[uint32][]byte{4: mediaKey})
	if err != nil {
		t.Fatalf("create serving media key ring: %v", err)
	}
	encryptedStore, _, err := blob.NewS3Store(ctx, s3Config, keys, true)
	if err != nil {
		t.Fatalf("create encrypted serving store after restart: %v", err)
	}
	restarted := startMigrationMediaHarness(t, ctx, dsn, serverKey, phone, encryptedStore, false)
	got := downloadDocument(t, ctx, restarted.client, doc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("ranged download after encrypted serving restart returned %d bytes; want %d byte-identical bytes", len(got), len(payload))
	}
	secondGot := downloadDocument(t, ctx, restarted.client, secondDoc)
	if !bytes.Equal(secondGot, secondPayload) {
		t.Fatalf("second ranged download after restart returned %q; want %q", secondGot, secondPayload)
	}
}

func requireRootMigrationFixture(t *testing.T) bool {
	t.Helper()
	if os.Geteuid() == 0 {
		return true
	}
	sudo, err := osexec.LookPath("sudo")
	if err != nil {
		t.Skip("blob-mode publication integration requires the CI root test environment")
		return false
	}
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	// #nosec G204 G702 -- Re-run this exact test binary with the runner's container runtime and Go environment.
	args := append([]string{"-n", "env"}, rootMigrationEnvironment()...)
	args = append(args, testBinary, "-test.run=^TestBlobEncryptionMigrationKillResume$")
	command := osexec.CommandContext(t.Context(), sudo, args...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("root migration integration test failed: %v\n%s", err, output)
	}
	return false
}

func TestRootMigrationEnvironmentPreservesContainerSettings(t *testing.T) {
	t.Setenv("TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX", "mirror.gcr.io")
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")

	got := strings.Join(rootMigrationEnvironment(), "\n")
	for _, setting := range []string{
		"TESTCONTAINERS_HUB_IMAGE_NAME_PREFIX=mirror.gcr.io",
		"TESTCONTAINERS_RYUK_DISABLED=true",
		"DOCKER_HOST=unix:///var/run/docker.sock",
	} {
		if !strings.Contains(got, setting) {
			t.Errorf("root migration environment omitted %q: %q", setting, got)
		}
	}
}

func rootMigrationEnvironment() []string {
	var env []string
	for _, setting := range os.Environ() {
		key, _, _ := strings.Cut(setting, "=")
		if key == "PATH" || key == "DOCKER_HOST" || key == "DOCKER_CONTEXT" ||
			key == "DOCKER_TLS_VERIFY" || key == "DOCKER_CERT_PATH" || key == "DOCKER_CONFIG" ||
			strings.HasPrefix(key, "TESTCONTAINERS_") || key == "GOTOOLCHAIN" || key == "GOMODCACHE" {
			env = append(env, setting)
		}
	}
	return env
}

type migrationMediaHarness struct {
	client       *mediaClient
	store        *store.Store
	stopServer   func()
	cancelClient context.CancelFunc
	once         sync.Once
}

func startMigrationMediaHarness(
	t *testing.T,
	ctx context.Context,
	dsn string,
	serverKey *rsa.PrivateKey,
	phone string,
	blobs blob.Store,
	seedUser bool,
) *migrationMediaHarness {
	t.Helper()
	st, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open media database: %v", err)
	}
	listener := mustListen(t, ctx, "127.0.0.1:0")
	codes := newMultiCodeSink()
	_, stopServer := bootServerWithLimitsAndRegistrationModeAndBlobs(
		t, ctx, serverKey, 2, st, dsn, codes.Logger(), listener,
		config.DefaultRateLimits(), config.RegistrationClosed, blobs,
	)
	if seedUser {
		seedUsernameUsers(t, ctx, st, phone)
	}
	clientContext, cancelClient := context.WithCancel(ctx)
	mc := &mediaClient{cmds: make(chan command), coll: newUpdateCollector(), errCh: make(chan error, 1), store: st, dsn: dsn, blobs: blobs}
	client := createClient(tcpPort(t, listener), serverKey, 2, mc.coll, nil)
	idCh := make(chan int64, 1)
	go func() { mc.errCh <- runInteractive(clientContext, client, flowFor(phone, codes), idCh, mc.cmds) }()
	select {
	case mc.id = <-idCh:
	case <-ctx.Done():
		cancelClient()
		stopServer()
		if closeErr := st.Close(); closeErr != nil {
			t.Errorf("media store close after login timeout: %v", closeErr)
		}
		t.Fatalf("media client %s login timed out: %v", phone, ctx.Err())
	}
	harness := &migrationMediaHarness{client: mc, store: st, stopServer: stopServer, cancelClient: cancelClient}
	t.Cleanup(func() { harness.close(t) })
	return harness
}

func (h *migrationMediaHarness) close(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		h.cancelClient()
		if err := <-h.client.errCh; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("media client stop: %v", err)
		}
		h.stopServer()
		if err := h.store.Close(); err != nil {
			t.Errorf("media store close: %v", err)
		}
	})
}

func buildMigrationCommand(t *testing.T) string {
	t.Helper()
	repoRoot := migrationRepoRoot(t)
	binary := filepath.Join(t.TempDir(), "blob-encrypt-migrate")
	goBinary, err := osexec.LookPath("go")
	if err != nil {
		t.Fatalf("locate Go executable: %v", err)
	}
	// #nosec G204 -- Run the Go tool on the fixed command package into a private temporary path.
	command := osexec.CommandContext(t.Context(), goBinary, "build", "-o", binary, "./cmd/blob-encrypt-migrate")
	command.Dir = repoRoot
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build migration command: %v\n%s", err, output)
	}
	return binary
}

func publishAcceptedS3Fixture(t *testing.T, fixtureRoot string, s3Config blob.S3Config) error {
	t.Helper()
	repoRoot := migrationRepoRoot(t)
	// #nosec G204 -- Run the repository's fixed fixture script with private test paths and a loopback endpoint.
	command := osexec.CommandContext(t.Context(), "python3",
		filepath.Join(repoRoot, "internal/blobmigration/testdata/publish_s3_accepted_fixture.py"),
		repoRoot, fixtureRoot, s3Config.Endpoint, s3Config.Bucket,
	)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("fixture process: %w\n%s", err, output)
	}
	return nil
}

func migrationRepoRoot(t *testing.T) string {
	t.Helper()
	current, err := os.Getwd()
	if err != nil {
		t.Fatalf("get test working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("find repository root from test working directory")
		}
		current = parent
	}
}

func migrationCommandEnv(dsn string, s3Config blob.S3Config, mediaKeyDir string) []string {
	keys := pgtest.EncKey()
	values := map[string]string{
		"TG_POSTGRES_DSN":                dsn,
		"TG_AUTHKEY_ENC_KEY":             hex.EncodeToString(keys),
		"TG_BLOB_S3_ENDPOINT":            s3Config.Endpoint,
		"TG_BLOB_S3_BUCKET":              s3Config.Bucket,
		"TG_BLOB_S3_PREFIX":              s3Config.Prefix,
		"TG_BLOB_S3_REGION":              s3Config.Region,
		"TG_BLOB_S3_ACCESS_KEY_ID":       s3Config.AccessKeyID,
		"TG_BLOB_S3_SECRET_ACCESS_KEY":   s3Config.SecretAccessKey,
		"TG_BLOB_S3_ALLOW_INSECURE_HTTP": "true",
		"TG_MEDIA_KEY_DIR":               mediaKeyDir,
		"TG_MEDIA_KEY_CURRENT":           "4",
		"TG_LISTEN_ADDR":                 "127.0.0.1:2443",
	}
	env := make([]string, 0, len(os.Environ())+len(values))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "TG_") || strings.EqualFold(key, "HTTP_PROXY") ||
			strings.EqualFold(key, "HTTPS_PROXY") || strings.EqualFold(key, "ALL_PROXY") {
			continue
		}
		env = append(env, entry)
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func runMigrationCommand(ctx context.Context, binary string, env []string, args ...string) (string, string, error) {
	// #nosec G204 -- The binary is built by this test and arguments refer only to its private fixtures.
	command := osexec.CommandContext(ctx, binary, args...)
	command.Env = env
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func killAndWaitMigration(t *testing.T, command *osexec.Cmd) {
	t.Helper()
	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("kill migration subprocess: %v", err)
	}
	if err := command.Wait(); err != nil {
		if exitErr, ok := errors.AsType[*osexec.ExitError](err); !ok || exitErr.ProcessState == nil {
			t.Errorf("wait for killed migration subprocess: %v", err)
		}
	}
}

func hasVerifiedProgress(progress []byte, key string) bool {
	for line := range bytes.SplitSeq(progress, []byte{'\n'}) {
		var event struct {
			Key   string `json:"key"`
			State string `json:"state"`
		}
		if json.Unmarshal(line, &event) == nil && event.Key == key && event.State == "verified" {
			return true
		}
	}
	return false
}

func containsMigrationObject(objects []blobinventory.ObjectID, key string) bool {
	for _, object := range objects {
		if object.Key == key {
			return true
		}
	}
	return false
}

type migrationS3 struct {
	server *httptest.Server
	raw    *blob.S3

	mu         sync.Mutex
	objects    map[string][]byte
	puts       map[string]int
	blockedKey string
	blockPuts  bool
	blockedPut chan struct{}
}

type migrationS3Listing struct {
	XMLName     xml.Name            `xml:"ListBucketResult"`
	IsTruncated bool                `xml:"IsTruncated"`
	Contents    []migrationS3Object `xml:"Contents"`
}

type migrationS3Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

func newMigrationS3(t *testing.T) *migrationS3 {
	t.Helper()
	remote := &migrationS3{objects: make(map[string][]byte), puts: make(map[string]int), blockedPut: make(chan struct{}, 1)}
	remote.server = httptest.NewServer(http.HandlerFunc(remote.serveHTTP))
	t.Cleanup(remote.server.Close)
	var err error
	remote.raw, err = blob.NewS3(blob.S3Config{
		Endpoint: remote.server.URL, Bucket: migrationTestBucket, Prefix: "teagramd/",
		Region: "us-east-1", AccessKeyID: "test-access", SecretAccessKey: "test-secret",
		AllowInsecureHTTP: true, OperationTimeout: 20 * time.Second,
	})
	if err != nil {
		t.Fatalf("create raw test S3 client: %v", err)
	}
	return remote
}

func (s *migrationS3) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/"+migrationTestBucket+"/")
	if path == r.URL.Path || path == "" {
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			s.serveList(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.servePut(w, r, path)
	case http.MethodGet:
		s.serveGet(w, r, path)
	case http.MethodHead:
		s.mu.Lock()
		body, ok := s.objects[path]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	case http.MethodDelete:
		s.mu.Lock()
		_, ok := s.objects[path]
		delete(s.objects, path)
		s.mu.Unlock()
		if !ok {
			writeMigrationS3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *migrationS3) serveList(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	s.mu.Lock()
	listing := migrationS3Listing{}
	for key, body := range s.objects {
		if strings.HasPrefix(key, prefix) {
			listing.Contents = append(listing.Contents, migrationS3Object{
				Key: key, LastModified: time.Now().UTC().Format(time.RFC3339Nano), ETag: "\"fixture\"", Size: int64(len(body)),
			})
		}
	}
	s.mu.Unlock()
	sort.Slice(listing.Contents, func(i, j int) bool { return listing.Contents[i].Key < listing.Contents[j].Key })
	w.Header().Set("Content-Type", "application/xml")
	if err := xml.NewEncoder(w).Encode(listing); err != nil {
		http.Error(w, "encode listing", http.StatusInternalServerError)
	}
}

func (s *migrationS3) servePut(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	s.puts[key]++
	blocked := s.blockPuts && key == s.blockedKey
	s.mu.Unlock()
	if blocked {
		select {
		case s.blockedPut <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read object", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.objects[key] = body
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (s *migrationS3) serveGet(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	body, ok := s.objects[key]
	body = append([]byte(nil), body...)
	s.mu.Unlock()
	if !ok {
		writeMigrationS3Error(w, http.StatusNotFound, "NoSuchKey")
		return
	}
	window := r.Header.Get("Range")
	if window == "" {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			return
		}
		return
	}
	var start, end int
	if _, err := fmt.Sscanf(window, "bytes=%d-%d", &start, &end); err != nil || start < 0 || end < start {
		http.Error(w, "invalid range", http.StatusBadRequest)
		return
	}
	if start >= len(body) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if end >= len(body) {
		end = len(body) - 1
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	if _, err := w.Write(body[start : end+1]); err != nil {
		return
	}
}

func writeMigrationS3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	if _, err := fmt.Fprintf(w, "<Error><Code>%s</Code></Error>", code); err != nil {
		return
	}
}

func (s *migrationS3) blockNextPut(key string) {
	s.mu.Lock()
	s.blockedKey, s.blockPuts = key, true
	s.mu.Unlock()
}

func (s *migrationS3) allowBlockedPuts() {
	s.mu.Lock()
	s.blockPuts = false
	s.mu.Unlock()
}

func (s *migrationS3) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

func (s *migrationS3) putCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.puts[key]
}
