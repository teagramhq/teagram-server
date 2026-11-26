package main

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for its WebSocket accept token.
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type forwardedHeaders struct {
	CFConnectingIP string `json:"cfConnectingIp"`
	XForwardedFor  string `json:"xForwardedFor"`
	XRealIP        string `json:"xRealIp"`
}

type webBootstrap struct {
	Server    string `json:"server"`
	AutoGuest bool   `json:"autoGuest"`
}

type clientDiscovery struct {
	Websocket struct {
		Endpoint string `json:"endpoint"`
	} `json:"websocket"`
}

func failf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func request(target string, port int, forwardedFor, cfConnectingIP, origin string) (int, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requestURL := "http://" + net.JoinHostPort(target, strconv.Itoa(port)) + "/apiws"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil) //nolint:gosec // The harness selects only loopback or its fixed tailnet-edge alias.
	if err != nil {
		failf("create request: %v", err)
	}
	request.Host = "teagram-dev.tailaa4918.ts.net"
	request.Header.Set("Origin", origin)
	if forwardedFor != "<missing>" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	request.Header.Set("Cf-Connecting-Ip", cfConnectingIP)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	response, err := client.Do(request) //nolint:gosec // The request target is selected by the fixed local harness modes.
	if err != nil {
		failf("GET /apiws: %v", err)
	}
	defer func() { _ = response.Body.Close() }() //nolint:errcheck // The response is consumed or has no body.
	var body []byte
	if response.StatusCode == http.StatusOK {
		body, err = io.ReadAll(response.Body)
		if err != nil {
			failf("read /apiws response: %v", err)
		}
	}
	return response.StatusCode, body
}

func assertForwardedHeaders(port int) {
	status, body := request("127.0.0.1", port, "203.0.113.8, 192.0.2.44", "198.51.100.99", "https://teagram-dev.tailaa4918.ts.net")
	if status != http.StatusOK {
		failf("valid X-Forwarded-For returned HTTP %d: %s", status, body)
	}
	var headers forwardedHeaders
	if err := json.Unmarshal(body, &headers); err != nil {
		failf("decode upstream headers: %v", err)
	}
	if headers.CFConnectingIP != "192.0.2.44" {
		failf("upstream CF-Connecting-IP=%q, want rightmost X-Forwarded-For address", headers.CFConnectingIP)
	}
	if headers.XForwardedFor != "" || headers.XRealIP != "" {
		failf("forged forwarding headers reached upstream: %+v", headers)
	}
}

func assertRejectedForwardedHeaders(port int) {
	for _, forwardedFor := range []string{"<missing>", "not-an-ip"} {
		status, _ := request("127.0.0.1", port, forwardedFor, "198.51.100.99", "https://teagram-dev.tailaa4918.ts.net")
		if status != http.StatusBadRequest {
			failf("X-Forwarded-For %q returned HTTP %d, want 400", forwardedFor, status)
		}
	}
}

func assertRejectedPublicOrigins(port int) {
	for _, origin := range []string{"https://demo.teagram.io", "https://web.teagram.io"} {
		status, _ := request("127.0.0.1", port, "203.0.113.8", "198.51.100.99", origin)
		if status != http.StatusForbidden {
			failf("Origin %q returned HTTP %d, want 403", origin, status)
		}
	}
}

func assertUntrustedPeerRejected() {
	status, _ := request("tailnet-edge", 8081, "203.0.113.8", "198.51.100.99", "https://teagram-dev.tailaa4918.ts.net")
	if status != http.StatusBadRequest {
		failf("untrusted container peer returned HTTP %d, want 400", status)
	}
}

func readJSON(port int, path, host string, target any) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, //nolint:gosec // The harness connects only to the local published edge.
		"http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+path, nil)
	if err != nil {
		failf("create %s request: %v", path, err)
	}
	request.Host = host
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	response, err := client.Do(request) //nolint:gosec // The harness connects only to the local published edge.
	if err != nil {
		failf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }() //nolint:errcheck // The response body is decoded below.
	if response.StatusCode != http.StatusOK {
		failf("GET %s returned HTTP %d, want 200", path, response.StatusCode)
	}
	if response.Header.Get("Content-Type") != "application/json" {
		failf("GET %s Content-Type=%q, want application/json", path, response.Header.Get("Content-Type"))
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		failf("decode %s: %v", path, err)
	}
}

func assertTailnetWebBootstrap(port int) string {
	const expectedServer = "teagram-dev.tailaa4918.ts.net"
	var bootstrap webBootstrap
	readJSON(port, "/teagram-config.json", expectedServer, &bootstrap)
	if bootstrap.Server != expectedServer || !bootstrap.AutoGuest {
		failf("tailnet Web bootstrap=%+v, want server %q with autoGuest enabled", bootstrap, expectedServer)
	}
	var discovery clientDiscovery
	readJSON(port, "/.well-known/teagramd/client", bootstrap.Server, &discovery)
	if discovery.Websocket.Endpoint != "wss://"+bootstrap.Server+"/apiws" {
		failf("tailnet discovery endpoint=%q, want wss://%s/apiws", discovery.Websocket.Endpoint, bootstrap.Server)
	}
	return bootstrap.Server
}

func assertWebSocketHandshake(port int, server string) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	connection, err := dialer.DialContext(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		failf("connect for WebSocket handshake: %v", err)
	}
	defer func() { _ = connection.Close() }() //nolint:errcheck // Close after the handshake assertion.
	if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		failf("set WebSocket deadline: %v", err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("tailnet-source-test-key"))
	if _, err := fmt.Fprintf(connection,
		"GET /apiws HTTP/1.1\r\nHost: %s\r\n"+
			"Origin: https://%s\r\n"+
			"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
			"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n"+
			"X-Forwarded-For: 203.0.113.8, 192.0.2.44\r\n"+
			"CF-Connecting-IP: 198.51.100.99\r\n\r\n", server, server, key); err != nil {
		failf("write WebSocket handshake: %v", err)
	}
	reader := bufio.NewReader(connection)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		failf("read WebSocket status: %v", err)
	}
	if !strings.Contains(statusLine, " 101 ") {
		failf("WebSocket handshake returned %q, want HTTP 101", strings.TrimSpace(statusLine))
	}
	acceptDigest := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11")) //nolint:gosec // RFC 6455 requires SHA-1 for its WebSocket accept token.
	wantAccept := base64.StdEncoding.EncodeToString(acceptDigest[:])
	accept := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			failf("read WebSocket response headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
		name, value, found := strings.Cut(line, ":")
		if found && strings.EqualFold(name, "Sec-WebSocket-Accept") {
			accept = strings.TrimSpace(value)
		}
	}
	if accept != wantAccept {
		failf("WebSocket accept %q, want %q", accept, wantAccept)
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "peer" {
		assertUntrustedPeerRejected()
		fmt.Println("untrusted tailnet-network peer is rejected")
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "host" {
		failf("usage: tailnet-source-client host <published-port> | peer")
	}
	port, err := strconv.Atoi(os.Args[2])
	if err != nil || port < 1 || port > 65535 {
		failf("invalid published port %q", os.Args[2])
	}
	assertForwardedHeaders(port)
	assertRejectedForwardedHeaders(port)
	assertRejectedPublicOrigins(port)
	server := assertTailnetWebBootstrap(port)
	assertWebSocketHandshake(port, server)
	fmt.Println("tailnet Nginx source-address, origin, and WebSocket checks pass")
}
