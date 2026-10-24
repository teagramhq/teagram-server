package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRequiresWriteQuiescenceChoiceBeforeLoadingConfiguration(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"--progress", t.TempDir() + "/progress.jsonl"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--writes stopped or --writes drained") {
		t.Fatalf("run error = %v, want required write-quiescence choice", err)
	}
}

func TestRunRejectsUnknownWriteQuiescenceChoiceBeforeLoadingConfiguration(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run([]string{"--progress", t.TempDir() + "/progress.jsonl", "--writes", "running"}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "--writes must be stopped or drained") {
		t.Fatalf("run error = %v, want invalid write-quiescence choice", err)
	}
}
