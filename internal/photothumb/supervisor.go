// Package photothumb bounds and supervises isolated photo derivative workers.
package photothumb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image/jpeg"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/teagramhq/teagram-server/internal/photowire"
)

const (
	// WorkerPath is the only executable accepted by the production supervisor.
	WorkerPath = "/usr/local/bin/photothumb"

	maxEncodedBytes   = 10 << 20
	maxEligiblePixels = 2_560_000
	maxThumbnailBytes = 65_536
	maxStrippedBytes  = 2_048
	maxFrameBytes     = 4 + 1 + 1 + 2 + 2 + 4 + maxThumbnailBytes + 2 + maxStrippedBytes
	maxStderrBytes    = 4 << 10
	thumbnailLongSide = 320
	strippedLongSide  = 40

	workerTotalTimeout       = 30 * time.Second
	workerAfterInputTimeout  = 5 * time.Second
	workerEnvironmentMemory  = "GOMEMLIMIT=224MiB"
	workerEnvironmentProcs   = "GOMAXPROCS=1"
	workerPixelLimitArgument = "2560000"
)

// FailureReason is a fixed, bounded class suitable for metrics. It never
// carries worker stderr or details derived from uploaded content.
type FailureReason uint8

const (
	FailureNone FailureReason = iota
	FailureBusy
	FailureInvalidInput
	FailureSetup
	FailureInput
	FailureTimeout
	FailureCanceled
	FailureWorker
	FailureOutput
	FailureValidation
	FailureCleanup
	FailureIneligible
	FailurePersistence
)

func (reason FailureReason) String() string {
	switch reason {
	case FailureNone:
		return "none"
	case FailureBusy:
		return "busy"
	case FailureInvalidInput:
		return "invalid_input"
	case FailureSetup:
		return "setup"
	case FailureInput:
		return "input"
	case FailureTimeout:
		return "timeout"
	case FailureCanceled:
		return "canceled"
	case FailureWorker:
		return "worker"
	case FailureOutput:
		return "output"
	case FailureValidation:
		return "validation"
	case FailureCleanup:
		return "cleanup"
	case FailureIneligible:
		return "ineligible"
	case FailurePersistence:
		return "persistence"
	default:
		return "unknown"
	}
}

// Derivatives contains only parent-validated bytes from one PTD1 frame.
type Derivatives struct {
	M        []byte
	MWidth   int
	MHeight  int
	Stripped []byte
	Retried  bool
}

// MetricSnapshot contains counters for the fixed set of derivative failures.
type MetricSnapshot struct {
	Busy         uint64
	InvalidInput uint64
	Setup        uint64
	Input        uint64
	Timeout      uint64
	Canceled     uint64
	Worker       uint64
	Output       uint64
	Validation   uint64
	Cleanup      uint64
	Ineligible   uint64
	Persistence  uint64
}

// TotalFailures sums every reason counter. Successful worker runs are not
// failures and are not included.
func (snapshot MetricSnapshot) TotalFailures() uint64 {
	return snapshot.Busy + snapshot.InvalidInput + snapshot.Setup + snapshot.Input + snapshot.Timeout + snapshot.Canceled + snapshot.Worker + snapshot.Output + snapshot.Validation + snapshot.Cleanup + snapshot.Ineligible + snapshot.Persistence
}

type failureCounters struct {
	busy         atomic.Uint64
	invalidInput atomic.Uint64
	setup        atomic.Uint64
	input        atomic.Uint64
	timeout      atomic.Uint64
	canceled     atomic.Uint64
	worker       atomic.Uint64
	output       atomic.Uint64
	validation   atomic.Uint64
	cleanup      atomic.Uint64
	ineligible   atomic.Uint64
	persistence  atomic.Uint64
}

type admissionSlots struct {
	mu       sync.Mutex
	active   int
	accounts map[int64]struct{}
	metrics  failureCounters
}

var replicaAdmission = &admissionSlots{accounts: make(map[int64]struct{})}

// Supervisor bounds isolated photo workers for one server replica.
type Supervisor struct {
	workerPath string
	totalLimit time.Duration
	inputLimit time.Duration
	admission  *admissionSlots
}

// New verifies the accepted root-owned executable path before processing any
// captured photo bytes.
func New() (*Supervisor, error) {
	if err := verifyWorkerPath(WorkerPath); err != nil {
		return nil, err
	}
	return newSupervisorWithAdmission(WorkerPath, workerTotalTimeout, workerAfterInputTimeout, replicaAdmission), nil
}

// NewForTesting accepts a temporary executable only from a Go test binary.
// Production always calls New, which accepts only WorkerPath after ownership
// and mode validation.
func NewForTesting(path string) (*Supervisor, error) {
	if !strings.HasSuffix(os.Args[0], ".test") {
		return nil, errors.New("test photo worker is available only in Go test binaries")
	}
	if !safePath(path) {
		return nil, errors.New("test photo worker path must be absolute and clean")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("test photo worker is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return nil, errors.New("test photo worker must be an executable regular file")
	}
	return newSupervisorWithAdmission(path, workerTotalTimeout, workerAfterInputTimeout, &admissionSlots{accounts: make(map[int64]struct{})}), nil
}

// TryAcquire reserves one of the two replica worker slots and the account's
// single slot without waiting. Acquire this before capturing the photo stream.
func (s *Supervisor) TryAcquire(accountID int64) (*Lease, FailureReason) {
	if s == nil || accountID <= 0 {
		if s != nil {
			s.record(FailureInvalidInput)
		}
		return nil, FailureInvalidInput
	}

	if s.admission == nil {
		return nil, FailureInvalidInput
	}
	s.admission.mu.Lock()
	defer s.admission.mu.Unlock()
	if s.admission.active >= 2 {
		s.record(FailureBusy)
		return nil, FailureBusy
	}
	if _, exists := s.admission.accounts[accountID]; exists {
		s.record(FailureBusy)
		return nil, FailureBusy
	}
	s.admission.active++
	s.admission.accounts[accountID] = struct{}{}
	return &Lease{supervisor: s, accountID: accountID}, FailureNone
}

// Metrics returns the process-local derivative failure counters.
func (s *Supervisor) Metrics() MetricSnapshot {
	if s == nil {
		return MetricSnapshot{}
	}
	if s.admission == nil {
		return MetricSnapshot{}
	}
	return MetricSnapshot{
		Busy:         s.admission.metrics.busy.Load(),
		InvalidInput: s.admission.metrics.invalidInput.Load(),
		Setup:        s.admission.metrics.setup.Load(),
		Input:        s.admission.metrics.input.Load(),
		Timeout:      s.admission.metrics.timeout.Load(),
		Canceled:     s.admission.metrics.canceled.Load(),
		Worker:       s.admission.metrics.worker.Load(),
		Output:       s.admission.metrics.output.Load(),
		Validation:   s.admission.metrics.validation.Load(),
		Cleanup:      s.admission.metrics.cleanup.Load(),
		Ineligible:   s.admission.metrics.ineligible.Load(),
		Persistence:  s.admission.metrics.persistence.Load(),
	}
}

func newSupervisorWithAdmission(path string, totalLimit, inputLimit time.Duration, admission *admissionSlots) *Supervisor {
	return &Supervisor{
		workerPath: path,
		totalLimit: totalLimit,
		inputLimit: inputLimit,
		admission:  admission,
	}
}

func (s *Supervisor) record(reason FailureReason) FailureReason {
	if s == nil || s.admission == nil {
		return reason
	}
	switch reason {
	case FailureBusy:
		s.admission.metrics.busy.Add(1)
	case FailureInvalidInput:
		s.admission.metrics.invalidInput.Add(1)
	case FailureSetup:
		s.admission.metrics.setup.Add(1)
	case FailureInput:
		s.admission.metrics.input.Add(1)
	case FailureTimeout:
		s.admission.metrics.timeout.Add(1)
	case FailureCanceled:
		s.admission.metrics.canceled.Add(1)
	case FailureWorker:
		s.admission.metrics.worker.Add(1)
	case FailureOutput:
		s.admission.metrics.output.Add(1)
	case FailureValidation:
		s.admission.metrics.validation.Add(1)
	case FailureCleanup:
		s.admission.metrics.cleanup.Add(1)
	case FailureIneligible:
		s.admission.metrics.ineligible.Add(1)
	case FailurePersistence:
		s.admission.metrics.persistence.Add(1)
	}
	return reason
}

// RecordFailure increments one bounded, content-free derivative reason.
func (s *Supervisor) RecordFailure(reason FailureReason) {
	if reason != FailureNone {
		s.record(reason)
	}
}

// Lease holds a replica and account slot while the validated original is
// captured and processed. Process consumes the lease and releases it on every
// return; Close releases a lease abandoned before Process.
type Lease struct {
	supervisor *Supervisor
	accountID  int64

	mu       sync.Mutex
	state    leaseState
	released atomic.Bool
}

type leaseState uint8

const (
	leaseOpen leaseState = iota
	leaseRunning
	leaseClosed
)

// Close releases an unused lease. A running Process owns release and is
// stopped through its context.
func (lease *Lease) Close() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	if lease.state != leaseOpen {
		lease.mu.Unlock()
		return
	}
	lease.state = leaseClosed
	lease.mu.Unlock()
	lease.release()
}

// Process sends exactly the captured bytes to the worker after the original
// has been stored and structurally validated. A failure returns no bytes and
// only a fixed reason code.
func (lease *Lease) Process(ctx context.Context, input []byte, width, height int) (Derivatives, FailureReason) {
	if lease == nil || lease.supervisor == nil {
		return Derivatives{}, FailureInvalidInput
	}
	lease.mu.Lock()
	if lease.state != leaseOpen {
		lease.mu.Unlock()
		return Derivatives{}, lease.supervisor.record(FailureInvalidInput)
	}
	lease.state = leaseRunning
	lease.mu.Unlock()
	defer func() {
		lease.mu.Lock()
		lease.state = leaseClosed
		lease.mu.Unlock()
		lease.release()
	}()

	if ctx == nil || len(input) < 1 || len(input) > maxEncodedBytes {
		return Derivatives{}, lease.supervisor.record(FailureInvalidInput)
	}
	if !eligible(width, height) {
		return Derivatives{}, lease.supervisor.record(FailureIneligible)
	}
	if err := ctx.Err(); err != nil {
		return Derivatives{}, lease.supervisor.record(FailureCanceled)
	}
	result, reason := lease.supervisor.run(ctx, input, width, height)
	if reason != FailureNone {
		lease.supervisor.record(reason)
		return Derivatives{}, reason
	}
	return result, FailureNone
}

// ProcessCaptured transfers a completed capture to the worker. Call it only
// after the original Put and structural validation succeed. Finishing seals
// the upload-feed branch to discard, so worker latency or failure cannot hold
// the upload stream open or retain another buffered copy.
func (lease *Lease) ProcessCaptured(ctx context.Context, capture *InputCapture, expectedBytes int64, width, height int) (Derivatives, FailureReason) {
	input, ready := capture.Finish(expectedBytes)
	if !ready {
		return lease.Process(ctx, nil, width, height)
	}
	defer clear(input)
	return lease.Process(ctx, input, width, height)
}

func (lease *Lease) release() {
	if !lease.released.CompareAndSwap(false, true) {
		return
	}
	admission := lease.supervisor.admission
	admission.mu.Lock()
	admission.active--
	delete(admission.accounts, lease.accountID)
	admission.mu.Unlock()
}

func eligible(width, height int) bool {
	if width < 1 || height < 1 || width > 10_000 || height > 10_000 || width+height > 10_000 {
		return false
	}
	short, long := width, height
	if short > long {
		short, long = long, short
	}
	return long <= short*20 && int64(width)*int64(height) <= maxEligiblePixels
}

type writeResult struct{ err error }
type readResult struct {
	data []byte
	err  error
}

type workerProcess struct {
	mu         sync.Mutex
	kill       func() error
	wait       func() error
	done       chan struct{}
	reaping    bool
	reaped     bool
	waitErr    error
	cleanupErr error
}

func newWorkerProcess(cmd *exec.Cmd) *workerProcess {
	return &workerProcess{
		kill: func() error { return killCommand(cmd) },
		wait: cmd.Wait,
		done: make(chan struct{}),
	}
}

func (worker *workerProcess) cancel() error {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.reaping || worker.reaped {
		return nil
	}
	return worker.kill()
}

func (worker *workerProcess) cleanupAndWait() (waitErr, cleanupErr error) {
	worker.mu.Lock()
	if worker.done == nil {
		worker.done = make(chan struct{})
	}
	if worker.reaped {
		waitErr, cleanupErr = worker.waitErr, worker.cleanupErr
		worker.mu.Unlock()
		return waitErr, cleanupErr
	}
	if worker.reaping {
		done := worker.done
		worker.mu.Unlock()
		<-done
		worker.mu.Lock()
		waitErr, cleanupErr = worker.waitErr, worker.cleanupErr
		worker.mu.Unlock()
		return waitErr, cleanupErr
	}
	cleanupErr = worker.kill()
	worker.cleanupErr = cleanupErr
	worker.reaping = true
	worker.mu.Unlock()

	waitErr = worker.wait()

	worker.mu.Lock()
	worker.waitErr = waitErr
	worker.reaped = true
	close(worker.done)
	worker.mu.Unlock()
	return waitErr, cleanupErr
}

func (s *Supervisor) run(ctx context.Context, input []byte, width, height int) (Derivatives, FailureReason) {
	totalTimer := time.NewTimer(s.totalLimit)
	defer totalTimer.Stop()
	args := formatWorkerArgs(input, width, height)
	cmd := exec.CommandContext(ctx, s.workerPath, args...) // #nosec G204 -- verified fixed executable and bounded numeric arguments.
	cmd.Dir = "/"
	cmd.Env = []string{workerEnvironmentProcs, workerEnvironmentMemory}
	cmd.ExtraFiles = nil

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Derivatives{}, FailureSetup
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Derivatives{}, closeSetupPipes(FailureSetup, stdin)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Derivatives{}, closeSetupPipes(FailureSetup, stdin, stdout)
	}
	if err := configureProcess(cmd); err != nil {
		return Derivatives{}, closeSetupPipes(FailureSetup, stdin, stdout, stderr)
	}
	if err := setCloseOnExecForExtraFiles(); err != nil {
		return Derivatives{}, closeSetupPipes(FailureSetup, stdin, stdout, stderr)
	}
	child := newWorkerProcess(cmd)
	cmd.Cancel = child.cancel
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return Derivatives{}, closeSetupPipes(FailureCanceled, stdin, stdout, stderr)
		}
		return Derivatives{}, closeSetupPipes(FailureSetup, stdin, stdout, stderr)
	}

	writes := make(chan writeResult, 1)
	outputs := make(chan readResult, 1)
	stderrs := make(chan error, 1)
	exits := make(chan error, 1)
	go func() {
		_, writeErr := io.Copy(stdin, bytes.NewReader(input))
		closeErr := stdin.Close()
		writes <- writeResult{err: errors.Join(writeErr, closeErr)}
	}()
	go func() {
		data, readErr := io.ReadAll(io.LimitReader(stdout, maxFrameBytes+1))
		outputs <- readResult{data: data, err: readErr}
	}()
	go func() {
		stderrs <- readBoundedStderr(stderr)
	}()

	var writeDone, outputDone, stderrDone, exitObserved bool
	exitWaitStarted := false
	var written writeResult
	var output readResult
	var stderrErr, exitObservationErr error
	var afterInputTimer *time.Timer
	var afterInput <-chan time.Time
	startExitWait := func() {
		if exitWaitStarted {
			return
		}
		exitWaitStarted = true
		go func() { exits <- waitProcessExitUnreaped(cmd.Process.Pid) }()
	}
	defer func() {
		if afterInputTimer != nil {
			afterInputTimer.Stop()
		}
	}()

	abort := func(reason FailureReason) (Derivatives, FailureReason) {
		var cleanupErr error
		if err := child.cancel(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if err := closePipe(stdin); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		waitErr, reapErr := child.cleanupAndWait()
		if reapErr != nil {
			cleanupErr = errors.Join(cleanupErr, reapErr)
		}
		if waitErr != nil && !errors.Is(waitErr, context.Canceled) {
			if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); !ok || exitErr == nil {
				cleanupErr = errors.Join(cleanupErr, waitErr)
			}
		}
		if err := closePipe(stdout); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if err := closePipe(stderr); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if !writeDone {
			written = <-writes
			writeDone = true
		}
		if !outputDone {
			output = <-outputs
			outputDone = true
		}
		if !stderrDone {
			stderrErr = <-stderrs
			stderrDone = true
		}
		if output.err != nil && !errors.Is(output.err, os.ErrClosed) {
			cleanupErr = errors.Join(cleanupErr, output.err)
		}
		if stderrErr != nil && !errors.Is(stderrErr, os.ErrClosed) {
			cleanupErr = errors.Join(cleanupErr, stderrErr)
		}
		if cleanupErr != nil {
			return Derivatives{}, FailureCleanup
		}
		return Derivatives{}, reason
	}
	finish := func(result Derivatives, reason FailureReason) (Derivatives, FailureReason) {
		waitErr, cleanupErr := child.cleanupAndWait()
		if cleanupErr != nil {
			return Derivatives{}, FailureCleanup
		}
		if ctx.Err() != nil {
			return Derivatives{}, FailureCanceled
		}
		if waitErr != nil {
			if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok && exitErr != nil {
				return Derivatives{}, FailureWorker
			}
			return Derivatives{}, FailureCleanup
		}
		return result, reason
	}

	for {
		if ctx.Err() != nil {
			return abort(FailureCanceled)
		}
		if outputDone && stderrDone {
			startExitWait()
		}
		if writeDone && outputDone && stderrDone && exitObserved {
			if written.err != nil {
				return finish(Derivatives{}, FailureInput)
			}
			if output.err != nil || stderrErr != nil || len(output.data) > maxFrameBytes {
				return finish(Derivatives{}, FailureOutput)
			}
			result, reason := parseFrame(output.data, width, height)
			return finish(result, reason)
		}

		select {
		case <-ctx.Done():
			return abort(FailureCanceled)
		case <-totalTimer.C:
			return abort(FailureTimeout)
		case <-afterInput:
			return abort(FailureTimeout)
		case value := <-writes:
			writeDone = true
			written = value
			if value.err != nil {
				return abort(FailureInput)
			}
			afterInputTimer = time.NewTimer(s.inputLimit)
			afterInput = afterInputTimer.C
		case value := <-outputs:
			outputDone = true
			output = value
			if value.err != nil || len(value.data) > maxFrameBytes {
				return abort(FailureOutput)
			}
		case value := <-stderrs:
			stderrDone = true
			stderrErr = value
			if value != nil {
				return abort(FailureOutput)
			}
		case value := <-exits:
			exitObserved = true
			exitObservationErr = value
			if exitObservationErr != nil {
				return abort(FailureWorker)
			}
		}
	}
}

type boundedBuffer struct {
	bytes []byte
}

func closeSetupPipes(reason FailureReason, closers ...io.Closer) FailureReason {
	var closeErr error
	for _, closer := range closers {
		if err := closePipe(closer); err != nil {
			closeErr = errors.Join(closeErr, err)
		}
	}
	if closeErr != nil {
		return FailureCleanup
	}
	return reason
}

func killCommand(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	var killErr error
	if err := killProcessGroup(cmd.Process.Pid); err != nil {
		killErr = errors.Join(killErr, err)
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		killErr = errors.Join(killErr, err)
	}
	return killErr
}

func closePipe(closer io.Closer) error {
	if err := closer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := maxStderrBytes - len(buffer.bytes)
	if remaining > 0 {
		buffer.bytes = append(buffer.bytes, value[:min(len(value), remaining)]...)
	}
	return len(value), nil
}

func parseFrame(frame []byte, inputWidth, inputHeight int) (Derivatives, FailureReason) {
	if len(frame) < 16 || len(frame) > maxFrameBytes || string(frame[:4]) != "PTD1" || frame[4] != 1 || frame[5]&^byte(3) != 0 {
		return Derivatives{}, FailureOutput
	}
	flags := frame[5]
	if flags&2 != 0 && flags&1 == 0 {
		return Derivatives{}, FailureOutput
	}
	mWidth := int(binary.BigEndian.Uint16(frame[6:8]))
	mHeight := int(binary.BigEndian.Uint16(frame[8:10]))
	mLength := int(binary.BigEndian.Uint32(frame[10:14]))
	position := 14
	if mLength > maxThumbnailBytes || position+mLength+2 > len(frame) {
		return Derivatives{}, FailureOutput
	}
	mBytes := frame[position : position+mLength]
	position += mLength
	strippedLength := int(binary.BigEndian.Uint16(frame[position : position+2]))
	position += 2
	if strippedLength < 4 || strippedLength > maxStrippedBytes || position+strippedLength != len(frame) {
		return Derivatives{}, FailureOutput
	}
	stripped := frame[position:]

	hasM := flags&1 != 0
	wantsM := max(inputWidth, inputHeight) > thumbnailLongSide
	if hasM != wantsM {
		return Derivatives{}, FailureOutput
	}
	if !hasM {
		if mWidth != 0 || mHeight != 0 || mLength != 0 || flags&2 != 0 {
			return Derivatives{}, FailureOutput
		}
		mBytes = nil
	} else {
		if mLength < 1 || !scaledDimensionsMatch(inputWidth, inputHeight, mWidth, mHeight, thumbnailLongSide) {
			return Derivatives{}, FailureValidation
		}
		if err := validateDerivativeJPEG(mBytes, mWidth, mHeight); err != nil {
			return Derivatives{}, FailureValidation
		}
	}
	if !scaledDimensionsMatch(inputWidth, inputHeight, int(stripped[2]), int(stripped[1]), strippedLongSide) {
		return Derivatives{}, FailureValidation
	}
	reconstructed, err := photowire.ReconstructStripped(stripped)
	if err != nil {
		return Derivatives{}, FailureValidation
	}
	if err := validateDerivativeJPEG(reconstructed, int(stripped[2]), int(stripped[1])); err != nil {
		return Derivatives{}, FailureValidation
	}
	return Derivatives{
		M:        mBytes,
		MWidth:   mWidth,
		MHeight:  mHeight,
		Stripped: stripped,
		Retried:  flags&2 != 0,
	}, FailureNone
}

func scaledDimensionsMatch(inputWidth, inputHeight, outputWidth, outputHeight, targetLong int) bool {
	if inputWidth < 1 || inputHeight < 1 || outputWidth < 1 || outputHeight < 1 {
		return false
	}
	inputLong := max(inputWidth, inputHeight)
	if max(outputWidth, outputHeight) != targetLong {
		return false
	}
	wantWidth := int(math.Round(float64(inputWidth) * float64(targetLong) / float64(inputLong)))
	wantHeight := int(math.Round(float64(inputHeight) * float64(targetLong) / float64(inputLong)))
	return absInt(outputWidth-wantWidth) <= 1 && absInt(outputHeight-wantHeight) <= 1
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func validateDerivativeJPEG(encoded []byte, wantWidth, wantHeight int) error {
	width, height, frameMarker, err := scanSingleJPEG(encoded)
	if err != nil {
		return err
	}
	if width != wantWidth || height != wantHeight || frameMarker != 0xc0 {
		return errors.New("JPEG frame does not match advertised dimensions or encoding")
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || config.Width != wantWidth || config.Height != wantHeight {
		return errors.New("JPEG header does not match advertised dimensions")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil || decoded.Bounds().Dx() != wantWidth || decoded.Bounds().Dy() != wantHeight {
		return errors.New("JPEG pixels do not match advertised dimensions")
	}
	return nil
}

func scanSingleJPEG(encoded []byte) (int, int, byte, error) {
	if len(encoded) < 4 || encoded[0] != 0xff || encoded[1] != 0xd8 {
		return 0, 0, 0, errors.New("missing JPEG SOI")
	}
	position := 2
	frameMarker := byte(0)
	var width, height int
	for position < len(encoded) {
		if encoded[position] != 0xff {
			return 0, 0, 0, errors.New("invalid JPEG marker prefix")
		}
		for position < len(encoded) && encoded[position] == 0xff {
			position++
		}
		if position >= len(encoded) {
			return 0, 0, 0, errors.New("truncated JPEG marker")
		}
		marker := encoded[position]
		position++
		if marker == 0xd8 || marker == 0xd9 || marker >= 0xd0 && marker <= 0xd7 || marker == 0x01 || position+2 > len(encoded) {
			return 0, 0, 0, errors.New("invalid standalone JPEG marker")
		}
		segmentLength := int(binary.BigEndian.Uint16(encoded[position : position+2]))
		if segmentLength < 2 || position+segmentLength > len(encoded) {
			return 0, 0, 0, errors.New("invalid JPEG segment length")
		}
		payload := encoded[position+2 : position+segmentLength]
		position += segmentLength
		switch marker {
		case 0xc0, 0xc1, 0xc2:
			if frameMarker != 0 || len(payload) < 5 {
				return 0, 0, 0, errors.New("invalid JPEG frame header")
			}
			frameMarker = marker
			height = int(binary.BigEndian.Uint16(payload[1:3]))
			width = int(binary.BigEndian.Uint16(payload[3:5]))
		case 0xda:
			if frameMarker == 0 {
				return 0, 0, 0, errors.New("JPEG scan precedes frame header")
			}
			for position < len(encoded) {
				if encoded[position] != 0xff {
					position++
					continue
				}
				position++
				for position < len(encoded) && encoded[position] == 0xff {
					position++
				}
				if position >= len(encoded) {
					return 0, 0, 0, errors.New("truncated JPEG entropy marker")
				}
				entropyMarker := encoded[position]
				position++
				if entropyMarker == 0x00 || entropyMarker >= 0xd0 && entropyMarker <= 0xd7 {
					continue
				}
				if entropyMarker != 0xd9 || position != len(encoded) {
					return 0, 0, 0, errors.New("JPEG has multiple scans or trailing bytes")
				}
				return width, height, frameMarker, nil
			}
			return 0, 0, 0, errors.New("JPEG has no terminal EOI")
		}
	}
	return 0, 0, 0, errors.New("JPEG has no scan")
}

func readBoundedStderr(reader io.Reader) error {
	var retained boundedBuffer
	_, err := io.Copy(&retained, reader)
	return err
}

func formatWorkerArgs(input []byte, width, height int) []string {
	return []string{"worker", strconv.Itoa(len(input)), strconv.Itoa(width), strconv.Itoa(height), workerPixelLimitArgument}
}

func safePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.ContainsRune(path, '\x00')
}
