// Package metrics exposes Prometheus counters/histograms used across the gateway.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	Requests = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_http_requests_total",
			Help: "Total HTTP requests handled by the gateway.",
		},
		[]string{"route", "method", "code"},
	)

	Latency = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_http_request_duration_seconds",
			Help:    "End-to-end latency through the gateway.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"route", "method"},
	)

	Throttled = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_ratelimit_throttled_total",
			Help: "Requests rejected due to rate limiting.",
		},
		[]string{"route", "key"},
	)

	UpstreamErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_upstream_errors_total",
			Help: "Errors dialing or reading from upstream services.",
		},
		[]string{"route"},
	)

	AuthFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_auth_failures_total",
			Help: "Requests rejected due to auth failure.",
		},
		[]string{"route", "reason"},
	)
)
