package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/healthcheck"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func cfgWith(routes ...config.Route) *config.Config {
	c := &config.Config{Routes: routes}
	c.ApplyDefaults()
	return c
}

func route(id, prefix, upstream string) config.Route {
	return config.Route{
		ID:        id,
		Match:     config.Match{Prefix: prefix},
		Upstreams: []config.Upstream{{URL: upstream}},
	}
}

func status(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func backend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

func TestReload_SwapsRoutes(t *testing.T) {
	up := backend(t)
	rt, err := NewRuntime(cfgWith(route("a", "/a/", up.URL)), Options{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	if got := status(t, rt, "/a/x"); got != http.StatusOK {
		t.Fatalf("/a/ before reload: %d", got)
	}
	if err := rt.Reload(cfgWith(route("b", "/b/", up.URL))); err != nil {
		t.Fatal(err)
	}
	if got := status(t, rt, "/b/x"); got != http.StatusOK {
		t.Fatalf("/b/ after reload: %d", got)
	}
	if got := status(t, rt, "/a/x"); got != http.StatusNotFound {
		t.Fatalf("/a/ after reload: %d", got)
	}
}

func TestReload_FailureKeepsRunningRoutes(t *testing.T) {
	up := backend(t)
	rt, err := NewRuntime(cfgWith(route("a", "/a/", up.URL)), Options{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	bad := route("b", "/b/", up.URL)
	bad.Auth = config.Auth{Type: "jwt", SecretEnv: "UNSET_VAR_FOR_TEST"}
	if err := rt.Reload(cfgWith(bad)); err == nil {
		t.Fatal("expected reload error")
	}
	if got := status(t, rt, "/a/x"); got != http.StatusOK {
		t.Fatalf("old route must keep serving: %d", got)
	}
}

func TestNewRuntime_FirstBuildFailureIsReturned(t *testing.T) {
	bad := route("b", "/b/", "http://localhost:1")
	bad.Auth = config.Auth{Type: "jwt", SecretEnv: "UNSET_VAR_FOR_TEST"}
	if _, err := NewRuntime(cfgWith(bad), Options{}, discard()); err == nil {
		t.Fatal("expected error")
	}
}

// Requests racing a reload must all be served by one generation or the other.
func TestReload_NoFailedRequestsDuringSwap(t *testing.T) {
	up := backend(t)
	cfg := cfgWith(route("a", "/a/", up.URL))
	rt, err := NewRuntime(cfg, Options{}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	gw := httptest.NewServer(rt)
	defer gw.Close()

	var failures atomic.Int32
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				resp, err := http.Get(gw.URL + "/a/x")
				if err != nil || resp.StatusCode != http.StatusOK {
					failures.Add(1)
				}
				if err == nil {
					_ = resp.Body.Close()
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if err := rt.Reload(cfgWith(route("a", "/a/", up.URL))); err != nil {
			t.Error(err)
		}
	}
	close(done)
	wg.Wait()
	if n := failures.Load(); n != 0 {
		t.Fatalf("%d requests failed during reloads", n)
	}
}

// A reload must stop the previous generation's probes. They would otherwise
// add up forever, and keep probing upstreams that have been removed.
func TestReload_StopsOldHealthProbes(t *testing.T) {
	up := backend(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var seen []context.Context
	var mu sync.Mutex
	watch := func(c context.Context) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, c)
	}

	rt, err := NewRuntime(cfgWith(route("a", "/a/", up.URL)), Options{Context: ctx}, discard())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	watch(rt.current.Load().context())
	if err := rt.Reload(cfgWith(route("a", "/a/", up.URL))); err != nil {
		t.Fatal(err)
	}
	watch(rt.current.Load().context())

	if seen[0].Err() == nil {
		t.Fatal("the replaced generation should be stopped")
	}
	if seen[1].Err() != nil {
		t.Fatal("the current generation should be running")
	}
	rt.Close()
	if seen[1].Err() == nil {
		t.Fatal("Close should stop the current generation")
	}
}

func TestBuild_WatchesEveryUpstream(t *testing.T) {
	up := backend(t)
	checker := healthcheck.New(discard())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := route("a", "/a/", up.URL)
	r.Upstreams = append(r.Upstreams, config.Upstream{URL: "http://never-resolves.invalid"})

	if _, err := Build(cfgWith(r), Options{HealthChecker: checker, Context: ctx}, discard()); err != nil {
		t.Fatal(err)
	}
	got := checker.HealthyUpstreams([]string{up.URL, "http://never-resolves.invalid"})
	if len(got) != 2 {
		t.Fatalf("upstreams start healthy: %v", got)
	}
	time.Sleep(time.Millisecond) // let the probe goroutines start before cancel
}
