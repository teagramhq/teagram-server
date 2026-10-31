package botapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseGetUpdatesClampsLimitAndTimeout(t *testing.T) {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/bot123:token/getUpdates", strings.NewReader(`{"limit":0,"timeout":500}`))
	request.Header.Set("Content-Type", "application/json")
	params, err := parseGetUpdatesRequest(request, []byte(`{"limit":0,"timeout":500}`))
	if err != nil {
		t.Fatalf("parse getUpdates: %v", err)
	}
	if params.limit != 1 || params.timeout != maxGetUpdatesTimeout {
		t.Fatalf("clamped params = limit %d timeout %d, want 1 and %d", params.limit, params.timeout, maxGetUpdatesTimeout)
	}

	request = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/bot123:token/getUpdates?limit=1000&timeout=1", nil)
	params, err = parseGetUpdatesRequest(request, nil)
	if err != nil {
		t.Fatalf("parse upper bounds: %v", err)
	}
	if params.limit != maxGetUpdatesLimit || params.timeout != 1 {
		t.Fatalf("upper params = limit %d timeout %d, want %d and 1", params.limit, params.timeout, maxGetUpdatesLimit)
	}
}

func TestParseGetUpdatesRejectsUnsupportedUpdateKindsAndDuplicateOffsets(t *testing.T) {
	for _, target := range []string{
		"/bot123:token/getUpdates?offset=1&offset=2",
		"/bot123:token/getUpdates?allowed_updates=%5B%22edited_message%22%5D",
	} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		if _, err := parseGetUpdatesRequest(request, nil); err == nil {
			t.Errorf("parse %q succeeded, want bad request", target)
		}
	}
}

func TestUpdatePollRegistryCapsOpenPollsAndReplacesSameBot(t *testing.T) {
	limiter := newAuthLimiter()
	polls := make(map[int64]*updatePoll, maxOpenGetUpdates)
	for botID := int64(1); botID <= maxOpenGetUpdates; botID++ {
		poll, ok := limiter.beginUpdatePoll(context.Background(), botID)
		if !ok {
			t.Fatalf("begin poll for bot %d failed below cap", botID)
		}
		polls[botID] = poll
	}
	if _, ok := limiter.beginUpdatePoll(context.Background(), maxOpenGetUpdates+1); ok {
		t.Fatal("poll above the open-poll cap was accepted")
	}
	replacement, ok := limiter.beginUpdatePoll(context.Background(), 1)
	if !ok {
		t.Fatal("second poll for an existing bot was rejected instead of replacing the first")
	}
	limiter.mu.Lock()
	activePolls := len(limiter.polls)
	limiter.mu.Unlock()
	if !polls[1].wasSuperseded() || activePolls != maxOpenGetUpdates {
		t.Fatalf("replacement left old poll active or changed cap count: active %d", activePolls)
	}
	polls[1] = replacement
	for botID, poll := range polls {
		limiter.endUpdatePoll(botID, poll)
	}
	limiter.mu.Lock()
	got := len(limiter.polls)
	limiter.mu.Unlock()
	if got != 0 {
		t.Fatalf("active poll count after cleanup = %d, want 0", got)
	}
}
