// Package loadbalance picks one upstream from a pool for each request.
package loadbalance

import (
	"fmt"
	"net/url"

	"github.com/ajmal/api-gateway/internal/config"
)

type Balancer interface {
	Pick() (*url.URL, error)
}

func New(strategy string, upstreams []config.Upstream) (Balancer, error) {
	if len(upstreams) == 0 {
		return nil, fmt.Errorf("no upstreams")
	}
	urls := make([]*url.URL, 0, len(upstreams))
	weights := make([]int, 0, len(upstreams))
	for _, u := range upstreams {
		parsed, err := url.Parse(u.URL)
		if err != nil {
			return nil, fmt.Errorf("parse upstream %q: %w", u.URL, err)
		}
		urls = append(urls, parsed)
		w := u.Weight
		if w <= 0 {
			w = 1
		}
		weights = append(weights, w)
	}
	switch strategy {
	case "", "roundrobin":
		return newRoundRobin(urls), nil
	case "weighted":
		return newWeighted(urls, weights), nil
	case "leastconn":
		return newLeastConn(urls), nil
	default:
		return nil, fmt.Errorf("unknown load balance strategy %q", strategy)
	}
}
