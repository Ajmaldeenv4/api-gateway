package ratelimit

import (
	"net/http"
	"strconv"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/metrics"
)

// Middleware enforces the rate limit policy for a single route.
// If lim is nil the middleware is a no-op.
func Middleware(lim Limiter, cfg config.RateLimit, routeID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if lim == nil || cfg.Mode == "off" {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := KeyFor(cfg, r)
			dec, err := lim.Allow(r.Context(), key)
			if err != nil {
				// Backend errored and limiter didn't fail-open.
				http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
				return
			}
			if !dec.Allowed {
				metrics.Throttled.WithLabelValues(routeID, cfg.Key).Inc()
				if dec.RetryAfter > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(dec.RetryAfter)+1))
				}
				http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
