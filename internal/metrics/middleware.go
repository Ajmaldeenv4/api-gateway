package metrics

import (
	"net/http"
	"strconv"
	"time"
)

// statusRecorder captures the status code written upstream so the middleware
// can label the counter correctly.
type statusRecorder struct {
	http.ResponseWriter
	code  int
	wrote bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.code = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.code = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Middleware records request count + latency for any handler it wraps.
// routeFn returns the route label for the given request (e.g. from context).
func Middleware(routeFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(rec, r)
			route := routeFn(r)
			Requests.WithLabelValues(route, r.Method, strconv.Itoa(rec.code)).Inc()
			Latency.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
		})
	}
}
