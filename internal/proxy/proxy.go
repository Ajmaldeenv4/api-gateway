// Package proxy wraps httputil.ReverseProxy with per-route upstream selection,
// prefix stripping, per-route timeout, and optional circuit-breaker protection.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ajmal/api-gateway/internal/breaker"
	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/loadbalance"
	"github.com/ajmal/api-gateway/internal/metrics"
)

type ctxKey string

const (
	// CtxKeyRoute holds the route ID.
	CtxKeyRoute ctxKey = "route"
	// CtxKeyUpstream holds the *url.URL chosen for this request.
	CtxKeyUpstream ctxKey = "upstream"

	// statusClientClosed is logged when the client hangs up first (nginx's 499).
	statusClientClosed = 499
)

// Proxy forwards requests for one route to its upstream pool.
type Proxy struct {
	route    *config.Route
	balancer loadbalance.Balancer
	breaker  *breaker.Breaker // nil means circuit breaker disabled
	rp       *httputil.ReverseProxy
	logger   *slog.Logger
}

// Options allow optional features to be attached at construction time.
type Options struct {
	Breaker *breaker.Breaker
	// Healthy lets the balancer skip upstreams that fail active health checks.
	Healthy loadbalance.HealthFunc
	// Tracing propagates the trace context to upstreams and records client spans.
	Tracing bool
}

// New builds the proxy for a route.
func New(route *config.Route, logger *slog.Logger, opts ...Options) (*Proxy, error) {
	var o Options
	if len(opts) > 0 {
		o = opts[0]
	}
	b, err := loadbalance.New(route.LoadBalance, route.Upstreams, o.Healthy)
	if err != nil {
		return nil, err
	}
	p := &Proxy{route: route, balancer: b, breaker: o.Breaker, logger: logger}
	p.rp = &httputil.ReverseProxy{
		Director:     p.director,
		ErrorHandler: p.errorHandler,
		Transport:    p.buildTransport(o.Tracing),
	}
	return p, nil
}

func (p *Proxy) director(r *http.Request) {
	upstream, ok := r.Context().Value(CtxKeyUpstream).(*url.URL)
	if !ok {
		return // forward always sets it; the transport rejects an empty URL.
	}
	r.URL.Scheme = upstream.Scheme
	r.URL.Host = upstream.Host
	if p.route.StripPrefix != "" {
		r.URL.Path = strings.TrimPrefix(r.URL.Path, p.route.StripPrefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
	}
	if _, ok := r.Header["User-Agent"]; !ok {
		r.Header.Set("User-Agent", "api-gateway")
	}
	r.Host = upstream.Host
}

func (p *Proxy) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(statusClientClosed)
		return
	}
	metrics.UpstreamErrors.WithLabelValues(p.route.ID).Inc()
	p.logger.Error("upstream error",
		"route", p.route.ID,
		"upstream", upstreamFromCtx(r),
		"err", err.Error(),
	)
	if errors.Is(err, context.DeadlineExceeded) {
		http.Error(w, "Gateway Timeout", http.StatusGatewayTimeout)
		return
	}
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

// ServeHTTP enforces the per-route timeout, runs the breaker (if enabled), then
// forwards the request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.route.Timeout)
	defer cancel()
	ctx = context.WithValue(ctx, CtxKeyRoute, p.route.ID)
	r = r.WithContext(ctx)

	if p.breaker == nil {
		p.forward(w, r)
		return
	}

	// The response goes straight to the client whatever its status, so upstream
	// errors stay visible and streaming keeps working. The breaker only needs
	// to know how it went.
	_, err := p.breaker.Execute(func() ([]byte, error) {
		sw := &statusWriter{ResponseWriter: w}
		p.forward(sw, r)
		if sw.status >= http.StatusInternalServerError {
			return nil, fmt.Errorf("upstream status %d", sw.status)
		}
		return nil, nil
	})
	if errors.Is(err, breaker.ErrOpen) || errors.Is(err, breaker.ErrTooManyRequests) {
		metrics.UpstreamErrors.WithLabelValues(p.route.ID).Inc()
		http.Error(w, "Service Unavailable (circuit open)", http.StatusServiceUnavailable)
	}
}

// forward takes an upstream for the length of one attempt.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request) {
	upstream, err := p.balancer.Pick()
	if err != nil {
		p.logger.Error("no upstream available", "route", p.route.ID, "err", err.Error())
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	defer p.balancer.Release(upstream)
	ctx := context.WithValue(r.Context(), CtxKeyUpstream, upstream)
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

func upstreamFromCtx(r *http.Request) string {
	if u, ok := r.Context().Value(CtxKeyUpstream).(*url.URL); ok {
		return u.String()
	}
	return "unknown"
}

func (p *Proxy) buildTransport(tracing bool) http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	if tracing {
		return otelhttp.NewTransport(t)
	}
	return t
}

// Route returns the bound route (useful for middleware wiring).
func (p *Proxy) Route() *config.Route { return p.route }

// statusWriter records the final status while passing everything through.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach Flush and Hijack underneath.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
