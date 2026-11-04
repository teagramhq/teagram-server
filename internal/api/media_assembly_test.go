package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type mediaAssemblyReadFault struct {
	blob.Store

	key         string
	err         error
	short       bool
	readStarted chan struct{}
	releaseRead chan struct{}
}

func (b *mediaAssemblyReadFault) inject(key string, err error, short bool) {
	b.key, b.err, b.short = key, err, short
}

func (b *mediaAssemblyReadFault) ReadAt(ctx context.Context, key string, offset, limit int64) ([]byte, error) {
	if key == b.key {
		b.key = ""
		if b.readStarted != nil {
			close(b.readStarted)
			select {
			case <-b.releaseRead:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if b.err != nil {
			return nil, b.err
		}
		payload, err := b.Store.ReadAt(ctx, key, offset, limit)
		if err != nil {
			return nil, err
		}
		if b.short && len(payload) > 0 {
			return payload[:len(payload)-1], nil
		}
		return payload, nil
	}
	return b.Store.ReadAt(ctx, key, offset, limit)
}

func (b *mediaAssemblyReadFault) blockMissing(key string) {
	b.key = key
	b.err = blob.ErrNotFound
	b.readStarted = make(chan struct{})
	b.releaseRead = make(chan struct{}, 1)
}

type mediaAssemblyCauseDroppingBlob struct {
	blob.Store
}

func (b mediaAssemblyCauseDroppingBlob) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	payload, err := io.ReadAll(r)
	if err != nil {
		// Mirrors a backend that returns an operation error without preserving
		// the reader's error as its cause.
		return int64(len(payload)), errors.New("blob put failed")
	}
	return b.Store.Put(ctx, key, bytes.NewReader(payload))
}

type mediaAssemblyEarlyFailingBlob struct {
	blob.Store

	readStarted <-chan struct{}
	putReturned chan struct{}
	readerDone  chan error
}

func (b mediaAssemblyEarlyFailingBlob) Put(_ context.Context, _ string, r io.Reader) (int64, error) {
	go func() {
		_, err := io.Copy(io.Discard, r)
		b.readerDone <- err
	}()
	<-b.readStarted
	close(b.putReturned)
	return 0, errors.New("blob put failed")
}

type mediaAssemblyByteCountMismatchBlob struct {
	blob.Store
}

func (mediaAssemblyByteCountMismatchBlob) Put(context.Context, string, io.Reader) (int64, error) {
	return 0, nil
}

type mediaAssemblyHarness struct {
	store    *store.Store
	local    *blob.Local
	partBlob *mediaAssemblyReadFault
	user     store.User
	dsn      string
}

func newMediaAssemblyHarness(t *testing.T) *mediaAssemblyHarness {
	t.Helper()
	ctx := context.Background()
	local, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("blob store: %v", err)
	}
	partBlob := &mediaAssemblyReadFault{Store: local}
	dsn := pgtest.DSN(t)
	s, err := store.Open(ctx, dsn, pgtest.EncKey(), store.WithBlobStore(partBlob))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	u, err := s.CreateUser(ctx, "+15551990001")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &mediaAssemblyHarness{store: s, local: local, partBlob: partBlob, user: u, dsn: dsn}
}

func (h *mediaAssemblyHarness) assemble(clientFileID int64, parts int, blobs blob.Store, maxUserStorageBytes int64) (store.File, error) {
	return api.AssembleFileForTest(h.store, h.user.ID, clientFileID, parts, "file.bin", "application/octet-stream", blobs, maxUserStorageBytes)
}

func saveMediaAssemblyParts(t *testing.T, s *store.Store, userID, fileID int64, parts [][]byte) int64 {
	t.Helper()
	var total int64
	for i, payload := range parts {
		if err := s.SaveUploadPart(context.Background(), userID, fileID, i, payload, 1<<20); err != nil {
			t.Fatalf("save part %d: %v", i, err)
		}
		total += int64(len(payload))
	}
	return total
}

func mediaAssemblyRPCCode(err error) string {
	if rpcErr, ok := errors.AsType[*tgerr.Error](err); ok {
		return rpcErr.Message
	}
	return ""
}

func TestAssembleMissingUploadPayloadReturnsMediaInvalidAndKeepsRetryParts(t *testing.T) {
	t.Parallel()
	for _, missingIndex := range []int{0, 1} {
		t.Run(map[int]string{0: "first_part", 1: "later_part"}[missingIndex], func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newMediaAssemblyHarness(t)
			parts := [][]byte{bytes.Repeat([]byte{0x31}, 4096), bytes.Repeat([]byte{0x72}, 1024)}
			const clientFileID = int64(7731)
			total := saveMediaAssemblyParts(t, h.store, h.user.ID, clientFileID, parts)
			refs, err := h.store.UploadPartRefs(ctx, h.user.ID, clientFileID)
			if err != nil {
				t.Fatalf("part refs: %v", err)
			}
			if err := h.local.Remove(ctx, refs[missingIndex].Key); err != nil {
				t.Fatalf("remove part %d bytes: %v", missingIndex, err)
			}
			if _, err := h.store.ReadUploadPart(ctx, refs[missingIndex]); !errors.Is(err, store.ErrUploadPartMissing) || !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("missing part read = %v, want typed missing-payload error", err)
			}

			before, err := h.store.AllocatedFileIDCeiling(ctx)
			if err != nil {
				t.Fatalf("file id ceiling before: %v", err)
			}
			if _, err := h.assemble(clientFileID, len(parts), mediaAssemblyCauseDroppingBlob{Store: h.local}, 1<<30); mediaAssemblyRPCCode(err) != "MEDIA_INVALID" {
				t.Fatalf("assemble with missing part %d = %v, want MEDIA_INVALID", missingIndex, err)
			}
			failedID, err := h.store.AllocatedFileIDCeiling(ctx)
			if err != nil {
				t.Fatalf("file id ceiling after: %v", err)
			}
			if failedID != before+1 {
				t.Fatalf("failed assembly allocated id %d, want %d", failedID, before+1)
			}
			files, err := h.store.FilesByIDs(ctx, []int64{failedID})
			if err != nil {
				t.Fatalf("stored files: %v", err)
			}
			if _, ok := files[failedID]; ok {
				t.Fatalf("failed assembly %d is marked stored", failedID)
			}
			if _, err := h.local.ReadAt(ctx, blob.Key(failedID), 0, total); !errors.Is(err, blob.ErrNotFound) {
				t.Fatalf("failed assembled bytes = %v, want ErrNotFound", err)
			}
			count, _, _, err := h.store.UploadPartsSummary(ctx, h.user.ID, clientFileID)
			if err != nil || count != int64(len(parts)) {
				t.Fatalf("upload parts after failure = %d, %v; want %d retained parts", count, err, len(parts))
			}

			if err := h.store.SaveUploadPart(ctx, h.user.ID, clientFileID, missingIndex, parts[missingIndex], 1<<20); err != nil {
				t.Fatalf("restore part %d: %v", missingIndex, err)
			}
			file, err := h.assemble(clientFileID, len(parts), mediaAssemblyCauseDroppingBlob{Store: h.local}, 1<<30)
			if err != nil {
				t.Fatalf("retry assembly: %v", err)
			}
			got, err := h.local.ReadAt(ctx, blob.Key(file.ID), 0, total)
			if err != nil {
				t.Fatalf("read retry bytes: %v", err)
			}
			want := append(append([]byte(nil), parts[0]...), parts[1]...)
			if !bytes.Equal(got, want) {
				t.Fatalf("retry payload differs: got %d bytes, want %d identical bytes", len(got), len(want))
			}
		})
	}
}

func TestAssembleMissingUploadPayloadWhenPutReturnsBeforeReadFinishes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	const clientFileID = int64(7734)
	saveMediaAssemblyParts(t, h.store, h.user.ID, clientFileID, [][]byte{bytes.Repeat([]byte{0x38}, 4096)})
	refs, err := h.store.UploadPartRefs(ctx, h.user.ID, clientFileID)
	if err != nil {
		t.Fatalf("part refs: %v", err)
	}
	h.partBlob.blockMissing(refs[0].Key)
	t.Cleanup(func() {
		select {
		case h.partBlob.releaseRead <- struct{}{}:
		default:
		}
	})
	blobs := mediaAssemblyEarlyFailingBlob{
		Store:       h.local,
		readStarted: h.partBlob.readStarted,
		putReturned: make(chan struct{}),
		readerDone:  make(chan error, 1),
	}
	assembled := make(chan error, 1)
	go func() {
		_, err := h.assemble(clientFileID, 1, blobs, 1<<30)
		assembled <- err
	}()
	<-blobs.putReturned
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-assembled:
		h.partBlob.releaseRead <- struct{}{}
		<-blobs.readerDone
		t.Fatalf("assembly returned %s before the active part read finished", mediaAssemblyRPCCode(err))
	case <-timer.C:
	}
	h.partBlob.releaseRead <- struct{}{}
	if err := <-assembled; mediaAssemblyRPCCode(err) != "MEDIA_INVALID" {
		t.Fatalf("assembly after early Put failure = %v, want MEDIA_INVALID", err)
	}
	if err := <-blobs.readerDone; !errors.Is(err, store.ErrUploadPartMissing) {
		t.Fatalf("stream read error = %v, want ErrUploadPartMissing", err)
	}
}

func TestSendMediaPhotoUploadPartOverwriteDuringAssembly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	recipient, err := h.store.CreateUser(ctx, "+15551990002")
	if err != nil {
		t.Fatalf("create recipient: %v", err)
	}
	const clientFileID = int64(7735)
	const randomID = int64(7736)
	body := jpegPhotoPayload(t, 640, 480)
	saveParts(t, h.store, h.user.ID, clientFileID, body)
	refs, err := h.store.UploadPartRefs(ctx, h.user.ID, clientFileID)
	if err != nil {
		t.Fatalf("upload part refs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("upload part refs = %d, want one", len(refs))
	}
	h.partBlob.key = refs[0].Key
	h.partBlob.readStarted = make(chan struct{})
	h.partBlob.releaseRead = make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case h.partBlob.releaseRead <- struct{}{}:
		default:
		}
	})
	request := &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(h.user.ID, recipient.ID), Media: uploadedPhoto(clientFileID, 1, "219343.jpg", jpegPhotoMD5(body)), RandomID: randomID,
	}
	type sendResult struct {
		result bin.Encoder
		err    error
	}
	sent := make(chan sendResult, 1)
	go func() {
		result, err := api.SendMediaForTest(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, request)
		sent <- sendResult{result: result, err: err}
	}()
	<-h.partBlob.readStarted
	if err := h.store.SaveUploadPart(ctx, h.user.ID, clientFileID, 0, body, 1<<20); err != nil {
		t.Fatalf("overwrite upload part during assembly: %v", err)
	}
	h.partBlob.releaseRead <- struct{}{}
	first := <-sent
	if got := mediaAssemblyRPCCode(first.err); got != "MEDIA_INVALID" {
		t.Fatalf("send during upload-part overwrite = %v, want MEDIA_INVALID", first.err)
	}
	if first.result != nil {
		t.Fatalf("send during upload-part overwrite returned %T, want no result", first.result)
	}
	if _, found, err := h.store.MessageByRandomID(ctx, h.user.ID, randomID); err != nil || found {
		t.Fatalf("message after overwritten assembly = found %v, err %v; want no message", found, err)
	}
	fileID, err := h.store.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("allocated file ID ceiling: %v", err)
	}
	files, err := h.store.FilesByIDs(ctx, []int64{fileID})
	if err != nil {
		t.Fatalf("load allocated file: %v", err)
	}
	if _, found := files[fileID]; found {
		t.Fatalf("overwritten assembly file %d is stored", fileID)
	}
	if count, _, _, err := h.store.UploadPartsSummary(ctx, h.user.ID, clientFileID); err != nil || count != 1 {
		t.Fatalf("upload parts after overwrite = %d, err=%v; want one retryable part", count, err)
	}

	retried, err := api.SendMediaForTest(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, request)
	if err != nil {
		t.Fatalf("retry photo send from replacement part: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, retried))
	files, err = h.store.FilesByIDs(ctx, []int64{photo.ID})
	if err != nil {
		t.Fatalf("load retry photo metadata: %v", err)
	}
	stored, ok := files[photo.ID]
	if !ok {
		t.Fatal("retry photo metadata is missing; want stored photo")
	}
	if stored.Kind != store.FileKindPhoto || stored.Width != 640 || stored.Height != 480 {
		t.Fatalf("retry photo metadata = %+v, want stored 640x480 photo", stored)
	}
}

func TestReadUploadPartMissingEmptyKeyIsTyped(t *testing.T) {
	t.Parallel()
	h := newMediaAssemblyHarness(t)
	_, err := h.store.ReadUploadPart(context.Background(), store.UploadPartRef{Index: 3})
	if !errors.Is(err, store.ErrUploadPartMissing) {
		t.Fatalf("empty-key part error = %v, want ErrUploadPartMissing", err)
	}
}

func TestAssembleNonMissingPartReadFailuresRemainInternal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		short bool
	}{
		{name: "backend_io", err: errors.New("backend unavailable")},
		{name: "canceled_read", err: context.Canceled},
		{name: "deadline_read", err: context.DeadlineExceeded},
		{name: "invalid_key", err: blob.ErrInvalidKey},
		{name: "short_read", short: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h := newMediaAssemblyHarness(t)
			const clientFileID = int64(7732)
			total := saveMediaAssemblyParts(t, h.store, h.user.ID, clientFileID, [][]byte{bytes.Repeat([]byte{0x41}, 4096)})
			refs, err := h.store.UploadPartRefs(ctx, h.user.ID, clientFileID)
			if err != nil {
				t.Fatalf("part refs: %v", err)
			}
			h.partBlob.inject(refs[0].Key, tc.err, tc.short)
			if _, err := h.assemble(clientFileID, 1, mediaAssemblyCauseDroppingBlob{Store: h.local}, 1<<30); mediaAssemblyRPCCode(err) != "INTERNAL" {
				t.Fatalf("assemble after %s = %v, want INTERNAL (size %d)", tc.name, err, total)
			}
		})
	}
}

func TestAssembleQuotaAndByteCountMismatchCodesRemainUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		want string
	}{
		{name: "quota", want: "STORAGE_CHECK_FAILED"},
		{name: "final_byte_count_mismatch", want: "INTERNAL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newMediaAssemblyHarness(t)
			const clientFileID = int64(7733)
			saveMediaAssemblyParts(t, h.store, h.user.ID, clientFileID, [][]byte{bytes.Repeat([]byte{0x53}, 4096)})
			blobs := blob.Store(mediaAssemblyCauseDroppingBlob{Store: h.local})
			maxUserStorageBytes := int64(1 << 30)
			if tc.name == "quota" {
				maxUserStorageBytes = 1
			} else {
				blobs = mediaAssemblyByteCountMismatchBlob{Store: h.local}
			}
			if _, err := h.assemble(clientFileID, 1, blobs, maxUserStorageBytes); mediaAssemblyRPCCode(err) != tc.want {
				t.Fatalf("assemble after %s = %v, want %s", tc.name, err, tc.want)
			}
		})
	}
}
