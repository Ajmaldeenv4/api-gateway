// Package server wires the chi router, all middleware, and per-route proxies.
// Middleware order (load-bearing):
//
//	recover → request-id → tracing → logging → metrics → auth → ratelimit → [cache] → proxy
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ajmal/api-gateway/internal/auth"
	"github.com/ajmal/api-gateway/internal/breaker"
	"github.com/ajmal/api-gateway/internal/cache"
	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/health"
	"github.com/ajmal/api-gateway/internal/healthcheck"
	"github.com/ajmal/api-gateway/internal/logging"
	metricsm "github.com/ajmal/api-gateway/internal/metrics"
	"github.com/ajmal/api-gateway/internal/proxy"
	"github.com/ajmal/api-gateway/internal/ratelimit"
)

type routeCtxKey struct{}

// Options holds optional phase-2 dependencies. All fields are nil-safe.
type Options struct {
	RedisClient   *redis.Client
	HealthChecker *healthcheck.Checker
	OTelEnabled   bool
}

// Build constructs and returns the main gateway http.Handler.
func Build(cfg *config.Config, opts Options, logger *slog.Logger) (http.Handler, error) {
	mux := chi.NewRouter()

	// Global middleware chain.
	mux.Use(logging.Recover(logger))
	mux.Use(logging.RequestID())
	if opts.OTelEnabled {
		mux.Use(otelMiddleware(routeLabel))
	}
	mux.Use(logging.Access(logger, routeLabel))
	mux.Use(middleware.RealIP)

	// Health.
	h := health.New(newRedisChecker(opts.RedisClient))
	mux.Get("/healthz", h.Liveness)
	mux.Get("/readyz", h.Readiness)

	// Per-route handlers.
	for i := range cfg.Routes {
		rt := &cfg.Routes[i]

		// Circuit breaker (optional per route).
		var cb *breaker.Breaker
		if rt.CircuitBreaker.Enabled {
			cbCfg := &breaker.Config{
				MaxRequests:  rt.CircuitBreaker.MaxRequests,
				Interval:     rt.CircuitBreaker.Interval,
				Timeout:      rt.CircuitBreaker.Timeout,
				MinRequests:  rt.CircuitBreaker.MinRequests,
				FailureRatio: rt.CircuitBreaker.FailureRatio,
			}
			cb = breaker.New(rt.ID, cbCfg, logger)
		}

		p, err := proxy.New(rt, logger, proxy.Options{Breaker: cb})
		if err != nil {
			return nil, fmt.Errorf("route %s proxy: %w", rt.ID, err)
		}

		// Rate limiter (factory picks local vs redis vs sliding window).
		lim, err := ratelimit.Build(rt.RateLimit, opts.RedisClient, rt.ID)
		if err != nil {
			return nil, fmt.Errorf("route %s ratelimit: %w", rt.ID, err)
		}

		// JWT verifier (nil when auth.type = none).
		var verifier *auth.Verifier
		if rt.Auth.Type == "jwt" {
			v, err := auth.NewVerifier(rt.Auth)
			if err != nil {
				return nil, fmt.Errorf("route %s auth: %w", rt.ID, err)
			}
			verifier = v
		}

		// Response cache (optional).
		var cacheStore *cache.Store
		if rt.Cache.Enabled && opts.RedisClient != nil {
			ttl := rt.Cache.TTL
			if ttl == 0 {
				ttl = 60e9 // 60s default
			}
			cacheStore = cache.New(opts.RedisClient, ttl, rt.Cache.VaryHeaders, "cache:"+rt.ID+":", logger)
		}

		// Start active health probes if a checker is provided.
		if opts.HealthChecker != nil {
			for _, u := range rt.Upstreams {
				opts.HealthChecker.Watch(context.Background(), rt.ID, healthcheck.Config{URL: u.URL})
			}
		}

		// Build handler chain (inner → outer):
		//   proxy ← [cache] ← ratelimit ← auth ← route-label ← metrics
		routeID := rt.ID
		rlCfg := rt.RateLimit

		var handler http.Handler = p
		if cacheStore != nil {
			handler = cacheStore.Middleware(routeID)(handler)
		}
		handler = ratelimit.Middleware(lim, rlCfg, routeID)(handler)
		handler = auth.Middleware(verifier, routeID)(handler)
		handler = setRouteLabel(routeID)(handler)
		// Metrics outermost so it records every request with the correct route label.
		id := routeID
		handler = metricsm.Middleware(func(_ *http.Request) string { return id })(handler)

		mux.Handle(rt.Match.Prefix+"*", handler)
		mux.Handle(rt.Match.Prefix, handler)
	}

	mux.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	})

	return mux, nil
}

// MetricsHandler returns the Prometheus handler for the separate metrics port.
func MetricsHandler() http.Handler {
	return promhttp.Handler()
}

func routeLabel(r *http.Request) string {
	if v, ok := r.Context().Value(routeCtxKey{}).(string); ok {
		return v
	}
	return "unknown"
}

func setRouteLabel(id string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), routeCtxKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func otelMiddleware(routeFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route := routeFn(r)
			otelhttp.NewHandler(next, route,
				otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
					return r.Method + " " + route
				}),
			).ServeHTTP(w, r)
		})
	}
}

// redisChecker implements health.Checker for Redis.
type redisChecker struct{ c *redis.Client }

func (r *redisChecker) Name() string { return "redis" }
func (r *redisChecker) Ping(ctx context.Context) error {
	if r.c == nil {
		return nil
	}
	return r.c.Ping(ctx).Err()
}
func newRedisChecker(c *redis.Client) health.Checker { return &redisChecker{c: c} }
