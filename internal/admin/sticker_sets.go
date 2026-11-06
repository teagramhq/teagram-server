package admin

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/teagramhq/teagram-server/internal/store"
)

const adminStickerSetActionBodyBytes = 4 << 10

var adminStickerSetsPageTemplate = template.Must(template.New("admin-sticker-sets").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>User sticker sets — teagram-server</title>
  <link rel="stylesheet" href="/admin/assets/dashboard.css">
</head>
<body class="style-nova min-h-screen bg-background text-foreground">
  <main class="dashboard-shell mx-auto max-w-[1440px]" id="main">
    <header class="dashboard-header">
      <div class="dashboard-title-block">
        <h1 class="text-xl font-semibold leading-tight">User-imported sticker sets</h1>
        <span class="text-xs text-muted-foreground">Operator actions and audit history</span>
      </div>
      <nav class="dashboard-controls" aria-label="Admin pages">
        <a href="/admin/dashboard" class="dashboard-control">Operations</a>
        <form method="post" action="/admin/logout" class="logout-form inline">
          <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
          <button type="submit" class="dashboard-control">Log out</button>
        </form>
      </nav>
    </header>
    {{if .Notice}}<p role="status" class="mb-4">{{.Notice}}</p>{{end}}
    {{if .Error}}<p role="alert" class="mb-4">{{.Error}}</p>{{end}}

    <section class="dashboard-section" aria-labelledby="reports-heading">
      <div class="dashboard-section-heading">
        <div>
          <h2 id="reports-heading" class="dashboard-section-title">Open reports</h2>
          <p class="dashboard-section-subtitle">Reports are grouped by user sticker set. Taking a set down closes its open reports as actioned.</p>
        </div>
      </div>
      {{range .ReportGroups}}
        <article class="dashboard-section" aria-labelledby="report-set-{{.SetID}}">
          <h3 id="report-set-{{.SetID}}">{{.SetTitle}} ({{.SetShortName}}, ID {{.SetID}})</h3>
          {{if .SetIsLive}}
          <form method="post" action="/admin/sticker-sets/takedown" class="dashboard-controls">
            <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
            <input type="hidden" name="id" value="{{.SetID}}">
            <input type="hidden" name="reason" value="User report">
            <button type="submit">Take down reported set</button>
          </form>
          {{else}}<p>This set is already taken down.</p>{{end}}
          <div class="dashboard-table-wrap">
            <table class="dashboard-table">
              <caption class="sr-only">Open reports for {{.SetTitle}}</caption>
              <thead><tr><th scope="col">Reporter</th><th scope="col">Message</th><th scope="col">Reason</th><th scope="col">Comment</th><th scope="col">Received</th><th scope="col">Action</th></tr></thead>
              <tbody>
              {{range .Reports}}
                <tr>
                  <td>{{.ReporterID}}</td>
                  <td>{{.MessagePeerType}} {{.MessagePeerID}}, message {{.MessageID}}</td>
                  <td>{{.Reason}}</td><td>{{.Comment}}</td><td>{{.CreatedAt}}</td>
                  <td>
                    <form method="post" action="/admin/sticker-reports/dismiss">
                      <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
                      <input type="hidden" name="id" value="{{.ID}}">
                      <button type="submit">Dismiss report</button>
                    </form>
                  </td>
                </tr>
              {{end}}
              </tbody>
            </table>
          </div>
          <nav aria-label="Reports for {{.SetTitle}}" class="dashboard-controls">
            {{if .HasPrevious}}<a href="/admin/sticker-sets?page={{$.Page}}&amp;report_set_id={{.SetID}}&amp;report_page={{.PreviousPage}}">Previous reports</a>{{end}}
            <span>Report page {{.Page}}</span>
            {{if .HasNext}}<a href="/admin/sticker-sets?page={{$.Page}}&amp;report_set_id={{.SetID}}&amp;report_page={{.NextPage}}">Next reports</a>{{end}}
          </nav>
        </article>
      {{else}}
        <p>No open user sticker-set reports.</p>
      {{end}}
      <nav aria-label="Report group pages" class="dashboard-controls">
        {{if .HasPrevious}}<a href="/admin/sticker-sets?page={{.PreviousPage}}">Previous</a>{{end}}
        <span>Page {{.Page}}</span>
        {{if .HasNext}}<a href="/admin/sticker-sets?page={{.NextPage}}">Next</a>{{end}}
      </nav>
    </section>

    <section class="dashboard-section" aria-labelledby="sets-heading">
      <div class="dashboard-section-heading">
        <div>
          <h2 id="sets-heading" class="dashboard-section-title">Sticker sets</h2>
          <p class="dashboard-section-subtitle">Titles, source strings, and short names are displayed as text.</p>
        </div>
      </div>
      <form method="post" action="/admin/sticker-sets/takedown" class="dashboard-controls">
        <input type="hidden" name="csrf_token" value="{{.CSRFToken}}">
        <label>Take down by short name <input name="short_name" maxlength="64" required></label>
        <label>Reason <input name="reason" maxlength="512" required></label>
        <button type="submit">Take down</button>
      </form>
      <div class="dashboard-table-wrap">
        <table class="dashboard-table">
          <caption class="sr-only">Paginated user-created sticker sets</caption>
          <thead><tr>
            <th scope="col">ID</th><th scope="col">Title</th><th scope="col">Short name</th>
            <th scope="col">Creator account</th><th scope="col">Created</th>
            <th scope="col">Declared source</th><th scope="col">State</th><th scope="col">Action</th>
          </tr></thead>
          <tbody>
          {{range .Sets}}
            <tr>
              <td>{{.ID}}</td><td>{{.Title}}</td><td>{{.ShortName}}</td>
              <td>{{.CreatorID}}</td><td>{{.CreatedAt}}</td><td>{{.Software}}</td>
              <td>{{if .IsLive}}Live{{else}}Taken down{{end}}</td>
              <td>
                {{if .IsLive}}
                <form method="post" action="/admin/sticker-sets/takedown">
                  <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
                  <input type="hidden" name="id" value="{{.ID}}">
                  <label>Reason <input name="reason" maxlength="512" required></label>
                  <button type="submit">Take down</button>
                </form>
                {{else}}
                <form method="post" action="/admin/sticker-sets/restore">
                  <input type="hidden" name="csrf_token" value="{{$.CSRFToken}}">
                  <input type="hidden" name="id" value="{{.ID}}">
                  <label>Reason <input name="reason" maxlength="512" required></label>
                  <button type="submit">Restore</button>
                </form>
                {{end}}
              </td>
            </tr>
          {{else}}
            <tr><td colspan="8">No user sticker sets.</td></tr>
          {{end}}
          </tbody>
        </table>
      </div>
      <nav aria-label="Sticker set pages" class="dashboard-controls">
        {{if .HasPrevious}}<a href="/admin/sticker-sets?page={{.PreviousPage}}">Previous</a>{{end}}
        <span>Page {{.Page}}</span>
        {{if .HasNext}}<a href="/admin/sticker-sets?page={{.NextPage}}">Next</a>{{end}}
      </nav>
    </section>

    <section class="dashboard-section" aria-labelledby="audit-heading">
      <div class="dashboard-section-heading"><h2 id="audit-heading" class="dashboard-section-title">Audit history</h2></div>
      <div class="dashboard-table-wrap">
        <table class="dashboard-table">
          <caption class="sr-only">Paginated append-only sticker-set takedown and restore events</caption>
          <thead><tr><th scope="col">Time</th><th scope="col">Action</th><th scope="col">Set</th><th scope="col">Reason</th><th scope="col">Admin session fingerprint</th></tr></thead>
          <tbody>
          {{range .Events}}
            <tr><td>{{.CreatedAt}}</td><td>{{.Action}}</td><td>{{.SetTitle}} ({{.SetShortName}}, ID {{.SetID}})</td><td>{{.Reason}}</td><td><code>{{.SessionFingerprint}}</code></td></tr>
          {{else}}
            <tr><td colspan="5">No operator actions recorded.</td></tr>
          {{end}}
          </tbody>
        </table>
      </div>
    </section>
  </main>
</body>
</html>`))

type adminStickerSetRow struct {
	ID        int64
	Title     string
	ShortName string
	CreatorID int64
	CreatedAt string
	Software  string
	IsLive    bool
}

type adminStickerSetEventRow struct {
	Action             string
	SetID              int64
	SetTitle           string
	SetShortName       string
	Reason             string
	SessionFingerprint string
	CreatedAt          string
}

type adminStickerSetReportRow struct {
	ID              int64
	ReporterID      int64
	MessagePeerType string
	MessagePeerID   int64
	MessageID       int64
	Reason          string
	Comment         string
	CreatedAt       string
}

type adminStickerSetReportGroup struct {
	SetID        int64
	SetTitle     string
	SetShortName string
	SetIsLive    bool
	Reports      []adminStickerSetReportRow
	Page         int32
	PreviousPage int32
	NextPage     int32
	HasPrevious  bool
	HasNext      bool
}

type adminStickerSetsPageData struct {
	CSRFToken    string
	Sets         []adminStickerSetRow
	Events       []adminStickerSetEventRow
	ReportGroups []adminStickerSetReportGroup
	Page         int
	PreviousPage int
	NextPage     int
	HasPrevious  bool
	HasNext      bool
	Notice       string
	Error        string
}

func adminStickerSetsGET(cfg LoginHandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if rawPage := r.URL.Query().Get("page"); rawPage != "" {
			parsed, err := strconv.Atoi(rawPage)
			if err != nil || parsed < 1 || parsed > int(^uint32(0)>>1)/int(store.AdminStickerSetPageSize) {
				http.Error(w, "invalid page", http.StatusBadRequest)
				return
			}
			page = parsed
		}
		reportSetID, reportPage, err := parseAdminStickerSetReportPage(r.URL.Query())
		if err != nil {
			http.Error(w, "invalid report page", http.StatusBadRequest)
			return
		}
		sessionCookie, err := r.Cookie(sessionCookieName)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		csrfToken, err := SessionCSRFToken(cfg.TokenHash, sessionCookie.Value)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		offset := int32((page - 1) * int(store.AdminStickerSetPageSize))
		sets, err := cfg.Store.AdminUserStickerSetsPage(r.Context(), offset, store.AdminStickerSetPageSize+1)
		if err != nil {
			adminStickerSetLog(cfg).Error("list admin user sticker sets", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		events, err := cfg.Store.AdminStickerSetEventsPage(r.Context(), offset, store.AdminStickerSetPageSize+1)
		if err != nil {
			adminStickerSetLog(cfg).Error("list admin sticker-set audit events", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		reportSetIDs, err := cfg.Store.AdminOpenUserStickerSetReportSetIDsPage(r.Context(), offset, store.AdminUserStickerSetReportPageSize+1)
		if err != nil {
			adminStickerSetLog(cfg).Error("list admin open sticker-set report groups", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		hasMoreReportGroups := len(reportSetIDs) > int(store.AdminUserStickerSetReportPageSize)
		if hasMoreReportGroups {
			reportSetIDs = reportSetIDs[:store.AdminUserStickerSetReportPageSize]
		}
		if reportSetID != 0 && !slices.Contains(reportSetIDs, reportSetID) {
			http.Redirect(w, r, "/admin/sticker-sets?notice=no-reports", http.StatusSeeOther)
			return
		}
		reportOffset := int32(0)
		if reportSetID != 0 {
			reportOffset = (reportPage - 1) * store.AdminUserStickerSetReportsPerGroupPageSize
		}
		reports, err := cfg.Store.AdminOpenUserStickerSetReportsForSets(
			r.Context(), reportSetIDs, reportSetID, reportOffset, store.AdminUserStickerSetReportsPerGroupPageSize+1,
		)
		if err != nil {
			adminStickerSetLog(cfg).Error("list admin open sticker-set reports", "err", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		data := adminStickerSetsPageData{
			CSRFToken: csrfToken, Page: page, PreviousPage: page - 1, NextPage: page + 1,
			HasPrevious: page > 1,
			HasNext:     len(sets) > int(store.AdminStickerSetPageSize) || len(events) > int(store.AdminStickerSetPageSize) || hasMoreReportGroups,
			Notice:      adminStickerSetNotice(r.URL.Query().Get("notice")),
		}
		if len(sets) > int(store.AdminStickerSetPageSize) {
			sets = sets[:store.AdminStickerSetPageSize]
		}
		if len(events) > int(store.AdminStickerSetPageSize) {
			events = events[:store.AdminStickerSetPageSize]
		}
		for _, set := range sets {
			data.Sets = append(data.Sets, adminStickerSetRow{
				ID: set.ID, Title: set.Title, ShortName: set.ShortName, CreatorID: set.CreatorID,
				CreatedAt: set.CreatedAt.Format(time.RFC3339), Software: set.Software, IsLive: set.IsLive,
			})
		}
		for _, event := range events {
			data.Events = append(data.Events, adminStickerSetEventRow{
				Action: event.Action, SetID: event.SetID, SetTitle: event.SetTitle,
				SetShortName: event.SetShortName, Reason: event.Reason,
				SessionFingerprint: hex.EncodeToString(event.SessionFingerprint),
				CreatedAt:          event.CreatedAt.Format(time.RFC3339),
			})
		}
		reportsBySet := make(map[int64][]store.AdminUserStickerSetReport, len(reportSetIDs))
		for _, report := range reports {
			reportsBySet[report.SetID] = append(reportsBySet[report.SetID], report)
		}
		for _, setID := range reportSetIDs {
			setReports := reportsBySet[setID]
			if len(setReports) == 0 {
				if setID == reportSetID {
					http.Redirect(w, r, "/admin/sticker-sets?notice=no-reports", http.StatusSeeOther)
					return
				}
				continue
			}
			hasMore := len(setReports) > int(store.AdminUserStickerSetReportsPerGroupPageSize)
			if hasMore {
				setReports = setReports[:store.AdminUserStickerSetReportsPerGroupPageSize]
			}
			groupPage := int32(1)
			if setID == reportSetID {
				groupPage = reportPage
			}
			first := setReports[0]
			group := adminStickerSetReportGroup{
				SetID: first.SetID, SetTitle: first.SetTitle,
				SetShortName: first.SetShortName, SetIsLive: first.SetIsLive,
				Reports: make([]adminStickerSetReportRow, 0, len(setReports)),
				Page:    groupPage, PreviousPage: groupPage - 1, NextPage: groupPage + 1,
				HasPrevious: groupPage > 1, HasNext: hasMore,
			}
			for _, report := range setReports {
				group.Reports = append(group.Reports, adminStickerSetReportRow{
					ID: report.ID, ReporterID: report.ReporterID,
					MessagePeerType: adminStickerSetReportPeerType(report.MessagePeerType),
					MessagePeerID:   report.MessagePeerID, MessageID: report.MessageID,
					Reason: report.Reason, Comment: report.Comment, CreatedAt: report.CreatedAt.Format(time.RFC3339),
				})
			}
			data.ReportGroups = append(data.ReportGroups, group)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if err := adminStickerSetsPageTemplate.Execute(w, data); err != nil {
			adminStickerSetLog(cfg).Error("render admin user sticker sets", "err", err)
		}
	}
}

func parseAdminStickerSetReportPage(query url.Values) (int64, int32, error) {
	setIDValue := query.Get("report_set_id")
	pageValue := query.Get("report_page")
	if setIDValue == "" {
		if pageValue != "" {
			return 0, 0, errors.New("report page requires a set id")
		}
		return 0, 1, nil
	}
	setID, err := strconv.ParseInt(setIDValue, 10, 64)
	if err != nil || setID <= 0 {
		return 0, 0, errors.New("invalid report set id")
	}
	page := int64(1)
	if pageValue != "" {
		page, err = strconv.ParseInt(pageValue, 10, 32)
		maxPage := int64(^uint32(0)>>1) / int64(store.AdminUserStickerSetReportsPerGroupPageSize)
		if err != nil || page < 1 || page > maxPage {
			return 0, 0, errors.New("invalid report page")
		}
	}
	return setID, int32(page), nil
}

func adminStickerSetReportPeerType(peerType store.PeerType) string {
	switch peerType {
	case store.PeerTypeUser:
		return "User"
	case store.PeerTypeChat:
		return "Chat"
	case store.PeerTypeChannel:
		return "Channel"
	default:
		return "Peer"
	}
}

func adminStickerSetAction(cfg LoginHandlerConfig, action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, adminStickerSetActionBodyBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !validAdminSessionCSRF(cfg.TokenHash, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ref, ok := parseAdminStickerSetRef(r.PostForm)
		if !ok {
			http.Error(w, "provide one sticker set id or short name", http.StatusBadRequest)
			return
		}
		sessionCookie, cookieErr := r.Cookie(sessionCookieName)
		if cookieErr != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fingerprint := HashSessionID(sessionCookie.Value)
		var err error
		var unhashedFiles []store.StickerSetUnhashedFile
		switch action {
		case "takedown":
			unhashedFiles, err = cfg.Store.TakedownUserStickerSet(r.Context(), ref, r.PostForm.Get("reason"), fingerprint)
		case "restore":
			err = cfg.Store.RestoreUserStickerSet(r.Context(), ref, r.PostForm.Get("reason"), fingerprint)
		default:
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			switch {
			case errors.Is(err, store.ErrStickerSetAdminInputInvalid):
				http.Error(w, "invalid reason or set reference", http.StatusBadRequest)
			case errors.Is(err, store.ErrStickerSetNotFound):
				http.Error(w, "user sticker set not found", http.StatusNotFound)
			case errors.Is(err, store.ErrStickerSetStateConflict):
				http.Error(w, "sticker set is already in that state", http.StatusConflict)
			case errors.Is(err, store.ErrStickerDocumentInvalid):
				message := "sticker set " + ref.String() + " files are unavailable"
				adminStickerSetLog(cfg).Error(message, "action", action, "set_ref", ref.String(), "err", err)
				http.Error(w, message, http.StatusConflict)
			default:
				adminStickerSetLog(cfg).Error("change user sticker set state", "action", action, "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}
		if action == "takedown" {
			for _, file := range unhashedFiles {
				adminStickerSetLog(cfg).Warn("user sticker set taken down with unhashed file",
					"set_ref", ref.String(), "file_id", file.FileID,
					"unhashed_files", len(unhashedFiles), "err", file.Err)
			}
		}
		if action == "restore" {
			http.Redirect(w, r, "/admin/sticker-sets?notice=restore", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/admin/sticker-sets?notice=takedown", http.StatusSeeOther)
	}
}

func adminStickerSetReportDismiss(cfg LoginHandlerConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, adminStickerSetActionBodyBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		if !validAdminSessionCSRF(cfg.TokenHash, r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		idValues := r.PostForm["id"]
		if len(idValues) != 1 {
			http.Error(w, "provide one report id", http.StatusBadRequest)
			return
		}
		reportID, err := strconv.ParseInt(strings.TrimSpace(idValues[0]), 10, 64)
		if err != nil || reportID <= 0 {
			http.Error(w, "invalid report id", http.StatusBadRequest)
			return
		}
		if err := cfg.Store.DismissUserStickerSetReport(r.Context(), reportID); err != nil {
			switch {
			case errors.Is(err, store.ErrUserStickerSetReportInputInvalid):
				http.Error(w, "invalid report id", http.StatusBadRequest)
			case errors.Is(err, store.ErrUserStickerSetReportNotFound):
				http.Error(w, "open report not found", http.StatusNotFound)
			default:
				adminStickerSetLog(cfg).Error("dismiss user sticker-set report", "report_id", reportID, "err", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
			return
		}
		http.Redirect(w, r, "/admin/sticker-sets?notice=dismiss", http.StatusSeeOther)
	}
}

func validAdminSessionCSRF(tokenHash string, r *http.Request) bool {
	if tokenHash == "" {
		return false
	}
	sessionCookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	got := r.PostForm.Get("csrf_token")
	expected, err := SessionCSRFToken(tokenHash, sessionCookie.Value)
	if err != nil || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1
}

func parseAdminStickerSetRef(form map[string][]string) (store.StickerSetAdminRef, bool) {
	idValues := form["id"]
	shortNameValues := form["short_name"]
	if len(idValues) > 1 || len(shortNameValues) > 1 {
		return nil, false
	}
	idText := ""
	if len(idValues) == 1 {
		idText = strings.TrimSpace(idValues[0])
	}
	shortName := ""
	if len(shortNameValues) == 1 {
		shortName = strings.TrimSpace(shortNameValues[0])
	}
	if idText != "" && shortName != "" {
		return nil, false
	}
	if idText != "" {
		id, err := strconv.ParseInt(idText, 10, 64)
		if err != nil || id <= 0 {
			return nil, false
		}
		ref, err := store.StickerSetAdminRefByID(id)
		return ref, err == nil
	}
	if shortName == "" || len(shortName) > 64 {
		return nil, false
	}
	ref, err := store.StickerSetAdminRefByShortName(shortName)
	return ref, err == nil
}

func adminStickerSetNotice(value string) string {
	switch value {
	case "takedown":
		return "Sticker set taken down."
	case "restore":
		return "Sticker set restored."
	case "dismiss":
		return "Report dismissed."
	case "no-reports":
		return "That sticker set has no open reports."
	default:
		return ""
	}
}

func adminStickerSetLog(cfg LoginHandlerConfig) *slog.Logger {
	if cfg.Logger != nil {
		return cfg.Logger
	}
	return slog.Default()
}
