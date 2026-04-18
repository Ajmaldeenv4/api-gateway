// Package proxy wraps httputil.ReverseProxy with per-route upstream selection,
// prefix stripping, per-route timeout, and optional circuit-breaker protection.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/ajmal/api-gateway/internal/breaker"
	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/loadbalance"
	"github.com/ajmal/api-gateway/internal/metrics"
)

type ctxKey string

const (
	CtxKeyRoute    ctxKey = "route"
	CtxKeyUpstream ctxKey = "upstream"
)

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
}

func New(route *config.Route, logger *slog.Logger, opts ...Options) (*Proxy, error) {
	b, err := loadbalance.New(route.LoadBalance, route.Upstreams)
	if err != nil {
		return nil, err
	}
	p := &Proxy{route: route, balancer: b, logger: logger}
	if len(opts) > 0 {
		p.breaker = opts[0].Breaker
	}
	p.rp = &httputil.ReverseProxy{
		Director:     p.director,
		ErrorHandler: p.errorHandler,
		Transport:    p.buildTransport(),
	}
	return p, nil
}

func (p *Proxy) director(r *http.Request) {
	upstream, err := p.balancer.Pick()
	if err != nil {
		*r = *r.WithContext(context.WithValue(r.Context(), ctxKey("err"), err))
		return
	}
	*r = *r.WithContext(context.WithValue(r.Context(), CtxKeyUpstream, upstream.String()))

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
	metrics.UpstreamErrors.WithLabelValues(p.route.ID).Inc()
	if errors.Is(err, context.Canceled) {
		return
	}
	p.logger.Error("upstream error",
		"route", p.route.ID,
		"upstream", upstreamFromCtx(r),
		"err", err.Error(),
	)
	http.Error(w, "Bad Gateway", http.StatusBadGateway)
}

// ServeHTTP enforces per-route timeout, runs the breaker (if enabled),
// then delegates to ReverseProxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), p.route.Timeout)
	defer cancel()
	ctx = context.WithValue(ctx, CtxKeyRoute, p.route.ID)
	r = r.WithContext(ctx)

	if p.breaker == nil {
		p.rp.ServeHTTP(w, r)
		return
	}

	// Run the upstream request through the circuit breaker.
	// We capture the response into a recorder, check for errors, then copy.
	_, err := p.breaker.Execute(func() ([]byte, error) {
		rec := httptest.NewRecorder()
		p.rp.ServeHTTP(rec, r)
		result := rec.Result()
		// Treat 5xx as a failure signal to the breaker.
		if result.StatusCode >= 500 {
			return nil, fmt.Errorf("upstream 5xx: %d", result.StatusCode)
		}
		// Copy captured response to the real writer.
		for k, vv := range result.Header {
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(result.StatusCode)
		_, copyErr := io.Copy(w, result.Body)
		return nil, copyErr
	})

	if errors.Is(err, breaker.ErrOpen) {
		metrics.UpstreamErrors.WithLabelValues(p.route.ID).Inc()
		http.Error(w, "Service Unavailable (circuit open)", http.StatusServiceUnavailable)
	}
}

func upstreamFromCtx(r *http.Request) string {
	if v, ok := r.Context().Value(CtxKeyUpstream).(string); ok {
		return v
	}
	return "unknown"
}

func (p *Proxy) buildTransport() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 512
	t.MaxIdleConnsPerHost = 64
	t.IdleConnTimeout = 90 * time.Second
	return t
}

// Route returns the bound route (useful for middleware wiring).
func (p *Proxy) Route() *config.Route { return p.route }
