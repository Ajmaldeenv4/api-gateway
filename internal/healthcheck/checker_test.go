package healthcheck

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func newChecker() *Checker {
	return New(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWatch_MarksDownThenRecovers(t *testing.T) {
	var status, probes atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		if r.URL.Path != "/healthz" {
			t.Errorf("probed %s", r.URL.Path)
		}
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newChecker()
	// A trailing slash in the config must not produce //healthz.
	url := srv.URL + "/"
	c.Watch(ctx, "r", Config{URL: url, Interval: 5 * time.Millisecond, Threshold: 2})

	if !c.Healthy(url) {
		t.Fatal("an upstream starts healthy")
	}
	status.Store(http.StatusServiceUnavailable)
	waitFor(t, "upstream to go unhealthy", func() bool { return !c.Healthy(url) })
	status.Store(http.StatusOK)
	waitFor(t, "upstream to recover", func() bool { return c.Healthy(url) })

	cancel()
	time.Sleep(20 * time.Millisecond) // let an in-flight probe finish
	stopped := probes.Load()
	time.Sleep(40 * time.Millisecond)
	if probes.Load() != stopped {
		t.Fatal("probes continued after the context ended")
	}
}

func TestWatch_RefusedConnectionIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newChecker()
	c.Watch(ctx, "r", Config{URL: url, Interval: 5 * time.Millisecond, Threshold: 2})
	waitFor(t, "dead upstream to go unhealthy", func() bool { return !c.Healthy(url) })
}

func TestHealthy_UnknownURLFailsOpen(t *testing.T) {
	if !newChecker().Healthy("http://never-watched") {
		t.Fatal("unknown upstream should count as healthy")
	}
}

func TestHealthyUpstreams(t *testing.T) {
	c := newChecker()
	down := &status{}
	c.statuses.Store("http://down", down)

	got := c.HealthyUpstreams([]string{"http://up", "http://down"})
	if len(got) != 1 || got[0] != "http://up" {
		t.Fatalf("got %v", got)
	}
	got = c.HealthyUpstreams([]string{"http://down"})
	if len(got) != 1 {
		t.Fatalf("all unhealthy should fail open: %v", got)
	}
}
