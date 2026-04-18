package auth

import (
	"context"
	"net/http"

	"github.com/ajmal/api-gateway/internal/metrics"
)

// Middleware authenticates requests against the given Verifier.
// If the verifier is nil, requests pass through unchanged.
func Middleware(v *Verifier, routeID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if v == nil {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, err := v.Verify(r.Header.Get("Authorization"))
			if err != nil {
				if !v.Required() && err == ErrMissing {
					next.ServeHTTP(w, r)
					return
				}
				reason := "invalid"
				if err == ErrMissing {
					reason = "missing"
				}
				metrics.AuthFailures.WithLabelValues(routeID, reason).Inc()
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), CtxKeyClaims, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
