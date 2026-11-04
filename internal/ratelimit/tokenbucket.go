package ratelimit

import (
	"context"
	"sync"

	"golang.org/x/time/rate"
)

// defaultMaxKeys bounds memory: one bucket per key, and keys come from clients.
const defaultMaxKeys = 100_000

// Local is an in-process token-bucket limiter keyed by an arbitrary string.
// One rate.Limiter is allocated per key, and the table is pruned once it holds
// maxKeys buckets.
type Local struct {
	rate    rate.Limit
	burst   int
	maxKeys int
	mu      sync.Mutex
	bag     map[string]*rate.Limiter
}

// NewLocal returns an in-process limiter allowing perSec requests per second.
func NewLocal(perSec float64, burst int) *Local {
	return &Local{
		rate:    rate.Limit(perSec),
		burst:   burst,
		maxKeys: defaultMaxKeys,
		bag:     make(map[string]*rate.Limiter),
	}
}

// Allow consumes one token from the bucket for key.
func (l *Local) Allow(_ context.Context, key string) (Decision, error) {
	l.mu.Lock()
	lim, ok := l.bag[key]
	if !ok {
		if len(l.bag) >= l.maxKeys {
			l.evict()
		}
		lim = l.newBucket()
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

func (l *Local) newBucket() *rate.Limiter { return rate.NewLimiter(l.rate, l.burst) }

// evict makes room in the table. A full bucket carries no state a new one would
// not have, so those go first. If that frees nothing (a flood of distinct keys)
// half the table is dropped at random, which only ever errs towards allowing a
// request. The caller holds l.mu.
func (l *Local) evict() {
	for k, lim := range l.bag {
		if lim.Tokens() >= float64(l.burst) {
			delete(l.bag, k)
		}
	}
	if len(l.bag) < l.maxKeys {
		return
	}
	for k := range l.bag {
		delete(l.bag, k)
		if len(l.bag) <= l.maxKeys/2 {
			return
		}
	}
}
