// Package admin exposes a simple REST API for managing gateway routes at runtime.
// Endpoints (all under /admin/):
//
//	GET    /admin/routes          — list all routes
//	GET    /admin/routes/{id}     — get one route
//	POST   /admin/routes          — create / replace route (full body)
//	DELETE /admin/routes/{id}     — soft-delete route
//	GET    /admin/health          — db + redis ping
package admin

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/store"
)

type API struct {
	db     *store.DB
	logger *slog.Logger
}

func New(db *store.DB, logger *slog.Logger) *API {
	return &API{db: db, logger: logger}
}

// Mount attaches admin routes to the given chi router under the prefix /admin.
func (a *API) Mount(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Get("/routes", a.listRoutes)
		r.Post("/routes", a.upsertRoute)
		r.Get("/routes/{id}", a.getRoute)
		r.Delete("/routes/{id}", a.deleteRoute)
		r.Get("/health", a.health)
	})
}

func (a *API) listRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := a.db.LoadRoutes(r.Context())
	if err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, routes, http.StatusOK)
}

func (a *API) getRoute(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	routes, err := a.db.LoadRoutes(r.Context())
	if err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, rt := range routes {
		if rt.ID == id {
			a.writeJSON(w, rt, http.StatusOK)
			return
		}
	}
	http.Error(w, "route not found", http.StatusNotFound)
}

// routeRequest is the JSON body for POST /admin/routes.
type routeRequest struct {
	ID          string            `json:"id"`
	Prefix      string            `json:"prefix"`
	StripPrefix string            `json:"strip_prefix"`
	LoadBalance string            `json:"load_balance"`
	TimeoutMs   int64             `json:"timeout_ms"`
	Upstreams   []config.Upstream `json:"upstreams"`
}

func (a *API) upsertRoute(w http.ResponseWriter, r *http.Request) {
	var req routeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" || req.Prefix == "" || len(req.Upstreams) == 0 {
		http.Error(w, "id, prefix, and upstreams are required", http.StatusBadRequest)
		return
	}
	timeoutMs := req.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	rt := config.Route{
		ID:          req.ID,
		Match:       config.Match{Prefix: req.Prefix},
		StripPrefix: req.StripPrefix,
		LoadBalance: req.LoadBalance,
		Timeout:     time.Duration(timeoutMs) * time.Millisecond,
		Upstreams:   req.Upstreams,
		Auth:        config.Auth{Type: "none"},
	}
	if err := a.db.UpsertRoute(r.Context(), rt); err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin: upserted route", "id", rt.ID)
	a.writeJSON(w, map[string]string{"status": "ok", "id": rt.ID}, http.StatusOK)
}

func (a *API) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := a.db.DeleteRoute(r.Context(), id); err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin: deleted route", "id", id)
	a.writeJSON(w, map[string]string{"status": "ok", "id": id}, http.StatusOK)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	status := map[string]string{}
	if err := a.db.Health(ctx); err != nil {
		status["postgres"] = err.Error()
		a.writeJSON(w, status, http.StatusServiceUnavailable)
		return
	}
	status["postgres"] = "ok"
	a.writeJSON(w, status, http.StatusOK)
}

func (a *API) writeJSON(w http.ResponseWriter, v interface{}, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.logger.Error("admin: encode response", "err", err)
	}
}

func (a *API) writeErr(w http.ResponseWriter, err error, code int) {
	a.logger.Error("admin error", "err", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}) //nolint:errcheck
}
