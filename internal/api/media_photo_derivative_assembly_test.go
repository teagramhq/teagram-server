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
	"reflect"
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
	workerPath := buildPhotoWorker(t, dir)
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

func buildPhotoWorker(t *testing.T, dir string) string {
	t.Helper()
	workerPath := filepath.Join(dir, "photothumb")
	buildCtx, cancelBuild := context.WithTimeout(t.Context(), time.Minute)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", workerPath, "../../cmd/photothumb") // #nosec G204 -- fixed local worker source and test temp output.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build photo worker: %v\n%s", err, output)
	}
	return workerPath
}

func newPhotoWorkerRecorderWithFirstFailure(t *testing.T, firstFailure string) photoWorkerRecorder {
	t.Helper()
	dir := t.TempDir()
	workerPath := buildPhotoWorker(t, dir)
	inputPath := filepath.Join(dir, "worker-input.jpg")
	callsPath := filepath.Join(dir, "worker-calls")
	firstFailurePath := filepath.Join(dir, "first-failure-used")
	wrapperPath := filepath.Join(dir, "recording-photothumb")
	firstFailureCommand := map[string]string{
		"timeout": "sleep 10",
		"worker":  "exit 1",
		"frame":   "printf '\\000'; exit 0",
	}[firstFailure]
	if firstFailureCommand == "" {
		t.Fatalf("unsupported first worker failure %q", firstFailure)
	}
	script := fmt.Sprintf("#!/bin/sh\nset -eu\nprintf x >> %s\n/bin/cat > %s\nif [ ! -f %s ]; then\n  : > %s\n  %s\nfi\nexec %s \"$@\" < %s\n",
		shellQuote(callsPath), shellQuote(inputPath), shellQuote(firstFailurePath), shellQuote(firstFailurePath), firstFailureCommand,
		shellQuote(workerPath), shellQuote(inputPath))
	if err := os.WriteFile(wrapperPath, []byte(script), 0o600); err != nil {
		t.Fatalf("write fail-once worker: %v", err)
	}
	if err := os.Chmod(wrapperPath, 0o700); err != nil { // #nosec G302 -- the supervisor requires an executable test worker.
		t.Fatalf("make fail-once worker executable: %v", err)
	}
	supervisor, err := photothumb.NewForTesting(wrapperPath)
	if err != nil {
		t.Fatalf("create fail-once worker supervisor: %v", err)
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
	assertPhotoMetadataMatchesStoredDerivatives(t, photo, stored.PhotoDerivatives, 1600, 1600)

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

func TestPhotoAssemblySupervisorFailuresPublishOriginalAndRecover(t *testing.T) {
	cases := []struct {
		name         string
		firstFailure string
		wantMetrics  photothumb.MetricSnapshot
	}{
		{name: "busy admission", wantMetrics: photothumb.MetricSnapshot{Busy: 1}},
		{name: "worker timeout", firstFailure: "timeout", wantMetrics: photothumb.MetricSnapshot{Timeout: 1}},
		{name: "worker death", firstFailure: "worker", wantMetrics: photothumb.MetricSnapshot{Worker: 1}},
		{name: "rejected frame", firstFailure: "frame", wantMetrics: photothumb.MetricSnapshot{Output: 1}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newMediaAssemblyHarness(t)
			recorder := newPhotoWorkerRecorder(t)
			if tc.firstFailure != "" {
				recorder = newPhotoWorkerRecorderWithFirstFailure(t, tc.firstFailure)
			}
			recipient, err := h.store.CreateUser(ctx, "+15551990004")
			if err != nil {
				t.Fatalf("create photo recipient: %v", err)
			}

			var heldLease *photothumb.Lease
			if tc.firstFailure == "" {
				var reason photothumb.FailureReason
				heldLease, reason = recorder.supervisor.TryAcquire(h.user.ID)
				if reason != photothumb.FailureNone || heldLease == nil {
					t.Fatalf("hold derivative admission = (%v, %v), want a lease", heldLease, reason)
				}
				defer heldLease.Close()
			}

			original := jpegPhotoPayload(t, 640, 480)
			clientFileID := int64(98201) + int64(i)*10
			randomID := int64(98202) + int64(i)*10
			result, err := sendPhotoWithSupervisor(t, h, recorder.supervisor, recipient.ID, clientFileID, randomID, original)
			if err != nil {
				t.Fatalf("photo send after %s: %v", tc.name, err)
			}
			photo := photoOfMessage(t, messageOf(t, result))
			assertOriginalOnlyPhotoSend(t, h, photo, original)
			if tc.firstFailure != "" && !bytes.Equal(recorder.input(t), original) {
				t.Fatal("failing worker input differs from the accepted original bytes")
			}
			if got := recorder.calls(t); got != boolToInt(tc.firstFailure != "") {
				t.Fatalf("worker calls after fallback = %d, want %d", got, boolToInt(tc.firstFailure != ""))
			}
			if got := recorder.supervisor.Metrics(); got != tc.wantMetrics {
				t.Fatalf("bounded fallback metrics = %+v, want %+v", got, tc.wantMetrics)
			}
			if heldLease != nil {
				heldLease.Close()
			}

			healthyBody := jpegPhotoPayload(t, 800, 600)
			healthyResult, err := sendPhotoWithSupervisor(t, h, recorder.supervisor, recipient.ID, clientFileID+1, randomID+1, healthyBody)
			if err != nil {
				t.Fatalf("healthy photo send after %s: %v", tc.name, err)
			}
			healthyPhoto := photoOfMessage(t, messageOf(t, healthyResult))
			files, err := h.store.FilesByIDs(ctx, []int64{healthyPhoto.ID})
			if err != nil {
				t.Fatalf("load healthy photo after %s: %v", tc.name, err)
			}
			stored, ok := files[healthyPhoto.ID]
			if !ok || !stored.Stored || stored.PhotoDerivatives == nil {
				t.Fatalf("healthy photo after %s = %+v, present %v; want stored derivatives", tc.name, stored, ok)
			}
			assertPhotoMetadataMatchesStoredDerivatives(t, healthyPhoto, stored.PhotoDerivatives, 800, 600)
			if got := recorder.calls(t); got != boolToInt(tc.firstFailure != "")+1 {
				t.Fatalf("worker calls after healthy retry = %d, want %d", got, boolToInt(tc.firstFailure != "")+1)
			}
			if got := recorder.supervisor.Metrics(); got != tc.wantMetrics {
				t.Fatalf("metrics after healthy retry = %+v, want unchanged %+v", got, tc.wantMetrics)
			}
		})
	}
}

func TestChannelPhotoRetryKeepsPersistedDerivativesWithoutDecodingAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	recorder := newPhotoWorkerRecorder(t)
	member, err := h.store.CreateUser(ctx, "+15551990101")
	if err != nil {
		t.Fatalf("create channel member: %v", err)
	}
	channel, err := h.store.CreateChannel(ctx, h.user.ID, "Photo derivative retry", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, h.store, channel, member.ID)

	body := jpegPhotoPayload(t, 640, 480)
	const clientFileID, randomID = int64(98301), int64(98302)
	saveParts(t, h.store, member.ID, clientFileID, body)
	request := &tg.MessagesSendMediaRequest{
		Peer:     channelPeer(member.ID, channel.ID),
		Media:    uploadedPhoto(clientFileID, 1, "photo.jpg", jpegPhotoMD5(body)),
		Message:  "retry derivative photo",
		RandomID: randomID,
	}
	first, err := api.SendMediaForTestWithPhotoThumbs(h.store, member.ID, h.local, api.TestMaxUserStorageBytes, recorder.supervisor, request)
	if err != nil {
		t.Fatalf("first channel photo send: %v", err)
	}
	firstPost, firstPts, firstPhoto, err := parseChannelPhotoPost(first)
	if err != nil {
		t.Fatalf("parse first channel photo send: %v", err)
	}
	files, err := h.store.FilesByIDs(ctx, []int64{firstPhoto.ID})
	if err != nil {
		t.Fatalf("load first channel photo: %v", err)
	}
	winner, ok := files[firstPhoto.ID]
	if !ok || !winner.Stored || winner.PhotoDerivatives == nil {
		t.Fatalf("first channel photo file = %+v, present %v; want stored derivatives", winner, ok)
	}
	assertPhotoMetadataMatchesStoredDerivatives(t, firstPhoto, winner.PhotoDerivatives, 640, 480)
	if got := recorder.calls(t); got != 1 {
		t.Fatalf("worker calls after first channel send = %d, want 1", got)
	}

	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	derivativeBefore := readPersistedPhotoDerivative(t, ctx, conn, firstPhoto.ID)
	postsBefore := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, h.dsn)

	retried, err := api.SendMediaForTestWithPhotoThumbs(h.store, member.ID, h.local, api.TestMaxUserStorageBytes, recorder.supervisor, request)
	if err != nil {
		t.Fatalf("channel photo retry: %v", err)
	}
	retryPost, retryPts, retryPhoto, err := parseChannelPhotoPost(retried)
	if err != nil {
		t.Fatalf("parse retried channel photo: %v", err)
	}
	if retryPost.ID != firstPost.ID || retryPts != firstPts {
		t.Fatalf("retry = post %d pts %d, want original post %d pts %d", retryPost.ID, retryPts, firstPost.ID, firstPts)
	}
	assertSamePhotoMetadata(t, retryPhoto, firstPhoto, "retry")
	if got := recorder.calls(t); got != 1 {
		t.Fatalf("worker calls after retry = %d, want 1: a retry must not decode again", got)
	}
	if got := channelWriteStats(t, conn, channel.ID); got != postsBefore {
		t.Fatalf("channel writes after retry = %+v, want %+v", got, postsBefore)
	}
	if got := countFiles(t, ctx, h.dsn); got != filesBefore {
		t.Fatalf("file rows after retry = %d, want %d", got, filesBefore)
	}
	files, err = h.store.FilesByIDs(ctx, []int64{firstPhoto.ID})
	if err != nil {
		t.Fatalf("reload channel photo after retry: %v", err)
	}
	retriedWinner, ok := files[firstPhoto.ID]
	if !ok || !retriedWinner.Stored || retriedWinner.PhotoDerivatives == nil {
		t.Fatalf("retried channel photo file = %+v, present %v; want stored derivatives", retriedWinner, ok)
	}
	if !reflect.DeepEqual(derivativeBefore, readPersistedPhotoDerivative(t, ctx, conn, firstPhoto.ID)) {
		t.Fatal("persisted derivative bytes or metadata changed after retry")
	}
	assertPhotoMetadataMatchesStoredDerivatives(t, retryPhoto, retriedWinner.PhotoDerivatives, 640, 480)
}

func TestChannelPhotoConcurrentDedupErasesOnlyOrphanDerivatives(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newMediaAssemblyHarness(t)
	workerPath := buildPhotoWorker(t, t.TempDir())
	firstSupervisor, err := photothumb.NewForTesting(workerPath)
	if err != nil {
		t.Fatalf("create first photo supervisor: %v", err)
	}
	secondSupervisor, err := photothumb.NewForTesting(workerPath)
	if err != nil {
		t.Fatalf("create second photo supervisor: %v", err)
	}
	member, err := h.store.CreateUser(ctx, "+15551990102")
	if err != nil {
		t.Fatalf("create channel member: %v", err)
	}
	channel, err := h.store.CreateChannel(ctx, h.user.ID, "Photo derivative race", "", true)
	if err != nil {
		t.Fatalf("create megagroup: %v", err)
	}
	joinChannelByInvite(t, h.store, channel, member.ID)

	conn, err := pgx.Connect(ctx, h.dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close: %v", err)
		}
	}()
	before := channelWriteStats(t, conn, channel.ID)
	filesBefore := countFiles(t, ctx, h.dsn)
	barrier := holdChannelStateBarrier(t, ctx, h.dsn, channel.ID)

	firstBody := jpegWithComment(t, jpegPhotoPayload(t, 640, 480), "first concurrent upload")
	secondBody := jpegWithComment(t, jpegPhotoPayload(t, 800, 600), "second concurrent upload with different length")
	if len(firstBody) == len(secondBody) {
		t.Fatalf("test uploads are both %d bytes, need distinct sizes to identify the orphan", len(firstBody))
	}
	const firstFileID, secondFileID, randomID = int64(98401), int64(98402), int64(98403)
	saveParts(t, h.store, member.ID, firstFileID, firstBody)
	saveParts(t, h.store, member.ID, secondFileID, secondBody)
	type sendResult struct {
		post  *tg.Message
		pts   int
		photo *tg.Photo
		err   error
	}
	send := func(fileID int64, body []byte, supervisor *photothumb.Supervisor) chan sendResult {
		done := make(chan sendResult, 1)
		go func() {
			request := &tg.MessagesSendMediaRequest{
				Peer:     channelPeer(member.ID, channel.ID),
				Media:    uploadedPhoto(fileID, 1, "photo.jpg", jpegPhotoMD5(body)),
				Message:  "same derivative random id",
				RandomID: randomID,
			}
			result, sendErr := api.SendMediaForTestWithPhotoThumbs(h.store, member.ID, h.local, api.TestMaxUserStorageBytes, supervisor, request)
			if sendErr != nil {
				done <- sendResult{err: sendErr}
				return
			}
			post, pts, photo, parseErr := parseChannelPhotoPost(result)
			done <- sendResult{post: post, pts: pts, photo: photo, err: parseErr}
		}()
		return done
	}
	firstSend := send(firstFileID, firstBody, firstSupervisor)
	secondSend := send(secondFileID, secondBody, secondSupervisor)
	waitForChannelStateWaiters(t, ctx, conn, 2)
	if _, err := barrier.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("release channel state barrier: %v", err)
	}
	collectCtx, cancelCollect := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelCollect()
	collect := func(done chan sendResult, label string) sendResult {
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("%s concurrent send: %v", label, got.err)
			}
			return got
		case <-collectCtx.Done():
			t.Fatalf("waiting for %s concurrent send: %s", label, collectCtx.Err())
			return sendResult{}
		}
	}
	a := collect(firstSend, "first")
	b := collect(secondSend, "second")
	if a.post.ID != b.post.ID || a.pts != b.pts {
		t.Fatalf("concurrent sends returned post %d pts %d and post %d pts %d, want one post at one pts", a.post.ID, a.pts, b.post.ID, b.pts)
	}
	assertSamePhotoMetadata(t, b.photo, a.photo, "second concurrent response")
	if got := channelWriteStats(t, conn, channel.ID); got.messages != before.messages+1 || got.events != before.events+1 {
		t.Fatalf("channel writes after concurrent dedup = %+v, want one post and event over %+v", got, before)
	}
	if got := countFiles(t, ctx, h.dsn); got != filesBefore+2 {
		t.Fatalf("file rows after concurrent assembly = %d, want %d", got, filesBefore+2)
	}
	if len(a.photo.Sizes) != 3 {
		t.Fatalf("winning photo sizes = %d, want stripped, m, and original", len(a.photo.Sizes))
	}
	original, ok := a.photo.Sizes[len(a.photo.Sizes)-1].(*tg.PhotoSize)
	if !ok || (original.W != 640 && original.W != 800) {
		t.Fatalf("winning original metadata = %#v, want 640 or 800 pixels wide", a.photo.Sizes[len(a.photo.Sizes)-1])
	}
	winnerBody, loserBody := firstBody, secondBody
	if original.W == 800 {
		winnerBody, loserBody = secondBody, firstBody
	}
	var loserFileID int64
	if err := conn.QueryRow(ctx, `SELECT id FROM files WHERE id <> $1 AND size = $2`, a.photo.ID, len(loserBody)).Scan(&loserFileID); err != nil {
		t.Fatalf("find losing assembled file: %v", err)
	}
	files, err := h.store.FilesByIDs(ctx, []int64{a.photo.ID, loserFileID})
	if err != nil {
		t.Fatalf("load concurrent photo assemblies: %v", err)
	}
	winner, winnerExists := files[a.photo.ID]
	loser, loserExists := files[loserFileID]
	if !winnerExists || !winner.Stored || winner.PhotoDerivatives == nil || !loserExists || !loser.Stored || loser.PhotoDerivatives == nil {
		t.Fatalf("winner file=%+v present=%v; loser file=%+v present=%v; want both completed derivative assemblies", winner, winnerExists, loser, loserExists)
	}
	assertPhotoMetadataMatchesStoredDerivatives(t, a.photo, winner.PhotoDerivatives, original.W, original.H)
	winnerDerivative := readPersistedPhotoDerivative(t, ctx, conn, a.photo.ID)
	_ = readPersistedPhotoDerivative(t, ctx, conn, loserFileID)
	var derivativesBefore int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM photo_derivatives`).Scan(&derivativesBefore); err != nil {
		t.Fatalf("count derivative rows before erasure: %v", err)
	}
	if derivativesBefore != 2 {
		t.Fatalf("derivative rows before erasure = %d, want winner and orphan", derivativesBefore)
	}

	counts, err := h.store.SweepMediaErasure(ctx, time.Now().Add(time.Hour), store.ErasureScanBatch)
	if err != nil {
		t.Fatalf("sweep media erasure: %v", err)
	}
	if counts.Erased != 1 || counts.ErasedBytes != int64(len(loserBody)) {
		t.Fatalf("sweep counts = %+v, want one orphan of %d original bytes", counts, len(loserBody))
	}
	if got := photoDerivativeRowCount(t, ctx, conn, a.photo.ID); got != 1 {
		t.Fatalf("winning derivative rows after erasure = %d, want 1", got)
	}
	if got := photoDerivativeRowCount(t, ctx, conn, loserFileID); got != 0 {
		t.Fatalf("orphan derivative rows after erasure = %d, want cascade deletion", got)
	}
	var derivativesAfter int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM photo_derivatives`).Scan(&derivativesAfter); err != nil {
		t.Fatalf("count derivative rows after erasure: %v", err)
	}
	if derivativesAfter != 1 {
		t.Fatalf("derivative rows after erasure = %d, want only the posted photo's row", derivativesAfter)
	}
	if !reflect.DeepEqual(winnerDerivative, readPersistedPhotoDerivative(t, ctx, conn, a.photo.ID)) {
		t.Fatal("posted photo derivatives changed while erasing the orphan")
	}
	assertChannelPhotoDownload(t, h.store, h.local, h.user.ID, a.photo, winnerBody, "x")
}

type persistedPhotoDerivative struct {
	width    int32
	height   int32
	size     int32
	mBytes   []byte
	stripped []byte
}

func readPersistedPhotoDerivative(t *testing.T, ctx context.Context, conn *pgx.Conn, fileID int64) persistedPhotoDerivative {
	t.Helper()
	var got persistedPhotoDerivative
	if err := conn.QueryRow(ctx, `SELECT m_width, m_height, m_size, m_bytes, stripped FROM photo_derivatives WHERE file_id = $1`, fileID).
		Scan(&got.width, &got.height, &got.size, &got.mBytes, &got.stripped); err != nil {
		t.Fatalf("read derivative row for file %d: %v", fileID, err)
	}
	if got.width <= 0 || got.height <= 0 || got.size <= 0 || len(got.mBytes) != int(got.size) || len(got.stripped) == 0 {
		t.Fatalf("derivative row for file %d is incomplete: %+v", fileID, got)
	}
	return got
}

func photoDerivativeRowCount(t *testing.T, ctx context.Context, conn *pgx.Conn, fileID int64) int64 {
	t.Helper()
	var count int64
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM photo_derivatives WHERE file_id = $1`, fileID).Scan(&count); err != nil {
		t.Fatalf("count derivative rows for file %d: %v", fileID, err)
	}
	return count
}

func assertSamePhotoMetadata(t *testing.T, got, want *tg.Photo, label string) {
	t.Helper()
	if got.ID != want.ID || got.AccessHash != want.AccessHash || !bytes.Equal(got.FileReference, want.FileReference) || got.Date != want.Date || got.DCID != want.DCID {
		t.Fatalf("%s photo identity or metadata differs from winner: got id/hash/date/dc %d/%d/%d/%d, want %d/%d/%d/%d", label, got.ID, got.AccessHash, got.Date, got.DCID, want.ID, want.AccessHash, want.Date, want.DCID)
	}
	if !reflect.DeepEqual(got.Sizes, want.Sizes) {
		t.Fatalf("%s photo sizes = %#v, want winning sizes %#v", label, got.Sizes, want.Sizes)
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func assertOriginalOnlyPhotoSend(t *testing.T, h *mediaAssemblyHarness, photo *tg.Photo, body []byte) {
	t.Helper()
	if len(photo.Sizes) != 1 {
		t.Fatalf("original-only photo has %d sizes, want just the original", len(photo.Sizes))
	}
	files, err := h.store.FilesByIDs(t.Context(), []int64{photo.ID})
	if err != nil {
		t.Fatalf("load original-only photo: %v", err)
	}
	file, ok := files[photo.ID]
	if !ok || !file.Stored || file.PhotoDerivatives != nil {
		t.Fatalf("original-only file = %+v, present %v; want stored without derivatives", file, ok)
	}
	got, err := h.local.ReadAt(t.Context(), blob.Key(photo.ID), 0, int64(len(body)))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("original bytes after fallback: read err %v, got %d bytes, want identical %d bytes", err, len(got), len(body))
	}
}

func assertPhotoMetadataMatchesStoredDerivatives(t *testing.T, photo *tg.Photo, stored *store.PhotoDerivatives, width, height int) {
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
	if !ok || original.W != width || original.H != height || original.Size == 0 {
		t.Fatalf("photo original metadata = %#v, want %dx%d", photo.Sizes[2], width, height)
	}
}
