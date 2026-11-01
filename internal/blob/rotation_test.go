package blob_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
)

func TestRewrapChangesOnlyTheEnvelopeHeaderAndKeepsRangeReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	key := "aa/1"
	want := pattern(2*blob.MinChunkSize + 137)
	oldRing := newRing(t, 1, 1)
	if _, err := newEncrypted(t, mem, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write version 1 object: %v", err)
	}
	oldObject := bytes.Clone(mem.raw(key))

	rotator := newEncrypted(t, mem, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare rewrap: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(oldObject))); err != nil {
		t.Fatalf("rewrap version 1 object: %v", err)
	}
	newObject := mem.raw(key)
	if len(oldObject) != len(newObject) || !bytes.Equal(oldObject[102:], newObject[102:]) {
		t.Fatal("rewrap changed ciphertext chunks instead of replacing only the header")
	}
	info, err := blob.InspectEnvelopeHeader(ctx, mem, newRing(t, 2, 1, 2), key, int64(len(newObject)))
	if err != nil || info.KeyVersion != 2 {
		t.Fatalf("rewrapped header = %+v, %v; want authenticated key version 2", info, err)
	}
	if got := hex.EncodeToString(info.WrapSalt[:]); got != plan.SaltID() {
		t.Fatalf("rewrapped salt = %s, want the persisted rotation salt %s", got, plan.SaltID())
	}

	got, err := rotator.ReadAt(ctx, key, 733, 4096)
	if err != nil || !bytes.Equal(got, want[733:733+4096]) {
		t.Fatalf("ranged read after rewrap differs from source: %v", err)
	}
	got, err = rotator.ReadAt(ctx, key, int64(len(want)-17), 100)
	if err != nil || !bytes.Equal(got, want[len(want)-17:]) {
		t.Fatalf("short ranged read after rewrap differs from source: %v", err)
	}
}

func TestRewriteUsesFreshDataKeyAndKeepsPlaintext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	key := "bb/2"
	want := pattern(2*blob.MinChunkSize + 91)
	oldRing := newRing(t, 1, 1)
	if _, err := newEncrypted(t, mem, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write version 1 object: %v", err)
	}
	oldObject := bytes.Clone(mem.raw(key))

	rotator := newEncrypted(t, mem, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, true)
	if err != nil {
		t.Fatalf("prepare rewrite: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(oldObject))); err != nil {
		t.Fatalf("rewrite version 1 object: %v", err)
	}
	newObject := mem.raw(key)
	if bytes.Equal(oldObject[102:], newObject[102:]) {
		t.Fatal("full rewrite reused the old encrypted chunk bytes")
	}
	got, err := rotator.ReadAt(ctx, key, 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("full read after rewrite differs from source: %v", err)
	}
	got, err = rotator.ReadAt(ctx, key, 101, 2048)
	if err != nil || !bytes.Equal(got, want[101:101+2048]) {
		t.Fatalf("ranged read after rewrite differs from source: %v", err)
	}
}

func TestRotateResumesAfterInterruptedLocalPut(t *testing.T) {
	if dir := os.Getenv("TEAGRAM_BLOB_ROTATION_CRASH_DIR"); dir != "" {
		runRotationPutCrashChild(t, dir)
		return
	}
	ctx := context.Background()
	local, dir := newLocal(t)
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Errorf("close local blob store: %v", err)
		}
	})
	key := blob.Key(81)
	want := pattern(blob.MinChunkSize + 37)
	oldRing := newRing(t, 1, 1)
	if _, err := newEncrypted(t, local, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write version 1 object: %v", err)
	}
	oldObject, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read original envelope: %v", err)
	}

	tempPath := filepath.Join(dir, filepath.FromSlash(key+blob.TempSuffix))
	killRotationPutChild(t, dir)
	tempInfo, err := os.Stat(tempPath)
	if err != nil || tempInfo.Size() == 0 {
		t.Fatalf("temporary file after SIGKILL = %+v, %v; want partial replacement bytes", tempInfo, err)
	}
	currentRing := newRing(t, 2, 1, 2)
	rotator := newEncrypted(t, local, currentRing, blob.MinChunkSize)
	got, err := rotator.ReadAt(ctx, key, 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("old object after interrupted Put = %v, %v; want original plaintext", got, err)
	}

	resumePlan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare resumed rotation: %v", err)
	}
	if err := rotator.Rotate(ctx, resumePlan, int64(len(oldObject))); err != nil {
		t.Fatalf("resume rotation over interrupted Put temp: %v", err)
	}
	newObject, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read rotated envelope: %v", err)
	}
	info, err := blob.InspectEnvelopeHeader(ctx, local, currentRing, key, int64(len(newObject)))
	if err != nil || info.KeyVersion != 2 {
		t.Fatalf("resumed rotation header = %+v, %v; want authenticated key version 2", info, err)
	}
	got, err = rotator.ReadAt(ctx, key, 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("plaintext after resumed rotation differs from original: %v", err)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("rotation temp after restart = %v, want not-exist", err)
	}
}

func killRotationPutChild(t *testing.T, dir string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("find current test executable: %v", err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestRotateResumesAfterInterruptedLocalPut$") //nolint:gosec // current test binary and fixed test filter
	cmd.Env = append(os.Environ(),
		"TEAGRAM_BLOB_ROTATION_CRASH_DIR="+dir,
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("open rotation child stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rotation child: %v", err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("rotation child readiness line = %q, want ready", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			waitErr := cmd.Wait()
			t.Fatalf("rotation child failed before Put pause: %v (exit: %v)", err, waitErr)
		}
	case <-time.After(10 * time.Second):
		killErr := cmd.Process.Kill()
		readerErr := <-ready
		waitErr := cmd.Wait()
		t.Fatalf("rotation child did not pause in Put: read=%v, kill=%v, exit=%v", readerErr, killErr, waitErr)
	}
	if err := cmd.Process.Kill(); err != nil {
		waitErr := cmd.Wait()
		t.Fatalf("kill rotation child in Put: %v (exit: %v)", err, waitErr)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("rotation child exited successfully after SIGKILL")
	}
}

func runRotationPutCrashChild(t *testing.T, dir string) {
	t.Helper()
	key := blob.Key(81)
	local, err := blob.OpenLocal(dir)
	if err != nil {
		t.Fatalf("open child blob store: %v", err)
	}
	defer func() {
		if err := local.Close(); err != nil {
			t.Errorf("close child blob store: %v", err)
		}
	}()
	var physicalBytes int64
	if err := local.Walk(context.Background(), func(entry blob.Entry) error {
		if entry.Key == key {
			physicalBytes = entry.Size
		}
		return nil
	}); err != nil {
		t.Fatalf("walk source envelope: %v", err)
	}
	if physicalBytes == 0 {
		t.Fatal("source envelope missing")
	}
	backend := &pausingRotationPutStore{Local: local, key: key, ready: os.Stdout}
	rotator := newEncrypted(t, backend, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare child rotation: %v", err)
	}
	if err := rotator.Rotate(context.Background(), plan, physicalBytes); err == nil {
		t.Fatal("child rotation unexpectedly completed")
	}
}

type pausingRotationPutStore struct {
	*blob.Local

	key   string
	ready io.Writer
}

func (s *pausingRotationPutStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if key != s.key {
		return s.Local.Put(ctx, key, r)
	}
	return s.Local.Put(ctx, key, &pauseAfterFirstRead{source: r, ready: s.ready})
}

type pauseAfterFirstRead struct {
	source   io.Reader
	ready    io.Writer
	readOnce bool
}

func (r *pauseAfterFirstRead) Read(p []byte) (int, error) {
	if r.readOnce {
		if _, err := io.WriteString(r.ready, "ready\n"); err != nil {
			return 0, err
		}
		select {}
	}
	n, err := r.source.Read(p)
	if n > 0 {
		r.readOnce = true
	}
	return n, err
}

func TestRotationPlanCannotReuseWrappingSaltForChangedObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	key := "cc/3"
	want := pattern(blob.MinChunkSize + 29)
	oldRing := newRing(t, 1, 1)
	oldWriter := newEncrypted(t, mem, oldRing, blob.MinChunkSize)
	if _, err := oldWriter.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write source object: %v", err)
	}
	rotator := newEncrypted(t, mem, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, true)
	if err != nil {
		t.Fatalf("prepare rewrite: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(mem.raw(key)))); err != nil {
		t.Fatalf("first rewrite: %v", err)
	}
	if _, err := oldWriter.Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("replace with a new old-version object: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(mem.raw(key)))); err == nil || !strings.Contains(err.Error(), "rotation plan has already been used") {
		t.Fatalf("reused rotation plan error = %v, want plan reuse rejected", err)
	}
}

func TestPartRotationBuffersBoundedEnvelopeBeforePut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	backend := &rotationCaptureStore{memStore: mem}
	key, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("draw part key: %v", err)
	}
	want := pattern(blob.MaxPartBytes)
	oldRing := newRing(t, 1, 1)
	if _, err := newEncrypted(t, backend, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write part envelope: %v", err)
	}
	oldObject := bytes.Clone(mem.raw(key))

	rotator := newEncrypted(t, backend, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare part rewrap: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(oldObject))); err != nil {
		t.Fatalf("rewrap part envelope: %v", err)
	}
	if _, ok := backend.lastPartReader.(*bytes.Reader); !ok {
		t.Fatalf("part rotation Put reader = %T, want bounded in-memory reader", backend.lastPartReader)
	}
	if got := mem.raw(key); len(got) != len(oldObject) || !bytes.Equal(got[102:], oldObject[102:]) {
		t.Fatal("part rewrap changed ciphertext chunks")
	}
}

func TestPartRotationRejectsOversizedPlaintext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	backend := &rotationCaptureStore{memStore: mem}
	key, err := blob.NewPartKey()
	if err != nil {
		t.Fatalf("draw part key: %v", err)
	}
	oldRing := newRing(t, 1, 1)
	tooLarge := pattern(blob.MaxPartBytes + 1)
	if _, err := newEncrypted(t, backend, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(tooLarge)); err != nil {
		t.Fatalf("write oversized test envelope: %v", err)
	}
	oldObject := bytes.Clone(mem.raw(key))
	rotator := newEncrypted(t, backend, newRing(t, 2, 1, 2), blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare oversized part rewrap: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(oldObject))); err == nil {
		t.Fatal("oversized part envelope was rotated")
	}
}

type rotationCaptureStore struct {
	*memStore

	lastPartReader io.Reader
}

func (s *rotationCaptureStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if blob.ParsePartKey(key) {
		s.lastPartReader = r
	}
	return s.memStore.Put(ctx, key, r)
}

func TestRewrapOverS3StreamsKnownLengthAndKeepsChunks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := "dd/4"
	want := pattern(3*blob.MinChunkSize + 11)
	oldRing := newRing(t, 1, 1)
	source := newMemStore()
	if _, err := newEncrypted(t, source, oldRing, blob.MinChunkSize).Put(ctx, key, bytes.NewReader(want)); err != nil {
		t.Fatalf("write source envelope: %v", err)
	}
	oldObject := bytes.Clone(source.raw(key))

	var mu sync.Mutex
	stored := bytes.Clone(oldObject)
	var requests, attempts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			value := r.Header.Get("Range")
			if !strings.HasPrefix(value, "bytes=") {
				t.Errorf("GET Range = %q, want a byte range", value)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			bounds := strings.TrimPrefix(value, "bytes=")
			startText, endText, ok := strings.Cut(bounds, "-")
			if !ok {
				t.Errorf("GET Range = %q, want start-end", value)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			start, startErr := strconv.ParseInt(startText, 10, 64)
			end, endErr := strconv.ParseInt(endText, 10, 64)
			if startErr != nil || endErr != nil || start < 0 || end < start {
				t.Errorf("invalid GET range %q", value)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			body := bytes.Clone(stored)
			mu.Unlock()
			end = min(end, int64(len(body))-1)
			if start >= int64(len(body)) || end < start {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			w.WriteHeader(http.StatusPartialContent)
			if _, err := w.Write(body[start : end+1]); err != nil {
				t.Errorf("write range response: %v", err)
			}
		case http.MethodPut:
			mu.Lock()
			attempts++
			mu.Unlock()
			if r.ContentLength != int64(len(oldObject)) {
				t.Errorf("PUT Content-Length = %d, want %d", r.ContentLength, len(oldObject))
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read PUT body: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			mu.Lock()
			stored = bytes.Clone(body)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected S3 method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	remote, err := blob.NewS3(blob.S3Config{
		Endpoint: server.URL, Bucket: "rotation-test", AccessKeyID: "access", SecretAccessKey: "secret",
		AllowInsecureHTTP: true, MaxAttempts: 2, OperationTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("create S3 store: %v", err)
	}
	currentRing := newRing(t, 2, 1, 2)
	rotator := newEncrypted(t, remote, currentRing, blob.MinChunkSize)
	plan, err := rotator.PrepareRotation(key, false)
	if err != nil {
		t.Fatalf("prepare S3 rewrap: %v", err)
	}
	if err := rotator.Rotate(ctx, plan, int64(len(oldObject))); err != nil {
		t.Fatalf("rewrap S3 object: %v", err)
	}
	mu.Lock()
	finalObject := bytes.Clone(stored)
	finalAttempts, finalRequests := attempts, requests
	mu.Unlock()
	if finalAttempts != 1 {
		t.Fatalf("S3 PUT attempts = %d, want one successful request", finalAttempts)
	}
	if finalRequests == 0 || len(finalObject) != len(oldObject) || !bytes.Equal(finalObject[102:], oldObject[102:]) {
		t.Fatal("S3 rewrap did not preserve the chunk bytes")
	}
	info, err := blob.InspectEnvelopeHeader(ctx, remote, currentRing, key, int64(len(finalObject)))
	if err != nil || info.KeyVersion != 2 {
		t.Fatalf("S3 rotated header = %+v, %v; want authenticated key version 2", info, err)
	}
	got, err := rotator.ReadAt(ctx, key, 297, 4096)
	if err != nil || !bytes.Equal(got, want[297:297+4096]) {
		t.Fatalf("S3 range read after rewrap differs from source: %v", err)
	}
}
