package api

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- Telegram inputFile requires this compatibility checksum.
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

const photoUploadS3Root = "/test-bucket/tenant/"

type photoUploadS3Put struct {
	contentLength int64
	body          []byte
	readErr       error
}

type photoUploadS3Server struct {
	mu       sync.Mutex
	objects  map[string][]byte
	filePuts []photoUploadS3Put
}

func newPhotoUploadS3Server() *photoUploadS3Server {
	return &photoUploadS3Server{objects: make(map[string][]byte)}
}

func (s *photoUploadS3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, photoUploadS3Root)
	if !ok || key == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, readErr := io.ReadAll(r.Body)
		if _, isFile := blob.ParseKey(key); isFile {
			s.mu.Lock()
			s.filePuts = append(s.filePuts, photoUploadS3Put{
				contentLength: r.ContentLength,
				body:          append([]byte(nil), body...),
				readErr:       readErr,
			})
			s.mu.Unlock()
		}
		if readErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.objects[key] = append([]byte(nil), body...)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.mu.Lock()
		body, found := s.objects[key]
		body = append([]byte(nil), body...)
		s.mu.Unlock()
		if !found {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if _, err := io.WriteString(w, "<Error><Code>NoSuchKey</Code></Error>"); err != nil {
				return
			}
			return
		}
		start, end, err := parsePhotoUploadRange(r.Header.Get("Range"))
		if err != nil || start >= int64(len(body)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= int64(len(body)) {
			end = int64(len(body)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		if _, err := w.Write(body[start : end+1]); err != nil {
			return
		}
	case http.MethodDelete:
		s.mu.Lock()
		delete(s.objects, key)
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func parsePhotoUploadRange(value string) (int64, int64, error) {
	value, ok := strings.CutPrefix(value, "bytes=")
	if !ok {
		return 0, 0, errors.New("missing range")
	}
	startText, endText, ok := strings.Cut(value, "-")
	if !ok {
		return 0, 0, errors.New("malformed range")
	}
	start, startErr := strconv.ParseInt(startText, 10, 64)
	end, endErr := strconv.ParseInt(endText, 10, 64)
	if startErr != nil || endErr != nil || start < 0 || end < start {
		return 0, 0, errors.New("invalid range")
	}
	return start, end, nil
}

func (s *photoUploadS3Server) filePutSnapshot() []photoUploadS3Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]photoUploadS3Put(nil), s.filePuts...)
}

func (s *photoUploadS3Server) fileObject(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.objects[key]
	return append([]byte(nil), body...), ok
}

func newPhotoUploadS3(t *testing.T, server *photoUploadS3Server) *blob.S3 {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	store, err := blob.NewS3(blob.S3Config{
		Endpoint:          httpServer.URL,
		Bucket:            "test-bucket",
		Prefix:            "tenant/",
		AccessKeyID:       "access",
		SecretAccessKey:   "secret",
		AllowInsecureHTTP: true,
		MaxAttempts:       2,
		OperationTimeout:  time.Second,
	})
	if err != nil {
		t.Fatalf("new S3 store: %v", err)
	}
	return store
}

func openStoreWithPhotoUploadS3(t *testing.T, blobs blob.Store) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), pgtest.DSN(t), pgtest.EncKey(), store.WithBlobStore(blobs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return s
}

func encodePhotoUploadRequest(t *testing.T, req interface{ Encode(*bin.Buffer) error }) *bin.Buffer {
	t.Helper()
	buf := &bin.Buffer{}
	if err := req.Encode(buf); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return buf
}

func photoUploadJPEGOfSize(t *testing.T, size int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := range 480 {
		for x := range 640 {
			img.SetRGBA(x, y, color.RGBA{R: 70, G: 120, B: 180, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	base := encoded.Bytes()
	if len(base) < 2 || !bytes.Equal(base[len(base)-2:], []byte{0xff, 0xd9}) {
		t.Fatal("encoded JPEG has no end marker")
	}
	commentSize := size - len(base) - 4
	if commentSize < 0 || commentSize > 65533 {
		t.Fatalf("cannot size JPEG from %d to %d bytes", len(base), size)
	}
	result := make([]byte, 0, size)
	result = append(result, base[:len(base)-2]...)
	result = append(result, 0xff, 0xfe)
	result = binary.BigEndian.AppendUint16(result, uint16(commentSize+2)) //nolint:gosec // G115: comment length is bounded above.
	result = append(result, bytes.Repeat([]byte{'x'}, commentSize)...)
	result = append(result, 0xff, 0xd9)
	if len(result) != size {
		t.Fatalf("sized JPEG = %d bytes, want %d", len(result), size)
	}
	if _, err := validateJPEG(bytes.NewReader(result), int64(size), photoUploadMD5(result)); err != nil {
		t.Fatalf("validate sized JPEG fixture: %v", err)
	}
	return result
}

func photoUploadMD5(body []byte) string {
	sum := md5.Sum(body) // #nosec G401 -- Telegram inputFile requires this compatibility checksum.
	return hex.EncodeToString(sum[:])
}

func TestUploadMediaPhotoStreamsKnownLengthToS3(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s3Server := newPhotoUploadS3Server()
	blobs := newPhotoUploadS3(t, s3Server)
	s := openStoreWithPhotoUploadS3(t, blobs)
	user, err := s.CreateUser(ctx, "+15551990111")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	h := testHandlers(s)
	h.blobs = blobs
	h.maxUserStorageBytes = 1 << 30
	h.uploadGrantTTL = time.Hour

	const clientFileID = int64(80101)
	body := photoUploadJPEGOfSize(t, 10708)
	saveReq := &tg.UploadSaveFilePartRequest{FileID: clientFileID, FilePart: 0, Bytes: body}
	if _, err := h.handleSaveFilePart(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, saveReq),
	}); err != nil {
		t.Fatalf("upload.saveFilePart: %v", err)
	}
	uploadReq := &tg.MessagesUploadMediaRequest{
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
			ID: clientFileID, Parts: 1, Name: "photo.jpg", MD5Checksum: photoUploadMD5(body),
		}},
	}
	media, err := h.handleUploadMedia(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, uploadReq),
	})
	if err != nil {
		t.Fatalf("messages.uploadMedia: %v", err)
	}
	messageMedia, ok := media.(*tg.MessageMediaPhoto)
	if !ok {
		t.Fatalf("messages.uploadMedia result = %T, want *tg.MessageMediaPhoto", media)
	}
	photo, ok := messageMedia.Photo.(*tg.Photo)
	if !ok {
		t.Fatalf("uploaded photo = %T, want *tg.Photo", messageMedia.Photo)
	}
	files, err := s.FilesByIDs(ctx, []int64{photo.ID})
	if err != nil {
		t.Fatalf("load uploaded photo: %v", err)
	}
	file, ok := files[photo.ID]
	if !ok || !file.Stored || file.Kind != store.FileKindPhoto || file.Size != int64(len(body)) {
		t.Fatalf("uploaded file = %+v (found %v), want a stored %d-byte photo", file, ok, len(body))
	}
	if count, _, _, err := s.UploadPartsSummary(ctx, user.ID, clientFileID); err != nil || count != 0 {
		t.Fatalf("upload parts after success = %d, err=%v; want removed", count, err)
	}

	puts := s3Server.filePutSnapshot()
	if len(puts) != 1 {
		t.Fatalf("assembled S3 PUT requests = %d, want one", len(puts))
	}
	if puts[0].contentLength != int64(len(body)) || puts[0].readErr != nil || !bytes.Equal(puts[0].body, body) {
		t.Fatalf("assembled S3 PUT = length %d, body %d bytes, read error %v; want length and bytes %d", puts[0].contentLength, len(puts[0].body), puts[0].readErr, len(body))
	}
	storedBody, found := s3Server.fileObject(blob.Key(photo.ID))
	if !found || !bytes.Equal(storedBody, body) {
		t.Fatalf("assembled S3 object = %d bytes (found %v), want %d identical bytes", len(storedBody), found, len(body))
	}
	readBack, err := blobs.ReadAt(ctx, blob.Key(photo.ID), 0, int64(len(body)))
	if err != nil || !bytes.Equal(readBack, body) {
		t.Fatalf("read back uploaded photo = %d bytes, err %v; want %d identical bytes", len(readBack), err, len(body))
	}
}

type photoUploadEarlyBody struct {
	io.Reader

	size int64
}

func (r photoUploadEarlyBody) Size() int64 { return r.size }

type photoUploadEarlyPutStore struct {
	blob.Store

	expected int64
}

func (s photoUploadEarlyPutStore) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	if _, isFile := blob.ParseKey(key); !isFile {
		return s.Store.Put(ctx, key, r)
	}
	if sized, ok := r.(interface{ Size() int64 }); ok && sized.Size() != s.expected {
		return 0, fmt.Errorf("assembly length %d, want %d", sized.Size(), s.expected)
	}
	return s.Store.Put(ctx, key, photoUploadEarlyBody{
		Reader: io.LimitReader(r, s.expected-1),
		size:   s.expected,
	})
}

func TestUploadMediaPhotoEarlyS3StreamDoesNotPublishOrConsumeParts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s3Server := newPhotoUploadS3Server()
	blobs := newPhotoUploadS3(t, s3Server)
	s := openStoreWithPhotoUploadS3(t, blobs)
	user, err := s.CreateUser(ctx, "+15551990112")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const clientFileID = int64(80102)
	body := photoUploadJPEGOfSize(t, 10708)
	h := testHandlers(s)
	h.blobs = blobs
	h.uploadGrantTTL = time.Hour
	if _, err := h.handleSaveFilePart(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.UploadSaveFilePartRequest{
			FileID: clientFileID, FilePart: 0, Bytes: body,
		}),
	}); err != nil {
		t.Fatalf("upload.saveFilePart: %v", err)
	}
	before, err := s.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling before assembly: %v", err)
	}
	h.blobs = photoUploadEarlyPutStore{Store: blobs, expected: int64(len(body))}
	h.maxUserStorageBytes = 1 << 30
	_, err = h.handleUploadMedia(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.MessagesUploadMediaRequest{
			Peer: &tg.InputPeerSelf{},
			Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
				ID: clientFileID, Parts: 1, Name: "photo.jpg", MD5Checksum: photoUploadMD5(body),
			}},
		}),
	})
	if err == nil {
		t.Fatal("messages.uploadMedia succeeded with a stream that ended early")
	}
	rpcErr, ok := errors.AsType[*tgerr.Error](err)
	if !ok || rpcErr.Message != "INTERNAL" {
		t.Fatalf("messages.uploadMedia early stream error = %v, want generic INTERNAL", err)
	}
	after, err := s.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling after assembly: %v", err)
	}
	if after != before+1 {
		t.Fatalf("file id ceiling after failed assembly = %d, want one allocated row after %d", after, before)
	}
	files, err := s.FilesByIDs(ctx, []int64{after})
	if err != nil {
		t.Fatalf("load failed photo row: %v", err)
	}
	if file, found := files[after]; found {
		t.Fatalf("failed photo was published as stored: %+v", file)
	}
	if count, _, _, err := s.UploadPartsSummary(ctx, user.ID, clientFileID); err != nil || count != 1 {
		t.Fatalf("upload parts after failed assembly = %d, err=%v; want one retryable part", count, err)
	}
	part, found, err := s.UploadPart(ctx, user.ID, clientFileID, 0)
	if err != nil || !found || !bytes.Equal(part, body) {
		t.Fatalf("retained upload part = %d bytes, found %v, err %v; want original %d bytes", len(part), found, err, len(body))
	}
	puts := s3Server.filePutSnapshot()
	if len(puts) != 1 || puts[0].contentLength != int64(len(body)) || puts[0].readErr == nil || len(puts[0].body) >= len(body) {
		t.Fatalf("short assembled S3 PUTs = %+v; want one request declaring %d bytes with a shorter body", puts, len(body))
	}
	if _, found := s3Server.fileObject(blob.Key(after)); found {
		t.Fatalf("failed assembled object %q was stored", blob.Key(after))
	}
}
