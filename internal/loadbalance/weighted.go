package loadbalance

import (
	"net/url"
	"sync"
)

// Smooth weighted round-robin (Nginx algorithm).
type weighted struct {
	mu      sync.Mutex
	urls    []*url.URL
	weights []int
	current []int
	total   int
}

func newWeighted(urls []*url.URL, weights []int) *weighted {
	total := 0
	for _, w := range weights {
		total += w
	}
	return &weighted{
		urls:    urls,
		weights: weights,
		current: make([]int, len(urls)),
		total:   total,
	}
}

func (w *weighted) Pick() (*url.URL, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	best := -1
	for i := range w.urls {
		w.current[i] += w.weights[i]
		if best == -1 || w.current[i] > w.current[best] {
			best = i
		}
	}
	w.current[best] -= w.total
	return w.urls[best], nil
}
