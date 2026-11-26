package main

import (
	"fmt"
	"net"
	"net/http"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		address, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			http.Error(w, "invalid remote address", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprintln(w, address)
	})
	if err := http.ListenAndServe(":8081", nil); err != nil {
		panic(err)
	}
}
