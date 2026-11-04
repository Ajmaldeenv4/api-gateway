package loadbalance

import "net/url"

// pool is the upstream list shared by every strategy.
type pool struct {
	urls    []*url.URL
	raw     []string // config URLs, the identity the health checker reports on
	healthy HealthFunc
}

// candidates returns the indexes a strategy may choose from. It is every
// upstream unless a health function rules some out; if that would leave none,
// it fails open and returns them all.
func (p *pool) candidates() []int {
	all := make([]int, 0, len(p.urls))
	live := make([]int, 0, len(p.urls))
	for i := range p.urls {
		all = append(all, i)
		if p.healthy == nil || p.healthy(p.raw[i]) {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return all
	}
	return live
}

// noRelease is embedded by strategies that keep no per-request state.
type noRelease struct{}

func (noRelease) Release(*url.URL) {}
