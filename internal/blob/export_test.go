package blob

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// CreateBucketForTest provisions the bucket used by the integration harness
// without expanding the production Store API with lifecycle operations.
func CreateBucketForTest(ctx context.Context, s *S3) error {
	opCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	resp, err := s.doRequest(opCtx, http.MethodPut, s.bucketPath(), "", nil, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close() //nolint:errcheck // test bucket provisioning cleanup
	}()
	return drainBody(resp.Body)
}

// PutRawObjectForTest seeds a test bucket with a key that the public Store API
// deliberately rejects, so the conformance suite can exercise objects created
// by an outside actor.
func PutRawObjectForTest(ctx context.Context, s *S3, key string, data []byte) error {
	body, err := inspectS3Body(bytes.NewReader(data))
	if err != nil {
		return err
	}
	opCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	resp, err := s.doRequest(opCtx, http.MethodPut, s.objectPath(s.prefix+key), "", body, nil)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close() //nolint:errcheck // test object provisioning cleanup
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if err := drainBody(resp.Body); err != nil {
			return err
		}
		return s.operationError("put", nil)
	}
	return drainBody(resp.Body)
}

func TestRotationBodyIsNotReplayedAfterEarlyRetryableResponse(t *testing.T) {
	store := &blockedRotationChunkStore{
		firstReadStarted:  make(chan struct{}),
		secondReadStarted: make(chan struct{}),
		release:           make(chan struct{}),
	}
	defer store.releaseReads()
	body := &rotationBody{
		ctx:     context.Background(),
		backend: store,
		key:     "aa/1",
		source:  envelopeHeader{chunkSize: 16, plainLen: 16},
		target:  envelopeHeader{chunkSize: 16, plainLen: 16},
	}
	requestBody, err := inspectS3Body(body)
	if err != nil {
		t.Fatalf("inspect rotation body: %v", err)
	}

	s3, err := NewS3(S3Config{
		Endpoint: "https://s3.example.test", Bucket: "rotation-test",
		AccessKeyID: "access", SecretAccessKey: "secret", MaxAttempts: 2,
		OperationTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("create S3 store: %v", err)
	}
	transport := &earlyResponseTransport{
		firstReadStarted:  store.firstReadStarted,
		secondReadStarted: store.secondReadStarted,
		readErrors:        make(chan error, 2),
		t:                 t,
	}
	s3.client.Transport = transport
	resp, err := s3.doRequest(context.Background(), http.MethodPut, s3.objectPath("aa/1"), "", requestBody, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close response body: %v", err)
		}
	}()
	store.releaseReads()
	transport.readers.Wait()
	if readErr := <-transport.readErrors; readErr != nil {
		t.Fatalf("read first rotation attempt body: %v", readErr)
	}

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d, want early retryable 503", resp.StatusCode)
	}
	if got := transport.attempts.Load(); got != 1 {
		t.Fatalf("PUT attempts = %d, want the in-flight rotation body sent once", got)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reads != 1 || store.maxActive != 1 {
		t.Fatalf("source chunk reads = %d (max concurrent %d), want one", store.reads, store.maxActive)
	}
}

type blockedRotationChunkStore struct {
	Store

	firstReadStarted  chan struct{}
	secondReadStarted chan struct{}
	release           chan struct{}
	mu                sync.Mutex
	reads             int
	active            int
	maxActive         int
	releaseOnce       sync.Once
}

func (s *blockedRotationChunkStore) ReadAt(_ context.Context, _ string, _ int64, limit int64) ([]byte, error) {
	s.mu.Lock()
	s.reads++
	s.active++
	s.maxActive = max(s.maxActive, s.active)
	read := s.reads
	s.mu.Unlock()
	switch read {
	case 1:
		close(s.firstReadStarted)
	case 2:
		close(s.secondReadStarted)
		s.releaseReads()
	}
	<-s.release
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return make([]byte, limit), nil
}

func (s *blockedRotationChunkStore) releaseReads() {
	s.releaseOnce.Do(func() { close(s.release) })
}

type earlyResponseTransport struct {
	attempts          atomic.Int32
	firstReadStarted  <-chan struct{}
	secondReadStarted <-chan struct{}
	readers           sync.WaitGroup
	readErrors        chan error
	t                 *testing.T
}

func (t *earlyResponseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempt := t.attempts.Add(1)
	if attempt == 1 {
		t.readers.Go(func() {
			defer func() {
				if err := req.Body.Close(); err != nil {
					t.t.Errorf("close first request body: %v", err)
				}
			}()
			t.readErrors <- readRotationHeaderAndChunk(req.Body)
		})
		select {
		case <-t.firstReadStarted:
		case <-time.After(3 * time.Second):
			return nil, context.DeadlineExceeded
		}
		return rotationResponse(req, http.StatusServiceUnavailable), nil
	}
	defer func() {
		if err := req.Body.Close(); err != nil {
			t.t.Errorf("close retry request body: %v", err)
		}
	}()
	if err := readRotationHeaderAndChunk(req.Body); err != nil {
		return nil, err
	}
	return rotationResponse(req, http.StatusOK), nil
}

func readRotationHeaderAndChunk(body io.Reader) error {
	header := make([]byte, envelopeHeaderLen)
	if _, err := io.ReadFull(body, header); err != nil {
		return err
	}
	chunk := make([]byte, envelopeHeaderLen)
	n, err := body.Read(chunk)
	if n == 0 {
		if err == nil {
			return io.ErrNoProgress
		}
		return err
	}
	return nil
}

func rotationResponse(req *http.Request, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}
