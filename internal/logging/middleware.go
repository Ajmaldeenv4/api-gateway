// Package logging provides request-scoped structured logging middleware.
package logging

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/ajmal/api-gateway/internal/clientip"
)

type ctxKey string

const (
	ctxKeyRequestID ctxKey = "request_id"
)

const headerRequestID = "X-Request-ID"

// RequestID pulls X-Request-ID from the incoming request or mints a new one,
// stores it on the context, echoes it on the response and forwards it upstream.
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(headerRequestID)
			if id == "" {
				id = uuid.NewString()
				r.Header.Set(headerRequestID, id)
			}
			w.Header().Set(headerRequestID, id)
			ctx := context.WithValue(r.Context(), ctxKeyRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// FromContext returns the request ID set by RequestID, or an empty string.
func FromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// Unwrap lets http.ResponseController reach Flush and Hijack underneath.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Access emits one structured log line per request. routeFn runs against the
// request as this middleware received it, so the route must already be on the
// context by the time Access is reached.
func Access(logger *slog.Logger, routeFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "request",
				slog.String("request_id", FromContext(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("route", routeFn(r)),
				slog.Int("status", rec.code),
				slog.String("remote", clientip.From(r)),
				slog.Duration("latency", time.Since(start)),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// Recover turns panics in downstream handlers into 500s and logs them.
// http.ErrAbortHandler is re-raised: it is how ReverseProxy tells net/http to
// cut a connection whose response is already partly written.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					logger.Error("panic recovered",
						"request_id", FromContext(r.Context()),
						"panic", rec,
					)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
