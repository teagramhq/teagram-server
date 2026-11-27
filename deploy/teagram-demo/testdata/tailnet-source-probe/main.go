package main

import (
	"fmt"
	"html"
	"net"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		address, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "invalid remote address", http.StatusInternalServerError)
			return
		}
		if _, err := fmt.Fprintln(w, html.EscapeString(address)); err != nil {
			return
		}
	})
	server := &http.Server{
		Addr:              ":8081",
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil {
		panic(err)
	}
}
