package loadbalance

import (
	"net/url"
	"testing"

	"github.com/ajmal/api-gateway/internal/config"
)

const (
	urlA = "http://a:1"
	urlB = "http://b:1"
	urlC = "http://c:1"
)

func upstreams(urls ...string) []config.Upstream {
	out := make([]config.Upstream, 0, len(urls))
	for _, u := range urls {
		out = append(out, config.Upstream{URL: u, Weight: 1})
	}
	return out
}

func build(t *testing.T, strategy string, ups []config.Upstream, healthy HealthFunc) Balancer {
	t.Helper()
	b, err := New(strategy, ups, healthy)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// pick takes an upstream and hands it straight back, as a finished request does.
func pick(t *testing.T, b Balancer) string {
	t.Helper()
	u, err := b.Pick()
	if err != nil {
		t.Fatal(err)
	}
	b.Release(u)
	return u.String()
}

func TestRoundRobin_Cycles(t *testing.T) {
	b := build(t, "roundrobin", upstreams(urlA, urlB), nil)
	for i, want := range []string{urlA, urlB, urlA, urlB} {
		if got := pick(t, b); got != want {
			t.Fatalf("pick %d: got %s, want %s", i, got, want)
		}
	}
}

func TestWeighted_HonoursWeights(t *testing.T) {
	b := build(t, "weighted", []config.Upstream{{URL: urlA, Weight: 2}, {URL: urlB, Weight: 1}}, nil)
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[pick(t, b)]++
	}
	if counts[urlA] != 200 || counts[urlB] != 100 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestLeastConn_PrefersFewestInFlight(t *testing.T) {
	b := build(t, "leastconn", upstreams(urlA, urlB), nil)
	first, _ := b.Pick()
	for i := 0; i < 5; i++ {
		if got := pick(t, b); got == first.String() {
			t.Fatalf("pick %d went to the busy upstream %s", i, got)
		}
	}
	// Once released, the first upstream is a candidate again.
	b.Release(first)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		seen[pick(t, b)] = true
	}
	if !seen[urlA] || !seen[urlB] {
		t.Fatalf("idle upstreams should share traffic: %v", seen)
	}
}

func TestLeastConn_CountersDoNotLeak(t *testing.T) {
	b := build(t, "leastconn", upstreams(urlA, urlB), nil).(*leastConn)
	for i := 0; i < 100; i++ {
		pick(t, b)
	}
	for i, n := range b.conns {
		if n != 0 {
			t.Fatalf("upstream %d: %d in flight after all requests finished", i, n)
		}
	}
}

func TestRelease_UnknownURLIsIgnored(t *testing.T) {
	for _, strategy := range []string{"roundrobin", "weighted", "leastconn"} {
		b := build(t, strategy, upstreams(urlA), nil)
		b.Release(&url.URL{Host: "elsewhere"})
	}
}

func TestStrategies_SkipUnhealthy(t *testing.T) {
	healthy := func(raw string) bool { return raw != urlA }
	for _, strategy := range []string{"roundrobin", "weighted", "leastconn"} {
		t.Run(strategy, func(t *testing.T) {
			b := build(t, strategy, upstreams(urlA, urlB, urlC), healthy)
			seen := map[string]int{}
			for i := 0; i < 30; i++ {
				seen[pick(t, b)]++
			}
			if seen[urlA] != 0 {
				t.Fatalf("unhealthy upstream got traffic: %v", seen)
			}
			if seen[urlB] == 0 || seen[urlC] == 0 {
				t.Fatalf("healthy upstreams should share traffic: %v", seen)
			}
		})
	}
}

// An idle dead upstream must not outrank a live one with requests in flight.
func TestLeastConn_IgnoresIdleUnhealthyUpstream(t *testing.T) {
	b := build(t, "leastconn", upstreams(urlA, urlB), func(raw string) bool { return raw != urlA })
	if _, err := b.Pick(); err != nil { // leaves urlB with one in flight
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if got := pick(t, b); got != urlB {
			t.Fatalf("pick %d: got %s", i, got)
		}
	}
}

func TestWeighted_SkippingKeepsRemainingRatio(t *testing.T) {
	ups := []config.Upstream{{URL: urlA, Weight: 5}, {URL: urlB, Weight: 2}, {URL: urlC, Weight: 1}}
	b := build(t, "weighted", ups, func(raw string) bool { return raw != urlA })
	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		counts[pick(t, b)]++
	}
	if counts[urlB] != 200 || counts[urlC] != 100 {
		t.Fatalf("counts = %v", counts)
	}
}

func TestAllUnhealthy_FailsOpen(t *testing.T) {
	none := func(string) bool { return false }
	for _, strategy := range []string{"roundrobin", "weighted", "leastconn"} {
		b := build(t, strategy, upstreams(urlA, urlB), none)
		seen := map[string]bool{}
		for i := 0; i < 8; i++ {
			seen[pick(t, b)] = true
		}
		if len(seen) != 2 {
			t.Fatalf("%s: want both upstreams, got %v", strategy, seen)
		}
	}
}

func TestNew_Errors(t *testing.T) {
	if _, err := New("roundrobin", nil, nil); err == nil {
		t.Fatal("no upstreams: expected error")
	}
	if _, err := New("nope", upstreams(urlA), nil); err == nil {
		t.Fatal("unknown strategy: expected error")
	}
}
