//go:build performance

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

func TestGetDialogsProfilerIdentifiesRateLimit(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{FloodWaitError(17), "first-page flood wait 17s"},
		{errors.New("private detail"), "first-page rate limit error"},
		{tgerr.New(400, "PRIVATE"), "first-page rate limit error"},
	} {
		if got := dialogsRateLimitStage(tc.err); got != tc.want {
			t.Errorf("stage = %q, want %q", got, tc.want)
		}
	}
}

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
