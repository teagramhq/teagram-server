//go:build fixtureauthprogress

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
		fixtureAuthProgressArtifactDigestEnv,
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
	if err := writer.recordNextBoundary(); err != nil {
		t.Fatalf("record artifact-ready: %v", err)
	}
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
	if got, want := sortedJSONKeys(report), []string{"identity", "incomplete", "schema_version", "snapshots"}; !equalJSONStrings(got, want) {
		t.Fatalf("report keys = %v, want %v", got, want)
	}
	var snapshots []mtproto.RPCProgressSnapshot
	if err := json.Unmarshal(report["snapshots"], &snapshots); err != nil {
		t.Fatalf("decode snapshots: %v", err)
	}
	if len(snapshots) != 3 {
		t.Fatalf("snapshot count = %d, want two boundaries and shutdown", len(snapshots))
	}
	if snapshots[0].Boundary != mtproto.RPCProgressBoundaryArtifactReady ||
		snapshots[1].Boundary != mtproto.RPCProgressBoundaryRunnerExit ||
		snapshots[2].Boundary != mtproto.RPCProgressBoundaryShutdownFinal {
		t.Fatalf("snapshot boundaries = %q, %q, %q", snapshots[0].Boundary, snapshots[1].Boundary, snapshots[2].Boundary)
	}
	if err := snapshots[2].Validate(); err != nil {
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
	if !report.Incomplete || len(report.Snapshots) != 2 {
		t.Fatalf("report incomplete=%v snapshots=%d, want true and artifact-ready plus final", report.Incomplete, len(report.Snapshots))
	}
}

func fixtureAuthProgressValues(path string) map[string]string {
	return map[string]string{
		fixtureAuthProgressPathEnv:           path,
		fixtureAuthProgressRunIDEnv:          strings.Repeat("a", 32),
		fixtureAuthProgressRunnerEnv:         strings.Repeat("b", 40),
		fixtureAuthProgressArtifactSourceEnv: strings.Repeat("c", 40),
		fixtureAuthProgressArtifactDigestEnv: "sha256:" + strings.Repeat("d", 64),
		fixtureAuthProgressHarnessEnv:        strings.Repeat("e", 40),
	}
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
