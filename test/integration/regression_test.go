package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/proxy"
	"github.com/ajmal/api-gateway/internal/server"
)

// serve builds a gateway around routes and registers its cleanup.
func serve(t *testing.T, logger *slog.Logger, routes ...config.Route) *httptest.Server {
	t.Helper()
	return serveConfig(t, logger, server.Options{}, &config.Config{Routes: routes})
}

func serveConfig(t *testing.T, logger *slog.Logger, opts server.Options, cfg *config.Config) *httptest.Server {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h, err := server.Build(cfg, opts, logger)
	if err != nil {
		t.Fatalf("build gateway: %v", err)
	}
	gw := httptest.NewServer(h)
	t.Cleanup(gw.Close)
	return gw
}

func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: miniredis.RunT(t).Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func getAs(t *testing.T, url, token string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func openRoute(id, prefix string, upstreams ...string) config.Route {
	r := config.Route{
		ID:          id,
		Match:       config.Match{Prefix: prefix},
		StripPrefix: strings.TrimSuffix(prefix, "/"),
		LoadBalance: "roundrobin",
		Timeout:     2 * time.Second,
		Auth:        config.Auth{Type: "none"},
		RateLimit:   config.RateLimit{Mode: "off"},
	}
	for _, u := range upstreams {
		r.Upstreams = append(r.Upstreams, config.Upstream{URL: u, Weight: 1})
	}
	return r
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// With a breaker on, upstream failures must still reach the client as failures.
func TestBreaker_Upstream5xxReachesClient(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	rt := openRoute("r", "/r/", backend.URL)
	rt.CircuitBreaker = config.CircuitBreaker{Enabled: true, MinRequests: 100}
	gw := serve(t, nil, rt)

	code, body := get(t, gw.URL+"/r/x")
	if code != http.StatusInternalServerError || !strings.Contains(body, "boom") {
		t.Fatalf("want upstream 500 boom, got %d %q", code, body)
	}
}

func TestBreaker_UnreachableUpstreamIs502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	rt := openRoute("r", "/r/", dead.URL)
	rt.CircuitBreaker = config.CircuitBreaker{Enabled: true, MinRequests: 100}
	gw := serve(t, nil, rt)

	if code, body := get(t, gw.URL+"/r/x"); code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %q", code, body)
	}
}

func TestBreaker_OpensThen503(t *testing.T) {
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer backend.Close()

	rt := openRoute("r", "/r/", backend.URL)
	rt.CircuitBreaker = config.CircuitBreaker{
		Enabled: true, MinRequests: 2, FailureRatio: 0.5, Timeout: time.Minute,
	}
	gw := serve(t, nil, rt)

	get(t, gw.URL+"/r/x")
	get(t, gw.URL+"/r/x")
	code, _ := get(t, gw.URL+"/r/x")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 once open, got %d", code)
	}
	if calls != 2 {
		t.Fatalf("open breaker must not reach upstream: calls=%d", calls)
	}
}

func TestMatchMethods_Enforced(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	rt := openRoute("r", "/r/", backend.URL)
	rt.Match.Methods = []string{"GET"}
	gw := serve(t, nil, rt)

	resp, err := http.Post(gw.URL+"/r/x", "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("POST on a GET-only route: want 404, got %d", resp.StatusCode)
	}
	if code, _ := get(t, gw.URL+"/r/x"); code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d", code)
	}
}

func TestLongestPrefixWins(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("root"))
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("api"))
	}))
	defer b.Close()

	root := openRoute("root", "/", a.URL)
	root.StripPrefix = ""
	gw := serve(t, nil, root, openRoute("api", "/api/", b.URL))

	if _, body := get(t, gw.URL+"/api/x"); body != "api" {
		t.Fatalf("/api/x went to %q", body)
	}
	if _, body := get(t, gw.URL+"/other"); body != "root" {
		t.Fatalf("/other went to %q", body)
	}
	if code, _ := get(t, gw.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz must not be swallowed by a / route: %d", code)
	}
}

func TestRequestID_ForwardedToUpstream(t *testing.T) {
	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("X-Request-ID")
	}))
	defer backend.Close()

	gw := serve(t, nil, openRoute("r", "/r/", backend.URL))
	resp, err := http.Get(gw.URL + "/r/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := resp.Header.Get("X-Request-ID")
	if got := <-seen; want == "" || got != want {
		t.Fatalf("upstream saw %q, client saw %q", got, want)
	}
}

func TestLeastConn_AvoidsBusyUpstream(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		_, _ = w.Write([]byte("busy"))
	}))
	defer busy.Close()
	defer close(release)
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("idle"))
	}))
	defer idle.Close()

	rt := openRoute("r", "/r/", busy.URL, idle.URL)
	rt.LoadBalance = "leastconn"
	gw := serve(t, nil, rt)

	// Send requests until one is stuck on the busy upstream. Every request after
	// that belongs on the idle one.
	for held := false; !held; {
		go func() {
			if resp, err := http.Get(gw.URL + "/r/hold"); err == nil {
				_ = resp.Body.Close()
			}
		}()
		select {
		case <-entered:
			held = true
		case <-time.After(50 * time.Millisecond):
		}
	}

	for i := 0; i < 6; i++ {
		client := &http.Client{Timeout: 500 * time.Millisecond}
		resp, err := client.Get(gw.URL + "/r/x")
		if err != nil {
			t.Fatalf("request %d hit the busy upstream: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(body) != "idle" {
			t.Fatalf("request %d: got %q", i, body)
		}
	}
}

func limitedRoute(backendURL string) config.Route {
	rt := openRoute("r", "/r/", backendURL)
	rt.RateLimit = config.RateLimit{Mode: "local", Key: "ip", Rate: 0.001, Burst: 2}
	return rt
}

// A client must not get a fresh bucket just by opening a new TCP connection.
func TestIPRateLimit_AppliesAcrossConnections(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	gw := serve(t, nil, limitedRoute(backend.URL))

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	throttled := 0
	for i := 0; i < 5; i++ {
		resp, err := client.Get(gw.URL + "/r/x")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled != 3 {
		t.Fatalf("burst=2 over 5 requests: want 3 throttled, got %d", throttled)
	}
}

// Forwarding headers from an untrusted peer must not change the rate-limit key.
func TestIPRateLimit_SpoofedForwardingHeadersIgnored(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()
	gw := serve(t, nil, limitedRoute(backend.URL))

	throttled := 0
	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", gw.URL+"/r/x", nil)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("10.0.0.%d", i))
		req.Header.Set("X-Real-IP", fmt.Sprintf("10.1.0.%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled != 3 {
		t.Fatalf("spoofed headers must not bypass the limit: want 3 throttled, got %d", throttled)
	}
}

// Behind a trusted proxy each real client gets its own bucket.
func TestIPRateLimit_TrustedProxyIdentifiesClients(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	cfg := &config.Config{Routes: []config.Route{limitedRoute(backend.URL)}}
	cfg.Server.TrustedProxies = []string{"127.0.0.1"}
	gw := serveConfig(t, nil, server.Options{}, cfg)

	for i := 0; i < 5; i++ {
		req, _ := http.NewRequest("GET", gw.URL+"/r/x", nil)
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("client %d: got %d", i, resp.StatusCode)
		}
	}
}

// Redis-backed limits are shared: two gateways draw from one bucket.
func TestRedisRateLimit_SharedBetweenGateways(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	rdb := newRedis(t)
	for name, rl := range map[string]config.RateLimit{
		"token_bucket":   {Mode: "redis", Algorithm: "token_bucket", Key: "ip", Rate: 0.001, Burst: 3},
		"sliding_window": {Mode: "redis", Algorithm: "sliding_window", Key: "ip", Rate: 3},
	} {
		t.Run(name, func(t *testing.T) {
			rt := openRoute(name, "/r/", backend.URL)
			rt.RateLimit = rl
			cfg := func() *config.Config { return &config.Config{Routes: []config.Route{rt}} }
			first := serveConfig(t, nil, server.Options{RedisClient: rdb}, cfg())
			second := serveConfig(t, nil, server.Options{RedisClient: rdb}, cfg())

			throttled := 0
			for i := 0; i < 6; i++ {
				gw := first
				if i%2 == 1 {
					gw = second
				}
				if code, _ := get(t, gw.URL+"/r/x"); code == http.StatusTooManyRequests {
					throttled++
				}
			}
			if throttled != 3 {
				t.Fatalf("limit of 3 over 6 requests: want 3 throttled, got %d", throttled)
			}
		})
	}
}

func cachedAuthRoute(backendURL string) config.Route {
	rt := openRoute("r", "/r/", backendURL)
	rt.Auth = config.Auth{
		Type: "jwt", Required: true, Algorithms: []string{"HS256"}, SecretEnv: "GW_TEST_SECRET",
	}
	rt.Cache = config.Cache{Enabled: true, TTL: time.Minute}
	return rt
}

// On a protected route, one user's cached response must not reach another.
func TestCache_IsolatesUsers(t *testing.T) {
	t.Setenv("GW_TEST_SECRET", testSecret)
	calls := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(r.Header.Get("Authorization")))
	}))
	defer backend.Close()

	gw := serveConfig(t, nil, server.Options{RedisClient: newRedis(t)},
		&config.Config{Routes: []config.Route{cachedAuthRoute(backend.URL)}})

	alice, bob := mintToken("alice"), mintToken("bob")
	_, aliceBody := getAs(t, gw.URL+"/r/me", alice)
	_, bobBody := getAs(t, gw.URL+"/r/me", bob)
	if strings.Contains(bobBody, alice) || !strings.Contains(bobBody, bob) {
		t.Fatalf("bob received %q, alice received %q", bobBody, aliceBody)
	}
	resp, again := getAs(t, gw.URL+"/r/me", alice)
	if resp.Header.Get("X-Cache") != "HIT" || again != aliceBody {
		t.Fatalf("alice repeat: %q %q", resp.Header.Get("X-Cache"), again)
	}
	if calls != 2 {
		t.Fatalf("backend ran %d times", calls)
	}
}

func TestCache_HitStillRequiresAuth(t *testing.T) {
	t.Setenv("GW_TEST_SECRET", testSecret)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer backend.Close()

	gw := serveConfig(t, nil, server.Options{RedisClient: newRedis(t)},
		&config.Config{Routes: []config.Route{cachedAuthRoute(backend.URL)}})

	getAs(t, gw.URL+"/r/x", mintToken("alice"))
	resp, body := getAs(t, gw.URL+"/r/x", "")
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, "secret") {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestProxy_SkipsUnhealthyUpstream(t *testing.T) {
	sick := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("sick"))
	}))
	defer sick.Close()
	well := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("well"))
	}))
	defer well.Close()

	rt := openRoute("r", "/r/", sick.URL, well.URL)
	p, err := proxy.New(&rt, slog.New(slog.NewTextHandler(io.Discard, nil)), proxy.Options{
		Healthy: func(u string) bool { return u != sick.URL },
	})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(p)
	defer gw.Close()

	for i := 0; i < 6; i++ {
		if _, body := get(t, gw.URL+"/r/x"); body != "well" {
			t.Fatalf("request %d went to %q", i, body)
		}
	}
}

// fullChain has every wrapper that sits between a client and the upstream.
func fullChain(t *testing.T, backendURL string) *httptest.Server {
	t.Helper()
	rt := openRoute("r", "/r/", backendURL)
	rt.CircuitBreaker = config.CircuitBreaker{Enabled: true}
	rt.Cache = config.Cache{Enabled: true, TTL: time.Minute}
	return serveConfig(t, nil, server.Options{RedisClient: newRedis(t)},
		&config.Config{Routes: []config.Route{rt}})
}

// Chunks must reach the client as they are produced, not when the handler ends.
func TestStreaming_ThroughFullChain(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("first\n"))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte("second\n"))
	}))
	defer backend.Close()
	gw := fullChain(t, backend.URL)

	resp, err := http.Get(gw.URL + "/r/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	lines := bufio.NewReader(resp.Body)

	got := make(chan string, 1)
	go func() {
		line, _ := lines.ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if line != "first\n" {
			t.Fatalf("got %q", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first chunk was buffered until the response ended")
	}
	close(release)
	if line, _ := lines.ReadString('\n'); line != "second\n" {
		t.Fatalf("got %q", line)
	}
}

// Upgrades need Hijack, which only works if every wrapper unwraps.
func TestUpgrade_ThroughFullChain(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString("echo " + line)
		_ = rw.Flush()
	}))
	defer backend.Close()
	gw := fullChain(t, backend.URL)

	conn, err := net.Dial("tcp", strings.TrimPrefix(gw.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	_, _ = fmt.Fprint(conn, "GET /r/ws HTTP/1.1\r\nHost: gw\r\nConnection: Upgrade\r\nUpgrade: echo\r\n\r\n")
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("got %d", resp.StatusCode)
	}
	_, _ = fmt.Fprint(conn, "hello\n")
	if line, _ := reader.ReadString('\n'); line != "echo hello\n" {
		t.Fatalf("got %q", line)
	}
}

func TestTraceContext_ReachesUpstream(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})

	seen := make(chan string, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Traceparent")
	}))
	defer backend.Close()

	gw := serveConfig(t, nil, server.Options{OTelEnabled: true},
		&config.Config{Routes: []config.Route{openRoute("r", "/r/", backend.URL)}})
	get(t, gw.URL+"/r/x")

	if got := <-seen; got == "" {
		t.Fatal("upstream saw no traceparent header")
	}
}

func TestAccessLog_HasRouteLabel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer backend.Close()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	gw := serve(t, logger, openRoute("my-route", "/r/", backend.URL))
	get(t, gw.URL+"/r/x")

	var line struct {
		Route string `json:"route"`
	}
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("parse log %q: %v", buf.String(), err)
	}
	if line.Route != "my-route" {
		t.Fatalf("access log route = %q", line.Route)
	}
}
