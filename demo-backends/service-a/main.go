// service-a: a tiny echo backend used for local demo + integration tests.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9001"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"service":    "service-a",
			"request_id": r.Header.Get("X-Request-ID"),
			"path":       r.URL.Path,
		})
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		headers := map[string]string{}
		for k, vv := range r.Header {
			headers[k] = vv[0]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"service": "service-a",
			"method":  r.Method,
			"path":    r.URL.Path,
			"query":   r.URL.RawQuery,
			"headers": headers,
		})
	})
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	log.Printf("service-a listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
