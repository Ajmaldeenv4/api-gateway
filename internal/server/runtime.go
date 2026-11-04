package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/ajmal/api-gateway/internal/config"
)

// Runtime is the gateway handler with hot-swappable routes. Each Reload builds
// a complete new handler and swaps it in atomically: requests already in flight
// finish on the generation that accepted them, and a config that fails to build
// leaves the running one untouched.
//
// A reload starts routes from scratch, so local rate-limit buckets and breaker
// state reset. Redis-backed limits and cached responses carry over.
type Runtime struct {
	opts    Options
	logger  *slog.Logger
	reload  sync.Mutex
	current atomic.Pointer[generation]
}

type generation struct {
	handler http.Handler
	ctx     context.Context // ends when this generation is replaced
	stop    context.CancelFunc
}

func (g *generation) context() context.Context { return g.ctx }

// NewRuntime builds the first generation from cfg.
func NewRuntime(cfg *config.Config, opts Options, logger *slog.Logger) (*Runtime, error) {
	rt := &Runtime{opts: opts, logger: logger}
	if err := rt.Reload(cfg); err != nil {
		return nil, err
	}
	return rt, nil
}

// ServeHTTP dispatches to the current generation.
func (rt *Runtime) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rt.current.Load().handler.ServeHTTP(w, r)
}

// Reload replaces the running routes with cfg's.
func (rt *Runtime) Reload(cfg *config.Config) error {
	rt.reload.Lock()
	defer rt.reload.Unlock()

	parent := rt.opts.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, stop := context.WithCancel(parent)
	opts := rt.opts
	opts.Context = ctx
	handler, err := Build(cfg, opts, rt.logger)
	if err != nil {
		stop()
		return err
	}
	if old := rt.current.Swap(&generation{handler: handler, ctx: ctx, stop: stop}); old != nil {
		old.stop()
	}
	rt.logger.Info("routes loaded", "routes", len(cfg.Routes))
	return nil
}

// Close stops the current generation's background work.
func (rt *Runtime) Close() {
	if g := rt.current.Load(); g != nil {
		g.stop()
	}
}
