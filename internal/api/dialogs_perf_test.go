//go:build performance

package api

import (
	"testing"
	"time"
)

func TestGetDialogsProfilerEmitsCompletedStages(t *testing.T) {
	var stages []string
	profile := newGetDialogsProfiler(func(name string, _ time.Duration) {
		stages = append(stages, name)
	})
	profile.mark("request validation")
	profile.mark("dialog page query")
	profile.finish()

	want := []string{"request validation", "dialog page query", "handler completion", "server handler total"}
	if len(stages) != len(want) {
		t.Fatalf("emitted %d stages, want %d: %v", len(stages), len(want), stages)
	}
	for i := range want {
		if stages[i] != want[i] {
			t.Errorf("stage %d = %q, want %q", i, stages[i], want[i])
		}
	}
}
