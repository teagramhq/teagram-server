package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/teagramhq/teagram-server/internal/blob"
)

type headerReadSpy struct {
	blob.Store

	reads []readWindow
}

type readWindow struct {
	key    string
	offset int64
	limit  int64
}

func (s *headerReadSpy) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	s.reads = append(s.reads, readWindow{key: key, offset: offset, limit: limit})
	return s.Store.ReadAt(ctx, key, offset, limit)
}

func TestInspectEnvelopeHeaderAuthenticatesOnlyTheHeader(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(7, map[uint32][]byte{7: bytes.Repeat([]byte{0x37}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	const key = "07/7"
	payload := bytes.Repeat([]byte("media"), 3_000)
	if _, err := sealed.Put(ctx, key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("put envelope: %v", err)
	}

	var physical int64
	if err := backend.Walk(ctx, func(e blob.Entry) error {
		if e.Key == key {
			physical = e.Size
		}
		return nil
	}); err != nil {
		t.Fatalf("walk backend: %v", err)
	}
	spy := &headerReadSpy{Store: backend}
	info, err := blob.InspectEnvelopeHeader(ctx, spy, ring, key, physical)
	if err != nil {
		t.Fatalf("inspect envelope: %v", err)
	}
	if !info.HasKeyVersion || info.KeyVersion != 7 || info.LogicalBytes != int64(len(payload)) {
		t.Fatalf("inspection = %+v, want key version 7 and %d logical bytes", info, len(payload))
	}
	if len(spy.reads) != 1 || spy.reads[0] != (readWindow{key: key, offset: 0, limit: 102}) {
		t.Fatalf("reads = %+v, want one 102-byte header read", spy.reads)
	}
}

func TestInspectEnvelopeHeaderRejectsPlaintext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	if _, err := backend.Put(ctx, "01/1", bytes.NewReader([]byte("plaintext media"))); err != nil {
		t.Fatalf("put plaintext: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x41}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, "01/1", int64(len("plaintext media")))
	if !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("inspection error = %v, want ErrEnvelope", err)
	}
	if info.HasKeyVersion {
		t.Fatalf("plaintext inspection claimed key version %d", info.KeyVersion)
	}
}

func TestInspectEnvelopeHeaderReadsOnlyAvailablePrefixForShortObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	const key = "01/1"
	if _, err := backend.Put(ctx, key, bytes.NewReader([]byte("tiny"))); err != nil {
		t.Fatalf("put short plaintext: %v", err)
	}
	spy := &headerReadSpy{Store: backend}
	info, err := blob.InspectEnvelopeHeader(ctx, spy, nil, key, 4)
	if !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("inspection error = %v, want ErrEnvelope", err)
	}
	if info.HasEnvelopeMagic || len(spy.reads) != 1 || spy.reads[0] != (readWindow{key: key, offset: 0, limit: 4}) {
		t.Fatalf("inspection = %+v with reads %+v, want one 4-byte header-prefix read", info, spy.reads)
	}
}

func TestInspectEnvelopeHeaderCannotValidateWithoutKeyRing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(3, map[uint32][]byte{3: bytes.Repeat([]byte{0x53}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	const key = "03/3"
	if _, err := sealed.Put(ctx, key, bytes.NewReader([]byte("payload"))); err != nil {
		t.Fatalf("put envelope: %v", err)
	}
	var physical int64
	if err := backend.Walk(ctx, func(e blob.Entry) error {
		if e.Key == key {
			physical = e.Size
		}
		return nil
	}); err != nil {
		t.Fatalf("walk backend: %v", err)
	}
	info, err := blob.InspectEnvelopeHeader(ctx, backend, nil, key, physical)
	if !errors.Is(err, blob.ErrEnvelopeKeyUnavailable) {
		t.Fatalf("inspection error = %v, want ErrEnvelopeKeyUnavailable", err)
	}
	if !info.HasKeyVersion || info.KeyVersion != 3 {
		t.Fatalf("inspection = %+v, want claimed key version 3", info)
	}
}

func TestInspectEnvelopeHeaderPreservesKeyClaimForTruncatedObject(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	ring, err := blob.NewKeyRing(5, map[uint32][]byte{5: bytes.Repeat([]byte{0x55}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(backend, ring, blob.MinChunkSize)
	if err != nil {
		t.Fatalf("create encrypted store: %v", err)
	}
	const key = "05/5"
	if _, err := sealed.Put(ctx, key, bytes.NewReader([]byte("payload"))); err != nil {
		t.Fatalf("put envelope: %v", err)
	}
	header, err := backend.ReadAt(ctx, key, 0, 102)
	if err != nil {
		t.Fatalf("read source header: %v", err)
	}
	if _, err := backend.Put(ctx, key, bytes.NewReader(header)); err != nil {
		t.Fatalf("replace with truncated envelope: %v", err)
	}

	info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, key, int64(len(header)))
	if !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("inspection error = %v, want invalid truncated envelope", err)
	}
	if !info.HasKeyVersion || info.KeyVersion != 5 || !info.HasLogicalBytes || info.LogicalBytes != 7 {
		t.Fatalf("inspection = %+v, want key version 5 and logical size 7 claim", info)
	}
}

func TestInspectEnvelopeHeaderPreservesUnknownFormatMarker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backend, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	header := make([]byte, 118)
	copy(header, "TGME")
	header[5] = 2
	if _, err := backend.Put(ctx, "01/1", bytes.NewReader(header)); err != nil {
		t.Fatalf("put unknown-format object: %v", err)
	}
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x31}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("create key ring: %v", err)
	}
	info, err := blob.InspectEnvelopeHeader(ctx, backend, ring, "01/1", int64(len(header)))
	if !errors.Is(err, blob.ErrEnvelope) {
		t.Fatalf("inspection error = %v, want ErrEnvelope", err)
	}
	if !info.HasEnvelopeMagic || info.HasKeyVersion {
		t.Fatalf("inspection = %+v, want unknown envelope marker without a readable version", info)
	}
}

func TestInspectEnvelopeHeaderPropagatesBackendReadFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("backend read failed")
	backend := readFailureStore{err: want}
	info, err := blob.InspectEnvelopeHeader(context.Background(), backend, nil, "01/1", 118)
	if !errors.Is(err, want) {
		t.Fatalf("inspection error = %v, want backend failure", err)
	}
	if info.HasKeyVersion {
		t.Fatalf("failed read returned header info %+v", info)
	}
}

type readFailureStore struct {
	err error
}

func (s readFailureStore) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, errors.New("unexpected Put")
}

func (s readFailureStore) ReadAt(context.Context, string, int64, int64) ([]byte, error) {
	return nil, s.err
}

func (s readFailureStore) Remove(context.Context, string) error {
	return errors.New("unexpected Remove")
}

func (s readFailureStore) WalkPrefix(context.Context, string, func(blob.Entry) error) error {
	return errors.New("unexpected WalkPrefix")
}
