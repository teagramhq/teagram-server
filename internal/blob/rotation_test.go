package blob_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestRewrapOverS3RetriesWithKnownLengthAndKeepsChunks(t *testing.T) {
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
			attempts++
			attempt := attempts
			if attempt > 1 {
				stored = bytes.Clone(body)
			}
			mu.Unlock()
			if attempt == 1 {
				writeS3Error(w, http.StatusServiceUnavailable, "SlowDown")
				return
			}
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
	if finalAttempts != 2 {
		t.Fatalf("S3 PUT attempts = %d, want retry after transient failure", finalAttempts)
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
