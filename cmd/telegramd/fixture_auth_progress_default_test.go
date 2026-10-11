//go:build !fixtureauthprogress

package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixtureAuthProgressUntaggedServeRejectsSettingBeforeStartup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "must-remain-absent.json")
	for _, value := range []string{"", target} {
		t.Setenv("TG_FIXTURE_AUTH_PROGRESS", value)
		err := runCommand([]string{"serve"}, slog.New(slog.DiscardHandler), &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "TG_FIXTURE_AUTH_PROGRESS") {
			t.Fatalf("serve error for supplied value %q = %v, want fixture-only rejection", value, err)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("rejected fixture path was touched: %v", err)
		}
	}
}

func TestFixtureAuthProgressUntaggedBinaryExcludesFixtureSymbols(t *testing.T) {
	const binary = ".fixture-auth-progress-test"
	if _, err := os.Lstat(binary); !os.IsNotExist(err) {
		t.Fatalf("test binary path already exists: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Remove(binary); err != nil && !os.IsNotExist(err) {
			t.Errorf("remove test binary: %v", err)
		}
	})
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build untagged server: %v\n%s", err, output)
	}
	nm := exec.CommandContext(t.Context(), "go", "tool", "nm", binary)
	output, err := nm.Output()
	if err != nil {
		t.Fatalf("inspect untagged server symbols: %v", err)
	}
	for _, forbidden := range []string{
		"fixtureAuthProgressWriter",
		"fixtureAuthProgressConfigFromEnv",
		"newFixtureAuthProgressWriter",
		"fixtureRPCSpanExporter",
	} {
		if bytes.Contains(output, []byte(forbidden)) {
			t.Fatalf("untagged binary contains fixture-only symbol %q", forbidden)
		}
	}
}
