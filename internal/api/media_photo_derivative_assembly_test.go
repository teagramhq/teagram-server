//go:build linux

package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/photothumb"
	"github.com/teagramhq/teagram-server/internal/store"
)

type photoWorkerRecorder struct {
	supervisor *photothumb.Supervisor
	workerPath string
	inputPath  string
	callsPath  string
}

func newPhotoWorkerRecorder(t *testing.T) photoWorkerRecorder {
	t.Helper()
	dir := t.TempDir()
	workerPath := filepath.Join(dir, "photothumb")
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", workerPath, "../../cmd/photothumb") // #nosec G204 -- fixed local worker source and test temp output.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build photo worker: %v\n%s", err, output)
	}

	inputPath := filepath.Join(dir, "worker-input.jpg")
	callsPath := filepath.Join(dir, "worker-calls")
	wrapperPath := filepath.Join(dir, "recording-photothumb")
	script := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf x >> %s\n/bin/cat > %s\nexec %s \"$@\" < %s\n",
		shellQuote(callsPath), shellQuote(inputPath), shellQuote(workerPath), shellQuote(inputPath))
	if err := os.WriteFile(wrapperPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write recording worker: %v", err)
	}
	if err := os.Chmod(wrapperPath, 0o700); err != nil { // #nosec G302 -- the supervisor requires an executable test worker.
		t.Fatalf("make recording worker executable: %v", err)
	}
	supervisor, err := photothumb.NewForTesting(wrapperPath)
	if err != nil {
		t.Fatalf("create recording worker supervisor: %v", err)
	}
	return photoWorkerRecorder{supervisor: supervisor, workerPath: workerPath, inputPath: inputPath, callsPath: callsPath}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (recorder photoWorkerRecorder) calls(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(recorder.callsPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read worker call count: %v", err)
	}
	return len(b)
}

func (recorder photoWorkerRecorder) input(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(recorder.inputPath)
	if err != nil {
		t.Fatalf("read worker input: %v", err)
	}
	return b
}

func jpegWithComment(t *testing.T, jpegBytes []byte, comment string) []byte {
	t.Helper()
	if len(jpegBytes) < 2 || !bytes.Equal(jpegBytes[:2], []byte{0xff, 0xd8}) {
		t.Fatal("test fixture does not begin with a JPEG SOI marker")
	}
	segmentLength := len(comment) + 2
	if segmentLength > 65535 {
		t.Fatal("test JPEG comment is too large")
	}
	out := make([]byte, 0, len(jpegBytes)+segmentLength+2)
	out = append(out, jpegBytes[:2]...)
	segmentLength16 := uint16(segmentLength) // #nosec G115 -- checked above to fit JPEG's uint16 length.
	var encodedLength [2]byte
	binary.BigEndian.PutUint16(encodedLength[:], segmentLength16)
	out = append(out, 0xff, 0xfe)
	out = append(out, encodedLength[:]...)
	out = append(out, comment...)
	out = append(out, jpegBytes[2:]...)
	return out
}

func corruptJPEGRawEntropy(t *testing.T, jpegBytes []byte) []byte {
	t.Helper()
	sos := bytes.Index(jpegBytes, []byte{0xff, 0xda})
	eoi := bytes.LastIndex(jpegBytes, []byte{0xff, 0xd9})
	if sos < 0 || eoi <= sos+4 {
		t.Fatal("test JPEG has no usable scan")
	}
	segmentLength := int(binary.BigEndian.Uint16(jpegBytes[sos+2 : sos+4]))
	entropyStart := sos + 2 + segmentLength
	if entropyStart >= eoi {
		t.Fatalf("test JPEG scan bounds = %d..%d", entropyStart, eoi)
	}
	corrupt := append([]byte(nil), jpegBytes[:entropyStart]...)
	corrupt = append(corrupt, 0)
	corrupt = append(corrupt, jpegBytes[eoi:]...)
	return corrupt
}

func sendPhotoWithSupervisor(
	t *testing.T,
	h *mediaAssemblyHarness,
	supervisor *photothumb.Supervisor,
	recipientID, clientFileID, randomID int64,
	body []byte,
) (bin.Encoder, error) {
	t.Helper()
	saveParts(t, h.store, h.user.ID, clientFileID, body)
	return api.SendMediaForTestWithPhotoThumbs(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, supervisor, &tg.MessagesSendMediaRequest{
		Peer: api.InputPeerUser(h.user.ID, recipientID), Media: uploadedPhoto(clientFileID, 1, "photo.jpg", jpegPhotoMD5(body)), RandomID: randomID,
	})
}

func TestPhotoAssemblyCapturesPublishedBytesAndKeepsFallbacks(t *testing.T) {
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	recorder := newPhotoWorkerRecorder(t)
	recipient, err := h.store.CreateUser(ctx, "+15551990003")
	if err != nil {
		t.Fatalf("create photo recipient: %v", err)
	}

	// Two overwrites race an in-flight read. The stale reference must fail
	// before the worker starts; the retry must decode the winning bytes only.
	const clientFileID, randomID = int64(98101), int64(98102)
	base := jpegPhotoPayload(t, 1600, 1600)
	initial := jpegWithComment(t, base, "old-bytes-001")
	intermediate := jpegWithComment(t, base, "new-bytes-001")
	winner := jpegWithComment(t, base, "new-bytes-002")
	saveParts(t, h.store, h.user.ID, clientFileID, initial)
	refs, err := h.store.UploadPartRefs(ctx, h.user.ID, clientFileID)
	if err != nil || len(refs) != 1 {
		t.Fatalf("upload part refs = %v, %v; want one ref", refs, err)
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
		Peer: api.InputPeerUser(h.user.ID, recipient.ID), Media: uploadedPhoto(clientFileID, 1, "photo.jpg", jpegPhotoMD5(winner)), RandomID: randomID,
	}
	firstDone := make(chan error, 1)
	go func() {
		_, sendErr := api.SendMediaForTestWithPhotoThumbs(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, recorder.supervisor, request)
		firstDone <- sendErr
	}()
	<-h.partBlob.readStarted
	if err := h.store.SaveUploadPart(ctx, h.user.ID, clientFileID, 0, intermediate, 1<<20); err != nil {
		t.Fatalf("first overwrite: %v", err)
	}
	if err := h.store.SaveUploadPart(ctx, h.user.ID, clientFileID, 0, winner, 1<<20); err != nil {
		t.Fatalf("second overwrite: %v", err)
	}
	h.partBlob.releaseRead <- struct{}{}
	if err := <-firstDone; mediaAssemblyRPCCode(err) != "MEDIA_INVALID" {
		t.Fatalf("send racing repeated overwrites = %v, want MEDIA_INVALID", err)
	}
	if got := recorder.calls(t); got != 0 {
		t.Fatalf("worker calls after rejected stale reference = %d, want 0", got)
	}

	retried, err := api.SendMediaForTestWithPhotoThumbs(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, recorder.supervisor, request)
	if err != nil {
		t.Fatalf("retry photo send from winning upload part: %v", err)
	}
	photo := photoOfMessage(t, messageOf(t, retried))
	if photo.ID == 0 || len(photo.Sizes) != 3 {
		t.Fatalf("published photo = id %d, sizes %d; want durable i/m/original metadata", photo.ID, len(photo.Sizes))
	}
	if got := recorder.calls(t); got != 1 {
		t.Fatalf("worker calls after retry = %d, want 1", got)
	}
	captured := recorder.input(t)
	if !bytes.Equal(captured, winner) {
		t.Fatalf("worker input differs from winning upload bytes: got %d bytes, want %d", len(captured), len(winner))
	}
	storedOriginal, err := h.local.ReadAt(ctx, blob.Key(photo.ID), 0, int64(len(winner)))
	if err != nil || !bytes.Equal(storedOriginal, captured) {
		t.Fatalf("stored original differs from worker input: read err %v, got %d bytes, want %d identical bytes", err, len(storedOriginal), len(captured))
	}
	files, err := h.store.FilesByIDs(ctx, []int64{photo.ID})
	if err != nil {
		t.Fatalf("load published photo row: %v", err)
	}
	stored, ok := files[photo.ID]
	if !ok || !stored.Stored || stored.Width != 1600 || stored.Height != 1600 || stored.PhotoDerivatives == nil {
		t.Fatalf("published file = %+v, found %v; want stored photo with persisted derivatives", stored, ok)
	}
	assertPhotoMetadataMatchesStoredDerivatives(t, photo, stored.PhotoDerivatives)

	// The winning message retry returns its existing file and must not run the
	// decoder or create a second derivative row.
	duplicate, err := api.SendMediaForTestWithPhotoThumbs(h.store, h.user.ID, h.local, api.TestMaxUserStorageBytes, recorder.supervisor, request)
	if err != nil {
		t.Fatalf("duplicate photo send retry: %v", err)
	}
	duplicatePhoto := photoOfMessage(t, messageOf(t, duplicate))
	if duplicatePhoto.ID != photo.ID || recorder.calls(t) != 1 {
		t.Fatalf("duplicate retry returned photo %d with %d worker calls, want original photo %d and one call", duplicatePhoto.ID, recorder.calls(t), photo.ID)
	}
	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		t.Fatalf("connect to verify derivative row: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close derivative verification connection: %v", err)
		}
	}()
	var derivativeRows int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM photo_derivatives WHERE file_id = $1`, photo.ID).Scan(&derivativeRows); err != nil {
		t.Fatalf("count derivative rows: %v", err)
	}
	if derivativeRows != 1 {
		t.Fatalf("derivative rows after duplicate retry = %d, want 1", derivativeRows)
	}
	var persistedM, persistedStripped []byte
	if err := conn.QueryRow(ctx, `SELECT m_bytes, stripped FROM photo_derivatives WHERE file_id = $1`, photo.ID).Scan(&persistedM, &persistedStripped); err != nil {
		t.Fatalf("read persisted derivative bytes: %v", err)
	}
	verificationSupervisor, err := photothumb.NewForTesting(recorder.workerPath)
	if err != nil {
		t.Fatalf("create derivative verification supervisor: %v", err)
	}
	verificationLease, reason := verificationSupervisor.TryAcquire(h.user.ID)
	if reason != photothumb.FailureNone || verificationLease == nil {
		t.Fatalf("admit derivative verification worker = (%v, %v)", verificationLease, reason)
	}
	expected, reason := verificationLease.Process(ctx, captured, 1600, 1600)
	if reason != photothumb.FailureNone {
		t.Fatalf("recompute derivatives from captured input: %v", reason)
	}
	if !bytes.Equal(persistedM, expected.M) || !bytes.Equal(persistedStripped, expected.Stripped) {
		t.Fatalf("persisted derivative bytes differ from worker output on the captured original")
	}

	// Malformed framing is rejected before any worker process is started.
	invalid := []byte("not a JPEG")
	if _, err := sendPhotoWithSupervisor(t, h, recorder.supervisor, recipient.ID, 98103, 98104, invalid); mediaAssemblyRPCCode(err) != "MEDIA_INVALID" {
		t.Fatalf("invalid photo send = %v, want MEDIA_INVALID", err)
	}
	if got := recorder.calls(t); got != 1 {
		t.Fatalf("worker calls after invalid photo = %d, want unchanged count 1", got)
	}

	// Structurally valid entropy can still be undecodable. That remains an
	// accepted original-only send, and an image just above the qualified cap is
	// also accepted without starting the worker.
	overCeiling := jpegPhotoPayload(t, 1600, 1601)
	tooLargeImage, err := sendPhotoWithSupervisor(t, h, recorder.supervisor, recipient.ID, 98105, 98106, overCeiling)
	if err != nil {
		t.Fatalf("over-ceiling but valid JPEG send: %v", err)
	}
	tooLargePhoto := photoOfMessage(t, messageOf(t, tooLargeImage))
	tooLargeFiles, err := h.store.FilesByIDs(ctx, []int64{tooLargePhoto.ID})
	if err != nil || tooLargeFiles[tooLargePhoto.ID].PhotoDerivatives != nil {
		t.Fatalf("over-ceiling file derivatives = %+v, read err %v; want original-only", tooLargeFiles[tooLargePhoto.ID].PhotoDerivatives, err)
	}
	if got := recorder.calls(t); got != 1 {
		t.Fatalf("worker calls after over-ceiling photo = %d, want unchanged count 1", got)
	}

	corrupt := corruptJPEGRawEntropy(t, jpegPhotoPayload(t, 640, 480))
	corruptResult, err := sendPhotoWithSupervisor(t, h, recorder.supervisor, recipient.ID, 98107, 98108, corrupt)
	if err != nil {
		t.Fatalf("structurally valid entropy-corrupt JPEG send: %v", err)
	}
	corruptPhoto := photoOfMessage(t, messageOf(t, corruptResult))
	corruptFiles, err := h.store.FilesByIDs(ctx, []int64{corruptPhoto.ID})
	if err != nil || corruptFiles[corruptPhoto.ID].PhotoDerivatives != nil {
		t.Fatalf("entropy-corrupt file derivatives = %+v, read err %v; want original-only", corruptFiles[corruptPhoto.ID].PhotoDerivatives, err)
	}
	storedCorrupt, err := h.local.ReadAt(ctx, blob.Key(corruptPhoto.ID), 0, int64(len(corrupt)))
	if err != nil || !bytes.Equal(storedCorrupt, corrupt) {
		t.Fatalf("entropy-corrupt original bytes changed: read err %v, got %d bytes, want %d", err, len(storedCorrupt), len(corrupt))
	}
	if got := recorder.calls(t); got != 2 {
		t.Fatalf("worker calls after entropy-corrupt fallback = %d, want 2 total", got)
	}
	metrics := recorder.supervisor.Metrics()
	if metrics.InvalidInput != 1 || metrics.Input != 1 || metrics.Ineligible != 1 || metrics.Worker != 1 {
		t.Fatalf("bounded derivative failure metrics = %+v, want invalid-input, input, ineligible, and worker each once", metrics)
	}
}

func assertPhotoMetadataMatchesStoredDerivatives(t *testing.T, photo *tg.Photo, stored *store.PhotoDerivatives) {
	t.Helper()
	if len(photo.Sizes) != 3 {
		t.Fatalf("photo sizes = %d, want stripped, m and original", len(photo.Sizes))
	}
	stripped, ok := photo.Sizes[0].(*tg.PhotoStrippedSize)
	if !ok || stripped.Type != "i" || !bytes.Equal(stripped.Bytes, stored.Stripped) {
		t.Fatalf("photo stripped metadata = %#v, want stored immutable bytes", photo.Sizes[0])
	}
	m, ok := photo.Sizes[1].(*tg.PhotoSize)
	if !ok || m.Type != "m" || m.W != stored.MWidth || m.H != stored.MHeight || m.Size != stored.MSize {
		t.Fatalf("photo m metadata = %#v, want stored dimensions %dx%d and size %d", photo.Sizes[1], stored.MWidth, stored.MHeight, stored.MSize)
	}
	original, ok := photo.Sizes[2].(*tg.PhotoSize)
	if !ok || original.W != 1600 || original.H != 1600 || original.Size == 0 {
		t.Fatalf("photo original metadata = %#v, want 1600x1600", photo.Sizes[2])
	}
}
