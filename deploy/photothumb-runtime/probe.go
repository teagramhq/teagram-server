package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	workerPath              = "/usr/local/bin/photothumb"
	workerReplacementBytes  = 10 << 20
	workerMaxInputBytes     = 10 << 20
	workerPixelCap          = 2_560_000
	workerWidth             = 1600
	workerHeight            = 1600
	workerMaxCPUSeconds     = 1.5
	workerMaxRSSBytes       = 240 << 20
	containerMemoryBytes    = 320 << 20
	maxThumbnailBytes       = 65_536
	maxStrippedBytes        = 2_048
	workerFixtureSHA256     = "31211d9908380b747cd78e9637752e582ba8284db40622e9c8464483d68890bc"
	workerMemoryLimitEnv    = "GOMEMLIMIT=224MiB"
	workerProcessorCountEnv = "GOMAXPROCS=1"
)

type result struct {
	Status              string  `json:"status"`
	Architecture        string  `json:"architecture"`
	FixtureBytes        int     `json:"fixture_bytes"`
	FixtureSHA256       string  `json:"fixture_sha256"`
	WorkerCPUSeconds    float64 `json:"worker_cpu_seconds"`
	WorkerPeakRSSBytes  int64   `json:"worker_peak_rss_bytes"`
	WorkerFDCount       int     `json:"worker_fd_count"`
	WorkerRuntimeFDs    int     `json:"worker_runtime_fds"`
	WorkerOOMScoreAdj   int     `json:"worker_oom_score_adj"`
	RetryFixtureBytes   int     `json:"retry_fixture_bytes"`
	RetryFixtureSHA256  string  `json:"retry_fixture_sha256"`
	RetryCPUSeconds     float64 `json:"retry_cpu_seconds"`
	RetryPeakRSSBytes   int64   `json:"retry_peak_rss_bytes"`
	RetryFlag           bool    `json:"retry_flag"`
	CgroupMemoryBytes   int64   `json:"cgroup_memory_bytes"`
	CgroupSwapBytes     int64   `json:"cgroup_swap_bytes"`
	OOMKillDelta        uint64  `json:"oom_kill_delta"`
	WorkerCleanupPassed bool    `json:"worker_cleanup_passed"`
	OOMCleanupPassed    bool    `json:"oom_cleanup_passed"`
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--oom-helper" {
		runOOMHelper()
		return
	}
	if err := run(); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "photothumb runtime probe:", err); writeErr != nil {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run() error {
	input, err := io.ReadAll(io.LimitReader(os.Stdin, workerMaxInputBytes+1))
	if err != nil {
		return fmt.Errorf("read fixture from stdin: %w", err)
	}
	if len(input) != workerReplacementBytes {
		return fmt.Errorf("replacement fixture size = %d, want %d", len(input), workerReplacementBytes)
	}
	fixtureDigest := sha256.Sum256(input)
	fixtureSHA256 := hex.EncodeToString(fixtureDigest[:])
	if fixtureSHA256 != workerFixtureSHA256 {
		return fmt.Errorf("fixture SHA-256 = %s, want %s", fixtureSHA256, workerFixtureSHA256)
	}

	memoryBytes, swapBytes, err := assertContainerMemoryLimit()
	if err != nil {
		return err
	}

	workerResult, err := runWorker(input)
	if err != nil {
		return err
	}
	if workerResult.cpuSeconds > workerMaxCPUSeconds {
		return fmt.Errorf("worker CPU = %.3fs, exceeds %.1fs", workerResult.cpuSeconds, workerMaxCPUSeconds)
	}
	if workerResult.peakRSSBytes > workerMaxRSSBytes {
		return fmt.Errorf("worker peak RSS = %d bytes, exceeds %d", workerResult.peakRSSBytes, workerMaxRSSBytes)
	}
	if err := verifyWorkerCleanup(input); err != nil {
		return err
	}

	runtime.GC()
	debug.FreeOSMemory()

	retryInput, err := makeRetryFixture()
	if err != nil {
		return err
	}
	if len(retryInput) < 1 || len(retryInput) > workerMaxInputBytes {
		return fmt.Errorf("generated retry fixture size = %d, outside 1..%d", len(retryInput), workerMaxInputBytes)
	}
	retryDigest := sha256.Sum256(retryInput)
	retrySHA256 := hex.EncodeToString(retryDigest[:])
	retryResult, err := runWorker(retryInput)
	if err != nil {
		return fmt.Errorf("run retry fixture: %w", err)
	}
	if retryResult.flags&2 == 0 {
		return errors.New("retry fixture did not exercise the bounded quality retry")
	}
	if retryResult.cpuSeconds > workerMaxCPUSeconds {
		return fmt.Errorf("retry worker CPU = %.3fs, exceeds %.1fs", retryResult.cpuSeconds, workerMaxCPUSeconds)
	}
	if retryResult.peakRSSBytes > workerMaxRSSBytes {
		return fmt.Errorf("retry worker peak RSS = %d bytes, exceeds %d", retryResult.peakRSSBytes, workerMaxRSSBytes)
	}
	retryFixtureBytes := len(retryInput)
	runtime.GC()
	debug.FreeOSMemory()

	oomKillDelta, err := verifyCgroupOOMCleanup()
	if err != nil {
		return err
	}

	return json.NewEncoder(os.Stdout).Encode(result{
		Status:              "photothumb_runtime_passed",
		Architecture:        runtime.GOARCH,
		FixtureBytes:        workerReplacementBytes,
		FixtureSHA256:       fixtureSHA256,
		WorkerCPUSeconds:    workerResult.cpuSeconds,
		WorkerPeakRSSBytes:  workerResult.peakRSSBytes,
		WorkerFDCount:       workerResult.fdCount,
		WorkerRuntimeFDs:    workerResult.runtimeFDs,
		WorkerOOMScoreAdj:   workerResult.oomScoreAdj,
		RetryFixtureBytes:   retryFixtureBytes,
		RetryFixtureSHA256:  retrySHA256,
		RetryCPUSeconds:     retryResult.cpuSeconds,
		RetryPeakRSSBytes:   retryResult.peakRSSBytes,
		RetryFlag:           retryResult.flags&2 != 0,
		CgroupMemoryBytes:   memoryBytes,
		CgroupSwapBytes:     swapBytes,
		OOMKillDelta:        oomKillDelta,
		WorkerCleanupPassed: true,
		OOMCleanupPassed:    true,
	})
}

type workerResult struct {
	cpuSeconds   float64
	peakRSSBytes int64
	fdCount      int
	runtimeFDs   int
	oomScoreAdj  int
	flags        byte
}

func runWorker(input []byte) (workerResult, error) {
	if len(input) < 1 || len(input) > workerMaxInputBytes {
		return workerResult{}, fmt.Errorf("worker input size = %d, outside 1..%d", len(input), workerMaxInputBytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, workerPath, "worker", strconv.Itoa(len(input)), strconv.Itoa(workerWidth), strconv.Itoa(workerHeight), strconv.Itoa(workerPixelCap)) // #nosec G204 -- fixed worker path and validated numeric arguments.
	cmd.Env = []string{workerProcessorCountEnv, workerMemoryLimitEnv}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = killProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return workerResult{}, fmt.Errorf("open worker stdin: %w", err)
	}
	var stdout bytes.Buffer
	var stderr limitedBuffer
	stderr.limit = 8 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return workerResult{}, errors.Join(fmt.Errorf("start worker: %w", err), stdin.Close())
	}
	pid := cmd.Process.Pid
	if _, err := io.Copy(stdin, bytes.NewReader(input)); err != nil {
		return workerResult{}, errors.Join(fmt.Errorf("write worker input: %w", err), cancelAndReap(cmd, stdin))
	}
	proc, err := inspectWorker(pid)
	if err != nil {
		return workerResult{}, errors.Join(err, cancelAndReap(cmd, stdin))
	}
	if err := stdin.Close(); err != nil {
		return workerResult{}, errors.Join(fmt.Errorf("signal worker input completion: %w", err), cancelAndReap(cmd, stdin))
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return workerResult{}, fmt.Errorf("worker exceeded 10-second runtime deadline: %w; stderr=%s", ctx.Err(), stderr.String())
		}
		return workerResult{}, fmt.Errorf("worker failed: %w; stderr=%s", err, stderr.String())
	}
	flags, err := assertWorkerFrame(stdout.Bytes())
	if err != nil {
		return workerResult{}, err
	}
	rusage, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok || rusage == nil {
		return workerResult{}, errors.New("worker process did not provide Linux resource usage")
	}
	cpuSeconds := float64(rusage.Utime.Sec) + float64(rusage.Utime.Usec)/1e6 + float64(rusage.Stime.Sec) + float64(rusage.Stime.Usec)/1e6
	return workerResult{
		cpuSeconds:   cpuSeconds,
		peakRSSBytes: rusage.Maxrss * 1024,
		fdCount:      proc.fdCount,
		runtimeFDs:   proc.runtimeFDs,
		oomScoreAdj:  proc.oomScoreAdj,
		flags:        flags,
	}, nil
}

type workerProc struct {
	fdCount     int
	runtimeFDs  int
	oomScoreAdj int
}

func inspectWorker(pid int) (workerProc, error) {
	procPath := filepath.Join("/proc", strconv.Itoa(pid))
	if err := assertResourceLimits(filepath.Join(procPath, "limits")); err != nil {
		return workerProc{}, err
	}
	if err := assertWorkerEnvironment(filepath.Join(procPath, "environ")); err != nil {
		return workerProc{}, err
	}
	fdEntries, err := os.ReadDir(filepath.Join(procPath, "fd"))
	if err != nil {
		return workerProc{}, fmt.Errorf("read worker file descriptors: %w", err)
	}
	standardFDs := map[string]bool{"0": false, "1": false, "2": false}
	runtimeFDs := map[string]bool{
		"/sys/fs/cgroup/cpu.max": false,
		"anon_inode:[eventpoll]": false,
		"anon_inode:[eventfd]":   false,
	}
	for _, entry := range fdEntries {
		target, err := os.Readlink(filepath.Join(procPath, "fd", entry.Name()))
		if err != nil {
			return workerProc{}, fmt.Errorf("read worker file descriptor %s: %w", entry.Name(), err)
		}
		if _, ok := standardFDs[entry.Name()]; ok {
			if !strings.HasPrefix(target, "pipe:[") {
				return workerProc{}, fmt.Errorf("worker standard descriptor %s points to %q, want a pipe", entry.Name(), target)
			}
			standardFDs[entry.Name()] = true
			continue
		}
		if _, ok := runtimeFDs[target]; !ok {
			return workerProc{}, fmt.Errorf("worker has unaccepted file descriptor %s=%q", entry.Name(), target)
		}
		runtimeFDs[target] = true
	}
	if len(fdEntries) != len(standardFDs)+len(runtimeFDs) {
		return workerProc{}, fmt.Errorf("worker has %d open file descriptors, want 3 stdio and 3 Go runtime descriptors", len(fdEntries))
	}
	for fd, present := range standardFDs {
		if !present {
			return workerProc{}, fmt.Errorf("worker is missing standard file descriptor %s", fd)
		}
	}
	for descriptor, present := range runtimeFDs {
		if !present {
			return workerProc{}, fmt.Errorf("worker is missing expected Go runtime descriptor %q", descriptor)
		}
	}
	oomScoreText, err := os.ReadFile(filepath.Join(procPath, "oom_score_adj")) // #nosec G304 -- procPath is derived from the child PID and the proc file is fixed.
	if err != nil {
		return workerProc{}, fmt.Errorf("read worker OOM score adjustment: %w", err)
	}
	oomScoreAdj, err := strconv.Atoi(strings.TrimSpace(string(oomScoreText)))
	if err != nil {
		return workerProc{}, fmt.Errorf("parse worker OOM score adjustment: %w", err)
	}
	if oomScoreAdj != 1000 {
		return workerProc{}, fmt.Errorf("worker oom_score_adj = %d, want 1000", oomScoreAdj)
	}
	return workerProc{fdCount: len(fdEntries), runtimeFDs: len(runtimeFDs), oomScoreAdj: oomScoreAdj}, nil
}

func assertResourceLimits(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- the caller supplies a fixed /proc/<child-pid>/limits path.
	if err != nil {
		return fmt.Errorf("read worker resource limits: %w", err)
	}
	limits := map[string][]string{
		"Max data size":  {strconv.Itoa(containerMemoryBytes), strconv.Itoa(containerMemoryBytes)},
		"Max cpu time":   {"2", "3"},
		"Max file size":  {"0", "0"},
		"Max open files": {"8", "8"},
	}
	for label, want := range limits {
		found := false
		for line := range strings.SplitSeq(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 5 || strings.Join(fields[:3], " ") != label {
				continue
			}
			if fields[3] == want[0] && fields[4] == want[1] {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("worker %s limit does not equal %v: %s", label, want, data)
		}
	}
	return nil
}

func assertWorkerEnvironment(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- the caller supplies a fixed /proc/<child-pid>/environ path.
	if err != nil {
		return fmt.Errorf("read worker environment: %w", err)
	}
	actual := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	if len(actual) != 2 || actual[0] == actual[1] {
		return fmt.Errorf("worker environment contains %d entries, want exactly the two accepted settings", len(actual))
	}
	want := map[string]bool{workerProcessorCountEnv: false, workerMemoryLimitEnv: false}
	for _, entry := range actual {
		if _, ok := want[entry]; !ok {
			return fmt.Errorf("worker has unexpected environment entry %q", entry)
		}
		want[entry] = true
	}
	for entry, found := range want {
		if !found {
			return fmt.Errorf("worker is missing environment entry %q", entry)
		}
	}
	return nil
}

func assertWorkerFrame(frame []byte) (byte, error) {
	const headerBytes = 14
	if len(frame) < headerBytes+2 {
		return 0, fmt.Errorf("worker frame is only %d bytes", len(frame))
	}
	if string(frame[:4]) != "PTD1" || frame[4] != 1 {
		return 0, fmt.Errorf("worker frame header = %q/%d, want PTD1/version 1", frame[:4], frame[4])
	}
	flags := frame[5]
	if flags&^byte(3) != 0 || flags&1 == 0 {
		return 0, fmt.Errorf("worker frame flags = %#02x, want m present and no unknown flags", flags)
	}
	width := int(binary.BigEndian.Uint16(frame[6:8]))
	height := int(binary.BigEndian.Uint16(frame[8:10]))
	thumbnailLength := int(binary.BigEndian.Uint32(frame[10:14]))
	if width != 320 || height != 320 {
		return 0, fmt.Errorf("worker thumbnail dimensions = %dx%d, want 320x320", width, height)
	}
	if thumbnailLength < 4 || thumbnailLength > maxThumbnailBytes || headerBytes+thumbnailLength+2 > len(frame) {
		return 0, fmt.Errorf("worker thumbnail length = %d, outside 4..%d or frame", thumbnailLength, maxThumbnailBytes)
	}
	thumbnail := frame[headerBytes : headerBytes+thumbnailLength]
	decoded, err := jpeg.Decode(bytes.NewReader(thumbnail))
	if err != nil {
		return 0, fmt.Errorf("decode worker thumbnail: %w", err)
	}
	if decoded.Bounds().Dx() != width || decoded.Bounds().Dy() != height {
		return 0, fmt.Errorf("worker thumbnail dimensions = %v, want %dx%d", decoded.Bounds(), width, height)
	}
	strippedLengthOffset := headerBytes + thumbnailLength
	strippedLength := int(binary.BigEndian.Uint16(frame[strippedLengthOffset : strippedLengthOffset+2]))
	if strippedLength < 4 || strippedLength > maxStrippedBytes || strippedLengthOffset+2+strippedLength != len(frame) {
		return 0, fmt.Errorf("worker stripped length = %d, inconsistent with frame or outside 4..%d", strippedLength, maxStrippedBytes)
	}
	stripped := frame[strippedLengthOffset+2:]
	if stripped[0] != 1 || stripped[1] != 40 || stripped[2] != 40 {
		return 0, fmt.Errorf("worker stripped preview prefix = %v, want [1 40 40]", stripped[:3])
	}
	return flags, nil
}

func verifyWorkerCleanup(input []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, workerPath, "worker", strconv.Itoa(len(input)), strconv.Itoa(workerWidth), strconv.Itoa(workerHeight), strconv.Itoa(workerPixelCap)) // #nosec G204 -- fixed worker path and validated numeric arguments.
	cmd.Env = []string{workerProcessorCountEnv, workerMemoryLimitEnv}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = killProcessGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open cleanup worker stdin: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return errors.Join(fmt.Errorf("start cleanup worker: %w", err), stdin.Close())
	}
	pid := cmd.Process.Pid
	if _, err := stdin.Write(input[:1]); err != nil {
		return errors.Join(fmt.Errorf("write cleanup worker prefix: %w", err), cancelAndReap(cmd, stdin))
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "limits")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return errors.Join(errors.New("cleanup worker did not remain alive while its input was incomplete"), cancelAndReap(cmd, stdin))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		return errors.Join(fmt.Errorf("kill cleanup worker process group: %w", err), cancelAndReap(cmd, stdin))
	}
	closeErr := stdin.Close()
	waitErr := cmd.Wait()
	if waitErr == nil {
		return errors.Join(closeErr, errors.New("cleanup worker survived process-group kill"))
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return errors.Join(closeErr, fmt.Errorf("cleanup worker ended with %w, want SIGKILL", waitErr))
	}
	if err := assertProcessGone(pid); err != nil {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

func cancelAndReap(cmd *exec.Cmd, stdin io.Closer) error {
	var cleanupErrors []error
	if cmd.Process != nil {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("kill worker process group: %w", err))
		}
	}
	if err := stdin.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("close worker stdin during cleanup: %w", err))
	}
	if err := cmd.Wait(); err != nil {
		status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("wait for killed worker: %w", err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func makeRetryFixture() ([]byte, error) {
	imageRGBA := image.NewRGBA(image.Rect(0, 0, workerWidth, workerHeight))
	for y := range workerHeight {
		for x := range workerWidth {
			pixel := color.RGBA{
				R: uint8((x*7 + y*3) & 0xff),
				G: uint8((x*5 + y*11) & 0xff),
				B: uint8((x*13 + y*2) & 0xff),
				A: 255,
			}
			switch {
			case x%5 == 0 && y%5 == 0:
				blockX, blockY := x/5, y/5
				noise := (blockX*73856093 ^ blockY*19349663 ^ blockX*blockY*83492791) & 0x7fffffff
				pixel.R = uint8(noise & 0xff)
				pixel.G = uint8((noise >> 8) & 0xff)
				pixel.B = uint8((noise >> 16) & 0xff)
			case x%5 != 0:
				pixel = imageRGBA.RGBAAt(x-1, y)
			default:
				pixel = imageRGBA.RGBAAt(x, y-1)
			}
			imageRGBA.SetRGBA(x, y, pixel)
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, imageRGBA, &jpeg.Options{Quality: 100}); err != nil {
		return nil, fmt.Errorf("encode retry fixture: %w", err)
	}
	return encoded.Bytes(), nil
}

func verifyCgroupOOMCleanup() (uint64, error) {
	before, err := memoryEventCount("oom_kill")
	if err != nil {
		return 0, err
	}
	executable, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("locate OOM helper executable: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--oom-helper") // #nosec G204 -- executable is this probe binary, resolved by os.Executable.
	cmd.Env = []string{workerProcessorCountEnv}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = killProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start cgroup OOM helper: %w", err)
	}
	pid := cmd.Process.Pid
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return 0, fmt.Errorf("cgroup OOM helper did not stop within 15 seconds: %w", ctx.Err())
	}
	if waitErr == nil {
		return 0, errors.New("cgroup OOM helper survived its 512 MiB allocation")
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		return 0, fmt.Errorf("cgroup OOM helper ended with %w, want SIGKILL", waitErr)
	}
	if err := assertProcessGone(pid); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after, err := memoryEventCount("oom_kill")
		if err != nil {
			return 0, err
		}
		if after > before {
			return after - before, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 0, errors.New("cgroup memory.events did not record the helper's OOM kill")
}

func runOOMHelper() {
	allocation := make([]byte, 512<<20)
	for offset := 0; offset < len(allocation); offset += os.Getpagesize() {
		allocation[offset] = 1
	}
	if _, err := fmt.Fprintln(os.Stderr, "cgroup OOM helper survived its allocation"); err != nil {
		os.Exit(91)
	}
	os.Exit(90)
}

func assertContainerMemoryLimit() (int64, int64, error) {
	memoryText, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return 0, 0, fmt.Errorf("read cgroup-v2 memory.max (required for the bounded OOM probe): %w", err)
	}
	memoryBytes, err := strconv.ParseInt(strings.TrimSpace(string(memoryText)), 10, 64)
	if err != nil || memoryBytes != containerMemoryBytes {
		return 0, 0, fmt.Errorf("cgroup memory.max = %q, want %d bytes", strings.TrimSpace(string(memoryText)), containerMemoryBytes)
	}
	swapText, err := os.ReadFile("/sys/fs/cgroup/memory.swap.max")
	if err != nil {
		return 0, 0, fmt.Errorf("read cgroup-v2 memory.swap.max (required for the bounded OOM probe): %w", err)
	}
	swapBytes, err := strconv.ParseInt(strings.TrimSpace(string(swapText)), 10, 64)
	if err != nil || swapBytes != 0 {
		return 0, 0, fmt.Errorf("cgroup memory.swap.max = %q, want 0", strings.TrimSpace(string(swapText)))
	}
	return memoryBytes, swapBytes, nil
}

func memoryEventCount(name string) (uint64, error) {
	data, err := os.ReadFile("/sys/fs/cgroup/memory.events")
	if err != nil {
		return 0, fmt.Errorf("read cgroup-v2 memory.events: %w", err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != name {
			continue
		}
		count, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse cgroup memory.events %s count: %w", name, err)
		}
		return count, nil
	}
	return 0, fmt.Errorf("cgroup-v2 memory.events has no %q counter", name)
}

func assertProcessGone(pid int) error {
	path := filepath.Join("/proc", strconv.Itoa(pid))
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("process %d remains after reap", pid)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect reaped process %d: %w", pid, err)
	}
	return nil
}

func killProcessGroup(cmd *exec.Cmd) func() error {
	return func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return nil
	}
}

type limitedBuffer struct {
	bytes.Buffer

	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len() >= b.limit {
		return len(p), nil
	}
	if remaining := b.limit - b.Len(); len(p) > remaining {
		_, err := b.Buffer.Write(p[:remaining])
		if err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
