// Package healthcheck actively probes upstream URLs and maintains a
// healthy/unhealthy status per upstream that the load balancer can consult.
//
// Design:
//   - One goroutine per upstream, polling on Interval.
//   - Status is stored in a sync.Map keyed by upstream URL.
//   - The load balancer wrapper (HealthAwareBalancer) skips unhealthy entries,
//     falling back to all upstreams if every one is unhealthy (fail-open).
//   - Prometheus gauges expose per-upstream health.
package healthcheck

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var upstreamHealth = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gateway_upstream_healthy",
		Help: "1 if the upstream passed its last active health check, 0 otherwise.",
	},
	[]string{"route", "upstream"},
)

// status wraps an atomic int32 so reads need no lock.
type status struct{ v int32 } // 1 = healthy, 0 = unhealthy

func (s *status) set(ok bool) {
	if ok {
		atomic.StoreInt32(&s.v, 1)
	} else {
		atomic.StoreInt32(&s.v, 0)
	}
}

func (s *status) ok() bool { return atomic.LoadInt32(&s.v) == 1 }

// Config for a single upstream probe.
type Config struct {
	URL       string        // upstream base URL
	Path      string        // probe path, default "/healthz"
	Interval  time.Duration // default 10s
	Timeout   time.Duration // per-probe timeout, default 3s
	Threshold int           // consecutive failures to mark unhealthy, default 2
}

// Checker runs active health probes for a set of upstreams and exposes their
// current healthy/unhealthy status.
type Checker struct {
	statuses sync.Map // url → *status
	client   *http.Client
	logger   *slog.Logger
}

func New(logger *slog.Logger) *Checker {
	return &Checker{
		client: &http.Client{Timeout: 5 * time.Second},
		logger: logger,
	}
}

// Watch starts a background goroutine that probes cfg.URL at cfg.Interval.
// Call Watch once per upstream. ctx cancellation stops the probe loop.
func (c *Checker) Watch(ctx context.Context, routeID string, cfg Config) {
	if cfg.Path == "" {
		cfg.Path = "/healthz"
	}
	if cfg.Interval == 0 {
		cfg.Interval = 10 * time.Second
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Threshold == 0 {
		cfg.Threshold = 2
	}

	// Start healthy; let first probe correct if needed.
	s := &status{}
	s.set(true)
	c.statuses.Store(cfg.URL, s)
	upstreamHealth.WithLabelValues(routeID, cfg.URL).Set(1)

	go c.loop(ctx, routeID, cfg, s)
}

func (c *Checker) loop(ctx context.Context, routeID string, cfg Config, s *status) {
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	failures := 0

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok := c.probe(ctx, cfg)
			if ok {
				failures = 0
				if !s.ok() {
					c.logger.Info("upstream recovered", "route", routeID, "url", cfg.URL)
				}
				s.set(true)
				upstreamHealth.WithLabelValues(routeID, cfg.URL).Set(1)
			} else {
				failures++
				if failures >= cfg.Threshold {
					if s.ok() {
						c.logger.Warn("upstream unhealthy", "route", routeID, "url", cfg.URL, "consecutive_failures", failures)
					}
					s.set(false)
					upstreamHealth.WithLabelValues(routeID, cfg.URL).Set(0)
				}
			}
		}
	}
}

func (c *Checker) probe(ctx context.Context, cfg Config) bool {
	probeURL := cfg.URL + cfg.Path
	reqCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, probeURL, nil) //nolint:gosec // URL is from gateway config, not user input
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "api-gateway/healthcheck")
	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode < 500
}

// Healthy returns whether the upstream at url is currently healthy.
// Returns true for unknown URLs (fail-open).
func (c *Checker) Healthy(url string) bool {
	if v, ok := c.statuses.Load(url); ok {
		return v.(*status).ok()
	}
	return true // unknown = assume healthy
}

// HealthyUpstreams filters a slice of upstream URLs to only healthy ones.
// If none are healthy, returns the full list (fail-open).
func (c *Checker) HealthyUpstreams(urls []string) []string {
	var healthy []string
	for _, u := range urls {
		if c.Healthy(u) {
			healthy = append(healthy, u)
		}
	}
	if len(healthy) == 0 {
		return urls // all unhealthy → fail-open
	}
	return healthy
}
