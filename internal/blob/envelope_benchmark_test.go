package blob_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
)

const (
	benchmarkObjectSize = 100 << 20
	benchmarkPartSize   = 512 << 10
	benchmarkRangeSize  = 4 << 10
	benchmarkSeqSize    = 1 << 20
	benchmarkMaxInt64   = 1<<63 - 1
)

var benchmarkChunkSizes = []struct {
	name string
	size int
}{
	{name: "chunk-16KiB", size: 16 << 10},
	{name: "chunk-64KiB", size: 64 << 10},
	{name: "chunk-256KiB", size: 256 << 10},
}

type measuredBlobStore struct {
	store     blob.Store
	readBytes atomic.Int64
	readCalls atomic.Int64
}

func (s *measuredBlobStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	return s.store.Put(ctx, key, r)
}

func (s *measuredBlobStore) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	data, err := s.store.ReadAt(ctx, key, offset, limit)
	s.readCalls.Add(1)
	if err == nil {
		s.readBytes.Add(int64(len(data)))
	}
	return data, err
}

func (s *measuredBlobStore) Remove(ctx context.Context, key string) error {
	return s.store.Remove(ctx, key)
}

func (s *measuredBlobStore) WalkPrefix(ctx context.Context, prefix string, fn func(blob.Entry) error) error {
	return s.store.WalkPrefix(ctx, prefix, fn)
}

func (s *measuredBlobStore) resetReadMetrics() {
	s.readBytes.Store(0)
	s.readCalls.Store(0)
}

// BenchmarkMediaEnvelopeChunkSizes compares plaintext with three envelope
// chunk sizes on both production blob backends. Run with -cpu=1 to match the
// single-vCPU default deployment; the mixed benchmark fixes two workers, one
// active reader and one upload assembly, matching its per-account read and
// assembly slots.
func BenchmarkMediaEnvelopeChunkSizes(b *testing.B) {
	payload := pattern(benchmarkObjectSize)
	variants := append([]struct {
		name string
		size int
	}{{name: "plaintext"}}, benchmarkChunkSizes...)

	for _, backendName := range []string{"Local", "S3"} {
		b.Run(backendName, func(b *testing.B) {
			backend := benchmarkBlobBackend(b, backendName)
			for _, variant := range variants {
				b.Run(variant.name, func(b *testing.B) {
					measured := &measuredBlobStore{store: backend}
					var store blob.Store = measured
					if variant.size != 0 {
						ring, err := blob.NewKeyRing(1, map[uint32][]byte{
							1: bytes.Repeat([]byte{0x5a}, blob.MediaKeyLen),
						})
						if err != nil {
							b.Fatalf("new benchmark key ring: %v", err)
						}
						encrypted, err := blob.NewEncrypted(measured, ring, variant.size)
						if err != nil {
							b.Fatalf("new encrypted store: %v", err)
						}
						store = encrypted
					}

					ctx := context.Background()
					prefix := "bench/" + variant.name
					largeKey := prefix + "/large"
					partKey := "parts/bench-" + variant.name
					if _, err := store.Put(ctx, largeKey, bytes.NewReader(payload)); err != nil {
						b.Fatalf("seed large object: %v", err)
					}
					if _, err := store.Put(ctx, partKey, bytes.NewReader(payload[:benchmarkPartSize])); err != nil {
						b.Fatalf("seed part object: %v", err)
					}
					seedRead, err := store.ReadAt(ctx, largeKey, 0, int64(len(payload)))
					if err != nil {
						b.Fatalf("verify seeded large object: %v", err)
					}
					if !bytes.Equal(seedRead, payload) {
						b.Fatal("seeded large object differs from source")
					}
					b.Cleanup(func() {
						if err := store.Remove(ctx, largeKey); err != nil {
							b.Errorf("remove large benchmark object: %v", err)
						}
						if err := store.Remove(ctx, partKey); err != nil {
							b.Errorf("remove part benchmark object: %v", err)
						}
					})

					b.Run("read-4KiB-random", func(b *testing.B) {
						benchmarkRandomRead(b, store, measured, largeKey, benchmarkObjectSize, benchmarkRangeSize)
					})
					b.Run("read-1MiB-sequential", func(b *testing.B) {
						benchmarkSequentialRead(b, store, measured, largeKey, benchmarkObjectSize, benchmarkSeqSize)
					})
					b.Run("read-512KiB-part", func(b *testing.B) {
						benchmarkFixedRead(b, store, measured, partKey, benchmarkPartSize)
					})
					b.Run("put-read-100MiB", func(b *testing.B) {
						benchmarkPutRead(b, store, measured, largeKey, payload)
					})
					b.Run("mixed-1R1W", func(b *testing.B) {
						benchmarkConcurrentReadWrite(b, store, measured, largeKey, prefix+"/write", payload)
					})
				})
			}
		})
	}
}

func benchmarkBlobBackend(b *testing.B, name string) blob.Store {
	b.Helper()
	switch name {
	case "Local":
		store, err := blob.NewLocal(filepath.Join(b.TempDir(), "blobs"))
		if err != nil {
			b.Fatalf("new local benchmark store: %v", err)
		}
		return store
	case "S3":
		return newRustFSStore(b)
	default:
		b.Fatalf("unknown benchmark backend %q", name)
		return nil
	}
}

func benchmarkRandomRead(b *testing.B, store blob.Store, measured *measuredBlobStore, key string, objectSize, readSize int64) {
	b.Helper()
	seed := uint64(1)
	b.SetBytes(readSize)
	measured.resetReadMetrics()
	var reads int64
	b.ResetTimer()
	for range b.N {
		seed ^= seed << 13
		seed ^= seed >> 7
		seed ^= seed << 17
		offset := int64(seed&benchmarkMaxInt64) % (objectSize - readSize + 1)
		data, err := store.ReadAt(context.Background(), key, offset, readSize)
		if err != nil {
			b.Fatalf("random range read: %v", err)
		}
		if int64(len(data)) != readSize {
			b.Fatalf("random range returned %d bytes, want %d", len(data), readSize)
		}
		reads++
	}
	b.StopTimer()
	reportReadMetrics(b, measured, reads)
}

func benchmarkSequentialRead(b *testing.B, store blob.Store, measured *measuredBlobStore, key string, objectSize, readSize int64) {
	b.Helper()
	b.SetBytes(readSize)
	measured.resetReadMetrics()
	rangeCount := (objectSize-readSize)/readSize + 1
	var reads int64
	b.ResetTimer()
	for i := range b.N {
		offset := (int64(i) % rangeCount) * readSize
		data, err := store.ReadAt(context.Background(), key, offset, readSize)
		if err != nil {
			b.Fatalf("sequential range read: %v", err)
		}
		if int64(len(data)) != readSize {
			b.Fatalf("sequential range returned %d bytes, want %d", len(data), readSize)
		}
		reads++
	}
	b.StopTimer()
	reportReadMetrics(b, measured, reads)
}

func benchmarkFixedRead(b *testing.B, store blob.Store, measured *measuredBlobStore, key string, readSize int64) {
	b.Helper()
	b.SetBytes(readSize)
	measured.resetReadMetrics()
	var reads int64
	b.ResetTimer()
	for range b.N {
		data, err := store.ReadAt(context.Background(), key, 0, readSize)
		if err != nil {
			b.Fatalf("part read: %v", err)
		}
		if int64(len(data)) != readSize {
			b.Fatalf("part read returned %d bytes, want %d", len(data), readSize)
		}
		reads++
	}
	b.StopTimer()
	reportReadMetrics(b, measured, reads)
}

func benchmarkPutRead(b *testing.B, store blob.Store, measured *measuredBlobStore, key string, payload []byte) {
	b.Helper()
	b.SetBytes(2 * int64(len(payload)))
	measured.resetReadMetrics()
	var reads int64
	b.ResetTimer()
	for range b.N {
		written, err := store.Put(context.Background(), key, bytes.NewReader(payload))
		if err != nil {
			b.Fatalf("put 100 MiB object: %v", err)
		}
		if written != int64(len(payload)) {
			b.Fatalf("put wrote %d bytes, want %d", written, len(payload))
		}
		data, err := store.ReadAt(context.Background(), key, 0, int64(len(payload)))
		if err != nil {
			b.Fatalf("full object read: %v", err)
		}
		if len(data) != len(payload) {
			b.Fatalf("full object read returned %d bytes, want %d", len(data), len(payload))
		}
		reads++
	}
	b.StopTimer()
	reportReadMetrics(b, measured, reads)
}

func benchmarkConcurrentReadWrite(b *testing.B, store blob.Store, measured *measuredBlobStore, readKey, writeKey string, payload []byte) {
	b.Helper()
	const readSize = 1 << 20
	b.SetBytes(2 * readSize)
	measured.resetReadMetrics()
	readJobs := make(chan *sync.WaitGroup)
	writeJobs := make(chan *sync.WaitGroup)
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	var workers sync.WaitGroup
	ready.Add(2)
	workers.Add(2)
	go func() {
		defer workers.Done()
		ready.Done()
		for start := range readJobs {
			start.Done()
			start.Wait()
			data, err := store.ReadAt(context.Background(), readKey, 32<<20, readSize)
			if err == nil && len(data) != readSize {
				err = fmt.Errorf("concurrent read returned %d bytes, want %d", len(data), readSize)
			}
			if err != nil {
				errs <- fmt.Errorf("concurrent read: %w", err)
			} else {
				errs <- nil
			}
		}
	}()
	go func() {
		defer workers.Done()
		ready.Done()
		for start := range writeJobs {
			start.Done()
			start.Wait()
			written, err := store.Put(context.Background(), writeKey, bytes.NewReader(payload[:readSize]))
			if err == nil && written != readSize {
				err = fmt.Errorf("concurrent write stored %d bytes, want %d", written, readSize)
			}
			if err != nil {
				errs <- fmt.Errorf("concurrent write: %w", err)
			} else {
				errs <- nil
			}
		}
	}()
	ready.Wait()
	defer func() {
		close(readJobs)
		close(writeJobs)
		workers.Wait()
	}()
	b.ResetTimer()
	for range b.N {
		var start sync.WaitGroup
		start.Add(2)
		readJobs <- &start
		writeJobs <- &start
		readErr, writeErr := <-errs, <-errs
		if readErr != nil {
			b.Fatal(readErr)
		}
		if writeErr != nil {
			b.Fatal(writeErr)
		}
	}
	b.StopTimer()
	reportReadMetrics(b, measured, int64(b.N))
}

func reportReadMetrics(b *testing.B, measured *measuredBlobStore, reads int64) {
	b.Helper()
	if reads == 0 {
		b.Fatal("benchmark completed without a read")
	}
	b.ReportMetric(float64(measured.readBytes.Load())/float64(reads), "backend-B/read")
	b.ReportMetric(float64(measured.readCalls.Load())/float64(reads), "backend-requests/read")
}
