// Package health exposes /healthz (liveness) and /readyz (readiness) endpoints.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Checker is anything that can report whether a dependency is ready.
type Checker interface {
	Ping(ctx context.Context) error
	Name() string
}

type Handler struct {
	checkers []Checker
}

func New(checkers ...Checker) *Handler {
	return &Handler{checkers: checkers}
}

func (h *Handler) Liveness(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *Handler) Readiness(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	type result struct {
		name string
		err  error
	}

	results := make([]result, len(h.checkers))
	for i, c := range h.checkers {
		results[i] = result{name: c.Name(), err: c.Ping(ctx)}
	}

	body := map[string]string{}
	allOK := true
	for _, res := range results {
		if res.err != nil {
			body[res.name] = res.err.Error()
			allOK = false
		} else {
			body[res.name] = "ok"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if !allOK {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(body)
}
