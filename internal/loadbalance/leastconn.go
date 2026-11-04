package loadbalance

import (
	"net/url"
	"sync"
)

// leastConn sends each request to the upstream with the fewest in-flight
// requests, rotating the starting point so ties spread across the pool.
type leastConn struct {
	pool
	mu    sync.Mutex
	conns []int64
	next  int
}

func newLeastConn(p pool) *leastConn {
	return &leastConn{pool: p, conns: make([]int64, len(p.urls))}
}

func (l *leastConn) Pick() (*url.URL, error) {
	c := l.candidates()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	best := c[l.next%len(c)]
	for _, i := range c {
		if l.conns[i] < l.conns[best] {
			best = i
		}
	}
	l.conns[best]++
	return l.urls[best], nil
}

// Release returns the slot taken by Pick.
func (l *leastConn) Release(u *url.URL) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, candidate := range l.urls {
		if candidate == u {
			if l.conns[i] > 0 {
				l.conns[i]--
			}
			return
		}
	}
}
