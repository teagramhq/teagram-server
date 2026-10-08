// Command linkprobe performs the fixed synthetic HTTP checks used by the
// isolated link-edge containers' Docker health checks.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/teagramhq/teagram-server/internal/linklanding"
)

const (
	selectorOrigin        = "http://127.0.0.1:8081"
	landingOrigin         = "http://127.0.0.1:8082"
	probePath             = "/syntheticname"
	maxProbeBody          = 4096
	diagnosticHTTPTimeout = 4 * time.Second
	contentPolicy         = "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: linkprobe landing|selector|diagnostic TARGET|hold")
		os.Exit(2)
	}
	if os.Args[1] == "hold" {
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "usage: linkprobe landing|selector|diagnostic TARGET|hold")
			os.Exit(2)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Args[1] == "diagnostic" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: linkprobe diagnostic selector-synthetic|selector-root|direct-landing")
			os.Exit(2)
		}
		target, ok := diagnosticTarget(os.Args[2])
		if !ok {
			fmt.Fprintln(os.Stderr, "invalid diagnostic target")
			os.Exit(2)
		}
		if runDiagnostic(newDiagnosticClient(), target, os.Stdout) != 0 {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: linkprobe landing|selector|diagnostic TARGET|hold")
		os.Exit(2)
	}

	client := &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   3 * time.Second,
	}
	var healthy bool
	switch os.Args[1] {
	case "landing":
		healthy = checkLanding(client, landingOrigin+probePath)
	case "selector":
		healthy = checkSelector(client, selectorOrigin)
	default:
		fmt.Fprintln(os.Stderr, "usage: linkprobe landing|selector")
		os.Exit(2)
	}
	if !healthy {
		fmt.Fprintln(os.Stderr, "link edge health check failed")
		os.Exit(1)
	}
}

func diagnosticTarget(name string) (string, bool) {
	switch name {
	case "selector-synthetic":
		return "http://selector:8081/syntheticname", true
	case "selector-root":
		return "http://selector:8081/", true
	case "direct-landing":
		return "http://linklanding:8082/syntheticname", true
	default:
		return "", false
	}
}

func newDiagnosticClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{Proxy: nil},
		Timeout:   diagnosticHTTPTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func runDiagnostic(client *http.Client, target string, output io.Writer) int {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		_, writeErr := fmt.Fprintln(output, "transport_error=diagnostic_failure")
		if writeErr != nil {
			return 1
		}
		return 1
	}
	response, err := client.Do(request)
	if err != nil {
		_, writeErr := fmt.Fprintf(output, "transport_error=%s\n", diagnosticErrorCategory(err))
		if writeErr != nil {
			return 1
		}
		return 1
	}
	status := response.StatusCode
	if closeErr := response.Body.Close(); closeErr != nil {
		_, writeErr := fmt.Fprintln(output, "transport_error=connect_failure")
		if writeErr != nil {
			return 1
		}
		return 1
	}
	if status < 100 || status > 599 {
		_, writeErr := fmt.Fprintln(output, "transport_error=diagnostic_failure")
		if writeErr != nil {
			return 1
		}
		return 1
	}
	if _, writeErr := fmt.Fprintf(output, "status=%d\n", status); writeErr != nil {
		return 1
	}
	return 0
}

func diagnosticErrorCategory(err error) string {
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok && dnsErr != nil {
		return "name_resolution"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) {
		return "timeout"
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return "timeout"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	return "connect_failure"
}

func checkSelector(client *http.Client, origin string) bool {
	return checkLanding(client, origin+probePath) && checkWebRoot(client, origin+"/")
}

func checkLanding(client *http.Client, target string) bool {
	response, ok := get(client, target)
	if !ok {
		return false
	}

	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxProbeBody+1))
	closeErr := response.Body.Close()
	expectedBody, supported := linklanding.StaticBodyForStatus(http.StatusOK)
	return readErr == nil && closeErr == nil && response.StatusCode == http.StatusOK &&
		supported &&
		len(body) <= maxProbeBody && string(body) == expectedBody &&
		response.Header.Get("Content-Type") == "text/html; charset=utf-8" &&
		response.Header.Get("Content-Length") == strconv.Itoa(len(expectedBody)) &&
		response.Header.Get("Cache-Control") == "no-store" &&
		response.Header.Get("Referrer-Policy") == "no-referrer" &&
		response.Header.Get("X-Content-Type-Options") == "nosniff" &&
		response.Header.Get("Content-Security-Policy") == contentPolicy
}

func checkWebRoot(client *http.Client, target string) bool {
	response, ok := get(client, target)
	if !ok {
		return false
	}
	closeErr := response.Body.Close()
	contentType, _, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return closeErr == nil && parseErr == nil && response.StatusCode == http.StatusOK && contentType == "text/html"
}

func get(client *http.Client, target string) (*http.Response, bool) {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	if err != nil {
		return nil, false
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, false
	}
	return response, true
}
