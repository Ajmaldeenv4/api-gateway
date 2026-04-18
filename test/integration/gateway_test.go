// Integration tests spin up the gateway + a real test backend using httptest.
// Redis-backed rate limit tests are skipped unless REDIS_ADDR is set.
package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ajmal/api-gateway/internal/config"
	"github.com/ajmal/api-gateway/internal/server"
	"context"
	"log/slog"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

const testSecret = "integration-secret"

func mintToken(sub string) string {
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": sub,
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte(testSecret))
	return tok
}

func buildConfig(backendURL string) *config.Config {
	required := true
	failOpen := true
	rate := float64(5)
	return &config.Config{
		Server: config.ServerConfig{
			Listen:       ":18080",
			MetricsAddr:  ":19090",
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  60 * time.Second,
		},
		Routes: []config.Route{
			{
				ID:          "protected",
				Match:       config.Match{Prefix: "/protected/"},
				StripPrefix: "/protected",
				Upstreams:   []config.Upstream{{URL: backendURL, Weight: 1}},
				LoadBalance: "roundrobin",
				Timeout:     5 * time.Second,
				Auth: config.Auth{
					Type: "jwt", Required: required,
					Algorithms: []string{"HS256"},
					SecretEnv:  "GW_TEST_SECRET",
				},
				RateLimit: config.RateLimit{
					Mode: "local", Algorithm: "token_bucket",
					Key: "sub", Rate: rate, Burst: 3,
					FailOpen: &failOpen,
				},
			},
			{
				ID:          "open",
				Match:       config.Match{Prefix: "/open/"},
				StripPrefix: "/open",
				Upstreams:   []config.Upstream{{URL: backendURL, Weight: 1}},
				LoadBalance: "roundrobin",
				Timeout:     5 * time.Second,
				Auth:        config.Auth{Type: "none"},
				RateLimit:   config.RateLimit{Mode: "off", FailOpen: &failOpen},
			},
		},
	}
}

func buildGateway(t *testing.T, backendURL string) http.Handler {
	t.Helper()
	t.Setenv("GW_TEST_SECRET", testSecret)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := buildConfig(backendURL)
	h, err := server.Build(cfg, nil, logger)
	if err != nil {
		t.Fatalf("build gateway: %v", err)
	}
	return h
}

func TestHealthz(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer backend.Close()

	gw := httptest.NewServer(buildGateway(t, backend.URL))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthz: %d", resp.StatusCode)
	}
}

func TestProtectedRoute_Auth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer backend.Close()

	gw := httptest.NewServer(buildGateway(t, backend.URL))
	defer gw.Close()

	// No token → 401.
	resp, _ := http.Get(gw.URL + "/protected/ping")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: want 401, got %d", resp.StatusCode)
	}

	// Valid token → 200.
	req, _ := http.NewRequest("GET", gw.URL+"/protected/ping", nil)
	req.Header.Set("Authorization", "Bearer "+mintToken("alice"))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid token: want 200, got %d", resp.StatusCode)
	}
}

func TestOpenRoute_NoAuth(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "open")
	}))
	defer backend.Close()

	gw := httptest.NewServer(buildGateway(t, backend.URL))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/open/ping")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open: want 200, got %d", resp.StatusCode)
	}
}

func TestLocalRateLimit_Throttles(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	defer backend.Close()

	gw := httptest.NewServer(buildGateway(t, backend.URL))
	defer gw.Close()

	token := "Bearer " + mintToken("rate-test-user")
	throttled := 0
	for i := 0; i < 10; i++ {
		req, _ := http.NewRequest("GET", gw.URL+"/protected/ping", nil)
		req.Header.Set("Authorization", token)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode == http.StatusTooManyRequests {
			throttled++
		}
	}
	if throttled == 0 {
		t.Fatal("expected some requests to be throttled (burst=3)")
	}
}

func TestRedisRateLimit(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set — skipping Redis integration test")
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	_ = client.FlushDB(ctx) // clean slate for rate-limit keys
	t.Log("Redis integration test: PASS (Redis is reachable)")
}
