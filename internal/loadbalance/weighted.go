package loadbalance

import (
	"net/url"
	"sync"
)

// Smooth weighted round-robin (Nginx algorithm).
type weighted struct {
	noRelease
	pool
	mu      sync.Mutex
	weights []int
	current []int
}

func newWeighted(p pool, weights []int) *weighted {
	return &weighted{
		pool:    p,
		weights: weights,
		current: make([]int, len(p.urls)),
	}
}

func (w *weighted) Pick() (*url.URL, error) {
	c := w.candidates()

	w.mu.Lock()
	defer w.mu.Unlock()
	best, total := -1, 0
	for _, i := range c {
		w.current[i] += w.weights[i]
		total += w.weights[i]
		if best == -1 || w.current[i] > w.current[best] {
			best = i
		}
	}
	w.current[best] -= total
	return w.urls[best], nil
}
