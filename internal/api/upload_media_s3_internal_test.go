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
	key           string
	contentLength int64
	body          []byte
	readErr       error
}

type photoUploadS3Server struct {
	mu              sync.Mutex
	objects         map[string][]byte
	puts            []photoUploadS3Put
	filePuts        []photoUploadS3Put
	filePutObserved chan struct{}
	gets            int
}

func newPhotoUploadS3Server() *photoUploadS3Server {
	return &photoUploadS3Server{
		objects:         make(map[string][]byte),
		filePutObserved: make(chan struct{}, 1),
	}
}

func (s *photoUploadS3Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		prefix := strings.TrimPrefix(r.URL.Query().Get("prefix"), "tenant/")
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		if _, err := io.WriteString(w, `<ListBucketResult><IsTruncated>false</IsTruncated>`); err != nil {
			return
		}
		for key, body := range s.objects {
			if strings.HasPrefix(key, prefix) {
				if _, err := fmt.Fprintf(w, `<Contents><Key>tenant/%s</Key><Size>%d</Size><LastModified>2020-01-01T00:00:00Z</LastModified></Contents>`, key, len(body)); err != nil {
					return
				}
			}
		}
		if _, err := io.WriteString(w, `</ListBucketResult>`); err != nil {
			return
		}
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, photoUploadS3Root)
	if !ok || key == "" {
		http.NotFound(w, r)
		return
	}

	switch r.Method {
	case http.MethodPut:
		body, readErr := io.ReadAll(r.Body)
		put := photoUploadS3Put{
			key:           key,
			contentLength: r.ContentLength,
			body:          append([]byte(nil), body...),
			readErr:       readErr,
		}
		_, isFile := blob.ParseKey(key)
		s.mu.Lock()
		s.puts = append(s.puts, put)
		if isFile {
			s.filePuts = append(s.filePuts, put)
		}
		if readErr == nil {
			s.objects[key] = append([]byte(nil), body...)
		}
		s.mu.Unlock()
		if isFile {
			select {
			case s.filePutObserved <- struct{}{}:
			default:
			}
		}
		if readErr != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		s.mu.Lock()
		s.gets++
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

func (s *photoUploadS3Server) waitForFilePuts(ctx context.Context, count int) error {
	for {
		s.mu.Lock()
		observed := len(s.filePuts)
		s.mu.Unlock()
		if observed >= count {
			return nil
		}
		select {
		case <-s.filePutObserved:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *photoUploadS3Server) putSnapshot() []photoUploadS3Put {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]photoUploadS3Put(nil), s.puts...)
}

func (s *photoUploadS3Server) getCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets
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

func TestUploadMediaPartsAssemblyAndRangedGetUseEncryptedS3Objects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s3Server := newPhotoUploadS3Server()
	remote := newPhotoUploadS3(t, s3Server)
	ring, err := blob.NewKeyRing(1, map[uint32][]byte{1: bytes.Repeat([]byte{0x61}, blob.MediaKeyLen)})
	if err != nil {
		t.Fatalf("new media key ring: %v", err)
	}
	sealed, err := blob.NewEncrypted(remote, ring, blob.DefaultChunkSize)
	if err != nil {
		t.Fatalf("new encrypted S3 store: %v", err)
	}
	s := openStoreWithPhotoUploadS3(t, sealed)
	user, err := s.CreateUser(ctx, "+15551990113")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	const clientFileID = int64(80103)
	const bodySize = 3*blob.DefaultChunkSize + 123
	body := bytes.Repeat([]byte{0x1f, 0x2e, 0x3d, 0x4c}, (bodySize+3)/4)[:bodySize]
	h := testHandlers(s)
	h.blobs = sealed
	h.maxUserStorageBytes = int64(len(body))
	h.uploadGrantTTL = time.Hour
	if _, err := h.handleSaveFilePart(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.UploadSaveFilePartRequest{
			FileID: clientFileID, FilePart: 0, Bytes: body,
		}),
	}); err != nil {
		t.Fatalf("upload.saveFilePart: %v", err)
	}

	partPuts := s3Server.putSnapshot()
	if len(partPuts) != 1 || !strings.HasPrefix(partPuts[0].key, blob.PartsPrefix) {
		t.Fatalf("upload part S3 PUTs = %+v, want one part object", partPuts)
	}
	if partPuts[0].readErr != nil || partPuts[0].contentLength != int64(len(partPuts[0].body)) ||
		!bytes.HasPrefix(partPuts[0].body, []byte("TGME")) || len(partPuts[0].body) <= len(body) {
		t.Fatalf("upload part object = length %d, content length %d, error %v; want known-length envelope larger than %d bytes",
			len(partPuts[0].body), partPuts[0].contentLength, partPuts[0].readErr, len(body))
	}

	before, err := s.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling before send: %v", err)
	}
	_, err = h.handleSendMedia(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.MessagesSendMediaRequest{
			Peer: &tg.InputPeerSelf{},
			Media: &tg.InputMediaUploadedDocument{
				File:     &tg.InputFile{ID: clientFileID, Parts: 1, Name: "encrypted.bin"},
				MimeType: "application/octet-stream",
			},
			Message:  "encrypted media",
			RandomID: 80103,
		}),
	})
	if err != nil {
		t.Fatalf("messages.sendMedia: %v", err)
	}
	fileID, err := s.AllocatedFileIDCeiling(ctx)
	if err != nil {
		t.Fatalf("file id ceiling after send: %v", err)
	}
	if fileID != before+1 {
		t.Fatalf("file id ceiling = %d, want one assembled file after %d", fileID, before)
	}
	files, err := s.FilesByIDs(ctx, []int64{fileID})
	if err != nil {
		t.Fatalf("load assembled file: %v", err)
	}
	file, found := files[fileID]
	if !found || !file.Stored || file.Size != int64(len(body)) {
		t.Fatalf("assembled file = %+v (found %v), want stored plaintext size %d", file, found, len(body))
	}
	if count, _, _, err := s.UploadPartsSummary(ctx, user.ID, clientFileID); err != nil || count != 0 {
		t.Fatalf("upload parts after assembly = %d, err=%v; want removed", count, err)
	}

	puts := s3Server.putSnapshot()
	if len(puts) != 2 || !strings.HasPrefix(puts[0].key, blob.PartsPrefix) || puts[1].key != blob.Key(fileID) {
		t.Fatalf("S3 PUTs = %+v, want encrypted part and assembled file", puts)
	}
	for _, put := range puts {
		if put.readErr != nil || put.contentLength != int64(len(put.body)) || !bytes.HasPrefix(put.body, []byte("TGME")) {
			t.Fatalf("S3 object %q = length %d, content length %d, read error %v; want known-length envelope",
				put.key, len(put.body), put.contentLength, put.readErr)
		}
	}
	assembled, found := s3Server.fileObject(blob.Key(fileID))
	if !found || len(assembled) <= len(body) || !bytes.HasPrefix(assembled, []byte("TGME")) {
		t.Fatalf("assembled S3 object = %d bytes, found %v; want envelope larger than %d bytes", len(assembled), found, len(body))
	}

	const offset int64 = 12345
	const limit int64 = 21000
	got, err := sealed.ReadAt(ctx, blob.Key(fileID), offset, limit)
	if err != nil || !bytes.Equal(got, body[offset:offset+limit]) {
		t.Fatalf("encrypted ranged read = %d bytes, err %v; want bytes [%d:%d] identical to upload", len(got), err, offset, offset+limit)
	}
	rpc, err := h.handleGetFile(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: file.ID, AccessHash: file.AccessHash},
			Offset:   offset,
			Limit:    int(limit),
		}),
	})
	if err != nil {
		t.Fatalf("upload.getFile range: %v", err)
	}
	window, ok := rpc.(*tg.UploadFile)
	if !ok || !bytes.Equal(window.Bytes, body[offset:offset+limit]) {
		t.Fatalf("upload.getFile result = %T, want exact %d-byte range", rpc, limit)
	}
	getsBeforeEOF := s3Server.getCount()
	eof, err := h.handleGetFile(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: file.ID, AccessHash: file.AccessHash},
			Offset:   file.Size,
			Limit:    1024,
		}),
	})
	if err != nil {
		t.Fatalf("upload.getFile at EOF: %v", err)
	}
	if result, ok := eof.(*tg.UploadFile); !ok || len(result.Bytes) != 0 {
		t.Fatalf("upload.getFile at EOF = %T, want empty upload file", eof)
	}
	if got := s3Server.getCount(); got != getsBeforeEOF {
		t.Fatalf("S3 GETs at EOF = %d, want unchanged at %d", got, getsBeforeEOF)
	}

	s3Server.mu.Lock()
	tampered := append([]byte(nil), s3Server.objects[blob.Key(fileID)]...)
	tampered[0] ^= 0xff
	s3Server.objects[blob.Key(fileID)] = tampered
	s3Server.mu.Unlock()
	_, err = h.handleGetFile(&mtproto.Request{
		Ctx: ctx, UserID: user.ID, Buf: encodePhotoUploadRequest(t, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{ID: file.ID, AccessHash: file.AccessHash},
			Offset:   0,
			Limit:    1024,
		}),
	})
	rpcErr, ok := errors.AsType[*tgerr.Error](err)
	if !ok || rpcErr.Message != "INTERNAL" {
		t.Fatalf("tampered envelope download error = %v, want generic INTERNAL", err)
	}
	if strings.Contains(rpcErr.Message, "test-bucket") || strings.Contains(rpcErr.Message, blob.Key(fileID)) {
		t.Fatalf("tampered envelope client error exposes S3 location: %q", rpcErr.Message)
	}

	var physicalBytes int64
	if err := sealed.WalkPrefix(ctx, blob.Key(fileID), func(entry blob.Entry) error {
		physicalBytes = entry.Size
		return nil
	}); err != nil {
		t.Fatalf("walk assembled envelope: %v", err)
	}
	if physicalBytes <= file.Size {
		t.Fatalf("physical size = %d, plaintext file size = %d; want envelope overhead included", physicalBytes, file.Size)
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
	waitCtx, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelWait()
	if err := s3Server.waitForFilePuts(waitCtx, 1); err != nil {
		t.Fatalf("wait for truncated assembled S3 PUT: %v", err)
	}
	puts := s3Server.filePutSnapshot()
	if len(puts) != 1 || puts[0].contentLength != int64(len(body)) || puts[0].readErr == nil || len(puts[0].body) >= len(body) {
		t.Fatalf("short assembled S3 PUTs = %+v; want one request declaring %d bytes with a shorter body", puts, len(body))
	}
	if _, found := s3Server.fileObject(blob.Key(after)); found {
		t.Fatalf("failed assembled object %q was stored", blob.Key(after))
	}
}
