package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// Local is an in-process token-bucket limiter keyed by an arbitrary string.
// One rate.Limiter is allocated per key; cleanup is deliberately not implemented
// for phase 1 — fine for bounded key spaces (IPs, user IDs). Phase 2 should add
// LRU eviction when scaling beyond dev.
type Local struct {
	rate  rate.Limit
	burst int
	mu    sync.Mutex
	bag   map[string]*rate.Limiter
}

func NewLocal(perSec float64, burst int) *Local {
	return &Local{
		rate:  rate.Limit(perSec),
		burst: burst,
		bag:   make(map[string]*rate.Limiter),
	}
}

func (l *Local) Allow(_ context.Context, key string) (Decision, error) {
	l.mu.Lock()
	lim, ok := l.bag[key]
	if !ok {
		lim = rate.NewLimiter(l.rate, l.burst)
		l.bag[key] = lim
	}
	l.mu.Unlock()

	if lim.Allow() {
		return Decision{Allowed: true}, nil
	}
	// Approximate retry-after: 1 / rate.
	retry := 0.0
	if l.rate > 0 {
		retry = 1.0 / float64(l.rate)
	}
	return Decision{Allowed: false, RetryAfter: retry}, nil
}
