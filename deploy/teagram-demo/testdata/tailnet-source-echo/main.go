package main

import (
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for its WebSocket accept token.
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type forwardedHeaders struct {
	CFConnectingIP string `json:"cfConnectingIp"`
	XForwardedFor  string `json:"xForwardedFor"`
	XRealIP        string `json:"xRealIp"`
}

func main() {
	http.HandleFunc("/apiws", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + websocketGUID)) //nolint:gosec // RFC 6455 requires SHA-1 for its WebSocket accept token.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "WebSocket upgrade is unavailable", http.StatusInternalServerError)
				return
			}
			connection, buffered, err := hijacker.Hijack()
			if err != nil {
				return
			}
			defer func() { _ = connection.Close() }() //nolint:errcheck // Close after the isolated handshake response.
			if _, err := fmt.Fprintf(buffered,
				"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"+
					"Connection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
				base64.StdEncoding.EncodeToString(accept[:])); err != nil {
				return
			}
			if err := buffered.Flush(); err != nil {
				return
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(forwardedHeaders{
			CFConnectingIP: r.Header.Get("Cf-Connecting-Ip"),
			XForwardedFor:  r.Header.Get("X-Forwarded-For"),
			XRealIP:        r.Header.Get("X-Real-IP"),
		}); err != nil {
			return
		}
	})
	server := &http.Server{
		Addr:              ":2446",
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
