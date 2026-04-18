package loadbalance

import (
	"net/url"
	"sync/atomic"
)

type roundRobin struct {
	urls []*url.URL
	n    uint64
}

func newRoundRobin(urls []*url.URL) *roundRobin {
	return &roundRobin{urls: urls}
}

func (r *roundRobin) Pick() (*url.URL, error) {
	i := atomic.AddUint64(&r.n, 1) - 1
	return r.urls[i%uint64(len(r.urls))], nil
}
