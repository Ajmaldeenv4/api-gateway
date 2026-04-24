// Package breaker wraps sony/gobreaker with Prometheus state-change metrics.
//
// State machine (gobreaker):
//
//	Closed ──(failures > threshold)──▶ Open
//	Open   ──(timeout elapsed)──────▶ Half-Open
//	Half-Open ──(success)───────────▶ Closed
//	Half-Open ──(failure)───────────▶ Open
package breaker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/sony/gobreaker/v2"
)

var (
	stateChanges = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_circuit_breaker_state_changes_total",
			Help: "Number of circuit breaker state transitions.",
		},
		[]string{"route", "from", "to"},
	)

	openCircuits = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gateway_circuit_breaker_open",
			Help: "1 if the circuit breaker for a route is currently open.",
		},
		[]string{"route"},
	)
)

// Config holds tuning knobs for a single breaker.
type Config struct {
	// MaxRequests allowed through while half-open (default 1).
	MaxRequests uint32
	// Interval is the rolling window for counting failures (default 60s).
	Interval time.Duration
	// Timeout before an open breaker enters half-open (default 10s).
	Timeout time.Duration
	// MinRequests before the error ratio is evaluated (default 5).
	MinRequests uint32
	// FailureRatio to trip the breaker (default 0.5 = 50 %).
	FailureRatio float64
}

func defaultConfig() Config {
	return Config{
		MaxRequests:  1,
		Interval:     60 * time.Second,
		Timeout:      10 * time.Second,
		MinRequests:  5,
		FailureRatio: 0.5,
	}
}

// Breaker wraps a gobreaker.CircuitBreaker and is safe for concurrent use.
type Breaker struct {
	cb      *gobreaker.CircuitBreaker[[]byte]
	routeID string
}

// New constructs a Breaker for the given route. Passing a nil cfg uses defaults.
func New(routeID string, cfg *Config, logger *slog.Logger) *Breaker {
	c := defaultConfig()
	if cfg != nil {
		if cfg.MaxRequests > 0 {
			c.MaxRequests = cfg.MaxRequests
		}
		if cfg.Interval > 0 {
			c.Interval = cfg.Interval
		}
		if cfg.Timeout > 0 {
			c.Timeout = cfg.Timeout
		}
		if cfg.MinRequests > 0 {
			c.MinRequests = cfg.MinRequests
		}
		if cfg.FailureRatio > 0 {
			c.FailureRatio = cfg.FailureRatio
		}
	}

	settings := gobreaker.Settings{
		Name:        routeID,
		MaxRequests: c.MaxRequests,
		Interval:    c.Interval,
		Timeout:     c.Timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			if counts.Requests < uint32(c.MinRequests) {
				return false
			}
			ratio := float64(counts.TotalFailures) / float64(counts.Requests)
			return ratio >= c.FailureRatio
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			fromStr := from.String()
			toStr := to.String()
			stateChanges.WithLabelValues(name, fromStr, toStr).Inc()
			if to == gobreaker.StateOpen {
				openCircuits.WithLabelValues(name).Set(1)
			} else {
				openCircuits.WithLabelValues(name).Set(0)
			}
			logger.Warn("circuit breaker state change",
				"route", name, "from", fromStr, "to", toStr)
		},
		IsSuccessful: func(err error) bool {
			// Treat context-cancelled requests as neutral (not a failure).
			return err == nil || errors.Is(err, context.Canceled)
		},
	}

	return &Breaker{
		cb:      gobreaker.NewCircuitBreaker[[]byte](settings),
		routeID: routeID,
	}
}

// State returns the current state name ("closed", "open", "half-open").
func (b *Breaker) State() string {
	return b.cb.State().String()
}

// ErrOpen is returned by Execute when the circuit is open.
var ErrOpen = gobreaker.ErrOpenState

// Execute runs fn through the circuit breaker.
// Returns gobreaker.ErrOpenState if the circuit is open.
func (b *Breaker) Execute(fn func() ([]byte, error)) ([]byte, error) {
	return b.cb.Execute(fn)
}
