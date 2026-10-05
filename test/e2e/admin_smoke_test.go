package e2e_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/store"
)

func testSmokeAdminProxyLogin(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const rawToken = "smoke-admin-token"
	tokenSum := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(tokenSum[:])
	identity, err := admin.NewProcessIdentity("edge-1")
	if err != nil {
		t.Fatalf("new serving process identity: %v", err)
	}
	const secondGeneration = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	for _, tc := range []struct {
		name            string
		configured      string
		wantAdminOrigin string
		wrongOrigin     string
	}{
		{
			name:            "configured HTTPS proxy origin",
			configured:      "https://telegram-server.tailaa4918.ts.net",
			wantAdminOrigin: "https://telegram-server.tailaa4918.ts.net",
			wrongOrigin:     "http://localhost:2445",
		},
		{
			name:            "derived listener origin",
			configured:      "",
			wantAdminOrigin: "http://127.0.0.1:2445",
			wrongOrigin:     "https://telegram-server.tailaa4918.ts.net",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secondReplicaID := "edge-2"
			for _, sample := range []store.FleetProcessSample{
				{
					Generation:       identity.Generation,
					ReplicaID:        identity.ReplicaID,
					Version:          "v1.2.3",
					Connections:      2,
					Sessions:         2,
					AccountIDs:       []int64{918273645, 918273646},
					AccountsComplete: true,
				},
				{
					Generation:       secondGeneration,
					ReplicaID:        &secondReplicaID,
					Version:          "v1.2.3",
					Connections:      3,
					Sessions:         2,
					AccountIDs:       []int64{918273646, 918273647},
					AccountsComplete: true,
				},
			} {
				if err := f.store.PublishFleetSnapshot(f.ctx, sample); err != nil {
					t.Fatalf("publish fleet sample for %s: %v", sample.Generation, err)
				}
			}
			metricsCache := admin.NewMetricsSnapshotCache(f.registry, f.store, identity, nil)

			t.Setenv("TG_POSTGRES_DSN", f.dsn)
			t.Setenv("TG_AUTHKEY_ENC_KEY", strings.Repeat("0", 64))
			t.Setenv("TG_AUTHKEY_ENC_KEY_FILE", "")
			t.Setenv("TG_ADMIN_LISTEN_ADDR", "127.0.0.1:2445")
			t.Setenv("TG_ADMIN_TOKEN_HASH", tokenHash)
			t.Setenv("TG_ADMIN_ORIGIN", tc.configured)

			cfg, err := config.Load(slog.Default())
			if err != nil {
				t.Fatalf("load admin config: %v", err)
			}
			if cfg.AdminOrigin == "" {
				t.Fatal("loaded config supplied an empty origin to the admin router")
			}
			if cfg.AdminOrigin != tc.wantAdminOrigin {
				t.Fatalf("admin origin = %q, want %q", cfg.AdminOrigin, tc.wantAdminOrigin)
			}

			router := admin.AdminRouter(admin.LoginHandlerConfig{
				Store:       f.store,
				TokenHash:   cfg.AdminTokenHash,
				Logger:      slog.Default(),
				AdminOrigin: cfg.AdminOrigin,
				Metrics:     metricsCache,
			}, f.registry)

			get := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/login", nil)
			getRec := httptest.NewRecorder()
			router.ServeHTTP(getRec, get)
			if getRec.Code != http.StatusOK {
				t.Fatalf("GET /admin/login returned %d, want 200", getRec.Code)
			}
			csrfToken := smokeFormCSRFToken(t, getRec.Body.String())
			csrfCookie := ""
			for _, cookie := range getRec.Result().Cookies() {
				if cookie.Name == "__Host-csrf-token" {
					csrfCookie = cookie.Value
				}
			}
			if csrfCookie == "" {
				t.Fatal("login page did not set its CSRF cookie")
			}

			wrongForm := strings.NewReader("csrf_token=" + csrfToken + "&token=" + rawToken)
			wrongReq := httptest.NewRequestWithContext(f.ctx, http.MethodPost, "/admin/login", wrongForm)
			wrongReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			wrongReq.Header.Set("Origin", tc.wrongOrigin)
			wrongReq.AddCookie(&http.Cookie{Name: "__Host-csrf-token", Value: csrfCookie}) //nolint:gosec // G124: test cookie
			wrongRec := httptest.NewRecorder()
			router.ServeHTTP(wrongRec, wrongReq)
			if wrongRec.Code != http.StatusUnauthorized {
				t.Fatalf("login with untrusted Origin %q returned %d, want 401", tc.wrongOrigin, wrongRec.Code)
			}

			form := strings.NewReader("csrf_token=" + csrfToken + "&token=" + rawToken)
			req := httptest.NewRequestWithContext(f.ctx, http.MethodPost, "/admin/login", form)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", cfg.AdminOrigin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Forwarded", "host=attacker.example;proto=http")
			req.Header.Set("X-Forwarded-Host", "attacker.example")
			req.Header.Set("X-Forwarded-Proto", "http")
			req.Host = "attacker.example"
			req.AddCookie(&http.Cookie{Name: "__Host-csrf-token", Value: csrfCookie}) //nolint:gosec // G124: test cookie
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusFound {
				t.Fatalf("login with trusted Origin returned %d, want 302", rec.Code)
			}

			sessionCookie := ""
			for _, cookie := range rec.Result().Cookies() {
				if cookie.Name == "__Host-admin-session" {
					sessionCookie = cookie.Value
				}
			}
			if sessionCookie == "" {
				t.Fatal("successful login did not set an admin session")
			}
			dashboardReq := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/dashboard", nil)
			dashboardReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionCookie}) //nolint:gosec // G124: test cookie
			dashboardRec := httptest.NewRecorder()
			router.ServeHTTP(dashboardRec, dashboardReq)
			if dashboardRec.Code != http.StatusOK {
				t.Fatalf("GET /admin/dashboard after login returned %d, want 200", dashboardRec.Code)
			}
			dashboardBody := dashboardRec.Body.String()
			for _, want := range []string{
				`id="v-fleet-connections" data-metric="fleet_connections" class="metric-value tabular-nums">5`,
				`id="v-fleet-accounts" data-metric="fleet_distinct_accounts" class="metric-value tabular-nums">3`,
				"edge-1",
				"edge-2",
				"v1.2.3",
				"Accounts on replica",
			} {
				if !strings.Contains(dashboardBody, want) {
					t.Errorf("server-rendered dashboard omitted fleet content %q", want)
				}
			}
			for _, accountID := range []string{"918273645", "918273646", "918273647"} {
				if strings.Contains(dashboardBody, accountID) {
					t.Fatalf("server-rendered dashboard exposed account identifier %s", accountID)
				}
			}

			metricsReq := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/metrics", nil)
			metricsReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionCookie}) //nolint:gosec // G124: test cookie
			metricsRec := httptest.NewRecorder()
			router.ServeHTTP(metricsRec, metricsReq)
			if metricsRec.Code != http.StatusOK {
				t.Fatalf("GET /admin/metrics after login returned %d, want 200", metricsRec.Code)
			}
			var metrics map[string]json.RawMessage
			if err := json.Unmarshal(metricsRec.Body.Bytes(), &metrics); err != nil {
				t.Fatalf("decode authenticated admin metrics: %v", err)
			}
			for _, field := range []string{"fleet_connections", "fleet_sessions", "fleet_distinct_accounts", "fleet_sampled_at", "fleet_replicas"} {
				if _, ok := metrics[field]; !ok {
					t.Errorf("authenticated admin metrics omitted %q", field)
				}
			}
			var connections, sessions, distinct int64
			if err := json.Unmarshal(metrics["fleet_connections"], &connections); err != nil || connections != 5 {
				t.Errorf("fleet connections = %d (decode error %v), want 5", connections, err)
			}
			if err := json.Unmarshal(metrics["fleet_sessions"], &sessions); err != nil || sessions != 4 {
				t.Errorf("fleet sessions = %d (decode error %v), want 4", sessions, err)
			}
			if err := json.Unmarshal(metrics["fleet_distinct_accounts"], &distinct); err != nil || distinct != 3 {
				t.Errorf("fleet distinct accounts = %d (decode error %v), want 3", distinct, err)
			}
			var sampledAt time.Time
			if err := json.Unmarshal(metrics["fleet_sampled_at"], &sampledAt); err != nil || sampledAt.IsZero() {
				t.Errorf("fleet sample timestamp = %s (decode error %v), want a Postgres sample timestamp", sampledAt, err)
			}

			if err := f.store.DeleteFleetSnapshot(f.ctx, secondGeneration); err != nil {
				t.Fatalf("expire second replica sample: %v", err)
			}
			reloadedCache := admin.NewMetricsSnapshotCache(f.registry, f.store, identity, nil)
			reloadedRouter := admin.AdminRouter(admin.LoginHandlerConfig{
				Store:       f.store,
				TokenHash:   cfg.AdminTokenHash,
				Logger:      slog.Default(),
				AdminOrigin: cfg.AdminOrigin,
				Metrics:     reloadedCache,
			}, f.registry)
			reloadedReq := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/dashboard", nil)
			reloadedReq.AddCookie(&http.Cookie{Name: "__Host-admin-session", Value: sessionCookie}) //nolint:gosec // G124: test cookie
			reloadedRec := httptest.NewRecorder()
			reloadedRouter.ServeHTTP(reloadedRec, reloadedReq)
			if reloadedRec.Code != http.StatusOK {
				t.Fatalf("GET /admin/dashboard after replica expiry returned %d, want 200", reloadedRec.Code)
			}
			reloadedBody := reloadedRec.Body.String()
			if !strings.Contains(reloadedBody, `id="v-fleet-connections" data-metric="fleet_connections" class="metric-value tabular-nums">2`) ||
				!strings.Contains(reloadedBody, `id="v-fleet-accounts" data-metric="fleet_distinct_accounts" class="metric-value tabular-nums">2`) {
				t.Fatal("dashboard did not update fleet totals after the second replica expired")
			}
			if strings.Contains(reloadedBody, "edge-2") {
				t.Fatal("dashboard retained the expired replica row")
			}
		})
	}
}

func smokeFormCSRFToken(t *testing.T, body string) string {
	t.Helper()
	marker := `name="csrf_token" value="`
	start := strings.Index(body, marker)
	if start < 0 {
		t.Fatal("page omitted csrf_token form field")
	}
	start += len(marker)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatal("csrf_token form field has no closing quote")
	}
	return body[start : start+end]
}
