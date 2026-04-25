package ratelimit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/redis/go-redis/v9"
)

// newTestRedis returns a Redis client connected to REDIS_ADDR, or skips the
// test when that env var is absent (e.g. running locally without Redis).
func newTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set — skipping Redis integration test")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	if err := c.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not reachable at %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// deadRedis returns a client pointing at a port nobody listens on.
func deadRedis() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:        "localhost:1",
		DialTimeout: 50 * time.Millisecond,
	})
}

// ── Redis token-bucket ──────────────────────────────────────────────────────

func TestRedis_AllowAndThrottle(t *testing.T) {
	client := newTestRedis(t)
	lim := NewRedis(client, 10, 3, false, t.Name()+":")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := lim.Allow(ctx, "user")
		if err != nil {
			t.Fatalf("request %d: unexpected error: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d should be allowed (burst=3)", i)
		}
	}

	d, err := lim.Allow(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("4th request should be throttled after burst exhausted")
	}
	if d.RetryAfter <= 0 {
		t.Fatalf("RetryAfter must be > 0, got %f", d.RetryAfter)
	}
}

func TestRedis_IndependentKeys(t *testing.T) {
	client := newTestRedis(t)
	lim := NewRedis(client, 10, 1, false, t.Name()+":")
	ctx := context.Background()

	d1, _ := lim.Allow(ctx, "key-a")
	if !d1.Allowed {
		t.Fatal("key-a should be allowed")
	}
	d2, _ := lim.Allow(ctx, "key-b")
	if !d2.Allowed {
		t.Fatal("key-b should be allowed — separate bucket from key-a")
	}
}

func TestRedis_FailOpen_OnUnreachable(t *testing.T) {
	client := deadRedis()
	defer func() { _ = client.Close() }()

	lim := NewRedis(client, 10, 5, true, "test:")
	d, err := lim.Allow(context.Background(), "user")
	if err != nil {
		t.Fatalf("fail-open should not surface error, got: %v", err)
	}
	if !d.Allowed {
		t.Fatal("fail-open: request must be allowed on Redis error")
	}
}

func TestRedis_FailClosed_OnUnreachable(t *testing.T) {
	client := deadRedis()
	defer func() { _ = client.Close() }()

	lim := NewRedis(client, 10, 5, false, "test:")
	_, err := lim.Allow(context.Background(), "user")
	if err == nil {
		t.Fatal("fail-closed: must return error when Redis is unreachable")
	}
}

// ── Sliding window ──────────────────────────────────────────────────────────

func TestSlidingWindow_AllowAndThrottle(t *testing.T) {
	client := newTestRedis(t)
	// 3 requests per 10-second window
	lim := NewSlidingWindow(client, 3, 10*time.Second, false, t.Name()+":")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		d, err := lim.Allow(ctx, "user")
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if !d.Allowed {
			t.Fatalf("request %d should be allowed (limit=3)", i)
		}
	}

	d, err := lim.Allow(ctx, "user")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("4th request should be throttled within window")
	}
}

func TestSlidingWindow_IndependentKeys(t *testing.T) {
	client := newTestRedis(t)
	lim := NewSlidingWindow(client, 1, 10*time.Second, false, t.Name()+":")
	ctx := context.Background()

	d1, _ := lim.Allow(ctx, "key-x")
	if !d1.Allowed {
		t.Fatal("key-x should be allowed")
	}
	d2, _ := lim.Allow(ctx, "key-y")
	if !d2.Allowed {
		t.Fatal("key-y should be allowed — independent of key-x")
	}
}

func TestSlidingWindow_FailOpen(t *testing.T) {
	client := deadRedis()
	defer func() { _ = client.Close() }()

	lim := NewSlidingWindow(client, 1, time.Second, true, "sw:")
	d, err := lim.Allow(context.Background(), "user")
	if err != nil {
		t.Fatalf("fail-open should not surface error, got: %v", err)
	}
	if !d.Allowed {
		t.Fatal("fail-open: request must be allowed on error")
	}
}

func TestSlidingWindow_FailClosed(t *testing.T) {
	client := deadRedis()
	defer func() { _ = client.Close() }()

	lim := NewSlidingWindow(client, 1, time.Second, false, "sw:")
	_, err := lim.Allow(context.Background(), "user")
	if err == nil {
		t.Fatal("fail-closed: must return error when Redis is unreachable")
	}
}

// ── KeyFor ──────────────────────────────────────────────────────────────────

func TestKeyFor_DefaultIP(t *testing.T) {
	cfg := config.RateLimit{Key: "ip"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"

	if got := KeyFor(cfg, req); got != "ip:1.2.3.4:5678" {
		t.Errorf("got %q, want ip:1.2.3.4:5678", got)
	}
}

func TestKeyFor_XForwardedFor(t *testing.T) {
	cfg := config.RateLimit{Key: "ip"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Forwarded-For", "10.0.0.1")

	if got := KeyFor(cfg, req); got != "ip:10.0.0.1" {
		t.Errorf("got %q, want ip:10.0.0.1", got)
	}
}

func TestKeyFor_APIKey(t *testing.T) {
	cfg := config.RateLimit{Key: "api_key"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "my-secret")

	if got := KeyFor(cfg, req); got != "apikey:my-secret" {
		t.Errorf("got %q, want apikey:my-secret", got)
	}
}

func TestKeyFor_APIKey_FallsBackToIP(t *testing.T) {
	cfg := config.RateLimit{Key: "api_key"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "9.9.9.9:1234"
	// no X-API-Key header

	if got := KeyFor(cfg, req); got != "ip:9.9.9.9:1234" {
		t.Errorf("got %q, want ip:9.9.9.9:1234", got)
	}
}

func TestKeyFor_Sub_FallsBackToIP(t *testing.T) {
	cfg := config.RateLimit{Key: "sub"}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "5.5.5.5:80"
	// no JWT subject in context

	if got := KeyFor(cfg, req); got != "ip:5.5.5.5:80" {
		t.Errorf("got %q, want ip:5.5.5.5:80", got)
	}
}

// ── Build (factory) ─────────────────────────────────────────────────────────

func TestBuild_Off(t *testing.T) {
	lim, err := Build(config.RateLimit{Mode: "off"}, nil, "r")
	if err != nil {
		t.Fatal(err)
	}
	if lim != nil {
		t.Fatal("mode=off must return nil limiter")
	}
}

func TestBuild_Empty(t *testing.T) {
	lim, err := Build(config.RateLimit{}, nil, "r")
	if err != nil {
		t.Fatal(err)
	}
	if lim != nil {
		t.Fatal("empty mode must return nil limiter")
	}
}

func TestBuild_Local_TokenBucket(t *testing.T) {
	lim, err := Build(config.RateLimit{Mode: "local", Rate: 10, Burst: 5}, nil, "r")
	if err != nil {
		t.Fatal(err)
	}
	if lim == nil {
		t.Fatal("expected non-nil limiter for local mode")
	}
}

func TestBuild_Local_UnknownAlgorithm(t *testing.T) {
	_, err := Build(config.RateLimit{Mode: "local", Algorithm: "magic"}, nil, "r")
	if err == nil {
		t.Fatal("expected error for unknown local algorithm")
	}
}

func TestBuild_UnknownMode(t *testing.T) {
	_, err := Build(config.RateLimit{Mode: "quantum"}, nil, "r")
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestBuild_Redis_NilClient_FailOpen(t *testing.T) {
	fo := true
	lim, err := Build(config.RateLimit{Mode: "redis", Rate: 10, Burst: 5, FailOpen: &fo}, nil, "r")
	if err != nil {
		t.Fatal(err)
	}
	if lim == nil {
		t.Fatal("fail-open with nil Redis should degrade to local limiter")
	}
}

func TestBuild_Redis_NilClient_FailClosed(t *testing.T) {
	fo := false
	_, err := Build(config.RateLimit{Mode: "redis", Rate: 10, Burst: 5, FailOpen: &fo}, nil, "r")
	if err == nil {
		t.Fatal("fail-closed with nil Redis client must error")
	}
}

func TestBuild_Redis_UnknownAlgorithm(t *testing.T) {
	client := newTestRedis(t)
	_, err := Build(config.RateLimit{Mode: "redis", Algorithm: "magic"}, client, "r")
	if err == nil {
		t.Fatal("expected error for unknown redis algorithm")
	}
}

func TestBuild_Redis_SlidingWindow(t *testing.T) {
	client := newTestRedis(t)
	lim, err := Build(config.RateLimit{Mode: "redis", Algorithm: "sliding_window", Rate: 10}, client, "r")
	if err != nil {
		t.Fatal(err)
	}
	if lim == nil {
		t.Fatal("expected non-nil sliding window limiter")
	}
}

// ── Middleware ───────────────────────────────────────────────────────────────

func TestMiddleware_NilLimiter_Passthrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	h := Middleware(nil, config.RateLimit{}, "r")(next)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Fatal("nil limiter should pass all requests through")
	}
}

func TestMiddleware_ModeOff_Passthrough(t *testing.T) {
	called := false
	next := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		called = true
	})

	lim := NewLocal(100, 100)
	h := Middleware(lim, config.RateLimit{Mode: "off"}, "r")(next)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	if !called {
		t.Fatal("mode=off should pass all requests through")
	}
}

func TestMiddleware_Allow(t *testing.T) {
	lim := NewLocal(100, 100)
	cfg := config.RateLimit{Mode: "local", Key: "ip"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h := Middleware(lim, cfg, "r")(next)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:1234"
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

func TestMiddleware_Throttle_Returns429(t *testing.T) {
	lim := NewLocal(1, 1) // burst=1 so second request is throttled
	cfg := config.RateLimit{Mode: "local", Key: "ip"}
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	h := Middleware(lim, cfg, "r")(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "2.2.2.2:80"

	// consume the burst
	h.ServeHTTP(httptest.NewRecorder(), req)

	// second request — throttled
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("throttled response must include Retry-After header")
	}
}

// ── toInt / parseInt helpers ─────────────────────────────────────────────────

func TestToInt_Types(t *testing.T) {
	cases := []struct {
		in      interface{}
		want    int64
		wantErr bool
	}{
		{int64(42), 42, false},
		{int(7), 7, false},
		{"99", 99, false},
		{"bad", 0, true},
		{3.14, 0, true},
	}
	for _, tc := range cases {
		got, err := toInt(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("toInt(%v): wantErr=%v got err=%v", tc.in, tc.wantErr, err)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("toInt(%v): got %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseInt_Types(t *testing.T) {
	cases := []struct {
		in      interface{}
		want    int64
		wantErr bool
	}{
		{int64(10), 10, false},
		{int(5), 5, false},
		{"123", 123, false},
		{"bad", 0, true},
		{nil, 0, true},
	}
	for _, tc := range cases {
		got, err := parseInt(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseInt(%v): wantErr=%v got err=%v", tc.in, tc.wantErr, err)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseInt(%v): got %d, want %d", tc.in, got, tc.want)
		}
	}
}
