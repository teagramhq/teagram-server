package mtproto

import (
	"errors"
	"math"
	"runtime"
	"sync/atomic"
)

const (
	RPCProgressSchemaVersion int32 = 1
	RPCProgressMaxCount            = int64(math.MaxInt32)
	rpcProgressMethodCount         = 4
	rpcProgressResultCount         = 9
)

var (
	ErrRPCProgressDisabled        = errors.New("rpc progress is disabled")
	ErrRPCProgressInvalidIdentity = errors.New("rpc progress identity is invalid")
	ErrRPCProgressInvalidBoundary = errors.New("rpc progress boundary is invalid")
	ErrRPCProgressInconsistent    = errors.New("rpc progress snapshot is inconsistent")
	ErrRPCProgressCounterReset    = errors.New("rpc progress counter reset")
	ErrRPCProgressCounterOverflow = errors.New("rpc progress counter overflow")
	ErrRPCProgressCancelled       = errors.New("rpc progress observation was cancelled")
	ErrRPCProgressUnavailable     = errors.New("rpc progress observation is unavailable")
	ErrRPCProgressIncomplete      = errors.New("rpc progress shutdown is incomplete")
)

type RPCProgressBoundary string

const (
	RPCProgressBoundaryArtifactReady RPCProgressBoundary = "artifact_ready"
	RPCProgressBoundaryRunnerExit    RPCProgressBoundary = "runner_exit"
	RPCProgressBoundaryShutdownFinal RPCProgressBoundary = "shutdown_final"
)

// RPCProgressIdentity binds observations to one immutable fixture execution.
type RPCProgressIdentity struct {
	RunID      string                `json:"run_id"`
	SourceRefs RPCProgressSourceRefs `json:"source_refs"`
}

// RPCProgressSourceRefs contains only full immutable source references.
type RPCProgressSourceRefs struct {
	RunnerRevision         string `json:"runner_revision"`
	ArtifactSourceRevision string `json:"artifact_source_revision"`
	ArtifactDigest         string `json:"artifact_digest"`
	HarnessRevision        string `json:"harness_revision"`
	ServerRevision         string `json:"server_revision"`
}

type RPCProgressResultCounts struct {
	Success          int64 `json:"success"`
	PasswordRequired int64 `json:"password_required"`
	InvalidRequest   int64 `json:"invalid_request"`
	Unauthenticated  int64 `json:"unauthenticated"`
	Unauthorized     int64 `json:"unauthorized"`
	RateLimited      int64 `json:"rate_limited"`
	Deadline         int64 `json:"deadline"`
	Internal         int64 `json:"internal"`
	TransportFailure int64 `json:"transport_failure"`
}

type RPCProgressMethodSnapshot struct {
	Started int64                   `json:"started"`
	Settled RPCProgressResultCounts `json:"settled"`
	Pending int64                   `json:"pending"`
}

type RPCProgressMethods struct {
	AuthSendCode      RPCProgressMethodSnapshot `json:"auth.sendCode"`
	AuthSignIn        RPCProgressMethodSnapshot `json:"auth.signIn"`
	AuthCheckPassword RPCProgressMethodSnapshot `json:"auth.checkPassword"`
	Other             RPCProgressMethodSnapshot `json:"other"`
}

type RPCProgressConnections struct {
	AuthKeyCreated            int64 `json:"auth_key_created"`
	TransportNegotiationError int64 `json:"transport_negotiation_error"`
}

type RPCProgressExporter struct {
	Enqueued          int64 `json:"enqueued"`
	Exported          int64 `json:"exported"`
	Dropped           int64 `json:"dropped"`
	ExporterErrors    int64 `json:"exporter_errors"`
	ExporterPanics    int64 `json:"exporter_panics"`
	ExportOutstanding int64 `json:"export_outstanding"`
}

// RPCProgressSnapshot is a versioned, closed-key, bounded counter record.
type RPCProgressSnapshot struct {
	SchemaVersion    int32                  `json:"schema_version"`
	Identity         RPCProgressIdentity    `json:"identity"`
	Boundary         RPCProgressBoundary    `json:"boundary"`
	Methods          RPCProgressMethods     `json:"methods"`
	Connections      RPCProgressConnections `json:"connections"`
	RPCPending       int64                  `json:"rpc_pending"`
	Exporter         RPCProgressExporter    `json:"exporter"`
	Overflow         bool                   `json:"overflow"`
	Inconsistent     bool                   `json:"inconsistent"`
	Cancelled        bool                   `json:"cancelled"`
	ShutdownComplete bool                   `json:"shutdown_complete"`
}

type RPCProgressMethodDelta struct {
	Started       int64                   `json:"started"`
	Settled       RPCProgressResultCounts `json:"settled"`
	PendingBefore int64                   `json:"pending_before"`
	PendingAfter  int64                   `json:"pending_after"`
}

type RPCProgressMethodsDelta struct {
	AuthSendCode      RPCProgressMethodDelta `json:"auth.sendCode"`
	AuthSignIn        RPCProgressMethodDelta `json:"auth.signIn"`
	AuthCheckPassword RPCProgressMethodDelta `json:"auth.checkPassword"`
	Other             RPCProgressMethodDelta `json:"other"`
}

type RPCProgressExporterDelta struct {
	Enqueued                int64 `json:"enqueued"`
	Exported                int64 `json:"exported"`
	Dropped                 int64 `json:"dropped"`
	ExporterErrors          int64 `json:"exporter_errors"`
	ExporterPanics          int64 `json:"exporter_panics"`
	ExportOutstandingBefore int64 `json:"export_outstanding_before"`
	ExportOutstandingAfter  int64 `json:"export_outstanding_after"`
}

// RPCProgressChange contains monotonic deltas between artifact-ready and runner-exit.
type RPCProgressChange struct {
	Identity         RPCProgressIdentity      `json:"identity"`
	Methods          RPCProgressMethodsDelta  `json:"methods"`
	Connections      RPCProgressConnections   `json:"connections"`
	RPCPendingBefore int64                    `json:"rpc_pending_before"`
	RPCPendingAfter  int64                    `json:"rpc_pending_after"`
	Exporter         RPCProgressExporterDelta `json:"exporter"`
}

type saturatingCounter struct {
	value atomic.Uint32
}

func rpcProgressMethodIndex(method string) int {
	switch method {
	case "auth.sendCode":
		return 0
	case "auth.signIn":
		return 1
	case "auth.checkPassword":
		return 2
	default:
		return 3
	}
}

func rpcProgressResultIndex(result RPCResultClass) int {
	switch sanitizeRPCResult(result) {
	case RPCResultSuccess:
		return 0
	case RPCResultPasswordRequired:
		return 1
	case RPCResultInvalidRequest:
		return 2
	case RPCResultUnauthenticated:
		return 3
	case RPCResultUnauthorized:
		return 4
	case RPCResultRateLimited:
		return 5
	case RPCResultDeadline:
		return 6
	case RPCResultInternal:
		return 7
	case RPCResultTransportFailure:
		return 8
	default:
		return 7
	}
}

func (c *saturatingCounter) increment(overflow *atomic.Bool) {
	for {
		current := c.value.Load()
		if current >= uint32(RPCProgressMaxCount) {
			overflow.Store(true)
			return
		}
		if c.value.CompareAndSwap(current, current+1) {
			return
		}
	}
}

func (c *saturatingCounter) load() int64 {
	return int64(c.value.Load())
}

// Validate checks that every identity field is a complete immutable reference.
func (identity RPCProgressIdentity) Validate() error {
	if !isLowerHex(identity.RunID, 32) ||
		!isLowerHex(identity.SourceRefs.RunnerRevision, 40) ||
		!isLowerHex(identity.SourceRefs.ArtifactSourceRevision, 40) ||
		!isLowerHex(identity.SourceRefs.HarnessRevision, 40) ||
		!isLowerHex(identity.SourceRefs.ServerRevision, 40) ||
		len(identity.SourceRefs.ArtifactDigest) != len("sha256:")+64 ||
		identity.SourceRefs.ArtifactDigest[:len("sha256:")] != "sha256:" ||
		!isLowerHex(identity.SourceRefs.ArtifactDigest[len("sha256:"):], 64) {
		return ErrRPCProgressInvalidIdentity
	}
	return nil
}

func isLowerHex(value string, wantLength int) bool {
	if len(value) != wantLength {
		return false
	}
	for i := range value {
		if (value[i] < '0' || value[i] > '9') && (value[i] < 'a' || value[i] > 'f') {
			return false
		}
	}
	return true
}

// Validate rejects snapshots that cannot safely be used as an observation.
func (snapshot RPCProgressSnapshot) Validate() error {
	if snapshot.SchemaVersion != RPCProgressSchemaVersion {
		return ErrRPCProgressInconsistent
	}
	if err := snapshot.Identity.Validate(); err != nil {
		return err
	}
	switch snapshot.Boundary {
	case RPCProgressBoundaryArtifactReady, RPCProgressBoundaryRunnerExit, RPCProgressBoundaryShutdownFinal:
	default:
		return ErrRPCProgressInvalidBoundary
	}
	if snapshot.Inconsistent {
		return ErrRPCProgressInconsistent
	}
	if snapshot.Overflow {
		return ErrRPCProgressCounterOverflow
	}
	if snapshot.Cancelled {
		return ErrRPCProgressCancelled
	}
	if err := validateMethodSnapshot(snapshot.Methods.AuthSendCode); err != nil {
		return err
	}
	if err := validateMethodSnapshot(snapshot.Methods.AuthSignIn); err != nil {
		return err
	}
	if err := validateMethodSnapshot(snapshot.Methods.AuthCheckPassword); err != nil {
		return err
	}
	if err := validateMethodSnapshot(snapshot.Methods.Other); err != nil {
		return err
	}
	pending := snapshot.Methods.AuthSendCode.Pending + snapshot.Methods.AuthSignIn.Pending +
		snapshot.Methods.AuthCheckPassword.Pending + snapshot.Methods.Other.Pending
	if !validProgressCount(snapshot.RPCPending) || pending != snapshot.RPCPending {
		return ErrRPCProgressInconsistent
	}
	if !validProgressCount(snapshot.Connections.AuthKeyCreated) || !validProgressCount(snapshot.Connections.TransportNegotiationError) {
		return ErrRPCProgressInconsistent
	}
	if err := validateExporterSnapshot(snapshot.Exporter); err != nil {
		return err
	}
	if snapshot.Exporter.Dropped != 0 || snapshot.Exporter.ExporterErrors != 0 || snapshot.Exporter.ExporterPanics != 0 {
		return ErrRPCProgressUnavailable
	}
	if snapshot.Boundary == RPCProgressBoundaryShutdownFinal {
		if !snapshot.ShutdownComplete || snapshot.RPCPending != 0 || snapshot.Exporter.ExportOutstanding != 0 {
			return ErrRPCProgressIncomplete
		}
	} else if snapshot.ShutdownComplete {
		return ErrRPCProgressInconsistent
	}
	return nil
}

func validateMethodSnapshot(method RPCProgressMethodSnapshot) error {
	if !validProgressCount(method.Started) || !validProgressCount(method.Pending) {
		return ErrRPCProgressInconsistent
	}
	settled := method.Settled.counts()
	for _, count := range settled {
		if !validProgressCount(count) {
			return ErrRPCProgressInconsistent
		}
	}
	total := int64(0)
	for _, count := range settled {
		total += count
	}
	if total+method.Pending != method.Started {
		return ErrRPCProgressInconsistent
	}
	return nil
}

func validateExporterSnapshot(exporter RPCProgressExporter) error {
	for _, count := range []int64{
		exporter.Enqueued, exporter.Exported, exporter.Dropped, exporter.ExporterErrors,
		exporter.ExporterPanics, exporter.ExportOutstanding,
	} {
		if !validProgressCount(count) {
			return ErrRPCProgressInconsistent
		}
	}
	if exporter.Exported+exporter.ExporterErrors+exporter.ExporterPanics+exporter.ExportOutstanding != exporter.Enqueued {
		return ErrRPCProgressInconsistent
	}
	return nil
}

func (counts RPCProgressResultCounts) counts() []int64 {
	return []int64{
		counts.Success, counts.PasswordRequired, counts.InvalidRequest, counts.Unauthenticated,
		counts.Unauthorized, counts.RateLimited, counts.Deadline, counts.Internal, counts.TransportFailure,
	}
}

func validProgressCount(value int64) bool {
	return value >= 0 && value <= RPCProgressMaxCount
}

// RPCProgressDelta validates and subtracts the two fixture observation boundaries.
func RPCProgressDelta(before, after RPCProgressSnapshot) (RPCProgressChange, error) {
	if err := before.Validate(); err != nil {
		return RPCProgressChange{}, err
	}
	if err := after.Validate(); err != nil {
		return RPCProgressChange{}, err
	}
	if before.Boundary != RPCProgressBoundaryArtifactReady || after.Boundary != RPCProgressBoundaryRunnerExit {
		return RPCProgressChange{}, ErrRPCProgressInvalidBoundary
	}
	if before.Identity != after.Identity {
		return RPCProgressChange{}, ErrRPCProgressInvalidIdentity
	}

	var change RPCProgressChange
	change.Identity = after.Identity
	var err error
	if change.Methods.AuthSendCode, err = methodDelta(before.Methods.AuthSendCode, after.Methods.AuthSendCode); err != nil {
		return RPCProgressChange{}, err
	}
	if change.Methods.AuthSignIn, err = methodDelta(before.Methods.AuthSignIn, after.Methods.AuthSignIn); err != nil {
		return RPCProgressChange{}, err
	}
	if change.Methods.AuthCheckPassword, err = methodDelta(before.Methods.AuthCheckPassword, after.Methods.AuthCheckPassword); err != nil {
		return RPCProgressChange{}, err
	}
	if change.Methods.Other, err = methodDelta(before.Methods.Other, after.Methods.Other); err != nil {
		return RPCProgressChange{}, err
	}
	change.RPCPendingBefore = before.RPCPending
	change.RPCPendingAfter = after.RPCPending
	change.Connections.AuthKeyCreated, err = monotonicDelta(before.Connections.AuthKeyCreated, after.Connections.AuthKeyCreated)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Connections.TransportNegotiationError, err = monotonicDelta(before.Connections.TransportNegotiationError, after.Connections.TransportNegotiationError)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.Enqueued, err = monotonicDelta(before.Exporter.Enqueued, after.Exporter.Enqueued)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.Exported, err = monotonicDelta(before.Exporter.Exported, after.Exporter.Exported)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.Dropped, err = monotonicDelta(before.Exporter.Dropped, after.Exporter.Dropped)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.ExporterErrors, err = monotonicDelta(before.Exporter.ExporterErrors, after.Exporter.ExporterErrors)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.ExporterPanics, err = monotonicDelta(before.Exporter.ExporterPanics, after.Exporter.ExporterPanics)
	if err != nil {
		return RPCProgressChange{}, err
	}
	change.Exporter.ExportOutstandingBefore = before.Exporter.ExportOutstanding
	change.Exporter.ExportOutstandingAfter = after.Exporter.ExportOutstanding
	return change, nil
}

func methodDelta(before, after RPCProgressMethodSnapshot) (RPCProgressMethodDelta, error) {
	var delta RPCProgressMethodDelta
	var err error
	if delta.Started, err = monotonicDelta(before.Started, after.Started); err != nil {
		return RPCProgressMethodDelta{}, err
	}
	beforeCounts, afterCounts := before.Settled.counts(), after.Settled.counts()
	deltaCounts := make([]int64, len(beforeCounts))
	for i := range beforeCounts {
		deltaCounts[i], err = monotonicDelta(beforeCounts[i], afterCounts[i])
		if err != nil {
			return RPCProgressMethodDelta{}, err
		}
	}
	delta.Settled = resultCountsFromSlice(deltaCounts)
	settledTotal := int64(0)
	for _, count := range deltaCounts {
		settledTotal += count
	}
	if settledTotal > before.Pending+delta.Started {
		return RPCProgressMethodDelta{}, ErrRPCProgressInconsistent
	}
	delta.PendingBefore = before.Pending
	delta.PendingAfter = after.Pending
	if before.Pending+delta.Started-settledTotal != after.Pending {
		return RPCProgressMethodDelta{}, ErrRPCProgressInconsistent
	}
	return delta, nil
}

func resultCountsFromSlice(counts []int64) RPCProgressResultCounts {
	return RPCProgressResultCounts{
		Success: counts[0], PasswordRequired: counts[1], InvalidRequest: counts[2],
		Unauthenticated: counts[3], Unauthorized: counts[4], RateLimited: counts[5],
		Deadline: counts[6], Internal: counts[7], TransportFailure: counts[8],
	}
}

func monotonicDelta(before, after int64) (int64, error) {
	if !validProgressCount(before) || !validProgressCount(after) {
		return 0, ErrRPCProgressInconsistent
	}
	if after < before {
		return 0, ErrRPCProgressCounterReset
	}
	return after - before, nil
}

func (t *RPCTracer) ProgressSnapshot(identity RPCProgressIdentity, boundary RPCProgressBoundary) (RPCProgressSnapshot, error) {
	if t == nil || t.done == nil {
		return RPCProgressSnapshot{}, ErrRPCProgressDisabled
	}
	if err := identity.Validate(); err != nil {
		return RPCProgressSnapshot{}, err
	}
	switch boundary {
	case RPCProgressBoundaryArtifactReady, RPCProgressBoundaryRunnerExit, RPCProgressBoundaryShutdownFinal:
	default:
		return RPCProgressSnapshot{}, ErrRPCProgressInvalidBoundary
	}

	counts := t.progressCounts()
	snapshot := RPCProgressSnapshot{
		SchemaVersion:    RPCProgressSchemaVersion,
		Identity:         identity,
		Boundary:         boundary,
		Methods:          counts.methods,
		Connections:      counts.connections,
		RPCPending:       counts.rpcPending,
		Exporter:         counts.exporter,
		Overflow:         counts.overflow,
		Inconsistent:     counts.inconsistent,
		Cancelled:        counts.cancelled,
		ShutdownComplete: counts.shutdownComplete,
	}
	return snapshot, nil
}

type rpcProgressCounts struct {
	methods          RPCProgressMethods
	connections      RPCProgressConnections
	rpcPending       int64
	exporter         RPCProgressExporter
	overflow         bool
	inconsistent     bool
	cancelled        bool
	shutdownComplete bool
}

func (t *RPCTracer) progressCounts() rpcProgressCounts {
	for range 4 {
		revision := t.progressRevision.Load()
		if t.progressWriters.Load() != 0 {
			runtime.Gosched()
			continue
		}
		counts := t.readProgressCounts()
		if t.progressWriters.Load() == 0 && t.progressRevision.Load() == revision {
			return counts
		}
		runtime.Gosched()
	}
	counts := t.readProgressCounts()
	counts.inconsistent = true
	return counts
}

func (t *RPCTracer) readProgressCounts() rpcProgressCounts {
	var counts rpcProgressCounts
	loadMethod := func(started *saturatingCounter, settled *[rpcProgressResultCount]saturatingCounter) RPCProgressMethodSnapshot {
		var resultCounts [rpcProgressResultCount]int64
		settledTotal := int64(0)
		for i := range settled {
			resultCounts[i] = settled[i].load()
			settledTotal += resultCounts[i]
		}
		startedCount := started.load()
		pending := int64(0)
		if settledTotal > startedCount {
			counts.inconsistent = true
		} else {
			pending = startedCount - settledTotal
		}
		if settledTotal > RPCProgressMaxCount {
			counts.overflow = true
		}
		return RPCProgressMethodSnapshot{
			Started: startedCount,
			Settled: resultCountsFromArray(resultCounts),
			Pending: pending,
		}
	}
	counts.methods.AuthSendCode = loadMethod(&t.rpcStarted[0], &t.rpcSettled[0])
	counts.methods.AuthSignIn = loadMethod(&t.rpcStarted[1], &t.rpcSettled[1])
	counts.methods.AuthCheckPassword = loadMethod(&t.rpcStarted[2], &t.rpcSettled[2])
	counts.methods.Other = loadMethod(&t.rpcStarted[3], &t.rpcSettled[3])
	for _, pending := range []int64{counts.methods.AuthSendCode.Pending, counts.methods.AuthSignIn.Pending, counts.methods.AuthCheckPassword.Pending, counts.methods.Other.Pending} {
		if counts.rpcPending > RPCProgressMaxCount-pending {
			counts.rpcPending = RPCProgressMaxCount
			counts.overflow = true
		} else {
			counts.rpcPending += pending
		}
	}
	counts.connections = RPCProgressConnections{
		AuthKeyCreated:            t.authKeyCreated.load(),
		TransportNegotiationError: t.transportNegotiationError.load(),
	}
	// Export outcomes are loaded before enqueued so an observed completion
	// always has its earlier enqueue included in this read.
	counts.exporter.Exported = t.exported.load()
	counts.exporter.ExporterErrors = t.exporterErrors.load()
	counts.exporter.ExporterPanics = t.exporterPanics.load()
	counts.exporter.Dropped = t.dropped.load()
	counts.exporter.Enqueued = t.enqueued.load()
	exportOutcomes := counts.exporter.Exported + counts.exporter.ExporterErrors + counts.exporter.ExporterPanics
	if exportOutcomes > counts.exporter.Enqueued {
		counts.inconsistent = true
	} else {
		counts.exporter.ExportOutstanding = counts.exporter.Enqueued - exportOutcomes
	}
	counts.overflow = counts.overflow || t.overflow.Load()
	counts.cancelled = t.cancelled.Load()
	counts.shutdownComplete = t.shutdownComplete.Load()
	return counts
}

func resultCountsFromArray(counts [rpcProgressResultCount]int64) RPCProgressResultCounts {
	return resultCountsFromSlice(counts[:])
}
