// Package server wires the chi router, all middleware, and per-route proxies.
// Middleware order (load-bearing):
//
//	recover → request-id → client-ip → route match → tracing → logging →
//	metrics → auth → ratelimit → [cache] → proxy
//
// The route is matched before tracing and logging so both can label by route.
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/ajmal/api-gateway/internal/auth"
	"github.com/ajmal/api-gateway/internal/breaker"
	"github.com/ajmal/api-gateway/internal/cache"
	"github.com/ajmal/api-gateway/internal/clientip"
	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/health"
	"github.com/ajmal/api-gateway/internal/healthcheck"
	"github.com/ajmal/api-gateway/internal/loadbalance"
	"github.com/ajmal/api-gateway/internal/logging"
	metricsm "github.com/ajmal/api-gateway/internal/metrics"
	"github.com/ajmal/api-gateway/internal/proxy"
	"github.com/ajmal/api-gateway/internal/ratelimit"
	"github.com/ajmal/api-gateway/internal/router"
	"github.com/ajmal/api-gateway/internal/tracing"
)

const defaultCacheTTL = 60 * time.Second

type routeCtxKey struct{}

// Options holds optional phase-2 dependencies. All fields are nil-safe.
type Options struct {
	RedisClient   *redis.Client
	HealthChecker *healthcheck.Checker
	OTelEnabled   bool
	// Context stops background work (health probes). Defaults to Background.
	Context context.Context
}

// Build constructs and returns the main gateway http.Handler.
func Build(cfg *config.Config, opts Options, logger *slog.Logger) (http.Handler, error) {
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ips, err := clientip.New(cfg.Server.TrustedProxies)
	if err != nil {
		return nil, err
	}

	handlers := make(map[string]http.Handler, len(cfg.Routes))
	for i := range cfg.Routes {
		rt := &cfg.Routes[i]
		h, err := buildRoute(ctx, rt, opts, logger)
		if err != nil {
			return nil, err
		}
		handlers[rt.ID] = h
	}

	mux := chi.NewRouter()
	mux.Use(logging.Recover(logger))
	mux.Use(logging.RequestID())
	mux.Use(ips.Middleware())
	mux.Use(matchRoute(router.New(cfg.Routes)))
	if opts.OTelEnabled {
		mux.Use(tracing.Middleware(routeLabel))
	}
	mux.Use(logging.Access(logger, routeLabel))

	// Health.
	h := health.New(newRedisChecker(opts.RedisClient))
	mux.Get("/healthz", h.Liveness)
	mux.Get("/readyz", h.Readiness)

	// Everything else goes to the route matched above.
	mux.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := handlers[routeLabel(r)]; ok {
			handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Not Found", http.StatusNotFound)
	}))

	return mux, nil
}

// buildRoute assembles one route's handler chain, inner → outer:
//
//	proxy ← [cache] ← ratelimit ← auth ← metrics
func buildRoute(ctx context.Context, rt *config.Route, opts Options, logger *slog.Logger) (http.Handler, error) {
	// Circuit breaker (optional per route).
	var cb *breaker.Breaker
	if rt.CircuitBreaker.Enabled {
		cb = breaker.New(rt.ID, &breaker.Config{
			MaxRequests:  rt.CircuitBreaker.MaxRequests,
			Interval:     rt.CircuitBreaker.Interval,
			Timeout:      rt.CircuitBreaker.Timeout,
			MinRequests:  rt.CircuitBreaker.MinRequests,
			FailureRatio: rt.CircuitBreaker.FailureRatio,
		}, logger)
	}

	// Active health checks feed the balancer, when a checker is provided.
	var healthy loadbalance.HealthFunc
	if opts.HealthChecker != nil {
		healthy = opts.HealthChecker.Healthy
	}

	p, err := proxy.New(rt, logger, proxy.Options{
		Breaker: cb,
		Healthy: healthy,
		Tracing: opts.OTelEnabled,
	})
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
	if rt.Cache.Enabled {
		if opts.RedisClient == nil {
			logger.Warn("response cache needs Redis, leaving it off", "route", rt.ID)
		} else {
			ttl := rt.Cache.TTL
			if ttl == 0 {
				ttl = defaultCacheTTL
			}
			cacheStore = cache.New(opts.RedisClient, ttl, rt.Cache.VaryHeaders, "cache:"+rt.ID+":", logger)
		}
	}

	// Probes run only once everything above has built, so a route that fails
	// to build leaves nothing running.
	if opts.HealthChecker != nil {
		for _, u := range rt.Upstreams {
			opts.HealthChecker.Watch(ctx, rt.ID, healthcheck.Config{URL: u.URL})
		}
	}

	var handler http.Handler = p
	if cacheStore != nil {
		handler = cacheStore.Middleware(rt.ID)(handler)
	}
	handler = ratelimit.Middleware(lim, rt.RateLimit, rt.ID)(handler)
	handler = auth.Middleware(verifier, rt.ID)(handler)
	// Metrics outermost so it records every request, rejected ones included.
	handler = metricsm.Middleware(routeLabel)(handler)
	return handler, nil
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

// matchRoute resolves the request to a route once, for everything downstream.
func matchRoute(m *router.Matcher) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if rt := m.Match(r); rt != nil {
				r = r.WithContext(context.WithValue(r.Context(), routeCtxKey{}, rt.ID))
			}
			next.ServeHTTP(w, r)
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
