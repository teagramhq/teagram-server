package admin_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/pgtest"
)

func TestAdminStickerSetReportsCanBeReviewedDismissedAndPaginated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn := pgtest.DSN(t)
	st := newAuthTestStoreForDSN(t, dsn)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to report database: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(ctx); err != nil {
			t.Errorf("close report database: %v", err)
		}
	})
	owner, err := st.CreateUser(ctx, "+15559308101")
	if err != nil {
		t.Fatalf("create sticker-set owner: %v", err)
	}
	reporter, err := st.CreateUser(ctx, "+15559308102")
	if err != nil {
		t.Fatalf("create report author: %v", err)
	}
	var setID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		VALUES (9308101, $1, 'Reported cats', 'reported_cats', 'regular', 1)
		RETURNING id`, owner.ID).Scan(&setID); err != nil {
		t.Fatalf("create reported set: %v", err)
	}
	const comment = "<script>report detail</script>"
	var reportID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO user_sticker_set_reports (
			reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment
		) VALUES ($1, $2, 1, $3, 42, 'copyright', $4)
		RETURNING id`, reporter.ID, setID, owner.ID, comment).Scan(&reportID); err != nil {
		t.Fatalf("create report: %v", err)
	}
	const reportPageSize = 50
	seeded, err := conn.Exec(ctx, `
		WITH new_reporters AS (
			INSERT INTO users (phone)
			SELECT '+1555998' || lpad(n::text, 4, '0')
			FROM generate_series(1, $3::int) AS n
			RETURNING id
		), numbered AS (
			SELECT id, row_number() OVER (ORDER BY id) AS n FROM new_reporters
		)
		INSERT INTO user_sticker_set_reports (
			reporter_id, set_id, message_peer_type, message_peer_id, message_id, reason, comment
		)
		SELECT id, $1, 1, $2, 1000 + n, 'copyright', 'large-group report ' || n
		FROM numbered
	`, setID, owner.ID, reportPageSize)
	if err != nil {
		t.Fatalf("seed large report group: %v", err)
	}
	if seeded.RowsAffected() != reportPageSize {
		t.Fatalf("seeded reports = %d, want %d", seeded.RowsAffected(), reportPageSize)
	}

	tokenHash := authTestTokenHash()
	const sessionID = "sticker-report-session"
	fingerprint, err := admin.TokenFingerprint(tokenHash)
	if err != nil {
		t.Fatalf("fingerprint admin token: %v", err)
	}
	if err := st.CreateAdminSession(ctx, admin.HashSessionID(sessionID), fingerprint, time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatalf("create admin session: %v", err)
	}
	h := newTestRouter(t, st, tokenHash)

	get := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/sticker-sets", nil)
	get.AddCookie(admin.SessionCookie(sessionID))
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /admin/sticker-sets returned %d, want 200", getRec.Code)
	}
	body := getRec.Body.String()
	for _, want := range []string{
		"Open reports", "Reported cats", "copyright", strconv.FormatInt(reporter.ID, 10), "message 42",
		"&lt;script&gt;report detail&lt;/script&gt;", "/admin/sticker-reports/dismiss", "/admin/sticker-sets/takedown",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sticker-set admin page omitted %q", want)
		}
	}
	if strings.Contains(body, comment) {
		t.Fatal("report comment was rendered as raw HTML")
	}
	if !strings.Contains(body, `name="id" value="`+strconv.FormatInt(setID, 10)+`"`) {
		t.Fatal("report group omitted the direct takedown action for its set")
	}
	if got := strings.Count(body, "Dismiss report"); got != reportPageSize {
		t.Fatalf("reports rendered on first group page = %d, want %d", got, reportPageSize)
	}
	if strings.Contains(body, "message 1050") || !strings.Contains(body, "report_page=2") {
		t.Fatal("first group page rendered its final report or omitted the next-page link")
	}
	page2Query := url.Values{
		"report_set_id": {strconv.FormatInt(setID, 10)},
		"report_page":   {"2"},
	}
	page2 := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/sticker-sets?"+page2Query.Encode(), nil)
	page2.AddCookie(admin.SessionCookie(sessionID))
	page2Rec := httptest.NewRecorder()
	h.ServeHTTP(page2Rec, page2)
	if page2Rec.Code != http.StatusOK {
		t.Fatalf("GET report page two returned %d, want 200", page2Rec.Code)
	}
	if got := strings.Count(page2Rec.Body.String(), "Dismiss report"); got != 1 {
		t.Fatalf("reports rendered on second group page = %d, want one", got)
	}
	if !strings.Contains(page2Rec.Body.String(), "message 1050") || !strings.Contains(page2Rec.Body.String(), "report_page=1") {
		t.Fatal("second group page omitted its report or previous-page link")
	}
	csrfToken, err := admin.SessionCSRFToken(tokenHash, sessionID)
	if err != nil {
		t.Fatalf("derive CSRF token: %v", err)
	}
	form := url.Values{"csrf_token": {csrfToken}, "id": {strconv.FormatInt(reportID, 10)}}
	post := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/sticker-reports/dismiss", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(admin.SessionCookie(sessionID))
	postRec := httptest.NewRecorder()
	h.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusSeeOther || postRec.Header().Get("Location") != "/admin/sticker-sets?notice=dismiss" {
		t.Fatalf("dismiss response = %d, Location %q; want 303 to dismissed notice", postRec.Code, postRec.Header().Get("Location"))
	}
	var status string
	var closedAt *time.Time
	if err := conn.QueryRow(ctx, `SELECT status, closed_at FROM user_sticker_set_reports WHERE id = $1`, reportID).Scan(&status, &closedAt); err != nil {
		t.Fatalf("read dismissed report: %v", err)
	}
	if status != "dismissed" || closedAt == nil {
		t.Fatalf("dismissed report status/closed_at = %q/%v, want dismissed/non-null", status, closedAt)
	}

	var drainedSetID int64
	if err := conn.QueryRow(ctx, `
		INSERT INTO user_created_sticker_sets (access_hash, creator_id, title, short_name, kind, item_count)
		VALUES (9308102, $1, 'Drained cats', 'drained_cats', 'regular', 1)
		RETURNING id`, owner.ID).Scan(&drainedSetID); err != nil {
		t.Fatalf("create set without reports: %v", err)
	}
	for _, query := range []url.Values{
		{"report_set_id": {strconv.FormatInt(drainedSetID, 10)}, "report_page": {"1"}},
		{"report_set_id": {strconv.FormatInt(drainedSetID+1000, 10)}, "report_page": {"2"}},
	} {
		stale := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/sticker-sets?"+query.Encode(), nil)
		stale.AddCookie(admin.SessionCookie(sessionID))
		staleRec := httptest.NewRecorder()
		h.ServeHTTP(staleRec, stale)
		if staleRec.Code != http.StatusSeeOther || staleRec.Header().Get("Location") != "/admin/sticker-sets?notice=no-reports" {
			t.Fatalf("stale report link %v = %d, Location %q; want 303 to the no-reports notice",
				query, staleRec.Code, staleRec.Header().Get("Location"))
		}
	}
	notice := httptest.NewRequestWithContext(ctx, http.MethodGet, "/admin/sticker-sets?notice=no-reports", nil)
	notice.AddCookie(admin.SessionCookie(sessionID))
	noticeRec := httptest.NewRecorder()
	h.ServeHTTP(noticeRec, notice)
	if noticeRec.Code != http.StatusOK || !strings.Contains(noticeRec.Body.String(), "no open reports") {
		t.Fatalf("no-reports notice page = %d, want 200 with the notice", noticeRec.Code)
	}
}
