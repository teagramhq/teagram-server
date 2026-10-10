//go:build linux

package photothumb //nolint:testpackage // Tests inspect PTD1 parsing and process cleanup without exporting test-only APIs.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

var realWorker string
var fakeWorker string

func newTestSupervisor(path string, totalLimit, inputLimit time.Duration) *Supervisor {
	return newSupervisorWithAdmission(path, totalLimit, inputLimit, &admissionSlots{accounts: make(map[int64]struct{})})
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "photothumb-supervisor-test-")
	if err != nil {
		writeTestMainErrorf("%v\n", err)
		os.Exit(1)
	}
	realWorker = filepath.Join(dir, "photothumb")
	buildContext, cancelBuild := context.WithTimeout(context.Background(), time.Minute)
	build := exec.CommandContext(buildContext, "go", "build", "-o", realWorker, "../../cmd/photothumb") // #nosec G204 -- fixed local worker source and test temp output.
	if output, buildErr := build.CombinedOutput(); buildErr != nil {
		cancelBuild()
		writeTestMainErrorf("build real photothumb worker: %v\n%s", buildErr, output)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			writeTestMainErrorf("remove temporary worker directory: %v\n", removeErr)
		}
		os.Exit(1)
	}
	cancelBuild()
	fakeWorker = filepath.Join(dir, "fakeworker")
	fakeBuildContext, cancelFakeBuild := context.WithTimeout(context.Background(), time.Minute)
	fakeBuild := exec.CommandContext(fakeBuildContext, "go", "build", "-o", fakeWorker, "./testdata/fakeworker.go") // #nosec G204 -- fixed test helper source and temp output.
	if output, buildErr := fakeBuild.CombinedOutput(); buildErr != nil {
		cancelFakeBuild()
		writeTestMainErrorf("build fake photothumb worker: %v\n%s", buildErr, output)
		if removeErr := os.RemoveAll(dir); removeErr != nil {
			writeTestMainErrorf("remove temporary worker directory: %v\n", removeErr)
		}
		os.Exit(1)
	}
	cancelFakeBuild()
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		writeTestMainErrorf("remove temporary worker directory: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func writeTestMainErrorf(format string, arguments ...any) {
	if _, err := fmt.Fprintf(os.Stderr, format, arguments...); err != nil {
		os.Exit(2)
	}
}

func TestTryAcquireBoundsReplicaAndAccountWorkers(t *testing.T) {
	s := newTestSupervisor("/unused", time.Second, time.Second)
	first, reason := s.TryAcquire(11)
	if reason != FailureNone || first == nil {
		t.Fatalf("first account admission = (%v, %v), want a lease", first, reason)
	}
	if lease, reason := s.TryAcquire(11); lease != nil || reason != FailureBusy {
		t.Fatalf("duplicate account admission = (%v, %v), want busy", lease, reason)
	}
	second, reason := s.TryAcquire(12)
	if reason != FailureNone || second == nil {
		t.Fatalf("second account admission = (%v, %v), want a lease", second, reason)
	}
	if lease, reason := s.TryAcquire(13); lease != nil || reason != FailureBusy {
		t.Fatalf("third replica admission = (%v, %v), want busy", lease, reason)
	}
	first.Close()
	if lease, reason := s.TryAcquire(11); reason != FailureNone || lease == nil {
		t.Fatalf("reacquire after release = (%v, %v), want a lease", lease, reason)
	} else {
		lease.Close()
	}
	second.Close()
	if got := s.Metrics().Busy; got != 2 {
		t.Fatalf("busy metric = %d, want 2", got)
	}
}

func TestSupervisorInstancesShareReplicaAdmission(t *testing.T) {
	firstSupervisor := newSupervisorWithAdmission("/unused", time.Second, time.Second, replicaAdmission)
	secondSupervisor := newSupervisorWithAdmission("/unused", time.Second, time.Second, replicaAdmission)
	busyBefore := firstSupervisor.Metrics().Busy
	first, reason := firstSupervisor.TryAcquire(71)
	if reason != FailureNone {
		t.Fatalf("first shared admission: %v", reason)
	}
	if lease, reason := secondSupervisor.TryAcquire(71); lease != nil || reason != FailureBusy {
		t.Fatalf("same-account cross-instance admission = (%v, %v), want busy", lease, reason)
	}
	second, reason := secondSupervisor.TryAcquire(72)
	if reason != FailureNone {
		t.Fatalf("second shared admission: %v", reason)
	}
	if lease, reason := firstSupervisor.TryAcquire(73); lease != nil || reason != FailureBusy {
		t.Fatalf("third cross-instance admission = (%v, %v), want busy", lease, reason)
	}
	first.Close()
	second.Close()
	if got := secondSupervisor.Metrics().Busy - busyBefore; got != 2 {
		t.Fatalf("shared busy metric increment = %d, want 2", got)
	}
}

func TestRealWorkerReturnsValidatedDerivatives(t *testing.T) {
	input := testJPEG(t, 1600, 1600)
	s := newTestSupervisor(realWorker, 30*time.Second, 5*time.Second)
	lease, reason := s.TryAcquire(42)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), input, 1600, 1600)
	if reason != FailureNone {
		t.Fatalf("real worker result: %v", reason)
	}
	if len(result.M) == 0 || result.MWidth != 320 || result.MHeight != 320 {
		t.Fatalf("m derivative = %dx%d, %d bytes, want 320x320 and bytes", result.MWidth, result.MHeight, len(result.M))
	}
	if len(result.Stripped) < 4 || result.Stripped[0] != 1 || result.Stripped[1] != 40 || result.Stripped[2] != 40 {
		t.Fatalf("stripped derivative prefix = %v, want [1 40 40]", result.Stripped[:min(3, len(result.Stripped))])
	}
	if got := s.Metrics(); got.TotalFailures() != 0 {
		t.Fatalf("failure metrics after valid worker = %+v", got)
	}
}

func TestRealWorkerReturnsStrippedOnlyForSmallPhoto(t *testing.T) {
	input := testJPEG(t, 320, 240)
	s := newTestSupervisor(realWorker, 30*time.Second, 5*time.Second)
	lease, reason := s.TryAcquire(43)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), input, 320, 240)
	if reason != FailureNone {
		t.Fatalf("real worker result: %v", reason)
	}
	if len(result.M) != 0 || result.MWidth != 0 || result.MHeight != 0 {
		t.Fatalf("small photo m derivative = %dx%d, %d bytes, want none", result.MWidth, result.MHeight, len(result.M))
	}
	if len(result.Stripped) < 4 || result.Stripped[0] != 1 || result.Stripped[1] != 30 || result.Stripped[2] != 40 {
		t.Fatalf("small photo stripped prefix = %v, want [1 30 40]", result.Stripped[:min(3, len(result.Stripped))])
	}
}

func TestProcessTimeoutKillsChildGroupAndReleasesSlots(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pids")
	worker := writeFakeWorker(t, fmt.Sprintf("printf '%%s ' \"$$\" > '%s'\n/bin/sleep 10 &\nprintf '%%s' \"$!\" >> '%s'\n/bin/cat >/dev/null\n/bin/sleep 10\n", pidFile, pidFile))
	s := newTestSupervisor(worker, time.Second, 80*time.Millisecond)
	lease, reason := s.TryAcquire(55)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), bytes.Repeat([]byte{0x42}, 64<<10), 1600, 1600)
	if reason != FailureTimeout {
		t.Fatalf("hung child result = (%+v, %v), want timeout", result, reason)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read process ids: %v", err)
	}
	fields := strings.Fields(string(pidBytes))
	if len(fields) != 2 {
		t.Fatalf("process ids = %q, want child and descendant", pidBytes)
	}
	for _, field := range fields {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil {
			t.Fatalf("parse process id %q: %v", field, parseErr)
		}
		waitProcessGone(t, pid)
	}
	if got := s.Metrics().Timeout; got != 1 {
		t.Fatalf("timeout metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(55); next == nil || reason != FailureNone {
		t.Fatalf("admission after timeout = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessCancellationKillsBlockedChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	worker := writeFakeWorker(t, fmt.Sprintf("printf '%%s' \"$$\" > '%s'\n/bin/sleep 10\n", pidFile))
	s := newTestSupervisor(worker, 5*time.Second, time.Second)
	lease, reason := s.TryAcquire(56)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan FailureReason, 1)
	go func() {
		_, processReason := lease.Process(ctx, bytes.Repeat([]byte{0x24}, 10<<20), 1600, 1600)
		done <- processReason
	}()
	waitForFile(t, pidFile)
	cancel()
	select {
	case reason := <-done:
		if reason != FailureCanceled {
			t.Fatalf("cancelled worker result = %v, want canceled", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled blocked worker did not return")
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child process id: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child process id: %v", err)
	}
	waitProcessGone(t, pid)
	if got := s.Metrics().Canceled; got != 1 {
		t.Fatalf("canceled metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(56); next == nil || reason != FailureNone {
		t.Fatalf("admission after cancellation = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessTotalDeadlineKillsWorkerDuringInputAndReleasesSlots(t *testing.T) {
	worker := writeFakeWorker(t, "/bin/sleep 10\n")
	s := newTestSupervisor(worker, 80*time.Millisecond, time.Second)
	lease, reason := s.TryAcquire(57)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	started := time.Now()
	result, reason := lease.Process(context.Background(), bytes.Repeat([]byte{0x51}, 10<<20), 1600, 1600)
	if reason != FailureTimeout || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("total-deadline result = (%+v, %v), want timeout and no derivatives", result, reason)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("total deadline returned after %s, want under 1s", elapsed)
	}
	if got := s.Metrics().Timeout; got != 1 {
		t.Fatalf("timeout metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(57); next == nil || reason != FailureNone {
		t.Fatalf("admission after total timeout = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestWorkerExitReturnsNoDerivativesAndReleasesSlots(t *testing.T) {
	worker := writeFakeWorker(t, "/bin/cat >/dev/null\nprintf 'private stderr must not be surfaced' >&2\nexit 7\n")
	s := newTestSupervisor(worker, time.Second, time.Second)
	lease, reason := s.TryAcquire(58)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), []byte("accepted input"), 1600, 1600)
	if reason != FailureWorker || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("failed worker result = (%+v, %v), want worker failure and no derivatives", result, reason)
	}
	if got := s.Metrics().Worker; got != 1 {
		t.Fatalf("worker failure metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(58); next == nil || reason != FailureNone {
		t.Fatalf("admission after worker death = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessSetupFailureReleasesAdmission(t *testing.T) {
	worker := filepath.Join(t.TempDir(), "missing-photothumb")
	s := newTestSupervisor(worker, time.Second, time.Second)
	lease, reason := s.TryAcquire(60)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), []byte("accepted input"), 1600, 1600)
	if reason != FailureSetup || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("setup failure result = (%+v, %v), want no derivatives", result, reason)
	}
	if got := s.Metrics().Setup; got != 1 {
		t.Fatalf("setup failure metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(60); next == nil || reason != FailureNone {
		t.Fatalf("admission after setup failure = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessStopsOversizedStdoutAtFrameLimit(t *testing.T) {
	worker := writeFakeWorker(t, "/bin/cat >/dev/null\n/bin/dd if=/dev/zero bs=70000 count=1 2>/dev/null\n")
	s := newTestSupervisor(worker, time.Second, time.Second)
	lease, reason := s.TryAcquire(61)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), []byte("accepted input"), 1600, 1600)
	if reason != FailureOutput || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("oversized worker output = (%+v, %v), want no derivatives", result, reason)
	}
	if got := s.Metrics().Output; got != 1 {
		t.Fatalf("output failure metric = %d, want 1", got)
	}
	if next, reason := s.TryAcquire(61); next == nil || reason != FailureNone {
		t.Fatalf("admission after oversized output = (%v, %v), want released lease", next, reason)
	} else {
		next.Close()
	}
}

func TestProcessDrainsLargeStderrWithoutSurfacingIt(t *testing.T) {
	worker := writeFakeWorker(t, "/bin/cat >/dev/null\n/bin/dd if=/dev/zero bs=70000 count=1 >&2 2>/dev/null\nexit 1\n")
	s := newTestSupervisor(worker, time.Second, time.Second)
	lease, reason := s.TryAcquire(62)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	result, reason := lease.Process(context.Background(), []byte("accepted input"), 1600, 1600)
	if reason != FailureWorker || len(result.M) != 0 || len(result.Stripped) != 0 {
		t.Fatalf("worker with large stderr = (%+v, %v), want worker failure and no derivatives", result, reason)
	}
	if got := s.Metrics().Worker; got != 1 {
		t.Fatalf("worker failure metric = %d, want 1", got)
	}
}

func TestWorkerReceivesOnlyAcceptedProcessConfiguration(t *testing.T) {
	dir := t.TempDir()
	reportFile := filepath.Join(dir, "report.json")
	sentinelPath := filepath.Join(dir, "parent-only")
	sentinel, err := os.Create(sentinelPath)
	if err != nil {
		t.Fatalf("create inherited-fd sentinel: %v", err)
	}
	defer func() {
		if err := sentinel.Close(); err != nil {
			t.Errorf("close inherited-fd sentinel: %v", err)
		}
	}()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, sentinel.Fd(), syscall.F_SETFD, 0); errno != 0 {
		t.Fatalf("make sentinel inheritable: %v", errno)
	}
	s := newTestSupervisor(fakeWorker, 2*time.Second, time.Second)
	lease, reason := s.TryAcquire(59)
	if reason != FailureNone {
		t.Fatalf("admit account: %v", reason)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan FailureReason, 1)
	go func() {
		_, processReason := lease.Process(ctx, []byte(reportFile), 1600, 1600)
		done <- processReason
	}()
	var state struct {
		Args              []string
		Environment       []string
		WorkingDir        string
		Descriptors       []int
		DescriptorTargets []string
		PID               int
		ProcessGroup      int
	}
	waitForReport(t, reportFile, &state)
	if got, want := state.Args, []string{"worker", strconv.Itoa(len(reportFile)), "1600", "1600", "2560000"}; !equalStrings(got, want) {
		t.Fatalf("worker args = %v, want %v", got, want)
	}
	if got, want := state.Environment, []string{"GOMAXPROCS=1", "GOMEMLIMIT=224MiB"}; !equalStrings(got, want) {
		t.Fatalf("worker environment = %v, want exactly %v", got, want)
	}
	if state.WorkingDir != "/" {
		t.Fatalf("worker cwd = %q, want /", state.WorkingDir)
	}
	for _, target := range state.DescriptorTargets {
		if target == sentinelPath {
			t.Fatalf("worker inherited parent-only descriptor %s", sentinelPath)
		}
	}
	if state.PID <= 0 || state.ProcessGroup != state.PID {
		t.Fatalf("worker pid/process group = %d/%d, want the child's own process group", state.PID, state.ProcessGroup)
	}
	command := exec.CommandContext(context.Background(), "/bin/true")
	if err := configureProcess(command); err != nil {
		t.Fatalf("configure worker process: %v", err)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid || command.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("worker process attributes = %+v, want process group and parent-death SIGKILL", command.SysProcAttr)
	}
	cancel()
	select {
	case reason := <-done:
		if reason != FailureCanceled {
			t.Fatalf("cancelled worker result = %v, want canceled", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled worker did not exit")
	}
	waitProcessGone(t, state.PID)
}

func TestParentRejectsMalformedAndOversizedFrames(t *testing.T) {
	valid := runRealWorker(t, testJPEG(t, 1600, 1600), 1600, 1600)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{name: "bad magic", data: append([]byte(nil), valid...)},
		{name: "trailing bytes", data: append(append([]byte(nil), valid...), 0)},
		{name: "over frame limit", data: bytes.Repeat([]byte{0x5a}, maxFrameBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "bad magic" {
				tc.data[0] = 'X'
			}
			if _, reason := parseFrame(tc.data, 1600, 1600); reason == FailureNone {
				t.Fatalf("parent accepted %s", tc.name)
			}
		})
	}
}

func TestParentRejectsBadDimensionsAndStrippedPreview(t *testing.T) {
	input := testJPEG(t, 1600, 997)
	frame := runRealWorker(t, input, 1600, 997)
	parsed, reason := parseFrame(frame, 1600, 997)
	if reason != FailureNone {
		t.Fatalf("real worker frame: %v", reason)
	}
	mutations := []struct {
		name string
		edit func([]byte)
	}{
		{name: "m dimension", edit: func(frame []byte) { binary.BigEndian.PutUint16(frame[8:10], 200) }},
		{name: "m aspect", edit: func(frame []byte) { binary.BigEndian.PutUint16(frame[6:8], 319) }},
		{name: "m SOF mismatch", edit: func(frame []byte) {
			sof := bytes.Index(frame[14:14+len(parsed.M)], []byte{0xff, 0xc0})
			if sof < 0 {
				t.Fatal("worker m has no baseline SOF")
			}
			binary.BigEndian.PutUint16(frame[14+sof+7:14+sof+9], 319)
		}},
		{name: "invalid m JPEG", edit: func(frame []byte) { frame[14] = 0 }},
		{name: "stripped prefix", edit: func(frame []byte) { frame[14+len(parsed.M)+2] = 2 }},
		{name: "stripped aspect", edit: func(frame []byte) { frame[14+len(parsed.M)+2+1] = 40 }},
		{name: "invalid stripped JPEG", edit: func(frame []byte) {
			start := 14 + len(parsed.M) + 2 + 3
			clear(frame[start:])
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			mutated := append([]byte(nil), frame...)
			tc.edit(mutated)
			if _, reason := parseFrame(mutated, 1600, 997); reason == FailureNone {
				t.Fatalf("parent accepted mutated %s", tc.name)
			}
		})
	}
}

func TestParentRejectsTrailingBytesInsideThumbnailJPEG(t *testing.T) {
	frame := runRealWorker(t, testJPEG(t, 1600, 997), 1600, 997)
	mLength := int(binary.BigEndian.Uint32(frame[10:14]))
	position := 14 + mLength
	mutated := make([]byte, len(frame)+1)
	copy(mutated[:position], frame[:position])
	mutated[position] = 0
	copy(mutated[position+1:], frame[position:])
	binary.BigEndian.PutUint32(mutated[10:14], uint32(mLength+1)) // #nosec G115 -- this test frame's m is bounded to 65,536 bytes.
	if _, reason := parseFrame(mutated, 1600, 997); reason != FailureValidation {
		t.Fatalf("thumbnail JPEG with trailing byte reason = %v, want validation failure", reason)
	}
}

func TestStderrRetentionIsBounded(t *testing.T) {
	var retained boundedBuffer
	input := bytes.Repeat([]byte{0x7e}, maxStderrBytes*8)
	n, err := retained.Write(input)
	if err != nil || n != len(input) {
		t.Fatalf("bounded stderr write = (%d, %v), want full drain", n, err)
	}
	if len(retained.bytes) != maxStderrBytes {
		t.Fatalf("stderr retained %d bytes, want %d", len(retained.bytes), maxStderrBytes)
	}
}

func TestVerifiedWorkerPathRejectsNonCanonicalExecutable(t *testing.T) {
	if err := verifyWorkerPath("relative/photothumb"); err == nil {
		t.Fatal("relative worker path was accepted")
	}
	if err := verifyWorkerPath("/bin/sh"); err == nil {
		t.Fatal("noncanonical worker executable was accepted")
	}
}

func testJPEG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x7f, A: 0xff})
		}
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode test JPEG: %v", err)
	}
	return out.Bytes()
}

func runRealWorker(t *testing.T, input []byte, width, height int) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), realWorker, "worker", strconv.Itoa(len(input)), strconv.Itoa(width), strconv.Itoa(height), "2560000") // #nosec G204 -- local test worker with synthetic dimensions.
	cmd.Dir = "/"
	cmd.Env = []string{"GOMAXPROCS=1", "GOMEMLIMIT=224MiB"}
	cmd.Stdin = bytes.NewReader(input)
	var output bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run real worker: %v: %s", err, stderr.String())
	}
	return output.Bytes()
}

func writeFakeWorker(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-photothumb")
	script := "#!/bin/sh\nset -eu\n" + body
	if err := os.WriteFile(path, []byte(script), 0o500); err != nil { // #nosec G306 -- fake child must be executable and is owner-only.
		t.Fatalf("write fake worker: %v", err)
	}
	return path
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("worker did not create %s", path)
}

func waitForReport(t *testing.T, path string, target any) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		encoded, err := os.ReadFile(path)
		if err == nil && json.Unmarshal(encoded, target) == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("worker did not write a valid report to %s", path)
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func waitProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid))); errors.Is(err, os.ErrNotExist) {
			return
		}
		if state, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat")); err == nil {
			fields := strings.Fields(string(state))
			if len(fields) > 2 && fields[2] == "Z" {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d survived worker group kill", pid)
}
