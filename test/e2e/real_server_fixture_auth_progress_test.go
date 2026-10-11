package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const (
	fixtureAuthProgressContractVersion  int32 = 1
	fixtureAuthProgressFrontSource            = "front.healthz.websocket_101_count"
	fixtureAuthProgressRunnerRevision         = "20b019aa9556827082bed3646f8fe903e45aa464"
	fixtureAuthProgressArtifactSource         = "569529c1f36923c098760ef727808254dcb6a662"
	fixtureAuthProgressHistoricalDigest       = "sha256:f352e4ef6163855cdbbcc4022386a306b6c768f56e1fa2aa1e35f434ba272f3f"
)

type fixtureAuthProgressReport struct {
	SchemaVersion   int32                         `json:"schema_version"`
	Identity        mtproto.RPCProgressIdentity   `json:"identity"`
	Boundaries      []fixtureAuthProgressBoundary `json:"boundaries"`
	ShutdownFinal   mtproto.RPCProgressSnapshot   `json:"shutdown_final"`
	BindingRejected bool                          `json:"binding_rejected"`
	Incomplete      bool                          `json:"incomplete"`
}

type fixtureAuthProgressBoundary struct {
	Server mtproto.RPCProgressSnapshot `json:"server"`
	Front  fixtureAuthProgressFront    `json:"front"`
}

type fixtureAuthProgressFront struct {
	WSAccepted int64  `json:"ws_accepted"`
	Source     string `json:"source"`
}

type fixtureAuthProgressResult struct {
	SchemaVersion     int32                       `json:"schema_version"`
	Identity          mtproto.RPCProgressIdentity `json:"identity"`
	Available         bool                        `json:"available"`
	UnavailableReason string                      `json:"unavailable_reason,omitempty"`
	Window            *fixtureAuthProgressWindow  `json:"window,omitempty"`
}

type fixtureAuthProgressWindow struct {
	ArtifactReady    fixtureAuthProgressBoundary     `json:"artifact_ready"`
	RunnerExit       fixtureAuthProgressBoundary     `json:"runner_exit"`
	ShutdownFinal    mtproto.RPCProgressSnapshot     `json:"shutdown_final"`
	RPCPendingBefore int64                           `json:"rpc_pending_before"`
	RPCPendingAfter  int64                           `json:"rpc_pending_after"`
	Methods          fixtureAuthProgressMethodsDelta `json:"methods"`
	Connections      fixtureAuthProgressConnections  `json:"connections"`
	Exporter         fixtureAuthProgressExporter     `json:"exporter"`
}

type fixtureAuthProgressMethodsDelta struct {
	AuthSendCode      fixtureAuthProgressMethodDelta `json:"auth.sendCode"`
	AuthSignIn        fixtureAuthProgressMethodDelta `json:"auth.signIn"`
	AuthCheckPassword fixtureAuthProgressMethodDelta `json:"auth.checkPassword"`
	Other             fixtureAuthProgressMethodDelta `json:"other"`
}

type fixtureAuthProgressMethodDelta struct {
	State         string                          `json:"state"`
	Started       int64                           `json:"started"`
	Settled       mtproto.RPCProgressResultCounts `json:"settled"`
	PendingBefore int64                           `json:"pending_before"`
	PendingAfter  int64                           `json:"pending_after"`
}

type fixtureAuthProgressConnections struct {
	AuthKeyCreated            int64                           `json:"auth_key_created"`
	TransportNegotiationError int64                           `json:"transport_negotiation_error"`
	WSAccepted                fixtureAuthProgressCounterDelta `json:"ws_accepted"`
}

type fixtureAuthProgressCounterDelta struct {
	Source string `json:"source"`
	Before int64  `json:"before"`
	After  int64  `json:"after"`
	Delta  int64  `json:"delta"`
}

type fixtureAuthProgressExporter struct {
	Enqueued                int64 `json:"enqueued"`
	Exported                int64 `json:"exported"`
	Dropped                 int64 `json:"dropped"`
	ExporterErrors          int64 `json:"exporter_errors"`
	ExporterPanics          int64 `json:"exporter_panics"`
	ExportOutstandingBefore int64 `json:"export_outstanding_before"`
	ExportOutstandingAfter  int64 `json:"export_outstanding_after"`
	ExportOutstandingFinal  int64 `json:"export_outstanding_final"`
}

type fixtureAuthControlOutcome struct {
	Result   string `json:"result"`
	Accounts int32  `json:"accounts"`
}

type fixtureAuthControlResults struct {
	AuthSendCode       fixtureAuthControlOutcome `json:"auth.sendCode"`
	AuthSignIn         fixtureAuthControlOutcome `json:"auth.signIn"`
	AccountGetPassword fixtureAuthControlOutcome `json:"account.getPassword"`
	AuthCheckPassword  fixtureAuthControlOutcome `json:"auth.checkPassword"`
	AuthSignUp         fixtureAuthControlOutcome `json:"auth.signUp"`
}

type fixtureAuthControlRecord struct {
	Event    string                      `json:"event"`
	Status   string                      `json:"status"`
	Identity mtproto.RPCProgressIdentity `json:"identity"`
	Results  fixtureAuthControlResults   `json:"results"`
}

type fixtureAuthProgressBindingRecord struct {
	Event    string                      `json:"event"`
	Status   string                      `json:"status"`
	Identity mtproto.RPCProgressIdentity `json:"identity"`
}

func TestRealServerFixtureAuthProgressStrictSchemaAndDeltas(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	report := newFixtureAuthProgressTestReport(identity)
	setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSendCode, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSignIn, 2, mtproto.RPCProgressResultCounts{PasswordRequired: 2}, 0)
	setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthCheckPassword, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.Other, 3, mtproto.RPCProgressResultCounts{Success: 2, InvalidRequest: 1}, 0)
	report.Boundaries[1].Server.Connections.AuthKeyCreated = 2
	report.Boundaries[1].Front.WSAccepted = 23
	report.ShutdownFinal = report.Boundaries[1].Server
	report.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
	report.ShutdownFinal.ShutdownComplete = true

	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	got := consumeFixtureAuthProgress(data, identity, false, false)
	if !got.Available || got.UnavailableReason != "" || got.Window == nil {
		t.Fatalf("valid observation = %+v", got)
	}
	if got.Window.Methods.AuthSendCode.State != "settled" || got.Window.Methods.AuthSendCode.Started != 2 || got.Window.Methods.AuthSendCode.Settled.Success != 2 {
		t.Fatalf("sendCode result = %+v", got.Window.Methods.AuthSendCode)
	}
	if got.Window.Methods.AuthSignIn.State != "settled" || got.Window.Methods.AuthSignIn.Settled.PasswordRequired != 2 {
		t.Fatalf("signIn result = %+v", got.Window.Methods.AuthSignIn)
	}
	if got.Window.Methods.AuthCheckPassword.State != "settled" || got.Window.Methods.AuthCheckPassword.Settled.Success != 2 {
		t.Fatalf("checkPassword result = %+v", got.Window.Methods.AuthCheckPassword)
	}
	if got.Window.Methods.Other.State != "settled" || got.Window.Methods.Other.Settled.Success != 2 || got.Window.Methods.Other.Settled.InvalidRequest != 1 {
		t.Fatalf("other method counts were collapsed: %+v", got.Window.Methods.Other)
	}
	if got.Window.Connections.WSAccepted.Source != fixtureAuthProgressFrontSource || got.Window.Connections.WSAccepted.Before != 20 || got.Window.Connections.WSAccepted.After != 23 || got.Window.Connections.WSAccepted.Delta != 3 {
		t.Fatalf("front websocket delta = %+v", got.Window.Connections.WSAccepted)
	}
	if got.Window.Connections.AuthKeyCreated != 2 {
		t.Fatalf("auth key delta = %d, want 2", got.Window.Connections.AuthKeyCreated)
	}
	if got.Window.Methods.AuthSendCode.Settled.Unauthorized != 0 || got.Window.Methods.AuthSendCode.Settled.TransportFailure != 0 {
		t.Fatal("zero result classes were omitted")
	}
}

func TestRealServerFixtureAuthProgressRequiresASetOncePreBaselineBinding(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	report := newFixtureAuthProgressTestReport(identity)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if got := consumeFixtureAuthProgressBound(data, nil, false, false); got.Available || got.UnavailableReason != "unavailable_incomplete" {
		t.Fatalf("missing consumer binding = %+v, want incomplete", got)
	}

	fixture := &realFixtureProcess{}
	if err := fixture.bindAuthProgressIdentity(identity); err != nil {
		t.Fatalf("initial binding: %v", err)
	}
	fixture.authProgressBaselineCaptured = true
	rebound := identity
	rebound.SourceRefs.ArtifactDigest = "sha256:" + strings.Repeat("9", 64)
	if err := fixture.bindAuthProgressIdentity(rebound); err == nil {
		t.Fatal("consumer accepted an identity rebind after baseline")
	}
	if fixture.authProgressIdentity == nil || *fixture.authProgressIdentity != identity {
		t.Fatalf("rebind changed expected identity: %+v", fixture.authProgressIdentity)
	}
}

func TestRealServerFixtureAuthProgressRejectsHistoricalDigestBinding(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	identity.SourceRefs.ArtifactDigest = fixtureAuthProgressHistoricalDigest
	report := newFixtureAuthProgressTestReport(identity)
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	got := consumeFixtureAuthProgress(data, identity, false, false)
	if got.Available || got.UnavailableReason != "unavailable_malformed_output" {
		t.Fatalf("historical digest binding = %+v, want malformed output", got)
	}
}

func TestRealServerFixtureAuthProgressMapsRejectedReferenceBindingToMalformed(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	report := newFixtureAuthProgressTestReport(identity)
	report.BindingRejected = true
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	got := consumeFixtureAuthProgress(data, identity, false, false)
	if got.Available || got.UnavailableReason != "unavailable_malformed_output" {
		t.Fatalf("rejected reference binding = %+v, want malformed output", got)
	}
}

func TestRealServerFixtureAuthProgressRejectsChangedArtifactBytesWithinOrBetweenLegs(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	ready := realFixtureArtifactReady{
		ArtifactDigest:  identity.SourceRefs.ArtifactDigest,
		LegDigestBefore: identity.SourceRefs.ArtifactDigest,
	}
	ended := realFixtureArtifactEnded{
		Event:           "artifact-leg-ended",
		Status:          "verified",
		Identity:        identity,
		LegDigestBefore: identity.SourceRefs.ArtifactDigest,
		LegDigestAfter:  "sha256:" + strings.Repeat("9", 64),
	}
	if err := validateFixtureArtifactLegEnded(ready, ended, identity); err == nil {
		t.Fatal("within-leg artifact mutation was accepted")
	}

	otherLeg := ended
	otherLeg.LegDigestAfter = identity.SourceRefs.ArtifactDigest
	otherLeg.Identity.SourceRefs.ArtifactDigest = "sha256:" + strings.Repeat("8", 64)
	if err := validateFixtureArtifactLegEnded(ready, otherLeg, identity); err == nil {
		t.Fatal("other-run artifact identity was accepted for the paired leg")
	}
}

func TestRealServerFixtureAuthProgressPairedCleanupRequiresBothLegsAndKeyRemoval(t *testing.T) {
	artifact := &realFixtureArtifactReady{ArtifactDigest: fixtureAuthProgressTestIdentity().SourceRefs.ArtifactDigest}
	tagged := &realFixtureProcess{
		stopped: true, teardownVerified: true, artifactIntegrityVerified: true,
		artifactReady: artifact, ready: realFixtureReady{Fingerprint: "fixture-key"},
	}
	untagged := &realFixtureProcess{
		stopped: true, teardownVerified: true, artifactIntegrityVerified: true,
		artifactReady: artifact, ready: realFixtureReady{Fingerprint: "fixture-key"},
	}
	if fixtureAuthProgressPairCleanupFailed(tagged, untagged, true) {
		t.Fatal("fully cleaned pair was rejected")
	}
	untagged.teardownVerified = false
	if !fixtureAuthProgressPairCleanupFailed(tagged, untagged, true) {
		t.Fatal("partial leg cleanup was accepted")
	}
	untagged.teardownVerified = true
	if !fixtureAuthProgressPairCleanupFailed(tagged, untagged, false) {
		t.Fatal("pair with retained key material was accepted")
	}

	identity := fixtureAuthProgressTestIdentity()
	data, err := json.Marshal(newFixtureAuthProgressTestReport(identity))
	if err != nil {
		t.Fatal(err)
	}
	got := consumeFixtureAuthProgressWithArtifactState(data, &identity, false, false, true)
	if got.Available || got.UnavailableReason != "unavailable_teardown_failure" {
		t.Fatalf("partial paired cleanup result = %+v, want teardown failure", got)
	}
}

func TestRealServerFixtureTaggedParityAndPositiveControls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	serverRevision := currentServerRevision(t)
	webRevision := fixtureAuthProgressArtifactSource
	//nolint:usetesting // The shared private key must live in /dev/shm tmpfs.
	pairKeyDir, err := os.MkdirTemp("/dev/shm", "teagram-fixture-pair-")
	if err != nil {
		t.Fatalf("create tmpfs pair-key directory: %v", err)
	}
	pairKeyPath := filepath.Join(pairKeyDir, "server-key.pem")
	t.Cleanup(func() {
		if err := os.RemoveAll(pairKeyDir); err != nil {
			t.Errorf("remove tmpfs pair-key directory: %v", err)
		}
	})

	taggedRunID := newRealFixtureRunID(t)
	taggedReportPath := filepath.Join(t.TempDir(), "auth-progress.json")
	tagged, err := startRealServerFixtureWithOptions(ctx, taggedRunID, serverRevision, webRevision, nil, realFixtureStartOptions{
		authProgress:       true,
		authProgressOutput: taggedReportPath,
		pairKeyOutput:      pairKeyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !tagged.stopped {
			if err := tagged.stopBySignal(); err != nil {
				t.Errorf("stop tagged fixture during cleanup: %v", err)
			}
		}
	})
	validateRealFixtureReady(t, tagged, taggedRunID, serverRevision, webRevision)
	tagged.authProgressPairPending = true
	if tagged.ready.ServerRevision == "47daaaea5c71b859d9865c03cabb50da5a1a013b" || tagged.ready.HarnessRevision == "5f294c39abb32fc8ae15a4f7ccb085974098b320" {
		t.Fatal("diagnostic server or harness revision reused a historical experiment reference")
	}
	artifactTagged := buildFixtureWebArtifact(ctx, t, webRevision, tagged.ready)
	artifactReadyTagged, err := tagged.attachArtifact(artifactTagged)
	if err != nil {
		t.Fatalf("tagged fixture rejected the pinned artifact: %v; stderr=%s", err, fixtureOutputTail([]byte(tagged.stderr.String())))
	}
	validateRealFixtureArtifactReady(t, artifactReadyTagged, tagged.ready, webRevision, nil)
	if artifactReadyTagged.ArtifactDigest == fixtureAuthProgressHistoricalDigest || tagged.authProgressIdentity == nil ||
		tagged.authProgressIdentity.SourceRefs.ArtifactDigest != artifactReadyTagged.ArtifactDigest {
		t.Fatalf("tagged per-experiment artifact digest was not bound: ready=%q identity=%+v", artifactReadyTagged.ArtifactDigest, tagged.authProgressIdentity)
	}
	controlsTagged, err := tagged.readAuthControls()
	if err != nil {
		t.Fatal(err)
	}
	assertFixtureAuthControlIdentity(t, controlsTagged, tagged.ready, artifactReadyTagged)
	if err := tagged.stopByEOF(); err != nil {
		t.Fatalf("stop tagged fixture: %v", err)
	}
	if tagged.authProgressResult != nil {
		t.Fatalf("tagged observation became valid before paired cleanup: %+v", tagged.authProgressResult)
	}
	assertFixtureCleanupVerified(t, tagged.stderr.String())
	assertProtectedCredentialFilesAbsent(t, tagged.ready)
	assertNamedFixtureResourcesAbsent(t, taggedRunID)

	untaggedRunID := newRealFixtureRunID(t)
	untagged, err := startRealServerFixtureWithOptions(ctx, untaggedRunID, serverRevision, webRevision, nil, realFixtureStartOptions{
		authControls: true,
		pairKeyInput: pairKeyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !untagged.stopped {
			if err := untagged.stopBySignal(); err != nil {
				t.Errorf("stop untagged fixture during cleanup: %v", err)
			}
		}
	})
	validateRealFixtureReady(t, untagged, untaggedRunID, serverRevision, webRevision)
	if untagged.ready.Fingerprint != tagged.ready.Fingerprint || untagged.ready.PublicKeySHA256 != tagged.ready.PublicKeySHA256 ||
		untagged.ready.PublicKeyPEM != tagged.ready.PublicKeyPEM {
		t.Fatal("paired legs do not share this experiment's target key")
	}
	if len(untagged.ready.Credentials) != 2 || len(tagged.ready.Credentials) != 2 ||
		untagged.ready.Credentials[0].Username == tagged.ready.Credentials[0].Username ||
		untagged.ready.Credentials[1].Username == tagged.ready.Credentials[1].Username ||
		untagged.ready.Credentials[0].PasswordFile == tagged.ready.Credentials[0].PasswordFile ||
		untagged.ready.Credentials[1].PasswordFile == tagged.ready.Credentials[1].PasswordFile {
		t.Fatal("paired legs reused synthetic account credentials")
	}
	artifactReadyUntagged, err := untagged.attachArtifact(artifactTagged)
	if err != nil {
		t.Fatalf("untagged fixture rejected the pinned artifact: %v; stderr=%s", err, fixtureOutputTail([]byte(untagged.stderr.String())))
	}
	validateRealFixtureArtifactReady(t, artifactReadyUntagged, untagged.ready, webRevision, nil)
	if artifactReadyUntagged.ArtifactDigest != artifactReadyTagged.ArtifactDigest {
		t.Fatalf("artifact digest changed between server modes: tagged=%q untagged=%q", artifactReadyTagged.ArtifactDigest, artifactReadyUntagged.ArtifactDigest)
	}
	controlsUntagged, err := untagged.readAuthControls()
	if err != nil {
		t.Fatal(err)
	}
	assertFixtureAuthControlIdentity(t, controlsUntagged, untagged.ready, artifactReadyUntagged)
	if !reflect.DeepEqual(controlsTagged.Results, controlsUntagged.Results) {
		t.Fatalf("tagged and untagged protocol control outcomes differ: tagged=%+v untagged=%+v", controlsTagged.Results, controlsUntagged.Results)
	}
	if err := untagged.stopByEOF(); err != nil {
		t.Fatalf("stop untagged fixture: %v", err)
	}
	assertFixtureCleanupVerified(t, untagged.stderr.String())
	assertProtectedCredentialFilesAbsent(t, untagged.ready)
	assertNamedFixtureResourcesAbsent(t, untaggedRunID)
	keyRemoveErr := os.Remove(pairKeyPath)
	directoryRemoveErr := os.Remove(pairKeyDir)
	_, keyStatErr := os.Lstat(pairKeyPath)
	_, directoryStatErr := os.Lstat(pairKeyDir)
	sharedKeyRemoved := errors.Is(keyStatErr, os.ErrNotExist) && errors.Is(directoryStatErr, os.ErrNotExist)
	tagged.finishAuthProgressPair(untagged, sharedKeyRemoved)
	if err := errors.Join(keyRemoveErr, directoryRemoveErr); err != nil {
		t.Fatalf("remove shared target key from tmpfs: %v", err)
	}
	if tagged.authProgressResult == nil || !tagged.authProgressResult.Available || tagged.authProgressResult.Window == nil {
		t.Fatalf("tagged auth-progress observation = %+v", tagged.authProgressResult)
	}
	assertFixtureAuthProgressControls(t, tagged.authProgressResult)
	assertFixtureAuthProgressDropsAreZero(t, tagged.authProgressResult)
	if tagged.authProgressResult.Identity.SourceRefs.RunnerRevision != fixtureAuthProgressRunnerRevision ||
		tagged.authProgressResult.Identity.SourceRefs.ArtifactSourceRevision != webRevision ||
		tagged.authProgressResult.Identity.SourceRefs.ArtifactDigest != artifactReadyTagged.ArtifactDigest ||
		tagged.authProgressResult.Identity.SourceRefs.HarnessRevision != tagged.ready.HarnessRevision ||
		tagged.authProgressResult.Identity.SourceRefs.ServerRevision != serverRevision {
		t.Fatalf("auth-progress references do not bind the run and immutable inputs: %+v", tagged.authProgressResult.Identity)
	}
}

func TestRealServerFixtureAuthProgressRejectsMalformedReports(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	base, err := json.Marshal(newFixtureAuthProgressTestReport(identity))
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity := identity
	otherIdentity.RunID = strings.Repeat("b", 32)
	for _, testCase := range []struct {
		name string
		data []byte
		want string
	}{
		{name: "extra key", data: appendFixtureAuthProgressJSONField(base, `"extra":true`), want: "unavailable_malformed_output"},
		{name: "duplicate key", data: bytes.Replace(base, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1), want: "unavailable_malformed_output"},
		{name: "missing nested field", data: bytes.Replace(base, []byte(`,"source":"front.healthz.websocket_101_count"`), nil, 1), want: "unavailable_malformed_output"},
		{name: "non integer", data: bytes.Replace(base, []byte(`"ws_accepted":20`), []byte(`"ws_accepted":20.5`), 1), want: "unavailable_malformed_output"},
		{name: "null integer", data: bytes.Replace(base, []byte(`"ws_accepted":20`), []byte(`"ws_accepted":null`), 1), want: "unavailable_malformed_output"},
		{name: "out of range", data: bytes.Replace(base, []byte(`"ws_accepted":20`), []byte(`"ws_accepted":2147483648`), 1), want: "unavailable_malformed_output"},
		{name: "non enum boundary", data: bytes.Replace(base, []byte(`"boundary":"artifact_ready"`), []byte(`"boundary":"unexpected"`), 1), want: "unavailable_malformed_output"},
		{name: "trailing data", data: append(append([]byte(nil), base...), []byte(` {}`)...), want: "unavailable_malformed_output"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := consumeFixtureAuthProgress(testCase.data, identity, false, false)
			if got.Available || got.UnavailableReason != testCase.want || got.Window != nil {
				t.Fatalf("malformed report = %+v, want %s with no deltas", got, testCase.want)
			}
		})
	}
	got := consumeFixtureAuthProgress(base, otherIdentity, false, false)
	if got.Available || got.UnavailableReason != "unavailable_malformed_output" {
		t.Fatalf("mismatched identity = %+v", got)
	}
	got = consumeFixtureAuthProgress(nil, identity, false, false)
	if got.Available || got.UnavailableReason != "unavailable_missing_output" {
		t.Fatalf("missing report = %+v", got)
	}
}

func TestRealServerFixtureAuthProgressControlOutcomesAreClosed(t *testing.T) {
	record := fixtureAuthControlRecord{
		Event:    "auth-controls",
		Status:   "passed",
		Identity: fixtureAuthProgressTestIdentity(),
		Results: fixtureAuthControlResults{
			AuthSendCode:       fixtureAuthControlOutcome{Result: "success", Accounts: 2},
			AuthSignIn:         fixtureAuthControlOutcome{Result: "password_required", Accounts: 2},
			AccountGetPassword: fixtureAuthControlOutcome{Result: "success", Accounts: 2},
			AuthCheckPassword:  fixtureAuthControlOutcome{Result: "success", Accounts: 2},
			AuthSignUp:         fixtureAuthControlOutcome{Result: "invalid_request", Accounts: 1},
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFixtureAuthControls(data)
	if err != nil || !reflect.DeepEqual(got, record) {
		t.Fatalf("valid auth control results = %+v, err=%v", got, err)
	}
	bad := bytes.Replace(data, []byte(`"result":"password_required"`), []byte(`"result":"success"`), 1)
	if _, err := decodeFixtureAuthControls(bad); err == nil {
		t.Fatal("auth.signIn control accepted success instead of password_required")
	}
	extra := appendFixtureAuthProgressJSONField(data, `"raw_error":"sentinel"`)
	if _, err := decodeFixtureAuthControls(extra); err == nil {
		t.Fatal("auth control output accepted an unexpected field")
	}
}

func TestRealServerFixtureAuthProgressPreservesPendingAtRunnerExit(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	report := newFixtureAuthProgressTestReport(identity)
	setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSignIn, 1, mtproto.RPCProgressResultCounts{}, 1)
	report.Boundaries[1].Server.RPCPending = 1
	setFixtureAuthProgressMethod(&report.ShutdownFinal.Methods.AuthSignIn, 1, mtproto.RPCProgressResultCounts{PasswordRequired: 1}, 0)
	report.ShutdownFinal.RPCPending = 0
	report.ShutdownFinal.ShutdownComplete = true
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	got := consumeFixtureAuthProgress(data, identity, false, false)
	if !got.Available || got.Window == nil {
		t.Fatalf("drained forced-pending observation = %+v", got)
	}
	method := got.Window.Methods.AuthSignIn
	if method.State != "pending" || method.Started != 1 || method.PendingAfter != 1 || method.Settled.PasswordRequired != 0 {
		t.Fatalf("runner-exit pending observation was replaced by shutdown settlement: %+v", method)
	}
	if got.Window.ShutdownFinal.Methods.AuthSignIn.Settled.PasswordRequired != 1 || got.Window.ShutdownFinal.Methods.AuthSignIn.Pending != 0 {
		t.Fatalf("shutdown-final snapshot did not preserve the later completion: %+v", got.Window.ShutdownFinal.Methods.AuthSignIn)
	}
}

func TestRealServerFixtureAuthProgressUnavailableReasonsAreClosedAndDeterministic(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	for _, testCase := range []struct {
		name     string
		mutate   func(*fixtureAuthProgressReport)
		cancel   bool
		teardown bool
		want     string
	}{
		{name: "missing runner boundary", mutate: func(report *fixtureAuthProgressReport) { report.Boundaries = report.Boundaries[:1] }, want: "unavailable_incomplete"},
		{name: "setup request pending at baseline", mutate: func(report *fixtureAuthProgressReport) {
			setFixtureAuthProgressMethod(&report.Boundaries[0].Server.Methods.Other, 1, mtproto.RPCProgressResultCounts{}, 1)
			report.Boundaries[0].Server.RPCPending = 1
			setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.Other, 1, mtproto.RPCProgressResultCounts{Success: 1}, 0)
			report.ShutdownFinal = report.Boundaries[1].Server
			report.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
			report.ShutdownFinal.ShutdownComplete = true
		}, want: "unavailable_incomplete"},
		{name: "counter overflow", mutate: func(report *fixtureAuthProgressReport) { report.Boundaries[1].Server.Overflow = true }, want: "unavailable_counter_overflow"},
		{name: "counter reset", mutate: func(report *fixtureAuthProgressReport) { report.Boundaries[1].Server.Connections.AuthKeyCreated = -1 }, want: "unavailable_malformed_output"},
		{name: "counter decreases", mutate: func(report *fixtureAuthProgressReport) {
			report.Boundaries[0].Server.Connections.AuthKeyCreated = 2
			report.Boundaries[1].Server.Connections.AuthKeyCreated = 1
		}, want: "unavailable_counter_reset"},
		{name: "exporter outstanding drains", mutate: func(report *fixtureAuthProgressReport) {
			report.Boundaries[0].Server.Exporter = mtproto.RPCProgressExporter{Enqueued: 1, ExportOutstanding: 1}
			report.Boundaries[1].Server.Exporter = mtproto.RPCProgressExporter{Enqueued: 1, Exported: 1}
			report.ShutdownFinal.Exporter = report.Boundaries[1].Server.Exporter
		}, want: ""},
		{name: "final export remains queued", mutate: func(report *fixtureAuthProgressReport) {
			report.ShutdownFinal.Exporter = mtproto.RPCProgressExporter{Enqueued: 1, ExportOutstanding: 1}
		}, want: "unavailable_incomplete"},
		{name: "drop at first boundary", mutate: func(report *fixtureAuthProgressReport) {
			report.Boundaries[0].Server.Exporter.Dropped = 1
			report.Boundaries[1].Server.Exporter.Dropped = 1
			report.ShutdownFinal.Exporter.Dropped = 1
		}, want: "unavailable_drop"},
		{name: "exporter error at final drain", mutate: func(report *fixtureAuthProgressReport) {
			report.ShutdownFinal.Exporter = mtproto.RPCProgressExporter{Enqueued: 1, ExporterErrors: 1}
		}, want: "unavailable_exporter_error"},
		{name: "exporter panic", mutate: func(report *fixtureAuthProgressReport) {
			report.ShutdownFinal.Exporter = mtproto.RPCProgressExporter{Enqueued: 1, ExporterPanics: 1}
		}, want: "unavailable_exporter_panic"},
		{name: "combined exporter failures", mutate: func(report *fixtureAuthProgressReport) {
			failure := mtproto.RPCProgressExporter{Enqueued: 2, Dropped: 1, ExporterErrors: 1, ExporterPanics: 1}
			report.Boundaries[0].Server.Exporter = failure
			report.Boundaries[1].Server.Exporter = failure
			report.ShutdownFinal.Exporter = failure
		}, want: "unavailable_exporter_panic"},
		{name: "report incomplete", mutate: func(report *fixtureAuthProgressReport) { report.Incomplete = true }, want: "unavailable_incomplete"},
		{name: "cancelled", cancel: true, want: "unavailable_cancelled"},
		{name: "teardown failure", teardown: true, want: "unavailable_teardown_failure"},
		{name: "cancellation wins combined failures", cancel: true, teardown: true, mutate: func(report *fixtureAuthProgressReport) {
			report.Boundaries[0].Server.Overflow = true
		}, want: "unavailable_cancelled"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			report := newFixtureAuthProgressTestReport(identity)
			if testCase.mutate != nil {
				testCase.mutate(&report)
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			got := consumeFixtureAuthProgress(data, identity, testCase.cancel, testCase.teardown)
			if testCase.want == "" {
				if !got.Available || got.Window == nil || got.UnavailableReason != "" {
					t.Fatalf("valid outstanding-export drain = %+v", got)
				}
				if got.Window.Exporter.ExportOutstandingBefore != 1 || got.Window.Exporter.ExportOutstandingAfter != 0 || got.Window.Exporter.ExportOutstandingFinal != 0 {
					t.Fatalf("outstanding export counts were not preserved: %+v", got.Window.Exporter)
				}
				return
			}
			if got.Available || got.UnavailableReason != testCase.want || got.Window != nil {
				t.Fatalf("invalid observation = %+v, want %s", got, testCase.want)
			}
		})
	}
}

func TestRealServerFixtureAuthProgressRejectsImpossibleArithmetic(t *testing.T) {
	identity := fixtureAuthProgressTestIdentity()
	tests := []struct {
		name   string
		mutate func(*fixtureAuthProgressReport)
	}{
		{name: "settled exceeds started", mutate: func(report *fixtureAuthProgressReport) {
			setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSignIn, 1, mtproto.RPCProgressResultCounts{PasswordRequired: 1}, 1)
		}},
		{name: "negative pending", mutate: func(report *fixtureAuthProgressReport) {
			setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSignIn, 1, mtproto.RPCProgressResultCounts{}, -1)
		}},
		{name: "settled total mismatch", mutate: func(report *fixtureAuthProgressReport) {
			setFixtureAuthProgressMethod(&report.Boundaries[1].Server.Methods.AuthSignIn, 2, mtproto.RPCProgressResultCounts{PasswordRequired: 1}, 0)
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			report := newFixtureAuthProgressTestReport(identity)
			testCase.mutate(&report)
			if testCase.name == "settled exceeds started" || testCase.name == "negative pending" || testCase.name == "settled total mismatch" {
				report.ShutdownFinal = report.Boundaries[1].Server
				report.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
				report.ShutdownFinal.ShutdownComplete = true
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			got := consumeFixtureAuthProgress(data, identity, false, false)
			if got.Available || got.UnavailableReason != "unavailable_malformed_output" {
				t.Fatalf("impossible arithmetic = %+v", got)
			}
		})
	}
}

func fixtureAuthProgressTestIdentity() mtproto.RPCProgressIdentity {
	return mtproto.RPCProgressIdentity{
		RunID: strings.Repeat("a", 32),
		SourceRefs: mtproto.RPCProgressSourceRefs{
			RunnerRevision:         strings.Repeat("b", 40),
			ArtifactSourceRevision: strings.Repeat("c", 40),
			ArtifactDigest:         "sha256:" + strings.Repeat("d", 64),
			HarnessRevision:        strings.Repeat("e", 40),
			ServerRevision:         strings.Repeat("f", 40),
		},
	}
}

func fixtureAuthProgressIdentity(ready realFixtureReady, digest string) mtproto.RPCProgressIdentity {
	return mtproto.RPCProgressIdentity{
		RunID: ready.RunID,
		SourceRefs: mtproto.RPCProgressSourceRefs{
			RunnerRevision:         fixtureAuthProgressRunnerRevision,
			ArtifactSourceRevision: ready.WebRevision,
			ArtifactDigest:         digest,
			HarnessRevision:        ready.HarnessRevision,
			ServerRevision:         ready.ServerRevision,
		},
	}
}

func newFixtureAuthProgressTestReport(identity mtproto.RPCProgressIdentity) fixtureAuthProgressReport {
	return fixtureAuthProgressReport{
		SchemaVersion: fixtureAuthProgressContractVersion,
		Identity:      identity,
		Boundaries: []fixtureAuthProgressBoundary{
			{
				Server: newFixtureAuthProgressTestSnapshot(identity, mtproto.RPCProgressBoundaryArtifactReady),
				Front:  fixtureAuthProgressFront{WSAccepted: 20, Source: fixtureAuthProgressFrontSource},
			},
			{
				Server: newFixtureAuthProgressTestSnapshot(identity, mtproto.RPCProgressBoundaryRunnerExit),
				Front:  fixtureAuthProgressFront{WSAccepted: 20, Source: fixtureAuthProgressFrontSource},
			},
		},
		ShutdownFinal: newFixtureAuthProgressTestSnapshot(identity, mtproto.RPCProgressBoundaryShutdownFinal),
	}
}

func newFixtureAuthProgressTestSnapshot(identity mtproto.RPCProgressIdentity, boundary mtproto.RPCProgressBoundary) mtproto.RPCProgressSnapshot {
	return mtproto.RPCProgressSnapshot{
		SchemaVersion:    fixtureAuthProgressContractVersion,
		Identity:         identity,
		Boundary:         boundary,
		ShutdownComplete: boundary == mtproto.RPCProgressBoundaryShutdownFinal,
	}
}

func setFixtureAuthProgressMethod(method *mtproto.RPCProgressMethodSnapshot, started int64, settled mtproto.RPCProgressResultCounts, pending int64) {
	method.Started = started
	method.Settled = settled
	method.Pending = pending
}

func appendFixtureAuthProgressJSONField(data []byte, field string) []byte {
	result := append([]byte(nil), data[:len(data)-1]...)
	result = append(result, ',')
	result = append(result, field...)
	return append(result, '}')
}

func assertFixtureAuthControlIdentity(t *testing.T, controls *fixtureAuthControlRecord, ready realFixtureReady, artifact *realFixtureArtifactReady) {
	t.Helper()
	if controls == nil || controls.Identity.RunID != ready.RunID ||
		controls.Identity.SourceRefs.RunnerRevision != fixtureAuthProgressRunnerRevision ||
		controls.Identity.SourceRefs.ArtifactSourceRevision != artifact.WebRevision ||
		controls.Identity.SourceRefs.ArtifactDigest != artifact.ArtifactDigest ||
		controls.Identity.SourceRefs.HarnessRevision != ready.HarnessRevision ||
		controls.Identity.SourceRefs.ServerRevision != ready.ServerRevision {
		t.Fatalf("auth control references do not bind this fixture and artifact: %+v", controls)
	}
}

func assertFixtureAuthProgressControls(t *testing.T, result *fixtureAuthProgressResult) {
	t.Helper()
	if result == nil || !result.Available || result.Window == nil {
		t.Fatalf("auth-progress window is unavailable: %+v", result)
	}
	window := result.Window
	if window.Methods.AuthSendCode.State != "settled" || window.Methods.AuthSendCode.Started != 2 ||
		window.Methods.AuthSendCode.Settled != (mtproto.RPCProgressResultCounts{Success: 2}) {
		t.Fatalf("sendCode controls = %+v", window.Methods.AuthSendCode)
	}
	if window.Methods.AuthSignIn.State != "settled" || window.Methods.AuthSignIn.Started != 2 ||
		window.Methods.AuthSignIn.Settled != (mtproto.RPCProgressResultCounts{PasswordRequired: 2}) {
		t.Fatalf("signIn controls = %+v", window.Methods.AuthSignIn)
	}
	if window.Methods.AuthCheckPassword.State != "settled" || window.Methods.AuthCheckPassword.Started != 2 ||
		window.Methods.AuthCheckPassword.Settled != (mtproto.RPCProgressResultCounts{Success: 2}) {
		t.Fatalf("checkPassword controls = %+v", window.Methods.AuthCheckPassword)
	}
	if window.Methods.Other.State != "settled" || window.Methods.Other.Started < 3 ||
		window.Methods.Other.Settled.Success < 2 || window.Methods.Other.Settled.InvalidRequest < 1 {
		t.Fatalf("account.getPassword or closed-signup control missing from other: %+v", window.Methods.Other)
	}
	if window.RPCPendingBefore != 0 || window.RPCPendingAfter != 0 {
		t.Fatalf("auth controls left RPCs pending: %d/%d", window.RPCPendingBefore, window.RPCPendingAfter)
	}
}

func assertFixtureAuthProgressDropsAreZero(t *testing.T, result *fixtureAuthProgressResult) {
	t.Helper()
	if result == nil || result.Window == nil {
		t.Fatalf("auth-progress report has no valid window: %+v", result)
	}
	for _, snapshot := range []mtproto.RPCProgressSnapshot{
		result.Window.ArtifactReady.Server,
		result.Window.RunnerExit.Server,
		result.Window.ShutdownFinal,
	} {
		if snapshot.Exporter.Dropped != 0 || snapshot.Exporter.ExporterErrors != 0 || snapshot.Exporter.ExporterPanics != 0 {
			t.Fatalf("auth-progress exporter invalidation counters are nonzero: %+v", snapshot.Exporter)
		}
	}
}

func consumeFixtureAuthProgress(data []byte, expected mtproto.RPCProgressIdentity, cancelled, teardownFailed bool) fixtureAuthProgressResult {
	return consumeFixtureAuthProgressBound(data, &expected, cancelled, teardownFailed)
}

func consumeFixtureAuthProgressBound(data []byte, expected *mtproto.RPCProgressIdentity, cancelled, teardownFailed bool) fixtureAuthProgressResult {
	return consumeFixtureAuthProgressWithArtifactState(data, expected, cancelled, false, teardownFailed)
}

func consumeFixtureAuthProgressWithArtifactState(data []byte, expected *mtproto.RPCProgressIdentity, cancelled, artifactMismatch, teardownFailed bool) fixtureAuthProgressResult {
	result := fixtureAuthProgressResult{SchemaVersion: fixtureAuthProgressContractVersion}
	if expected == nil {
		reasons := map[string]bool{"unavailable_incomplete": true}
		if artifactMismatch {
			reasons["unavailable_malformed_output"] = true
		}
		if cancelled {
			reasons["unavailable_cancelled"] = true
		}
		if teardownFailed {
			reasons["unavailable_teardown_failure"] = true
		}
		result.UnavailableReason = fixtureAuthProgressPreferredReason(reasons)
		return result
	}
	if expected.Validate() != nil {
		result.UnavailableReason = "unavailable_malformed_output"
		return result
	}
	if len(data) == 0 {
		result.UnavailableReason = "unavailable_missing_output"
		return result
	}
	report, err := decodeFixtureAuthProgressReport(data)
	if err != nil || report.SchemaVersion != fixtureAuthProgressContractVersion {
		result.UnavailableReason = "unavailable_malformed_output"
		return result
	}
	result.Identity = *expected
	if report.Identity.Validate() != nil && report.Incomplete {
		reasons := map[string]bool{"unavailable_incomplete": true}
		if artifactMismatch || report.BindingRejected {
			reasons["unavailable_malformed_output"] = true
		}
		if cancelled {
			reasons["unavailable_cancelled"] = true
		}
		if teardownFailed {
			reasons["unavailable_teardown_failure"] = true
		}
		result.UnavailableReason = fixtureAuthProgressPreferredReason(reasons)
		return result
	}
	if report.Identity.Validate() != nil || report.Identity != *expected || expected.SourceRefs.ArtifactDigest == fixtureAuthProgressHistoricalDigest {
		result.UnavailableReason = "unavailable_malformed_output"
		return result
	}

	reasons := make(map[string]bool)
	addReason := func(reason string) { reasons[reason] = true }
	if cancelled {
		addReason("unavailable_cancelled")
	}
	if teardownFailed {
		addReason("unavailable_teardown_failure")
	}
	if artifactMismatch {
		addReason("unavailable_malformed_output")
	}
	if report.BindingRejected {
		addReason("unavailable_malformed_output")
	}
	if report.Incomplete {
		addReason("unavailable_incomplete")
	}
	if len(report.Boundaries) != 2 {
		addReason("unavailable_incomplete")
	}

	boundaries := make([]fixtureAuthProgressBoundary, 2)
	copy(boundaries, report.Boundaries)
	expectedBoundaries := []mtproto.RPCProgressBoundary{
		mtproto.RPCProgressBoundaryArtifactReady,
		mtproto.RPCProgressBoundaryRunnerExit,
	}
	for index := range boundaries {
		if index >= len(report.Boundaries) {
			continue
		}
		boundary := report.Boundaries[index]
		if boundary.Server.Boundary == "" || boundary.Server.SchemaVersion == 0 {
			addReason("unavailable_incomplete")
		} else if boundary.Server.Boundary != expectedBoundaries[index] {
			addReason("unavailable_malformed_output")
		}
		if err := validateFixtureAuthProgressSnapshot(boundary.Server, *expected, expectedBoundaries[index]); err != nil {
			if errors.Is(err, errFixtureAuthProgressIncomplete) {
				addReason("unavailable_incomplete")
			} else {
				addReason("unavailable_malformed_output")
			}
		}
		collectFixtureAuthProgressSnapshotReasons(boundary.Server, false, addReason)
		if boundary.Front.Source != fixtureAuthProgressFrontSource || !fixtureAuthProgressCountValid(boundary.Front.WSAccepted) {
			addReason("unavailable_malformed_output")
		}
		boundaries[index] = boundary
	}

	final := report.ShutdownFinal
	if final.Boundary == "" || final.SchemaVersion == 0 {
		addReason("unavailable_incomplete")
	} else if final.Boundary != mtproto.RPCProgressBoundaryShutdownFinal {
		addReason("unavailable_malformed_output")
	}
	if err := validateFixtureAuthProgressSnapshot(final, *expected, mtproto.RPCProgressBoundaryShutdownFinal); err != nil {
		if errors.Is(err, errFixtureAuthProgressIncomplete) {
			addReason("unavailable_incomplete")
		} else {
			addReason("unavailable_malformed_output")
		}
	}
	collectFixtureAuthProgressSnapshotReasons(final, true, addReason)

	if len(report.Boundaries) == 2 {
		if boundaries[0].Server.RPCPending != 0 || fixtureAuthProgressPending(boundaries[0].Server.Methods) != 0 {
			addReason("unavailable_incomplete")
		}
		if boundaries[1].Front.WSAccepted < boundaries[0].Front.WSAccepted {
			addReason("unavailable_counter_reset")
		}
		if fixtureAuthProgressHasCounterReset(boundaries[0].Server, boundaries[1].Server) ||
			fixtureAuthProgressHasCounterReset(boundaries[1].Server, final) ||
			fixtureAuthProgressHasCounterReset(boundaries[0].Server, final) {
			addReason("unavailable_counter_reset")
		}
	}
	if final.RPCPending != 0 || fixtureAuthProgressPending(final.Methods) != 0 || final.Exporter.ExportOutstanding != 0 || !final.ShutdownComplete {
		addReason("unavailable_incomplete")
	}
	if reason := fixtureAuthProgressPreferredReason(reasons); reason != "" {
		result.UnavailableReason = reason
		return result
	}

	window, ok := fixtureAuthProgressBuildWindow(boundaries[0], boundaries[1], final)
	if !ok {
		result.UnavailableReason = "unavailable_malformed_output"
		return result
	}
	result.Available = true
	result.Window = &window
	return result
}

var errFixtureAuthProgressIncomplete = errors.New("fixture auth-progress observation is incomplete")

func decodeFixtureAuthProgressReport(data []byte) (fixtureAuthProgressReport, error) {
	var report fixtureAuthProgressReport
	if err := rejectFixtureAuthProgressDuplicateKeys(data); err != nil {
		return report, err
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeOf(report)); err != nil {
		return report, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return report, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return report, errors.New("fixture auth-progress report has trailing data")
	}
	return report, nil
}

func decodeFixtureAuthControls(data []byte) (fixtureAuthControlRecord, error) {
	var record fixtureAuthControlRecord
	if err := rejectFixtureAuthProgressDuplicateKeys(data); err != nil {
		return record, err
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeOf(record)); err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return record, errors.New("fixture auth controls have trailing data")
	}
	if record.Event != "auth-controls" || record.Status != "passed" || record.Identity.Validate() != nil {
		return record, errors.New("fixture auth controls metadata is invalid")
	}
	for _, outcome := range []fixtureAuthControlOutcome{
		record.Results.AuthSendCode,
		record.Results.AuthSignIn,
		record.Results.AccountGetPassword,
		record.Results.AuthCheckPassword,
		record.Results.AuthSignUp,
	} {
		if outcome.Accounts < 1 || outcome.Accounts > 2 {
			return record, errors.New("fixture auth controls account count is invalid")
		}
	}
	if record.Results.AuthSendCode.Result != "success" || record.Results.AuthSendCode.Accounts != 2 ||
		record.Results.AuthSignIn.Result != "password_required" || record.Results.AuthSignIn.Accounts != 2 ||
		record.Results.AccountGetPassword.Result != "success" || record.Results.AccountGetPassword.Accounts != 2 ||
		record.Results.AuthCheckPassword.Result != "success" || record.Results.AuthCheckPassword.Accounts != 2 ||
		record.Results.AuthSignUp.Result != "invalid_request" || record.Results.AuthSignUp.Accounts != 1 {
		return record, errors.New("fixture auth controls outcomes are invalid")
	}
	return record, nil
}

func decodeFixtureAuthProgressBinding(data []byte) (fixtureAuthProgressBindingRecord, error) {
	var record fixtureAuthProgressBindingRecord
	if err := rejectFixtureAuthProgressDuplicateKeys(data); err != nil {
		return record, err
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeOf(record)); err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return record, errors.New("fixture artifact binding has trailing data")
	}
	return record, nil
}

func decodeFixtureArtifactLegEnded(data []byte) (realFixtureArtifactEnded, error) {
	var record realFixtureArtifactEnded
	if err := rejectFixtureAuthProgressDuplicateKeys(data); err != nil {
		return record, err
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeOf(record)); err != nil {
		return record, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return record, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return record, errors.New("fixture artifact end record has trailing data")
	}
	return record, nil
}

func validateFixtureArtifactLegEnded(ready realFixtureArtifactReady, ended realFixtureArtifactEnded, identity mtproto.RPCProgressIdentity) error {
	if ready.ArtifactDigest != identity.SourceRefs.ArtifactDigest || ready.LegDigestBefore != identity.SourceRefs.ArtifactDigest ||
		ended.Event != "artifact-leg-ended" || ended.Status != "verified" || ended.Identity != identity ||
		ended.LegDigestBefore != ready.LegDigestBefore || ended.LegDigestBefore != identity.SourceRefs.ArtifactDigest ||
		ended.LegDigestAfter != identity.SourceRefs.ArtifactDigest {
		return errors.New("fixture leg did not preserve the bound artifact digest")
	}
	return nil
}

func rejectFixtureAuthProgressDuplicateKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanFixtureAuthProgressJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("fixture auth-progress report has trailing data")
	}
	return nil
}

func scanFixtureAuthProgressJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("fixture auth-progress object key is invalid")
			}
			if _, exists := seen[key]; exists {
				return errors.New("fixture auth-progress report has duplicate keys")
			}
			seen[key] = struct{}{}
			if err := scanFixtureAuthProgressJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("fixture auth-progress object is incomplete")
		}
	case '[':
		for decoder.More() {
			if err := scanFixtureAuthProgressJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("fixture auth-progress array is incomplete")
		}
	default:
		return errors.New("fixture auth-progress JSON delimiter is invalid")
	}
	return nil
}

func validateFixtureAuthProgressJSONShape(data []byte, target reflect.Type) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("fixture auth-progress value is null")
	}
	switch target.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(data, &object); err != nil || object == nil {
			return errors.New("fixture auth-progress object is invalid")
		}
		fields := make(map[string]reflect.Type, target.NumField())
		for field := range target.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				return errors.New("fixture auth-progress schema has an untagged field")
			}
			fields[name] = field.Type
		}
		if len(object) != len(fields) {
			return errors.New("fixture auth-progress object has missing or unexpected fields")
		}
		for name, fieldType := range fields {
			value, ok := object[name]
			if !ok {
				return errors.New("fixture auth-progress object has a missing field")
			}
			if err := validateFixtureAuthProgressJSONShape(value, fieldType); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil || values == nil {
			return errors.New("fixture auth-progress array is invalid")
		}
		for _, value := range values {
			if err := validateFixtureAuthProgressJSONShape(value, target.Elem()); err != nil {
				return err
			}
		}
	default:
		value := reflect.New(target).Interface()
		if err := json.Unmarshal(data, value); err != nil {
			return errors.New("fixture auth-progress scalar has the wrong type")
		}
	}
	return nil
}

func validateFixtureAuthProgressSnapshot(snapshot mtproto.RPCProgressSnapshot, identity mtproto.RPCProgressIdentity, boundary mtproto.RPCProgressBoundary) error {
	if snapshot.Boundary == "" || snapshot.SchemaVersion == 0 {
		return errFixtureAuthProgressIncomplete
	}
	if snapshot.SchemaVersion != fixtureAuthProgressContractVersion || snapshot.Identity != identity || snapshot.Identity.Validate() != nil || snapshot.Boundary != boundary || snapshot.Inconsistent {
		return errors.New("fixture auth-progress snapshot metadata is invalid")
	}
	if err := validateFixtureAuthProgressMethod(snapshot.Methods.AuthSendCode); err != nil {
		return err
	}
	if err := validateFixtureAuthProgressMethod(snapshot.Methods.AuthSignIn); err != nil {
		return err
	}
	if err := validateFixtureAuthProgressMethod(snapshot.Methods.AuthCheckPassword); err != nil {
		return err
	}
	if err := validateFixtureAuthProgressMethod(snapshot.Methods.Other); err != nil {
		return err
	}
	pending := fixtureAuthProgressPending(snapshot.Methods)
	if !fixtureAuthProgressCountValid(snapshot.RPCPending) || pending != snapshot.RPCPending {
		if pending > mtproto.RPCProgressMaxCount && snapshot.RPCPending == mtproto.RPCProgressMaxCount && snapshot.Overflow {
			// The tracer saturates only the aggregate pending count and raises Overflow.
		} else {
			return errors.New("fixture auth-progress pending arithmetic is invalid")
		}
	}
	if !fixtureAuthProgressCountValid(snapshot.Connections.AuthKeyCreated) || !fixtureAuthProgressCountValid(snapshot.Connections.TransportNegotiationError) {
		return errors.New("fixture auth-progress connection count is invalid")
	}
	if !fixtureAuthProgressCountValid(snapshot.Exporter.Enqueued) || !fixtureAuthProgressCountValid(snapshot.Exporter.Exported) ||
		!fixtureAuthProgressCountValid(snapshot.Exporter.Dropped) || !fixtureAuthProgressCountValid(snapshot.Exporter.ExporterErrors) ||
		!fixtureAuthProgressCountValid(snapshot.Exporter.ExporterPanics) || !fixtureAuthProgressCountValid(snapshot.Exporter.ExportOutstanding) ||
		snapshot.Exporter.Exported+snapshot.Exporter.ExporterErrors+snapshot.Exporter.ExporterPanics+snapshot.Exporter.ExportOutstanding != snapshot.Exporter.Enqueued {
		return errors.New("fixture auth-progress exporter arithmetic is invalid")
	}
	if boundary != mtproto.RPCProgressBoundaryShutdownFinal && snapshot.ShutdownComplete {
		return errors.New("fixture auth-progress boundary is marked shutdown-complete")
	}
	return nil
}

func validateFixtureAuthProgressMethod(method mtproto.RPCProgressMethodSnapshot) error {
	if !fixtureAuthProgressCountValid(method.Started) || !fixtureAuthProgressCountValid(method.Pending) {
		return errors.New("fixture auth-progress method count is invalid")
	}
	settled := fixtureAuthProgressCounts(method.Settled)
	total := int64(0)
	for _, count := range settled {
		if !fixtureAuthProgressCountValid(count) {
			return errors.New("fixture auth-progress settled count is invalid")
		}
		total += count
	}
	if total+method.Pending != method.Started {
		return errors.New("fixture auth-progress method arithmetic is invalid")
	}
	return nil
}

func collectFixtureAuthProgressSnapshotReasons(snapshot mtproto.RPCProgressSnapshot, final bool, add func(string)) {
	if snapshot.Cancelled {
		add("unavailable_cancelled")
	}
	if snapshot.Overflow {
		add("unavailable_counter_overflow")
	}
	if snapshot.Exporter.ExporterPanics != 0 {
		add("unavailable_exporter_panic")
	}
	if snapshot.Exporter.ExporterErrors != 0 {
		add("unavailable_exporter_error")
	}
	if snapshot.Exporter.Dropped != 0 {
		add("unavailable_drop")
	}
	if final && (!snapshot.ShutdownComplete || snapshot.RPCPending != 0 || snapshot.Exporter.ExportOutstanding != 0) {
		add("unavailable_incomplete")
	}
}

func fixtureAuthProgressPreferredReason(reasons map[string]bool) string {
	// This fixed order makes combined invalidations stable in reports and tests.
	for _, reason := range []string{
		"unavailable_malformed_output",
		"unavailable_cancelled",
		"unavailable_teardown_failure",
		"unavailable_counter_overflow",
		"unavailable_counter_reset",
		"unavailable_exporter_panic",
		"unavailable_exporter_error",
		"unavailable_drop",
		"unavailable_incomplete",
	} {
		if reasons[reason] {
			return reason
		}
	}
	return ""
}

func fixtureAuthProgressBuildWindow(before, after fixtureAuthProgressBoundary, final mtproto.RPCProgressSnapshot) (fixtureAuthProgressWindow, bool) {
	window := fixtureAuthProgressWindow{
		ArtifactReady:    before,
		RunnerExit:       after,
		ShutdownFinal:    final,
		RPCPendingBefore: before.Server.RPCPending,
		RPCPendingAfter:  after.Server.RPCPending,
	}
	var ok bool
	if window.Methods.AuthSendCode, ok = fixtureAuthProgressMethodWindow(before.Server.Methods.AuthSendCode, after.Server.Methods.AuthSendCode); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Methods.AuthSignIn, ok = fixtureAuthProgressMethodWindow(before.Server.Methods.AuthSignIn, after.Server.Methods.AuthSignIn); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Methods.AuthCheckPassword, ok = fixtureAuthProgressMethodWindow(before.Server.Methods.AuthCheckPassword, after.Server.Methods.AuthCheckPassword); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Methods.Other, ok = fixtureAuthProgressMethodWindow(before.Server.Methods.Other, after.Server.Methods.Other); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	var delta int64
	if delta, ok = fixtureAuthProgressMonotonicDelta(before.Server.Connections.AuthKeyCreated, after.Server.Connections.AuthKeyCreated); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	window.Connections.AuthKeyCreated = delta
	if delta, ok = fixtureAuthProgressMonotonicDelta(before.Server.Connections.TransportNegotiationError, after.Server.Connections.TransportNegotiationError); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	window.Connections.TransportNegotiationError = delta
	frontDelta, ok := fixtureAuthProgressMonotonicDelta(before.Front.WSAccepted, after.Front.WSAccepted)
	if !ok {
		return fixtureAuthProgressWindow{}, false
	}
	window.Connections.WSAccepted = fixtureAuthProgressCounterDelta{Source: fixtureAuthProgressFrontSource, Before: before.Front.WSAccepted, After: after.Front.WSAccepted, Delta: frontDelta}
	if window.Exporter.Enqueued, ok = fixtureAuthProgressMonotonicDelta(before.Server.Exporter.Enqueued, after.Server.Exporter.Enqueued); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Exporter.Exported, ok = fixtureAuthProgressMonotonicDelta(before.Server.Exporter.Exported, after.Server.Exporter.Exported); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Exporter.Dropped, ok = fixtureAuthProgressMonotonicDelta(before.Server.Exporter.Dropped, after.Server.Exporter.Dropped); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Exporter.ExporterErrors, ok = fixtureAuthProgressMonotonicDelta(before.Server.Exporter.ExporterErrors, after.Server.Exporter.ExporterErrors); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	if window.Exporter.ExporterPanics, ok = fixtureAuthProgressMonotonicDelta(before.Server.Exporter.ExporterPanics, after.Server.Exporter.ExporterPanics); !ok {
		return fixtureAuthProgressWindow{}, false
	}
	window.Exporter.ExportOutstandingBefore = before.Server.Exporter.ExportOutstanding
	window.Exporter.ExportOutstandingAfter = after.Server.Exporter.ExportOutstanding
	window.Exporter.ExportOutstandingFinal = final.Exporter.ExportOutstanding
	return window, true
}

func fixtureAuthProgressMethodWindow(before, after mtproto.RPCProgressMethodSnapshot) (fixtureAuthProgressMethodDelta, bool) {
	started, ok := fixtureAuthProgressMonotonicDelta(before.Started, after.Started)
	if !ok {
		return fixtureAuthProgressMethodDelta{}, false
	}
	beforeCounts, afterCounts := fixtureAuthProgressCounts(before.Settled), fixtureAuthProgressCounts(after.Settled)
	var settledDelta [9]int64
	settledTotal := int64(0)
	for index := range beforeCounts {
		settledDelta[index], ok = fixtureAuthProgressMonotonicDelta(beforeCounts[index], afterCounts[index])
		if !ok {
			return fixtureAuthProgressMethodDelta{}, false
		}
		settledTotal += settledDelta[index]
	}
	if settledTotal > before.Pending+started || before.Pending+started-settledTotal != after.Pending {
		return fixtureAuthProgressMethodDelta{}, false
	}
	state := "settled"
	if started == 0 {
		state = "not_reached"
	} else if after.Pending != 0 {
		state = "pending"
	}
	return fixtureAuthProgressMethodDelta{
		State:         state,
		Started:       started,
		Settled:       fixtureAuthProgressCountsFromArray(settledDelta),
		PendingBefore: before.Pending,
		PendingAfter:  after.Pending,
	}, true
}

func fixtureAuthProgressHasCounterReset(before, after mtproto.RPCProgressSnapshot) bool {
	if after.Methods.AuthSendCode.Started < before.Methods.AuthSendCode.Started ||
		after.Methods.AuthSignIn.Started < before.Methods.AuthSignIn.Started ||
		after.Methods.AuthCheckPassword.Started < before.Methods.AuthCheckPassword.Started ||
		after.Methods.Other.Started < before.Methods.Other.Started ||
		after.Connections.AuthKeyCreated < before.Connections.AuthKeyCreated ||
		after.Connections.TransportNegotiationError < before.Connections.TransportNegotiationError ||
		after.Exporter.Enqueued < before.Exporter.Enqueued || after.Exporter.Exported < before.Exporter.Exported ||
		after.Exporter.Dropped < before.Exporter.Dropped || after.Exporter.ExporterErrors < before.Exporter.ExporterErrors ||
		after.Exporter.ExporterPanics < before.Exporter.ExporterPanics {
		return true
	}
	for _, methods := range []struct {
		before mtproto.RPCProgressMethodSnapshot
		after  mtproto.RPCProgressMethodSnapshot
	}{
		{before.Methods.AuthSendCode, after.Methods.AuthSendCode},
		{before.Methods.AuthSignIn, after.Methods.AuthSignIn},
		{before.Methods.AuthCheckPassword, after.Methods.AuthCheckPassword},
		{before.Methods.Other, after.Methods.Other},
	} {
		beforeCounts, afterCounts := fixtureAuthProgressCounts(methods.before.Settled), fixtureAuthProgressCounts(methods.after.Settled)
		for index := range beforeCounts {
			if afterCounts[index] < beforeCounts[index] {
				return true
			}
		}
	}
	return false
}

func fixtureAuthProgressPending(methods mtproto.RPCProgressMethods) int64 {
	return methods.AuthSendCode.Pending + methods.AuthSignIn.Pending + methods.AuthCheckPassword.Pending + methods.Other.Pending
}

func fixtureAuthProgressCountValid(value int64) bool {
	return value >= 0 && value <= mtproto.RPCProgressMaxCount
}

func fixtureAuthProgressCounts(counts mtproto.RPCProgressResultCounts) [9]int64 {
	return [9]int64{counts.Success, counts.PasswordRequired, counts.InvalidRequest, counts.Unauthenticated,
		counts.Unauthorized, counts.RateLimited, counts.Deadline, counts.Internal, counts.TransportFailure}
}

func fixtureAuthProgressCountsFromArray(counts [9]int64) mtproto.RPCProgressResultCounts {
	return mtproto.RPCProgressResultCounts{
		Success: counts[0], PasswordRequired: counts[1], InvalidRequest: counts[2], Unauthenticated: counts[3],
		Unauthorized: counts[4], RateLimited: counts[5], Deadline: counts[6], Internal: counts[7], TransportFailure: counts[8],
	}
}

func fixtureAuthProgressMonotonicDelta(before, after int64) (int64, bool) {
	if !fixtureAuthProgressCountValid(before) || !fixtureAuthProgressCountValid(after) || after < before {
		return 0, false
	}
	return after - before, true
}

func TestRealServerFixtureAuthProgressModeIsExplicit(t *testing.T) {
	command := fixtureCommand(context.Background(), "bash", "real_server_fixture/run.sh",
		"--server-revision", "invalid",
		"--web-revision", realFixtureWebRevision,
		"--run-id", "00000000000000000000000000000000",
		"--auth-progress",
	)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err == nil {
		t.Fatal("fixture accepted an invalid server revision")
	}
	if !strings.Contains(stderr.String(), "server revision must be a full lowercase 40-character SHA") {
		t.Fatalf("auth-progress mode was rejected before revision validation: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("fixture wrote stdout before validating its revision: %q", stdout.String())
	}
}

func TestRealServerFixtureAuthProgressBuildIsTaggedAndProcessScoped(t *testing.T) {
	startScript, err := os.ReadFile(filepath.Join("real_server_fixture", "start.sh"))
	if err != nil {
		t.Fatal(err)
	}
	start := string(startScript)
	for _, required := range []string{
		`CGO_ENABLED=0 go build -tags fixtureauthprogress -o "$BUILD_DIR/telegramd" ./cmd/telegramd`,
		`CGO_ENABLED=0 go build -o "$BUILD_DIR/telegramd" ./cmd/telegramd`,
		`TG_FIXTURE_AUTH_PROGRESS=/run/log/$AUTH_PROGRESS_RELATIVE_PATH`,
		`--env "TG_FIXTURE_AUTH_PROGRESS_RUN_ID=$RUN_ID"`,
		`--tmpfs /run/log:rw,noexec,nosuid,nodev,size=8m,uid=65532,gid=65532`,
		`docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -USR1 $server_pid"`,
		`docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -TERM $server_pid"`,
		`docker exec --user 65532:65532 "$BACKEND" /bin/sh -c "kill -KILL $server_pid"`,
		`AUTH_PROGRESS_BASELINE_CAPTURED=0`,
		`AUTH_PROGRESS_BINDING_ATTEMPTED=0`,
		`if [[ $AUTH_PROGRESS_BASELINE_CAPTURED == 1 ]]; then`,
		`AUTH_PROGRESS_BASELINE_CAPTURED=1`,
		`if [[ $AUTH_PROGRESS_BINDING_ATTEMPTED == 1 && -n $AUTH_PROGRESS_OUTPUT ]]; then`,
		`kill -USR2 $server_pid`,
		`python3 "$SCRIPT_DIR/artifact.py" digest --directory "$snapshot_directory"`,
		`SECRET_ROOT=/dev/shm`,
		`signal_auth_progress_boundary runner_exit 2`,
	} {
		if !strings.Contains(start, required) {
			t.Errorf("fixture auth-progress lifecycle is missing %q", required)
		}
	}
	if strings.Contains(start, `--env "TG_FIXTURE_AUTH_PROGRESS=`) && strings.Contains(start, "docker run --detach --name \"$BACKEND\"") {
		serverLaunch := strings.Index(start, "docker run --detach --name \"$BACKEND\"")
		fixtureEnv := strings.Index(start, `--env "TG_FIXTURE_AUTH_PROGRESS=`)
		if fixtureEnv < serverLaunch {
			t.Fatal("fixture auth-progress environment is present in the backend container configuration")
		}
	}
	for _, dockerfile := range []string{filepath.Join("..", "..", "Dockerfile"), filepath.Join("..", "..", "Dockerfile.mc")} {
		data, err := os.ReadFile(dockerfile)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("fixtureauthprogress")) {
			t.Fatalf("production Dockerfile %s includes the fixture-only build tag", dockerfile)
		}
	}
}
