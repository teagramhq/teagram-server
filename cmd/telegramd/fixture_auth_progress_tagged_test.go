//go:build fixtureauthprogress

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func TestFixtureAuthProgressTaggedServeRequiresConfiguration(t *testing.T) {
	for _, name := range []string{
		fixtureAuthProgressPathEnv,
		fixtureAuthProgressRunIDEnv,
		fixtureAuthProgressRunnerEnv,
		fixtureAuthProgressArtifactSourceEnv,
		fixtureAuthProgressHarnessEnv,
	} {
		t.Setenv(name, "")
	}
	if err := validateServeEnvironment(); err == nil || !strings.Contains(err.Error(), fixtureAuthProgressPathEnv) {
		t.Fatalf("tagged serve validation error = %v, want required output path", err)
	}
}

func TestFixtureAuthProgressPathRejectsOutsideAndSymlinkTargets(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "progress.json")
	if err := os.WriteFile(outsideFile, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write outside sentinel: %v", err)
	}

	if _, err := validateFixtureAuthProgressPath(outsideFile, root); err == nil {
		t.Fatal("path outside fixture root was accepted")
	}
	link := filepath.Join(root, "progress-link.json")
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := validateFixtureAuthProgressPath(link, root); err == nil {
		t.Fatal("symlink output target was accepted")
	}
	if _, err := validateFixtureAuthProgressPath(filepath.Join(root, "..", "outside", "progress.json"), root); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if got, err := os.ReadFile(outsideFile); err != nil || string(got) != "keep" {
		t.Fatalf("outside target changed: content=%q err=%v", got, err)
	}
}

func TestFixtureAuthProgressReportContainsOnlyClosedSnapshotData(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{Exporter: mtproto.RPCSpanExporterFunc(func(context.Context, mtproto.RPCSpan) error { return nil })})
	tracer.Record(mtproto.RPCSpan{Method: "hostile.method.secret", Result: mtproto.RPCResultClass("secret-error"), Duration: 42})
	writer, err := newFixtureAuthProgressWriter(config, tracer)
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if err := writer.bindIdentity(fixtureAuthProgressTestIdentity()); err != nil {
		t.Fatalf("bind artifact identity: %v", err)
	}
	writeFixtureAuthProgressFrontCounter(t, root, 7)
	if err := writer.recordNextBoundary(); err != nil {
		t.Fatalf("record artifact-ready: %v", err)
	}
	writeFixtureAuthProgressFrontCounter(t, root, 10)
	if err := writer.recordNextBoundary(); err != nil {
		t.Fatalf("record runner-exit: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close progress writer: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat report: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("report permissions = %04o, want 0600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	for _, forbidden := range []string{"hostile.method.secret", "secret-error", "progress.json", root, "42"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("durable report contains forbidden data %q: %s", forbidden, data)
		}
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if got, want := sortedJSONKeys(report), []string{"binding_rejected", "boundaries", "identity", "incomplete", "schema_version", "shutdown_final"}; !equalJSONStrings(got, want) {
		t.Fatalf("report keys = %v, want %v", got, want)
	}
	var boundaries []fixtureAuthProgressBoundary
	if err := json.Unmarshal(report["boundaries"], &boundaries); err != nil {
		t.Fatalf("decode boundaries: %v", err)
	}
	if len(boundaries) != 2 {
		t.Fatalf("boundary count = %d, want two", len(boundaries))
	}
	if boundaries[0].Server.Boundary != mtproto.RPCProgressBoundaryArtifactReady ||
		boundaries[1].Server.Boundary != mtproto.RPCProgressBoundaryRunnerExit {
		t.Fatalf("snapshot boundaries = %q, %q", boundaries[0].Server.Boundary, boundaries[1].Server.Boundary)
	}
	if boundaries[0].Front.WSAccepted != 7 || boundaries[1].Front.WSAccepted != 10 || boundaries[0].Front.Source != fixtureAuthProgressFrontSource {
		t.Fatalf("front boundary observations = %+v", boundaries)
	}
	var shutdownFinal mtproto.RPCProgressSnapshot
	if err := json.Unmarshal(report["shutdown_final"], &shutdownFinal); err != nil {
		t.Fatalf("decode shutdown snapshot: %v", err)
	}
	if shutdownFinal.Boundary != mtproto.RPCProgressBoundaryShutdownFinal {
		t.Fatalf("shutdown boundary = %q", shutdownFinal.Boundary)
	}
	if err := shutdownFinal.Validate(); err != nil {
		t.Fatalf("valid final snapshot rejected: %v", err)
	}
}

func TestFixtureAuthProgressWriterMarksMissingBoundaryIncomplete(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{Exporter: mtproto.RPCSpanExporterFunc(func(context.Context, mtproto.RPCSpan) error { return nil })})
	writer, err := newFixtureAuthProgressWriter(config, tracer)
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if err := writer.bindIdentity(fixtureAuthProgressTestIdentity()); err != nil {
		t.Fatalf("bind artifact identity: %v", err)
	}
	writeFixtureAuthProgressFrontCounter(t, root, 7)
	if err := writer.recordNextBoundary(); err != nil {
		t.Fatalf("record artifact-ready: %v", err)
	}
	if err := writer.Close(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("close error = %v, want incomplete output", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report fixtureAuthProgressReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Incomplete || len(report.Boundaries) != 1 || report.ShutdownFinal.Boundary != mtproto.RPCProgressBoundaryShutdownFinal {
		t.Fatalf("report incomplete=%v boundaries=%d final=%q, want one boundary and final", report.Incomplete, len(report.Boundaries), report.ShutdownFinal.Boundary)
	}
}

func TestFixtureAuthProgressWriterRejectsInvalidFrontCounter(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{Exporter: mtproto.RPCSpanExporterFunc(func(context.Context, mtproto.RPCSpan) error { return nil })})
	writer, err := newFixtureAuthProgressWriter(config, tracer)
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	if err := writer.bindIdentity(fixtureAuthProgressTestIdentity()); err != nil {
		t.Fatalf("bind artifact identity: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, fixtureAuthProgressFrontCounter), []byte("01\n"), 0o600); err != nil {
		t.Fatalf("write malformed front counter: %v", err)
	}
	if err := writer.recordNextBoundary(); err == nil {
		t.Fatal("noncanonical front counter was accepted")
	}
	if err := writer.Close(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("close error = %v, want incomplete output", err)
	}
}

func TestFixtureAuthProgressWriterRejectsMissingAndLateBinding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	writer, err := newFixtureAuthProgressWriter(config, newFixtureAuthProgressTestTracer())
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	writeFixtureAuthProgressFrontCounter(t, root, 7)
	if err := writer.recordNextBoundary(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("unbound baseline error = %v, want incomplete", err)
	}
	if err := writer.bindIdentity(fixtureAuthProgressTestIdentity()); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("late binding error = %v, want incomplete", err)
	}
	if err := writer.Close(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("close error = %v, want incomplete", err)
	}
}

func TestFixtureAuthProgressWriterRejectsPostBaselineRebind(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	writer, err := newFixtureAuthProgressWriter(config, newFixtureAuthProgressTestTracer())
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	identity := fixtureAuthProgressTestIdentity()
	if err := writer.bindIdentity(identity); err != nil {
		t.Fatalf("bind artifact identity: %v", err)
	}
	writeFixtureAuthProgressFrontCounter(t, root, 7)
	if err := writer.recordNextBoundary(); err != nil {
		t.Fatalf("record baseline: %v", err)
	}
	identity.SourceRefs.ArtifactDigest = "sha256:" + strings.Repeat("9", 64)
	if err := writer.bindIdentity(identity); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("post-baseline rebind error = %v, want incomplete", err)
	}
	if err := writer.Close(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("close error = %v, want incomplete", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report fixtureAuthProgressReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Incomplete || report.Identity != fixtureAuthProgressTestIdentity() {
		t.Fatalf("rebind changed or validated report identity: %+v", report)
	}
}

func TestFixtureAuthProgressWriterMarksReferenceMismatchRejected(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "progress.json")
	config, err := newFixtureAuthProgressConfig(fixtureAuthProgressValues(path), root, strings.Repeat("f", 40))
	if err != nil {
		t.Fatalf("fixture config: %v", err)
	}
	writer, err := newFixtureAuthProgressWriter(config, newFixtureAuthProgressTestTracer())
	if err != nil {
		t.Fatalf("create writer: %v", err)
	}
	identity := fixtureAuthProgressTestIdentity()
	identity.SourceRefs.RunnerRevision = strings.Repeat("9", 40)
	if err := writer.bindIdentity(identity); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("reference mismatch error = %v, want incomplete writer state", err)
	}
	if err := writer.Close(); !errors.Is(err, ErrFixtureAuthProgressIncomplete) {
		t.Fatalf("close error = %v, want incomplete output", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	var report fixtureAuthProgressReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if !report.Incomplete || !report.BindingRejected || report.Identity != (mtproto.RPCProgressIdentity{}) {
		t.Fatalf("reference mismatch report = %+v, want rejected incomplete binding", report)
	}
}

func writeFixtureAuthProgressFrontCounter(t *testing.T, root string, count int64) {
	t.Helper()
	data := []byte(strconv.FormatInt(count, 10) + "\n")
	if err := os.WriteFile(filepath.Join(root, fixtureAuthProgressFrontCounter), data, 0o600); err != nil {
		t.Fatalf("write front counter: %v", err)
	}
}

func fixtureAuthProgressValues(path string) map[string]string {
	return map[string]string{
		fixtureAuthProgressPathEnv:           path,
		fixtureAuthProgressRunIDEnv:          strings.Repeat("a", 32),
		fixtureAuthProgressRunnerEnv:         strings.Repeat("b", 40),
		fixtureAuthProgressArtifactSourceEnv: strings.Repeat("c", 40),
		fixtureAuthProgressHarnessEnv:        strings.Repeat("e", 40),
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

func newFixtureAuthProgressTestTracer() *mtproto.RPCTracer {
	return mtproto.NewRPCTracer(mtproto.RPCTracerConfig{
		Exporter: mtproto.RPCSpanExporterFunc(func(context.Context, mtproto.RPCSpan) error { return nil }),
	})
}

func sortedJSONKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func equalJSONStrings(a, b []string) bool {
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
