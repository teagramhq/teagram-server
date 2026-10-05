package linkselector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSelectorCompletionDiagnostics(t *testing.T) {
	tests := []struct {
		name               string
		target             string
		configure          func(*testing.T, *selector, *http.Request)
		writer             func() http.ResponseWriter
		wantRouteClass     string
		wantStatus         int
		wantBody           string
		wantReason         string
		wantUpstreamStatus *int
	}{
		{
			name:               "ok",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), nil),
			wantRouteClass:     "web",
			wantStatus:         http.StatusOK,
			wantBody:           "SENTINEL_UPSTREAM_BODY",
			wantReason:         "ok",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "ok_landing",
			target:             "/+SENTINEL_CAP?x=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), nil),
			wantRouteClass:     "invite",
			wantStatus:         http.StatusOK,
			wantBody:           "<!doctype html><html lang=\"en\"><body><main>Open this in Telegramd</main></body></html>",
			wantReason:         "ok",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:   "client_canceled",
			target: "/main.js?t=SENTINEL_Q",
			configure: func(t *testing.T, _ *selector, request *http.Request) {
				t.Helper()
				ctx, cancel := context.WithCancel(request.Context())
				cancel()
				*request = *request.WithContext(ctx)
			},
			wantStatus:     http.StatusBadGateway,
			wantRouteClass: "unavailable",
			wantBody:       landingUnavailable,
			wantReason:     "client_canceled",
		},
		{
			name:   "deadline",
			target: "/main.js?t=SENTINEL_Q",
			configure: func(t *testing.T, _ *selector, request *http.Request) {
				t.Helper()
				ctx, cancel := context.WithDeadline(request.Context(), time.Now().Add(-time.Second))
				t.Cleanup(cancel)
				*request = *request.WithContext(ctx)
			},
			wantStatus:     http.StatusBadGateway,
			wantRouteClass: "unavailable",
			wantBody:       landingUnavailable,
			wantReason:     "deadline",
		},
		{
			name:           "transport",
			target:         "/main.js?t=SENTINEL_Q",
			configure:      returnDiagnosticError(errors.New("SENTINEL_ERR transport")),
			wantStatus:     http.StatusBadGateway,
			wantRouteClass: "unavailable",
			wantBody:       landingUnavailable,
			wantReason:     "transport",
		},
		{
			name:           "transport_timeout",
			target:         "/main.js?t=SENTINEL_Q",
			configure:      returnDiagnosticError(diagnosticTimeoutError{}),
			wantStatus:     http.StatusBadGateway,
			wantRouteClass: "unavailable",
			wantBody:       landingUnavailable,
			wantReason:     "deadline",
		},
		{
			name:           "message_link_transport",
			target:         "/SENTINEL_USER/1?x=SENTINEL_Q",
			configure:      returnDiagnosticError(errors.New("SENTINEL_ERR landing transport")),
			wantRouteClass: "unavailable",
			wantStatus:     http.StatusServiceUnavailable,
			wantBody:       landingUnavailable,
			wantReason:     "transport",
		},
		{
			name:   "real_url_error_query_redaction",
			target: "/main.js?t=SENTINEL_REAL_Q",
			configure: func(t *testing.T, selectorHandler *selector, _ *http.Request) {
				t.Helper()
				upstream := httptest.NewServer(http.NotFoundHandler())
				origin := upstream.URL
				upstream.Close()
				parsed, err := url.Parse(origin)
				if err != nil {
					t.Fatalf("parse test origin: %v", err)
				}
				probeURL := *parsed
				probeURL.Path = "/main.js"
				probeURL.RawQuery = "t=SENTINEL_REAL_Q"
				probeRequest := &http.Request{Method: http.MethodGet, URL: &probeURL, Header: make(http.Header), Body: http.NoBody}
				probeResponse, probeErr := selectorHandler.client.Do(probeRequest)
				if probeResponse != nil {
					if err := probeResponse.Body.Close(); err != nil {
						t.Fatalf("close probe response: %v", err)
					}
				}
				var urlErr *url.Error
				if !errors.As(probeErr, &urlErr) || !strings.Contains(urlErr.URL, "SENTINEL_REAL_Q") {
					t.Fatalf("client.Do error = %v, want *url.Error containing the Web query sentinel", probeErr)
				}
				selectorHandler.webURL = parsed
			},
			wantStatus:     http.StatusBadGateway,
			wantRouteClass: "unavailable",
			wantBody:       landingUnavailable,
			wantReason:     "transport",
		},
		{
			name:   "upstream_5xx",
			target: "/main.js?t=SENTINEL_Q",
			configure: returnDiagnosticResponse(http.StatusServiceUnavailable, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), &diagnosticBody{
				body: "SENTINEL_UPSTREAM_BODY", closeErr: errors.New("SENTINEL_ERR upstream close"),
			}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "upstream_5xx",
			wantUpstreamStatus: intPointer(http.StatusServiceUnavailable),
		},
		{
			name:               "upstream_status_unmapped",
			target:             "/+SENTINEL_CAP?x=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusTeapot, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), nil),
			wantStatus:         http.StatusServiceUnavailable,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "upstream_status_unmapped",
			wantUpstreamStatus: intPointer(http.StatusTeapot),
		},
		{
			name:               "body_too_large",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "SENTINEL_UPSTREAM_BODY", maxWebBodyBytes+1, nil),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "body_too_large",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "body_read",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "", 1, &diagnosticBody{readErr: errors.New("SENTINEL_ERR body read")}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "body_read",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "body_read_canceled",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "", 1, &diagnosticBody{readErr: context.Canceled}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "client_canceled",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "body_read_deadline",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "", 1, &diagnosticBody{readErr: context.DeadlineExceeded}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "deadline",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "body_read_timeout",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "", 1, &diagnosticBody{readErr: diagnosticTimeoutError{}}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "deadline",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "landing_body_read_deadline",
			target:             "/+SENTINEL_CAP?x=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "", 1, &diagnosticBody{readErr: context.DeadlineExceeded}),
			wantStatus:         http.StatusServiceUnavailable,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "deadline",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:   "landing_body_read_context_canceled",
			target: "/+SENTINEL_CAP?x=SENTINEL_Q",
			configure: func(t *testing.T, selectorHandler *selector, request *http.Request) {
				t.Helper()
				ctx, cancel := context.WithCancel(request.Context())
				t.Cleanup(cancel)
				*request = *request.WithContext(ctx)
				selectorHandler.client.Transport = roundTripperFunc(func(upstreamRequest *http.Request) (*http.Response, error) {
					body := &diagnosticBody{readErr: errors.New("SENTINEL_ERR landing body read"), cancel: cancel}
					return diagnosticResponse(upstreamRequest, http.StatusOK, body, 1), nil
				})
			},
			wantStatus:         http.StatusServiceUnavailable,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "client_canceled",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "body_close",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), &diagnosticBody{body: "SENTINEL_UPSTREAM_BODY", closeErr: errors.New("SENTINEL_ERR body close")}),
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "body_close",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:   "staging_budget_exhausted",
			target: "/main.js?t=SENTINEL_Q",
			configure: func(t *testing.T, selectorHandler *selector, _ *http.Request) {
				t.Helper()
				if err := selectorHandler.stagedWebBodies.Acquire(context.Background(), maxStagedWebBodyBytes); err != nil {
					t.Fatalf("reserve staged body budget: %v", err)
				}
				t.Cleanup(func() { selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes) })
				selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
					return diagnosticResponse(request, http.StatusOK, io.NopCloser(strings.NewReader("asset")), int64(len("asset"))), nil
				})
			},
			wantStatus:         http.StatusBadGateway,
			wantRouteClass:     "unavailable",
			wantBody:           landingUnavailable,
			wantReason:         "staging_budget_exhausted",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
		{
			name:               "downstream_write",
			target:             "/main.js?t=SENTINEL_Q",
			configure:          returnDiagnosticResponse(http.StatusOK, "SENTINEL_UPSTREAM_BODY", int64(len("SENTINEL_UPSTREAM_BODY")), nil),
			writer:             func() http.ResponseWriter { return &diagnosticFailingWriter{header: make(http.Header)} },
			wantStatus:         http.StatusOK,
			wantRouteClass:     "unavailable",
			wantBody:           "",
			wantReason:         "downstream_write",
			wantUpstreamStatus: intPointer(http.StatusOK),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logger, logs, validator := newDiagnosticLogger()
			handler, err := NewHandler("http://SENTINEL_ORIGIN.invalid:8080", "http://SENTINEL_LANDING.invalid:8081", logger)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			selectorHandler, ok := handler.(*selector)
			if !ok {
				t.Fatalf("NewHandler returned %T, want *selector", handler)
			}
			request := diagnosticRequest(test.target)
			if test.configure != nil {
				test.configure(t, selectorHandler, request)
			} else {
				t.Fatal("diagnostic case has no transport setup")
			}

			var response http.ResponseWriter = httptest.NewRecorder()
			if test.writer != nil {
				response = test.writer()
			}
			handler.ServeHTTP(response, request)

			if recorder, ok := response.(*httptest.ResponseRecorder); ok {
				if recorder.Code != test.wantStatus || recorder.Body.String() != test.wantBody {
					t.Errorf("response = %d %q, want %d %q", recorder.Code, recorder.Body.String(), test.wantStatus, test.wantBody)
				}
			} else if failingWriter, ok := response.(*diagnosticFailingWriter); ok {
				if failingWriter.status != test.wantStatus || failingWriter.body.String() != test.wantBody {
					t.Errorf("response = %d %q, want %d %q", failingWriter.status, failingWriter.body.String(), test.wantStatus, test.wantBody)
				}
			}

			records := diagnosticCompletionRecords(t, logs.Bytes())
			lines := bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n"))
			if len(lines) > 3 {
				t.Errorf("log lines = %d, want a constant-bounded maximum of three: %s", len(lines), logs.String())
			}
			if len(records) != 1 {
				t.Fatalf("completion records = %d, want exactly one: %s", len(records), logs.String())
			}
			record := records[0]
			if got := record["reason"]; got != test.wantReason {
				t.Errorf("reason = %v, want %q", got, test.wantReason)
			}
			if got := record["route_class"]; got != test.wantRouteClass {
				t.Errorf("route_class = %v, want %q", got, test.wantRouteClass)
			}
			if got := record["status"]; got != float64(test.wantStatus) {
				t.Errorf("downstream status = %v, want %d", got, test.wantStatus)
			}
			elapsed, ok := record["elapsed_ms"].(float64)
			if !ok || elapsed < 0 || elapsed != float64(int64(elapsed)) {
				t.Errorf("elapsed_ms = %v, want a nonnegative integer", record["elapsed_ms"])
			}
			if test.wantUpstreamStatus == nil {
				if _, ok := record["upstream_status"]; ok {
					t.Errorf("unexpected upstream_status: %v", record["upstream_status"])
				}
			} else if got := record["upstream_status"]; got != float64(*test.wantUpstreamStatus) {
				t.Errorf("upstream_status = %v, want %d", got, *test.wantUpstreamStatus)
			}
			if logs.Len() > 0 {
				for _, sentinel := range []string{
					"SENTINEL_CAP", "SENTINEL_USER", "SENTINEL_Q", "SENTINEL_REAL_Q",
					"SENTINEL_COOKIE", "SENTINEL_AUTH", "SENTINEL_CUSTOM_HEADER",
					"SENTINEL_REQUEST_BODY", "SENTINEL_ORIGIN", "SENTINEL_LANDING",
					"SENTINEL_UPSTREAM_HEADER", "SENTINEL_UPSTREAM_BODY", "SENTINEL_ERR",
					"SENTINEL_DOWNSTREAM_WRITE", "Cookie", "Authorization", "X-Custom-Sentinel",
				} {
					if bytes.Contains(logs.Bytes(), []byte(sentinel)) {
						t.Errorf("raw logs contain %q: %s", sentinel, logs.String())
					}
				}
			}
			if failures := validator.failures(); len(failures) != 0 {
				t.Errorf("strict log validation failed: %s", strings.Join(failures, "; "))
			}
		})
	}
}

func TestSelectorLogValidatorRejectsUnsafeFields(t *testing.T) {
	logger, _, validator := newDiagnosticLogger()
	logger.Info("selector response", "dynamic", "value")
	logger.Info("selector response", "reason", "unlisted")
	logger.Info("selector response", "route_class", "unlisted")
	logger.Info("selector response", "status", float64(http.StatusOK))
	logger.Info("selector response", "elapsed_ms", int64(-1))
	logger.Info("selector response", "cause", errors.New("secret"))
	if got := len(validator.failures()); got != 7 {
		t.Fatalf("validator failures = %d, want 7: %v", got, validator.failures())
	}
}

func returnDiagnosticResponse(status int, body string, contentLength int64, customBody io.ReadCloser) func(*testing.T, *selector, *http.Request) {
	return func(_ *testing.T, selectorHandler *selector, _ *http.Request) {
		selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			var responseBody io.ReadCloser
			if customBody != nil {
				responseBody = customBody
			} else {
				responseBody = io.NopCloser(strings.NewReader(body))
			}
			return diagnosticResponse(request, status, responseBody, contentLength), nil
		})
	}
}

func returnDiagnosticError(err error) func(*testing.T, *selector, *http.Request) {
	return func(_ *testing.T, selectorHandler *selector, _ *http.Request) {
		selectorHandler.client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, err
		})
	}
}

func diagnosticResponse(request *http.Request, status int, body io.ReadCloser, contentLength int64) *http.Response {
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        http.Header{"X-Upstream-Sentinel": {"SENTINEL_UPSTREAM_HEADER"}},
		Body:          body,
		ContentLength: contentLength,
		Request:       request,
	}
}

func diagnosticRequest(target string) *http.Request {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, strings.NewReader("SENTINEL_REQUEST_BODY"))
	request.Header.Set("Cookie", "SENTINEL_COOKIE")
	request.Header.Set("Authorization", "SENTINEL_AUTH")
	request.Header.Set("X-Custom-Sentinel", "SENTINEL_CUSTOM_HEADER")
	return request
}

type diagnosticBody struct {
	body     string
	readErr  error
	closeErr error
	reader   *strings.Reader
	cancel   context.CancelFunc
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	if b.cancel != nil {
		b.cancel()
	}
	if b.readErr != nil {
		return 0, b.readErr
	}
	if b.reader == nil {
		b.reader = strings.NewReader(b.body)
	}
	return b.reader.Read(p)
}

func (b *diagnosticBody) Close() error { return b.closeErr }

type diagnosticFailingWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

type diagnosticTimeoutError struct{}

func (diagnosticTimeoutError) Error() string   { return "SENTINEL_ERR timeout" }
func (diagnosticTimeoutError) Timeout() bool   { return true }
func (diagnosticTimeoutError) Temporary() bool { return true }

func (w *diagnosticFailingWriter) Header() http.Header { return w.header }

func (w *diagnosticFailingWriter) WriteHeader(status int) { w.status = status }

func (w *diagnosticFailingWriter) Write(body []byte) (int, error) {
	return 0, errors.New("SENTINEL_DOWNSTREAM_WRITE")
}

func intPointer(value int) *int { return new(value) }

type selectorDiagnosticValidator struct {
	next  slog.Handler
	state *selectorDiagnosticValidation
}

type selectorDiagnosticValidation struct {
	mu     sync.Mutex
	errors []string
}

func newDiagnosticLogger() (*slog.Logger, *bytes.Buffer, *selectorDiagnosticValidator) {
	var output bytes.Buffer
	validator := &selectorDiagnosticValidator{next: slog.NewJSONHandler(&output, nil), state: &selectorDiagnosticValidation{}}
	return slog.New(validator), &output, validator
}

func (h *selectorDiagnosticValidator) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *selectorDiagnosticValidator) Handle(ctx context.Context, record slog.Record) error {
	record.Attrs(func(attr slog.Attr) bool {
		h.validate(attr)
		return true
	})
	if record.Message != "selector response" && record.Message != "selector response write failed" && record.Message != "selector upstream response close failed" {
		h.addFailure("unknown log message")
	}
	return h.next.Handle(ctx, record)
}

func (h *selectorDiagnosticValidator) WithAttrs(attrs []slog.Attr) slog.Handler {
	for _, attr := range attrs {
		h.validate(attr)
	}
	return &selectorDiagnosticValidator{next: h.next.WithAttrs(attrs), state: h.state}
}

func (h *selectorDiagnosticValidator) WithGroup(name string) slog.Handler {
	if name != "" {
		h.addFailure("groups are not allowed")
	}
	return &selectorDiagnosticValidator{next: h.next.WithGroup(name), state: h.state}
}

func (h *selectorDiagnosticValidator) validate(attr slog.Attr) {
	value := attr.Value.Resolve()
	if value.Kind() == slog.KindAny {
		h.addFailure("KindAny attribute")
	}
	switch attr.Key {
	case "route_class":
		if value.Kind() != slog.KindString || !oneOf(value.String(), "landing_other", "invite", "username", "message", "web", "unavailable") {
			h.addFailure("invalid route_class")
		}
	case "status":
		if value.Kind() != slog.KindInt64 || value.Int64() < 100 || value.Int64() > 599 {
			h.addFailure("invalid status")
		}
	case "reason":
		if value.Kind() != slog.KindString || !oneOf(value.String(), "ok", "client_canceled", "deadline", "transport", "upstream_5xx", "upstream_status_unmapped", "body_too_large", "body_read", "body_close", "staging_budget_exhausted", "downstream_write") {
			h.addFailure("invalid reason")
		}
	case "elapsed_ms":
		if value.Kind() != slog.KindInt64 || value.Int64() < 0 {
			h.addFailure("invalid elapsed_ms")
		}
	case "upstream_status":
		if value.Kind() != slog.KindInt64 || value.Int64() < 100 || value.Int64() > 599 {
			h.addFailure("invalid upstream_status")
		}
	default:
		h.addFailure("unknown attribute key: " + attr.Key)
	}
}

func (h *selectorDiagnosticValidator) addFailure(message string) {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	h.state.errors = append(h.state.errors, message)
}

func (h *selectorDiagnosticValidator) failures() []string {
	h.state.mu.Lock()
	defer h.state.mu.Unlock()
	return append([]string(nil), h.state.errors...)
}

func oneOf(value string, choices ...string) bool {
	return slices.Contains(choices, value)
}

func diagnosticCompletionRecords(t *testing.T, logs []byte) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range bytes.SplitSeq(bytes.TrimSpace(logs), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		if record["msg"] == "selector response" {
			records = append(records, record)
		}
	}
	return records
}
