package linkselector

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/teagramhq/teagram-server/internal/linklanding"
)

const admissionLandingBody = `<!doctype html><html lang="en"><body><main>Open this in Telegramd</main></body></html>`
const realTransportAsset = "real-transport-asset"

func TestWebAssetWaitsForStagingCapacityAndPreservesResponse(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	returned := make(chan string, 3)
	assetBody := newAdmissionBody("unchanged-asset")
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		returned <- request.URL.Path
		switch request.URL.Path {
		case "/holder-one.js", "/holder-two.js":
			return admissionResponse(request, "holder", maxWebBodyBytes, http.Header{"Content-Type": {"text/javascript"}}), nil
		case "/asset.js":
			response := admissionResponse(request, "unchanged-asset", int64(len("unchanged-asset")), http.Header{
				"Content-Type":            {"application/javascript"},
				"Content-Security-Policy": {"default-src 'self'"},
				"Cache-Control":           {"public, max-age=60"},
				"Content-Length":          {"15"},
				"X-Asset-Revision":        {"rev-7"},
			})
			response.Body = assetBody
			return response, nil
		default:
			t.Errorf("unexpected upstream path %q", request.URL.Path)
			return nil, io.EOF
		}
	})

	writeStarted := make(chan struct{}, 2)
	releaseWrites := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrites) }) }
	defer release()
	type result struct {
		path string
		code int
		body string
		head http.Header
	}
	finished := make(chan result, 3)
	for _, path := range []string{"/holder-one.js", "/holder-two.js"} {
		go func() {
			recorder := httptest.NewRecorder()
			selectorHandler.ServeHTTP(&gatedResponseWriter{ResponseRecorder: recorder, writeStarted: writeStarted, release: releaseWrites}, admissionRequest(path, context.Background()))
			finished <- result{path: path, code: recorder.Code, body: recorder.Body.String(), head: recorder.Header().Clone()}
		}()
	}
	for range 2 {
		waitSignal(t, writeStarted, "holder downstream write")
	}
	for range 2 {
		waitSignal(t, returned, "holder upstream response")
	}

	assetFinished := make(chan result, 1)
	go func() {
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest("/asset.js", context.Background()))
		assetFinished <- result{path: "/asset.js", code: recorder.Code, body: recorder.Body.String(), head: recorder.Header().Clone()}
	}()
	waitValue(t, returned, "/asset.js", "asset upstream response")
	waitForAdmissionWaiter(t, selectorHandler)
	select {
	case got := <-assetFinished:
		release()
		t.Fatalf("asset did not wait for held staging capacity: %+v", got)
	case <-time.After(15 * time.Millisecond):
	}
	if assetBody.reads.Load() != 0 {
		t.Fatalf("asset body reads before admission = %d, want zero", assetBody.reads.Load())
	}

	release()
	var completed []result
	for range 3 {
		select {
		case got := <-finished:
			completed = append(completed, got)
		case got := <-assetFinished:
			completed = append(completed, got)
		case <-time.After(time.Second):
			t.Fatal("requests did not complete after staging capacity was released")
		}
	}
	var asset result
	for _, got := range completed {
		if got.path == "/asset.js" {
			asset = got
		}
	}
	if asset.code != http.StatusOK || asset.body != "unchanged-asset" {
		t.Errorf("asset response = %d %q, want unchanged 200 response", asset.code, asset.body)
	}
	for name, want := range map[string]string{
		"Content-Type":            "application/javascript",
		"Content-Security-Policy": "default-src 'self'",
		"Cache-Control":           "public, max-age=60",
		"Content-Length":          "15",
		"X-Asset-Revision":        "rev-7",
	} {
		if got := asset.head.Get(name); got != want {
			t.Errorf("asset %s = %q, want %q", name, got, want)
		}
	}
	if assetBody.reads.Load() == 0 {
		t.Error("asset body was not read after capacity became available")
	}
	assertAdmissionLogs(t, logs.Bytes(), validator, 3, "ok")
}

func TestWebAdmissionDoesNotLetLaterSmallAssetPassLargeWaiter(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	returned := make(chan string, 2)
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		returned <- request.URL.Path
		switch request.URL.Path {
		case "/large.js":
			return admissionResponse(request, "large", maxWebBodyBytes, nil), nil
		case "/small.js":
			return admissionResponse(request, "small", maxWebBodyBytes/8, nil), nil
		default:
			t.Errorf("unexpected upstream path %q", request.URL.Path)
			return nil, io.EOF
		}
	})

	if err := selectorHandler.stagedWebBodies.Acquire(context.Background(), maxStagedWebBodyBytes/2); err != nil {
		t.Fatalf("reserve first holder: %v", err)
	}
	if err := selectorHandler.stagedWebBodies.Acquire(context.Background(), maxStagedWebBodyBytes/2); err != nil {
		selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes / 2)
		t.Fatalf("reserve second holder: %v", err)
	}
	defer selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes / 2)
	largeWriteStarted := make(chan struct{}, 1)
	largeWriteRelease := make(chan struct{})
	largeFinished := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(&gatedResponseWriter{ResponseRecorder: recorder, writeStarted: largeWriteStarted, release: largeWriteRelease}, admissionRequest("/large.js", context.Background()))
		largeFinished <- recorder.Code
	}()
	waitValue(t, returned, "/large.js", "large waiter upstream response")
	waitForAdmissionWaiter(t, selectorHandler)
	select {
	case code := <-largeFinished:
		t.Fatalf("large waiter returned before capacity release: status %d", code)
	case <-time.After(15 * time.Millisecond):
	}

	smallFinished := make(chan int, 1)
	go func() {
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest("/small.js", context.Background()))
		smallFinished <- recorder.Code
	}()
	waitValue(t, returned, "/small.js", "small waiter upstream response")
	select {
	case code := <-smallFinished:
		t.Fatalf("later small waiter passed the earlier large waiter before release: status %d", code)
	case <-time.After(15 * time.Millisecond):
	}

	selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes / 2)
	waitSignal(t, largeWriteStarted, "large waiter downstream write")
	select {
	case code := <-smallFinished:
		t.Fatalf("small waiter passed the blocked large holder: status %d", code)
	case <-time.After(15 * time.Millisecond):
	}
	close(largeWriteRelease)
	select {
	case code := <-largeFinished:
		if code != http.StatusOK {
			t.Errorf("large response status = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("large waiter did not finish after downstream release")
	}
	select {
	case code := <-smallFinished:
		if code != http.StatusOK {
			t.Errorf("small response status = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("small waiter did not finish after the large reservation released")
	}
	assertAdmissionLogs(t, logs.Bytes(), validator, 2, "ok")
}

func TestWebUpstreamDoesNotReceiveBrowserAcceptEncoding(t *testing.T) {
	var mu sync.Mutex
	var encodings []string
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		encodings = append(encodings, r.Header.Get("Accept-Encoding"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Security-Policy", "default-src 'self'")
		if _, err := io.WriteString(w, "byte-identical-small-asset"); err != nil {
			t.Errorf("write small-asset fixture: %v", err)
		}
	}))
	t.Cleanup(web.Close)
	landing := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(landing.Close)
	selectorHandler := newAdmissionTestSelectorWithOrigins(t, web.URL, landing.URL, slog.New(slog.DiscardHandler))

	const requests = 8
	writeStarted := make(chan struct{}, requests)
	releaseWrites := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseWrites) }) }
	defer release()
	type response struct {
		code int
		body string
		head http.Header
	}
	finished := make(chan response, requests)
	for i := range requests {
		go func(i int) {
			recorder := httptest.NewRecorder()
			request := admissionRequest(fmt.Sprintf("/asset-%d.js", i), context.Background())
			request.Header.Set("Accept-Encoding", "gzip, br")
			selectorHandler.ServeHTTP(&gatedResponseWriter{ResponseRecorder: recorder, writeStarted: writeStarted, release: releaseWrites}, request)
			finished <- response{code: recorder.Code, body: recorder.Body.String(), head: recorder.Header().Clone()}
		}(i)
	}
	for range requests {
		waitSignal(t, writeStarted, "concurrent small-asset downstream write")
	}
	mu.Lock()
	gotEncodings := append([]string(nil), encodings...)
	mu.Unlock()
	if len(gotEncodings) != requests {
		t.Fatalf("Web requests = %d, want %d", len(gotEncodings), requests)
	}
	for _, got := range gotEncodings {
		if got != "" {
			t.Errorf("Web received Accept-Encoding %q, want it omitted", got)
		}
	}
	release()
	for range requests {
		select {
		case got := <-finished:
			if got.code != http.StatusOK || got.body != "byte-identical-small-asset" {
				t.Errorf("small asset response = %d %q", got.code, got.body)
			}
			if got.head.Get("Content-Type") != "application/javascript" ||
				got.head.Get("Content-Security-Policy") != "default-src 'self'" ||
				got.head.Get("Content-Length") != "26" {
				t.Errorf("small asset response headers = %v", got.head)
			}
		case <-time.After(time.Second):
			t.Fatal("small-asset request did not finish")
		}
	}
}

func TestWebAdmissionReleasesBudgetAcrossFiftyMixedSizeRounds(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	weights := []int64{10 << 20, 12 << 20, 8 << 20, 6 << 20, 4 << 20, 10 << 20, 12 << 20, 6 << 20}
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		for index, weight := range weights {
			if strings.HasSuffix(request.URL.Path, "-asset-"+string(rune('0'+index))+".js") {
				return admissionResponse(request, request.URL.Path, weight, http.Header{"Content-Type": {"application/javascript"}}), nil
			}
		}
		t.Errorf("unexpected mixed-size request path %q", request.URL.Path)
		return nil, io.EOF
	})

	for round := range 50 {
		type result struct {
			path string
			code int
			body string
		}
		finished := make(chan result, len(weights))
		for index := range weights {
			path := fmt.Sprintf("/round-%02d-asset-%d.js", round, index)
			go func(path string) {
				recorder := httptest.NewRecorder()
				selectorHandler.ServeHTTP(recorder, admissionRequest(path, context.Background()))
				finished <- result{path: path, code: recorder.Code, body: recorder.Body.String()}
			}(path)
		}
		for range weights {
			select {
			case got := <-finished:
				if got.code != http.StatusOK || got.body != got.path {
					t.Errorf("round %d response = %d %q, want 200 with unchanged body %q", round, got.code, got.body, got.path)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("round %d did not complete all mixed-size assets", round)
			}
		}
		if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
			t.Fatalf("round %d left part of the aggregate staging allowance reserved", round)
		}
		selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
		if selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes + 1) {
			selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes + 1)
			t.Fatalf("round %d increased the aggregate staging allowance", round)
		}
	}
	assertAdmissionLogs(t, logs.Bytes(), validator, 50*len(weights), "ok")
}

func TestWebWaitingCancellationClosesUnreadResponse(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	selectorHandler.admissionTimeout = time.Second
	releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
	defer releaseHold()
	body := newAdmissionBody("must-not-be-read")
	returned := make(chan struct{}, 1)
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		returned <- struct{}{}
		return admissionResponseBody(request, body, int64(len("must-not-be-read")), nil), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code     int
		body     string
		head     http.Header
		recorder *httptest.ResponseRecorder
	}
	finished := make(chan result, 1)
	go func() {
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest("/waiting.js", ctx))
		finished <- result{code: recorder.Code, body: recorder.Body.String(), head: recorder.Header().Clone(), recorder: recorder}
	}()
	waitSignal(t, returned, "waiting response headers")
	waitForAdmissionWaiter(t, selectorHandler)
	cancel()
	got := waitSignal(t, finished, "canceled admission response")
	if got.code != http.StatusBadGateway || got.body != landingUnavailable {
		t.Errorf("canceled response = %d %q, want fixed 502", got.code, got.body)
	}
	assertUnavailableHeaders(t, got.head)
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Errorf("waiting response reads/closes = %d/%d, want zero/one", body.reads.Load(), body.closes.Load())
	}
	releaseHold()
	if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
		t.Fatal("full staging allowance was not available after cancel and holder release")
	}
	selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
	if got.recorder.Code != http.StatusBadGateway || got.recorder.Body.String() != landingUnavailable {
		t.Errorf("canceled response changed after capacity release: %d %q", got.recorder.Code, got.recorder.Body.String())
	}
	assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonClientCanceled, http.StatusBadGateway, http.StatusOK)
}

func TestWebAdmissionWaitDeadlineStartsWithUpstreamRequest(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	selectorHandler.admissionTimeout = 200 * time.Millisecond
	releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
	defer releaseHold()
	body := newAdmissionBody("unread")
	returned := make(chan struct{}, 1)
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		returned <- struct{}{}
		return admissionResponseBody(request, body, -1, nil), nil
	})
	started := time.Now()
	recorder := httptest.NewRecorder()
	selectorHandler.ServeHTTP(recorder, admissionRequest("/unknown-length.js", context.Background()))
	elapsed := time.Since(started)
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != landingUnavailable {
		t.Errorf("timed-out response = %d %q, want fixed 502", recorder.Code, recorder.Body.String())
	}
	if elapsed < 180*time.Millisecond || elapsed > 270*time.Millisecond {
		t.Errorf("admission returned after %s, want deadline measured from upstream request start", elapsed)
	}
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Errorf("timed-out unknown body reads/closes = %d/%d, want zero/one", body.reads.Load(), body.closes.Load())
	}
	waitSignal(t, returned, "delayed upstream response")
	releaseHold()
	if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
		t.Fatal("full staging allowance was not available after timeout")
	}
	selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
	assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonStagingBudget, http.StatusBadGateway, http.StatusOK)
}

func TestWebAdmissionHonorsEarlierIncomingDeadline(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	selectorHandler.admissionTimeout = time.Second
	releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
	defer releaseHold()
	body := newAdmissionBody("unread")
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return admissionResponseBody(request, body, -1, nil), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	recorder := httptest.NewRecorder()
	selectorHandler.ServeHTTP(recorder, admissionRequest("/deadline.js", ctx))
	if elapsed := time.Since(started); elapsed < 25*time.Millisecond || elapsed > 300*time.Millisecond {
		t.Errorf("incoming deadline returned after %s, want its earlier deadline", elapsed)
	}
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != landingUnavailable {
		t.Errorf("deadline response = %d %q, want fixed 502", recorder.Code, recorder.Body.String())
	}
	if body.reads.Load() != 0 || body.closes.Load() != 1 {
		t.Errorf("deadline body reads/closes = %d/%d, want zero/one", body.reads.Load(), body.closes.Load())
	}
	releaseHold()
	if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
		t.Fatal("full staging allowance was not available after incoming deadline")
	}
	selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
	assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonDeadline, http.StatusBadGateway, http.StatusOK)
}

func TestWebCancellationDuringBodyReadReleasesReservation(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	selectorHandler := newAdmissionTestSelector(t, logger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &cancelingAdmissionBody{cancel: cancel}
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return admissionResponseBody(request, body, 1, nil), nil
	})
	recorder := httptest.NewRecorder()
	selectorHandler.ServeHTTP(recorder, admissionRequest("/read-cancel.js", ctx))
	cancel()
	if recorder.Code != http.StatusBadGateway || recorder.Body.String() != landingUnavailable {
		t.Errorf("read cancellation response = %d %q, want fixed 502", recorder.Code, recorder.Body.String())
	}
	if body.reads.Load() == 0 || body.closes.Load() != 1 {
		t.Errorf("read cancellation body reads/closes = %d/%d, want a read and one close", body.reads.Load(), body.closes.Load())
	}
	if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
		t.Fatal("reservation was not released after body-read cancellation")
	}
	selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
	assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonClientCanceled, http.StatusBadGateway, http.StatusOK)
}

func TestWebAdmissionCancellationReleaseRaceIsAtomic(t *testing.T) {
	selectorHandler := newAdmissionTestSelector(t, slog.New(slog.DiscardHandler))
	bodies := make(chan *admissionBody, 1)
	selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		body := newAdmissionBody("complete-body")
		bodies <- body
		return admissionResponseBody(request, body, int64(len("complete-body")), nil), nil
	})
	type result struct {
		code int
		body string
	}
	for round := range 1000 {
		releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan result, 1)
		go func() {
			recorder := httptest.NewRecorder()
			selectorHandler.ServeHTTP(recorder, admissionRequest("/race.js", ctx))
			finished <- result{code: recorder.Code, body: recorder.Body.String()}
		}()
		body := waitSignal(t, bodies, "race response body")
		barrier := make(chan struct{})
		released := make(chan struct{})
		go func() {
			<-barrier
			cancel()
		}()
		go func() {
			<-barrier
			releaseHold()
			close(released)
		}()
		close(barrier)
		got := waitSignal(t, finished, "race request completion")
		cancel()
		switch got.code {
		case http.StatusOK:
			if got.body != "complete-body" {
				t.Fatalf("round %d returned partial success body %q", round, got.body)
			}
			if body.reads.Load() == 0 {
				t.Fatalf("round %d returned success without staging the response body", round)
			}
		case http.StatusBadGateway:
			if got.body != landingUnavailable {
				t.Fatalf("round %d returned partial failure body %q", round, got.body)
			}
			if body.reads.Load() != 0 {
				t.Fatalf("round %d read the body before returning fixed cancellation response", round)
			}
		default:
			t.Fatalf("round %d returned %d %q, want complete 200 or fixed 502", round, got.code, got.body)
		}
		if body.closes.Load() != 1 {
			t.Fatalf("round %d response body closes = %d, want exactly one", round, body.closes.Load())
		}
		waitSignal(t, released, "holder release")
		if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
			t.Fatalf("round %d left staging capacity reserved", round)
		}
		selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
	}
}

func TestWebOversizeResponsesKeepAdmissionClassification(t *testing.T) {
	t.Run("known length rejects before admission", func(t *testing.T) {
		logger, logs, validator := newDiagnosticLogger()
		selectorHandler := newAdmissionTestSelector(t, logger)
		selectorHandler.admissionTimeout = time.Second
		releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
		defer releaseHold()
		body := newAdmissionBody("body-too-large")
		selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return admissionResponseBody(request, body, maxWebBodyBytes+1, nil), nil
		})
		started := time.Now()
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest("/known-large.js", context.Background()))
		if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
			t.Errorf("known oversize response waited %s with full budget, want prompt rejection", elapsed)
		}
		if recorder.Code != http.StatusBadGateway || recorder.Body.String() != landingUnavailable {
			t.Errorf("known oversize response = %d %q, want fixed 502", recorder.Code, recorder.Body.String())
		}
		if body.reads.Load() != 0 || body.closes.Load() != 1 {
			t.Errorf("known oversize body reads/closes = %d/%d, want zero/one", body.reads.Load(), body.closes.Load())
		}
		assertUnavailableHeaders(t, recorder.Header())
		releaseHold()
		if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
			t.Fatal("known oversize rejection changed the available staging allowance")
		}
		selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
		assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonBodyTooLarge, http.StatusBadGateway, http.StatusOK)
	})

	t.Run("unknown length reads only through the limit", func(t *testing.T) {
		logger, logs, validator := newDiagnosticLogger()
		selectorHandler := newAdmissionTestSelector(t, logger)
		body := &generatedAdmissionBody{remaining: maxWebBodyBytes + 4096}
		selectorHandler.client.Transport = roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			return admissionResponseBody(request, body, -1, nil), nil
		})
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest("/unknown-large.js", context.Background()))
		if recorder.Code != http.StatusBadGateway || recorder.Body.String() != landingUnavailable {
			t.Errorf("unknown oversize response = %d %q, want fixed 502", recorder.Code, recorder.Body.String())
		}
		if got := body.readBytes.Load(); got != maxWebBodyBytes+1 {
			t.Errorf("unknown oversize bytes read = %d, want bounded %d", got, maxWebBodyBytes+1)
		}
		if body.closes.Load() != 1 {
			t.Errorf("unknown oversize body closes = %d, want one", body.closes.Load())
		}
		assertUnavailableHeaders(t, recorder.Header())
		if !selectorHandler.stagedWebBodies.TryAcquire(maxStagedWebBodyBytes) {
			t.Fatal("unknown oversize read leaked staging capacity")
		}
		selectorHandler.stagedWebBodies.Release(maxStagedWebBodyBytes)
		assertAdmissionFailureLog(t, logs.Bytes(), validator, reasonBodyTooLarge, http.StatusBadGateway, http.StatusOK)
	})
}

func TestWebWaitersDoNotBlockLandingRoutesWithRealTransport(t *testing.T) {
	logger, logs, validator := newDiagnosticLogger()
	webRequests := make(chan string, 9)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := realTransportAsset
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write Web fixture: %v", err)
		}
		webRequests <- r.URL.Path
	}))
	t.Cleanup(web.Close)
	landingRequests := make(chan string, 5)
	landingHandler := linklanding.NewHandler(slog.New(slog.DiscardHandler))
	landing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		landingRequests <- r.URL.RequestURI()
		landingHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(landing.Close)
	selectorHandler := newAdmissionTestSelectorWithOrigins(t, web.URL, landing.URL, logger)
	transport, ok := selectorHandler.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("selector transport is %T, want *http.Transport", selectorHandler.client.Transport)
	}
	if transport.MaxConnsPerHost != 8 {
		t.Fatalf("Web transport MaxConnsPerHost = %d, want existing cap 8", transport.MaxConnsPerHost)
	}
	selectorHandler.admissionTimeout = 3 * time.Second
	releaseHold := holdStagingCapacity(t, selectorHandler, maxStagedWebBodyBytes)
	type result struct {
		path string
		code int
		body string
	}
	webFinished := make(chan result, 9)
	for index := range 9 {
		path := fmt.Sprintf("/asset-%d.js", index)
		go func(path string) {
			recorder := httptest.NewRecorder()
			selectorHandler.ServeHTTP(recorder, admissionRequest(path, context.Background()))
			webFinished <- result{path: path, code: recorder.Code, body: recorder.Body.String()}
		}(path)
	}
	for range 8 {
		waitSignal(t, webRequests, "one of eight Web upstream responses")
	}
	select {
	case path := <-webRequests:
		t.Fatalf("ninth Web request reached upstream before a connection was released: %q", path)
	case <-time.After(40 * time.Millisecond):
	}
	select {
	case got := <-webFinished:
		t.Fatalf("Web waiter completed while the aggregate staging allowance was held: %+v", got)
	case <-time.After(20 * time.Millisecond):
	}

	routes := []struct {
		target, upstreamPath, wantBody string
		status                         int
	}{
		{target: "/+synthetic-invite", upstreamPath: "/+redacted", wantBody: admissionLandingBody, status: http.StatusOK},
		{target: "/syntheticname", upstreamPath: "/redacted", wantBody: admissionLandingBody, status: http.StatusOK},
		{target: "/syntheticname/123", upstreamPath: "/redacted/1", wantBody: admissionLandingBody, status: http.StatusOK},
		{target: "/admin", upstreamPath: "/admin", wantBody: `<!doctype html><html lang="en"><body><main>Not found</main></body></html>`, status: http.StatusNotFound},
		{target: "/healthz/", upstreamPath: "/", wantBody: admissionLandingBody, status: http.StatusOK},
	}
	for _, route := range routes {
		started := time.Now()
		recorder := httptest.NewRecorder()
		selectorHandler.ServeHTTP(recorder, admissionRequest(route.target, context.Background()))
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Errorf("landing route %s took %s with Web waiters active", route.target, elapsed)
		}
		if recorder.Code != route.status || recorder.Body.String() != route.wantBody {
			t.Errorf("landing route %s = %d %q, want %d %q", route.target, recorder.Code, recorder.Body.String(), route.status, route.wantBody)
		}
		if got := waitSignal(t, landingRequests, "canonical landing request"); got != route.upstreamPath {
			t.Errorf("landing route %s reached upstream as %q, want %q", route.target, got, route.upstreamPath)
		}
	}

	releaseHold()
	for range 9 {
		select {
		case got := <-webFinished:
			if got.code != http.StatusOK || got.body != realTransportAsset {
				t.Errorf("real Web response %s = %d %q, want unchanged 200 body", got.path, got.code, got.body)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Web response waiters did not drain after capacity release")
		}
	}
	waitSignal(t, webRequests, "ninth Web request after a connection was released")
	if failures := validator.failures(); len(failures) != 0 {
		t.Errorf("strict log validation failed: %s", strings.Join(failures, "; "))
	}
	if records := diagnosticCompletionRecords(t, logs.Bytes()); len(records) != 14 {
		t.Errorf("completion records = %d, want nine Web and five landing records", len(records))
	}
}

func newAdmissionTestSelector(t *testing.T, logger *slog.Logger) *selector {
	t.Helper()
	return newAdmissionTestSelectorWithOrigins(t, "http://web.example", "http://landing.example", logger)
}

func newAdmissionTestSelectorWithOrigins(t *testing.T, webOrigin, landingOrigin string, logger *slog.Logger) *selector {
	t.Helper()
	handler, err := NewHandler(webOrigin, landingOrigin, logger)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	selectorHandler, ok := handler.(*selector)
	if !ok {
		t.Fatalf("NewHandler returned %T, want *selector", handler)
	}
	return selectorHandler
}

func admissionRequest(path string, ctx context.Context) *http.Request {
	return (&http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{},
		RequestURI: path,
		Header:     make(http.Header),
		Body:       http.NoBody,
	}).WithContext(ctx)
}

func admissionResponse(request *http.Request, body string, contentLength int64, header http.Header) *http.Response {
	return admissionResponseBody(request, newAdmissionBody(body), contentLength, header)
}

func admissionResponseBody(request *http.Request, body io.ReadCloser, contentLength int64, header http.Header) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          body,
		ContentLength: contentLength,
		Request:       request,
	}
}

type admissionBody struct {
	reader *strings.Reader
	reads  atomic.Int32
	closes atomic.Int32
}

func newAdmissionBody(body string) *admissionBody {
	return &admissionBody{reader: strings.NewReader(body)}
}

func (b *admissionBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.reader.Read(p)
}

func (b *admissionBody) Close() error {
	b.closes.Add(1)
	return nil
}

func waitSignal[T any](t *testing.T, signal <-chan T, description string) T {
	t.Helper()
	select {
	case value := <-signal:
		return value
	case <-time.After(time.Second):
		var zero T
		t.Fatalf("timed out waiting for %s", description)
		return zero
	}
}

func waitValue[T comparable](t *testing.T, signal <-chan T, want T, description string) {
	t.Helper()
	if got := waitSignal(t, signal, description); got != want {
		t.Fatalf("%s = %v, want %v", description, got, want)
	}
}

func waitForAdmissionWaiter(t *testing.T, selectorHandler *selector) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		if !selectorHandler.stagedWebBodies.TryAcquire(0) {
			return
		}
		select {
		case <-ticker.C:
		case <-timer.C:
			t.Fatal("no request reached the staging admission queue")
		}
	}
}

func assertAdmissionLogs(t *testing.T, logs []byte, validator *selectorDiagnosticValidator, count int, reason string) {
	t.Helper()
	records := diagnosticCompletionRecords(t, logs)
	if len(records) != count {
		t.Fatalf("completion records = %d, want %d: %s", len(records), count, logs)
	}
	for _, record := range records {
		if record["route_class"] != "web" || record["reason"] != reason || record["status"] != float64(http.StatusOK) {
			t.Errorf("completion record = %v, want web/%s status 200", record, reason)
		}
	}
	if failures := validator.failures(); len(failures) != 0 {
		t.Errorf("strict log validation failed: %s", strings.Join(failures, "; "))
	}
}

func holdStagingCapacity(t *testing.T, selectorHandler *selector, amount int64) func() {
	t.Helper()
	if err := selectorHandler.stagedWebBodies.Acquire(context.Background(), amount); err != nil {
		t.Fatalf("hold %d bytes of staging capacity: %v", amount, err)
	}
	var once sync.Once
	release := func() { once.Do(func() { selectorHandler.stagedWebBodies.Release(amount) }) }
	t.Cleanup(release)
	return release
}

func assertAdmissionFailureLog(t *testing.T, logs []byte, validator *selectorDiagnosticValidator, reason completionReason, status, upstreamStatus int) {
	t.Helper()
	records := diagnosticCompletionRecords(t, logs)
	if len(records) != 1 {
		t.Fatalf("completion records = %d, want one: %s", len(records), logs)
	}
	record := records[0]
	if record["route_class"] != "unavailable" || record["reason"] != string(reason) ||
		record["status"] != float64(status) || record["upstream_status"] != float64(upstreamStatus) {
		t.Errorf("completion record = %v, want unavailable/%s %d/%d", record, reason, status, upstreamStatus)
	}
	if failures := validator.failures(); len(failures) != 0 {
		t.Errorf("strict log validation failed: %s", strings.Join(failures, "; "))
	}
}

func assertUnavailableHeaders(t *testing.T, header http.Header) {
	t.Helper()
	want := map[string]string{
		"Cache-Control":           "no-store",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"Content-Type":            "text/html; charset=utf-8",
		"Content-Security-Policy": "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
	}
	for name, value := range want {
		if got := header.Get(name); got != value {
			t.Errorf("fixed failure %s = %q, want %q", name, got, value)
		}
	}
}

type cancelingAdmissionBody struct {
	cancel context.CancelFunc
	reads  atomic.Int32
	closes atomic.Int32
}

func (b *cancelingAdmissionBody) Read([]byte) (int, error) {
	b.reads.Add(1)
	b.cancel()
	return 0, context.Canceled
}

func (b *cancelingAdmissionBody) Close() error {
	b.closes.Add(1)
	return nil
}

type generatedAdmissionBody struct {
	remaining int64
	readBytes atomic.Int64
	closes    atomic.Int32
}

func (b *generatedAdmissionBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), b.remaining)
	for index := range n {
		p[index] = 'x'
	}
	b.remaining -= n
	b.readBytes.Add(n)
	return int(n), nil
}

func (b *generatedAdmissionBody) Close() error {
	b.closes.Add(1)
	return nil
}
