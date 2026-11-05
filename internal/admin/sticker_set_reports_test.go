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

func TestAdminStickerSetReportsCanBeReviewedAndDismissed(t *testing.T) {
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
}
