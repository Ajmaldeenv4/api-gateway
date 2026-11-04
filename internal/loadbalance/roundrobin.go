package loadbalance

import (
	"net/url"
	"sync/atomic"
)

type roundRobin struct {
	noRelease
	pool
	n uint64
}

func newRoundRobin(p pool) *roundRobin {
	return &roundRobin{pool: p}
}

func (r *roundRobin) Pick() (*url.URL, error) {
	c := r.candidates()
	i := atomic.AddUint64(&r.n, 1) - 1
	return r.urls[c[i%uint64(len(c))]], nil
}
