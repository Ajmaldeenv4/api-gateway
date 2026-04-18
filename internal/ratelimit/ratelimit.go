// Package ratelimit implements token-bucket rate limiting with pluggable
// backends (in-process or Redis).
package ratelimit

import (
	"context"
	"errors"
	"net/http"

	"github.com/ajmal/api-gateway/internal/auth"
	"github.com/ajmal/api-gateway/internal/config"
)

// Decision is returned by Limiter.Allow.
type Decision struct {
	Allowed    bool
	RetryAfter float64 // seconds; 0 if allowed
}

// Limiter is the interface both local + redis implementations satisfy.
type Limiter interface {
	Allow(ctx context.Context, key string) (Decision, error)
}

// KeyFor extracts the rate-limit key from a request based on the policy.
func KeyFor(cfg config.RateLimit, r *http.Request) string {
	switch cfg.Key {
	case "sub":
		if s := auth.Subject(r.Context()); s != "" {
			return "sub:" + s
		}
		return "ip:" + clientIP(r)
	case "api_key":
		if k := r.Header.Get("X-API-Key"); k != "" {
			return "apikey:" + k
		}
		return "ip:" + clientIP(r)
	default: // ip
		return "ip:" + clientIP(r)
	}
}

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return ip
	}
	return r.RemoteAddr
}

// ErrNotConfigured indicates the route has no rate limiter.
var ErrNotConfigured = errors.New("rate limit not configured")
