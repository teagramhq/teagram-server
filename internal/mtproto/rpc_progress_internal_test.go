package mtproto

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/crypto"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

func TestRPCProgressCountsPendingBeforeHandlerReturns(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil })})
	t.Cleanup(func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close tracer: %v", err)
		}
	})

	started := make(chan struct{})
	release := make(chan struct{})
	d := NewDispatcher().HandleFuncNamed(tg.AuthSignInRequestTypeID, "auth.signIn", func(*Conn, *Request) error {
		close(started)
		<-release
		return nil
	})
	var body bin.Buffer
	wrapped := &tg.InvokeWithLayerRequest{
		Layer: 100,
		Query: &tg.InitConnectionRequest{
			APIID:          1,
			DeviceModel:    "fixture-device",
			SystemVersion:  "fixture-system",
			AppVersion:     "fixture-app",
			SystemLangCode: "en",
			LangCode:       "en",
			Query: &tg.AuthSignInRequest{
				PhoneNumber:   "+15550000000",
				PhoneCodeHash: "fixture-hash",
				PhoneCode:     "12345",
			},
		},
	}
	if err := wrapped.Encode(&body); err != nil {
		t.Fatalf("encode wrapped signIn: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		result <- tracer.Wrap(UnpackInvoke(d)).OnMessage(nil, &Request{Buf: &body, Ctx: context.Background()})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}

	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryArtifactReady)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if got := snapshot.Methods.AuthSignIn.Started; got != 1 {
		t.Fatalf("signIn started = %d, want 1", got)
	}
	if got := snapshot.Methods.AuthSignIn.Pending; got != 1 || snapshot.RPCPending != 1 {
		t.Fatalf("signIn pending = %d, rpc_pending = %d, want 1", got, snapshot.RPCPending)
	}
	if got := snapshot.Methods.AuthSignIn.Settled; got != (RPCProgressResultCounts{}) {
		t.Fatalf("signIn settled while handler is pending: %+v", got)
	}

	close(release)
	if err := <-result; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("close tracer: %v", err)
	}

	final, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryShutdownFinal)
	if err != nil {
		t.Fatalf("final progress snapshot: %v", err)
	}
	if final.RPCPending != 0 || final.Methods.AuthSignIn.Settled.Success != 1 {
		t.Fatalf("final signIn progress = %+v, want one successful settlement", final.Methods.AuthSignIn)
	}
	if !final.ShutdownComplete || final.Exporter.ExportOutstanding != 0 {
		t.Fatalf("final shutdown = %+v, want completed drain", final.Exporter)
	}
}

func TestRPCProgressCollapsesMethodsAndClassifiesPasswordRequiredExactly(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil })})
	defer func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close tracer: %v", err)
		}
	}()

	for _, tc := range []struct {
		name string
		err  error
		want RPCResultClass
	}{
		{name: "exact password challenge", err: tgerr.New(401, "SESSION_PASSWORD_NEEDED"), want: RPCResultPasswordRequired},
		{name: "near miss password challenge", err: tgerr.New(401, "SESSION_PASSWORD_NEEDED_EXTRA"), want: RPCResultUnauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDispatcher().HandleFuncNamed(tg.AuthSignInRequestTypeID, "auth.signIn", func(*Conn, *Request) error { return tc.err })
			var body bin.Buffer
			body.PutID(tg.AuthSignInRequestTypeID)
			if err := tracer.Wrap(d).OnMessage(nil, &Request{Buf: &body, Ctx: context.Background()}); !errors.Is(err, tc.err) {
				t.Fatalf("dispatch error = %v, want %v", err, tc.err)
			}
		})
	}
	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryRunnerExit)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	counts := snapshot.Methods.AuthSignIn.Settled
	if counts.PasswordRequired != 1 || counts.Unauthenticated != 1 {
		t.Fatalf("signIn results = %+v, want exact and near-miss 401 classes", counts)
	}
	if got := ClassifyRPCError(tgerr.New(401, "anything_else")); got != RPCResultUnauthenticated {
		t.Fatalf("near-miss 401 class = %q, want unauthenticated", got)
	}
	if err := tracer.Close(); err != nil {
		t.Fatalf("close tracer: %v", err)
	}
}

func TestRPCProgressSnapshotSchemaAndConsistency(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil })})
	defer func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close tracer: %v", err)
		}
	}()

	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryArtifactReady)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("valid zero snapshot rejected: %v", err)
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode snapshot object: %v", err)
	}
	if got, want := sortedKeys(root), []string{"boundary", "cancelled", "connections", "exporter", "identity", "inconsistent", "methods", "overflow", "rpc_pending", "schema_version", "shutdown_complete"}; !equalStrings(got, want) {
		t.Fatalf("root keys = %v, want %v", got, want)
	}
	var methods map[string]json.RawMessage
	if err := json.Unmarshal(root["methods"], &methods); err != nil {
		t.Fatalf("decode methods object: %v", err)
	}
	if got, want := sortedKeys(methods), []string{"auth.checkPassword", "auth.sendCode", "auth.signIn", "other"}; !equalStrings(got, want) {
		t.Fatalf("method keys = %v, want %v", got, want)
	}
	var method map[string]json.RawMessage
	if err := json.Unmarshal(methods["other"], &method); err != nil {
		t.Fatalf("decode method snapshot: %v", err)
	}
	if got, want := sortedKeys(method), []string{"pending", "settled", "started"}; !equalStrings(got, want) {
		t.Fatalf("method keys = %v, want %v", got, want)
	}
	var settled map[string]json.RawMessage
	if err := json.Unmarshal(method["settled"], &settled); err != nil {
		t.Fatalf("decode settled counts: %v", err)
	}
	if got, want := sortedKeys(settled), []string{"deadline", "internal", "invalid_request", "password_required", "rate_limited", "success", "transport_failure", "unauthenticated", "unauthorized"}; !equalStrings(got, want) {
		t.Fatalf("settlement keys = %v, want %v", got, want)
	}
	var connections map[string]json.RawMessage
	if err := json.Unmarshal(root["connections"], &connections); err != nil {
		t.Fatalf("decode connection counters: %v", err)
	}
	if got, want := sortedKeys(connections), []string{"auth_key_created", "transport_negotiation_error"}; !equalStrings(got, want) {
		t.Fatalf("connection keys = %v, want %v", got, want)
	}
	var exporter map[string]json.RawMessage
	if err := json.Unmarshal(root["exporter"], &exporter); err != nil {
		t.Fatalf("decode exporter counters: %v", err)
	}
	if got, want := sortedKeys(exporter), []string{"dropped", "enqueued", "export_outstanding", "exported", "exporter_errors", "exporter_panics"}; !equalStrings(got, want) {
		t.Fatalf("exporter keys = %v, want %v", got, want)
	}
	var sourceRefs map[string]json.RawMessage
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(root["identity"], &identity); err != nil {
		t.Fatalf("decode identity: %v", err)
	}
	if got, want := sortedKeys(identity), []string{"run_id", "source_refs"}; !equalStrings(got, want) {
		t.Fatalf("identity keys = %v, want %v", got, want)
	}
	if err := json.Unmarshal(identity["source_refs"], &sourceRefs); err != nil {
		t.Fatalf("decode source refs: %v", err)
	}
	if got, want := sortedKeys(sourceRefs), []string{"artifact_digest", "artifact_source_revision", "harness_revision", "runner_revision", "server_revision"}; !equalStrings(got, want) {
		t.Fatalf("source reference keys = %v, want %v", got, want)
	}

	bad := snapshot
	bad.Methods.AuthSignIn.Settled.Success = 1
	if err := bad.Validate(); err == nil {
		t.Fatal("inconsistent started/settled totals were accepted")
	}
	bad = snapshot
	bad.RPCPending = -1
	if err := bad.Validate(); err == nil {
		t.Fatal("negative pending total was accepted")
	}
	bad = snapshot
	bad.Methods.Other.Started = RPCProgressMaxCount + 1
	if err := bad.Validate(); err == nil {
		t.Fatal("out-of-range count was accepted")
	}
	bad = snapshot
	bad.Identity.RunID = "unapproved-run-label"
	if err := bad.Validate(); !errors.Is(err, ErrRPCProgressInvalidIdentity) {
		t.Fatalf("invalid identity error = %v, want invalid identity", err)
	}
	bad = snapshot
	bad.Boundary = RPCProgressBoundaryShutdownFinal
	if err := bad.Validate(); !errors.Is(err, ErrRPCProgressIncomplete) {
		t.Fatalf("incomplete final error = %v, want incomplete", err)
	}
}

func TestRPCProgressDeltaRejectsResetAndKeepsPendingSeparateFromExporter(t *testing.T) {
	identity := testRPCProgressIdentity()
	before := emptyRPCProgressSnapshot(identity, RPCProgressBoundaryArtifactReady)
	after := emptyRPCProgressSnapshot(identity, RPCProgressBoundaryRunnerExit)
	after.Methods.AuthSignIn.Started = 2
	after.Methods.AuthSignIn.Settled.PasswordRequired = 1
	after.Methods.AuthSignIn.Pending = 1
	after.RPCPending = 1
	after.Exporter.Enqueued = 3
	after.Exporter.Exported = 1
	after.Exporter.ExportOutstanding = 2

	delta, err := RPCProgressDelta(before, after)
	if err != nil {
		t.Fatalf("progress delta: %v", err)
	}
	if delta.RPCPending != 1 || delta.Exporter.ExportOutstandingAfter != 2 {
		t.Fatalf("pending/export backlog delta = %d/%d, want 1/2", delta.RPCPending, delta.Exporter.ExportOutstandingAfter)
	}

	resetBefore := before
	resetBefore.Methods.AuthSignIn.Started = 1
	resetBefore.Methods.AuthSignIn.Settled.Success = 1
	reset := after
	reset.Methods.AuthSignIn.Started = 0
	reset.Methods.AuthSignIn.Settled.PasswordRequired = 0
	reset.Methods.AuthSignIn.Pending = 0
	reset.RPCPending = 0
	if _, err := RPCProgressDelta(resetBefore, reset); !errors.Is(err, ErrRPCProgressCounterReset) {
		t.Fatalf("counter reset error = %v, want %v", err, ErrRPCProgressCounterReset)
	}

	inconsistent := after
	inconsistent.Exporter.ExportOutstanding = 1
	if _, err := RPCProgressDelta(before, inconsistent); err == nil {
		t.Fatal("inconsistent export backlog was accepted")
	}
}

func TestRPCProgressCountersSaturateAndMarkOverflow(t *testing.T) {
	var overflow atomic.Bool
	counter := saturatingCounter{}
	counter.value.Store(uint32(RPCProgressMaxCount))
	counter.increment(&overflow)
	if got := counter.value.Load(); got != uint32(RPCProgressMaxCount) {
		t.Fatalf("saturated count = %d, want %d", got, RPCProgressMaxCount)
	}
	if !overflow.Load() {
		t.Fatal("counter saturation did not set overflow")
	}
}

func TestRPCProgressCountsExecutingExportAsOutstanding(t *testing.T) {
	exporter := &progressBlockingExporter{started: make(chan struct{}), release: make(chan struct{})}
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: exporter, QueueCapacity: 2})
	tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("exporter did not start")
	}

	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryArtifactReady)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if snapshot.Exporter.Enqueued != 1 || snapshot.Exporter.Exported != 0 || snapshot.Exporter.ExportOutstanding != 1 {
		t.Fatalf("executing export counters = %+v, want outstanding 1", snapshot.Exporter)
	}
	close(exporter.release)
	if err := tracer.Close(); err != nil {
		t.Fatalf("close tracer: %v", err)
	}
}

func TestRPCProgressRejectsDroppedExporterErrorAndPanicObservations(t *testing.T) {
	t.Run("drop", func(t *testing.T) {
		exporter := &progressBlockingExporter{started: make(chan struct{}), release: make(chan struct{})}
		tracer := NewRPCTracer(RPCTracerConfig{Exporter: exporter, QueueCapacity: 1})
		tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
		select {
		case <-exporter.started:
		case <-time.After(time.Second):
			t.Fatal("exporter did not start")
		}
		tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
		tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
		close(exporter.release)
		if err := tracer.Close(); err != nil {
			t.Fatalf("close tracer: %v", err)
		}
		snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryRunnerExit)
		if err != nil {
			t.Fatalf("progress snapshot: %v", err)
		}
		if snapshot.Exporter.Dropped != 1 {
			t.Fatalf("dropped = %d, want 1", snapshot.Exporter.Dropped)
		}
		if err := snapshot.Validate(); !errors.Is(err, ErrRPCProgressUnavailable) {
			t.Fatalf("validation error = %v, want unavailable", err)
		}
	})

	t.Run("error and panic", func(t *testing.T) {
		var calls atomic.Int64
		tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error {
			switch calls.Add(1) {
			case 1:
				return errors.New("private exporter detail")
			default:
				panic("private panic detail")
			}
		}), QueueCapacity: 2})
		tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
		tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
		if err := tracer.Close(); err != nil {
			t.Fatalf("close tracer: %v", err)
		}
		snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryRunnerExit)
		if err != nil {
			t.Fatalf("progress snapshot: %v", err)
		}
		if snapshot.Exporter.ExporterErrors != 1 || snapshot.Exporter.ExporterPanics != 1 {
			t.Fatalf("exporter failures = %+v, want one error and one panic", snapshot.Exporter)
		}
		if err := snapshot.Validate(); !errors.Is(err, ErrRPCProgressUnavailable) {
			t.Fatalf("validation error = %v, want unavailable", err)
		}
	})
}

func TestRPCProgressCancellationCannotValidateAsZero(t *testing.T) {
	exporter := &progressBlockingExporter{started: make(chan struct{}), release: make(chan struct{})}
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: exporter})
	tracer.Record(RPCSpan{Method: "messages.sendMessage", Result: RPCResultSuccess})
	select {
	case <-exporter.started:
	case <-time.After(time.Second):
		t.Fatal("exporter did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tracer.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("shutdown error = %v, want canceled", err)
	}
	close(exporter.release)
	if err := tracer.Close(); err != nil {
		t.Fatalf("finish tracer shutdown: %v", err)
	}
	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryShutdownFinal)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if err := snapshot.Validate(); !errors.Is(err, ErrRPCProgressCancelled) {
		t.Fatalf("validation error = %v, want cancelled", err)
	}
}

func TestConnectionObservationsCountOnlyPersistedKeysAndEveryNegotiationFailure(t *testing.T) {
	tracer := NewRPCTracer(RPCTracerConfig{Exporter: RPCSpanExporterFunc(func(context.Context, RPCSpan) error { return nil })})
	defer func() {
		if err := tracer.Close(); err != nil {
			t.Errorf("close tracer: %v", err)
		}
	}()

	server := &Server{keys: progressAuthKeyStore{saveErr: errors.New("storage unavailable")}, rpcTracer: tracer, log: slog.New(slog.DiscardHandler)}
	if err := server.persistAuthKey(context.Background(), crypto.AuthKey{}); err == nil {
		t.Fatal("failed auth-key persistence returned nil")
	}
	server.keys = progressAuthKeyStore{}
	if err := server.persistAuthKey(context.Background(), crypto.AuthKey{}); err != nil {
		t.Fatalf("persist auth key: %v", err)
	}
	server.logNegotiation(errors.New("first negotiation failure"))
	server.logNegotiation(errors.New("second negotiation failure"))

	snapshot, err := tracer.ProgressSnapshot(testRPCProgressIdentity(), RPCProgressBoundaryArtifactReady)
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if snapshot.Connections.AuthKeyCreated != 1 || snapshot.Connections.TransportNegotiationError != 2 {
		t.Fatalf("connection observations = %+v, want persisted=1 and negotiation failures=2", snapshot.Connections)
	}
}

func testRPCProgressIdentity() RPCProgressIdentity {
	return RPCProgressIdentity{
		RunID: strings.Repeat("a", 32),
		SourceRefs: RPCProgressSourceRefs{
			RunnerRevision:         strings.Repeat("b", 40),
			ArtifactSourceRevision: strings.Repeat("c", 40),
			ArtifactDigest:         "sha256:" + strings.Repeat("d", 64),
			HarnessRevision:        strings.Repeat("e", 40),
			ServerRevision:         strings.Repeat("f", 40),
		},
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func emptyRPCProgressSnapshot(identity RPCProgressIdentity, boundary RPCProgressBoundary) RPCProgressSnapshot {
	return RPCProgressSnapshot{SchemaVersion: RPCProgressSchemaVersion, Identity: identity, Boundary: boundary}
}

type progressBlockingExporter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *progressBlockingExporter) Export(context.Context, RPCSpan) error {
	e.once.Do(func() { close(e.started) })
	<-e.release
	return nil
}

type progressAuthKeyStore struct {
	saveErr error
}

func (s progressAuthKeyStore) Save(context.Context, crypto.AuthKey) error { return s.saveErr }
func (progressAuthKeyStore) Get(context.Context, [8]byte, time.Duration) (crypto.AuthKey, int64, bool, PendingLogin, bool, error) {
	return crypto.AuthKey{}, 0, false, PendingLogin{}, false, nil
}
func (progressAuthKeyStore) Touch(context.Context, [8]byte) error { return nil }
