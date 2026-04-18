package loadbalance

import (
	"net/url"
	"sync"
	"sync/atomic"
)

type leastConn struct {
	mu    sync.Mutex
	urls  []*url.URL
	conns []int64
}

func newLeastConn(urls []*url.URL) *leastConn {
	return &leastConn{urls: urls, conns: make([]int64, len(urls))}
}

func (l *leastConn) Pick() (*url.URL, error) {
	l.mu.Lock()
	best := 0
	for i := range l.urls {
		if atomic.LoadInt64(&l.conns[i]) < atomic.LoadInt64(&l.conns[best]) {
			best = i
		}
	}
	atomic.AddInt64(&l.conns[best], 1)
	l.mu.Unlock()
	return l.urls[best], nil
}

// Release should be called when a request using the picked URL finishes.
// Not wired into the Balancer interface yet — phase 2 hooks proxy middleware to call it.
func (l *leastConn) Release(u *url.URL) {
	for i, x := range l.urls {
		if x.String() == u.String() {
			atomic.AddInt64(&l.conns[i], -1)
			return
		}
	}
}
