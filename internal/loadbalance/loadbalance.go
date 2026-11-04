// Package loadbalance picks one upstream from a pool for each request.
package loadbalance

import (
	"fmt"
	"net/url"

	"github.com/ajmal/api-gateway/internal/config"
)

// Balancer chooses an upstream for a request. Every successful Pick must be
// paired with a Release once the request has finished.
type Balancer interface {
	Pick() (*url.URL, error)
	Release(*url.URL)
}

// HealthFunc reports whether the upstream with this config URL can take
// traffic. A nil HealthFunc treats every upstream as healthy.
type HealthFunc func(rawURL string) bool

// New builds a balancer for the given strategy.
func New(strategy string, upstreams []config.Upstream, healthy HealthFunc) (Balancer, error) {
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams")
	}
	p := pool{healthy: healthy}
	weights := make([]int, 0, len(upstreams))
	for _, u := range upstreams {
		parsed, err := url.Parse(u.URL)
		if err != nil {
			return nil, fmt.Errorf("parse upstream %q: %w", u.URL, err)
		}
		p.urls = append(p.urls, parsed)
		p.raw = append(p.raw, u.URL)
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		weights = append(weights, w)
	}
	switch strategy {
	case "", "roundrobin":
		return newRoundRobin(p), nil
	case "weighted":
		return newWeighted(p, weights), nil
	case "leastconn":
		return newLeastConn(p), nil
	default:
		return nil, fmt.Errorf("unknown load balance strategy %q", strategy)
	}
}
