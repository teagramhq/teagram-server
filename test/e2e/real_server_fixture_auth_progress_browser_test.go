package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

type fixtureAuthProgressBrowserPasswordEvidence struct {
	Stage string `json:"stage"`
	Count int64  `json:"count"`
}

type fixtureAuthProgressBrowserReport struct {
	Status               string                                       `json:"status"`
	Mode                 string                                       `json:"mode"`
	Stage                string                                       `json:"stage"`
	UsernameSubmissions  int64                                        `json:"usernameSubmissions"`
	PasswordFills        int64                                        `json:"passwordFills"`
	PasswordSubmissions  int64                                        `json:"passwordSubmissions"`
	BobContextCount      int64                                        `json:"bobContextCount"`
	LaterScenarioCount   int64                                        `json:"laterScenarioCount"`
	ContextCount         int64                                        `json:"contextCount"`
	ControlContextCount  int64                                        `json:"controlContextCount"`
	ScreenshotsCaptured  int64                                        `json:"screenshotsCaptured"`
	PasswordEvidence     []fixtureAuthProgressBrowserPasswordEvidence `json:"passwordEvidence"`
	Network              fixtureAuthProgressBrowserNetwork            `json:"network"`
	WorkerObservation    json.RawMessage                              `json:"workerObservation"`
	RunID                string                                       `json:"runId"`
	HarnessRevision      string                                       `json:"harnessRevision"`
	ServerRevision       string                                       `json:"serverRevision"`
	WebRevision          string                                       `json:"webRevision"`
	ArtifactSourceCommit string                                       `json:"artifactSourceCommit"`
	ArtifactDigest       string                                       `json:"artifactDigest"`
}

type fixtureAuthProgressBrowserNetwork struct {
	UnexpectedAttempts int64                                               `json:"unexpectedAttempts"`
	ObserverErrors     int64                                               `json:"observerErrors"`
	Contexts           map[string]fixtureAuthProgressBrowserNetworkContext `json:"contexts"`
}

type fixtureAuthProgressBrowserNetworkContext struct {
	WorkerTargets              map[string]int64 `json:"workerTargets"`
	AllowedWebSockets          int64            `json:"allowedWebSockets"`
	AttemptsBySource           map[string]int64 `json:"attemptsBySource"`
	UnexpectedAttemptsBySource map[string]int64 `json:"unexpectedAttemptsBySource"`
	AllowedWebSocketsBySource  map[string]int64 `json:"allowedWebSocketsBySource"`
}

type fixtureAuthProgressBrowserWorkerObservation struct {
	UnexpectedAttempts    int64                                              `json:"unexpectedAttempts"`
	ObserverErrors        int64                                              `json:"observerErrors"`
	ForeignTargets        int64                                              `json:"foreignTargets"`
	ContextlessTargets    int64                                              `json:"contextlessTargets"`
	AppContextlessTargets int64                                              `json:"appContextlessTargets"`
	DiscoveryActive       int64                                              `json:"discoveryActive"`
	AttachedTargets       int64                                              `json:"attachedTargets"`
	ObserverOverflow      int64                                              `json:"observerOverflow"`
	CountedEvents         int64                                              `json:"countedEvents"`
	Contexts              map[string]fixtureAuthProgressBrowserWorkerContext `json:"contexts"`
	Controls              map[string]fixtureAuthProgressBrowserWorkerControl `json:"controls"`
}

type fixtureAuthProgressBrowserWorkerContext struct {
	PageTargets               int64    `json:"pageTargets"`
	SharedWorkerTargets       int64    `json:"sharedWorkerTargets"`
	BlobSharedWorkerTargets   int64    `json:"blobSharedWorkerTargets"`
	ScriptSharedWorkerTargets int64    `json:"scriptSharedWorkerTargets"`
	ServiceWorkerTargets      int64    `json:"serviceWorkerTargets"`
	DedicatedWorkerTargets    int64    `json:"dedicatedWorkerTargets"`
	WorkerSourceChunks        []string `json:"workerSourceChunks"`
	MtprotoSourceChunk        string   `json:"mtprotoSourceChunk"`
	MtprotoSourceFetch        string   `json:"mtprotoSourceFetch"`
	SharedWorkerState         string   `json:"sharedWorkerState"`
	MtprotoWorker             string   `json:"mtprotoWorker"`
	WorkerFailure             string   `json:"workerFailure"`
	WorkerFailureValidation   string   `json:"workerFailureValidation"`
	ObserverOverflow          int64    `json:"observerOverflow"`
	CountedEvents             int64    `json:"countedEvents"`
	AttachedTargets           int64    `json:"attachedTargets"`
	ObserverErrors            int64    `json:"observerErrors"`
	CoverageComplete          int64    `json:"coverageComplete"`
	DiscoveryActive           int64    `json:"discoveryActive"`
	PageCoverageComplete      int64    `json:"pageCoverageComplete"`
}

type fixtureAuthProgressBrowserWorkerControl struct {
	PageTargets               int64    `json:"pageTargets"`
	SharedWorkerTargets       int64    `json:"sharedWorkerTargets"`
	BlobSharedWorkerTargets   int64    `json:"blobSharedWorkerTargets"`
	ScriptSharedWorkerTargets int64    `json:"scriptSharedWorkerTargets"`
	ServiceWorkerTargets      int64    `json:"serviceWorkerTargets"`
	DedicatedWorkerTargets    int64    `json:"dedicatedWorkerTargets"`
	WorkerSourceChunks        []string `json:"workerSourceChunks"`
	MtprotoSourceChunk        string   `json:"mtprotoSourceChunk"`
	MtprotoSourceFetch        string   `json:"mtprotoSourceFetch"`
	SharedWorkerState         string   `json:"sharedWorkerState"`
	MtprotoWorker             string   `json:"mtprotoWorker"`
	WorkerFailure             string   `json:"workerFailure"`
	WorkerFailureValidation   string   `json:"workerFailureValidation"`
	ObserverOverflow          int64    `json:"observerOverflow"`
	CountedEvents             int64    `json:"countedEvents"`
	AttachedTargets           int64    `json:"attachedTargets"`
	ObserverErrors            int64    `json:"observerErrors"`
	CoverageComplete          int64    `json:"coverageComplete"`
	DiscoveryActive           int64    `json:"discoveryActive"`
	PageCoverageComplete      int64    `json:"pageCoverageComplete"`
	Expected                  string   `json:"expected"`
}

type fixtureAuthProgressBrowserSummary struct {
	ManifestArtifactDigest string `json:"manifest_artifact_digest"`
	Status                 string `json:"status"`
	Stage                  string `json:"stage"`
	UsernameSubmissions    int64  `json:"username_submissions"`
	PasswordFills          int64  `json:"password_fills"`
	PasswordSubmissions    int64  `json:"password_submissions"`
	PasswordFieldCount     *int64 `json:"password_field_count,omitempty"`
	ContextCount           int64  `json:"context_count"`
	ControlContextCount    int64  `json:"control_context_count"`
	UnexpectedAttempts     int64  `json:"unexpected_attempts"`
	ObserverErrors         int64  `json:"observer_errors"`
	AliceAllowedWebSockets *int64 `json:"alice_allowed_websockets,omitempty"`
}

type fixtureAuthProgressBrowserPairOutput struct {
	SchemaVersion          int32                              `json:"schema_version"`
	Status                 string                             `json:"status"`
	UnavailableReason      string                             `json:"unavailable_reason,omitempty"`
	RunnerRevision         string                             `json:"runner_revision,omitempty"`
	ManifestArtifactDigest string                             `json:"manifest_artifact_digest,omitempty"`
	FrozenArtifactDigest   string                             `json:"frozen_artifact_digest,omitempty"`
	TaggedIdentity         *mtproto.RPCProgressIdentity       `json:"tagged_identity,omitempty"`
	UntaggedIdentity       *mtproto.RPCProgressIdentity       `json:"untagged_identity,omitempty"`
	ControlOutcomes        *fixtureAuthControlResults         `json:"control_outcomes,omitempty"`
	ControlParity          *bool                              `json:"control_parity,omitempty"`
	TaggedBrowser          *fixtureAuthProgressBrowserSummary `json:"tagged_browser,omitempty"`
	UntaggedBrowser        *fixtureAuthProgressBrowserSummary `json:"untagged_browser,omitempty"`
	Server                 *fixtureAuthProgressWindow         `json:"server,omitempty"`
}

type fixtureAuthProgressBrowserPairResultInput struct {
	PairUnavailableReason string
	CleanupFailed         bool
	RunnerRevision        string
	Tagged                *realFixtureProcess
	Untagged              *realFixtureProcess
	TaggedControls        *fixtureAuthControlRecord
	UntaggedControls      *fixtureAuthControlRecord
	TaggedBrowser         *fixtureAuthProgressBrowserReport
	UntaggedBrowser       *fixtureAuthProgressBrowserReport
}

var fixtureAuthProgressBrowserStages = map[string]struct{}{
	"runtime_inputs": {}, "runtime_input_validation": {}, "runtime_credential_validation": {},
	"browser_launch": {}, "browser_worker_discovery": {}, "observer_controls": {}, "observer_control_mismatch": {},
	"alice_context": {}, "alice_sign_in_form": {}, "alice_artifact_load": {}, "alice_manifest_load": {},
	"alice_manifest_mismatch": {}, "alice_username_field": {}, "alice_username_field_not_unique": {},
	"alice_password_form": {}, "alice_password_field_ready": {}, "alice_password_field_absent": {},
	"alice_password_field_not_unique": {}, "alice_password_field_hidden": {}, "alice_password_field_unmasked": {},
	"alice_password_card_not_reached": {}, "browser_worker_observation": {}, "browser_egress_or_page_errors": {},
	"browser_worker_observation_incomplete": {}, "browser_worker_or_websocket_evidence": {},
	"worker_observation_unclassified": {}, "cleanup": {},
}

var fixtureAuthProgressBrowserSources = []string{"page", "shared_worker", "service_worker", "worker"}
var fixtureAuthProgressBrowserWorkerTargetSources = []string{"shared_worker", "service_worker"}

var fixtureAuthProgressBrowserControlNames = []string{
	"c1_shared_worker_created", "c2_missing_module_script", "c3_no_construction",
	"c4_attach_failure", "c4_overflow", "c5_attach_budget",
}

func decodeFixtureAuthProgressBrowserReport(data []byte, expected mtproto.RPCProgressIdentity, expectedManifestDigest string) (fixtureAuthProgressBrowserReport, error) {
	var report fixtureAuthProgressBrowserReport
	if expected.Validate() != nil || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(expectedManifestDigest) {
		return report, errors.New("expected browser observation identity is invalid")
	}
	if err := rejectFixtureAuthProgressDuplicateKeys(data); err != nil {
		return report, errors.New("browser observation JSON is invalid or has duplicate keys")
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeFor[fixtureAuthProgressBrowserReport]()); err != nil {
		return report, errors.New("browser observation JSON shape is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return report, errors.New("browser observation JSON decoding failed")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return report, errors.New("browser observation has trailing data")
	}
	if err := validateFixtureAuthProgressBrowserReport(report, data, expected, expectedManifestDigest); err != nil {
		return fixtureAuthProgressBrowserReport{}, errors.New("browser observation contract failed: " + err.Error())
	}
	return report, nil
}

func validateFixtureAuthProgressBrowserReport(report fixtureAuthProgressBrowserReport, data []byte, expected mtproto.RPCProgressIdentity, expectedManifestDigest string) error {
	if report.Mode != "auth_progress_one_submission" || (report.Status != "passed" && report.Status != "failed") {
		return errors.New("browser observation metadata is invalid")
	}
	if _, ok := fixtureAuthProgressBrowserStages[report.Stage]; !ok {
		return errors.New("browser observation stage is invalid")
	}
	if report.RunID != expected.RunID || report.HarnessRevision != expected.SourceRefs.HarnessRevision ||
		report.ServerRevision != expected.SourceRefs.ServerRevision || report.WebRevision != expected.SourceRefs.ArtifactSourceRevision ||
		report.ArtifactSourceCommit != expected.SourceRefs.ArtifactSourceRevision || report.ArtifactDigest != expectedManifestDigest {
		return errors.New("browser observation references differ from the bound run or manifest digest")
	}
	if report.UsernameSubmissions < 0 || report.UsernameSubmissions > 1 || report.PasswordFills != 0 ||
		report.PasswordSubmissions != 0 || report.BobContextCount != 0 || report.LaterScenarioCount != 0 ||
		report.ContextCount < 0 || report.ContextCount > 1 || report.ControlContextCount < 0 ||
		report.ControlContextCount > int64(len(fixtureAuthProgressBrowserControlNames)) || report.ScreenshotsCaptured != 0 {
		return errors.New("browser observation counts are invalid")
	}
	if report.ContextCount == 0 && report.UsernameSubmissions != 0 ||
		report.UsernameSubmissions == 1 && report.ContextCount != 1 ||
		report.ContextCount == 1 && report.ControlContextCount != int64(len(fixtureAuthProgressBrowserControlNames)) {
		return errors.New("browser observation context counts are inconsistent")
	}
	if len(report.PasswordEvidence) > 1 {
		return errors.New("browser observation password evidence is invalid")
	}
	for _, evidence := range report.PasswordEvidence {
		if !strings.HasPrefix(evidence.Stage, "alice_") || evidence.Count < 0 || evidence.Count > 1_000_000 ||
			!fixtureAuthProgressBrowserPasswordEvidenceValid(evidence) {
			return errors.New("browser observation password evidence is invalid")
		}
	}
	isPasswordStage := strings.HasPrefix(report.Stage, "alice_password_field_") || report.Stage == "alice_password_card_not_reached"
	if isPasswordStage && (len(report.PasswordEvidence) != 1 || report.PasswordEvidence[0].Stage != report.Stage || report.UsernameSubmissions != 1) ||
		len(report.PasswordEvidence) == 1 && report.UsernameSubmissions != 1 {
		return errors.New("browser observation password evidence does not match its stage")
	}
	if err := validateFixtureAuthProgressBrowserNetwork(report.Network, data, report.Status); err != nil {
		return err
	}
	workerObservation, err := validateFixtureAuthProgressBrowserWorkerObservation(report.WorkerObservation, report.Status)
	if err != nil {
		return err
	}
	if report.Status == "passed" && (!workerObservation || !fixtureAuthProgressBrowserSuccessful(report)) {
		return errors.New("successful one-submission observation is incomplete")
	}
	return nil
}

func validateFixtureAuthProgressBrowserWorkerObservation(data json.RawMessage, status string) (bool, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if status == "passed" {
			return false, errors.New("successful browser worker observation is missing")
		}
		return false, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return false, errors.New("browser worker observation is invalid")
	}
	if len(raw) == 1 {
		if classification, ok := raw["classification"]; ok {
			var value string
			if json.Unmarshal(classification, &value) == nil && value == "unclassified" && status == "failed" {
				return false, nil
			}
		}
	}
	var rawContexts map[string]json.RawMessage
	if err := json.Unmarshal(raw["contexts"], &rawContexts); err != nil || rawContexts == nil {
		return false, errors.New("browser worker contexts are invalid")
	}
	for name, context := range rawContexts {
		if !fixtureAuthProgressBrowserMapNameValid(name) ||
			validateFixtureAuthProgressJSONShape(context, reflect.TypeFor[fixtureAuthProgressBrowserWorkerContext]()) != nil {
			return false, errors.New("browser worker context schema is invalid")
		}
	}
	var rawControls map[string]json.RawMessage
	if err := json.Unmarshal(raw["controls"], &rawControls); err != nil || rawControls == nil {
		return false, errors.New("browser worker controls are invalid")
	}
	for name, control := range rawControls {
		if !fixtureAuthProgressBrowserMapNameValid(name) ||
			validateFixtureAuthProgressJSONShape(control, reflect.TypeFor[fixtureAuthProgressBrowserWorkerControl]()) != nil {
			return false, errors.New("browser worker control schema is invalid")
		}
	}
	if err := validateFixtureAuthProgressJSONShape(data, reflect.TypeFor[fixtureAuthProgressBrowserWorkerObservation]()); err != nil {
		return false, errors.New("browser worker observation schema is invalid")
	}
	var observation fixtureAuthProgressBrowserWorkerObservation
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil || observation.UnexpectedAttempts < 0 || observation.UnexpectedAttempts > 1_000_000 ||
		observation.ObserverErrors < 0 || observation.ObserverErrors > 4096 || observation.ForeignTargets < 0 || observation.ForeignTargets > 1_000_000 ||
		observation.ContextlessTargets < 0 || observation.ContextlessTargets > 1_000_000 || observation.AppContextlessTargets < 0 || observation.AppContextlessTargets > 1_000_000 ||
		!fixtureAuthProgressBrowserFlag(observation.DiscoveryActive) || observation.AttachedTargets < 0 || observation.AttachedTargets > 4096 ||
		!fixtureAuthProgressBrowserFlag(observation.ObserverOverflow) || observation.CountedEvents < 0 || observation.CountedEvents > 1_000_000 ||
		observation.Contexts == nil || observation.Controls == nil {
		return false, errors.New("browser worker observation counts are invalid")
	}
	for _, context := range observation.Contexts {
		if !fixtureAuthProgressBrowserWorkerContextValid(context) {
			return false, errors.New("browser worker context is invalid")
		}
	}
	for _, control := range observation.Controls {
		context := fixtureAuthProgressBrowserWorkerContext{
			PageTargets: control.PageTargets, SharedWorkerTargets: control.SharedWorkerTargets,
			BlobSharedWorkerTargets: control.BlobSharedWorkerTargets, ScriptSharedWorkerTargets: control.ScriptSharedWorkerTargets,
			ServiceWorkerTargets: control.ServiceWorkerTargets, DedicatedWorkerTargets: control.DedicatedWorkerTargets,
			WorkerSourceChunks: control.WorkerSourceChunks, MtprotoSourceChunk: control.MtprotoSourceChunk,
			MtprotoSourceFetch: control.MtprotoSourceFetch, SharedWorkerState: control.SharedWorkerState,
			MtprotoWorker: control.MtprotoWorker, WorkerFailure: control.WorkerFailure,
			WorkerFailureValidation: control.WorkerFailureValidation, ObserverOverflow: control.ObserverOverflow,
			CountedEvents: control.CountedEvents, AttachedTargets: control.AttachedTargets,
			ObserverErrors: control.ObserverErrors, CoverageComplete: control.CoverageComplete,
			DiscoveryActive: control.DiscoveryActive, PageCoverageComplete: control.PageCoverageComplete,
		}
		if !fixtureAuthProgressBrowserWorkerContextValid(context) || (control.Expected != "pass" && control.Expected != "fail") {
			return false, errors.New("browser worker control is invalid")
		}
	}
	if status == "passed" {
		if observation.DiscoveryActive != 1 || observation.ForeignTargets != 0 || observation.AppContextlessTargets != 0 ||
			observation.ObserverOverflow != 0 || len(observation.Contexts) != 1 || len(observation.Controls) != len(fixtureAuthProgressBrowserControlNames) {
			return false, errors.New("successful browser worker observation is incomplete")
		}
		alice, ok := observation.Contexts["alice"]
		if !ok || alice.PageTargets != 1 || alice.CoverageComplete != 1 {
			return false, errors.New("successful browser worker context is incomplete")
		}
		for _, name := range fixtureAuthProgressBrowserControlNames {
			if control, ok := observation.Controls[name]; !ok || control.Expected != "pass" {
				return false, errors.New("successful browser worker controls are incomplete")
			}
		}
	}
	return true, nil
}

func fixtureAuthProgressBrowserWorkerContextValid(context fixtureAuthProgressBrowserWorkerContext) bool {
	for _, count := range []int64{context.PageTargets, context.SharedWorkerTargets, context.BlobSharedWorkerTargets,
		context.ScriptSharedWorkerTargets, context.ServiceWorkerTargets, context.DedicatedWorkerTargets} {
		if count < 0 || count > 512 {
			return false
		}
	}
	if context.PageTargets > 8 || context.CountedEvents < 0 || context.CountedEvents > 512 ||
		context.AttachedTargets < 0 || context.AttachedTargets > 16 || context.ObserverErrors < 0 || context.ObserverErrors > 4096 ||
		!fixtureAuthProgressBrowserFlag(context.ObserverOverflow) || !fixtureAuthProgressBrowserFlag(context.CoverageComplete) ||
		!fixtureAuthProgressBrowserFlag(context.DiscoveryActive) || !fixtureAuthProgressBrowserFlag(context.PageCoverageComplete) {
		return false
	}
	if context.MtprotoSourceChunk != "none" && context.MtprotoSourceChunk != "mtproto_worker" ||
		!fixtureAuthProgressBrowserEnum(context.MtprotoSourceFetch, "not_requested", "ok", "http_4xx", "http_5xx", "failed") ||
		!fixtureAuthProgressBrowserEnum(context.SharedWorkerState, "created", "absent", "unknown") ||
		!fixtureAuthProgressBrowserEnum(context.MtprotoWorker, "mtproto_candidate", "ambiguous", "absent", "unknown") ||
		!fixtureAuthProgressBrowserEnum(context.WorkerFailure, "script_load_failed", "module_resolve_failed", "module_fetch_failed", "evaluation_exception", "csp_blocked", "destroyed_before_attach", "no_failure_observed", "unknown") ||
		!fixtureAuthProgressBrowserEnum(context.WorkerFailureValidation, "validated", "unvalidated") {
		return false
	}
	allowedChunks := map[string]struct{}{
		"mtproto_worker": {}, "crypto_worker": {}, "lottie_worker": {}, "spoiler_worker": {}, "webp_worker": {},
		"tinyld_worker": {}, "compositor_worker": {}, "service_worker": {}, "probe_shared_worker": {},
		"probe_missing_script": {}, "blob_same_origin": {}, "other_same_origin": {}, "cross_origin": {},
	}
	seenChunks := make(map[string]struct{}, len(context.WorkerSourceChunks))
	for _, chunk := range context.WorkerSourceChunks {
		if _, ok := allowedChunks[chunk]; !ok {
			return false
		}
		if _, duplicate := seenChunks[chunk]; duplicate {
			return false
		}
		seenChunks[chunk] = struct{}{}
	}
	return true
}

func fixtureAuthProgressBrowserFlag(value int64) bool {
	return value == 0 || value == 1
}

func fixtureAuthProgressBrowserEnum(value string, allowed ...string) bool {
	return slices.Contains(allowed, value)
}

func fixtureAuthProgressBrowserMapNameValid(value string) bool {
	if len(value) == 0 || len(value) > 32 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, character := range value[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}

func fixtureAuthProgressBrowserPasswordEvidenceValid(evidence fixtureAuthProgressBrowserPasswordEvidence) bool {
	switch evidence.Stage {
	case "alice_password_field_ready", "alice_password_field_hidden", "alice_password_field_unmasked":
		return evidence.Count == 1
	case "alice_password_field_absent":
		return evidence.Count == 0
	case "alice_password_field_not_unique":
		return evidence.Count >= 2
	case "alice_password_card_not_reached":
		return evidence.Count <= 1
	default:
		return false
	}
}

func fixtureAuthProgressBrowserSuccessful(report fixtureAuthProgressBrowserReport) bool {
	context, ok := report.Network.Contexts["alice"]
	return report.Stage == "alice_password_field_ready" && report.UsernameSubmissions == 1 &&
		len(report.PasswordEvidence) == 1 && report.PasswordEvidence[0] == (fixtureAuthProgressBrowserPasswordEvidence{Stage: "alice_password_field_ready", Count: 1}) &&
		report.ContextCount == 1 && report.ControlContextCount == int64(len(fixtureAuthProgressBrowserControlNames)) &&
		report.Network.UnexpectedAttempts == 0 && report.Network.ObserverErrors == 0 && ok &&
		context.AllowedWebSockets > 0 && context.WorkerTargets["service_worker"] > 0
}

func validateFixtureAuthProgressBrowserNetwork(network fixtureAuthProgressBrowserNetwork, data []byte, status string) error {
	if network.UnexpectedAttempts < 0 || network.UnexpectedAttempts > 1_000_000 || network.ObserverErrors < 0 || network.ObserverErrors > 4096 {
		return errors.New("browser network counts are invalid")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return errors.New("browser observation object is invalid")
	}
	var rawNetwork map[string]json.RawMessage
	if err := json.Unmarshal(root["network"], &rawNetwork); err != nil || !fixtureAuthProgressBrowserJSONKeys(rawNetwork, "unexpectedAttempts", "observerErrors", "contexts") {
		return errors.New("browser network schema is invalid")
	}
	var rawContexts map[string]json.RawMessage
	if err := json.Unmarshal(rawNetwork["contexts"], &rawContexts); err != nil || rawContexts == nil || len(rawContexts) > 1 {
		return errors.New("browser network contexts are invalid")
	}
	if _, ok := rawContexts["alice"]; !ok && len(rawContexts) != 0 {
		return errors.New("browser network contains an unknown context")
	}
	if status == "passed" && len(rawContexts) != 1 {
		return errors.New("successful browser observation has no Alice network evidence")
	}
	var unexpectedTotal int64
	for name, rawContext := range rawContexts {
		if name != "alice" || validateFixtureAuthProgressJSONShape(rawContext, reflect.TypeFor[fixtureAuthProgressBrowserNetworkContext]()) != nil {
			return errors.New("browser network context schema is invalid")
		}
		var context fixtureAuthProgressBrowserNetworkContext
		if err := json.Unmarshal(rawContext, &context); err != nil || context.AllowedWebSockets < 0 || context.AllowedWebSockets > 1_000_000 ||
			!fixtureAuthProgressBrowserWorkerTargetsValid(context.WorkerTargets) ||
			!fixtureAuthProgressBrowserSourceCountsValid(context.AttemptsBySource) ||
			!fixtureAuthProgressBrowserSourceCountsValid(context.UnexpectedAttemptsBySource) ||
			!fixtureAuthProgressBrowserSourceCountsValid(context.AllowedWebSocketsBySource) {
			return errors.New("browser network source counts are invalid")
		}
		var allowedTotal, contextUnexpected int64
		for _, source := range fixtureAuthProgressBrowserSources {
			attempts := context.AttemptsBySource[source]
			unexpected := context.UnexpectedAttemptsBySource[source]
			allowed := context.AllowedWebSocketsBySource[source]
			if unexpected > attempts || allowed > attempts {
				return errors.New("browser network source counts are inconsistent")
			}
			allowedTotal += allowed
			contextUnexpected += unexpected
		}
		if allowedTotal != context.AllowedWebSockets {
			return errors.New("browser websocket source counts are inconsistent")
		}
		unexpectedTotal += contextUnexpected
	}
	if unexpectedTotal != network.UnexpectedAttempts {
		return errors.New("browser unexpected-attempt totals are inconsistent")
	}
	return nil
}

func fixtureAuthProgressBrowserSourceCountsValid(counts map[string]int64) bool {
	if len(counts) != len(fixtureAuthProgressBrowserSources) {
		return false
	}
	for _, source := range fixtureAuthProgressBrowserSources {
		count, ok := counts[source]
		if !ok || count < 0 || count > 1_000_000 {
			return false
		}
	}
	return true
}

func fixtureAuthProgressBrowserWorkerTargetsValid(targets map[string]int64) bool {
	if len(targets) != len(fixtureAuthProgressBrowserWorkerTargetSources) {
		return false
	}
	for _, source := range fixtureAuthProgressBrowserWorkerTargetSources {
		count, ok := targets[source]
		if !ok || count < 0 || count > 1_000_000 {
			return false
		}
	}
	return true
}

func fixtureAuthProgressBrowserJSONKeys(object map[string]json.RawMessage, expected ...string) bool {
	if len(object) != len(expected) {
		return false
	}
	for _, key := range expected {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	return true
}

func validateFixtureAuthProgressBrowserServerDelta(browser fixtureAuthProgressBrowserReport, server fixtureAuthProgressResult, manifestDigest, boundArtifactDigest string) error {
	if !server.Available || server.Window == nil || server.Identity.RunID != browser.RunID ||
		server.Identity.SourceRefs.RunnerRevision != fixtureAuthProgressRunnerRevision ||
		server.Identity.SourceRefs.ArtifactSourceRevision != browser.WebRevision ||
		browser.ArtifactDigest != manifestDigest || server.Identity.SourceRefs.ArtifactDigest != boundArtifactDigest ||
		server.Identity.SourceRefs.HarnessRevision != browser.HarnessRevision ||
		server.Identity.SourceRefs.ServerRevision != browser.ServerRevision {
		return errors.New("browser and server observations do not bind the same fixture identity")
	}
	window := server.Window
	if window.Methods.AuthCheckPassword.Started != 0 || window.Methods.AuthCheckPassword.State != "not_reached" ||
		window.Methods.AuthCheckPassword.Settled != (mtproto.RPCProgressResultCounts{}) {
		return errors.New("browser observation includes a password check")
	}
	if browser.Status == "passed" {
		if window.Methods.AuthSendCode.State != "settled" || window.Methods.AuthSendCode.Started != 1 ||
			window.Methods.AuthSendCode.Settled != (mtproto.RPCProgressResultCounts{Success: 1}) ||
			window.Methods.AuthSignIn.State != "settled" || window.Methods.AuthSignIn.Started != 1 ||
			window.Methods.AuthSignIn.Settled != (mtproto.RPCProgressResultCounts{PasswordRequired: 1}) {
			return errors.New("successful browser submission differs from measured server outcomes")
		}
		return nil
	}
	if window.Methods.AuthSendCode.Started > 1 || window.Methods.AuthSignIn.Started > 1 ||
		browser.UsernameSubmissions == 0 && (window.Methods.AuthSendCode.Started != 0 || window.Methods.AuthSignIn.Started != 0) {
		return errors.New("failed browser stage differs from measured server action counts")
	}
	return nil
}

func TestFixtureRunnerEnvironmentExcludesAmbientRuntimeAndCredentials(t *testing.T) {
	t.Setenv("PATH", "/trusted/tools")
	t.Setenv("HOME", "/caller/home")
	t.Setenv("TMPDIR", "/caller/tmp")
	t.Setenv("CI", "false")
	t.Setenv("DOCKER_HOST", "tcp://fixture-docker:2375")
	t.Setenv("DOCKER_CONFIG", "/caller/docker-config")
	t.Setenv("DOCKER_AUTH_CONFIG", "caller-docker-credentials")
	t.Setenv("NODE_OPTIONS", "--require=/tmp/injected.cjs")
	t.Setenv("NODE_PATH", "/tmp/injected-modules")
	t.Setenv("NODE_EXTRA_CA_CERTS", "/tmp/injected-ca.pem")
	t.Setenv("LD_PRELOAD", "/tmp/injected.so")
	t.Setenv("DYLD_INSERT_LIBRARIES", "/tmp/injected.dylib")
	t.Setenv("BASH_ENV", "/tmp/injected.sh")
	t.Setenv("PYTHONPATH", "/tmp/injected-python")
	t.Setenv("NPM_TOKEN", "npm-secret")
	t.Setenv("GITHUB_TOKEN", "github-secret")
	t.Setenv("AWS_ACCESS_KEY_ID", "aws-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/agent.sock")
	t.Setenv("HTTPS_PROXY", "https://proxy-user:proxy-password@example.invalid")
	t.Setenv("NPM_CONFIG_USERCONFIG", "/caller/npmrc")
	t.Setenv("NPM_CONFIG_GLOBALCONFIG", "/caller/global-npmrc")
	t.Setenv("TG_FIXTURE_AUTH_PROGRESS", "/tmp/caller-progress.json")
	t.Setenv("MTPROTO_PRIVATE_ENDPOINT", "wss://caller.invalid")

	want := []string{
		"PATH=/trusted/tools",
		"HOME=/caller/home",
		"TMPDIR=/tmp",
		"CI=true",
		"DOCKER_HOST=tcp://fixture-docker:2375",
		"NPM_CONFIG_USERCONFIG=/dev/null",
		"NPM_CONFIG_GLOBALCONFIG=/dev/null",
	}
	if got := fixtureRunnerEnvironment(); !reflect.DeepEqual(got, want) {
		t.Fatalf("runner environment = %q, want only the minimal runner environment %q", got, want)
	}
}

func TestRealServerFixtureAuthProgressBrowserReportStrictSchema(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	manifestDigest := fixtureAuthProgressBrowserTestManifestDigest()
	ready := realFixtureArtifactReady{
		Event: "artifact-ready", Status: "ready", RunID: identity.RunID,
		HarnessRevision: identity.SourceRefs.HarnessRevision, ServerRevision: identity.SourceRefs.ServerRevision,
		WebRevision: identity.SourceRefs.ArtifactSourceRevision, ArtifactDigest: identity.SourceRefs.ArtifactDigest,
		ManifestArtifactDigest: manifestDigest, FrozenArtifactDigest: identity.SourceRefs.ArtifactDigest,
		LegDigestBefore: identity.SourceRefs.ArtifactDigest,
	}
	runnerReadyLine, err := fixtureAuthProgressBrowserRunnerReadinessLine(ready, identity)
	if err != nil {
		t.Fatalf("build runner-bound readiness view: %v", err)
	}
	var runnerReady realFixtureArtifactReady
	if err := json.Unmarshal(runnerReadyLine, &runnerReady); err != nil {
		t.Fatalf("decode runner-bound readiness view: %v", err)
	}
	if runnerReady.ArtifactDigest != manifestDigest || runnerReady.ManifestArtifactDigest != manifestDigest ||
		runnerReady.FrozenArtifactDigest != identity.SourceRefs.ArtifactDigest || ready.ArtifactDigest != identity.SourceRefs.ArtifactDigest {
		t.Fatalf("runner readiness did not keep manifest and full-tree digests separate: ready=%+v runner=%+v", ready, runnerReady)
	}
	report, err := decodeFixtureAuthProgressBrowserReport(fixtureAuthProgressBrowserPassingJSON(t, identity), identity, manifestDigest)
	if err != nil {
		t.Fatalf("decode accepted one-submission report: %v", err)
	}
	if report.Status != "passed" || report.Stage != "alice_password_field_ready" ||
		report.UsernameSubmissions != 1 || report.PasswordFills != 0 || report.PasswordSubmissions != 0 ||
		report.BobContextCount != 0 || report.LaterScenarioCount != 0 || report.ContextCount != 1 ||
		report.ControlContextCount != 6 || report.ScreenshotsCaptured != 0 || len(report.PasswordEvidence) != 1 ||
		report.PasswordEvidence[0] != (fixtureAuthProgressBrowserPasswordEvidence{Stage: "alice_password_field_ready", Count: 1}) {
		t.Fatalf("decoded browser observation did not preserve the actual card stage and counts: %+v", report)
	}
}

func TestRealServerFixtureAuthProgressBrowserReportRejectsMalformedOrForeignOutput(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	manifestDigest := fixtureAuthProgressBrowserTestManifestDigest()
	valid := fixtureAuthProgressBrowserPassingJSON(t, identity)
	mutations := []struct {
		name string
		data func() []byte
	}{
		{name: "duplicate key", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"status":"passed"`, `"status":"passed","status":"passed"`, 1))
		}},
		{name: "extra key", data: func() []byte {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(valid, &object); err != nil {
				t.Fatal(err)
			}
			object["rawCapture"] = json.RawMessage(`"forbidden"`)
			data, err := json.Marshal(object)
			if err != nil {
				t.Fatal(err)
			}
			return data
		}},
		{name: "trailing value", data: func() []byte { return append(append([]byte(nil), valid...), []byte(` {}`)...) }},
		{name: "foreign digest", data: func() []byte {
			return []byte(strings.Replace(string(valid), manifestDigest, "sha256:"+strings.Repeat("8", 64), 1))
		}},
		{name: "foreign run id", data: func() []byte {
			return fixtureAuthProgressBrowserReportWithForeignStringField(t, valid, "runId", strings.Repeat("1", 32))
		}},
		{name: "foreign harness revision", data: func() []byte {
			return fixtureAuthProgressBrowserReportWithForeignStringField(t, valid, "harnessRevision", strings.Repeat("2", 40))
		}},
		{name: "foreign server revision", data: func() []byte {
			return fixtureAuthProgressBrowserReportWithForeignStringField(t, valid, "serverRevision", strings.Repeat("3", 40))
		}},
		{name: "foreign web revision", data: func() []byte {
			return fixtureAuthProgressBrowserReportWithForeignStringField(t, valid, "webRevision", strings.Repeat("4", 40))
		}},
		{name: "foreign artifact source commit", data: func() []byte {
			return fixtureAuthProgressBrowserReportWithForeignStringField(t, valid, "artifactSourceCommit", strings.Repeat("5", 40))
		}},
		{name: "password submitted", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"passwordSubmissions":0`, `"passwordSubmissions":1`, 1))
		}},
		{name: "non-enum stage", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"alice_password_field_ready"`, `"free-form stage"`, 1))
		}},
		{name: "out of range count", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"usernameSubmissions":1`, `"usernameSubmissions":2`, 1))
		}},
		{name: "nested worker extra field", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"pageTargets":1`, `"pageTargets":1,"rawUrl":"forbidden"`, 1))
		}},
		{name: "nested worker missing field", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"pageTargets":1,`, "", 1))
		}},
		{name: "worker targets reject event source keys", data: func() []byte {
			return []byte(strings.Replace(
				string(valid),
				`"workerTargets":{"shared_worker":1,"service_worker":1}`,
				`"workerTargets":{"page":0,"shared_worker":1,"service_worker":1}`,
				1,
			))
		}},
		{name: "worker targets require both worker keys", data: func() []byte {
			return []byte(strings.Replace(
				string(valid),
				`"workerTargets":{"shared_worker":1,"service_worker":1}`,
				`"workerTargets":{"shared_worker":1}`,
				1,
			))
		}},
		{name: "worker target count rejects null", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"workerTargets":{"shared_worker":1`, `"workerTargets":{"shared_worker":null`, 1))
		}},
		{name: "worker target count is bounded", data: func() []byte {
			return []byte(strings.Replace(
				string(valid),
				`"workerTargets":{"shared_worker":1,"service_worker":1}`,
				`"workerTargets":{"shared_worker":1,"service_worker":1000001}`,
				1,
			))
		}},
		{name: "source count is bounded", data: func() []byte {
			return []byte(strings.Replace(
				string(valid),
				`"attemptsBySource":{"page":1,`,
				`"attemptsBySource":{"page":1000001,`,
				1,
			))
		}},
		{name: "attempt source count rejects null", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"attemptsBySource":{"page":1,"shared_worker":0`, `"attemptsBySource":{"page":1,"shared_worker":null`, 1))
		}},
		{name: "unexpected attempt source count rejects null", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"unexpectedAttemptsBySource":{"page":0`, `"unexpectedAttemptsBySource":{"page":null`, 1))
		}},
		{name: "allowed websocket source count rejects null", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"allowedWebSocketsBySource":{"page":1,"shared_worker":0`, `"allowedWebSocketsBySource":{"page":1,"shared_worker":null`, 1))
		}},
		{name: "allowed websocket aggregate matches source counts", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"allowedWebSockets":1`, `"allowedWebSockets":2`, 1))
		}},
		{name: "unexpected attempt aggregate matches source counts", data: func() []byte {
			return []byte(strings.Replace(string(valid), `"unexpectedAttempts":0`, `"unexpectedAttempts":1`, 1))
		}},
		{name: "unexpected source count cannot exceed attempts", data: func() []byte {
			data := strings.Replace(string(valid), `"unexpectedAttempts":0`, `"unexpectedAttempts":2`, 1)
			return []byte(strings.Replace(data, `"unexpectedAttemptsBySource":{"page":0`, `"unexpectedAttemptsBySource":{"page":2`, 1))
		}},
		{name: "allowed source count cannot exceed attempts", data: func() []byte {
			data := strings.Replace(string(valid), `"allowedWebSockets":1`, `"allowedWebSockets":2`, 1)
			return []byte(strings.Replace(data, `"allowedWebSocketsBySource":{"page":1`, `"allowedWebSocketsBySource":{"page":2`, 1))
		}},
	}
	for _, mutation := range mutations {
		data := mutation.data()
		if _, err := decodeFixtureAuthProgressBrowserReport(data, identity, manifestDigest); err == nil {
			t.Errorf("accepted %s browser output", mutation.name)
		}
	}
}

func fixtureAuthProgressBrowserReportWithForeignStringField(t *testing.T, data []byte, field, value string) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	encodedValue, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	object[field] = encodedValue
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestFixtureAuthProgressBrowserPairResultInvalidatesUnavailablePairs(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(*fixtureAuthProgressBrowserPairResultInput)
		want   string
	}{
		{
			name: "control parity mismatch",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.UntaggedControls.Results.AuthSignUp.Accounts++
			},
			want: "unavailable_malformed_output",
		},
		{
			name: "cleanup failure",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.CleanupFailed = true
			},
			want: "unavailable_teardown_failure",
		},
		{
			name: "server delta mismatch",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.Tagged.authProgressResult.Window.Methods.AuthCheckPassword.Started = 1
				input.Tagged.authProgressResult.Window.Methods.AuthCheckPassword.State = "settled"
			},
			want: "unavailable_malformed_output",
		},
		{
			name: "tagged server unavailable",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.Tagged.authProgressResult.Available = false
				input.Tagged.authProgressResult.UnavailableReason = "unavailable_counter_overflow"
				input.Tagged.authProgressResult.Window = nil
			},
			want: "unavailable_counter_overflow",
		},
		{
			name: "malformed pair reason precedes cleanup and tagged teardown",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.PairUnavailableReason = "unavailable_malformed_output"
				input.CleanupFailed = true
				input.Tagged.authProgressResult.Available = false
				input.Tagged.authProgressResult.UnavailableReason = "unavailable_teardown_failure"
				input.Tagged.authProgressResult.Window = nil
			},
			want: "unavailable_malformed_output",
		},
		{
			name: "cleanup reason precedes tagged incomplete",
			mutate: func(input *fixtureAuthProgressBrowserPairResultInput) {
				input.CleanupFailed = true
				input.Tagged.authProgressResult.Available = false
				input.Tagged.authProgressResult.UnavailableReason = "unavailable_incomplete"
				input.Tagged.authProgressResult.Window = nil
			},
			want: "unavailable_teardown_failure",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			input := fixtureAuthProgressBrowserPairResultTestInput(t)
			testCase.mutate(&input)
			got := fixtureAuthProgressBrowserPairResultFor(input)
			if got.Status != "unavailable" || got.UnavailableReason != testCase.want {
				t.Fatalf("pair result status=%q reason=%q, want unavailable/%s", got.Status, got.UnavailableReason, testCase.want)
			}
			if got.TaggedBrowser != nil || got.UntaggedBrowser != nil || got.Server != nil {
				t.Fatalf("unavailable pair exposed usable deltas: %+v", got)
			}
		})
	}
}

func TestFixtureAuthProgressBrowserPairResultPublishesValidatedDeltas(t *testing.T) {
	got := fixtureAuthProgressBrowserPairResultFor(fixtureAuthProgressBrowserPairResultTestInput(t))
	if got.Status != "available" || got.UnavailableReason != "" || got.ControlParity == nil || !*got.ControlParity ||
		got.TaggedBrowser == nil || got.UntaggedBrowser == nil || got.Server == nil {
		t.Fatalf("valid pair result = %+v, want available validated deltas", got)
	}
}

func fixtureAuthProgressBrowserPairResultTestInput(t *testing.T) fixtureAuthProgressBrowserPairResultInput {
	t.Helper()
	identity := fixtureAuthProgressBrowserTestIdentity()
	manifestDigest := fixtureAuthProgressBrowserTestManifestDigest()
	browser, err := decodeFixtureAuthProgressBrowserReport(fixtureAuthProgressBrowserPassingJSON(t, identity), identity, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	serverReport := newFixtureAuthProgressTestReport(identity)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[0].Server.Methods.AuthSendCode, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[0].Server.Methods.AuthSignIn, 2, mtproto.RPCProgressResultCounts{PasswordRequired: 2}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[1].Server.Methods.AuthSendCode, 3, mtproto.RPCProgressResultCounts{Success: 3}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[1].Server.Methods.AuthSignIn, 3, mtproto.RPCProgressResultCounts{PasswordRequired: 3}, 0)
	serverReport.ShutdownFinal = serverReport.Boundaries[1].Server
	serverReport.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
	serverReport.ShutdownFinal.ShutdownComplete = true
	encoded, err := json.Marshal(serverReport)
	if err != nil {
		t.Fatal(err)
	}
	server := consumeFixtureAuthProgress(encoded, identity, false, false)
	if !server.Available || server.Window == nil {
		t.Fatalf("build valid pair result test server window: %+v", server)
	}
	ready := realFixtureArtifactReady{
		ManifestArtifactDigest: manifestDigest,
		FrozenArtifactDigest:   identity.SourceRefs.ArtifactDigest,
	}
	controlResults := fixtureAuthControlResults{
		AuthSendCode:       fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthSignIn:         fixtureAuthControlOutcome{Result: "password_required", Accounts: 2},
		AccountGetPassword: fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthCheckPassword:  fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthSignUp:         fixtureAuthControlOutcome{Result: "invalid_request", Accounts: 1},
	}
	return fixtureAuthProgressBrowserPairResultInput{
		RunnerRevision: fixtureAuthProgressRunnerRevision,
		Tagged: &realFixtureProcess{
			authProgressIdentity: &identity,
			authProgressResult:   &server,
			artifactReady:        &ready,
		},
		Untagged:         &realFixtureProcess{authProgressIdentity: &identity},
		TaggedControls:   &fixtureAuthControlRecord{Results: controlResults},
		UntaggedControls: &fixtureAuthControlRecord{Results: controlResults},
		TaggedBrowser:    &browser,
		UntaggedBrowser:  &browser,
	}
}

func TestRunFixtureAuthProgressOneSubmissionRejectsRunnerCleanupFailure(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	manifestDigest := fixtureAuthProgressBrowserTestManifestDigest()
	var runnerReportObject map[string]json.RawMessage
	if err := json.Unmarshal(fixtureAuthProgressBrowserPassingJSON(t, identity), &runnerReportObject); err != nil {
		t.Fatal(err)
	}
	runnerReportObject["status"] = json.RawMessage(`"failed"`)
	runnerReportObject["stage"] = json.RawMessage(`"cleanup"`)
	runnerReportObject["passwordEvidence"] = json.RawMessage(`[]`)
	runnerReport, err := json.Marshal(runnerReportObject)
	if err != nil {
		t.Fatal(err)
	}

	binDirectory := t.TempDir()
	runnerCheckout := t.TempDir()
	gitPath := filepath.Join(binDirectory, "git")
	gitScript := "#!/bin/sh\ncase \"$*\" in\n  *\"rev-parse HEAD\") printf '%s\\n' '" + fixtureAuthProgressRunnerRevision + "' ;;\n  *\"status --porcelain --untracked-files=all\") exit 0 ;;\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(gitPath, []byte(gitScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(gitPath, 0o700); err != nil { //nolint:gosec // The test uses a temporary git stub to verify the pinned revision.
		t.Fatal(err)
	}
	rtkPath := filepath.Join(binDirectory, "rtk")
	if err := os.WriteFile(rtkPath, []byte("#!/bin/sh\ncat \"${0}.report\"\nexit 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(rtkPath, 0o700); err != nil { //nolint:gosec // The test executes a temporary runner stub.
		t.Fatal(err)
	}
	if err := os.WriteFile(rtkPath+".report", runnerReport, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDirectory+":"+os.Getenv("PATH"))

	ready := realFixtureArtifactReady{
		Event: "artifact-ready", Status: "ready", RunID: identity.RunID,
		HarnessRevision: identity.SourceRefs.HarnessRevision, ServerRevision: identity.SourceRefs.ServerRevision,
		WebRevision: identity.SourceRefs.ArtifactSourceRevision, ArtifactDigest: identity.SourceRefs.ArtifactDigest,
		ManifestArtifactDigest: manifestDigest, FrozenArtifactDigest: identity.SourceRefs.ArtifactDigest,
		LegDigestBefore: identity.SourceRefs.ArtifactDigest,
	}
	fixture := &realFixtureProcess{
		readyLine: "{}\n", artifactReadyLine: "{}\n", artifactReady: &ready,
		authProgressIdentity: &identity,
	}
	report, unavailableReason, err := runFixtureAuthProgressOneSubmission(context.Background(), runnerCheckout, fixture)
	if err == nil || unavailableReason != "unavailable_teardown_failure" || !reflect.DeepEqual(report, fixtureAuthProgressBrowserReport{}) {
		t.Fatalf("cleanup failure result = report:%+v reason:%q err:%v; want empty report and unavailable_teardown_failure", report, unavailableReason, err)
	}
}

func TestRealServerFixtureAuthProgressBrowserObservationPreservesMeasuredOutcomes(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	manifestDigest := fixtureAuthProgressBrowserTestManifestDigest()
	browser, err := decodeFixtureAuthProgressBrowserReport(fixtureAuthProgressBrowserPassingJSON(t, identity), identity, manifestDigest)
	if err != nil {
		t.Fatal(err)
	}
	serverReport := newFixtureAuthProgressTestReport(identity)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[0].Server.Methods.AuthSendCode, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[0].Server.Methods.AuthSignIn, 2, mtproto.RPCProgressResultCounts{PasswordRequired: 2}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[0].Server.Methods.AuthCheckPassword, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[1].Server.Methods.AuthSendCode, 3, mtproto.RPCProgressResultCounts{Success: 3}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[1].Server.Methods.AuthSignIn, 3, mtproto.RPCProgressResultCounts{PasswordRequired: 3}, 0)
	setFixtureAuthProgressMethod(&serverReport.Boundaries[1].Server.Methods.AuthCheckPassword, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	serverReport.ShutdownFinal = serverReport.Boundaries[1].Server
	serverReport.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
	serverReport.ShutdownFinal.ShutdownComplete = true
	encoded, err := json.Marshal(serverReport)
	if err != nil {
		t.Fatal(err)
	}
	server := consumeFixtureAuthProgress(encoded, identity, false, false)
	if err := validateFixtureAuthProgressBrowserServerDelta(browser, server, manifestDigest, identity.SourceRefs.ArtifactDigest); err != nil {
		t.Fatalf("one username submission was not matched to actual server counts: %v", err)
	}
	if server.Window.Methods.AuthSendCode.Settled.Success != 1 ||
		server.Window.Methods.AuthSignIn.Settled.PasswordRequired != 1 ||
		server.Window.Methods.AuthCheckPassword.Started != 0 {
		t.Fatalf("browser delta included controls or a password action: %+v", server.Window.Methods)
	}

	server.Window.Methods.AuthCheckPassword.Started = 1
	server.Window.Methods.AuthCheckPassword.State = "settled"
	if err := validateFixtureAuthProgressBrowserServerDelta(browser, server, manifestDigest, identity.SourceRefs.ArtifactDigest); err == nil {
		t.Fatal("browser observation accepted a checkPassword action before password entry")
	}
}

func TestRealServerFixtureAuthProgressBrowserControlsStayBeforeBaseline(t *testing.T) {
	identity := fixtureAuthProgressBrowserTestIdentity()
	controls := fixtureAuthControlResults{
		AuthSendCode:       fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthSignIn:         fixtureAuthControlOutcome{Result: "password_required", Accounts: 2},
		AccountGetPassword: fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthCheckPassword:  fixtureAuthControlOutcome{Result: "success", Accounts: 2},
		AuthSignUp:         fixtureAuthControlOutcome{Result: "invalid_request", Accounts: 1},
	}
	report := newFixtureAuthProgressTestReport(identity)
	for index := range report.Boundaries {
		methods := &report.Boundaries[index].Server.Methods
		setFixtureAuthProgressMethod(&methods.AuthSendCode, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
		setFixtureAuthProgressMethod(&methods.AuthSignIn, 2, mtproto.RPCProgressResultCounts{PasswordRequired: 2}, 0)
		setFixtureAuthProgressMethod(&methods.AuthCheckPassword, 2, mtproto.RPCProgressResultCounts{Success: 2}, 0)
	}
	report.ShutdownFinal = report.Boundaries[1].Server
	report.ShutdownFinal.Boundary = mtproto.RPCProgressBoundaryShutdownFinal
	report.ShutdownFinal.ShutdownComplete = true
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	server := consumeFixtureAuthProgress(encoded, identity, false, false)
	if !server.Available || server.Window == nil {
		t.Fatalf("prebaseline controls did not settle into a nonzero baseline: %+v", server)
	}
	if controls.AuthSendCode.Accounts != 2 || controls.AuthSignIn.Result != "password_required" ||
		server.Window.ArtifactReady.Server.Methods.AuthSendCode.Started != 2 ||
		server.Window.ArtifactReady.Server.Methods.AuthSignIn.Settled.PasswordRequired != 2 {
		t.Fatalf("accepted auth controls are absent from the prebaseline snapshots: controls=%+v baseline=%+v", controls, server.Window.ArtifactReady.Server.Methods)
	}
	for name, method := range map[string]fixtureAuthProgressMethodDelta{
		"auth.sendCode":      server.Window.Methods.AuthSendCode,
		"auth.signIn":        server.Window.Methods.AuthSignIn,
		"auth.checkPassword": server.Window.Methods.AuthCheckPassword,
	} {
		if method.State != "not_reached" || method.Started != 0 || method.Settled != (mtproto.RPCProgressResultCounts{}) {
			t.Errorf("prebaseline control %s was counted as a browser action: %+v", name, method)
		}
	}
	browser := fixtureAuthProgressBrowserReport{
		Status: "failed", Stage: "alice_context", RunID: identity.RunID,
		HarnessRevision: identity.SourceRefs.HarnessRevision, ServerRevision: identity.SourceRefs.ServerRevision,
		WebRevision: identity.SourceRefs.ArtifactSourceRevision, ArtifactDigest: fixtureAuthProgressBrowserTestManifestDigest(),
	}
	if err := validateFixtureAuthProgressBrowserServerDelta(
		browser, server, fixtureAuthProgressBrowserTestManifestDigest(), identity.SourceRefs.ArtifactDigest,
	); err != nil {
		t.Fatalf("zero browser submissions did not preserve zero postbaseline method deltas: %v", err)
	}
}

func fixtureAuthProgressBrowserTestIdentity() mtproto.RPCProgressIdentity {
	identity := fixtureAuthProgressTestIdentity()
	identity.SourceRefs.RunnerRevision = fixtureAuthProgressRunnerRevision
	identity.SourceRefs.ArtifactSourceRevision = fixtureAuthProgressArtifactSource
	return identity
}

func fixtureAuthProgressBrowserPassingJSON(t *testing.T, identity mtproto.RPCProgressIdentity) []byte {
	t.Helper()
	const aliceNetwork = `{"workerTargets":{"shared_worker":1,"service_worker":1},"allowedWebSockets":1,"attemptsBySource":{"page":1,"shared_worker":0,"service_worker":0,"worker":0},"unexpectedAttemptsBySource":{"page":0,"shared_worker":0,"service_worker":0,"worker":0},"allowedWebSocketsBySource":{"page":1,"shared_worker":0,"service_worker":0,"worker":0}}`
	network := json.RawMessage(`{"unexpectedAttempts":0,"observerErrors":0,"contexts":{"alice":` + string(aliceNetwork) + `}}`)
	workerContext := map[string]any{
		"pageTargets": 1, "sharedWorkerTargets": 1, "blobSharedWorkerTargets": 0,
		"scriptSharedWorkerTargets": 1, "serviceWorkerTargets": 1, "dedicatedWorkerTargets": 0,
		"workerSourceChunks": []string{"mtproto_worker"}, "mtprotoSourceChunk": "mtproto_worker",
		"mtprotoSourceFetch": "ok", "sharedWorkerState": "created", "mtprotoWorker": "mtproto_candidate",
		"workerFailure": "no_failure_observed", "workerFailureValidation": "unvalidated",
		"observerOverflow": 0, "countedEvents": 1, "attachedTargets": 1, "observerErrors": 0,
		"coverageComplete": 1, "discoveryActive": 1, "pageCoverageComplete": 1,
	}
	control := make(map[string]any, len(workerContext)+1)
	maps.Copy(control, workerContext)
	control["expected"] = "pass"
	controls := map[string]any{
		"c1_shared_worker_created": control,
		"c2_missing_module_script": control,
		"c3_no_construction":       control,
		"c4_attach_failure":        control,
		"c4_overflow":              control,
		"c5_attach_budget":         control,
	}
	workerObservation := map[string]any{
		"unexpectedAttempts": 0, "observerErrors": 0, "foreignTargets": 0,
		"contextlessTargets": 0, "appContextlessTargets": 0, "discoveryActive": 1,
		"attachedTargets": 1, "observerOverflow": 0, "countedEvents": 1,
		"contexts": map[string]any{"alice": workerContext}, "controls": controls,
	}
	payload := map[string]any{
		"status": "passed", "mode": "auth_progress_one_submission", "stage": "alice_password_field_ready",
		"usernameSubmissions": 1, "passwordFills": 0, "passwordSubmissions": 0,
		"bobContextCount": 0, "laterScenarioCount": 0, "contextCount": 1,
		"controlContextCount": 6, "screenshotsCaptured": 0,
		"passwordEvidence": []map[string]any{{"stage": "alice_password_field_ready", "count": 1}},
		"network":          network, "workerObservation": workerObservation,
		"runId": identity.RunID, "harnessRevision": identity.SourceRefs.HarnessRevision,
		"serverRevision":       identity.SourceRefs.ServerRevision,
		"webRevision":          identity.SourceRefs.ArtifactSourceRevision,
		"artifactSourceCommit": identity.SourceRefs.ArtifactSourceRevision,
		"artifactDigest":       fixtureAuthProgressBrowserTestManifestDigest(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureAuthProgressBrowserTestManifestDigest() string {
	return "sha256:" + strings.Repeat("9", 64)
}

func TestRealServerFixtureAuthProgressBrowserObservation(t *testing.T) {
	runnerCheckoutPath := os.Getenv("TEAGRAM_FIXTURE_AUTH_PROGRESS_RUNNER_CHECKOUT")
	if runnerCheckoutPath == "" {
		t.Skip("set TEAGRAM_FIXTURE_AUTH_PROGRESS_RUNNER_CHECKOUT to a clean checkout at the accepted runner revision")
	}
	runnerCheckout, err := verifiedFixtureAuthProgressRunnerCheckout(runnerCheckoutPath)
	if err != nil {
		t.Fatalf("verify configured pinned browser runner checkout: %v", err)
	}
	if _, err := osexec.LookPath("rtk"); err != nil {
		t.Fatalf("rtk command is unavailable: %v", err)
	}
	if _, err := osexec.LookPath("pnpm"); err != nil {
		t.Fatalf("pnpm command is unavailable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	var output fixtureAuthProgressBrowserPairOutput
	var pairUnavailableReason string
	var runnerRevision string
	var pairKeyDirectory string
	var pairKeyPath string
	var artifactDirectory string
	var tagged *realFixtureProcess
	var untagged *realFixtureProcess
	var taggedControls *fixtureAuthControlRecord
	var untaggedControls *fixtureAuthControlRecord
	var taggedBrowser *fixtureAuthProgressBrowserReport
	var untaggedBrowser *fixtureAuthProgressBrowserReport
	var taggedReportDirectory string
	var taggedReportPath string

	defer func() {
		cleanupFailed := false
		for _, fixture := range []*realFixtureProcess{untagged, tagged} {
			if fixture != nil && !fixture.stopped {
				if err := fixture.stopByEOF(); err != nil {
					t.Errorf("stop fixture during paired observation cleanup: %v", err)
					cleanupFailed = true
				}
			}
		}

		sharedKeyRemoved := pairKeyPath == "" && pairKeyDirectory == ""
		if pairKeyPath != "" {
			if err := os.Remove(pairKeyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove paired fixture key: %v", err)
				cleanupFailed = true
			}
			if err := os.Remove(pairKeyDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove paired fixture key directory: %v", err)
				cleanupFailed = true
			}
			_, keyErr := os.Lstat(pairKeyPath)
			_, directoryErr := os.Lstat(pairKeyDirectory)
			sharedKeyRemoved = errors.Is(keyErr, os.ErrNotExist) && errors.Is(directoryErr, os.ErrNotExist)
			if !sharedKeyRemoved {
				cleanupFailed = true
			}
		}

		if artifactDirectory != "" {
			if err := os.RemoveAll(artifactDirectory); err != nil {
				t.Errorf("remove frozen caller artifact: %v", err)
				cleanupFailed = true
			}
			if _, err := os.Lstat(artifactDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("frozen caller artifact remains after cleanup: %v", err)
				cleanupFailed = true
			}
		}

		priorFailure := t.Failed()
		for _, fixture := range []*realFixtureProcess{untagged, tagged} {
			if fixture == nil || fixture.ready.RunID == "" {
				continue
			}
			assertProtectedCredentialFilesAbsent(t, fixture.ready)
			assertNamedFixtureResourcesAbsent(t, fixture.ready.RunID)
		}
		if !priorFailure && t.Failed() {
			cleanupFailed = true
		}

		if tagged != nil {
			tagged.finishAuthProgressPair(untagged, sharedKeyRemoved)
		}
		for _, path := range []string{taggedReportPath, taggedReportDirectory} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Errorf("remove paired auth-progress report resource: %v", err)
				cleanupFailed = true
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("paired auth-progress report resource remains after cleanup")
				cleanupFailed = true
			}
		}
		if _, err := verifiedFixtureAuthProgressRunnerCheckout(runnerCheckout); err != nil {
			t.Errorf("pinned browser runner checkout changed during the paired observation")
			cleanupFailed = true
		} else {
			runnerRevision = fixtureAuthProgressRunnerRevision
		}

		output = fixtureAuthProgressBrowserPairResultFor(fixtureAuthProgressBrowserPairResultInput{
			PairUnavailableReason: pairUnavailableReason,
			CleanupFailed:         cleanupFailed,
			RunnerRevision:        runnerRevision,
			Tagged:                tagged,
			Untagged:              untagged,
			TaggedControls:        taggedControls,
			UntaggedControls:      untaggedControls,
			TaggedBrowser:         taggedBrowser,
			UntaggedBrowser:       untaggedBrowser,
		})

		encoded, err := json.Marshal(output)
		if err != nil {
			t.Errorf("encode privacy-safe paired browser observation: %v", err)
			return
		}
		t.Logf("auth-progress-observation=%s", encoded)
	}()

	//nolint:usetesting // The paired private key must remain in /dev/shm tmpfs.
	pairKeyDirectory, err = os.MkdirTemp("/dev/shm", "teagram-auth-progress-pair-")
	if err != nil {
		t.Fatal("create tmpfs pair-key directory")
	}
	pairKeyPath = filepath.Join(pairKeyDirectory, "server-key.pem")
	serverRevision := currentServerRevision(t)
	webRevision := fixtureAuthProgressArtifactSource
	taggedRunID := newRealFixtureRunID(t)
	taggedReportDirectory = t.TempDir()
	taggedReportPath = filepath.Join(taggedReportDirectory, "auth-progress.json")
	tagged, err = startRealServerFixtureWithOptions(ctx, taggedRunID, serverRevision, webRevision, nil, realFixtureStartOptions{
		authProgress:        true,
		prebaselineControls: true,
		authProgressOutput:  taggedReportPath,
		pairKeyOutput:       pairKeyPath,
	})
	if err != nil {
		t.Fatal("start tagged paired fixture")
	}
	tagged.authProgressPairPending = true
	validateRealFixtureReady(t, tagged, taggedRunID, serverRevision, webRevision)
	artifact := buildFixtureWebArtifact(ctx, t, webRevision, tagged.ready)
	artifactDirectory = filepath.Dir(artifact)
	taggedArtifact, err := tagged.attachArtifact(artifact)
	if err != nil {
		t.Fatal("tagged fixture rejected the frozen artifact")
	}
	validateRealFixtureArtifactReady(t, taggedArtifact, tagged.ready, webRevision, nil)
	taggedControls, err = tagged.readAuthControls()
	if err != nil {
		t.Fatal("tagged fixture did not complete its prebaseline auth controls")
	}
	assertFixtureAuthControlIdentity(t, taggedControls, tagged.ready, taggedArtifact)
	if tagged.authProgressIdentity == nil || tagged.authProgressIdentity.SourceRefs.ArtifactDigest != taggedArtifact.ArtifactDigest {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("tagged fixture did not bind its audited artifact before baseline")
	}
	taggedObservation, reason, err := runFixtureAuthProgressOneSubmission(ctx, runnerCheckout, tagged)
	if err != nil {
		pairUnavailableReason = reason
		t.Fatalf("tagged browser observation did not complete: %s", err)
	}
	taggedBrowser = &taggedObservation
	if err := tagged.stopByEOF(); err != nil {
		pairUnavailableReason = "unavailable_teardown_failure"
		t.Fatal("tagged fixture cleanup failed")
	}

	untaggedRunID := newRealFixtureRunID(t)
	untagged, err = startRealServerFixtureWithOptions(ctx, untaggedRunID, serverRevision, webRevision, nil, realFixtureStartOptions{
		authControls:        true,
		prebaselineControls: true,
		pairKeyInput:        pairKeyPath,
	})
	if err != nil {
		t.Fatal("start untagged paired fixture")
	}
	validateRealFixtureReady(t, untagged, untaggedRunID, serverRevision, webRevision)
	if untagged.ready.Fingerprint != tagged.ready.Fingerprint ||
		untagged.ready.PublicKeySHA256 != tagged.ready.PublicKeySHA256 ||
		untagged.ready.PublicKeyPEM != tagged.ready.PublicKeyPEM {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("paired fixture legs do not share the generated target key")
	}
	if len(untagged.ready.Credentials) != 2 || len(tagged.ready.Credentials) != 2 ||
		untagged.ready.Credentials[0].Username == tagged.ready.Credentials[0].Username ||
		untagged.ready.Credentials[1].Username == tagged.ready.Credentials[1].Username ||
		untagged.ready.Credentials[0].PasswordFile == tagged.ready.Credentials[0].PasswordFile ||
		untagged.ready.Credentials[1].PasswordFile == tagged.ready.Credentials[1].PasswordFile {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("paired fixture legs reused synthetic account credentials")
	}
	untaggedArtifact, err := untagged.attachArtifact(artifact)
	if err != nil {
		t.Fatal("untagged fixture rejected the frozen artifact")
	}
	validateRealFixtureArtifactReady(t, untaggedArtifact, untagged.ready, webRevision, nil)
	if untaggedArtifact.ArtifactDigest != taggedArtifact.ArtifactDigest ||
		untaggedArtifact.ManifestArtifactDigest != taggedArtifact.ManifestArtifactDigest ||
		untaggedArtifact.FrozenArtifactDigest != taggedArtifact.FrozenArtifactDigest || untagged.authProgressIdentity == nil ||
		untagged.authProgressIdentity.SourceRefs.ArtifactDigest != taggedArtifact.ArtifactDigest {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("paired fixture legs did not bind the same frozen artifact digest")
	}
	untaggedControls, err = untagged.readAuthControls()
	if err != nil {
		t.Fatal("untagged fixture did not complete its prebaseline auth controls")
	}
	assertFixtureAuthControlIdentity(t, untaggedControls, untagged.ready, untaggedArtifact)
	if tagged.authProgressIdentity.SourceRefs != untagged.authProgressIdentity.SourceRefs {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("paired fixture source references differ")
	}
	if !reflect.DeepEqual(taggedControls.Results, untaggedControls.Results) {
		pairUnavailableReason = "unavailable_malformed_output"
		t.Fatal("tagged and untagged auth-control outcomes differ")
	}
	untaggedObservation, reason, err := runFixtureAuthProgressOneSubmission(ctx, runnerCheckout, untagged)
	if err != nil {
		pairUnavailableReason = reason
		t.Fatalf("untagged browser observation did not complete: %s", err)
	}
	untaggedBrowser = &untaggedObservation
	if err := untagged.stopByEOF(); err != nil {
		pairUnavailableReason = "unavailable_teardown_failure"
		t.Fatal("untagged fixture cleanup failed")
	}
}

func verifiedFixtureAuthProgressRunnerCheckout(path string) (string, error) {
	if path == "" {
		return "", errors.New("runner checkout path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("runner checkout path is invalid")
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("runner checkout is unavailable")
	}
	info, err := os.Stat(absolute) //nolint:gosec // The configured runner checkout is resolved above and only read.
	if err != nil || !info.IsDir() {
		return "", errors.New("runner checkout is not a directory")
	}
	command := fixtureCommand(context.Background(), "git", "-C", absolute, "rev-parse", "HEAD")
	head, err := command.Output()
	if err != nil || strings.TrimSpace(string(head)) != fixtureAuthProgressRunnerRevision {
		return "", errors.New("runner checkout does not match the accepted immutable revision")
	}
	command = fixtureCommand(context.Background(), "git", "-C", absolute, "status", "--porcelain", "--untracked-files=all")
	dirty, err := command.Output()
	if err != nil || len(dirty) != 0 {
		return "", errors.New("runner checkout is not clean")
	}
	return absolute, nil
}

func fixtureAuthProgressBrowserRunnerReadinessLine(ready realFixtureArtifactReady, identity mtproto.RPCProgressIdentity) ([]byte, error) {
	if identity.Validate() != nil || ready.Event != "artifact-ready" || ready.Status != "ready" ||
		ready.RunID != identity.RunID || ready.HarnessRevision != identity.SourceRefs.HarnessRevision ||
		ready.ServerRevision != identity.SourceRefs.ServerRevision || ready.WebRevision != identity.SourceRefs.ArtifactSourceRevision ||
		ready.ArtifactDigest != identity.SourceRefs.ArtifactDigest || ready.ArtifactDigest != ready.FrozenArtifactDigest ||
		ready.LegDigestBefore != ready.FrozenArtifactDigest ||
		!regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(ready.ManifestArtifactDigest) {
		return nil, errors.New("fixture readiness does not match the bound full artifact identity")
	}
	runnerReady := ready
	runnerReady.ArtifactDigest = ready.ManifestArtifactDigest
	line, err := json.Marshal(runnerReady)
	if err != nil {
		return nil, errors.New("encode pinned-runner readiness view")
	}
	return line, nil
}

func runFixtureAuthProgressOneSubmission(ctx context.Context, runnerCheckout string, fixture *realFixtureProcess) (report fixtureAuthProgressBrowserReport, unavailableReason string, resultErr error) {
	var empty fixtureAuthProgressBrowserReport
	if fixture == nil || fixture.authProgressIdentity == nil || fixture.artifactReady == nil || fixture.artifactReadyLine == "" {
		return empty, "unavailable_incomplete", errors.New("fixture is not bound and artifact-ready")
	}
	if _, err := verifiedFixtureAuthProgressRunnerCheckout(runnerCheckout); err != nil {
		return empty, "unavailable_malformed_output", err
	}
	identity := *fixture.authProgressIdentity
	runnerArtifactReadyLine, err := fixtureAuthProgressBrowserRunnerReadinessLine(*fixture.artifactReady, identity)
	if err != nil {
		return empty, "unavailable_malformed_output", err
	}
	readiness, err := os.CreateTemp("/dev/shm", "teagram-auth-progress-readiness-")
	if err != nil {
		return empty, "unavailable_incomplete", errors.New("create tmpfs runner readiness file")
	}
	readinessPath := readiness.Name()
	readinessClosed := false
	readinessCleanup := func() error {
		var closeErr error
		if !readinessClosed {
			closeErr = readiness.Close()
			readinessClosed = true
		}
		removeErr := os.Remove(readinessPath)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		_, statErr := os.Lstat(readinessPath)
		var failures []string
		if closeErr != nil {
			failures = append(failures, "close")
		}
		if removeErr != nil {
			failures = append(failures, "remove")
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			failures = append(failures, "verify")
		}
		if len(failures) == 0 {
			return nil
		}
		return errors.New(strings.Join(failures, ","))
	}
	defer func() {
		if err := readinessCleanup(); err != nil {
			report = empty
			unavailableReason = "unavailable_teardown_failure"
			resultErr = errors.New("runner readiness cleanup failed: " + err.Error())
		}
	}()
	if err := readiness.Chmod(0o600); err != nil {
		return empty, "unavailable_incomplete", errors.New("protect tmpfs runner readiness file")
	}
	if _, err := io.WriteString(readiness, fixture.readyLine+string(runnerArtifactReadyLine)+"\n"); err != nil {
		return empty, "unavailable_incomplete", errors.New("write fixture-bound runner readiness")
	}
	if err := readiness.Close(); err != nil {
		readinessClosed = true
		return empty, "unavailable_incomplete", errors.New("close fixture-bound runner readiness")
	}
	readinessClosed = true
	if info, err := os.Lstat(readinessPath); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return empty, "unavailable_incomplete", errors.New("runner readiness file permissions are invalid")
	}

	command := fixtureCommand(ctx, "rtk", "pnpm", "--silent", "run", "test:real-client", "--",
		"--readiness-file", readinessPath,
		"--harness-revision", identity.SourceRefs.HarnessRevision,
		"--server-revision", identity.SourceRefs.ServerRevision,
		"--web-revision", identity.SourceRefs.ArtifactSourceRevision,
		"--run-id", identity.RunID,
		"--auth-progress-one-submission",
	)
	command.Dir = runnerCheckout
	command.Env = fixtureRunnerEnvironment()
	stdout := &fixtureBoundedOutput{limit: 1 << 20}
	stderr := &fixtureBoundedOutput{limit: 1 << 20}
	command.Stdout = stdout
	command.Stderr = stderr
	runErr := command.Run()
	if stdout.exceeded || stderr.exceeded {
		return empty, "unavailable_malformed_output", errors.New("runner output exceeded its fixed bound")
	}
	report, err = decodeFixtureAuthProgressBrowserReport(stdout.Bytes(), identity, fixture.artifactReady.ManifestArtifactDigest)
	if err != nil {
		return empty, "unavailable_malformed_output", errors.New("runner output did not match the closed one-submission contract: " + err.Error() + "; " + fixtureAuthProgressBrowserSafeDiagnostic(stdout.Bytes()) + "; " + fixtureAuthProgressBrowserSafeRunnerError(stderr.Bytes()))
	}
	if _, err := verifiedFixtureAuthProgressRunnerCheckout(runnerCheckout); err != nil {
		return empty, "unavailable_malformed_output", errors.New("runner checkout changed while the browser operation ran")
	}
	if report.Status == "failed" && report.Stage == "cleanup" {
		return empty, "unavailable_teardown_failure", errors.New("browser runner reported a cleanup failure")
	}
	if runErr == nil && report.Status != "passed" {
		return empty, "unavailable_malformed_output", errors.New("runner exit status disagrees with its validated report")
	}
	if runErr != nil {
		var exitError *osexec.ExitError
		if !errors.As(runErr, &exitError) || exitError.ExitCode() != 1 || report.Status != "failed" {
			return empty, "unavailable_incomplete", errors.New("runner command did not complete with an accepted status")
		}
	}
	return report, "", nil
}

func fixtureAuthProgressBrowserSafeDiagnostic(data []byte) string {
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	for _, line := range slices.Backward(lines) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var summary struct {
			Status              string `json:"status"`
			Mode                string `json:"mode"`
			Stage               string `json:"stage"`
			UsernameSubmissions int64  `json:"usernameSubmissions"`
		}
		if json.Unmarshal(line, &summary) != nil || (summary.Status != "passed" && summary.Status != "failed") ||
			summary.Mode != "auth_progress_one_submission" {
			return "no validated report header on stdout"
		}
		if _, ok := fixtureAuthProgressBrowserStages[summary.Stage]; !ok || summary.UsernameSubmissions < 0 || summary.UsernameSubmissions > 1 {
			return "report header contains an invalid stage or submission count"
		}
		return "report status=" + summary.Status + " stage=" + summary.Stage +
			" username_submissions=" + strconv.FormatInt(summary.UsernameSubmissions, 10)
	}
	return "stdout is empty"
}

func fixtureAuthProgressBrowserSafeRunnerError(data []byte) string {
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "isolated auth progress runner failed":
			return "browser operation did not return a report"
		case line == "isolated auth progress runner returned an invalid report":
			return "browser operation returned an invalid report"
		case line == "fixture browser isolation does not match the accepted contract":
			return "fixture browser isolation validation failed"
		case line == "fixture TLS SPKI pin is invalid" || line == "fixture TLS front address is invalid":
			return "fixture TLS validation failed"
		case line == "fixture browser has a default network route":
			return "fixture browser network validation failed"
		case line == "screenshot cleanup failed" || line == "browser runner cleanup failed":
			return "browser runner cleanup failed"
		case strings.Contains(line, "fixture readiness"):
			return "fixture readiness validation failed"
		case strings.Contains(line, "synthetic credential") || strings.Contains(line, "synthetic password"):
			return "fixture credential validation failed"
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return "runner stderr is empty"
	}
	return "runner stderr has no recognized fixed failure category"
}

type fixtureBoundedOutput struct {
	data     []byte
	limit    int
	exceeded bool
}

func (w *fixtureBoundedOutput) Write(data []byte) (int, error) {
	remaining := w.limit - len(w.data)
	if remaining <= 0 {
		w.exceeded = true
		return len(data), nil
	}
	if len(data) > remaining {
		w.data = append(w.data, data[:remaining]...)
		w.exceeded = true
		return len(data), nil
	}
	w.data = append(w.data, data...)
	return len(data), nil
}

func (w *fixtureBoundedOutput) Bytes() []byte {
	return w.data
}

func fixtureRunnerEnvironment() []string {
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=/tmp",
		"CI=true",
	}
	if dockerHost := os.Getenv("DOCKER_HOST"); dockerHost != "" {
		environment = append(environment, "DOCKER_HOST="+dockerHost)
	}
	environment = append(environment,
		"NPM_CONFIG_USERCONFIG=/dev/null",
		"NPM_CONFIG_GLOBALCONFIG=/dev/null",
	)
	return environment
}

func fixtureAuthProgressBrowserSummaryFor(report fixtureAuthProgressBrowserReport) *fixtureAuthProgressBrowserSummary {
	summary := &fixtureAuthProgressBrowserSummary{
		ManifestArtifactDigest: report.ArtifactDigest,
		Status:                 report.Status,
		Stage:                  report.Stage,
		UsernameSubmissions:    report.UsernameSubmissions,
		PasswordFills:          report.PasswordFills,
		PasswordSubmissions:    report.PasswordSubmissions,
		ContextCount:           report.ContextCount,
		ControlContextCount:    report.ControlContextCount,
		UnexpectedAttempts:     report.Network.UnexpectedAttempts,
		ObserverErrors:         report.Network.ObserverErrors,
	}
	if len(report.PasswordEvidence) == 1 {
		count := report.PasswordEvidence[0].Count
		summary.PasswordFieldCount = &count
	}
	if alice, ok := report.Network.Contexts["alice"]; ok {
		allowed := alice.AllowedWebSockets
		summary.AliceAllowedWebSockets = &allowed
	}
	return summary
}

func fixtureAuthProgressBrowserPairResultFor(input fixtureAuthProgressBrowserPairResultInput) fixtureAuthProgressBrowserPairOutput {
	output := fixtureAuthProgressBrowserPairOutput{
		SchemaVersion:     fixtureAuthProgressContractVersion,
		Status:            "unavailable",
		UnavailableReason: "unavailable_incomplete",
		RunnerRevision:    input.RunnerRevision,
	}
	pairReason := input.PairUnavailableReason
	if input.Tagged != nil && input.Tagged.authProgressIdentity != nil {
		identity := *input.Tagged.authProgressIdentity
		output.TaggedIdentity = &identity
	}
	if input.Untagged != nil && input.Untagged.authProgressIdentity != nil {
		identity := *input.Untagged.authProgressIdentity
		output.UntaggedIdentity = &identity
	}
	if input.TaggedControls != nil && input.UntaggedControls != nil {
		parity := reflect.DeepEqual(input.TaggedControls.Results, input.UntaggedControls.Results)
		output.ControlParity = &parity
		if parity {
			controls := input.TaggedControls.Results
			output.ControlOutcomes = &controls
		} else if pairReason == "" {
			pairReason = "unavailable_malformed_output"
		}
	}

	if input.Tagged != nil && input.Tagged.authProgressResult != nil && !input.Tagged.authProgressResult.Available && pairReason == "" {
		pairReason = input.Tagged.authProgressResult.UnavailableReason
	}
	if input.CleanupFailed && pairReason == "" {
		pairReason = "unavailable_teardown_failure"
	}
	if pairReason == "" && input.Tagged != nil && input.Tagged.authProgressResult != nil &&
		input.Tagged.authProgressResult.Available && input.Tagged.authProgressResult.Window != nil &&
		input.Tagged.artifactReady != nil && input.Tagged.authProgressIdentity != nil &&
		input.Untagged != nil && input.TaggedControls != nil && input.UntaggedControls != nil &&
		input.TaggedBrowser != nil && input.UntaggedBrowser != nil && output.ControlParity != nil && *output.ControlParity {
		if err := validateFixtureAuthProgressBrowserServerDelta(
			*input.TaggedBrowser, *input.Tagged.authProgressResult,
			input.Tagged.artifactReady.ManifestArtifactDigest, input.Tagged.authProgressIdentity.SourceRefs.ArtifactDigest,
		); err != nil {
			pairReason = "unavailable_malformed_output"
		}
	}

	if pairReason == "" && !input.CleanupFailed && input.Tagged != nil && input.Untagged != nil &&
		input.Tagged.authProgressResult != nil && input.Tagged.authProgressResult.Available && input.Tagged.authProgressResult.Window != nil &&
		input.Tagged.artifactReady != nil && input.Tagged.authProgressIdentity != nil &&
		input.TaggedBrowser != nil && input.UntaggedBrowser != nil && output.ControlParity != nil && *output.ControlParity {
		output.Status = "available"
		output.UnavailableReason = ""
		output.ManifestArtifactDigest = input.Tagged.artifactReady.ManifestArtifactDigest
		output.FrozenArtifactDigest = input.Tagged.artifactReady.FrozenArtifactDigest
		output.TaggedBrowser = fixtureAuthProgressBrowserSummaryFor(*input.TaggedBrowser)
		output.UntaggedBrowser = fixtureAuthProgressBrowserSummaryFor(*input.UntaggedBrowser)
		window := *input.Tagged.authProgressResult.Window
		output.Server = &window
		return output
	}

	output.UnavailableReason = fixtureAuthProgressBrowserPairUnavailableReason(pairReason, input.CleanupFailed, input.Tagged)
	output.TaggedBrowser = nil
	output.UntaggedBrowser = nil
	output.Server = nil
	return output
}

func fixtureAuthProgressBrowserPairUnavailableReason(pairReason string, cleanupFailed bool, tagged *realFixtureProcess) string {
	reasons := make(map[string]bool)
	if pairReason != "" {
		reasons[pairReason] = true
	}
	if cleanupFailed {
		reasons["unavailable_teardown_failure"] = true
	}
	if tagged != nil && tagged.authProgressResult != nil && !tagged.authProgressResult.Available {
		reasons[tagged.authProgressResult.UnavailableReason] = true
	}
	if len(reasons) == 1 {
		for reason := range reasons {
			return reason
		}
	}
	if preferred := fixtureAuthProgressPreferredReason(reasons); preferred != "" {
		return preferred
	}
	return "unavailable_incomplete"
}
