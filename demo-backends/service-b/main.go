// service-b: second echo backend, no auth required by gateway.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

func main() {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":9002"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service":    "service-b",
			"request_id": r.Header.Get("X-Request-ID"),
			"path":       r.URL.Path,
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"service": "service-b",
			"path":    r.URL.Path,
		})
	})
	log.Printf("service-b listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
