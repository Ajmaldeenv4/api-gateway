package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestReloadOnSignal_ReloadsAndSurvivesFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal)
	calls := make(chan int, 2)
	n := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		reloadOnSignal(ctx, sig, func(context.Context) error {
			n++
			calls <- n
			return errors.New("bad config")
		}, discard())
	}()

	// The second signal is only received if the failed reload left the loop alive.
	sig <- syscall.SIGHUP
	sig <- syscall.SIGHUP
	for want := 1; want <= 2; want++ {
		select {
		case got := <-calls:
			if got != want {
				t.Fatalf("reload call %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("reload did not run")
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("loop did not stop with its context")
	}
}

func TestRunProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()

	if got := runProbe(srv.URL + "/healthz"); got != 0 {
		t.Fatalf("healthy: exit %d", got)
	}
	if got := runProbe(srv.URL + "/other"); got != 1 {
		t.Fatalf("503: exit %d", got)
	}
	srv.Close()
	if got := runProbe(srv.URL + "/healthz"); got != 1 {
		t.Fatalf("down: exit %d", got)
	}
}

func TestLoadConfig_WithoutStoreUsesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.yaml")
	doc := "routes:\n  - id: a\n    match: {prefix: /a/}\n    upstreams: [{url: 'http://localhost:1'}]\n"
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(context.Background(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].ID != "a" {
		t.Fatalf("routes = %+v", cfg.Routes)
	}
}
