package tracing

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Middleware wraps an HTTP handler to:
//   - Extract W3C traceparent from incoming requests (propagation).
//   - Start a server span for each request.
//   - Inject span context into the response headers.
//
// The span is named "<method> <route>" and carries standard semconv attributes.
// routeFn must be able to answer from the incoming request's context.
func Middleware(routeFn func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		// otelhttp creates the span, propagates context, and records status.
		return otelhttp.NewHandler(next, "gateway",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				return r.Method + " " + routeFn(r)
			}),
		)
	}
}
