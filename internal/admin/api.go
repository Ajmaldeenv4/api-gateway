// Package admin exposes a simple REST API for managing gateway routes at runtime.
// It runs on its own listener and every endpoint needs a bearer token.
//
// Endpoints (all under /admin/):
//
//	GET    /admin/routes          — list all routes
//	GET    /admin/routes/{id}     — get one route
//	POST   /admin/routes          — create / replace route (full body)
//	DELETE /admin/routes/{id}     — soft-delete route
//	POST   /admin/reload          — re-read routes from the store
//	GET    /admin/health          — postgres ping
//
// Route bodies use the same field names as a route in gateway.yaml, as JSON or
// YAML. Writes take effect as soon as they are saved.
package admin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"gopkg.in/yaml.v3"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/store"
)

const maxBodyBytes = 1 << 20

// Store is the route persistence the API needs.
type Store interface {
	LoadRoutes(ctx context.Context) ([]config.Route, error)
	UpsertRoute(ctx context.Context, r config.Route) error
	DeleteRoute(ctx context.Context, id string) error
	Health(ctx context.Context) error
}

// API serves the admin endpoints.
type API struct {
	db     Store
	token  string
	apply  func(context.Context) error
	logger *slog.Logger
}

// New returns an API requiring token. apply makes saved routes live.
func New(db Store, token string, apply func(context.Context) error, logger *slog.Logger) *API {
	return &API{db: db, token: token, apply: apply, logger: logger}
}

// Handler returns the authenticated admin router.
func (a *API) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(a.requireToken)
	a.Mount(r)
	return r
}

// Mount attaches admin routes to the given chi router under the prefix /admin.
func (a *API) Mount(r chi.Router) {
	r.Route("/admin", func(r chi.Router) {
		r.Get("/routes", a.listRoutes)
		r.Post("/routes", a.upsertRoute)
		r.Get("/routes/{id}", a.getRoute)
		r.Delete("/routes/{id}", a.deleteRoute)
		r.Post("/reload", a.reload)
		r.Get("/health", a.health)
	})
}

func (a *API) requireToken(next http.Handler) http.Handler {
	want := []byte("Bearer " + a.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if a.token == "" || subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *API) listRoutes(w http.ResponseWriter, r *http.Request) {
	routes, err := a.db.LoadRoutes(r.Context())
	if err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	if routes == nil {
		routes = []config.Route{}
	}
	a.writeRoutes(w, routes)
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
			a.writeRoutes(w, rt)
			return
		}
	}
	http.Error(w, "route not found", http.StatusNotFound)
}

func (a *API) upsertRoute(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	rt, err := config.ParseRoute(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// The route must also fit alongside the ones already stored.
	existing, err := a.db.LoadRoutes(r.Context())
	if err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	candidate := []config.Route{rt}
	for _, other := range existing {
		if other.ID != rt.ID {
			candidate = append(candidate, other)
		}
	}
	if err := config.ValidateRoutes(candidate); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	if err := a.db.UpsertRoute(r.Context(), rt); err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin: upserted route", "id", rt.ID)
	a.finishWrite(w, r, rt.ID)
}

func (a *API) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := a.db.DeleteRoute(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "route not found", http.StatusNotFound)
			return
		}
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin: deleted route", "id", id)
	a.finishWrite(w, r, id)
}

// finishWrite makes a saved change live. Build-time problems, such as a route
// naming an env var that is not set, only show up here, and the change stays
// saved, so the caller has to be told it is not serving yet.
func (a *API) finishWrite(w http.ResponseWriter, r *http.Request, id string) {
	if err := a.apply(r.Context()); err != nil {
		a.logger.Error("admin: reload failed", "id", id, "err", err)
		a.writeJSON(w, map[string]string{
			"status": "saved_not_applied", "id": id, "error": err.Error(),
		}, http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, map[string]string{"status": "ok", "id": id}, http.StatusOK)
}

func (a *API) reload(w http.ResponseWriter, r *http.Request) {
	if err := a.apply(r.Context()); err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.logger.Info("admin: routes reloaded")
	a.writeJSON(w, map[string]string{"status": "ok"}, http.StatusOK)
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

// writeRoutes renders routes with the same field names and duration format as
// gateway.yaml, so a GET response can be edited and POSTed straight back.
func (a *API) writeRoutes(w http.ResponseWriter, v interface{}) {
	raw, err := yaml.Marshal(v)
	if err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	var out interface{}
	if err := yaml.Unmarshal(raw, &out); err != nil {
		a.writeErr(w, err, http.StatusInternalServerError)
		return
	}
	a.writeJSON(w, out, http.StatusOK)
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
	json.NewEncoder(w).Encode(map[string]string{"error": strings.TrimSpace(err.Error())}) //nolint:errcheck
}
