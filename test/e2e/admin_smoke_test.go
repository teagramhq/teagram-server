package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/blob"
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
			configured:      "https://teagram.tailaa4918.ts.net",
			wantAdminOrigin: "https://teagram.tailaa4918.ts.net",
			wrongOrigin:     "http://localhost:2445",
		},
		{
			name:            "derived listener origin",
			configured:      "",
			wantAdminOrigin: "http://127.0.0.1:2445",
			wrongOrigin:     "https://teagram.tailaa4918.ts.net",
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

func testSmokeAdminStickerTakedown(t *testing.T) {
	t.Helper()
	f := newSmokeFixture(t)
	const rawToken = "smoke-admin-sticker" //nolint:gosec // G101: test credential
	tokenSum := sha256.Sum256([]byte(rawToken))
	tokenHash := hex.EncodeToString(tokenSum[:])
	owner, err := f.store.CreateUser(f.ctx, "+15551925190")
	if err != nil {
		t.Fatalf("create sticker-set owner: %v", err)
	}
	conn, err := pgx.Connect(f.ctx, f.dsn)
	if err != nil {
		t.Fatalf("connect to seed user sticker set: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(f.ctx); err != nil {
			t.Errorf("close sticker-set seed connection: %v", err)
		}
	})
	var setID int64
	if err := conn.QueryRow(f.ctx, `
		INSERT INTO user_created_sticker_sets
		    (access_hash, creator_id, title, short_name, software, kind, item_count)
		VALUES (991725190, $1, '<img src=x>', 'security_cats', '<script>source</script>', 'regular', 1)
		RETURNING id`, owner.ID).Scan(&setID); err != nil {
		t.Fatalf("seed user sticker set: %v", err)
	}
	stickerBytes := []byte("stored sticker bytes used for takedown checks")
	stickerFile, err := f.store.AllocateAndCompleteFile(f.ctx, owner.ID, int64(len(stickerBytes)), "application/octet-stream", "sticker.tgs", 1<<30, []string{}, func(file store.File) error {
		written, err := f.blobs.Put(f.ctx, blob.Key(file.ID), bytes.NewReader(stickerBytes))
		if err != nil {
			return err
		}
		if written != int64(len(stickerBytes)) {
			return fmt.Errorf("stored %d bytes, want %d", written, len(stickerBytes))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store user sticker file: %v", err)
	}
	if _, err := conn.Exec(f.ctx, `
		INSERT INTO user_created_sticker_set_items
		    (set_id, position, file_id, emoji, format, width, height)
		VALUES ($1, 0, $2, '⭐', 'TGS', 512, 512)`, setID, stickerFile.ID); err != nil {
		t.Fatalf("seed sticker-set item: %v", err)
	}
	if _, err := conn.Exec(f.ctx, `INSERT INTO user_sticker_sets (user_id, set_id) VALUES ($1, $2)`, owner.ID, setID); err != nil {
		t.Fatalf("seed sticker-set install: %v", err)
	}

	identity, err := admin.NewProcessIdentity("edge-admin-sticker")
	if err != nil {
		t.Fatalf("new serving process identity: %v", err)
	}
	metricsCache := admin.NewMetricsSnapshotCache(f.registry, f.store, identity, nil)
	router := admin.AdminRouter(admin.LoginHandlerConfig{
		Store: f.store, TokenHash: tokenHash, Logger: slog.Default(), Metrics: metricsCache,
	}, f.registry)
	sessionID := smokeAdminLogin(t, f.ctx, router, rawToken)
	adminCookie := &http.Cookie{Name: admin.SessionCookieName(), Value: sessionID} //nolint:gosec // G124: test cookie
	otherToken := "separate-admin-token"
	otherTokenSum := sha256.Sum256([]byte(otherToken))
	otherRouter := admin.AdminRouter(admin.LoginHandlerConfig{
		Store: f.store, TokenHash: hex.EncodeToString(otherTokenSum[:]), Logger: slog.Default(),
	}, f.registry)
	nonAdminSession := smokeAdminLogin(t, f.ctx, otherRouter, otherToken)
	nonAdminReq := httptest.NewRequestWithContext(f.ctx, http.MethodPost, "/admin/sticker-sets/takedown", strings.NewReader(url.Values{"id": {strconv.FormatInt(setID, 10)}, "reason": {"not admin"}}.Encode()))
	nonAdminReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	nonAdminReq.AddCookie(&http.Cookie{Name: admin.SessionCookieName(), Value: nonAdminSession}) //nolint:gosec // G124: test cookie
	nonAdminRec := httptest.NewRecorder()
	router.ServeHTTP(nonAdminRec, nonAdminReq)
	if nonAdminRec.Code != http.StatusUnauthorized {
		t.Fatalf("request with a session for another admin token returned %d, want 401", nonAdminRec.Code)
	}

	pageReq := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/sticker-sets", nil)
	pageReq.AddCookie(adminCookie)
	pageRec := httptest.NewRecorder()
	router.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/sticker-sets returned %d, want 200", pageRec.Code)
	}
	pageBody := pageRec.Body.String()
	for _, want := range []string{"security_cats", "&lt;img src=x&gt;", "&lt;script&gt;source&lt;/script&gt;", "Created", "Audit"} {
		if !strings.Contains(pageBody, want) {
			t.Errorf("sticker-set admin page omitted %q", want)
		}
	}
	if !strings.Contains(pageBody, `name="short_name"`) {
		t.Fatal("sticker-set admin page omitted the short-name takedown form")
	}
	if strings.Contains(pageBody, "<img src=x>") || strings.Contains(pageBody, "<script>source</script>") {
		t.Fatal("sticker-set admin page rendered attacker-supplied text as HTML")
	}
	if _, err := conn.Exec(f.ctx, `
		INSERT INTO user_created_sticker_sets
		    (access_hash, creator_id, title, short_name, software, kind, item_count)
		SELECT 991725300 + n, $1, 'Page test', 'page_test_' || n::text, '', 'regular', 1
		FROM generate_series(1, 50) AS n`, owner.ID); err != nil {
		t.Fatalf("seed paginated sticker sets: %v", err)
	}
	pageTwoReq := httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/sticker-sets?page=2", nil)
	pageTwoReq.AddCookie(adminCookie)
	pageTwoRec := httptest.NewRecorder()
	router.ServeHTTP(pageTwoRec, pageTwoReq)
	if pageTwoRec.Code != http.StatusOK || !strings.Contains(pageTwoRec.Body.String(), "security_cats") {
		t.Fatalf("second sticker-set page status=%d contains original set=%t, want 200/true", pageTwoRec.Code, strings.Contains(pageTwoRec.Body.String(), "security_cats"))
	}

	post := func(path string, values url.Values, withCSRF bool) *httptest.ResponseRecorder {
		t.Helper()
		if withCSRF {
			csrfToken, err := admin.SessionCSRFToken(tokenHash, sessionID)
			if err != nil {
				t.Fatalf("derive session CSRF token: %v", err)
			}
			values.Set("csrf_token", csrfToken)
		}
		req := httptest.NewRequestWithContext(f.ctx, http.MethodPost, path, strings.NewReader(values.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(adminCookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	unauthReq := httptest.NewRequestWithContext(f.ctx, http.MethodPost, "/admin/sticker-sets/takedown", strings.NewReader(url.Values{"short_name": {"security_cats"}, "reason": {"unauthenticated"}}.Encode()))
	unauthReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unauthRec := httptest.NewRecorder()
	router.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated takedown returned %d, want 401", unauthRec.Code)
	}
	noCSRF := post("/admin/sticker-sets/takedown", url.Values{"id": {strconv.FormatInt(setID, 10)}, "reason": {"without token"}}, false)
	if noCSRF.Code != http.StatusUnauthorized {
		t.Fatalf("takedown without CSRF token returned %d, want 401", noCSRF.Code)
	}
	var live bool
	var auditCount int
	if err := conn.QueryRow(f.ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, setID).Scan(&live); err != nil {
		t.Fatalf("read set after CSRF rejection: %v", err)
	}
	if err := conn.QueryRow(f.ctx, `SELECT count(*) FROM admin_sticker_set_events`).Scan(&auditCount); err != nil {
		t.Fatalf("count audit events after CSRF rejection: %v", err)
	}
	if !live || auditCount != 0 {
		t.Fatalf("CSRF rejection changed state: is_live=%t audit events=%d", live, auditCount)
	}
	tooLong := post("/admin/sticker-sets/takedown", url.Values{"id": {strconv.FormatInt(setID, 10)}, "reason": {strings.Repeat("x", 513)}}, true)
	if tooLong.Code != http.StatusBadRequest {
		t.Fatalf("takedown with 513-byte reason returned %d, want 400", tooLong.Code)
	}
	catalogue := post("/admin/sticker-sets/takedown", url.Values{"short_name": {"twemoji_reactions"}, "reason": {"catalogue"}}, true)
	if catalogue.Code != http.StatusNotFound {
		t.Fatalf("catalogue-set takedown returned %d, want 404", catalogue.Code)
	}
	if err := conn.QueryRow(f.ctx, `SELECT count(*) FROM admin_sticker_set_events`).Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatalf("invalid actions changed audit count to %d, err=%v; want 0", auditCount, err)
	}

	takedown := post("/admin/sticker-sets/takedown", url.Values{"short_name": {"SECURITY_CATS"}, "reason": {"DMCA 2026-11-01"}}, true)
	if takedown.Code != http.StatusSeeOther {
		t.Fatalf("takedown returned %d, want 303", takedown.Code)
	}
	if err := conn.QueryRow(f.ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, setID).Scan(&live); err != nil || live {
		t.Fatalf("set after takedown is_live=%t, err=%v; want false", live, err)
	}
	var installCount int
	if err := conn.QueryRow(f.ctx, `SELECT count(*) FROM user_sticker_sets WHERE user_id = $1 AND set_id = $2`, owner.ID, setID).Scan(&installCount); err != nil || installCount != 1 {
		t.Fatalf("install count after takedown = %d, err=%v; want 1", installCount, err)
	}
	var storedDigest []byte
	if err := conn.QueryRow(f.ctx, `SELECT content_sha256 FROM user_created_sticker_set_items WHERE set_id = $1 AND file_id = $2`, setID, stickerFile.ID).Scan(&storedDigest); err != nil {
		t.Fatalf("read takedown content hash: %v", err)
	}
	wantDigest := sha256.Sum256(stickerBytes)
	if !bytes.Equal(storedDigest, wantDigest[:]) {
		t.Fatalf("takedown content hash = %x, want %x", storedDigest, wantDigest)
	}
	pageReq = httptest.NewRequestWithContext(f.ctx, http.MethodGet, "/admin/sticker-sets", nil)
	pageReq.AddCookie(adminCookie)
	pageRec = httptest.NewRecorder()
	router.ServeHTTP(pageRec, pageReq)
	if pageRec.Code != http.StatusOK || !strings.Contains(pageRec.Body.String(), "DMCA 2026-11-01") || !strings.Contains(pageRec.Body.String(), "takedown") {
		t.Fatalf("admin page did not show takedown audit event: status=%d", pageRec.Code)
	}

	restore := post("/admin/sticker-sets/restore", url.Values{"id": {strconv.FormatInt(setID, 10)}, "reason": {"mistake"}}, true)
	if restore.Code != http.StatusSeeOther {
		t.Fatalf("restore returned %d, want 303", restore.Code)
	}
	if err := conn.QueryRow(f.ctx, `SELECT is_live FROM user_created_sticker_sets WHERE id = $1`, setID).Scan(&live); err != nil || !live {
		t.Fatalf("set after restore is_live=%t, err=%v; want true", live, err)
	}
	var actionCount int
	var sessionFingerprint []byte
	if err := conn.QueryRow(f.ctx, `SELECT count(*) FROM admin_sticker_set_events WHERE set_id = $1`, setID).Scan(&actionCount); err != nil {
		t.Fatalf("read takedown and restore audit records: %v", err)
	}
	if err := conn.QueryRow(f.ctx, `SELECT session_fingerprint FROM admin_sticker_set_events WHERE set_id = $1 ORDER BY id DESC LIMIT 1`, setID).Scan(&sessionFingerprint); err != nil {
		t.Fatalf("read audit session fingerprint: %v", err)
	}
	if actionCount != 2 || !strings.EqualFold(hex.EncodeToString(sessionFingerprint), hex.EncodeToString(admin.HashSessionID(sessionID))) {
		t.Fatalf("audit records = %d, session fingerprint = %x; want two records for this session", actionCount, sessionFingerprint)
	}
	if _, err := conn.Exec(f.ctx, `UPDATE admin_sticker_set_events SET reason = 'rewritten' WHERE set_id = $1`, setID); err == nil {
		t.Fatal("admin audit update succeeded")
	}
	if _, err := conn.Exec(f.ctx, `DELETE FROM admin_sticker_set_events WHERE set_id = $1`, setID); err == nil {
		t.Fatal("admin audit delete succeeded")
	}
	if err := conn.QueryRow(f.ctx, `SELECT count(*) FROM user_sticker_sets WHERE user_id = $1 AND set_id = $2`, owner.ID, setID).Scan(&installCount); err != nil || installCount != 1 {
		t.Fatalf("install count after restore = %d, err=%v; want 1", installCount, err)
	}
}

func smokeAdminLogin(t *testing.T, ctx context.Context, router http.Handler, rawToken string) string {
	t.Helper()
	getReq := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/login", nil)
	getRec := httptest.NewRecorder()
	router.ServeHTTP(getRec, getReq)
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
		t.Fatal("admin login page did not set its CSRF cookie")
	}
	form := url.Values{"csrf_token": {csrfToken}, "token": {rawToken}}
	postReq := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.AddCookie(&http.Cookie{Name: "__Host-csrf-token", Value: csrfCookie}) //nolint:gosec // G124: test cookie
	postRec := httptest.NewRecorder()
	router.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusFound {
		t.Fatalf("POST /admin/login returned %d, want 302", postRec.Code)
	}
	for _, cookie := range postRec.Result().Cookies() {
		if cookie.Name == admin.SessionCookieName() {
			return cookie.Value
		}
	}
	t.Fatal("successful admin login did not set a session cookie")
	return ""
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
