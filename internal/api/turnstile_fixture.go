//go:build fixtureturnstile

package api

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// The isolated real-client guest fixture uses an in-process Siteverify peer.
// This file is absent from production builds and accepts exactly one synthetic
// signup token, so browser CI needs no external network access or QA bypass.
func init() {
	if os.Getenv("TG_FIXTURE_TURNSTILE_SITEVERIFY") != "1" {
		return
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("start fixture Siteverify listener: " + err.Error())
	}
	var used atomic.Bool
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			valid := r.Method == http.MethodPost && r.URL.Path == "/siteverify" &&
				r.Form.Get("secret") == "fixture-turnstile-secret" &&
				r.Form.Get("response") == "fixture-turnstile-signup-token" && used.CompareAndSwap(false, true)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if !valid {
				_ = json.NewEncoder(w).Encode(map[string]any{"success": false})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success":  true,
				"action":   "signup",
				"hostname": "demo.teagram.io",
			})
		}),
	}
	go func() { _ = server.Serve(listener) }()
	turnstileSiteverifyEndpoint = "http://" + listener.Addr().String() + "/siteverify"
}
