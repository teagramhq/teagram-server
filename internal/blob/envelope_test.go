package blob_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/blob"
)

const envelopeHeaderLen = 102

// memStore is an in-memory backend that counts the backend bytes ReadAt hands
// back, so a test can assert what a window fetched.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	reads   []readCall
}

type readCall struct{ offset, limit, got int64 }

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Put(_ context.Context, key string, r io.Reader) (int64, error) {
	if err := blob.ValidateKey(key); err != nil {
		return 0, err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return int64(len(b)), nil
}

func (m *memStore) ReadAt(_ context.Context, key string, offset, limit int64) ([]byte, error) {
	if err := blob.ValidateKey(key); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, blob.ErrNotFound
	}
	lo := min(offset, int64(len(b)))
	hi := min(lo+limit, int64(len(b)))
	m.reads = append(m.reads, readCall{offset, limit, hi - lo})
	return bytes.Clone(b[lo:hi]), nil
}

func (m *memStore) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *memStore) WalkPrefix(context.Context, string, func(blob.Entry) error) error { return nil }

func (m *memStore) raw(key string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[key]
}

func (m *memStore) set(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
}

func (m *memStore) takeReads() []readCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.reads
	m.reads = nil
	return r
}

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, blob.MediaKeyLen) }

func versionKey(v uint32) []byte {
	key := testKey(0x5a)
	binary.BigEndian.PutUint32(key, v)
	return key
}

func newRing(t *testing.T, current uint32, versions ...uint32) *blob.KeyRing {
	t.Helper()
	keys := map[uint32][]byte{}
	for _, v := range versions {
		keys[v] = versionKey(v)
	}
	ring, err := blob.NewKeyRing(current, keys)
	if err != nil {
		t.Fatalf("NewKeyRing: %v", err)
	}
	return ring
}

func newEncrypted(t *testing.T, backend blob.Store, ring *blob.KeyRing, chunk int) *blob.Encrypted {
	t.Helper()
	e, err := blob.NewEncrypted(backend, ring, chunk)
	if err != nil {
		t.Fatalf("NewEncrypted: %v", err)
	}
	return e
}

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i>>8)
	}
	return b
}

var patternTable = func() (t [256]byte) {
	for i := range t {
		t[i] = byte(i*151 + 7)
	}
	return t
}()

// patternReader yields a deterministic stream of n bytes without holding it,
// and declares its length the way an assembled upload does.
type patternReader struct{ n, pos int64 }

func (p *patternReader) Size() int64 { return p.n }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.pos >= p.n {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.n-p.pos)]
	for i := range b {
		x := p.pos + int64(i)
		b[i] = patternTable[(x*31^x>>9)&0xff]
	}
	p.pos += int64(len(b))
	return len(b), nil
}

func TestEncryptedLargeStreamOverLocal(t *testing.T) {
	const size = 100 << 20
	local, _ := newLocal(t)
	e := newEncrypted(t, local, newRing(t, 1, 1), blob.DefaultChunkSize)
	ctx := context.Background()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	n, err := e.Put(ctx, "parts/large", &patternReader{n: size})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if n != size {
		t.Fatalf("Put = %d, want %d", n, size)
	}
	// Total allocation bounds peak allocation. A Put that buffered the object
	// would allocate at least its size; a streaming one allocates a few chunks.
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16*blob.DefaultChunkSize {
		t.Fatalf("Put allocated %d bytes for a %d-byte object, want at most %d", alloc, size, 16*blob.DefaultChunkSize)
	}

	want := sha256.New()
	if _, err := io.Copy(want, &patternReader{n: size}); err != nil {
		t.Fatal(err)
	}
	got := sha256.New()
	const window = 1 << 20
	for off := int64(0); off < size; off += window {
		b, err := e.ReadAt(ctx, "parts/large", off, window)
		if err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if len(b) != window {
			t.Fatalf("ReadAt(%d) = %d bytes, want %d", off, len(b), window)
		}
		got.Write(b)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Fatal("100 MiB read back differs from what was written")
	}
}

func TestEncryptedRoundTripSizes(t *testing.T) {
	t.Parallel()
	const chunk = blob.MinChunkSize
	for _, size := range []int{0, 1, chunk - 1, chunk, chunk + 1, 3 * chunk, 3*chunk + 17} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			t.Parallel()
			mem := newMemStore()
			e := newEncrypted(t, mem, newRing(t, 1, 1), chunk)
			want := pattern(size)
			n, err := e.Put(context.Background(), "aa/1", bytes.NewReader(want))
			if err != nil || n != int64(size) {
				t.Fatalf("Put = %d, %v; want %d", n, err, size)
			}
			chunks := max(1, (size+chunk-1)/chunk)
			if got, wantLen := len(mem.raw("aa/1")), envelopeHeaderLen+size+16*chunks; got != wantLen {
				t.Fatalf("stored %d bytes, want %d", got, wantLen)
			}
			got, err := e.ReadAt(context.Background(), "aa/1", 0, int64(size)+100)
			if err != nil {
				t.Fatalf("ReadAt: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("read back %d bytes, differs from %d written", len(got), size)
			}
		})
	}
}

func TestEncryptedReadAtFetchesOnlyOverlappedChunks(t *testing.T) {
	t.Parallel()
	const chunk = 64 << 10
	const size = 10*chunk + 1234
	mem := newMemStore()
	e := newEncrypted(t, mem, newRing(t, 1, 1), chunk)
	want := pattern(size)
	ctx := context.Background()
	if _, err := e.Put(ctx, "aa/1", bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}

	for _, offset := range []int64{0, 5, chunk - 100, 3*chunk + 777, 7*chunk - 2048, size - 4096} {
		mem.takeReads()
		got, err := e.ReadAt(ctx, "aa/1", offset, 4096)
		if err != nil {
			t.Fatalf("ReadAt(%d): %v", offset, err)
		}
		if !bytes.Equal(got, want[offset:offset+4096]) {
			t.Fatalf("ReadAt(%d) returned wrong bytes", offset)
		}
		overlapped := (offset+4095)/chunk - offset/chunk + 1
		budget := envelopeHeaderLen + overlapped*(chunk+16) + 1
		var fetched int64
		for _, r := range mem.takeReads() {
			fetched += r.got
		}
		if fetched > budget {
			t.Fatalf("ReadAt(%d) fetched %d backend bytes, want at most %d", offset, fetched, budget)
		}
	}

	got, err := e.ReadAt(ctx, "aa/1", size-10, 4096)
	if err != nil || !bytes.Equal(got, want[size-10:]) {
		t.Fatalf("short read at end = %d bytes, %v; want the last 10", len(got), err)
	}
	got, err = e.ReadAt(ctx, "aa/1", size+5, 4096)
	if err != nil || len(got) != 0 {
		t.Fatalf("read past end = %d bytes, %v; want empty success", len(got), err)
	}
	mem.takeReads()
	got, err = e.ReadAt(ctx, "aa/1", 1234, 0)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("zero window = %v, %v; want empty success", got, err)
	}
	if reads := mem.takeReads(); len(reads) != 0 {
		t.Fatalf("zero window made %d backend reads, want 0", len(reads))
	}
	if _, err := e.ReadAt(ctx, "aa/1", -1, 10); err == nil {
		t.Fatal("negative offset accepted")
	}
	if _, err := e.ReadAt(ctx, "aa/1", 0, -1); err == nil {
		t.Fatal("negative limit accepted")
	}
}

func TestEncryptedFailsClosed(t *testing.T) {
	t.Parallel()
	const chunk = blob.MinChunkSize
	const size = 4*chunk + 100
	ctx := context.Background()
	ring := newRing(t, 1, 1)
	sealed := func(t *testing.T) (*memStore, *blob.Encrypted, []byte) {
		t.Helper()
		mem := newMemStore()
		e := newEncrypted(t, mem, ring, chunk)
		if _, err := e.Put(ctx, "aa/1", bytes.NewReader(pattern(size))); err != nil {
			t.Fatal(err)
		}
		return mem, e, bytes.Clone(mem.raw("aa/1"))
	}
	chunkAt := func(i int) (int, int) {
		start := envelopeHeaderLen + i*(chunk+16)
		return start, min(start+chunk+16, envelopeHeaderLen+size+5*16)
	}

	cases := map[string]func(mem *memStore, raw []byte) (key string){
		"copied to another key": func(mem *memStore, raw []byte) string {
			mem.set("aa/2", raw)
			return "aa/2"
		},
		"chunks swapped": func(mem *memStore, raw []byte) string {
			a0, a1 := chunkAt(1)
			b0, b1 := chunkAt(2)
			out := bytes.Clone(raw)
			copy(out[a0:a1], raw[b0:b1])
			copy(out[b0:b1], raw[a0:a1])
			mem.set("aa/1", out)
			return "aa/1"
		},
		"chunk dropped": func(mem *memStore, raw []byte) string {
			a0, a1 := chunkAt(1)
			mem.set("aa/1", append(bytes.Clone(raw[:a0]), raw[a1:]...))
			return "aa/1"
		},
		"truncated": func(mem *memStore, raw []byte) string {
			mem.set("aa/1", raw[:len(raw)-1])
			return "aa/1"
		},
		"data appended": func(mem *memStore, raw []byte) string {
			mem.set("aa/1", append(bytes.Clone(raw), 0))
			return "aa/1"
		},
		"header shorter than fixed size": func(mem *memStore, raw []byte) string {
			mem.set("aa/1", raw[:envelopeHeaderLen-1])
			return "aa/1"
		},
		"unknown format version": func(mem *memStore, raw []byte) string {
			out := bytes.Clone(raw)
			binary.BigEndian.PutUint16(out[4:6], 2)
			mem.set("aa/1", out)
			return "aa/1"
		},
		"unknown key version": func(mem *memStore, raw []byte) string {
			out := bytes.Clone(raw)
			binary.BigEndian.PutUint32(out[6:10], 9)
			mem.set("aa/1", out)
			return "aa/1"
		},
		"plaintext object": func(mem *memStore, raw []byte) string {
			mem.set("aa/1", pattern(size))
			return "aa/1"
		},
		"empty object": func(mem *memStore, raw []byte) string {
			mem.set("aa/1", []byte{})
			return "aa/1"
		},
	}
	for bit := range envelopeHeaderLen * 8 {
		cases[fmt.Sprintf("header bit %d flipped", bit)] = func(mem *memStore, raw []byte) string {
			out := bytes.Clone(raw)
			out[bit/8] ^= 1 << (bit % 8)
			mem.set("aa/1", out)
			return "aa/1"
		}
	}
	for name, tamper := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mem, e, raw := sealed(t)
			key := tamper(mem, raw)
			_, err := e.ReadAt(ctx, key, 0, size)
			if !errors.Is(err, blob.ErrEnvelope) {
				t.Fatalf("ReadAt = %v, want ErrEnvelope", err)
			}
			if errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("ReadAt = %v, must not be ErrNotFound", err)
			}
		})
	}

	t.Run("missing object stays not found", func(t *testing.T) {
		t.Parallel()
		_, e, _ := sealed(t)
		if _, err := e.ReadAt(ctx, "aa/404", 0, 10); !errors.Is(err, blob.ErrNotFound) {
			t.Fatalf("ReadAt = %v, want ErrNotFound", err)
		}
	})
	t.Run("damaged chunk fails windows touching it", func(t *testing.T) {
		t.Parallel()
		mem, e, raw := sealed(t)
		a0, _ := chunkAt(2)
		raw[a0+5] ^= 1
		mem.set("aa/1", raw)
		if _, err := e.ReadAt(ctx, "aa/1", 2*chunk+10, 10); !errors.Is(err, blob.ErrEnvelope) {
			t.Fatalf("window over damaged chunk = %v, want ErrEnvelope", err)
		}
		if _, err := e.ReadAt(ctx, "aa/1", 0, 10); err != nil {
			t.Fatalf("window over intact chunk = %v, want success", err)
		}
	})
}

// TestEncryptedEmptyObjectFailsClosed checks that reading an empty object
// still authenticates its one tag-only chunk, since no window ever overlaps it.
func TestEncryptedEmptyObjectFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealed := func(t *testing.T) (*memStore, *blob.Encrypted, []byte) {
		t.Helper()
		mem := newMemStore()
		e := newEncrypted(t, mem, newRing(t, 1, 1), blob.MinChunkSize)
		if _, err := e.Put(ctx, "aa/1", bytes.NewReader(nil)); err != nil {
			t.Fatal(err)
		}
		return mem, e, bytes.Clone(mem.raw("aa/1"))
	}

	t.Run("intact", func(t *testing.T) {
		t.Parallel()
		_, e, _ := sealed(t)
		for _, w := range [][2]int64{{0, 1}, {0, 4096}, {7, 1}} {
			got, err := e.ReadAt(ctx, "aa/1", w[0], w[1])
			if err != nil || got == nil || len(got) != 0 {
				t.Fatalf("ReadAt(%d, %d) = %v, %v; want empty success", w[0], w[1], got, err)
			}
		}
	})
	t.Run("zero window", func(t *testing.T) {
		t.Parallel()
		mem, e, raw := sealed(t)
		mem.set("aa/1", raw[:envelopeHeaderLen])
		got, err := e.ReadAt(ctx, "aa/1", 0, 0)
		if err != nil || got == nil || len(got) != 0 {
			t.Fatalf("ReadAt(0, 0) = %v, %v; want empty success", got, err)
		}
	})
	mutations := map[string]func(raw []byte) []byte{
		"truncated to header": func(raw []byte) []byte { return raw[:envelopeHeaderLen] },
		"tag truncated":       func(raw []byte) []byte { return raw[:len(raw)-1] },
		"tag corrupted": func(raw []byte) []byte {
			raw[len(raw)-1] ^= 1
			return raw
		},
		"data appended": func(raw []byte) []byte { return append(raw, 0) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mem, e, raw := sealed(t)
			mem.set("aa/1", mutate(raw))
			for _, w := range [][2]int64{{0, 1}, {5, 10}} {
				if _, err := e.ReadAt(ctx, "aa/1", w[0], w[1]); !errors.Is(err, blob.ErrEnvelope) {
					t.Fatalf("ReadAt(%d, %d) = %v, want ErrEnvelope", w[0], w[1], err)
				}
			}
		})
	}
}

func TestEncryptedKeyRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	want := pattern(3*blob.MinChunkSize + 9)
	old := newEncrypted(t, mem, newRing(t, 1, 1), blob.MinChunkSize)
	if _, err := old.Put(ctx, "aa/1", bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}

	rotated := newEncrypted(t, mem, newRing(t, 2, 1, 2), 2*blob.MinChunkSize)
	got, err := rotated.ReadAt(ctx, "aa/1", 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read after rotation = %v; want the original bytes", err)
	}
	if _, err := rotated.Put(ctx, "aa/2", bytes.NewReader(want)); err != nil {
		t.Fatal(err)
	}
	if v := binary.BigEndian.Uint32(mem.raw("aa/2")[6:10]); v != 2 {
		t.Fatalf("new write under key version %d, want 2", v)
	}

	retired := newEncrypted(t, mem, newRing(t, 2, 2), blob.MinChunkSize)
	if _, err := retired.ReadAt(ctx, "aa/1", 0, 10); !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("read after version 1 removed = %v, want ErrEnvelope", err)
	}
	if got, err := retired.ReadAt(ctx, "aa/2", 0, int64(len(want))); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("read of version 2 object = %v, want the original bytes", err)
	}
}

func TestEncryptedWrongKeySameVersionFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	writer := newEncrypted(t, mem, newRing(t, 1, 1), blob.MinChunkSize)
	if _, err := writer.Put(ctx, "aa/1", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	other, err := blob.NewKeyRing(1, map[uint32][]byte{1: testKey(0xee)})
	if err != nil {
		t.Fatal(err)
	}
	reader := newEncrypted(t, mem, other, blob.MinChunkSize)
	if _, err := reader.ReadAt(ctx, "aa/1", 0, 10); !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("ReadAt under another key = %v, want ErrEnvelope", err)
	}
}

func TestEncryptedEachPutUsesFreshDataKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	e := newEncrypted(t, mem, newRing(t, 1, 1), blob.MinChunkSize)
	if _, err := e.Put(ctx, "aa/1", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	first := bytes.Clone(mem.raw("aa/1"))
	if _, err := e.Put(ctx, "aa/1", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	second := mem.raw("aa/1")
	if bytes.Equal(first[22:envelopeHeaderLen], second[22:envelopeHeaderLen]) ||
		bytes.Equal(first[envelopeHeaderLen:], second[envelopeHeaderLen:]) {
		t.Fatal("two writes of the same bytes to the same key share salt, data key or ciphertext")
	}
}

type sizedReader struct {
	r    io.Reader
	size int64
}

func (s sizedReader) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s sizedReader) Size() int64                { return s.size }

func TestEncryptedPutRejectsBadSources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]io.Reader{
		"unsized":  io.MultiReader(strings.NewReader(payload)),
		"shorter":  sizedReader{strings.NewReader(payload), int64(len(payload)) + 1},
		"longer":   sizedReader{strings.NewReader(payload), int64(len(payload)) - 1},
		"negative": sizedReader{strings.NewReader(payload), -1},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local, _ := newLocal(t)
			e := newEncrypted(t, local, newRing(t, 1, 1), blob.MinChunkSize)
			if _, err := e.Put(ctx, "aa/1", src); err == nil {
				t.Fatal("Put accepted a source that does not match its declared length")
			}
			if _, err := e.ReadAt(ctx, "aa/1", 0, 1); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("failed Put left an object behind: %v", err)
			}
		})
	}
}

func TestEncryptedValidatesKeysBeforeBackend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mem := newMemStore()
	e := newEncrypted(t, mem, newRing(t, 1, 1), blob.MinChunkSize)
	for _, key := range []string{"", "../x", "a//b", "a/./b", "bad key"} {
		if _, err := e.Put(ctx, key, strings.NewReader(payload)); !errors.Is(err, blob.ErrInvalidKey) {
			t.Fatalf("Put(%q) = %v, want ErrInvalidKey", key, err)
		}
		if _, err := e.ReadAt(ctx, key, 0, 1); !errors.Is(err, blob.ErrInvalidKey) {
			t.Fatalf("ReadAt(%q) = %v, want ErrInvalidKey", key, err)
		}
		if err := e.Remove(ctx, key); !errors.Is(err, blob.ErrInvalidKey) {
			t.Fatalf("Remove(%q) = %v, want ErrInvalidKey", key, err)
		}
	}
	if reads := mem.takeReads(); len(reads) != 0 {
		t.Fatalf("invalid keys reached the backend %d times", len(reads))
	}
}

func TestEncryptedRemove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, _ := newLocal(t)
	e := newEncrypted(t, local, newRing(t, 1, 1), blob.MinChunkSize)
	if _, err := e.Put(ctx, "aa/1", strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, "aa/1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Remove(ctx, "aa/1"); err != nil {
		t.Fatalf("second Remove = %v, want no-op", err)
	}
	if _, err := e.ReadAt(ctx, "aa/1", 0, 1); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("ReadAt after Remove = %v, want ErrNotFound", err)
	}
}

// TestEncryptedOverS3SendsKnownLength puts through the decorator over S3,
// failing the first attempt, and checks every attempt carried the sealed
// length, the retry was sealed under a fresh data key, and the stored body
// reads back.
func TestEncryptedOverS3SendsKnownLength(t *testing.T) {
	t.Parallel()
	const chunk = blob.MinChunkSize
	want := pattern(5*chunk + 3)
	sealedLen := int64(envelopeHeaderLen + len(want) + 6*16)
	var mu sync.Mutex
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != sealedLen {
			t.Errorf("Content-Length = %d, want %d", r.ContentLength, sealedLen)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mu.Unlock()
		if attempt == 1 {
			writeS3Error(w, http.StatusServiceUnavailable, "SlowDown")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	s3, err := blob.NewS3(blob.S3Config{
		Endpoint:          server.URL,
		Bucket:            "test-bucket",
		AccessKeyID:       "access",
		SecretAccessKey:   "secret",
		AllowInsecureHTTP: true,
		MaxAttempts:       2,
		OperationTimeout:  5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ring := newRing(t, 1, 1)
	n, err := newEncrypted(t, s3, ring, chunk).Put(context.Background(), "aa/1", bytes.NewReader(want))
	if err != nil || n != int64(len(want)) {
		t.Fatalf("Put = %d, %v; want %d", n, err, len(want))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d attempts, want 2", len(bodies))
	}
	if bytes.Equal(bodies[0][22:envelopeHeaderLen], bodies[1][22:envelopeHeaderLen]) {
		t.Fatal("retry reused the first attempt's salt and data key")
	}
	mem := newMemStore()
	mem.set("aa/1", bodies[1])
	got, err := newEncrypted(t, mem, ring, chunk).ReadAt(context.Background(), "aa/1", 0, int64(len(want)))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("stored S3 body does not read back: %v", err)
	}
}

// TestEncryptedOverS3RejectsOverlongSource checks that a source holding more
// than it declared fails the Put even though the transport stops reading at
// Content-Length and never asks for the byte that gives the source away.
func TestEncryptedOverS3RejectsOverlongSource(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	stored := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		stored = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	s3, err := blob.NewS3(blob.S3Config{
		Endpoint: server.URL, Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s",
		AllowInsecureHTTP: true, OperationTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	e := newEncrypted(t, s3, newRing(t, 1, 1), blob.MinChunkSize)
	for _, size := range []int{0, 10, blob.MinChunkSize, 3*blob.MinChunkSize + 5} {
		src := sizedReader{bytes.NewReader(pattern(size + 1)), int64(size)}
		if _, err := e.Put(context.Background(), "aa/1", src); err == nil {
			t.Fatalf("Put of a source declaring %d bytes and holding %d succeeded", size, size+1)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if stored {
		t.Fatal("the server received a complete body for an overlong source")
	}
}

func TestEncryptedOverS3RefusesUnsizedSource(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unsized Put reached the network")
	}))
	t.Cleanup(server.Close)
	s3, err := blob.NewS3(blob.S3Config{
		Endpoint: server.URL, Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s",
		AllowInsecureHTTP: true, OperationTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		if err := pw.Close(); err != nil {
			t.Error(err)
		}
	})
	e := newEncrypted(t, s3, newRing(t, 1, 1), blob.MinChunkSize)
	if _, err := e.Put(context.Background(), "aa/1", pr); err == nil {
		t.Fatal("Put accepted an unsized source")
	}
}

func TestEncryptedReadsObjectsWrittenWithDifferentChunkSizes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	local, _ := newLocal(t)
	ring := newRing(t, 1, 1)
	reader := newEncrypted(t, local, ring, blob.DefaultChunkSize)

	writerChunks := []int{64 << 10}
	if blob.DefaultChunkSize == writerChunks[0] {
		writerChunks = append(writerChunks, 16<<10)
	}
	for i, chunk := range writerChunks {
		key := fmt.Sprintf("chunk-change/%d", i)
		want := pattern(2*chunk + 113)
		writer := newEncrypted(t, local, ring, chunk)
		if _, err := writer.Put(ctx, key, bytes.NewReader(want)); err != nil {
			t.Fatalf("write with %d-byte chunks: %v", chunk, err)
		}
		got, err := reader.ReadAt(ctx, key, 0, int64(len(want)))
		if err != nil {
			t.Fatalf("read %d-byte chunks with %d-byte default: %v", chunk, blob.DefaultChunkSize, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("read %d-byte chunks with %d-byte default returned different bytes", chunk, blob.DefaultChunkSize)
		}
	}
}

func TestNewEncryptedRejectsChunkSize(t *testing.T) {
	t.Parallel()
	ring := newRing(t, 1, 1)
	for _, size := range []int{0, blob.MinChunkSize - 1, blob.MaxChunkSize + 1} {
		if _, err := blob.NewEncrypted(newMemStore(), ring, size); err == nil {
			t.Fatalf("chunk size %d accepted", size)
		}
	}
}

// TestNewEncryptedRejectsZeroRing checks that a ring not built by NewKeyRing
// is refused, so no write can wrap a data key under an empty secret.
func TestNewEncryptedRejectsZeroRing(t *testing.T) {
	t.Parallel()
	for name, ring := range map[string]*blob.KeyRing{"literal": {}, "new": new(blob.KeyRing)} {
		if _, err := blob.NewEncrypted(newMemStore(), ring, blob.MinChunkSize); err == nil {
			t.Errorf("%s zero-value ring accepted", name)
		}
	}
}

func TestNewKeyRingRejects(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		current uint32
		keys    map[uint32][]byte
	}{
		"empty":           {1, map[uint32][]byte{}},
		"version zero":    {0, map[uint32][]byte{0: testKey(1)}},
		"short key":       {1, map[uint32][]byte{1: testKey(1)[:31]}},
		"long key":        {1, map[uint32][]byte{1: append(testKey(1), 0)}},
		"current missing": {2, map[uint32][]byte{1: testKey(1)}},
		"duplicate bytes": {1, map[uint32][]byte{1: testKey(1), 2: testKey(1)}},
	}
	for name, c := range cases {
		if _, err := blob.NewKeyRing(c.current, c.keys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestKeyRingNeverPrintsKeys(t *testing.T) {
	t.Parallel()
	ring := newRing(t, 2, 1, 2)
	key := versionKey(2)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("ring", "media_keys", ring)
	js, err := json.Marshal(struct {
		Ring *blob.KeyRing `json:"ring"`
	}{ring})
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{
		"%v":   fmt.Sprintf("%v", ring),
		"%+v":  fmt.Sprintf("%+v", ring),
		"%#v":  fmt.Sprintf("%#v", ring),
		"%s":   ring.String(),
		"%x":   fmt.Sprintf("%x", ring),
		"json": string(js),
		"slog": logged.String(),
	}
	for name, out := range outputs {
		for _, form := range []string{string(key), hex.EncodeToString(key), base64.StdEncoding.EncodeToString(key)} {
			if strings.Contains(out, form) {
				t.Fatalf("%s output carries key bytes: %s", name, out)
			}
		}
	}
	if string(js) != `{"ring":{"current":2,"versions":[1,2]}}` {
		t.Fatalf("json = %s", js)
	}
}

type timeoutStore struct {
	blob.Store

	timeout time.Duration
}

func (s timeoutStore) OperationTimeout() time.Duration { return s.timeout }

func TestEncryptedPreservesBackendOperationTimeout(t *testing.T) {
	t.Parallel()
	local, _ := newLocal(t)
	const timeout = 37 * time.Second
	wrapped, err := blob.NewEncrypted(timeoutStore{Store: local, timeout: timeout}, newRing(t, 1, 1), blob.DefaultChunkSize)
	if err != nil {
		t.Fatalf("new encrypted store: %v", err)
	}
	provider, ok := any(wrapped).(blob.OperationTimeoutProvider)
	if !ok {
		t.Fatal("encrypted store does not preserve the backend operation timeout")
	}
	if got := provider.OperationTimeout(); got != timeout {
		t.Fatalf("operation timeout = %s, want %s", got, timeout)
	}
}
