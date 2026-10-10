//go:build linux

package photothumb //nolint:testpackage // Tests assert the bounded upload-feed buffer without exporting internals.

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestInputCaptureTransfersExactBytesWithoutCopying(t *testing.T) {
	capture := NewInputCapture()
	want := bytes.Repeat([]byte{0x4a}, 1024)
	if n, err := capture.Write(want); err != nil || n != len(want) {
		t.Fatalf("capture write = (%d, %v), want %d bytes", n, err, len(want))
	}
	buffer := capture.buffer
	got, ok := capture.Finish(int64(len(want)))
	if !ok || !bytes.Equal(got, want) {
		t.Fatalf("finished capture = (%d bytes, %t), want exact input", len(got), ok)
	}
	if len(got) == 0 || &got[0] != &buffer[0] {
		t.Fatal("finished capture copied the bounded upload buffer")
	}
	if len(capture.buffer) != 0 || cap(capture.buffer) != 0 {
		t.Fatalf("capture retained %d bytes with capacity %d after finish", len(capture.buffer), cap(capture.buffer))
	}
}

func TestInputCaptureDiscardsOnOverflowAndContinuesUploadFeed(t *testing.T) {
	capture := NewInputCapture()
	chunk := bytes.Repeat([]byte{0x52}, 1<<20)
	for written := 0; written < maxEncodedBytes; written += len(chunk) {
		if n, err := capture.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("bounded capture write = (%d, %v), want %d bytes", n, err, len(chunk))
		}
		if len(capture.buffer) > maxEncodedBytes || cap(capture.buffer) > maxEncodedBytes {
			t.Fatalf("capture grew to len=%d cap=%d, limit %d", len(capture.buffer), cap(capture.buffer), maxEncodedBytes)
		}
	}
	if len(capture.buffer) != maxEncodedBytes {
		t.Fatalf("buffered bytes = %d, want %d", len(capture.buffer), maxEncodedBytes)
	}
	if n, err := capture.Write([]byte{0x33}); err != nil || n != 1 {
		t.Fatalf("overflow write = (%d, %v), want accepted discard", n, err)
	}
	if len(capture.buffer) != 0 || cap(capture.buffer) != 0 {
		t.Fatalf("overflow retained len=%d cap=%d, want released buffer", len(capture.buffer), cap(capture.buffer))
	}
	if n, err := capture.Write(chunk); err != nil || n != len(chunk) {
		t.Fatalf("discarded feed write = (%d, %v), want accepted discard", n, err)
	}
	if len(capture.buffer) != 0 || cap(capture.buffer) != 0 {
		t.Fatalf("discarded feed retained len=%d cap=%d", len(capture.buffer), cap(capture.buffer))
	}
	if got, ok := capture.Finish(maxEncodedBytes); ok || len(got) != 0 {
		t.Fatalf("overflow finish = (%d bytes, %t), want no captured input", len(got), ok)
	}
}

func TestProcessCapturedRejectsShortInputAndReleasesAdmission(t *testing.T) {
	capture := NewInputCapture()
	if n, err := capture.Write([]byte("partial")); err != nil || n != len("partial") {
		t.Fatalf("capture partial input = (%d, %v), want %d bytes", n, err, len("partial"))
	}
	s := newTestSupervisor("/unused", time.Second, time.Second)
	lease, reason := s.TryAcquire(900)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.ProcessCaptured(context.Background(), capture, 8, 1600, 1600)
	if reason != FailureInvalidInput || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("short captured process = (%+v, %v), want invalid input and no derivatives", result, reason)
	}
	if next, reason := s.TryAcquire(900); next == nil || reason != FailureNone {
		t.Fatalf("admission after short capture = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessCapturedSwitchesLateFeedToDiscardAfterWorkerTimeout(t *testing.T) {
	reportPath := filepath.Join(t.TempDir(), "worker-report.json")
	capture := NewInputCapture()
	if n, err := capture.Write([]byte(reportPath)); err != nil || n != len(reportPath) {
		t.Fatalf("capture worker input = (%d, %v), want %d bytes", n, err, len(reportPath))
	}
	s := newTestSupervisor(fakeWorker, 3*time.Second, 500*time.Millisecond)
	lease, reason := s.TryAcquire(901)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	type processResult struct {
		derivatives Derivatives
		reason      FailureReason
	}
	done := make(chan processResult, 1)
	go func() {
		derivatives, processReason := lease.ProcessCaptured(context.Background(), capture, int64(len(reportPath)), 1600, 1600)
		done <- processResult{derivatives: derivatives, reason: processReason}
	}()
	waitForFile(t, reportPath)

	lateFeed := bytes.Repeat([]byte{0x7f}, 4<<20)
	if n, err := capture.Write(lateFeed); err != nil || n != len(lateFeed) {
		t.Fatalf("late upload-feed write = (%d, %v), want discard without blocking", n, err)
	}
	if len(capture.buffer) != 0 || cap(capture.buffer) != 0 {
		t.Fatalf("late upload feed retained len=%d cap=%d after worker start", len(capture.buffer), cap(capture.buffer))
	}

	select {
	case result := <-done:
		if result.reason != FailureTimeout || len(result.derivatives.M) != 0 || len(result.derivatives.Stripped) != 0 {
			t.Fatalf("timed out captured process = (%+v, %v), want no derivatives", result.derivatives, result.reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out captured process did not return")
	}
	if len(capture.buffer) != 0 || cap(capture.buffer) != 0 {
		t.Fatalf("capture retained len=%d cap=%d after worker timeout", len(capture.buffer), cap(capture.buffer))
	}
}
