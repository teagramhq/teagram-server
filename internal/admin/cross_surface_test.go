package admin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/pgtest"
	"github.com/teagramhq/teagram-server/internal/store"
)

type pushSurfacePayload struct {
	PushLatencyP50                 float64            `json:"push_latency_p50_ms"`
	PushLatencyP50Overflow         bool               `json:"push_latency_p50_overflow"`
	PushLatencyP95                 float64            `json:"push_latency_p95_ms"`
	PushLatencyP95Overflow         bool               `json:"push_latency_p95_overflow"`
	PushLatencySampleCount         int64              `json:"push_latency_sample_count"`
	PushWindowSeconds              float64            `json:"push_window_seconds"`
	PushOutcomes                   admin.PushOutcomes `json:"push_outcomes"`
	PushLatencyBucketUpperBoundsMS [15]float64        `json:"push_latency_bucket_upper_bounds_ms"`
	PushLatencyBucketCounts        [16]int64          `json:"push_latency_bucket_counts"`
}

type rateLimitDenialSurfacePayload struct {
	Count         int64                           `json:"rate_limit_denials_count"`
	WindowSeconds float64                         `json:"rate_limit_denials_window_seconds"`
	RatePerSecond float64                         `json:"rate_limit_denials_rate_per_second"`
	BySurface     admin.RateLimitDenialsBySurface `json:"rate_limit_denials_by_surface"`
	Dropped       int64                           `json:"rate_limit_denials_dropped"`
}

var rateLimitDenialPayloadKeys = []string{
	"rate_limit_denials_by_surface",
	"rate_limit_denials_count",
	"rate_limit_denials_dropped",
	"rate_limit_denials_rate_per_second",
	"rate_limit_denials_window_seconds",
}

var rateLimitDenialSurfacePayloadKeys = []string{
	"add_chat_user", "check_password", "check_password_ip", "contacts_search",
	"create_channel", "create_chat", "dialog_filter_mutation", "get_password", "get_password_ip",
	"message_send", "messages_search", "messages_search_global", "password_proof",
	"save_file_part", "send_code_ip_calls", "send_code_ip_distinct_numbers",
	"sign_in_fail_ip", "sign_up_ip", "update_profile", "upload_get_file",
}

var pushSurfaceKeys = []string{
	"push_latency_p50_ms",
	"push_latency_p50_overflow",
	"push_latency_p95_ms",
	"push_latency_p95_overflow",
	"push_latency_sample_count",
	"push_window_seconds",
	"push_outcomes",
	"push_latency_bucket_upper_bounds_ms",
	"push_latency_bucket_counts",
}

func TestAuthenticatedJSONAndSSESharePushSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newAuthTestStore(t)
	registry := mtproto.NewSessionRegistry()
	start := time.Unix(1_700_000_000, 0)
	now := start
	metrics := store.NewNotificationMetricsWithClock(func() time.Time { return now })
	now = start.Add(24 * time.Millisecond)
	metrics.RecordPushOutcome(store.PushOutcomeSuccess, start)
	now = start.Add(42 * time.Second)
	metrics.RecordPushOutcome(store.PushOutcomeOwnerMismatch, now)
	recorded := metrics.Snapshot().Push

	want := pushSurfacePayload{
		PushLatencyP50:         50,
		PushLatencyP50Overflow: false,
		PushLatencyP95:         50,
		PushLatencyP95Overflow: false,
		PushLatencySampleCount: 1,
		PushWindowSeconds:      42,
		PushOutcomes: admin.PushOutcomes{
			Success:       1,
			OwnerMismatch: 1,
		},
		PushLatencyBucketUpperBoundsMS: [15]float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000},
		PushLatencyBucketCounts:        [16]int64{0, 0, 0, 0, 0, 1},
	}
	if recorded.P50Milliseconds != want.PushLatencyP50 || recorded.P95Milliseconds != want.PushLatencyP95 ||
		recorded.SampleCount != want.PushLatencySampleCount || recorded.WindowSeconds != want.PushWindowSeconds ||
		recorded.Outcomes != (store.PushOutcomeCounts{Success: 1, OwnerMismatch: 1}) ||
		recorded.LatencyBucketUpperBoundsMilliseconds != want.PushLatencyBucketUpperBoundsMS ||
		recorded.LatencyBucketCounts != want.PushLatencyBucketCounts {
		t.Fatalf("recorder snapshot = %+v, want %+v", recorded, want)
	}

	b := sseTestBroadcaster(t, admin.BroadcasterConfig{
		Sample:            admin.NewMetricsSampler(registry, st, metrics),
		Render:            admin.DefaultFragmentRenderer,
		Interval:          time.Hour,
		Heartbeat:         time.Hour,
		MaxStreamDuration: 5 * time.Second,
	})
	rawToken := "cross-surface-token"
	h := admin.AdminRouter(admin.LoginHandlerConfig{
		Store:         st,
		TokenHash:     sha256hex([]byte(rawToken)),
		Logger:        slog.New(slog.DiscardHandler),
		Events:        b,
		NotifyMetrics: metrics,
	}, registry)

	baseline := requestMetrics(t, ctx, admin.Handler(registry, st))
	wantUninstrumented := []string{"push_latency_p50_ms", "push_latency_p95_ms"}
	if !slices.Equal(baseline.Uninstrumented, wantUninstrumented) {
		t.Fatalf("uninstrumented baseline = %v, want %v", baseline.Uninstrumented, wantUninstrumented)
	}

	sessionID := loginAndGetSession(t, h, rawToken)
	jsonBody, jsonResponse := authenticatedMetrics(t, h, sessionID)
	if len(jsonResponse.Uninstrumented) != 0 {
		t.Fatalf("instrumented uninstrumented = %v, want only the two push percentile names removed", jsonResponse.Uninstrumented)
	}

	gotJSON := decodePushSurface(t, "JSON", pushFields(t, "JSON", []byte(jsonBody), true))
	if gotJSON != want {
		t.Fatalf("JSON push payload = %+v, want %+v", gotJSON, want)
	}

	sseServer := httptest.NewServer(h)
	t.Cleanup(sseServer.Close)
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sseServer.URL+"/admin/events", nil)
	if err != nil {
		t.Fatalf("new authenticated SSE request: %v", err)
	}
	sseReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	sseResponse, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("authenticated SSE request: %v", err)
	}
	defer func() { _ = sseResponse.Body.Close() }() //nolint:errcheck // best-effort close
	if sseResponse.StatusCode != http.StatusOK {
		t.Fatalf("authenticated SSE status = %d, want 200", sseResponse.StatusCode)
	}
	sseBody := readSSEUntil(t, sseResponse.Body, `<script id="push-telemetry" type="application/json">`, 5*time.Second)
	sseJSON := extractPushTelemetryJSON(t, sseBody)
	gotSSE := decodePushSurface(t, "SSE", pushFields(t, "SSE", sseJSON, false))
	if gotSSE != gotJSON {
		t.Fatalf("SSE push payload = %+v, JSON = %+v", gotSSE, gotJSON)
	}
	if gotSSE != want {
		t.Fatalf("SSE push payload = %+v, want %+v", gotSSE, want)
	}
}

func TestAuthenticatedJSONAndSSEShareFleetSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := newAuthTestStore(t)
	registry := mtproto.NewSessionRegistry()

	firstID, secondID := "edge", "edge"
	for _, sample := range []store.FleetProcessSample{
		{
			Generation:       "00000000000000000000000000000081",
			ReplicaID:        &firstID,
			Version:          "v1.2.3",
			Connections:      2,
			Sessions:         2,
			AccountIDs:       []int64{918273645, 918273646},
			AccountsComplete: true,
		},
		{
			Generation:       "00000000000000000000000000000082",
			ReplicaID:        &secondID,
			Version:          "v1.2.3",
			Connections:      3,
			Sessions:         2,
			AccountIDs:       []int64{918273646, 918273647},
			AccountsComplete: true,
		},
	} {
		if err := st.PublishFleetSnapshot(ctx, sample); err != nil {
			t.Fatalf("publish fleet snapshot for %s: %v", sample.Generation, err)
		}
	}

	identity := admin.ProcessIdentity{
		StartedAt:  time.Now().UTC(),
		Generation: "00000000000000000000000000000081",
		ReplicaID:  &firstID,
	}
	cache := admin.NewMetricsSnapshotCache(registry, st, identity, nil)
	b := sseTestBroadcaster(t, admin.BroadcasterConfig{
		Sample:            cache.Snapshot,
		Render:            admin.DashboardFragmentRenderer,
		Interval:          time.Hour,
		Heartbeat:         time.Hour,
		MaxStreamDuration: 5 * time.Second,
	})
	const rawToken = "fleet-admin-token"
	h := admin.AdminRouter(admin.LoginHandlerConfig{
		Store:     st,
		TokenHash: sha256hex([]byte(rawToken)),
		Logger:    slog.New(slog.DiscardHandler),
		Events:    b,
		Metrics:   cache,
	}, registry)

	sessionID := loginAndGetSession(t, h, rawToken)
	dashboardReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/dashboard", nil)
	dashboardReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	dashboardRec := httptest.NewRecorder()
	h.ServeHTTP(dashboardRec, dashboardReq)
	if dashboardRec.Code != http.StatusOK {
		t.Fatalf("authenticated dashboard status = %d, want 200", dashboardRec.Code)
	}
	for _, want := range []string{
		`id="v-fleet-connections" data-metric="fleet_connections" class="metric-value tabular-nums">5`,
		`id="v-fleet-accounts" data-metric="fleet_distinct_accounts" class="metric-value tabular-nums">3`,
		"edge",
		"Collision",
		"This replica",
		"v1.2.3",
	} {
		if !strings.Contains(dashboardRec.Body.String(), want) {
			t.Errorf("authenticated dashboard omitted fleet content %q", want)
		}
	}
	for _, accountID := range []string{"918273645", "918273646", "918273647"} {
		if strings.Contains(dashboardRec.Body.String(), accountID) {
			t.Fatalf("authenticated dashboard exposed live-set account identifier %s", accountID)
		}
	}
	jsonBody, _ := authenticatedMetrics(t, h, sessionID)
	type replica struct {
		Generation         string    `json:"process_generation"`
		ReplicaID          *string   `json:"replica_id"`
		Version            *string   `json:"version"`
		ProcessStartedAt   time.Time `json:"process_started_at"`
		HeartbeatAt        time.Time `json:"heartbeat_at"`
		Connections        int64     `json:"connections"`
		Sessions           int64     `json:"sessions"`
		DistinctAccounts   *int64    `json:"distinct_accounts"`
		DuplicateReplicaID bool      `json:"duplicate_replica_id"`
	}
	type fleetSample struct {
		SampleState      string    `json:"sample_state"`
		SampleAgeSeconds float64   `json:"sample_age_seconds"`
		SampledAt        time.Time `json:"fleet_sampled_at"`
		Connections      int64     `json:"fleet_connections"`
		Sessions         int64     `json:"fleet_sessions"`
		DistinctAccounts *int64    `json:"fleet_distinct_accounts"`
		Replicas         []replica `json:"fleet_replicas"`
	}
	decodeFleetSample := func(surface string, payload []byte) fleetSample {
		t.Helper()
		var sample fleetSample
		if err := json.Unmarshal(payload, &sample); err != nil {
			t.Fatalf("decode %s fleet sample: %v", surface, err)
		}
		if sample.SampledAt.IsZero() {
			t.Fatalf("%s fleet_sampled_at is missing", surface)
		}
		if sample.DistinctAccounts == nil || *sample.DistinctAccounts != 3 {
			t.Fatalf("%s fleet distinct accounts = %v, want 3", surface, sample.DistinctAccounts)
		}
		if sample.Connections != 5 || sample.Sessions != 4 {
			t.Fatalf("%s fleet totals = %d connections, %d sessions, want 5 connections and 4 sessions", surface, sample.Connections, sample.Sessions)
		}
		if len(sample.Replicas) != 2 {
			t.Fatalf("%s fleet replicas = %d, want 2", surface, len(sample.Replicas))
		}
		if sample.SampleState != "available" || sample.SampleAgeSeconds < 0 || sample.SampleAgeSeconds >= 1 {
			t.Fatalf("%s sample freshness = %q at %v seconds, want a recent available sample", surface, sample.SampleState, sample.SampleAgeSeconds)
		}
		return sample
	}
	jsonFleet := decodeFleetSample("JSON", []byte(jsonBody))
	var localMetrics struct {
		Connections int `json:"connections"`
		Sessions    int `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(jsonBody), &localMetrics); err != nil {
		t.Fatalf("decode process-local metrics: %v", err)
	}
	if localMetrics.Connections != 0 || localMetrics.Sessions != 0 {
		t.Fatalf("process-local metrics were reinterpreted as fleet totals: %+v", localMetrics)
	}
	if strings.Contains(jsonBody, "918273645") || strings.Contains(jsonBody, "918273646") || strings.Contains(jsonBody, "918273647") {
		t.Fatal("JSON metrics exposed a live-set account identifier")
	}
	byGeneration := make(map[string]replica, len(jsonFleet.Replicas))
	for _, item := range jsonFleet.Replicas {
		if item.ReplicaID == nil || *item.ReplicaID != "edge" || item.Version == nil || item.DistinctAccounts == nil || *item.DistinctAccounts < 0 ||
			item.ProcessStartedAt.IsZero() || item.HeartbeatAt.IsZero() {
			t.Fatalf("JSON fleet replica omitted safe metadata or its exact local account count: %+v", item)
		}
		if !item.DuplicateReplicaID {
			t.Errorf("duplicate configured replica ID was not flagged: %+v", item)
		}
		byGeneration[item.Generation] = item
	}
	firstReplica, firstOK := byGeneration["00000000000000000000000000000081"]
	secondReplica, secondOK := byGeneration["00000000000000000000000000000082"]
	if len(byGeneration) != 2 || !firstOK || !secondOK || firstReplica.Connections != 2 || *firstReplica.DistinctAccounts != 2 ||
		secondReplica.Connections != 3 || *secondReplica.DistinctAccounts != 2 {
		t.Fatalf("JSON per-generation metrics = %+v, want two generations with (2,2) and (3,2) gauges", byGeneration)
	}

	sseServer := httptest.NewServer(h)
	t.Cleanup(sseServer.Close)
	sseReq, err := http.NewRequestWithContext(ctx, http.MethodGet, sseServer.URL+"/admin/events", nil)
	if err != nil {
		t.Fatalf("new authenticated SSE request: %v", err)
	}
	sseReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	sseResponse, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("authenticated SSE request: %v", err)
	}
	defer func() { _ = sseResponse.Body.Close() }() //nolint:errcheck // best-effort close
	if sseResponse.StatusCode != http.StatusOK {
		t.Fatalf("authenticated SSE status = %d, want 200", sseResponse.StatusCode)
	}
	sseBody := readSSEUntil(t, sseResponse.Body, `id="fleet-telemetry"`, 5*time.Second)
	for _, want := range []string{
		`id="v-fleet-connections" data-metric="fleet_connections" class="metric-value tabular-nums">5`,
		`id="v-fleet-accounts" data-metric="fleet_distinct_accounts" class="metric-value tabular-nums">3`,
		"edge",
		"Collision",
	} {
		if !strings.Contains(sseBody, want) {
			t.Errorf("authenticated SSE fragment omitted fleet content %q", want)
		}
	}
	const fleetScript = `<script id="fleet-telemetry" type="application/json">`
	start := strings.Index(sseBody, fleetScript)
	if start < 0 {
		t.Fatalf("SSE fleet telemetry script missing from stream: %s", sseBody)
	}
	start += len(fleetScript)
	end := strings.Index(sseBody[start:], `</script>`)
	if end < 0 {
		t.Fatal("SSE fleet telemetry script is not closed")
	}
	ssePayload := []byte(sseBody[start : start+end])
	sseFleet := decodeFleetSample("SSE", ssePayload)
	if strings.Contains(sseBody, "918273645") || strings.Contains(sseBody, "918273646") || strings.Contains(sseBody, "918273647") {
		t.Fatal("SSE metrics exposed a live-set account identifier")
	}
	if !sseFleet.SampledAt.Equal(jsonFleet.SampledAt) || sseFleet.Connections != jsonFleet.Connections ||
		sseFleet.Sessions != jsonFleet.Sessions || *sseFleet.DistinctAccounts != *jsonFleet.DistinctAccounts ||
		sseFleet.SampleState != jsonFleet.SampleState || !reflect.DeepEqual(sseFleet.Replicas, jsonFleet.Replicas) {
		t.Fatalf("JSON and SSE fleet snapshots differ: JSON=%+v SSE=%+v", jsonFleet, sseFleet)
	}
	if delta := sseFleet.SampleAgeSeconds - jsonFleet.SampleAgeSeconds; delta < 0 || delta >= 1 {
		t.Fatalf("JSON/SSE sample ages diverged: JSON=%v seconds, SSE=%v seconds", jsonFleet.SampleAgeSeconds, sseFleet.SampleAgeSeconds)
	}
}

func TestAuthenticatedJSONAndSSEShareRateLimitDenialSnapshot(t *testing.T) {
	t.Parallel()

	st := newAuthTestStore(t)
	registry := mtproto.NewSessionRegistry()
	start := time.Unix(1_700_000_000, 0)
	now := start
	metrics := store.NewNotificationMetricsWithClock(func() time.Time { return now })
	now = start.Add(42 * time.Second)
	surfaces := []string{
		"message_send",
		"create_chat",
		"add_chat_user",
		"create_channel",
		"messages_search",
		"contacts_search",
		"messages_search_global",
		"save_file_part",
		"upload_get_file",
		"send_code_ip_calls",
		"send_code_ip_distinct_numbers",
		"sign_in_fail_ip",
		"check_password",
		"check_password_ip",
		"get_password_ip",
		"sign_up_ip",
		"password_proof",
		"get_password",
		"update_profile",
		"dialog_filter_mutation",
	}
	for i, surface := range surfaces {
		for range i + 1 {
			metrics.RecordRateLimitDenial(surface)
		}
	}
	metrics.RecordRateLimitDenial("unknown_surface")
	metrics.RecordRateLimitDenial("unknown_surface")

	want := rateLimitDenialSurfacePayload{
		Count:         210,
		WindowSeconds: 42,
		RatePerSecond: 210.0 / 42.0,
		BySurface: admin.RateLimitDenialsBySurface{
			MessageSend:               1,
			CreateChat:                2,
			AddChatUser:               3,
			CreateChannel:             4,
			MessagesSearch:            5,
			ContactsSearch:            6,
			MessagesSearchGlobal:      7,
			SaveFilePart:              8,
			UploadGetFile:             9,
			SendCodeIPCalls:           10,
			SendCodeIPDistinctNumbers: 11,
			SignInFailIP:              12,
			CheckPassword:             13,
			CheckPasswordIP:           14,
			GetPasswordIP:             15,
			SignUpIP:                  16,
			PasswordProof:             17,
			GetPassword:               18,
			UpdateProfile:             19,
			DialogFilterMutation:      20,
		},
		Dropped: 2,
	}

	b := sseTestBroadcaster(t, admin.BroadcasterConfig{
		Sample:            admin.NewMetricsSampler(registry, st, metrics),
		Render:            admin.DashboardFragmentRenderer,
		Interval:          time.Hour,
		Heartbeat:         time.Hour,
		MaxStreamDuration: 5 * time.Second,
	})
	rawToken := "rate-limit-cross-surface-token"
	h := admin.AdminRouter(admin.LoginHandlerConfig{
		Store:         st,
		TokenHash:     sha256hex([]byte(rawToken)),
		Logger:        slog.New(slog.DiscardHandler),
		Events:        b,
		NotifyMetrics: metrics,
	}, registry)

	sessionID := loginAndGetSession(t, h, rawToken)
	jsonBody, _ := authenticatedMetrics(t, h, sessionID)
	gotJSON := decodeRateLimitDenialSurface(t, "JSON", []byte(jsonBody), true)
	if gotJSON != want {
		t.Fatalf("JSON rate-limit denial payload = %+v, want %+v", gotJSON, want)
	}

	sseServer := httptest.NewServer(h)
	t.Cleanup(sseServer.Close)
	sseReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, sseServer.URL+"/admin/events", nil)
	if err != nil {
		t.Fatalf("new authenticated SSE request: %v", err)
	}
	sseReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	sseResponse, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatalf("authenticated SSE request: %v", err)
	}
	defer func() { _ = sseResponse.Body.Close() }() //nolint:errcheck // best-effort close
	if sseResponse.StatusCode != http.StatusOK {
		t.Fatalf("authenticated SSE status = %d, want 200", sseResponse.StatusCode)
	}
	sseBody := readSSEUntil(t, sseResponse.Body, `<script id="rate-limit-denials-telemetry" type="application/json">`, 5*time.Second)
	gotSSE := decodeRateLimitDenialSurface(t, "SSE", extractRateLimitDenialTelemetryJSON(t, sseBody), false)
	if gotSSE != gotJSON {
		t.Fatalf("SSE rate-limit denial payload = %+v, JSON = %+v", gotSSE, gotJSON)
	}
	if gotSSE != want {
		t.Fatalf("SSE rate-limit denial payload = %+v, want %+v", gotSSE, want)
	}
}

func TestAdminLoginAttemptBudgetIsSharedAcrossRouters(t *testing.T) {
	t.Parallel()

	dsn := pgtest.DSN(t)
	firstStore := newAuthTestStoreForDSN(t, dsn)
	secondStore := newAuthTestStoreForDSN(t, dsn)
	first := newTestRouter(t, firstStore, authTestTokenHash())
	second := newTestRouter(t, secondStore, authTestTokenHash())

	for range 5 {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/login", nil)
		req.RemoteAddr = "198.51.100.23:4567"
		rec := httptest.NewRecorder()
		first.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("first router login attempt status = %d, want 401", rec.Code)
		}
	}

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/admin/login", nil)
	req.RemoteAddr = "198.51.100.23:4567"
	rec := httptest.NewRecorder()
	start := time.Now()
	second.ServeHTTP(rec, req)
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("sixth attempt on second router took %s, want the shared limit delay", elapsed)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("second router login attempt status = %d, want 401", rec.Code)
	}
}

func TestAdminStreamCapIsSharedAcrossBroadcasters(t *testing.T) {
	t.Parallel()

	dsn := pgtest.DSN(t)
	firstStore := newAuthTestStoreForDSN(t, dsn)
	secondStore := newAuthTestStoreForDSN(t, dsn)
	first := admin.NewBroadcaster(admin.BroadcasterConfig{Store: firstStore, MaxClients: 1})
	second := admin.NewBroadcaster(admin.BroadcasterConfig{Store: secondStore, MaxClients: 1})

	_, _, releaseFirst, err := first.SubscribeForTest()
	if err != nil {
		t.Fatalf("first broadcaster subscribe: %v", err)
	}
	defer releaseFirst()
	if _, _, releaseSecond, err := second.SubscribeForTest(); err == nil {
		releaseSecond()
		t.Fatal("second broadcaster admitted a stream beyond the shared cap")
	}
	releaseFirst()
	_, _, releaseSecond, err := second.SubscribeForTest()
	if err != nil {
		t.Fatalf("second broadcaster subscribe after lease release: %v", err)
	}
	releaseSecond()
}

func authenticatedMetrics(t *testing.T, h http.Handler, sessionID string) (string, admin.MetricsResponse) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/metrics", nil)
	req.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionID}) //nolint:gosec // G124: test cookie
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated metrics status = %d, want 200", rec.Code)
	}
	var response admin.MetricsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode authenticated metrics: %v", err)
	}
	return rec.Body.String(), response
}

func pushFields(t *testing.T, label string, body []byte, endpoint bool) map[string]json.RawMessage {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode %s JSON: %v", label, err)
	}
	fields := make(map[string]json.RawMessage, len(pushSurfaceKeys))
	for _, key := range pushSurfaceKeys {
		raw, ok := root[key]
		if !ok {
			t.Fatalf("%s payload missing %q", label, key)
		}
		fields[key] = raw
	}
	pushKeyCount := 0
	for key := range root {
		if !strings.HasPrefix(key, "push_") {
			continue
		}
		pushKeyCount++
		if !slices.Contains(pushSurfaceKeys, key) {
			t.Fatalf("%s payload has unexpected push key %q", label, key)
		}
	}
	if pushKeyCount != len(pushSurfaceKeys) {
		t.Fatalf("%s payload has %d push keys, want %d", label, pushKeyCount, len(pushSurfaceKeys))
	}
	if !endpoint && len(root) != len(pushSurfaceKeys) {
		t.Fatalf("%s push payload keys = %v, want exactly %v", label, sortedKeys(root), pushSurfaceKeys)
	}
	return fields
}

func decodeRateLimitDenialSurface(t *testing.T, label string, body []byte, endpoint bool) rateLimitDenialSurfacePayload {
	t.Helper()
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode %s rate-limit denial JSON: %v", label, err)
	}
	fields := make(map[string]json.RawMessage, len(rateLimitDenialPayloadKeys))
	for _, key := range rateLimitDenialPayloadKeys {
		raw, ok := root[key]
		if !ok {
			t.Fatalf("%s rate-limit denial payload missing %q", label, key)
		}
		fields[key] = raw
	}
	if !endpoint && !slices.Equal(sortedKeys(root), rateLimitDenialPayloadKeys) {
		t.Fatalf("%s rate-limit denial top-level keys = %v, want %v", label, sortedKeys(root), rateLimitDenialPayloadKeys)
	}

	var surface map[string]json.RawMessage
	if err := json.Unmarshal(fields["rate_limit_denials_by_surface"], &surface); err != nil {
		t.Fatalf("decode %s rate-limit denial surfaces: %v", label, err)
	}
	if !slices.Equal(sortedKeys(surface), rateLimitDenialSurfacePayloadKeys) {
		t.Fatalf("%s rate-limit denial surface keys = %v, want %v", label, sortedKeys(surface), rateLimitDenialSurfacePayloadKeys)
	}
	for key, raw := range surface {
		if string(bytes.TrimSpace(raw)) == "null" {
			t.Errorf("%s rate-limit denial surface %s is null, want integer", label, key)
			continue
		}
		var count int64
		if err := json.Unmarshal(raw, &count); err != nil {
			t.Errorf("decode %s rate-limit denial surface %s as integer: %v", label, key, err)
		}
	}

	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode %s rate-limit denial fields: %v", label, err)
	}
	var payload rateLimitDenialSurfacePayload
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode %s rate-limit denial fields with exact types: %v", label, err)
	}
	return payload
}

func decodePushSurface(t *testing.T, label string, fields map[string]json.RawMessage) pushSurfacePayload {
	t.Helper()
	var outcomes map[string]json.RawMessage
	if err := json.Unmarshal(fields["push_outcomes"], &outcomes); err != nil {
		t.Fatalf("decode %s push_outcomes: %v", label, err)
	}
	wantOutcomeKeys := []string{"encode_failure", "owner_mismatch", "success", "write_failure"}
	if !slices.Equal(sortedKeys(outcomes), wantOutcomeKeys) {
		t.Fatalf("%s push_outcomes keys = %v, want %v", label, sortedKeys(outcomes), wantOutcomeKeys)
	}
	for _, key := range wantOutcomeKeys {
		var value int64
		if err := json.Unmarshal(outcomes[key], &value); err != nil {
			t.Fatalf("decode %s push_outcomes.%s as integer: %v", label, key, err)
		}
	}
	var bounds []json.RawMessage
	if err := json.Unmarshal(fields["push_latency_bucket_upper_bounds_ms"], &bounds); err != nil {
		t.Fatalf("decode %s bucket bounds: %v", label, err)
	}
	if len(bounds) != 15 {
		t.Fatalf("%s bucket bounds length = %d, want 15", label, len(bounds))
	}
	for i, raw := range bounds {
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decode %s bucket bound %d as number: %v", label, i, err)
		}
	}
	var counts []json.RawMessage
	if err := json.Unmarshal(fields["push_latency_bucket_counts"], &counts); err != nil {
		t.Fatalf("decode %s bucket counts: %v", label, err)
	}
	if len(counts) != 16 {
		t.Fatalf("%s bucket counts length = %d, want 16", label, len(counts))
	}
	for i, raw := range counts {
		var value int64
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("decode %s bucket count %d as integer: %v", label, i, err)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode %s push fields: %v", label, err)
	}
	var payload pushSurfacePayload
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode %s push fields with exact types: %v", label, err)
	}
	return payload
}

func sortedKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
